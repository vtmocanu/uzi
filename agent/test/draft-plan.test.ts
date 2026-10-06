import test from "node:test";
import assert from "node:assert/strict";
import type { z } from "zod";
import { buildSignalMcpServer, isSignalToolName, scanSignals } from "../src/signals.js";
import { buildCodexDynamicTools } from "../src/codex/dynamic-tools.js";
import { renderCodexRun } from "../src/codex/render.js";
import { RunTurnReducerImpl } from "../src/harness-reducer.js";
import { MessageBatcher, MAX_MESSAGE_BYTES } from "../src/batcher.js";
import { makeRedactor, makeTextRedactor } from "../src/redact.js";
import type { WorkerClient } from "../src/client.js";
import type { OutgoingMessage } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { capture, claudeScript, codexScript, request, toolNote } from "./draft-plan-script.js";

const ack = "Draft capture requested; this is not plan submission or approval.";
const tool = "save_draft_plan";
const completed = { kind: "turn_completed" as const, method: "turn/completed", threadId: "th-1", turnId: "tn-1", status: "completed",
  params: { threadId: "th-1", turn: { id: "tn-1", status: "completed" } } };

test("Claude registration requires nonblank Markdown and enforces decoded maxLength before trim", async () => {
  const server = buildSignalMcpServer() as unknown as { instance: { _registeredTools: Record<string, {
    inputSchema: z.ZodType; handler: () => Promise<{ content: { text: string }[] }>;
  }> } };
  const registered = server.instance._registeredTools[tool]!;
  assert.ok(registered);
  assert.equal(registered.inputSchema.safeParse({ plan_md: "# draft" }).success, true);
  for (const plan_md of [undefined, null, 1, "", " \n\t", " ".repeat(65_536) + "x"]) {
    assert.equal(registered.inputSchema.safeParse({ plan_md }).success, false);
    assert.deepEqual(scanSignals(capture(plan_md)), {});
  }
  const originalTrim = String.prototype.trim;
  try {
    String.prototype.trim = function () {
      assert.ok(this.length <= 65_536, "oversized draft must be rejected before trim");
      return originalTrim.call(this);
    };
    assert.equal(registered.inputSchema.safeParse({ plan_md: "x".repeat(65_537) }).success, false);
    assert.deepEqual(scanSignals(capture("x".repeat(65_537))), {});
  } finally {
    String.prototype.trim = originalTrim;
  }
  assert.equal(registered.inputSchema.safeParse({ plan_md: "x".repeat(65_536) }).success, true);
  assert.equal((await registered.handler()).content[0]?.text, ack);
  assert.equal(isSignalToolName("mcp__uzi__save_draft_plan"), true);
});

test("raw capture JSON is bounded before parsing; extras are not traversed", () => {
  const rawFrame = (input: unknown) => ({ type: "assistant", message: { content: [
    { type: "tool_use", name: "mcp__uzi__save_draft_plan", input },
  ] } });
  for (const input of ["{", JSON.stringify({ plan_md: "x".repeat(65_537) }),
    JSON.stringify({ plan_md: "# draft", extra: "x".repeat(256 * 1024) }),
    JSON.stringify({ plan_md: "# draft", extra: "世".repeat(100_000) })]) {
    assert.deepEqual(scanSignals(rawFrame(input)), {});
  }
  const exact = JSON.stringify({ plan_md: "# draft" });
  assert.deepEqual(scanSignals(rawFrame(exact + " ".repeat(256 * 1024 - exact.length))), { draftPlans: ["# draft"] });
  const input = { plan_md: "# draft", get extra(): never { throw new Error("extras traversed"); } };
  assert.deepEqual(scanSignals(rawFrame(input)), { draftPlans: ["# draft"] });
  assert.deepEqual(scanSignals(rawFrame(JSON.stringify({ plan_md: "# draft" }))), { draftPlans: ["# draft"] });
  assert.deepEqual(scanSignals(capture("# draft", { parent_tool_use_id: "child" })), {});
});

