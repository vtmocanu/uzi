/**
 * Issue #2014 (ADR-2014 D1/D3): the worker-side capture of per-assistant-message Claude usage, kept
 * on a side channel so a leg interrupted before its `result` frame still leaves its model calls
 * on record (POST /worker/runs/{id}/usage), never in run_messages.
 *
 * Two parts:
 *  - {@link UsageLeg}: one `query()` (one init frame). Records are keyed by Anthropic `message.id`,
 *    numbered with a dense ordinal 1..n per distinct id in SDK stream order, and merged per token
 *    column with GREATEST (consecutive assistant frames share an id, and an assistant frame's usage
 *    is a message_start snapshot; the final output count arrives on `message_delta`).
 *  - {@link UsageRecorder}: the run's sender. It debounces, posts at most USAGE_POST_MAX_RECORDS per
 *    request, backs off on a transient failure, and is drained by the run's MessageBatcher before
 *    every flush/close. A drain waits at most USAGE_DRAIN_DEADLINE_MS, then ABANDONS what is unsent
 *    and ABORTS the in-flight request through its AbortController, so a hung route never holds a
 *    park or a shutdown.
 *
 * Memory is bounded: at most USAGE_PENDING_MAX distinct unsent messages (new ones past it are
 * dropped and counted in `dropped_records`) and at most USAGE_LEG_MAX_MESSAGES distinct ids per leg.
 * Nothing is spilled to disk: a hard kill loses the unsent tail, which the server reports as
 * `leg_not_closed` / `ordinal_gap`.
 */
import { randomUUID } from "node:crypto";
import { RequestError } from "./client.js";
import type { Logger } from "./log.js";
import { isSubagentFrame } from "./signals.js";
import { errMessage } from "./util.js";

/** Distinct unsent messages held in memory before new ones are dropped (and counted). */
export const USAGE_PENDING_MAX = 2000;
/** Dropped message ids remembered per leg, so a later frame of a dropped message is not recorded
 *  half-way. Past it a new rejection is still counted in dropped_records but not remembered, so a
 *  later frame of that message may be counted again, or recorded from that later frame once room
 *  frees; dropped_records stays > 0, so coverage still reads records_dropped. */
export const USAGE_DROPPED_IDS_MAX = 2000;
/** Records one /usage request carries (the api's maxUsagePostRecords). */
export const USAGE_POST_MAX_RECORDS = 500;
/** Leg markers one /usage request carries (the api's maxUsagePostLegs). */
const USAGE_POST_MAX_LEGS = 16;
/** Longest a drain waits before it abandons the unsent records and aborts the request. */
export const USAGE_DRAIN_DEADLINE_MS = 3000;
/** Debounce between a change and the post that carries it. */
const USAGE_DEBOUNCE_MS = 250;
/** Per-request timeout, independent of the client's own. */
const USAGE_REQUEST_TIMEOUT_MS = 10_000;
/** Transient-failure backoff: base * 2^(failures-1), capped. */
const USAGE_BACKOFF_BASE_MS = 500;
const USAGE_BACKOFF_MAX_MS = 30_000;
/** Distinct message ids one leg tracks; matches the api's per-run record cap, so more is dropped. */
const USAGE_LEG_MAX_MESSAGES = 20_000;
/** Untyped 404/405 answers in a row after which the route is disabled for the process (the same
 *  pattern as INCLUSION_ROUTE_MISSING_LIMIT: a new worker against an api that predates the route). */
export const USAGE_ROUTE_MISSING_LIMIT = 8;
/** The SDK's placeholder model on a worker-synthesized frame; never a real API call. */
const SYNTHETIC_MODEL = "<synthetic>";

/** Process-wide: an api that answered the route untyped 404/405 USAGE_ROUTE_MISSING_LIMIT times. */
let routeDisabled = false;
let routeMissingStreak = 0;

