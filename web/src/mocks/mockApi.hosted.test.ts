import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";

// Each test gets a fresh mockApi module so the in-memory fleet starts from seed.
async function fresh() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

describe("mockApi hosted workers (PRD #58 M5)", () => {
  it("returns { worker } and NO token — the field the compiler cannot guard", async () => {
    // This assertion exists because `api: typeof realApi` does NOT catch an extra
    // field. It catches a MISSING method or a missing promised field, but mockApi is
    // declared bare and checked as a reference, so excess-property checking never
    // fires; and each value leaves through delay<T>, which infers T from its argument
    // rather than from the declared return type. So a coder copying createWorker's
    // mock — it mints a uzi_wk_… and returns { worker, token } — compiles clean.
    // Nothing else in the codebase notices. This does.
    const api = await fresh();
    await api.login("vlad@uzi.local", "x");
    const res = await api.provisionHostedWorker("base", "m");
    expect(res.worker).toBeTruthy();
    expect("token" in res).toBe(false);
  });

  it("hardcodes hosting ON with a quota of 5 — a demo of a hidden feature is not a demo", async () => {
    // On a real stack the flag is off by default and there is no controller until M3,
    // so the mock is the only place M5 can be seen at all.
    const api = await fresh();
    await api.login("vlad@uzi.local", "x");
    expect(await api.hostedConfig()).toEqual({ enabled: true, quota: 5, ephemeral_enabled: true, docker_enabled: true });
  });

  it("puts the at-quota journey three clicks away: four seeded hosted workers of five", async () => {
    // The point of the seed. web-ux drives provision → at quota → button disables →
    // delete → it enables again, which is the only way to prove the client-side gate
    // RELEASES. A component test asserting "disabled at quota" passes either way.
    const api = await fresh();
    await api.login("vlad@uzi.local", "x");
    // PERSISTENT hosted rows only: the quota (server CountHostedWorkersForUser) excludes
    // ephemeral workers, and so does the page's hostedCount. The seeded leased ephemeral
    // worker (PRD #2006 M2) is hosted but must not eat the one slot of headroom.
    const persistent = (w: { kind: string; ephemeral?: boolean }) => w.kind === "hosted" && w.ephemeral !== true;
    const all = (await api.listWorkers()).workers;
    expect(all.filter((w) => w.kind === "hosted" && w.ephemeral === true)).toHaveLength(1);
    const seeded = all.filter(persistent);
    // FOUR seeded hosted workers against a quota of FIVE: PRD #113 M5 added the failed
    // roller, and PRD #496 added two cordoned demo workers. The numbers moved together
    // deliberately: what this test exists to prove is that the gate RELEASES, which
    // needs exactly one slot of headroom, and that is unchanged.
    expect(seeded).toHaveLength(4);
    expect(seeded[0].hosted_size).toBe("m");

    const { worker } = await api.provisionHostedWorker("base", "l");
    const after = (await api.listWorkers()).workers.filter(persistent);
    expect(after).toHaveLength(5); // at quota

    // Delete is kind-blind, exactly as the real DELETE /api/workers/{id} is.
    await api.deleteWorker(worker.id);
    expect((await api.listWorkers()).workers.filter(persistent)).toHaveLength(4);
  });

  it("provisions offline, unreported, with the chosen size — the controller has not started it yet", async () => {
    const api = await fresh();
    await api.login("vlad@uzi.local", "x");
    const { worker } = await api.provisionHostedWorker("jvm", "l");
    expect(worker.kind).toBe("hosted");
    expect(worker.hosted_size).toBe("l"); // the wire value, never the "L" the UI shows
    expect(worker.status).toBe("offline");
    expect(worker.template_declared).toBe("jvm");
    expect(worker.template_reported).toBeNull();
    expect(worker.stats_cpu_pct).toBeNull();
  });

  it("derives a name from template + size when the form sends none", async () => {
    // The M5 form collects type + size and no name, so this is the live path, not a
    // fallback: it mirrors the handler's derivedHostedWorkerName, now AWS-style
    // `base.s-<4-hex>` (dot notation, lowercase letter, hex suffix). The mock's suffix
    // is counter-derived rather than crypto/rand, so we match the shape, not a literal.
    const api = await fresh();
    await api.login("vlad@uzi.local", "x");
    expect((await api.provisionHostedWorker("base", "s")).worker.name).toMatch(/^base\.s-[0-9a-f]{4}$/);
    // docker=false, then a WHITESPACE name (4th arg) → still derived. The name arg
    // moved behind docker (PRD #83 M3), so it is passed positionally here.
    expect((await api.provisionHostedWorker("jvm", "m", false, "  ")).worker.name).toMatch(/^jvm\.m-[0-9a-f]{4}$/);
  });

  it("marks every seeded hand-run worker external, so the badge means something", async () => {
    const api = await fresh();
    await api.login("vlad@uzi.local", "x");
    const external = (await api.listWorkers()).workers.filter((w) => w.kind === "external");
    expect(external.length).toBeGreaterThan(0);
    expect(external.every((w) => w.hosted_size === null)).toBe(true);
  });

  it("keeps createWorker external and still token-bearing (the other kind is untouched)", async () => {
    const api = await fresh();
    await api.login("vlad@uzi.local", "x");
    const res = await api.createWorker("laptop-2", "base");
    expect(res.worker.kind).toBe("external");
    expect(res.worker.hosted_size).toBeNull();
    expect(res.token).toMatch(/^uzi_wk_/);
  });
});


