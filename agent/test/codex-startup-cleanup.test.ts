import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { createInterface } from "node:readline";
import * as launcher from "../src/codex/launcher.js";
import { boundaryProcessSpawnerForTest, HeldRunCommandCache } from "../src/codex/codex-executor.js";
import { createCodexExecutionSafety, CodexBoundaryError } from "../src/codex/safety.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { COMMAND_UID } from "../src/runner-uid.js";
import { CheckpointChildStartupTimeoutError } from "../src/harness.js";
import type { StartupCleanupAuthorization, CodexEffectLaunchSpec } from "../src/codex/launcher.js";

type Variant = "valid" | "unconfirmed" | "wrong_authority" | "bad_array" | "wrong_id" |
  "missing_exit" | "nonzero_exit" | "missing_close" | "missing_output_end" | "missing_evidence_end" | "missing_started" |
  "hard_expiry" | "late_unsafe" | "late_malformed" | "late_abnormal" |
  "exit_before_evidence" | "exit_before_bad_evidence" | "stdout_overflow" | "stderr_overflow" | "at_output_caps";

class Transport extends EventEmitter {
  readonly pid = 12345;
  readonly stdin = new PassThrough();
  readonly stdout = new PassThrough();
  readonly stderr = new PassThrough();
  readonly control = new PassThrough();
  readonly evidence = new PassThrough();
  readonly stdio = [this.stdin, this.stdout, this.stderr, this.control, this.evidence];
  readonly operations: string[] = [];
  private hasStarted = false;
  readonly timers: ReturnType<typeof setTimeout>[] = [];
  readonly keepAlive = setInterval(() => {}, 1000);
  constructor(readonly variant: Variant = "valid") {
    super();
    createInterface({ input: this.control }).on("line", (line) => {
      const frame = JSON.parse(line);
      this.operations.push(frame.op);
      if (frame.op !== "dispose") return;
      // This frame is independently parsed AFTER the startup deadline; launch
      // remains rejected even though it eventually establishes a valid posture.
      if (variant !== "missing_started" && !this.hasStarted) this.started();
      if (["stdout_overflow", "stderr_overflow", "at_output_caps"].includes(variant)) {
        const chunk = Buffer.alloc(1024 * 1024);
        for (const stream of variant === "at_output_caps" ? [this.stdout, this.stderr] :
          [variant === "stdout_overflow" ? this.stdout : this.stderr]) {
          for (let i = 0; i < (variant === "at_output_caps" ? 64 : 65); i++) stream.write(chunk);
        }
      }
      const drained = {
        event: "dispose", id: variant === "wrong_id" ? frame.id + 1 : frame.id,
        state: "drained", authority: variant === "wrong_authority" ? "group" : "ECHILD+__WALL",
        killed: [], reaped: variant === "bad_array" ? ["unsafe"] : [this.pid + 1],
      };
      const emitEvidence = (): void => {
        this.frame(variant === "unconfirmed" ? {
          event: "dispose", id: frame.id, state: "unconfirmed", reason: "deadline",
          killed: [], reaped: [], children: [this.pid + 1],
        } : drained);
        if (variant === "late_unsafe") this.started({ nondumpable: false });
        if (variant === "late_malformed" || variant === "exit_before_bad_evidence") this.evidence.write("not-json\n");
        if (variant === "late_abnormal") this.frame({ event: "abnormal", reason: "untrusted-diagnostic" });
      };
      const finish = (): void => {
        if (variant !== "missing_exit") this.emit("exit", variant === "nonzero_exit" ? 7 : 0, null);
        if (variant === "exit_before_evidence" || variant === "exit_before_bad_evidence") emitEvidence();
        if (variant === "missing_close") return;
        if (variant !== "missing_output_end") { this.stdout.end(); this.stderr.end(); }
        if (variant !== "missing_evidence_end") this.evidence.end();
        // Node close is distinct from exit and follows stream final consumption.
        this.later(() => this.emit("close", 0, null), variant === "hard_expiry" ? 100 : 2);
      };
      if (variant !== "exit_before_evidence" && variant !== "exit_before_bad_evidence") emitEvidence();
      this.later(finish, 5);
    });
  }
  later(fn: () => void, ms: number): void { this.timers.push(setTimeout(fn, ms)); }
  frame(value: unknown): void { this.evidence.write(JSON.stringify(value) + "\n"); }
  started(extra: Record<string, unknown> = {}): void {
    this.hasStarted = true;
    this.frame({ event: "started", supervisorPid: this.pid, childPid: this.pid + 1,
      subreaper: true, nondumpable: true, uid: COMMAND_UID, liveCapsZero: true,
      capBoundingSet: "0xc0", noNewPrivs: true, ...extra });
  }
  destroy(): void {
    clearInterval(this.keepAlive);
    for (const timer of this.timers) clearTimeout(timer);
    for (const stream of this.stdio) stream.destroy();
  }
}
const transports: Transport[] = [];
afterEach(() => { for (const t of transports) t.destroy(); transports.length = 0; });
function transport(variant: Variant = "valid"): Transport {
  const t = new Transport(variant); transports.push(t); return t;
}
const spec: CodexEffectLaunchSpec = {
  identity: "command", command: "/bin/true", args: [], cwd: "/work/repo", env: {},
  supervisorBin: "/usr/local/bin/uzi-codex-supervisor",
};
function authorization(ms = 300): StartupCleanupAuthorization {
  return Object.freeze({ hardDeadlineAt: Date.now() + ms, attempt: Object.freeze({}) });
}
function launch(t: Transport, context?: StartupCleanupAuthorization, started = 2) {
  return launcher.launchCodexEffectRoot(spec, {
    env: { UZI_UID_SPLIT: "1" }, resolveCommandUid: () => COMMAND_UID,
    spawnSupervisor: () => t as unknown as launcher.SupervisorProcess, deadlines: { started, exit: 100 }, startupCleanup: context,
  });
}
async function rejection(promise: Promise<unknown>): Promise<unknown> {
  try { await promise; } catch (error) { return error; }
  assert.fail("startup must reject, never return a successful handle");
}
function certificate(error: unknown, context?: StartupCleanupAuthorization): boolean {
  // Namespace access allows the regression to execute on the unfixed module.
  assert.equal(typeof launcher.hasVerifiedStartupCleanup, "function", "launcher must verify attempt-bound cleanup");
  return launcher.hasVerifiedStartupCleanup(error, context);
}

