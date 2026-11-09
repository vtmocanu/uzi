import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { FollowUpOutcome, RunContext, SecretRemediationDecision } from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import { nonexistentWorktreeFactory, nullLogger } from "./helpers.js";

/**
 * Issue #1932 m3 — the Claude executor consults ctx.secretRemediationGate at the done point,
 * BEFORE the done checkpoint / completion attempt: `remediate` re-prompts the SAME session,
 * `fail` stops with no checkpoint and no attempt, and an interactive run never reaches the gate.
 */

function submitPlan(plan: string, sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t1", name: "mcp__uzi__submit_plan", input: { plan_md: plan } }] },
  } as unknown as SDKMessage;
}
function signalDone(sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t2", name: "mcp__uzi__signal_done", input: {} }] },
  } as unknown as SDKMessage;
}
function resultSuccess(sessionId = "sess-1"): SDKMessage {
  return {
    type: "result",
    subtype: "success",
    is_error: false,
    num_turns: 1,
    session_id: sessionId,
  } as unknown as SDKMessage;
}

/** A queryFn replaying one scripted stream per turn and recording each turn's prompt text. */
function fakeTurns(scripts: SDKMessage[][]): { queryFn: SdkQueryFn; prompts: string[] } {
  let i = 0;
  const prompts: string[] = [];
  const queryFn: SdkQueryFn = (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    i++;
    return (async function* () {
      let text = "";
      for await (const m of params.prompt) text += JSON.stringify(m);
      prompts.push(text);
      for (const m of script) yield m;
    })();
  };
  return { queryFn, prompts };
}

let homeDir: string;
const nonexistentWorktree = nonexistentWorktreeFactory("uzi-secretgate");

function makeCtx(overrides: Partial<RunContext> = {}): RunContext {
  const approve: PlanVerdict = { kind: "approve", selection: { status: "absent" } };
  return {
    runId: "r1",
    issueIid: 5,
    issueTitle: "Fix login",
    issueDescription: "please implement",
    worktreePath: nonexistentWorktree(),
    branch: "agent/issue-5",
    emit: () => {},
    oauthToken: "dummy-oauth-token-do-not-scan-0000",
    agents: [],
    config: null,
    sessionId: null,
    onSessionId: () => {},
    gatePlan: async () => approve,
    pullFollowUps: () => [],
    reportIteration: () => {},
    checkpoint: async () => {},
    worktreeFingerprint: async () => "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n",
    ...overrides,
  };
}

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-secretgatehome-"));
});
afterEach(() => {
  fs.rmSync(homeDir, { recursive: true, force: true });
});

const REMEDIATE = "REMEDIATE-SECRET-MARKER: rewrite the flagged commit";

