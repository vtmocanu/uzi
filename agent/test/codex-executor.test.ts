import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { PassThrough } from "node:stream";

import {
  CodexExecutor,
  FailClosedExecutor,
  CodexAdviceCredentialBridge,
  buildRunLaneReconcile,
  buildAppServerRefreshBridge,
  makeCodexAdviceHarness,
  CODEX_PRODUCTION_PROVIDER,
  makeDefaultSpawnCommand,
  registeredRoot,
  buildCommandEnv,
  buildCodexToolHandlers,
  MAX_COMMAND_CAPTURE_BYTES,
  COMMAND_CAPTURE_KILLED_CODE,
  commandSandboxArgv,
  type CodexExecutorDeps,
  type CodexCommittedGenerationCell,
} from "../src/codex/codex-executor.js";
import { forgeToolNames } from "../src/forge-tools.js";
import { memoryToolNames } from "../src/memory-tools.js";
import { reportIncidentalIssueToolName } from "../src/findings-tools.js";
import { selectCodexBinding, CodexSelectionError, type CodexBinding } from "../src/codex/select.js";
import type { CodexLaunchRootResult, CodexProviderConfig } from "../src/codex/codex-harness.js";
import { ExecutionRegistry, newLocalExecutionEpoch, type RegisteredRoot } from "../src/codex/registry.js";
import { createCodexExecutionSafety } from "../src/codex/safety.js";
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
import type { BoundaryRequest } from "../src/harness.js";
import type { CodexEffectLaunchSpec, CodexRootHandle } from "../src/codex/launcher.js";

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
  private interceptor?: (note: CodexNotification, frameBytes: number) => boolean;

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
    // The pinned app-server auth handshake (createCodexAppServerAuth → authenticate): answer
    // initialize + account/login/start here so EVERY responder (and requestOverride) is free of
    // the auth plumbing. `account/login/start` echoes the login `type` (apiKey | chatgptAuthTokens).
    if (method === "initialize") {
      return Promise.resolve({ userAgent: "codex/0.153.2", codexHome: "/owned/codex", platformFamily: "unix", platformOs: "linux" } as T);
    }
    if (method === "account/login/start") {
      return Promise.resolve({ type: rec(params).type } as T);
    }
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

  installServerRequestInterceptor(interceptor: (note: CodexNotification, frameBytes: number) => boolean): () => void {
    this.interceptor = interceptor;
    return () => {
      if (this.interceptor === interceptor) this.interceptor = undefined;
    };
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

/** A root signal_done tool call on the active (thread, turn). The m2 implement/review loop
 *  finishes ONLY on a folded `done`, so a pre-approved run's implement turn must signal_done to
 *  resolve — exactly as a real implement turn declares the work finished. The broker each run
 *  builds carries the lead's root grants (isRoot + signal tools), so it accepts this and the
 *  harness surfaces its `{done:true}` on the main frame the reducer folds. */
function signalDone(threadId = "th-1", turnId = "tn-1", requestId = 700, callId = "c-done"): CodexNotification {
  return toolCall(requestId, "signal_done", {}, threadId, turnId, callId);
}

// --- binding + client + context builders ---------------------------------------
function bindingOf(codex: Record<string, unknown>): CodexBinding {
  const selection = selectCodexBinding({ codex });
  if (selection.kind !== "codex") throw new Error("expected a codex selection");
  return selection.binding;
}

const SUBSCRIPTION = {
  auth_mode: "subscription",
  access_token: "claim-tok",
  capability: "run-cap",
  generation: 3,
  chatgpt_account_id: "verified-account",
  chatgpt_plan_type: null,
};
const API_KEY = { auth_mode: "api_key", access_token: "claim-tok", capability: "run-cap" };

interface FakeClient {
  releaseCalls: { runId: string; capability: string }[];
  refreshCalls: { runId: string; operation_id: string; observed_generation: number }[];
  releaseCodex(runId: string, req: { capability: string }): Promise<{ access_token: string }>;
  refreshCodex(runId: string, req: { capability: string; operation_id: string; observed_generation: number }): Promise<{ access_token: string; generation: number; chatgpt_account_id: string; outcome: string }>;
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
      return { access_token: `${token}-refreshed`, generation: gen++, chatgpt_account_id: "verified-account", outcome: "advanced" };
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
  effectDisposes: () => number;
  providerLaunches: () => number;
  sessionOps: { adopt: number; removeCalls: number; inspect: number; persist: number };
  deps: CodexExecutorDeps;
}

function makeRig(opts: { responder?: Responder; token?: string } = {}): Rig {
  const transport = new FakeTransport(opts.responder ?? defaultResponder);
  const client = fakeClient(opts.token);
  const { root, reaped, disposed } = trackedRoot();
  const fh = fakeFileopHandle();
  const spawnCommandCalls: Rig["spawnCommandCalls"] = [];
  const fileopSpawns: Rig["fileopSpawns"] = [];
  const sessionOps = { adopt: 0, removeCalls: 0, inspect: 0, persist: 0 };
  let effectDisposes = 0;
  let providerLaunches = 0;
  const deps: CodexExecutorDeps = {
    launchProviderRoot: async (spec, authMode): Promise<CodexLaunchRootResult> => {
      providerLaunches += 1;
      // Under app-server auth the launcher receives the immutable auth mode, NOT a credential —
      // the token flows over the login RPC. Stash both for the tests. `spec.credentialValue` is
      // always undefined here (the harness zeroes it when appServerAuth is present).
      (transport as unknown as { launchAuthMode?: string; specCredential?: string }).launchAuthMode = authMode;
      (transport as unknown as { launchAuthMode?: string; specCredential?: string }).specCredential = spec.credentialValue;
      return { root, transport, supervisorPid: 1234 };
    },
    spawnCommand: async (argv, cmdOpts) => {
      spawnCommandCalls.push({ argv, opts: cmdOpts });
      return { code: 0, stdout: "ok", stderr: "" };
    },
    launchEffectRoot: async (spec: CodexEffectLaunchSpec): Promise<CodexRootHandle> => {
      fileopSpawns.push({ worktreePath: WORKSPACE, env: spec.env });
      const stdin = new PassThrough();
      const stdout = new PassThrough();
      const stderr = new PassThrough();
      return {
        started: {
          event: "started", supervisorPid: 200, childPid: 201, subreaper: true,
          nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true,
        },
        supervisorPid: 200,
        transport: { stdin, stdout, stderr },
        snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
        waitChild: async () => ({ event: "child_exit", code: 0 }),
        dispose: async () => {
          effectDisposes += 1;
          return {
            clean: true,
            event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" },
          };
        },
        failed: undefined,
        whenFailed: new Promise<Error>(() => undefined),
      };
    },
    wireFileop: () => fh.handle,
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
      persist: async () => {
        sessionOps.persist += 1;
        return { files: 0, bytes: 0 };
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
    effectDisposes: () => effectDisposes,
    providerLaunches: () => providerLaunches,
    sessionOps,
    deps,
  };
}

function makeExecutor(
  rig: { client: FakeClient; deps: CodexExecutorDeps },
  binding: CodexBinding,
  log: Logger = noopLog,
): CodexExecutor {
  return new CodexExecutor(
    log,
    "/data/agent-home/run-1",
    { binding, client: rig.client as never, provider },
    rig.deps,
  );
}

// --- a MULTI-EPOCH rig (m4 new-root resume) ------------------------------------
// The m4 recreation path builds a BRAND-NEW provider epoch (registry + harness + transport) at
// plan approval and at every cooperative checkpoint reap. A recreated harness consumes a FRESH
// transport's single-consumer notifications(), so each epoch needs its OWN FakeTransport + tracked
// root. `launchProviderRoot` hands out the next epoch's transport/root in launch order; each
// epoch's responder scripts that epoch's turn frames independently.
interface EpochFake {
  transport: FakeTransport;
  root: RegisteredRoot;
  reaped: () => number;
  disposed: () => number;
}
interface MultiRig {
  epochs: EpochFake[];
  client: FakeClient;
  sessionOps: { adopt: number; removeCalls: number; inspect: number; persist: number };
  providerLaunches: () => number;
  effectDisposes: () => number;
  deps: CodexExecutorDeps;
}
function makeMultiEpochRig(responders: Responder[], opts: { token?: string } = {}): MultiRig {
  const client = fakeClient(opts.token);
  const epochs: EpochFake[] = responders.map((responder) => {
    const t = trackedRoot();
    return { transport: new FakeTransport(responder), root: t.root, reaped: t.reaped, disposed: t.disposed };
  });
  const fh = fakeFileopHandle();
  const sessionOps = { adopt: 0, removeCalls: 0, inspect: 0, persist: 0 };
  let providerLaunches = 0;
  let effectDisposes = 0;
  const deps: CodexExecutorDeps = {
    launchProviderRoot: async (_spec, authMode): Promise<CodexLaunchRootResult> => {
      const epoch = epochs[providerLaunches];
      providerLaunches += 1;
      if (!epoch) throw new Error(`no scripted epoch for provider launch #${providerLaunches}`);
      (epoch.transport as unknown as { launchAuthMode?: string }).launchAuthMode = authMode;
      return { root: epoch.root, transport: epoch.transport, supervisorPid: 1000 + providerLaunches };
    },
    launchEffectRoot: async (_spec: CodexEffectLaunchSpec): Promise<CodexRootHandle> => {
      const stdin = new PassThrough();
      const stdout = new PassThrough();
      const stderr = new PassThrough();
      return {
        started: {
          event: "started", supervisorPid: 200, childPid: 201, subreaper: true,
          nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true,
        },
        supervisorPid: 200,
        transport: { stdin, stdout, stderr },
        snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
        waitChild: async () => ({ event: "child_exit", code: 0 }),
        dispose: async () => {
          effectDisposes += 1;
          return { clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } };
        },
        failed: undefined,
        whenFailed: new Promise<Error>(() => undefined),
      };
    },
    wireFileop: () => fh.handle,
    sessionStore: {
      adopt: async () => { sessionOps.adopt += 1; return { files: 0 }; },
      inspect: async () => { sessionOps.inspect += 1; return "absent"; },
      remove: async () => { sessionOps.removeCalls += 1; },
      persist: async () => { sessionOps.persist += 1; return { files: 0, bytes: 0 }; },
    },
    idleMs: 5000,
    wallMs: 5000,
    boundaryDeadlineMs: 200,
    childTurnDeadlineMs: 5000,
    commandTmpdir: "/run/runner-tmp",
  };
  return {
    epochs,
    client,
    sessionOps,
    providerLaunches: () => providerLaunches,
    effectDisposes: () => effectDisposes,
    deps,
  };
}

// Build a per-epoch responder for a MULTI-epoch rig. Each recreated epoch gets its OWN
// FakeTransport, so its counters restart (turnStartCount === 1 within the epoch). thread/start
// AND thread/resume both return this epoch's thread id (epoch 0 starts fresh; a recreated epoch
// resumes the prior thread on its fresh transport). turn/start pushes this epoch's threadStarted +
// turn frames, then returns the turn id (the harness consumes the queued frames after the RPC).
function epochResponder(
  threadId: string,
  turnId: string,
  pushFrames: (t: FakeTransport, threadId: string, turnId: string) => void,
): Responder {
  return (c) => {
    if (c.method === "thread/start" || c.method === "thread/resume") return { thread: { id: threadId } };
    if (c.method === "turn/start") {
      c.transport.push(threadStarted(threadId));
      pushFrames(c.transport, threadId, turnId);
      return { turn: { id: turnId } };
    }
    return {};
  };
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
      { auth_mode: "api_key", access_token: "SECRET-TOKEN", capability: "SECRET-CAP", generation: 1 }, // api_key_unexpected_subscription_fields
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
    rig.transport.push(threadStarted()).push(agentMessage("working on it")).push(signalDone()).push(turnCompleted("completed")).end();
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

  for (const scenario of [
    { name: "idle", idleMs: 20, wallMs: 1000, expected: /idle timeout/ },
    { name: "wall", idleMs: 1000, wallMs: 20, expected: /wall-clock timeout/ },
  ] as const) {
    it(`${scenario.name} trip propagates the active turn signal into the shell effect`, async () => {
      const rig = makeRig();
      let shellObservedAbort = false;
      rig.deps = {
        ...rig.deps,
        idleMs: scenario.idleMs,
        wallMs: scenario.wallMs,
        spawnCommand: async (_argv, opts) => new Promise((resolve) => {
          const settle = (): void => {
            shellObservedAbort = true;
            resolve({ code: 137, stdout: "", stderr: "" });
          };
          if (opts.signal?.aborted) settle();
          else opts.signal?.addEventListener("abort", settle, { once: true });
        }),
      };
      rig.transport
        .push(threadStarted())
        .push(toolCall(91, "Bash", { command: "sleep 60" }, "th-1", "tn-1", `trip-${scenario.name}`));
      await assert.rejects(
        withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(makeCtx().ctx), 3000, `${scenario.name} shell trip`),
        scenario.expected,
      );
      assert.equal(shellObservedAbort, true, "the pending shell received the turn abort before run cleanup");
    });
  }

  it("user cancel propagates the active turn signal into the shell effect", async () => {
    const controller = new AbortController();
    const rig = makeRig();
    let shellObservedAbort = false;
    rig.deps = {
      ...rig.deps,
      spawnCommand: async (_argv, opts) => new Promise((resolve) => {
        const settle = (): void => {
          shellObservedAbort = true;
          resolve({ code: 137, stdout: "", stderr: "" });
        };
        if (opts.signal?.aborted) settle();
        else opts.signal?.addEventListener("abort", settle, { once: true });
      }),
    };
    rig.transport
      .push(threadStarted())
      .push(toolCall(92, "Bash", { command: "sleep 60" }, "th-1", "tn-1", "trip-cancel"));
    const running = makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(makeCtx({ signal: controller.signal }).ctx);
    await tick();
    controller.abort();
    await assert.rejects(withTimeout(running, 3000, "cancel shell trip"), /run cancelled/);
    assert.equal(shellObservedAbort, true);
  });
});

