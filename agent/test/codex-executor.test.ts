import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  CodexExecutor,
  FailClosedExecutor,
  CodexAdviceCredentialBridge,
  makeCodexAdviceHarness,
  CODEX_PRODUCTION_PROVIDER,
  makeDefaultSpawnCommand,
  MAX_COMMAND_CAPTURE_BYTES,
  COMMAND_CAPTURE_KILLED_CODE,
  type CodexExecutorDeps,
} from "../src/codex/codex-executor.js";
import { selectCodexBinding, CodexSelectionError, type CodexBinding } from "../src/codex/select.js";
import type { CodexLaunchRootResult, CodexProviderConfig } from "../src/codex/codex-harness.js";
import type { RegisteredRoot } from "../src/codex/registry.js";
import type { FileopHelperHandle } from "../src/codex/fileop-client.js";
import type {
  CodexAdviceLaunchResult,
  LaunchAdviceRootSeam,
} from "../src/codex/codex-advice-harness.js";
import { CodexAdviceHarness } from "../src/codex/codex-advice-harness.js";
import { makeRedactor, makeTextRedactor } from "../src/redact.js";
import type { CodexNotification, CodexTransport } from "../src/codex/transport.js";
import type { RunContext, EmittedMessage, Executor } from "../src/executor.js";
import type { Logger } from "../src/log.js";
import type { AgentTemplate } from "../src/protocol.js";

// PRD #1171 (M3, milestone 3, Phase 2A) — the production CodexExecutor + the claim-aware
// DARK selection seam, driven with an in-memory transport and scripted app-server frames
// (NO real Codex process, NO real launcher). Every external effect is an INJECTED SEAM.

const WORKSPACE = "/work/repo";
const FRESH_TOKEN = "fresh-codex-access-token-XXXXXXXX";

const provider: CodexProviderConfig = {
  name: "openai",
  baseUrl: "http://127.0.0.1:9/v1",
  envKey: "OPENAI_API_KEY",
  model: "gpt-6-astra",
};

const noopLog: Logger = {
  debug() {},
  info() {},
  warn() {},
  error() {},
  addSecret() {},
  removeSecret() {},
  child() {
    return noopLog;
  },
};

// A RECORDING logger that faithfully mirrors src/log.ts's reference-counted secret
// registry (>= 8-char floor, split/join scrub) and records every added/removed secret and
// every (already-scrubbed) emitted line — so a test can assert a secret was registered,
// evicted, and never rode a log line verbatim.
interface RecordingLog {
  log: Logger;
  added: string[];
  removed: string[];
  lines: string[];
}
function recordingLog(): RecordingLog {
  const counts = new Map<string, number>();
  const added: string[] = [];
  const removed: string[] = [];
  const lines: string[] = [];
  const scrub = (s: string): string => {
    let out = s;
    for (const sec of counts.keys()) out = out.split(sec).join("***REDACTED***");
    return out;
  };
  const emit = (level: string, msg: string, fields?: Record<string, unknown>): void => {
    lines.push(scrub(JSON.stringify({ level, msg, ...fields })));
  };
  const log: Logger = {
    debug: (m, f) => emit("debug", m, f),
    info: (m, f) => emit("info", m, f),
    warn: (m, f) => emit("warn", m, f),
    error: (m, f) => emit("error", m, f),
    addSecret: (s) => {
      added.push(s);
      if (s && s.length >= 8) counts.set(s, (counts.get(s) ?? 0) + 1);
    },
    removeSecret: (s) => {
      removed.push(s);
      const n = counts.get(s);
      if (n === undefined) return;
      if (n <= 1) counts.delete(s);
      else counts.set(s, n - 1);
    },
    child() {
      return log;
    },
  };
  return { log, added, removed, lines };
}

function rec(v: unknown): Record<string, unknown> {
  return (v ?? {}) as Record<string, unknown>;
}

// --- a scriptable in-memory transport with root/child id sequencing ------------
interface ResponderCtx {
  transport: FakeTransport;
  method: string;
  params: unknown;
  threadStartCount: number;
  turnStartCount: number;
}
type Responder = (c: ResponderCtx) => unknown;

class FakeTransport implements CodexTransport {
  requests: { method: string; params: unknown; opts?: { signal?: AbortSignal } }[] = [];
  responses: { requestId: number | string; response: unknown }[] = [];
  notifies: { method: string; params: unknown }[] = [];
  closes = 0;
  threadStartCount = 0;
  turnStartCount = 0;
  /** When set for a method, request() returns THIS (rejects/pends) instead of the responder. */
  requestOverride?: (c: ResponderCtx, opts?: { signal?: AbortSignal }) => Promise<unknown> | undefined;

  private readonly queue: CodexNotification[] = [];
  private ended = false;
  private waiter: ((r: IteratorResult<CodexNotification>) => void) | undefined;
  private consumed = false;
  private closedFlag = false;

  constructor(private readonly responder: Responder) {}

  push(note: CodexNotification): this {
    if (this.waiter) {
      const w = this.waiter;
      this.waiter = undefined;
      w({ value: note, done: false });
    } else {
      this.queue.push(note);
    }
    return this;
  }