describe("iteration resume remediation preflight", () => {
  for (const resumed of [false, true]) {
    it(`SDK preflight requires resume and park capability; resumed=${resumed}`, async () => {
      const { queryFn } = fakeTurns([[signalDone(), resultSuccess()]]);
      let preflights = 0;
      await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
        kind: "issue", sessionId: "sess-1", resumed, planApproved: true, approvedPlan: "approved",
        config: { max_iterations: 1, ...(resumed ? {} : { iteration_park_version: 1 }) },
        parkForIteration: async () => "parked",
        secretRemediationGate: async options => { if (options) preflights++; return { action: "proceed" }; },
      }));
      assert.equal(preflights, 0);
    });
  }
  for (const sessionId of ["sess-1", null]) {
    for (const action of ["remediate", "proceed", "fail"] as const) {
      it(`SDK preflight session=${sessionId} action=${action} owns only the first work turn`, async () => {
        const { queryFn, prompts } = fakeTurns([[resultSuccess()]]);
        let checks = 0;
        let ownerPulls = 0;
        const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
          kind: "issue", resumed: true, sessionId, seeded: true,
          planApproved: true, approvedPlan: "approved work",
          config: { max_iterations: 2, iteration_park_version: 1 },
          reportIteration: async () => ({ completionAttempts: 0 }),
          parkForIteration: async () => "parked",
          pullFollowUps: () => { ownerPulls++; return []; },
          secretRemediationGate: async options => {
            checks++;
            assert.deepEqual(options, { unpublishedOnly: true });
            return action === "remediate" ? { action, followUp: REMEDIATE } : { action };
          },
        }));
        assert.equal(checks, 1);
        assert.equal(prompts.length, 2, "preflight does not add an implementation turn");
        assert.equal(prompts[0]!.includes(REMEDIATE), action === "remediate");
        assert.equal(prompts[1]!.includes(REMEDIATE), false, "guidance is not replayed on later ordinary turns");
        if (action === "remediate") assert.equal(ownerPulls, 1, "system guidance owns the first turn only");
        assert.deepEqual(result.iterationParked, { outcome: "parked" });
      });
    }
  }

  it("SDK preflight cancellation during scan dispatches no model work", async () => {
    const { queryFn, prompts } = fakeTurns([[resultSuccess()]]);
    let cancelled = false;
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
      kind: "issue", resumed: true, sessionId: "sess-1", planApproved: true, approvedPlan: "approved",
      config: { max_iterations: 1, iteration_park_version: 1 }, parkForIteration: async () => "parked",
      cancelRequested: () => cancelled,
      secretRemediationGate: async () => { cancelled = true; return { action: "remediate", followUp: REMEDIATE }; },
    })), /cancel/i);
    assert.equal(prompts.length, 0);
  });

  for (const control of ["wall", "disk", "switch"] as const) {
    it(`SDK preflight preserves ${control} arriving during its await`, async () => {
      const { queryFn, prompts } = fakeTurns([[resultSuccess()]]);
      let mode: "wall" | "disk" | null = null;
      let pause: (() => void) | undefined;
      let credentialSwitch: (() => void) | undefined;
      let switches = 0;
      const running = new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
        kind: "issue", resumed: true, sessionId: "sess-1", planApproved: true, approvedPlan: "approved",
        config: { max_iterations: 1, iteration_park_version: 1 }, parkForIteration: async () => "parked",
        pauseModeRequested: () => mode, onPauseNow: callback => { pause = callback; },
        onCredentialSwitch: callback => { credentialSwitch = callback; },
        attemptCredentialSwitch: async () => { switches++; return "released"; },
        parkForWall: async () => "parked",
        secretRemediationGate: async () => {
          if (control === "switch") { assert.ok(credentialSwitch); credentialSwitch(); }
          else { mode = control; if (control === "wall") { assert.ok(pause); pause(); } }
          return { action: "remediate", followUp: REMEDIATE };
        },
      }));
      if (control === "disk") await assert.rejects(running, error => error instanceof Error && error.constructor.name === "DiskParkSignal");
      else {
        const result = await running;
        if (control === "switch") { assert.equal(switches, 1); assert.equal(result.switchReleased, true); }
        else assert.ok(result.walled);
      }
      assert.equal(prompts.length, 0, "control wins before model dispatch");
    });
  }

  it("SDK preflight is regenerated after a crash before first model evidence", async () => {
    const { queryFn, prompts } = fakeTurns([[resultSuccess()]]);
    let checks = 0;
    const context = () => makeCtx({
      kind: "issue", resumed: true, sessionId: null, seeded: true,
      planApproved: true, approvedPlan: "approved",
      config: { max_iterations: 1, iteration_park_version: 1 },
      reportIteration: async () => ({ completionAttempts: 0 }), parkForIteration: async () => "parked",
      secretRemediationGate: async () => {
        if (++checks === 1) throw new Error("simulated crash before model evidence");
        return { action: "remediate", followUp: REMEDIATE };
      },
    });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(context()), /simulated crash/);
    assert.equal(prompts.length, 0);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(context());
    assert.equal(checks, 2);
    assert.equal(prompts.length, 1);
    assert.ok(prompts[0]!.includes(REMEDIATE));
  });
});

