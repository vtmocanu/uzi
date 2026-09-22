// PRD #1171 (M3, plan §2.1) — the stock Codex app-server JSON-RPC-over-stdio transport.
//
// This is the lowest layer of the Codex run/advice adapter: it owns request/response
// correlation, notification decoding, bounded newline framing, outbound backpressure,
// per-request cancellation and unexpected-EOF classification. It is deliberately
// DECOUPLED from any process — it takes an injected readable (the app-server's stdout)
// and writable (the app-server's stdin), so unit tests drive it with in-memory duplex
// pipes and no real Codex binary. Production construction binds it to a live
// `CodexRootHandle.transport` (launcher.ts: `{ stdin: Writable, stdout: Readable }`);
// the fake endpoint is injected only by a non-exported integration composition.
//
// FRAMING is mirrored from the frozen M0 characterization corpus (do NOT edit those
// files; they are evidence): the app-server speaks newline-delimited JSON, one object
// per line — e2e/codex-m0/harness.mjs:314-317 writes `${JSON.stringify(value)}\n` to
// stdin and harness.mjs:267-270 reads stdout line-by-line, asserting each JSONL line
// stays within a 4 MiB cap (harness.mjs:12 `MAX_BYTES = 4 * 1024 * 1024`). The RPC
// vocabulary is likewise M0's: a client request is `{ id, method, params }`
// (harness.mjs:332-334), a response is `{ id, result }` or `{ id, error:{code,message} }`
// (harness.mjs:292-297, :285), and a notification is a `{ method, params? }` frame with
// NO `id` (harness.mjs:273-276, :305 `{ method: "initialized" }`). A frame that carries
// BOTH a `method` and an `id` is a server→client request (harness.mjs:277-291, e.g.
// `*/requestApproval`, `item/tool/call`); the transport surfaces it as liveness carrying
// its `requestId` — replying to it is the worker-owned callback lane (a later unit).
//
// SECURITY: this module NEVER logs raw frames, bodies, params, or tokens. The only
// diagnostics it emits are the static, bounded HarnessError messages below (a JSON-RPC
// error surfaces its numeric `code` at most, never the provider-supplied message
// string). We enforce the frame cap DURING accumulation so a hostile or corrupt stream
// becomes a bounded protocol failure, never an unbounded read buffer.

import type { Readable, Writable } from "node:stream";

import type { HarnessError, HarnessErrorCategory } from "../harness.js";

/** Mirrors e2e/codex-m0/harness.mjs:12 `MAX_BYTES`. Per-line cap, enforced on read. */
const DEFAULT_MAX_FRAME_BYTES = 4 * 1024 * 1024;
/** Bound on serialized frames buffered for write while the peer applies backpressure. */
const DEFAULT_MAX_OUTBOUND_FRAMES = 1024;
/** A single outbound JSONL frame may not exceed the characterized inbound frame cap. */
const DEFAULT_MAX_OUTBOUND_FRAME_BYTES = DEFAULT_MAX_FRAME_BYTES;
/** Aggregate serialized bytes retained in the transport's own outbound queue. */
const DEFAULT_MAX_OUTBOUND_BYTES = 64 * 1024 * 1024;
/** Bound on decoded notifications buffered while no consumer is draining them. */
const DEFAULT_MAX_INBOUND_NOTIFICATIONS = 4096;
/**
 * Aggregate-byte ceiling on the buffered notifications, independent of the count bound
 * above. The count alone is not a memory bound: each queued note retains its raw frame
 * `.params` up to the per-frame cap (DEFAULT_MAX_FRAME_BYTES = 4 MiB), so the count bound
 * alone permits ~4096 × 4 MiB ≈ 16 GiB of retained frames — an OOM crash of the worker
 * (default heap ~2 GB) long before 4096 items accrue, under an untrusted app-server
 * emitting near-cap frames to a slow/non-draining `notifications()` consumer. 64 MiB is
 * well below Node's default old-space (~2 GB, `--max-old-space-size`), leaving ample
 * headroom for the rest of the worker while still absorbing a healthy notification burst;
 * whichever of the two bounds trips first terminates the transport. The running sum is
 * decremented as notes are consumed, so a draining consumer keeps the transport healthy.
 */
const DEFAULT_MAX_INBOUND_BYTES = 64 * 1024 * 1024;

/**
 * A neutral, typed error carrying a reusable {@link HarnessError}. Every rejection and
 * throw out of the transport is one of these, so a caller inspects `.failure.category`
 * rather than pattern-matching a message. `protocol` is reserved for framing/EOF safety
 * (Codex-only; it must never be confused with Claude's clean-EOF `exhausted`).
 */
