import assert from "node:assert/strict";
import { it } from "node:test";
import { spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { CodexBoundaryError } from "../src/codex/safety.js";
import { captureFailureOf, EpochCleanupUnprovenError } from "../src/harness.js";
import { GitBoundaryAbortError } from "../src/checkpoint-stop.js";
import { TrustedExecutionRefusal } from "../src/trusted-execution-refusal.js";
import { StaleClaimError } from "../src/runner.js";
import { DiskParkSignal } from "../src/cache-cap.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { api, client, fx, git, fakeGitlab, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, FakeCodexExecutor, manualDeadline, statuses } from "./runner-codex-sinks-fixture.js";
import { JOIN_TOKEN } from "./runner-held-publication-fixture.js";

installHarness();

for (const route of ["publish-refused", "pause-rejected", "pause-ack-lost", "different-facade"] as const) {
  it(`completed false pause retains only its matching unproven epoch: ${route}`, async t => {
    const rig = codexRig();
    const claim = { ...gitlabClaim(2852), claim_generation: 1, park_run_supported: true };
    let clone = "", pauses = 0, publications = 0, packedBytes = 0;
    const publish = client.publishCheckpoint.bind(client);
    t.mock.method(client, "publishCheckpoint", async (...args: Parameters<typeof publish>) => {
      publications++;
      for await (const chunk of args[2]) packedBytes += chunk.length;
      return route === "publish-refused" || route === "different-facade"
        ? { ok: false, httpStatus: 400 }
        : { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-2852" } };
    });
    const report = client.reportState.bind(client);
    t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
      if (args[1].status !== "paused") return report(...args);
      pauses++;
      if (route === "pause-rejected") return { applied: false, status: "running" };
      const response = await report(...args);
      if (route === "pause-ack-lost") throw new Error("fixture lost pause acknowledgement");
      return response;
    });
    const executor = new FakeCodexExecutor(rig.safety, async ctx => {
      clone = ctx.worktreePath;
      fs.writeFileSync(path.join(clone, "PAUSE.txt"), "retained pause bytes\n");
      fs.writeFileSync(path.join(homeDir, "pause-session"), "retained session\n");
      assert.equal(await ctx.parkForPause!({ completedCount: 1 }), false, "real pause sink declined after capture");
      throw new EpochCleanupUnprovenError(route === "different-facade" ? codexRig().safety : rig.safety);
    }, rig.settle, rig.capture);
    const runner = runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, JOIN_TOKEN,
      undefined, { checkpointIntervalMs: 0 });
    await runner.execute(claim);
    assert.ok(publications > 0, "actual checkpoint publication reached");
    assert.ok(packedBytes > 0, "the broker consumed the real Git pack");
    assert.equal(pauses > 0, route === "pause-rejected" || route === "pause-ack-lost");
    assert.equal(statuses(claim.run_id).includes("failed"), route === "different-facade",
      "only the same refused pause facade selects B1 instead of terminal failure");
    if (route !== "different-facade") {
      assert.equal(api.states.some(row => row.body.status === "completed"), false);
      assert.equal(fs.readFileSync(path.join(clone, "PAUSE.txt"), "utf8"), "retained pause bytes\n");
      assert.equal(fs.readFileSync(path.join(homeDir, "pause-session"), "utf8"), "retained session\n");
    }
  });
}