/** One record on the /usage wire (the frozen contract: api/internal/workersvc/usage_tail.go). */
export interface UsageWireRecord {
  message_id: string;
  leg_id: string;
  ordinal: number;
  frame_session_id?: string;
  model: string;
  subagent: boolean;
  input_tokens: number;
  cache_read_input_tokens: number;
  cache_creation_input_tokens: number;
  cache_creation_5m_input_tokens?: number;
  cache_creation_1h_input_tokens?: number;
  output_tokens: number;
  output_final: boolean;
  service_tier?: string;
  speed?: string;
  inference_geo?: string;
}

/** One leg marker on the /usage wire. */
export interface UsageWireLeg {
  leg_id: string;
  closed_through?: number;
  dropped_records?: number;
}

export interface UsageWireRequest {
  legs: UsageWireLeg[];
  messages: UsageWireRecord[];
}

/** The client surface the recorder posts through (WorkerClient implements it). */
export interface UsageClient {
  postUsage(runId: string, body: UsageWireRequest, generation?: number, signal?: AbortSignal): Promise<void>;
}

/** What the run lane's harness records into; see {@link UsageRecorder.startLeg}. */
export interface UsageSink {
  startLeg(): UsageLeg;
}

function asRecord(v: unknown): Record<string, unknown> | undefined {
  return v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
}

function asString(v: unknown): string | undefined {
  return typeof v === "string" && v.length > 0 ? v : undefined;
}

/** A token count: a finite non-negative number floored to an integer, else undefined. */
function tokenOf(v: unknown): number | undefined {
  return typeof v === "number" && Number.isFinite(v) && v >= 0 ? Math.floor(v) : undefined;
}

interface Rec {
  wire: UsageWireRecord;
  /** Bumped on every change, so a post that raced a change leaves the record dirty. */
  version: number;
  sentVersion: number;
  /** A change to this already-sent record was lost for want of pending room (counted once). */
  lost?: boolean;
}

const COLUMNS = [
  "input_tokens",
  "cache_read_input_tokens",
  "cache_creation_input_tokens",
  "output_tokens",
] as const;

/** Per-leg recorder: one `query()`. Never throws; every input is untrusted SDK data. */
export class UsageLeg {
  readonly legId = randomUUID();
  private readonly byId = new Map<string, Rec>();
  /** Ids dropped for memory (up to USAGE_DROPPED_IDS_MAX), so a later frame of the same message is
   *  not recorded half-way. Past the cap a rejection is counted but not remembered. */
  private readonly droppedIds = new Set<string>();
  /** The current message id per lane (`parent_tool_use_id ?? "main"`), set by message_start. */
  private readonly lane = new Map<string, string>();
  private ordinal = 0;
  private droppedCount = 0;
  private closed = false;
  /** closed_through/dropped_records changed since the last accepted post. */
  private markerVersion = 0;
  private markerSent = 0;
  /** A leg that has not recorded or closed anything needs no marker. */
  private markerNeeded = false;
  private readonly dirty = new Set<Rec>();

  constructor(private readonly owner: UsageRecorder) {}

  /** The leg's highest ordinal so far: stamped on the result frame as `usage_through`. */
  get maxOrdinal(): number {
    return this.ordinal;
  }

  /** Record an `assistant` frame (before projection and signal filtering). */
  observeAssistant(frame: Record<string, unknown>): void {
    try {
      const message = asRecord(frame["message"]);
      const id = asString(message?.["id"]);
      if (id === undefined || this.closed) return;
      const model = asString(message?.["model"]);
      if (model === SYNTHETIC_MODEL) return;
      const rec = this.upsert(id, model, isSubagentFrame(frame), frame["session_id"]);
      if (rec) this.merge(rec, asRecord(message?.["usage"]));
    } catch {
      // never fail a frame over its usage
    }
  }

