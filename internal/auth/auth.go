// Package auth implements booth-storage's side of the defense-in-depth requirement in
// contracts/core-platform-api.md: "a module must independently verify the identity core
// forwards it rather than trusting the network path implicitly."
//
// booth-core's gateway resolves workspace/role from the token's groups claim and forwards
// them as X-Booth-Workspace/X-Booth-Role (ADR 0025). Unlike booth-module-store, this
// package does NOT take the forwarded role on trust: verifying the token proves who the
// caller is, not that they hold the role in a header, and anyone able to reach the pod
// without going through the gateway could otherwise send a valid low-privilege token with a
// forged "owner" header. So the role is re-derived here from the verified token's groups
// claim, and the effective role is never stronger than what the token itself grants for the
// requested workspace (see Middleware), and a header claiming more than the token grants
// is rejected with 403. This is ADR 0041, required of every module that authorizes by role.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	oidc "github.com/coreos/go-oidc/v3/oidc"
)

// Header names the gateway forwards to a backing module (ADR 0025).
const (
	HeaderBoothWorkspace = "X-Booth-Workspace"
	HeaderBoothRole      = "X-Booth-Role"
)

// Role is a caller's workspace role (ADR 0025).
type Role string

const (
	RoleOwner  Role = "owner"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

// Claims is the subset of a verified token this module cares about.
type Claims struct {
	Subject string
	// Groups is the token's workspace-membership claim (ADR 0025), e.g.
	// "/workspaces/acme/owner". Empty if the token carries none.
	Groups []string
}

// TokenVerifier verifies a raw bearer token. *Verifier is the production
// implementation; the interface exists so the HTTP layer can be tested without a live
// OIDC provider.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (*Claims, error)
}

// OIDCConfig is the identity-provider configuration — the same shape as booth-core's, so
// every module verifies against the same provider.
type OIDCConfig struct {
	IssuerURL       string
	ClientID        string
	RequireAudience bool
	// GroupsClaim names the token claim carrying workspace memberships. Configurable
	// because not every OIDC provider calls it "groups"; must match booth-core's setting.
	// Empty means the default, "groups".
	GroupsClaim string
	// WorkloadIssuerURL, if set, is booth-core's own issuer URL (ADR 0056), trusted as a
	// second issuer for short-lived workload tokens that represent an unattended run rather
	// than a person. Its keys are read from <WorkloadIssuerURL>/.well-known/jwks.json. Such a
	// token carries the same audience and groups claim as a human's, so everything downstream
	// of verification is unchanged. Empty (the default) trusts the identity provider only.
	WorkloadIssuerURL string
}

// DefaultGroupsClaim matches booth-core's default (ADR 0025).
const DefaultGroupsClaim = "groups"

// workloadJWKSPath is where booth-core publishes its workload-token signing keys, relative
// to its issuer URL (ADR 0056).
const workloadJWKSPath = "/.well-known/jwks.json"

// Verifier verifies bearer tokens against the same OIDC provider booth-core is
// configured against (signature via JWKS, issuer, expiry, and, per deployment policy,
// audience) and, when configured, against booth-core's own workload-token issuer.
type Verifier struct {
	// byIssuer maps a trusted issuer URL to the verifier for it. A token's (unverified)
	// iss only selects which verifier runs; that verifier then checks the issuer itself
	// along with signature, expiry and audience, so a forged iss gains nothing.
	byIssuer    map[string]*oidc.IDTokenVerifier
	groupsClaim string
}

func NewVerifier(ctx context.Context, cfg OIDCConfig) (*Verifier, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery against %s: %w", cfg.IssuerURL, err)
	}
	claim := cfg.GroupsClaim
	if claim == "" {
		claim = DefaultGroupsClaim
	}
	verifierConfig := &oidc.Config{
		SkipClientIDCheck: !cfg.RequireAudience,
		ClientID:          cfg.ClientID,
	}
	v := &Verifier{
		byIssuer:    map[string]*oidc.IDTokenVerifier{cfg.IssuerURL: provider.Verifier(verifierConfig)},
		groupsClaim: claim,
	}
	// Core strips a trailing slash from its issuer URL, so match that form in `iss`.
	if wl := strings.TrimRight(cfg.WorkloadIssuerURL, "/"); wl != "" {
		if wl == cfg.IssuerURL {
			return nil, fmt.Errorf("the workload issuer must differ from the OIDC issuer (both %s)", wl)
		}
		// Keys are fetched lazily, on the first workload token: booth-core may not be up yet
		// when this module starts, and that must not stop it serving human tokens.
		keys := oidc.NewRemoteKeySet(ctx, wl+workloadJWKSPath)
		v.byIssuer[wl] = oidc.NewVerifier(wl, keys, verifierConfig)
	}
	return v, nil
}

// unverifiedIssuer reads the iss claim from a JWT's payload without checking anything. It
// is only ever used to choose which verifier to hand the token to.
func unverifiedIssuer(rawToken string) (string, error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("malformed token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("malformed token payload: %w", err)
	}
	var p struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", fmt.Errorf("malformed token payload: %w", err)
	}
	return p.Issuer, nil
}