describe("mockApi ephemeral preferences partial updates", () => {
  const key = "uzi.mock.v4";

  beforeEach(() => {
    const values = new Map<string, string>();
    vi.stubGlobal("localStorage", {
      getItem: (k: string) => values.get(k) ?? null,
      setItem: (k: string, value: string) => void values.set(k, value),
    });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("restores both preferences in the default session and backing users after hard reloads", async () => {
    let api = await fresh();
    const admin = (await api.me()).user;
    await api.setEphemeralWorkersEnabled({ docker: true });
    api = await fresh();
    expect((await api.me()).user).toMatchObject({ id: admin.id, ephemeral_workers_enabled: false, ephemeral_docker_enabled: true });
    await api.logout();
    expect((await api.login("vlad@uzi.local", "x")).user).toMatchObject({ ephemeral_workers_enabled: false, ephemeral_docker_enabled: true });
    await api.setEphemeralWorkersEnabled(true);
    await api.putMySettings({ theme: "mission" });
    api = await fresh();
    expect((await api.me()).user).toMatchObject({ ephemeral_workers_enabled: true, ephemeral_docker_enabled: true });
    expect((await api.getMySettings()).settings.theme).toBe("mission");
    await api.setEphemeralWorkersEnabled({ enabled: false, docker: false });
    api = await fresh();
    expect((await api.me()).user).toMatchObject({ ephemeral_workers_enabled: false, ephemeral_docker_enabled: false });
  });

  it("retains isolated preferences for another user through settings writes and reloads", async () => {
    let api = await fresh();
    const admin = (await api.setEphemeralWorkersEnabled({ docker: true })).user;
    await api.logout();
    const other = (await api.login("mira@uzi.local", "x")).user;
    expect(other).toMatchObject({ ephemeral_workers_enabled: false, ephemeral_docker_enabled: false });
    await api.setEphemeralWorkersEnabled({ enabled: true, docker: false });
    await api.updateSettings({ ephemeral_workers_enabled: "true" });
    api = await fresh();
    expect((await api.me()).user).toMatchObject({ id: admin.id, ephemeral_workers_enabled: false, ephemeral_docker_enabled: true });
    await api.logout();
    expect((await api.login("mira@uzi.local", "x")).user).toMatchObject({ id: other.id, ephemeral_workers_enabled: true, ephemeral_docker_enabled: false });
    const blob = JSON.parse(localStorage.getItem(key)!);
    expect(blob.ephemeralPreferences).toEqual({
      [admin.id]: { ephemeral_workers_enabled: false, ephemeral_docker_enabled: true },
      [other.id]: { ephemeral_workers_enabled: true, ephemeral_docker_enabled: false },
    });
  });

  it("rejects tierless writes before changing storage or either preference", async () => {
    let api = await fresh();
    await api.setEphemeralWorkersEnabled({ enabled: false, docker: false });
    const before = localStorage.getItem(key);
    const { workersApi } = await import("./mockApi/workers");
    const config = vi.spyOn(workersApi, "hostedConfig").mockResolvedValue({ enabled: true, quota: 5, ephemeral_enabled: true, docker_enabled: false });
    try {
      await expect(api.setEphemeralWorkersEnabled({ enabled: true, docker: true })).rejects.toMatchObject({ status: 409 });
      expect(localStorage.getItem(key)).toBe(before);
      expect((await api.me()).user).toMatchObject({ ephemeral_workers_enabled: false, ephemeral_docker_enabled: false });
    } finally {
      config.mockRestore();
    }
    api = await fresh();
    expect((await api.me()).user).toMatchObject({ ephemeral_workers_enabled: false, ephemeral_docker_enabled: false });
  });

  it.each([[undefined], [null], [[]], ["invalid"], [{ malformed: true }]])("accepts old blobs and fails closed on malformed preference maps: %j", async (prefs) => {
    let api = await fresh();
    const admin = (await api.me()).user;
    await api.putMySettings({ theme: "mission" });
    const blob = JSON.parse(localStorage.getItem(key)!);
    blob.ephemeralPreferences = prefs;
    localStorage.setItem(key, JSON.stringify(blob));
    api = await fresh();
    expect((await api.getMySettings()).settings.theme).toBe("mission");
    expect((await api.me()).user).toMatchObject({ id: admin.id, ephemeral_workers_enabled: false, ephemeral_docker_enabled: false });
  });

  it("rejects malformed preference pairs without trusting extra identity fields", async () => {
    let api = await fresh();
    const admin = (await api.me()).user;
    await api.putMySettings({ theme: "mission" });
    const blob = JSON.parse(localStorage.getItem(key)!);
    blob.ephemeralPreferences = { [admin.id]: { ephemeral_workers_enabled: true, ephemeral_docker_enabled: "true", is_admin: false } };
    localStorage.setItem(key, JSON.stringify(blob));
    api = await fresh();
    expect((await api.me()).user).toMatchObject({ is_admin: true, ephemeral_workers_enabled: false, ephemeral_docker_enabled: false });
    blob.ephemeralPreferences[admin.id] = { ephemeral_workers_enabled: true, ephemeral_docker_enabled: true, is_admin: false };
    localStorage.setItem(key, JSON.stringify(blob));
    api = await fresh();
    expect((await api.me()).user).toMatchObject({ is_admin: true, ephemeral_workers_enabled: true, ephemeral_docker_enabled: true });
    await api.setEphemeralWorkersEnabled({});
    expect(JSON.parse(localStorage.getItem(key)!).ephemeralPreferences[admin.id]).toEqual({ ephemeral_workers_enabled: true, ephemeral_docker_enabled: true });
  });

  it("preserves omitted fields, supports both/neither, and retains Docker with auto-provision off across login", async () => {
    const api = await fresh();
    await api.login("vlad@uzi.local", "x");
    const prefs = (u: { ephemeral_workers_enabled: boolean; ephemeral_docker_enabled: boolean }) =>
      [u.ephemeral_workers_enabled, u.ephemeral_docker_enabled];
    expect(prefs((await api.setEphemeralWorkersEnabled({ enabled: true, docker: true })).user)).toEqual([true, true]);
    expect(prefs((await api.setEphemeralWorkersEnabled(false)).user)).toEqual([false, true]);
    expect(prefs((await api.setEphemeralWorkersEnabled({})).user)).toEqual([false, true]);
    expect(prefs((await api.setEphemeralWorkersEnabled({ docker: false })).user)).toEqual([false, false]);
    await api.setEphemeralWorkersEnabled({ docker: true });
    expect(prefs((await api.me()).user)).toEqual([false, true]);
    await api.logout();
    expect(prefs((await api.login("vlad@uzi.local", "x")).user)).toEqual([false, true]);
    // Another user's backing record must be unchanged.
    await api.logout();
    expect(prefs((await api.login("mira@uzi.local", "x")).user)).toEqual([false, false]);
  });

  it("refuses tierless Docker atomically, including backing user; disabling remains allowed", async () => {
    const api = await fresh();
    const { workersApi } = await import("./mockApi/workers");
    await api.login("vlad@uzi.local", "x");
    await api.setEphemeralWorkersEnabled({ docker: true });
    const config = vi.spyOn(workersApi, "hostedConfig").mockResolvedValue({ enabled: true, quota: 5, ephemeral_enabled: true, docker_enabled: false });
    try {
      await expect(api.setEphemeralWorkersEnabled({ enabled: true, docker: true })).rejects.toMatchObject({ status: 409 });
      expect((await api.me()).user).toMatchObject({ ephemeral_workers_enabled: false, ephemeral_docker_enabled: true });
      await api.logout();
      expect((await api.login("vlad@uzi.local", "x")).user).toMatchObject({ ephemeral_workers_enabled: false, ephemeral_docker_enabled: true });
      expect((await api.setEphemeralWorkersEnabled({ docker: false })).user).toMatchObject({ ephemeral_workers_enabled: false, ephemeral_docker_enabled: false });
      expect((await api.setEphemeralWorkersEnabled(true)).user).toMatchObject({ ephemeral_workers_enabled: true, ephemeral_docker_enabled: false });
    } finally {
      config.mockRestore();
    }
  });
});
