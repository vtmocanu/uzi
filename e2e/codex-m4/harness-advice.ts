// PRD #1287 C4 — the shared U-layer driver for the Codex advice-ceiling conformance cases. These
// drive the REAL CodexAdviceHarness (render.ts renderCodexAdvice ceiling + disposeOnce + the
// api_key/subscription appserver-auth lane) with an in-memory scriptable transport and a fake
// LaunchAdviceRootSeam (a dispose spy). There is NO registry and NO broker in this lane — the
// tool-less ceiling by construction (D5).
//
// The production modules load through the SAME src-root resolution as packaged-modules.ts (host
// `agent/src` via `dir: agent`, or CODEX_M4_SRC → /app/src on an image/CI run). The FakeTransport
// is a local copy (the agent/test one is not importable across the e2e/agent boundary); its
// close() ENDS the notifications iterator so an auth-poison-triggered close unblocks `consume`.

import path from "node:path";
import { pathToFileURL } from "node:url";

import type * as AdviceModuleT from "../../agent/src/codex/codex-advice-harness.js";
import type * as AppServerAuthModuleT from "../../agent/src/codex/appserver-auth.js";
import type * as RenderModuleT from "../../agent/src/codex/render.js";
import type {
  CodexAdviceHarnessOptions,
  CodexAdviceLaunchResult,
  CodexAdviceLaunchSpec,
  LaunchAdviceRootSeam,
} from "../../agent/src/codex/codex-advice-harness.js";
import type { CodexProviderConfig } from "../../agent/src/codex/codex-harness.js";
import type { CodexAppServerAuthSession } from "../../agent/src/codex/appserver-auth.js";
import type { AdviceRequest, AdviceResultPolicy } from "../../agent/src/harness.js";
import type { CodexNotification, CodexTransport } from "../../agent/src/codex/transport.js";
import type { Logger } from "../../agent/src/log.js";

function srcDir(): string {
  const override = process.env.CODEX_M4_SRC;
  if (override && override.trim().length > 0) return override;
  return path.resolve(process.cwd(), "src");
}
async function load<T>(relative: string): Promise<T> {
  return (await import(pathToFileURL(`${srcDir()}/${relative}`).href)) as T;
}

/** The production modules the advice cases exercise directly, resolved once in a `before()`. */
export interface AdviceModules {
  advice: typeof AdviceModuleT;
  appServerAuth: typeof AppServerAuthModuleT;
  render: typeof RenderModuleT;
}

export async function loadAdviceModules(): Promise<AdviceModules> {
  const advice = await load<typeof AdviceModuleT>("codex/codex-advice-harness.ts");
  const appServerAuth = await load<typeof AppServerAuthModuleT>("codex/appserver-auth.ts");
  const render = await load<typeof RenderModuleT>("codex/render.ts");
  return { advice, appServerAuth, render };
}

