// Issue #3644 m3: one worker generation as a REAL separate node process, for runner-independent-restore-producer.test.ts.
//
//   P  : the producer. A production RunRunner over the default-incarnation AuthorityRuntime of THIS process, this process's real
//        boundary record, real phaseClone / early pin / bindFlightSource, and a stub executor that parks at the plan gate and is
//        never approved. The test SIGKILLs it there; nothing of it is hand-written afterwards.
//   S  : the successor, a fresh process (its own incarnation) over the same data root, claiming generation 2 of the same run.
//   S2 : a later restart, claiming generation 3.
//
// The child only builds the runner and runs one claim; every assertion lives in the parent. It logs JSONL to cfg.logPath.
import "./setup/hermetic-proc.js";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { WorkerClient } from "../src/client.js";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { installWorkerLifetime, processWorkerIncarnation, requireWorkerLifetime } from "../src/completion-authority.js";
import { StubExecutor, type Executor } from "../src/executor.js";
import { GitLabClient, type FetchFn } from "../src/forge.js";
import { GitCache } from "../src/git.js";
import { Outbox } from "../src/outbox.js";
import type { StateRequest } from "../src/protocol.js";
import type { Logger } from "../src/log.js";
import { establishProcessBoundary } from "../src/restore-boundary.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { runDirKey } from "../src/util.js";
import { testLockOptions } from "./authority-test-support.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { testGitCacheOptions } from "./helpers.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";
import type { ChildConfig } from "./restore-producer-rig.js";

const cfg = JSON.parse(fs.readFileSync(process.argv[2]!, "utf8")) as ChildConfig;

function line(level: string, msg: string, base: Record<string, unknown>, fields?: Record<string, unknown>): void {
  let text: string;
  try { text = JSON.stringify({ level, msg, ...base, ...fields }); }
  catch { text = JSON.stringify({ level, msg, unserializable: true }); }
  fs.appendFileSync(cfg.logPath, text + "\n");
}
function makeLogger(base: Record<string, unknown>): Logger {
  return {
    debug: (m, f) => line("debug", m, base, f), info: (m, f) => line("info", m, base, f),
    warn: (m, f) => line("warn", m, base, f), error: (m, f) => line("error", m, base, f),
    addSecret() {}, removeSecret() {}, child: fields => makeLogger({ ...base, ...fields }),
  };
}
const log = makeLogger({});
const note = (msg: string, fields: Record<string, unknown> = {}): void => line("info", msg, { role: cfg.role }, fields);

const GIT_ENV: NodeJS.ProcessEnv = { GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", PATH: process.env.PATH };
const gitIn = (dir: string, args: string[]): string => execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim();

/** The forge transport: every request is forwarded to the fake forge the parent hosts over HTTP. */
const forgeFetch: FetchFn = async (url, init) => {
  const res = await fetch(`${cfg.forgeUrl}/forward`, { method: "POST", body: JSON.stringify({ url, method: init.method, headers: init.headers, body: init.body }) });
  if (res.status !== 200) throw new Error(`forge bridge answered ${res.status}`);
  const answer = await res.json() as { status: number; body: string };
  return { status: answer.status, text: async () => answer.body };
};

/** An executor that records what the generation sees at admission, writes the files an agent's HOME and provisioning step would, then
 *  hands over to the real stub (P parks at its plan gate; S and S2 complete). */
function observing(stub: StubExecutor, generation: number | undefined, runId: string): Executor {
  const wrapped = Object.create(stub) as Executor;
  wrapped.run = async ctx => {
    const home = path.join(cfg.homeRoot, runDirKey(runId, generation));
    fs.mkdirSync(path.join(home, ".npm", "_cacache"), { recursive: true });
    fs.writeFileSync(path.join(home, ".npm", "_cacache", "blob"), "cached bytes");
    fs.writeFileSync(path.join(home, "session-proof"), `session of ${cfg.role}`);
    const provision = path.join(cfg.dataDir, "provision", runDirKey(runId, generation));
    fs.mkdirSync(provision, { recursive: true });
    fs.writeFileSync(path.join(provision, "marker"), `provision bytes of ${cfg.role}`);
    note("child:executor admitted", {
      generation: generation ?? null, worktree: ctx.worktreePath, head: gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]),
      tracked: gitIn(ctx.worktreePath, ["ls-files"]).split("\n"),
      untracked: gitIn(ctx.worktreePath, ["ls-files", "--others", "--exclude-standard"]).split("\n").filter(Boolean),
    });
    if (cfg.role !== "S") return stub.run(ctx);
    // S's work, as the certified-completion test of the seed file models it: a checkpoint, more work after it, and a deliberate leftover.
    const commit = (name: string): string => {
      fs.writeFileSync(path.join(ctx.worktreePath, name), name + "\n");
      gitIn(ctx.worktreePath, ["add", name]);
      gitIn(ctx.worktreePath, ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-m", name]);
      return gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
    };
    const checkpointHead = commit("checkpoint.txt");
    await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
    const finalHead = commit("post-checkpoint.txt");
    fs.writeFileSync(path.join(ctx.worktreePath, "deliberate-leftover.txt"), "discard only after safe retirement\n");
    finalHeadSeen = finalHead;
    await fetch(`${cfg.forgeUrl}/head`, { method: "POST", body: finalHead });
    note("child:executor worked", { checkpointHead, finalHead });
    return { branch: ctx.branch };
  };
  return wrapped;
}

