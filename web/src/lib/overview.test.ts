import { describe, expect, it } from "vitest";
import type { RecoveryCustodyHold, RecoveryCustodyHolds, RunStatus, RunSummaryItem } from "./api";
import {
  decisionCount,
  decisionRows,
  GROUP_BUTTON,
  GROUP_PREVIEW,
  groupTitle,
  limitCredentialNoun,
  mergeOverviewRows,
  planDetail,
  openPrs,
  overviewCounts,
  overviewDecisions,
  parkDisposition,
  ownRecoveryWaitCount,
  setupComplete,
  runTotalsLine,
  workerJoined,
} from "./overview";

function run(over: Partial<RunSummaryItem> & { status: RunStatus }): RunSummaryItem {
  return { id: `r-${Math.random()}`, health: "ok", ...over } as RunSummaryItem;
}

describe("overviewCounts", () => {
  it("buckets non-terminal runs on the raw status: running executes, queued and claimed queue, the other waits park", () => {
    const counts = overviewCounts([
      run({ status: "running" }),
      run({ status: "running" }),
      run({ status: "queued" }),
      run({ status: "claimed" }),
      run({ status: "limit_wait" }),
      run({ status: "pool_wait" }),
      run({ status: "recovery_wait" }),
      run({ status: "paused" }),
    ]);
    expect(counts).toEqual({ executing: 2, queued: 2, parked: 4 });
  });

  it("counts an account cooldown as parked, not executing", () => {
    const counts = overviewCounts([
      run({ status: "running", health: "waiting_account_cooldown" }),
      run({ status: "running" }),
    ]);
    expect(counts).toEqual({ executing: 1, queued: 0, parked: 1 });
  });

  it("leaves runs that need the owner out of every bucket", () => {
    const counts = overviewCounts([
      run({ status: "awaiting_approval" }),
      run({ status: "awaiting_input" }),
      run({ status: "awaiting_followup" }),
    ]);
    expect(counts).toEqual({ executing: 0, queued: 0, parked: 0 });
  });

  it("ignores terminal runs", () => {
    const counts = overviewCounts([
      run({ status: "completed" }),
      run({ status: "failed" }),
      run({ status: "cancelled" }),
      run({ status: "running" }),
    ]);
    expect(counts).toEqual({ executing: 1, queued: 0, parked: 0 });
  });
});

describe("mergeOverviewRows", () => {
  const mine = run({ id: "a", status: "running", owner_email: "me@x.io" });
  const mineAgain = run({ id: "b", status: "queued" });

  it("adds only the admin rows another user owns, keeping the viewer's own rows first", () => {
    const theirs = run({ id: "c", status: "running", owner_email: "them@x.io" });
    const ownCopy = run({ id: "a", status: "running", owner_email: "me@x.io" });
    const otherCopyOfMine = run({ id: "z", status: "running", owner_email: "me@x.io" });
    const merged = mergeOverviewRows([mine, mineAgain], [ownCopy, theirs, otherCopyOfMine], "me@x.io");
    expect(merged.map((r) => r.id)).toEqual(["a", "b", "c"]);
  });

  it("dedupes by run id with the viewer's own row winning, even when the admin copy names another owner", () => {
    const conflicting = { ...mine, owner_email: "them@x.io", status: "queued" } as RunSummaryItem;
    const merged = mergeOverviewRows([mine], [conflicting, conflicting], "me@x.io");
    expect(merged).toEqual([mine]);
  });
});

describe("ownRecoveryWaitCount", () => {
  it("counts only recovery_wait rows", () => {
    expect(
      ownRecoveryWaitCount([
        run({ status: "recovery_wait" }),
        run({ status: "recovery_wait" }),
        run({ status: "limit_wait" }),
        run({ status: "running" }),
      ]),
    ).toBe(2);
    expect(ownRecoveryWaitCount([])).toBe(0);
  });
});

