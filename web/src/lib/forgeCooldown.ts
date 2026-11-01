import type { ForgeOriginCooldown } from "./apiTypes";
import { stripDisplayCell } from "./safeText";

type ForgeWait = {
  status: string;
  recovery_wait_cause?: string | null;
  recovery_retry_not_before?: string | null;
  recovery_wait_phase?: string | null;
};

// Null is an explicit owner decision. Older APIs that omit timing prove neither mode.
export function forgeWaitMode(run: ForgeWait): "held" | "timed" | "unknown" | null {
  if (run.status !== "recovery_wait" || run.recovery_wait_cause !== "forge_unreachable") return null;
  if (run.recovery_retry_not_before === null) return "held";
  return typeof run.recovery_retry_not_before === "string" && Number.isFinite(Date.parse(run.recovery_retry_not_before))
    ? "timed" : "unknown";
}

// Issue #1324: which part of the run a forge park interrupted. "publication" is the only
// phase this client knows; any other non-empty value is "other" and keeps the plain forge
// wording (the panel names the raw value). No phase is the pre-clone forge park, unchanged.
export function forgeWaitPhase(run: ForgeWait): "publication" | "other" | null {
  if (forgeWaitMode(run) === null) return null;
  const phase = run.recovery_wait_phase;
  if (phase === "publication") return "publication";
  return typeof phase === "string" && phase !== "" ? "other" : null;
}

export function forgeWaitLabel(run: ForgeWait): string | null {
  const mode = forgeWaitMode(run);
  if (forgeWaitPhase(run) === "publication") {
    return mode === "held" ? "Publishing held: needs your decision"
      : mode === "timed" ? "Publishing paused"
      : "Publishing paused: retry timing unknown";
  }
  return mode === "held" ? "Forge recovery needs your decision"
    : mode === "timed" ? "Forge retry scheduled"
    : mode === "unknown" ? "Forge retry timing unknown" : null;
}

// Fixed blocker sentence per owner-hold reason. An unrecognised reason is shown as the
// (sanitised) raw value rather than mapped to a known one.
export function publicationHoldCopy(reason: string): string {
  switch (reason) {
    case "publication_retry_exhausted":
      return "The publication wait limit was reached.";
    case "publication_custodian_unavailable":
      return "The worker holding the unpublished work is unavailable.";
    case "publication_model_required":
      return "Publishing needs model work, which a publication-only resume never does.";
    case "publication_source_unavailable":
      return "The unpublished work is not available.";
    case "publication_intent_mismatch":
      return "The publication record did not match.";
    default:
      return `Unrecognised hold reason: ${stripDisplayCell(reason)}.`;
  }
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
  const deadline = cooldown.state !== "cooling" || (Number.isFinite(until) && until <= nowMs)
    ? ` Cooldown deadline: ${time}.` : "";
  return `${origin}: ${state}${deadline}${cooldown.detail ? ` Last reported: ${stripDisplayCell(cooldown.detail)}` : ""}`;
}
