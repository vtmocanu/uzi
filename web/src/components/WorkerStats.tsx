// Live worker resource gauges (PRD #49). A worker self-reports container CPU/mem on
// its heartbeat; the API stores the latest sample on the worker DTO (stats_* fields).
// This renders it two ways: WorkerStatGauges (bars, for Settings → Workers) and
// WorkerStatLine (a compact one-liner, for denser lists).
//
// Both are display-only and defensive: they render nothing until a sample exists,
// dim when the worker is offline (last-known, never live-looking), clamp bar widths
// to 100% regardless of the stored value (the server accepts up to 6400% cpu_pct),
// and label a process-source sample "worker process only".

import { useId } from "react";
import { cx } from "./ui";
import { MeterTrack } from "./Meter";
import type { Worker, WorkerRunDisk } from "../lib/api";

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

/** One reported disk volume. `pct` is the bar fill: the bytes ratio, or the inode ratio
 *  when that is fuller (the dind and data volumes report inodes). `inodePct` is set only when
 *  the ROUNDED inode percent exceeds the rounded bytes percent, i.e. exactly when it
 *  explains the displayed fill (78.2% bytes / 78.4% inodes is not "78% · inodes 78%"). */
interface DiskVolume {
  label: string;
  hint?: string;
  used: number;
  total: number;
  bytesPct: number;
  inodePct: number | null;
  pct: number;
}

/** The disk volumes a worker reports: /nix and /data (PRD #837), plus the docker-tier
 *  DinD data root (issue #1759). A volume is "present" only when BOTH its used and total
 *  bytes are non-null — they arrive as a pair — so a worker can report any subset
 *  independently of the mem sample. A present volume with a zero (or negative) total has
 *  no meaningful ratio, so pctOf returns null and the volume is skipped rather than drawn
 *  as an empty bar. The dind volume also carries an inode pair; a malformed one (either
 *  side null, or a zero total) is ignored rather than skewing the bar. */
function diskVolumes(w: Worker): DiskVolume[] {
  const out: DiskVolume[] = [];
  const vols: { label: string; hint?: string; used: number | null; total: number | null; inodes?: number | null; totalInodes?: number | null }[] = [
    { label: "Disk /nix", used: w.stats_disk_nix_bytes, total: w.stats_disk_nix_total_bytes },
    {
      label: "Disk /data",
      used: w.stats_disk_data_bytes,
      total: w.stats_disk_data_total_bytes,
      // PRD #1809 M6: the data volume reports inodes too, so a volume full of small cache
      // files reads as full even when its bytes look roomy.
      inodes: w.stats_disk_data_inodes,
      totalInodes: w.stats_disk_data_total_inodes,
    },
    {
      label: "Disk dind",
      hint: "docker daemon data (images, layers, containers)",
      used: w.stats_disk_dind_bytes,
      total: w.stats_disk_dind_total_bytes,
      inodes: w.stats_disk_dind_inodes,
      totalInodes: w.stats_disk_dind_total_inodes,
    },
  ];
  for (const { label, hint, used, total, inodes, totalInodes } of vols) {
    if (used == null || total == null) continue;
    const bytesPct = pctOf(used, total);
    if (bytesPct == null) continue;
    const rawInodePct = inodes == null ? null : pctOf(inodes, totalInodes ?? null);
    // Compare what is displayed, not the raw ratios: inodes "dominate" only when they
    // would read as a higher whole percent than the bytes beside them.
    const inodePct = rawInodePct != null && Math.round(rawInodePct) > Math.round(bytesPct) ? rawInodePct : null;
    out.push({ label, hint, used, total, bytesPct, inodePct, pct: inodePct ?? bytesPct });
  }
  return out;
}

/** The worker's largest run by HOME size (PRD #1809 M6), or null when it reports none. The
 *  server sends run_disk largest first; the max is taken anyway so the order is not load-bearing. */
function largestRun(w: Worker): WorkerRunDisk | null {
  const runs = w.run_disk ?? [];
  if (runs.length === 0) return null;
  return runs.reduce((a, b) => (b.home_bytes > a.home_bytes ? b : a));
}

/** "4.2 GiB" or "at least 4.2 GiB" when the worker's size walk was truncated. */
function runBytes(bytes: number, truncated: boolean): string {
  return truncated ? `at least ${formatBytes(bytes)}` : formatBytes(bytes);
}

/** The largest run's HOME and cache size under the disk bars (PRD #1809 M6), so one run growing
 *  toward filling the data volume is visible before it does. Nothing when no size is reported. */
