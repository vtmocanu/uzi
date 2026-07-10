import path from "node:path";
import { loadConfig } from "./config.js";
import { createLogger, type Logger } from "./log.js";
import { WorkerClient } from "./client.js";
import { GitCache } from "./git.js";
import { StubExecutor, type Executor } from "./executor.js";
import { SdkExecutor } from "./sdk-executor.js";
import { ChatExecutor } from "./chat-executor.js";
import { RunRunner } from "./runner.js";
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
  const sdkHomeDir = path.join(config.dataDir, "agent-home");
  const executor: Executor =
    config.executor === "stub"
      ? new StubExecutor(log, { planGate: config.stubPlanGate, homeDir: sdkHomeDir })
      : new SdkExecutor(log, sdkHomeDir, {
          // Deny a Bash `cat` of the join-token file (a read-only secret mount
          // persists it); the built-in /run/secrets/ prefix already covers the
          // shipping default, this adds a non-default UZI_WORKER_TOKEN_FILE path.
          secretPaths: config.workerTokenFile ? [config.workerTokenFile] : [],
        });
  const runner = new RunRunner(client, git, executor, log, config.messageBatchMs, config.workerToken, {
    pollMs: config.pollIntervalMs,
    planApprovalTimeoutMs: config.planApprovalTimeoutMs,
  });

  // The chat lane (PRD #39). Its executor gets the SAME join-token secret path as the
  // run executor (task #9) so the Bash + path-guard hooks deny a read of it, and the
  // SAME pinned HOME so a chat SDK session survives a restart for Continue/resume. The
  // ChatRunner resolves each run's clocks from the claim config over these worker
  // defaults; the chat lane always runs the real ChatExecutor (no stub — chat has no
  // E2E stub path yet, and an unqueued chat lane just polls 204 harmlessly).
  const chatExecutor = new ChatExecutor(log, sdkHomeDir, {
    secretPaths: config.workerTokenFile ? [config.workerTokenFile] : [],
  });
  const chatRunner = new ChatRunner(client, chatExecutor, log, config.messageBatchMs, {
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
