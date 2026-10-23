import assert from "node:assert/strict";
import { it } from "node:test";
import type { Executor } from "../src/executor.js";
import type { GracefulShutdownCheckpoint } from "../src/harness.js";
import { api, client, deferred, fakeGitHub, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, installRecoveryRootCleanup, virtualBoundaryClock } from "./runner-codex-sinks-fixture.js";
import { shutdownContentProof } from "./shutdown-3101-content-fixture.js";
import { waitForTestEvent } from "./helpers.js";

installHarness();
installRecoveryRootCleanup();

const turn = () => new Promise<void>(resolve => setImmediate(resolve));

for (const mode of ["shutdown", "park"] as const) {
  it(mode === "shutdown"
    ? "m3 sibling shutdown preempts held and queued done imports within one original grace"
    : "m3 sibling pause waits for the predecessor done soft stop before preserving latest bytes", async t => {
    const clock = virtualBoundaryClock();
    t.mock.method(Date, "now", clock.now);
    client.protocolFeatures = ["run_checkpoint_durability"];
    const ids = ["31233000-0000-4000-8000-000000000001", "31233000-0000-4000-8000-000000000002"];
    const iids = mode === "shutdown" ? [312360, 312361] : [312362, 312363];
    const ready = [deferred(), deferred()];
    const allowDone = [deferred(), deferred()];
    const held = deferred(), queued = deferred(), terminal = deferred(), cancelled = deferred();
    const doneSettled = [deferred(), deferred()];
    const rigs: Array<ReturnType<typeof codexRig> | undefined> = [];
    const proofs: Array<ReturnType<typeof shutdownContentProof>> = [];
    const shutdownProofs: Array<GracefulShutdownCheckpoint | undefined> = [];
    const failures: unknown[] = [];
    const verificationFailures: unknown[] = [];
    const executions: Promise<unknown>[] = [];
    const roots: Promise<unknown>[] = [];
    const events: string[] = [];
    const doneImports = [0, 0], executorCalls = [0, 0];
    const ackAt = [0, 0], doneReturned = [false, false];
    let holding = false, released = false, childSettled = false;
    let disposals = 0, reaps = 0, lateQueuedTransport = 0, successorMutationBeforeSettlement = false;
    let parked: boolean | undefined, shutdownAt = 0, parkAt = 0;
    let credentialAtStop: number[] = [];
    const credentialAtDone: number[] = [];
    const credentialAtTerminal: number[] = [];
    const typedShutdownExchanges = [0, 0];
    const captureCallbacks = [0, 0], capturesSettled = [0, 0];

    const fetch = git.fetchAgentBranch.bind(git);
    t.mock.method(git, "fetchAgentBranch", (...args: Parameters<typeof git.fetchAgentBranch>) => {
      const index = ids.indexOf(args[3]);
      const isDone = holding && args[4].checkpointImportMode === "done";
      if (isDone && index >= 0) doneImports[index]!++;
      const step = args[4].checkpointStep;
      return fetch(args[0], args[1], args[2], args[3], {
        ...args[4],
        checkpointStep: name => {
          step?.(name);
          if (holding && index === 1 && name === "checkpoint_import_lock_wait") {
            events.push("b lock wait"); queued.resolve();
          }
          if (holding && index === 1 && name === "checkpoint_import_transport") {
            if (!childSettled) successorMutationBeforeSettlement = true;
            if (mode === "shutdown" && isDone) lateQueuedTransport++;
            events.push("b transport");
          }
        },
      });
    });
    const internal = git as unknown as {
      execScoped(command: string, args: string[], options: unknown): Promise<unknown>;
    };
    const exec = internal.execScoped.bind(git);
    t.mock.method(internal, "execScoped", (command: string, args: string[], options: unknown) => {
      if (holding && !childSettled && args.includes("-C") && args.includes(git.barePathFor(fx.originPath)) &&
          (args.includes("update-ref") || args.includes("--replace-all"))) successorMutationBeforeSettlement = true;
      return exec(command, args, options);
    });

    const executors: Executor[] = ids.map((_, index) => ({
      get safety() { return rigs[index]?.safety; },
      withCredentialFreeCapture: async (ms, action, purpose, signal, attempt) => {
        const rig = rigs[index]!;
        const credentials = rig.refreshCalls() + rig.releaseCalls();
        const result = await rig.capture(ms, context => {
          captureCallbacks[index]!++;
          return action(context);
        }, purpose, signal, attempt);
        assert.equal(result.kind, "observed_empty");
        assert.equal(rig.refreshCalls() + rig.releaseCalls(), credentials, "capture adds no credential exchange");
        capturesSettled[index]!++;
        return result;
      },
      run: async ctx => {
        executorCalls[index]!++;
        const rig = codexRig({
          lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]),
          now: clock.now, armDeadline: clock.armDeadline,
          boundaryProcess: request => {
            if (index !== 0 || !holding || released || !request.argv.includes("fetch") ||
                !request.argv.some(arg => arg.startsWith("file://"))) return undefined;
            events.push("a held transport");
            held.resolve();
            const settlement = terminal.promise.then(() => {
              childSettled = true; events.push("a child settled"); return { code: 128 };
            });
            roots.push(settlement);
            return {
              stdin: null, stdout: null, stderr: null,
              waitChild: () => settlement,
              root: {
                kind: "boundary_action",
                dispose: async () => {
                  disposals++; events.push("a cancellation"); cancelled.resolve(); await settlement;
                },
                reap: async () => { reaps++; await settlement; events.push("a reaped"); return { ok: true }; },
              },
            };
          },
        });
        rigs[index] = rig;
        const dispose = rig.safety.dispose.bind(rig.safety);
        rig.safety.dispose = request => {
          credentialAtTerminal[index] = rig.refreshCalls() + rig.releaseCalls();
          return dispose(request);
        };
        const boundary = rig.safety.withBoundary.bind(rig.safety);
        rig.safety.withBoundary = async (request, action) => {
          if (request.boundary === "shutdown") {
            shutdownProofs[index] = request.gracefulShutdownCheckpoint;
            events.push(index + " shutdown sink");
          }
          const before = rig.refreshCalls() + rig.releaseCalls();
          try { return await boundary(request, action); }
          finally {
            // Count only the real typed authority, never a boundary label alone.
            if (request.gracefulShutdownCheckpoint) {
              typedShutdownExchanges[index]! += rig.refreshCalls() + rig.releaseCalls() - before;
            }
          }
        };
        try {
          clock.advance(1);
          await proofs[index]!.prepare(ctx);
          ready[index]!.resolve();
          await allowDone[index]!.promise;
          if (index === 1 && mode === "park") {
            parked = await ctx.parkForPause!({ completedCount: 1, total: 2 });
            return { branch: ctx.branch, ...(parked ? { pausedAt: { completedCount: 1, total: 2 } } : {}) };
          }
          try {
            await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" });
            doneReturned[index] = true;
            credentialAtDone[index] = rig.refreshCalls() + rig.releaseCalls();
          } finally { doneSettled[index]!.resolve(); }
          if (!ctx.signal!.aborted) await new Promise<void>(resolve => ctx.signal!.addEventListener("abort", () => resolve(), { once: true }));
          throw new Error("provider unwound after sibling done");
        } catch (error) {
          failures[index] = error;
          throw error;
        }
      },
    }));
    const worker = runnerWith(runId => {
      const index = ids.indexOf(runId);
      if (index < 0) throw new Error("unexpected sibling runId " + runId);
      return { executor: executors[index]! };
    }, fakeGitlab().gitlab, undefined, undefined, {
      github: fakeGitHub().github,
      checkpointIntervalMs: 1, checkpointTickIntervalMs: 0,
      checkpointTestHooks: { absoluteNow: clock.now, armSoftDeadline: clock.arm },
    });
    const start = (index: number) => {
      const proof = shutdownContentProof(fx, client, iids[index]!, ids[index]!,
        mode === "park" && index === 1 ? "park_marker" : "overlay");
      proofs.push(proof);
      proof.onShutdownReceipt(() => { ackAt[index] = clock.now(); events.push(index + " ack"); });
      const execution = worker.execute(gitlabClaim(iids[index]!, {
        run_id: ids[index], claim_generation: 1,
        repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath,
          forge_type: "github", default_branch: "main" },
      }));
      executions.push(execution);
      return execution;
    };
    const awaitEvent = (event: Promise<unknown>, label: string) => waitForTestEvent(
      Promise.race([event, ...executions.map(run => run.then(() => { throw new Error("execution ended before " + label); }))]),
      // Fixture prepare includes real clone/fsync/pack work before the simulated done clock starts.
      label, label.endsWith(" prepare") ? 20_000 : 5_000);
    let testFailure: unknown;
    const settlementFailures: unknown[] = [];
    try {
      start(0);
      await awaitEvent(ready[0]!.promise, "A prepare");
      // Each constructor commits OLD; each prepare advances it to NEW. Never overlap
      // the fixture origin writes or let B startup queue behind the tested transport.
      start(1);
      await awaitEvent(ready[1]!.promise, "B prepare");
      holding = true;
      const doneAt = clock.now();
      allowDone[0]!.resolve();
      await awaitEvent(held.promise, "A held done transport");
      if (mode === "park") clock.advance(40_000 - (clock.now() - doneAt));
      parkAt = clock.now();
      allowDone[1]!.resolve();
      await awaitEvent(queued.promise, "B actual import lock wait");
      await turn();
      const beforeStop = { settled: childSettled, tips: proofs.map(proof => proof.tips.length) };
      if (mode === "shutdown") {
        shutdownAt = clock.now();
        credentialAtStop = rigs.map(rig => rig!.refreshCalls() + rig!.releaseCalls());
        worker.shutdown();
        await turn();
        clock.advance(5_000);
      } else clock.advance(60_000 - (clock.now() - doneAt));
      await turn();
      const blocked = {
        cancelled: disposals > 0, childSettled, tips: proofs.map(proof => proof.tips.length),
        ackAt: [...ackAt], doneReturned: [...doneReturned], successorMutationBeforeSettlement,
      };
      // External test control releases even if cancellation regressed. Assertions below
      // therefore name the ordering defect without waiting for real boundary deadlines.
      released = true;
      terminal.resolve();
      if (mode === "park") {
        await executions[1];
        await waitForTestEvent(doneSettled[0]!.promise, "A soft stopped done", 5_000);
        shutdownAt = clock.now();
        credentialAtStop = rigs.map(rig => rig!.refreshCalls() + rig!.releaseCalls());
        worker.shutdown();
      }
      await Promise.all(executions);
      for (const proof of proofs) {
        try { await proof.verify(git); } catch (error) { verificationFailures.push(error); }
      }
      assert.deepEqual(beforeStop, { settled: false, tips: [1, 1] });
      assert.equal(blocked.cancelled, true, "A owned child cancellation requested before external release");
      assert.equal(blocked.childSettled, false, "child terminal is controlled only by external release");
      assert.deepEqual(blocked.tips, [1, 1], "neither receiver ACKs newer bytes before predecessor release");
      assert.deepEqual(blocked.ackAt, [0, 0]);
      assert.equal(blocked.doneReturned[0], false, "A done waits for its owned child settlement");
      assert.equal(blocked.successorMutationBeforeSettlement, false, "no successor bare mutation before predecessor settlement");
      assert.equal(successorMutationBeforeSettlement, false);
      assert.equal(lateQueuedTransport, 0, "cancelled B done waiter never starts transport");
      assert.ok(disposals >= 1 && reaps >= 1, "root safety owns verified child cleanup");
      assert.ok(events.indexOf("a child settled") < events.indexOf("b transport"), "successor transport follows child settlement");
      assert.ok(events.indexOf("a reaped") < events.indexOf("b transport"), "successor transport follows verified root closure");
      assert.deepEqual(executorCalls, [1, 1], "no extra model turns");
      assert.deepEqual(doneImports, mode === "shutdown" ? [1, 1] : [1, 0], "no extra done snapshot import retry");
      assert.equal(doneReturned[0], true);
      if (mode === "shutdown") assert.equal(doneReturned[1], true);
      else {
        assert.equal(parked, true, "B actual pause sink parks");
        assert.equal(ackAt[1]! - parkAt, 20_000, "B waits twenty simulated seconds in the existing credential-free pause path");
        assert.ok(api.states.some(state => state.runId === ids[1] && state.body.status === "paused"));
      }
      for (const index of [0, 1]) {
        const rig = rigs[index]!;
        const captured = mode === "park" && index === 1;
        assert.equal(captureCallbacks[index], captured ? 1 : 0, "only the pause participant uses capture");
        assert.equal(capturesSettled[index], captured ? 1 : 0, "capture owner settles before execution returns");
        assert.equal(rig.registry.isPoisoned(), captured, "only capture permanently closes epoch admission");
        if (captured) assert.deepEqual(rig.registry.reserveLaunch("provider"), { kind: "denied", reason: "admission_closed" });
        assert.equal(rig.registry.state(), "disposed", "each run disposes only its own registry");
        assert.equal(rig.refreshCalls() + rig.releaseCalls(), credentialAtStop[index]! + typedShutdownExchanges[index]!,
          "no ordinary credential authority after shutdown: run=" + index +
          " done=" + credentialAtDone[index] + " terminalEntry=" + credentialAtTerminal[index]);
        assert.ok(events.indexOf("a child settled") < events.indexOf(index + " ack"));
        if (mode === "shutdown" || index === 0) {
          assert.ok(shutdownProofs[index], "genuine typed shutdown sink observed");
          assert.equal(shutdownProofs[index]!.deadlineAt, shutdownAt + 15_000);
          assert.ok(ackAt[index]! >= shutdownAt + (mode === "shutdown" ? 5_000 : 0) &&
            ackAt[index]! < shutdownProofs[index]!.deadlineAt, "receiver verifies bytes before the original deadline");
        }
      }
    } catch (error) { testFailure = error; }
    finally {
      for (const latch of allowDone) latch.resolve();
      released = true; terminal.resolve(); worker.shutdown();
      const settled = await Promise.allSettled([...executions, ...roots]);
      for (const proof of [...proofs].reverse()) {
        try { proof.cleanup(); } catch (error) { settlementFailures.push(error); }
      }
      for (const result of settled) if (result.status === "rejected") settlementFailures.push(result.reason);
    }
    const errors = [...(testFailure === undefined ? [] : [testFailure]), ...verificationFailures, ...settlementFailures];
    if (errors.length > 0) throw new AggregateError(errors,
      mode + " sibling sink contract failed after all owned executions settled");
    // Runner catches executor errors; only the intentional post-done unwind is allowed.
    for (const [index, failure] of failures.entries()) {
      if (mode === "park" && index === 1) assert.equal(failure, undefined);
      else assert.equal((failure as Error | undefined)?.message, "provider unwound after sibling done");
    }
  });
}
