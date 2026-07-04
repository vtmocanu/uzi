import type { WorkerClient } from "./client.js";
import type { Logger } from "./log.js";
import type { EmittedMessage } from "./executor.js";
import type { OutgoingMessage } from "./protocol.js";
import { errMessage, sleep } from "./util.js";

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
 */
export class MessageBatcher {
  private buffer: OutgoingMessage[] = [];
  private seq: number;
  private timer: NodeJS.Timeout | undefined;
  private flushing = false;
  private closed = false;
  /** The currently running flush, if any, so close() can await it. */
  private inFlight: Promise<void> | undefined;

  constructor(
    private readonly client: WorkerClient,
    private readonly runId: string,
    lastSeq: number,
    private readonly batchMs: number,
    private readonly log: Logger,
  ) {
    this.seq = lastSeq;
  }

  emit(msg: EmittedMessage): void {
    if (this.closed) return;
    this.seq += 1;
    const out: OutgoingMessage = { seq: this.seq, kind: msg.kind, payload: msg.payload };
    if (msg.agent !== undefined) out.agent = msg.agent;
    this.buffer.push(out);
    this.scheduleFlush();
  }

  /** Highest seq assigned so far (for pinning / diagnostics). */
  currentSeq(): number {
    return this.seq;
  }

  private scheduleFlush(): void {
    if (this.timer || this.closed) return;
    this.timer = setTimeout(() => {
      this.timer = undefined;
      void this.flush();
    }, this.batchMs);
    // Don't keep the event loop alive just for a pending flush.
    this.timer.unref?.();
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

  private async doFlush(): Promise<void> {
    const batch = this.buffer;
    this.buffer = [];
    try {
      await this.client.postMessages(this.runId, batch);
    } catch (err) {
      // Put the batch back at the head so order + gaplessness hold on retry.
      this.buffer = batch.concat(this.buffer);
      this.log.warn("message batch flush failed, will retry", { run_id: this.runId, count: batch.length, error: errMessage(err) });
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
    for (let attempt = 0; attempt < 3 && this.buffer.length > 0; attempt++) {
      await this.flush();
      if (this.buffer.length > 0) await sleep(200 * (attempt + 1));
    }
    if (this.buffer.length > 0) {
      this.log.warn("message batcher closed with undelivered messages", { run_id: this.runId, dropped: this.buffer.length });
    }
  }
}