describe("runTotalsLine", () => {
  const none = { executing: 0, queued: 0, parked: 0 };
  it("omits an all-zero line", () => {
    expect(runTotalsLine({ counts: none, acrossFactory: false })).toBeNull();
  });

  it("lists each nonzero all-kind bucket in order", () => {
    expect(runTotalsLine({ counts: { executing: 3, queued: 1, parked: 2 }, acrossFactory: false })).toBe(
      "3 executing · 1 queued · 2 parked",
    );
    expect(runTotalsLine({ counts: { executing: 0, queued: 1, parked: 0 }, acrossFactory: false })).toBe("1 queued");
  });

  it("adds people only to factory executing runs", () => {
    const counts = { executing: 6, queued: 1, parked: 0 };
    expect(runTotalsLine({ counts, acrossFactory: true, executingPeople: 3 })).toBe("6 executing for 3 people · 1 queued");
    expect(runTotalsLine({ counts, acrossFactory: false, executingPeople: 3 })).toBe("6 executing · 1 queued");
    expect(runTotalsLine({ counts: { ...counts, executing: 0 }, acrossFactory: true, executingPeople: 3 })).toBe("1 queued");
  });
});

describe("setup helpers", () => {
  it("treats a worker that ever registered as joined, even offline", () => {
    expect(workerJoined([{ status: "offline", last_heartbeat_at: "2026-07-05T12:00:00Z" }])).toBe(true);
    expect(workerJoined([{ status: "online", last_heartbeat_at: null }])).toBe(true);
    expect(workerJoined([{ status: "offline", last_heartbeat_at: null }])).toBe(false);
    expect(workerJoined([])).toBe(false);
  });

  it("is complete only when every step is done", () => {
    const done = { hasToken: true, hasForge: true, reposEnabled: 1 };
    expect(setupComplete(done, true)).toBe(true);
    expect(setupComplete(done, false)).toBe(false);
    expect(setupComplete({ ...done, reposEnabled: 0 }, true)).toBe(false);
    expect(setupComplete({ ...done, hasToken: false }, true)).toBe(false);
    expect(setupComplete({ ...done, hasForge: false }, true)).toBe(false);
  });
});

const STAMP = "2026-07-05T12:30:00Z";

describe("parkDisposition", () => {
  const table: Array<[string, Partial<RunSummaryItem> & { status: RunStatus }, "automatic" | "owner"]> = [
    ["limit_wait", { status: "limit_wait" }, "automatic"],
    ["pool_wait", { status: "pool_wait" }, "automatic"],
    ["paused (owner pause)", { status: "paused" }, "owner"],
    ["paused budget_exhausted", { status: "paused", hold_reason: "budget_exhausted" }, "owner"],
    ["codex_policy_hold", { status: "paused", hold_reason: "codex_policy_hold" }, "owner"],
    ["iteration_budget_exhausted", { status: "paused", hold_reason: "iteration_budget_exhausted" }, "owner"],
    ["provider_unavailable", { status: "paused", hold_reason: "provider_unavailable" }, "owner"],
    ["base_attestation_missing", { status: "paused", hold_reason: "base_attestation_missing" }, "owner"],
    ["credential_disabled", { status: "paused", hold_reason: "credential_disabled" }, "owner"],
    ["credentials_reset", { status: "paused", hold_reason: "credentials_reset" }, "owner"],
    ["completion_blocked", { status: "paused", completion_phase: "blocked" }, "owner"],
    ["worker_requeue_exhausted", { status: "recovery_wait", recovery_wait_cause: "worker_requeue_exhausted" }, "owner"],
    ["worker_memory_pressure", { status: "recovery_wait", recovery_wait_cause: "worker_memory_pressure" }, "owner"],
    ["anthropic_spend_limit", { status: "recovery_wait", recovery_wait_cause: "anthropic_spend_limit", recovery_retry_not_before: null }, "owner"],
    [
      "retained_recovery_adoption_failed even with a stamp",
      { status: "recovery_wait", recovery_wait_cause: "retained_recovery_adoption_failed", recovery_retry_not_before: STAMP },
      "owner",
    ],
    ["weekly reserve pause", { status: "paused", hold_reason: "weekly_reserve" }, "automatic"],
    ["account cooldown", { status: "running", health: "waiting_account_cooldown" }, "automatic"],
  ];
  it.each(table)("%s", (_name, over, want) => {
    expect(parkDisposition(run(over))).toBe(want);
  });

  it("covers the credential-switch holds as owner", () => {
    for (const hold of ["settling", "unsettled"]) {
      const r = run({ status: "paused", hold_reason: `credential_switch_${hold}` });
      expect(parkDisposition(r)).toBe("owner");
    }
  });

  it.each(["retained_recovery_exhausted", "codex_idle_stall"])("%s: null owner, valid stamp automatic, absent or garbage owner", (cause) => {
    const base = { status: "recovery_wait" as const, recovery_wait_cause: cause };
    expect(parkDisposition(run({ ...base, recovery_retry_not_before: null }))).toBe("owner");
    expect(parkDisposition(run({ ...base, recovery_retry_not_before: STAMP }))).toBe("automatic");
    expect(parkDisposition(run(base))).toBe("owner");
    expect(parkDisposition(run({ ...base, recovery_retry_not_before: "garbage" }))).toBe("owner");
  });

  it.each(["forge_unreachable", "vault_locked", "codex_account_unavailable", null, "provider_outage"])("plain recovery_wait cause %s needs a scheduled retry", (cause) => {
    const base = { status: "recovery_wait" as const, recovery_wait_cause: cause };
    expect(parkDisposition(run({ ...base, recovery_retry_not_before: STAMP }))).toBe("automatic");
    expect(parkDisposition(run({ ...base, recovery_retry_not_before: null }))).toBe("owner");
    expect(parkDisposition(run(base))).toBe("owner");
    expect(parkDisposition(run({ ...base, recovery_retry_not_before: "garbage" }))).toBe("owner");
  });

  it("maps an unknown future status to owner", () => {
    expect(parkDisposition({ status: "some_future_park" })).toBe("owner");
  });
});

