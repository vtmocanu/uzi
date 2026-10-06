import { it } from "node:test";
import assert from "node:assert/strict";
import { PassThrough, Writable } from "node:stream";
import { CodexCrossCheck, crossCheckPrompt, type CrossCheckModelDeps } from "../src/codex/cross-check.js";
import type { CodexRootHandle, CodexLaunchSpec, CodexEffectLaunchSpec } from "../src/codex/launcher.js";
import type { ClaimResponse } from "../src/protocol.js";
import type { WorkerClient } from "../src/client.js";
import type { Logger } from "../src/log.js";

const verdict = JSON.stringify({ verdict: "approve", summary: "Anchors checked", items: [] });
const claim = (subscription = false): ClaimResponse => ({
 kind: "cross_check", run_id: "check", issue_iid: null, issue_title: "Issue",
 repo: { id: "repo", full_name: "owner/repo", url: "https://example.test/owner/repo", clone_url: "https://example.test/owner/repo.git", default_branch: "main" },
 last_seq: 0, agents: [],
 issue_description: "Untrusted issue", config: { default_model: "unknown-check-model", default_effort: "high" },
 secrets: { forge_pat: "", codex: subscription
  ? { auth_mode: "subscription", access_token: "claim-token", capability: "cap", generation: 7,
      chatgpt_account_id: "account", chatgpt_plan_type: null }
  : { auth_mode: "api_key", access_token: "claim-token", capability: "cap" } },
 cross_check: { stage: "plan", lead_run_id: "lead", round: 1, candidate_digest: "digest",
  deadline_at: "2099-01-01T00:00:00Z", plan_md: "Plan", milestones: [], required_capabilities: [],
  required_tools: [], size_class: "small", base_commit: "a".repeat(40), planning_diff: "diff" },
} as ClaimResponse);

type Frame = { id?: number | string; method?: string; params?: Record<string, any>; result?: any };
type Emit = (frame: unknown) => void;

// Both injected roots speak the real NDJSON contracts. The fileop stand-in exposes
// only its dictionary root. This proves functional wiring, not kernel isolation.
function rig(options: {
 notes?: (emit: Emit) => void;
 onToolReply?: (frame: Frame, emit: Emit) => void;
 clean?: boolean;
 failDispose?: boolean;
 subscription?: boolean;
 holdTurn?: boolean;
 holdStat?: boolean;
 refresh?: boolean;
} = {}) {
 const requests: Frame[] = [];
 const ops: string[] = [];
 const specs: (CodexLaunchSpec | CodexEffectLaunchSpec)[] = [];
 const disposed: string[] = [];
 const secrets = new Set<string>();
 const usage: Record<string, unknown>[] = [];
 const refreshes: unknown[] = [];
 let failProvider!: (error: Error) => void;
 const roots = (name: string, onFrame: (frame: Frame, emit: Emit) => void): CodexRootHandle => {
  const stdout = new PassThrough();
  const emit: Emit = (frame) => stdout.write(JSON.stringify(frame) + "\n");
  let buffer = "";
  const stdin = new Writable({ write(chunk, _encoding, done) {
   buffer += chunk.toString();
   let newline: number;
   while ((newline = buffer.indexOf("\n")) >= 0) {
    const line = buffer.slice(0, newline);
    buffer = buffer.slice(newline + 1);
    onFrame(JSON.parse(line), emit);
   }
   done();
  } });
  const whenFailed = new Promise<Error>((resolve) => { if (name === "provider") failProvider = resolve; });
  return { transport: { stdin, stdout, stderr: null }, whenFailed, failed: undefined,
   dispose: async () => {
    disposed.push(name);
    stdout.end();
    if (options.failDispose && name === "provider") throw new Error("dispose");
    return { clean: options.clean !== false };
   },
  } as unknown as CodexRootHandle;
 };
 const terminal = (emit: Emit, text = verdict): void => {
  emit({ method: "item/completed", params: { threadId: "thread", turnId: "turn",
   item: { type: "agentMessage", id: "assistant", text } } });
  emit({ method: "turn/completed", params: { threadId: "thread", turn: { id: "turn", status: "completed" } } });
 };
 const provider = roots("provider", (frame, emit) => {
  requests.push(frame);
  if (frame.method === "initialize") emit({ id: frame.id, result: { userAgent: "fake", codexHome: "/owned", platformFamily: "unix", platformOs: "linux" } });
  else if (frame.method === "account/login/start") emit({ id: frame.id, result: { type: options.subscription ? "chatgptAuthTokens" : "apiKey" } });
  else if (frame.method === "thread/start") emit({ id: frame.id, result: { thread: { id: "thread" } } });
  else if (frame.method === "turn/start") {
   emit({ id: frame.id, result: { turn: { id: "turn" } } });
   queueMicrotask(() => { if (!options.holdTurn) (options.notes ?? terminal)(emit); });
  } else if (!frame.method && frame.id !== undefined) options.onToolReply?.(frame, emit);
 });
 const fileop = roots("fileop", (frame, emit) => {
  const op = frame as unknown as { id: number; op: string; path: string };
  ops.push(op.op);
  if (op.op === "stat" && options.holdStat) return;
  emit({ id: op.id, ok: op.op === "stat" || (op.op === "read" && op.path === "anchor.ts"),
   data: Buffer.from("export const anchor = true;").toString("base64"), size: Buffer.byteLength("export const anchor = true;") });
 });
 const deps: CrossCheckModelDeps = {
  prepareHome: async () => {},
  launchFileop: async (spec) => { specs.push(spec); return fileop; },
  launchProvider: async (spec) => { specs.push(spec); return provider; },
 };
 const client: Pick<WorkerClient, "releaseCodex" | "refreshCodex"> = {
  releaseCodex: async () => options.subscription
   ? { auth_mode: "subscription", access_token: "released-token", generation: 7, chatgpt_account_id: "account", chatgpt_plan_type: null }
   : { auth_mode: "api_key", access_token: "released-token" },
  refreshCodex: async (_run, request, expected) => {
   if (!options.refresh) throw new Error("unused");
   refreshes.push({ request, expected });
   return { auth_mode: "subscription", access_token: "refreshed-token", generation: 8,
    chatgpt_account_id: "account", chatgpt_plan_type: null, outcome: "advanced" };
  },
 };
 const log = { addSecret: (s: string) => secrets.add(s), removeSecret: (s: string) => secrets.delete(s) } as unknown as Logger;
 const checker = new CodexCrossCheck(client, log, deps);
 return { requests, ops, specs, disposed, secrets, usage, refreshes, terminal, fail: () => failProvider(new Error("root failed")),
  run: (signal = new AbortController().signal) => checker.run(claim(options.subscription), "/checkout", "/owned", signal,
   async (payload) => { usage.push(payload); }) };
}

