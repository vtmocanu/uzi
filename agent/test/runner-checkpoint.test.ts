import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import { type Executor } from "../src/executor.js";
import type { Logger } from "../src/log.js";
import { WIP_PARK_COMMIT_PREFIX, type OwedCandidateContext } from "../src/git.js";
import { ScratchPublicationError, type ScratchPublicationKind, type ScratchPublicationStep } from "../src/git.js";
import type { PublishResult } from "../src/protocol.js";
import { followSuccessfulPush } from "./publication-fixture.js";
import {
  api,
  client,
  fakeGitHub,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  installHarness,
  runner,
  runnerWith,
} from "./runner-harness.js";

installHarness();

// The checkpoint callback (PRD #122 M6) is constructed inside RunRunner.execute and handed
// to the executor as ctx.checkpoint. These tests drive it through a CUSTOM executor that
// commits in the (real) runner clone, then calls ctx.checkpoint, so the whole path —
// Decision 6 no-op rejection, reap-before-git ordering, and the milestone report — is
// exercised against a real fixture bare + clone with no live SDK.

const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

/** Commit a new file in the runner clone so its branch tip advances. */
function commitInClone(dir: string, file: string): void {
  const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
  fs.writeFileSync(path.join(dir, file), `${file}\n`);
  execFileSync("git", ["-C", dir, "add", file], { env });
  execFileSync("git", ["-C", dir, ...IDENT, "commit", "-m", `work ${file}`], { env });
}

/** Spy on git.fetchAgentBranch, recording a "fetch" event and calling through. Returns a
 *  restore fn (call it in a finally). */
function spyFetch(events: string[]): () => void {
  const orig = git.fetchAgentBranch.bind(git);
  (git as unknown as { fetchAgentBranch: unknown }).fetchAgentBranch = async (
    ...args: unknown[]
  ) => {
    events.push("fetch");
    return (orig as (...a: unknown[]) => Promise<unknown>)(...args);
  };
  return () => {
    (git as unknown as { fetchAgentBranch: unknown }).fetchAgentBranch = orig;
  };
}

/** Spy on client.publishCheckpoint (PRD #122 M8), recording a "publish" event. Drains the
 *  real pack stream so pack-objects can exit, and optionally throws to prove a publish
 *  failure never fails the run. `nullFirst` returns a non-2xx PublishResult (the client's
 *  `{ ok: false, httpStatus }` contract for an UNCONFIRMED publish, issue #1030) for the
 *  first N calls before succeeding, so a test can drive a fail-once-then-succeed broker
 *  without throwing (PRD #267 Fix 1). Returns a restore fn (call it in a finally). */
function spyPublish(
  events: string[],
  opts: { throws?: boolean; nullFirst?: number } = {},
): () => void {
  const orig = client.publishCheckpoint.bind(client);
  let calls = 0;
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    ...args: unknown[]
  ) => {
    events.push("publish");
    const pack = args[2] as Readable | undefined;
    if (pack) for await (const _chunk of pack) { /* wait for git to exit before the fake ACK */ }
    if (opts.throws) throw new Error("boom publish");
    calls += 1;
    // A non-2xx result models an UNCONFIRMED publish: it did NOT confirmably land.
    if (opts.nullFirst && calls <= opts.nullFirst) return { ok: false, httpStatus: 500 };
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return () => {
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
  };
}

/** A Logger that records every `info` message string (across children, which share the
 *  same list). Used to prove the PRD #267 M3 time-based-publish signal is emitted. */
function capturingLogger(): { logger: Logger; infos: string[] } {
  const infos: string[] = [];
  const self: Logger = {
    debug() {},
    info: (msg) => {
      infos.push(msg);
    },
    warn() {},
    error() {},
    addSecret() {},
    removeSecret() {},
    child: () => self,
  };
  return { logger: self, infos };
}

const TIME_BASED_MSG = "checkpoint published to origin (time-based)";

describe("RunRunner — milestone checkpoint (PRD #122 M6)", () => {
  it("reap:true reaps the agent tree BEFORE the fetch-back (reap-before-git, B1 order)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(60);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    const restore = spyFetch(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restore();
    }
    // The cooperative checkpoint reaps, then fetches — in that order.
    assert.deepStrictEqual(afterCheckpoint, ["kill", "fetch"]);
  });

  it("reap:false fetches back WITHOUT reaping (the iteration-boundary fallback)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(61);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    const restore = spyFetch(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({ reap: false, progress: { completed: ["m1"], in_progress: [] } });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restore();
    }
    // Fallback: a fetch, but NO reap — a backgrounded dev server must survive.
    assert.deepStrictEqual(afterCheckpoint, ["fetch"]);
  });

  it("skips the FETCH (not the reap) when the branch tip is unmoved since the last checkpoint (Decision 6 no-op)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(62);
    const events: string[] = [];
    let afterSecond: string[] = [];
    const restore = spyFetch(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        // First checkpoint fetches and writes the tracking ref.
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        const beforeSecond = events.length;
        // Second checkpoint with NO new commit: the tip is unmoved, so the FETCH is skipped.
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        afterSecond = events.slice(beforeSecond);
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restore();
    }
    // #1036 fix: the reap is tied to opts.reap (the real "safe to reap" signal), NOT to the
    // fetch — it now fires on EVERY reap:true checkpoint, STRICTLY before any git below,
    // because the #1036 overlay adds a credentialed op the reap must precede. So an unmoved
    // tip still skips the fetch, but reap:true still reaps.
    assert.deepStrictEqual(afterSecond, ["kill"]);
  });

  it("reports the checkpointed milestone as a running report (no iteration_count)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(63);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({
          reap: true,
          progress: { completed: ["m1"], in_progress: ["m2"] },
        });
        return { branch: ctx.branch };
      },
      killAgentTree: () => {},
    };
    await runner(exec, gitlab).execute(claim);

    const report = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body)
      .find((b) => b.status === "running" && Array.isArray(b.milestones_completed));
    assert.ok(report, "a running report carried the checkpointed milestone progress");
    assert.deepStrictEqual(report!.milestones_completed, ["m1"]);
    assert.deepStrictEqual(report!.milestones_in_progress, ["m2"]);
    assert.ok(
      !("iteration_count" in report!),
      "the checkpoint report omits iteration_count so it cannot regress the GREATEST-merged counter",
    );
  });

  it("ctx.reportProgress sends an immediate running report (milestone fields, no iteration_count) and serializes BEFORE the next reportIteration (PRD #1064 M1)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(90);
    const exec: Executor = {
      run: async (ctx) => {
        // The immediate push (fire-and-forget, enqueued on the per-run chain).
        await ctx.reportProgress!({ completed: ["m1"], in_progress: ["m2"] });
        // The turn-boundary report AWAITS the per-run chain, so once it resolves the push
        // above has landed on the api — that ordering is the resurrection guard (Decision 1).
        await ctx.reportIteration!(1, { completed: ["m1"], in_progress: [] });
        return { branch: ctx.branch };
      },
      killAgentTree: () => {},
    };
    await runner(exec, gitlab).execute(claim);

    const bodies = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
    const pushIdx = bodies.findIndex(
      (b) =>
        b.status === "running" &&
        !("iteration_count" in b) &&
        Array.isArray(b.milestones_in_progress) &&
        b.milestones_in_progress.includes("m2"),
    );
    assert.ok(pushIdx >= 0, "an immediate running report carried in_progress [m2] and omitted iteration_count");
    assert.deepStrictEqual(bodies[pushIdx]!.milestones_completed, ["m1"]);
    // The turn-boundary report (iteration_count present, in_progress cleared) comes AFTER
    // the push on the wire — a late push can never overtake the next snapshot.
    const iterIdx = bodies.findIndex(
      (b) => b.status === "running" && b.iteration_count === 1,
    );
    assert.ok(iterIdx >= 0, "the turn-boundary report was sent");
    assert.ok(pushIdx < iterIdx, "the immediate push reached the api before the turn-boundary report");
  });

  it("a reportProgress push whose reportState rejects does not fail the run (PRD #1064 M1)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(91);
    // Make the NEXT reportState call (the immediate push) reject once, proving the runner's
    // reportProgress swallows it. reportState has bounded retries in production; here we
    // force a hard reject to exercise the log-and-continue path.
    const origReportState = client.reportState.bind(client);
    let failNext = false;
    (client as unknown as { reportState: unknown }).reportState = async (
      ...args: unknown[]
    ) => {
      if (failNext) {
        failNext = false;
        throw new Error("reportState boom");
      }
      return (origReportState as (...a: unknown[]) => Promise<unknown>)(...args);
    };
    try {
      let reachedEnd = false;
      const exec: Executor = {
        run: async (ctx) => {
          failNext = true;
          await ctx.reportProgress!({ completed: [], in_progress: ["m2"] });
          // Drain the chain so the (rejecting) push has run before we finish.
          await ctx.reportIteration!(1, undefined);
          reachedEnd = true;
          return { branch: ctx.branch };
        },
        killAgentTree: () => {},
      };
      // execute() returns void; a rejected push must not throw out of it.
      await assert.doesNotReject(runner(exec, gitlab).execute(claim));
      assert.ok(reachedEnd, "the executor ran to the end despite the rejected push");
    } finally {
      (client as unknown as { reportState: unknown }).reportState = origReportState;
    }
  });

  it("drains the per-run chain BEFORE the terminal report, so a milestone completion in a final-frame reportProgress push is not lost (PRD #1064 M1, CodeRabbit)", async () => {
    // Regression for the ordering gap: the final SDK frame can carry BOTH report_progress
    // (a milestone newly completed) AND signal_done. reportProgress is fire-and-forget, so
    // without the drain in phasePreflightHandoff the terminal `completed` report can land
    // FIRST; the queued `running` push then no-ops against the finished run (SetRunRunning's
    // WHERE guard) and the m2 completion — reported only via report_progress — is lost.
    //
    // The test is deterministic, not timing-based: the m2 push's reportState blocks on a
    // `completedSeen` gate that the terminal `completed` report resolves. WITHOUT the drain
    // the completed report runs first, resolves the gate, and the push records AFTER it —
    // the assertion fails. WITH the drain, execute() cannot reach phasePublish until the
    // chain settles, so `completedSeen` never resolves there; a self-release timeout lets
    // the push proceed, it records FIRST, and only then does `completed` land. The timeout
    // elapses ONLY on the fixed path, so the un-fixed path fails immediately.
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(92);
    let releaseCompletedSeen!: () => void;
    const completedSeen = new Promise<void>((r) => {
      releaseCompletedSeen = r;
    });
    const origReportState = client.reportState.bind(client);
    (client as unknown as { reportState: unknown }).reportState = async (...args: unknown[]) => {
      const body = args[1] as {
        status?: string;
        iteration_count?: number;
        milestones_completed?: unknown;
      };
      const isM2Push =
        body?.status === "running" &&
        !("iteration_count" in body) &&
        Array.isArray(body.milestones_completed) &&
        (body.milestones_completed as string[]).includes("m2");
      if (isM2Push) {
        // Hold the push until the terminal report has landed (un-fixed path) or a self-release
        // timeout fires (fixed path, where completed is gated behind the chain). Either way the
        // push resolves on its own — a gate the fix cannot deadlock on.
        await Promise.race([completedSeen, new Promise((r) => setTimeout(r, 1000))]);
      }
      const out = await (origReportState as (...a: unknown[]) => Promise<unknown>)(...args);
      if (body?.status === "completed") releaseCompletedSeen();
      return out;
    };
    try {
      const exec: Executor = {
        run: async (ctx) => {
          commitInClone(ctx.worktreePath, "NEW.txt");
          // Final-frame shape: the milestone completion arrives ONLY as a fire-and-forget
          // push (no following awaited reportIteration carries it), then the run ends.
          await ctx.reportProgress!({ completed: ["m2"], in_progress: [] });
          return { branch: ctx.branch };
        },
        killAgentTree: () => {},
      };
      await runner(exec, gitlab).execute(claim);
    } finally {
      (client as unknown as { reportState: unknown }).reportState = origReportState;
    }
    const bodies = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
    const pushIdx = bodies.findIndex(
      (b) =>
        b.status === "running" &&
        !("iteration_count" in b) &&
        Array.isArray(b.milestones_completed) &&
        b.milestones_completed.includes("m2"),
    );
    const completedIdx = bodies.findIndex((b) => b.status === "completed");
    assert.ok(pushIdx >= 0, "the m2-completed push reached the api");
    assert.ok(completedIdx >= 0, "the run reported completed");
    assert.ok(
      pushIdx < completedIdx,
      "the fire-and-forget m2 push landed BEFORE the terminal completed (the drain enforced ordering)",
    );
  });

  it("drains the per-run chain when executor.run REJECTS, so a final-frame reportProgress push is not lost against the terminal failed report (PRD #1064 M1, CodeRabbit)", async () => {
    // The reject-path twin of the test above: the success-only drain left the throw leg
    // exposed. When executor.run rejects, the catch in execute() reports `failed`; a queued
    // fire-and-forget reportProgress push must still reach the api first, or it no-ops against
    // the finished run. The drain lives in a `finally`, so it covers this path too. Same
    // deterministic gate: the m2 push blocks on `failedSeen` (resolved by the terminal `failed`
    // report), raced against a self-release timeout so the fixed path cannot deadlock.
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(93);
    let releaseFailedSeen!: () => void;
    const failedSeen = new Promise<void>((r) => {
      releaseFailedSeen = r;
    });
    const origReportState = client.reportState.bind(client);
    (client as unknown as { reportState: unknown }).reportState = async (...args: unknown[]) => {
      const body = args[1] as {
        status?: string;
        iteration_count?: number;
        milestones_completed?: unknown;
      };
      const isM2Push =
        body?.status === "running" &&
        !("iteration_count" in body) &&
        Array.isArray(body.milestones_completed) &&
        (body.milestones_completed as string[]).includes("m2");
      if (isM2Push) {
        await Promise.race([failedSeen, new Promise((r) => setTimeout(r, 1000))]);
      }
      const out = await (origReportState as (...a: unknown[]) => Promise<unknown>)(...args);
      if (body?.status === "failed") releaseFailedSeen();
      return out;
    };
    try {
      const exec: Executor = {
        run: async (ctx) => {
          // Report a milestone completion as a fire-and-forget push, then FAIL the run before
          // any turn-boundary report carries it — the reject-path shape of the loss.
          await ctx.reportProgress!({ completed: ["m2"], in_progress: [] });
          throw new Error("boom after reporting m2 complete");
        },
        killAgentTree: () => {},
      };
      // execute() swallows the failure into a `failed` report; it must not throw out.
      await runner(exec, gitlab).execute(claim);
    } finally {
      (client as unknown as { reportState: unknown }).reportState = origReportState;
    }
    const bodies = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
    const pushIdx = bodies.findIndex(
      (b) =>
        b.status === "running" &&
        !("iteration_count" in b) &&
        Array.isArray(b.milestones_completed) &&
        b.milestones_completed.includes("m2"),
    );
    const failedIdx = bodies.findIndex((b) => b.status === "failed");
    assert.ok(pushIdx >= 0, "the m2-completed push reached the api");
    assert.ok(failedIdx >= 0, "the run reported failed");
    assert.ok(
      pushIdx < failedIdx,
      "the fire-and-forget m2 push landed BEFORE the terminal failed report (the finally drain covers the reject path)",
    );
  });

  it("reap:true brokers the checkpoint pack to origin, strictly AFTER the reap and fetch-back (PRD #122 M8)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(64);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    // Reap, then fetch-back, then the brokered publish — publish is credentialed-class and
    // must stay after the reap and the tracking-ref update the fetch-back writes.
    assert.deepStrictEqual(afterCheckpoint, ["kill", "fetch", "publish"]);
  });

  it("reap:false does NOT publish (the iteration-boundary fallback fetches only)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(65);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({ reap: false, progress: { completed: ["m1"], in_progress: [] } });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    assert.deepStrictEqual(afterCheckpoint, ["fetch"]);
  });

  it("a thrown publish error is swallowed — the run still completes and reports state (best-effort)", async () => {
    const forge = fakeGitlab();
    followSuccessfulPush(git, forge.pr);
    const claim = gitlabClaim(66);
    const events: string[] = [];
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events, { throws: true });
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      // Must NOT reject even though publishCheckpoint throws.
      await runner(exec, forge.gitlab).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    assert.ok(events.includes("publish"), "the publish was attempted");
    const completed = api.states.some(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    );
    assert.ok(completed, "the run completed despite the publish failure");
    assert.equal(events.filter(e => e === "publish").length, 1, "a transport throw adds no checkpoint retry");
    assert.equal(execFileSync("git", ["-C", fx.originPath, "rev-parse", "--verify", "refs/heads/agent/issue-66"],
      { encoding: "utf8" }).trim().length, 40, "final publication pushes the work branch after checkpoint failure");
    assert.equal(forge.calls.filter(c => c.method === "POST").length, 1, "final publication opens one MR");
    const texts = api.messages(claim.run_id).map(m => String(m.payload.text ?? ""));
    assert.ok(texts.some(t => t === "checkpoint publish failed: error (phase=publish_request, cause=unknown)"),
      "checkpoint failure remains a distinct diagnostic after successful final delivery");
  });
});

