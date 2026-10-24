import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { Executor, RunContext } from "../src/executor.js";
import { CodexBoundaryError } from "../src/codex/safety.js";
import { api, client, fakeGitlab, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, FakeCodexExecutor, spyCustodySettle, spyPublishLands, statuses } from "./runner-codex-sinks-fixture.js";
import { fixture, parks, QUIESCENT, waitForAbort } from "./runner-terminal-disk-deferral-fixture.js";

installHarness();

for (const mode of ["claude", "stub", "codex"] as const) {
  it(`wall ownership cancellation probe preserves ${mode} behavior`, async t => {
    const rig = codexRig();
    const claim = gitlabClaim(286300 + ["claude", "stub", "codex"].indexOf(mode));
    let cancelledInCallback: boolean | undefined, outcome: unknown, finishes = 0;
    const behavior: Executor["run"] = async ctx => {
      fs.writeFileSync(path.join(ctx.worktreePath, "PROBE.txt"), "probe work\n");
      outcome = await ctx.parkForWall!({ completedCount: 0 });
      cancelledInCallback = ctx.cancelRequested!();
      return { branch: ctx.branch, walled: { reason: "probe complete" } };
    };
    const executor: Executor = mode === "codex"
      ? new FakeCodexExecutor(rig.safety, behavior, undefined, rig.capture)
      : { run: behavior, ...(mode === "claude" ? { sandboxesCommands: false } : {}) };
    const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab);
    const seam = runner as unknown as { finishWallCancellation(...args: unknown[]): Promise<void> };
    const finish = seam.finishWallCancellation.bind(runner);
    t.mock.method(seam, "finishWallCancellation", async (...args: unknown[]) => {
      finishes++;
      return finish(...args);
    });
    t.mock.method(client, "reportWallPark", async () => ({ applied: false, status: "running" }));
    const ownership = client.getRunOwnership.bind(client);
    t.mock.method(client, "getRunOwnership", async (...args: Parameters<typeof ownership>) => {
      const own = await ownership(...args);
      return { ...own, status: "cancelled" };
    });
    await runner.execute(claim);
    assert.equal(outcome, "cancelled");
    assert.equal(cancelledInCallback, true, "the base ownership helper already observes cancellation");
    assert.equal(finishes, mode === "codex" ? 1 : 0);
  });
}