  /** Record a `stream_event` frame: message_start creates/updates the lane's current message,
   *  message_delta carries its final usage. Everything else (pings, content deltas) is ignored. */
  observeStream(frame: Record<string, unknown>): void {
    try {
      if (this.closed) return;
      const event = asRecord(frame["event"]);
      const type = event?.["type"];
      const parent = asString(frame["parent_tool_use_id"]);
      const laneKey = parent ?? "main";
      if (type === "message_start") {
        const message = asRecord(event?.["message"]);
        const id = asString(message?.["id"]);
        if (id === undefined) return;
        if (asString(message?.["model"]) === SYNTHETIC_MODEL) return;
        this.lane.set(laneKey, id);
        const rec = this.upsert(id, asString(message?.["model"]), parent !== undefined, frame["session_id"]);
        if (rec) this.merge(rec, asRecord(message?.["usage"]));
      } else if (type === "message_delta") {
        const id = this.lane.get(laneKey);
        const rec = id === undefined ? undefined : this.byId.get(id);
        if (!rec) return;
        this.merge(rec, asRecord(event?.["usage"]), true);
      }
    } catch {
      // never fail a frame over its usage
    }
  }

  /** The leg's query ended: enqueue the close marker (closed_through = highest ordinal). */
  close(): void {
    if (this.closed) return;
    this.closed = true;
    // A leg that recorded nothing has nothing to close: no marker, no leg row on the api.
    if (this.ordinal === 0 && this.droppedCount === 0) return;
    this.touchMarker();
  }

  private touchMarker(): void {
    this.markerNeeded = true;
    this.markerVersion++;
    this.owner.changed();
  }

  private upsert(id: string, model: string | undefined, subagent: boolean, frameSession: unknown): Rec | undefined {
    const existing = this.byId.get(id);
    if (existing) {
      if (model !== undefined && existing.wire.model === "unknown") {
        existing.wire.model = model;
        this.bump(existing);
      }
      return existing;
    }
    if (this.droppedIds.has(id)) return undefined;
    if (this.byId.size >= USAGE_LEG_MAX_MESSAGES || this.owner.pendingTotal(this) >= USAGE_PENDING_MAX) {
      if (this.droppedIds.size < USAGE_DROPPED_IDS_MAX) this.droppedIds.add(id);
      this.owner.noteUnconfirmedLoss();
      this.droppedCount++;
      this.touchMarker();
      return undefined;
    }
    const wire: UsageWireRecord = {
      message_id: id,
      leg_id: this.legId,
      ordinal: ++this.ordinal,
      model: model ?? "unknown",
      subagent,
      input_tokens: 0,
      cache_read_input_tokens: 0,
      cache_creation_input_tokens: 0,
      output_tokens: 0,
      output_final: false,
    };
    const fs = asString(frameSession);
    if (fs !== undefined) wire.frame_session_id = fs;
    const rec: Rec = { wire, version: 1, sentVersion: 0 };
    this.byId.set(id, rec);
    this.dirty.add(rec);
    this.owner.changed();
    return rec;
  }

  /** GREATEST-merge a usage object into the record; `final` marks output_final. */
  private merge(rec: Rec, usage: Record<string, unknown> | undefined, final = false): void {
    let changed = false;
    if (usage) {
      for (const col of COLUMNS) {
        const v = tokenOf(usage[col]);
        if (v !== undefined && v > rec.wire[col]) {
          rec.wire[col] = v;
          changed = true;
        }
      }
      const split = asRecord(usage["cache_creation"]);
      for (const [key, col] of [
        ["ephemeral_5m_input_tokens", "cache_creation_5m_input_tokens"],
        ["ephemeral_1h_input_tokens", "cache_creation_1h_input_tokens"],
      ] as const) {
        const v = tokenOf(split?.[key]);
        if (v !== undefined && v > (rec.wire[col] ?? -1)) {
          rec.wire[col] = v;
          changed = true;
        }
      }
      for (const key of ["service_tier", "speed", "inference_geo"] as const) {
        const s = asString(usage[key]);
        if (s !== undefined && rec.wire[key] !== s) {
          rec.wire[key] = s;
          changed = true;
        }
      }
    }
    if (final && !rec.wire.output_final) {
      rec.wire.output_final = true;
      changed = true;
    }
    if (changed) this.bump(rec);
  }

