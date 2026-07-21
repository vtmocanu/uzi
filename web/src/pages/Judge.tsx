// The Judge menu (PRD #98 M3): a cross-run recommendation workbench. It reads the
// caller's recommendations deduped by (category, target) across all their runs
// (GET /me/judge/recommendations), and disposes a whole group — or several via
// multi-select — in one bulk fan-out call (PUT .../disposition). Filing stays the
// existing #68 per-recommendation browser draft, reachable from a group's occurrence
// expander (bulk file-as-one-issue is a follow-up PRD, Decision 3).
//
// Three contracts consumed AS DOCUMENTED, not as assumed (see api.ts + the checkpoint):
//   - `triage` is the canonical count from the separate stats query — the tabs, the nav
//     badge and the notification all read it, never a tally of the groups on screen.
//   - `truncated` means a SURVIVING group may be understated and a just-acted-on group
//     may be ABSENT; a missing group is UNKNOWN, never "settled and gone".
//   - a bulk disposition RE-READS the acted-on groups at bucket=all, so a just-dismissed
//     row RE-RENDERS at its new rollup rather than being filtered out client-side.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import {
  api,
  ApiError,
  type JudgeBacklog,
  type JudgeBacklogBucket,
  type JudgeDispositionCoord,
  type JudgeOccurrence,
  type JudgeRecommendationGroup,
  type JudgeSettledMember,
  type Repo,
} from "../lib/api";
import {
  bucketTabCount,
  bucketTabLabel,
  JUDGE_BUCKETS,
  recentVerdictTrend,
  rollupLabel,
  rollupTone,
  seenInRunsLabel,
} from "../lib/judgeBacklog";
import { coordKey, recommendationLabel, verdictLabel, verdictTone } from "../lib/judge";
import { TriageSummary } from "./RunView";
import { OccurrenceFileIssue } from "../components/OccurrenceFileIssue";
import {
  Alert,
  Badge,
  Button,
  Card,
  cx,
  EmptyState,
  ListSkeleton,
  PageHeader,
  SectionTitle,
} from "../components/ui";
import { ChevronDownIcon, ChevronRightIcon, ExternalLinkIcon, ScaleIcon, XIcon } from "../components/icons";

// A member the page can Undo: the (run, rec) it will clear a disposition on. Taken from the
// RESPONSE's `settled` list — the members the server actually wrote — never from this page's
// own view of which occurrences were open.
//
// The distinction is not pedantry, it is the difference between a revert and a destructive
// delete (PRD #98 review BLK-UNDO). scope=open membership is decided SERVER-SIDE at write
// time; this page's `backlog` is as old as its last load. Any member settled in between is
// `todo` here and excluded there, so a snapshot-based undo issues deleteDisposition for a
// disposition this action never created. For an M6 issue-close auto-done that is
// IRREVERSIBLE: close_synced_at is already stamped, so the edge-triggered poller never
// re-fires and the set_via='issue_close' provenance is destroyed. The page cannot narrow
// this itself — `updated` is a bare count, and its own view is the stale thing.
type UndoMember = JudgeSettledMember;

type Toast = {
  message: string;
  // The members a bulk action settled; Undo clears each one's disposition. Empty when
  // there is nothing to undo (e.g. the action matched no open member).
  undo: UndoMember[];
};

// UNDO_CONCURRENCY bounds the parallel DELETEs an Undo issues (PRD #98 review N4). Undo is
// the one path that re-expands a fan-out the server deliberately collapsed to a SINGLE
// statement: a 100-coordinate action can settle far more members than that, and firing them
// all at once from the browser is exactly the amplification M2's one-statement write exists
// to prevent, just moved to the client. Small enough to stay polite, large enough that a
// realistic undo is not perceptibly serial.
const UNDO_CONCURRENCY = 6;