// ================================================================================
describe("CodexExecutor: credential bridge + isolation", () => {
  it("(11) a subscription run releases a FRESH token EAGERLY, seeds the app-server login with it (NOT the launcher env), constructs the auth session, registers it as a logger secret then evicts it at terminal cleanup, and never emits it", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted()).push(signalDone()).push(turnCompleted("completed")).end();
    const { ctx, emitted } = makeCtx();
    const rlog = recordingLog();
    await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION), rlog.log).run(ctx), 3000, "release run");

    // The initial committed token is released EAGERLY, exactly once, with the binding capability.
    assert.equal(rig.client.releaseCalls.length, 1, "releaseCodex called once (eager initial release)");
    const rel = rig.client.releaseCalls[0];
    assert.ok(rel);
    assert.equal(rel.capability, "run-cap", "with the binding capability");
    assert.equal(rel.runId, "run-1");

    // The pinned app-server auth session was CONSTRUCTED and drove initialize + login. The login
    // carried the fresh token as a chatgptAuthTokens credential — the token flows over the login
    // RPC, NOT the launcher env (the transitional OPENAI_API_KEY path is gone).
    assert.ok(rig.transport.requests.some((r) => r.method === "initialize"), "the auth session initialized the connection");
    const login = rig.transport.requests.find((r) => r.method === "account/login/start");
    assert.ok(login, "the auth session performed account/login/start");
    const loginParams = rec(login.params);
    assert.equal(loginParams.type, "chatgptAuthTokens", "subscription logs in with chatgptAuthTokens (never an api key)");
    assert.equal(loginParams.accessToken, FRESH_TOKEN, "the eager-released token seeds the login RPC");
    assert.equal(loginParams.chatgptAccountId, "verified-account", "with the server-owned account id");

    // The launcher received the immutable auth mode and NO env credential (spec.credentialValue
    // is zeroed by the harness when appServerAuth is present).
    assert.equal((rig.transport as unknown as { launchAuthMode?: string }).launchAuthMode, "subscription");
    assert.equal((rig.transport as unknown as { specCredential?: string }).specCredential, undefined, "no credential in the launch spec");

    // The fresh token NEVER appears in an emitted message payload.
    assert.doesNotMatch(JSON.stringify(emitted), new RegExp(FRESH_TOKEN));
    // Registered with the logger (so any accidental embedding would be scrubbed) AND evicted at
    // terminal cleanup — the addSecret is balanced by a removeSecret, so a long-lived worker's
    // secret set does not grow per run (part C). No emitted log line carries the token verbatim.
    assert.deepEqual(rlog.added, [FRESH_TOKEN], "the fresh token was registered as a logger secret exactly once");
    assert.deepEqual(rlog.removed, [FRESH_TOKEN], "the fresh token was evicted at terminal cleanup (add balanced by remove)");
    assert.doesNotMatch(rlog.lines.join("\n"), new RegExp(FRESH_TOKEN));
  });

  it("(11c) a setup failure AFTER the eager release still evicts the initial token (add balanced by remove; releasedTokens ends empty)", async () => {
    // Inject an effect-root launcher that throws — the fileop/effect-root wiring runs AFTER the
    // eager initial release (which registered the token with the redactor) but BEFORE the model
    // turns, so this exercises the setup-failure path the widened terminal finally must cover.
    const rig = makeRig();
    rig.deps = {
      ...rig.deps,
      launchEffectRoot: async (): Promise<CodexRootHandle> => {
        throw new Error("effect-root launch failed during setup");
      },
    };
    const rlog = recordingLog();
    await assert.rejects(
      withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION), rlog.log).run(makeCtx().ctx), 3000, "setup-failure run"),
      /effect-root launch failed during setup/,
    );
    // FAIL-OLD/PASS-FIXED: before the widening the eager release registered the token but the
    // eviction lived only in the model-turn finally, so a setup failure left it in the shared
    // per-worker secret set forever (over-retention across runs). It must be added AND removed.
    assert.ok(rlog.added.includes(FRESH_TOKEN), "the initial token was registered with the redactor before the failure");
    assert.ok(rlog.removed.includes(FRESH_TOKEN), "the widened terminal finally evicted it on the setup-failure path");
    const added = rlog.added.filter((t) => t === FRESH_TOKEN).length;
    const removed = rlog.removed.filter((t) => t === FRESH_TOKEN).length;
    assert.equal(added, removed, "every registration is balanced by an eviction (releasedTokens ends empty)");
  });

  it("(11b) an api_key run logs in with an api key (not chatgptAuthTokens) and constructs the auth session", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted()).push(signalDone()).push(turnCompleted("completed")).end();
    await withTimeout(makeExecutor(rig, bindingOf(API_KEY)).run(makeCtx().ctx), 3000, "api_key auth run");
    const login = rig.transport.requests.find((r) => r.method === "account/login/start");
    assert.ok(login, "the api_key auth session performed account/login/start");
    const loginParams = rec(login.params);
    assert.equal(loginParams.type, "apiKey", "api_key logs in with an api key");
    assert.equal(loginParams.apiKey, FRESH_TOKEN, "the eager-released api key seeds the login RPC");
    assert.equal(loginParams.accessToken, undefined, "no chatgptAuthTokens fields on the api_key login");
    assert.equal((rig.transport as unknown as { launchAuthMode?: string }).launchAuthMode, "api_key");
  });

  it("(3-run) an api_key run NEVER calls refresh", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted()).push(signalDone()).push(turnCompleted("completed")).end();
    await withTimeout(makeExecutor(rig, bindingOf(API_KEY)).run(makeCtx().ctx), 3000, "api_key run");
    assert.equal(rig.client.refreshCalls.length, 0, "refreshCodex is never called on the run path");
  });

  it("(12) a resume seeds adopt (credential-free) AND still releases a fresh token", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted("resumed-1")).push(signalDone("resumed-1", "tn-1")).push(turnCompleted("completed", "resumed-1")).end();
    const { ctx } = makeCtx({ sessionId: "prior-session" });
    await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx), 3000, "resume run");
    assert.ok(rig.sessionOps.adopt >= 1, "adopt seeded the credential-free session subset");
    assert.equal(rig.client.releaseCalls.length, 1, "a resumed root still releases a fresh token");
    const resume = rig.transport.requests.find((r) => r.method === "thread/resume");
    assert.ok(resume, "the harness resumed the prior session");
  });

  it("(13) the fileop helper is spawned with a SCRUBBED, replaced env (no inherited secrets)", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted()).push(signalDone()).push(turnCompleted("completed")).end();
    await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(makeCtx().ctx), 3000, "fileop run");
    assert.equal(rig.fileopSpawns.length, 1);
    const spawn0 = rig.fileopSpawns[0];
    assert.ok(spawn0);
    const env = spawn0.env;
    // No packages provisioned in this rig (the real provisionRunTools is a no-op with no
    // config.tool_packages), so the command env is exactly the fixed scrubbed keys — no
    // folded nix vars, and the PATH is the fixed toolchain+system boundary (item 6).
    assert.deepEqual(Object.keys(env).sort(), ["HOME", "LANG", "PATH", "TMPDIR"], "exactly the scrubbed keys");
    assert.equal(env.PATH, "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/opt/uzi-toolchain/bin");
    assert.equal(env.LANG, "C");
    assert.match(String(env.HOME), /^\/tmp\/uzi-codex-command-/);
    assert.equal(env.TMPDIR, env.HOME, "one random Landlock-allowed tmp per command root");
    // Nothing inherited from the worker: no PAT/token/provider credential shape.
    assert.doesNotMatch(JSON.stringify(env), new RegExp(FRESH_TOKEN + "|run-cap|claim-tok"));
    assert.equal(spawn0.worktreePath, WORKSPACE);
  });

  it("(14) terminal cleanup reaps the registry roots, persists the session, and NEVER removes the store (m4-3)", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted()).push(signalDone()).push(turnCompleted("completed")).end();
    await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(makeCtx().ctx), 3000, "cleanup run");
    assert.ok(rig.reaped() >= 1, "the provider root was reaped");
    assert.ok(rig.disposed() >= 1, "the provider root was disposed");
    assert.ok(rig.fileopDisposed() >= 1, "the fileop handle was disposed");
    // m4-3: the executor PERSISTS the final session at terminal (so a park/preserve resume can
    // adopt it) and NO LONGER removes the store — the runner's runHome lifecycle owns removal.
    assert.ok(rig.sessionOps.persist >= 1, "the final session was persisted at terminal");
    assert.equal(rig.sessionOps.removeCalls, 0, "terminalCleanup NO LONGER removes the store (runner owns removal)");
  });

  it("(16) safety is populated and killAgentTree is undefined", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted()).push(signalDone()).push(turnCompleted("completed")).end();
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
    // The ROOT then signals done so the m2 implement/review loop finishes (delegation is not a
    // signal — only this root signal_done ends the run).
    rig.transport.push(signalDone("th-1", "tn-1")).push(turnCompleted("completed", "th-1", "tn-1")).end();
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
      .push(signalDone())
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
    // The ROOT then signals done so the m2 implement/review loop finishes.
    rig.transport.push(signalDone("th-1", "tn-1")).push(turnCompleted("completed", "th-1", "tn-1")).end();
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
  // The low-level supervisor is faked, while the production reservation, registry,
  // capture-cap and whole-root reap path is real.
  const runEnv: NodeJS.ProcessEnv = { PATH: process.env.PATH ?? "/usr/bin:/bin", LANG: "C", TMPDIR: "/tmp" };

  function supervisedOutput(output: Buffer, code = 0): (spec: CodexEffectLaunchSpec) => Promise<CodexRootHandle> {
    return async () => {
      const stdin = new PassThrough();
      const stdout = new PassThrough();
      const stderr = new PassThrough();
      let disposed = false;
      stdout.write(output);
      return {
        started: { event: "started", supervisorPid: 10, childPid: 11, subreaper: true, nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true },
        supervisorPid: 10,
        transport: { stdin, stdout, stderr },
        snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
        waitChild: async () => ({ event: "child_exit", code }),
        dispose: async () => {
          if (!disposed) { disposed = true; stdout.end(); stderr.end(); }
          return { clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } };
        },
        failed: undefined,
        whenFailed: new Promise<Error>(() => undefined),
      };
    };
  }

  it("(A) caps combined stdout+stderr at MAX_COMMAND_CAPTURE_BYTES, SIGKILLs the child, and RESOLVES (never rejects) with the truncated result", async () => {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    // The child would emit 8 MiB (>> the 1 MiB cap). WITHOUT the cap the seam would
    // accumulate the whole 8 MiB (and an UNBOUNDED producer like `yes` would grow the JS
    // string until the worker OOMs). WITH the cap it stops at 1 MiB and kills the child.
    const bytesToEmit = 8 * 1024 * 1024;
    const spawnCommand = makeDefaultSpawnCommand(
      registry,
      supervisedOutput(Buffer.alloc(bytesToEmit)),
      1000,
      "/data/runner/repo/run-1",
      runEnv,
    );
    const res = await withTimeout(
      spawnCommand(["/bin/sh", "-c", `head -c ${bytesToEmit} /dev/zero`], { cwd: "/data/runner/repo/run-1" }),
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
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const spawnCommand = makeDefaultSpawnCommand(
      registry,
      supervisedOutput(Buffer.from("hello world")),
      1000,
      "/data/runner/repo/run-1",
      runEnv,
    );
    const res = await withTimeout(
      spawnCommand(["/bin/sh", "-c", "printf 'hello world'"], { cwd: "/data/runner/repo/run-1" }),
      30000,
      "small command",
    );
    assert.equal(res.code, 0, "a clean exit reports the child's own code, not the kill sentinel");
    assert.equal(res.stdout, "hello world");
    assert.equal(res.stderr, "");
  });

  it("reserves before launch and an active-turn abort reaps the command root before rejecting", async () => {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(2));
    let reservedAtLaunch = false;
    let disposes = 0;
    const launch = async (): Promise<CodexRootHandle> => {
      reservedAtLaunch = registry.pendingLaunchCount() === 1;
      const stdin = new PassThrough();
      const stdout = new PassThrough();
      const stderr = new PassThrough();
      return {
        started: { event: "started", supervisorPid: 20, childPid: 21, subreaper: true, nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true },
        supervisorPid: 20,
        transport: { stdin, stdout, stderr },
        snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
        waitChild: async () => new Promise(() => undefined),
        dispose: async () => {
          disposes += 1;
          stdout.end(); stderr.end();
          return { clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } };
        },
        failed: undefined,
        whenFailed: new Promise<Error>(() => undefined),
      };
    };
    const spawnCommand = makeDefaultSpawnCommand(registry, launch, 1000, "/data/runner/repo/run-2", runEnv);
    const abort = new AbortController();
    const running = spawnCommand(["/bin/sh", "-c", "sleep 60"], {
      cwd: "/data/runner/repo/run-2",
      signal: abort.signal,
    });
    await new Promise<void>((resolve) => setImmediate(resolve));
    assert.equal(registry.hasLiveCommandRoot(), true, "registered before effect use");
    abort.abort();
    await assert.rejects(running, /command aborted/);
    assert.equal(reservedAtLaunch, true);
    assert.equal(disposes, 1, "abort awaited the supervisor's clean whole-root disposal");
    assert.equal(registry.hasLiveCommandRoot(), false);
  });

  it("builds the fixed Landlock wrapper argv for only the current worktree/private tmp", () => {
    const args = commandSandboxArgv(
      "/data/runner/repo/run-a",
      "/data/runner/repo/run-a/sub",
      "/bin/sh",
      ["-c", "pwd"],
      "/tmp/uzi-codex-command-test-a",
    );
    assert.deepEqual(args, [
      "--root", "/data/runner/repo/run-a",
      "--tmp", "/tmp/uzi-codex-command-test-a",
      "--cwd", "/data/runner/repo/run-a/sub",
      "--", "/bin/sh", "-c", "pwd",
    ]);
  });
});

