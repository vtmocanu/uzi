// The Overview preview's Capacity card (PRD #3880 M1): run slots in use across the viewer's
// own fleet, a segmented bar, and every owner worker as a row with its stats behind a
// disclosure. "Open" means advertised spare slots, not a promise that a run is claimed.

import type { CSSProperties } from "react";
import { Link } from "react-router-dom";
import type { Worker } from "../lib/api";
import { capacitySlots, rollNote, workerTakesNoNewRuns, type CapacitySlots } from "../lib/capacity";
import { stripUnsafeChars } from "../lib/safeText";
import { WorkerStatLine, hasStats } from "./WorkerStats";
import { WorkerCordonBadge } from "./WorkerCordonBadge";
import { WorkerRunBadge } from "./WorkerRunBadge";
import { ChevronRightIcon } from "./icons";
import { Badge, Card, SectionTitle, cx } from "./ui";

const linkClass = "text-xs font-medium text-brand hover:text-brand-hover";

export function CapacityRail({ workers }: { workers: Worker[] }) {
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
              Open slots are advertised spare capacity; admission still decides claims.
            </p>
            <Exclusions slots={slots} />
            <ul className="mt-3 divide-y divide-edge">
              {workers.map((w) => (
                <WorkerRow key={w.id} worker={w} />
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
// without relying on colour contrast between two greys.
const CELL_CLASS: Record<CellKind, string> = {
  "in-use": "bg-info",
  closed: "border border-edge-strong",
  open: "border border-edge-strong",
};
const CELL_STYLE: Record<CellKind, CSSProperties | undefined> = {
  "in-use": undefined,
  closed: { background: "repeating-linear-gradient(135deg, rgb(var(--edge-strong)) 0 2px, transparent 2px 5px)" },
  open: undefined,
};

function LegendItem({ kind, label }: { kind: CellKind; label: string }) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <span aria-hidden="true" className={cx("h-[9px] w-[9px] shrink-0 rounded-[2px]", CELL_CLASS[kind])} style={CELL_STYLE[kind]} />
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
          <span key={i} data-slot={k} className={cx("h-3 flex-1 rounded-[3px]", CELL_CLASS[k])} style={CELL_STYLE[k]} />
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
        <span key={k} data-slot={k} className={CELL_CLASS[k]} style={{ ...CELL_STYLE[k], width: pct(n) }} />
      ))}
    </div>
  );
}

function WorkerRow({ worker: w }: { worker: Worker }) {
  const note = rollNote(w);
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
            </div>
            <span data-worker-name className="min-w-[6rem] flex-1 truncate whitespace-nowrap text-sm font-medium text-fg">
              {stripUnsafeChars(w.name)}
            </span>
          </div>
          <ChevronRightIcon className="h-4 w-4 shrink-0 text-faint transition-transform group-open:rotate-90" aria-hidden="true" />
        </summary>
        <div className="mt-2 space-y-1 pl-1 text-xs text-muted">
          <div>{hasStats(w) ? <WorkerStatLine worker={w} /> : "No stats reported"}</div>
          {(workerTakesNoNewRuns(w) || note) && (
            <p>{[workerTakesNoNewRuns(w) && "Takes no new runs.", note && `${note}.`].filter(Boolean).join(" ")}</p>
          )}
        </div>
      </details>
    </li>
  );
}