  end(): this {
    this.ended = true;
    if (this.waiter) {
      const w = this.waiter;
      this.waiter = undefined;
      w({ value: undefined, done: true });
    }
    return this;
  }

  request<T = unknown>(method: string, params?: unknown, opts?: { signal?: AbortSignal }): Promise<T> {
    this.requests.push({ method, params, opts });
    if (method === "thread/start") this.threadStartCount += 1;
    if (method === "turn/start") this.turnStartCount += 1;
    const c: ResponderCtx = {
      transport: this,
      method,
      params,
      threadStartCount: this.threadStartCount,
      turnStartCount: this.turnStartCount,
    };
    if (this.requestOverride) {
      const overridden = this.requestOverride(c, opts);
      if (overridden !== undefined) return overridden as Promise<T>;
    }
    try {
      return Promise.resolve(this.responder(c) as T);
    } catch (err) {
      return Promise.reject(err instanceof Error ? err : new Error(String(err)));
    }
  }

  notify(method: string, params?: unknown): void {
    this.notifies.push({ method, params });
  }

  respond(
    requestId: number | string,
    response: { readonly result: unknown } | { readonly error: { readonly code: number; readonly message: string } },
  ): void {
    if (this.closedFlag) throw new Error("codex transport is closed");
    this.responses.push({ requestId, response });
  }

  notifications(): AsyncIterableIterator<CodexNotification> {
    if (this.consumed) throw new Error("notifications() is single-consumer");
    this.consumed = true;
    const next = (): Promise<IteratorResult<CodexNotification>> =>
      new Promise((resolve) => {
        const q = this.queue.shift();
        if (q !== undefined) {
          resolve({ value: q, done: false });
          return;
        }
        if (this.ended) {
          resolve({ value: undefined, done: true });
          return;
        }
        this.waiter = resolve;
      });
    return {
      next,
      return: () => Promise.resolve({ value: undefined, done: true }),
      [Symbol.asyncIterator]() {
        return this;
      },
    };
  }

  close(): Promise<void> {
    this.closes += 1;
    this.closedFlag = true;
    return Promise.resolve();
  }
}

// Default responder: root ids th-1 / tn-1, first child th-child / tn-child.
function defaultResponder(c: ResponderCtx): unknown {
  if (c.method === "thread/start") return { thread: { id: c.threadStartCount === 1 ? "th-1" : "th-child" } };
  if (c.method === "thread/resume") return { thread: { id: "resumed-1" } };
  if (c.method === "turn/start") return { turn: { id: c.turnStartCount === 1 ? "tn-1" : "tn-child" } };
  return {};
}

// --- notification builders (the M0 app-server wire vocabulary) -----------------
function threadStarted(threadId = "th-1"): CodexNotification {
  return { kind: "thread_started", method: "thread/started", threadId, params: { thread: { id: threadId } } };
}
function turnCompleted(status = "completed", threadId = "th-1", turnId = "tn-1"): CodexNotification {
  return { kind: "turn_completed", method: "turn/completed", threadId, turnId, status, params: { threadId, turn: { id: turnId, status } } };
}
function agentMessage(text: string, threadId = "th-1"): CodexNotification {
  return { kind: "activity", method: "item/completed", params: { threadId, item: { type: "agentMessage", text } } };
}
function toolCall(
  requestId: number,
  tool: string,
  args: unknown,
  threadId: string,
  turnId: string,
  callId: string,
): CodexNotification {
  return { kind: "activity", method: "item/tool/call", requestId, params: { threadId, turnId, callId, tool, arguments: args } };
}

// --- binding + client + context builders ---------------------------------------
function bindingOf(codex: Record<string, unknown>): CodexBinding {
  const selection = selectCodexBinding({ codex });
  if (selection.kind !== "codex") throw new Error("expected a codex selection");
  return selection.binding;
}

const SUBSCRIPTION = { auth_mode: "subscription", access_token: "claim-tok", capability: "run-cap", generation: 3 };
const API_KEY = { auth_mode: "api_key", access_token: "claim-tok", capability: "run-cap" };

interface FakeClient {
  releaseCalls: { runId: string; capability: string }[];
  refreshCalls: { runId: string; operation_id: string; observed_generation: number }[];
  releaseCodex(runId: string, req: { capability: string }): Promise<{ access_token: string }>;
  refreshCodex(runId: string, req: { capability: string; operation_id: string; observed_generation: number }): Promise<{ access_token: string; generation: number; outcome: string }>;
}
function fakeClient(token = FRESH_TOKEN): FakeClient {
  const releaseCalls: FakeClient["releaseCalls"] = [];
  const refreshCalls: FakeClient["refreshCalls"] = [];
  let gen = 4;
  return {
    releaseCalls,
    refreshCalls,
    async releaseCodex(runId, req) {
      releaseCalls.push({ runId, capability: req.capability });
      return { access_token: token };
    },
    async refreshCodex(runId, req) {
      refreshCalls.push({ runId, operation_id: req.operation_id, observed_generation: req.observed_generation });
      return { access_token: `${token}-refreshed`, generation: gen++, outcome: "advanced" };
    },
  };
}

