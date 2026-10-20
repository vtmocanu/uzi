import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { WorkerClient, type ClaimResponse } from "../../src/client.js";
import { prepareCIEvidence, type CIEvidenceDownloader, type PreparedCIEvidence } from "../../src/ci-evidence.js";
import { buildCIEvidenceServer, CI_EVIDENCE_READ, ciEvidencePrompt } from "../../src/ci-evidence-tools.js";
import { SdkExecutor, type SdkQueryFn } from "../../src/sdk-executor.js";
import type { RunContext } from "../../src/executor.js";
import type { HarnessAgent, RunTurnRequest } from "../../src/harness.js";
import { CodexHarness } from "../../src/codex/codex-harness.js";
import { CodexCallbackBroker } from "../../src/codex/broker.js";
import { CodexDelegationRunner } from "../../src/codex/delegation.js";
import { buildCodexRunPlan } from "../../src/codex/run-builder.js";
import { buildCodexDynamicTools } from "../../src/codex/dynamic-tools.js";
import { buildCodexToolHandlers } from "../../src/codex/codex-executor.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../../src/codex/registry.js";
import type { CodexNotification, CodexTransport } from "../../src/codex/transport.js";
import { nullLogger } from "../helpers.js";

type Message = Record<string, unknown>;
type ToolResult = { isError?: boolean; content: { text: string }[] };
export interface CIEvidenceHarnessOptions {
  harness: "claude" | "codex";
  claim: ClaimResponse;
  client: CIEvidenceDownloader;
  home: string;
  clone: string;
  expected: { manifest_id: string; text: string; sha256?: string; size?: number; truncated?: boolean };
  mode: "fresh" | "resume";
  previousGeneration?: number;
  signal?: AbortSignal;
  /** Hooks run with the real materialized reader alive / revoked, for the UID suite. */
  beforeModel?: (context: { prepared: PreparedCIEvidence; file: string }) => Promise<void>;
  afterDispose?: (context: { file: string }) => Promise<void>;
}
export interface CIEvidenceHarnessReport {
  harness: "claude" | "codex";
  mode: "fresh" | "resume";
  generation: number;
  run_id: string;
  manifest_id: string;
  sha256: string;
  size: number;
  home: string;
  clone: string;
  filemode: number;
  expectedreadercontent: string;
  downloads: number;
  metadataFence: true;
  outsideClone: true;
  noInputCloneWrites: true;
  modelTurns: number;
}

// All streams have one consumer, a fixed script and a caller/deadline abort.
// A failed assertion aborts the invocation; sibling calls are never retried.
class Notes {
  private queue: CodexNotification[] = [];
  private waiting?: (value: IteratorResult<CodexNotification>) => void;
  private ended = false;
  push(note: CodexNotification) {
    if (this.waiting) { const resolve = this.waiting; this.waiting = undefined; resolve({ value: note, done: false }); }
    else this.queue.push(note);
  }
  end() { this.ended = true; this.waiting?.({ value: undefined, done: true }); this.waiting = undefined; }
  iterator(): AsyncIterableIterator<CodexNotification> {
    return {
      next: () => {
        const value = this.queue.shift();
        if (value) return Promise.resolve({ value, done: false });
        if (this.ended) return Promise.resolve({ value: undefined, done: true });
        assert.equal(this.waiting, undefined);
        return new Promise(resolve => { this.waiting = resolve; });
      },
      [Symbol.asyncIterator]() { return this; },
    };
  }
}