export class CodexTransportError extends Error {
  readonly failure: HarnessError;
  constructor(failure: HarnessError) {
    super(failure.message);
    this.name = "CodexTransportError";
    this.failure = failure;
  }
}

function fail(category: HarnessErrorCategory, message: string): CodexTransportError {
  return new CodexTransportError({ category, message });
}

/**
 * A decoded inbound notification (or server-initiated request, surfaced as liveness).
 * Known thread/turn lifecycle methods (the M0 vocabulary) are typed; every UNKNOWN
 * method — including Codex `item/*` streaming updates and server→client requests — is a
 * neutral `activity` record so nothing is dropped silently and nothing throws unless
 * protocol-framing safety demands it. Raw `params` are retained for the adapter above
 * (never logged).
 */
/**
 * One usage breakdown from a `thread/tokenUsage/updated` notification (PRD #1332 C4a / D5).
 * The pinned 0.153.2 app-server v2 protocol projects camelCase token counts (source commit
 * `657a993cbee87acf52d14b758ce49dbd46d1b8eb`, `codex-rs/app-server-protocol/src/protocol/v2/
 * thread.rs`). Each field is a finite `>= 0` number; a missing/invalid field decodes to `0`
 * (a present-but-partial breakdown is still usable). `inputTokens` is the TOTAL input for the
 * response(s) — uncached input is `max(inputTokens - cachedInputTokens - cacheWriteInputTokens,
 * 0)` (D5). `outputTokens` already INCLUDES `reasoningOutputTokens` (a subset, never re-added). */
export interface CodexUsageBreakdown {
  readonly inputTokens: number;
  readonly cachedInputTokens: number;
  readonly cacheWriteInputTokens: number;
  readonly outputTokens: number;
  readonly reasoningOutputTokens: number;
  readonly totalTokens: number;
}

/** The token-usage payload of a `thread/tokenUsage/updated` notification: `total` is the
 *  thread's CUMULATIVE usage (the replay/dedup/recovery key), `last` is the single most-recent
 *  upstream response's usage (the per-response pricing basis, D5). `modelContextWindow` is the
 *  model's context size when reported (unused by C4a token accounting).
 *
 *  `pricingEvidenceComplete` (PRD #1332 m3, CodeRabbit 4004800884) is the FAIL-CLOSED pricing gate:
 *  false when a PRICING-REQUIRED bucket (`inputTokens`, `cachedInputTokens`, `cacheWriteInputTokens`,
 *  `outputTokens`) of EITHER breakdown is absent or malformed (non-numeric, negative, NaN, or
 *  Infinite). The pinned protocol requires all four; treating a missing bucket as affirmative zero
 *  would understate a metered cost. Token totals are retained through zero placeholders, and this
 *  flag ONLY gates cost: the api-key branch of {@link
 *  CodexUsageAccountant.aggregateByModel} degrades the model to `unreported` on `false`. Optional so
 *  a pre-decoded test construction (which omits it) reads as complete; the real decoder always sets
 *  it. The two non-priced buckets (`totalTokens`, `reasoningOutputTokens`) never enter the price and
 *  are not consulted. */
export interface CodexThreadTokenUsage {
  readonly total: CodexUsageBreakdown;
  readonly last: CodexUsageBreakdown;
  readonly modelContextWindow?: number;
  readonly pricingEvidenceComplete?: boolean;
}

export type CodexNotification =
  | { readonly kind: "thread_started"; readonly method: string; readonly threadId: string; readonly params: unknown }
  | {
      readonly kind: "turn_started";
      readonly method: string;
      readonly threadId: string;
      readonly turnId: string;
      readonly params: unknown;
    }
  | {
      readonly kind: "turn_completed";
      readonly method: string;
      readonly threadId: string;
      readonly turnId: string;
      readonly status?: string;
      readonly params: unknown;
    }
  | {
      // PRD #1332 C4a / D5: a TYPED per-thread token-usage update, decoded off the pinned
      // `thread/tokenUsage/updated` method rather than treated as generic activity. A frame
      // whose threadId/turnId/total/last cannot be parsed falls to `activity` (fail-safe
      // liveness), never a mis-shaped token_usage.
      readonly kind: "token_usage_updated";
      readonly method: string;
      readonly threadId: string;
      readonly turnId: string;
      readonly usage: CodexThreadTokenUsage;
      readonly params: unknown;
    }
  | {
      // PRD #1534: a fail-safe-decoded provider `ErrorNotification` (method `error`), bound to
      // the active turn by threadId/turnId. Decoded here ONLY when threadId, turnId and a real
      // boolean willRetry are all present; parsing of the provider `error`/`willRetry` fields
      // into a terminal classification is deferred to the harness/normalizer (a later milestone).
      readonly kind: "codex_error";
      readonly method: string;
      readonly threadId: string;
      readonly turnId: string;
      readonly willRetry: boolean;
      readonly params: unknown;
    }
  | {
      readonly kind: "activity";
      /** null when a framing-intact frame carried neither a method nor a matched id. */
      readonly method: string | null;
      /** present when the frame was a server-initiated request or an unmatched response id. */
      readonly requestId?: number | string;
      readonly params: unknown;
    };

