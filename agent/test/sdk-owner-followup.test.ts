import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { RunContext, SecretRemediationDecision } from "../src/executor.js";
import { PauseNowSignal } from "../src/steering.js";
import type { AskUserQuestion } from "../src/protocol.js";
import { nonexistentWorktreeFactory, nullLogger } from "./helpers.js";

// Issue #1800: the Claude executor's OWNER follow-up slot. A queued owner follow-up is pulled
// (ctx.pullFollowUp) only immediately before an ordinary implement prompt is built, rides that
// prompt, and is reported included (ctx.followUpIncluded) exactly once, when the turn carrying
// it yields its FIRST event. A system text (completion rework, clarification answer, secret
// remediation) owns its turn outright: the follow-up waits in its slot for the next ordinary
// turn. A turn that is dropped before streaming (a declined `now` pause) keeps the follow-up.
//
// Same harness shape as checkpoint-followup-drain.test.ts: `queryFn` is faked per invocation
// with scripted SDK messages; dummy credentials only.

const OAUTH = "dummy-oauth-token-do-not-scan-0000";
const FAKE_PAT = "dummy-forge-pat-do-not-scan-1111";
const FAKE_JOIN_TOKEN = "dummy-join-token-do-not-scan-2222";

const nonexistentWorktree = nonexistentWorktreeFactory("uzi-owner-followup");

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
function askUser(questions: AskUserQuestion[], sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t3", name: "mcp__uzi__ask_user", input: { questions } }] },
  } as unknown as SDKMessage;
}
function resultSuccess(sessionId = "sess-1"): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: sessionId } as unknown as SDKMessage;
}

interface ScriptedTurn {
  messages: SDKMessage[];
  /** Follow-ups that arrive on the queue while this invocation runs (after its prompt was read). */
  arrive?: string[];
}

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

interface Probe {
  ctx: RunContext;
  /** What each pullFollowUp call returned (undefined when the queue was empty). */
  pulls: Array<string | undefined>;
  /** Ids passed to ctx.followUpIncluded, in call order. */
  included: number[];
}

function makeCtx(queue: string[], overrides: Partial<RunContext> = {}): Probe {
  const pulls: Array<string | undefined> = [];
  const included: number[] = [];
  let nextId = 1;
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
      const body = queue.shift();
      pulls.push(body);
      return body === undefined ? undefined : { id: nextId++, body };
    },
    followUpIncluded: (id) => included.push(id),
    checkpoint: async () => {},
    reportIteration: async () => undefined,
    ...overrides,
  };
  return { ctx, pulls, included };
}

const PLAN: ScriptedTurn = { messages: [submitPlan("# Plan"), resultSuccess()] };
const WORK: ScriptedTurn = { messages: [assistantText("working"), resultSuccess()] };
const DONE: ScriptedTurn = { messages: [assistantText("finished"), signalDone(), resultSuccess()] };

const A = "FOLLOWUP-MARKER-ALPHA-7f3a";
const SYSTEM_TEXT = "SYSTEM-REMEDIATION-TEXT-91bc";

let homeDir: string;
let saved: Record<string, string | undefined>;

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-ownerfuhome-"));
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

