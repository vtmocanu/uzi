import type { WorkerClient } from "./client.js";
import { RequestError, isTransient } from "./client.js";
import type { Logger } from "./log.js";
import type { EmittedMessage } from "./executor.js";
import type { OutgoingMessage } from "./protocol.js";
import type { PayloadRedactor, TextRedactor } from "./redact.js";
import { emptyCounts, countsTotal, sanitizePayload, sanitizeText } from "./sanitize.js";
import { errMessage, sleep } from "./util.js";

/**
 * Deliberately well below the api's own cap, whose counterpart is `maxBodyBytes =
 * 1 << 20` in `api/internal/httpx/respond.go`. That constant is unexported and the
 * api advertises it nowhere, so any number here is a DUPLICATE that can silently
 * drift — which is the argument for a conservative one: a cap this far under makes
 * the server's 413 a backstop the worker should essentially never reach, instead of
 * the mechanism it depends on.
 *
 * **Local accounting is primary; the 413 is the backstop, and it must stay that way
 * — "no 413" is NOT proof the body was under the cap.** `http.MaxBytesReader` fires
 * on the READ, not on the value, so if the decoder finishes the top-level value
 * before a read crosses the limit, an over-cap body can succeed rather than 413.
 * That is buffering-dependent in both directions and cannot be relied on either way.
 *
 * `MAX_BATCH_BYTES` is a SOFT grouping target: a flush takes the longest head
 * prefix that fits. `MAX_MESSAGE_BYTES` is the HARD limit, and the two are
 * deliberately far apart. A message between them is postable — it just goes in a
 * sub-batch of its own — so it must not be mangled; only a message above the hard
 * limit can never be delivered under any grouping.
 */
export const MAX_BATCH_BYTES = 512 * 1024;

/**
 * A single message above this can never be delivered: it does not fit the server's
 * 1 MiB cap even alone. Set just under that cap (not at the batch cap) so the
 * worker never truncates a message the SERVER would have accepted — that would be
 * a regression for a legitimately chatty tool result, and today's code delivers
 * those fine.
 */
export const MAX_MESSAGE_BYTES = 900 * 1024;

/** Ceiling on the retry delay. At 30s a wedged run polls at 0.03 Hz instead of the
 *  incident's ~2 Hz, so riding out even a long outage costs nothing. */
export const MAX_BACKOFF_MS = 30_000;

/** ±20%, so a fleet of workers that all broke at once does not retry in lockstep. */
const BACKOFF_JITTER = 0.2;

/** `{"messages":[]}` plus a little slack for a future top-level field. */
const FRAMING_BYTES = 32;

/**
 * Posts one bisection may spend isolating a poisoned message. One-sided search
 * costs `ceil(log2 n)` — 8 for the incident's n = 239 — so 24 is a wide backstop,
 * not a working budget. Exhausting it with more than one candidate left trips the
 * breaker rather than searching on: an unbounded search is the bug being fixed.
 */
export const MAX_BISECT_POSTS = 24;

/**
 * How long an unbroken run of TRANSIENT failures may last before the breaker
 * trips. Deliberately generous, and it is a duration rather than a count.
 *
 * After the api's M2 lands, a 5xx on this route means a GENUINE transient — the
 * poison class returns 400 — so a tight `N=5` would fail healthy runs through an
 * ordinary api restart, which is Decision 4's mirror-image bug and exactly what
 * this PRD must not introduce. The hot loop is already dead from backoff alone
 * (0.03 Hz at the 30s cap), so waiting costs almost nothing and the breaker's real
 * job is the PERMANENT class: 401/403/404, a rejected tombstone, or an exhausted
 * bisect budget, all of which trip immediately.
 */
export const TRANSIENT_TRIP_MS = 10 * 60_000;

/**
 * How the batcher must react to a failed post. The whole point of PRD #108 is that
 * a status code is a retry CONTRACT, so this classification is the design.
 */
type Verdict =
  /** 5xx, 408, 429, or any transport failure — retry with backoff. */
  | "transient"
  /**
   * 413. The SIZE path, never the poison path: split and retry, do not spend
   * bisect budget, do not tombstone above size 1, and RESET the failure streak —
   * a batch that legitimately splits is progress, not a repeat failure, and
   * counting it would trip the breaker on a perfectly healthy run.
   */
  | "oversize"
  /** 400 and other 4xx — this batch can never succeed as-is. Bisect. */
  | "permanent"
  /** 401/403/404 — every message fails; bisecting only burns the budget proving it. */
  | "fatal";

