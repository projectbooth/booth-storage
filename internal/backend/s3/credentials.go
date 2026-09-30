package s3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/minio/madmin-go/v3"

	"github.com/projectbooth/booth-storage/internal/backend"
)

// This file is the s3 kind's side of the ADR 0080 credential broker (contracts/credential-
// broker.md, ADR 0088): given this backend's own real admin credentials (ADR 0039 — the
// module holds them, nobody else ever does) and a request scoped to one path inside it, mint
// a short-lived credential narrower than the real one and never return the real one itself.
//
// Design record: docs/decisions/0006-s3-credential-broker-provider-design.md, written before
// booth-core's broker existed; this implements it.

// ErrScopeNotSupported means this backend cannot satisfy the request as narrowly as asked —
// the credential-broker contract's "refuse rather than widen" (ADR 0080). Distinct from an
// infrastructure failure: this is refused by design, not by accident.
var ErrScopeNotSupported = errors.New("this backend cannot issue a scoped s3 credential for this request")

// minServiceAccountTTL is MinIO's own floor on an expiring service account's lifetime
// (verified against a real server by booth-lakehouse's ADR 0084 first pass, not assumed). A
// request for less is clamped up to it, per the broker contract: a provider's documented
// capability floor may clamp back past the broker's own TTL ceiling; that's the provider's
// limit, not a violation of it.
//
// The floor itself is a strict "greater than 15 minutes," not "at least" — verified directly
// against a real server here: an expiration of exactly now+15m is rejected
// ("invalid service account expiration"), now+15m1s succeeds. Clamping to exactly 15 minutes
// lands right on that boundary, and the time the request actually spends in flight erodes the
// margin further, so it fails intermittently depending on latency. booth-lakehouse's own
// reference implementation (tests/integration/fakecore.py) already carries a few seconds of
// slack for the same reason (`max(ttl, 905)`); this does the same, deliberately.
const minServiceAccountTTL = 15*time.Minute + 5*time.Second

// MintRequest is one scoped-credential ask, already authorized by the broker (ADR 0080: "a
// provider never re-derives trust, it trusts the broker's forwarded, already-authorized
// request") — Path and Access are the only things this backend still needs to decide *how*
// to scope the credential.
type MintRequest struct {
	// Path confines the credential to this sub-prefix of the backend (relative, no leading
	// slash — the same shape backend.CleanPrefix accepts). Empty means the whole backend.
	Path string
	// Access is "read" or "readwrite" (contracts/credential-broker.md's shared field); the
	// broker has already checked the caller's role permits it.
	Access string
	// TTL is what the broker is willing to grant (already clamped to its own ceiling, ADR
	// 0088's 5-minute default). This backend may still clamp it upward to its own floor.
	TTL time.Duration
	// AllowSessionToken is false when the caller needs a bare access-key/secret-key pair with
	// no session token at all (Lakekeeper 0.13.6's static-key warehouse credential has no
	// field for one — docs/decisions/0006 §1). true means a session token is fine if the
	// credential naturally has one; it does not require one to be present.
	AllowSessionToken bool
}

const (
	AccessRead      = "read"
	AccessReadWrite = "readwrite"
)

// MintedCredential is a short-lived credential scoped to one request, plus the resolved
// real-world location it's good for — the only place that mapping is known (ADR 0045), so a
// caller never has to understand this backend's own configuration.
type MintedCredential struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // "" when none was issued (always true for the AllowSessionToken=false case)
	Endpoint        string
	Region          string
	Bucket          string
	KeyPrefix       string
	PathStyle       bool
	ExpiresAt       time.Time
}

