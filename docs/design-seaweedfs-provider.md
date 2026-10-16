# Design: a `seaweedfs` credential-broker provider

Status: design note, 2026-10-16, written against ADR 0118 (accepted). No product code changed.
Checked against `main` (`df0330e`), `internal/credentialbroker/provider.go`,
`internal/backend/s3/credentials.go`, `internal/registry/migrations/`, the two prior spikes
(`docs/design-homelab-warehouse-store.md`, branch `design-homelab-warehouse-store`;
`docs/design-seaweedfs-iam-spike.md` commit `b3c7863`, branch `design-seaweedfs-iam-spike`), and a
third, new spike run for this note specifically (section 1 below - a real `aws-chunked`
trailer-checksum test using the AWS SDK for Go v2, not botocore). Spike containers
(`chrislusf/seaweedfs:latest`) ran locally in Docker and were torn down before this note was
written; nothing here touches the homelab. This note assumes ADR 0118's ruling: SeaweedFS is the
S3 store, the provider mints an ephemeral IAM user + prefix policy + bare access key (no session
token), STS is not used.

## 1. The `aws-chunked` trailer-checksum gap - closed

The second spike (`design-seaweedfs-iam-spike.md` section 3) could only get `botocore` to send
checksums as a plain header for `DeleteObjects`, never a genuine `aws-chunked` trailer, and didn't
have time to try a real AWS SDK. This note closes that gap with the AWS SDK for Go v2
(`github.com/aws/aws-sdk-go-v2` `v1.47.3`, `service/s3` `v1.114.3` - current as of this spike, not
pinned to anything `booth-spark` uses, since this question is about the *protocol*, not
`booth-spark`'s specific jar versions), against `chrislusf/seaweedfs:latest` (`30GB 4.48`, the same
build the prior two spikes used). A custom `http.RoundTripper` dumped every outbound request's
headers and raw body bytes before they reached the wire, so this is a direct observation of what
was actually sent, not an inference from success/failure.

**Headline finding: the SDK will only send a genuine `aws-chunked` trailer checksum over TLS.**
Over plain HTTP (the transport every spike so far has used, since none stood up a cert), the SDK
either refuses outright (`"compute input header checksum failed, unseekable stream is not
supported without TLS and trailing checksum"` for an unseekable streaming body under the default
checksum setting) or silently falls back to a plain header/no-checksum mode for anything with a
known length. This is a client-library security gate, not a SeaweedFS limitation - it was never
going to be observed by this or either prior spike until a TLS listener was involved. Once
SeaweedFS was restarted with `-s3.cert.file`/`-s3.key.file` (a self-signed cert; `weed` server
takes these flags natively, no proxy needed) and the client pointed at `https://`, this is what
really happened, status codes included:

| Operation | Default (`when_supported`) over TLS | `when_required` over TLS |
|---|---|---|
| `PutObject`, known-length body (1 KiB) | **Genuine trailer**: `Content-Encoding: aws-chunked`, `X-Amz-Trailer: x-amz-checksum-crc32`, `X-Amz-Content-Sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER`, body literally framed as `400\r\n<1024 bytes>\r\n0\r\nx-amz-checksum-crc32:...\r\n\r\n` (`400` hex = 1024). **200 OK.** | `X-Amz-Content-Sha256: UNSIGNED-PAYLOAD`, no checksum computed at all, plain body. **200 OK.** |
| `PutObject`, unseekable streaming body (`io.Pipe`) | Same genuine trailer as above. **200 OK** - this is the exact case that failed outright over plain HTTP in both the Go SDK (this spike) and, per the second spike's own account, botocore's reasoning for never producing one. TLS is what unblocks it. | Same `UNSIGNED-PAYLOAD`, no checksum. **200 OK.** |
| `UploadPart` x2, 5 MiB parts | Same genuine trailer, framed as `500000\r\n<5242880 bytes>\r\n0\r\n...checksum...` (`500000` hex = 5242880). **200 OK** for both parts; `CompleteMultipartUpload` succeeds. | `UNSIGNED-PAYLOAD`, no checksum. **200 OK.** |
| `DeleteObjects` (batch) | **Never trailer mode, under any setting or transport.** Always a plain header (`X-Amz-Checksum-Crc32: ...`) with a real, non-streaming `X-Amz-Content-Sha256` hash and an ordinary (non-chunked) body - this operation's body (a small, in-memory XML key list) is structurally never a streaming/unknown-length payload, so the SDK's streaming-trailer code path never engages, TLS or not. **200 OK.** | **Still carries a checksum header even here** (`X-Amz-Checksum-Crc32: ...`) - `DeleteObjects` is one of the operations the SDK always checksums regardless of the `when_supported`/`when_required` setting, because S3 requires one for this API. **200 OK.** |

