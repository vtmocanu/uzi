import type { WorkerClient } from "./client.js";
import type { RunRunner } from "./runner.js";
import type { ChatRunner } from "./chat-runner.js";
import type { JudgeRunner } from "./judge-runner.js";
import type { Logger } from "./log.js";
import type { Config } from "./config.js";
import type { WorkerStats } from "./protocol.js";
import { StatsCollector } from "./stats.js";
import { errMessage, sleep } from "./util.js";

/**
 * Outbound-only worker loop (multica's daemon model): register once, heartbeat on
 * an interval, and poll for claims. No inbound ports.
 *
 * TWO independent, CONCURRENT claim lanes (PRD #39 Decision 4): the RUN lane
 * executes up to WORKER_MAX_CONCURRENT_RUNS issue/ci_fix runs at once, bounded by a
 * slot semaphore (PRD #42 Decision 1 — default cap 1, i.e. the pre-#42 serial
 * behavior), and the CHAT lane polls the disjoint `?lane=chat` queue at
 * WORKER_CHAT_POLL_MS and runs up to WORKER_CHAT_SESSIONS chat sessions alongside
 * the run slots. The two caps are deliberately distinct knobs (run lane vs chat
 * lane). The shared collaborators are audited for this concurrency (see
 * chatClaimLoop; the per-run executor/HOME isolation is PRD #42 M1).
 */
export class Worker {
  constructor(
    private readonly config: Config,
    private readonly client: WorkerClient,
    private readonly runner: RunRunner,
    private readonly chatRunner: ChatRunner,
    private readonly judgeRunner: JudgeRunner,
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
        // Self-report the `docker` capability ONLY when a daemon sidecar is reachable
        // (PRD #83 Q1) — the wiring was resolved once at startup (main.ts). An array so
        // #84 can grow the vocabulary; #83 only ever sends `["docker"]` or omits it. The
        // `?.` degrades safe to "no capability" if wiring is somehow unset: registration
        // is a load-bearing loop that must never throw on a config quirk.
        const capabilities = this.config.dockerWiring?.dockerHost ? ["docker"] : undefined;
        const res = await this.client.register(
          this.config.workerName,
          this.config.workerTemplate,
          this.config.maxConcurrentRuns,
          capabilities,
        );
        this.log.info("registered", {
          name: this.config.workerName,
          template: this.config.workerTemplate,
          capabilities: capabilities ?? [],
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
    // One collector for the loop's lifetime so the CPU% delta carries across ticks
    // (PRD #49). Its first tick omits cpu_pct (no prior sample); a worker restart
    // just re-runs that omission.
    const stats = new StatsCollector();
    while (!signal.aborted) {
      try {
        await this.client.heartbeat(this.collectStats(stats));
      } catch (err) {
        this.log.warn("heartbeat failed", { error: errMessage(err) });
      }
      await sleep(this.config.heartbeatIntervalMs, signal);
    }
  }

  /** Sample container stats, never letting a collector failure reach the heartbeat
   *  (PRD #49 Decision 3 / M1). collect() is already internally guarded; this is the
   *  belt-and-suspenders outer guard so liveness never hinges on telemetry. */
  private collectStats(collector: StatsCollector): WorkerStats | undefined {
    try {
      return collector.collect();
    } catch (err) {
      this.log.warn("stats collection failed", { error: errMessage(err) });
      return undefined;
    }
  }

  /**
   * The RUN lane, bounded by a slot semaphore (PRD #42 Decision 1). Executes up to
   * WORKER_MAX_CONCURRENT_RUNS issue/ci_fix runs concurrently as tracked promises;
   * a slot is released only when its run settles (the promise resolves/rejects),
   * held the whole time a run is parked at the plan gate inside `runner.execute`
   * (Decision 2). Slot-before-claim: a fresh claim is taken only when a slot is
   * free, so the worker never manufactures a claimed-but-waiting run and
   * `SweepClaimedNeverStarted` semantics are untouched (Decision 8; no new claim
   * SQL). At the default cap of 1 this is the pre-#42 serial loop at the observable
   * level. Mirrors chatClaimLoop's tracked-promise pool (the RUN lane has no
   * per-execution signal — a run drives its own cancel via steering, so shutdown
   * drains rather than aborts; see below).
   */
  private async claimLoop(signal: AbortSignal): Promise<void> {
    const cap = this.config.maxConcurrentRuns;
    const active = new Set<Promise<void>>();
    let loggedAtCapacity = false;
    while (!signal.aborted) {
      if (active.size >= cap) {
        // At capacity: defer the claim (never claim without a free slot) and wake
        // when a slot frees or after a poll. Log once per saturation episode so a
        // saturated worker is observable without spamming while slots stay pinned
        // (Decision 1 — multica daemon.go "poll: at capacity"). Suppressed at the
        // default cap of 1, where "busy with the one run" is the steady state, not
        // saturation — this keeps the default path's output identical to pre-#42.
        if (cap > 1 && !loggedAtCapacity) {
          this.log.info("run lane at capacity, deferring claim", { active: active.size, cap });
          loggedAtCapacity = true;
        }
        await Promise.race([...active, sleep(this.config.pollIntervalMs, signal)]);
        continue;
      }
      loggedAtCapacity = false;
      let claimed = false;
      try {
        const claim = await this.client.claimRun();
        if (claim) {
          claimed = true;
          // PRD #46: a judge claim rides the same run lane (it counts toward worker
          // capacity, Decision 8) but is executed by the slim JudgeRunner — no
          // clone/worktree/git, just fetch the trace, call the model, post the review.
          const exec =
            claim.kind === "judge" ? this.judgeRunner.execute(claim) : this.runner.execute(claim);
          const run = exec.catch((err) =>
            this.log.warn("claim/execute cycle failed", { error: errMessage(err) }),
          );
          active.add(run);
          void run.finally(() => active.delete(run));
        }
      } catch (err) {
        this.log.warn("claim/execute cycle failed", { error: errMessage(err) });
      }
      // A claim yielded a run: immediately loop to fill the next free slot (up to
      // cap). Otherwise wait a poll before asking again.
      if (!claimed) await sleep(this.config.pollIntervalMs, signal);
    }
    // On shutdown, drain every in-flight run (mirrors chatClaimLoop's drain). A run
    // carries no shutdown signal — it runs to its terminal state; the container's
    // SIGKILL grace is the hard backstop and the sweeper requeues anything the kill
    // interrupts, so a stuck run can't wedge shutdown past the grace window.
    await Promise.allSettled([...active]);
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