function makeCtx(overrides: Partial<RunContext> = {}): { ctx: RunContext; emitted: EmittedMessage[] } {
  const emitted: EmittedMessage[] = [];
  const ctx: RunContext = {
    runId: "run-1",
    issueIid: 42,
    issueTitle: "do a thing",
    issueDescription: "the description",
    worktreePath: WORKSPACE,
    branch: "agent/issue-42",
    emit: (m) => emitted.push(m),
    planApproved: true,
    approvedPlan: "the approved plan",
    agents: [],
    ...overrides,
  };
  return { ctx, emitted };
}

// A registry-ownable root whose reap/dispose are tracked.
function trackedRoot(): { root: RegisteredRoot; reaped: () => number; disposed: () => number } {
  let reaps = 0;
  let disposes = 0;
  const root: RegisteredRoot = {
    kind: "provider",
    reap: async () => {
      reaps += 1;
      return { ok: true };
    },
    dispose: async () => {
      disposes += 1;
    },
  };
  return { root, reaped: () => reaps, disposed: () => disposes };
}

function fakeFileopHandle(): { handle: FileopHelperHandle; disposed: () => number } {
  let disposes = 0;
  return {
    handle: {
      client: { op: async () => ({ ok: true }) },
      dispose: async () => {
        disposes += 1;
      },
    },
    disposed: () => disposes,
  };
}

interface Rig {
  transport: FakeTransport;
  client: FakeClient;
  root: RegisteredRoot;
  reaped: () => number;
  disposed: () => number;
  fileopDisposed: () => number;
  spawnCommandCalls: { argv: readonly string[]; opts: { cwd?: string } }[];
  fileopSpawns: { worktreePath: string; env: NodeJS.ProcessEnv }[];
  sessionOps: { adopt: number; removeCalls: number; inspect: number };
  deps: CodexExecutorDeps;
}

function makeRig(opts: { responder?: Responder; token?: string } = {}): Rig {
  const transport = new FakeTransport(opts.responder ?? defaultResponder);
  const client = fakeClient(opts.token);
  const { root, reaped, disposed } = trackedRoot();
  const fh = fakeFileopHandle();
  const spawnCommandCalls: Rig["spawnCommandCalls"] = [];
  const fileopSpawns: Rig["fileopSpawns"] = [];
  const sessionOps = { adopt: 0, removeCalls: 0, inspect: 0 };
  const deps: CodexExecutorDeps = {
    launchProviderRoot: async (_spec, credential): Promise<CodexLaunchRootResult> => {
      // Assert the FRESH credential reaches the launcher (its env). Stash for the test.
      (transport as unknown as { credential?: string }).credential = credential;
      return { root, transport, supervisorPid: 1234 };
    },
    spawnCommand: async (argv, cmdOpts) => {
      spawnCommandCalls.push({ argv, opts: cmdOpts });
      return { code: 0, stdout: "ok", stderr: "" };
    },
    spawnFileop: (worktreePath, env) => {
      fileopSpawns.push({ worktreePath, env });
      return fh.handle;
    },
    sessionStore: {
      adopt: async () => {
        sessionOps.adopt += 1;
        return { files: 0 };
      },
      inspect: async () => {
        sessionOps.inspect += 1;
        return "absent";
      },
      remove: async () => {
        sessionOps.removeCalls += 1;
      },
    },
    idleMs: 5000,
    wallMs: 5000,
    boundaryDeadlineMs: 200,
    childTurnDeadlineMs: 5000,
    commandTmpdir: "/run/runner-tmp",
  };
  return {
    transport,
    client,
    root,
    reaped,
    disposed,
    fileopDisposed: fh.disposed,
    spawnCommandCalls,
    fileopSpawns,
    sessionOps,
    deps,
  };
}

function makeExecutor(rig: Rig, binding: CodexBinding, log: Logger = noopLog): CodexExecutor {
  return new CodexExecutor(
    log,
    "/data/agent-home/run-1",
    { binding, client: rig.client as never, provider },
    rig.deps,
  );
}

async function withTimeout<T>(p: Promise<T>, ms: number, label: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      p,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error(`timed out waiting for ${label}`)), ms);
      }),
    ]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}
function tick(): Promise<void> {
  return new Promise((resolve) => setImmediate(resolve));
}
async function waitFor(cond: () => boolean, label: string, ms = 3000): Promise<void> {
  const start = Date.now();
  while (!cond()) {
    if (Date.now() - start > ms) throw new Error(`timed out waiting for ${label}`);
    await tick();
  }
}

