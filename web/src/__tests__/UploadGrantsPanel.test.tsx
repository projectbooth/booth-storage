import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { StorageApp } from "../StorageApp";
import { adminFixture, fail, mockFetch, ok, type Routes } from "./testUtils";

beforeEach(() => {
  window.history.pushState({}, "", "/storage/admin");
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

function mount() {
  return render(<StorageApp workspace="acme" role="owner" theme="light" getAccessToken={() => "tok"} view="admin" />);
}

const allKinds = { kinds: ["s3", "filesystem", "azure", "gcs"], filesystemEnabled: true };
const lake = adminFixture();

const grant = (over: Record<string, unknown> = {}) => ({
  id: "g1",
  workspace: "acme",
  principal: "streamlit:acme:app1",
  backendId: "lake",
  prefix: "incoming/",
  extensions: ["csv", "png"],
  maxBytes: 1024,
  grantedBy: "alice",
  createdAt: "2026-10-15T00:00:00Z",
  ...over,
});

function baseRoutes(grants: unknown[] = [], backends: unknown[] = [lake]): Routes {
  return [
    ["GET /admin/backends", () => ok(backends)],
    ["GET /kinds", () => ok(allKinds)],
    ["GET /admin/upload-grants", () => ok(grants)],
  ];
}

async function openUploadsTab(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole("tab", { name: "App uploads" }));
}

describe("App uploads tab", () => {
  it("is not fetched at all while the Backends tab is active", async () => {
    const m = mockFetch([["GET /admin/backends", () => ok([lake])], ["GET /kinds", () => ok(allKinds)]]);
    mount();
    await screen.findByRole("table");
    expect(m.calls("GET", /upload-grants/)).toHaveLength(0);
  });

  it("lists live grants with their backend, folder, extensions and size", async () => {
    mockFetch(baseRoutes([grant()]));
    const user = userEvent.setup();
    mount();
    await openUploadsTab(user);

    const table = await screen.findByRole("table");
    const row = within(table).getAllByRole("row")[1];
    expect(row).toHaveTextContent("streamlit:acme:app1");
    expect(row).toHaveTextContent("Data lake");
    expect(row).toHaveTextContent("incoming/");
    expect(row).toHaveTextContent(".csv, .png");
    expect(row).toHaveTextContent("1.0 KB");
  });

  it("shows an empty state", async () => {
    mockFetch(baseRoutes([]));
    const user = userEvent.setup();
    mount();
    await openUploadsTab(user);
    expect(await screen.findByText("No apps have upload access")).toBeInTheDocument();
  });

  it("creates a grant and normalizes the extensions list", async () => {
    const m = mockFetch([...baseRoutes([]), ["POST /admin/upload-grants", () => ok(grant())]]);
    const user = userEvent.setup();
    mount();
    await openUploadsTab(user);

    await user.click(await screen.findByRole("button", { name: "Grant upload access" }));
    const form = within(screen.getByRole("form", { name: "Grant upload access" }));
    await user.type(form.getByLabelText(/^App principal/), "streamlit:acme:app1");
    await user.selectOptions(form.getByLabelText(/^Backend/), "lake");
    await user.type(form.getByLabelText("Folder"), "incoming");
    await user.type(form.getByLabelText(/^Allowed file extensions/), "csv, .png, csv");
    await user.type(form.getByLabelText(/^Maximum file size/), "1024");
    await user.click(form.getByRole("button", { name: "Grant access" }));

    await waitFor(() => expect(m.calls("POST", /upload-grants$/)).toHaveLength(1));
    expect(m.calls("POST", /upload-grants$/)[0].body).toEqual({
      principal: "streamlit:acme:app1",
      backendId: "lake",
      prefix: "incoming",
      extensions: ["csv", ".png", "csv"], // the server normalizes; the client sends what was typed
      maxBytes: 1024,
    });
    expect(await screen.findByText(/Granted streamlit:acme:app1 access to lake/)).toBeInTheDocument();
  });

  it("validates locally before any request", async () => {
    const m = mockFetch(baseRoutes([]));
    const user = userEvent.setup();
    mount();
    await openUploadsTab(user);

    await user.click(await screen.findByRole("button", { name: "Grant upload access" }));
    const form = within(screen.getByRole("form", { name: "Grant upload access" }));
    await user.click(form.getByRole("button", { name: "Grant access" }));

    expect(form.getByText(/Must be the app's workload-token subject/)).toBeInTheDocument();
    expect(form.getByText("At least one file extension is required")).toBeInTheDocument();
    expect(form.getByText(/Must be a whole number of bytes/)).toBeInTheDocument();
    expect(m.calls("POST", /upload-grants$/)).toHaveLength(0);
  });

  it("shows a server validation error against the field it names", async () => {
    mockFetch([...baseRoutes([]), ["POST /admin/upload-grants", () => fail(422, "names workspace \"globex\", not \"acme\"", "principal")]]);
    const user = userEvent.setup();
    mount();
    await openUploadsTab(user);

    await user.click(await screen.findByRole("button", { name: "Grant upload access" }));
    const form = within(screen.getByRole("form", { name: "Grant upload access" }));
    await user.type(form.getByLabelText(/^App principal/), "streamlit:globex:app1");
    await user.selectOptions(form.getByLabelText(/^Backend/), "lake");
    await user.type(form.getByLabelText(/^Allowed file extensions/), "csv");
    await user.type(form.getByLabelText(/^Maximum file size/), "1024");
    await user.click(form.getByRole("button", { name: "Grant access" }));

    expect(await screen.findByText(/names workspace "globex"/)).toBeInTheDocument();
  });

  it("warns when the selected backend hasn't passed the create-only probe", async () => {
    const unverified = adminFixture({ id: "s3lake", displayName: "S3 Lake", kind: "s3", createOnlySupported: false });
    mockFetch(baseRoutes([], [lake, unverified]));
    const user = userEvent.setup();
    mount();
    await openUploadsTab(user);

    await user.click(await screen.findByRole("button", { name: "Grant upload access" }));
    const form = within(screen.getByRole("form", { name: "Grant upload access" }));
    await user.selectOptions(form.getByLabelText(/^Backend/), "s3lake");
    expect(form.getByText(/hasn't passed the create-only capability probe/)).toBeInTheDocument();
  });

  it("revokes a grant after confirming", async () => {
    let list = [grant()];
    const m = mockFetch([
      ["GET /admin/backends", () => ok([lake])],
      ["GET /kinds", () => ok(allKinds)],
      ["GET /admin/upload-grants", () => ok(list)],
      ["DELETE /admin/upload-grants/g1", () => { list = []; return { status: 204 }; }],
    ]);
    const user = userEvent.setup();
    mount();
    await openUploadsTab(user);

    await user.click(await screen.findByRole("button", { name: "Revoke streamlit:acme:app1" }));
    const confirm = within(screen.getByRole("group", { name: "Confirm revoking streamlit:acme:app1" }));
    expect(m.calls("DELETE", /./)).toHaveLength(0);

    await user.click(confirm.getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(m.calls("DELETE", /upload-grants\/g1$/)).toHaveLength(1));
    expect(await screen.findByText(/Revoked streamlit:acme:app1's access to lake/)).toBeInTheDocument();
  });

  it("cancelling revoke sends nothing", async () => {
    const m = mockFetch(baseRoutes([grant()]));
    const user = userEvent.setup();
    mount();
    await openUploadsTab(user);

    await user.click(await screen.findByRole("button", { name: "Revoke streamlit:acme:app1" }));
    await user.click(within(screen.getByRole("group", { name: "Confirm revoking streamlit:acme:app1" })).getByRole("button", { name: "Cancel" }));
    expect(m.calls("DELETE", /./)).toHaveLength(0);
  });
});

describe("URL prefill from a booth-streamlit link", () => {
  it("opens straight to the App uploads tab and form, prefilled", async () => {
    window.history.pushState({}, "", "/storage/admin?panel=upload-grants&principal=streamlit%3Aacme%3Aapp1&backendId=lake&prefix=incoming");
    mockFetch(baseRoutes([]));
    mount();

    const tab = await screen.findByRole("tab", { name: "App uploads" });
    expect(tab).toHaveAttribute("aria-selected", "true");

    const form = within(await screen.findByRole("form", { name: "Grant upload access" }));
    expect(form.getByLabelText(/^App principal/)).toHaveValue("streamlit:acme:app1");
    expect(form.getByLabelText(/^Backend/)).toHaveValue("lake");
    expect(form.getByLabelText("Folder")).toHaveValue("incoming");
  });

  it("a plain /storage/admin URL still defaults to Backends, closed form", async () => {
    mockFetch(baseRoutes([]));
    mount();
    const tab = await screen.findByRole("tab", { name: "Backends" });
    expect(tab).toHaveAttribute("aria-selected", "true");
    expect(screen.queryByRole("form", { name: "Grant upload access" })).not.toBeInTheDocument();
  });
});
