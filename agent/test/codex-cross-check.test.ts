import { it } from "node:test";
import assert from "node:assert/strict";
import { PassThrough, Writable } from "node:stream";
import { CodexCrossCheck, crossCheckPrompt, type CrossCheckModelDeps } from "../src/codex/cross-check.js";
import type { CodexRootHandle, CodexLaunchSpec, CodexEffectLaunchSpec } from "../src/codex/launcher.js";
import type { ClaimResponse } from "../src/protocol.js";
import type { WorkerClient } from "../src/client.js";
import type { Logger } from "../src/log.js";
import { recordingLogger } from "./helpers.js";
import { CrossCheckCheckerUnavailableError } from "../src/codex/model-rejection.js";

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
 round?: number;
 notes?: (emit: Emit) => void;
 beforeResponse?: (frame: Frame, emit: Emit) => void;
 afterResponse?: (frame: Frame, emit: Emit) => void;
 coalesced?: boolean;
 queuedResponse?: boolean;
 failUsage?: boolean;
 claim?: ClaimResponse;
 rpcFailure?: string;
 rpcError?: unknown;
 authFailure?: boolean;
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
    if (options.coalesced) {
     const frames: unknown[] = [];
     let gathering = true;
     onFrame(JSON.parse(line), (frame) => { if (gathering) frames.push(frame); else emit(frame); });
     gathering = false;
     stdout.write(frames.map((frame) => JSON.stringify(frame) + "\n").join(""));
    } else onFrame(JSON.parse(line), emit);
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
  options.beforeResponse?.(frame, emit);
  if ((options.rpcFailure && frame.method === options.rpcFailure) || (options.authFailure && frame.method === "account/login/start")) {
   const respond = (): void => { emit({ id: frame.id, error: options.rpcError }); };
   if (options.queuedResponse) queueMicrotask(respond); else respond();
  } else if (frame.method === "initialize") emit({ id: frame.id, result: { userAgent: "fake", codexHome: "/owned", platformFamily: "unix", platformOs: "linux" } });
  else if (frame.method === "account/login/start") emit({ id: frame.id, result: { type: options.subscription ? "chatgptAuthTokens" : "apiKey" } });
  else if (frame.method === "thread/start") emit({ id: frame.id, result: { thread: { id: "thread" } } });
  else if (frame.method === "turn/start") {
   emit({ id: frame.id, result: { turn: { id: "turn" } } });
   queueMicrotask(() => { if (!options.holdTurn) (options.notes ?? terminal)(emit); });
  } else if (!frame.method && frame.id !== undefined) options.onToolReply?.(frame, emit);
  options.afterResponse?.(frame, emit);
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
 let releases = 0;
 const client: Pick<WorkerClient, "releaseCodex" | "refreshCodex"> = {
  releaseCodex: async () => { releases++; return options.subscription
   ? { auth_mode: "subscription", access_token: "released-token", generation: 7, chatgpt_account_id: "account", chatgpt_plan_type: null }
   : { auth_mode: "api_key", access_token: "released-token" }; },
  refreshCodex: async (_run, request, expected) => {
   if (!options.refresh) throw new Error("unused");
   refreshes.push({ request, expected });
   return { auth_mode: "subscription", access_token: "refreshed-token", generation: 8,
    chatgpt_account_id: "account", chatgpt_plan_type: null, outcome: "advanced" };
  },
 };
 const recorded = recordingLogger();
 const log = { ...recorded.logger, addSecret: (s: string) => secrets.add(s), removeSecret: (s: string) => secrets.delete(s) } as unknown as Logger;
 const checker = new CodexCrossCheck(client, log, deps);
 return { logs: recorded.lines, get releases() { return releases; }, requests, ops, specs, disposed, secrets, usage, refreshes, terminal, fail: () => failProvider(new Error("root failed")),
  run: (signal = new AbortController().signal) => {
   const c = options.claim ?? claim(options.subscription);
   if (options.round !== undefined) c.cross_check!.round = options.round;
   return checker.run(c, "/checkout", "/owned", signal, async (payload) => {
    usage.push(payload); if (options.failUsage && payload.event === "result") throw new Error("usage failed");
   });
  } };
}