// ================================================================================
// A fake client for the reconcile differential test: records each refresh's operation_id +
// observed_generation and can fail its first N refresh calls (to model a lost-reply RETRY).
interface DiffClient {
  refreshCalls: { operation_id: string; observed_generation: number }[];
  releaseCalls: { capability: string }[];
  releaseCodex(runId: string, req: { capability: string }): Promise<{ access_token: string }>;
  refreshCodex(
    runId: string,
    req: { capability: string; operation_id: string; observed_generation: number },
  ): Promise<{ access_token: string; generation: number; outcome: string }>;
}
function diffClient(opts: { failCalls?: number } = {}): DiffClient {
  const refreshCalls: DiffClient["refreshCalls"] = [];
  const releaseCalls: DiffClient["releaseCalls"] = [];
  let calls = 0;
  let gen = 10; // the generation each SUCCESSFUL refresh commits (10, 11, …)
  return {
    refreshCalls,
    releaseCalls,
    async releaseCodex(_runId, req) {
      releaseCalls.push({ capability: req.capability });
      return { access_token: "diff-release-tok" };
    },
    async refreshCodex(_runId, req) {
      calls += 1;
      refreshCalls.push({ operation_id: req.operation_id, observed_generation: req.observed_generation });
      if (opts.failCalls && calls <= opts.failCalls) throw new Error("refresh contended");
      return { access_token: "diff-refresh-tok", generation: gen++, outcome: "advanced" };
    },
  };
}

const RECONCILE_REQ: BoundaryRequest = { boundary: "finalize", deadlineMs: 1000 };
const RECONCILE_SIGNAL = new AbortController().signal;

describe("CodexExecutor: run-lane reconcile ⟷ advice bridge — differential (m4 part 4, no drift)", () => {
  it("run-lane reconciliation propagates boundary cancellation to the credential HTTP seam", async () => {
    const controller = new AbortController();
    let seenSignal: AbortSignal | undefined;
    const client = {
      refreshCodex: async (
        _runId: string,
        _request: unknown,
        _expected: unknown,
        signal?: AbortSignal,
      ): Promise<never> => {
        seenSignal = signal;
        return new Promise<never>((_, reject) => {
          signal?.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
        });
      },
      releaseCodex: async (): Promise<never> => { throw new Error("unused"); },
    };
    const reconcile = buildRunLaneReconcile("run-1", client as never, bindingOf(SUBSCRIPTION), () => {});
    const pending = reconcile(RECONCILE_REQ, controller.signal);
    controller.abort();
    assert.equal((await pending).kind, "blocked");
    assert.equal(seenSignal, controller.signal);
  });

  it("subscription: BOTH reuse the operation id across a retry AND advance the generation on success", async () => {
    // Advice bridge: a failed refresh() then a retry reuse the SAME op id; the observed
    // generation is unchanged on the failed attempt (no double exchange).
    const ac = diffClient({ failCalls: 1 });
    const bridge = new CodexAdviceCredentialBridge("run-1", ac as never, bindingOf(SUBSCRIPTION));
    await assert.rejects(bridge.refresh(), /contended/, "the advice bridge fails closed");
    await bridge.refresh(); // retry succeeds
    assert.equal(ac.refreshCalls.length, 2);
    assert.equal(ac.refreshCalls[0]!.operation_id, ac.refreshCalls[1]!.operation_id, "advice reuses the op id across a retry");
    assert.equal(ac.refreshCalls[0]!.observed_generation, 3, "advice starts from the claim generation");
    assert.equal(ac.refreshCalls[1]!.observed_generation, 3, "advice does NOT advance the generation on a failed attempt");

    // Run-lane reconcile: mirror it — a `blocked` outcome then a `ready` retry reuse the op id.
    const rc = diffClient({ failCalls: 1 });
    const reconcile = buildRunLaneReconcile("run-1", rc as never, bindingOf(SUBSCRIPTION), () => {});
    assert.equal((await reconcile(RECONCILE_REQ, RECONCILE_SIGNAL)).kind, "blocked", "the run-lane reconcile fails closed on the first attempt");
    assert.equal((await reconcile(RECONCILE_REQ, RECONCILE_SIGNAL)).kind, "ready", "the retry succeeds");
    assert.equal(rc.refreshCalls.length, 2);
    assert.equal(rc.refreshCalls[0]!.operation_id, rc.refreshCalls[1]!.operation_id, "run-lane reuses the op id across the retry");
    assert.equal(rc.refreshCalls[0]!.observed_generation, 3);
    assert.equal(rc.refreshCalls[1]!.observed_generation, 3, "run-lane re-uses the SAME observed generation (unchanged on failure)");
    // A NEW boundary after success mints a FRESH op id and uses the advanced generation.
    assert.equal((await reconcile(RECONCILE_REQ, RECONCILE_SIGNAL)).kind, "ready");
    assert.notEqual(rc.refreshCalls[2]!.operation_id, rc.refreshCalls[0]!.operation_id, "a new boundary mints a fresh op id");
    assert.equal(rc.refreshCalls[2]!.observed_generation, 10, "and it uses the advanced generation the prior success committed");
  });

  it("api_key: BOTH perform ZERO refresh (advice fails closed on refresh; run-lane releases only)", async () => {
    const ac = diffClient();
    const bridge = new CodexAdviceCredentialBridge("run-1", ac as never, bindingOf(API_KEY));
    await bridge.release();
    await assert.rejects(bridge.refresh(), /not permitted for an api_key/);
    assert.equal(ac.refreshCalls.length, 0, "advice never refreshes an api_key credential");

    const rc = diffClient();
    const reconcile = buildRunLaneReconcile("run-1", rc as never, bindingOf(API_KEY), () => {});
    assert.equal((await reconcile(RECONCILE_REQ, RECONCILE_SIGNAL)).kind, "ready");
    assert.equal(rc.refreshCalls.length, 0, "the run-lane api_key reconcile performs ZERO refresh");
    assert.equal(rc.releaseCalls.length, 1, "it freshly releases (re-authorizes) instead");
  });

  it("blocked outcome fails closed for BOTH (a persistence failure never yields a ready/token)", async () => {
    const ac = diffClient({ failCalls: 99 });
    const bridge = new CodexAdviceCredentialBridge("run-1", ac as never, bindingOf(SUBSCRIPTION));
    await assert.rejects(bridge.refresh(), /contended/);

    const rc = diffClient({ failCalls: 99 });
    const reconcile = buildRunLaneReconcile("run-1", rc as never, bindingOf(SUBSCRIPTION), () => {});
    const out = await reconcile(RECONCILE_REQ, RECONCILE_SIGNAL);
    assert.equal(out.kind, "blocked");
    if (out.kind === "blocked") {
      assert.ok(out.errors.length >= 1, "the blocked outcome carries evidence");
      assert.doesNotMatch(JSON.stringify(out.errors), /diff-refresh-tok|diff-release-tok/, "no token leaks into the neutral outcome");
    }
  });
});

