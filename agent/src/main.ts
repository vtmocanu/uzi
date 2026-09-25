import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadConfig, MAX_CONCURRENT_RUNS_SOFT_CEILING, type ExecutorKind } from "./config.js";
import { createLogger, type Logger } from "./log.js";
import { WorkerClient, RequestError } from "./client.js";
import { GitCache } from "./git.js";
import { StubExecutor } from "./executor.js";
import { SdkExecutor } from "./sdk-executor.js";
import { selectCodexBinding, CodexSelectionError } from "./codex/select.js";
import { CodexExecutor, FailClosedExecutor, CODEX_PRODUCTION_PROVIDER, makeProductionCodexAdviceHarnessFactory } from "./codex/codex-executor.js";
import { ChatExecutor, type ChatExecutorLike } from "./chat-executor.js";
import { StubChatExecutor } from "./chat-executor-stub.js";
import { RunRunner, type ExecutorFactory, type RunExecution } from "./runner.js";
import { ChatRunner } from "./chat-runner.js";
import { Outbox, deriveTerminalReserveBytes } from "./outbox.js";
import { ActiveRunRegistry } from "./active-run-registry.js";
import { JudgeRunner } from "./judge-runner.js";
import { ReviewRunner } from "./review-runner.js";
import { stubJudgeQueryFn } from "./judge-runner-stub.js";
import { Worker } from "./worker.js";
import { reclaimStrandedRunHomes } from "./home-reclaim.js";
import { errMessage } from "./util.js";
import { uidSplitActive } from "./runner-uid.js";
import { resolveDockerWiring, dockerSidecarExpected, type DockerWiring } from "./docker-wiring.js";
import { probeCodexRuntime } from "./codex/codex-runtime-probe.js";
import { probeLandlockAvailability, resolveCodexHarnessAvailability } from "./codex/codex-capability.js";
import { reapCodexCommandOrphans, type ReapOrphansResult } from "./codex/launcher.js";
import type { ClaimCodexSecrets } from "./protocol.js";
import type { CommandSandboxMode } from "./config.js";

/**
 * Issue #1598: the worker-startup pass of the Codex command orphan reaper (command tmps
 * and per-run command caches left behind by a retained cleanup, an unconfirmed drain, or
 * a killed supervisor/holder). Gated only by the uid split, the one profile with a
 * command uid, a cache root and the supervisor. It never throws: a missing binary or a
 * failed pass is logged and startup continues (see {@link startupReapVerdict} for the
 * two results that stop it).
 *
 * `signal` is the worker's shutdown signal: an abort kills the reaper (as on its own
 * deadline) and returns promptly.
 *
 * INVARIANT: call this BEFORE the worker launches ANY run (before `worker.run()`). The
 * reaper's no-command-user proof assumes a fresh container PID namespace in which no new
 * command-uid process can start during the pass.
 */
export async function reapCodexOrphansAtStartup(
  log: Pick<Logger, "info" | "warn" | "error">,
  deps: {
    readonly splitActive?: boolean;
    readonly reap?: (signal?: AbortSignal) => Promise<ReapOrphansResult>;
    readonly signal?: AbortSignal;
  } = {},
): Promise<ReapOrphansResult | undefined> {
  if (!(deps.splitActive ?? uidSplitActive())) return undefined;
  let result: ReapOrphansResult;
  try {
    result = await (deps.reap ?? ((signal?: AbortSignal) => reapCodexCommandOrphans({ signal })))(deps.signal);
  } catch (err) {
    // reapCodexCommandOrphans never throws; an injected one might.
    result = { ok: false, reason: "protocol", detail: errMessage(err) };
  }
  if (result.ok) {
    const fields = {
      scanned: result.scanned,
      live: result.live,
      removed: result.removed,
      retained: result.retained,
      foreign: result.foreign,
      proof: result.proof,
      truncated: result.truncated,
      dirents_examined: result.direntsExamined,
    };
    // A truncated pass (the pass budget stopped it early), a retained or live candidate, or
    // an unproven pass leaves disk behind for the next startup. None of them blocks startup.
    if (result.truncated) log.warn("codex command orphan reap was truncated; orphans may remain until a later startup", fields);
    else if (result.retained > 0 || result.live > 0 || result.proof !== "held") log.warn("codex command orphan reap left candidates", fields);
    else log.info("codex command orphan reap", fields);
  } else if (result.reason === "timeout_unkilled") {
    log.error("codex command orphan reaper could not be stopped; no run may start while it may still run", {
      reason: result.reason,
      ...(result.detail ? { detail: result.detail } : {}),
    });
  } else if (result.reason === "aborted") {
    log.info("codex command orphan reap aborted by shutdown");
  } else {
    log.warn("codex command orphan reap failed", { reason: result.reason, ...(result.detail ? { detail: result.detail } : {}) });
  }
  return result;
}

