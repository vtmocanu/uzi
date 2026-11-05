// The Overview preview's Held work card (PRD #3880 M2): the owner's custody capacity and holds
// as three always-present figures, with the same headline and tone as the Dashboard's custody
// alert when custodyAlertView says there is something to say. It replaces CustodyBoardAlert on
// the preview, so the alert's own request is gone: the holds arrive as a prop from
// useOverviewData. A "Review held work" link appears only when a hold needs a decision.

import { Link } from "react-router-dom";
import type { RecoveryCustodyHolds } from "../lib/api";
import { custodyAlertView, recoveryCapacityUsed } from "../lib/recovery";
import { ChevronRightIcon } from "./icons";
import { Card, SectionTitle, cx } from "./ui";

export const HELD_WORK_HREF = "/workers?tab=workers#recovery-holds";

export function HeldWorkCard({
  holds,
  recoveryWaitCount,
}: {
  holds: RecoveryCustodyHolds | null;
  recoveryWaitCount: number;
}) {
  const title = (
    <SectionTitle>
      <span id="held-work-heading">Held work</span>{" "}
      <span className="font-normal normal-case tracking-normal">yours</span>
    </SectionTitle>
  );

  if (!holds) {
    return (
      <Card>
        <section aria-labelledby="held-work-heading">
          {title}
          <p className="mt-3 text-sm text-muted">Held work is unavailable right now.</p>
        </section>
      </Card>
    );
  }

  const agg = holds.aggregate;
  const used = recoveryCapacityUsed(agg);
  const view = custodyAlertView(agg, recoveryWaitCount);
  const danger = view?.tone === "danger";
  const accent = danger ? "text-danger" : "text-warn";
  const fill = view ? (danger ? "bg-danger" : "bg-warn") : "bg-info";
  const limit = agg.custody_hold_limit;
  const open = agg.open_holds;
  const decisions = agg.decision_needed;

  return (
    <Card>
      <section
        // Same live regions as the Dashboard alert: blocked runs are an alert, the other visible
        // states a status. With nothing to say there is no live region, only a labelled section.
        {...(view
          ? {
              role: danger ? "alert" : "status",
              "aria-live": danger ? "assertive" : "polite",
              "aria-label": "Held work",
            }
          : { "aria-labelledby": "held-work-heading" })}
      >
        <div className="flex items-center justify-between gap-2">{title}</div>

        {view && <h3 className={cx("mt-3 text-sm font-semibold", accent)}>{view.headline}</h3>}

        <SlotMeter used={used} limit={limit} fill={fill} />

        <div className="mt-3 grid grid-cols-3 gap-3">
          <Figure value={`${used} / ${limit}`} label="custody slots used" />
          <Figure value={String(open)} label={open === 1 ? "open custody hold" : "open custody holds"} />
          <Figure value={String(decisions)} label={decisions === 1 ? "hold needs a decision" : "holds need a decision"} />
        </div>

        {recoveryWaitCount > 0 && (
          <p className="mt-2 text-xs text-faint">
            {recoveryWaitCount} {recoveryWaitCount === 1 ? "run" : "runs"} waiting to recover
          </p>
        )}

        <p className="mt-3 text-xs text-faint">
          Holds protect each run&apos;s committed but unpushed work, while it runs and after a worker is lost.
          {limit > 0 && ` New runs wait for admission once all ${limit} custody slots are taken.`}
        </p>

        {view && view.decisionNeeded > 0 && (
          <div className="mt-3">
            {/* A Link styled as a button, never a Link wrapping a Button (two tab stops). */}
            <Link
              to={HELD_WORK_HREF}
              className={cx(
                "inline-flex h-7 shrink-0 select-none items-center justify-center gap-1 rounded-lg px-2.5 text-xs font-medium transition-colors",
                danger
                  ? "bg-brand text-on-brand hover:bg-brand-hover"
                  : "border border-edge bg-raised text-fg hover:border-edge-strong hover:bg-raised/70",
              )}
            >
              Review held work <ChevronRightIcon aria-hidden="true" />
            </Link>
          </div>
        )}
      </section>
    </Card>
  );
}

function Figure({ value, label }: { value: string; label: string }) {
  return (
    <div className="min-w-0">
      <strong className="block text-lg font-semibold tabular-nums text-fg">{value}</strong>
      <span className="text-xs text-muted">{label}</span>
    </div>
  );
}

// Discrete cells for a small positive limit, a proportional bar otherwise so it never
// overflows. Decorative: the figures carry the same numbers as text.
function SlotMeter({ used, limit, fill }: { used: number; limit: number; fill: string }) {
  if (limit > 0 && limit <= 12) {
    return (
      <div aria-hidden="true" className="mt-3 flex gap-1">
        {Array.from({ length: limit }, (_, i) => (
          <span key={i} className={cx("h-3 flex-1 rounded-[3px]", i < used ? fill : "bg-edge")} />
        ))}
      </div>
    );
  }
  const pct = limit > 0 ? Math.min(100, (used / limit) * 100) : 0;
  return (
    <div aria-hidden="true" className="mt-3 flex h-3 w-full overflow-hidden rounded bg-edge">
      <span className={fill} style={{ width: `${pct}%` }} />
    </div>
  );
}
