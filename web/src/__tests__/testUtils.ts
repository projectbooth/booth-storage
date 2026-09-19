import { vi } from "vitest";

export interface RecordedRequest {
  method: string;
  url: string;
  headers: Headers;
  body: unknown;
}

type Handler = (req: RecordedRequest) => { status?: number; json?: unknown; text?: string; blob?: Blob } | Promise<{ status?: number; json?: unknown; text?: string; blob?: Blob }>;

/** Routes are matched on "METHOD /path" where /path is everything after the API base
 *  (the query string included), or a RegExp tested against the same string. */
export type Routes = Array<[string | RegExp, Handler]>;

export const API_BASE = "/modules/storage/api";

/** Stubs global fetch with a tiny router and records every request. Unmatched requests
 *  fail the test loudly — a stray call is a bug, not something to silently 404. */
export function mockFetch(routes: Routes) {
  const requests: RecordedRequest[] = [];

  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = (init?.method ?? "GET").toUpperCase();
    let body: unknown = undefined;
    if (typeof init?.body === "string") {
      try {
        body = JSON.parse(init.body);
      } catch {
        body = init.body;
      }
    }
    const req: RecordedRequest = { method, url, headers: new Headers(init?.headers), body };
    requests.push(req);

    const rest = url.startsWith(API_BASE) ? url.slice(API_BASE.length) : url;
    const key = `${method} ${rest}`;
    for (const [pattern, handler] of routes) {
      const hit = typeof pattern === "string" ? pattern === key : pattern.test(key);
      if (!hit) continue;
      const out = await handler(req);
      const status = out.status ?? 200;
      if (out.json !== undefined) {
        return new Response(JSON.stringify(out.json), { status, headers: { "Content-Type": "application/json" } });
      }
      return new Response(out.text ?? null, { status });
    }
    throw new Error(`unexpected request: ${key}`);
  });

  vi.stubGlobal("fetch", fn);
  return { fn, requests, calls: (method: string, pathRe: RegExp) => requests.filter((r) => r.method === method && pathRe.test(r.url)) };
}

export const ok = (json: unknown) => ({ json });
export const fail = (status: number, error: string, field?: string) => ({ status, json: field ? { error, field } : { error } });

export const backendFixture = (over: Record<string, unknown> = {}) => ({
  id: "lake",
  displayName: "Data lake",
  kind: "s3",
  location: "s3://acme-data",
  createdAt: "2026-09-01T00:00:00Z",
  updatedAt: "2026-09-01T00:00:00Z",
  ...over,
});

export const adminFixture = (over: Record<string, unknown> = {}) => ({
  ...backendFixture(),
  config: { bucket: "acme-data", endpoint: "http://minio:9000", pathStyle: true },
  credentialsSet: true,
  createdBy: "alice",
  ...over,
});