describe("openPrs", () => {
  const pr = (over: Partial<RunSummaryItem>) =>
    run({ status: "completed", kind: "issue", mr_iid: 5, mr_state: "opened", repo_id: "repo-1", created_at: "2026-07-01T00:00:00Z", ...over });

  it("keeps only completed issue runs with an open MR", () => {
    const kept = pr({ id: "ok" });
    const got = openPrs([
      kept,
      pr({ id: "failed", status: "failed", mr_iid: 6 }),
      pr({ id: "merged", mr_state: "merged", mr_iid: 7 }),
      pr({ id: "rework", kind: "mr_rework", mr_iid: 8 }),
      pr({ id: "nomr", mr_iid: null }),
    ]);
    expect(got.map((r) => r.id)).toEqual(["ok"]);
  });

  it("dedupes by repo and MR keeping the newest, and keeps the same MR number in another repo", () => {
    const got = openPrs([
      pr({ id: "old", finished_at: "2026-07-02T00:00:00Z" }),
      pr({ id: "new", finished_at: "2026-07-03T00:00:00Z" }),
      pr({ id: "other-repo", repo_id: "repo-2" }),
    ]);
    expect(got.map((r) => r.id).sort()).toEqual(["new", "other-repo"]);
  });

  // #4167: the MR watcher refreshes only the newest run, so an older run's stale "opened"
  // must not outlive a newer run that saw the merge.
  it("reads the MR state from the newest run, so a stale older opened run does not count", () => {
    const got = openPrs([
      pr({ id: "old", mr_iid: 4100, mr_state: "opened", finished_at: "2026-11-08T00:00:00Z" }),
      pr({ id: "new", mr_iid: 4100, mr_state: "merged", finished_at: "2026-11-09T00:00:00Z" }),
    ]);
    expect(got).toEqual([]);
  });

  it("keeps the PR when the newest run says opened over an older merged run", () => {
    const got = openPrs([
      pr({ id: "old", mr_iid: 4100, mr_state: "merged", finished_at: "2026-11-08T00:00:00Z" }),
      pr({ id: "new", mr_iid: 4100, mr_state: "opened", finished_at: "2026-11-09T00:00:00Z" }),
    ]);
    expect(got.map((r) => r.id)).toEqual(["new"]);
  });

  it("hides the PR while the newest run's MR state is not yet observed", () => {
    const got = openPrs([
      pr({ id: "old", mr_iid: 4100, mr_state: "opened", finished_at: "2026-11-08T00:00:00Z" }),
      pr({ id: "new", mr_iid: 4100, mr_state: null, finished_at: "2026-11-09T00:00:00Z" }),
    ]);
    expect(got).toEqual([]);
  });
});

describe("overviewDecisions stale PR state (#4167)", () => {
  it("offers no Review PR item when a newer run on the same PR saw it merged", () => {
    const got = overviewDecisions([
      run({ id: "old", status: "completed", kind: "issue", repo_id: "repo-1", mr_iid: 4100, mr_state: "opened", finished_at: "2026-11-08T00:00:00Z" }),
      run({ id: "new", status: "completed", kind: "issue", repo_id: "repo-1", mr_iid: 4100, mr_state: "merged", finished_at: "2026-11-09T00:00:00Z" }),
    ]);
    expect(got.filter((d) => d.kind === "pr")).toEqual([]);
  });
});

