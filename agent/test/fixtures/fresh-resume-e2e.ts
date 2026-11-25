// Launched by TestFreshResumeSeparateWorkerE2E (api/internal/handler). One process is one worker generation; the Go test owns the
// API (real routes, LiveDB), the owner steps (preview, start, cancel) and every assertion about server rows. This process uses the
// REAL WorkerClient and, for the destination, the REAL RunRunner. No fake API, state route, receipt or recovery client.
//
// Modes (UZI_FRESH_E2E_FIXTURE.mode):
//   w1            the predecessor W1: claim the run, publish a checkpoint and upload a capture through the real routes, park the run
//                 at its owner hold. W1 works in its own root; nothing of it is ever given to W2.
//   claim         register and claim only (the destination W2, or a reclaiming W3): no runner, no seed.
//   w2            the destination W2: register, claim, RunRunner.execute (seed, receipt, L4, continuation, finalize).
//   edit-journal  rewrite W2's fresh journal and attempt ledger entry to a different attempt id (offline, no API).
//
// The last stdout line is "RESULT <json>"; the Go test parses it. Worker logs go to cfg.logFile (never the token).
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { Readable } from "node:stream";
import { WorkerClient } from "../../src/client.js";
import { freshResumeCapabilities } from "../../src/fresh-resume.js";
import { freshJournalJson, freshJournalKey, parseFreshJournal } from "../../src/fresh-journal.js";
import { GitCache } from "../../src/git.js";
import { GitLabClient } from "../../src/forge.js";
import { RunRunner } from "../../src/runner.js";
import { mintAttemptId, setQuiescenceViewForTests } from "../../src/run-quiescence.js";
import type { Executor, RunContext } from "../../src/executor.js";
import type { Logger } from "../../src/log.js";
import { scopedRealView } from "../fake-proc.js";
import { acquireWorkerLifetime } from "../../src/authority-lock.js";
import { AuthorityRuntime } from "../../src/authority-runtime.js";
import { installWorkerLifetime, requireWorkerLifetime } from "../../src/completion-authority.js";
import { testLockOptions } from "../authority-test-support.js";

const cfg = JSON.parse(process.env.UZI_FRESH_E2E_FIXTURE ?? "") as {
  mode: "w1" | "claim" | "w2" | "edit-journal";
  url: string; token: string; run: string; origin: string; root: string; logFile: string; markerFile: string;
  /** w1: which evidence it leaves behind. */
  evidence?: "capture" | "checkpoint_ref" | "junk_capture" | "ambiguous";
  /** w2: the executor's behaviour after the seed. `hold` touches markerFile and waits for the abort. */
  executor?: "finish" | "hold";
  /** w2: run with the production completion-authority runtime (the worker-lifetime lock and the slot ledger), as main.ts does. A
   *  hard-killed generation then leaves its slot active under a dead incarnation. */
  authority?: boolean;
  /** w2: how many claim polls to make before giving up (a restart waits for the requeue). */
  claimPolls?: number;
};
assert.ok(path.isAbsolute(cfg.root) && path.isAbsolute(cfg.origin) && path.isAbsolute(cfg.logFile));
process.umask(0o077);
fs.mkdirSync(cfg.root, { recursive: true });
setQuiescenceViewForTests(scopedRealView());

const record = (level: string) => (message: string, fields?: Record<string, unknown>) => {
  fs.appendFileSync(cfg.logFile, JSON.stringify({ level, mode: cfg.mode, message, fields }) + "\n");
};
const log: Logger = {
  debug: record("debug"), info: record("info"), warn: record("warn"), error: record("error"),
  addSecret: () => {}, removeSecret: () => {}, child: () => log,
};
const GIT_ENV = {
  ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0",
  GIT_CONFIG_COUNT: "3",
  GIT_CONFIG_KEY_0: "maintenance.auto", GIT_CONFIG_VALUE_0: "false",
  GIT_CONFIG_KEY_1: "gc.auto", GIT_CONFIG_VALUE_1: "0",
  GIT_CONFIG_KEY_2: "core.fsmonitor", GIT_CONFIG_VALUE_2: "false",
};
const IDENT = ["-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false"];
function gitAt(dir: string, args: string[], input?: Buffer): string {
  return execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"], ...(input ? { input } : {}) }).trim();
}
function gitBytes(dir: string, args: string[], input: string): Buffer {
  return execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, input, stdio: ["pipe", "pipe", "pipe"], maxBuffer: 64 << 20 });
}
const FORGE_URL = "https://forge.example/fixture.git";
const W1_CAPS = ["recovery_archive_v1", "recovery_archive_v2"];
const W2_CAPS = ["completion_interlock_v1", "recovery_archive_v1", "recovery_archive_v2", "recovery_inventory_v1",
  "credential_switch_v1", "repo_agent_source_v1"];

