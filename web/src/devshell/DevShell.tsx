import { useCallback, useEffect, useRef, useState } from "react";
import { StorageApp } from "../StorageApp";
import type { WorkspaceRole } from "../types";

const ROLES: WorkspaceRole[] = ["owner", "editor", "viewer"];

// Stand-in for booth-design's real shell — local-dev scaffolding only, never shipped (the
// same idea as booth-module-store's dev harness). It exists so this package can be built
// and looked at standalone against a locally running backend, and doubles as a manual
// check of the props contract (ADR 0031/0033): workspace/role/theme/getAccessToken.
//
// The backend trusts the X-Booth-* headers only alongside a verifiable bearer token, so to
// use this against a real backend paste a token from your OIDC provider (see
// booth-architecture/local-dev). The harness also mimics the shell's routing just enough
// for the two views: it drives window.location, exactly as booth-design's router does.
export function DevShell() {
  const [theme, setTheme] = useState<"light" | "dark">(() => {
    try {
      return (localStorage.getItem("storage-dev-theme") as "light" | "dark") ?? "light";
    } catch {
      return "light";
    }
  });
  const [workspace, setWorkspace] = useState("acme");
  const [role, setRole] = useState<WorkspaceRole>("owner");
  const [tokenInput, setTokenInput] = useState("");

  // A ref, not state read directly, so getAccessToken keeps a stable identity across
  // renders while always returning whatever was last typed — the same shape as a real
  // in-memory token store (ADR 0033).
  const tokenRef = useRef(tokenInput);
  useEffect(() => {
    tokenRef.current = tokenInput;
  }, [tokenInput]);
  const getAccessToken = useCallback(() => tokenRef.current || null, []);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    try {
      localStorage.setItem("storage-dev-theme", theme);
    } catch {
      // best-effort only
    }
  }, [theme]);

  return (
    <div className="min-h-screen bg-slate-50 dark:bg-slate-950">
      <header className="flex flex-wrap items-center justify-between gap-3 border-b border-slate-200 bg-white px-6 py-3 dark:border-slate-800 dark:bg-slate-900">
        <div>
          <p className="text-xs uppercase tracking-wide text-slate-400">Dev harness — not the real shell</p>
          <h1 className="text-lg font-semibold text-slate-900 dark:text-slate-100">Storage</h1>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <label className="text-xs text-slate-500 dark:text-slate-400">
            Workspace
            <input
              type="text"
              value={workspace}
              onChange={(e) => setWorkspace(e.target.value)}
              className="ml-1.5 w-28 rounded-md border border-slate-300 px-2 py-1 text-sm dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100"
            />
          </label>
          <label className="text-xs text-slate-500 dark:text-slate-400">
            Role
            <select
              value={role}
              onChange={(e) => setRole(e.target.value as WorkspaceRole)}
              className="ml-1.5 rounded-md border border-slate-300 px-2 py-1 text-sm dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100"
            >
              {ROLES.map((r) => (
                <option key={r} value={r}>{r}</option>
              ))}
            </select>
          </label>
          <label className="text-xs text-slate-500 dark:text-slate-400">
            Access token
            <input
              type="password"
              value={tokenInput}
              onChange={(e) => setTokenInput(e.target.value)}
              placeholder="(empty = logged out)"
              className="ml-1.5 w-40 rounded-md border border-slate-300 px-2 py-1 text-sm dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100"
            />
          </label>
          <button
            type="button"
            onClick={() => setTheme((t) => (t === "light" ? "dark" : "light"))}
            className="rounded-md border border-slate-300 px-3 py-1.5 text-sm text-slate-700 hover:bg-slate-100 dark:border-slate-700 dark:text-slate-300 dark:hover:bg-slate-800"
          >
            {theme === "light" ? "Dark mode" : "Light mode"}
          </button>
        </div>
      </header>
      <main>
        <StorageApp workspace={workspace} role={role} theme={theme} getAccessToken={getAccessToken} />
      </main>
    </div>
  );
}
