import { afterEach, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { ExecutorResult, IterationParkOutcome } from "../src/executor.js";
import type { SteeringChannel } from "../src/steering.js";
import { recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";
import { codexRig, commitInTree, drain, enabledRecovery, FakeCodexExecutor, GIT_ENV, installRecoveryRootCleanup, spyCustodySettle } from "./runner-codex-sinks-fixture.js";

installHarness();
installRecoveryRootCleanup();
let callbackErrors: unknown[] = [];
afterEach(() => {
  const errors = callbackErrors;
  callbackErrors = [];
  assert.deepEqual(errors, [], "Codex executor assertions must not be swallowed");
});

function barrier() {
  let resolve!: () => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<void>((done, fail) => { resolve = done; reject = fail; });
  const timer = setTimeout(() => reject(new Error("iteration admission event did not arrive")), 30_000);
  return { promise: promise.finally(() => clearTimeout(timer)), resolve };
}
const modes = ["capture", "root cancel", "root fence", "preadmission cancel", "preadmission fence", "pending admission", "joined cleanup rejection"] as const;
for (const mode of modes) {
  it(`Codex iteration capture: ${mode} preserves source and custody after joined sink`, async t => {
    const iid = 359800 + modes.indexOf(mode);
    const claim = gitlabClaim(iid, { kind: "issue", claim_generation: 7, config: { iteration_park_version: 1 } });
    const { coord, archive } = enabledRecovery();
    const { logger, lines } = recordingLogger();
    const { gitlab, calls } = fakeGitlab();
    const admission = barrier();
    const release = barrier();
    // These barriers are only needed in the pending admission scenario.
    if (mode !== "pending admission") { admission.resolve(); release.resolve(); }
    const lifecycle = new AbortController();
    let sink = false, publication = false, injected = 0, joined = false;
    let revoked = false, source = 0, turns = 0, freeCalls = 0;
    let steering: SteeringChannel | undefined;
    type Flight = { steering: SteeringChannel; preserveSession: boolean; preserveRecoveryClone: boolean; iterationParkState: string };
    let flight: Flight | undefined;
    const outcomes: IterationParkOutcome[] = [];
    const rig = codexRig({
      lifecycleSignal: lifecycle.signal,
      boundaryProcess: async request => {
        if (mode !== "joined cleanup rejection" || !publication || injected) return undefined;
        injected++;
        const [command, ...args] = request.argv;
        const child = spawn(command!, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
        const terminal = new Promise<{ code: number }>((resolve, reject) => {
          child.once("error", reject);
          child.once("close", code => resolve({ code: code ?? 1 }));
        });
        return {
          stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, waitChild: () => terminal,
          root: {
            kind: "boundary_action",
            reap: async () => {
              await terminal;
              return { ok: false, error: { category: "tool", message: "iteration owned cleanup rejected" } };
            },
            dispose: async () => {
              if (child.exitCode === null) child.kill("SIGKILL");
              await terminal.catch(() => undefined);
            },
          },
        };
      },
    });
    const reservation = rig.registry.reserveLaunch("provider");
    assert.equal(reservation.kind, "reserved");
    if (reservation.kind === "reserved") rig.registry.registerRoot(reservation.reservation, {
      kind: "provider",
      reap: async () => {
        if (sink && !revoked && (mode === "root cancel" || mode === "root fence")) {
          revoked = true;
          if (mode === "root cancel") steering!.markCancelled();
          else t.mock.method(steering!, "claimFence", () => "stale");
        }
        return { ok: true };
      },
      dispose: async () => {},
    });
    const boundary = rig.safety.withBoundary.bind(rig.safety);
    t.mock.method(rig.safety, "withBoundary", async (...args: Parameters<typeof boundary>) => {
      if (sink && mode === "pending admission") {
        admission.resolve();
        await release.promise;
      }
      try { return await boundary(...args); } finally { if (sink) joined = true; }
    });
    for (const name of ["worktreeStatus", "commitWipMarker", "fetchAgentBranch"] as const) {
      const original = git[name].bind(git);
      t.mock.method(git, name, async (...args: unknown[]) => {
        if (sink) source++;
        return Reflect.apply(original, git, args);
      });
    }
    const exec = new FakeCodexExecutor(rig.safety, async ctx => {
      try {
        turns++;
        ctx.terminalLifecycleSignal!.addEventListener("abort", () => lifecycle.abort(ctx.terminalLifecycleSignal!.reason), { once: true });
        fs.mkdirSync(homeDir, { recursive: true });
        fs.writeFileSync(path.join(homeDir, "SESSION"), "Codex retained session\n");
        commitInTree(ctx.worktreePath, "COMMITTED.txt", "Codex committed iteration work\n");
        fs.writeFileSync(path.join(ctx.worktreePath, "DIRTY.txt"), "Codex dirty iteration work\n");
        sink = true;
        const first = ctx.parkForIteration!({ iterationCount: 10, maxIterations: 10 });
        const second = ctx.parkForIteration!({ iterationCount: 10, maxIterations: 10 });
        assert.equal(first, second);
        const outcome = await first;
        assert.equal(await second, outcome);
        outcomes.push(outcome);
        return { branch: ctx.branch, iterationParked: { outcome } } satisfies ExecutorResult;
      } catch (error) { callbackErrors.push(error); throw error; }
    }, undefined, async (ms, action, purpose, signal, attempt) => {
      freeCalls++;
      return rig.capture(ms, action, purpose, signal, attempt);
    });
    const runner = runnerWith(() => ({ executor: exec, homeDir }), gitlab, undefined, logger, {
      recovery: coord, checkpointIntervalMs: 0, recoveryRetryMs: 1,
    });
    const custody = spyCustodySettle(runner);
    const internal = runner as unknown as {
      runGatedSink: (f: Flight, action: () => Promise<unknown>) => Promise<unknown>;
      reportGenericFailure: (...args: unknown[]) => Promise<void>;
      buildCheckpointOverlay: (...args: unknown[]) => Promise<unknown>;
    };
    const gated = internal.runGatedSink.bind(runner);
    t.mock.method(internal, "runGatedSink", (f: Flight, action: () => Promise<unknown>) => {
      flight = f;
      steering = f.steering;
      if (sink && mode.startsWith("preadmission")) {
        assert.equal(f.iterationParkState, "entered");
        assert.equal(f.preserveSession, true);
        assert.equal(f.preserveRecoveryClone, true);
        revoked = true;
        if (mode === "preadmission cancel") steering.markCancelled();
        else t.mock.method(steering, "claimFence", () => "stale");
      }
      return gated(f, action);
    });
    let genericFailures = 0, pushes = 0;
    t.mock.method(internal, "reportGenericFailure", async () => { genericFailures++; });
    t.mock.method(git, "pushBranch", async () => { pushes++; });
    t.mock.method(client, "publishCheckpoint", async (_id: string, _head: string, pack: Parameters<typeof client.publishCheckpoint>[2]) => {
      await drain(pack);
      return { ok: true as const, body: { published: true, ref: "refs/uzi-checkpoints/codex-iteration" } };
    });
    if (mode === "joined cleanup rejection") t.mock.method(internal, "buildCheckpointOverlay", async () => {
      publication = true;
      await git.trackingTip(git.barePathFor(fx.originPath), `agent/issue-${iid}`);
      throw new Error("owned cleanup must reject before publication");
    });

    const execution = runner.execute(claim);
    try {
      if (mode === "pending admission") {
        await Promise.race([admission.promise, execution.then(() => assert.fail("execution ended before admission barrier"))]);
        assert.ok(flight);
        assert.equal(flight.iterationParkState, "entered");
        assert.equal(flight.preserveSession, true);
        assert.equal(flight.preserveRecoveryClone, true);
        assert.equal(source, 0, "pending admission has not inspected source");
        assert.equal(api.iterationParkRequests.length, 0);
        assert.equal(custody(), 0);
        assert.ok(fs.existsSync(worktreeDirFor(iid)));
        assert.ok(fs.existsSync(path.join(homeDir, "SESSION")));
      }
    } finally {
      release.resolve();
      admission.resolve();
      await execution;
      await Promise.all([admission.promise, release.promise]);
    }
    assert.ok(outcomes.length > 0, JSON.stringify(lines));
    assert.deepEqual(outcomes, [mode === "capture" || mode === "pending admission" ? "parked" : "retained-stop"]);
    assert.equal(turns, 1);
    assert.equal(genericFailures, 0);
    assert.equal(pushes, 0);
    assert.equal(calls.length, 0);
    assert.equal(custody(), 0, "enabled custody remains held");
    assert.equal(archive.releaseCalls.length, 0);
    assert.equal(api.completionPermitRequests.length, 0);
    assert.equal(api.wallParkRequests.length, 0);
    assert.ok(!api.states.some(s => ["failed", "completed", "pause_failed"].includes(s.body.status!)));
    assert.ok(api.iterationParkRequests.length <= 2);
    assert.equal(fs.readFileSync(path.join(worktreeDirFor(iid), "DIRTY.txt"), "utf8"), "Codex dirty iteration work\n");
    assert.equal(fs.readFileSync(path.join(homeDir, "SESSION"), "utf8"), "Codex retained session\n");
    const bare = git.barePathFor(fx.originPath);
    const journal = JSON.parse(execFileSync("git", ["-C", bare, "config", `uzi-recovery.agent/issue-${iid}.clone`], { env: GIT_ENV, encoding: "utf8" }));
    assert.equal(journal.clonePath, worktreeDirFor(iid));
    assert.equal(journal.runId, claim.run_id, "retained journal belongs to this run");
    if (mode.includes("cancel") || mode.includes("fence")) {
      assert.equal(revoked, true, "authority control actually fired");
      assert.equal(source, 0, "revoked authority forbids source reads");
      assert.equal(freeCalls, 0, "revocation never permits fresh fallback");
      assert.equal(api.iterationParkRequests.length, 0);
    } else if (mode === "joined cleanup rejection") {
      assert.equal(injected, 1, "owned publication root cleanup actually failed");
      assert.equal(joined, true, "primary capture cleanup joined before stop");
    } else {
      assert.equal(joined, true);
      assert.equal(api.iterationParkRequests.length, 1);
      const body = api.iterationParkRequests[0]!.body;
      assert.equal(body.claim_generation, 7);
      assert.equal(body.iteration_count, 10);
      assert.equal(body.max_iterations, 10);
      assert.match(String(body.head), /^[a-f0-9]{40}$/);
      assert.equal(execFileSync("git", ["-C", bare, "show", body.head + ":DIRTY.txt"], { env: GIT_ENV, encoding: "utf8" }), "Codex dirty iteration work\n");
    }
  });
}
