import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { api, fakeGitlab, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { FakeCodexExecutor } from "./runner-codex-sinks-fixture.js";
import { observe } from "./runner-codex-capture-proof-fixture.js";

installHarness();

for (const site of ["wall", "hold", "recovery"] as const) {
  it(`healthy ${site} holds its live proof through Git primary, full root, upload and ACK`, async t => {
    const lifecycle = new AbortController();
    const proof = observe({ authenticated: true, lifecycleSignal: lifecycle.signal });
    t.after(proof.restore);
    const executor = new FakeCodexExecutor(proof.rig.safety, async ctx => {
      if (ctx.terminalLifecycleSignal!.aborted) lifecycle.abort(ctx.terminalLifecycleSignal!.reason);
      else ctx.terminalLifecycleSignal!.addEventListener("abort", () => lifecycle.abort(ctx.terminalLifecycleSignal!.reason), { once: true });
      fs.writeFileSync(path.join(ctx.worktreePath, "SUPERVISED.txt"), "supervised healthy capture\n");
      proof.arm();
      if (site === "recovery") throw new TransientRecoveryError("supervision fixture");
      if (site === "hold") {
        assert.equal(await ctx.enterCompletionHold!("supervised hold"), true);
        return { branch: ctx.branch, completionHeld: { reason: "supervised hold" } };
      }
      assert.equal(await ctx.parkForWall!({ completedCount: 0 }), "parked");
      return { branch: ctx.branch, walled: { reason: "supervised wall" } };
    }, undefined, proof.capture);
    api.setCompletionHoldResponse("paused");
    api.setWallParkResponse("paused", 200);
    await proof.execute(runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined,
      { recoveryRetryMs: 1 }).execute(gitlabClaim(285500 + ["wall", "hold", "recovery"].indexOf(site))));
    proof.assertProof(true, true);
  });
}
