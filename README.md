# booth-storage

Project Booth's storage module (nav group **Manage**). A workspace registers any number of
storage backends side by side — **S3-compatible, server filesystem, Azure Blob Storage and
Google Cloud Storage** (ADR 0013) — each addressable by a stable ID, with credentials held
securely. Other modules read, write and list through one generic API keyed by backend ID
(ADR 0035). Brief: `../booth-architecture/agent-briefs/storage.md`.

## What v0 delivers

| Definition-of-done item | Where |
|---|---|
| Multiple simultaneous backends per workspace, all four kinds, no "current backend" | `internal/registry`, `internal/backend/{s3,filesystem,azure,gcs}` |
| Credentials stored securely (ADR 0020) | Kubernetes Secrets — `internal/registry/kubernetes.go`, [decision 0001](docs/decisions/0001-runtime-credentials-as-kubernetes-secrets.md) |
| Unified file browser over a backend's real contents (ADR 0037): folders synthesized from `/`-delimited keys on S3/GCS/Azure, real directories on filesystem, paginated, prefix-aware `list` | `internal/backend/*` (`List`), `web/src/components/ObjectBrowser.tsx` |
| File browser writes (ADR 0038): create folder, upload, rename, delete — `editor`/`owner` only, hidden from viewers in the UI **and** refused server-side (403) | `internal/api/server.go` (`auth.Require(CanWrite)`), `internal/backend/flatstore.go`, `web/src/components/ObjectBrowser.tsx` |
| Distinct admin view (`adminNavPath`, ADR 0036) + regular browse/select view | `web/src/views/{AdminView,BrowseView}.tsx`; owner-only `/api/admin/*` |
| Generic read/write/list API keyed by backend ID | `internal/api/server.go`, `internal/backend/backend.go` |
| Manifest + health check | `charts/booth-storage/templates/boothmodule.yaml`, `/healthz` |
| CI per `contracts/testing-strategy.md` | `.github/workflows/` |

## Stack

Matches booth-core / booth-module-store: **Go** + `chi` + `go-oidc` backend; **React + TypeScript
+ Vite + Tailwind** UI in `web/`, published as `@projectbooth/storage-ui` (ADR 0030); Helm chart
with a `BoothModule` manifest (ADR 0019). Additions: PostgreSQL via `pgx` for backend metadata
(ADR 0014), `client-go` for credential Secrets. Go floor is **1.26** (current `client-go`
requires it; sibling repos are on 1.23 — each repo builds independently).

```
cmd/storage/           entrypoint
internal/backend/      the generic Backend interface + path rules; one package per kind
  backendtest/         conformance suite every kind must pass
internal/registry/     backends collection: service, Postgres + Kubernetes-Secret stores, filesystem policy
internal/api/          HTTP routes (regular, data, admin)
internal/auth/         OIDC verification + role checks (defense in depth)
web/                   the UI package (regular view + admin view)
charts/booth-storage/  Helm chart, BoothModule manifest, narrowly-scoped RBAC
docs/decisions/        judgment calls the ADRs didn't settle — read these
test/contract, test/integration
hack/                  docker-compose for MinIO/Azurite/Postgres test containers
```

## API (what other modules call)

Reached through core's gateway at `/modules/storage/api/...`. Every request needs
`Authorization: Bearer …` and `X-Workspace`. Errors are `{"error": "...", "field"?: "..."}`.

| Route | Role | |
|---|---|---|
| `GET /api/backends`, `GET /api/backends/{id}` | any | registered backends: id, name, kind, short location |
| `GET /api/backends/{id}/objects?prefix=&recursive=&limit=&cursor=` | any | list (paged; opaque cursor) |
| `GET /api/backends/{id}/objects/{path}` | any | stream an object |
| `PUT /api/backends/{id}/objects/{path}` | editor, owner | write (replaces; atomic — a failed upload leaves nothing) |
| `DELETE /api/backends/{id}/objects/{path}` | editor, owner | delete one object (404 if missing) |
| `POST /api/backends/{id}/folders` `{"path"}` | editor, owner | create a folder (idempotent) |
| `DELETE /api/backends/{id}/folders/{path}` | editor, owner | delete a folder and everything in it → `{"deleted": n}` |
| `POST /api/backends/{id}/move` `{"from","to","folder"}` | editor, owner | rename/move an object or folder; **never overwrites** (409) |
| `GET/POST /api/admin/backends`, `GET/PUT/DELETE …/{id}` | **owner** | register / edit / remove backends and credentials |
| `POST /api/admin/backends/{id}/test`, `POST /api/admin/test-connection` | **owner** | test a saved or unsaved config |
| `GET /healthz` · `GET /livez` | none | readiness (checks DB; what core polls) · liveness |

Paths are relative, `/`-separated, no `..`/empty segments/leading slash — identical rules on
all four kinds. **Folders on object stores** (S3, GCS, Azure) are synthesized from `/`-delimited keys;
"create folder" writes a zero-byte `folder/` placeholder object (ADR 0038) — expected, and visible
to external tools as a literal empty object. On the filesystem they are real directories. Folder
delete/rename on object stores is list-then-act per object, capped at 100,000 objects per request,
and a folder rename there is copy-then-delete (not atomic): it makes every copy first, rolls the
copies back if one fails, and only then removes the originals. Credentials are **write-only**: no route returns them. Removing a backend never
touches its data.

## Running and testing