for (const pinned of [false, true]) {
it("round two runs the confined checker and fences its round without a child budget snapshot: " + (pinned ? "pinned" : "default"), async () => {
 const c = pinned ? pinnedClaim() : claim();
 assert.equal("automatic_revision_limit" in c.cross_check!, false);
 assert.equal("automatic_rounds_enabled" in c.cross_check!, false);
 const r = rig({ round: 2, claim: c });
 assert.equal(await r.run(), verdict);
 assert.equal(r.specs.length, 2);
 assert.ok("args" in r.specs[0]! && r.specs[0].args.includes("--cross-check"));
 const turn = r.requests.find((frame) => frame.method === "turn/start");
 assert.ok(turn);
 const candidate = JSON.parse(/<candidate_[a-f0-9]{32}>\n([\s\S]*?)\n<\/candidate_[a-f0-9]{32}>/.exec(turn.params!.input[0].text)![1]!);
 assert.equal(candidate.round, 2);
 if (pinned) {
  assert.equal(candidate.model_source, "pin");
  assert.equal(candidate.effort_source, "pin");
  assertOneAttempt(r);
 }
 assert.deepEqual(r.ops, ["stat"]);
 assert.deepEqual(r.disposed, ["provider", "fileop"]);
});
}

it("checker rejects rounds outside positive integers one through five before launch", async () => {
 for (const round of [0, 6, 1.5, NaN]) {
  const r = rig({ round });
  await assert.rejects(r.run(), /unsupported cross-check candidate/);
  assert.deepEqual(r.specs, []);
 }
 const c = claim();
 c.cross_check!.round = 5;
 assert.match(crossCheckPrompt(c), /"round":5/);
});

// Synthetic 0.159.3 local Responses HTTP 400 selected frame (#2151),
// copied after inspecting the capture/schema. Not hosted entitlement evidence.
const capturedError = {
 message: JSON.stringify({ error: { message: "SYNTHETIC model rejection marker B",
  type: "invalid_request_error", param: "model", code: "model_not_found" } }),
 codexErrorInfo: "other", additionalDetails: null, misalignment: null,
};
function pinnedClaim(): ClaimResponse {
 const c = claim();
 c.config = { default_model: "gpt-6-astra", default_effort: "xhigh" };
 c.cross_check!.model_source = "pin";
 c.cross_check!.effort_source = "pin";
 return c;
}
function rejection(emit: Emit, error: unknown = capturedError, overrides: Record<string, unknown> = {}): void {
 emit({ method: "error", params: { threadId: "thread", turnId: "turn", willRetry: false, error, ...overrides } });
}
function failedTerminal(emit: Emit, error: unknown = capturedError, threadId = "thread", id = "turn", items: unknown[] = []): void {
 emit({ method: "turn/completed", params: { threadId, turn: { id, status: "failed", items, error } } });
}
function assertOneAttempt(r: ReturnType<typeof rig>, turn = true): void {
 assert.equal(r.specs.filter((s) => "kind" in s && s.kind === "provider").length, 1);
 assert.equal(r.requests.filter((f) => f.method === "thread/start").length, 1);
 assert.equal(r.requests.filter((f) => f.method === "turn/start").length, turn ? 1 : 0);
 for (const frame of r.requests.filter((f) => f.method === "thread/start" || f.method === "turn/start")) {
  assert.equal(frame.params!.model, "gpt-6-astra");
  if (frame.method === "turn/start") assert.equal(frame.params!.effort, "xhigh");
 }
 assert.deepEqual(r.disposed, ["provider", "fileop"]);
 assert.equal(r.secrets.size, 0);
}
for (const round of [1, 2]) for (const mode of ["async", "terminal", "thread/start", "turn/start"] as const) {
 it("maps pinned startup rejection over real transport: " + mode + " round " + round, async () => {
  const r = rig({ round, claim: pinnedClaim(),
   ...(mode.includes("/") ? { rpcFailure: mode, rpcError: { code: -32000, message: capturedError.message } } : {}),
   notes: (emit) => mode === "terminal" ? failedTerminal(emit) : rejection(emit) });
  await assert.rejects(r.run(), CrossCheckCheckerUnavailableError);
  assertOneAttempt(r, mode !== "thread/start");
 });
}
for (const round of [1, 2]) for (const source of [undefined, "worker default", "effort-only"] as const) {
 it("retains model error without model pin: " + source + " round " + round, async () => {
  const c = pinnedClaim();
  if (source === "worker default") c.cross_check!.model_source = source;
  else delete c.cross_check!.model_source;
  if (source !== "effort-only") delete c.cross_check!.effort_source;
  const r = rig({ round, claim: c, notes: (emit) => rejection(emit) });
  await assert.rejects(r.run(), (e: Error) => !(e instanceof CrossCheckCheckerUnavailableError) && /model error/.test(e.message));
  assertOneAttempt(r);
 });
}
for (const effort of ["", "minimal", "XHIGH", " xhigh", "xhigh ", "unknown"]) {
 it("refuses pinned effort before release: " + JSON.stringify(effort), async () => {
  const c = pinnedClaim(); c.config!.default_effort = effort as NonNullable<ClaimResponse["config"]>["default_effort"];
  const r = rig({ claim: c });
  await assert.rejects(r.run(), CrossCheckCheckerUnavailableError);
  assert.equal(r.releases, 0); assert.equal(r.specs.length, 0); assert.equal(r.requests.length, 0);
 });
}
for (const effort of ["low", "medium", "high", "xhigh", "max"] as const) {
 it("passes pinned effort unchanged: " + effort, async () => {
  const c = pinnedClaim(); c.config!.default_effort = effort;
  const r = rig({ claim: c });
  assert.equal(await r.run(), verdict);
  assert.equal(r.requests.find((f) => f.method === "turn/start")!.params!.effort, effort);
 });
}
const otherCode = JSON.stringify({ error: { message: "model missing or unavailable", code: "unsupported_model",
 param: "model", type: "invalid_request_error" } });
