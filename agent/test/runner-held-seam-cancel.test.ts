import { after, afterEach, before, describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { createHash, randomUUID } from "node:crypto";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import { GitCache, type AttemptSeedOptions } from "../src/git.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { acquireWorkerLifetime, AuthorityShutdownRefusal, SourceUseHeldRefusal } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { Outbox } from "../src/outbox.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { FakeRecoveryClient } from "./codex-reap-fixture.js";
import { LiveAttemptRegistry, mintAttemptId, setQuiescenceViewForTests, type QuiesceRunOutcome, type QuiesceRunRequest } from "../src/run-quiescence.js";
import { testLockOptions } from "./authority-test-support.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { HERMETIC_VIEW, restoreHermeticView } from "./setup/hermetic-proc.js";
import { nullLogger, recordingLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, input, installHarness } from "./runner-harness.js";

// issue #3860: a claim that stops at a held-predecessor refusal must still honour an owner cancel
// that the server reports as requested (ownership `cancellation_requested`), but only when no other
// outcome of the run can be racing it. The seam is read-only: it never reads the inputs (an older api
// consumes them on read), so an api that does not serve the field leaves the cancel pending.

installHarness();

const HAS_PROCFS = process.platform === "linux";
const TOKEN = "held-seam-" + randomUUID();

before(() => setQuiescenceViewForTests(HERMETIC_VIEW));
after(() => restoreHermeticView());

const tmpDirs: string[] = [];
afterEach(() => {
  for (const d of tmpDirs.splice(0)) fs.rmSync(d, { recursive: true, force: true });
});

// ─── helpers (adapted from clone-attempt-paths.test.ts) ───────────────────────────────────────

const neverQuiesce = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
  throw new Error(`the capture must not start (${req.site})`);
};

function treeHash(root: string): string {
  const h = createHash("sha256");
  const walk = (dir: string): void => {
    for (const name of fs.readdirSync(dir).sort()) {
      const p = path.join(dir, name);
      const st = fs.lstatSync(p);
      h.update(`${path.relative(root, p)}\0${st.isDirectory() ? "d" : st.isSymbolicLink() ? "l" : "f"}\0`);
      if (st.isDirectory()) walk(p);
      else if (st.isFile()) h.update(fs.readFileSync(p));
    }
  };
  walk(root);
  return h.digest("hex");
}

function fixtureSeed(attemptId: string): AttemptSeedOptions {
  return { attemptId, isLive: () => false, beforeSeed: async () => {} };
}

async function seedPredecessor(iid: number, runId: string) {
  const b = await git.ensureClone(fx.originPath);
  const attemptId = mintAttemptId(1);
  const clone = await git.createOrAttachRunnerClone(b, iid, noProofReseed, runId, false, undefined, fixtureSeed(attemptId));
  fs.writeFileSync(path.join(clone.path, "ONLY_COPY.txt"), "must survive recovery\n");
  await git.markRecoveryCapture(b, clone.path, `agent/issue-${iid}`, runId, clone.attemptId);
  api.setOwnershipStatus(runId, "running", 2);
  return { clonePath: clone.path, attemptId };
}

function transientFactory(): { factory: ExecutorFactory; started: () => number } {
  let started = 0;
  const factory: ExecutorFactory = runId => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (_ctx: RunContext): Promise<ExecutorResult> => {
        started++;
        throw new Error("stop after the seed");
      },
    },
  });
  return { factory, started: () => started };
}

async function mkOutbox(): Promise<Outbox> {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "held-seam-outbox-"));
  tmpDirs.push(dir);
  const outbox = new Outbox({
    root: path.join(dir, "outbox"), log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000,
  });
  await outbox.init();
  for (let page = 0; page < 8; page++) if ((await outbox.scanTerminalObservationsPage()).passComplete) break;
  assert.equal(outbox.policyInventoryCoverageAvailable(), true, "fixture: the inventory pass completed");
  return outbox;
}

