import type { WorkerClient } from "./client.js";
import type { Logger } from "./log.js";
import type { EmittedMessage } from "./executor.js";
import type { OutgoingMessage } from "./protocol.js";
import type { PayloadRedactor, TextRedactor } from "./redact.js";
import { errMessage, sleep } from "./util.js";

/**
 * The api caps request bodies at `maxBodyBytes = 1 << 20` and enforces it with
 * `io.LimitReader` inside `DecodeJSON` (`api/internal/httpx/respond.go`). Over the
 * cap the body is TRUNCATED, the decode fails, and `WorkerRunMessages` answers with
 * the same generic `400 {"error":"invalid request body"}` it gives any malformed
 * batch — so **the worker can never learn "too large" from the response.** It has
 * to decide locally, from bytes it measured before sending, which is why every
 * limit here is enforced worker-side.
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

/** How many attempts that make NO progress close() will buy before giving up. A
 *  sub-batch that lands is progress and does not consume one, so a large buffer
 *  still drains fully; only genuine failure counts against the bound. */
const CLOSE_MAX_FAILED_ATTEMPTS = 3;

/** A message plus its measured wire size. Measured once at emit() so a flush never
 *  re-serialises, and so the size survives the re-buffer on failure. */
interface Buffered {
  msg: OutgoingMessage;
  bytes: number;
}

/** What one message costs on the wire, measured the way client.ts sends it. */
function messageBytes(msg: OutgoingMessage): number {
  return Buffer.byteLength(JSON.stringify(msg), "utf8");
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
  ) {
    this.seq = lastSeq;
    this.redact = redact ?? ((p) => p);
    this.redactText = redactText ?? ((s) => s);
  }

  emit(msg: EmittedMessage): void {
    if (this.closed) return;
    this.seq += 1;
    // Redact before buffering so no secret is ever persisted or broadcast, even
    // if a tool_result echoed the OAuth token from the agent's env.
    const out: OutgoingMessage = { seq: this.seq, kind: msg.kind, payload: this.redact(msg.payload) };
    if (msg.agent !== undefined) out.agent = msg.agent;
    // PRD #99: copied the same way as `agent` — present only when the frame had
    // them, so the API's pgText("") maps absence to SQL NULL rather than "".
    // Both go through redactText: they are top-level siblings of the payload, so
    // `redact` (which walks INSIDE a payload object) never touches them.
    // agent_label is the one that matters — free model-authored prose. An
    // SDK-minted `toolu_*` id cannot hold a secret, but it is scrubbed too so the
    // treatment is symmetric and a future id format cannot re-open the hole.
    if (msg.agentInstance !== undefined) out.agent_instance = this.redactText(msg.agentInstance);
    if (msg.agentLabel !== undefined) out.agent_label = this.redactText(msg.agentLabel);
    // PRD #108 M3: measure the wire size ONCE, here. A message above the hard cap
    // cannot be delivered at any batch size — the server would truncate the body
    // and 400 it forever — so replace its payload with an ASCII marker rather than
    // let it wedge the run. The seq, kind and agent are kept, so the stream stays
    // CONTIGUOUS: `web/src/lib/runStream.ts` renders nothing past a seq gap
    // (`seq > lastSeq + 1` buffers into `pending` and never advances `lastSeq`),
    // so dropping the message outright would freeze the live view for the rest of
    // the run. The marker is deliberately tiny and self-describing; the operator
    // still gets the full payload on the debug line below.
    let bytes = messageBytes(out);
    if (bytes > MAX_MESSAGE_BYTES) {
      this.log.warn("run message too large for the api to accept; replaced with a marker", {
        run_id: this.runId,
        seq: out.seq,
        kind: out.kind,
        bytes,
        max_message_bytes: MAX_MESSAGE_BYTES,
      });
      out.payload = {
        event: "message_truncated",
        reason: "the message was too large for the api to accept and was replaced by the worker",
        kind: out.kind,
        bytes,
      };
      bytes = messageBytes(out);
    }
    // Every outgoing run message passes through here — the single chokepoint for
    // dumping raw frames to the operator's `docker logs` at debug level (PRD #11
    // §4). Log the redacted payload (the child logger's SecretRegistry scrubs the
    // serialized line a second time); UZI_LOG_LEVEL=debug turns it on, info stays
    // terse. The browser never shows raw JSON — this is the debug surface.
    this.log.debug("run event", { seq: out.seq, kind: out.kind, agent: out.agent, payload: out.payload });
    this.buffer.push({ msg: out, bytes });
    this.scheduleFlush();
  }

  /** Highest seq assigned so far (for pinning / diagnostics). */
  currentSeq(): number {
    return this.seq;
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
    let total = FRAMING_BYTES;
    let n = 0;
    for (const item of this.buffer) {
      const next = total + item.bytes + 1; // +1 for the separating comma
      if (n > 0 && next > MAX_BATCH_BYTES) break;
      total = next;
      n += 1;
    }
    return this.buffer.splice(0, n);
  }

  async flush(): Promise<void> {
    if (this.flushing || this.buffer.length === 0) return;
    this.flushing = true;
    this.inFlight = this.doFlush();
    try {
      await this.inFlight;
    } finally {
      this.inFlight = undefined;
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
      while (this.buffer.length > 0) {
        const batch = this.takePrefix();
        if (batch.length === 0) break;
        try {
          await this.client.postMessages(
            this.runId,
            batch.map((b) => b.msg),
          );
          // Any success clears the backoff.
          this.consecutiveFailures = 0;
        } catch (err) {
          // Put the batch back at the head so order + gaplessness hold on retry.
          // The whole buffer no longer rides on one request, so this re-buffer can
          // no longer grow a body across the server's cap — the next attempt
          // re-splits from the same head.
          this.buffer = batch.concat(this.buffer);
          this.consecutiveFailures += 1;
          this.log.warn("message batch flush failed, will retry", {
            run_id: this.runId,
            count: batch.length,
            consecutive_failures: this.consecutiveFailures,
            error: errMessage(err),
          });
          break;
        }
      }
    } finally {
      this.flushing = false;
      if (this.buffer.length > 0 && !this.closed) this.scheduleFlush();
    }
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