export function Judge() {
  const { user } = useAuth();
  const [searchParams, setSearchParams] = useSearchParams();
  const runAnchor = searchParams.get("run") ?? "";
  // Bucket lives in the URL so a tab is shareable/back-navigable. Default is `todo`
  // (the backlog's reason to exist) — but when a run anchor is present and no bucket is
  // pinned, default to `all`: a notification deep-link (/judge?run={id}) must reliably
  // show that run's recommendations, and a run whose recs are already settled would look
  // empty under todo.
  const bucketParam = searchParams.get("bucket");
  const bucket: JudgeBacklogBucket = isBucket(bucketParam)
    ? bucketParam
    : runAnchor
      ? "all"
      : "todo";

  const [backlog, setBacklog] = useState<JudgeBacklog | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [actionErr, setActionErr] = useState("");
  const [repos, setRepos] = useState<Repo[]>([]);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [toast, setToast] = useState<Toast | null>(null);
  const toastTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const data = await api.getJudgeBacklog(bucket, runAnchor || undefined);
      setBacklog(data);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Failed to load the backlog");
    } finally {
      setLoading(false);
    }
  }, [bucket, runAnchor]);

  useEffect(() => {
    // Selection is keyed on coordinates that may not survive a reload; drop it whenever
    // the view changes so a stale checkbox can never drive an action.
    setSelected(new Set());
    load();
  }, [load]);

  // The file-issue picker lists every connected repo (#68). Best-effort: a failure just
  // leaves the picker empty and the draft still opens.
  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        const { repos } = await api.listRepos();
        if (alive) setRepos(repos);
      } catch {
        /* picker stays empty */
      }
    })();
    return () => {
      alive = false;
    };
  }, []);

  // Clear any pending toast timer on unmount.
  useEffect(() => () => {
    if (toastTimer.current) clearTimeout(toastTimer.current);
  }, []);

  const showToast = useCallback((t: Toast) => {
    if (toastTimer.current) clearTimeout(toastTimer.current);
    setToast(t);
    toastTimer.current = setTimeout(() => setToast(null), 9000);
  }, []);

  const setBucket = (b: JudgeBacklogBucket) => {
    const next = new URLSearchParams(searchParams);
    next.set("bucket", b);
    setSearchParams(next, { replace: true });
  };

  const clearRunAnchor = () => {
    const next = new URLSearchParams(searchParams);
    next.delete("run");
    setSearchParams(next, { replace: true });
  };

  // dispose fans one verdict out to every OPEN member of the given coordinates (scope=open
  // — a filed/settled member is left alone), then reconciles the page from the response's
  // bucket=all re-read (DELIBERATELY not a client-side filter): the acted-on rows RE-RENDER
  // at their new rollup, `triage` is taken from the canonical aggregate in the same
  // round-trip, and — on a truncated page — a coordinate ABSENT from the response is left
  // as-is (UNKNOWN), never dropped. Undo reverts exactly the members the RESPONSE reports
  // settling — never a set derived from this page's own (older) view of what was open.
  const dispose = useCallback(
    async (coords: JudgeDispositionCoord[], status: "done" | "dismissed", reason?: "wont_do" | "not_an_issue") => {
      if (coords.length === 0) return;
      setActionErr("");
      try {
        const res = await api.bulkSetJudgeDisposition(coords, status, reason, "open");
        // Reconcile IN PLACE from res.groups (the bucket=all re-read). Replace each acted-on
        // row with its fresh version so it re-renders at its new rollup; NEVER filter a row
        // out because it left the active bucket, and NEVER drop a coordinate that is absent
        // from res.groups (truncation → its state is UNKNOWN, not settled).
        setBacklog((prev) => {
          if (!prev) return prev;
          const fresh = new Map(res.groups.map((g) => [coordKey(g.category, g.target), g]));
          const groups = prev.groups.map((g) => fresh.get(coordKey(g.category, g.target)) ?? g);
          return {
            ...prev,
            groups,
            triage: res.triage,
            truncated: prev.truncated || res.truncated,
          };
        });
        setSelected(new Set());
        const verb = status === "done" ? "marked done" : "dismissed";
        showToast({
          message:
            res.updated === 0
              ? "Nothing to update — those members were already settled."
              : // "coordinates", not "recommendations": `updated` counts (review_id,
                // category, target) TRIPLES, and one review can carry the same coordinate on
                // two recommendations that share ONE disposition row. So this number can be
                // lower than the recommendations the group visibly spans, and calling it a
                // recommendation count states something the user can disprove by expanding
                // the group. (The CLI's `uzi review resolve --category/--target` says
                // "member coordinate(s)" for the same reason.)
                `${res.updated} ${res.updated === 1 ? "coordinate" : "coordinates"} ${verb}${
                  res.truncated ? " (backlog partial — some may be off-page)" : ""
                }.`,
          // Straight from the response: the members the SERVER settled. Never a set derived
          // from `res.updated` or from this page's occurrences — see UndoMember.
          undo: res.settled,
        });
      } catch (e) {
        setActionErr(e instanceof ApiError ? e.message : "Could not apply the disposition");
      }
    },
    [backlog, showToast],
  );

  const undo = useCallback(async () => {
    const members = toast?.undo ?? [];
    setToast(null);
    if (toastTimer.current) clearTimeout(toastTimer.current);
    if (members.length === 0) return;
    setActionErr("");
    // Clear each settled coordinate. deleteDisposition swallows a 404 (already cleared), so
    // a duplicate member (two recs sharing a coordinate in one review) is a safe no-op.
    //
    // Two properties this loop has and `Promise.all(members.map(...))` did not (PRD #98
    // review N4):
    //
    //  1. CONCURRENCY IS BOUNDED. Undo re-expands, one request per member, the fan-out the
    //     server deliberately collapsed into a single statement — so the unbounded form put
    //     back exactly the amplification M2 removed, on the client side.
    //  2. PARTIAL FAILURE IS REPORTED HONESTLY. Promise.all rejects on the FIRST failure
    //     while the remaining requests stay in flight and their outcomes are discarded, so
    //     the user was told "Could not undo" while an unknown number of members HAD been
    //     reverted. Undo is a destructive operation; "some of it happened and I will not say
    //     which" is the one report it must not give. Every member is now attempted and the
    //     tally is reported.
    let failed = 0;
    const queue = [...members];
    const workers = Array.from({ length: Math.min(UNDO_CONCURRENCY, queue.length) }, async () => {
      for (let m = queue.shift(); m !== undefined; m = queue.shift()) {
        try {
          await api.deleteDisposition(m.run_id, m.rec_id);
        } catch {
          failed += 1;
        }
      }
    });
    await Promise.all(workers);
    if (failed > 0) {
      setActionErr(
        failed === members.length
          ? "Could not undo — nothing was reverted."
          : `Partly undone: ${members.length - failed} of ${members.length} reverted, ${failed} failed. Re-check the affected groups.`,
      );
    }
    // Reload either way: after a partial failure the page must show what IS true, not the
    // state either outcome would have implied.
    await load();
  }, [toast, load]);

  const toggleSelect = (key: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  const selectedCoords: JudgeDispositionCoord[] = useMemo(() => {
    const out: JudgeDispositionCoord[] = [];
    for (const g of backlog?.groups ?? []) {
      if (selected.has(coordKey(g.category, g.target))) out.push({ category: g.category, target: g.target });
    }
    return out;
  }, [backlog, selected]);

  const triage = backlog?.triage;
  // Inbox-zero is a first-class LANDING view: it shows when the To-triage bucket comes
  // back genuinely empty. It gates on `groups.length === 0` — NOT on triage.todo alone —
  // so a bulk action that drops todo to 0 does not yank the just-acted-on rows out from
  // under the user mid-interaction (they stay, re-rendered at their new rollup, with Undo);
  // the zero-state appears on the next load/navigation instead.
  const showZeroState =
    bucket === "todo" && !runAnchor && triage != null && triage.todo === 0 && (backlog?.groups.length ?? 0) === 0;

  return (
    <div className="space-y-6 pb-24">
      <PageHeader
        titleNode={
          <h1 className="flex items-center gap-2 text-xl font-semibold tracking-tight">
            <span className="text-brand" aria-hidden="true">
              <ScaleIcon />
            </span>
            Judge
          </h1>
        }
        description="Recommendations across all your runs, deduped by target. Triage a whole group in one action."
      />

      {error && <Alert message={error} />}
      {actionErr && <Alert message={actionErr} />}

      {runAnchor && (
        <div className="flex flex-wrap items-center gap-2 rounded-lg border border-brand/30 bg-brand/[0.06] px-3 py-2 text-sm">
          <span className="text-muted">Filtered to one run's recommendations (from a notification).</span>
          <button
            type="button"
            onClick={clearRunAnchor}
            className="inline-flex items-center gap-1 font-medium text-brand underline underline-offset-2 hover:text-brand-hover"
          >
            <XIcon /> Clear filter
          </button>
        </div>
      )}

      {/* The aggregate strip's new home (Decision 7): the count that used to live on /runs. */}
      {triage && triage.total > 0 && (
        <TriageSummary triage={triage} title="Recommendations · all your runs" aside="all time" />
      )}

      {/* Bucket tabs — counts straight from the canonical triage aggregate, never tallied
          off the (possibly filtered/truncated) groups on screen. */}
      {triage && (
        <div role="tablist" aria-label="Backlog bucket" className="flex flex-wrap gap-1 border-b border-edge">
          {JUDGE_BUCKETS.map((b) => {
            const active = b === bucket;
            return (
              <button
                key={b}
                role="tab"
                aria-selected={active}
                onClick={() => setBucket(b)}
                className={cx(
                  "-mb-px border-b-2 px-3 py-2 text-sm font-medium transition-colors",
                  active
                    ? "border-brand text-fg"
                    : "border-transparent text-faint hover:border-edge-strong hover:text-muted",
                )}
              >
                {bucketTabLabel(b)}
                <span className="ml-1.5 tabular-nums text-faint">{bucketTabCount(triage, b)}</span>
              </button>
            );
          })}
        </div>
      )}

      {backlog?.truncated && (
        <Alert
          tone="warning"
          message="This backlog is large and was truncated — some groups' counts may be understated and a few groups may be missing. Dispose a smaller slice or narrow by bucket to see the whole picture."
        />
      )}

      {loading && <ListSkeleton rows={5} />}

      {!loading && backlog && (
        <>
          {showZeroState ? (
            <ZeroState judgeEnabled={!!user?.judge_enabled} />
          ) : backlog.groups.length === 0 ? (
            <EmptyState
              icon={<ScaleIcon />}
              title={runAnchor ? "No recommendations for this run" : `Nothing under ${bucketTabLabel(bucket)}`}
              description={
                runAnchor
                  ? "This run has no recommendations in the selected bucket. Clear the filter or switch buckets."
                  : "Switch buckets to see recommendations in another state."
              }
            />
          ) : (
            <ul className="space-y-2">
              {backlog.groups.map((g) => (
                <GroupRow
                  key={coordKey(g.category, g.target)}
                  group={g}
                  selected={selected.has(coordKey(g.category, g.target))}
                  onToggleSelect={() => toggleSelect(coordKey(g.category, g.target))}
                  onDispose={(status, reason) => dispose([{ category: g.category, target: g.target }], status, reason)}
                  repos={repos}
                  onFiled={load}
                />
              ))}
            </ul>
          )}
        </>
      )}

      {selectedCoords.length > 0 && (
        <MultiSelectBar
          count={selectedCoords.length}
          onClear={() => setSelected(new Set())}
          onDispose={(status, reason) => dispose(selectedCoords, status, reason)}
        />
      )}

      {toast && <UndoToast toast={toast} onUndo={undo} onDismiss={() => setToast(null)} />}
    </div>
  );
}

