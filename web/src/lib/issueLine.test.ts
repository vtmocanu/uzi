// PRD #3879 M1: where each run stands on the issue-work line, and the counts and captions
// the card prints.
import { readFileSync } from "node:fs";
import { describe, expect, it, vi } from "vitest";
import { aRun } from "../pages/overviewFixtures";
import { issueLineModel, issueLineStage, runRef, type IssueStage } from "./issueLine";
import { parkLabel } from "./runBadge";
import { runKindLabel } from "./runKind";

// parkLabel delegates to the real one; the sanitising test below swaps its answer.
vi.mock("./runBadge", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./runBadge")>();
  return { ...actual, parkLabel: vi.fn(actual.parkLabel) };
});

const none: ReadonlySet<string> = new Set();
const stage = (over: Parameters<typeof aRun>[0], pr: ReadonlySet<string> = none): IssueStage =>
  issueLineStage(aRun(over), pr);

const registry = JSON.parse(
  readFileSync(new URL("../../../fixtures/run-kinds/registry.json", import.meta.url), "utf8"),
) as { kinds: string[] };

describe("issueLineStage", () => {
  it("puts a terminal run at the PR station only when it is one of the open PRs, otherwise nowhere", () => {
    for (const status of ["completed", "failed", "cancelled"] as const) {
      expect(stage({ id: "t", status }, new Set(["t"]))).toBe("pr");
      expect(stage({ id: "t", status })).toBe("none");
    }
  });

  it("never lets a finished run of any kind reach Other work", () => {
    expect(registry.kinds).toContain("job");
    for (const kind of registry.kinds) {
      for (const status of ["completed", "failed", "cancelled"] as const) {
        const got = stage({ kind: kind as never, status });
        expect(["pr", "none"], `${kind} ${status}`).toContain(got);
      }
    }
  });

  it("sends every non-issue kind that is still active to Other work, ci_fix and mr_rework included", () => {
    for (const kind of ["job", "prompt", "task", "self_improve", "ci_fix", "mr_rework"] as const) {
      for (const status of ["queued", "running", "awaiting_approval", "awaiting_input", "paused", "limit_wait"] as const) {
        expect(stage({ kind, status }), `${kind} ${status}`).toBe("other");
      }
    }
  });

  it("parks an account cooldown at claimed, running or awaiting approval", () => {
    for (const status of ["claimed", "running", "awaiting_approval"] as const) {
      expect(stage({ status, health: "waiting_account_cooldown" }), status).toBe("parked");
    }
    // The cooldown health on a status it does not apply to changes nothing.
    expect(stage({ status: "queued", health: "waiting_account_cooldown" })).toBe("queued");
  });

  it("puts queued and claimed at Queued", () => {
    expect(stage({ status: "queued" })).toBe("queued");
    expect(stage({ status: "claimed" })).toBe("queued");
  });

  it("puts a planning run and a revising run at Planning", () => {
    expect(stage({ status: "running", is_planning: true })).toBe("planning");
    expect(stage({ status: "awaiting_approval", is_revising: true })).toBe("planning");
    // is_revising only counts at the gate, is_planning only while running.
    expect(stage({ status: "running", is_revising: true })).toBe("building");
    expect(stage({ status: "awaiting_approval", is_planning: true })).toBe("review");
  });

  it("puts a plan waiting for approval at the gate", () => {
    expect(stage({ status: "awaiting_approval" })).toBe("review");
  });

  it("puts a run waiting on a question or a follow-up at Asking", () => {
    expect(stage({ status: "awaiting_input" })).toBe("asking");
    expect(stage({ status: "awaiting_followup" })).toBe("asking");
  });

  it("parks limit, pool and recovery waits and a pause", () => {
    for (const status of ["limit_wait", "pool_wait", "recovery_wait", "paused"] as const) {
      expect(stage({ status }), status).toBe("parked");
    }
  });

  it("builds a running run, including an empty phase and a stalled one", () => {
    expect(stage({ status: "running" })).toBe("building");
    expect(stage({ status: "running", progress: { phase: "", state: "stalled" } as never })).toBe("building");
  });

  it("falls back to Building for a status it does not name, so a run never drops off the line", () => {
    expect(stage({ status: "some_future_status" as never })).toBe("building");
  });
});

