import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { StaleClaimError } from "../src/runner.js";
import type { Executor } from "../src/executor.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { carriesForgeCredential } from "../src/residue-quarantine.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, FakeCodexExecutor, GIT_ENV, spyPublishLands, statuses, manualDeadline } from "./runner-codex-sinks-fixture.js";

installHarness();

it("canonical fence action failure wrapped by poison never grants fallback", async t => {
  const rig = codexRig();
  let armed = false, fallbacks = 0, refused = false;
  const read = git.worktreeStatus.bind(git);
  t.mock.method(git, "worktreeStatus", async (...args: Parameters<typeof read>) => {
    if (armed) {
      rig.registry.poison({ category: "tool", message: "fixture cleanup poison" });
      throw new StaleClaimError();
    }
    return read(...args);
  });
  const executor = new FakeCodexExecutor(rig.safety, async ctx => {
    armed = true;
    try { await ctx.enterCompletionHold!("fenced capture"); }
    catch { refused = true; }
    finally { armed = false; }
    return { branch: ctx.branch, completionHeld: { reason: "fenced capture" } };
  }, undefined, (ms, action, purpose, signal, attempt) => {
    fallbacks++;
    return rig.capture(ms, action, purpose, signal, attempt);
  });
  const { gitlab } = fakeGitlab();
  await runnerWith(() => ({ executor }), gitlab).execute(gitlabClaim(285260));
  assert.equal(refused, true);
  assert.equal(fallbacks, 0);
});

it("independent A credential deferral retains its configured per-call capture budget", async t => {
  const publication = spyPublishLands();
  t.after(publication);
  const rig = codexRig({ vaultLocked: () => true });
  const budgets: number[] = [];
  const executor = new FakeCodexExecutor(rig.safety, async ctx => {
    fs.writeFileSync(path.join(ctx.worktreePath, "A.txt"), "independent A work\n");
    await rig.safety.withBoundary({ boundary: "shutdown", deadlineMs: 15_000 }, async () => {});
    throw new Error("locked reconcile unexpectedly admitted");
  }, undefined, (ms, action, purpose, signal, attempt) => {
    budgets.push(ms);
    return rig.capture(ms, action, purpose, signal, attempt);
  });
  const { gitlab } = fakeGitlab();
  await runnerWith(() => ({ executor }), gitlab, undefined, undefined,
    { codexBoundaryDeadlineMs: 15_000, recoveryRetryMs: 1 }).execute(gitlabClaim(285261));
  assert.ok(budgets.length > 0);
  assert.ok(budgets.every(ms => ms > 0 && ms <= 15_000), JSON.stringify(budgets));
});

// The public callbacks must discriminate Codex before run() has installed safety.
for (const mode of ["codex-refuse", "codex-free", "claude"] as const) {
  it(`missing safety ${mode}: completion capture requires the appropriate scope`, async t => {
    const rig = mode === "codex-free" ? codexRig() : undefined;
    let sourceReads = 0, holdRequests = 0, callbackResult: boolean | undefined, refused = false, clone = "";
    let armed = false, free = false;
    const requestHold = client.requestCompletionHold.bind(client);
    t.mock.method(client, "requestCompletionHold", async (...args: Parameters<typeof requestHold>) => {
      if (armed) holdRequests++;
      return requestHold(...args);
    });
    const read = git.worktreeStatus.bind(git);
    t.mock.method(git, "worktreeStatus", async (...args: Parameters<typeof read>) => {
      if (armed) {
        sourceReads++;
        if (mode === "codex-free") assert.equal(free, true);
      }
      return read(...args);
    });
    const executor: Executor = {
      sandboxesCommands: mode !== "claude",
      ...(rig ? { withCredentialFreeCapture: ((ms, action, purpose, signal, attempt) =>
        rig.capture(ms, async held => {
          free = true;
          try { return await action(held); } finally { free = false; }
        }, purpose, signal, attempt)) as NonNullable<Executor["withCredentialFreeCapture"]> } : {}),
      run: async ctx => {
        clone = ctx.worktreePath;
        fs.writeFileSync(path.join(clone, "FOUNDATION.txt"), "keep local work\n");
        armed = true;
        try { callbackResult = await ctx.enterCompletionHold!("foundation hold"); }
        catch { refused = true; }
        finally { armed = false; }
        return { branch: ctx.branch, completionHeld: { reason: "foundation hold" } };
      },
    };
    api.setCompletionHoldResponse("paused");
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(285240 + ["codex-refuse", "codex-free", "claude"].indexOf(mode));
    await runnerWith(() => ({ executor }), gitlab).execute(claim);
    if (mode === "codex-refuse") {
      assert.equal(refused, false);
      assert.equal(sourceReads, 0);
      assert.equal(holdRequests, 0, "missing proof never requests a hold with an invented head");
      assert.equal(callbackResult, false, "the public hold callback refuses without verified capture");
    } else {
      assert.equal(refused, false);
      assert.equal(callbackResult, true);
      assert.equal(holdRequests, 1);
      assert.ok(sourceReads > 0);
    }
    assert.ok(fs.existsSync(clone), "the source remains available");
    assert.equal(rig?.refreshCalls() ?? 0, 0, "no authenticated admission without safety");
  });
}