**Answering the question directly: does `when_required` fix a trailer-mode failure?** No, and the
reason matters: there was nothing for `when_required` to fix in the trailer sense, because once TLS
is present, the default (`when_supported`) setting already produces a working, SeaweedFS-accepted
trailer for every operation that can use one. The real, reproducible failure this spike found was
over **plain HTTP**, not from the checksum-calculation setting - and there, `when_required` *does*
change the outcome for the one case that mattered: an unseekable streaming `PutObject` body, which
hard-fails under `when_supported` + HTTP (the SDK won't send an unauthenticated trailer over an
insecure channel and has no other way to checksum a stream it can't rewind) but succeeds under
`when_required` + HTTP, because `when_required` skips checksum computation entirely for an
operation that doesn't strictly need one (`PutObject` isn't in S3's "checksum mandatory" set) and
falls back to `UNSIGNED-PAYLOAD`. For a known-length body, `when_supported` + HTTP already works in
plain-header mode, so `when_required` changes nothing there. And for `DeleteObjects`, which *is*
checksum-mandatory, `when_required` cannot avoid the checksum at all (that's the operation's own
requirement, not this setting's), so the question doesn't apply to it the same way.

**What this means for ADR 0118's verification gate 3** ("aws-chunked trailer checksums ... are
tested on PUT and DeleteObjects. The spike confirmed header mode only. If trailers fail, fix on the
client side"): trailers do **not** fail against SeaweedFS - they were simply never exercised
without TLS. **The real client-side implication is the opposite of what the gate anticipated**: a
production deployment needs TLS between Spark/Lakekeeper and SeaweedFS for the AWS SDK's default
trailer behavior to engage at all; without it, large/streaming uploads either fail outright (older
or default-configured SDKs) or silently downgrade (if the client happens to set
`when_required`). This is a deployment requirement (see below, and `docs/design-seaweedfs-
deployment.md`), not a code change in this module - booth-storage itself never originates S3
traffic to SeaweedFS (it only mints credentials; Spark and Lakekeeper talk to SeaweedFS directly),
so there is nothing for `booth-storage`'s own `s3.go` client to configure differently here.
`DeleteObjects` is unaffected by any of this either way, confirmed working over both HTTP and TLS,
under both checksum settings, in every spike run so far.

**What I could not verify here**: the real AWS SDK for Java v2 (what `booth-lakehouse`'s Lakekeeper
and `booth-spark`'s Hadoop S3A actually run) behaving identically to the Go SDK's trailer logic -
this spike used Go specifically because it's this repo's own ecosystem and fastest to stand up, not
because it's the consumer that matters. The two SDKs' flexible-checksum implementations are
independently written (same AWS feature, different language teams) and the TLS gate found here is
plausible but not proven to be a cross-SDK rule - Verification gate 1 (Hadoop S3A against a scoped
key, in `booth-spark`'s own CI, which has Maven) is exactly where this should be re-confirmed,
ideally over TLS, not just over HTTP as the prior spikes did.

## 2. The `seaweedfs` provider's configuration

This module's existing `s3.go` backend (list/read/write/delete) is **unchanged** - SeaweedFS
speaks the same S3 data-plane API, so a SeaweedFS-backed registration is still `backend.KindS3`,
using the exact same `Config`/`Credentials` shape and the exact same `minio-go` client this module
already has (confirmed in the create-only and multipart spikes: ordinary reads/writes/lists/
deletes already work against SeaweedFS with no code change). What's new is only the
**credential-minting** path - `internal/backend/s3/credentials.go`'s `Config.MintScopedCredential`
today has exactly one minting strategy (MinIO's admin API), gated on `cfg.Endpoint != ""`.
SeaweedFS needs a second strategy, not a second backend kind.

**Proposed config addition**, parallel to the existing `Config`/`Credentials` shape
(`internal/backend/s3/s3.go:22-43`):

```go
type Config struct {
    Endpoint   string
    Region     string
    Bucket     string
    Prefix     string
    PathStyle  bool
    // CredentialProvider selects how MintScopedCredential derives a scoped credential for
    // this backend. "" or "minio-admin" (default, unchanged): MinIO's admin API
    // (AddServiceAccount). "seaweedfs-iam": the embedded IAM CreateUser/PutUserPolicy/
    // CreateAccessKey flow this note describes. Anything else is a config-time validation
    // error, not a silent fallback.
    CredentialProvider string `json:"credentialProvider,omitempty"`
}
```

An operator registering a SeaweedFS-backed backend sets `credentialProvider: "seaweedfs-iam"` in
the backend's config (the same JSON blob `POST /api/admin/backends` already accepts) alongside the
ordinary S3 fields. The admin credential this module holds for that backend (ADR 0039 - the same
Kubernetes Secret flow every S3 backend already uses) is the IAM **admin** identity's access
key/secret (`CreateUser`/`PutUserPolicy`/`CreateAccessKey`/`DeleteAccessKey`/`DeleteUser` all need
this identity's own credentials, the same way `AddServiceAccount` needs MinIO's admin credentials
today) - no new credential *shape*, just a different identity with different permissions on the
SeaweedFS side.

**The IAM endpoint is the S3 endpoint** - confirmed directly in both spikes: SeaweedFS's embedded
IAM API is served on the same port as the S3 API, not a separate one. So no new endpoint field is
needed; `MintScopedCredential`'s SeaweedFS path reuses `cfg.Endpoint`/`cfg.hostAndTLS()` exactly as
the MinIO path does, just pointed at a `sts`/IAM-shaped AWS SDK client
(`github.com/aws/aws-sdk-go-v2/service/iam`, matching the raw calls both spikes made with
`boto3`'s `iam` client) instead of `madmin.New`.

**The mint call sequence**, mirroring section 1 of `design-seaweedfs-iam-spike.md` exactly:

```go
func (cfg Config) mintSeaweedFSIAM(ctx context.Context, admin Credentials, req MintRequest) (MintedCredential, error) {
    // 1. A fresh, per-lease IAM username (e.g. "booth-" + leaseID, or workspace+backend+short
    //    random suffix - must be unique per mint, never reused, so the sweeper can delete one
    //    lease's user without touching another's).
    // 2. iam.CreateUser(UserName: username)
    // 3. iam.PutUserPolicy(UserName: username, PolicyName: "scope", PolicyDocument: scopedPolicy(...))
    //    - the same two-statement shape s3.go's scopedPolicy already builds for MinIO (object
    //    actions unconditioned; ListBucket conditioned on s3:prefix), reused verbatim: the
    //    second spike's section 6 policy-authoring trap (don't mix an s3:prefix condition into a
    //    statement that also grants object actions) applies identically here, since it's a
    //    real IAM policy-evaluation rule, not a MinIO or SeaweedFS quirk.
    // 4. iam.CreateAccessKey(UserName: username) -> AccessKeyId, SecretAccessKey (no token, no
    //    Expiration field - confirmed in both spikes).
    // 5. Record the lease (see section 4) before returning, not after: a crash between
    //    CreateAccessKey and the lease INSERT would otherwise mint a credential this module
    //    doesn't know to sweep - the live key would leak until someone notices. Mint, then
    //    record, then return; if the record write fails, immediately call
    //    DeleteAccessKey+DeleteUser to undo the mint rather than return an untracked credential.
}
```

Request-size/TTL handling mirrors the existing MinIO path's `req.TTL`/`minServiceAccountTTL`
pattern, but **SeaweedFS's bare-pair path has no floor to clamp to** (unlike MinIO's real
15-minute server-enforced minimum) - there's nothing stopping this module from honoring the
broker's own TTL exactly, including shorter leases than MinIO could ever issue. `AllowSessionToken:
false` is the only mode this path ever satisfies (mirrors the real constraint, not a policy
choice) - a request with `AllowSessionToken: true` against a `seaweedfs-iam`-configured backend
should be refused with `ErrScopeNotSupported`, the same "refuse rather than widen" answer the
existing MinIO path gives real AWS S3's `AllowSessionToken: true` case today
(`credentials.go:110-115`) - if a future caller needs a session-token-tolerant credential from a
SeaweedFS-backed backend, that's `GetFederationToken` (confirmed working once the base identity has
a real attached policy, per the second spike's section 6), a structurally separate code path this
note doesn't design, since nothing in this module's current `MintRequest` contract asks for it.

## 3. Scope echo

Per the second spike's own finding (section 5): **scope-echo is this module's job, not the backing
store's.** `internal/credentialbroker/provider.go`'s response already echoes
`BackendID`/`Path`/`Access` from the inbound request verbatim (`s3ScopeEcho`,
`provider.go:129-133,267`), regardless of which provider actually minted the credential - the
MinIO path satisfies this today purely by construction (it never changes what it was asked for, it
only ever narrows-or-refuses). The SeaweedFS path is identical: `MintScopedCredential`'s
`req.Path`/`req.Access` **are** what gets echoed; the provider's job is only to make sure what it
actually built (the IAM policy's resource ARNs, section 2 step 3) matches what it was asked to
scope, which is exactly what `scopedPolicy(bucket, keyPrefix, access)` already guarantees by
constructing the ARN directly from the same `keyPrefix` the response echoes - no separate
verification step is needed because there's no path for the policy and the echo to diverge: both
are built from the same `req.Path`/`req.Access` values in the same function call.

## 4. Lease table and sweeper

A bare SeaweedFS IAM key has **no native expiry** (confirmed, both spikes) - unlike MinIO's
expiring service accounts, which this module today can simply mint-and-forget (MinIO itself
refuses to honor the credential past its `Expiration`). This is genuinely new durable state for
this module, the same category of gap the first spike's Garage sketch (section 6, not built)
already flagged, except here the mechanism is actually worth building - SeaweedFS's bare-pair path
is the one that matches Lakekeeper's own contract (ADR 0118's whole premise).

**Migration**, in the style of `internal/registry/migrations/0003_storage_upload_grants.sql`:

```sql
-- SeaweedFS IAM leases: a bare access key/secret pair has no native expiry (unlike MinIO's
-- expiring service accounts), so this module tracks its own and a sweeper deletes what's expired.
CREATE TABLE storage_seaweedfs_leases (
    id             UUID        NOT NULL,
    workspace      TEXT        NOT NULL,
    backend_id     TEXT        NOT NULL,
    iam_user_name  TEXT        NOT NULL,
    access_key_id  TEXT        NOT NULL,
    prefix         TEXT        NOT NULL,
    access         TEXT        NOT NULL, -- "read" | "readwrite"
    expires_at     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    swept_at       TIMESTAMPTZ,          -- NULL until the sweeper has confirmed cleanup
    PRIMARY KEY (id)
);

-- The sweeper's own lookup: every lease past expiry that hasn't been confirmed cleaned up yet.
CREATE INDEX storage_seaweedfs_leases_sweep ON storage_seaweedfs_leases (expires_at) WHERE swept_at IS NULL;
```

`iam_user_name` is a fresh, per-lease username (never reused), so deleting one lease's user can
never affect another's - no shared/pooled IAM users, which would turn "delete this lease's access"
into "figure out if anyone else still needs this user first."

**The sweeper**: a background goroutine in `cmd/storage/main.go`, alongside the HTTP server (same
process, no new deployment unit), polling on a fixed interval. **Interval recommendation: 1
minute** - short enough that the "lease plus one sweep interval" bound ADR 0118 ruling 3 describes
stays tight relative to the broker's own default 5-minute TTL ceiling (ADR 0088), not so short that
it hammers SeaweedFS's IAM API with mostly-empty polls. Each cycle:

1. `SELECT * FROM storage_seaweedfs_leases WHERE swept_at IS NULL AND expires_at < now()`.
2. For each: `iam.DeleteAccessKey(UserName, AccessKeyId)` **then** `iam.DeleteUser(UserName)` - in
   that order, because (mirroring real AWS IAM, which SeaweedFS's IAM API is modeled on) a user
   with an attached access key typically can't be deleted until the key is removed first; deleting
   the key first also means a `DeleteUser` crash leaves a *keyless* orphaned user (harmless - no
   credential, nothing to leak) rather than a *user-less* orphaned key (meaningless on its own, but
   worth avoiding for tidiness).
3. `DeleteAccessKey`/`DeleteUser` returning "no such user"/"no such key" is treated as success, not
   a failure - the lease is already gone from SeaweedFS's side (manually cleaned up, or a previous
   sweep crashed after deleting but before marking `swept_at`); the row still gets `swept_at =
   now()`.
4. On any other error (SeaweedFS unreachable, a real API error): **do not mark `swept_at`**, log
   it, and let the next cycle retry. **This is the fail-closed behavior ADR 0118 ruling 3
   requires**: an unreachable SeaweedFS does not stop the sweeper from running (other leases still
   get cleaned up), does not crash the process, and does not pretend a failed cleanup succeeded -
   it just leaves that one lease's `swept_at` NULL so it's retried every subsequent cycle until it
   succeeds. The actual exposure window in the worst case (SeaweedFS down for an extended period)
   is bounded only by the prefix policy's own scope (never wider than the one bucket/prefix the
   lease was minted for) - consistent with ADR 0118's own framing ("leaked keys are bounded by the
   lease plus one sweep interval **and by the prefix policy**").
5. Emit one summary log line per cycle (stdout, matching every other audit line this module already
   emits - ADR 0022's "no logging API to integrate against", confirmed: this module has no
   Prometheus or other metrics endpoint today, so a grep-able log line is the real, available
   mechanism, not a gap specific to this feature):
   ```
   audit: seaweedfs sweep workspace=<ws> backend=<id> swept=3 failed=0 outstanding=0 oldest_outstanding_age=0s
   ```
   `outstanding` is the count of unswept-but-not-yet-expired leases (informational); the one worth
   alerting on is **`oldest_outstanding_age`** - the age, past its `expires_at`, of the oldest lease
   still waiting on a successful sweep. Zero in the healthy case; growing without bound is exactly
   the "SeaweedFS has been down for a while" signal an operator needs. This is the "metric for
   outstanding users" ADR 0118's consequences section asks for - expressed as a log-derived value
   today, and the natural first real Prometheus gauge if this module ever adds metrics
   instrumentation (it has none today, confirmed by a repo-wide check for any `prometheus`/metrics
   dependency).

## 5. Renewal

Confirmed, both spikes: a bare IAM key cannot be extended, only replaced. Renewal is **mint a
brand-new user + key for the same scope, insert a new lease row, and let the old lease's own
expiry (and the sweeper) retire the old one on its own schedule** - never synchronously delete the
old credential at renewal time. This mirrors the existing MinIO path's own pattern exactly
(`credentials.go`'s `MintScopedCredential` always mints fresh, never extends), and avoids the race
the second spike's section 4 flagged: a renewal that raced a sweep of the *old* lease could
otherwise leave a caller with no valid credential for a moment if the old one were deleted
synchronously and the new one hadn't propagated yet.

**When does a caller renew?** This module doesn't decide that - the broker contract already puts
TTL in the caller's hands (`ttlSeconds` on the inbound request, `contracts/credential-broker.md`,
ADR 0088), and Lakekeeper's own renewal cadence (re-requesting before `expiresAt`) is
`booth-lakehouse`'s concern, not this provider's. What this provider guarantees is only that
**every mint call, renewal or not, is indistinguishable from a first-time mint** - same IAM flow,
same lease row, same sweeper eligibility. There is no "renew this specific lease" API surface, by
design: the broker's contract has no such verb today, and adding one would be new scope beyond
what ADR 0118 asked for.

## 6. What happens when SeaweedFS is down

**Fails closed, no exceptions, no cached fallback.** A mint request that can't reach SeaweedFS's
IAM API returns an error from `MintScopedCredential`, which the existing provider handler already
turns into `502 mint_failed` ("the backend could not be reached to mint a credential",
`provider.go:252`) - this is the same path every other mint failure already takes, so **no new
error-handling code is needed in `provider.go` itself**, only in `credentials.go`'s new SeaweedFS
branch. This is consistent with every other "can't verify, so refuse" pattern already in this
module: the create-only capability probe gates S3 writes on a passed probe rather than guessing
(ADR 0117), and the grants-service revoke-principal identity fails closed if this module can't be
reached at all (ADR 0117 item 1a, `booth-streamlit`'s side). A SeaweedFS outage should read to a
caller exactly like "the credential broker is unavailable for this backend" - not like a slow or
degraded success.

## Questions for other repos

- **`booth-spark`**: re-run Verification gate 1 (Hadoop S3A against a scoped, non-admin key) in
  `booth-spark`'s own CI (it has Maven, this spike's environment didn't) - and do it over **TLS**,
  not plain HTTP, given section 1's finding that trailer-mode checksums are TLS-gated at the SDK
  level. Worth confirming the Java AWS SDK v2's trailer behavior independently rather than
  assuming it matches the Go SDK's.
- **`booth-lakehouse`**: Verification gate 2 (Lakekeeper itself creates a warehouse/namespace/
  table, writes metadata, purges, using a minted key) is unaffected by anything in this note -
  nothing here changes what credential shape Lakekeeper receives, only how booth-storage produces
  it internally.
- **`booth-core`**: none. The broker's wire contract needs no change (confirmed already,
  `design-native-s3-endpoint.md` section 1 - still unmerged but its finding here is just a fact
  about the existing contract, not a dependency on that note's own un-reviewed recommendation).

## What I could not verify

- The real AWS SDK for Java v2's trailer-checksum behavior (section 1) - this spike used Go.
- SeaweedFS's IAM API under real concurrent load or a multi-node deployment - single-node,
  single-client spike, same limitation both prior spikes carried.
- Whether `iam.DeleteAccessKey`/`DeleteUser` on a user that still has a live, in-use session
  (another request mid-flight using the about-to-be-deleted key) causes that in-flight request to
  fail cleanly or in some confusing partial way - not exercised; the sweeper design above assumes a
  clean failure is acceptable (the broker's own TTL is supposed to mean "don't expect this to still
  work"), but it wasn't observed directly.
- The exact SeaweedFS IAM API error shape for "no such user"/"no such key" (used in section 4
  step 3's idempotent-sweep logic) - inferred from standard AWS IAM error codes
  (`NoSuchEntityException`), not independently reproduced against SeaweedFS's own IAM error
  responses this pass.
