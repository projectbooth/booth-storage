# SeaweedFS IAM/STS deep-dive spike: a bare-pair path that actually works, and the STS fix

Status: spike results, 2026-10-16. No product code changed. Follows directly from
`docs/design-homelab-warehouse-store.md` (branch `design-homelab-warehouse-store`, commit
`d24bd51`+ — SeaweedFS's `GetFederationToken` returned unexplained `AccessDenied`) and
`docs/design-native-s3-endpoint.md` (branch `design-native-s3-endpoint`, kept deliberately
unmerged — the "build our own S3 endpoint" feasibility note). Spike containers (SeaweedFS
`chrislusf/seaweedfs:latest`, Garage `dxflrs/garage:v2.4.1`) ran locally in Docker and were
torn down before this note was written; nothing here touches the homelab.

**Headline finding, upside down from where the prior spike left it**: SeaweedFS's embedded
IAM's plain `CreateUser`/`PutUserPolicy`/`CreateAccessKey` path — not `GetFederationToken` —
is the one that actually matches Lakekeeper's bare-pair requirement, and it works correctly,
with real enforced prefix scoping, today. And the STS gap from the prior spike *is* now
root-caused and fixed (§6) — it turned out to need a different kind of policy on the base
identity than either spike had tried, not a dead end.

## 1. Credentials: a scoped IAM user and bare access key/secret pair

**Setup, exact calls used** (`weed` 30GB 4.48, embedded S3+IAM, both on port 8333):

```
weed server -s3 -s3.ip.bind=0.0.0.0 -s3.iam -s3.iam.readOnly=false \
  -s3.iam.config=/etc/seaweedfs/iam.json -dir=/data -ip.bind=0.0.0.0
```
`iam.json` carries only `{"sts": {"signingKey": "<base64 32 bytes>"}}` — required even for the
plain (non-STS) IAM API: without it, S3 startup logs `Failed to load IAM configuration: no
signing key found for STS service` and the server never reaches a working state at all. This
wasn't needed by the prior spike's note because it used the same flag already; confirmed here
independently.

An admin identity has to be seeded via `weed shell` before any IAM call works — the server
boots with **zero identities**, logging `S3 config file ... loaded no identities - every
request is denied until one is configured`:
```
echo 's3.configure -access_key admin_key -secret_key admin_secret_1234 -user admin -actions Admin -apply' | weed shell
```

Then, via `boto3`'s `iam` client against the S3 port (confirmed: the IAM API shares the S3
port, not a separate one):

```
CreateUser(UserName="nflapp")
PutUserPolicy(UserName="nflapp", PolicyName="IncomingPrefix", PolicyDocument={
  two statements: Get/Put/DeleteObject on "arn:aws:s3:::nfl-spike/incoming/*",
  and ListBucket on "arn:aws:s3:::nfl-spike" with Condition StringLike s3:prefix "incoming/*"
})
CreateAccessKey(UserName="nflapp")
  -> AccessKeyId=AKIA..., SecretAccessKey=... (a genuine two-field pair, no token, no
     Expiration field in the response at all)
```

All three calls succeeded on the first try, no retries, no guessing at an undocumented shape —
the AWS IAM API wire format just works against this server.

