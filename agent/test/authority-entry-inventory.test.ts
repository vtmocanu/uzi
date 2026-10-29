import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import type { Readable } from "node:stream";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import { fileURLToPath } from "node:url";
import { acquireWorkerLifetime, AuthorityRefusal } from "../src/authority-lock.js";
import { AuthorityRuntime, type AuthorityScope } from "../src/authority-runtime.js";
import { openAuthorityStore } from "../src/completion-authority.js";
import type { Executor } from "../src/executor.js";
import { GitCache } from "../src/git.js";
import type { GitAuthorityHooks } from "../src/git-authority-hooks.js";
import { RecoveryCoordinator, type RecoveryArchiveClient, type RecoveryBundleProducer } from "../src/recovery.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { RunRunner } from "../src/runner.js";
import type { WorkerClient } from "../src/client.js";
import { testLockOptions } from "./authority-test-support.js";
import { makeFixture, makeScratchRoot, type Fixture } from "./fixture-repo.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

// issue #3404 M2 U3 — the entry points that still need A. Every production entry below is driven with a
// REAL authority runtime over a GitCache built authorityRequired: a site that lost (or never had) its wrap
// would be refused by the hook (AuthorityRefusal) and the effect would not happen; the recording hooks also
// say whether A was held when the hook ran. Sites that need no A (their only hook is the transaction-free
// assertBareClear) are pinned the other way, so the wrapper is not reintroduced around transport.

const TOKEN = "unit-join-" + randomBytes(12).toString("hex");

interface HookCall { hook: string; op: string; aHeld: boolean }

interface Env {
  dataDir: string;
  rt: AuthorityRuntime;
  cache: GitCache;
  calls: HookCall[];
  bare: string;
}

/** Hooks that record whether A was held, then delegate to the real runtime. */
function recordingHooks(rt: AuthorityRuntime, calls: HookCall[]): GitAuthorityHooks {
  const note = (hook: string, op: string): void => { calls.push({ hook, op, aHeld: rt.active() !== undefined }); };
  return {
    admitSourceUse: async (p, op) => { note("admitSourceUse", op); await rt.hooks.admitSourceUse(p, op); },
    beginBareMutation: async (b, op, footprint) => { note("beginBareMutation", op); return rt.hooks.beginBareMutation(b, op, footprint); },
    assertBareClear: async (b, op) => { note("assertBareClear", op); await rt.hooks.assertBareClear(b, op); },
    beginDestructive: async (paths, op) => { note("beginDestructive", op); return rt.hooks.beginDestructive(paths, op); },
  };
}

async function envFor(dataDir: string): Promise<{ rt: AuthorityRuntime; cache: GitCache; calls: HookCall[] }> {
  const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
  const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 });
  const calls: HookCall[] = [];
  const cache = new GitCache(dataDir, nullLogger(), undefined, testGitCacheOptions({
    authority: recordingHooks(rt, calls), authorityRequired: true, gitleaksBin: defaultGitleaksShim(),
  }));
  return { rt, cache, calls };
}

const calledUnderA = (calls: HookCall[], hook: string, op: string): boolean =>
  calls.some((c) => c.hook === hook && c.op === op) && calls.filter((c) => c.hook === hook && c.op === op).every((c) => c.aHeld);

// ---- the runner's sites --------------------------------------------------------------------

function fakeFlight(executor: Executor, runId: string) {
  const lifecycle = new AbortController();
  return {
    runId, executor, cancel: new AbortController(), steering: { lifecycleSignal: () => lifecycle.signal },
    owedAnnounced: { heads: new Set<string>(), archived: new Set<string>() },
    sourceLease: undefined as unknown, sourceUsePaths: undefined as Set<string> | undefined,
  };
}

interface Wait { outermost: boolean; options: { signal?: AbortSignal; timeoutMs?: number } | undefined }