const negatives: [string, unknown, Record<string, unknown>?][] = [
 ["generic400", { message: "400 model missing", codexErrorInfo: "badRequest" }],
 ["captured404", { message: "unexpected status 404 Not Found: SYNTHETIC model rejection marker A",
  codexErrorInfo: { httpConnectionFailed: { httpStatusCode: 404 } } }],
 ["othercode", { ...capturedError, message: otherCode }],
 ["prose", { ...capturedError, message: "model_not_found: model does not exist" }],
 ["badRequest", { ...capturedError, codexErrorInfo: "badRequest" }],
 ["auth", { ...capturedError, codexErrorInfo: "unauthorized" }],
 ["rate", { ...capturedError, codexErrorInfo: "rateLimitExceeded" }],
 ["transport", { ...capturedError, codexErrorInfo: { httpConnectionFailed: { httpStatusCode: 400 } } }],
 ["confinement", { ...capturedError, codexErrorInfo: "sandboxError" }],
 ["foreignthread", capturedError, { threadId: "foreign" }],
 ["foreignturn", capturedError, { turnId: "foreign" }],
 ["retrying", capturedError, { willRetry: true }],
 ["malformed", capturedError, { willRetry: "false" }],
 ["array", { ...capturedError, message: "[" + capturedError.message + "]" }],
 ["nestedstring", { ...capturedError, message: JSON.stringify(capturedError.message) }],
 ["oversize", { ...capturedError, message: capturedError.message + " ".repeat(8192) }],
];
for (const round of [1, 2]) for (const [label, error, overrides] of negatives) {
 it("does not map negative rejection control: " + label + " round " + round, async () => {
  const r = rig({ round, claim: pinnedClaim(), notes: (emit) => rejection(emit, error, overrides) });
  await assert.rejects(r.run(), (e: Error) => !(e instanceof CrossCheckCheckerUnavailableError));
  assertOneAttempt(r);
 });
}
for (const round of [1, 2]) for (const activity of ["delta", "reasoning", "tool", "completed", "terminal-items", "usage-output"]) {
 it("does not reclassify after model activity: " + activity + " round " + round, async () => {
  const r = rig({ round, claim: pinnedClaim(), notes: (emit) => {
   if (activity === "terminal-items") { failedTerminal(emit, capturedError, "thread", "turn", [{ type: "reasoning" }]); return; }
   if (activity === "usage-output") {
    const usage = { totalTokens: 2, inputTokens: 1, cachedInputTokens: 0, cacheWriteInputTokens: 0, outputTokens: 1, reasoningOutputTokens: 0 };
    emit({ method: "thread/tokenUsage/updated", params: { threadId: "thread", turnId: "turn", tokenUsage: { total: usage, last: usage } } });
    rejection(emit); return;
   }
   emit({ ...(activity === "tool" ? { id: 90 } : {}),
    method: activity === "delta" ? "item/agentMessage/delta" : activity === "tool" ? "item/tool/call"
     : activity === "reasoning" ? "item/reasoning/textDelta" : "item/completed",
    params: { threadId: "thread", turnId: "turn", itemId: "assistant", delta: "output",
     callId: "read", tool: "Read", arguments: { path: "anchor.ts" },
     item: { type: "agentMessage", id: "assistant", text: verdict } } });
   rejection(emit);
  } });
  await assert.rejects(r.run(), (e: Error) => !(e instanceof CrossCheckCheckerUnavailableError));
  assertOneAttempt(r);
 });
}
it("does not map authentication RPC rejection", async () => {
 const r = rig({ claim: pinnedClaim(), authFailure: true, rpcError: { code: -32000, message: capturedError.message } });
 await assert.rejects(r.run(), (e: Error) => !(e instanceof CrossCheckCheckerUnavailableError));
 assert.equal(r.requests.filter((f) => f.method === "thread/start").length, 0);
 assert.deepEqual(r.disposed, ["provider", "fileop"]);
});
it("does not classify unmatched RPC after authentication", async () => {
 const r = rig({ claim: pinnedClaim(), notes: (emit) => emit({ id: 9999, error: { code: -32000, message: capturedError.message } }) });
 await assert.rejects(r.run(), (e: Error) => !(e instanceof CrossCheckCheckerUnavailableError));
 assertOneAttempt(r);
});
it("cleanup failure overrides mapped model rejection and disposes both roots", async () => {
 const r = rig({ claim: pinnedClaim(), failDispose: true, notes: (emit) => rejection(emit) });
 await assert.rejects(r.run(), /cleanup unconfirmed/); assertOneAttempt(r);
});
it("cancellation retains disposition before rejection consumption", async () => {
 const controller = new AbortController();
 const r = rig({ claim: pinnedClaim(), notes: (emit) => { rejection(emit); controller.abort(); } });
 await assert.rejects(r.run(controller.signal), (e: Error) => !(e instanceof CrossCheckCheckerUnavailableError));
 assertOneAttempt(r);
});