```sh
docker compose -f hack/docker-compose.emulators.yml up -d --wait   # MinIO, Azurite, Postgres
eval "$(hack/test-env.sh)"                                          # BOOTH_TEST_* variables
go test ./...                                                       # unit + contract
(cd web && npm ci && npm run typecheck && npm run lint && npm test -- --run && npm run build)
```

Without the containers those tests **skip** locally (CI sets `BOOTH_TEST_REQUIRE_EMULATORS=1`,
which makes a missing emulator a failure). GCS runs in-process (`fake-gcs-server`); the
symlink tests need Linux (they skip on Windows). `test/contract` needs `helm`.

Local run without a cluster or database: `BOOTH_STORAGE_DEV_MEMORY=true` (state and credentials
live in memory and vanish on exit). Also needs `BOOTH_OIDC_ISSUER_URL` / `BOOTH_OIDC_CLIENT_ID`;
`BOOTH_STORAGE_FILESYSTEM_ROOTS=/data/{workspace}` enables the filesystem kind
([decision 0003](docs/decisions/0003-filesystem-backend-allow-list.md)). `web`'s `npm run dev`
is a dev harness proxying to a backend on `:8080` (`BOOTH_STORAGE_DEV_BACKEND` overrides).

CI: `ci.yml` (every push/PR — vet, `-race` tests against real emulators, web checks, helm lint,
image build), `integration.yml` (kind cluster, merge-to-main + nightly), `publish.yml`
(`storage-ui-v*` tags → npm), `release.yml` (`v*.*.*` tags → image + chart).
**Branch protection on `main` is a GitHub setting and has not been configured from here.**

## Wiring into booth-design

Add `@projectbooth/storage-ui` as a dependency, import `…/dist/style.css`, and register it:
`registerNativeModule("storage", StorageApp)`. **Read
[decision 0002](docs/decisions/0002-admin-view-mounting-gap.md) first** — the shell passes no
route/view prop and renders no nav link to `adminNavPath`.

## Read before deploying

- [0001](docs/decisions/0001-runtime-credentials-as-kubernetes-secrets.md) — credentials are Secrets written by this module (ratified: ADR 0039). Install it in a namespace of its own.
- [0002](docs/decisions/0002-admin-view-mounting-gap.md) — admin view mounting gap; routed to booth-design.
- [0003](docs/decisions/0003-filesystem-backend-allow-list.md) — filesystem kind is off by default (ratified: ADR 0040).
- [0004](docs/decisions/0004-role-header-trust.md) — the role is derived from the token's `groups` claim and an over-claiming `X-Booth-Role` is rejected with 403 (ADR 0041). **`oidc.groupsClaim` must match booth-core's**, or every request is refused.
- [0005](docs/decisions/0005-workload-token-issuer.md) — optionally trusts booth-core's JWKS as a second token issuer for unattended-run (workload) tokens (ADR 0056); off unless `oidc.workloadIssuerUrl` is set.

## Behaviors worth knowing

- **Database (ADR 0053/0054).** The manifest declares `database: {enabled: true}`, so booth-core
  creates this module's PostgreSQL database and role and delivers the Secret
  `booth-database-credentials` (`dsn` key) into the release namespace — nothing to create by hand.
  The pod may wait in `CreateContainerConfigError` briefly after install until core writes it. To
  bring your own database instead: `postgres.provisionedByCore=false` and `postgres.dsnSecret.name`.
- **Filesystem `rootPath` is checked at registration.** A directory that exists is used as is; one
  that is missing is **created if its parent directory exists**; if the parent is missing too, or the
  path is a file, registration is refused (`422`). (Found by booth-e2e: a nonexistent root used to
  register fine and then fail every read and write.) Only the leaf is ever created, deliberately: if
  a volume failed to mount, creating the whole path would silently put data on the container's
  ephemeral disk. "Test connection" reports an uncreatable path as a failed connection and never
  creates anything.
- **A path that runs through a file is not a server error.** `?prefix=` naming a file lists as
  empty, reading through one is `404`, and creating beneath one is `409` — the same on every kind.

## Not done

- **Azure hierarchical-namespace accounts are untested.** ADR 0037 names that mode; Azurite can't
  emulate it. Listing goes through the Blob API's `/` delimiter (which serves both modes) and
  skips `hdi_isfolder` directory-marker blobs — that skip is tested with a simulated marker, but
  nothing has run against a real HNS account.
- **Azure hierarchical-namespace "create folder"** uses the same `folder/` placeholder blob as flat
  mode rather than a real directory (ADR 0038 would prefer a real one); untestable without a real
  HNS account.
- **Object stores can't rename atomically**, and a folder rename/delete is bounded (100k objects).
  A single-object move above S3's 5 GiB copy limit relies on minio-go's compose fallback, which
  isn't exercised by any test here.
- **Upload has no progress bar** (sequential `fetch`; per-file status only) and the overwrite prompt
  only knows about entries already loaded on screen — the server replaces regardless.
- No "copy location" affordance in the browser: `booth-catalog` needs a way to reference a
  browsed-to location, and that reference format (a shared contract) isn't specified anywhere, so
  I didn't invent one. Today a consumer addresses an object as backend ID + path.
- Deploying alongside a real, pinned booth-core (`test/integration/README.md`).
- The layer-3 workflow has never been executed (no kind/k3d available when written).
- Quotas, object copy (as distinct from move), and handing credentials to other modules — out of v0 scope.
- Nothing has run this module against a real booth-core's database provisioning yet: the chart
  declares `database: {enabled: true}` and reads `booth-database-credentials`/`dsn`, and CI
  exercises that with a hand-made Secret of the same shape, not one core wrote.