/** A new worker process starts with nothing active, and says so at register (as worker.ts does): the api then requeues the runs a
 *  predecessor generation of this worker left behind. */
const EMPTY_SNAPSHOT = () => ({ snapshot_epoch: 1, active: [], pending_overflow: false });

function result(value: Record<string, unknown>): void {
  console.log("RESULT " + JSON.stringify(value));
}

async function runW1(): Promise<void> {
  assert.deepEqual(freshResumeCapabilities({ UZI_POD_UID: undefined }), [], "a non-hosted slot never advertises fresh_resume_v1");
  const client = new WorkerClient(cfg.url, cfg.token, "0.1.0-test", log, { terminalRetrySchedule: [5, 5] });
  await client.register("fresh-w1", undefined, 1, [], W1_CAPS);
  const claim = await client.claimRun();
  assert.ok(claim, "W1 claims the queued run");
  assert.equal(claim.run_id, cfg.run);
  const generation = claim.claim_generation;
  assert.ok(typeof generation === "number" && generation > 0);
  // An issue run carries no explicit coordinates: the server derives agent/issue-<iid> for both the work branch and the checkpoint key.
  const coords = (claim.checkpoint_coordinates as { work_branch: string; checkpoint_key: string } | undefined) ??
    { work_branch: `agent/issue-${(claim as unknown as { issue_iid: number }).issue_iid}`, checkpoint_key: `agent/issue-${(claim as unknown as { issue_iid: number }).issue_iid}` };
  const running = await client.reportState(cfg.run, { status: "running", claim_generation: generation });
  assert.equal(running.applied, true, "W1's running report applied");

  const clone = path.join(cfg.root, "clone");
  execFileSync("git", ["clone", "-q", cfg.origin, clone], { env: GIT_ENV, stdio: "pipe" });
  const mainTip = gitAt(clone, ["rev-parse", "origin/main"]);
  gitAt(clone, ["checkout", "-q", "-b", coords.work_branch, "origin/main"]);
  const commit = (file: string, text: string): string => {
    fs.writeFileSync(path.join(clone, file), text);
    gitAt(clone, ["add", file]);
    gitAt(clone, [...IDENT, "commit", "-q", "-m", file]);
    return gitAt(clone, ["rev-parse", "HEAD"]);
  };
  const m1 = commit("milestone-1.txt", "milestone one, committed by W1\n");
  const m2 = commit("milestone-2.txt", "milestone two, committed by W1\n");

  const publish = async (tip: string): Promise<void> => {
    const pack = gitBytes(clone, ["pack-objects", "--revs", "--stdout", "-q"], `${tip}\n^${mainTip}\n`);
    const out = await client.publishCheckpoint(cfg.run, tip, Readable.from([pack]), undefined, generation);
    assert.ok(out.ok && out.body.published === true, "W1's checkpoint publish through the real route: " + JSON.stringify(out));
  };
  const capture = async (source: string, bundleBytes: Buffer, prerequisites: string[]): Promise<string> => {
    const reserved = await client.reserveRecoveryCapture(cfg.run, {
      run_id: cfg.run, idempotency_key: "fresh-e2e-" + source.slice(0, 12) + "-" + createHash("sha256").update(bundleBytes).digest("hex").slice(0, 8),
      source_sha: source, generation,
    });
    const file = path.join(cfg.root, "w1-" + reserved.capture_id + ".bundle");
    fs.writeFileSync(file, bundleBytes);
    const status = await client.uploadRecoveryBundle(cfg.run, reserved.capture_id, {
      byte_size: bundleBytes.length, checksum: createHash("sha256").update(bundleBytes).digest("hex"),
      chunk_count: Math.max(1, Math.ceil(bundleBytes.length / (1024 * 1024))), prerequisite_shas: prerequisites,
    }, fs.createReadStream(file));
    assert.equal(status.state, "available", "the capture is available after the upload: " + JSON.stringify(status));
    return reserved.capture_id;
  };
  // The worker's recovery bundles carry exactly one ref, refs/heads/recovered-source, at the captured head.
  const bundle = (tipSha: string, excludes: string[]): Buffer => {
    const file = path.join(cfg.root, "tmp.bundle");
    fs.rmSync(file, { force: true });
    gitAt(clone, ["update-ref", "refs/heads/recovered-source", tipSha]);
    try { gitAt(clone, ["bundle", "create", "-q", file, "refs/heads/recovered-source", ...excludes.map(x => "^" + x)]); }
    finally { gitAt(clone, ["update-ref", "-d", "refs/heads/recovered-source"]); }
    return fs.readFileSync(file);
  };

  let captureId = "";
  let tip = "";
  let source = m2;
  switch (cfg.evidence) {
    case "capture":
      // An early checkpoint at M1 and a later thin capture of M2 on top of it: the capture dominates the checkpoint.
      await publish(m1); tip = m1;
      captureId = await capture(m2, bundle(m2, [m1]), [m1]);
      break;
    case "checkpoint_ref":
      await publish(m2); tip = m2;
      break;
    case "junk_capture":
      // A capture whose bytes are not a git bundle but whose manifest is consistent: the server stores it, W2 cannot verify it.
      captureId = await capture(m2, Buffer.from("this is not a git bundle\n".repeat(8)), []);
      break;
    case "ambiguous": {
      // A checkpoint at M1 and a capture of a diverged commit: neither contains the other.
      await publish(m1); tip = m1;
      gitAt(clone, ["checkout", "-q", "-b", "diverged", "origin/main"]);
      source = commit("diverged.txt", "diverged work\n");
      captureId = await capture(source, bundle(source, [mainTip]), [mainTip]);
      break;
    }
    default:
      throw new Error("unknown evidence " + String(cfg.evidence));
  }
  const parked = await client.reportState(cfg.run, { status: "recovery_wait", recovery_cause: "retained_recovery_adoption_failed", claim_generation: generation });
  result({ generation, m1, m2, source, mainTip, tip, captureId, parked: { applied: parked.applied, status: parked.status, cause: parked.recoveryCause ?? null },
    workBranch: coords.work_branch, checkpointKey: coords.checkpoint_key });
}

