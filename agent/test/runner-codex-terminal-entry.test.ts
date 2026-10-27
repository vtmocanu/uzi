import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import type { Executor } from "../src/executor.js";
import { git, gitlabClaim, fakeGitlab, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, FakeCodexExecutor } from "./runner-codex-sinks-fixture.js";

installHarness();

type Flight = {
  executor: Executor; worktreePath: string; runId: string; claimGeneration: number;
  preserveRecoveryClone?: boolean; stoppedSourceKept?: boolean; sourceExitDenied?: boolean;
  evidenceProcessJoined?: Executor;
  keepGuardedInventoryOpen?: boolean;
  deferredTerminalRelease?: Record<string, unknown>;
  terminalDisposal?: { source: Record<string, unknown>; result: { kind: string } };
};
type Seam = {
  retainStoppedSource(flight: Flight, executor: Executor, reason: string,
    handler: { memo: boolean; stale: boolean; record: "none" }): Promise<unknown>;
  phaseClone(...args: unknown[]): Promise<void>;
  reportGenericFailure(...args: unknown[]): Promise<void>;
};
const reasons = [
  "plan_revision_recovery_stopped", "input_receipt_unconfirmed", "stale_revision_rejection",
  "terminal_disk_precheck", "recovery_confirm_gone", "recovery_confirm_terminal",
  "recovery_receipt_failed", "recovery_terminal_status", "recovery_cancel_ack",
  "recovery_capture_verified", "terminal_disk_recheck", "terminal_disk_pending_terminal",
  "recovery_park_ack_terminal",
];

it("M2 entry seam: every release reason defers reads and safety with live admissions", async t => {
  const rig = codexRig();
  const executor = new FakeCodexExecutor(rig.safety, async () => { throw new Error("unused"); });
  const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab);
  const seam = runner as unknown as Seam;
  const root = rig.registry.reserveLaunch("provider");
  assert.equal(root.kind, "reserved");
  let disposed = 0;
  if (root.kind === "reserved") rig.registry.registerRoot(root.reservation, {
    kind: "provider", reap: async () => ({ ok: true }), dispose: async () => { disposed++; },
  });
  const pending = rig.registry.reserveLaunch("provider");
  assert.equal(pending.kind, "reserved");
  let reads = 0, acquired = 0, inspectingEntries = false, entries = 0, failures = 0;
  let firstFailure: unknown;
  for (const name of ["worktreeHead", "worktreeStatus", "verifyStoppedSourceCovered"] as const) {
    const original = git[name].bind(git);
    t.mock.method(git, name, async (...args: unknown[]) => {
      if (inspectingEntries) { reads++; throw new Error("premature source read"); }
      return Reflect.apply(original, git, args);
    });
  }
  for (const name of ["withBoundary", "dispose"] as const) {
    const original = rig.safety[name].bind(rig.safety);
    t.mock.method(rig.safety, name, async (...args: unknown[]) => {
      if (inspectingEntries) { acquired++; throw new Error("premature safety acquisition"); }
      return Reflect.apply(original, rig.safety, args);
    });
  }
  const clone = seam.phaseClone.bind(runner);
  t.mock.method(seam, "reportGenericFailure", async () => {});
  t.mock.method(seam, "phaseClone", async (...args: unknown[]) => {
    await clone(...args);
    const actual = args[1] as Flight;
    inspectingEntries = true;
    try {
      for (const reason of reasons) {
        entries++;
        try {
        const priorReads = reads, priorAcquired = acquired;
        // Preserve the production flight's real steering, gate, source and clone context.
        // Each request has its own provisional flags, against the still-live same epoch.
        const flight: Flight = { ...actual };
        assert.equal(await seam.retainStoppedSource(flight, executor, reason,
          { memo: false, stale: true, record: "none" }), "retained_local", reason);
        assert.equal(reads, priorReads, reason + ": no source reads or coverage");
        assert.equal(acquired, priorAcquired, reason + ": no queue acquisition");
        assert.equal(disposed, 0, reason + ": registered root remains alive");
        assert.equal(rig.registry.state(), "open");
        assert.equal(rig.registry.pendingLaunchCount(), 1);
        assert.equal(flight.preserveRecoveryClone, true);
        assert.equal(flight.sourceExitDenied, true);
        assert.equal(flight.keepGuardedInventoryOpen, true, reason + ": stale inventory guard precedes deferral");
        const request = flight.deferredTerminalRelease!;
        assert.ok(Object.isFrozen(request), reason + ": immutable request");
        assert.equal(request.reason, reason);
        assert.equal(request.executor, executor);
        assert.equal(request.safety, rig.safety);
        assert.equal(request.clonePath, actual.worktreePath);
        assert.equal(request.runId, actual.runId);
        assert.equal(request.generation, actual.claimGeneration);
        flight.worktreePath += "-different";
        assert.equal(request.clonePath, actual.worktreePath, "source identity is a snapshot");
        } catch (error) { failures++; firstFailure ??= error; }
      }
    } finally { inspectingEntries = false; }
    throw new Error("entry matrix complete before model admission");
  });
  try {
    await runner.execute(gitlabClaim(285689, { claim_generation: 3 }));
    assert.equal(entries, reasons.length, "all release entry reasons executed");
    assert.equal(failures, 0, String(firstFailure));
  } finally {
    if (pending.kind === "reserved") rig.registry.cancelReservation(pending.reservation);
  }
});