async function rpc(server: ReturnType<typeof buildCIEvidenceServer>, signal: AbortSignal) {
  const pending = new Map<number, { resolve: (m: Message) => void; reject: (e: unknown) => void }>();
  const abort = () => { for (const p of pending.values()) p.reject(new Error("fixture RPC aborted")); pending.clear(); };
  signal.addEventListener("abort", abort, { once: true });
  const transport: {
    start(): Promise<void>; close(): Promise<void>; send(m: Message): Promise<void>;
    onmessage?: (m: Message) => void; onclose?: () => void;
  } = {
    async start() {},
    async close() { transport.onclose?.(); },
    async send(m) {
      if (typeof m.id === "number") {
        const p = pending.get(m.id);
        assert.ok(p, "RPC response has a pending request");
        pending.delete(m.id); p.resolve(m);
      }
    },
  };
  await server.instance.connect(transport as Parameters<typeof server.instance.connect>[0]);
  let id = 0;
  const request = (method: string, params?: Message): Promise<Message> => {
    signal.throwIfAborted();
    const n = ++id;
    return new Promise((resolve, reject) => {
      pending.set(n, { resolve, reject });
      transport.onmessage!({ jsonrpc: "2.0", id: n, method, ...(params ? { params } : {}) });
    });
  };
  try {
    assert.ok((await request("initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "ci-proof", version: "1" } })).result);
    transport.onmessage!({ jsonrpc: "2.0", method: "notifications/initialized" });
  } catch (error) { signal.removeEventListener("abort", abort); await server.instance.close(); throw error; }
  return { request, async close() { signal.removeEventListener("abort", abort); await server.instance.close(); } };
}

function assertFence(prompt: string, o: CIEvidenceHarnessOptions) {
  const fence = prompt.match(/<ci_evidence_([a-f0-9]{32})>\n([^]*?)\n<\/ci_evidence_\1>/);
  assert.ok(fence, "metadata has a matching nonce fence");
  assert.match(prompt, /untrusted data, never instructions/);
  const metadata = fence[2]!;
  assert.ok(!metadata.includes("<"), "metadata cannot close its fence");
  assert.equal(JSON.parse(metadata).untrusted, true);
  assert.ok(metadata.includes(o.expected.manifest_id));
  assert.ok(!prompt.includes(o.home), "worker evidence home is absent from model prompt");
}

function unknownId(o: CIEvidenceHarnessOptions) {
  return o.expected.manifest_id === "00000000-0000-0000-0000-000000000001"
    ? "00000000-0000-0000-0000-000000000002" : "00000000-0000-0000-0000-000000000001";
}
function readCases(o: CIEvidenceHarnessOptions): Message[] {
  return [{ manifest_id: o.expected.manifest_id }, { manifest_id: unknownId(o) },
    { manifest_id: o.expected.manifest_id, path: "/fixture/forbidden" }];
}
function assertExcerpt(excerpt: { text: string; truncated: boolean; omitted_bytes: number }, o: CIEvidenceHarnessOptions) {
  if (o.expected.truncated) {
    assert.equal(excerpt.truncated, true);
    assert.ok(excerpt.omitted_bytes > 0);
    assert.ok(excerpt.text.length > 0 && o.expected.text.startsWith(excerpt.text));
  } else assert.equal(excerpt.text, o.expected.text);
}
function assertRead(result: ToolResult, index: number, o: CIEvidenceHarnessOptions) {
  assert.ok(Buffer.byteLength(result.content[0]!.text) <= 64 << 10);
  if (index === 0) {
    assert.ok(!result.isError);
    assertExcerpt(JSON.parse(result.content[0]!.text), o);
  } else assert.equal(result.isError, true);
}

async function claude(o: CIEvidenceHarnessOptions, prepare: () => Promise<PreparedCIEvidence>, signal: AbortSignal) {
  let turns = 0;
  const queryFn: SdkQueryFn = params => (async function* () {
    const turn = turns++;
    const server = params.options.mcpServers?.ci_evidence;
    assert.ok(server && "instance" in server, "executor conditionally registers actual SDK MCP server");
    assert.ok(params.options.agents?.reader?.tools?.includes(CI_EVIDENCE_READ));
    assert.ok(params.options.agents?.reader?.mcpServers?.includes("ci_evidence"));
    assert.ok(!params.options.agents?.noRead?.tools?.includes(CI_EVIDENCE_READ));
    assert.ok(!params.options.agents?.noRead?.mcpServers?.includes("ci_evidence"));
    assert.ok(params.options.agents?.noRead?.disallowedTools?.includes(CI_EVIDENCE_READ));
    if (turn === 0) assert.equal(params.options.resume, o.claim.session_id ?? undefined);
    for await (const prompt of params.prompt) assertFence(String((prompt as { message: { content: unknown } }).message.content), o);
    const wire = await rpc(server as ReturnType<typeof buildCIEvidenceServer>, signal);
    try {
      const listed = await wire.request("tools/list");
      const tools = (listed.result as { tools: { name: string; inputSchema: { additionalProperties: boolean } }[] }).tools;
      assert.deepEqual(tools.map(t => t.name), ["read"]);
      assert.equal(tools[0]!.inputSchema.additionalProperties, false);
      for (const [index, args] of readCases(o).entries()) {
        const reply = await wire.request("tools/call", { name: "read", arguments: args });
        assertRead(reply.result as ToolResult, index, o);
      }
    } finally { await wire.close(); }
    const planning = !o.claim.plan_approved && turn === 0;
    yield { type: "assistant", session_id: "fixture-session", message: { content: [{
      type: "tool_use", id: "fixture-signal", name: "mcp__uzi__" + (planning ? "submit_plan" : "signal_done"),
      input: planning ? { plan_md: "fixture approved plan" } : {},
    }] } } as unknown as SDKMessage;
    yield { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: "fixture-session" } as unknown as SDKMessage;
  })();
  const ctx: RunContext = {
    runId: o.claim.run_id, claimGeneration: o.claim.claim_generation, kind: o.claim.kind,
    issueIid: null, issueTitle: "CI evidence proof", issueDescription: "Read the supplied CI evidence.",
    worktreePath: o.clone, branch: "agent/ci-evidence-proof", emit() {}, oauthToken: "fixture-" + "oauth",
    config: o.claim.config ?? undefined, signal,
    agents: [
      { name: "reader", description: "Read evidence", prompt_body: "Read evidence", tools: ["Read"] },
      { name: "noRead", description: "No reading", prompt_body: "No reading", tools: [] },
    ],
    planApproved: o.claim.plan_approved, approvedPlan: o.claim.plan_md ?? undefined,
    sessionId: o.claim.session_id ?? undefined, resumePhase: o.claim.resume_phase,
    gatePlan: async () => ({ kind: "approve", selection: { status: "absent" } }),
    prepareCIEvidence: prepare,
  };
  await new SdkExecutor(nullLogger(), o.home, { queryFn, installDeps: async () => ({ results: [], truncated: false }) }).run(ctx);
  assert.ok(turns > 0 && turns <= 2);
  return turns;
}

async function codex(o: CIEvidenceHarnessOptions, prepared: PreparedCIEvidence, signal: AbortSignal) {
  const role = (names: string[]): HarnessAgent => ({
    description: "fixture role", prompt: "Read only if permitted", tools: { kind: "allow", names },
    deniedTools: [], toolServers: [], skills: [],
  });
  const req: RunTurnRequest = {
    prompt: "Read CI evidence." + ciEvidencePrompt(prepared), systemPrompt: "Fixture", signal,
    phase: o.claim.plan_approved ? "implement" : "plan", leadSkills: [], ciEvidencePrepared: true,
    agents: { reader: role(["Read"]), noRead: role([]) },
    ...(o.claim.session_id ? { resumeSessionId: o.claim.session_id } : {}),
  };
  const plan = buildCodexRunPlan(req);
  assert.equal(plan.roles.get("reader")!.grants.allowedTools.has(CI_EVIDENCE_READ), true);
  assert.equal(plan.roles.get("noRead")!.grants.allowedTools.has(CI_EVIDENCE_READ), false);
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(o.claim.claim_generation!));
  const handlers = buildCodexToolHandlers({ client: o.client as WorkerClient, runId: o.claim.run_id,
    log: nullLogger(), emit() {}, skills: [], ciEvidence: prepared });
  const notes = new Notes();
  const scripts = new Map<string, { role?: string; calls: Message[]; index: number }>();
  let sequence = 0;
  let childSequence = 0;
  let callbacks = 0;
  let replyFailure: unknown;
  const pendingCalls = new Map<number | string, string>();
  let rootStarted = false;
  let rootResumed = false;
  const pushCall = (thread: string) => {
    const script = scripts.get(thread)!;
    const args = script.calls[script.index];
    if (!args) {
      notes.push({ kind: "turn_completed", method: "turn/completed", threadId: thread, turnId: "turn",
        status: "completed", params: { threadId: thread, turn: { id: "turn", status: "completed" } } });
      return;
    }
    const delegation = "subagent_type" in args;
    const requestId = ++sequence;
    pendingCalls.set(requestId, thread);
    notes.push({ kind: "activity", method: "item/tool/call", requestId,
      params: { threadId: thread, turnId: "turn", callId: String(sequence),
        tool: delegation ? "spawn_agent" : "uzi_ci_evidence_read", arguments: args } });
  };
  // The injected provider speaks wire notifications only. Production harness, builder,
  // delegation runner and brokers own registration, role admission and callback dispatch.
  const transport: CodexTransport = {
    async request<T = unknown>(method: string, params?: unknown): Promise<T> {
      const p = params as Message;
      if (method === "thread/start" || method === "thread/resume") {
        const child = p.ephemeral === true;
        const thread = child ? "child-" + (++childSequence) : "root";
        const roleName = child ? (childSequence === 1 ? "reader" : "noRead") : undefined;
        const tools = p.dynamicTools as { name: string; inputSchema: { additionalProperties: boolean } }[];
        const spec = tools?.find(t => t.name === "uzi_ci_evidence_read");
        if (method === "thread/start") assert.equal(!!spec, roleName !== "noRead");
        else assert.equal(p.threadId, o.claim.session_id, "resume uses actual claimed session");
        if (spec) assert.equal(spec.inputSchema.additionalProperties, false);
        if (!child) { rootStarted = method === "thread/start"; rootResumed = method === "thread/resume"; }
        scripts.set(thread, { role: roleName, index: 0, calls: roleName
          ? (roleName === "reader" ? readCases(o) : [readCases(o)[0]!])
          : [readCases(o)[0]!, { subagent_type: "reader", prompt: "Read evidence" }, { subagent_type: "noRead", prompt: "Attempt evidence read" }] });
        return { thread: { id: thread } } as T;
      }
      if (method === "turn/start") {
        const thread = p.threadId as string;
        if (thread === "root") assertFence((p.input as { text: string }[]).map(item => item.text).join("\n"), o);
        pushCall(thread);
        return { turn: { id: "turn" } } as T;
      }
      return {} as T;
    },
    notify() {},
    respond(id, response) {
      const reply = response as { result?: { success: boolean; contentItems?: { text?: string }[] }; error?: unknown };
      const thread = pendingCalls.get(id);
      assert.ok(thread, "reply matches the exact outstanding wire request");
      pendingCalls.delete(id);
      const script = scripts.get(thread)!;
      // Finish the fixed traffic script even after an oracle fails; report the
      // original assertion after dispatch settles, never via a real-time wait.
      try {
        assert.ok(!reply.error);
        const result = reply.result!;
        assert.ok(result);
        const evidenceRead = script.role === "reader" || (!script.role && "manifest_id" in script.calls[script.index]!);
        if (evidenceRead) {
          const index = script.role ? script.index : 0;
          assert.equal(result.success, index === 0);
          const text = result.contentItems?.map(c => c.text ?? "").join("") ?? "";
          assert.ok(Buffer.byteLength(text) <= 64 << 10, `Codex model-visible response has ${Buffer.byteLength(text)} bytes`);
          if (index === 0) assertExcerpt(JSON.parse(text), o);
          callbacks++;
        } else if (script.role === "noRead") { assert.equal(result.success, false); callbacks++; }
        else assert.equal(result.success, true);
      } catch (error) { replyFailure ??= error; }
      script.index++;
      pushCall(thread);
    },
    notifications: () => notes.iterator(),
    async close() { notes.end(); },
  };
  const noEffect = async (): Promise<never> => { throw new Error("unexpected fixture effect"); };
  const effects = { registry, worktreePath: o.clone, spawnCommand: noEffect, fileop: { op: noEffect }, toolHandlers: handlers };
  let harness: CodexHarness;
  const delegation = new CodexDelegationRunner({
    ...effects, roles: plan.roles, signal, childTurnDeadlineMs: 10_000,
    startChildTurn: async spec => {
      const started = await harness.requestOnTransport<{ thread: { id: string } }>("thread/start", {
        ephemeral: true, dynamicTools: buildCodexDynamicTools(spec.grants), developerInstructions: spec.systemPrompt,
      }, { signal: spec.signal });
      const threadId = started.thread.id;
      const childNotes = new Notes();
      harness.registerChildSink(threadId, childNotes);
      try {
        const turn = await harness.requestOnTransport<{ turn: { id: string } }>("turn/start", {
          threadId, input: [{ type: "text", text: spec.taskInput }],
        }, { signal: spec.signal });
        harness.bindChildDispatch(threadId, spec.parent, spec.role);
        return { threadId, turnId: turn.turn.id, notifications: () => childNotes.iterator(),
          respond: (id, reply) => harness.respondOnTransport(id, reply),
          interrupt: async () => {}, close: async () => { harness.unregisterChildSink(threadId); childNotes.end(); } };
      } catch (error) { harness.unregisterChildSink(threadId); childNotes.end(); throw error; }
    },
  });
  const broker = new CodexCallbackBroker({ ...effects, grants: plan.lead.grants,
    allowedRoles: plan.allowedRoles, delegate: request => delegation.run(request) });
  harness = new CodexHarness({ registry, broker, workspace: o.clone, homeDir: o.home, log: nullLogger(),
    provider: { name: "openai", baseUrl: "http://127.0.0.1:9/v1", envKey: "OPENAI_API_KEY", model: "gpt-6-astra" },
    sessionInspect: async () => "present",
    launchRoot: async () => ({ root: { kind: "provider", reap: async () => ({ ok: true }), dispose: async () => {} },
      transport, supervisorPid: 1234 }),
  });
  const turn = harness.startTurn(req);
  let finished = false;
  try { for await (const event of turn.events) if (event.kind === "turn_finished") finished = true; }
  catch (error) { throw replyFailure ?? error; }
  finally { await turn.close(); }
  if (replyFailure !== undefined) throw replyFailure;
  assert.equal(finished, true);
  assert.equal(callbacks, 5, "root read, three reader-child calls and one denied child call");
  assert.equal(rootResumed, !!o.claim.session_id);
  assert.equal(rootStarted, !o.claim.session_id);
  return 1;
}

/** Adapter proof only: Go owns sealed storage and the UID suite owns the OS boundary. */
export async function exerciseCIEvidenceHarness(o: CIEvidenceHarnessOptions): Promise<CIEvidenceHarnessReport> {
  assert.ok(path.isAbsolute(o.home) && path.isAbsolute(o.clone));
  assert.ok(Number.isSafeInteger(o.claim.claim_generation) && o.claim.claim_generation! > 0, "actual claim generation required");
  assert.ok(o.claim.ci_repair, "claim must carry CI repair evidence");
  if (o.mode === "resume") {
    assert.ok(Number.isSafeInteger(o.previousGeneration), "resume requires previous generation");
    assert.ok(o.claim.claim_generation! > o.previousGeneration!, "reclaim must advance generation");
    assert.ok(o.claim.plan_approved && o.claim.plan_md && o.claim.session_id && o.claim.resume_phase, "resume requires actual approved session/phase");
  } else assert.equal(o.previousGeneration, undefined);
  const signal = AbortSignal.any([AbortSignal.timeout(30_000), ...(o.signal ? [o.signal] : [])]);
  await fs.mkdir(o.home, { recursive: true });
  assert.ok((await fs.stat(o.clone)).isDirectory());
  const snapshot = async () => {
    const entries = await fs.readdir(o.clone, { recursive: true, withFileTypes: true });
    assert.ok(entries.length <= 10_000, "fixture clone snapshot budget");
    const rows = [];
    for (const e of entries) {
      const filename = path.join(e.parentPath, e.name);
      const st = await fs.lstat(filename);
      rows.push([path.relative(o.clone, filename), st.mode, st.size, st.isFile()
        ? createHash("sha256").update(await fs.readFile(filename)).digest("hex") : st.isSymbolicLink() ? await fs.readlink(filename) : ""]);
    }
    return JSON.stringify(rows.sort((a, b) => String(a[0]).localeCompare(String(b[0]))));
  };
  const before = await snapshot();
  let current = true;
  let prepared: PreparedCIEvidence | undefined;
  let downloads = 0;
  const downloader: CIEvidenceDownloader = {
    downloadJobFile: async (...args) => { downloads++; await o.client.downloadJobFile(...args); },
  };
  const fileSpec = o.claim.ci_repair.input_files.find(f => f.manifest_id === o.expected.manifest_id);
  assert.ok(fileSpec);
  const file = path.join(o.home, "ci-evidence", "inputs", fileSpec.name);
  const prepare = async () => {
    await prepared?.dispose();
    prepared = await prepareCIEvidence({ claim: { ...o.claim, config: o.claim.config ?? undefined }, claimGeneration: o.claim.claim_generation!,
      runHome: o.home, cloneRoot: o.clone, downloader, signal, isCurrent: () => current });
    assertFence("fixture" + ciEvidencePrompt(prepared), o);
    await o.beforeModel?.({ prepared, file });
    return prepared;
  };
  let modelTurns: number;
  try {
    modelTurns = o.harness === "claude" ? await claude(o, prepare, signal) : await codex(o, await prepare(), signal);
  } finally {
    current = false;
    await prepared?.dispose();
    if (prepared) await assert.rejects(prepared.read({ manifest_id: o.expected.manifest_id }));
    await o.afterDispose?.({ file });
  }
  const st = await fs.lstat(file);
  assert.equal(st.mode & 0o7777, 0o400);
  const sha256 = createHash("sha256").update(await fs.readFile(file)).digest("hex");
  assert.equal(st.size, fileSpec.size);
  assert.equal(sha256, fileSpec.sha256);
  if (o.expected.sha256 !== undefined) assert.equal(sha256, o.expected.sha256);
  if (o.expected.size !== undefined) assert.equal(st.size, o.expected.size);
  const real = await fs.realpath(file);
  const clone = await fs.realpath(o.clone);
  assert.ok(real !== clone && !real.startsWith(clone + path.sep));
  assert.equal(await snapshot(), before, "adapter must not write CI inputs into clone");
  return { harness: o.harness, mode: o.mode, generation: o.claim.claim_generation!, run_id: o.claim.run_id,
    manifest_id: fileSpec.manifest_id, sha256, size: st.size, home: o.home, clone: o.clone,
    filemode: st.mode & 0o7777, expectedreadercontent: o.expected.text, downloads,
    metadataFence: true, outsideClone: true, noInputCloneWrites: true, modelTurns };
}

let cliStage: "configuration" | "claim" | "adapter" | "state" = "configuration";

async function cli() {
  const configPath = process.argv[2] ?? process.env.UZI_CI_ARTIFACTS_CONFIG;
  const config = configPath ? JSON.parse(await fs.readFile(configPath, "utf8")) : {};
  const get = (key: string) => config[key] ?? process.env["UZI_CI_ARTIFACTS_" + key.toUpperCase()];
  const url = get("url"), token = get("token"), harness = get("harness"), mode = get("mode");
  const home = get("home"), clone = get("clone"), statePath = get("state");
  const expected = config.expected ?? (get("expected") ? JSON.parse(get("expected")) : undefined);
  assert.ok(url && token && home && clone && expected?.manifest_id && typeof expected.text === "string", "URL/token/home/clone/expected config required");
  assert.ok(harness === "claude" || harness === "codex", "harness required");
  assert.ok(mode === "fresh" || mode === "resume", "mode required");
  const previous: CIEvidenceHarnessReport | undefined = mode === "resume" && statePath
    ? JSON.parse(await fs.readFile(statePath, "utf8")) : undefined;
  if (previous) {
    assert.equal(previous.home, home); assert.equal(previous.clone, clone); assert.equal(previous.harness, harness);
    assert.equal(previous.manifest_id, expected.manifest_id);
    assert.equal(previous.expectedreadercontent, expected.text);
  }
  const client = new WorkerClient(url, token, "ci-artifacts-proof", nullLogger());
  cliStage = "claim";
  const claim = await client.claimRun();
  assert.ok(claim, "real API returned no claim");
  if (previous) assert.equal(claim.run_id, previous.run_id);
  cliStage = "adapter";
  const report = await exerciseCIEvidenceHarness({ harness, mode, home, clone, expected, claim, client,
    previousGeneration: previous?.generation ?? config.previousGeneration });
  if (previous) { assert.equal(report.sha256, previous.sha256); assert.equal(report.size, previous.size); }
  cliStage = "state";
  if (statePath) await fs.writeFile(statePath, JSON.stringify(report) + "\n", { mode: 0o600 });
  process.stdout.write(JSON.stringify(report) + "\n");
}

// Importing the fixture is inert. Explicit CLI invocations fail closed on missing config.
if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  cli().catch((error: unknown) => {
    // Report only fixed stages and source coordinates: assertion values, HTTP
    // bodies and config may contain private fixture credentials or evidence.
    const location = error instanceof Error
      ? /ci-artifacts-repair-e2e\.ts:(\d+):(\d+)/.exec(error.stack ?? "")
      : null;
    const source = location ? ` fixture:${location[1]}:${location[2]}` : "";
    process.stderr.write(`CI artifacts fixture failed stage=${cliStage}${source}\n`);
    process.exitCode = 1;
  });
}
