// The Overview preview's issue-work line (PRD #3879): which station of the issue lifecycle
// each active run stands at, and the counts and captions the card prints. Pure; nothing
// here fetches or renders. Everything derives from the summary rows the Overview already
// loads (PRD #3878), and the park wording and the automatic-versus-owner split come from
// the existing seams (parkLabel, parkDisposition), so no second rule lives here.

import { isTerminalRun, type RunSummaryItem } from "./api";
import { openPrs, parkDisposition } from "./overview";
import { isAccountCooldownWaiting, isPlanningRun, isRevisingRun, parkLabel } from "./runBadge";
import { runKindLabel } from "./runKind";
import { stripUnsafeChars } from "./safeText";

export type IssueStage =
  | "queued"
  | "planning"
  | "review"
  | "building"
  | "asking"
  | "parked"
  | "other"
  | "pr"
  | "none";

// The stages that hold an active run (everything but the PR station and "none").
export type LineStage = Exclude<IssueStage, "pr" | "none">;

// Where one run stands. `openPrIds` is the set of run ids openPrs() returned for the
// viewer's own runs: a terminal run is "pr" only when it is in that set. Rules run in order,
// so a finished job or task never reaches "other", and a run in a status this list does not
// name falls through to "building" rather than dropping off the line.
export function issueLineStage(run: RunSummaryItem, openPrIds: ReadonlySet<string>): IssueStage {
  if (isTerminalRun(run.status)) return openPrIds.has(run.id) ? "pr" : "none";
  if (run.kind !== "issue") return "other";
  // Before the status rules: a cooldown run can be claimed, running or awaiting approval,
  // and the Overview counts it as waiting, not executing.
  if (isAccountCooldownWaiting(run)) return "parked";
  switch (run.status) {
    case "queued":
    case "claimed":
      return "queued";
    case "awaiting_approval":
      return isRevisingRun(run) ? "planning" : "review";
    case "awaiting_input":
    case "awaiting_followup":
      return "asking";
    case "limit_wait":
    case "pool_wait":
    case "recovery_wait":
    case "paused":
      return "parked";
  }
  return isPlanningRun(run) ? "planning" : "building";
}

export interface LineEntry {
  run: RunSummaryItem;
  // True for the viewer's own run; false for another user's run on an admin's line.
  own: boolean;
}

export interface IssueLineModel {
  // The active runs at each stage: the viewer's own first, then other users', input order.
  entries: Record<LineStage, LineEntry[]>;
  // The viewer's own open pull requests, one per (repo, PR). Never part of the active totals.
  prs: RunSummaryItem[];
  // Runs on the line: every stage except "other" and the PR station.
  onLine: number;
  captions: {
    queued: string;
    planning: string;
    review: string;
    building: string;
    askingTitle: string;
    asking: string;
    parked: string;
    // The second parked line (single run: its ref and reason; several: the distinct
    // reasons); null when nothing is parked.
    parkedReason: string | null;
    other: string;
    pr: string;
    prNote: string;
  };
}

function plural(n: number, one: string, many: string): string {
  return n === 1 ? one : many;
}

// A run's short reference: its issue number, or its kind label when it has none.
export function runRef(run: Pick<RunSummaryItem, "issue_iid" | "kind">): string {
  return run.issue_iid != null ? `#${run.issue_iid}` : stripUnsafeChars(runKindLabel(run.kind));
}

// Why a parked run waits, in the words the status pill uses.
function parkedReason(run: RunSummaryItem): string {
  return isAccountCooldownWaiting(run) ? "account cooldown" : stripUnsafeChars(parkLabel(run));
}

// "1 on you, 2 on others" for a wait that belongs partly to the viewer.
function split(own: number, total: number): string {
  return `${own} on you, ${total - own} on others`;
}

function gateCaption(list: LineEntry[]): string {
  const own = list.filter((e) => e.own).length;
  if (list.length === 0) return "Nothing waiting";
  if (own === list.length) return "Waiting on you";
  if (own === 0) return "Waiting on owners";
  return split(own, list.length);
}

