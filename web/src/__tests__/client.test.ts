import { afterEach, describe, expect, it, vi } from "vitest";
import {
  ApiError,
  createBackend,
  deleteBackend,
  downloadObject,
  encodeObjectPath,
  fetchBackends,
  listObjects,
  testSavedBackend,
  updateBackend,
} from "../api/client";
import { API_BASE, fail, mockFetch, ok } from "./testUtils";

afterEach(() => vi.unstubAllGlobals());

const ctx = (token: string | null = "tok-1", workspace = "acme") => ({ workspace, getAccessToken: () => token });

describe("request plumbing (ADR 0025 / 0033)", () => {
  it("calls booth-core's gateway prefix, not bare /api (booth-module-store's first-release bug)", async () => {
    const m = mockFetch([["GET /backends", () => ok([])]]);
    await fetchBackends(ctx());
    expect(m.requests[0].url).toBe("/modules/storage/api/backends");
    expect(API_BASE).toBe("/modules/storage/api");
  });

  it("sends the workspace header and a bearer token", async () => {
    const m = mockFetch([["GET /backends", () => ok([])]]);
    await fetchBackends(ctx("secret-token", "globex"));
    expect(m.requests[0].headers.get("X-Workspace")).toBe("globex");
    expect(m.requests[0].headers.get("Authorization")).toBe("Bearer secret-token");
  });

  it("omits Authorization entirely for a null token instead of sending 'Bearer null'", async () => {
    const m = mockFetch([["GET /backends", () => ok([])]]);
    await fetchBackends(ctx(null));
    expect(m.requests[0].headers.has("Authorization")).toBe(false);
  });

  it("reads the token fresh on every request, so a silent refresh is picked up (ADR 0033)", async () => {
    const m = mockFetch([["GET /backends", () => ok([])]]);
    let token = "old";
    const c = { workspace: "acme", getAccessToken: () => token };
    await fetchBackends(c);
    token = "refreshed";
    await fetchBackends(c);
    expect(m.requests.map((r) => r.headers.get("Authorization"))).toEqual(["Bearer old", "Bearer refreshed"]);
  });

  it("never uses cookie credentials (booth-core has no session mechanism)", async () => {
    const m = mockFetch([["GET /backends", () => ok([])]]);
    await fetchBackends(ctx());
    const init = m.fn.mock.calls[0][1] as RequestInit;
    expect(init.credentials).toBeUndefined();
  });
});

describe("error handling", () => {
  it("surfaces the server's JSON error message and offending field", async () => {
    mockFetch([["POST /admin/backends", () => fail(422, "bucket is required", "config")]]);
    const err = await createBackend(ctx(), { id: "x", kind: "s3", config: {} }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(422);
    expect((err as ApiError).message).toBe("bucket is required");
    expect((err as ApiError).field).toBe("config");
  });

  it("falls back to the raw body for a non-JSON error (e.g. a gateway page)", async () => {
    mockFetch([["GET /backends", () => ({ status: 502, text: "Bad Gateway" })]]);
    const err = await fetchBackends(ctx()).catch((e: unknown) => e);
    expect((err as ApiError).message).toBe("Bad Gateway");
    expect((err as ApiError).status).toBe(502);
  });
});

describe("object paths and listing", () => {
  it("encodes each path segment but keeps the separators", () => {
    expect(encodeObjectPath("a/b c/d+e.csv")).toBe("a/b%20c/d%2Be.csv");
    expect(encodeObjectPath("../x")).toBe("../x"); // the server rejects traversal; the client doesn't rewrite it
    expect(encodeObjectPath("é/日本.txt")).toBe("%C3%A9/%E6%97%A5%E6%9C%AC.txt");
  });

  it("builds list queries, omitting unset options", async () => {
    const m = mockFetch([[/^GET \/backends\/lake\/objects/, () => ok({ entries: [] })]]);
    await listObjects(ctx(), "lake");
    await listObjects(ctx(), "lake", { prefix: "a/b/", cursor: "tok==", limit: 50 });
    expect(m.requests[0].url).toBe("/modules/storage/api/backends/lake/objects");
    expect(m.requests[1].url).toBe("/modules/storage/api/backends/lake/objects?prefix=a%2Fb%2F&cursor=tok%3D%3D&limit=50");
  });

  it("downloads an object with auth headers (a bare link would be unauthenticated)", async () => {
    const m = mockFetch([["GET /backends/lake/objects/dir/f%20x.csv", () => ({ text: "a,b" })]]);
    const blob = await downloadObject(ctx("dl-token"), "lake", "dir/f x.csv");
    expect(blob.size).toBe(3); // "a,b" (jsdom's Blob has no .text())
    expect(m.requests[0].headers.get("Authorization")).toBe("Bearer dl-token");
    expect(m.requests[0].headers.get("X-Workspace")).toBe("acme");
  });

  it("rejects a failed download with the server's message", async () => {
    mockFetch([[/objects\/gone/, () => fail(404, "object not found")]]);
    await expect(downloadObject(ctx(), "lake", "gone.txt")).rejects.toThrow("object not found");
  });
});

describe("admin calls", () => {
  it("sends JSON bodies with the right verbs and paths", async () => {
    const m = mockFetch([
      ["POST /admin/backends", () => ok({ id: "x" })],
      ["PUT /admin/backends/x", () => ok({ id: "x" })],
      ["DELETE /admin/backends/x", () => ({ status: 204 })],
      ["POST /admin/backends/x/test", () => ok({ ok: true })],
    ]);
    await createBackend(ctx(), { id: "x", kind: "s3", config: { bucket: "b" }, credentials: { accessKeyId: "A", secretAccessKey: "B" } });
    await updateBackend(ctx(), "x", { displayName: "n" });
    await expect(deleteBackend(ctx(), "x")).resolves.toBeUndefined();
    await testSavedBackend(ctx(), "x");

    expect(m.requests[0].headers.get("Content-Type")).toBe("application/json");
    expect(m.requests[0].body).toEqual({ id: "x", kind: "s3", config: { bucket: "b" }, credentials: { accessKeyId: "A", secretAccessKey: "B" } });
    expect(m.requests[1].body).toEqual({ displayName: "n" });
    expect(m.requests[3].body).toBeUndefined(); // a bare test-saved sends no body
  });

  it("URL-encodes a backend id in the path", async () => {
    const m = mockFetch([[/DELETE/, () => ({ status: 204 })]]);
    await deleteBackend(ctx(), "a/b");
    expect(m.requests[0].url).toBe("/modules/storage/api/admin/backends/a%2Fb");
  });
});
