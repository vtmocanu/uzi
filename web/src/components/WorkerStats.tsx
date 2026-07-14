// Live worker resource gauges (PRD #49). A worker self-reports container CPU/mem on
// its heartbeat; the API stores the latest sample on the worker DTO (stats_* fields).
// This renders it two ways: WorkerStatGauges (bars, for Settings → Workers) and
// WorkerStatLine (a compact one-liner, for denser lists).
//
// Both are display-only and defensive: they render nothing until a sample exists,
// dim when the worker is offline (last-known, never live-looking), clamp bar widths
// to 100% regardless of the stored value (the server accepts up to 6400% cpu_pct),
// and label a process-source sample "worker process only".

import { cx } from "./ui";
import type { Worker } from "../lib/api";

const KIB = 1024;
const MIB = 1024 * KIB;
const GIB = 1024 * MIB;
const TIB = 1024 * GIB;

/** A binary unit + divisor for a byte count. */
function unitFor(bytes: number): { div: number; unit: string } {
  if (bytes >= TIB) return { div: TIB, unit: "TiB" };
  if (bytes >= GIB) return { div: GIB, unit: "GiB" };
  if (bytes >= MIB) return { div: MIB, unit: "MiB" };
  if (bytes >= KIB) return { div: KIB, unit: "KiB" };
  return { div: 1, unit: "B" };
}

/** One decimal, trailing ".0" trimmed: 4.0 → "4", 2.13 → "2.1". */
function trim(v: number): string {
  return (v >= 100 ? Math.round(v).toString() : v.toFixed(1)).replace(/\.0$/, "");
}

/** A byte count in its own unit ("2.1 GiB", "512 MiB"). */
export function formatBytes(bytes: number): string {
  const { div, unit } = unitFor(bytes);
  return `${trim(bytes / div)} ${unit}`;
}

/** "used/limit unit" sharing the limit's unit ("2.1/4 GiB"). */
export function formatBytesPair(used: number, limit: number): string {
  const { div, unit } = unitFor(limit);
  return `${trim(used / div)}/${trim(limit / div)} ${unit}`;
}

/** Percent of a limit, or null when the limit is unknown/zero (⇒ no percentage bar). */
function pctOf(used: number, limit: number | null): number | null {
  if (limit == null || limit <= 0) return null;
  return (used / limit) * 100;
}

/** Clamp a percentage to a DOM-safe [0,100] bar width — the server accepts up to
 *  6400% cpu_pct, so the bar must never overflow its track (PRD #49 Decision 6). */
function clampPct(pct: number): number {
  return Math.max(0, Math.min(100, pct));
}

/** Warn ≥80%, danger ≥95% (PRD #49 Decision 6). Applied to both bars by their fill
 *  fraction: a worker pinning its CPU allowance is as worth flagging as one near its
 *  memory limit. */
function toneFor(pct: number): "ok" | "warn" | "danger" {
  if (pct >= 95) return "danger";
  if (pct >= 80) return "warn";
  return "ok";
}

const FILL: Record<"ok" | "warn" | "danger", string> = {
  ok: "bg-ok",
  warn: "bg-warn",
  danger: "bg-danger",
};

/** True once the worker has reported a usable sample. The single source of truth for
 *  "does this worker have stats to render" — the gauges, the compact line, and the
 *  Dashboard fleet card's filter all gate on this, so no surface can disagree about
 *  which workers show up (reviewer M3 nit). */
export function hasStats(w: Worker): boolean {
  return w.stats_source != null && w.stats_mem_bytes != null;
}

function Bar({ label, value, fillPct }: { label: string; value: string; fillPct: number }) {
  // Tone, width, and aria-valuenow all key off the same clamped, rounded integer, so
  // the colour can never disagree with the percentage the label shows (a 94.99% that
  // rounds to "95%" is danger, not warn).
  const now = Math.round(clampPct(fillPct));
  const tone = toneFor(now);
  return (
    <div>
      <div className="flex items-center justify-between text-xs">
        <span className="text-faint">{label}</span>
        <span className="tabular-nums text-muted">{value}</span>
      </div>
      <div
        className="mt-1 h-1.5 overflow-hidden rounded-full bg-raised"
        role="progressbar"
        aria-label={label}
        aria-valuenow={now}
        aria-valuemin={0}
        aria-valuemax={100}
      >
        <div className={cx("h-full rounded-full", FILL[tone])} style={{ width: `${now}%` }} />
      </div>
    </div>
  );
}

/**
 * The full gauge block: a CPU bar plus a memory bar ("used / limit · %") when a limit
 * is known, else an absolute memory readout with no bar. Renders nothing until the
 * worker has reported a sample. Dimmed + labeled last-known when the worker is offline.
 */
export function WorkerStatGauges({ worker }: { worker: Worker }) {
  if (!hasStats(worker)) return null;
  const offline = worker.status !== "online";
  const isProcess = worker.stats_source === "process";
  const cpu = worker.stats_cpu_pct;
  const mem = worker.stats_mem_bytes!;
  const limit = worker.stats_mem_limit_bytes;
  const memPct = pctOf(mem, limit);

  return (
    <div
      className={cx("space-y-2", offline && "opacity-50")}
      title={isProcess ? "measures the worker process only" : undefined}
      aria-label={offline ? "last-known resource usage (worker offline)" : "resource usage"}
    >
      <Bar label="CPU" value={cpu == null ? "—" : `${Math.round(cpu)}%`} fillPct={cpu ?? 0} />
      {memPct != null ? (
        <Bar label="Memory" value={`${formatBytesPair(mem, limit!)} · ${Math.round(memPct)}%`} fillPct={memPct} />
      ) : (
        <div className="flex items-center justify-between text-xs">
          <span className="text-faint">Memory</span>
          <span className="tabular-nums text-muted">
            {formatBytes(mem)}
            <span className="text-faint"> · no limit</span>
          </span>
        </div>
      )}
      {isProcess && <p className="text-[0.7rem] text-faint">worker process only</p>}
    </div>
  );
}

/**
 * A compact one-liner "cpu 34% · mem 2.1/4 GiB" for denser lists. Same display-only
 * rules as the gauges: nothing until a sample exists, dimmed when offline, and the
 * process-source tooltip.
 */
export function WorkerStatLine({ worker }: { worker: Worker }) {
  if (!hasStats(worker)) return null;
  const offline = worker.status !== "online";
  const cpu = worker.stats_cpu_pct;
  const mem = worker.stats_mem_bytes!;
  const limit = worker.stats_mem_limit_bytes;
  const memText = limit != null && limit > 0 ? formatBytesPair(mem, limit) : formatBytes(mem);
  return (
    <span
      className={cx("tabular-nums text-xs text-faint", offline && "opacity-50")}
      title={worker.stats_source === "process" ? "measures the worker process only" : undefined}
    >
      cpu {cpu == null ? "—" : `${Math.round(cpu)}%`} · mem {memText}
    </span>
  );
}