test("oversized raw capture is rejected without a UTF8 scan", (t) => {
  const oversized = JSON.stringify({ plan_md: "# draft", extra: "x".repeat(2 * 1024 * 1024) });
  const byteLength = Buffer.byteLength;
  t.mock.method(Buffer, "byteLength", (value: Parameters<typeof Buffer.byteLength>[0], encoding?: BufferEncoding) => {
    assert.notEqual(value, oversized, "oversized input must be rejected before byteLength");
    return byteLength(value, encoding);
  });
  assert.deepEqual(scanSignals({ type: "assistant", message: { content: [
    { type: "tool_use", name: "mcp__uzi__save_draft_plan", input: oversized },
  ] } }), {});
});

test("Claude consumes draft result IDs without hiding sibling or reused ordinary results", async () => {
  const result = (content: string, markers = {}) => ({ type: "user", ...markers,
    message: { content: [{ type: "tool_result", tool_use_id: "draft-call", content }] } });
  const harness = claudeScript([
    capture("# draft"),
    result("sibling result", { parent_tool_use_id: "validator", subagent_type: "tester" }),
    result(ack),
    { type: "assistant", message: { content: [
      { type: "tool_use", id: "draft-call", name: "Read", input: { file_path: "WORK.txt" } },
    ] } },
    result("ordinary reused result"),
  ]);
  const reducer = new RunTurnReducerImpl({ request() {}, get: async () => undefined });
  reducer.beginTurn();
  const messages = [];
  for await (const event of harness.startTurn(request).events) messages.push(...(await reducer.accept(event)).messages);
  const results = messages.filter((m) => m.kind === "tool_result");
  assert.equal(results.length, 2);
  assert.ok(JSON.stringify(results[0]).includes("sibling result"));
  assert.ok(JSON.stringify(results[1]).includes("ordinary reused result"));
  assert.ok(!JSON.stringify(messages).includes(ack));
});

test("Codex child and unknown callbacks are refused, malformed captures reject, root schema is bounded", async () => {
  const script = codexScript([]);
  const spec = buildCodexDynamicTools({
    role: "lead", phase: "plan", isRoot: true, allowedTools: new Set([tool]), allowedSkills: new Set(),
  })[0]!;
  assert.deepEqual(spec.inputSchema.required, ["plan_md"]);
  assert.equal((spec.inputSchema.properties as Record<string, { maxLength: number }>).plan_md?.maxLength, 65_536);
  const child = renderCodexRun({
    ...request, agents: { tester: { description: "test", prompt: "test", tools: { kind: "allow", names: ["mcp__uzi__save_draft_plan"] }, deniedTools: [], toolServers: [], skills: [] } },
  });
  assert.ok(!child.perRoleGrants.get("tester")?.allowedTools.has(tool));
  let seq = 0;
  const call = (args: unknown, origin: "root" | "child" | "unknown") =>
    script.broker.handleToolCall({ threadId: "t", turnId: "u", callId: String(++seq) }, tool, args, origin);
  for (const origin of ["child", "unknown"] as const) {
    assert.equal((await call({ plan_md: "# draft" }, origin)).ok, false);
  }
  for (const args of [{}, { plan_md: "" }, { plan_md: 5 }, { plan_md: " ".repeat(65_537) },
    JSON.stringify({ plan_md: "# draft", extra: "x".repeat(256 * 1024) })]) {
    assert.equal((await call(args, "root")).ok, false);
  }
  assert.equal((await call({ plan_md: "# draft", get extra(): never { throw Error("traversed"); } }, "root")).ok, true);
});

test("both adapters deliver draft immediately, filter raw calls/results, and keep submission independent", async () => {
  for (const adapter of ["claude", "codex"] as const) {
    const script = adapter === "claude" ? { harness: claudeScript([
      capture("# draft"),
      { type: "user", message: { content: [{ type: "tool_result", tool_use_id: "draft-call", content: ack }] } },
      { type: "assistant", message: { content: [
        { type: "tool_use", name: "mcp__uzi__submit_plan", input: { plan_md: "# submitted" } },
      ] } },
    ]) } : codexScript([toolNote(tool, { plan_md: "# draft" }), toolNote("submit_plan", { plan_md: "# submitted" }, "submit"),
      { kind: "turn_completed", method: "turn/completed", threadId: "th-1", turnId: "tn-1", status: "completed",
        params: { threadId: "th-1", turn: { id: "tn-1", status: "completed" } } },
    ]);
    const reducer = new RunTurnReducerImpl({ request() {}, get: async () => undefined });
    reducer.beginTurn();
    const messages = [];
    let sawCapture = false;
    for await (const event of script.harness.startTurn(request).events) {
      const reduction = await reducer.accept(event);
      messages.push(...reduction.messages);
      if (reduction.messages.some((m) => m.payload.event === "draft_plan_capture")) {
        sawCapture = true;
        const interim = reducer.finish({ kind: "exhausted" }).result;
        assert.equal(interim.plan, undefined);
        assert.equal(interim.done, false);
        assert.equal(interim.checkpoint, undefined);
      }
      if (event.kind === "turn_finished") assert.equal(sawCapture, true);
    }
    assert.equal(reducer.finish({ kind: "exhausted" }).result.plan, "# submitted");
    assert.equal(messages.filter((m) => m.payload.event === "draft_plan_capture").length, 1);
    assert.equal(messages.some((m) => m.kind === "tool_use" || m.kind === "tool_result"), false);
    if ("replies" in script) assert.ok(JSON.stringify(script.replies[0]).includes(ack));
  }
});