describe("issueLineModel counts", () => {
  it("keeps open PRs out of the active totals and collapses a PR shared by two runs", () => {
    const pr = (id: string, finished: string) =>
      aRun({ id, status: "completed", mr_iid: 5, mr_state: "opened", repo_id: "r1", finished_at: finished });
    const model = issueLineModel({
      ownRuns: [pr("old", "2026-07-05T10:00:00Z"), pr("new", "2026-07-05T11:00:00Z"), aRun({ id: "run", status: "running" })],
      otherRuns: [],
    });
    expect(model.prs.map((r) => r.id)).toEqual(["new"]);
    expect(model.onLine).toBe(1);
    expect(model.entries.building).toHaveLength(1);
    expect(model.captions.pr).toBe("PR state as last seen by uzi");
    expect(model.captions.prNote).toBe("Not counted as active");
  });

  it("counts a PR for the viewer only, never another user's", () => {
    const theirs = aRun({ id: "x", status: "completed", mr_iid: 9, mr_state: "opened", owner_email: "mira@uzi.local" });
    expect(issueLineModel({ ownRuns: [], otherRuns: [theirs] }).prs).toEqual([]);
  });

  it("counts the line as every stage but Other work and PR open, and lists the viewer's runs first", () => {
    const model = issueLineModel({
      ownRuns: [aRun({ id: "q", status: "queued" }), aRun({ id: "o", kind: "job", status: "running" })],
      otherRuns: [aRun({ id: "xq", status: "queued", owner_email: "mira@uzi.local" })],
    });
    expect(model.entries.queued.map((e) => [e.run.id, e.own])).toEqual([["q", true], ["xq", false]]);
    expect(model.onLine).toBe(2);
    expect(model.entries.other).toHaveLength(1);
  });

  it("counts a run on the admin list that is also the viewer's own once", () => {
    const own = aRun({ id: "same", status: "queued" });
    const model = issueLineModel({ ownRuns: [own], otherRuns: [{ ...own, owner_email: "admin@uzi.local" }] });
    expect(model.entries.queued).toHaveLength(1);
    expect(model.entries.queued[0].own).toBe(true);
  });

  it("reads the gate as Nothing waiting when the only awaiting_approval run is revising", () => {
    const model = issueLineModel({
      ownRuns: [aRun({ status: "awaiting_approval", is_revising: true })],
      otherRuns: [],
    });
    expect(model.entries.planning).toHaveLength(1);
    expect(model.entries.review).toHaveLength(0);
    expect(model.captions.review).toBe("Nothing waiting");
  });

  it("uses the empty and live captions for queued, planning and building", () => {
    const empty = issueLineModel({ ownRuns: [], otherRuns: [] }).captions;
    expect([empty.queued, empty.planning, empty.building]).toEqual(["Queue empty", "Nothing planning", "Nothing building"]);
    const live = issueLineModel({
      ownRuns: [aRun({ id: "a", status: "queued" }), aRun({ id: "b", status: "running", is_planning: true }), aRun({ id: "c" })],
      otherRuns: [],
    }).captions;
    expect([live.queued, live.planning, live.building]).toEqual([
      "Claiming a slot",
      "Lead drafting a plan",
      "Implement, review, validate",
    ]);
  });
});

describe("issueLineModel whose-wait captions", () => {
  const plan = (id: string, owner?: string) => aRun({ id, status: "awaiting_approval", ...(owner ? { owner_email: owner } : {}) });
  const ask = (id: string, over = {}) => aRun({ id, status: "awaiting_input", ...over });
  const theirs = (r: ReturnType<typeof aRun>) => ({ ...r, owner_email: "mira@uzi.local" });

  it("reads the gate by whose plans wait", () => {
    expect(issueLineModel({ ownRuns: [], otherRuns: [] }).captions.review).toBe("Nothing waiting");
    expect(issueLineModel({ ownRuns: [plan("a"), plan("b")], otherRuns: [] }).captions.review).toBe("Waiting on you");
    expect(issueLineModel({ ownRuns: [], otherRuns: [plan("a", "mira@uzi.local")] }).captions.review).toBe("Waiting on owners");
    expect(
      issueLineModel({ ownRuns: [plan("a")], otherRuns: [plan("b", "m@uzi.local"), plan("c", "n@uzi.local")] }).captions.review,
    ).toBe("1 on you, 2 on others");
  });

  it("names the Asking bay and its line by whose questions wait", () => {
    const none_ = issueLineModel({ ownRuns: [], otherRuns: [] }).captions;
    expect([none_.askingTitle, none_.asking]).toEqual(["Asking you", "No open questions"]);

    const one = issueLineModel({ ownRuns: [ask("a", { issue_iid: 409 })], otherRuns: [] }).captions;
    expect([one.askingTitle, one.asking]).toEqual(["Asking you", "#409 paused on a question"]);

    const several = issueLineModel({ ownRuns: [ask("a"), ask("b"), ask("c")], otherRuns: [] }).captions;
    expect([several.askingTitle, several.asking]).toEqual(["Asking you", "3 questions waiting"]);

    const others = issueLineModel({ ownRuns: [], otherRuns: [theirs(ask("x"))] }).captions;
    expect([others.askingTitle, others.asking]).toEqual(["Questions", "Waiting on owners"]);

    const mixed = issueLineModel({ ownRuns: [ask("a")], otherRuns: [theirs(ask("x")), theirs(ask("y"))] }).captions;
    expect([mixed.askingTitle, mixed.asking]).toEqual(["Questions", "1 on you, 2 on others"]);
  });
});