async function runtime(): Promise<AuthorityRuntime> {
  const dataDir = fs.realpathSync(fx.dataDir);
  const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
  return new AuthorityRuntime({ lifetime, workerToken: TOKEN, workerIncarnation: randomUUID(), timeoutMs: 5000 });
}

type Priv = any;

interface Owner {
  status: string; generation: number | undefined; released: boolean; armed: boolean;
  /** Served as `cancellation_requested` once the seam is reached; undefined omits the field (an older api). */
  requested: unknown;
  /** When set, the ownership answer waits for it, holding the seam's read in flight. */
  gate: Promise<void> | undefined;
}

interface RigOptions {
  site: "A" | "B";
  iid: number;
  generation?: number;
  outbox?: "ok" | "none" | "disabled";
  /** Called inside the seam, after the observable pre-state is captured and before the helper runs. */
  pre?: (rig: Rig, flight: Priv, claim: Priv) => void | Promise<void>;
  /** An owner cancel is persisted: a pending cancel row, and the server reports it as requested. */
  cancel?: boolean;
  /** Override what the server serves as `cancellation_requested` (default: true with `cancel`, else omitted). */
  requested?: unknown;
  /** Ownership answer once the seam is reached (default: running at the claim generation). */
  owner?: Partial<Pick<Owner, "status" | "generation" | "released">>;
  runnerOpts?: Record<string, unknown>;
  /** Run with a token-keyed (enabled) recovery coordinator and an inventory-guarded claim. */
  recoveryEnabled?: boolean;
}

interface Rig {
  t: TestContext;
  runner: RunRunner;
  runId: string;
  iid: number;
  generation: number;
  pred: { clonePath: string; attemptId: string };
  before: string;
  started: () => number;
  outbox: Outbox | undefined;
  owner: Owner;
  lines: unknown[];
  seamCalls: () => number;
  getsAtSeam: number | undefined;
  ownAtSeam: number | undefined;
  /** Ownership reads the seam itself issued (after it was reached). */
  ownReads: () => number;
  flight: Priv | undefined;
  reserve: { mock: { callCount(): number } };
  current: AuthorityRuntime | undefined;
  earlier: Set<string> | undefined;
  claim: (generation?: number) => ReturnType<typeof gitlabClaim>;
  run: (generation?: number) => Promise<void>;
  gets: () => number;
  failed: () => Array<{ runId: string; body: Record<string, unknown> }>;
  /** Hold the seam's ownership read in flight until the returned release is called. */
  holdOwnership: () => () => void;
}

