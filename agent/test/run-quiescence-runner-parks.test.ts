import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import { execFileSync } from "node:child_process";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import { REASON_WORKER_RESIDUE_BLOCKED, type ExecutorFactory } from "../src/runner.js";
import type { RecoveryCoordinator } from "../src/recovery.js";
import type { ProcessQuiescenceState, QuiesceRunOutcome, QuiesceRunRequest } from "../src/run-quiescence.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith, simulateCommittedWork, worktreeDirFor } from "./runner-harness.js";

// issue #1783 review round — the runner's non-terminal parks fail closed WITHOUT failing: the pause
// park, the wall park and the completion hold each prove the clone quiescent at their own sink and,
// on `survivors`/`unverified`, skip the sink body (no fetch-back, no publish, the tracking ref
// untouched) and keep the clone. The wall park and the completion hold still park; the pause, with
// no durable checkpoint, reports pause_failed and the run continues (Decision 8). The finalize
// gate sits BELOW their early returns, so a parked run is never re-reported `failed`. Every credentialed custody settle is gated too, and a
// residue's detail reaches failure_reason sanitized. The quiescence primitive is injected here.

installHarness();

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function commitWork(tree: string): void {
  fs.writeFileSync(path.join(tree, "WORK.txt"), "work\n");
  execFileSync("git", ["-C", tree, "add", "WORK.txt"], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", tree, ...IDENT, "commit", "-m", "work"], { env: GIT_ENV, stdio: "pipe" });
}

function trackingSha(iid: number): string | null {
  try {
    return execFileSync("git", ["-C", git.barePathFor(fx.originPath), "rev-parse", "--verify", `refs/uzi-runner/agent/issue-${iid}`], {
      env: GIT_ENV,
      encoding: "utf8",
      stdio: "pipe",
    }).trim();
  } catch {
    return null;
  }
}

/** A quiescer that answers `state` for every call and records the requests. */
function scriptedQuiescer(state: ProcessQuiescenceState, detail = `scripted ${state}`) {
  const calls: QuiesceRunRequest[] = [];
  const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
    calls.push(req);
    return {
      process: {
        state,
        processes: state === "quiescent" ? [] : [{ pid: 4242, uid: 10002, comm: "sleep", cwd: req.targetPaths[0]!, reason: "own_attempt:kill_unconfirmed" }],
        killed: [],
        detail,
      },
      docker: { state: "not_wired", removed: [], detail: "" },
    };
  };
  return { calls, quiesceRun };
}

function drain(stream: Readable): Promise<void> {
  return new Promise((resolve, reject) => {
    stream.on("data", () => undefined);
    stream.on("end", () => resolve());
    stream.on("error", reject);
  });
}

/** Spy client.publishCheckpoint: every call is a landed publish, counted. */
function spyPublish(): { calls: () => number } {
  let n = 0;
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (_runId: string, _tip: string, pack: Readable) => {
    await drain(pack);
    n += 1;
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return { calls: () => n };
}

const statuses = (runId: string): string[] => api.states.filter((s) => s.runId === runId).map((s) => String(s.body.status));
const RUNNER_OPTS = { checkpointIntervalMs: 0, recoveryRetryMs: 1 };
const NOT_QUIESCENT: ProcessQuiescenceState[] = ["survivors", "unverified"];

// ─── pause park ────────────────────────────────────────────────────────────────────────────

/** What the pause sink left behind, observed by the executor right after parkForPause returned
 *  (before any later boundary can touch the clone, the tracking ref or the publish seam). */
interface PauseSnapshot {
  parked: boolean | undefined;
  published: number;
  tracking: string | null;
  cloneHasWork: boolean;
  cloneHead: string;
}

function pauseFactory(iid: number, pub: { calls: () => number }, seen: PauseSnapshot[]): ExecutorFactory {
  return (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        commitWork(ctx.worktreePath);
        const at = { completedCount: 1, total: 2 };
        const p = await ctx.parkForPause?.(at);
        seen.push({
          parked: p,
          published: pub.calls(),
          tracking: trackingSha(iid),
          cloneHasWork: fs.existsSync(path.join(ctx.worktreePath, "WORK.txt")),
          cloneHead: execFileSync("git", ["-C", ctx.worktreePath, "log", "-1", "--format=%s"], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim(),
        });
        return p ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      },
    },
  });
}

