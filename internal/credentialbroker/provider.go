// Package credentialbroker is booth-storage's provider side of the ADR 0080 credential
// broker (contracts/credential-broker.md, mechanism fixed by ADR 0088): booth-core routes an
// already-authorized request here by declared kind, and this module mints the actual
// short-lived, scoped credential — never its own real backend credential (ADR 0039). Today
// this serves the "s3" kind only, via internal/backend/s3's MintScopedCredential.
//
// Authentication here is deliberately not internal/auth.Middleware: the caller is
// booth-core itself, presenting a shared secret it derives from an HMAC key only core holds
// (contracts/credential-broker.md's "Provider path and authentication", `credentialbroker.
// Keys.ProviderCredential` on core's side). What core delivers to this module — as the
// booth-credential-broker-provider-credentials Secret — is already the one finished
// credential string core will present on every call, not the raw key it was derived from
// ("a provider need not be Go — it needs its own copy of the shared secret from the
// delivered Secret, not this package", per the contract). So verification here is a
// constant-time comparison against that stored copy, not a re-derivation: the same security
// property (only a holder of core's key could have produced that exact value), achieved
// without this module ever needing the key itself.
//
// Design record: docs/decisions/0006-s3-credential-broker-provider-design.md.
package credentialbroker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/projectbooth/booth-storage/internal/backend"
	"github.com/projectbooth/booth-storage/internal/backend/s3"
	"github.com/projectbooth/booth-storage/internal/registry"
)

// ProviderPath is the fixed path every credential-broker provider implements
// (contracts/credential-broker.md's "Provider path and authentication", mirroring core's own
// exported credentialbroker.ProviderPath constant). Core calls exactly this path on this
// module's in-cluster Service URL — never through the gateway, and never authenticated the
// way ordinary /api/* routes are.
const ProviderPath = "/internal/credentials"

// maxRequestBytes bounds a provider request body; the largest legitimate one is a handful of
// short strings.
const maxRequestBytes = 16 << 10

// kindS3 is the only credential kind this provider currently serves.
const kindS3 = "s3"

// Deps is what the provider HTTP handler needs.
type Deps struct {
	// Credential is this module's own copy of the shared secret core presents as
	// `Authorization: Bearer <Credential>` (the booth-credential-broker-provider-credentials
	// Secret's `credential` key). Empty means the route refuses every request — there is no
	// meaningful "unconfigured, so allow" state for a route this privileged.
	Credential string
	Registry   *registry.Service
	// Now and NewLeaseID are overridden in tests; both default sensibly when zero.
	Now        func() time.Time
	NewLeaseID func() string
	// Mint defaults to s3.Config.MintScopedCredential — the real, network-calling
	// implementation. Overridable so tests can exercise the handler's request parsing,
	// validation, routing and response envelope deterministically, without a real backend to
	// mint against; production always uses the default.
	Mint func(ctx context.Context, cfg s3.Config, admin s3.Credentials, req s3.MintRequest) (s3.MintedCredential, error)
}

// NewHandler builds the provider endpoint. Deliberately not wired into internal/api's chi
// router/auth.Middleware stack: this route has its own, different authentication mechanism
// (see the package doc) and is never reached through booth-core's gateway.
func NewHandler(deps Deps) http.Handler {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.NewLeaseID == nil {
		deps.NewLeaseID = uuid.NewString
	}
	if deps.Mint == nil {
		deps.Mint = func(ctx context.Context, cfg s3.Config, admin s3.Credentials, req s3.MintRequest) (s3.MintedCredential, error) {
			return cfg.MintScopedCredential(ctx, admin, req)
		}
	}
	return &provider{deps}
}

type provider struct{ Deps }

// ---- wire shapes -------------------------------------------------------------------------
//
// Top-level fields mirror core's real internal/credentialbroker.providerRequest/Response
// exactly (contracts/credential-broker.md's request/response shape, ADR 0088). The `s3`-kind
// scope/options/credential shapes are this module's own to define (the contract leaves them
// opaque per kind) — adopted from booth-lakehouse's already-built, already-tested client
// (client/src/booth_lakehouse/broker.py) rather than invented fresh, so the one real consumer
// needs no further translation layer.

type inboundRequest struct {
	Kind       string          `json:"kind"`
	TTLSeconds int             `json:"ttlSeconds"`
	Access     string          `json:"access"`
	Scope      json.RawMessage `json:"scope"`
	Options    json.RawMessage `json:"options,omitempty"`
	Requester  requester       `json:"requester"`
}

type requester struct {
	Subject   string `json:"subject"`
	Workspace string `json:"workspace"`
	Role      string `json:"role"`
}

// s3Scope is this request's opaque `scope` for the s3 kind: which backend, and which
// sub-prefix of it (ADR 0045 — only this module knows how that maps onto a real bucket).
type s3Scope struct {
	BackendID string `json:"backendId"`
	Path      string `json:"path"`
}

// s3ScopeEcho is the `scope` this provider echoes back in its response. It repeats Access
// alongside BackendID/Path (even though the request's Access is a separate, shared top-level
// field per ADR 0088) because booth-lakehouse's already-built client checks the echo
// specifically to make sure the broker didn't return a credential for a different scope than
// it asked for ("refuse rather than widen" cuts both ways: a caller shouldn't trust an
// unchecked echo either).
type s3ScopeEcho struct {
	BackendID string `json:"backendId"`
	Path      string `json:"path"`
	Access    string `json:"access"`
}

// s3Options is this request's opaque `options` for the s3 kind.
type s3Options struct {
	// SessionToken is "forbidden" (a bare access-key/secret-key pair only — Lakekeeper's
	// static-key warehouse credential has no field for one) or "allowed" (a session token is
	// fine if the minted credential naturally has one; it is not required). Empty means
	// "allowed", the more permissive default.
	SessionToken string `json:"sessionToken"`
}

