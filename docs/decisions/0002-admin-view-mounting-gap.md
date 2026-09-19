# 0002: The shell gives a native module no way to know which of its two views to show

Status: **routed to booth-design** — the coordinator ruled this a `booth-design`-side gap, not
this repo's to fix. The workaround below stays in place until the shell passes a route/view prop
and renders a nav entry for `adminNavPath`.

## Context

ADR 0036 requires two views — a regular browse/select view and a distinct admin view — and
says "`booth-design`'s shell already knows how to route to an admin path — no shell-side
changes needed." Reading `booth-design` (`src/pages/ModuleRoutePage.tsx`,
`src/components/shell/NativeModulePane.tsx`, `src/lib/nativeModules.ts`) shows that is only
half true:

1. **Route matching works.** `findModuleForPath` matches both `navPath` and `adminNavPath`
   to the same module.
2. **But the module can't tell which one matched.** `NativeModulePane` mounts the single
   component registered for the module id and passes only the four props of
   `NativeModuleProps` (`workspace`, `role`, `theme`, `getAccessToken` — ADR 0031/0033).
   There is no route, path, or "view" prop. Registration is one component per module id.
3. **Nothing links to `adminNavPath`.** A grep of `booth-design`'s nav components finds no
   rendering of `adminNavPath`, and no role-gated entry for it. The route resolves if typed
   into the address bar, but no user would find it.

(`core.md`'s brief says "the role-gating logic is core's to expose" for admin routing; nothing
in core's `/api/me`/module listing is used by the shell for this today.)

## What this repo does

`StorageApp` (`web/src/StorageApp.tsx`) is one component that picks its view itself:

- an explicit `view` prop wins if a future shell passes one;
- otherwise it reads `window.location.pathname` — the shell is a browser-router SPA, so the
  address bar is always right — and shows the admin view for `adminPath` (default
  `/storage/admin`, matching the manifest) or anything nested under it;
- it re-renders on back/forward and on the shell's own re-render (`useSyncExternalStore`);
- it provides its own in-view links between the two (an owner-only "Manage backends" button
  in the regular view, "Back to storage" in the admin view), navigating with `pushState` plus
  a synthetic `popstate`, which a browser router picks up **without a page reload** (a reload
  would drop the shell's in-memory token, ADR 0032). A shell-supplied `onNavigate` is used
  instead if given.
- `StorageBrowseApp` / `StorageAdminApp` are also exported, for a shell that registers the
  two separately.

The admin view also refuses non-owners with an explanation, but that is UX only: **every
`/api/admin/*` route enforces owner-only server-side** (tested), whatever any UI does.

## What's still needed from elsewhere (not done here)

- `booth-design` should render `adminNavPath` as a nav entry visible only to owners, so the
  admin view is discoverable without the in-view button. Today the in-view "Manage backends"
  button (owners only) is the only way in.
- Cleaner long-term: extend `NativeModuleProps` with the matched route (or which of
  `navPath`/`adminNavPath` matched) so a module doesn't sniff `window.location`. That is a
  contract change (`contracts/ui-integration.md`, ADRs 0031/0033) and so an ADR, not
  something to add unilaterally.

## Consequences

- This works with today's shell unmodified, but it couples to the address bar and to the
  manifest's `adminNavPath` string (a prop, defaulted to `/storage/admin`).
- If the shell later passes an explicit view/route prop, this repo can delete the
  `window.location` reading in one file (`web/src/navigation.ts`).
