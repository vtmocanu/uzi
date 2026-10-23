import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync, spawn } from "node:child_process";
import { CodexBoundaryError, acceptsHeldCaptureCleanup } from "../src/codex/safety.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, commitInTree, FakeCodexExecutor, GIT_ENV, manualDeadline } from "./runner-codex-sinks-fixture.js";
import { nullLogger } from "./helpers.js";

installHarness();

const bytes = "verified capture survives optional publication\n";
const readGit = (tree: string, ...args: string[]) =>
  execFileSync("git", ["-C", tree, ...args], { env: GIT_ENV, encoding: "utf8" });

it("no-held capture with publication disabled acquires before its first source read", async t => {
  const rig = codexRig();
  let direct = false, admitted = false, checked = false, completed = false, reads = 0;
  const boundary = rig.safety.withBoundary.bind(rig.safety);
  t.mock.method(rig.safety, "withBoundary", async (request: Parameters<typeof boundary>[0], action: Parameters<typeof boundary>[1]) =>
    boundary(request, async permit => {
      admitted = true;
      try { return await action(permit); } finally { admitted = false; }
    }));
  const status = git.worktreeStatus.bind(git);
  t.mock.method(git, "worktreeStatus", async (...args: Parameters<typeof status>) => {
    if (direct) {
      reads++;
      assert.equal(admitted, true, "publication=false must not bypass capture admission");
      assert.equal(rig.registry.state(), "closed", "the writer registry is closed before reading");
    }
    return status(...args);
  });
  const claim = gitlabClaim(285370);
  const executor = new FakeCodexExecutor(rig.safety, async ctx => {
    fs.writeFileSync(path.join(ctx.worktreePath, "RESULT.txt"), bytes);
    assert.equal(await ctx.enterCompletionHold!("direct capture control"), true);
    return { branch: ctx.branch, completionHeld: { reason: "direct capture control" } };
  }, undefined, rig.capture);
  const { gitlab } = fakeGitlab();
  const runner = runnerWith(() => ({ executor }), gitlab);
  const internal = runner as unknown as {
    runGatedSink: (flight: unknown, action: () => Promise<unknown>) => Promise<unknown>;
    captureHoldContext: (candidate: typeof claim, flight: unknown, log: ReturnType<typeof nullLogger>, options: { publishCheckpoint: false }) =>
      Promise<{ verified: boolean; published: boolean; head: string | null }>;
  };
  const gated = internal.runGatedSink.bind(runner);
  t.mock.method(internal, "runGatedSink", async (flight: unknown, action: () => Promise<unknown>) => {
    if (!checked) {
      checked = true;
      direct = true;
      try {
        const result = await internal.captureHoldContext(claim, flight, nullLogger(), { publishCheckpoint: false });
        assert.equal(result.verified, true);
        assert.equal(result.published, false);
        const bare = git.barePathFor(fx.originPath);
        assert.equal(result.head, readGit(bare, "rev-parse", "refs/uzi-runner/agent/issue-285370").trim());
        assert.equal(readGit(bare, "show", result.head + ":RESULT.txt"), bytes);
        completed = true;
      } finally { direct = false; }
    }
    return gated(flight, action);
  });
  api.setCompletionHoldResponse("paused");
  await runner.execute(claim);
  assert.equal(checked, true, "the real flight reached the intercepted sink");
  assert.equal(completed, true, "the direct capture assertions completed, rather than being swallowed by runner failure dispatch");
  assert.equal(reads, 1, "the direct capture read actual source");
});