async function runClaim(): Promise<void> {
  const client = new WorkerClient(cfg.url, cfg.token, "0.1.0-test", log, { terminalRetrySchedule: [5, 5] });
  const caps = [...W2_CAPS, ...freshResumeCapabilities()];
  await client.register("fresh-claimer", undefined, 1, [], caps, EMPTY_SNAPSHOT());
  if (cfg.claimPolls === 0) { result({ registered: true, caps }); return; }
  const claim = await pollClaim(client, cfg.claimPolls ?? 8);
  assert.ok(claim, "the claimer received the run");
  result({ claim_generation: claim.claim_generation, fresh_resume: claim.fresh_resume ?? null, caps });
}

async function pollClaim(client: WorkerClient, polls: number) {
  for (let i = 0; i < polls; i++) {
    const claim = await client.claimRun();
    if (claim) return claim;
    await new Promise(r => setTimeout(r, 250));
  }
  return null;
}

async function runW2(): Promise<void> {
  const data = path.join(cfg.root, "data");
  fs.mkdirSync(data, { recursive: true });
  // Offline forge transport: only the fixture's HTTPS remote is rewritten to its bare origin.
  const forgeGitConfig = path.join(cfg.root, "forge-git-config");
  fs.writeFileSync(forgeGitConfig, `[url "${cfg.origin}"]\n\tinsteadOf = ${FORGE_URL}\n`);
  process.env.GIT_CONFIG_GLOBAL = forgeGitConfig;
  GIT_ENV.GIT_CONFIG_GLOBAL = forgeGitConfig;

  const hosted = freshResumeCapabilities();
  assert.deepEqual(hosted, ["fresh_resume_v1"], "UZI_POD_UID makes this slot a hosted fresh resume destination");
  const client = new WorkerClient(cfg.url, cfg.token, "0.1.0-test", log, { terminalRetrySchedule: [5, 5] });
  let authority: AuthorityRuntime | undefined;
  if (cfg.authority) {
    installWorkerLifetime(await acquireWorkerLifetime(data, testLockOptions()));
    authority = new AuthorityRuntime({ lifetime: requireWorkerLifetime(), workerToken: cfg.token, log });
  }
  const cache = new GitCache(data, log, undefined, authority ? { authority: authority.hooks, authorityRequired: true } : undefined);
  // This host lacks the worker/command UID split the retained-source readiness helper needs; model only that readiness.
  cache.normalizeRetainedSourceObjects = async () => {};
  // Observation only: record why a seed step failed (the runner reports a closed reason string).
  for (const step of ["fetchFreshBundle", "seedFreshAttempt"] as const) {
    const original = cache[step].bind(cache) as (...a: unknown[]) => Promise<unknown>;
    (cache as unknown as Record<string, unknown>)[step] = async (...a: unknown[]) => {
      try { return await original(...a); }
      catch (err) { log.warn("fresh seed step failed", { step, error: err instanceof Error ? err.name + ": " + err.message : String(err) }); throw err; }
    };
  }
  let mrCreates = 0;
  let description = "";
  let finalHead = "";
  const forge = new GitLabClient({
    fetchFn: async (url, init) => {
      if (init.method === "POST" && url.includes("/merge_requests")) {
        mrCreates++;
        finalHead = gitAt(cfg.origin, ["for-each-ref", "--format=%(objectname)", "refs/heads/agent/"]).split("\n")[0] ?? "";
        description = String((JSON.parse(init.body ?? "{}") as { description?: string }).description ?? "");
        return { status: 201, text: async () => JSON.stringify({ iid: 42, web_url: "https://forge.example/fixture/merge_requests/42" }) };
      }
      if (init.method === "GET") return { status: 200, text: async () => JSON.stringify({ sha: finalHead, target_branch: "main", description, state: "opened" }) };
      if (init.method === "PUT") {
        description = String((JSON.parse(init.body ?? "{}") as { description?: string }).description ?? "");
        return { status: 200, text: async () => "{}" };
      }
      throw new Error("unexpected forge edge");
    },
  });

  const facts: Record<string, unknown> = { executorRuns: 0 };
  let continuedHead = "";
  const factory = () => {
    const executor: Executor = {
      sandboxesCommands: false,
      run: async (ctx: RunContext) => {
        facts.executorRuns = (facts.executorRuns as number) + 1;
        const tree = ctx.worktreePath;
        const bare = cache.barePathFor(FORGE_URL);
        const defaultTip = gitAt(cfg.origin, ["rev-parse", "refs/heads/main"]);
        const head = gitAt(tree, ["rev-parse", "HEAD"]);
        const journal = await cache.readFreshJournal(bare, ctx.branch);
        facts.executionBase = head;
        facts.defaultTip = defaultTip;
        facts.attemptPath = tree;
        facts.journal = journal ?? null;
        facts.strictlyAhead = head !== defaultTip && (() => {
          try { gitAt(tree, ["merge-base", "--is-ancestor", defaultTip, head]); return true; } catch { return false; }
        })();
        facts.commitsAhead = Number(gitAt(tree, ["rev-list", "--count", `${defaultTip}..${head}`]));
        facts.files = fs.readdirSync(tree).filter(f => !f.startsWith(".")).sort();
        if (cfg.executor === "hold") {
          fs.writeFileSync(cfg.markerFile, "executor entered\n");
          await new Promise<void>((_, reject) => {
            const onAbort = () => reject(ctx.signal?.reason ?? new Error("executor aborted"));
            if (ctx.signal?.aborted) onAbort(); else ctx.signal?.addEventListener("abort", onAbort, { once: true });
          });
        }
        fs.writeFileSync(path.join(tree, "continued.txt"), "the model continued on W2\n");
        gitAt(tree, ["add", "continued.txt"]);
        gitAt(tree, [...IDENT, "commit", "-q", "-m", "continued on W2"]);
        continuedHead = gitAt(tree, ["rev-parse", "HEAD"]);
        return { branch: ctx.branch };
      },
    };
    return { executor, homeDir: path.join(cfg.root, "home") };
  };
  fs.mkdirSync(path.join(cfg.root, "home"), { recursive: true });
  const runner = new RunRunner(client, cache, factory, log, 10, cfg.token, {
    gitlab: forge, recoveryRetryMs: 10, pollMs: 20, checkpointTickIntervalMs: 0, ...(authority ? { authority } : {}),
  });
  let outcome = "returned";
  let error: string | null = null;
  try {
    const registration = await client.register("fresh-w2", undefined, 1, [], [...W2_CAPS, ...hosted], EMPTY_SNAPSHOT());
    facts.workerId = registration.worker_id;
    const claim = await pollClaim(client, cfg.claimPolls ?? 8);
    assert.ok(claim, "W2 received the run");
    facts.claimGeneration = claim.claim_generation;
    facts.claimFresh = claim.fresh_resume ?? null;
    try {
      await runner.execute(claim);
    } catch (err) {
      outcome = "threw";
      error = err instanceof Error ? err.name + ": " + err.message : String(err);
    }
  } finally {
    runner.shutdown();
  }
  const bare = cache.barePathFor(FORGE_URL);
  let journalAfter: unknown = null;
  if (fs.existsSync(bare)) {
    const branch = await branchOf(bare);
    journalAfter = branch ? await cache.readFreshJournal(bare, branch).catch(() => null) ?? null : null;
  }
  result({ outcome, error, mrCreates, continuedHead, finalHead, description, ...facts, journalAfter });
}

