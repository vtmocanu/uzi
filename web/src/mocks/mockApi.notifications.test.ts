// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { mockAdmin, mockNotifications } from "./data";

// Each test re-imports a fresh mockApi (resetting the in-memory notifications
// state), so mutating tests (mark-read) don't bleed into the read-only ones.
async function freshApi() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

// The expectations are DERIVED from the fixture, not snapshotted from it.
//
// They used to be hardcoded ids and counts, and adding one demo row to `data.ts` for PRD
// #98 M5 turned five of these six tests red at once — none of them for a reason about
// mockApi. The property under test is the mock's SEMANTICS (own-scoping, newest-first,
// offset paging, unread bookkeeping); the fixture's shape is not the specification, and
// pinning it made data.ts unable to gain a row without an unrelated red. Deriving keeps the
// semantics asserted and lets the demo fixture grow, which it must — M3 requires it to
// render every state.
//
// Guard against the obvious objection (a test re-implementing the thing it tests): the sort
// below is an INDEPENDENT computation over the raw fixture, and the assertions still bind
// mockApi's output to it. What is no longer asserted is "the fixture contains exactly these
// four rows", which was never a property of mockApi.
const own = [...mockNotifications]
  .filter((n) => n.user_id === mockAdmin.id)
  .sort((a, b) => b.created_at.localeCompare(a.created_at));
const ownIDs = own.map((n) => n.id);
const ownUnread = own.filter((n) => !n.read_at).length;

afterEach(() => vi.resetModules());

describe("mockApi notifications (PRD #46 M2)", () => {
  // Fixture precondition. Every assertion below is vacuous or untestable without it —
  // paging needs more rows than a page, mark-read needs an unread row, and the all-view
  // needs a row belonging to somebody else.
  it("fixture is shaped so the assertions below can fail", () => {
    expect(own.length).toBeGreaterThan(2);
    expect(ownUnread).toBeGreaterThan(0);
    expect(mockNotifications.some((n) => n.user_id !== mockAdmin.id)).toBe(true);
  });

  it("lists the caller's own inbox newest-first with an unread count", async () => {
    const api = await freshApi();
    const res = await api.listNotifications();
    expect(res.notifications.map((n) => n.id)).toEqual(ownIDs);
    // Ordering asserted as a property too, so a fixture that happened to be authored in
    // descending order could not make the id comparison above pass by accident.
    const times = res.notifications.map((n) => n.created_at);
    expect([...times].sort((a, b) => b.localeCompare(a))).toEqual(times);
    expect(res.total).toBe(own.length);
    expect(res.unread).toBe(ownUnread);
    // Own-view rows carry no owner block, and no other user's row leaks in.
    expect(res.notifications.every((n) => n.owner === undefined)).toBe(true);
  });

  it("unread count matches the list envelope", async () => {
    const api = await freshApi();
    expect((await api.unreadNotificationCount()).unread).toBe(ownUnread);
  });

  it("admin all-view includes other users' rows with an owner block", async () => {
    const api = await freshApi();
    const res = await api.listNotifications({ all: true });
    expect(res.total).toBe(mockNotifications.length);
    const mira = res.notifications.find((n) => n.owner?.email === "mira@uzi.local");
    expect(mira).toBeTruthy();
    expect(mira?.owner?.id).toBe("u-mira");
  });

  it("clamps the page size and pages with offset", async () => {
    const api = await freshApi();
    const first = await api.listNotifications({ limit: 2 });
    expect(first.notifications.map((n) => n.id)).toEqual(ownIDs.slice(0, 2));
    expect(first.total).toBe(own.length);
    const next = await api.listNotifications({ limit: 2, offset: 2 });
    expect(next.notifications.map((n) => n.id)).toEqual(ownIDs.slice(2, 4));
  });

  it("marking read clears it from the unread count and is idempotent", async () => {
    const api = await freshApi();
    const target = own.find((n) => !n.read_at)!;
    const { notification } = await api.markNotificationRead(target.id);
    expect(notification.read_at).toBeTruthy();
    expect((await api.unreadNotificationCount()).unread).toBe(ownUnread - 1);
    // A second mark-read is a no-op, not an error.
    await api.markNotificationRead(target.id);
    expect((await api.unreadNotificationCount()).unread).toBe(ownUnread - 1);
  });

  it("mark-read on an unknown or foreign id is a 404", async () => {
    const api = await freshApi();
    const foreign = mockNotifications.find((n) => n.user_id !== mockAdmin.id)!;
    await expect(api.markNotificationRead(foreign.id)).rejects.toMatchObject({ status: 404 });
    await expect(api.markNotificationRead("nope")).rejects.toMatchObject({ status: 404 });
  });
});
