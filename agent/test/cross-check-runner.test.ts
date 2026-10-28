import { it, mock } from "node:test";
import assert from "node:assert/strict";
import { PassThrough, Writable } from "node:stream";
import { CodexCrossCheck, CrossCheckMalformedError, type CrossCheckModelDeps } from "../src/codex/cross-check.js";
import type { CodexRootHandle, CodexLaunchSpec, CodexEffectLaunchSpec } from "../src/codex/launcher.js";
import type { ClaimResponse } from "../src/protocol.js";
import { WorkerClient, RequestError } from "../src/client.js";
import type { Logger } from "../src/log.js";
import { MessageBatcher } from "../src/batcher.js";
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
 return { deps, log, requests, ops, specs, disposed, secrets, usage, refreshes, terminal, fail: () => failProvider(new Error("root failed")),
  run: (signal = new AbortController().signal) => checker.run(claim(options.subscription), "/checkout", "/owned", signal,
   async (payload) => { usage.push(payload); }) };
}

import http from "node:http";
import fs from "node:fs/promises";
import path from "node:path";
import { CrossCheckRunner } from "../src/cross-check-runner.js";
import { GitCache } from "../src/git.js";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { Outbox } from "../src/outbox.js";
import { nullLogger, recordingLogger } from "./helpers.js";

await fs.mkdir(path.resolve(import.meta.dirname, "../../.uzi/scratch"), { recursive: true });

