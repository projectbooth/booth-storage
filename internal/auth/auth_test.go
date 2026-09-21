package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fakeIdP is a minimal but real OIDC provider: a discovery document, a JWKS endpoint,
// and a signing key, so Verifier is exercised through the same go-oidc code path
// production uses rather than a stub.
type fakeIdP struct {
	server *httptest.Server
	key    *rsa.PrivateKey
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{key: key}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.server.URL,
			"jwks_uri":                              idp.server.URL + "/jwks",
			"authorization_endpoint":                idp.server.URL + "/auth",
			"token_endpoint":                        idp.server.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

type tokenOpts struct {
	issuer   string
	subject  string
	audience string
	expiry   time.Duration
	signWith *rsa.PrivateKey
	// groups, if non-nil, is emitted as the claim named groupsClaim (default "groups").
	groups      any
	groupsClaim string
}

func (idp *fakeIdP) token(t *testing.T, o tokenOpts) string {
	t.Helper()
	if o.issuer == "" {
		o.issuer = idp.server.URL
	}
	if o.expiry == 0 {
		o.expiry = time.Hour
	}
	key := o.signWith
	if key == nil {
		key = idp.key
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	claims := jwt.Claims{Issuer: o.issuer, Subject: o.subject, IssuedAt: jwt.NewNumericDate(time.Now()), Expiry: jwt.NewNumericDate(time.Now().Add(o.expiry))}
	if o.audience != "" {
		claims.Audience = jwt.Audience{o.audience}
	}
	builder := jwt.Signed(signer).Claims(claims)
	if o.groups != nil {
		name := o.groupsClaim
		if name == "" {
			name = "groups"
		}
		builder = builder.Claims(map[string]any{name: o.groups})
	}
	raw, err := builder.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestVerifier(t *testing.T) {
	idp := newFakeIdP(t)
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ctx := context.Background()

	lax, err := NewVerifier(ctx, OIDCConfig{IssuerURL: idp.server.URL, ClientID: "booth-storage"})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	strict, err := NewVerifier(ctx, OIDCConfig{IssuerURL: idp.server.URL, ClientID: "booth-storage", RequireAudience: true})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		verifier *Verifier
		opts     tokenOpts
		wantSub  string
		wantErr  bool
	}{
		{"valid", lax, tokenOpts{subject: "alice"}, "alice", false},
		{"valid, audience not required so any audience passes", lax, tokenOpts{subject: "alice", audience: "account"}, "alice", false},
		{"expired", lax, tokenOpts{subject: "alice", expiry: -time.Hour}, "", true},
		{"wrong issuer", lax, tokenOpts{subject: "alice", issuer: "https://evil.example"}, "", true},
		{"signed by an unknown key", lax, tokenOpts{subject: "alice", signWith: otherKey}, "", true},
		{"missing subject", lax, tokenOpts{subject: ""}, "", true},
		{"audience required and matching", strict, tokenOpts{subject: "alice", audience: "booth-storage"}, "alice", false},
		{"audience required but wrong", strict, tokenOpts{subject: "alice", audience: "someone-else"}, "", true},
		{"audience required but absent", strict, tokenOpts{subject: "alice"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := tc.verifier.Verify(ctx, idp.token(t, tc.opts))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Verify succeeded with claims %+v, want an error", claims)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if claims.Subject != tc.wantSub {
				t.Errorf("subject = %q, want %q", claims.Subject, tc.wantSub)
			}
		})
	}

	if _, err := lax.Verify(ctx, "not.a.jwt"); err == nil {
		t.Error("garbage token verified")
	}
}

// newFakeCore is booth-core's workload-token issuer as ADR 0056 describes it: a signing key
// and a JWKS at the fixed well-known path. Unlike fakeIdP it serves no discovery document,
// which is how the verifier must be able to trust it.
func newFakeCore(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	core := &fakeIdP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	core.server = httptest.NewServer(mux)
	t.Cleanup(core.server.Close)
	return core
}

func TestVerifier_WorkloadIssuer(t *testing.T) {
	idp := newFakeIdP(t)
	core := newFakeCore(t)
	ctx := context.Background()

	both, err := NewVerifier(ctx, OIDCConfig{IssuerURL: idp.server.URL, ClientID: "booth-storage", RequireAudience: true, WorkloadIssuerURL: core.server.URL})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	idpOnly, err := NewVerifier(ctx, OIDCConfig{IssuerURL: idp.server.URL, ClientID: "booth-storage", RequireAudience: true})
	if err != nil {
		t.Fatal(err)
	}
	groups := []string{"/workspaces/acme/editor"}

	cases := []struct {
		name     string
		verifier *Verifier
		token    string
		wantSub  string
		wantErr  bool
	}{
		{"human token still verifies", both, idp.token(t, tokenOpts{subject: "alice", audience: "booth-storage", groups: groups}), "alice", false},
		{"workload token verifies", both, core.token(t, tokenOpts{subject: "job:42", audience: "booth-storage", groups: groups}), "job:42", false},
		{"workload token with no second issuer configured is refused", idpOnly, core.token(t, tokenOpts{subject: "job:42", audience: "booth-storage", groups: groups}), "", true},
		{"workload token audience is enforced like a human's", both, core.token(t, tokenOpts{subject: "job:42", audience: "someone-else", groups: groups}), "", true},
		{"expired workload token", both, core.token(t, tokenOpts{subject: "job:42", audience: "booth-storage", expiry: -time.Hour}), "", true},
		// Each issuer's keys are good for that issuer only: neither can vouch for the other.
		{"core's key claiming the IdP's issuer", both, core.token(t, tokenOpts{subject: "mallory", issuer: idp.server.URL, audience: "booth-storage"}), "", true},
		{"IdP's key claiming core's issuer", both, idp.token(t, tokenOpts{subject: "mallory", issuer: core.server.URL, audience: "booth-storage"}), "", true},
		{"an untrusted issuer", both, core.token(t, tokenOpts{subject: "mallory", issuer: "https://evil.example", audience: "booth-storage"}), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := tc.verifier.Verify(ctx, tc.token)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Verify succeeded with claims %+v, want an error", claims)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if claims.Subject != tc.wantSub {
				t.Errorf("subject = %q, want %q", claims.Subject, tc.wantSub)
			}
			// The role-derivation input (ADR 0041) is the same whichever issuer signed it.
			if got := RoleInWorkspace(claims.Groups, "acme"); got != RoleEditor {
				t.Errorf("role in acme = %q, want editor", got)
			}
		})
	}
}