async function rig(t: TestContext, o: RigOptions): Promise<Rig> {
  const generation = o.generation ?? 2;
  const runId = randomUUID();
  const pred = await seedPredecessor(o.iid, runId);
  const before = treeHash(pred.clonePath);
  const { factory, started } = transientFactory();
  const outbox = o.outbox === "none" ? undefined : await mkOutbox();
  const { logger, lines } = recordingLogger();
  const { gitlab } = fakeGitlab();
  const restartedGit = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
  let current: AuthorityRuntime | undefined;
  let earlier: Set<string> | undefined;
  if (o.site === "A") {
    current = await runtime();
    earlier = new Set<string>();
    await current.recordAdmissions(earlier, () => current!.bindSource(pred.clonePath));
    current.retireSink(earlier);
  }
  const recovery = o.recoveryEnabled
    ? new RecoveryCoordinator({
      ...(current ? { authority: current } : {}),
      client: new FakeRecoveryClient(), git: restartedGit, log: nullLogger(),
      recoveryRoot: restartedGit.recoveryRoot, workerToken: TOKEN,
    })
    : undefined;
  const runner = new RunRunner(client, restartedGit, factory, logger, 20, undefined, {
    pollMs: 1_000, // the steering poll never beats the seam
    planApprovalTimeoutMs: 0,
    questionTimeoutMs: 600,
    gitlab,
    quiesceRun: neverQuiesce,
    liveAttempts: new LiveAttemptRegistry(),
    recoveryRetryMs: 5,
    heldSeamCancelObserveMs: 2_000,
    ...(current ? { authority: current } : {}),
    ...(outbox ? { outbox } : {}),
    ...(recovery ? { recovery } : {}),
    ...o.runnerOpts,
  } as ConstructorParameters<typeof RunRunner>[6]);
  const reserve = t.mock.method(restartedGit, "reserveRecoveryIteration");
  if (o.site === "B") {
    t.mock.method(runner as Priv, "admitPredecessorSource", async () => {
      throw new SourceUseHeldRefusal("held_unended (fixture)");
    });
  }
  const owner: Owner = { status: "running", generation, released: false, armed: false, requested: "requested" in o ? o.requested : (o.cancel ? true : undefined), gate: undefined };
  api.ownershipWire.set(runId, res => {
    const armed = owner.armed;
    const answer = (): void => {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({
        status: armed ? owner.status : "running",
        claim_released: armed ? owner.released : false,
        ...(armed ? (owner.generation !== undefined ? { claim_generation: owner.generation } : {}) : { claim_generation: generation }),
        ...(armed && owner.requested !== undefined ? { cancellation_requested: owner.requested } : {}),
      }));
    };
    if (armed && owner.gate) void owner.gate.then(answer); else answer();
  });
  if (o.owner) Object.assign(owner, o.owner);
  const r = { t, runner, runId, iid: o.iid, generation, pred, before, started, outbox, owner, lines, getsAtSeam: undefined,
    flight: undefined, reserve, current, earlier } as unknown as Rig;
  const orig = (runner as Priv).observeCancelAtHeldSeam.bind(runner) as (c: unknown, f: unknown) => Promise<void>;
  const seam = t.mock.method(runner as Priv, "observeCancelAtHeldSeam", async (claim: Priv, flight: Priv) => {
    r.flight = flight;
    owner.armed = true;
    // The cancel lands after steering's first read, so only the seam can observe it.
    // The cancel row is persisted but only the ownership field can surface it to the seam.
    if (o.cancel) api.setInputs(runId, [input("cancel")]);
    r.ownAtSeam = api.ownershipRequests.filter(id => id === runId).length;
    if (outbox && o.outbox === "disabled") (outbox as Priv).disabled = true;
    await o.pre?.(r, flight, claim);
    r.getsAtSeam = r.gets();
    return orig(claim, flight);
  });
  r.seamCalls = () => seam.mock.callCount();
  r.claim = (g = generation) => gitlabClaim(o.iid, {
    run_id: runId, claim_generation: g, ...(o.recoveryEnabled ? { inventory_guarded: true } : {}),
  });
  r.run = async (g = generation) => { api.setOwnershipStatus(runId, "running", g); await runner.execute(r.claim(g)); };
  r.gets = () => api.inputGets.get(runId) ?? 0;
  r.ownReads = () => api.ownershipRequests.filter(id => id === runId).length - (r.ownAtSeam ?? 0);
  r.holdOwnership = () => {
    let release!: () => void;
    owner.gate = new Promise<void>(resolve => { release = resolve; });
    return release;
  };
  r.failed = () => api.states.filter(s => s.runId === runId && s.body.status === "failed") as unknown as Array<{ runId: string; body: Record<string, unknown> }>;
  return r;
}

function assertReachedSeam(r: Rig): void {
  assert.equal(r.seamCalls() >= 1, true, "the claim reached the held-predecessor refusal");
}

/** The shared "nothing happened" evidence: custody kept, nothing started, no outcome reported. */
function assertPlainStop(r: Rig, why: string): void {
  assertReachedSeam(r);
  assert.equal(r.started(), 0, `${why}: no executor`);
  assert.equal(r.failed().length, 0, `${why}: no failed report`);
  assert.equal(r.flight.steering.isCancelled(), false, `${why}: cancel not committed`);
  assert.equal(r.gets(), r.getsAtSeam, `${why}: the seam never reads inputs`);
  assert.equal(treeHash(r.pred.clonePath), r.before, `${why}: custody and clone untouched`);
}

function journaledGenerations(r: Rig): unknown[] {
  return r.lines
    .filter(l => (l as { msg?: string }).msg === "outbox: terminal journaled write-ahead")
    .map(l => (l as { claim_generation?: number }).claim_generation);
}

