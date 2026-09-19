import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import {
  createFolder,
  deleteFolder,
  deleteObject,
  downloadObject,
  listObjects,
  moveEntry,
  uploadObject,
  type ApiContext,
} from "../api/client";
import { errorMessage } from "../hooks";
import type { BackendSummary, ObjectEntry } from "../types";
import { Banner, Button, formatBytes, inputClass } from "./ui";

const PAGE_SIZE = 100;

function baseName(path: string): string {
  const trimmed = path.endsWith("/") ? path.slice(0, -1) : path;
  return trimmed.slice(trimmed.lastIndexOf("/") + 1);
}

/** "a/b/c.txt" -> "a/b/", "top.txt" -> "" (the folder an entry lives in). */
function parentPrefix(path: string): string {
  const trimmed = path.endsWith("/") ? path.slice(0, -1) : path;
  const i = trimmed.lastIndexOf("/");
  return i < 0 ? "" : trimmed.slice(0, i + 1);
}

/** "a/b/" -> [{name:"a", prefix:"a/"}, {name:"b", prefix:"a/b/"}] */
export function breadcrumbs(prefix: string): { name: string; prefix: string }[] {
  const parts = prefix.split("/").filter(Boolean);
  return parts.map((name, i) => ({ name, prefix: parts.slice(0, i + 1).join("/") + "/" }));
}

/** Client-side check for a single new file/folder name (the server validates again). */
export function nameProblem(name: string): string | null {
  const n = name.trim();
  if (!n) return "Enter a name.";
  if (n.includes("/") || n.includes("\\")) return "A name can't contain slashes.";
  if (n === "." || n === "..") return `"${n}" isn't a valid name.`;
  return null;
}

interface Listing {
  entries: ObjectEntry[];
  nextCursor?: string;
}

type Upload = { name: string; state: "uploading" | "done" | "error"; error?: string };

/** The unified file browser for one registered backend (ADR 0037): navigate folders, page
 *  through large listings, download objects — and, for editors and owners, create folders,
 *  upload, rename and delete (ADR 0038).
 *
 *  `canWrite` decides whether the write controls are rendered *at all* — a viewer is shown
 *  no control they can't use. That is UX only: the server independently refuses every write
 *  from a viewer (403), so hiding a button is never what protects anything.
 *
 *  Mount with `key={backend.id}`: switching backend then starts a fresh browser at that
 *  backend's root instead of carrying over the previous backend's folder. */