// ================================================================================
describe("CodexExecutor: dark selection seam (the makeExecutor decision)", () => {
  it("(1)/(17) absence of secrets.codex is the claude path — the ordinary claim is byte-identical", () => {
    assert.deepEqual(selectCodexBinding({ codex: undefined }), { kind: "claude" });
    assert.deepEqual(selectCodexBinding({}), { kind: "claude" });
  });

  it("(2) a subscription block selects codex with a validated binding", () => {
    const sel = selectCodexBinding({ codex: SUBSCRIPTION });
    assert.equal(sel.kind, "codex");
    if (sel.kind === "codex") assert.equal(sel.binding.authMode, "subscription");
  });

  it("(3) an api_key block selects codex; the binding carries no generation", () => {
    const sel = selectCodexBinding({ codex: API_KEY });
    assert.equal(sel.kind, "codex");
    if (sel.kind === "codex") {
      assert.equal(sel.binding.authMode, "api_key");
      assert.equal(sel.binding.generation, undefined);
    }
  });

  it("(4) each CodexSelectionErrorReason → a FailClosedExecutor whose run() throws a secret-free message, no fallback", async () => {
    const broken: Record<string, unknown>[] = [
      { auth_mode: "nope", access_token: "SECRET-TOKEN", capability: "SECRET-CAP" }, // invalid_auth_mode
      { auth_mode: "subscription", access_token: "", capability: "SECRET-CAP", generation: 1 }, // invalid_access_token
      { auth_mode: "subscription", access_token: "SECRET-TOKEN", capability: "", generation: 1 }, // invalid_capability
      { auth_mode: "subscription", access_token: "SECRET-TOKEN", capability: "SECRET-CAP" }, // subscription_missing_generation
      { auth_mode: "api_key", access_token: "SECRET-TOKEN", capability: "SECRET-CAP", generation: 1 }, // api_key_unexpected_generation
    ];
    for (const codex of broken) {
      let err: unknown;
      try {
        selectCodexBinding({ codex });
      } catch (e) {
        err = e;
      }
      assert.ok(err instanceof CodexSelectionError, "a broken block throws CodexSelectionError");
      const message = (err as CodexSelectionError).message;
      assert.doesNotMatch(message, /SECRET-TOKEN|SECRET-CAP/, "the message never echoes the token/capability");
      const failClosed = new FailClosedExecutor(message);
      await assert.rejects(failClosed.run(makeCtx().ctx), (e: Error) => e.message === message);
      // No model work: FailClosedExecutor never touches a harness/registry — it throws only.
    }
  });
});

// ================================================================================
describe("CodexExecutor: run() control flow (run-lane precedence)", () => {
  it("(9) a clean-EOF terminal returns and emits the accumulated result", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted()).push(agentMessage("working on it")).push(turnCompleted("completed")).end();
    const { ctx, emitted } = makeCtx();
    const result = await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx), 3000, "clean run");
    assert.equal(result.branch, "agent/issue-42");
    const texts = emitted.flatMap((m) => (typeof m.payload.text === "string" ? [m.payload.text] : []));
    assert.ok(texts.some((t) => t.includes("working on it")), "the accumulated agent text was emitted");
  });

  it("(8) a failed turn_finished is represented as DATA once and materializes+throws ONCE (never double-thrown)", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted()).push(turnCompleted("failed")).end();
    const { ctx, emitted } = makeCtx();
    await assert.rejects(
      withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx), 3000, "failed run"),
      /codex turn failed/,
    );
    // "ONCE" is gated observably, not just by the rejection message: the provider terminal
    // failure is surfaced as neutral DATA (a single error result message from the reducer's
    // terminal projection) AND thrown exactly once (the rejection above). If the harness
    // ALSO threw a separate terminal, or the terminal were double-decoded, there would be
    // zero or two error results here — not exactly one.
    const errorResults = emitted.filter((m) => m.kind === "error" && rec(m.payload).event === "result");
    assert.equal(errorResults.length, 1, "the failed terminal is materialized as data exactly once");
  });

  it("(10) an unexpected EOF (no terminal) throws a protocol error, never a fabricated success", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted()).push(agentMessage("half a turn")).end();
    await assert.rejects(
      withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(makeCtx().ctx), 3000, "eof run"),
      /ended before turn completion/,
    );
  });

  it("(5) a watchdog (idle) trip beats a later terminal", async () => {
    const rig = makeRig();
    rig.deps = { ...rig.deps, idleMs: 25 };
    // Go quiet after the init frame; the idle timer trips before any terminal.
    rig.transport.push(threadStarted());
    // A terminal pushed AFTER the trip must NOT turn the run into a success.
    setTimeout(() => rig.transport.push(turnCompleted("completed")).end(), 80).unref?.();
    await assert.rejects(
      withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(makeCtx().ctx), 3000, "idle run"),
      /idle timeout/,
    );
  });

  it("(6) a cancel beats the raw aborted error the transport throws mid-setup", async () => {
    const controller = new AbortController();
    const rig = makeRig();
    // turn/start pends until its signal aborts, then rejects with a RAW aborted error.
    rig.transport.requestOverride = (c, opts) => {
      if (c.method !== "turn/start") return undefined;
      return new Promise((_, reject) => {
        const sig = opts?.signal;
        const fail = (): void => reject(new Error("AbortError: the transport request was aborted"));
        if (sig?.aborted) fail();
        else sig?.addEventListener("abort", fail, { once: true });
      });
    };
    rig.transport.push(threadStarted());
    const { ctx } = makeCtx({ signal: controller.signal });
    const p = makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx);
    await tick();
    controller.abort();
    await assert.rejects(withTimeout(p, 3000, "cancel run"), (e: Error) => {
      assert.equal(e.message, "run cancelled", "the trip wins over the raw AbortError");
      assert.doesNotMatch(e.message, /AbortError/);
      return true;
    });
  });

  it("(7) a transport/setup throw propagates (no trip, not a success)", async () => {
    const rig = makeRig({
      responder: (c) => {
        if (c.method === "turn/start") throw new Error("boom: transport exploded");
        return defaultResponder(c);
      },
    });
    rig.transport.push(threadStarted());
    await assert.rejects(
      withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(makeCtx().ctx), 3000, "throw run"),
      /boom: transport exploded/,
    );
  });
});

