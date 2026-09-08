import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { GitCache, PendingRecoveryCaptureError } from "../src/git.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

const GIT_ENV = {
  ...process.env, GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0",
};
function gitRead(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim();
}
function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => { resolve = done; });
  return { promise, resolve };
}
function factoryFor(claim: ReturnType<typeof gitlabClaim>, committed = false) {
  let clone = "";
  let calls = 0;
  const runHome = path.join(homeDir, claim.run_id);
  const factory: ExecutorFactory = () => ({
    homeDir: runHome,
    executor: {
      run: async (ctx) => {
        calls++;
        clone = ctx.worktreePath;
        fs.mkdirSync(runHome, { recursive: true });
        fs.writeFileSync(path.join(runHome, "session"), "transcript");
        fs.mkdirSync(skillsPluginDir(clone), { recursive: true });
        fs.writeFileSync(path.join(skillsPluginDir(clone), "marker"), "plugin");
        fs.writeFileSync(path.join(clone, "ONLY_COPY.txt"), "must survive recovery\n");
        if (committed) {
          gitRead(clone, "add", "ONLY_COPY.txt");
          gitRead(clone, "-c", "user.name=test", "-c", "user.email=test@example.com",
            "-c", "commit.gpgsign=false", "commit", "-m", "fixture work");
        }
        throw new TransientRecoveryError();
      },
    },
  });
  return { factory, runHome, clone: () => clone, calls: () => calls };
}

