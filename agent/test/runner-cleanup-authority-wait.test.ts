import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import { fileURLToPath } from "node:url";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import type { Executor } from "../src/executor.js";
import { GitCache } from "../src/git.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { RunRunner } from "../src/runner.js";
import type { WorkerClient } from "../src/client.js";
import { testLockOptions } from "./authority-test-support.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

// issue #3404 M2 rework 13 item 3 — the recovery-pin deletion at an authorized cancellation/park cleanup waits for A on
// shutdown and the independent maintenance budget only. The flight's cancel/lifecycle signals are already
// aborted during a cancellation, so waiting on them refused the deletion the cleanup exists to perform.
// Both cleanup sites are driven through their real methods over a real authority runtime and GitCache; the
// pin's physical presence is read from the bare with git itself.

const TOKEN = "cleanup-wait-" + randomBytes(12).toString("hex");
const RUN = "run-3404";
const GEN = 7;
const PIN = `refs/uzi-recovery-pin/${RUN}/${GEN}`;

const GIT_ENV: NodeJS.ProcessEnv = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
for (const k of Object.keys(GIT_ENV)) if (k.startsWith("GIT_") && k !== "GIT_CONFIG_GLOBAL" && k !== "GIT_CONFIG_SYSTEM") delete GIT_ENV[k];
const gitIn = (bare: string, ...args: string[]): string =>
  execFileSync("git", ["-C", bare, ...args], { env: GIT_ENV, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
const pinPresent = (bare: string): boolean => gitIn(bare, "for-each-ref", "--format=%(refname)", PIN) === PIN;

interface Wait { options: { signal?: AbortSignal; timeoutMs?: number } | undefined }

interface Internals {
  disposeCredentialFreeOwnerCancel(claim: unknown, flight: unknown): Promise<"released" | "retained">;
  settleRecoveryGeneration(claim: unknown, flight: unknown, runLog: unknown, signal?: AbortSignal): Promise<void>;
  shutdownSignal: AbortController;
  freeCaptureFlights: WeakMap<object, unknown>;
  runFreeCapture: unknown;
  quiesceRun: unknown;
  transferRestorePointToTrustedBare: unknown;
  pinRecoveryGeneration: unknown;
}

describe("cancellation/park cleanup waits for A without the flight's aborted signals", () => {
  let fx: Fixture;
  let rt: AuthorityRuntime;
  let cache: GitCache;
  let bare: string;
  let sha: string;
  let runner: RunRunner;
  let budget: MaintenanceBudget;
  let lifecycle: AbortController;
  let waits: Wait[];
  let released: Array<[string, number]>;
  let ack: Record<string, unknown>;
  let forgotten: Array<[string, number]>;
  let onForget: () => void;
  let fenced: object | undefined;
  let outcome: Record<string, unknown>;

  const internals = (): Internals => runner as unknown as Internals;
  const abortBudget = () => (budget as unknown as { controller: AbortController }).controller.abort(new Error("budget exhausted"));
  const sleep = (ms: number) => new Promise<void>(resolve => setTimeout(resolve, ms));

  /** The single outermost wait the cleanup recorded. */
  const onlyWait = (): Wait["options"] => {
    assert.equal(waits.length, 1, "exactly one outermost wait for A");
    return waits[0]!.options;
  };

  /**
   * Hold A in a competing transaction, start `start` so its cleanup wait blocks on A, then `abort` while it
   * is waiting. The cleanup must be refused while A is STILL held (settled before the competitor releases),
   * so a wait that ignored the aborted source would only finish after the release and delete the pin.
   */
  async function abortWhileWaitingForA<T>(start: () => Promise<T>, abort: () => void): Promise<T> {
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    let holding!: () => void;
    const held = new Promise<void>(resolve => { holding = resolve; });
    const competitor = rt.withTransaction(async () => { holding(); await gate; });
    await held;
    waits.length = 0;
    let settled = false;
    let refusedWhileHeld = false;
    let run: Promise<T> | undefined;
    try {
      run = start().finally(() => { settled = true; });
      run.catch(() => undefined);
      for (let i = 0; i < 300 && waits.length === 0; i++) await sleep(10);
      assert.equal(waits.length, 1, "the cleanup is waiting for A");
      await sleep(50);
      assert.equal(settled, false, "still blocked on the competing transaction");
      abort();
      for (let i = 0; i < 500 && !settled; i++) await sleep(10);
      refusedWhileHeld = settled;
    } finally {
      // Always free A, even when an assertion above failed, so no transaction outlives the fixture.
      release();
      await competitor;
    }
    const result = await run;
    assert.equal(refusedWhileHeld, true, "the wait was refused while A was still held");
    return result;
  }

  const claim = (generation = GEN) => ({
    run_id: RUN, kind: "issue", issue_iid: 3404, claim_generation: generation, session_id: null,
    secrets: { codex: { auth_mode: "api_key", access_token: "k", capability: "c" }, forge_pat: "p" },
    repo: { id: "repo-1", default_branch: "main", clone_url: "https://forge.example/o/r.git" },
  });

  /** A cancelled flight whose independent cancellation budget is live. */
  function cancelledFlight() {
    const cancel = new AbortController();
    const flight = {
      runId: RUN, claimGeneration: GEN, executor: { withCredentialFreeCapture: () => undefined } as unknown as Executor,
      cancel, cancellationBudget: budget, runLog: nullLogger(),
      worktreePath: path.join(fx.dataDir, "unused-worktree"), barePath: bare, trustedPublishedStart: sha,
      steering: {
        isCancelled: () => true, claimFence: () => fenced, lifecycleSignal: () => lifecycle.signal,
        markCancelled: () => undefined, abortLifecycle: () => lifecycle.abort(),
      },
      preserveRecoveryClone: false, preserveSession: false,
    };
    cancel.abort();
    lifecycle.abort();
    return flight;
  }

  beforeEach(async () => {
    fx = makeFixture();
    const dataDir = fs.realpathSync(fx.dataDir);
    const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
    rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 });
    cache = new GitCache(dataDir, nullLogger(), undefined, testGitCacheOptions({
      authority: rt.hooks, authorityRequired: true, gitleaksBin: defaultGitleaksShim(),
    }));
    bare = await cache.ensureClone(fx.originPath);
    fs.chmodSync(path.dirname(bare), 0o700);
    fs.chmodSync(bare, 0o700);
    sha = gitIn(bare, "rev-parse", "HEAD");
    assert.equal(await cache.anchorRecoveryHead(bare, RUN, GEN, sha), true);
    assert.equal(pinPresent(bare), true, "the fixture pin exists");

    budget = new MaintenanceBudget(60_000);
    lifecycle = new AbortController();
    waits = [];
    released = [];
    forgotten = [];
    fenced = undefined;
    onForget = () => undefined;
    ack = { run_id: RUN, generation: GEN, released: true, holds_released: 1, retained: false };
    outcome = { state: "uploaded", reason: "already_published", captureId: "cap-1" };
    const original = rt.withTransaction.bind(rt) as AuthorityRuntime["withTransaction"];
    mock.method(rt, "withTransaction", ((fn: never, options?: Wait["options"]) => {
      if (rt.active() === undefined) waits.push({ options });
      return original(fn, options);
    }) as AuthorityRuntime["withTransaction"]);

    const client = {
      protocolFeatures: ["recovery_release_exact_echo"],
      hasHeldPublicationV2: () => false,
      releaseRecoveryCustody: async (runId: string, generation: number) => { released.push([runId, generation]); return ack; },
    };
    const recovery = {
      enabled: true,
      forgetGeneration: async (runId: string, generation: number) => { forgotten.push([runId, generation]); onForget(); },
      captureAndUpload: async () => outcome,
    };
    runner = new RunRunner(client as unknown as WorkerClient, cache, () => ({ executor: {} as Executor }), nullLogger(), 20, undefined,
      { authority: rt, recovery: recovery as never });
    // Only the expensive collaborators are replaced. The held-capture scope stays real: the stand-in
    // registers the held scope exactly as runFreeCapture does, so assertHeldFreeCapture still guards.
    const r = internals();
    r.runFreeCapture = async (_claim: unknown, flight: unknown, _deadlineAt: number, signal: AbortSignal | undefined,
      action: (held: unknown) => Promise<unknown>) => {
      const held = { context: undefined, signal, deadlineAt: Infinity };
      r.freeCaptureFlights.set(held, flight);
      try { return { kind: "observed_empty", value: await action(held) }; } finally { r.freeCaptureFlights.delete(held); }
    };
    r.quiesceRun = async () => ({
      outcome: { process: { state: "quiescent", processes: [], killed: [], detail: "fixture" }, docker: { state: "not_wired", removed: [], detail: "fixture" } },
      blocked: false,
    });
    cache.credentialFreeCancelCleanHead = async () => sha;
    // The clean-head proof needs a real worktree; its result is the only thing the cleanup consumes.
    r.transferRestorePointToTrustedBare = async () => sha;
    r.pinRecoveryGeneration = async () => ({ sourceSha: sha });
  });

  afterEach(() => {
    budget.dispose();
    mock.restoreAll();
    fx.cleanup();
  });

  describe("disposeCredentialFreeOwnerCancel", () => {
    it("releases and deletes the recovery pin although the flight's cancel and lifecycle signals are aborted", async () => {
      const flight = cancelledFlight();
      assert.equal(flight.cancel.signal.aborted, true);
      assert.equal(lifecycle.signal.aborted, true);
      assert.equal(await internals().disposeCredentialFreeOwnerCancel(claim(), flight), "released");
      assert.deepEqual(released, [[RUN, GEN]]);
      assert.deepEqual(forgotten, [[RUN, GEN]]);
      assert.equal(pinPresent(bare), false, "the pin was deleted");
      const wait = onlyWait();
      assert.ok(wait?.signal instanceof AbortSignal);
      assert.ok(typeof wait?.timeoutMs === "number" && wait.timeoutMs > 0 && wait.timeoutMs <= 300_000);
    });

    it("the wait for A is aborted by the maintenance budget, not by the flight's cancellation", async () => {
      assert.equal(await internals().disposeCredentialFreeOwnerCancel(claim(), cancelledFlight()), "released");
      const signal = onlyWait()!.signal!;
      assert.equal(signal.aborted, false, "cancel and lifecycle are aborted yet the wait is live");
      abortBudget();
      assert.equal(signal.aborted, true);
    });

    it("the wait for A is aborted by worker shutdown", async () => {
      assert.equal(await internals().disposeCredentialFreeOwnerCancel(claim(), cancelledFlight()), "released");
      const signal = onlyWait()!.signal!;
      assert.equal(signal.aborted, false);
      internals().shutdownSignal.abort(new Error("shutdown"));
      assert.equal(signal.aborted, true);
    });

    it("the wait is clamped to the budget's remaining time, below the boundary deadline", async () => {
      budget.dispose();
      // Below the 30 s boundary default, yet long enough that the cleanup finishes under CPU contention.
      budget = new MaintenanceBudget(20_000);
      assert.equal(await internals().disposeCredentialFreeOwnerCancel(claim(), cancelledFlight()), "released");
      const timeoutMs = onlyWait()!.timeoutMs!;
      assert.ok(timeoutMs <= 20_000, `timeoutMs ${timeoutMs} is within the budget, below the boundary deadline`);
    });

    it("pre-wait guard negative: an exhausted cancellation budget retains the pin", async () => {
      abortBudget();
      assert.equal(await internals().disposeCredentialFreeOwnerCancel(claim(), cancelledFlight()), "retained");
      assert.equal(pinPresent(bare), true);
    });

    it("pre-wait guard negative: a budget that expires before the deletion retains the pin", async () => {
      onForget = abortBudget;
      assert.equal(await internals().disposeCredentialFreeOwnerCancel(claim(), cancelledFlight()), "retained");
      assert.equal(pinPresent(bare), true);
    });

    it("pre-wait guard negative: worker shutdown before the deletion retains the pin", async () => {
      onForget = () => internals().shutdownSignal.abort(new Error("shutdown"));
      assert.equal(await internals().disposeCredentialFreeOwnerCancel(claim(), cancelledFlight()), "retained");
      assert.equal(pinPresent(bare), true);
      assert.equal(onlyWait()?.signal?.aborted, true, "the wait for A was opened already aborted and refused");
    });

    it("a budget that expires while waiting for A retains the pin", async () => {
      const result = await abortWhileWaitingForA(
        () => internals().disposeCredentialFreeOwnerCancel(claim(), cancelledFlight()), abortBudget);
      assert.equal(result, "retained");
      assert.equal(pinPresent(bare), true);
    });

    it("worker shutdown while waiting for A retains the pin", async () => {
      const result = await abortWhileWaitingForA(
        () => internals().disposeCredentialFreeOwnerCancel(claim(), cancelledFlight()),
        () => internals().shutdownSignal.abort(new Error("shutdown")));
      assert.equal(result, "retained");
      assert.equal(pinPresent(bare), true);
    });

    it("a fenced claim retains the pin", async () => {
      fenced = { fenced: true };
      assert.equal(await internals().disposeCredentialFreeOwnerCancel(claim(), cancelledFlight()), "retained");
      assert.equal(pinPresent(bare), true);
      assert.deepEqual(released, [], "nothing was released");
    });

    it("a claim generation that differs from the flight's retains the pin", async () => {
      assert.equal(await internals().disposeCredentialFreeOwnerCancel(claim(GEN + 1), cancelledFlight()), "retained");
      assert.equal(pinPresent(bare), true);
      assert.deepEqual(released, []);
    });

    it("a release acknowledgement with released:false retains the pin", async () => {
      ack = { ...ack, released: false };
      assert.equal(await internals().disposeCredentialFreeOwnerCancel(claim(), cancelledFlight()), "retained");
      assert.deepEqual(released, [[RUN, GEN]], "the release was attempted");
      assert.deepEqual(forgotten, []);
      assert.equal(pinPresent(bare), true);
    });
  });

  describe("settleRecoveryGeneration (already_published)", () => {
    it("deletes the recovery pin although the flight's cancel and lifecycle signals are aborted", async () => {
      const flight = cancelledFlight();
      await internals().settleRecoveryGeneration(claim(), flight, nullLogger());
      assert.equal(pinPresent(bare), false, "the pin was deleted");
      const signal = onlyWait()!.signal!;
      assert.equal(signal.aborted, false);
      abortBudget();
      assert.equal(signal.aborted, true, "the wait follows the maintenance budget");
    });

    it("the wait for A is aborted by worker shutdown", async () => {
      await internals().settleRecoveryGeneration(claim(), cancelledFlight(), nullLogger());
      const signal = onlyWait()!.signal!;
      assert.equal(signal.aborted, false);
      internals().shutdownSignal.abort(new Error("shutdown"));
      assert.equal(signal.aborted, true);
    });

    it("the wait is clamped to the budget's remaining time, below the boundary deadline", async () => {
      budget.dispose();
      // Below the 30 s boundary default, yet long enough that the cleanup finishes under CPU contention.
      budget = new MaintenanceBudget(20_000);
      await internals().settleRecoveryGeneration(claim(), cancelledFlight(), nullLogger());
      assert.equal(pinPresent(bare), false, "the deletion still ran");
      const timeoutMs = onlyWait()!.timeoutMs!;
      assert.ok(timeoutMs <= 20_000, `timeoutMs ${timeoutMs} is within the budget, below the boundary deadline`);
    });

    it("pre-wait guard negative: an exhausted cancellation budget retains the pin", async () => {
      abortBudget();
      await internals().settleRecoveryGeneration(claim(), cancelledFlight(), nullLogger());
      assert.equal(pinPresent(bare), true);
    });

    it("a budget that expires while waiting for A retains the pin", async () => {
      await abortWhileWaitingForA(
        () => internals().settleRecoveryGeneration(claim(), cancelledFlight(), nullLogger()), abortBudget);
      assert.equal(pinPresent(bare), true);
    });

    it("worker shutdown while waiting for A retains the pin", async () => {
      await abortWhileWaitingForA(
        () => internals().settleRecoveryGeneration(claim(), cancelledFlight(), nullLogger()),
        () => internals().shutdownSignal.abort(new Error("shutdown")));
      assert.equal(pinPresent(bare), true);
    });

    it("pre-wait guard negative: worker shutdown before the deletion retains the pin", async () => {
      outcome = { state: "uploaded", reason: "already_published", captureId: "cap-1" };
      (runner as unknown as { recovery: { captureAndUpload: () => Promise<unknown> } }).recovery.captureAndUpload = async () => {
        internals().shutdownSignal.abort(new Error("shutdown"));
        return outcome;
      };
      await internals().settleRecoveryGeneration(claim(), cancelledFlight(), nullLogger());
      assert.equal(pinPresent(bare), true);
    });

    it("an outcome other than already_published leaves the pin", async () => {
      outcome = { state: "needs_action", reason: "upload_failed", captureId: "cap-2" };
      await internals().settleRecoveryGeneration(claim(), cancelledFlight(), nullLogger());
      assert.equal(pinPresent(bare), true);
      assert.equal(waits.length, 0);
    });

    it("retained local custody leaves the pin", async () => {
      const flight = { ...cancelledFlight(), retainedLocalCustody: true };
      await internals().settleRecoveryGeneration(claim(), flight, nullLogger());
      assert.equal(pinPresent(bare), true);
    });
  });
});