// PRD #267: the reap:false iteration-boundary checkpoint may now publish to origin on a
// time budget, WITHOUT reaping the agent tree. These tests drive the checkpoint callback
// through a custom executor that commits then calls ctx.checkpoint({reap:false}), with an
// injected fake clock and a short (1000ms) interval so the time-gate is deterministic.
describe("RunRunner — time-based checkpoint (PRD #267)", () => {
  const progress = { completed: [], in_progress: ["m1"] };

  it("reap:false past the interval with a moved tip publishes AND does zero killAgentTree", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(70);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        fakeNow = 1001; // cross the 1000ms interval since run start (lastPublish=0)
        await ctx.checkpoint!({ reap: false, progress });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 1000, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    // A moved tip fetches, then the open time-gate publishes — and reap:false never kills.
    assert.ok(!afterCheckpoint.includes("kill"), "reap:false did NOT kill the agent tree");
    assert.deepStrictEqual(afterCheckpoint, ["fetch", "publish"]);
  });

  it("reap:false before the interval does not publish", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(71);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        fakeNow = 500; // still inside the interval
        await ctx.checkpoint!({ reap: false, progress });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 1000, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    // Tip moved so it fetches, but the gate is closed — no publish.
    assert.deepStrictEqual(afterCheckpoint, ["fetch"]);
  });

  it("idle-commit regression (Decision 9): a commit that goes idle for >= the interval still publishes exactly once", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(72);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        // Interval closed at fakeNow=0: fetch happens, no publish yet.
        await ctx.checkpoint!({ reap: false, progress });
        assert.ok(!events.includes("publish"), "first checkpoint did not publish (gate closed)");
        // No new commit; the tip is now unmoved since the last fetch. Advancing past the
        // interval must STILL publish — the gate keys on lastPublishedTip, not the fetch-skip.
        fakeNow = 1001;
        await ctx.checkpoint!({ reap: false, progress });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 1000, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    const publishes = afterCheckpoint.filter((e) => e === "publish").length;
    const fetches = afterCheckpoint.filter((e) => e === "fetch").length;
    assert.equal(publishes, 1, "the idle commit shipped exactly once");
    assert.equal(fetches, 1, "the second checkpoint skipped the fetch (tip unmoved) but still published");
  });

  it("nothing new since last publish publishes nothing", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(73);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        fakeNow = 1001;
        await ctx.checkpoint!({ reap: false, progress }); // publishes tip
        // No new commit; a later checkpoint sees cloneTip === lastPublishedTip.
        fakeNow = 3000;
        await ctx.checkpoint!({ reap: false, progress });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 1000, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    const publishes = afterCheckpoint.filter((e) => e === "publish").length;
    assert.equal(publishes, 1, "an already-published tip is not re-published");
  });

  it("CHECKPOINT_INTERVAL=0 disables the time path", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(74);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        fakeNow = 999_999; // arbitrarily far past any interval
        await ctx.checkpoint!({ reap: false, progress });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 0, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    // interval 0 => the time path never publishes, however much time elapsed.
    assert.deepStrictEqual(afterCheckpoint, ["fetch"]);
  });

  it("a milestone (reap:true) publish resets the gate so a following reap:false under the interval does not publish", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(75);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "A.txt");
        // Advance the clock to a NONZERO time BEFORE the milestone so the gate reset moves
        // lastPublish to a distinct value (5000). If the milestone did NOT reset the gate,
        // the reap:false at 5500 below would see 5500 - 0 >= 1000 (gate open) and publish B —
        // so the publishes==1 assertion genuinely exercises the reset, not "never moved".
        fakeNow = 5000;
        await ctx.checkpoint!({ reap: true, progress });
        commitInClone(ctx.worktreePath, "B.txt");
        fakeNow = 5500; // < 1000 after the milestone reset at t=5000
        await ctx.checkpoint!({ reap: false, progress });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 1000, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    const publishes = afterCheckpoint.filter((e) => e === "publish").length;
    assert.equal(publishes, 1, "only the milestone published; the gate reset blocks the next one");
    assert.ok(afterCheckpoint.includes("kill"), "the milestone (reap:true) reaped the agent tree");
  });

  it("publishes at most once per interval even with new commits each iteration", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(76);
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "A.txt");
        fakeNow = 1001; // crosses the interval => publishes, lastPublish := 1001
        await ctx.checkpoint!({ reap: false, progress });
        commitInClone(ctx.worktreePath, "B.txt");
        fakeNow = 1400; // 399 since the last publish => gated
        await ctx.checkpoint!({ reap: false, progress });
        commitInClone(ctx.worktreePath, "C.txt");
        fakeNow = 1900; // 899 since the last publish => still gated
        await ctx.checkpoint!({ reap: false, progress });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 1000, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    const publishes = afterCheckpoint.filter((e) => e === "publish").length;
    assert.equal(publishes, 1, "at most one origin publish per interval per run");
  });

  it("logs the time-based-publish signal when a reap:false checkpoint publishes past the interval (PRD #267 M3)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(77);
    const events: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const { logger, infos } = capturingLogger();
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const executor: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        fakeNow = 1001; // cross the 1000ms interval since run start
        await ctx.checkpoint!({ reap: false, progress });
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runnerWith(() => ({ executor }), gitlab, undefined, logger, {
        checkpointIntervalMs: 1000,
        now,
      }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    assert.ok(events.includes("publish"), "the time-based checkpoint published");
    assert.ok(
      infos.some((m) => m.includes(TIME_BASED_MSG)),
      "a reap:false publish past the interval logs the time-based signal",
    );
  });

  it("a milestone (reap:true) publish does NOT log the time-based signal (PRD #267 M3)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(78);
    const events: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const { logger, infos } = capturingLogger();
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const executor: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({ reap: true, progress });
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runnerWith(() => ({ executor }), gitlab, undefined, logger, {
        checkpointIntervalMs: 1000,
        now,
      }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    assert.ok(events.includes("publish"), "the milestone checkpoint published");
    assert.ok(
      !infos.some((m) => m.includes(TIME_BASED_MSG)),
      "the milestone publish is visible via its running report, not the time-based line",
    );
  });

  it("a FAILED time-based publish does NOT strand the tip — the idle commit ships on retry (PRD #267 Fix 1)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(79);
    const events: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    // The FIRST publish returns a non-2xx PublishResult (the client's { ok: false,
    // httpStatus } contract — an UNCONFIRMED publish); every later publish succeeds. A
    // failed publish must NOT advance
    // lastPublishedTip, so the SAME tip stays publish-eligible next interval.
    const restorePublish = spyPublish(events, { nullFirst: 3 });
    const publishesAtEachStage: number[] = [];
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        // t=1001: gate open, tip moved → publishes, but the broker fails (null). lastPublish
        // advances to 1001 (bounds retry cadence) yet lastPublishedTip does NOT.
        fakeNow = 1001;
        await ctx.checkpoint!({ reap: false, progress });
        publishesAtEachStage.push(events.filter((e) => e === "publish").length);
        // NO new commit. One full interval later the same idle tip must be retried and this
        // time land — the "worst-case loss ~one interval" guarantee (Decision 9).
        fakeNow = 2002;
        await ctx.checkpoint!({ reap: false, progress });
        publishesAtEachStage.push(events.filter((e) => e === "publish").length);
        // A THIRD interval later, with the tip now CONFIRMED published, there is nothing new
        // to ship — proving the second publish actually advanced lastPublishedTip.
        fakeNow = 3003;
        await ctx.checkpoint!({ reap: false, progress });
        publishesAtEachStage.push(events.filter((e) => e === "publish").length);
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 1000, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    // 1 (failed) → 2 (retry lands) → 2 (nothing new): the failed publish did not strand the
    // tip, and the successful retry then stopped it re-publishing.
    assert.deepStrictEqual(
      publishesAtEachStage,
      [3, 4, 4],
      "a failed publish leaves the tip publish-eligible; the retry lands then stops re-publishing",
    );
  });

  it("no running report on a pure-idle checkpoint — tip unmoved AND nothing published (PRD #267 Fix 2)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(80);
    const events: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    let idleReportDelta = -1;
    let idleEvents: string[] = [];
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        // First checkpoint at a CLOSED gate (t=0): the tip moved, so it fetches and emits a
        // running report — but does not publish.
        await ctx.checkpoint!({ reap: false, progress });
        const beforeStates = api.states.filter((s) => s.runId === claim.run_id).length;
        const beforeEvents = events.length;
        // Second checkpoint: NO new commit and the gate still closed → pure idle. It must do
        // nothing: no fetch, no publish, and — restoring the pre-M1 early-return — no report.
        await ctx.checkpoint!({ reap: false, progress });
        idleReportDelta =
          api.states.filter((s) => s.runId === claim.run_id).length - beforeStates;
        idleEvents = events.slice(beforeEvents);
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, { checkpointIntervalMs: 1000, now }).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    assert.deepStrictEqual(idleEvents, [], "a pure-idle checkpoint neither fetches nor publishes");
    assert.equal(idleReportDelta, 0, "a pure-idle checkpoint emits no running report");
  });
});

