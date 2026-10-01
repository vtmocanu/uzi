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
// it first yields an event evidencing the model processed it (not init/lifecycle). A system text (completion rework, clarification answer, secret
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

function initFrame(sessionId = "sess-1"): SDKMessage {
  return { type: "system", subtype: "init", session_id: sessionId, model: "m" } as unknown as SDKMessage;
}
function rateLimitEvent(status: string, sessionId = "sess-1"): SDKMessage {
  return {
    type: "rate_limit_event",
    session_id: sessionId,
    rate_limit_info: { status, resetsAt: 1_900_000_000, rateLimitType: "five_hour" },
  } as unknown as SDKMessage;
}
/** The SDK's worker-synthesized assistant notice (e.g. the usage-limit message): never model output. */
function syntheticAssistant(text: string, sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { model: "<synthetic>", content: [{ type: "text", text }] },
  } as unknown as SDKMessage;
}
function resultErrorOneTurn(subtype: string, sessionId = "sess-1"): SDKMessage {
  return { type: "result", subtype, is_error: true, num_turns: 1, session_id: sessionId } as unknown as SDKMessage;
}
function resultZeroTurns(sessionId = "sess-1"): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 0, session_id: sessionId } as unknown as SDKMessage;
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
    assert.ok(/<follow_up_[0-9a-f]{16}>/.test(prompts[1]!), "framed as untrusted input");
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
    assert.deepStrictEqual(probe.included, [77], "reported with the park's input id once that turn reached the model");
  });
});

