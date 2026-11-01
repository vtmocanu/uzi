import { afterEach, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { ExecutorResult, IterationParkOutcome, RunContext } from "../src/executor.js";
import type { SteeringChannel } from "../src/steering.js";
import { recordingLogger } from "./helpers.js";
import { quiesceRunAttempt } from "../src/run-quiescence.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";
import { commitInTree, drain, enabledRecovery, GIT_ENV, installRecoveryRootCleanup, spyCustodySettle } from "./runner-codex-sinks-fixture.js";

installHarness();
installRecoveryRootCleanup();

// The runner catches executor errors. Preserve assertion failures independently so a
// swallowed assertion can never make a retention test green.
let callbackErrors: unknown[] = [];
afterEach(() => {
  const errors = callbackErrors;
  callbackErrors = [];
  assert.deepEqual(errors, [], "executor assertions completed without being swallowed");
});

const at = { iterationCount: 10, maxIterations: 10 };
const bytes = "iteration work awaiting continuation\n";
const sid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";
function readGit(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim();
}
function plantTranscript(home: string): void {
  const dir = path.join(home, ".claude", "projects", "-runner-iteration");
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, sid + ".jsonl"), "{}\n");
}

const cases = [
  "capture success", "degraded capture", "failed publication", "unproven quiescence",
  "committed park lost response", "404", "503", "unmodelled reply", "generation change",
  "cancel during capture", "cancel during report", "permanent failure during sink",
  "refused running prior_attempt", "refused running pending_control", "refused running stale_claim",
] as const;
for (const mode of cases) {
  it(`iteration park: ${mode} retains clone HOME and custody without finalize or redrive`, async t => {
    const iid = 359700 + cases.indexOf(mode);
    const claim = gitlabClaim(iid, { kind: "issue", claim_generation: 7, config: { iteration_park_version: 1 } });
    const { gitlab, calls } = fakeGitlab();
    const { coord, archive } = enabledRecovery();
    const { logger, lines } = recordingLogger();
    const outcomes: IterationParkOutcome[] = [];
    let turns = 0, sourceReads = 0, verified = 0, publications = 0, episodes = 0;
    let steering: SteeringChannel | undefined;
    let flight: unknown;
    let samePromise = false;
    let sinkEntered = false;
    const executor = {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        try {
          turns++;
          fs.mkdirSync(homeDir, { recursive: true });
          fs.writeFileSync(path.join(homeDir, "SESSION"), "session retained");
          commitInTree(ctx.worktreePath, "COMMITTED.txt", "committed iteration work\n");
          fs.writeFileSync(path.join(ctx.worktreePath, "DIRTY.txt"), bytes);
          const first = ctx.parkForIteration!(at);
          sinkEntered = true;
          const second = ctx.parkForIteration!(at);
          samePromise = first === second;
          const outcome = await first;
          assert.equal(await second, outcome);
          // Calling again after settlement cannot renew the report/capture budget.
          assert.equal(await ctx.parkForIteration!(at), outcome);
          outcomes.push(outcome);
          assert.equal(ctx.takeWallParkRefresh?.(), undefined, "iteration refusals never rearm wall control");
          return { branch: ctx.branch, iterationParked: { outcome } };
        } catch (error) { callbackErrors.push(error); throw error; }
      },
    };
    const runner = runnerWith(() => ({ executor, homeDir }), gitlab, undefined, logger, {
      recovery: coord, checkpointIntervalMs: 0, recoveryRetryMs: 1,
      quiesceRun: async request => mode === "unproven quiescence" && request.site === "iteration_park"
        ? { process: { state: "unverified", processes: [], killed: [], detail: "iteration fixture proof refused" },
            docker: { state: "not_wired", removed: [], detail: "" } }
        : quiesceRunAttempt(request),
    });
    const custody = spyCustodySettle(runner);
    const internal = runner as unknown as {
      runGatedSink: (f: { steering: SteeringChannel }, action: () => Promise<unknown>) => Promise<unknown>;
      newCaptureEpisode: (...args: unknown[]) => unknown;
      handlePermanentFailure: (...args: unknown[]) => Promise<void>;
      reportGenericFailure: (...args: unknown[]) => Promise<void>;
    };
    let genericFailures = 0, finalizes = 0, hookCalls = 0;
    const gated = internal.runGatedSink.bind(runner);
    t.mock.method(internal, "runGatedSink", (f: { steering: SteeringChannel }, action: () => Promise<unknown>) => {
      flight = f;
      steering = f.steering;
      return gated(f, action);
    });
    const episode = internal.newCaptureEpisode.bind(runner);
    t.mock.method(internal, "newCaptureEpisode", (...args: unknown[]) => { episodes++; return episode(...args); });
    t.mock.method(internal, "reportGenericFailure", async () => { genericFailures++; });
    t.mock.method(git, "pushBranch", async () => { finalizes++; });
    const status = git.worktreeStatus.bind(git);
    t.mock.method(git, "worktreeStatus", async (...args: Parameters<typeof status>) => {
      if (sinkEntered) sourceReads++;
      return status(...args);
    });
    const verify = git.verifyRunnerTrackingCovers.bind(git);
    t.mock.method(git, "verifyRunnerTrackingCovers", async (...args: Parameters<typeof verify>) => {
      const result = await verify(...args);
      if (mode === "degraded capture") return false;
      if (result) verified++;
      if (mode === "cancel during capture") steering!.markCancelled();
      return result;
    });
    t.mock.method(client, "publishCheckpoint", async (_id: string, _head: string, pack: Parameters<typeof client.publishCheckpoint>[2]) => {
      publications++;
      await drain(pack);
      if (mode === "permanent failure during sink") {
        hookCalls++;
        await internal.handlePermanentFailure(claim, flight, "iteration fixture permanent failure");
      }
      return mode === "failed publication"
        ? { ok: false as const, httpStatus: 503 }
        : { ok: true as const, body: { published: true, ref: "refs/uzi-checkpoints/iteration" } };
    });
    const report = client.reportIterationPark.bind(client);
    t.mock.method(client, "reportIterationPark", async (...args: Parameters<typeof report>) => {
      const reply = await report(...args);
      if (mode === "cancel during report") steering!.markCancelled();
      return reply;
    });
    if (mode === "committed park lost response") api.commitIterationParkThenLoseFirstReply();
    if (mode === "404" || mode === "503") api.setIterationParkResponse({ error: "unknown delivery" }, Number(mode));
    if (mode === "unmodelled reply") api.setIterationParkResponse({ run: { status: "paused" } });
    if (mode === "generation change") api.setIterationParkResponse({
      applied: true, status: "paused", hold_reason: "iteration_budget_exhausted", claim_generation: 8,
    });
    if (mode.startsWith("refused running ")) api.setIterationParkRefusal(
      mode.endsWith("prior_attempt") ? "prior_attempt" : mode.endsWith("pending_control") ? "pending_control" : "stale_claim",
      "running", mode.endsWith("stale_claim") ? 8 : 7,
    );

    await runner.execute(claim);
    const stopped = ["404", "503", "unmodelled reply", "generation change", "cancel during capture", "cancel during report"].includes(mode)
      || mode.startsWith("refused");
    assert.ok(outcomes.length > 0, JSON.stringify(lines));
    assert.deepEqual(outcomes, [stopped ? "retained-stop" : "parked"]);
    assert.equal(turns, 1, "no next model turn after the sink");
    assert.equal(samePromise, true, "repeated callbacks join the exact same promise");
    assert.equal(episodes, 1, "one shared capture episode");
    const requests = api.iterationParkRequests;
    const count = ["404", "503", "unmodelled reply", "generation change", "committed park lost response"].includes(mode) ? 2
      : mode === "cancel during capture" ? 0 : 1;
    assert.equal(requests.length, count, "at most two requests; typed refusal is never redriven");
    for (const { body } of requests) {
      assert.equal(body.claim_generation, 7);
      assert.equal(body.iteration_count, 10);
      assert.equal(body.max_iterations, 10);
      assert.equal(body.head === "", mode === "degraded capture" || mode === "unproven quiescence");
      assert.equal(body.published, !["failed publication", "degraded capture", "unproven quiescence"].includes(mode));
    }
    if (requests.length === 2) assert.deepEqual(requests[0]!.body, requests[1]!.body, "retry preserves exact fenced evidence");
    assert.equal(finalizes, 0);
    assert.equal(genericFailures, 0);
    assert.equal(calls.length, 0, "no MR or forge finalization");
    assert.equal(api.completionPermitRequests.length, 0);
    assert.equal(api.wallParkRequests.length, 0, "separate iteration control never enters wall park");
    assert.ok(!api.states.some(s => s.runId === claim.run_id && ["failed", "completed", "pause_failed"].includes(s.body.status!)));
    assert.ok(fs.existsSync(worktreeDirFor(iid)), "clone retained");
    assert.equal(fs.readFileSync(path.join(homeDir, "SESSION"), "utf8"), "session retained");
    assert.equal(fs.readFileSync(path.join(worktreeDirFor(iid), "DIRTY.txt"), "utf8"), bytes);
    assert.equal(custody(), 0, "enabled recovery never settles away custody");
    assert.equal(archive.releaseCalls.length, 0);
    const bare = git.barePathFor(fx.originPath);
    const journal = JSON.parse(readGit(bare, "config", `uzi-recovery.agent/issue-${iid}.clone`));
    assert.equal(journal.clonePath, worktreeDirFor(iid), "real runner clone journal retained");
    assert.equal(journal.runId, claim.run_id, "retained journal belongs to this run");
    if (mode === "unproven quiescence") {
      assert.equal(sourceReads, 0, "unproven writers forbid capture reads");
      assert.equal(publications, 0);
    } else if (!["degraded capture", "cancel during capture"].includes(mode)) {
      assert.ok(verified > 0, "actual capture verified");
      assert.equal(readGit(bare, "show", requests[0]!.body.head + ":DIRTY.txt"), bytes.trim());
    }
    if (mode === "permanent failure during sink") assert.equal(hookCalls, 1, "permanent-failure hook actually ran inside sink");
  });
}

