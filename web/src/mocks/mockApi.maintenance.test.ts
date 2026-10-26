import { describe, expect, it, vi } from "vitest";

import { healthApi } from "./mockApi/health";
import { maintenanceApi } from "./mockApi/maintenance";
import { mockScenario } from "./mockApi/shared";

vi.mock("./mockApi/shared", async (importOriginal) => ({
  ...await importOriginal<typeof import("./mockApi/shared")>(),
  requireSession: vi.fn(),
  requireAdmin: vi.fn(() => ({ display_name: "Robin Diaz", email: "robin@example.com" })),
  delay: <T>(value: T) => Promise.resolve(value),
  mockScenario: vi.fn(),
}));

// ?mock=maintenance (PRD #3454): starts on with a reason and a not-clear readout whose
// categories carry both tags; the toggle stays live, and Health warns on maintenance.active.
describe("maintenance mock scenario", () => {
  it("seeds an active state, a not-clear readout, and the warning health check", async () => {
    vi.mocked(mockScenario).mockReturnValue("maintenance");
    const pub = await maintenanceApi.getMaintenance();
    expect(pub).toMatchObject({ active: true, reason: "Node pool upgrade, back by 15:00" });
    expect(Object.keys(pub).sort()).toEqual(["active", "reason", "since"]);

    const admin = await maintenanceApi.getAdminMaintenance();
    expect(admin.readout.clear).toBe(false);
    expect(admin.readout.held.items.map((i) => i.tag)).toEqual(["needs_owner", "progressing", "progressing"]);
    expect(admin.readout.custody.count).toBe(1);
    expect(admin.readout.queued.count).toBeGreaterThan(0);

    const doc = await healthApi.getAdminHealth();
    expect(doc.checks.find((c) => c.id === "maintenance.active")).toMatchObject({
      scope: "instance", group: "housekeeping", severity: "warn",
    });

    // Updating the reason while on keeps `since`; turning off clears the state.
    const updated = await maintenanceApi.setMaintenance(true, "  New reason ");
    expect(updated.state).toMatchObject({ active: true, reason: "New reason", since: pub.since });
    const off = await maintenanceApi.setMaintenance(false, "");
    expect(off.state).toEqual({ active: false, since: null, reason: null, by_label: null });
    await expect(maintenanceApi.setMaintenance(true, "x".repeat(281))).rejects.toThrow(/at most 280/);
  });

  it("keeps maintenance.active ok outside the scenario", async () => {
    vi.mocked(mockScenario).mockReturnValue("health-silent");
    const doc = await healthApi.getAdminHealth();
    expect(doc.checks.find((c) => c.id === "maintenance.active")?.severity).toBe("ok");
  });
});