function assertCancelledOnce(r: Rig, generation = r.generation): void {
  assertReachedSeam(r);
  const failed = r.failed();
  assert.equal(failed.length, 1, "exactly one failed report");
  assert.deepEqual(journaledGenerations(r), [generation], "the one terminal outcome is journaled at the claim generation");
  assert.equal(r.started(), 0, "no executor started");
  assert.equal(r.gets(), r.getsAtSeam, "the seam never reads inputs");
  assert.equal(treeHash(r.pred.clonePath), r.before, "custody and the clone are retained untouched");
  assert.equal(r.flight.steering.isCancelled(), true);
  assert.equal(r.lines.filter(l => (l as { msg?: string }).msg ===
    "retained predecessor held; owner cancel observed at the refusal, reporting cancellation").length, 1, "the held seam's own cancel line");
}

/** The journal is either gone or marked settled by the server (nothing is left to resend). */
function assertSettledOrRetired(r: Rig): void {
  const left = r.outbox!.listPendingTerminals().filter(p => p.run_id === r.runId);
  assert.equal(left.every(p => p.server_settled === true), true, "no unsettled journal remains");
}

function journalFailed(outbox: Outbox, runId: string, generation: number): Promise<unknown> {
  return outbox.journalTerminal(runId, generation, "running", 0,
    { status: "failed", failure_reason: "earlier outcome", claim_generation: generation });
}

// ─── the cases ────────────────────────────────────────────────────────────────────────────────

