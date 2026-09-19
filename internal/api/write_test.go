package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ADR 0038: the file browser writes — create folder, rename/move, delete — for editors and
// owners; viewers are read-only. Enforced here on the server, independent of any UI.

var writeRoutes = []struct {
	name, method, path string
	body               any
}{
	{"create folder", "POST", "/api/backends/x/folders", map[string]any{"path": "newdir"}},
	{"delete object", "DELETE", "/api/backends/x/objects/f.txt", nil},
	{"delete folder", "DELETE", "/api/backends/x/folders/somedir", nil},
	{"move", "POST", "/api/backends/x/move", map[string]any{"from": "f.txt", "to": "g.txt"}},
}

func TestWriteRoutesRequireAuthentication(t *testing.T) {
	e := newTestEnv(t)
	for _, rt := range writeRoutes {
		t.Run(rt.name, func(t *testing.T) {
			expect(t, e.do(t, caller{}, rt.method, rt.path, rt.body), 401)
			expect(t, e.do(t, caller{token: "forged", workspace: "acme", role: "owner"}, rt.method, rt.path, rt.body), 401)
		})
	}
}

// A viewer is refused on every mutating route, and nothing changes on disk.
func TestViewerCannotMutateAnything(t *testing.T) {
	e := newTestEnv(t)
	dir := e.registerFS(t, "acme", "x")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "somedir"), 0o750); err != nil {
		t.Fatal(err)
	}

	for _, rt := range writeRoutes {
		t.Run(rt.name, func(t *testing.T) {
			r := e.do(t, carol, rt.method, rt.path, rt.body)
			expect(t, r, 403)
			if !strings.Contains(r.errorMessage(), "editors and owners") {
				t.Errorf("message = %q", r.errorMessage())
			}
		})
	}
	// Uploads too (covered by the role matrix, restated here as the ADR's whole point).
	expect(t, e.do(t, carol, "PUT", "/api/backends/x/objects/new.txt", "x"), 403)

	if data, err := os.ReadFile(filepath.Join(dir, "f.txt")); err != nil || string(data) != "keep" {
		t.Errorf("viewer's rejected requests changed f.txt: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "somedir")); err != nil {
		t.Errorf("viewer's rejected delete removed a folder: %v", err)
	}
	for _, name := range []string{"newdir", "g.txt", "new.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("viewer's rejected request created %s", name)
		}
	}
}

func TestEditorsAndOwnersCanUseEveryWriteOperation(t *testing.T) {
	for name, c := range map[string]caller{"editor": bob, "owner": alice} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			dir := e.registerFS(t, "acme", "x")

			expect(t, e.do(t, c, "POST", "/api/backends/x/folders", map[string]any{"path": "reports/2026"}), 201)
			expect(t, e.do(t, c, "PUT", "/api/backends/x/objects/reports/2026/q1.csv", "a,b"), 200)
			expect(t, e.do(t, c, "POST", "/api/backends/x/move", map[string]any{"from": "reports/2026/q1.csv", "to": "reports/2026/q1-final.csv"}), 200)
			expect(t, e.do(t, c, "POST", "/api/backends/x/move", map[string]any{"from": "reports", "to": "archive", "folder": true}), 200)
			if data, err := os.ReadFile(filepath.Join(dir, "archive", "2026", "q1-final.csv")); err != nil || string(data) != "a,b" {
				t.Fatalf("moved file: %q %v", data, err)
			}
			expect(t, e.do(t, c, "DELETE", "/api/backends/x/objects/archive/2026/q1-final.csv", nil), 204)
			expect(t, e.do(t, c, "DELETE", "/api/backends/x/objects/archive/2026/q1-final.csv", nil), 404)

			expect(t, e.do(t, c, "PUT", "/api/backends/x/objects/archive/keep.txt", "k"), 200)
			r := e.do(t, c, "DELETE", "/api/backends/x/folders/archive", nil)
			expect(t, r, 200)
			var out struct{ Deleted int }
			r.json(t, &out)
			if out.Deleted != 1 {
				t.Errorf("deleted = %d, want 1 (keep.txt)", out.Deleted)
			}
			if _, err := os.Stat(filepath.Join(dir, "archive")); err == nil {
				t.Error("folder still on disk after delete")
			}
		})
	}
}

