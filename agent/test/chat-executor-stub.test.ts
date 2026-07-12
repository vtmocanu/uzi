import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { StubChatExecutor, STUB_CHAT_PROPOSE } from "../src/chat-executor-stub.js";
import type { ChatContext } from "../src/chat-executor.js";
import type { UziToolHandlers, ToolTextResult } from "../src/uzi-tools.js";
import type { EmittedMessage } from "../src/executor.js";
import { nullLogger } from "./helpers.js";

// The stub chat executor drives the real ChatContext park/turn loop with canned
// replies (no live SDK), so the M6 chat e2e can run under UZI_EXECUTOR=stub. These
// prove: an assistant reply per turn, idle-completion, the turn cap, End-chat cancel,
// a persisted session id, and the propose sentinel driving the real propose_issue.

interface Probe {
  ctx: ChatContext;
  emits: EmittedMessage[];
  sessions: string[];
  proposeCalls: Array<Parameters<UziToolHandlers["proposeIssue"]>[0]>;
}

const okResult = (text: string): ToolTextResult => ({ content: [{ type: "text", text }] });

function makeCtx(
  queue: (string | undefined)[],
  overrides: Partial<ChatContext> = {},
  proposeResult: ToolTextResult = okResult("drafted prop-1"),
): Probe {
  const emits: EmittedMessage[] = [];
  const sessions: string[] = [];
  const proposeCalls: Probe["proposeCalls"] = [];
  const pending = [...queue];
  const uziTools = {
    async listRuns() { return okResult("[]"); },
    async getRun() { return okResult("{}"); },
    async getRunMessages() { return okResult("[]"); },
    async proposeIssue(args: Parameters<UziToolHandlers["proposeIssue"]>[0]) {
      proposeCalls.push(args);
      return proposeResult;
    },
  } as unknown as UziToolHandlers;
  const ctx: ChatContext = {
    runId: "chat-1",
    sessionId: null,
    emit: (m) => emits.push(m),
    onSessionId: (s) => sessions.push(s),
    maxTurns: 50,
    turnTimeoutMs: 5000,
    uziTools,
    nextUserMessage: () => Promise.resolve<string | undefined>(pending.length ? pending.shift() : undefined),
    ...overrides,
  };
  return { ctx, emits, sessions, proposeCalls };
}

const texts = (emits: EmittedMessage[]): string[] =>
  emits.filter((m) => m.kind === "text").map((m) => String(m.payload["text"]));

describe("StubChatExecutor", () => {
  it("answers each turn with a canned reply and idle-completes when input runs out", async () => {
    const probe = makeCtx(["how does the plan gate work?", undefined]);
    const result = await new StubChatExecutor(nullLogger()).run(probe.ctx);
    assert.strictEqual(result.turns, 1);
    assert.strictEqual(result.endReason, "idle");
    assert.ok(texts(probe.emits).some((t) => /stub chat reply to: how does the plan gate work\?/.test(t)));
    // A terminal result frame is emitted per turn (feed turn boundary).
    assert.ok(probe.emits.some((m) => m.kind === "status" && m.payload["event"] === "result"));
  });

  it("reports a stable session id (so Continue has something to resume)", async () => {
    const probe = makeCtx([undefined]);
    const result = await new StubChatExecutor(nullLogger()).run(probe.ctx);
    assert.deepStrictEqual(probe.sessions, ["stub-chat-chat-1"]);
    assert.strictEqual(result.sessionId, "stub-chat-chat-1");
  });

  it("caps the conversation at maxTurns", async () => {
    const probe = makeCtx(["a", "b", "c"], { maxTurns: 2 });
    const result = await new StubChatExecutor(nullLogger()).run(probe.ctx);
    assert.strictEqual(result.turns, 2);
    assert.strictEqual(result.endReason, "turn_cap");
    assert.ok(probe.emits.some((m) => m.kind === "status" && /turn limit/.test(String(m.payload["text"]))));
  });

  it("ends (not idle) when the external signal is aborted", async () => {
    const controller = new AbortController();
    controller.abort();
    const probe = makeCtx([undefined], { signal: controller.signal });
    const result = await new StubChatExecutor(nullLogger()).run(probe.ctx);
    assert.strictEqual(result.endReason, "ended");
  });

  it("drives the real propose_issue on the sentinel: repo_path parsed, card handler called", async () => {
    const probe = makeCtx([`${STUB_CHAT_PROPOSE} group/project Add a metrics dashboard`, undefined]);
    await new StubChatExecutor(nullLogger()).run(probe.ctx);
    assert.strictEqual(probe.proposeCalls.length, 1);
    const call = probe.proposeCalls[0]!;
    assert.strictEqual(call.repo_path, "group/project");
    assert.strictEqual(call.title, "Add a metrics dashboard");
    assert.ok(texts(probe.emits).some((t) => /click Create/.test(t)));
  });

  it("echoes the tool's error guidance when propose_issue fails (no phantom success)", async () => {
    const probe = makeCtx([`${STUB_CHAT_PROPOSE}`, undefined], {}, { content: [{ type: "text", text: "needs the target repo" }], isError: true });
    await new StubChatExecutor(nullLogger()).run(probe.ctx);
    // No repo_path in the sentinel → proposeIssue returns an error we surface verbatim.
    assert.strictEqual(probe.proposeCalls[0]!.repo_path, undefined);
    assert.ok(texts(probe.emits).some((t) => /needs the target repo/.test(t)));
    assert.ok(!texts(probe.emits).some((t) => /click Create/.test(t)));
  });
});