describe("SdkExecutor secret remediation gate (Issue #1932)", () => {
  for (const interlocked of [false, true]) {
    const label = interlocked ? "interlocked" : "non-interlocked";

    it(`${label}: remediate re-prompts the same session, then proceed completes; the done checkpoint waits for the gate`, async () => {
      const { queryFn, prompts } = fakeTurns([
        [submitPlan("plan"), resultSuccess()],
        [signalDone(), resultSuccess()],
        [signalDone(), resultSuccess()],
      ]);
      const events: string[] = [];
      const decisions: SecretRemediationDecision[] = [{ action: "remediate", followUp: REMEDIATE }, { action: "proceed" }];
      let attempts = 0;
      const ctx = makeCtx({
        ...(interlocked
          ? { completionInterlock: true, recordCompletionAttempt: async () => { attempts++; events.push("attempt"); return { unmet: [], attemptCount: attempts }; } }
          : {}),
        checkpoint: async (o) => { events.push(o.reap ? "checkpoint:reap" : "checkpoint"); },
        secretRemediationGate: async () => { events.push("gate"); return decisions.shift()!; },
        codeCrossCheckGate: async () => { events.push("code"); return { action: "proceed" }; },
      });
      if (interlocked) ctx.kind = "issue";
      const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
      assert.strictEqual(result.branch, "agent/issue-5");
      assert.strictEqual(prompts.length, 3, "plan + implement + one remediation turn");
      assert.ok(prompts[2]!.includes(REMEDIATE), "the remediation follow-up is the next turn's prompt");
      assert.ok(!prompts[1]!.includes(REMEDIATE));
      assert.strictEqual(events[0], "gate", "the gate runs before any done checkpoint on the done turn");
      assert.deepStrictEqual(events.slice(0, 2), ["gate", "gate"], "remediate produced no checkpoint before the second gate");
      if (interlocked) assert.strictEqual(attempts, 1, "the completion attempt runs once, after proceed");
      assert.deepEqual(events, interlocked ? ["gate", "gate", "checkpoint:reap", "attempt", "code"] : ["gate", "gate", "code"],
        "code gate follows the secret gate and successful interlock without another checkpoint");
    });

    it(`${label}: fail stops with no done checkpoint and no completion attempt`, async () => {
      const { queryFn, prompts } = fakeTurns([
        [submitPlan("plan"), resultSuccess()],
        [signalDone(), resultSuccess()],
      ]);
      const checkpoints: unknown[] = [];
      let attempts = 0;
      let codeCalls = 0;
      const ctx = makeCtx({
        kind: "issue",
        ...(interlocked ? { completionInterlock: true } : {}),
        recordCompletionAttempt: async () => { attempts++; return { unmet: [], attemptCount: attempts }; },
        checkpoint: async (o) => { checkpoints.push(o); },
        secretRemediationGate: async () => ({ action: "fail" }),
        codeCrossCheckGate: async () => { codeCalls++; return { action: "proceed" }; },
      });
      const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
      assert.strictEqual(result.branch, "agent/issue-5", "the run returns a result, no throw");
      assert.strictEqual(prompts.length, 2, "no further turn");
      assert.strictEqual(checkpoints.length, 0, "no done checkpoint");
      assert.strictEqual(attempts, 0, "no completion attempt");
      assert.strictEqual(codeCalls, 0, "no code gate after secret failure");
    });
  }

  for (const reason of ["idle", "stopped"] as const) {
    it(`an interactive run whose park ends ${reason} never calls the gate`, async () => {
      const { queryFn } = fakeTurns([
        [submitPlan("plan"), resultSuccess()],
        [signalDone(), resultSuccess()],
      ]);
      let gates = 0;
      let codeGates = 0;
      const outcome: FollowUpOutcome = { kind: "ended", reason };
      const ctx = makeCtx({
        interactive: true,
        awaitFollowUp: async () => outcome,
        secretRemediationGate: async () => { gates++; return { action: "fail" }; },
        codeCrossCheckGate: async () => { codeGates++; return { action: "proceed" }; },
      });
      const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
      assert.strictEqual(result.branch, "agent/issue-5");
      assert.strictEqual(gates, 0);
      assert.strictEqual(codeGates, 1, "only idle/stopped finalization checks code");
    });
  }

  for (const interlocked of [false, true]) {
    it(`code advisory wait exceeds local wall allowance without a wall park: ${interlocked ? "interlocked" : "legacy"}`, async () => {
      const { queryFn } = fakeTurns([
        [submitPlan("plan"), resultSuccess()],
        [signalDone(), resultSuccess()],
      ]);
      let gates = 0;
      const ctx = makeCtx({
        kind: "issue", config: { run_timeout_seconds: 0.2 },
        completionInterlock: interlocked,
        recordCompletionAttempt: async () => ({ unmet: [], attemptCount: 1 }),
        parkForWall: async () => { assert.fail("advisory wait must not arm wall park"); },
        codeCrossCheckGate: async () => {
          gates++;
          await new Promise<void>((resolve) => setTimeout(resolve, 300));
          return { action: "proceed" };
        },
      });
      const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
      assert.equal(gates, 1);
      assert.equal(result.walled, undefined);
      assert.equal(result.branch, ctx.branch);
    });
  }

  it("unmet completion attempts do not call code until the successful done exit", async () => {
    const { queryFn, prompts } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const events: string[] = [];
    let attempts = 0;
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
      kind: "issue", completionInterlock: true,
      checkpoint: async () => { events.push("checkpoint"); },
      recordCompletionAttempt: async () => {
        events.push("attempt");
        return { unmet: ++attempts === 1 ? ["M1"] : [], attemptCount: attempts };
      },
      codeCrossCheckGate: async () => { events.push("code"); return { action: "proceed" }; },
    }));
    assert.equal(prompts.length, 3);
    assert.deepEqual(events, ["checkpoint", "attempt", "checkpoint", "attempt", "code"]);
  });

  for (const reason of ["idle", "stopped", "cancelled"] as const) {
    it(`interactive multiple follow-ups check code only at finalization: ${reason}`, async () => {
      const { queryFn } = fakeTurns([
        [submitPlan("plan"), resultSuccess()],
        [signalDone(), resultSuccess()],
        [signalDone(), resultSuccess()],
        [signalDone(), resultSuccess()],
      ]);
      let parks = 0;
      let code = 0;
      const operation = new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
        kind: "task", interactive: true,
        awaitFollowUp: async () => {
          assert.equal(code, 0);
          if (++parks <= 2) return { kind: "followup", id: parks, body: "next task" };
          return { kind: "ended", reason };
        },
        codeCrossCheckGate: async () => { code++; return { action: "proceed" }; },
      }));
      if (reason === "cancelled") await assert.rejects(operation, /cancel/i);
      else await operation;
      assert.equal(parks, 3);
      assert.equal(code, reason === "cancelled" ? 0 : 1);
    });
  }

  it("SDK completion forwards report-only metadata to the code gate", async () => {
    const done = signalDone();
    (done as unknown as { message: { content: { input: unknown }[] } }).message.content[0]!.input = { report_only: true };
    const { queryFn } = fakeTurns([[submitPlan("plan"), resultSuccess()], [done, resultSuccess()]]);
    let reportOnly: boolean | undefined;
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
      kind: "issue",
      codeCrossCheckGate: async (completion) => { reportOnly = completion.reportOnly; return { action: "proceed" }; },
    }));
    assert.equal(reportOnly, true);
    assert.equal(result.reportOnly, true);
  });

  it("an absent gate seam leaves the done path unchanged", async () => {
    const { queryFn, prompts } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const checkpoints: unknown[] = [];
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({ checkpoint: async (o) => { checkpoints.push(o); } }));
    assert.strictEqual(result.branch, "agent/issue-5");
    assert.strictEqual(prompts.length, 2);
    // A non-interlocked done path takes no done checkpoint; pin that the absent gate adds none.
    assert.deepStrictEqual(checkpoints, []);
  });
});

