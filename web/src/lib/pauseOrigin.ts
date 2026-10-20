import type { Run, RunMessage } from "./api";

export type PauseOrigin = "" | "lead_park" | "owner";

// Validate RFC3339 calendar fields before Date parsing (which normalizes bad days),
// then retain nanoseconds separately: Date alone truncates sub-millisecond evidence.
function timestamp(value: unknown): bigint | null {
  if (typeof value !== "string") return null;
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:\d{2})$/.exec(value);
  if (!m) return null;
  const [, year, month, day, hour, minute, second, fraction = "", zone] = m;
  const y = Number(year), mo = Number(month), d = Number(day);
  const leap = y % 4 === 0 && (y % 100 !== 0 || y % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  if (mo < 1 || mo > 12 || d < 1 || d > days[mo - 1] ||
      Number(hour) > 23 || Number(minute) > 59 || Number(second) > 59 ||
      (zone !== "Z" && (Number(zone.slice(1, 3)) > 23 || Number(zone.slice(4)) > 59))) return null;
  const ms = Date.parse(`${year}-${month}-${day}T${hour}:${minute}:${second}${zone}`);
  if (!Number.isFinite(ms)) return null;
  const ns = BigInt(ms) * 1000000n + BigInt((fraction + "000000000").slice(0, 9));
  // Go time.Time.IsZero is the instant 0001-01-01T00:00:00Z, in any zone.
  return ns === -62135596800000000000n ? null : ns;
}

export function pauseOrigin(
  run: Pick<Run, "status" | "status_since" | "hold_reason">,
  messages: readonly RunMessage[],
  ready = false,
): PauseOrigin {
  if (!ready || run.status !== "paused" || run.hold_reason) return "";
  const since = timestamp(run.status_since);
  if (since === null) return "";
  let newest: RunMessage | undefined;
  for (const message of messages) {
    if (message.kind === "paused" && (!newest || message.seq > newest.seq)) newest = message;
  }
  if (!newest || newest.seq <= 0) return "";
  const at = timestamp(newest.created_at);
  if (at === null || at < since) return "";
  const payload: unknown = newest.payload;
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) return "";
  const origin = (payload as Record<string, unknown>).origin;
  return origin === "lead_park" || origin === "owner" ? origin : "";
}
