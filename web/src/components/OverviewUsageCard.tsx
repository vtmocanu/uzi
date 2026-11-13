import { useEffect, useRef, useState, type ReactNode, type Ref } from "react";
import type { SelfUsage, AdminUsage, HarnessRuns, RunOutcomes } from "../lib/api";
import { formatTokens } from "../lib/formatTokens";
import { aggregateDisclosure } from "../lib/costStatus";
import { useDemoMode } from "../lib/demoMode";
import { maskEmail } from "../lib/demoMask";
import { overviewScope, USAGE_SCOPE_KEY } from "../lib/prefs";
import { stripUnsafeChars } from "../lib/safeText";
import { useNow } from "../lib/useNow";
import { breakdown, harnessShare, perUserRows } from "../lib/usageByUser";
import { costShareRows } from "../lib/costShares";
import { Card, SectionTitle } from "./ui";
import type { UsageWindow } from "./UsageCards";
import {
  failRate, failRateDelta, formatSince, summaryCost, perPrText, failureAgo,
  FailedRunsBlock, TopCauses, SinceLastFailedRun, SegmentedToggle, WindowToggle, Th, Td, hasRecordedUsage, USAGE_SCOPE_NOTE,
} from "./usageParts";

// PRD #3881: the Overview preview's one Usage card. A You / All users scope toggle (admins
// only) and the window toggle switch one set of figures, instead of the current Overview's
// side-by-side columns; the All users scope adds a By user table below. The admin's scope
// choice is remembered per browser (#4052). The By user table ends in one combined bar per row:
// its length is the user's share of the factory's cost, its colours their Claude / Codex runs (#4227).

type Scope = "you" | "all";

const SCOPE_OPTIONS = [
  { value: "you", label: "You" },
  { value: "all", label: "All users" },
] as const;

function Kpi({ label, value, valueClass = "", sub }: { label: string; value: ReactNode; valueClass?: string; sub?: ReactNode }) {
  return (
    <div className="min-w-0">
      <h4 className="text-xs text-muted">{label}</h4>
      <p className={`mt-0.5 whitespace-nowrap font-mono text-[28px] font-medium leading-[1.2] tabular-nums ${valueClass}`}>{value}</p>
      {sub != null && sub !== "" && <div className="mt-0.5 text-xs text-faint">{sub}</div>}
    </div>
  );
}

const LAST_FAILURE_TITLE =
  "All time: the API serves the last failure time for all time only, so this column is the same in both windows";

function FailRateCell({ outcomes, total }: { outcomes: RunOutcomes; total?: boolean }) {
  return (
    <Td variant="preview" total={total} title={`${outcomes.failed} of ${outcomes.finished} finished runs`}>
      {failRate(outcomes)}
      <span className="block text-[11px] font-normal text-faint">of {outcomes.finished.toLocaleString("en-US")} finished</span>
    </Td>
  );
}

// Compact cost notes (issue #4227): the long "+ N runs without a cost estimate and M unreported
// runs" sentence becomes one small chip per non-zero count under the cost. A chip is focusable so
// the full sentence is reachable by keyboard as well as hover; its accessible name is that sentence.
function CostChip({ count, short, full, detail }: { count: number; short: string; full: (n: string) => string; detail: string }) {
  const n = count.toLocaleString("en-US");
  const sentence = full(n);
  return (
    <span
      role="note"
      tabIndex={0}
      aria-label={sentence}
      title={`${sentence}: ${detail}`}
      className="cursor-help whitespace-nowrap rounded-full border border-edge bg-raised px-[7px] text-[11px] font-normal leading-[17px] text-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
    >
      +{n} {short}
    </span>
  );
}

function CostCell({ cost, subscription, unreported, total }: { cost: number; subscription: number; unreported: number; total?: boolean }) {
  return (
    <Td variant="preview" total={total}>
      <span className="font-mono">{summaryCost(cost)}</span>
      {(subscription > 0 || unreported > 0) && (
        <span className="mt-1 flex flex-wrap justify-end gap-1">
          {subscription > 0 && (
            <CostChip
              count={subscription}
              short="no est."
              full={(n) => `${n} ${subscription === 1 ? "run" : "runs"} without a cost estimate`}
              detail="tokens only, no dollar figure"
            />
          )}
          {unreported > 0 && (
            <CostChip
              count={unreported}
              short="unrep."
              full={(n) => `${n} unreported ${unreported === 1 ? "run" : "runs"}`}
              detail="cost incomplete; recorded partial costs included"
            />
          )}
        </span>
      )}
    </Td>
  );
}