func TestWriteOperations_ConflictsAndErrors(t *testing.T) {
	e := newTestEnv(t)
	e.registerFS(t, "acme", "x")
	expect(t, e.do(t, bob, "PUT", "/api/backends/x/objects/a.txt", "a"), 200)
	expect(t, e.do(t, bob, "PUT", "/api/backends/x/objects/b.txt", "b"), 200)

	// A rename onto an existing object is a 409, and destroys nothing.
	expect(t, e.do(t, bob, "POST", "/api/backends/x/move", map[string]any{"from": "a.txt", "to": "b.txt"}), 409)
	if r := e.do(t, carol, "GET", "/api/backends/x/objects/b.txt", nil); r.Body.String() != "b" {
		t.Errorf("rejected move altered the destination: %q", r.Body.String())
	}
	// A file where a folder is wanted.
	expect(t, e.do(t, bob, "POST", "/api/backends/x/folders", map[string]any{"path": "a.txt"}), 409)

	expect(t, e.do(t, bob, "POST", "/api/backends/x/move", map[string]any{"from": "nope", "to": "x"}), 404)
	expect(t, e.do(t, bob, "DELETE", "/api/backends/x/folders/nope", nil), 404)
	expect(t, e.do(t, bob, "POST", "/api/backends/unregistered/folders", map[string]any{"path": "d"}), 404)

	// Malformed and unknown-field bodies.
	expect(t, e.do(t, bob, "POST", "/api/backends/x/folders", "{nope"), 400)
	expect(t, e.do(t, bob, "POST", "/api/backends/x/folders", map[string]any{"path": "d", "extra": 1}), 400)
	expect(t, e.do(t, bob, "POST", "/api/backends/x/move", map[string]any{"from": "a.txt", "to": "c.txt", "surprise": true}), 400)
}