/** Record the options of every OUTERMOST wait for A the runtime sees. */
function recordWaits(rt: AuthorityRuntime): Wait[] {
  const waits: Wait[] = [];
  const original = rt.withTransaction.bind(rt) as AuthorityRuntime["withTransaction"];
  mock.method(rt, "withTransaction", ((fn: never, options?: Wait["options"]) => {
    if (rt.active() === undefined) waits.push({ outermost: true, options });
    return original(fn, options);
  }) as AuthorityRuntime["withTransaction"]);
  return waits;
}

/** Every outermost wait carries the caller's signal and an explicit deadline well under the 600 s default. */
function assertBoundedWaits(waits: Wait[]): void {
  assert.ok(waits.length > 0, "the entry waited for A");
  for (const wait of waits) {
    assert.ok(wait.options?.signal instanceof AbortSignal, "the wait carries the flight's cancel/lifecycle signal");
    assert.ok(typeof wait.options?.timeoutMs === "number" && wait.options.timeoutMs > 0 && wait.options.timeoutMs <= 300_000,
      `an explicit deadline, not the default: ${String(wait.options?.timeoutMs)}`);
  }
}

describe("runner entry points that reach a mutating hook", () => {
  let fx: Fixture;
  let env: Env;
  let runner: RunRunner;
  type Internals = {
    runnerCloneForClaim(bare: string, claim: unknown, reseed: unknown, executor: Executor, attempt: unknown, coords: unknown,
      evidence: unknown, flight: unknown): Promise<{ path: string }>;
    bindFlightSource(flight: unknown, clonePath: string): Promise<void>;
    admitPredecessorSource(flight: unknown, clonePath: string, wait: object): Promise<void>;
    requireLaunchAuthority(flight: unknown): Promise<void>;
    endFlightSourceUse(flight: unknown, log: unknown): Promise<void>;
    reconcileConfirmedOwed(flight: unknown, bare: string, sha: string): Promise<void>;
    settler: { cleanup: { deleteRecoveryPin(bare: string, runId: string, generation: number): Promise<void> } };
  };
  const internals = (): Internals => runner as unknown as Internals;
  const claimFor = (iid: number) => ({ run_id: `run-${iid}`, kind: "issue", issue_iid: iid, session_id: null, secrets: {}, repo: { id: "repo-1" } });

  beforeEach(async () => {
    fx = makeFixture();
    const dataDir = fs.realpathSync(fx.dataDir);
    const base = await envFor(dataDir);
    const bare = await base.cache.ensureClone(fx.originPath);
    fs.chmodSync(path.dirname(bare), 0o700);
    fs.chmodSync(bare, 0o700);
    env = { dataDir, ...base, bare };
    runner = new RunRunner({} as unknown as WorkerClient, env.cache, () => ({ executor: {} as Executor }), nullLogger(), 20, undefined, { authority: env.rt });
  });
  afterEach(() => fx.cleanup());

  const rows: Array<{
    name: string;
    drive: (e: Env, i: () => Internals) => Promise<void>;
    expect: Array<[hook: string, op: string]>;
  }> = [
    {
      name: "runnerCloneForClaim: a canonical seed, then a reseed that frees the existing clone",
      expect: [["admitSourceUse", "seed-canonical"], ["beginDestructive", "runner-uid-delete"]],
      drive: async (e, i) => {
        const flight = fakeFlight({} as Executor, "run-3410");
        const clone = await i().runnerCloneForClaim(e.bare, claimFor(3410), noProofReseed, flight.executor, undefined, undefined, undefined, flight);
        assert.equal(fs.existsSync(path.join(clone.path, ".git")), true);
        fs.writeFileSync(path.join(clone.path, "STALE.txt"), "freed by the reseed\n");
        e.calls.length = 0;
        await i().runnerCloneForClaim(e.bare, claimFor(3410), noProofReseed, flight.executor, undefined, undefined, undefined, flight);
        assert.equal(fs.existsSync(path.join(clone.path, "STALE.txt")), false, "the reseed freed the old clone through the destructive hook");
        assert.deepEqual([...(flight.sourceUsePaths ?? [])], [clone.path], "the flight recorded the path it admitted");
      },
    },
    {
      name: "reconcileConfirmedOwed: the owed-pin deletion",
      expect: [["beginBareMutation", "owed-reconcile-delete-ref"]],
      drive: async (e, i) => {
        // The Git child is replaced; the hook plumbing it uses (GitCache.beginBare) is the real one.
        e.cache.reconcileOwedCandidates = async (bare: string) => {
          const settlement = await (e.cache as unknown as { beginBare(b: string, op: string, f: string[]): Promise<{ settle(o: { ok: true }): Promise<void> }> })
            .beginBare(bare, "owed-reconcile-delete-ref", ["refs/uzi-owed/run-3411/x"]);
          await settlement.settle({ ok: true });
          return { removedShas: [] } as never;
        };
        await i().reconcileConfirmedOwed(fakeFlight({} as Executor, "run-3411"), e.bare, "a".repeat(40));
      },
    },
    {
      name: "the predecessor settler's recovery-pin cleanup",
      expect: [["beginBareMutation", "recovery-pin-delete-ref"]],
      drive: async (e, i) => {
        const sha = execFileSync("git", ["-C", e.bare, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
        assert.equal(await e.cache.anchorRecoveryHead(e.bare, "run-3412", 5, sha), true);
        await i().settler.cleanup.deleteRecoveryPin(e.bare, "run-3412", 5);
        assert.throws(() => execFileSync("git", ["-C", e.bare, "rev-parse", "--verify", "--quiet", "refs/uzi-recovery-pin/run-3412/5"], { stdio: "pipe" }));
      },
    },
  ];

  for (const row of rows) {
    it(row.name, async () => {
      env.calls.length = 0;
      const waits = recordWaits(env.rt);
      await row.drive(env, internals);
      for (const [hook, op] of row.expect) {
        assert.ok(calledUnderA(env.calls, hook, op), `${hook}(${op}) ran, and only while A was held; calls: ${JSON.stringify(env.calls)}`);
      }
      assertBoundedWaits(waits);
    });
  }

  it("the flight-bound entries (bind, predecessor admission, launch gate, end of use) take their own A", async () => {
    const flight = fakeFlight({} as Executor, "run-3413");
    const target = env.cache.runnerClonePath(env.bare, "issue-3413");
    const predecessor = env.cache.runnerClonePath(env.bare, "issue-3414");
    fs.mkdirSync(target, { recursive: true });
    fs.mkdirSync(predecessor, { recursive: true });
    const waits = recordWaits(env.rt);
    await internals().bindFlightSource(flight, target);
    assert.ok(flight.sourceLease);
    await internals().admitPredecessorSource(flight, predecessor, { signal: new AbortController().signal, timeoutMs: 5000 });
    (flight as { runnerClone?: { path: string } }).runnerClone = { path: target };
    await internals().requireLaunchAuthority(flight);
    const beforeEnd = waits.length;
    await internals().endFlightSourceUse({ ...flight, worktreePath: target, sourceTailsJoined: true }, nullLogger());
    // The end-of-flight step must still run after a cancel, so it carries a deadline and no abort signal.
    assert.equal(waits.length - beforeEnd, 1);
    assert.equal(waits[beforeEnd]!.options?.timeoutMs, 30_000);
    assert.equal(waits[beforeEnd]!.options?.signal, undefined);
    const slots = await env.rt.withTransaction(async (tx) => {
      const store = await openAuthorityStore(tx, { workerToken: TOKEN });
      return Promise.all([target, predecessor].map(async (p) => (await store.readSlot(tx, await store.slotKey(tx, p)))?.state));
    });
    assert.deepEqual(slots, ["ineligible", "ineligible"], "both admitted paths left active at the end of use");
    assertBoundedWaits(waits.slice(0, beforeEnd));
    // No hook ran with A absent, anywhere in this test.
    assert.equal(env.calls.every((c) => c.aHeld || c.hook === "assertBareClear"), true);
  });
});

// ---- the runner's call-site inventory -------------------------------------------------------

describe("runner.ts call sites: A wraps exactly the git calls that can reach a mutating hook", () => {
  const source = fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "src", "runner.ts"), "utf8");
  const WRAPPERS = /(?:inAuthority|inFlightAuthority|seedInAuthority)\(/g;

  /** Whether the call starting at `index` lies inside the argument list of an A wrapper call. */
  function wrapped(index: number): boolean {
    for (const m of source.matchAll(WRAPPERS)) {
      const open = (m.index ?? 0) + m[0].length - 1;
      if (open > index) break;
      let depth = 0;
      for (let k = open; k < source.length; k++) {
        const ch = source[k];
        if (ch === "(") depth++;
        else if (ch === ")" && --depth === 0) { if (k > index) return true; break; }
      }
    }
    return false;
  }
  const sites = (method: string): number[] => [...source.matchAll(new RegExp(`this\\.git\\.${method}\\(`, "g"))].map((m) => m.index ?? 0);

  // Methods whose body reaches admitSourceUse, beginBareMutation or beginDestructive.
  const NEEDS_A = ["reconcileOwedCandidates", "deleteRecoveryPin", "normalizeRetainedSourceObjects", "prepareRecoverySuccessor",
    "createOrAttachRunnerClone", "runnerCloneForBranch"];
  // Methods whose only authority hook is the transaction-free assertBareClear.
  const NO_A = ["fetchAgentBranch", "updateTrackingRef", "retainCurrentOwedCandidate", "anchorRecoveryHead"];

  for (const method of NEEDS_A) {
    it(`${method}: every call is inside an A wrapper`, () => {
      const found = sites(method);
      assert.ok(found.length > 0, "the site exists");
      for (const index of found) assert.equal(wrapped(index), true, `this.git.${method}( at offset ${index} is not inside an A wrapper`);
    });
  }
  for (const method of NO_A) {
    it(`${method}: no call holds A (assertBareClear needs none)`, () => {
      const found = sites(method);
      assert.ok(found.length > 0, "the site exists");
      for (const index of found) assert.equal(wrapped(index), false, `this.git.${method}( at offset ${index} is wrapped in A`);
    });
  }
});

// ---- the recovery coordinator's cycles ------------------------------------------------------

describe("recovery cycle wrappers that reach a mutating hook", () => {
  const RUN = "parked-run";
  const NAME = "forge.example+org+repo.git";
  const ref = (generation: number) => `refs/uzi-recovery-pin/${RUN}/${generation}`;
  const git = (cwd: string, ...args: string[]) => execFileSync("git", ["-C", cwd, ...args], {
    encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_NOSYSTEM: "1" },
  }).trim();

  class Archive implements RecoveryArchiveClient {
    uploadError?: Error = new Error("temporary upload failure");
    async reserveRecoveryCapture() { return { capture_id: "server-capture", state: "preparing" as const }; }
    async getRecoveryCaptureStatus() { return { capture_id: "server-capture", state: "preparing" as const, manifest_bound: false }; }
    async uploadRecoveryBundle(_run: string, _capture: string, _manifest: unknown, bundle: Readable) {
      for await (const _ of bundle) { /* consume the journaled bytes */ }
      if (this.uploadError) throw this.uploadError;
      return { capture_id: "server-capture", state: "available" as const, manifest_bound: true };
    }
    async releaseRecoveryCustody(): Promise<never> { throw new Error("archive upload must not release custody"); }
    async listRecoveryHolds() { return { run_id: RUN, holds: [] }; }
  }

  let root: string;
  let bare: string;
  let sha: string;
  let env: { rt: AuthorityRuntime; cache: GitCache; calls: HookCall[] };
  let archive: Archive;
  let now: number;

  const coord = () => new RecoveryCoordinator({
    authority: env.rt, client: archive, git: env.cache as RecoveryBundleProducer, log: nullLogger(),
    recoveryRoot: env.cache.recoveryRoot, workerToken: TOKEN, now: () => now,
  });
  const present = (generation: number): boolean => {
    try { return git(bare, "rev-parse", "--verify", ref(generation)) === sha; } catch { return false; }
  };

  beforeEach(async () => {
    root = makeScratchRoot("authority-inventory-");
    const dataDir = path.join(root, "data");
    await fsp.mkdir(dataDir, { recursive: true });
    env = await envFor(fs.realpathSync(dataDir));
    bare = path.join(dataDir, "repos", NAME);
    await fsp.mkdir(bare, { recursive: true });
    git(bare, "init", "--bare", "-b", "main");
    const work = path.join(root, "work");
    await fsp.mkdir(work);
    git(work, "init", "-b", "main");
    git(work, "config", "user.name", "fixture");
    git(work, "config", "user.email", "fixture@example.invalid");
    git(work, "config", "commit.gpgsign", "false");
    await fsp.writeFile(path.join(work, "work.txt"), "parked work\n");
    git(work, "add", "work.txt");
    git(work, "commit", "-m", "park");
    sha = git(work, "rev-parse", "HEAD");
    git(work, "push", bare, "HEAD:refs/uzi-runner/agent/park");
    mock.method(env.cache, "fetchDefaultTip", async () => { throw new Error("no forge credentials"); });
    archive = new Archive();
    now = 100_000;
  });
  afterEach(async () => {
    mock.restoreAll();
    await fsp.rm(root, { recursive: true, force: true });
  });

  /** A park-shaped record whose upload failed once: bundled, with its pin and a sibling generation's pin. */
  async function bundled() {
    assert.equal(await env.cache.anchorRecoveryHead(bare, RUN, 4, sha), true);
    assert.equal(await env.cache.anchorRecoveryHead(bare, RUN, 9, sha), true);
    const c = coord();
    const record = (await c.pin({ runId: RUN, sourceSha: sha, kind: "issue", branch: "agent/park", generation: 4 }))!;
    const outcome = await c.captureAndUpload({ record, barePath: bare, defaultBranch: "main" });
    assert.equal(outcome.state, "needs_action");
    assert.ok(present(4));
    archive.uploadError = undefined;
    env.calls.length = 0;
    return { c, record };
  }
  const pinDeletedUnderA = (): void => {
    assert.equal(present(4), false, "the archived pin was deleted: its authority hook was admitted");
    assert.ok(calledUnderA(env.calls, "beginBareMutation", "recovery-pin-archive-delete-ref"),
      `the archived-pin deletion registered its footprint while A was held; calls: ${JSON.stringify(env.calls)}`);
  };

  it("captureAndUpload: the upload and the archived-pin deletion run inside one A", async () => {
    const { c, record } = await bundled();
    const outcome = await c.captureAndUpload({ record, barePath: bare, defaultBranch: "main" });
    assert.equal(outcome.state, "uploaded");
    pinDeletedUnderA();
  });

  it("resumePending (boot): the pending-step cycle and the boot pin cleanup run inside A", async () => {
    const { c } = await bundled();
    now += 1_000_000;
    await c.resumePending();
    pinDeletedUnderA();
  });

  it("resumeLive: the live pass runs inside A", async () => {
    const { c } = await bundled();
    now += 1_000_000;
    await c.resumeLive({ authenticatedAtMs: now + 1, isExecuting: () => false });
    pinDeletedUnderA();
  });

  // Entries whose cycle body needs a full record: here only the order is observable. The scope is
  // instrumented, the cycle lock is observed, and the body is allowed to fail on the bare record.
  describe("A is entered before the capture-cycle lock", () => {
    const scopeOf = (events: string[]): AuthorityScope => ({
      withTransaction: async (fn) => {
        events.push("A:enter");
        try { return await fn(undefined as never); } finally { events.push("A:exit"); }
      },
    });
    const record = { runId: "run-1", captureId: "11111111-1111-4111-8111-111111111111", generation: 7, inventoryGuarded: true } as never;
    const entries: Array<{ name: string; enters: boolean; call: (c: RecoveryCoordinator) => Promise<unknown> }> = [
      { name: "forgetGeneration", enters: true, call: (c) => c.forgetGeneration("run-1", 7) },
      { name: "captureAndUpload", enters: true, call: (c) => c.captureAndUpload({ record, barePath: "/nonexistent", defaultBranch: "main" } as never) },
      { name: "operateFrozenInventory", enters: true, call: (c) => c.operateFrozenInventory({ record, barePath: "/nonexistent", defaultBranch: "main" } as never) },
      { name: "releaseHeldPublication", enters: true, call: (c) => c.releaseHeldPublication(record, "22222222-2222-4222-8222-222222222222") },
      // freezeInventory's only hook is the transaction-free assertBareClear: it must NOT take A.
      { name: "freezeInventory", enters: false, call: (c) => c.freezeInventory({ context: { runId: "run-1", generation: 7 }, currentSha: "a".repeat(40), defaultBranch: "main" } as never) },
    ];
    for (const entry of entries) {
      it(`${entry.name}: ${entry.enters ? "A:enter precedes the cycle key" : "takes no A"}`, async (t) => {
        const events: string[] = [];
        const rootDir = makeScratchRoot("authority-order-");
        t.after(() => fs.rmSync(rootDir, { recursive: true, force: true }));
        const c = new RecoveryCoordinator({
          authority: scopeOf(events), client: {} as RecoveryArchiveClient, git: {} as RecoveryBundleProducer, log: nullLogger(),
          recoveryRoot: path.join(rootDir, "recovery"), workerToken: TOKEN,
        });
        const internals = c as unknown as { cycleKey: (...a: unknown[]) => Promise<string> };
        const cycleKey = internals.cycleKey.bind(c);
        internals.cycleKey = async (...a) => { events.push("C"); return cycleKey(...a); };
        await entry.call(c).catch(() => undefined);
        if (entry.enters) {
          assert.equal(events[0], "A:enter", `events: ${events.join(",")}`);
          assert.ok(events.indexOf("C") > events.indexOf("A:enter"), `A before C; events: ${events.join(",")}`);
        } else {
          assert.equal(events.includes("A:enter"), false, `events: ${events.join(",")}`);
        }
      });
    }
  });

  it("a cycle under a maintenance budget waits for A no longer than that budget, and ends with its signal", async (t) => {
    const seen: Array<{ signal?: AbortSignal; timeoutMs?: number } | undefined> = [];
    const rootDir = makeScratchRoot("authority-wait-");
    t.after(() => fs.rmSync(rootDir, { recursive: true, force: true }));
    const c = new RecoveryCoordinator({
      authority: { withTransaction: async (fn, options) => { seen.push(options); return fn(undefined as never); } },
      client: {} as RecoveryArchiveClient, git: {} as RecoveryBundleProducer, log: nullLogger(),
      recoveryRoot: path.join(rootDir, "recovery"), workerToken: TOKEN,
    });
    await MaintenanceBudget.run(new MaintenanceBudget(5_000), () => c.forgetGeneration("run-1", 7)).catch(() => undefined);
    assert.equal(seen.length, 1);
    assert.ok(typeof seen[0]?.timeoutMs === "number" && seen[0].timeoutMs > 0 && seen[0].timeoutMs <= 5_000, String(seen[0]?.timeoutMs));
    assert.ok(seen[0]?.signal instanceof AbortSignal);
  });

  it("an AuthorityRefusal from a missing wrap would leave the pin in place (guard for the rows above)", async () => {
    const { c, record } = await bundled();
    // The same coordinator without an authority scope: the hook refuses (no active transaction), the pin stays.
    const bare2 = new RecoveryCoordinator({ client: archive, git: env.cache as RecoveryBundleProducer, log: nullLogger(),
      recoveryRoot: env.cache.recoveryRoot, workerToken: TOKEN, now: () => now });
    void c;
    const outcome = await bare2.captureAndUpload({ record, barePath: bare, defaultBranch: "main" });
    assert.equal(outcome.state, "uploaded");
    assert.equal(present(4), true, "unwrapped, the authority refused the deletion");
    assert.equal(env.calls.some((x) => x.hook === "beginBareMutation" && !x.aHeld), true);
    assert.ok(AuthorityRefusal);
  });
});