describe("CodexExecutor: app-server refresh bridge — generation-after-delivery (m1)", () => {
  // A subscription refresh client that REPLAYS a same operation_id (returns the generation it
  // first committed for that op) and ADVANCES a new op to observed+1, mirroring the server's
  // advanced/replayed contract. Records every observed_generation it was sent.
  function bridgeClient(): {
    calls: { operation_id: string; observed_generation: number }[];
    refreshCodex: (
      runId: string,
      req: { capability: string; operation_id: string; observed_generation: number },
    ) => Promise<{ auth_mode: "subscription"; access_token: string; generation: number; chatgpt_account_id: string; chatgpt_plan_type: null; outcome: string }>;
  } {
    const calls: { operation_id: string; observed_generation: number }[] = [];
    const committedByOp = new Map<string, number>();
    return {
      calls,
      async refreshCodex(_runId, req) {
        calls.push({ operation_id: req.operation_id, observed_generation: req.observed_generation });
        let g = committedByOp.get(req.operation_id);
        if (g === undefined) {
          g = req.observed_generation + 1; // ADVANCE a fresh op to observed+1
          committedByOp.set(req.operation_id, g);
        }
        return {
          auth_mode: "subscription",
          access_token: `bridge-tok-${g}`,
          generation: g,
          chatgpt_account_id: "verified-account",
          chatgpt_plan_type: null,
          outcome: "advanced",
        };
      },
    };
  }

  it("advances the SHARED generation only on a NEW operation id; a same-op retry replays the OLD committed generation (fail-old/pass-fixed for B)", async () => {
    const client = bridgeClient();
    const committed: CodexCommittedGenerationCell = { value: 3 };
    const registered: string[] = [];
    const bridge = buildAppServerRefreshBridge("run-1", client as never, bindingOf(SUBSCRIPTION), committed, (t) => registered.push(t));
    const signal = new AbortController().signal;

    // op-1 first delivery: observed = the seed generation (3). The committed generation is HELD
    // (as pending), NOT advanced — the token has not yet been proven delivered to Codex.
    const first = await bridge.refresh({ operationId: "op-1", signal });
    assert.equal(first.accountId, "verified-account", "the bridge returns the server-owned account id");
    assert.equal(committed.value, 3, "committed unchanged until a NEW op id proves delivery");

    // op-1 RETRY (a lost-delivery replay reuses the SAME op id): the OLD committed generation (3)
    // is replayed as observed, NOT the not-yet-delivered pending one. A version that advances
    // committed immediately after the response would send 4 here — this is the fail-old assertion.
    await bridge.refresh({ operationId: "op-1", signal });

    // op-2 is a genuinely NEW op id → the auth owner minted it only after a fully-delivered
    // success → fold pending → committed, then send the advanced generation.
    await bridge.refresh({ operationId: "op-2", signal });

    assert.deepEqual(
      client.calls.map((c) => c.observed_generation),
      [3, 3, 4],
      "observed_generation: seed, replay-OLD-on-retry, then advanced-after-delivery",
    );
    assert.equal(committed.value, 4, "the shared cell advanced ONLY after a NEW op id proved delivery");
    // Every returned token was registered with the redactor.
    assert.ok(registered.length >= 3 && registered.every((t) => t.startsWith("bridge-tok-")), "each refreshed token is redactor-registered");
  });

  it("forwards caller cancellation to the refresh HTTP seam (aborts the in-flight refresh)", async () => {
    const controller = new AbortController();
    let seenSignal: AbortSignal | undefined;
    const client = {
      refreshCodex: (_runId: string, _req: unknown, _expected: unknown, signal?: AbortSignal): Promise<never> => {
        seenSignal = signal;
        return new Promise<never>((_, reject) => {
          signal?.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
        });
      },
    };
    const bridge = buildAppServerRefreshBridge("run-1", client as never, bindingOf(SUBSCRIPTION), { value: 3 }, () => {});
    const pending = bridge.refresh({ operationId: "op-1", signal: controller.signal });
    controller.abort();
    await assert.rejects(pending, /aborted/);
    assert.equal(seenSignal, controller.signal, "the caller's signal reached the refresh HTTP seam");
  });

  it("an api_key binding builds NO refresh bridge (fail-closed)", () => {
    assert.throws(
      () => buildAppServerRefreshBridge("run-1", fakeClient() as never, bindingOf(API_KEY), { value: undefined }, () => {}),
      /requires a subscription binding/,
    );
  });
});

describe("CodexExecutor: advice lane app-server refresh bridge — generation-after-delivery (m1)", () => {
  // A subscription refresh client that ADVANCES a fresh operation_id to observed+1 and REPLAYS a
  // same operation_id (returns the generation it first committed for that op), AND — like the real
  // client's validateCodexRefreshResponse — REJECTS a response whose generation is not strictly
  // greater than the observed_generation the caller sent. Records every observed_generation sent.
  function validatingRefreshClient(): {
    calls: { operation_id: string; observed_generation: number }[];
    refreshCodex: (
      runId: string,
      req: { capability: string; operation_id: string; observed_generation: number },
    ) => Promise<{ auth_mode: "subscription"; access_token: string; generation: number; chatgpt_account_id: string; chatgpt_plan_type: null; outcome: string }>;
  } {
    const calls: { operation_id: string; observed_generation: number }[] = [];
    const committedByOp = new Map<string, number>();
    return {
      calls,
      async refreshCodex(_runId, req) {
        calls.push({ operation_id: req.operation_id, observed_generation: req.observed_generation });
        let g = committedByOp.get(req.operation_id);
        if (g === undefined) {
          g = req.observed_generation + 1; // ADVANCE a fresh op to observed+1
          committedByOp.set(req.operation_id, g);
        }
        // The real client rejects a generation that does not strictly advance past what it sent.
        if (g <= req.observed_generation) throw new Error("codex refresh generation <= observed (rejected)");
        return {
          auth_mode: "subscription",
          access_token: `advice-tok-${g}`,
          generation: g,
          chatgpt_account_id: "verified-account",
          chatgpt_plan_type: null,
          outcome: "advanced",
        };
      },
    };
  }

  // The OLD (now-removed) buildAdviceAuthConfig behavior: map the app-server op id onto the advice
  // bridge's RETAINED id and let CodexAdviceCredentialBridge.refresh advance the observed
  // generation IMMEDIATELY on each success. Reconstructed here ONLY to prove the fix — a
  // delivery-failed same-op retry must break it.
  function immediateAdvanceAdviceBridge(
    bridge: CodexAdviceCredentialBridge,
    accountId: string,
  ): { refresh: (r: { operationId: string; signal: AbortSignal }) => Promise<{ accessToken: string; accountId: string }> } {
    let lastSeenOperationId: string | undefined;
    return {
      refresh: async ({ operationId, signal }): Promise<{ accessToken: string; accountId: string }> => {
        if (operationId !== lastSeenOperationId) {
          bridge.beginRefreshOperation();
          lastSeenOperationId = operationId;
        }
        const accessToken = await bridge.refresh(signal);
        return { accessToken, accountId };
      },
    };
  }

  it("advances the observed generation only on a NEW op id; a delivery-failed same-op retry replays the OLD generation (reuses the run-lane bridge)", async () => {
    const client = validatingRefreshClient();
    const bridge = new CodexAdviceCredentialBridge("run-1", client as never, bindingOf(SUBSCRIPTION));
    const registered: string[] = [];
    const refreshBridge = bridge.buildSubscriptionRefreshBridge((t) => registered.push(t));
    const signal = new AbortController().signal;

    // op-1 first delivery: observed = the binding generation (3). Held pending, NOT advanced.
    const first = await refreshBridge.refresh({ operationId: "op-1", signal });
    assert.equal(first.accountId, "verified-account", "the bridge returns the server-owned account id");
    // op-1 RETRY: the pinned app-server auth owner RETAINS the op id on a delivery failure, so the
    // advice bridge must replay the OLD committed generation (3), not an advanced one.
    await refreshBridge.refresh({ operationId: "op-1", signal });
    // op-2: a genuinely NEW op id (minted only after a fully-delivered success) → fold + advance.
    await refreshBridge.refresh({ operationId: "op-2", signal });

    assert.deepEqual(
      client.calls.map((c) => c.observed_generation),
      [3, 3, 4],
      "observed_generation: seed, replay-OLD-on-retry, advanced-after-delivery — byte-identical to the run lane",
    );
    assert.ok(
      registered.length >= 3 && registered.every((t) => t.startsWith("advice-tok-")),
      "each refreshed advice token is redactor-registered (closes the advice-lane redactor gap)",
    );
  });

  it("FAIL-OLD: the removed immediate-advance advice bridge re-sends an advanced generation on a same-op retry and is REJECTED", async () => {
    const client = validatingRefreshClient();
    const bridge = new CodexAdviceCredentialBridge("run-1", client as never, bindingOf(SUBSCRIPTION));
    const legacy = immediateAdvanceAdviceBridge(bridge, "verified-account");
    const signal = new AbortController().signal;

    await legacy.refresh({ operationId: "op-1", signal }); // first delivery advances observed 3 → 4
    // The delivery-failed retry reuses the SAME app-server op id. The immediate-advance bridge now
    // sends the ALREADY-ADVANCED observed generation (4); the server replays op-1's committed 4,
    // which is NOT > 4, so the response is rejected — exactly the inconsistency the fix removes.
    await assert.rejects(legacy.refresh({ operationId: "op-1", signal }), /generation <= observed/);
    assert.deepEqual(
      client.calls.map((c) => c.observed_generation),
      [3, 4],
      "the immediate-advance bridge sent an advanced observed generation on the retry (the bug)",
    );
  });
});

