// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";

// ?mock=overview-next (PRD #3878): scenario-gated seeding for the Overview preview. The
// scenario is read when the store seeds, so each test re-imports a fresh mockApi.
async function freshApi(scenario: string | null) {
  vi.resetModules();
  localStorage.clear();
  if (scenario) localStorage.setItem("uzi_mock_scenario", scenario);
  return (await import("./mockApi")).mockApi;
}

afterEach(() => localStorage.clear());

describe("overview-next mock scenario", () => {
  it("adds other users' runs to the admin list only, with their owners", async () => {
    const api = await freshApi("overview-next");
    const mine = (await api.listRuns({ view: "summary" })).runs;
    const factory = (await api.adminListRuns({ view: "summary" })).runs;
    const byId = new Map(factory.map((r) => [r.id, r]));

    expect(byId.get("run-ov-priya-approval")).toMatchObject({ status: "awaiting_approval", owner_email: "priya@uzi.local" });
    expect(byId.get("run-ov-sam-running")).toMatchObject({ status: "running", owner_email: "sam@uzi.local" });
    expect(byId.get("run-ov-jordan-limit")).toMatchObject({ status: "limit_wait", owner_email: "jordan@uzi.local" });
    for (const id of ["run-ov-priya-approval", "run-ov-sam-running", "run-ov-jordan-limit"]) {
      expect(mine.some((r) => r.id === id)).toBe(false);
    }
  });

  it("seeds an own completed run sharing repo and MR with run-ci-halted", async () => {
    const api = await freshApi("overview-next");
    const runs = (await api.listRuns({ view: "summary" })).runs;
    const halted = runs.find((r) => r.id === "run-ci-halted")!;
    const dup = runs.find((r) => r.id === "run-ov-ci-halted-retry")!;
    expect(dup).toMatchObject({ status: "completed", kind: "issue", mr_state: "opened" });
    expect([dup.repo_id, dup.mr_iid]).toEqual([halted.repo_id, halted.mr_iid]);
  });

  it("turns maintenance on and serves a health document that raises the admin attention banner", async () => {
    const api = await freshApi("overview-next");
    expect((await api.getMaintenance()).active).toBe(true);
    const doc = await api.getAdminHealth();
    const banner = doc.checks.filter((c) => c.scope === "owner" && c.severity === "warn" && c.attention_banner === true);
    expect(banner.length).toBeGreaterThan(0);
    expect(doc.blocking).toBe(false);
  });

  it("serves an owner fleet covering every capacity state, only under the scenario", async () => {
    const api = await freshApi("overview-next");
    const fleet = (await api.listWorkers()).workers;
    const byId = new Map(fleet.map((w) => [w.id, w]));
    expect(byId.get("w-ov-rolling")).toMatchObject({ kind: "hosted", upgrade_status: "upgrading", upgrade_target: "v0.86.1-rc.2", active_runs: 1 });
    expect(byId.get("w-ov-rolling")?.draining_since).not.toBeNull();
    expect(byId.get("w-ov-cordoned")).toMatchObject({ effective_cordoned: true, active_runs: 0, max_concurrent_runs: 1 });
    expect(byId.get("w-ov-offline")?.status).toBe("offline");
    expect(byId.get("w-ov-nocap")?.max_concurrent_runs).toBeNull();
    expect(byId.get("w-ov-nostats")?.stats_source).toBeNull();
    const plain = await freshApi(null);
    expect((await plain.listWorkers()).workers.some((w) => w.id.startsWith("w-ov-"))).toBe(false);
  });

  it("seeds none of it outside the scenario", async () => {
    const api = await freshApi(null);
    const factory = (await api.adminListRuns({ view: "summary" })).runs;
    const own = (await api.listRuns({ view: "summary" })).runs;
    expect(factory.some((r) => r.id.startsWith("run-ov-"))).toBe(false);
    expect(own.some((r) => r.id.startsWith("run-ov-"))).toBe(false);
    expect((await api.getMaintenance()).active).toBe(false);
  });
});
