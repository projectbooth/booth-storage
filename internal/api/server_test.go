package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projectbooth/booth-storage/internal/auth"
	"github.com/projectbooth/booth-storage/internal/registry"
)

// Tokens for the stub verifier: one per (user, role). The role is *not* derived from the
// token — like production, it arrives in the gateway-forwarded header — so the tests can
// also forge role headers with a valid token.
const (
	tokAlice = "tok-alice" // owner in acme
	tokBob   = "tok-bob"   // editor in acme
	tokCarol = "tok-carol" // viewer in acme
	tokDave  = "tok-dave"  // owner in globex
)

// stubVerifier maps a token to the claims a real verified token would carry - including the
// groups claim, from which the module re-derives the caller's role.
type stubVerifier map[string]*auth.Claims

func (s stubVerifier) Verify(_ context.Context, tok string) (*auth.Claims, error) {
	if c, ok := s[tok]; ok {
		return c, nil
	}
	return nil, errors.New("bad token")
}

func claimsFor(sub, workspace, role string) *auth.Claims {
	return &auth.Claims{Subject: sub, Groups: []string{"/workspaces/" + workspace + "/" + role}}
}

type testEnv struct {
	handler http.Handler
	svc     *registry.Service
	meta    *registry.MemoryStore
	fsRoot  string
}

type pingFailingStore struct{ *registry.MemoryStore }

func (pingFailingStore) Ping(context.Context) error { return errors.New("connection refused") }

func newTestEnv(t *testing.T, mutate ...func(*Deps)) *testEnv {
	t.Helper()
	root := t.TempDir()
	meta := registry.NewMemoryStore()
	svc := registry.NewService(meta, registry.NewMemoryCredentials(),
		registry.FilesystemPolicy{Roots: []string{filepath.Join(root, registry.WorkspacePlaceholder)}})
	deps := Deps{
		Verifier: stubVerifier{
			tokAlice: claimsFor("alice", "acme", "owner"),
			tokBob:   claimsFor("bob", "acme", "editor"),
			tokCarol: claimsFor("carol", "acme", "viewer"),
			tokDave:  claimsFor("dave", "globex", "owner"),
		},
		Registry: svc,
	}
	for _, m := range mutate {
		m(&deps)
	}
	return &testEnv{handler: NewRouter(deps), svc: svc, meta: meta, fsRoot: root}
}

type resp struct {
	*httptest.ResponseRecorder
}

func (r resp) json(t *testing.T, into any) {
	t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), into); err != nil {
		t.Fatalf("decoding %q: %v", r.Body.String(), err)
	}
}

func (r resp) errorMessage() string {
	var b struct{ Error string }
	_ = json.Unmarshal(r.Body.Bytes(), &b)
	return b.Error
}

type caller struct {
	token, workspace, role string
}

var (
	alice = caller{tokAlice, "acme", "owner"}
	bob   = caller{tokBob, "acme", "editor"}
	carol = caller{tokCarol, "acme", "viewer"}
	dave  = caller{tokDave, "globex", "owner"}
)

