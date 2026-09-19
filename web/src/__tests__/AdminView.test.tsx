import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { StorageApp } from "../StorageApp";
import type { BackendKind, WorkspaceRole } from "../types";
import { adminFixture, fail, mockFetch, ok, type Routes } from "./testUtils";

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function mount(role: WorkspaceRole = "owner", extra: Record<string, unknown> = {}) {
  return render(<StorageApp workspace="acme" role={role} theme="light" getAccessToken={() => "tok"} view="admin" {...extra} />);
}

const allKinds = { kinds: ["s3", "filesystem", "azure", "gcs"], filesystemEnabled: true };

const lake = adminFixture();
const scratch = adminFixture({
  id: "scratch",
  displayName: "Scratch",
  kind: "filesystem",
  location: "/data/acme/scratch",
  config: { rootPath: "/data/acme/scratch" },
  credentialsSet: false,
});

function baseRoutes(list: unknown[] = [lake, scratch], kinds: unknown = allKinds): Routes {
  return [
    ["GET /admin/backends", () => ok(list)],
    ["GET /kinds", () => ok(kinds)],
  ];
}

async function openCreateForm(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole("button", { name: "Add backend" }));
  return screen.getByRole("form", { name: "Add a storage backend" });
}

describe("AdminView is owner-only (ADR 0023 / 0036)", () => {
  it.each<WorkspaceRole>(["editor", "viewer"])("shows a %s an explanation and makes no admin API calls", async (role) => {
    const m = mockFetch([]); // any request at all would throw
    const onOpenBrowse = vi.fn();
    const user = userEvent.setup();
    mount(role, { onNavigate: onOpenBrowse });

    expect(screen.getByText(/Only workspace owners can register, edit or remove/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add backend" })).not.toBeInTheDocument();
    expect(m.requests).toHaveLength(0);

    await user.click(screen.getByRole("button", { name: "Back to storage" }));
    expect(onOpenBrowse).toHaveBeenCalledWith("/storage");
  });
});

describe("AdminView list", () => {
  it("shows every backend with its kind, location and whether credentials are set — never the credentials", async () => {
    mockFetch(baseRoutes());
    mount();

    const table = await screen.findByRole("table");
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent("Data lake");
    expect(rows[0]).toHaveTextContent("S3-compatible");
    expect(rows[0]).toHaveTextContent("Set");
    expect(rows[1]).toHaveTextContent("Scratch");
    expect(rows[1]).toHaveTextContent("None");
  });

  it("shows an empty state and a load error", async () => {
    mockFetch(baseRoutes([]));
    const { unmount } = mount();
    expect(await screen.findByText("No storage backends yet")).toBeInTheDocument();
    unmount();

    mockFetch([["GET /admin/backends", () => fail(500, "boom")], ["GET /kinds", () => ok(allKinds)]]);
    mount();
    expect(await screen.findByText(/Couldn't load storage backends: boom/)).toBeInTheDocument();
  });
});

describe("registering a backend", () => {
  it("creates an S3 backend: secrets go in `credentials`, never in `config`", async () => {
    const m = mockFetch([...baseRoutes([]), ["POST /admin/backends", () => ok(adminFixture({ displayName: "Data lake" }))]]);
    const user = userEvent.setup();
    mount();

    const form = within(await openCreateForm(user));
    await user.type(form.getByLabelText("Display name"), "My Data Lake");
    expect(form.getByLabelText(/^ID/)).toHaveValue("my-data-lake"); // suggested from the name
    await user.type(form.getByLabelText(/^Bucket/), "acme-data");
    await user.type(form.getByLabelText("Endpoint"), "http://minio:9000");
    await user.click(form.getByLabelText(/path-style/));
    await user.type(form.getByLabelText("Access key ID"), "AKIA123");
    await user.type(form.getByLabelText("Secret access key"), "s3cr3t-value");
    await user.click(form.getByRole("button", { name: "Add backend" }));

    await waitFor(() => expect(m.calls("POST", /admin\/backends$/)).toHaveLength(1));
    const body = m.calls("POST", /admin\/backends$/)[0].body as Record<string, unknown>;
    expect(body).toEqual({
      id: "my-data-lake",
      displayName: "My Data Lake",
      kind: "s3",
      config: { bucket: "acme-data", endpoint: "http://minio:9000", pathStyle: true },
      credentials: { accessKeyId: "AKIA123", secretAccessKey: "s3cr3t-value" },
    });
    expect(JSON.stringify(body.config)).not.toMatch(/AKIA123|s3cr3t/);
    expect(await screen.findByText(/Registered "Data lake"/)).toBeInTheDocument();
  });

  it("masks credential inputs", async () => {
    mockFetch(baseRoutes([]));
    const user = userEvent.setup();
    mount();
    const form = within(await openCreateForm(user));
    expect(form.getByLabelText("Access key ID")).toHaveAttribute("type", "password");
    expect(form.getByLabelText("Secret access key")).toHaveAttribute("type", "password");
    expect(form.getByLabelText("Session token")).toHaveAttribute("type", "password");
  });

  // ADR 0013: all four kinds are registerable, each with its own fields.
  it.each<[BackendKind, string[], string[]]>([
    ["s3", ["Bucket", "Endpoint", "Region"], ["Access key ID", "Secret access key"]],
    ["filesystem", ["Root directory"], []],
    ["azure", ["Storage account", "Container"], ["Account key", "SAS token"]],
    ["gcs", ["Bucket", "Object prefix"], ["Service account key (JSON)"]],
  ])("offers the %s kind with its own location and credential fields", async (kind, config, credentials) => {
    mockFetch(baseRoutes([]));
    const user = userEvent.setup();
    mount();
    const form = within(await openCreateForm(user));

    await user.selectOptions(form.getByLabelText("Kind"), kind);
    for (const label of config) expect(form.getByLabelText(new RegExp(`^${label}`))).toBeInTheDocument();
    for (const label of credentials) expect(form.getByLabelText(label)).toBeInTheDocument();
    if (credentials.length === 0) expect(form.queryByText("Credentials")).not.toBeInTheDocument();
  });

  it("creates a filesystem backend with no credentials at all", async () => {
    const m = mockFetch([...baseRoutes([]), ["POST /admin/backends", () => ok(scratch)]]);
    const user = userEvent.setup();
    mount();
    const form = within(await openCreateForm(user));

    await user.selectOptions(form.getByLabelText("Kind"), "filesystem");
    await user.type(form.getByLabelText("Display name"), "Scratch");
    await user.type(form.getByLabelText(/^Root directory/), "/data/acme/scratch");
    await user.click(form.getByRole("button", { name: "Add backend" }));

    await waitFor(() => expect(m.calls("POST", /admin\/backends$/)).toHaveLength(1));
    const body = m.calls("POST", /admin\/backends$/)[0].body as Record<string, unknown>;
    expect(body).toEqual({ id: "scratch", displayName: "Scratch", kind: "filesystem", config: { rootPath: "/data/acme/scratch" } });
    expect(body).not.toHaveProperty("credentials");
  });

  it("does not offer the filesystem kind when the operator hasn't enabled it", async () => {
    mockFetch(baseRoutes([], { kinds: ["s3", "azure", "gcs"], filesystemEnabled: false }));
    const user = userEvent.setup();
    mount();
    const form = within(await openCreateForm(user));
    const options = within(form.getByLabelText("Kind")).getAllByRole("option").map((o) => o.textContent);
    expect(options).toEqual(["S3-compatible", "Azure Blob Storage", "Google Cloud Storage"]);
  });

  it("changing the kind discards fields from the previous kind, including typed secrets", async () => {
    const m = mockFetch([...baseRoutes([]), ["POST /admin/backends", () => ok(lake)]]);
    const user = userEvent.setup();
    mount();
    const form = within(await openCreateForm(user));

    await user.type(form.getByLabelText(/^Bucket/), "old-bucket");
    await user.type(form.getByLabelText("Secret access key"), "leftover-secret");
    await user.selectOptions(form.getByLabelText("Kind"), "gcs");
    await user.type(form.getByLabelText("Display name"), "Models");
    await user.type(form.getByLabelText(/^Bucket/), "acme-models");
    await user.click(form.getByRole("button", { name: "Add backend" }));

    await waitFor(() => expect(m.calls("POST", /admin\/backends$/)).toHaveLength(1));
    const body = m.calls("POST", /admin\/backends$/)[0].body as Record<string, unknown>;
    expect(body).toEqual({ id: "models", displayName: "Models", kind: "gcs", config: { bucket: "acme-models" } });
    expect(JSON.stringify(body)).not.toContain("leftover-secret");
  });

  it("validates locally before any request: required fields and ID format", async () => {
    const m = mockFetch([...baseRoutes([]), ["POST /admin/backends", () => ok(lake)]]);
    const user = userEvent.setup();
    mount();
    const form = within(await openCreateForm(user));

    await user.click(form.getByRole("button", { name: "Add backend" }));
    expect(form.getAllByText("Required").length).toBeGreaterThanOrEqual(2); // ID and bucket
    expect(m.calls("POST", /admin\/backends$/)).toHaveLength(0);

    await user.type(form.getByLabelText(/^ID/), "Bad ID!");
    await user.click(form.getByRole("button", { name: "Add backend" }));
    expect(form.getByText(/Use lowercase letters, digits and hyphens/)).toBeInTheDocument();
    expect(m.calls("POST", /admin\/backends$/)).toHaveLength(0);
  });

  it("shows a server validation error against the field it names, and stays open", async () => {
    mockFetch([...baseRoutes([]), ["POST /admin/backends", () => fail(409, "a backend with that id already exists in this workspace", "id")]]);
    const user = userEvent.setup();
    mount();
    const form = within(await openCreateForm(user));

    await user.type(form.getByLabelText("Display name"), "Lake");
    await user.type(form.getByLabelText(/^Bucket/), "b");
    await user.click(form.getByRole("button", { name: "Add backend" }));

    const idField = form.getByLabelText(/^ID/);
    await waitFor(() => expect(idField).toHaveAttribute("aria-invalid", "true"));
    expect(form.getByText("a backend with that id already exists in this workspace")).toBeInTheDocument();
    expect(screen.getByRole("form", { name: "Add a storage backend" })).toBeInTheDocument(); // not dismissed
    expect(form.getByRole("button", { name: "Add backend" })).toBeEnabled(); // can retry
  });

  it("shows a server error that names no input as a general banner", async () => {
    mockFetch([...baseRoutes([]), ["POST /admin/backends", () => fail(422, "rootPath is outside the directories this workspace may use (allowed: /data/acme)", "config")]]);
    const user = userEvent.setup();
    mount();
    const form = within(await openCreateForm(user));

    await user.selectOptions(form.getByLabelText("Kind"), "filesystem");
    await user.type(form.getByLabelText("Display name"), "Bad");
    await user.type(form.getByLabelText(/^Root directory/), "/etc");
    await user.click(form.getByRole("button", { name: "Add backend" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("outside the directories this workspace may use");
  });

  it("cancelling closes the form without sending anything", async () => {
    const m = mockFetch(baseRoutes([]));
    const user = userEvent.setup();
    mount();
    const form = within(await openCreateForm(user));
    await user.click(form.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("form")).not.toBeInTheDocument();
    expect(m.requests.filter((r) => r.method !== "GET")).toHaveLength(0);
  });
});

describe("testing a connection", () => {
  it("tests an unsaved backend and reports success", async () => {
    const m = mockFetch([...baseRoutes([]), ["POST /admin/test-connection", () => ok({ ok: true })]]);
    const user = userEvent.setup();
    mount();
    const form = within(await openCreateForm(user));

    await user.type(form.getByLabelText("Display name"), "Lake");
    await user.type(form.getByLabelText(/^Bucket/), "b");
    await user.type(form.getByLabelText("Access key ID"), "A");
    await user.type(form.getByLabelText("Secret access key"), "B");
    await user.click(form.getByRole("button", { name: "Test connection" }));

    expect(await screen.findByText("Connection succeeded.")).toBeInTheDocument();
    expect(m.calls("POST", /test-connection/)[0].body).toMatchObject({ kind: "s3", config: { bucket: "b" }, credentials: { accessKeyId: "A", secretAccessKey: "B" } });
    expect(m.calls("POST", /admin\/backends$/)).toHaveLength(0); // testing saves nothing
  });

  it("reports a failed connection with the reason, and clears the result when the form changes", async () => {
    mockFetch([...baseRoutes([]), ["POST /admin/test-connection", () => ok({ ok: false, error: "access denied by the storage service" })]]);
    const user = userEvent.setup();
    mount();
    const form = within(await openCreateForm(user));

    await user.type(form.getByLabelText("Display name"), "Lake");
    await user.type(form.getByLabelText(/^Bucket/), "b");
    await user.click(form.getByRole("button", { name: "Test connection" }));
    expect(await screen.findByText(/Connection failed: access denied by the storage service/)).toBeInTheDocument();

    await user.type(form.getByLabelText(/^Bucket/), "x"); // any edit invalidates the stale result
    expect(screen.queryByText(/Connection failed/)).not.toBeInTheDocument();
  });

  it("tests a saved backend from its row", async () => {
    const m = mockFetch([
      ...baseRoutes(),
      ["POST /admin/backends/lake/test", () => ok({ ok: true })],
      ["POST /admin/backends/scratch/test", () => ok({ ok: false, error: "root directory does not exist" })],
    ]);
    const user = userEvent.setup();
    mount();

    await user.click(await screen.findByRole("button", { name: "Test Data lake" }));
    expect(await screen.findByText("Connection OK")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Test Scratch" }));
    expect(await screen.findByText("Connection failed: root directory does not exist")).toBeInTheDocument();
    expect(m.calls("POST", /\/test$/)).toHaveLength(2);
  });
});

describe("editing a backend — credentials are write-only", () => {
  async function openEdit(user: ReturnType<typeof userEvent.setup>, name: string) {
    await user.click(await screen.findByRole("button", { name: `Edit ${name}` }));
    return within(screen.getByRole("form", { name: `Edit ${name}` }));
  }

  it("seeds the form from saved config, locks the ID and kind, and never shows secrets", async () => {
    mockFetch(baseRoutes());
    const user = userEvent.setup();
    mount();
    const form = await openEdit(user, "Data lake");

    expect(form.getByLabelText("Display name")).toHaveValue("Data lake");
    expect(form.getByLabelText(/^ID/)).toBeDisabled();
    expect(form.getByLabelText("Kind")).toBeDisabled();
    expect(form.getByLabelText(/^Bucket/)).toHaveValue("acme-data");
    expect(form.getByLabelText("Endpoint")).toHaveValue("http://minio:9000");
    expect(form.getByLabelText(/path-style/)).toBeChecked();
    expect(form.getByLabelText("Access key ID")).toHaveValue("");
    expect(form.getByLabelText("Secret access key")).toHaveValue("");
    expect(form.getByLabelText("Secret access key")).toHaveAttribute("placeholder", "Leave blank to keep the stored value");
  });

  it("blank credentials means KEEP: the request carries no `credentials` key", async () => {
    const m = mockFetch([...baseRoutes(), ["PUT /admin/backends/lake", () => ok(lake)]]);
    const user = userEvent.setup();
    mount();
    const form = await openEdit(user, "Data lake");

    await user.clear(form.getByLabelText("Display name"));
    await user.type(form.getByLabelText("Display name"), "Renamed lake");
    await user.click(form.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(m.calls("PUT", /admin\/backends\/lake$/)).toHaveLength(1));
    const body = m.calls("PUT", /admin\/backends\/lake$/)[0].body as Record<string, unknown>;
    expect(body).toMatchObject({ displayName: "Renamed lake", config: { bucket: "acme-data" } });
    expect(body).not.toHaveProperty("credentials"); // omitted => server keeps the stored set
    expect(await screen.findByText(/Saved "Data lake"/)).toBeInTheDocument();
  });

  it("typing credentials replaces them", async () => {
    const m = mockFetch([...baseRoutes(), ["PUT /admin/backends/lake", () => ok(lake)]]);
    const user = userEvent.setup();
    mount();
    const form = await openEdit(user, "Data lake");

    await user.type(form.getByLabelText("Access key ID"), "ROTATED");
    await user.type(form.getByLabelText("Secret access key"), "rotated-secret");
    await user.click(form.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(m.calls("PUT", /admin\/backends\/lake$/)).toHaveLength(1));
    expect((m.calls("PUT", /admin\/backends\/lake$/)[0].body as Record<string, unknown>).credentials).toEqual({
      accessKeyId: "ROTATED",
      secretAccessKey: "rotated-secret",
    });
  });

  it("clearing credentials is an explicit opt-in and sends an empty set", async () => {
    const m = mockFetch([...baseRoutes(), ["PUT /admin/backends/lake", () => ok(lake)]]);
    const user = userEvent.setup();
    mount();
    const form = await openEdit(user, "Data lake");

    await user.click(form.getByLabelText("Remove the stored credentials"));
    await user.click(form.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(m.calls("PUT", /admin\/backends\/lake$/)).toHaveLength(1));
    expect((m.calls("PUT", /admin\/backends\/lake$/)[0].body as Record<string, unknown>).credentials).toEqual({});
  });

  it("typed credentials win over the 'remove' checkbox, which is then disabled", async () => {
    mockFetch(baseRoutes());
    const user = userEvent.setup();
    mount();
    const form = await openEdit(user, "Data lake");

    await user.type(form.getByLabelText("Access key ID"), "A");
    expect(form.getByLabelText("Remove the stored credentials")).toBeDisabled();
  });

  it("offers no 'remove credentials' option when none are stored, and says so", async () => {
    mockFetch(baseRoutes([adminFixture({ credentialsSet: false })]));
    const user = userEvent.setup();
    mount();
    const form = await openEdit(user, "Data lake");
    expect(form.queryByLabelText("Remove the stored credentials")).not.toBeInTheDocument();
    expect(form.getByText(/No credentials are stored/)).toBeInTheDocument();
  });

  it("previews an edit's connection without saving, using stored credentials", async () => {
    const m = mockFetch([...baseRoutes(), ["POST /admin/backends/lake/test", () => ok({ ok: true })]]);
    const user = userEvent.setup();
    mount();
    const form = await openEdit(user, "Data lake");

    await user.clear(form.getByLabelText(/^Bucket/));
    await user.type(form.getByLabelText(/^Bucket/), "other-bucket");
    await user.click(form.getByRole("button", { name: "Test connection" }));

    expect(await screen.findByText("Connection succeeded.")).toBeInTheDocument();
    const req = m.calls("POST", /lake\/test$/)[0];
    expect((req.body as { config: Record<string, unknown> }).config.bucket).toBe("other-bucket");
    expect(req.body).not.toHaveProperty("credentials");
    expect(m.calls("PUT", /./)).toHaveLength(0);
  });
});

describe("removing a backend", () => {
  it("asks first, explains data is untouched, then deletes", async () => {
    let list = [lake, scratch];
    const m = mockFetch([
      ["GET /admin/backends", () => ok(list)],
      ["GET /kinds", () => ok(allKinds)],
      ["DELETE /admin/backends/lake", () => { list = [scratch]; return { status: 204 }; }],
    ]);
    const user = userEvent.setup();
    mount();

    await user.click(await screen.findByRole("button", { name: "Remove Data lake" }));
    const confirm = within(screen.getByRole("group", { name: "Confirm removing Data lake" }));
    expect(confirm.getByText(/Data in the underlying store is not touched/)).toBeInTheDocument();
    expect(m.calls("DELETE", /./)).toHaveLength(0); // nothing sent until confirmed

    await user.click(confirm.getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(m.calls("DELETE", /admin\/backends\/lake$/)).toHaveLength(1));
    expect(await screen.findByText(/Removed "lake"/)).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole("button", { name: "Remove Data lake" })).not.toBeInTheDocument());
    expect(screen.getByRole("button", { name: "Remove Scratch" })).toBeInTheDocument();
  });

  it("cancelling sends nothing", async () => {
    const m = mockFetch(baseRoutes());
    const user = userEvent.setup();
    mount();

    await user.click(await screen.findByRole("button", { name: "Remove Data lake" }));
    await user.click(within(screen.getByRole("group", { name: "Confirm removing Data lake" })).getByRole("button", { name: "Cancel" }));
    expect(m.calls("DELETE", /./)).toHaveLength(0);
    expect(screen.getByRole("button", { name: "Remove Data lake" })).toBeInTheDocument();
  });

  it("reports a failed removal", async () => {
    mockFetch([...baseRoutes(), ["DELETE /admin/backends/lake", () => fail(500, "boom")]]);
    const user = userEvent.setup();
    mount();

    await user.click(await screen.findByRole("button", { name: "Remove Data lake" }));
    await user.click(within(screen.getByRole("group", { name: "Confirm removing Data lake" })).getByRole("button", { name: "Remove" }));
    expect(await screen.findByText(/Couldn't remove "lake": boom/)).toBeInTheDocument();
  });
});
