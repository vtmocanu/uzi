// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";

// Each test re-imports a fresh mockApi (resetting the in-memory notifications
// state), so mutating tests (mark-read) don't bleed into the read-only ones.
async function freshApi() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

afterEach(() => vi.resetModules());

describe("mockApi notifications (PRD #46 M2)", () => {
  it("lists the caller's own inbox newest-first with an unread count", async () => {
    const api = await freshApi();
    const res = await api.listNotifications();
    // The seeded admin owns three rows; the fourth belongs to another user.
    expect(res.notifications.map((n) => n.id)).toEqual(["ntf-1", "ntf-2", "ntf-3"]);
    expect(res.total).toBe(3);
    expect(res.unread).toBe(2);
    // Own-view rows carry no owner block.
    expect(res.notifications.every((n) => n.owner === undefined)).toBe(true);
  });

  it("unread count matches the list envelope", async () => {
    const api = await freshApi();
    expect((await api.unreadNotificationCount()).unread).toBe(2);
  });

  it("admin all-view includes other users' rows with an owner block", async () => {
    const api = await freshApi();
    const res = await api.listNotifications({ all: true });
    expect(res.total).toBe(4);
    const mira = res.notifications.find((n) => n.owner?.email === "mira@uzi.local");
    expect(mira).toBeTruthy();
    expect(mira?.owner?.id).toBe("u-mira");
  });

  it("clamps the page size and pages with offset", async () => {
    const api = await freshApi();
    const first = await api.listNotifications({ limit: 2 });
    expect(first.notifications.map((n) => n.id)).toEqual(["ntf-1", "ntf-2"]);
    expect(first.total).toBe(3);
    const next = await api.listNotifications({ limit: 2, offset: 2 });
    expect(next.notifications.map((n) => n.id)).toEqual(["ntf-3"]);
  });

  it("marking read clears it from the unread count and is idempotent", async () => {
    const api = await freshApi();
    const { notification } = await api.markNotificationRead("ntf-1");
    expect(notification.read_at).toBeTruthy();
    expect((await api.unreadNotificationCount()).unread).toBe(1);
    // A second mark-read is a no-op, not an error.
    await api.markNotificationRead("ntf-1");
    expect((await api.unreadNotificationCount()).unread).toBe(1);
  });

  it("mark-read on an unknown or foreign id is a 404", async () => {
    const api = await freshApi();
    // ntf-4 belongs to u-mira, not the seeded admin → not found for this caller.
    await expect(api.markNotificationRead("ntf-4")).rejects.toMatchObject({ status: 404 });
    await expect(api.markNotificationRead("nope")).rejects.toMatchObject({ status: 404 });
  });
});