let finalHeadSeen = "";

/** The api-side facts a claim of this run depends on (custody holds, the capture and checkpoint endpoints, the completion receipt),
 *  answered the way the certified-completion test of runner-independent-restore-seed.test.ts answers them. FakeApi serves none of them. */
function installServerStandIn(client: WorkerClient, workerId: string): void {
  const wire = JSON.parse(fs.readFileSync(new URL("../../fixtures/completed-publication/state-ack.json", import.meta.url), "utf8"));
  const literal = wire.ack.completed_publication_receipt;
  const claim = cfg.claim;
  const generation = claim.claim_generation!;
  const originPath = claim.repo!.clone_url;
  const branch = `agent/issue-${claim.issue_iid}`;
  let open = cfg.role !== "S2";
  let manifest: { checksum: string; byte_size: number } | undefined;
  client.getRunOwnership = async () => ({ status: completedReported ? "completed" : "running", claim_generation: generation, inventory_guarded: true });
  client.listRecoveryHolds = async runId => ({ run_id: runId, holds: open ? [{
    hold_id: literal.hold_id, generation, inventory_guarded: true, has_available_capture: !!manifest }] : [] });
  client.reserveRecoveryCapture = async () => ({ capture_id: "00000000-0000-4000-8000-000000002507", state: "preparing" });
  client.uploadRecoveryBundle = async (_run, _id, m, stream) => {
    for await (const chunk of stream) if (!chunk) throw new Error("empty bundle chunk");
    manifest = m;
    return { capture_id: "00000000-0000-4000-8000-000000002507", state: "available", manifest_bound: true };
  };
  client.getRecoveryCaptureStatus = async () => ({ capture_id: "00000000-0000-4000-8000-000000002507", state: manifest ? "available" : "preparing",
    manifest_bound: !!manifest, checksum: manifest?.checksum, byte_size: manifest?.byte_size, expires_at: new Date(Date.now() + 60_000).toISOString() });
  client.releaseRecoveryCustody = async (runId, g) => {
    open = false;
    return { run_id: runId, generation: g, released: true, holds_released: 1 };
  };
  client.publishCheckpoint = async (_run, tip, pack) => {
    const chunks: Buffer[] = [];
    for await (const chunk of pack) chunks.push(Buffer.from(chunk));
    execFileSync("git", ["-C", originPath, "index-pack", "--stdin", "--fix-thin"], { input: Buffer.concat(chunks), env: { ...process.env, ...GIT_ENV } });
    gitIn(originPath, ["update-ref", `refs/uzi-checkpoints/${branch}`, tip]);
    return { ok: true, body: { published: true, ref: `refs/uzi-checkpoints/${branch}` } };
  };
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    if (url.origin === new URL(cfg.apiUrl).origin && url.pathname.endsWith("/terminal-rejection-custody")) {
      return Response.json({ run_id: claim.run_id, worker_id: workerId, generation,
        exact_holds: [{ id: literal.hold_id, state: open ? "open" : "released" }], sibling_holds: [], exact_count: 1, sibling_count: 0,
        complete: true, exact_complete: true, sibling_complete: true, outcome: open ? "retained" : "settled" });
    }
    const response = await originalFetch(input, init);
    if (url.origin !== new URL(cfg.apiUrl).origin || url.pathname !== `/api/worker/runs/${claim.run_id}/state`) return response;
    const body = JSON.parse(String(init?.body)) as StateRequest;
    if (body.status !== "completed") return response;
    open = false;
    completedReported = true;
    return Response.json({ ...wire.ack, run: { ...wire.ack.run, id: claim.run_id, worker_id: workerId },
      completed_publication_receipt: { ...literal, run_id: claim.run_id, worker_id: workerId, generation,
        repo_id: claim.repo!.id, branch, final_head: finalHeadSeen, observed_branch_head: finalHeadSeen, mr_iid: body.mr_iid } });
  }) as typeof fetch;
}
let completedReported = false;

