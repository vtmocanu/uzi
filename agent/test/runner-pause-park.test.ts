import { describe, it } from "node:test";
import { MessageBatcher } from "../src/batcher.js";
import { Outbox } from "../src/outbox.js";
import { RequestError } from "../src/client.js";
import { PauseNowSignal, SteeringChannel } from "../src/steering.js";
import { isParkAnswer } from "../src/park-run.js";
import type { AnswerVerdict } from "../src/steering.js";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { LimitReachedError } from "../src/limit.js";
import { GitCache } from "../src/git.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";
import {
  api,
  client,
  fakeGitlab,
  git,
  fx,
  gitlabClaim,
  installHarness,
} from "./runner-harness.js";

installHarness();

// ── PRD #1190 M2 — the runner's owner-requested pause park (handlePausePark) ─────────────
//
// The implement loop's ctx.parkForPause delegates here. Unlike a limit park, the checkpoint is
// published FIRST and the run parks ONLY if it lands (Decision 8); a failed publish reports
// `pause_failed` (the server clears the request) and the run CONTINUES. The park keys off the
// RETURNED status being literally "paused" (never `applied`) — the same ack contract as the
// limit park. Driven with a FAKE executor that commits work, fetch-backs via ctx.checkpoint (as
// the mid-run checkpoint does before the loop-top pause branch), then calls ctx.parkForPause.

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function commitInTree(treePath: string, file: string, content: string): void {
  fs.writeFileSync(path.join(treePath, file), content);
  execFileSync("git", ["-C", treePath, "add", file], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", treePath, ...IDENT, "commit", "-m", `add ${file}`], {
    env: GIT_ENV,
    stdio: "pipe",
  });
}

/** Fully buffer a Readable. */
function drain(stream: Readable): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    stream.on("data", (c: Buffer) => chunks.push(Buffer.from(c)));
    stream.on("end", () => resolve(Buffer.concat(chunks)));
    stream.on("error", reject);
  });
}

/** A recorded publishCheckpoint call (mirrors runner-park-checkpoint.test.ts's PublishCall). */
interface PublishCall {
  runId: string;
  tipOid: string;
}

/** Spy on client.publishCheckpoint: buffer the pack, RECORD the call, and return a landed publish.
 *  Returns { calls, restore } so a test can assert how many publish RPCs were made — an
 *  empty/unmoved tip produces no pack, so publishCheckpointBestEffort records ZERO calls. */
function spyPublish(): { calls: PublishCall[]; restore: () => void } {
  const calls: PublishCall[] = [];
  const orig = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    runId: string,
    tipOid: string,
    pack: Readable,
  ) => {
    await drain(pack);
    calls.push({ runId, tipOid });
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return {
    calls,
    restore: () => {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
    },
  };
}

/** Spy on client.publishCheckpoint that returns a non-2xx PublishResult (a publish FAILURE). */
function spyPublishHttpError(httpStatus: number): { restore: () => void } {
  const orig = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    _runId: string,
    _tipOid: string,
    pack: Readable,
  ) => {
    await drain(pack);
    return { ok: false, httpStatus };
  };
  return {
    restore: () => {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
    },
  };
}

function runnerWithGit(
  factory: ExecutorFactory,
  gitlab: ReturnType<typeof fakeGitlab>["gitlab"],
  outbox?: Outbox,
): RunRunner {
  return new RunRunner(client, git, factory, nullLogger(), 20, undefined, {
    outbox,
    outboxTerminalMaxBytes: 1 << 20,
    gapFillMax: 100,
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    questionTimeoutMs: 600,
    gitlab,
    // Disable the mid-run time-based publish so ctx.checkpoint({reap:false}) only fetches the
    // committed work back to the tracking ref — handlePausePark then does the publish itself,
    // so these tests exercise its publish path directly.
    checkpointIntervalMs: 0,
    checkpointTestHooks: { retryBackoffMs: [0, 0] },
  });
}

/** An executor that commits work, fetch-backs (as the mid-run checkpoint does), then requests a
 *  pause. Mirrors the real sdk-executor: it latches ExecutorResult.pausedAt only when the park
 *  took, so phasePublish skips finalization for a parked run and finalizes when it did not. */
function pauseFactory(homeRoot: string): {
  factory: ExecutorFactory;
  parkResults: (boolean | undefined)[];
} {
  const parkResults: (boolean | undefined)[] = [];
  const factory: ExecutorFactory = (runId) => ({
    homeDir: path.join(homeRoot, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
        commitInTree(ctx.worktreePath, "WORK.txt", "work before the pause\n");
        await ctx.checkpoint?.({ reap: false });
        const at = { completedCount: 2, total: 3 };
        const parked = await ctx.parkForPause?.(at);
        parkResults.push(parked);
        return parked ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      },
    },
  });
  return { factory, parkResults };
}

/** An executor that requests a pause WITHOUT committing or checkpointing anything — a `now` pause
 *  before any work. The clone's branch tip is still its base commit, so there is nothing to publish:
 *  handlePausePark falls through to publishCheckpointBestEffort, whose false result reports
 *  pause_failed and keeps the run running (Decision 8: no park without a durable checkpoint). */
function emptyPauseFactory(homeRoot: string): {
  factory: ExecutorFactory;
  parkResults: (boolean | undefined)[];
} {
  const parkResults: (boolean | undefined)[] = [];
  const factory: ExecutorFactory = (runId) => ({
    homeDir: path.join(homeRoot, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
        // No commit, no ctx.checkpoint: a `now` pause dropped the first turn before any work.
        const at = { completedCount: 0 };
        const parked = await ctx.parkForPause?.(at);
        parkResults.push(parked);
        return parked ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      },
    },
  });
  return { factory, parkResults };
}

/** An executor that commits a milestone, fetch-backs it (as the mid-run checkpoint does), then
 *  commits a SECOND milestone WITHOUT fetching it back, and finally requests a pause. This is the
 *  bug shape: at pause time the runner clone's tip (`expected.tip`, the second commit) is AHEAD of
 *  the bare tracking ref the checkpoint publish reads (still the first commit). A correct pause
 *  fetches the clone tip back BEFORE publishing, so the published checkpoint carries the SECOND
 *  commit; the unfixed path published the STALE first commit while recording the second as
 *  lastPublishedTip, silently losing the second commit's work on a cross-worker resume. */
function stalePauseFactory(homeRoot: string): {
  factory: ExecutorFactory;
  parkResults: (boolean | undefined)[];
  expected: { tip: string | undefined };
} {
  const parkResults: (boolean | undefined)[] = [];
  const expected: { tip: string | undefined } = { tip: undefined };
  const factory: ExecutorFactory = (runId) => ({
    homeDir: path.join(homeRoot, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
        // Milestone 1: commit, then fetch-back to the tracking ref (the mid-run checkpoint's job).
        commitInTree(ctx.worktreePath, "WORK.txt", "milestone 1\n");
        await ctx.checkpoint?.({ reap: false });
        // Milestone 2: commit MORE work but do NOT checkpoint — the tracking ref is now STALE
        // relative to the clone tip, exactly as a `now` pause mid-milestone leaves it.
        commitInTree(ctx.worktreePath, "WORK2.txt", "milestone 2 — committed, not yet fetched back\n");
        expected.tip = execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"], {
          env: GIT_ENV,
          stdio: "pipe",
        })
          .toString()
          .trim();
        const at = { completedCount: 2, total: 3 };
        const parked = await ctx.parkForPause?.(at);
        parkResults.push(parked);
        return parked ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      },
    },
  });
  return { factory, parkResults, expected };
}

const stateStatuses = (runId: string): string[] =>
  api.states.filter((s) => s.runId === runId).map((s) => s.body.status);

const messageKinds = (runId: string): string[] => api.messages(runId).map((m) => m.kind);

