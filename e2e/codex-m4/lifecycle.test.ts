// PRD #1287 C4 (D7 layer U) — Codex delegation & cleanup (lifecycle) conformance, driving the
// REAL production ExecutionRegistry + CodexExecutionSafety facade (+ the real CodexCallbackBroker
// for the post-completion callback case). These reuse the M3 primitives, NOT a replacement policy
// broker (D5): every fake here is a process/seam injection, and every case pairs a POSITIVE
// control (a permitted action that reaches its effect) with an independent NEGATIVE-effect oracle
// (sink-called counter / spawn-seam counter / registry terminal state / sticky poison) — never a
// denial string (D4).
//
// Distinct angle from agent/test/codex-safety.test.ts + codex-registry.test.ts: those assert the
// registry/facade transitions directly; these frame the SAME real primitives as lifecycle
// invariants observed through withBoundary's durability SINK and the broker's command-spawn
// effect, and add the plural "every command root" and sticky-poison-across-boundaries angles the
// existing single-root / single-boundary cases do not cover.

import { before, describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  loadLifecycleModules,
  FakeRoot,
  spawnCounter,
  registerRoot,
  tick,
  boundaryReq,
  type LifecycleModules,
} from "./harness-lifecycle.js";
import { loadUModules, makeUBroker, leadGrants, type UModules } from "./harness-u.js";
import { recordEvidence } from "./evidence.js";
import type { BoundarySeams } from "../../agent/src/codex/safety.js";
import {
  CODEX_U_LIFECYCLE_SYNC_CHILD_TITLE,
  CODEX_U_LIFECYCLE_HELD_CALLBACK_TITLE,
  CODEX_U_LIFECYCLE_LATE_CALLBACK_TITLE,
  CODEX_U_LIFECYCLE_ROOT_ORDERING_TITLE,
  CODEX_U_LIFECYCLE_TIMEOUT_UNCONFIRMED_TITLE,
} from "./titles-c4.js";

let mods: LifecycleModules;
let umods: UModules;
before(async () => {
  mods = await loadLifecycleModules();
  umods = await loadUModules();
});

