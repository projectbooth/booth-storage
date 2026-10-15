# Create-only uploads for apps: design note (ADR 0117 point 6, Reconciled design item 8)

Status: proposal, 2026-10-14. No code. Checked against this module's `main` (`7e4b555`),
`booth-architecture`'s ADR 0117 (including "Reconciled design"), `contracts/credential-broker.md`,
ADR 0025/0038/0041/0056, and `booth-streamlit`'s `docs/design-app-writes.md` on its local branch
`design-app-writes` (read-only).

## 1. Create-only PUT

`If-None-Match: *`, `412` if the object exists. Per kind, each using that kind's own native
precondition rather than check-then-write:

- **Filesystem**: already writes to a sibling temp file with `O_EXCL` and `Sync`s it before
  committing (`internal/backend/filesystem/filesystem.go:141-165`). Today's commit is
  `os.Rename`, which unconditionally replaces. For create-only, commit with `os.Link(tmp, final)`
  (atomically fails with `EEXIST` if `final` exists) then remove the temp file. Content is fully
  written and synced before the link, so no partial object is ever visible and no existing one is
  ever clobbered. Reliable — a real OS guarantee, no server-side variance.
- **GCS**: `bucket.Object(key).If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)` — a
  documented precondition, enforced by the real GCS service. Reliable.
- **Azure Blob**: `blockblob.UploadStreamOptions` with `IfNoneMatch: "*"` in the access
  conditions — also documented and enforced by the real service. Reliable.
- **S3**: `minio-go` v7.3.0 already has `PutObjectOptions.SetMatchETagExcept("*")`, sending
  `If-None-Match: *`. The gap is the *server*, not the client: AWS S3 has enforced this since
  2024, but an arbitrary self-hosted S3-compatible backend an operator registers might silently
  ignore the header and overwrite — exactly the failure this ADR exists to prevent, and worse than
  refusing. This module has no way to know from code alone whether a given registered backend
  actually enforces it.

No kind is architecturally stuck at `501` by its SDK. I'd reserve `501` for an S3 backend that
hasn't *proven* it honors the precondition: add a one-time capability probe to the existing
test-connection path (`internal/registry.Test`/`TestSaved`) — PUT a short-lived probe key with the
precondition twice and confirm the second attempt fails — and record the result on the backend.
Create-only PUT on an S3 backend answers `501` until that probe has passed. Open question 1.

## 2. Upload grants held by booth-storage

Per the Reconciled design's own ruling (item 1): storage holds this record, not core — superseding
the "recommended: core" option in `booth-streamlit`'s note, section 2/3.

Shape: `{id, workspace, principal, backendID, prefix, extensions []string, maxBytes, grantedBy,
createdAt}`. `principal` is `"app:<id>"` (see open question 4). A surrogate `id` (not a composite
key) because an owner could plausibly grant the same app two prefixes on the same backend, and it
makes revoke-by-id simple — the same shape as the existing backend registry's own records.

Management API: owner-only, mirroring `/api/admin`'s existing gate
(`auth.Require(auth.Identity.IsAdmin, ...)`, `server.go:102-111`): `GET/POST /api/admin/upload-grants`,
`DELETE /api/admin/upload-grants/{id}`. No update route — narrowing is delete-and-recreate, the
same CRUD shape the backend admin API already uses. No UI here: `booth-streamlit`'s "Write
targets" panel calls this API as the signed-in owner, exactly as it already plans to call
`booth-api`'s equivalent. Every grant/revoke gets an `audit:` log line, matching
`handleAdminCreate`/`handleAdminDelete`'s existing convention.

Owner losing editor/owner: unlike `booth-api`'s write sidecar (Reconciled design item 3), a
storage grant backs no live credential — there's no lease to expire, and this module has no side
channel to ask "does `grantedBy` still hold owner/editor" independent of whatever token is
presenting right now. I'd rely on Reconciled design item 1's own wording instead: take-ownership
"asks both modules to revoke the app's grants" — the module coordinating ownership changes calls
`DELETE`, and storage's job is only to honor it promptly, not to independently track owner health.
Open question 2 if that's not acceptable.

## 3. The admission rule

Every existing write route already sits behind `auth.Require(auth.Identity.CanWrite, ...)`
(`server.go:92-99`) — editor/owner only. An app's token is always minted `roleCeiling: viewer`
(ADR 0117 point 3), so it already gets `403` there today, unchanged.