it("stale deferred release keeps guarded inventory closed despite veto or invalidation", async t => {
  const rig = codexRig();
  const executor = new FakeCodexExecutor(rig.safety, async () => { throw new Error("model must not run"); });
  const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab, "stale-inventory-proof");
  const seam = runner as unknown as Seam & {
    settleGuardedInventory(claim: ReturnType<typeof gitlabClaim>, flight: Flight): Promise<void>;
    owedOptions(...args: unknown[]): Promise<never>;
    terminalProcessProof(flight: Flight): boolean;
  };
  const claim = gitlabClaim(285696, { claim_generation: 3 });
  const clone = seam.phaseClone.bind(runner);
  let actual: Flight | undefined;
  const cases: { flight: Flight; reason: string; mode: string }[] = [];
  let guardedWork = 0;
  t.mock.method(seam, "owedOptions", async () => {
    guardedWork++;
    throw new Error("guarded inventory reached ownership/capture preparation");
  });
  t.mock.method(seam, "reportGenericFailure", async () => {});
  t.mock.method(seam, "phaseClone", async (...args: unknown[]) => {
    await clone(...args);
    actual = args[1] as Flight;
    for (const reason of ["plan_revision_recovery_stopped", "stale_revision_rejection", "terminal_disk_precheck"]) {
      for (const mode of ["deferred", "prior-veto", "later-keep", "changed-source"]) {
        const flight: Flight = { ...actual, preserveRecoveryClone: mode === "prior-veto" };
        await seam.retainStoppedSource(flight, executor, reason, { memo: false, stale: true, record: "none" });
        if (mode === "later-keep") flight.preserveRecoveryClone = true;
        if (mode === "changed-source") flight.worktreePath += "-changed";
        cases.push({ flight, reason, mode });
      }
    }
    // An independently kept real flight reaches a successful terminal disposal,
    // so incomplete disposal cannot incidentally supply the inventory guard.
    actual.preserveRecoveryClone = true;
    await rig.safety.withBoundary({ boundary: "finalize", deadlineMs: 120000 }, async () => {});
    throw new Error("stale inventory matrix complete before model admission");
  });
  await runner.execute(claim);
  assert.ok(actual);
  assert.equal(seam.terminalProcessProof(actual), true, "exact executor disposal succeeded");
  assert.equal(cases.length, 12);
  const beforeStaleCallbacks = guardedWork;
  for (const { flight, reason, mode } of cases) {
    assert.equal(flight.keepGuardedInventoryOpen, true, reason + "/" + mode);
    if (mode === "prior-veto" || mode === "later-keep")
      assert.equal(flight.deferredTerminalRelease, undefined, mode + ": no request to restore the latch later");
    await seam.settleGuardedInventory(claim, flight);
  }
  assert.equal(guardedWork, beforeStaleCallbacks, "stale reporting callbacks cannot prepare ownership, freeze or upload");
  // Prove the real settle path is enabled: removing only this guard reaches its
  // first preparation step even though sourceExitDenied remains set.
  const control: Flight = { ...cases[0]!.flight, keepGuardedInventoryOpen: false };
  await seam.settleGuardedInventory(claim, control);
  assert.equal(guardedWork, beforeStaleCallbacks + 1, "positive control reaches guarded inventory preparation");
});