/**
 * Issue #1598: what main() does after the startup reap.
 *   - "exit": the reaper timed out or was aborted and did not close after its kill, so it
 *     may still be scanning. Starting a run would break the reaper's proof (doc.go: the
 *     reaper must never overlap a run), so the worker exits non-zero instead: the
 *     container restart ends every process in its PID namespace, the reaper included,
 *     and the next start takes a fresh pass.
 *   - "shutdown": the worker was signalled during the pass; it is stopping anyway, so it
 *     does not start the claim loops.
 *   - "run": start the worker (a failed but finished pass only leaves disk behind).
 */
export function startupReapVerdict(result: ReapOrphansResult | undefined, aborted: boolean): "run" | "shutdown" | "exit" {
  if (result !== undefined && !result.ok && result.reason === "timeout_unkilled") return "exit";
  if (aborted) return "shutdown";
  return "run";
}

// Set once the logger exists so the last-resort fatal handler can scrub through
// the SecretRegistry instead of writing a raw (unredacted) line.
let fatalLog: Logger | undefined;

/** Dependencies {@link buildRunExecutor} needs, factored out of `main()`'s closure so
 *  the seam is callable in isolation (PRD #1429 M6 test). Production always supplies
 *  the real `log`/`client` and the live-resolved `sdkHomeRoot`/`dockerWiring`; a test
 *  supplies lightweight stand-ins (a `nullLogger()`, a throwaway `WorkerClient`, a
 *  temp dir, a fabricated `DockerWiring`). */
export interface BuildRunExecutorDeps {
  log: Logger;
  client: WorkerClient;
  sdkHomeRoot: string;
  executorKind: ExecutorKind;
  stubPlanGate: boolean;
  workerTokenFile?: string;
  dockerWiring: DockerWiring;
  /** PRD #1493 M3: the worker-configured command-sandbox mode, threaded into the
   *  CodexExecutor so its command/fileop argv carries the `--mode` token. */
  codexCommandSandbox: CommandSandboxMode;
  /** PRD #1493 M3: true when the sandbox is running degraded (best-effort on a
   *  Landlock-less kernel); the CodexExecutor writes one feed line per run. */
  codexSandboxDegraded: boolean;
}

/**
 * Build a per-execution executor for a run id. Factored out of `main()`'s
 * `makeExecutor` closure (PRD #1429 M6) ONLY so a test can call it directly without
 * importing `main.ts`'s side-effecting `main()` entrypoint (guarded below); the
 * construction logic and its ordering are otherwise untouched.
 *
 * PRD #1171 M3 — the claim-aware DARK selection seam. `selectCodexBinding` is a pure,
 * fail-closed discriminator: ABSENCE of the block takes the LITERAL current Claude/stub
 * path below (Claude byte-for-byte); a COMPLETE server-owned block selects Codex; a
 * PRESENT-but-BROKEN block throws a bounded, secret-free CodexSelectionError. We wrap
 * the select so that throw becomes a FailClosedExecutor whose run() re-throws the
 * message (routing through the runner's failed-run catch), NEVER a Claude fallback and
 * NEVER a crash of executeClaim itself.
 */
