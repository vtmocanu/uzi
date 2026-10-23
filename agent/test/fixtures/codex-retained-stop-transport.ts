// Scripted provider transport shared only by the retained-stop HTTP composition.
// Adapted from codex-executor.test.ts: no worker API or Executor.run substitution.
import assert from "node:assert/strict";
import type { CodexNotification, CodexTransport } from "../../src/codex/transport.js";
function rec(value: unknown): Record<string, unknown> { return (value ?? {}) as Record<string, unknown>; }
interface ResponderCtx {
  transport: FakeTransport; method: string; params: unknown;
  threadStartCount: number; turnStartCount: number;
}
type Responder = (ctx: ResponderCtx) => unknown;
export class FakeTransport implements CodexTransport {
  requests: { method: string; params: unknown; opts?: { signal?: AbortSignal; deadlineMs?: number } }[] = [];
  responses: { requestId: number | string; response: unknown }[] = [];
  notifies: { method: string; params: unknown }[] = [];
  closes = 0;
  threadStartCount = 0;
  turnStartCount = 0;
  private readonly firstTurnListeners = new Set<() => void>();

  onFirstTurnStart(listener: () => void): () => void {
    const owner = this.countsFrom ?? this;
    if (owner.turnStartCount > 0) {
      listener();
      return () => {};
    }
    owner.firstTurnListeners.add(listener);
    return () => { owner.firstTurnListeners.delete(listener); };
  }

  firstTurnListenerCount(): number {
    return (this.countsFrom ?? this).firstTurnListeners.size;
  }
  /** When set for a method, request() returns THIS (rejects/pends) instead of the responder. */
  requestOverride?: (c: ResponderCtx, opts?: { signal?: AbortSignal; deadlineMs?: number }) => Promise<unknown> | undefined;

  private readonly queue: CodexNotification[] = [];
  private ended = false;
  private waiter: ((r: IteratorResult<CodexNotification>) => void) | undefined;
  private consumed = false;
  private closedFlag = false;
  private interceptor?: (note: CodexNotification, frameBytes: number) => boolean;

  /** Issue #1782: when proven cleanup permits epoch recreation after a refused wall park,
   *  a single-transport rig hands that epoch a SUCCESSOR transport whose request log and counters feed
   *  this (primary) transport, keeping cumulative assertions on `rig.transport` meaningful.
   *  Only `requests` and the thread/turn start counters are mirrored: responses, notifies, closes,
   *  requestOverride and the server-request interceptor stay per-transport and are NOT copied to a
   *  successor, so assert those on the transport that owns them. */
  countsFrom?: FakeTransport;

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

  request<T = unknown>(method: string, params?: unknown, opts?: { signal?: AbortSignal; deadlineMs?: number }): Promise<T> {
    const counts = this.countsFrom ?? this;
    this.requests.push({ method, params, opts });
    if (counts !== this) counts.requests.push({ method, params, opts });
    // The pinned app-server auth handshake (createCodexAppServerAuth → authenticate): answer
    // initialize + account/login/start here so EVERY responder (and requestOverride) is free of
    // the auth plumbing. `account/login/start` echoes the login `type` (apiKey | chatgptAuthTokens).
    if (method === "initialize") {
      return Promise.resolve({ userAgent: "codex/0.153.2", codexHome: "/owned/codex", platformFamily: "unix", platformOs: "linux" } as T);
    }
    if (method === "account/login/start") {
      return Promise.resolve({ type: rec(params).type } as T);
    }
    if (method === "thread/start") counts.threadStartCount += 1;
    if (method === "turn/start") {
      counts.turnStartCount += 1;
      const listeners = [...counts.firstTurnListeners];
      counts.firstTurnListeners.clear();
      for (const listener of listeners) listener();
    }
    const c: ResponderCtx = {
      transport: this,
      method,
      params,
      threadStartCount: counts.threadStartCount,
      turnStartCount: counts.turnStartCount,
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

  deliverServerRequest(note: CodexNotification): void {
    assert.ok(this.interceptor?.(note, Buffer.byteLength(JSON.stringify(note))), "live transport interceptor accepts the server request");
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
