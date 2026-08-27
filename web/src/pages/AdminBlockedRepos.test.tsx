// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminBlockedRepos } from "./AdminBlockedRepos";
import { api, type BlockedRepo } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      adminListBlockedRepos: vi.fn(),
      setRepoGuardrailOverride: vi.fn(),
      clearRepoGuardrailOverride: vi.fn(),
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

function rowFor(name: string): HTMLElement {
  const tr = screen.getByText(name).closest("tr");
  if (!tr) throw new Error(`no row for ${name}`);
  return tr;
}

beforeEach(() => {
  mockApi.adminListBlockedRepos.mockResolvedValue({ repos: [], checks_unknown: false });
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
    });
    renderPage();
    await screen.findByText("team/atlas");
    expect(within(rowFor("team/atlas")).getByText(/stale/i)).toBeTruthy();
  });

  it("shows the R1 'unknown, not none' caveat when checks_unknown is true", async () => {
    mockApi.adminListBlockedRepos.mockResolvedValue({ repos: [], checks_unknown: true });
    renderPage();
    await waitFor(() => expect(screen.getByText(/unknown, not none blocked/i)).toBeTruthy());
  });

  it("Allow-anyway requires a reason, then POSTs and refetches", async () => {
    mockApi.adminListBlockedRepos.mockResolvedValue({ repos: [blocked()], checks_unknown: false });
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
    mockApi.adminListBlockedRepos.mockResolvedValue({ repos: [blocked()], checks_unknown: false });
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
    mockApi.adminListBlockedRepos.mockResolvedValue({ repos: [overridden()], checks_unknown: false });
    mockApi.clearRepoGuardrailOverride.mockResolvedValue({ repo: {} as never });
    renderPage();
    await screen.findByText("team/atlas");
    fireEvent.click(within(rowFor("team/atlas")).getByRole("button", { name: /revoke/i }));
    await waitFor(() => expect(mockApi.clearRepoGuardrailOverride).toHaveBeenCalledWith("repo-allowed"));
    await waitFor(() => expect(mockApi.adminListBlockedRepos).toHaveBeenCalledTimes(2));
  });
});