const SHARE_TITLE = "Share of recorded API-equivalent cost in the selected period";
const HARNESS_NOTE =
  "share of each user's runs by harness, the same primary work runs as the Runs column";

// Issue #4227: one combined bar per row. Its length is the row's share of the factory's cost
// (ratio, unrounded); when the API sends the harness split, that length is divided into a Claude
// and a Codex segment by the row's runs, and the "C / X" figure beside it names the split. A row
// with no runs (or no split at all) draws one cost-only segment. Zero recorded factory cost has
// no length to draw: the percentage reads "—" and the track stays empty.
function ShareCell({ pct, ratio, runs, withHarness, total }: {
  pct: number | undefined;
  ratio: number | undefined;
  runs: HarnessRuns | undefined;
  withHarness: boolean;
  total?: boolean;
}) {
  const split = withHarness ? harnessShare(runs) : null;
  const width = ratio == null ? 0 : Math.min(Math.max(ratio, 0), 1) * 100;
  let title = pct == null ? "No recorded factory cost in the selected period" : `${pct}% of factory cost`;
  if (split) title += `; runs ${split.claude}% Claude, ${split.codex}% Codex`;
  return (
    <Td start variant="preview" total={total} title={title}>
      <span className="flex items-center gap-2">
        <span className="w-9 shrink-0 text-right">{pct == null ? "—" : `${pct}%`}</span>
        <span aria-hidden="true" className="flex h-1.5 w-[120px] shrink-0 overflow-hidden rounded-[3px] bg-raised">
          {ratio != null && (split ? (
            <>
              <span className="block h-full bg-brand-claude" style={{ width: `${(width * split.claude) / 100}%` }} />
              <span className="block h-full bg-muted" style={{ width: `${(width * split.codex) / 100}%` }} />
            </>
          ) : (
            <span className="block h-full bg-brand" style={{ width: `${width}%` }} />
          ))}
        </span>
        {withHarness && (
          <span className="w-14 shrink-0 whitespace-nowrap text-right text-[11px] font-normal tabular-nums text-faint">
            {split ? `${split.claude} / ${split.codex}` : ""}
          </span>
        )}
      </span>
    </Td>
  );
}

