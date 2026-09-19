import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { isAdminPath } from "../navigation";
import { StorageApp } from "../StorageApp";
import type { WorkspaceRole } from "../types";
import { adminFixture, backendFixture, mockFetch, ok } from "./testUtils";

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

const allRoutes = () =>
  mockFetch([
    ["GET /backends", () => ok([backendFixture()])],
    [/^GET \/backends\/lake\/objects/, () => ok({ entries: [] })],
    ["GET /admin/backends", () => ok([adminFixture()])],
    ["GET /kinds", () => ok({ kinds: ["s3", "filesystem", "azure", "gcs"], filesystemEnabled: true })],
  ]);

function mount(role: WorkspaceRole = "owner", extra: Record<string, unknown> = {}) {
  return render(<StorageApp workspace="acme" role={role} theme="light" getAccessToken={() => "tok"} {...extra} />);
}

describe("view selection — the shell passes no route or view prop, so the address bar decides", () => {
  it("shows the regular view at the manifest's navPath", async () => {
    window.history.pushState({}, "", "/storage");
    allRoutes();
    mount();
    expect(await screen.findByText("Browse the storage backends registered in this workspace.")).toBeInTheDocument();
    expect(screen.queryByText("Manage storage backends")).not.toBeInTheDocument();
  });

  it("shows the admin view at the manifest's adminNavPath", async () => {
    window.history.pushState({}, "", "/storage/admin");
    allRoutes();
    mount();
    expect(await screen.findByRole("heading", { name: "Manage storage backends" })).toBeInTheDocument();
  });

  it("treats routes nested under the admin path as the admin view, but not lookalikes", () => {
    expect(isAdminPath("/storage/admin")).toBe(true);
    expect(isAdminPath("/storage/admin/backends/x")).toBe(true);
    expect(isAdminPath("/storage")).toBe(false);
    expect(isAdminPath("/storage/administrators")).toBe(false);
    expect(isAdminPath("/storage/adminx")).toBe(false);
    expect(isAdminPath("/other/storage/admin")).toBe(false);
  });

  it("honours a custom adminPath", async () => {
    window.history.pushState({}, "", "/s/manage");
    allRoutes();
    mount("owner", { adminPath: "/s/manage" });
    expect(await screen.findByRole("heading", { name: "Manage storage backends" })).toBeInTheDocument();
  });

  it("lets an explicit `view` prop win over the address bar (for a future shell that passes one)", async () => {
    window.history.pushState({}, "", "/storage/admin");
    allRoutes();
    mount("owner", { view: "browse" });
    expect(await screen.findByText("Browse the storage backends registered in this workspace.")).toBeInTheDocument();
  });

  it("follows browser back/forward", async () => {
    window.history.pushState({}, "", "/storage");
    allRoutes();
    mount();
    await screen.findByText("Browse the storage backends registered in this workspace.");

    act(() => {
      window.history.pushState({}, "", "/storage/admin");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });
    expect(await screen.findByRole("heading", { name: "Manage storage backends" })).toBeInTheDocument();
  });
});

describe("moving between the two views", () => {
  it("switches in place with history navigation, without a page reload (which would drop the in-memory token)", async () => {
    window.history.pushState({}, "", "/storage");
    allRoutes();
    const user = userEvent.setup();
    mount();

    await user.click(await screen.findByRole("button", { name: "Manage backends" }));
    expect(window.location.pathname).toBe("/storage/admin");
    expect(await screen.findByRole("heading", { name: "Manage storage backends" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Back to storage" }));
    expect(window.location.pathname).toBe("/storage");
    expect(await screen.findByText("Browse the storage backends registered in this workspace.")).toBeInTheDocument();
  });

  it("uses the shell's own navigate function when one is supplied", async () => {
    window.history.pushState({}, "", "/storage");
    allRoutes();
    const onNavigate = vi.fn();
    const user = userEvent.setup();
    mount("owner", { onNavigate });

    await user.click(await screen.findByRole("button", { name: "Manage backends" }));
    expect(onNavigate).toHaveBeenCalledWith("/storage/admin");
    expect(window.location.pathname).toBe("/storage"); // left to the shell
  });

  it.each<WorkspaceRole>(["editor", "viewer"])(
    "a %s who reaches the admin URL directly gets an explanation, not the admin UI or its API calls",
    async (role) => {
      window.history.pushState({}, "", "/storage/admin");
      const m = mockFetch([]); // any request would throw
      mount(role);
      expect(await screen.findByText(/Only workspace owners can register/)).toBeInTheDocument();
      expect(m.requests).toHaveLength(0);
    },
  );
});

describe("props contract (ADR 0031 / 0033)", () => {
  it("applies the theme to its own root so it renders correctly wherever mounted", async () => {
    allRoutes();
    const { container } = render(<StorageApp workspace="acme" role="viewer" theme="dark" getAccessToken={() => "t"} view="browse" />);
    expect(container.firstElementChild).toHaveAttribute("data-theme", "dark");
    await screen.findByText("Browse the storage backends registered in this workspace.");
  });

  it("re-fetches when the workspace changes and uses the new workspace header", async () => {
    const m = allRoutes();
    const { rerender } = render(<StorageApp workspace="acme" role="viewer" theme="light" getAccessToken={() => "t"} view="browse" />);
    await waitFor(() => expect(m.calls("GET", /\/backends$/)).toHaveLength(1));
    expect(m.calls("GET", /\/backends$/)[0].headers.get("X-Workspace")).toBe("acme");

    rerender(<StorageApp workspace="globex" role="viewer" theme="light" getAccessToken={() => "t"} view="browse" />);
    await waitFor(() => expect(m.calls("GET", /\/backends$/)).toHaveLength(2));
    expect(m.calls("GET", /\/backends$/)[1].headers.get("X-Workspace")).toBe("globex");
  });

  it("calls getAccessToken for each request instead of caching one value", async () => {
    allRoutes();
    const getAccessToken = vi.fn(() => "t");
    render(<StorageApp workspace="acme" role="viewer" theme="light" getAccessToken={getAccessToken} view="browse" />);
    await screen.findByText("Browse the storage backends registered in this workspace.");
    await waitFor(() => expect(getAccessToken.mock.calls.length).toBeGreaterThanOrEqual(2)); // backends + first listing
  });
});