function isBucket(v: string | null): v is JudgeBacklogBucket {
  return v === "todo" || v === "filed" || v === "done" || v === "dismissed" || v === "all";
}

// GroupRow is one deduped (category, target) row: the category + target header, the "seen
// in N runs" frequency chip, the rollup badge, the group actions, and the occurrence
// expander. rationale_preview is UNTRUSTED judge text — rendered as escaped React text with
// whitespace-pre-wrap, NEVER through a markdown renderer or dangerouslySetInnerHTML.
function GroupRow({
  group,
  selected,
  onToggleSelect,
  onDispose,
  repos,
  onFiled,
}: {
  group: JudgeRecommendationGroup;
  selected: boolean;
  onToggleSelect: () => void;
  onDispose: (status: "done" | "dismissed", reason?: "wont_do" | "not_an_issue") => void;
  repos: Repo[];
  onFiled: () => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const openCount = group.open_count;

  return (
    <li className="rounded-lg border border-edge bg-raised/40">
      <div className="flex flex-wrap items-start gap-2 px-3 py-2.5">
        <input
          type="checkbox"
          checked={selected}
          onChange={onToggleSelect}
          aria-label={`Select ${recommendationLabel(group.category)} ${group.target}`}
          className="mt-1 h-4 w-4 shrink-0 accent-brand"
        />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <Badge tone="info">{recommendationLabel(group.category)}</Badge>
            {group.target.trim() !== "" && (
              <code className="rounded bg-raised px-1.5 py-0.5 font-mono text-xs text-fg">{group.target}</code>
            )}
            <span className="text-xs text-faint">{seenInRunsLabel(group.run_count)}</span>
            {group.bucket !== "todo" && <Badge tone={rollupTone(group.bucket)}>{rollupLabel(group.bucket)}</Badge>}
            {openCount > 0 && <span className="text-xs text-faint">{openCount} open</span>}
          </div>
          {group.rationale_preview.trim() !== "" && (
            <p className="mt-1.5 line-clamp-3 whitespace-pre-wrap text-sm text-muted">{group.rationale_preview}</p>
          )}
        </div>
        <div className="flex shrink-0 items-center gap-1.5">
          <GroupDisposeControls disabled={openCount === 0} onDispose={onDispose} />
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            aria-expanded={expanded}
            aria-label={expanded ? "Collapse occurrences" : "Expand occurrences"}
            className="rounded-md p-1 text-faint transition-colors hover:bg-raised hover:text-fg"
          >
            {expanded ? <ChevronDownIcon /> : <ChevronRightIcon />}
          </button>
        </div>
      </div>

      {expanded && (
        <ul className="space-y-2 border-t border-edge px-3 py-2.5">
          {group.occurrences.map((occ) => (
            <OccurrenceRow
              key={`${occ.run_id} ${occ.rec_id}`}
              occ={occ}
              repos={repos}
              onFiled={onFiled}
            />
          ))}
        </ul>
      )}
    </li>
  );
}

// GroupDisposeControls: the per-group Mark done + Dismiss ▾ (Won't do / Not an issue),
// mirroring RunView's per-rec controls but fanning out across the group. Disabled when
// the group has no open member — scope=open would settle nothing.
function GroupDisposeControls({
  disabled,
  onDispose,
}: {
  disabled: boolean;
  onDispose: (status: "done" | "dismissed", reason?: "wont_do" | "not_an_issue") => void;
}) {
  const [menuOpen, setMenuOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!menuOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setMenuOpen(false);
        wrapRef.current?.querySelector<HTMLElement>("button")?.focus();
      }
    };
    const onPointerDown = (e: Event) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setMenuOpen(false);
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("pointerdown", onPointerDown);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("pointerdown", onPointerDown);
    };
  }, [menuOpen]);

  if (disabled) return null;

  return (
    <div className="flex items-center gap-1.5">
      <Button size="sm" variant="secondary" onClick={() => onDispose("done")}>
        Mark done
      </Button>
      <div className="relative" ref={wrapRef}>
        <Button
          size="sm"
          variant="secondary"
          aria-haspopup="menu"
          aria-expanded={menuOpen}
          onClick={() => setMenuOpen((o) => !o)}
        >
          Dismiss ▾
        </Button>
        {menuOpen && (
          <div
            role="menu"
            className="absolute right-0 z-10 mt-1 w-56 rounded-lg border border-edge-strong bg-surface p-1 shadow-lg"
          >
            <button
              type="button"
              role="menuitem"
              onClick={() => {
                setMenuOpen(false);
                onDispose("dismissed", "wont_do");
              }}
              className="flex w-full flex-col gap-0.5 rounded-md px-2.5 py-2 text-left text-sm text-fg transition-colors hover:bg-raised"
            >
              Won't do
              <span className="text-xs text-faint">Valid, but not worth acting on</span>
            </button>
            <button
              type="button"
              role="menuitem"
              onClick={() => {
                setMenuOpen(false);
                onDispose("dismissed", "not_an_issue");
              }}
              className="flex w-full flex-col gap-0.5 rounded-md px-2.5 py-2 text-left text-sm text-fg transition-colors hover:bg-raised"
            >
              Not an issue
              <span className="text-xs text-faint">False positive — the judge got it wrong</span>
            </button>
          </div>
        )}
      </div>
    </div>
  );
}