describe("issueLineModel parked captions", () => {
  const parked = (over: Parameters<typeof aRun>[0]) => aRun({ id: "p", status: "limit_wait", issue_iid: 94, ...over });
  const captions = (...runs: ReturnType<typeof aRun>[]) => issueLineModel({ ownRuns: runs, otherRuns: [] }).captions;

  it("reads Nothing parked with no reason line", () => {
    const c = captions();
    expect([c.parked, c.parkedReason]).toEqual(["Nothing parked", null]);
  });

  it("names a usage-limit park in the pill's words, and says it resumes on its own", () => {
    const run = parked({});
    const c = captions(run);
    expect(parkLabel(run)).toBe("limit wait");
    expect(c.parked).toBe("Resumes on its own");
    expect(c.parkedReason).toBe(`#94 ${parkLabel(run)}`);
  });

  it("reads a weekly-reserve pause as automatic, with the reserve label", () => {
    const run = parked({ status: "paused", hold_reason: "weekly_reserve" });
    const c = captions(run);
    expect(c.parked).toBe("Resumes on its own");
    expect(c.parkedReason).toBe(`#94 ${parkLabel(run)}`);
    expect(parkLabel(run)).toMatch(/reserve/);
  });

  it("reads a plain pause as needing the owner", () => {
    const run = parked({ status: "paused" });
    const own = captions(run);
    expect(own.parked).toBe("Needs you");
    expect(own.parkedReason).toBe(`#94 ${parkLabel(run)}`);
    const theirs = issueLineModel({ ownRuns: [], otherRuns: [{ ...run, owner_email: "mira@uzi.local" }] }).captions;
    expect(theirs.parked).toBe("Needs the owner");
  });

  it("reads a recovery wait by its retry stamp and cause", () => {
    const retrying = parked({ status: "recovery_wait", recovery_retry_not_before: "2026-07-05T13:00:00Z" });
    expect(captions(retrying).parked).toBe("Resumes on its own");
    expect(captions(retrying).parkedReason).toBe(`#94 ${parkLabel(retrying)}`);
    const exhausted = parked({ status: "recovery_wait", recovery_wait_cause: "worker_requeue_exhausted" });
    expect(captions(exhausted).parked).toBe("Needs you");
    expect(captions(exhausted).parkedReason).toBe(`#94 ${parkLabel(exhausted)}`);
  });

  it("reads an account cooldown as automatic with its own reason", () => {
    const c = captions(parked({ status: "running", health: "waiting_account_cooldown" }));
    expect([c.parked, c.parkedReason]).toEqual(["Resumes on its own", "#94 account cooldown"]);
  });

  it("splits a mixed park between automatic and owner", () => {
    const c = captions(parked({ id: "a" }), parked({ id: "b", status: "paused" }));
    expect(c.parked).toBe("1 resumes on its own, 1 needs action");
    const many = captions(parked({ id: "a" }), parked({ id: "b" }), parked({ id: "c", status: "paused" }), parked({ id: "d", status: "paused" }));
    expect(many.parked).toBe("2 resume on their own, 2 need action");
  });

  it("lists the distinct reasons of several parked runs, three at most, then +N more", () => {
    const three = captions(
      parked({ id: "a" }),
      parked({ id: "b" }),
      parked({ id: "c", status: "pool_wait" }),
      parked({ id: "d", status: "paused" }),
    );
    expect(three.parkedReason).toBe("limit wait, waiting for pool, paused");
    const more = captions(
      parked({ id: "a" }),
      parked({ id: "b", status: "pool_wait" }),
      parked({ id: "c", status: "paused" }),
      parked({ id: "d", status: "recovery_wait" }),
      parked({ id: "e", status: "running", health: "waiting_account_cooldown" }),
    );
    expect(more.parkedReason).toBe("limit wait, waiting for pool, paused, +2 more");
  });
});

describe("runRef", () => {
  it("is the issue number, or the kind label when the run has no issue", () => {
    expect(runRef({ kind: "issue", issue_iid: 12 })).toBe("#12");
    expect(runRef({ kind: "job", issue_iid: null })).toBe(runKindLabel("job"));
    expect(runRef({ kind: "job", issue_iid: null })).not.toMatch(/^#/);
  });
});

describe("issueLineModel untrusted text", () => {
  it("strips control and bidi characters from the parked reason line", async () => {
    const real = await vi.importActual<typeof import("./runBadge")>("./runBadge");
    vi.mocked(parkLabel).mockImplementation(() => "evil\u202E reason\u0007");
    try {
      const run = aRun({ id: "p", status: "limit_wait", issue_iid: 94 });
      const one = issueLineModel({ ownRuns: [run], otherRuns: [] }).captions.parkedReason;
      expect(one).toBe("#94 evil reason");
      const two = issueLineModel({ ownRuns: [run, { ...run, id: "q" }], otherRuns: [] }).captions.parkedReason;
      expect(two).toBe("evil reason");
    } finally {
      vi.mocked(parkLabel).mockImplementation(real.parkLabel);
    }
  });
});