export function buildRunExecutor(runId: string, codex: ClaimCodexSecrets | undefined, deps: BuildRunExecutorDeps): RunExecution {
  const { log, client, sdkHomeRoot, executorKind, stubPlanGate, workerTokenFile, dockerWiring, codexCommandSandbox, codexSandboxDegraded } = deps;
  let selection;
  try {
    selection = selectCodexBinding({ codex });
  } catch (err) {
    if (err instanceof CodexSelectionError) {
      return { executor: new FailClosedExecutor(err.message) };
    }
    throw err;
  }

  // PRD #1429 M6: the harness-neutral e2e stub seam. This MUST run before the
  // `selection.kind === "codex"` branch below, or a stub-configured worker would
  // still build the production CodexExecutor for a Codex-bound claim and call/
  // release the real credential — defeating the point of the stub. The stub
  // simulates plan/implement and calls no provider; it never reads
  // `selection.binding`, never claims/releases the Codex credential and
  // constructs no Codex class, so it is safe to return for EITHER
  // `selection.kind`. The stub has no SDK $HOME (no session transcript to
  // isolate); its only homeDir use is the provisioning subprocess HOME, which
  // stays SHARED (warm-start) like the SDK executor's. So the stub keeps the
  // shared root and there is nothing per-run to clean (homeDir omitted from the
  // RunExecution).
  //
  // Production is unchanged: `parseExecutor` (config.ts) only ever returns
  // "stub" or "sdk" ("sdk" is the default for undefined/omitted), and only
  // e2e/tests set UZI_EXECUTOR=stub — so `config.executor` is never "stub" in
  // production and this branch is inert there. A real (non-stub) Codex claim
  // still falls through to the `selection.kind === "codex"` branch below
  // unchanged.
  if (executorKind === "stub") {
    return { executor: new StubExecutor(log, { planGate: stubPlanGate, homeDir: sdkHomeRoot }) };
  }

  if (selection.kind === "codex") {
    // A validated Codex binding: the production CodexExecutor over the M3a launcher, the
    // dark core and the M1 credential bridge. Per-run owned HOME (like SdkExecutor).
    const runHome = path.join(sdkHomeRoot, runId);
    const executor = new CodexExecutor(
      log,
      runHome,
      {
        binding: selection.binding,
        client,
        provider: CODEX_PRODUCTION_PROVIDER,
        // The nix/devbox provisioning HOME + root stay SHARED worker-lifetime paths
        // (Decision 5): only the per-run Codex $HOME (runHome) is per-run, so warm-start
        // state doesn't fragment per run — and the provisioning subprocess never
        // materializes the per-run HOME with the wrong gid. provisionRoot takes its
        // computed default (path.dirname(provisionHomeDir)/provision).
        provisionHomeDir: sdkHomeRoot,
        // PRD #1493 M3: the worker-configured command-sandbox mode (never run/repo/model
        // input). Threaded into commandSandboxArgv's `--mode` token; degraded drives the
        // per-run feed line.
        commandSandbox: codexCommandSandbox,
        commandSandboxDegraded: codexSandboxDegraded,
      },
      {
        // PRD #1171 m4 (F1): the RUNNER owns the terminal registry teardown. Its post-run
        // durability sinks (park/shutdown/finalize) reap the provider root through
        // withBoundary AFTER run() returns, and executeClaim's finally calls safety.dispose
        // once the last sink settles — so run()'s finally must leave the registry ALIVE.
        deferRegistryTeardown: true,
      },
    );
    return { executor, homeDir: runHome };
  }

  // selection.kind === "claude": the EXACT legacy path, unchanged.
  const runHome = path.join(sdkHomeRoot, runId);
  const executor = new SdkExecutor(log, runHome, {
    // Deny a Bash `cat` of the join-token file (a read-only secret mount
    // persists it); the built-in /run/secrets/ prefix already covers the
    // shipping default, this adds a non-default UZI_WORKER_TOKEN_FILE path.
    secretPaths: workerTokenFile ? [workerTokenFile] : [],
    // The nix/devbox provisioning HOME + root stay SHARED worker-lifetime paths
    // (Decision 5): only the SDK $HOME (runHome) is per-run, so warm-start state
    // doesn't fragment per run. The per-run provision DIR still isolates the
    // synthesized devbox.json.
    provisionHomeDir: sdkHomeRoot,
    // Docker wiring (PRD #83 M1): the same startup-resolved wiring for every run —
    // gates the Bash guardrail's docker rule and supplies DOCKER_HOST to the SDK env.
    dockerWiring,
    // PRD #90: the worker→API client, so the lead's save_memory MCP tool can POST a
    // cross-run learning (the server derives (user, repo) from the run claim).
    client,
  });
  return { executor, homeDir: runHome };
}

