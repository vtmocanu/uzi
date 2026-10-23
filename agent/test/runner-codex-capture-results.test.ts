import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { CodexBoundaryError } from "../src/codex/safety.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, FakeCodexExecutor, GIT_ENV, manualDeadline } from "./runner-codex-sinks-fixture.js";
import { nullLogger } from "./helpers.js";

installHarness();

const bytes = "verified capture survives optional publication\n";
const readGit = (tree: string, ...args: string[]) =>
  execFileSync("git", ["-C", tree, ...args], { env: GIT_ENV, encoding: "utf8" });

it("no-held capture with publication disabled acquires before its first source read", async t => {
  const rig = codexRig();
  let direct = false, admitted = false, checked = false, reads = 0;
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
      } finally { direct = false; }
    }
    return gated(flight, action);
  });
  api.setCompletionHoldResponse("paused");
  await runner.execute(claim);
  assert.equal(checked, true, "the real flight reached the intercepted sink");
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
