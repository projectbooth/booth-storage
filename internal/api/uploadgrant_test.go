package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/projectbooth/booth-storage/internal/registry"
)

// ADR 0117 point 8/8a: create-only uploads by an app, admitted through a live upload
// grant rather than the ordinary editor/owner CanWrite gate.

const (
	tokAppOne = "tok-app-one" // streamlit:acme:app1, viewer in acme
	tokAppTwo = "tok-app-two" // streamlit:acme:app2, viewer in acme
)

var (
	appOne = caller{tokAppOne, "acme", "viewer"}
	appTwo = caller{tokAppTwo, "acme", "viewer"}
)

// withAppTokens extends the stub verifier with two app workload tokens, alongside the
// usual human ones every other test in this package relies on.
func withAppTokens(d *Deps) {
	d.Verifier = stubVerifier{
		tokAlice:  claimsFor("alice", "acme", "owner"),
		tokBob:    claimsFor("bob", "acme", "editor"),
		tokCarol:  claimsFor("carol", "acme", "viewer"),
		tokDave:   claimsFor("dave", "globex", "owner"),
		tokAppOne: claimsFor("streamlit:acme:app1", "acme", "viewer"),
		tokAppTwo: claimsFor("streamlit:acme:app2", "acme", "viewer"),
	}
}

func newGrantTestEnv(t *testing.T) *testEnv {
	return newTestEnv(t, withAppTokens)
}

// ---- admin CRUD --------------------------------------------------------------

func TestUploadGrants_AdminLifecycle(t *testing.T) {
	e := newGrantTestEnv(t)
	e.registerFS(t, "acme", "lake")

	body := map[string]any{
		"principal": "streamlit:acme:app1", "backendId": "lake", "prefix": "incoming",
		"extensions": []string{"csv", "PNG"}, "maxBytes": 1024,
	}
	r := e.do(t, alice, "POST", "/api/admin/upload-grants", body)
	expect(t, r, 201)
	var created uploadGrantView
	r.json(t, &created)
	if created.Principal != "streamlit:acme:app1" || created.BackendID != "lake" || created.Prefix != "incoming/" {
		t.Errorf("created = %+v", created)
	}
	if len(created.Extensions) != 2 || created.Extensions[0] != "csv" || created.Extensions[1] != "png" {
		t.Errorf("extensions not normalized: %+v", created.Extensions)
	}
	if created.GrantedBy != "alice" {
		t.Errorf("grantedBy = %q, want alice", created.GrantedBy)
	}

	// List.
	r = e.do(t, alice, "GET", "/api/admin/upload-grants", nil)
	expect(t, r, 200)
	var list []uploadGrantView
	r.json(t, &list)
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v", list)
	}

	// Editor and viewer can't reach any of this (ADR 0036: owner-only admin view).
	for _, c := range []caller{bob, carol} {
		expect(t, e.do(t, c, "GET", "/api/admin/upload-grants", nil), 403)
		expect(t, e.do(t, c, "POST", "/api/admin/upload-grants", body), 403)
		expect(t, e.do(t, c, "DELETE", "/api/admin/upload-grants/"+created.ID, nil), 403)
		expect(t, e.do(t, c, "POST", "/api/admin/upload-grants/revoke-principal", map[string]any{"principal": "streamlit:acme:app1"}), 403)
	}

	// Delete.
	expect(t, e.do(t, alice, "DELETE", "/api/admin/upload-grants/"+created.ID, nil), 204)
	r = e.do(t, alice, "GET", "/api/admin/upload-grants", nil)
	r.json(t, &list)
	if len(list) != 0 {
		t.Errorf("after delete, list = %+v", list)
	}
	expect(t, e.do(t, alice, "DELETE", "/api/admin/upload-grants/"+created.ID, nil), 404)
}

