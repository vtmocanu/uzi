// Runs index — two routed tabs under the ONE sidebar "Runs" destination
// (ux-tweaks amendment 3): /runs is the live console (your Active runs, plus the
// admin Factory card showing OTHER users' runs — amendment 2), /runs/history is
// the archive (search + date grouping + progressive reveal). The strip is the
// SettingsShell/AdminShell NavLink pattern, so deep links and back/forward work
// natively — React Router ranks the static /runs/history above /runs/:id by
// construction, and run ids are run-*/UUIDs besides.
//
// Archive mechanics: grouping grain is days this week → weeks this month →
// months beyond (lib/runGroups.ts, pure + clock-explicit); the render slice
// reuses the board's own lanePaging (PRD #304: cap 10, page 50, an active search
// lifts the baseline — display, never membership). The sort keeps multica's rule
// that failed outranks cancelled outranks completed at equal timestamps
// (PAST_STATUS_RANK). The row status pill keeps PRD #12's "a deliberate stop is
// not a failure" nuance: isStoppedRun collapses cancelled / stop_kind-stamped-
// failed runs (PRD #33) to a calm "stopped" pill, not "failed".

import { useCallback, useEffect, useState } from "react";
import { Link, NavLink, Outlet, useOutletContext, useSearchParams } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, isTerminalRun, type AdminWorker, type RunListItem, type RunUsage } from "../lib/api";
import { Alert, Badge, Card, EmptyState, Input, ListSkeleton, PageHeader, SectionTitle, StatusPill, cx } from "../components/ui";
import { ActivityIcon } from "../components/icons";
import { MrChip } from "../components/MrChip";
import { RunIssueRef } from "../components/RunIssueRef";
import { mrAbbrev } from "../lib/forgeNoun";
import { effectiveRunStatus, isStoppedRun, milestoneBadge, milestoneBadgeText, mrChipState } from "../lib/runBadge";
import { RunPriorityBadge } from "../components/RunPriorityBadge";
import { formatTokens, formatCost } from "../lib/formatTokens";
import { runDurationLabel } from "../lib/runDuration";
import { useNow } from "../lib/rateLimits";
import { hasTemplateDrift } from "../lib/workerTemplates";
import { WorkerRunBadge } from "../components/WorkerRunBadge";
import { RunHealthBadge } from "../components/RunHealthBadge";
import { JudgeRunBadge } from "../components/JudgeRunBadge";
import { RunCredential } from "../components/RunCredential";
import { stripUnsafeChars } from "../lib/safeText";
import { formatUptimeSince } from "../lib/formatUptimeSince";
import { anthropicTokenCount } from "../lib/hasToken";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";
import { lanePaging } from "../lib/boardColumns";
import { groupRuns, runMatchesQuery } from "../lib/runGroups";

const PAST_STATUS_RANK: Record<string, number> = { failed: 0, cancelled: 1, completed: 2 };

// The archive's render slice, borrowing the board's constants (PRD #304):
// PAST_CAP rows up front, one PAGE per "Show 50 more", and an active search lifts
// the baseline to a full page (lanePaging owns that rule).
const PAST_CAP = 10;
const PAGE = 50;
// The search input's stable id, so `/` can focus it (the board's M5 shortcut).
const SEARCH_INPUT_ID = "runs-search";

// When a past run HAPPENED, for both sorting and date grouping: finished_at is the
// honest anchor, updated_at the pre-feature fallback — one function so the sort and
// the group headers can never disagree about where a run belongs.
function pastAnchor(r: RunListItem): string {
  return r.finished_at ?? r.updated_at;
}

// pastInstant parses the anchor to an epoch-ms instant. Date.parse and NOT a string
// compare: Go trims trailing zeros from the fractional seconds, so same-second stamps
// of differing precision ("…:00Z" vs "…:00.5Z") order correctly as instants and
// INCORRECTLY as strings (see lib/boardOrder.ts's timeKey).
function pastInstant(r: RunListItem): number {
  const t = Date.parse(pastAnchor(r));
  return Number.isNaN(t) ? -Infinity : t;
}

export function sortPast(a: RunListItem, b: RunListItem): number {
  const t = pastInstant(b) - pastInstant(a);
  if (t !== 0) return t;
  return (PAST_STATUS_RANK[a.status] ?? 3) - (PAST_STATUS_RANK[b.status] ?? 3);
}

