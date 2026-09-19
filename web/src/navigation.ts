import { useSyncExternalStore } from "react";

// The shell (booth-design) matches BOTH a module's navPath and adminNavPath to the same
// module id and mounts the module's one registered component (src/lib/nativeModules.ts,
// ADR 0030) — but NativeModuleProps (ADR 0031/0033) carries no route or "which view"
// prop. So this package has to work out which of its two views to show itself.
//
// It reads window.location.pathname: the shell is a browser-router SPA, so the address
// bar is the one source of truth that's always right, and it needs no contract change.
// StorageApp also accepts an explicit `view` prop, which wins if a future shell passes
// one — this is a workaround for a gap, not a preference. See docs/decisions/0002.

export const DEFAULT_ADMIN_PATH = "/storage/admin";
export const DEFAULT_BROWSE_PATH = "/storage";

export type ViewName = "browse" | "admin";

/** Whether `pathname` is the admin route (exactly it, or anything nested beneath it). */
export function isAdminPath(pathname: string, adminPath: string = DEFAULT_ADMIN_PATH): boolean {
  return pathname === adminPath || pathname.startsWith(adminPath + "/");
}

const NAVIGATE_EVENT = "booth-storage:navigate";

function subscribe(onChange: () => void): () => void {
  window.addEventListener("popstate", onChange);
  window.addEventListener(NAVIGATE_EVENT, onChange);
  return () => {
    window.removeEventListener("popstate", onChange);
    window.removeEventListener(NAVIGATE_EVENT, onChange);
  };
}

/** The current pathname, re-rendering on browser back/forward and on this package's own
 *  navigations. A navigation the shell's router makes via pushState doesn't fire an event,
 *  but it re-renders the mounted component, and useSyncExternalStore re-reads the
 *  snapshot on every render — so that case is covered too. */
export function usePathname(): string {
  return useSyncExternalStore(
    subscribe,
    () => window.location.pathname,
    () => "/",
  );
}

/** Navigates to `path` inside the shell without a page reload.
 *
 *  A full reload would drop booth-design's in-memory access token (ADR 0032) and force a
 *  fresh login redirect. So: pushState, then a synthetic popstate — which is what the
 *  shell's browser router listens for to notice a location change. Used only when the
 *  shell doesn't supply its own `onNavigate`. */
export function defaultNavigate(path: string): void {
  window.history.pushState({}, "", path);
  window.dispatchEvent(new PopStateEvent("popstate"));
  window.dispatchEvent(new Event(NAVIGATE_EVENT));
}
