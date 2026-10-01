/**
 * Issue #1800: reports to the api which owner follow-ups this worker really put into an executor
 * prompt (POST /worker/runs/{id}/inputs/included), so a follow-up consumed but never rendered can
 * be recovered on the next claim. Shared by SteeringChannel and ChatSteering.
 *
 * An id is stamped at most once per reporter (mark is idempotent). A flush is ONE request for
 * every queued id; it never throws. A transient failure keeps the ids queued for the next flush.
 * An UNTYPED 404/405 is ambiguous (an api pod that predates the route, mid rolling upgrade): the
 * ids stay queued and flush backs off, skipping 1, 2, 4... (capped) poll ticks, and only
 * INCLUSION_ROUTE_MISSING_LIMIT of them since the last accepted receipt stop reporting. The reporter also stops, for
 * good, on a typed not-owned 404 (reason "stale") or a receipt saying the claim is inactive (the
 * next claim's requeue recovers what is left). Any other 4xx drops the batch.
 */
import { RequestError, type WorkerClient } from "./client.js";
import type { Logger } from "./log.js";
import { errMessage } from "./util.js";

/** Most ids one /inputs/included accepts (the api's validInputIDs bound). */
const MAX_INCLUDE_BATCH = 1000;
/** Attempts a stopping channel makes to flush what is still queued, so a failing api never holds
 *  the shutdown. Whatever is left is recovered by the next claim's requeue. */
const STOP_INCLUDE_ATTEMPTS = 3;
/** Untyped 404/405 answers since the last accepted receipt (across flushes) after which reporting
 *  stops for the claim. With the backoff below the 8th answer lands after 1+2+4+8+16+16+16 = 63
 *  skipped ticks, so the window before a permanent stop is about 70 poll ticks: roughly 70s for
 *  chat (1s poll) and 3.5 minutes for steering, long enough for a rolling api upgrade. */
export const INCLUSION_ROUTE_MISSING_LIMIT = 8;
/** Cap on the flush() calls skipped after an untyped 404/405: the k-th skips min(2^(k-1), cap). */
export const INCLUSION_ROUTE_MISSING_BACKOFF_MAX_TICKS = 16;

/** The api's typed error reason (`{"error","reason"}`), or undefined for an untyped body. */
function errorReason(err: RequestError): string | undefined {
  try {
    const parsed: unknown = JSON.parse(err.body);
    if (typeof parsed === "object" && parsed !== null) {
      const reason = (parsed as Record<string, unknown>).reason;
      if (typeof reason === "string") return reason;
    }
  } catch {
    // an untyped (or proxy) body: the status is the signal
  }
  return undefined;
}

export class InclusionReporter {
  private readonly seen = new Set<number>();
  private queue: number[] = [];
  private stopped = false;
  private missingStreak = 0;
  private skipTicks = 0;

  constructor(
    private readonly client: WorkerClient,
    private readonly runId: string,
    private readonly claimGeneration: number,
    private readonly log: Logger,
  ) {}

  /** Queue `id` for a receipt; a no-op for an id already marked on this channel. */
  mark(id: number): void {
    if (this.stopped || this.seen.has(id)) return;
    this.seen.add(id);
    this.queue.push(id);
  }

  get pending(): boolean {
    return this.queue.length > 0;
  }

  /** One request for the queued ids (a no-op when none, or while backing off). Never throws. */
  async flush(): Promise<void> {
    if (this.skipTicks > 0) {
      this.skipTicks--;
      return;
    }
    await this.send();
  }

  private async send(): Promise<void> {
    const ids = this.queue.slice(0, MAX_INCLUDE_BATCH);
    if (this.stopped || !ids.length) return;
    try {
      const receipt = await this.client.includeInputs(this.runId, ids, this.claimGeneration);
      if (typeof receipt?.active !== "boolean") throw new Error("invalid input included response");
      this.missingStreak = 0;
      this.skipTicks = 0;
      this.drop(ids);
      if (!receipt.active) {
        // The claim was superseded or released; its follow-ups are recovered by the claim that
        // replaced it, so nothing later on this reporter can be taken either.
        this.stop("the claim is no longer active");
      }
    } catch (err) {
      if (err instanceof RequestError && errorReason(err) === "stale" && err.status === 404) {
        this.stop("run not owned");
      } else if (err instanceof RequestError && (err.status === 404 || err.status === 405)) {
        this.missingStreak++;
        if (this.missingStreak >= INCLUSION_ROUTE_MISSING_LIMIT) {
          this.stop("no /inputs/included route");
        } else {
          this.skipTicks = Math.min(2 ** (this.missingStreak - 1), INCLUSION_ROUTE_MISSING_BACKOFF_MAX_TICKS);
          this.log.warn("steering: inclusion receipt got an untyped 404/405; backing off", { run_id: this.runId, streak: this.missingStreak });
        }
      } else if (err instanceof RequestError && err.status >= 400 && err.status < 500 && err.status !== 408 && err.status !== 429) {
        this.drop(ids);
        this.log.warn("steering: the api refused an inclusion receipt; dropping it", { run_id: this.runId, error: errMessage(err) });
      } else {
        this.log.warn("steering: inclusion receipt failed; retrying", { run_id: this.runId, error: errMessage(err) });
      }
    }
  }

  /** Flush at stop: bounded attempts that ignore the tick backoff, then give up (the next claim's
   *  requeue recovers the rest). */
  async drain(): Promise<void> {
    for (let i = 0; i < STOP_INCLUDE_ATTEMPTS && this.pending; i++) await this.send();
  }

  private stop(why: string): void {
    this.stopped = true;
    this.queue = [];
    this.log.warn("steering: not reporting inclusion receipts", { run_id: this.runId, why });
  }

  private drop(ids: number[]): void {
    const gone = new Set(ids);
    this.queue = this.queue.filter((id) => !gone.has(id));
  }
}
