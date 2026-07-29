import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { SteeringChannel, type AnswerVerdict } from "../src/steering.js";
import type { WorkerClient } from "../src/client.js";
import type { UserInput } from "../src/protocol.js";
import { scanSignals, isSignalToolName, ASK_USER_TOOL, SIGNAL_SERVER_NAME } from "../src/signals.js";
import { nullLogger } from "./helpers.js";

// PRD #88 M1: the ask_user signal and the answer half of the steering channel.
// Driven with a scripted getInputs and hand-built SDK frames — no live server, no
// SDK session, matching how the plan gate is proved.

const ASK_USER_QUALIFIED = `mcp__${SIGNAL_SERVER_NAME}__${ASK_USER_TOOL}`;

function fakeClient(batches: UserInput[][]): WorkerClient {
  let i = 0;
  return { getInputs: async () => batches[i++] ?? [] } as unknown as WorkerClient;
}

const inp = (kind: UserInput["kind"], body?: string): UserInput => ({ id: 1, kind, body: body ?? null });
const answerInput = (questionId: string, ...answers: string[]): UserInput =>
  inp("answer", JSON.stringify({ question_id: questionId, answers }));
const tick = (ms = 10): Promise<void> => new Promise((r) => setTimeout(r, ms));

function makeChannel(
  batches: UserInput[][],
  notices?: string[],
): { ch: SteeringChannel; cancel: AbortController } {
  const cancel = new AbortController();
  const ch = new SteeringChannel(fakeClient(batches), "run-1", 1, nullLogger(), cancel, {
    notify: notices ? (t) => notices.push(t) : undefined,
  });
  return { ch, cancel };
}

/** An assistant frame carrying one ask_user tool_use. `extra` merges onto the
 *  OUTER frame, which is where the subagent markers live. */
function askFrame(questions: unknown, extra: Record<string, unknown> = {}): unknown {
  return {
    type: "assistant",
    ...extra,
    message: { content: [{ type: "tool_use", name: ASK_USER_QUALIFIED, input: { questions } }] },
  };
}

const oneQuestion = [{ question: "Which database?", header: "DB choice" }];

describe("ask_user signal extraction", () => {
  it("extracts questions from a lead frame", () => {
    const sig = scanSignals(askFrame(oneQuestion));
    assert.equal(sig.questions?.length, 1);
    assert.equal(sig.questions?.[0]?.question, "Which database?");
    assert.equal(sig.questions?.[0]?.header, "DB choice");
  });

  it("carries options and multiSelect through", () => {
    const sig = scanSignals(
      askFrame([
        {
          question: "Which database?",
          header: "DB",
          options: [{ label: "postgres", description: "what we run today" }],
          multiSelect: true,
        },
      ]),
    );
    assert.deepEqual(sig.questions?.[0]?.options, [{ label: "postgres", description: "what we run today" }]);
    assert.equal(sig.questions?.[0]?.multiSelect, true);
  });

  // The security-relevant half of the extraction: a prompt-injected subagent reaching
  // ask_user could post attacker-chosen text into the owner's Slack DM under uzi's bot
  // identity. agents.ts denies the mcp__uzi prefix, but signals.ts states that whether
  // disallowedTools beats a custom template's `tools` allowlist is unproven, so this
  // worker-side scan is the load-bearing guarantee rather than a redundant one.
  it("ignores an ask_user from a subagent frame (subagent_type)", () => {
    assert.deepEqual(scanSignals(askFrame(oneQuestion, { subagent_type: "reviewer" })), {});
  });

  it("ignores an ask_user from a subagent frame (parent_tool_use_id)", () => {
    assert.deepEqual(scanSignals(askFrame(oneQuestion, { parent_tool_use_id: "toolu_1" })), {});
  });

  it("drops malformed questions without throwing", () => {
    assert.equal(scanSignals(askFrame("not-an-array")).questions, undefined);
    assert.equal(scanSignals(askFrame([{ header: "no question text" }])).questions, undefined);
    assert.equal(scanSignals(askFrame([null, 7])).questions, undefined);
  });

  it("clamps model-authored text, since it reaches three untrusted-text sinks", () => {
    const sig = scanSignals(askFrame([{ question: "q".repeat(5000), header: "h".repeat(500) }]));
    assert.equal(sig.questions?.[0]?.question.length, 2000);
    assert.equal(sig.questions?.[0]?.header.length, 60);
  });

  // Without this the question JSON ALSO persists as an ordinary tool_use run-message,
  // a second unpinned sink for the same attacker-influenceable text.
  it("treats ask_user as a signal tool so its payload is not double-persisted", () => {
    assert.equal(isSignalToolName(ASK_USER_QUALIFIED), true);
  });
});