for (const bucket of ["outputTokens", "reasoningOutputTokens"] as const) {
 for (const mode of ["async", "terminal"] as const) {
  it("cumulative " + bucket + " closes startup with zero last usage: " + mode, async () => {
   const r = rig({ claim: pinnedClaim(), notes: (emit) => {
    const zero = { totalTokens: 0, inputTokens: 0, cachedInputTokens: 0, cacheWriteInputTokens: 0, outputTokens: 0, reasoningOutputTokens: 0 };
    emit({ method: "thread/tokenUsage/updated", params: { threadId: "thread", turnId: "turn",
     tokenUsage: { total: { ...zero, [bucket]: 1 }, last: zero } } });
    if (mode === "async") rejection(emit); else failedTerminal(emit);
   } });
   await assert.rejects(r.run(), (e: Error) => !(e instanceof CrossCheckCheckerUnavailableError));
   assertOneAttempt(r);
  });
 }
}
for (const timing of ["coalesced", "queued"] as const) {
 for (const binding of ["bound", "foreign-thread", "foreign-turn", "missing-start"] as const) {
  it("arrival activity before rejected turn RPC: " + timing + "/" + binding, async () => {
   const r = rig({ claim: pinnedClaim(), coalesced: timing === "coalesced", queuedResponse: timing === "queued",
    rpcFailure: "turn/start", rpcError: { code: -32000, message: capturedError.message },
    beforeResponse: (frame, emit) => {
     if (frame.method !== "turn/start") return;
     if (binding !== "missing-start") emit({ method: "turn/started", params: {
      threadId: binding === "foreign-thread" ? "foreign" : "thread", turn: { id: "turn" } } });
     emit({ method: "item/started", params: { threadId: "thread",
      turnId: binding === "foreign-turn" ? "foreign" : "turn", item: { type: "reasoning" } } });
    } });
   await assert.rejects(r.run(), (e: Error) => binding === "bound"
    ? !(e instanceof CrossCheckCheckerUnavailableError) : e instanceof CrossCheckCheckerUnavailableError);
   assertOneAttempt(r);
  });
 }
}
for (const phase of ["initialize", "account/login/start", "turn/start"] as const) {
 for (const mode of ["async", "terminal"] as const) {
  it("rejection arrival provenance: " + phase + "/" + mode, async () => {
   const r = rig({ claim: pinnedClaim(), coalesced: true, holdTurn: true,
    beforeResponse: (frame, emit) => {
     if (frame.method === phase) { if (mode === "async") rejection(emit); else failedTerminal(emit); }
    } });
   await assert.rejects(r.run(), (e: Error) => phase === "turn/start"
    ? e instanceof CrossCheckCheckerUnavailableError : !(e instanceof CrossCheckCheckerUnavailableError));
   assertOneAttempt(r);
  });
 }
}
for (const method of ["thread/start", "turn/start"] as const) {
 it("coalesced malformed frame preserves RPC protocol failure: " + method, async () => {
  const r = rig({ claim: pinnedClaim(), coalesced: true, rpcFailure: method,
   rpcError: { code: -32000, message: capturedError.message },
   afterResponse: (frame, emit) => { if (frame.method === method) emit([]); } });
  await assert.rejects(r.run(), /received a non-object frame/);
  assertOneAttempt(r, method !== "thread/start");
 });
}
for (const cleanupFails of [false, true]) {
 it("original failure survives usage failure with cleanup priority: " + cleanupFails, async () => {
  const r = rig({ claim: pinnedClaim(), failUsage: true, failDispose: cleanupFails, notes: (emit) => rejection(emit) });
  await assert.rejects(r.run(), (e: Error) => cleanupFails ? /cleanup unconfirmed/.test(e.message)
   : e instanceof CrossCheckCheckerUnavailableError);
  assertOneAttempt(r);
  assert.equal(r.usage.at(-1)!.is_error, true);
 });
}
it("successful verdict preserves usage settlement failure and removes secrets", async () => {
 const r = rig({ failUsage: true });
 await assert.rejects(r.run(), /usage failed/);
 assert.deepEqual(r.disposed, ["provider", "fileop"]);
 assert.equal(r.secrets.size, 0);
});

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
   reasoningOutputTokens: 1, costStatus: "unreported",
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