// OccurrenceRow is one run's instance inside the expander: the run link, its verdict, the
// per-run triage state, and the per-recommendation File-issue draft. run_title is UNTRUSTED
// (the run's issue_title) — rendered as escaped React text.
function OccurrenceRow({ occ, repos, onFiled }: { occ: JudgeOccurrence; repos: Repo[]; onFiled: () => void }) {
  return (
    <li className="rounded-md border border-edge bg-surface/60 px-2.5 py-2">
      <div className="flex flex-wrap items-center gap-2">
        <Link
          to={`/runs/${occ.run_id}`}
          className="min-w-0 max-w-full truncate text-sm font-medium text-fg underline-offset-2 hover:underline"
        >
          {occ.run_title || "Untitled run"}
        </Link>
        <Badge tone={verdictTone(occ.verdict)}>{verdictLabel(occ.verdict)}</Badge>
        <OccurrenceBucketChip occ={occ} />
      </div>
      {/* Filing stays per-recommendation via the existing #68 browser draft (Decision 3):
          a filed occurrence renders its link, an open one offers the draft, a settled-but-
          unfiled one (done/dismissed) offers nothing to file. */}
      {occ.filed_issue ? (
        <OccurrenceFileIssue runId={occ.run_id} recId={occ.rec_id} filed={occ.filed_issue} repos={repos} />
      ) : occ.bucket === "todo" ? (
        <OccurrenceFileIssue runId={occ.run_id} recId={occ.rec_id} repos={repos} onFiled={onFiled} />
      ) : null}
    </li>
  );
}

