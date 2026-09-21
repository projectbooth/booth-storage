# 0003: The filesystem kind is off by default and confined to operator-allowed roots

Status: **ratified as built** — promoted to `booth-architecture` ADR 0040
(`decisions/0040-storage-filesystem-backend-allowlist.md`), which is the authoritative record.

## Context

ADR 0013 requires a local/server-filesystem backend at v0, and ADR 0036 lets a workspace
**owner** register backends from a UI. Combined naively, that means any workspace owner can
type `/` (or another tenant's directory, or `/var/run/secrets/...`) as a "storage backend"
and then read and write it through the API. Workspaces are tenants (ADR 0008); a filesystem
backend is the one kind where "which bytes can be reached" is decided by the *server's* disk,
not by the tenant's own cloud account.

## Decision

- **Disabled by default.** With no configuration the `filesystem` kind is not offered in the
  admin UI (`GET /api/kinds` omits it) and registration is refused with a clear message.
- **The operator decides what's shareable**: `BOOTH_STORAGE_FILESYSTEM_ROOTS` (chart value
  `filesystem.roots`) is an allow-list of directories. A registered `rootPath` must equal or
  sit inside one of them.
- **Per-workspace roots** via a `{workspace}` placeholder: `/data/{workspace}` lets
  workspace `acme` register directories under `/data/acme/` and never `/data/globex/`. A root
  without the placeholder is shared by every workspace — an explicit operator choice, fine for
  a single-tenant install.
- The workspace slug is re-validated (`^[a-z0-9-]+$`, ≤63) before being interpolated into a
  path, rather than trusting the gateway header alone.
- Enforced **twice**: at registration, and again every time a backend is opened — so an
  operator tightening the roots takes effect on already-registered backends.
- Checked lexically **and** after resolving symlinks, so a symlink inside an allowed root
  that points outside it can't be registered.
- At I/O time every operation goes through `os.Root` (Go 1.24+), which makes the OS reject
  `..` and escaping symlinks itself; `backend.CleanPath` runs first as defense in depth, not
  as the boundary.
- Writes are atomic (temp file + rename in the same directory), and never create the root
  itself — only directories inside it.

## Root directory handling (added 2026-09-21, after booth-e2e's first full run)

booth-e2e found that registering a `rootPath` that didn't exist returned `201` and then failed every
read and write with a 502. Implementation-level fix, not a contract change: `rootPath` is now
checked at registration (and on edit).

- exists and is a directory → used as is;
- exists but is a file → refused (`422`);
- missing, **parent exists** → the leaf directory is created (`0750`);
- missing, parent missing too → refused (`422`), nothing created.

**Why only the leaf**, and not the whole path: if the operator's volume failed to mount, creating
the full path would silently succeed on the container's ephemeral disk, and a workspace would
store "durable" data that vanishes on restart. Requiring the parent turns a missing mount into a
loud error at registration. This runs *after* the allow-list policy, so nothing outside an allowed
root is ever created. "Test connection" never creates anything and reports an uncreatable path as
a failed check.

## Consequences

- An operator who wants filesystem backends must mount volumes, set the roots, and make the
  directories writable by uid 65532 (the chart documents this and exposes
  `filesystem.volumes` / `filesystem.volumeMounts` / `podSecurityContext.fsGroup`).
- Sharing a directory across workspaces is possible only by an operator choosing a
  placeholder-free root.
- Not addressed: quotas, and two workspaces on a placeholder-free root can see each other's
  files (that is what "shared" means).
