import os from "node:os";
import { it } from "node:test";
import assert from "node:assert/strict";
import { PassThrough, Writable } from "node:stream";
import { CodexCrossCheck, CrossCheckMalformedError, type CrossCheckModelDeps } from "../src/codex/cross-check.js";
import type { CodexRootHandle, CodexLaunchSpec, CodexEffectLaunchSpec } from "../src/codex/launcher.js";
import type { ClaimResponse } from "../src/protocol.js";
import { WorkerClient, RequestError } from "../src/client.js";
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
import { nullLogger } from "./helpers.js";

for (const mode of ["approve", "invalidfinding"] as const) {
it("outer checker uses real Read broker, actual HTTP verdict delivery and journaled child completion: " + mode, async () => {
 const expectedVerdict = mode === "approve" ? "approve" : "failed";
 const expectedReason = mode === "approve" ? "approve" : "malformed";
 const expectedStatus = mode === "approve" ? "completed" : "failed";
 const assistantText = mode === "approve" ? verdict : JSON.stringify({
  verdict: "approve", summary: "Anchors checked",
  items: [{ file: "anchor.ts", severity: "invalid", summary: "Finding", rationale: "Read anchor" }],
 });
 const root = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "cross-check-test-"));
 const outbox = new Outbox({ root: path.join(root, "outbox"), log: nullLogger(),
  runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86400000 });
 await outbox.init();
 const registry = new ActiveRunRegistry();
 const posts: { url: string; body: any }[] = [];
 const r = rig({
  notes: (emit) => emit({ id: 90, method: "item/tool/call", params: {
   threadId: "thread", turnId: "turn", callId: "read", tool: "Read", arguments: { path: "anchor.ts" } } }),
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
 const server = http.createServer(async (req, res) => {
  let raw = "";
  for await (const chunk of req) raw += chunk;
  const body = raw ? JSON.parse(raw) : {};
  if (req.method === "POST") posts.push({ url: req.url!, body });
  res.setHeader("Content-Type", "application/json");
  if (req.url?.endsWith("/codex/release")) res.end(JSON.stringify({ auth_mode: "api_key", access_token: "released-token" }));
  else if (req.url?.endsWith("/inputs")) res.end(JSON.stringify({ inputs: [] }));
  else if (req.url?.endsWith("/cross-check-verdict")) {
   assert.equal(body.claim_generation, 7);
   assert.equal(body.verdict, expectedVerdict);
   assert.equal(body.reason_class, expectedReason);
   res.end("{}");
  } else if (req.url?.endsWith("/state")) {
   if (body.status === "completed" || body.status === "failed") {
    assert.equal(body.status, expectedStatus);
    assert.equal(outbox.hasPendingTerminal(c.run_id, 7), true);
    const journal = await outbox.readTerminalJournal(c.run_id, 7);
    assert.equal(journal?.body.status, expectedStatus);
    assert.equal(journal?.body.claim_generation, 7);
    assert.equal(journal?.messagesThroughSeq, 2);
   }
   res.end(JSON.stringify({ applied: true, status: body.status }));
  } else res.end("{}");
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
 try {
  await new CrossCheckRunner(client, git, nullLogger(), { homeRoot: root, pollMs: 1,
   activeRuns: registry, outbox, model: new CodexCrossCheck(client, r.log, r.deps) }).execute(c);
  assert.deepEqual(clones, [["/bare", c.cross_check!.base_commit, c.run_id]]);
  assert.deepEqual(r.ops, ["stat", "read"]);
  assert.deepEqual(r.disposed, ["provider", "fileop"]);
  const delivered = posts.filter((p) => p.url.endsWith("/messages"));
  assert.deepEqual(delivered.map((p) => p.body.messages[0].seq), [1, 2]);
  assert.deepEqual(delivered.map((p) => p.body.messages[0].payload.event), ["init", "result"]);
  assert.equal(delivered[1]!.body.messages[0].payload.is_error, mode !== "approve");
  assert.equal(r.secrets.size, 0);
  assert.ok(posts.every((p) => p.url.includes("/runs/check/")));
  const verdictIndex = posts.findIndex((p) => p.url.endsWith("/cross-check-verdict"));
  assert.ok(verdictIndex > 0);
  assert.equal(posts[verdictIndex]!.body.verdict, expectedVerdict);
  assert.equal(posts[verdictIndex]!.body.reason_class, expectedReason);
  assert.equal(posts[verdictIndex + 1]!.body.status, expectedStatus);
  assert.equal(outbox.hasPendingTerminal(c.run_id, 7), false);
  assert.equal(registry.size, 0);
  assert.deepEqual(removed, ["/checkout"]);
 } finally {
  server.closeAllConnections();
  await new Promise<void>((resolve, reject) => server.close((err) => err ? reject(err) : resolve()));
  await fs.rm(root, { recursive: true, force: true });
 }
});
}

for (const mode of ["stale-running", "stale-verdict", "malformed", "delivery", "timeout", "cancel", "lost-ack", "lost-terminal-ack", "cancel-probe", "timeout-probe", "finding-schema"] as const) {
 it("outer checker fails closed or abandons: " + mode, async () => {
  const root = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "checker-failure-"));
  const states: string[] = [];
  const decisions: any[] = [];
  let modelCalls = 0;
  let clones = 0;
  let removals = 0;
  const cancel = new AbortController();
  const c = { ...claim(), claim_generation: 7 };
  const client = {
   reportState: async (_id: string, body: any) => {
    states.push(body.status);
    if (states.length === 2 && mode === "cancel-probe") cancel.abort();
    if (states.length === 2 && mode === "timeout-probe") await new Promise((resolve) => setTimeout(resolve, 2000));
    if (mode === "lost-terminal-ack" && body.status === "completed") throw new Error("terminal ACK lost");
    return mode === "stale-running" || (mode === "stale-verdict" && states.length >= 3) ? { staleClaim: true } : { applied: true };
   },
   getInputs: async () => ({ inputs: [] }),
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
   await new CrossCheckRunner(client, git, nullLogger(), { homeRoot: root, pollMs: 1,
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

it("exact-commit clone stays on immutable base after default and tracking refs advance", async () => {
 const root = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "exact-check-"));
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