describe("issue #3860: owner cancel at a held-predecessor refusal", { skip: !HAS_PROCFS }, () => {
  for (const site of ["A", "B"] as const) {
    for (const status of ["running", "claimed"]) {
      it(`site ${site}: a persisted cancel is reported once (ownership ${status})`, async t => {
        const r = await rig(t, { site, iid: site === "A" ? 3860 : 3861, cancel: true, outbox: "ok", owner: { status } });
        await r.run();
        assertCancelledOnce(r);
        if (site === "A") assert.equal(r.reserve.mock.callCount(), 0, "a held source never charges a recovery iteration");
        else assert.ok(r.reserve.mock.callCount() > 0, "the precharged reservation is kept");
      });
    }
  }

  it("recovery enabled with an inventory-guarded claim: the cancel report keeps custody (no reap, no settle, source kept)", async t => {
    const r = await rig(t, {
      site: "A", iid: 3855, cancel: true, outbox: "ok", recoveryEnabled: true,
      pre: async (rg, f) => {
        // A real clone's bare, as the retained-source seam holds it (0700 dirs, 0600 config).
        const bare = await git.ensureClone(fx.originPath);
        fs.chmodSync(path.dirname(bare), 0o700);
        fs.chmodSync(bare, 0o700);
        fs.chmodSync(path.join(bare, "config"), 0o600);
        f.barePath = bare;
        assert.equal((rg.runner as Priv).recovery.enabled, true, "fixture: recovery is enabled");
      },
    });
    const reap = t.mock.method(r.runner as Priv, "reapRecoveryProviderForSettle");
    const settle = t.mock.method(r.runner as Priv, "settleRecoveryGeneration");
    const dispose = t.mock.method(r.runner as Priv, "disposeCredentialFreeOwnerCancel");
    await r.run();
    assertCancelledOnce(r);
    assert.equal(r.flight.retainedLocalCustody, true, "custody is retained before the seam");
    assert.equal(reap.mock.callCount(), 0, "no provider reap");
    assert.equal(settle.mock.callCount(), 0, "the recovery generation is not settled");
    assert.equal(dispose.mock.callCount(), 0, "no credential-free owner-cancel disposal");
  });

  it("a cancel the server starts reporting while the read is in flight is honoured", async t => {
    const r = await rig(t, { site: "A", iid: 3862, outbox: "ok", requested: undefined });
    const release = r.holdOwnership();
    const done = r.run();
    while (r.flight === undefined || r.ownReads() < 1) await new Promise(res => setTimeout(res, 5));
    api.setInputs(r.runId, [input("cancel")]);
    r.owner.requested = true;
    release();
    await done;
    assertCancelledOnce(r);
  });

  it("an api that does not serve the field (older pod): a plain stop, zero input reads, the cancel stays pending", async t => {
    const r = await rig(t, { site: "A", iid: 3863, outbox: "ok", cancel: true, requested: undefined });
    await r.run();
    assertPlainStop(r, "field absent");
    assert.equal(r.ownReads() >= 1, true, "the seam asked the server");
    assert.equal(api.pendingInputRows(r.runId).some(row => row.kind === "cancel"), true, "the persisted cancel was not consumed");
  });

  it("the field false: a plain stop", async t => {
    const r = await rig(t, { site: "A", iid: 3864, outbox: "ok", requested: false });
    await r.run();
    assertPlainStop(r, "field false");
  });

  it("exactly one bounded ownership read at the seam, no retry", async t => {
    const r = await rig(t, { site: "A", iid: 3865, outbox: "ok", requested: false });
    await r.run();
    assertPlainStop(r, "one read");
    assert.equal(r.ownReads(), 1);
  });

  describe("unsafe before the read: no input read at all", () => {
    const unsafe: Array<[string, Partial<RigOptions> & { setup?: (r: Rig, flight: Priv) => void | Promise<void> }]> = [
      ["pending terminal journal at the current generation", { outbox: "ok", setup: (r) => journalFailed(r.outbox!, r.runId, r.generation) as Promise<void> }],
      ["pending terminal journal at an earlier generation", { outbox: "ok", setup: (r) => journalFailed(r.outbox!, r.runId, r.generation - 1) as Promise<void> }],
      ["pending finalize journal at an earlier generation", { outbox: "ok", setup: async (r) => { await r.outbox!.journalFinalize(r.runId, r.generation - 1); } }],
      ["outbox disabled", { outbox: "disabled" }],
      ["outbox absent", { outbox: "none" }],
      ["policy inventory coverage unavailable", { outbox: "ok", setup: r => { r.t.mock.method(r.outbox!, "policyInventoryCoverageAvailable", () => false); } }],
      ["policy outcome pending", { outbox: "ok", setup: r => { r.t.mock.method(r.outbox!, "hasPendingPolicyOutcome", async () => true); } }],
      ["iteration park entered", { outbox: "ok", setup: (_r, f) => { f.iterationParkState = "entered"; } }],
      ["completion hold unresolved", { outbox: "ok", setup: (_r, f) => { f.completionHoldOutcome = "unresolved"; } }],
      ["policy hold", { outbox: "ok", setup: (_r, f) => { f.policyHold = { outcomeId: "x" }; } }],
      ["terminal resolve held", { outbox: "ok", setup: r => { r.outbox!.holdTerminalResolve(r.runId, r.generation); } }],
    ];
    for (const [name, c] of unsafe) {
      it(name, async t => {
        const r = await rig(t, {
          site: "A", iid: 3870 + unsafe.findIndex(u => u[0] === name), cancel: true, outbox: c.outbox,
          pre: async (rg, f) => { await c.setup?.(rg, f); },
        });
        await r.run();
        assertPlainStop(r, name);
        assert.equal(r.ownReads(), 0, "no ownership read at all");
      });
    }
  });

  it("an unresolved predecessor tail: no read, no cancel", async t => {
    let original: Promise<void> | undefined;
    const r = await rig(t, {
      site: "A", iid: 3891, cancel: true, outbox: "ok",
      pre: rg => {
        const m = (rg.runner as Priv).executionTails as Map<string, Promise<void>>;
        original = m.get(rg.runId);
        m.set(rg.runId, new Promise(() => {}));
      },
    });
    await r.run();
    const m = (r.runner as Priv).executionTails as Map<string, Promise<void>>;
    if (original) m.set(r.runId, original);
    assertPlainStop(r, "tail replaced");
    assert.equal(r.ownReads(), 0, "no ownership read at all");
  });

  it("this flight's own execution tail is registered: the cancel is honoured", async t => {
    let seen: { registered: unknown; own: unknown } | undefined;
    const r = await rig(t, {
      site: "A", iid: 3892, cancel: true, outbox: "ok",
      pre: (rg, f) => { seen = { registered: (rg.runner as Priv).executionTails.get(rg.runId), own: f.executionTail }; },
    });
    await r.run();
    assert.ok(seen?.own !== undefined && seen.registered === seen.own, "fixture: the registered tail is this flight's");
    assertCancelledOnce(r);
  });

  describe("the safety set fails while the read is in flight", () => {
    const mid: Array<[string, (r: Rig, f: Priv) => void | Promise<void>]> = [
      ["an earlier-generation terminal journal", (r) => journalFailed(r.outbox!, r.runId, r.generation - 1) as Promise<void>],
      ["a current-generation terminal journal", (r) => journalFailed(r.outbox!, r.runId, r.generation) as Promise<void>],
      ["a pending finalize journal", async (r) => { await r.outbox!.journalFinalize(r.runId, r.generation - 1); }],
      ["a claim fence", (_r, f) => { (f.steering as Priv).fence = "fenced"; }],
      ["a worker shutdown", (_r, f) => { f.active = { shuttingDown: true }; }],
      ["a policy hold", (_r, f) => { f.policyHold = { outcomeId: "x" }; }],
      ["a pending policy outcome", r => { r.t.mock.method(r.outbox!, "hasPendingPolicyOutcome", async () => true); }],
      ["a completion send attempt", (_r, f) => { f.completionSendAttempted = true; }],
      ["the terminal lifecycle ending", (_r, f) => { (f.steering as Priv).terminalLifecycle.abort(); }],
    ];
    for (const [name, mutate] of mid) {
      it(name, async t => {
        const r = await rig(t, { site: "A", iid: 3900 + mid.findIndex(m => m[0] === name), cancel: true, outbox: "ok" });
        const release = r.holdOwnership();
        const done = r.run();
        while (r.flight === undefined || r.ownReads() < 1) await new Promise(res => setTimeout(res, 5));
        await mutate(r, r.flight);
        release();
        await done;
        assertPlainStop(r, name);
      });
    }
  });

  it("a fully joined predecessor: the next generation's claim reaches the refusal and is cancelled once", async t => {
    const r = await rig(t, { site: "A", iid: 3910, outbox: "ok" });
    await r.run(2);
    assertPlainStop(r, "first execution");
    api.setInputs(r.runId, [input("cancel")]);
    r.owner.generation = 3;
    r.owner.requested = true;
    await r.run(3);
    const failed = r.failed();
    assert.equal(failed.length, 1);
    assert.deepEqual(journaledGenerations(r), [3]);
    assert.equal(r.started(), 0);
    assert.equal(treeHash(r.pred.clonePath), r.before);
  });

  describe("report delivery", () => {
    it("a failed send leaves the journal pending with custody kept; a later resolve delivers it", async t => {
      const r = await rig(t, { site: "A", iid: 3920, cancel: true, outbox: "ok", pre: () => { api.failStateNext(1000); } });
      await r.run();
      assert.equal(r.failed().length, 0, "no report landed");
      assert.equal(r.outbox!.hasPendingTerminal(r.runId, r.generation), true, "journaled write-ahead");
      assert.deepEqual(journaledGenerations(r), [r.generation]);
      assert.equal(treeHash(r.pred.clonePath), r.before);
      api.failStateNext(0);
      await (r.runner as Priv).resolveRunPendingTerminals(r.runId);
      assert.equal(r.failed().length, 1);
      assertSettledOrRetired(r);
    });

    it("a lost acknowledgement: the resend meets an already-terminal run and retires the journal", async t => {
      const r = await rig(t, { site: "A", iid: 3921, cancel: true, outbox: "ok" });
      const stopDropping = api.dropStatesWhen(r.runId, b => b.status === "failed");
      await r.run();
      assert.equal(r.outbox!.hasPendingTerminal(r.runId, r.generation), true);
      stopDropping();
      api.markAlreadyTerminal(r.runId);
      await (r.runner as Priv).resolveRunPendingTerminals(r.runId);
      assertSettledOrRetired(r);
    });

    it("a stale_claim reply ends quietly and retires the journal", async t => {
      const r = await rig(t, { site: "A", iid: 3922, cancel: true, outbox: "ok" });
      api.failStateWhen(r.runId, b => b.status === "failed", { httpStatus: 409, runStatus: "running", disposition: "stale_claim" });
      await r.run();
      assert.equal(r.failed().length, 0, "the stale report was not applied");
      assert.equal(r.outbox!.hasPendingTerminal(r.runId, r.generation), false, "stale-retired");
      assert.equal(r.started(), 0);
      assert.equal(treeHash(r.pred.clonePath), r.before);
    });
  });

  describe("ownership does not confirm this claim", () => {
    const cases: Array<[string, Partial<Owner>]> = [
      ["a later generation owns the run", { generation: 3 }],
      ["the claim is released", { released: true }],
      ["the run is not running or claimed", { status: "paused" }],
      ["the generation is unknown", { generation: undefined }],
    ];
    for (const [name, owner] of cases) {
      it(name, async t => {
        const r = await rig(t, { site: "A", iid: 3930 + cases.findIndex(c => c[0] === name), cancel: true, outbox: "ok", owner });
        await r.run();
        assertPlainStop(r, name);
      });
    }
  });

  it("a read that outlives its bound ends in a plain stop", async t => {
    const r = await rig(t, { site: "A", iid: 3940, cancel: true, outbox: "ok", runnerOpts: { heldSeamCancelObserveMs: 50 } });
    const release = r.holdOwnership();
    const started = Date.now();
    await r.run();
    assert.ok(Date.now() - started < 10_000, "bounded");
    release();
    assertPlainStop(r, "timeout");
  });

  it("a retained stop outside the two seam sites (fresh-claim stopOnHeldSource) never invokes the helper", async t => {
    const r = await rig(t, { site: "B", iid: 3950, outbox: "ok" });
    const freshRunId = randomUUID();
    t.mock.method(r.runner as Priv, "runnerCloneForClaim", async () => {
      api.setInputs(freshRunId, [input("cancel")]);
      api.cancellationRequested.set(freshRunId, true);
      throw new SourceUseHeldRefusal("held_unended (fixture, fresh seed)");
    });
    api.setOwnershipStatus(freshRunId, "running", 2);
    await r.runner.execute(gitlabClaim(3951, { run_id: freshRunId, claim_generation: 2 }));
    assert.equal(r.seamCalls(), 0, "the seam helper is not invoked");
    assert.equal(r.started(), 0);
    assert.equal(api.ownershipRequests.includes(freshRunId), false, "no ownership cancel read for the fresh claim");
    assert.equal(api.states.some(s => s.runId === freshRunId && s.body.status === "failed"), false, "no failed report");
    assert.ok(r.lines.some(l => String((l as { msg?: string }).msg).includes("clone path held by an earlier flight")),
      "the claim stopped through stopOnHeldSource");
  });

  it("issue #4066: a shutdown refusal at the retained predecessor admission is a nonterminal stop, never preservation_failed", async t => {
    const r = await rig(t, { site: "B", iid: 4066, outbox: "ok" });
    t.mock.method(r.runner as Priv, "admitPredecessorSource", async () => { throw new AuthorityShutdownRefusal(); });
    await r.run();
    assert.equal(r.started(), 0, "no executor");
    assert.equal(r.failed().length, 0, "no failed report (preservation_failed would be one)");
    assert.equal(r.seamCalls(), 0, "it is not the held-source seam");
    assert.ok(r.lines.some(l => String((l as { msg?: string }).msg).includes("the worker is shutting down; stopping claim for retry")),
      "the claim stopped through the shutdown branch");
    assert.equal(treeHash(r.pred.clonePath), r.before, "custody and clone untouched");
  });

  it("issue #4066: a shutdown refusal on a fresh claim's seed stops through stopOnHeldSource", async t => {
    const r = await rig(t, { site: "B", iid: 4067, outbox: "ok" });
    const freshRunId = randomUUID();
    t.mock.method(r.runner as Priv, "runnerCloneForClaim", async () => { throw new AuthorityShutdownRefusal(); });
    api.setOwnershipStatus(freshRunId, "running", 2);
    await r.runner.execute(gitlabClaim(4068, { run_id: freshRunId, claim_generation: 2 }));
    assert.equal(r.started(), 0);
    assert.equal(api.states.some(s => s.runId === freshRunId && s.body.status === "failed"), false, "no failed report");
    assert.ok(r.lines.some(l => String((l as { msg?: string }).msg).includes("authority sealed for worker shutdown; stopping claim for retry")),
      "the claim stopped through stopOnHeldSource");
  });

  it("a claim with no held predecessor: the ordinary live-poller cancel still works, without the seam", async t => {
    const { gitlab } = fakeGitlab();
    const waiter: ExecutorFactory = runId => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: (ctx: RunContext) => new Promise<ExecutorResult>((_resolve, reject) => {
          const onAbort = (): void => reject(new Error("run cancelled"));
          if (ctx.signal?.aborted) return onAbort();
          ctx.signal?.addEventListener("abort", onAbort, { once: true });
        }),
      },
    });
    const runner = new RunRunner(client, git, waiter, nullLogger(), 20, undefined, { pollMs: 50, gitlab });
    const seam = t.mock.method(runner as Priv, "observeCancelAtHeldSeam");
    const claim = gitlabClaim(3970);
    api.setInputs(claim.run_id, [input("cancel")]);
    await runner.execute(claim);
    const failed = api.states.find(s => s.runId === claim.run_id && s.body.status === "failed");
    assert.ok(failed, "the cancelled run reports failed");
    assert.match(String(failed!.body.failure_reason ?? ""), /run cancelled/);
    assert.equal(seam.mock.callCount(), 0, "the held-predecessor seam is not involved");
  });

  describe("heldSeamSyncSafe", () => {
    const flightFor = (r: Rig, tail: Promise<void>) => ({
      runId: r.runId, claimGeneration: r.generation, executionTail: tail,
      steering: { claimFence: () => undefined, terminalLifecycleSignal: () => new AbortController().signal },
      active: undefined, iterationParkState: "none", completionHoldOutcome: "none",
    });
    const mutations: Array<[string, (f: Priv) => void]> = [
      ["worker memory hold", f => { f.workerMemory = { hold: {} }; }],
      ["iteration park entered", f => { f.iterationParkState = "entered"; }],
      ["iteration park retained", f => { f.iterationParkState = "retained"; }],
      ["completion hold unresolved", f => { f.completionHoldOutcome = "unresolved"; }],
      ["policy hold", f => { f.policyHold = { outcomeId: "x" }; }],
      ["claim fenced", f => { f.steering.claimFence = () => "fenced"; }],
      ["worker shutting down", f => { f.active = { shuttingDown: true }; }],
      ["terminal lifecycle aborted", f => { f.steering.terminalLifecycleSignal = () => AbortSignal.abort(); }],
      ["terminal already resolved", f => { f.terminalResolved = true; }],
      ["no execution tail", f => { f.executionTail = undefined; }],
    ];
    it("is true for an otherwise-safe flight and false for each unsafe state", async t => {
      const r = await rig(t, { site: "A", iid: 3960, outbox: "ok" });
      const tail = new Promise<void>(() => {});
      (r.runner as Priv).executionTails.set(r.runId, tail);
      const safe = (f: Priv): boolean => (r.runner as Priv).heldSeamSyncSafe(f);
      assert.equal(safe(flightFor(r, tail)), true, "control: the baseline flight is safe");
      for (const [name, mutate] of mutations) {
        const f = flightFor(r, tail);
        mutate(f);
        assert.equal(safe(f), false, name);
      }
      assert.equal(safe({}), false, "a malformed flight is unsafe, not a throw");
    });
  });
});
