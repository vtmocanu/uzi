// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminEgressProfiles } from "./AdminEgressProfiles";
import { api, type EgressProfile, type EgressProfileProblem } from "../lib/api";
import { ApiError } from "../lib/apiError";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      adminListEgressProfiles: vi.fn(),
      adminCreateEgressProfile: vi.fn(),
      adminUpdateEgressProfile: vi.fn(),
      adminDeleteEgressProfile: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

function profile(over: Partial<EgressProfile> & Pick<EgressProfile, "id" | "name">): EgressProfile {
  return {
    description: "",
    hosts: [],
    multi_publisher_override: [],
    warnings: [],
    created_by: null,
    updated_by: null,
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
    ...over,
  };
}

const OVERRIDE_MSG =
  "github.com is code hosting: many publishers share this host; admitted by an explicit override, so every publisher on it is reachable";

const KERNEL = profile({
  id: "ep-1",
  name: "linux-kernel-sources",
  description: "Kernel release notes",
  hosts: ["www.kernel.org", "github.com"],
  multi_publisher_override: ["github.com"],
  warnings: [{ entry: "github.com", code: "multi_publisher_override", message: OVERRIDE_MSG }],
});
const VENDOR = profile({
  id: "ep-2",
  name: "vendor-x-docs",
  description: "Vendor X documentation",
  hosts: ["docs.vendor-x.com", "*.cdn.vendor-x.com"],
});

function invalid(problems: EgressProfileProblem[]): ApiError {
  return new ApiError(422, "the egress profile is invalid: see problems", {
    error: "the egress profile is invalid: see problems",
    reason: "invalid_egress_profile",
    problems,
  });
}

function renderPage() {
  return render(
    <MemoryRouter>
      <AdminEgressProfiles />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockApi.adminListEgressProfiles.mockResolvedValue({ egress_profiles: [KERNEL, VENDOR] });
});
afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

async function openCreate() {
  renderPage();
  await screen.findByText("linux-kernel-sources");
  fireEvent.click(screen.getByRole("button", { name: "New site list" }));
  return screen.getByRole("form", { name: "New site list" });
}

