// The Overview preview's Capacity card (PRD #3880 M1): run slots in use across the viewer's
// own fleet, a segmented bar, and every owner worker as a row with its stats behind a
// disclosure. "Open" means advertised spare slots, not a promise that a run is claimed.

import { Link } from "react-router-dom";
import type { RunSummaryItem, Worker } from "../lib/api";
import { capacitySlots, rollNote, workerTakesNoNewRuns, type CapacitySlots } from "../lib/capacity";
import { formatUptimeSince } from "../lib/formatUptimeSince";
import { formatAgo } from "../lib/rateLimits";
import { stripUnsafeChars } from "../lib/safeText";
import { WorkerDiskLine, WorkerStatGauges, diskDangerPct, hasStats, runHomeSize } from "./WorkerStats";
import { HELD_WORK_HREF } from "./HeldWorkCard";
import { WorkerCordonBadge } from "./WorkerCordonBadge";
import { EPHEMERAL_WORKER_TITLE, WorkerLeaseNote } from "./WorkerLeaseBadge";
import { WorkerMemoryAuthorityBadge } from "./WorkerMemoryAuthorityBadge";
import { WorkerQuarantineBadge } from "./WorkerQuarantineBadge";
import { WorkerRunBadge } from "./WorkerRunBadge";
import { WorkerUpgradeBadge, workerFailed } from "./WorkerUpgradeBadge";
import { ChevronRightIcon } from "./icons";
import { Badge, Card, SectionTitle, cx } from "./ui";

const linkClass = "text-xs font-medium text-brand hover:text-brand-hover";

export function CapacityRail({ workers, runs, now }: { workers: Worker[]; runs: RunSummaryItem[]; now: number }) {
  const slots = capacitySlots(workers);
  return (
    <Card>
      <section aria-labelledby="capacity-heading">
        <div className="flex items-center justify-between gap-2">
          <SectionTitle>
            <span id="capacity-heading">Capacity</span>{" "}
            <span className="font-normal normal-case tracking-normal">your fleet</span>
          </SectionTitle>
          <Link to="/workers" className={linkClass}>
            Workers →
          </Link>
        </div>

        {workers.length === 0 ? (
          <p className="mt-3 text-sm text-muted">
            No workers yet.{" "}
            <Link to="/workers" className="font-medium text-brand hover:text-brand-hover">
              Add one on the Workers page
            </Link>
          </p>
        ) : (
          <>
            <p className="mt-3 flex flex-wrap items-baseline gap-x-2">
              <strong className="text-2xl font-semibold tabular-nums text-fg">
                {slots.inUse} of {slots.total}
              </strong>
              <span className="text-sm text-faint">run slots in use</span>
            </p>
            <SlotBar slots={slots} />
            <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted">
              <LegendItem kind="in-use" label={`In use ${slots.inUse}`} />
              <LegendItem kind="closed" label={`Closed ${slots.closed}`} />
              <LegendItem kind="open" label={`Open ${slots.open}`} />
            </div>
            <p className="mt-2 text-xs text-faint">
              Open slots are advertised spare capacity; credentials, cordons and admission still decide claims.
            </p>
            <Exclusions slots={slots} />
            <ul className="mt-3 divide-y divide-edge">
              {workers.map((w) => (
                <WorkerRow key={w.id} worker={w} runs={runs} now={now} />
              ))}
            </ul>
          </>
        )}
      </section>
    </Card>
  );
}

function Exclusions({ slots }: { slots: CapacitySlots }) {
  const parts: string[] = [];
  if (slots.excludedOffline > 0) parts.push(`${slots.excludedOffline} offline`);
  if (slots.excludedUnknownCap > 0) parts.push(`${slots.excludedUnknownCap} with no advertised slot cap`);
  if (parts.length === 0 && slots.overCap === 0) return null;
  return (
    <div className="mt-2 space-y-1 text-xs text-muted">
      {parts.length > 0 && <p>Not counted: {parts.join(", ")}</p>}
      {slots.overCap > 0 && (
        <p>
          {slots.overCap} {slots.overCap === 1 ? "run" : "runs"} over advertised caps
        </p>
      )}
    </div>
  );
}

type CellKind = "in-use" | "closed" | "open";