describe("CodexExecutor: F1 registry teardown relocation", () => {
  it("an unconfirmed launcher dispose is a failing RegisteredRoot.dispose, never marked disposed", async () => {
    const handle: CodexRootHandle = {
      started: { event: "started", supervisorPid: 30, childPid: 31, subreaper: true, nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true },
      supervisorPid: 30,
      transport: { stdin: null, stdout: null, stderr: null },
      snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
      waitChild: async () => ({ event: "child_exit", code: 0 }),
      dispose: async () => ({ clean: false, reason: "deadline" }),
      failed: undefined,
      whenFailed: new Promise<Error>(() => undefined),
    };
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(8));
    const root = registeredRoot(handle, "command");
    const reservation = registry.reserveLaunch("command");
    assert.equal(reservation.kind, "reserved");
    if (reservation.kind !== "reserved") return;
    assert.equal(registry.registerRoot(reservation.reservation, root).ok, true);
    const disposed = await registry.disposeTools(50);
    assert.equal(disposed.kind, "incomplete");
    assert.equal(registry.isPoisoned(), true);
  });

  it("(F1) deferRegistryTeardown: run()'s finally leaves the registry ALIVE, then a runner sink reaps and the terminal dispose tears down", async () => {
    const rig = makeRig();
    rig.deps = { ...rig.deps, deferRegistryTeardown: true };
    rig.transport.push(threadStarted()).push(signalDone()).push(turnCompleted("completed")).end();
    const rlog = recordingLog();
    const exec = makeExecutor(rig, bindingOf(SUBSCRIPTION), rlog.log);
    await withTimeout(exec.run(makeCtx().ctx), 3000, "deferred run");
    // run()'s finally did NOT reap/dispose the registry — it survives for the runner's sinks.
    assert.equal(rig.reaped(), 0, "the provider root was NOT reaped by run()");
    assert.equal(rig.disposed(), 0, "the provider root was NOT disposed by run()");
    assert.equal(rig.effectDisposes(), 0, "the registered persistent fileop root remains for the durability boundary");
    assert.ok(exec.safety, "safety is populated");

    // Simulate the runner's terminal/finalize withBoundary sink, then its executeClaim-finally
    // dispose. The subscription reconcile runs before the boundary.
    await exec.safety!.withBoundary({ boundary: "finalize", deadlineMs: 200 }, async () => {});
    assert.ok(rig.reaped() >= 1, "the runner's finalize sink reaped the provider root");
    assert.ok(rig.effectDisposes() >= 1, "the same boundary reaped the registered fileop command root");
    assert.ok(rig.client.refreshCalls.length >= 1, "the sink's subscription reconcile refreshed first");
    // The sink's reconcile registered a FRESH refresh token with the redactor; it is NOT yet
    // evicted (run()'s finally already ran, before this post-run sink).
    const sinkTok = `${FRESH_TOKEN}-refreshed`;
    assert.ok(rlog.added.includes(sinkTok), "the post-run sink reconcile registered its refresh token");
    assert.ok(!rlog.removed.includes(sinkTok), "the sink token is NOT evicted until the terminal dispose");
    await exec.safety!.dispose({ boundary: "terminal", deadlineMs: 200 });
    assert.ok(rig.disposed() >= 1, "the runner's terminal dispose tore down the provider root");
    // FAIL-OLD/PASS-FIXED (executor adoption of onDispose): the terminal dispose evicts the
    // post-run sink token via the executor's onDispose closure, so EVERY registered token is
    // balanced by a removal — no leaked redactor entry per Codex run. Deleting the executor's
    // 4th `createCodexExecutionSafety` arg leaves `sinkTok` in `added` but not `removed`.
    assert.ok(rlog.removed.includes(sinkTok), "the terminal dispose evicted the post-run sink token (onDispose adoption)");
    assert.deepEqual([...rlog.added].sort(), [...rlog.removed].sort(), "every registered token was evicted (no redactor leak)");
  });

  it("(F1) standalone (no deferRegistryTeardown): run()'s finally BACKSTOPS the registry teardown (provider root torn down)", async () => {
    const rig = makeRig();
    rig.transport.push(threadStarted()).push(signalDone()).push(turnCompleted("completed")).end();
    await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(makeCtx().ctx), 3000, "standalone run");
    assert.ok(rig.reaped() >= 1, "the backstop reaped the provider root");
    assert.ok(rig.disposed() >= 1, "the backstop disposed the provider root");
  });

  it("(F1) an error mid-turn still tears down the provider root via the standalone backstop", async () => {
    const rig = makeRig({
      responder: (c) => {
        if (c.method === "turn/start") throw new Error("boom: mid-turn failure");
        return defaultResponder(c);
      },
    });
    rig.transport.push(threadStarted());
    await assert.rejects(
      withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(makeCtx().ctx), 3000, "error run"),
      /boom: mid-turn failure/,
    );
    // The provider root was registered before turn/start threw; the backstop still tore it down.
    assert.ok(rig.disposed() >= 1, "the error-before-completion path tore down the provider root via the backstop");
  });
});

// ================================================================================
// The FIXED production Codex vendor target. A silent change to name/baseUrl/envKey/model
// would re-point every DARK production run at a different endpoint/model; pin it byte-for-byte
// so a mutation of the production constructor's constant is caught (mutation evidence).
describe("CodexExecutor: production provider constant (mutation evidence)", () => {
  it("pins the fixed production Codex vendor target byte-for-byte", () => {
    assert.deepEqual(CODEX_PRODUCTION_PROVIDER, {
      name: "openai",
      baseUrl: "https://api.openai.com/v1",
      envKey: "OPENAI_API_KEY",
      model: "gpt-6-astra",
    });
  });
});

// ================================================================================
// PRD #1171 m5 — the per-turn PHASE-CORRECT broker. The broker enforces the plan-phase
// file-write ban through `grants.phase` (broker.dispatchFileWrite → write_denied_in_plan), so
// the broker serving the PLAN turn must carry plan-phase grants. The old single implement-phase
// broker (frozen at construction, reused across both turns) left that ban INERT during the plan
// turn — a model apply_patch/Write mutated the worktree BEFORE approval. These tests drive a
// real plan turn and a real implement turn and are fail-old/pass-fixed.
describe("CodexExecutor: per-turn phase-correct broker (plan write ban)", () => {
  // A file-write callback carried on the turn each run drives (Write → apply_patch → file_write).
  const WRITE_ID = 77;
  const writeCall = (threadId: string, turnId: string): CodexNotification =>
    toolCall(
      WRITE_ID,
      "Write",
      { file_path: "PLAN_NOTES.md", content: "must not be written during planning" },
      threadId,
      turnId,
      "cc-write",
    );

  function replyForId(rig: Rig, id: number): { success?: boolean; text?: string } {
    const entry = rig.transport.responses.find((r) => r.requestId === id);
    const response = rec(entry?.response);
    const result = rec(response.result) as { success?: boolean; contentItems?: { text?: string }[] };
    return { success: result.success, text: result.contentItems?.[0]?.text };
  }

  // A fileop client that RECORDS every op it is asked to run, so a test can prove the worktree
  // was (or was not) mutated: under the plan-phase ban the write is denied BEFORE any fileop.
  function recordingFileop(): { handle: FileopHelperHandle; ops: string[] } {
    const ops: string[] = [];
    return {
      handle: {
        client: {
          op: async (req) => {
            ops.push(req.op);
            return { ok: true, size: 32 };
          },
        },
        dispose: async () => {},
      },
      ops,
    };
  }

  // gatePlan is provided ONLY to activate run()'s plan-gate branch. The plan turn here streams a
  // Write but NEVER a submit_plan, so it is a GENUINELY EMPTY plan turn: the m2 signal-routing
  // folds nothing (no submit_plan callback), planResult.plan stays undefined, and run() fails
  // closed on the retained "produced no plan" guard — so this approver is never invoked. The
  // write-ban side effects (the DENIED reply + the untouched fileop) are the assertion.
  const gatePlan: NonNullable<RunContext["gatePlan"]> = async () => ({ kind: "cancel" });

  it("(P1) a Write during the PLAN turn is DENIED (write_denied_in_plan) and never reaches the fileop helper; the SAME Write during the IMPLEMENT turn is APPLIED", async () => {
    // --- PLAN turn: the broker MUST be plan-phase, so the write is denied before any fileop.
    const planRig = makeRig();
    const planFileop = recordingFileop();
    planRig.deps = { ...planRig.deps, wireFileop: () => planFileop.handle };
    planRig.transport.push(threadStarted()).push(writeCall("th-1", "tn-1")).push(turnCompleted("completed")).end();
    const { ctx: planCtx } = makeCtx({ planApproved: false, approvedPlan: undefined, gatePlan });
    // The write ban fires mid-turn; run() then rejects on the "produced no plan" guard because
    // this plan turn submits no plan (a genuinely empty plan turn). The rejection is incidental —
    // the recorded reply + fileop ops are the point.
    await assert.rejects(
      withTimeout(makeExecutor(planRig, bindingOf(SUBSCRIPTION)).run(planCtx), 3000, "plan-turn write"),
      /produced no plan/,
    );
    const planReply = replyForId(planRig, WRITE_ID);
    assert.equal(planReply.success, false, "the plan-turn Write was DENIED (fails on the old single implement-phase broker)");
    assert.match(planReply.text ?? "", /not permitted during the plan phase/, "the deny is write_denied_in_plan");
    assert.deepEqual(planFileop.ops, [], "no fileop op ran during the plan phase — the worktree was NOT mutated");

    // --- IMPLEMENT turn: the same Write is APPLIED (a pre-approved run drives only implement).
    const implRig = makeRig();
    const implFileop = recordingFileop();
    implRig.deps = { ...implRig.deps, wireFileop: () => implFileop.handle };
    implRig.transport.push(threadStarted()).push(writeCall("th-1", "tn-1")).push(signalDone()).push(turnCompleted("completed")).end();
    const { ctx: implCtx } = makeCtx(); // pre-approved (planApproved + approvedPlan) → implement turn only
    const result = await withTimeout(makeExecutor(implRig, bindingOf(SUBSCRIPTION)).run(implCtx), 3000, "implement-turn write");
    assert.equal(result.branch, "agent/issue-42");
    assert.equal(replyForId(implRig, WRITE_ID).success, true, "the implement-turn Write was APPLIED");
    assert.deepEqual(implFileop.ops, ["write"], "the write reached the fileop helper during the implement phase");
  });

  it("(P2) a subagent spawned during the PLAN turn inherits plan-phase grants — its Write is DENIED and never mutates the worktree", async () => {
    const agents: AgentTemplate[] = [
      { name: "lead", description: "the lead", prompt_body: "lead body", tools: null, skills: [] },
      { name: "coder", description: "a coder", prompt_body: "coder body", tools: null, skills: [] },
    ];
    const CHILD_WRITE_ID = 88;
    const responder: Responder = (c) => {
      if (c.method === "thread/start") return { thread: { id: c.threadStartCount === 1 ? "th-1" : "th-child" } };
      if (c.method === "turn/start") {
        if (c.turnStartCount === 1) return { turn: { id: "tn-1" } };
        // The CHILD turn: a Write callback (denied under plan-phase CHILD grants) + its terminal.
        c.transport
          .push(toolCall(CHILD_WRITE_ID, "Write", { file_path: "CHILD.md", content: "child must not write in plan" }, "th-child", "tn-child", "cc-child-write"))
          .push(turnCompleted("completed", "th-child", "tn-child"));
        return { turn: { id: "tn-child" } };
      }
      if (c.method === "turn/interrupt") return {};
      return {};
    };
    const rig = makeRig({ responder });
    const childFileop = recordingFileop();
    rig.deps = { ...rig.deps, wireFileop: () => childFileop.handle };
    rig.transport.push(threadStarted()).push(toolCall(1, "spawn_agent", { role: "coder", prompt: "help" }, "th-1", "tn-1", "c-root"));

    const { ctx } = makeCtx({ planApproved: false, approvedPlan: undefined, gatePlan, agents });
    const runP = makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx);

    // The parent spawn_agent callback resolves only after the child settles; once it has, close
    // the root plan turn (which submits no plan, so it yields no plan and run() fails closed).
    await waitFor(() => rig.transport.responses.some((r) => r.requestId === 1), "parent spawn_agent reply", 5000);
    rig.transport.push(turnCompleted("completed", "th-1", "tn-1")).end();
    await assert.rejects(withTimeout(runP, 5000, "plan-turn subagent write"), /produced no plan/);

    const childReply = replyForId(rig, CHILD_WRITE_ID);
    assert.equal(childReply.success, false, "the child's plan-turn Write was DENIED (fails on the old implement-phase child grants)");
    assert.match(childReply.text ?? "", /not permitted during the plan phase/, "the child deny is write_denied_in_plan");
    assert.deepEqual(childFileop.ops, [], "no child fileop op ran during the plan phase");
    assert.equal(rig.transport.turnStartCount, 2, "the child turn ran on the same transport (demuxed)");
  });
});

