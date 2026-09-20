import type { HealthDoc } from "../../lib/api";
import { degradedDoc, healthySilentDoc, incidentDoc } from "../data";
import { delay, mockScenario, requireAdmin } from "./shared";

// Admin health (PRD #1484). GET /api/admin/health backs the Health tab and (M5) the
// Overview card + danger banner. Admin-only (RequireAdminRO), so the mock gates on admin
// exactly as the real route does. The scenario picks which fixture to serve:
//
//   ?mock=health-degraded  → two warnings, nothing blocked (warn)
//   ?mock=health-incident  → the fleet is pinned to an unpublished image (danger); the
//                            workers mock returns the stuck fleet so the table populates
//   ?mock=health-silent    → every check passing (healthy/all-ok)
//   (default)              → healthy, same as health-silent
//
// See docs/dev-conventions.md's demo-scenarios section for the registered names.
export const healthApi = {
  getAdminHealth: async (): Promise<HealthDoc> => {
    requireAdmin();
    switch (mockScenario()) {
      case "health-degraded":
        return delay(degradedDoc());
      case "health-incident":
        return delay(incidentDoc());
      case "health-silent":
      default:
        return delay(healthySilentDoc());
    }
  },
};