async function main(): Promise<void> {
  const token = cfg.joinToken;
  installWorkerLifetime(await acquireWorkerLifetime(cfg.dataDir, testLockOptions()));
  // Exactly as main.ts does, plus the fixture procfs this generation lives in.
  const boundary = await establishProcessBoundary({ dataDir: cfg.dataDir, workerToken: token, procRoot: cfg.procRoot, hostname: () => cfg.hostname });
  note("child:identity", { incarnation: processWorkerIncarnation(), boundaryOk: boundary.ok, pid: process.pid });
  if (!boundary.ok) throw new Error(`boundary record not written: ${boundary.reason}`);

  const authority = new AuthorityRuntime({ lifetime: requireWorkerLifetime(), workerToken: token, log });
  const git = new GitCache(cfg.dataDir, log, undefined, testGitCacheOptions({
    authority: authority.hooks, authorityRequired: true, gitleaksBin: defaultGitleaksShim(),
  }));
  const client = new WorkerClient(cfg.apiUrl, cfg.apiToken, "0.1.0-test", log, { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
  client.protocolFeatures = cfg.features;
  if (cfg.echoRecoveryCause) {
    const realReport = client.reportState.bind(client) as unknown as (...a: unknown[]) => Promise<Record<string, unknown>>;
    client.reportState = (async (...args: unknown[]) => {
      const body = args[1] as Record<string, unknown>;
      const ack = await realReport(...args);
      return body.status === "recovery_wait" && typeof body.recovery_cause === "string" ? { ...ack, recoveryCause: body.recovery_cause } : ack;
    }) as never;
  }

  const registration = await client.register("restore-producer-child", undefined, undefined, undefined, cfg.features);
  if (!registration.worker_id) throw new Error("the api did not register the worker");
  installServerStandIn(client, registration.worker_id);
  const outbox = new Outbox({ root: path.join(cfg.dataDir, "outbox"), log, runMaxBytes: 64 << 20, maxBytes: 128 << 20, retentionMs: 86_400_000 });
  await outbox.init();
  const makeExecutor: ExecutorFactory = (id, _codex, addr) => {
    const homeDir = path.join(cfg.homeRoot, runDirKey(id, addr?.generation));
    note("child:executor built", { generation: addr?.generation ?? null, home: homeDir });
    return { executor: observing(new StubExecutor(log, { planGate: cfg.role === "P" }), addr?.generation, id), homeDir };
  };
  const runner = new RunRunner(client, git, makeExecutor, log, 20, token, {
    pollMs: 25, planApprovalTimeoutMs: 0, questionTimeoutMs: 600, prDescriptionHeadLagMs: 0,
    gitlab: new GitLabClient({ fetchFn: forgeFetch }), teardownTestDeps: portableTeardownTestDeps,
    authority, codexBoundaryDeadlineMs: 5000, recoveryRetryMs: 5,
    restoreIdentity: { procRoot: cfg.procRoot, hostname: () => cfg.hostname },
    checkpointTestHooks: { retryBackoffMs: [0, 0] }, outbox,
    ...(cfg.role === "S" ? { checkpointIntervalMs: 1, checkpointTickIntervalMs: 0 } : {}),
    ...(cfg.dockerHost === undefined ? {} : {
      dockerHost: cfg.dockerHost,
      // The scripted quiescence of runner-docker-resume.test.ts: a Docker-wired worker that never contacts a daemon.
      quiesceRun: async () => ({
        process: { state: "quiescent", processes: [], killed: [], detail: "producer-child fixture" },
        docker: { state: "docker_unconfirmed", removed: [], detail: "producer-child fixture" },
      }),
    }),
  });

  note("child:execute start", { generation: cfg.claim.claim_generation ?? null });
  await runner.execute(cfg.claim);
  note("child:execute returned");
  await runner.resumePendingRecoveries();
}

// The parent holds our stdin open; if it dies (or closes it), do not outlive it.
process.stdin.on("end", () => process.exit(1));
process.stdin.resume();

main().then(() => { note("child:done"); process.exit(0); }, error => {
  note("child:error", { error: error instanceof Error ? (error.stack ?? error.message) : String(error) });
  process.exit(1);
});