// ================================================================================
// PRD #1171 m2 — the trusted ROOT signal-routing frame + the bounded implement/review loop.
// A root submit_plan/signal_done/checkpoint callback the broker ACCEPTS now surfaces its
// scanned signals on a main-origin frame the run-lane reducer folds (codex-harness.ts), so the
// plan gate gates on a REAL plan and the implement loop finishes on a real signal_done. These
// are fail-old/pass-fixed: without the signals frame planResult.plan is undefined and run()
// throws "produced no plan" before the gate, so a resolving success test can only pass wired.
describe("CodexExecutor: plan folding + implement/review loop (m2)", () => {
  it("(m2-1) a folded submit_plan gates, approval recreates a fresh provider epoch, and a root signal_done on the NEW root resolves { branch }", async () => {
    // m4 change: plan approval now RECREATES the provider epoch (new-root resume), so the plan
    // turn and the implement turn run on DISTINCT provider roots/transports. Each epoch is scripted
    // independently on its own FakeTransport.
    const rig = makeMultiEpochRig([
      epochResponder("th-1", "tn-1", (t, th, tn) => {
        t.push(toolCall(1, "submit_plan", { plan_md: "the codex plan body" }, th, tn, "c-plan")).push(turnCompleted("completed", th, tn));
      }),
      epochResponder("resumed-1", "tn-2", (t, th, tn) => {
        t.push(toolCall(2, "signal_done", {}, th, tn, "c-done")).push(turnCompleted("completed", th, tn));
      }),
    ]);
    let gated = false;
    let gatedPlan: string | undefined;
    const gatePlan: NonNullable<RunContext["gatePlan"]> = async (planMd) => {
      gated = true;
      gatedPlan = planMd;
      return { kind: "approve", selection: { source: "own", agents: [] } } as never;
    };
    const { ctx } = makeCtx({ planApproved: false, approvedPlan: undefined, gatePlan });
    const result = await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx), 5000, "plan→implement run");

    // FAIL-OLD/PASS-FIXED: without the m2 signals frame planResult.plan is undefined and run()
    // throws "produced no plan" BEFORE the gate — so a green here proves the fold is wired.
    assert.equal(gated, true, "the folded plan reached the gate");
    assert.equal(gatedPlan, "the codex plan body", "the scanned plan_md was gated verbatim");
    assert.equal(result.branch, "agent/issue-42", "the implement turn on the NEW epoch finished on a root signal_done");
    assert.equal(rig.epochs[0]!.transport.turnStartCount, 1, "the plan turn ran on epoch 0");
    assert.equal(rig.epochs[1]!.transport.turnStartCount, 1, "the implement turn ran on epoch 1 (the recreated root)");
  });

  it("passes reviewer feedback to the revised plan turn and bounds revision attempts", async () => {
    const feedback = "Split the database change from the worker integration.";
    const planResponder: Responder = (c) => {
      if (c.method === "thread/start") return { thread: { id: "th-plan" } };
      if (c.method === "turn/start") {
        const turnId = `tn-plan-${c.turnStartCount}`;
        if (c.turnStartCount === 1) c.transport.push(threadStarted("th-plan"));
        c.transport
          .push(toolCall(c.turnStartCount, "submit_plan", { plan_md: `plan-${c.turnStartCount}` }, "th-plan", turnId, `c-plan-${c.turnStartCount}`))
          .push(turnCompleted("completed", "th-plan", turnId));
        return { turn: { id: turnId } };
      }
      return {};
    };
    const rig = makeMultiEpochRig([
      planResponder,
      epochResponder("resumed-plan", "tn-implement", (t, th, tn) => {
        t.push(toolCall(99, "signal_done", {}, th, tn, "c-done")).push(turnCompleted("completed", th, tn));
      }),
    ]);
    let gateCalls = 0;
    const gatePlan: NonNullable<RunContext["gatePlan"]> = async () => {
      gateCalls += 1;
      return gateCalls === 1
        ? { kind: "revise", feedback }
        : { kind: "approve", selection: { source: "own", agents: [] } } as never;
    };
    const { ctx } = makeCtx({
      planApproved: false,
      approvedPlan: undefined,
      gatePlan,
      config: { plan_max_revisions: 1 },
    });

    await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx), 5000, "revised plan run");
    const planTurns = rig.epochs[0]!.transport.requests.filter((request) => request.method === "turn/start");
    assert.equal(planTurns.length, 2);
    assert.doesNotMatch(JSON.stringify(planTurns[0]!.params), new RegExp(feedback));
    assert.match(JSON.stringify(planTurns[1]!.params), new RegExp(feedback));

    const capped = makeRig({
      responder: (c) => {
        if (c.method === "thread/start") return { thread: { id: "th-1" } };
        if (c.method === "turn/start") {
          c.transport.push(threadStarted()).push(toolCall(7, "submit_plan", { plan_md: "unchanged" }, "th-1", "tn-1", "c-plan")).push(turnCompleted("completed"));
          return { turn: { id: "tn-1" } };
        }
        return {};
      },
    });
    const cappedCtx = makeCtx({
      planApproved: false,
      approvedPlan: undefined,
      gatePlan: async () => ({ kind: "revise", feedback }),
      config: { plan_max_revisions: 0 },
    }).ctx;
    await assert.rejects(
      withTimeout(makeExecutor(capped, bindingOf(SUBSCRIPTION)).run(cappedCtx), 5000, "revision cap"),
      /revision budget exhausted/,
    );
    assert.equal(capped.transport.turnStartCount, 1, "zero revision budget cannot start another plan turn");
  });

  it("(m2-2) a cooperative checkpoint (not done) drives ctx.checkpoint({reap:true}), recreates the epoch, and the next implement turn on the NEW root reaches done", async () => {
    // m4 change: a cooperative checkpoint reap recreates the provider epoch, so the second implement
    // turn runs on a DISTINCT root/transport (the reaped root's registry is permanently closed).
    const rig = makeMultiEpochRig([
      epochResponder("th-1", "tn-1", (t, th, tn) => {
        t.push(toolCall(1, "checkpoint", {}, th, tn, "c-ckpt")).push(turnCompleted("completed", th, tn));
      }),
      epochResponder("resumed-1", "tn-2", (t, th, tn) => {
        t.push(toolCall(2, "signal_done", {}, th, tn, "c-done")).push(turnCompleted("completed", th, tn));
      }),
    ]);
    const checkpoints: { reap: boolean }[] = [];
    // Pre-approved by default (makeCtx) → straight to the implement loop, no gate.
    const { ctx } = makeCtx({ checkpoint: async (opts) => { checkpoints.push({ reap: opts.reap }); } });
    const result = await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx), 5000, "checkpoint run");

    assert.equal(result.branch, "agent/issue-42");
    assert.equal(rig.providerLaunches(), 2, "the cooperative checkpoint recreated a fresh provider epoch");
    assert.equal(rig.epochs[1]!.transport.turnStartCount, 1, "the second implement turn ran on the NEW root");
    // Exactly one cooperative reap:true; the done turn breaks before any iteration-boundary fallback.
    assert.deepEqual(checkpoints, [{ reap: true }], "the cooperative checkpoint reaped (reap:true) exactly once");
  });

  it("(m2-3) a child-origin signal is DENIED (no fold, no root transition); only the ROOT signal_done resolves the run", async () => {
    const agents: AgentTemplate[] = [
      { name: "lead", description: "the lead", prompt_body: "lead body", tools: null, skills: [] },
      { name: "coder", description: "a coder", prompt_body: "coder body", tools: null, skills: [] },
    ];
    const CHILD_PLAN_ID = 55;
    const responder: Responder = (c) => {
      if (c.method === "thread/start") return { thread: { id: c.threadStartCount === 1 ? "th-1" : "th-child" } };
      if (c.method === "turn/start") {
        if (c.turnStartCount === 1) return { turn: { id: "tn-1" } };
        // The CHILD turn: a submit_plan (a CHILD-origin signal — must be DENIED, never folded).
        c.transport
          .push(toolCall(CHILD_PLAN_ID, "submit_plan", { plan_md: "child tries to plan" }, "th-child", "tn-child", "cc-plan"))
          .push(turnCompleted("completed", "th-child", "tn-child"));
        return { turn: { id: "tn-child" } };
      }
      if (c.method === "turn/interrupt") return {};
      return {};
    };
    const rig = makeRig({ responder });
    // Pre-approved: one implement turn. A root spawn_agent drives the child; only after the child
    // settles does the ROOT signal_done end the run (pushed once the parent reply lands).
    rig.transport.push(threadStarted()).push(toolCall(1, "spawn_agent", { subagent_type: "coder", prompt: "help" }, "th-1", "tn-1", "c-root"));
    const { ctx } = makeCtx({ agents });
    const runP = makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx);
    await waitFor(() => rig.transport.responses.some((r) => r.requestId === 1), "parent spawn_agent reply", 5000);
    rig.transport
      .push(toolCall(2, "signal_done", {}, "th-1", "tn-1", "c-done"))
      .push(turnCompleted("completed", "th-1", "tn-1"));
    const result = await withTimeout(runP, 5000, "child-signal-deny run");

    assert.equal(result.branch, "agent/issue-42");
    // The child submit_plan was DENIED (signal_root_only) — never folded as a root plan.
    const childReply = rec(rec(rig.transport.responses.find((r) => r.requestId === CHILD_PLAN_ID)?.response).result);
    assert.equal(childReply.success, false, "a child-origin submit_plan is denied, never folded into the run");
    assert.equal(rig.transport.turnStartCount, 2, "only the root turn + one child turn ran (no root transition from the child)");
  });

  it("(m2-4) the implement loop is BOUNDED: a never-done turn fails closed at the iteration budget", async () => {
    const responder: Responder = (c) => {
      if (c.method === "thread/start") return { thread: { id: "th-1" } };
      if (c.method === "turn/start") {
        const id = `tn-${c.turnStartCount}`;
        // Every implement turn completes but NEVER signals done/checkpoint.
        c.transport.push(turnCompleted("completed", "th-1", id));
        return { turn: { id } };
      }
      return {};
    };
    const rig = makeRig({ responder });
    rig.transport.push(threadStarted());
    const checkpoints: { reap: boolean }[] = [];
    // Pre-approved by default; a small budget so the bound trips deterministically.
    const { ctx } = makeCtx({ config: { max_iterations: 2 }, checkpoint: async (opts) => { checkpoints.push({ reap: opts.reap }); } });
    await assert.rejects(
      withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx), 5000, "bounded run"),
      /iteration budget without completing/,
    );
    // iteration 1: not done → fallback checkpoint (reap:false) → continue; iteration 2: not done
    // AND iteration >= max → throw BEFORE a second fallback. Exactly one reap:false checkpoint.
    assert.deepEqual(checkpoints, [{ reap: false }], "one iteration-boundary fallback checkpoint before the budget tripped");
  });
});