// The meta line's "tok" figure is the run's ALL-token total (fresh + cached + cache
// creation + output), matching the mock's single "1.33M tok".
function runUsageTotalTokens(u: RunUsage): number {
  return u.input_tokens + u.cache_read_tokens + u.cache_creation_tokens + u.output_tokens;
}

// The data both tabs read, owned by the persistent layout below and delivered
// through the router's Outlet context — the one-fetch seam the tab-count nit
// exposed: when each tab page fetched for itself, every switch remounted the
// page, refetched, and blanked the counted tab label until the load returned.
interface RunsData {
  runs: RunListItem[];
  loading: boolean;
  error: string;
  tokenCount: number;
  // PRD #320 M6: re-run the layout's one runs fetch, so an Expedite/undo on a queued
  // row can refresh the priority pill + queue ordering the way RunView's refreshRun
  // does — on top of the 10s visibility-gated background poll (PRD #518) that keeps
  // the list live without a manual reload.
  reload: () => void;
}

function useRunsData(): RunsData {
  return useOutletContext<RunsData>();
}

// One constant header + tab strip across both tabs — the SettingsShell treatment,
// so nothing jumps on a switch. This is a LAYOUT ROUTE (App.tsx nests /runs and
// /runs/history under it), so it survives tab switches: the runs fetch lives here,
// both tabs read it via Outlet context, and the archive tab's count neither
// refetches nor resets on a switch. While the first load is in flight the tab
// shows the bare label rather than a flashing 0 (past-count null); after that the
// count exists for the layout's whole life.
export function RunsLayout() {
  const [runs, setRuns] = useState<RunListItem[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  // PRD #295: the ">1 Anthropic token" gate for the personal credential badge,
  // computed once from the viewer's secrets. A single-token user sees no badge.
  const [tokenCount, setTokenCount] = useState(0);

  // The one runs fetch, extracted so an Expedite/undo can re-run it (reload below). The
  // isAlive guard defaults to always-true for a reload (the component is mounted when the
  // user clicks) and is a mount-scoped flag for the initial load, so an in-flight first
  // fetch never setStates after unmount.
  const load = useCallback(async (isAlive: () => boolean = () => true) => {
    try {
      const [{ runs }, { secrets }] = await Promise.all([
        api.listRuns(),
        // Best-effort (like Dashboard's usage calls): the secrets fetch only powers
        // the cosmetic ">1 token" credential-badge gate, so a secrets-endpoint
        // failure must not blank the whole Runs area. Fall back to no badge.
        api.listSecrets().catch(() => ({ secrets: [] })),
      ]);
      if (!isAlive()) return;
      setRuns(runs);
      setTokenCount(anthropicTokenCount(secrets));
    } catch (err) {
      if (isAlive()) setError(err instanceof ApiError ? err.message : "Failed to load runs");
    } finally {
      if (isAlive()) setLoading(false);
    }
  }, []);

  useEffect(() => {
    let alive = true;
    void load(() => alive);
    return () => {
      alive = false;
    };
  }, [load]);

  // Liveness (PRD #518): re-fetch only the volatile runs list every 10s while the
  // tab is visible (catching up immediately on focus), so runs moving queued →
  // running → completed/failed, and new runs appearing, show without a manual
  // browser refresh — matching Board and Dashboard. A transient poll failure keeps
  // the last-good rows: unlike the first load, this swallows its error and never
  // routes through setError/setLoading, so a blip cannot re-flash the skeleton or
  // pop an error banner. The credential-badge secrets/tokenCount stay mount-only
  // (they change rarely), so the poll re-fetches api.listRuns() alone.
  const poll = useCallback(async () => {
    try {
      const { runs } = await api.listRuns();
      setRuns(runs);
    } catch {
      // keep the last-good list
    }
  }, []);
  usePollWhileVisible(poll, 10000);

  const pastCount = loading ? null : runs.filter((r) => isTerminalRun(r.status)).length;
  const tabs = [
    { to: "/runs", label: "Active", end: true },
    { to: "/runs/history", label: pastCount == null ? "Past runs" : `Past runs · ${pastCount}`, end: false },
  ];
  return (
    <div className="space-y-6">
      <PageHeader title="Runs" description="Your agent runs. Open one to watch it live." />
      <div className="flex gap-1 overflow-x-auto border-b border-edge">
        {tabs.map((t) => (
          <NavLink
            key={t.to}
            to={t.to}
            end={t.end}
            className={({ isActive }) =>
              cx(
                "-mb-px shrink-0 whitespace-nowrap border-b-2 px-3 py-2 text-sm font-medium transition-colors",
                isActive
                  ? "border-brand text-fg"
                  : "border-transparent text-muted hover:border-edge-strong hover:text-fg",
              )
            }
          >
            {t.label}
          </NavLink>
        ))}
      </div>
      <Outlet context={{ runs, loading, error, tokenCount, reload: () => void load() } satisfies RunsData} />
    </div>
  );
}

function RunRow({
  run,
  now,
  showOwner,
  waitingForVault = false,
  showCredential = false,
  owned = false,
  onExpedited,
}: {
  run: RunListItem;
  // now (issue #256 M3): a Date.now()-style clock, ticked by useNow in the parent, so
  // the live duration token re-derives without a per-row timer.
  now: number;
  showOwner?: boolean;
  // waitingForVault (PRD #32): this is the current user's own queued run and their
  // vault is locked, so it will not claim until they unlock — surfaced as a distinct
  // amber state instead of a bare "queued" pill.
  waitingForVault?: boolean;
  // showCredential (PRD #295): render the compact credential badge in the pill
  // cluster. Gated off by default; the caller decides (personal list: viewer holds
  // >1 token; admin factory list: always). RunCredential itself self-hides on a run
  // with no label, so a mixed list needs no per-row guard beyond this flag. The
  // compact badge is inherently non-linked (it lives inside this row's own <Link>),
  // so there is no linkable flag to thread through.
  showCredential?: boolean;
  // owned (PRD #320 M6): the viewer owns this run, so the Expedite/undo action may show.
  // RunListItem carries no per-row is_mine, so ownership is threaded from the caller: the
  // personal Active list is all the viewer's own runs (owned), the admin factory list is
  // OTHER users' runs (not owned). The queued gate + the server's owner-scoped 404 are
  // the backstop, but this keeps a non-owner from ever seeing a control that 404s.
  owned?: boolean;
  // onExpedited (PRD #320 M6): re-run the list fetch after a successful Expedite/undo so
  // the priority pill + queue ordering refresh. Wired to the layout's reload.
  onExpedited?: () => void;
}) {
  // A deliberate human stop (cancelled, or failed carrying a server-stamped
  // stop_kind — PRD #33) reads "stopped" / neutral, never "failed" / danger. Fold
  // that into the pill's status so the shared StatusPill palette renders it calm.
  const pillStatus = isStoppedRun(run.status, run.stop_kind) ? "stopped" : effectiveRunStatus(run);
  // MR chip state (PRD #33): open renders exactly as before; merged/closed get a
  // label and closed is muted + struck. This is a per-run frozen hint.
  const mrState = mrChipState(run.mr_state);
  // PRD #122: compact milestone progress for the row; null on a non-milestone run,
  // which then renders no new badge (the row had none before this feature).
  const ms = milestoneBadge(run);
  const msBadge = ms ? milestoneBadgeText(ms) : null;
  // Issue #256 M3: a live, per-state duration token ("running 1h 30m", "ran 42m", …);
  // "" for a pre-feature/no-anchor run, which then adds nothing to the meta line.
  const duration = runDurationLabel(run, now);
  // PRD #320 M6: the owner's Expedite/undo action on a queued row. Owner-scoped +
  // queued-only, matching the server; a background/restored/normal row offers "Expedite",
  // an already-expedited row offers "Undo". The action lives INSIDE the row's <Link>, so
  // the click handler must preventDefault + stopPropagation or it navigates instead of
  // firing the mutation (the row-Link trap the credential badge also had to dodge).
  const [expediting, setExpediting] = useState(false);
  const canExpedite = owned && run.status === "queued" && !waitingForVault;
  const isExpedited = run.priority === "expedited";
  const doExpedite = async () => {
    if (expediting) return;
    setExpediting(true);
    try {
      await api.expediteRun(run.id, !isExpedited);
      onExpedited?.();
    } catch {
      // Best-effort: a 409 (raced out of queued) or 404 (not the owner) backstops a
      // stale gate; the row simply stays as it was and the next load reconciles it.
    } finally {
      setExpediting(false);
    }
  };
  return (
    // Issue #485 NB1: RunIssueRef and the Expedite control are real interactive elements,
    // so they can no longer nest inside the card's navigational <Link> (an <a>; a nested
    // <a> is invalid HTML). The <li> is a `group relative` container, the run-details
    // <Link> is a stretched absolute overlay, and the card content sits in normal flow
    // with each genuinely-interactive descendant raised above the overlay (relative
    // z-10). The card's hover border/bg live on the content layer via group-hover so the
    // transparent overlay never paints over the content.
    <li className="group relative">
      <Link
        to={`/runs/${run.id}`}
        // Issue #485 review FIX 2: name the link by the run's visible title so multiple
        // issue-less runs (task/ci_fix/chat/self-improve, all "Open run" otherwise) get
        // distinct, meaningful accessible names — the title now sits under the overlay
        // and no longer contributes to the link name on its own. issue_title is untrusted
        // forge text, so it stays sanitized (same stripUnsafeChars as the visible title).
        aria-label={`Open run${run.issue_iid != null ? ` for issue #${run.issue_iid}` : ""}${
          run.issue_title.trim() ? `: ${stripUnsafeChars(run.issue_title)}` : ""
        }`}
        className="absolute inset-0 rounded-lg"
      />
      <div className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-edge bg-raised/40 px-3 py-2.5 transition-colors group-hover:border-edge-strong group-hover:bg-raised/70">
        {/* w-full below sm stacks the badge cluster UNDER the title (review-wave
            fix 1): with min-w-0 alone the title column could shrink to nothing, so
            at 390px the unshrinkable badges starved it to a few characters before
            flex-wrap ever fired. From sm up the old one-row layout is unchanged. */}
        <div className="w-full min-w-0 sm:w-auto sm:flex-1">
          {/* Issue #124: the run title is the forge ISSUE title — writable by anyone who
              can open an issue on the target repo, so it is untrusted free text on the same
              footing as judge output. Display-only here; the raw value stays the identity. */}
          <p className="truncate text-sm font-medium text-fg">{stripUnsafeChars(run.issue_title)}</p>
          {/* PRD #362 M4: a one-line intent preview ("what this run will implement"),
              shown once the summary lands. UNTRUSTED model output over an
              attacker-influenceable issue/PRD, so it goes through stripUnsafeChars and is
              truncated to one line like the title — never <Markdown>. Absent until the
              worker posts the intent summary, so a pre-feature/early run shows only the title. */}
          {run.summary_intent && run.summary_intent.trim() !== "" && (
            <p className="mt-0.5 truncate text-xs text-muted">{stripUnsafeChars(run.summary_intent)}</p>
          )}
          <p className="mt-0.5 flex flex-wrap items-center gap-x-2 text-xs text-faint">
            <span className="inline-flex items-center gap-1">
              {run.repo_path}{" "}
              <RunIssueRef
                issueIid={run.issue_iid}
                issueWebUrl={run.issue_web_url}
                kind={run.kind}
                forgeType={run.forge_type}
                raised
              />
            </span>
            {run.worker_name && <span>· {run.worker_name}</span>}
            {showOwner && run.owner_email && <span>· {run.owner_email}</span>}
            <span>· {new Date(run.updated_at).toLocaleString()}</span>
            {duration && <span className="font-mono tabular-nums">· {duration}</span>}
            {run.mr_iid != null && (
              <MrChip
                variant="inline"
                label={`· ${mrAbbrev(run.forge_type)} `}
                forgeType={run.forge_type}
                mrIid={run.mr_iid}
                mrState={mrState}
                href={null}
                // Issue #485 review FIX 1: raised above the card's stretched-link overlay
                // (relative z-10) so its native `title` (MR state) fires on hover; the
                // href-less chip is not click-to-navigate, which is fine for a status chip.
                className="relative z-10 font-medium"
              />
            )}
            {/* PRD #40: tokens + cost join the meta line; hidden for a run with no
                usage rows (a pre-feature run) — never a fabricated 0. A running run
                shows its "so far" figure, which grows as phases fold. */}
            {run.usage && (
              <>
                <span className="font-mono tabular-nums">
                  · {formatTokens(runUsageTotalTokens(run.usage))} tok
                  {run.status === "running" ? " so far" : ""}
                </span>
                {run.usage.cost_usd > 0 && (
                  <span className="font-mono text-brand/90">· {formatCost(run.usage.cost_usd)}</span>
                )}
              </>
            )}
          </p>
        </div>
        {/* Issue #485 review FIX 1: raise the whole right-side badge cluster above the
            card's stretched-link overlay (relative z-10) so every badge's native `title`
            tooltip fires on hover — the transparent overlay otherwise intercepts the
            pointer and its title is empty. These are status badges nobody clicks to open
            a run, so trading their click-to-navigate for working tooltips is correct; the
            title/body text stays under the overlay and still navigates. The Expedite
            button (already relative z-10, stopPropagation) is unaffected by a raised
            ancestor. */}
        <div className="relative z-10 flex flex-wrap items-center gap-2">
          {run.auto_approve && (
            <Badge tone="brand" title="Autopilot: started from the label, plan auto-approved">
              autopilot
            </Badge>
          )}
          {waitingForVault ? (
            <Badge tone="warning" title="This run will claim once you unlock your vault.">
              <span aria-hidden="true">🔒</span> waiting for vault unlock
            </Badge>
          ) : (
            <>
              {/* PRD #122: compact milestone progress; a non-milestone run adds nothing.
                  PRD #265 M2: "not reported" (M–/N) reads distinct from a genuine 0/N. */}
              {msBadge && (
                <Badge tone="info" title={msBadge.title}>
                  {msBadge.label}
                </Badge>
              )}
              {/* The health flag (PRD #47) sits beside the status pill; hidden here
                  when waitingForVault already explains a locked queued run. */}
              <RunHealthBadge run={run} />
              {/* The judge verdict (PRD #98 M4) sits with the other per-run pills;
                  absent entirely on an unjudged run. */}
              <JudgeRunBadge run={run} />
              {/* PRD #320 M6: the queue-priority pill (Deprioritized/Expedited/Restored),
                  self-hiding on a normal or non-queued run. */}
              <RunPriorityBadge priority={run.priority} status={run.status} />
              <StatusPill status={pillStatus} />
              {/* PRD #320 M6: the owner-only Expedite/undo control, queued rows only. It is
                  raised above the card's stretched-link overlay (relative z-10, issue #485
                  NB1) so the click hits the button rather than the run-details Link; it
                  still preventDefaults + stopPropagates defensively before firing the
                  mutation. A non-owner never reaches this branch (owned gate). */}
              {canExpedite && (
                <button
                  type="button"
                  disabled={expediting}
                  onClick={(e) => {
                    e.preventDefault();
                    e.stopPropagation();
                    void doExpedite();
                  }}
                  title={
                    isExpedited
                      ? "Return this run to its natural queue position."
                      : "Bump this run to the front of the queue."
                  }
                  className="relative z-10 rounded-md border border-edge bg-raised px-2 py-1 text-xs font-medium text-muted transition-colors hover:border-edge-strong hover:text-fg disabled:opacity-50"
                >
                  {isExpedited ? "Undo" : "Expedite"}
                </button>
              )}
              {/* PRD #295: which Anthropic token this run's claim billed. Gated by
                  the caller (personal list: viewer holds >1 token) and self-hiding
                  on a run with no recorded label. */}
              {showCredential && <RunCredential run={run} variant="compact" />}
            </>
          )}
        </div>
      </div>
    </li>
  );
}

// The live console: your active runs, and (admin) the factory's other-user runs.
// Runs/secrets come from the layout's one fetch (Outlet context); only the
// admin-scoped lists are fetched here, since the non-admin majority never needs
// them and the layout must stay role-agnostic.
export function RunsList() {
  const { user, vaultUnlocked } = useAuth();
  const isAdmin = !!user?.is_admin;
  // Issue #256 M3: a 1s tick drives the live duration token on every row; the 10s
  // data poll (PRD #518) refreshes the rows but does not re-derive the token between
  // fetches, so it would freeze without this finer clock.
  const now = useNow(1000);

  const { runs, loading, error, tokenCount, reload } = useRunsData();
  const [adminRuns, setAdminRuns] = useState<RunListItem[]>([]);
  const [adminWorkers, setAdminWorkers] = useState<AdminWorker[]>([]);
  const [adminError, setAdminError] = useState("");
  const [adminLoading, setAdminLoading] = useState(true);

  useEffect(() => {
    if (!isAdmin) return;
    let alive = true;
    Promise.all([api.adminListRuns(), api.adminListWorkers()])
      .then(([r, w]) => {
        if (!alive) return;
        setAdminRuns(r.runs);
        setAdminWorkers(w.workers);
      })
      .catch((err) => {
        if (alive) setAdminError(err instanceof ApiError ? err.message : "Failed to load factory status");
      })
      .finally(() => {
        if (alive) setAdminLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [isAdmin]);

  // Liveness (PRD #518): the admin Factory card re-fetches its own volatile lists
  // (runs + workers) on the same 10s visibility-gated cadence, so other users' runs
  // and worker online/offline state update live. The hook is called unconditionally
  // (React rules of hooks); the callback self-gates on isAdmin, mirroring the mount
  // effect's early return, so no admin endpoint is hit for a normal user. A blip is
  // swallowed to keep last-good — it must NOT route through setAdminError/
  // setAdminLoading, or a transient failure would pop the in-card Alert over working
  // data. This is happy-path setState only, separate from the mount effect's
  // first-load semantics.
  const pollAdmin = useCallback(async () => {
    if (!isAdmin) return;
    try {
      const [r, w] = await Promise.all([api.adminListRuns(), api.adminListWorkers()]);
      setAdminRuns(r.runs);
      setAdminWorkers(w.workers);
    } catch {
      // keep the last-good factory data
    }
  }, [isAdmin]);
  usePollWhileVisible(pollAdmin, 10000);

  const active = runs.filter((r) => !isTerminalRun(r.status));
  const past = runs.filter((r) => isTerminalRun(r.status));
  // The factory card shows OTHER users' runs only (amendment 2026-08-14 (2)): the
  // admin's own runs already appear in Active above. owner_email is the admin-list
  // discriminator (RunListItem carries no is_mine); a row without one stays visible.
  const factoryRuns = adminRuns.filter((r) => r.owner_email !== user?.email);

  return (
    <>
      {/* The global judge-recommendation strip moved to the Judge page header (PRD #98
          Decision 7); each run row now carries its own verdict badge (JudgeRunBadge). */}

      {error && <Alert message={error} />}
      {loading && <ListSkeleton rows={4} />}

      {!loading && (
        <>
          {active.length === 0 && past.length === 0 ? (
            <EmptyState
              icon={<ActivityIcon />}
              title="No runs yet"
              description="Open a board and press Start run on a PRD card — the agent plans, waits for your approval, then implements and opens a merge/pull request."
              action={
                <Link to="/repos" className="text-sm font-medium text-brand hover:text-brand-hover">
                  Go to boards →
                </Link>
              }
            />
          ) : (
            <div className="space-y-2">
              <SectionTitle>Active</SectionTitle>
              {active.length === 0 ? (
                <p className="text-sm text-faint">Nothing in flight right now.</p>
              ) : (
                <ul className="space-y-2">
                  {active.map((r) => (
                    <RunRow
                      key={r.id}
                      run={r}
                      now={now}
                      waitingForVault={!vaultUnlocked && r.status === "queued"}
                      showCredential={tokenCount > 1}
                      // The personal Active list is all the viewer's own runs, so the
                      // Expedite/undo action is offered here (PRD #320 M6); reload
                      // refreshes the pill + ordering after the mutation.
                      owned
                      onExpedited={reload}
                    />
                  ))}
                </ul>
              )}
            </div>
          )}
        </>
      )}

      {isAdmin && !adminLoading && (
        <Card className="space-y-4 border-brand/20">
          <SectionTitle className="text-brand">Factory status (admin)</SectionTitle>
          {/* A factory-fetch failure alerts INSIDE the card rather than blanking the
              page: the personal sections above come from the layout's own fetch and
              are unaffected. Rendering empty lists here on a failed fetch would be a
              silent lie ("no runs across the factory"). */}
          {adminError && <Alert message={adminError} />}
          {!adminError && (
          <>
          <div>
            <h3 className="mb-2 text-xs font-semibold uppercase tracking-wider text-faint">
              Active runs · other users
            </h3>
            {/* The admin's OWN runs are hidden here (amendment 2026-08-14 (2)): they
                already sit in the Active section directly above, so repeating them
                made this card read as a duplicate. The full factory picture is the
                union of the two adjacent sections — nothing is lost, nothing repeats.
                No waitingForVault prop on these rows: with own rows gone, every row
                is another owner's, whose vault state is unknown here (PRD #32), so a
                queued row renders as a plain "queued" pill by construction. */}
            {factoryRuns.length === 0 ? (
              <p className="text-sm text-faint">No active runs from other users.</p>
            ) : (
              <ul className="space-y-2">
                {factoryRuns.map((r) => (
                  <RunRow
                    key={r.id}
                    run={r}
                    now={now}
                    showOwner
                    // PRD #295 D2: the admin factory list shows every run's credential
                    // (an admin auditing spend wants provenance), unconditionally — the
                    // >1-token gate is the personal viewer's, not this cross-user list's.
                    // The compact badge is inherently non-linked (it renders inside the
                    // row <Link>), so it never points the admin at their own /settings for
                    // another user's token — no linkable=false is needed.
                    showCredential
                  />
                ))}
              </ul>
            )}
          </div>
          <div>
            <h3 className="mb-2 text-xs font-semibold uppercase tracking-wider text-faint">
              Workers · all users
            </h3>
            {adminWorkers.length === 0 ? (
              <p className="text-sm text-faint">No workers registered.</p>
            ) : (
              <ul className="space-y-2">
                {adminWorkers.map((w) => (
                  <li
                    key={w.id}
                    className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-edge bg-raised/40 px-3 py-2 text-sm"
                  >
                    <div>
                      {/* Issue #124, and this one is CROSS-PRINCIPAL: this list comes from
                          `api.adminListWorkers()` -> ListAllWorkers, which embeds every
                          user's worker row, so an admin reads names ANOTHER user chose.

                          The ingest gap this was written against is CLOSED as of #169:
                          `handler/workers.go` now runs `termsafe.Validate`, so a name with
                          a bidi override or a bare ESC is a 400 rather than a stored row.
                          The strip stays, and is not redundant, for the reason #169 gives
                          for splitting the two halves: rows stored BEFORE that validator
                          landed cannot be cleaned retroactively, so the render boundary is
                          the trust boundary and the validator is defence in depth behind
                          it -- not the other way round. */}
                      <span className="font-medium text-fg">{stripUnsafeChars(w.name)}</span>
                      <span className="ml-2 text-xs text-faint">{w.owner_email}</span>
                      {(w.template_reported || w.template_declared) && (
                        <span className="ml-2 text-xs text-faint">
                          template {w.template_reported ?? `${w.template_declared} (declared)`}
                        </span>
                      )}
                      {w.status === "online" && w.online_since && (
                        <span className="ml-2 text-xs text-faint">up {formatUptimeSince(w.online_since)}</span>
                      )}
                    </div>
                    <div className="flex items-center gap-1.5">
                      {hasTemplateDrift(w.template_declared, w.template_reported) && (
                        <Badge
                          tone="warning"
                          title={`Declared ${w.template_declared}, worker reports ${w.template_reported}`}
                        >
                          template drift
                        </Badge>
                      )}
                      <Badge tone={w.status === "online" ? "ok" : "neutral"} dot>
                        {w.status}
                      </Badge>
                      <WorkerRunBadge worker={w} />
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </div>
          </>
          )}
        </Card>
      )}
    </>
  );
}

// The archive: every finished run, searchable, date-grouped, progressively
// revealed. Search state lives in ?q= so a filtered view is shareable — the whole
// point of the route split (replace:true keeps typing out of the history stack, so
// Back leaves the page rather than un-typing).
export function RunsHistory() {
  // Every row here is terminal, so the duration token is static ("ran 42m") — a
  // minute-cadence clock keeps the "N minutes ago"-class derivations honest without
  // the live console's 1s tick, which only running rows need (review-wave fix 5).
  const now = useNow(60_000);

  // Runs, secrets gate, load/error state all come from the layout's one fetch —
  // this page owns only its search + reveal state.
  const { runs, loading, error, tokenCount } = useRunsData();
  const [searchParams, setSearchParams] = useSearchParams();
  const query = searchParams.get("q") ?? "";
  const setQuery = useCallback(
    (v: string) => setSearchParams(v ? { q: v } : {}, { replace: true }),
    [setSearchParams],
  );
  // The render slice the reveal button grows; lanePaging clamps it up to the
  // baseline (PAST_CAP, or PAGE while a search is active), so 0 means "at baseline".
  const [shownCount, setShownCount] = useState(0);

  // `/` focuses the archive search — the board's M5 shortcut, same guard: never
  // steal a literal slash the user is typing into another field.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "/") return;
      const el = document.activeElement as HTMLElement | null;
      const tag = el?.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA" || el?.isContentEditable) return;
      e.preventDefault();
      document.getElementById(SEARCH_INPUT_ID)?.focus();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  const q = query.trim();
  const searchActive = q.length > 0;
  // Starting or clearing a search re-baselines the reveal (the board does the same
  // on its searchActive toggle): a slice grown while browsing must not leak into
  // search results, nor the reverse.
  useEffect(() => {
    setShownCount(0);
  }, [searchActive]);

  const past = runs.filter((r) => isTerminalRun(r.status)).sort(sortPast);
  // Membership → search → slice → group (the board's Decision 6 order, transposed):
  // grouping runs over the SLICED list so a group never renders half its rows with a
  // header count claiming more, while the reveal button carries the honest remainder.
  const pastFiltered = searchActive ? past.filter((r) => runMatchesQuery(r, q)) : past;
  const paging = lanePaging({
    total: pastFiltered.length,
    shownCount,
    cap: PAST_CAP,
    page: PAGE,
    searchActive,
  });
  const pastGroups = groupRuns(pastFiltered.slice(0, paging.render), pastAnchor, now);

  return (
    <>
      {error && <Alert message={error} />}
      {loading && <ListSkeleton rows={4} />}

      {!loading && past.length === 0 && (
        <EmptyState
          icon={<ActivityIcon />}
          title="No finished runs yet"
          description="Finished runs collect here — completed, failed, and stopped alike. Start one from a board and it lands in this archive when it's done."
          action={
            <Link to="/repos" className="text-sm font-medium text-brand hover:text-brand-hover">
              Go to boards →
            </Link>
          }
        />
      )}

      {!loading && past.length > 0 && (
        <div className="space-y-3">
          {/* Archive toolbar, the board's shape: search first (the primary action),
              the slice state right-aligned. The tab strip above already names the
              page, so no repeated "Past runs" heading. */}
          <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
            <label htmlFor={SEARCH_INPUT_ID} className="sr-only">
              Search past runs
            </label>
            <Input
              id={SEARCH_INPUT_ID}
              type="search"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Escape") {
                  setQuery("");
                  e.currentTarget.blur();
                }
              }}
              placeholder="Search past runs…"
              className="w-72 py-1 text-xs"
            />
            <span className="ml-auto text-xs tabular-nums text-faint">
              {paging.countLabel || String(pastFiltered.length)}
            </span>
          </div>

          {/* Result count only while searching (the board's rule) — a status
              region so assistive tech hears the count settle, not each key. */}
          {searchActive && (
            <p role="status" className="text-xs text-faint">
              {pastFiltered.length} result{pastFiltered.length === 1 ? "" : "s"} for “{q}”
            </p>
          )}

          {pastGroups.map((g) => (
            <div key={g.key} className="space-y-2">
              {/* Date group header (lib/runGroups.ts): days this week, weeks
                  this month, months beyond. The hairline carries the eye across
                  without a boxed subheader. */}
              <h3 className="flex items-center gap-2 text-[11px] font-semibold uppercase tracking-wider text-faint">
                {g.label}
                <span aria-hidden="true" className="h-px flex-1 bg-edge" />
              </h3>
              <ul className="space-y-2">
                {g.runs.map((r) => (
                  <RunRow key={r.id} run={r} now={now} showCredential={tokenCount > 1} />
                ))}
              </ul>
            </div>
          ))}

          {searchActive && pastFiltered.length === 0 && (
            <p className="text-sm text-faint">
              No past runs match “{q}”.{" "}
              <button
                type="button"
                onClick={() => setQuery("")}
                className="font-medium text-brand hover:text-brand-hover"
              >
                Clear search
              </button>
            </p>
          )}

          {/* The reveal rail, copy-for-copy the board lane's (PRD #304): Show
              more grows the slice a page; Collapse returns to baseline. */}
          {(paging.showMoreBy > 0 || paging.canCollapse) && (
            <div className="flex flex-col items-start gap-1.5">
              {paging.showMoreBy > 0 && (
                <button
                  type="button"
                  onClick={() =>
                    setShownCount((c) => Math.max(c, searchActive ? PAGE : PAST_CAP) + PAGE)
                  }
                  aria-label={`Show ${paging.showMoreBy} more past runs, ${paging.remaining} hidden`}
                  className="rounded-md border border-edge bg-raised px-2 py-1 text-xs text-muted transition-colors hover:text-fg"
                >
                  Show {paging.showMoreBy} more · {paging.remaining} left
                </button>
              )}
              {paging.canCollapse && (
                <button
                  type="button"
                  onClick={() => setShownCount(0)}
                  aria-label="Collapse past runs"
                  className="text-xs text-faint transition-colors hover:text-muted"
                >
                  Collapse
                </button>
              )}
              {paging.nudgeSearch && !searchActive && (
                <p className="text-[11px] text-faint">Too many to show — use search to narrow.</p>
              )}
            </div>
          )}
        </div>
      )}
    </>
  );
}
