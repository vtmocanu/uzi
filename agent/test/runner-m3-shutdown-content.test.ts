import assert from "node:assert/strict";
import { it } from "node:test";
import { spawn, execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Executor } from "../src/executor.js";
import type { GracefulShutdownCheckpoint } from "../src/harness.js";
import type { CheckpointStop } from "../src/checkpoint-stop.js";
import { SinkGate } from "../src/sink-gate.js";
import { api, client, fakeGitHub, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, GIT_ENV, installRecoveryRootCleanup, virtualBoundaryClock } from "./runner-codex-sinks-fixture.js";
import { shutdownContentProof } from "./shutdown-3101-content-fixture.js";

installHarness();
installRecoveryRootCleanup();

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}

const points = ["sink queue", "boundary queue", "installer", "bare queue", "running transport",
  "context", "candidate context", "pin", "governance", "pending", "cas", "owner", "committed", "readback", "expired pending"] as const;

// Observation hooks call the real implementation first, while its original scope and lock
// remain held. They never manufacture ownership, a receipt, or a successful Git result.
interface ImportObservation {
  execScoped(command: string, args: string[], options: unknown): Promise<unknown>;
  writeOwedFile(bare: string, name: string, value: unknown, directory?: string): Promise<void>;
  persistOwedContext(context: unknown): Promise<void>;
  checkedTrackingOwner(bare: string, branch: string, sha: string): Promise<{ runId: string } | undefined>;
}