/**
 * `isTransient` (`client.ts`) is `>= 500 || 408 || 429`, so 413 already falls out as
 * non-transient — but only by accident of not being in that set. It is pinned by an
 * explicit test rather than inherited, because the 413 arm depends on it.
 */
function classify(err: unknown): Verdict {
  if (err instanceof RequestError) {
    if (err.status === 413) return "oversize";
    if (err.status === 401 || err.status === 403 || err.status === 404) return "fatal";
    if (isTransient(err)) return "transient";
    return "permanent";
  }
  return "transient";
}

/** How many attempts that make NO progress close() will buy before giving up. A
 *  sub-batch that lands is progress and does not consume one, so a large buffer
 *  still drains fully; only genuine failure counts against the bound. */
const CLOSE_MAX_FAILED_ATTEMPTS = 3;

/** A message plus its measured wire size. Measured once at emit() so a flush never
 *  re-serialises, and so the size survives the re-buffer on failure. */
interface Buffered {
  msg: OutgoingMessage;
  bytes: number;
  /**
   * True once this entry IS a worker-minted marker. Load-bearing against an
   * infinite loop: without it, a tombstone the api also rejects gets tombstoned
   * again, and again, forever — the very shape of bug this PRD exists to remove.
   * A rejected tombstone is the one true drop, and it trips the breaker.
   */
  tombstoned?: boolean;
}

/** What one message costs on the wire, measured the way client.ts sends it. */
function messageBytes(msg: OutgoingMessage): number {
  return Buffer.byteLength(JSON.stringify(msg), "utf8");
}

/** Why a message was replaced. `message_dropped` = the api rejected the payload;
 *  `message_truncated` = it was too large to be accepted at all. */
type TombstoneEvent = "message_dropped" | "message_truncated";

/**
 * Replace a message's payload in place, KEEPING its seq, with a small ASCII marker.
 *
 * **Tombstone, never drop** (PRD #108, overriding the PRD's own "drop only that
 * one"). `web/src/lib/runStream.ts` requires seq CONTIGUITY: `ingest` buffers any
 * `seq > lastSeq + 1` into `pending`, returns `gap: true`, and does not advance
 * `messages` or `lastSeq`. A permanently-missing seq therefore freezes the live run
 * view at the poison for the rest of the run while the browser re-requests a replay
 * the server can never satisfy — trading a server-side wedge for a client-side one.
 *
 * The marker rides as **`kind: "status"` with a `text` field**, and both halves are
 * load-bearing. `describeStatus` (`web/src/components/RunEvent.tsx`) checks `text`
 * FIRST and returns it verbatim, and falls back to `status: <event>` for an unknown
 * event — so the tombstone renders either way, and the run's history shows the loss
 * instead of hiding it. The ORIGINAL kind rides inside the payload, where it is
 * data rather than a routing decision.
 */
function tombstone(item: Buffered, event: TombstoneEvent, reason: string, redactText: TextRedactor): Buffered {
  // Already a tombstone → return it unchanged. A marker that is re-buffered and then
  // grouped with following messages can escape the `batch.length === 1` guard in
  // handleFailure and reach here a second time; re-tombstoning it would overwrite the
  // ORIGINAL kind/size (the only forensic content a tombstone carries) with this
  // marker's own — "status", a few hundred bytes — losing what was lost. Idempotent
  // by construction (PRD #108 B5).
  if (item.tombstoned) return item;
  const originalKind = item.msg.kind;
  const text =
    event === "message_dropped"
      ? `message dropped: ${reason} (kind ${originalKind}, ${item.bytes} bytes)`
      : `message too large to deliver: ${reason} (kind ${originalKind}, ${item.bytes} bytes)`;
  const msg: OutgoingMessage = {
    seq: item.msg.seq,
    kind: "status",
    // The text is worker-minted ASCII and cannot carry a secret, but it is scrubbed
    // anyway so a future edit that interpolates payload content cannot open a hole.
    payload: { text: redactText(text), event, reason, kind: originalKind, bytes: item.bytes },
  };
  // Carry the whole attribution triple, not just `agent`. Without agent_instance and
  // agent_label the marker leaves the subagent lane it belongs to (RunEvent groups on
  // agent_instance) and renders in the top-level stream — so the loss is shown, but
  // not where it happened. These are already scrubbed on the original frame at emit().
  if (item.msg.agent !== undefined) msg.agent = item.msg.agent;
  if (item.msg.agent_instance !== undefined) msg.agent_instance = item.msg.agent_instance;
  if (item.msg.agent_label !== undefined) msg.agent_label = item.msg.agent_label;
  return { msg, bytes: messageBytes(msg), tombstoned: true };
}