describe("M1 rejected startup cleanup through the actual launcher parser", () => {
  it("delayed started -> drained -> exit -> close still rejects with an exact-attempt certificate", async () => {
    const t = transport();
    const context = authorization();
    const error = await rejection(launch(t, context));
    assert.deepEqual(t.operations, ["dispose"], "startup expiry must explicitly dispose before rejecting");
    assert.ok(error instanceof launcher.CodexLaunchError);
    assert.equal(error.classification, "started_deadline");
    assert.equal(error.cleanupStatus, "verified");
    assert.equal(certificate(error, context), true);
    assert.equal(certificate(error, authorization()), false, "foreign attempts cannot reuse a proof");
    assert.deepEqual(t.operations, ["dispose"]);
    assert.equal(t.evidence.readableEnded, true);
    assert.equal(t.stdout.readableEnded, true);
    assert.equal(t.stderr.readableEnded, true);
  });
  for (const variant of ["unconfirmed", "wrong_authority", "bad_array", "wrong_id", "missing_exit",
    "nonzero_exit", "missing_close", "missing_output_end", "missing_evidence_end", "missing_started", "hard_expiry", "late_unsafe",
    "late_malformed", "late_abnormal", "exit_before_bad_evidence", "stdout_overflow", "stderr_overflow"] as Variant[]) {
    it(`forbids startup recovery: ${variant}`, async () => {
      const t = transport(variant);
      const context = authorization(variant === "hard_expiry" ? 35 : 150);
      const error = await rejection(launch(t, context));
      assert.ok(error instanceof launcher.CodexLaunchError);
      assert.equal(certificate(error, context), false);
      assert.equal(error.cleanupStatus, "unconfirmed");
      assert.equal(t.control.writableEnded || t.control.destroyed, true, "failed cleanup ends control best effort");
      assert.ok(!error.message.includes("untrusted-diagnostic"));
      if (variant.endsWith("overflow")) assert.equal(error.classification, "output_limit");
    });
  }
  it("settles cleanup within the original hard deadline after startup spends most of its budget", async () => {
    const context = authorization(1000);
    const t = transport();
    const error = await rejection(launch(t, context, 900));
    assert.deepEqual(t.operations, ["dispose"]);
    assert.equal(certificate(error, context), true);
    assert.ok(Date.now() < context.hardDeadlineAt);
  });
  it("consumes buffered evidence after exit before accepting close", async () => {
    const context = authorization();
    assert.equal(certificate(await rejection(launch(transport("exit_before_evidence"), context)), context), true);
  });
  it("drains exactly 64MiB on EACH output channel without storing output", async () => {
    const context = authorization(1000);
    const t = transport("at_output_caps");
    assert.equal(certificate(await rejection(launch(t, context)), context), true);
    assert.equal(t.stdout.readableLength, 0);
    assert.equal(t.stderr.readableLength, 0);
  });
  it("a reused authorization is refused before spawning another owned process", async () => {
    const context = authorization();
    assert.equal(certificate(await rejection(launch(transport(), context)), context), true);
    let spawned = false;
    const error = await rejection(launcher.launchCodexEffectRoot(spec, {
      env: { UZI_UID_SPLIT: "1" }, startupCleanup: context,
      spawnSupervisor: () => { spawned = true; return transport() as unknown as launcher.SupervisorProcess; },
    }));
    assert.equal(spawned, false);
    assert.equal(certificate(error, context), false);
  });
  it("plain launches reject startup without requesting authorized disposal", async () => {
    const t = transport();
    const error = await rejection(launch(t));
    assert.ok(error instanceof launcher.CodexLaunchError);
    assert.equal(error.classification, "started_deadline");
    assert.equal(error.cleanupStatus, "unconfirmed");
    assert.deepEqual(t.operations, []);
    assert.equal(t.control.writableEnded, true);
  });
  it("classifies allowlisted spawn errno without rendering diagnostics", async () => {
    const cause = Object.assign(new Error("private-path-and-provider-input"), { code: "EACCES" });
    const error = await rejection(launcher.launchCodexEffectRoot(spec, {
      env: { UZI_UID_SPLIT: "1" }, resolveCommandUid: () => COMMAND_UID,
      spawnSupervisor: () => { throw cause; },
    }));
    assert.ok(error instanceof launcher.CodexLaunchError);
    assert.equal(error.classification, "spawn_EACCES");
    assert.equal(error.cause, cause);
    assert.equal(error.message.includes(cause.message), false);
  });
});

