import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { Executor } from "../src/executor.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { CodexBoundaryError } from "../src/codex/safety.js";
import { api, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, FakeCodexExecutor, spyPublishLands, statuses } from "./runner-codex-sinks-fixture.js";
import { fixture, parks, QUIESCENT, readGit } from "./runner-terminal-disk-deferral-fixture.js";
import { nullLogger } from "./helpers.js";

installHarness();

for (const free of [false, true]) {
  it(`terminalDisk ${free ? "A held-free" : "ordinary authenticated"} capture reuses its proof and saves real bytes`, async t => {
    const rig = codexRig();
    let deferred = false;
    let depth = 0, maxDepth = 0, proofs = 0, captures = 0;
    let armed = false;
    const source: Array<{ depth: number; free: boolean }> = [];
    let inFree = false;
    const boundary = rig.safety.withBoundary.bind(rig.safety);
    t.mock.method(rig.safety, "withBoundary", async (request: Parameters<typeof boundary>[0], action: Parameters<typeof boundary>[1]) => {
      assert.equal(inFree, false, "held-free capture never opens an authenticated boundary");
      assert.equal(deferred, false, "A deferral never retries authenticated admission");
      if (free && request.boundary === "shutdown") {
        deferred = true;
        throw new CodexBoundaryError("reconcile", [], undefined, "vault_locked");
      }
      depth++;
      maxDepth = Math.max(maxDepth, depth);
      try { return await boundary(request, action); } finally { depth--; }
    });
    for (const name of ["worktreeStatus", "commitWipMarker", "worktreeHead", "verifyRunnerTrackingCovers", "trackingTip"] as const) {
      const original = git[name].bind(git);
      t.mock.method(git, name, async (...args: any[]) => {
        if (armed) source.push({ depth, free: inFree });
        return Reflect.apply(original, git, args);
      });
    }
    const publication = spyPublishLands();
    t.after(publication);
    const f = fixture({
      safety: rig.safety,
      features: ["recovery_cause_data_volume_full", "claim_generation_fence", "recovery_cause_vault_locked"],
      withCredentialFreeCapture: (ms, action, purpose, signal, attempt) => rig.capture(ms, async held => {
        captures++;
        inFree = true;
        try { return await action(held); } finally { inFree = false; }
      }, purpose, signal, attempt),
      beforeReject: () => { armed = true; },
      runner: { quiesceRun: async req => {
        if (req.site === "terminal_disk:after_runner_git") {
          proofs++;
          assert.equal(free ? inFree : depth === 1, true, "post-Git proof stays in the original scope");
        }
        return QUIESCENT;
      } },
    });
    await f.runner.execute(f.claim);
    assert.equal(f.calls(), 1);
    assert.equal(parks().length, 1);
    assert.ok(proofs > 0, "terminal disk post-Git proof executed");
    assert.equal(maxDepth, free ? 0 : 1, "no nested authenticated boundary");
    assert.equal(rig.refreshCalls(), free ? 0 : 1, "A has zero auth activity; ordinary recovery reconciles once");
    assert.equal(parks()[0]!.body.recovery_cause, free ? "vault_locked" : "data_volume_full");
    assert.equal(rig.releaseCalls(), 0);
    assert.equal(captures, free ? 1 : 0);
    assert.ok(source.length > 3, "real source capture and readback executed");
    assert.ok(source.every(event => free ? event.free : event.depth === 1 && !event.free), JSON.stringify(source));
    const bare = git.barePathFor(fx.originPath);
    for (const [name, bytes] of [["COMMITTED.txt", "committed sentinel"], ["DIRTY.txt", "dirty sentinel"]]) {
      assert.equal(readGit(bare, "show", `refs/uzi-runner/agent/issue-2201:${name}`), bytes);
      assert.equal(fs.readFileSync(path.join(f.clone(), name!), "utf8"), bytes + "\n");
    }
    assert.ok(!statuses(f.claim.run_id).includes("failed"));
    assert.equal(f.forge.calls.length, 0);
  });
}

for (const codex of [true, false]) for (const route of ["in-place", "outer"] as const) {
  it(`${codex ? "safety-backed Codex" : "Claude control"} ${route} credential switch capture`, async t => {
    const rig = codexRig();
    let armed = false, reads = 0, clone = "", retainedChecked = false;
    const originalStatus = git.worktreeStatus.bind(git);
    t.mock.method(git, "worktreeStatus", async (...args: Parameters<typeof originalStatus>) => {
      if (armed) reads++;
      return originalStatus(...args);
    });
    const publication = spyPublishLands();
    t.after(publication);
    const claim = gitlabClaim(285280 + Number(codex) * 2 + Number(route === "outer"));
    api.overrideStateStatus(claim.run_id, "queued");
    const behavior: Executor["run"] = async ctx => {
      clone = ctx.worktreePath;
      fs.writeFileSync(path.join(clone, "SWITCH.txt"), "switch variant local bytes\n");
      armed = true;
      if (route === "outer") throw new CredentialSwitchSignal();
      if (codex) {
        await assert.rejects(() => ctx.attemptCredentialSwitch!());
        // runGatedSink supplies the actual live flight; no synthetic custody object.
        assert.ok(actualFlight);
        const result = await privateRunner.enterRetainedCredentialSwitch(claim, actualFlight, nullLogger());
        assert.equal(result, "stopped");
        retainedChecked = true;
        throw new CredentialSwitchSignal();
      }
      await ctx.attemptCredentialSwitch!();
      throw new CredentialSwitchSignal();
    };
    const executor: Executor = codex
      ? new FakeCodexExecutor(rig.safety, behavior, undefined, rig.capture)
      : { run: behavior };
    const { gitlab } = fakeGitlab();
    const runner = runnerWith(() => ({ executor }), gitlab);
    const privateRunner = runner as unknown as {
      runGatedSink: (flight: unknown, action: () => Promise<unknown>) => Promise<unknown>;
      enterRetainedCredentialSwitch: (candidate: typeof claim, flight: unknown, log: ReturnType<typeof nullLogger>) => Promise<string>;
    };
    let actualFlight: unknown;
    const gated = privateRunner.runGatedSink.bind(runner);
    t.mock.method(privateRunner, "runGatedSink", async (flight: unknown, action: () => Promise<unknown>) => {
      actualFlight = flight;
      return gated(flight, action);
    });
    await runner.execute(claim);
    if (codex) {
      assert.equal(reads, 0, "both switch routes refuse before reading source");
      assert.equal(rig.refreshCalls(), 0);
      assert.equal(rig.releaseCalls(), 0);
      assert.deepEqual(rig.boundaries, []);
      assert.ok(!statuses(claim.run_id).includes("credential_switch"));
      assert.ok(!statuses(claim.run_id).includes("credential_switch_failed"));
      assert.equal(retainedChecked, route === "in-place");
    } else {
      assert.ok(reads > 0, "Claude still captures its source");
      assert.ok(statuses(claim.run_id).includes("credential_switch"));
    }
    assert.equal(fs.readFileSync(path.join(clone, "SWITCH.txt"), "utf8"), "switch variant local bytes\n");
  });
}