export interface CodexTransportOptions {
  /** The app-server's stdout — decoded frames are read from here. */
  readonly inbound: Readable;
  /** The app-server's stdin — request/notification frames are written here. */
  readonly outbound: Writable;
  readonly maxFrameBytes?: number;
  readonly maxOutboundFrames?: number;
  readonly maxOutboundFrameBytes?: number;
  readonly maxOutboundBytes?: number;
  readonly maxInboundNotifications?: number;
  readonly maxInboundBytes?: number;
}

export interface CodexTransport {
  /**
   * Send a JSON-RPC request and resolve with its `result`, reject with a typed
   * {@link CodexTransportError} on a JSON-RPC error, `deadlineMs` timeout, `signal`
   * abort, or a transport/protocol failure (over-cap frame, unexpected EOF, closed).
   */
  request<T = unknown>(
    method: string,
    params?: unknown,
    opts?: { signal?: AbortSignal; deadlineMs?: number },
  ): Promise<T>;
  /** Send a fire-and-forget notification (no id). Throws a typed transport failure if
   *  the bounded outbound queue is full. */
  notify(method: string, params?: unknown): void;
  /**
   * Answer a server→client request (a frame carrying BOTH a `method` and an `id`, e.g.
   * `item/tool/call` — see {@link decodeNotification}) by its `requestId`. This is the
   * worker-owned callback reply lane: the adapter above decodes the request off
   * {@link notifications}, routes it to the broker, and replies here with the neutral
   * result (`{ result }`) or a JSON-RPC error (`{ error }`). Mirrors the M0 client reply
   * `{ id, result }` / `{ id, error }` (e2e/codex-m0/harness.mjs:291, :285). Additive to
   * the M3 transport; existing behavior is unchanged. Throws a typed transport failure if
   * the transport is closed or the bounded outbound queue is full.
   */
  respond(
    requestId: number | string,
    response: { readonly result: unknown } | { readonly error: { readonly code: number; readonly message: string } },
  ): void;
  /**
   * Install the single narrow server-request interceptor. It runs synchronously in the
   * read path, before an id-bearing request can enter the notification queue. Returning
   * true claims that request; false leaves it on the ordinary single-consumer stream.
   *
   * Optional only for structural compatibility with credential-free test transports.
   * The pinned app-server auth owner requires it and fails closed when it is absent.
   */
  installServerRequestInterceptor?(interceptor: (note: CodexNotification, frameBytes: number) => boolean): () => void;
  /** Single-consumer async iterator of decoded notifications. Ends (done) on a clean
   *  close; throws the terminal {@link CodexTransportError} on a framing/EOF failure. */
  notifications(): AsyncIterableIterator<CodexNotification>;
  /** Idempotent transport-side closure: detaches stream handlers and rejects every
   *  in-flight request. Does not end the injected streams (the launcher owns those). */
  close(): Promise<void>;
}

interface Pending {
  onResponse(msg: Record<string, unknown>): void;
  onError(err: unknown): void;
}

interface QueuedFrame {
  readonly line: string;
  readonly bytes: number;
}

function readStringProp(obj: unknown, key: string): string | undefined {
  if (typeof obj === "object" && obj !== null) {
    const v = (obj as Record<string, unknown>)[key];
    if (typeof v === "string") return v;
  }
  return undefined;
}

function readObjectProp(obj: unknown, key: string): Record<string, unknown> | undefined {
  if (typeof obj === "object" && obj !== null) {
    const v = (obj as Record<string, unknown>)[key];
    if (typeof v === "object" && v !== null && !Array.isArray(v)) return v as Record<string, unknown>;
  }
  return undefined;
}

/** A finite `>= 0` number off `obj[key]`, else 0. Missing/negative/NaN/Infinity collapse to 0
 *  so a partial or attacker-shaped breakdown retains bounded, non-negative token placeholders.
 *  Pricing fails closed separately in {@link breakdownPricingEvidenceComplete}; this coercion is
 *  never evidence that a required pricing bucket was actually present. */