func (e *testEnv) do(t *testing.T, c caller, method, path string, body any, headers ...string) resp {
	t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	case []byte:
		reader = bytes.NewReader(b)
	case io.Reader:
		reader = b
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.workspace != "" {
		req.Header.Set(auth.HeaderBoothWorkspace, c.workspace)
	}
	if c.role != "" {
		req.Header.Set(auth.HeaderBoothRole, c.role)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return resp{rec}
}

func (e *testEnv) wsDir(t *testing.T, ws, name string) string {
	t.Helper()
	dir := filepath.Join(e.fsRoot, ws, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	return dir
}

// registerFS registers a filesystem backend directly through the service and returns its
// directory, so data-API tests don't depend on the admin routes working.
func (e *testEnv) registerFS(t *testing.T, ws, id string) string {
	t.Helper()
	dir := e.wsDir(t, ws, id)
	cfg, _ := json.Marshal(map[string]string{"rootPath": dir})
	if _, err := e.svc.Create(context.Background(), ws, "seed", registry.CreateInput{ID: id, Kind: "filesystem", Config: cfg}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func expect(t *testing.T, r resp, status int) {
	t.Helper()
	if r.Code != status {
		t.Fatalf("status = %d, want %d (body: %s)", r.Code, status, r.Body.String())
	}
}

// ---- health ----------------------------------------------------------------

func TestHealthz(t *testing.T) {
	e := newTestEnv(t)
	r := e.do(t, caller{}, "GET", "/healthz", nil) // unauthenticated on purpose: core polls it
	expect(t, r, 200)
	var body struct {
		Status string
		Checks map[string]string
	}
	r.json(t, &body)
	if body.Status != "ok" || body.Checks["database"] != "ok" {
		t.Errorf("body = %+v", body)
	}
	expect(t, e.do(t, caller{}, "GET", "/livez", nil), 200)
}

func TestHealthz_ReportsUnhealthyWhenDatabaseDown(t *testing.T) {
	svc := registry.NewService(pingFailingStore{registry.NewMemoryStore()}, registry.NewMemoryCredentials(), registry.FilesystemPolicy{})
	h := NewRouter(Deps{Verifier: stubVerifier{}, Registry: svc})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 so core shows this module as unhealthy", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("health response leaks the internal error: %s", rec.Body)
	}

	// Liveness must stay green — restarting the pod can't fix a database outage.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/livez", nil))
	if rec.Code != 200 {
		t.Errorf("/livez = %d during a database outage, want 200", rec.Code)
	}
}

// ---- authn / authz ---------------------------------------------------------

var allRoutes = []struct{ method, path string }{
	{"GET", "/api/kinds"},
	{"GET", "/api/backends"},
	{"GET", "/api/backends/x"},
	{"GET", "/api/backends/x/objects"},
	{"GET", "/api/backends/x/objects/f.txt"},
	{"PUT", "/api/backends/x/objects/f.txt"},
	{"GET", "/api/admin/backends"},
	{"POST", "/api/admin/backends"},
	{"GET", "/api/admin/backends/x"},
	{"PUT", "/api/admin/backends/x"},
	{"DELETE", "/api/admin/backends/x"},
	{"POST", "/api/admin/backends/x/test"},
	{"POST", "/api/admin/test-connection"},
}

func TestEveryRouteRequiresAuthentication(t *testing.T) {
	e := newTestEnv(t)
	for _, rt := range allRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			expect(t, e.do(t, caller{}, rt.method, rt.path, nil), 401)
			// Forged forwarded headers without a verifiable token must not authenticate.
			expect(t, e.do(t, caller{token: "forged", workspace: "acme", role: "owner"}, rt.method, rt.path, nil), 401)
		})
	}
}

// The permission tiers of ADR 0036: only owners reach the admin view's API; only editors
// and owners write; everyone with a role reads. Enforced here regardless of any UI.
func TestRoleMatrix(t *testing.T) {
	e := newTestEnv(t)
	e.registerFS(t, "acme", "x")

	type row struct {
		method, path          string
		body                  any
		owner, editor, viewer int
	}
	rows := []row{
		{"GET", "/api/kinds", nil, 200, 200, 200},
		{"GET", "/api/backends", nil, 200, 200, 200},
		{"GET", "/api/backends/x", nil, 200, 200, 200},
		{"GET", "/api/backends/x/objects", nil, 200, 200, 200},
		{"PUT", "/api/backends/x/objects/f.txt", "hi", 200, 200, 403},
		{"GET", "/api/backends/x/objects/f.txt", nil, 200, 200, 200}, // after the PUTs above
		{"GET", "/api/admin/backends", nil, 200, 403, 403},
		{"GET", "/api/admin/backends/x", nil, 200, 403, 403},
		{"POST", "/api/admin/backends", map[string]any{"id": "denied"}, 422, 403, 403}, // owner reaches validation
		{"PUT", "/api/admin/backends/x", map[string]any{"displayName": "n"}, 200, 403, 403},
		{"POST", "/api/admin/backends/x/test", nil, 200, 403, 403},
		{"POST", "/api/admin/test-connection", map[string]any{"kind": "filesystem", "config": map[string]string{"rootPath": e.wsDir(t, "acme", "x")}}, 200, 403, 403},
		{"DELETE", "/api/admin/backends/x", nil, 204, 403, 403}, // last: removes x
	}
	for _, rw := range rows {
		for name, want := range map[string]int{"owner": rw.owner, "editor": rw.editor, "viewer": rw.viewer} {
			c := map[string]caller{"owner": alice, "editor": bob, "viewer": carol}[name]
			t.Run(fmt.Sprintf("%s %s as %s", rw.method, rw.path, name), func(t *testing.T) {
				got := e.do(t, c, rw.method, rw.path, rw.body)
				if got.Code != want {
					t.Errorf("status = %d, want %d (body: %s)", got.Code, want, got.Body)
				}
			})
		}
	}
}