describe("SdkExecutor code cross-check on the last allowed iteration", () => {
  for (const interlocked of [false, true]) {
    it(`${interlocked ? "interlocked" : "legacy"}: findings with no iteration left publish without a repair turn or a hold`, async () => {
      const { queryFn, prompts } = fakeTurns([
        [submitPlan("plan"), resultSuccess()],
        [signalDone(), resultSuccess()],
        [signalDone(), resultSuccess()],
      ]);
      const holds: string[] = [];
      const availability: Array<boolean | undefined> = [];
      const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
        kind: "issue", config: { max_iterations: 1 }, completionInterlock: interlocked,
        recordCompletionAttempt: async () => ({ unmet: [], attemptCount: 1 }),
        enterCompletionHold: async (reason) => { holds.push(reason); return true; },
        // Models the real gate: findings lead to a repair only while an iteration remains.
        codeCrossCheckGate: async (completion) => {
          availability.push(completion.repairAvailable);
          return completion.repairAvailable === false || availability.length > 1 ? { action: "proceed" } : { action: "repair", followUp: "REPAIR-MARKER" };
        },
      }));
      assert.deepEqual(holds, []);
      assert.deepEqual(availability, [false]);
      assert.equal(prompts.length, 2, "plan + implement only; no repair turn");
      assert.equal(result.branch, "agent/issue-5");
      assert.equal(result.completionHeld, undefined);
    });
  }
});
