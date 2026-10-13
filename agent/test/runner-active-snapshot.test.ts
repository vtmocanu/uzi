import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { api, git, installHarness, runnerWith, gitlabClaim, fakeGitlab, deferred } from "./runner-harness.js";
import type { RunContext, ExecutorResult } from "../src/executor.js";
import type { ExecutorFactory } from "../src/runner.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { ActiveSnapshotPhase } from "../src/protocol.js";
import { mockCommittedTracking } from "./runner-tracking-fixture.js";

// PRD #1390 M2a — the run lane's wiring into the shared active-run registry: a run is
// registered at `running` (with its claim generation) while it executes, its phase is
// advanced through the single reportState choke point, and it is removed on terminal.

/** Records every setPhase call so a test can prove the reportState choke point (not just
 *  the initial add()) drives the registry. `add()` never calls setPhase, so any recorded
 *  entry came from a state report going through the choke point. */
class RecordingRegistry extends ActiveRunRegistry {
  readonly phaseLog: Array<{ runId: string; phase: ActiveSnapshotPhase }> = [];
  override setPhase(runId: string, phase: ActiveSnapshotPhase): void {
    this.phaseLog.push({ runId, phase });
    super.setPhase(runId, phase);
  }
}

installHarness();

describe("runner active-run snapshot registry (PRD #1390 M2a)", () => {
  it("unresolved completion hold settles execute and removes both live registries", async () => {
    const { gitlab, all } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-hold-registry-"));
    const started = deferred();
    const gate = deferred();
    const registry = new ActiveRunRegistry();
    let cancelSignal: AbortSignal | undefined;
    const claim = gitlabClaim(2817, { claim_generation: 7 });
    api.setCompletionHoldResponse("", 200);
    api.committedHoldOwnership.set(claim.run_id, {
      status: "running", claim_generation: 7, hold_reason: null, claim_released: false,
    });
    const runner = runnerWith(() => ({
      homeDir: homeRoot,
      executor: {
        run: async (ctx) => {
          started.resolve();
          await gate.promise;
          git.worktreeStatus = async () => [];
          mockCommittedTracking(git, "cafef00dcafef00dcafef00dcafef00dcafef00d");
          git.verifyRunnerTrackingCovers = async () => true;
          git.trackingTip = async () => "cafef00dcafef00dcafef00dcafef00dcafef00d";
          git.checkpointPack = async () => null;
          cancelSignal = ctx.signal;
          assert.equal(await ctx.enterCompletionHold!("ambiguous registry hold"), true);
          return { branch: ctx.branch, completionHeld: { reason: "ambiguous registry hold" } };
        },
      },
    }), gitlab, undefined, undefined, { activeRuns: registry, recoveryRetryMs: 1,
      completionHoldReconcileTimeoutMs: 5_000 });
    const executing = runner.execute(claim);
    try {
      await started.promise;
      assert.equal(runner.isExecuting(claim.run_id), true);
      assert.equal(registry.size, 1);
      gate.resolve();
      await executing;
      assert.equal(runner.isExecuting(claim.run_id), false);
      assert.equal(registry.size, 0);
      assert.deepEqual(registry.build().active, []);
      assert.deepEqual(api.ownershipRequests, [claim.run_id]);
      assert.ok(!api.states.some((s) => s.runId === claim.run_id &&
        ["completed", "failed"].includes(s.body.status)));
      assert.deepEqual(all, []);
      assert.equal(cancelSignal?.aborted, false);
      runner.shutdown();
      assert.equal(cancelSignal?.aborted, false, "shutdown has no stale active flight to abort");
    } finally {
      gate.resolve();
      await executing;
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("lists an executing run at `running` (with its claim generation), then removes it on terminal", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1390-snap-"));
    try {
      const started = deferred();
      const gate = deferred();
      const factory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeRoot, runId),
        executor: {
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            started.resolve();
            await gate.promise; // hold the run live so the snapshot can be observed mid-flight
            return { branch: ctx.branch };
          },
        },
      });
      const reg = new RecordingRegistry();
      const runner = runnerWith(factory, gitlab, undefined, undefined, { activeRuns: reg });
      const claim = gitlabClaim(300, { wait_on_limit: true, claim_generation: 9 });
      const p = runner.execute(claim);

      // The run is executing (blocked inside run()): the registry lists it.
      await started.promise;
      const snap = reg.build();
      assert.strictEqual(snap.active.length, 1, "the executing run is listed");
      assert.strictEqual(snap.active[0]!.run_id, claim.run_id);
      assert.strictEqual(snap.active[0]!.phase, "running");
      assert.strictEqual(snap.active[0]!.claim_generation, 9, "the generation it was claimed at");
      assert.strictEqual(snap.active[0]!.terminal_pending, false, "a #1390 worker journals no terminal outcome");
      assert.strictEqual(snap.pending_overflow, false, "a #1390 worker never overflows");
      // The reportState choke point drove at least one `running` phase update (the first
      // `running` report from phaseClone) — add() never calls setPhase, so this proves the
      // choke-point wiring, not just registration.
      assert.ok(
        reg.phaseLog.some((e) => e.runId === claim.run_id && e.phase === "running"),
        "the reportState choke point advances the run's phase in the registry",
      );

      gate.resolve();
      await p;
      assert.strictEqual(reg.size, 0, "the run is removed from the registry on terminal");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});
