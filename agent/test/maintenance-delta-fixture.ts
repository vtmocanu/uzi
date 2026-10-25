import assert from "node:assert/strict";
import type { TestContext } from "node:test";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { execFile, spawn } from "node:child_process";
import { promisify } from "node:util";
import { PassThrough, type Readable } from "node:stream";
import { GitCache } from "../src/git.js";
import { MaintenanceResources, RecoveryMaintenance } from "../src/recovery-maintenance.js";
import type { MaintenanceResource } from "../src/recovery-maintenance.js";
import type { BoundaryProcessRequest } from "../src/harness.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { nullLogger } from "./helpers.js";

const exec = promisify(execFile);
export type TransferPhase = "walk" | "selection" | "pack" | "proof" | "import";

/** Fixed-phase unit observation only; this is not the C2 composed measurement harness. */
class DeltaCache extends GitCache {
  phase?: TransferPhase;
  beforePhase?: (phase: TransferPhase) => Promise<void>;
  afterPhase?: (phase: TransferPhase) => Promise<void>;
  protected override async observeMaintenanceTransferPhase<T>(phase: TransferPhase, action: () => Promise<T>): Promise<T> {
    this.phase = phase;
    try {
      await this.beforePhase?.(phase);
      const result = await action();
      await this.afterPhase?.(phase);
      return result;
    } finally { this.phase = undefined; }
  }
}

/**
 * Real Git bytes, family qualification, canonical attempt/journal metadata, locks,
 * children and original resource slot. Only trusted ancestor DAC is modeled.
 * beforeSemantic models the owner's pair; no live custody API or output index.
 */