// ================================================================================
describe("CodexExecutor: credential bridge + isolation", () => {
  it("(11) a provider-root start releases a FRESH token with the binding capability; the token rides into the launcher env, is registered as a logger secret then evicted at terminal cleanup, and never appears in an emitted payload (redaction itself is covered by (15))", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted()).push(turnCompleted("completed")).end();
    const { ctx, emitted } = makeCtx();
    const rlog = recordingLog();
    await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION), rlog.log).run(ctx), 3000, "release run");
    assert.equal(rig.client.releaseCalls.length, 1, "releaseCodex called once per provider root");
    const rel = rig.client.releaseCalls[0];
    assert.ok(rel);
    assert.equal(rel.capability, "run-cap", "with the binding capability");
    assert.equal(rel.runId, "run-1");
    assert.equal((rig.transport as unknown as { credential?: string }).credential, FRESH_TOKEN, "the fresh token rode into the launcher");
    // The fresh token NEVER appears in an emitted message payload.
    assert.doesNotMatch(JSON.stringify(emitted), new RegExp(FRESH_TOKEN));
    // The token is registered with the logger (so any accidental log embedding would be
    // scrubbed) AND evicted at terminal cleanup — the addSecret is balanced by a
    // removeSecret, so a long-lived worker's secret set does not grow per provider root
    // (part C). No emitted log line carries the token verbatim.
    assert.deepEqual(rlog.added, [FRESH_TOKEN], "the fresh token was registered as a logger secret exactly once");
    assert.deepEqual(rlog.removed, [FRESH_TOKEN], "the fresh token was evicted at terminal cleanup (add balanced by remove)");
    assert.doesNotMatch(rlog.lines.join("\n"), new RegExp(FRESH_TOKEN));
  });

  it("(3-run) an api_key run NEVER calls refresh", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted()).push(turnCompleted("completed")).end();
    await withTimeout(makeExecutor(rig, bindingOf(API_KEY)).run(makeCtx().ctx), 3000, "api_key run");
    assert.equal(rig.client.refreshCalls.length, 0, "refreshCodex is never called on the run path");
  });

  it("(12) a resume seeds adopt (credential-free) AND still releases a fresh token", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted("resumed-1")).push(turnCompleted("completed", "resumed-1")).end();
    const { ctx } = makeCtx({ sessionId: "prior-session" });
    await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx), 3000, "resume run");
    assert.ok(rig.sessionOps.adopt >= 1, "adopt seeded the credential-free session subset");
    assert.equal(rig.client.releaseCalls.length, 1, "a resumed root still releases a fresh token");
    const resume = rig.transport.requests.find((r) => r.method === "thread/resume");
    assert.ok(resume, "the harness resumed the prior session");
  });

  it("(13) the fileop helper is spawned with a SCRUBBED, replaced env (no inherited secrets)", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted()).push(turnCompleted("completed")).end();
    await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(makeCtx().ctx), 3000, "fileop run");
    assert.equal(rig.fileopSpawns.length, 1);
    const spawn0 = rig.fileopSpawns[0];
    assert.ok(spawn0);
    const env = spawn0.env;
    assert.deepEqual(Object.keys(env).sort(), ["LANG", "PATH", "TMPDIR"], "exactly the scrubbed keys");
    assert.equal(env.PATH, "/usr/bin:/bin");
    assert.equal(env.LANG, "C");
    assert.equal(env.TMPDIR, "/run/runner-tmp");
    // Nothing inherited from the worker: no PAT/token/provider credential shape.
    assert.doesNotMatch(JSON.stringify(env), new RegExp(FRESH_TOKEN + "|run-cap|claim-tok"));
    assert.equal(spawn0.worktreePath, WORKSPACE);
  });

  it("(14) terminal cleanup reaps the registry roots and removes the session store", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted()).push(turnCompleted("completed")).end();
    await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(makeCtx().ctx), 3000, "cleanup run");
    assert.ok(rig.reaped() >= 1, "the provider root was reaped");
    assert.ok(rig.disposed() >= 1, "the provider root was disposed");
    assert.ok(rig.fileopDisposed() >= 1, "the fileop handle was disposed");
    assert.equal(rig.sessionOps.removeCalls, 1, "the credential-free store was removed");
  });

  it("(16) safety is populated and killAgentTree is undefined", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted()).push(turnCompleted("completed")).end();
    const exec = makeExecutor(rig, bindingOf(SUBSCRIPTION));
    await withTimeout(exec.run(makeCtx().ctx), 3000, "safety run");
    assert.ok(exec.safety, "safety populated");
    assert.equal(exec.safety?.kind, "codex");
    assert.equal((exec as Executor).killAgentTree, undefined, "no killAgentTree (async evidence-based reap)");
  });
});