func TestUploadGrants_CreateValidatesAndChecksBackendExists(t *testing.T) {
	e := newGrantTestEnv(t)
	e.registerFS(t, "acme", "lake")

	// Backend doesn't exist in this workspace.
	r := e.do(t, alice, "POST", "/api/admin/upload-grants", map[string]any{
		"principal": "streamlit:acme:app1", "backendId": "nope", "prefix": "a", "extensions": []string{"csv"}, "maxBytes": 1,
	})
	expect(t, r, 404)

	// Principal names the wrong workspace.
	r = e.do(t, alice, "POST", "/api/admin/upload-grants", map[string]any{
		"principal": "streamlit:globex:app1", "backendId": "lake", "prefix": "a", "extensions": []string{"csv"}, "maxBytes": 1,
	})
	expect(t, r, 422)
	var eb struct{ Error, Field string }
	r.json(t, &eb)
	if eb.Field != "principal" {
		t.Errorf("field = %q, want principal (message: %s)", eb.Field, eb.Error)
	}

	// No extensions.
	expect(t, e.do(t, alice, "POST", "/api/admin/upload-grants", map[string]any{
		"principal": "streamlit:acme:app1", "backendId": "lake", "prefix": "a", "maxBytes": 1,
	}), 422)

	// Malformed body.
	expect(t, e.do(t, alice, "POST", "/api/admin/upload-grants", "{not json"), 400)
}

func TestUploadGrants_SecondCreateForSameScopeReplaces(t *testing.T) {
	e := newGrantTestEnv(t)
	e.registerFS(t, "acme", "lake")
	in := map[string]any{"principal": "streamlit:acme:app1", "backendId": "lake", "prefix": "incoming", "extensions": []string{"csv"}, "maxBytes": 100}

	var first uploadGrantView
	e.do(t, alice, "POST", "/api/admin/upload-grants", in).json(t, &first)

	in["maxBytes"] = 999
	var second uploadGrantView
	r2 := e.do(t, alice, "POST", "/api/admin/upload-grants", in)
	expect(t, r2, 201)
	r2.json(t, &second)

	if second.ID == first.ID {
		t.Error("the replacement kept the old grant ID")
	}
	var list []uploadGrantView
	e.do(t, alice, "GET", "/api/admin/upload-grants", nil).json(t, &list)
	if len(list) != 1 || list[0].MaxBytes != 999 {
		t.Errorf("after replace, list = %+v", list)
	}
}

func TestUploadGrants_RevokePrincipalAcrossBackendsIsIdempotent(t *testing.T) {
	e := newGrantTestEnv(t)
	e.registerFS(t, "acme", "lake")
	e.registerFS(t, "acme", "other")

	e.do(t, alice, "POST", "/api/admin/upload-grants", map[string]any{
		"principal": "streamlit:acme:app1", "backendId": "lake", "prefix": "a", "extensions": []string{"csv"}, "maxBytes": 1,
	})
	e.do(t, alice, "POST", "/api/admin/upload-grants", map[string]any{
		"principal": "streamlit:acme:app1", "backendId": "other", "prefix": "b", "extensions": []string{"csv"}, "maxBytes": 1,
	})
	e.do(t, alice, "POST", "/api/admin/upload-grants", map[string]any{
		"principal": "streamlit:acme:app2", "backendId": "lake", "prefix": "c", "extensions": []string{"csv"}, "maxBytes": 1,
	})

	r := e.do(t, alice, "POST", "/api/admin/upload-grants/revoke-principal", map[string]any{"principal": "streamlit:acme:app1"})
	expect(t, r, 200)
	var out struct {
		Revoked int
		Items   []uploadGrantView
	}
	r.json(t, &out)
	if out.Revoked != 2 || len(out.Items) != 2 {
		t.Fatalf("revoke-principal = %+v", out)
	}

	var remaining []uploadGrantView
	e.do(t, alice, "GET", "/api/admin/upload-grants", nil).json(t, &remaining)
	if len(remaining) != 1 || remaining[0].Principal != "streamlit:acme:app2" {
		t.Errorf("remaining = %+v, want only app2's grant", remaining)
	}

	// Idempotent.
	r = e.do(t, alice, "POST", "/api/admin/upload-grants/revoke-principal", map[string]any{"principal": "streamlit:acme:app1"})
	expect(t, r, 200)
	out = struct {
		Revoked int
		Items   []uploadGrantView
	}{}
	r.json(t, &out)
	if out.Revoked != 0 || len(out.Items) != 0 {
		t.Errorf("second revoke-principal = %+v, want 0 and empty", out)
	}
}

// ---- admission: the create-only write itself --------------------------------