function readNonNegNumber(obj: Record<string, unknown>, key: string): number {
  const v = obj[key];
  return typeof v === "number" && Number.isFinite(v) && v >= 0 ? v : 0;
}

/** The four PRICING-REQUIRED buckets (`token-accounting.ts` responsesReconcileDelta / the price
 *  table): a malformed one understates a metered cost, so it is the only corruption {@link
 *  breakdownPricingEvidenceComplete} fails closed on. `totalTokens` and `reasoningOutputTokens`
 *  never price and are intentionally excluded. */
const PRICED_BUCKET_KEYS = ["inputTokens", "cachedInputTokens", "cacheWriteInputTokens", "outputTokens"] as const;

/** Pricing-evidence completeness for ONE breakdown (PRD #1332 m3, CodeRabbit 4004800884).
 *  Every priced bucket is required by the pinned protocol and must be a finite non-negative number.
 *  Missing and malformed values both fail cost closed; parseUsageBreakdown still retains their
 *  zero-valued token placeholders independently. */
function breakdownPricingEvidenceComplete(raw: Record<string, unknown> | undefined): boolean {
  if (raw === undefined) return false;
  for (const key of PRICED_BUCKET_KEYS) {
    const v = raw[key];
    if (!(typeof v === "number" && Number.isFinite(v) && v >= 0)) return false;
  }
  return true;
}

function parseUsageBreakdown(raw: Record<string, unknown> | undefined): CodexUsageBreakdown | undefined {
  if (raw === undefined) return undefined;
  return {
    inputTokens: readNonNegNumber(raw, "inputTokens"),
    cachedInputTokens: readNonNegNumber(raw, "cachedInputTokens"),
    cacheWriteInputTokens: readNonNegNumber(raw, "cacheWriteInputTokens"),
    outputTokens: readNonNegNumber(raw, "outputTokens"),
    reasoningOutputTokens: readNonNegNumber(raw, "reasoningOutputTokens"),
    totalTokens: readNonNegNumber(raw, "totalTokens"),
  };
}

/** Resolve the object carrying the `total`/`last` breakdowns of a `thread/tokenUsage/updated`
 *  frame. VERIFIED against the pinned 0.153.2 v2 protocol (commit
 *  `657a993cbee87acf52d14b758ce49dbd46d1b8eb`, `codex-rs/app-server-protocol/src/protocol/v2/
 *  thread.rs`): the notification payload is `ThreadTokenUsageUpdatedNotification { thread_id,
 *  turn_id, token_usage: ThreadTokenUsage { total, last, model_context_window } }` under
 *  `#[serde(rename_all = "camelCase")]` and the adjacently-tagged `ServerNotification`
 *  (`#[serde(tag = "method", content = "params")]`, `common.rs`), so over the wire the
 *  breakdowns live under `params.tokenUsage` — the SAME camelCase convention `thread/started`,
 *  `turn/started` and `turn/completed` params already use in this file. A frame whose
 *  `tokenUsage` object is absent or lacks either breakdown ⇒ undefined (the caller falls to
 *  `activity`, fail-safe liveness). */
function tokenUsageContainer(params: Record<string, unknown> | undefined): Record<string, unknown> | undefined {
  if (params === undefined) return undefined;
  const container = readObjectProp(params, "tokenUsage");
  if (container !== undefined && readObjectProp(container, "total") !== undefined && readObjectProp(container, "last") !== undefined) {
    return container;
  }
  return undefined;
}

class CodexTransportImpl implements CodexTransport {
  private readonly inbound: Readable;
  private readonly outbound: Writable;
  private readonly maxFrameBytes: number;
  private readonly maxOutbound: number;
  private readonly maxOutboundFrameBytes: number;
  private readonly maxOutboundBytes: number;
  private readonly maxInbound: number;
  private readonly maxInboundBytes: number;

  private nextId = 1;
  private readonly pending = new Map<number, Pending>();

  private readBuf = "";
  private closed = false;
  private terminalError: CodexTransportError | undefined;

  private readonly sendQueue: QueuedFrame[] = [];
  private sendBytes = 0;
  private draining = false;

  private readonly notesQueue: Array<{ note: CodexNotification; bytes: number }> = [];
  private notesBytes = 0;
  private notesWaiter: { resolve: (r: IteratorResult<CodexNotification>) => void; reject: (e: unknown) => void } | undefined;
  private notesIterated = false;
  private serverRequestInterceptor?: (note: CodexNotification, frameBytes: number) => boolean;