**Prefix limits verified to actually hold**, using the issued key (not admin's):

| Operation | Result |
|---|---|
| `PUT incoming/x.csv` (inside the granted prefix) | **Allowed** |
| `GET incoming/x.csv` | **Allowed** |
| `PUT other/x.csv` (different prefix, same bucket) | **Refused**, `AccessDenied` |
| `PUT root.csv` (bucket root) | **Refused**, `AccessDenied` |
| `GET root.csv` (an object an admin seeded outside the prefix) | **Refused**, `AccessDenied` |

This is real server-side enforcement, not a client-side illusion: the refused calls used the
scoped key exactly as issued, against objects that genuinely exist (seeded with the admin
key first), and got back `AccessDenied` from the server, not a local validation error.

## 2. Operations under the prefix

Exercised with the `boto3`/botocore S3 client (the same REST operations Hadoop S3A issues
under the hood) against the scoped `nflapp` key from §1. **Hadoop S3A itself was not
separately re-run this pass** — no Maven and no cached dependency jars were available in this
spike's environment, and rebuilding the classpath by hand without Maven wasn't worth this
budget given the prior spike already proved the JVM mechanism works end-to-end against
SeaweedFS with static credentials (`docs/design-homelab-warehouse-store.md` §4); what was new
and worth testing here was whether the *scoped, non-admin* key works for the same operation
set, which the identical wire-level calls below answer directly.

| Operation | Result |
|---|---|
| `PutObject` | **Pass** |
| `GetObject` | **Pass** — body and ETag round-trip correctly |
| `HeadObject` | **Pass** |
| Ranged `GetObject` + matching `If-Match` | **Pass** — `206`, correct `Content-Range: bytes 5-9/20`, correct 5-byte slice |
| Ranged `GetObject` + mismatched `If-Match` | **Pass** — correctly refused `412 PreconditionFailed`, not silently served |
| `ListObjectsV2` with continuation tokens | **Pass** — 25 objects, `MaxKeys=7`, 4 pages, continuation token correctly resumed each time, no duplicates/gaps |
| `DeleteObject` (single) | **Pass** |
| `DeleteObjects` (batch, up to 1000 keys; 24 tested) | **Pass** — all 24 deleted, zero per-key errors |
| `CreateMultipartUpload`/`UploadPart`×3/`CompleteMultipartUpload`, **16 MiB parts** (two 16 MiB + one 5 MiB trailing part, 38,797,312 bytes total) | **Pass** — composite ETag carries the `-3` suffix, `HeadObject` reports the exact byte count, full read-back matches the concatenated parts exactly |
| `AbortMultipartUpload` | **Pass** — confirmed no object exists after abort |
| S3A's rename path (`CopyObject` then `DeleteObject`) | **Pass** — source gone, destination holds the original bytes |

Every operation the task asked about passed, under the scoped key, including the one most
likely to reveal a scoping gap (ranged `GetObject` combined with a conditional header).

## 3. Checksums: header mode confirmed; trailer mode not reproduced from this client stack

`DeleteObjects` (batch delete) was tested under two `botocore` `request_checksum_calculation`
settings, `WHEN_SUPPORTED` and `WHEN_REQUIRED`. **Both passed** — but a direct inspection of
the actual wire headers (via a `botocore` event-hook spy, not guessed from the SDK's
documented *intent*) showed both settings produced the **same transmission mode**: a plain
`x-amz-checksum-crc32` request header plus `x-amz-sdk-checksum-algorithm: CRC32`, with an
ordinary (non-chunked) `x-amz-content-sha256` body hash — never an `aws-chunked`
Content-Encoding with a trailing checksum, for either setting, for this operation. This is
very likely because `DeleteObjects`' body (an XML list of keys) is always small and fully
buffered in memory before the request is sent — `botocore`'s chunked-trailer path is
triggered by streaming, unknown-length bodies, which this operation's body structurally never
is, regardless of the checksum-calculation setting. **I could not force genuine trailer-mode
transmission for this specific operation from this client stack, and did not have time to
stand up a real AWS SDK v2 (Java) harness to check whether *it* behaves differently for this
operation** (no Maven or cached dependency jars locally — see §2's note). What is confirmed:
SeaweedFS correctly accepts and processes a checksummed batch-delete request in the one
transmission mode this spike could actually produce.

## 4. Expiry design — and a split this spike surfaces that the prior one didn't

SeaweedFS turns out to offer **two structurally different credential shapes**, with opposite
expiry properties, and the one that matches Lakekeeper's own requirement (§5) is the one with
**no native expiry**:

- **IAM `CreateAccessKey`** (§1's path — a bare pair, no session token): the response carries
  no `Expiration` field at all, and there is no duration parameter on the call. This mirrors
  AWS IAM itself and Garage's own admin-API keys (`docs/design-homelab-warehouse-store.md`
  §6): **a minted key just exists until explicitly revoked.** A `booth-storage` provider for
  this path would need the same durable-state sweeper design already sketched for Garage:
  track every minted `{accessKeyId → iamUsername, workspace, backendId, prefix, expiresAt}`
  in its own store, and run a sweeper that calls `DeleteAccessKey` then `DeleteUser` once the
  broker's TTL elapses. A missed sweep leaves a credential live past its advertised expiry —
  the same real weakening of the "a credential that actually stops working at expiry"
  property MinIO's native service-account expiry gives for free, that the Garage sketch
  already flagged.
- **STS `GetFederationToken`** (§6 — now working): the response **does** carry a real
  `Expiration` timestamp honored precisely (requested `DurationSeconds=900`, got back exactly
  15 minutes later — consistent with the prior spike's finding). No sweeper needed for this
  path; the credential stops working on its own. But it issues a 3-tuple with a session
  token, which is the wrong shape for Lakekeeper specifically (§5) — it would be the right
  path for a different, session-token-tolerant consumer (`docs/decisions/0006`'s "general
  shape").

**Renewal**, for the path that actually matters here (bare pair): mirror MinIO's existing
`MintScopedCredential` pattern of minting a *fresh* credential rather than extending an old
one (`internal/backend/s3/credentials.go`) — on renewal, create a new IAM user (or a new
access key under a reused per-grant user; either works, a fresh user is simpler to sweep
atomically), issue its key, and let the sweeper clean up the previous one on its own schedule
rather than synchronously on renewal, so a renewal that races a revoke can't leave nothing
valid for a moment.

## 5. Checked against Lakekeeper's actual warehouse-credential contract

Verified against primary sources, not the prior notes' paraphrase, since the task asked to
re-verify rather than trust an earlier quote:

| Requirement | Source | Result against SeaweedFS |
|---|---|---|
| Static pair, no session token | `docs/decisions/0006` §1 ("Lakekeeper 0.13.6's static-key warehouse credential has no field for one"); `booth-lakehouse/docs/decisions/0001` line 108 ("Lakekeeper 0.13.6's `access-key` warehouse credential has **no session-token field**") | **Satisfied** — §1's `CreateAccessKey` path is exactly this shape, confirmed by direct API response, not inferred |
| Scope echoed equal to the request | `booth-lakehouse/docs/decisions/0001` line 105: "The client refuses any grant whose echoed scope differs from the request" | **Not SeaweedFS's responsibility — this module's.** Scope-echo is a property of `booth-storage`'s own credential-broker response shape (`internal/credentialbroker/provider.go`'s `scope: <echo>`), which the MinIO provider already satisfies today by construction (it echoes back whatever `MintRequest` it was given, regardless of backing store). A SeaweedFS provider would satisfy this the identical way — nothing about SeaweedFS's own API changes this answer |
| Grant must cover the warehouse root | docs.lakekeeper.io "storage-validation" page (fetched directly this pass): the `lakekeeper-read-write` check requires Lakekeeper itself to "write, read back and delete a probe file in the Warehouse's object storage" — i.e. **Lakekeeper's own standing credential**, not a per-table vended one, needs access at the warehouse's root, not a narrower per-table prefix | **High confidence, not separately re-tested with a dedicated whole-bucket-scoped user this pass.** §1 already proves prefix-scoped IAM policies work correctly; granting `arn:aws:s3:::bucket/*` + `arn:aws:s3:::bucket` (the warehouse root) instead of a narrower prefix is the identical mechanism with a wider ARN glob — nothing in §1's result suggests this wouldn't also work, but it wasn't independently exercised with its own test this pass |
| Lakekeeper's write/read/delete probe on every renewal | docs.lakekeeper.io "storage-validation" page: `lakekeeper-read-write` ("writes, reads back, and deletes a probe object"); the `validate-credential` endpoint explicitly covers "a replacement credential" (renewal); a `cleanup` check confirms "everything written during validation was removed again" | **Not something SeaweedFS needs to do anything special for** — it's an ordinary write+read+delete sequence against whatever credential Lakekeeper is given, which §1's and §2's results already show SeaweedFS answers correctly for a scoped bare-pair credential. The page does not state whether a *different set* of checks runs specifically on renewal vs. initial validation — this spike did not find that distinction documented anywhere, and did not attempt to force a renewal against a real Lakekeeper instance to observe directly |

**Correction to the prior spike's framing**: `docs/design-homelab-warehouse-store.md` focused
entirely on `GetFederationToken` as "the one thing that would actually unblock the broker."
For Lakekeeper's specific bare-pair need, that framing was backwards — `GetFederationToken`
structurally cannot produce the no-session-token shape Lakekeeper requires (an STS credential
is always a 3-tuple), so it was never the right mechanism for *this* consumer regardless of
whether its `AccessDenied` bug got fixed. The plain IAM path (§1) is the one that matters for
Lakekeeper, and it already works.

## 6. STS root cause — found, not just guessed this time

The prior spike's own words: "SeaweedFS's 'advanced IAM' mode may expect the base identity's
permissions to be expressed as an attached IAM policy document... rather than the legacy
`actions: [...]` list... flagged as a guess, not a finding." **This spike confirms the guess
was exactly right**, and the fix is concrete:

- Reproduced the bug first: `GetFederationToken` with the `admin` identity (created only via
  `weed shell`'s `s3.configure`, which sets the legacy `actions: ["Admin"]` list and nothing
  else) mechanically succeeds — correct `Expiration`, inline session policy round-trips in the
  session token's own JWT `spol` claim, `sts:GetCallerIdentity` resolves the federated
  principal correctly — and then every S3 operation with the resulting session credential is
  denied with a bare `AccessDenied`. Exactly the prior spike's result, reproduced independently.
- **The fix**: attach a real IAM policy document to the base identity via `PutUserPolicy`
  (the same call used in §1 for the scoped user) — `{"Effect":"Allow","Action":["s3:*"],
  "Resource":["*"]}` on `admin`. After that one call, with no other change, the identical
  `GetFederationToken` call started working: the session JWT gained a `pol`
  claim referencing the newly attached policy (`__inline_policy__/admin/FullAccess`), and the
  federated credential's `PutObject` succeeded.
- **Scoping through STS also verified correct**, once a policy-authoring mistake from this
  spike's own first attempt was found and fixed: mixing an `s3:prefix` `Condition` into a
  statement that also grants `GetObject`/`PutObject` denies *all* actions in that statement
  (the condition key isn't meaningful for those actions, so it evaluates false and the whole
  statement fails) — exactly the kind of real-AWS IAM policy-authoring trap this module should
  expect to hit again when it writes its own policy-generation code, the same category of
  lesson as `docs/decisions/0006` §3's MinIO `GetBucketLocation`-needs-its-own-statement
  finding. Split into two statements (object actions unconditioned, `ListBucket` conditioned),
  and the federated credential correctly got **allowed** inside the granted prefix and
  **refused** (`AccessDenied`) both outside the prefix and at the bucket root — real,
  server-enforced downscoping through a real STS session token.
- **Not independently re-verified**: actual expiry enforcement at the moment a token lapses
  (confirming the `Expiration` timestamp is honored, not just returned, would need waiting out
  a full 15-minute TTL, which this time-boxed spike didn't spend time on — the prior spike
  already established the timestamp itself is computed correctly from a requested duration).

## 7. Garage v2.4.1 re-test

Re-ran the exact same probe as the prior spike (two `PutObject` calls with `If-None-Match: *`
to the same key, header presence confirmed via `botocore`'s request serializer before sending,
not assumed from the SDK call alone) against `dxflrs/garage:v2.4.1` — the version specified as
actually deployed on the homelab. **Unchanged: fails.** Both writes returned `200 OK`; the
object's final content was the second write's body (`"second"`), with a different ETag each
time — Garage still silently overwrites rather than honoring the precondition, identically to
v1.0.1 in the prior spike. This is the same real, confirmed behavior, now checked against the
specific version in question rather than extrapolated from v1.0.1.

## What a SeaweedFS provider in `booth-storage` would need from core, Spark, and Lakehouse

Answered directly, not left as an open question, since this spike now has enough to say:

- **From `booth-storage` itself** (not asked here, but stated for completeness since it's the
  natural next step): a new provider in `internal/credentialbroker`/`internal/backend/s3`
  parallel to the MinIO one, implementing §1's IAM flow (`CreateUser`→`PutUserPolicy`→
  `CreateAccessKey`) for the bare-pair/no-session-token case Lakekeeper needs, plus the
  sweeper state §4 describes (this module doesn't have anything like it today — it's new,
  durable state, the same category of gap the Garage sketch already flagged, just now with a
  mechanism that's actually worth building it for, unlike Garage's). `GetFederationToken`
  (§6) is available as a second code path for a future session-token-tolerant consumer, but
  nothing in this module's current `MintRequest`/provider contract needs it yet.
- **From `booth-core`**: nothing new identified this pass. The broker's existing wire contract
  (confirmed already correct for this shape in `docs/design-native-s3-endpoint.md` §1) doesn't
  need to change — `options.sessionToken: "forbidden"` already exists and is exactly what the
  §1 path satisfies.
- **From `booth-spark`**: nothing new identified this pass beyond what the prior spike already
  confirmed (pinned `hadoop-aws:3.4.2` + AWS SDK v2 `bundle:2.29.52` works against SeaweedFS's
  S3 API with static credentials) — this spike used the scoped key at the raw SDK level, which
  exercises the identical wire operations S3A issues, but did not re-run the actual JVM client
  (§2). Worth an independent confirmation with the real S3A client against a *scoped* (not
  admin) key before relying on this in production, since a scoped key is the one thing this
  spike changed relative to the prior one's S3A test.
- **From `booth-lakehouse`/Lakekeeper**: nothing structurally new — §5 found every one of its
  stated contract requirements satisfied by SeaweedFS's bare-pair IAM path, with one gap
  (whole-warehouse-root scoping) inferred rather than directly re-tested. Re-confirm that one
  specifically before depending on it.

## What I could not verify

- Hadoop S3A against SeaweedFS **with the scoped (non-admin) key specifically** — only
  exercised at the raw SDK/REST level this pass, not through a real JVM client (no Maven or
  cached dependency jars available in this environment). The prior spike proved the JVM
  mechanism works with *admin* credentials; this spike proves the *scoped* credential's
  operations work at the wire level; the combination of the two (S3A + a freshly-scoped key)
  was not directly observed together.
- Genuine `aws-chunked`-trailer checksum transmission for `DeleteObjects` — `botocore` only
  ever produced header-mode checksums for this specific operation regardless of configuration
  (§3); whether AWS SDK v2 (Java, the SDK `booth-spark` actually runs) behaves differently for
  this operation was not checked.
- Whether Lakekeeper's storage-validation checks genuinely differ between initial validation
  and renewal, beyond the `validate-credential` endpoint's description covering "a replacement
  credential" (§5) — the documentation doesn't spell out a distinction, and this spike didn't
  run a real Lakekeeper instance to observe one directly.
- A dedicated whole-bucket (warehouse-root) scoped IAM user, as distinct from the prefix-scoped
  one actually tested (§5) — inferred to work from the identical mechanism, not independently
  run.
- Real STS token expiry enforcement at the moment of lapse (§6) — the `Expiration` timestamp's
  correctness was confirmed by the prior spike; actually waiting out a TTL to see the token
  stop working was not attempted in this time-boxed pass.
- Any behavior under real concurrent load, a multi-node SeaweedFS cluster, or failure
  injection — single-node, single-client spike by design, same as the prior one.