  private bump(rec: Rec): void {
    rec.version++;
    // A record already sent re-enters the pending set only while there is room for it (the cap is
    // recorder-wide). A change it could not carry (a final output count) is counted as dropped, once
    // per record, so coverage reads partial rather than complete-but-undercounted.
    if (!this.dirty.has(rec)) {
      if (this.owner.pendingTotal(this) >= USAGE_PENDING_MAX) {
        if (!rec.lost) {
          this.owner.noteUnconfirmedLoss();
          rec.lost = true;
          this.droppedCount++;
          this.touchMarker();
        }
        return;
      }
      this.dirty.add(rec);
    }
    this.owner.changed();
  }

  // --- the recorder's view ---

  /** @internal */
  get pendingCount(): number {
    return this.dirty.size;
  }

  /** @internal Up to `limit` unsent records, lowest ordinal first. */
  takeDirty(limit: number): Rec[] {
    const out: Rec[] = [];
    for (const rec of [...this.dirty].sort((a, b) => a.wire.ordinal - b.wire.ordinal)) {
      if (out.length >= limit) break;
      out.push(rec);
    }
    return out;
  }

  /** @internal The marker to send, when it changed since the last accepted post. */
  markerToSend(): { marker: UsageWireLeg; version: number } | undefined {
    if (!this.markerNeeded || this.markerSent >= this.markerVersion) return undefined;
    const marker: UsageWireLeg = { leg_id: this.legId };
    if (this.closed) marker.closed_through = this.ordinal;
    if (this.droppedCount > 0) marker.dropped_records = this.droppedCount;
    return { marker, version: this.markerVersion };
  }

  /** @internal An accepted post: clear what it carried unless it changed meanwhile. */
  markSent(sent: ReadonlyMap<Rec, number>, markerVersion: number | undefined): void {
    for (const [rec, version] of sent) {
      rec.sentVersion = version;
      if (rec.version === version) this.dirty.delete(rec);
    }
    if (markerVersion !== undefined) this.markerSent = Math.max(this.markerSent, markerVersion);
  }

  /** @internal Drop everything unsent (an abandoned drain, a stopped recorder). */
  abandon(): void {
    this.dirty.clear();
    this.markerSent = this.markerVersion;
  }

  /** @internal A closed leg with nothing left to send holds no more memory worth keeping. */
  get finished(): boolean {
    return this.closed && this.dirty.size === 0 && this.markerSent >= this.markerVersion;
  }

  /** @internal The records of this leg a rejected request carried: counted as dropped. */
  countDropped(n: number): void {
    if (n <= 0) return;
    this.owner.noteUnconfirmedLoss();
    this.droppedCount += n;
    this.touchMarker();
  }
}

export interface UsageRecorderOptions {
  client: UsageClient;
  runId: string;
  /** The claim generation stamped on every post (0 = legacy: never sent, the client decides). */
  claimGeneration: number;
  log: Logger;
  /** Tests shrink these. */
  debounceMs?: number;
  requestTimeoutMs?: number;
  backoffBaseMs?: number;
}

/** The run's usage sender. Created beside the run's MessageBatcher, which drains it. */
export class UsageRecorder implements UsageSink {
  private readonly legs: UsageLeg[] = [];
  private timer: NodeJS.Timeout | undefined;
  private inFlight: { done: Promise<void>; abort: AbortController } | undefined;
  private failures = 0;
  /** Monotonic count of failed attempts (a drain compares it to see whether its attempt failed). */
  private failedAttempts = 0;
  private backoffUntil = 0;
  private stopped = false;
  private unconfirmedLoss = false;
  private confirming = false;
  private strictRetry = false;
  private retryRequest: ReturnType<UsageRecorder["buildRequest"]>;

  /** @internal Loss remains observable even after a finished leg is pruned. */
  noteUnconfirmedLoss(): void { this.unconfirmedLoss = true; }
  /** A missing receipt after refused/abandoned usage cannot be repaired by a later drain. */
  get hasUnconfirmedLoss(): boolean { return this.unconfirmedLoss; }
  private readonly debounceMs: number;
  private readonly requestTimeoutMs: number;
  private readonly backoffBaseMs: number;

