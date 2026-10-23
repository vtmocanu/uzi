import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { setImmediate as tick } from "node:timers/promises";
import type { Executor } from "../src/executor.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { carriesForgeCredential } from "../src/residue-quarantine.js";
import { api, git, gitlabClaim, fakeGitlab, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, FakeCodexExecutor, spyPublishLands, statuses, VAULT_FEATURE } from "./runner-codex-sinks-fixture.js";
import { client } from "./runner-harness.js";
import { nullLogger } from "./helpers.js";

installHarness();

// Observe the existing private seams, preserving their implementations and policy.
type Flight = {
  sourceExitDenied?: boolean; preserveRecoveryClone?: boolean; stoppedSourceKept?: boolean;
};
type Seam = {
  runGatedSink<T>(flight: Flight, action: () => Promise<T>): Promise<T>;
  settleRecoveryGeneration(...args: unknown[]): Promise<unknown>;
  reapThenSettleRecoveryGeneration(...args: unknown[]): Promise<unknown>;
  retainStoppedSource(flight: Flight, executor: Executor, reason: string, ...rest: unknown[]): Promise<unknown>;
};

for (const mode of ["healthy-B", "fallback-B", "vault-A", "claude", "claude-terminal", "codex-terminal"] as const) {
  it(`recovery capture tail: ${mode} settlement and cleanup preserve actual authority`, async t => {
    const lifecycle = new AbortController();
    let armed = false, authenticated = false, free = false, captureReturned = false;
    let observed: Flight | undefined;
    let settlements = 0, reapSettlements = 0, freeEntries = 0;
    const reasons: string[] = [];
    const lateReads: string[] = [];
    const credentialLaunches: boolean[] = [];
    const authDeltas: Array<{ refresh: number; release: number }> = [];
    const claude = mode.startsWith("claude");
    const rig = codexRig({
      lifecycleSignal: lifecycle.signal,
      vaultLocked: () => mode === "vault-A",
      blockReconcile: mode === "fallback-B",
      boundaryProcess: request => {
        if (free) credentialLaunches.push(carriesForgeCredential(request.env));
        return undefined;
      },
    });
    const boundary = rig.safety.withBoundary.bind(rig.safety);
    t.mock.method(rig.safety, "withBoundary", async (request: Parameters<typeof boundary>[0], action: Parameters<typeof boundary>[1]) =>
      boundary(request, async permit => {
        authenticated = true;
        try { return await action(permit); }
        finally { authenticated = false; captureReturned = armed; }
      }));
    const capture: typeof rig.capture = (ms, action, purpose, signal, attempt) =>
      rig.capture(ms, async held => {
        freeEntries++;
        free = true;
        const refresh = rig.refreshCalls(), release = rig.releaseCalls();
        try { return await action(held); }
        finally {
          authDeltas.push({ refresh: rig.refreshCalls() - refresh, release: rig.releaseCalls() - release });
          free = false;
          captureReturned = true;
        }
      }, purpose, signal, attempt);
    for (const name of ["worktreeStatus", "worktreeHead", "commitWipMarker", "verifyRunnerTrackingCovers", "trackingTip"] as const) {
      const original = git[name].bind(git);
      t.mock.method(git, name, async (...args: unknown[]) => {
        if (armed && captureReturned && !authenticated && !free) lateReads.push(name);
        return Reflect.apply(original, git, args);
      });
    }
    const publication = spyPublishLands();
    t.after(publication);
    if (mode === "vault-A") client.protocolFeatures = [VAULT_FEATURE];
    const claim = gitlabClaim(285480 + ["healthy-B", "fallback-B", "vault-A", "claude", "claude-terminal", "codex-terminal"].indexOf(mode), { claim_generation: 3 });
    const behavior: Executor["run"] = async ctx => {
      if (ctx.terminalLifecycleSignal!.aborted) lifecycle.abort(ctx.terminalLifecycleSignal!.reason);
      else ctx.terminalLifecycleSignal!.addEventListener("abort", () => lifecycle.abort(ctx.terminalLifecycleSignal!.reason), { once: true });
      fs.writeFileSync(path.join(ctx.worktreePath, "TAIL.txt"), "capture tail real bytes\n");
      // Obtain the flight through an actual existing sink, without inventing authority.
      await ctx.codeCrossCheckSummary?.();
      armed = true;
      if (mode.endsWith("terminal")) api.overrideStateStatus(claim.run_id, "failed");
      if (mode === "vault-A") {
        await rig.safety.withBoundary({ boundary: "shutdown", deadlineMs: 15_000 }, async () => {});
        throw new Error("real locked vault unexpectedly admitted");
      }
      throw new TransientRecoveryError("capture tail transient");
    };
    const executor: Executor = claude ? { run: behavior } : new FakeCodexExecutor(rig.safety, behavior, undefined, capture);
    const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { recoveryRetryMs: 1 });
    const seam = runner as unknown as Seam;
    const sink = seam.runGatedSink.bind(runner);
    t.mock.method(seam, "runGatedSink", async <T>(flight: Flight, action: () => Promise<T>) => {
      observed = flight;
      return sink(flight, action);
    });
    const settle = seam.settleRecoveryGeneration.bind(runner);
    t.mock.method(seam, "settleRecoveryGeneration", async (...args: unknown[]) => {
      settlements++;
      if (!claude) {
        assert.equal(authenticated, true, "final settle enters within authenticated capture");
        assert.equal(free, false);
        assert.equal(rig.registry.state(), "closed");
        await tick(); // Require the same authority across an awaited tail barrier.
        assert.equal(authenticated, true, "settle barrier cannot outlive authenticated capture");
        assert.equal(rig.registry.state(), "closed");
      }
      return settle(...args);
    });
    const reapSettle = seam.reapThenSettleRecoveryGeneration.bind(runner);
    t.mock.method(seam, "reapThenSettleRecoveryGeneration", async (...args: unknown[]) => {
      reapSettlements++;
      return reapSettle(...args);
    });
    const retain = seam.retainStoppedSource.bind(runner);
    t.mock.method(seam, "retainStoppedSource", async (flight: Flight, exec: Executor, reason: string, ...rest: unknown[]) => {
      reasons.push(reason);
      return retain(flight, exec, reason, ...rest);
    });
    await runner.execute(claim);
    if (!claude) assert.ok(observed, "actual runner sink supplied the flight");
    if (mode === "codex-terminal") {
      assert.ok(reasons.includes("recovery_park_ack_terminal"), "terminal reason retains its existing coverage path");
      assert.ok(!reasons.includes("recovery_park_keep"));
    } else if (claude) {
      assert.ok(reasons.includes(mode === "claude-terminal" ? "recovery_park_ack_terminal" : "recovery_park_keep"),
        JSON.stringify(reasons));
    } else {
      assert.deepEqual(lateReads, [], "Codex final diagnostic never reads source after capture release");
      assert.ok(!reasons.includes("recovery_park_keep"), "Codex skips the actual cleanup diagnostic helper");
      assert.equal(observed!.sourceExitDenied, true, "cleanup denies source exit");
      assert.equal(observed!.preserveRecoveryClone, true, "cleanup keeps clone custody");
      assert.equal(observed!.stoppedSourceKept, true, "cleanup keeps stopped-source latch");
      assert.ok(statuses(claim.run_id).includes("recovery_wait"));
      if (mode === "healthy-B") {
        assert.ok(settlements > 0, "real final settlement was called");
        assert.equal(reapSettlements, 0, "held final settlement needs no second reap");
      } else {
        assert.equal(settlements, 0, "fallback/deferred recovery never settles");
        assert.equal(reapSettlements, 0, "fallback/deferred recovery never reap-then-settles");
        assert.ok(freeEntries > 0, "real credential-free capture entered");
        assert.ok(credentialLaunches.length > 0, "actual capture Git launches observed");
        assert.ok(credentialLaunches.every(value => !value), "fallback Git never receives forge credentials");
        assert.ok(authDeltas.every(delta => delta.refresh === 0 && delta.release === 0),
          "fallback leaves refresh/release credentials untouched");
      }
    }
  });
}

