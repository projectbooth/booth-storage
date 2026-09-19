import { useMemo } from "react";
import type { ApiContext, GetAccessToken } from "./api/client";
import {
  DEFAULT_ADMIN_PATH,
  DEFAULT_BROWSE_PATH,
  defaultNavigate,
  isAdminPath,
  usePathname,
  type ViewName,
} from "./navigation";
import type { WorkspaceRole } from "./types";
import { AdminView } from "./views/AdminView";
import { BrowseView } from "./views/BrowseView";

/**
 * Props contract agreed with booth-design and pinned into contracts/ui-integration.md by
 * ADR 0031 (workspace/role/theme) and ADR 0033 (getAccessToken): plain React props, not
 * shared context, so this package never depends on anything booth-design exports (that
 * would invert the dependency direction ADR 0030 established).
 *
 * The optional props below the contract four are this package's own, none required.
 */
export interface StorageAppProps {
  /** Active workspace slug (ADR 0025) — required for every API call this makes. */
  workspace: string;
  /** Caller's role (ADR 0025). Gates what this UI offers; booth-storage's backend
   *  independently enforces the same rules on every request (ADR 0023), so this is a UX
   *  nicety and never the security boundary. */
  role: WorkspaceRole;
  theme: "dark" | "light";
  /** Returns booth-design's current bearer token (ADR 0032), or null when not
   *  authenticated. Called fresh before every request, never cached (ADR 0033). */
  getAccessToken: GetAccessToken;

  /** Force a view. When omitted (today's shell passes no such prop) the view is chosen
   *  from the address bar: the manifest's adminNavPath shows the admin view, anything
   *  else the regular one. See docs/decisions/0002 for why. */
  view?: ViewName;
  /** The manifest's adminNavPath, used to recognise the admin route. Default "/storage/admin". */
  adminPath?: string;
  /** The manifest's navPath, used for the "back to storage" links. Default "/storage". */
  browsePath?: string;
  /** How to move between the two views. Defaults to in-place history navigation, which
   *  the shell's router picks up without a page reload; pass the shell's own navigate
   *  function if it offers one. */
  onNavigate?: (path: string) => void;
}

/**
 * The native-mode component booth-design's shell mounts for booth-storage (ADR 0030),
 * published as @projectbooth/storage-ui. One component, two views (ADR 0036): the regular
 * browse/select view every role sees, and the distinct owner-only admin view for
 * registering backends and credentials.
 */
export function StorageApp({
  workspace,
  role,
  theme,
  getAccessToken,
  view,
  adminPath = DEFAULT_ADMIN_PATH,
  browsePath = DEFAULT_BROWSE_PATH,
  onNavigate = defaultNavigate,
}: StorageAppProps) {
  const pathname = usePathname();
  const current: ViewName = view ?? (isAdminPath(pathname, adminPath) ? "admin" : "browse");

  // Stable across renders unless the workspace or token accessor actually changes, so
  // effects keyed on it don't refire spuriously.
  const ctx = useMemo<ApiContext>(() => ({ workspace, getAccessToken }), [workspace, getAccessToken]);

  return (
    <div data-theme={theme} className="text-slate-900 dark:text-slate-100">
      {current === "admin" ? (
        <AdminView ctx={ctx} role={role} onOpenBrowse={() => onNavigate(browsePath)} />
      ) : (
        <BrowseView ctx={ctx} role={role} onOpenAdmin={() => onNavigate(adminPath)} />
      )}
    </div>
  );
}

/** The regular view only, for a shell that registers the two views separately. */
export function StorageBrowseApp(props: Omit<StorageAppProps, "view">) {
  return <StorageApp {...props} view="browse" />;
}

/** The admin view only, for a shell that registers the two views separately. */
export function StorageAdminApp(props: Omit<StorageAppProps, "view">) {
  return <StorageApp {...props} view="admin" />;
}