for (const round of [1, 2]) for (const mode of ["approve", "approve-round2", "approve-pinned", "invalidfinding", "rejection", "terminal-rejection", "legacy-rejection", "default-rejection", "unrelated-rejection", "activity-rejection", "cleanup-rejection", "invalid-effort"] as const) {
 if (round === 2 && mode === "approve-round2") continue;
it("outer checker uses real Read broker, actual HTTP verdict delivery and journaled child completion: " + mode + " round " + (mode === "approve-round2" ? 2 : round), async () => {
 const approves = mode === "approve" || mode === "approve-round2" || mode === "approve-pinned";
 const expectedVerdict = approves ? "approve" : "failed";
 const expectedReason = approves ? "approve" : mode === "invalidfinding" ? "malformed"
  : ["legacy-rejection", "default-rejection", "unrelated-rejection", "activity-rejection"].includes(mode) ? "model_error"
  : mode === "cleanup-rejection" ? "confinement_failed" : "checker_unavailable";
 const modelRejection = {
  message: JSON.stringify({ error: { message: "SYNTHETIC model rejection marker B",
   type: "invalid_request_error", param: "model", code: "model_not_found" } }),
  codexErrorInfo: "other", additionalDetails: null, misalignment: null,
 };
 const rejected = !approves && mode !== "invalidfinding";
 const expectedStatus = approves ? "completed" : "failed";
 const assistantText = approves ? verdict : JSON.stringify({
  verdict: "approve", summary: "Anchors checked",
  items: [{ file: "anchor.ts", severity: "invalid", summary: "Finding", rationale: "Read anchor" }],
 });
 const root = await fs.mkdtemp(path.join(path.resolve(import.meta.dirname, "../../.uzi/scratch"), "cross-check-test-"));
 const outbox = new Outbox({ root: path.join(root, "outbox"), log: nullLogger(),
  runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86400000 });
 await outbox.init();
 const registry = new ActiveRunRegistry();
 const posts: { url: string; body: any }[] = [];
 const r = rig({
  failDispose: mode === "cleanup-rejection",
  notes: (emit) => {
   if (rejected) {
    if (mode === "activity-rejection") emit({ method: "item/reasoning/textDelta", params: {
     threadId: "thread", turnId: "turn", itemId: "reasoning", delta: "Checking anchors" } });
    const error = mode === "unrelated-rejection" ? { ...modelRejection, codexErrorInfo: "unauthorized" } : modelRejection;
    if (mode === "terminal-rejection") emit({ method: "turn/completed", params: {
     threadId: "thread", turn: { id: "turn", items: [], status: "failed", error: modelRejection } } });
    else emit({ method: "error", params: { threadId: "thread", turnId: "turn", willRetry: false, error } });
    return;
   }
   emit({ id: 90, method: "item/tool/call", params: {
   threadId: "thread", turnId: "turn", callId: "read", tool: "Read", arguments: { path: "anchor.ts" } } }); },
  onToolReply: (frame, emit) => {
   assert.equal(frame.result.success, true);
   const excerpt = /^<(repository_excerpt_[a-f0-9]{32})>\n([\s\S]*)\n<\/\1>$/.exec(frame.result.contentItems[0].text);
   assert.ok(excerpt, "Read output remains inside its untrusted repository fence");
   assert.deepEqual(JSON.parse(excerpt[2]!), {
    size: Buffer.byteLength("export const anchor = true;"), content: "export const anchor = true;", offset: 1,
    linesReturned: 1, partialLastLine: false, truncated: false,
   });
   r.terminal(emit, assistantText);
  },
 });
 const c = { ...claim(), claim_generation: 7 };
 c.cross_check!.round = mode === "approve-round2" ? 2 : round;
 assert.equal("automatic_revision_limit" in c.cross_check!, false);
 assert.equal("automatic_rounds_enabled" in c.cross_check!, false);
 if (rejected || mode === "approve-pinned") {
  c.config = { default_model: "gpt-6-astra", default_effort: (mode === "invalid-effort" ? "xhigh " : "xhigh") as NonNullable<ClaimResponse["config"]>["default_effort"] };
  if (mode !== "legacy-rejection") c.cross_check!.model_source = mode === "default-rejection" ? "worker default" : "pin";
  c.cross_check!.effort_source = "pin";
 }
 // Only the selected child integration test accepts the regression injection.
 const injection = mode === "approve" && round === 1 ? process.env.UZI_CROSS_CHECK_HANDLER_FAILURE : undefined;
 const injected: string[] = [];
 let hasHandlerError = false;
 let handlerError: unknown;
 const retainHandlerError = (error: unknown) => {
  if (!hasHandlerError) { hasHandlerError = true; handlerError = error; }
 };
 const server = http.createServer(async (req, res) => {
  let response = "{}";
  try {
   let raw = "";
   for await (const chunk of req) raw += chunk;
   const body = raw ? JSON.parse(raw) : {};
   if (req.method === "POST") posts.push({ url: req.url!, body });
   res.setHeader("Content-Type", "application/json");
   if (req.url?.endsWith("/codex/release")) response = JSON.stringify({ auth_mode: "api_key", access_token: "released-token" });
   else if (req.url?.endsWith("/inputs")) response = JSON.stringify({ inputs: [] });
   else if (req.url?.endsWith("/cross-check-verdict")) {
    if (injection === "verdict" || injection === "both") {
     injected.push("verdict");
     assert.fail("cross-check handler injected verdict assertion");
    }
    assert.equal(req.url, "/api/worker/runs/check/cross-check-verdict");
    assert.equal(body.claim_generation, 7);
    assert.equal(body.verdict, expectedVerdict);
    assert.equal(body.reason_class, expectedReason);
   } else if (req.url?.endsWith("/state")) {
    response = JSON.stringify({ applied: true, status: body.status });
    if (body.status === "completed" || body.status === "failed") {
     if (injection === "state" || injection === "both") {
      injected.push("state");
      assert.fail("cross-check handler injected state assertion");
     }
     assert.equal(body.status, expectedStatus);
     assert.equal(outbox.hasPendingTerminal(c.run_id, 7), true);
     const journal = await outbox.readTerminalJournal(c.run_id, 7);
     assert.equal(journal?.body.status, expectedStatus);
     assert.equal(journal?.body.claim_generation, 7);
     // marker, init, result and summary all ride the batcher; the fence is its closed tail. The modes
     // whose model reaches the real Read broker also record one check_activity row (PRD #3166).
     assert.equal(journal?.messagesThroughSeq, mode === "invalid-effort" ? 2 : rejected ? 4 : 5);
    }
   }
  } catch (error) {
   retainHandlerError(error);
  } finally {
   try { res.end(response); } catch (error) {
    retainHandlerError(error);
    // An unusable response must not leave its socket waiting for a reply.
    try { res.destroy(); } catch (destroyError) { retainHandlerError(destroyError); }
   }
  }
 });
 await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
 const address = server.address() as { port: number };
 const client = new WorkerClient("http://127.0.0.1:" + address.port, "join-fixture", "test", nullLogger());
 const clones: unknown[][] = [];
 const removed: string[] = [];
 const git = {
  ensureClone: async () => "/bare",
  runnerCloneAtCommit: async (...args: unknown[]) => {
   assert.equal(registry.has(c.run_id), true);
   clones.push(args); return "/checkout";
  },
  removeRunnerClone: async (checkout: string) => { removed.push(checkout); },
 } as unknown as GitCache;
 let hasTestError = false;
 let testError: unknown;
 try {
  let hasExecutionError = false;
  let executionError: unknown;
  try {
   await new CrossCheckRunner(client, git, nullLogger(), { homeRoot: root, pollMs: 1,
    activeRuns: registry, outbox, model: new CodexCrossCheck(client, r.log, r.deps) }).execute(c);
  } catch (error) { hasExecutionError = true; executionError = error; }
  if (injection) console.log("CROSS_CHECK_HANDLER_OBSERVATION " + JSON.stringify({
   injected, executionSettled: true, executionFailed: hasExecutionError,
   verdictPosts: posts.filter((p) => p.url.endsWith("/cross-check-verdict")).length,
   terminalPosts: posts.filter((p) => p.url.endsWith("/state") && ["completed", "failed"].includes(p.body.status)).length,
   journalCleared: !outbox.hasPendingTerminal(c.run_id, 7),
   registryRemoved: registry.size === 0, checkoutRemoved: removed.length === 1 && removed[0] === "/checkout",
  }));
  if (hasHandlerError) throw handlerError;
  if (hasExecutionError) throw executionError;
  assert.deepEqual(clones, [["/bare", c.cross_check!.base_commit, c.run_id]]);
  assert.deepEqual(r.ops, mode === "invalid-effort" ? [] : rejected ? ["stat"] : ["stat", "read"]);
  assert.deepEqual(r.disposed, mode === "invalid-effort" ? [] : ["provider", "fileop"]);
  const delivered = posts.filter((p) => p.url.endsWith("/messages")).flatMap((p) => p.body.messages as any[]);
  // A mode whose model reaches the real Read broker also records that read as one check_activity row.
  const events = mode === "invalid-effort" ? ["check_activity_start", "check_activity_summary"]
   : rejected ? ["check_activity_start", "init", "result", "check_activity_summary"]
   : ["check_activity_start", "init", "check_activity", "result", "check_activity_summary"];
  assert.deepEqual(delivered.map((m) => m.seq), events.map((_e, i) => i + 1));
  assert.deepEqual(delivered.map((m) => m.payload.event), events);
  if (mode !== "invalid-effort") assert.equal(delivered.find((m) => m.payload.event === "result")!.payload.is_error, !approves);
  if (!rejected) {
   const read = delivered.find((m) => m.payload.event === "check_activity")!.payload;
   assert.equal(read.op, "read");
   assert.equal(read.outcome, "ok");
  }
  // A pinned effort the checker refuses throws before the provider launches, so capture is incomplete.
  assert.equal(delivered.at(-1)!.payload.complete, mode !== "invalid-effort");
  assert.equal(delivered.at(-1)!.payload.generation, 7);
  if (mode !== "invalid-effort") {
   const start = r.requests.find((f) => f.method === "turn/start")!;
   const candidate = JSON.parse(/<candidate_[a-f0-9]{32}>\n([\s\S]*?)\n<\/candidate_[a-f0-9]{32}>/.exec(start.params!.input[0].text)![1]!);
   assert.equal(candidate.round, c.cross_check!.round);
   assert.equal(candidate.model_source, c.cross_check!.model_source);
   assert.equal(candidate.effort_source, c.cross_check!.effort_source);
  }
  if (rejected || mode === "approve-pinned") {
   const starts = r.requests.filter((f) => f.method === "turn/start");
   assert.equal(starts.length, mode === "invalid-effort" ? 0 : 1);
   assert.equal(r.specs.filter((s) => "kind" in s && s.kind === "provider").length, mode === "invalid-effort" ? 0 : 1);
   if (starts.length) {
    assert.equal(starts[0]!.params!.model, "gpt-6-astra");
    assert.equal(starts[0]!.params!.effort, "xhigh");
   }
   assert.equal(JSON.stringify(posts).includes("SYNTHETIC"), false);
  }
  assert.equal(r.secrets.size, 0);
  assert.ok(posts.every((p) => p.url.includes("/runs/check/")));
  const verdictIndex = posts.findIndex((p) => p.url.endsWith("/cross-check-verdict"));
  assert.ok(verdictIndex > 0);
  assert.ok(posts.slice(verdictIndex).every((p) => !p.url.endsWith("/messages")), "every row is delivered before the verdict");
  assert.equal(posts.filter((p) => p.url.endsWith("/cross-check-verdict")).length, 1);
  assert.equal(posts.some((p) => p.url.includes("/cross-checks/plan")), false, "the child never submits an automatic revision");
  assert.equal(posts[verdictIndex]!.body.verdict, expectedVerdict);
  assert.equal(posts[verdictIndex]!.body.reason_class, expectedReason);
  assert.equal(posts[verdictIndex + 1]!.body.status, expectedStatus);
  if (expectedStatus === "failed") assert.equal(posts[verdictIndex + 1]!.body.failure_reason,
   expectedReason === "checker_unavailable"
    ? "plan cross-check: checker unavailable" : "The cross-check did not complete.");
  assert.equal(outbox.hasPendingTerminal(c.run_id, 7), false);
  assert.equal(registry.size, 0);
  assert.deepEqual(removed, ["/checkout"]);
 } catch (error) { hasTestError = true; testError = error; }
 finally {
  try {
   try {
    server.closeAllConnections();
    await new Promise<void>((resolve, reject) => server.close((err) => err ? reject(err) : resolve()));
   } finally { await fs.rm(root, { recursive: true, force: true }); }
  } catch (error) {
   if (!hasTestError) { hasTestError = true; testError = error; }
  }
 }
 if (hasHandlerError) throw handlerError;
 if (hasTestError) throw testError;
});
}

