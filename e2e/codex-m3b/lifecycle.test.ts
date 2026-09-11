// PRD #1171 m5 — the packaged DARK CodexExecutor LIFECYCLE proof.
//
// This suite drives the REAL, PACKAGED `CodexExecutor` (loaded from the image-baked /app/src
// in the container, or the source tree host-side — see packaged-modules.ts) through the run
// lifecycle and proves the credential-isolation boundary end to end:
//
//   plan→approval / new-root resume → implement → synchronous subagent → root-only signals
//   → checkpoint → cancel → finalization,
//
// asserting clean/poison outcomes AND that the INJECTED credential/capability CANARIES never
// reach a PUBLIC message, a PROVIDER-VISIBLE request, a LOG line, or the COMMAND-ROOT state.
//
// Two legs, one file:
//
//   • BLOCK A (ALWAYS runs — host `node --test` AND in-image): drives the packaged executor
//     with an INJECTED fake WorkerClient + an IN-MEMORY fake provider transport (scripted
//     app-server frames — NO real Codex process). This is the in-worker-validated security
//     proof, and when run in-image (CODEX_M3B_SRC=/app/src) it is the packaged unit-composition
//     proof over the baked adapter. Every external effect is an injected seam.
//
//   • BLOCK B (image only — gated on CODEX_M3B_PACKAGED=1, set by run-lifecycle.sh): drives the
//     packaged executor through the REAL launcher → real static supervisor → real Codex →
//     loopback fake provider (fake-provider.ts) under `--network none`, proving the baked
//     supervisor + fileop binaries and the launch wiring, and re-asserting the canary boundary
//     against the REAL process surface. VERIFIED 2026-09-10 in both native AMD64 worker
//     images on a Landlock-capable OKD cluster; it remains SKIPPED host-side.

import { before, describe, it } from "node:test";
import assert from "node:assert/strict";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import nodePath from "node:path";
import { PassThrough } from "node:stream";

import {
  loadPackagedCodexExecutor,
  loadPackagedSessionState,
  loadPackagedSelect,
  loadPackagedLauncher,
  loadPackagedTransport,
  type CodexExecutorModule,
  type LauncherModule,
  type SessionStateModule,
  type SelectModule,
  type TransportModule,
} from "./packaged-modules.js";
import { bearerDigest, codexCanaries, type CodexCanaries } from "./fake-provider.js";

import type { CodexExecutorDeps } from "../../agent/src/codex/codex-executor.js";
import type { CodexBinding } from "../../agent/src/codex/select.js";
import type { CodexLaunchRootResult, CodexLaunchRootSpec, CodexProviderConfig } from "../../agent/src/codex/codex-harness.js";
import type { RegisteredRoot } from "../../agent/src/codex/registry.js";
import type { FileopHelperHandle } from "../../agent/src/codex/fileop-client.js";
import type { CodexNotification, CodexTransport } from "../../agent/src/codex/transport.js";
import type { RunContext, EmittedMessage, Executor } from "../../agent/src/executor.js";
import type { Logger } from "../../agent/src/log.js";
import type { AgentTemplate, ClaimCodexSecrets } from "../../agent/src/protocol.js";
import type { CodexEffectLaunchSpec, CodexRootHandle } from "../../agent/src/codex/launcher.js";
import type { CodexAppServerAuthMode } from "../../agent/src/codex/appserver-auth.js";
import type { LaunchAdviceRootSeam } from "../../agent/src/codex/codex-advice-harness.js";
import type { AdviceRequest, AdviceResultPolicy, HarnessTerminal } from "../../agent/src/harness.js";

// ─── the PACKAGED adapter, loaded once before any test (image /app/src, or the host
// source tree). A top-level `before` (not top-level await — the CommonJS-typed e2e tree
// forbids it) resolves them before the first `it` body runs. ────────────────────
let CodexExecutor: CodexExecutorModule["CodexExecutor"];
let FailClosedExecutor: CodexExecutorModule["FailClosedExecutor"];
let CodexAdviceCredentialBridge: CodexExecutorModule["CodexAdviceCredentialBridge"];
let makeCodexAdviceHarness: CodexExecutorModule["makeCodexAdviceHarness"];
let registeredRoot: CodexExecutorModule["registeredRoot"];
let selectCodexBinding: SelectModule["selectCodexBinding"];
let CodexSelectionError: SelectModule["CodexSelectionError"];
let CodexSessionStore: SessionStateModule["CodexSessionStore"];
let launchCodexRoot: LauncherModule["launchCodexRoot"];
let launchCodexEffectRoot: LauncherModule["launchCodexEffectRoot"];
let CODEX_BIN: LauncherModule["CODEX_BIN"];
let SUPERVISOR_BIN: LauncherModule["SUPERVISOR_BIN"];
let PROVIDER_CHILD_ARGV: LauncherModule["PROVIDER_CHILD_ARGV"];
let createCodexTransport: TransportModule["createCodexTransport"];

before(async () => {
  const exec = await loadPackagedCodexExecutor();
  CodexExecutor = exec.CodexExecutor;
  FailClosedExecutor = exec.FailClosedExecutor;
  CodexAdviceCredentialBridge = exec.CodexAdviceCredentialBridge;
  makeCodexAdviceHarness = exec.makeCodexAdviceHarness;
  registeredRoot = exec.registeredRoot;
  const sel = await loadPackagedSelect();
  selectCodexBinding = sel.selectCodexBinding;
  CodexSelectionError = sel.CodexSelectionError;
  CodexSessionStore = (await loadPackagedSessionState()).CodexSessionStore;
  const launcher = await loadPackagedLauncher();
  launchCodexRoot = launcher.launchCodexRoot;
  launchCodexEffectRoot = launcher.launchCodexEffectRoot;
  CODEX_BIN = launcher.CODEX_BIN;
  SUPERVISOR_BIN = launcher.SUPERVISOR_BIN;
  PROVIDER_CHILD_ARGV = launcher.PROVIDER_CHILD_ARGV;
  createCodexTransport = (await loadPackagedTransport()).createCodexTransport;
});

const WORKSPACE = "/work/repo";

// The provider config the executor carries. For BLOCK A the base URL is inert (the fake
// transport never issues a real POST); BLOCK B overrides it with the loopback fake's URL.
const provider: CodexProviderConfig = {
  name: "openai",
  baseUrl: "http://127.0.0.1:9/v1",
  envKey: "OPENAI_API_KEY",
  model: "gpt-6-astra",
};

// ─── a RECORDING logger mirroring src/log.ts's ref-counted secret registry ──────
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

// ─── a scriptable in-memory transport (the in-process fake provider) ────────────
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
  /** The immutable auth MODE the launch seam now receives (the credential rides the login RPC,
   *  NOT the launcher). Stashed so a test can prove the seam got the mode, never a token. */
  launchAuthMode?: string;
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
    // The pinned app-server auth handshake (createCodexAppServerAuth → authenticate) drives
    // initialize + account/login/start on EVERY provider root BEFORE any thread/turn work.
    // Answer them here (mirroring the migrated agent FakeTransport) so every responder/override
    // stays free of the auth plumbing. `initialize` returns the exact shape appserver-auth
    // requires (userAgent/codexHome/platformFamily/platformOs); `account/login/start` echoes the
    // login `type` (apiKey | chatgptAuthTokens). The CREDENTIAL rides in the login params — the
    // intended delivery, not a leak — so surfacesOf() excludes this lane from providerRequests.
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
    // The pinned auth session installs a refresh pump through this. Block A never pushes a
    // subscription-refresh server request, so storing the handler (a no-op) is sufficient; it
    // satisfies the harness's fail-closed requirement that the interceptor be present.
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

// ─── notification builders (the M0 app-server wire vocabulary) ──────────────────
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
/** A root `signal_done` tool call on the active (thread, turn). The m2 implement/review loop
 *  finishes ONLY on a folded `done`, so every resolving Block A run must signal_done on its ROOT
 *  turn — exactly as a real implement turn declares the work finished. The broker each run builds
 *  carries the lead's root grants (isRoot + signal tools), so it accepts this and the harness
 *  surfaces its `{done:true}` on the main frame the run-lane reducer folds. */
function signalDone(threadId = "th-1", turnId = "tn-1", requestId = 700, callId = "c-done"): CodexNotification {
  return toolCall(requestId, "signal_done", {}, threadId, turnId, callId);
}

// ─── binding + client + context builders ────────────────────────────────────────
function bindingOf(codex: Record<string, unknown>): CodexBinding {
  const selection = selectCodexBinding({ codex });
  if (selection.kind !== "codex") throw new Error("expected a codex selection");
  return selection.binding;
}

interface FakeClient {
  releaseCalls: { runId: string; capability: string }[];
  refreshCalls: { runId: string; operation_id: string; observed_generation: number }[];
  releaseCodex(runId: string, req: { capability: string }): Promise<{ access_token: string }>;
  refreshCodex(runId: string, req: { capability: string; operation_id: string; observed_generation: number }): Promise<{ access_token: string; generation: number; outcome: string }>;
}
/** The fake WorkerClient: every release/refresh hands back the CREDENTIAL canary (or a
 *  derived refresh token), so the isolation boundary is proven against the exact injected
 *  secret. */
function fakeClient(credential: string): FakeClient {
  const releaseCalls: FakeClient["releaseCalls"] = [];
  const refreshCalls: FakeClient["refreshCalls"] = [];
  let gen = 4;
  return {
    releaseCalls,
    refreshCalls,
    async releaseCodex(runId, req) {
      releaseCalls.push({ runId, capability: req.capability });
      return { access_token: credential };
    },
    async refreshCodex(runId, req) {
      refreshCalls.push({ runId, operation_id: req.operation_id, observed_generation: req.observed_generation });
      return { access_token: `${credential}-refreshed`, generation: gen++, outcome: "advanced" };
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
    ...overrides,
  };
  return { ctx, emitted };
}

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

/** One in-memory supervised effect-root handle (the command/fileop lane). Shared verbatim by the
 *  single- and multi-epoch rigs so their fileop/command surface is byte-identical. */
function fakeEffectHandle(): CodexRootHandle {
  const stdin = new PassThrough();
  const stdout = new PassThrough();
  const stderr = new PassThrough();
  return {
    started: { event: "started", supervisorPid: 200, childPid: 201, subreaper: true, nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true },
    supervisorPid: 200,
    transport: { stdin, stdout, stderr },
    snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
    waitChild: async () => ({ event: "child_exit", code: 0 }),
    dispose: async () => ({ clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } }),
    failed: undefined,
    whenFailed: new Promise<Error>(() => undefined),
  };
}

interface Rig {
  transport: FakeTransport;
  /** Every provider epoch's transport (one for a single-epoch rig). surfacesOf aggregates these. */
  transports: FakeTransport[];
  client: FakeClient;
  root: RegisteredRoot;
  reaped: () => number;
  disposed: () => number;
  fileopDisposed: () => number;
  spawnCommandCalls: { argv: readonly string[]; opts: { cwd?: string; env?: NodeJS.ProcessEnv } }[];
  fileopSpawns: { worktreePath: string; env: NodeJS.ProcessEnv }[];
  sessionOps: { adopt: number; removeCalls: number; inspect: number };
  deps: CodexExecutorDeps;
}

