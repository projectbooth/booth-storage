import { useState } from "react";
import { deleteUploadGrant, fetchUploadGrants, type ApiContext } from "../api/client";
import { errorMessage, useLoad } from "../hooks";
import type { AdminBackend, UploadGrant } from "../types";
import { Banner, Button, PageHeader, formatBytes } from "./ui";
import { UploadGrantForm, type UploadGrantPrefill } from "./UploadGrantForm";

/** The "App uploads" panel (ADR 0117 "Reconciled design" item 1/8a): an owner grants,
 *  lists and revokes which app may make a create-only write into which backend prefix.
 *  This module holds the grant — not booth-streamlit, and not core — so this page, not
 *  booth-streamlit's own "Write targets" panel, is where it's actually made. */
export function UploadGrantsPanel({
  ctx,
  backends,
  prefill,
}: {
  ctx: ApiContext;
  backends: AdminBackend[];
  /** From the URL, when an owner arrived via a link from booth-streamlit (see
   *  docs/contracts/upload-grants.md's "URL prefill"); opens the form immediately. */
  prefill?: UploadGrantPrefill;
}) {
  const grants = useLoad(() => fetchUploadGrants(ctx), [ctx.workspace]);
  const [showForm, setShowForm] = useState(prefill !== undefined);
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [notice, setNotice] = useState<{ tone: "error" | "success"; text: string } | null>(null);

  const list = grants.state.status === "ready" ? grants.state.data : [];
  const backendById = new Map(backends.map((b) => [b.id, b]));

  async function remove(g: UploadGrant) {
    setDeleting(true);
    setNotice(null);
    try {
      await deleteUploadGrant(ctx, g.id);
      setConfirmDelete(null);
      setNotice({ tone: "success", text: `Revoked ${g.principal}'s access to ${g.backendId}.` });
      grants.reload();
    } catch (err) {
      setNotice({ tone: "error", text: `Couldn't revoke that grant: ${errorMessage(err)}` });
    } finally {
      setDeleting(false);
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title="App uploads"
        subtitle="Let a Streamlit app make a single create-only write into one backend folder — never an overwrite, a delete, or anywhere else."
        actions={
          !showForm && (
            <Button variant="primary" onClick={() => { setNotice(null); setShowForm(true); }}>
              Grant upload access
            </Button>
          )
        }
      />

      {notice && <Banner tone={notice.tone}>{notice.text}</Banner>}

      {showForm && (
        <UploadGrantForm
          ctx={ctx}
          backends={backends}
          prefill={prefill}
          onCancel={() => setShowForm(false)}
          onSaved={(saved) => {
            setShowForm(false);
            setNotice({ tone: "success", text: `Granted ${saved.principal} access to ${saved.backendId}.` });
            grants.reload();
          }}
        />
      )}

      {grants.state.status === "loading" && <p className="text-sm text-slate-500 dark:text-slate-400">Loading grants…</p>}
      {grants.state.status === "error" && <Banner tone="error">Couldn't load upload grants: {grants.state.error}</Banner>}

      {grants.state.status === "ready" && list.length === 0 && !showForm && (
        <div className="rounded-lg border border-dashed border-slate-300 p-8 text-center dark:border-slate-700">
          <p className="text-sm font-medium text-slate-700 dark:text-slate-200">No apps have upload access</p>
          <p className="mt-1 text-sm text-slate-500 dark:text-slate-400">Grant one a folder to write into.</p>
        </div>
      )}

      {list.length > 0 && (
        <div className="overflow-x-auto rounded-lg border border-slate-200 dark:border-slate-800">
          <table className="w-full text-left text-sm">
            <thead className="bg-slate-50 text-xs uppercase tracking-wide text-slate-500 dark:bg-slate-900 dark:text-slate-400">
              <tr>
                <th scope="col" className="px-3 py-2 font-medium">App</th>
                <th scope="col" className="px-3 py-2 font-medium">Backend</th>
                <th scope="col" className="px-3 py-2 font-medium">Folder</th>
                <th scope="col" className="hidden px-3 py-2 font-medium md:table-cell">Allowed</th>
                <th scope="col" className="hidden px-3 py-2 font-medium md:table-cell">Up to</th>
                <th scope="col" className="px-3 py-2"><span className="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-100 dark:divide-slate-800">
              {list.map((g) => (
                <tr key={g.id} className="bg-white align-top dark:bg-slate-950">
                  <td className="px-3 py-2 font-mono text-xs text-slate-700 dark:text-slate-300">{g.principal}</td>
                  <td className="px-3 py-2 text-slate-700 dark:text-slate-300">{backendById.get(g.backendId)?.displayName ?? g.backendId}</td>
                  <td className="px-3 py-2 font-mono text-xs text-slate-500 dark:text-slate-400">{g.prefix || "/"}</td>
                  <td className="hidden px-3 py-2 text-xs text-slate-500 md:table-cell dark:text-slate-400">
                    {g.extensions.map((e) => `.${e}`).join(", ")}
                  </td>
                  <td className="hidden px-3 py-2 text-xs text-slate-500 md:table-cell dark:text-slate-400">{formatBytes(g.maxBytes)}</td>
                  <td className="px-3 py-2">
                    {confirmDelete === g.id ? (
                      <div className="flex flex-col items-end gap-2" role="group" aria-label={`Confirm revoking ${g.principal}`}>
                        <p className="max-w-xs text-right text-xs text-slate-600 dark:text-slate-300">
                          Revoke <strong>{g.principal}</strong>'s access to <strong>{g.backendId}</strong>?
                        </p>
                        <div className="flex gap-2">
                          <Button variant="danger" onClick={() => remove(g)} disabled={deleting}>
                            {deleting ? "Revoking…" : "Revoke"}
                          </Button>
                          <Button onClick={() => setConfirmDelete(null)} disabled={deleting}>Cancel</Button>
                        </div>
                      </div>
                    ) : (
                      <div className="flex justify-end">
                        <Button variant="danger" onClick={() => setConfirmDelete(g.id)} aria-label={`Revoke ${g.principal}`}>
                          Revoke
                        </Button>
                      </div>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