for (const identity of ["facadeless", "swapped-to-Claude"] as const) {
  it(`M2 direct release entry: ${identity} Codex retains before sensitive access`, async t => {
    const rig = codexRig();
    const executor: Executor = Object.assign(
      new FakeCodexExecutor(rig.safety, async () => { throw new Error("unused"); }), { sandboxesCommands: true });
    if (identity === "facadeless") executor.safety = undefined;
    const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab);
    const seam = runner as unknown as Seam;
    const clone = seam.phaseClone.bind(runner);
    let checking = false, reads = 0, scans = 0, acquisitions = 0;
    let flight: Flight | undefined, entryError: unknown;
    for (const name of ["worktreeHead", "worktreeStatus", "verifyStoppedSourceCovered"] as const) {
      const original = git[name].bind(git);
      t.mock.method(git, name, async (...args: unknown[]) => {
        if (checking) { reads++; throw new Error("premature source read"); }
        return Reflect.apply(original, git, args);
      });
    }
    const quiescence = runner as unknown as { quiesceRun(...args: unknown[]): Promise<unknown> };
    const quiesce = quiescence.quiesceRun.bind(runner);
    t.mock.method(quiescence, "quiesceRun", async (...args: unknown[]) => {
      if (checking) { scans++; throw new Error("premature process scan"); }
      return quiesce(...args);
    });
    for (const name of ["withBoundary", "dispose"] as const) {
      const original = rig.safety[name].bind(rig.safety);
      t.mock.method(rig.safety, name, async (...args: unknown[]) => {
        if (checking) { acquisitions++; throw new Error("premature safety acquisition"); }
        return Reflect.apply(original, rig.safety, args);
      });
    }
    t.mock.method(seam, "reportGenericFailure", async () => {});
    t.mock.method(seam, "phaseClone", async (...args: unknown[]) => {
      await clone(...args);
      flight = args[1] as Flight;
      executor.safety = undefined;
      if (identity === "swapped-to-Claude") flight.executor = { run: executor.run.bind(executor) };
      checking = true;
      try {
        // Independent custody is already latched and must remain independent.
        flight.preserveRecoveryClone = true;
        for (const reason of reasons) {
          assert.equal(await seam.retainStoppedSource(flight, flight.executor, reason,
            { memo: false, stale: true, record: "none" }), "retained_local");
          assert.equal(flight.preserveRecoveryClone, true);
          assert.equal(flight.stoppedSourceKept, true);
          assert.equal(flight.sourceExitDenied, true);
          assert.equal(flight.deferredTerminalRelease, undefined, "no facade can supply a release request");
        }
      } catch (error) { entryError = error; } finally { checking = false; }
      throw new Error("entry control complete before model admission");
    });
    await runner.execute(gitlabClaim(identity === "facadeless" ? 285692 : 285693, { claim_generation: 3 }));
    if (entryError !== undefined) throw entryError;
    assert.ok(flight, "actual buildFlight and clone reached the direct release entry");
    assert.equal(reads, 0, "no HEAD/status/coverage");
    assert.equal(scans, 0, "no procfs or process scan");
    assert.equal(acquisitions, 0, "no queue acquisition or disposal");
    assert.equal(flight.preserveRecoveryClone, true);
    assert.equal(fs.existsSync(flight.worktreePath), true);
  });
}