it("uses claimed model/effort, required wrappers, real Read broker and strict complete verdict", async () => {
 const r = rig({
  notes: (emit) => emit({ id: 90, method: "item/tool/call", params: {
   threadId: "thread", turnId: "turn", callId: "read", tool: "Read", arguments: { path: "anchor.ts" } } }),
  onToolReply: (frame, emit) => {
   assert.equal(frame.result.success, true);
   assert.match(frame.result.contentItems[0].text, /repository_excerpt_/);
   const excerpt = /^<(repository_excerpt_[a-f0-9]{32})>\n([\s\S]*)\n<\/\1>$/.exec(frame.result.contentItems[0].text);
   assert.ok(excerpt, "Read output remains inside its untrusted repository fence");
   assert.deepEqual(JSON.parse(excerpt[2]!), {
    size: Buffer.byteLength("export const anchor = true;"), content: "export const anchor = true;", offset: 1,
    linesReturned: 1, partialLastLine: false, truncated: false,
   });
   r.terminal(emit);
  },
 });
 assert.equal(await r.run(), verdict);
 const thread = r.requests.find((f) => f.method === "thread/start")!.params!;
 assert.equal(thread.model, "unknown-check-model");
 assert.deepEqual(thread.dynamicTools.map((t: any) => t.name).sort(), ["uzi_read", "uzi_search"]);
 assert.equal(thread.config.shell_tool, false);
 assert.equal(thread.config.project_doc_max_bytes, 0);
 assert.deepEqual(thread.environments, []);
 assert.equal(r.requests.find((f) => f.method === "turn/start")!.params!.effort, "high");
 assert.equal((r.specs[1] as CodexLaunchSpec).crossCheckReadOnly, true);
 assert.ok((r.specs[0] as CodexEffectLaunchSpec).args.includes("--cross-check"));
 assert.deepEqual(r.ops, ["stat", "read"]);
 assert.deepEqual(r.disposed, ["provider", "fileop"]);
 assert.equal(r.secrets.size, 0);
 assert.equal(r.usage.at(-1)!.is_error, false);
});