describe("AdminEgressProfiles", () => {
  it("lists every site list with its hosts and its stored warnings", async () => {
    renderPage();
    expect(await screen.findByText("linux-kernel-sources")).toBeTruthy();
    expect(screen.getByText("vendor-x-docs")).toBeTruthy();
    const vendorHosts = screen.getByRole("list", { name: "Hosts in vendor-x-docs" });
    expect(within(vendorHosts).getByText("*.cdn.vendor-x.com")).toBeTruthy();
    // The override warning is shown on the row, with the open-host badge.
    expect(screen.getByText(OVERRIDE_MSG, { exact: false })).toBeTruthy();
    expect(screen.getByText("1 open host")).toBeTruthy();
  });

  it("offers a create action from the empty state", async () => {
    mockApi.adminListEgressProfiles.mockResolvedValue({ egress_profiles: [] });
    renderPage();
    expect(await screen.findByText("No site lists yet")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "New site list" }));
    expect(screen.getByRole("form", { name: "New site list" })).toBeTruthy();
  });

  it("sends a create with one host per line and pins each 422 problem to its entry and field", async () => {
    mockApi.adminCreateEgressProfile.mockRejectedValue(
      invalid([
        { field: "name", code: "invalid_name", message: "name must be 1-64 characters of lowercase letters" },
        { field: "hosts[1]", entry: "https://bad.example.com", code: "scheme", message: "list a host name, not a URL" },
      ]),
    );
    const form = await openCreate();
    fireEvent.change(within(form).getByLabelText("Name"), { target: { value: "Bad Name" } });
    fireEvent.change(within(form).getByLabelText("Hosts, one per line"), {
      target: { value: "docs.example.com\n\n  https://bad.example.com  \n" },
    });
    fireEvent.click(within(form).getByRole("button", { name: "Create site list" }));

    await waitFor(() =>
      expect(mockApi.adminCreateEgressProfile).toHaveBeenCalledWith({
        name: "Bad Name",
        description: "",
        hosts: ["docs.example.com", "https://bad.example.com"],
        multi_publisher_override: [],
      }),
    );
    expect(await within(form).findByText("2 problems to fix before this site list can be saved. Nothing was stored.")).toBeTruthy();
    expect(within(form).getByText("name must be 1-64 characters of lowercase letters")).toBeTruthy();
    // The entry problem sits in the ledger row of the entry it names, which keeps the
    // textarea's line number (line 3: the blank line 2 is skipped, not renumbered).
    const row = within(form).getByText("list a host name, not a URL").closest("li")!;
    expect(within(row).getByText("https://bad.example.com")).toBeTruthy();
    expect(row.textContent).toContain("Line 3");
    expect(within(row).getByText("Refused")).toBeTruthy();
  });

  it("requires an explicit per-entry tick before a multi-publisher host is sent as an override", async () => {
    const MP_MSG =
      "github.com is code hosting: many publishers share this host; allowing it admits every publisher on it. Add it to multi_publisher_override to accept that";
    mockApi.adminCreateEgressProfile
      .mockRejectedValueOnce(
        invalid([{ field: "hosts[1]", entry: "github.com", code: "multi_publisher_needs_override", message: MP_MSG }]),
      )
      // The second refusal is about something else entirely: it no longer names github.com.
      .mockRejectedValueOnce(
        invalid([{ field: "name", code: "invalid_name", message: "name must be 1-64 characters of lowercase letters" }]),
      )
      .mockResolvedValueOnce({
        egress_profile: profile({ id: "ep-new", name: "kernel", hosts: ["docs.kernel.org", "github.com"] }),
      });
    const form = await openCreate();
    fireEvent.change(within(form).getByLabelText("Name"), { target: { value: "kernel" } });
    fireEvent.change(within(form).getByLabelText("Hosts, one per line"), {
      target: { value: "docs.kernel.org\ngithub.com" },
    });
    fireEvent.click(within(form).getByRole("button", { name: "Create site list" }));

    const box = await within(form).findByRole("checkbox", { name: "Allow every publisher on github.com" });
    expect((box as HTMLInputElement).checked).toBe(false);
    // The server's reason is the checkbox's description, read with it by a screen reader.
    const describedBy = box.getAttribute("aria-describedby")!;
    expect(document.getElementById(describedBy)?.textContent).toBe(MP_MSG);
    expect(within(form).getByText("Needs your decision")).toBeTruthy();
    // Only the flagged entry gets a consent control.
    expect(within(form).getAllByRole("checkbox")).toHaveLength(1);

    fireEvent.click(box);
    // A ticked consent is a decision made, not a problem left: the summary stops counting it.
    expect(within(form).queryByText(/problems? to fix/)).toBeNull();
    fireEvent.click(within(form).getByRole("button", { name: "Create site list" }));
    await waitFor(() => expect(mockApi.adminCreateEgressProfile).toHaveBeenCalledTimes(2));
    expect(await within(form).findByText("name must be 1-64 characters of lowercase letters")).toBeTruthy();
    expect(within(form).getByText("1 problem to fix before this site list can be saved. Nothing was stored.")).toBeTruthy();
    // The consent the admin gave stays visible, still ticked, with its reason, even though
    // the latest refusal does not mention the entry: an override is never sent unseen.
    const kept = within(form).getByRole("checkbox", { name: "Allow every publisher on github.com" });
    expect((kept as HTMLInputElement).checked).toBe(true);
    expect(document.getElementById(kept.getAttribute("aria-describedby")!)?.textContent).toBe(MP_MSG);
    expect(within(form).getByText("Every publisher allowed")).toBeTruthy();

    fireEvent.change(within(form).getByLabelText("Name"), { target: { value: "kernel" } });
    fireEvent.click(within(form).getByRole("button", { name: "Create site list" }));
    await waitFor(() => expect(mockApi.adminCreateEgressProfile).toHaveBeenCalledTimes(3));
    expect(mockApi.adminCreateEgressProfile.mock.calls[0][0].multi_publisher_override).toEqual([]);
    expect(mockApi.adminCreateEgressProfile.mock.calls[2][0]).toEqual({
      name: "kernel",
      description: "",
      hosts: ["docs.kernel.org", "github.com"],
      multi_publisher_override: ["github.com"],
    });
    expect(await screen.findByText("Site list kernel created.")).toBeTruthy();
    expect(mockApi.adminListEgressProfiles).toHaveBeenCalledTimes(2);
  });

  it("a ticked consent for a deleted-then-re-added entry comes back visible, never sent unseen", async () => {
    const MP_MSG = "github.com is code hosting: many publishers share this host";
    mockApi.adminCreateEgressProfile
      .mockRejectedValueOnce(
        invalid([
          { field: "hosts[0]", entry: "github.com", code: "multi_publisher_needs_override", message: MP_MSG },
          { field: "hosts[1]", entry: "bad://x", code: "scheme", message: "no scheme" },
        ]),
      )
      // github.com is no longer listed, so this refusal names only the bad entry.
      .mockRejectedValueOnce(invalid([{ field: "hosts[0]", entry: "bad://x", code: "scheme", message: "no scheme" }]))
      .mockResolvedValueOnce({ egress_profile: profile({ id: "ep-new", name: "kernel", hosts: ["github.com"] }) });
    const form = await openCreate();
    fireEvent.change(within(form).getByLabelText("Name"), { target: { value: "kernel" } });
    const hostsField = within(form).getByLabelText("Hosts, one per line");
    fireEvent.change(hostsField, { target: { value: "github.com\nbad://x" } });
    fireEvent.click(within(form).getByRole("button", { name: "Create site list" }));
    fireEvent.click(await within(form).findByRole("checkbox", { name: "Allow every publisher on github.com" }));

    fireEvent.change(hostsField, { target: { value: "bad://x" } });
    fireEvent.click(within(form).getByRole("button", { name: "Create site list" }));
    await waitFor(() => expect(mockApi.adminCreateEgressProfile).toHaveBeenCalledTimes(2));
    await within(form).findByText("no scheme");

    fireEvent.change(hostsField, { target: { value: "github.com" } });
    const back = within(form).getByRole("checkbox", { name: "Allow every publisher on github.com" });
    expect((back as HTMLInputElement).checked).toBe(true);
    expect(document.getElementById(back.getAttribute("aria-describedby")!)?.textContent).toBe(MP_MSG);
    fireEvent.click(within(form).getByRole("button", { name: "Create site list" }));
    await waitFor(() => expect(mockApi.adminCreateEgressProfile).toHaveBeenCalledTimes(3));
    expect(mockApi.adminCreateEgressProfile.mock.calls[2][0].multi_publisher_override).toEqual(["github.com"]);
  });

  it("edits a stored list: the stored override is pre-ticked, and unticking drops it from the PUT", async () => {
    mockApi.adminUpdateEgressProfile.mockResolvedValue({ egress_profile: KERNEL });
    renderPage();
    await screen.findByText("linux-kernel-sources");
    fireEvent.click(screen.getByRole("button", { name: "Edit linux-kernel-sources" }));
    const form = screen.getByRole("form", { name: "Edit linux-kernel-sources" });
    // Name is immutable: no name input in edit mode.
    expect(within(form).queryByLabelText("Name")).toBeNull();
    expect(within(form).getByLabelText("Description (optional)")).toBeTruthy();

    const box = within(form).getByRole("checkbox", { name: "Allow every publisher on github.com" });
    expect((box as HTMLInputElement).checked).toBe(true);
    fireEvent.click(box);
    fireEvent.click(within(form).getByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(mockApi.adminUpdateEgressProfile).toHaveBeenCalledWith("linux-kernel-sources", {
        description: "Kernel release notes",
        hosts: ["www.kernel.org", "github.com"],
        multi_publisher_override: [],
      }),
    );
  });

  it("deletes only after the inline confirm, which takes focus", async () => {
    mockApi.adminDeleteEgressProfile.mockResolvedValue(null);
    renderPage();
    await screen.findByText("vendor-x-docs");
    fireEvent.click(screen.getByRole("button", { name: "Delete vendor-x-docs" }));
    expect(mockApi.adminDeleteEgressProfile).not.toHaveBeenCalled();

    const confirm = screen.getByRole("group", { name: "Confirm deleting vendor-x-docs" });
    const confirmBtn = within(confirm).getByRole("button", { name: "Delete site list" });
    await waitFor(() => expect(document.activeElement).toBe(confirmBtn));
    fireEvent.click(confirmBtn);
    await waitFor(() => expect(mockApi.adminDeleteEgressProfile).toHaveBeenCalledWith("vendor-x-docs"));
    expect(await screen.findByText("Site list vendor-x-docs deleted.")).toBeTruthy();
  });

  it("cancelling the confirm deletes nothing", async () => {
    renderPage();
    await screen.findByText("vendor-x-docs");
    fireEvent.click(screen.getByRole("button", { name: "Delete vendor-x-docs" }));
    const confirm = screen.getByRole("group", { name: "Confirm deleting vendor-x-docs" });
    fireEvent.click(within(confirm).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("group", { name: "Confirm deleting vendor-x-docs" })).toBeNull();
    expect(screen.getByRole("button", { name: "Delete vendor-x-docs" })).toBeTruthy();
    expect(mockApi.adminDeleteEgressProfile).not.toHaveBeenCalled();
  });

  it("shows a non-422 failure (a taken name) as a banner", async () => {
    mockApi.adminCreateEgressProfile.mockRejectedValue(
      new ApiError(409, "an egress profile with that name already exists"),
    );
    const form = await openCreate();
    fireEvent.change(within(form).getByLabelText("Name"), { target: { value: "vendor-x-docs" } });
    fireEvent.change(within(form).getByLabelText("Hosts, one per line"), { target: { value: "a.example.com" } });
    fireEvent.click(within(form).getByRole("button", { name: "Create site list" }));
    expect(await within(form).findByText("an egress profile with that name already exists")).toBeTruthy();
  });

  it("shows a 422 with no usable problems as a banner, so a refused save is never silent", async () => {
    mockApi.adminCreateEgressProfile.mockRejectedValue(invalid([]));
    const form = await openCreate();
    fireEvent.change(within(form).getByLabelText("Name"), { target: { value: "vendor" } });
    fireEvent.change(within(form).getByLabelText("Hosts, one per line"), { target: { value: "a.example.com" } });
    fireEvent.click(within(form).getByRole("button", { name: "Create site list" }));
    expect(await within(form).findByText("the egress profile is invalid: see problems")).toBeTruthy();
  });

  it("renders server strings in the editor as text, never as markup", async () => {
    const payload = '<img src=x onerror="alert(1)">';
    mockApi.adminListEgressProfiles.mockResolvedValue({
      egress_profiles: [
        profile({
          id: "ep-x",
          name: "odd",
          hosts: ["github.com", payload],
          warnings: [
            { entry: "github.com", code: "multi_publisher_needs_override", message: `mp ${payload}` },
            { entry: payload, code: "stale_entry", message: `stale ${payload}` },
          ],
        }),
      ],
    });
    mockApi.adminUpdateEgressProfile.mockRejectedValue(
      invalid([
        { field: "description", code: "unsafe_text", message: `desc ${payload}` },
        { field: "hosts[1]", entry: payload, code: "invalid_host", message: `host ${payload}` },
        { field: "something_new", code: "other", message: `other ${payload}` },
      ]),
    );
    const { container } = renderPage();
    await screen.findByText("odd");
    fireEvent.click(screen.getByRole("button", { name: "Edit odd" }));
    const form = screen.getByRole("form", { name: "Edit odd" });
    // Stored-entry warnings: the multi-publisher reason and the stale warning, plus the
    // echoed entry itself in the ledger.
    const box = within(form).getByRole("checkbox", { name: "Allow every publisher on github.com" });
    expect(document.getElementById(box.getAttribute("aria-describedby")!)?.textContent).toBe(`mp ${payload}`);
    expect(within(form).getByText(`stale ${payload}`)).toBeTruthy();
    const staleRow = within(form).getByText(`stale ${payload}`).closest("li")!;
    expect(within(staleRow).getByText(payload)).toBeTruthy();

    fireEvent.click(within(form).getByRole("button", { name: "Save changes" }));
    // Problem messages: a field problem, an entry problem in the ledger, an unplaced one.
    expect(await within(form).findByText(`desc ${payload}`)).toBeTruthy();
    expect(within(form).getByText(`host ${payload}`).closest("li")).toBe(staleRow);
    expect(within(form).getByText(`other ${payload}`)).toBeTruthy();
    expect(container.querySelector("img")).toBeNull();
  });

  it("renders server strings as text, never as markup", async () => {
    const payload = '<img src=x onerror="alert(1)">';
    mockApi.adminListEgressProfiles.mockResolvedValue({
      egress_profiles: [
        profile({
          id: "ep-x",
          name: "odd",
          description: `desc ${payload}`,
          hosts: ["forum.example.com"],
          warnings: [{ entry: "forum.example.com", code: "stale_entry", message: `msg ${payload}` }],
        }),
      ],
    });
    const { container } = renderPage();
    // Positive half: the literal markup is visible as text...
    expect(await screen.findByText(`desc ${payload}`)).toBeTruthy();
    expect(screen.getByText(`msg ${payload}`, { exact: false })).toBeTruthy();
    // ...paired with the negative half: no element was created from it. The row's host
    // chips render as <li>, so this is not an empty container.
    expect(container.querySelectorAll("li").length).toBeGreaterThan(0);
    expect(container.querySelector("img")).toBeNull();
  });
});