for (const codex of [true, false]) {
  it(`M2 procfs positive writer: initial ${codex ? "Codex" : "Claude"} identity survives executor replacement`,
    { skip: process.platform !== "linux" }, async t => {
    const executor: Executor = { sandboxesCommands: codex, run: async ctx => ({ branch: ctx.branch }) };
    const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab);
    const seam = runner as unknown as Seam & {
      quiesceRun(flight: Flight, executor: Executor, opts: { mode: "own"; site: string }):
        Promise<{ blocked: boolean; outcome: { process?: { state: string } } }>;
    };
    const clone = seam.phaseClone.bind(runner);
    let observed: { blocked: boolean; state?: string; joined: boolean } | undefined;
    t.mock.method(seam, "reportGenericFailure", async () => {});
    t.mock.method(seam, "phaseClone", async (...args: unknown[]) => {
      await clone(...args);
      const flight = args[1] as Flight;
      flight.executor = { run: executor.run };
      const q = await seam.quiesceRun(flight, flight.executor, { mode: "own", site: "terminal_retire" });
      observed = { blocked: q.blocked, state: q.outcome.process?.state,
        joined: flight.evidenceProcessJoined === flight.executor };
      throw new Error("snapshot writer control complete before model admission");
    });
    await runner.execute(gitlabClaim(codex ? 285694 : 285695, { claim_generation: 3 }));
    assert.ok(observed, "actual flight and quiescence scan completed");
    assert.equal(observed.state, "quiescent", "real hermetic procfs snapshot is positive");
    assert.equal(observed.blocked, false, "snapshot blocking behavior is unchanged");
    assert.equal(observed.joined, !codex, "snapshot grants joined evidence only to Claude");
  });
}

for (const state of ["open", "closing"] as const) {
  it(`M2 terminal disposal refuses current-run ${state} epoch after early throw`, async t => {
    const rig = codexRig();
    const executor = new FakeCodexExecutor(rig.safety, async () => { throw new Error("model must not run"); });
    const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab);
    const seam = runner as unknown as Seam;
    const clone = seam.phaseClone.bind(runner);
    let reached = false, failures = 0, coverage = 0, disposalEntries = 0;
    let flight: Flight | undefined;
    let closing: Promise<unknown> | undefined;
    const admission = rig.registry.reserveLaunch("provider");
    assert.equal(admission.kind, "reserved");
    const cleanup = rig.registry.reserveLaunch("provider");
    assert.equal(cleanup.kind, "reserved");
    if (cleanup.kind === "reserved") rig.registry.registerRoot(cleanup.reservation, {
      kind: "provider", reap: async () => ({ ok: true }),
      dispose: async () => {
        // Settle the actual waiter through its admission event, including on the
        // unfixed base where disposal itself does not poison/signal closing.
        if (admission.kind === "reserved") rig.registry.cancelReservation(admission.reservation);
      },
    });
    // Throw immediately after clone creation, before the normal successful boundary.
    // Suppress failure reporting at its existing seam so it cannot reap this epoch.
    t.mock.method(seam, "reportGenericFailure", async () => { failures++; });
    t.mock.method(seam, "phaseClone", async (...args: unknown[]) => {
      await clone(...args);
      flight = args[1] as Flight;
      reached = true;
      await seam.retainStoppedSource(flight, executor, "terminal_disk_precheck",
        { memo: false, stale: true, record: "none" });
      if (state === "closing") closing = rig.registry.quiesceChildren(120000);
      assert.equal(rig.registry.state(), state);
      throw new Error("early phase throw");
    });
    t.mock.method(git, "verifyStoppedSourceCovered", async () => {
      coverage++;
      throw new Error("unproven epoch must not inspect source bytes");
    });
    const dispose = rig.safety.dispose.bind(rig.safety);
    t.mock.method(rig.safety, "dispose", async (request: Parameters<typeof dispose>[0]) => {
      disposalEntries++;
      assert.equal(rig.registry.state(), state, "exact M1 refusal branch reached");
      return dispose(request);
    });
    try {
      await runner.execute(gitlabClaim(state === "open" ? 285690 : 285691, { claim_generation: 3 }));
      assert.equal(reached, true);
      assert.equal(failures, 1);
      assert.equal(disposalEntries, 1);
      assert.equal(coverage, 0, "no source byte coverage without process proof");
      assert.ok(flight);
      assert.equal(flight.terminalDisposal?.result.kind, "incomplete");
      assert.ok(Object.isFrozen(flight.terminalDisposal));
      assert.ok(Object.isFrozen(flight.terminalDisposal!.source));
      assert.equal(flight.terminalDisposal!.source.executor, executor);
      assert.equal(flight.terminalDisposal!.source.safety, rig.safety);
      assert.equal(flight.terminalDisposal!.source.clonePath, flight.worktreePath);
      assert.equal(flight.preserveRecoveryClone, true);
      assert.equal(fs.existsSync(flight.worktreePath), true, "real clone retained");
    } finally {
      if (admission.kind === "reserved") rig.registry.cancelReservation(admission.reservation);
      await closing;
    }
  });
}
