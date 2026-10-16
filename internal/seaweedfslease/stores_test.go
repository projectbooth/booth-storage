package seaweedfslease

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/projectbooth/booth-storage/internal/registry"
)

var testCtx = context.Background()

func sampleLease(id, workspace, backendID string, expiresAt time.Time) Lease {
	return Lease{
		ID: id, Workspace: workspace, BackendID: backendID,
		IAMUserName: "booth-" + id, AccessKeyID: "AKIA" + id, Prefix: "incoming/",
		Access: "read", ExpiresAt: expiresAt.UTC().Truncate(time.Microsecond),
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
}

// runStoreTests is the contract every Store must satisfy, run against both the
// in-memory and the real PostgreSQL implementation so they can't drift apart.
func runStoreTests(t *testing.T, newStore func(t *testing.T) Store) {
	t.Run("RecordThenPendingSweepFindsExpiredOnly", func(t *testing.T) {
		s := newStore(t)
		now := time.Now().UTC()
		expired := sampleLease("11111111-1111-1111-1111-111111111111", "acme", "lake", now.Add(-time.Minute))
		active := sampleLease("22222222-2222-2222-2222-222222222222", "acme", "lake", now.Add(time.Hour))
		if err := s.Record(testCtx, expired); err != nil {
			t.Fatal(err)
		}
		if err := s.Record(testCtx, active); err != nil {
			t.Fatal(err)
		}

		pending, err := s.PendingSweep(testCtx, now, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 1 || pending[0].ID != expired.ID {
			t.Fatalf("PendingSweep = %+v, want only the expired lease", pending)
		}
		if pending[0].IAMUserName != expired.IAMUserName || pending[0].AccessKeyID != expired.AccessKeyID || pending[0].Prefix != expired.Prefix {
			t.Errorf("round trip mismatch: got %+v want %+v", pending[0], expired)
		}
	})

	t.Run("PendingSweepOrdersOldestExpiryFirstAndRespectsLimit", func(t *testing.T) {
		s := newStore(t)
		now := time.Now().UTC()
		oldest := sampleLease("33333333-3333-3333-3333-333333333333", "acme", "lake", now.Add(-10*time.Minute))
		middle := sampleLease("44444444-4444-4444-4444-444444444444", "acme", "lake", now.Add(-5*time.Minute))
		newest := sampleLease("55555555-5555-5555-5555-555555555555", "acme", "lake", now.Add(-time.Minute))
		for _, l := range []Lease{newest, oldest, middle} { // inserted out of order
			if err := s.Record(testCtx, l); err != nil {
				t.Fatal(err)
			}
		}
		pending, err := s.PendingSweep(testCtx, now, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 2 || pending[0].ID != oldest.ID || pending[1].ID != middle.ID {
			t.Fatalf("PendingSweep = %+v, want [oldest, middle] in that order", pending)
		}
	})

	t.Run("MarkSweptExcludesFromPendingSweep", func(t *testing.T) {
		s := newStore(t)
		now := time.Now().UTC()
		l := sampleLease("66666666-6666-6666-6666-666666666666", "acme", "lake", now.Add(-time.Minute))
		if err := s.Record(testCtx, l); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkSwept(testCtx, l.ID, now); err != nil {
			t.Fatal(err)
		}
		pending, err := s.PendingSweep(testCtx, now, 10)
		if err != nil || len(pending) != 0 {
			t.Fatalf("PendingSweep after MarkSwept = %+v, %v; want empty", pending, err)
		}
	})

	t.Run("MarkSweptOnAlreadyGoneLeaseIsNotAnError", func(t *testing.T) {
		if err := newStore(t).MarkSwept(testCtx, "no-such-lease", time.Now()); err != nil {
			t.Errorf("MarkSwept(missing) = %v, want nil (idempotent)", err)
		}
	})

	t.Run("ActiveCountCountsOnlyUnsweptAndNotYetExpired", func(t *testing.T) {
		s := newStore(t)
		now := time.Now().UTC()
		active := sampleLease("77777777-7777-7777-7777-777777777777", "acme", "lake", now.Add(time.Hour))
		expiredUnswept := sampleLease("88888888-8888-8888-8888-888888888888", "acme", "lake", now.Add(-time.Minute))
		expiredSwept := sampleLease("99999999-9999-9999-9999-999999999999", "acme", "lake", now.Add(-time.Minute))
		for _, l := range []Lease{active, expiredUnswept, expiredSwept} {
			if err := s.Record(testCtx, l); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.MarkSwept(testCtx, expiredSwept.ID, now); err != nil {
			t.Fatal(err)
		}
		n, err := s.ActiveCount(testCtx, now)
		if err != nil || n != 1 {
			t.Fatalf("ActiveCount = %d, %v; want 1 (only the still-active lease)", n, err)
		}
	})

	t.Run("Ping", func(t *testing.T) {
		if err := newStore(t).Ping(testCtx); err != nil {
			t.Errorf("Ping: %v", err)
		}
	})
}

func TestMemoryStore(t *testing.T) {
	runStoreTests(t, func(*testing.T) Store { return NewMemoryStore() })
}

var schemaSeq atomic.Int64

// TestPostgresStore runs the same suite against a real PostgreSQL, reusing
// registry.NewPostgresStore to apply this module's migrations (storage_seaweedfs_leases
// is one of them) before handing this package's own Store wrapper the resulting pool.
func TestPostgresStore(t *testing.T) {
	dsn := os.Getenv("BOOTH_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("BOOTH_TEST_REQUIRE_EMULATORS") == "1" {
			t.Fatal("BOOTH_TEST_REQUIRE_EMULATORS=1 but BOOTH_TEST_POSTGRES_DSN is not set")
		}
		t.Skip("BOOTH_TEST_POSTGRES_DSN not set; skipping PostgreSQL tests (see hack/docker-compose.emulators.yml)")
	}

	runStoreTests(t, func(t *testing.T) Store {
		schema := fmt.Sprintf("t_swl_%d_%d", os.Getpid(), schemaSeq.Add(1))
		admin, err := pgx.Connect(testCtx, dsn)
		if err != nil {
			t.Fatalf("connecting: %v", err)
		}
		if _, err := admin.Exec(testCtx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = admin.Exec(testCtx, "DROP SCHEMA "+schema+" CASCADE")
			admin.Close(testCtx)
		})

		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		meta, err := registry.NewPostgresStore(testCtx, dsn+sep+"search_path="+schema)
		if err != nil {
			t.Fatalf("NewPostgresStore (migrations): %v", err)
		}
		t.Cleanup(meta.Close)
		return NewPostgresStore(meta.Pool())
	})
}