function makeRig(canaries: CodexCanaries, opts: { responder?: Responder } = {}): Rig {
  const transport = new FakeTransport(opts.responder ?? defaultResponder);
  const client = fakeClient(canaries.credential);
  const { root, reaped, disposed } = trackedRoot();
  const fh = fakeFileopHandle();
  const spawnCommandCalls: Rig["spawnCommandCalls"] = [];
  const fileopSpawns: Rig["fileopSpawns"] = [];
  const sessionOps = { adopt: 0, removeCalls: 0, inspect: 0 };
  const deps: CodexExecutorDeps = {
    launchProviderRoot: async (_spec, authMode): Promise<CodexLaunchRootResult> => {
      // Under app-server auth the launch seam receives the immutable auth MODE, not a credential —
      // the token flows over the account/login/start RPC (answered by FakeTransport.request). Stash
      // the mode so a test proves the seam got the mode, never a token.
      transport.launchAuthMode = authMode;
      return { root, transport, supervisorPid: 1234 };
    },
    spawnCommand: async (argv, cmdOpts) => {
      spawnCommandCalls.push({ argv, opts: cmdOpts });
      return { code: 0, stdout: "ok", stderr: "" };
    },
    launchEffectRoot: async (spec: CodexEffectLaunchSpec): Promise<CodexRootHandle> => {
      fileopSpawns.push({ worktreePath: "/work/repo", env: spec.env });
      return fakeEffectHandle();
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
      // m4 runner-owned session lifecycle: the executor persists the credential-free subset before
      // every reap/recreation and at the terminal. Best-effort in the real store; a no-op here.
      persist: async () => ({ files: 0, bytes: 0 }),
    },
    idleMs: 5000,
    wallMs: 5000,
    boundaryDeadlineMs: 200,
    childTurnDeadlineMs: 5000,
    commandTmpdir: "/run/runner-tmp",
  };
  return {
    transport,
    transports: [transport],
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

// ─── a MULTI-EPOCH rig (m4 new-root resume) ─────────────────────────────────────
// m4 makes plan approval AND every cooperative-checkpoint reap RECREATE the provider epoch: a
// fresh registry + harness + transport. A recreated harness consumes a FRESH transport's
// single-consumer notifications(), so each epoch needs its OWN FakeTransport + tracked root.
// `launchProviderRoot` hands out the next epoch's transport/root in launch order; each epoch's
// responder scripts that epoch's turn frames independently. The command/fileop surface + session
// ops + fake client are SHARED (as in the real executor), so surfacesOf still sees one command
// state and the canary boundary is asserted across every epoch's transport.
interface EpochFake {
  transport: FakeTransport;
  root: RegisteredRoot;
  reaped: () => number;
  disposed: () => number;
}
interface MultiRig {
  epochs: EpochFake[];
  transports: FakeTransport[];
  client: FakeClient;
  spawnCommandCalls: Rig["spawnCommandCalls"];
  fileopSpawns: Rig["fileopSpawns"];
  sessionOps: { adopt: number; removeCalls: number; inspect: number };
  providerLaunches: () => number;
  deps: CodexExecutorDeps;
}
function makeMultiEpochRig(canaries: CodexCanaries, responders: Responder[]): MultiRig {
  const client = fakeClient(canaries.credential);
  const epochs: EpochFake[] = responders.map((responder) => {
    const t = trackedRoot();
    return { transport: new FakeTransport(responder), root: t.root, reaped: t.reaped, disposed: t.disposed };
  });
  const fh = fakeFileopHandle();
  const spawnCommandCalls: Rig["spawnCommandCalls"] = [];
  const fileopSpawns: Rig["fileopSpawns"] = [];
  const sessionOps = { adopt: 0, removeCalls: 0, inspect: 0 };
  let providerLaunches = 0;
  const deps: CodexExecutorDeps = {
    launchProviderRoot: async (_spec, authMode): Promise<CodexLaunchRootResult> => {
      const epoch = epochs[providerLaunches];
      providerLaunches += 1;
      if (!epoch) throw new Error(`no scripted epoch for provider launch #${providerLaunches}`);
      epoch.transport.launchAuthMode = authMode;
      return { root: epoch.root, transport: epoch.transport, supervisorPid: 1000 + providerLaunches };
    },
    spawnCommand: async (argv, cmdOpts) => {
      spawnCommandCalls.push({ argv, opts: cmdOpts });
      return { code: 0, stdout: "ok", stderr: "" };
    },
    launchEffectRoot: async (spec: CodexEffectLaunchSpec): Promise<CodexRootHandle> => {
      fileopSpawns.push({ worktreePath: "/work/repo", env: spec.env });
      return fakeEffectHandle();
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
      // m4 runner-owned session lifecycle: the executor persists the credential-free subset before
      // every reap/recreation and at the terminal. Best-effort in the real store; a no-op here.
      persist: async () => ({ files: 0, bytes: 0 }),
    },
    idleMs: 5000,
    wallMs: 5000,
    boundaryDeadlineMs: 200,
    childTurnDeadlineMs: 5000,
    commandTmpdir: "/run/runner-tmp",
  };
  return {
    epochs,
    transports: epochs.map((e) => e.transport),
    client,
    spawnCommandCalls,
    fileopSpawns,
    sessionOps,
    providerLaunches: () => providerLaunches,
    deps,
  };
}

// Build a per-epoch responder for a MULTI-epoch rig. Each recreated epoch gets its OWN
// FakeTransport, so its counters restart (turnStartCount === 1 within the epoch). thread/start AND
// thread/resume both return this epoch's thread id (epoch 0 starts fresh; a recreated epoch resumes
// the prior thread on its fresh transport). turn/start pushes this epoch's threadStarted + turn
// frames, then returns the turn id (the harness consumes the queued frames after the RPC).
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

// Default responder: root ids th-1 / tn-1, resume id resumed-1, first child th-child / tn-child.
function defaultResponder(c: ResponderCtx): unknown {
  if (c.method === "thread/start") return { thread: { id: c.threadStartCount === 1 ? "th-1" : "th-child" } };
  if (c.method === "thread/resume") return { thread: { id: "resumed-1" } };
  if (c.method === "turn/start") return { turn: { id: c.turnStartCount === 1 ? "tn-1" : "tn-child" } };
  return {};
}

function makeExecutor(
  rig: { client: FakeClient; deps: CodexExecutorDeps },
  binding: CodexBinding,
  log: Logger,
): InstanceType<typeof CodexExecutor> {
  // The fake WorkerClient implements only releaseCodex/refreshCodex (the credential-bridge seam);
  // the executor also feeds `opts.client` to buildCodexToolHandlers, which needs the full
  // WorkerClient (saveMemory/forge/findings) — so the cast is unavoidable here (see the m5-TS
  // report: CodexExecutorOptions.client cannot be narrowed to a release/refresh Pick).
  return new CodexExecutor(
    log,
    "/data/agent-home/run-1",
    { binding, client: rig.client as never, provider },
    rig.deps,
  );
}

// ─── the four SURFACES the credential/capability canaries must never touch ──────
interface Surfaces {
  emitted: string;
  providerRequests: string;
  logLines: string;
  commandState: string;
  /** The `account/login/start` requests, kept SEPARATE from providerRequests: the credential
   *  legitimately rides this lane (the intended token delivery), so the credential canary is
   *  allowed here — but the CAPABILITY canary must never appear here, and it is still scanned. */
  authLoginRequests: string;
}
/** ONLY the `account/login/start` RPC delivers the credential, so ONLY it is split off from the
 *  provider-visible request surface (the credential in the login params is the intended delivery,
 *  not a leak; codex-executor.ts drops it from the launcher env). `initialize` carries only static
 *  clientInfo — no secret — so it stays in providerRequests and IS scanned for both canaries. Every
 *  other request (thread/turn start, child requests) must stay canary-free. The login lane is still
 *  scanned for the capability canary (which has no business riding any request). */
const AUTH_LANE_METHODS: ReadonlySet<string> = new Set(["account/login/start"]);

/** The subset of a rig surfacesOf reads: every provider epoch's transport plus the shared command
 *  state. Both {@link Rig} and {@link MultiRig} satisfy it. */
interface SurfaceSource {
  transports: FakeTransport[];
  spawnCommandCalls: Rig["spawnCommandCalls"];
  fileopSpawns: Rig["fileopSpawns"];
}
function surfacesOf(source: SurfaceSource, emitted: EmittedMessage[], log: RecordingLog): Surfaces {
  return {
    emitted: JSON.stringify(emitted),
    // The provider-visible prompts/requests are the params the harness put on the transport
    // (initialize + thread/turn start + every child request), aggregated across EVERY provider
    // epoch's transport. ONLY the account/login/start lane is split off (see authLoginRequests),
    // so the credential AND capability must NOT appear in anything left here (initialize included).
    providerRequests: JSON.stringify(
      source.transports.flatMap((t) => t.requests.filter((r) => !AUTH_LANE_METHODS.has(r.method))),
    ),
    logLines: log.lines.join("\n"),
    // The command-root state: the argv + cwd of every command spawn AND the scrubbed env of
    // every command/fileop spawn.
    commandState: JSON.stringify({ commands: source.spawnCommandCalls, fileop: source.fileopSpawns }),
    // The login lane, kept separate: the credential legitimately rides it, but the capability
    // must not, so it is scanned for the capability canary only (below).
    authLoginRequests: JSON.stringify(
      source.transports.flatMap((t) => t.requests.filter((r) => AUTH_LANE_METHODS.has(r.method))),
    ),
  };
}
/** Assert the two INJECTED secrets (the fresh provider credential + the claim capability) are
 *  absent from every public/provider/log/command surface. The credential is ALLOWED only on the
 *  account/login/start lane (its intended delivery); the capability is allowed on NO surface. */
function assertCanaryBoundary(surfaces: Surfaces, canaries: CodexCanaries): void {
  const { authLoginRequests, ...bothScanned } = surfaces;
  for (const [name, blob] of Object.entries(bothScanned)) {
    assert.doesNotMatch(blob, new RegExp(escapeRe(canaries.credential)), `credential canary leaked into ${name}`);
    assert.doesNotMatch(blob, new RegExp(escapeRe(canaries.capability)), `capability canary leaked into ${name}`);
  }
  // The credential legitimately rides account/login/start; the CAPABILITY must never appear there.
  assert.doesNotMatch(authLoginRequests, new RegExp(escapeRe(canaries.capability)), "capability canary leaked into authLoginRequests");
}
function escapeRe(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
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

// Aggregate counts the image orchestrator (run-lifecycle.sh) greps for positive values.
const counts = { tests: 0, callbacks: 0, delegations: 0, roots: 0 };

/** Build a subscription codex block whose capability IS the canary and whose claim token is a
 *  distinct fresh secret (asserted absent too). */
function subscriptionBlock(canaries: CodexCanaries, claimToken: string): ClaimCodexSecrets {
  return {
    auth_mode: "subscription",
    access_token: claimToken,
    capability: canaries.capability,
    generation: 3,
    chatgpt_account_id: "verified-account",
    chatgpt_plan_type: null,
  };
}

const agents: AgentTemplate[] = [
  { name: "lead", description: "the lead", prompt_body: "lead body", tools: null, skills: [] },
  { name: "coder", description: "a coder", prompt_body: "coder body", tools: null, skills: [] },
];

// ================================================================================
// BLOCK A — injected-fake lifecycle (host + image); the validated canary-boundary proof.
// ================================================================================
describe("codex-m3b packaged lifecycle (injected fakes)", () => {
  it("dark selection seam: a broken codex block fails CLOSED without echoing the secret or doing model work", async () => {
    counts.tests += 1;
    const canaries = codexCanaries();
    // A subscription block missing its generation is a CodexSelectionError.
    let err: unknown;
    try {
      selectCodexBinding({ codex: { auth_mode: "subscription", access_token: canaries.credential, capability: canaries.capability } });
    } catch (e) {
      err = e;
    }
    assert.ok(err instanceof CodexSelectionError, "a broken block throws CodexSelectionError");
    const message = (err as InstanceType<typeof CodexSelectionError>).message;
    assert.doesNotMatch(message, new RegExp(escapeRe(canaries.credential)), "the error never echoes the token");
    assert.doesNotMatch(message, new RegExp(escapeRe(canaries.capability)), "the error never echoes the capability");
    // FailClosedExecutor.run throws that exact message and does NO model work.
    const failClosed = new FailClosedExecutor(message);
    await assert.rejects((failClosed as Executor).run(makeCtx().ctx), (e: Error) => e.message === message);
  });

  it("new-root resume → implement: a clean turn returns the branch, releases a FRESH token, and keeps every canary off all four surfaces", async () => {
    counts.tests += 1;
    const canaries = codexCanaries();
    const claimToken = `codex-claim-canary-${randomBytes(9).toString("hex")}`;
    const rig = makeRig(canaries);
    // A pre-approved RESUME skips the plan turn/gate (planApproved + approvedPlan + sessionId):
    // the executor resumes the prior session and drives one implement turn that signals done.
    rig.transport
      .push(threadStarted("resumed-1"))
      .push(agentMessage("implementing the approved plan", "resumed-1"))
      .push(signalDone("resumed-1", "tn-1"))
      .push(turnCompleted("completed", "resumed-1"))
      .end();
    const { ctx, emitted } = makeCtx({ planApproved: true, approvedPlan: "the approved plan", sessionId: "prior-session" });
    const log = recordingLog();
    const result = await withTimeout(makeExecutor(rig, bindingOf(subscriptionBlock(canaries, claimToken)), log.log).run(ctx), 5000, "resume run");
    counts.roots += rig.reaped();

    assert.equal(result.branch, "agent/issue-42", "the resumed implement turn returned the branch");
    assert.ok(rig.transport.requests.some((r) => r.method === "thread/resume"), "the harness resumed the prior session");
    assert.ok(rig.sessionOps.adopt >= 1, "the credential-free session subset was adopted");
    assert.equal(rig.client.releaseCalls.length, 1, "a fresh token was released once for the provider root");
    assert.equal(rig.client.releaseCalls[0]?.capability, canaries.capability, "released with the binding capability");
    // The fresh credential rides the app-server login RPC (account/login/start) — NOT the launcher
    // env, which now receives only the immutable auth MODE. This is the intended delivery lane.
    const login = rig.transport.requests.find((r) => r.method === "account/login/start");
    assert.ok(login, "the auth session performed account/login/start");
    assert.equal((login!.params as { accessToken?: string }).accessToken, canaries.credential, "the fresh credential rode the login RPC");
    assert.equal(rig.transport.launchAuthMode, "subscription", "the launch seam received the auth MODE, not a credential");
    const surfaces = surfacesOf(rig, emitted, log);
    assertCanaryBoundary(surfaces, canaries);
    for (const [name, blob] of Object.entries(surfaces)) {
      assert.doesNotMatch(blob, new RegExp(escapeRe(claimToken)), `claim token leaked into ${name}`);
    }
  });

  it("plan gate → implement: a folded root submit_plan gates, approval RECREATES a fresh provider epoch, and a root signal_done on the NEW root resolves the run — with every canary off all four surfaces", async () => {
    counts.tests += 1;
    const canaries = codexCanaries();
    // m2 wires the trusted ROOT signal-routing frame: a root submit_plan the broker accepts now
    // surfaces its scanned plan on a main-origin frame the run-lane reducer folds, so the gate
    // gates on a REAL plan (gated === true) instead of failing closed. m4 then makes plan approval
    // RECREATE the provider epoch (new-root resume), so the plan turn (epoch 0) and the implement
    // turn (epoch 1, resuming the plan thread) run on DISTINCT provider roots/transports — each
    // scripted independently on its own FakeTransport. The submit_plan carries the plan-arg canary
    // as `plan_md` (the arg scanSignals reads); a plan body is model-chosen content, so it may ride
    // emitted output — the credential/capability canaries are what the boundary asserts absent.
    const rig = makeMultiEpochRig(canaries, [
      epochResponder("th-1", "tn-1", (t, th, tn) => {
        t.push(toolCall(1, "submit_plan", { plan_md: canaries.planArg }, th, tn, "c-plan")).push(turnCompleted("completed", th, tn));
      }),
      epochResponder("resumed-1", "tn-2", (t, th, tn) => {
        t.push(signalDone(th, tn, 700, "c-done")).push(turnCompleted("completed", th, tn));
      }),
    ]);
    let gated = false;
    const { ctx, emitted } = makeCtx({
      agents,
      planApproved: false,
      approvedPlan: undefined,
      gatePlan: async () => {
        gated = true;
        return { kind: "approve", selection: { source: "own", agents: [] } } as never;
      },
    });
    const log = recordingLog();
    const result = await withTimeout(makeExecutor(rig, bindingOf(subscriptionBlock(canaries, "t")), log.log).run(ctx), 5000, "plan run");
    for (const e of rig.epochs) counts.roots += e.reaped();

    assert.equal(gated, true, "the folded plan reached the approval gate (m2 signal routing is wired)");
    assert.equal(result.branch, "agent/issue-42", "approval drove the implement loop on the NEW epoch to a root signal_done");
    assert.equal(rig.providerLaunches(), 2, "plan approval recreated a fresh provider epoch (new-root resume)");
    assert.equal(rig.epochs[0]!.transport.turnStartCount, 1, "the plan turn ran on epoch 0");
    assert.equal(rig.epochs[1]!.transport.turnStartCount, 1, "the implement turn ran on epoch 1 (the recreated root)");
    // Even on the wired plan→implement path the credential/capability canaries never leaked, across
    // BOTH epochs' transports.
    assertCanaryBoundary(surfacesOf(rig, emitted, log), canaries);
  });

  it("synchronous subagent + root-only signals: a root spawn_agent runs a demuxed child turn; the child's effect runs, its signal + nested delegation are DENIED, and no canary leaks", async () => {
    counts.tests += 1;
    const canaries = codexCanaries();
    const responder: Responder = (c) => {
      if (c.method === "thread/start") return { thread: { id: c.threadStartCount === 1 ? "th-1" : "th-child" } };
      if (c.method === "turn/start") {
        if (c.turnStartCount === 1) return { turn: { id: "tn-1" } };
        // The CHILD turn: a Bash effect (canary in the arg), a submit_plan (ROOT-only signal),
        // and a nested spawn_agent (nested delegation) — the last two must be denied.
        c.transport
          .push(toolCall(11, "Bash", { command: `echo ${canaries.bashArg}` }, "th-child", "tn-child", "cc-bash"))
          .push(toolCall(12, "submit_plan", { plan: canaries.planArg }, "th-child", "tn-child", "cc-sig"))
          .push(toolCall(13, "spawn_agent", { subagent_type: "coder" }, "th-child", "tn-child", "cc-nest"))
          .push(turnCompleted("completed", "th-child", "tn-child"));
        return { turn: { id: "tn-child" } };
      }
      if (c.method === "turn/interrupt") return {};
      return {};
    };
    const rig = makeRig(canaries, { responder });
    rig.transport.push(threadStarted()).push(toolCall(1, "spawn_agent", { subagent_type: "coder", prompt: canaries.spawnArg }, "th-1", "tn-1", "c-root"));
    const { ctx, emitted } = makeCtx({ agents });
    const log = recordingLog();
    const runP = makeExecutor(rig, bindingOf(subscriptionBlock(canaries, "t")), log.log).run(ctx);

    await waitFor(() => rig.transport.responses.some((r) => r.requestId === 1), "parent spawn_agent reply", 5000);
    // Once the child has settled, a ROOT signal_done on the root turn folds `done` and resolves the
    // implement loop (m2: the loop finishes only on a folded root signal).
    rig.transport.push(signalDone("th-1", "tn-1", 700, "c-done")).push(turnCompleted("completed", "th-1", "tn-1")).end();
    await withTimeout(runP, 5000, "delegation run");
    counts.delegations += 1;
    counts.roots += rig.reaped();

    assert.equal(rig.transport.threadStartCount, 2, "a child thread/start was demuxed onto the same transport");
    assert.equal(rig.transport.turnStartCount, 2, "a child turn/start was demuxed");
    const bash = rig.spawnCommandCalls.find((s) => JSON.stringify(s.argv).includes(canaries.bashArg));
    assert.ok(bash, "the child Bash effect reached the command-identity spawn seam");
    // The scrubbed command-identity env is now exposed THROUGH the seam (finding [3]): the
    // recorded spawn carries LANG=C and no credential/capability.
    assert.equal(bash.opts.env?.LANG, "C", "the command spawn received the scrubbed command-identity env via the seam");
    assert.doesNotMatch(JSON.stringify(bash.opts.env ?? {}), new RegExp(escapeRe(canaries.credential)), "the scrubbed command env carries no credential");
    counts.callbacks += rig.spawnCommandCalls.length;

    const replyOf = (id: number): { success?: boolean } => rec(rec(rig.transport.responses.find((r) => r.requestId === id)?.response).result) as { success?: boolean };
    assert.equal(replyOf(11).success, true, "the child Bash callback succeeded");
    assert.equal(replyOf(12).success, false, "a child submit_plan (non-root signal) is DENIED");
    assert.equal(replyOf(13).success, false, "a child nested spawn_agent is DENIED");
    assert.equal(replyOf(1).success, true, "the parent spawn_agent callback succeeded after the child settled");

    // The Bash ARG canary legitimately reaches the command surface (the model chose it); the
    // CREDENTIAL/CAPABILITY canaries must not. Assert exactly that split.
    const surfaces = surfacesOf(rig, emitted, log);
    assert.match(surfaces.commandState, new RegExp(escapeRe(canaries.bashArg)), "the model-chosen Bash arg flowed to its effect (redaction is not over-broad)");
    assertCanaryBoundary(surfaces, canaries);
  });

  it("checkpoint → finalization: the runner's post-run durability sink reaps through withBoundary, the sink reconcile refreshes + registers/evicts its token, and no canary leaks", async () => {
    counts.tests += 1;
    const canaries = codexCanaries();
    const rig = makeRig(canaries);
    rig.deps = { ...rig.deps, deferRegistryTeardown: true };
    // A pre-approved implement turn signals done so the run resolves cleanly; the POST-run finalize
    // sink then reaps the still-alive registry (deferRegistryTeardown).
    rig.transport.push(threadStarted()).push(signalDone()).push(turnCompleted("completed")).end();
    const { ctx, emitted } = makeCtx({ planApproved: true, approvedPlan: "plan" });
    const log = recordingLog();
    const exec = makeExecutor(rig, bindingOf(subscriptionBlock(canaries, "t")), log.log);
    await withTimeout(exec.run(ctx), 5000, "deferred run");
    // run()'s finally left the registry ALIVE for the post-run sinks.
    assert.equal(rig.reaped(), 0, "run() did not reap the provider root (deferred)");
    assert.ok(exec.safety, "the Codex safety facade is populated");

    // The runner's finalize sink: withBoundary runs the auth-mode reconcile first, then reaps.
    await exec.safety!.withBoundary({ boundary: "finalize", deadlineMs: 200 }, async () => {});
    counts.roots += rig.reaped();
    assert.ok(rig.reaped() >= 1, "the finalize sink reaped the provider root");
    assert.ok(rig.client.refreshCalls.length >= 1, "the sink's subscription reconcile refreshed first");
    const sinkTok = `${canaries.credential}-refreshed`;
    assert.ok(log.added.includes(sinkTok), "the sink reconcile registered its refresh token with the redactor");
    assert.ok(!log.removed.includes(sinkTok), "the sink token is NOT evicted until the terminal dispose");

    // The runner's executeClaim-finally terminal dispose evicts the post-run sink token.
    await exec.safety!.dispose({ boundary: "terminal", deadlineMs: 200 });
    assert.ok(rig.disposed() >= 1, "the terminal dispose tore down the provider root");
    assert.ok(log.removed.includes(sinkTok), "the terminal dispose evicted the post-run sink token (onDispose adoption)");
    assert.deepEqual([...log.added].sort(), [...log.removed].sort(), "every registered token was evicted (no redactor leak)");
    // The reconcile tokens are refresh-derived from the credential canary; the canary itself
    // never rides a public/provider surface.
    assertCanaryBoundary(surfacesOf(rig, emitted, log), canaries);
  });

  it("cancel → finalization: a run cancel first-wins over the raw aborted transport error, tears the provider root down, and leaks no canary", async () => {
    counts.tests += 1;
    const canaries = codexCanaries();
    const controller = new AbortController();
    const rig = makeRig(canaries);
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
    const { ctx, emitted } = makeCtx({ planApproved: true, approvedPlan: "plan", signal: controller.signal });
    const log = recordingLog();
    const p = makeExecutor(rig, bindingOf(subscriptionBlock(canaries, "t")), log.log).run(ctx);
    await waitFor(
      () => rig.transport.turnStartCount === 1,
      "cancel test provider root and turn/start admission",
      5000,
    );
    controller.abort();
    await assert.rejects(withTimeout(p, 5000, "cancel run"), (e: Error) => {
      assert.equal(e.message, "run cancelled", "the cancel trip wins over the raw AbortError");
      return true;
    });
    // The standalone backstop still tore the provider root down on the cancel path.
    assert.ok(rig.disposed() >= 1, "the cancel path tore down the provider root via the backstop");
    assertCanaryBoundary(surfacesOf(rig, emitted, log), canaries);
  });

  it("poison outcome: a failed provider terminal is materialized ONCE as data and thrown once, with no canary leak", async () => {
    counts.tests += 1;
    const canaries = codexCanaries();
    const rig = makeRig(canaries);
    rig.transport.push(threadStarted()).push(turnCompleted("failed")).end();
    const { ctx, emitted } = makeCtx({ planApproved: true, approvedPlan: "plan" });
    const log = recordingLog();
    await assert.rejects(
      withTimeout(makeExecutor(rig, bindingOf(subscriptionBlock(canaries, "t")), log.log).run(ctx), 5000, "failed run"),
      /codex turn failed/,
    );
    const errorResults = emitted.filter((m) => m.kind === "error" && rec(m.payload).event === "result");
    assert.equal(errorResults.length, 1, "the failed terminal is materialized as data exactly once");
    assertCanaryBoundary(surfacesOf(rig, emitted, log), canaries);
  });

  it("emits positive lifecycle counts for the image orchestrator", () => {
    // A machine-readable summary run-lifecycle.sh greps: every count must be > 0 so a
    // silently-empty image run (no callback ran, no delegation demuxed) fails the per-image
    // assertion rather than reading green.
    console.log(`CODEX_M3B_COUNTS ${JSON.stringify(counts)}`);
    assert.ok(counts.tests > 0, "at least one lifecycle test ran");
    assert.ok(counts.callbacks > 0, "at least one callback effect ran");
    assert.ok(counts.delegations > 0, "at least one delegation was demuxed");
    assert.ok(counts.roots > 0, "at least one provider root was reaped");
  });
});

// ================================================================================
// BLOCK B — REAL launch against the loopback fake provider (image only, maintainer-verified).
//
// m6 (items 7-9): this leg drives the PACKAGED CodexExecutor through the REAL launcher → real
// static supervisor → real Codex → the loopback FakeProvider, over the REAL WorkerClient Bearer
// routes (release/refresh) into a throwaway migrated Postgres when run-lifecycle.sh supplies the
// server-contract env. It emits a SEPARATE real-path counts object and requires every category the
// lifecycle exercises to be nonzero — so a real-path exception (ran=false) or a silently-empty
// stage FAILS here rather than reading green (the vacuous Block B is retired). The exact real
// app-server item framing + parent/child turn interleaving is VERIFIED by the 2026-09-10
// native both-image run; the recording responder always eventually drives a root
// `signal_done`, so a clean run reaches `ran === true`. The outer run-lifecycle.sh
// watchdog bounds a wedged real process.
// ================================================================================
const PACKAGED = process.env.CODEX_M3B_PACKAGED === "1";

// LIVE (PRD #1106 M3b live-acceptance): strictly opt-in via env, set only by run-lifecycle.sh's
// live branch (live.sh --live). When set, the subscription leg drives the REAL Codex provider with
// a maintainer-injected login and the api_key leg is skipped (subscription-only). When UNSET,
// nothing below reads it, so Block A and the offline Block B are byte-for-byte unchanged.
const LIVE = process.env.CODEX_M3B_LIVE === "1";

// The production Responses base URL fallback when CODEX_M3B_LIVE_BASE_URL is unset (mirrors
// CODEX_PRODUCTION_PROVIDER.baseUrl in agent/src/codex/codex-executor.ts).
const LIVE_BASE_URL_FALLBACK = "https://api.openai.com/v1";

// REFRESH PROBES (PRD #1171 M3, item 1): the sequential advance+replay probe and the
// two-concurrent-refresh "check-b" REPEAT the already-accepted coordinated-refresh proof
// (PRD #1171 "Maintainer-only live acceptance" check 2), and each real refresh ROTATES the
// seat's refresh-token family. So they are strictly OPT-IN and DEFAULT OFF: today's live
// pass proves the subscription advice + cancel/root-settlement + no-leak checks WITHOUT
// rotating the seat. Set CODEX_M3B_LIVE_REFRESH_PROBES=1 to reproduce accepted check 2.
const LIVE_REFRESH_PROBES = process.env.CODEX_M3B_LIVE_REFRESH_PROBES === "1";

describe("codex-m3b real-provider responder routing", () => {
  it("rejects a non-empty bearer that the worker API did not release", async () => {
    const { FakeProvider } = await import("./fake-provider.js");
    const allowedBearerDigests = new Set<string>();
    const fake = await FakeProvider.start({ allowedBearerDigests, respond: () => [] });
    try {
      const rejected = await fetch(`${fake.baseUrl}/responses`, {
        method: "POST",
        headers: { authorization: "Bearer unreleased-test-token", "content-type": "application/json" },
        body: "{}",
      });
      assert.equal(rejected.status, 500, "an unreleased non-empty bearer must fail closed");
      assert.equal(fake.requests.length, 0, "an unauthorized request never reaches the provider responder");

      const released = "released-test-token";
      allowedBearerDigests.add(bearerDigest(released));
      const accepted = await fetch(`${fake.baseUrl}/responses`, {
        method: "POST",
        headers: { authorization: `Bearer ${released}`, "content-type": "application/json" },
        body: "{}",
      });
      assert.equal(accepted.status, 200, "a bearer recorded from a worker release is accepted");
      assert.equal(fake.requests.length, 1);
    } finally {
      await fake.close();
    }
  });

  it("a child turn finishes without consuming the root checkpoint stage", async () => {
    const { recordingLifecycleResponder, codexCanaries: freshCanaries } = await import("./fake-provider.js");
    const canaries = freshCanaries();
    const { respond } = recordingLifecycleResponder(canaries);
    const root = () => respond({ input: [] }, undefined as never);
    assert.equal(root()[0]?.name, "submit_plan");
    assert.equal(root()[0]?.type, "message", "submit_plan ends its root turn");
    assert.equal(root()[0]?.name, "uzi_bash");
    assert.equal(root()[0]?.name, "uzi_apply_patch");
    assert.equal(root()[0]?.name, "spawn_agent");
    const child = respond({ input: [{ type: "input_text", text: canaries.spawnArg }] }, undefined as never);
    assert.equal(child[0]?.type, "message", "the child receives a terminal message");
    const afterChild = respond({
      input: [{ type: "function_call", arguments: JSON.stringify({ prompt: canaries.spawnArg }) }],
    }, undefined as never);
    assert.equal(afterChild[0]?.name, "checkpoint", "the parent history does not misclassify and keeps its next root stage");
  });
});

/** The instance type of the REAL worker→API transport (agent/src/client.ts). Loaded at runtime
 *  from the image-baked (or host source-tree) `src`, mirroring packaged-modules.ts, so Block B
 *  hits the REAL Bearer release/refresh routes. */
type WorkerClientInstance = import("../../agent/src/client.js").WorkerClient;

/** The item-7 server-contract env run-lifecycle.sh injects from the go test-server's JSON line.
 *  When present, Block B drives the REAL WorkerClient against the throwaway Postgres; when absent
 *  it falls back to the in-memory fake client (a bare CODEX_M3B_PACKAGED run with no server still
 *  exercises the launcher), and the same item-8 assertions apply. */
interface ServerContract {
  readonly baseUrl: string;
  readonly workerToken: string;
  readonly sub: { readonly runId: string; readonly capability: string; readonly account: string; readonly generation: number };
  readonly apiKey: { readonly runId: string; readonly capability: string };
}

function readServerContract(): ServerContract | undefined {
  const baseUrl = process.env.CODEX_M3B_WORKER_BASE_URL;
  const workerToken = process.env.CODEX_M3B_WORKER_TOKEN;
  const subRunId = process.env.CODEX_M3B_SUB_RUN_ID;
  const subCap = process.env.CODEX_M3B_SUB_CAP;
  const subAccount = process.env.CODEX_M3B_SUB_ACCOUNT;
  const subGenRaw = process.env.CODEX_M3B_SUB_GEN;
  const apiRunId = process.env.CODEX_M3B_APIKEY_RUN_ID;
  const apiCap = process.env.CODEX_M3B_APIKEY_CAP;
  // Subscription fields are always required. In LIVE mode the api_key arm is skipped
  // (subscription-only seeding), so its fields may be absent — the subscription leg still uses
  // the real WorkerClient. Offline they are required exactly as before.
  const apiKeyOk = LIVE || (Boolean(apiRunId) && Boolean(apiCap));
  if (!baseUrl || !workerToken || !subRunId || !subCap || !subAccount || !subGenRaw || !apiKeyOk) {
    return undefined;
  }
  const generation = Number(subGenRaw);
  if (!Number.isSafeInteger(generation) || generation < 0) return undefined;
  return {
    baseUrl,
    workerToken,
    sub: { runId: subRunId, capability: subCap, account: subAccount, generation },
    apiKey: { runId: apiRunId ?? "", capability: apiCap ?? "" },
  };
}

/** Load the REAL WorkerClient constructor from the packaged (or host) `src`, resolving the dir
 *  exactly like packaged-modules.ts (CODEX_M3B_SRC=/app/src in the image, else `<cwd>/src`). */
async function loadWorkerClientCtor(): Promise<typeof import("../../agent/src/client.js").WorkerClient> {
  const { pathToFileURL } = await import("node:url");
  const override = process.env.CODEX_M3B_SRC;
  const src = override && override.trim().length > 0 ? override : nodePath.resolve(process.cwd(), "src");
  const mod = (await import(pathToFileURL(`${src}/client.ts`).href)) as typeof import("../../agent/src/client.js");
  return mod.WorkerClient;
}

/** release/refresh call tallies observed THROUGH the real WorkerClient (the executor's credential
 *  bridge is internal, so a counting Proxy is the only external seam). */
interface ClientCounts {
  release: number;
  refresh: number;
  refreshAdvanced: number;
  refreshReplayed: number;
}

/** Wrap the REAL WorkerClient so release/refresh are counted (and refresh outcomes classified)
 *  while every other method — the tool-handler forge/memory/findings surface — delegates
 *  unchanged to the real instance. */
function countingWorkerClient(
  real: WorkerClientInstance,
  counts: ClientCounts,
  allowedBearerDigests: Set<string>,
): WorkerClientInstance {
  return new Proxy(real, {
    get(target, prop, receiver) {
      if (prop === "releaseCodex") {
        return async (...args: Parameters<WorkerClientInstance["releaseCodex"]>) => {
          counts.release += 1;
          const res = await target.releaseCodex(...args);
          allowedBearerDigests.add(bearerDigest(res.access_token));
          return res;
        };
      }
      if (prop === "refreshCodex") {
        return async (...args: Parameters<WorkerClientInstance["refreshCodex"]>) => {
          counts.refresh += 1;
          const res = await target.refreshCodex(...args);
          allowedBearerDigests.add(bearerDigest(res.access_token));
          if (res.outcome === "advanced") counts.refreshAdvanced += 1;
          else if (res.outcome === "replayed") counts.refreshReplayed += 1;
          return res;
        };
      }
      const value = Reflect.get(target, prop, receiver) as unknown;
      return typeof value === "function" ? (value as (...a: unknown[]) => unknown).bind(target) : value;
    },
  });
}

/** The credential-free session store, counting its ops so the test observes the reap-boundary
 *  `persist` (called before every provider-root reap and at terminal) in the REAL launch path. */
interface SessionOps {
  adopt: number;
  adoptFiles: number;
  persist: number;
  persistFiles: number;
  persistFailures: number;
  lastPersistError?: string;
  inspect: number;
  remove: number;
}
function countingSessionStore(ops: SessionOps): NonNullable<CodexExecutorDeps["sessionStore"]> {
  return {
    adopt: async (...args) => {
      ops.adopt += 1;
      const result = await CodexSessionStore.adopt(...args);
      ops.adoptFiles += result.files;
      return result;
    },
    inspect: async (...args) => {
      ops.inspect += 1;
      return CodexSessionStore.inspect(...args);
    },
    remove: async (...args) => {
      ops.remove += 1;
      return CodexSessionStore.remove(...args);
    },
    persist: async (...args) => {
      ops.persist += 1;
      try {
        const result = await CodexSessionStore.persist(...args);
        ops.persistFiles += result.files;
        return result;
      } catch (error) {
        ops.persistFailures += 1;
        ops.lastPersistError = error instanceof Error ? `${error.name}:${error.message}` : "unknown";
        throw error;
      }
    },
  };
}

/** A subscription binding whose capability/account/generation MATCH the seeded run so the REAL
 *  Bearer release/refresh routes authorize. `access_token` is a required non-empty field the
 *  selector validates but the executor never uses — it RE-RELEASES the committed token via
 *  releaseCodex, and the real credential comes from the throwaway Postgres. */
function subscriptionBindingFromContract(c: ServerContract): CodexBinding {
  return bindingOf({
    auth_mode: "subscription",
    access_token: "m3b-binding-token-unused",
    capability: c.sub.capability,
    generation: c.sub.generation,
    chatgpt_account_id: c.sub.account,
    chatgpt_plan_type: null,
  });
}
/** An api_key binding for the seeded api_key run (no subscription generation/account). */
function apiKeyBindingFromContract(c: ServerContract): CodexBinding {
  return bindingOf({ auth_mode: "api_key", access_token: "m3b-binding-token-unused", capability: c.apiKey.capability });
}

/** Fresh writable worktree + home under /data/runner's setgid runner-group tree. Do not use
 *  mkdtemp: Node forces 0700, which would falsify the uid-10003 command-root access posture this
 *  packaged control exercises. */
function makeScratch(): { scratch: string; worktree: string; home: string } {
  const scratch = nodePath.join("/data/runner", `codex-m3b-${randomBytes(8).toString("hex")}`);
  fs.mkdirSync(scratch, { recursive: true, mode: 0o2770 });
  fs.chmodSync(scratch, 0o2770);
  const worktree = nodePath.join(scratch, "work");
  fs.mkdirSync(worktree, { recursive: true, mode: 0o2770 });
  fs.chmodSync(worktree, 0o2770);
  return { scratch, worktree, home: nodePath.join(scratch, "home") };
}

function sleepMs(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

interface LiveTurnStartLatch {
  readonly observed: Promise<void>;
  readonly released: Promise<void>;
  markObserved(): void;
  release(): void;
}

function liveTurnStartLatch(): LiveTurnStartLatch {
  let markObserved!: () => void;
  let release!: () => void;
  let observedOnce = false;
  let releasedOnce = false;
  const observed = new Promise<void>((resolve) => { markObserved = resolve; });
  const released = new Promise<void>((resolve) => { release = resolve; });
  return {
    observed,
    released,
    markObserved: () => {
      if (!observedOnce) {
        observedOnce = true;
        markObserved();
      }
    },
    release: () => {
      if (!releasedOnce) {
        releasedOnce = true;
        release();
      }
    },
  };
}

interface LiveTransportRequest {
  readonly method: string;
  readonly params: unknown;
}

interface LiveServerRequest {
  readonly method: string | null;
  readonly requestId: number | string;
  readonly params: unknown;
}

interface LiveServerResponse {
  readonly requestId: number | string;
  readonly kind: "result" | "error";
}

/** In-process evidence from a TEST-ONLY adapter around the packaged production launcher and
 * transport. It retains no stdout/stderr and is never printed; only count/boolean projections enter
 * CODEX_M3B_LIVE_SUB_COUNTS. */
interface LiveLaunchEvidence {
  providerLaunched: number;
  actionLaunched: number;
  providerSettled: number;
  actionSettled: number;
  disposalFailures: number;
  transportClosed: number;
  turnStarts: number;
  readonly requests: LiveTransportRequest[];
  readonly serverRequests: LiveServerRequest[];
  readonly serverResponses: LiveServerResponse[];
  readonly threadIds: Set<string>;
  readonly turnIds: Set<string>;
  readonly turnStartLatch?: LiveTurnStartLatch;
}

function liveLaunchEvidence(turnStartLatch?: LiveTurnStartLatch): LiveLaunchEvidence {
  return {
    providerLaunched: 0,
    actionLaunched: 0,
    providerSettled: 0,
    actionSettled: 0,
    disposalFailures: 0,
    transportClosed: 0,
    turnStarts: 0,
    requests: [],
    serverRequests: [],
    serverResponses: [],
    threadIds: new Set<string>(),
    turnIds: new Set<string>(),
    ...(turnStartLatch === undefined ? {} : { turnStartLatch }),
  };
}

function addLiveId(set: Set<string>, value: unknown): void {
  if (typeof value === "string" && value.length > 0) set.add(value);
}

function recordLiveRequestIds(evidence: LiveLaunchEvidence, params: unknown, result: unknown): void {
  const request = rec(params);
  addLiveId(evidence.threadIds, request.threadId);
  addLiveId(evidence.threadIds, request.sessionId);
  addLiveId(evidence.turnIds, request.turnId);
  const response = rec(result);
  addLiveId(evidence.threadIds, rec(response.thread).id);
  addLiveId(evidence.turnIds, rec(response.turn).id);
}

function recordLiveNoteIds(evidence: LiveLaunchEvidence, note: CodexNotification): void {
  if (note.kind !== "activity") {
    addLiveId(evidence.threadIds, note.threadId);
    if (note.kind !== "thread_started") addLiveId(evidence.turnIds, note.turnId);
  }
  const params = rec(note.params);
  addLiveId(evidence.threadIds, params.threadId);
  addLiveId(evidence.threadIds, params.sessionId);
  addLiveId(evidence.turnIds, params.turnId);
  addLiveId(evidence.threadIds, rec(params.thread).id);
  addLiveId(evidence.turnIds, rec(params.turn).id);
}

/** Observe the real JSON-RPC transport without changing request/response semantics. A cancel test
 * may hold the first successful turn/start response long enough to abort deterministically after
 * the app-server admitted the turn but before the harness can consume a terminal. */
function observingLiveTransport(inner: CodexTransport, evidence: LiveLaunchEvidence): CodexTransport {
  let heldTurnStart = false;
  let closePromise: Promise<void> | undefined;
  return {
    async request<T = unknown>(method: string, params?: unknown, opts?: { signal?: AbortSignal; deadlineMs?: number }): Promise<T> {
      evidence.requests.push({ method, params });
      const result = await inner.request<T>(method, params, opts);
      recordLiveRequestIds(evidence, params, result);
      if (method === "turn/start" && typeof rec(rec(result).turn).id === "string") {
        evidence.turnStarts += 1;
        if (!heldTurnStart && evidence.turnStartLatch !== undefined) {
          heldTurnStart = true;
          evidence.turnStartLatch.markObserved();
          await evidence.turnStartLatch.released;
        }
      }
      return result;
    },
    notify(method, params): void {
      inner.notify(method, params);
    },
    respond(requestId, response): void {
      evidence.serverResponses.push({ requestId, kind: "error" in response ? "error" : "result" });
      inner.respond(requestId, response);
    },
    installServerRequestInterceptor(interceptor): () => void {
      const install = inner.installServerRequestInterceptor;
      if (install === undefined) throw new Error("live transport lacks the production auth interceptor seam");
      return install.call(inner, (note, frameBytes) => {
        recordLiveNoteIds(evidence, note);
        if (note.kind === "activity" && note.requestId !== undefined) {
          evidence.serverRequests.push({ method: note.method, requestId: note.requestId, params: note.params });
        }
        return interceptor(note, frameBytes);
      });
    },
    notifications(): AsyncIterableIterator<CodexNotification> {
      const source = inner.notifications();
      const iterator: AsyncIterableIterator<CodexNotification> = {
        next: async () => {
          const step = await source.next();
          if (!step.done) recordLiveNoteIds(evidence, step.value);
          return step;
        },
        [Symbol.asyncIterator](): AsyncIterableIterator<CodexNotification> {
          return iterator;
        },
      };
      return iterator;
    },
    close(): Promise<void> {
      if (closePromise === undefined) {
        closePromise = inner.close().then(() => {
          evidence.transportClosed += 1;
        });
      }
      return closePromise;
    },
  };
}

function trackedLiveRoot(
  handle: CodexRootHandle,
  evidence: LiveLaunchEvidence,
  kind: "provider" | "action",
): CodexRootHandle {
  let settled = false;
  return {
    started: handle.started,
    supervisorPid: handle.supervisorPid,
    transport: handle.transport,
    snapshot: (timeoutMs) => handle.snapshot(timeoutMs),
    waitChild: (timeoutMs) => handle.waitChild(timeoutMs),
    dispose: async (timeoutMs) => {
      try {
        const outcome = await handle.dispose(timeoutMs);
        if (outcome.clean && !settled) {
          settled = true;
          if (kind === "provider") evidence.providerSettled += 1;
          else evidence.actionSettled += 1;
        } else if (!outcome.clean) {
          evidence.disposalFailures += 1;
        }
        return outcome;
      } catch (error) {
        evidence.disposalFailures += 1;
        throw error;
      }
    },
    get failed() {
      return handle.failed;
    },
    whenFailed: handle.whenFailed,
  };
}

/** The TEST-ONLY launcher adapter used by both live run and advice passes. Its body deliberately
 * mirrors production defaultLaunchProviderRoot: fixed binaries/argv, app-server auth, provider
 * stderr drained, and createCodexTransport over the launched root's stdio. */
async function launchTrackedLiveProvider(
  spec: CodexLaunchRootSpec,
  authMode: CodexAppServerAuthMode,
  evidence: LiveLaunchEvidence,
): Promise<{ readonly handle: CodexRootHandle; readonly transport: CodexTransport; readonly supervisorPid: number }> {
  if (spec.credentialValue !== undefined) {
    throw new Error("live app-server-auth launch received an environment credential");
  }
  const launched = await launchCodexRoot({
    ownedDataRoot: spec.ownedDataRoot,
    provider: {
      name: spec.provider.name,
      baseUrl: spec.provider.baseUrl,
      envKey: spec.provider.envKey,
    },
    model: spec.model,
    codexBin: CODEX_BIN,
    supervisorBin: SUPERVISOR_BIN,
    kind: "provider",
    childArgv: [...PROVIDER_CHILD_ARGV],
    cwd: spec.cwd,
    useAppServerAuth: true,
    authMode,
    seedSession: spec.seedSession,
  });
  evidence.providerLaunched += 1;
  const handle = trackedLiveRoot(launched, evidence, "provider");
  const stdout = handle.transport.stdout;
  const stdin = handle.transport.stdin;
  if (!stdout || !stdin) {
    await handle.dispose(5000).catch(() => undefined);
    throw new Error("live provider root is missing a stdio transport channel");
  }
  handle.transport.stderr?.resume();
  const transport = observingLiveTransport(createCodexTransport({ inbound: stdout, outbound: stdin }), evidence);
  return { handle, transport, supervisorPid: handle.supervisorPid ?? -1 };
}

function liveProviderLaunchSeam(evidence: LiveLaunchEvidence): NonNullable<CodexExecutorDeps["launchProviderRoot"]> {
  return async (spec, authMode) => {
    const launched = await launchTrackedLiveProvider(spec, authMode, evidence);
    return {
      root: registeredRoot(launched.handle, "provider"),
      transport: launched.transport,
      supervisorPid: launched.supervisorPid,
    };
  };
}

function liveEffectLaunchSeam(evidence: LiveLaunchEvidence): NonNullable<CodexExecutorDeps["launchEffectRoot"]> {
  return async (spec, deadlineMs) => {
    const handle = await launchCodexEffectRoot(
      spec,
      deadlineMs === undefined ? {} : { deadlines: { started: deadlineMs } },
    );
    evidence.actionLaunched += 1;
    return trackedLiveRoot(handle, evidence, "action");
  };
}

function liveAdviceLaunchSeam(scratch: string, evidence: LiveLaunchEvidence): LaunchAdviceRootSeam {
  let launchSequence = 0;
  return async (spec) => {
    if (spec.credentialValue !== undefined) {
      throw new Error("live advice launch received an environment credential");
    }
    if (spec.signal?.aborted) throw new Error("live advice launch was aborted before root creation");
    launchSequence += 1;
    const cwd = nodePath.join(scratch, `advice-work-${launchSequence}`);
    fs.mkdirSync(cwd, { recursive: true, mode: 0o2770 });
    fs.chmodSync(cwd, 0o2770);
    const launched = await launchTrackedLiveProvider(
      {
        kind: "provider",
        provider: spec.provider,
        model: spec.model,
        cwd,
        ownedDataRoot: nodePath.join(scratch, `advice-root-${launchSequence}`),
      },
      "subscription",
      evidence,
    );
    let disposePromise: Promise<void> | undefined;
    const dispose = (): Promise<void> => {
      if (disposePromise === undefined) {
        disposePromise = (async () => {
          let transportClosed = true;
          try {
            await launched.transport.close();
          } catch {
            transportClosed = false;
          }
          const outcome = await launched.handle.dispose(5000);
          if (!outcome.clean) throw new Error("live advice provider root did not settle cleanly");
          if (!transportClosed) throw new Error("live advice transport did not close cleanly");
        })();
      }
      return disposePromise;
    };
    if (spec.signal?.aborted) {
      await dispose().catch(() => undefined);
      throw new Error("live advice launch was aborted after root creation");
    }
    return { transport: launched.transport, cwd, dispose };
  };
}

const LIVE_AUTH_REQUEST_METHODS = new Set([
  "account/login/start",
  "account/chatgptAuthTokens/refresh",
]);
const LIVE_PROTOCOL_ID_KEYS = new Set([
  "id",
  "requestId",
  "request_id",
  "threadId",
  "thread_id",
  "turnId",
  "turn_id",
  "sessionId",
  "session_id",
  "callId",
  "call_id",
]);

/** Remove only protocol-routing identifier fields from the request-evidence projection. The raw
 * in-process evidence is retained for structural assertions; this projection proves an id cannot
 * escape in model text/config while allowing threadId/turnId in their required RPC routing slots. */
function withoutProtocolIds(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(withoutProtocolIds);
  if (value === null || typeof value !== "object") return value;
  const out: Record<string, unknown> = {};
  for (const [key, nested] of Object.entries(value as Record<string, unknown>)) {
    if (!LIVE_PROTOCOL_ID_KEYS.has(key)) out[key] = withoutProtocolIds(nested);
  }
  return out;
}

function publicLiveRequestEvidence(
  evidences: readonly LiveLaunchEvidence[],
  stripProtocolIds: boolean,
): string {
  const project = (value: unknown): unknown => stripProtocolIds ? withoutProtocolIds(value) : value;
  return JSON.stringify(evidences.flatMap((evidence) => [
    ...evidence.requests
      .filter((request) => !LIVE_AUTH_REQUEST_METHODS.has(request.method))
      .map((request) => ({ method: request.method, params: project(request.params) })),
    ...evidence.serverRequests
      .filter((request) => request.method === null || !LIVE_AUTH_REQUEST_METHODS.has(request.method))
      .map((request) => ({ method: request.method, params: project(request.params) })),
  ]));
}

/** Assert absence without handing assert/regex the needle or haystack. If this trips, Node prints
 * only the static category/surface message, never the secret or the evidence containing it. */
function assertLiveValueAbsent(blob: string, needle: string | undefined, message: string): void {
  if (needle !== undefined && needle.length > 0 && blob.includes(needle)) throw new Error(message);
}

async function withStaticLiveFailure<T>(work: Promise<T>, message: string): Promise<T> {
  try {
    return await work;
  } catch {
    throw new Error(message);
  }
}

function endpointNeedles(raw: string): string[] {
  const out = [raw];
  try {
    const url = new URL(raw);
    out.push(url.origin, url.host, url.hostname);
  } catch {
    throw new Error("live endpoint is not a valid URL");
  }
  return [...new Set(out.filter((value) => value.length > 0))];
}

function assertLiveNoLeak(options: {
  readonly evidences: readonly LiveLaunchEvidence[];
  readonly emitted: readonly EmittedMessage[];
  readonly additionalMessages?: readonly string[];
  readonly log: RecordingLog;
  readonly tokens: readonly string[];
  readonly authForbiddenTokens: readonly string[];
  readonly capability: string;
  readonly accountId: string;
  readonly endpoints: readonly string[];
  readonly extraIds?: readonly string[];
}): void {
  const allIds = new Set(options.extraIds ?? []);
  for (const evidence of options.evidences) {
    for (const id of evidence.threadIds) allIds.add(id);
    for (const id of evidence.turnIds) allIds.add(id);
  }
  const messageBlob = JSON.stringify({ emitted: options.emitted, additional: options.additionalMessages ?? [] });
  const logBlob = options.log.lines.join("\n");
  const surfaces = [
    { name: "messages", blob: messageBlob },
    { name: "logs", blob: logBlob },
    // Keep protocol ids in this projection while checking every OTHER category. A token/account
    // smuggled into an `id` field must not disappear merely because ids have an allowed wire slot.
    { name: "request evidence", blob: publicLiveRequestEvidence(options.evidences, false) },
  ] as const;
  const categories: ReadonlyArray<readonly [string, readonly string[]]> = [
    ["access or refresh token", [...new Set(options.tokens.filter((value) => value.length > 0))]],
    ["capability", [options.capability]],
    ["account id", [options.accountId]],
    ["provider URL or host", [...new Set(options.endpoints.flatMap(endpointNeedles))]],
  ];
  for (const surface of surfaces) {
    for (const [category, needles] of categories) {
      for (const needle of needles) {
        assertLiveValueAbsent(surface.blob, needle, `live ${category} appeared in ${surface.name}`);
      }
    }
  }

  const idSurfaces = [
    { name: "messages", blob: messageBlob },
    { name: "logs", blob: logBlob },
    // Remove only required RPC routing fields for the id check. The same value in prompt/config/text
    // remains visible and fails; tokens/accounts/endpoints were checked against the raw projection.
    { name: "request evidence", blob: publicLiveRequestEvidence(options.evidences, true) },
  ] as const;
  for (const surface of idSurfaces) {
    for (const id of allIds) {
      assertLiveValueAbsent(surface.blob, id, `live session or thread id appeared in ${surface.name}`);
    }
  }

  // The auth lane legitimately contains the released access token + account id. Even there, the
  // refresh token, run capability, and endpoint coordinates must be absent. Keep this raw lane
  // in-process and use the same non-disclosing failure helper.
  const rawAuthEvidence = JSON.stringify(options.evidences.flatMap((evidence) => [
    ...evidence.requests.filter((request) => LIVE_AUTH_REQUEST_METHODS.has(request.method)),
    ...evidence.serverRequests.filter((request) => request.method !== null && LIVE_AUTH_REQUEST_METHODS.has(request.method)),
  ]));
  for (const token of options.authForbiddenTokens) {
    assertLiveValueAbsent(rawAuthEvidence, token, "live forbidden token appeared in auth request evidence");
  }
  assertLiveValueAbsent(rawAuthEvidence, options.capability, "live capability appeared in auth request evidence");
  for (const endpoint of options.endpoints.flatMap(endpointNeedles)) {
    assertLiveValueAbsent(rawAuthEvidence, endpoint, "live provider URL or host appeared in auth request evidence");
  }
  const rawRefreshRequests = JSON.stringify(options.evidences.flatMap((evidence) =>
    evidence.serverRequests.filter((request) => request.method === "account/chatgptAuthTokens/refresh"),
  ));
  for (const token of options.tokens) {
    assertLiveValueAbsent(rawRefreshRequests, token, "live token appeared in a provider refresh request");
  }
}

function assertSubscriptionLoginShape(
  evidence: LiveLaunchEvidence,
  accountId: string,
  capturedTokens: readonly string[],
): void {
  const logins = evidence.requests.filter((request) => request.method === "account/login/start");
  if (logins.length !== evidence.providerLaunched || logins.length === 0) {
    throw new Error("every live provider root must authenticate exactly once");
  }
  for (const login of logins) {
    const params = rec(login.params);
    if (params.type !== "chatgptAuthTokens") throw new Error("live provider root did not use subscription authentication");
    if (typeof params.accessToken !== "string" || !capturedTokens.includes(params.accessToken)) {
      throw new Error("live provider root did not use a freshly released access token");
    }
    if (params.chatgptAccountId !== accountId) {
      throw new Error("live provider root did not use the server-verified account id");
    }
    if (params.chatgptPlanType !== null) {
      throw new Error("live provider root did not pin the subscription plan hint to null");
    }
    assert.deepEqual(
      Object.keys(params).sort(),
      ["accessToken", "chatgptAccountId", "chatgptPlanType", "type"],
      "the subscription login RPC carries only the pinned auth fields",
    );
  }
}

function assertAdviceCeiling(evidence: LiveLaunchEvidence): void {
  const starts = evidence.requests.filter((request) => request.method === "thread/start");
  assert.equal(starts.length, 1, "one isolated advice thread was started");
  const params = rec(starts[0]?.params);
  if (!Array.isArray(params.dynamicTools) || params.dynamicTools.length !== 0) {
    throw new Error("the live advice thread exposed a dynamic tool");
  }
  if (!Array.isArray(params.environments) || params.environments.length !== 0) {
    throw new Error("the live advice thread exposed a native environment");
  }
  if (params.ephemeral !== true) throw new Error("the live advice thread was not ephemeral");
  if (params.approvalPolicy !== "never") throw new Error("the live advice thread could request approval");
  const config = rec(params.config);
  if (config.project_doc_max_bytes !== 0) throw new Error("the live advice thread could read project instructions");
  if (config.web_search !== "disabled") throw new Error("the live advice thread did not disable web search");
  if (rec(config.agents).enabled !== false) throw new Error("the live advice thread did not disable native agents");
  const featureValues = Object.values(rec(config.features));
  if (featureValues.length === 0 || featureValues.some((value) => value !== false)) {
    throw new Error("the live advice thread did not disable every declared native feature");
  }
  const cwd = params.cwd;
  if (typeof cwd !== "string" || rec(rec(config.projects)[cwd]).trust_level !== "untrusted") {
    throw new Error("the live advice project was not explicitly untrusted");
  }

  for (const request of evidence.serverRequests) {
    if (request.method === "account/chatgptAuthTokens/refresh") continue;
    const response = evidence.serverResponses.find((candidate) => candidate.requestId === request.requestId);
    if (response?.kind !== "error") {
      throw new Error("a non-auth advice server request was not refused fail-closed");
    }
  }
}

describe("codex-m3b live no-leak oracle calibration", () => {
  it("checks raw id fields for secrets, allows routing ids only in their slots, and never discloses a matched value", () => {
    const token = `live-token-calibration-${randomBytes(12).toString("hex")}`;
    const tokenEvidence = liveLaunchEvidence();
    tokenEvidence.requests.push({ method: "turn/start", params: { id: token } });
    let tokenError: unknown;
    try {
      assertLiveNoLeak({
        evidences: [tokenEvidence],
        emitted: [],
        log: recordingLog(),
        tokens: [token],
        authForbiddenTokens: [],
        capability: "calibration-capability",
        accountId: "calibration-account",
        endpoints: ["https://provider.example.test/v1"],
      });
    } catch (error) {
      tokenError = error;
    }
    if (!(tokenError instanceof Error)
      || tokenError.message !== "live access or refresh token appeared in request evidence"
      || tokenError.message.includes(token)) {
      throw new Error("live token no-leak calibration did not fail safely");
    }

    const threadId = `live-thread-calibration-${randomBytes(12).toString("hex")}`;
    const idEvidence = liveLaunchEvidence();
    idEvidence.threadIds.add(threadId);
    idEvidence.requests.push({ method: "turn/start", params: { threadId } });
    assertLiveNoLeak({
      evidences: [idEvidence],
      emitted: [],
      log: recordingLog(),
      tokens: [],
      authForbiddenTokens: [],
      capability: "calibration-capability",
      accountId: "calibration-account",
      endpoints: ["https://provider.example.test/v1"],
    });
    idEvidence.requests.push({
      method: "turn/start",
      params: { threadId, input: [{ type: "text", text: threadId }] },
    });
    let idError: unknown;
    try {
      assertLiveNoLeak({
        evidences: [idEvidence],
        emitted: [],
        log: recordingLog(),
        tokens: [],
        authForbiddenTokens: [],
        capability: "calibration-capability",
        accountId: "calibration-account",
        endpoints: ["https://provider.example.test/v1"],
      });
    } catch (error) {
      idError = error;
    }
    if (!(idError instanceof Error)
      || idError.message !== "live session or thread id appeared in request evidence"
      || idError.message.includes(threadId)) {
      throw new Error("live identifier no-leak calibration did not fail safely");
    }
  });

  it("prepares the shared home ancestors owned by an injected real provider-launch seam", () => {
    const scratch = fs.mkdtempSync(nodePath.join(process.env.TMPDIR ?? "/tmp", "codex-m3b-live-paths-"));
    try {
      const paths = makeLiveRunPaths(scratch, "cancel");
      assert.equal(fs.statSync(paths.worktree).isDirectory(), true);
      assert.equal(fs.statSync(paths.home).isDirectory(), true);
      assert.equal(fs.statSync(nodePath.join(paths.home, "codex-data")).isDirectory(), true);
    } finally {
      fs.rmSync(scratch, { recursive: true, force: true });
    }
  });
});

/** A proxy over the real WorkerClient that CAPTURES every plaintext access token a release/refresh
 *  hands back (the executor's and the probes'), so the live no-leak proof can scan messages, logs and
 *  non-auth request evidence without ever printing a token. Every other method delegates unchanged. */
function tokenCapturingClient(inner: WorkerClientInstance, captured: string[]): WorkerClientInstance {
  return new Proxy(inner, {
    get(target, prop, receiver) {
      if (prop === "releaseCodex") {
        return async (...args: Parameters<WorkerClientInstance["releaseCodex"]>) => {
          const res = await target.releaseCodex(...args);
          if (res.access_token) captured.push(res.access_token);
          return res;
        };
      }
      if (prop === "refreshCodex") {
        return async (...args: Parameters<WorkerClientInstance["refreshCodex"]>) => {
          const res = await target.refreshCodex(...args);
          if (res.access_token) captured.push(res.access_token);
          return res;
        };
      }
      const value = Reflect.get(target, prop, receiver) as unknown;
      return typeof value === "function" ? (value as (...a: unknown[]) => unknown).bind(target) : value;
    },
  });
}

type LiveRefreshResp = Awaited<ReturnType<WorkerClientInstance["refreshCodex"]>>;

/** Run one coordinated subscription refresh, retaining the operation id across a retry: a 409
 *  "contended" (another operation holds the live lease) is the documented retry-and-reconcile
 *  signal, and re-sending the SAME operation id after the winner commits replays/reconciles to the
 *  committed token rather than starting a second exchange. */
async function refreshWithRetry(
  client: WorkerClientInstance,
  runId: string,
  capability: string,
  account: string,
  operationId: string,
  observedGeneration: number,
  maxAttempts = 8,
): Promise<LiveRefreshResp> {
  let lastErr: unknown;
  for (let attempt = 0; attempt < maxAttempts; attempt += 1) {
    try {
      return await client.refreshCodex(
        runId,
        { capability, operation_id: operationId, observed_generation: observedGeneration },
        { authMode: "subscription", chatgptAccountId: account },
      );
    } catch (err) {
      lastErr = err;
      await sleepMs(250 * (attempt + 1));
    }
  }
  throw lastErr instanceof Error ? lastErr : new Error(String(lastErr));
}

interface LiveLoginSecrets {
  readonly accessToken: string;
  readonly refreshToken: string;
}

function readLiveLoginSecrets(): LiveLoginSecrets {
  const raw = process.env.CODEX_M3B_LIVE_LOGIN_JSON;
  if (!raw) throw new Error("live no-leak proof requires CODEX_M3B_LIVE_LOGIN_JSON in the lifecycle container");
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    throw new Error("live login JSON is invalid");
  }
  const record = rec(value);
  if (typeof record.access_token !== "string" || record.access_token.length === 0
    || typeof record.refresh_token !== "string" || record.refresh_token.length === 0) {
    throw new Error("live login JSON must contain non-empty access_token and refresh_token strings");
  }
  if (Object.keys(record).some((key) => key !== "access_token" && key !== "refresh_token")) {
    throw new Error("live login JSON contains an unsupported field");
  }
  return { accessToken: record.access_token, refreshToken: record.refresh_token };
}

function makeLiveRunPaths(scratch: string, label: string): { readonly worktree: string; readonly home: string } {
  const worktree = nodePath.join(scratch, `${label}-work`);
  fs.mkdirSync(worktree, { recursive: true, mode: 0o2770 });
  fs.chmodSync(worktree, 0o2770);
  // An injected launchProviderRoot owns its synthetic filesystem, so CodexExecutor deliberately
  // skips prepareCodexRunHome. Create the worker-owned shared ancestors the real launcher expects;
  // its runner-owned epoch mkdir is intentionally non-recursive and must not create these itself.
  const home = nodePath.join(scratch, `${label}-home`);
  const codexData = nodePath.join(home, "codex-data");
  fs.mkdirSync(codexData, { recursive: true, mode: 0o2770 });
  fs.chmodSync(home, 0o2770);
  fs.chmodSync(codexData, 0o2770);
  return { worktree, home };
}

function liveTimeoutMs(envName: string, fallback: number): number {
  const raw = process.env[envName];
  if (raw === undefined || raw.length === 0) return fallback;
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value <= 0) throw new Error(`${envName} must be a positive integer`);
  return value;
}

/** The LIVE subscription leg (PRD #1106 M3b live-acceptance). Selected only when CODEX_M3B_LIVE=1;
 *  the offline body is untouched. It gates one real tool-less advice pass, a run cancellation after
 *  observed turn/start, root/action settlement, fresh release/fail-closed construction, and a broad
 *  non-disclosing no-leak oracle. Advice text and the complete model run's content remain non-gating.
 *  The token-rotating sequential/check-b refresh probes run only under LIVE_REFRESH_PROBES; default
 *  live acceptance does not execute or require them. */
async function runLiveSubscription(): Promise<void> {
  const contract = readServerContract();
  assert.ok(contract, "live subscription requires the server contract (the real WorkerClient); is the test server seeding live?");
  const c = contract as NonNullable<typeof contract>;
  const liveLogin = readLiveLoginSecrets();

  const log = recordingLog();
  const WorkerClient = await loadWorkerClientCtor();
  // Default to the PRODUCTION worker budget (8s codexHTTPTimeoutMs) so pass/fail is production-
  // equivalent; the server keeps its production 7.5s route / 7s lease + 2.5s per-provider-call cap.
  // CODEX_M3B_LIVE_RELAX_TIMEOUTS=1 relaxes the client to 30s (and drops the server per-call cap) as
  // an explicit diagnostic mode only.
  const relaxTimeouts = process.env.CODEX_M3B_LIVE_RELAX_TIMEOUTS === "1";
  const realClient = new WorkerClient(c.baseUrl, c.workerToken, "codex-m3b", log.log, {
    codexHTTPTimeoutMs: relaxTimeouts ? 30_000 : 8_000,
    httpTimeoutMs: 30_000,
  });
  const counts: ClientCounts = { release: 0, refresh: 0, refreshAdvanced: 0, refreshReplayed: 0 };
  const capturedTokens: string[] = [];
  // Count releases/refresh outcomes AND capture every plaintext token for the no-leak proof.
  const client = tokenCapturingClient(countingWorkerClient(realClient, counts, new Set<string>()), capturedTokens);

  const binding = subscriptionBindingFromContract(c);
  const account = c.sub.account;
  const runId = c.sub.runId;
  const liveBaseUrl = process.env.CODEX_M3B_LIVE_BASE_URL ?? LIVE_BASE_URL_FALLBACK;
  const liveProvider: CodexProviderConfig = { ...provider, baseUrl: liveBaseUrl };
  const sessionOps: SessionOps = {
    adopt: 0,
    adoptFiles: 0,
    persist: 0,
    persistFiles: 0,
    persistFailures: 0,
    inspect: 0,
    remove: 0,
  };
  const { scratch, worktree, home } = makeScratch();
  const cancelPaths = makeLiveRunPaths(scratch, "cancel");
  const adviceEvidence = liveLaunchEvidence();
  const cancelLatch = liveTurnStartLatch();
  const cancelEvidence = liveLaunchEvidence(cancelLatch);
  const evidences = [adviceEvidence, cancelEvidence] as const;
  const emittedAll: EmittedMessage[] = [];
  const adviceMessages: string[] = [];

  const depsFor = (evidence: LiveLaunchEvidence): CodexExecutorDeps => ({
    sessionStore: countingSessionStore(sessionOps),
    deferRegistryTeardown: true,
    launchProviderRoot: liveProviderLaunchSeam(evidence),
    launchEffectRoot: liveEffectLaunchSeam(evidence),
    idleMs: 60_000,
    wallMs: 300_000,
    boundaryDeadlineMs: 5000,
    childTurnDeadlineMs: 60_000,
    commandTmpdir: process.env.TMPDIR ?? "/tmp",
  });
  // Keep the complete run on the literal production default launcher path. The injected adapter is
  // only for the new advice seam and the single-epoch cancellation observation; using it for a
  // plan-approval root recreation would bypass the production session-staging branch.
  const fullRunDeps: CodexExecutorDeps = {
    sessionStore: countingSessionStore(sessionOps),
    deferRegistryTeardown: true,
    idleMs: 60_000,
    wallMs: 300_000,
    boundaryDeadlineMs: 5000,
    childTurnDeadlineMs: 60_000,
    commandTmpdir: process.env.TMPDIR ?? "/tmp",
  };

  let fullExec: InstanceType<typeof CodexExecutor> | undefined;
  let cancelExec: InstanceType<typeof CodexExecutor> | undefined;
  let fullTerminalDisposed = false;
  let cancelTerminalDisposed = false;
  let ran = false;
  let adviceCompleted = false;
  let adviceReleaseFailClosed = false;
  let advicePolicyCalls = 0;
  let adviceReleaseCount = 0;
  let cancelWon = false;
  let noLeak = false;
  let refreshProbeAdvanced = false;
  let refreshProbeReplayed = false;
  let checkBSingleStep = false;
  let checkBConverged = false;

  try {
    // (1) One REAL subscription advice pass. Production advice stays unchanged: the test-only
    // launch seam composes the packaged launchCodexRoot + createCodexTransport exactly like the
    // production run launcher, while makeCodexAdviceHarness owns fresh release + app-server auth.
    const adviceLaunch = liveAdviceLaunchSeam(scratch, adviceEvidence);
    const releaseBeforeAdvice = counts.release;
    const adviceBridge = new CodexAdviceCredentialBridge(runId, client, binding);
    const adviceHarness = await withStaticLiveFailure(
      makeCodexAdviceHarness(adviceBridge, liveProvider, adviceLaunch, log.log),
      "live advice construction failed closed",
    );
    adviceReleaseCount = counts.release - releaseBeforeAdvice;
    assert.equal(adviceReleaseCount, 1, "advice construction released exactly one fresh committed token");
    assert.equal(adviceEvidence.providerLaunched, 0, "advice construction performs no model work before run()");

    let policyTerminal: HarnessTerminal | undefined;
    let policySawError: boolean | undefined;
    const advicePolicy: AdviceResultPolicy = {
      onTerminal: (terminal, context) => {
        advicePolicyCalls += 1;
        policyTerminal = terminal;
        policySawError = context.isError;
        if (context.latest !== undefined) throw new Error("Codex advice unexpectedly fabricated rate-limit evidence");
      },
    };
    const adviceController = new AbortController();
    const adviceTimeoutMs = liveTimeoutMs("CODEX_M3B_LIVE_ADVICE_TIMEOUT_MS", 180_000);
    const adviceRequest: AdviceRequest = {
      label: "summary",
      systemPrompt: "Return a short plain-text summary. Do not use tools or mention runtime metadata.",
      prompt: "Summarize this sentence: the live advice lifecycle completed.",
      model: liveProvider.model,
      output: { kind: "text" },
      signal: adviceController.signal,
      timeoutMs: adviceTimeoutMs,
    };
    const adviceResult = await withStaticLiveFailure(
      withTimeout(
        adviceHarness.run(adviceRequest, advicePolicy),
        adviceTimeoutMs + 10_000,
        "live subscription advice pass",
      ),
      "live advice pass failed closed",
    );
    adviceMessages.push(adviceResult.text);
    assert.equal(adviceResult.end.kind, "terminal", "the real advice pass reached a terminal");
    if (adviceResult.end.kind !== "terminal") throw new Error("the real advice pass did not return a terminal");
    assert.equal(adviceResult.end.terminal.outcome, "success", "the real advice terminal succeeded");
    assert.equal(advicePolicyCalls, 1, "the advice policy ran exactly once before cleanup");
    assert.equal(policyTerminal, adviceResult.end.terminal, "the advice policy observed the returned terminal");
    assert.equal(policySawError, false, "the successful advice terminal was not classified as an error");
    assertAdviceCeiling(adviceEvidence);
    assert.equal(adviceEvidence.providerLaunched, 1, "the advice pass launched one isolated provider root");
    assert.equal(adviceEvidence.providerSettled, 1, "the advice pass settled its isolated provider root");
    assert.equal(adviceEvidence.transportClosed, 1, "the advice pass closed its app-server transport");
    assert.equal(adviceEvidence.actionLaunched, 0, "the advice pass launched no command/action root");
    assert.equal(adviceEvidence.disposalFailures, 0, "the advice pass had no root-disposal failure");
    adviceCompleted = true;

    // A deterministic negative beside the live pass pins factory fail-closed behavior: release
    // authority fails before launch, so no fallback/uncredentialed advice root can be constructed.
    const deniedMessage = "test-only advice release denied";
    const deniedClient = {
      releaseCodex: async () => { throw new Error(deniedMessage); },
      refreshCodex: async () => { throw new Error("test-only advice refresh must not run"); },
    };
    const deniedBridge = new CodexAdviceCredentialBridge(runId, deniedClient as never, binding);
    const launchesBeforeDeniedRelease = adviceEvidence.providerLaunched;
    await assert.rejects(
      makeCodexAdviceHarness(deniedBridge, liveProvider, adviceLaunch, log.log),
      (error: unknown) => error instanceof Error && error.message === deniedMessage,
    );
    assert.equal(
      adviceEvidence.providerLaunched,
      launchesBeforeDeniedRelease,
      "a denied advice release launched no provider root",
    );
    adviceReleaseFailClosed = true;

    // (2) A REAL subscription cancellation after a successful turn/start. The observing adapter
    // holds that response until the external abort fires, making the precedence deterministic:
    // owner cancel must win over a queued provider terminal or raw transport close.
    const cancelController = new AbortController();
    const { ctx: cancelCtx, emitted: cancelEmitted } = makeCtx({
      runId,
      worktreePath: cancelPaths.worktree,
      planApproved: true,
      approvedPlan: "exercise cancellation only",
      agents,
      checkpoint: async () => {},
      signal: cancelController.signal,
    });
    cancelExec = new CodexExecutor(
      log.log,
      cancelPaths.home,
      { binding, client, provider: liveProvider },
      depsFor(cancelEvidence),
    );
    const releasesBeforeCancel = counts.release;
    const cancelRun = cancelExec.run(cancelCtx);
    const prematureCancelSettlement = cancelRun.then(
      () => { throw new Error("live cancel run completed before turn/start was observed"); },
      () => {
        const lastMethod = cancelEvidence.requests.at(-1)?.method;
        const safeLastMethod = lastMethod === "initialize"
          || lastMethod === "account/login/start"
          || lastMethod === "thread/start"
          || lastMethod === "turn/start"
          ? lastMethod
          : "none-or-other";
        const stage = counts.release === releasesBeforeCancel
          ? "before-release"
          : cancelEvidence.providerLaunched === 0
            ? "after-release-before-provider"
            : cancelEvidence.turnStarts === 0
              ? "after-provider-before-turn-start"
              : "after-turn-start";
        throw new Error(`live cancel run failed before turn/start was observed (${stage}; last=${safeLastMethod})`);
      },
    );
    await withTimeout(
      Promise.race([cancelLatch.observed, prematureCancelSettlement]),
      120_000,
      "live cancel turn/start admission",
    );
    cancelController.abort();
    cancelLatch.release();
    let cancelError: unknown;
    try {
      await withTimeout(cancelRun, 120_000, "live cancel settlement");
    } catch (error) {
      cancelError = error;
    }
    if (!(cancelError instanceof Error) || cancelError.message !== "run cancelled") {
      throw new Error("live owner cancellation did not win after observed turn/start");
    }
    cancelWon = true;
    for (const message of cancelEmitted) emittedAll.push(message);

    const cancelSafety = cancelExec.safety;
    assert.ok(cancelSafety, "the live cancel run constructed the Codex safety owner");
    const cancelDisposal = await cancelSafety.dispose({ boundary: "terminal", deadlineMs: 5000 });
    assert.equal(cancelDisposal.kind, "disposed", "terminal dispose settled registered roots and actions after cancel");
    cancelTerminalDisposed = true;
    assert.ok(cancelEvidence.turnStarts > 0, "cancel fired only after a real turn/start succeeded");
    assert.ok(cancelEvidence.providerLaunched > 0, "the cancel pass launched a real provider root");
    assert.ok(cancelEvidence.actionLaunched > 0, "the cancel pass launched its registered command/file action root");
    assert.equal(
      cancelEvidence.providerSettled,
      cancelEvidence.providerLaunched,
      "terminal dispose settled every cancel provider root",
    );
    assert.equal(
      cancelEvidence.actionSettled,
      cancelEvidence.actionLaunched,
      "terminal dispose settled every cancel action root",
    );
    assert.equal(
      cancelEvidence.transportClosed,
      cancelEvidence.providerLaunched,
      "cancel cleanup closed every provider transport",
    );
    assert.equal(cancelEvidence.disposalFailures, 0, "cancel cleanup had no root-disposal failure");

    // (3) Attempt the complete real-model run unless the maintainer opts out. Its content/tool
    // choices remain non-gating; released tokens and request evidence still join the no-leak proof.
    if (process.env.CODEX_M3B_LIVE_SKIP_EXEC === "1") {
      console.log("CODEX_M3B_LIVE complete run skipped; advice + cancel + no-leak gates still run");
    } else {
      const { ctx, emitted } = makeCtx({
        runId,
        worktreePath: worktree,
        planApproved: false,
        approvedPlan: undefined,
        gatePlan: async () => ({ kind: "approve", selection: { source: "own", agents: [] } }) as never,
        agents,
        checkpoint: async () => {},
      });
      const runTimeoutMs = liveTimeoutMs("CODEX_M3B_LIVE_RUN_TIMEOUT_MS", 300_000);
      fullExec = new CodexExecutor(
        log.log,
        home,
        { binding, client, provider: liveProvider },
        fullRunDeps,
      );
      try {
        await withTimeout(fullExec.run(ctx), runTimeoutMs, "live subscription complete run");
        ran = true;
      } catch {
        ran = false;
      }
      for (const message of emitted) emittedAll.push(message);
      // These sinks mirror the real runner. They remain non-gating with the complete model run,
      // but cleanup is attempted and all resulting surfaces are included in the no-leak proof.
      try {
        if (fullExec.safety) {
          await fullExec.safety.withBoundary({ boundary: "finalize", deadlineMs: 5000 }, async () => {});
        }
      } catch {
        // Non-gating complete run; deterministic advice/cancel gates already passed.
      }
      try {
        if (fullExec.safety) {
          const disposal = await fullExec.safety.dispose({ boundary: "terminal", deadlineMs: 5000 });
          fullTerminalDisposed = disposal.kind === "disposed";
        }
      } catch {
        fullTerminalDisposed = false;
      }
    }

    // (4) Optional refresh-rotation acceptance. DEFAULT OFF: neither sequential advance/replay nor
    // check-b runs unless the maintainer explicitly sets CODEX_M3B_LIVE_REFRESH_PROBES=1.
    if (LIVE_REFRESH_PROBES) {
      const g0 = await (async (): Promise<number> => {
        const release = await client.releaseCodex(
          runId,
          { capability: c.sub.capability },
          { authMode: "subscription", chatgptAccountId: account, minimumGeneration: 0 },
        );
        assert.equal(release.auth_mode, "subscription", "the live refresh probe released a subscription token");
        return release.auth_mode === "subscription" ? release.generation : 0;
      })();
      const sequentialOperation = randomUUID();
      const advanced = await refreshWithRetry(
        client,
        runId,
        c.sub.capability,
        account,
        sequentialOperation,
        g0,
      );
      assert.equal(advanced.outcome, "advanced", "a fresh refresh operation advances exactly once");
      assert.equal(advanced.generation, g0 + 1, "the sequential refresh advanced one generation");
      const replayed = await refreshWithRetry(
        client,
        runId,
        c.sub.capability,
        account,
        sequentialOperation,
        g0,
      );
      assert.equal(replayed.outcome, "replayed", "the retained refresh operation replays");
      if (replayed.access_token !== advanced.access_token) {
        throw new Error("the retained refresh operation did not replay the committed access token");
      }
      assert.equal(replayed.generation, advanced.generation, "the retained refresh operation replayed the committed generation");
      refreshProbeAdvanced = true;
      refreshProbeReplayed = true;

      const releaseB = await client.releaseCodex(
        runId,
        { capability: c.sub.capability },
        { authMode: "subscription", chatgptAccountId: account, minimumGeneration: 0 },
      );
      const generationB = releaseB.auth_mode === "subscription" ? releaseB.generation : 0;
      const [resultA, resultB] = await Promise.all([
        refreshWithRetry(client, runId, c.sub.capability, account, randomUUID(), generationB),
        refreshWithRetry(client, runId, c.sub.capability, account, randomUUID(), generationB),
      ]);
      const oneAdvanced = [resultA, resultB].filter((result) => result.outcome === "advanced").length === 1;
      const releaseAfter = await client.releaseCodex(
        runId,
        { capability: c.sub.capability },
        { authMode: "subscription", chatgptAccountId: account, minimumGeneration: 0 },
      );
      const committedAfter = releaseAfter.auth_mode === "subscription" ? releaseAfter.generation : -1;
      checkBSingleStep = oneAdvanced
        && committedAfter === generationB + 1
        && resultA.generation === generationB + 1
        && resultB.generation === generationB + 1;
      checkBConverged = resultA.generation === resultB.generation
        && resultA.access_token === resultB.access_token;
      assert.equal(checkBSingleStep, true, "check-b committed exactly one generation advance");
      assert.equal(checkBConverged, true, "check-b converged on one committed token and generation");
    }

    // (5) Structural auth + no-leak gates. The actual access/refresh tokens, capability, account id,
    // endpoint coordinates, and every observed session/thread/turn id are checked across messages,
    // logs, and request evidence. Failures reveal only a static category/surface, never a value.
    assertSubscriptionLoginShape(adviceEvidence, account, capturedTokens);
    assertSubscriptionLoginShape(cancelEvidence, account, capturedTokens);
    assertLiveNoLeak({
      evidences,
      emitted: emittedAll,
      additionalMessages: adviceMessages,
      log,
      tokens: [liveLogin.accessToken, liveLogin.refreshToken, c.workerToken, ...capturedTokens],
      authForbiddenTokens: [liveLogin.refreshToken, c.workerToken],
      capability: c.sub.capability,
      accountId: account,
      endpoints: [liveBaseUrl, c.baseUrl],
    });
    noLeak = true;

    // (6) Sanitized machine summary. It contains booleans/counts only. The parser always gates
    // advice, cancel, release and no-leak; refresh fields gate only when refreshProbesEnabled=true.
    const liveCounts = {
      ran,
      fullTerminalDisposed,
      releases: counts.release,
      adviceCompleted,
      adviceReleaseCount,
      adviceReleaseFailClosed,
      advicePolicyCalls,
      adviceRoots: adviceEvidence.providerLaunched,
      adviceRootsSettled: adviceEvidence.providerLaunched === adviceEvidence.providerSettled,
      adviceTransportClosed: adviceEvidence.transportClosed === adviceEvidence.providerLaunched,
      adviceActions: adviceEvidence.actionLaunched,
      adviceActionsZero: adviceEvidence.actionLaunched === 0,
      cancelTurnStarts: cancelEvidence.turnStarts,
      cancelWon,
      cancelTerminalDisposed,
      cancelProviderRoots: cancelEvidence.providerLaunched,
      cancelRootsSettled: cancelEvidence.providerLaunched === cancelEvidence.providerSettled,
      cancelTransportsClosed: cancelEvidence.transportClosed === cancelEvidence.providerLaunched,
      cancelActions: cancelEvidence.actionLaunched,
      cancelActionsSettled: cancelEvidence.actionLaunched === cancelEvidence.actionSettled,
      noLeak,
      refreshProbesEnabled: LIVE_REFRESH_PROBES,
      refreshProbeAdvanced,
      refreshProbeReplayed,
      checkBSingleStep,
      checkBConverged,
    };
    console.log(`CODEX_M3B_LIVE_SUB_COUNTS ${JSON.stringify(liveCounts)}`);

    assert.ok(counts.release >= 2, "the advice and cancel gates each released a real committed token");
    if (!ran && process.env.CODEX_M3B_LIVE_SKIP_EXEC !== "1") {
      console.log("CODEX_M3B_LIVE complete run did not finish; its content remains non-gating");
    }
  } finally {
    cancelLatch.release();
    if (!cancelTerminalDisposed && cancelExec?.safety) {
      await cancelExec.safety.dispose({ boundary: "terminal", deadlineMs: 5000 }).catch(() => undefined);
    }
    if (!fullTerminalDisposed && fullExec?.safety) {
      await fullExec.safety.dispose({ boundary: "terminal", deadlineMs: 5000 }).catch(() => undefined);
    }
    try {
      fs.rmSync(scratch, { recursive: true, force: true });
    } catch {
      // Best-effort: a model run's uid-10003 command files can be unremovable by this uid (EACCES).
      // The pod is throwaway, so a leftover scratch dir never affects the proof.
    }
  }
}

describe("codex-m3b packaged lifecycle (real launch → loopback provider)", { skip: PACKAGED ? false : "image-only (set CODEX_M3B_PACKAGED=1 inside the worker image)" }, () => {
  it("subscription: drives the packaged CodexExecutor through the REAL supervisor/fileop/codex + real WorkerClient with NONZERO real-path counts and no canary leak", async () => {
    // LIVE mode drives the REAL provider via a wholly separate body (no fake). When CODEX_M3B_LIVE
    // is unset this guard is inert and the offline body below is byte-for-byte unchanged.
    if (LIVE) {
      await runLiveSubscription();
      return;
    }
    const { FakeProvider, recordingLifecycleResponder, countToolCallbacks, toolCallbackTexts, codexCanaries: freshCanaries } = await import("./fake-provider.js");
    const canaries = freshCanaries();
    const contract = readServerContract();
    const allowedBearerDigests = new Set<string>();
    if (contract === undefined) {
      allowedBearerDigests.add(bearerDigest(canaries.credential));
      allowedBearerDigests.add(bearerDigest(`${canaries.credential}-refreshed`));
    }
    // The real-server path populates this set only from successful WorkerClient release/refresh
    // results before Codex can present them. The fallback's known fake responses are seeded above.
    const { respond, evidence } = recordingLifecycleResponder(canaries);
    const fake = await FakeProvider.start({ allowedBearerDigests, respond });

    // Wire the client: REAL WorkerClient against the throwaway Postgres when the contract is
    // present, else the in-memory fake client (still exercises the real launcher).
    const counts: ClientCounts = { release: 0, refresh: 0, refreshAdvanced: 0, refreshReplayed: 0 };
    let realClient: WorkerClientInstance | undefined;
    const log = recordingLog();
    let client: WorkerClientInstance;
    let binding: CodexBinding;
    let runId: string;
    if (contract) {
      const WorkerClient = await loadWorkerClientCtor();
      realClient = new WorkerClient(contract.baseUrl, contract.workerToken, "codex-m3b", log.log);
      client = countingWorkerClient(realClient, counts, allowedBearerDigests);
      binding = subscriptionBindingFromContract(contract);
      runId = contract.sub.runId;
    } else {
      const rig = makeRig(canaries);
      client = rig.client as never;
      binding = bindingOf(subscriptionBlock(canaries, "claim"));
      runId = "run-1";
    }

    const sessionOps: SessionOps = { adopt: 0, adoptFiles: 0, persist: 0, persistFiles: 0, persistFailures: 0, inspect: 0, remove: 0 };
    // deferRegistryTeardown keeps the provider registry ALIVE across run() so the post-run
    // finalize boundary can run the executor's OWN auth-mode reconcile (a subscription refresh
    // over the real route) and reap the live provider root.
    const realDeps: CodexExecutorDeps = {
      sessionStore: countingSessionStore(sessionOps),
      deferRegistryTeardown: true,
      appServerAuthOpenAIBaseUrlForTest: fake.baseUrl,
      idleMs: 20000,
      wallMs: 60000,
      boundaryDeadlineMs: 3000,
      childTurnDeadlineMs: 15000,
      commandTmpdir: process.env.TMPDIR ?? "/tmp",
    };
    const loopbackProvider: CodexProviderConfig = { ...provider, baseUrl: fake.baseUrl };
    const { scratch, worktree, home } = makeScratch();
    const markerName = canaries.patchArg.replace(/[^a-z-]+/g, "-");

    let checkpoints = 0;
    let planGates = 0;
    const { ctx, emitted } = makeCtx({
      runId,
      worktreePath: worktree,
      planApproved: false,
      approvedPlan: undefined,
      gatePlan: async () => {
        planGates += 1;
        return { kind: "approve", selection: { source: "own", agents: [] } } as never;
      },
      agents,
      checkpoint: async () => {
        checkpoints += 1;
      },
    });

    let exec: InstanceType<typeof CodexExecutor> | undefined;
    let ran = true;
    let ranError = "";
    let finalizeReaped = false;
    let terminalDisposed = false;
    try {
      exec = new CodexExecutor(log.log, home, { binding, client, provider: loopbackProvider }, realDeps);
      try {
        await withTimeout(exec.run(ctx), 55000, "packaged subscription run");
      } catch (err) {
        ran = false;
        ranError = err instanceof Error ? err.message : String(err);
      }
      // The runner's post-run durability sinks: withBoundary runs the subscription reconcile
      // (a refresh over the REAL route → advance) then reaps the live provider root; the terminal
      // dispose tears down every registered root (provider + command). Both wrapped so a real
      // process is always reaped even if the run threw.
      try {
        if (exec.safety) {
          await exec.safety.withBoundary({ boundary: "finalize", deadlineMs: 3000 }, async () => {});
          finalizeReaped = true;
        }
      } catch {
        finalizeReaped = false;
      }
      try {
        if (exec.safety) {
          await exec.safety.dispose({ boundary: "terminal", deadlineMs: 3000 });
          terminalDisposed = true;
        }
      } catch {
        terminalDisposed = false;
      }

      const refreshProbeOutcomes: string[] = [];
      // Direct advance+replay probe over the REAL refresh route (subscription): read the current
      // committed generation, advance once with a fresh operation id (→ "advanced"), then re-send
      // the SAME operation id + observed generation (→ "replayed"). Counted via the proxy. Server
      // path only — the fake client cannot exercise the coordinated-refresh replay dedup.
      if (contract && realClient) {
        try {
          const probe = countingWorkerClient(realClient, counts, allowedBearerDigests);
          const rel = await probe.releaseCodex(
            contract.sub.runId,
            { capability: contract.sub.capability },
            { authMode: "subscription", chatgptAccountId: contract.sub.account, minimumGeneration: 0 },
          );
          if (rel.auth_mode === "subscription") {
            const g0 = rel.generation;
            // The worker route strictly parses operation_id as a UUID. A descriptive
            // arbitrary string is rejected before CoordinatedCodexRefresh and makes the
            // probe's catch look like a missing replay.
            const op = randomUUID();
            const advanced = await probe.refreshCodex(
              contract.sub.runId,
              { capability: contract.sub.capability, operation_id: op, observed_generation: g0 },
              { authMode: "subscription", chatgptAccountId: contract.sub.account },
            );
            refreshProbeOutcomes.push(advanced.outcome);
            const replayed = await probe.refreshCodex(
              contract.sub.runId,
              { capability: contract.sub.capability, operation_id: op, observed_generation: g0 },
              { authMode: "subscription", chatgptAccountId: contract.sub.account },
            );
            refreshProbeOutcomes.push(replayed.outcome);
          }
        } catch {
          // A probe failure leaves refreshReplayed at 0, which the nonzero assertion below reports.
        }
      }

      const markerExists = ((): boolean => {
        try {
          return fs.existsSync(nodePath.join(worktree, markerName));
        } catch {
          return false;
        }
      })();
      const distinctBearers = [...new Set(fake.observedBearers)];
      const checkpointCallbackTexts = toolCallbackTexts(fake.requests, "cc-ckpt");
      const patchCallbackTexts = toolCallbackTexts(fake.requests, "cc-patch");
      const bashCallbackTexts = toolCallbackTexts(fake.requests, "cc-bash");
      const patchAccepted = patchCallbackTexts.some((text) => text.includes('"written":true'));
      const bashAccepted = bashCallbackTexts.some((text) => text.includes('"code":0'));

      // The SEPARATE real-path counts. Each is derived from provider-visible evidence + the
      // observable executor seams (the fake provider's recorded requests/bearers, the recording
      // responder's self-reported stages, ctx.checkpoint calls, the counting WorkerClient, the
      // counting session store, and the on-disk fileop marker). Categories the fully-real launch
      // does not surface directly are derived from their strongest external proxy, noted inline.
      const realCounts = {
        ran,
        // login/auth: the real Codex presented the released credential as a Bearer here.
        login: distinctBearers.length,
        // provider turns: every /v1/responses POST the real app-server made.
        providerTurns: fake.requests.length,
        // callbacks: tool-call replies the broker executed and fed back to Codex.
        callbacks: countToolCallbacks(fake.requests),
        checkpointCallbacks: checkpointCallbackTexts.length,
        checkpointAccepted: checkpointCallbackTexts.some((text) => text.includes('"checkpoint":true')),
        patchAccepted,
        bashAccepted,
        // delegation: the provider observed the child thread's own user-task request.
        delegation: evidence.childTurns,
        // signals: submit_plan + signal_done (the run-completing root signal).
        submitPlanSignals: evidence.submitPlan,
        doneSignals: evidence.signalDone,
        planGates,
        // checkpoints: cooperative checkpoint reaches the runner ctx.checkpoint sink.
        checkpoints,
        sessionPersistFiles: sessionOps.persistFiles,
        sessionAdoptFiles: sessionOps.adoptFiles,
        sessionPersistFailures: sessionOps.persistFailures,
        sessionPersistError: sessionOps.lastPersistError,
        // provider roots registered: one releaseCodex per provider epoch (>=1; >=2 after a
        // cooperative-checkpoint new-root resume). In the fake-client fallback this reads the
        // fake's releaseCalls.
        providerRootsRegistered: contract ? counts.release : (client as unknown as { releaseCalls?: unknown[] }).releaseCalls?.length ?? 0,
        // provider roots reaped: the finalize boundary reaped the live provider root.
        providerRootsReaped: finalizeReaped ? 1 : 0,
        // command roots registered: the REAL openat2 fileop root returned an accepted write.
        commandRootsRegistered: patchAccepted ? 1 : 0,
        // command roots reaped: the terminal dispose tore down every registered root.
        commandRootsReaped: terminalDisposed ? 1 : 0,
        // refresh advance/replay over the real coordinated-refresh route.
        refreshAdvanced: contract ? counts.refreshAdvanced : (client as unknown as { refreshCalls?: unknown[] }).refreshCalls?.length ?? 0,
        refreshReplayed: counts.refreshReplayed,
        refreshProbeOutcomes,
        // finalization: the run reached its terminal cleanly.
        finalization: ran ? 1 : 0,
        evidence,
      };
      console.log(`CODEX_M3B_PACKAGED_REAL_COUNTS ${JSON.stringify(realCounts)}`);

      // A real-path exception FAILS (no more silently-logged ran).
      assert.ok(ran, `the packaged real-launch run completed cleanly (ran=${ran}${ranError ? `: ${ranError}` : ""})`);
      assert.ok(
        fake.requests.length > 0,
        "the real Codex app-server reached the in-container loopback provider",
      );
      assert.deepEqual(fake.errors, [], "the loopback fake saw no auth/transport errors");

      const requireNonzero: ReadonlyArray<readonly [string, number]> = [
        ["login", realCounts.login],
        ["providerTurns", realCounts.providerTurns],
        ["callbacks", realCounts.callbacks],
        ["delegation", realCounts.delegation],
        ["submitPlanSignals", realCounts.submitPlanSignals],
        ["doneSignals", realCounts.doneSignals],
        ["planGates", realCounts.planGates],
        ["checkpoints", realCounts.checkpoints],
        ["sessionPersistFiles", realCounts.sessionPersistFiles],
        ["sessionAdoptFiles", realCounts.sessionAdoptFiles],
        ["providerRootsRegistered", realCounts.providerRootsRegistered],
        ["providerRootsReaped", realCounts.providerRootsReaped],
        ["commandRootsRegistered", realCounts.commandRootsRegistered],
        ["commandRootsReaped", realCounts.commandRootsReaped],
        ["refreshAdvanced", realCounts.refreshAdvanced],
        ["finalization", realCounts.finalization],
      ];
      for (const [name, value] of requireNonzero) {
        assert.ok(value > 0, `real-path count ${name} must be > 0 (got ${value})`);
      }
      assert.equal(realCounts.checkpointAccepted, true, "the app-server delivered the accepted checkpoint result back to the provider");
      assert.equal(realCounts.bashAccepted, true, "the screened shell callback completed through its command supervisor root");
      assert.equal(realCounts.patchAccepted, true, "the openat2 fileop callback completed through its persistent command root");
      assert.equal(markerExists, true, "the accepted fileop callback left its marker in the worktree");
      if (contract) {
        assert.ok(realCounts.refreshReplayed > 0, `real-path count refreshReplayed must be > 0 (got ${realCounts.refreshReplayed})`);
        assert.deepEqual(realCounts.refreshProbeOutcomes, ["advanced", "replayed"], "the same valid operation id advances once and then replays");
      } else {
        console.log("CODEX_M3B_PACKAGED_REAL not-asserted: refreshReplayed (no server contract; the fake client cannot exercise the coordinated-refresh replay path)");
      }

      // The canary boundary (item 8 / part C): the credential canary is the token the REAL server
      // released (captured from the fake's login record); the capability canary is the contract
      // (or fallback) capability. Neither may ride a public message or a log line.
      const capabilityCanary = contract ? contract.sub.capability : canaries.capability;
      const emittedBlob = JSON.stringify(emitted);
      const logBlob = log.lines.join("\n");
      assert.match(
        emittedBlob,
        /m3b lifecycle turn finished/,
        "the real app-server agent-message item decoded into a public frame",
      );
      for (const cred of distinctBearers) {
        assert.doesNotMatch(emittedBlob, new RegExp(escapeRe(cred)), "released credential leaked into an emitted message");
        assert.doesNotMatch(logBlob, new RegExp(escapeRe(cred)), "released credential leaked into a log line");
      }
      assert.doesNotMatch(emittedBlob, new RegExp(escapeRe(capabilityCanary)), "capability canary leaked into an emitted message");
      assert.doesNotMatch(logBlob, new RegExp(escapeRe(capabilityCanary)), "capability canary leaked into a log line");
    } finally {
      // Backstop: always reap the real provider root/supervisor, then close the loopback fake and
      // remove the scratch tree.
      if (!terminalDisposed && exec?.safety) {
        await exec.safety.dispose({ boundary: "terminal", deadlineMs: 3000 }).catch(() => undefined);
      }
      await fake.close();
      fs.rmSync(scratch, { recursive: true, force: true });
    }
  });

  it("api_key: drives the packaged CodexExecutor with an api_key binding and makes ZERO refresh calls (a release re-authorizes, never a refresh)", { skip: LIVE ? "subscription-only in live mode" : false }, async () => {
    const { FakeProvider, recordingLifecycleResponder, codexCanaries: freshCanaries } = await import("./fake-provider.js");
    const canaries = freshCanaries();
    const contract = readServerContract();
    const allowedBearerDigests = new Set<string>();
    if (contract === undefined) allowedBearerDigests.add(bearerDigest(canaries.credential));
    const { respond } = recordingLifecycleResponder(canaries);
    const fake = await FakeProvider.start({ allowedBearerDigests, respond });

    const counts: ClientCounts = { release: 0, refresh: 0, refreshAdvanced: 0, refreshReplayed: 0 };
    const log = recordingLog();
    let client: WorkerClientInstance;
    let binding: CodexBinding;
    let runId: string;
    let fakeRefreshCalls: () => number = () => 0;
    if (contract) {
      const WorkerClient = await loadWorkerClientCtor();
      const realClient = new WorkerClient(contract.baseUrl, contract.workerToken, "codex-m3b", log.log);
      client = countingWorkerClient(realClient, counts, allowedBearerDigests);
      binding = apiKeyBindingFromContract(contract);
      runId = contract.apiKey.runId;
    } else {
      const rig = makeRig(canaries);
      client = rig.client as never;
      binding = bindingOf({ auth_mode: "api_key", access_token: canaries.credential, capability: canaries.capability });
      runId = "run-1";
      fakeRefreshCalls = () => rig.client.refreshCalls.length;
    }

    const sessionOps: SessionOps = { adopt: 0, adoptFiles: 0, persist: 0, persistFiles: 0, persistFailures: 0, inspect: 0, remove: 0 };
    const realDeps: CodexExecutorDeps = {
      sessionStore: countingSessionStore(sessionOps),
      deferRegistryTeardown: true,
      appServerAuthOpenAIBaseUrlForTest: fake.baseUrl,
      idleMs: 20000,
      wallMs: 60000,
      boundaryDeadlineMs: 3000,
      childTurnDeadlineMs: 15000,
      commandTmpdir: process.env.TMPDIR ?? "/tmp",
    };
    const loopbackProvider: CodexProviderConfig = { ...provider, baseUrl: fake.baseUrl };
    const { scratch, worktree, home } = makeScratch();

    const { ctx, emitted } = makeCtx({
      runId,
      worktreePath: worktree,
      planApproved: true,
      approvedPlan: "plan",
      agents,
      checkpoint: async () => {},
    });

    let exec: InstanceType<typeof CodexExecutor> | undefined;
    let ran = true;
    let ranError = "";
    let terminalDisposed = false;
    try {
      exec = new CodexExecutor(log.log, home, { binding, client, provider: loopbackProvider }, realDeps);
      try {
        await withTimeout(exec.run(ctx), 55000, "packaged api_key run");
      } catch (err) {
        ran = false;
        ranError = err instanceof Error ? err.message : String(err);
      }
      // The api_key boundary reconcile issues a fresh releaseCodex — never a refresh. Drive it
      // through the finalize boundary so "issues a release, never a refresh" is proven, then reap.
      try {
        if (exec.safety) await exec.safety.withBoundary({ boundary: "finalize", deadlineMs: 3000 }, async () => {});
      } catch {
        // recorded via the ran assertion / counts below
      }
      try {
        if (exec.safety) {
          await exec.safety.dispose({ boundary: "terminal", deadlineMs: 3000 });
          terminalDisposed = true;
        }
      } catch {
        terminalDisposed = false;
      }

      const refreshCount = contract ? counts.refresh : fakeRefreshCalls();
      const releaseCount = contract ? counts.release : (client as unknown as { releaseCalls?: unknown[] }).releaseCalls?.length ?? 0;
      const distinctBearers = [...new Set(fake.observedBearers)];
      console.log(`CODEX_M3B_PACKAGED_REAL_APIKEY_COUNTS ${JSON.stringify({ ran, login: distinctBearers.length, providerTurns: fake.requests.length, release: releaseCount, refresh: refreshCount })}`);

      assert.ok(ran, `the packaged api_key run completed cleanly (ran=${ran}${ranError ? `: ${ranError}` : ""})`);
      assert.ok(
        fake.requests.length > 0,
        "the real Codex app-server reached the in-container loopback provider",
      );
      assert.deepEqual(fake.errors, [], "the loopback fake saw no auth/transport errors");
      // The load-bearing api_key invariant: ZERO refresh calls across the whole run + boundary.
      assert.equal(refreshCount, 0, `an api_key run must make ZERO codex/refresh calls (got ${refreshCount})`);
      assert.ok(releaseCount > 0, `an api_key run releases at least once (got ${releaseCount})`);

      const capabilityCanary = contract ? contract.apiKey.capability : canaries.capability;
      const emittedBlob = JSON.stringify(emitted);
      const logBlob = log.lines.join("\n");
      for (const cred of distinctBearers) {
        assert.doesNotMatch(emittedBlob, new RegExp(escapeRe(cred)), "released credential leaked into an emitted message");
        assert.doesNotMatch(logBlob, new RegExp(escapeRe(cred)), "released credential leaked into a log line");
      }
      assert.doesNotMatch(emittedBlob, new RegExp(escapeRe(capabilityCanary)), "capability canary leaked into an emitted message");
      assert.doesNotMatch(logBlob, new RegExp(escapeRe(capabilityCanary)), "capability canary leaked into a log line");
    } finally {
      if (!terminalDisposed && exec?.safety) {
        await exec.safety.dispose({ boundary: "terminal", deadlineMs: 3000 }).catch(() => undefined);
      }
      await fake.close();
      fs.rmSync(scratch, { recursive: true, force: true });
    }
  });
});
