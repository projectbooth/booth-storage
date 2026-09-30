# 0006: `s3`-kind credential broker provider — design answers to `booth-lakehouse`'s constraints

Status: **implemented** (ADR 0088 shipped the concrete broker mechanism) —
`internal/credentialbroker` (the `POST /internal/credentials` provider route) and
`internal/backend/s3/credentials.go` (`Config.MintScopedCredential`, the actual minting
logic). This file originally recorded a design pass written before the broker existed; it now
also records what was actually built and how it was verified. The three questions
`booth-lakehouse`'s first pass (ADR 0084) sharpened are unchanged from the original design —
implementation followed the plan below as written.

## 1. AWS IAM keys can't expire — when do we refuse?

The sharpened requirement isn't "issue an s3 credential"; it's two different shapes:

- **A general native credential** (access key, secret key, *optional* session token) — what
  most consumers of a broker grant can use.
- **A bare 2-tuple, no session token at all** — what Lakekeeper 0.13.6's static-key warehouse
  credential specifically requires, because it has no field to put one in.

These need different answers on AWS:

- **General shape → `sts:AssumeRole` against a system identity, per ADR 0039's original framing
  of native access.** This is the standard AWS-native way to get an expiring credential and is
  the right answer once a consumer can accept a session token (a direct Iceberg engine, DuckDB's
  `ATTACH` with `session_token` support, a future `booth-spark`). Requires an IAM role this
  module can assume and pass through `path`/`prefix`-scoped session policies — real config work,
  not built here, but not blocked on anything either.