for (const [index, point] of points.entries()) {
  it(point === "expired pending" ? "m3 shutdown expiry during pending settlement fails closed with source custody" :
    "m3 shutdown during done at " + point + " preserves independent content within the original grace", async t => {
    const clock = virtualBoundaryClock();
    t.mock.method(Date, "now", clock.now);
    client.protocolFeatures = ["run_checkpoint_durability"];
    const iid = 312330 + index;
    const proof = shutdownContentProof(fx, client, iid);
    let active = false, fired = false, callbackRuns = 0, childSettled = false;
    let shutdownAt = 0, ackAt = 0, candidate = "", source = "", branch = "", runId = "";
    let shutdownProof: GracefulShutdownCheckpoint | undefined;
    let doneReturned = false, credentialCallsAtStop = 0, ordinaryAfterStop = -1;
    let lateDoneImports = 0, lateQueueMutations = 0, gateObserved = false;
    let settledOwner: unknown, failure: unknown, setupFailure: unknown, gateFailure: unknown, lifecycleReason: unknown;
    let rig: ReturnType<typeof codexRig> | undefined;
    const events: string[] = [];
    const expired = point === "expired pending";
    const stop = () => {
      if (!active || fired) return;
      fired = true;
      shutdownAt = clock.now();
      events.push("stop");
      credentialCallsAtStop = rig!.refreshCalls() + rig!.releaseCalls();
      worker.shutdown();
      const flight = (worker as unknown as { activeRuns: Map<string, { steering: { lifecycleSignal(): AbortSignal } }> }).activeRuns.get(runId);
      lifecycleReason = flight!.steering.lifecycleSignal().reason;
      clock.advance(expired ? 15_001 : 5_000);
    };
    proof.onShutdownReceipt(() => { ackAt = clock.now(); events.push("ack"); });

    const internal = git as unknown as ImportObservation;
    const exec = internal.execScoped.bind(git);
    t.mock.method(internal, "execScoped", async (command: string, args: string[], options: unknown) => {
      if (active && fired && point === "bare queue" &&
          (args.includes("fetch") || args.includes("update-ref") || args.includes("--replace-all"))) lateQueueMutations++;
      const result = await exec(command, args, options);
      if (active && !fired) {
        if (point === "pin" && args.includes("update-ref") && args.includes(`refs/uzi-owed/${runId}/${candidate}`)) stop();
        if (point === "governance" && args.includes("--replace-all") && args.includes(`uzi-trackowner.${branch}.receiptversion`)) stop();
        if (point === "cas" && args.includes("update-ref") && args.includes(`refs/uzi-runner/${branch}`)) stop();
        if (point === "owner" && args.includes("--replace-all") && args.includes(`uzi-trackowner.${branch}.owner`)) stop();
      }
      return result;
    });
    const write = internal.writeOwedFile.bind(git);
    t.mock.method(internal, "writeOwedFile", async (bare: string, name: string, value: unknown, directory?: string) => {
      const record = value as { phase?: string; sha?: string };
      if (active && !fired && expired && name.startsWith("receipt-") && record.phase === "pending") {
        const pendingWrite = write(bare, name, value, directory);
        const release = deferred();
        stop();
        events.push("pending deadline");
        queueMicrotask(release.resolve);
        await release.promise;
        await pendingWrite;
        events.push("pending settled");
        return;
      }
      await write(bare, name, value, directory);
      if (active && !fired && (
        (point === "candidate context" && name.startsWith("candidate-") && record.sha === candidate) ||
        (name.startsWith("receipt-") && record.phase === point))) {
        stop();
      }
    });
    const persist = internal.persistOwedContext.bind(git);
    t.mock.method(internal, "persistOwedContext", async (context: unknown) => {
      await persist(context);
      // The earlier acknowledged checkpoint already persisted this exact context.
      // Observe its real reuse under the mutation mask, without deleting metadata.
      if (active && point === "context") stop();
    });
    const owner = internal.checkedTrackingOwner.bind(git);
    t.mock.method(internal, "checkedTrackingOwner", async (bare: string, name: string, sha: string) => {
      const result = await owner(bare, name, sha);
      if (active && point === "readback" && name === branch && sha === candidate && result?.runId === runId) stop();
      return result;
    });

    const gateRun = SinkGate.prototype.run;
    t.mock.method(SinkGate.prototype, "run", async function<T>(this: SinkGate, fn: () => Promise<T>, checkpointStop?: CheckpointStop): Promise<T> {
      if (!active || fired || point !== "sink queue" || !checkpointStop) return gateRun.call(this, fn, checkpointStop) as Promise<T>;
      const release = this.tryAcquire(() => { stop(); queueMicrotask(() => release!()); });
      if (!release) throw new Error("expected idle real sink gate");
      try {
        return await gateRun.call(this, async () => { callbackRuns++; return fn(); }, checkpointStop) as T;
      } finally { release(); }
    });

    const fetch = git.fetchAgentBranch.bind(git);
    t.mock.method(git, "fetchAgentBranch", async (...args: Parameters<typeof git.fetchAgentBranch>) => {
      if (active && fired) lateDoneImports++;
      if (!active || fired || point !== "bare queue") return fetch(...args);
      const entered = deferred(), release = deferred();
      const predecessor = git.withBareLock(args[0], async () => {
        events.push("bare held"); entered.resolve(); await release.promise; events.push("bare settled");
      });
      await entered.promise;
      const onStep = args[4].checkpointStep;
      const result = fetch(...args.slice(0, 4) as [string, string, string, string], {
        ...args[4], checkpointStep: step => {
          onStep?.(step);
          if (step === "checkpoint_import_lock_wait") queueMicrotask(() => { stop(); release.resolve(); });
        },
      });
      try { return await result; } finally { release.resolve(); await predecessor; }
    });

    const executor: Executor = {
      get safety() { return rig?.safety; },
      run: async ctx => {
        source = ctx.worktreePath; branch = ctx.branch; runId = ctx.runId;
        rig = codexRig({
          lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]),
          now: clock.now, armDeadline: clock.armDeadline,
          boundaryProcess: async request => {
            if (!active || fired || point !== "running transport" || !request.argv.includes("fetch") ||
                !request.argv.some(arg => arg.startsWith("file://"))) return undefined;
            const child = spawn(request.argv[0]!, request.argv.slice(1), { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
            const terminal = new Promise<{ code: number }>((resolve, reject) => {
              child.once("error", reject);
              child.once("close", code => { childSettled = true; events.push("child settled"); resolve({ code: code ?? 128 }); });
            });
            child.once("spawn", () => { events.push("child running"); stop(); });
            return {
              stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, waitChild: () => terminal,
              root: {
                kind: "boundary_action",
                dispose: async () => { if (child.exitCode === null) child.kill("SIGKILL"); await terminal; },
                reap: async () => { await terminal; return { ok: true }; },
              },
            };
          },
        });
        const boundary = rig.safety.withBoundary.bind(rig.safety);
        rig.safety.withBoundary = async (request, action) => {
          if (request.boundary === "shutdown") {
            shutdownProof = request.gracefulShutdownCheckpoint;
            events.push("shutdown sink");
          }
          if (active && !fired && point === "boundary queue" && request.sink === "done_checkpoint") {
            const entered = deferred(), release = deferred();
            const predecessor = boundary({ boundary: "checkpoint", deadlineMs: 90_000 }, async () => {
              events.push("boundary held"); entered.resolve(); await release.promise; events.push("boundary settled");
            });
            await entered.promise;
            const queued = boundary(request, async permit => { callbackRuns++; return action(permit); });
            queueMicrotask(() => { stop(); release.resolve(); });
            try { return await queued; } finally { release.resolve(); await predecessor; }
          }
          return boundary(request, action);
        };
        clock.advance(1);
        await proof.prepare(ctx).catch(error => { setupFailure = error; throw error; });
        candidate = execFileSync("git", ["-C", source, "rev-parse", "HEAD"], { env: GIT_ENV, encoding: "utf8" }).trim();
        if (point === "installer") {
          const finish = deferred();
          rig.installer.promise = finish.promise.then(() => ({ results: [], truncated: false }));
          rig.installer.abort.signal.addEventListener("abort", () => queueMicrotask(() => {
            stop(); events.push("installer settled"); finish.resolve();
          }), { once: true });
        }
        active = true;
        try {
          await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" });
          doneReturned = true;
        } catch (error) { failure = error; }
        ordinaryAfterStop = rig.refreshCalls() + rig.releaseCalls() - credentialCallsAtStop;
        if (!expired && fired && ctx.codeCrossCheckGate) {
          gateObserved = true;
          await ctx.codeCrossCheckGate({ interlocked: true }).catch(error => { gateFailure = error; });
        }
        active = false;
        if (!expired && ["context", "candidate context", "pin", "governance", "pending", "cas", "owner", "committed", "readback"].includes(point)) {
          settledOwner = await git.committedTrackingOwnership(git.barePathFor(fx.originPath), branch, runId, candidate);
        }
        throw new Error("provider unwound after done checkpoint");
      },
    };
    const worker = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, {
      github: fakeGitHub().github,
      checkpointIntervalMs: 1, checkpointTickIntervalMs: 0,
      checkpointTestHooks: { absoluteNow: clock.now, armSoftDeadline: clock.arm },
    });
    try {
      await worker.execute(gitlabClaim(iid, { code_cross_check_required: true, claim_generation: 1, repo: {
        id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath,
        forge_type: "github", default_branch: "main",
      } }));
      assert.ifError(setupFailure);
      assert.equal(fired, true, "intended real transition reached");
      if (expired) {
        assert.equal(proof.tips.length, 1, "hard expiry cannot acknowledge newer content");
        assert.equal(ackAt, 0);
        assert.ok(events.indexOf("pending deadline") < events.indexOf("pending settled"));
        assert.ok(failure instanceof Error && (failure.name === "CodexBoundaryError" ||
          (failure as Error & { checkpointBoundary?: string }).checkpointBoundary === "hard"),
        "hard expiry remains a boundary failure rather than a clean soft stop");
        assert.ok(fs.existsSync(source), "source custody retained");
        assert.equal(fs.readFileSync(path.join(source, "TRACKED-3101.txt"), "utf8"), "tracked dirty bytes\n");
        assert.equal(fs.readFileSync(path.join(source, "UNTRACKED-3101.txt"), "utf8"), "separate untracked bytes\n");
        assert.equal(execFileSync("git", ["-C", source, "rev-parse", "HEAD"], { env: GIT_ENV, encoding: "utf8" }).trim(), candidate);
        const bare = git.barePathFor(fx.originPath);
        const refs = execFileSync("git", ["-C", bare, "for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/", `refs/uzi-owed/${runId}/`],
          { env: GIT_ENV, encoding: "utf8" });
        assert.ok(refs.split("\n").filter(sha => sha === candidate).length >= 2, "incoming stage and owed pin retain C");
        const receipts = fs.readdirSync(path.join(bare, "uzi-owed")).filter(name => name.startsWith("receipt-"));
        assert.ok(receipts.some(name => {
          const receipt = JSON.parse(fs.readFileSync(path.join(bare, "uzi-owed", name), "utf8"));
          return receipt.trackingSha === candidate && receipt.phase === "pending";
        }), "pending custody is not repaired or deleted");
      } else {
        await proof.verify(git);
        assert.equal(doneReturned, true, "cooperative shutdown returns only after owned work settles");
        assert.equal(failure, undefined);
        assert.equal(rig!.registry.isPoisoned(), false);
        assert.ok(shutdownProof, "real typed shutdown authority observed");
        assert.equal(shutdownProof.deadlineAt, shutdownAt + 15_000);
        assert.ok(ackAt >= shutdownAt + 5_000 && ackAt < shutdownAt + 15_000, "receiver verified content before ACK inside original grace");
        assert.equal(shutdownProof.stopSignal.aborted, false);
        if (point === "sink queue" || point === "boundary queue") assert.equal(callbackRuns, 0, "forfeited callback never ran");
        assert.equal(ordinaryAfterStop, 0, "no ordinary credential exchange after shutdown");
        assert.equal(gateObserved, true, "code gate was invoked after shutdown");
        assert.strictEqual(gateFailure, lifecycleReason, "queued retry propagates the real lifecycle stop");
        if (settledOwner !== undefined) assert.equal((settledOwner as { kind: string }).kind, "owned");
        if (point === "running transport") {
          assert.equal(childSettled, true);
          assert.ok(events.indexOf("child running") < events.indexOf("stop"));
          assert.ok(events.indexOf("child settled") < events.indexOf("shutdown sink"));
        }
        for (const predecessor of ["bare", "boundary"]) {
          if (events.includes(predecessor + " held")) assert.ok(events.indexOf(predecessor + " settled") < events.indexOf("shutdown sink"));
        }
      }
      assert.ok(api.states.length > 0);
      assert.equal(lateDoneImports, 0, "no fresh done snapshot import starts after shutdown");
      assert.equal(lateQueueMutations, 0, "forfeited bare waiter never starts a mutation");
    } finally { proof.cleanup(); }
  });
}
