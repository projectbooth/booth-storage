import { useState } from "react";
import { deleteBackend, fetchAdminBackends, fetchKinds, testSavedBackend, type ApiContext } from "../api/client";
import { BackendForm } from "../components/BackendForm";
import { Banner, Button, KindBadge, PageHeader } from "../components/ui";
import { errorMessage, useLoad } from "../hooks";
import type { AdminBackend, TestResult, WorkspaceRole } from "../types";

type Mode = { kind: "list" } | { kind: "create" } | { kind: "edit"; backend: AdminBackend };

/** The admin view (ADR 0036): register, edit, test and remove backends and their
 *  credentials. Reached at the manifest's adminNavPath.
 *
 *  Owner-only. The role check here is a UX courtesy so a viewer who lands on the URL sees
 *  an explanation instead of a wall of 403s — the real enforcement is server-side on
 *  every /api/admin route, whatever this component renders (ADR 0023). */
export function AdminView({ ctx, role, onOpenBrowse }: { ctx: ApiContext; role: WorkspaceRole; onOpenBrowse: () => void }) {
  if (role !== "owner") {
    return (
      <div className="flex flex-col gap-4 p-6">
        <PageHeader title="Manage storage backends" />
        <Banner tone="info">Only workspace owners can register, edit or remove storage backends and their credentials.</Banner>
        <div>
          <Button onClick={onOpenBrowse}>Back to storage</Button>
        </div>
      </div>
    );
  }
  return <OwnerAdmin ctx={ctx} onOpenBrowse={onOpenBrowse} />;
}

