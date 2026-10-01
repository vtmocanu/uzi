/**
 * Issue #1800: reports to the api which owner follow-ups this worker really put into an executor
 * prompt (POST /worker/runs/{id}/inputs/included), so a follow-up consumed but never rendered can
 * be recovered on the next claim. Shared by SteeringChannel and ChatSteering.
 *
 * An id is stamped at most once per reporter (mark is idempotent). A flush is ONE request for
 * every queued id; it never throws. A transient failure keeps the ids queued for the next flush,
 * a definitive refusal or an inactive claim drops them (the claim ended, or the api will never
 * take them), and a 404/405 means the api cannot take receipts at all (a pod that predates the
 * route, or a run it does not own), so the reporter stops sending.
 */
import { RequestError, type WorkerClient } from "./client.js";
import type { Logger } from "./log.js";
import { errMessage } from "./util.js";

/** Most ids one /inputs/included accepts (the api's validInputIDs bound). */
const MAX_INCLUDE_BATCH = 1000;
/** Attempts a stopping channel makes to flush what is still queued, so a failing api never holds
 *  the shutdown. Whatever is left is recovered by the next claim's requeue. */
const STOP_INCLUDE_ATTEMPTS = 3;

export class InclusionReporter {
  private readonly seen = new Set<number>();
  private queue: number[] = [];
  private unsupported = false;

  constructor(
    private readonly client: WorkerClient,
    private readonly runId: string,
    private readonly claimGeneration: number,
    private readonly log: Logger,
  ) {}

  /** Queue `id` for a receipt; a no-op for an id already marked on this channel. */
  mark(id: number): void {
    if (this.unsupported || this.seen.has(id)) return;
    this.seen.add(id);
    this.queue.push(id);
  }

  get pending(): boolean {
    return this.queue.length > 0;
  }

  /** One request for the queued ids (a no-op when none). Never throws. */
  async flush(): Promise<void> {
    const ids = this.queue.slice(0, MAX_INCLUDE_BATCH);
    if (!ids.length) return;
    try {
      const receipt = await this.client.includeInputs(this.runId, ids, this.claimGeneration);
      if (typeof receipt?.active !== "boolean") throw new Error("invalid input included response");
      // Active or not, these ids are settled: an inactive claim was superseded or released, and
      // its follow-ups are recovered by the claim that replaced it.
      this.drop(ids);
    } catch (err) {
      if (err instanceof RequestError && (err.status === 404 || err.status === 405)) {
        this.unsupported = true;
        this.queue = [];
        this.log.warn("steering: the api cannot take inclusion receipts (no /inputs/included, or run not owned); not reporting", {
          run_id: this.runId,
        });
      } else if (err instanceof RequestError && err.status >= 400 && err.status < 500 && err.status !== 408 && err.status !== 429) {
        this.drop(ids);
        this.log.warn("steering: the api refused an inclusion receipt; dropping it", { run_id: this.runId, error: errMessage(err) });
      } else {
        this.log.warn("steering: inclusion receipt failed; retrying", { run_id: this.runId, error: errMessage(err) });
      }
    }
  }

  /** Flush at stop: bounded attempts, then give up (the next claim's requeue recovers the rest). */
  async drain(): Promise<void> {
    for (let i = 0; i < STOP_INCLUDE_ATTEMPTS && this.pending; i++) await this.flush();
  }

  private drop(ids: number[]): void {
    const gone = new Set(ids);
    this.queue = this.queue.filter((id) => !gone.has(id));
  }
}
