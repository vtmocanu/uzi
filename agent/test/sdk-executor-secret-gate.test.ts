import { afterEach, beforeEach, describe, it, mock } from "node:test";
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

/**
 * Issue #4258 m2 — the advisory, non-terminal mid-run seam ctx.midRunSecretRemediation. The executor
 * calls it at an admitted work-turn boundary only: not interactive, no control pending, no system
 * text (clarification answer, completion rework, code repair, resume preflight) owning the turn.
 * remediate replaces that turn's prompt and adds no turn; proceed changes nothing.
 */
function assistantText(text: string, sessionId = "sess-1"): SDKMessage {
  return { type: "assistant", session_id: sessionId, message: { content: [{ type: "text", text }] } } as unknown as SDKMessage;
}
function askUserTurn(question: string, sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "ta", name: "mcp__uzi__ask_user", input: { questions: [{ question }] } }] },
  } as unknown as SDKMessage;
}
const MID = "MIDRUN-REMEDIATE-MARKER: rewrite the flagged commit";
const work = (): SDKMessage[] => [assistantText("working"), resultSuccess()];
const finish = (): SDKMessage[] => [signalDone(), resultSuccess()];

/** A mid-run seam that models a trigger latched by the first turn: it stays `proceed` until a work
 *  turn has been dispatched, then returns remediate `times` times. Records the prompt count at each call. */
function midSeam(prompts: string[], times = 1): { seam: () => Promise<SecretRemediationDecision>; seenAt: number[] } {
  const seenAt: number[] = [];
  let left = times;
  return {
    seenAt,
    seam: async () => {
      seenAt.push(prompts.length);
      if (prompts.length >= 1 && left > 0) { left--; return { action: "remediate", followUp: MID }; }
      return { action: "proceed" };
    },
  };
}
const runCtx = (overrides: Partial<RunContext> = {}): RunContext => makeCtx({
  kind: "issue", seeded: true, planApproved: true, approvedPlan: "approved", config: { max_iterations: 8 }, ...overrides,
});