  private readonly onDataHandler = (chunk: string): void => this.onData(chunk);
  private readonly onEndHandler = (): void => this.onEof();
  private readonly onInboundError = (): void => this.terminate(fail("transport", "codex transport read stream error"));
  private readonly onOutboundError = (): void => this.terminate(fail("transport", "codex transport write stream error"));

  constructor(opts: CodexTransportOptions) {
    this.inbound = opts.inbound;
    this.outbound = opts.outbound;
    this.maxFrameBytes = opts.maxFrameBytes ?? DEFAULT_MAX_FRAME_BYTES;
    this.maxOutbound = opts.maxOutboundFrames ?? DEFAULT_MAX_OUTBOUND_FRAMES;
    this.maxOutboundFrameBytes = opts.maxOutboundFrameBytes ?? DEFAULT_MAX_OUTBOUND_FRAME_BYTES;
    this.maxOutboundBytes = opts.maxOutboundBytes ?? DEFAULT_MAX_OUTBOUND_BYTES;
    this.maxInbound = opts.maxInboundNotifications ?? DEFAULT_MAX_INBOUND_NOTIFICATIONS;
    this.maxInboundBytes = opts.maxInboundBytes ?? DEFAULT_MAX_INBOUND_BYTES;

    this.inbound.setEncoding("utf8");
    this.inbound.on("data", this.onDataHandler);
    this.inbound.on("end", this.onEndHandler);
    this.inbound.on("close", this.onEndHandler);
    this.inbound.on("error", this.onInboundError);
    this.outbound.on("error", this.onOutboundError);
  }

  request<T = unknown>(
    method: string,
    params?: unknown,
    opts?: { signal?: AbortSignal; deadlineMs?: number },
  ): Promise<T> {
    return new Promise<T>((resolve, reject) => {
      if (this.closed) {
        // A caller-initiated close (or a request after a clean EOF) is not a protocol
        // violation; only a terminal framing/EOF failure (`terminalError`) carries
        // `protocol`. Absent one, the transport is simply closed.
        reject(this.terminalError ?? fail("transport", "codex transport is closed"));
        return;
      }
      const signal = opts?.signal;
      if (signal?.aborted) {
        reject(fail("aborted", "codex transport request aborted before send"));
        return;
      }
      const id = this.nextId++;
      let timer: ReturnType<typeof setTimeout> | undefined;
      let onAbort: (() => void) | undefined;
      let settled = false;
      const finish = (fn: () => void): void => {
        if (settled) return;
        settled = true;
        this.pending.delete(id);
        if (timer) clearTimeout(timer);
        if (onAbort && signal) signal.removeEventListener("abort", onAbort);
        fn();
      };
      const entry: Pending = {
        onResponse: (msg) =>
          finish(() => {
            if (msg.error !== undefined) reject(this.jsonRpcError(msg.error));
            else resolve(msg.result as T);
          }),
        onError: (err) => finish(() => reject(err)),
      };
      this.pending.set(id, entry);
      const deadlineMs = opts?.deadlineMs;
      if (deadlineMs !== undefined && deadlineMs > 0) {
        timer = setTimeout(() => entry.onError(fail("timeout", "codex transport request deadline exceeded")), deadlineMs);
      }
      if (signal) {
        onAbort = (): void => entry.onError(fail("aborted", "codex transport request aborted"));
        signal.addEventListener("abort", onAbort, { once: true });
        // Close the pre-check/listener-install race. AbortSignal dispatch is
        // synchronous, so a signal that flipped between those two points is now
        // observed before any frame is sent.
        if (signal.aborted) {
          entry.onError(fail("aborted", "codex transport request aborted before send"));
          return;
        }
      }
      try {
        this.enqueueFrame(params === undefined ? { id, method } : { id, method, params });
      } catch (err) {
        // Use the common settlement path so a serialization/queue failure removes
        // the pending entry, timer, and abort listener exactly once.
        entry.onError(err);
      }
    });
  }

  notify(method: string, params?: unknown): void {
    if (this.closed) throw this.terminalError ?? fail("transport", "codex transport is closed");
    this.enqueueFrame(params === undefined ? { method } : { method, params });
  }

  respond(
    requestId: number | string,
    response: { readonly result: unknown } | { readonly error: { readonly code: number; readonly message: string } },
  ): void {
    if (this.closed) throw this.terminalError ?? fail("transport", "codex transport is closed");
    // A reply is correlated by the server-supplied `id`, never one we mint. It is a
    // `{ id, result }` or `{ id, error }` frame (never `method`), so a peer routes it to
    // its pending request rather than treating it as a fresh notification.
    const frame = "error" in response ? { id: requestId, error: response.error } : { id: requestId, result: response.result };
    this.enqueueFrame(frame);
  }

