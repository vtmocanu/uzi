import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { SteeringChannel } from "../src/steering.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { api, fakeGitlab, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, FakeCodexExecutor, statuses } from "./runner-codex-sinks-fixture.js";

installHarness();

for (const site of ["wall", "hold", "recovery"] as const) for (const authority of ["cancel", "fence"] as const) {
  it(`${site}: ${authority} during initial root settlement grants no capture or fallback`, async t => {
    const lifecycle = new AbortController();
    const rig = codexRig({ lifecycleSignal: lifecycle.signal });
    let armed = false, revoked = false, fallbacks = 0, source = 0, clone = "";
    let steering: SteeringChannel | undefined;
    const root = rig.registry.reserveLaunch("provider");
    assert.equal(root.kind, "reserved");
    if (root.kind === "reserved") rig.registry.registerRoot(root.reservation, {
      kind: "provider", reap: async () => {
        if (armed && !revoked) {
          assert.ok(steering);
          revoked = true;
          if (authority === "cancel") steering.markCancelled();
          else t.mock.method(steering, "claimFence", () => "stale");
        }
        return { ok: true };
      }, dispose: async () => {},
    });
    for (const name of ["worktreeStatus", "commitWipMarker", "fetchAgentBranch"] as const) {
      const original = git[name].bind(git);
      t.mock.method(git, name, async (...args: unknown[]) => {
        if (armed) source++;
        return Reflect.apply(original, git, args);
      });
    }
    const executor = new FakeCodexExecutor(rig.safety, async ctx => {
      clone = ctx.worktreePath;
      ctx.terminalLifecycleSignal!.addEventListener("abort", () => lifecycle.abort(ctx.terminalLifecycleSignal!.reason), { once: true });
      fs.writeFileSync(path.join(clone, "AUTHORITY.txt"), "authority stop local bytes\n");
      armed = true;
      if (site === "recovery") throw new TransientRecoveryError("authority fixture");
      if (site === "hold") {
        await ctx.enterCompletionHold!("authority hold");
        return { branch: ctx.branch, completionHeld: { reason: "authority hold" } };
      }
      await ctx.parkForWall!({ completedCount: 0 });
      return { branch: ctx.branch, walled: { reason: "authority wall" } };
    }, undefined, (ms, action, purpose, signal, attempt) => {
      fallbacks++;
      return rig.capture(ms, action, purpose, signal, attempt);
    });
    const run = runnerWith(() => ({ executor }), fakeGitlab().gitlab);
    const internal = run as unknown as { runGatedSink: (flight: { steering: SteeringChannel }, action: () => Promise<unknown>) => Promise<unknown> };
    const gated = internal.runGatedSink.bind(run);
    t.mock.method(internal, "runGatedSink", async (flight: { steering: SteeringChannel }, action: () => Promise<unknown>) => {
      steering = flight.steering;
      return gated(flight, action);
    });
    const claim = gitlabClaim(285510 + ["wall", "hold", "recovery"].indexOf(site) * 2 + Number(authority === "fence"));
    api.setCompletionHoldResponse("paused");
    api.setWallParkResponse("paused", 200);
    let executionError: unknown;
    await run.execute(claim).catch(error => { executionError = error; });
    assert.equal(executionError, undefined, "revoked capture unwinds into the canonical control disposition");
    assert.equal(revoked, true, "real registered root settlement crossed the authority boundary");
    assert.equal(source, 0, "revocation precedes source inspection and WIP/import");
    assert.equal(fallbacks, 0, "revocation never authorizes A fallback");
    assert.equal(api.completionHoldRequests.length, 0);
    assert.equal(api.wallParkRequests.length, 0);
    assert.ok(!statuses(claim.run_id).includes("recovery_wait"));
    assert.equal(fs.readFileSync(path.join(clone, "AUTHORITY.txt"), "utf8"), "authority stop local bytes\n");
  });
}
