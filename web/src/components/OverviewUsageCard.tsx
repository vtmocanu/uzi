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
import { Card, SectionTitle } from "./ui";
import type { UsageWindow } from "./UsageCards";
import {
  failRate, failRateDelta, formatSince, summaryCost, perPrText, failureAgo,
  FailedRunsBlock, TopCauses, SinceLastFailedRun, SegmentedToggle, WindowToggle, Th, Td,
} from "./usageParts";

// PRD #3881: the Overview preview's one Usage card. A You / All users scope toggle (admins
// only) and the window toggle switch one set of figures, instead of the current Overview's
// side-by-side columns; the All users scope adds a By user table below. The admin's scope
// choice is remembered per browser (#4052).

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

function CostCell({ cost, disclosure, total }: { cost: number; disclosure: ReturnType<typeof aggregateDisclosure>; total?: boolean }) {
  return (
    <Td variant="preview" total={total}>
      <span className="font-mono">{summaryCost(cost)}</span>
      {disclosure.incomplete && <span className="ml-auto block max-w-[16ch] whitespace-normal lg:max-w-none lg:whitespace-nowrap text-[11px] font-normal text-faint" title={disclosure.title}>{disclosure.text}</span>}
    </Td>
  );
}

const HARNESS_TITLE =
  "Share of each user's runs by harness, the same runs as the Runs column; cross-check runs count under the harness they ran on";

// Issue #3876: a user's Claude / Codex share as a two-segment bar plus its figures. A window
// with no runs reads "—".
function HarnessCell({ runs }: { runs: HarnessRuns }) {
  const share = harnessShare(runs);
  if (!share) return <Td variant="preview">—</Td>;
  return (
    <Td variant="preview" title={`${runs.claude.toLocaleString("en-US")} Claude, ${runs.codex.toLocaleString("en-US")} Codex runs`}>
      <span className="inline-flex items-center gap-2">
        <span aria-hidden="true" className="flex h-1.5 w-[72px] overflow-hidden rounded-[3px] bg-raised">
          <span className="block h-full bg-brand-claude" style={{ width: `${share.claude}%` }} />
          <span className="block h-full bg-muted" style={{ width: `${share.codex}%` }} />
        </span>
        <span className="min-w-12 text-right text-[11px] font-normal tabular-nums text-faint">{share.claude} / {share.codex}</span>
      </span>
    </Td>
  );
}

// PRD #3881 M2: the Factory scope's per-user table. The columns are fixed on every API version,
// except Claude / Codex (issue #3876), which appears only when the API sends every row's
// harness split for the window; a PR count the API does not report reads "—", never 0.
// The section takes focus (tabIndex -1) when the People chip's "Usage by user" lands here.
function ByUserTable({ admin, window, viewerId, ref }: { admin: AdminUsage; window: UsageWindow; viewerId: string; ref: Ref<HTMLElement> }) {
  const now = useNow(30_000);
  const demo = useDemoMode();
  const { available, rows, showHarness, factoryRuns, factoryPRs, factoryOutcomes, factory, factoryDisclosure } = perUserRows(admin, window);
  const recent = window === "last_7_days";
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
        <div data-testid="by-user-scroll" role="region" aria-label="By user, scrollable" className="mt-2 overflow-x-auto">
          {/* The Claude / Codex column (#3876) widens the floor to the mock's 780px; without it the table keeps 640px. */}
          <table aria-label="By user" className={`w-full ${showHarness ? "min-w-[780px]" : "min-w-[640px]"} border-collapse text-[13px]`}>
            <thead>
              <tr>
                <Th variant="preview" left>User</Th>
                <Th variant="preview" title="Runs with recorded usage in the period">Runs</Th>
                <Th variant="preview">PRs</Th>
                <Th variant="preview">Failed</Th>
                <Th variant="preview" title="Failed of finished runs in the period">Fail rate</Th>
                <Th variant="preview" title={LAST_FAILURE_TITLE}>Last failure</Th>
                <Th variant="preview">Cost</Th>
                {showHarness && <Th variant="preview" title={HARNESS_TITLE}>Claude / Codex</Th>}
              </tr>
            </thead>
            <tbody>
              {rows.map((u) => (
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
                  <CostCell cost={u.b.cost} disclosure={aggregateDisclosure(u.subscription, u.unreported)} />
                  {showHarness && <HarnessCell runs={u.selectedHarness!} />}
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
                <CostCell cost={factory.cost} disclosure={factoryDisclosure} total />
                {showHarness && <Td variant="preview" total />}
              </tr>
            </tfoot>
          </table>
        </div>
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
        {usage.run_count === 0 && <p className="mt-3 text-xs text-muted">No token usage recorded yet.</p>}
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