/** A quiescer that answers `first` at the first proof (the pause sink) and `quiescent` after, so a
 *  run whose pause failed continues to a clean finalize like any other pause_failed run. */
function firstCallQuiescer(first: ProcessQuiescenceState) {
  const calls: QuiesceRunRequest[] = [];
  const blocked = scriptedQuiescer(first);
  const clean = scriptedQuiescer("quiescent");
  const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
    calls.push(req);
    return calls.length === 1 ? blocked.quiesceRun(req) : clean.quiesceRun(req);
  };
  return { calls, quiesceRun };
}

describe("issue #1783: a pause the clone cannot be proven quiescent for does not park (Decision 8)", () => {
  for (const state of NOT_QUIESCENT) {
    it(`${state}: pause_failed (never paused), nothing fetched back or published, the clone kept, the run continues`, async () => {
      const { gitlab } = fakeGitlab();
      const iid = 1810 + (state === "survivors" ? 0 : 1);
      const pub = spyPublish();
      const { calls, quiesceRun } = firstCallQuiescer(state);
      const seen: PauseSnapshot[] = [];
      const claim = gitlabClaim(iid);
      await runnerWith(pauseFactory(iid, pub, seen), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun }).execute(claim);

      assert.equal(seen.length, 1);
      const at = seen[0]!;
      assert.equal(at.parked, false, "the pause did not park");
      assert.equal(at.published, 0, "no checkpoint published at the pause");
      assert.equal(at.tracking, null, "no fetch-back: the tracking ref is untouched");
      assert.equal(at.cloneHasWork, true, "the clone is kept");
      assert.equal(at.cloneHead, "work", "no wip(park): marker was committed");

      const st = statuses(claim.run_id);
      assert.ok(st.includes("pause_failed"), `a pause_failed report was sent: ${st.join(",")}`);
      assert.ok(!st.includes("paused"), `the run did NOT park: ${st.join(",")}`);
      assert.ok(st.includes("completed"), `the run continued and finalized like any pause_failed run: ${st.join(",")}`);
      assert.equal(calls[0]?.mode, "own", "the first proof is the pause sink's");

      const pf = api.messages(claim.run_id).find((m) => m.kind === "pause_failed");
      assert.ok(pf, "a pause_failed feed message was emitted");
      assert.match(String(pf!.payload.text), /could not be stopped/i, "the feed names the residue as the reason");
      assert.match(String(pf!.payload.text), /still running/i, "the feed says the run is still running");
    });
  }

  it("quiescent: the checkpoint is published and the run parks", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1812;
    const pub = spyPublish();
    const { calls, quiesceRun } = scriptedQuiescer("quiescent");
    const seen: PauseSnapshot[] = [];
    const claim = gitlabClaim(iid);
    await runnerWith(pauseFactory(iid, pub, seen), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun }).execute(claim);
    assert.deepEqual(seen.map((s) => s.parked), [true]);
    const st = statuses(claim.run_id);
    assert.ok(st.includes("paused"));
    assert.ok(!st.includes("pause_failed"));
    assert.ok(pub.calls() >= 1, "the checkpoint was published");
    assert.notEqual(trackingSha(iid), null, "the work was fetched back into the tracking ref");
    assert.ok(calls.length >= 1);
  });
});

// ─── wall park ─────────────────────────────────────────────────────────────────────────────

function wallFactory(outcomes: unknown[]): ExecutorFactory {
  return (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        commitWork(ctx.worktreePath);
        const o = await ctx.parkForWall?.({ completedCount: 1, total: 2 });
        outcomes.push(o);
        return o === "parked" || o === "undeliverable"
          ? { branch: ctx.branch, walled: { reason: "run exceeded its wall-clock timeout" } }
          : { branch: ctx.branch };
      },
    },
  });
}