// OccurrenceBucketChip renders one occurrence's triage state from its bucket. The
// occurrence DTO carries no dismiss reason, so a dismissed occurrence reads a plain
// "Dismissed" — the group-level controls carry the won't-do / not-an-issue distinction.
//
// A DONE splits by provenance (PRD #98 Decision 6): a person's "✓ Done" and the M6
// issue-close sync's "Done via #IID" are different claims, and rendering them identically
// attributes a system inference to the user. Both are bucket "done", so the split is on
// set_via, which is the only thing that carries the difference.
function OccurrenceBucketChip({ occ }: { occ: JudgeOccurrence }) {
  switch (occ.bucket) {
    case "done":
      // An auto-done always has a filed link — the sync fires FROM one closing — but the
      // link is rendered defensively anyway: a filed row deleted after the sync would
      // otherwise print "Done via #undefined". Without an iid the provenance is still
      // stated, just unnamed.
      if (occ.set_via === "issue_close") {
        return (
          <Badge tone="ok" title="Marked done automatically when the filed issue was closed">
            <span aria-hidden="true">✓</span>{" "}
            {occ.filed_issue ? `Done via #${occ.filed_issue.issue_iid}` : "Done via issue close"}
          </Badge>
        );
      }
      return (
        <Badge tone="ok">
          <span aria-hidden="true">✓</span> Done
        </Badge>
      );
    case "dismissed":
      return <Badge tone="neutral">Dismissed</Badge>;
    case "filed":
      return occ.filed_issue && isHttpsUrl(occ.filed_issue.issue_url) ? (
        <a
          href={occ.filed_issue.issue_url}
          target="_blank"
          rel="noopener noreferrer"
          className="inline-flex items-center gap-1 text-xs font-medium text-info underline underline-offset-2 hover:text-info"
        >
          Filed #{occ.filed_issue.issue_iid} <ExternalLinkIcon />
        </a>
      ) : (
        <Badge tone="info">Filed</Badge>
      );
    default:
      return <Badge tone="neutral">To do</Badge>;
  }
}

