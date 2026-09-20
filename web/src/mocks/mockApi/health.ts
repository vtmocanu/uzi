import type { HealthDoc } from "../../lib/api";
import { degradedDoc, healthySilentDoc, incidentDoc } from "../data";
import { delay, mockScenario, requireAdmin } from "./shared";

// Admin health (PRD #1484). GET /api/admin/health backs the Health tab and (M5) the
// Overview card + danger banner. Admin-only (RequireAdminRO), so the mock gates on admin
// exactly as the real route does. The scenario picks which fixture to serve:
//
//   ?mock=health-degraded  → two warnings, nothing blocked (warn)
//   ?mock=health-incident  → the fleet is pinned to an unpublished image (danger); the
//                            workers mock returns the stuck fleet so the table populates,
//                            AND a non-admin viewer sees the platform line on Overview
//   ?mock=health-silent    → every check passing (healthy/all-ok)
//   (default)              → healthy, same as health-silent
//
// See docs/dev-conventions.md's demo-scenarios section for the registered names.

// The incident scenario's banner snooze (M5): the caller's own per-episode snooze. A demo
// snooze flips this so the very next getAdminHealth carries snoozed_until, exactly as the
// real endpoint does (the api stores the per-(episode, caller) snooze and the next poll
// reads it back). Module-level so it survives across the poll cadence, reset on a scenario
// switch is not modelled (a demo reload clears it).
let snoozedUntil: string | null = null;

export const healthApi = {
  getAdminHealth: async (): Promise<HealthDoc> => {
    requireAdmin();
    switch (mockScenario()) {
      case "health-degraded":
        return delay(degradedDoc());
      case "health-incident": {
        const d = incidentDoc();
        // Reflect a prior snooze so the banner stays hidden until the snooze expires or a
        // new episode opens — the same round-trip the real endpoint gives the banner.
        if (snoozedUntil) d.snoozed_until = snoozedUntil;
        return delay(d);
      }
      case "health-silent":
      default:
        return delay(healthySilentDoc());
    }
  },

  // POST /api/admin/health/snooze (M5, D2): snooze the caller's Danger banner for 1 h against
  // the open episode. Admin-only write; returns the episode id and the new snoozed_until, and
  // flips the module snooze so the next getAdminHealth confirms it.
  snoozeAdminHealth: async (): Promise<{ episode_id: string; snoozed_until: string }> => {
    requireAdmin();
    const until = new Date(Date.now() + 60 * 60 * 1000).toISOString();
    snoozedUntil = until;
    const episodeId = incidentDoc().episode_id ?? "b1f0c2ep";
    return delay({ episode_id: episodeId, snoozed_until: until });
  },
};