// The stamp boundary: "included" means the turn carrying the follow-up reached the MODEL, so a
// turn that starts and dies before any model output (init, rate_limit_event, a zero-turn result,
// a throw) must leave the follow-up unreported and held for the next turn.
describe("issue #1800: a follow-up is stamped only once the model processed its turn", () => {
  const exec = (queryFn: SdkQueryFn, extra: Record<string, number> = {}): SdkExecutor =>
    new SdkExecutor(nullLogger(), homeDir, { queryFn, emptyTurnBackoffBaseMs: 0, ...extra });

  it("an init-only empty turn is retried and the follow-up is stamped by the retry that streamed output", async () => {
    const queue = [A];
    const { queryFn, prompts } = fakeTurns(
      [PLAN, { messages: [initFrame(), resultZeroTurns()] }, { messages: [initFrame(), ...DONE.messages] }],
      queue,
    );
    const probe = makeCtx(queue);
    const invocationsAtStamp: number[] = [];
    const base = probe.ctx.followUpIncluded!;
    probe.ctx.followUpIncluded = (id) => {
      invocationsAtStamp.push(prompts.length);
      base(id);
    };
    await exec(queryFn, { emptyTurnMaxRetries: 2 }).run(probe.ctx);
    assert.ok(prompts[1]!.includes(A) && prompts[2]!.includes(A), "the empty turn and its retry both carry it");
    assert.deepStrictEqual(probe.included, [1], "stamped exactly once");
    assert.deepStrictEqual(invocationsAtStamp, [3], "stamped by the third query (the retry), not the init-only second");
    assert.deepStrictEqual(probe.pulls, [A], "pulled once");
  });

  it("an exhausted empty turn (init, rate_limit_event, zero turns) leaves the follow-up unreported", async () => {
    const queue = [A];
    const { queryFn, prompts } = fakeTurns(
      [PLAN, { messages: [initFrame(), rateLimitEvent("allowed"), resultZeroTurns()] }],
      queue,
    );
    const probe = makeCtx(queue);
    await assert.rejects(exec(queryFn, { emptyTurnMaxRetries: 0 }).run(probe.ctx));
    assert.ok(prompts[1]!.includes(A), "the turn was started with the follow-up in its prompt");
    assert.deepStrictEqual(probe.included, [], "never stamped: the next claim re-queues it");
  });

  it("a rate-limit-rejected empty turn leaves the follow-up unreported", async () => {
    const queue = [A];
    const { queryFn } = fakeTurns(
      [PLAN, { messages: [initFrame(), rateLimitEvent("rejected"), resultZeroTurns()] }],
      queue,
    );
    const probe = makeCtx(queue);
    await assert.rejects(exec(queryFn, { emptyTurnMaxRetries: 0 }).run(probe.ctx));
    assert.deepStrictEqual(probe.included, []);
  });

  it("a turn that starts and then throws before any model event leaves the follow-up unreported", async () => {
    const queue = [A];
    const prompts: string[] = [];
    let call = 0;
    const queryFn: SdkQueryFn = (params) => {
      const n = call++;
      return (async function* () {
        for await (const p of params.prompt) {
          const content = (p as { message?: { content?: unknown } }).message?.content;
          prompts[n] = typeof content === "string" ? content : JSON.stringify(content);
        }
        if (n === 0) {
          yield submitPlan("# Plan");
          yield resultSuccess();
          return;
        }
        yield initFrame();
        throw new Error("spawn failed after init");
      })();
    };
    const probe = makeCtx(queue);
    await assert.rejects(exec(queryFn).run(probe.ctx), /spawn failed after init/);
    assert.ok(prompts[1]!.includes(A), "the throwing turn had the follow-up in its prompt");
    assert.deepStrictEqual(probe.included, [], "no model event, so nothing reported");
  });

  it("a usage-limit-rejected turn (synthetic notice, is_error result with num_turns 1) leaves the follow-up unreported", async () => {
    // The real limit-rejected shape: the terminal reports a POSITIVE turn count even though the
    // model never ran, and the executor consults the evidence before the reducer classifies it.
    const queue = [A];
    const { queryFn, prompts } = fakeTurns(
      [
        PLAN,
        {
          messages: [
            initFrame(),
            rateLimitEvent("rejected"),
            syntheticAssistant("You've hit your limit"),
            { type: "result", subtype: "success", is_error: true, num_turns: 1, session_id: "sess-1" } as unknown as SDKMessage,
          ],
        },
      ],
      queue,
    );
    const probe = makeCtx(queue);
    await assert.rejects(exec(queryFn, { emptyTurnMaxRetries: 0 }).run(probe.ctx));
    assert.ok(prompts[1]!.includes(A), "the turn was started with the follow-up in its prompt");
    assert.deepStrictEqual(probe.included, [], "the model never read it, so the next claim re-queues it");
  });

  it("a turn whose only assistant frame is a synthetic notice and whose terminal is an error leaves the follow-up unreported", async () => {
    const queue = [A];
    const { queryFn } = fakeTurns(
      [PLAN, { messages: [initFrame(), syntheticAssistant("worker notice"), resultErrorOneTurn("error_during_execution")] }],
      queue,
    );
    const probe = makeCtx(queue);
    await assert.rejects(exec(queryFn, { emptyTurnMaxRetries: 0 }).run(probe.ctx));
    assert.deepStrictEqual(probe.included, []);
  });

  // Frames that are NOT the lead model answering: a subagent assistant message, a main-thread
  // user tool_result and a replayed user message. None shows the lead read the follow-up.
  const subagentAssistant = (): SDKMessage =>
    ({
      type: "assistant",
      session_id: "sess-1",
      parent_tool_use_id: "toolu_parent",
      message: { content: [{ type: "text", text: "subagent chatter" }] },
    }) as unknown as SDKMessage;
  const mainToolResult = (): SDKMessage =>
    ({
      type: "user",
      session_id: "sess-1",
      parent_tool_use_id: null,
      message: { content: [{ type: "tool_result", tool_use_id: "t9", content: "ok" }] },
    }) as unknown as SDKMessage;
  const replayUser = (): SDKMessage =>
    ({
      type: "user",
      session_id: "sess-1",
      isReplay: true,
      message: { content: [{ type: "tool_result", tool_use_id: "t8", content: "replayed result" }] },
    }) as unknown as SDKMessage;

  const nonLeadFrames: Array<[string, () => SDKMessage]> = [
    ["a subagent assistant frame", subagentAssistant],
    ["a main-thread user tool_result frame", mainToolResult],
    ["a replayed user frame", replayUser],
  ];

  for (const [label, frame] of nonLeadFrames) {
    it(`${label} as the only evidence before the turn fails leaves the follow-up unreported`, async () => {
      const queue = [A];
      const { queryFn, prompts } = fakeTurns(
        [PLAN, { messages: [initFrame(), frame(), resultZeroTurns()] }],
        queue,
      );
      const probe = makeCtx(queue);
      await assert.rejects(exec(queryFn, { emptyTurnMaxRetries: 0 }).run(probe.ctx));
      assert.ok(prompts[1]!.includes(A), "the turn carried the follow-up");
      assert.deepStrictEqual(probe.included, [], "never stamped: the lead model did not answer");
    });

    it(`${label} does not stamp; the retry carries the follow-up and is the one that stamps`, async () => {
      const queue = [A];
      const { queryFn, prompts } = fakeTurns(
        [PLAN, { messages: [initFrame(), frame(), resultZeroTurns()] }, { messages: [initFrame(), ...DONE.messages] }],
        queue,
      );
      const probe = makeCtx(queue);
      const queriesAtStamp: number[] = [];
      const base = probe.ctx.followUpIncluded!;
      probe.ctx.followUpIncluded = (id) => {
        queriesAtStamp.push(prompts.length);
        base(id);
      };
      await exec(queryFn, { emptyTurnMaxRetries: 2 }).run(probe.ctx);
      assert.ok(prompts[1]!.includes(A) && prompts[2]!.includes(A), "both attempts carry it");
      assert.deepStrictEqual(probe.included, [1], "stamped exactly once");
      assert.deepStrictEqual(queriesAtStamp, [3], "stamped by the retry's lead assistant frame, not the earlier frame");
    });
  }

  it("a lead main-thread assistant frame stamps even after a non-lead frame", async () => {
    const queue = [A];
    const { queryFn } = fakeTurns(
      [PLAN, { messages: [initFrame(), subagentAssistant(), assistantText("lead answer"), resultSuccess()] }, DONE],
      queue,
    );
    const probe = makeCtx(queue);
    await exec(queryFn).run(probe.ctx);
    assert.deepStrictEqual(probe.included, [1]);
  });
});
