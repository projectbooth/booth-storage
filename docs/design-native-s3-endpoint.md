# Feasibility: booth-storage serving its own S3-compatible endpoint

Status: feasibility note, 2026-10-16. No code changed, no containers run. Checked against this
module's `main` (`868ab72`), `internal/backend/backend.go`, `internal/backend/filesystem`,
`internal/backend/s3`, `internal/credentialbroker`, `docs/decisions/0006-s3-credential-broker-
provider-design.md`, and the prior spike's findings
(`docs/design-homelab-warehouse-store.md`, branch `design-homelab-warehouse-store`, commit
`d24bd51`): Garage can't do create-only at all; SeaweedFS's `GetFederationToken` session
credentials got unexplained `AccessDenied` on every operation. This note asks a different
question: instead of depending on a third party's STS, could this module issue its own
credentials and enforce them itself, as the S3-protocol server in front of its own backends?

## 1. The key insight — checked against the broker's real contract, not assumed

**It holds.** The broker's wire contract already has a documented, implemented path for a bare,
non-expiring-at-AWS-level, two-part access-key/secret-key pair with **no session token at all**:
`s3Options.SessionToken` can be `"forbidden"` (`internal/credentialbroker/provider.go:136-142`),
which the handler turns into `MintRequest.AllowSessionToken = false`
(`provider.go:244`/`credentials.go:58-62`). `s3CredentialBody.SessionToken` is
`omitempty` (`provider.go:155`) — the field is designed to be *absent*, not empty, for exactly
this shape. This isn't new plumbing to add; it's the shape **Lakekeeper's own warehouse
credential already requires** (`docs/decisions/0006` §1-2: "Lakekeeper 0.13.6's static-key
warehouse credential has no field for one"). Today only MinIO's `AddServiceAccount` can satisfy
it, because a MinIO expiring service account happens to be a genuine bare pair
(`credentials.go:85-97`); real AWS S3 structurally can't (STS always returns a 3-tuple), and
that's exactly the gap the prior spike confirmed neither Garage nor SeaweedFS fills either.

If booth-storage itself verifies the SigV4 signature on every S3-protocol request, it doesn't
need AWS STS, MinIO's admin API, or SeaweedFS's IAM service at all — it already *is* the thing
deciding whether an access-key-id is still valid, so "does this key still work" is a lookup in
this module's own store, not a property some other server's expiry mechanism has to honor. The
reasoning holds; what it actually requires is the rest of this note.

## 2. What SigV4 verification and self-minted credentials would require

**Verification mechanics.** SigV4 is a defined algorithm, not a proprietary one: canonicalize
the request (method, path, sorted-and-normalized query string, a fixed set of signed headers,
a hash of the body), build a string-to-sign from that canonical request plus a credential scope
(`date/region/service/aws4_request`), derive a signing key by chaining HMAC-SHA256 from the
secret key through date→region→service→`aws4_request`, HMAC the string-to-sign with it, and
compare to the `Authorization` header's own hex signature. This module would need: (a) a lookup
from the access-key-id in the `Authorization` header's `Credential=` field to a stored secret
key, scope, and expiry; (b) the canonicalization logic itself; (c) handling for the
request-body-hash field, which is `UNSIGNED-PAYLOAD` for some clients and a real SHA-256 for
others. Three edge cases matter specifically for the callers in play here, and I could not
verify any of them against hadoop-aws's or Iceberg's actual source in this pass (see §7):

- **Chunked/streaming uploads** (`Content-Encoding: aws-chunked`, `x-amz-content-sha256:
  STREAMING-AWS4-HMAC-SHA256-PAYLOAD`): some S3 clients sign large PUTs as a sequence of signed
  chunks rather than one whole-body hash, each chunk carrying its own trailing signature. If
  Hadoop S3A's AWS SDK v2 client uses this for large uploads (plausible but **not confirmed
  here**), the verifier needs to handle per-chunk signatures, not just one whole-request
  signature — materially more code than single-shot SigV4.
