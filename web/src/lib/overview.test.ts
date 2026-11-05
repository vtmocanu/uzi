import { describe, expect, it } from "vitest";
import type { RunStatus, RunSummaryItem } from "./api";
import {
  mergeOverviewRows,
  openPrs,
  overviewCounts,
  overviewDecisions,
  parkDisposition,
  ownRecoveryWaitCount,
  setupComplete,
  statusSentence,
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

describe("statusSentence", () => {
  const none = { executing: 0, queued: 0, parked: 0 };
  const base = { decisions: 0, prs: 0, counts: none, acrossFactory: false, adminUnavailable: false };

  it("reads calm when nothing needs the owner", () => {
    expect(statusSentence({ ...base, counts: { executing: 1, queued: 0, parked: 0 } })).toBe(
      "Nothing needs you right now. 1 run is executing.",
    );
  });

  it("uses a singular verb for one decision and one pull request, a plural for the others", () => {
    expect(statusSentence({ ...base, decisions: 1 })).toMatch(/^1 decision is waiting on you\./);
    expect(statusSentence({ ...base, prs: 2 })).toMatch(/^2 pull requests are waiting on you\./);
    expect(statusSentence({ ...base, prs: 1 })).toMatch(/^1 pull request is waiting on you\./);
    expect(statusSentence({ ...base, decisions: 1, prs: 1 })).toMatch(
      /^1 decision and 1 pull request are waiting on you\./,
    );
    expect(statusSentence({ ...base, decisions: 3, prs: 1 })).toMatch(
      /^3 decisions and 1 pull request are waiting on you\./,
    );
  });

  it("lists queued and parked counts, dropping a zero one", () => {
    expect(statusSentence({ ...base, counts: { executing: 3, queued: 1, parked: 2 } })).toBe(
      "Nothing needs you right now. 3 runs are executing, 1 queued and 2 parked.",
    );
    expect(statusSentence({ ...base, counts: { executing: 3, queued: 0, parked: 2 } })).toBe(
      "Nothing needs you right now. 3 runs are executing and 2 parked.",
    );
    expect(statusSentence({ ...base, counts: { executing: 2, queued: 4, parked: 0 } })).toBe(
      "Nothing needs you right now. 2 runs are executing and 4 queued.",
    );
  });

  it("says across the factory for an admin's factory scope", () => {
    expect(statusSentence({ ...base, acrossFactory: true, counts: { executing: 3, queued: 1, parked: 2 } })).toBe(
      "Nothing needs you right now. 3 runs are executing across the factory, 1 queued and 2 parked.",
    );
  });

  it("counts only the viewer's runs, says so and never claims the factory when the admin list is unavailable", () => {
    const sentence = statusSentence({
      ...base,
      acrossFactory: true,
      adminUnavailable: true,
      counts: { executing: 1, queued: 0, parked: 0 },
    });
    expect(sentence).toBe("Nothing needs you right now. 1 run is executing. Counting only your runs.");
    expect(sentence).not.toContain("across the factory");
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
    ["completion_blocked", { status: "paused", completion_phase: "blocked" }, "owner"],
    ["worker_requeue_exhausted", { status: "recovery_wait", recovery_wait_cause: "worker_requeue_exhausted" }, "owner"],
    ["worker_memory_pressure", { status: "recovery_wait", recovery_wait_cause: "worker_memory_pressure" }, "owner"],
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
});

describe("overviewDecisions", () => {
  const at = (m: number) => `2026-07-05T12:${String(m).padStart(2, "0")}:00Z`;

  it("excludes revising plans, stalled running runs and the weekly-reserve pause; includes owner parks", () => {
    const got = overviewDecisions([
      run({ id: "revising", status: "awaiting_approval", is_revising: true }),
      run({ id: "stalled", status: "running", progress: { state: "stalled" } as never }),
      run({ id: "reserve", status: "paused", hold_reason: "weekly_reserve" }),
      run({ id: "limit", status: "limit_wait" }),
      run({ id: "owner-park", status: "paused" }),
    ]);
    expect(got.map((d) => [d.run.id, d.kind, d.verb])).toEqual([["owner-park", "park", "Resume"]]);
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
    expect(got.map((d) => [d.run.id, d.verb, d.since])).toEqual([
      ["plan-old", "Review plan", at(5)],
      ["plan-new", "Review plan", at(8)],
      ["q-old", "Answer", at(4)],
      ["q-new", "Answer", at(9)],
      ["park", "Resume", at(3)],
      ["pr", "Review PR", at(2)],
    ]);
  });

  it("orders unparseable timestamps deterministically: last, in input order", () => {
    const got = overviewDecisions([
      run({ id: "bad-1", status: "awaiting_approval", status_since: "garbage" }),
      run({ id: "bad-2", status: "awaiting_approval", status_since: "nope" }),
      run({ id: "good", status: "awaiting_approval", status_since: at(5) }),
    ]);
    expect(got.map((d) => d.run.id)).toEqual(["good", "bad-1", "bad-2"]);
  });
});

describe("statusSentence from decisions", () => {
  const sentenceFor = (runs: RunSummaryItem[]) => {
    const items = overviewDecisions(runs);
    return statusSentence({
      decisions: items.filter((d) => d.kind !== "pr").length,
      prs: items.filter((d) => d.kind === "pr").length,
      counts: { executing: 0, queued: 0, parked: 0 },
      acrossFactory: false,
      adminUnavailable: false,
    });
  };
  const plan = run({ id: "p", status: "awaiting_approval" });
  const pr = (id: string) =>
    run({ id, status: "completed", kind: "issue", mr_iid: 9, mr_state: "opened", repo_id: "r", created_at: "2026-07-01T00:00:00Z" });

  it("reads the four documented sentences", () => {
    expect(sentenceFor([plan, pr("a")])).toMatch(/^1 decision and 1 pull request are waiting on you\./);
    expect(sentenceFor([plan])).toMatch(/^1 decision is waiting on you\./);
    expect(sentenceFor([pr("a"), run({ id: "b", status: "completed", kind: "issue", mr_iid: 10, mr_state: "opened", repo_id: "r", created_at: "2026-07-01T00:00:00Z" })])).toMatch(
      /^2 pull requests are waiting on you\./,
    );
    expect(sentenceFor([])).toMatch(/^Nothing needs you right now\./);
  });

  it("counts two runs sharing one PR once", () => {
    expect(sentenceFor([pr("a"), pr("b")])).toMatch(/^1 pull request is waiting on you\./);
  });
});