for (const text of ["prefix " + verdict, verdict + " suffix", "```json\n" + verdict + "\n```",
 '{"verdict":"maybe","summary":"","items":[]}', '{"verdict":"approve","summary":"","items":[],"extra":true}']) {
 it("rejects whole-item invalid verdict: " + text.slice(0, 20), async () => {
  const r = rig({ notes: (emit) => r.terminal(emit, text) });
  await assert.rejects(r.run(), /verdict/);
  assert.equal(r.usage.at(-1)!.is_error, true);
  assert.deepEqual(r.disposed, ["provider", "fileop"]);
 });
}

for (const method of ["item/agentMessage/delta", "item/completed", "turn/completed", "item/tool/call"]) {
 it("rejects foreign thread and stale turn for " + method, async () => {
  for (const ids of [{ threadId: "foreign", turnId: "turn" }, { threadId: "thread", turnId: "stale" }]) {
   const r = rig({ notes: (emit) => {
    emit({ ...(method === "item/tool/call" ? { id: 50 } : {}), method, params: {
     ...ids, delta: "x", itemId: "assistant", item: { type: "agentMessage", id: "assistant", text: verdict },
     turn: { id: ids.turnId, status: "completed" }, callId: "x", tool: "Read", arguments: { path: "anchor.ts" },
    } });
   } });
   await assert.rejects(r.run(), /foreign/);
   assert.deepEqual(r.ops, ["stat"]);
   assert.equal(r.usage.at(-1)!.is_error, true);
  }
 });
}

it("denies effects, delegation and Skill and charges all callbacks including repeated denials", async () => {
 let count = 0;
 const r = rig({ notes: (emit) => next(emit), onToolReply: (frame, emit) => {
  assert.equal(frame.result.success, false);
  next(emit);
 } });
 const next = (emit: Emit): void => {
  emit({ id: 100 + count, method: "item/tool/call", params: { threadId: "thread", turnId: "turn",
   callId: "denied-" + count, tool: ["Bash", "apply_patch", "spawn_agent", "Skill"][count++ % 4], arguments: {} } });
 };
 await assert.rejects(r.run(), /tool budget/);
 assert.equal(count, 201);
 assert.deepEqual(r.ops, ["stat"]);
});

it("bounds streaming plus complete text and rejects duplicate assistant items", async () => {
 for (const mode of ["delta", "complete", "combined", "duplicate"]) {
  const r = rig({ notes: (emit) => {
   if (mode === "delta" || mode === "combined") emit({ method: "item/agentMessage/delta", params: {
    threadId: "thread", turnId: "turn", itemId: "assistant", delta: "x".repeat(mode === "delta" ? 65537 : 65536) } });
   if (mode === "duplicate") {
    emit({ method: "item/completed", params: { threadId: "thread", turnId: "turn", item: { type: "agentMessage", id: "assistant", text: verdict } } });
   }
   r.terminal(emit, mode === "complete" ? "x".repeat(65537) : verdict);
  } });
  await assert.rejects(r.run(), /budget|bounded complete|disagrees/);
 }
});

it("fails model errors, missing verdict and unsuccessful terminal turns, retaining available usage", async () => {
 for (const failure of ["error", "missing", "failed"]) {
  const r = rig({ subscription: true, notes: (emit) => {
   const bucket = { inputTokens: 10, cachedInputTokens: 2, cacheWriteInputTokens: 0, outputTokens: 3, reasoningOutputTokens: 1, totalTokens: 13 };
   emit({ method: "thread/tokenUsage/updated", params: { threadId: "thread", turnId: "turn", tokenUsage: { total: bucket, last: bucket } } });
   emit({ method: "thread/tokenUsage/updated", params: { threadId: "thread", turnId: "turn", tokenUsage: { total: bucket, last: bucket } } });
   if (failure === "error") emit({ method: "error", params: { message: "provider secret" } });
   else emit({ method: "turn/completed", params: { threadId: "thread", turn: { id: "turn", status: failure === "missing" ? "completed" : "failed" } } });
  } });
  await assert.rejects(r.run());
  const usage = r.usage.at(-1)!;
  assert.equal(usage.is_error, true);
  assert.deepEqual((usage.modelUsage as any)["unknown-check-model"], {
   inputTokens: 8, cacheReadInputTokens: 2, cacheCreationInputTokens: 0, outputTokens: 3,
   reasoningOutputTokens: 1, costStatus: "subscription",
  });
  assert.equal(r.secrets.size, 0);
 }
});

it("unknown API-key model retains tokens with unreported cost", async () => {
 const r = rig({ notes: (emit) => {
  const bucket = { inputTokens: 10, cachedInputTokens: 0, cacheWriteInputTokens: 0, outputTokens: 3, totalTokens: 13 };
  emit({ method: "thread/tokenUsage/updated", params: { threadId: "thread", turnId: "turn", tokenUsage: { total: bucket, last: bucket } } });
  r.terminal(emit);
 } });
 await r.run();
 const usage = (r.usage.at(-1)!.modelUsage as any)["unknown-check-model"];
 assert.equal(usage.inputTokens, 10);
 assert.equal(usage.costStatus, "unreported");
 assert.equal(usage.costUSD, undefined);
});