for (const timing of ["reap", "microtask"] as const) {
  it("R2 independent child error before capture expiry must reject pause: " + timing, async t => {
    const { PassThrough } = await import("node:stream");
    const deadline = manualDeadline("checkpoint");
    const primary = new TrustedExecutionRefusal("independent child verification failure");
    let sawPrimary = false;
    const rig = codexRig({ armDeadline: deadline.armDeadline,
      boundaryProcess: async () => ({
        stdin: null, stdout: new PassThrough(), stderr: new PassThrough(),
        waitChild: () => {
          sawPrimary = true;
          const terminal = Promise.reject(primary);
          if (timing === "microtask") queueMicrotask(() => queueMicrotask(() => deadline.fire()));
          return terminal;
        },
        root: { kind: "boundary_action", dispose: async () => {},
          reap: async () => {
            assert.equal(sawPrimary, true);
            if (timing === "reap") deadline.fire();
            return { ok: true };
          },
        },
      }),
    });
    const runner = runnerWith(() => assert.fail("no executor needed"), fakeGitlab().gitlab);
    const internal = runner as any;
    t.mock.method(internal, "runGatedSink", async (_flight: unknown, action: () => Promise<unknown>) => action());
    const reports: unknown[] = [], messages: unknown[] = [];
    const flight: any = {
      executor: { safety: rig.safety, withCredentialFreeCapture: rig.capture },
      steering: { getPauseMode: () => null, checkpointAuthoritySignal: () => new AbortController().signal },
      runId: "r2-child-primary", runLog: { warn: () => {} },
      batcher: { emit: (message: unknown) => messages.push(message), flush: async () => {} },
      reportState: async (body: unknown) => { reports.push(body); return { applied: true, status: "running" }; },
    };
    t.mock.method(internal, "handlePauseParkHeld", async () => git.branchTip(fx.originPath, "main"));
    await assert.rejects(internal.handlePausePark(gitlabClaim(2852), flight, { completedCount: 1 }, { origin: "owner" }),
      (error: unknown) => {
        assert.ok(error instanceof CodexBoundaryError);
        assert.equal(error.actionError, primary);
        assert.equal(captureFailureOf(error)?.deadlineExpired, true);
        assert.equal(captureFailureOf(error)?.actionRevoked, undefined);
        return true;
      });
    assert.equal(flight.refusedPauseCaptureSafety, undefined);
    assert.deepEqual(reports, []);
    assert.deepEqual(messages, []);
  });
}

it("epoch cleanup refusal keeps facade identity out of wire fields", () => {
  const { safety } = codexRig();
  const error = new EpochCleanupUnprovenError(safety);
  assert.ok(error instanceof TrustedExecutionRefusal);
  assert.equal(error.name, "TrustedExecutionRefusal");
  assert.equal(error.message, "prior Codex epoch cleanup unproven");
  assert.equal(error.safety, safety);
  assert.equal(Object.hasOwn(error, "safety"), false);
  assert.equal(JSON.stringify(error), JSON.stringify({ name: "TrustedExecutionRefusal" }));
});

