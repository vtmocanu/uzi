// The Overview preview's Capacity card (PRD #3880 M1): run slots in use across the viewer's
// own fleet, a segmented bar, and every owner worker as a row with its stats behind a
// disclosure. "Open" means advertised spare slots, not a promise that a run is claimed.

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
            <p className="mt-3 text-sm text-muted">
              <strong className="font-semibold tabular-nums text-fg">
                {slots.inUse} of {slots.total}
              </strong>{" "}
              run slots in use
            </p>
            <SlotBar slots={slots} />
            <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted">
              <span>In use {slots.inUse}</span>
              <span>Closed {slots.closed}</span>
              <span>Open {slots.open}</span>
            </div>
            <p className="mt-2 text-xs text-faint">
              Open means advertised spare slots; credentials, cordons and admission still decide whether a run is claimed.
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

const CELL_IN_USE = "bg-info";
const CELL_CLOSED = "bg-edge-strong";
const CELL_OPEN = "border border-edge-strong bg-edge";

// Discrete cells for a small fleet, proportional segments otherwise so the bar never
// overflows. Decorative: the headline and legend carry the same numbers as text.
function SlotBar({ slots }: { slots: CapacitySlots }) {
  const { inUse, closed, open, total } = slots;
  if (total <= 0) return null;
  if (total <= 12) {
    const cells = [
      ...Array.from({ length: inUse }, () => CELL_IN_USE),
      ...Array.from({ length: closed }, () => CELL_CLOSED),
      ...Array.from({ length: open }, () => CELL_OPEN),
    ];
    return (
      <div aria-hidden="true" className="mt-2 flex items-center gap-1">
        {cells.map((c, i) => (
          <span key={i} className={cx("h-3 w-3 shrink-0 rounded-[3px]", c)} />
        ))}
      </div>
    );
  }
  const pct = (n: number) => `${(n / total) * 100}%`;
  return (
    <div aria-hidden="true" className="mt-2 flex h-3 w-full gap-px overflow-hidden rounded">
      <span className={CELL_IN_USE} style={{ width: pct(inUse) }} />
      <span className={CELL_CLOSED} style={{ width: pct(closed) }} />
      <span className={CELL_OPEN} style={{ width: pct(open) }} />
    </div>
  );
}

function WorkerRow({ worker: w }: { worker: Worker }) {
  const note = rollNote(w);
  return (
    <li className="py-2">
      <details className="group">
        <summary className="flex cursor-pointer list-none items-center gap-2 [&::-webkit-details-marker]:hidden">
          <div className="flex min-w-0 flex-1 flex-wrap items-center gap-2">
            <Badge tone={w.status === "online" ? "ok" : "neutral"} dot>
              {w.status}
            </Badge>
            <WorkerCordonBadge worker={w} />
            <WorkerRunBadge worker={w} hideIdleCrossChecks />
            <span className="min-w-0 max-w-full truncate text-sm font-medium text-fg">{stripUnsafeChars(w.name)}</span>
          </div>
          <ChevronRightIcon className="h-4 w-4 shrink-0 text-faint transition-transform group-open:rotate-90" aria-hidden="true" />
        </summary>
        <div className="mt-2 space-y-1 pl-1 text-xs text-muted">
          <div>{hasStats(w) ? <WorkerStatLine worker={w} /> : "No stats reported"}</div>
          {workerTakesNoNewRuns(w) && <p>Takes no new runs.</p>}
          {note && <p>{note}</p>}
        </div>
      </details>
    </li>
  );
}
