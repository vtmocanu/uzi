import { it } from "node:test";
import assert from "node:assert/strict";
import type { RunContext } from "../src/executor.js";
import type { RunKind } from "../src/protocol.js";
import type { RunRunner } from "../src/runner.js";
import type { SteeringChannel } from "../src/steering.js";
import { api, client, fakeGitlab, gitlabClaim, input, installHarness, runnerWith } from "./runner-harness.js";

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
function aborted(signal: AbortSignal): Promise<void> {
  return signal.aborted ? Promise.resolve() : new Promise(resolve => {
    signal.addEventListener("abort", () => resolve(), { once: true });
  });
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
for (const kind of ["issue", "ci_fix", "self_improve", "prompt", "task", "mr_rework"] as RunKind[]) {
  it(`real runner setup fetches exactly once for ${kind}, with no SDK or resumed gate`, async () => {
    await register();
    api.interruptedCallsResponse = response();
    let context: RunContext | undefined;
    const { gitlab } = fakeGitlab();
    const runner = runnerWith(() => ({ executor: { run: async ctx => {
      context = ctx;
      assert.equal(api.interruptedCallsGets.length, 1);
      assert.equal(ctx.getInterruptedCalls?.()?.entries[0]?.command, "interrupted-only-command");
      await Promise.resolve();
      assert.ok(ctx.getInterruptedCalls?.());
      return { branch: ctx.branch, reportOnly: true, summary: "done" };
    } } }), gitlab);
    await runner.execute(gitlabClaim(7, { run_id: run, kind, claim_generation: 3,
      ...(kind === "task" ? { branch: `uzi/task/${run}` } : {}),
      ...(kind === "mr_rework" ? { branch: "agent/issue-7", pipeline_ref: "agent/issue-7", mr_iid: 7 } : {}),
      ...(kind === "ci_fix" ? { pipeline: { id: 1, ref: "agent/issue-7", web_url: "https://example.test/1", failed_jobs: [] } } : {}),
    }));
    assert.ok(context, JSON.stringify(api.states));
    assert.equal(api.interruptedCallsGets.length, 1);
    assert.equal(api.interruptedCallsGets[0]!.nonce, api.registerNonce);
    assert.equal(api.interruptedCallsGets[0]!.generation, "3");
    assert.equal(context.getInterruptedCalls?.(), undefined, "ended flight cannot deliver evidence");
    assert.ok(!JSON.stringify({ states: api.states, messages: api.messages(run) }).includes("interrupted-only-command"));
  });
}
for (const mode of ["cold", "preapproved", "planning-resume", "codex-bound"] as const) {
  it(`setup is independent of ${mode}; registration change during a planning wait invalidates getter`, async () => {
    await register();
    api.interruptedCallsResponse = response();
    let executed = false;
    const { gitlab } = fakeGitlab();
    const runner = runnerWith(() => ({ executor: { run: async ctx => {
      executed = true;
      assert.ok(ctx.getInterruptedCalls?.());
      await client.register("same-worker");
      assert.equal(ctx.getInterruptedCalls?.(), undefined);
      return { branch: ctx.branch, reportOnly: true, summary: "done" };
    } } }), gitlab);
    await runner.execute(gitlabClaim(7, { run_id: run, kind: "prompt", claim_generation: 3,
      ...(mode === "preapproved" ? { plan_source: "seeded", plan_approved: true, plan_md: "# approved" } : {}),
      ...(mode === "planning-resume" ? { session_id: "missing-transcript", resume_phase: "awaiting_approval", plan_md: "# waiting" } : {}),
      ...(mode === "codex-bound" ? { secrets: { forge_pat: "fixture-forge-pat-000000", codex: { auth_mode: "bogus" } } } : {}),
    }));
    assert.ok(executed, JSON.stringify(api.states));
    assert.equal(api.interruptedCallsGets.length, 1);
  });
}
for (const stop of ["cancel", "claim-loss"] as const) {
  it(`late optional fetch failure preserves real ${stop} lifecycle handling`, async t => {
    await register();
    let entered = false;
    let executed = false;
    let observedFence: string | undefined;
    const { gitlab } = fakeGitlab();
    const runner = runnerWith(() => ({ executor: { run: async ctx => {
      executed = true;
      return { branch: ctx.branch, reportOnly: true, summary: "unexpected" };
    } } }), gitlab);
    t.mock.method(client, "getInterruptedCalls", async (...[_run, _generation, _identity, signal]: Parameters<typeof client.getInterruptedCalls>) => {
      entered = true;
      const active = activeRuns(runner).get(run);
      assert.ok(active);
      if (stop === "cancel") {
        api.rawInputGets(run, { inputs: [], terminal_cancelled: { run_id: run, generation: 3 } });
      } else {
        api.strictReceiptGenerations = true;
        api.setInputClaimGeneration(run, 4);
        api.setInputFenceReason(run, "stale");
        api.setInputs(run, [input("follow_up", "late input")]);
      }
      await bounded(aborted(signal), "steering did not abort optional fetch");
      observedFence = active.steering.claimFence();
      throw new Error("optional network failure after lifecycle ended");
    });
    await executeBounded(runner, gitlabClaim(7, { run_id: run, kind: "prompt", claim_generation: 3 }));
    assert.ok(entered);
    assert.equal(observedFence, stop === "cancel" ? "terminal_cancelled" : "stale");
    if (stop === "claim-loss") assert.ok(api.timeline.some(e =>
      e.type === "receipt_reply" && e.runId === run && e.kind === "ack" && e.httpStatus === 409));
    assert.equal(executed, false, "optional failure did not erase lifecycle stop");
    assert.ok(!api.states.some(s => s.body.status === "completed"));
  });
}

for (const stop of ["cancel", "claim-loss"] as const) {
  it(`getter propagates real ${stop} after an execution planning wait`, async () => {
    await register();
    api.interruptedCallsResponse = response();
    let checked = false;
    const { gitlab } = fakeGitlab();
    const runner = runnerWith(() => ({ executor: { run: async ctx => {
      assert.ok(ctx.getInterruptedCalls?.());
      if (stop === "cancel") {
        api.rawInputGets(run, { inputs: [], terminal_cancelled: { run_id: run, generation: 3 } });
      } else {
        api.strictReceiptGenerations = true;
        api.setInputClaimGeneration(run, 4);
        api.setInputFenceReason(run, "stale");
        api.setInputs(run, [input("follow_up", "input after wait")]);
      }
      await bounded(aborted(ctx.signal!), "steering did not end planning wait");
      assert.equal(activeRuns(runner).get(run)!.steering.claimFence(), stop === "cancel" ? "terminal_cancelled" : "stale");
      assert.throws(() => ctx.getInterruptedCalls?.());
      checked = true;
      ctx.signal!.throwIfAborted();
      return { branch: ctx.branch };
    } } }), gitlab);
    await executeBounded(runner, gitlabClaim(7, { run_id: run, kind: "prompt", claim_generation: 3 }));
    assert.ok(checked);
  });
}

for (const phase of ["fetch", "planning wait"] as const) {
  it(`real runner discards evidence when its active flight is replaced during ${phase}`, async t => {
    await register();
    api.interruptedCallsResponse = response();
    let original: TestActive | undefined;
    let replacement: TestActive | undefined;
    let checked = false;
    const { gitlab } = fakeGitlab();
    const runner = runnerWith(() => ({ executor: { run: async ctx => {
      try {
        if (phase === "planning wait") {
          assert.ok(ctx.getInterruptedCalls?.());
          await Promise.resolve();
          replace();
        }
        assert.equal(ctx.getInterruptedCalls?.(), undefined);
        restore();
        assert.equal(ctx.getInterruptedCalls?.(), undefined,
          "observed flight replacement permanently discards evidence even if the original returns");
        checked = true;
        return { branch: ctx.branch, reportOnly: true, summary: "done" };
      } finally { restore(); }
    } } }), gitlab);
    function replace() {
      const map = activeRuns(runner);
      original = map.get(run);
      assert.ok(original);
      replacement = { ...original };
      map.set(run, replacement);
      assert.equal(map.get(run), replacement);
    }
    function restore() {
      if (!replacement) return;
      const map = activeRuns(runner);
      assert.equal(map.get(run), replacement, "fixture still owns the replacement");
      map.set(run, original!);
      assert.equal(map.get(run), original, "original flight restored");
      replacement = undefined;
    }
    if (phase === "fetch") {
      const fetch = client.getInterruptedCalls.bind(client);
      t.mock.method(client, "getInterruptedCalls", async (...args: Parameters<typeof fetch>) => {
        const result = await fetch(...args);
        replace();
        return result;
      });
    }
    try {
      await executeBounded(runner, gitlabClaim(7, { run_id: run, kind: "prompt", claim_generation: 3 }));
      assert.ok(checked, JSON.stringify(api.states));
    } finally { restore(); }
  });
}

it("generation one setup does not fetch", async () => {
  await register();
  const { gitlab } = fakeGitlab();
  const runner = runnerWith(() => ({ executor: { run: async ctx => {
    assert.equal(ctx.getInterruptedCalls?.(), undefined);
    return { branch: ctx.branch, reportOnly: true, summary: "done" };
  } } }), gitlab);
  await runner.execute(gitlabClaim(7, { run_id: run, kind: "prompt", claim_generation: 1 }));
  assert.equal(api.interruptedCallsGets.length, 0);
});