// Exercise the task's handlePausePark/runFreeCapture seams with real capture ownership.
// Only the source action is replaced, so its primary failure is independent of cleanup.
for (const mode of ["expired-marker", "sentinel-expiry", "sentinel-cleanup", "boundary", "refusal", "boundary-expiry",
  "unrelated-expired", "git-primary-expiry", "ordinary-factory-expiry", "deferral", "cancel", "cancel-only", "branded-external", "branded-both", "stale", "stale-state", "shutdown", "disk", "disk-primary", "credential", "credential-state",
  "shutdown-primary", "disk-primary-error", "credential-primary-error", "dedup"] as const) {
  it(`R2 pause capture preserves primary and newer outcome: ${mode}`, async t => {
    const deadline = manualDeadline("checkpoint");
    const external = new AbortController();
    const authority = new AbortController();
    const sentinel = mode === "git-primary-expiry"
      ? new GitBoundaryAbortError("independent Git boundary", new Error("primary cause"))
      : new Error("meaningful primary sentinel", { cause: new Error("primary cause") });
    const ordinary = new CodexBoundaryError("action", [{ category: "tool", message: "unrelated boundary" }], sentinel);
    const deferred = new CodexBoundaryError("reconcile", [{ category: "authorization", message: "vault locked" }], sentinel, "vault_locked");
    const refusal = new TrustedExecutionRefusal("credential-free capture scope unavailable");
    const newer = mode === "stale" || mode === "stale-state" ? new StaleClaimError()
      : mode === "disk" || mode === "disk-primary" ? new DiskParkSignal(false)
      : mode === "credential" || mode === "credential-state" || mode === "credential-primary-error" ? new CredentialSwitchSignal() : new Error(mode + " authority stop");
    let cleanupJoined = false;
    const rig = codexRig({
      armDeadline: deadline.armDeadline,
      boundaryProcess: async request => {
        if (mode !== "sentinel-cleanup" && mode !== "dedup") return undefined;
        const [bin, ...args] = request.argv;
        assert.ok(bin);
        const child = spawn(bin, args, { cwd: request.cwd, env: request.env, stdio: "pipe" });
        const terminal = new Promise<{ code: number }>((resolve, reject) => {
          child.once("error", reject);
          child.once("close", code => resolve({ code: code ?? 1 }));
        });
        return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, waitChild: () => terminal,
          root: { kind: "boundary_action",
            reap: async () => { await terminal; cleanupJoined = true; return { ok: false,
              error: { category: "tool", message: "R2 independent child cleanup refusal" } }; },
            dispose: async () => { child.stdout.resume(); child.stderr.resume(); await terminal; },
          } };
      },
    });
    const runner = runnerWith(() => assert.fail("no executor factory needed"), fakeGitlab().gitlab);
    const internal = runner as any;
    t.mock.method(internal, "runGatedSink", async (_flight: unknown, action: () => Promise<unknown>) => action());
    const reports: unknown[] = [], messages: unknown[] = [], warnings: unknown[] = [];
    const flight: any = {
      executor: { safety: rig.safety, withCredentialFreeCapture: rig.capture },
      steering: { getPauseMode: () => mode === "disk" || mode === "disk-primary-error" ? "disk" : null,
        checkpointAuthoritySignal: () => authority.signal },
      runId: "r2-unit", runLog: { warn: (...args: unknown[]) => warnings.push(args) },
      batcher: { emit: (message: unknown) => messages.push(message), flush: async () => {} },
      reportState: async (body: unknown) => { reports.push(body); return { applied: true, status: "running" }; },
    };
    t.mock.method(internal, "handlePauseParkHeld", async (
      _claim: unknown, _flight: unknown, held: unknown, _at: unknown, _origin: unknown, _ack: unknown,
      reportRefusal: (blocked: boolean) => Promise<false>,
    ) => {
      if (mode === "sentinel-cleanup" || mode === "dedup") {
        await git.branchTip(fx.originPath, "main").catch(() => null);
        assert.equal(cleanupJoined, true);
        if (mode === "dedup") return reportRefusal(false);
        throw sentinel;
      }
      if (mode === "boundary") throw ordinary;
      if (mode === "refusal") throw refusal;
      if (mode === "git-primary-expiry" || mode === "ordinary-factory-expiry") {
        // Exercise branchTip's real boundaryAbortError catch after a primary
        // failure and a later, independent capture deadline.
        t.mock.method(git as any, "runGitAsRunner", async () => {
          try { throw sentinel; } finally { deadline.fire(); }
        });
        return git.branchTip(fx.originPath, "main");
      }
      if (mode === "branded-external" || mode === "branded-both") {
        external.abort(newer);
        if (mode === "branded-both") deadline.fire();
        throw new GitBoundaryAbortError("capture revoked externally", undefined, (held as { signal: AbortSignal }).signal);
      }
      if (mode !== "cancel-only") deadline.fire();
      if (mode === "expired-marker") internal.assertHeldFreeCapture(flight, held);
      if (mode === "sentinel-expiry") throw sentinel;
      if (mode === "unrelated-expired") throw refusal;
      if (mode === "boundary-expiry") throw ordinary;
      if (mode === "deferral") throw deferred;
      if (mode === "shutdown" || mode === "shutdown-primary") internal.shutdownSignal.abort(newer);
      else if (mode === "cancel" || mode === "cancel-only") external.abort(newer);
      else if (mode !== "disk" && mode !== "stale" && mode !== "credential" && mode !== "disk-primary") authority.abort(newer);
      if (mode === "stale" || mode === "credential" || mode === "disk-primary") throw newer;
      if (mode === "shutdown-primary" || mode === "disk-primary-error" || mode === "credential-primary-error") throw ordinary;
      return false;
    });
    // Real safety's external abort input, independently of its own armed timer.
    flight.executor.withCredentialFreeCapture = (ms: number, action: any, purpose: any, signal: AbortSignal) =>
      rig.capture(ms, action, purpose, AbortSignal.any([signal, external.signal]));
    const operation = internal.handlePausePark(gitlabClaim(2852), flight, { completedCount: 1 }, { origin: "owner" });
    if (mode === "expired-marker" || mode === "dedup") {
      assert.equal(await operation, false);
      assert.equal(flight.sourceJoinFailed, true);
      assert.equal(flight.preserveRecoveryClone, true);
      assert.equal(flight.preserveSession, true);
      assert.equal(flight.refusedPauseCaptureSafety, rig.safety, "this incomplete refusal records its exact facade");
      assert.equal(reports.length, 1, "body plus cleanup emits one refusal");
      assert.equal(messages.length, 1);
      assert.doesNotMatch(JSON.stringify(messages), /residue|process/i);
    } else {
      await assert.rejects(operation, (error: unknown) => {
        if (mode === "boundary") assert.equal(error, ordinary);
        else if (mode === "refusal") assert.equal(error, refusal);
        else if (mode === "stale" || mode === "credential" || mode === "stale-state" || mode === "credential-state" || mode === "shutdown" || mode === "shutdown-primary" || mode === "credential-primary-error" || mode === "disk-primary") assert.equal(error, newer);
        else if (mode === "deferral") {
          assert.equal(error, deferred);
          assert.equal(error.deferral, "vault_locked", "outer runner deferral probe reads the direct error");
          assert.equal(error.actionError, sentinel);
          assert.equal(error.cause, sentinel);
        }
        else if (mode === "disk" || mode === "disk-primary-error") assert.ok(error instanceof DiskParkSignal);
        else {
          assert.ok(error instanceof CodexBoundaryError);
          if (mode === "cancel" || mode === "cancel-only" || mode === "branded-external" || mode === "branded-both") {
            assert.equal(captureFailureOf(error)?.externallyAborted, true);
            if (mode === "branded-external" || mode === "branded-both") {
              assert.equal(captureFailureOf(error)?.actionRevoked, true);
              assert.equal(captureFailureOf(error)?.deadlineExpired, mode === "branded-both");
            }
          }
          else {
            assert.equal(mode === "ordinary-factory-expiry" ? (error.actionError as Error).cause : error.actionError, mode === "unrelated-expired" ? refusal : mode === "boundary-expiry" ? ordinary
              : sentinel);
            if (mode === "sentinel-cleanup") assert.deepEqual(captureFailureOf(error), {
              deadlineExpired: false, childCleanupFailed: true, externallyAborted: false,
            });
            assert.equal(error.cause, error.actionError);
            assert.equal(captureFailureOf(error)?.actionRevoked, undefined);
            if (mode === "git-primary-expiry") assert.equal(error.actionError, sentinel);
            if (mode === "ordinary-factory-expiry") {
              assert.ok(error.actionError instanceof GitBoundaryAbortError);
              assert.equal(error.actionError.cause, sentinel);
            }
          }
          assert.ok(error.errors.length > 0, "cleanup/expiry evidence retained separately");
        }
        return true;
      });
      if (mode === "deferral" || mode === "stale" || mode === "credential" || mode === "disk-primary") {
        assert.equal(warnings.length, 1, "capture cleanup diagnostics retained separately from exact control");
      }
      assert.equal(flight.refusedPauseCaptureSafety, undefined, "primary and lifecycle exits never authorize guard retention");
      assert.deepEqual(reports, [], "no pause_failed can replace a meaningful primary or newer outcome");
      assert.deepEqual(messages, []);
    }
  });
}
