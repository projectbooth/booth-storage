# 0004: The role is re-derived from the token, not trusted from the gateway header

Status: **implemented** — this finding became `booth-architecture` ADR 0041
(`decisions/0041-module-role-derivation-from-token.md`), which makes it a fleet-wide requirement.
This file records the measurement that led to it and how booth-storage implements it.

## What was found (before the change)

## Finding

Verified against a real Keycloak (booth-architecture's `local-dev` realm) and the real
server binary: a **viewer's** valid token plus a forged `X-Booth-Role: owner` header,
sent directly to the pod, gets `200` on `GET /api/admin/backends`. The token verifies
(signature, issuer, expiry) and the module then takes the role from the header.

This is inherent to ADR 0025 / `core-platform-api.md`: core's gateway derives
`(workspace, role)` from the token's `groups` claim and forwards it as
`X-Booth-Workspace`/`X-Booth-Role`, and modules trust those "only because they also verify
the token". The token check proves *who* is calling; it does not prove they hold the *role*
in the header. `booth-module-store` has the same property.

It matters more for booth-storage than most modules: the owner-only surface is credential
registration (cloud keys, service-account JSON), and a role forgery reaches all of it.
The contract's own wording is that a module must not trust "the network path implicitly" —
here authorization, not authentication, still does.

ADR 0038 makes this sharper: the same header now also gates **destructive file operations**
(delete a folder and everything in it, rename, upload) for `editor`, and `viewer` is defined as
read-only. A viewer's valid token plus a forged `X-Booth-Role: editor` reaches every one of them
when the pod is reachable directly (verified for the admin route; the write routes share the
identical gate). The brief asks to "verify `X-Booth-Role` independently on every write" — this
module checks the header on every write route (tested), but the header itself is only as
trustworthy as the network path, which is exactly the gap described above.

## What is implemented now

The updated brief says: verify `X-Booth-Role` independently on every write, don't just trust the
gateway forwarded it correctly. So `internal/auth` now re-derives the role from the
**already-verified token's groups claim** (ADR 0025's `/workspaces/<slug>/<owner|editor|viewer>`
shape) for the requested workspace, following ADR 0041 exactly:

- a forwarded role **stronger than the token grants is rejected with 403** (and logged as
  `auth: rejected: forwarded role "owner" exceeds token-derived role …`) — not downgraded and
  carried on;
- a header may still *narrow* the grant (a gateway restricting a session); an absent header means
  "use the token's grant"; an unrecognized value means no access;
- a token with no role in the requested workspace is refused (403) before any handler runs;
- so the effective role is never stronger than the token's grant, whatever the network path.

Re-measured against the real Keycloak realm and binary: a viewer's real token + `X-Booth-Role: owner`
was `403` on the admin API and every write route (it had been `200`). Since ADR 0041 the same
request is rejected outright, reads included. Unit tests cover the same
through a real signed-JWT verifier and the HTTP layer, and fail if the header is trusted again.

**Deployment consequence:** `oidc.groupsClaim` (env `BOOTH_OIDC_GROUPS_CLAIM`, default `groups`)
**must match booth-core's** setting, or every request is refused (fail-closed: "your token grants
no role in this workspace"). A provider that emits workspace roles some other way than a groups
claim would need this extended.

## Remaining exposure (original analysis, kept for context)

Only reachable by a caller who can send requests to the pod **without going through core**
— i.e. anyone with in-cluster network access, or a cluster with no ingress restriction. Via
core's gateway the header is set from the token and cannot be forged by the client. Nothing
in this pass restricts pod ingress to core.

## Options considered (2 is what was built; 1 is still worthwhile as defense in depth)

1. **NetworkPolicy** allowing ingress to the module only from booth-core. Deploy-level, no
   code change, but needs core's namespace/labels and a CNI that enforces policy.
2. **Verify the role from the token in the module**: read the configurable groups claim
   (`^/workspaces/([a-z0-9-]+)/(owner|editor|viewer)$`, ADR 0025) from the already-verified
   token and require the header's role to be no stronger than what the token grants for that
   workspace. ~60 lines plus config for the claim name; makes authorization independent of
   network trust. Best done fleet-wide (every module), so it is probably an ADR.

Recommendation: adopt option 2 fleet-wide (an ADR amending 0025) and add option 1 as a second
layer. Note the residual: the module still trusts the *token's* claims once verified, which is
the platform's identity model working as intended; what it no longer trusts is an unsigned header.