async function persist(plan_md: string, secrets: string[] = []) {
  const sent: OutgoingMessage[] = [];
  const client = { async postMessages(_run: string, messages: OutgoingMessage[]) { sent.push(...messages); } } as unknown as WorkerClient;
  const batcher = new MessageBatcher(client, "draft", 0, 60_000, nullLogger(), makeRedactor(secrets), makeTextRedactor(secrets));
  batcher.emit({ kind: "status", agent: "lead", payload: {
    event: "draft_plan_capture", version: 1, label: "draft, unapproved, possibly incomplete", plan_md, truncated: false,
  } });
  await batcher.close();
  assert.equal(sent.length, 1);
  assert.ok(Buffer.byteLength(JSON.stringify(sent[0]), "utf8") <= MAX_MESSAGE_BYTES);
  return sent[0]!.payload;
}

test("sanitize whole input, redact whole secrets spanning the cutoff, then truncate", async () => {
  const secret = "glpat-" + "abcdefghijklmnopqrst";
  const head = "x".repeat(32_760);
  const split = secret.slice(0, 10) + "\u0000" + secret.slice(10);
  const out = await persist(head + split + "tail".repeat(100), [secret]);
  assert.equal(out.plan_md, head + "***REDAC");
  assert.equal(out.truncated, true);
  assert.equal(JSON.stringify(out).includes("glpat-"), false);
});

test("Codex accepted drafts use live runtime redaction before batcher truncation", async () => {
  const first = "glpat-" + "abcdefghijklmnopqrst";
  const refreshed = "glpat-" + "zyxwvutsrqponmlkjihg";
  const claimSecret = "claim-only-secret";
  const released = new Set([first]);
  const head = "x".repeat(32_760);
  const split = refreshed.slice(0, 10) + "\u0000" + refreshed.slice(10);
  const script = codexScript([
    toolNote(tool, { plan_md: first + " " + claimSecret }, "short"),
    toolNote(tool, { plan_md: head + split + "tail".repeat(100) }, "long"), completed,
  ], undefined, (s) => makeTextRedactor([claimSecret])(makeTextRedactor([...released])(s)));
  const sent: OutgoingMessage[] = [];
  const client = { async postMessages(_run: string, messages: OutgoingMessage[]) { sent.push(...messages); } } as unknown as WorkerClient;
  const batcher = new MessageBatcher(client, "live-draft", 0, 60_000, nullLogger(),
    makeRedactor([claimSecret]), makeTextRedactor([claimSecret]));
  const reducer = new RunTurnReducerImpl({ request() {}, get: async () => undefined });
  reducer.beginTurn();
  const immediate = [];
  try {
    for await (const event of script.harness.startTurn(request).events) {
      for (const message of (await reducer.accept(event)).messages) {
        if (message.payload.event !== "draft_plan_capture") continue;
        immediate.push(message);
        batcher.emit(message);
        if (immediate.length === 1) released.add(refreshed);
      }
    }
  } finally {
    await batcher.close();
  }
  assert.deepEqual(immediate.map((m) => m.payload.plan_md), [
    "***REDACTED*** ***REDACTED***",
    head + "***REDACTED***" + "tail".repeat(100),
  ]);
  assert.deepEqual(immediate.map((m) => m.payload.truncated), [false, false]);
  assert.deepEqual(sent.map((m) => m.payload.plan_md), [
    "***REDACTED*** ***REDACTED***", head + "***REDAC",
  ]);
  assert.deepEqual(sent.map((m) => m.payload.truncated), [false, true]);
  assert.deepEqual(sent.map((m) => m.seq), [1, 2]);
  for (const message of sent) {
    assert.ok(Buffer.byteLength(JSON.stringify(message), "utf8") <= MAX_MESSAGE_BYTES);
    assert.ok(Buffer.byteLength(String(message.payload.plan_md), "utf8") <= 32_768);
  }
  assert.equal(JSON.stringify([immediate, sent]).includes("glpat-"), false);
});