// In use is solid, closed is hatched, open is an outline only, so the three read apart
// without relying on colour contrast between two greys. The hatch is a class over the
// edge-strong token, so inline style stays limited to the bar widths.
const CLOSED_HATCH = "bg-[repeating-linear-gradient(135deg,rgb(var(--edge-strong))_0_2px,transparent_2px_5px)]";
const CELL_CLASS: Record<CellKind, string> = {
  "in-use": "bg-info",
  closed: `border border-edge-strong ${CLOSED_HATCH}`,
  open: "border border-edge-strong",
};

function LegendItem({ kind, label }: { kind: CellKind; label: string }) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <span aria-hidden="true" className={cx("h-[9px] w-[9px] shrink-0 rounded-[2px]", CELL_CLASS[kind])} />
      {label}
    </span>
  );
}

// Equal flex cells for a small fleet, proportional segments otherwise so the bar never
// overflows. Decorative: the headline and legend carry the same numbers as text.
function SlotBar({ slots }: { slots: CapacitySlots }) {
  const { inUse, closed, open, total } = slots;
  if (total <= 0) return null;
  if (total <= 12) {
    const cells: CellKind[] = [
      ...Array.from({ length: inUse }, () => "in-use" as const),
      ...Array.from({ length: closed }, () => "closed" as const),
      ...Array.from({ length: open }, () => "open" as const),
    ];
    return (
      <div aria-hidden="true" className="mt-2 flex gap-1">
        {cells.map((k, i) => (
          <span key={i} data-slot={k} className={cx("h-3 flex-1 rounded-[3px]", CELL_CLASS[k])} />
        ))}
      </div>
    );
  }
  const pct = (n: number) => `${(n / total) * 100}%`;
  const segments: [CellKind, number][] = [
    ["in-use", inUse],
    ["closed", closed],
    ["open", open],
  ];
  return (
    <div aria-hidden="true" className="mt-2 flex h-3 w-full gap-px overflow-hidden rounded">
      {segments.filter(([, n]) => n > 0).map(([k, n]) => (
        <span key={k} data-slot={k} className={CELL_CLASS[k]} style={{ width: pct(n) }} />
      ))}
    </div>
  );
}

const MAX_SIZED_RUNS = 5;

interface HostedRunRow {
  runId: string;
  phase: string;
  size: string | null;
}

// What the worker says it is executing, minus pending outcomes (not live work). Sizes come
// from the five largest run_disk samples; a reported run with no sample is still listed,
// after the sized ones, in report order. Largest first among the sized.
function hostedRunRows(w: Worker): HostedRunRow[] {
  const live = (w.reported_runs ?? []).filter((r) => r.terminal_pending !== true);
  const sizes = new Map(
    [...(w.run_disk ?? [])]
      .sort((a, b) => b.home_bytes - a.home_bytes)
      .slice(0, MAX_SIZED_RUNS)
      .map((d) => [d.run_id, { bytes: d.home_bytes, label: runHomeSize(d.home_bytes, d.truncated) }] as const),
  );
  const rows = live.map((r, i) => ({
    i,
    bytes: sizes.get(r.run_id)?.bytes ?? null,
    row: {
      runId: r.run_id,
      phase: stripUnsafeChars(r.phase).replace(/_/g, " "),
      size: sizes.get(r.run_id)?.label ?? null,
    },
  }));
  rows.sort((a, b) => {
    if (a.bytes != null && b.bytes != null) return b.bytes - a.bytes || a.i - b.i;
    if (a.bytes != null) return -1;
    if (b.bytes != null) return 1;
    return a.i - b.i;
  });
  return rows.map((r) => r.row);
}

function RunIdentity({ runId, runs }: { runId: string; runs: RunSummaryItem[] }) {
  const safeId = stripUnsafeChars(runId);
  const shortId = safeId.slice(0, 8);
  const run = runs.find((r) => r.id === runId);
  if (!run) {
    return (
      <span className="truncate font-mono" title={safeId}>
        run {shortId}
      </span>
    );
  }
  const title = stripUnsafeChars(run.issue_title ?? "").trim();
  const label = title ? (run.issue_iid != null ? `#${run.issue_iid} ${title}` : title) : shortId;
  return (
    <Link to={`/runs/${run.id}`} title={label} className="truncate font-medium text-brand hover:text-brand-hover">
      {label}
    </Link>
  );
}