// ================================================================================
describe("CodexExecutor: redactor canaries (buildFlight ordering)", () => {
  it("(15) a payload carrying the codex token/capability AND a thrown failure_reason are scrubbed by both redactors", () => {
    // Mirror runner.buildFlight's APPEND: the codex canaries are appended after the
    // forge/anthropic/join/gitBasic set, then both redactors are built over the union.
    // Server-generated codex secrets are well above the redactor's 8-char floor.
    const accessToken = "codex-access-token-abcdef123456";
    const capability = "codex-run-capability-abcdef123456";
    const secrets = ["forge-pat-value", "anthropic-oauth-value", "join-token-value", "git-basic-value", accessToken, capability];
    const redact = makeRedactor(secrets);
    const redactText = makeTextRedactor(secrets);
    const scrubbedPayload = redact({ tool_result: `used ${accessToken} with cap ${capability}` });
    assert.doesNotMatch(JSON.stringify(scrubbedPayload), /codex-access-token-abcdef|codex-run-capability-abcdef/);
    const scrubbedReason = redactText(`run failed: token ${accessToken} / capability ${capability}`);
    assert.doesNotMatch(scrubbedReason, /codex-access-token-abcdef|codex-run-capability-abcdef/);
    assert.match(scrubbedReason, /REDACTED/);
  });
});

