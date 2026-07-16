import { describe, it, expect, vi } from "vitest";

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

  it("hardcodes hosting ON with a quota of 2 — a demo of a hidden feature is not a demo", async () => {
    // On a real stack the flag is off by default and there is no controller until M3,
    // so the mock is the only place M5 can be seen at all.
    const api = await fresh();
    await api.login("vlad@uzi.local", "x");
    expect(await api.hostedConfig()).toEqual({ enabled: true, quota: 2 });
  });

  it("puts the at-quota journey three clicks away: one seeded hosted worker of two", async () => {
    // The point of the seed. web-ux drives provision → 2 of 2 → button disables →
    // delete → it enables again, which is the only way to prove the client-side gate
    // RELEASES. A component test asserting "disabled at quota" passes either way.
    const api = await fresh();
    await api.login("vlad@uzi.local", "x");
    const seeded = (await api.listWorkers()).workers.filter((w) => w.kind === "hosted");
    expect(seeded).toHaveLength(1);
    expect(seeded[0].hosted_size).toBe("m");

    const { worker } = await api.provisionHostedWorker("base", "l");
    const after = (await api.listWorkers()).workers.filter((w) => w.kind === "hosted");
    expect(after).toHaveLength(2); // at quota

    // Delete is kind-blind, exactly as the real DELETE /api/workers/{id} is.
    await api.deleteWorker(worker.id);
    expect((await api.listWorkers()).workers.filter((w) => w.kind === "hosted")).toHaveLength(1);
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
    // fallback: it mirrors the handler's derivedHostedWorkerName. Upper-case is for
    // display only — hosted_size above stays lowercase.
    const api = await fresh();
    await api.login("vlad@uzi.local", "x");
    expect((await api.provisionHostedWorker("base", "s")).worker.name).toBe("base (S)");
    expect((await api.provisionHostedWorker("jvm", "m", "  ")).worker.name).toBe("jvm (M)");
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
