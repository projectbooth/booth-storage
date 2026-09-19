import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { nameProblem } from "../components/ObjectBrowser";
import { StorageApp } from "../StorageApp";
import type { WorkspaceRole } from "../types";
import { backendFixture, fail, mockFetch, ok, type Routes } from "./testUtils";

// ADR 0038: editors and owners create folders, upload, rename and delete in the file
// browser. The mock server below holds a small mutable tree so each action's effect on the
// next listing is visible.

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function mount(role: WorkspaceRole = "editor") {
  return render(<StorageApp workspace="acme" role={role} theme="light" getAccessToken={() => "tok"} view="browse" />);
}

type Entry = { path: string; size: number; isDir?: boolean };

/** A mock backend whose listing reflects the mutations made through it. */
function tree(initial: Entry[]) {
  const entries = [...initial];
  const routes: Routes = [
    ["GET /backends", () => ok([backendFixture()])],
    [/^GET \/backends\/lake\/objects/, (req) => {
      const prefix = new URL(req.url, "http://x").searchParams.get("prefix") ?? "";
      return ok({ entries: entries.filter((e) => e.path.startsWith(prefix) && !e.path.slice(prefix.length).replace(/\/$/, "").includes("/")) });
    }],
  ];
  return { entries, routes };
}

const root = [
  { path: "report.csv", size: 10 },
  { path: "docs/", size: 0, isDir: true },
];