describe("overviewDecisions", () => {
  const at = (m: number) => `2026-07-05T12:${String(m).padStart(2, "0")}:00Z`;
  // Run-only fixtures: narrow a decision to its run item.
  const runOf = (d: ReturnType<typeof overviewDecisions>[number]) => {
    if (d.kind === "held") throw new Error("unexpected held item");
    return d.run;
  };

  // PRD #4040: the server's pending-question excerpt rides only awaiting_input question items.
  it("carries the pending-question excerpt on awaiting_input items only", () => {
    const got = overviewDecisions([
      run({ id: "ask", status: "awaiting_input", status_since: at(1), pending_question: "Keep the cap?" }),
      run({ id: "follow", status: "awaiting_followup", status_since: at(2), pending_question: "Not shown" }),
      run({ id: "missing", status: "awaiting_input", status_since: at(3) }),
      run({ id: "empty", status: "awaiting_input", status_since: at(4), pending_question: "" }),
      run({ id: "plan", status: "awaiting_approval", status_since: at(5), pending_question: "Not shown" }),
    ]);
    const byId = new Map(got.map((d) => [runOf(d).id, d]));
    expect(byId.get("ask")).toMatchObject({ kind: "question", excerpt: "Keep the cap?" });
    for (const id of ["follow", "missing", "empty", "plan"]) {
      expect(byId.get(id), id).toBeDefined();
      expect(Object.prototype.hasOwnProperty.call(byId.get(id), "excerpt"), id).toBe(false);
    }
    expect(byId.get("follow")?.kind).toBe("question");
  });

  it("excludes revising plans, stalled running runs and the weekly-reserve pause; includes owner parks", () => {
    const got = overviewDecisions([
      run({ id: "revising", status: "awaiting_approval", is_revising: true }),
      run({ id: "stalled", status: "running", progress: { state: "stalled" } as never }),
      run({ id: "reserve", status: "paused", hold_reason: "weekly_reserve" }),
      run({ id: "limit", status: "limit_wait" }),
      run({ id: "owner-park", status: "paused" }),
    ]);
    expect(got.map((d) => [runOf(d).id, d.kind, d.verb])).toEqual([["owner-park", "park", "Resume"]]);
  });

  it("orders by group then oldest first, with the verb per group", () => {
    const got = overviewDecisions([
      run({ id: "pr", status: "completed", kind: "issue", mr_iid: 1, mr_state: "opened", created_at: at(1), finished_at: at(2) }),
      run({ id: "park", status: "paused", status_since: at(3) }),
      run({ id: "q-new", status: "awaiting_input", status_since: at(9) }),
      run({ id: "q-old", status: "awaiting_followup", status_since: at(4) }),
      run({ id: "plan-new", status: "awaiting_approval", status_since: at(8) }),
      run({ id: "plan-old", status: "awaiting_approval", status_since: null, created_at: at(5) }),
    ]);
    expect(got.map((d) => [runOf(d).id, d.verb, d.since])).toEqual([
      ["plan-old", "Review plan", at(5)],
      ["plan-new", "Review plan", at(8)],
      ["q-old", "Answer", at(4)],
      ["q-new", "Answer", at(9)],
      ["park", "Resume", at(3)],
      ["pr", "Review PR", at(2)],
    ]);
  });

  it("puts held work first and open PRs last, oldest first inside a kind (#4041)", () => {
    const got = overviewDecisions(
      [
        run({ id: "pr-new", status: "completed", kind: "issue", mr_iid: 2, mr_state: "opened", repo_id: "r", created_at: at(1), finished_at: at(7) }),
        run({ id: "pr-old", status: "completed", kind: "issue", mr_iid: 1, mr_state: "opened", repo_id: "r", created_at: at(1), finished_at: at(2) }),
        run({ id: "plan", status: "awaiting_approval", status_since: at(3) }),
      ],
      heldHolds({ open_holds: 1, decision_needed: 1 }, [decisionHold("h", at(0))]),
    );
    expect(got.map((d) => (d.kind === "held" ? "held" : runOf(d).id))).toEqual(["held", "plan", "pr-old", "pr-new"]);
  });

  it("orders unparseable timestamps deterministically: last, in input order", () => {
    const got = overviewDecisions([
      run({ id: "bad-1", status: "awaiting_approval", status_since: "garbage" }),
      run({ id: "bad-2", status: "awaiting_approval", status_since: "nope" }),
      run({ id: "good", status: "awaiting_approval", status_since: at(5) }),
    ]);
    expect(got.map((d) => runOf(d).id)).toEqual(["good", "bad-1", "bad-2"]);
  });
});

