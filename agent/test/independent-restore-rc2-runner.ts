// issue #3644 m4c: the production RunRunner and RetainedSourceServer, pointed at the ON-DISK state of independent-restore-rc2-compat.proof.ts
// (its bare repository, data dir and origin), instead of a rig-built state. Nothing here knows the proof's fixture: every path
// is a parameter, and the runner is the same class with the same options the restore-route rig gives it (restore-route-rig.ts).
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { WorkerClient } from "../src/client.js";
import type { Executor } from "../src/executor.js";
import { GitLabClient } from "../src/forge.js";
import { Outbox } from "../src/outbox.js";
import { GitCache } from "../src/git.js";
import type { ClaimResponse } from "../src/protocol.js";
import { establishProcessBoundary } from "../src/restore-boundary.js";
import { RunRunner } from "../src/runner.js";
import { testLockOptions } from "./authority-test-support.js";
import { FakeApi } from "./fake-api.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { makeClaim, nullLogger, recordingLogger, testGitCacheOptions } from "./helpers.js";
import { FEATURES, host, makeProc } from "./restore-route-rig.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";

const TOKEN = "tkn-runner-123456";
const AUTH_TOKEN = `unit-join-${randomBytes(12).toString("hex")}`;

export interface RunnerOver {
  runner: RunRunner;
  api: FakeApi;
  git: GitCache;
  authority: AuthorityRuntime;
  claim: ClaimResponse;
  logs: ReturnType<typeof recordingLogger>;
  /** Every `makeExecutor` call and every model turn: both must stay zero on a stop. */
  counters: { factories: number; ran: number };
  close(): Promise<void>;
}

export interface RunnerOverOptions {
  dataDir: string;
  originPath: string;
  runId: string;
  iid: number;
  claimGeneration: number;
  /** The HOME the factory hands the run (the proof passes P's legacy HOME, so its bytes are checked). */
  homeDir: string;
  /** Queue an owner Cancel for the run before it is claimed. */
  cancel?: boolean;
}

/** A RunRunner (restore ON, a real AuthorityRuntime over `dataDir`) whose claim of `runId` is served by a fake api. */
export async function runnerOverDisk(o: RunnerOverOptions): Promise<RunnerOver> {
  const api = new FakeApi(TOKEN);
  const baseUrl = await api.listen();
  const client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
  client.protocolFeatures = FEATURES;
  const git = new GitCache(o.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
  const lifetime = await acquireWorkerLifetime(o.dataDir, testLockOptions());
  const sInc = randomUUID();
  const authority = new AuthorityRuntime({ lifetime, workerToken: AUTH_TOKEN, workerIncarnation: sInc, timeoutMs: 5000 });
  const tmp = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "restore-rc2-runner-")));
  const sProc = path.join(tmp, "proc-s");
  await makeProc(sProc, { ticks: "2000" });
  fs.mkdirSync(path.join(o.dataDir, ".uzi-authority"), { recursive: true, mode: 0o700 });
  const boundary = await establishProcessBoundary({ dataDir: o.dataDir, workerToken: AUTH_TOKEN, hostname: host, procRoot: sProc, incarnation: sInc });
  if (!boundary.ok) throw new Error("the successor's process boundary could not be established");

  const claim: ClaimResponse = makeClaim({
    run_id: o.runId, issue_iid: o.iid, issue_title: `Fix thing ${o.iid}`, claim_generation: o.claimGeneration, inventory_guarded: true,
    repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: o.originPath, default_branch: "main" },
    secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
  });
  api.setOwnershipStatus(o.runId, "running", o.claimGeneration);
  // The Cancel is persisted server-side the way the server derives it (a cancel row still unapplied, also served as an input row),
  // and the outbox lets the stop's ownership read run. In the proof steering routes the row first; see CANCEL_LOGS there.
  let outbox: Outbox | undefined;
  if (o.cancel === true) {
    api.setInputs(o.runId, [{ id: 1, kind: "cancel", body: null }]);
    api.deriveCancellationRequested = true;
    outbox = new Outbox({ root: path.join(tmp, "outbox"), log: nullLogger(), runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
    await outbox.init();
    for (let page = 0; page < 8; page++) if ((await outbox.scanTerminalObservationsPage()).passComplete) break;
  }

  const counters = { factories: 0, ran: 0 };
  const executor: Executor = { run: async () => { counters.ran++; throw new Error("a stopped claim runs no model"); } };
  const logs = recordingLogger();
  // The forge is never reached by a stop; a transport that answers 404 makes any reach fail loudly.
  const gitlab = new GitLabClient({ fetchFn: async () => ({ status: 404, text: async () => "{}" }) });
  const runner = new RunRunner(client, git, () => { counters.factories++; return { executor, homeDir: o.homeDir }; }, logs.logger, 20, AUTH_TOKEN, {
    pollMs: 5, planApprovalTimeoutMs: 0, questionTimeoutMs: 600, prDescriptionHeadLagMs: 0, gitlab, teardownTestDeps: portableTeardownTestDeps,
    authority, codexBoundaryDeadlineMs: 5000, recoveryRetryMs: 5, restoreIdentity: { procRoot: sProc, hostname: host }, independentRestore: true,
    checkpointTestHooks: { retryBackoffMs: [0, 0] }, ...(outbox ? { outbox } : {}),
  });
  return {
    runner, api, git, authority, claim, logs, counters,
    // The worker lifetime lock (acquireWorkerLifetime, "no release") and the AuthorityRuntime expose no release or close API: both are
    // held for the life of the test process, so only the api and the temp directory are released here.
    close: async () => { try { await api.close(); } finally { fs.rmSync(tmp, { recursive: true, force: true }); } },
  };
}