export function ObjectBrowser({ ctx, backend, canWrite }: { ctx: ApiContext; backend: BackendSummary; canWrite: boolean }) {
  const [prefix, setPrefix] = useState("");
  const [listing, setListing] = useState<Listing | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  const [reloadTick, setReloadTick] = useState(0);
  const reload = useCallback(() => setReloadTick((n) => n + 1), []);

  const [actionError, setActionError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [downloading, setDownloading] = useState<string | null>(null);

  const [newFolderName, setNewFolderName] = useState<string | null>(null); // null = form closed
  const [renaming, setRenaming] = useState<{ path: string; value: string } | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [uploads, setUploads] = useState<Upload[]>([]);
  const [pendingReplace, setPendingReplace] = useState<{ files: File[]; names: string[] } | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);

  // Re-list when the backend, folder or a mutation (reloadTick) changes. `cancelled` guards
  // against a slow response for a folder the user has already left overwriting the current
  // one.
  useEffect(() => {
    let cancelled = false;
    setError(null);
    listObjects(ctx, backend.id, { prefix, limit: PAGE_SIZE }).then(
      (res) => {
        if (!cancelled) setListing({ entries: res.entries, nextCursor: res.nextCursor });
      },
      (err: unknown) => {
        if (!cancelled) setError(errorMessage(err));
      },
    );
    return () => {
      cancelled = true;
    };
    // ctx is rebuilt each render by the parent; only what it *contains* matters here, and
    // the token is read fresh at request time. Re-listing on every parent render would
    // loop, so depend on the identifying values instead.
  }, [backend.id, prefix, ctx.workspace, reloadTick]);

  function navigate(next: string) {
    setListing(null);
    setActionError(null);
    setNotice(null);
    setRenaming(null);
    setConfirmDelete(null);
    setNewFolderName(null);
    setPrefix(next);
  }

  const loadMore = useCallback(async () => {
    if (!listing?.nextCursor) return;
    setLoadingMore(true);
    try {
      const res = await listObjects(ctx, backend.id, { prefix, cursor: listing.nextCursor, limit: PAGE_SIZE });
      setListing((cur) => (cur ? { entries: [...cur.entries, ...res.entries], nextCursor: res.nextCursor } : cur));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setLoadingMore(false);
    }
  }, [ctx, backend.id, prefix, listing?.nextCursor]);

  async function download(entry: ObjectEntry) {
    setActionError(null);
    setDownloading(entry.path);
    try {
      const blob = await downloadObject(ctx, backend.id, entry.path);
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = baseName(entry.path);
      document.body.appendChild(a);
      a.click();
      a.remove();
      URL.revokeObjectURL(url);
    } catch (err) {
      setActionError(`Couldn't download ${baseName(entry.path)}: ${errorMessage(err)}`);
    } finally {
      setDownloading(null);
    }
  }

  // ---- write actions (canWrite only) ----

  /** Runs one mutation: clears stale messages, shows a failure inline, reloads on success. */
  async function mutate(run: () => Promise<string | void>, failure: string) {
    setBusy(true);
    setActionError(null);
    setNotice(null);
    try {
      const msg = await run();
      if (msg) setNotice(msg);
      reload();
      return true;
    } catch (err) {
      setActionError(`${failure}: ${errorMessage(err)}`);
      return false;
    } finally {
      setBusy(false);
    }
  }

  async function submitNewFolder(e: FormEvent) {
    e.preventDefault();
    if (newFolderName === null || nameProblem(newFolderName)) return;
    const name = newFolderName.trim();
    const ok = await mutate(async () => {
      await createFolder(ctx, backend.id, prefix + name);
      return `Created folder ${name}.`;
    }, `Couldn't create folder ${name}`);
    if (ok) setNewFolderName(null);
  }

  async function submitRename(entry: ObjectEntry, e: FormEvent) {
    e.preventDefault();
    if (!renaming || nameProblem(renaming.value)) return;
    const newName = renaming.value.trim();
    if (newName === baseName(entry.path)) {
      setRenaming(null);
      return;
    }
    const to = parentPrefix(entry.path) + newName;
    const ok = await mutate(async () => {
      await moveEntry(ctx, backend.id, entry.path, to, !!entry.isDir);
      return `Renamed ${baseName(entry.path)} to ${newName}.`;
    }, `Couldn't rename ${baseName(entry.path)}`);
    if (ok) setRenaming(null);
  }

  async function confirmAndDelete(entry: ObjectEntry) {
    const name = baseName(entry.path);
    const ok = await mutate(async () => {
      if (entry.isDir) {
        const { deleted } = await deleteFolder(ctx, backend.id, entry.path);
        return `Deleted folder ${name} (${deleted} ${deleted === 1 ? "file" : "files"}).`;
      }
      await deleteObject(ctx, backend.id, entry.path);
      return `Deleted ${name}.`;
    }, `Couldn't delete ${name}`);
    if (ok) setConfirmDelete(null);
  }

  async function runUploads(files: File[]) {
    setPendingReplace(null);
    setActionError(null);
    setNotice(null);
    setUploads(files.map((f) => ({ name: f.name, state: "uploading" as const })));
    setBusy(true);
    let failed = 0;
    // One at a time: simple, and a failure on one file never hides the state of the others.
    for (const [i, file] of files.entries()) {
      try {
        await uploadObject(ctx, backend.id, prefix + file.name, file);
        setUploads((u) => u.map((x, j) => (j === i ? { ...x, state: "done" } : x)));
      } catch (err) {
        failed++;
        setUploads((u) => u.map((x, j) => (j === i ? { ...x, state: "error", error: errorMessage(err) } : x)));
      }
    }
    setBusy(false);
    if (failed === 0) {
      setNotice(`Uploaded ${files.length} ${files.length === 1 ? "file" : "files"}.`);
      setUploads([]);
    }
    reload();
  }

  function onFilesChosen(list: FileList | null) {
    const files = list ? Array.from(list) : [];
    if (fileInput.current) fileInput.current.value = ""; // allow choosing the same file again
    if (files.length === 0) return;
    // An upload silently replaces an object with the same name, so ask first — but only
    // about names in what's loaded on screen; the server replaces regardless.
    const existing = new Set((listing?.entries ?? []).filter((e) => !e.isDir).map((e) => baseName(e.path)));
    const clashes = files.filter((f) => existing.has(f.name)).map((f) => f.name);
    if (clashes.length > 0) setPendingReplace({ files, names: clashes });
    else void runUploads(files);
  }

  const crumbs = breadcrumbs(prefix);
  // Folders first, then files, each alphabetically — object stores return them interleaved
  // by key, which is hard to scan.
  const entries = [...(listing?.entries ?? [])].sort((a, b) => Number(!!b.isDir) - Number(!!a.isDir) || a.path.localeCompare(b.path));
  const folderNameError = newFolderName !== null && newFolderName !== "" ? nameProblem(newFolderName) : null;

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <nav aria-label="Location" className="flex flex-wrap items-center gap-1 text-sm">
          <button type="button" onClick={() => navigate("")} className="rounded px-1 font-medium text-indigo-600 hover:underline dark:text-indigo-400">
            {backend.displayName}
          </button>
          {crumbs.map((c, i) => (
            <span key={c.prefix} className="flex items-center gap-1">
              <span aria-hidden="true" className="text-slate-400">/</span>
              {i === crumbs.length - 1 ? (
                <span aria-current="page" className="text-slate-700 dark:text-slate-200">{c.name}</span>
              ) : (
                <button type="button" onClick={() => navigate(c.prefix)} className="rounded px-1 text-indigo-600 hover:underline dark:text-indigo-400">
                  {c.name}
                </button>
              )}
            </span>
          ))}
        </nav>

        {canWrite && (
          <div className="flex items-center gap-2" role="toolbar" aria-label="File actions">
            <Button onClick={() => setNewFolderName("")} disabled={busy || newFolderName !== null}>New folder</Button>
            <Button onClick={() => fileInput.current?.click()} disabled={busy}>Upload</Button>
            <input ref={fileInput} type="file" multiple hidden aria-label="Upload files" onChange={(e) => onFilesChosen(e.target.files)} />
          </div>
        )}
      </div>

      {canWrite && newFolderName !== null && (
        <form onSubmit={submitNewFolder} aria-label="New folder" className="flex flex-wrap items-start gap-2 rounded-md border border-slate-200 p-3 dark:border-slate-800">
          <div className="flex flex-col gap-1">
            <label htmlFor="new-folder-name" className="text-xs font-medium text-slate-600 dark:text-slate-300">Folder name</label>
            <input
              id="new-folder-name"
              className={inputClass}
              value={newFolderName}
              autoFocus
              autoComplete="off"
              aria-invalid={folderNameError ? true : undefined}
              onChange={(e) => setNewFolderName(e.target.value)}
            />
            {folderNameError && <p role="alert" className="text-xs text-red-600 dark:text-red-400">{folderNameError}</p>}
          </div>
          <div className="flex gap-2 pt-5">
            <Button type="submit" variant="primary" disabled={busy || !newFolderName.trim() || !!folderNameError}>Create</Button>
            <Button onClick={() => setNewFolderName(null)} disabled={busy}>Cancel</Button>
          </div>
        </form>
      )}

      {pendingReplace && (
        <Banner tone="info">
          <p>
            {pendingReplace.names.length === 1 ? "A file named" : "Files named"} <strong>{pendingReplace.names.join(", ")}</strong>{" "}
            already {pendingReplace.names.length === 1 ? "exists" : "exist"} here. Uploading replaces {pendingReplace.names.length === 1 ? "it" : "them"}.
          </p>
          <div className="mt-2 flex gap-2">
            <Button variant="danger" onClick={() => void runUploads(pendingReplace.files)}>Replace</Button>
            <Button onClick={() => setPendingReplace(null)}>Cancel</Button>
          </div>
        </Banner>
      )}

      {uploads.length > 0 && (
        <ul aria-label="Uploads" className="flex flex-col gap-1 text-sm">
          {uploads.map((u, i) => (
            <li key={`${u.name}-${i}`} className={u.state === "error" ? "text-red-600 dark:text-red-400" : "text-slate-600 dark:text-slate-300"}>
              {u.name} — {u.state === "uploading" ? "uploading…" : u.state === "done" ? "uploaded" : `failed: ${u.error}`}
            </li>
          ))}
        </ul>
      )}

      {error && <Banner tone="error">Couldn't list {backend.displayName}: {error}</Banner>}
      {actionError && <Banner tone="error">{actionError}</Banner>}
      {notice && <Banner tone="success">{notice}</Banner>}
      {!error && listing === null && <p className="text-sm text-slate-500 dark:text-slate-400">Loading…</p>}

      {!error && listing !== null && entries.length === 0 && (
        <p className="rounded-md border border-dashed border-slate-300 p-6 text-center text-sm text-slate-500 dark:border-slate-700 dark:text-slate-400">
          {prefix ? "This folder is empty." : "This backend is empty."}
        </p>
      )}

      {listing !== null && entries.length > 0 && (
        <div className="overflow-x-auto rounded-lg border border-slate-200 dark:border-slate-800">
          <table className="w-full text-left text-sm">
            <thead className="bg-slate-50 text-xs uppercase tracking-wide text-slate-500 dark:bg-slate-900 dark:text-slate-400">
              <tr>
                <th scope="col" className="px-3 py-2 font-medium">Name</th>
                <th scope="col" className="px-3 py-2 text-right font-medium">Size</th>
                <th scope="col" className="hidden px-3 py-2 font-medium sm:table-cell">Modified</th>
                <th scope="col" className="px-3 py-2"><span className="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-100 dark:divide-slate-800">
              {entries.map((e) => {
                const name = baseName(e.path);
                const isRenaming = renaming?.path === e.path;
                const renameError = isRenaming && renaming.value !== name ? nameProblem(renaming.value) : null;
                return (
                  <tr key={e.path} className="bg-white align-top dark:bg-slate-950">
                    <td className="px-3 py-2">
                      {isRenaming ? (
                        <form onSubmit={(ev) => void submitRename(e, ev)} aria-label={`Rename ${name}`} className="flex flex-col gap-1">
                          <div className="flex items-center gap-2">
                            <input
                              className={inputClass}
                              aria-label={`New name for ${name}`}
                              value={renaming.value}
                              autoFocus
                              autoComplete="off"
                              aria-invalid={renameError ? true : undefined}
                              onChange={(ev) => setRenaming({ path: e.path, value: ev.target.value })}
                            />
                            <Button type="submit" variant="primary" disabled={busy || !!renameError}>Save</Button>
                            <Button onClick={() => setRenaming(null)} disabled={busy}>Cancel</Button>
                          </div>
                          {renameError && <p role="alert" className="text-xs text-red-600 dark:text-red-400">{renameError}</p>}
                        </form>
                      ) : e.isDir ? (
                        <button type="button" onClick={() => navigate(e.path)} className="font-medium text-indigo-600 hover:underline dark:text-indigo-400" aria-label={`Open folder ${name}`}>
                          {name}/
                        </button>
                      ) : (
                        <span className="text-slate-900 dark:text-slate-100">{name}</span>
                      )}
                    </td>
                    <td className="px-3 py-2 text-right tabular-nums text-slate-500 dark:text-slate-400">{e.isDir ? "" : formatBytes(e.size)}</td>
                    <td className="hidden px-3 py-2 text-slate-500 sm:table-cell dark:text-slate-400">{e.modTime ? new Date(e.modTime).toLocaleString() : ""}</td>
                    <td className="px-3 py-2">
                      {confirmDelete === e.path ? (
                        <div className="flex flex-col items-end gap-2" role="group" aria-label={`Confirm deleting ${name}`}>
                          <p className="max-w-xs text-right text-xs text-slate-600 dark:text-slate-300">
                            {e.isDir ? (
                              <>Delete folder <strong>{name}</strong> and <strong>everything inside it</strong>? This can't be undone.</>
                            ) : (
                              <>Delete <strong>{name}</strong>? This can't be undone.</>
                            )}
                          </p>
                          <div className="flex gap-2">
                            <Button variant="danger" onClick={() => void confirmAndDelete(e)} disabled={busy}>{busy ? "Deleting…" : "Delete"}</Button>
                            <Button onClick={() => setConfirmDelete(null)} disabled={busy}>Cancel</Button>
                          </div>
                        </div>
                      ) : (
                        !isRenaming && (
                          <div className="flex flex-wrap justify-end gap-2">
                            {!e.isDir && (
                              <Button onClick={() => void download(e)} disabled={downloading === e.path} aria-label={`Download ${name}`}>
                                {downloading === e.path ? "…" : "Download"}
                              </Button>
                            )}
                            {canWrite && (
                              <>
                                <Button onClick={() => { setActionError(null); setNotice(null); setRenaming({ path: e.path, value: name }); }} disabled={busy} aria-label={`Rename ${name}`}>
                                  Rename
                                </Button>
                                <Button variant="danger" onClick={() => { setActionError(null); setNotice(null); setConfirmDelete(e.path); }} disabled={busy} aria-label={`Delete ${name}`}>
                                  Delete
                                </Button>
                              </>
                            )}
                          </div>
                        )
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      {listing?.nextCursor && (
        <div>
          <Button onClick={loadMore} disabled={loadingMore}>{loadingMore ? "Loading…" : "Load more"}</Button>
        </div>
      )}
    </div>
  );
}
