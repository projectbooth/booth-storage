# A Garage provider for the credential broker, and a SeaweedFS spike (ARCHITECTURE item 67)

Status: proposal, 2026-10-15. No product code changed. Checked against this module's `main`
(`868ab72`), `internal/backend/s3`, `internal/registry/probe.go`, `internal/credentialbroker`,
`booth-architecture`'s ARCHITECTURE item 67 and `agent-briefs/storage.md`'s 2026-10-15 entries, and
`booth-spark`'s `images/spark-runtime/Dockerfile` and `internal/runs/objects_data.go` (its real
Hadoop/AWS SDK versions and S3A wiring). Spike containers run locally in Docker and torn down;
nothing here touches the homelab.

## 1. Why this exists

MinIO is unmaintained (item 67): its community repo is archived, its last release is an unpatched
CVE, and its images are gone from Docker Hub and quay.io. This module's only S3-compatible
credential-broker provider (`internal/backend/s3/credentials.go`'s `MintScopedCredential`) mints
scoped, expiring credentials by calling MinIO's **admin API** (`madmin.AddServiceAccount`) — a
mechanism specific to MinIO, not part of the S3 data-plane API any S3-compatible store implements.
So today, no S3-compatible store the broker can actually vend credentials for exists in this fleet:
Garage and SeaweedFS (the two maintained alternatives, item 67) both lack a MinIO-compatible admin
API. `booth-spark` and `booth-lakehouse` take every object-store key from the broker — vended
credentials are off — so neither can use a Garage or SeaweedFS warehouse until this module has a
provider for one of them.

Item 67(b) also asked for a fleet audit of every MinIO reference in this repo, so an operator can
see the real blast radius of eventually dropping it; that's §5 below.

## 2. Spike: the create-only probe against both candidate stores

`registry.probeCreateOnly` (`internal/registry/probe.go:27-46`) is the exact test run: a
create-only write (`If-None-Match: *`) to a fresh key must succeed; the identical write again must
answer `ErrPreconditionFailed` (412). I ran this literally — the same two-write sequence, the same
header — against a local, disposable container of each store, using `aws s3api put-object
--if-none-match '*'` (confirmed via `--debug` that the signed request actually carried
`If-None-Match: *`, not just that the CLI flag parsed).

- **Garage v1.0.1** (`dxflrs/garage`, single-node, SQLite metadata, replication factor 1): **fails**.
  Both writes returned `200 OK` with the same ETag (`d41d8cd98f00b204e9800998ecf8427e`, an empty
  object both times) — Garage silently ignores `If-None-Match` and overwrites. This is confirmed,
  not inferred: the header was present and correctly signed in the request Garage received. Until
  Garage fixes this (or a different release behaves differently — only v1.0.1 was tested), a Garage
  backend registered in this module answers `501` for create-only writes forever, per the existing
  design (`docs/design-app-writes.md` §1) — ordinary writes are unaffected.
- **SeaweedFS 30GB 4.48** (`chrislusf/seaweedfs`, single-process `weed server -s3 -filer`): **passes**.
  First write succeeds; the second answers `412 PreconditionFailed` (`"At least one of the
  pre-conditions you specified did not hold"`) — a real, server-enforced precondition, not a client
  illusion.

## 3. Spike: SeaweedFS's STS (`GetFederationToken`) — passes the parts I could test, fails the one that matters

Started SeaweedFS with its embedded "advanced IAM" integration (`-s3.iam.config`, a signing key
required for the STS service — `"sts": {"signingKey": "<32 random bytes>"}`; the flag's error
message was explicit about this, nothing guessed). Confirmed:

- **`GetFederationToken` works mechanically**, signed with the long-term admin identity's own keys,
  exactly as `agent-briefs/storage.md`'s desk research predicted: `DurationSeconds` is honored
  precisely (requested 900s, `Expiration` came back exactly 15 minutes after issuance), and an
  inline session policy is accepted and round-trips — it's base64/JSON-embedded in the returned
  session token's own JWT claims (`"spol"` claim), not discarded.
- **`sts:GetCallerIdentity` works** against the resulting session credential — it resolves to the
  federated-user principal correctly, so the credential itself is a real, usable AWS SigV4 identity
  as far as STS is concerned.
- **But every S3 data operation with that session credential was denied** — `PutObject`,
  `ListObjectsV2`, and `HeadObject` all answered a bare `AccessDenied`, with no further detail in
  either the client response or the server's own log output at default verbosity. This held
  regardless of:
  - the inline session policy's scope — a prefix-scoped policy (`s3:GetObject`/`s3:PutObject` on
    `arn:aws:s3:::nfl-spike/incoming/*`) and a maximally permissive one (`s3:*` on `*`) were both
    denied identically;
  - the underlying identity's action list in `s3.json` — `["Admin","Read","Write","List",
    "Tagging"]` and a bare `["Admin"]` were both denied identically;
  - a full container restart between attempts (ruling out a stale-process artifact).
  - I confirmed this at the raw `aws s3api` level and again through a real Hadoop S3A client (§4) —
    the same denial, through two independent HTTP clients.

I could not resolve this within the spike's time budget. My best guess, **flagged as a guess, not a
finding**: SeaweedFS's "advanced IAM" mode may expect the base identity's permissions to be
expressed as an attached IAM policy document (ARN-referenced) rather than the legacy
`actions: [...]` list this module's own `s3.json`-style config uses, and I found no documented way
to express that without more time than a spike warrants, with no access to SeaweedFS's own docs
site to check. **This is the one thing in this note I could not get working — not inferred to work,
not assumed broken, just unresolved.** It's the one capability that actually matters for the
broker (a real, usable scoped-and-expiring S3 credential), so it's the one that would need to be
proven before any provider design banks on it.

## 4. Spike: multipart and Hadoop S3A

- **Multipart, SeaweedFS, static admin credentials**: `aws s3 cp` of a 20,000,000-byte file
  completed and the resulting `ETag` carries the `-3` multipart suffix (three parts); read-back
  `HeadObject` reports the exact original size. Works.
- **Hadoop S3A, static admin credentials**: built a minimal Maven project pinned to the exact
  versions `booth-spark`'s runtime image bundles
  (`images/spark-runtime/Dockerfile:21-25`: `hadoop-aws:3.4.2`, AWS SDK v2
  `bundle:2.29.52` — not v1's `aws-java-sdk-bundle`, which `objects_data.go`'s
  `spark.hadoop.fs.s3.impl = org.apache.hadoop.fs.s3a.S3AFileSystem` config confirms is the SDK
  generation actually in use). Configured `fs.s3a.endpoint`, `fs.s3a.path.style.access=true`,
  `fs.s3a.endpoint.region`, and `org.apache.hadoop.fs.s3a.SimpleAWSCredentialsProvider` with the
  static admin key pair against the same SeaweedFS container. A real `FileSystem.create`/
  `getFileStatus`/`open` round-trip through `s3a://nfl-spike/...` succeeded end to end — the
  fundamental S3A-against-SeaweedFS mechanism (endpoint override, path-style addressing, the SDK
  v2 generation `booth-spark` actually runs) works.
