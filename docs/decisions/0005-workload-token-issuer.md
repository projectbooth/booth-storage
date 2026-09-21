# 0005: Trust booth-core as a second token issuer, for workload tokens

Status: **implemented** — `booth-architecture` ADR 0056; core's side is `booth-core`
decision 0010.

## What and why

`booth-pipeline` must call this API for scheduled runs with no human present. Core mints a
short-lived (10 minute) JWT for the run, and every module that should accept it trusts core's
own signing keys as a second issuer, alongside the deployment's OIDC provider. The token's
`groups` claim has exactly ADR 0025's shape (`["/workspaces/<ws>/<role>"]`), so the ADR 0041
role derivation in `internal/auth` is untouched — the only change is *which signers are
trusted*. `sub` names the run (`job:<id>`), so request logs already tell a human from a job.

## How

- **Opt-in.** `oidc.workloadIssuerUrl` (env `BOOTH_WORKLOAD_ISSUER_URL`) is booth-core's issuer
  URL — the same value as core's workload issuer, its in-cluster Service URL by default. Empty
  (the default) trusts the IdP only, exactly as before. It must differ from `oidc.issuerUrl`.
- **Keys** come from `<issuer>/.well-known/jwks.json` (`oidc.NewRemoteKeySet`), not OIDC
  discovery, so it works whether or not core's discovery document is present.
- **Lazy.** Keys are fetched on the first workload token, not at startup. The IdP is still
  discovered at startup (unchanged), but core being down or not yet installed must not stop
  this module serving people. Until core is reachable, workload tokens are refused with 401.
- **Same checks.** Audience (core sets `aud` to the OIDC client ID; `oidc.requireAudience`
  applies to both), expiry, signature and the groups claim name are handled identically for
  both issuers. `oidc.groupsClaim` must match what core puts in workload tokens (it uses the
  deployment's configured claim name, so this is already the case if it matches core).
- **Routing.** `Verifier.Verify` reads the token's *unverified* `iss` only to pick a verifier;
  that verifier then re-checks issuer, signature, expiry and audience. A token naming one
  issuer but signed by the other's key fails, and neither issuer's keys can vouch for the other
  (both directions are tested).

## Not done

Nothing revokes an already-issued workload token before its 10 minutes are up; that is core's
documented residual limit (its decision 0010), not something this module can improve.