function HostedRuns({ worker: w, runs }: { worker: Worker; runs: RunSummaryItem[] }) {
  const rows = hostedRunRows(w);
  if (rows.length === 0) return null;
  const offline = w.status !== "online";
  return (
    <div className={cx(offline && "opacity-50")}>
      <p className="text-[0.7rem] font-semibold uppercase tracking-wide text-faint">{offline ? "Last reported" : "On this worker"}</p>
      <div className="mt-1 space-y-0.5">
        {rows.map((r, i) => (
          <p key={`${r.runId}:${i}`} data-run-row className="grid grid-cols-[minmax(0,1fr)_auto] items-baseline gap-x-2">
            <RunIdentity runId={r.runId} runs={runs} />
            <span className="whitespace-nowrap font-mono text-faint">{r.size ? `${r.phase} · ${r.size}` : r.phase}</span>
          </p>
        ))}
      </div>
    </div>
  );
}

function footerParts(w: Worker, now: number): string[] {
  const parts: string[] = [];
  const v = stripUnsafeChars(w.version ?? "").trim();
  if (v) parts.push(`v${v}`);
  if (w.status === "online") {
    if (typeof w.online_since === "string") {
      const u = formatUptimeSince(w.online_since, now);
      if (u) parts.push(`up ${u}`);
    }
  } else if (w.last_heartbeat_at === null) {
    parts.push("never seen");
  } else if (typeof w.last_heartbeat_at === "string" && !Number.isNaN(Date.parse(w.last_heartbeat_at))) {
    parts.push(`last seen ${formatAgo(w.last_heartbeat_at, now)}`);
  }
  return parts;
}

function WorkerRow({ worker: w, runs, now }: { worker: Worker; runs: RunSummaryItem[]; now: number }) {
  const note = rollNote(w);
  const disk = diskDangerPct(w);
  const offline = w.status !== "online";
  const footer = footerParts(w, now);
  return (
    <li className="py-2">
      <details className="group">
        <summary className="flex cursor-pointer list-none items-center gap-2 rounded-sm py-1 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/60 [&::-webkit-details-marker]:hidden">
          <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-2 gap-y-1">
            <div className="flex flex-wrap items-center gap-2">
              <Badge tone={w.status === "online" ? "ok" : "neutral"} dot>
                {w.status}
              </Badge>
              <WorkerCordonBadge worker={w} />
              <WorkerRunBadge worker={w} hideIdleCrossChecks />
              <WorkerQuarantineBadge worker={w} nowMs={now} />
              <WorkerMemoryAuthorityBadge worker={w} nowMs={now} />
              {workerFailed(w) && <WorkerUpgradeBadge worker={w} />}
              {disk && (
                <Badge
                  tone="danger"
                  title={`${disk.label} ${disk.pct}% full${disk.inodes ? " (inodes)" : ""}${offline ? " (last known)" : ""}`}
                >
                  disk {disk.pct}%
                </Badge>
              )}
            </div>
            <span data-worker-name className="min-w-[5rem] flex-1 truncate whitespace-nowrap text-sm font-medium text-fg">
              {stripUnsafeChars(w.name)}
            </span>
          </div>
          <ChevronRightIcon className="h-4 w-4 shrink-0 text-faint transition-transform group-open:rotate-90" aria-hidden="true" />
        </summary>
        <div className="mt-2 space-y-2 pl-1 text-xs text-muted">
          {hasStats(w) ? (
            <WorkerStatGauges worker={w} showLargestRun={false} />
          ) : (
            <div className="space-y-1">
              <div>No stats reported</div>
              <div>
                <WorkerDiskLine worker={w} />
              </div>
            </div>
          )}
          {offline && hasStats(w) && <p>Last known readings. Not counted in run slots while offline.</p>}
          <HostedRuns worker={w} runs={runs} />
          {(workerTakesNoNewRuns(w) || note) && (
            <p>{[workerTakesNoNewRuns(w) && "Takes no new runs.", note && `${note}.`].filter(Boolean).join(" ")}</p>
          )}
          {w.ephemeral === true && <p>{EPHEMERAL_WORKER_TITLE}</p>}
          <WorkerLeaseNote worker={w} />
          {(w.custody_decisions_needed ?? 0) > 0 && (
            <p>
              Retaining unpublished committed work that needs an owner decision.{" "}
              <Link to={HELD_WORK_HREF} className="font-medium text-brand hover:text-brand-hover">
                Review held work →
              </Link>
            </p>
          )}
          {w.memory_authority_lost_at && (
            <p>Guard-bound runs are withheld from this worker. Restart the worker's container to regain memory authority.</p>
          )}
          {footer.length > 0 && <p className="font-mono text-[0.7rem] text-faint">{footer.join(" · ")}</p>}
        </div>
      </details>
    </li>
  );
}
