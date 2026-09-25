import type { RunContext } from "./executor.js";
import type { Milestone, MilestoneProgress } from "./protocol.js";

/**
 * PRD #1064 M1 (Decisions 1/2): build the per-turn progress observer the scan loop calls
 * on each `report_progress` observation. It closes over:
 *   - `lastObserved`, the diff base, seeded from the loop-scope `latestProgress` (the
 *     previous turn's final snapshot) and advanced across the turn's several observations,
 *     so an id newly `in_progress`/`completed` emits its transition frame exactly ONCE and
 *     a repeat emits nothing;
 *   - the frozen milestone titles, taken from the loop-scope frozen list (assigned at the
 *     gate) or, when that is undefined (a pre-approved resume), the claim's frozen list on
 *     `ctx.frozenMilestones`; a missing title degrades to a titleless frame, never a skip.
 * It emits the D2 frames and enqueues the immediate running push via `ctx.reportProgress`.
 */
export function makeProgressObserver(
  ctx: RunContext,
  seed: MilestoneProgress | undefined,
  frozen: Milestone[] | undefined,
): (progress: MilestoneProgress) => void {
  let lastObserved: MilestoneProgress | undefined = seed;
  const titles = new Map<string, string>();
  for (const m of frozen ?? ctx.frozenMilestones ?? []) titles.set(m.id, m.title);
  return (progress: MilestoneProgress): void => {
    const prevInProgress = new Set(lastObserved?.in_progress ?? []);
    const prevCompleted = new Set(lastObserved?.completed ?? []);
    // Emit completions before starts: when one observation both finishes a milestone and
    // starts the next, "m1 reported complete" then "m2 started" is the natural reading.
    for (const id of progress.completed) {
      if (!prevCompleted.has(id))
        emitTransitionFrame(ctx, id, "complete", titles);
    }
    for (const id of progress.in_progress) {
      if (!prevInProgress.has(id))
        emitTransitionFrame(ctx, id, "started", titles);
    }
    lastObserved = progress;
    // Fire-and-forget: the runner enqueues onto the per-run chain and returns at once, so
    // this never blocks the scan loop. The stub/test executors that do not wire it fall
    // back to the turn-boundary report, which still carries the folded latestProgress.
    // Swallow BOTH a synchronous throw and an async rejection — a progress push is
    // informational and must never fail the run (#122 additive-optional). The runner's own
    // implementation never rejects; this guards a mis-wired executor/test double.
    try {
      void Promise.resolve(ctx.reportProgress?.(progress)).catch(() => undefined);
    } catch {
      /* reportProgress threw synchronously — swallowed, see above */
    }
  };
}

/**
 * PRD #1064 M1 (Decision 2): one worker `status` feed frame per NEW milestone transition —
 * the worker's OWN words, not the raw signal tool call (which stays suppressed). Wording is
 * "reported complete", never "done"/"verified" (PRD #122 D6). A milestone with no known
 * title degrades to a titleless frame rather than being skipped (resumed-run fallback).
 */
function emitTransitionFrame(
  ctx: RunContext,
  id: string,
  kind: "started" | "complete",
  titles: Map<string, string>,
): void {
  const title = titles.get(id);
  const verb = kind === "started" ? "started" : "reported complete";
  const text =
    title !== undefined && title !== ""
      ? `milestone ${id} ${verb} — ${title}`
      : `milestone ${id} ${verb}`;
  ctx.emit({ kind: "status", agent: "worker", payload: { text } });
}