// issue #1030: a checkpoint-publish failure or best-effort SKIP must be VISIBLE on the run
// feed rather than silently swallowed. These drive the mid-run (reap:true milestone) publish
// through a spy returning a FIXED PublishResult, then assert the emitted status feed lines,
// the deduping, and that a non-published outcome leaves the tip publish-eligible (so the next
// checkpoint retries — the interval-retry behaviour, exercised here by a second checkpoint).
describe("RunRunner — checkpoint-publish outcome is visible on the feed (issue #1030)", () => {
  /** Spy on client.publishCheckpoint returning a FIXED PublishResult, draining the pack so
   *  the git child exits and counting calls. Returns [restore, count]. */
  function spyPublishResult(result: PublishResult): { restore: () => void; count: () => number } {
    const orig = client.publishCheckpoint.bind(client);
    let calls = 0;
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
      ...args: unknown[]
    ) => {
      calls += 1;
      const pack = args[2] as Readable | undefined;
      if (pack) for await (const _chunk of pack) { /* wait for git to exit before the fake ACK */ }
      return result;
    };
    return {
      restore: () => {
        (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
      },
      count: () => calls,
    };
  }

  const statusTexts = (runId: string): string[] =>
    api
      .messages(runId)
      .filter((m) => m.kind === "status")
      .map((m) => String(m.payload.text));

  it("a 2xx {published:false, skipped:workflow_scope} does not advance the tip and emits ONE deduped skip line", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(90);
    const { restore, count } = spyPublishResult({
      ok: true,
      body: { published: false, ref: "", skipped: "workflow_scope" },
    });
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        // First milestone checkpoint publishes; the server SKIPS (workflow_scope), so the
        // tip is NOT marked published.
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        // No new commit. Because the skip left lastPublishedTip un-advanced, the SAME tip is
        // still "new work", so the next checkpoint RE-ATTEMPTS the publish — the mid-run proxy
        // for the ~20-min interval retry.
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        return { branch: ctx.branch };
      },
      killAgentTree: () => {},
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restore();
    }
    assert.equal(count(), 2, "the skip left the tip publish-eligible, so the second checkpoint retried");
    const skips = statusTexts(claim.run_id).filter((t) =>
      t.includes("checkpoint publish skipped: workflow_scope"),
    );
    assert.equal(
      skips.length,
      1,
      `exactly one deduped skip feed line across two identical attempts, got ${JSON.stringify(statusTexts(claim.run_id))}`,
    );
  });

  for (const label of ["tip_missing", "pack_too_large", "pack_invalid", "superseded", "unsupported", "ci_config_guard"] as const) {
    it(`a 2xx {published:false, skipped:${label}} surfaces the label on the feed, not "other"`, async () => {
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(92);
      const { restore } = spyPublishResult({
        ok: true,
        body: { published: false, ref: "", skipped: label },
      });
      const exec: Executor = {
        run: async (ctx) => {
          commitInClone(ctx.worktreePath, "NEW.txt");
          await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
          return { branch: ctx.branch };
        },
        killAgentTree: () => {},
      };
      try {
        await runner(exec, gitlab).execute(claim);
      } finally {
        restore();
      }
      const texts = statusTexts(claim.run_id);
      assert.ok(
        texts.some((t) => t.includes(`checkpoint publish skipped: ${label}`)),
        `expected skip label ${label}, got ${JSON.stringify(texts)}`,
      );
      assert.ok(
        !texts.some((t) => t.includes("checkpoint publish skipped: other")),
        `label must not fold to other, got ${JSON.stringify(texts)}`,
      );
    });
  }

  it("a non-2xx (HTTP 500) does not advance the tip and emits ONE deduped feed line naming the status", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(91);
    const { restore, count } = spyPublishResult({ ok: false, httpStatus: 500 });
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        // Two identical failing attempts on the same tip (the HTTP 500 does not advance it).
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        return { branch: ctx.branch };
      },
      killAgentTree: () => {},
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restore();
    }
    assert.equal(count(), 6, "the HTTP 500 left the tip publish-eligible, so the second checkpoint retried");
    const failures = statusTexts(claim.run_id).filter((t) =>
      t.includes("checkpoint publish failed: HTTP 500"),
    );
    assert.equal(
      failures.length,
      1,
      `two identical failing attempts produce exactly one feed line, got ${JSON.stringify(statusTexts(claim.run_id))}`,
    );
  });

  it("a successful publish emits NO failure/skip feed line", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(92);
    const { restore } = spyPublishResult({
      ok: true,
      body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" },
    });
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        return { branch: ctx.branch };
      },
      killAgentTree: () => {},
    };
    try {
      await runner(exec, gitlab).execute(claim);
    } finally {
      restore();
    }
    assert.ok(
      !statusTexts(claim.run_id).some((t) => /checkpoint publish (failed|skipped)/.test(t)),
      "a confirmed publish is silent on the failure/skip channel",
    );
  });
});