describe("issue #1783: the wall park is gated and still stands (degraded)", () => {
  for (const state of NOT_QUIESCENT) {
    it(`${state}: parked with an empty head, nothing captured or published, never failed, the clone kept`, async () => {
      const { gitlab, calls: mrCalls } = fakeGitlab();
      const iid = 1815 + (state === "survivors" ? 0 : 1);
      const pub = spyPublish();
      const { calls, quiesceRun } = scriptedQuiescer(state);
      const outcomes: unknown[] = [];
      const claim = gitlabClaim(iid);
      await runnerWith(wallFactory(outcomes), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun }).execute(claim);
      assert.deepEqual(outcomes, ["parked"], "the wall park stands");
      assert.equal(api.wallParkRequests.length, 1);
      assert.deepEqual(api.wallParkRequests[0]!.body, { ...api.wallParkRequests[0]!.body, head: "", published: false });
      assert.ok(!statuses(claim.run_id).includes("failed"), "never converted into a failure");
      assert.equal(pub.calls(), 0, "no checkpoint published");
      assert.equal(trackingSha(iid), null, "the tracking ref is untouched");
      assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "WORK.txt")), true, "the clone is kept");
      assert.equal(mrCalls.length, 0);
      assert.deepEqual(calls.map((c) => c.mode), ["own"], "one proof, at the wall sink; the finalize gate never ran");
    });
  }

  it("quiescent: the capture runs and publishes, the park reports a head", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1817;
    const pub = spyPublish();
    const { quiesceRun } = scriptedQuiescer("quiescent");
    const outcomes: unknown[] = [];
    await runnerWith(wallFactory(outcomes), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun }).execute(gitlabClaim(iid));
    assert.deepEqual(outcomes, ["parked"]);
    const body = api.wallParkRequests[0]!.body as { head: string; published: boolean };
    assert.equal(body.head.length, 40);
    assert.equal(body.published, true);
    assert.ok(pub.calls() >= 1);
    assert.equal(trackingSha(iid), body.head);
  });
});

// ─── completion hold ───────────────────────────────────────────────────────────────────────

function holdFactory(held: Array<boolean | undefined>): ExecutorFactory {
  return (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        commitWork(ctx.worktreePath);
        const h = await ctx.enterCompletionHold!("test hold");
        held.push(h);
        if (h) return { branch: ctx.branch, completionHeld: { reason: "test hold" } };
        throw new Error("legacy terminal (could not hold)");
      },
    },
  });
}

describe("issue #1783: the completion hold is gated and still stands", () => {
  for (const state of NOT_QUIESCENT) {
    it(`${state}: held on the clone's base (no capture, no publish), never failed, the clone kept`, async () => {
      const { gitlab, calls: mrCalls } = fakeGitlab();
      const iid = 1820 + (state === "survivors" ? 0 : 1);
      api.setCompletionHoldResponse("paused", 200);
      const pub = spyPublish();
      const { calls, quiesceRun } = scriptedQuiescer(state);
      const held: Array<boolean | undefined> = [];
      const claim = gitlabClaim(iid);
      await runnerWith(holdFactory(held), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun }).execute(claim);
      assert.deepEqual(held, [true], "the hold stands");
      assert.equal(api.completionHoldRequests.length, 1);
      const head = (api.completionHoldRequests[0]!.body as { head: string }).head;
      assert.equal(head.length, 40, "a real head (the clone base; the tracking ref has none) rode the request");
      assert.ok(!statuses(claim.run_id).includes("failed"), "never converted into a failure");
      assert.equal(pub.calls(), 0, "no checkpoint published");
      assert.equal(trackingSha(iid), null, "no capture: the tracking ref is untouched");
      assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "WORK.txt")), true, "the clone is kept");
      assert.equal(mrCalls.length, 0);
      assert.deepEqual(calls.map((c) => c.mode), ["own"], "one proof, at the hold sink; the finalize gate never ran");
    });
  }

  it("quiescent: the capture runs (tracking ref = the held head) and publishes", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1822;
    api.setCompletionHoldResponse("paused", 200);
    const pub = spyPublish();
    const { quiesceRun } = scriptedQuiescer("quiescent");
    const held: Array<boolean | undefined> = [];
    await runnerWith(holdFactory(held), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun }).execute(gitlabClaim(iid));
    assert.deepEqual(held, [true]);
    const head = (api.completionHoldRequests[0]!.body as { head: string }).head;
    assert.equal(trackingSha(iid), head);
    assert.ok(pub.calls() >= 1);
  });
});

