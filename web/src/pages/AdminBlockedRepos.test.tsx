// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminBlockedRepos } from "./AdminBlockedRepos";
import { api, type BlockedRepo, type GuardrailOverrideRequest } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      adminListBlockedRepos: vi.fn(),
      setRepoGuardrailOverride: vi.fn(),
      clearRepoGuardrailOverride: vi.fn(),
      approveGuardrailOverrideRequest: vi.fn(),
      rejectGuardrailOverrideRequest: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

function blocked(over: Partial<BlockedRepo> = {}): BlockedRepo {
  return {
    id: "repo-blocked",
    path: "team/payments",
    owner_id: "u2",
    owner_email: "dana@x",
    forge_type: "gitlab",
    blocked: true,
    block_messages: ["the write role may push to protected main"],
    guardrail_override: null,
    privilege_status: "violations",
    privilege_checked_at: "2026-08-13T00:00:00Z",
    ...over,
  };
}

function overridden(over: Partial<BlockedRepo> = {}): BlockedRepo {
  return blocked({
    id: "repo-allowed",
    path: "team/atlas",
    owner_email: "vlad@x",
    blocked: false,
    block_messages: [],
    guardrail_override: { reason: "forge fix scheduled", by: "admin@x", at: daysAgoISO(3) },
    ...over,
  });
}

function daysAgoISO(n: number): string {
  return new Date(Date.now() - n * 24 * 60 * 60 * 1000).toISOString();
}

function request(over: Partial<GuardrailOverrideRequest> = {}): GuardrailOverrideRequest {
  return {
    id: "gor-1",
    repo_id: "repo-ledger",
    repo_path: "team-beta/ledger",
    owner_id: "u-mel",
    owner_email: "mel@x",
    forge_type: "gitlab",
    reason: "we fixed the push rule; please allow it",
    findings: [{ code: "write_role_can_push", severity: "block", message: "the write role may push to protected main" }],
    status: "pending",
    created_at: daysAgoISO(1),
    ...over,
  };
}

function rowFor(name: string): HTMLElement {
  const tr = screen.getByText(name).closest("tr");
  if (!tr) throw new Error(`no row for ${name}`);
  return tr;
}