describe("SteeringChannel — answers", () => {
  it("resolves a parked question with the matching answer", async () => {
    const { ch } = makeChannel([[answerInput("q-1", "postgres")]]);
    ch.start();
    assert.deepStrictEqual(await ch.awaitAnswer("q-1"), {
      kind: "answer",
      answers: ["postgres"],
    } satisfies AnswerVerdict);
    await ch.stop();
  });

  // The lost-wakeup case, and the direct guard against the silent-drop failure:
  // /inputs is consume-on-read, so an answer consumed before the park is decided is
  // gone from the server. It must be buffered, never dropped.
  it("buffers an answer that arrives before anything is parked", async () => {
    const { ch } = makeChannel([[answerInput("q-1", "postgres")]]);
    ch.start();
    await tick(); // let the poll loop consume it with no waiter parked
    assert.deepStrictEqual(await ch.awaitAnswer("q-1"), { kind: "answer", answers: ["postgres"] });
    await ch.stop();
  });

  // The stale-answer case the identity key exists for: a reply written against
  // question 1 that lands after the lead has already asked question 2.
  it("discards an answer naming a different question, with a feed notice", async () => {
    const notices: string[] = [];
    const { ch } = makeChannel([[answerInput("q-1", "postgres")]], notices);
    ch.start();
    let resolved: AnswerVerdict | undefined;
    void ch.awaitAnswer("q-2").then((v) => (resolved = v));
    await tick(30);
    assert.equal(resolved, undefined, "an answer to q-1 must not resolve a park on q-2");
    assert.equal(notices.length, 1);
    assert.match(notices[0]!, /earlier question/);
    await ch.stop();
  });

  // The mirror of the case above, and the one a guard tightened for staleness alone
  // would fail: an answer submitted BEFORE a worker death, consumed after the resume
  // re-parks on the SAME question id, must still be honoured. This is why the key is
  // the question's identity and not a clock or an arrival ordinal.
  it("honours an answer consumed after a re-park on the same question", async () => {
    const { ch } = makeChannel([[answerInput("q-1", "postgres")]]);
    ch.start();
    await tick(); // consumed while the pre-death park is gone
    // The resumed worker re-parks on the same id, seeded from the claim.
    assert.deepStrictEqual(await ch.awaitAnswer("q-1"), { kind: "answer", answers: ["postgres"] });
    await ch.stop();
  });

  it("resolves a parked question with cancel, which is identity-exempt", async () => {
    const { ch } = makeChannel([[inp("cancel")]]);
    ch.start();
    assert.deepStrictEqual(await ch.awaitAnswer("q-anything"), { kind: "cancel" } satisfies AnswerVerdict);
    await ch.stop();
  });

  it("ignores a malformed answer body rather than resolving the park", async () => {
    const { ch } = makeChannel([[inp("answer", "not json")], [inp("answer", JSON.stringify({ answers: ["x"] }))]]);
    ch.start();
    let resolved: AnswerVerdict | undefined;
    void ch.awaitAnswer("q-1").then((v) => (resolved = v));
    await tick(30);
    assert.equal(resolved, undefined, "an answer that cannot name its question has no safe reading");
    await ch.stop();
  });

  // An answer must not resolve the PLAN gate, and a plan verdict must not resolve a
  // question: the two use separate buffers and separate waiter slots on purpose.
  it("keeps the answer path independent of the plan gate", async () => {
    const { ch } = makeChannel([[answerInput("q-1", "postgres")]]);
    ch.start();
    let gate: unknown;
    void ch.awaitVerdict().then((v) => (gate = v));
    await tick(30);
    assert.equal(gate, undefined, "an answer must not satisfy the plan gate");
    assert.deepStrictEqual(await ch.awaitAnswer("q-1"), { kind: "answer", answers: ["postgres"] });
    await ch.stop();
  });

  it("does not disturb the gate epoch", async () => {
    const { ch } = makeChannel([[answerInput("q-1", "postgres")]]);
    ch.start();
    const before = ch.currentEpoch();
    await ch.awaitAnswer("q-1");
    assert.equal(ch.currentEpoch(), before, "the question path must never bump the plan gate's epoch");
    await ch.stop();
  });
});
