import { after, afterEach, before, describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { createHash, randomUUID } from "node:crypto";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import { GitCache, type AttemptSeedOptions } from "../src/git.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { acquireWorkerLifetime, SourceUseHeldRefusal } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { Outbox } from "../src/outbox.js";
import { LiveAttemptRegistry, mintAttemptId, setQuiescenceViewForTests, type QuiesceRunOutcome, type QuiesceRunRequest } from "../src/run-quiescence.js";
import { testLockOptions } from "./authority-test-support.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { HERMETIC_VIEW, restoreHermeticView } from "./setup/hermetic-proc.js";
import { nullLogger, recordingLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, input, installHarness } from "./runner-harness.js";

// issue #3860: a claim that stops at a held-predecessor refusal must still honour an owner cancel
// that is already persisted, but only when no other outcome of the run can be racing it.

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

interface Owner { status: string; generation: number | undefined; released: boolean; armed: boolean }

interface RigOptions {
  site: "A" | "B";
  iid: number;
  generation?: number;
  outbox?: "ok" | "none" | "disabled";
  /** Called inside the seam, after the observable pre-state is captured and before the helper runs. */
  pre?: (rig: Rig, flight: Priv, claim: Priv) => void | Promise<void>;
  /** Queue an owner cancel row before the claim. */
  cancel?: boolean;
  /** Ownership answer once the seam is reached (default: running at the claim generation). */
  owner?: Partial<Pick<Owner, "status" | "generation" | "released">>;
  runnerOpts?: Record<string, unknown>;
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
  flight: Priv | undefined;
  reserve: { mock: { callCount(): number } };
  current: AuthorityRuntime | undefined;
  earlier: Set<string> | undefined;
  claim: (generation?: number) => ReturnType<typeof gitlabClaim>;
  run: (generation?: number) => Promise<void>;
  gets: () => number;
  failed: () => Array<{ runId: string; body: Record<string, unknown> }>;
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
    ...o.runnerOpts,
  } as ConstructorParameters<typeof RunRunner>[6]);
  const reserve = t.mock.method(restartedGit, "reserveRecoveryIteration");
  if (o.site === "B") {
    t.mock.method(runner as Priv, "admitPredecessorSource", async () => {
      throw new SourceUseHeldRefusal("held_unended (fixture)");
    });
  }
  const owner: Owner = { status: "running", generation, released: false, armed: false };
  api.ownershipWire.set(runId, res => {
    const armed = owner.armed;
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({
      status: armed ? owner.status : "running",
      claim_released: armed ? owner.released : false,
      ...(armed ? (owner.generation !== undefined ? { claim_generation: owner.generation } : {}) : { claim_generation: generation }),
    }));
  });
  if (o.owner) Object.assign(owner, o.owner);
  const r = { t, runner, runId, iid: o.iid, generation, pred, before, started, outbox, owner, lines, getsAtSeam: undefined,
    flight: undefined, reserve, current, earlier } as unknown as Rig;
  const orig = (runner as Priv).observeCancelAtHeldSeam.bind(runner) as (c: unknown, f: unknown) => Promise<void>;
  const seam = t.mock.method(runner as Priv, "observeCancelAtHeldSeam", async (claim: Priv, flight: Priv) => {
    r.flight = flight;
    owner.armed = true;
    // The cancel lands after steering's first read, so only the seam can observe it.
    if (o.cancel) api.setInputs(runId, [input("cancel")]);
    if (outbox && o.outbox === "disabled") (outbox as Priv).disabled = true;
    await o.pre?.(r, flight, claim);
    r.getsAtSeam = r.gets();
    return orig(claim, flight);
  });
  r.seamCalls = () => seam.mock.callCount();
  r.claim = (g = generation) => gitlabClaim(o.iid, { run_id: runId, claim_generation: g });
  r.run = async (g = generation) => { api.setOwnershipStatus(runId, "running", g); await runner.execute(r.claim(g)); };
  r.gets = () => api.inputGets.get(runId) ?? 0;
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
  assert.equal(treeHash(r.pred.clonePath), r.before, "custody and the clone are retained untouched");
  assert.equal(r.flight.steering.isCancelled(), true);
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

  it("a cancel inserted while the read is in flight is honoured", async t => {
    const r = await rig(t, { site: "A", iid: 3862, outbox: "ok" });
    const hold = api.holdNextInputGet(r.runId, 1);
    const done = r.run();
    await hold.entered;
    api.setInputs(r.runId, [input("cancel")]);
    hold.release();
    await done;
    assertCancelledOnce(r);
  });

  it("no cancel row: a plain stop, one read, no report", async t => {
    const r = await rig(t, { site: "A", iid: 3863, outbox: "ok" });
    await r.run();
    assertPlainStop(r, "no cancel");
    assert.equal(r.gets() - r.getsAtSeam!, 1, "exactly one read, no retry");
  });

  describe("unsafe before the read: no input read at all", () => {
    const unsafe: Array<[string, Partial<RigOptions> & { setup?: (r: Rig, flight: Priv) => void | Promise<void> }]> = [
      ["pending terminal journal at the current generation", { outbox: "ok", setup: (r) => journalFailed(r.outbox!, r.runId, r.generation) as Promise<void> }],
      ["pending terminal journal at an earlier generation", { outbox: "ok", setup: (r) => journalFailed(r.outbox!, r.runId, r.generation - 1) as Promise<void> }],
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
        assert.equal(r.gets(), r.getsAtSeam, "no input GET after the seam");
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
    assert.equal(r.gets(), r.getsAtSeam);
  });

  describe("a conflicting outcome appears while the read is in flight", () => {
    it("an earlier-generation terminal journal installed during the delayed read", async t => {
      const r = await rig(t, { site: "A", iid: 3900, cancel: true, outbox: "ok" });
      const hold = api.holdNextInputGet(r.runId, 1);
      const done = r.run();
      await hold.entered;
      await journalFailed(r.outbox!, r.runId, r.generation - 1);
      hold.release();
      await done;
      assertPlainStop(r, "journal during the read");
    });

    it("an earlier-generation terminal journal installed during the ownership check", async t => {
      const r = await rig(t, { site: "A", iid: 3901, cancel: true, outbox: "ok" });
      let installed: Promise<unknown> | undefined;
      const wire = api.ownershipWire.get(r.runId)!;
      api.ownershipWire.set(r.runId, res => {
        if (r.getsAtSeam !== undefined && installed === undefined) {
          installed = journalFailed(r.outbox!, r.runId, r.generation - 1);
          void installed.then(() => wire(res));
          return;
        }
        wire(res);
      });
      await r.run();
      assert.ok(installed, "the ownership check ran after the read");
      assertPlainStop(r, "journal during the ownership check");
    });

    it("legacy mode: a conflict during the read withholds the cancel but routes the rest", async t => {
      const r = await rig(t, { site: "A", iid: 3903, outbox: "ok" });
      const routed: string[] = [];
      const hold = api.holdNextInputGet(r.runId, 1);
      const done = r.run();
      await hold.entered;
      // An older api consumes on read: the rows are gone from the server once this read is answered.
      api.legacyConsumeOnRead = true;
      api.setInputs(r.runId, [input("cancel"), { id: 2, kind: "follow_up", body: "carry on" }]);
      const steering = r.flight.steering as Priv;
      const route = steering.route.bind(steering);
      t.mock.method(steering, "route", (...a: unknown[]) => { routed.push(String(a[0])); return route(...a); });
      await journalFailed(r.outbox!, r.runId, r.generation - 1);
      hold.release();
      await done;
      assertPlainStop(r, "legacy conflict");
      assert.deepEqual(routed, ["follow_up"], "the cancel row is withheld, the other row is routed");
    });
  });

  it("a fully joined predecessor: the next generation's claim reaches the refusal and is cancelled once", async t => {
    const r = await rig(t, { site: "A", iid: 3910, outbox: "ok" });
    await r.run(2);
    assertPlainStop(r, "first execution");
    api.setInputs(r.runId, [input("cancel")]);
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
    const hold = api.holdNextInputGet(r.runId, 1);
    const started = Date.now();
    await r.run();
    assert.ok(Date.now() - started < 10_000, "bounded");
    hold.release();
    assertPlainStop(r, "timeout");
  });

  it("a retained stop outside the two seam sites (fresh-claim stopOnHeldSource) never invokes the helper", async t => {
    const r = await rig(t, { site: "B", iid: 3950, outbox: "ok" });
    const freshRunId = randomUUID();
    t.mock.method(r.runner as Priv, "runnerCloneForClaim", async () => {
      api.setInputs(freshRunId, [input("cancel")]);
      throw new SourceUseHeldRefusal("held_unended (fixture, fresh seed)");
    });
    api.setOwnershipStatus(freshRunId, "running", 2);
    await r.runner.execute(gitlabClaim(3951, { run_id: freshRunId, claim_generation: 2 }));
    assert.equal(r.seamCalls(), 0, "the seam helper is not invoked");
    assert.equal(r.started(), 0);
    assert.equal(api.states.some(s => s.runId === freshRunId && s.body.status === "failed"), false, "no failed report");
    assert.ok(r.lines.some(l => String((l as { msg?: string }).msg).includes("clone path held by an earlier flight")),
      "the claim stopped through stopOnHeldSource");
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
