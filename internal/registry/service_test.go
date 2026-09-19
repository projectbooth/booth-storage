package registry

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/projectbooth/booth-storage/internal/backend"
)

type env struct {
	svc    *Service
	meta   *MemoryStore
	creds  *MemoryCredentials
	fsRoot string
}

// newEnv builds a Service over in-memory stores with one allowed filesystem root that
// is private per workspace ({workspace} placeholder).
func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{meta: NewMemoryStore(), creds: NewMemoryCredentials(), fsRoot: root}
	e.svc = NewService(e.meta, e.creds, FilesystemPolicy{Roots: []string{filepath.Join(root, WorkspacePlaceholder)}})
	return e
}

// wsDir makes (and returns) a directory inside a workspace's private filesystem root.
func (e *env) wsDir(t *testing.T, ws, name string) string {
	t.Helper()
	dir := filepath.Join(e.fsRoot, ws, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fsConfig(dir string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"rootPath": dir})
	return b
}

var s3Config = json.RawMessage(`{"endpoint":"http://minio:9000","bucket":"data","pathStyle":true}`)

func s3Creds() map[string]string {
	return map[string]string{"accessKeyId": "AKIAEXAMPLE", "secretAccessKey": "super-secret-value"}
}

// fakeServiceAccountJSON returns a structurally valid service-account key backed by a
// throwaway RSA key generated for this test, so GCS registration can be exercised
// without depending on Application Default Credentials in the test environment (and
// without any real credential in the repository).
func fakeServiceAccountJSON(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	out, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "test", "private_key_id": "k1",
		"private_key": string(pemKey), "client_email": "test@test.iam.gserviceaccount.com",
		"token_uri": "https://oauth2.googleapis.com/token",
	})
	return string(out)
}

func asValidation(t *testing.T, err error) *ValidationError {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v (%T), want *ValidationError", err, err)
	}
	return ve
}