for (const site of ["wall", "hold", "recovery"] as const) {
  for (const interruption of ["expiry", "typed-deferral"] as const) {
    it(`${site}: post-verification ${interruption} keeps local capture without readmission`, async t => {
      let now = Date.now(), armed = false, injected = 0, fallbacks = 0, verifies = 0, fetches = 0, markers = 0;
      let clone = "", verifiedHead = "";
      const deadline = manualDeadline("shutdown");
      const rig = codexRig({ now: () => now, armDeadline: deadline.armDeadline });
      const verify = git.verifyRunnerTrackingCovers.bind(git);
      t.mock.method(git, "verifyRunnerTrackingCovers", async (...args: Parameters<typeof verify>) => {
        const result = await verify(...args);
        if (armed) {
          verifies++;
          assert.equal(result, true);
          verifiedHead = readGit(git.barePathFor(fx.originPath), "rev-parse", `refs/uzi-runner/agent/issue-${iid}`).trim();
        }
        return result;
      });
      const fetch = git.fetchAgentBranch.bind(git);
      t.mock.method(git, "fetchAgentBranch", async (...args: Parameters<typeof fetch>) => {
        if (armed) fetches++;
        return fetch(...args);
      });
      const marker = git.commitWipMarker.bind(git);
      t.mock.method(git, "commitWipMarker", async (...args: Parameters<typeof marker>) => {
        if (armed) markers++;
        return marker(...args);
      });
      const executor = new FakeCodexExecutor(rig.safety, async ctx => {
        clone = ctx.worktreePath;
        fs.writeFileSync(path.join(clone, "RESULT.txt"), bytes);
        armed = true;
        if (site === "recovery") throw new TransientRecoveryError("post-local publication fixture");
        if (site === "hold") {
          assert.equal(await ctx.enterCompletionHold!("post-local hold"), true);
          return { branch: ctx.branch, completionHeld: { reason: "post-local hold" } };
        }
        assert.equal(await ctx.parkForWall!({ completedCount: 0 }), "parked");
        return { branch: ctx.branch, walled: { reason: "post-local wall" } };
      }, undefined, (ms, action, purpose, signal, attempt) => {
        fallbacks++;
        return rig.capture(ms, action, purpose, signal, attempt);
      });
      const { gitlab } = fakeGitlab();
      const runner = runnerWith(() => ({ executor }), gitlab, undefined, undefined, { recoveryRetryMs: 1 });
      const internal = runner as unknown as { buildCheckpointOverlay: (...args: unknown[]) => Promise<unknown> };
      t.mock.method(internal, "buildCheckpointOverlay", async () => {
        injected++;
        assert.equal(verifies, 1, "publication begins only after positive local verification");
        assert.ok(verifiedHead);
        if (interruption === "expiry") {
          now += 120_001;
          t.mock.method(Date, "now", () => now);
          deadline.fire();
          throw new Error("publication phase expired after local verification");
        }
        throw new CodexBoundaryError("reconcile", [], undefined, "vault_locked");
      });
      let uploads = 0;
      t.mock.method(client, "publishCheckpoint", async () => { uploads++; throw new Error("overlay interruption must precede upload"); });
      api.setCompletionHoldResponse("paused");
      api.setWallParkResponse("paused", 200);
      const iid = 285371 + ["wall", "hold", "recovery"].indexOf(site) * 2 + Number(interruption === "typed-deferral");
      const claim = gitlabClaim(iid);
      await runner.execute(claim);
      assert.equal(injected, 1);
      assert.equal(verifies, 1, "verified local result is not recaptured");
      assert.equal(fetches, 1);
      assert.equal(markers, 1);
      assert.equal(fallbacks, 0, "optional publication cannot buy another capture");
      assert.equal(rig.refreshCalls(), 1, "optional publication cannot refresh again");
      assert.equal(uploads, 0);
      assert.equal(deadline.fired(), interruption === "expiry" ? 1 : 0);
      const bare = git.barePathFor(fx.originPath);
      assert.equal(readGit(bare, "show", `refs/uzi-runner/agent/issue-${iid}:RESULT.txt`), bytes);
      assert.equal(readGit(clone, "rev-parse", "HEAD").trim(), verifiedHead);
      assert.equal(fs.readFileSync(path.join(clone, "RESULT.txt"), "utf8"), bytes);
      if (site === "hold") {
        assert.equal(api.completionHoldRequests.length, 1, "publication expiry must retain the verified head in the hold report");
        assert.equal(api.completionHoldRequests[0]!.body.head, verifiedHead);
      } else if (site === "wall") {
        assert.equal(api.wallParkRequests.length, 1, "publication expiry must retain the verified head in the wall report");
        assert.equal(api.wallParkRequests[0]!.body.head, verifiedHead);
        assert.equal(api.wallParkRequests[0]!.body.published, false);
      } else {
        assert.ok(api.states.some(state => state.runId === claim.run_id && state.body.status === "recovery_wait"),
          "recovery acknowledges its verified local restore point");
      }
    });
  }
}

