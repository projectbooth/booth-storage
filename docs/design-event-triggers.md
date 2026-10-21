# Publishing `storage.object.created` (ADR 0123): design note

Status: proposal, 2026-10-21. No code. Checked against this module's `main` (`b02da3a`),
`booth-architecture`'s ADR 0123, 0007, 0026, 0046, 0065, 0117, 0120, and
`agent-briefs/adr-0123-prompts.md`'s questions for `booth-storage`.

## 1. Where to publish

There is exactly one HTTP route that writes object content: `PUT /api/backends/{id}/objects/*`
(`handleWriteObject`, `internal/api/server.go:464-552`). Every caller funnels through it today —
the admin UI's Storage-page upload button, a direct scripted API call from an editor/owner token,
and an app's create-only grant-based upload (ADR 0117) — there is no second write path to miss.
The publish call belongs right after `b.Write` returns successfully (`server.go:530`, before
`writeJSON` at line 551), inside the same success branch that already logs the app-upload audit
line. A `412`/`413`/`415`/`403` or any backend error returns before that point today and would
simply skip the new call — no new branching needed.

Two writes never reach this handler at all, so they are excluded by construction, not by a path
check: the create-only capability probe (`internal/registry/probe.go:34-37`, key prefix
`.booth-create-only-probe-`) and any `Backend.Mkdir` placeholder a kind might write internally.
Both call `backend.Write` directly on the `Backend` interface, never through
`handleWriteObject`. That answers question 4 (which paths are internal): there are none left to
special-case inside the handler — "internal" here already means "doesn't go through this route."

## 2. What's known, and the "replace with no change" question

At the publish point: `backendID` (URL param), `clean` (the normalized path), `info.Size` (from
the returned `ObjectInfo`), and `id.Subject` (the caller's token subject — a person's OIDC `sub`
for an editor/owner write, `streamlit:<workspace>:<appId>` for an app's create-only upload; no
shape assumption either way, consistent with this module's ADR 0120 gate-2 note). The ADR's fixed
payload also wants `via`: this handler can tell "app upload" (`grant != nil`) from "editor/owner
write" (`grant == nil`), but not UI-button from a scripted API call with the same token — both
carry an identical identity and HTTP shape, and there's no honest way to tell them apart server
side. Recommend `via: "app-upload"` or `via: "editor"`; flagged as open question 1 rather than
guessing the user wants finer granularity.

"New" vs. "replaced" isn't actually in the ADR's payload shape (`{backendId, path, size,
principal, via}`), so nothing to add there. The part worth answering is the ADR's "not published
for overwrites that change nothing": that requires knowing the old content's hash/ETag before the
new write lands, which means a `Stat` before every write — a real extra round trip on every call,
not a cheap check, and the opposite of ADR 0117's "never check-then-write" instinct for this same
code path. Given ADR 0123 point 1 already requires consumers to tolerate duplicates and
reordering, and a same-content rewrite is rare in practice (most overwrites are a genuinely new
version), I'd skip this optimization entirely in v0: publish on every successful write this
handler makes, full stop. Simpler, and the cost of occasionally triggering a no-op pipeline run is
exactly the cost the ADR already accepted for at-least-once delivery generally.

## 3. Publishing today: nothing exists yet

This module has no bus client of any kind — no NATS import anywhere in the tree. Building this
needs: a JetStream publisher (connect once at startup, matching the lifecycle of the Postgres pool
in `cmd/storage/main.go`), and the fixed envelope from ADR 0026 (`workspace`, `eventType:
"storage.object.created"`, `publishedAt`, `publishedBy: "storage"`, `data`). Subject:
`booth.<id.Workspace>.storage.object.created`.

If the bus is down or the publish call errors at commit time: the object write has already
succeeded and the client has already been told so (or is about to be) — nothing about the write
itself should wait on or roll back for the event. Recommend a short bounded timeout (on the order
of 1-2s) on the publish call, made synchronously before `writeJSON` so a successful publish can't
race the handler's own return, but on timeout or error: log and continue, same severity as this
module's existing `audit:` lines. No outbox, no retry queue — ADR 0123 point 6 already gives the
platform a safety net (a slow interval/cron trigger) specifically for the case a trigger event is
missed, so building persistence here to avoid that same rare case would duplicate a mitigation the
ADR already decided belongs at the pipeline layer, not the publisher. Open question 2 if the user
wants stronger delivery guarantees than that.

## 4. Volume

A 74-file upload burst is 74 ordinary `PUT` requests already arriving one at a time (or with
whatever concurrency the client uses) — nothing here batches them, so it's 74 ordinary publish
calls, each cheap. ADR 0117 point 8a already deferred any per-principal write rate limit as
unneeded until there's a real abuse case; I see no reason to add one now just because writes also
publish. No new limit recommended.

## 5. Test plan

- **Unit**: a successful plain write and a successful create-only write each produce exactly one
  publish call with the right subject and envelope fields (including the right `via`); a `412`
  precondition failure, a `403`, and a backend write error each produce zero publish calls; a
  publish-side error (simulate via a fake publisher) doesn't change the HTTP response or status
  code.
- **Payload shape**: `backendId`/`path`/`size`/`principal`/`via` match an app-upload request and
  an editor-write request, field for field, against the ADR's shape.
- **Real bus** (once a NATS/JetStream test emulator is wired into `hack/docker-compose.emulators.yml`,
  matching this module's existing `BOOTH_TEST_*_ENDPOINT` pattern for MinIO/Azurite/SeaweedFS/Postgres):
  a published event is actually readable back off `booth.<ws>.storage.object.created` with a real
  JetStream consumer.

## What I need

**From `booth-core`**: whether the bus enforces per-module publish permissions today, and if so,
that `storage` is granted publish on `booth.*.storage.object.created` before this ships — a
publish this module makes without that permission should behave exactly like "bus down" (log,
continue), not surface as a write failure to the caller. Also: confirmation that `publishedBy:
"storage"` is this module's correct manifest id (matching `"superset"`/`"streamlit"`'s precedent
in ADR 0026/0046).

**From `booth-pipeline`**: sign off on the `via` values (§2) and confirm "editor" is granular
enough for the finance-dev use case's inbox prefix (ADR 0123's own example watches an app-upload
path, so `via: "app-upload"` is likely the only value that use case needs).

## Open questions for the user

1. Is `via: "app-upload"` / `via: "editor"` (§2) granular enough, given this handler can't tell a
   UI button click from a scripted API call using the same editor token?
2. Best-effort publish with log-and-continue on bus failure, relying on ADR 0123 point 6's safety
   net instead of an outbox (§3) — acceptable, or does this module need stronger delivery
   guarantees for this one event type specifically?
