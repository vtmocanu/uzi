// PRD #1287 C3 — a shared driver for the REAL CodexExecutor over an in-memory scripted transport
// (D7 layer U: real construction/reducer policy, no app-server). It mirrors the injected-fakes
// composition of e2e/codex-m3b/lifecycle.test.ts Block A and agent/test/codex-executor.test.ts:
// the executor is real; only the launcher / command / fileop / session-store OS seams are injected,
// so the production run-builder / renderer / harness thread construction (thread/start,
// thread/resume, turn/start) is genuinely exercised and observable on the fake transport.
//
// C3 uses it to observe the trust construction across start, resume AND a subsequent turn.

import { PassThrough } from "node:stream";
import path from "node:path";
import { pathToFileURL } from "node:url";

import type { CodexExecutor as CodexExecutorT, CodexExecutorDeps } from "../../agent/src/codex/codex-executor.js";
import type { selectCodexBinding as selectCodexBindingT, CodexBinding } from "../../agent/src/codex/select.js";
import type { CodexLaunchRootResult, CodexProviderConfig } from "../../agent/src/codex/codex-harness.js";
import type { RegisteredRoot } from "../../agent/src/codex/registry.js";
import type { FileopHelperHandle } from "../../agent/src/codex/fileop-client.js";
import type { CodexNotification, CodexTransport } from "../../agent/src/codex/transport.js";
import type { RunContext, EmittedMessage } from "../../agent/src/executor.js";
import type { Logger } from "../../agent/src/log.js";
import type { CodexEffectLaunchSpec, CodexRootHandle } from "../../agent/src/codex/launcher.js";

function srcDir(): string {
  const override = process.env.CODEX_M4_SRC;
  if (override && override.trim().length > 0) return override;
  return path.resolve(process.cwd(), "src");
}
async function load<T>(relative: string): Promise<T> {
  return (await import(pathToFileURL(`${srcDir()}/${relative}`).href)) as T;
}

export interface ExecModules {
  CodexExecutor: typeof CodexExecutorT;
  selectCodexBinding: typeof selectCodexBindingT;
}
export async function loadExecModules(): Promise<ExecModules> {
  const exec = await load<typeof import("../../agent/src/codex/codex-executor.js")>("codex/codex-executor.ts");
  const select = await load<typeof import("../../agent/src/codex/select.js")>("codex/select.ts");
  return { CodexExecutor: exec.CodexExecutor, selectCodexBinding: select.selectCodexBinding };
}

export const EXEC_WORKSPACE = "/work/repo";
export const EXEC_HOME_ROOT = "/data/agent-home/run-1";
const FRESH_TOKEN = "fresh-codex-access-token-XXXXXXXX";

const provider: CodexProviderConfig = {
  name: "openai",
  baseUrl: "http://127.0.0.1:9/v1",
  envKey: "OPENAI_API_KEY",
  model: "gpt-6-astra",
};

export const noopLog: Logger = {
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

function rec(v: unknown): Record<string, unknown> {
  return (v ?? {}) as Record<string, unknown>;
}

export interface ResponderCtx {
  transport: FakeExecTransport;
  method: string;
  params: unknown;
  threadStartCount: number;
  turnStartCount: number;
}
export type Responder = (c: ResponderCtx) => unknown;

/** An in-memory scriptable transport with the app-server auth handshake answered inline. */
export class FakeExecTransport implements CodexTransport {
  requests: { method: string; params: unknown; opts?: { signal?: AbortSignal } }[] = [];
  responses: { requestId: number | string; response: unknown }[] = [];
  notifies: { method: string; params: unknown }[] = [];
  closes = 0;
  threadStartCount = 0;
  turnStartCount = 0;

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
    if (method === "initialize") {
      return Promise.resolve({ userAgent: "codex/0.153.2", codexHome: "/owned/codex", platformFamily: "unix", platformOs: "linux" } as T);
    }
    if (method === "account/login/start") {
      return Promise.resolve({ type: rec(params).type } as T);
    }
    if (method === "thread/start") this.threadStartCount += 1;
    if (method === "turn/start") this.turnStartCount += 1;
    const c: ResponderCtx = { transport: this, method, params, threadStartCount: this.threadStartCount, turnStartCount: this.turnStartCount };
    try {
      return Promise.resolve(this.responder(c) as T);
    } catch (err) {
      return Promise.reject(err instanceof Error ? err : new Error(String(err)));
    }
  }

  notify(method: string, params?: unknown): void {
    this.notifies.push({ method, params });
  }

  installServerRequestInterceptor(): () => void {
    return () => undefined;
  }

  respond(requestId: number | string, response: { readonly result: unknown } | { readonly error: { readonly code: number; readonly message: string } }): void {
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
    return { next, return: () => Promise.resolve({ value: undefined, done: true }), [Symbol.asyncIterator]() { return this; } };
  }

  close(): Promise<void> {
    this.closes += 1;
    this.closedFlag = true;
    return Promise.resolve();
  }
}