for (const site of ["wall", "hold", "recovery", "permit", "prepush"] as const) {
  for (const unsafe of [false, true]) {
    it(`${site}: ${unsafe ? "unsafe" : "safe"} publication root cleanup gates capture disposition`, async t => {
      const finalize = site === "permit" || site === "prepush";
      let publication = false, injected = 0, verifies = 0, freeCalls = 0, joined = false, sourceRemovals = 0;
      let clone = "", verifiedHead = "";
      const iid = 285900 + ["wall", "hold", "recovery", "permit", "prepush"].indexOf(site) * 2 + Number(unsafe);
      const rig = codexRig({ boundaryProcess: async request => {
        if (!publication || injected) return undefined;
        injected++;
        const [bin, ...args] = request.argv;
        const child = spawn(bin!, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
        const terminal = new Promise<{ code: number }>((resolve, reject) => {
          child.once("error", reject);
          child.once("close", code => resolve({ code: code ?? 1 }));
        });
        void terminal.catch(() => undefined);
        return {
          stdin: child.stdin, stdout: child.stdout, stderr: child.stderr,
          waitChild: () => terminal,
          root: {
            kind: "boundary_action",
            reap: async () => {
              await terminal;
              return unsafe ? { ok: false, error: { category: "tool", message: "publication full-root cleanup refused" } } : { ok: true };
            },
            dispose: async () => {
              if (child.exitCode === null) child.kill("SIGKILL");
              await terminal.catch(() => undefined);
            },
          },
        };
      }});
      const remove = git.removeRunnerClone.bind(git);
      t.mock.method(git, "removeRunnerClone", async (...args: Parameters<typeof remove>) => {
        if (args[0] === clone) sourceRemovals++;
        return remove(...args);
      });
      const boundary = rig.safety.withBoundary.bind(rig.safety);
      t.mock.method(rig.safety, "withBoundary", async (...args: Parameters<typeof boundary>) => {
        try { return await boundary(...args); } finally { joined = true; }
      });
      const verify = git.verifyRunnerTrackingCovers.bind(git);
      t.mock.method(git, "verifyRunnerTrackingCovers", async (...args: Parameters<typeof verify>) => {
        const result = await verify(...args);
        if (result) {
          verifies++;
          verifiedHead = readGit(git.barePathFor(fx.originPath), "rev-parse", `refs/uzi-runner/agent/issue-${iid}`).trim();
        }
        return result;
      });
      const executor = new FakeCodexExecutor(rig.safety, async ctx => {
        clone = ctx.worktreePath;
        if (finalize) commitInTree(clone, "RESULT.txt", bytes);
        else fs.writeFileSync(path.join(clone, "RESULT.txt"), bytes);
        if (site === "recovery") throw new TransientRecoveryError("publication cleanup fixture");
        if (site === "hold") {
          await ctx.enterCompletionHold!("publication cleanup");
          return { branch: ctx.branch, completionHeld: { reason: "publication cleanup" } };
        }
        if (site === "wall") {
          await ctx.parkForWall!({ completedCount: 0 });
          return { branch: ctx.branch, walled: { reason: "publication cleanup" } };
        }
        return { branch: ctx.branch, ...(site === "prepush" ? { nonClosing: { reason: "remaining work" } } : {}) };
      }, undefined, async () => {
        freeCalls++;
        assert.equal(joined, true, "fresh fallback must wait for primary settlement");
        return { kind: "incomplete", errors: [{ category: "tool", message: "no fresh empty proof" }] };
      });
      const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { recoveryRetryMs: 1 });
      const internal = runner as unknown as {
        buildCheckpointOverlay: (...args: unknown[]) => Promise<unknown>;
        captureHoldContext: (...args: unknown[]) => Promise<unknown>;
      };
      const publicationChild = async () => {
        publication = true;
        await git.trackingTip(git.barePathFor(fx.originPath), `agent/issue-${iid}`);
      };
      if (finalize) {
        // Both finalize callers disable publication. Exercise their held continuation after
        // local proof with the same real child cleanup refusal, without enabling publication.
        const capture = internal.captureHoldContext.bind(runner);
        t.mock.method(internal, "captureHoldContext", async (...args: unknown[]) => {
          const result = await capture(...args);
          await publicationChild();
          return result;
        });
        if (site === "prepush") t.mock.method(git, "publishedCommitMessages", async () => null);
        api.setRegisterProtocolFeatures(["claim_generation_fence", "completion_non_closing_v1"]);
        await client.register("cleanup-finalize");
      } else {
        t.mock.method(internal, "buildCheckpointOverlay", async () => {
          await publicationChild();
          throw new Error("ordinary optional publication failure after safe cleanup");
        });
      }
      api.setCompletionHoldResponse("paused");
      api.setWallParkResponse("paused", 200);
      const claim = gitlabClaim(iid, finalize ? {
        claim_generation: 7, config: { completion_contract_version: 1, contract_revision: 1 },
      } : {});
      if (finalize) {
        api.setPublicationState(claim.run_id, [], 1);
        api.setCompletionPermitResponse(false, { denyReason: "incomplete" });
      }
      await runner.execute(claim);
      assert.equal(injected, 1, "a real Git child completed in the tested scope");
      assert.equal(verifies, 1, "no reuse or recapture of invalidated provisional evidence");
      assert.equal(rig.refreshCalls(), 1, "no second credential refresh");
      assert.ok(verifiedHead);
      assert.equal(readGit(git.barePathFor(fx.originPath), "show", verifiedHead + ":RESULT.txt"), bytes);
      assert.equal(readGit(clone, "rev-parse", "HEAD").trim(), verifiedHead);
      assert.equal(fs.readFileSync(path.join(clone, "RESULT.txt"), "utf8"), bytes, "original source remains on this worker");
      if (site === "wall") {
        assert.equal(api.wallParkRequests.length, unsafe ? 0 : 1);
        if (!unsafe) assert.equal(api.wallParkRequests[0]!.body.head, verifiedHead);
      } else if (site === "recovery") {
        assert.equal(api.states.filter(state => state.runId === claim.run_id && state.body.status === "recovery_wait").length, unsafe ? 0 : 1);
      } else {
        assert.equal(api.completionHoldRequests.length, unsafe ? 0 : 1, "unsafe cleanup cannot authorize a hold ACK, even with degraded capture allowed");
        if (!unsafe) assert.equal(api.completionHoldRequests[0]!.body.head, verifiedHead);
      }
      assert.equal(freeCalls, unsafe ? 1 : 0, "only a fresh proof can replace invalidated evidence");
      assert.equal(sourceRemovals, 0, "capture refusal never deletes the original source");
    });
  }
}

