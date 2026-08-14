// Runs index. Active runs are always visible up top; past (terminal) runs read
// like the board (PRD #304): searchable, grouped by date at a recency-graded
// grain (days this week → weeks this month → months beyond, lib/runGroups.ts),
// and render-sliced through the board's own lanePaging (cap 10, page 50 — the
// slice decides display, never membership). The sort keeps multica's rule that
// failed outranks cancelled outranks completed at equal timestamps
// (PAST_STATUS_RANK). The row status pill keeps PRD #12's "a deliberate stop is
// not a failure" nuance: isStoppedRun collapses cancelled / stop_kind-stamped-
// failed runs (PRD #33) to a calm "stopped" pill, not "failed".

import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, isTerminalRun, type AdminWorker, type RunListItem, type RunUsage } from "../lib/api";
import { Alert, Badge, Card, EmptyState, Input, ListSkeleton, PageHeader, SectionTitle, StatusPill } from "../components/ui";
import { ActivityIcon } from "../components/icons";
import { lanePaging } from "../lib/boardColumns";
import { groupRuns, runMatchesQuery } from "../lib/runGroups";
import { MrChip } from "../components/MrChip";
import { mrAbbrev } from "../lib/forgeNoun";
import { isStoppedRun, milestoneBadge, milestoneBadgeText, mrChipState } from "../lib/runBadge";
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

const PAST_STATUS_RANK: Record<string, number> = { failed: 0, cancelled: 1, completed: 2 };

// The past section's render slice, borrowing the board's constants (PRD #304):
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

// The meta line's "tok" figure is the run's ALL-token total (fresh + cached + cache
// creation + output), matching the mock's single "1.33M tok".
function runUsageTotalTokens(u: RunUsage): number {
  return u.input_tokens + u.cache_read_tokens + u.cache_creation_tokens + u.output_tokens;
}

function sortPast(a: RunListItem, b: RunListItem): number {
  const t = pastAnchor(b).localeCompare(pastAnchor(a));
  if (t !== 0) return t;
  return (PAST_STATUS_RANK[a.status] ?? 3) - (PAST_STATUS_RANK[b.status] ?? 3);
}

function RunRow({
  run,
  now,
  showOwner,
  waitingForVault = false,
  showCredential = false,
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
}) {
  // A deliberate human stop (cancelled, or failed carrying a server-stamped
  // stop_kind — PRD #33) reads "stopped" / neutral, never "failed" / danger. Fold
  // that into the pill's status so the shared StatusPill palette renders it calm.
  const pillStatus = isStoppedRun(run.status, run.stop_kind) ? "stopped" : run.status;
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
  return (
    <li>
      <Link
        to={`/runs/${run.id}`}
        className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-edge bg-raised/40 px-3 py-2.5 transition-colors hover:border-edge-strong hover:bg-raised/70"
      >
        <div className="min-w-0 flex-1">
          {/* Issue #124: the run title is the forge ISSUE title — writable by anyone who
              can open an issue on the target repo, so it is untrusted free text on the same
              footing as judge output. Display-only here; the raw value stays the identity. */}
          <p className="truncate text-sm font-medium text-fg">{stripUnsafeChars(run.issue_title)}</p>
          <p className="mt-0.5 flex flex-wrap items-center gap-x-2 text-xs text-faint">
            <span>
              {run.repo_path} #{run.issue_iid}
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
                className="font-medium"
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
        <div className="flex flex-wrap items-center gap-2">
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
              <StatusPill status={pillStatus} />
              {/* PRD #295: which Anthropic token this run's claim billed. Gated by
                  the caller (personal list: viewer holds >1 token) and self-hiding
                  on a run with no recorded label. */}
              {showCredential && <RunCredential run={run} variant="compact" />}
            </>
          )}
        </div>
      </Link>
    </li>
  );
}

