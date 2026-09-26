import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { RunContext } from "../src/executor.js";
import { nullLogger } from "./helpers.js";

// Issue #1152: the implement loop's queued follow-up drain (`ctx.pullFollowUp`) must run
// at a cooperative-checkpoint boundary too. Before the fix the checkpoint branch
// `continue`d past the only drain, so (1) a follow-up queued during a checkpointing turn
// starved until some later non-checkpoint turn, and (2) the follow-up the checkpointing
// turn itself carried was REPLAYED into the next prompt.
//
// Same harness shape as scope-ceiling-honor.test.ts: `queryFn` is faked per invocation,
// the MCP signals are scripted tool_use blocks, and follow-ups are pushed onto the queue
// from INSIDE the fake query for a given invocation, modelling a steer that arrives while
// that turn is running. Deterministic, no timers, dummy credentials only.

const OAUTH = "dummy-oauth-token-do-not-scan-0000";
const FAKE_PAT = "dummy-forge-pat-do-not-scan-1111";
const FAKE_JOIN_TOKEN = "dummy-join-token-do-not-scan-2222";

// Per-process, per-call unique non-existent worktree (see the race note in
// sdk-executor.test.ts: the executor materializes a sibling `.uzi-skills-<basename>`).
let nonexistentWorktreeSeq = 0;
function nonexistentWorktree(): string {
  return path.join(os.tmpdir(), `uzi-ckpt-followup-wt-${process.pid}-${nonexistentWorktreeSeq++}`);
}

function assistantText(text: string, sessionId = "sess-1"): SDKMessage {
  return { type: "assistant", session_id: sessionId, message: { content: [{ type: "text", text }] } } as unknown as SDKMessage;
}
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
function checkpointSignal(sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "tc", name: "mcp__uzi__checkpoint", input: {} }] },
  } as unknown as SDKMessage;
}
function resultSuccess(sessionId = "sess-1"): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: sessionId } as unknown as SDKMessage;
}

interface ScriptedTurn {
  messages: SDKMessage[];
  /** Follow-ups that arrive on the queue while this invocation runs. */
  arrive?: string[];
}

/**
 * A fake `query` replaying one scripted stream per invocation (0 = planning turn,
 * n = implement iteration n). It records each invocation's prompt text and pushes that
 * invocation's `arrive` follow-ups onto `queue` mid-invocation, after the prompt was read.
 */
function fakeTurns(scripts: ScriptedTurn[], queue: string[]): { queryFn: SdkQueryFn; prompts: string[] } {
  const prompts: string[] = [];
  let i = 0;
  const queryFn: SdkQueryFn = (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    const idx = i++;
    prompts.push("");
    return (async function* () {
      for await (const p of params.prompt) {
        const content = (p as { message?: { content?: unknown } }).message?.content;
        prompts[idx] = typeof content === "string" ? content : JSON.stringify(content);
      }
      for (const f of script.arrive ?? []) queue.push(f);
      for (const m of script.messages) yield m;
    })();
  };
  return { queryFn, prompts };
}

function makeCtx(queue: string[]): { ctx: RunContext; pulls: Array<string | undefined> } {
  const pulls: Array<string | undefined> = [];
  const ctx: RunContext = {
    runId: "r1",
    issueIid: 5,
    issueTitle: "Fix login",
    issueDescription: "please implement",
    worktreePath: nonexistentWorktree(),
    branch: "agent/issue-5",
    emit: () => {},
    oauthToken: OAUTH,
    agents: [],
    config: null,
    sessionId: null,
    gatePlan: async () => ({ kind: "approve", selection: { status: "absent" } }),
    pullFollowUp: () => {
      const next = queue.shift();
      pulls.push(next);
      return next;
    },
    checkpoint: async () => {},
    reportIteration: async () => undefined,
  };
  return { ctx, pulls };
}

const PLAN: ScriptedTurn = { messages: [submitPlan("# Plan"), resultSuccess()] };
const DONE: ScriptedTurn = { messages: [assistantText("finished"), signalDone(), resultSuccess()] };

const A = "FOLLOWUP-MARKER-ALPHA-7f3a";
const B = "FOLLOWUP-MARKER-BRAVO-2c9e";
const F = "FOLLOWUP-MARKER-FOXTROT-81d0";