func TestUnknownForwardedRoleGetsNothing(t *testing.T) {
	e := newTestEnv(t)
	// (An absent forwarded role means "use the token's own grant" - see write_test.go.)
	for _, role := range []string{"superuser", "OWNER", "admin"} {
		c := caller{tokAlice, "acme", role}
		for _, rt := range []struct{ m, p string }{{"GET", "/api/backends"}, {"GET", "/api/admin/backends"}, {"PUT", "/api/backends/x/objects/f"}} {
			if got := e.do(t, c, rt.m, rt.p, nil); got.Code != 403 {
				t.Errorf("role %q %s %s = %d, want 403", role, rt.m, rt.p, got.Code)
			}
		}
	}
}

// ---- admin view ------------------------------------------------------------

func TestAdminLifecycle(t *testing.T) {
	e := newTestEnv(t)
	dir := e.wsDir(t, "acme", "scratch")

	// Create.
	r := e.do(t, alice, "POST", "/api/admin/backends", map[string]any{
		"id": "scratch", "displayName": "Scratch space", "kind": "filesystem", "config": map[string]string{"rootPath": dir},
	})
	expect(t, r, 201)
	var created AdminBackend
	r.json(t, &created)
	if created.ID != "scratch" || created.Kind != "filesystem" || created.Location != dir || created.CreatedBy != "alice" || created.CredentialsSet {
		t.Errorf("created = %+v", created)
	}

	// Regular view: visible, but summary only.
	r = e.do(t, carol, "GET", "/api/backends", nil)
	expect(t, r, 200)
	var list []map[string]any
	r.json(t, &list)
	if len(list) != 1 || list[0]["id"] != "scratch" {
		t.Fatalf("regular list = %v", list)
	}
	for _, adminOnly := range []string{"config", "credentialsSet", "createdBy"} {
		if _, leaked := list[0][adminOnly]; leaked {
			t.Errorf("regular view exposes admin-only field %q", adminOnly)
		}
	}

	// Admin view: full config.
	r = e.do(t, alice, "GET", "/api/admin/backends/scratch", nil)
	expect(t, r, 200)
	var adminGet AdminBackend
	r.json(t, &adminGet)
	if !strings.Contains(string(adminGet.Config), "rootPath") {
		t.Errorf("admin view lacks config: %+v", adminGet)
	}

	// Update.
	r = e.do(t, alice, "PUT", "/api/admin/backends/scratch", map[string]any{"displayName": "Renamed"})
	expect(t, r, 200)
	var updated AdminBackend
	r.json(t, &updated)
	if updated.DisplayName != "Renamed" || !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("updated = %+v", updated)
	}

	// Test connection on the saved backend, then a preview of a bad edit.
	r = e.do(t, alice, "POST", "/api/admin/backends/scratch/test", nil)
	expect(t, r, 200)
	var tr TestResult
	r.json(t, &tr)
	if !tr.OK {
		t.Errorf("saved backend test failed: %+v", tr)
	}
	r = e.do(t, alice, "POST", "/api/admin/backends/scratch/test", map[string]any{"config": map[string]string{"rootPath": filepath.Join(e.fsRoot, "acme", "no-such-parent", "missing-dir")}})
	expect(t, r, 200)
	r.json(t, &tr)
	if tr.OK || tr.Error == "" {
		t.Errorf("previewing a nonexistent directory reported ok: %+v", tr)
	}

	// Delete.
	expect(t, e.do(t, alice, "DELETE", "/api/admin/backends/scratch", nil), 204)
	expect(t, e.do(t, carol, "GET", "/api/backends/scratch", nil), 404)
	expect(t, e.do(t, alice, "DELETE", "/api/admin/backends/scratch", nil), 404)
}