for (const mode of ["rpc", "async", "terminal", "prose", "data"] as const) {
 it("provider prose, raw data and secrets never enter new error/usage/logs: " + mode, async () => {
  const secret = "glpat-" + "0123456789abcdefghij";
  const message = JSON.stringify({ error: { message: secret, code: "model_not_found",
   param: "model", type: "invalid_request_error" } });
  const raw = { ...capturedError, message: mode === "prose" ? "model_not_found " + secret : message,
   additionalDetails: secret, data: { raw: secret }, cause: secret };
  const r = rig({ claim: pinnedClaim(),
   ...(mode === "rpc" || mode === "data" ? { rpcFailure: "turn/start", rpcError: {
    code: -32000, message: mode === "data" ? secret : message,
    data: { error: { code: "model_not_found", param: "model", type: "invalid_request_error" }, secret },
   } } : {}),
   notes: (emit) => mode === "terminal" ? failedTerminal(emit, raw) : rejection(emit, raw) });
  await assert.rejects(r.run(), (e: Error) => {
   assert.equal(e instanceof CrossCheckCheckerUnavailableError, mode !== "prose" && mode !== "data");
   assert.equal(String(e).includes(secret), false);
   assert.equal(JSON.stringify(e).includes(secret), false);
   assert.equal(Object.hasOwn(e, "cause"), false);
   return true;
  });
  assert.equal(JSON.stringify(r.logs).includes(secret), false);
  assert.equal(JSON.stringify(r.usage).includes(secret), false);
  assertOneAttempt(r);
 });
}
for (const ids of [{ threadId: "foreign", id: "turn" }, { threadId: "thread", id: "foreign" }]) {
 it("terminal fallback requires matching thread and turn: " + JSON.stringify(ids), async () => {
  const r = rig({ claim: pinnedClaim(), notes: (emit) => failedTerminal(emit, capturedError, ids.threadId, ids.id) });
  await assert.rejects(r.run(), (e: Error) => !(e instanceof CrossCheckCheckerUnavailableError) && /foreign/.test(e.message));
  assertOneAttempt(r);
 });
}

it("subscription checker posts a metered API-equivalent amount for a known model", async () => {
 const c = claim(true);
 c.config = { default_model: "gpt-6.1-sol", default_effort: "high" };
 const r = rig({ subscription: true, claim: c, notes: (emit) => {
  const bucket = { inputTokens: 1000, cachedInputTokens: 600, cacheWriteInputTokens: 100, outputTokens: 200, reasoningOutputTokens: 50, totalTokens: 1200 };
  emit({ method: "thread/tokenUsage/updated", params: { threadId: "thread", turnId: "turn", tokenUsage: { total: bucket, last: bucket } } });
  r.terminal(emit);
 } });
 await r.run();
 assert.deepEqual((r.usage.at(-1)!.modelUsage as any)["gpt-6.1-sol"], {
  inputTokens: 300, cacheReadInputTokens: 600, cacheCreationInputTokens: 100,
  outputTokens: 200, reasoningOutputTokens: 50, costStatus: "metered", costUSD: 0.00291,
 });
});