for (const codex of [true, false]) {
  it(`missing safety ${codex ? "Codex" : "Claude"}: outer credential switch preserves harness behavior`, async t => {
    let armed = false, sourceReads = 0, clone = "";
    const read = git.worktreeStatus.bind(git);
    t.mock.method(git, "worktreeStatus", async (...args: Parameters<typeof read>) => {
      if (armed) sourceReads++;
      return read(...args);
    });
    const publication = spyPublishLands();
    t.after(publication);
    const executor: Executor = {
      sandboxesCommands: codex,
      run: async ctx => {
        clone = ctx.worktreePath;
        fs.writeFileSync(path.join(clone, "SWITCH.txt"), "keep switch work\n");
        armed = true;
        throw new CredentialSwitchSignal();
      },
    };
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(codex ? 285250 : 285251);
    api.overrideStateStatus(claim.run_id, "queued");
    await runnerWith(() => ({ executor }), gitlab).execute(claim);
    if (codex) {
      assert.equal(sourceReads, 0);
      assert.ok(!statuses(claim.run_id).includes("credential_switch"));
      assert.ok(!statuses(claim.run_id).includes("credential_switch_failed"));
    } else {
      assert.ok(sourceReads > 0);
      assert.ok(statuses(claim.run_id).includes("credential_switch"));
    }
    assert.ok(fs.existsSync(clone));
  });
}