export function RunsList() {
  const { user, vaultUnlocked } = useAuth();
  const isAdmin = !!user?.is_admin;
  // Issue #256 M3: a 1s tick drives the live duration token on every row; the page
  // otherwise loads once (no poll), so the token would freeze without this clock.
  const now = useNow(1000);

  const [runs, setRuns] = useState<RunListItem[]>([]);
  const [adminRuns, setAdminRuns] = useState<RunListItem[]>([]);
  const [adminWorkers, setAdminWorkers] = useState<AdminWorker[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  // Past-section search + progressive reveal (ux-tweaks item 3, the board's PRD #304
  // pattern). shownCount is a render-only slice size the reveal button grows;
  // lanePaging clamps it up to the baseline (PAST_CAP, or PAGE while a search is
  // active), so 0 means "at baseline".
  const [query, setQuery] = useState("");
  const [shownCount, setShownCount] = useState(0);
  // PRD #295: the ">1 Anthropic token" gate for the personal credential badge,
  // computed once from the viewer's secrets. A single-token user sees no badge.
  const [tokenCount, setTokenCount] = useState(0);

  const load = useCallback(async () => {
    setError("");
    try {
      const [{ runs }, { secrets }, admin] = await Promise.all([
        api.listRuns(),
        // Best-effort (like Dashboard's usage calls): the secrets fetch only powers
        // the cosmetic ">1 token" credential-badge gate, so a secrets-endpoint failure
        // must not blank the whole Runs page. Fall back to no tokens → no badge.
        api.listSecrets().catch(() => ({ secrets: [] })),
        isAdmin ? Promise.all([api.adminListRuns(), api.adminListWorkers()]) : Promise.resolve(null),
      ]);
      setRuns(runs);
      setTokenCount(anthropicTokenCount(secrets));
      if (admin) {
        setAdminRuns(admin[0].runs);
        setAdminWorkers(admin[1].workers);
      }
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load runs");
    } finally {
      setLoading(false);
    }
  }, [isAdmin]);

  useEffect(() => {
    load();
  }, [load]);

  // `/` focuses the past-runs search — the board's M5 shortcut, same guard: never
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
  // Starting or clearing a search re-baselines the reveal (the board does the same on
  // its searchActive toggle): a slice grown while browsing must not leak into search
  // results, nor the reverse.
  useEffect(() => {
    setShownCount(0);
  }, [searchActive]);

  const active = runs.filter((r) => !isTerminalRun(r.status));
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
    <div className="space-y-6">
      <PageHeader title="Runs" description="Your agent runs. Open one to watch it live." />

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
                    />
                  ))}
                </ul>
              )}
            </div>
          )}
        </>
      )}

      {isAdmin && !loading && (
        <Card className="space-y-4 border-brand/20">
          <SectionTitle className="text-brand">Factory status (admin)</SectionTitle>
          <div>
            <h3 className="mb-2 text-xs font-semibold uppercase tracking-wider text-faint">
              Active runs · all users
            </h3>
            {adminRuns.length === 0 ? (
              <p className="text-sm text-faint">No active runs across the factory.</p>
            ) : (
              <ul className="space-y-2">
                {adminRuns.map((r) => (
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
                    // Only the current admin's OWN queued rows can show the vault state —
                    // another owner's vault status is unknown here (PRD #32), so theirs
                    // render as plain "queued".
                    waitingForVault={
                      !vaultUnlocked && r.status === "queued" && r.owner_email === user?.email
                    }
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
        </Card>
      )}

      {/* Past runs come LAST, after the admin factory card (user decision, amendment
          2026-08-14): the page reads live-to-archival — your active runs, then (admin)
          the factory's live state, then history. The archive is the one section that
          GROWS (Show 50 more), so anything below it would sit under an unbounded
          scroll; for non-admins there is no factory card and this was already the
          tail, so the order is now the same story for both roles. */}
      {!loading && past.length > 0 && (
        <div className="space-y-3">
          {/* Past runs are no longer hidden behind a "Show past runs" click: the
              render slice already keeps the page short, and a search box over
              invisible content would be a control pointing at nothing. */}
          <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
            <SectionTitle>Past runs</SectionTitle>
            <span className="text-xs tabular-nums text-faint">
              {paging.countLabel || String(pastFiltered.length)}
            </span>
            <div className="ml-auto">
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
                className="w-52 py-1 text-xs"
              />
            </div>
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
    </div>
  );
}
