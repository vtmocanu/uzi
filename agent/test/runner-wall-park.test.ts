import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { type ExecutorResult, type RunContext, type WallParkOutcome } from "../src/executor.js";
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

// ── PRD #1497 M2 — the runner's capture-first WALL PARK (enterWallPark) ─────────────────────────
//
// ctx.parkForWall delegates here. Unlike handlePausePark (publish-or-stay), the wall park is
// CAPTURE-FIRST (D4): a VERIFIED local capture makes the park, the publish is best-effort, and even
// an UNVERIFIABLE capture parks DEGRADED (flags stay set, clone + HOME retained, head reported
// empty) rather than failing. It NEVER reports pause_failed and NEVER continues on its own. An
// UNDELIVERABLE report (server unreachable / a 404 reclaim) ends the flight NON-TERMINAL keeping the
// work (D17); reportGenericFailure is UNREACHABLE from a wall trip. A running heartbeat that a server
// park has fenced surfaces the run's paused+budget_exhausted status on the stale_claim ACK, which the
// reportState closure turns into a ServerWallParkedError (retain-and-stop), not a plain StaleClaim.

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

function drain(stream: Readable): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    stream.on("data", (c: Buffer) => chunks.push(Buffer.from(c)));
    stream.on("end", () => resolve(Buffer.concat(chunks)));
    stream.on("error", reject);
  });
}

/** Spy on client.publishCheckpoint returning a LANDED publish, so captureHoldContext's best-effort
 *  publish reports published:true. */
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

/** Spy on client.publishCheckpoint returning a non-2xx PublishResult (a publish FAILURE) — the
 *  capture is still VERIFIED locally, so the park lands with published:false. */
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
    checkpointIntervalMs: 0,
  });
}

/** An executor that commits work, fetch-backs (mid-run checkpoint), then requests a WALL park.
 *  Mirrors the real sdk-executor: it latches ExecutorResult.walled when the park took (parked OR
 *  undeliverable — both keep the work and skip finalize), ends as a cancel on "cancelled", and
 *  finalizes on "refused". */
function wallFactory(homeRoot: string): {
  factory: ExecutorFactory;
  outcomes: (WallParkOutcome | undefined)[];
} {
  const outcomes: (WallParkOutcome | undefined)[] = [];
  const factory: ExecutorFactory = (runId) => ({
    homeDir: path.join(homeRoot, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
        commitInTree(ctx.worktreePath, "WORK.txt", "work before the wall\n");
        await ctx.checkpoint?.({ reap: false });
        const at = { completedCount: 2, total: 3 };
        const outcome = await ctx.parkForWall?.(at);
        outcomes.push(outcome);
        if (outcome === "parked" || outcome === "undeliverable") {
          return { branch: ctx.branch, walled: { reason: "run exceeded its wall-clock timeout" } };
        }
        if (outcome === "cancelled") throw new Error("run cancelled");
        return { branch: ctx.branch };
      },
    },
  });
  return { factory, outcomes };
}

/** An executor that reports a running heartbeat (ctx.reportIteration) — the report a server-side
 *  wall park FENCES, so its ACK carries disposition:"stale_claim" + status:"paused" +
 *  hold_reason:"budget_exhausted", which the reportState closure turns into a ServerWallParkedError.
 *  The executor does NOT catch it (mirrors the loop top): it propagates to executeClaim's catch. */
function heartbeatFactory(homeRoot: string): { factory: ExecutorFactory } {
  const factory: ExecutorFactory = (runId) => ({
    homeDir: path.join(homeRoot, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
        commitInTree(ctx.worktreePath, "WORK.txt", "work before the server park\n");
        await ctx.checkpoint?.({ reap: false });
        // The next heartbeat is fenced by the server-side park; reportIteration rethrows the
        // ServerWallParkedError (it does not swallow it), so this throws out of run().
        await ctx.reportIteration?.(1);
        return { branch: ctx.branch };
      },
    },
  });
  return { factory };
}

/** An executor that parks at the wall ONLY when a `wall` mode is pending (the re-claim + seed path).
 *  Mirrors the loop-top wall branch: it reads ctx.pauseModeRequested and, on 'wall', parks. */