let homeDir: string;
let saved: Record<string, string | undefined>;

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-ckptfuhome-"));
  saved = { UZI_WORKER_TOKEN: process.env.UZI_WORKER_TOKEN, UZI_FORGE_PAT: process.env.UZI_FORGE_PAT };
  process.env.UZI_WORKER_TOKEN = FAKE_JOIN_TOKEN;
  process.env.UZI_FORGE_PAT = FAKE_PAT;
});

afterEach(() => {
  fs.rmSync(homeDir, { recursive: true, force: true });
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
});

describe("issue #1152: queued follow-ups drain at a cooperative checkpoint", () => {
  it("delivers a follow-up queued during a checkpointing turn to the next turn", async () => {
    const queue: string[] = [];
    const { queryFn, prompts } = fakeTurns(
      [PLAN, { messages: [assistantText("iter 1"), checkpointSignal(), resultSuccess()], arrive: [A] }, DONE],
      queue,
    );
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx(queue).ctx);
    assert.strictEqual(prompts.length, 3, "planning turn + two implement turns");
    assert.ok(!prompts[1]!.includes(A), "iteration 1 predates the follow-up");
    assert.ok(prompts[2]!.includes(A), "iteration 2 carries the follow-up queued during the checkpoint turn");
    assert.deepStrictEqual(queue, [], "the queue is fully drained");
  });

  it("drains successive checkpoints one follow-up per turn, in FIFO order", async () => {
    const queue: string[] = [];
    const { queryFn, prompts } = fakeTurns(
      [
        PLAN,
        { messages: [assistantText("iter 1"), checkpointSignal(), resultSuccess()], arrive: [A, B] },
        { messages: [assistantText("iter 2"), checkpointSignal(), resultSuccess()] },
        { messages: [assistantText("iter 3"), resultSuccess()] },
        DONE,
      ],
      queue,
    );
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx(queue).ctx);
    assert.strictEqual(prompts.length, 5, "planning turn + four implement turns");
    assert.ok(!prompts[1]!.includes(A) && !prompts[1]!.includes(B), "iteration 1 carries neither");
    assert.ok(prompts[2]!.includes(A) && !prompts[2]!.includes(B), "iteration 2 carries A only");
    assert.ok(prompts[3]!.includes(B) && !prompts[3]!.includes(A), "iteration 3 carries B only");
    assert.ok(!prompts[4]!.includes(A) && !prompts[4]!.includes(B), "iteration 4 carries neither");
  });

  it("does not replay the follow-up a checkpointing turn already carried", async () => {
    const queue: string[] = [];
    const { queryFn, prompts } = fakeTurns(
      [
        PLAN,
        { messages: [assistantText("iter 1"), resultSuccess()], arrive: [F] },
        { messages: [assistantText("iter 2"), checkpointSignal(), resultSuccess()] },
        DONE,
      ],
      queue,
    );
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx(queue).ctx);
    assert.strictEqual(prompts.length, 4, "planning turn + three implement turns");
    assert.ok(!prompts[1]!.includes(F), "iteration 1 predates the follow-up");
    assert.ok(prompts[2]!.includes(F), "iteration 2 carries the follow-up");
    assert.ok(!prompts[3]!.includes(F), "iteration 3 (after a checkpoint) does not replay it");
  });

  it("delivers queued follow-ups one per ordinary (non-checkpoint) turn, in order", async () => {
    const queue: string[] = [];
    const { queryFn, prompts } = fakeTurns(
      [
        PLAN,
        { messages: [assistantText("iter 1"), resultSuccess()], arrive: [A, B] },
        { messages: [assistantText("iter 2"), resultSuccess()] },
        DONE,
      ],
      queue,
    );
    const { ctx, pulls } = makeCtx(queue);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(prompts.length, 4, "planning turn + three implement turns");
    assert.ok(prompts[2]!.includes(A) && !prompts[2]!.includes(B), "iteration 2 carries the first only");
    assert.ok(prompts[3]!.includes(B) && !prompts[3]!.includes(A), "iteration 3 carries the second only");
    assert.deepStrictEqual(pulls, [A, B], "one dequeue per ordinary turn boundary");
  });
});