describe("RunRunner — report_only checkpoint orphan guard (issues #299 / #2967)", () => {
  // This pins the SAME-WORKER leg of the union guard: a run that publishes a checkpoint
  // mid-run (landedCheckpoint is set on the confirmed publish) and THEN declares
  // report_only must FAIL, because a report-only completion opens no branch/MR and would
  // orphan the published refs/uzi-checkpoints/<branch>. hasCommittedCheckpoint is NOT stubbed
  // here (the fixture bare carries no mirrored checkpoint ref — the publish went through
  // the client spy), so the guard trips on landedCheckpoint alone.
  it("#2967 completes report_only after this worker published only recovery coverage", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(2967);
    const events: string[] = [];
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        // Worker coverage commits retain a tree without adding agent content.
        for (const round of [0, 1]) execFileSync("git", ["-C", ctx.worktreePath,
          "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
          "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m",
          `uzi recovery coverage fixture round ${round} batch 0`]);
        await ctx.checkpoint!({ reap: true });
        return { branch: ctx.branch, reportOnly: true, summary: "measurement report delivered" };
      },
      killAgentTree: () => events.push("kill"),
    };
    try { await runner(exec, gitlab).execute(claim); }
    finally { restorePublish(); restoreFetch(); }
    assert.ok(events.includes("publish"), "a real checkpoint pack was confirmed published");
    assert.equal(api.states.some(s => s.runId === claim.run_id && s.body.status === "failed"), false);
    const completed = api.states.find(s => s.runId === claim.run_id && s.body.status === "completed");
    assert.ok(completed, "floor-tree-only confirmed publication permits report-only completion");
    assert.equal(completed.body.report_only, true);
    assert.equal(calls.length, 0, "no MR is opened");
  });

  // #3360: an issue run that declares report_only after publishing a checkpoint is delivered
  // as a NON-CLOSING merge request instead of failing (the #299 failure remains for an empty
  // candidate, a non-issue kind and a refused attempt; see runner-push-mr / runner-report-only-fallback).
  it("delivers a report_only completion non-closing after this worker published a checkpoint (#3360)", async () => {
    api.setRegisterProtocolFeatures(["refresh_publication_context_v1"]);
    await client.register("checkpoint");
    const forge = fakeGitlab();
    const { calls } = forge;
    followSuccessfulPush(git, forge.pr);
    const claim = gitlabClaim(67, { claim_generation: 7 });
    const events: string[] = [];
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        return { branch: ctx.branch, reportOnly: true, summary: "verified after milestone m1" };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, forge.gitlab).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    assert.ok(events.includes("publish"), "the mid-run checkpoint published (sanity)");
    assert.equal(api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed"), false,
      "report_only after substantive published work no longer fails");
    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    );
    assert.ok(completed, "the run completes through delivery");
    assert.notEqual(completed.body.report_only, true, "delivered as an MR, not a report-only completion");
    assert.equal(execFileSync("git", ["-C", fx.originPath, "rev-parse", "--verify", "refs/heads/agent/issue-67"],
      { encoding: "utf8" }).trim().length, 40, "the branch was pushed");
    const posts = calls.filter((c) => c.method === "POST");
    assert.equal(posts.length, 1, "exactly one MR opened");
    const description = String((JSON.parse(posts[0]!.body ?? "{}") as { description?: unknown }).description);
    assert.doesNotMatch(description, /Closes #67/);
    assert.match(description, /Issue remains open/);
    assert.deepEqual(completed.body.source_publication?.non_closing, true, "terminal proof records non-closing delivery");
  });
});

// PRD #1062 M2 (#1036): the mid-run overlay's default-tip fetch (git.fetchDefaultTip) is a
// PAT-bearing git op. REAP-BEFORE-GIT requires the agent tree be killed BEFORE it runs, so a
// survivor cannot read the PAT from the git child's /proc/<pid>/environ. The bug this pins:
// the reap was nested under `!tipUnmovedSinceFetch`, but the overlay is gated on opts.reap —
// so on the reachable state `tipUnmovedSinceFetch && hasNewWork && opts.reap` (a reap:false
// iteration fetched-but-didn't-publish with the gate closed, then a reap:true milestone fires
// with the tip unmoved) the reap was SKIPPED yet the overlay's PAT fetch still ran, agent alive.
describe("RunRunner — reap precedes the #1036 overlay's credentialed fetch (REAP-BEFORE-GIT, #1036)", () => {
  /** Spy on git.fetchDefaultTip (the overlay's ONLY PAT-bearing op), recording a
   *  "fetch-default-tip" event and then throwing so buildWorkflowOverlay fail-softs to realTip
   *  (GATE 1) — the event is what we assert on. Returns a restore fn (call it in a finally). */
  function spyFetchDefaultTip(events: string[]): () => void {
    const orig = git.fetchDefaultTip.bind(git);
    (git as unknown as { fetchDefaultTip: unknown }).fetchDefaultTip = async () => {
      events.push("fetch-default-tip");
      throw new Error("stub: credentialed default-tip fetch — recorded for ordering");
    };
    return () => {
      (git as unknown as { fetchDefaultTip: unknown }).fetchDefaultTip = orig;
    };
  }

  it("kills the agent tree BEFORE the overlay's PAT fetch on the reap:true + tip-unmoved + new-work path", async () => {
    const { github } = fakeGitHub();
    const { gitlab } = fakeGitlab();
    // GitHub forge so buildCheckpointOverlay yields a real overlay context (undefined off GitHub).
    const claim = gitlabClaim(93, {
      repo: {
        id: "r1",
        url: "https://github.com/org/repo",
        clone_url: fx.originPath,
        forge_type: "github",
      },
    });
    const events: string[] = [];
    let afterCheckpoint: string[] = [];
    let fakeNow = 0;
    const now = () => fakeNow;
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events);
    const restoreDefaultTip = spyFetchDefaultTip(events);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        // 1) reap:false with the time-gate CLOSED (now=0, interval=1000): fetches back (writes
        //    the tracking ref → tip now "unmoved") but does NOT publish, so lastPublishedTip
        //    stays undefined and the committed tip is still "new work".
        await ctx.checkpoint!({ reap: false, progress: { completed: [], in_progress: ["m1"] } });
        // 2) reap:true milestone, tip unmoved since (1): tipUnmovedSinceFetch === true AND
        //    hasNewWork === true — the exact state where the overlay's PAT fetch runs. The reap
        //    MUST have fired first.
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        afterCheckpoint = [...events];
        return { branch: ctx.branch };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, gitlab, undefined, {
        github,
        checkpointIntervalMs: 1000,
        now,
      }).execute(claim);
    } finally {
      restoreDefaultTip();
      restorePublish();
      restoreFetch();
    }
    const killIdx = afterCheckpoint.indexOf("kill");
    const patFetchIdx = afterCheckpoint.indexOf("fetch-default-tip");
    assert.ok(
      patFetchIdx >= 0,
      `the overlay's credentialed PAT fetch ran on this path; got ${JSON.stringify(afterCheckpoint)}`,
    );
    // On the UNFIXED structure (reap nested under !tipUnmovedSinceFetch) the reap is SKIPPED on
    // this tip-unmoved path, so killAgentTree is never called (killIdx === -1) and this fails.
    assert.ok(
      killIdx >= 0,
      `killAgentTree fired on the reap:true checkpoint; got ${JSON.stringify(afterCheckpoint)}`,
    );
    assert.ok(
      killIdx < patFetchIdx,
      `the reap ran BEFORE the overlay's credentialed fetch; got ${JSON.stringify(afterCheckpoint)}`,
    );
  });
});