for (const misuse of ["stale", "foreign-flight"] as const) {
  it(`authenticated capture ${misuse} rejects before source read even with publication disabled`, async t => {
    const lifecycle = new AbortController();
    const rig = codexRig({ lifecycleSignal: lifecycle.signal });
    const claim = gitlabClaim(misuse === "stale" ? 285490 : 285491);
    let probing = false, reads = 0, checked = false;
    let flight: Flight | undefined, options: Record<string, unknown> | undefined;
    type CaptureSeam = Seam & {
      captureHoldContext(candidate: typeof claim, flight: Flight, log: ReturnType<typeof nullLogger>,
        options: Record<string, unknown>): Promise<unknown>;
    };
    const status = git.worktreeStatus.bind(git);
    t.mock.method(git, "worktreeStatus", async (...args: Parameters<typeof status>) => {
      if (probing) reads++;
      return status(...args);
    });
    const executor = new FakeCodexExecutor(rig.safety, async ctx => {
      ctx.terminalLifecycleSignal!.addEventListener("abort",
        () => lifecycle.abort(ctx.terminalLifecycleSignal!.reason), { once: true });
      fs.writeFileSync(path.join(ctx.worktreePath, "PROOF.txt"), "real proof bytes\n");
      await ctx.enterCompletionHold!("proof lifetime");
      return { branch: ctx.branch, completionHeld: { reason: "proof lifetime" } };
    }, undefined, rig.capture);
    api.setCompletionHoldResponse("paused");
    const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab);
    const seam = runner as unknown as CaptureSeam;
    const sink = seam.runGatedSink.bind(runner);
    t.mock.method(seam, "runGatedSink", async <T>(actual: Flight, action: () => Promise<T>) => {
      flight = actual;
      return sink(actual, action);
    });
    const capture = seam.captureHoldContext.bind(runner);
    t.mock.method(seam, "captureHoldContext", async (candidate: typeof claim, actual: Flight,
      log: ReturnType<typeof nullLogger>, supplied: Record<string, unknown> = {}) => {
      if (supplied.heldAuthenticated && !checked) {
        checked = true;
        options = { ...supplied, publishCheckpoint: false };
        assert.equal(actual, flight, "held callback belongs to actual intercepted flight");
        if (misuse === "foreign-flight") {
          probing = true;
          try {
            await assert.rejects(() => capture(candidate, { ...actual }, log, options!),
              /capture|scope|flight|proof|authenticated/i);
          } finally { probing = false; }
        }
      }
      return capture(candidate, actual, log, supplied);
    });
    await runner.execute(claim);
    assert.equal(checked, true, "actual held callback supplied authenticated capability");
    assert.ok(flight);
    assert.ok(options);
    if (misuse === "stale") {
      probing = true;
      try {
        await assert.rejects(() => capture(claim, flight!, nullLogger(), options!),
          /capture|scope|flight|proof|authenticated/i);
      } finally { probing = false; }
    }
    assert.equal(reads, 0, "invalid capability refuses before the sensitive status read");
  });
}
