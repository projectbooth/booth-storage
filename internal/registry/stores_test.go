package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/projectbooth/booth-storage/internal/backend"
)

var testCtx = context.Background()

func sampleRecord(ws, id string) Record {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return Record{
		Workspace: ws, ID: id, DisplayName: "Display " + id, Kind: backend.KindS3,
		Config:         json.RawMessage(`{"bucket":"b","pathStyle":true}`),
		HasCredentials: true, CreatedBy: "alice", CreatedAt: now, UpdatedAt: now,
	}
}

// runMetadataStoreTests is the contract every MetadataStore must satisfy, run against
// both the in-memory and the real PostgreSQL implementation so they can't drift apart.
func runMetadataStoreTests(t *testing.T, newStore func(t *testing.T) MetadataStore) {
	t.Run("CreateGetRoundTrip", func(t *testing.T) {
		s := newStore(t)
		want := sampleRecord("acme", "lake")
		if err := s.Create(testCtx, want); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(testCtx, "acme", "lake")
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != want.ID || got.DisplayName != want.DisplayName || got.Kind != want.Kind ||
			!got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) ||
			got.CreatedBy != "alice" || !got.HasCredentials {
			t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
		}
		var a, b map[string]any
		_ = json.Unmarshal(got.Config, &a)
		_ = json.Unmarshal(want.Config, &b)
		if fmt.Sprint(a) != fmt.Sprint(b) {
			t.Errorf("config = %s, want %s", got.Config, want.Config)
		}
	})

	t.Run("DuplicateIDIsErrExists", func(t *testing.T) {
		s := newStore(t)
		if err := s.Create(testCtx, sampleRecord("acme", "dup")); err != nil {
			t.Fatal(err)
		}
		if err := s.Create(testCtx, sampleRecord("acme", "dup")); !errors.Is(err, ErrExists) {
			t.Errorf("second Create error = %v, want ErrExists", err)
		}
	})

	// ADR 0035: many backends per workspace, and the same ID may exist in different
	// workspaces without colliding.
	t.Run("MultipleBackendsPerWorkspaceAndWorkspaceIsolation", func(t *testing.T) {
		s := newStore(t)
		for _, r := range []Record{
			sampleRecord("acme", "zeta"), sampleRecord("acme", "alpha"), sampleRecord("acme", "mid"),
			sampleRecord("other", "alpha"), // same ID, different workspace
		} {
			if err := s.Create(testCtx, r); err != nil {
				t.Fatalf("Create(%s/%s): %v", r.Workspace, r.ID, err)
			}
		}
		acme, err := s.List(testCtx, "acme")
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range acme {
			ids = append(ids, r.ID)
			if r.Workspace != "acme" {
				t.Errorf("acme listing leaked %s/%s", r.Workspace, r.ID)
			}
		}
		if fmt.Sprint(ids) != "[alpha mid zeta]" {
			t.Errorf("acme IDs = %v, want [alpha mid zeta] (all three, ordered by ID)", ids)
		}
		if _, err := s.Get(testCtx, "other", "zeta"); !errors.Is(err, ErrNotFound) {
			t.Errorf("other/zeta should not exist, got %v", err)
		}
	})

	t.Run("ListEmptyWorkspaceIsEmptyNotError", func(t *testing.T) {
		s := newStore(t)
		got, err := s.List(testCtx, "nobody")
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("List = %v, %v; want empty non-nil slice", got, err)
		}
	})

	t.Run("UpdateChangesOnlyMutableFields", func(t *testing.T) {
		s := newStore(t)
		orig := sampleRecord("acme", "up")
		if err := s.Create(testCtx, orig); err != nil {
			t.Fatal(err)
		}
		upd := orig
		upd.DisplayName = "Renamed"
		upd.Config = json.RawMessage(`{"bucket":"other"}`)
		upd.HasCredentials = false
		upd.UpdatedAt = orig.UpdatedAt.Add(time.Second)
		upd.Kind = backend.KindGCS // must be ignored: kind is immutable
		upd.CreatedBy = "mallory"  // must be ignored
		if err := s.Update(testCtx, upd); err != nil {
			t.Fatal(err)
		}

		got, _ := s.Get(testCtx, "acme", "up")
		if got.DisplayName != "Renamed" || got.HasCredentials || !got.UpdatedAt.Equal(upd.UpdatedAt) {
			t.Errorf("mutable fields not updated: %+v", got)
		}
		if got.Kind != backend.KindS3 || got.CreatedBy != "alice" || !got.CreatedAt.Equal(orig.CreatedAt) {
			t.Errorf("immutable fields changed: %+v", got)
		}
	})

	t.Run("UpdateAndDeleteMissingAreErrNotFound", func(t *testing.T) {
		s := newStore(t)
		if err := s.Update(testCtx, sampleRecord("acme", "ghost")); !errors.Is(err, ErrNotFound) {
			t.Errorf("Update(missing) = %v", err)
		}
		if err := s.Delete(testCtx, "acme", "ghost"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Delete(missing) = %v", err)
		}
		if _, err := s.Get(testCtx, "acme", "ghost"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(missing) = %v", err)
		}
	})

	t.Run("DeleteRemoves", func(t *testing.T) {
		s := newStore(t)
		_ = s.Create(testCtx, sampleRecord("acme", "gone"))
		if err := s.Delete(testCtx, "acme", "gone"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(testCtx, "acme", "gone"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get after Delete = %v", err)
		}
	})

	t.Run("Ping", func(t *testing.T) {
		if err := newStore(t).Ping(testCtx); err != nil {
			t.Errorf("Ping: %v", err)
		}
	})
}

