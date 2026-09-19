import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { breadcrumbs } from "../components/ObjectBrowser";
import { formatBytes } from "../components/ui";
import { StorageApp } from "../StorageApp";
import type { WorkspaceRole } from "../types";
import { backendFixture, fail, mockFetch, ok, type Routes } from "./testUtils";

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

function mount(role: WorkspaceRole = "viewer", extra: Record<string, unknown> = {}) {
  return render(<StorageApp workspace="acme" role={role} theme="light" getAccessToken={() => "tok"} view="browse" {...extra} />);
}

const backends = [
  backendFixture({ id: "lake", displayName: "Data lake", kind: "s3", location: "s3://acme-data" }),
  backendFixture({ id: "scratch", displayName: "Scratch", kind: "filesystem", location: "/data/acme/scratch" }),
];

const listing = (entries: unknown[], nextCursor?: string) => ok({ entries, nextCursor });

describe("BrowseView", () => {
  it("lists every registered backend with its kind and location — no 'current backend' (ADR 0035)", async () => {
    mockFetch([
      ["GET /backends", () => ok(backends)],
      [/^GET \/backends\/lake\/objects/, () => listing([])],
    ]);
    mount();

    const list = await screen.findByRole("list", { name: "Storage backends" });
    const items = within(list).getAllByRole("listitem");
    expect(items).toHaveLength(2);
    expect(within(list).getByText("Data lake")).toBeInTheDocument();
    expect(within(list).getByText("S3-compatible")).toBeInTheDocument();
    expect(within(list).getByText("s3://acme-data")).toBeInTheDocument();
    expect(within(list).getByText("Filesystem")).toBeInTheDocument();
    expect(within(list).getByText("/data/acme/scratch")).toBeInTheDocument();
  });

  it("selects the first backend by default and lets the user switch", async () => {
    const m = mockFetch([
      ["GET /backends", () => ok(backends)],
      ["GET /backends/lake/objects?limit=100", () => listing([{ path: "from-lake.csv", size: 10 }])],
      ["GET /backends/scratch/objects?limit=100", () => listing([{ path: "from-scratch.txt", size: 5 }])],
    ]);
    const user = userEvent.setup();
    mount();

    expect(await screen.findByText("from-lake.csv")).toBeInTheDocument();
    const list = screen.getByRole("list", { name: "Storage backends" });
    expect(within(list).getByRole("button", { name: /Data lake/ })).toHaveAttribute("aria-pressed", "true");

    await user.click(within(list).getByRole("button", { name: /Scratch/ }));
    expect(await screen.findByText("from-scratch.txt")).toBeInTheDocument();
    expect(within(list).getByRole("button", { name: /Scratch/ })).toHaveAttribute("aria-pressed", "true");
    expect(screen.queryByText("from-lake.csv")).not.toBeInTheDocument();
    expect(m.calls("GET", /scratch\/objects/)).toHaveLength(1);
  });

  it("navigates into folders and back via breadcrumbs, folders listed first", async () => {
    const m = mockFetch([
      ["GET /backends", () => ok([backends[0]])],
      ["GET /backends/lake/objects?limit=100", () => listing([{ path: "zeta.txt", size: 1 }, { path: "reports/", size: 0, isDir: true }, { path: "alpha.txt", size: 2 }])],
      ["GET /backends/lake/objects?prefix=reports%2F&limit=100", () => listing([{ path: "reports/q3.csv", size: 2048, modTime: "2026-09-01T00:00:00Z" }])],
    ]);
    const user = userEvent.setup();
    mount();

    await screen.findByText("zeta.txt");
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows[0]).toHaveTextContent("reports/"); // folders first, then files alphabetically
    expect(rows[1]).toHaveTextContent("alpha.txt");
    expect(rows[2]).toHaveTextContent("zeta.txt");

    await user.click(screen.getByRole("button", { name: "Open folder reports" }));
    expect(await screen.findByText("q3.csv")).toBeInTheDocument();
    expect(screen.getByText("2.0 KB")).toBeInTheDocument();
    expect(screen.getByRole("navigation", { name: "Location" })).toHaveTextContent("Data lake/reports");

    await user.click(within(screen.getByRole("navigation", { name: "Location" })).getByRole("button", { name: "Data lake" }));
    expect(await screen.findByText("zeta.txt")).toBeInTheDocument();
    expect(m.calls("GET", /objects/)).toHaveLength(3);
  });

  it("pages through a large listing with Load more", async () => {
    mockFetch([
      ["GET /backends", () => ok([backends[0]])],
      ["GET /backends/lake/objects?limit=100", () => listing([{ path: "a.txt", size: 1 }], "cursor-1")],
      ["GET /backends/lake/objects?cursor=cursor-1&limit=100", () => listing([{ path: "b.txt", size: 1 }])],
    ]);
    const user = userEvent.setup();
    mount();

    await screen.findByText("a.txt");
    await user.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByText("b.txt")).toBeInTheDocument();
    expect(screen.getByText("a.txt")).toBeInTheDocument(); // appended, not replaced
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
  });

  it("downloads through an authenticated fetch, not a plain link", async () => {
    const created: string[] = [];
    // jsdom can't perform the navigation an anchor click to a blob: URL triggers; stub it.
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    vi.stubGlobal("URL", Object.assign(URL, { createObjectURL: vi.fn((b: Blob) => { created.push(String(b.size)); return "blob:mock"; }), revokeObjectURL: vi.fn() }));
    const m = mockFetch([
      ["GET /backends", () => ok([backends[0]])],
      ["GET /backends/lake/objects?limit=100", () => listing([{ path: "dir/data.csv", size: 3 }])],
      ["GET /backends/lake/objects/dir/data.csv", () => ({ text: "a,b" })],
    ]);
    const user = userEvent.setup();
    mount();

    await user.click(await screen.findByRole("button", { name: "Download data.csv" }));
    await waitFor(() => expect(created).toEqual(["3"]));
    const dl = m.calls("GET", /objects\/dir\/data\.csv/)[0];
    expect(dl.headers.get("Authorization")).toBe("Bearer tok");
    expect(dl.headers.get("X-Workspace")).toBe("acme");
  });

  it("shows an actionable error when listing fails, and keeps the rest usable", async () => {
    mockFetch([
      ["GET /backends", () => ok(backends)],
      ["GET /backends/lake/objects?limit=100", () => fail(502, "access denied by the storage service — check the credentials")],
      ["GET /backends/scratch/objects?limit=100", () => listing([{ path: "ok.txt", size: 1 }])],
    ]);
    const user = userEvent.setup();
    mount();

    expect(await screen.findByRole("alert")).toHaveTextContent("access denied by the storage service");
    await user.click(within(screen.getByRole("list", { name: "Storage backends" })).getByRole("button", { name: /Scratch/ }));
    expect(await screen.findByText("ok.txt")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("reports a failure to load the backends at all", async () => {
    mockFetch([["GET /backends", () => fail(503, "database unavailable")]]);
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent("Couldn't load storage backends: database unavailable");
  });

  it("shows empty states", async () => {
    mockFetch([
      ["GET /backends", () => ok([backends[0]])],
      ["GET /backends/lake/objects?limit=100", () => listing([])],
    ]);
    mount();
    expect(await screen.findByText("This backend is empty.")).toBeInTheDocument();
  });

  it("shows a viewer no write controls at all — hidden, not merely disabled (ADR 0038)", async () => {
    mockFetch([
      ["GET /backends", () => ok([backends[0]])],
      ["GET /backends/lake/objects?limit=100", () => listing([{ path: "a.txt", size: 1 }, { path: "dir/", size: 0, isDir: true }])],
    ]);
    mount("viewer");
    await screen.findByText("a.txt");
    for (const name of [/upload/i, /new folder/i, /rename/i, /delete/i, /remove/i]) {
      expect(screen.queryByRole("button", { name })).not.toBeInTheDocument();
    }
    expect(screen.queryByLabelText("Upload files")).not.toBeInTheDocument();
    expect(screen.queryByRole("toolbar", { name: "File actions" })).not.toBeInTheDocument();
    // Read-only actions remain.
    expect(screen.getByRole("button", { name: "Download a.txt" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Open folder dir" })).toBeInTheDocument();
    expect(screen.queryByText(/credential/i)).not.toBeInTheDocument();
  });
});

describe("BrowseView by role", () => {
  const routes: Routes = [
    ["GET /backends", () => ok([])],
  ];

  it("offers owners a way into the admin view", async () => {
    mockFetch(routes);
    const onNavigate = vi.fn();
    const user = userEvent.setup();
    mount("owner", { onNavigate, adminPath: "/storage/admin" });

    await user.click(await screen.findByRole("button", { name: "Manage backends" }));
    expect(onNavigate).toHaveBeenCalledWith("/storage/admin");
  });

  it.each<WorkspaceRole>(["editor", "viewer"])("does not offer the admin view to a %s", async (role) => {
    mockFetch(routes);
    mount(role);
    await screen.findByText("No storage backends registered yet");
    expect(screen.queryByRole("button", { name: "Manage backends" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add a backend" })).not.toBeInTheDocument();
    expect(screen.getByText(/workspace owner needs to register one/)).toBeInTheDocument();
  });

  it("points an owner with an empty workspace at registering their first backend", async () => {
    mockFetch(routes);
    mount("owner");
    expect(await screen.findByRole("button", { name: "Add a backend" })).toBeInTheDocument();
  });
});

describe("helpers", () => {
  it("breadcrumbs", () => {
    expect(breadcrumbs("")).toEqual([]);
    expect(breadcrumbs("a/b/")).toEqual([{ name: "a", prefix: "a/" }, { name: "b", prefix: "a/b/" }]);
  });

  it("formatBytes", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(1023)).toBe("1023 B");
    expect(formatBytes(1536)).toBe("1.5 KB");
    expect(formatBytes(5 * 1024 * 1024)).toBe("5.0 MB");
    expect(formatBytes(-1)).toBe("");
  });
});
