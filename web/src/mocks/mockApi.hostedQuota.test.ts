// @vitest-environment jsdom
import { beforeEach, afterEach, describe, it, expect, vi } from "vitest";

async function reload() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}
beforeEach(() => { localStorage.clear(); vi.resetModules(); });
afterEach(() => vi.resetModules());

describe("mock hosted worker quota", () => {
  it("seeds 2, updates and reads back each boundary, and retains the quota after reload", async () => {
    let api = await reload();
    expect((await api.getSettings()).settings.hosted_worker_quota).toBe("2");
    for (const value of ["0", "20", "3"]) {
      expect((await api.updateSettings({ hosted_worker_quota: value })).settings.hosted_worker_quota).toBe(value);
      expect((await api.getSettings()).settings.hosted_worker_quota).toBe(value);
    }
    api = await reload();
    expect((await api.getSettings()).settings.hosted_worker_quota).toBe("3");
  });

  it.each(["-1", "21", "1.5", ""])("refuses %j and leaves the quota unchanged", async (value) => {
    const api = await reload();
    await expect(api.updateSettings({ hosted_worker_quota: value })).rejects.toMatchObject({ status: 400 });
    expect((await api.getSettings()).settings.hosted_worker_quota).toBe("2");
  });

  it("backfills a legacy persisted settings blob without discarding other edits", async () => {
    const api = await reload();
    await api.updateSettings({ uzi_label: "eligible" });
    const key = Array.from({ length: localStorage.length }, (_, i) => localStorage.key(i))
      .find((k) => k?.startsWith("uzi.mock."))!;
    expect(key).toBeTruthy();
    const blob = JSON.parse(localStorage.getItem(key)!);
    delete blob.appSettings.hosted_worker_quota;
    localStorage.setItem(key, JSON.stringify(blob));
    const next = await reload();
    expect((await next.getSettings()).settings).toMatchObject({ hosted_worker_quota: "2", uzi_label: "eligible" });
  });

  it("refuses a persisted non-string quota", async () => {
    const api = await reload();
    await api.updateSettings({ uzi_label: "eligible" });
    const key = Array.from({ length: localStorage.length }, (_, i) => localStorage.key(i))
      .find((k) => k?.startsWith("uzi.mock."))!;
    const blob = JSON.parse(localStorage.getItem(key)!);
    blob.appSettings.hosted_worker_quota = 3;
    localStorage.setItem(key, JSON.stringify(blob));
    const next = await reload();
    expect((await next.getSettings()).settings).toMatchObject({ hosted_worker_quota: "2", uzi_label: "uzi" });
  });
});