describe("decisionCount from Overview decisions", () => {
  const countFor = (runs: RunSummaryItem[], holds?: RecoveryCustodyHolds) => decisionCount(overviewDecisions(runs, holds));
  const plan = run({ id: "p", status: "awaiting_approval" });
  const pr = (id: string) =>
    run({ id, status: "completed", kind: "issue", mr_iid: 9, mr_state: "opened", repo_id: "r", created_at: "2026-07-01T00:00:00Z" });

  it("counts owner decisions but excludes PRs", () => {
    expect(countFor([plan, pr("a")])).toBe(1);
    expect(countFor([plan])).toBe(1);
    expect(countFor([])).toBe(0);
  });

  it("counts two runs sharing one PR once", () => {
    expect(overviewDecisions([pr("a"), pr("b")]).filter((d) => d.kind === "pr")).toHaveLength(1);
  });

  it("counts the aggregate's held-work decisions", () => {
    const holds = heldHolds({ decision_needed: 2, open_holds: 2 }, [decisionHold("h1", "2026-07-05T10:01:00Z"), decisionHold("h2", "2026-07-05T10:02:00Z")]);
    expect(countFor([], holds)).toBe(2);
    expect(countFor([plan], holds)).toBe(3);
  });
});

function decisionHold(runId: string, createdAt: string, over: Partial<RecoveryCustodyHold> = {}): RecoveryCustodyHold {
  return {
    id: `hold-${runId}`,
    run_id: runId,
    generation: 1,
    state: "open",
    attention: "source_only",
    worker_id: "w",
    inventory_guarded: false,
    has_available_capture: false,
    created_at: createdAt,
    updated_at: createdAt,
    ...over,
  };
}

function heldHolds(
  agg: Partial<RecoveryCustodyHolds["aggregate"]>,
  holds: RecoveryCustodyHold[] = [],
): RecoveryCustodyHolds {
  return {
    aggregate: { open_holds: 0, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0, ...agg },
    holds,
  };
}

describe("overviewDecisions held work", () => {
  const T1 = "2026-07-05T10:00:00Z";
  const T2 = "2026-07-05T11:00:00Z";
  const held = (holds: RecoveryCustodyHolds) => overviewDecisions([], holds).filter((d) => d.kind === "held");

  it("adds no row when the aggregate has no decision, even with blocked runs", () => {
    expect(held(heldHolds({ open_holds: 3, decision_needed: 0, blocked_runs: 2 }))).toEqual([]);
  });

  it("adds no row when no hold is open, even if decision_needed is positive", () => {
    expect(held(heldHolds({ open_holds: 0, decision_needed: 2 }, [decisionHold("a", T1)]))).toEqual([]);
  });

  it("adds no row without holds", () => {
    expect(overviewDecisions([])).toEqual([]);
    expect(overviewDecisions([], null)).toEqual([]);
  });

  it("collapses several decision holds into one row with the oldest age and no run", () => {
    const got = held(heldHolds({ open_holds: 2, decision_needed: 2 }, [decisionHold("newer", T2), decisionHold("older", T1)]));
    expect(got).toEqual([{ kind: "held", verb: "Review held work", count: 2, runId: null, since: T1, label: "Decision required" }]);
  });

  it("names the run of a single decision hold", () => {
    const got = held(
      heldHolds({ open_holds: 2, decision_needed: 1 }, [
        decisionHold("only", T2),
        decisionHold("healthy", T1, { attention: "active" }),
      ]),
    );
    expect(got).toEqual([{ kind: "held", verb: "Review held work", count: 1, runId: "only", since: T2, label: "Decision required" }]);
  });

  it("stays generic when the aggregate says 1 but the list has no decision hold", () => {
    const got = held(heldHolds({ open_holds: 1, decision_needed: 1 }, [decisionHold("x", T1, { attention: "active" })]));
    expect(got).toEqual([{ kind: "held", verb: "Review held work", count: 1, runId: null, since: null, label: null }]);
  });

  it("ignores released holds when picking the row's run and age", () => {
    const got = held(
      heldHolds({ open_holds: 1, decision_needed: 1 }, [decisionHold("gone", T1, { state: "released" }), decisionHold("live", T2)]),
    );
    expect(got).toMatchObject([{ runId: "live", since: T2 }]);
  });

  it("labels the row with the oldest decision hold's state", () => {
    const got = held(
      heldHolds({ open_holds: 2, decision_needed: 2 }, [
        decisionHold("newer", T2, { attention: "source_only" }),
        decisionHold("older", T1, { attention: "needs_action" }),
      ]),
    );
    expect(got).toMatchObject([{ label: "Needs attention" }]);
  });

  it("sorts an unparseable created_at after a parseable one", () => {
    const got = held(heldHolds({ open_holds: 2, decision_needed: 2 }, [decisionHold("bad", "garbage"), decisionHold("ok", T2)]));
    expect(got).toMatchObject([{ since: T2 }]);
  });

  it("places the held row first, before plan, park and pr", () => {
    const got = overviewDecisions(
      [
        run({ id: "pr", status: "completed", kind: "issue", mr_iid: 1, mr_state: "opened", created_at: T1, finished_at: T1 }),
        run({ id: "park", status: "paused" }),
        run({ id: "plan", status: "awaiting_approval" }),
      ],
      heldHolds({ open_holds: 1, decision_needed: 1 }, [decisionHold("h", T1)]),
    );
    expect(got.map((d) => d.kind)).toEqual(["held", "plan", "park", "pr"]);
  });
});