- **Bare-2-tuple shape → refuse on an AWS-backed backend, always.** `sts:AssumeRole` output is a
  3-tuple; the session token isn't optional metadata, it's required on every request made with
  those temporary credentials. There is no AWS API that returns "an access key and secret that
  expire, no token." The only way to hand out a bare pair on real AWS is a permanent IAM user
  access key, which structurally cannot expire — the exact thing ADR 0080 says to refuse rather
  than hand out. **Self-rotation (a reaper deleting/deactivating IAM access keys after a TTL) was
  considered and rejected**: it doesn't produce a credential that actually stops working at
  expiry (only one we've promised to later revoke), which is a materially weaker guarantee than
  MinIO's real expiry and doesn't satisfy "every issued credential has a mandatory TTL" in the
  spirit ADR 0080 means it — plus it adds standing IAM permissions (create/delete access keys)
  and an operational cleanup job this module doesn't otherwise need. So: this decides whether
  `booth-lakehouse` works on AWS S3 at all, and the honest answer is **not for the bare-2-tuple
  shape, only MinIO (or another provider whose expiring-service-account credential is a genuine
  bare pair) can serve that request** — refused cleanly (ADR 0080's required behavior), not
  degraded silently.

Implementation consequence: the provider's response/request shape needs a way to say "no
session token, or refuse" as a distinct request parameter from "session token OK if you have
one" — not inferred from backend kind alone, since the same AWS backend can serve the general
shape while refusing the bare one.

## 2. The grant carries its resolved location

Already naturally available: `internal/backend/s3.Config` (`internal/backend/s3/s3.go`) already
holds exactly `Endpoint`, `Region`, `Bucket`, `Prefix`, `PathStyle` per registered backend — this
is ADR 0045's `{backendId, path}` → real-bucket mapping, already private to this module. The
broker-grant response for `s3` kind should be a superset of that Config plus the minted
credential and its expiry: `{accessKeyId, secretAccessKey, sessionToken?, expiresAt, endpoint,
bucket, keyPrefix, region, pathStyle}`, with `keyPrefix` being the backend's configured `Prefix`
joined with the request's own scoped path — the caller never sees or computes that join itself.
`sessionToken` must be omitted from the response entirely (not sent empty) when the bare-2-tuple
shape was requested, matching Lakekeeper's own field absence rather than an empty-string
placeholder.

## 3. MinIO-specific floors, as the reference implementation

- **15-minute TTL floor.** A requested TTL below 15 minutes is **clamped up to 15 minutes**, not
  refused — the caller asked for "at most this long," and MinIO's own floor is a provider
  capability limit, not a scope-narrowing problem ADR 0080's refusal rule is about. The *ceiling*
  side (broker/core's default TTL cap) is a separate, stricter-than-`grant()` concern that's
  core's to set; this only fixes MinIO's floor as a clamp on the low end.
- **`s3:GetBucketLocation` needs its own policy statement, with no `s3:prefix` condition** —
  MinIO rejects that condition on that action. Any inline-policy generator for a MinIO grant
  needs at least two statements: one ordinary prefix-scoped statement for the object/bucket data
  actions, and a second, unconditional statement granting `s3:GetBucketLocation` on the bucket
  alone. Worth a unit test once built, since this is exactly the kind of MinIO quirk that's easy
  to silently regress.

## What ADR 0088 fixed, and how the design above landed

- **Manifest field**: `providesCredentials: {kinds: ["s3"]}`, rendered only when
  `credentialBroker.enabled` is set (off by default — a new privileged capability, not an
  operational convenience like database provisioning; see the chart's own comment).
- **Provider path/auth**: `POST /internal/credentials` (`credentialbroker.ProviderPath`),
  authenticated by comparing the presented `Authorization: Bearer` against this module's own
  copy of the Secret `booth-credential-broker-provider-credentials` (`credential` key) —
  **a constant-time string comparison, not a re-derived HMAC**. Core never hands this module the
  raw HMAC key it derives that credential from (`credentialbroker.Keys` is core-only, and the
  provisioned Secret carries the finished value); the contract itself says a provider "needs its
  own copy of the shared secret from the delivered Secret, not this package," so comparing
  against that stored copy achieves the identical property (only a holder of core's key could
  have produced that exact value) without this module ever needing the key.
- **Request/response wire shape for `s3`**: adopted verbatim from `booth-lakehouse`'s own
  already-built, already-tested client (`client/src/booth_lakehouse/broker.py`,
  `tests/integration/fakecore.py`) rather than inventing a second one — `scope: {backendId,
  path}`, `options: {sessionToken: "forbidden"|"allowed"}` for exactly the §1 split below, and a
  response `credential` carrying `endpoint/region/bucket/keyPrefix/pathStyle/accessKeyId/
  secretAccessKey/sessionToken?` per §2. `options.sessionToken` absent defaults to `"allowed"`
  (a session token is fine if the credential naturally has one — it does not require one).
- **§1 landed narrower than first drafted, in one useful way**: MinIO's `AddServiceAccount`
  (an expiring service account) always returns a bare pair, so it alone satisfies *both*
  `options.sessionToken` values for a MinIO-backed (any backend with a configured `Endpoint`)
  backend — no separate STS code path was needed there after all. Real AWS S3 (empty
  `Endpoint`) refuses both values today: the bare-pair case is the structural impossibility
  described below; the session-token-allowed case would need `sts:AssumeRole` against a system
  identity, which has no config surface in this module yet (`assumeRoleArn` was never added) —
  refused as "not implemented" rather than attempted half-built.
- **§3 built exactly as designed**: `scopedPolicy` in `credentials.go` produces the
  three-statement shape (prefix-scoped data actions, unconditional `GetBucketLocation`,
  `ListBucket` with the `s3:prefix` `StringLike` condition), plus one case the original design
  didn't call out: a backend/request with **no prefix at all** (a whole-bucket grant) needed its
  own branch so the resource ARN reads `bucket/*` rather than `bucket//*` — a leading-slash
  resource would match no real object key and silently deny everything. Covered by
  `TestScopedPolicy`.

## How this was verified

- **Deterministic, no real MinIO needed**: `internal/credentialbroker/provider_test.go` covers
  authentication (including that an unconfigured/empty credential refuses even an empty bearer,
  never matching on two empty strings), request validation, kind/backend-kind routing errors,
  the AWS refusal path, a minting-layer failure being relayed as `502`, and — the part that
  matters most for real interop — the exact success response shape (`leaseId/kind/expiresAt/
  scope/credential` with every field `booth-lakehouse`'s real client parser requires), the audit
  log recording the issuance without ever containing the minted secret, and that
  `options.sessionToken` correctly reaches `MintRequest.AllowSessionToken`.
  `internal/backend/s3/credentials_test.go` covers `scopedPolicy` directly and the AWS-refusal /
  input-validation paths with no network at all.
- **Real MinIO** (`TestMintScopedCredential_RealMinIO*`, same `BOOTH_TEST_S3_ENDPOINT`-gated
  pattern as `s3_test.go`): mints a real expiring service account, then uses the *minted*
  credential (not the admin's) to prove an in-scope object is readable, an out-of-scope one
  isn't, a read-only grant can't write, and the TTL floor/ceiling clamp both directions. Written
  against a blocked emulator (ADR 0087) and not executable at the time; **run for real once ADR
  0091's shared mirror (`ghcr.io/projectbooth/minio-test`) was published** — and it immediately
  caught a real bug the design pass had no way to find without a real server: MinIO's 15-minute
  floor is a *strict* `>`, not `≥`. Clamping to exactly `15 * time.Minute` landed right on that
  boundary, and the time a request actually spends in flight was enough to push the arrival
  below it, so `AddServiceAccount` failed with "invalid service account expiration" — not always
  (it raced request latency), but reliably enough to fail outright in this environment. Fixed by
  clamping to `15m5s`, matching the few seconds of slack `booth-lakehouse`'s own reference
  implementation (`tests/integration/fakecore.py`'s `max(ttl, 905)`) already carries for the
  identical reason. Full suite (this module's, real MinIO/Azurite/Postgres included) passes.
- **Not attempted: a real cross-process test against `booth-core`'s actual binary.** Looked at
  seriously (core has a real dev mode — `BOOTH_DEV_REGISTRY_PATH` — that needs no Kubernetes
  cluster), but `internal/devregistry`'s file format has no field for `providesCredentials` at
  all (nor `database`/`workloadIdentity` — it's deliberately scoped to gateway routing only, not
  the provisioning-controller-dependent fields), so `findProvider("s3")` can never succeed
  against a dev-mode registry: core's real broker *routing* structurally requires a real
  Kubernetes cluster with the CRD controller running, which is `test/integration/README.md`'s
  already-documented, pre-existing layer-3 gap, not a new one. Given that, the two test layers
  above are the strongest verification achievable without one.