  installServerRequestInterceptor(interceptor: (note: CodexNotification, frameBytes: number) => boolean): () => void {
    if (this.closed) throw this.terminalError ?? fail("transport", "codex transport is closed");
    if (this.serverRequestInterceptor !== undefined) {
      throw fail("protocol", "codex transport server-request interceptor is already installed");
    }
    this.serverRequestInterceptor = interceptor;
    return (): void => {
      if (this.serverRequestInterceptor === interceptor) this.serverRequestInterceptor = undefined;
    };
  }

  notifications(): AsyncIterableIterator<CodexNotification> {
    if (this.notesIterated) throw fail("transport", "codex transport notifications() is single-consumer");
    this.notesIterated = true;
    const iterator: AsyncIterableIterator<CodexNotification> = {
      next: (): Promise<IteratorResult<CodexNotification>> =>
        new Promise<IteratorResult<CodexNotification>>((resolve, reject) => {
          const queued = this.notesQueue.shift();
          if (queued !== undefined) {
            this.notesBytes -= queued.bytes;
            resolve({ value: queued.note, done: false });
            return;
          }
          if (this.closed) {
            if (this.terminalError) reject(this.terminalError);
            else resolve({ value: undefined, done: true });
            return;
          }
          this.notesWaiter = { resolve, reject };
        }),
      return: (): Promise<IteratorResult<CodexNotification>> => Promise.resolve({ value: undefined, done: true }),
      [Symbol.asyncIterator](): AsyncIterableIterator<CodexNotification> {
        return this;
      },
    };
    return iterator;
  }

  async close(): Promise<void> {
    if (!this.closed) this.terminate();
  }

  // --- write side (bounded, backpressure-aware) -------------------------------

  private enqueueFrame(frame: unknown): void {
    if (this.sendQueue.length >= this.maxOutbound) {
      throw fail("transport", "codex transport outbound queue is full");
    }
    let line: string;
    try {
      line = `${JSON.stringify(frame)}\n`;
    } catch {
      throw fail("transport", "codex transport could not serialize outbound frame");
    }
    const bytes = Buffer.byteLength(line, "utf8");
    if (bytes > this.maxOutboundFrameBytes) {
      throw fail("transport", "codex transport outbound frame exceeds size cap");
    }
    if (this.sendBytes + bytes > this.maxOutboundBytes) {
      throw fail("transport", "codex transport outbound byte queue is full");
    }
    this.sendQueue.push({ line, bytes });
    this.sendBytes += bytes;
    this.kickDrain();
  }

  private kickDrain(): void {
    if (this.draining) return;
    this.draining = true;
    this.drainLoop();
  }

  private drainLoop(): void {
    while (this.sendQueue.length > 0) {
      if (this.closed) {
        this.draining = false;
        return;
      }
      const queued = this.sendQueue.shift();
      if (queued === undefined) break;
      this.sendBytes -= queued.bytes;
      if (!this.outbound.write(queued.line)) {
        // Peer is applying backpressure; resume once the buffer drains.
        this.outbound.once("drain", () => {
          if (!this.closed) this.drainLoop();
        });
        return;
      }
    }
    this.draining = false;
  }

  // --- read side (bounded newline framing) ------------------------------------

  private onData(chunk: string): void {
    if (this.closed) return;
    this.readBuf += chunk;
    let nl = this.readBuf.indexOf("\n");
    while (nl !== -1) {
      const line = this.readBuf.slice(0, nl);
      this.readBuf = this.readBuf.slice(nl + 1);
      const byteLen = Buffer.byteLength(line, "utf8");
      if (byteLen > this.maxFrameBytes) {
        this.terminate(fail("protocol", "codex transport inbound frame exceeds size cap"));
        return;
      }
      this.handleLine(line, byteLen);
      if (this.closed) return;
      nl = this.readBuf.indexOf("\n");
    }
    // A partial line is still accumulating; bound it so a newline-less flood cannot
    // grow the read buffer without limit.
    if (Buffer.byteLength(this.readBuf, "utf8") > this.maxFrameBytes) {
      this.terminate(fail("protocol", "codex transport inbound frame exceeds size cap"));
    }
  }