/** Injected by each runner so a breaker trip can be reported out of band. */
export interface PermanentFailureInfo {
  reason: string;
  lastSeq: number;
  dropped: number;
}

export interface MessageBatcherOptions {
  /**
   * Called ONCE when the breaker trips. It must not travel through the batcher:
   * `concat` is order-preserving, so an emitted explanation queues behind the
   * poison and never lands, and after a trip nothing flushes at all. Each runner
   * wires this to its own `reportState`, which has bounded retries, 4xx-fatal
   * semantics and already-terminal-is-success.
   */
  onPermanentFailure?: (info: PermanentFailureInfo) => void;
}

/**
 * Buffers run messages and flushes them in seq-numbered batches (PRD: 500ms),
 * continuing numbering from the claim's `last_seq` so a resuming worker never
 * collides seqs (→ no dropped messages, replayable stream).
 *
 * Delivery is best-effort but order-preserving and gapless-for-what-is-sent: a
 * failed batch is put back at the head of the buffer and retried on the next
 * flush, so the server (idempotent on (run_id, seq)) never sees a gap in what it
 * receives. The server persists every message before broadcasting, so this
 * channel is a liveness optimization, never the source of truth.
 *
 * PRD #108 M3 (steps 1-3 of the blocking order in Decision 4): the batch is
 * BOUNDED and SPLIT, and retries back off exponentially. Before this, `doFlush`
 * posted the entire buffer and, on failure, concatenated it back at the head while
 * new messages piled on behind — so a wedged run's body grew monotonically until
 * it crossed the server's 1 MiB cap, at which point the failure changed class from
 * 500 to a 400 no amount of waiting could clear (measured in
 * `batcher-poison.test.ts`: the crossing landed at 1,082,141 bytes).
 *
 * That growth is why "never retry a 4xx" must not ship on its own: a healthy run
 * merely riding out a transient outage would grow into a permanent 400 it never
 * earned, failing a run that was fine. Capping and splitting first removes the
 * growth, so the 4xx rule can later be applied to genuine poison only.
 */
export class MessageBatcher {
  private buffer: Buffered[] = [];
  /** Consecutive FAILED flushes; drives the backoff delay and resets on any 2xx. */
  private consecutiveFailures = 0;
  /** When the current unbroken run of failures started, for the transient trip. */
  private failingSince: number | undefined;
  /** Terminal: the breaker tripped. Nothing flushes again. */
  private tripped = false;
  private tripReason: string | undefined;
  /** Cap on the NEXT prefix's message count, set when a 413 says to split. */
  private splitLimit: number | undefined;
  private permanentFailureHandler: ((info: PermanentFailureInfo) => void) | undefined;
  private seq: number;
  private timer: NodeJS.Timeout | undefined;
  private flushing = false;
  private closed = false;
  /** The currently running flush, if any, so close() can await it. */
  private inFlight: Promise<void> | undefined;

  /** Scrubs known secrets from every payload before it leaves the worker. */
  private readonly redact: PayloadRedactor;
  /**
   * Scrubs known secrets from the top-level STRING fields that ride beside the
   * payload. `redact` walks a payload object and never sees its siblings, so
   * without this an `agent_label` — free, model-authored prose (PRD #99) — would
   * reach Postgres, the /api/ws frame, the browser and `uzi run logs` unscrubbed.
   * That is precisely the class redact.ts exists for: the OAuth token lives in
   * the agent subprocess env as CLAUDE_CODE_OAUTH_TOKEN, and payload scrubbing is
   * the defense-in-depth net behind the guardrails' credential-read denial.
   */
  private readonly redactText: TextRedactor;