// The backend root can never be deleted, and no write route can be steered outside it.
func TestWriteOperations_RefuseRootAndTraversal(t *testing.T) {
	e := newTestEnv(t)
	dir := e.registerFS(t, "acme", "x")
	expect(t, e.do(t, bob, "PUT", "/api/backends/x/objects/precious.txt", "p"), 200)
	outside := filepath.Join(e.fsRoot, "acme", "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"/api/backends/x/folders/", "/api/backends/x/folders//", "/api/backends/x/folders/."} {
		if r := e.do(t, alice, "DELETE", p, nil); r.Code < 400 || r.Code >= 500 {
			t.Errorf("DELETE %s = %d, want a 4xx (the root must not be deletable)", p, r.Code)
		}
	}
	for _, p := range []string{"/api/backends/x/folders/..", "/api/backends/x/folders/../..", "/api/backends/x/objects/../outside.txt", "/api/backends/x/folders/%2e%2e"} {
		if r := e.do(t, alice, "DELETE", p, nil); r.Code < 400 || r.Code >= 500 {
			t.Errorf("DELETE %s = %d, want a 4xx", p, r.Code)
		}
	}
	for _, mv := range []map[string]any{
		{"from": "precious.txt", "to": "../moved.txt"},
		{"from": "../outside.txt", "to": "in.txt"},
		{"from": "precious.txt", "to": "/etc/x"},
		{"from": "..", "to": "z", "folder": true},
		{"from": "", "to": "z", "folder": true},
	} {
		if r := e.do(t, alice, "POST", "/api/backends/x/move", mv); r.Code < 400 || r.Code >= 500 {
			t.Errorf("move %v = %d, want a 4xx", mv, r.Code)
		}
	}
	expect(t, e.do(t, alice, "POST", "/api/backends/x/folders", map[string]any{"path": "../evil"}), 400)

	if data, err := os.ReadFile(filepath.Join(dir, "precious.txt")); err != nil || string(data) != "p" {
		t.Errorf("backend data damaged by refused requests: %q %v", data, err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "outside" {
		t.Errorf("file outside the backend was touched: %q %v", data, err)
	}
	for _, n := range []string{"moved.txt", "in.txt", "evil"} {
		for _, base := range []string{dir, filepath.Dir(dir)} {
			if _, err := os.Stat(filepath.Join(base, n)); err == nil {
				t.Errorf("%s created under %s", n, base)
			}
		}
	}
}

// One workspace can neither see nor mutate another's backend through the write routes.
func TestWriteOperations_WorkspaceIsolation(t *testing.T) {
	e := newTestEnv(t)
	acmeDir := e.registerFS(t, "acme", "shared")
	e.registerFS(t, "globex", "shared")
	expect(t, e.do(t, bob, "PUT", "/api/backends/shared/objects/f.txt", "acme"), 200)
	expect(t, e.do(t, dave, "PUT", "/api/backends/shared/objects/f.txt", "globex"), 200)

	expect(t, e.do(t, dave, "DELETE", "/api/backends/shared/objects/f.txt", nil), 204)
	if data, _ := os.ReadFile(filepath.Join(acmeDir, "f.txt")); string(data) != "acme" {
		t.Errorf("globex's delete affected acme's data: %q", data)
	}
}

func TestWriteOperations_ManyFilesFolderDelete(t *testing.T) {
	e := newTestEnv(t)
	e.registerFS(t, "acme", "x")
	for i := 0; i < 25; i++ {
		expect(t, e.do(t, bob, "PUT", fmt.Sprintf("/api/backends/x/objects/bulk/n/f%02d.txt", i), "x"), 200)
	}
	r := e.do(t, bob, "DELETE", "/api/backends/x/folders/bulk", nil)
	expect(t, r, 200)
	var out struct{ Deleted int }
	r.json(t, &out)
	if out.Deleted != 25 {
		t.Errorf("deleted = %d, want 25", out.Deleted)
	}
}

// ADR 0038 / the brief: the role is verified independently, not trusted from the header. A
// valid token for a low-privilege user, with a forged X-Booth-Role, gains nothing - on the
// admin API or any write route.
func TestForgedRoleHeaderGainsNothing(t *testing.T) {
	e := newTestEnv(t)
	dir := e.registerFS(t, "acme", "x")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, c := range []caller{{tokCarol, "acme", "owner"}, {tokCarol, "acme", "editor"}} { // carol is a viewer
		t.Run("as "+c.role, func(t *testing.T) {
			expect(t, e.do(t, c, "GET", "/api/admin/backends", nil), 403)
			expect(t, e.do(t, c, "POST", "/api/admin/backends", map[string]any{"id": "evil"}), 403)
			expect(t, e.do(t, c, "PUT", "/api/backends/x/objects/planted.txt", "x"), 403)
			expect(t, e.do(t, c, "DELETE", "/api/backends/x/objects/f.txt", nil), 403)
			expect(t, e.do(t, c, "DELETE", "/api/backends/x/folders/anything", nil), 403)
			expect(t, e.do(t, c, "POST", "/api/backends/x/folders", map[string]any{"path": "d"}), 403)
			expect(t, e.do(t, c, "POST", "/api/backends/x/move", map[string]any{"from": "f.txt", "to": "g.txt"}), 403)
			// ADR 0041: not merely "no privileged access" — the forged request is rejected
			// outright, reads included.
			expect(t, e.do(t, c, "GET", "/api/backends/x/objects/f.txt", nil), 403)
			expect(t, e.do(t, c, "GET", "/api/backends", nil), 403)
		})
	}
	if data, err := os.ReadFile(filepath.Join(dir, "f.txt")); err != nil || string(data) != "keep" {
		t.Errorf("forged requests changed data: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "planted.txt")); err == nil {
		t.Error("a forged-role upload landed")
	}
}

func TestTokenWithoutMembershipIsRefusedEverywhere(t *testing.T) {
	e := newTestEnv(t)
	e.registerFS(t, "acme", "x")
	// dave is an owner of globex only; claiming acme (with any role) must fail.
	for _, role := range []string{"owner", "editor", "viewer", ""} {
		c := caller{tokDave, "acme", role}
		for _, rt := range []struct{ m, p string }{{"GET", "/api/backends"}, {"GET", "/api/backends/x/objects"}, {"GET", "/api/admin/backends"}, {"PUT", "/api/backends/x/objects/f"}} {
			if r := e.do(t, c, rt.m, rt.p, nil); r.Code != 403 {
				t.Errorf("role %q %s %s = %d, want 403 (token grants nothing in acme)", role, rt.m, rt.p, r.Code)
			}
		}
	}
}

// The forwarded header may narrow what the token grants (a gateway restricting a session),
// and an absent header means "use the token's grant". (It may never exceed it: ADR 0041.)
func TestForwardedRoleCanOnlyNarrow(t *testing.T) {
	e := newTestEnv(t)
	e.registerFS(t, "acme", "x")

	narrowed := caller{tokAlice, "acme", "viewer"} // alice owns acme; the gateway says viewer
	expect(t, e.do(t, narrowed, "GET", "/api/admin/backends", nil), 403)
	expect(t, e.do(t, narrowed, "PUT", "/api/backends/x/objects/f", "x"), 403)
	expect(t, e.do(t, narrowed, "GET", "/api/backends", nil), 200)

	noHeader := caller{tokAlice, "acme", ""}
	expect(t, e.do(t, noHeader, "GET", "/api/admin/backends", nil), 200) // the token alone says owner
}