// PRD #3881 M2: the Factory scope's per-user table. The columns are fixed on every API version,
// except that the last one's Claude / Codex half (issue #3876) appears only when the API sends
// every row's harness split for the window; a PR count the API does not report reads "—", never 0.
// The last column (issue #4227) is always present: the cost share with its combined bar. Under the
// table a one-line legend explains the bar. The section takes focus (tabIndex -1) when the
// People chip's "Usage by user" lands here.
function ByUserTable({ admin, window, viewerId, ref }: { admin: AdminUsage; window: UsageWindow; viewerId: string; ref: Ref<HTMLElement> }) {
  const now = useNow(30_000);
  const demo = useDemoMode();
  const {
    available, rows, showHarness, factoryRuns, factoryPRs, factoryOutcomes, factory,
    factorySubscription, factoryUnreported,
  } = perUserRows(admin, window);
  const recent = window === "last_7_days";
  const shares = costShareRows(rows.map((r) => r.b.cost), factory.cost);
  const factoryHarness: HarnessRuns | undefined = showHarness
    ? {
        claude: rows.reduce((sum, r) => sum + r.selectedHarness!.claude, 0),
        codex: rows.reduce((sum, r) => sum + r.selectedHarness!.codex, 0),
      }
    : undefined;
  return (
    <section ref={ref} id="overview-usage-by-user" tabIndex={-1} aria-label="By user" className="mt-5 border-t border-edge pt-4 scroll-mt-20 rounded-md outline-hidden focus:ring-2 focus:ring-ring">
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
        <h3 className="text-sm font-semibold text-fg">By user</h3>
        <span className="text-xs text-faint">{recent ? "last 7 days" : "all time"}, by cost</span>
      </div>
      {/* The scroller follows RunUsage.tsx: a named region without an unconditional tabIndex, since
          Chrome already focuses an overflowing scroller and a permanent stop would sit empty at desktop widths. */}
      {recent && !available ? (
        <p className="mt-2 text-sm text-muted">Seven-day per-user usage is unavailable. Upgrade the API to view this period.</p>
      ) : rows.length === 0 ? (
        <p className="mt-2 text-sm text-faint">No usage across the factory yet.</p>
      ) : (
        <>
          <div data-testid="by-user-scroll" role="region" aria-label="By user, scrollable" className="mt-2 overflow-x-auto">
            {/* One floor, sized so the whole table fits the 935px-wide card body at a 1280px
                viewport; narrower widths scroll inside this region. */}
            <table aria-label="By user" className="w-full min-w-[860px] border-collapse text-[13px]">
              <thead>
                <tr>
                  <Th variant="preview" left>User</Th>
                  <Th variant="preview" title="Primary work runs with recorded usage in the period">Runs</Th>
                  <Th variant="preview">PRs</Th>
                  <Th variant="preview">Failed</Th>
                  <Th variant="preview" title="Failed of finished runs in the period">Fail rate</Th>
                  <Th variant="preview" title={LAST_FAILURE_TITLE}>Last failure</Th>
                  <Th variant="preview">Cost</Th>
                  <Th variant="preview" start title={showHarness ? `${SHARE_TITLE}; Claude / Codex: ${HARNESS_NOTE}` : SHARE_TITLE}>
                    {showHarness ? "Cost share · Claude / Codex" : "Cost share"}
                  </Th>
                </tr>
              </thead>
              <tbody>
                {rows.map((u, i) => (
                  <tr key={u.user_id}>
                    <Td variant="preview" left>
                      {stripUnsafeChars(maskEmail(u.email, demo))}
                      {u.user_id === viewerId && <span className="font-normal text-faint"> (you)</span>}
                    </Td>
                    <Td variant="preview">{u.selectedRuns.toLocaleString("en-US")}</Td>
                    <Td variant="preview">{u.selectedPRs == null ? "—" : u.selectedPRs.toLocaleString("en-US")}</Td>
                    <Td variant="preview">{u.selectedOutcomes.failed.toLocaleString("en-US")}</Td>
                    <FailRateCell outcomes={u.selectedOutcomes} />
                    <Td variant="preview">{failureAgo(u.outcomes, now)}</Td>
                    <CostCell cost={u.b.cost} subscription={u.subscription} unreported={u.unreported} />
                    <ShareCell
                      pct={shares?.[i]}
                      ratio={shares ? u.b.cost / factory.cost : undefined}
                      runs={u.selectedHarness}
                      withHarness={showHarness}
                    />
                  </tr>
                ))}
              </tbody>
              <tfoot>
                <tr>
                  <Td variant="preview" left total>Factory</Td>
                  <Td variant="preview" total>{factoryRuns.toLocaleString("en-US")}</Td>
                  <Td variant="preview" total>{factoryPRs == null ? "—" : factoryPRs.toLocaleString("en-US")}</Td>
                  <Td variant="preview" total>{factoryOutcomes.failed.toLocaleString("en-US")}</Td>
                  <FailRateCell outcomes={factoryOutcomes} total />
                  <Td variant="preview" total>{failureAgo(admin.factory.outcomes.lifetime, now)}</Td>
                  <CostCell cost={factory.cost} subscription={factorySubscription ?? 0} unreported={factoryUnreported ?? 0} total />
                  <ShareCell pct={shares ? 100 : undefined} ratio={shares ? 1 : undefined} runs={factoryHarness} withHarness={showHarness} total />
                </tr>
              </tfoot>
            </table>
          </div>
          {shares && (
            <p className="mt-2 flex flex-wrap items-center gap-x-3.5 gap-y-1 text-xs text-muted">
              <span>Bar length: cost share of the factory</span>
              {showHarness && (
                <>
                  <span className="inline-flex items-center gap-1.5"><span aria-hidden="true" className="inline-block h-1.5 w-2.5 rounded-sm bg-brand-claude" />Claude runs</span>
                  <span className="inline-flex items-center gap-1.5"><span aria-hidden="true" className="inline-block h-1.5 w-2.5 rounded-sm bg-muted" />Codex runs</span>
                </>
              )}
            </p>
          )}
        </>
      )}
    </section>
  );
}

