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
    pullFollowUp: () => undefined,
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
    });

    it(`${label}: fail stops with no done checkpoint and no completion attempt`, async () => {
      const { queryFn, prompts } = fakeTurns([
        [submitPlan("plan"), resultSuccess()],
        [signalDone(), resultSuccess()],
      ]);
      const checkpoints: unknown[] = [];
      let attempts = 0;
      const ctx = makeCtx({
        kind: "issue",
        ...(interlocked ? { completionInterlock: true } : {}),
        recordCompletionAttempt: async () => { attempts++; return { unmet: [], attemptCount: attempts }; },
        checkpoint: async (o) => { checkpoints.push(o); },
        secretRemediationGate: async () => ({ action: "fail" }),
      });
      const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
      assert.strictEqual(result.branch, "agent/issue-5", "the run returns a result, no throw");
      assert.strictEqual(prompts.length, 2, "no further turn");
      assert.strictEqual(checkpoints.length, 0, "no done checkpoint");
      assert.strictEqual(attempts, 0, "no completion attempt");
    });
  }

  for (const reason of ["idle", "stopped"] as const) {
    it(`an interactive run whose park ends ${reason} never calls the gate`, async () => {
      const { queryFn } = fakeTurns([
        [submitPlan("plan"), resultSuccess()],
        [signalDone(), resultSuccess()],
      ]);
      let gates = 0;
      const outcome: FollowUpOutcome = { kind: "ended", reason };
      const ctx = makeCtx({
        interactive: true,
        awaitFollowUp: async () => outcome,
        secretRemediationGate: async () => { gates++; return { action: "fail" }; },
      });
      const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
      assert.strictEqual(result.branch, "agent/issue-5");
      assert.strictEqual(gates, 0);
    });
  }

  it("an absent gate seam leaves the done path unchanged", async () => {
    const { queryFn, prompts } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const checkpoints: unknown[] = [];
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx({ checkpoint: async (o) => { checkpoints.push(o); } }));
    assert.strictEqual(result.branch, "agent/issue-5");
    assert.strictEqual(prompts.length, 2);
  });
});