function isHttpsUrl(u: string): boolean {
  try {
    return new URL(u).protocol === "https:";
  } catch {
    return false;
  }
}

// MultiSelectBar is the sticky action bar for the checkbox selection: it fans one verdict
// out across every selected group in one bulk call (Decision 3's multi-select).
function MultiSelectBar({
  count,
  onClear,
  onDispose,
}: {
  count: number;
  onClear: () => void;
  onDispose: (status: "done" | "dismissed", reason?: "wont_do" | "not_an_issue") => void;
}) {
  const [menuOpen, setMenuOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!menuOpen) return;
    const onPointerDown = (e: Event) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setMenuOpen(false);
    };
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [menuOpen]);

  return (
    <div className="fixed inset-x-0 bottom-0 z-20 border-t border-edge bg-surface/95 px-4 py-3 backdrop-blur">
      <div className="mx-auto flex w-full max-w-5xl flex-wrap items-center gap-3">
        <span className="text-sm font-medium text-fg">
          {count} {count === 1 ? "group" : "groups"} selected
        </span>
        <span className="text-xs text-faint">Actions apply to open members only.</span>
        <div className="ml-auto flex items-center gap-2">
          <Button size="sm" variant="secondary" onClick={() => onDispose("done")}>
            Mark done
          </Button>
          <div className="relative" ref={wrapRef}>
            <Button
              size="sm"
              variant="secondary"
              aria-haspopup="menu"
              aria-expanded={menuOpen}
              onClick={() => setMenuOpen((o) => !o)}
            >
              Dismiss ▾
            </Button>
            {menuOpen && (
              <div
                role="menu"
                className="absolute bottom-full right-0 z-10 mb-1 w-56 rounded-lg border border-edge-strong bg-surface p-1 shadow-lg"
              >
                <button
                  type="button"
                  role="menuitem"
                  onClick={() => {
                    setMenuOpen(false);
                    onDispose("dismissed", "wont_do");
                  }}
                  className="flex w-full flex-col gap-0.5 rounded-md px-2.5 py-2 text-left text-sm text-fg transition-colors hover:bg-raised"
                >
                  Won't do
                  <span className="text-xs text-faint">Valid, but not worth acting on</span>
                </button>
                <button
                  type="button"
                  role="menuitem"
                  onClick={() => {
                    setMenuOpen(false);
                    onDispose("dismissed", "not_an_issue");
                  }}
                  className="flex w-full flex-col gap-0.5 rounded-md px-2.5 py-2 text-left text-sm text-fg transition-colors hover:bg-raised"
                >
                  Not an issue
                  <span className="text-xs text-faint">False positive — the judge got it wrong</span>
                </button>
              </div>
            )}
          </div>
          <Button size="sm" variant="ghost" onClick={onClear}>
            Clear
          </Button>
        </div>
      </div>
    </div>
  );
}