export async function maintenanceDeltaFixture(t: TestContext, options: { clean?: boolean; dissociated?: boolean; decodedLimit?: number; shallowSource?: boolean } = {}) {
  const oldMask = process.umask(0o077);
  t.after(() => process.umask(oldMask));
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "maintenance-delta-"));
  await fs.chmod(dir, 0o700);
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const cache = new DeltaCache(dir, nullLogger(), undefined, { recoveryDecodedLimitBytes: options.decodedLimit });
  const seed = path.join(dir, "seed");
  const native = async (cwd: string, args: string[]) => {
    // Explicit environment clears all inherited GIT_* variables.
    const command = exec("/usr/bin/git", ["-C", cwd, ...args], { env: {
      PATH: "/usr/bin:/bin", HOME: dir, GIT_CONFIG_NOSYSTEM: "1",
      GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
      GIT_AUTHOR_NAME: "fixture", GIT_AUTHOR_EMAIL: "fixture@example.test",
      GIT_COMMITTER_NAME: "fixture", GIT_COMMITTER_EMAIL: "fixture@example.test",
    } });
    command.child.stdin?.end();
    return (await command).stdout.trim();
  };
  await fs.mkdir(seed);
  await native(seed, ["init", "-q", "-b", "main"]);
  await fs.writeFile(path.join(seed, "base"), "canonical prerequisite\n");
  await native(seed, ["add", "."]); await native(seed, ["commit", "-qm", "base"]);
  let originSource = seed;
  if (options.shallowSource) {
    await fs.writeFile(path.join(seed, "second"), "shallow-source descendant\n");
    await native(seed, ["add", "."]); await native(seed, ["commit", "-qm", "second source commit"]);
    originSource = path.join(dir, "shallow-origin");
    await native(dir, ["clone", "--depth=1", "--no-local", "-q", "file://" + seed, originSource]);
  }
  const bare = cache.barePathFor(originSource);
  await fs.mkdir(path.dirname(bare), { recursive: true });
  await native(dir, ["clone", "--bare", "--no-hardlinks", "-q", originSource, bare]);
  const base = await native(bare, ["rev-parse", "HEAD"]);
  await native(bare, ["update-ref", "refs/remotes/origin/main", base]);
  await native(bare, ["update-ref", "refs/uzi-runner/feature", base]);
  const attemptId = "20261025T120000Z-g1-1234567890abcdef", branch = "feature", key = "issue-3441";
  const clone = cache.runnerClonePath(bare, key) + ".attempt-" + attemptId;
  await fs.mkdir(path.dirname(clone), { recursive: true });
  await native(dir, ["clone", "--shared", ...(options.dissociated ? ["--dissociate"] : []), "-q", bare, clone]);
  if (!options.clean) {
    await fs.writeFile(path.join(clone, "new"), "new selected blob\n");
    await native(clone, ["add", "."]); await native(clone, ["commit", "-qm", "new"]);
  }
  const tip = await native(clone, ["rev-parse", "HEAD"]);
  const stat = await fs.lstat(clone, { bigint: true });
  const source = { clonePath: clone, runId: "run", attemptId, identity: stat.dev + ":" + stat.ino };
  await native(bare, ["config", "uzi-attempts." + branch + ".entry",
    JSON.stringify({ runId: source.runId, clonePath: clone, attemptId, state: "abandoned" })]);
  await native(bare, ["config", "uzi-recovery." + branch + ".clone",
    JSON.stringify({ runId: source.runId, clonePath: clone, attemptId })]);
  await fs.mkdir(cache.recoveryRoot, { mode: 0o700 });

  // Model ONLY trusted ancestor permissions, including descriptor observations.
  // Real descendants retain dev/ino, nlink, contents, mode and ownership.
  const ancestors = new Set<string>();
  for (let p = dir;; p = path.dirname(p)) { ancestors.add(p); if (p === path.dirname(p)) break; }
  const descriptors = new Map<number, string>();
  const physical = (p: unknown) => {
    if (typeof p !== "string") return "";
    const m = /^\/proc\/self\/fd\/([0-9]+)(?:\/(.*))?$/.exec(p);
    return m && descriptors.has(Number(m[1])) ? path.join(descriptors.get(Number(m[1]))!, m[2] ?? "") : p;
  };
  const project = <T extends { mode: number | bigint; uid: number | bigint; gid: number | bigint }>(p: string, s: T) => {
    if (ancestors.has(p)) {
      const big = typeof s.mode === "bigint";
      s.mode = big ? BigInt(Number(s.mode) & ~0o7022) : Number(s.mode) & ~0o7022;
      s.uid = big ? BigInt(process.getuid!()) : process.getuid!();
      s.gid = big ? BigInt(process.getgid!()) : process.getgid!();
    }
    return s;
  };
  const hooks: {
    prepare?: () => Promise<void>;
    beforePair?: () => Promise<void>;
    beforeSpawn?: (request: BoundaryProcessRequest) => Promise<void>;
    stdout?: (request: BoundaryProcessRequest, stream: Readable) => Readable;
    open?: (file: Awaited<ReturnType<typeof fs.open>>, p: string) => Promise<void> | void;
    completed?: () => Promise<void>;
    cancel?: () => Promise<void>;
    holdStderr?: boolean;
    stderrDestroy?: (error: Error | null, callback: (error?: Error | null) => void) => void;
    stderr?: PassThrough;
  } = {};
  const lstat = fs.lstat.bind(fs), open = fs.open.bind(fs);
  t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) =>
    project(physical(args[0]), await lstat(...args)));
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const file = await open(...args), p = physical(args[0]);
    descriptors.set(file.fd, p);
    const fstat = file.stat.bind(file);
    t.mock.method(file, "stat", async (...a: Parameters<typeof file.stat>) => project(p, (await fstat(...a))!));
    await hooks.open?.(file, p);
    return file;
  });

  const ledger = new RecoveryMaintenance({ root: path.join(dir, "recovery-maintenance"), workerToken: "fixture" });
  const admitted = await ledger.admit({
    identity: { run: "run", generation: 1, hold: "hold", repo: "repo", custodian: "worker",
      bareBasename: path.basename(bare), origin: "a".repeat(64), originalSource: "b".repeat(64), sourceConfig: "c".repeat(64) },
    config: RecoveryMaintenance.configuration(), exactCustody: true, terminalNonStopped: true, source: "b".repeat(64),
  });
  assert.ok(admitted.ok);
  t.after(() => ledger.finish(admitted.value, "transient"));
  const charges: Array<{ resource: MaintenanceResource; amount: number; phase?: TransferPhase }> = [];
  const consume = MaintenanceResources.consume.bind(MaintenanceResources);
  // consumeIfActive routes here too. Never replace accounting or mint a fresh slot.
  t.mock.method(MaintenanceResources, "consume", (resource: MaintenanceResource, amount: number) => {
    consume(resource, amount);
    charges.push({ resource, amount, phase: cache.phase });
  });
  const controller = new AbortController();
  const requests: BoundaryProcessRequest[] = [], trace: string[] = [];
  let joined = 0, pairs = 0, cancelled = 0;
  const request = {
    source, original: { barePath: bare, baseCommit: base, cloneUrl: originSource }, tip, defaultBranch: "main", branch, key,
    family: [source], outDir: path.join(cache.recoveryRoot, "delta"),
    prepare: async () => { controller.signal.throwIfAborted(); await hooks.prepare?.(); },
    beforeSemantic: async () => {
      await hooks.beforePair?.();
      MaintenanceResources.consume("rpcAttempts", 2); pairs++; trace.push("pair");
    },
    validateFamily: async (snapshot: { sources: typeof source[]; metadata: string; origin: string | undefined }) => {
      assert.deepEqual(snapshot.sources, [source], "actual locked family matches the admitted tuple");
      assert.ok(snapshot.metadata.includes("uzi-attempts."), "family metadata is canonical");
      assert.match(snapshot.origin!, /^[a-f0-9]{64}$/, "real canonical origin qualifies");
      trace.push("family");
    },
  };
  const inScope = <T>(action: () => Promise<T>) => MaintenanceResources.run(admitted.value, () => MaintenanceBudget.runProducer(budget =>
    cache.withBoundaryProcessSpawner(async childRequest => {
      await hooks.beforeSpawn?.(childRequest);
      controller.signal.throwIfAborted();
      requests.push(childRequest);
      trace.push("git:" + gitVerb(childRequest));
      const child = spawn(childRequest.argv[0]!, childRequest.argv.slice(1), { cwd: childRequest.cwd, env: childRequest.env });
      const completed = new Promise<{ code: number }>((resolve, reject) => {
        child.once("error", reject);
        child.once("close", code => {
          void (async () => { await hooks.completed?.(); joined++; resolve({ code: code ?? -1 }); })().catch(reject);
        });
      });
      const stderr = hooks.holdStderr ? (hooks.stderr = new PassThrough({ destroy: hooks.stderrDestroy })) : child.stderr;
      if (hooks.holdStderr && hooks.stderr) child.stderr.pipe(hooks.stderr, { end: false });
      return { stdin: child.stdin, stdout: hooks.stdout?.(childRequest, child.stdout) ?? child.stdout, stderr, completed,
        cancel: async () => { cancelled++; child.kill("SIGKILL"); await hooks.cancel?.(); await completed; } };
    }, controller.signal, () => cache.withRecoveryOperation(controller.signal, Date.now() + 110000,
      action, budget))));
  // A genuine capture binds its own process owner and cannot overlay the ordinary spawner.
  const inCaptureScope = <T>(action: () => Promise<T>) => MaintenanceResources.run(admitted.value, () =>
    MaintenanceBudget.runProducer(budget => cache.withRecoveryOperation(controller.signal, Date.now() + 110000, action, budget)));
  const run = () => inScope(() => cache.importMaintenanceTip(request));
  const charge = (resource: MaintenanceResource, amount: number) => MaintenanceResources.run(admitted.value,
    () => MaintenanceResources.consume(resource, amount));
  const refs = () => native(bare, ["for-each-ref", "--format=%(refname) %(objectname)"]);
  const retained = async () => {
    assert.equal((await fs.lstat(clone)).isDirectory(), true, "source retained");
    assert.equal(admitted.value.ordinal, 1, "original custody slot remains the only slot");
    assert.match(await native(bare, ["config", "--get", "uzi-recovery." + branch + ".clone"]), /"runId":"run"/,
      "canonical custody descriptor retained");
  };
  return { dir, seed, bare, base, clone, tip, source, cache, native, request, run, refs, retained,
    requests, trace, charges, controller, hooks, physical, inScope, inCaptureScope, charge,
    pairs: () => pairs, joined: () => joined, cancelled: () => cancelled };
}

export function gitVerb(request: BoundaryProcessRequest): string {
  return request.argv.find(arg => ["config", "rev-parse", "for-each-ref", "merge-base", "rev-list", "cat-file", "pack-objects", "index-pack"].includes(arg)) ?? "unknown";
}
export function canonicalImport(request: BoundaryProcessRequest): boolean {
  return gitVerb(request) === "index-pack" && request.argv.includes("--stdin");
}