test("both adapters append captures immediately and persist no malformed or child signal payload", async () => {
  for (const adapter of ["claude", "codex"] as const) {
    const forbidden = "raw-rejected-draft";
    const childNote = { ...toolNote(tool, { plan_md: forbidden }, "child"),
      params: { threadId: "foreign-child", turnId: "tn-1", callId: "child", tool, arguments: { plan_md: forbidden }, namespace: null } };
    const script = adapter === "claude" ? { harness: claudeScript([
      capture("# first"), capture({ raw: forbidden }),
      capture(forbidden, { parent_tool_use_id: "child", subagent_type: "tester" }),
      capture("# second"), capture("# third"),
    ]) } : codexScript([
      toolNote(tool, { plan_md: "# first" }, "first"),
      toolNote(tool, { plan_md: { raw: forbidden } }, "malformed"),
      childNote,
      toolNote(tool, { plan_md: "# second" }, "second"),
      toolNote(tool, { plan_md: "# third" }, "third"), completed,
    ]);
    const sent: OutgoingMessage[] = [];
    const client = { async postMessages(_run: string, messages: OutgoingMessage[]) { sent.push(...messages); } } as unknown as WorkerClient;
    const batcher = new MessageBatcher(client, "append-draft", 0, 60_000, nullLogger(), makeRedactor([]), makeTextRedactor([]));
    const reducer = new RunTurnReducerImpl({ request() {}, get: async () => undefined });
    reducer.beginTurn();
    const immediate = [];
    try {
      for await (const event of script.harness.startTurn(request).events) {
        const messages = (await reducer.accept(event)).messages;
        immediate.push(...messages.filter((m) => m.payload.event === "draft_plan_capture"));
        for (const message of messages) batcher.emit(message);
        assert.deepEqual(immediate.map((m) => m.payload.plan_md),
          ["# first", "# second", "# third"].slice(0, immediate.length));
      }
    } finally {
      await batcher.close();
    }
    assert.deepEqual(immediate.map((m) => m.payload.plan_md), ["# first", "# second", "# third"]);
    const captures = sent.filter((m) => m.payload.event === "draft_plan_capture");
    assert.deepEqual(captures.map((m) => m.payload.plan_md), ["# first", "# second", "# third"]);
    assert.ok(captures[0]!.seq < captures[1]!.seq && captures[1]!.seq < captures[2]!.seq);
    assert.equal(JSON.stringify([immediate, sent]).includes(forbidden), false);
    assert.equal(sent.some((m) => m.kind === "tool_use" || m.kind === "tool_result"), false);
  }
});

test("UTF8 exact boundary, Unicode, NUL, lone surrogates and escaped JSON stay bounded", async () => {
  for (const [input, expected, truncated] of [
    ["x".repeat(32_768), "x".repeat(32_768), false],
    ["x".repeat(32_768) + "y", "x".repeat(32_768), true],
    ["x".repeat(32_767) + "😀", "x".repeat(32_767), true],
    ["😀".repeat(8192), "😀".repeat(8192), false],
    ["😀".repeat(8193), "😀".repeat(8192), true],
    ["a\u0000b\ud800c\udc00d", "ab\uFFFDc\uFFFDd", false],
    ['"\\\n'.repeat(10_000), '"\\\n'.repeat(10_000), false],
  ] as const) {
    const out = await persist(input);
    assert.equal(out.plan_md, expected);
    assert.equal(out.truncated, truncated);
    assert.ok(Buffer.byteLength(String(out.plan_md), "utf8") <= 32_768);
    assert.equal(JSON.parse(JSON.stringify(out)).plan_md, expected);
  }
});
