import path from "node:path";
import { loadConfig, MAX_CONCURRENT_RUNS_SOFT_CEILING } from "./config.js";
import { createLogger, type Logger } from "./log.js";
import { WorkerClient, RequestError } from "./client.js";
import { GitCache } from "./git.js";
import { StubExecutor } from "./executor.js";
import { SdkExecutor } from "./sdk-executor.js";
import { ChatExecutor, type ChatExecutorLike } from "./chat-executor.js";
import { StubChatExecutor } from "./chat-executor-stub.js";
import { RunRunner, type ExecutorFactory } from "./runner.js";
import { ChatRunner } from "./chat-runner.js";
import { JudgeRunner } from "./judge-runner.js";
import { stubJudgeQueryFn } from "./judge-runner-stub.js";
import { Worker } from "./worker.js";
import { reclaimStrandedRunHomes } from "./home-reclaim.js";
import { errMessage } from "./util.js";
import { uidSplitActive } from "./runner-uid.js";
import { resolveDockerWiring, dockerSidecarExpected } from "./docker-wiring.js";

// Set once the logger exists so the last-resort fatal handler can scrub through
// the SecretRegistry instead of writing a raw (unredacted) line.
let fatalLog: Logger | undefined;

async function main(): Promise<void> {
  // PRD #51 M4: under the uid split, run the worker with umask 002 so (a) the runner-owned
  // /data subtrees the worker mkdirs (runner clone parents, per-run SDK HOME, provision)
  // are group-`runner`-writable — the runner-uid children can then create their per-run
  // dirs — and (b) the runner children inherit umask 002 (it crosses fork/exec/setpriv),
  // so their files are group-writable and the worker (a `runner`-group member) can tear
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

  log.info("uzi-agent starting", {
    version: config.version,
    api_url: config.apiUrl,
    data_dir: config.dataDir,
    worker_name: config.workerName,
    executor: config.executor,
    max_concurrent_runs: config.maxConcurrentRuns,
    docker_wired: config.dockerWiring.dockerHost !== undefined,
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
  const makeExecutor: ExecutorFactory = (runId) => {
    // The stub has no SDK $HOME (no session transcript to isolate); its only homeDir
    // use is the provisioning subprocess HOME, which stays SHARED (warm-start) like
    // the SDK executor's. So the stub keeps the shared root and there is nothing
    // per-run to clean (homeDir omitted from the RunExecution).
    if (config.executor === "stub") {
      return { executor: new StubExecutor(log, { planGate: config.stubPlanGate, homeDir: sdkHomeRoot }) };
    }
    const runHome = path.join(sdkHomeRoot, runId);
    const executor = new SdkExecutor(log, runHome, {
      // Deny a Bash `cat` of the join-token file (a read-only secret mount
      // persists it); the built-in /run/secrets/ prefix already covers the
      // shipping default, this adds a non-default UZI_WORKER_TOKEN_FILE path.
      secretPaths: config.workerTokenFile ? [config.workerTokenFile] : [],
      // The nix/devbox provisioning HOME + root stay SHARED worker-lifetime paths
      // (Decision 5): only the SDK $HOME (runHome) is per-run, so warm-start state
      // doesn't fragment per run. The per-run provision DIR still isolates the
      // synthesized devbox.json.
      provisionHomeDir: sdkHomeRoot,
      // Docker wiring (PRD #83 M1): the same startup-resolved wiring for every run —
      // gates the Bash guardrail's docker rule and supplies DOCKER_HOST to the SDK env.
      dockerWiring: config.dockerWiring,
      // PRD #90: the worker→API client, so the lead's save_memory MCP tool can POST a
      // cross-run learning (the server derives (user, repo) from the run claim).
      client,
    });
    return { executor, homeDir: runHome };
  };
  const runner = new RunRunner(client, git, makeExecutor, log, config.messageBatchMs, config.workerToken, {
    pollMs: config.pollIntervalMs,
    planApprovalTimeoutMs: config.planApprovalTimeoutMs,
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
    ...(config.executor === "stub" ? {} : { sdkHomeDir: sdkHomeRoot }),
  });

  // The judge lane (PRD #46): a slim runner for `judge` claims. It reuses the SDK
  // HOME root but needs no executor/clone — it fetches the trace, calls the model
  // once, and posts a verdict. Under UZI_E2E_EXECUTOR=stub the model call is the
  // stub judge queryFn (no live Anthropic, deterministic fallback), mirroring the
  // run/chat stub executors above so the e2e can drive the judge lane with a dummy
  // token and zero spend.
  const judgeRunner = new JudgeRunner(client, log, {
    homeRoot: sdkHomeRoot,
    ...(config.executor === "stub" ? { queryFn: stubJudgeQueryFn } : {}),
  });

  const worker = new Worker(config, client, runner, chatRunner, judgeRunner, log);

  // Signal handlers FIRST, before anything that can take real time. Until these
  // are installed a SIGTERM hits Node's default disposition and terminates the
  // process immediately, so a container stopped during startup dies rather than
  // shutting down — and the HOME reclaim below is exactly the kind of startup work
  // that can be in flight when a rollout sends SIGTERM.
  const controller = new AbortController();
  for (const sig of ["SIGINT", "SIGTERM"] as const) {
    process.on(sig, () => {
      log.info("shutting down", { signal: sig });
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

  await worker.run(controller.signal);
  log.info("uzi-agent stopped");
}

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