describe("issue #1800: the Claude executor delivers and reports the owner follow-up", () => {
  it("puts a follow-up queued before the implement loop (the plan gate) into the FIRST implement prompt", async () => {
    // Before the fix the follow-up was pulled only at the end of a turn, so the first implement
    // prompt never carried one that arrived while the run sat at the plan gate.
    const queue = [A];
    const { queryFn, prompts } = fakeTurns([PLAN, WORK, DONE], queue);
    const probe = makeCtx(queue);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.ok(prompts[1]!.includes(A), "the first implement prompt carries the follow-up");
    assert.ok(prompts[1]!.includes("<follow_up>"), "framed as untrusted input");
    assert.ok(!prompts[2]!.includes(A), "no replay on the next turn");
    assert.deepStrictEqual(probe.included, [1], "reported included exactly once");
  });

  it("reports a gate follow-up included even when the run finishes in that one turn", async () => {
    // Before the fix a follow-up on a one-turn run was never delivered at all.
    const queue = [A];
    const { queryFn, prompts } = fakeTurns([PLAN, DONE], queue);
    const probe = makeCtx(queue);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.strictEqual(prompts.length, 2, "planning turn + one implement turn");
    assert.ok(prompts[1]!.includes(A));
    assert.deepStrictEqual(probe.included, [1]);
  });

  it("does not report a follow-up that arrives during the final turn (it never rode a prompt)", async () => {
    const queue: string[] = [];
    const { queryFn, prompts } = fakeTurns([PLAN, { messages: DONE.messages, arrive: [A] }], queue);
    const probe = makeCtx(queue);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.ok(!prompts.some((p) => p.includes(A)), "no prompt carried it");
    assert.deepStrictEqual(probe.included, [], "nothing was reported, so the next claim re-queues it");
    assert.deepStrictEqual(queue, [A], "it was never pulled out of the queue");
  });

  it("keeps the follow-up when a turn is dropped before streaming (declined now pause), then delivers it once", async () => {
    const queue = [A];
    const { queryFn, prompts } = fakeTurns([PLAN, DONE], queue);
    const cancel = new AbortController();
    let aborted = false;
    const probe = makeCtx(queue, {
      signal: cancel.signal,
      parkForPause: async () => false, // declined: the loop restarts the dropped turn
      reportIteration: async (n) => {
        if (n === 1 && !aborted) {
          aborted = true;
          cancel.abort(new PauseNowSignal());
        }
        return { pauseRequested: false, completedCount: 0 };
      },
    });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    // The dropped turn threw at driveTurn's trip guard before queryFn, so it consumed no script.
    assert.strictEqual(prompts.length, 2, "planning turn + the restarted implement turn");
    assert.ok(prompts[1]!.includes(A), "the restarted turn carries the follow-up the dropped turn held");
    assert.deepStrictEqual(probe.pulls, [A], "pulled once, never re-pulled");
    assert.deepStrictEqual(probe.included, [1], "reported once, by the turn that really started");
  });

  it("a secret-remediation turn does not carry the follow-up; the next ordinary turn does", async () => {
    const queue: string[] = [];
    const { queryFn, prompts } = fakeTurns(
      [PLAN, { messages: DONE.messages, arrive: [A] }, WORK, DONE],
      queue,
    );
    const decisions: SecretRemediationDecision[] = [{ action: "remediate", followUp: SYSTEM_TEXT }];
    const probe = makeCtx(queue, { secretRemediationGate: async () => decisions.shift() ?? { action: "proceed" } });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.ok(prompts[2]!.includes(SYSTEM_TEXT), "turn 2 is the remediation turn");
    assert.ok(!prompts[2]!.includes(A), "the remediation turn does not carry the follow-up");
    assert.ok(prompts[3]!.includes(A) && !prompts[3]!.includes(SYSTEM_TEXT), "the next ordinary turn carries it");
    assert.deepStrictEqual(probe.pulls, [undefined, A], "one pull per ORDINARY turn: none while the system text owned turn 2");
    assert.deepStrictEqual(probe.included, [1]);
  });

  it("a completion-rework turn does not carry the follow-up; the next ordinary turn does", async () => {
    const queue: string[] = [];
    const { queryFn, prompts } = fakeTurns(
      [PLAN, { messages: DONE.messages, arrive: [A] }, WORK, DONE],
      queue,
    );
    const unmet = [["m-open"], []];
    const probe = makeCtx(queue, {
      completionInterlock: true,
      worktreeFingerprint: async () => "tip\n",
      recordCompletionAttempt: async () => ({ unmet: unmet.shift() ?? [], attemptCount: 1 }),
    });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.match(prompts[2]!, /Completion check/, "turn 2 is the rework turn");
    assert.ok(!prompts[2]!.includes(A), "the rework turn does not carry the follow-up");
    assert.ok(prompts[3]!.includes(A), "the next ordinary turn carries it");
    assert.deepStrictEqual(probe.included, [1]);
  });

  it("a clarification-answer turn does not carry the follow-up; the next ordinary turn does", async () => {
    const queue: string[] = [];
    const questions: AskUserQuestion[] = [{ question: "Which database?", header: "DB" }];
    const { queryFn, prompts } = fakeTurns(
      [PLAN, { messages: [askUser(questions), resultSuccess()], arrive: [A] }, WORK, DONE],
      queue,
    );
    const probe = makeCtx(queue, { askUser: async () => ({ kind: "answer", answers: ["postgres"] }) });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.match(prompts[2]!, /postgres/, "turn 2 carries the answer");
    assert.ok(!prompts[2]!.includes(A), "the answer turn does not carry the follow-up");
    assert.ok(prompts[3]!.includes(A), "the next ordinary turn carries it");
    assert.deepStrictEqual(probe.included, [1]);
  });

  it("reports an interactive park's follow-up when the turn that carries it starts, not at the park", async () => {
    const queue: string[] = [];
    const { queryFn, prompts } = fakeTurns([PLAN, DONE, DONE], queue);
    const outcomes = [
      { kind: "followup" as const, id: 77, body: A },
      { kind: "ended" as const, reason: "idle" as const },
    ];
    let includedAtPark: number[] | undefined;
    const probe = makeCtx(queue, {
      interactive: true,
      awaitFollowUp: async () => {
        const o = outcomes.shift()!;
        if (o.kind === "followup") includedAtPark = [...probe.included];
        return o;
      },
    });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.deepStrictEqual(includedAtPark, [], "nothing is reported while the park hands the follow-up over");
    assert.ok(prompts[2]!.includes(A), "the resumed turn carries the follow-up");
    assert.deepStrictEqual(probe.included, [77], "reported with the park's input id once that turn started");
  });
});