function seededWallFactory(homeRoot: string): {
  factory: ExecutorFactory;
  outcomes: (WallParkOutcome | undefined)[];
} {
  const outcomes: (WallParkOutcome | undefined)[] = [];
  const factory: ExecutorFactory = (runId) => ({
    homeDir: path.join(homeRoot, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
        commitInTree(ctx.worktreePath, "WORK.txt", "work before the first turn\n");
        await ctx.checkpoint?.({ reap: false });
        if (ctx.pauseModeRequested?.() === "wall") {
          const outcome = await ctx.parkForWall?.({ completedCount: 0 });
          outcomes.push(outcome);
          if (outcome === "parked" || outcome === "undeliverable") {
            return { branch: ctx.branch, walled: { reason: "run exceeded its wall-clock timeout" } };
          }
        }
        return { branch: ctx.branch };
      },
    },
  });
  return { factory, outcomes };
}

const stateStatuses = (runId: string): string[] =>
  api.states.filter((s) => s.runId === runId).map((s) => s.body.status);

const messageKinds = (runId: string): string[] => api.messages(runId).map((m) => m.kind);

describe("RunRunner — capture-first wall park (PRD #1497 M2)", () => {
  it("parks (reports wall_park with a captured head) without pause_failed, and does not finalize", async () => {
    const { gitlab, calls: mrCalls } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1497-ok-"));
    const { restore } = spyPublish();
    try {
      const { factory, outcomes } = wallFactory(homeRoot);
      const claim = gitlabClaim(1501);
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.deepEqual(outcomes, ["parked"], "parkForWall returned parked (server ACKed paused)");
      assert.equal(api.wallParkRequests.length, 1, "one wall_park report was sent");
      const body = api.wallParkRequests[0]!.body;
      assert.ok(typeof body.head === "string" && body.head.length === 40, `a captured head was reported, got ${JSON.stringify(body.head)}`);
      assert.equal(body.published, true, "the checkpoint published, so published:true");
      const statuses = stateStatuses(claim.run_id);
      assert.ok(!statuses.includes("pause_failed"), "a wall park never reports pause_failed");
      assert.ok(!statuses.includes("completed") && !statuses.includes("failed"), "a parked run must NOT finalize");
      assert.equal(mrCalls.length, 0, "a parked run opens no merge request");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("a FAILED publish still parks — the wall_park carries published:false and the run parks", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1497-pubfail-"));
    const { restore } = spyPublishHttpError(500);
    try {
      const { factory, outcomes } = wallFactory(homeRoot);
      const claim = gitlabClaim(1502);
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.deepEqual(outcomes, ["parked"], "a verified LOCAL capture makes the park even when the publish fails");
      assert.equal(api.wallParkRequests.length, 1, "one wall_park report was sent");
      const body = api.wallParkRequests[0]!.body;
      assert.ok(typeof body.head === "string" && body.head.length === 40, "a verified head is still reported");
      assert.equal(body.published, false, "the publish failed, so published:false");
      const statuses = stateStatuses(claim.run_id);
      assert.ok(!statuses.includes("failed") && !statuses.includes("completed"), "the run parked, not failed/finalized");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("an UNVERIFIABLE capture parks DEGRADED — head reported empty, clone kept, a feed message notes unverified local work", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1497-degraded-"));
    const { restore } = spyPublish();
    // Force the capture to never verify: verifyRunnerTrackingCovers returns false, so
    // captureHoldContext returns verified:false / head:null and the park lands DEGRADED.
    const origVerify = git.verifyRunnerTrackingCovers.bind(git);
    (git as unknown as { verifyRunnerTrackingCovers: unknown }).verifyRunnerTrackingCovers =
      async () => false;
    try {
      const { factory, outcomes } = wallFactory(homeRoot);
      const claim = gitlabClaim(1503);
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.deepEqual(outcomes, ["parked"], "a degraded park still parks (D4: never fails)");
      assert.equal(api.wallParkRequests.length, 1, "one wall_park report was sent");
      assert.equal(api.wallParkRequests[0]!.body.head, "", "an unverified capture reports an EMPTY head");
      const statuses = stateStatuses(claim.run_id);
      assert.ok(!statuses.includes("failed") && !statuses.includes("completed"), "the degraded park did not fail/finalize");
      const feed = api.messages(claim.run_id);
      const note = feed.find(
        (m) => m.kind === "status" && /lives on this worker only|could not be verified/i.test(String(m.payload.text ?? "")),
      );
      assert.ok(note, "a feed message notes the latest local work is unverified and lives on this worker only");
      // The clone is kept (the run is non-terminal for resume): the HOME dir survives.
      assert.ok(fs.existsSync(path.join(homeRoot, claim.run_id)), "the run's HOME is preserved for resume");
    } finally {
      (git as unknown as { verifyRunnerTrackingCovers: unknown }).verifyRunnerTrackingCovers = origVerify;
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("an UNDELIVERABLE wall_park (server unreachable) ends the flight NON-TERMINAL with clone + HOME kept and reportGenericFailure NEVER called", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1497-undeliverable-"));
    const { restore } = spyPublish();
    // The wall-park endpoint answers 404 (a reclaim) — the client throws, so the report is
    // UNDELIVERABLE (D17): retain everything, report NOTHING terminal.
    api.setWallParkResponse("paused", 404);
    try {
      const { factory, outcomes } = wallFactory(homeRoot);
      const claim = gitlabClaim(1504);
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.deepEqual(outcomes, ["undeliverable"], "the undeliverable report is surfaced as 'undeliverable'");
      const statuses = stateStatuses(claim.run_id);
      assert.ok(
        !statuses.includes("failed"),
        `reportGenericFailure must NOT fire on an undeliverable wall park, got ${JSON.stringify(statuses)}`,
      );
      assert.ok(!statuses.includes("completed"), "no terminal completed report either");
      assert.ok(fs.existsSync(path.join(homeRoot, claim.run_id)), "the HOME is retained for a resume");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("a running heartbeat that a SERVER park fenced ends as parked (ServerWallParkedError) with clone + HOME kept and no terminal report", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1497-serverpark-"));
    const { restore } = spyPublish();
    try {
      const { factory } = heartbeatFactory(homeRoot);
      const claim = gitlabClaim(1505);
      // The next iteration heartbeat is fenced by the server-side park: a 409 carrying
      // disposition:stale_claim + status:paused + hold_reason:budget_exhausted.
      api.failStateWhen(
        claim.run_id,
        (b) => b.status === "running" && typeof b.iteration_count === "number",
        { httpStatus: 409, runStatus: "paused", disposition: "stale_claim", holdReason: "budget_exhausted" },
      );
      await runnerWithGit(factory, gitlab).execute(claim);

      const statuses = stateStatuses(claim.run_id);
      assert.ok(
        !statuses.includes("failed") && !statuses.includes("completed"),
        `a server-parked flight ends NON-TERMINAL, got ${JSON.stringify(statuses)}`,
      );
      assert.ok(fs.existsSync(path.join(homeRoot, claim.run_id)), "clone + HOME retained for a resume on another incarnation");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("an explicit wall_park after the server already parked is answered paused (idempotent) and treated as parked", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1497-idempotent-"));
    const { restore } = spyPublish();
    // The server already parked the row; the wall_park report is answered 200/paused (idempotent).
    api.setWallParkResponse("paused", 200);
    try {
      const { factory, outcomes } = wallFactory(homeRoot);
      const claim = gitlabClaim(1506);
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.deepEqual(outcomes, ["parked"], "an idempotent paused answer is treated as parked");
      const statuses = stateStatuses(claim.run_id);
      assert.ok(!statuses.includes("failed") && !statuses.includes("completed"), "no terminal report on an idempotent park");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("a re-claim with pause_pending + pause_mode='wall' parks before the first turn (seed → mode → wall seam)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1497-reclaim-"));
    const { restore } = spyPublish();
    try {
      const { factory, outcomes } = seededWallFactory(homeRoot);
      // A resume: the durable pause columns survived the requeue, so the claim re-delivers the
      // pending wall request. buildFlight seeds the steering channel (seedPauseRequested('wall')).
      const claim = gitlabClaim(1507, { pause_pending: true, pause_mode: "wall" });
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.deepEqual(outcomes, ["parked"], "the resumed run parked at its first boundary from the seeded wall mode");
      const statuses = stateStatuses(claim.run_id);
      assert.ok(!statuses.includes("completed") && !statuses.includes("failed"), "a re-claimed wall park does not finalize");
      assert.ok(!messageKinds(claim.run_id).includes("pause_failed"), "a wall park never reports pause_failed");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});