  constructor(private readonly opts: UsageRecorderOptions) {
    this.debounceMs = opts.debounceMs ?? USAGE_DEBOUNCE_MS;
    this.requestTimeoutMs = opts.requestTimeoutMs ?? USAGE_REQUEST_TIMEOUT_MS;
    this.backoffBaseMs = opts.backoffBaseMs ?? USAGE_BACKOFF_BASE_MS;
  }

  /** Begin a leg (one `query()`). */
  startLeg(): UsageLeg {
    const leg = new UsageLeg(this);
    if (!this.stopped && !routeDisabled) this.legs.push(leg);
    return leg;
  }

  /** True once this recorder stopped (a stale/fenced answer) or the route is disabled. */
  get inactive(): boolean {
    return this.stopped || routeDisabled;
  }

  /** @internal Called by a leg when it has something new to send. */
  changed(): void {
    if (this.inactive) return;
    this.schedule(this.debounceMs);
  }

  private schedule(delayMs: number): void {
    if (this.timer || this.inFlight || this.inactive || this.confirming ||
        (this.strictRetry && this.retryRequest)) return;
    const wait = Math.max(delayMs, this.backoffUntil - Date.now());
    this.timer = setTimeout(() => {
      this.timer = undefined;
      void this.pump();
    }, wait);
    this.timer.unref?.();
  }

  /** @internal Distinct pending messages across every leg (plus `leg` itself when unregistered). */
  pendingTotal(leg: UsageLeg): number {
    let n = this.legs.includes(leg) ? 0 : leg.pendingCount;
    for (const l of this.legs) n += l.pendingCount;
    return n;
  }

  /**
   * The run's batcher closed: nothing more can be delivered or retried. Abort the in-flight request,
   * drop what is unsent and stop scheduling, so a persistently failing api cannot keep a closed
   * recorder retrying (the unsent tail surfaces server-side as leg_not_closed / ordinal_gap).
   */
  release(): void {
    if (this.stopped) return;
    const unsent = this.hasWork();
    this.stopped = true;
    this.inFlight?.abort.abort();
    this.abandonUnsent();
    this.legs.length = 0;
    if (this.timer) clearTimeout(this.timer);
    this.timer = undefined;
    if (unsent) this.opts.log.warn("usage: closed with unsent usage records; abandoned them", { run_id: this.opts.runId });
  }

  private hasWork(): boolean {
    return this.legs.some((l) => l.pendingCount > 0 || l.markerToSend() !== undefined);
  }

  private buildRequest(): { body: UsageWireRequest; sent: Map<UsageLeg, Map<Rec, number>>; markers: Map<UsageLeg, number> } | undefined {
    const body: UsageWireRequest = { legs: [], messages: [] };
    const sent = new Map<UsageLeg, Map<Rec, number>>();
    const markers = new Map<UsageLeg, number>();
    for (const leg of this.legs) {
      const room = USAGE_POST_MAX_RECORDS - body.messages.length;
      if (room > 0) {
        const recs = leg.takeDirty(room);
        if (recs.length > 0) {
          const m = new Map<Rec, number>();
          for (const r of recs) {
            body.messages.push({ ...r.wire });
            m.set(r, r.version);
          }
          sent.set(leg, m);
        }
      }
      if (body.legs.length < USAGE_POST_MAX_LEGS) {
        const mk = leg.markerToSend();
        // A marker travels only once every record of its leg is on the wire (this request or an
        // earlier one), so a close never overtakes the records it closes.
        if (mk && leg.pendingCount === (sent.get(leg)?.size ?? 0)) {
          body.legs.push(mk.marker);
          markers.set(leg, mk.version);
        }
      }
    }
    if (body.messages.length === 0 && body.legs.length === 0) return undefined;
    return { body, sent, markers };
  }

  /** Send one request (the next slice of pending records and markers), if there is one. */
  private async pump(ignoreBackoff = false): Promise<void> {
    if (this.inactive || this.inFlight) return;
    if (!ignoreBackoff && Date.now() < this.backoffUntil) {
      this.schedule(0);
      return;
    }
    const req = this.strictRetry ? this.retryRequest ?? this.buildRequest() : this.buildRequest();
    if (!req) return;
    this.retryRequest = req;
    const abort = new AbortController();
    const signal = AbortSignal.any([abort.signal, AbortSignal.timeout(this.requestTimeoutMs)]);
    const done = this.send(req, abort, signal);
    this.inFlight = { done, abort };
    try {
      await done;
    } finally {
      if (this.inFlight?.done === done) this.inFlight = undefined;
    }
    this.pruneLegs();
    if (this.hasWork()) this.schedule(0);
  }

