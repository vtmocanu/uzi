import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";

import { api, type RecoveryCustodyHolds } from "../lib/api";
import { custodyAlertView } from "../lib/recovery";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";
import { cx } from "./ui";
import { ShieldIcon, ChevronRightIcon } from "./icons";

// CustodyBoardAlert is the board's conditional custody-pressure alert (PRD #1349 M6, D8).
// It is mounted once, at the top of the dashboard, and it SELF-HIDES: nothing renders until
// a hold needs an owner decision, a run is blocked, or the admission limit is reached — a
// fleet with only healthy active protection shows nothing (D6). At the full admission limit
// it ESCALATES from warning to error styling, because at the limit every further code-run
// claim for the owner stops (D8).
//
// It owns its own data fetch (GET /api/recovery/holds) and refreshes on the same visible
// poll cadence as the dashboard, so it updates live as holds change (D10 — the web state
// tracks the reconciler without a reload). `recoveryWaitCount` is derived CLIENT-SIDE by the
// dashboard (counting the owner's runs in recovery_wait) and threaded in for diagnosis; it
// never triggers the alert on its own (PRD #1349 scope: no lifetime cap on that count).
//
// A single instance means there is exactly one alert — no dedup logic is needed, and no
// second custody alarm competes with it.
export function CustodyBoardAlert({ recoveryWaitCount }: { recoveryWaitCount: number }) {
  const [holds, setHolds] = useState<RecoveryCustodyHolds | null>(null);

  const load = useCallback(async () => {
    try {
      setHolds(await api.getRecoveryHolds());
    } catch {
      // Best-effort: a fetch failure keeps the last-good aggregate (or stays hidden on the
      // first load). The alert must never break the dashboard.
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);
  usePollWhileVisible(load, 10000);

  if (!holds) return null;
  const view = custodyAlertView(holds.aggregate, recoveryWaitCount);
  if (!view) return null;

  const danger = view.tone === "danger";
  const accent = danger ? "text-danger" : "text-warn";

  return (
    <section
      // Danger (claims blocked) is an alert; the warning tier is a status region. Both are
      // live so an update announces while the region stays mounted.
      role={danger ? "alert" : "status"}
      aria-live={danger ? "assertive" : "polite"}
      aria-label="Held work"
      className={cx(
        "rounded-xl border p-4",
        danger ? "border-danger/50 bg-danger/10" : "border-warn/40 bg-warn/10",
      )}
    >
      <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
        <div className="flex min-w-0 items-start gap-3">
          <ShieldIcon className={cx("mt-0.5 h-5 w-5 shrink-0", accent)} aria-hidden="true" />
          <div className="min-w-0 space-y-2">
            <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
              <h2 className={cx("text-sm font-semibold", accent)}>{view.headline}</h2>
              <SlotMeter used={view.slotsUsed} limit={view.slotsLimit} danger={danger} />
              <span className="text-xs tabular-nums text-muted">{view.slotsLabel}</span>
            </div>
            <p className="text-sm text-muted">
              {danger
                ? "Every custody slot is in use, so new code runs cannot claim a worker until you resolve held work."
                : "Some runs are retaining unpublished committed work that only you can resolve."}
            </p>
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted">
              {/* Each count line renders only when its value > 0 (matching recoveryWaitCount),
                  so the alert never shows "0 holds need a decision" or "0 runs blocked". The
                  self-hide/escalation decision still lives in custodyAlertView. */}
              {view.decisionNeeded > 0 && (
                <Count value={view.decisionNeeded} singular="hold needs a decision" plural="holds need a decision" />
              )}
              {view.blockedRuns > 0 && (
                <Count value={view.blockedRuns} singular="run blocked" plural="runs blocked" />
              )}
              {view.recoveryWaitCount > 0 && (
                <Count
                  value={view.recoveryWaitCount}
                  singular="run waiting to recover"
                  plural="runs waiting to recover"
                  muted
                />
              )}
            </div>
          </div>
        </div>
        <div className="shrink-0">
          {/* One primary action (D8): the Workers resolution surface. A <Link> STYLED as a
              button (never a <Link> wrapping a <Button>): a nested anchor+button is two tab
              stops and a doubled screen-reader announcement, and invalid HTML. The global
              a:focus-visible rule (index.css) gives it the same keyboard ring. */}
          <Link
            to="/workers?tab=workers#recovery-holds"
            className={cx(
              "inline-flex h-7 shrink-0 select-none items-center justify-center gap-1 rounded-lg px-2.5 text-xs font-medium transition-colors",
              danger
                ? "bg-brand text-on-brand hover:bg-brand-hover"
                : "border border-edge bg-raised text-fg hover:border-edge-strong hover:bg-raised/70",
            )}
          >
            Review held work <ChevronRightIcon />
          </Link>
        </div>
      </div>
    </section>
  );
}

// SlotMeter is the signature element: a row of custody "safety slots" filling toward a hard
// wall. `limit` slots, `used` filled. Small counts render as discrete cells (the incident is
// about the exact slot count); a large/absent limit falls back to a proportional bar so the
// row never overflows. Decorative — the adjacent text carries the same numbers for AT users.
function SlotMeter({ used, limit, danger }: { used: number; limit: number; danger: boolean }) {
  const fill = danger ? "bg-danger" : "bg-warn";
  if (limit > 0 && limit <= 12) {
    return (
      <span aria-hidden="true" className="inline-flex items-center gap-0.5">
        {Array.from({ length: limit }, (_, i) => (
          <span
            key={i}
            className={cx(
              "h-3 w-1.5 rounded-[2px]",
              i < used ? fill : "bg-edge",
              // The last cell is the wall: a hair taller so "full" reads at a glance.
              i === limit - 1 && "h-4",
            )}
          />
        ))}
      </span>
    );
  }
  const pct = limit > 0 ? Math.min(100, (used / limit) * 100) : 0;
  return (
    <span aria-hidden="true" className="inline-flex h-2 w-24 overflow-hidden rounded bg-edge">
      <span className={fill} style={{ width: `${pct}%` }} />
    </span>
  );
}

function Count({
  value,
  singular,
  plural,
  muted = false,
}: {
  value: number;
  singular: string;
  plural: string;
  muted?: boolean;
}) {
  return (
    <span className={cx("whitespace-nowrap", muted ? "text-faint" : "text-muted")}>
      <span className="font-semibold tabular-nums text-fg">{value}</span> {value === 1 ? singular : plural}
    </span>
  );
}