  constructor(
    private readonly client: WorkerClient,
    private readonly runId: string,
    lastSeq: number,
    private readonly batchMs: number,
    private readonly log: Logger,
    redact?: PayloadRedactor,
    redactText?: TextRedactor,
    opts: MessageBatcherOptions = {},
  ) {
    this.seq = lastSeq;
    this.redact = redact ?? ((p) => p);
    this.redactText = redactText ?? ((s) => s);
    this.permanentFailureHandler = opts.onPermanentFailure;
  }

  emit(msg: EmittedMessage): void {
    if (this.closed) {
      // A post-close emit CANNOT be delivered — close() has already drained — but
      // it must not vanish silently. All three runner paths reach here: the success
      // path closes the batcher and then reports terminal state, and a throw from
      // that report lands in a catch that emits an `error` frame into the closed
      // batcher (`runner.ts`, and the same shape in `chat-runner.ts`). A silently
      // dropped error frame is precisely the class of failure this PRD exists to
      // remove, so say so. The information still reaches the product through
      // `reportState`'s `failure_reason`; what would be lost is the operator's
      // ability to see that it happened.
      this.log.warn("run message emitted after the batcher closed; not delivered", {
        run_id: this.runId,
        kind: msg.kind,
        agent: msg.agent,
      });
      return;
    }
    this.seq += 1;
    // SANITIZE BEFORE REDACT, and the order is a security property, not a style
    // choice. The redactors are exact-substring matchers (`redact.ts`), so a secret
    // carrying an embedded NUL does not match and survives redaction — and
    // stripping the NUL afterwards would then reconstitute it in the clear, in the
    // payload, on the wire and in the browser. Stripping first means the redactor
    // sees the same bytes the server will.
    const counts = emptyCounts();
    const sanitized = sanitizePayload(msg.payload, counts);
    // `kind` and `agent` are worker-side vocabularies, not model output, but redaction
    // lives ONLY in the worker (the api never redacts), so an unscrubbed value reaches
    // Postgres, the /api/ws frame, the browser and `uzi run logs` in the clear. Scrub
    // both the same way agent_instance/agent_label are below — sanitize then redact —
    // so the treatment is symmetric and a future kind/agent source cannot re-open the
    // hole (PRD #108 B6). The kind cast is safe: kind is a closed vocabulary, so
    // scrubbing a legitimate value never changes it.
    const out: OutgoingMessage = {
      seq: this.seq,
      kind: this.redactText(sanitizeText(msg.kind, counts)) as OutgoingMessage["kind"],
      payload: this.redact(sanitized),
    };
    if (msg.agent !== undefined) out.agent = this.redactText(sanitizeText(msg.agent, counts));
    // PRD #99: copied the same way as `agent` — present only when the frame had
    // them, so the API's pgText("") maps absence to SQL NULL rather than "".
    // Both go through redactText: they are top-level siblings of the payload, so
    // `redact` (which walks INSIDE a payload object) never touches them.
    // agent_label is the one that matters — free model-authored prose. An
    // SDK-minted `toolu_*` id cannot hold a secret, but it is scrubbed too so the
    // treatment is symmetric and a future id format cannot re-open the hole.
    // Same order for the siblings: `agent_label` is free model-authored prose and
    // Postgres `text` cannot hold a NUL any more than `jsonb` can, so a NUL here
    // wedges a run entirely outside a payload-only fix.
    if (msg.agentInstance !== undefined) out.agent_instance = this.redactText(sanitizeText(msg.agentInstance, counts));
    if (msg.agentLabel !== undefined) out.agent_label = this.redactText(sanitizeText(msg.agentLabel, counts));
    // Count and log every strip, so a NUL-emitting tool stays visible rather than
    // being silently laundered (PRD #108, Risk "sanitation hiding a real bug").
    if (countsTotal(counts) > 0) {
      this.log.warn("stripped codepoints Postgres cannot store from a run message", {
        run_id: this.runId,
        seq: this.seq,
        kind: msg.kind,
        nul: counts.nul,
        unpaired_surrogate: counts.surrogate,
      });
    }
    // PRD #108 M3: measure the wire size ONCE, here, so no flush re-serialises and
    // the size survives the re-buffer on failure. A message above the HARD cap can
    // never be delivered at any batch size, so it is tombstoned right here — which
    // deletes the unsplittable-oversize case from the flush state machine entirely.
    let item: Buffered = { msg: out, bytes: messageBytes(out) };
    if (item.bytes > MAX_MESSAGE_BYTES) {
      this.log.warn("run message too large for the api to accept; replaced with a marker", {
        run_id: this.runId,
        seq: out.seq,
        kind: out.kind,
        bytes: item.bytes,
        max_message_bytes: MAX_MESSAGE_BYTES,
      });
      item = tombstone(item, "message_truncated", "the api cannot accept a message this large", this.redactText);
    }
    // Every outgoing run message passes through here — the single chokepoint for
    // dumping raw frames to the operator's `docker logs` at debug level (PRD #11
    // §4). Log the redacted payload (the child logger's SecretRegistry scrubs the
    // serialized line a second time); UZI_LOG_LEVEL=debug turns it on, info stays
    // terse. The browser never shows raw JSON — this is the debug surface.
    this.log.debug("run event", { seq: out.seq, kind: out.kind, agent: out.agent, payload: out.payload });
    this.buffer.push(item);
    this.scheduleFlush();
  }