async function main(): Promise<void> {
  // PRD #51 M4: under the uid split, run the worker with umask 002 so (a) the runner-owned
  // /data subtrees the worker mkdirs (runner clone parents, per-run SDK HOME, provision)
  // are group-`runner`-writable — runner and runner-cmd children can then create/access their per-run
  // dirs — and (b) the runner children inherit umask 002 (it crosses fork/exec/setpriv),
  // so their files are group-writable and the worker/runner-cmd (`runner`-group members) can tear
  // them down on terminal. This never widens a WORKER-owned path to the runner: those are
  // group `worker` (which the runner is not in), so 002 only adds group-`worker` write
  // there (inert). Single-uid (#58): unchanged (default umask, no separate runner).
  if (uidSplitActive()) process.umask(0o002);
  const config = loadConfig();
  const log = createLogger(config.logLevel);
  // Scrub the join token from all output before it can appear anywhere.
  log.addSecret(config.workerToken);
  fatalLog = log;

  // Docker wiring keystone (PRD #83 M1): resolve ONCE at startup with a bounded liveness
  // probe (loadConfig can't — it's sync). The single result feeds the register capability
  // report, the guardrail's allow-when-wired decision, and the SDK's DOCKER_HOST. M2
  // (compose socket) / M3 (k8s DOCKER_HOST) supply a real sidecar to it. M2 follow-up: when
  // a sidecar is EXPECTED (DOCKER_HOST/UZI_DIND_SOCKET set) the probe is RETRIED up to the
  // readiness budget so a daemon container slightly slower than the worker does not cost the
  // capability; a non-docker worker (neither set) does one fast probe and never blocks.
  config.dockerWiring = await resolveDockerWiring(process.env, {
    readyIntervalMs: config.dockerReadyIntervalMs,
    readyTimeoutMs: config.dockerReadyTimeoutMs,
  });
  // Degrade-to-unwired is silent for a normal non-docker worker, but LOUD when a sidecar was
  // expected and never came up — that is a misconfig/timing bug an operator must see.
  if (dockerSidecarExpected(process.env) && config.dockerWiring.dockerHost === undefined) {
    log.warn(
      "docker sidecar EXPECTED (DOCKER_HOST/UZI_DIND_SOCKET set) but no daemon became reachable within the readiness timeout; continuing WITHOUT docker (capability unreported, guardrail denies docker, DOCKER_HOST not injected)",
      { docker_ready_timeout_ms: config.dockerReadyTimeoutMs },
    );
  }

  // Codex runtime-probe keystone (PRD #1332 D3 / M5A C2): resolve ONCE at startup,
  // mirroring the docker-wiring resolve-once above. It reads the root-owned Codex receipt
  // and recomputes member digests WITHOUT executing Codex, searching PATH, starting
  // app-server or using the network; the single boolean result feeds the register
  // protocol-capability report (worker.ts appends codex_harness_v1 only when capable). A
  // stripped/hand-built/corrupt/mismatched/old image resolves to not-capable and keeps
  // serving Claude. The probe never throws, but a defensive catch degrades to not-capable
  // so a probe fault can never break startup.
  config.codexProbe = await probeCodexRuntime().catch((err) => {
    log.warn("codex runtime probe threw unexpectedly; advertising no codex capability", {
      error: errMessage(err),
    });
    return { capable: false as const };
  });
  // PRD #1493 M3: honest advertisement. The receipt probe alone no longer decides
  // codex_harness_v1 — combine it with the uid-split state AND a Landlock availability
  // probe (with the configured sandbox mode). A split-less worker, and a split worker on
  // a Landlock-less kernel in `required` mode, stop advertising a Codex run they were
  // going to fail; those runs queue with the api's existing reason instead. The Landlock
  // probe invokes `uzi-codex-command-sandbox --probe` (never Codex, never the network), so
  // probeCodexRuntime keeps its no-exec contract. Each failing precondition logs a
  // distinct reason.
  const uidSplit = uidSplitActive();
  const landlock = probeLandlockAvailability();
  config.codexHarness = resolveCodexHarnessAvailability({
    receiptCapable: config.codexProbe.capable,
    receiptReason: config.codexProbe.reason,
    uidSplit,
    mode: config.codexCommandSandbox,
    landlock,
  });
  if (config.codexHarness.advertise && config.codexHarness.degraded) {
    // Log the DEGRADED mode ONCE here at startup (no per-command model-visible warning;
    // the executor writes one line into each Codex run's feed).
    log.warn("codex_harness_v1 advertised DEGRADED: best-effort mode on a kernel without Landlock — commands run WITHOUT filesystem confinement (uid split still enforced)", {
      sandbox_mode: config.codexCommandSandbox,
      landlock,
    });
  } else if (config.codexHarness.advertise) {
    log.info("codex runtime probe OK — advertising codex_harness_v1", {
      sandbox_mode: config.codexCommandSandbox,
      landlock,
    });
  } else {
    log.info("codex_harness_v1 NOT advertised (serving Claude)", {
      reason: config.codexHarness.reason ?? "not capable",
      landlock,
      uid_split: uidSplit,
      sandbox_mode: config.codexCommandSandbox,
    });
  }

  log.info("uzi-agent starting", {
    version: config.version,
    api_url: config.apiUrl,
    data_dir: config.dataDir,
    worker_name: config.workerName,
    executor: config.executor,
    max_concurrent_runs: config.maxConcurrentRuns,
    docker_wired: config.dockerWiring.dockerHost !== undefined,
    codex_capable: config.codexProbe.capable,
    codex_advertise: config.codexHarness.advertise,
    codex_sandbox_mode: config.codexCommandSandbox,
    codex_sandbox_degraded: config.codexHarness.degraded,
  });
  // Soft-ceiling warn (PRD #42 Decision 3): the cap is honored as configured, but a
  // value above the documented ceiling is almost certainly a fat-finger — each slot
  // is ~one SDK CLI + git ops + optional devbox provisioning, and they share one
  // container cgroup, so shout before the OOM killer does. Warn here (not in
  // loadConfig) because the logger is only built after the config parse.
  if (config.maxConcurrentRuns > MAX_CONCURRENT_RUNS_SOFT_CEILING) {
    log.warn("WORKER_MAX_CONCURRENT_RUNS above the soft ceiling; size the container for that many concurrent runs", {
      max_concurrent_runs: config.maxConcurrentRuns,
      soft_ceiling: MAX_CONCURRENT_RUNS_SOFT_CEILING,
    });
  }

  const client = new WorkerClient(config.apiUrl, config.workerToken, config.version, log, {
    httpTimeoutMs: config.httpTimeoutMs,
  });
  const git = new GitCache(config.dataDir, log);

  // PRD #1391 M2: the worker-owned message outbox and the shared re-arm registry,
  // built + initialised BEFORE the runners/worker so a boot backlog is already
  // loadable and every batcher can spill into the same store. `init()` mints/loads the
  // worker-local HMAC key, scans the run tree and captures the spill-unclean flags;
  // it fails closed (disables the store) rather than throwing, so a broken /data never
  // blocks startup. The registry maps a live run's id to its batcher's rearm() so the
  // drainer can return a still-running run to the network once its segments retire.
  const outbox = new Outbox({
    root: config.outboxDataDir,
    log,
    runMaxBytes: config.outboxRunMaxBytes,
    maxBytes: config.outboxMaxBytes,
    retentionMs: config.outboxRetentionMs,
    // PRD #1391 M3 (Run B, D2): size the physical `.reserve` for the terminal journals, derived from
    // ONE source — the per-record cap times how many hard-max journals must survive a full volume,
    // plus a per-record overhead. init() grows a deployed Run A worker's 64 KiB reserve up to this.
    reserveBytes: deriveTerminalReserveBytes(config.outboxReserveTerminals, config.outboxTerminalMaxBytes),
  });
  await outbox.init();
  const rearm = new Map<string, () => void>();
  // PRD #1390 M2a: the shared active-run registry the run lane + judge/review runners write
  // as they start/transition/finish, and the worker reads to build the ActiveSnapshot that
  // rides every heartbeat and run-lane claim. ONE instance so the snapshot epoch is a single
  // process-monotonic counter across the heartbeat and claim loops.
  // PRD #1391 Run B M4: thread the outbox pending-terminal lister + the register-returned server cap
  // into the registry (function seams, so the registry stays decoupled from Outbox/WorkerClient).
  // The cap getter reads the client field the register response populated, so it is correct on every
  // build after register; before register it reads undefined (cap 0), which only matters if a build
  // ran that early (it does not — snapshots build after register).
  const activeRuns = new ActiveRunRegistry(
    () => outbox.listPendingTerminals(),
    () => client.workerOutboxMaxPending,
  );
  // Pin the SDK's HOME (session transcripts under $HOME/.claude/projects) onto
  // the persistent data volume so `docker compose down && up` doesn't wipe
  // sessions and resume still works.
  const sdkHomeRoot = path.join(config.dataDir, "agent-home");
  // Per-execution executor factory (PRD #42 Decisions 4/5): each RUN builds its OWN
  // executor — so SdkExecutor.spawnedPids and killAgentTree are private to the run,
  // never shared across two concurrent runs (the B1 pre-push reap) — and gets its
  // OWN HOME `agent-home/<runId>` so the SDK's process-global $HOME/.claude state
  // (history/todos/shell-snapshots/~/.claude.json) can't race or leak between runs.
  // The runner removes it on terminal.
  //
  // This comment used to end "the runId keeps it stable across resume (a requeue keeps
  // the run_id)". The run id is stable, but the PATH is only stable ON THIS WORKER: it
  // lives on the claiming worker's own data volume, and a requeued run whose affinity
  // grace lapsed can be claimed by a different worker (or by this one on a fresh
  // volume), where `agent-home/<runId>` has never existed. The claim still carries the
  // session id, but the transcript it names does not — and the SDK resolves a resume
  // LOCALLY, so that used to kill the run on its first turn. The wording is arguably
  // what hid it: "stable across resume" reads as a guarantee it cannot make. The
  // runner now preflights the transcript and drops an unresolvable resume with an
  // honest run message (issue #105, sdk-session.ts).
  // The construction logic (dark Codex selection, the stub short-circuit, real
  // Codex/SdkExecutor wiring) lives in the top-level `buildRunExecutor` above,
  // factored out so a test can call it directly. This closure only binds it to
  // this process's live config/log/client/sdkHomeRoot.
  const makeExecutor: ExecutorFactory = (runId, codex) =>
    buildRunExecutor(runId, codex, {
      log,
      client,
      sdkHomeRoot,
      executorKind: config.executor,
      stubPlanGate: config.stubPlanGate,
      workerTokenFile: config.workerTokenFile,
      dockerWiring: config.dockerWiring,
      codexCommandSandbox: config.codexCommandSandbox,
      codexSandboxDegraded: config.codexHarness.degraded,
    });
  const runner = new RunRunner(client, git, makeExecutor, log, config.messageBatchMs, config.workerToken, {
    pollMs: config.pollIntervalMs,
    planApprovalTimeoutMs: config.planApprovalTimeoutMs,
    checkpointIntervalMs: config.checkpointIntervalMs,
    // issue #1597 M2: the mid-turn checkpoint tick cadence (CHECKPOINT_TICK_INTERVAL; 0 disables).
    checkpointTickIntervalMs: config.checkpointTickIntervalMs,
    // PRD #1391 M2: spill collaborators for every run's batcher.
    outbox,
    rearm,
    transientTripMs: config.transientTripMs,
    outboxSpillBufferBytes: config.outboxSpillBufferBytes,
    // PRD #1391 Run B M3: the terminal-journal send-path knobs (canonicaliser cap + gap-fill bound).
    outboxTerminalMaxBytes: config.outboxTerminalMaxBytes,
    gapFillMax: config.gapFillMax,
    // PRD #1390 M2a: the shared active-run registry the worker reads to build snapshots.
    activeRuns,
  });

  // The chat lane (PRD #39). Per-session executor factory (PRD #42 Decision 4): each
  // chat session builds its OWN executor so its spawnedPids/reap are private (no
  // sharing at WORKER_CHAT_SESSIONS>1) — the same isolation the run lane got above.
  // Under UZI_EXECUTOR=stub each session is a StubChatExecutor (no live Anthropic
  // session) so the M6 chat e2e runs on the isolated stack with dummy creds (task
  // #15); otherwise the real ChatExecutor, which gets the SAME join-token secret path
  // as the run executor (task #9) so the Bash + path-guard hooks deny a read of it.
  //
  // HOME divergence from runs (PRD #42 Decision 5): a RUN gets a per-run HOME
  // (agent-home/<runId>, above), but every chat session shares the SDK HOME
  // (sdkHomeRoot). A chat "Continue" creates a NEW run (new run_id) that resumes the
  // SAME SDK session by session_id, and that transcript resolves within the HOME it
  // was written under — a per-run HOME would file it under the new run_id and break
  // resume. Chat is read-only (no clone, no PAT, no Bash), so the process-global
  // $HOME/.claude races that per-run HOME closes for runs don't apply the same way.
  const makeChatExecutor = (): ChatExecutorLike =>
    config.executor === "stub"
      ? new StubChatExecutor(log)
      : new ChatExecutor(log, sdkHomeRoot, {
          secretPaths: config.workerTokenFile ? [config.workerTokenFile] : [],
        });
  const chatRunner = new ChatRunner(client, makeChatExecutor, log, config.messageBatchMs, {
    maxTurns: config.chatMaxTurns,
    turnTimeoutMs: config.chatTurnTimeoutMs,
    idleTimeoutMs: config.chatIdleTimeoutMs,
    pollMs: config.chatPollMs,
  }, config.workerToken, {
    // Issue #105: the HOME a Continue's transcript would have to live under, so the
    // runner can tell a resumable session from one written on another worker. The check
    // globs this HOME's project dirs (sdk-session.ts), so only the HOME is passed — the
    // chat executor's cwd is invariant (the baked source snapshot), so the shared chat
    // HOME holds exactly one project dir. Omitted for the stub (it persists no real SDK
    // session) — the same discriminator the run lane gets for free from the stub's
    // absent per-run HOME above.
    //
    // The conditional-spread idiom is the point (PRD #103 M3): it OMITS the key
    // rather than setting it to undefined, which is what lets the stub be told apart
    // from a worker whose sdkHomeDir happens to be unset. (No oxlint
    // unicorn/no-useless-spread disable is needed here: the PRD #1391 M2 fields below
    // give the object sibling keys, so the rule no longer reads the spread as useless.)
    ...(config.executor === "stub" ? {} : { sdkHomeDir: sdkHomeRoot }),
    // PRD #1391 M2: chat keeps the message outbox (Run A), spilling into the same store
    // and re-arm registry as the run lane.
    outbox,
    rearm,
    transientTripMs: config.transientTripMs,
    outboxSpillBufferBytes: config.outboxSpillBufferBytes,
  });

  // PRD #1429 M3: the Codex advice-harness factory judge/review thread into
  // runReadOnlyModelPass (model-pass.ts) so a Codex judge/review claim (secrets.codex
  // present) gets a REAL Codex advice pass instead of the missing-token fallback. Built
  // HERE — one of the two sites semgrep/codex-fixed-constructor.yml allows to construct a
  // Codex class (the other is codex/codex-executor.ts itself) — and injected as an option.
  //
  // UNLIKE the run lane's `buildRunExecutor` (PRD #1429 M6, above this function): this
  // factory is always built, and a codex-bound judge/review claim always constructs the
  // REAL CodexAdviceHarness, regardless of `config.executor`. The judge/review e2e path
  // proves itself through the packaged-only LOOPBACK app-server provider
  // (`CODEX_M3B_LOOPBACK_PROVIDER_NAME`, wired via `appServerAuthOpenAIBaseUrlForTest`),
  // not through a generic stub — so there is no judge-lane equivalent of the run lane's
  // stub-before-codex short-circuit. `stubJudgeQueryFn` below only ever gates the CLAUDE
  // judge's `queryFn`; the Codex judge/review path (`runCodexAdviceModelPass`) takes no
  // `queryFn` at all and is unaffected by it.
  //
  // (This comment used to say makeExecutor "unconditionally" selected the real
  // CodexExecutor the same way, with UZI_EXECUTOR=stub "only ever" affecting the Claude
  // path. That was the M6 bug this PRD fixes, not an intended parallel: the run lane's
  // stub now DOES short-circuit a codex-bound claim to StubExecutor before it ever
  // reaches the codex branch. Only this judge/review advice lane keeps the old
  // always-real-CodexAdviceHarness shape.)
  const codexAdviceHarnessFactory = makeProductionCodexAdviceHarnessFactory(client, log, sdkHomeRoot);

  // The judge lane (PRD #46): a slim runner for `judge` claims. It reuses the SDK
  // HOME root but needs no executor/clone — it fetches the trace, calls the model
  // once, and posts a verdict. Under UZI_E2E_EXECUTOR=stub the model call is the
  // stub judge queryFn (no live Anthropic, deterministic fallback), mirroring the
  // run/chat stub executors above so the e2e can drive the judge lane with a dummy
  // token and zero spend.
  const judgeRunner = new JudgeRunner(client, log, {
    homeRoot: sdkHomeRoot,
    // PRD #1390 M2a: a judge attempt holds a run slot, so it is listed in the snapshot.
    activeRuns,
    // PRD #1391 Run B M3b (D6): journal the judge's terminal STATE write-ahead (never the verdict).
    outbox,
    outboxTerminalMaxBytes: config.outboxTerminalMaxBytes,
    gapFillMax: config.gapFillMax,
    codexAdviceHarnessFactory,
    ...(config.executor === "stub" ? { queryFn: stubJudgeQueryFn } : {}),
  });

  // The review lane (PRD #400 M4b): a slim runner for a `task` claim carrying a
  // review_target_run_id. Like the judge it reuses the SDK HOME root, but it also needs
  // `git` (RunRunner-style) to clone the reviewed branch and compute the diff before the
  // text-only model call. Under UZI_EXECUTOR=stub the model call is the stub judge queryFn
  // (no live Anthropic — it yields an error result, so the runner posts a graceful `failed`
  // review), mirroring the judge lane so the e2e can drive it with a dummy token.
  const reviewRunner = new ReviewRunner(client, git, log, {
    homeRoot: sdkHomeRoot,
    // PRD #1390 M2a: a review attempt holds a run slot, so it is listed in the snapshot.
    activeRuns,
    // PRD #1391 Run B M3b (D6): journal the review's terminal STATE write-ahead (never the review POST).
    outbox,
    outboxTerminalMaxBytes: config.outboxTerminalMaxBytes,
    gapFillMax: config.gapFillMax,
    codexAdviceHarnessFactory,
    ...(config.executor === "stub" ? { queryFn: stubJudgeQueryFn } : {}),
  });

  // PRD #1391 M2: the Worker owns the per-worker outbox drainer + the heartbeat outbox
  // report, so it takes the same outbox + re-arm registry the runners spill into. The
  // `undefined` preserves the default boot toolchain preflight (only tests inject one).
  const worker = new Worker(config, client, runner, chatRunner, judgeRunner, reviewRunner, log, undefined, outbox, rearm, activeRuns);

  // Signal handlers FIRST, before anything that can take real time. Until these
  // are installed a SIGTERM hits Node's default disposition and terminates the
  // process immediately, so a container stopped during startup dies rather than
  // shutting down — and the HOME reclaim below is exactly the kind of startup work
  // that can be in flight when a rollout sends SIGTERM.
  const controller = new AbortController();
  for (const sig of ["SIGINT", "SIGTERM"] as const) {
    process.on(sig, () => {
      log.info("shutting down", { signal: sig });
      // PRD #218 M1: trigger the run lane's graceful shutdown FIRST — it marks every
      // in-flight run and aborts its controller so each fetches its committed work back
      // into the worker bare as it unwinds (the sweeper then requeues a run whose tree
      // is safe). shutdown() does no git and is synchronous; the fetch-backs run inside
      // the unwinding execute()s, which the claim-loop drain — gated by `worker.run`
      // below — waits for within the container's termination grace. controller.abort()
      // then unblocks the loops as today.
      runner.shutdown();
      controller.abort();
    });
  }

  // PRD #108 M6: one-off reclaim of HOMEs stranded by the pre-fix cleanup, which
  // could not remove the Go module cache's 0555 directories (167.3 MB measured for
  // one run). It never throws, and it deletes only run ids the API positively
  // reports terminal — every kind of not-knowing skips (home-reclaim.ts).
  //
  // It runs before worker.run() for defence in depth, NOT because it has to: a run
  // this worker later claims reads `claimed`/`running` (non-terminal, skipped) and
  // has a fresh mtime, so the ordering is not what makes the sweep safe. What the
  // ordering DOES cost is startup latency, since worker.run() is where the
  // toolchain preflight, registration, orphan recovery and both claim loops live.
  // The sweep therefore bails out after a few consecutive status-lookup failures
  // and holds a wall-clock deadline: when the api is unreachable nothing can be
  // reclaimed anyway, and a worker restarting while the api is unhealthy is a
  // CORRELATED failure, not an exotic one — they roll together.
  if (config.homeReclaimEnabled) {
    await reclaimStrandedRunHomes(
      sdkHomeRoot,
      async (runId) => {
        try {
          return (await client.getChatRun(runId)).status;
        } catch (err) {
          // A 404 is the API ANSWERING not-found — the run's row is gone, which is
          // exactly what the oldest stranded HOMEs look like. Return undefined so the
          // sweep SKIPS without counting it toward the outage bail; let every other
          // error (down / 5xx / timeout) propagate as a genuine could-not-ask that
          // DOES count (PRD #108 B2/B3, home-reclaim.ts RunStatusLookup contract).
          if (err instanceof RequestError && err.status === 404) return undefined;
          throw err;
        }
      },
      log,
    ).catch((err) => log.warn("run HOME reclaim failed", { error: errMessage(err) }));
  }

  // Issue #1598: reap Codex command orphans (tmps + per-run caches) from a previous
  // container. MUST run here, before worker.run(): nothing may launch a run (and so no
  // command-uid process may start) while the reaper's proof is being taken.
  const reap = await reapCodexOrphansAtStartup(log, { signal: controller.signal });
  const verdict = startupReapVerdict(reap, controller.signal.aborted);
  if (verdict === "exit") {
    // A reaper that may still run must never overlap a run: exit non-zero so the
    // container restarts (its PID namespace, and the reaper with it, goes away). The
    // error was logged above. An explicit exit, not exitCode: the reaper's still-open
    // child handle would otherwise keep the event loop alive.
    process.exit(1);
  }
  if (verdict === "shutdown") {
    log.info("uzi-agent stopped during the startup reap; the worker was not started");
    return;
  }

  await worker.run(controller.signal);
  log.info("uzi-agent stopped");
}

