// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Notifications } from "./Notifications";
import { api, type Notification } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// Mock only the network + auth; the real notifications helpers (notificationLink, grouping)
// run for real so the kind-conditional deep-link is exercised end to end.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: { listNotifications: vi.fn(), markNotificationRead: vi.fn(), unreadNotificationCount: vi.fn() },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

function findingNotif(over: Partial<Notification> = {}): Notification {
  return {
    id: "ntf-find",
    kind: "incidental_finding",
    payload: { run_id: "run-live", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi", count: 2, finding_ids: ["f1", "f2"] },
    run_id: "run-live",
    review_id: null,
    read_at: null,
    created_at: "2026-07-05T12:00:00Z",
    ...over,
  };
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ user: { id: "u1", is_admin: false } } as unknown as ReturnType<typeof useAuth>);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderInbox() {
  return render(
    <MemoryRouter>
      <Notifications />
    </MemoryRouter>,
  );
}

describe("incidental_finding notification renderer (PRD #333 M7)", () => {
  it("shows the count, the repo_path, and a 'Review findings' deep-link to the run", async () => {
    mockApi.listNotifications.mockResolvedValue({ notifications: [findingNotif()], unread: 1, total: 1 });
    renderInbox();

    await waitFor(() => expect(screen.getByText("Run flagged 2 findings")).toBeTruthy());
    expect(screen.getByText("vtmocanu/uzi")).toBeTruthy();
    const link = screen.getByRole("link", { name: /Review findings/ });
    expect(link.getAttribute("href")).toBe("/findings?run=run-live");
  });

  it("singularizes the count and renders the repo_path INERT (no bidi/control char in the DOM)", async () => {
    const RLO = "\u202E";
    const ZWSP = "\u200B";
    mockApi.listNotifications.mockResolvedValue({
      notifications: [
        findingNotif({ payload: { run_id: "run-live", repo_path: `ai-${RLO}team/${ZWSP}uzi`, count: 1, finding_ids: ["f1"] } }),
      ],
      unread: 1,
      total: 1,
    });
    const { container } = renderInbox();

    await waitFor(() => expect(screen.getByText("Run flagged 1 finding")).toBeTruthy());
    expect(container.textContent ?? "").not.toMatch(/[\p{Cc}\p{Cf}]/u);
  });
});