  /** Highest seq assigned so far (for pinning / diagnostics). */
  currentSeq(): number {
    return this.seq;
  }

  /** True once the breaker has tripped and nothing more will be delivered. */
  isTripped(): boolean {
    return this.tripped;
  }

  /**
   * Wire the breaker's out-of-band report. A setter rather than a constructor
   * argument because both runners build their `reportState` closure AFTER the
   * batcher (it captures the session id the executor later observes), and
   * reordering that would be a bigger change than this one line.
   */
  onPermanentFailureReport(handler: (info: PermanentFailureInfo) => void): void {
    this.permanentFailureHandler = handler;
  }

  /**
   * Delay before the next flush: `batchMs` while healthy, and after a failure
   * `batchMs * 2^n` capped at `MAX_BACKOFF_MS`, with ±20% jitter.
   *
   * This replaces the fixed sub-second cadence that made the incident a hot loop —
   * measured on the unfixed batcher, 62 posts in 400ms with a flat 6.49ms gap.
   */
  private nextDelayMs(): number {
    if (this.consecutiveFailures === 0) return this.batchMs;
    const backoff = Math.min(this.batchMs * 2 ** this.consecutiveFailures, MAX_BACKOFF_MS);
    const jitter = backoff * BACKOFF_JITTER * (Math.random() * 2 - 1);
    return Math.max(0, Math.round(backoff + jitter));
  }

  private scheduleFlush(): void {
    if (this.timer || this.closed) return;
    this.timer = setTimeout(() => {
      this.timer = undefined;
      void this.flush();
    }, this.nextDelayMs());
    // Don't keep the event loop alive just for a pending flush.
    this.timer.unref?.();
  }

  /**
   * The longest head PREFIX of the buffer that fits `MAX_BATCH_BYTES`, removed
   * from the buffer.
   *
   * The cap is a grouping target, not a hard limit: a single message larger than
   * it goes out ALONE rather than never, because it is under `MAX_MESSAGE_BYTES`
   * and the server will take it. Only the first message gets that exemption, so
   * one large message can never drag a whole batch over with it.
   *
   * Always a PREFIX, and sub-batches go out strictly sequentially in ascending
   * seq: reordering would deliver WS frames out of order and trip `runStream`'s
   * gap path, and concurrency would break the single-flight `inFlight` invariant
   * `close()` depends on.
   */
  private takePrefix(): Buffered[] {
    // A 413 means the local byte accounting under-counted, so the next prefix is
    // additionally capped by message COUNT until something lands.
    const limit = this.splitLimit ?? this.buffer.length;
    let total = FRAMING_BYTES;
    let n = 0;
    for (const item of this.buffer) {
      if (n >= limit) break;
      const next = total + item.bytes + 1; // +1 for the separating comma
      if (n > 0 && next > MAX_BATCH_BYTES) break;
      total = next;
      n += 1;
    }
    return this.buffer.splice(0, n);
  }

  async flush(): Promise<void> {
    if (this.flushing || this.buffer.length === 0 || this.tripped) return;
    this.flushing = true;
    this.inFlight = this.doFlush();
    try {
      await this.inFlight;
    } finally {
      this.inFlight = undefined;
    }
  }