for (const mode of ["stale-running", "stale-verdict", "malformed", "delivery", "timeout", "cancel", "lost-ack", "lost-terminal-ack", "cancel-probe", "timeout-probe", "finding-schema"] as const) {
 it("outer checker fails closed or abandons: " + mode, async () => {
  const root = await fs.mkdtemp(path.join(path.resolve(import.meta.dirname, "../../.uzi/scratch"), "checker-failure-"));
  const states: string[] = [];
  const decisions: any[] = [];
  let modelCalls = 0;
  let clones = 0;
  let removals = 0;
  const cancel = new AbortController();
  const c = { ...claim(), claim_generation: 7 };
  const recorded = recordingLogger();
  const client = {
   reportState: async (_id: string, body: any) => {
    states.push(body.status);
    if (states.length === 2 && mode === "cancel-probe") cancel.abort();
    if (states.length === 2 && mode === "timeout-probe") await new Promise((resolve) => setTimeout(resolve, 2000));
    if (mode === "lost-terminal-ack" && body.status === "completed") throw new Error("terminal ACK lost");
    return mode === "stale-running" || (mode === "stale-verdict" && states.length >= 3) ? { staleClaim: true } : { applied: true };
   },
   getInputs: async () => ({ inputs: [] }),
   postMessages: async () => {},
   reportCrossCheckVerdict: async (_id: string, _generation: number, value: any) => {
    decisions.push(value);
    if (mode === "stale-verdict") throw new RequestError("POST", "/cross-check-verdict", 409,
      '{"reason":"cross_check_refused"}');
    if (mode === "lost-ack") {
     if (value.verdict !== "failed") throw new Error("verdict ACK lost");
     throw new RequestError("POST", "/cross-check-verdict", 409, '{"reason":"cross_check_refused"}');
    }
    if (mode === "delivery" && value.verdict !== "failed") throw new Error("delivery failed");
   },
  } as unknown as WorkerClient;
  const git = {
   ensureClone: async () => "/bare",
   runnerCloneAtCommit: async () => { clones++; return "/checkout"; },
   removeRunnerClone: async () => { removals++; },
  } as unknown as GitCache;
  try {
   await new CrossCheckRunner(client, git, recorded.logger, { homeRoot: root, pollMs: 1,
    modelTimeoutMs: mode === "timeout" ? 5 : 1000,
    model: { run: async (_claim, _checkout, _home, signal) => {
     modelCalls++;
     if (mode === "finding-schema") throw new CrossCheckMalformedError("cross-check invalid finding schema");
     if (mode === "cancel") cancel.abort();
     if (mode === "timeout" || mode === "cancel") {
      await new Promise<void>((resolve) => {
       if (signal.aborted) resolve();
       else signal.addEventListener("abort", () => resolve(), { once: true });
      });
      signal.throwIfAborted();
     }
     return mode === "malformed" ? "prefix " + verdict : verdict;
    } },
   }).execute(c, cancel.signal);
   if (mode === "timeout" || mode === "timeout-probe") {
    assert.ok(!JSON.stringify(recorded.lines).includes("cross-check verdict malformed"));
   }
   if (mode === "stale-running") {
    assert.deepEqual(states, ["running"]);
    assert.equal(modelCalls, 0);
    assert.equal(clones, 0);
    assert.equal(decisions.length, 0);
   } else {
    assert.equal(removals, 1);
    if (mode === "cancel-probe" || mode === "timeout-probe") {
     assert.ok(decisions.every((decision) => decision.verdict !== "approve"), "aborted custody probe cannot approve");
     assert.ok(!states.includes("completed"));
    }
    if (mode === "stale-verdict") {
     assert.deepEqual(states, ["running", "running", "running"]);
     assert.equal(decisions.length, 1);
    } else if (mode === "lost-terminal-ack") {
     assert.equal(states.at(-1), "failed");
     assert.equal(decisions.length, 1, "a delivered verdict is never overwritten");
     assert.equal(decisions[0].verdict, "approve");
    } else {
     assert.equal(states.at(-1), "failed");
     assert.equal(decisions.at(-1).verdict, "failed");
     assert.equal(decisions.at(-1).reason_class, mode === "timeout" || mode === "timeout-probe" ? "model_timeout"
      : mode === "malformed" || mode === "finding-schema" ? "malformed" : "model_error");
    }
   }
  } finally { await fs.rm(root, { recursive: true, force: true }); }
 });
}