for (const expired of [false, true]) {
  it(`recovery: uncertain park delivery ${expired ? "stops after expiry" : "retries inside live scope"}`, async t => {
    let now = Date.now(), reports = 0, clone = "", verifies = 0;
    const deadline = manualDeadline("shutdown");
    const rig = codexRig({ now: () => now, armDeadline: deadline.armDeadline });
    const verify = git.verifyRunnerTrackingCovers.bind(git);
    t.mock.method(git, "verifyRunnerTrackingCovers", async (...args: Parameters<typeof verify>) => {
      const result = await verify(...args);
      if (result) verifies++;
      return result;
    });
    const executor = new FakeCodexExecutor(rig.safety, async ctx => {
      clone = ctx.worktreePath;
      fs.writeFileSync(path.join(clone, "RESULT.txt"), bytes);
      throw new TransientRecoveryError("uncertain park fixture");
    }, undefined, rig.capture);
    const report = client.reportState.bind(client);
    t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
      if (args[1].status !== "recovery_wait") return report(...args);
      reports++;
      if (reports === 1) {
        if (expired) {
          now += 120_001;
          t.mock.method(Date, "now", () => now);
          deadline.fire();
        }
        throw new Error("unknown park transport outcome");
      }
      return report(...args);
    });
    const iid = 285920 + Number(expired);
    const claim = gitlabClaim(iid);
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);
    assert.equal(deadline.fired(), expired ? 1 : 0);
    assert.equal(reports, expired ? 1 : 2, "an uncertain disposition cannot replay after its parent scope joins");
    assert.equal(api.states.filter(state => state.runId === claim.run_id && state.body.status === "recovery_wait").length, expired ? 0 : 1,
      "a sent request and an acknowledged park are distinct");
    assert.equal(verifies, 1, "retrying delivery does not recapture source");
    assert.equal(fs.readFileSync(path.join(clone, "RESULT.txt"), "utf8"), bytes);
    assert.equal(readGit(git.barePathFor(fx.originPath), "show", `refs/uzi-runner/agent/issue-${iid}:RESULT.txt`), bytes);
  });
}