// s3CredentialBody is the minted credential this provider returns, including its resolved
// real-world location (docs/decisions/0006 §2) — endpoint, bucket, key prefix, region, path
// style — so the caller never needs to understand this backend's own configuration.
type s3CredentialBody struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region,omitempty"`
	Bucket          string `json:"bucket"`
	KeyPrefix       string `json:"keyPrefix,omitempty"`
	PathStyle       bool   `json:"pathStyle"`
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken,omitempty"`
}

type outboundResponse struct {
	LeaseID    string    `json:"leaseId"`
	Kind       string    `json:"kind"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Scope      any       `json:"scope"`
	Credential any       `json:"credential"`
}

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// ---- handler -------------------------------------------------------------------------

func (p *provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store") // a 2xx body carries a live credential

	if !p.authenticate(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}

	var req inboundRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body: "+err.Error())
		return
	}

	if req.Kind != kindS3 {
		writeError(w, http.StatusUnprocessableEntity, "scope_not_supported", fmt.Sprintf("this provider only issues %q credentials", kindS3))
		return
	}
	if req.Access != s3.AccessRead && req.Access != s3.AccessReadWrite {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("access must be %q or %q", s3.AccessRead, s3.AccessReadWrite))
		return
	}

	var scope s3Scope
	if err := json.Unmarshal(req.Scope, &scope); err != nil || scope.BackendID == "" {
		writeError(w, http.StatusBadRequest, "bad_scope", "scope must include a non-empty backendId and path")
		return
	}
	var opts s3Options
	if len(req.Options) > 0 {
		if err := json.Unmarshal(req.Options, &opts); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid options: "+err.Error())
			return
		}
	}
	if opts.SessionToken != "" && opts.SessionToken != "forbidden" && opts.SessionToken != "allowed" {
		writeError(w, http.StatusBadRequest, "invalid_request", `options.sessionToken must be "forbidden" or "allowed"`)
		return
	}

	rec, creds, err := p.Registry.RawCredentials(r.Context(), req.Requester.Workspace, scope.BackendID)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "no_such_backend", fmt.Sprintf("no backend %q in workspace %q", scope.BackendID, req.Requester.Workspace))
		return
	case errors.Is(err, registry.ErrNoCredentials):
		creds = nil // MintScopedCredential turns "no admin credentials" into a clear refusal below.
	case err != nil:
		log.Printf("credential broker: reading backend %s/%s: %v", req.Requester.Workspace, scope.BackendID, err)
		writeError(w, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if rec.Kind != backend.KindS3 {
		writeError(w, http.StatusUnprocessableEntity, "scope_not_supported", fmt.Sprintf("backend %q is a %s backend, not s3", scope.BackendID, rec.Kind))
		return
	}

	var cfg s3.Config
	if err := json.Unmarshal(rec.Config, &cfg); err != nil {
		log.Printf("credential broker: backend %s/%s has unparseable s3 config: %v", req.Requester.Workspace, scope.BackendID, err)
		writeError(w, http.StatusInternalServerError, "internal_error", "")
		return
	}
	admin := s3.Credentials{
		AccessKeyID: creds[s3.CredAccessKeyID], SecretAccessKey: creds[s3.CredSecretAccessKey], SessionToken: creds[s3.CredSessionToken],
	}

	minted, err := p.Mint(r.Context(), cfg, admin, s3.MintRequest{
		Path: scope.Path, Access: req.Access, TTL: time.Duration(req.TTLSeconds) * time.Second,
		AllowSessionToken: opts.SessionToken != "forbidden",
	})
	if err != nil {
		if errors.Is(err, s3.ErrScopeNotSupported) {
			writeError(w, http.StatusUnprocessableEntity, "scope_not_supported", err.Error())
			return
		}
		log.Printf("credential broker: minting for backend %s/%s: %v", req.Requester.Workspace, scope.BackendID, err)
		writeError(w, http.StatusBadGateway, "mint_failed", "the backend could not be reached to mint a credential")
		return
	}

	leaseID := p.NewLeaseID()
	echoPath := strings.Trim(scope.Path, "/")
	// Audit trail (ADR 0080's own mandatory record, distinct from this module's ordinary
	// stdout logging, but stdout is what this module has — see contracts/credential-
	// broker.md's requirements and ADR 0022's "no logging API to integrate against"): who,
	// what, when, expiry — never the credential value itself.
	log.Printf("audit: credential issued lease=%s kind=s3 subject=%s workspace=%s role=%s backend=%s path=%q access=%s expiresAt=%s",
		leaseID, req.Requester.Subject, req.Requester.Workspace, req.Requester.Role, scope.BackendID, echoPath, req.Access, minted.ExpiresAt.UTC().Format(time.RFC3339))

	writeJSON(w, http.StatusCreated, outboundResponse{
		LeaseID: leaseID, Kind: kindS3, ExpiresAt: minted.ExpiresAt,
		Scope: s3ScopeEcho{BackendID: scope.BackendID, Path: echoPath, Access: req.Access},
		Credential: s3CredentialBody{
			Endpoint: minted.Endpoint, Region: minted.Region, Bucket: minted.Bucket, KeyPrefix: minted.KeyPrefix, PathStyle: minted.PathStyle,
			AccessKeyID: minted.AccessKeyID, SecretAccessKey: minted.SecretAccessKey, SessionToken: minted.SessionToken,
		},
	})
}

func (p *provider) authenticate(r *http.Request) bool {
	presented := bearerToken(r)
	if presented == "" {
		return false
	}
	if p.Credential == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(p.Credential)) == 1
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: code, Message: message})
}