func (v *Verifier) Verify(ctx context.Context, rawToken string) (*Claims, error) {
	issuer, err := unverifiedIssuer(rawToken)
	if err != nil {
		return nil, fmt.Errorf("token verification failed: %w", err)
	}
	verifier, ok := v.byIssuer[issuer]
	if !ok {
		return nil, fmt.Errorf("token verification failed: issuer %q is not trusted", issuer)
	}
	idToken, err := verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("token verification failed: %w", err)
	}
	if idToken.Subject == "" {
		return nil, fmt.Errorf("token missing subject")
	}

	var raw map[string]json.RawMessage
	if err := idToken.Claims(&raw); err != nil {
		return nil, fmt.Errorf("reading token claims: %w", err)
	}
	var groups []string
	if g, ok := raw[v.groupsClaim]; ok {
		// A claim of the wrong shape is treated as "no groups" (fail closed), not an error:
		// the token is genuine, it simply grants no workspace role.
		_ = json.Unmarshal(g, &groups)
	}
	return &Claims{Subject: idToken.Subject, Groups: groups}, nil
}

// groupRE is ADR 0025's workspace-membership group shape.
var groupRE = regexp.MustCompile(`^/workspaces/([a-z0-9-]+)/(owner|editor|viewer)$`)

func rank(r Role) int {
	switch r {
	case RoleOwner:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

// RoleInWorkspace returns the highest role the groups grant in workspace, or "" if none.
func RoleInWorkspace(groups []string, workspace string) Role {
	var best Role
	for _, g := range groups {
		m := groupRE.FindStringSubmatch(g)
		if m == nil || m[1] != workspace {
			continue
		}
		if r := Role(m[2]); rank(r) > rank(best) {
			best = r
		}
	}
	return best
}

// EffectiveRole combines the gateway-forwarded role with what the token itself grants: the
// result is never stronger than the token's grant, and never stronger than the forwarded
// role (a gateway may legitimately narrow, never widen). An absent forwarded role means
// "use the token's". Any unrecognized forwarded value yields "" — no access. (Middleware
// rejects a forwarded role that *exceeds* the grant before this is reached; this is the
// remaining narrowing/fallback logic.)
func EffectiveRole(forwarded, granted Role) Role {
	if forwarded == "" {
		return granted
	}
	if rank(forwarded) == 0 {
		return ""
	}
	if rank(forwarded) < rank(granted) {
		return forwarded
	}
	return granted
}

type contextKey struct{}

// Identity is the caller identity attached to a request's context by Middleware.
type Identity struct {
	Subject   string
	Workspace string
	Role      Role
}

// CanRead reports whether the caller may list and read objects and see which backends
// are registered: any recognized workspace role.
func (i Identity) CanRead() bool {
	return i.Role == RoleOwner || i.Role == RoleEditor || i.Role == RoleViewer
}

// CanWrite reports whether the caller may write objects: editors and owners.
func (i Identity) CanWrite() bool { return i.Role == RoleOwner || i.Role == RoleEditor }

// IsAdmin reports whether the caller may register, edit and remove backends and their
// credentials. Owners only — this is the "admin/owner-level workspace role" ADR 0023
// gates the admin view on, and the server enforces it regardless of what any client UI
// shows.
func (i Identity) IsAdmin() bool { return i.Role == RoleOwner }

// FromContext returns the identity Middleware attached, if any.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(contextKey{}).(Identity)
	return id, ok
}

// WithIdentity attaches an identity the way Middleware does — for tests exercising
// handlers downstream of it.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// Middleware verifies the request's bearer token and reads the gateway-forwarded
// workspace/role headers, attaching the result to the request context.
func Middleware(verifier TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				WriteError(w, http.StatusUnauthorized, "missing bearer token")
				return
			}
			claims, err := verifier.Verify(r.Context(), token)
			if err != nil {
				WriteError(w, http.StatusUnauthorized, "invalid token")
				return
			}
			workspace := r.Header.Get(HeaderBoothWorkspace)
			if workspace == "" {
				WriteError(w, http.StatusBadRequest, "missing "+HeaderBoothWorkspace+" header")
				return
			}

			// Independent authorization: what does the *token* say this caller is in the
			// requested workspace? The forwarded header can only narrow that, never widen it.
			granted := RoleInWorkspace(claims.Groups, workspace)
			if granted == "" {
				WriteError(w, http.StatusForbidden, "your token grants no role in this workspace")
				return
			}
			forwarded := Role(r.Header.Get(HeaderBoothRole))
			// ADR 0041: a forwarded role stronger than the token grants is not "downgraded and
			// carried on" — it is a forged or corrupted request, and is rejected outright.
			if rank(forwarded) > rank(granted) {
				log.Printf("auth: rejected: forwarded role %q exceeds token-derived role %q for sub=%s workspace=%s", forwarded, granted, claims.Subject, workspace)
				WriteError(w, http.StatusForbidden, "the forwarded role exceeds what your token grants in this workspace")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), Identity{
				Subject:   claims.Subject,
				Workspace: workspace,
				Role:      EffectiveRole(forwarded, granted),
			})))
		})
	}
}

// Require returns middleware that rejects callers failing check with 403. It must run
// after Middleware.
func Require(check func(Identity) bool, message string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := FromContext(r.Context())
			if !ok {
				WriteError(w, http.StatusUnauthorized, "no identity")
				return
			}
			if !check(id) {
				WriteError(w, http.StatusForbidden, message)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// WriteError writes the JSON error body used across this module's API.
func WriteError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