for (const resume of ["retained journal compatible initial claim", "unavailable", "attempt paths"] as const) {
  it(`iteration park: same-worker next generation ${resume}`, async t => {
    const compatible = resume === "retained journal compatible initial claim";
    const iid = 359750 + ["retained journal compatible initial claim", "unavailable", "attempt paths"].indexOf(resume);
    const claim = gitlabClaim(iid, { kind: "issue", claim_generation: 7, session_id: sid, config: { iteration_park_version: 1 } });
    // Resolve the compatible first claim before execute; the unavailable control has no transcript.
    if (compatible) plantTranscript(homeDir);
    const { coord, archive } = enabledRecovery();
    const { logger, lines } = recordingLogger();
    const { gitlab, calls } = fakeGitlab();
    const seen: (string | undefined)[] = [];
    const clones: string[] = [];
    const outcomes: IterationParkOutcome[] = [];
    let executions = 0;
    const runner = runnerWith(() => ({ homeDir, executor: {
      run: async (ctx): Promise<ExecutorResult> => {
        try {
          seen.push(ctx.sessionId ?? undefined);
          clones.push(ctx.worktreePath);
          executions++;
          if (executions === 1) {
            commitInTree(ctx.worktreePath, "COMMITTED.txt", "committed before iteration park\n");
            fs.writeFileSync(path.join(homeDir, "SESSION"), "session retained");
          } else {
            assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "CONTINUE.txt"), "utf8"), "ordinary iteration continuation\n", "retained dirty work adopted before executor writes");
            assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "COMMITTED.txt"), "utf8"), "committed before iteration park\n");
            assert.equal(fs.readFileSync(path.join(homeDir, "SESSION"), "utf8"), "session retained");
          }
          plantTranscript(homeDir);
          await ctx.onSessionId?.(sid);
          fs.writeFileSync(path.join(ctx.worktreePath, "CONTINUE.txt"), "ordinary iteration continuation\n");
          const outcome = await ctx.parkForIteration!(at);
          outcomes.push(outcome);
          return { branch: ctx.branch, iterationParked: { outcome } };
        } catch (error) { callbackErrors.push(error); throw error; }
      },
    } }), gitlab, undefined, logger, {
      recovery: resume === "attempt paths" ? coord : undefined, checkpointIntervalMs: 0, recoveryRetryMs: 1,
    });
    const custody = spyCustodySettle(runner);
    let genericFailures = 0, pushes = 0;
    // Exercise phaseResume\u0027s existing effectiveAttemptPaths rule without a Docker daemon.
    if (resume === "attempt paths") t.mock.method(git, "recoveryAttemptMode", async () => true);
    t.mock.method(runner as unknown as { reportGenericFailure: () => Promise<void> }, "reportGenericFailure", async () => { genericFailures++; });
    t.mock.method(git, "pushBranch", async () => { pushes++; });
    await runner.execute(claim);
    assert.ok(outcomes.length > 0, JSON.stringify(lines));
    assert.deepEqual(outcomes, ["parked"]);
    assert.deepEqual(seen, [compatible ? sid : undefined], JSON.stringify(lines));
    const initialFeed = api.messages(claim.run_id);
    assert.ok(initialFeed.some(m => m.payload.event === (compatible ? "resume_continued" : "resume_lineage_break")));
    if (compatible) {
      assert.equal(clones[0], worktreeDirFor(iid), "compatible initial claim uses the ordinary same cwd");
      assert.ok(!initialFeed.some(m => m.payload.event === "resume_lineage_break"));
    } else {
      assert.ok(initialFeed.some(m => /WITHOUT its earlier context/.test(String(m.payload.text))));
    }
    assert.ok(fs.existsSync(clones[0]!));
    assert.ok(fs.existsSync(path.join(homeDir, ".claude")));
    if (resume === "unavailable") fs.rmSync(path.join(homeDir, ".claude"), { recursive: true });
    const before = api.messages(claim.run_id).length;
    // The owner resumes and the server reclaims this run before the new invocation.
    api.setOwnershipStatus(claim.run_id, "running", 8);
    await runner.execute({ ...claim, claim_generation: 8, session_id: sid,
      last_seq: api.messages(claim.run_id).at(-1)?.seq ?? 0 });
    assert.deepEqual(seen, [compatible ? sid : undefined, undefined], JSON.stringify(lines.slice(-12)));
    assert.equal(executions, 2, "exactly one executor run per claim");
    assert.deepEqual(outcomes, ["parked", "parked"]);
    const feed = api.messages(claim.run_id).slice(before);
    // Retained-journal adoption uses an attempt cwd even without an explicit recovery coordinator.
    assert.ok(feed.some(m => m.payload.event === "resume_lineage_break"));
    assert.ok(!feed.some(m => m.payload.event === "resume_continued"));
    assert.ok(feed.some(m => /WITHOUT its earlier context/.test(String(m.payload.text))));
    assert.notEqual(clones[0], clones[1], "fresh attempt cwd cannot resume the earlier session");
    assert.ok(feed.some(m => m.payload.reason === "cwd_changed_attempt_path"));
    assert.equal(genericFailures, 0);
    assert.equal(pushes, 0);
    assert.equal(custody(), 0);
    assert.equal(archive.releaseCalls.length, 0);
    assert.equal(calls.length, 0);
    assert.equal(api.iterationParkRequests.length, 2);
    assert.ok(fs.existsSync(clones[1]!));
    const journal = JSON.parse(readGit(git.barePathFor(fx.originPath), "config", `uzi-recovery.agent/issue-${iid}.clone`));
    assert.equal(journal.clonePath, clones[1]);
    assert.equal(journal.runId, claim.run_id);
    assert.ok(fs.existsSync(homeDir));
    assert.ok(!api.states.some(s => ["failed", "completed"].includes(s.body.status!)));
  });
}