for (const site of ["hold", "wall", "recovery"] as const) {
  for (const refusal of ["healthy", "reconcile", "poisoned", "reap", "expiry"] as const) {
    it(`whole-region ${site}: ${refusal} captures real bytes under a held proof`, async t => {
      const lifecycle = new AbortController();
      let armed = false, authenticated = false, free = false, reaped = false;
      let permitSignal: AbortSignal | undefined;
      const source: Array<{ name: string; state: string; authenticated: boolean; free: boolean; reaped: boolean }> = [];
      const launches: Array<{ identity: string; credential: boolean }> = [];
      const acknowledgments: Array<{ authenticated: boolean; free: boolean }> = [];
      const deadline = manualDeadline("shutdown");
      let reapCalls = 0;
      const rig = codexRig({
        armDeadline: refusal === "expiry" ? deadline.armDeadline : undefined,
        lifecycleSignal: lifecycle.signal,
        blockReconcile: refusal === "reconcile",
        boundaryProcess: request => {
          if (armed) launches.push({ identity: request.identity, credential: carriesForgeCredential(request.env) });
          return undefined;
        },
      });
      const root = rig.registry.reserveLaunch("provider");
      assert.equal(root.kind, "reserved");
      if (root.kind === "reserved") rig.registry.registerRoot(root.reservation, {
        kind: "provider", reap: async () => {
          reapCalls++;
          if (reapCalls === 1 && refusal === "reap") return { ok: false, error: { category: "tool", message: "first reap incomplete" } };
          if (reapCalls === 1 && refusal === "expiry") deadline.fire();
          reaped = true;
          return { ok: true };
        }, dispose: async () => {},
      });
      const boundary = rig.safety.withBoundary.bind(rig.safety);
      t.mock.method(rig.safety, "withBoundary", async (request: Parameters<typeof boundary>[0], action: Parameters<typeof boundary>[1]) => boundary(request, async permit => {
        authenticated = true;
        permitSignal = permit.signal;
        try { return await action(permit); } finally { authenticated = false; }
      }));
      const capture: typeof rig.capture = (ms, action, purpose, signal, attempt) => rig.capture(ms, async held => {
        free = true;
        try { return await action(held); } finally { free = false; }
      }, purpose, signal, attempt);
      for (const name of ["worktreeStatus", "commitWipMarker", "worktreeHead", "verifyRunnerTrackingCovers", "trackingTip"] as const) {
        const original = git[name].bind(git);
        t.mock.method(git, name, async (...args: any[]) => {
          if (armed) source.push({ name, state: rig.registry.state(), authenticated, free, reaped });
          return Reflect.apply(original, git, args);
        });
      }
      const hold = client.requestCompletionHold.bind(client);
      t.mock.method(client, "requestCompletionHold", async (...args: Parameters<typeof hold>) => {
        acknowledgments.push({ authenticated, free });
        if (authenticated) {
          assert.ok(args[2], "HTTP consumes the scoped capture signal");
          assert.notEqual(args[2], permitSignal, "HTTP does not use the bare parent permit signal");
        }
        return hold(...args);
      });
      const wall = client.reportWallPark.bind(client);
      t.mock.method(client, "reportWallPark", async (...args: Parameters<typeof wall>) => {
        acknowledgments.push({ authenticated, free });
        return wall(...args);
      });
      const state = client.reportState.bind(client);
      t.mock.method(client, "reportState", async (...args: Parameters<typeof state>) => {
        if (args[1].status === "recovery_wait") {
          acknowledgments.push({ authenticated, free });
          if (authenticated) {
            assert.ok(args[2]);
            assert.notEqual(args[2], permitSignal);
          }
        }
        return state(...args);
      });
      const publication = spyPublishLands();
      t.after(publication);
      api.setCompletionHoldResponse("paused");
      api.setWallParkResponse("paused", 200);
      const { gitlab } = fakeGitlab();
      const iid = 285200 + ["hold", "wall", "recovery"].indexOf(site) * 10 + ["healthy", "reconcile", "poisoned", "reap", "expiry"].indexOf(refusal);
      const claim = gitlabClaim(iid, { claim_generation: 3 });
      let clone = "", callbackReached = false;
      const executor = new FakeCodexExecutor(rig.safety, async ctx => {
        clone = ctx.worktreePath;
        if (ctx.terminalLifecycleSignal!.aborted) lifecycle.abort(ctx.terminalLifecycleSignal!.reason);
        else ctx.terminalLifecycleSignal!.addEventListener("abort", () => lifecycle.abort(ctx.terminalLifecycleSignal!.reason), { once: true });
        fs.writeFileSync(path.join(clone, "CAPTURE.txt"), "whole-region local work\n");
        if (refusal === "poisoned") rig.registry.poison({ category: "tool", message: "previous boundary failed" });
        armed = true;
        if (site === "recovery") throw new TransientRecoveryError("fixture recovery");
        if (site === "hold") {
          const held = await ctx.enterCompletionHold!("fixture completion hold");
          callbackReached = held;
          return { branch: ctx.branch, completionHeld: { reason: "fixture completion hold" } };
        }
        const outcome = await ctx.parkForWall!({ completedCount: 0 });
        callbackReached = outcome === "parked";
        return { branch: ctx.branch, walled: { reason: "fixture wall" } };
      }, undefined, capture);
      let executionError: unknown;
      await runnerWith(() => ({ executor }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim)
        .catch(error => { executionError = error; });
      assert.ok(site === "recovery" || callbackReached, "public runner callback completed the park");
      assert.ok(source.length > 3, "healthy capture or refused-primary fallback must actually capture and verify local bytes");
      assert.equal(executionError, undefined, "refusal completes its local-preservation disposition");
      assert.ok(source.every(event => event.reaped && (refusal === "healthy"
        ? event.authenticated && !event.free && event.state === "closed"
        : event.free && !event.authenticated)), JSON.stringify(source));
      assert.equal(acknowledgments.length, 1);
      assert.deepEqual(acknowledgments[0], { authenticated: refusal === "healthy", free: refusal !== "healthy" });
      assert.ok(launches.length > 0, "capture Git used supervised process seam");
      assert.ok(launches.some(launch => launch.identity === "command"), "clone operations use command identity");
      assert.ok(launches.some(launch => launch.identity === "worker_pat"), "bare operations use worker identity");
      if (refusal !== "healthy") assert.ok(launches.every(launch => !launch.credential), "fallback has no forge credentials");
      assert.equal(rig.refreshCalls(), refusal === "poisoned" ? 0 : 1, "only primary admission reconciles");
      assert.ok(!statuses(claim.run_id).includes("failed"));
      if (site === "recovery" && refusal !== "healthy") {
        const messages = api.messages(claim.run_id);
        assert.doesNotMatch(JSON.stringify(messages), /paused for credential recovery|credential refresh outcome|refresh outcome is unknown|vault is locked/i);
        assert.ok(!api.states.some(state => state.runId === claim.run_id && state.body.recovery_cause === "vault_locked"));
      }
      const bare = await git.ensureClone(fx.originPath);
      const bytes = execFileSync("git", ["-C", bare, "show", `refs/uzi-runner/agent/issue-${iid}:CAPTURE.txt`], { env: GIT_ENV, encoding: "utf8" });
      assert.equal(bytes, "whole-region local work\n");
      assert.ok(fs.existsSync(clone), "park keeps the original source");
    });
  }
}