beforeEach(() => {
  mockApi.adminListBlockedRepos.mockResolvedValue({ repos: [], checks_unknown: false, requests: [] });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderPage() {
  return render(
    <MemoryRouter>
      <AdminBlockedRepos />
    </MemoryRouter>,
  );
}

describe("AdminBlockedRepos", () => {
  it("renders a blocked row with its findings and an overridden row with reason/actor", async () => {
    mockApi.adminListBlockedRepos.mockResolvedValue({
      repos: [blocked(), overridden()],
      checks_unknown: false,
      requests: [],
    });
    renderPage();
    await screen.findByText("team/payments");

    const b = within(rowFor("team/payments"));
    expect(b.getByText(/runs blocked/i)).toBeTruthy();
    expect(b.getByText(/the write role may push to protected main/i)).toBeTruthy();
    expect(b.getByRole("button", { name: /allow anyway/i })).toBeTruthy();
    expect(b.getByText("dana@x")).toBeTruthy();

    const o = within(rowFor("team/atlas"));
    expect(o.getByText(/allowed by admin/i)).toBeTruthy();
    expect(o.getByText(/forge fix scheduled/i)).toBeTruthy();
    expect(o.getByText(/by admin@x/i)).toBeTruthy();
    expect(o.getByRole("button", { name: /revoke/i })).toBeTruthy();
  });

  it("flags an override older than 30 days as stale", async () => {
    mockApi.adminListBlockedRepos.mockResolvedValue({
      repos: [overridden({ guardrail_override: { reason: "old", by: "a@x", at: daysAgoISO(40) } })],
      checks_unknown: false,
      requests: [],
    });
    renderPage();
    await screen.findByText("team/atlas");
    expect(within(rowFor("team/atlas")).getByText(/stale/i)).toBeTruthy();
  });

  it("shows the R1 'unknown, not none' caveat when checks_unknown is true", async () => {
    mockApi.adminListBlockedRepos.mockResolvedValue({ repos: [], checks_unknown: true, requests: [] });
    renderPage();
    await waitFor(() => expect(screen.getByText(/unknown, not none blocked/i)).toBeTruthy());
  });

  it("Allow-anyway requires a reason, then POSTs and refetches", async () => {
    mockApi.adminListBlockedRepos.mockResolvedValue({ repos: [blocked()], checks_unknown: false, requests: [] });
    mockApi.setRepoGuardrailOverride.mockResolvedValue({ repo: {} as never });
    renderPage();
    await screen.findByText("team/payments");
    fireEvent.click(within(rowFor("team/payments")).getByRole("button", { name: /allow anyway/i }));

    const dialog = await screen.findByRole("dialog");
    const allowBtn = within(dialog).getByRole("button", { name: /allow anyway/i });
    expect((allowBtn as HTMLButtonElement).disabled).toBe(true);
    // The exact finding is named in the modal.
    expect(within(dialog).getByText(/the write role may push to protected main/i)).toBeTruthy();

    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "accepting the risk" } });
    expect((allowBtn as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(allowBtn);
    await waitFor(() =>
      expect(mockApi.setRepoGuardrailOverride).toHaveBeenCalledWith("repo-blocked", "accepting the risk"),
    );
    await waitFor(() => expect(mockApi.adminListBlockedRepos).toHaveBeenCalledTimes(2));
  });

  it("closes the Allow-anyway modal on Escape (a11y: shared Modal)", async () => {
    mockApi.adminListBlockedRepos.mockResolvedValue({ repos: [blocked()], checks_unknown: false, requests: [] });
    renderPage();
    await screen.findByText("team/payments");
    fireEvent.click(within(rowFor("team/payments")).getByRole("button", { name: /allow anyway/i }));

    const dialog = await screen.findByRole("dialog");
    // On open, focus is inside the dialog (never left on the page behind the backdrop).
    expect(dialog.contains(document.activeElement)).toBe(true);

    fireEvent.keyDown(dialog, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("Revoke clears the override and refetches", async () => {
    mockApi.adminListBlockedRepos.mockResolvedValue({ repos: [overridden()], checks_unknown: false, requests: [] });
    mockApi.clearRepoGuardrailOverride.mockResolvedValue({ repo: {} as never });
    renderPage();
    await screen.findByText("team/atlas");
    fireEvent.click(within(rowFor("team/atlas")).getByRole("button", { name: /revoke/i }));
    await waitFor(() => expect(mockApi.clearRepoGuardrailOverride).toHaveBeenCalledWith("repo-allowed"));
    await waitFor(() => expect(mockApi.adminListBlockedRepos).toHaveBeenCalledTimes(2));
  });
});

describe("AdminBlockedRepos — pending override requests (issue #1432)", () => {
  it("lists a pending request with its reason, owner, and coded findings", async () => {
    mockApi.adminListBlockedRepos.mockResolvedValue({
      repos: [],
      checks_unknown: false,
      requests: [request()],
    });
    renderPage();
    await screen.findByText(/pending override requests/i);
    // The request's owner, repo, reason and finding message all render.
    expect(screen.getByText("mel@x")).toBeTruthy();
    expect(screen.getByText("team-beta/ledger")).toBeTruthy();
    expect(screen.getByText(/we fixed the push rule/i)).toBeTruthy();
    expect(screen.getByText(/the write role may push to protected main/i)).toBeTruthy();
    // The copy makes clear approving does not enable the repo for the owner.
    expect(screen.getByText(/owner can retry Enable/i)).toBeTruthy();
  });

  it("Approve opens a note modal — the row click does not decide the request", async () => {
    mockApi.adminListBlockedRepos.mockResolvedValue({
      repos: [],
      checks_unknown: false,
      requests: [request({ id: "gor-42" })],
    });
    renderPage();
    await screen.findByText(/pending override requests/i);
    fireEvent.click(screen.getByRole("button", { name: /^Approve$/ }));

    expect(await screen.findByRole("dialog", { name: /approve the request/i })).toBeTruthy();
    expect(mockApi.approveGuardrailOverrideRequest).not.toHaveBeenCalled();
  });

  it("Approve WITH a note calls approveGuardrailOverrideRequest with (id, note) and reloads", async () => {
    mockApi.adminListBlockedRepos.mockResolvedValue({
      repos: [],
      checks_unknown: false,
      requests: [request({ id: "gor-42" })],
    });
    mockApi.approveGuardrailOverrideRequest.mockResolvedValue({ request: {} as never });
    renderPage();
    await screen.findByText(/pending override requests/i);
    fireEvent.click(screen.getByRole("button", { name: /^Approve$/ }));

    const dialog = await screen.findByRole("dialog", { name: /approve the request/i });
    // The note is OPTIONAL: the Approve confirm is enabled even with an empty field.
    const approveBtn = within(dialog).getByRole("button", { name: /^Approve$/ });
    expect((approveBtn as HTMLButtonElement).disabled).toBe(false);

    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "fixed, allowing it" } });
    fireEvent.click(approveBtn);
    await waitFor(() =>
      expect(mockApi.approveGuardrailOverrideRequest).toHaveBeenCalledWith("gor-42", "fixed, allowing it"),
    );
    await waitFor(() => expect(mockApi.adminListBlockedRepos).toHaveBeenCalledTimes(2));
  });

  it("Approve with an EMPTY note omits decision_note (id, undefined)", async () => {
    mockApi.adminListBlockedRepos.mockResolvedValue({
      repos: [],
      checks_unknown: false,
      requests: [request({ id: "gor-45" })],
    });
    mockApi.approveGuardrailOverrideRequest.mockResolvedValue({ request: {} as never });
    renderPage();
    await screen.findByText(/pending override requests/i);
    fireEvent.click(screen.getByRole("button", { name: /^Approve$/ }));

    const dialog = await screen.findByRole("dialog", { name: /approve the request/i });
    fireEvent.click(within(dialog).getByRole("button", { name: /^Approve$/ }));
    await waitFor(() =>
      expect(mockApi.approveGuardrailOverrideRequest).toHaveBeenCalledWith("gor-45", undefined),
    );
  });

  it("Reject opens a note modal, sends the optional note, and reloads", async () => {
    mockApi.adminListBlockedRepos.mockResolvedValue({
      repos: [],
      checks_unknown: false,
      requests: [request({ id: "gor-43" })],
    });
    mockApi.rejectGuardrailOverrideRequest.mockResolvedValue({ request: {} as never });
    renderPage();
    await screen.findByText(/pending override requests/i);
    fireEvent.click(screen.getByRole("button", { name: /^Reject$/ }));

    const dialog = await screen.findByRole("dialog", { name: /reject the request/i });
    // The note is OPTIONAL: the Reject button is enabled even with an empty field.
    const rejectBtn = within(dialog).getByRole("button", { name: /reject request/i });
    expect((rejectBtn as HTMLButtonElement).disabled).toBe(false);

    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "protection still open" } });
    fireEvent.click(rejectBtn);
    await waitFor(() =>
      expect(mockApi.rejectGuardrailOverrideRequest).toHaveBeenCalledWith("gor-43", "protection still open"),
    );
    await waitFor(() => expect(mockApi.adminListBlockedRepos).toHaveBeenCalledTimes(2));
  });

  it("Reject with an empty note omits decision_note", async () => {
    mockApi.adminListBlockedRepos.mockResolvedValue({
      repos: [],
      checks_unknown: false,
      requests: [request({ id: "gor-44" })],
    });
    mockApi.rejectGuardrailOverrideRequest.mockResolvedValue({ request: {} as never });
    renderPage();
    await screen.findByText(/pending override requests/i);
    fireEvent.click(screen.getByRole("button", { name: /^Reject$/ }));

    const dialog = await screen.findByRole("dialog", { name: /reject the request/i });
    fireEvent.click(within(dialog).getByRole("button", { name: /reject request/i }));
    await waitFor(() =>
      expect(mockApi.rejectGuardrailOverrideRequest).toHaveBeenCalledWith("gor-44", undefined),
    );
  });
});