// PRD #1429 M6: only auto-run `main()` when this module is the process ENTRYPOINT
// (npm run start -> `tsx src/main.ts`, so process.argv[1] resolves to this exact
// file), never when it is merely IMPORTED — e.g. by an agent/test/*.test.ts that
// wants `buildRunExecutor` without paying for the full worker bootstrap
// (loadConfig/resolveDockerWiring/probeCodexRuntime/worker.run(), none of which a
// unit test can satisfy). Production is unaffected: `npm run start`/`tsx
// src/main.ts` always sets argv[1] to this file, so the guard is true there.
const isEntrypoint = (() => {
  try {
    return process.argv[1] !== undefined && fileURLToPath(import.meta.url) === path.resolve(process.argv[1]);
  } catch {
    return false;
  }
})();

if (isEntrypoint) {
  main().catch((err) => {
    // Last-resort handler: config errors and unexpected fatals land here.
    const message = errMessage(err);
    if (fatalLog) {
      // Route through the logger so any registered secret is scrubbed.
      fatalLog.error("fatal", { error: message });
    } else {
      // Logger not up yet — config load failed before any secret was registered
      // (loadConfig errors carry only env key names / duration values, never the
      // token), so a raw line is safe here.
      process.stderr.write(JSON.stringify({ level: "error", msg: "fatal", error: message }) + "\n");
    }
    process.exitCode = 1;
  });
}