export const provider: CodexProviderConfig = {
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

export function defaultResponder(method: string): unknown {
  if (method === "thread/start") return { thread: { id: "th-1" } };
  if (method === "turn/start") return { turn: { id: "tn-1" } };
  return {};
}

/** Answer the pinned app-server auth handshake (initialize/login) so a real
 *  CodexAppServerAuthSession can authenticate over the fake transport. */
export function authResponder(method: string, params: unknown): unknown {
  if (method === "initialize") {
    return { userAgent: "codex/0.153.2", codexHome: "/owned/codex", platformFamily: "unix", platformOs: "linux" };
  }
  if (method === "account/login/start") {
    return { type: (params as { type?: unknown } | undefined)?.type };
  }
  return defaultResponder(method);
}

/** A scriptable in-memory {@link CodexTransport}: request/respond/notify are captured, requests
 *  answer from an injected responder, and `notifications()` drains a queue the test pre-loads via
 *  push()/end(). close() also ENDS the iterator so an auth-poison-triggered close unblocks the
 *  advice consume loop (the pinned production transport ends notifications on a clean close). */
export class FakeTransport implements CodexTransport {
  requests: { method: string; params: unknown }[] = [];
  responses: { requestId: number | string; response: unknown }[] = [];
  notifies: { method: string; params: unknown }[] = [];
  closes = 0;

  private readonly queue: CodexNotification[] = [];
  private ended = false;
  private waiter: ((r: IteratorResult<CodexNotification>) => void) | undefined;
  private consumed = false;
  private closedFlag = false;
  private interceptor?: (note: CodexNotification, frameBytes: number) => boolean;
  onRespond?: (requestId: number | string, response: unknown) => void;

  constructor(private readonly responder: (method: string, params: unknown) => unknown = defaultResponder) {}

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

  /** Emit a server→client request through the installed interceptor (the auth refresh pump). */
  emitServerRequest(note: CodexNotification): boolean {
    const requestId = note.kind === "activity" ? note.requestId : undefined;
    const frameBytes = Buffer.byteLength(
      JSON.stringify({ id: requestId, method: note.method, params: note.params }),
      "utf8",
    );
    if (this.interceptor?.(note, frameBytes)) return true;
    this.push(note);
    return false;
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

  request<T = unknown>(method: string, params?: unknown): Promise<T> {
    this.requests.push({ method, params });
    return Promise.resolve(this.responder(method, params) as T);
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
    this.onRespond?.(requestId, response);
  }

  installServerRequestInterceptor(interceptor: (note: CodexNotification, frameBytes: number) => boolean): () => void {
    if (this.interceptor !== undefined) throw new Error("interceptor already installed");
    this.interceptor = interceptor;
    return () => {
      if (this.interceptor === interceptor) this.interceptor = undefined;
    };
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
    // A closed transport ENDS the notification stream (unblocks a pending consume).
    this.ended = true;
    if (this.waiter) {
      const w = this.waiter;
      this.waiter = undefined;
      w({ value: undefined, done: true });
    }
    return Promise.resolve();
  }
}

// --- notification builders (the app-server wire vocabulary, per the M0 fixtures) -----

export function threadStarted(threadId = "th-1"): CodexNotification {
  return { kind: "thread_started", method: "thread/started", threadId, params: { thread: { id: threadId } } };
}

export function turnCompleted(status = "completed", usage?: unknown, threadId = "th-1", turnId = "tn-1"): CodexNotification {
  const turn: Record<string, unknown> = { id: turnId, status };
  if (usage !== undefined) turn.usage = usage;
  return { kind: "turn_completed", method: "turn/completed", threadId, turnId, status, params: { threadId, turn } };
}

export function agentMessage(text: string, threadId = "th-1"): CodexNotification {
  return { kind: "activity", method: "item/completed", params: { threadId, item: { type: "agentMessage", text } } };
}

export function refreshRequest(requestId: number): CodexNotification {
  return {
    kind: "activity",
    method: "account/chatgptAuthTokens/refresh",
    requestId,
    params: { reason: "unauthorized", previousAccountId: null },
  };
}

// --- request + harness builders -----------------------------------------------------

export function makeAdviceRequest(overrides: Partial<AdviceRequest> = {}): AdviceRequest {
  return {
    label: "review",
    systemPrompt: "you are the reviewer",
    prompt: "review this",
    model: "gpt-6-astra",
    output: { kind: "text" },
    signal: new AbortController().signal,
    timeoutMs: 5000,
    ...overrides,
  };
}

export const noThrowPolicy: AdviceResultPolicy = { onTerminal: () => {} };

export interface AdviceBits {
  harness: InstanceType<typeof AdviceModuleT.CodexAdviceHarness>;
  transport: FakeTransport;
  launchSpecs: CodexAdviceLaunchSpec[];
  disposeCalls: () => number;
}

export function makeAdviceHarness(
  mods: AdviceModules,
  opts: {
    transport?: FakeTransport;
    credentialValue?: string;
    cwd?: string;
    appServerAuth?: CodexAppServerAuthSession;
    dispose?: () => Promise<void>;
    log?: Logger;
  } = {},
): AdviceBits {
  const transport = opts.transport ?? new FakeTransport();
  const launchSpecs: CodexAdviceLaunchSpec[] = [];
  let disposeCalls = 0;
  const launchRoot: LaunchAdviceRootSeam = async (spec): Promise<CodexAdviceLaunchResult> => {
    launchSpecs.push(spec);
    return {
      transport,
      cwd: opts.cwd ?? "/isolated/advice-home/work",
      dispose: async () => {
        disposeCalls += 1;
        await opts.dispose?.();
      },
    };
  };
  const options: CodexAdviceHarnessOptions = {
    launchRoot,
    provider,
    appServerAuth: opts.appServerAuth,
    credentialValue: opts.credentialValue,
    log: opts.log ?? noopLog,
  };
  const harness = new mods.advice.CodexAdviceHarness(options);
  return { harness, transport, launchSpecs, disposeCalls: () => disposeCalls };
}
