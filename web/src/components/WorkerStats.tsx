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
import { MeterTrack } from "./Meter";
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

/** The two disk volumes a worker reports (PRD #837), each a used/total byte pair.
 *  A volume is "present" only when BOTH its used and total are non-null — they arrive
 *  as a pair — so a worker can report /nix, /data, both, or neither independently of
 *  the mem sample. Returns each present volume with its used/total pct (total is always
 *  positive when present, so pctOf never falls back to null here). */
function diskVolumes(w: Worker): { label: string; used: number; total: number; pct: number }[] {
  const out: { label: string; used: number; total: number; pct: number }[] = [];
  const pairs: [string, number | null, number | null][] = [
    ["Disk /nix", w.stats_disk_nix_bytes, w.stats_disk_nix_total_bytes],
    ["Disk /data", w.stats_disk_data_bytes, w.stats_disk_data_total_bytes],
  ];
  for (const [label, used, total] of pairs) {
    if (used == null || total == null) continue;
    const pct = pctOf(used, total);
    if (pct == null) continue;
    out.push({ label, used, total, pct });
  }
  return out;
}

/** True once the worker has reported a usable sample. The single source of truth for
 *  "does this worker have stats to render" — the gauges, the compact line, and the
 *  Dashboard fleet card's filter all gate on this, so no surface can disagree about
 *  which workers show up (reviewer M3 nit). */
export function hasStats(w: Worker): boolean {
  return w.stats_source != null && w.stats_mem_bytes != null;
}

function Bar({ label, value, valueText, fillPct }: { label: string; value: string; valueText: string; fillPct: number }) {
  // The label row; MeterTrack (shared with the PRD #53 rate-limit meters) is the
  // accessible bar itself, keying tone/width/aria-valuenow off one clamped, rounded
  // integer. valueText is read by a screen reader instead of the bare "N percent":
  // the byte figures for memory, and "no reading yet" for a first-tick CPU.
  return (
    <div>
      <div className="flex items-center justify-between text-xs">
        {/* text-muted (not text-faint) so the label clears WCAG AA 4.5:1 at 12px
            (web-ux finding) and matches the value span. */}
        <span className="text-muted">{label}</span>
        <span className="tabular-nums text-muted">{value}</span>
      </div>
      <MeterTrack className="mt-1 h-1.5" label={label} fillPct={fillPct} valueText={valueText} />
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
      <Bar
        label="CPU"
        value={cpu == null ? "—" : `${Math.round(cpu)}%`}
        valueText={cpu == null ? "no reading yet" : `${Math.round(cpu)}%`}
        fillPct={cpu ?? 0}
      />
      {memPct != null ? (
        <Bar
          label="Memory"
          value={`${formatBytesPair(mem, limit!)} · ${Math.round(memPct)}%`}
          valueText={`${formatBytesPair(mem, limit!)}, ${Math.round(memPct)}%`}
          fillPct={memPct}
        />
      ) : (
        <div className="flex items-center justify-between text-xs">
          {/* Same contrast fix as the Bar label (web-ux); the no-limit case has no
              progressbar, so the byte count sits in plain, SR-readable text. */}
          <span className="text-muted">Memory</span>
          <span className="tabular-nums text-muted">
            {formatBytes(mem)}
            <span className="text-faint"> · no limit</span>
          </span>
        </div>
      )}
      {diskVolumes(worker).map((d) => (
        <Bar
          key={d.label}
          label={d.label}
          value={`${formatBytesPair(d.used, d.total)} · ${Math.round(d.pct)}%`}
          valueText={`${formatBytesPair(d.used, d.total)}, ${Math.round(d.pct)}%`}
          fillPct={d.pct}
        />
      ))}
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
  // The fuller of the two reported volumes (highest used/total pct) stands in for disk
  // on the one-liner; omitted entirely when no volume is reported (no dangling "· disk").
  const fullestDisk = diskVolumes(worker).sort((a, b) => b.pct - a.pct)[0];
  return (
    <span
      className={cx("tabular-nums text-xs text-faint", offline && "opacity-50")}
      title={worker.stats_source === "process" ? "measures the worker process only" : undefined}
    >
      cpu {cpu == null ? "—" : `${Math.round(cpu)}%`} · mem {memText}
      {fullestDisk && <> · disk {formatBytesPair(fullestDisk.used, fullestDisk.total)}</>}
    </span>
  );
}
