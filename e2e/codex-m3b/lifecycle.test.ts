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
//     against the REAL process surface. It is 🔴 MAINTAINER-VERIFIED (in-worker image builds
//     are storage-flaky / arm64-blocked, and the real app-server item framing is unconfirmed);
//     it is SKIPPED host-side.

import { before, describe, it } from "node:test";
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import fs from "node:fs";
import nodePath from "node:path";
import { PassThrough } from "node:stream";

import {
  loadPackagedCodexExecutor,
  loadPackagedSelect,
  type CodexExecutorModule,
  type SelectModule,
} from "./packaged-modules.js";
import { codexCanaries, type CodexCanaries } from "./fake-provider.js";

import type { CodexExecutorDeps } from "../../agent/src/codex/codex-executor.js";
import type { CodexBinding } from "../../agent/src/codex/select.js";
import type { CodexLaunchRootResult, CodexProviderConfig } from "../../agent/src/codex/codex-harness.js";
import type { RegisteredRoot } from "../../agent/src/codex/registry.js";
import type { FileopHelperHandle } from "../../agent/src/codex/fileop-client.js";
import type { CodexNotification, CodexTransport } from "../../agent/src/codex/transport.js";
import type { RunContext, EmittedMessage, Executor } from "../../agent/src/executor.js";
import type { Logger } from "../../agent/src/log.js";
import type { AgentTemplate, ClaimCodexSecrets } from "../../agent/src/protocol.js";
import type { CodexEffectLaunchSpec, CodexRootHandle } from "../../agent/src/codex/launcher.js";

// ─── the PACKAGED adapter, loaded once before any test (image /app/src, or the host
// source tree). A top-level `before` (not top-level await — the CommonJS-typed e2e tree
// forbids it) resolves them before the first `it` body runs. ────────────────────
let CodexExecutor: CodexExecutorModule["CodexExecutor"];
let FailClosedExecutor: CodexExecutorModule["FailClosedExecutor"];
let selectCodexBinding: SelectModule["selectCodexBinding"];
let CodexSelectionError: SelectModule["CodexSelectionError"];