describe("RunRunner — owner-requested pause park (PRD #1190 M2)", () => {
  it("unchanged generation 2 resume publishes generation 1 work before parking", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1935, { claim_generation: 2, session_id: "resume-session",
      pause_pending: true, pause_mode: "now" });
    const branch = "agent/issue-1935";
    const bare = await git.ensureClone(fx.originPath);
    const seed = await git.runnerCloneForBranch(bare, branch, "issue-1935", noProofReseed, claim.run_id);
    assert.ok(seed.defaultBranchCommit);
    commitInTree(seed.path, "WORK.txt", "generation 1 work\n");
    const h = execFileSync("git", ["-C", seed.path, "rev-parse", "HEAD"], { env: GIT_ENV }).toString().trim();
    const result = await git.fetchAgentBranch(bare, seed.path, branch, claim.run_id, {
      context: { barePath: bare, branch, runId: claim.run_id, generation: 1, kind: "issue",
        defaultIdentity: { ref: "refs/remotes/origin/main", sha: seed.defaultBranchCommit } },
    });
    assert.equal(result.kind, "updated");
    assert.equal((await git.committedTrackingOwnership(bare, branch, claim.run_id, h, 1)).kind, "owned");
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-resume-pause-"));
    const { calls, restore } = spyPublish();
    const publish = client.publishCheckpoint.bind(client);
    client.publishCheckpoint = async (...args) => {
      assert.equal(args[1], h);
      assert.equal(api.states.some((state) => state.body.status === "paused"), false);
      assert.equal((await git.committedTrackingOwnership(bare, branch, claim.run_id, h, 2)).kind, "owned");
      const owed = await git.enumerateOwedCandidates(bare, claim.run_id);
      assert.ok(owed.find(candidate => candidate.sha === h)?.contexts.some(context => context.generation === 1),
        "producing generation stays recoverable before remote confirmation");
      return publish(...args);
    };
    try {
      const { factory, parkResults } = emptyPauseFactory(homeRoot);
      await runnerWithGit(factory, gitlab).execute(claim);
      assert.deepEqual(parkResults, [true]);
      assert.deepEqual(calls, [{ runId: claim.run_id, tipOid: h }]);
      const parked = api.states.find((state) => state.body.status === "paused");
      assert.ok(parked);
      assert.notEqual(parked.body.checkpoint_contains_latest, false);
      assert.equal((await git.committedTrackingOwnership(bare, branch, claim.run_id, h, 2)).kind, "owned");
      const old = (await git.enumerateOwedCandidates(bare, claim.run_id)).find((candidate) => candidate.sha === h);
      assert.equal(old, undefined, "only the confirmed checkpoint containing H clears the old producing pin");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("reports `paused` ONLY after a successful checkpoint publish, and does not finalize", async () => {
    const { gitlab, calls: mrCalls } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1190-ok-"));
    const { restore } = spyPublish();
    try {
      const { factory, parkResults } = pauseFactory(homeRoot);
      const claim = gitlabClaim(1101);
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.deepEqual(parkResults, [true], "parkForPause parked the run (publish landed)");
      const statuses = stateStatuses(claim.run_id);
      assert.ok(statuses.includes("paused"), `a paused report was sent, got ${JSON.stringify(statuses)}`);
      assert.ok(
        !statuses.includes("completed") && !statuses.includes("failed"),
        "a parked run must NOT finalize (no completed/failed report)",
      );
      assert.equal(mrCalls.length, 0, "a parked run opens no merge request");
      assert.ok(messageKinds(claim.run_id).includes("paused"), "a `paused` feed event was emitted");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("reports `pause_failed` and KEEPS RUNNING (finalizes) when the publish rejects", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1190-fail-"));
    const { restore } = spyPublishHttpError(500);
    try {
      const { factory, parkResults } = pauseFactory(homeRoot);
      const claim = gitlabClaim(1102);
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.equal(messageKinds(claim.run_id).includes("paused"), false, "no completed pause marker");
      assert.deepEqual(parkResults, [false], "the park was declined (publish failed)");
      const statuses = stateStatuses(claim.run_id);
      assert.ok(statuses.includes("pause_failed"), `a pause_failed report was sent, got ${JSON.stringify(statuses)}`);
      assert.ok(!statuses.includes("paused"), "the run did NOT park — no paused report");
      // The run KEEPS RUNNING and finalizes normally (the fake executor returned no pausedAt).
      assert.ok(statuses.includes("completed"), "the run continued and completed");
      const feed = api.messages(claim.run_id);
      const pf = feed.find((m) => m.kind === "pause_failed");
      assert.ok(pf, "a pause_failed feed message was emitted");
      assert.match(
        String(pf!.payload.text),
        /still running/i,
        "the pause_failed message tells the owner the run is still running",
      );
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("cleans up as UNPARKED when the paused ACK returns a status other than `paused`", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1190-ackmiss-"));
    const { restore } = spyPublish();
    try {
      const { factory, parkResults } = pauseFactory(homeRoot);
      const claim = gitlabClaim(1103);
      // The server declines the park (e.g. a concurrent cancel): every /state ACK returns
      // "running", so the paused report's RETURNED status is not "paused".
      api.overrideStateStatus(claim.run_id, "running");
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.equal(messageKinds(claim.run_id).includes("paused"), false, "no completed pause marker");
      assert.deepEqual(parkResults, [false], "the park did not take (ack status != paused)");
      const statuses = stateStatuses(claim.run_id);
      assert.ok(statuses.includes("paused"), "a paused report WAS attempted");
      // Cleaned up as unparked: the run continued and finalized rather than parking.
      assert.ok(statuses.includes("completed"), "the run cleaned up as unparked and completed");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("reports `pause_failed` and KEEPS RUNNING on an EMPTY/unpublishable pack (a `now` pause before any commit)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1190-empty-"));
    // A `now` pause dropped the first turn before any commit landed durably, so the clone's tip is
    // still its unmoved base. Use a HEALTHY transport (spyPublish, NOT a forced HTTP error): with a
    // healthy transport the ONLY reason no publish lands is that checkpointPack produces no pack for
    // an empty/unmoved tip, so publishCheckpointBestEffort makes ZERO publish RPCs and still returns
    // false → pause_failed, and the run keeps running (Decision 8: no clean-park shortcut without a
    // durable checkpoint). The calls.length === 0 assertion below is what catches an unsafe base-tip
    // shortcut — forcing the failure with an HTTP 500 would MASK it, since a 500 yields pause_failed
    // whether or not a pack was ever produced/published. (A base-tip shortcut would be UNSAFE on the
    // seededFrom:"tracking" resume leg, where baseCommit is durable on origin only if the prior
    // park's best-effort publish landed.)
    const { calls, restore } = spyPublish();
    try {
      const { factory, parkResults } = emptyPauseFactory(homeRoot);
      const claim = gitlabClaim(1105);
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.equal(messageKinds(claim.run_id).includes("paused"), false, "no completed pause marker");
      assert.deepEqual(parkResults, [false], "an empty/unpublishable pack does NOT park");
      assert.equal(
        calls.length,
        0,
        `an empty/unmoved tip produces no pack, so ZERO publish RPCs are made, got ${calls.length}`,
      );
      const statuses = stateStatuses(claim.run_id);
      assert.ok(
        statuses.includes("pause_failed"),
        `a pause_failed report was sent, got ${JSON.stringify(statuses)}`,
      );
      assert.ok(!statuses.includes("paused"), "an empty pack must NOT report paused");
      // The run KEEPS RUNNING past the declined park and finalizes rather than staying parked. This
      // fake commits nothing, so the continued run terminates as `failed` (no work to submit) — the
      // load-bearing point is that it did NOT park (a real run would restart the interrupted turn).
      assert.ok(
        statuses.includes("failed") || statuses.includes("completed"),
        `the run continued to a terminal state, not parked, got ${JSON.stringify(statuses)}`,
      );
      const feed = api.messages(claim.run_id);
      const pf = feed.find((m) => m.kind === "pause_failed");
      assert.ok(pf, "a pause_failed feed message was emitted");
      assert.match(
        String(pf!.payload.text),
        /still running/i,
        "the pause_failed message tells the owner the run is still running",
      );
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("publishes the CURRENT clone tip on pause, fetching committed work back before the checkpoint (not a stale tip)", async () => {
    const { gitlab, calls: mrCalls } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1190-stale-"));
    const { calls, restore } = spyPublish();
    try {
      const { factory, parkResults, expected } = stalePauseFactory(homeRoot);
      const claim = gitlabClaim(1106);
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.ok(expected.tip, "the executor recorded the clone's second-commit tip");
      assert.deepEqual(parkResults, [true], "the run parked (the committed work was made durable)");
      const statuses = stateStatuses(claim.run_id);
      assert.ok(statuses.includes("paused"), `a paused report was sent, got ${JSON.stringify(statuses)}`);
      assert.ok(!statuses.includes("completed") && !statuses.includes("failed"), "a parked run must NOT finalize");
      assert.equal(mrCalls.length, 0, "a parked run opens no merge request");
      // The load-bearing assertion: the checkpoint published the SECOND commit (the current clone
      // tip), not the stale first commit the bare tracking ref still held. On the unfixed path this
      // is the first commit's SHA, so this equality fails.
      assert.ok(calls.length >= 1, `a checkpoint was published, got ${calls.length} publish calls`);
      assert.equal(
        calls[calls.length - 1]!.tipOid,
        expected.tip,
        "the pause checkpoint published the current clone tip (committed work fetched back first), not a stale earlier tip",
      );
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("a resumed worker (claim.pause_pending) parks at its first boundary with NO fresh input", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1190-resume-"));
    const { restore } = spyPublish();
    try {
      const { factory, parkResults } = pauseFactory(homeRoot);
      // A resume: the durable pause columns survived the requeue, so the claim re-delivers the
      // pending pause. buildFlight seeds the steering channel from these (seedPauseRequested).
      const claim = gitlabClaim(1104, { pause_pending: true, pause_mode: "now" });
      // NO pause input is ever enqueued (api.inputs stays empty) — the run parks purely from the
      // re-delivered request + the executor's boundary check.
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.deepEqual(parkResults, [true], "the resumed run parked at its first boundary");
      const statuses = stateStatuses(claim.run_id);
      assert.ok(statuses.includes("paused"), "the resumed run reported paused with no fresh input");
      assert.ok(!statuses.includes("completed"), "a parked resume does not finalize");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});

describe("accepted pause delivery", () => {
  for (const mode of ["milestone", "now", "postclone", "emit-error", "flush-error", "fatal", "refused", "report-error"] as const) {
    it(`${mode}: only an accepted durable owner pause emits; delivery cannot replace the park`, async () => {
      const { gitlab } = fakeGitlab();
      const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-pause-delivery-"));
      const outbox = new Outbox({ root: path.join(homeRoot, "outbox"), log: nullLogger(),
        runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000 });
      await outbox.init();
      const claim = gitlabClaim(2992, { claim_generation: 1 });
      const publish = spyPublish();
      const report = client.reportState.bind(client);
      const emit = MessageBatcher.prototype.emit;
      const flush = MessageBatcher.prototype.flush;
      let ackAccepted = false;
      let pinned = false;
      let deliveryAttempts = 0;
      let reaps = 0;
      let reapsAtDelivery = 0;
      let clonePath = "";
      let signal: AbortSignal | undefined;
      let parked: boolean | undefined;
      let batcher: MessageBatcher | undefined;
      const at = mode === "now" ? { completedCount: 0 } : { completedCount: 2, total: 3 };
      const payloads: unknown[] = [];
      const factory: ExecutorFactory = runId => ({
        homeDir: path.join(homeRoot, runId),
        executor: {
          killAgentTree: () => { reaps++; },
          run: async ctx => {
            fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
            clonePath = ctx.worktreePath;
            signal = ctx.signal;
            commitInTree(ctx.worktreePath, "WORK.txt", "durable pause work\n");
            if (mode === "postclone") throw new PauseNowSignal();
            parked = await ctx.parkForPause!(at);
            return parked ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
          },
        },
      });
      const run = runnerWithGit(factory, gitlab, outbox);
      const hooks = run as unknown as { pinRecoveryGeneration: (...args: unknown[]) => Promise<void> };
      const pin = hooks.pinRecoveryGeneration.bind(run);
      hooks.pinRecoveryGeneration = async (...args) => {
        await pin(...args);
        batcher = (args[1] as { batcher: MessageBatcher }).batcher;
        pinned = true;
      };
      client.reportState = async (...args) => {
        if (args[1].status !== "paused") return report(...args);
        assert.equal(deliveryAttempts, 0, "no marker before report or ACK");
        if (mode === "report-error") throw new Error("pause report unavailable");
        const ack = await report(...args);
        assert.equal(deliveryAttempts, 0, "no marker while ACK is in flight");
        pinned = false;
        ackAccepted = ack.status === "paused";
        if (mode === "fatal") api.failMessagesNext(100, 401);
        return ack;
      };
      if (mode === "refused") api.overrideStateStatus(claim.run_id, "running");
      MessageBatcher.prototype.emit = function(message) {
        if (message.kind === "paused") {
          deliveryAttempts++;
          assert.equal(ackAccepted, true);
          assert.equal(pinned, true, "recovery pin precedes delivery");
          reapsAtDelivery = reaps;
          payloads.push(message.payload);
          if (mode === "emit-error") throw new Error("synchronous emit failure");
        }
        return emit.call(this, message);
      };
      MessageBatcher.prototype.flush = function(...args) {
        if (this === batcher && deliveryAttempts > 0 && mode === "flush-error") return Promise.reject(new Error("flush failure"));
        return flush.apply(this, args);
      };
      try {
        await run.execute(claim);
        const accepted = mode !== "refused" && mode !== "report-error";
        assert.equal(deliveryAttempts, accepted ? 1 : 0);
        if (!accepted) {
          assert.equal(parked, false);
          assert.equal(messageKinds(claim.run_id).includes("paused"), false);
          return;
        }
        assert.deepEqual(payloads, [{ origin: "owner", completed: mode === "postclone" ? 0 : at.completedCount,
          ...(mode !== "now" && mode !== "postclone" ? { total: 3 } : {}) }]);
        if (mode !== "postclone") assert.equal(parked, true);
        await batcher!.awaitPermanentFailureSettled();
        assert.equal(batcher!.isTripped(), mode === "fatal", "fatal response must trip the REAL breaker");
        assert.equal(signal?.aborted, false, "accepted park must not cancel the executor");
        assert.equal(outbox.hasPendingTerminal(claim.run_id, 1), false);
        assert.equal(await outbox.readTerminalJournal(claim.run_id, 1), undefined);
        assert.deepEqual(stateStatuses(claim.run_id).filter(s => s === "failed" || s === "completed"), []);
        assert.equal(stateStatuses(claim.run_id).filter(s => s === "paused").length, 1, "no extra park");
        // Normal return reaps at handoff, phasePublish, and terminal_retire; the signal path only retires.
        assert.equal(reaps, reapsAtDelivery + (mode === "postclone" ? 1 : 3), "no transport-failure reap");
        assert.equal(fs.existsSync(path.join(homeRoot, claim.run_id)), true, "parked HOME preserved");
        assert.equal(fs.existsSync(clonePath), false, "existing verified-clone retirement is unchanged");
        const bare = await git.ensureClone(fx.originPath);
        assert.equal(execFileSync("git", ["-C", bare, "show", "refs/uzi-runner/agent/issue-2992:WORK.txt"],
          { env: GIT_ENV }).toString(), "durable pause work\n", "checkpoint tracking source retained");
      } finally {
        client.reportState = report;
        MessageBatcher.prototype.emit = emit;
        MessageBatcher.prototype.flush = flush;
        publish.restore();
        fs.rmSync(homeRoot, { recursive: true, force: true });
      }
    });
  }
});

describe("RunRunner — M2 explicit park callback", () => {
  async function exercise(options: {
    supported?: boolean; empty?: boolean; answer?: string; cancel?: boolean;
    retry?: boolean; lostAck?: boolean; guardedPause?: boolean; unknown?: boolean;
    unattended?: boolean; ownership?: "wall" | "released" | "foreign" | "completion";
    deadlineRetry?: boolean; cancelledBefore?: boolean; legacyGeneration?: boolean;
    nonissue?: boolean; requestRefused?: boolean; wallPending?: boolean; guardedWall?: boolean;
    guardedExistingQuestion?: boolean; failedProbe?: boolean; resumeGuardedWall?: boolean;
    interruptWait?: boolean; publishFailure?: boolean; resumeFence?: "paused" | "foreign";
    interruptReconcile?: "pause_failed" | "ownership";
    pauseTransport?: "before" | "after"; questionTransport?: "replay" | "exhausted" | "httpFailure" | "unreadable" | "completionHold" | "refused";
  } = {}) {
    const claim = { ...gitlabClaim(2657), claim_generation: options.legacyGeneration ? 0 : 1,
      ...(options.nonissue ? { kind: "prompt" as const } : {}),
      park_run_supported: options.supported !== false, auto_approve: options.unattended ?? false };
    api.parkRunGeneration = options.supported === false ? undefined : 1;
    if (!options.unknown) api.parkRunOwnership = {
      status: options.lostAck ? "paused" : "running",
      claim_generation: 1, hold_reason: null, claim_released: false,
    };
    if (options.ownership === "wall") api.parkRunOwnership!.hold_reason = "budget_exhausted";
    if (options.ownership === "wall") api.parkRunOwnership!.status = "paused";
    if (options.ownership === "completion") {
      api.parkRunOwnership!.status = "paused";
      api.parkRunOwnership!.hold_reason = "completion_blocked";
    }
    if (options.ownership === "released") api.parkRunOwnership!.claim_released = true;
    if (options.ownership === "foreign") api.parkRunOwnership!.claim_generation = 2;
    if (options.guardedPause) api.guardedQuestionStatus = "paused";
    if (options.guardedExistingQuestion) api.guardedQuestionStatus = "awaiting_input";
    api.workerPauseRefused = !!options.requestRefused || !!options.wallPending;
    if (options.wallPending) api.workerPauseReason = "wall_pending";
    if (options.guardedWall) {
      api.guardedQuestionStatus = "running";
      api.guardedQuestionReason = "wall_pending";
    }
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-park-run-"));
    const publish = spyPublish();
    const report = client.reportState.bind(client);
    const awaitAnswer = SteeringChannel.prototype.awaitAnswer;
    const lifecycleSignal = SteeringChannel.prototype.lifecycleSignal;
    let seedWall: (() => void) | undefined;
    let reconcileInterrupted = false;
    SteeringChannel.prototype.lifecycleSignal = function() {
      seedWall = () => this.seedPauseRequested("wall");
      return lifecycleSignal.call(this);
    };
    const interruptReconcile = () => {
      if (reconcileInterrupted) return;
      assert.ok(seedWall, "the real callback steering channel is active");
      reconcileInterrupted = true;
      seedWall();
    };
    let waitEntries = 0;
    if (options.interruptWait) SteeringChannel.prototype.awaitAnswer = function(questionId) {
      if (++waitEntries === 1) {
        this.seedPauseRequested("wall");
        return Promise.reject(new PauseNowSignal());
      }
      return awaitAnswer.call(this, questionId);
    };
    const publication = client.publishCheckpoint.bind(client);
    if (options.publishFailure) client.publishCheckpoint = async (...args) => {
      await publication(...args);
      return { ok: false, httpStatus: 500 };
    };
    const requestPause = client.requestWorkerPause.bind(client);
    const ownership = client.getRunOwnership.bind(client);
    const originalFetch = globalThis.fetch;
    let pauseAttempts = 0;
    let boundedReads = 0;
    let clonePath: string | undefined;
    const questionBodies: string[] = [];
    client.requestWorkerPause = async (...args) => {
      pauseAttempts++;
      if (options.pauseTransport === "before") throw new RequestError("POST", "/pause", 0, "transport failed");
      const ack = await requestPause(...args);
      if (options.pauseTransport === "after") throw new RequestError("POST", "/pause", 0, "lost ACK");
      return ack;
    };
    client.getRunOwnership = async (...args) => {
      const bounded = !!args[1] && "deadline" in args[1];
      if (bounded) boundedReads++;
      if (options.failedProbe) throw new TypeError("probe unavailable");
      const own = await ownership(...args);
      if (bounded && options.interruptReconcile === "ownership") interruptReconcile();
      // Keep the empty-checkpoint seam empty until the sink has declined it.
      if ((options.ownership === "completion" || options.guardedExistingQuestion || ["exhausted", "httpFailure", "unreadable", "completionHold"].includes(options.questionTransport ?? "")) && clonePath) fs.writeFileSync(path.join(clonePath, "DIRTY.txt"), "retained uncommitted work\n");
      return own;
    };
    if (options.questionTransport) globalThis.fetch = async (input, init) => {
      const body = typeof init?.body === "string" ? JSON.parse(init.body) : undefined;
      if (!body?.require_running || body.status !== "awaiting_input") return originalFetch(input, init);
      questionBodies.push(init!.body as string);
      if (options.questionTransport === "refused") return new Response("refused", { status: 403 });
      if (options.questionTransport === "exhausted") throw new TypeError("question transport unavailable");
      if (options.questionTransport === "httpFailure") return new Response("unavailable", { status: 503 });
      if (options.questionTransport === "unreadable") return new Response("unreadable ACK");
      if (options.questionTransport === "completionHold") return Response.json({ run: { status: "paused", hold_reason: "completion_blocked" }, applied: false }, { status: 409 });
      if (questionBodies.length === 1) {
        await originalFetch(input, init); // Commit the question, then lose the response.
        throw new TypeError("lost question ACK");
      }
      // Model the API no-write replay response; the real client must retry the exact body.
      return Response.json({ run: { status: "awaiting_input" } });
    };
    client.reportState = async (...args) => {
      if (args[1].status === "paused")
        assert.equal(messageKinds(claim.run_id).includes("paused"), false, "no marker before paused ACK");
      const ack = await report(...args);
      if (args[1].status === "paused")
        assert.equal(messageKinds(claim.run_id).includes("paused"), false, "no marker until ACK returned");
      if (options.interruptReconcile === "pause_failed" && args[1].status === "pause_failed") interruptReconcile();
      if (options.lostAck && args[1].status === "paused") throw new Error("lost paused ACK");
      return ack;
    };
    let result: Awaited<ReturnType<NonNullable<RunContext["requestParkRun"]>>> | undefined;
    let turns = 0;
    let questions = 0;
    let callbackEntries = 0;
    let callbackPublications = 0;
    const questionIds: (string | undefined)[] = [];
    const timerDelays: number[] = [];
    let callbackReads = 0;
    const clockStart = Date.now();
    api.onState(claim.run_id, (state) => {
      if (state.status !== "awaiting_input") return;
      questions++;
      questionIds.push(state.open_question_id);
      if (options.interruptWait && questions === 1) return;
      if (options.deadlineRetry && questions > 1) return;
      const answer = (options.retry || options.deadlineRetry) && questions === 1 ? " /park " : options.answer ?? "repair the checkpoint";
      api.setInputs(claim.run_id, [{
        id: questions, kind: options.cancel ? "cancel" : "answer",
        body: options.cancel ? null : JSON.stringify({ question_id: state.open_question_id, answers: [answer] }),
      }]);
      if (options.retry && questions === 1) api.parkRunOwnership!.status = "paused";
    });
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeRoot, runId),
      executor: { run: async (ctx) => {
        turns++;
        clonePath = ctx.worktreePath;
        if (!options.empty) fs.writeFileSync(path.join(ctx.worktreePath, "DIRTY.txt"), "retained uncommitted work\n");
        fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
        assert.equal(ctx.parkRunSupported, options.supported !== false);
        if (!options.empty) commitInTree(ctx.worktreePath, "WORK.txt", "callback checkpoint\n");
        if (options.cancelledBefore) {
          api.setInputs(claim.run_id, [{ id: 99, kind: "cancel", body: null }]);
          await new Promise<void>((resolve, reject) => {
            const deadline = Date.now() + 1000;
            const check = () => {
              if (ctx.cancelRequested?.()) return resolve();
              if (Date.now() >= deadline) return reject(new Error("cancel delivery deadline"));
              setTimeout(check, 5);
            };
            check();
          });
        }
        const readsBefore = api.ownershipRequests.length;
        try {
          callbackEntries++;
          try { result = await ctx.requestParkRun!({ completedCount: 1, total: 2 }); }
          catch (error) {
            if (!options.resumeGuardedWall || !(error instanceof PauseNowSignal)) throw error;
            api.guardedQuestionStatus = undefined;
            api.guardedQuestionReason = undefined;
            api.setWallParkResponse("running", 409);
            assert.equal(await ctx.parkForWall!({ completedCount: 1, total: 2 }), "refused");
            ctx.clearWallMode!();
            if (options.resumeFence === "paused") api.guardedQuestionStatus = "paused";
            if (options.resumeFence === "foreign") api.parkRunGeneration = 2;
            callbackEntries++;
            result = await ctx.requestParkRun!({ completedCount: 1, total: 2 });
          }
        }
        finally { callbackReads = api.ownershipRequests.length - readsBefore; }
        callbackPublications = publish.calls.length;
        if (options.publishFailure) {
          for (let i = 0; i < callbackPublications; i += 3) {
            const tips = publish.calls.slice(i, i + 3).map(call => call.tipOid);
            assert.deepEqual(tips, Array(3).fill(tips[0]), "each failed capture retries the same tip");
          }
        }
        if (options.questionTransport === "replay" && result.kind === "answer")
          commitInTree(ctx.worktreePath, "WORK.txt", "work after recovered guidance\n");
        return result.kind === "paused"
          ? { branch: ctx.branch, pausedAt: { completedCount: 1, total: 2 } }
          : { branch: ctx.branch };
      } },
    });
    try {
      const { gitlab } = fakeGitlab();
      const runner = options.deadlineRetry || options.interruptWait
        ? new RunRunner(client, git, factory, nullLogger(), 20, undefined, {
            pollMs: 5, questionTimeoutMs: 600, gitlab, checkpointIntervalMs: 0,
            checkpointTestHooks: { retryBackoffMs: [0, 0] },
            now: () => clockStart + (options.interruptWait ? (callbackEntries > 1 ? 200 : 0) : questions * 601),
            setTimer: (cb, ms) => {
              timerDelays.push(ms);
              const timer = setTimeout(cb, ms);
              timer.unref();
              return () => clearTimeout(timer);
            },
          })
        : runnerWithGit(factory, gitlab);
      await runner.execute(claim);
      if (options.lostAck || options.publishFailure || publish.calls.length === 0)
        assert.equal(messageKinds(claim.run_id).includes("paused"), false, "no completed marker without an accepted checkpoint park");
      const workPreserved = !!clonePath && fs.existsSync(path.join(clonePath, "DIRTY.txt")) &&
        fs.existsSync(path.join(homeRoot, claim.run_id));
      return { claim, result, turns, questions, callbackReads, published: publish.calls.length,
        pauseAttempts, boundedReads, questionBodies, workPreserved, callbackEntries, callbackPublications, questionIds, timerDelays };
    } finally {
      client.reportState = report;
      SteeringChannel.prototype.awaitAnswer = awaitAnswer;
      SteeringChannel.prototype.lifecycleSignal = lifecycleSignal;
      client.requestWorkerPause = requestPause;
      client.getRunOwnership = ownership;
      globalThis.fetch = originalFetch;
      publish.restore();
      api.setWallParkResponse("paused", 200);
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  }

  for (const interruptReconcile of ["pause_failed", "ownership"] as const) {
    it(`resumes false-pause reconciliation after wall at ${interruptReconcile} without a new attempt`, async () => {
      const outcome = await exercise({ publishFailure: true, interruptReconcile, resumeGuardedWall: true });
      assert.equal(outcome.callbackEntries, 2);
      assert.equal(outcome.pauseAttempts, 1, "a refused wall is not a consumed owner command");
      assert.equal(outcome.callbackPublications, 6, "one exhausted pause and one exhausted wall capture, each with three requests");
      assert.equal(outcome.boundedReads, 1, "the false pause still requires one bounded diagnostic read");
      assert.equal(outcome.callbackReads, 2, "one pause diagnostic plus the existing refused-wall probe");
      assert.equal(outcome.questions, 1);
      assert.deepEqual(outcome.result, { kind: "answer", answers: ["repair the checkpoint"] });
    });
  }

  it("a consumed /park after early reconciliation interruption permits one fresh attempt", async () => {
    const outcome = await exercise({ empty: true, interruptReconcile: "pause_failed", resumeGuardedWall: true, retry: true });
    assert.equal(outcome.pauseAttempts, 2);
    assert.equal(outcome.boundedReads, 1, "the fresh successful pause needs no false-result diagnostic");
    assert.equal(outcome.questions, 1);
    assert.deepEqual(outcome.result, { kind: "paused" });
  });

  for (const resumeFence of ["paused", "foreign"] as const) {
    it(`early reconciliation continuation cannot authorize a question over ${resumeFence} ownership`, async () => {
      const outcome = await exercise({ publishFailure: true, interruptReconcile: "ownership", resumeGuardedWall: true, resumeFence });
      assert.equal(outcome.pauseAttempts, 1);
      assert.equal(outcome.boundedReads, 1);
      assert.equal(outcome.questions, 0);
      assert.equal(outcome.result?.kind, resumeFence === "paused" ? "paused" : undefined);
      assert.ok(!stateStatuses(outcome.claim.run_id).includes("failed"));
    });
  }

  it("resumes the failure question after refused wall without another worker pause", async () => {
    const outcome = await exercise({ empty: true, guardedWall: true, resumeGuardedWall: true });
    assert.deepEqual(outcome.result, { kind: "answer", answers: ["repair the checkpoint"] });
    assert.equal(outcome.callbackEntries, 2);
    assert.equal(outcome.pauseAttempts, 1);
    assert.equal(outcome.questions, 1);
  });

  for (const resumeFence of ["paused", "foreign"] as const) {
    it(`pending phase does not authorize a question over ${resumeFence} ownership`, async () => {
      const outcome = await exercise({ empty: true, guardedWall: true, resumeGuardedWall: true, resumeFence });
      assert.equal(outcome.pauseAttempts, 1);
      assert.equal(outcome.callbackEntries, 2);
      assert.equal(outcome.questions, 0, "guard still controls question-write authority");
      assert.equal(outcome.result?.kind, resumeFence === "paused" ? "paused" : undefined);
      assert.ok(!stateStatuses(outcome.claim.run_id).includes("failed"));
    });
  }

  it("cancel settles the resumed failure question without another worker pause", async () => {
    const outcome = await exercise({ empty: true, guardedWall: true, resumeGuardedWall: true, cancel: true });
    assert.deepEqual(outcome.result, { kind: "cancel" });
    assert.equal(outcome.pauseAttempts, 1);
  });

  it("retains an interrupted active question ID and absolute deadline on callback re-entry", async () => {
    const outcome = await exercise({ empty: true, interruptWait: true, resumeGuardedWall: true });
    assert.equal(outcome.pauseAttempts, 1);
    assert.equal(outcome.callbackEntries, 2);
    assert.equal(outcome.questionIds.length, 2);
    assert.equal(outcome.questionIds[0], outcome.questionIds[1]);
    assert.deepEqual(outcome.timerDelays, [600, 400]);
    assert.deepEqual(outcome.result, { kind: "answer", answers: ["repair the checkpoint"] });
  });

  it("does not republish failed work after a refused wall without a fresh command", async () => {
    const outcome = await exercise({ publishFailure: true, guardedWall: true, resumeGuardedWall: true });
    assert.equal(outcome.pauseAttempts, 1);
    assert.equal(outcome.callbackPublications, 6, "one exhausted pause and one exhausted wall capture, each with three requests");
    assert.equal(outcome.result?.kind, "answer");
  });

  it("a fresh consumed retry after refused wall authorizes exactly one more attempt", async () => {
    const outcome = await exercise({ empty: true, guardedWall: true, resumeGuardedWall: true, retry: true });
    assert.equal(outcome.pauseAttempts, 2);
    assert.equal(outcome.questions, 1);
    assert.deepEqual(outcome.result, { kind: "paused" });
  });

  it("parks through the real checkpoint sink without finalizing", async () => {
    const outcome = await exercise();
    assert.deepEqual(outcome.result, { kind: "paused" });
    assert.deepEqual(api.messages(outcome.claim.run_id).filter(m => m.kind === "paused").map(m => m.payload),
      [{ origin: "lead_park", completed: 1, total: 2 }]);
    assert.equal(outcome.published, 1);
    assert.deepEqual(api.workerPauseRequests, [{ runId: outcome.claim.run_id, generation: 1 }]);
    assert.equal(outcome.callbackReads, 0);
    assert.equal(outcome.questions, 0);
    assert.ok(!stateStatuses(outcome.claim.run_id).some(s => s === "failed" || s === "completed"));
  });

  it("empty checkpoint asks a guarded question once and returns actionable guidance", async () => {
    const outcome = await exercise({ empty: true });
    assert.deepEqual(outcome.result, { kind: "answer", answers: ["repair the checkpoint"] });
    assert.equal(outcome.published, 0);
    assert.equal(outcome.turns, 1);
    assert.equal(api.workerPauseRequests.length, 1);
    assert.equal(outcome.callbackReads, 1);
    assert.equal(api.states.find(s => s.body.status === "awaiting_input")?.body.require_running, true);
    assert.equal(messageKinds(outcome.claim.run_id).filter(k => k === "pause_failed").length, 1);
  });

  it("only a consumed /park answer authorizes another attempt after confirmed running settlement", async () => {
    const outcome = await exercise({ empty: true, retry: true });
    assert.deepEqual(outcome.result, { kind: "paused" });
    assert.equal(api.workerPauseRequests.length, 2);
    assert.equal(outcome.questions, 1);
    assert.equal(outcome.turns, 1);
    const statuses = stateStatuses(outcome.claim.run_id);
    assert.ok(statuses.indexOf("running", statuses.indexOf("awaiting_input")) > statuses.indexOf("awaiting_input"));
  });

  it("returns an owner cancel without another pause request", async () => {
    const outcome = await exercise({ empty: true, cancel: true });
    assert.deepEqual(outcome.result, { kind: "cancel" });
    assert.equal(api.workerPauseRequests.length, 1);
  });

  it("reconciles a lost paused ACK using exact unreleased ordinary ownership", async () => {
    const outcome = await exercise({ lostAck: true });
    assert.deepEqual(outcome.result, { kind: "paused" });
    assert.equal(outcome.callbackReads, 1);
    assert.equal(outcome.questions, 0);
    assert.ok(!stateStatuses(outcome.claim.run_id).includes("failed"));
  });

  for (const options of [
    { lostAck: true, failedProbe: true },
    { pauseTransport: "before" as const }, { pauseTransport: "after" as const },
    { empty: true, questionTransport: "exhausted" as const },
    { empty: true, questionTransport: "httpFailure" as const },
    { empty: true, questionTransport: "unreadable" as const },
  ]) {
    it(`retains ambiguous park work without failing or retrying: ${JSON.stringify(options)}`, async () => {
      const outcome = await exercise(options);
      assert.equal(outcome.result, undefined);
      assert.equal(outcome.turns, 1);
      assert.equal(outcome.pauseAttempts, 1);
      assert.equal(outcome.questions, 0);
      assert.equal(outcome.workPreserved, true);
      assert.ok(!stateStatuses(outcome.claim.run_id).includes("failed"));
      if (options.questionTransport) assert.equal(outcome.questionBodies.length, options.questionTransport === "unreadable" ? 1 : 3, "bounded client transport attempts");
    });
  }

  it("the real client recovers a lost guarded question ACK by replaying the same ID", async () => {
    const outcome = await exercise({ empty: true, questionTransport: "replay" });
    assert.deepEqual(outcome.result, { kind: "answer", answers: ["repair the checkpoint"] });
    assert.equal(outcome.questionBodies.length, 2);
    assert.equal(outcome.questionBodies[0], outcome.questionBodies[1]);
    assert.equal(outcome.questions, 1);
    assert.equal(outcome.pauseAttempts, 1);
    assert.ok(!stateStatuses(outcome.claim.run_id).includes("failed"));
  });

  it("authoritative guarded question HTTP refusal keeps the ordinary failure disposition", async () => {
    const outcome = await exercise({ empty: true, questionTransport: "refused" });
    assert.equal(outcome.result, undefined);
    assert.equal(outcome.questionBodies.length, 1);
    assert.ok(stateStatuses(outcome.claim.run_id).includes("failed"));
  });

  for (const options of [
    { empty: true, ownership: "completion" as const },
    { empty: true, questionTransport: "completionHold" as const },
    { empty: true, guardedExistingQuestion: true },
  ]) {
    it(`preserves a concurrent hold or unrelated question: ${JSON.stringify(options)}`, async () => {
      const outcome = await exercise(options);
      assert.equal(outcome.result, undefined);
      assert.equal(outcome.questions, 0);
      assert.equal(outcome.pauseAttempts, 1);
      assert.equal(outcome.workPreserved, true);
      assert.ok(!stateStatuses(outcome.claim.run_id).includes("failed"));
    });
  }

  it("guard refusal reporting an ordinary pause returns paused without waiting or terminal failure", async () => {
    const outcome = await exercise({ empty: true, guardedPause: true });
    assert.deepEqual(outcome.result, { kind: "paused" });
    assert.equal(outcome.questions, 0);
    assert.ok(!stateStatuses(outcome.claim.run_id).includes("failed"));
  });

  it("a refused guarded write cannot wait on an unrelated existing question", async () => {
    const outcome = await exercise({ empty: true, guardedExistingQuestion: true });
    assert.equal(outcome.result, undefined);
    assert.equal(outcome.questions, 0);
    assert.equal(api.workerPauseRequests.length, 1);
  });

  it("unsupported server asks an unguarded worker question and never calls pause", async () => {
    const outcome = await exercise({ supported: false });
    assert.equal(outcome.result?.kind, "answer");
    assert.equal(api.workerPauseRequests.length, 0);
    assert.equal(outcome.callbackReads, 0);
    assert.equal(api.states.find(s => s.body.status === "awaiting_input")?.body.require_running, undefined);
    assert.ok(JSON.stringify(api.messages(outcome.claim.run_id)).includes("uzi run pause"));
  });

  it("an unsupported generation-zero server still gets a bounded compatibility question", async () => {
    const outcome = await exercise({ supported: false, legacyGeneration: true });
    assert.equal(outcome.result?.kind, "answer");
    assert.equal(api.workerPauseRequests.length, 0);
    assert.equal(outcome.questions, 1);
  });

  it("refuses nonissue callbacks independently of the harness", async () => {
    const outcome = await exercise({ nonissue: true });
    assert.equal(outcome.result, undefined);
    assert.equal(api.workerPauseRequests.length, 0);
    assert.equal(outcome.questions, 0);
  });

  it("a running request refusal skips the checkpoint sink before asking", async () => {
    const outcome = await exercise({ requestRefused: true });
    assert.equal(outcome.result?.kind, "answer");
    assert.equal(outcome.published, 0);
    assert.equal(outcome.questions, 1);
  });

  for (const options of [{ wallPending: true }, { empty: true, guardedWall: true }]) {
    it(`a pending wall control wins at ${options.wallPending ? "request" : "question"} boundary`, async () => {
      const outcome = await exercise(options);
      assert.equal(outcome.result, undefined);
      assert.equal(outcome.questions, 0);
      assert.ok(!stateStatuses(outcome.claim.run_id).includes("failed"));
    });
  }

  it("unknown ownership ends once without a question or another attempt", async () => {
    const outcome = await exercise({ empty: true, unknown: true });
    assert.equal(outcome.result, undefined);
    assert.equal(api.workerPauseRequests.length, 1);
    assert.equal(outcome.callbackReads, 1);
    assert.equal(outcome.questions, 0);
  });

  it("unattended refusal never enters awaiting_input", async () => {
    const outcome = await exercise({ empty: true, unattended: true });
    assert.equal(outcome.result, undefined);
    assert.equal(outcome.questions, 0);
    assert.equal(api.workerPauseRequests.length, 1);
  });

  it("reuses the absolute question deadline across explicit retries", async () => {
    const outcome = await exercise({ empty: true, deadlineRetry: true });
    assert.equal(outcome.result, undefined);
    assert.equal(outcome.questions, 2);
    assert.equal(api.workerPauseRequests.length, 2);
    assert.ok(JSON.stringify(api.messages(outcome.claim.run_id)).includes("timed out"));
  });

  it("a sticky cancel wins before requesting any pause", async () => {
    const outcome = await exercise({ cancelledBefore: true });
    assert.deepEqual(outcome.result, { kind: "cancel" });
    assert.equal(api.workerPauseRequests.length, 0);
    assert.equal(outcome.published, 0);
  });

  for (const ownership of ["wall", "released", "foreign"] as const) {
    it(`preserves the existing ${ownership} ownership disposition without asking`, async () => {
      const outcome = await exercise({ empty: true, ownership });
      assert.equal(outcome.result, undefined);
      assert.equal(outcome.questions, 0);
      assert.ok(!stateStatuses(outcome.claim.run_id).includes("failed"));
    });
  }

  it("recognizes only exact trimmed nonempty park answers", () => {
    const verdicts: Array<[AnswerVerdict, boolean]> = [
      [{ kind: "cancel" }, false], [{ kind: "answer", answers: [] }, false],
      [{ kind: "answer", answers: [" ", " /park ", "/park"] }, true],
      [{ kind: "answer", answers: ["/park", "continue"] }, false],
      [{ kind: "answer", answers: ["/park later"] }, false],
    ];
    for (const [verdict, expected] of verdicts) assert.equal(isParkAnswer(verdict), expected);
  });
});

// issue #3239 — a pause park whose WIP marker could not be vetted (the rollback was refused) must
// fetch and publish nothing, report pause_failed and keep the clone. A core file slips past the scan
// so the backstop refuses its rollback. The park then undoes the exact marker the capture wrote with a
// compare-and-swap on the named run branch only: a branch still at the marker is rewound even when
// the agent switched HEAD to another branch B (B and HEAD are untouched); a branch that moved past the
// marker is left alone.
describe("RunRunner — pause park with an unvetted WIP marker (issue #3239)", () => {
  for (const scenario of ["branch-moved", "head-on-other-branch", "commit-rejects"] as const) {
    it(`reports pause_failed and publishes nothing (${scenario})`, async () => {
      const { gitlab } = fakeGitlab();
      const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-3239-pause-"));
      const rd = (cwd: string, ...args: string[]) =>
        execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim();
      const state = { armed: false, marker: "", branch: "" };
      const rGit = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
        gitleaksBin: defaultGitleaksShim(),
        wipCapture: {
          // A commit that wrote the marker and then rejected (timeout): the backstop never ran on it.
          get commitRejectsAfterWrite() { return state.armed && scenario === "commit-rejects"; },
          afterOmissionCheck: async (p) => { if (state.armed && scenario !== "commit-rejects") fs.writeFileSync(path.join(p, "core"), "tiny\n"); },
          beforeBackstop: async (p, marker) => {
            if (!state.armed) return;
            state.marker = marker;
            if (scenario === "head-on-other-branch") {
              rd(p, "branch", "B", marker);
              rd(p, "symbolic-ref", "HEAD", "refs/heads/B");
            }
          },
          beforeRollbackWrite: async (p, marker) => {
            if (!state.armed || scenario !== "branch-moved") return;
            // Swap the run branch to a sibling marker after the final recheck: the compare-and-swap is
            // refused while HEAD still names the run branch, at a wip(park) tip.
            const sibling = rd(p, "commit-tree", `${marker}^{tree}`, "-p", `${marker}^`, "-m", "wip(park): sibling marker");
            rd(p, "update-ref", `refs/heads/${state.branch}`, sibling, marker);
          },
        },
      }));
      const { calls, restore } = spyPublish();
      const seen: { subject?: string; head?: string; runTip?: string; runSubject?: string; bTip?: string | null; status?: string; trackedBefore?: string; tracked?: string | null } = {};
      try {
        const iid = 3243;
        const factory: ExecutorFactory = (runId) => ({
          homeDir: path.join(homeRoot, runId),
          executor: { run: async (ctx: RunContext): Promise<ExecutorResult> => {
            fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
            state.branch = ctx.branch;
            commitInTree(ctx.worktreePath, "WORK.txt", "committed\n");
            await ctx.checkpoint?.({ reap: false });
            seen.trackedBefore = rd(rGit.barePathFor(fx.originPath), "rev-parse", "--verify", `refs/uzi-runner/agent/issue-${iid}`);
            fs.writeFileSync(path.join(ctx.worktreePath, "WIP.txt"), "dirty\n");
            state.armed = true;
            const parked = await ctx.parkForPause?.({ completedCount: 1, total: 2 });
            state.armed = false;
            seen.subject = rd(ctx.worktreePath, "log", "-1", "--format=%s");
            seen.head = rd(ctx.worktreePath, "symbolic-ref", "HEAD");
            seen.runTip = rd(ctx.worktreePath, "rev-parse", `refs/heads/${ctx.branch}`);
            seen.runSubject = rd(ctx.worktreePath, "log", "-1", "--format=%s", `refs/heads/${ctx.branch}`);
            seen.bTip = (() => { try { return rd(ctx.worktreePath, "rev-parse", "--verify", "refs/heads/B"); } catch { return null; } })();
            seen.status = rd(ctx.worktreePath, "status", "--porcelain");
            seen.tracked = (() => { try { return rd(rGit.barePathFor(fx.originPath), "rev-parse", "--verify", `refs/uzi-runner/agent/issue-${iid}`); } catch { return null; } })();
            return parked ? { branch: ctx.branch, pausedAt: { completedCount: 1, total: 2 } } : { branch: ctx.branch };
          } },
        });
        const claim = gitlabClaim(iid);
        await new RunRunner(client, rGit, factory, nullLogger(), 20, undefined, {
          pollMs: 5, planApprovalTimeoutMs: 0, questionTimeoutMs: 600, gitlab, checkpointIntervalMs: 0,
          checkpointTestHooks: { retryBackoffMs: [0, 0] },
        }).execute(claim);

        if (scenario !== "commit-rejects") assert.ok(state.marker, "the rollback was refused (marker unvetted)");
        const statuses = stateStatuses(claim.run_id);
        assert.ok(statuses.includes("pause_failed"), `pause_failed reported, got ${JSON.stringify(statuses)}`);
        assert.ok(!statuses.includes("paused"), "the run did not park");
        assert.deepEqual(calls, [], "nothing was published");
        assert.ok(seen.trackedBefore, "the checkpoint established a tracking tip before the pause");
        assert.equal(seen.tracked, seen.trackedBefore, "the pause fetched nothing back, including a sibling marker");
        if (scenario === "branch-moved") {
          assert.equal(seen.head, `refs/heads/${state.branch}`);
          assert.ok(seen.runSubject!.startsWith("wip(park)"), "the moved branch (a sibling marker) was not rewound");
          assert.notEqual(seen.runTip, state.marker);
        } else if (scenario === "commit-rejects") {
          // The marker the rejected commit call wrote is recovered as this capture's own commit and
          // undone by the fenced compare-and-swap: content back to uncommitted, nothing published.
          assert.equal(seen.head, `refs/heads/${state.branch}`);
          assert.ok(!seen.runSubject!.startsWith("wip(park)"), `the run branch was rewound off the marker, got ${seen.runSubject}`);
          assert.match(seen.status!, /WIP\.txt/, "the WIP is back on disk, uncommitted");
        } else {
          assert.equal(seen.head, "refs/heads/B", "HEAD stays on B");
          assert.equal(seen.bTip, state.marker, "B was not rewound");
          assert.ok(!seen.runSubject!.startsWith("wip(park)"), `the run branch was rewound off the marker, got ${seen.runSubject}`);
          assert.notEqual(seen.runTip, state.marker);
        }
      } finally {
        restore();
        fs.rmSync(homeRoot, { recursive: true, force: true });
      }
    });
  }
});

// A clean tree is not evidence that an earlier failed rollback became safe.
for (const removed of [false, true]) for (const followup of ["pause", "limit"] as const) {
  it(`unsafe WIP refusal survives a second pause until history is clean: removed=${removed} followup=${followup}`, async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-3239-repeat-pause-"));
    const rd = (cwd: string, ...args: string[]) => execFileSync("git", ["-C", cwd, ...args],
      { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim();
    let armed = false, marker = "";
    const rGit = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
      gitleaksBin: defaultGitleaksShim(),
      wipCapture: {
        afterOmissionCheck: async p => { if (armed) fs.writeFileSync(path.join(p, "core"), "dump\n"); },
        beforeRollbackWrite: async (_p, sha) => {
          if (!armed) return;
          marker = sha;
          throw new Error("fixture: rollback write refused");
        },
      },
    }));
    // Model the pause's second CAS refusing too, so the actual unsafe marker remains.
    rGit.undoWipMarkerOnBranch = async () => false;
    const { calls, restore } = spyPublish();
    const seen: { second?: boolean; clean?: string; marker?: string } = {};
    try {
      const factory: ExecutorFactory = runId => ({ homeDir: path.join(homeRoot, runId), executor: {
        run: async ctx => {
          fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
          commitInTree(ctx.worktreePath, "WORK.txt", "committed\n");
          await ctx.checkpoint?.({ reap: false });
          fs.writeFileSync(path.join(ctx.worktreePath, "WIP.txt"), "dirty\n");
          armed = true;
          await ctx.parkForPause?.({ completedCount: 1, total: 2 });
          armed = false;
          seen.marker = marker;
          seen.clean = rd(ctx.worktreePath, "status", "--porcelain");
          if (removed) rd(ctx.worktreePath, "update-ref", `refs/heads/${ctx.branch}`, rd(ctx.worktreePath, "rev-parse", `${marker}^`), marker);
          if (followup === "limit") throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
          seen.second = await ctx.parkForPause?.({ completedCount: 1, total: 2 });
          // Stop this scripted executor at the observation boundary; finalization is not under test.
          return { branch: ctx.branch, pausedAt: { completedCount: 1, total: 2 } };
        },
      } });
      await new RunRunner(client, rGit, factory, nullLogger(), 20, undefined, {
        pollMs: 5, planApprovalTimeoutMs: 0, questionTimeoutMs: 600, gitlab, checkpointIntervalMs: 0,
        checkpointTestHooks: { retryBackoffMs: [0, 0] },
      }).execute(gitlabClaim(3244, { wait_on_limit: true }));
      assert.match(seen.marker ?? "", /^[0-9a-f]{40}$/, "a real unsafe marker was written");
      assert.equal(seen.clean, "", "the second capture starts from a clean worktree");
      if (followup === "pause") assert.equal(seen.second, removed, "only verified removal from history permits the next pause");
      assert.equal(calls.length, removed ? 1 : 0, "a clean tree cannot authorize publishing the unsafe marker");
    } finally { restore(); fs.rmSync(homeRoot, { recursive: true, force: true }); }
  });
}

describe("unsafe WIP flight quarantine", () => {
  function fixture() {
    const cache = new GitCache("unused", nullLogger());
    const runner = new RunRunner(client, cache, () => ({ executor: { run: async () => ({ branch: "branch" }) } }), nullLogger(), 20);
    const flight = { runId: "quarantine", runLog: nullLogger(), batcher: { emit() {} },
      preserveRecoveryClone: false, sourceJoinFailed: false } as unknown as Parameters<RunRunner["wipSafeTip"]>[0];
    const record = (marker?: string, unverifiedRefTip?: string | null) => runner["wipIncompleteHandler"](flight, "park")({
      omitted: [], omittedTotal: 0, markerCreated: false, rollbackRefused: true, marker, unverifiedRefTip,
    });
    return { cache, runner, flight, record };
  }

  it("checks all known markers and still blocks their later reintroduction without releasing retention", async () => {
    const f = fixture(), a = "a".repeat(40), b = "b".repeat(40), tip = "c".repeat(40);
    f.record(a); f.record(b);
    let present = b;
    f.cache.branchTip = async () => tip;
    f.cache.wipMarkerAbsentFromHistory = async (_path, _tip, marker) => marker !== present;
    assert.equal(await f.runner["wipSafeTip"](f.flight, "clone", "branch"), null);
    present = "";
    assert.equal(await f.runner["wipSafeTip"](f.flight, "clone", "branch"), tip);
    assert.equal(f.flight.wipMarkerUnsafe, false);
    assert.equal(f.flight.preserveRecoveryClone, true);
    assert.equal(f.flight.sourceJoinFailed, true);
    present = a;
    assert.equal(await f.runner["wipSafeTip"](f.flight, "clone", "branch"), null, "clearance is not permission to reintroduce a marker");
    assert.equal(f.flight.wipUnsafeMarker, undefined, "publication quarantine does not grant marker-undo identity");
  });

  it("quarantines a conflicting ref tip without authorizing its undo", async () => {
    const f = fixture(), written = "a".repeat(40), conflict = "b".repeat(40);
    f.record(written, conflict);
    f.cache.branchTip = async () => conflict;
    f.cache.wipMarkerAbsentFromHistory = async (_path, _tip, marker) => marker !== conflict;
    assert.equal(await f.runner["wipSafeTip"](f.flight, "clone", "branch"), null);
    assert.equal(f.flight.wipUnsafeMarker, written, "only the capture's own marker may be undone");
  });

  it("pins the proved candidate through the existing import fence", async () => {
    const f = fixture(), marker = "a".repeat(40), tip = "b".repeat(40);
    f.record(marker);
    f.cache.branchTip = async () => tip;
    f.cache.wipMarkerAbsentFromHistory = async () => true;
    Object.assign(f.runner, { owedOptions: async () => ({}) });
    let imports = 0;
    f.cache.fetchAgentBranch = async (_bare, _clone, _branch, _run, options) => {
      imports++;
      assert.equal(options.expectedCandidateSha, tip, "the proof's literal SHA reaches acquisition");
      throw new Error("checkpoint candidate changed during acquisition");
    };
    await assert.rejects(f.runner["fetchTracking"](f.flight, "bare", "clone", "branch"), /candidate changed/);
    assert.equal(imports, 1);
    imports = 0;
    await assert.rejects(f.runner["fetchTracking"](f.flight, "bare", "clone", "branch", {
      expectedCandidateSha: "c".repeat(40),
    }), /tracking preservation refused/);
    assert.equal(imports, 0, "an existing incompatible candidate fence is never replaced");
  });

  for (const unknown of ["missing", "overflow"] as const) {
    it(`fails closed on ${unknown} marker identity`, async () => {
      const f = fixture();
      if (unknown === "missing") f.record();
      else for (let i = 0; i < 70; i++) f.record(i.toString(16).padStart(40, "0"));
      f.cache.branchTip = async () => { throw new Error("unknown identity must not attempt clearance"); };
      assert.equal(await f.runner["wipSafeTip"](f.flight, "clone", "branch"), null);
      assert.ok(f.flight.wipUnsafeMarkers!.size <= 65, "bounded known identities plus the unknown sentinel");
      assert.equal(f.flight.wipMarkerUnsafe, true);
    });
  }
});