for (const stage of ["plan", "code"] as const) {
 for (const mode of ["typed explicit", "typed unannotated", "plain JSON error", "returned invalid JSON"] as const) {
  it("malformed diagnostic runner " + stage + " " + mode, async () => {
   const root = await fs.mkdtemp(path.join(path.resolve(import.meta.dirname, "../../.uzi/scratch"), "malformed-diagnostic-"));
   const recorded = recordingLogger();
   const sentinel = "SENTINEL-malformed-provider-output-3432";
   const states: Record<string, unknown>[] = [];
   const decisions: unknown[] = [];
   const messages: unknown[] = [];
   const c = { ...claim(), claim_generation: 7 };
   if (stage === "code") c.cross_check = {
    ...c.cross_check!, stage: "code", round: 1, head_commit: "b".repeat(40),
    candidate_digest: "c".repeat(64), code_context: { task: "Inspect committed code" }, guidance_snapshot: "",
   };
   let modelCalls = 0;
   let diffReads = 0;
   let removals = 0;
   const client = {
    reportState: async (_id: string, body: Record<string, unknown>) => { states.push(body); return { applied: true }; },
    getInputs: async () => ({ inputs: [] }),
    postMessages: async (_id: string, batch: unknown[]) => { messages.push(...batch); },
    reportCrossCheckVerdict: async (_id: string, _generation: number, body: unknown) => {
     assert.equal(stage, "plan"); decisions.push(body);
    },
    reportCodeCrossCheckVerdict: async (_id: string, _generation: number, body: unknown) => {
     assert.equal(stage, "code"); decisions.push(body);
    },
    getCodeSnapshotCleanup: async () => ({ row_present: false }),
   } as unknown as WorkerClient;
   const git = {
    ensureClone: async () => { assert.equal(stage, "plan"); return "/bare"; },
    existingBarePathFor: async () => "/bare",
    runnerCloneAtCommit: async () => "/checkout",
    readBare: async () => { diffReads++; return { text: "committed patch", truncated: false }; },
    removeRunnerClone: async () => { removals++; },
   } as unknown as GitCache;
   try {
    await new CrossCheckRunner(client, git, recorded.logger, { homeRoot: root, pollMs: 1, modelTimeoutMs: 1000,
     model: { run: async (_claim, _checkout, _home, _signal, _usage, cleanupConfirmed) => {
      modelCalls++;
      if (stage === "code") { assert.equal(typeof cleanupConfirmed, "function"); cleanupConfirmed!(); }
      if (mode === "typed explicit") throw new CrossCheckMalformedError(sentinel, "finding_schema");
      if (mode === "typed unannotated") throw new CrossCheckMalformedError(sentinel);
      if (mode === "plain JSON error") throw new Error("JSON " + sentinel);
      return "{invalid JSON " + sentinel;
     } },
    }).execute(c);
    assert.equal(modelCalls, 1);
    assert.equal(diffReads, stage === "code" ? 2 : 0);
    assert.equal(removals, 1);
    assert.deepEqual(states, [
     { status: "running", claim_generation: 7 },
     { status: "failed", failure_reason: "The cross-check did not complete.", claim_generation: 7 },
    ]);
    assert.deepEqual(decisions, [stage === "plan"
     ? { verdict: "failed", reason_class: "malformed", summary: "The cross-check did not complete.", items: [] }
     : { outcome: "failed", reason_class: "malformed", findings: [] }]);
    assert.equal(JSON.stringify({ logs: recorded.lines, messages, decisions, states }).includes(sentinel), false);
    const warnings = recorded.lines.filter((line) => (line as { level?: string }).level === "warn");
    assert.deepEqual(warnings, [{
     level: "warn", msg: "cross-check verdict malformed", run_id: "check", stage,
     subreason: mode === "typed explicit" ? "finding_schema"
      : mode === "returned invalid JSON" && stage === "code" ? "code_findings" : "unknown",
    }]);
   } finally { await fs.rm(root, { recursive: true, force: true }); }
  });
 }
}