func TestMemoryStore(t *testing.T) {
	runMetadataStoreTests(t, func(*testing.T) MetadataStore { return NewMemoryStore() })
}

var schemaSeq atomic.Int64

// TestPostgresStore runs the same suite against a real PostgreSQL. CI provides one;
// locally see hack/docker-compose.emulators.yml.
func TestPostgresStore(t *testing.T) {
	dsn := os.Getenv("BOOTH_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("BOOTH_TEST_REQUIRE_EMULATORS") == "1" {
			t.Fatal("BOOTH_TEST_REQUIRE_EMULATORS=1 but BOOTH_TEST_POSTGRES_DSN is not set")
		}
		t.Skip("BOOTH_TEST_POSTGRES_DSN not set; skipping PostgreSQL tests (see hack/docker-compose.emulators.yml)")
	}

	runMetadataStoreTests(t, func(t *testing.T) MetadataStore {
		// One schema per test, so each test sees an empty, freshly migrated database.
		schema := fmt.Sprintf("t_%d_%d", os.Getpid(), schemaSeq.Add(1))
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
		store, err := NewPostgresStore(testCtx, dsn+sep+"search_path="+schema)
		if err != nil {
			t.Fatalf("NewPostgresStore: %v", err)
		}
		t.Cleanup(store.Close)
		return store
	})
}

// Migrations must be idempotent and safe under concurrent startup (every replica runs
// them at boot).
func TestPostgresMigrationsIdempotentAndConcurrent(t *testing.T) {
	dsn := os.Getenv("BOOTH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BOOTH_TEST_POSTGRES_DSN not set")
	}
	schema := fmt.Sprintf("t_mig_%d_%d", os.Getpid(), schemaSeq.Add(1))
	admin, err := pgx.Connect(testCtx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(testCtx)
	if _, err := admin.Exec(testCtx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(testCtx, "DROP SCHEMA "+schema+" CASCADE") //nolint:errcheck

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	full := dsn + sep + "search_path=" + schema

	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			s, err := NewPostgresStore(testCtx, full)
			if err == nil {
				s.Close()
			}
			errs <- err
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent NewPostgresStore: %v", err)
		}
	}
}