describe("decisionRows", () => {
  const T = "2026-07-05T10:00:00Z";
  const prRun = (id: string, iid: number) =>
    run({ id, status: "completed", kind: "issue", mr_iid: iid, mr_state: "opened", repo_id: "r", created_at: T });
  const holds = heldHolds({ open_holds: 1, decision_needed: 1 }, [decisionHold("h", T)]);
  const rowsOf = (runs: RunSummaryItem[], h?: RecoveryCustodyHolds) => decisionRows(overviewDecisions(runs, h));

  it("keeps a lone plan, question or park as a single row", () => {
    for (const r of [
      run({ status: "awaiting_approval" }),
      run({ status: "awaiting_input" }),
      run({ status: "paused" }),
    ]) {
      const rows = rowsOf([r]);
      expect(rows).toHaveLength(1);
      expect(rows[0].type).toBe("single");
    }
  });

  it("groups two plans", () => {
    const rows = rowsOf([run({ id: "a", status: "awaiting_approval" }), run({ id: "b", status: "awaiting_approval" })]);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ type: "group", kind: "plan" });
    expect(rows[0].type === "group" && rows[0].items).toHaveLength(2);
  });

  it("groups awaiting_input and awaiting_followup into one question group, both verbed Answer", () => {
    const rows = rowsOf([run({ id: "a", status: "awaiting_input" }), run({ id: "b", status: "awaiting_followup" })]);
    expect(rows).toHaveLength(1);
    const row = rows[0];
    if (row.type !== "group") throw new Error("expected a group");
    expect(row.kind).toBe("question");
    expect(row.items.map((i) => i.verb)).toEqual(["Answer", "Answer"]);
  });

  it("always groups open PRs, even a single one", () => {
    const rows = rowsOf([prRun("a", 1)]);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ type: "group", kind: "pr" });
  });

  it("turns held work into a held row", () => {
    const rows = rowsOf([], holds);
    expect(rows).toHaveLength(1);
    expect(rows[0].type).toBe("held");
  });

  it("orders rows held, plan, question, park, pr", () => {
    const rows = rowsOf(
      [
        prRun("pr", 1),
        run({ id: "park", status: "paused" }),
        run({ id: "q", status: "awaiting_input" }),
        run({ id: "plan", status: "awaiting_approval" }),
      ],
      holds,
    );
    expect(rows.map((r) => (r.type === "group" ? `group:${r.kind}` : r.type === "single" ? r.item.kind : r.type))).toEqual([
      "held",
      "plan",
      "question",
      "park",
      "group:pr",
    ]);
  });

  it("does not change decisionCount: it counts items, not rows", () => {
    const items = overviewDecisions([
      run({ status: "awaiting_approval" }),
      run({ status: "awaiting_approval" }),
      run({ status: "awaiting_input" }),
      prRun("pr", 1),
    ]);
    expect(decisionRows(items)).toHaveLength(3);
    expect(decisionCount(items)).toBe(3);
  });
});

