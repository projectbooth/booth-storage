# Contract: upload grants and create-only writes

Version: v0 (ADR 0117, its "Reconciled design" and its ruling items 8/8a; this module's own
design note, `docs/design-app-writes.md`). Owned by `booth-storage`. Written for
`booth-streamlit` (the "Write targets" panel and the create-only upload call) and for anyone
building against either surface. Mirrors `booth-api`'s `docs/contracts/app-writes.md` where it
applies: owner-only management routes, a grant replacing an earlier one for the same scope, and
a single `revoke-principal` call with the same shape and idempotency.

Two surfaces:

1. **The grant management API**: owner-only routes on this module's ordinary admin API, called
   with a person's own sign-in token. `booth-streamlit`'s panel calls these *as the signed-in
   owner* — this module holds the grant (ADR 0117 "Reconciled design" item 1), not core, and not
   a second copy inside `booth-streamlit` itself.
2. **The create-only write**: the existing `PUT /api/backends/{id}/objects/*` route, called with
   the app's own workload token and `If-None-Match: *`.

## Principal

A grant names the **literal subject the caller's token carries**: `streamlit:<workspace>:<appId>`
(`<appId>` matches booth-api's own contract: `[A-Za-z0-9._-]{1,128}`). `<workspace>` must be the
workspace the grant is made in — a grant can never admit a write from another workspace's app.
`apiwrite:<api-id>` (booth-api's own write-sidecar subject) is a different module's concern and is
never a valid principal here.

## 1. Management API

Base: `/modules/storage/api` through core's gateway (this module serves it as `/api`).
Headers: `Authorization: Bearer <person's OIDC token>`, `X-Booth-Workspace: <slug>`.
Errors: `{"error": "<message>"}`, plus `"field"` on a `422` validation refusal. Every route below
needs the **owner** role in the workspace (`403` otherwise, re-derived from the verified token,
never trusted from a forwarded header — ADR 0041). An app's own workload token gets `403` here
too: it is never an acceptable caller of its own management API.

```
GET    /api/admin/upload-grants                       owner   ?principal=
POST   /api/admin/upload-grants                       owner   {"principal","backendId","prefix","extensions","maxBytes"}
DELETE /api/admin/upload-grants/{id}                   owner
POST   /api/admin/upload-grants/revoke-principal       owner   {"principal"}
```

There is no update route: narrowing a grant is delete-and-recreate, the same shape this module's
backend admin API already uses.

A grant:

```json
{"id": "…", "workspace": "acme", "principal": "streamlit:acme:a2b3c4", "backendId": "lake",
 "prefix": "incoming/", "extensions": ["csv", "png"], "maxBytes": 26214400,
 "grantedBy": "<owner sub>", "createdAt": "…"}
```

- **`GET`** lists every grant in the workspace, or only `principal`'s if given.
- **`POST`** creates a grant (`201`), or — for the same `(principal, backendId, prefix)` —
  **replaces it outright with a fresh id and the new `extensions`/`maxBytes`**, the way saving an
  owner's "Write targets" panel is meant to work: one call, not a check-then-decide. A different
  `prefix` on the same backend is a distinct grant, not a replacement — an owner can grant one app
  two separate upload targets.
  - `extensions` is required, non-empty; each entry is normalized to a bare lowercase extension
    (a leading `.` is stripped, so `.CSV` and `csv` are equivalent).
  - `prefix` follows this module's own listing-prefix rules (`backend.CleanPrefix`): `""` means
    the backend's root, otherwise it is normalized to end in `/`.
  - `maxBytes` must be greater than zero. There is no platform-wide ceiling here: the grant's own
    number *is* the control (ADR 0117 "Reconciled design" item 8a; a per-principal rate limit was
    considered and deferred until there's a real abuse case).
  - Refusals: `404` if `backendId` doesn't name a backend in this workspace; `422` with a `field`
    if `principal` isn't `streamlit:<this workspace>:<appId>`, or `extensions`/`maxBytes` are
    invalid.
- **`DELETE /{id}`** removes one grant. A second delete of the same id is `404`.
- **`revoke-principal`** removes every live grant the principal holds in the workspace, across
  every backend, in one statement. It answers `{"revoked": n, "items": [grant, …]}`. It is
  idempotent (`revoked: 0`, `items: []` the second time). Two different callers may reach it — see
  below.

### `revoke-principal`'s second caller: the grants-service identity (ADR 0117 item 1a)

`booth-streamlit` cannot hold a person's token: its UI is reached through core's iframe proxy,
which gives it a signed identity assertion, not a bearer token. So `revoke-principal` — and only
`revoke-principal` — also accepts a **core-minted workload token `booth-streamlit` mints for
itself**:

```
POST /api/admin/upload-grants/revoke-principal
Authorization: Bearer <core-minted token, subject streamlit:<workspace>:grants, booth_module=streamlit>
X-Booth-Workspace: <that same workspace>
{"principal": "streamlit:<that workspace>:<appId>"}
```

- The subject must be the literal `streamlit:<workspace>:grants`, and the token's `booth_module`
  claim must be `streamlit` — this module checks both, so a different module can't claim this
  identity merely by guessing the subject shape. `X-Booth-Workspace` must equal the token's own
  embedded workspace (never trusted alone, same as every other header this module reads).
- This identity is refused on **every other route** in this module — grant create, list and
  delete-by-id; every backend and object route; everything. It satisfies none of
  `CanRead`/`CanWrite`/`IsAdmin`, even though the token it presents carries a real, viewer-capped
  workspace-membership claim core mints it with: that claim is deliberately not honoured here.
- Even on `revoke-principal`, it may only name a principal shaped `streamlit:<its own
  workspace>:<appId>` — never another workspace's, never a non-app subject, and never the literal
  `…:grants` subject itself (reserved: `POST /api/admin/upload-grants` already refuses an owner
  trying to create a grant for that principal, so there is never anything to revoke there).
  Anything else is `403`.
- `booth-streamlit` calls this on take-ownership, as part of the same step as the owner change,
  and **must fail closed** if this module can't be reached or answers non-2xx: the owner change
  doesn't happen.

Every create, delete and revoke is logged on stdout (`audit: …`, ADR 0022) with the workspace,
grant id, principal, backend and the owner who acted — never the grant's byte contents, because
there are none to log.

## 2. The create-only write

```
PUT /api/backends/{id}/objects/{path}
Authorization: Bearer <the app's own workload token, unchanged>
X-Booth-Workspace: <the app's workspace>
If-None-Match: *
```

**Admission (ADR 0117 point 8a).** This route is not behind the ordinary editor/owner write gate.
Instead:

- An **editor or owner** token may always write here, `If-None-Match: *` or not — unchanged from
  before this contract existed. For them, the header simply asks for create-only semantics on an
  otherwise ordinary write; no grant is needed or consulted.
- Anyone else needs **all** of: `If-None-Match: *` present, a live grant whose `principal` equals
  the token's verified subject, whose `backendId` matches `{id}`, and whose `prefix` equals the
  object path's directory **exactly** — one segment below the grant, never deeper, never outside
  it. A plain `PUT` (no header), `DELETE`, and folder delete/create/move are **not** reachable this
  way at all: they stay behind the ordinary `CanWrite` gate, which a viewer-capped app token never
  satisfies. There is no way for an admitted app to do anything to this backend except the one
  create-only write its grant describes.

**Validation, in order:**

| Status | When |
|---|---|
| `403` | No live grant for this principal/backend/prefix combination (includes: outside the grant's prefix, or more than one segment below it) |
| `415` | The object's extension isn't in the grant's `extensions` |
| `413` | The body exceeds the grant's `maxBytes` (never the module-wide upload limit, which may be larger or absent) |
| `501` | The backend is S3 and hasn't yet passed the create-only capability probe (below) — for **every** caller, not only an admitted app |
| `412` | The object already exists (the actual create-only precondition, enforced by the backend itself) |
| `200` | Stored; body is the same `ObjectInfo` an ordinary write returns |

**Per-kind atomicity.** Filesystem, Azure Blob and GCS always honour `If-None-Match: *`
atomically — a real OS guarantee or a documented, server-enforced precondition, never
check-then-write. An S3 backend only answers with create-only semantics once it has passed a
one-time capability probe: `POST /api/admin/backends/{id}/test` with no body (the existing
"test connection" action against an already-saved backend, not the edit-preview form of that same
route) now also attempts a real conditional write twice against the registered server and records
whether the second attempt was refused. Until that has run and passed, this route answers `501`
for that backend — not a guess, and never a silent overwrite. A config change (a different
endpoint or bucket) resets the result, since nothing proved the new server's behaviour.
`GET /api/admin/backends/{id}` reports the current result as `createOnlySupported`.

## What each layer enforces

| Layer | Refuses |
|---|---|
| `booth-core` / gateway | Forwards the app's own viewer-capped workload token unchanged; mints nothing new for this path (ADR 0056) |
| `auth.Middleware` (this module) | Re-derives the caller's role from the verified token, independent of any forwarded header (ADR 0041); a token granting no role in the workspace |
| `internal/uploadgrant` | Any write outside a live grant's principal, backend, prefix, extension allowlist or size cap |
| `internal/backend` (per kind) | The actual create-only precondition, atomically, per kind — never checked-then-written |
| `revoke-principal`'s own check (ADR 0117 item 1a) | A grants-service identity naming anything but `streamlit:<its own workspace>:<appId>`; that identity on every other route |

## What `booth-streamlit` needs to do

- It cannot hold a person's token (core's iframe proxy, not a bearer token), so the owner makes
  the grant **in this module's own Admin page**, with the owner's own token — not a record of
  `booth-streamlit`'s own, and not core's. `booth-streamlit`'s "Write targets" panel links to that
  page with the app's principal, backend and prefix filled in (see "URL prefill" below).
- Call the create-only write exactly as described above: the app's own token, unchanged,
  `If-None-Match: *`, through the gateway.
- On take-ownership, mint itself a workload token (subject `streamlit:<workspace>:grants`,
  `booth_module: streamlit`, viewer cap) and call `revoke-principal` with it, and fail the
  ownership change closed if that call doesn't come back `2xx`.

## URL prefill for the Admin page's "App uploads" panel

A link from `booth-streamlit`'s "Write targets" panel into this module's Admin page may carry
query parameters that prefill the grant-creation form (storage-ui, not this HTTP API — listed
here because the names are part of what `booth-streamlit` builds against):

```
/storage/admin?panel=upload-grants&principal=streamlit%3Aacme%3Aa2b3c4&backendId=lake&prefix=incoming
```

| Parameter | Prefills |
|---|---|
| `panel=upload-grants` | Opens directly to the "App uploads" panel |
| `principal` | The principal field (`streamlit:<workspace>:<appId>`) |
| `backendId` | The backend select |
| `prefix` | The prefix field |

All three are plain prefill: the owner still reviews and submits the form. An unknown
`backendId` or a malformed `principal` is left for the ordinary form validation to catch on
submit, the same as if the owner had typed it.
