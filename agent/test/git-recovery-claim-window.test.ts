import { test, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { GitCache, type RecoveryJournalEntry } from "../src/git.js";
import { recoveryProgress, type RecoveryBlocker } from "../src/recovery-progress.js";
import { RequestError } from "../src/client.js";
import { RunRunner } from "../src/runner.js";
import { SinkGate } from "../src/sink-gate.js";
import { nullLogger } from "./helpers.js";

const permanentBlockers = {
  capture_failed: true, source_missing: true, adoption_failed: true, clock_invalid: true,
  oversize: true, prerequisites_unavailable: true, quiescence_failed: true,
  preservation_failed: true, decoded_history_limit: true, readiness_cleanup_unconfirmed: true,
} satisfies Record<Exclude<RecoveryBlocker, "budget_exhausted">, true>;

// Exercise the production reservation and parser with persistent journal storage in memory.
// Source/lock/Git IO are stubs here; the real-Git and runner suites cover those Linux boundaries.
function fixture(t: TestContext) {
  let now = 1_000_000;
  t.mock.method(Date, "now", () => now);
  const source = { runId: "run-window", clonePath: "/tmp/run-window" };
  let journal: RecoveryJournalEntry = { ...source };
  function recreate() {
    const cache = new GitCache("/tmp/claim-window", nullLogger());
    const seam = cache as unknown as {
      withLock: (bare: string, action: () => Promise<unknown>) => Promise<unknown>;
      checkedRecovery: () => Promise<RecoveryJournalEntry>;
      writeRecovery: (bare: string, branch: string, value: RecoveryJournalEntry) => Promise<void>;
      runGit: () => Promise<string>;
    };
    seam.withLock = async (_bare, action) => action();
    seam.checkedRecovery = async () => {
      if (journal.recovery) recoveryProgress(journal.recovery);
      return structuredClone(journal);
    };
    seam.writeRecovery = async (_bare, _branch, value) => {
      if (value.recovery) recoveryProgress(value.recovery);
      journal = structuredClone(value);
    };
    seam.runGit = async () => "";
    return cache;
  }
  const reserve = (gen?: number, cache = recreate(), episode?: number) =>
    cache.reserveRecoveryIteration("/tmp/bare", "branch", "key", source, 1000, gen, episode);
  return { reserve, recreate, source, read: () => structuredClone(journal),
    write: (value: RecoveryJournalEntry) => { journal = structuredClone(value); },
    advance: (ms: number) => { now += ms; } };
}

for (const state of ["partly-used", "exhausted-unblocked", "budget-blocked"] as const) {
  test(`new server episode renews ${state} with a fresh window and base backoff`, async t => {
    const f = fixture(t);
    await f.reserve(4, f.recreate(), 7);
    if (state !== "partly-used") {
      await f.reserve(4, f.recreate(), 7);
      await f.reserve(4, f.recreate(), 7);
    }
    if (state === "budget-blocked") await assert.rejects(f.reserve(4, f.recreate(), 7), /budget/);
    const before = f.read();
    f.advance(400_000);
    const renewed = await f.reserve(5, f.recreate(), 8);
    const expected = { ...before.recovery! };
    delete expected.blocker;
    assert.deepEqual(renewed, {
      ...expected, episode: 8, claimGeneration: 5, attempts: 1,
      startedAt: 1_400_000, deadline: 1_700_000, backoffMs: 1000, stage: "capturing",
    });
    assert.equal(Object.hasOwn(renewed, "blocker"), false);
    assert.deepEqual(f.read(), { ...before, recovery: renewed });
    assert.equal((await f.reserve(6, f.recreate(), 8)).attempts, 2);
    const third = await f.reserve(7, f.recreate());
    assert.equal(third.attempts, 3, "an older API cannot refund the renewed allowance");
    assert.equal(third.episode, 8, "an older API cannot lower the durable episode");
    await assert.rejects(f.reserve(8, f.recreate(), 8), /budget/);
    assert.equal(f.read().recovery?.attempts, 3);
    assert.equal(f.read().recovery?.episode, 8);
  });
}

test("legacy absent episode is logically zero and can renew once without serializing zero", async t => {
  const f = fixture(t);
  await f.reserve(4);
  const legacy = f.read();
  assert.equal(Object.hasOwn(legacy.recovery!, "episode"), false);
  const renewed = await f.reserve(5, f.recreate(), 1);
  assert.equal(renewed.episode, 1);
  assert.equal(renewed.attempts, 1);
  assert.equal((await f.reserve(6, f.recreate(), 0)).attempts, 2);
  assert.equal(f.read().recovery?.episode, 1, "explicit zero cannot lower stored allowance");
});

test("initial zero episode preserves legacy reservation bytes", async t => {
  const f = fixture(t);
  await f.reserve(1);
  const legacy = f.read();
  f.write(f.source);
  await f.reserve(1, f.recreate(), 0);
  assert.equal(JSON.stringify(f.read()), JSON.stringify(legacy));
});

test("initial reservation records a positive server episode without changing legacy serialization", async t => {
  const f = fixture(t);
  const first = await f.reserve(1, f.recreate(), 7);
  assert.equal(first.episode, 7);
  assert.equal(first.attempts, 1);
  assert.equal(first.backoffMs, 1000);
  assert.equal(first.deadline, 1_300_000);
});

for (const blocker of Object.keys(permanentBlockers) as Exclude<RecoveryBlocker, "budget_exhausted">[]) {
  test(`newer episode cannot renew permanent ${blocker}; runner stops before ownership`, async t => {
    const f = fixture(t);
    await f.reserve(4, f.recreate(), 7);
    const before = f.read();
    before.recovery = { ...before.recovery!, stage: "blocked", blocker };
    f.write(before);
    f.advance(400_000);
    await assert.rejects(f.reserve(5, f.recreate(), 8), /blocked/);
    assert.deepEqual(f.read(), before);
    const h = runnerFixture(f, async () => ({ status: "running", claim_generation: 5 }));
    await assert.rejects(h.run(5, 8));
    assert.equal(h.ownershipReads(), 0);
    assert.equal(h.quiescenceCalls(), 0);
    assert.deepEqual(f.read(), before);
  });
}

for (const state of ["partly-used", "budget-blocked"] as const) {
  for (const generation of [undefined, 3, 4]) {
    test(`newer episode with ${String(generation)} fence preserves ${state} whole journal`, async t => {
      const f = fixture(t);
      await f.reserve(4, f.recreate(), 7);
      if (state === "budget-blocked") {
        await f.reserve(4, f.recreate(), 7);
        await f.reserve(4, f.recreate(), 7);
        await assert.rejects(f.reserve(4, f.recreate(), 7), /budget/);
      }
      const before = f.read();
      await assert.rejects(f.reserve(generation, f.recreate(), 8), /fence/);
      assert.deepEqual(f.read(), before);
      if (state === "budget-blocked") {
        const h = runnerFixture(f, async () => ({ status: "running", claim_generation: generation }));
        await assert.rejects(h.run(generation, 8));
        assert.equal(h.quiescenceCalls(), 0);
        assert.deepEqual(f.read(), before, "runner failure finalization cannot rewrite blocked predecessor");
      }
    });
  }
}

function runnerFixture(f: ReturnType<typeof fixture>, ownership: () => Promise<unknown>) {
  let reads = 0, quiescence = 0;
  const runner = Object.assign(Object.create(RunRunner.prototype), {
    git: f.recreate(), recoveryRetryMs: 1000, codexBoundaryDeadlineMs: 1000,
    freeCaptureFlights: new WeakMap(), shutdownSignal: new AbortController(),
    client: { getRunOwnership: async () => { reads++; return ownership(); } },
    cloneCoordinates: () => ({ branch: "branch" }),
    retainedLifecycleGuard: () => {},
    retainedLifecycleSignal: () => new AbortController().signal,
    quiesceRun: async () => { quiescence++; return { blocked: true }; },
    reportGenericFailure: async () => {},
  });
  return {
    ownershipReads: () => reads, quiescenceCalls: () => quiescence,
    run: (generation: number | undefined, episode: number) => runner.recoverRetainedClaim({
      run_id: f.source.runId, secrets: {}, worker_recovery_episode: episode,
    }, {
      barePath: "/tmp/bare", runId: f.source.runId, claimGeneration: generation,
      executor: { run: async () => ({ branch: "branch" }) }, sinkGate: new SinkGate(),
      survivingTickGroups: [], runLog: nullLogger(),
    }, "key", f.read()),
  };
}

for (const ownership of ["owned", "unavailable", "stale"] as const) {
  test(`budget-blocked renewal ${ownership} passes real ownership and quiescence wrappers`, async t => {
    const f = fixture(t);
    for (let i = 0; i < 3; i++) await f.reserve(4, f.recreate(), 7);
    await assert.rejects(f.reserve(4, f.recreate(), 7), /budget/);
    const before = f.read();
    f.advance(400_000);
    const h = runnerFixture(f, async () => {
      if (ownership === "unavailable") throw new TypeError("fetch failed");
      return { status: "running", claim_generation: ownership === "owned" ? 5 : 4 };
    });
    await assert.rejects(h.run(5, 8));
    assert.ok(h.ownershipReads() > 0);
    assert.equal(h.quiescenceCalls(), ownership === "owned" ? 1 : 0);
    if (ownership === "owned") {
      const progress = f.read().recovery!;
      assert.equal(progress.episode, 8);
      assert.equal(progress.claimGeneration, 5);
      assert.equal(progress.attempts, 1);
      assert.equal(progress.startedAt, 1_400_000);
      assert.equal(progress.deadline, 1_700_000);
      assert.equal(progress.backoffMs, 1000);
      assert.equal(progress.blocker, "quiescence_failed");
    } else assert.deepEqual(f.read(), before);
  });
}

test("rollback during budget-blocked newer episode leaves whole journal unchanged through runner failure", async t => {
  const f = fixture(t);
  for (let i = 0; i < 3; i++) await f.reserve(4, f.recreate(), 7);
  await assert.rejects(f.reserve(4, f.recreate(), 7), /budget/);
  const before = f.read();
  f.advance(-1);
  await assert.rejects(f.reserve(5, f.recreate(), 8), /clock/);
  assert.deepEqual(f.read(), before);
  const h = runnerFixture(f, async () => ({ status: "running", claim_generation: 5 }));
  await assert.rejects(h.run(5, 8));
  assert.equal(h.quiescenceCalls(), 0);
  assert.deepEqual(f.read(), before);
});

test("fresh fenced claim after an interrupted long turn rebases deadline without refunding attempts", async t => {
  const f = fixture(t);
  const first = await f.reserve(1);
  f.advance(300_001);
  const second = await f.reserve(2);
  assert.equal(second.attempts, 2);
  assert.equal(second.backoffMs, 2000);
  assert.equal(second.deadline, first.deadline + 300_001);
  assert.equal(second.claimGeneration, 2);
  assert.deepEqual(second.source, first.source);
});

test("same-claim crash and recreation preserve deadline; expiry blocks", async t => {
  const f = fixture(t);
  const first = await f.reserve(1);
  f.advance(1000);
  const second = await f.reserve(1, f.recreate());
  assert.equal(second.deadline, first.deadline);
  f.advance(299_001);
  await assert.rejects(f.reserve(1, f.recreate()), /budget/);
  assert.equal(f.read().recovery?.blocker, "budget_exhausted");
});

test("fourth reservation stays refused across fresh claims", async t => {
  const f = fixture(t);
  for (const gen of [1, 2, 3]) { await f.reserve(gen); f.advance(300_001); }
  await assert.rejects(f.reserve(4), /budget/);
  assert.equal(f.read().recovery?.attempts, 3);
});

for (const blocker of [...Object.keys(permanentBlockers), "budget_exhausted"] as RecoveryBlocker[]) {
  test(`already blocked ${blocker} survives a new claim and expiry unchanged`, async t => {
    const f = fixture(t);
    await f.reserve(1);
    const value = f.read();
    value.recovery = { ...value.recovery!, stage: "blocked", blocker };
    f.write(value);
    f.advance(300_001);
    await assert.rejects(f.reserve(2), /blocked/);
    assert.deepEqual(f.read(), value);
  });
}

test("legacy episode binds once to a positive claim without inferring old identity", async t => {
  const f = fixture(t);
  const first = await f.reserve();
  f.advance(300_001);
  const bound = await f.reserve(7);
  assert.equal(bound.claimGeneration, 7);
  assert.equal(bound.attempts, 2);
  assert.equal(bound.deadline, first.deadline + 300_001);
  f.advance(300_001);
  await assert.rejects(f.reserve(7), /budget/);
});

test("legacy caller cannot refresh an expired legacy record", async t => {
  const f = fixture(t);
  await f.reserve();
  f.advance(300_001);
  await assert.rejects(f.reserve(), /budget/);
});

test("older or invalid claim fences cannot refresh a window", async t => {
  const f = fixture(t);
  await f.reserve(4);
  const before = f.read();
  for (const gen of [3, 0, -1, 1.5, Number.NaN]) {
    await assert.rejects(f.reserve(gen));
    assert.deepEqual(f.read(), before);
  }
});

test("clock rollback still blocks even for a new claim", async t => {
  const f = fixture(t);
  await f.reserve(1);
  f.advance(-1);
  await assert.rejects(f.reserve(2), /clock/);
  assert.equal(f.read().recovery?.blocker, "clock_invalid");
});

for (const binding of ["bound predecessor", "legacy"] as const) {
  test(`runner rebases expired ${binding} before applying the operation deadline`, async t => {
    const f = fixture(t);
    const first = await f.reserve(binding === "legacy" ? undefined : 1);
    f.advance(300_001);
    let reports = "", reads = 0, quiescenceCalls = 0;
    // Run the production wrapper and ownership check, stopping at the first quiescence
    // boundary. The Linux runner suite covers capture/adoption and real source IO.
    const runner = Object.assign(Object.create(RunRunner.prototype), {
      git: f.recreate(), recoveryRetryMs: 1000, codexBoundaryDeadlineMs: 1000,
      freeCaptureFlights: new WeakMap(),
      shutdownSignal: new AbortController(),
      client: { getRunOwnership: async () => { reads++; return { status: "running", claim_generation: 2 }; } },
      cloneCoordinates: () => ({ branch: "branch" }),
      retainedLifecycleGuard: () => {},
      retainedLifecycleSignal: () => new AbortController().signal,
      quiesceRun: async (_flight: unknown, _executor: unknown, opts: unknown) => {
        quiescenceCalls++;
        assert.deepEqual(opts, { mode: "capture", site: "predecessor_capture",
          targetPaths: [f.source.clonePath], propagateControl: true });
        return { blocked: true };
      },
      reportGenericFailure: async (_claim: unknown, _flight: unknown, error: Error) => { reports = error.message; },
    });
    await assert.rejects(runner.recoverRetainedClaim({ run_id: f.source.runId, secrets: {} }, {
      barePath: "/tmp/bare", runId: f.source.runId, claimGeneration: 2,
      executor: { run: async () => ({ branch: "branch" }) },
      sinkGate: new SinkGate(), survivingTickGroups: [],
      runLog: nullLogger(),
    }, "key", f.read()));
    assert.equal(quiescenceCalls, 1, "the production capture wrapper reached quiescence");
    assert.ok(reads > 0, "positive ownership verification preceded the reservation");
    assert.equal(f.read().recovery?.claimGeneration, 2);
    assert.equal(f.read().recovery?.deadline, first.deadline + 300_001);
    assert.equal(f.read().recovery?.attempts, 2);
    assert.match(reports, /quiescence failed/);
    assert.doesNotMatch(reports, /budget exhausted/);
  });
}

for (const blocker of ["clock_invalid", "decoded_history_limit"] as const) {
  test(`runner failure preserves expired already-blocked ${blocker}`, async t => {
    const f = fixture(t);
    await f.reserve(2);
    const before = f.read();
    before.recovery = { ...before.recovery!, stage: "blocked", blocker };
    f.write(before);
    f.advance(300_001);
    let report = "";
    const runner = Object.assign(Object.create(RunRunner.prototype), {
      git: f.recreate(), codexBoundaryDeadlineMs: 1000,
      cloneCoordinates: () => ({ branch: "branch" }),
      retainedLifecycleGuard: () => {},
      retainedLifecycleSignal: () => new AbortController().signal,
      reportGenericFailure: async (_claim: unknown, _flight: unknown, error: Error) => { report = error.message; },
    });
    await assert.rejects(runner.recoverRetainedClaim({ run_id: f.source.runId }, {
      barePath: "/tmp/bare", runId: f.source.runId, claimGeneration: 2, runLog: nullLogger(),
    }, "key", f.read()));
    assert.deepEqual(f.read(), before);
    assert.doesNotMatch(report, /budget exhausted/);
    assert.match(report, blocker === "clock_invalid" ? /clock invalid/ : /previous decoded verification exhaustion/);
  });
}

test("runner cannot rebase without positive matching claim ownership", async t => {
  const f = fixture(t);
  const first = await f.reserve(1);
  f.advance(300_001);
  const before = f.read();
  let proof = false;
  const runner = Object.assign(Object.create(RunRunner.prototype), {
    git: f.recreate(), recoveryRetryMs: 1000, codexBoundaryDeadlineMs: 1000,
    shutdownSignal: new AbortController(),
    client: { getRunOwnership: async () => ({ status: "running", claim_generation: 1 }) },
    cloneCoordinates: () => ({ branch: "branch" }),
    retainedLifecycleGuard: () => {},
    retainedLifecycleSignal: () => new AbortController().signal,
    quiesceRun: async () => { proof = true; return { blocked: true }; },
    reportGenericFailure: async () => {},
  });
  await assert.rejects(runner.recoverRetainedClaim({ run_id: f.source.runId }, {
    barePath: "/tmp/bare", runId: f.source.runId, claimGeneration: 2, runLog: nullLogger(),
  }, "key", f.read()));
  assert.equal(proof, false, "no predecessor capture without matching ownership");
  assert.deepEqual(f.read(), before, "a lost claim must not block or mutate its predecessor episode");
  assert.equal(f.read().recovery?.claimGeneration, 1);
  assert.equal(f.read().recovery?.deadline, first.deadline);
  assert.equal(f.read().recovery?.attempts, 1);
});

test("first recovery window durably charges before ownership probes and preserves outage retries", async t => {
  const f = fixture(t);
  const observed: number[] = [];
  let report = "";
  const runner = Object.assign(Object.create(RunRunner.prototype), {
    git: f.recreate(), recoveryRetryMs: 1, codexBoundaryDeadlineMs: 1000,
    shutdownSignal: new AbortController(),
    waitRecoveryRetry: async () => {},
    client: { getRunOwnership: async () => {
      observed.push(f.read().recovery?.attempts ?? 0);
      throw new Error("ownership unavailable");
    } },
    cloneCoordinates: () => ({ branch: "branch" }),
    retainedLifecycleGuard: () => {},
    retainedLifecycleSignal: () => new AbortController().signal,
    reportGenericFailure: async (_claim: unknown, _flight: unknown, error: Error) => { report = error.message; },
  });
  await assert.rejects(runner.recoverRetainedClaim({ run_id: f.source.runId }, {
    barePath: "/tmp/bare", runId: f.source.runId, claimGeneration: 2,
    runLog: nullLogger(), batcher: { emit: () => {} },
  }, "key", f.read()));
  assert.deepEqual([...new Set(observed)], [1, 2, 3]);
  assert.ok(observed.every(attempt => attempt > 0), "every ownership probe follows its durable charge");
  assert.equal(f.read().recovery?.attempts, 3);
  assert.equal(f.read().recovery?.blocker, "budget_exhausted");
  assert.match(report, /budget exhausted/);
});

for (const fault of ["network", "503"] as const) {
  test(`renewal ${fault} proof failure stops retryably without changing the predecessor episode`, async t => {
    const f = fixture(t);
    await f.reserve(1);
    f.advance(300_001);
    const before = f.read();
    let reports = 0, proofs = 0;
    const runner = Object.assign(Object.create(RunRunner.prototype), {
      git: f.recreate(), recoveryRetryMs: 1, codexBoundaryDeadlineMs: 1000,
      shutdownSignal: new AbortController(),
      client: { getRunOwnership: async () => {
        if (fault === "503") throw new RequestError("GET", "/ownership", 503, "service unavailable");
        throw new TypeError("fetch failed");
      } },
      cloneCoordinates: () => ({ branch: "branch" }),
      retainedLifecycleGuard: () => {},
      retainedLifecycleSignal: () => new AbortController().signal,
      quiesceRun: async () => { proofs++; return { blocked: true }; },
      reportGenericFailure: async () => { reports++; },
    });
    await assert.rejects(runner.recoverRetainedClaim({ run_id: f.source.runId }, {
      barePath: "/tmp/bare", runId: f.source.runId, claimGeneration: 2, runLog: nullLogger(),
    }, "key", f.read()));
    assert.deepEqual(f.read(), before, "a transient renewal proof cannot charge, rebase or block saved work");
    assert.equal(reports, 0, "no terminal failure report; leave the claim for bounded missing-flight requeue");
    assert.equal(proofs, 0, "no capture authority without ownership proof");
    const next = await f.reserve(3);
    assert.equal(next.attempts, 2, "a later valid claim can still reserve the remaining budget");
    assert.equal(next.claimGeneration, 3);
  });
}

for (const outcome of ["typed 404", "terminal", "foreign generation"] as const) {
  test(`renewal ${outcome} remains a stop with its predecessor unchanged`, async t => {
    const f = fixture(t);
    await f.reserve(1);
    f.advance(300_001);
    const before = f.read();
    let reports = 0, proofs = 0;
    const runner = Object.assign(Object.create(RunRunner.prototype), {
      git: f.recreate(), recoveryRetryMs: 1, codexBoundaryDeadlineMs: 1000,
      shutdownSignal: new AbortController(),
      client: { getRunOwnership: async () => {
        if (outcome === "typed 404") throw new RequestError("GET", `/api/worker/runs/${f.source.runId}/ownership`,
          404, JSON.stringify({ error: "run not found for this worker" }), undefined, { complete: true, bytes: 41 });
        return { status: outcome === "terminal" ? "completed" : "running", claim_generation: outcome === "terminal" ? 2 : 3 };
      } },
      cloneCoordinates: () => ({ branch: "branch" }),
      retainedLifecycleGuard: () => {},
      retainedLifecycleSignal: () => new AbortController().signal,
      quiesceRun: async () => { proofs++; return { blocked: true }; },
      reportGenericFailure: async () => { reports++; },
    });
    await assert.rejects(runner.recoverRetainedClaim({ run_id: f.source.runId }, {
      barePath: "/tmp/bare", runId: f.source.runId, claimGeneration: 2, runLog: nullLogger(),
    }, "key", f.read()));
    assert.deepEqual(f.read(), before);
    assert.equal(reports, 0);
    assert.equal(proofs, 0);
  });
}