// ================================================================================
// PRD #1171 m4 — new-root resume + the runner-owned session persist/adopt lifecycle. A recreated
// provider root requires a BRAND-NEW ExecutionRegistry (a durability boundary permanently closes
// the prior one) and a fresh credential release, and it adopts the credential-free session subset
// from the shared store. These are fail-old/pass-fixed against an executor that reused the reaped
// root/registry (which fails the next turn at provider admission).
describe("CodexExecutor: new-root resume + session lifecycle (m4)", () => {
  it("(m4-1) a cooperative checkpoint reap:true reaps the provider then RECREATES a fresh epoch; the next implement turn on the NEW root reaches done → { branch }", async () => {
    // PASS-FIXED: ctx.checkpoint routes through the LIVE epoch's safety facade (exactly as the
    // runner's reapForSink does — it re-reads executor.safety fresh), reaping epoch 0's roots; the
    // executor then recreates a fresh epoch and the second implement turn drives the NEW root to done.
    const rig = makeMultiEpochRig([
      epochResponder("th-1", "tn-1", (t, th, tn) => {
        t.push(toolCall(1, "checkpoint", {}, th, tn, "c-ckpt")).push(turnCompleted("completed", th, tn));
      }),
      epochResponder("resumed-1", "tn-2", (t, th, tn) => {
        t.push(toolCall(2, "signal_done", {}, th, tn, "c-done")).push(turnCompleted("completed", th, tn));
      }),
    ]);
    let exec!: CodexExecutor;
    const { ctx } = makeCtx({
      checkpoint: async (opts) => {
        // Faithfully mirror the runner: reap the CURRENT epoch's roots through withBoundary.
        if (opts.reap) await exec.safety!.withBoundary({ boundary: "checkpoint", deadlineMs: 200 }, async () => {});
      },
    });
    exec = makeExecutor(rig, bindingOf(SUBSCRIPTION));
    const result = await withTimeout(exec.run(ctx), 5000, "m4-1 run");

    assert.equal(result.branch, "agent/issue-42", "the run resolved on the NEW root's signal_done");
    assert.equal(rig.providerLaunches(), 2, "the checkpoint reap recreated a fresh provider epoch");
    assert.ok(rig.epochs[0]!.reaped() >= 1, "epoch 0's provider root was reaped by the checkpoint boundary");
    assert.ok(rig.epochs[0]!.disposed() >= 1, "epoch 0 was fully disposed on recreation");
    assert.equal(rig.epochs[1]!.transport.turnStartCount, 1, "the next implement turn ran on the NEW root");
    assert.ok(rig.epochs[1]!.transport.requests.some((r) => r.method === "thread/resume"), "the new epoch resumed the prior session");
  });

  it("(m4-1 fail-old) a checkpoint withBoundary reap PERMANENTLY closes the registry, so reusing it for the next provider root is DENIED (recreation REQUIRES a fresh registry)", async () => {
    // The exact failure an executor that reused the reaped registry (instead of recreating) would
    // hit on its next turn: after a durability boundary the registry is "closed" forever and denies
    // reserveLaunch("provider") — so a fresh provider root can only come from a brand-new registry.
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(0));
    const { root } = trackedRoot();
    const reservation = registry.reserveLaunch("provider");
    assert.equal(reservation.kind, "reserved");
    if (reservation.kind === "reserved") assert.equal(registry.registerRoot(reservation.reservation, root).ok, true);
    const safety = createCodexExecutionSafety(registry, () => Promise.reject(new Error("no boundary root")));
    await safety.withBoundary({ boundary: "checkpoint", deadlineMs: 200 }, async () => {});
    assert.equal(registry.state(), "closed", "the boundary reap left the registry permanently closed");
    assert.equal(
      registry.reserveLaunch("provider").kind,
      "denied",
      "a fresh provider launch on the reaped registry is DENIED — the next turn REQUIRES a new registry (the m4 recreation)",
    );
  });

  it("(m4-2) plan approval recreates the epoch: a SECOND provider root is launched with a fresh release, the old epoch is disposed, and implement runs on the new epoch", async () => {
    const rig = makeMultiEpochRig([
      epochResponder("th-1", "tn-1", (t, th, tn) => {
        t.push(toolCall(1, "submit_plan", { plan_md: "the plan" }, th, tn, "c-plan")).push(turnCompleted("completed", th, tn));
      }),
      epochResponder("resumed-1", "tn-2", (t, th, tn) => {
        t.push(toolCall(2, "signal_done", {}, th, tn, "c-done")).push(turnCompleted("completed", th, tn));
      }),
    ]);
    const gatePlan: NonNullable<RunContext["gatePlan"]> = async () => ({ kind: "approve", selection: { source: "own", agents: [] } } as never);
    const { ctx } = makeCtx({ planApproved: false, approvedPlan: undefined, gatePlan });
    const result = await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx), 5000, "m4-2 run");

    assert.equal(result.branch, "agent/issue-42");
    assert.equal(rig.providerLaunches(), 2, "approval launched a SECOND provider root");
    assert.equal(rig.client.releaseCalls.length, 2, "each epoch freshly released the committed credential (a fresh releaseCodex per root)");
    assert.ok(rig.epochs[0]!.disposed() >= 1, "the old (plan) epoch's registry was disposed on recreation");
    assert.equal(rig.epochs[1]!.transport.turnStartCount, 1, "implement ran on the recreated epoch");
    assert.ok(rig.epochs[1]!.transport.requests.some((r) => r.method === "thread/resume"), "the implement epoch resumed the plan session");
  });

  it("(m4-3) persist runs BEFORE each reap and at terminal, a resume adopts the store, and the store is NEVER removed", async () => {
    const rig = makeMultiEpochRig([
      epochResponder("th-1", "tn-1", (t, th, tn) => {
        t.push(toolCall(1, "checkpoint", {}, th, tn, "c-ckpt")).push(turnCompleted("completed", th, tn));
      }),
      epochResponder("resumed-1", "tn-2", (t, th, tn) => {
        t.push(toolCall(2, "signal_done", {}, th, tn, "c-done")).push(turnCompleted("completed", th, tn));
      }),
    ]);
    const { ctx } = makeCtx({ checkpoint: async () => undefined });
    const result = await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx), 5000, "m4-3 run");
    assert.equal(result.branch, "agent/issue-42");
    // persist: once before the cooperative checkpoint reap (capturing the live session), once at
    // terminal (capturing the final session for a park/preserve resume).
    assert.equal(rig.sessionOps.persist, 2, "persist ran before the reap AND at terminal");
    // adopt: each epoch's provider launch seeds the fresh HOME from the shared store.
    assert.equal(rig.sessionOps.adopt, 2, "each recreated epoch adopted the credential-free store");
    assert.equal(rig.sessionOps.removeCalls, 0, "the executor NEVER removes the store (the runner's runHome lifecycle owns removal)");
  });

  it("(m4-4) a reap:false iteration-boundary checkpoint does NOT recreate the epoch — the SAME provider root survives and drives the next turn", async () => {
    const responder: Responder = (c) => {
      if (c.method === "thread/start") return { thread: { id: "th-1" } };
      if (c.method === "turn/start") {
        if (c.turnStartCount === 1) {
          // turn 1: complete with NO signal (not done, not checkpoint) → iteration-boundary reap:false.
          c.transport.push(turnCompleted("completed", "th-1", "tn-1"));
          return { turn: { id: "tn-1" } };
        }
        // turn 2: a root signal_done → done.
        c.transport.push(toolCall(2, "signal_done", {}, "th-1", "tn-2", "c-done")).push(turnCompleted("completed", "th-1", "tn-2"));
        return { turn: { id: "tn-2" } };
      }
      return {};
    };
    const rig = makeRig({ responder });
    rig.transport.push(threadStarted());
    const checkpoints: { reap: boolean }[] = [];
    const { ctx } = makeCtx({ checkpoint: async (opts) => { checkpoints.push({ reap: opts.reap }); } });
    const result = await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx), 5000, "m4-4 run");
    assert.equal(result.branch, "agent/issue-42");
    assert.equal(rig.providerLaunches(), 1, "a reap:false checkpoint did NOT recreate the provider epoch");
    assert.equal(rig.transport.turnStartCount, 2, "both turns ran on the SAME (surviving) provider root");
    assert.deepEqual(checkpoints, [{ reap: false }], "exactly one iteration-boundary reap:false checkpoint");
    assert.equal(rig.client.releaseCalls.length, 1, "only one credential release (no recreation)");
  });

  it("(m4-5) each recreated root freshly releases the committed credential AND the shared committed-generation cell carries across epochs", async () => {
    // Three epochs: epoch 0 and epoch 1 each checkpoint (their boundary reconcile refreshes and
    // advances the SHARED generation cell), epoch 2 finishes. Because the cell is shared, epoch 1's
    // boundary sends the generation epoch 0's boundary advanced it to.
    const rig = makeMultiEpochRig([
      epochResponder("th-1", "tn-1", (t, th, tn) => {
        t.push(toolCall(1, "checkpoint", {}, th, tn, "c-ckpt-0")).push(turnCompleted("completed", th, tn));
      }),
      epochResponder("resumed-1", "tn-2", (t, th, tn) => {
        t.push(toolCall(2, "checkpoint", {}, th, tn, "c-ckpt-1")).push(turnCompleted("completed", th, tn));
      }),
      epochResponder("resumed-2", "tn-3", (t, th, tn) => {
        t.push(toolCall(3, "signal_done", {}, th, tn, "c-done")).push(turnCompleted("completed", th, tn));
      }),
    ]);
    let exec!: CodexExecutor;
    const { ctx } = makeCtx({
      checkpoint: async (opts) => {
        if (opts.reap) await exec.safety!.withBoundary({ boundary: "checkpoint", deadlineMs: 200 }, async () => {});
      },
    });
    exec = makeExecutor(rig, bindingOf(SUBSCRIPTION));
    const result = await withTimeout(exec.run(ctx), 5000, "m4-5 run");

    assert.equal(result.branch, "agent/issue-42");
    assert.equal(rig.providerLaunches(), 3, "three provider epochs launched");
    assert.equal(rig.client.releaseCalls.length, 3, "each recreated root freshly releases the committed credential");
    // The subscription binding seeds the SHARED cell at generation 3. epoch 0's boundary reconcile
    // sends observed_generation 3 and the fake advances it to 4; epoch 1's boundary — on a FRESH
    // epoch/safety facade — must send 4, proving the cell carried across the recreation.
    assert.equal(rig.client.refreshCalls.length, 2, "one boundary reconcile per checkpointing epoch");
    assert.equal(rig.client.refreshCalls[0]!.observed_generation, 3, "epoch 0's boundary reconciled from the seeded generation");
    assert.equal(rig.client.refreshCalls[1]!.observed_generation, 4, "epoch 1's boundary reconciled from the generation epoch 0 advanced (SHARED cell carried)");
  });

  it("(m4-6) the old epoch's registry is fully disposed on recreation — no leaked undisposed registry (provider AND fileop roots reaped + disposed)", async () => {
    const rig = makeMultiEpochRig([
      epochResponder("th-1", "tn-1", (t, th, tn) => {
        t.push(toolCall(1, "checkpoint", {}, th, tn, "c-ckpt")).push(turnCompleted("completed", th, tn));
      }),
      epochResponder("resumed-1", "tn-2", (t, th, tn) => {
        t.push(toolCall(2, "signal_done", {}, th, tn, "c-done")).push(turnCompleted("completed", th, tn));
      }),
    ]);
    const { ctx } = makeCtx({ checkpoint: async () => undefined });
    await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx), 5000, "m4-6 run");
    // The OLD epoch (epoch 0) was reaped AND disposed on recreation — its registry's disposeTools
    // ran (the provider root's dispose fired) rather than leaving a live, undisposed registry.
    assert.ok(rig.epochs[0]!.reaped() >= 1, "the old epoch's provider root was reaped");
    assert.ok(rig.epochs[0]!.disposed() >= 1, "the old epoch's provider root was disposed (registry.disposeTools ran)");
    // Both epochs' fileop command roots are disposed by the end (epoch 0 at recreation, epoch 1 at
    // terminal), so no epoch leaks an undisposed effect root either.
    assert.ok(rig.effectDisposes() >= 2, "both epochs' fileop command roots were disposed");
  });
});