// Trusting core must not depend on core being reachable when this module starts: its keys
// are fetched on the first workload token, and human tokens work meanwhile.
func TestVerifier_WorkloadIssuerDownAtStartup(t *testing.T) {
	idp := newFakeIdP(t)
	ctx := context.Background()
	v, err := NewVerifier(ctx, OIDCConfig{IssuerURL: idp.server.URL, ClientID: "booth-storage", WorkloadIssuerURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("NewVerifier must not need core to be up: %v", err)
	}
	if _, err := v.Verify(ctx, idp.token(t, tokenOpts{subject: "alice"})); err != nil {
		t.Errorf("human token refused while core is down: %v", err)
	}
}

func TestVerifier_WorkloadIssuerTrailingSlash(t *testing.T) {
	idp := newFakeIdP(t)
	core := newFakeCore(t)
	ctx := context.Background()
	v, err := NewVerifier(ctx, OIDCConfig{IssuerURL: idp.server.URL, ClientID: "booth-storage", WorkloadIssuerURL: core.server.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(ctx, core.token(t, tokenOpts{subject: "job:1"})); err != nil {
		t.Errorf("workload token refused when the issuer is configured with a trailing slash: %v", err)
	}
}

func TestNewVerifier_WorkloadIssuerMustDifferFromIdP(t *testing.T) {
	idp := newFakeIdP(t)
	if _, err := NewVerifier(context.Background(), OIDCConfig{IssuerURL: idp.server.URL, WorkloadIssuerURL: idp.server.URL}); err == nil {
		t.Error("NewVerifier accepted the IdP as its own workload issuer")
	}
}

func TestNewVerifier_UnreachableIssuer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := NewVerifier(ctx, OIDCConfig{IssuerURL: "http://127.0.0.1:1"}); err == nil {
		t.Error("NewVerifier succeeded against an unreachable issuer")
	}
}

type stubVerifier map[string]*Claims

func (s stubVerifier) Verify(_ context.Context, tok string) (*Claims, error) {
	if c, ok := s[tok]; ok {
		return c, nil
	}
	return nil, errors.New("bad token")
}

func TestMiddleware(t *testing.T) {
	var seen Identity
	handler := Middleware(stubVerifier{"good": {Subject: "alice", Groups: []string{"/workspaces/acme/editor"}}})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	do := func(headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no token", map[string]string{HeaderBoothWorkspace: "acme"}, http.StatusUnauthorized},
		{"non-bearer scheme", map[string]string{"Authorization": "Basic Zm9v", HeaderBoothWorkspace: "acme"}, http.StatusUnauthorized},
		{"invalid token", map[string]string{"Authorization": "Bearer nope", HeaderBoothWorkspace: "acme"}, http.StatusUnauthorized},
		// The forwarded headers alone must never be enough — that is the whole point of
		// independently verifying the token.
		{"forged workspace/role headers without a token", map[string]string{HeaderBoothWorkspace: "acme", HeaderBoothRole: "owner"}, http.StatusUnauthorized},
		{"valid token but no workspace header", map[string]string{"Authorization": "Bearer good"}, http.StatusBadRequest},
		{"valid", map[string]string{"Authorization": "Bearer good", HeaderBoothWorkspace: "acme", HeaderBoothRole: "editor"}, http.StatusOK},
		{"scheme is case-insensitive", map[string]string{"Authorization": "bearer good", HeaderBoothWorkspace: "acme", HeaderBoothRole: "viewer"}, http.StatusOK},
		{"token grants no role in the requested workspace", map[string]string{"Authorization": "Bearer good", HeaderBoothWorkspace: "globex", HeaderBoothRole: "owner"}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen = Identity{}
			rec := do(tc.headers)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
			if tc.want != http.StatusOK {
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
					t.Errorf("error body = %q, want JSON {\"error\": ...}", rec.Body)
				}
				if seen != (Identity{}) {
					t.Errorf("handler ran despite rejection: %+v", seen)
				}
			}
		})
	}

	do(map[string]string{"Authorization": "Bearer good", HeaderBoothWorkspace: "acme", HeaderBoothRole: "editor"})
	if seen.Subject != "alice" || seen.Workspace != "acme" || seen.Role != RoleEditor {
		t.Errorf("identity = %+v", seen)
	}
}