function LargestRun({ worker }: { worker: Worker }) {
  const run = largestRun(worker);
  if (!run) return null;
  return (
    <div className="flex items-center justify-between text-xs" title={`run ${run.run_id}`}>
      <span className="text-muted">Largest run</span>
      <span className="tabular-nums text-muted">
        {runBytes(run.home_bytes, run.truncated)} home
        <span className="text-faint"> · {formatBytes(run.cache_bytes)} cache</span>
      </span>
    </div>
  );
}

/** " · inodes 98%" when inodes (not bytes) are what fill the bar, else "". */
function inodeSuffix(d: DiskVolume, sep: string): string {
  return d.inodePct == null ? "" : `${sep}inodes ${Math.round(d.inodePct)}%`;
}

/** True once the worker has reported a usable sample. The single source of truth for
 *  "does this worker have stats to render" — the gauges, the compact line, and the
 *  Dashboard fleet card's filter all gate on this, so no surface can disagree about
 *  which workers show up (reviewer M3 nit). */
export function hasStats(w: Worker): boolean {
  return w.stats_source != null && w.stats_mem_bytes != null;
}

function Bar({ label, hint, value, valueText, fillPct }: { label: string; hint?: string; value: string; valueText: string; fillPct: number }) {
  // The label row; MeterTrack (shared with the PRD #53 rate-limit meters) is the
  // accessible bar itself, keying tone/width/aria-valuenow off one clamped, rounded
  // integer. valueText is read by a screen reader instead of the bare "N percent":
  // the byte figures for memory, and "no reading yet" for a first-tick CPU. A hint is a
  // hover title for sighted users and, via aria-describedby on the bar, a description for
  // assistive tech (the accessible name stays the bare label).
  const hintId = useId();
  return (
    <div>
      <div className="flex items-center justify-between text-xs">
        {/* text-muted (not text-faint) so the label clears WCAG AA 4.5:1 at 12px
            (web-ux finding) and matches the value span. */}
        <span className="text-muted" title={hint}>
          {label}
        </span>
        <span className="tabular-nums text-muted">{value}</span>
      </div>
      <MeterTrack
        className="mt-1 h-1.5"
        label={label}
        fillPct={fillPct}
        valueText={valueText}
        describedBy={hint ? hintId : undefined}
      />
      {hint && (
        <span id={hintId} className="sr-only">
          {hint}
        </span>
      )}
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
          hint={d.hint}
          value={`${formatBytesPair(d.used, d.total)} · ${Math.round(d.bytesPct)}%${inodeSuffix(d, " · ")}`}
          valueText={`${formatBytesPair(d.used, d.total)}, ${Math.round(d.bytesPct)}%${inodeSuffix(d, ", ")}`}
          fillPct={d.pct}
        />
      ))}
      <LargestRun worker={worker} />
      {isProcess && <p className="text-[0.7rem] text-faint">worker process only</p>}
    </div>
  );
}

/** Disk-only compact readout, independent of the CPU/memory sample. */
export function WorkerDiskLine({ worker }: { worker: Worker }) {
  const disk = diskVolumes(worker).sort((a, b) => b.pct - a.pct)[0];
  if (!disk) return null;
  return (
    <span className={cx("tabular-nums text-xs text-faint", worker.status !== "online" && "opacity-50")}>
      disk {formatBytesPair(disk.used, disk.total)}
      {disk.inodePct != null && ` (inodes ${Math.round(disk.inodePct)}%)`}
    </span>
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
  // The fullest reported volume (highest fill pct, inode-aware for dind) stands in for
  // disk on the one-liner; omitted entirely when no volume is reported (no dangling
  // "· disk"). When inodes are what fill it, say so, or "8/20 GiB" would read as roomy.
  const fullestDisk = diskVolumes(worker).sort((a, b) => b.pct - a.pct)[0];
  return (
    <span
      className={cx("tabular-nums text-xs text-faint", offline && "opacity-50")}
      title={worker.stats_source === "process" ? "measures the worker process only" : undefined}
    >
      cpu {cpu == null ? "—" : `${Math.round(cpu)}%`} · mem {memText}
      {fullestDisk && (
        <>
          {" "}
          · disk {formatBytesPair(fullestDisk.used, fullestDisk.total)}
          {fullestDisk.inodePct != null && ` (inodes ${Math.round(fullestDisk.inodePct)}%)`}
        </>
      )}
    </span>
  );
}