// issue #1086 (F2 from #1036): the two-tip checkpoint reconciliation. The CONFIRMED tip
// (flight.lastCheckpointRefTip) advances only on a real ACK; an AMBIGUOUS publish (a non-2xx,
// or a throw after the pack tip is known) records the ATTEMPTED tip
// (flight.lastAttemptedCheckpointRefTip) so the NEXT overlay chains from it (`attempted ??
// confirmed`) and still descends the broker's actual ref if the prior push landed but its ACK
// was lost. These pin each reconciliation arm by driving publishCheckpointBestEffort /
// buildCheckpointOverlay directly (both private) through the same `as unknown as {...}` cast the
// spies in this file use, with git.checkpointPack and client.publishCheckpoint stubbed so the
// arm is exercised hermetically with no real pack/broker.
describe("issue #1086: two-tip checkpoint reconciliation (F2)", () => {
  interface TestFlight {
    runId: string;
    barePath: string;
    branch: string;
    claimGeneration: number;
    owedContext: Promise<OwedCandidateContext>;
    lastCheckpointRefTip: string | undefined;
    lastAttemptedCheckpointRefTip: string | undefined;
    reportedPublishOutcomes: Set<string>;
    runLog: Logger;
    batcher: { emit: (m: unknown) => void };
  }

  /** A minimal RunFlight carrying only the fields publishCheckpointBestEffort /
   *  buildCheckpointOverlay touch. Cast to `unknown` at the private-method seam. */
  function makeFlight(over: Partial<TestFlight> = {}): TestFlight {
    const log: Logger = {
      debug() {},
      info() {},
      warn() {},
      error() {},
      addSecret() {},
      removeSecret() {},
      child: () => log,
    };
    const context: OwedCandidateContext = {
      runId: "run-1086", branch: "branch", kind: "issue", barePath: "bare", generation: 9,
      defaultIdentity: { ref: "refs/remotes/origin/main", sha: "a".repeat(40) },
    };
    git.committedTrackingOwnership = async () => ({ kind: "owned", sha: "b".repeat(40), context });
    git.readRemediationRestrictionFacts = async (bare, branch, runId) => {
      assert.deepEqual([bare, branch, runId], ["bare", "branch", "run-1086"]);
      return { enrolled: false, blocked: false, roots: [], flaggedOIDs: [], fingerprint: "unit-confirmed-absence" };
    };
    return {
      runId: "run-1086",
      barePath: "bare",
      branch: "branch",
      claimGeneration: 9,
      owedContext: Promise.resolve(context),
      lastCheckpointRefTip: undefined,
      lastAttemptedCheckpointRefTip: undefined,
      reportedPublishOutcomes: new Set<string>(),
      runLog: log,
      batcher: { emit() {} },
      ...over,
    };
  }

  const noopExec: Executor = {
    run: async (ctx) => ({ branch: ctx.branch }),
    killAgentTree: () => {},
  };

  type PrivateRunner = {
    publishCheckpointBestEffort: (
      flight: unknown,
      barePath: string,
      branch: string,
      overlay?: unknown,
    ) => Promise<boolean>;
    buildCheckpointOverlay: (
      claim: unknown,
      flight: unknown,
      barePath: string,
    ) => Promise<{ prevCheckpointTip?: string } | undefined>;
  };

  /** Stub git.checkpointPack: return a fixed tip (pack is inert — the client is stubbed too),
   *  or throw to model a failure DURING the pack. Returns a restore fn. */
  function spyCheckpointPack(opts: { tipOid?: string; throws?: boolean }): () => void {
    const orig = git.checkpointPack.bind(git);
    (git as unknown as { checkpointPack: unknown }).checkpointPack = async () => {
      if (opts.throws) throw new Error("boom checkpointPack");
      const replay = async () => ({ pack: { readableEnded: true }, exited: Promise.resolve(0), cancel: () => {} });
      return { tipOid: opts.tipOid ?? "PACKTIP", ...await replay(), replay };
    };
    return () => {
      (git as unknown as { checkpointPack: unknown }).checkpointPack = orig;
    };
  }

  /** Stub client.publishCheckpoint to a fixed result, or throw. Returns a restore fn. */
  function spyPublishFixed(opts: { result?: unknown; throws?: boolean }): () => void {
    const orig = client.publishCheckpoint.bind(client);
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async () => {
      if (opts.throws) throw new Error("boom publish");
      return opts.result;
    };
    return () => {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
    };
  }

  it("(a) a CONFIRMED publish advances the confirmed tip AND clears the attempted tip", async () => {
    const { gitlab } = fakeGitlab();
    // A pending attempted tip (sentinel) must be reconciled away by the confirmed publish.
    const flight = makeFlight({
      lastCheckpointRefTip: "OLD_CONFIRMED",
      lastAttemptedCheckpointRefTip: "PENDING_ATTEMPT",
    });
    const restorePack = spyCheckpointPack({ tipOid: "NEW_TIP" });
    const restorePub = spyPublishFixed({
      result: { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } },
    });
    let ok: boolean;
    try {
      ok = await (runner(noopExec, gitlab) as unknown as PrivateRunner).publishCheckpointBestEffort(
        flight,
        "bare",
        "branch",
        undefined,
      );
    } finally {
      restorePub();
      restorePack();
    }
    assert.equal(ok, true, "a confirmed publish returns true");
    assert.equal(flight.lastCheckpointRefTip, "NEW_TIP", "the confirmed tip advanced to the packed tip");
    assert.equal(
      flight.lastAttemptedCheckpointRefTip,
      undefined,
      "the confirmed publish reconciled/cleared the attempted tip",
    );
  });

  it("(b) a 2xx explicit skip advances neither tip", async () => {
    const { gitlab } = fakeGitlab();
    const flight = makeFlight({
      lastCheckpointRefTip: "OLD_CONFIRMED",
      lastAttemptedCheckpointRefTip: "PRIOR_ATTEMPT",
    });
    const restorePack = spyCheckpointPack({ tipOid: "SKIPPED_TIP" });
    const restorePub = spyPublishFixed({
      result: { ok: true, body: { published: false, ref: "", skipped: "workflow_scope" } },
    });
    let ok: boolean;
    try {
      ok = await (runner(noopExec, gitlab) as unknown as PrivateRunner).publishCheckpointBestEffort(
        flight,
        "bare",
        "branch",
        undefined,
      );
    } finally {
      restorePub();
      restorePack();
    }
    assert.equal(ok, false, "a skip returns false");
    assert.equal(flight.lastCheckpointRefTip, "OLD_CONFIRMED", "a definitive skip does not advance the confirmed tip");
    assert.equal(
      flight.lastAttemptedCheckpointRefTip,
      "PRIOR_ATTEMPT",
      "a definitive skip records nothing as attempted (the server did not advance its ref)",
    );
  });

  it("(c) an ambiguous result records the attempted tip; a throw during the pack records nothing", async () => {
    const { gitlab } = fakeGitlab();

    // non-2xx: the broker may have accepted before the ACK was lost → record the attempted tip.
    {
      const flight = makeFlight({ lastCheckpointRefTip: "OLD_CONFIRMED" });
      const restorePack = spyCheckpointPack({ tipOid: "ATTEMPT_500" });
      const restorePub = spyPublishFixed({ result: { ok: false, httpStatus: 500 } });
      try {
        await (runner(noopExec, gitlab) as unknown as PrivateRunner).publishCheckpointBestEffort(
          flight,
          "bare",
          "branch",
          undefined,
        );
      } finally {
        restorePub();
        restorePack();
      }
      assert.equal(flight.lastAttemptedCheckpointRefTip, "ATTEMPT_500", "a non-2xx records the attempted tip");
      assert.equal(flight.lastCheckpointRefTip, "OLD_CONFIRMED", "a non-2xx does not advance the confirmed tip");
    }

    // thrown AFTER the pack tip is known: still ambiguous → record the attempted tip.
    {
      const flight = makeFlight({ lastCheckpointRefTip: "OLD_CONFIRMED" });
      const restorePack = spyCheckpointPack({ tipOid: "ATTEMPT_THROW" });
      const restorePub = spyPublishFixed({ throws: true });
      try {
        await (runner(noopExec, gitlab) as unknown as PrivateRunner).publishCheckpointBestEffort(
          flight,
          "bare",
          "branch",
          undefined,
        );
      } finally {
        restorePub();
        restorePack();
      }
      assert.equal(
        flight.lastAttemptedCheckpointRefTip,
        "ATTEMPT_THROW",
        "a throw after the pack tip is known records the attempted tip",
      );
      assert.equal(flight.lastCheckpointRefTip, "OLD_CONFIRMED", "a post-pack throw does not advance the confirmed tip");
    }

    // thrown DURING checkpointPack: packedTip never obtained → neither field moves.
    {
      const flight = makeFlight({
        lastCheckpointRefTip: "OLD_CONFIRMED",
        lastAttemptedCheckpointRefTip: "PRIOR_ATTEMPT",
      });
      const restorePack = spyCheckpointPack({ throws: true });
      const restorePub = spyPublishFixed({ throws: true });
      try {
        await (runner(noopExec, gitlab) as unknown as PrivateRunner).publishCheckpointBestEffort(
          flight,
          "bare",
          "branch",
          undefined,
        );
      } finally {
        restorePub();
        restorePack();
      }
      assert.equal(
        flight.lastAttemptedCheckpointRefTip,
        "PRIOR_ATTEMPT",
        "a throw during checkpointPack records nothing as attempted",
      );
      assert.equal(flight.lastCheckpointRefTip, "OLD_CONFIRMED", "a throw during checkpointPack does not advance the confirmed tip");
    }
  });

  it("scratch preflight refusal names the class and keeps both reconciliation tips", async () => {
    const { gitlab } = fakeGitlab();
    const flight = makeFlight({
      lastCheckpointRefTip: "CONFIRMED",
      lastAttemptedCheckpointRefTip: "ATTEMPTED",
    });
    const emitted: string[] = [];
    flight.batcher.emit = (message) => {
      const status = message as { payload?: { text?: string } };
      if (status.payload?.text) emitted.push(status.payload.text);
    };
    const originalPack = git.checkpointPack.bind(git);
    const originalPublish = client.publishCheckpoint.bind(client);
    let publishes = 0;
    git.checkpointPack = (async () => {
      throw new ScratchPublicationError("remote text with credential");
    }) as typeof git.checkpointPack;
    client.publishCheckpoint = (async (...args: Parameters<typeof client.publishCheckpoint>) => {
      publishes++;
      return originalPublish(...args);
    }) as typeof client.publishCheckpoint;
    try {
      const subject = runner(noopExec, gitlab) as unknown as {
        publishCheckpointOutcome: (flight: unknown, bare: string, branch: string) => Promise<unknown>;
      };
      assert.deepEqual(await subject.publishCheckpointOutcome(flight, "bare", "branch"), {
        published: false, reason: "scratch_publication_refused",
      });
    } finally {
      git.checkpointPack = originalPack;
      client.publishCheckpoint = originalPublish;
    }
    assert.equal(publishes, 0);
    assert.equal(flight.lastCheckpointRefTip, "CONFIRMED");
    assert.equal(flight.lastAttemptedCheckpointRefTip, "ATTEMPTED");
    assert.deepEqual(emitted, ["checkpoint publish failed: scratch_publication_refused (exec_failed)"]);
  });

  it("M1 allowlists every refusal kind and step, and normalizes hostile metadata before feed and dedupe", async () => {
    const { gitlab } = fakeGitlab();
    const flight = makeFlight({ lastCheckpointRefTip: "CONFIRMED", lastAttemptedCheckpointRefTip: "ATTEMPTED" });
    const feed: string[] = [];
    flight.batcher.emit = (m) => feed.push((m as { payload: { text: string } }).payload.text);
    const kinds: ScratchPublicationKind[] = [
      "tip_unavailable", "shallow_history", "missing_objects", "object_walk_failed",
      "exec_failed", "scratch_present", "floor_unverified", "checkpoint_range",
      "remote_changed_during_refresh", "remote_candidate_diverged",
      "new_remote_candidate_diverged", "remote_branch_advanced",
    ];
    const steps: Array<ScratchPublicationStep | undefined> = [
      "resolve_tip", "shallow_check", "object_walk", "scratch_walk", "floor_refresh", "checkpoint_floor", undefined,
    ];
    const originalPack = git.checkpointPack.bind(git);
    const originalPublish = client.publishCheckpoint.bind(client);
    let refusal = new ScratchPublicationError("untrusted remote text");
    let uploads = 0;
    git.checkpointPack = async () => { throw refusal; };
    client.publishCheckpoint = async () => { uploads++; throw new Error("unexpected upload"); };
    const subject = runner(noopExec, gitlab) as unknown as {
      publishCheckpointOutcome: (f: unknown, b: string, name: string) => Promise<unknown>;
    };
    try {
      for (const kind of kinds) for (const step of steps) {
        refusal = new ScratchPublicationError("untrusted remote text", new Error("credential text"), { kind, step });
        const pair = `${kind}${step === undefined ? "" : ` at ${step}`}`;
        const before = feed.length;
        for (let attempt = 0; attempt < 2; attempt++) {
          assert.deepEqual(await subject.publishCheckpointOutcome(flight, "bare", "branch"), {
            published: false, reason: "scratch_publication_refused",
          });
        }
        assert.equal(feed.length, before + 1, `distinct pair ${pair}, repeated once`);
        assert.equal(feed.at(-1), `checkpoint publish failed: scratch_publication_refused (${pair})`);
        assert.ok(flight.reportedPublishOutcomes.has(`scratch_publication_refused:${pair}`));
      }
      // No coercion: even prototype names and objects with hostile stringification are unknown.
      const hostile: unknown[] = ["remote secret\ntext", "constructor", "__proto__", "toString", "", undefined, null, 42, false,
        { toString() { throw new Error("must not coerce metadata"); } }, ["scratch_present"], Symbol("secret")];
      for (const value of hostile) {
        refusal = new ScratchPublicationError("untrusted remote text", undefined, {
          kind: value as ScratchPublicationKind, step: value as ScratchPublicationStep,
        });
        // The constructor defaults null/undefined kinds; exercise malformed runtime fields too.
        Object.defineProperty(refusal, "kind", { value });
        flight.reportedPublishOutcomes.delete("scratch_publication_refused:other");
        const before = feed.length;
        await subject.publishCheckpointOutcome(flight, "bare", "branch");
        assert.equal(feed.length, before + 1, "each malformed runtime pair emits its normalized code");
        assert.equal(feed.at(-1), "checkpoint publish failed: scratch_publication_refused (other)");
        assert.ok(flight.reportedPublishOutcomes.has("scratch_publication_refused:other"));
      }
      assert.equal(flight.reportedPublishOutcomes.size, kinds.length * steps.length + 1, "unknown pairs share one normalized key");
      refusal = new ScratchPublicationError("secret", undefined, { kind: "scratch_present", step: "constructor" as ScratchPublicationStep });
      await subject.publishCheckpointOutcome(flight, "bare", "branch");
      assert.equal(feed.length, kinds.length * steps.length + hostile.length, "invalid step equals absent step");
      refusal = new ScratchPublicationError("secret", undefined, { kind: "constructor" as ScratchPublicationKind, step: "floor_refresh" });
      await subject.publishCheckpointOutcome(flight, "bare", "branch");
      assert.equal(feed.at(-1), "checkpoint publish failed: scratch_publication_refused (other at floor_refresh)");
      assert.equal(uploads, 0);
      assert.equal(flight.lastCheckpointRefTip, "CONFIRMED");
      assert.equal(flight.lastAttemptedCheckpointRefTip, "ATTEMPTED");
    } finally {
      git.checkpointPack = originalPack;
      client.publishCheckpoint = originalPublish;
    }
  });

  it("M1 snapshots changing runtime accessors before checkpoint feed and dedupe allowlists", async () => {
    const { gitlab } = fakeGitlab();
    const subject = runner(noopExec, gitlab) as unknown as {
      publishCheckpointOutcome: (f: unknown, b: string, name: string) => Promise<unknown>;
    };
    const originalPack = git.checkpointPack.bind(git);
    const originalPublish = client.publishCheckpoint.bind(client);
    let uploads = 0;
    client.publishCheckpoint = async () => { uploads++; throw new Error("unexpected upload"); };
    try {
      for (const changing of ["kind", "step", "both"]) {
        const flight = makeFlight();
        const feed: string[] = [];
        flight.batcher.emit = (m) => feed.push((m as { payload: { text: string } }).payload.text);
        const hostile = "hostile remote metadata";
        let kindReads = 0;
        let stepReads = 0;
        const refusal = new ScratchPublicationError("remote text");
        // logScratchPublicationRefused reads first; the diagnostic must snapshot the second read.
        Object.defineProperties(refusal, {
          kind: { get() {
            kindReads++;
            return kindReads <= 2 || changing === "step" ? "floor_unverified" : hostile;
          } },
          step: { get() {
            stepReads++;
            return stepReads <= 2 || changing === "kind" ? "floor_refresh" : hostile;
          } },
        });
        git.checkpointPack = async () => { throw refusal; };
        assert.deepEqual(await subject.publishCheckpointOutcome(flight, "bare", "branch"), {
          published: false, reason: "scratch_publication_refused",
        });
        assert.deepEqual(feed, [
          "checkpoint publish failed: scratch_publication_refused (floor_unverified at floor_refresh)",
        ], changing);
        assert.deepEqual([...flight.reportedPublishOutcomes], [
          "scratch_publication_refused:floor_unverified at floor_refresh",
        ], changing);
        assert.ok(!JSON.stringify([feed, [...flight.reportedPublishOutcomes]]).includes(hostile), changing);
        assert.equal(kindReads, 2, `${changing}: kind read once by log and once by diagnostic`);
        assert.equal(stepReads, 2, `${changing}: step read once by log and once by diagnostic`);
      }
      assert.equal(uploads, 0);
    } finally {
      git.checkpointPack = originalPack;
      client.publishCheckpoint = originalPublish;
    }
  });

  it("M1 shares refusal dedupe across bridge and publish and resets only after confirmed success", async () => {
    const { gitlab } = fakeGitlab();
    const flight = makeFlight();
    const feed: string[] = [];
    flight.batcher.emit = (m) => feed.push((m as { payload: { text: string } }).payload.text);
    const subject = runner(noopExec, gitlab) as unknown as {
      bridgeBareTrackingRefIfDivergent: () => Promise<unknown>;
      bridgeParkSinkBestEffort: (b: string, name: string, f: unknown, log: Logger, sink: string) => Promise<unknown>;
      publishCheckpointOutcome: (f: unknown, b: string, name: string) => Promise<unknown>;
    };
    const refusal = new ScratchPublicationError("remote text", undefined, { kind: "floor_unverified", step: "floor_refresh" });
    subject.bridgeBareTrackingRefIfDivergent = async () => { throw refusal; };
    const originalPack = git.checkpointPack.bind(git);
    const originalPublish = client.publishCheckpoint.bind(client);
    git.checkpointPack = async () => { throw refusal; };
    try {
      assert.deepEqual(await subject.bridgeParkSinkBestEffort("bare", "branch", flight, flight.runLog, "park"), { kind: "failed" });
      await subject.publishCheckpointOutcome(flight, "bare", "branch");
      const expected = "checkpoint publish failed: scratch_publication_refused (floor_unverified at floor_refresh)";
      assert.deepEqual(feed, [expected], "same pair across catch sites shares a key");
      const restorePack = spyCheckpointPack({});
      const restorePublish = spyPublishFixed({ result: { ok: true, body: { published: false, skipped: "busy" } } });
      try {
        await subject.publishCheckpointOutcome(flight, "bare", "branch");
        assert.ok(flight.reportedPublishOutcomes.has("scratch_publication_refused:floor_unverified at floor_refresh"), "unconfirmed skip retains refusal dedupe");
        client.publishCheckpoint = async () => ({ ok: true, body: { published: true, ref: "ref" } });
        assert.deepEqual(await subject.publishCheckpointOutcome(flight, "bare", "branch"), { published: true });
      } finally { restorePublish(); restorePack(); }
      assert.equal(flight.reportedPublishOutcomes.size, 0, "confirmed success clears dedupe");
      await subject.publishCheckpointOutcome(flight, "bare", "branch");
      assert.equal(feed.filter((line) => line === expected).length, 2, "refusal recurs after confirmed success");
    } finally {
      git.checkpointPack = originalPack;
      client.publishCheckpoint = originalPublish;
    }
  });

  it("(d) accepted-publish + lost-response + new-milestone: the next overlay chains from the ATTEMPTED tip, not the stale confirmed tip", async () => {
    const { gitlab } = fakeGitlab();
    // A GitHub claim so buildCheckpointOverlay yields a real overlay context; default_branch is
    // set so it never reaches the credentialed git.defaultBranchName probe.
    const claim = gitlabClaim(1086, {
      repo: {
        id: "r1",
        url: "https://github.com/org/repo",
        clone_url: fx.originPath,
        forge_type: "github",
        default_branch: "main",
      },
    });
    const flight = makeFlight({ lastCheckpointRefTip: "CONFIRMED_0" });

    // 1) An ambiguous first publish (accepted by the broker, ACK lost → non-2xx) records the
    //    attempted tip.
    const restorePack = spyCheckpointPack({ tipOid: "ATTEMPTED_1" });
    const restorePub = spyPublishFixed({ result: { ok: false, httpStatus: 502 } });
    try {
      await (runner(noopExec, gitlab) as unknown as PrivateRunner).publishCheckpointBestEffort(
        flight,
        "bare",
        "branch",
        undefined,
      );
    } finally {
      restorePub();
      restorePack();
    }
    assert.equal(flight.lastAttemptedCheckpointRefTip, "ATTEMPTED_1", "the ambiguous publish recorded the attempted tip");

    // 2) The NEXT overlay must chain from the attempted tip (precedence attempted ?? confirmed),
    //    NOT the stale confirmed tip — this is the whole bug the fix closes.
    const overlay = await (
      runner(noopExec, gitlab) as unknown as PrivateRunner
    ).buildCheckpointOverlay(claim, flight, "bare");
    assert.ok(overlay, "a GitHub claim yields an overlay context");
    assert.equal(
      overlay!.prevCheckpointTip,
      "ATTEMPTED_1",
      "the next overlay chains from the ATTEMPTED tip, not the stale confirmed tip",
    );

    // Control: with no attempted tip pending, the overlay falls back to the confirmed tip.
    const cleared = makeFlight({ lastCheckpointRefTip: "CONFIRMED_0" });
    const overlay2 = await (
      runner(noopExec, gitlab) as unknown as PrivateRunner
    ).buildCheckpointOverlay(claim, cleared, "bare");
    assert.equal(
      overlay2!.prevCheckpointTip,
      "CONFIRMED_0",
      "with no attempted tip pending, the overlay uses the confirmed tip",
    );
  });
});