it("exact-commit clone stays on immutable base after default and tracking refs advance", async () => {
 const root = await fs.mkdtemp(path.join(path.resolve(import.meta.dirname, "../../.uzi/scratch"), "exact-check-"));
 const exec = promisify(execFile);
 const origin = path.join(root, "origin");
 const git = async (...args: string[]) => (await exec("git", ["-C", origin, ...args],
  { env: { ...process.env, GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null" } })).stdout.trim();
 let checkout: string | undefined;
 try {
  await fs.mkdir(origin);
  await git("init", "-b", "main");
  await git("config", "user.name", "Fixture");
  await git("config", "user.email", "fixture@example.test");
  await fs.writeFile(path.join(origin, "anchor.ts"), "base");
  await git("add", "anchor.ts");
  await git("commit", "-m", "base");
  const base = await git("rev-parse", "HEAD");
  await fs.writeFile(path.join(origin, "anchor.ts"), "advanced");
  await git("commit", "-am", "advance");
  const cache = new GitCache(path.join(root, "cache"), nullLogger());
  const bare = await cache.ensureClone(origin);
  await exec("git", ["-C", bare, "update-ref", "refs/uzi-runner/agent/issue-1", await git("rev-parse", "HEAD")]);
  checkout = await cache.runnerCloneAtCommit(bare, base, "child-id");
  assert.match(path.basename(checkout), /^cross-check-child-id-/);
  assert.equal((await exec("git", ["-C", checkout, "rev-parse", "HEAD"])).stdout.trim(), base);
  assert.equal(await fs.readFile(path.join(checkout, "anchor.ts"), "utf8"), "base");
  await assert.rejects(fs.stat(path.join(checkout, ".git/objects/info/alternates")), { code: "ENOENT" });
  await assert.rejects(cache.runnerCloneAtCommit(bare, "f".repeat(40), "child-id"));
 } finally { await fs.rm(root, { recursive: true, force: true }); }
});

// ---- Delivery contract (PRD #3166): activity and usage ride one batcher; close-then-fence ----

interface Scenario {
 checker: (ctx: { signal: AbortSignal; usage: (p: Record<string, unknown>) => Promise<void>;
  activity: import("../src/cross-check-activity.js").CheckActivityRecorder | undefined }) => Promise<string>;
 post?: (msgs: any[], index: number, signal?: AbortSignal) => void | Promise<void>;
 modelTimeoutMs?: number;
 preModelFailure?: boolean;
 /** Put the claim's deadline_at this many ms ahead (default: the fixture's far-future deadline). */
 deadlineInMs?: number;
 forgePat?: string;
 /** Override the reportState(running) ack; `n` counts the running calls (0 is the first). */
 running?: (n: number) => Record<string, unknown>;
}

async function scenario(s: Scenario) {
 const root = await fs.mkdtemp(path.join(path.resolve(import.meta.dirname, "../../.uzi/scratch"), "checker-delivery-"));
 const outbox = new Outbox({ root: path.join(root, "outbox"), log: nullLogger(),
  runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86400000 });
 await outbox.init();
 const c = { ...claim(), claim_generation: 7 };
 if (s.forgePat) c.secrets.forge_pat = s.forgePat;
 if (s.deadlineInMs !== undefined) (c.cross_check as any).deadline_at = new Date(Date.now() + s.deadlineInMs).toISOString();
 const calls: string[] = [];
 const landed: any[] = [];
 const verdicts: any[] = [];
 const terminals: { status: string; fence: number | undefined }[] = [];
 let postIndex = 0;
 let runningIndex = 0;
 const client = new WorkerClient("http://127.0.0.1:1", "join-fixture", "test", nullLogger()) as any;
 client.reportState = async (_id: string, body: any) => {
  if (body.status === "completed" || body.status === "failed") {
   calls.push("terminal:" + body.status);
   terminals.push({ status: body.status, fence: (await outbox.readTerminalJournal(c.run_id, 7))?.messagesThroughSeq });
  }
  if (body.status === "running" && s.running) return s.running(runningIndex++);
  return { applied: true };
 };
 client.getInputs = async () => ({ inputs: [] });
 client.postMessages = async (_run: string, msgs: any[], _gen: number, signal?: AbortSignal) => {
  const index = postIndex++;
  signal?.throwIfAborted();
  await s.post?.(msgs, index, signal);
  calls.push("messages");
  landed.push(...msgs);
 };
 client.reportCrossCheckVerdict = async (_id: string, _g: number, value: any) => {
  calls.push("verdict:" + value.verdict + (value.reason_class === "approve" ? "" : ":" + value.reason_class));
  verdicts.push(value);
 };
 const git = { ensureClone: async () => "/bare", runnerCloneAtCommit: async () => "/checkout",
  removeRunnerClone: async () => {} } as unknown as GitCache;
 try {
  await new CrossCheckRunner(client, git, nullLogger(), { homeRoot: root, pollMs: 1, outbox, batchMs: 5,
   modelTimeoutMs: s.modelTimeoutMs ?? 5000,
   model: { run: async (_c, _co, _h, signal, usage, _confirm, activity) => {
    // The fake checker crosses into the model unless a test says it failed first.
    if (s.preModelFailure !== true) activity?.modelStarted();
    return s.checker({ signal, usage, activity });
   } },
  }).execute(c);
 } finally { await fs.rm(root, { recursive: true, force: true }); }
 return { calls, landed, verdicts, terminals, events: landed.map((m) => m.payload.event as string) };
}

const settleRead = (a: any, id: string, p: string) => { a.begin(id, "read", p); a.settle(id, { outcome: "ok", lines: [1, 3] }); };
const initFrame = { event: "init", text: "init" };
const resultFrame = { event: "result", is_error: false };

function assertClosedThenFenced(r: Awaited<ReturnType<typeof scenario>>, status: string) {
 const terminal = r.calls.indexOf("terminal:" + status);
 assert.ok(terminal > 0, "terminal reported");
 assert.ok(r.calls.slice(terminal).every((x) => x !== "messages"), "no row is posted after the terminal report");
 assert.equal(r.terminals.length, 1);
 assert.equal(r.terminals[0]!.fence, r.landed.at(-1)!.seq, "fence is the batcher's closed tail");
 assert.deepEqual(r.landed.map((m) => m.seq), r.landed.map((_m, i) => i + 1), "stream is contiguous");
}

it("checker delivery: marker, activity, usage and summary go through the batcher with redaction before verdict and terminal", async () => {
 const pat = "forge-pat-secret-0123456789";
 const r = await scenario({ forgePat: pat, checker: async ({ usage, activity }) => {
  await usage({ ...initFrame, text: "token " + pat });
  settleRead(activity, "a", "api/x.go");
  settleRead(activity, "b", "web/y.ts");
  await usage(resultFrame);
  return verdict;
 } });
 assert.deepEqual(r.events, ["check_activity_start", "init", "check_activity", "check_activity", "result", "check_activity_summary"]);
 assert.equal(JSON.stringify(r.landed).includes(pat), false);
 assert.ok(JSON.stringify(r.landed).includes("***REDACTED***"), "the secret was replaced, not dropped");
 assert.equal(r.landed.at(-1)!.payload.complete, true);
 assert.equal(r.landed.at(-1)!.payload.generation, 7);
 assert.ok(r.landed.every((m) => m.agent === "cross-checker" && m.kind === "status"));
 assert.deepEqual(r.calls.filter((x) => x !== "messages"), ["verdict:approve", "terminal:completed"]);
 assert.ok(r.calls.lastIndexOf("messages") < r.calls.indexOf("verdict:approve"));
 assertClosedThenFenced(r, "completed");
});

it("checker delivery: a failed check still persists the summary and the result frame", async () => {
 const r = await scenario({ checker: async ({ usage, activity }) => {
  await usage(initFrame);
  settleRead(activity, "a", "api/x.go");
  await usage({ event: "result", is_error: true });
  throw new Error("boom");
 } });
 assert.deepEqual(r.events, ["check_activity_start", "init", "check_activity", "result", "check_activity_summary"]);
 assert.deepEqual(r.calls.filter((x) => x !== "messages"), ["verdict:failed:model_error", "terminal:failed"]);
 assert.ok(r.calls.lastIndexOf("messages") < r.calls.indexOf("verdict:failed:model_error"));
 assertClosedThenFenced(r, "failed");
});

it("checker delivery: a model timeout closes the batcher before its terminal report", async () => {
 const r = await scenario({ modelTimeoutMs: 30, checker: async ({ signal, usage, activity }) => {
  await usage(initFrame);
  activity!.begin("pending", "read", "a.ts");
  await new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
  signal.throwIfAborted();
  return verdict;
 } });
 assert.deepEqual(r.calls.filter((x) => x !== "messages"), ["verdict:failed:model_timeout", "terminal:failed"]);
 assert.equal(r.landed.at(-1)!.payload.complete, false);
 assert.equal(r.landed.at(-1)!.payload.unsettled, 1);
 assertClosedThenFenced(r, "failed");
});

it("checker delivery: a transient post rejection that close later delivers leaves the verdict standing", async () => {
 const r = await scenario({ post: (_m, index) => {
  if (index === 0) throw new RequestError("POST", "/messages", 503, "{}");
 }, checker: async ({ usage }) => { await usage(initFrame); await usage(resultFrame); return verdict; } });
 assert.deepEqual(r.calls.filter((x) => x !== "messages"), ["verdict:approve", "terminal:completed"]);
 assert.deepEqual(r.events, ["check_activity_start", "init", "result", "check_activity_summary"]);
 assertClosedThenFenced(r, "completed");
});

it("checker delivery: persistent rejection leaves usage undelivered, so the check fails model_error but the terminal is delivered", async () => {
 const r = await scenario({ post: () => { throw new RequestError("POST", "/messages", 503, "{}"); },
  checker: async ({ usage }) => { await usage(initFrame); return verdict; } });
 assert.deepEqual(r.calls, ["verdict:failed:model_error", "terminal:failed"]);
 assert.equal(r.terminals[0]!.fence, 3, "marker, init and summary were emitted; the fence is the batcher's tail");
});

it("checker delivery: a tombstoned usage frame fails the check model_error", async () => {
 const r = await scenario({ post: (msgs) => {
  if (msgs.some((m) => m.payload.event === "init")) throw new RequestError("POST", "/messages", 400, "{}");
 }, checker: async ({ usage }) => { await usage(initFrame); await usage(resultFrame); return verdict; } });
 assert.deepEqual(r.calls.filter((x) => x !== "messages"), ["verdict:failed:model_error", "terminal:failed"]);
 assert.ok(r.landed.some((m) => m.payload.event === "message_dropped"));
 assertClosedThenFenced(r, "failed");
});

it("checker delivery: a tombstoned activity row with usage delivered leaves the verdict standing", async () => {
 const r = await scenario({ post: (msgs) => {
  if (msgs.some((m) => m.payload.event === "check_activity")) throw new RequestError("POST", "/messages", 400, "{}");
 }, checker: async ({ usage, activity }) => {
  await usage(initFrame);
  settleRead(activity, "a", "api/x.go");
  await usage(resultFrame);
  return verdict;
 } });
 assert.deepEqual(r.calls.filter((x) => x !== "messages"), ["verdict:approve", "terminal:completed"]);
 assert.ok(r.landed.some((m) => m.payload.event === "message_dropped"));
 assertClosedThenFenced(r, "completed");
});

it("checker delivery: a tripped batcher (fatal 401) turns a would-be success into model_error and still reports terminal", async () => {
 const r = await scenario({ post: () => { throw new RequestError("POST", "/messages", 401, "{}"); },
  checker: async ({ usage }) => {
   await usage(initFrame);
   await new Promise((resolve) => setTimeout(resolve, 80));
   return verdict;
  } });
 assert.deepEqual(r.calls, ["verdict:failed:model_error", "terminal:failed"]);
});

it("checker delivery: 250 tool calls emit 200 activity rows plus the summary, dropped 50", async () => {
 const r = await scenario({ checker: async ({ usage, activity }) => {
  await usage(initFrame);
  for (let i = 0; i < 250; i++) settleRead(activity, "id" + i, "f" + i + ".ts");
  await usage(resultFrame);
  return verdict;
 } });
 assert.equal(r.events.filter((e) => e === "check_activity").length, 200);
 assert.equal(r.landed.at(-1)!.payload.dropped, 50);
 assert.equal(r.landed.at(-1)!.payload.complete, true);
 assert.deepEqual(r.calls.filter((x) => x !== "messages"), ["verdict:approve", "terminal:completed"]);
 assertClosedThenFenced(r, "completed");
});

const summaryPost = (msgs: any[]) => msgs.some((m) => m.payload.event === "check_activity_summary");
const sleepMs = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

it("checker delivery: a 413 on a single usage frame is a tombstone and fails the check model_error", async () => {
 const r = await scenario({ post: (msgs) => {
  if (msgs.some((m) => m.payload.event === "init")) throw new RequestError("POST", "/messages", 413, "{}");
 }, checker: async ({ usage }) => { await usage(initFrame); await usage(resultFrame); return verdict; } });
 assert.deepEqual(r.calls.filter((x) => x !== "messages"), ["verdict:failed:model_error", "terminal:failed"]);
 assert.ok(r.landed.some((m) => m.payload.event === "message_truncated"));
 assertClosedThenFenced(r, "failed");
});

it("checker delivery: a slow summary post cannot turn a finished verdict into a model timeout", async () => {
 const r = await scenario({ modelTimeoutMs: 300,
  post: async (msgs) => { if (summaryPost(msgs)) await sleepMs(400); },
  checker: async ({ usage }) => { await usage(initFrame); await sleepMs(100); await usage(resultFrame); return verdict; } });
 assert.deepEqual(r.calls.filter((x) => x !== "messages"), ["verdict:approve", "terminal:completed"]);
 assertClosedThenFenced(r, "completed");
});

it("checker delivery: an expired deadline stops the runner before the checker runs and reports incomplete capture, not 0 reads complete", async () => {
 let invoked = false;
 const r = await scenario({ deadlineInMs: -1000, checker: async () => { invoked = true; return verdict; } });
 assert.equal(invoked, false);
 assert.deepEqual(r.events, ["check_activity_start", "check_activity_summary"]);
 assert.equal(r.landed.at(-1)!.payload.complete, false);
 assert.deepEqual(r.calls.filter((x) => x !== "messages"), ["verdict:failed:model_timeout", "terminal:failed"]);
});

it("checker delivery: a checker that throws unavailable before the model starts reports incomplete capture", async () => {
 const r = await scenario({ preModelFailure: true, checker: async () => { throw new CrossCheckCheckerUnavailableError(); } });
 assert.equal(r.events.at(-1), "check_activity_summary");
 assert.equal(r.landed.at(-1)!.payload.complete, false);
 assert.deepEqual(r.landed.at(-1)!.payload.reads, { ok: 0, denied: 0, oversize: 0, limit: 0, error: 0 });
});

it("checker delivery: the paused model timer never re-arms past deadline_at", async () => {
 const r = await scenario({ modelTimeoutMs: 5000, deadlineInMs: 300,
  post: async (msgs) => { if (summaryPost(msgs)) await sleepMs(400); },
  checker: async ({ usage }) => { await usage(initFrame); await sleepMs(100); await usage(resultFrame); return verdict; } });
 assert.deepEqual(r.calls.filter((x) => x !== "messages"), ["verdict:failed:model_timeout", "terminal:failed"]);
 assertClosedThenFenced(r, "failed");
});

it("checker delivery: a checker_unavailable failure keeps its reason when the close is slow", async () => {
 const r = await scenario({ modelTimeoutMs: 150,
  post: async (msgs) => { if (summaryPost(msgs)) await sleepMs(300); },
  checker: async ({ usage }) => { await usage(initFrame); await sleepMs(20); throw new CrossCheckCheckerUnavailableError(); } });
 assert.deepEqual(r.calls.filter((x) => x !== "messages"), ["verdict:failed:checker_unavailable", "terminal:failed"]);
});

it("checker delivery: a stale-claim return closes the batcher, posts its rows and sends no terminal state", async () => {
 const r = await scenario({ running: (n) => (n >= 1 ? { applied: false, staleClaim: true } : { applied: true }),
  checker: async ({ usage }) => { await usage(initFrame); await usage(resultFrame); return verdict; } });
 assert.deepEqual(r.events, ["check_activity_start", "init", "result", "check_activity_summary"]);
 assert.deepEqual(r.calls.filter((x) => x !== "messages"), []);
});

it("checker delivery: a stale refusal inside the checker returns from catch, and finally still closes the batcher", async () => {
 const r = await scenario({ checker: async ({ usage, activity }) => {
  await usage(initFrame);
  settleRead(activity, "r1", "a.ts");
  await usage(resultFrame);
  throw new RequestError("POST", "/messages", 409, JSON.stringify({ disposition: "stale_claim" }));
 } });
 assert.deepEqual(r.events, ["check_activity_start", "init", "check_activity", "result", "check_activity_summary"]);
 assert.deepEqual(r.calls.filter((x) => x !== "messages"), []);
});

it("checker delivery: a tripped batcher alone, with every usage frame delivered, fails the check model_error", async () => {
 // Only the summary post is fatal: usage seqs are delivered, so only isTripped() can reject it.
 const r = await scenario({ post: (msgs) => {
  if (summaryPost(msgs)) throw new RequestError("POST", "/messages", 401, "{}");
 }, checker: async ({ usage }) => { await usage(initFrame); await usage(resultFrame); await sleepMs(80); return verdict; } });
 assert.deepEqual(r.landed.map((m) => m.payload.event), ["check_activity_start", "init", "result"]);
 assert.deepEqual(r.calls.filter((x) => x !== "messages"), ["verdict:failed:model_error", "terminal:failed"]);
});

it("checker delivery: a usage frame only journaled to the outbox fails the check model_error", async () => {
 // Seqs: 1 marker, 2 init, 3 result. The init frame's only delivery evidence is the journal.
 const spy = mock.method(MessageBatcher.prototype, "journalOnlySeqs", () => ({ seqs: new Set<number>([2]), overflow: false }));
 try {
  const r = await scenario({ checker: async ({ usage }) => { await usage(initFrame); await usage(resultFrame); return verdict; } });
  assert.deepEqual(r.calls.filter((x) => x !== "messages"), ["verdict:failed:model_error", "terminal:failed"]);
 } finally { spy.mock.restore(); }
});

it("checker delivery: a journal-only activity row with usage delivered leaves the verdict standing", async () => {
 const spy = mock.method(MessageBatcher.prototype, "journalOnlySeqs", () => ({ seqs: new Set<number>([1]), overflow: false }));
 try {
  const r = await scenario({ checker: async ({ usage }) => { await usage(initFrame); await usage(resultFrame); return verdict; } });
  assert.equal(r.calls.filter((x) => x !== "messages")[0]?.startsWith("verdict:failed"), false);
 } finally { spy.mock.restore(); }
});

it("checker delivery: a journal-only overflow alone fails the check model_error", async () => {
 const spy = mock.method(MessageBatcher.prototype, "journalOnlySeqs", () => ({ seqs: new Set<number>(), overflow: true }));
 try {
  const r = await scenario({ checker: async ({ usage }) => { await usage(initFrame); await usage(resultFrame); return verdict; } });
  assert.deepEqual(r.calls.filter((x) => x !== "messages"), ["verdict:failed:model_error", "terminal:failed"]);
 } finally { spy.mock.restore(); }
});

it("checker delivery: a tombstone-set overflow alone fails the check model_error", async () => {
 const spy = mock.method(MessageBatcher.prototype, "tombstonedSeqs", () => ({ seqs: new Set<number>(), overflow: true }));
 try {
  const r = await scenario({ checker: async ({ usage }) => { await usage(initFrame); await usage(resultFrame); return verdict; } });
  assert.deepEqual(r.calls.filter((x) => x !== "messages"), ["verdict:failed:model_error", "terminal:failed"]);
 } finally { spy.mock.restore(); }
});

for (const cancel of [false, true]) {
 it(`M3 actual checker runner ${cancel ? "cancels" : "admits"} a required subscription wait`, async () => {
  const fs = await import("node:fs/promises");
  const { fileURLToPath } = await import("node:url");
  const { CrossCheckRunner } = await import("../src/cross-check-runner.js");
  const { ActiveRunRegistry } = await import("../src/active-run-registry.js");
  const { acquireCodexOperationScope } = await import("../src/codex-operation-scope.js");
  const { AdmissionClock, flushUntil } = await import("./operation-admission-clock.js");
  const root = await fs.mkdtemp(fileURLToPath(new URL("../../.uzi/scratch/m3-checker-", import.meta.url)));
  const clock = new AdmissionClock();
  const registry = new ActiveRunRegistry();
  const c = claim(true); c.claim_generation = 7;
  const owner = new AbortController();
  const r = rig({ subscription: true });
  let reads = 0;
  const client = {
   hasFeature: () => true, codexOperationNegotiation: 1,
   checkCodexOperationAdmission: async (_id: string, request: import("../src/protocol.js").CodexOperationRequest) => {
    reads++;
    return reads === 1 ? { decision: "wait", ...request, class: "overload",
     episode_id: "00000000-0000-4000-8000-000000000091",
     server_now: new Date(0).toISOString(), eligible_at: new Date(1000).toISOString() }
     : { decision: "admit", operation_id: request.operation_id, claim_generation: request.claim_generation };
   },
   releaseCodex: async () => ({ auth_mode: "subscription", access_token: "released-token", generation: 7, chatgpt_account_id: "account", chatgpt_plan_type: null }),
   reportState: async () => ({ applied: true }), getInputs: async () => ({ inputs: [] }),
   postMessages: async () => {}, reportCrossCheckVerdict: async () => {},
  } as unknown as WorkerClient;
  const scope = acquireCodexOperationScope(client, c.run_id, 7, { now: () => clock.now, sleep: clock.sleep });
  const git = { ensureClone: async () => "bare", runnerCloneAtCommit: async () => "checkout", removeRunnerClone: async () => {} } as never;
  const runner = new CrossCheckRunner(client, git, { ...r.log, warn() {}, info() {}, error() {}, debug() {} } as Logger,
   { homeRoot: root, activeRuns: registry, pollMs: 1000, model: new CodexCrossCheck(client, r.log, r.deps) });
  const done = runner.execute(c, owner.signal);
  try {
   await flushUntil(() => clock.sleepers.size === 1);
   assert.equal(registry.build().active[0]?.account_cooldown?.kind, "checker");
   assert.equal(r.requests.some(f => f.method === "turn/start"), false);
   if (cancel) owner.abort(); else clock.advance(1000);
   await done;
   assert.equal(r.requests.filter(f => f.method === "turn/start").length, cancel ? 0 : 1);
   assert.equal(reads, cancel ? 1 : 2);
   assert.equal(registry.size, 0);
   assert.deepEqual(r.disposed.sort(), ["fileop", "provider"]);
  } finally { owner.abort(); await done; scope.close(); await fs.rm(root, { recursive: true, force: true }); }
 });
}