// UndoToast confirms a bulk action and offers a one-click revert. Undo clears exactly the
// members the SERVER reported settling (the response's `settled` list), one deleteDisposition
// each, at bounded concurrency — not the members this page believed were open, which is a
// staler set and can include coordinates the action never touched (see UndoMember). A
// role="status" live region so the confirmation is announced.
function UndoToast({ toast, onUndo, onDismiss }: { toast: Toast; onUndo: () => void; onDismiss: () => void }) {
  return (
    <div className="fixed inset-x-0 bottom-20 z-30 flex justify-center px-4">
      <div
        role="status"
        className="flex items-center gap-3 rounded-lg border border-edge-strong bg-surface px-4 py-2.5 text-sm shadow-lg"
      >
        <span className="text-fg">{toast.message}</span>
        {toast.undo.length > 0 && (
          <button
            type="button"
            onClick={onUndo}
            className="font-medium text-brand underline underline-offset-2 hover:text-brand-hover"
          >
            Undo
          </button>
        )}
        <button
          type="button"
          onClick={onDismiss}
          aria-label="Dismiss"
          className="rounded-md p-0.5 text-faint transition-colors hover:text-fg"
        >
          <XIcon />
        </button>
      </div>
    </div>
  );
}

// ZeroState is the first-class inbox-zero view (Decision 8): to-triage = 0 is the goal, so
// the page is not blank. It fetches the bucket=all snapshot to show the recent-verdict trend
// and the recently Filed / Done groups, and — when the user has not opted into the judge —
// an opt-in card linking Settings. A badge-less nav item most of the week is expected; this
// is what keeps the destination worth opening.
function ZeroState({ judgeEnabled }: { judgeEnabled: boolean }) {
  const [all, setAll] = useState<JudgeBacklog | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        const data = await api.getJudgeBacklog("all");
        if (alive) setAll(data);
      } catch {
        /* the headline still renders without the snapshot */
      } finally {
        if (alive) setLoading(false);
      }
    })();
    return () => {
      alive = false;
    };
  }, []);

  const trend = all ? recentVerdictTrend(all.groups) : null;
  const settled = (all?.groups ?? [])
    .filter((g) => g.bucket === "done" || g.bucket === "filed")
    .slice(0, 6);

  return (
    <div className="space-y-4">
      <Card className="space-y-2 border-ok/30 bg-ok/[0.04] p-5 text-center">
        <p className="text-lg font-semibold text-fg">Inbox zero — nothing to triage.</p>
        <p className="text-sm text-muted">
          Every recommendation across your runs is handled. New verdicts will show up here as your runs finish.
        </p>
      </Card>

      {!judgeEnabled && (
        <Card className="flex flex-wrap items-center justify-between gap-3 border-brand/30 bg-brand/[0.05] p-4">
          <div className="min-w-0">
            <p className="text-sm font-medium text-fg">The judge is off for your account.</p>
            <p className="text-sm text-faint">
              Turn it on and uzi reviews each finished run, surfacing recommendations here.
            </p>
          </div>
          <Link
            to="/settings"
            className="shrink-0 rounded-lg bg-brand px-3 py-1.5 text-sm font-medium text-on-brand hover:bg-brand-hover"
          >
            Enable in Settings
          </Link>
        </Card>
      )}

      {loading && <ListSkeleton rows={2} />}

      {trend && trend.total > 0 && (
        <Card className="space-y-2 p-4">
          <SectionTitle>Recent verdicts</SectionTitle>
          <div className="flex flex-wrap gap-x-4 gap-y-1 text-sm">
            <VerdictCount label="ideal" n={trend.ideal} tone="ok" />
            <VerdictCount label="ok" n={trend.ok} tone="info" />
            <VerdictCount label="issues" n={trend.issues} tone="warning" />
            <span className="text-faint">across {trend.total} judged {trend.total === 1 ? "run" : "runs"}</span>
          </div>
        </Card>
      )}

      {settled.length > 0 && (
        <Card className="space-y-2 p-4">
          <SectionTitle>Recently handled</SectionTitle>
          <ul className="space-y-1.5">
            {settled.map((g) => (
              <li key={coordKey(g.category, g.target)} className="flex flex-wrap items-center gap-2 text-sm">
                <Badge tone={rollupTone(g.bucket)}>{rollupLabel(g.bucket)}</Badge>
                <span className="text-muted">{recommendationLabel(g.category)}</span>
                {g.target.trim() !== "" && (
                  <code className="rounded bg-raised px-1.5 py-0.5 font-mono text-xs text-fg">{g.target}</code>
                )}
                <span className="text-xs text-faint">{seenInRunsLabel(g.run_count)}</span>
              </li>
            ))}
          </ul>
        </Card>
      )}
    </div>
  );
}

function VerdictCount({ label, n, tone }: { label: string; n: number; tone: "ok" | "info" | "warning" }) {
  const dot = tone === "ok" ? "bg-ok" : tone === "info" ? "bg-info" : "bg-warn";
  return (
    <span className="inline-flex items-baseline gap-1.5">
      <span aria-hidden="true" className={cx("inline-block h-2 w-2 self-center rounded-full", dot)} />
      <b className="text-sm font-semibold tabular-nums text-fg">{n}</b>
      <span className="uppercase tracking-wide text-faint">{label}</span>
    </span>
  );
}