/** The branch the bare's fresh journal names ("" when it carries none). */
async function branchOf(bare: string): Promise<string> {
  let keys: string[];
  try {
    keys = gitAt(bare, ["config", "--local", "--name-only", "--get-regexp", "^uzi-freshresume\\..*\\.journal$"]).split("\n");
  } catch { return ""; }
  const m = /^uzi-freshresume\.(.+)\.journal$/.exec(keys[0] ?? "");
  return m?.[1] ?? "";
}

async function runEditJournal(): Promise<void> {
  const data = path.join(cfg.root, "data");
  const cache = new GitCache(data, log);
  const bare = cache.barePathFor(FORGE_URL);
  const branch = await branchOf(bare);
  assert.ok(branch, "W2's bare carries a fresh journal");
  const journal = parseFreshJournal(gitAt(bare, ["config", "--local", "--get", freshJournalKey(branch)]));
  const other = mintAttemptId(journal.generation, new Date(Date.now() + 120_000));
  assert.notEqual(other, journal.attemptId);
  const next = { ...journal, attemptId: other };
  gitAt(bare, ["config", "--local", freshJournalKey(branch), freshJournalJson(next)]);
  // The ledger names the attempt by id too: a journal and a ledger that agree on another attempt, as a replaced seed would leave.
  const ledgerKey = `uzi-attempts.${branch}.entry`;
  const entries = gitAt(bare, ["config", "--local", "--get-all", ledgerKey]).split("\n").filter(Boolean);
  gitAt(bare, ["config", "--local", "--unset-all", ledgerKey]);
  for (const entry of entries) gitAt(bare, ["config", "--local", "--add", ledgerKey, entry.split(journal.attemptId).join(other)]);
  result({ from: journal.attemptId, to: other, ledgerEntries: entries.length });
}

try {
  if (cfg.mode === "w1") await runW1();
  else if (cfg.mode === "claim") await runClaim();
  else if (cfg.mode === "w2") await runW2();
  else await runEditJournal();
  process.exit(0);
} catch (err) {
  console.error("fixture failed: " + (err instanceof Error ? err.stack ?? err.message : String(err)));
  process.exit(1);
}