for (const reply of ["park", "provider-hold", "untyped-provider-hold", "wrong-hold", "running", "statusless", "stale-generation", "cancelled", "shutdown"] as const) {
  it(`recovery: confirmed reply at healthy expiry preserves ${reply} disposition`, async t => {
    let now = Date.now(), reports = 0, verifies = 0, settlementsAfterReply = 0;
    let replied = false, clone = "", parked = 0;
    const deadline = manualDeadline("shutdown");
    const rig = codexRig({ now: () => now, armDeadline: deadline.armDeadline });
    const verify = git.verifyRunnerTrackingCovers.bind(git);
    t.mock.method(git, "verifyRunnerTrackingCovers", async (...args: Parameters<typeof verify>) => {
      const result = await verify(...args);
      if (result) verifies++;
      return result;
    });
    const provider = reply === "provider-hold" || reply === "wrong-hold";
    const executor = new FakeCodexExecutor(rig.safety, async ctx => {
      clone = ctx.worktreePath;
      fs.writeFileSync(path.join(clone, "RESULT.txt"), bytes);
      throw new TransientRecoveryError("received park reply fixture", { providerOrigin: provider });
    }, undefined, rig.capture);
    const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { recoveryRetryMs: 1 });
    const internal = runner as unknown as {
      handleRecoveryExhausted: (...args: unknown[]) => Promise<boolean>;
      settleRecoveryGeneration: (...args: unknown[]) => Promise<unknown>;
      reapThenSettleRecoveryGeneration: (...args: unknown[]) => Promise<unknown>;
    };
    const recover = internal.handleRecoveryExhausted.bind(runner);
    t.mock.method(internal, "handleRecoveryExhausted", async (...args: unknown[]) => {
      const result = await recover(...args);
      if (result) parked++;
      return result;
    });
    for (const method of ["settleRecoveryGeneration", "reapThenSettleRecoveryGeneration"] as const) {
      const settle = internal[method].bind(runner);
      t.mock.method(internal, method, async (...args: unknown[]) => {
        if (replied) settlementsAfterReply++;
        return settle(...args);
      });
    }
    api.setRegisterProtocolFeatures(["claim_generation_fence", "recovery_cause_provider_outage"]);
    await client.register("received-park-reply");
    const report = client.reportState.bind(client);
    t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
      if (args[1].status !== "recovery_wait") return report(...args);
      reports++;
      assert.equal(args[1].claim_generation, 7, "the flight reporter binds the request generation");
      assert.equal(args[1].recovery_cause === "provider_outage", provider);
      const ack = await report(...args);
      replied = true;
      now += 120_001;
      t.mock.method(Date, "now", () => now);
      deadline.fire();
      if (reply === "shutdown") runner.shutdown();
      if (reply === "stale-generation") return { ...ack, status: "recovery_wait", staleClaim: true, applied: false };
      if (reply === "cancelled") return { ...ack, status: "cancelled" };
      if (reply === "running") return { ...ack, status: "running" };
      if (reply === "statusless") return { applied: false };
      if (reply === "provider-hold" || reply === "untyped-provider-hold" || reply === "wrong-hold")
        return { ...ack, status: "paused", holdReason: reply === "wrong-hold" ? "budget_exhausted" : "provider_unavailable" };
      return ack;
    });
    const iid = 285940 + ["park", "provider-hold", "untyped-provider-hold", "wrong-hold", "running", "statusless", "stale-generation", "cancelled", "shutdown"].indexOf(reply);
    const execution = runner.execute(gitlabClaim(iid, { claim_generation: 7 }));
    if (reply === "shutdown") await assert.rejects(execution, { name: "AbortError" });
    else if (reply === "stale-generation") await assert.rejects(execution, { name: "StaleClaimError" });
    else await execution;
    assert.equal(reports, 1, "received or refused replies never cause a second POST after join");
    assert.equal(deadline.fired(), 1);
    assert.equal(parked, reply === "park" || reply === "provider-hold" ? 1 : 0,
      "only an accepted park or typed provider hold survives expiry; lifecycle authority still wins");
    assert.equal(settlementsAfterReply, 0, "no source settlement runs after scoped expiry");
    assert.equal(verifies, 1);
    assert.equal(rig.refreshCalls(), 1);
    assert.equal(fs.readFileSync(path.join(clone, "RESULT.txt"), "utf8"), bytes);
    assert.equal(readGit(git.barePathFor(fx.originPath), "show", `refs/uzi-runner/agent/issue-${iid}:RESULT.txt`), bytes);
  });
}