describe("groupTitle and GROUP_BUTTON", () => {
  it("reads singular for one and plural otherwise, per kind", () => {
    expect(groupTitle("plan", 1)).toBe("1 plan is ready for review");
    expect(groupTitle("plan", 3)).toBe("3 plans are ready for review");
    expect(groupTitle("question", 1)).toBe("1 run needs your input");
    expect(groupTitle("question", 2)).toBe("2 runs need your input");
    expect(groupTitle("park", 1)).toBe("1 run waits on you");
    expect(groupTitle("park", 4)).toBe("4 runs wait on you");
    expect(groupTitle("limit", 1)).toBe("1 run on it has a fix you can apply");
    expect(groupTitle("limit", 2)).toBe("2 runs on it have a fix you can apply");
    expect(groupTitle("pr", 1)).toBe("1 pull request is open for your review");
    expect(groupTitle("pr", 5)).toBe("5 pull requests are open for your review");
  });

  it("names the group buttons and the preview size", () => {
    expect(GROUP_BUTTON).toEqual({ plan: "Review plans", question: "Open runs", park: "Resume runs", limit: "Show runs", pr: "Review PRs" });
    expect(GROUP_PREVIEW).toBe(3);
  });
});

describe("planDetail", () => {
  const ms = (n: number) => Array.from({ length: n }, (_, i) => ({ id: `M${i + 1}`, title: `m${i + 1}` }));
  const detail = (over: Record<string, unknown>) => planDetail(over as Parameters<typeof planDetail>[0]);

  it("prefers the candidate milestones", () => {
    expect(detail({ milestones: ms(2), milestones_candidate: ms(4) })).toBe("Plan ready, 4 milestones");
  });

  it("falls back to the frozen milestones when the candidate is null or absent", () => {
    expect(detail({ milestones: ms(2), milestones_candidate: null })).toBe("Plan ready, 2 milestones");
    expect(detail({ milestones: ms(2) })).toBe("Plan ready, 2 milestones");
  });

  it("says just Plan ready with no milestones, and an empty candidate does not fall back", () => {
    expect(detail({ milestones: null, milestones_candidate: null })).toBe("Plan ready");
    expect(detail({})).toBe("Plan ready");
    expect(detail({ milestones: ms(2), milestones_candidate: [] })).toBe("Plan ready");
  });

  it("is singular for one milestone", () => {
    expect(detail({ milestones: null, milestones_candidate: ms(1) })).toBe("Plan ready, 1 milestone");
  });
});

describe("decisionCount", () => {
  it("counts non-pr run items plus the held aggregate count, never open PRs", () => {
    const items = overviewDecisions(
      [
        run({ id: "plan", status: "awaiting_approval" }),
        run({ id: "q", status: "awaiting_input" }),
        run({ id: "pr", status: "completed", kind: "issue", mr_iid: 1, mr_state: "opened", created_at: "2026-07-05T10:00:00Z" }),
      ],
      heldHolds({ open_holds: 2, decision_needed: 2 }, [decisionHold("a", "2026-07-05T10:00:00Z")]),
    );
    expect(decisionCount(items)).toBe(4);
    expect(decisionCount([])).toBe(0);
  });
});

