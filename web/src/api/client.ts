import type {
  AdminBackend,
  BackendSummary,
  CreateBackendRequest,
  KindsResponse,
  ListResult,
  TestResult,
  UpdateBackendRequest,
} from "../types";

// This component is mounted by booth-design's shell, so its requests resolve against the
// shell's origin and must go through booth-core's gateway at /modules/{id}/* — which
// strips the /modules/storage prefix before forwarding to this repo's own backend routes
// (/api/...). A bare "/api/..." here would hit booth-core's own API instead (the exact
// mistake booth-module-store's first published package made). The dev harness's Vite
// proxy (vite.config.ts) mimics the same prefix-stripping so identical paths work
// standalone.
const BASE = "/modules/storage/api";

export type GetAccessToken = () => string | null;

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    /** The request field a validation error is about, when the server named one. */
    public field?: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

/** Everything a request needs from the mounting shell (ADR 0031/0033). */
export interface ApiContext {
  workspace: string;
  getAccessToken: GetAccessToken;
}

// workspace sets the X-Workspace header booth-core's gateway requires on every
// authenticated request (ADR 0025) to resolve/forward X-Booth-Workspace downstream.
//
// getAccessToken is called fresh immediately before each request, never cached —
// booth-design's token can be silently renewed at any time (ADR 0032), and a value
// captured earlier can go stale with no guarantee a re-render would refresh it. A null
// return (not-yet-authenticated, logged out) omits the Authorization header rather than
// sending the literal string "null" (ADR 0033). booth-core has no cookie/session support,
// so there is no credentials mode to fall back on.
function buildHeaders(ctx: ApiContext, init?: RequestInit): Headers {
  const headers = new Headers(init?.headers);
  headers.set("X-Workspace", ctx.workspace);
  const token = ctx.getAccessToken();
  if (token !== null) {
    headers.set("Authorization", `Bearer ${token}`);
  }
  return headers;
}

async function toApiError(res: Response): Promise<ApiError> {
  const text = await res.text();
  try {
    const body = JSON.parse(text) as { error?: string; field?: string };
    if (body.error) return new ApiError(res.status, body.error, body.field);
  } catch {
    // not JSON — e.g. a gateway error page; fall through to the raw text
  }
  return new ApiError(res.status, text || res.statusText || `HTTP ${res.status}`);
}

async function request<T>(ctx: ApiContext, path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(BASE + path, { ...init, headers: buildHeaders(ctx, init) });
  if (!res.ok) throw await toApiError(res);
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

function jsonInit(method: string, body?: unknown): RequestInit {
  return {
    method,
    headers: { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  };
}

/** Encodes an object path for a URL, one segment at a time so "/" separators survive. */
export function encodeObjectPath(path: string): string {
  return path.split("/").map(encodeURIComponent).join("/");
}

// ---- regular view ----------------------------------------------------------

export const fetchKinds = (ctx: ApiContext) => request<KindsResponse>(ctx, "/kinds");

export const fetchBackends = (ctx: ApiContext) => request<BackendSummary[]>(ctx, "/backends");

export function listObjects(
  ctx: ApiContext,
  backendId: string,
  opts: { prefix?: string; cursor?: string; limit?: number } = {},
): Promise<ListResult> {
  const q = new URLSearchParams();
  if (opts.prefix) q.set("prefix", opts.prefix);
  if (opts.cursor) q.set("cursor", opts.cursor);
  if (opts.limit) q.set("limit", String(opts.limit));
  const qs = q.toString();
  return request<ListResult>(ctx, `/backends/${encodeURIComponent(backendId)}/objects${qs ? `?${qs}` : ""}`);
}

/** Downloads an object into memory as a Blob. The bearer token has to travel in a
 *  header, so a plain <a href> can't be used — that would be an unauthenticated
 *  navigation. */
export async function downloadObject(ctx: ApiContext, backendId: string, path: string): Promise<Blob> {
  const res = await fetch(`${BASE}/backends/${encodeURIComponent(backendId)}/objects/${encodeObjectPath(path)}`, {
    headers: buildHeaders(ctx),
  });
  if (!res.ok) throw await toApiError(res);
  return res.blob();
}

// ---- file-browser writes (ADR 0038) — editor and owner only; the server enforces it ----

const folderPath = (p: string) => p.replace(/\/+$/, "");

export const createFolder = (ctx: ApiContext, backendId: string, path: string) =>
  request<{ path: string }>(ctx, `/backends/${encodeURIComponent(backendId)}/folders`, jsonInit("POST", { path: folderPath(path) }));

/** Uploads one file, replacing any object already at `path`. The File is sent as the
 *  request body directly, so the browser streams it from disk instead of reading it all
 *  into memory. */
export function uploadObject(ctx: ApiContext, backendId: string, path: string, file: File): Promise<unknown> {
  return request<unknown>(ctx, `/backends/${encodeURIComponent(backendId)}/objects/${encodeObjectPath(path)}`, {
    method: "PUT",
    headers: { "Content-Type": file.type || "application/octet-stream" },
    body: file,
  });
}

export const deleteObject = (ctx: ApiContext, backendId: string, path: string) =>
  request<void>(ctx, `/backends/${encodeURIComponent(backendId)}/objects/${encodeObjectPath(path)}`, { method: "DELETE" });

/** Deletes a folder and everything inside it; resolves with how many objects were removed. */
export const deleteFolder = (ctx: ApiContext, backendId: string, path: string) =>
  request<{ deleted: number }>(ctx, `/backends/${encodeURIComponent(backendId)}/folders/${encodeObjectPath(folderPath(path))}`, { method: "DELETE" });

/** Renames/moves a file or folder. Never overwrites: an existing destination is a 409. */
export const moveEntry = (ctx: ApiContext, backendId: string, from: string, to: string, folder: boolean) =>
  request<unknown>(ctx, `/backends/${encodeURIComponent(backendId)}/move`, jsonInit("POST", { from: folderPath(from), to: folderPath(to), folder }));

// ---- admin view ------------------------------------------------------------

export const fetchAdminBackends = (ctx: ApiContext) => request<AdminBackend[]>(ctx, "/admin/backends");

export const createBackend = (ctx: ApiContext, body: CreateBackendRequest) =>
  request<AdminBackend>(ctx, "/admin/backends", jsonInit("POST", body));

export const updateBackend = (ctx: ApiContext, id: string, body: UpdateBackendRequest) =>
  request<AdminBackend>(ctx, `/admin/backends/${encodeURIComponent(id)}`, jsonInit("PUT", body));

export const deleteBackend = (ctx: ApiContext, id: string) =>
  request<void>(ctx, `/admin/backends/${encodeURIComponent(id)}`, { method: "DELETE" });

/** Tests a not-yet-saved backend. A failed connection resolves as {ok: false}; only a
 *  malformed request rejects. */
export const testNewBackend = (ctx: ApiContext, body: CreateBackendRequest) =>
  request<TestResult>(ctx, "/admin/test-connection", jsonInit("POST", body));

/** Tests a saved backend, optionally previewing an edit without saving it. */
export const testSavedBackend = (ctx: ApiContext, id: string, preview?: UpdateBackendRequest) =>
  request<TestResult>(ctx, `/admin/backends/${encodeURIComponent(id)}/test`, jsonInit("POST", preview));