for (const control of ["park", "pause", "cancel", "shutdown", "revoked"] as const) {
  it(`typed terminal-disk handoff: actual ${control} disposition`, async t => {
    let elapsed = 0;
    const realNow = Date.now.bind(Date);
    t.mock.method(Date, "now", () => realNow() + elapsed);
    const rig = codexRig();
    const publication = spyPublishLands();
    t.after(publication);
    let ctx!: RunContext, deferred = false, interrupted = false, captureCalls = 0;
    let verified = false, ownershipRechecks = 0, diskProofs = 0;
    let operationDeadline: number | undefined, expectedDeadline: number | undefined;
    let handoffSignal: AbortSignal | undefined;
    let handoffAbortedDuringCapture: boolean | undefined, captureAbortedDuringCapture: boolean | undefined;
    const freeDeadlines: number[] = [];
    const boundary = rig.safety.withBoundary.bind(rig.safety);
    t.mock.method(rig.safety, "withBoundary", async (request: Parameters<typeof boundary>[0]) => {
      assert.equal(deferred, false, "no credential boundary after typed deferral");
      expectedDeadline = Date.now() + request.deadlineMs + 60_000;
      // Advance virtual admission time so renewing the end-to-end deadline is observable.
      elapsed += 1_000;
      deferred = true;
      throw new CodexBoundaryError("reconcile", [], undefined, "vault_locked");
    });
    const f = fixture({
      safety: rig.safety,
      features: ["recovery_cause_data_volume_full", "claim_generation_fence", "recovery_cause_vault_locked"],
      observe: value => { ctx = value; },
      withCredentialFreeCapture: (ms, action, purpose, signal, attempt) => {
        captureCalls++;
        freeDeadlines.push(Date.now() + ms);
        return rig.capture(ms, async held => {
          if (control !== "park" && control !== "revoked" && !interrupted) {
            interrupted = true;
            if (control === "shutdown") f.runner.shutdown();
            else api.setInputs(f.claim.run_id, [{ id: 1, kind: control, ...(control === "pause" ? { body: "now" } : {}) }]);
            await waitForAbort(ctx.signal!);
            // Observe the actual pending handoff/capture, before its action can exit.
            handoffAbortedDuringCapture = handoffSignal?.aborted;
            captureAbortedDuringCapture = signal?.aborted;
          }
          return action(held);
        }, purpose, signal, attempt);
      },
      runner: { quiesceRun: async request => {
        if (deferred && request.site === "terminal_disk:after_runner_git") diskProofs++;
        return QUIESCENT;
      } },
    });
    const custody = spyCustodySettle(f.runner);
    const verify = git.verifyRunnerTrackingCovers.bind(git);
    t.mock.method(git, "verifyRunnerTrackingCovers", async (...args: Parameters<typeof verify>) => {
      const result = await verify(...args);
      if (deferred && result) verified = true;
      return result;
    });
    const ownership = client.getRunOwnership.bind(client);
    t.mock.method(client, "getRunOwnership", async (...args: Parameters<typeof ownership>) => {
      const own = await ownership(...args);
      if (verified) {
        ownershipRechecks++;
        if (control === "revoked") return { ...own, claim_generation: 5 };
      }
      return own;
    });
    // Observe the real Git operation's deadline, while keeping its cancellation and timer.
    const operation = git.withRecoveryOperation.bind(git);
    t.mock.method(git, "withRecoveryOperation", async <T>(signal: AbortSignal, deadline: number, action: (signal: AbortSignal) => Promise<T>) => {
      if (deferred && operationDeadline === undefined) {
        operationDeadline = deadline;
        handoffSignal = signal;
      }
      return operation(signal, deadline, action);
    });
    await f.runner.execute(f.claim);
    assert.equal(deferred, true);
    assert.ok(captureCalls > 0);
    assert.ok(operationDeadline);
    assert.ok(Math.abs(operationDeadline - expectedDeadline!) < 100, "handoff keeps the initial 120 s plus reserved 60 s deadline");
    assert.ok(Math.abs(freeDeadlines[0]! - operationDeadline!) < 100, "the pending disk capture uses the original absolute B deadline");
    assert.equal(rig.refreshCalls() + rig.releaseCalls(), 0);
    assert.equal(f.calls(), 1);
    if (control === "park") {
      assert.equal(parks().length, 1);
      assert.equal(parks()[0]!.body.recovery_cause, "vault_locked", "credential typing keeps precedence");
      assert.ok(api.messages(f.claim.run_id).some(row => JSON.stringify(row).includes(
        "Paused: the run owner's vault is locked. The recovery checkpoint is published; this run resumes automatically at its next retry once the vault is unlocked.")), "credential notice keeps precedence");
    } else if (control === "revoked") {
      assert.equal(parks().length, 0, "ownership recheck forbids a park after generation revocation");
      assert.ok(!statuses(f.claim.run_id).includes("failed"));
    } else {
      assert.equal(interrupted, true);
      if (control !== "pause") {
        assert.equal(handoffAbortedDuringCapture, true, "terminal interruption aborts the pending handoff signal");
        assert.equal(captureAbortedDuringCapture, true, "terminal interruption reaches the pending free capture signal");
      }
      assert.equal(parks().length, 0, "interruption forbids recovery park");
      if (control === "cancel") {
        assert.ok(api.states.some(row => row.body.status === "failed" && row.body.failure_reason === "run cancelled"));
      } else assert.ok(!statuses(f.claim.run_id).includes("failed"));
    }
    if (control === "park" || control === "revoked") {
      assert.ok(diskProofs > 0, "typed handoff still proves terminal disk quiescence");
      assert.ok(ownershipRechecks > 0, "disk ownership is rechecked after verified capture");
    }
    assert.equal(custody(), 0, "typed handoff retains exact generation custody");
    assert.ok(fs.existsSync(f.clone()), "source custody remains local");
  });
}