The new path is one narrow, additive exception on the *same* `PUT /api/backends/{id}/objects/*`
route, dispatched on the precondition header rather than a new URL (matching how
`booth-streamlit`'s note already calls it): if `If-None-Match: *` is present, `Identity.Role ==
RoleViewer`, and a live grant exists with `principal == Identity.Subject` and the cleaned path
under the grant's `prefix` — admit, write create-only, skip `CanWrite`. No request without that
exact combination takes this branch.

The principal match is the real guard, not a separate "is this a workload token" check: a human's
token subject is an OIDC `sub`, never `app:<id>`, so it can never match a grant and always falls
through to the ordinary `CanWrite` `403`. A plain PUT (no header), `DELETE`, and folder-delete
aren't touched by any of this — they stay behind the unchanged `CanWrite`/`IsAdmin` gates, so they
stay `403` for a viewer-capped app token by construction, not by a separate check I have to get
right.

## 4. Validation in storage, independent of the proxy

The proxy validates name shape, extension, and size while streaming (its note, section 3), but a
caller reaching this module directly — including a leaked, still-live app token, replayed before
its 10-minute expiry — must be bound by the same rules regardless, per this package's own
defense-in-depth doc comment (`internal/auth/auth.go:1-13`). Inside the admission check:

- **Path**: `backend.CleanPath` already rejects traversal; additionally require the cleaned path
  to fall under the grant's `prefix` and be exactly one segment below it — an app can't use a
  nested grant prefix to write arbitrarily deep.
- **Extension**: the final segment's extension (case-insensitive) against the grant's allowlist;
  missing/unlisted is `415`.
- **Size**: `http.MaxBytesReader` as today (`server.go:289-291`), but capped at the grant's own
  `maxBytes`, never the module-wide default.
- **Content-Type**: unchanged, freely client-supplied — the extension allowlist is the real gate.

## 5. Audit and limits

Audit: extend the existing `audit:` convention to this path — workspace, backend, path, principal,
grant id, size, outcome (`201`/`412`/`403`/`413`/`415`/`501`), never bytes. Grant create/revoke
logged the same way.

Limits: `booth-api`'s numeric limits (Reconciled design item 5) are sized for table rows, not
objects, so I wouldn't copy them. The grant's `maxBytes` is the primary control. A simple,
in-memory per-principal rate limit would blunt a buggy or compromised app hammering the endpoint —
this module has no existing per-caller limiter to extend, so this is new, small, single-pod state,
not a precise distributed one. Exact number is open question 3's cousin, not settled here.

Retention: stdout only (ADR 0022), like every other audit line in this module today — no new
retention mechanism.

## 6. The pipeline's read side: unaffected

`booth-pipeline` reads via its own workload token (subject names the run, never `app:<id>`),
through the unmodified `GET` routes, gated only by `CanRead` — any real role, unconditionally.
Nothing here narrows, alters, or bypasses that path. `ObjectInfo` carries no provenance field
today, and this design doesn't add one: storage has no notion of "how an object arrived" once it
exists, whether through an app's create-only PUT, the file-browser's ordinary Upload button, or a
notebook. A pipeline's interval trigger keeps listing/reading the same watched prefix exactly as it
does today.

## 7. Test plan

- **Admission (unit)**: live grant + header + viewer → admitted; no grant → `403`; editor/owner
  caller → ordinary `CanWrite` path, untouched; grant exists but path outside prefix, wrong
  extension, or over size → `403`/`415`/`413`, never a silent pass-through; plain PUT (no header)
  from an app token → `403`, unchanged from today.
- **Grant CRUD (unit)**: owner-only create/list/revoke; editor → `403`, matching `IsAdmin`'s
  existing precedent.
- **Per-kind (real backend, this module's existing `BOOTH_TEST_*_ENDPOINT`-gated pattern)**:
  create-only PUT on an empty key succeeds; the identical PUT again answers `412`, verified by
  reading back the *first* write's bytes, never the second's; concurrent create-only PUTs for the
  same key under `-race` — exactly one succeeds. S3 specifically: the capability probe (§1) run
  against the real pinned MinIO test mirror (ADR 0091), since this is the one thing reading code
  can't settle.
- **Real-core (test/integration, once `booth-api`/`booth-streamlit`'s pieces exist)**: an app
  uploads once, the same stored name `409`s [proxy-side], outside-prefix `403`s, bad extension
  `415`s, oversize `413`s — overlaps `booth-streamlit`'s own real-core list; I'd expect to co-own
  this scenario rather than duplicate it from storage's side alone.

## What I need

**From `booth-core`**: nothing mechanical — the app's workload token and the workload-issuer trust
are both already unchanged and already implemented (ADR 0056). Worth a one-line confirmation, not
new code: that an app token's `sub` really is `app:<id>` (open question 4), and that `roleCeiling`
for an app token is always exactly `viewer`.

**From `booth-streamlit`**: call this module's new owner-only upload-grant API as the signed-in
owner (same shape it already plans for `booth-api`'s dataset grants), not core's record — its own
note (section 2/3) currently says the declaration "recommended: core," which the Reconciled design
has since superseded; that note needs a small correction here. Call the create-only PUT exactly as
already planned (`If-None-Match: *`, through the gateway, the app's own token, `X-Workspace`). Call
this module's revoke endpoint on take-ownership, per Reconciled design item 1 — that's its
responsibility to trigger, not something storage can detect on its own.

## Open questions for the user

1. Require the S3 capability probe (§1) before honoring create-only PUT on a given S3 backend
   (`501` until proven), or assume `If-None-Match` works on every S3-compatible backend an
   operator might register? I recommend the probe.
2. Is "the coordinating module calls `DELETE` on ownership change" (§2) an acceptable answer, or
   does storage need its own, independent way to notice a granting owner's role has lapsed?
3. A specific per-principal rate-limit number for uploads (§5), or defer until there's a real
   abuse case?
4. Confirm the literal principal string this module's grants key off: `app:<id>` (Reconciled
   design item 1) vs. `streamlit:<ws>:<appId>` (`booth-streamlit`'s own draft, section 1) — a
   one-line answer, not a design choice of mine to make.