- **Presigned URLs** (`X-Amz-Signature` as a query parameter instead of an `Authorization`
  header): a different code path through the same algorithm. Whether Hadoop S3A, Iceberg's
  `S3FileIO`, or Lakekeeper's "remote signing" mode (found in §3 below) ever produce or expect
  presigned URLs is **unconfirmed** — flagged, not assumed either way.
- **Multipart part-signing**: each `UploadPart` call is an independently signed request against
  the same access key; this falls out of ordinary per-request verification for free as long as
  the credential/expiry lookup is cheap enough to do on every part, which it should be (an
  in-memory or indexed-DB lookup, not a network call).

**Workspace scoping is a real, concrete design requirement, not a detail.** A raw S3A or boto3
request carries no `X-Booth-Workspace` header and no bearer token — only an access-key-id. So
the access-key-id itself becomes the only handle this module has on who's asking: minting a
credential means generating a fresh access-key-id and recording `{accessKeyId → secretKey,
workspace, backendId, prefix, access, expiresAt}` in a new store, the same shape
`internal/uploadgrant` already uses for Streamlit's grants, but keyed by a credential identifier
instead of a token subject. **This is new durable state** — nothing in this module today maps an
opaque key-id back to a workspace; `auth.Middleware`'s whole model is "the token carries the
workspace," which a SigV4 request structurally cannot do.

## 3. The S3 operation surface actually needed

I could not find an authoritative, current list of exactly which calls `S3AFileSystem` issues,
and general web search did not turn up Hadoop's own S3A source or its current documentation
directly (see §7) — the operations below are the well-documented minimum any S3-compatible
object store needs for this class of client, not a verified S3A-specific trace:

| Operation | Needed by | Confidence |
|---|---|---|
| `GetObject` (with `Range`) | S3A reads; Parquet/ORC columnar pushdown reads small ranges repeatedly | High — this is the entire point of range-addressable formats, confirmed by general knowledge, not this session's search |
| `HeadObject` | S3A `getFileStatus`; existence/size checks | High |
| `PutObject` | S3A writes (single-shot, under S3A's multipart threshold) | High |
| `ListObjectsV2` | S3A directory listing; **S3A has defaulted to the v2 list API since Hadoop 2.8 per a 2017 Hadoop JIRA ticket, and that same ticket already warned some non-AWS S3 implementations don't support v2** — worth re-verifying for whatever store or custom server ends up on the other end | Medium — ticket is from 2017; not re-checked against 3.4.2's current source |
| `DeleteObject` / bulk `DeleteObjects` | S3A delete, `rm -r` | High |
| `CopyObject` | S3A rename (object stores have no native rename) | High |
| `CreateMultipartUpload` / `UploadPart` / `CompleteMultipartUpload` / `AbortMultipartUpload` / `ListParts` | S3A large writes above its multipart threshold | High (standard flow; not S3A-specific-verified) |
| Conditional `PutObject` (`If-None-Match`) | **Iceberg's own atomic-commit primitive**, confirmed by this session's search: Iceberg (and Delta Lake) use S3's conditional-write support so only one of two competing commit writers succeeds, the other retries cleanly | Medium-high — multiple independent sources describe this as *the* mechanism, but none I found names `S3FileIO`'s code path specifically |
| *(none found)* | **Lakekeeper itself appears not to perform data-plane S3 operations at all** — every source found describes it vending (or remote-signing) credentials that the query engine then uses to talk to S3 directly, bypassing the catalog server for data I/O | Medium — consistent across several independent descriptions of the vended-credential pattern, but I found no explicit Lakekeeper-source confirmation that it never touches S3 data itself (e.g., for purge-on-drop) |

**Consolidated minimum surface**: Get/Head/Put/conditional-Put/Delete/bulk-Delete/Copy/List(v2)
objects, plus the five-call multipart set. This matches almost exactly what
`internal/backend/s3/s3.go` already does as an S3 *client* — the gap is building the S3
*server* side (§5) that answers these calls by calling into `backend.Backend`, not finding new
operations this module doesn't already understand conceptually.

## 4. `backend.Backend`'s real gap: no multipart, no Range, confirmed from the interface itself

`internal/backend/backend.go:129-170}`'s `Backend` interface has `Read(ctx, path) (io.ReadCloser,
ObjectInfo, error)` — **no byte-range parameter at all** — and a single-shot `Write(ctx, path, r
io.Reader, opts) (ObjectInfo, error)` — **no multipart concept**. Confirmed further at the HTTP
layer: `handleReadObject` (`internal/api/server.go:279-314`) always streams from byte 0 via
`io.Copy`, with no `Range` header handling anywhere in that function — there is no partial-read
path in this module today, for any backend kind, for any caller. An S3-protocol front end would
need to add both:

- **Range reads**: translate an S3 `GetObject` `Range` header into a range read against each
  backend kind (`os.File.Seek`+limited `io.Copy` for filesystem; the S3/Azure/GCS SDKs each have
  their own native range-GET option already, so those three kinds are a smaller lift than
  filesystem, somewhat ironically, since "filesystem first" per the task is the one kind needing
  genuinely new code here).
- **Multipart upload state**: `CreateMultipartUpload` needs an upload-id and a place to track
  parts until `CompleteMultipartUpload` assembles them (or `AbortMultipartUpload` discards them).
  Nothing like this exists in `backend.Backend` or any kind's implementation today — this is
  wholly new, stateful machinery, not a reuse of the existing create-only buffering path (that
  path already buffers one whole body in memory per write, §6 below; multipart's whole purpose
  is to *avoid* that for large objects, so building it by just wrapping the existing single-shot
  `Write` would defeat the point).

## 5. Embed an S3-over-POSIX gateway, or build the subset — researched candidates

The core trade-off, stated plainly: **embedding removes the SigV4-and-operations implementation
work, but does not remove the broker-integration work.** Every candidate below authenticates
against a static user list or a config file reloaded from disk — none take a "mint me a
brand-new scoped credential right now, programmatically, for a request that just arrived" call
the way this module's own credential-broker provider needs to. Making any of them work still
means: this module writes the new credential to wherever that gateway reads its credential
store from (a file, a small database), and waits for or forces a reload — a different, but not
smaller, integration surface than writing SigV4-verification code directly. None of this was
independently re-verified by running the candidates (no spike containers this time, per the
task); the facts below are from research this pass, each flagged for how firm it is.

| Candidate | License | Maintenance (as researched) | Conditional `PutObject` | Dynamic credential issuance |
|---|---|---|---|---|
| **SeaweedFS's own S3 gateway** | Apache-2.0 (confirmed by several third-party sources, not the LICENSE file itself) | Active (already the basis of the prior spike) | **Confirmed passing** in the prior spike (§2 of that note) | Pluggable credential *store* backends exist (filer/SQLite/Postgres/in-memory, per `weed/credential`'s README) for the *static* identity list; this is a different thing from the STS mint-on-demand path that failed in the spike — **unconfirmed** whether writing straight into one of these stores sidesteps the STS `AccessDenied` gap, since the gap was in authorization, not in where identities are stored |
| **`rclone serve s3`** | Apache-2.0 (rclone itself) | Active | **Unconfirmed** — a late-2025 rclone forum thread reports `rclone copy --header-upload 'If-None-Match: *'` erroring on the *client* side; nothing found about the `serve s3` *server* side honoring the header from an incoming client | No native concept of issuing scoped credentials at all — it's a single-user or fixed-user-list gateway over a remote; would need its own credential-mapping layer regardless |
| **`andrewgaul/s3proxy`** | Apache-2.0 (confirmed, Maven Central metadata) | **Unconfirmed for 2026** — a Docker Hub mirror showed a 2-day-old image at research time, but that doesn't establish the upstream project's own cadence; commercial support is separately offered by its author | A release note documents "Emulate conditional PutObject" was added at some point (issues #732/#831); the current README's limitations list no longer excludes it, suggesting it's supported in a recent release — **not independently confirmed against a running instance** | Config-file-based static credential list, same integration gap as the others |
| **Zenko CloudServer (Scality)** | Apache-2.0 | A third-party repo-health aggregator reported commits as recent as August 2026 — **not a Scality statement**, treat as unconfirmed | Not researched this pass | Not researched this pass |
| **Versity `versitygw`** | Apache-2.0 (per a secondary source describing its release as "open-sources S3 gateway with no poison pill," explicitly contrasted with MinIO's AGPLv3) | Not independently confirmed this pass | Not researched this pass | Not researched this pass |
| **MinIO** | **AGPLv3** — confirmed: MinIO's own messaging requires anything that "distributes, hosts, or creates derivative works... over the network" to release the combined work's complete source. Embedding or forking it is the licensing risk that's already the whole reason item 67 exists | Its own gateway/NAS-mode (the one feature that would have let it front an arbitrary POSIX directory) was **removed** in `RELEASE.2022-10-29T06-21-33Z` — confirmed via MinIO's own blog post title "Deprecation of the MinIO gateway" | N/A — not a candidate here at all, listed only to close the loop: the one thing that would have made MinIO fit this task (gateway mode) no longer exists in any current release | N/A |

**Conclusion of this section**: no researched candidate is a clean drop-in. SeaweedFS is the
closest (already proven to pass create-only and multipart in the prior spike, Apache-licensed,
active), but adopting it here would mean reusing its S3 *data-plane* server while writing
directly into its credential store on every mint — a real integration, not configuration — and
it still carries forward the exact "is this really a different product than the thing whose STS
just failed" question the prior spike left open. **Building the subset avoids depending on any
of these being reliable for this specific need**, at the cost of writing the operation set and
SigV4 verification this module doesn't have today — itemized next.

## 6. Effort estimate for building the subset

Qualitative only — there is no prior sizing to calibrate against in this codebase, and I will
not fabricate week-counts with false precision.

| Piece | Size | Reuse vs. new |
|---|---|---|
| SigV4 verification middleware (canonical request, signing-key derivation, comparison; chunked-upload and presigned-URL handling if needed per §2) | **Large** — the single biggest net-new component, and the one with the most unverified edge cases (§2, §7) | Entirely new |
| Access-key-id → `{workspace, backendId, prefix, access, secretKey, expiresAt}` store and its minting/lookup/expiry-check path | **Medium** | New state, but a close structural cousin of `internal/uploadgrant`'s existing store/validation pattern — same shape of problem, different key |
| Per-kind operation handlers translating S3 verbs to `backend.Backend` calls (Get/Head/Put/Delete/bulk-Delete/Copy/List) | **Medium** | Mostly reuse — `backend.Backend` already has List/Read/Write/DeleteObject/DeleteFolder/MoveObject; this is a translation layer, not new backend logic |
| Range-read support added to every backend kind | **Medium**, filesystem specifically needs new code (`Seek`+limited copy); S3/Azure/GCS SDKs already support range GETs natively, so those three are smaller | Partially new (filesystem), partially reuse (object-store kinds already have the SDK primitive, just not plumbed through `Backend`'s interface) |
| Multipart upload state tracking (create/part/complete/abort/list-parts) | **Large** — genuinely new, stateful, and interacts with the create-only/atomicity guarantees this module already cares about (ADR 0117): a completed multipart upload needs the same "never leave a partial object visible" guarantee `Write`'s doc comment already promises | Entirely new |
| Conditional-`PutObject` wiring into the new S3-protocol server | **Small** | Full reuse — every kind already implements `WriteOptions.CreateOnly` atomically (this session's own earlier work); the new server just needs to map `If-None-Match: *` to it, exactly as the existing ordinary HTTP API already does at `internal/api/server.go`'s `handleWriteObject` |
| A new ingress path, reachable by Spark/Iceberg clients without going through core's gateway or `auth.Middleware` (SigV4 traffic carries no bearer token or workspace header for the gateway to check) | **Medium**, and **there is already a precedent for exactly this shape in this repo**: the credential-broker provider route (`POST /internal/credentials`, `internal/credentialbroker`) is already mounted on this module's own router but deliberately bypasses `auth.Middleware`, authenticated instead by its own shared-secret check (`provider.go:275-284`) — the same "a different path, a different auth mechanism, still this module's own server" pattern would extend naturally to an S3-protocol path | Pattern reuse, implementation new |

Net: this is not a small feature. The SigV4 layer and multipart state are both substantial,
genuinely new subsystems; everything else leans on code this module already has.

## 7. Throughput risks

- **Range GETs are the real question mark, not a minor gap.** Confirmed from `backend.go` and
  `server.go` (§4): there is no partial-read path anywhere in this module today. Spark's columnar
  predicate/projection pushdown on Parquet/ORC depends on many small `Range` GETs per query
  file — if every one of those still has to go through this module's own Go process (true for
  *any* design here, embedded gateway or built-from-scratch; the only question is whether that
  process's own range-handling is efficient), the real question is whether `os.File`'s
  `io.CopyN`/`io.Copy` path after a `Seek` is fast enough under the concurrency a real Spark job
  generates, or whether this module's own process becomes the bottleneck ahead of actual
  disk/network I/O. **This needs a real load test once built — it is not answerable from reading
  the code alone**, and nothing in this pass attempted one (no spike containers this time, per
  the task).
- **The create-only path's full-body buffering is a known, existing pattern that must not leak
  into ordinary large multipart writes.** This session's earlier S3-backend work already buffers
  the whole body via `io.ReadAll` for a create-only write, because a single conditional request
  needs the whole body available in one shot. That's fine for create-only (bounded by
  `MaxUploadBytes`/grant caps already). It would be a real problem if an S3-protocol front end's
  *ordinary* (non-create-only) multipart write path reused that pattern for a multi-gigabyte
  Spark output file — each part must stream, not buffer whole-object, or a handful of concurrent
  large writes could pressure this module's memory badly. This is a design constraint to hold,
  not yet a measured risk.
- **Concurrency**: many Spark executors opening connections simultaneously against this module's
  own HTTP server is an open question I did not attempt to answer — this module's current server
  (`net/http` via chi, nothing exotic) has no documented connection-pool sizing concern today
  because its current callers (the browser UI, a handful of app/pipeline writes) are nowhere near
  this profile. Whether it holds up under real Spark-executor fan-out is untested.

## 8. What `booth-core`, `booth-spark`, and `booth-lakehouse` would need to change

- **`booth-core`**: a raw S3A/boto3 client sends an AWS SigV4 `Authorization` header or query
  parameters — not this platform's bearer token, and no `X-Booth-Workspace` header. This traffic
  almost certainly cannot go through core's existing gateway unmodified, since the gateway's job
  is bearer-token verification and workspace-header injection, neither of which a SigV4 client
  does. **Open question for core, not resolved here**: either this module gets a second,
  directly-reachable ingress path (the credential-broker provider route is the existing
  precedent for "this module's own router, a different auth mechanism, never through the
  gateway" — §6), or core needs its own change to recognize and pass through SigV4-signed
  traffic unmolested. I have a structural reason to lean toward the former (it needs no core
  change at all, and this module already has the pattern), but this is core's call, not mine.
- **`booth-spark`**: pointing `fs.s3a.endpoint` at this new in-house endpoint, with path-style
  addressing — both already proven to work against an S3-compatible server using the exact
  pinned `hadoop-aws:3.4.2` + AWS SDK v2 `bundle:2.29.52` versions in the prior spike (§4 of
  `docs/design-homelab-warehouse-store.md`). The one real simplification: if this module issues
  plain access-key/secret-key pairs with **no session token**, Hadoop S3A's credential-provider
  configuration gets simpler, not harder — `org.apache.hadoop.fs.s3a.SimpleAWSCredentialsProvider`
  instead of `TemporaryAWSCredentialsProvider`, which the prior spike already confirmed works
  end-to-end against a real S3-compatible server with static credentials (§4 of that note). This
  is a probable simplification, not a blocker, **but it was proven there with admin-level static
  keys reused as-is, not with a key freshly minted by a brand-new per-request credential path** —
  that specific combination is untested.
- **`booth-lakehouse`/Lakekeeper**: this is where the plain-pair requirement gets *easier*, not
  harder, to satisfy, confirmed by re-reading this module's own prior design record
  (`docs/decisions/0006` §1-2): Lakekeeper's static-key warehouse credential already has no field
  for a session token, and the broker contract already has a request option
  (`sessionToken: "forbidden"`) built exactly for this case. A self-issued plain pair satisfies
  that shape *natively* — there's no structural tension here the way there is with AWS STS or (so
  far) SeaweedFS's STS. No Lakekeeper-side change is implied by anything in this note.

## What I could not verify

- Whether Hadoop S3A's AWS SDK v2 client signs large uploads with `aws-chunked` streaming
  signatures (vs. one whole-body signature) — general web search did not surface S3A's own
  source or current documentation on this point (§2, §3).
- Whether Hadoop S3A, Iceberg's `S3FileIO`, or Lakekeeper's "remote signing" mode ever use
  presigned URLs rather than header-based SigV4 (§2).
- Whether `ListObjectsV2` support gaps in some non-AWS S3 implementations (flagged by a 2017
  Hadoop JIRA ticket) are still a live concern against the current 3.4.2 source (§3) — not
  re-checked.
- Whether Iceberg's `S3FileIO` itself issues the conditional `PutObject` for commits, or whether
  that happens entirely inside whatever catalog client library Lakekeeper's own REST protocol
  uses — found strong general evidence that *S3 conditional writes* are the mechanism competing
  Iceberg/Delta writers rely on, but no source confirming the exact code path (§3).
- Whether Lakekeeper ever issues S3 operations itself (e.g., purge-on-drop cleanup) rather than
  purely vending credentials for engines to use directly — found consistent descriptions of the
  vended-credential *data* path, nothing confirming or ruling out a server-side purge path (§3).
- Real throughput under Spark-scale concurrent Range GETs and concurrent large multipart writes
  (§7) — this is a feasibility note, no load test was run.
- Current (2026) license/maintenance/conditional-PUT/credential-flexibility facts for Zenko
  CloudServer and `versitygw` beyond what's in §5's table — general web search surfaced partial,
  largely unconfirmed signals for both.

## Questions for other repos

- **`booth-core`**: does a SigV4-signed S3-protocol endpoint need a new, separate ingress path
  that bypasses the gateway entirely (mirroring this module's existing credential-broker
  provider route), or should core's gateway itself be taught to recognize and pass through this
  traffic? This decides whether anything in core changes at all.
- **`booth-spark`**: once/if this module can mint its own plain access-key credentials, confirm
  whether `SidecarCredentialsProvider.java`'s file-based credential re-read (distinct from the
  prior spike's direct environment-variable test) handles a credential that has **no** session
  token field at all cleanly, or whether it currently assumes one is always present.
- **`booth-lakehouse`**: nothing structurally new is asked of it by this note — flagging only
  that if this path is built, Lakekeeper's warehouse-credential contract should be re-confirmed
  against whatever exact field shape this module ends up minting, the same way it already was
  for MinIO (`docs/decisions/0006` §2).