for (const site of ["wall", "hold", "recovery", "permit", "prepush"] as const) {
  for (const fault of ["raw-tool", "raw-tool-expiry", "raw-tool-late-expiry", "clean-child-deadline"] as const) {
    const unsafe = fault !== "clean-child-deadline";
    it(`${site}: facade cleanup witness rejects unknown poison and preserves ${fault}`, async t => {
      const finalize = site === "permit" || site === "prepush";
      let publication = false, injected = 0, verifies = 0, freeCalls = 0, joined = false, sourceRemovals = 0;
      let clone = "", verifiedHead = "";
      const iid = 286100 + ["wall", "hold", "recovery", "permit", "prepush"].indexOf(site) * 4 + ["raw-tool", "raw-tool-expiry", "raw-tool-late-expiry", "clean-child-deadline"].indexOf(fault);
      const deadline = manualDeadline(finalize ? "finalize" : "shutdown");
      let heldPermit: Parameters<typeof acceptsHeldCaptureCleanup>[1] | undefined;
      const rig = codexRig({ armDeadline: deadline.armDeadline, boundaryProcess: async request => {
        if (unsafe || !publication || injected) return undefined;
        injected++;
        const [bin, ...args] = request.argv;
        const child = spawn(bin!, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
        const terminal = new Promise<{ code: number }>((resolve, reject) => {
          child.once("error", reject);
          child.once("close", code => resolve({ code: code ?? 1 }));
        });
        void terminal.catch(() => undefined);
        return {
          stdin: child.stdin, stdout: child.stdout, stderr: child.stderr,
          waitChild: () => { deadline.fire(); return terminal; },
          root: {
            kind: "boundary_action",
            reap: async () => {
              await terminal;
              return { ok: true }; // Fixture root observation, not kernel reaper evidence.
            },
            dispose: async () => {
              if (child.exitCode === null) child.kill("SIGKILL");
              await terminal.catch(() => undefined);
            },
          },
        };
      }});
      const remove = git.removeRunnerClone.bind(git);
      t.mock.method(git, "removeRunnerClone", async (...args: Parameters<typeof remove>) => {
        if (args[0] === clone) sourceRemovals++;
        return remove(...args);
      });
      const boundary = rig.safety.withBoundary.bind(rig.safety);
      t.mock.method(rig.safety, "withBoundary", async (...args: Parameters<typeof boundary>) => {
        const [request, action] = args;
        try { return await boundary(request, async permit => {
          heldPermit = permit;
          return action(permit);
        }); } finally { heldPermit = undefined; joined = true; }
      });
      const verify = git.verifyRunnerTrackingCovers.bind(git);
      t.mock.method(git, "verifyRunnerTrackingCovers", async (...args: Parameters<typeof verify>) => {
        const result = await verify(...args);
        if (result) {
          verifies++;
          verifiedHead = readGit(git.barePathFor(fx.originPath), "rev-parse", `refs/uzi-runner/agent/issue-${iid}`).trim();
        }
        return result;
      });
      const executor = new FakeCodexExecutor(rig.safety, async ctx => {
        clone = ctx.worktreePath;
        if (finalize) commitInTree(clone, "RESULT.txt", bytes);
        else fs.writeFileSync(path.join(clone, "RESULT.txt"), bytes);
        if (site === "recovery") throw new TransientRecoveryError("publication cleanup fixture");
        if (site === "hold") {
          await ctx.enterCompletionHold!("publication cleanup");
          return { branch: ctx.branch, completionHeld: { reason: "publication cleanup" } };
        }
        if (site === "wall") {
          await ctx.parkForWall!({ completedCount: 0 });
          return { branch: ctx.branch, walled: { reason: "publication cleanup" } };
        }
        return { branch: ctx.branch, ...(site === "prepush" ? { nonClosing: { reason: "remaining work" } } : {}) };
      }, undefined, async () => {
        freeCalls++;
        assert.equal(joined, true, "fresh fallback must wait for primary settlement");
        return { kind: "incomplete", errors: [{ category: "tool", message: "no fresh empty proof" }] };
      });
      const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { recoveryRetryMs: 1 });
      const internal = runner as unknown as {
        buildCheckpointOverlay: (...args: unknown[]) => Promise<unknown>;
        captureHoldContext: (...args: unknown[]) => Promise<unknown>;
      };
      const publicationChild = async () => {
        publication = true;
        await git.trackingTip(git.barePathFor(fx.originPath), `agent/issue-${iid}`);
        if (unsafe) {
          injected++;
          if (fault === "raw-tool-late-expiry") fs.writeFileSync(path.join(clone, "RESULT.txt"), "later unverified bytes\n");
          rig.registry.poison({ category: "tool", message: "independent cleanup unconfirmed" });
          if (fault !== "raw-tool") deadline.fire();
        }
      };
      if (finalize) {
        // Both finalize callers disable publication. Exercise their held continuation after
        // local proof with the same real child cleanup refusal, without enabling publication.
        const capture = internal.captureHoldContext.bind(runner);
        t.mock.method(internal, "captureHoldContext", async (...args: unknown[]) => {
          const result = await capture(...args);
          await publicationChild();
          return result;
        });
        if (site === "prepush") t.mock.method(git, "publishedCommitMessages", async () => null);
        api.setRegisterProtocolFeatures(["claim_generation_fence", "completion_non_closing_v1"]);
        await client.register("cleanup-finalize");
      } else {
        t.mock.method(internal, "buildCheckpointOverlay", async () => {
          await publicationChild();
          throw new Error("ordinary optional publication failure after safe cleanup");
        });
      }
      const holdRequest = client.requestCompletionHold.bind(client);
      t.mock.method(client, "requestCompletionHold", async (...args: Parameters<typeof holdRequest>) => {
        // These deadline continuations are joined; ordinary healthy controls above remain held.
        assert.equal(joined, true);
        assert.equal(heldPermit, undefined);
        return holdRequest(...args);
      });
      api.setCompletionHoldResponse("paused");
      api.setWallParkResponse("paused", 200);
      const claim = gitlabClaim(iid, finalize ? {
        claim_generation: 7, config: { completion_contract_version: 1, contract_revision: 1 },
      } : {});
      if (finalize) {
        api.setPublicationState(claim.run_id, [], 1);
        api.setCompletionPermitResponse(false, { denyReason: "incomplete" });
      }
      await runner.execute(claim);
      assert.equal(injected, 1, "a real Git child completed in the tested scope");
      assert.equal(verifies, 1, "no reuse or recapture of invalidated provisional evidence");
      assert.equal(rig.refreshCalls(), 1, "no second credential refresh");
      assert.ok(verifiedHead);
      assert.equal(readGit(git.barePathFor(fx.originPath), "show", verifiedHead + ":RESULT.txt"), bytes);
      assert.equal(readGit(clone, "rev-parse", "HEAD").trim(), verifiedHead);
      assert.equal(fs.readFileSync(path.join(clone, "RESULT.txt"), "utf8"),
        fault === "raw-tool-late-expiry" ? "later unverified bytes\n" : bytes,
        "the exact original source survives invalidated local evidence");
      if (site === "wall") {
        assert.equal(api.wallParkRequests.length, unsafe ? 0 : 1);
        if (!unsafe) assert.equal(api.wallParkRequests[0]!.body.head, verifiedHead);
      } else if (site === "recovery") {
        assert.equal(api.states.filter(state => state.runId === claim.run_id && state.body.status === "recovery_wait").length, unsafe ? 0 : 1);
      } else {
        assert.equal(api.completionHoldRequests.length, unsafe ? 0 : 1, "unsafe cleanup cannot authorize a hold ACK, even with degraded capture allowed");
        if (!unsafe) assert.equal(api.completionHoldRequests[0]!.body.head, verifiedHead);
      }
      assert.equal(freeCalls, unsafe ? 1 : 0, "only a fresh proof can replace invalidated evidence");
      assert.equal(sourceRemovals, 0, "capture refusal never deletes the original source");
    });
  }
}