  private handleLine(line: string, byteLen: number): void {
    if (line.trim().length === 0) return;
    let value: unknown;
    try {
      value = JSON.parse(line);
    } catch {
      this.terminate(fail("protocol", "codex transport received an unparseable frame"));
      return;
    }
    if (typeof value !== "object" || value === null || Array.isArray(value)) {
      this.terminate(fail("protocol", "codex transport received a non-object frame"));
      return;
    }
    const msg = value as Record<string, unknown>;
    const method = msg.method;
    const id = msg.id;
    if (typeof method === "string") {
      const requestId = typeof id === "number" || typeof id === "string" ? id : undefined;
      const note = this.decodeNotification(method, msg.params, requestId);
      if (requestId !== undefined && this.serverRequestInterceptor !== undefined) {
        let handled: boolean;
        try {
          handled = this.serverRequestInterceptor(note, byteLen);
        } catch {
          this.terminate(fail("protocol", "codex transport server-request interceptor failed"));
          return;
        }
        if (handled) return;
      }
      this.pushNotification(note, byteLen);
      return;
    }
    if (typeof id === "number" || typeof id === "string") {
      this.dispatchResponse(id, msg, byteLen);
      return;
    }
    // Framing intact, but neither a method nor a correlatable id: liveness only.
    this.pushNotification({ kind: "activity", method: null, params: msg }, byteLen);
  }

  private decodeNotification(method: string, params: unknown, requestId: number | string | undefined): CodexNotification {
    // Server-initiated requests (method + id) and unknown methods are liveness only.
    if (requestId === undefined) {
      if (method === "thread/started") {
        const threadId = readStringProp((params as Record<string, unknown> | undefined)?.thread, "id");
        if (threadId !== undefined) return { kind: "thread_started", method, threadId, params };
      } else if (method === "turn/started") {
        const threadId = readStringProp(params, "threadId");
        const turnId = readStringProp((params as Record<string, unknown> | undefined)?.turn, "id");
        if (threadId !== undefined && turnId !== undefined) return { kind: "turn_started", method, threadId, turnId, params };
      } else if (method === "turn/completed") {
        const threadId = readStringProp(params, "threadId");
        const turnId = readStringProp((params as Record<string, unknown> | undefined)?.turn, "id");
        const status = readStringProp((params as Record<string, unknown> | undefined)?.turn, "status");
        if (threadId !== undefined && turnId !== undefined) {
          return status === undefined
            ? { kind: "turn_completed", method, threadId, turnId, params }
            : { kind: "turn_completed", method, threadId, turnId, status, params };
        }
      } else if (method === "thread/tokenUsage/updated") {
        // PRD #1332 C4a / D5: decode as a TYPED usage notification. It requires threadId,
        // turnId AND both `total`/`last` breakdowns; any missing/mis-shaped part falls through
        // to `activity` (fail-safe liveness) so a malformed frame never mis-accounts.
        const p = params as Record<string, unknown> | undefined;
        const threadId = readStringProp(params, "threadId");
        const turnId = readStringProp(params, "turnId");
        const container = tokenUsageContainer(p);
        const rawTotal = readObjectProp(container, "total");
        const rawLast = readObjectProp(container, "last");
        const total = parseUsageBreakdown(rawTotal);
        const last = parseUsageBreakdown(rawLast);
        if (threadId !== undefined && turnId !== undefined && total !== undefined && last !== undefined) {
          const rawWindow = container?.modelContextWindow;
          // PRD #1332 m3 (CodeRabbit 4004800884): fail-closed pricing gate. Token totals were still
          // coerced+retained above; this flags whether a PRICING-REQUIRED bucket of EITHER breakdown
          // was present-but-malformed, so the accountant can degrade the model to `unreported`
          // instead of emitting an understated metered cost off a silently-zeroed field.
          const pricingEvidenceComplete =
            breakdownPricingEvidenceComplete(rawTotal) && breakdownPricingEvidenceComplete(rawLast);
          const usage: CodexThreadTokenUsage =
            typeof rawWindow === "number" && Number.isFinite(rawWindow) && rawWindow >= 0
              ? { total, last, modelContextWindow: rawWindow, pricingEvidenceComplete }
              : { total, last, pricingEvidenceComplete };
          return { kind: "token_usage_updated", method, threadId, turnId, usage, params };
        }
      } else if (method === "error") {
        // PRD #1534: decode a provider ErrorNotification, reading threadId/turnId/willRetry
        // from the TOP LEVEL of params (mirrors the thread/tokenUsage/updated branch above).
        const threadId = readStringProp(params, "threadId");
        const turnId = readStringProp(params, "turnId");
        const rawWillRetry = (params as Record<string, unknown> | undefined)?.willRetry;
        const willRetry = typeof rawWillRetry === "boolean" ? rawWillRetry : undefined;
        // FAIL-SAFE GATE: emit codex_error ONLY when threadId and turnId are non-empty strings
        // AND willRetry is a real boolean. A malformed or future-shaped error frame (an absent
        // or non-boolean willRetry, a missing binding id) is NOT defaulted — it falls through to
        // the generic `activity` liveness below, and the terminal turn.error fallback (added in a
        // later milestone) still covers the turn's classification.
        if (
          threadId !== undefined &&
          threadId.length > 0 &&
          turnId !== undefined &&
          turnId.length > 0 &&
          willRetry !== undefined
        ) {
          return { kind: "codex_error", method, threadId, turnId, willRetry, params };
        }
      }
    }
    return requestId === undefined
      ? { kind: "activity", method, params }
      : { kind: "activity", method, requestId, params };
  }

