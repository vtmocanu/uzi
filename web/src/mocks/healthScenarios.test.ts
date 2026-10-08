import { describe, expect, it, vi } from "vitest";

import { healthApi } from "./mockApi/health";
import { workersApi } from "./mockApi/workers";
import { ownerOnlyWaitingOwnerId, ownerOnlyWaitingRunId } from "./data/health";
import { mockScenario } from "./mockApi/shared";

vi.mock("./mockApi/shared", async (importOriginal) => ({
  ...await importOriginal<typeof import("./mockApi/shared")>(),
  requireSession: vi.fn(),
  delay: <T>(value: T) => Promise.resolve(value),
  mockScenario: vi.fn(),
  requireAdmin: vi.fn(),
}));

describe("health scenario routing", () => {
  it("emits fleet.quarantine in the workers group, ok, right after fleet.rundisk", async () => {
    vi.mocked(mockScenario).mockReturnValue("health-silent");
    const doc = await healthApi.getAdminHealth();
    const ids = doc.checks.map((c) => c.id);
    expect(ids.indexOf("fleet.quarantine")).toBe(ids.indexOf("fleet.rundisk") + 1);
    expect(doc.checks.find((c) => c.id === "fleet.quarantine")).toMatchObject({
      scope: "owner", group: "workers", title: "Worker residue quarantine", severity: "ok",
      summary: "No worker reports a residue quarantine.", doc: "hosted-workers",
    });
  });

  it("emits forge.sync in the integrations group, ok, right after forge.ciwatch", async () => {
    vi.mocked(mockScenario).mockReturnValue("health-silent");
    const doc = await healthApi.getAdminHealth();
    const ids = doc.checks.map((c) => c.id);
    expect(ids.indexOf("forge.sync")).toBeGreaterThan(0);
    expect(ids.indexOf("forge.sync")).toBe(ids.indexOf("forge.ciwatch") + 1);
    expect(doc.checks.find((c) => c.id === "forge.sync")).toMatchObject({
      scope: "owner", group: "integrations", title: "Forge issue sync", severity: "ok",
      summary: "0 of 3 failing; 0 pending.", doc: null,
    });
  });

  it("serves owner-only danger without an instance episode", async () => {
    vi.mocked(mockScenario).mockReturnValue("health-owner-only");
    const doc = await healthApi.getAdminHealth();
    expect(doc.status).toBe("danger");
    expect(doc.blocking).toBe(false);
    expect(doc.episode_id).toBeNull();
    expect(doc.snoozed_until).toBeNull();
    expect(doc.checks.find((c) => c.id === "fleet.capacity")).toMatchObject({ scope: "owner", severity: "danger" });
    for (const id of ["fleet.capacity", "queue.waiting"]) {
      expect(doc.checks.find((c) => c.id === id)?.evidence[0]).toEqual({
        label: "Waiting run",
        value: `run ${ownerOnlyWaitingRunId}; owner ${ownerOnlyWaitingOwnerId}; waited 36m; stored reason no online worker can run this — it needs a capability none of your workers has; provision a ca`,
      });
    }
    const { workers } = await workersApi.adminListWorkers();
    expect(workers).toHaveLength(4);
    expect(doc.checks.find((c) => c.id === "fleet.roll")?.severity).toBe("ok");
    for (const worker of workers) {
      expect(worker.upgrade_status).toBe("up_to_date");
      expect(worker.upgrade_blocking_reason).toBeNull();
    }
    await expect(healthApi.snoozeAdminHealth()).rejects.toMatchObject({ status: 409 });
  });

  it("serves confirmed-drain as a healthy orderly 40m upgrade wait without an episode", async () => {
    vi.mocked(mockScenario).mockReturnValue("health-confirmed-drain");
    const doc = await healthApi.getAdminHealth();
    expect(doc.status).toBe("ok");
    expect(doc.blocking).toBe(false);
    expect(doc.episode_id).toBeNull();
    expect(doc.snoozed_until).toBeNull();
    expect(doc.counts).toEqual({ ok: 17, warn: 0, danger: 0, unknown: 0, na: 0 });
    expect(doc.checks.find((c) => c.id === "fleet.roll")).toMatchObject({
      scope: "instance", severity: "ok", summary: "All 4 hosted workers are rolling cleanly.",
    });
    expect(doc.checks.find((c) => c.id === "fleet.capacity")).toMatchObject({
      scope: "owner", severity: "ok",
      summary: "1 owner(s) are waiting while workers finish their current runs before an upgrade.",
      evidence: [{ label: "Waiting run", value: expect.stringContaining("; waited 40m; stored reason ") }],
    });
    expect(doc.checks.find((c) => c.id === "queue.waiting")).toMatchObject({
      scope: "owner", severity: "ok",
      summary: "Runs are waiting while workers finish their current runs before an upgrade.",
    });
    const { workers } = await workersApi.adminListWorkers();
    expect(workers).toHaveLength(4);
    const draining = workers.filter((w) => w.draining_since !== null);
    expect(draining).toHaveLength(2);
    for (const worker of draining) {
      expect(worker).toMatchObject({
        owner_email: "user.a@uzi.local", status: "online",
        upgrade_status: "upgrading", busy: true, active_runs: 1,
      });
      expect(Date.now() - Date.parse(worker.draining_since!)).toBeGreaterThanOrEqual(40 * 60_000);
      expect(Date.now() - Date.parse(worker.draining_since!)).toBeLessThan(41 * 60_000);
    }
    for (const worker of workers) {
      expect(worker.upgrade_status).not.toBe("upgrade_failed");
      expect(worker.upgrade_blocking_reason).toBeNull();
      expect(worker.upgrade_blocking_container).toBeNull();
    }
    await expect(healthApi.snoozeAdminHealth()).rejects.toMatchObject({ status: 409 });
  });

  it("confirms the incident episode's snooze without adding episodes to owner or drain scenarios", async () => {
    vi.mocked(mockScenario).mockReturnValue("health-incident");
    const doc = await healthApi.getAdminHealth();
    expect(doc.blocking).toBe(true);
    expect(doc.episode_id).toBe("b1f0c2ep");
    const snooze = await healthApi.snoozeAdminHealth();
    expect(snooze.episode_id).toBe(doc.episode_id);
    expect((await healthApi.getAdminHealth()).snoozed_until).toBe(snooze.snoozed_until);
    for (const scenario of ["health-owner-only", "health-confirmed-drain"]) {
      vi.mocked(mockScenario).mockReturnValue(scenario);
      expect(await healthApi.getAdminHealth()).toMatchObject({
        blocking: false, episode_id: null, snoozed_until: null,
      });
    }
    vi.mocked(mockScenario).mockReturnValue("health-incident");
    expect((await healthApi.getAdminHealth()).snoozed_until).toBe(snooze.snoozed_until);
  });
});
