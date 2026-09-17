import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { installHarness, runnerWith, gitlabClaim, fakeGitlab, deferred } from "./runner-harness.js";
import type { RunContext, ExecutorResult } from "../src/executor.js";
import type { ExecutorFactory } from "../src/runner.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { ActiveSnapshotPhase } from "../src/protocol.js";

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
