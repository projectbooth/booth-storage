# 0006: `s3`-kind credential broker provider — design answers to `booth-lakehouse`'s constraints

Status: **design only, not implemented** — `booth-core`'s broker routing (ADR 0080) hasn't
landed, so there is no manifest field or request/response shape to build against yet. This
records the three questions `booth-lakehouse`'s first pass (ADR 0084) sharpened, so
implementation follows a settled plan instead of starting cold once the contract exists.

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

## What's still genuinely open, left to `booth-core`'s contract

- The actual manifest field / registration shape for declaring "I provide kind `s3`."
- The request/response envelope and how a provider is authorized to trust a forwarded request
  (`contracts/credential-broker.md`'s "not yet fixed" list).
- Whether "no session token allowed" is a formal part of the shared request vocabulary the
  broker defines, or an `s3`-kind-specific parameter this module alone interprets. §1 above
  assumes the latter is acceptable but the former would be cleaner if `postgres`-kind or a future
  kind ever has an analogous constraint.

Not building the provider endpoint itself in this pass — there's nothing to route to it yet, and
`booth-core`'s broker routing landing first is the correct order per the brief.