function askingCaptions(list: LineEntry[]): { title: string; sub: string } {
  const own = list.filter((e) => e.own).length;
  const title = own === list.length ? "Asking you" : "Questions";
  if (list.length === 0) return { title, sub: "No open questions" };
  if (own === 0) return { title, sub: "Waiting on owners" };
  if (own < list.length) return { title, sub: split(own, list.length) };
  if (list.length === 1) return { title, sub: `${runRef(list[0].run)} paused on a question` };
  return { title, sub: `${list.length} questions waiting` };
}

function parkedCaptions(list: LineEntry[]): { sub: string; reason: string | null } {
  if (list.length === 0) return { sub: "Nothing parked", reason: null };
  const owner = list.filter((e) => parkDisposition(e.run) === "owner");
  const automatic = list.length - owner.length;
  let sub: string;
  if (owner.length === 0) {
    sub = "Resumes on its own";
  } else if (automatic === 0) {
    const own = owner.filter((e) => e.own).length;
    if (own === owner.length) sub = "Needs you";
    else if (own === 0) sub = "Needs the owner";
    else {
      const others = owner.length - own;
      sub = `${own} ${plural(own, "needs", "need")} you, ${others} ${plural(others, "needs", "need")} the owner`;
    }
  } else {
    sub = `${automatic} ${plural(automatic, "resumes on its own", "resume on their own")}, ${owner.length} ${plural(owner.length, "needs", "need")} action`;
  }
  if (list.length === 1) {
    return { sub, reason: `${runRef(list[0].run)} ${parkedReason(list[0].run)}` };
  }
  const distinct = [...new Set(list.map((e) => parkedReason(e.run)))];
  const shown = distinct.slice(0, 3).join(", ");
  return { sub, reason: distinct.length > 3 ? `${shown}, +${distinct.length - 3} more` : shown };
}

// `ownRuns` is the viewer's full own list (terminal rows included, for the PR station).
// `otherRuns` is other users' rows, already filtered by the caller to the Runs widget's
// "Other users" population (not the viewer's, not terminal); empty for a non-admin.
export function issueLineModel(input: {
  ownRuns: ReadonlyArray<RunSummaryItem>;
  otherRuns: ReadonlyArray<RunSummaryItem>;
}): IssueLineModel {
  const { ownRuns, otherRuns } = input;
  const prs = openPrs(ownRuns);
  const prIds = new Set(prs.map((r) => r.id));
  const ownIds = new Set(ownRuns.map((r) => r.id));
  const entries: Record<LineStage, LineEntry[]> = {
    queued: [],
    planning: [],
    review: [],
    building: [],
    asking: [],
    parked: [],
    other: [],
  };
  const place = (run: RunSummaryItem, own: boolean) => {
    const stage = issueLineStage(run, prIds);
    if (stage !== "pr" && stage !== "none") entries[stage].push({ run, own });
  };
  for (const run of ownRuns) place(run, true);
  for (const run of otherRuns) if (!ownIds.has(run.id)) place(run, false);

  const asking = askingCaptions(entries.asking);
  const parked = parkedCaptions(entries.parked);
  const onLine =
    entries.queued.length +
    entries.planning.length +
    entries.review.length +
    entries.building.length +
    entries.asking.length +
    entries.parked.length;
  return {
    entries,
    prs,
    onLine,
    captions: {
      queued: entries.queued.length ? "Claiming a slot" : "Queue empty",
      planning: entries.planning.length ? "Lead drafting a plan" : "Nothing planning",
      review: gateCaption(entries.review),
      building: entries.building.length ? "Implement, review, validate" : "Nothing building",
      askingTitle: asking.title,
      asking: asking.sub,
      parked: parked.sub,
      parkedReason: parked.reason,
      other: "Jobs, self-improve, CI fixes, reworks",
      pr: "PR state as last seen by uzi",
      prNote: "Not counted as active",
    },
  };
}