function OwnerAdmin({ ctx, onOpenBrowse }: { ctx: ApiContext; onOpenBrowse: () => void }) {
  const backends = useLoad(() => fetchAdminBackends(ctx), [ctx.workspace]);
  const kinds = useLoad(() => fetchKinds(ctx), [ctx.workspace]);

  const [mode, setMode] = useState<Mode>({ kind: "list" });
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [notice, setNotice] = useState<{ tone: "error" | "success"; text: string } | null>(null);
  // Per-row connection-test outcomes, keyed by backend id.
  const [tests, setTests] = useState<Record<string, TestResult | "pending">>({});

  const list = backends.state.status === "ready" ? backends.state.data : [];
  const availableKinds = kinds.state.status === "ready" ? kinds.state.data.kinds : ["s3", "azure", "gcs"] as const;

  async function runTest(b: AdminBackend) {
    setTests((t) => ({ ...t, [b.id]: "pending" }));
    try {
      const result = await testSavedBackend(ctx, b.id);
      setTests((t) => ({ ...t, [b.id]: result }));
    } catch (err) {
      setTests((t) => ({ ...t, [b.id]: { ok: false, error: errorMessage(err) } }));
    }
  }

  async function remove(id: string) {
    setDeleting(true);
    setNotice(null);
    try {
      await deleteBackend(ctx, id);
      setConfirmDelete(null);
      setNotice({ tone: "success", text: `Removed "${id}". Any data in the underlying store is untouched.` });
      backends.reload();
    } catch (err) {
      setNotice({ tone: "error", text: `Couldn't remove "${id}": ${errorMessage(err)}` });
    } finally {
      setDeleting(false);
    }
  }

  return (
    <div className="flex flex-col gap-4 p-6">
      <PageHeader
        title="Manage storage backends"
        subtitle="Register the storage this workspace can use, and the credentials to reach it. Every registered backend stays available side by side."
        actions={
          <>
            <Button onClick={onOpenBrowse}>Back to storage</Button>
            {mode.kind === "list" && (
              <Button variant="primary" onClick={() => { setNotice(null); setMode({ kind: "create" }); }}>
                Add backend
              </Button>
            )}
          </>
        }
      />

      {notice && <Banner tone={notice.tone}>{notice.text}</Banner>}
      {kinds.state.status === "error" && <Banner tone="error">Couldn't determine which backend kinds are available: {kinds.state.error}</Banner>}

      {mode.kind === "create" && (
        <BackendForm
          ctx={ctx}
          kinds={[...availableKinds]}
          onCancel={() => setMode({ kind: "list" })}
          onSaved={(saved) => {
            setMode({ kind: "list" });
            setNotice({ tone: "success", text: `Registered "${saved.displayName}".` });
            backends.reload();
          }}
        />
      )}

      {mode.kind === "edit" && (
        <BackendForm
          key={mode.backend.id}
          ctx={ctx}
          existing={mode.backend}
          kinds={[mode.backend.kind]}
          onCancel={() => setMode({ kind: "list" })}
          onSaved={(saved) => {
            setMode({ kind: "list" });
            setNotice({ tone: "success", text: `Saved "${saved.displayName}".` });
            setTests((t) => {
              const { [saved.id]: _stale, ...rest } = t; // a saved edit invalidates its last test
              void _stale;
              return rest;
            });
            backends.reload();
          }}
        />
      )}

      {backends.state.status === "loading" && <p className="text-sm text-slate-500 dark:text-slate-400">Loading backends…</p>}
      {backends.state.status === "error" && <Banner tone="error">Couldn't load storage backends: {backends.state.error}</Banner>}

      {backends.state.status === "ready" && list.length === 0 && mode.kind === "list" && (
        <div className="rounded-lg border border-dashed border-slate-300 p-8 text-center dark:border-slate-700">
          <p className="text-sm font-medium text-slate-700 dark:text-slate-200">No storage backends yet</p>
          <p className="mt-1 text-sm text-slate-500 dark:text-slate-400">Add one to make it available to this workspace.</p>
        </div>
      )}

      {list.length > 0 && (
        <div className="overflow-x-auto rounded-lg border border-slate-200 dark:border-slate-800">
          <table className="w-full text-left text-sm">
            <thead className="bg-slate-50 text-xs uppercase tracking-wide text-slate-500 dark:bg-slate-900 dark:text-slate-400">
              <tr>
                <th scope="col" className="px-3 py-2 font-medium">Backend</th>
                <th scope="col" className="px-3 py-2 font-medium">Kind</th>
                <th scope="col" className="hidden px-3 py-2 font-medium md:table-cell">Location</th>
                <th scope="col" className="px-3 py-2 font-medium">Credentials</th>
                <th scope="col" className="px-3 py-2"><span className="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-100 dark:divide-slate-800">
              {list.map((b) => {
                const test = tests[b.id];
                return (
                  <tr key={b.id} className="bg-white align-top dark:bg-slate-950">
                    <td className="px-3 py-2">
                      <div className="font-medium text-slate-900 dark:text-slate-100">{b.displayName}</div>
                      <div className="font-mono text-xs text-slate-500 dark:text-slate-400">{b.id}</div>
                      {test && test !== "pending" && (
                        <div role="status" className={`mt-1 text-xs ${test.ok ? "text-emerald-700 dark:text-emerald-400" : "text-red-600 dark:text-red-400"}`}>
                          {test.ok ? "Connection OK" : `Connection failed: ${test.error}`}
                        </div>
                      )}
                    </td>
                    <td className="px-3 py-2"><KindBadge kind={b.kind} /></td>
                    <td className="hidden max-w-xs truncate px-3 py-2 font-mono text-xs text-slate-500 md:table-cell dark:text-slate-400" title={b.location}>
                      {b.location}
                    </td>
                    <td className="px-3 py-2 text-slate-600 dark:text-slate-300">{b.credentialsSet ? "Set" : "None"}</td>
                    <td className="px-3 py-2">
                      {confirmDelete === b.id ? (
                        <div className="flex flex-col items-end gap-2" role="group" aria-label={`Confirm removing ${b.displayName}`}>
                          <p className="max-w-xs text-right text-xs text-slate-600 dark:text-slate-300">
                            Remove <strong>{b.displayName}</strong>? This unregisters it and deletes its stored credentials. Data in the underlying store is not touched.
                          </p>
                          <div className="flex gap-2">
                            <Button variant="danger" onClick={() => remove(b.id)} disabled={deleting}>
                              {deleting ? "Removing…" : "Remove"}
                            </Button>
                            <Button onClick={() => setConfirmDelete(null)} disabled={deleting}>Cancel</Button>
                          </div>
                        </div>
                      ) : (
                        <div className="flex flex-wrap justify-end gap-2">
                          <Button onClick={() => runTest(b)} disabled={test === "pending"} aria-label={`Test ${b.displayName}`}>
                            {test === "pending" ? "Testing…" : "Test"}
                          </Button>
                          <Button onClick={() => { setNotice(null); setMode({ kind: "edit", backend: b }); }} aria-label={`Edit ${b.displayName}`}>
                            Edit
                          </Button>
                          <Button variant="danger" onClick={() => setConfirmDelete(b.id)} aria-label={`Remove ${b.displayName}`}>
                            Remove
                          </Button>
                        </div>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
