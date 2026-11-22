import type { HealthDoc } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { confirmedDrainDoc, degradedDoc, healthySilentDoc, incidentDoc, maintenanceDoc, overviewNextDoc, ownerOnlyDoc } from "../data";
import { delay, mockScenario, requireAdmin } from "./shared";
import { agentSourceHealthCheck } from "./agentSource";
import { appSettings } from "./settings";

function withAgentSource(doc: HealthDoc): HealthDoc {
  const source = agentSourceHealthCheck();
  const autoEnabled = appSettings.worker_hibernation_enabled === "true" && appSettings.worker_hibernation_auto === "true";
  const checks = doc.checks.map((c) => {
    if (c.id === source.id) return source;
    if (c.id !== "fleet.hibernation") return c;
    // These routed scenarios all have hosted workers and model only off/healthy auto status.
    return { ...c, severity: autoEnabled ? "ok" : "na", summary: autoEnabled
      ? "No idle hosted worker is blocked from automatic hibernation." : "Automatic hibernation is off." };
  });
  const counts = { ok: 0, warn: 0, danger: 0, unknown: 0, na: 0 };
  for (const c of checks) counts[c.severity as keyof typeof counts]++;
  return {
    ...doc,
    checks,
    counts,
    status: counts.danger > 0 ? "danger" : counts.warn + counts.unknown > 0 ? "warn" : "ok",
    blocking: checks.some((c) => c.scope === "instance" && c.severity === "danger"),
  };
}

// Admin health (PRD #1484). GET /api/admin/health backs the Health tab and (M5) the
// Overview HealthChip + danger banner. Admin-only (RequireAdminRO), so the mock gates on admin
// exactly as the real route does. The scenario picks which fixture to serve:
//
//   ?mock=health-degraded  → two checks need attention (warn)
//   ?mock=health-incident  → the fleet is pinned to an unpublished image (danger); the
//                            workers mock returns the stuck fleet so the table populates,
//                            AND a non-admin viewer sees the platform line on Overview
//   ?mock=health-owner-only → owner danger without an instance blocker or episode
//   ?mock=health-confirmed-drain → orderly upgrade wait, all checks ok and no episode
//   ?mock=health-silent    → every applicable check passing (automatic hibernation is off)
//   ?mock=overview-next    → one owner warning that raises the admin attention banner
//                            (the default Overview scenario, PRD #3878 and #3877)
//   ?mock=maintenance      → maintenance.active warns (PRD #3454; see mockApi/maintenance.ts)
//   (default)              → healthy, same as health-silent
//
// See docs/dev-conventions.md's demo-scenarios section for the registered names.

// The incident scenario's banner snooze (M5): the caller's own per-episode snooze. A demo
// snooze flips this so the very next getAdminHealth carries snoozed_until, exactly as the
// real endpoint does (the api stores the per-(episode, caller) snooze and the next poll
// reads it back). Module-level so it survives across the poll cadence, reset on a scenario
// switch is not modelled (a demo reload clears it).
let snoozed: { episode: string; until: string } | null = null;

export const healthApi = {
  getAdminHealth: async (): Promise<HealthDoc> => {
    requireAdmin();
    switch (mockScenario()) {
      case "health-degraded":
        return delay(withAgentSource(degradedDoc()));
      case "health-owner-only":
        return delay(withAgentSource(ownerOnlyDoc()));
      case "health-confirmed-drain":
        return delay(withAgentSource(confirmedDrainDoc()));
      case "maintenance":
        return delay(withAgentSource(maintenanceDoc()));
      case "overview-next":
        return delay(withAgentSource(overviewNextDoc()));
      case "health-incident": {
        const d = incidentDoc();
        // Reflect a prior snooze so the banner stays hidden until the snooze expires or a
        // new episode opens — the same round-trip the real endpoint gives the banner.
        if (snoozed?.episode === d.episode_id) d.snoozed_until = snoozed.until;
        return delay(withAgentSource(d));
      }
      case "agent-source-update":
      case "health-silent":
      default:
        return delay(withAgentSource(healthySilentDoc()));
    }
  },

  // POST /api/admin/health/snooze (M5, D2): snooze the caller's Danger banner for 1 h against
  // the open episode. Admin-only write; returns the episode id and the new snoozed_until, and
  // flips the module snooze so the next getAdminHealth confirms it.
  snoozeAdminHealth: async (): Promise<{ episode_id: string; snoozed_until: string }> => {
    requireAdmin();
    const until = new Date(Date.now() + 60 * 60 * 1000).toISOString();
    if (mockScenario() !== "health-incident") {
      throw new ApiError(409, "no open health episode to snooze");
    }
    const episodeId = incidentDoc().episode_id!;
    snoozed = { episode: episodeId, until };
    return delay({ episode_id: episodeId, snoozed_until: until });
  },
};