// ADR 0035: a workspace holds several backends at once — of different kinds — each
// addressable by its own ID, with no "current" one.
func TestService_MultipleSimultaneousBackends(t *testing.T) {
	e := newEnv(t)

	if _, err := e.svc.Create(testCtx, "acme", "alice", CreateInput{ID: "scratch", Kind: backend.KindFilesystem, Config: fsConfig(e.wsDir(t, "acme", "scratch"))}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Create(testCtx, "acme", "alice", CreateInput{ID: "lake", DisplayName: "Data lake", Kind: backend.KindS3, Config: s3Config, Credentials: s3Creds()}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Create(testCtx, "acme", "alice", CreateInput{
		ID: "archive", Kind: backend.KindAzure,
		Config:      json.RawMessage(`{"accountName":"acmestore","container":"cold"}`),
		Credentials: map[string]string{"sasToken": "sv=2022&sig=abc"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Create(testCtx, "acme", "alice", CreateInput{
		ID: "models", Kind: backend.KindGCS,
		Config:      json.RawMessage(`{"bucket":"acme-models"}`),
		Credentials: map[string]string{"serviceAccountJson": fakeServiceAccountJSON(t)},
	}); err != nil {
		t.Fatal(err)
	}

	list, err := e.svc.List(testCtx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range list {
		got = append(got, string(r.Kind)+":"+r.ID)
	}
	if strings.Join(got, ",") != "azure:archive,s3:lake,gcs:models,filesystem:scratch" {
		t.Errorf("registered backends = %v; all four kinds should coexist, ordered by ID", got)
	}

	// Each is independently addressable by ID.
	for _, id := range []string{"scratch", "lake", "archive", "models"} {
		if _, err := e.svc.Get(testCtx, "acme", id); err != nil {
			t.Errorf("Get(%s): %v", id, err)
		}
	}
}

func TestService_WorkspacesAreIsolated(t *testing.T) {
	e := newEnv(t)
	for _, ws := range []string{"acme", "globex"} {
		if _, err := e.svc.Create(testCtx, ws, "u", CreateInput{ID: "lake", Kind: backend.KindS3, Config: s3Config, Credentials: s3Creds()}); err != nil {
			t.Fatalf("%s: %v", ws, err)
		}
	}
	if err := e.svc.Delete(testCtx, "acme", "lake"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Get(testCtx, "globex", "lake"); err != nil {
		t.Errorf("deleting acme's backend removed globex's: %v", err)
	}
	if _, err := e.creds.Get(testCtx, "globex", "lake"); err != nil {
		t.Errorf("deleting acme's backend removed globex's credentials: %v", err)
	}
	if _, err := e.svc.Get(testCtx, "acme", "lake"); !errors.Is(err, ErrNotFound) {
		t.Errorf("acme/lake still present: %v", err)
	}
}

// ADR 0020: credentials go to the credential store, never into metadata — not in the
// persisted record, not in its config, not in what the service returns.
func TestService_CredentialsNeverLandInMetadata(t *testing.T) {
	e := newEnv(t)
	rec, err := e.svc.Create(testCtx, "acme", "alice", CreateInput{ID: "lake", Kind: backend.KindS3, Config: s3Config, Credentials: s3Creds()})
	if err != nil {
		t.Fatal(err)
	}

	stored, _ := e.meta.Get(testCtx, "acme", "lake")
	for name, blob := range map[string][]byte{"returned": mustJSON(t, rec), "persisted": mustJSON(t, stored)} {
		for _, secret := range []string{"super-secret-value", "AKIAEXAMPLE"} {
			if bytes.Contains(blob, []byte(secret)) {
				t.Errorf("%s record contains credential value %q: %s", name, secret, blob)
			}
		}
	}
	if !rec.HasCredentials {
		t.Error("HasCredentials should be true")
	}
	if got, _ := e.creds.Get(testCtx, "acme", "lake"); got["secretAccessKey"] != "super-secret-value" {
		t.Errorf("credential store has %v", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A duplicate-ID request must be rejected without clobbering the existing backend's
// credentials — the ordering bug this guards against is "write Secret, then discover
// the ID is taken".
func TestService_DuplicateCreateDoesNotOverwriteCredentials(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Create(testCtx, "acme", "alice", CreateInput{ID: "lake", Kind: backend.KindS3, Config: s3Config, Credentials: s3Creds()}); err != nil {
		t.Fatal(err)
	}

	attacker := map[string]string{"accessKeyId": "EVIL", "secretAccessKey": "evil-secret"}
	_, err := e.svc.Create(testCtx, "acme", "mallory", CreateInput{ID: "lake", Kind: backend.KindS3, Config: s3Config, Credentials: attacker})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Create error = %v, want ErrExists", err)
	}
	got, _ := e.creds.Get(testCtx, "acme", "lake")
	if got["accessKeyId"] != "AKIAEXAMPLE" {
		t.Errorf("existing credentials were overwritten by a rejected duplicate: %v", got)
	}
}

// brokenCreds is a credential store whose writes always fail, simulating an unavailable
// Kubernetes API.
type brokenCreds struct{ *MemoryCredentials }

func (brokenCreds) Put(context.Context, string, string, map[string]string) error {
	return errors.New("secrets API unavailable")
}

func TestService_CreateRollsBackRecordWhenCredentialWriteFails(t *testing.T) {
	meta := NewMemoryStore()
	svc := NewService(meta, &brokenCreds{NewMemoryCredentials()}, FilesystemPolicy{})

	_, err := svc.Create(testCtx, "acme", "alice", CreateInput{ID: "lake", Kind: backend.KindS3, Config: s3Config, Credentials: s3Creds()})
	if err == nil || !strings.Contains(err.Error(), "storing credentials") {
		t.Fatalf("Create error = %v, want a credential-storage failure", err)
	}
	if _, err := meta.Get(testCtx, "acme", "lake"); !errors.Is(err, ErrNotFound) {
		t.Errorf("record left behind without credentials: %v", err)
	}
	// And the ID is reusable afterwards.
	svc2 := NewService(meta, NewMemoryCredentials(), FilesystemPolicy{})
	if _, err := svc2.Create(testCtx, "acme", "alice", CreateInput{ID: "lake", Kind: backend.KindS3, Config: s3Config, Credentials: s3Creds()}); err != nil {
		t.Errorf("retry after rollback failed: %v", err)
	}
}

func TestService_UpdateKeepsStoredCredentialsWhenOmitted(t *testing.T) {
	e := newEnv(t)
	created, _ := e.svc.Create(testCtx, "acme", "alice", CreateInput{ID: "lake", Kind: backend.KindS3, Config: s3Config, Credentials: s3Creds()})

	name := "Renamed lake"
	rec, err := e.svc.Update(testCtx, "acme", "lake", UpdateInput{DisplayName: &name, Config: json.RawMessage(`{"endpoint":"http://minio:9000","bucket":"data2","pathStyle":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if rec.DisplayName != "Renamed lake" || !strings.Contains(string(rec.Config), "data2") {
		t.Errorf("update not applied: %+v", rec)
	}
	if !rec.UpdatedAt.After(created.UpdatedAt) {
		t.Error("UpdatedAt did not advance")
	}
	if got, _ := e.creds.Get(testCtx, "acme", "lake"); got["secretAccessKey"] != "super-secret-value" {
		t.Errorf("stored credentials were lost or changed by an update that omitted them: %v", got)
	}
	if rec.Kind != backend.KindS3 || rec.ID != "lake" {
		t.Error("kind/ID must be immutable")
	}
}

func TestService_UpdateReplacesAndClearsCredentials(t *testing.T) {
	e := newEnv(t)
	_, _ = e.svc.Create(testCtx, "acme", "alice", CreateInput{ID: "lake", Kind: backend.KindS3, Config: s3Config, Credentials: s3Creds()})

	rotated := map[string]string{"accessKeyId": "NEW", "secretAccessKey": "rotated"}
	if _, err := e.svc.Update(testCtx, "acme", "lake", UpdateInput{Credentials: rotated}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.creds.Get(testCtx, "acme", "lake"); got["accessKeyId"] != "NEW" {
		t.Errorf("credentials not rotated: %v", got)
	}

	// An explicit empty set means anonymous access (valid for S3) and removes the Secret.
	rec, err := e.svc.Update(testCtx, "acme", "lake", UpdateInput{Credentials: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if rec.HasCredentials {
		t.Error("HasCredentials should be false after clearing")
	}
	if _, err := e.creds.Get(testCtx, "acme", "lake"); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("credentials should be gone: %v", err)
	}
}

func TestService_UpdateRejectsInvalidChanges(t *testing.T) {
	e := newEnv(t)
	_, _ = e.svc.Create(testCtx, "acme", "alice", CreateInput{ID: "lake", Kind: backend.KindS3, Config: s3Config, Credentials: s3Creds()})

	if _, err := e.svc.Update(testCtx, "acme", "lake", UpdateInput{Config: json.RawMessage(`{"bucket":""}`)}); err == nil {
		t.Error("update to an invalid config succeeded")
	} else {
		asValidation(t, err)
	}
	if _, err := e.svc.Update(testCtx, "acme", "missing", UpdateInput{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update(missing) = %v", err)
	}
	// The failed update changed nothing.
	rec, _ := e.svc.Get(testCtx, "acme", "lake")
	if !strings.Contains(string(rec.Config), `"data"`) {
		t.Errorf("failed update modified the record: %s", rec.Config)
	}
}

func TestService_CreateValidation(t *testing.T) {
	e := newEnv(t)
	dir := e.wsDir(t, "acme", "ok")

	cases := []struct {
		name  string
		ws    string
		in    CreateInput
		field string
	}{
		{"empty id", "acme", CreateInput{Kind: backend.KindS3, Config: s3Config}, "id"},
		{"uppercase id", "acme", CreateInput{ID: "Lake", Kind: backend.KindS3, Config: s3Config}, "id"},
		{"id with slash", "acme", CreateInput{ID: "a/b", Kind: backend.KindS3, Config: s3Config}, "id"},
		{"id too long", "acme", CreateInput{ID: strings.Repeat("a", 64), Kind: backend.KindS3, Config: s3Config}, "id"},
		{"unknown kind", "acme", CreateInput{ID: "x", Kind: "ftp", Config: s3Config}, "kind"},
		{"unknown config field (typo)", "acme", CreateInput{ID: "x", Kind: backend.KindS3, Config: json.RawMessage(`{"buket":"b"}`)}, "config"},
		{"missing bucket", "acme", CreateInput{ID: "x", Kind: backend.KindS3, Config: json.RawMessage(`{}`)}, "config"},
		{"partial s3 credentials", "acme", CreateInput{ID: "x", Kind: backend.KindS3, Config: s3Config, Credentials: map[string]string{"accessKeyId": "A"}}, "credentials"},
		{"unknown credential key", "acme", CreateInput{ID: "x", Kind: backend.KindS3, Config: s3Config, Credentials: map[string]string{"password": "p"}}, "credentials"},
		{"azure with no credential", "acme", CreateInput{ID: "x", Kind: backend.KindAzure, Config: json.RawMessage(`{"accountName":"acct","container":"cont"}`)}, "credentials"},
		{"azure with both credentials", "acme", CreateInput{ID: "x", Kind: backend.KindAzure, Config: json.RawMessage(`{"accountName":"acct","container":"cont"}`), Credentials: map[string]string{"accountKey": "a", "sasToken": "b"}}, "credentials"},
		{"gcs external_account key", "acme", CreateInput{ID: "x", Kind: backend.KindGCS, Config: json.RawMessage(`{"bucket":"my-bucket"}`), Credentials: map[string]string{"serviceAccountJson": `{"type":"external_account"}`}}, "credentials.serviceAccountJson"},
		{"filesystem with credentials", "acme", CreateInput{ID: "x", Kind: backend.KindFilesystem, Config: fsConfig(dir), Credentials: map[string]string{"k": "v"}}, "credentials"},
		{"empty credential value", "acme", CreateInput{ID: "x", Kind: backend.KindS3, Config: s3Config, Credentials: map[string]string{"accessKeyId": "", "secretAccessKey": "s"}}, "credentials.accessKeyId"},
		{"bad workspace slug", "Acme/../x", CreateInput{ID: "x", Kind: backend.KindS3, Config: s3Config}, "workspace"},
		{"display name too long", "acme", CreateInput{ID: "x", DisplayName: strings.Repeat("n", 101), Kind: backend.KindS3, Config: s3Config}, "displayName"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.svc.Create(testCtx, tc.ws, "u", tc.in)
			if err == nil {
				t.Fatal("Create succeeded, want a validation error")
			}
			ve := asValidation(t, err)
			if tc.field != "" && ve.Field != tc.field {
				t.Errorf("error field = %q (%v), want %q", ve.Field, ve, tc.field)
			}
		})
	}
	if list, _ := e.svc.List(testCtx, "acme"); len(list) != 0 {
		t.Errorf("rejected requests left records behind: %v", list)
	}
}

func TestService_FilesystemPolicyEnforced(t *testing.T) {
	e := newEnv(t)
	mine := e.wsDir(t, "acme", "mine")
	theirs := e.wsDir(t, "globex", "theirs")

	if _, err := e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "mine", Kind: backend.KindFilesystem, Config: fsConfig(mine)}); err != nil {
		t.Fatalf("registering a directory inside the workspace's own root: %v", err)
	}

	refused := map[string]string{
		"another workspace's directory": theirs,
		"the shared parent":             e.fsRoot,
		"the filesystem root":           string(filepath.Separator),
		"a traversal out of the root":   filepath.Join(mine, "..", "..", "globex", "theirs"),
	}
	for name, dir := range refused {
		_, err := e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "bad", Kind: backend.KindFilesystem, Config: fsConfig(dir)})
		if err == nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		asValidation(t, err)
	}

	if _, err := e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "rel", Kind: backend.KindFilesystem, Config: json.RawMessage(`{"rootPath":"relative/dir"}`)}); err == nil {
		t.Error("relative rootPath accepted")
	}
}

func TestService_FilesystemDisabledByDefault(t *testing.T) {
	svc := NewService(NewMemoryStore(), NewMemoryCredentials(), FilesystemPolicy{})
	if svc.FilesystemEnabled() {
		t.Error("filesystem kind should be disabled until the operator configures roots")
	}
	_, err := svc.Create(testCtx, "acme", "u", CreateInput{ID: "x", Kind: backend.KindFilesystem, Config: fsConfig(t.TempDir())})
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("Create error = %v, want a 'not enabled' message", err)
	}
}

// The policy is re-applied when a backend is opened, not just at registration: an
// operator tightening the allowed roots must take effect on already-registered backends.
func TestService_OpenRechecksFilesystemPolicy(t *testing.T) {
	e := newEnv(t)
	dir := e.wsDir(t, "acme", "data")
	if _, err := e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "data", Kind: backend.KindFilesystem, Config: fsConfig(dir)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Open(testCtx, "acme", "data"); err != nil {
		t.Fatalf("Open under the original policy: %v", err)
	}

	tightened := NewService(e.meta, e.creds, FilesystemPolicy{Roots: []string{filepath.Join(e.fsRoot, "elsewhere")}})
	if _, err := tightened.Open(testCtx, "acme", "data"); err == nil {
		t.Error("Open succeeded after the operator removed the directory from the allowed roots")
	}
}

func TestService_OpenReadWriteEndToEnd(t *testing.T) {
	e := newEnv(t)
	dir := e.wsDir(t, "acme", "scratch")
	_, _ = e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "scratch", Kind: backend.KindFilesystem, Config: fsConfig(dir)})

	b, err := e.svc.Open(testCtx, "acme", "scratch")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(testCtx, "reports/q3.csv", strings.NewReader("a,b\n1,2\n"), backend.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := b.List(testCtx, backend.ListOptions{Recursive: true})
	if err != nil || len(res.Entries) != 1 || res.Entries[0].Path != "reports/q3.csv" {
		t.Errorf("List = %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "reports", "q3.csv")); err != nil {
		t.Errorf("object not written to the registered directory: %v", err)
	}

	if _, err := e.svc.Open(testCtx, "acme", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open(unregistered) = %v, want ErrNotFound", err)
	}
	if _, err := e.svc.Open(testCtx, "globex", "scratch"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open from another workspace = %v, want ErrNotFound", err)
	}
}

func TestService_OpenCachesUntilRecordChanges(t *testing.T) {
	e := newEnv(t)
	dir := e.wsDir(t, "acme", "data")
	_, _ = e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "data", Kind: backend.KindFilesystem, Config: fsConfig(dir)})

	first, _ := e.svc.Open(testCtx, "acme", "data")
	second, _ := e.svc.Open(testCtx, "acme", "data")
	if first != second {
		t.Error("repeated Open rebuilt the backend instead of reusing the cached one")
	}

	other := e.wsDir(t, "acme", "other")
	if _, err := e.svc.Update(testCtx, "acme", "data", UpdateInput{Config: fsConfig(other)}); err != nil {
		t.Fatal(err)
	}
	third, _ := e.svc.Open(testCtx, "acme", "data")
	if third == first {
		t.Error("Open kept serving the stale backend after the record was updated")
	}
}

func TestService_OpenReportsMissingCredentials(t *testing.T) {
	e := newEnv(t)
	_, _ = e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "lake", Kind: backend.KindS3, Config: s3Config, Credentials: s3Creds()})
	_ = e.creds.Delete(testCtx, "acme", "lake") // Secret removed out from under us

	_, err := e.svc.Open(testCtx, "acme", "lake")
	if err == nil || !strings.Contains(err.Error(), "credentials are missing") {
		t.Errorf("Open error = %v, want an actionable missing-credentials message", err)
	}
}

func TestService_DeleteNeverTouchesData(t *testing.T) {
	e := newEnv(t)
	dir := e.wsDir(t, "acme", "keep")
	_, _ = e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "keep", Kind: backend.KindFilesystem, Config: fsConfig(dir)})
	b, _ := e.svc.Open(testCtx, "acme", "keep")
	_, _ = b.Write(testCtx, "precious.txt", strings.NewReader("data"), backend.WriteOptions{})

	if err := e.svc.Delete(testCtx, "acme", "keep"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "precious.txt")); err != nil {
		t.Errorf("unregistering a backend destroyed its data: %v", err)
	}
	if err := e.svc.Delete(testCtx, "acme", "keep"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestService_TestConnection(t *testing.T) {
	e := newEnv(t)
	good := e.wsDir(t, "acme", "good")

	if err := e.svc.Test(testCtx, "acme", CreateInput{Kind: backend.KindFilesystem, Config: fsConfig(good)}); err != nil {
		t.Errorf("Test of a working backend: %v", err)
	}
	missing := filepath.Join(e.fsRoot, "acme", "does-not-exist")
	if err := e.svc.Test(testCtx, "acme", CreateInput{Kind: backend.KindFilesystem, Config: fsConfig(missing)}); err == nil {
		t.Error("Test of a nonexistent directory succeeded")
	}
	// Testing persists nothing.
	if list, _ := e.svc.List(testCtx, "acme"); len(list) != 0 {
		t.Errorf("Test left records behind: %v", list)
	}
	// Bad input is a validation error, not a connectivity failure.
	asValidation(t, e.svc.Test(testCtx, "acme", CreateInput{Kind: backend.KindS3, Config: json.RawMessage(`{}`)}))
}

func TestService_TestUpdateUsesStoredCredentials(t *testing.T) {
	e := newEnv(t)
	dir := e.wsDir(t, "acme", "d")
	_, _ = e.svc.Create(testCtx, "acme", "u", CreateInput{ID: "d", Kind: backend.KindFilesystem, Config: fsConfig(dir)})

	other := e.wsDir(t, "acme", "e")
	if err := e.svc.TestUpdate(testCtx, "acme", "d", UpdateInput{Config: fsConfig(other)}); err != nil {
		t.Errorf("TestUpdate: %v", err)
	}
	// It must not have saved the new config.
	rec, _ := e.svc.Get(testCtx, "acme", "d")
	if !strings.Contains(string(rec.Config), "/d") && !strings.Contains(string(rec.Config), `\\d`) {
		t.Errorf("TestUpdate persisted the tested config: %s", rec.Config)
	}
}

func TestLocation(t *testing.T) {
	cases := []struct {
		kind backend.Kind
		cfg  string
		want string
	}{
		{backend.KindS3, `{"bucket":"b","prefix":"x/y/"}`, "s3://b/x/y"},
		{backend.KindS3, `{"bucket":"b"}`, "s3://b"},
		{backend.KindAzure, `{"accountName":"acct","container":"c","prefix":"p"}`, "azure://acct/c/p"},
		{backend.KindGCS, `{"bucket":"g"}`, "gs://g"},
		{backend.KindFilesystem, `{"rootPath":"/data/x"}`, "/data/x"},
	}
	for _, tc := range cases {
		if got := Location(tc.kind, json.RawMessage(tc.cfg)); got != tc.want {
			t.Errorf("Location(%s, %s) = %q, want %q", tc.kind, tc.cfg, got, tc.want)
		}
	}
}

// A sanity check that the Kubernetes-backed store plugs into the service the way
// main wires it, and that no credential ever leaks into anything but the Secret.
func TestService_WithKubernetesCredentials(t *testing.T) {
	client := fake.NewSimpleClientset()
	svc := NewService(NewMemoryStore(), NewKubernetesCredentials(client, "booth-storage"), FilesystemPolicy{})

	if _, err := svc.Create(testCtx, "acme", "alice", CreateInput{ID: "lake", Kind: backend.KindS3, Config: s3Config, Credentials: s3Creds()}); err != nil {
		t.Fatal(err)
	}
	secret, err := client.CoreV1().Secrets("booth-storage").Get(testCtx, SecretName("acme", "lake"), metav1.GetOptions{})
	if err != nil || string(secret.Data["secretAccessKey"]) != "super-secret-value" {
		t.Fatalf("Secret not provisioned as ADR 0020 requires: %v %+v", err, secret)
	}
	if _, err := svc.Open(testCtx, "acme", "lake"); err != nil {
		t.Errorf("Open via Kubernetes credentials: %v", err)
	}
	if err := svc.Delete(testCtx, "acme", "lake"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Secrets("booth-storage").Get(testCtx, SecretName("acme", "lake"), metav1.GetOptions{}); err == nil {
		t.Error("Secret survived deleting its backend")
	}
}