describe("recovery capture retry and restart safety (#1197)", () => {
  for (const failure of ["wip", "fetch", "verify"] as const) {
    it(`${failure} failure retains active work until capture verifies`, async () => {
      const { gitlab } = fakeGitlab();
      const iid = failure === "wip" ? 1301 : failure === "fetch" ? 1302 : 1303;
      const claim = gitlabClaim(iid);
      const fixture = factoryFor(claim, failure === "fetch");
      const secondFailure = deferred();
      let attempts = 0;
      let allowCapture = false;
      const fail = () => { if (++attempts >= 2) secondFailure.resolve(); };
      const marker = git.commitWipMarker.bind(git);
      const fetch = git.fetchAgentBranch.bind(git);
      const verify = git.verifyRunnerTrackingCovers.bind(git);
      if (failure === "wip") git.commitWipMarker = async (...args) => {
        if (allowCapture) return marker(...args);
        fail(); return false;
      };
      if (failure === "fetch") git.fetchAgentBranch = async (...args) => {
        if (allowCapture) return fetch(...args);
        fail(); throw new Error("injected fetch failure");
      };
      if (failure === "verify") git.verifyRunnerTrackingCovers = async (...args) => {
        if (allowCapture) return verify(...args);
        fail(); return false;
      };
      const runner = runnerWith(fixture.factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
      const execution = runner.execute(claim);
      try {
        await Promise.race([
          secondFailure.promise,
          execution.then(() => { throw new Error("execution exited before retrying unverified capture"); }),
        ]);
        assert.equal(api.states.some((s) => s.body.status === "recovery_wait"), false);
        assert.equal(fs.readFileSync(path.join(fixture.clone(), "ONLY_COPY.txt"), "utf8"), "must survive recovery\n");
        assert.equal(fs.existsSync(path.join(fixture.runHome, "session")), true);
        assert.equal(fs.existsSync(path.join(skillsPluginDir(fixture.clone()), "marker")), true);
        allowCapture = true;
        await execution;
        assert.equal(fixture.calls(), 1, "capture retries never invoke the model again");
        assert.equal(api.states.filter((s) => s.body.status === "recovery_wait").length, 1);
        assert.equal(api.states.some((s) => s.body.status === "failed"), false);
        const bare = git.barePathFor(fx.originPath);
        assert.equal(gitRead(bare, "show", `refs/uzi-runner/agent/issue-${iid}:ONLY_COPY.txt`), "must survive recovery");
        assert.equal(fs.existsSync(fixture.clone()), false, "only verified work permits clone cleanup");
      } finally {
        allowCapture = true;
        runner.shutdown();
        await execution;
      }
    });
  }

  it("shutdown retains source and journal; a new runner captures them before reseeding", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1304;
    const claim = gitlabClaim(iid);
    const fixture = factoryFor(claim);
    const failed = deferred();
    git.commitWipMarker = async () => { failed.resolve(); return false; };
    const first = runnerWith(fixture.factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    const execution = first.execute(claim);
    await Promise.race([failed.promise, execution.then(() => { throw new Error("capture path not reached"); })]);
    first.shutdown();
    await execution;
    assert.equal(api.states.some((s) => s.body.status === "recovery_wait"), false);
    assert.equal(fs.readFileSync(path.join(fixture.clone(), "ONLY_COPY.txt"), "utf8"), "must survive recovery\n");
    assert.equal(fs.existsSync(path.join(fixture.runHome, "session")), true);

    const restartedGit = new GitCache(fx.dataDir, nullLogger());
    const bare = restartedGit.barePathFor(fx.originPath);
    await assert.rejects(
      restartedGit.createOrAttachRunnerClone(bare, iid, claim.run_id),
      PendingRecoveryCaptureError,
      "the durable worker journal blocks same-run destructive reseeding",
    );
    await assert.rejects(
      restartedGit.createOrAttachRunnerClone(bare, iid, "99999999-9999-4999-8999-999999999999"),
      /another run/,
      "a foreign run cannot adopt or erase retained work",
    );
    await assert.rejects(
      restartedGit.runnerCloneForBranch(bare, `agent/issue-${iid}`, "different-kind-clone", "99999999-9999-4999-8999-999999999999"),
      /another run/,
      "a different clone key cannot bypass the retained branch ownership journal",
    );
    let modelStarted = false;
    const restarted = new RunRunner(client, restartedGit, () => ({
      homeDir: fixture.runHome,
      executor: { run: async () => { modelStarted = true; throw new Error("must capture first"); } },
    }), nullLogger(), 20, undefined, { pollMs: 5, gitlab, recoveryRetryMs: 5 });
    await restarted.execute(claim);
    assert.equal(modelStarted, false, "the retained source is captured before any new SDK run");
    assert.equal(api.states.filter((s) => s.body.status === "recovery_wait").length, 1);
    const reseeded = await restartedGit.createOrAttachRunnerClone(bare, iid, claim.run_id);
    assert.equal(fs.readFileSync(path.join(reseeded.path, "ONLY_COPY.txt"), "utf8"), "must survive recovery\n");
    assert.equal(reseeded.wipRecovered, true);
  });

  it("a failed park report retries while the worker stays alive", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1305);
    const fixture = factoryFor(claim);
    api.failStateWhen(claim.run_id, (body) => body.status === "recovery_wait", { httpStatus: 400 });
    let reports = 0;
    const original = client.reportState.bind(client);
    client.reportState = async (runId, body) => {
      if (body.status === "recovery_wait") reports++;
      return original(runId, body);
    };
    await runnerWith(fixture.factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(reports, 2, "a failed report cannot abandon a running row on a healthy worker");
    assert.equal(api.states.filter((s) => s.body.status === "recovery_wait").length, 1);
    assert.equal(fs.existsSync(path.join(fixture.runHome, "session")), true);
  });

  it("a failed pre-work ownership write never starts the model", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1306);
    const fixture = factoryFor(claim);
    let attempted = false;
    git.markRecoveryCapture = async () => {
      attempted = true;
      throw new Error("injected ownership journal write failure");
    };
    await runnerWith(fixture.factory, gitlab).execute(claim);
    assert.equal(attempted, true, "the ownership write was reached");
    assert.equal(fixture.calls(), 0, "never create model work without a durable ownership record");
    assert.equal(api.states.some((s) => s.body.status === "recovery_wait"), false);
    assert.ok(api.states.some((s) => s.body.status === "failed"
      && /ownership journal write failure/.test(s.body.failure_reason ?? "")));
  });

  it("owner cancellation interrupts failed capture retries", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1307);
    const fixture = factoryFor(claim);
    // Mirror Service.SetState: the cancel input stamped stop_kind='cancelled',
    // and only the worker's subsequent failed report performs CancelRunByWorker.
    let stopRequested = false;
    api.onState(claim.run_id, (body) => {
      const status = body.status === "failed" && stopRequested ? "cancelled" : body.status;
      api.setOwnershipStatus(claim.run_id, status);
      api.overrideStateStatus(claim.run_id, status);
    });
    const failed = deferred();
    git.commitWipMarker = async () => { failed.resolve(); return false; };
    const runner = runnerWith(fixture.factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    const execution = runner.execute(claim);
    await Promise.race([failed.promise, execution.then(() => { throw new Error("capture path not reached"); })]);
    stopRequested = true;
    api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
    await execution;
    assert.equal((await client.getRunOwnership(claim.run_id)).status, "cancelled",
      "consuming the cancel input alone must not leave the authoritative run running");
    assert.ok(api.states.some((s) => s.body.status === "failed" && s.body.failure_reason === "run cancelled"));
    assert.equal(api.states.some((s) => s.body.status === "recovery_wait"), false);
    assert.equal(fs.existsSync(fixture.clone()), false, "explicit cancellation keeps normal cleanup");
    assert.equal(fs.existsSync(fixture.runHome), false);
  });

  it("same-run claims wait for delayed ACK and cleanup, then refresh their message cursors", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1308);
    const ackReached = deferred();
    const releaseAck = deferred();
    const cleanupReached = deferred();
    const releaseCleanup = deferred();
    const secondStarted = deferred();
    const releaseSecond = deferred();
    let firstEmit: import("../src/executor.js").RunContext["emit"] | undefined;
    let factories = 0;
    const factory: ExecutorFactory = () => {
      const attempt = ++factories;
      return {
        homeDir: path.join(homeDir, claim.run_id),
        executor: {
          run: async (ctx) => {
            if (attempt === 1) firstEmit = ctx.emit;
            if (attempt === 2) { secondStarted.resolve(); await releaseSecond.promise; }
            ctx.emit({ kind: "status", payload: { text: `execution-${attempt}` } });
            throw new TransientRecoveryError();
          },
        },
      };
    };
    const originalReport = client.reportState.bind(client);
    let parks = 0;
    client.reportState = async (runId, body) => {
      const ack = await originalReport(runId, body);
      if (body.status === "recovery_wait" && ++parks === 1) {
        ackReached.resolve();
        await releaseAck.promise;
      }
      return ack;
    };
    const originalRemove = git.removeRunnerClone.bind(git);
    let removals = 0;
    git.removeRunnerClone = async (clone) => {
      if (++removals === 1) { cleanupReached.resolve(); await releaseCleanup.promise; }
      await originalRemove(clone);
    };
    const pages: Array<{ after: number; count: number }> = [];
    client.getChatRunMessages = async (runId, after = 0, limit = 200) => {
      const page = api.messages(runId).filter((m) => m.seq > after)
        .sort((a, b) => a.seq - b.seq).slice(0, limit);
      pages.push({ after, count: page.length });
      return page.map((m) => ({ ...m, agent: m.agent ?? null, created_at: "2026-09-08T00:00:00Z" }));
    };
    const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    const pending = [runner.execute(claim)];
    try {
      await ackReached.promise;
      // Promotion took this snapshot while the previous worker response was lost.
      const duplicate = { ...claim, last_seq: Math.max(0, ...api.messages(claim.run_id).map((m) => m.seq)) };
      pending.push(runner.execute(duplicate));
      assert.equal(factories, 1, "duplicate factory work must wait for the entire previous execution");
      for (let i = 0; i < 450; i++) firstEmit!({ kind: "status", payload: { text: `late-${i}` } });
      releaseAck.resolve();
      await cleanupReached.promise;
      assert.equal(factories, 1, "even a completed batcher does not release clone/session cleanup ownership");
      assert.equal(pages.length, 0, "cursor refresh happens only after all previous cleanup");
      const firstFinalSeq = Math.max(...api.messages(claim.run_id).map((m) => m.seq));
      assert.ok(firstFinalSeq > duplicate.last_seq + 400, "the stale claim predates multiple pages of late messages");
      releaseCleanup.resolve();
      await secondStarted.promise;
      pending.push(runner.execute(duplicate));
      assert.equal(factories, 2, "the first completion cannot delete a later queued execution's chain entry");
      releaseSecond.resolve();
      await Promise.all(pending);
      assert.equal(factories, 3, "duplicates are queued, never dropped");
      const messages = api.messages(claim.run_id);
      for (const attempt of [1, 2, 3]) {
        const emitted = messages.find((m) => m.payload.text === `execution-${attempt}`);
        assert.ok(emitted, `execution ${attempt}'s message was not lost to a stale sequence collision`);
        if (attempt > 1) assert.ok(emitted.seq > firstFinalSeq);
      }
      assert.ok(pages.filter((page) => page.count === 200).length >= 2,
        "cursor refresh paginates all messages left by the earlier execution");
      assert.deepEqual(messages.map((m) => m.seq).sort((a, b) => a - b),
        Array.from({ length: messages.length }, (_, i) => i + 1));
    } finally {
      releaseAck.resolve(); releaseCleanup.resolve(); releaseSecond.resolve();
      runner.shutdown();
      await Promise.allSettled(pending);
    }
  });

  it("a cancellation report that cannot land retains work and session on shutdown", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1309);
    const fixture = factoryFor(claim);
    const captureFailed = deferred();
    const cancelRetried = deferred();
    let reports = 0;
    git.commitWipMarker = async () => { captureFailed.resolve(); return false; };
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body) => {
      if (body.status === "failed" && body.failure_reason === "run cancelled") {
        if (++reports >= 2) cancelRetried.resolve();
        throw new Error("injected cancellation transport failure");
      }
      return report(runId, body);
    };
    const runner = runnerWith(fixture.factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    const execution = runner.execute(claim);
    try {
      await captureFailed.promise;
      api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
      await Promise.race([
        cancelRetried.promise,
        execution.then(() => { throw new Error("abandoned an unreported cancellation"); }),
      ]);
      assert.equal((await client.getRunOwnership(claim.run_id)).status, "running");
      runner.shutdown();
      await execution;
      assert.equal(fs.readFileSync(path.join(fixture.clone(), "ONLY_COPY.txt"), "utf8"), "must survive recovery\n");
      assert.equal(fs.existsSync(path.join(fixture.runHome, "session")), true);
      assert.equal(api.states.some((s) => s.body.status === "recovery_wait"), false);
      await assert.rejects(git.createOrAttachRunnerClone(git.barePathFor(fx.originPath), 1309, claim.run_id), PendingRecoveryCaptureError);
    } finally {
      runner.shutdown();
      await execution;
    }
  });

  for (const outcome of ["valid ACK", "cancel", "shutdown"] as const) {
    it(`a statusless recovery ACK retains the active session until ${outcome}`, async () => {
      const { gitlab } = fakeGitlab();
      const iid = outcome === "valid ACK" ? 1310 : outcome === "cancel" ? 1311 : 1312;
      const claim = gitlabClaim(iid);
      const fixture = factoryFor(claim);
      const retryReached = deferred();
      const releaseRetry = deferred();
      const report = client.reportState.bind(client);
      let reports = 0;
      let cancelReports = 0;
      let statuslessResponses = 0;
      let allowValidAck = false;
      client.reportState = async (runId, body) => {
        if (body.status === "recovery_wait") {
          if (++reports === 2) { retryReached.resolve(); await releaseRetry.promise; }
          if (allowValidAck) {
            api.setOwnershipStatus(runId, "recovery_wait");
            api.sendRawState(runId, 200, JSON.stringify({ run: { status: "recovery_wait" } }));
          } else {
            // Exercise the REAL WorkerClient HTTP204 parser. This response does
            // not move the fake's authoritative ownership state from running.
            api.sendRawState(runId, 204, "");
          }
          const ack = await report(runId, body);
          if (!allowValidAck) {
            assert.deepEqual(ack, { applied: true, status: undefined });
            statuslessResponses++;
          }
          return ack;
        }
        if (body.status === "failed" && body.failure_reason === "run cancelled") {
          cancelReports++;
          // The queued cancel has stamped stop_kind; Service's failed report
          // routes to CancelRunByWorker and returns this authoritative state.
          api.setOwnershipStatus(runId, "cancelled");
          api.sendRawState(runId, 200, JSON.stringify({ run: { status: "cancelled" } }));
        }
        return report(runId, body);
      };
      const runner = runnerWith(fixture.factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
      const execution = runner.execute(claim);
      try {
        await Promise.race([
          retryReached.promise,
          execution.then(() => { throw new Error("statusless ACK abandoned the live recovery execution"); }),
        ]);
        assert.equal(statuslessResponses, 1, "one real HTTP204 response triggered a retry");
        await client.heartbeat();
        assert.equal(api.heartbeats, 1, "the worker remains healthy, so stale-worker recovery cannot rescue an abandoned row");
        assert.equal((await client.getRunOwnership(claim.run_id)).status, "running");
        assert.equal(fs.existsSync(path.join(fixture.runHome, "session")), true);
        assert.equal(fs.existsSync(path.join(skillsPluginDir(fixture.clone()), "marker")), true);
        if (outcome === "valid ACK") allowValidAck = true;
        if (outcome === "cancel") api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
        if (outcome === "shutdown") runner.shutdown();
        releaseRetry.resolve();
        await execution;
        assert.equal(fixture.calls(), 1, "status-report retries never restart the model");
        if (outcome === "valid ACK") {
          assert.equal(reports, 2);
          assert.equal((await client.getRunOwnership(claim.run_id)).status, "recovery_wait");
          assert.equal(fs.existsSync(path.join(fixture.runHome, "session")), true);
        } else if (outcome === "cancel") {
          assert.equal(cancelReports, 1);
          assert.equal((await client.getRunOwnership(claim.run_id)).status, "cancelled");
          assert.equal(fs.existsSync(fixture.runHome), false);
        } else {
          assert.equal(cancelReports, 0);
          assert.equal((await client.getRunOwnership(claim.run_id)).status, "running");
          assert.equal(fs.existsSync(path.join(fixture.runHome, "session")), true);
          assert.equal(fs.existsSync(path.join(skillsPluginDir(fixture.clone()), "marker")), true);
          assert.equal(gitRead(git.barePathFor(fx.originPath), "show", `refs/uzi-runner/agent/issue-${iid}:ONLY_COPY.txt`), "must survive recovery");
        }
      } finally {
        releaseRetry.resolve();
        runner.shutdown();
        await execution;
      }
    });
  }
});