function safetyFor(seam: ReturnType<typeof boundaryProcessSpawnerForTest>) {
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(2465));
  return { registry, safety: createCodexExecutionSafety(registry, async () => { throw new Error("unused"); },
    undefined, undefined, seam) };
}
const request = { argv: ["/bin/true"], cwd: "/work/repo", env: {}, identity: "command" as const,
  timeoutMs: 2, recoverableTimeout: true as const };
function cache() {
  return new HeldRunCommandCache({
    path: "/cache/test", alive: () => true,
    release: async () => ({ clean: true }),
  } as unknown as launcher.CommandCacheHolder);
}

describe("M1 boundary and cache authority", () => {
  it("keeps the reservation pending, preserves hard deadline through cache/spawner, then throws recoverable typed startup", async () => {
    const held = cache();
    let seamContext: StartupCleanupAuthorization | undefined;
    let cacheContext: StartupCleanupAuthorization | undefined;
    let launcherContext: StartupCleanupAuthorization | undefined;
    let authenticError: unknown;
    let startupBudget: number | undefined;
    let registry: ExecutionRegistry;
    const wrapped = held.track(async (_spec, startupMs, context) => {
      launcherContext = context;
      startupBudget = startupMs;
      assert.equal(registry.pendingLaunchCount(), 1);
      const t = transport();
      t.control.on("data", () => assert.equal(registry.pendingLaunchCount(), 1, "cleanup retains launch reservation"));
      try { return await launch(t, context, startupMs); }
      catch (error) { authenticError = error; throw error; }
    });
    const boundarySeam = boundaryProcessSpawnerForTest("required", async (launchSpec, ms, context) => {
      cacheContext = context;
      return wrapped(launchSpec, ms, context);
    });
    const rig = safetyFor(async (processRequest, ms, context) => {
      seamContext = context;
      return boundarySeam(processRequest, ms, context);
    });
    registry = rig.registry;
    await rig.safety.withBoundary({ boundary: "checkpoint", deadlineMs: 1000 }, async (permit) => {
      const error = await rejection(rig.safety.spawnBoundaryProcess(permit, request));
      assert.ok(error instanceof CheckpointChildStartupTimeoutError);
      assert.equal(error.cause, authenticError);
      assert.ok(authenticError instanceof launcher.CodexLaunchError);
      assert.equal(error.classification, "started_deadline");
      assert.equal(error.cleanupStatus, "verified");
      assert.equal(certificate(error, seamContext), false, "the recovery marker is not cleanup authority");
      assert.equal(certificate(authenticError, seamContext), true);
      assert.equal(seamContext, cacheContext);
      assert.equal(cacheContext, launcherContext);
      assert.ok(Object.isFrozen(seamContext));
      assert.equal(seamContext!.hardDeadlineAt,
        (rig.safety as unknown as { currentDeadlineAt: number }).currentDeadlineAt);
      assert.equal(cacheContext!.hardDeadlineAt, seamContext!.hardDeadlineAt);
      assert.equal(launcherContext!.hardDeadlineAt, seamContext!.hardDeadlineAt);
      assert.ok(startupBudget! > 0 && startupBudget! <= request.timeoutMs);
      assert.equal(registry.pendingLaunchCount(), 0);
      assert.equal(registry.isPoisoned(), false);
      assert.equal(held.drained(), true, "cache settled authentic rejection before safety wrapped it");
    });
    assert.equal(held.drained(), true, "only certified rejected roots count as clean");
  });
  it("cache forwards the ORIGINAL context object and absolute deadline unchanged", async () => {
    const held = cache();
    const context = authorization(1000);
    const deadline = context.hardDeadlineAt;
    const t = transport();
    let forwarded: StartupCleanupAuthorization | undefined;
    const tracked = held.track(async (_spec, ms, auth) => {
      forwarded = auth;
      return launch(t, auth, ms);
    });
    const error = await rejection(tracked(spec, 2, context));
    assert.equal(forwarded, context);
    assert.equal(forwarded!.hardDeadlineAt, deadline);
    assert.equal(certificate(error, context), true);
    assert.equal(held.drained(), true);
  });
  it("a replay with the SAME authorization object cannot certify a later cache invocation", async () => {
    const held = cache();
    const context = authorization();
    const error = await rejection(held.track(async (_spec, ms, auth) => launch(transport(), auth, ms))(spec, 2, context));
    assert.equal(held.drained(), true);
    await rejection(held.track(async () => { throw error; })(spec, 2, context));
    assert.equal(held.drained(), false, "proof must be minted during this invocation, not merely match its context");
  });
  it("a mismatched/reused genuine rejection poisons the new attempt and taints the cache", async () => {
    const original = authorization();
    const reused = await rejection(launch(transport(), original));
    assert.equal(certificate(reused, original), true);
    const held = cache();
    const seam = boundaryProcessSpawnerForTest("required", held.track(async () => { throw reused; }));
    const { registry, safety } = safetyFor(seam);
    await assert.rejects(safety.withBoundary({ boundary: "checkpoint", deadlineMs: 1000 }, async (permit) => {
      const error = await rejection(safety.spawnBoundaryProcess(permit, request));
      assert.ok(error instanceof launcher.CodexLaunchError);
      assert.equal(error.cause, reused);
      assert.equal(error.cleanupStatus, "unconfirmed");
    }), CodexBoundaryError);
    assert.equal(registry.isPoisoned(), true);
    assert.equal(held.drained(), false);
  });
  for (const fabricated of ["untyped", "launcher", "worker_marker"] as const) {
    it(`poisons fabricated ${fabricated} rejection`, async () => {
      const held = cache();
      const cause = fabricated === "launcher" ? new launcher.CodexLaunchError("started_deadline", "verified", undefined) :
        fabricated === "worker_marker" ? new CheckpointChildStartupTimeoutError(new Error("private-input")) :
        new Error("started deadline exceeded private-input");
      const { registry, safety } = safetyFor(boundaryProcessSpawnerForTest("required",
        held.track(async () => { throw cause; })));
      await assert.rejects(safety.withBoundary({ boundary: "checkpoint", deadlineMs: 1000 }, async (permit) => {
        const error = await rejection(safety.spawnBoundaryProcess(permit, request));
        assert.ok(error instanceof launcher.CodexLaunchError);
        assert.equal(error.classification, "unknown");
        assert.equal(error.cause, cause);
        assert.equal(error.message.includes("private-input"), false);
      }), CodexBoundaryError);
      assert.equal(registry.isPoisoned(), true);
      assert.equal(held.drained(), false);
    });
  }
  it("ordinary boundary launches do not receive a hard cleanup capability", async () => {
    const { safety } = safetyFor(boundaryProcessSpawnerForTest("required", async (_spec, ms, context) => {
      assert.equal(context, undefined);
      const t = transport();
      t.started();
      return launch(t, context, ms);
    }));
    await safety.withBoundary({ boundary: "checkpoint", deadlineMs: 1000 }, async (permit) => {
      const child = await safety.spawnBoundaryProcess(permit, { ...request, recoverableTimeout: undefined });
      await child.cancel();
    });
  });
});