// ================================================================================
// PRD #1171 M3 (item 6) — the credential-free command lane env: the fixed
// /opt/uzi-toolchain/bin + system dirs come FIRST, the run's allowlisted provisioned
// toolEnv is folded in (PATH appended, nix vars folded), and nothing breaches the
// boundary literals or inherits process.env.
describe("CodexExecutor: credential-free command env (item 6)", () => {
  const FIXED_PATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/opt/uzi-toolchain/bin";

  it("puts the fixed toolchain+system dirs FIRST and APPENDS the provisioned PATH (fail-old/pass-fixed on /opt/uzi-toolchain/bin)", () => {
    const env = buildCommandEnv("/private/tmp", {
      PATH: "/provisioned/tool/bin",
      NIX_SSL_CERT_FILE: "/nix/cacert",
      LOCALE_ARCHIVE: "/nix/locale-archive",
    });
    // fail-old: the retired "/usr/bin:/bin" literal lacked the toolchain dir.
    assert.ok(env.PATH!.includes("/opt/uzi-toolchain/bin"), "the fixed toolchain dir is on the command PATH");
    assert.ok(env.PATH!.startsWith(FIXED_PATH), "the fixed boundary dirs lead the PATH");
    assert.ok(
      env.PATH!.indexOf("/opt/uzi-toolchain/bin") < env.PATH!.indexOf("/provisioned/tool/bin"),
      "fixed dirs precede the appended provisioned PATH",
    );
    // The other allowlisted vars are folded; boundary literals are fixed.
    assert.equal(env.NIX_SSL_CERT_FILE, "/nix/cacert");
    assert.equal(env.LOCALE_ARCHIVE, "/nix/locale-archive");
    assert.equal(env.LANG, "C");
    assert.equal(env.TMPDIR, "/private/tmp");
  });

  it("with no provisioned PATH the fixed boundary PATH stands alone", () => {
    assert.equal(buildCommandEnv("/private/tmp", {}).PATH, FIXED_PATH);
  });

  it("a hostile toolEnv can never overwrite PATH's fixed prefix, TMPDIR, LANG, HOME, or a PROTECTED_ENV_KEYS member", () => {
    const env = buildCommandEnv("/private/tmp", {
      PATH: "/evil/bin", // only ever APPENDED after the fixed dirs, never a replacement
      TMPDIR: "/evil/tmp",
      LANG: "evil",
      HOME: "/evil/home",
      CLAUDE_CODE_OAUTH_TOKEN: "leaked-oauth-token",
      ANTHROPIC_API_KEY: "leaked-anthropic-key",
      ANTHROPIC_AUTH_TOKEN: "leaked-anthropic-auth",
      AGENT_BROWSER_ARGS: "--evil",
    } as Record<string, string>);
    assert.ok(env.PATH!.startsWith(FIXED_PATH), "the fixed prefix survives a hostile provisioned PATH");
    assert.ok(env.PATH!.endsWith("/evil/bin"), "a provisioned PATH is appended, never a replacement");
    assert.equal(env.TMPDIR, "/private/tmp", "TMPDIR stays the boundary value");
    assert.equal(env.LANG, "C", "LANG stays C");
    assert.equal(env.HOME, undefined, "HOME is never taken from toolEnv (commandEffectSpec pins it to the private tmp)");
    assert.equal(env.CLAUDE_CODE_OAUTH_TOKEN, undefined, "the OAuth credential can never be folded in");
    assert.equal(env.ANTHROPIC_API_KEY, undefined);
    assert.equal(env.ANTHROPIC_AUTH_TOKEN, undefined);
    assert.equal(env.AGENT_BROWSER_ARGS, undefined);
  });

  it("run() folds the injected provisionRunTools toolEnv into the command identity and never inherits process.env", async () => {
    const rig = makeRig();
    rig.deps = {
      ...rig.deps,
      provisionRunTools: async () => ({
        toolEnv: { PATH: "/provisioned/tool/bin", NIX_SSL_CERT_FILE: "/nix/cacert", LOCALE_ARCHIVE: "/nix/locale-archive" },
      }),
    };
    rig.transport
      .push(threadStarted())
      .push(toolCall(1, "Bash", { command: "echo hi" }, "th-1", "tn-1", "c1"))
      .push(signalDone())
      .push(turnCompleted("completed"))
      .end();
    const { ctx } = makeCtx();
    process.env.UZI_CODEX_COMMANDENV_CANARY = "must-not-appear-in-command-env";
    try {
      await withTimeout(makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx), 3000, "command-env run");
    } finally {
      delete process.env.UZI_CODEX_COMMANDENV_CANARY;
    }
    // The fileop root is launched once under the command env (commandEffectSpec overrides
    // HOME/TMPDIR to a private tmp, but PATH/LANG/nix vars come straight from commandEnv).
    const env = rig.fileopSpawns[0]!.env;
    assert.ok(String(env.PATH).includes("/opt/uzi-toolchain/bin"), "the fixed toolchain dir is on the command PATH");
    assert.ok(String(env.PATH).includes("/provisioned/tool/bin"), "the provisioned toolEnv PATH is appended");
    assert.ok(
      String(env.PATH).indexOf("/opt/uzi-toolchain/bin") < String(env.PATH).indexOf("/provisioned/tool/bin"),
      "fixed dirs precede the provisioned PATH",
    );
    assert.equal(env.NIX_SSL_CERT_FILE, "/nix/cacert", "an allowlisted nix var is folded into the command env");
    assert.equal(env.LOCALE_ARCHIVE, "/nix/locale-archive");
    assert.equal(env.UZI_CODEX_COMMANDENV_CANARY, undefined, "process.env is not inherited into the credential-free command identity");
  });
});

// ================================================================================
// PRD #1171 M3 (item 5) — the production tool-handler map + its inheritance into a
// delegated child broker.
describe("CodexExecutor: production tool-handler map (item 5)", () => {
  const NOOP: Logger = { debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {}, child() { return NOOP; } };

  it("buildCodexToolHandlers keys the map by the qualified callback names (forge x8, memory, findings) plus the CANONICAL Skill ONLY (no per-skill mcp__skills__<name>)", () => {
    const map = buildCodexToolHandlers({
      client: {} as never,
      runId: "run-x",
      log: NOOP,
      emit: () => undefined,
      skills: [{ name: "prd-lifecycle", description: "d", body: "b" }],
    });
    for (const name of forgeToolNames()) assert.equal(map.has(name), true, `forge ${name} keyed`);
    assert.equal(map.has(memoryToolNames()[0]!), true, "memory keyed");
    assert.equal(map.has(reportIncidentalIssueToolName()), true, "findings keyed");
    assert.equal(map.has("Skill"), true, "canonical Skill keyed (reads args.skill; the broker gates allowedSkills)");
    // m3-review nit: the per-skill mcp__skills__<name> keys are unreachable under render grants
    // (which expose only the canonical `Skill` tool) and their key/args mismatch a latent
    // inconsistency, so they are DELETED — only `Skill` remains.
    assert.equal(map.has("mcp__skills__prd-lifecycle"), false, "per-granted-skill mcp__skills__<name> is NOT keyed");
  });

  it("a granted Skill(skill=...) still returns the skill body through the canonical handler", async () => {
    const map = buildCodexToolHandlers({
      client: {} as never,
      runId: "run-x",
      log: NOOP,
      emit: () => undefined,
      skills: [{ name: "prd-lifecycle", description: "the prd lifecycle", body: "PRD BODY TEXT" }],
    });
    const handler = map.get("Skill");
    assert.ok(handler, "the canonical Skill handler is present");
    const result = await handler!({ skill: "prd-lifecycle" });
    assert.match(JSON.stringify(result), /PRD BODY TEXT/, "the canonical Skill handler returns the granted skill body");
  });

  it("(item 5) a forge callback in a DELEGATED child with an explicit forge allowlist reaches the shared handler (the runner forwards the map)", async () => {
    const FORGE_GET_ISSUE = forgeToolNames()[0]!; // mcp__forge__get_issue
    const agents: AgentTemplate[] = [
      { name: "lead", description: "the lead", prompt_body: "lead body", tools: null, skills: [] },
      // The child holds an EXPLICIT forge allowlist (render keeps a recognized mcp__forge__* on a subagent).
      { name: "coder", description: "a coder", prompt_body: "coder body", tools: [FORGE_GET_ISSUE, "Read"], skills: [] },
    ];
    const responder: Responder = (c) => {
      if (c.method === "thread/start") return { thread: { id: c.threadStartCount === 1 ? "th-1" : "th-child" } };
      if (c.method === "turn/start") {
        if (c.turnStartCount === 1) return { turn: { id: "tn-1" } };
        c.transport
          .push(toolCall(21, FORGE_GET_ISSUE, { iid: 7 }, "th-child", "tn-child", "cc-forge"))
          .push(turnCompleted("completed", "th-child", "tn-child"));
        return { turn: { id: "tn-child" } };
      }
      return {};
    };
    const rig = makeRig({ responder });
    // Extend the run's client with the forge read the handler calls; record the iid it saw.
    const forgeCalls: number[] = [];
    (rig.client as unknown as { getForgeIssue: (runId: string, iid: number) => Promise<unknown> }).getForgeIssue =
      async (_runId, iid) => { forgeCalls.push(iid); return { iid, title: "FORGE_CHILD_CANARY" }; };
    rig.transport.push(threadStarted()).push(toolCall(1, "spawn_agent", { role: "coder", prompt: "help" }, "th-1", "tn-1", "c-root"));

    const { ctx } = makeCtx({ agents });
    const runP = makeExecutor(rig, bindingOf(SUBSCRIPTION)).run(ctx);
    await waitFor(() => rig.transport.responses.some((r) => r.requestId === 1), "parent spawn_agent reply");
    rig.transport.push(signalDone("th-1", "tn-1")).push(turnCompleted("completed", "th-1", "tn-1")).end();
    await withTimeout(runP, 3000, "child-forge run");

    const replyOf = (id: number): { success?: boolean } => rec(rec(rig.transport.responses.find((r) => r.requestId === id)?.response).result) as { success?: boolean };
    assert.equal(replyOf(21).success, true, "the child forge callback reached its handler (NOT denied_tool)");
    assert.deepEqual(forgeCalls, [7], "the shared forge handler ran with the child-supplied iid");
  });
});
