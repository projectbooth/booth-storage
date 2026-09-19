# 0001: Credentials registered at runtime are stored as Kubernetes Secrets written by booth-storage

Status: **ratified as built** — promoted to `booth-architecture` ADR 0039
(`decisions/0039-storage-runtime-credential-secrets.md`). That ADR is the authoritative record and
also makes this the pattern for any module accepting runtime-registered credentials; this file
keeps the build-time reasoning and alternatives.

## Context

ADR 0020 says credentials are delivered as Kubernetes `Secret`s "provisioned ahead of
time", read by the consuming module "the ordinary Kubernetes way: mounted files or
environment variables", with no live credential-serving API. It names `booth-storage`
explicitly as a "granting module" that provisions such a Secret.

`booth-storage`'s brief (and ADR 0036) also requires that an owner can register, edit and
remove backends **and their credentials at runtime, from a UI**. A cloud credential typed
into that form does not exist "ahead of time", and a pod cannot mount or receive as an env
var a Secret that didn't exist when the pod started.

So two parts of the settled design pull against each other for exactly this module: ADR
0020's "provisioned ahead of time, mounted" and ADR 0036's "registered at runtime in a UI".

## Decision (as built)

- **Non-secret metadata** (id, display name, kind, bucket/endpoint/root path, whether
  credentials exist) lives in PostgreSQL — this module's own database on the shared cluster
  (ADR 0014).
- **Credentials never touch the database.** Each backend's credentials are one Kubernetes
  `Secret` in booth-storage's own namespace, written by booth-storage when an owner saves the
  form (`internal/registry/kubernetes.go`). The Secret name is an opaque hash of
  `(workspace, id)`; it is labelled `app.kubernetes.io/managed-by=booth-storage`,
  `booth.projectbooth.io/workspace`, `booth.projectbooth.io/storage-backend`.
- booth-storage **reads its own Secrets back through the Kubernetes API** (`get` by derived
  name) when it opens a backend, rather than via a mount or env var — a mount can't see a
  Secret created after the pod started.
- The chart grants a namespaced `Role` with exactly `get/create/update/delete` on `secrets`
  in that one namespace. No `list`, no `watch`, no ClusterRole. A contract test
  (`test/contract/manifest_test.go`) and the integration workflow both assert this.
- Credentials are write-only through the API: no route ever returns them (a test asserts
  this against every route that returns backend records), and an edit that leaves credential inputs blank keeps the
  stored ones.
- Other modules never receive credentials from booth-storage. They call the generic
  read/write/list API; booth-storage holds the credential and does the I/O. (Handing a
  credential to another module — e.g. `booth-spark` needing direct S3 access — is a
  separate, later question this decision does not answer.)

## Why this needs a decision

This keeps ADR 0020's actual trust boundary — credentials live in Kubernetes `Secret`s, and
there is no endpoint that serves a credential to other modules on demand — but it departs
from its letter in two ways worth an explicit yes/no:

1. booth-storage's service account can **create and read Secrets in its namespace at
   runtime**. ADR 0020 framed the "no live credential-write capability" lesson around core
   not holding a standing write credential; here booth-storage does hold one, scoped to one
   namespace. It cannot read other namespaces' Secrets. Anything else that keeps Secrets in
   booth-storage's namespace is readable by it, so **install it in a namespace of its own**
   (the chart's `values.yaml` says so).
2. Reading via the API rather than a mount. Rotation is therefore immediate (an edit takes
   effect on the next request, subject to a ≤10-minute client cache) rather than
   "re-provision and restart the pod".

## Alternatives considered

- **Encrypt credentials into PostgreSQL** with a key from a chart-provisioned Secret. Needs
  no Kubernetes write access, but puts credentials in the database (backups, replicas,
  anyone with SQL access) and invents a key-management story ADR 0020 deliberately avoided.
  Rejected as a bigger departure from 0020's intent than the chosen design.
- **Registration only through core/GitOps**, with the UI just selecting pre-provisioned
  Secrets. Preserves 0020 to the letter, but contradicts ADR 0036 (a UI for registering
  credentials) and pushes credential handling out to a mechanism that doesn't exist yet
  (core has no Secret-provisioning API for module use).
- **External secrets manager** (Vault / ESO). ADR 0020 already deferred this; still not v0.

## If this is rejected

The `CredentialStore` interface (`internal/registry/model.go`) is the seam: only
`kubernetes.go` and `cmd/storage/main.go` would change to swap in another implementation.