  /**
   * Trip the breaker: stop retrying and report out of band, exactly once.
   *
   * The explanation must NOT be emitted — `concat` is order-preserving, so it would
   * queue behind the poison, and after a trip nothing flushes anyway. It goes to
   * the injected `onPermanentFailure`, which each runner wires to `reportState`.
   * `reportState` does no redaction of its own (`client.ts`), so the text is passed
   * through `redactText` here; `redact.ts` names `failure_reason` as exactly this
   * case, so the precedent is already in the file.
   */
  private trip(reason: string, lastSeq: number): void {
    if (this.tripped) return;
    this.tripped = true;
    this.tripReason = reason;
    const dropped = this.buffer.length;
    const text = this.redactText(
      `message persistence failed permanently: ${reason}; ${dropped} buffered message(s) were not delivered. ` +
        `Work up to seq ${Math.max(0, lastSeq - 1)} is persisted. This is a message-transport failure, not an agent failure.`,
    );
    this.log.error("message batcher breaker tripped", {
      run_id: this.runId,
      reason,
      last_seq: lastSeq,
      dropped,
    });
    try {
      this.permanentFailureHandler?.({ reason: text, lastSeq, dropped });
    } catch (err) {
      // A throwing handler must not take the batcher (or the run) with it.
      this.log.warn("permanent-failure handler threw", { run_id: this.runId, error: errMessage(err) });
    }
  }

  /**
   * Isolate the single poisoned message in a batch the api permanently rejected,
   * tombstone it, and let everything else land.
   *
   * ONE-SIDED search, and that is what makes the PRD's "~8 round-trips for 239"
   * true. The candidate window `[lo, hi)` is known to contain a poison. Post its
   * left half:
   *
   *  - it FAILS permanently  → the poison is on the left; `hi = mid`. The right
   *    half was never posted and stays queued.
   *  - it SUCCEEDS           → those rows are now persisted and *confirmed clean*
   *    by a real 2xx; `lo = mid`.
   *
   * One post per level, the window strictly halves, so it terminates at size 1 in
   * `ceil(log2 n)` posts. (The two-sided variant posts both halves per level and
   * costs up to 16 for the same n — that is what made the PRD's number look wrong.)
   *
   * Returns the messages that still need delivering, in seq order.
   */
  private async bisect(batch: Buffered[]): Promise<Buffered[]> {
    let lo = 0;
    let hi = batch.length;
    let posts = 0;
    while (hi - lo > 1) {
      if (posts >= MAX_BISECT_POSTS) {
        this.trip(
          `a poisoned message could not be isolated within ${MAX_BISECT_POSTS} posts`,
          batch[lo]!.msg.seq,
        );
        return [];
      }
      const mid = lo + Math.floor((hi - lo) / 2);
      const half = batch.slice(lo, mid);
      posts += 1;
      try {
        await this.client.postMessages(
          this.runId,
          half.map((b) => b.msg),
        );
        lo = mid; // confirmed clean by a 2xx
      } catch (err) {
        const verdict = classify(err);
        if (verdict === "fatal") {
          this.trip(`the api rejected the run's messages with ${errMessage(err)}`, batch[lo]!.msg.seq);
          return [];
        }
        if (verdict === "transient") {
          // The search cannot conclude anything from a transient failure. Abandon
          // it and let the ordinary backed-off retry re-drive from the same head;
          // everything before `lo` is already persisted, so no work is repeated
          // that the server's (run_id, seq) idempotency does not absorb anyway.
          this.log.warn("bisection abandoned on a transient failure; will retry", {
            run_id: this.runId,
            error: errMessage(err),
          });
          return batch.slice(lo);
        }
        if (verdict === "oversize") {
          // A 413 is a SIZE signal, NOT evidence about the payload — so it must not
          // narrow the poison window. `hi = mid` on a 413 would eventually tombstone
          // whatever seq lands at `lo`, which need not be the poison at all: measured,
          // it tombstoned a CLEAN seq and wrote "payload rejected by the api" — a false
          // statement — into the run's permanent history, worse than the loss itself.
          // Abandon the search exactly like a transient and hand the window back; the
          // OUTER oversize arm splits it, and everything before `lo` is already
          // persisted (PRD #108 B1).
          this.log.warn("bisection abandoned on an oversize (413) verdict; the outer arm will re-split", {
            run_id: this.runId,
            error: errMessage(err),
          });
          return batch.slice(lo);
        }
        // Permanent: the poison is on the left.
        hi = mid;
      }
    }
    if (hi - lo < 1) return batch.slice(lo); // nothing left to isolate

    // `batch[lo]` is the poison. Tombstone it under its OWN seq so the stream stays
    // contiguous, and post it alone.
    const poison = batch[lo]!;
    const marker = tombstone(poison, "message_dropped", "payload rejected by the api", this.redactText);
    this.log.warn("isolated a permanently-rejected message; replacing it with a tombstone", {
      run_id: this.runId,
      seq: poison.msg.seq,
      kind: poison.msg.kind,
      bytes: poison.bytes,
      bisect_posts: posts,
    });
    try {
      await this.client.postMessages(this.runId, [marker.msg]);
      return batch.slice(lo + 1);
    } catch (err) {
      if (classify(err) === "transient") {
        // Keep the tombstone (not the original) queued: the payload is known bad.
        return [marker, ...batch.slice(lo + 1)];
      }
      // A rejected TOMBSTONE is the only true drop, and it means something is wrong
      // beyond the payload — the marker is worker-minted ASCII.
      this.trip(`the api rejected even the worker's replacement marker for seq ${poison.msg.seq}`, poison.msg.seq);
      return [];
    }
  }