it("aborts a pending turn and reaps both roots on root failure", async () => {
 for (const mode of ["abort", "root"]) {
  const r = rig({ holdTurn: true });
  const controller = new AbortController();
  const work = r.run(controller.signal);
  await new Promise((resolve) => setImmediate(resolve));
  if (mode === "root") r.fail(); else controller.abort();
  await assert.rejects(work);
  assert.deepEqual(r.disposed, ["provider", "fileop"]);
  assert.equal(r.usage.at(-1)!.is_error, true);
 }
});

it("aborts a pending helper readiness call before provider launch", async () => {
 const r = rig({ holdStat: true });
 const controller = new AbortController();
 const work = r.run(controller.signal);
 await new Promise((resolve) => setImmediate(resolve));
 controller.abort();
 await assert.rejects(work);
 assert.equal(r.specs.length, 1);
 assert.deepEqual(r.disposed, ["fileop"]);
});

it("cleanup failure fails the check, disposes sibling and marks usage error", async () => {
 const r = rig({ failDispose: true });
 await assert.rejects(r.run(), /cleanup/);
 assert.deepEqual(r.disposed, ["provider", "fileop"]);
 assert.equal(r.usage.at(-1)!.is_error, true);
});

it("subscription refresh uses the existing bridge and removes refreshed credential custody", async () => {
 const r = rig({ subscription: true, refresh: true,
  notes: (emit) => emit({ id: 91, method: "account/chatgptAuthTokens/refresh",
   params: { reason: "unauthorized", previousAccountId: "untrusted-hint" } }),
  onToolReply: (frame, emit) => {
   assert.equal(frame.id, 91);
   assert.equal(frame.result.accessToken, "refreshed-token");
   assert.equal(frame.result.chatgptAccountId, "account");
   r.terminal(emit);
  },
 });
 await r.run();
 assert.equal(r.refreshes.length, 1);
 const refresh = r.refreshes[0] as any;
 assert.equal(refresh.request.observed_generation, 7);
 assert.equal(refresh.expected.chatgptAccountId, "account");
 assert.equal(r.secrets.size, 0);
});

it("same-chunk protocol poison cannot hide behind a queued valid terminal", async () => {
 const r = rig({ notes: (emit) => {
  r.terminal(emit);
  // 10,001 additional framed lines trip the lifetime frame cap before consumption.
  for (let i = 0; i < 10001; i++) emit({ method: "activity" });
 } });
 await assert.rejects(r.run(), /protocol budget|overflow/);
 assert.equal(r.usage.at(-1)!.is_error, true);
});

for (const [field, cap] of [["issue_title", 4096], ["issue_description", 256 * 1024]] as const) {
 it("accepts exact UTF-8 prompt boundary and refuses +1 byte: " + field, () => {
  const c = claim();
  c[field] = "é".repeat(cap / 2);
  assert.doesNotThrow(() => crossCheckPrompt(c));
  c[field] += "x";
  assert.throws(() => crossCheckPrompt(c), /cap/);
 });
}

it("accepts matching streamed verdict at the full text boundary without double charging", async () => {
 const text = verdict + " ".repeat(65536 - Buffer.byteLength(verdict));
 const r = rig({ notes: (emit) => {
  emit({ method: "item/agentMessage/delta", params: {
   threadId: "thread", turnId: "turn", itemId: "assistant", delta: text } });
  r.terminal(emit, text);
 } });
 assert.equal(await r.run(), text);
});

it("refuses streamed prefixes and suffixes omitted by the completed item", async () => {
 for (const delta of ["prefix " + verdict, verdict + " suffix"]) {
  const r = rig({ notes: (emit) => {
   emit({ method: "item/agentMessage/delta", params: {
    threadId: "thread", turnId: "turn", itemId: "assistant", delta } });
   r.terminal(emit);
  } });
  await assert.rejects(r.run(), /disagrees/);
 }
});

it("fences untrusted prompt fields and rejects oversized candidate inputs before launch", () => {
 assert.match(crossCheckPrompt(claim()), /<candidate_[a-f0-9]{32}>/);
 const c = claim();
 c.cross_check!.planning_diff = "x".repeat(512 * 1024 + 1);
 assert.throws(() => crossCheckPrompt(c), /cap/);
});