func (e *testEnv) grantUpload(t *testing.T, backendID, prefix string, extensions []string, maxBytes int) {
	t.Helper()
	r := e.do(t, alice, "POST", "/api/admin/upload-grants", map[string]any{
		"principal": "streamlit:acme:app1", "backendId": backendID, "prefix": prefix, "extensions": extensions, "maxBytes": maxBytes,
	})
	expect(t, r, 201)
}

func TestAdmission_AppCreateOnlyWriteSucceedsOnceThen412(t *testing.T) {
	e := newGrantTestEnv(t)
	e.registerFS(t, "acme", "lake")
	e.grantUpload(t, "lake", "incoming", []string{"csv"}, 1024)

	r := e.do(t, appOne, "PUT", "/api/backends/lake/objects/incoming/report.csv", "a,b\n1,2\n", "If-None-Match", "*")
	expect(t, r, 200)

	r = e.do(t, appOne, "PUT", "/api/backends/lake/objects/incoming/report.csv", "second attempt", "If-None-Match", "*")
	expect(t, r, 412)

	// The first write's bytes are the ones that survived.
	got := e.do(t, alice, "GET", "/api/backends/lake/objects/incoming/report.csv", nil)
	if got.Body.String() != "a,b\n1,2\n" {
		t.Errorf("stored content = %q, want the first write's bytes", got.Body.String())
	}
}

func TestAdmission_PlainPUTFromAppStays403(t *testing.T) {
	e := newGrantTestEnv(t)
	e.registerFS(t, "acme", "lake")
	e.grantUpload(t, "lake", "incoming", []string{"csv"}, 1024)

	// No If-None-Match header at all: a live grant exists, but a plain PUT is still
	// refused — admission is narrow to exactly the create-only call.
	r := e.do(t, appOne, "PUT", "/api/backends/lake/objects/incoming/report.csv", "x")
	expect(t, r, 403)
}

func TestAdmission_DeleteAndFolderOperationsFromAppStay403(t *testing.T) {
	e := newGrantTestEnv(t)
	e.registerFS(t, "acme", "lake")
	e.grantUpload(t, "lake", "incoming", []string{"csv"}, 1024)
	expect(t, e.do(t, bob, "PUT", "/api/backends/lake/objects/incoming/x.csv", "a"), 200)

	expect(t, e.do(t, appOne, "DELETE", "/api/backends/lake/objects/incoming/x.csv", nil), 403)
	expect(t, e.do(t, appOne, "DELETE", "/api/backends/lake/folders/incoming", nil), 403)
	expect(t, e.do(t, appOne, "POST", "/api/backends/lake/folders", map[string]any{"path": "d"}), 403)
	expect(t, e.do(t, appOne, "POST", "/api/backends/lake/move", map[string]any{"from": "incoming/x.csv", "to": "incoming/y.csv"}), 403)
	// Even with the create-only header, these other methods are untouched.
	expect(t, e.do(t, appOne, "DELETE", "/api/backends/lake/objects/incoming/x.csv", nil, "If-None-Match", "*"), 403)

	got := e.do(t, alice, "GET", "/api/backends/lake/objects/incoming/x.csv", nil)
	if got.Body.String() != "a" {
		t.Error("a rejected app request mutated the object")
	}
}

func TestAdmission_NoGrantRefuses403(t *testing.T) {
	e := newGrantTestEnv(t)
	e.registerFS(t, "acme", "lake")
	// appTwo holds no grant at all.
	r := e.do(t, appTwo, "PUT", "/api/backends/lake/objects/incoming/report.csv", "x", "If-None-Match", "*")
	expect(t, r, 403)
}

func TestAdmission_OutsidePrefixAndNestedPathRefuse403(t *testing.T) {
	e := newGrantTestEnv(t)
	e.registerFS(t, "acme", "lake")
	e.grantUpload(t, "lake", "incoming", []string{"csv"}, 1024)

	expect(t, e.do(t, appOne, "PUT", "/api/backends/lake/objects/other/report.csv", "x", "If-None-Match", "*"), 403)
	// Two segments below the grant's prefix, not exactly one.
	expect(t, e.do(t, appOne, "PUT", "/api/backends/lake/objects/incoming/sub/report.csv", "x", "If-None-Match", "*"), 403)
}

func TestAdmission_WrongExtensionIs415(t *testing.T) {
	e := newGrantTestEnv(t)
	e.registerFS(t, "acme", "lake")
	e.grantUpload(t, "lake", "incoming", []string{"csv"}, 1024)

	r := e.do(t, appOne, "PUT", "/api/backends/lake/objects/incoming/report.png", "x", "If-None-Match", "*")
	expect(t, r, 415)
}