before(async () => {
  const exec = await loadPackagedCodexExecutor();
  CodexExecutor = exec.CodexExecutor;
  FailClosedExecutor = exec.FailClosedExecutor;
  const sel = await loadPackagedSelect();
  selectCodexBinding = sel.selectCodexBinding;
  CodexSelectionError = sel.CodexSelectionError;
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
    await tick();
    controller.abort();
    await assert.rejects(withTimeout(p, 5000, "cancel run"), (e: Error) => {
      assert.equal(e.message, "run cancelled", "the cancel trip wins over the raw AbortError");
      assert.doesNotMatch(e.message, /AbortError/);
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
// app-server item framing + parent/child turn interleaving is 🔴 MAINTAINER-VERIFIED (unconfirmed
// in code); the recording responder always eventually drives a root `signal_done`, so a clean run
// reaches `ran === true`. The outer run-lifecycle.sh watchdog bounds a wedged real process.
// ================================================================================
const PACKAGED = process.env.CODEX_M3B_PACKAGED === "1";

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
  if (!baseUrl || !workerToken || !subRunId || !subCap || !subAccount || !subGenRaw || !apiRunId || !apiCap) {
    return undefined;
  }
  const generation = Number(subGenRaw);
  if (!Number.isSafeInteger(generation) || generation < 0) return undefined;
  return {
    baseUrl,
    workerToken,
    sub: { runId: subRunId, capability: subCap, account: subAccount, generation },
    apiKey: { runId: apiRunId, capability: apiCap },
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
function countingWorkerClient(real: WorkerClientInstance, counts: ClientCounts): WorkerClientInstance {
  return new Proxy(real, {
    get(target, prop, receiver) {
      if (prop === "releaseCodex") {
        return (...args: Parameters<WorkerClientInstance["releaseCodex"]>) => {
          counts.release += 1;
          return target.releaseCodex(...args);
        };
      }
      if (prop === "refreshCodex") {
        return async (...args: Parameters<WorkerClientInstance["refreshCodex"]>) => {
          counts.refresh += 1;
          const res = await target.refreshCodex(...args);
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
  persist: number;
  inspect: number;
  remove: number;
}
function countingSessionStore(ops: SessionOps): NonNullable<CodexExecutorDeps["sessionStore"]> {
  return {
    adopt: async () => {
      ops.adopt += 1;
      return { files: 0 };
    },
    inspect: async () => {
      ops.inspect += 1;
      return "absent";
    },
    remove: async () => {
      ops.remove += 1;
    },
    persist: async () => {
      ops.persist += 1;
      return { files: 0, bytes: 0 };
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

describe("codex-m3b packaged lifecycle (real launch → loopback provider)", { skip: PACKAGED ? false : "image-only (set CODEX_M3B_PACKAGED=1 inside the worker image)" }, () => {
  it("subscription: drives the packaged CodexExecutor through the REAL supervisor/fileop/codex + real WorkerClient with NONZERO real-path counts and no canary leak", async () => {
    const { FakeProvider, recordingLifecycleResponder, countToolCallbacks, codexCanaries: freshCanaries } = await import("./fake-provider.js");
    const canaries = freshCanaries();
    const contract = readServerContract();
    // Accept-any bearer + record it: on the real-server path the RELEASED token (the credential
    // canary) is unknown ahead of time, so the fake accepts whatever the real Codex presents and
    // records it for the boundary assertion.
    const { respond, evidence } = recordingLifecycleResponder(canaries);
    const fake = await FakeProvider.start({ acceptAnyBearer: true, respond });

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
      client = countingWorkerClient(realClient, counts);
      binding = subscriptionBindingFromContract(contract);
      runId = contract.sub.runId;
    } else {
      const rig = makeRig(canaries);
      client = rig.client as never;
      binding = bindingOf(subscriptionBlock(canaries, "claim"));
      runId = "run-1";
    }

    const sessionOps: SessionOps = { adopt: 0, persist: 0, inspect: 0, remove: 0 };
    // deferRegistryTeardown keeps the provider registry ALIVE across run() so the post-run
    // finalize boundary can run the executor's OWN auth-mode reconcile (a subscription refresh
    // over the real route) and reap the live provider root.
    const realDeps: CodexExecutorDeps = {
      sessionStore: countingSessionStore(sessionOps),
      deferRegistryTeardown: true,
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
    const { ctx, emitted } = makeCtx({
      runId,
      worktreePath: worktree,
      planApproved: true,
      approvedPlan: "plan",
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

      // Direct advance+replay probe over the REAL refresh route (subscription): read the current
      // committed generation, advance once with a fresh operation id (→ "advanced"), then re-send
      // the SAME operation id + observed generation (→ "replayed"). Counted via the proxy. Server
      // path only — the fake client cannot exercise the coordinated-refresh replay dedup.
      if (contract && realClient) {
        try {
          const probe = countingWorkerClient(realClient, counts);
          const rel = await probe.releaseCodex(
            contract.sub.runId,
            { capability: contract.sub.capability },
            { authMode: "subscription", chatgptAccountId: contract.sub.account, minimumGeneration: 0 },
          );
          if (rel.auth_mode === "subscription") {
            const g0 = rel.generation;
            const op = `codex-m3b-op-${randomBytes(9).toString("hex")}`;
            await probe.refreshCodex(
              contract.sub.runId,
              { capability: contract.sub.capability, operation_id: op, observed_generation: g0 },
              { authMode: "subscription", chatgptAccountId: contract.sub.account },
            );
            await probe.refreshCodex(
              contract.sub.runId,
              { capability: contract.sub.capability, operation_id: op, observed_generation: g0 },
              { authMode: "subscription", chatgptAccountId: contract.sub.account },
            );
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
        // delegation: a spawn_agent child turn was driven.
        delegation: evidence.spawn,
        // signals: submit_plan + signal_done (the run-completing root signal).
        submitPlanSignals: evidence.submitPlan,
        doneSignals: evidence.signalDone,
        // checkpoints: cooperative checkpoint reaches the runner ctx.checkpoint sink.
        checkpoints,
        // provider roots registered: one releaseCodex per provider epoch (>=1; >=2 after a
        // cooperative-checkpoint new-root resume). In the fake-client fallback this reads the
        // fake's releaseCalls.
        providerRootsRegistered: contract ? counts.release : (client as unknown as { releaseCalls?: unknown[] }).releaseCalls?.length ?? 0,
        // provider roots reaped: the finalize boundary reaped the live provider root.
        providerRootsReaped: finalizeReaped ? 1 : 0,
        // command roots registered: the REAL openat2 fileop root applied the patch marker on disk.
        commandRootsRegistered: markerExists ? 1 : 0,
        // command roots reaped: the terminal dispose tore down every registered root.
        commandRootsReaped: terminalDisposed ? 1 : 0,
        // refresh advance/replay over the real coordinated-refresh route.
        refreshAdvanced: contract ? counts.refreshAdvanced : (client as unknown as { refreshCalls?: unknown[] }).refreshCalls?.length ?? 0,
        refreshReplayed: counts.refreshReplayed,
        // finalization: the run reached its terminal cleanly.
        finalization: ran ? 1 : 0,
        evidence,
      };
      console.log(`CODEX_M3B_PACKAGED_REAL_COUNTS ${JSON.stringify(realCounts)}`);

      // A real-path exception FAILS (no more silently-logged ran).
      assert.ok(ran, `the packaged real-launch run completed cleanly (ran=${ran}${ranError ? `: ${ranError}` : ""})`);
      assert.deepEqual(fake.errors, [], "the loopback fake saw no auth/transport errors");

      const requireNonzero: ReadonlyArray<readonly [string, number]> = [
        ["login", realCounts.login],
        ["providerTurns", realCounts.providerTurns],
        ["callbacks", realCounts.callbacks],
        ["delegation", realCounts.delegation],
        ["submitPlanSignals", realCounts.submitPlanSignals],
        ["doneSignals", realCounts.doneSignals],
        ["checkpoints", realCounts.checkpoints],
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
      if (contract) {
        assert.ok(realCounts.refreshReplayed > 0, `real-path count refreshReplayed must be > 0 (got ${realCounts.refreshReplayed})`);
      } else {
        console.log("CODEX_M3B_PACKAGED_REAL not-asserted: refreshReplayed (no server contract; the fake client cannot exercise the coordinated-refresh replay path)");
      }

      // The canary boundary (item 8 / part C): the credential canary is the token the REAL server
      // released (captured from the fake's login record); the capability canary is the contract
      // (or fallback) capability. Neither may ride a public message or a log line.
      const capabilityCanary = contract ? contract.sub.capability : canaries.capability;
      const emittedBlob = JSON.stringify(emitted);
      const logBlob = log.lines.join("\n");
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

  it("api_key: drives the packaged CodexExecutor with an api_key binding and makes ZERO refresh calls (a release re-authorizes, never a refresh)", async () => {
    const { FakeProvider, recordingLifecycleResponder, codexCanaries: freshCanaries } = await import("./fake-provider.js");
    const canaries = freshCanaries();
    const contract = readServerContract();
    const { respond } = recordingLifecycleResponder(canaries);
    const fake = await FakeProvider.start({ acceptAnyBearer: true, respond });

    const counts: ClientCounts = { release: 0, refresh: 0, refreshAdvanced: 0, refreshReplayed: 0 };
    const log = recordingLog();
    let client: WorkerClientInstance;
    let binding: CodexBinding;
    let runId: string;
    let fakeRefreshCalls: () => number = () => 0;
    if (contract) {
      const WorkerClient = await loadWorkerClientCtor();
      const realClient = new WorkerClient(contract.baseUrl, contract.workerToken, "codex-m3b", log.log);
      client = countingWorkerClient(realClient, counts);
      binding = apiKeyBindingFromContract(contract);
      runId = contract.apiKey.runId;
    } else {
      const rig = makeRig(canaries);
      client = rig.client as never;
      binding = bindingOf({ auth_mode: "api_key", access_token: canaries.credential, capability: canaries.capability });
      runId = "run-1";
      fakeRefreshCalls = () => rig.client.refreshCalls.length;
    }

    const sessionOps: SessionOps = { adopt: 0, persist: 0, inspect: 0, remove: 0 };
    const realDeps: CodexExecutorDeps = {
      sessionStore: countingSessionStore(sessionOps),
      deferRegistryTeardown: true,
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