- **Hadoop S3A with a session token**: same setup, `org.apache.hadoop.fs.s3a.TemporaryAWSCredentialsProvider`
  (note: not under `.auth.` — I initially guessed wrong and corrected it by inspecting the real
  class list inside `hadoop-aws-3.4.2.jar`, not from memory) with the SeaweedFS-issued
  `AccessKeyId`/`SecretAccessKey`/`SessionToken`. S3A built the client and sent a real signed
  request carrying the session token correctly — and got the identical `403 AccessDenied` as the
  raw `aws s3api` test in §3. This is the same unresolved STS-authorization gap, now also confirmed
  through the actual client stack `booth-spark` runs, not just the AWS CLI.

## 5. Fleet audit: every MinIO reference in this repo (item 67(b))

**SDK dependency** (the data-plane client; needed by any S3-compatible backend, MinIO or not):
- `go.mod:15` (`github.com/minio/madmin-go/v3`), `go.mod:16` (`github.com/minio/minio-go/v7`),
  `go.mod:84-85` (`minio/crc64nvme`, `minio/md5-simd`, indirect) — and `go.sum`'s matching entries.
- `internal/backend/s3/s3.go:16-17` (imports), and every `minio.*` call site in that file: client
  construction (`s3.go:83-84,104,108,117`), reads (`s3.go:173`), writes (`s3.go:193,234`), copy
  (`s3.go:288-289`), delete (`s3.go:294`), stat (`s3.go:298`), and error-code mapping
  (`s3.go:311,320`). This is the S3 kind's one and only HTTP client library — replacing it (not in
  scope here) would mean re-implementing every one of these call sites against a different SDK or
  raw SigV4, not a config change.