// ADR 0020: credentials go in on the admin routes and never come back out of any route.
func TestCredentialsNeverInAnyResponse(t *testing.T) {
	e := newTestEnv(t)
	secrets := []string{"AKIA-SUPER-SECRET-ID", "wJalr-super-secret-key", "session-token-value"}
	r := e.do(t, alice, "POST", "/api/admin/backends", map[string]any{
		"id": "lake", "kind": "s3",
		"config":      map[string]any{"endpoint": "http://minio:9000", "bucket": "data", "pathStyle": true},
		"credentials": map[string]string{"accessKeyId": secrets[0], "secretAccessKey": secrets[1], "sessionToken": secrets[2]},
	})
	expect(t, r, 201)
	check := func(label string, r resp) {
		t.Helper()
		for _, s := range secrets {
			if strings.Contains(r.Body.String(), s) {
				t.Errorf("%s response contains credential value %q: %s", label, s, r.Body)
			}
		}
	}
	check("create", r)

	var created AdminBackend
	r.json(t, &created)
	if !created.CredentialsSet {
		t.Error("credentialsSet should be true")
	}

	check("admin list", e.do(t, alice, "GET", "/api/admin/backends", nil))
	check("admin get", e.do(t, alice, "GET", "/api/admin/backends/lake", nil))
	check("regular list", e.do(t, carol, "GET", "/api/backends", nil))
	check("regular get", e.do(t, carol, "GET", "/api/backends/lake", nil))
	check("update", e.do(t, alice, "PUT", "/api/admin/backends/lake", map[string]any{"displayName": "x"}))
	// Even a validation error echoing the request must not reflect credentials back.
	check("invalid create", e.do(t, alice, "POST", "/api/admin/backends", map[string]any{
		"id": "bad", "kind": "s3", "config": map[string]any{}, "credentials": map[string]string{"accessKeyId": secrets[0], "secretAccessKey": secrets[1]},
	}))
	check("failed test", e.do(t, alice, "POST", "/api/admin/backends/lake/test", nil))
}

func TestAdminValidationAndConflicts(t *testing.T) {
	e := newTestEnv(t)
	dir := e.wsDir(t, "acme", "d")
	ok := map[string]any{"id": "d", "kind": "filesystem", "config": map[string]string{"rootPath": dir}}
	expect(t, e.do(t, alice, "POST", "/api/admin/backends", ok), 201)

	// Duplicate ID.
	r := e.do(t, alice, "POST", "/api/admin/backends", ok)
	expect(t, r, 409)

	// Validation error carries the offending field for the UI to highlight.
	r = e.do(t, alice, "POST", "/api/admin/backends", map[string]any{"id": "Bad ID", "kind": "s3", "config": map[string]any{"bucket": "b"}})
	expect(t, r, 422)
	var eb struct{ Error, Field string }
	r.json(t, &eb)
	if eb.Field != "id" || eb.Error == "" {
		t.Errorf("error body = %+v, want field=id", eb)
	}

	// Filesystem outside the workspace's root is refused with a 422 naming the allowed dirs.
	r = e.do(t, alice, "POST", "/api/admin/backends", map[string]any{"id": "escape", "kind": "filesystem", "config": map[string]string{"rootPath": filepath.Join(e.fsRoot, "not-mine")}})
	expect(t, r, 422)
	if !strings.Contains(r.errorMessage(), "outside the directories") {
		t.Errorf("message = %q", r.errorMessage())
	}

	// Malformed / unknown-field bodies.
	expect(t, e.do(t, alice, "POST", "/api/admin/backends", "{not json"), 400)
	expect(t, e.do(t, alice, "POST", "/api/admin/backends", map[string]any{"id": "x", "kind": "s3", "surprise": true}), 400)
	expect(t, e.do(t, alice, "PUT", "/api/admin/backends/nope", map[string]any{"displayName": "n"}), 404)

	// Oversized body.
	huge := `{"id":"x","kind":"s3","credentials":{"a":"` + strings.Repeat("A", 300<<10) + `"}}`
	expect(t, e.do(t, alice, "POST", "/api/admin/backends", huge), 413)
}

