import { it } from "node:test";
import assert from "node:assert/strict";
import type { RunRunner } from "../src/runner.js";
import { PauseNowSignal, CredentialSwitchSignal, type SteeringChannel } from "../src/steering.js";
import { api, client, fakeGitlab, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();
const run = "22222222-2222-4222-8222-222222222222";
type TestActive = { steering: SteeringChannel; cancel: AbortController; shuttingDown: boolean };
function activeRuns(runner: RunRunner): Map<string, TestActive> {
  return (runner as unknown as { activeRuns: Map<string, TestActive> }).activeRuns;
}
async function bounded<T>(promise: Promise<T>, label: string, ms = 4000): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([promise, new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new Error(label)), ms);
    })]);
  } finally { clearTimeout(timer); }
}
// The fixture bounds both execution and cleanup; shutdown releases lifecycle waits even
// when an assertion fails. Steering stop is awaited before the fixture returns.
async function executeBounded(runner: RunRunner, claim: ReturnType<typeof gitlabClaim>): Promise<void> {
  const executing = runner.execute(claim);
  try {
    await bounded(executing, "runner fixture did not finish", 8000);
  } finally {
    const active = activeRuns(runner).get(claim.run_id);
    runner.shutdown();
    active?.cancel.abort();
    await bounded(Promise.all([executing, active?.steering.stop()]), "runner fixture cleanup did not finish");
  }
}
async function register() {
  api.registerNonce = "interrupted-test-nonce";
  await client.register("interrupted-worker");
}
function response() {
  return { status: 200, body: { format: 1, run_id: run, worker_id: client.interruptedCallsRegistration!.workerId,
    register_nonce: api.registerNonce, claim_generation: 3, source_generation: 2, inspected_count: 1,
    history_incomplete: true, unavailable: false, truncated: false, entries: [{
      seq: 5, source_generation: 2, agent: "lead", agent_instance: null, agent_label: null,
      tool_name: "Bash", command: "interrupted-only-command", display_omitted: false,
    }] } };
}

for (const generation of [1, 3]) {
  for (const Stop of [PauseNowSignal, CredentialSwitchSignal]) {
    for (const sharedCancel of [false, true]) {
      it(`retired ${Stop.name} G${generation} sharedCancel=${sharedCancel} must discard advisory without reviving stop`, async () => {
        await register();
        api.interruptedCallsResponse = response();
        let reached = false;
        let getterError: unknown;
        let delivered: unknown;
        const { gitlab } = fakeGitlab();
        const runner = runnerWith(() => ({ executor: { run: async ctx => {
          const active = activeRuns(runner).get(run)!;
          assert.ok(active);
          assert.equal(typeof ctx.getInterruptedCalls, "function");
          assert.equal(!!ctx.getInterruptedCalls!(), generation === 3);
          const retired = active.steering.lifecycleSignal();
          const reason = new Stop();
          active.steering.abortLifecycle(reason, false);
          if (sharedCancel) active.cancel.abort(reason);
          active.steering.rearmLifecycle();
          assert.notEqual(active.steering.lifecycleSignal(), retired);
          assert.equal(retired.aborted, true);
          assert.equal(active.steering.lifecycleSignal().aborted, false);
          assert.equal(active.steering.terminalLifecycleSignal().aborted, false);
          assert.equal(active.steering.isCancelled(), false);
          assert.equal(active.steering.claimFence(), undefined);
          try { delivered = ctx.getInterruptedCalls!(); } catch (err) { getterError = err; }
          assert.equal(ctx.getInterruptedCalls!(), undefined, "discarded evidence never revives");
          active.steering.abortLifecycle(new Stop(), false);
          active.steering.rearmLifecycle();
          assert.equal(ctx.getInterruptedCalls!(), undefined, "another rearm cannot revive evidence");
          reached = true;
          return { branch: ctx.branch, reportOnly: true, summary: "done" };
        } } }), gitlab);
        await executeBounded(runner, gitlabClaim(7, { run_id: run, kind: "prompt", claim_generation: generation }));
        assert.ok(reached, JSON.stringify(api.states));
        assert.equal(api.interruptedCallsGets.length, generation === 3 ? 1 : 0);
        assert.equal(getterError, undefined, "retired lifecycle must not be replayed by late advisory getter");
        assert.equal(delivered, undefined, "retired observation is discarded");
      });
    }
    it(`current ${Stop.name} G${generation} still propagates`, async () => {
      await register();
      api.interruptedCallsResponse = response();
      let reached = false;
      const { gitlab } = fakeGitlab();
      const runner = runnerWith(() => ({ executor: { run: async ctx => {
        const active = activeRuns(runner).get(run)!;
        const reason = new Stop();
        active.steering.abortLifecycle(reason, false);
        assert.throws(() => ctx.getInterruptedCalls!(), error => error === reason);
        reached = true;
        return { branch: ctx.branch, reportOnly: true, summary: "done" };
      } } }), gitlab);
      await executeBounded(runner, gitlabClaim(7, { run_id: run, kind: "prompt", claim_generation: generation }));
      assert.ok(reached, JSON.stringify(api.states));
      assert.equal(api.interruptedCallsGets.length, generation === 3 ? 1 : 0);
    });
  }

  for (const stop of ["pause", "switch", "terminal", "shutdown", "cancel"] as const) {
    it(`live ${stop} G${generation} propagates exactly${stop === "cancel" ? " without rearm" : " after rearm"}`, async () => {
      await register();
      api.interruptedCallsResponse = response();
      let reached = false;
      const { gitlab } = fakeGitlab();
      const runner = runnerWith(() => ({ executor: { run: async ctx => {
        const active = activeRuns(runner).get(run)!;
        assert.equal(!!ctx.getInterruptedCalls!(), generation === 3);
        if (stop !== "cancel") {
          const retired = new PauseNowSignal();
          active.steering.abortLifecycle(retired, false);
          active.cancel.abort(retired);
          active.steering.rearmLifecycle();
          assert.equal(active.steering.lifecycleSignal().aborted, false);
        }
        let reason: unknown;
        if (stop === "shutdown") {
          runner.shutdown();
          reason = active.steering.lifecycleSignal().reason;
        } else {
          reason = stop === "pause" ? new PauseNowSignal() :
            stop === "switch" ? new CredentialSwitchSignal() : new Error(`live ${stop}`);
          if (stop === "cancel") active.cancel.abort(reason);
          else active.steering.abortLifecycle(reason, stop === "terminal");
        }
        assert.throws(() => ctx.getInterruptedCalls!(), error => error === reason);
        assert.throws(() => ctx.getInterruptedCalls!(), error => error === reason,
          "discarding evidence on a throw does not erase an active stop");
        reached = true;
        return { branch: ctx.branch, reportOnly: true, summary: "done" };
      } } }), gitlab);
      await executeBounded(runner, gitlabClaim(7, { run_id: run, kind: "prompt", claim_generation: generation }));
      assert.ok(reached, JSON.stringify(api.states));
      assert.equal(api.interruptedCallsGets.length, generation === 3 ? 1 : 0);
    });
  }
}