// runCredentialStoreTests is the contract every CredentialStore must satisfy.
func runCredentialStoreTests(t *testing.T, newStore func(t *testing.T) CredentialStore) {
	t.Run("PutGetRoundTrip", func(t *testing.T) {
		s := newStore(t)
		in := map[string]string{"accessKeyId": "AKIA", "secretAccessKey": "s3cr3t"}
		if err := s.Put(testCtx, "acme", "lake", in); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(testCtx, "acme", "lake")
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(in) {
			t.Errorf("got %v, want %v", got, in)
		}
	})

	t.Run("GetMissingIsErrNoCredentials", func(t *testing.T) {
		if _, err := newStore(t).Get(testCtx, "acme", "none"); !errors.Is(err, ErrNoCredentials) {
			t.Errorf("Get(missing) = %v, want ErrNoCredentials", err)
		}
	})

	// A dropped key must actually disappear — a merge-style update would silently keep
	// a stale secret alive.
	t.Run("PutReplacesWholesale", func(t *testing.T) {
		s := newStore(t)
		_ = s.Put(testCtx, "acme", "x", map[string]string{"accessKeyId": "A", "secretAccessKey": "B", "sessionToken": "C"})
		_ = s.Put(testCtx, "acme", "x", map[string]string{"accessKeyId": "A2", "secretAccessKey": "B2"})
		got, _ := s.Get(testCtx, "acme", "x")
		if _, stale := got["sessionToken"]; stale || got["accessKeyId"] != "A2" {
			t.Errorf("after replace got %v", got)
		}
	})

	t.Run("BackendsAreIsolated", func(t *testing.T) {
		s := newStore(t)
		_ = s.Put(testCtx, "acme", "a", map[string]string{"k": "acme-a"})
		_ = s.Put(testCtx, "acme", "b", map[string]string{"k": "acme-b"})
		_ = s.Put(testCtx, "other", "a", map[string]string{"k": "other-a"})
		for ws, id := range map[string]string{"acme/a": "acme-a", "acme/b": "acme-b", "other/a": "other-a"} {
			parts := strings.SplitN(ws, "/", 2)
			got, err := s.Get(testCtx, parts[0], parts[1])
			if err != nil || got["k"] != id {
				t.Errorf("%s = %v, %v; want %s", ws, got, err, id)
			}
		}
	})

	t.Run("DeleteIsIdempotent", func(t *testing.T) {
		s := newStore(t)
		_ = s.Put(testCtx, "acme", "d", map[string]string{"k": "v"})
		if err := s.Delete(testCtx, "acme", "d"); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(testCtx, "acme", "d"); err != nil {
			t.Errorf("second Delete = %v, want nil", err)
		}
		if _, err := s.Get(testCtx, "acme", "d"); !errors.Is(err, ErrNoCredentials) {
			t.Errorf("Get after Delete = %v", err)
		}
	})
}

func TestMemoryCredentials(t *testing.T) {
	runCredentialStoreTests(t, func(*testing.T) CredentialStore { return NewMemoryCredentials() })
}

func TestKubernetesCredentials(t *testing.T) {
	runCredentialStoreTests(t, func(*testing.T) CredentialStore {
		return NewKubernetesCredentials(fake.NewSimpleClientset(), "booth-storage")
	})
}

func TestKubernetesCredentials_SecretShape(t *testing.T) {
	client := fake.NewSimpleClientset()
	s := NewKubernetesCredentials(client, "booth-storage")
	if err := s.Put(testCtx, "acme", "lake", map[string]string{"accessKeyId": "AKIA", "secretAccessKey": "s3cr3t"}); err != nil {
		t.Fatal(err)
	}

	name := SecretName("acme", "lake")
	secret, err := client.CoreV1().Secrets("booth-storage").Get(testCtx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Secret %s not created in the module namespace: %v", name, err)
	}
	if string(secret.Data["secretAccessKey"]) != "s3cr3t" {
		t.Errorf("secret data = %v", secret.Data)
	}
	if secret.Labels[labelManagedBy] != "booth-storage" || secret.Labels[labelWorkspace] != "acme" || secret.Labels[labelBackendID] != "lake" {
		t.Errorf("labels = %v, want managed-by/workspace/backend labels for discoverability", secret.Labels)
	}
	if strings.Contains(name, "acme") || strings.Contains(name, "lake") {
		t.Errorf("Secret name %q embeds workspace/id; it should be an opaque hash", name)
	}
}

func TestSecretName_UnambiguousAcrossWorkspaceIDSplit(t *testing.T) {
	// Concatenating with a hyphen would make these collide ("a-b-c" both ways).
	if SecretName("a-b", "c") == SecretName("a", "b-c") {
		t.Fatal("distinct (workspace, id) pairs map to the same Secret name")
	}
	if SecretName("acme", "lake") != SecretName("acme", "lake") {
		t.Fatal("SecretName is not deterministic")
	}
}
