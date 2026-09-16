import type { WorkerClient } from "./client.js";
import { RequestError } from "./client.js";
import { replaySegment } from "./batcher.js";
import type { Outbox } from "./outbox.js";
import type { RunRunner } from "./runner.js";
import type { ChatRunner } from "./chat-runner.js";
import type { JudgeRunner } from "./judge-runner.js";
import type { ReviewRunner } from "./review-runner.js";
import type { Logger } from "./log.js";
import type { Config } from "./config.js";
import type { ActiveSnapshot, OutboxHeartbeatEntry, WorkerStats } from "./protocol.js";
import type { ActiveRunRegistry } from "./active-run-registry.js";
import { StatsCollector } from "./stats.js";
import { errMessage, sleep } from "./util.js";
import { toolchainPreflight, type PreflightResult } from "./toolchain-preflight.js";
import { CODEX_HARNESS_CAPABILITY } from "./codex/codex-runtime-probe.js";

/**
 * Outbound-only worker loop (a daemon model): register once, heartbeat on
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
    private readonly reviewRunner: ReviewRunner,
    private readonly log: Logger,
    // PRD #92 M3: the boot toolchain preflight, injectable so the concurrency/semaphore
    // unit tests (which run on a non-image host with no `/opt/uzi-toolchain`) can pass a
    // stub; production uses the real check against the runner PATH.
    private readonly preflight: () => PreflightResult = () => toolchainPreflight(process.env),
    // PRD #1391 M2: the worker-owned message outbox (main.ts builds + inits it before
    // constructing the worker). The per-worker drainer replays every run's spilled
    // segments over the messages route on each successful heartbeat and once on boot,
    // and re-arms each run's live batcher (via `rearm`) once its segments retire.
    // Undefined only in the concurrency/semaphore unit tests that never spill.
    private readonly outbox?: Outbox,
    // PRD #1391 M2: the shared re-arm registry — runId → the live batcher's `rearm()`.
    // A RunRunner/ChatRunner registers its batcher here while it holds pending
    // segments; the drainer calls the hook once the run's segments retire so the live
    // batcher returns its flush target to the network.
    private readonly rearm?: Map<string, () => void>,
    // PRD #1390 M2a: the shared active-run registry the run lane + judge/review runners
    // write as they start/transition/finish. The worker READS it to build the
    // ActiveSnapshot that rides every heartbeat and run-lane claim. Undefined in the
    // concurrency/semaphore unit tests that never negotiate the feature — buildActiveSnapshot
    // then returns undefined and no snapshot is ever sent.
    private readonly activeRuns?: ActiveRunRegistry,
  ) {}

  /** PRD #1391 M2: single-flight guard — never two outbox drains at once (a heartbeat
   *  tick must not start a drain while the boot drain, or a prior tick's drain, is
   *  still running). */
  private draining = false;

  async run(signal: AbortSignal): Promise<void> {
    // PRD #92 M3 — fail-loud boot toolchain preflight, BEFORE the register retry loop.
    // A worker whose `/nix` store is missing the baked go/python3/gcc/pip/openssl (a stale seed
    // after an image roll — see PRD #92 root cause) must FAIL REGISTRATION visibly, not
    // retry forever (the toolchain won't appear by retrying) or emit silent 127s to
    // subagents mid-run. THROW so it propagates to main.ts's fatal handler (exit 1) and
    // the pod surfaces the error to an operator — do NOT swallow it into registerWithRetry.
    const pf = this.preflight();
    if (!pf.ok) {
      this.log.error("toolchain preflight FAILED — refusing to register", {
        missing: pf.missing,
        likely_cause:
          "the baked worker toolchain is missing from the runner PATH — most likely a stale /nix seed after an image roll (PRD #92): the seed init container tars /nix into the PVC once, so a rolled image does not re-seed an existing worker",
      });
      throw new Error(
        `toolchain preflight failed: missing ${pf.missing.join(", ")} — baked worker toolchain not on the runner PATH (likely a stale /nix seed after an image roll; see PRD #92)`,
      );
    }
    await this.registerWithRetry(signal);
    if (signal.aborted) return;
    // PRD #1296 M3 (D3/D5) — after registering (so the worker is authenticated), re-drive
    // any durable-recovery capture the previous life left journaled: re-upload the exact
    // journaled bundle bytes with NO forge PAT. Fire-and-forget and fully swallowed — a
    // resume failure must never block the claim loops, and the source stays protected by
    // the journal + the server custody hold regardless.
    void this.runner.resumePendingRecoveries(signal).catch((err) => {
      this.log.warn("recovery: restart resume sweep failed", { error: errMessage(err) });
    });
    // PRD #1391 M2: admit any spill tail a crash may have lost, then drain the outbox
    // once in the background. On boot, for each run whose `spilled_unclean` flag was set
    // at init, log ONCE that an unflushed tail MAY have been lost — naming NO span and
    // NO count, because nothing durable can size it (the crash-before-range-flush
    // admitted-loss log). Then a boot drain replays any pending segments without waiting
    // for the first heartbeat. Fire-and-forget and fully swallowed — a drain must never
    // block the claim loops.
    if (this.outbox) {
      for (const runId of this.outbox.uncleanRuns()) {
        this.log.warn(
          "outbox: a run was spilling when the worker last stopped; an unflushed tail may have been lost",
          { run_id: runId },
        );
      }
      void this.drainOutbox(signal).catch((err) => {
        this.log.warn("outbox boot drain failed", { error: errMessage(err) });
      });
    }
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
        // Self-report the PROTOCOL capabilities this image implements (PRD #1226 M1, D2).
        // This image always implements the structural completion protocol, so it
        // unconditionally announces completion_interlock_v1. It also implements the PRD
        // #1296 durable-recovery archive protocol (D9), so it announces
        // recovery_archive_v1 — the flag M1's ClaimRun reads to open a custody hold for
        // this worker on a code-publishing run — AND the PRD #1349 generation-exact
        // extension recovery_archive_v2, advertised ALONGSIDE v1 during rollout (v2 is a
        // strict superset; the server's RecoveryCapable gate stays keyed on v1 until a
        // later milestone flips it, so advertising both is safe). Kept SEPARATE from
        // `capabilities` (the scheduler vocabulary) on the wire: the server stores it in
        // workers.protocol_capabilities and the ClaimRun hard clause reads it there,
        // OUTSIDE required_capabilities and the capability_aware kill-switch, so an old
        // image that omits it can never claim an interlocked run or open a custody hold.
        const protocolCapabilities = [
          "completion_interlock_v1",
          "recovery_archive_v1",
          "recovery_archive_v2",
          // PRD #1247 M5b (D3/protocol §9): this image implements the held-state credential-switch
          // protocol — it stamps claim_generation on every mutating report (already landed in W2a),
          // surfaces the credential_switch signal, and performs the two-phase release. Advertised
          // UNCONDITIONALLY (like completion_interlock_v1): the server then REQUIRES claim_generation
          // on every mutating report for this worker's fenced claims, which W2a already stamps, so it
          // is safe to land now. An image WITHOUT this flag keeps working on legacy claims, and the
          // held-state `set-token` verb 409s naming the worker.
          "credential_switch_v1",
        ];
        // PRD #1332 D3 (M5A / C2): advertise the Codex harness PROTOCOL capability ONLY
        // after a successful startup runtime probe of the pinned, image-baked Codex
        // package. main.ts resolved that probe ONCE (like dockerWiring) and stored the
        // boolean on config; a positive result APPENDS codex_harness_v1, a failed/absent
        // one leaves the array unchanged so a stripped/corrupt/mismatched/old image keeps
        // serving Claude. The `?.` degrades safe to "not capable" if the field is somehow
        // unset — registration must never throw on a config quirk. The server now ADMITS
        // codex_harness_v1 into its protocol vocabulary (FilterProtocol keeps it) and gates
        // run placement/claim on it (the ClaimRun dedicated Codex clause admits a
        // Codex-indicating run only for a worker that self-reported it). M5A stays dark
        // regardless: no public DTO/CLI/web selector exposes Codex, so advertising this
        // protocol fact is invisible to users until a later milestone lights it up.
        if (this.config.codexProbe?.capable) {
          protocolCapabilities.push(CODEX_HARNESS_CAPABILITY);
        }
        const res = await this.client.register(
          this.config.workerName,
          this.config.workerTemplate,
          this.config.maxConcurrentRuns,
          capabilities,
          protocolCapabilities,
        );
        this.log.info("registered", {
          name: this.config.workerName,
          template: this.config.workerTemplate,
          capabilities: capabilities ?? [],
          protocol_capabilities: protocolCapabilities,
          worker_id: res.worker_id ?? null,
        });
        return;
      } catch (err) {
        // A 401/403 is a PERMANENT auth rejection of the worker join token (rotated or
        // invalid): retrying can never clear it, so FAIL LOUD — throw so it propagates to
        // main.ts's fatal handler (exit 1) and the pod surfaces the error, exactly like the
        // toolchain preflight above. Every other error (network, timeout, 5xx, 408/429, any
        // other status) keeps retrying with the capped backoff below.
        if (err instanceof RequestError && (err.status === 401 || err.status === 403)) {
          this.log.error("register REJECTED — permanent auth failure, refusing to retry", {
            status: err.status,
            reason:
              "the worker join token was rejected as unauthorized/forbidden (rotated or invalid) — registration cannot succeed by retrying",
          });
          throw err;
        }
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
    // Pass the data volume so the disk sample targets the worker's real data dir
    // (PRD #837 M1); /nix stays the fixed default inside the collector.
    const stats = new StatsCollector({ dataDir: this.config.dataDir });
    while (!signal.aborted) {
      let ok = false;
      try {
        // PRD #1391 M5: report per-run outbox depth alongside the resource sample. The
        // client sends the array only when the server negotiated `heartbeat_outbox`,
        // so an older api sees a byte-identical heartbeat. Assembled BEFORE the send so
        // the first recovered heartbeat carries the depth ahead of that tick's drain.
        // PRD #1390 M2a: the active-run snapshot rides the same send (built here so its
        // epoch is drawn from the ONE monotonic counter the claim loop also draws from).
        await this.client.heartbeat(this.collectStats(stats), this.outboxEntries(), this.buildActiveSnapshot());
        ok = true;
      } catch (err) {
        this.log.warn("heartbeat failed", { error: errMessage(err) });
      }
      // PRD #1391 M2: the re-arm trigger — on EACH successful heartbeat, drain the
      // outbox (single-flight). FIRE-AND-FORGET, like the boot drain: `drainOutbox`
      // replays the ENTIRE per-run backlog with no time budget, so awaiting it here
      // could push the next heartbeat past the api's 45s stale cutoff (message POSTs
      // do not refresh liveness) → the sweeper marks the worker offline and re-queues
      // its still-running runs (duplicate execution) during the very recovery this
      // feature exists to handle. The heartbeat must keep ticking on its interval; the
      // single-flight `draining` guard already makes a tick that fires mid-drain a
      // no-op. The depth was already assembled and SENT above (before this tick's
      // drain starts), so it is reported regardless. Guarded so a drain error never
      // escapes the loop (mirrors the heartbeat try/catch above).
      if (ok) {
        void this.drainOutbox(signal).catch((err) => {
          this.log.warn("outbox drain failed", { error: errMessage(err) });
        });
      }
      await sleep(this.config.heartbeatIntervalMs, signal);
    }
  }

  /** PRD #1391 M5: assemble the per-run outbox depth for the heartbeat, mapping every
   *  run with pending outbox depth to the wire {@link OutboxHeartbeatEntry} shape
   *  (`pending_terminal` is always 0 in Run A). Undefined when there is no outbox or
   *  nothing pending, so the heartbeat wire stays byte-identical. */
  private outboxEntries(): OutboxHeartbeatEntry[] | undefined {
    if (!this.outbox) return undefined;
    const entries: OutboxHeartbeatEntry[] = [];
    for (const runId of this.outbox.runsWithPending()) {
      const d = this.outbox.depthFor(runId);
      if (!d) continue;
      entries.push({
        run_id: d.runId,
        pending_messages: d.pendingMessages,
        pending_terminal: d.pendingTerminal,
        stale_retired: d.staleRetired,
        since: d.since,
        ...(d.blockedReason ? { blocked_reason: d.blockedReason } : {}),
      });
    }
    return entries.length > 0 ? entries : undefined;
  }

  /**
   * PRD #1390 M2a: build the active-run snapshot the heartbeat and the run-lane claim
   * both carry, from the shared {@link ActiveRunRegistry}. Returns undefined — so no
   * snapshot is sent and no epoch is spent — unless the registry exists AND the server
   * negotiated `active_run_snapshot`. Both loops call this, so their `snapshot_epoch`
   * values come from the ONE monotonic counter the registry owns. The client stamps the
   * register nonce on send.
   */
  private buildActiveSnapshot(): ActiveSnapshot | undefined {
    if (!this.activeRuns) return undefined;
    if (!this.client.hasFeature("active_run_snapshot")) return undefined;
    return this.activeRuns.build();
  }

  /**
   * PRD #1391 M2: the per-worker, single-flight outbox drainer. For each run with
   * pending segments, replay them in seq order over the messages route (poison inside
   * a segment is tombstoned and the rest lands — {@link replaySegment}); on a fully
   * retired run, re-arm its live batcher (if any) so it returns to the network. One
   * run at a time, awaiting each and yielding between them, so two drains never run
   * concurrently. A non-2xx that {@link replaySegment} re-throws (transient/fatal)
   * stops that ONE run and leaves the rest pending for the next heartbeat.
   */
  private async drainOutbox(signal?: AbortSignal): Promise<void> {
    const outbox = this.outbox;
    if (!outbox) return;
    if (this.draining) return; // single-flight
    this.draining = true;
    try {
      for (const runId of outbox.runsWithPending()) {
        if (signal?.aborted) break;
        try {
          const res = await outbox.drainRun(runId, (msgs, gen) =>
            replaySegment(this.client, runId, msgs, gen, this.log),
          );
          if (res.retired) this.rearm?.get(runId)?.();
        } catch (err) {
          this.log.warn("outbox drain failed for a run; will retry next heartbeat", {
            run_id: runId,
            error: errMessage(err),
          });
        }
        // Yield between runs so a long backlog never starves the event loop.
        await Promise.resolve();
      }
    } finally {
      this.draining = false;
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
   * level. Mirrors chatClaimLoop's tracked-promise pool. The RUN lane takes no
   * per-execution signal here, but shutdown is ABORT-THEN-DRAIN, not drain-only
   * (PRD #218): `Runner.shutdown()` (from main.ts's SIGTERM handler) aborts each
   * in-flight run's own cancel controller so it unwinds and fetches its committed work
   * back into the worker bare, and the `Promise.allSettled` drain below then awaits
   * those unwinding executes; see below.
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
        // (Decision 1 — "poll: at capacity"). Suppressed at the
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
        // PRD #1390 M2a: carry the active-run snapshot on the claim (built from the SAME
        // monotonic epoch counter the heartbeat draws from) so the api's pre-claim dedupe
        // sees this worker's live runs even before the first post-outage heartbeat lands.
        const claim = await this.client.claimRun(this.buildActiveSnapshot());
        if (claim) {
          claimed = true;
          // PRD #400 M4b: a DIFF-REVIEW claim is a `task`-kind claim carrying a non-null
          // review_target_run_id — routed to the slim ReviewRunner (clone + diff + reviewer
          // model, report-only) FIRST, before the kind switch, since it is a task by kind
          // but must NOT run the normal task executor. PRD #46: a judge claim rides the same
          // run lane (it counts toward worker capacity, Decision 8) but is executed by the
          // slim JudgeRunner — no clone/worktree/git, just fetch the trace, call the model,
          // post the review.
          const exec = claim.review_target_run_id
            ? this.reviewRunner.execute(claim)
            : claim.kind === "judge"
              ? this.judgeRunner.execute(claim)
              : this.runner.execute(claim);
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
    // On shutdown, drain every in-flight run (mirrors chatClaimLoop's drain). Post-#218
    // shutdown is abort-then-drain: `Runner.shutdown()` (main.ts SIGTERM handler) has
    // already aborted each run's cancel controller, so each run unwinds — reaping its
    // agent tree, then fetching its committed work back into the worker bare — and is
    // left NON-terminal for the sweeper to requeue onto the recovered tree. This drain
    // awaits those unwinding fetch-backs, and process exit is gated on `worker.run`, so
    // they complete inside the container's termination grace. The SIGKILL grace is the
    // hard backstop: a stuck run can't wedge shutdown past the grace window.
    //
    // The Set is passed directly rather than spread (PRD #103 M3, oxlint
    // unicorn/no-useless-spread). Equivalent, not merely tidier: Promise.allSettled
    // drains its iterable SYNCHRONOUSLY before any settle callback runs, so the
    // `active.delete(run)` in the `.finally` above cannot mutate it mid-drain and
    // the array copy was buying nothing. The `[...active, sleep(...)]` spreads in
    // the Promise.race calls above STAY — those build a longer array and the spread
    // is load-bearing there.
    await Promise.allSettled(active);
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
    // Set passed directly, same reasoning as the run lane's drain above.
    await Promise.allSettled(active);
  }
}
