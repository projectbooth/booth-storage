package seaweedfssweep

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/projectbooth/booth-storage/internal/backend"
	"github.com/projectbooth/booth-storage/internal/registry"
	"github.com/projectbooth/booth-storage/internal/seaweedfslease"
)

func newTestRegistry(t *testing.T) *registry.Service {
	t.Helper()
	return registry.NewService(registry.NewMemoryStore(), registry.NewMemoryCredentials(), registry.FilesystemPolicy{})
}

// registerSeaweedFSBackend registers a seaweedfs-iam-configured s3 backend pointed at an
// address nothing listens on, so any real IAM call against it fails fast with a
// connection error (registry.Service.Create never contacts the network itself — s3.New
// only builds a client).
func registerSeaweedFSBackend(t *testing.T, reg *registry.Service, workspace, id string) {
	t.Helper()
	cfg, err := json.Marshal(map[string]any{
		"endpoint": "http://127.0.0.1:1", "bucket": "lake", "credentialProvider": "seaweedfs-iam",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(context.Background(), workspace, "test", registry.CreateInput{
		ID: id, Kind: backend.KindS3, Config: cfg,
		Credentials: map[string]string{"accessKeyId": "admin", "secretAccessKey": "adminsecret"},
	}); err != nil {
		t.Fatalf("registering backend: %v", err)
	}
}

func TestSweepOnce_FailClosedOnUnreachableBackendAndSucceedsOnGoneBackend(t *testing.T) {
	ctx := context.Background()
	reg := newTestRegistry(t)
	registerSeaweedFSBackend(t, reg, "ws1", "unreachable")
	// "gone" is deliberately never registered — RawCredentials returns ErrNotFound,
	// which sweepLease treats as "nothing left to clean up," i.e. success.

	leases := seaweedfslease.NewMemoryStore()
	now := time.Now()
	unreachable := seaweedfslease.Lease{
		ID: "lease-1", Workspace: "ws1", BackendID: "unreachable",
		IAMUserName: "booth-u1", AccessKeyID: "AKIAU1", Prefix: "p1", Access: "read",
		ExpiresAt: now.Add(-2 * time.Minute), CreatedAt: now.Add(-10 * time.Minute),
	}
	gone := seaweedfslease.Lease{
		ID: "lease-2", Workspace: "ws1", BackendID: "gone",
		IAMUserName: "booth-u2", AccessKeyID: "AKIAU2", Prefix: "p2", Access: "read",
		ExpiresAt: now.Add(-1 * time.Minute), CreatedAt: now.Add(-10 * time.Minute),
	}
	if err := leases.Record(ctx, unreachable); err != nil {
		t.Fatal(err)
	}
	if err := leases.Record(ctx, gone); err != nil {
		t.Fatal(err)
	}

	res := sweepOnce(ctx, Deps{Leases: leases, Registry: reg, Now: func() time.Time { return now }})

	if res.swept != 1 {
		t.Errorf("swept = %d, want 1 (the gone-backend lease)", res.swept)
	}
	if res.failed != 1 {
		t.Errorf("failed = %d, want 1 (the unreachable-backend lease)", res.failed)
	}
	// The oldest pending lease (by ExpiresAt) is "unreachable" (expired 2 minutes ago,
	// vs "gone" at 1 minute ago) — the backlog-age metric must reflect it, computed from
	// the state *before* this cycle's sweep attempts.
	if want := 2 * time.Minute; res.oldestBacklogAge != want {
		t.Errorf("oldestBacklogAge = %v, want %v", res.oldestBacklogAge, want)
	}

	pending, err := leases.PendingSweep(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "lease-1" {
		t.Errorf("pending after sweep = %v, want only lease-1 (still unswept)", pending)
	}
}

func TestSweepOnce_NothingPendingIsANoop(t *testing.T) {
	ctx := context.Background()
	reg := newTestRegistry(t)
	leases := seaweedfslease.NewMemoryStore()
	res := sweepOnce(ctx, Deps{Leases: leases, Registry: reg})
	if res.swept != 0 || res.failed != 0 || res.oldestBacklogAge != 0 {
		t.Errorf("res = %+v, want all zero", res)
	}
}

func TestSweepOnce_ActiveLeaseIsNotSweptButCountedOutstanding(t *testing.T) {
	ctx := context.Background()
	reg := newTestRegistry(t)
	leases := seaweedfslease.NewMemoryStore()
	now := time.Now()
	if err := leases.Record(ctx, seaweedfslease.Lease{
		ID: "active", Workspace: "ws1", BackendID: "whatever",
		IAMUserName: "booth-u3", AccessKeyID: "AKIAU3", Prefix: "p3", Access: "read",
		ExpiresAt: now.Add(5 * time.Minute), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	res := sweepOnce(ctx, Deps{Leases: leases, Registry: reg, Now: func() time.Time { return now }})
	if res.swept != 0 || res.failed != 0 {
		t.Errorf("res = %+v, want no sweep attempts for a not-yet-expired lease", res)
	}
	if res.active != 1 {
		t.Errorf("active = %d, want 1", res.active)
	}
}