describe("SdkExecutor mid-run secret remediation seam (Issue #4258)", () => {
  it("remediate owns exactly the next boundary's turn without signal_done, then work resumes", async () => {
    const { queryFn, prompts } = fakeTurns([work(), work(), finish()]);
    const mid = midSeam(prompts);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(runCtx({ midRunSecretRemediation: () => mid.seam() }));
    assert.equal(prompts.length, 3, "the guidance replaces a boundary's prompt and adds no turn");
    assert.equal(prompts[0]!.includes(MID), false);
    assert.equal(prompts[1]!.includes(MID), true);
    assert.equal(prompts[2]!.includes(MID), false);
    assert.equal(mid.seenAt[0], 0, "the first boundary only asks; nothing is latched before a work turn");
  });

  it("an owner follow-up queued at that boundary rides the following turn, receipt after it", async () => {
    const { queryFn, prompts } = fakeTurns([work(), work(), finish()]);
    const mid = midSeam(prompts);
    const OWNER = "OWNER-FOLLOWUP-MARKER-4258";
    let pulled = false;
    const included: number[] = [];
    const promptsAtReceipt: number[] = [];
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(runCtx({
      midRunSecretRemediation: () => mid.seam(),
      pullFollowUps: () => (prompts.length >= 1 && !pulled ? (pulled = true, [{ id: 7, body: OWNER }]) : []),
      followUpIncluded: id => { included.push(id); promptsAtReceipt.push(prompts.length); },
    }));
    assert.equal(prompts.length, 3);
    assert.equal(prompts[1]!.includes(MID), true);
    assert.equal(prompts[1]!.includes(OWNER), false, "system guidance owns its turn; the follow-up waits");
    assert.equal(prompts[2]!.includes(OWNER), true);
    assert.deepEqual(included, [7]);
    assert.deepEqual(promptsAtReceipt, [3], "receipt follows the turn that carried it");
  });

  it("proceed leaves every prompt byte-identical to a run without the seam", async () => {
    const base = fakeTurns([work(), work(), finish()]);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn: base.queryFn }).run(runCtx());
    const withSeam = fakeTurns([work(), work(), finish()]);
    let calls = 0;
    await new SdkExecutor(nullLogger(), homeDir, { queryFn: withSeam.queryFn }).run(runCtx({
      midRunSecretRemediation: async () => { calls++; return { action: "proceed" }; },
    }));
    assert.ok(calls >= 2);
    assert.deepEqual(withSeam.prompts, base.prompts);
  });

  it("an interactive run never calls the seam", async () => {
    const { queryFn, prompts } = fakeTurns([work(), finish()]);
    let calls = 0;
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
      kind: "task", interactive: true, seeded: true, planApproved: true, approvedPlan: "approved",
      awaitFollowUp: async () => ({ kind: "ended", reason: "idle" }),
      midRunSecretRemediation: async () => { calls++; return { action: "remediate", followUp: MID }; },
    }));
    assert.equal(calls, 0);
    assert.ok(prompts.every(p => !p.includes(MID)));
    const control = fakeTurns([work(), finish()]);
    let controlCalls = 0;
    await new SdkExecutor(nullLogger(), homeDir, { queryFn: control.queryFn }).run(runCtx({
      midRunSecretRemediation: async () => { controlCalls++; return { action: "proceed" }; },
    }));
    assert.ok(controlCalls >= 1, "control: the same shape non-interactively does consult the seam");
  });

  const controls = ["cancel", "shutdown", "pause", "wall", "switch"] as const;
  type Control = typeof controls[number];
  /** Wires one control onto a ctx; `fire` raises it. */
  function controlRig(control: Control): { overrides: Partial<RunContext>; fire: () => void; switches: () => number } {
    let cancelled = false;
    let mode: "wall" | "now" | null = null;
    let pause: (() => void) | undefined;
    let credentialSwitch: (() => void) | undefined;
    let switches = 0;
    const abort = new AbortController();
    return {
      switches: () => switches,
      overrides: {
        ...(control === "shutdown" ? { signal: abort.signal } : {}),
        cancelRequested: () => cancelled,
        pauseModeRequested: () => mode,
        onPauseNow: cb => { pause = cb; },
        onCredentialSwitch: cb => { credentialSwitch = cb; },
        attemptCredentialSwitch: async () => { switches++; return "released"; },
        parkForWall: async () => "parked",
        parkForPause: async () => true,
      },
      fire: () => {
        if (control === "cancel") cancelled = true;
        else if (control === "shutdown") abort.abort(new Error("shutdown"));
        else if (control === "pause") { mode = "now"; assert.ok(pause); pause(); }
        else if (control === "wall") { mode = "wall"; assert.ok(pause); pause(); }
        else { assert.ok(credentialSwitch); credentialSwitch(); }
      },
    };
  }
  async function settle(control: Control, running: Promise<unknown>, rig: { switches: () => number }): Promise<void> {
    if (control === "cancel" || control === "shutdown") await assert.rejects(running, /cancel|shutdown/i);
    else {
      const result = await running as { walled?: unknown; pausedAt?: unknown; switchReleased?: boolean };
      if (control === "pause") assert.ok(result.pausedAt);
      else if (control === "wall") assert.ok(result.walled);
      else { assert.equal(rig.switches(), 1); assert.equal(result.switchReleased, true); }
    }
  }

  for (const control of controls) {
    it(`${control} pending at the boundary is honoured and the seam is not called there`, async () => {
      const { queryFn, prompts } = fakeTurns([work(), work(), finish()]);
      const rig = controlRig(control);
      let reports = 0;
      const calledAfterFire: number[] = [];
      let calledBeforeFire = 0;
      const running = new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(runCtx({
        ...rig.overrides,
        reportIteration: async () => {
          if (++reports === 2) rig.fire();
          // An owner pause-now reaches the boundary through the running-report ACK.
          return control === "pause" && reports >= 2 ? { completionAttempts: 0, pauseRequested: true } : { completionAttempts: 0 };
        },
        midRunSecretRemediation: async () => {
          if (reports >= 2) calledAfterFire.push(prompts.length); else calledBeforeFire++;
          return prompts.length >= 1 ? { action: "remediate", followUp: MID } : { action: "proceed" };
        },
      }));
      await settle(control, running, rig);
      assert.ok(calledBeforeFire >= 1, "control: the seam is consulted at an uncontrolled boundary");
      assert.deepEqual(calledAfterFire, []);
      assert.ok(prompts.every(p => !p.includes(MID)), "no model turn carries the guidance");
    });

    it(`${control} arriving during the seam's await wins before any model event`, async () => {
      const { queryFn, prompts } = fakeTurns([work(), work(), finish()]);
      const rig = controlRig(control);
      let entered!: () => void;
      const inSeam = new Promise<void>(resolve => { entered = resolve; });
      let release!: () => void;
      const gate = new Promise<void>(resolve => { release = resolve; });
      const running = new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(runCtx({
        ...rig.overrides,
        midRunSecretRemediation: async () => {
          if (prompts.length === 0) return { action: "proceed" };
          entered();
          await gate;
          return { action: "remediate", followUp: MID };
        },
      }));
      await Promise.race([inSeam, running.then(() => assert.fail("the seam was never called"))]);
      rig.fire();
      release();
      await settle(control, running, rig);
      assert.equal(prompts.length, 1, "only the first work turn ran");
      assert.ok(prompts.every(p => !p.includes(MID)));
    });
  }

  /** Holds the seam open after the first work turn, runs `during`, then releases it with a `proceed`
   *  (so the owner follow-up pull, which a remediate would skip, is observable) or a remediate.
   *  `pullsAfter()` counts owner follow-up pulls made after `during` ran. */
  async function arriveDuringAwait(
    during: () => void, overrides: Partial<RunContext>, decision: SecretRemediationDecision = { action: "remediate", followUp: MID },
  ): Promise<{ running: Promise<unknown>; prompts: string[]; pullsAfter: () => number }> {
    const { queryFn, prompts } = fakeTurns([work(), work(), finish()]);
    let fired = false;
    let pulls = 0;
    let entered!: () => void;
    const inSeam = new Promise<void>(resolve => { entered = resolve; });
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    const running = new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(runCtx({
      ...overrides,
      pullFollowUps: () => { if (fired) pulls++; return []; },
      midRunSecretRemediation: async () => {
        if (prompts.length === 0) return { action: "proceed" };
        entered();
        await gate;
        return decision;
      },
    }));
    const settled = running.then(() => undefined, () => undefined);
    await Promise.race([inSeam, settled.then(() => assert.fail("the seam was never called"))]);
    fired = true;
    during();
    release();
    return { running, prompts, pullsAfter: () => pulls };
  }

  const stopCases: Array<{ name: string; wire: () => { during: () => void; overrides: Partial<RunContext> }; rejects: (e: unknown) => boolean }> = [
    {
      name: "a terminal fence",
      wire: () => { let fenced = false; return { during: () => { fenced = true; }, overrides: { claimFenced: () => fenced } }; },
      rejects: e => e instanceof Error && /cancel/i.test(e.message),
    },
    {
      name: "a terminal lifecycle signal",
      wire: () => {
        const lifecycle = new AbortController();
        return { during: () => lifecycle.abort(new Error("terminal")), overrides: { terminalLifecycleSignal: lifecycle.signal } };
      },
      rejects: e => e instanceof Error && /cancel/i.test(e.message),
    },
    {
      name: "a disk stop",
      wire: () => {
        let mode: "disk" | null = null;
        return { during: () => { mode = "disk"; }, overrides: { pauseModeRequested: () => mode } };
      },
      rejects: e => e instanceof Error && e.constructor.name === "DiskParkSignal",
    },
  ];
  for (const stop of stopCases) {
    for (const decision of [{ action: "remediate", followUp: MID }, { action: "proceed" }] as SecretRemediationDecision[]) {
      it(`${stop.name} arriving during the seam's await (${decision.action}) stops the run before any further turn or follow-up pull`, async () => {
        const wired = stop.wire();
        const { running, prompts, pullsAfter } = await arriveDuringAwait(wired.during, wired.overrides, decision);
        await assert.rejects(running, stop.rejects);
        assert.equal(prompts.length, 1, "only the first work turn ran");
        assert.ok(prompts.every(p => !p.includes(MID)));
        assert.equal(pullsAfter(), 0, "the owner follow-up queue is untouched after the stop");
      });
    }
  }

  type Owner = { name: string; scripts: SDKMessage[][]; ctx: () => Partial<RunContext>; ownerPromptMarker: string };
  const owners: Owner[] = [
    {
      name: "a clarification answer",
      scripts: [[askUserTurn("which one?"), resultSuccess()], work(), work(), finish()],
      ctx: () => ({ askUser: async () => ({ kind: "answer", answers: ["CLARIFY-ANSWER-MARKER"] }) }),
      ownerPromptMarker: "CLARIFY-ANSWER-MARKER",
    },
    {
      name: "a completion rework",
      scripts: [finish(), work(), work(), finish()],
      ctx: () => {
        let attempts = 0;
        return {
          completionInterlock: true, checkpoint: async () => {},
          recordCompletionAttempt: async () => ({ unmet: ++attempts === 1 ? ["M1"] : [], attemptCount: attempts }),
        };
      },
      ownerPromptMarker: "M1",
    },
    {
      name: "a code-repair follow-up",
      scripts: [finish(), work(), work(), finish()],
      ctx: () => {
        let checks = 0;
        return { codeCrossCheckGate: async () => (++checks === 1 ? { action: "repair", followUp: "REPAIR-MARKER-4258" } : { action: "proceed" }) };
      },
      ownerPromptMarker: "REPAIR-MARKER-4258",
    },
  ];
  for (const owner of owners) {
    it(`${owner.name} owns the turn, the seam is not called that boundary, and the trigger stays pending`, async () => {
      const { queryFn, prompts } = fakeTurns(owner.scripts);
      const mid = midSeam(prompts);
      await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(runCtx({ ...owner.ctx(), midRunSecretRemediation: () => mid.seam() }));
      assert.ok(prompts[1]!.includes(owner.ownerPromptMarker), "the system text owns the first boundary's turn");
      assert.equal(prompts[1]!.includes(MID), false);
      assert.equal(mid.seenAt.includes(1), false, "not called at the boundary the text owns");
      assert.ok(mid.seenAt.includes(2), "the pending trigger is consumed at the next free boundary");
      assert.equal(prompts[2]!.includes(MID), true);
    });
  }

  it("a resume preflight remediate at the same boundary suppresses the mid-run call there only", async () => {
    const { queryFn, prompts } = fakeTurns([work(), work()]);
    const PRE = "PREFLIGHT-REMEDIATE-MARKER";
    const seenAt: number[] = [];
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({
      kind: "issue", resumed: true, sessionId: null, seeded: true, planApproved: true, approvedPlan: "approved",
      config: { max_iterations: 2, iteration_park_version: 1 },
      reportIteration: async () => ({ completionAttempts: 0 }), parkForIteration: async () => "parked",
      secretRemediationGate: async () => ({ action: "remediate", followUp: PRE }),
      midRunSecretRemediation: async () => { seenAt.push(prompts.length); return { action: "remediate", followUp: MID }; },
    }));
    assert.equal(prompts.length, 2);
    assert.ok(prompts[0]!.includes(PRE) && !prompts[0]!.includes(MID));
    assert.deepEqual(seenAt, [1], "called only at the next boundary, after the preflight text was spent");
    assert.ok(prompts[1]!.includes(MID));
  });

  it("an exhausted wall at the boundary skips the seam, so no attempt is spent and the run ends at the wall", async () => {
    // The wall is debited as the first turn's Date.now() delta (disarmWall). The test clock is
    // advanced by 10s inside that turn, against a 1s run_timeout_seconds, so the real wall timer
    // (1s away) never fires in-turn: the turn completes normally and leaves wallRemainingMs <= 0
    // with no trip latched. The next boundary is then the one the seam guard must refuse.
    const realNow = Date.now.bind(Date);
    let skew = 0;
    const clock = mock.method(Date, "now", () => realNow() + skew);
    try {
      const turns: SDKMessage[][] = [work(), work(), finish()];
      let i = 0;
      const prompts: string[] = [];
      const queryFn: SdkQueryFn = (params) => {
        const script = turns[Math.min(i++, turns.length - 1)]!;
        return (async function* () {
          let text = "";
          for await (const m of params.prompt) text += JSON.stringify(m);
          prompts.push(text);
          if (prompts.length === 1) skew = 10_000;
          for (const m of script) yield m;
        })();
      };
      let calls = 0;
      let firstBoundary = 0;
      let latched = true; // the fake's trigger/attempts state: must be untouched
      await assert.rejects(
        new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(runCtx({
          config: { max_iterations: 8, run_timeout_seconds: 1 },
          midRunSecretRemediation: async () => {
            // The first boundary (before any turn) legitimately consults the seam: the wall is intact.
            if (prompts.length === 0) { firstBoundary++; return { action: "proceed" }; }
            calls++;
            latched = false;
            return { action: "remediate", followUp: MID };
          },
        })),
        /wall-clock timeout/,
      );
      assert.equal(firstBoundary, 1, "control: the seam is consulted while the wall is intact");
      assert.equal(calls, 0, "the seam must not run when the wall is already exhausted");
      assert.equal(latched, true, "no remediation attempt was spent and the trigger was not cleared");
      assert.equal(prompts.length, 1, "no further turn was dispatched");
      assert.ok(prompts.every(p => !p.includes(MID)));
    } finally {
      clock.mock.restore();
    }
  });
});
