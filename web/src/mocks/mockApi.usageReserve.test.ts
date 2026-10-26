// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { mockApi } from "./mockApi";
import { mockUsers } from "./data";

afterEach(async () => {
  // Restore the seed so the shared session user does not leak into other tests.
  await mockApi.setUsageReserve(null, true);
  vi.resetModules();
});

// PRD #3522 M2: the mock twin of PUT /api/me/usage-reserve (handler/usage_reserve.go).
describe("mockApi.setUsageReserve (PRD #3522)", () => {
  it("seeds every demo user off, with pause-running at its default", () => {
    for (const u of mockUsers) {
      expect(u.weekly_reserve_pct, u.id).toBeNull();
      expect(u.weekly_reserve_pause_running, u.id).toBe(true);
    }
  });

  it("writes the pair onto the session user and echoes it in the user envelope", async () => {
    const { user } = await mockApi.setUsageReserve(70, false);
    expect(user.weekly_reserve_pct).toBe(70);
    expect(user.weekly_reserve_pause_running).toBe(false);
    const { user: me } = await mockApi.me();
    expect(me.weekly_reserve_pct).toBe(70);
    expect(me.weekly_reserve_pause_running).toBe(false);
  });

  it("projects the reserve into GET /me/settings, as the server reads it from the user row", async () => {
    await mockApi.setUsageReserve(85, true);
    const { settings } = await mockApi.getMySettings();
    expect(settings.weekly_reserve_pct).toBe(85);
    expect(settings.weekly_reserve_pause_running).toBe(true);
    await mockApi.setUsageReserve(null, false);
    const off = await mockApi.getMySettings();
    expect(off.settings.weekly_reserve_pct).toBeNull();
    expect(off.settings.weekly_reserve_pause_running).toBe(false);
  });

  it("accepts the bounds 1 and 100", async () => {
    expect((await mockApi.setUsageReserve(1, true)).user.weekly_reserve_pct).toBe(1);
    expect((await mockApi.setUsageReserve(100, true)).user.weekly_reserve_pct).toBe(100);
  });

  it.each([0, 101, 1.5, -1, Number.NaN])("refuses threshold %s with the server's 400 and keeps the stored value", async (pct) => {
    await mockApi.setUsageReserve(60, true);
    await expect(mockApi.setUsageReserve(pct, true)).rejects.toMatchObject({
      status: 400,
      message: "threshold_pct must be an integer from 1 to 100, or null",
    });
    expect((await mockApi.me()).user.weekly_reserve_pct).toBe(60);
  });

  it("refuses an absent field or a non-boolean pause_running, like the server", async () => {
    const loose = mockApi.setUsageReserve as unknown as (a?: unknown, b?: unknown) => Promise<unknown>;
    await expect(loose(undefined, true)).rejects.toMatchObject({ status: 400 });
    await expect(loose(70)).rejects.toMatchObject({
      status: 400,
      message: "pause_running is required (true or false)",
    });
    await expect(loose(70, "yes")).rejects.toMatchObject({
      status: 400,
      message: "pause_running must be true or false",
    });
    await expect(loose("70", true)).rejects.toMatchObject({ status: 400 });
  });
});