  /**
   * Drain the buffer in byte-capped prefixes, one request in flight at a time,
   * stopping at the first failure (the rest stays buffered for the backed-off
   * retry).
   *
   * The loop keeps `flush()`'s contract — "get what is buffered onto the stream" —
   * intact now that one request may not cover the buffer. `runner.ts` relies on
   * exactly that when it flushes the plan before opening the approval gate. In the
   * ordinary case the buffer is far under the cap, this runs once, and the wire
   * behaviour is identical to before.
   */
  private async doFlush(): Promise<void> {
    try {
      while (this.buffer.length > 0 && !this.tripped) {
        const batch = this.takePrefix();
        if (batch.length === 0) break;
        try {
          await this.client.postMessages(
            this.runId,
            batch.map((b) => b.msg),
          );
          // Any success clears the backoff, the failure clock and the split limit.
          this.consecutiveFailures = 0;
          this.failingSince = undefined;
          this.splitLimit = undefined;
        } catch (err) {
          if (await this.handleFailure(batch, err)) break;
        }
      }
    } finally {
      this.flushing = false;
      if (this.buffer.length > 0 && !this.closed && !this.tripped) this.scheduleFlush();
    }
  }

  /**
   * React to one failed sub-batch. Returns true when the flush loop should stop and
   * wait for the backed-off retry, false when it should keep going immediately.
   */
  private async handleFailure(batch: Buffered[], err: unknown): Promise<boolean> {
    const verdict = classify(err);
    const lastSeq = batch[0]?.msg.seq ?? this.seq;

    // A rejected TOMBSTONE is the one true drop. The marker is worker-minted ASCII
    // a few hundred bytes long, so if the api refuses even that, the problem is not
    // this payload and no further replacement can help. Checked FIRST, before any
    // arm that would replace it again: without this the batcher re-tombstones the
    // same seq forever — an infinite loop of exactly the shape this PRD removes.
    // (Found by a test that hung for 122 seconds, not by inspection.)
    if (verdict !== "transient" && batch.length === 1 && batch[0]?.tombstoned) {
      this.trip(`the api rejected even the worker's replacement marker for seq ${lastSeq}`, lastSeq);
      return true;
    }

    if (verdict === "fatal") {
      this.buffer = batch.concat(this.buffer);
      this.trip(`the api rejected the run's messages with ${errMessage(err)}`, lastSeq);
      return true;
    }

    if (verdict === "oversize") {
      if (batch.length > 1) {
        // SPLIT AND RETRY, never a poison verdict. And critically: this RESETS the
        // failure streak. A batch that legitimately splits is progress, so counting
        // it would read as "the same batch failed N times" and trip the breaker on
        // a perfectly healthy run — Decision 4's failure mode arriving through the
        // 413 door.
        this.splitLimit = Math.max(1, Math.floor(batch.length / 2));
        this.buffer = batch.concat(this.buffer);
        this.consecutiveFailures = 0;
        this.failingSince = undefined;
        this.log.warn("api rejected the batch as too large; splitting and retrying", {
          run_id: this.runId,
          count: batch.length,
          next_max_count: this.splitLimit,
        });
        return false; // retry immediately, smaller
      }
      // A single message the api calls too large means the emit-time cap failed.
      const only = batch[0]!;
      this.log.error("api rejected a SINGLE message as too large; the emit-time cap did not catch it", {
        run_id: this.runId,
        seq: only.msg.seq,
        kind: only.msg.kind,
        bytes: only.bytes,
        max_message_bytes: MAX_MESSAGE_BYTES,
      });
      this.buffer = [tombstone(only, "message_truncated", "the api rejected the message as too large", this.redactText), ...this.buffer];
      this.splitLimit = undefined;
      return false;
    }

    if (verdict === "permanent") {
      if (batch.length === 1) {
        const only = batch[0]!;
        this.log.warn("api permanently rejected a message; replacing it with a tombstone", {
          run_id: this.runId,
          seq: only.msg.seq,
          kind: only.msg.kind,
          error: errMessage(err),
        });
        this.buffer = [tombstone(only, "message_dropped", "payload rejected by the api", this.redactText), ...this.buffer];
        return false;
      }
      const remaining = await this.bisect(batch);
      this.buffer = remaining.concat(this.buffer);
      this.consecutiveFailures = 0;
      this.failingSince = undefined;
      return this.tripped;
    }

    // Transient. Put the batch back at the head so order + gaplessness hold. The
    // whole buffer no longer rides on one request, so this re-buffer can no longer
    // grow a body across the server's cap — the next attempt re-splits.
    this.buffer = batch.concat(this.buffer);
    this.consecutiveFailures += 1;
    const now = Date.now();
    this.failingSince ??= now;
    this.log.warn("message batch flush failed, will retry", {
      run_id: this.runId,
      count: batch.length,
      consecutive_failures: this.consecutiveFailures,
      failing_for_ms: now - this.failingSince,
      error: errMessage(err),
    });
    if (now - this.failingSince >= TRANSIENT_TRIP_MS) {
      this.trip(
        `the api has been unreachable or failing for ${Math.round((now - this.failingSince) / 1000)}s`,
        lastSeq,
      );
    }
    return true;
  }

