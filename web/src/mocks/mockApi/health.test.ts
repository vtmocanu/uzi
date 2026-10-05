import { describe, expect, it, vi } from "vitest";

import { healthApi } from "./health";
import { mockScenario } from "./shared";

vi.mock("./shared", () => ({
  delay: <T>(value: T) => Promise.resolve(value),
  mockScenario: vi.fn(),
  requireAdmin: vi.fn(),
}));

describe("health scenario routing", () => {
  it("serves owner-only danger without an instance episode", async () => {
    vi.mocked(mockScenario).mockReturnValue("health-owner-only");
    const doc = await healthApi.getAdminHealth();
    expect(doc.status).toBe("danger");
    expect(doc.blocking).toBe(false);
    expect(doc.episode_id).toBeNull();
    expect(doc.snoozed_until).toBeNull();
    expect(doc.checks.find((c) => c.id === "fleet.capacity")).toMatchObject({ scope: "owner", severity: "danger" });
    await expect(healthApi.snoozeAdminHealth()).rejects.toMatchObject({ status: 409 });
  });

  it("serves confirmed-drain as a healthy orderly 40m upgrade wait without an episode", async () => {
    vi.mocked(mockScenario).mockReturnValue("health-confirmed-drain");
    const doc = await healthApi.getAdminHealth();
    expect(doc.status).toBe("ok");
    expect(doc.blocking).toBe(false);
    expect(doc.episode_id).toBeNull();
    expect(doc.snoozed_until).toBeNull();
    expect(doc.counts).toEqual({ ok: 15, warn: 0, danger: 0, unknown: 0, na: 0 });
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
