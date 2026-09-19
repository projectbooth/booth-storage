# Real-cluster integration tests (layer 3)

Per `contracts/testing-strategy.md` and ADR 0024. Runs on merge to `main` and nightly
(`.github/workflows/integration.yml`), not on every push — it's the slower "actually
deployed to a real cluster" layer.

## What this covers

Deploys the chart into an ephemeral `kind` cluster alongside a throwaway PostgreSQL and
verifies:

- The pod becomes `Ready`, and `/healthz` returns 200 through the in-cluster Service — which
  only happens once migrations have run and Postgres is reachable.
- The chart's `BoothModule` custom resource is created with `navPath` **and** `adminNavPath`
  (ADR 0019 registration, ADR 0036), against a vendored copy of booth-core's CRD
  (`fixtures/boothmodule-crd.yaml`).
- The API rejects unauthenticated requests from inside the cluster.
- **RBAC is exactly what ADR 0020 needs and no more** (`kubectl auth can-i` as the
  module's service account): `get/create/update/delete` on Secrets in its own namespace;
  no `list`/`watch`, nothing in other namespaces, no other resources.
- `credentials_test.go` (build tag `integration`): the credential store creates, replaces
  (wholesale — a dropped key really disappears), reads and deletes real Secrets on a real
  API server.

Everything else — all four backends' read/write/list, the registry, the HTTP API, RBAC roles
on routes — is covered by the per-push unit/contract layer (against real MinIO, Azurite and
PostgreSQL containers, plus an in-process GCS server).

## What this doesn't cover yet, and why

**Deploying alongside a real `booth-core`** and exercising booth-storage through core's
gateway (`/modules/storage/*`), with core's controller actually reconciling our `BoothModule`
and polling `/healthz`. This is the layer-3 scenario `contracts/testing-strategy.md`
describes, and it is the main gap.

`booth-core` has tagged `v0.1.0` and `v0.1.1`, and its `release.yml` publishes an image and a
chart on `v*` tags, so a pinned build exists to target — I did **not** verify those artifacts
are actually published and pullable. I deliberately did not wire it up in this pass:

- Pulling them from a private org needs a cross-repo credential (the workflow's default
  `GITHUB_TOKEN` can't read another repo's release asset or package), and I can't tell whether
  such a secret exists.
- core's chart pulls in NATS and requires an `iframeSigningKey`, so this is a real,
  multi-part deployment.
- There is no `kind`/`k3d` available where this was written, so I could not execute *any* of
  the cluster workflow, let alone a larger one. A multi-repo cluster workflow shipped without
  ever having run is more likely to be red for reasons unrelated to storage than useful.

Wire it up once a token for pulling core's pinned build is in place, and pin to a tag rather
than core's `main` (`contracts/testing-strategy.md`). `booth-e2e` is the intended home for the
cross-module smoke path.

An authenticated end-to-end call (real token → data API) also isn't here: it needs an OIDC
provider in the cluster. The auth path is tested at the unit layer against an in-process OIDC
provider (real discovery, JWKS and signed JWTs) instead.

## A note on the OIDC issuer used in CI

`integration.yml` points `oidc.issuerUrl` at a real, publicly reachable OIDC discovery
document purely so `auth.NewVerifier`'s startup-time discovery succeeds and the pod becomes
healthy — no real login happens, and `requireAudience` is left off. A CI convenience, not a
statement about which identity provider a real deployment should use (ADR 0004).