// ─── credentialed custody settles (item 5) ─────────────────────────────────────────────────

/** An enabled recovery coordinator stand-in that counts the credentialed capture. */
function fakeRecovery(): { recovery: RecoveryCoordinator; captures: () => number } {
  let captures = 0;
  const recovery = {
    enabled: true,
    pin: async (input: { runId: string; sourceSha: string; branch: string; generation?: number }) => ({
      version: 1,
      runId: input.runId,
      captureId: "cap-1",
      sourceSha: input.sourceSha,
      kind: "issue",
      branch: input.branch,
      generation: input.generation,
    }),
    captureAndUpload: async () => {
      captures += 1;
      return { state: "needs_action", captureId: "cap-1", reason: "test" };
    },
    release: async () => undefined,
    inventoryHolds: async () => [],
    inspect: async () => [],
    resumePending: async () => undefined,
  };
  return { recovery: recovery as unknown as RecoveryCoordinator, captures: () => captures };
}

function failingFactory(kind: "crash" | "limit"): ExecutorFactory {
  return (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        commitWork(ctx.worktreePath);
        if (kind === "limit") {
          const { LimitReachedError } = await import("../src/limit.js");
          throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
        }
        throw new Error("agent crashed");
      },
    },
  });
}

describe("issue #1783: credentialed custody settles are gated on quiescence", () => {
  for (const kind of ["crash", "limit"] as const) {
    for (const state of NOT_QUIESCENT) {
      it(`${kind} (${state}): the settle is skipped, custody kept, the clone kept`, async () => {
        const { gitlab } = fakeGitlab();
        const iid = 1830 + (kind === "crash" ? 0 : 2) + (state === "survivors" ? 0 : 1);
        const { recovery, captures } = fakeRecovery();
        const { quiesceRun } = scriptedQuiescer(state);
        const claim = gitlabClaim(iid, kind === "limit" ? { wait_on_limit: false } : {});
        await runnerWith(failingFactory(kind), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun, recovery }).execute(claim);
        assert.ok(statuses(claim.run_id).includes("failed"));
        assert.equal(captures(), 0, "no credentialed capture/settle ran");
        assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "WORK.txt")), true, "the clone is kept");
      });
    }

    it(`${kind} (quiescent): the settle runs`, async () => {
      const { gitlab } = fakeGitlab();
      const iid = 1836 + (kind === "crash" ? 0 : 1);
      const { recovery, captures } = fakeRecovery();
      const { quiesceRun } = scriptedQuiescer("quiescent");
      const claim = gitlabClaim(iid, kind === "limit" ? { wait_on_limit: false } : {});
      await runnerWith(failingFactory(kind), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun, recovery }).execute(claim);
      assert.ok(statuses(claim.run_id).includes("failed"));
      assert.equal(captures(), 1, "the credentialed settle ran once");
    });
  }
});

// ─── failure_reason sanitization (item 8) ──────────────────────────────────────────────────

describe("issue #1783: a residue's detail reaches failure_reason short and sanitized", () => {
  it("control and bidi characters are replaced, and the detail is capped", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1840;
    simulateCommittedWork();
    const { quiesceRun } = scriptedQuiescer("survivors", `evil\u001b[2J‮${"x".repeat(500)}`);
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: { run: async (ctx: RunContext): Promise<ExecutorResult> => ({ branch: ctx.branch, summary: "done" }) },
    });
    const claim = gitlabClaim(iid);
    await runnerWith(factory, gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun }).execute(claim);
    const failed = api.states.filter((s) => s.body.status === "failed").at(-1)?.body;
    assert.equal(failed?.fail_origin, "worker_residue_blocked");
    const reason = String(failed?.failure_reason);
    assert.ok(reason.startsWith(`${REASON_WORKER_RESIDUE_BLOCKED}: `));
    assert.ok(reason.includes("evil?[2J?x"), reason.slice(0, 120));
    assert.ok(![...reason].some((ch) => { const c = ch.codePointAt(0)!; return c < 0x20 || c === 0x202e; }), "no control/bidi code point");
    assert.ok(reason.length < 400, `capped: ${reason.length}`);
  });
});