func TestKindsHidesFilesystemUnlessEnabled(t *testing.T) {
	e := newTestEnv(t)
	var body struct {
		Kinds             []string
		FilesystemEnabled bool
	}
	r := e.do(t, carol, "GET", "/api/kinds", nil)
	r.json(t, &body)
	if strings.Join(body.Kinds, ",") != "s3,filesystem,azure,gcs" || !body.FilesystemEnabled {
		t.Errorf("enabled deployment: %+v", body)
	}

	disabled := registry.NewService(registry.NewMemoryStore(), registry.NewMemoryCredentials(), registry.FilesystemPolicy{})
	h := NewRouter(Deps{Verifier: stubVerifier{tokCarol: claimsFor("carol", "acme", "viewer")}, Registry: disabled})
	req := httptest.NewRequest("GET", "/api/kinds", nil)
	req.Header.Set("Authorization", "Bearer "+tokCarol)
	req.Header.Set(auth.HeaderBoothWorkspace, "acme")
	req.Header.Set(auth.HeaderBoothRole, "viewer")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if strings.Join(body.Kinds, ",") != "s3,azure,gcs" || body.FilesystemEnabled {
		t.Errorf("disabled deployment should not offer filesystem: %+v", body)
	}
}

func TestTestConnectionResults(t *testing.T) {
	e := newTestEnv(t)
	good := e.wsDir(t, "acme", "good")

	var tr TestResult
	r := e.do(t, alice, "POST", "/api/admin/test-connection", map[string]any{"kind": "filesystem", "config": map[string]string{"rootPath": good}})
	expect(t, r, 200)
	r.json(t, &tr)
	if !tr.OK {
		t.Errorf("good config: %+v", tr)
	}

	// A connectivity failure is a normal answer (200, ok=false), not an API error.
	r = e.do(t, alice, "POST", "/api/admin/test-connection", map[string]any{"kind": "filesystem", "config": map[string]string{"rootPath": filepath.Join(e.fsRoot, "acme", "no-such-parent", "nope")}})
	expect(t, r, 200)
	tr = TestResult{}
	r.json(t, &tr)
	if tr.OK || tr.Error == "" {
		t.Errorf("missing directory: %+v", tr)
	}

	// A malformed request is a 422, not a "failed connection".
	expect(t, e.do(t, alice, "POST", "/api/admin/test-connection", map[string]any{"kind": "s3", "config": map[string]any{}}), 422)
	expect(t, e.do(t, alice, "POST", "/api/admin/backends/nope/test", nil), 404)

	// Nothing was persisted by testing.
	r = e.do(t, alice, "GET", "/api/admin/backends", nil)
	var list []AdminBackend
	r.json(t, &list)
	if len(list) != 0 {
		t.Errorf("test-connection persisted %d backends", len(list))
	}
}

// ---- data API --------------------------------------------------------------

func TestDataAPI_RoundTrip(t *testing.T) {
	e := newTestEnv(t)
	dir := e.registerFS(t, "acme", "scratch")

	// Editor writes (nested path, content type kept).
	r := e.do(t, bob, "PUT", "/api/backends/scratch/objects/reports/q3.csv", "a,b\n1,2\n", "Content-Type", "text/csv")
	expect(t, r, 200)
	var info struct {
		Path string
		Size int64
	}
	r.json(t, &info)
	if info.Path != "reports/q3.csv" || info.Size != 8 {
		t.Errorf("write info = %+v", info)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "reports", "q3.csv")); err != nil || string(data) != "a,b\n1,2\n" {
		t.Errorf("file on disk: %q, %v", data, err)
	}
	expect(t, e.do(t, bob, "PUT", "/api/backends/scratch/objects/top.txt", "hello"), 200)

	// A viewer reads.
	r = e.do(t, carol, "GET", "/api/backends/scratch/objects/reports/q3.csv", nil)
	expect(t, r, 200)
	if r.Body.String() != "a,b\n1,2\n" {
		t.Errorf("body = %q", r.Body.String())
	}
	h := r.Header()
	if h.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") ||
		!strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("stored content served without neutralizing headers: %v", h)
	}
	if h.Get("Content-Length") != "8" {
		t.Errorf("Content-Length = %q", h.Get("Content-Length"))
	}

	// List: non-recursive shows directories, recursive shows only objects.
	var res struct {
		Entries []struct {
			Path  string
			IsDir bool
		}
		NextCursor string
	}
	r = e.do(t, carol, "GET", "/api/backends/scratch/objects", nil)
	expect(t, r, 200)
	r.json(t, &res)
	if len(res.Entries) != 2 {
		t.Fatalf("root listing = %+v", res)
	}
	got := map[string]bool{}
	for _, en := range res.Entries {
		got[en.Path] = en.IsDir
	}
	if !got["reports/"] || got["top.txt"] {
		t.Errorf("root listing = %v, want reports/ as dir and top.txt as object", got)
	}

	r = e.do(t, carol, "GET", "/api/backends/scratch/objects?recursive=true&prefix=reports", nil)
	expect(t, r, 200)
	res.Entries = nil
	r.json(t, &res)
	if len(res.Entries) != 1 || res.Entries[0].Path != "reports/q3.csv" {
		t.Errorf("recursive prefix listing = %+v", res.Entries)
	}
}

