import type { WorkerClient } from "./client.js";
import type { RunRunner } from "./runner.js";
import type { Logger } from "./log.js";
import type { Config } from "./config.js";
import { errMessage, sleep } from "./util.js";

/**
 * Outbound-only worker loop (multica's daemon model): register once, heartbeat
 * on an interval, and poll for a claim. No inbound ports. One run at a time in
 * M2 (a claimed run is executed to a terminal state before the next claim).
 */
export class Worker {
  constructor(
    private readonly config: Config,
    private readonly client: WorkerClient,
    private readonly runner: RunRunner,
    private readonly log: Logger,
  ) {}

  async run(signal: AbortSignal): Promise<void> {
    await this.registerWithRetry(signal);
    if (signal.aborted) return;
    // Heartbeat and claim loops run concurrently until the signal aborts.
    await Promise.all([this.heartbeatLoop(signal), this.claimLoop(signal)]);
  }

  private async registerWithRetry(signal: AbortSignal): Promise<void> {
    let attempt = 0;
    while (!signal.aborted) {
      try {
        const res = await this.client.register(this.config.workerName);
        this.log.info("registered", { name: this.config.workerName, worker_id: res.worker_id ?? null });
        return;
      } catch (err) {
        attempt++;
        const backoff = Math.min(this.config.pollIntervalMs * attempt, 30_000);
        this.log.warn("register failed, retrying", { attempt, backoff_ms: backoff, error: errMessage(err) });
        await sleep(backoff, signal);
      }
    }
  }

  private async heartbeatLoop(signal: AbortSignal): Promise<void> {
    while (!signal.aborted) {
      try {
        await this.client.heartbeat();
      } catch (err) {
        this.log.warn("heartbeat failed", { error: errMessage(err) });
      }
      await sleep(this.config.heartbeatIntervalMs, signal);
    }
  }

  private async claimLoop(signal: AbortSignal): Promise<void> {
    while (!signal.aborted) {
      let claimed = false;
      try {
        const claim = await this.client.claimRun();
        if (claim) {
          claimed = true;
          await this.runner.execute(claim);
        }
      } catch (err) {
        this.log.warn("claim/execute cycle failed", { error: errMessage(err) });
      }
      // After finishing a run, immediately try for the next; otherwise wait a poll.
      if (!claimed) await sleep(this.config.pollIntervalMs, signal);
    }
  }
}