describe("runner.ts: which sites use the cleanup wait", () => {
  const source = fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "src", "runner.ts"), "utf8");
  /** The statement starting at `index`, up to its terminating semicolon. */
  const statement = (index: number): string => source.slice(index, source.indexOf(";\n", index));
  const starts = (needle: string): number[] => {
    const out: number[] = [];
    for (let i = source.indexOf(needle); i >= 0; i = source.indexOf(needle, i + 1)) out.push(i);
    return out;
  };

  it("exactly the two flight recovery-pin deletions use cleanupAuthorityWait", () => {
    const deletions = starts("this.inAuthority(() => this.git.deleteRecoveryPin(").map(statement);
    assert.equal(deletions.length, 2);
    assert.ok(deletions.every(s => s.includes("this.cleanupAuthorityWait(")), deletions.join("\n"));
    assert.equal(starts("this.cleanupAuthorityWait(").length, 2, "no other caller");
    assert.equal(deletions.filter(s => s.includes("flight.barePath!, flight.runId, gen)")).length, 1);
    assert.equal(deletions.filter(s => s.includes("record.sourceSha")).length, 1);
  });

  it("the owed reconcile, bind, launch, seed and memory classification waits use authorityWait", () => {
    const reconcile = starts("this.inAuthority(() => this.git.reconcileOwedCandidates(").map(statement);
    assert.equal(reconcile.length, 1);
    assert.ok(reconcile[0]!.includes("this.authorityWait(flight)"));
    assert.ok(statement(source.indexOf("this.authority!.bindSource(")).includes("this.authorityWait(flight, CLONE_AUTHORITY_WAIT_MS)"));
    assert.ok(statement(source.indexOf("this.authority.assertLaunchable(")).includes("this.authorityWait(flight, CLONE_AUTHORITY_WAIT_MS)"));
    assert.ok(statement(source.indexOf("const seedInAuthority =")).includes("this.authorityWait(flight, CLONE_AUTHORITY_WAIT_MS)"));
    const classification = starts("authority.classifySourceAdmission(").map(statement);
    assert.equal(classification.length, 1);
    assert.ok(classification[0]!.includes("this.authorityWait(flight)"), "memory classification uses the flight lifecycle wait");
    assert.equal(starts("this.authorityWait(").length, 5, "no other authorityWait caller");
  });
});