**Admin-API dependency** (MinIO-specific; the actual blocker in this note):
- `internal/backend/s3/credentials.go:11` (`github.com/minio/madmin-go/v3` import), `:123`
  (`madmin.New`, opens an admin client), `:138` (`madmin.AddServiceAccountReq`, the one call this
  entire mechanism depends on). Doc comments at `:29,88-89,154,161,164-165` name MinIO explicitly as
  the reason for the 15-minute TTL floor and the two policy-statement quirks (verified against a
  real server per those comments' own citations, not assumed).

**Test-only** (CI fixtures and local dev emulators; never shipped):
- `hack/docker-compose.emulators.yml:12-19` — the `minio` service, pinned to
  `ghcr.io/projectbooth/minio-test@sha256:04c918e8...` (a source-built mirror per ADR 0091, "for
  tests only and must not be used in production" per ARCHITECTURE item 67's own wording).
- `.github/workflows/ci.yml:4,49` — comments/step name referencing "real MinIO" for the backend
  test job.
- `internal/registry/service_test.go`, `internal/registry/probe_test.go`,
  `internal/api/server_test.go`, `internal/backend/s3/s3_test.go`,
  `internal/backend/s3/credentials_test.go`, `internal/credentialbroker/provider_test.go` — real
  tests gated on a live MinIO instance (`BOOTH_TEST_REQUIRE_EMULATORS=1` / `BOOTH_TEST_*_ENDPOINT`,
  the same pattern used throughout this module's test suite), exercising the backend, the
  create-only probe, and the credential broker's real `madmin.AddServiceAccount` path.
- `web/src/__tests__/AdminView.test.tsx`, `web/src/__tests__/testUtils.ts` — "minio" appears only as
  a sample backend kind/displayName string in UI test fixtures, not a real dependency.

**Docs-only / naming MinIO as an example, not a dependency on it specifically:**
- `README.md:28,44,78,113,151,163` — describes `madmin-go`'s purpose, the local dev compose stack,
  and decision 0006's scope note ("only self-hosted MinIO-compatible backends can actually issue
  one today").
- `docs/design-app-writes.md` and `docs/decisions/0006-s3-credential-broker-provider-design.md` —
  design rationale, already superseded in spirit by this note where it discusses the broker's
  MinIO dependency.
- `internal/backend/backend.go:25` — a one-line doc comment listing MinIO as an example
  S3-compatible store (`KindS3`'s doc, not a dependency).
- `charts/booth-storage/values.yaml:70` — a comment describing what "self-hosted,
  MinIO-compatible" means for the `endpoint` field; no MinIO-specific default or image reference in
  the chart itself.
- `web/src/kinds.ts:36,47` — UI copy ("AWS S3, MinIO, Ceph...", "Needed by MinIO and most
  self-hosted stores") for the `s3` kind's registration form; cosmetic, not a dependency.
- `test/integration/README.md:26` — describes the per-push test layer's real backends.

**Summary**: one real *admin-API* dependency (`credentials.go`, the broker provider — the actual
blocker), one real *SDK* dependency (`s3.go`, needed regardless of which S3-compatible store an
operator points this module at — not MinIO-specific in itself), and everything else is either a
test fixture pinned to a source-built, tests-only mirror, or prose that names MinIO as an example
of "an S3-compatible store," not a hidden dependency on it.

## 6. A Garage provider: not recommended now, sketched for completeness

Since Garage fails the create-only probe outright (§2), a credential-broker provider for it would
only ever serve ordinary (non-create-only) reads/writes — and ADR 0117's whole point is that
create-only matters for exactly the workloads (apps writing once, pipeline tasks landing files)
this kind of workspace would use. I'd still sketch the shape, since the coordinator's note raised it
as option 1:

- **Mapping**: Garage's admin API (distinct from its S3 API, confirmed running on its own port in
  the spike — `admin.api_bind_addr`) has `bucket create`/`key create`/`bucket allow --read --write
  <bucket> --key <key>` calls (used directly in the spike to set up the test bucket/key), but **no
  per-prefix scoping** — permissions are granted at the whole-bucket level, not a sub-prefix. A
  provider could only satisfy a `MintRequest` whose `Path` is empty (the whole bucket), refusing
  (`ErrScopeNotSupported`) any narrower request — the opposite of what ADR 0080 wants ("refuse
  rather than widen" implies the common case, a sub-prefix grant, would always be refused here,
  not just an edge case).
- **Expiry**: Garage's admin API (as exercised in the spike) has no visible TTL/expiration field on
  a key — a minted key just exists until explicitly revoked. A provider would need to track its own
  minted keys and run a sweeper to call `key delete` after the broker's TTL elapses, which is new,
  durable state this module doesn't have anywhere else (every other kind's credential-broker
  behavior is stateless, deriving directly from the backend's own expiry mechanism).
- **Cleanup**: explicit `key delete` required (unlike MinIO's own expiring service accounts, which
  lapse on their own) — another reason this needs a sweeper, not just a mint call.
- **What it refuses**: anything narrower than a whole bucket; anything wanting `AllowSessionToken:
  false` with an actual expiry (same structural tension as real AWS S3 in the existing design,
  `credentials.go:91-97`, since Garage's key pairs are also bare, non-expiring 2-tuples absent a
  sweeper).
- **Assumptions this leans on, unverified**: that Garage's admin API's bucket-level
  `read`/`write` flags map cleanly onto this module's `AccessRead`/`AccessReadWrite`; that a
  sweeper polling on this module's own schedule is an acceptable new failure mode (a missed sweep
  leaves a credential live past its advertised expiry — a real weakening of the broker's "a real
  bare key pair that actually stops working at its expiry" property quoted from `credentials.go:91`
  for MinIO).

**I would not build this.** A provider that can only ever grant whole-bucket access, needs its own
durable sweeper state, and still fails the create-only probe doesn't clear the bar the existing
MinIO provider sets, and ADR 0080 explicitly wants "refuse rather than widen," not "refuse almost
always." It's recorded here because it was asked for, not because I'm recommending it.

## 7. Where this leaves the decision

- **Garage**: registerable and usable today through this module's ordinary API (any editor/owner
  or platform-access pipeline task) for everything except create-only writes, which will permanently
  `501` on it (§2). No credential-broker provider is worth building for it (§6) — Spark/Lakehouse
  access is out of reach regardless.
- **SeaweedFS**: passes create-only (§2) and multipart (§4), and its S3A-over-path-style mechanism
  works with `booth-spark`'s exact runtime versions (§4) — real positives, not assumptions. But its
  STS path, the one thing that would actually unblock the broker, denied every S3 operation in my
  spike regardless of policy or base-identity shape, and I don't have a resolution — only a flagged
  guess (§3). This is the one open technical question that actually decides whether SeaweedFS is
  viable here, and it isn't answered yet.
- Neither store is ready to recommend as the `s3`-kind credential-broker's next backing store right
  now: Garage structurally can't satisfy prefix scoping or create-only; SeaweedFS might, but its STS
  authorization model needs to actually work first, and it didn't in this spike.

## Questions for other repos

- **`booth-spark`**: none found needing a version bump — `hadoop-aws:3.4.2` + AWS SDK v2
  `bundle:2.29.52` (its own pinned versions) worked against SeaweedFS's S3 API with static
  credentials in this spike (§4). If SeaweedFS's STS gap (§3) gets resolved, worth independently
  confirming `SidecarCredentialsProvider.java`'s own file-based credential re-read (distinct from
  this spike's direct env-var-based `TemporaryAWSCredentialsProvider` test) handles a session token
  field correctly — it wasn't exercised here.
- **`booth-lakehouse`**: if SeaweedFS ever becomes the warehouse store, confirm whether Lakekeeper's
  own warehouse-credential shape (no session-token field, per the existing `credentials.go:58-62`
  constraint carried over from the MinIO design) is compatible with whatever SeaweedFS's STS
  actually ends up able to issue once §3 is resolved — this note doesn't re-litigate that constraint,
  just flags that it still applies to any new provider.
- **`booth-core`**: none — the broker's request/response contract (`contracts/credential-broker.md`)
  needs no change for either candidate; this is entirely a provider-side (this module's) question.

## What I could not test

- SeaweedFS's STS authorization gap (§3) — root cause unresolved, flagged as a guess, not fixed or
  confirmed broken at a specific layer.
- SeaweedFS's advertised cluster-wide atomicity for `If-None-Match` under real multi-node
  replication (replication factor 1, single node, in this spike — the ADR 0117 concurrent-same-key
  race test from `docs/design-app-writes.md` §7 was not re-run here, since the single node makes
  that race trivially safe and wouldn't prove anything about a real multi-node deployment).
- SeaweedFS's `AssumeRole`/`AssumeRoleWithWebIdentity`/`AssumeRoleWithLDAPIdentity` actions (only
  `GetFederationToken` was tried, since it needs no separate role/trust-policy configuration to
  attempt) — one of these might behave differently than `GetFederationToken` and resolve §3's gap;
  not ruled out, just not tried.
- Any behavior under real concurrent load, failure injection, or a multi-node Garage/SeaweedFS
  cluster — this was a single-node, single-client spike by design (ARCHITECTURE item 67(b)'s own
  framing: "no cluster").
- Real AWS S3 as a third candidate — out of scope for this note, which was asked to compare Garage
  and SeaweedFS specifically.