export function OverviewUsageCard({ self, admin, window, onWindowChange, viewerId, scope: controlledScope, onScopeChange, focusByUser }: {
  self: SelfUsage;
  admin?: AdminUsage;
  window: UsageWindow;
  onWindowChange: (w: UsageWindow) => void;
  viewerId: string;
  // Optional controlled scope (the People chip's "Usage by user" switches it from outside). The
  // parent then owns remembering the choice; without them the card keeps its own, as before.
  scope?: Scope;
  onScopeChange?: (s: Scope) => void;
  // A counter the parent bumps to move focus to the By user section once it is shown.
  focusByUser?: number;
}) {
  const demo = useDemoMode();
  const [chosen, setChosen] = useState<Scope>(() => overviewScope.get(USAGE_SCOPE_KEY));
  const setScope = (s: Scope) => {
    if (controlledScope !== undefined) {
      onScopeChange?.(s);
      return;
    }
    setChosen(s);
    overviewScope.set(USAGE_SCOPE_KEY, s);
  };
  const scope: Scope = admin ? (controlledScope ?? chosen) : "you";
  const byUserShown = scope === "all" && admin != null;

  // Each new nonzero focusByUser value is handled once: scroll the By user section to the top
  // and focus it, as soon as it is mounted (the scope switch and the bump can land together).
  const byUserRef = useRef<HTMLElement>(null);
  const handledFocus = useRef(0);
  useEffect(() => {
    if (!focusByUser || focusByUser === handledFocus.current) return;
    const el = byUserRef.current;
    if (!byUserShown || !el) return;
    handledFocus.current = focusByUser;
    if (typeof el.scrollIntoView === "function") el.scrollIntoView({ block: "start" });
    el.focus({ preventScroll: true });
  }, [focusByUser, byUserShown]);
  const personal = scope === "you";
  const usage = scope === "all" && admin ? admin.factory : self;

  const selected = breakdown(usage[window]);
  const outcomes = usage.outcomes[window];
  const lifetime = usage.outcomes.lifetime;
  const recent = window === "last_7_days";
  const disclosure = aggregateDisclosure(
    recent ? usage.last7_subscription_run_count : usage.lifetime_subscription_run_count,
    recent ? usage.last7_unreported_run_count : usage.lifetime_unreported_run_count,
  );
  const prCount = recent ? usage.last7_pr_count : usage.lifetime_pr_count;

  const failureOwner = admin?.users.find((u) => u.user_id === admin.factory.outcomes.lifetime.last_failed_user_id);
  const owner = !personal && failureOwner ? maskEmail(failureOwner.email, demo) : undefined;

  let failSub: ReactNode = null;
  if (recent) failSub = failRateDelta(outcomes, lifetime);
  else if (!personal && admin) {
    const since = formatSince(admin.earliest_run);
    if (since) failSub = `across every user since ${since}`;
  }

  return (
    <Card className="min-w-0">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <SectionTitle>Usage</SectionTitle>
        <div className="flex flex-wrap items-center gap-2">
          {/* steadyWidth: the same toggle look as Issue work's, and no shift on a pick (#4052). */}
          {admin && <SegmentedToggle label="Usage scope" options={SCOPE_OPTIONS} value={scope} onChange={setScope} className="" steadyWidth />}
          <WindowToggle value={window} onChange={onWindowChange} className="" steadyWidth />
        </div>
      </div>
      <section aria-label={personal ? "Your usage" : "Factory usage"} className="min-w-0">
        <div className="mt-4 grid grid-cols-2 gap-x-6 gap-y-4 md:grid-cols-4">
          <Kpi
            label="Cost (API-equivalent)"
            valueClass="text-brand"
            value={summaryCost(selected.cost)}
            sub={disclosure.incomplete ? <span title={disclosure.title}>{disclosure.text}</span> : "estimated at API list prices"}
          />
          <Kpi
            label="Pull requests opened"
            value={prCount == null ? "—" : prCount.toLocaleString("en-US")}
            sub={prCount == null ? null : perPrText(prCount, selected.cost, disclosure)}
          />
          <Kpi label="Failed runs rate" value={failRate(outcomes)} sub={failSub} />
          <Kpi
            label="Tokens"
            value={formatTokens(selected.total)}
            sub={`in ${formatTokens(selected.fresh)}, cached ${formatTokens(selected.cached)}, out ${formatTokens(selected.out)}`}
          />
        </div>
        <p className="mt-3 text-[11px] leading-relaxed text-muted">{USAGE_SCOPE_NOTE}</p>
        {!hasRecordedUsage(usage) && <p className="mt-3 text-xs text-muted">No token usage recorded yet.</p>}
        <div className="mt-5 grid gap-6 border-t border-edge pt-4 md:grid-cols-[minmax(0,1.3fr)_minmax(0,1fr)]">
          <div className="min-w-0 space-y-3">
            <FailedRunsBlock outcomes={outcomes} personal={personal} framed={false} />
            {outcomes.needs_landing > 0 && (
              <p className="text-[11px] text-muted">{outcomes.needs_landing.toLocaleString("en-US")} failed runs with recoverable work. May include work already landed.</p>
            )}
          </div>
          <div className="min-w-0 space-y-3">
            <TopCauses outcomes={outcomes} personal={personal} variant="list" />
            <SinceLastFailedRun outcomes={lifetime} owner={owner} label="Latest failure (all time)" wrap />
          </div>
        </div>
      </section>
      {scope === "all" && admin && <ByUserTable ref={byUserRef} admin={admin} window={window} viewerId={viewerId} />}
    </Card>
  );
}