// ─── notification builders ──────────────────────────────────────────────────────
export function threadStarted(threadId = "th-1"): CodexNotification {
  return { kind: "thread_started", method: "thread/started", threadId, params: { thread: { id: threadId } } };
}
export function turnCompleted(status = "completed", threadId = "th-1", turnId = "tn-1"): CodexNotification {
  return { kind: "turn_completed", method: "turn/completed", threadId, turnId, status, params: { threadId, turn: { id: turnId, status } } };
}
export function toolCall(requestId: number, tool: string, args: unknown, threadId: string, turnId: string, callId: string): CodexNotification {
  return { kind: "activity", method: "item/tool/call", requestId, params: { threadId, turnId, callId, tool, arguments: args } };
}
export function signalDone(threadId = "th-1", turnId = "tn-1", requestId = 700, callId = "c-done"): CodexNotification {
  return toolCall(requestId, "signal_done", {}, threadId, turnId, callId);
}

const SUBSCRIPTION = {
  auth_mode: "subscription",
  access_token: "claim-tok",
  capability: "run-cap",
  generation: 3,
  chatgpt_account_id: "verified-account",
  chatgpt_plan_type: null,
};

interface FakeClient {
  releaseCodex(runId: string, req: { capability: string }): Promise<{ access_token: string }>;
  refreshCodex(runId: string, req: { capability: string; operation_id: string; observed_generation: number }): Promise<{ access_token: string; generation: number; chatgpt_account_id: string; outcome: string }>;
}
function fakeClient(): FakeClient {
  let gen = 4;
  return {
    async releaseCodex() {
      return { access_token: FRESH_TOKEN };
    },
    async refreshCodex() {
      return { access_token: `${FRESH_TOKEN}-refreshed`, generation: gen++, chatgpt_account_id: "verified-account", outcome: "advanced" };
    },
  };
}

export interface ExecRig {
  transport: FakeExecTransport;
  spawnCommandCalls: { argv: readonly string[] }[];
  deps: CodexExecutorDeps;
  binding: CodexBinding;
}

/** Build a rig around one scripted transport, driving the REAL executor's construction. */
export function makeExecRig(mods: ExecModules, responder: Responder): ExecRig {
  const transport = new FakeExecTransport(responder);
  const root: RegisteredRoot = { kind: "provider", reap: async () => ({ ok: true }), dispose: async () => undefined };
  const spawnCommandCalls: ExecRig["spawnCommandCalls"] = [];
  const fileopHandle: FileopHelperHandle = { client: { op: async () => ({ ok: true }) }, dispose: async () => undefined };
  const deps: CodexExecutorDeps = {
    launchProviderRoot: async (): Promise<CodexLaunchRootResult> => ({ root, transport, supervisorPid: 1234 }),
    spawnCommand: async (argv) => {
      spawnCommandCalls.push({ argv });
      return { code: 0, stdout: "ok", stderr: "" };
    },
    launchEffectRoot: async (_spec: CodexEffectLaunchSpec): Promise<CodexRootHandle> => {
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
    },
    wireFileop: () => fileopHandle,
    sessionStore: {
      adopt: async () => ({ files: 1 }),
      inspect: async () => "absent",
      remove: async () => undefined,
      persist: async () => ({ files: 0, bytes: 0 }),
    },
    idleMs: 5000,
    wallMs: 5000,
    boundaryDeadlineMs: 200,
    childTurnDeadlineMs: 5000,
    commandTmpdir: "/run/runner-tmp",
  };
  const selection = mods.selectCodexBinding({ codex: SUBSCRIPTION });
  if (selection.kind !== "codex") throw new Error("expected a codex selection");
  return { transport, spawnCommandCalls, deps, binding: selection.binding };
}

export function buildExecutor(mods: ExecModules, rig: ExecRig): InstanceType<typeof CodexExecutorT> {
  return new mods.CodexExecutor(noopLog, EXEC_HOME_ROOT, { binding: rig.binding, client: fakeClient() as never, provider }, rig.deps);
}

export function makeCtx(overrides: Partial<RunContext> = {}): { ctx: RunContext; emitted: EmittedMessage[] } {
  const emitted: EmittedMessage[] = [];
  const ctx: RunContext = {
    runId: "run-1",
    issueIid: 42,
    issueTitle: "do a thing",
    issueDescription: "the description",
    worktreePath: EXEC_WORKSPACE,
    branch: "agent/issue-42",
    emit: (m) => emitted.push(m),
    planApproved: true,
    approvedPlan: "the approved plan",
    agents: [],
    ...overrides,
  };
  return { ctx, emitted };
}

export async function withTimeout<T>(p: Promise<T>, ms: number, label: string): Promise<T> {
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
