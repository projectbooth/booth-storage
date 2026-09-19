import { useEffect, useState } from "react";
import { fetchBackends, type ApiContext } from "../api/client";
import { ObjectBrowser } from "../components/ObjectBrowser";
import { Banner, Button, KindBadge, PageHeader } from "../components/ui";
import { useLoad } from "../hooks";
import type { WorkspaceRole } from "../types";

/** The regular view (ADR 0036/0037/0038): anyone with a workspace role sees which backends
 *  are registered, picks one, and browses it as a file browser. Editors and owners can also
 *  create folders, upload, rename and delete there; viewers are read-only. Registering,
 *  editing and removing backends and credentials lives entirely in the separate admin view. */
export function BrowseView({
  ctx,
  role,
  onOpenAdmin,
}: {
  ctx: ApiContext;
  role: WorkspaceRole;
  /** Navigates to the admin view. Only offered to owners, since only they can use it. */
  onOpenAdmin: () => void;
}) {
  const { state } = useLoad(() => fetchBackends(ctx), [ctx.workspace]);
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const backends = state.status === "ready" ? state.data : [];

  // Default to the first backend so a single-backend workspace lands straight on its
  // contents, and re-pick if the selected one disappears (removed, or workspace switch).
  useEffect(() => {
    if (state.status !== "ready") return;
    if (!state.data.some((b) => b.id === selectedId)) {
      setSelectedId(state.data[0]?.id ?? null);
    }
  }, [state, selectedId]);

  const selected = backends.find((b) => b.id === selectedId) ?? null;

  return (
    <div className="flex flex-col gap-4 p-6">
      <PageHeader
        title="Storage"
        subtitle="Browse the storage backends registered in this workspace."
        actions={role === "owner" ? <Button onClick={onOpenAdmin}>Manage backends</Button> : undefined}
      />

      {state.status === "loading" && <p className="text-sm text-slate-500 dark:text-slate-400">Loading backends…</p>}
      {state.status === "error" && <Banner tone="error">Couldn't load storage backends: {state.error}</Banner>}

      {state.status === "ready" && backends.length === 0 && (
        <div className="rounded-lg border border-dashed border-slate-300 p-8 text-center dark:border-slate-700">
          <p className="text-sm font-medium text-slate-700 dark:text-slate-200">No storage backends registered yet</p>
          <p className="mt-1 text-sm text-slate-500 dark:text-slate-400">
            {role === "owner"
              ? "Register an S3-compatible store, a filesystem directory, Azure Blob Storage or Google Cloud Storage to get started."
              : "A workspace owner needs to register one before there's anything to browse."}
          </p>
          {role === "owner" && (
            <Button variant="primary" className="mt-4" onClick={onOpenAdmin}>
              Add a backend
            </Button>
          )}
        </div>
      )}

      {state.status === "ready" && backends.length > 0 && (
        <div className="grid gap-4 md:grid-cols-[16rem_minmax(0,1fr)]">
          <ul aria-label="Storage backends" className="flex flex-col gap-2">
            {backends.map((b) => {
              const active = b.id === selectedId;
              return (
                <li key={b.id}>
                  <button
                    type="button"
                    aria-pressed={active}
                    onClick={() => setSelectedId(b.id)}
                    className={`flex w-full flex-col gap-1 rounded-lg border p-3 text-left ${
                      active
                        ? "border-indigo-500 bg-indigo-50 dark:border-indigo-400 dark:bg-indigo-950"
                        : "border-slate-200 bg-white hover:border-slate-300 dark:border-slate-800 dark:bg-slate-900 dark:hover:border-slate-700"
                    }`}
                  >
                    <span className="flex items-center justify-between gap-2">
                      <span className="truncate text-sm font-semibold text-slate-900 dark:text-slate-100">{b.displayName}</span>
                      <KindBadge kind={b.kind} />
                    </span>
                    <span className="truncate font-mono text-xs text-slate-500 dark:text-slate-400" title={b.location}>
                      {b.location}
                    </span>
                    <span className="text-xs text-slate-400 dark:text-slate-500">id: {b.id}</span>
                  </button>
                </li>
              );
            })}
          </ul>

          <section aria-label="Contents" className="min-w-0">
            {selected && <ObjectBrowser key={selected.id} ctx={ctx} backend={selected} canWrite={role === "owner" || role === "editor"} />}
          </section>
        </div>
      )}
    </div>
  );
}
