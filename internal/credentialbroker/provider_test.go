package credentialbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/projectbooth/booth-storage/internal/backend"
	"github.com/projectbooth/booth-storage/internal/backend/s3"
	"github.com/projectbooth/booth-storage/internal/registry"
)

const testCredential = "bcbp.storage.test-mac-value"

func newTestRegistry(t *testing.T) *registry.Service {
	t.Helper()
	return registry.NewService(registry.NewMemoryStore(), registry.NewMemoryCredentials(), registry.FilesystemPolicy{})
}

// registerRawBackend writes a Record straight to a MemoryStore, bypassing the Service's own
// kind-specific config validation and filesystem allow-list gating — for setting up a
// registered *non*-s3 backend, whose config shape this test has no reason to construct
// correctly (only the provider's own kind check is under test).
func registerRawBackend(t *testing.T, store *registry.MemoryStore, workspace, id string, kind backend.Kind) {
	t.Helper()
	if err := store.Create(context.Background(), registry.Record{
		Workspace: workspace, ID: id, DisplayName: id, Kind: kind, Config: json.RawMessage(`{}`),
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func registerS3Backend(t *testing.T, reg *registry.Service, workspace, id string, cfg s3.Config, creds map[string]string) {
	t.Helper()
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(context.Background(), workspace, "tester", registry.CreateInput{
		ID: id, DisplayName: id, Kind: backend.KindS3, Config: cfgJSON, Credentials: creds,
	}); err != nil {
		t.Fatalf("registering backend: %v", err)
	}
}

// fixedMint returns a canned successful mint, recording the request it was called with.
func fixedMint(t *testing.T, want *s3.MintRequest) func(context.Context, s3.Config, s3.Credentials, s3.MintRequest) (s3.MintedCredential, error) {
	t.Helper()
	return func(_ context.Context, cfg s3.Config, admin s3.Credentials, req s3.MintRequest) (s3.MintedCredential, error) {
		if want != nil {
			*want = req
		}
		return s3.MintedCredential{
			AccessKeyID: "minted-access-key", SecretAccessKey: "minted-secret-key",
			Endpoint: cfg.Endpoint, Region: cfg.Region, Bucket: cfg.Bucket, KeyPrefix: "lake/table1", PathStyle: cfg.PathStyle,
			ExpiresAt: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC),
		}, nil
	}
}

func doRequest(t *testing.T, h http.Handler, credential string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, ProviderPath, bytes.NewReader(raw))
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestProvider_Authentication(t *testing.T) {
	reg := newTestRegistry(t)
	h := NewHandler(Deps{Credential: testCredential, Registry: reg})
	body := map[string]any{"kind": "s3", "access": "read", "ttlSeconds": 300, "scope": map[string]string{"backendId": "b1", "path": ""}, "requester": map[string]string{"subject": "job:1", "workspace": "acme", "role": "editor"}}

	cases := []struct {
		name       string
		credential string
		want       int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong credential", "not-the-real-one", http.StatusUnauthorized},
		{"correct credential but unregistered backend", testCredential, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, h, tc.credential, body)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

// An unconfigured (empty) Credential must refuse every call, never match on an empty
// presented value — the "no meaningful unconfigured-so-allow state" the Deps doc promises.
func TestProvider_UnconfiguredCredentialRefusesEvenAnEmptyBearer(t *testing.T) {
	h := NewHandler(Deps{Credential: "", Registry: newTestRegistry(t)})
	req := httptest.NewRequest(http.MethodPost, ProviderPath, bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// These cases are all rejected before any minting is attempted — several inside the handler
// itself, and "path traversal" one level down in the real s3.MintScopedCredential's own
// backend.CleanPrefix check (which runs before any network call), so this deliberately uses
// the real, non-stubbed Mint (Deps.Mint left nil) rather than a canned success: it proves
// these requests really can't reach a live backend, not just that a stub wasn't called.
func TestProvider_RequestValidation(t *testing.T) {
	reg := newTestRegistry(t)
	registerS3Backend(t, reg, "acme", "lake", s3.Config{Endpoint: "http://minio.storage.svc:9000", Bucket: "lake"}, map[string]string{s3.CredAccessKeyID: "admin", s3.CredSecretAccessKey: "adminsecret"})
	h := NewHandler(Deps{Credential: testCredential, Registry: reg})

	base := func() map[string]any {
		return map[string]any{"kind": "s3", "access": "read", "ttlSeconds": 300, "scope": map[string]string{"backendId": "lake", "path": "t1"}, "requester": map[string]string{"subject": "job:1", "workspace": "acme", "role": "editor"}}
	}

	cases := []struct {
		name    string
		mutate  func(map[string]any)
		want    int
		wantErr string
	}{
		{"unknown top-level field", func(b map[string]any) { b["extra"] = true }, http.StatusBadRequest, "invalid_request"},
		{"unrecognized kind", func(b map[string]any) { b["kind"] = "postgres" }, http.StatusUnprocessableEntity, "scope_not_supported"},
		{"bad access", func(b map[string]any) { b["access"] = "admin" }, http.StatusBadRequest, "invalid_request"},
		{"missing backendId", func(b map[string]any) { b["scope"] = map[string]string{"path": "t1"} }, http.StatusBadRequest, "bad_scope"},
		{"bad options value", func(b map[string]any) { b["options"] = map[string]string{"sessionToken": "sometimes"} }, http.StatusBadRequest, "invalid_request"},
		{"path traversal", func(b map[string]any) { b["scope"] = map[string]string{"backendId": "lake", "path": "../../etc"} }, http.StatusUnprocessableEntity, "scope_not_supported"},
		{"no such backend", func(b map[string]any) { b["scope"] = map[string]string{"backendId": "nope", "path": "t1"} }, http.StatusNotFound, "no_such_backend"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := base()
			tc.mutate(b)
			rec := doRequest(t, h, testCredential, b)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
			if tc.wantErr != "" {
				var got errorBody
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.Error != tc.wantErr {
					t.Errorf("error = %q, want %q", got.Error, tc.wantErr)
				}
			}
		})
	}
}

// scope/options are opaque per kind (contracts/credential-broker.md) — an extra field inside
// them (like the pre-ADR-0088 shape's nested "access", still present in some real requests
// during a client's own migration) must not fail decoding the way an unrecognized top-level
// field does.
func TestProvider_ExtraScopeFieldIsTolerated(t *testing.T) {
	reg := newTestRegistry(t)
	registerS3Backend(t, reg, "acme", "lake", s3.Config{Endpoint: "http://minio.storage.svc:9000", Bucket: "lake"}, map[string]string{s3.CredAccessKeyID: "admin", s3.CredSecretAccessKey: "adminsecret"})
	h := NewHandler(Deps{Credential: testCredential, Registry: reg, Mint: fixedMint(t, nil)})
	body := map[string]any{"kind": "s3", "access": "read", "ttlSeconds": 300, "scope": map[string]any{"backendId": "lake", "path": "t1", "access": "read"}, "requester": map[string]string{"subject": "job:1", "workspace": "acme", "role": "editor"}}
	rec := doRequest(t, h, testCredential, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
}

func TestProvider_NonS3BackendKindIsRefused(t *testing.T) {
	store := registry.NewMemoryStore()
	reg := registry.NewService(store, registry.NewMemoryCredentials(), registry.FilesystemPolicy{})
	registerRawBackend(t, store, "acme", "fs1", backend.KindFilesystem)
	h := NewHandler(Deps{Credential: testCredential, Registry: reg, Mint: fixedMint(t, nil)})
	body := map[string]any{"kind": "s3", "access": "read", "ttlSeconds": 300, "scope": map[string]string{"backendId": "fs1", "path": ""}, "requester": map[string]string{"subject": "job:1", "workspace": "acme", "role": "editor"}}
	rec := doRequest(t, h, testCredential, body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", rec.Code, rec.Body)
	}
	var got errorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Error != "scope_not_supported" {
		t.Errorf("error = %q, want scope_not_supported", got.Error)
	}
}

// A refusal from the minting layer (ErrScopeNotSupported — e.g. an AWS-backed backend) is
// relayed as the contract's 422 scope_not_supported, never as a generic failure.
func TestProvider_MintRefusalIsRelayedAsScopeNotSupported(t *testing.T) {
	reg := newTestRegistry(t)
	registerS3Backend(t, reg, "acme", "aws1", s3.Config{Bucket: "prod"}, map[string]string{s3.CredAccessKeyID: "AKIA...", s3.CredSecretAccessKey: "secret"})
	h := NewHandler(Deps{Credential: testCredential, Registry: reg, Mint: func(context.Context, s3.Config, s3.Credentials, s3.MintRequest) (s3.MintedCredential, error) {
		return s3.MintedCredential{}, s3.ErrScopeNotSupported
	}})
	body := map[string]any{"kind": "s3", "access": "read", "ttlSeconds": 300, "scope": map[string]string{"backendId": "aws1", "path": ""}, "requester": map[string]string{"subject": "job:1", "workspace": "acme", "role": "editor"}}
	rec := doRequest(t, h, testCredential, body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", rec.Code, rec.Body)
	}
}

// A genuine infrastructure failure minting (MinIO unreachable, etc.) is a clean non-2xx the
// broker relays verbatim, never a 2xx with a dud credential.
func TestProvider_MintInfrastructureFailureIs502(t *testing.T) {
	reg := newTestRegistry(t)
	registerS3Backend(t, reg, "acme", "lake", s3.Config{Endpoint: "http://minio:9000", Bucket: "lake"}, map[string]string{s3.CredAccessKeyID: "a", s3.CredSecretAccessKey: "s"})
	h := NewHandler(Deps{Credential: testCredential, Registry: reg, Mint: func(context.Context, s3.Config, s3.Credentials, s3.MintRequest) (s3.MintedCredential, error) {
		return s3.MintedCredential{}, errTestNetwork
	}})
	body := map[string]any{"kind": "s3", "access": "readwrite", "ttlSeconds": 300, "scope": map[string]string{"backendId": "lake", "path": "t1"}, "requester": map[string]string{"subject": "job:1", "workspace": "acme", "role": "owner"}}
	rec := doRequest(t, h, testCredential, body)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", rec.Code, rec.Body)
	}
}

var errTestNetwork = errors.New("connection refused")

// The success response's exact shape — what booth-lakehouse's already-built client
// (client/src/booth_lakehouse/broker.py's _parse) requires field-for-field. Also verifies
// AllowSessionToken is derived correctly from options.sessionToken, and that the audit log
// records the issuance without ever including the minted secret.
func TestProvider_SuccessResponseShapeAndAudit(t *testing.T) {
	reg := newTestRegistry(t)
	registerS3Backend(t, reg, "acme", "lake", s3.Config{Endpoint: "http://minio.storage.svc:9000", Bucket: "lake", Prefix: "warehouses"}, map[string]string{s3.CredAccessKeyID: "admin", s3.CredSecretAccessKey: "adminsecret"})

	var gotReq s3.MintRequest
	var logBuf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	h := NewHandler(Deps{Credential: testCredential, Registry: reg, Mint: fixedMint(t, &gotReq)})
	body := map[string]any{
		"kind": "s3", "access": "readwrite", "ttlSeconds": 120,
		"scope":     map[string]string{"backendId": "lake", "path": "ns1/t1"},
		"options":   map[string]string{"sessionToken": "forbidden"},
		"requester": map[string]string{"subject": "job:42", "workspace": "acme", "role": "owner"},
	}
	rec := doRequest(t, h, testCredential, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}

	if gotReq.Path != "ns1/t1" || gotReq.Access != "readwrite" || gotReq.AllowSessionToken {
		t.Errorf("mint request = %+v, want path=ns1/t1 access=readwrite AllowSessionToken=false", gotReq)
	}

	var resp map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"leaseId", "kind", "expiresAt", "scope", "credential"} {
		if _, ok := resp[field]; !ok {
			t.Errorf("response missing top-level field %q (body %s)", field, rec.Body)
		}
	}
	var scope map[string]any
	_ = json.Unmarshal(resp["scope"], &scope)
	if scope["backendId"] != "lake" || scope["path"] != "ns1/t1" || scope["access"] != "readwrite" {
		t.Errorf("scope echo = %+v, want backendId=lake path=ns1/t1 access=readwrite", scope)
	}
	var cred map[string]any
	_ = json.Unmarshal(resp["credential"], &cred)
	for _, field := range []string{"endpoint", "bucket", "pathStyle", "accessKeyId", "secretAccessKey"} {
		if _, ok := cred[field]; !ok {
			t.Errorf("credential missing field %q (body %s)", field, rec.Body)
		}
	}
	if _, ok := cred["sessionToken"]; ok {
		t.Errorf("credential has sessionToken even though none was minted and it was forbidden: %+v", cred)
	}
	if cred["accessKeyId"] != "minted-access-key" || cred["secretAccessKey"] != "minted-secret-key" {
		t.Errorf("credential = %+v, want the minted values, never the admin's own", cred)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "audit:") || !strings.Contains(logged, "job:42") {
		t.Errorf("no audit line for the issuance: %q", logged)
	}
	if strings.Contains(logged, "minted-secret-key") || strings.Contains(logged, "minted-access-key") {
		t.Fatalf("audit log leaked the minted credential value: %q", logged)
	}
}
