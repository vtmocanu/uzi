import type { HarnessLimitEvidence } from "../harness.js";
import type { CodexAppServerAuthMode } from "./appserver-auth.js";

interface Window {
  used: number;
  duration?: number;
  reset?: number;
}
const reachedTypes = [
  "rate_limit_reached", "workspace_owner_credits_depleted",
  "workspace_member_credits_depleted", "workspace_owner_usage_limit_reached",
  "workspace_member_usage_limit_reached",
] as const;
type Reached = typeof reachedTypes[number] | "absent" | "unrecognized";
interface Snapshot {
  primary?: Window;
  secondary?: Window;
  reached: Reached;
  spend?: boolean;
}
function object(value: unknown): Record<string, unknown> | undefined {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown> : undefined;
}
function integer(value: unknown, min: number, max: number): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= min && value <= max;
}
function window(value: unknown): Window | undefined {
  const raw = object(value);
  if (!raw || !integer(raw.usedPercent, 0, Number.MAX_SAFE_INTEGER)) return undefined;
  return {
    used: raw.usedPercent,
    duration: integer(raw.windowDurationMins, 1, 44640) ? raw.windowDurationMins : undefined,
    reset: integer(raw.resetsAt, 1e9, 4.1e9) ? raw.resetsAt * 1000 : undefined,
  };
}
function duration(value: number | undefined): string {
  return value === 300 ? "five_hour" : value === 10080 ? "seven_day" : "unknown";
}

/** Pure per-turn account observer. Four equality keys bound retention; replacement
 * never retains provider metadata. A rejected latest update poisons older evidence. */
export class CodexRateLimitObserver {
  private readonly buckets = new Map<string, Snapshot>();
  private latest?: Snapshot;

  observe(value: unknown): void {
    this.latest = undefined;
    const raw = object(value);
    if (!raw) return;
    const id = raw.limitId == null ? "codex" : raw.limitId;
    if (typeof id !== "string" || !/^[A-Za-z0-9._:-]{1,64}$/.test(id)) return;
    if (!this.buckets.has(id) && this.buckets.size === 4) return;
    const type = raw.rateLimitReachedType;
    const snapshot: Snapshot = {
      primary: window(raw.primary), secondary: window(raw.secondary),
      reached: type == null ? "absent" :
        reachedTypes.includes(type as typeof reachedTypes[number]) ? type as Reached : "unrecognized",
      spend: typeof raw.spendControlReached === "boolean" ? raw.spendControlReached : undefined,
    };
    this.buckets.set(id, snapshot);
    this.latest = snapshot;
  }

  classify(authMode: CodexAppServerAuthMode | undefined): HarnessLimitEvidence | undefined {
    const snapshot = this.latest;
    if (authMode !== "subscription" || !snapshot || snapshot.spend === true ||
        (snapshot.reached !== "absent" && snapshot.reached !== "rate_limit_reached")) return undefined;
    const windows = [snapshot.primary, snapshot.secondary].filter((w): w is Window => w !== undefined);
    let selected = windows.filter(w => w.used >= 100);
    if (selected.length === 0) {
      if (snapshot.reached !== "rate_limit_reached") return undefined;
      const highest = Math.max(...windows.map(w => w.used));
      selected = windows.filter(w => w.used === highest);
    }
    // At most two selected windows. Missing resets suppress the reset entirely;
    // the latest reset-bearing window still supplies the duration hint.
    const bearing = selected.filter(w => w.reset !== undefined);
    const latest = bearing.reduce<Window | undefined>((a, b) =>
      !a || b.reset! > a.reset! ? b : a, undefined);
    return {
      explicitExhaustion: true,
      latest: {
        status: "rejected",
        resetsAtMs: selected.length > 0 && bearing.length === selected.length ? latest?.reset : undefined,
        window: duration(latest?.duration),
      },
    };
  }
}
