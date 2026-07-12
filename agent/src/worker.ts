import type { WorkerClient } from "./client.js";
import type { RunRunner } from "./runner.js";
import type { ChatRunner } from "./chat-runner.js";
import type { Logger } from "./log.js";
import type { Config } from "./config.js";
import { errMessage, sleep } from "./util.js";

/**
 * Outbound-only worker loop (multica's daemon model): register once, heartbeat on
 * an interval, and poll for claims. No inbound ports.
 *
 * TWO independent, CONCURRENT claim lanes (PRD #39 Decision 4): the RUN lane runs
 * one issue/ci_fix run at a time to a terminal state (today's behavior, no lane
 * param — back-compat), and the CHAT lane polls the disjoint `?lane=chat` queue at
 * WORKER_CHAT_POLL_MS and runs up to WORKER_CHAT_SESSIONS chat sessions alongside
 * the run slot. This is the first time the worker executes more than one thing at
 * once; the shared collaborators are audited for it (see chatClaimLoop).
 */
export class Worker {
  constructor(
    private readonly config: Config,
    private readonly client: WorkerClient,
    private readonly runner: RunRunner,
    private readonly chatRunner: ChatRunner,
    private readonly log: Logger,
  ) {}

  async run(signal: AbortSignal): Promise<void> {
    await this.registerWithRetry(signal);
    if (signal.aborted) return;
    // Heartbeat, the run lane, and the chat lane run concurrently until abort.
    await Promise.all([this.heartbeatLoop(signal), this.claimLoop(signal), this.chatClaimLoop(signal)]);
  }

  private async registerWithRetry(signal: AbortSignal): Promise<void> {
    let attempt = 0;
    while (!signal.aborted) {
      try {
        const res = await this.client.register(this.config.workerName, this.config.workerTemplate);
        this.log.info("registered", {
          name: this.config.workerName,
          template: this.config.workerTemplate,
          worker_id: res.worker_id ?? null,
        });
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

  /**
   * The CHAT lane (PRD #39 Decision 4), independent of and concurrent with the run
   * lane. Polls `?lane=chat` at WORKER_CHAT_POLL_MS and executes up to
   * WORKER_CHAT_SESSIONS chat sessions at once; a fresh claim is only taken when a
   * session slot is free, so a second chat queues server-side until one frees.
   *
   * Concurrency audit (Decision 4): the collaborators shared with the run lane are
   * safe under this concurrency because Node is single-threaded and their shared
   * state is only ever touched synchronously — `Logger.addSecret`/`scrub` mutate and
   * read the secret Set without an await between, so no interleave can corrupt it,
   * and `WorkerClient` holds no mutable per-call state (each request is an
   * independent fetch). Every ChatRunner builds its OWN batcher, redactor, steering,
   * and executor instance, so nothing run-scoped is shared between concurrent chats.
   */
  private async chatClaimLoop(signal: AbortSignal): Promise<void> {
    const active = new Set<Promise<void>>();
    while (!signal.aborted) {
      if (active.size >= this.config.chatSessions) {
        // All chat slots busy: wake when one frees or after a poll, then re-check.
        await Promise.race([...active, sleep(this.config.chatPollMs, signal)]);
        continue;
      }
      let claimed = false;
      try {
        const claim = await this.client.claimChat();
        if (claim) {
          claimed = true;
          // Pass the shutdown signal so an in-flight chat aborts promptly on SIGTERM
          // (the drain below then resolves quickly instead of parking until idle).
          const run = this.chatRunner
            .execute(claim, signal)
            .catch((err) => this.log.warn("chat execute failed", { error: errMessage(err) }));
          active.add(run);
          void run.finally(() => active.delete(run));
        }
      } catch (err) {
        this.log.warn("chat claim/execute cycle failed", { error: errMessage(err) });
      }
      if (!claimed) await sleep(this.config.chatPollMs, signal);
    }
    // On shutdown let in-flight chats drain (each sees the abort via its own cancel).
    await Promise.allSettled([...active]);
  }
}
