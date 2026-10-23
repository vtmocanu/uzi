import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { api, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { FakeCodexExecutor, GIT_ENV } from "./runner-codex-sinks-fixture.js";
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

// The native ChildProcess seam sees both spawn and execFile (including the
// promisified execFile captured by GitCache at module initialization). Counting
// only facade requests would miss a new child that silently uses the legacy path.
for (const site of ["wall", "hold", "recovery"] as const) for (const fallback of [false, true]) {
  it(`${site} ${fallback ? "fallback" : "healthy"}: every native capture child consumes a scoped launch`, async t => {
    const { ChildProcess } = await import("node:child_process");
    const { codexRig } = await import("./runner-codex-sinks-fixture.js");
    type NativeOptions = { file: string; args: string[]; cwd?: string };
    const prototype = ChildProcess.prototype as unknown as { spawn(options: NativeOptions): number };
    const native = prototype.spawn;
    const tickets = new Map<string, number>();
    const unowned: string[] = [];
    let armed = false, depth = 0, launches = 0, clone = "";
    const key = (file: string, args: readonly string[], cwd?: string) => JSON.stringify([file, args, String(cwd ?? "")]);
    const rig = codexRig({ blockReconcile: fallback, boundaryProcess: request => {
      if (armed && depth > 0) {
        const requestKey = key(request.argv[0]!, request.argv.slice(1), request.cwd);
        tickets.set(requestKey, (tickets.get(requestKey) ?? 0) + 1);
      }
      return undefined;
    } });
    t.mock.method(prototype, "spawn", function(this: InstanceType<typeof ChildProcess>, options: NativeOptions) {
      if (armed && depth > 0) {
        launches++;
        const requestKey = key(options.file, options.args.slice(1), options.cwd);
        const remaining = tickets.get(requestKey) ?? 0;
        if (remaining === 0) {
          unowned.push(path.basename(options.file));
        }
        else if (remaining === 1) tickets.delete(requestKey);
        else tickets.set(requestKey, remaining - 1);
      }
      return Reflect.apply(native, this, [options]);
    });
    const executor = new FakeCodexExecutor(rig.safety, async ctx => {
      clone = ctx.worktreePath;
      fs.writeFileSync(path.join(clone, "ALL-CHILDREN.txt"), "all capture launches supervised\n");
      armed = true;
      if (site === "recovery") throw new TransientRecoveryError("native seam capture");
      if (site === "hold") {
        await ctx.enterCompletionHold!("native seam hold");
        return { branch: ctx.branch, completionHeld: { reason: "native seam hold" } };
      }
      await ctx.parkForWall!({ completedCount: 0 });
      return { branch: ctx.branch, walled: { reason: "native seam wall" } };
    }, undefined, rig.capture);
    api.setCompletionHoldResponse("paused");
    api.setWallParkResponse("paused", 200);
    const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { recoveryRetryMs: 1 });
    const helper = site === "wall" ? "enterWallPark" : site === "hold" ? "enterCompletionHold" : "handleRecoveryExhausted";
    const internal = runner as unknown as Record<typeof helper, (...args: unknown[]) => Promise<unknown>>;
    const original = internal[helper].bind(runner);
    // Scope the independent native observation to the actual B capture/disposition
    // helper, including its admission, records and tail. Unrelated execute-result
    // owed-candidate bookkeeping has its own policy and is outside this interval.
    t.mock.method(internal, helper, async (...args: unknown[]) => {
      depth++;
      try { return await original(...args); } finally { depth--; }
    });
    const iid = 285530 + ["wall", "hold", "recovery"].indexOf(site) * 2 + Number(fallback);
    const claim = gitlabClaim(iid);
    await runner.execute(claim);
    if (site === "hold") assert.ok(api.completionHoldRequests[0]?.body.head, "real verified hold completed");
    else if (site === "wall") assert.ok(api.wallParkRequests[0]?.body.head, "real verified wall park completed");
    else assert.ok(api.states.some(state => state.runId === claim.run_id && state.body.status === "recovery_wait"));
    assert.equal(execFileSync("git", ["-C", git.barePathFor(fx.originPath), "show", `refs/uzi-runner/agent/issue-${iid}:ALL-CHILDREN.txt`],
      { env: GIT_ENV, encoding: "utf8" }), "all capture launches supervised\n", "the independently observed children produced the actual local capture");
    assert.ok(launches > 0, "native child creation was observed independently of facade calls");
    assert.equal(unowned.length, 0, `first child bypassing its scoped spawner: ${unowned[0] ?? "none"}`);
    assert.equal(tickets.size, 0, "each scoped Git launch reached exactly one native child");
    assert.equal(fs.readFileSync(path.join(clone, "ALL-CHILDREN.txt"), "utf8"), "all capture launches supervised\n");
  });
}
