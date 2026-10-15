package uploadgrant

import (
	"context"
	"errors"
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

func sampleGrant(ws, principal, backendID, prefix string) Grant {
	return Grant{
		ID: "11111111-1111-1111-1111-111111111111", Workspace: ws, Principal: principal,
		BackendID: backendID, Prefix: prefix, Extensions: []string{"csv", "png"}, MaxBytes: 1024,
		GrantedBy: "alice", CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
}

// runStoreTests is the contract every Store must satisfy, run against both the
// in-memory and the real PostgreSQL implementation so they can't drift apart.
func runStoreTests(t *testing.T, newStore func(t *testing.T) Store) {
	t.Run("UpsertGetRoundTrip", func(t *testing.T) {
		s := newStore(t)
		want := sampleGrant("acme", "streamlit:acme:app1", "lake", "incoming/")
		got, err := s.Upsert(testCtx, want)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != want.ID || got.Principal != want.Principal || got.BackendID != want.BackendID ||
			got.Prefix != want.Prefix || got.MaxBytes != want.MaxBytes || got.GrantedBy != want.GrantedBy {
			t.Errorf("round trip mismatch: got %+v want %+v", got, want)
		}
		if fmt.Sprint(got.Extensions) != fmt.Sprint(want.Extensions) {
			t.Errorf("extensions = %v, want %v", got.Extensions, want.Extensions)
		}

		fetched, err := s.Get(testCtx, "acme", got.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fetched.Principal != want.Principal {
			t.Errorf("Get = %+v", fetched)
		}
	})

	t.Run("GetMissingIsErrNotFound", func(t *testing.T) {
		if _, err := newStore(t).Get(testCtx, "acme", "nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(missing) = %v, want ErrNotFound", err)
		}
	})

	// ADR 0117: a second grant for the same (workspace, principal, backend, prefix)
	// replaces the first outright, with a fresh ID — mirroring booth-api's write-grants.
	t.Run("UpsertSameScopeReplaces", func(t *testing.T) {
		s := newStore(t)
		first := sampleGrant("acme", "streamlit:acme:app1", "lake", "incoming/")
		if _, err := s.Upsert(testCtx, first); err != nil {
			t.Fatal(err)
		}
		second := first
		second.ID = "22222222-2222-2222-2222-222222222222"
		second.MaxBytes = 2048
		second.Extensions = []string{"json"}
		got, err := s.Upsert(testCtx, second)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != second.ID || got.MaxBytes != 2048 {
			t.Errorf("replace = %+v", got)
		}
		if _, err := s.Get(testCtx, "acme", first.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("the replaced grant's old ID should be gone, got %v", err)
		}
		all, _ := s.List(testCtx, "acme", "streamlit:acme:app1")
		if len(all) != 1 {
			t.Errorf("after replace, %d grants exist for this scope, want 1: %+v", len(all), all)
		}
	})

	// A different prefix on the same backend is a distinct grant, not a replacement —
	// an owner can grant the same app two separate upload targets.
	t.Run("DifferentPrefixIsADistinctGrant", func(t *testing.T) {
		s := newStore(t)
		a := sampleGrant("acme", "streamlit:acme:app1", "lake", "incoming/")
		b := sampleGrant("acme", "streamlit:acme:app1", "lake", "outgoing/")
		b.ID = "33333333-3333-3333-3333-333333333333"
		if _, err := s.Upsert(testCtx, a); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Upsert(testCtx, b); err != nil {
			t.Fatal(err)
		}
		all, err := s.List(testCtx, "acme", "streamlit:acme:app1")
		if err != nil || len(all) != 2 {
			t.Fatalf("List = %+v, %v; want 2 distinct grants", all, err)
		}
	})

	t.Run("ListFiltersByPrincipalAndWorkspace", func(t *testing.T) {
		s := newStore(t)
		for _, g := range []Grant{
			sampleGrant("acme", "streamlit:acme:app1", "lake", "a/"),
			func() Grant {
				g := sampleGrant("acme", "streamlit:acme:app2", "lake", "b/")
				g.ID = "44444444-4444-4444-4444-444444444444"
				return g
			}(),
			func() Grant {
				g := sampleGrant("other", "streamlit:other:app1", "lake", "a/")
				g.ID = "55555555-5555-5555-5555-555555555555"
				return g
			}(),
		} {
			if _, err := s.Upsert(testCtx, g); err != nil {
				t.Fatalf("Upsert(%+v): %v", g, err)
			}
		}
		acme, err := s.List(testCtx, "acme", "")
		if err != nil || len(acme) != 2 {
			t.Fatalf("List(acme, all) = %+v, %v; want 2", acme, err)
		}
		app1, err := s.List(testCtx, "acme", "streamlit:acme:app1")
		if err != nil || len(app1) != 1 || app1[0].Principal != "streamlit:acme:app1" {
			t.Fatalf("List(acme, app1) = %+v, %v", app1, err)
		}
		other, err := s.List(testCtx, "other", "")
		if err != nil || len(other) != 1 {
			t.Fatalf("List(other) leaked or lost rows: %+v, %v", other, err)
		}
	})

	t.Run("DeleteRemovesAndIsScopedToWorkspace", func(t *testing.T) {
		s := newStore(t)
		g, _ := s.Upsert(testCtx, sampleGrant("acme", "streamlit:acme:app1", "lake", "a/"))
		if err := s.Delete(testCtx, "globex", g.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("Delete from the wrong workspace = %v, want ErrNotFound", err)
		}
		if err := s.Delete(testCtx, "acme", g.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(testCtx, "acme", g.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get after Delete = %v", err)
		}
		if err := s.Delete(testCtx, "acme", g.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("second Delete = %v, want ErrNotFound", err)
		}
	})

	t.Run("DeleteByPrincipalRemovesAcrossBackendsAndIsIdempotent", func(t *testing.T) {
		s := newStore(t)
		a := sampleGrant("acme", "streamlit:acme:app1", "lake", "a/")
		b := sampleGrant("acme", "streamlit:acme:app1", "other-backend", "b/")
		b.ID = "66666666-6666-6666-6666-666666666666"
		keep := sampleGrant("acme", "streamlit:acme:app2", "lake", "c/")
		keep.ID = "77777777-7777-7777-7777-777777777777"
		for _, g := range []Grant{a, b, keep} {
			if _, err := s.Upsert(testCtx, g); err != nil {
				t.Fatal(err)
			}
		}
		revoked, err := s.DeleteByPrincipal(testCtx, "acme", "streamlit:acme:app1")
		if err != nil || len(revoked) != 2 {
			t.Fatalf("DeleteByPrincipal = %+v, %v; want 2 revoked", revoked, err)
		}
		remaining, _ := s.List(testCtx, "acme", "")
		if len(remaining) != 1 || remaining[0].Principal != "streamlit:acme:app2" {
			t.Errorf("remaining = %+v, want only app2's grant left", remaining)
		}

		again, err := s.DeleteByPrincipal(testCtx, "acme", "streamlit:acme:app1")
		if err != nil || len(again) != 0 {
			t.Errorf("second DeleteByPrincipal = %+v, %v; want 0, nil (idempotent)", again, err)
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
// registry.NewPostgresStore to apply this module's migrations (storage_upload_grants is
// one of them) before handing this package's own Store wrapper the resulting pool.
func TestPostgresStore(t *testing.T) {
	dsn := os.Getenv("BOOTH_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("BOOTH_TEST_REQUIRE_EMULATORS") == "1" {
			t.Fatal("BOOTH_TEST_REQUIRE_EMULATORS=1 but BOOTH_TEST_POSTGRES_DSN is not set")
		}
		t.Skip("BOOTH_TEST_POSTGRES_DSN not set; skipping PostgreSQL tests (see hack/docker-compose.emulators.yml)")
	}

	runStoreTests(t, func(t *testing.T) Store {
		schema := fmt.Sprintf("t_ug_%d_%d", os.Getpid(), schemaSeq.Add(1))
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
