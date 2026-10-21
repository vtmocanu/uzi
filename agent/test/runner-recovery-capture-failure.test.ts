import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { ForeignCaptureBlockedError, GitCache, PendingRecoveryCaptureError, CapturePathMismatchError, type RecoveryJournalEntry } from "../src/git.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import type { Executor } from "../src/executor.js";
import type { BoundaryRequest, BoundaryPermit } from "../src/harness.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { nullLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";

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
  for (const boundary of ["success", "throw", "not-entered", "throw-after-action"] as const) {
    it(`authenticated recovery drains before source writes (${boundary})`, { timeout: 10_000 }, async (t) => {
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(1399, { claim_generation: 1 });
      const fixture = factoryFor(claim);
      const events: string[] = [];
      let callbackCalls = 0;
      let legacyCalls = 0;
      let captures = 0;
      let fetches = 0;
      let publications = 0;
      let retries = 0;
      let runner!: RunRunner;
      const assertDrained = () => {
        const summary = events.indexOf("summary");
        const commands = events.indexOf("commands");
        const drain = events.indexOf("drained");
        assert.ok(summary >= 0 && commands > summary && drain > commands,
          "summary and confirmed commands precede successful normal boundary return");
      };
      const marker = git.commitWipMarker.bind(git);
      t.mock.method(git, "commitWipMarker", async (...args: Parameters<typeof marker>) => {
        captures++;
        assertDrained();
        events.push("marker");
        return marker(...args);
      });
      const fetch = git.fetchAgentBranch.bind(git);
      t.mock.method(git, "fetchAgentBranch", async (...args: Parameters<typeof fetch>) => {
        fetches++;
        assertDrained();
        events.push("fetch");
        return fetch(...args);
      });
      const publish = client.publishCheckpoint.bind(client);
      t.mock.method(client, "publishCheckpoint", async (...args: Parameters<typeof publish>) => {
        publications++;
        assert.equal(callbackCalls, 0);
        assert.equal(legacyCalls, 0);
        assert.ok(events.indexOf("marker") > events.indexOf("drained"));
        assert.ok(events.indexOf("fetch") > events.indexOf("drained"));
        assert.ok(events.filter(event => event === "boundary").length >= 2,
          "publication acquires its own authenticated boundary after the drain");
        return publish(...args);
      });
      // This facade tests admission and ordering only; fixture Git runs on the host.
      // Production Codex root verification remains the normal boundary's responsibility.
      t.mock.method(git, "withBoundaryProcessSpawner", async (...args: Parameters<typeof git.withBoundaryProcessSpawner>) => args[2]());
      const factory: ExecutorFactory = (...args) => {
        const made = fixture.factory(...args);
        const run = made.executor.run.bind(made.executor);
        made.executor.run = async ctx => {
          try { return await run(ctx); }
          finally {
            made.executor.safety = {
              kind: "codex",
              withBoundary: async <T>(request: BoundaryRequest, action: (permit: BoundaryPermit) => Promise<T>): Promise<T> => {
                assert.equal(callbackCalls, 0);
                assert.equal(legacyCalls, 0);
                events.push("boundary");
                if (captures === 0) {
                  assert.equal(request.boundary, "shutdown");
                  assert.ok(events.indexOf("commands") >= 0, "commands confirmed before drain");
                  if (boundary === "throw") throw new Error("injected boundary failure");
                  // Deliberately broken admission facade: a resolved return alone is no proof.
                  if (boundary === "not-entered") return undefined as T;
                  const result = await action({ signal: new AbortController().signal } as never);
                  if (boundary === "throw-after-action") throw new Error("injected boundary return failure");
                  events.push("drained");
                  return result;
                }
                return action({ signal: new AbortController().signal } as never);
              },
              spawnBoundaryProcess: async () => { throw new Error("fixture uses host Git"); },
              dispose: async () => ({ kind: "disposed" }),
            };
            made.executor.closeAndSettlePlanSummary = async () => { events.push("summary"); };
            made.executor.settleWorkerCommands = async () => { events.push("commands"); return { confirmed: true }; };
            made.executor.settleForCredentialFreeCapture = async () => {
              legacyCalls++;
              throw new Error("authenticated recovery must not use legacy free settle");
            };
            made.executor.withCredentialFreeCapture = (async () => {
              callbackCalls++;
              runner.shutdown();
              throw new Error("authenticated recovery must not acquire free capture");
            }) as Executor["withCredentialFreeCapture"];
          }
        };
        return made;
      };
      runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 });
      const owner = runner as unknown as { waitRecoveryRetry(): Promise<void> };
      t.mock.method(owner, "waitRecoveryRetry", async () => {
        if (++retries === 6) runner.shutdown();
      });
      try {
        await runner.execute(claim);
      } finally {
        runner.shutdown();
      }
      assert.equal(callbackCalls, 0);
      assert.equal(legacyCalls, 0);
      assert.equal(fixture.calls(), 1, "settlement retries never restart the model");
      if (boundary === "success") {
        assert.ok(captures > 0);
        assert.ok(fetches > 0);
        assert.ok(publications > 0, "authenticated publication succeeds");
        assert.equal(api.states.filter(s => s.body.status === "recovery_wait").length, 1);
      } else {
        assert.equal(retries, 6, "incomplete boundary retries without spending process-proof attempts");
        assert.equal(captures, 0);
        assert.equal(fetches, 0);
        assert.equal(publications, 0);
        assert.equal(api.states.some(s => s.body.status === "recovery_wait" || s.body.status === "failed"), false);
        assert.equal(fs.readFileSync(path.join(fixture.clone(), "ONLY_COPY.txt"), "utf8"), "must survive recovery\n");
        assert.ok(fs.existsSync(path.join(fixture.runHome, "session")), "shutdown retains the session");
      }
    });
  }

  for (const poison of ["false", "throw"] as const) {
    it(`Claude noncredential recovery blocks capture and publication on ${poison} settlement`, { timeout: 10_000 }, async () => {
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(1398, { claim_generation: 1 });
      assert.equal(claim.secrets.codex, undefined);
      const fixture = factoryFor(claim);
      const retried = deferred();
      let settlements = 0;
      let captures = 0;
      let publications = 0;
      const marker = git.commitWipMarker.bind(git);
      git.commitWipMarker = async (...args) => { captures++; return marker(...args); };
      const publish = client.publishCheckpoint.bind(client);
      client.publishCheckpoint = async (...args) => { publications++; return publish(...args); };
      const factory: ExecutorFactory = (...args) => {
        const made = fixture.factory(...args);
        made.executor.settleWorkerCommands = async () => {
          if (fixture.calls() === 0) return { confirmed: true };
          if (++settlements >= 6) retried.resolve();
          if (poison === "throw") throw new Error("poisoned Claude command settlement");
          return { confirmed: false };
        };
        return made;
      };
      const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 });
      const execution = runner.execute(claim);
      try {
        await Promise.race([
          retried.promise,
          execution.then(() => { throw new Error("recovery exited before retrying strict settlement"); }),
        ]);
        assert.equal(captures, 0, "unconfirmed commands prevent WIP capture");
        assert.equal(publications, 0, "unconfirmed commands prevent checkpoint publication");
        assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
        assert.equal(api.states.some(s => s.body.status === "failed"), false,
          "incomplete settlement is retried rather than charged as a failed process proof");
        assert.equal(fixture.calls(), 1, "settlement retries never restart the model");
        assert.equal(fs.readFileSync(path.join(fixture.clone(), "ONLY_COPY.txt"), "utf8"), "must survive recovery\n");
      } finally {
        runner.shutdown();
        await execution;
      }
      assert.equal(captures, 0);
      assert.equal(publications, 0);
      assert.ok(fs.existsSync(fixture.clone()), "shutdown retains the unsettled source");
      assert.ok(fs.existsSync(path.join(fixture.runHome, "session")));
    });
  }

  for (const failure of ["wip", "fetch", "verify"] as const) {
    it(`${failure} failure retains active work until capture verifies`, async () => {
      const { gitlab } = fakeGitlab();
      const iid = failure === "wip" ? 1301 : failure === "fetch" ? 1302 : 1303;
      const claim = gitlabClaim(iid, { claim_generation: 1 });
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
      if (failure === "fetch") git.fetchAgentBranch = (async (...args: Parameters<typeof fetch>) => {
        if (allowCapture) return fetch(...args);
        fail(); throw new Error("injected fetch failure");
      }) as typeof git.fetchAgentBranch;
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
        assert.ok(fs.existsSync(path.join(fixture.clone(), ".git")), "the verified park keeps the exact clone");
        const journal = JSON.parse(gitRead(bare, "config", "--local", "--get", `uzi-recovery.agent/issue-${iid}.clone`)) as RecoveryJournalEntry;
        assert.equal(journal.clonePath, fixture.clone(), "the covered park keeps its clone journal");
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
    const scratchArtifact = path.join(fixture.clone(), ".uzi", "scratch", "capture.log");
    fs.writeFileSync(scratchArtifact, "retained during capture\n");
    first.shutdown();
    await execution;
    assert.equal(fs.readFileSync(scratchArtifact, "utf8"), "retained during capture\n");
    assert.equal(api.states.some((s) => s.body.status === "recovery_wait"), false);
    assert.equal(fs.readFileSync(path.join(fixture.clone(), "ONLY_COPY.txt"), "utf8"), "must survive recovery\n");
    assert.equal(fs.existsSync(path.join(fixture.runHome, "session")), true);

    const restartedGit = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
    const bare = restartedGit.barePathFor(fx.originPath);
    await assert.rejects(
      restartedGit.createOrAttachRunnerClone(bare, iid, noProofReseed, claim.run_id),
      PendingRecoveryCaptureError,
      "the durable worker journal blocks same-run destructive reseeding",
    );
    assert.equal(fs.readFileSync(scratchArtifact, "utf8"), "retained during capture\n", "the same retained clone keeps scratch until capture settles");
    // issue #1315: the same-path foreign case is now the RECLAIMABLE
    // ForeignCaptureBlockedError, carrying the exact owner/clone/branch the runner
    // needs for its authoritative owner probe. (Case B — matched canonical pair.)
    const foreign = "99999999-9999-4999-8999-999999999999";
    await assert.rejects(
      restartedGit.createOrAttachRunnerClone(bare, iid, noProofReseed, foreign),
      (err: unknown) => {
        assert.ok(err instanceof ForeignCaptureBlockedError, "same-path foreign owner -> ForeignCaptureBlockedError");
        assert.equal(err.ownerRunId, claim.run_id);
        assert.equal(err.clonePath, worktreeDirFor(iid));
        assert.equal(err.branch, `agent/issue-${iid}`);
        return true;
      },
      "a foreign run cannot adopt or erase retained work",
    );
    // A DIFFERENT clone key computes a different canonical path: the git layer classifies a
    // claimant-relative path mismatch (CapturePathMismatchError, Case A) and never probes
    // owner status here — reclaimability is now decided by the runner's owner validation
    // (issue #1319), not the git layer.
    await assert.rejects(
      restartedGit.runnerCloneForBranch(bare, `agent/issue-${iid}`, "different-kind-clone", noProofReseed, foreign),
      (err: unknown) => {
        assert.ok(err instanceof CapturePathMismatchError, "different clone key -> CapturePathMismatchError");
        assert.equal(err.journaledPath, worktreeDirFor(iid));
        assert.equal(err.branch, `agent/issue-${iid}`);
        return true;
      },
      "a different clone key cannot bypass the retained branch ownership journal",
    );
    let modelPath = "";
    let modelSession: string | null | undefined;
    let admittedJournal: RecoveryJournalEntry | undefined;
    const restarted = new RunRunner(client, restartedGit, () => ({
      homeDir: fixture.runHome,
      executor: { run: async (ctx) => {
        modelPath = ctx.worktreePath;
        modelSession = ctx.sessionId;
        admittedJournal = JSON.parse(gitRead(bare, "config", "--local", "--get", `uzi-recovery.agent/issue-${iid}.clone`)) as RecoveryJournalEntry;
        throw new Error("fixture stops after model admission");
      } },
    }), nullLogger(), 20, undefined, { pollMs: 5, gitlab, recoveryRetryMs: 5 });
    const successor = { ...claim, claim_generation: 2, session_id: "predecessor-session" };
    api.setOwnershipStatus(claim.run_id, "running", 2);
    await restarted.execute(successor);
    assert.ok(modelPath, "verified local capture starts the successor model in the fenced claim");
    assert.notEqual(modelPath, fixture.clone());
    assert.match(modelPath, /\.attempt-.*-g2-/);
    assert.equal(modelSession, undefined, "the successor starts a fresh model session");
    assert.equal(api.states.filter((s) => s.body.status === "recovery_wait").length, 0);
    assert.equal(fs.readFileSync(path.join(modelPath, "ONLY_COPY.txt"), "utf8"), "must survive recovery\n");
    assert.equal(fs.existsSync(path.join(modelPath, ".uzi", "scratch", "capture.log")), false, "old scratch is not adopted");
    assert.equal(fs.readFileSync(scratchArtifact, "utf8"), "retained during capture\n");
    assert.equal(fs.readFileSync(path.join(fixture.clone(), "ONLY_COPY.txt"), "utf8"), "must survive recovery\n");
    assert.equal(fs.readFileSync(path.join(fixture.runHome, "session"), "utf8"), "transcript");
    assert.equal(admittedJournal?.clonePath, modelPath);
    assert.equal(admittedJournal?.runId, claim.run_id);
    assert.equal(admittedJournal?.recovery?.stage, "ready-for-model");
    assert.equal(admittedJournal?.recovery?.attempts, 1, "model admission preserves the charged iteration");
    assert.ok(admittedJournal?.retainedSources?.some(source => source.clonePath === fixture.clone() && source.runId === claim.run_id));
    const retained = JSON.parse(gitRead(bare, "config", "--local", "--get", `uzi-recovery.agent/issue-${iid}.clone`)) as RecoveryJournalEntry;
    assert.equal(retained.recovery?.attempts, 1, "an interrupted model does not reset the episode");
    assert.equal(retained.recovery?.stage, "ready-for-model");
    assert.ok(gitRead(bare, "for-each-ref", "--format=%(refname)", `refs/uzi-recovery-episode/${claim.run_id}/`), "episode pins retain custody");
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
    const claim = gitlabClaim(1308, { claim_generation: 1 });
    const ackReached = deferred();
    const releaseAck = deferred();
    const cleanupReached = deferred();
    const releaseCleanup = deferred();
    const secondStarted = deferred();
    const releaseSecond = deferred();
    let firstEmit: import("../src/executor.js").RunContext["emit"] | undefined;
    let factories = 0;
    let modelRuns = 0;
    const factory: ExecutorFactory = () => {
      factories++;
      return {
        homeDir: path.join(homeDir, claim.run_id),
        executor: {
          run: async (ctx) => {
            const attempt = ++modelRuns;
            assert.equal(ctx.sessionId, undefined, "each retained-source claim starts a fresh session");
            if (attempt === 1) firstEmit = ctx.emit;
            if (attempt === 2) { secondStarted.resolve(); await releaseSecond.promise; }
            ctx.emit({ kind: "status", payload: { text: `execution-${attempt}` } });
            throw new TransientRecoveryError();
          },
        },
      };
    };
    const originalReport = client.reportState.bind(client);
    const originalPost = client.postMessages.bind(client);
    const originalMessages = client.getChatRunMessages.bind(client);
    const latePersisted = deferred();
    client.postMessages = async (...args) => {
      await originalPost(...args);
      if (api.messages(claim.run_id).filter(m => /^late-\d+$/.test(String(m.payload.text))).length === 450) {
        latePersisted.resolve();
      }
    };
    let parks = 0;
    client.reportState = async (runId, body) => {
      const park = body.status === "recovery_wait" ? ++parks : 0;
      if (park === 1) {
        ackReached.resolve();
        await releaseAck.promise;
      }
      const ack = await originalReport(runId, body);
      if (park === 2) api.setOwnershipStatus(claim.run_id, "running", 3);
      return ack;
    };
    // Parks retain the clone. Hold the final stopped-source proof to exercise serialization
    // through cleanup without relying on physical clone retirement.
    let cleanups = 0;
    const quiesceRun: NonNullable<import("../src/runner.js").RunnerOptions["quiesceRun"]> = async req => {
      if (req.site === "stopped_source" && ++cleanups === 1) {
        cleanupReached.resolve();
        await releaseCleanup.promise;
      }
      return {
        process: { state: "quiescent", processes: [], killed: [], detail: "fixture roots reaped" },
        docker: { state: "not_wired", removed: [], detail: "" },
      };
    };
    const waitForEvent = async (event: Promise<void>, label: string) => {
      let timer: ReturnType<typeof setTimeout> | undefined;
      try {
        await Promise.race([event, new Promise<never>((_, reject) => {
          timer = setTimeout(() => reject(new Error(`fixture never reached ${label}`)), 10_000);
        })]);
      } finally { clearTimeout(timer); }
    };
    const pages: Array<{ after: number; count: number }> = [];
    client.getChatRunMessages = async (runId, after = 0, limit = 200) => {
      const page = api.messages(runId).filter((m) => m.seq > after)
        .sort((a, b) => a.seq - b.seq).slice(0, limit);
      pages.push({ after, count: page.length });
      return page.map((m) => ({ ...m, agent: m.agent ?? null, created_at: "2026-09-08T00:00:00Z" }));
    };
    // Queue fenced successor claims while each predecessor still owns its ACK/cleanup tail.
    api.setOwnershipStatus(claim.run_id, "running", 1);
    const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recoveryRetryMs: 5, quiesceRun });
    const pending = [runner.execute(claim)];
    try {
      await waitForEvent(ackReached.promise, "first park ACK");
      // Promotion took this snapshot while the previous worker response was lost.
      const duplicate = { ...claim, claim_generation: 2, last_seq: Math.max(0, ...api.messages(claim.run_id).map((m) => m.seq)) };
      pending.push(runner.execute(duplicate));
      assert.equal(factories, 1, "duplicate factory work must wait for the entire previous execution");
      for (let i = 0; i < 450; i++) firstEmit!({ kind: "status", payload: { text: `late-${i}` } });
      await waitForEvent(latePersisted.promise, "450 late messages persisted before park application");
      assert.equal((await client.getRunOwnership(claim.run_id)).status, "running");
      releaseAck.resolve();
      await waitForEvent(cleanupReached.promise, "stopped-source cleanup");
      assert.equal(factories, 1, "even a completed batcher does not release clone/session cleanup ownership");
      assert.equal(pages.length, 0, "cursor refresh happens only after all previous cleanup");
      const firstFinalSeq = Math.max(...api.messages(claim.run_id).map((m) => m.seq));
      assert.ok(firstFinalSeq > duplicate.last_seq + 400, "the stale claim predates multiple pages of late messages");
      api.setOwnershipStatus(claim.run_id, "running", 2);
      releaseCleanup.resolve();
      await waitForEvent(secondStarted.promise, "second model admission");
      pending.push(runner.execute({ ...duplicate, claim_generation: 3 }));
      assert.equal(modelRuns, 2, "the successor starts only after the predecessor cleanup");
      assert.equal(factories, 2, "the queued successor factory starts only after predecessor cleanup");
      releaseSecond.resolve();
      await Promise.all(pending);
      assert.equal(modelRuns, 3, "each fenced queued claim runs once after the preceding tail settles");
      assert.equal(factories, 3, "each queued claim constructs its own fixture executor");
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
      client.reportState = originalReport;
      client.postMessages = originalPost;
      client.getChatRunMessages = originalMessages;
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
      await assert.rejects(git.createOrAttachRunnerClone(git.barePathFor(fx.originPath), 1309, noProofReseed, claim.run_id), PendingRecoveryCaptureError);
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