  /** Stop accepting messages and drain the buffer with a few bounded retries. */
  async close(): Promise<void> {
    this.closed = true;
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = undefined;
    }
    // A flush kicked off by the batch timer may be in flight; await it so a
    // failed flush re-buffers its batch before we decide what is left to drain
    // (otherwise close() could observe an empty buffer and return while the
    // in-flight flush later fails and strands those messages).
    if (this.inFlight) await this.inFlight;
    // A tripped breaker skips the drain entirely. The 3 attempts plus 600ms of
    // sleeps below exist to ride out a blip; a trip has already established that
    // this is not a blip, so buying three more futile round-trips only delays the
    // run's terminal report. The `await this.inFlight` above still runs first and
    // unconditionally — that guard is what stops close() observing an empty buffer
    // while a doomed flush is still airborne, and a trip does not make it less true.
    if (this.tripped) {
      if (this.buffer.length > 0) {
        this.log.warn("message batcher closed with undelivered messages", {
          run_id: this.runId,
          dropped: this.buffer.length,
          trip_reason: this.tripReason,
          last_rejected_seq: this.buffer[0]?.msg.seq,
        });
      }
      return;
    }
    // Bounded by FAILED attempts, not by total attempts (PRD #108 M3). A flush now
    // posts only a byte-capped prefix, so a large buffer needs several successful
    // rounds to drain; counting those against the bound would strand messages that
    // were landing fine. Progress — the buffer shrank — is free and does not
    // consume an attempt; only an attempt that moved nothing does.
    let failed = 0;
    while (failed < CLOSE_MAX_FAILED_ATTEMPTS && this.buffer.length > 0) {
      const before = this.buffer.length;
      await this.flush();
      if (this.buffer.length === 0) break;
      if (this.buffer.length < before) continue; // a sub-batch landed; keep going
      failed += 1;
      await sleep(200 * failed);
    }
    if (this.buffer.length > 0) {
      this.log.warn("message batcher closed with undelivered messages", { run_id: this.runId, dropped: this.buffer.length });
    }
  }
}