// ================================================================================
describe("CodexExecutor: child-thread delegation demux (part C)", () => {
  const agents: AgentTemplate[] = [
    { name: "lead", description: "the lead", prompt_body: "lead body", tools: null, skills: [] },
    { name: "coder", description: "a coder", prompt_body: "coder body", tools: null, skills: [] },
  ];

  it("(18) a root spawn_agent runs a CHILD turn demuxed onto a child thread; child effects run, child signals + nested delegation are denied, and the child settles before the parent callback resolves", async () => {
    // The child's frames are pushed WHEN its turn/start is issued (i.e. AFTER the child
    // sink is registered), modelling the app-server sending them after turn/start.
    const responder: Responder = (c) => {
      if (c.method === "thread/start") return { thread: { id: c.threadStartCount === 1 ? "th-1" : "th-child" } };
      if (c.method === "turn/start") {
        if (c.turnStartCount === 1) return { turn: { id: "tn-1" } };
        // The CHILD turn: emit its callbacks + terminal now that the sink is registered.
        c.transport
          .push(toolCall(11, "Bash", { command: "echo hi" }, "th-child", "tn-child", "cc-bash"))
          .push(toolCall(12, "submit_plan", { plan: "child cannot plan" }, "th-child", "tn-child", "cc-sig"))
          .push(toolCall(13, "spawn_agent", { role: "coder" }, "th-child", "tn-child", "cc-nest"))
          .push(turnCompleted("completed", "th-child", "tn-child"));
        return { turn: { id: "tn-child" } };
      }
      if (c.method === "turn/interrupt") return {};
      return {};
    };
    const rig = makeRig({ responder });
    // The ROOT turn: a single spawn_agent callback, then (pushed once the parent replies)
    // the root terminal.
    rig.transport.push(threadStarted()).push(toolCall(1, "spawn_agent", { role: "coder", prompt: "help" }, "th-1", "tn-1", "c-root"));

    const { ctx } = makeCtx({ agents });
    const runP = makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx);

    // The parent spawn_agent callback resolves ONLY after the child settles; once the
    // parent has replied, close the root turn.
    await waitFor(() => rig.transport.responses.some((r) => r.requestId === 1), "parent spawn_agent reply");
    rig.transport.push(turnCompleted("completed", "th-1", "tn-1")).end();
    await withTimeout(runP, 3000, "delegation run");

    // A child thread + turn were started on the SAME transport (demuxed).
    assert.equal(rig.transport.threadStartCount, 2, "a child thread/start was issued");
    assert.equal(rig.transport.turnStartCount, 2, "a child turn/start was issued");

    // The child's Bash effect ran as the command identity (through the demux + child broker).
    const bash = rig.spawnCommandCalls.find((s) => JSON.stringify(s.argv).includes("echo hi"));
    assert.ok(bash, "the child Bash effect reached the command-identity spawn seam");

    const replyOf = (id: number): { success?: boolean } => {
      const entry = rig.transport.responses.find((r) => r.requestId === id);
      const response = rec(entry?.response);
      return rec(response.result) as { success?: boolean };
    };
    assert.equal(replyOf(11).success, true, "the child Bash callback succeeded");
    assert.equal(replyOf(12).success, false, "a child submit_plan (non-root signal) is denied");
    assert.equal(replyOf(13).success, false, "a child nested spawn_agent is denied");
    assert.equal(replyOf(1).success, true, "the parent spawn_agent callback succeeded after the child settled");
  });

  it("(19) the non-delegation root path is byte-identical after the demux edit (no child thread/start)", async () => {
    const rig = makeRig();
    rig.transport
      .push(threadStarted())
      .push(agentMessage("a plain root turn"))
      .push(toolCall(1, "Bash", { command: "echo root" }, "th-1", "tn-1", "c1"))
      .push(turnCompleted("completed"))
      .end();
    const { ctx, emitted } = makeCtx({ agents });
    const result = await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx), 3000, "non-delegation run");
    assert.equal(result.branch, "agent/issue-42");
    assert.equal(rig.transport.threadStartCount, 1, "no child thread was started");
    assert.equal(rig.transport.turnStartCount, 1, "no child turn was started");
    // The root Bash ran inline (command identity), the root text was emitted.
    assert.ok(rig.spawnCommandCalls.some((s) => JSON.stringify(s.argv).includes("echo root")));
    assert.ok(emitted.flatMap((m) => (typeof m.payload.text === "string" ? [m.payload.text] : [])).some((t) => t.includes("a plain root turn")));
  });

  it("(20) a delegation whose child streams frames spanning longer than idleMs does NOT falsely trip REASON_IDLE — each demuxed child frame re-arms the root idle watchdog (fail-old/pass-fixed for B)", async () => {
    const IDLE_MS = 120;
    const GAP_MS = 30; // each child frame arrives well within IDLE_MS of the previous one
    const STEPS = 8; // the child turn spans ~STEPS*GAP_MS ≈ 240ms, TWICE the idle window
    const responder: Responder = (c) => {
      if (c.method === "thread/start") return { thread: { id: c.threadStartCount === 1 ? "th-1" : "th-child" } };
      if (c.method === "turn/start") {
        if (c.turnStartCount === 1) return { turn: { id: "tn-1" } };
        // The CHILD turn: stream liveness frames GAP_MS apart, total span > IDLE_MS. With
        // the fix each demuxed child frame yields a CONTENT-FREE root `activity` that re-arms
        // idle; with the OLD bare `continue` the root idle timer never re-armed during the
        // delegation and REASON_IDLE tripped mid-child.
        const t = c.transport;
        let n = 0;
        const pump = (): void => {
          n += 1;
          if (n <= STEPS) {
            t.push(agentMessage(`child liveness ${n}`, "th-child"));
            setTimeout(pump, GAP_MS).unref?.();
          } else {
            t.push(turnCompleted("completed", "th-child", "tn-child"));
          }
        };
        setTimeout(pump, GAP_MS).unref?.();
        return { turn: { id: "tn-child" } };
      }
      if (c.method === "turn/interrupt") return {};
      return {};
    };
    const rig = makeRig({ responder });
    rig.deps = { ...rig.deps, idleMs: IDLE_MS, wallMs: 5000 };
    rig.transport.push(threadStarted()).push(toolCall(1, "spawn_agent", { role: "coder", prompt: "help" }, "th-1", "tn-1", "c-root"));

    const { ctx } = makeCtx({ agents });
    const runP = makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx);

    // The idle watchdog is live throughout the child stream — a false trip would reject runP
    // (with /idle timeout/) before the parent ever replies. The parent spawn_agent callback
    // resolves only after the child fully settles; once it has, close the root turn.
    await waitFor(() => rig.transport.responses.some((r) => r.requestId === 1), "parent spawn_agent reply", 5000);
    rig.transport.push(turnCompleted("completed", "th-1", "tn-1")).end();
    const result = await withTimeout(runP, 5000, "delegation-liveness run");
    assert.equal(result.branch, "agent/issue-42", "the root turn completed instead of tripping REASON_IDLE");
    assert.equal(rig.transport.turnStartCount, 2, "the child turn ran on the same transport");
    assert.equal(replyOf1(rig).success, true, "the parent spawn_agent callback succeeded after the child settled");
  });
});

/** The success flag of the reply the transport was told to send for requestId 1. */
function replyOf1(rig: Rig): { success?: boolean } {
  const entry = rig.transport.responses.find((r) => r.requestId === 1);
  const response = rec(entry?.response);
  return rec(response.result) as { success?: boolean };
}

