import path from "node:path";
import { loadConfig } from "./config.js";
import { createLogger, type Logger } from "./log.js";
import { WorkerClient } from "./client.js";
import { GitCache } from "./git.js";
import { StubExecutor } from "./executor.js";
import { SdkExecutor } from "./sdk-executor.js";
import { ChatExecutor } from "./chat-executor.js";
import { RunRunner, type ExecutorFactory } from "./runner.js";
import { ChatRunner } from "./chat-runner.js";
import { Worker } from "./worker.js";
import { errMessage } from "./util.js";

// Set once the logger exists so the last-resort fatal handler can scrub through
// the SecretRegistry instead of writing a raw (unredacted) line.
let fatalLog: Logger | undefined;

async function main(): Promise<void> {
  const config = loadConfig();
  const log = createLogger(config.logLevel);
  // Scrub the join token from all output before it can appear anywhere.
  log.addSecret(config.workerToken);
  fatalLog = log;

  log.info("uzi-agent starting", {
    version: config.version,
    api_url: config.apiUrl,
    data_dir: config.dataDir,
    worker_name: config.workerName,
    executor: config.executor,
  });

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
  // The runId keeps it stable across resume (a requeue keeps the run_id) and the
  // runner removes it on terminal.
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
    });
    return { executor, homeDir: runHome };
  };
  const runner = new RunRunner(client, git, makeExecutor, log, config.messageBatchMs, config.workerToken, {
    pollMs: config.pollIntervalMs,
    planApprovalTimeoutMs: config.planApprovalTimeoutMs,
  });

  // The chat lane (PRD #39). Its executor gets the SAME join-token secret path as the
  // run executor (task #9) so the Bash + path-guard hooks deny a read of it, and the
  // pinned HOME so a chat SDK session survives a restart for Continue/resume. The
  // ChatRunner resolves each run's clocks from the claim config over these worker
  // defaults; the chat lane always runs the real ChatExecutor (no stub — chat has no
  // E2E stub path yet, and an unqueued chat lane just polls 204 harmlessly).
  //
  // HOME divergence from runs (PRD #42 Decision 5): a RUN gets a per-run HOME
  // (agent-home/<runId>, above), but chat deliberately keeps the SHARED agent-home.
  // A chat "Continue" creates a NEW run (new run_id) that resumes the SAME SDK
  // session by session_id, and that transcript resolves within the HOME it was
  // written under — a per-run HOME would file it under the new run_id and break
  // resume. Chat is read-only (no clone, no PAT, no Bash), so the process-global
  // $HOME/.claude races that per-run HOME closes for runs don't apply the same way.
  // Per-session executor factory (PRD #42 Decision 4, chat lane): each chat session
  // gets its OWN ChatExecutor so its spawnedPids/reap are private (no sharing at
  // WORKER_CHAT_SESSIONS>1). All sessions share the SDK HOME (sdkHomeRoot) — a
  // Continue resumes the same session under a new run_id, so per-run HOME would
  // break chat resume.
  const makeChatExecutor = (): ChatExecutor =>
    new ChatExecutor(log, sdkHomeRoot, {
      secretPaths: config.workerTokenFile ? [config.workerTokenFile] : [],
    });
  const chatRunner = new ChatRunner(client, makeChatExecutor, log, config.messageBatchMs, {
    maxTurns: config.chatMaxTurns,
    turnTimeoutMs: config.chatTurnTimeoutMs,
    idleTimeoutMs: config.chatIdleTimeoutMs,
    pollMs: config.chatPollMs,
  }, config.workerToken);

  const worker = new Worker(config, client, runner, chatRunner, log);

  const controller = new AbortController();
  for (const sig of ["SIGINT", "SIGTERM"] as const) {
    process.on(sig, () => {
      log.info("shutting down", { signal: sig });
      controller.abort();
    });
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