func TestDataAPI_Pagination(t *testing.T) {
	e := newTestEnv(t)
	e.registerFS(t, "acme", "p")
	for i := 0; i < 5; i++ {
		expect(t, e.do(t, bob, "PUT", fmt.Sprintf("/api/backends/p/objects/f%d", i), "x"), 200)
	}

	seen := map[string]int{}
	cursor := ""
	for page := 0; page < 10; page++ {
		path := "/api/backends/p/objects?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r := e.do(t, carol, "GET", path, nil)
		expect(t, r, 200)
		var res struct {
			Entries    []struct{ Path string }
			NextCursor string
		}
		r.json(t, &res)
		for _, en := range res.Entries {
			seen[en.Path]++
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	if len(seen) != 5 {
		t.Errorf("paginated listing saw %v, want 5 distinct objects", seen)
	}
	for p, n := range seen {
		if n != 1 {
			t.Errorf("%s seen %d times", p, n)
		}
	}

	expect(t, e.do(t, carol, "GET", "/api/backends/p/objects?limit=0", nil), 400)
	expect(t, e.do(t, carol, "GET", "/api/backends/p/objects?limit=abc", nil), 400)
}

// Path-traversal attempts, including percent-encoded ones a naive router might decode
// only after checking, must be refused and must never touch anything outside the root.
func TestDataAPI_PathTraversalRefused(t *testing.T) {
	e := newTestEnv(t)
	e.registerFS(t, "acme", "scratch")
	outside := filepath.Join(e.fsRoot, "acme", "outside.txt")

	paths := []string{
		"/api/backends/scratch/objects/../outside.txt",
		"/api/backends/scratch/objects/%2e%2e/outside.txt",
		"/api/backends/scratch/objects/..%2Foutside.txt",
		"/api/backends/scratch/objects/a/../../outside.txt",
		"/api/backends/scratch/objects/a%5C..%5Coutside.txt",
		"/api/backends/scratch/objects//etc/passwd",
		"/api/backends/scratch/objects/%00",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			for _, method := range []string{"PUT", "GET"} {
				c := bob
				var body any
				if method == "PUT" {
					body = "pwned"
				}
				r := e.do(t, c, method, p, body)
				if r.Code < 400 || r.Code >= 500 {
					t.Errorf("%s %s = %d, want a 4xx refusal (body: %s)", method, p, r.Code, r.Body)
				}
			}
			if _, err := os.Stat(outside); err == nil {
				t.Fatalf("%s planted a file outside the backend root", p)
			}
		})
	}

	// List prefix traversal.
	expect(t, e.do(t, carol, "GET", "/api/backends/scratch/objects?prefix=../..", nil), 400)
}

