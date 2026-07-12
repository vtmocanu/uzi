// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Notifications } from "./Notifications";
import { api, type Notification } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// Mock only the network + auth; keep the real notifications helpers so the
// title/body convention and the change event run for real.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: { listNotifications: vi.fn(), markNotificationRead: vi.fn(), unreadNotificationCount: vi.fn() },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

function aNotif(over: Partial<Notification> = {}): Notification {
  return {
    id: "ntf-1",
    kind: "judge_review",
    payload: { title: "Run review ready", body: "verdict: issues" },
    run_id: "run-done",
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

describe("Notifications inbox (PRD #46 M2)", () => {
  it("renders own notifications with the payload title/body and a run deep link", async () => {
    mockApi.listNotifications.mockResolvedValue({ notifications: [aNotif()], unread: 1, total: 1 });

    render(
      <MemoryRouter>
        <Notifications />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Run review ready")).toBeTruthy());
    expect(screen.getByText("verdict: issues")).toBeTruthy();
    const link = screen.getByText("· Open run").closest("a");
    expect(link?.getAttribute("href")).toBe("/runs/run-done");
    // Own view was requested (no all-scope).
    expect(mockApi.listNotifications).toHaveBeenCalledWith(undefined);
  });

  it("falls back to a humanized kind when the payload has no title", async () => {
    mockApi.listNotifications.mockResolvedValue({
      notifications: [aNotif({ payload: {} })],
      unread: 1,
      total: 1,
    });
    render(
      <MemoryRouter>
        <Notifications />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByText("judge review")).toBeTruthy());
  });

  it("marks a notification read and drops it from the unread state", async () => {
    mockApi.listNotifications.mockResolvedValue({ notifications: [aNotif()], unread: 1, total: 1 });
    mockApi.markNotificationRead.mockResolvedValue({
      notification: aNotif({ read_at: "2026-07-05T13:00:00Z" }),
    });

    render(
      <MemoryRouter>
        <Notifications />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Mark read")).toBeTruthy());
    fireEvent.click(screen.getByText("Mark read"));

    await waitFor(() => expect(mockApi.markNotificationRead).toHaveBeenCalledWith("ntf-1"));
    // The row flips to "read" and the mark-read control is gone.
    await waitFor(() => expect(screen.getByText("read")).toBeTruthy());
    expect(screen.queryByText("Mark read")).toBeNull();
  });

  it("hides the admin scope toggle for a non-admin", async () => {
    mockApi.listNotifications.mockResolvedValue({ notifications: [], unread: 0, total: 0 });
    render(
      <MemoryRouter>
        <Notifications />
      </MemoryRouter>,
    );
    await waitFor(() => expect(mockApi.listNotifications).toHaveBeenCalled());
    expect(screen.queryByText("All users")).toBeNull();
  });

  it("admin can switch to the all-users view, which shows the owner and requests all=1", async () => {
    vi.mocked(useAuth).mockReturnValue({ user: { id: "admin", is_admin: true } } as unknown as ReturnType<typeof useAuth>);
    mockApi.listNotifications
      .mockResolvedValueOnce({ notifications: [], unread: 0, total: 0 }) // initial own view
      .mockResolvedValueOnce({
        notifications: [aNotif({ owner: { id: "u-mira", email: "mira@uzi.local", display_name: "Mira Ionescu" } })],
        unread: 0,
        total: 1,
      });

    render(
      <MemoryRouter>
        <Notifications />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("All users")).toBeTruthy());
    fireEvent.click(screen.getByText("All users"));

    await waitFor(() => expect(screen.getByText("Mira Ionescu")).toBeTruthy());
    expect(mockApi.listNotifications).toHaveBeenLastCalledWith({ all: true });
    // Another user's row offers no mark-read control (own-row only).
    expect(screen.queryByText("Mark read")).toBeNull();
  });
});
