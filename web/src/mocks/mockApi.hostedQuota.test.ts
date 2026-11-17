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

describe("mock worker size, cap and lease settings (PRD #4212)", () => {
  it("seeds the defaults with source default, then saves, reports db and resets", async () => {
    const api = await reload();
    const first = await api.getSettings();
    expect(first.settings).toMatchObject({ ephemeral_max_per_user: "2", ephemeral_default_size: "m", hosted_default_size: "l", ephemeral_lease: "2h" });
    expect(first.sources.hosted_default_size).toBe("default");
    const saved = await api.updateSettings({ hosted_default_size: "s", ephemeral_lease: "30m" });
    expect(saved.settings).toMatchObject({ hosted_default_size: "s", ephemeral_lease: "30m" });
    expect(saved.sources.hosted_default_size).toBe("db");
    expect(await api.hostedConfig()).toMatchObject({ default_size: "s" });
    const reset = await api.updateSettings({ hosted_default_size: "" });
    expect(reset.settings.hosted_default_size).toBe("l");
    expect(reset.sources.hosted_default_size).toBe("default");
    expect(await api.hostedConfig()).toMatchObject({ default_size: "l" });
  });

  it.each([
    ["ephemeral_max_per_user", "21"],
    ["ephemeral_max_per_user", "abc"],
    ["ephemeral_default_size", "xl"],
    ["hosted_default_size", "M"],
    ["ephemeral_lease", "3h"],
    ["ephemeral_lease", "abc"],
  ])("refuses %s=%j", async (key, value) => {
    const api = await reload();
    await expect(api.updateSettings({ [key]: value })).rejects.toMatchObject({ status: 400 });
  });
});