describe("RunRunner — milestones_agents transport (PRD #1224 M1)", () => {
  // A declared per-milestone agent attribution must actually reach the api on ALL THREE
  // report paths. An optional field silently dropped from one projection in runner.ts is
  // exactly the failure these guard: each asserts the mapping appears in the reportState
  // body captured by the fake api (a real JSON round-trip over the fake HTTP transport).
  const AGENTS = [
    { id: "m2", agent: "coder", agent_label: "backend" },
    { id: "m3", agent: "reviewer" },
  ];

  it("the immediate reportProgress push carries milestones_agents", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(95);
    const exec: Executor = {
      run: async (ctx) => {
        await ctx.reportProgress!({
          completed: [],
          in_progress: ["m2", "m3"],
          milestones_agents: AGENTS,
        });
        // Serialize behind the per-run chain so the push has landed on the api.
        await ctx.reportIteration!(1, undefined);
        return { branch: ctx.branch };
      },
      killAgentTree: () => {},
    };
    await runner(exec, gitlab).execute(claim);

    const push = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body)
      .find((b) => b.status === "running" && !("iteration_count" in b) && Array.isArray(b.milestones_agents));
    assert.ok(push, "an immediate running report carried milestones_agents");
    assert.deepStrictEqual(push!.milestones_agents, AGENTS);
  });

  it("the iteration running report carries milestones_agents", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(96);
    const exec: Executor = {
      run: async (ctx) => {
        await ctx.reportIteration!(1, {
          completed: [],
          in_progress: ["m2", "m3"],
          milestones_agents: AGENTS,
        });
        return { branch: ctx.branch };
      },
      killAgentTree: () => {},
    };
    await runner(exec, gitlab).execute(claim);

    const iter = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body)
      .find((b) => b.status === "running" && b.iteration_count === 1);
    assert.ok(iter, "the turn-boundary running report was sent");
    assert.deepStrictEqual(iter!.milestones_agents, AGENTS);
  });

  it("the checkpoint report carries milestones_agents", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(97);
    const exec: Executor = {
      run: async (ctx) => {
        commitInClone(ctx.worktreePath, "NEW.txt");
        await ctx.checkpoint!({
          reap: true,
          progress: {
            completed: ["m1"],
            in_progress: ["m2", "m3"],
            milestones_agents: AGENTS,
          },
        });
        return { branch: ctx.branch };
      },
      killAgentTree: () => {},
    };
    await runner(exec, gitlab).execute(claim);

    const report = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body)
      .find((b) => b.status === "running" && Array.isArray(b.milestones_agents));
    assert.ok(report, "a checkpoint running report carried milestones_agents");
    assert.deepStrictEqual(report!.milestones_agents, AGENTS);
    assert.deepStrictEqual(report!.milestones_completed, ["m1"]);
    assert.deepStrictEqual(report!.milestones_in_progress, ["m2", "m3"]);
  });
});

