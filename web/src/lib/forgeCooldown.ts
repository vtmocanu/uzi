import type { ForgeOriginCooldown } from "./apiTypes";
import { stripDisplayCell } from "./safeText";

type ForgeWait = {
  status: string;
  recovery_wait_cause?: string | null;
  recovery_retry_not_before?: string | null;
};

// Null is an explicit owner decision. Older APIs that omit timing prove neither mode.
export function forgeWaitMode(run: ForgeWait): "held" | "timed" | "unknown" | null {
  if (run.status !== "recovery_wait" || run.recovery_wait_cause !== "forge_unreachable") return null;
  if (run.recovery_retry_not_before === null) return "held";
  return typeof run.recovery_retry_not_before === "string" && Number.isFinite(Date.parse(run.recovery_retry_not_before))
    ? "timed" : "unknown";
}

export function forgeWaitLabel(run: ForgeWait): string | null {
  const mode = forgeWaitMode(run);
  return mode === "held" ? "Forge recovery needs your decision"
    : mode === "timed" ? "Forge retry scheduled"
    : mode === "unknown" ? "Forge retry timing unknown" : null;
}

export function forgeCooldownText(cooldown: ForgeOriginCooldown, nowMs: number): string {
  const origin = stripDisplayCell(cooldown.origin);
  const until = Date.parse(cooldown.cooled_until);
  const time = Number.isFinite(until) ? new Date(until).toLocaleString() : "unknown time";
  let state: string;
  switch (cooldown.state) {
    case "half_open":
      state = cooldown.half_open_run_id
        ? `Half-open attempt reserved for run ${stripDisplayCell(cooldown.half_open_run_id)}; recovery unverified.`
        : "Half-open reservation; run identity unavailable, recovery unverified.";
      break;
    case "legacy_serialized_until_upgrade":
      state = `Legacy worker: forge attempts serialized for the active flight${cooldown.half_open_run_id ? ` (run ${stripDisplayCell(cooldown.half_open_run_id)})` : " (run identity unavailable)"} until it finishes or qualifying proof is recorded; recovery unverified. Upgrading enables direct clone/fetch recovery reporting.`;
      break;
    case "reserved_unverified":
      state = "Attempt reserved; run identity unverified. Recovery unverified.";
      break;
    case "retry_eligible_unverified":
      state = "Cooldown elapsed; retry eligible, recovery unverified.";
      break;
    case "cooling":
      state = Number.isFinite(until) && until <= nowMs
        ? "Cooldown elapsed; retry eligible, recovery unverified."
        : `Forge cooldown until ${time}; recovery unverified.`;
      break;
    default:
      state = "Forge cooldown state unavailable; recovery unverified.";
  }
  return `${origin}: ${state}${cooldown.detail ? ` Last reported: ${stripDisplayCell(cooldown.detail)}` : ""}`;
}