describe.each<WorkspaceRole>(["editor", "owner"])("write controls for a %s", (role) => {
  it("shows New folder, Upload, Rename and Delete", async () => {
    const t = tree(root);
    mockFetch(t.routes);
    mount(role);
    await screen.findByText("report.csv");

    const toolbar = screen.getByRole("toolbar", { name: "File actions" });
    expect(within(toolbar).getByRole("button", { name: "New folder" })).toBeEnabled();
    expect(within(toolbar).getByRole("button", { name: "Upload" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Rename report.csv" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Delete report.csv" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Delete docs" })).toBeInTheDocument();
  });
});

describe("creating a folder", () => {
  it("creates it under the current folder and refreshes the listing", async () => {
    const t = tree(root);
    const m = mockFetch([
      ...t.routes,
      ["POST /backends/lake/folders", (req) => {
        t.entries.push({ path: `${(req.body as { path: string }).path}/`, size: 0, isDir: true });
        return { status: 201, json: { path: "x/" } };
      }],
    ]);
    const user = userEvent.setup();
    mount();
    await screen.findByText("report.csv");

    await user.click(screen.getByRole("button", { name: "New folder" }));
    await user.type(screen.getByLabelText("Folder name"), "  archive  ");
    await user.click(screen.getByRole("button", { name: "Create" }));

    expect(await screen.findByRole("button", { name: "Open folder archive" })).toBeInTheDocument();
    expect(m.calls("POST", /folders$/)[0].body).toEqual({ path: "archive" }); // trimmed, at the root
    expect(screen.getByText("Created folder archive.")).toBeInTheDocument();
    expect(screen.queryByRole("form", { name: "New folder" })).not.toBeInTheDocument(); // form closed
  });

  it("creates inside a folder the user has navigated into", async () => {
    const t = tree([...root, { path: "docs/a.txt", size: 1 }]);
    const m = mockFetch([...t.routes, ["POST /backends/lake/folders", () => ({ status: 201, json: {} })]]);
    const user = userEvent.setup();
    mount();
    await user.click(await screen.findByRole("button", { name: "Open folder docs" }));
    await screen.findByText("a.txt");

    await user.click(screen.getByRole("button", { name: "New folder" }));
    await user.type(screen.getByLabelText("Folder name"), "sub");
    await user.click(screen.getByRole("button", { name: "Create" }));

    await waitFor(() => expect(m.calls("POST", /folders$/)).toHaveLength(1));
    expect(m.calls("POST", /folders$/)[0].body).toEqual({ path: "docs/sub" });
  });

  it("rejects bad names locally without a request", async () => {
    const t = tree(root);
    const m = mockFetch(t.routes);
    const user = userEvent.setup();
    mount();
    await screen.findByText("report.csv");
    await user.click(screen.getByRole("button", { name: "New folder" }));

    const input = screen.getByLabelText("Folder name");
    expect(screen.getByRole("button", { name: "Create" })).toBeDisabled(); // empty
    await user.type(input, "a/b");
    expect(screen.getByRole("alert")).toHaveTextContent("can't contain slashes");
    expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
    await user.clear(input);
    await user.type(input, "..");
    expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
    expect(m.calls("POST", /./)).toHaveLength(0);
  });

  it("shows a server refusal and keeps the form open", async () => {
    const t = tree(root);
    mockFetch([...t.routes, ["POST /backends/lake/folders", () => fail(409, "already exists")]]);
    const user = userEvent.setup();
    mount();
    await screen.findByText("report.csv");
    await user.click(screen.getByRole("button", { name: "New folder" }));
    await user.type(screen.getByLabelText("Folder name"), "docs");
    await user.click(screen.getByRole("button", { name: "Create" }));

    expect(await screen.findByText(/Couldn't create folder docs: already exists/)).toBeInTheDocument();
    expect(screen.getByRole("form", { name: "New folder" })).toBeInTheDocument();
  });
});

describe("uploading", () => {
  const file = (name: string, body = "data", type = "text/plain") => new File([body], name, { type });

  it("uploads each chosen file into the current folder with its content type", async () => {
    const t = tree([{ path: "docs/", size: 0, isDir: true }, { path: "docs/old.txt", size: 1 }]);
    const m = mockFetch([...t.routes, [/^PUT \/backends\/lake\/objects\//, (req) => { t.entries.push({ path: decodeURIComponent(req.url.split("/objects/")[1]), size: 4 }); return ok({}); }]]);
    const user = userEvent.setup();
    mount();
    await user.click(await screen.findByRole("button", { name: "Open folder docs" }));
    await screen.findByText("old.txt");

    await user.upload(screen.getByLabelText("Upload files"), [file("one.csv", "a,b", "text/csv"), file("two.bin", "xx", "")]);

    expect(await screen.findByText("Uploaded 2 files.")).toBeInTheDocument();
    const puts = m.calls("PUT", /objects\//);
    expect(puts.map((r) => r.url)).toEqual([
      "/modules/storage/api/backends/lake/objects/docs/one.csv",
      "/modules/storage/api/backends/lake/objects/docs/two.bin",
    ]);
    expect(puts[0].headers.get("Content-Type")).toBe("text/csv");
    expect(puts[1].headers.get("Content-Type")).toBe("application/octet-stream"); // unknown type falls back
    expect(puts[0].headers.get("Authorization")).toBe("Bearer tok");
    expect(await screen.findByText("one.csv")).toBeInTheDocument(); // listing refreshed
  });

  it("asks before replacing a file that's already listed, and skips the upload if declined", async () => {
    const t = tree(root);
    const m = mockFetch([...t.routes, [/^PUT /, () => ok({})]]);
    const user = userEvent.setup();
    mount();
    await screen.findByText("report.csv");

    await user.upload(screen.getByLabelText("Upload files"), file("report.csv"));
    expect(screen.getByText(/already exists here\. Uploading replaces it\./)).toBeInTheDocument();
    expect(m.calls("PUT", /./)).toHaveLength(0); // nothing sent yet

    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(m.calls("PUT", /./)).toHaveLength(0);
    expect(screen.queryByText(/already exists here/)).not.toBeInTheDocument();
  });

  it("replaces after confirmation", async () => {
    const t = tree(root);
    const m = mockFetch([...t.routes, [/^PUT /, () => ok({})]]);
    const user = userEvent.setup();
    mount();
    await screen.findByText("report.csv");

    await user.upload(screen.getByLabelText("Upload files"), file("report.csv"));
    await user.click(screen.getByRole("button", { name: "Replace" }));
    await waitFor(() => expect(m.calls("PUT", /./)).toHaveLength(1));
    expect(m.calls("PUT", /./)[0].url).toBe("/modules/storage/api/backends/lake/objects/report.csv");
  });

  it("reports which file failed without hiding the ones that succeeded", async () => {
    const t = tree(root);
    mockFetch([
      ...t.routes,
      ["PUT /backends/lake/objects/good.txt", () => ok({})],
      ["PUT /backends/lake/objects/bad.txt", () => fail(502, "access denied by the storage service")],
    ]);
    const user = userEvent.setup();
    mount();
    await screen.findByText("report.csv");

    await user.upload(screen.getByLabelText("Upload files"), [file("good.txt"), file("bad.txt")]);
    const list = await screen.findByRole("list", { name: "Uploads" });
    await waitFor(() => expect(within(list).getByText(/bad\.txt — failed: access denied/)).toBeInTheDocument());
    expect(within(list).getByText(/good\.txt — uploaded/)).toBeInTheDocument();
  });
});

describe("renaming", () => {
  it("renames a file within its folder and refreshes", async () => {
    const t = tree([{ path: "docs/", size: 0, isDir: true }, { path: "docs/a.txt", size: 1 }]);
    const m = mockFetch([
      ...t.routes,
      ["POST /backends/lake/move", (req) => {
        const b = req.body as { from: string; to: string };
        t.entries.find((e) => e.path === b.from)!.path = b.to;
        return ok(b);
      }],
    ]);
    const user = userEvent.setup();
    mount();
    await user.click(await screen.findByRole("button", { name: "Open folder docs" }));
    await screen.findByText("a.txt");

    await user.click(screen.getByRole("button", { name: "Rename a.txt" }));
    const input = screen.getByLabelText("New name for a.txt");
    expect(input).toHaveValue("a.txt");
    await user.clear(input);
    await user.type(input, "b.txt");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText("b.txt")).toBeInTheDocument();
    expect(m.calls("POST", /move$/)[0].body).toEqual({ from: "docs/a.txt", to: "docs/b.txt", folder: false });
    expect(screen.getByText("Renamed a.txt to b.txt.")).toBeInTheDocument();
  });

  it("renames a folder as a folder move", async () => {
    const t = tree(root);
    const m = mockFetch([...t.routes, ["POST /backends/lake/move", () => ok({})]]);
    const user = userEvent.setup();
    mount();
    await user.click(await screen.findByRole("button", { name: "Rename docs" }));
    const input = screen.getByLabelText("New name for docs");
    await user.clear(input);
    await user.type(input, "papers");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(m.calls("POST", /move$/)).toHaveLength(1));
    expect(m.calls("POST", /move$/)[0].body).toEqual({ from: "docs", to: "papers", folder: true });
  });

  it("validates the new name, and an unchanged name sends nothing", async () => {
    const t = tree(root);
    const m = mockFetch(t.routes);
    const user = userEvent.setup();
    mount();
    await user.click(await screen.findByRole("button", { name: "Rename report.csv" }));
    const input = screen.getByLabelText("New name for report.csv");

    await user.clear(input);
    await user.type(input, "a/b");
    expect(screen.getByRole("alert")).toHaveTextContent("can't contain slashes");
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();

    await user.clear(input);
    await user.type(input, "report.csv"); // same as before
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(m.calls("POST", /./)).toHaveLength(0);
    expect(screen.queryByRole("form", { name: "Rename report.csv" })).not.toBeInTheDocument();
  });

  it("shows a name clash from the server (409) and keeps the editor open", async () => {
    const t = tree(root);
    mockFetch([...t.routes, ["POST /backends/lake/move", () => fail(409, 'already exists: "docs"')]]);
    const user = userEvent.setup();
    mount();
    await user.click(await screen.findByRole("button", { name: "Rename report.csv" }));
    const input = screen.getByLabelText("New name for report.csv");
    await user.clear(input);
    await user.type(input, "docs");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText(/Couldn't rename report\.csv: already exists/)).toBeInTheDocument();
    expect(screen.getByLabelText("New name for report.csv")).toBeInTheDocument();
  });
});

describe("deleting", () => {
  it("asks first, then deletes a file", async () => {
    const t = tree(root);
    const m = mockFetch([
      ...t.routes,
      ["DELETE /backends/lake/objects/report.csv", () => { t.entries.splice(0, 1); return { status: 204 }; }],
    ]);
    const user = userEvent.setup();
    mount();
    await user.click(await screen.findByRole("button", { name: "Delete report.csv" }));

    const confirm = within(screen.getByRole("group", { name: "Confirm deleting report.csv" }));
    expect(confirm.getByText(/can't be undone/)).toBeInTheDocument();
    expect(m.calls("DELETE", /./)).toHaveLength(0); // nothing sent until confirmed

    await user.click(confirm.getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(screen.queryByText("report.csv")).not.toBeInTheDocument());
    expect(screen.getByText("Deleted report.csv.")).toBeInTheDocument();
  });

  it("warns that deleting a folder deletes everything inside, and reports the count", async () => {
    const t = tree(root);
    const m = mockFetch([...t.routes, ["DELETE /backends/lake/folders/docs", () => ok({ deleted: 7 })]]);
    const user = userEvent.setup();
    mount();
    await user.click(await screen.findByRole("button", { name: "Delete docs" }));

    const confirm = within(screen.getByRole("group", { name: "Confirm deleting docs" }));
    expect(confirm.getByText(/everything inside it/)).toBeInTheDocument();
    await user.click(confirm.getByRole("button", { name: "Delete" }));

    expect(await screen.findByText("Deleted folder docs (7 files).")).toBeInTheDocument();
    expect(m.calls("DELETE", /folders\/docs$/)).toHaveLength(1);
  });

  it("cancelling sends nothing", async () => {
    const t = tree(root);
    const m = mockFetch(t.routes);
    const user = userEvent.setup();
    mount();
    await user.click(await screen.findByRole("button", { name: "Delete report.csv" }));
    await user.click(within(screen.getByRole("group", { name: "Confirm deleting report.csv" })).getByRole("button", { name: "Cancel" }));
    expect(m.calls("DELETE", /./)).toHaveLength(0);
    expect(screen.getByRole("button", { name: "Delete report.csv" })).toBeInTheDocument();
  });

  it("shows the reason when the server refuses (e.g. a role change mid-session)", async () => {
    const t = tree(root);
    mockFetch([...t.routes, ["DELETE /backends/lake/objects/report.csv", () => fail(403, "only workspace editors and owners can write objects")]]);
    const user = userEvent.setup();
    mount();
    await user.click(await screen.findByRole("button", { name: "Delete report.csv" }));
    await user.click(within(screen.getByRole("group", { name: "Confirm deleting report.csv" })).getByRole("button", { name: "Delete" }));

    expect(await screen.findByText(/Couldn't delete report\.csv: only workspace editors and owners/)).toBeInTheDocument();
    // Still listed: the row's name cell is still there (the open confirm prompt also names it).
    expect(screen.getAllByText("report.csv").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByRole("button", { name: "Open folder docs" })).toBeInTheDocument();
  });
});

describe("nameProblem", () => {
  it.each([["", true], ["  ", true], ["a/b", true], ["a\\b", true], [".", true], ["..", true], ["ok.txt", false], [" spaced ", false], ["dots..", false], [".hidden", false]])(
    "%j -> problem: %s",
    (name, problem) => expect(nameProblem(name) !== null).toBe(problem),
  );
});