// PRD #4123: the `limit` decision kind, from the summary row's compact weekly_limit.
describe("overviewDecisions limit kind (PRD #4123)", () => {
  const at = (m: number) => `2026-07-05T12:${String(m).padStart(2, "0")}:00Z`;
  const wl = (key: string, label: string, tier: "warning" | "notice" = "warning") => ({
    tier,
    credential_key: key,
    credential_label: label,
  });
  const limits = (runs: RunSummaryItem[]) =>
    overviewDecisions(runs).flatMap((d) => (d.kind === "limit" ? [d.run.id] : []));

  it("lists warning-tier runs with the Review fix verb, oldest first, before open PRs", () => {
    const got = overviewDecisions([
      run({ id: "pr", status: "completed", kind: "issue", mr_iid: 1, mr_state: "opened", created_at: at(1), finished_at: at(2) }),
      run({ id: "late", status: "running", status_since: at(9), weekly_limit: wl("anthropic:a", "A") }),
      run({ id: "early", status: "running", status_since: at(3), weekly_limit: wl("anthropic:a", "A") }),
    ]);
    expect(got.map((d) => (d.kind === "held" ? "held" : [d.run.id, d.kind, d.verb]))).toEqual([
      ["early", "limit", "Review fix"],
      ["late", "limit", "Review fix"],
      ["pr", "pr", "Review PR"],
    ]);
  });

  it("gives notice-tier runs and runs without a block no row", () => {
    expect(
      limits([
        run({ id: "notice", status: "running", weekly_limit: wl("anthropic:a", "A", "notice") }),
        run({ id: "none", status: "running" }),
      ]),
    ).toEqual([]);
  });

  it("gives a timed limit_wait and a weekly-reserve pause (both automatic) their limit row", () => {
    expect(
      limits([
        run({ id: "timed", status: "limit_wait", rate_limit_type: "seven_day", weekly_limit: wl("anthropic:a", "A") }),
        run({ id: "reserve", status: "paused", hold_reason: "weekly_reserve", weekly_limit: wl("codex:c1", "C1") }),
      ]),
    ).toEqual(["timed", "reserve"]);
  });

  it("never lists a run twice: an owner park, plan or question keeps its own row", () => {
    const got = overviewDecisions([
      run({ id: "owner-park", status: "paused", weekly_limit: wl("anthropic:a", "A") }),
      run({ id: "plan", status: "awaiting_approval", weekly_limit: wl("anthropic:a", "A") }),
      run({ id: "ask", status: "awaiting_input", weekly_limit: wl("anthropic:a", "A") }),
      run({ id: "running", status: "running", weekly_limit: wl("anthropic:a", "A") }),
    ]);
    const ids = got.flatMap((d) => (d.kind === "held" ? [] : [d.run.id]));
    expect(new Set(ids).size).toBe(ids.length);
    expect(got.find((d) => d.kind !== "held" && d.run.id === "owner-park")?.kind).toBe("park");
    expect(got.filter((d) => d.kind === "limit").map((d) => (d.kind === "held" ? "" : d.run.id))).toEqual(["running"]);
  });

  it("ignores a block on a terminal run", () => {
    expect(limits([run({ id: "done", status: "failed", weekly_limit: wl("anthropic:a", "A") })])).toEqual([]);
  });

  it("counts each limit run as a decision", () => {
    expect(
      decisionCount(
        overviewDecisions([
          run({ id: "a", status: "running", weekly_limit: wl("anthropic:a", "A") }),
          run({ id: "b", status: "running", weekly_limit: wl("anthropic:a", "A") }),
        ]),
      ),
    ).toBe(2);
  });
});

describe("decisionRows limit grouping (PRD #4123)", () => {
  const wl = (key: string, label: string) => ({ tier: "warning" as const, credential_key: key, credential_label: label });
  const rowsOf = (runs: RunSummaryItem[]) => decisionRows(overviewDecisions(runs));

  it("groups by credential_key, one row even for a single run", () => {
    const rows = rowsOf([
      run({ id: "r1", status: "running", status_since: "2026-07-05T12:01:00Z", weekly_limit: wl("anthropic:a", "A") }),
      run({ id: "r2", status: "running", status_since: "2026-07-05T12:02:00Z", weekly_limit: wl("codex:c1", "C1") }),
      run({ id: "r3", status: "running", status_since: "2026-07-05T12:03:00Z", weekly_limit: wl("anthropic:a", "A") }),
    ]);
    expect(rows.map((r) => (r.type === "limit" ? [r.credentialKey, r.credentialLabel, r.items.map((i) => i.run.id)] : r.type))).toEqual([
      ["anthropic:a", "A", ["r1", "r3"]],
      ["codex:c1", "C1", ["r2"]],
    ]);
  });

  it("keeps two credentials that share a label as separate rows", () => {
    const rows = rowsOf([
      run({ id: "r1", status: "running", weekly_limit: wl("anthropic:a", "work") }),
      run({ id: "r2", status: "running", weekly_limit: wl("anthropic:b", "work") }),
    ]);
    expect(rows.map((r) => (r.type === "limit" ? r.credentialKey : r.type))).toEqual(["anthropic:a", "anthropic:b"]);
  });

  it("names the credential's vendor from the key prefix only", () => {
    expect(limitCredentialNoun("anthropic:sec-1")).toBe("Token");
    expect(limitCredentialNoun("codex:acct-1")).toBe("Codex account");
    expect(limitCredentialNoun("future:x")).toBe("Credential");
  });
});