func TestAdmission_OverGrantMaxBytesIs413(t *testing.T) {
	e := newGrantTestEnv(t)
	e.registerFS(t, "acme", "lake")
	e.grantUpload(t, "lake", "incoming", []string{"csv"}, 4)

	r := e.do(t, appOne, "PUT", "/api/backends/lake/objects/incoming/report.csv", "too long", "If-None-Match", "*")
	expect(t, r, 413)
	// Module-wide MaxUploadBytes, if larger or unset, must not override the grant's own cap.
}

func TestAdmission_GrantCapIsIndependentOfModuleWideLimit(t *testing.T) {
	e := newTestEnv(t, withAppTokens, func(d *Deps) { d.MaxUploadBytes = 1 << 20 }) // generous module-wide cap
	e.registerFS(t, "acme", "lake")
	e.grantUpload(t, "lake", "incoming", []string{"csv"}, 4) // narrow grant cap

	r := e.do(t, appOne, "PUT", "/api/backends/lake/objects/incoming/report.csv", "too long", "If-None-Match", "*")
	expect(t, r, 413)
}

// ADR 0117 point 8a: an S3 backend answers 501 until its create-only capability probe
// has passed, for every caller — not only an admitted app.
func TestAdmission_UnverifiedS3BackendIs501(t *testing.T) {
	e := newGrantTestEnv(t)
	cfg, _ := json.Marshal(map[string]any{"endpoint": "http://127.0.0.1:1", "bucket": "b", "pathStyle": true})
	if _, err := e.svc.Create(context.Background(), "acme", "seed", registry.CreateInput{
		ID: "s3lake", Kind: "s3", Config: cfg, Credentials: map[string]string{"accessKeyId": "A", "secretAccessKey": "B"},
	}); err != nil {
		t.Fatal(err)
	}
	e.grantUpload(t, "s3lake", "incoming", []string{"csv"}, 1024)

	r := e.do(t, appOne, "PUT", "/api/backends/s3lake/objects/incoming/report.csv", "x", "If-None-Match", "*")
	expect(t, r, 501)

	// And the same for an editor/owner explicitly asking for create-only, unrelated to
	// any grant: the rule isn't only about app tokens.
	r = e.do(t, bob, "PUT", "/api/backends/s3lake/objects/other.csv", "x", "If-None-Match", "*")
	expect(t, r, 501)
}

// Editors and owners may request create-only semantics too (ADR 0117 point 8 is a
// general PUT capability); it is simply never required of them.
func TestAdmission_EditorCreateOnlyWriteUnrelatedToGrants(t *testing.T) {
	e := newGrantTestEnv(t)
	e.registerFS(t, "acme", "lake")

	expect(t, e.do(t, bob, "PUT", "/api/backends/lake/objects/a.txt", "first", "If-None-Match", "*"), 200)
	expect(t, e.do(t, bob, "PUT", "/api/backends/lake/objects/a.txt", "second", "If-None-Match", "*"), 412)
	// An ordinary PUT (no header) still overwrites, unchanged.
	expect(t, e.do(t, bob, "PUT", "/api/backends/lake/objects/a.txt", "third"), 200)
	got := e.do(t, alice, "GET", "/api/backends/lake/objects/a.txt", nil)
	if got.Body.String() != "third" {
		t.Errorf("body = %q, want third", got.Body.String())
	}
}

// A human token's subject can never collide with an app principal's shape, so even a
// forged workspace/role combination gains nothing against the admission check itself.
func TestAdmission_HumanTokenNeverMatchesAnAppGrant(t *testing.T) {
	e := newGrantTestEnv(t)
	e.registerFS(t, "acme", "lake")
	e.grantUpload(t, "lake", "incoming", []string{"csv"}, 1024)

	// carol (viewer, acme) has the right role shape but the wrong subject.
	r := e.do(t, carol, "PUT", "/api/backends/lake/objects/incoming/report.csv", "x", "If-None-Match", "*")
	expect(t, r, 403)
	if strings.Contains(r.errorMessage(), "editors and owners") {
		t.Error("expected the grant-admission 403, not the ordinary CanWrite message, for a viewer with the right header")
	}
}