// #3944: the claim fetch mirrors origin's refs/uzi-checkpoints/* into the worker bare and never
// prunes it, so a cancelled predecessor's work can outlive its custody on origin. A report-only
// run whose own (lost-ack) publish was content-free must not be failed by that stale mirror, while
// anything that could hold work (a present ref, a failed probe, an unreadable proof, a substantive
// attempted tip, a non-issue run) keeps blocking.
describe("RunRunner — report_only orphan guard vs a stale local checkpoint mirror (#3944)", () => {
  const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
  const ORPHAN_REASON = /published committed work to a checkpoint ref/;
  let indexSeq = 0;

  function g(dir: string, args: string[]): string {
    return execFileSync("git", ["-C", dir, ...IDENT, ...args], { env: ENV, encoding: "utf8" }).trim();
  }

  /** A commit in the fixture origin built with plumbing, so no branch or checkout is touched. */
  function originCommit(parent: string, file: string, content: string, subject: string): string {
    const idx = path.join(fx.originPath, ".git", `tmp-index-3944-${indexSeq++}`);
    const env = { ...ENV, GIT_INDEX_FILE: idx };
    execFileSync("git", ["-C", fx.originPath, "read-tree", parent], { env });
    const blob = execFileSync("git", ["-C", fx.originPath, "hash-object", "-w", "--stdin"],
      { env: ENV, input: content, encoding: "utf8" }).trim();
    execFileSync("git", ["-C", fx.originPath, "update-index", "--add", "--cacheinfo", `100644,${blob},${file}`], { env });
    const tree = execFileSync("git", ["-C", fx.originPath, "write-tree"], { env, encoding: "utf8" }).trim();
    fs.rmSync(idx, { force: true });
    return g(fx.originPath, ["commit-tree", tree, "-p", parent, "-m", subject]);
  }

  /** A cancelled predecessor's work-bearing checkpoint on origin (three commits above the initial
   *  commit) that is NOT an ancestor of main, which then moves on. */
  function seedPredecessor(n: number, ref = `refs/uzi-checkpoints/agent/issue-${n}`): { tip: string; base: string; ref: string } {
    const base = g(fx.originPath, ["rev-parse", "refs/heads/main"]);
    let tip = base;
    for (const i of [1, 2, 3]) tip = originCommit(tip, `predecessor-${i}.txt`, `predecessor work ${i}\n`, `predecessor milestone ${i}`);
    g(fx.originPath, ["update-ref", ref, tip]);
    fs.writeFileSync(path.join(fx.originPath, "main-advance.txt"), "main moved on\n");
    g(fx.originPath, ["add", "main-advance.txt"]);
    g(fx.originPath, ["commit", "-m", "main advances"]);
    return { tip, base, ref };
  }

  /** Every refs/uzi-* ref (checkpoints, held, recovery, runner, incoming...) with its object. */
  function snap(repo: string): string {
    return g(repo, ["for-each-ref", "--format=%(refname) %(objectname)"])
      .split("\n").filter((line) => line.startsWith("refs/uzi-")).join("\n");
  }

  function rmObject(bare: string, oid: string): void {
    fs.unlinkSync(path.join(bare, "objects", oid.slice(0, 2), oid.slice(2)));
  }

  interface DriveOpts {
    n: number;
    kind?: "prompt";
    /** Run ctx.checkpoint with a publish whose ack is lost (default true). */
    checkpoint?: boolean;
    reportOnly?: boolean;
    /** Replace the default content-free coverage commit. */
    commit?: (dir: string, bare: string) => void;
    /** Runs inside the executor right after the checkpoint (or the commit when none). */
    inExecutor?: (bare: string, dir: string) => void;
    /** Runs inside the executor before the commit and checkpoint. */
    beforeWork?: (bare: string, dir: string) => void;
    delivery?: boolean;
    /** Return an operator scope-capped zero slice instead of a report_only result. */
    scopeCapped?: boolean;
    /** Runs after the claim is built and before it executes. */
    prepare?: (claim: { run_id: string }) => void;
  }

  async function drive(opts: DriveOpts) {
    api.setRegisterProtocolFeatures(["refresh_publication_context_v1"]);
    await client.register("checkpoint");
    const forge = fakeGitlab();
    if (opts.delivery) followSuccessfulPush(git, forge.pr);
    const claim = gitlabClaim(opts.n, { claim_generation: 7, ...(opts.kind ? { kind: opts.kind } : {}) });
    opts.prepare?.(claim);
    const bare = git.barePathFor(fx.originPath);
    const events: string[] = [];
    const snapshots: { bare: string; origin: string } = { bare: "", origin: "" };
    const restoreFetch = spyFetch(events);
    const restorePublish = spyPublish(events, { throws: true });
    const exec: Executor = {
      run: async (ctx) => {
        opts.beforeWork?.(bare, ctx.worktreePath);
        (opts.commit ?? ((dir: string) => {
          g(dir, ["commit", "--allow-empty", "-m", "uzi recovery coverage fixture round 0 batch 0"]);
        }))(ctx.worktreePath, bare);
        if (opts.checkpoint !== false) await ctx.checkpoint!({ reap: true });
        opts.inExecutor?.(bare, ctx.worktreePath);
        snapshots.bare = snap(bare);
        snapshots.origin = snap(fx.originPath);
        if (opts.scopeCapped) {
          return { branch: ctx.branch, scopeCapped: { completedCount: 0, total: 3 }, summary: "scope capped zero slice" };
        }
        return opts.reportOnly === false
          ? { branch: ctx.branch }
          : { branch: ctx.branch, reportOnly: true, summary: "stale mirror report" };
      },
      killAgentTree: () => events.push("kill"),
    };
    try {
      await runner(exec, forge.gitlab).execute(claim);
    } finally {
      restorePublish();
      restoreFetch();
    }
    const states = api.states.filter((s) => s.runId === claim.run_id);
    return {
      claim, bare, events, snapshots, calls: forge.calls,
      completed: states.find((s) => s.body.status === "completed")?.body,
      failed: states.find((s) => s.body.status === "failed")?.body,
      after: { bare: snap(bare), origin: snap(fx.originPath) },
    };
  }

  /** The run completed report-only, opened no MR and made no call besides state reports. */
  function assertReportOnlyCompletion(res: Awaited<ReturnType<typeof drive>>): void {
    assert.equal(res.failed, undefined, `no failure expected, got ${JSON.stringify(res.failed)}`);
    assert.ok(res.completed, "the run completed");
    assert.equal(res.completed.report_only, true);
    assert.equal(res.calls.length, 0, "no MR is opened");
    assert.ok(api.requestLog.every((entry) => entry === "messages" || entry.startsWith("state:")),
      `no custody, settle or discard request: ${JSON.stringify(api.requestLog)}`);
  }

  /** Fail-closed by an earlier guard (a broken object store fails the run before the orphan guard). */
  function assertRefused(res: Awaited<ReturnType<typeof drive>>): void {
    assert.ok(res.failed, "the run failed closed");
    assert.equal(res.completed, undefined);
    assert.equal(res.calls.length, 0, "no MR is opened");
  }

  function assertOrphanFailure(res: Awaited<ReturnType<typeof drive>>): void {
    assert.ok(res.failed, "the run failed to avoid orphaning the checkpoint");
    assert.match(res.failed.failure_reason ?? "", ORPHAN_REASON);
    assert.equal(res.completed, undefined);
    assert.equal(res.calls.length, 0, "no MR is opened");
  }

  it("r1 (incident) completes report-only when the stale mirror's origin ref is gone and the attempted tip is content-free", async () => {
    const pred = seedPredecessor(39441);
    const res = await drive({
      n: 39441,
      inExecutor: () => g(fx.originPath, ["update-ref", "-d", pred.ref]),
    });
    assert.ok(res.events.includes("publish"), "the checkpoint publish was attempted (and its ack lost)");
    assert.equal(g(res.bare, ["rev-parse", pred.ref]), pred.tip, "the worker mirror still names the predecessor tip");
    assertReportOnlyCompletion(res);
    assert.ok(res.snapshots.bare.includes(`${pred.ref} ${pred.tip}`), "the snapshot is not vacuous");
    assert.equal(res.after.bare, res.snapshots.bare, "the bare's checkpoint, held, recovery and runner refs are untouched");
    assert.equal(res.after.origin, res.snapshots.origin, "origin's refs are untouched");
  });

  it("r1c hands the claim's credentials and clone url to the origin probe", async () => {
    const pred = seedPredecessor(39461);
    const target = git as unknown as { originCheckpointRefState: (...a: unknown[]) => Promise<unknown> };
    const original = target.originCheckpointRefState.bind(git);
    const seen: unknown[][] = [];
    target.originCheckpointRefState = (...args: unknown[]) => {
      seen.push(args);
      return original(...args);
    };
    const res = await drive({
      n: 39461,
      prepare: (claim) => {
        const secrets = (claim as unknown as { secrets: Record<string, string> }).secrets;
        secrets.forge_pat = "fixture-pat-value";
        secrets.forge_username = "fixture-user-value";
      },
      inExecutor: () => g(fx.originPath, ["update-ref", "-d", pred.ref]),
    });
    assertReportOnlyCompletion(res);
    assert.equal(seen.length, 1, "the origin probe ran once");
    assert.equal(seen[0]![1], pred.ref);
    assert.deepEqual(seen[0]![2], {
      pat: "fixture-pat-value",
      repoUrl: fx.originPath,
      username: "fixture-user-value",
    });
  });

  it("r1d scope-capped zero slice completes (scope_capped) when the stale mirror is absent on origin", async () => {
    const pred = seedPredecessor(39462);
    const res = await drive({
      n: 39462,
      scopeCapped: true,
      inExecutor: () => g(fx.originPath, ["update-ref", "-d", pred.ref]),
    });
    assert.equal(res.failed, undefined, `no failure expected, got ${JSON.stringify(res.failed)}`);
    assert.ok(res.completed, "the run completed");
    assert.equal(res.completed.report_only, true);
    assert.equal(res.completed.scope_capped, true);
    assert.equal(res.calls.length, 0, "no MR is opened");
  });

  it("r1e scope-capped zero slice does not take the no-MR completion over a content-bearing lost-ack attempted tip", async () => {
    const res = await drive({
      n: 39463,
      scopeCapped: true,
      delivery: true,
      commit: (dir) => {
        commitInClone(dir, "HIDDEN.txt");
        g(dir, ["revert", "--no-edit", "HEAD"]);
      },
    });
    assert.ok(res.events.includes("publish"));
    // Falls through to the normal push + (partial) MR delivery, not the no-MR zero-slice completion.
    assert.equal(res.failed, undefined);
    assert.ok(res.completed, "the run completes through delivery");
    assert.notEqual(res.completed.report_only, true, "not the MR-less scope-capped report-only completion");
    assert.equal(res.calls.filter((c) => c.method === "POST").length, 1, "exactly one MR opened");
  });

  it("r1s keeps the predecessor tip under origin's recovery ref untouched while completing", async () => {
    const pred = seedPredecessor(39442);
    const recovery = "refs/uzi-recovery/predecessor-run";
    const res = await drive({
      n: 39442,
      inExecutor: () => {
        g(fx.originPath, ["update-ref", "-d", pred.ref]);
        g(fx.originPath, ["update-ref", recovery, pred.tip]);
      },
    });
    assertReportOnlyCompletion(res);
    assert.equal(g(fx.originPath, ["rev-parse", recovery]), pred.tip);
    assert.equal(res.after.origin, res.snapshots.origin);
    assert.equal(res.after.bare, res.snapshots.bare);
  });

  it("r2 still fails (#299) when the predecessor ref is still on origin; refs are preserved", async () => {
    const pred = seedPredecessor(39443);
    const res = await drive({ n: 39443 });
    assertOrphanFailure(res);
    assert.equal(g(fx.originPath, ["rev-parse", pred.ref]), pred.tip);
    assert.equal(g(res.bare, ["rev-parse", pred.ref]), pred.tip);
    assert.equal(res.after.bare, res.snapshots.bare);
    assert.equal(res.after.origin, res.snapshots.origin);
  });

  it("r3 delivers a lost-ack content-bearing attempted tip non-closing (#3360) although no mirror exists", async () => {
    const res = await drive({
      n: 39444,
      delivery: true,
      commit: (dir) => {
        commitInClone(dir, "LOST_ACK.txt");
      },
    });
    assert.ok(res.events.includes("publish"));
    assert.equal(res.failed, undefined);
    assert.ok(res.completed, "the run completes through delivery");
    assert.notEqual(res.completed.report_only, true, "delivered as an MR, not a report-only completion");
    assert.equal(res.calls.filter((c) => c.method === "POST").length, 1, "exactly one MR opened");
    assert.equal(res.completed.source_publication?.non_closing, true);
  });

  it("r4 fails (#299) when the origin probe fails", async () => {
    seedPredecessor(39445);
    const res = await drive({
      n: 39445,
      inExecutor: (bare) => {
        g(fx.originPath, ["update-ref", "-d", "refs/uzi-checkpoints/agent/issue-39445"]);
        g(bare, ["config", "remote.origin.url", path.join(fx.dataDir, "no-such-origin")]);
      },
    });
    assertOrphanFailure(res);
  });

  it("r4b fails (#299) when the origin probe answers unknown", async () => {
    seedPredecessor(39446);
    (git as unknown as { originCheckpointRefState: () => Promise<string> }).originCheckpointRefState = async () => "unknown";
    const res = await drive({
      n: 39446,
      inExecutor: () => g(fx.originPath, ["update-ref", "-d", "refs/uzi-checkpoints/agent/issue-39446"]),
    });
    assertOrphanFailure(res);
  });

  it("r5a fails (#299, empty candidate) when an attempted tip hides content behind an equal tip tree", async () => {
    const res = await drive({
      n: 39447,
      commit: (dir) => {
        commitInClone(dir, "HIDDEN.txt");
        g(dir, ["revert", "--no-edit", "HEAD"]);
      },
    });
    assertOrphanFailure(res);
  });

  it("r5b fails (#299, empty candidate) when the attempted tip is a two-parent wip(park) look-alike over substantive history", async () => {
    const res = await drive({
      n: 39448,
      commit: (dir) => {
        const base = g(dir, ["rev-parse", "HEAD"]);
        commitInClone(dir, "SIDE.txt");
        const side = g(dir, ["rev-parse", "HEAD"]);
        const lookAlike = g(dir, ["commit-tree", `${base}^{tree}`, "-p", base, "-p", side, "-m", `${WIP_PARK_COMMIT_PREFIX} forged`]);
        g(dir, ["reset", "--hard", lookAlike]);
      },
    });
    assertOrphanFailure(res);
  });

  it("r5c fails (#299) when the attempted tip is unreadable", async () => {
    const res = await drive({
      n: 39449,
      // The publish reports a tip the bare does not hold, as a lost ack of an unverifiable push would.
      beforeWork: () => {
        const real = git.checkpointPack.bind(git);
        (git as unknown as { checkpointPack: unknown }).checkpointPack = async (...args: Parameters<typeof real>) => {
          const packed = await real(...args);
          return packed && { ...packed, tipOid: "a".repeat(40) };
        };
      },
    });
    assertOrphanFailure(res);
  });

  it("r6a fails (#299) with every recovery, held and mirror ref byte-identical when the predecessor publish lands before the probe", async () => {
    const pred = seedPredecessor(39450);
    g(fx.originPath, ["update-ref", "refs/uzi-held/predecessor-run/1", pred.tip]);
    g(fx.originPath, ["update-ref", "refs/uzi-recovery/predecessor-run", pred.tip]);
    const res = await drive({
      n: 39450,
      inExecutor: (bare) => {
        g(bare, ["update-ref", "refs/uzi-held/predecessor-run/1", pred.tip]);
        g(bare, ["update-ref", "refs/uzi-recovery/predecessor-run", pred.tip]);
      },
    });
    assertOrphanFailure(res);
    assert.equal(res.after.bare, res.snapshots.bare);
    assert.equal(res.after.origin, res.snapshots.origin);
    assert.match(res.after.bare, /refs\/uzi-held\/predecessor-run\/1/);
    assert.match(res.after.origin, /refs\/uzi-recovery\/predecessor-run/);
  });

  it("r6b completes when the predecessor publish lands AFTER the probe, leaving every ref as it was (origin gains only the late tip)", async () => {
    const pred = seedPredecessor(39451);
    const late = originCommit(pred.tip, "late.txt", "late predecessor work\n", "late predecessor milestone");
    g(fx.originPath, ["update-ref", "refs/uzi-held/predecessor-run/1", pred.tip]);
    g(fx.originPath, ["update-ref", "refs/uzi-recovery/predecessor-run", pred.tip]);
    const target = git as unknown as { originCheckpointRefState: (...a: unknown[]) => Promise<unknown> };
    const original = target.originCheckpointRefState.bind(git);
    let probes = 0;
    target.originCheckpointRefState = async (...args: unknown[]) => {
      const state = await original(...args);
      probes++;
      g(fx.originPath, ["update-ref", pred.ref, late]);
      return state;
    };
    const res = await drive({
      n: 39451,
      inExecutor: (bare) => {
        g(fx.originPath, ["update-ref", "-d", pred.ref]);
        g(bare, ["update-ref", "refs/uzi-held/predecessor-run/1", pred.tip]);
        g(bare, ["update-ref", "refs/uzi-recovery/predecessor-run", pred.tip]);
      },
    });
    assert.equal(probes, 1, "the origin probe ran exactly once");
    assertReportOnlyCompletion(res);
    assert.equal(res.after.bare, res.snapshots.bare, "no local ref changed after the discount");
    assert.equal(g(fx.originPath, ["rev-parse", pred.ref]), late, "origin's checkpoint ref is the late tip");
    const lines = (text: string) => text.split("\n").filter((line) => !line.startsWith(`${pred.ref} `));
    assert.deepEqual(lines(snap(fx.originPath)),
      lines(res.snapshots.origin), "every other origin ref equals the pre-finalize snapshot");
  });

  it("r7 still fails a zero-diff prompt run (prompt-kind orphan guard) over a substantive stale mirror absent from origin", async () => {
    // A prompt run's checkpoint key is its own branch (uzi/prompt-<run id>), so the stale mirror is
    // the run's own earlier publication, e.g. before a resume.
    let pred!: ReturnType<typeof seedPredecessor>;
    const res = await drive({
      n: 39452,
      kind: "prompt",
      reportOnly: false,
      prepare: (claim) => {
        pred = seedPredecessor(39452, `refs/uzi-checkpoints/uzi/prompt-${claim.run_id}`);
      },
      inExecutor: () => g(fx.originPath, ["update-ref", "-d", pred.ref]),
    });
    assert.equal(g(res.bare, ["rev-parse", pred.ref]), pred.tip, "the substantive mirror is present");
    assertOrphanFailure(res);
  });

  for (const withCheckpoint of [true, false]) {
    const label = withCheckpoint ? "a content-free attempted tip" : "no publish at all";
    for (const kind of ["tip", "boundary", "outside-tree", "shallow"] as const) {
      it(`r8 ${kind === "tip" ? "is refused (fails before the guard, at fetch-back)" : "fails (#299)"} over an unreadable mirror proof (${kind}) with ${label}, origin ref absent`, async () => {
        const pred = seedPredecessor(39460);
        const res = await drive({
          n: 39460,
          checkpoint: withCheckpoint,
          inExecutor: (bare) => {
            g(fx.originPath, ["update-ref", "-d", pred.ref]);
            if (kind === "tip") rmObject(bare, pred.tip);
            else if (kind === "boundary") rmObject(bare, g(bare, ["rev-parse", `${pred.base}^{tree}`]));
            else if (kind === "outside-tree") rmObject(bare, g(bare, ["rev-parse", `${pred.tip}^{tree}`]));
            else fs.writeFileSync(path.join(bare, "shallow"), `${pred.base}\n`);
          },
        });
        // A missing tip object breaks the later fetch-back before the orphan guard speaks.
        if (kind === "tip") assertRefused(res);
        else assertOrphanFailure(res);
      });
    }
  }
});
