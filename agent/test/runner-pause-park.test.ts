import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { nullLogger } from "./helpers.js";
import {
  api,
  client,
  fakeGitlab,
  git,
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

/** Spy on client.publishCheckpoint: buffer the pack and return a landed publish. */
function spyPublish(): { restore: () => void } {
  const orig = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    _runId: string,
    _tipOid: string,
    pack: Readable,
  ) => {
    await drain(pack);
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return {
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
): RunRunner {
  return new RunRunner(client, git, factory, nullLogger(), 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    questionTimeoutMs: 600,
    gitlab,
    // Disable the mid-run time-based publish so ctx.checkpoint({reap:false}) only fetches the
    // committed work back to the tracking ref — handlePausePark then does the publish itself,
    // so these tests exercise its publish path directly.
    checkpointIntervalMs: 0,
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
 *  before any work. The clone's branch tip is still its base commit, so handlePausePark's empty-pack
 *  detection must park cleanly rather than reporting pause_failed. */
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

const stateStatuses = (runId: string): string[] =>
  api.states.filter((s) => s.runId === runId).map((s) => s.body.status);

const messageKinds = (runId: string): string[] => api.messages(runId).map((m) => m.kind);

describe("RunRunner — owner-requested pause park (PRD #1190 M2)", () => {
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

  it("parks cleanly on an EMPTY pack (a `now` pause before any commit), not pause_failed", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1190-empty-"));
    // A FAILING publish spy: if the empty-pack case wrongly reached the publish path it would report
    // pause_failed. The empty-pack detection must bypass publish entirely and park cleanly, so with
    // this spy in place a `paused` (not `pause_failed`) proves publish was never attempted.
    const { restore } = spyPublishHttpError(500);
    try {
      const { factory, parkResults } = emptyPauseFactory(homeRoot);
      const claim = gitlabClaim(1105);
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.deepEqual(parkResults, [true], "an empty-pack pause parks cleanly (nothing to lose)");
      const statuses = stateStatuses(claim.run_id);
      assert.ok(statuses.includes("paused"), `a paused report was sent, got ${JSON.stringify(statuses)}`);
      assert.ok(!statuses.includes("pause_failed"), "an empty pack must NOT report pause_failed");
      assert.ok(!statuses.includes("completed"), "a parked run does not finalize");
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