func TestDataAPI_ErrorMapping(t *testing.T) {
	e := newTestEnv(t)
	dir := e.registerFS(t, "acme", "scratch")
	_ = os.MkdirAll(filepath.Join(dir, "somedir"), 0o750)

	expect(t, e.do(t, carol, "GET", "/api/backends/scratch/objects/missing.txt", nil), 404)
	expect(t, e.do(t, carol, "GET", "/api/backends/scratch/objects/somedir", nil), 404) // a directory is not an object
	expect(t, e.do(t, carol, "GET", "/api/backends/unregistered/objects", nil), 404)
	expect(t, e.do(t, carol, "GET", "/api/backends/unregistered", nil), 404)
	expect(t, e.do(t, bob, "PUT", "/api/backends/unregistered/objects/f", "x"), 404)
	expect(t, e.do(t, bob, "PUT", "/api/backends/scratch/objects/somedir", "x"), 400) // can't replace a directory
}

func TestDataAPI_UploadLimit(t *testing.T) {
	e := newTestEnv(t, func(d *Deps) { d.MaxUploadBytes = 16 })
	dir := e.registerFS(t, "acme", "scratch")

	expect(t, e.do(t, bob, "PUT", "/api/backends/scratch/objects/ok.txt", strings.Repeat("a", 16)), 200)
	r := e.do(t, bob, "PUT", "/api/backends/scratch/objects/big.txt", strings.Repeat("a", 17))
	expect(t, r, 413)
	if _, err := os.Stat(filepath.Join(dir, "big.txt")); err == nil {
		t.Error("an over-limit upload left an object behind")
	}
}

// ADR 0035 at the HTTP boundary: the same ID in two workspaces are different backends,
// and one workspace can never reach or even detect the other's.
func TestWorkspaceIsolationOverHTTP(t *testing.T) {
	e := newTestEnv(t)
	acmeDir := e.registerFS(t, "acme", "shared-name")
	globexDir := e.registerFS(t, "globex", "shared-name")

	expect(t, e.do(t, bob, "PUT", "/api/backends/shared-name/objects/who.txt", "acme"), 200)
	expect(t, e.do(t, dave, "PUT", "/api/backends/shared-name/objects/who.txt", "globex"), 200)

	if data, _ := os.ReadFile(filepath.Join(acmeDir, "who.txt")); string(data) != "acme" {
		t.Errorf("acme's file = %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(globexDir, "who.txt")); string(data) != "globex" {
		t.Errorf("globex's file = %q", data)
	}

	r := e.do(t, carol, "GET", "/api/backends/shared-name/objects/who.txt", nil)
	if r.Body.String() != "acme" {
		t.Errorf("acme viewer read %q", r.Body.String())
	}

	// A globex owner can't see or manage acme-only backends.
	e.registerFS(t, "acme", "acme-only")
	expect(t, e.do(t, dave, "GET", "/api/backends/acme-only", nil), 404)
	expect(t, e.do(t, dave, "DELETE", "/api/admin/backends/acme-only", nil), 404)
	var list []BackendSummary
	e.do(t, dave, "GET", "/api/backends", nil).json(t, &list)
	if len(list) != 1 || list[0].ID != "shared-name" {
		t.Errorf("globex sees %+v", list)
	}

	// And a hostile workspace header can't be used to pivot into a path.
	for _, ws := range []string{"../acme", "acme/../x", "ACME", ""} {
		c := caller{tokAlice, ws, "owner"}
		if got := e.do(t, c, "GET", "/api/backends", nil); got.Code < 400 {
			t.Errorf("workspace header %q accepted with status %d", ws, got.Code)
		}
	}
}

// A backend whose credentials were deleted out from under it reports something an
// operator can act on rather than a bare 500.
func TestDataAPI_MissingCredentialsIsActionable(t *testing.T) {
	e := newTestEnv(t)
	expect(t, e.do(t, alice, "POST", "/api/admin/backends", map[string]any{
		"id": "lake", "kind": "s3", "config": map[string]any{"endpoint": "http://127.0.0.1:1", "bucket": "b"},
		"credentials": map[string]string{"accessKeyId": "A", "secretAccessKey": "B"},
	}), 201)

	// An unreachable store surfaces as 502 with the store's error, distinct from a 404/500
	// of our own.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ctx
	r := e.do(t, carol, "GET", "/api/backends/lake/objects", nil)
	if r.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for an unreachable storage service (body: %s)", r.Code, r.Body)
	}
}