// ================================================================================
describe("CodexExecutor: advice auth bridge + factory (part F)", () => {
  const fakeAdviceLaunch: LaunchAdviceRootSeam = async (): Promise<CodexAdviceLaunchResult> => {
    throw new Error("advice launch is not driven in these construction tests");
  };

  it("a subscription bridge releases a fresh token and refreshes with a RETAINED operation id", async () => {
    const client = fakeClient();
    const bridge = new CodexAdviceCredentialBridge("run-1", client as never, bindingOf(SUBSCRIPTION));
    assert.equal(await bridge.release(), FRESH_TOKEN);
    assert.equal(client.releaseCalls.length, 1);
    // Two refresh() calls with NO new-operation boundary reuse the SAME operation id
    // (retained across retries), and the observed generation advances.
    await bridge.refresh();
    await bridge.refresh();
    assert.equal(client.refreshCalls.length, 2);
    const [r0, r1] = client.refreshCalls;
    assert.ok(r0 && r1);
    assert.equal(r0.operation_id, r1.operation_id, "operation id retained across retries");
    assert.equal(r0.observed_generation, 3, "first observed generation is the claim's");
    assert.equal(r1.observed_generation, 4, "the observed generation advanced after the first refresh");
    // A NEW logical refresh mints a fresh id.
    bridge.beginRefreshOperation();
    await bridge.refresh();
    const r2 = client.refreshCalls[2];
    assert.ok(r2);
    assert.notEqual(r2.operation_id, r0.operation_id, "a new logical refresh gets a new id");
  });

  it("an api_key bridge releases but FAILS CLOSED on refresh (no fallback)", async () => {
    const client = fakeClient();
    const bridge = new CodexAdviceCredentialBridge("run-1", client as never, bindingOf(API_KEY));
    assert.equal(await bridge.release(), FRESH_TOKEN);
    await assert.rejects(bridge.refresh(), /not permitted for an api_key/);
    assert.equal(client.refreshCalls.length, 0, "api_key never invokes refresh");
  });

  it("makeCodexAdviceHarness builds an isolated harness from a fresh release; a release failure fails closed (no root built)", async () => {
    const client = fakeClient();
    const bridge = new CodexAdviceCredentialBridge("run-1", client as never, bindingOf(SUBSCRIPTION));
    const harness = await makeCodexAdviceHarness(bridge, CODEX_PRODUCTION_PROVIDER, fakeAdviceLaunch, noopLog);
    assert.ok(harness instanceof CodexAdviceHarness);
    assert.equal(harness.kind, "codex");
    assert.equal(client.releaseCalls.length, 1, "the credential was released once");

    // Authority unavailable → the bridge's release throws → NO advice harness is built.
    const failing = {
      releaseCodex: async () => {
        throw new Error("capability revoked");
      },
      refreshCodex: async () => {
        throw new Error("unused");
      },
    };
    const deadBridge = new CodexAdviceCredentialBridge("run-1", failing as never, bindingOf(SUBSCRIPTION));
    await assert.rejects(makeCodexAdviceHarness(deadBridge, CODEX_PRODUCTION_PROVIDER, fakeAdviceLaunch, noopLog), /capability revoked/);
  });
});

// ================================================================================
describe("CodexExecutor: default command capture is byte-capped (A — untrusted-input OOM)", () => {
  // A REAL spawn: without UZI_UID_SPLIT, commandRootCommand is a pass-through, so the
  // default seam spawns the bare command. A custom (non-scrubbed) env is used only so the
  // shell resolves the coreutils it needs — env scrubbing is (13)'s concern, not this test's.
  const runEnv: NodeJS.ProcessEnv = { PATH: process.env.PATH ?? "/usr/bin:/bin", LANG: "C", TMPDIR: "/tmp" };

  it("(A) caps combined stdout+stderr at MAX_COMMAND_CAPTURE_BYTES, SIGKILLs the child, and RESOLVES (never rejects) with the truncated result", async () => {
    const spawnCommand = makeDefaultSpawnCommand(runEnv);
    // The child would emit 8 MiB (>> the 1 MiB cap). WITHOUT the cap the seam would
    // accumulate the whole 8 MiB (and an UNBOUNDED producer like `yes` would grow the JS
    // string until the worker OOMs). WITH the cap it stops at 1 MiB and kills the child.
    const bytesToEmit = 8 * 1024 * 1024;
    const res = await withTimeout(
      spawnCommand(["/bin/sh", "-c", `head -c ${bytesToEmit} /dev/zero`], { cwd: "/tmp" }),
      30000,
      "capped command",
    );
    // Killed at the cap → the SIGKILL sentinel code (not the child's own clean 0).
    assert.equal(res.code, COMMAND_CAPTURE_KILLED_CODE, "a cap-kill reports the SIGKILL sentinel exit code");
    const captured = Buffer.byteLength(res.stdout, "utf8") + Buffer.byteLength(res.stderr, "utf8");
    assert.ok(captured <= MAX_COMMAND_CAPTURE_BYTES, `captured ${captured} is bounded at the cap ${MAX_COMMAND_CAPTURE_BYTES}`);
    assert.ok(captured >= MAX_COMMAND_CAPTURE_BYTES - 64 * 1024, "captured up to the cap (truncated, not empty)");
    assert.ok(captured < bytesToEmit, "far below what the child would have produced (proves truncation)");
  });

  it("(A) does NOT cap or kill a command whose output is under the cap (clean exit code preserved)", async () => {
    const spawnCommand = makeDefaultSpawnCommand(runEnv);
    const res = await withTimeout(
      spawnCommand(["/bin/sh", "-c", "printf 'hello world'; printf 'oops' 1>&2"], { cwd: "/tmp" }),
      30000,
      "small command",
    );
    assert.equal(res.code, 0, "a clean exit reports the child's own code, not the kill sentinel");
    assert.equal(res.stdout, "hello world");
    assert.equal(res.stderr, "oops");
  });
});
