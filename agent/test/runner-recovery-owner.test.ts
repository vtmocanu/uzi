import assert from "node:assert/strict";
import { test } from "node:test";
import { RunRunner } from "../src/runner.js";
import { RequestError } from "../src/client.js";
import { ownerFixture } from "./recovery-owner-fixture.js";
import { nullLogger } from "./helpers.js";

const cause = "retained_recovery_adoption_failed";
async function fixture(options: {
  feature?: boolean; persistence?: "write" | "read" | "other"; quiescent?: boolean | "throw" | "incomplete";
  ownership?: "stale" | "cancelled" | "unknown" | "newer-after-proof";
  ack?: "unknown" | "stale" | "cancelled" | "different" | "400" | "unknown-400";
} = {}) {
  const f = ownerFixture();
  await f.reserve(); await f.block();
  const cache = f.recreate();
  let captures = 0, models = 0, proofs = 0, ownership = 0, terminal = 0;
  const reports: unknown[] = [];
  const flight: any = { barePath: f.bare, runId: f.source.runId, claimGeneration: 2,
    executor: {}, runLog: nullLogger(), parked: false,
    reportState: async (body: unknown) => {
      reports.push(body);
      if (options.ack === "unknown") return {};
      if (options.ack === "unknown-400" && reports.length === 1) return {};
      if (options.ack === "400" || options.ack === "unknown-400") throw new RequestError("POST", "/state", 400, "invalid recovery cause");
      if (options.ack === "stale") return { staleClaim: true, status: "recovery_wait", recoveryCause: cause };
      if (options.ack === "cancelled") return { status: "cancelled" };
      if (options.ack === "different") return { status: "recovery_wait", recoveryCause: "data_volume_full" };
      return { status: "recovery_wait", recoveryCause: cause };
    } };
  if (options.persistence === "write") cache.blockRecoveryEpisode = async () => { throw new Error("write unknown"); };
  cache.terminalRetainedSnapshot = async () => {
    if (options.persistence === "read") throw new Error("read unknown");
    const journal = f.read();
    if (options.persistence === "other") journal.recovery!.blocker = "capture_failed";
    return { journal, paths: [journal.clonePath] };
  };
  const runner: any = Object.assign(Object.create(RunRunner.prototype), {
    git: cache, recoveryRetryMs: 1, codexBoundaryDeadlineMs: 1000,
    client: { protocolFeatures: options.feature === false ? [] : ["recovery_cause_" + cause],
      getRunOwnership: async () => {
        ownership++;
        if (options.ownership === "unknown") throw new TypeError("ownership unknown");
        return { status: options.ownership === "cancelled" ? "cancelled" : "running",
          claim_generation: options.ownership === "stale" || (options.ownership === "newer-after-proof" && ownership > 1) ? 3 : 2 };
      } },
    cloneCoordinates: () => ({ branch: f.branch }),
    retainedLifecycleGuard: () => {}, retainedLifecycleSignal: () => new AbortController().signal,
    runFreeCapture: async (_claim: unknown, _flight: unknown, _deadline: number, _signal: AbortSignal, action: (held: unknown) => Promise<unknown>) =>
      options.quiescent === "incomplete" ? { kind: "incomplete" } : { kind: "observed_empty", value: await action({}) },
    assertHeldFreeCapture: () => {},
    quiesceRun: async () => { proofs++; if (options.quiescent === "throw") throw new Error("proof unavailable"); return { blocked: options.quiescent === false }; },
    captureRecoveryRestorePoint: async () => { captures++; throw new Error("must not capture"); },
    reportGenericFailure: async () => { terminal++; },
    makeExecutor: () => ({ executor: { run: async () => { models++; } } }),
  });
  const claim: any = { run_id: f.source.runId, worker_recovery_episode: 99, worker_recovery_owner_episode: 0 };
  return { f, runner, flight, claim, run: () => runner.recoverRetainedClaim(claim, flight, f.key, f.read()),
    counts: () => ({ captures, models, proofs, ownership, terminal, sends: reports.length }), reports };
}

test("same raw grant with a derived rise reholds repeatedly without capture/model and proves ownership/quiescence", async () => {
  const h = await fixture();
  const before = h.f.read();
  for (let i = 0; i < 2; i++) {
    await assert.rejects(h.run());
    assert.equal(h.flight.parked, true);
    assert.deepEqual(h.f.read(), before);
  }
  assert.deepEqual(h.counts(), { captures: 0, models: 0, proofs: 2, ownership: 4, terminal: 0, sends: 2 });
  assert.deepEqual(h.reports[0], { status: "recovery_wait", recovery_cause: cause, claim_generation: 2 });
});