  private async send(
    req: NonNullable<ReturnType<UsageRecorder["buildRequest"]>>,
    abort: AbortController,
    signal: AbortSignal,
  ): Promise<void> {
    const accept = (): void => {
      this.retryRequest = undefined;
      if (!this.confirming) this.strictRetry = false;
      for (const leg of new Set([...req.sent.keys(), ...req.markers.keys()])) {
        leg.markSent(req.sent.get(leg) ?? new Map(), req.markers.get(leg));
      }
    };
    try {
      await this.opts.client.postUsage(this.opts.runId, req.body, this.opts.claimGeneration, signal);
      this.failures = 0;
      this.backoffUntil = 0;
      routeMissingStreak = 0;
      accept();
    } catch (err) {
      if (abort.signal.aborted) return; // abandoned by a drain: the records were dropped there
      this.classify(err, req, accept);
    }
  }

  private classify(
    err: unknown,
    req: NonNullable<ReturnType<UsageRecorder["buildRequest"]>>,
    accept: () => void,
  ): void {
    const { log, runId } = this.opts;
    if (err instanceof RequestError) {
      if (err.status === 404 && errorReason(err) === "stale") {
        this.stop("run not owned");
        return;
      }
      if (err.status === 409) {
        // stale_claim (a newer claim superseded this one) or a missing claim generation: either way
        // this recorder cannot post again for this claim.
        this.stop("claim fence");
        return;
      }
      if (err.status === 404 || err.status === 405) {
        routeMissingStreak++;
        if (routeMissingStreak >= USAGE_ROUTE_MISSING_LIMIT) {
          routeDisabled = true;
          this.stop("no /usage route");
        } else {
          this.backoff();
          log.warn("usage: untyped 404/405; backing off", { run_id: runId, streak: routeMissingStreak });
        }
        return;
      }
      if (err.status >= 400 && err.status < 500 && err.status !== 408 && err.status !== 429) {
        // The api refused this request outright; retrying it would loop. Drop it and count the
        // records so coverage says so.
        this.noteUnconfirmedLoss();
        log.warn("usage: the api refused a usage report; dropping it", { run_id: runId, status: err.status });
        for (const [leg, m] of req.sent) leg.countDropped(m.size);
        accept();
        return;
      }
    }
    this.backoff();
    log.warn("usage: report failed; retrying", { run_id: runId, error: errMessage(err) });
  }

  private backoff(): void {
    this.failures++;
    this.failedAttempts++;
    const delay = Math.min(this.backoffBaseMs * 2 ** (this.failures - 1), USAGE_BACKOFF_MAX_MS);
    this.backoffUntil = Date.now() + delay;
  }

  private stop(why: string): void {
    if (this.stopped) return;
    this.stopped = true;
    this.abandonUnsent();
    this.legs.length = 0;
    if (this.timer) clearTimeout(this.timer);
    this.timer = undefined;
    this.opts.log.warn("usage: not recording the usage tail", { run_id: this.opts.runId, why });
  }

  /** Forget everything unsent. The legs stay registered: an open leg keeps recording and a later
   *  change to it posts again (the records dropped here surface as ordinal_gap / leg_not_closed). */
  private abandonUnsent(): void {
    if (this.hasWork() || this.inFlight) this.noteUnconfirmedLoss();
    this.retryRequest = undefined;
    for (const leg of this.legs) leg.abandon();
    this.pruneLegs();
  }

  private pruneLegs(): void {
    for (let i = this.legs.length - 1; i >= 0; i--) if (this.legs[i]!.finished) this.legs.splice(i, 1);
  }