describe("codex U lifecycle (real ExecutionRegistry + CodexExecutionSafety)", () => {
  it(CODEX_U_LIFECYCLE_SYNC_CHILD_TITLE, async () => {
    const { ExecutionRegistry, newLocalExecutionEpoch } = mods.registry;
    const { createCodexExecutionSafety, CodexBoundaryError } = mods.safety;

    // NEGATIVE: a delegated child effect (a real callback reservation) admitted but NOT yet
    // settled. The parent's result/durability boundary must NOT settle while it is in flight.
    const regBusy = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const child = regBusy.reserveCallback({ threadId: "root", turnId: "t1", callId: "child-effect", fingerprint: "fp" });
    assert.equal(child.kind, "admitted", "a fresh child callback is admitted");
    const safetyBusy = createCodexExecutionSafety(regBusy, spawnCounter().seam);
    let busySink = 0;
    await assert.rejects(
      safetyBusy.withBoundary(boundaryReq("finalize", 40), async () => { busySink += 1; }),
      (e: unknown) => e instanceof CodexBoundaryError && e.stage === "quiesce",
    );
    assert.equal(busySink, 0, "the parent result sink stays UNCALLED while the child callback is in flight");
    assert.equal(regBusy.isPoisoned(), true, "an unsettled child at the boundary poisons the epoch");

    // POSITIVE: the SAME shape, but the child settles first → the parent boundary proceeds.
    const regDone = new ExecutionRegistry(newLocalExecutionEpoch(2));
    const settled = regDone.reserveCallback({ threadId: "root", turnId: "t1", callId: "child-effect", fingerprint: "fp" });
    assert.equal(settled.kind, "admitted");
    if (settled.kind === "admitted") regDone.settleCallback(settled.token, "ok");
    const safetyDone = createCodexExecutionSafety(regDone, spawnCounter().seam);
    let doneSink = 0;
    await safetyDone.withBoundary(boundaryReq("finalize"), async () => { doneSink += 1; });
    assert.equal(doneSink, 1, "once the delegated child settled, the parent boundary runs its sink exactly once");
    assert.equal(regDone.isPoisoned(), false);

    recordEvidence(CODEX_U_LIFECYCLE_SYNC_CHILD_TITLE, "pass");
  });

  it(CODEX_U_LIFECYCLE_HELD_CALLBACK_TITLE, async () => {
    const { ExecutionRegistry, newLocalExecutionEpoch } = mods.registry;
    const { createCodexExecutionSafety, CodexBoundaryError } = mods.safety;

    // A held callback reservation keeps the boundary quiesce PENDING and the protected sink
    // UNCALLED until it drains; a mid-wait settle then drains it clean.
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const held = reg.reserveCallback({ threadId: "root", turnId: "t1", callId: "held-cb", fingerprint: "fp" });
    assert.equal(held.kind, "admitted");
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam);
    let sink = 0;
    let boundarySettled = false;
    const p = safety
      .withBoundary(boundaryReq("checkpoint", 1000), async () => { sink += 1; })
      .then(() => { boundarySettled = true; });
    await tick();
    assert.equal(sink, 0, "the protected sink stays UNCALLED while the callback is held");
    assert.equal(boundarySettled, false, "the boundary quiesce is still pending (blocked on the held callback)");
    // Drain it mid-wait.
    if (held.kind === "admitted") reg.settleCallback(held.token, "ok");
    await p;
    assert.equal(sink, 1, "a drained (settled) held callback lets the boundary run its sink once");
    assert.equal(reg.isPoisoned(), false, "an in-time drain is clean, not poisoned");

    // NEGATIVE: a held callback that NEVER drains poisons at the deadline; the sink never fires.
    const reg2 = new ExecutionRegistry(newLocalExecutionEpoch(2));
    reg2.reserveCallback({ threadId: "root", turnId: "t1", callId: "never-drains", fingerprint: "fp" });
    const safety2 = createCodexExecutionSafety(reg2, spawnCounter().seam);
    let sink2 = 0;
    await assert.rejects(
      safety2.withBoundary(boundaryReq("shutdown", 25), async () => { sink2 += 1; }),
      (e: unknown) => e instanceof CodexBoundaryError && e.stage === "quiesce",
    );
    assert.equal(sink2, 0, "a never-drained held callback keeps the sink UNCALLED");
    assert.equal(reg2.isPoisoned(), true);

    recordEvidence(CODEX_U_LIFECYCLE_HELD_CALLBACK_TITLE, "pass");
  });

  it(CODEX_U_LIFECYCLE_LATE_CALLBACK_TITLE, async () => {
    // Drive the REAL broker over the REAL registry so a "no second effect" oracle is a
    // command-spawn count, not a marker read.
    const h = makeUBroker(umods, { grants: leadGrants() });
    const id = { threadId: "root", turnId: "t1", callId: "c-orig" };

    // The original callback runs its command effect exactly once.
    const orig = await h.broker.handleToolCall(id, "Bash", { command: "echo hi" }, "root");
    assert.equal(orig.ok, true, "the original callback succeeds");
    assert.equal(h.spawn.calls.length, 1, "the original callback reached the command spawn seam once");

    // The parent turn completes: quiesce to closed (the callback already settled → clean).
    const q = await h.registry.quiesceChildren(1000);
    assert.equal(q.kind, "quiescent");
    assert.equal(h.registry.state(), "closed");

    // A LATE replay of the SAME identity+payload after completion returns the cached terminal —
    // NO second command spawn, NO reducer transition.
    const replay = await h.broker.handleToolCall(id, "Bash", { command: "echo hi" }, "root");
    assert.equal(replay.ok, true);
    if (replay.ok) assert.deepEqual(replay.output, { replay: true }, "a post-completion replay returns the cached terminal, not a re-run");
    assert.equal(h.spawn.calls.length, 1, "the late replay produced NO second command spawn");

    // A NEW late callback after admission closed is refused admission_closed (no effect).
    const late = await h.broker.handleToolCall(
      { threadId: "root", turnId: "t1", callId: "c-late-new" },
      "Bash",
      { command: "echo bye" },
      "root",
    );
    assert.equal(late.ok, false, "a new callback after completion is refused");
    if (!late.ok) assert.equal(late.code, "admission_closed");
    assert.equal(h.spawn.calls.length, 1, "a new post-completion callback never reaches the command spawn seam");

    recordEvidence(CODEX_U_LIFECYCLE_LATE_CALLBACK_TITLE, "pass");
  });

  it(CODEX_U_LIFECYCLE_ROOT_ORDERING_TITLE, async () => {
    const { ExecutionRegistry, newLocalExecutionEpoch } = mods.registry;
    const { CodexExecutionSafetyImpl } = mods.safety;

    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    registerRoot(reg, new FakeRoot("provider")); // a provider root is also present in the run
    const cmd1 = new FakeRoot("command");
    const cmd2 = new FakeRoot("command");
    registerRoot(reg, cmd1);
    registerRoot(reg, cmd2);
    await reg.quiesceChildren(1000); // closed; both command roots stay unreaped
    assert.equal(reg.hasLiveCommandRoot(), true);

    const { state: spawnState, seam } = spawnCounter();
    // A reap seam that CLAIMS observed-empty without actually reaping the registry's roots — the
    // [R3-2] guard must not trust it; it re-checks the registry directly.
    const seams: BoundarySeams = {
      quiesce: (r) => reg.quiesceChildren(r.deadlineMs),
      reap: async (_r, epoch) => ({ kind: "observed_empty", evidence: "supervisor_echild", epoch }),
      dispose: (r) => reg.disposeTools(r.deadlineMs),
      spawnRoot: seam,
    };
    const safety = new CodexExecutionSafetyImpl(reg, seams);

    const outcomes: string[] = [];
    await safety.withBoundary(boundaryReq("credentialed_git"), async (permit) => {
      const a = await safety.spawnBoundaryAction(permit, ["git", "push"], "worker_pat");
      outcomes.push(a.kind === "refused" ? `refused:${a.reason}` : a.kind);
      assert.equal(spawnState.calls, 0, "no git child spawns while BOTH command roots are live");
      await reg.reapRoot(cmd1, 1000); // reap ONE — one command root still live
      const b = await safety.spawnBoundaryAction(permit, ["git", "push"], "worker_pat");
      outcomes.push(b.kind === "refused" ? `refused:${b.reason}` : b.kind);
      assert.equal(spawnState.calls, 0, "no git child spawns while ANY command root is still live");
      await reg.reapRoot(cmd2, 1000); // reap the LAST — now zero command roots live
      const c = await safety.spawnBoundaryAction(permit, ["git", "push"], "worker_pat");
      outcomes.push(c.kind);
    });

    assert.deepEqual(
      outcomes,
      ["refused:command_roots_live", "refused:command_roots_live", "settled"],
      "worker_pat stays refused until EVERY command root reaps",
    );
    assert.equal(spawnState.calls, 1, "the worker_pat git child spawned ONLY after EVERY command root reaped");
    assert.equal(spawnState.lastIdentity, "worker_pat");
    assert.equal(reg.hasLiveCommandRoot(), false);

    recordEvidence(CODEX_U_LIFECYCLE_ROOT_ORDERING_TITLE, "pass");
  });

  it(CODEX_U_LIFECYCLE_TIMEOUT_UNCONFIRMED_TITLE, async () => {
    const { ExecutionRegistry, newLocalExecutionEpoch } = mods.registry;
    const { CodexExecutionSafetyImpl, createCodexExecutionSafety, CodexBoundaryError } = mods.safety;

    // POSITIVE: a fresh registry with clean seams runs the durability sink (proves reachability).
    const regOk = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const safetyOk = createCodexExecutionSafety(regOk, spawnCounter().seam);
    let okSink = 0;
    await safetyOk.withBoundary(boundaryReq("finalize"), async () => { okSink += 1; });
    assert.equal(okSink, 1, "a clean boundary reaches its durability sink");

    // NEGATIVE: a boundary whose quiesce settles only AFTER the deadline → timeout poisons and
    // the durability sink stays UNCALLED (an unconfirmed boundary never mints a permit).
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(2));
    const slowSeams: BoundarySeams = {
      quiesce: async () => {
        await new Promise<void>((r) => setTimeout(r, 40));
        return { kind: "quiescent", epoch: 2 };
      },
      reap: async (_r, epoch) => ({ kind: "observed_empty", evidence: "supervisor_echild", epoch }),
      dispose: async () => ({ kind: "disposed" }),
      spawnRoot: spawnCounter().seam,
    };
    const safety = new CodexExecutionSafetyImpl(reg, slowSeams);
    let durability = 0;
    await assert.rejects(
      safety.withBoundary(boundaryReq("shutdown", 15), async () => { durability += 1; }),
      (e: unknown) => e instanceof CodexBoundaryError,
    );
    assert.equal(durability, 0, "a timed-out (unconfirmed) boundary never fires the durability sink");
    assert.equal(reg.isPoisoned(), true);

    // STICKY: a LATER clean-seam boundary on the SAME poisoned registry STILL cannot fire the
    // sink — the unconfirmed/poisoned state permanently prevents the protected durability outcome.
    const cleanSeams: BoundarySeams = {
      quiesce: (r) => reg.quiesceChildren(r.deadlineMs), // poisoned → incomplete
      reap: async (_r, epoch) => ({ kind: "observed_empty", evidence: "supervisor_echild", epoch }),
      dispose: (r) => reg.disposeTools(r.deadlineMs),
      spawnRoot: spawnCounter().seam,
    };
    const safety2 = new CodexExecutionSafetyImpl(reg, cleanSeams);
    await assert.rejects(
      safety2.withBoundary(boundaryReq("finalize"), async () => { durability += 1; }),
      (e: unknown) => e instanceof CodexBoundaryError && e.stage === "quiesce",
    );
    assert.equal(durability, 0, "the sticky poison keeps every later durability sink UNCALLED");

    recordEvidence(CODEX_U_LIFECYCLE_TIMEOUT_UNCONFIRMED_TITLE, "pass");
  });
});