for (const control of ["write", "read", "other", "quiescence", "stale-owner", "cancelled-owner", "unknown-owner",
  "unknown", "stale", "cancelled", "different", "400", "unknown-400", "no-feature"] as const) {
  test(`adoption rehold respects ${control}`, async () => {
    const h = await fixture({
      ...(["write", "read", "other"].includes(control) ? { persistence: control as "write" | "read" | "other" } : {}),
      ...(control === "quiescence" ? { quiescent: false } : {}),
      ...(control.endsWith("-owner") ? { ownership: control.split("-")[0] as "stale" | "cancelled" | "unknown" } : {}),
      ...(["unknown", "stale", "cancelled", "different", "400", "unknown-400"].includes(control)
        ? { ack: control as "unknown" | "stale" | "cancelled" | "different" | "400" | "unknown-400" } : {}),
      ...(control === "no-feature" ? { feature: false } : {}),
    });
    await assert.rejects(h.run());
    assert.equal(h.flight.parked, false);
    assert.equal(h.counts().captures, 0); assert.equal(h.counts().models, 0);
    assert.equal(h.counts().terminal, ["400", "no-feature", "other", "write", "read", "quiescence"].includes(control) ? 1 : 0);
    assert.ok(h.counts().sends <= 2);
    assert.equal(h.f.read().recovery?.blocker, "adoption_failed");
  });
}

for (const proof of ["throw", "incomplete"] as const) {
  for (const ownership of [undefined, "newer-after-proof"] as const) {
    test(`unproven ${proof} quiescence with ${ownership ?? "current-owner"} keeps custody without a hold`, async () => {
      const h = await fixture({ quiescent: proof, ownership });
      const before = h.f.read();
      await assert.rejects(h.run());
      assert.equal(h.flight.parked, false);
      assert.equal(h.counts().sends, 0);
      assert.equal(h.counts().terminal, ownership ? 0 : 1);
      assert.equal(h.flight.preserveRecoveryClone, true);
      assert.equal(h.flight.retainedEpisodeCustody, true);
      assert.deepEqual(h.f.read(), before);
    });
  }
}

test("quiescence deadline exhaustion before park attempts terminal failure with custody retained", async t => {
  const h = await fixture();
  const now = Date.now();
  h.runner.quiesceRun = async () => {
    t.mock.method(Date, "now", () => now + 2000);
    return { blocked: false };
  };
  await assert.rejects(h.run());
  assert.equal(h.flight.parked, false);
  assert.equal(h.counts().sends, 0);
  assert.equal(h.counts().terminal, 1);
  assert.equal(h.flight.preserveRecoveryClone, true);
  assert.equal(h.flight.retainedEpisodeCustody, true);
});

test("confirmed earlier blocker takes precedence over attempted adoption-failure park", async () => {
  const h = await fixture();
  const block = h.runner.git.blockRecoveryEpisode.bind(h.runner.git);
  h.runner.git.blockRecoveryEpisode = async (...args: any[]) => {
    const journal = h.f.read(); journal.recovery!.blocker = "capture_failed"; h.f.write(journal);
    await block(...args);
  };
  await assert.rejects(h.run());
  assert.equal(h.counts().sends, 0);
  assert.equal(h.counts().terminal, 1);
});

for (const side of ["before", "after"] as const) {
  test(`runner pending ${side.toUpperCase()} preserves journal without false failure`, async () => {
    const h = await fixture();
    h.f.fault((args, phase) => {
      if (args.at(-2) === h.f.journalKey && phase === side) throw new Error("interrupted charge");
    });
    await assert.rejects(h.f.reserve(2, 1, 8), /interrupted/);
    h.f.fault();
    h.claim.worker_recovery_owner_episode = 1;
    const raw = h.f.values.get(h.f.journalKey)![0];
    await assert.rejects(h.run());
    assert.equal(h.f.values.get(h.f.journalKey)![0], raw);
    assert.equal(h.counts().terminal, 0);
    assert.equal(h.counts().captures, 0);
    assert.equal(h.counts().models, 0);
    assert.equal(h.counts().sends, side === "before" ? 1 : 0);
    assert.equal(h.counts().proofs, side === "before" ? 1 : 0);
  });
}

for (const ownership of [undefined, "stale", "unknown"] as const) {
  test(`runner pending BEFORE first charge checks ${ownership ?? "current"} ownership inside reservation`, async () => {
    const h = await fixture({ ownership });
    h.f.values.delete(h.f.ownerKey);
    h.f.write(h.f.source);
    h.f.fault((args, phase) => {
      if (args.at(-2) === h.f.journalKey && phase === "before") throw new Error("interrupted first");
    });
    await assert.rejects(h.f.reserve(1, 0, 7), /interrupted/);
    h.f.fault();
    h.claim.worker_recovery_episode = 7;
    const before = h.f.read();
    const reserve = h.runner.git.reserveRecoveryIteration.bind(h.runner.git);
    let admitted = false;
    h.runner.git.reserveRecoveryIteration = async (...args: Parameters<typeof reserve>) => {
      const charged = await reserve(...args);
      admitted = true;
      assert.equal(charged.claimGeneration, 2);
      assert.equal(JSON.parse(h.f.values.get(h.f.ownerKey)![0]!).state, "committed");
      // Stop at the reservation boundary; capture/model are outside this test's seam.
      throw new Error("charged boundary reached");
    };
    await assert.rejects(h.run());
    assert.ok(h.counts().ownership > 0);
    assert.equal(admitted, ownership === undefined);
    assert.equal(h.counts().captures, 0);
    assert.equal(h.counts().models, 0);
    if (ownership === "stale") {
      assert.deepEqual(h.f.read(), before);
      assert.equal(h.counts().terminal, 0);
    }
  });
}
