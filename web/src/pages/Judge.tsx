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
import { useSearchParams } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import {
  api,
  type JudgeBacklog,
  type JudgeBacklogBucket,
  type JudgeDispositionCoord,
  type RecommendationCategory,
  type Repo,
} from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import {
  bucketTabCount,
  bucketTabLabel,
  judgeBridgeLine,
  JUDGE_BUCKETS,
} from "../lib/judgeBacklog";
import { coordKey, JUDGE_CATEGORIES, isCategory, recommendationLabel } from "../lib/judge";
import { TriageSummary } from "./RunView";
import { useSetJudgeTodo } from "../components/JudgeTodoContext";
import {
  Alert,
  cx,
  EmptyState,
  ListSkeleton,
  PageHeader,
} from "../components/ui";
import { ScaleIcon, XIcon } from "../components/icons";
import type { Toast } from "./judge/shared";
import { LabelFilter } from "./judge/LabelFilter";
import { MultiSelectBar } from "./judge/MultiSelectBar";
import { GroupRow } from "./judge/GroupRow";
import { ZeroState } from "./judge/ZeroState";
import { UndoToast } from "./judge/UndoToast";

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

  // ?category= is a comma-separated, multi-select label filter (PRD #235), enforced
  // server-side before the row cap — the same shape as ?bucket=/?run=. Unknown tokens are
  // dropped SILENTLY here, the way isBucket guards ?bucket=: an unrecognised URL value must
  // not render as an empty list. Parsed off the raw string in a useMemo so the derived array
  // is referentially stable for `load`'s deps (a fresh array every render would re-fetch on
  // every keystroke elsewhere in the URL). Order follows JUDGE_CATEGORIES, not URL order, so
  // the request query and the chip row cannot disagree.
  const categoryParam = searchParams.get("category") ?? "";
  const categories: RecommendationCategory[] = useMemo(() => {
    const set = new Set(categoryParam.split(",").map((s) => s.trim()).filter(isCategory));
    return JUDGE_CATEGORIES.filter((c) => set.has(c));
  }, [categoryParam]);

  const [backlog, setBacklog] = useState<JudgeBacklog | null>(null);
  // The per-category chip-count MATRIX (PRD #270), the canonical /me/judge/category-stats
  // aggregate: bucket → category → group count, held WHOLE in state and indexed at render by
  // the active bucket (categoryCounts below). Uncapped and anchor-aware, but now TAB-SCOPED
  // and TRIAGE-VARIANT — a mark-done moves a group between buckets — so it is refetched on
  // every disposition/undo/file mutation (via reloadCategoryStats()) and on a run-anchor
  // change (the useAsyncData deps below), though NOT on a bucket-tab or category toggle since
  // all buckets arrive in one payload.
  // Derived below from the useAsyncData hook, defaulting to {} so a slow or failed fetch
  // renders 0-count chips rather than crashing (see categoryStats).
  // Whether `categoryStats` reflects the LATEST issued fetch. A disposition installs the new
  // `triage` (recommendation totals) synchronously but refetches the matrix async, so between
  // the two the bridge line would reconcile the new recommendation count against the STALE
  // group total. Gate the bridge on this: false while a matrix fetch is in flight (or after it
  // failed), true only once the current fetch lands — so the reconciliation is never shown mid-flight.
  const [categoryStatsFresh, setCategoryStatsFresh] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [actionErr, setActionErr] = useState("");
  const [repos, setRepos] = useState<Repo[]>([]);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [toast, setToast] = useState<Toast | null>(null);
  const toastTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Publishes the canonical to-triage count to the nav badge. A no-op when this page is
  // mounted outside an AppShell (every unit test does that), which is exactly why the
  // BLK-BADGE regression test mounts the two TOGETHER — apart, both are already correct.
  const setJudgeTodo = useSetJudgeTodo();

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const data = await api.getJudgeBacklog(bucket, runAnchor || undefined, categories);
      setBacklog(data);
      // Keep the nav badge in step with every canonical triage this page learns, not only
      // the ones a disposition produces (PRD #98 review BLK-BADGE). `triage` here IS the
      // /me/judge/stats aggregate — the server sources it from that query rather than
      // tallying the returned rows — so this publishes the same number the badge's own poll
      // would fetch, without the round-trip.
      setJudgeTodo(data.triage.todo);
    } catch (e) {
      setError(errorMessage(e, "Failed to load the backlog"));
    } finally {
      setLoading(false);
    }
  }, [bucket, runAnchor, categories, setJudgeTodo]);

  useEffect(() => {
    // Selection is keyed on coordinates that may not survive a reload; drop it whenever
    // the view changes so a stale checkbox can never drive an action.
    setSelected(new Set());
    load();
  }, [load]);

  // The per-category chip-count MATRIX (PRD #270) is refetched on disposition/undo/file and
  // on a run-anchor change — NOT on a bucket-tab or category toggle: the matrix carries all
  // five buckets in one payload, so the active tab's counts are DERIVED at render below
  // (categoryCounts) with no round-trip. The aggregate is TAB-SCOPED and TRIAGE-VARIANT now
  // (a mark-done moves a group between buckets), reversing PRD #244's fetched-once property.
  // Best-effort: a failure leaves the chips at 0. The fetch is keyed on runAnchor only — the
  // anchor is the sole request input — and the dispose/undo/file handlers below call
  // reloadCategoryStats() explicitly on the same triggers that re-read the backlog.
  //
  // Self-healing transient (Decision 6): right after a bulk mark-done on the To-triage tab the
  // acted-on card stays visible at its new rollup (dispose re-renders it, never filters it
  // out) while a refetched `todo` chip has already decremented — a brief, deliberate mismatch
  // that reconciles on the next navigation/load.
  //
  // The stale-response/generation guard lives in useAsyncData now (one place): every fetch
  // stamps a monotonic generation and only applies its result if it is still the latest issued.
  // Without this, a back-to-back mutation (e.g. mark-done then undo within one response window)
  // resolves last-in-by-ARRIVAL, so a stale post-action matrix could stick on the chips until
  // the next navigation. The hook's guard also invalidates any in-flight fetch on unmount /
  // run-anchor change, so no response lands on an unmounted page — the discipline the old
  // `alive` flag had. Our own setCategoryStatsFresh side-effect rides the same stamp via
  // isCurrent(), so a superseded fetch cannot flip freshness true either.
  const {
    data: categoryStatsData,
    reload: reloadCategoryStats,
  } = useAsyncData(
    async ({ isCurrent }) => {
      const stats = await api.getJudgeCategoryStats(runAnchor || undefined);
      // Only the latest issued fetch may flip the freshness flag true — the hook's gen guard
      // drops a superseded result, and isCurrent() is the same stamp for our own side-effect.
      if (isCurrent()) setCategoryStatsFresh(true);
      return stats.counts_by_bucket;
    },
    [runAnchor],
    {
      // Reset every fetch (deps-driven and reload) so a post-mutation refetch hides the bridge
      // until the fresh matrix lands — the pre-hook setCategoryStatsFresh(false) opener.
      onFetchStart: () => setCategoryStatsFresh(false),
      // Best-effort: a failed matrix fetch leaves the chips at 0 and the bridge hidden; the
      // hook's own error slot is unused here (Judge keeps its own loading/error state).
      mapError: () => "",
    },
  );
  // Defaults to {} so a slow or failed fetch renders 0-count chips rather than crashing.
  const categoryStats = categoryStatsData ?? {};

  // The chip row is per-tab: index the whole matrix by the resolved bucket (todo/filed/done/
  // dismissed/all — `all` is a real key, so indexing is uniform). A bucket with no groups is
  // {} in the matrix, so its chips read 0; LabelFilter is untouched and still takes {cat: n}.
  const categoryCounts = categoryStats[bucket] ?? {};
  // The whole-backlog GROUP total for the active bucket, summed from the canonical
  // category-stats matrix slice (uncapped, not category-filtered) — the honest denominator to
  // reconcile against the whole-backlog recommendation count in the bridge line below. Never
  // backlog.groups.length, which is capped/filtered.
  const bridgeGroupTotal = Object.values(categoryCounts).reduce((s, n) => s + n, 0);

  // reloadAfterMutation refreshes BOTH the backlog and the chip matrix — the file-issue path
  // needs both (filing moves a group todo→filed), and it is passed as `onFiled` in place of a
  // bare `load` so the chips track a filing the same way they track a dispose/undo.
  const reloadAfterMutation = useCallback(() => {
    load();
    reloadCategoryStats();
  }, [load, reloadCategoryStats]);

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

  // Clearing the run anchor must not silently change the BUCKET (PRD #98 review N5). The
  // default is derived — anchored defaults to `all`, un-anchored to `todo` (Decision 1's
  // deliberate exception) — so dropping `run` from a URL that never pinned `bucket` snaps the
  // view from All back to To triage. The user asked to stop filtering by run, not to change
  // which rung they are looking at, and rows would vanish under them.
  //
  // Pinning the CURRENT bucket explicitly preserves what is on screen. It also makes the
  // resulting URL self-describing, which the derived-default form is not.
  const clearRunAnchor = () => {
    const next = new URLSearchParams(searchParams);
    next.delete("run");
    next.set("bucket", bucket);
    setSearchParams(next, { replace: true });
  };

  // toggleCategory flips one label in the ?category= set (PRD #235), a mirror of setBucket's
  // URLSearchParams writer. The joined value is ordered by JUDGE_CATEGORIES so the URL is
  // canonical regardless of click order, and an EMPTY selection DELETES the param rather than
  // leaving `?category=` behind — an empty value is "all labels" and must read as the
  // absent-param case (the server normalizes `[""]` away, but a clean URL states it too).
  const toggleCategory = (cat: RecommendationCategory) => {
    const set = new Set(categories);
    if (set.has(cat)) set.delete(cat);
    else set.add(cat);
    const ordered = JUDGE_CATEGORIES.filter((c) => set.has(c));
    const next = new URLSearchParams(searchParams);
    if (ordered.length) next.set("category", ordered.join(","));
    else next.delete("category");
    setSearchParams(next, { replace: true });
  };

  const clearCategories = () => {
    const next = new URLSearchParams(searchParams);
    next.delete("category");
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
        // The nav badge moves with the tab, in the SAME round-trip. Without this the two
        // silently disagreed after every dispose: AppShell polls on pathname changes, and a
        // disposition is not one (BLK-BADGE). The response already carries the canonical
        // aggregate, so there is nothing to re-fetch.
        setJudgeTodo(res.triage.todo);
        // Refetch the chip matrix on the SAME trigger that re-read the backlog (Decision 6):
        // a disposition moves a group between buckets, so the tab-scoped chip counts are now
        // stale. The bulk response carries the acted-on rows and the triage totals but NOT the
        // per-category matrix, so this is a separate round-trip.
        reloadCategoryStats();
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
        setActionErr(errorMessage(e, "Could not apply the disposition"));
      }
    },
    [showToast, setJudgeTodo, reloadCategoryStats],
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
    // state either outcome would have implied. Refresh the chip matrix alongside the backlog —
    // an undo moves groups back between buckets, same as a dispose (Decision 6).
    await load();
    reloadCategoryStats();
  }, [toast, load, reloadCategoryStats]);

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
  // A category-filtered view that comes back empty is NOT inbox-zero — the todo backlog may
  // be full of other labels — so a filter narrows out of the zero-state into the "no groups
  // match these labels" empty state below.
  const showZeroState =
    bucket === "todo" &&
    !runAnchor &&
    categories.length === 0 &&
    triage != null &&
    triage.todo === 0 &&
    (backlog?.groups.length ?? 0) === 0;

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
        description="Recommendations across all your runs. Triage a whole group in one action."
      />

      {error && <Alert message={error} />}
      {actionErr && <Alert message={actionErr} />}

      {runAnchor && (
        <div className="flex flex-wrap items-center gap-2 rounded-lg border border-brand/30 bg-brand/[0.06] px-3 py-2 text-sm">
          <span className="text-muted">Filtered to one run's recommendations (from a notification).</span>
          <button
            type="button"
            onClick={clearRunAnchor}
            className="inline-flex min-h-[24px] items-center gap-1 rounded-md px-2 py-1 text-xs font-medium text-muted transition-colors hover:bg-raised hover:text-fg"
          >
            <XIcon /> Clear filter
          </button>
        </div>
      )}

      {/* The aggregate strip's new home (Decision 7): the count that used to live on /runs. */}
      {triage && triage.total > 0 && (
        <TriageSummary triage={triage} title="Recommendations · all your runs" aside="all time" />
      )}

      {/* Recommendation-label filter (PRD #235): multi-select chips ABOVE the bucket tabs.
          The filter narrows the GROUP LIST only — the tabs and nav badge stay whole-backlog.
          Each chip carries a per-category GROUP count sourced from the canonical
          /me/judge/category-stats matrix, now SCOPED TO THE SELECTED TAB (PRD #270): the
          derived categoryCounts is the matrix indexed by the active bucket, so a chip on
          To triage reads that category's `todo` count and on All reads its `all` count. The
          count is a real server aggregate over the uncapped backlog, NOT a tally of the groups
          on screen: the on-screen groups are capped-before-grouping and bucket-filtered, so
          tallying them stays the anti-pattern the codebase forbids — a chip can honestly read 6
          while a truncated list shows 4 cards. */}
      <LabelFilter
        selected={categories}
        counts={categoryCounts}
        onToggle={toggleCategory}
        onClear={clearCategories}
      />

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

      {/* The bridge line reconciling the two count units on this page: the whole-backlog
          recommendation-ROW count for the active bucket against the whole-backlog deduped
          GROUP total. Suppressed when a category filter is active (the rec half is a
          whole-backlog count and can only honestly reconcile against a whole-backlog group
          total), when the backlog is truncated (the truncation Alert already flags the picture
          as incomplete), and when the group total is 0 (a failed/empty category-stats fetch,
          which would otherwise read "across 0 groups"). Also suppressed under a run anchor
          (`?run=`): the recommendation half (`triage`) is whole-account, computed with no run
          argument, while the group half is run-anchor scoped, so the two cannot honestly
          reconcile there either. The "Showing N groups" line below stays unchanged and covers
          the filtered/on-screen scope. Also gated on categoryStatsFresh: a disposition installs
          the new triage synchronously but refetches the matrix async, so without this the bridge
          would briefly reconcile the new recommendation count against the stale group total. */}
      {!loading && backlog && !showZeroState && !runAnchor && triage && categories.length === 0 && !backlog.truncated && categoryStatsFresh && bridgeGroupTotal > 0 && (
        <p className="text-sm text-muted">
          {judgeBridgeLine(bucketTabCount(triage, bucket), bridgeGroupTotal, bucket)}
        </p>
      )}

      {/* A plain view hint reading the length of the RETURNED (filtered) groups — open
          question 4's "result line", deliberately not a restyled triage strip. It reads the
          groups actually on screen, so it can never be mistaken for the canonical triage
          counts the tabs and badge show. */}
      {!loading && backlog && !showZeroState && (
        <p role="status" aria-live="polite" className="text-sm text-faint">
          {categories.length > 0 ? (
            <>
              Showing <b className="font-semibold text-muted tabular-nums">{backlog.groups.length}</b>{" "}
              {backlog.groups.length === 1 ? "group" : "groups"} matching{" "}
              {categories.map((c) => recommendationLabel(c)).join(", ")}
            </>
          ) : (
            <>
              Showing <b className="font-semibold text-muted tabular-nums">{backlog.groups.length}</b>{" "}
              {backlog.groups.length === 1 ? "group" : "groups"}
            </>
          )}
        </p>
      )}

      {loading && <ListSkeleton rows={5} />}

      {!loading && backlog && (
        <>
          {showZeroState ? (
            <ZeroState judgeEnabled={!!user?.judge_enabled} />
          ) : backlog.groups.length === 0 ? (
            <EmptyState
              icon={<ScaleIcon />}
              title={
                categories.length > 0
                  ? "No groups match these labels in this bucket"
                  : runAnchor
                    ? "No recommendations for this run"
                    : `Nothing under ${bucketTabLabel(bucket)}`
              }
              description={
                categories.length > 0
                  ? "Clear a label or pick another, or switch buckets to see more groups."
                  : runAnchor
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
                  onFiled={reloadAfterMutation}
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

// isBucket validates the ?bucket= search param, and it is THE ONE client-side bucket site
// TypeScript does not guard (PRD #98 review N7, found independently by both validators).
//
// Everywhere else the compiler does the work for free: those fields are typed as the literal
// union JudgeBacklogBucket, so a drifted spelling is a TS2367 "no overlap" error, and
// judgeBacklog.ts's Record<JudgeBacklogBucket, …> maps plus its exhaustive switch fail to
// compile if a rung is added or renamed. This function is different only because its input
// is `string | null` — a raw URL param — so every comparison is legal against any string and
// a drift here fails SILENTLY, rendering an empty list rather than erroring.
//
// Deriving the check from JUDGE_BUCKETS removes the hand-copied union, and as of PRD #98
// review N-a that array really does carry the guarantee. It did not before: the old
// `JudgeBacklogBucket[]` annotation checked only that every ELEMENT was a legal bucket, never
// that every bucket was an element, so a 5-element array stayed assignable when the union
// grew to 6 — measured, and the errors all landed in judgeBacklog.ts's Record maps, none at
// the array. It is now `as const satisfies` plus a type-level exhaustiveness assertion, so
// adding a rung to the union without listing it, or dropping one from the list, is a compile
// error AT THE ARRAY, and this validator follows automatically.
function isBucket(v: string | null): v is JudgeBacklogBucket {
  return v !== null && (JUDGE_BUCKETS as readonly string[]).includes(v);
}