// MintScopedCredential derives a credential narrower than admin's real one, scoped to
// req.Path inside this backend and no wider.
//
// Only a backend with a configured Endpoint (self-hosted, e.g. MinIO) can do this today: it
// uses MinIO's admin API to create an expiring service account — a genuine bare key pair that
// actually stops working at its expiry, not a promise to revoke one later. Real AWS S3 (empty
// Endpoint) is refused unconditionally: AWS's only mechanism for an expiring credential is STS
// AssumeRole, whose output is inherently a 3-tuple (access key, secret key, session token) —
// there is no AWS API that returns an expiring bare pair, so AllowSessionToken=false can never
// be satisfied there, structurally, not as a missing feature. AllowSessionToken=true would be
// satisfiable via `sts:AssumeRole` against a system identity (docs/decisions/0006 §1), but that
// needs an assume-role-arn this module has no config surface for yet, so it is also refused
// here for now, distinctly (not implemented, rather than impossible) — see the design doc.
func (cfg Config) MintScopedCredential(ctx context.Context, admin Credentials, req MintRequest) (MintedCredential, error) {
	if req.Access != AccessRead && req.Access != AccessReadWrite {
		return MintedCredential{}, fmt.Errorf("%w: access must be %q or %q", ErrScopeNotSupported, AccessRead, AccessReadWrite)
	}
	prefix, err := backend.CleanPrefix(req.Path)
	if err != nil {
		return MintedCredential{}, fmt.Errorf("%w: %v", ErrScopeNotSupported, err)
	}
	if admin.AccessKeyID == "" || admin.SecretAccessKey == "" {
		return MintedCredential{}, fmt.Errorf("%w: this backend has no admin credentials of its own to derive a scoped one from", ErrScopeNotSupported)
	}

	if cfg.Endpoint == "" {
		if !req.AllowSessionToken {
			return MintedCredential{}, fmt.Errorf("%w: real AWS S3 has no way to issue an expiring credential without a session token (plain IAM access keys never expire, and STS output always includes one) — see docs/decisions/0006", ErrScopeNotSupported)
		}
		return MintedCredential{}, fmt.Errorf("%w: real AWS S3 credential issuance (via sts:AssumeRole) is not implemented yet, only self-hosted (MinIO-compatible) backends are", ErrScopeNotSupported)
	}

	keyPrefix := strings.Trim(strings.TrimSuffix(cfg.Prefix, "/")+"/"+strings.TrimSuffix(prefix, "/"), "/")
	host, secure, err := cfg.hostAndTLS()
	if err != nil {
		return MintedCredential{}, fmt.Errorf("%w: %v", ErrScopeNotSupported, err)
	}

	adm, err := madmin.New(host, admin.AccessKeyID, admin.SecretAccessKey, secure)
	if err != nil {
		return MintedCredential{}, fmt.Errorf("creating admin client: %w", err)
	}

	ttl := req.TTL
	if ttl < minServiceAccountTTL {
		ttl = minServiceAccountTTL
	}
	expiry := time.Now().Add(ttl)

	pol, err := json.Marshal(scopedPolicy(cfg.Bucket, keyPrefix, req.Access))
	if err != nil {
		return MintedCredential{}, fmt.Errorf("building scoped policy: %w", err)
	}
	created, err := adm.AddServiceAccount(ctx, madmin.AddServiceAccountReq{
		Policy:      pol,
		Expiration:  &expiry,
		Name:        "booth-credential-broker",
		Description: "short-lived, ADR 0080 credential-broker grant — booth-storage",
	})
	if err != nil {
		return MintedCredential{}, fmt.Errorf("minting a scoped service account: %w", err)
	}

	minted := MintedCredential{
		AccessKeyID: created.AccessKey, SecretAccessKey: created.SecretKey,
		Endpoint: cfg.Endpoint, Region: cfg.Region, Bucket: cfg.Bucket, KeyPrefix: keyPrefix, PathStyle: cfg.PathStyle,
		ExpiresAt: expiry,
	}
	if req.AllowSessionToken {
		// MinIO's expiring service accounts don't issue one (created.SessionToken is always
		// empty here), but carry through anything a future minting path might return.
		minted.SessionToken = created.SessionToken
	}
	return minted, nil
}

// scopedPolicy is the IAM-shaped inline policy for one prefix, matching MinIO's two
// documented quirks (booth-lakehouse's ADR 0084 first pass, verified against a real server,
// not assumed): s3:GetBucketLocation must be its own statement with no s3:prefix condition
// (MinIO rejects that condition on that action), and s3:ListBucket needs the condition
// instead, scoped with the trailing "/*" MinIO/AWS both expect for "this prefix and below".
//
// keyPrefix == "" means the whole bucket (a backend registered with no prefix, granted with
// no further path) — object resources then match "bucket/*", not "bucket//*", and the list
// condition is unconstrained ("*") rather than a literal empty prefix; a leading-slash ARN
// would match no real object key at all and silently deny everything.
func scopedPolicy(bucket, keyPrefix, access string) map[string]any {
	actions := []string{"s3:GetObject"}
	if access == AccessReadWrite {
		actions = append(actions, "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload")
	}
	bucketARN := "arn:aws:s3:::" + bucket
	objectResource, listCondition := bucketARN+"/*", []string{"*"}
	if keyPrefix != "" {
		objectResource = bucketARN + "/" + keyPrefix + "/*"
		listCondition = []string{keyPrefix, keyPrefix + "/*"}
	}
	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{"Effect": "Allow", "Action": actions, "Resource": []string{objectResource}},
			{"Effect": "Allow", "Action": []string{"s3:GetBucketLocation"}, "Resource": []string{bucketARN}},
			{"Effect": "Allow", "Action": []string{"s3:ListBucket"}, "Resource": []string{bucketARN},
				"Condition": map[string]any{"StringLike": map[string]any{"s3:prefix": listCondition}}},
		},
	}
}