  private dispatchResponse(id: number | string, msg: Record<string, unknown>, byteLen: number): void {
    const pending = typeof id === "number" ? this.pending.get(id) : undefined;
    if (!pending) {
      // Unmatched response id — not a request we track (or a string id we never mint).
      // Surface as liveness rather than drop it silently.
      this.pushNotification({ kind: "activity", method: null, requestId: id, params: msg }, byteLen);
      return;
    }
    pending.onResponse(msg);
  }

  private jsonRpcError(err: unknown): CodexTransportError {
    let code: number | undefined;
    if (typeof err === "object" && err !== null) {
      const c = (err as Record<string, unknown>).code;
      if (typeof c === "number") code = c;
    }
    // The provider `message` is NOT embedded — it can be attacker-shaped and must never
    // reach a log through an error string. Only the bounded numeric code is retained.
    const suffix = code === undefined ? "" : ` (code ${code})`;
    return fail("protocol", `codex app-server returned a JSON-RPC error${suffix}`);
  }

  // --- notification delivery (bounded, single-consumer) -----------------------

  private pushNotification(note: CodexNotification, bytes: number): void {
    if (this.closed) return;
    if (this.notesWaiter) {
      // Delivered straight to a waiting consumer — never retained, so no byte accounting.
      const waiter = this.notesWaiter;
      this.notesWaiter = undefined;
      waiter.resolve({ value: note, done: false });
      return;
    }
    if (this.notesQueue.length >= this.maxInbound) {
      this.terminate(fail("transport", "codex transport inbound notification buffer overflow"));
      return;
    }
    // Independent aggregate-byte ceiling: the count bound above does not cap retained
    // memory (each note holds its raw frame `.params`), so enforce the byte sum too —
    // whichever bound trips first wins.
    if (this.notesBytes + bytes > this.maxInboundBytes) {
      this.terminate(fail("transport", "codex transport inbound notification byte overflow"));
      return;
    }
    this.notesQueue.push({ note, bytes });
    this.notesBytes += bytes;
  }

  // --- terminal state ---------------------------------------------------------

  private onEof(): void {
    if (this.closed) return;
    // EOF is only a failure MID-REQUEST — an unexpected close while a call is in flight
    // is a Codex protocol failure (distinct from Claude's clean-EOF `exhausted`). With
    // no pending request it is an ordinary close.
    if (this.pending.size > 0) this.terminate(fail("protocol", "codex transport stream closed mid-request"));
    else this.terminate();
  }

  private terminate(error?: CodexTransportError): void {
    if (this.closed) return;
    this.closed = true;
    this.terminalError = error;

    this.inbound.removeListener("data", this.onDataHandler);
    this.inbound.removeListener("end", this.onEndHandler);
    this.inbound.removeListener("close", this.onEndHandler);
    this.inbound.removeListener("error", this.onInboundError);
    this.outbound.removeListener("error", this.onOutboundError);
    this.serverRequestInterceptor = undefined;
    this.sendQueue.length = 0;
    this.sendBytes = 0;

    // A bare terminate() is a caller-initiated close (or a clean EOF with no request in
    // flight) — a transport closure, not a `protocol` framing/EOF violation. Genuine
    // protocol/EOF failures pass their own `error` and keep that category.
    const reason = error ?? fail("transport", "codex transport closed");
    const entries = [...this.pending.values()];
    this.pending.clear();
    for (const entry of entries) entry.onError(reason);

    const waiter = this.notesWaiter;
    this.notesWaiter = undefined;
    if (waiter) {
      if (error) waiter.reject(error);
      else waiter.resolve({ value: undefined, done: true });
    }
  }
}

/**
 * Construct a Codex app-server transport over injected streams. Production binds
 * `inbound = handle.transport.stdout` and `outbound = handle.transport.stdin`; tests
 * inject in-memory duplex pipes.
 */
export function createCodexTransport(opts: CodexTransportOptions): CodexTransport {
  return new CodexTransportImpl(opts);
}