  /** ACK-only settlement. One failure ends this call; replay remains pending.
   * Cancellation aborts and awaits ownership rather than abandoning the tail.
   */
  async drainConfirmed(deadlineMs = USAGE_DRAIN_DEADLINE_MS, signal?: AbortSignal): Promise<boolean> {
    if (!Number.isSafeInteger(deadlineMs) || deadlineMs < 0)
      throw new RangeError("confirmed usage deadline must be a nonnegative safe integer");
    if (this.confirming) return false;
    this.confirming = true;
    this.strictRetry = true;
    if (this.timer) clearTimeout(this.timer);
    this.timer = undefined;
    const expiry = AbortSignal.any([AbortSignal.timeout(Math.max(0, Math.min(deadlineMs, USAGE_DRAIN_DEADLINE_MS))),
      ...(signal ? [signal] : [])]);
    const abort = (): void => { this.inFlight?.abort.abort(); };
    expiry.addEventListener("abort", abort, { once: true });
    const failed = this.failedAttempts;
    try {
      while (!expiry.aborted && !this.inactive && !this.unconfirmedLoss &&
        this.failedAttempts === failed && (this.inFlight || this.hasWork())) {
        await (this.inFlight?.done ?? this.pump(true));
        if (this.inFlight) await Promise.resolve();
      }
      if (expiry.aborted) abort();
      await this.inFlight?.done;
      return !expiry.aborted && !this.inactive && !this.unconfirmedLoss &&
        this.failedAttempts === failed && !this.hasWork();
    } finally {
      expiry.removeEventListener("abort", abort);
      this.confirming = false;
      if (!this.retryRequest) this.strictRetry = false;
      if (this.hasWork()) this.schedule(0);
    }
  }

  /** Post pending usage within the deadline, abandoning unsent records on expiry.
   * Unlike drainConfirmed, refusal or abandonment can settle this ordinary drain.
   */
  async drain(deadlineMs: number = USAGE_DRAIN_DEADLINE_MS, signal?: AbortSignal): Promise<void> {
    if (this.confirming) return;
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = undefined;
    }
    if (this.inactive) {
      this.abandonUnsent();
      return;
    }
    const expired = new AbortController();
    const timer = setTimeout(() => expired.abort(), Math.max(0, Math.min(deadlineMs, USAGE_DRAIN_DEADLINE_MS)));
    const onCaller = (): void => expired.abort();
    if (signal?.aborted) expired.abort();
    else signal?.addEventListener("abort", onCaller, { once: true });
    const expiry = new Promise<"expired">((resolve) => {
      if (expired.signal.aborted) resolve("expired");
      else expired.signal.addEventListener("abort", () => resolve("expired"), { once: true });
    });
    try {
      // A failed attempt ends the drain (the records stay pending for the debounced retry with
      // backoff): an api outage must not turn every flush into a multi-second stall.
      const failedAtStart = this.failedAttempts;
      while (!expired.signal.aborted && !this.inactive && this.failedAttempts === failedAtStart && (this.inFlight || this.hasWork())) {
        const settled = this.inFlight ? this.inFlight.done : this.pump(true);
        if ((await Promise.race([settled.then(() => "done" as const), expiry])) === "expired") break;
        this.pruneLegs();
      }
      if (expired.signal.aborted && !this.inactive && (this.inFlight || this.hasWork())) {
        this.inFlight?.abort.abort();
        this.abandonUnsent();
        this.opts.log.warn("usage: drain deadline passed; abandoned the unsent usage records", { run_id: this.opts.runId });
      }
    } finally {
      clearTimeout(timer);
      signal?.removeEventListener("abort", onCaller);
    }
  }
}

/** The api's typed error reason (`{"error","reason"}`), or undefined for an untyped body. */
function errorReason(err: RequestError): string | undefined {
  try {
    const parsed: unknown = JSON.parse(err.body);
    const reason = asRecord(parsed)?.["reason"];
    if (typeof reason === "string") return reason;
  } catch {
    // an untyped (or proxy) body: the status is the signal
  }
  return undefined;
}

/** Tests: forget the process-wide disable. */
export function resetUsageRouteStateForTests(): void {
  routeDisabled = false;
  routeMissingStreak = 0;
}
