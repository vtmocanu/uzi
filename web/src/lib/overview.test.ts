import { describe, expect, it } from "vitest";
import type { RunStatus, RunSummaryItem } from "./api";
import {
  mergeOverviewRows,
  overviewCounts,
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