func TestRolePermissions(t *testing.T) {
	cases := []struct {
		role                     Role
		read, write, admin, want bool
	}{
		{RoleOwner, true, true, true, true},
		{RoleEditor, true, true, false, true},
		{RoleViewer, true, false, false, true},
		{"", false, false, false, true},
		{"superuser", false, false, false, true}, // unknown roles get nothing
	}
	for _, tc := range cases {
		id := Identity{Role: tc.role}
		if id.CanRead() != tc.read || id.CanWrite() != tc.write || id.IsAdmin() != tc.admin {
			t.Errorf("role %q: read=%v write=%v admin=%v, want %v/%v/%v", tc.role, id.CanRead(), id.CanWrite(), id.IsAdmin(), tc.read, tc.write, tc.admin)
		}
	}
}

func TestRequire(t *testing.T) {
	h := Require(Identity.IsAdmin, "owners only")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	for role, want := range map[Role]int{RoleOwner: 204, RoleEditor: 403, RoleViewer: 403, "": 403} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(WithIdentity(req.Context(), Identity{Role: role}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("role %q: status %d, want %d", role, rec.Code, want)
		}
	}

	// No identity at all (Middleware was skipped) must fail closed.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing identity: status %d, want 401", rec.Code)
	}
}

// The point of independent verification: a valid token cannot be upgraded by a forged
// X-Booth-Role header. (Measured against the real server before this existed: a viewer's
// token plus "X-Booth-Role: owner" reached the admin API.)
func TestMiddleware_RoleIsDerivedFromTheTokenNotTheHeader(t *testing.T) {
	verifier := stubVerifier{
		"viewer-token": {Subject: "carol", Groups: []string{"/workspaces/acme/viewer", "/workspaces/other/owner"}},
		"multi-token":  {Subject: "erin", Groups: []string{"/workspaces/acme/viewer", "/workspaces/acme/editor", "/workspaces/skip/owner", "not-a-workspace-group", "/workspaces/acme/superuser"}},
	}
	var got Identity
	h := Middleware(verifier)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got, _ = FromContext(r.Context()) }))

	cases := []struct {
		name, token, workspace, header string
		wantStatus                     int
		want                           Role
	}{
		// ADR 0041: a header stronger than the token grants is REJECTED, not downgraded.
		{"forged owner header with a viewer token", "viewer-token", "acme", "owner", 403, ""},
		{"forged editor header with a viewer token", "viewer-token", "acme", "editor", 403, ""},
		{"editor token claiming owner", "multi-token", "acme", "owner", 403, ""},
		{"absent header falls back to the token's grant", "viewer-token", "acme", "", 200, RoleViewer},
		{"a header may narrow the grant", "multi-token", "acme", "viewer", 200, RoleViewer},
		{"the highest of several grants applies in the workspace", "multi-token", "acme", "editor", 200, RoleEditor},
		{"a role held in ANOTHER workspace does not carry over", "viewer-token", "nowhere", "owner", 403, ""},
		{"an unrecognized forwarded role yields no access", "viewer-token", "acme", "root", 200, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got = Identity{}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			req.Header.Set(HeaderBoothWorkspace, tc.workspace)
			if tc.header != "" {
				req.Header.Set(HeaderBoothRole, tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body)
			}
			if tc.wantStatus == 200 && got.Role != tc.want {
				t.Errorf("effective role = %q, want %q", got.Role, tc.want)
			}
		})
	}
}

func TestEffectiveRole(t *testing.T) {
	cases := []struct{ forwarded, granted, want Role }{
		{"", RoleOwner, RoleOwner}, {"", RoleViewer, RoleViewer},
		{RoleOwner, RoleOwner, RoleOwner}, {RoleOwner, RoleViewer, RoleViewer}, {RoleEditor, RoleViewer, RoleViewer},
		{RoleViewer, RoleOwner, RoleViewer}, {RoleEditor, RoleOwner, RoleEditor},
		{"root", RoleOwner, ""}, {"OWNER", RoleOwner, ""},
	}
	for _, tc := range cases {
		if got := EffectiveRole(tc.forwarded, tc.granted); got != tc.want {
			t.Errorf("EffectiveRole(%q, %q) = %q, want %q", tc.forwarded, tc.granted, got, tc.want)
		}
	}
}

func TestRoleInWorkspace(t *testing.T) {
	groups := []string{"/workspaces/acme/viewer", "/workspaces/acme/owner", "/workspaces/acme-evil/owner"}
	if got := RoleInWorkspace(groups, "acme"); got != RoleOwner {
		t.Errorf("acme = %q, want owner", got)
	}
	// Similar-looking names and malformed groups must not match.
	if got := RoleInWorkspace([]string{"/workspaces/acme-evil/owner"}, "acme"); got != "" {
		t.Errorf("prefix-sibling workspace matched: %q", got)
	}
	bad := []string{"/workspaces/acme/owner/extra", "workspaces/acme/owner", "/workspaces/acme/superuser", "/workspaces/ACME/owner"}
	if got := RoleInWorkspace(bad, "acme"); got != "" {
		t.Errorf("malformed groups matched: %q", got)
	}
	if got := RoleInWorkspace(nil, "acme"); got != "" {
		t.Errorf("no groups matched: %q", got)
	}
}

// Through the real verifier: the groups claim is read from a genuinely signed token, under
// the configured claim name, and a malformed claim grants nothing.
func TestVerifier_ReadsGroupsClaim(t *testing.T) {
	idp := newFakeIdP(t)
	ctx := context.Background()

	def, err := NewVerifier(ctx, OIDCConfig{IssuerURL: idp.server.URL, ClientID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	custom, err := NewVerifier(ctx, OIDCConfig{IssuerURL: idp.server.URL, ClientID: "c", GroupsClaim: "memberships"})
	if err != nil {
		t.Fatal(err)
	}

	tok := idp.token(t, tokenOpts{subject: "alice", groups: []string{"/workspaces/acme/owner", "/workspaces/x/viewer"}})
	c, err := def.Verify(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Groups) != 2 || RoleInWorkspace(c.Groups, "acme") != RoleOwner {
		t.Errorf("groups = %v", c.Groups)
	}

	// Configured claim name: the default name is then ignored, the custom one read.
	if c, _ := custom.Verify(ctx, tok); len(c.Groups) != 0 {
		t.Errorf("custom-claim verifier read the default claim: %v", c.Groups)
	}
	tok2 := idp.token(t, tokenOpts{subject: "alice", groupsClaim: "memberships", groups: []string{"/workspaces/acme/editor"}})
	if c, _ := custom.Verify(ctx, tok2); RoleInWorkspace(c.Groups, "acme") != RoleEditor {
		t.Errorf("custom claim not read: %v", c.Groups)
	}

	// No claim, or one of the wrong shape: a genuine token that simply grants nothing.
	for name, groups := range map[string]any{"absent": nil, "a string": "/workspaces/acme/owner", "a number": 7, "an object": map[string]string{"a": "b"}} {
		c, err := def.Verify(ctx, idp.token(t, tokenOpts{subject: "alice", groups: groups}))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if RoleInWorkspace(c.Groups, "acme") != "" {
			t.Errorf("%s: granted a role from a malformed/absent claim: %v", name, c.Groups)
		}
	}
}
