import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  ExecutionRegistry,
  MAX_CALLBACK_RESERVATIONS,
  MAX_POISON_ERRORS,
  newLocalExecutionEpoch,
  type ReapOutcome,
  type RegisteredRoot,
  type RootKind,
} from "../src/codex/registry.js";

// PRD #1171 (M3 m1, first unit) — the immutable local-execution-epoch registry.
// Pure logic: every "process" is an injected fake root.

const DEADLINE = 1000;

class FakeRoot implements RegisteredRoot {
  reapCalls = 0;
  disposeCalls = 0;
  constructor(
    readonly kind: RootKind,
    private readonly reapImpl: (d: number) => Promise<ReapOutcome> = async () => ({ ok: true }),
    private readonly disposeImpl: (d: number) => Promise<void> = async () => {},
  ) {}
  async reap(d: number): Promise<ReapOutcome> {
    this.reapCalls += 1;
    return this.reapImpl(d);
  }
  async dispose(d: number): Promise<void> {
    this.disposeCalls += 1;
    return this.disposeImpl(d);
  }
}

const failReap: (d: number) => Promise<ReapOutcome> = async () => ({
  ok: false,
  error: { category: "timeout", message: "reap timed out" },
});

function registerRoot(reg: ExecutionRegistry, root: RegisteredRoot): void {
  const reserved = reg.reserveLaunch(root.kind);
  assert.equal(reserved.kind, "reserved");
  if (reserved.kind !== "reserved") return;
  reg.registerRoot(reserved.reservation, root);
}

describe("ExecutionRegistry: epoch identity and branding", () => {
  it("freezes the branded local execution epoch handed at construction", () => {
    const epoch = newLocalExecutionEpoch(7);
    const reg = new ExecutionRegistry(epoch);
    assert.equal(reg.epoch(), 7);
    assert.equal(reg.state(), "open");
  });

  it("treats a foreign (claim-shaped) epoch number as stale in reapProcesses", async () => {
    // The API claim epoch lives in a DISTINCT branded domain; a bare number that is
    // not this registry's frozen local execution epoch must never mint observed_empty.
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(42));
    const q = await reg.quiesceChildren(DEADLINE);
    assert.equal(q.kind, "quiescent");
    const reap = await reg.reapProcesses(DEADLINE, 99); // 99 = some other epoch domain
    assert.equal(reap.kind, "incomplete");
    assert.equal(reg.state(), "poisoned");
  });
});

describe("ExecutionRegistry: launch reservations vs close races", () => {
  it("quiesces clean when every launch reservation is settled by a root", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    registerRoot(reg, new FakeRoot("provider"));
    assert.equal(reg.pendingLaunchCount(), 0);
    const q = await reg.quiesceChildren(DEADLINE);
    assert.equal(q.kind, "quiescent");
    if (q.kind === "quiescent") assert.equal(q.epoch, 1);
  });

  it("quiesces clean when a reservation is cancelled (launch aborted)", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const reserved = reg.reserveLaunch("command");
    assert.equal(reserved.kind, "reserved");
    if (reserved.kind === "reserved") reg.cancelReservation(reserved.reservation);
    assert.equal(reg.pendingLaunchCount(), 0);
    const q = await reg.quiesceChildren(DEADLINE);
    assert.equal(q.kind, "quiescent");
  });

  it("an unresolved launch reservation at quiesce is incomplete AND poisons", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    reg.reserveLaunch("provider"); // never settled
    const q = await reg.quiesceChildren(DEADLINE);
    assert.equal(q.kind, "incomplete");
    assert.equal(reg.state(), "poisoned");
  });

  it("refuses new provider/command launches once admission is closed", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    await reg.quiesceChildren(DEADLINE);
    assert.equal(reg.state(), "closed");
    const denied = reg.reserveLaunch("provider");
    assert.equal(denied.kind, "denied");
  });

  it("opens the trusted boundary-action lane only after admission closes", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    await reg.quiesceChildren(DEADLINE);
    // The boundary-action lane is admitted in the closed state...
    const lane = reg.reserveLaunch("boundary_action");
    assert.equal(lane.kind, "reserved");
    // ...but ordinary model/command roots stay refused.
    assert.equal(reg.reserveLaunch("command").kind, "denied");
  });
});

describe("ExecutionRegistry: quiesceChildren bounded settlement (finding 5)", () => {
  it("waits for an in-flight launch reservation to settle before the deadline and quiesces clean", async () => {
    // The settle-before-boundary contract: a launch reservation still in flight when
    // quiesce begins must be given until the deadline to settle, not poisoned on sight.
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const reserved = reg.reserveLaunch("provider");
    assert.equal(reserved.kind, "reserved");
    if (reserved.kind !== "reserved") return;
    // Start quiescing while the reservation is still unsettled; do NOT await yet.
    const p = reg.quiesceChildren(1000);
    // Settle it well before the deadline.
    const registered = reg.registerRoot(reserved.reservation, new FakeRoot("provider"));
    assert.equal(registered.ok, true);
    const r = await p;
    assert.equal(r.kind, "quiescent");
    if (r.kind === "quiescent") assert.equal(r.epoch, 1);
    assert.equal(reg.isPoisoned(), false);
    assert.equal(reg.state(), "closed");
  });

  it("waits for an in-flight callback to settle before the deadline and quiesces clean", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const res = reg.reserveCallback({ threadId: "t1", turnId: "u1", callId: "c1", fingerprint: "fp" });
    assert.equal(res.kind, "admitted");
    if (res.kind !== "admitted") return;
    const p = reg.quiesceChildren(1000); // start the bounded wait, do NOT await
    reg.settleCallback(res.token, "ok"); // settle before the deadline
    const r = await p;
    assert.equal(r.kind, "quiescent");
    assert.equal(reg.isPoisoned(), false);
    assert.equal(reg.state(), "closed");
  });

  it("quiesces clean when an in-flight reservation is CANCELLED before the deadline", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const reserved = reg.reserveLaunch("command");
    assert.equal(reserved.kind, "reserved");
    if (reserved.kind !== "reserved") return;
    const p = reg.quiesceChildren(1000);
    reg.cancelReservation(reserved.reservation); // launch aborted before it spawned
    const r = await p;
    assert.equal(r.kind, "quiescent");
    assert.equal(reg.isPoisoned(), false);
  });

  it("poisons at the deadline when accepted work never settles within the bound", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    reg.reserveLaunch("provider"); // never settled
    const r = await reg.quiesceChildren(30); // small deadline: let it elapse
    assert.equal(r.kind, "incomplete");
    if (r.kind === "incomplete") assert.ok(r.errors.length >= 1);
    assert.equal(reg.isPoisoned(), true);
    assert.equal(reg.state(), "poisoned");
  });

  it("short-circuits to incomplete when the epoch is poisoned mid-wait", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const reserved = reg.reserveLaunch("provider");
    assert.equal(reserved.kind, "reserved");
    // Start the bounded wait, then hit a real protocol fault during it.
    const p = reg.quiesceChildren(1000);
    reg.poison({ category: "protocol", message: "mid-wait fault" });
    const r = await p;
    assert.equal(r.kind, "incomplete");
    if (r.kind === "incomplete") assert.ok(r.errors.some((e) => e.message === "mid-wait fault"));
    assert.equal(reg.isPoisoned(), true);
  });
});

describe("ExecutionRegistry: registerRoot returns a checked result (finding 6)", () => {
  it("returns ok:false AND poisons on a root-kind mismatch", () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const reserved = reg.reserveLaunch("provider");
    assert.equal(reserved.kind, "reserved");
    if (reserved.kind !== "reserved") return;
    // The launch reserved a provider, but the root produced is a command: a protocol
    // fault. The caller must be told (ok:false) so it does not keep using the root.
    const result = reg.registerRoot(reserved.reservation, new FakeRoot("command"));
    assert.equal(result.ok, false);
    if (!result.ok) assert.equal(result.error.category, "protocol");
    assert.equal(reg.isPoisoned(), true);
    assert.equal(reg.rootCount(), 0); // the mismatched root was never admitted
  });

  it("validates the caller's reservation copy against the stored reservation", () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const reserved = reg.reserveLaunch("provider");
    assert.equal(reserved.kind, "reserved");
    if (reserved.kind !== "reserved") return;
    // A structurally forged copy keeps the valid id but substitutes a kind that
    // matches the produced root. Comparing root.kind only with the caller's copy
    // admits it; the registry must bind both to the stored reservation instead.
    const substituted = { ...reserved.reservation, kind: "command" as const };
    const result = reg.registerRoot(substituted, new FakeRoot("command"));
    assert.equal(result.ok, false);
    assert.equal(reg.isPoisoned(), true);
    assert.equal(reg.rootCount(), 0);
  });

  it("returns ok:false AND poisons on an unknown/already-settled reservation", () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const reserved = reg.reserveLaunch("provider");
    assert.equal(reserved.kind, "reserved");
    if (reserved.kind !== "reserved") return;
    assert.equal(reg.registerRoot(reserved.reservation, new FakeRoot("provider")).ok, true);
    // Re-registering the SAME (now-settled) reservation is a protocol fault.
    const again = reg.registerRoot(reserved.reservation, new FakeRoot("provider"));
    assert.equal(again.ok, false);
    if (!again.ok) assert.equal(again.error.category, "protocol");
    assert.equal(reg.isPoisoned(), true);
  });

  it("returns ok:true on a matching-kind registration without poisoning", () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const reserved = reg.reserveLaunch("provider");
    assert.equal(reserved.kind, "reserved");
    if (reserved.kind !== "reserved") return;
    const result = reg.registerRoot(reserved.reservation, new FakeRoot("provider"));
    assert.equal(result.ok, true);
    assert.equal(reg.isPoisoned(), false);
    assert.equal(reg.rootCount(), 1);
  });
});

describe("ExecutionRegistry: reapProcesses aggregation", () => {
  it("returns observed_empty only when the epoch matches and EVERY root reaped", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(5));
    const provider = new FakeRoot("provider");
    const command = new FakeRoot("command");
    registerRoot(reg, provider);
    registerRoot(reg, command);
    await reg.quiesceChildren(DEADLINE);
    const reap = await reg.reapProcesses(DEADLINE, 5);
    assert.equal(reap.kind, "observed_empty");
    if (reap.kind === "observed_empty") {
      assert.equal(reap.evidence, "supervisor_echild");
      assert.equal(reap.epoch, 5);
    }
    assert.equal(provider.reapCalls, 1);
    assert.equal(command.reapCalls, 1);
    assert.equal(reg.hasLiveCommandRoot(), false);
  });

  it("one incomplete root poisons and blocks observed_empty (a drained root is not the run)", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(5));
    const good = new FakeRoot("provider");
    const bad = new FakeRoot("command", failReap);
    registerRoot(reg, good);
    registerRoot(reg, bad);
    await reg.quiesceChildren(DEADLINE);
    const reap = await reg.reapProcesses(DEADLINE, 5);
    assert.equal(reap.kind, "incomplete");
    if (reap.kind === "incomplete") assert.ok(reap.errors.length >= 1);
    assert.equal(reg.state(), "poisoned");
    // every root was still ASKED — no short-circuit.
    assert.equal(good.reapCalls, 1);
    assert.equal(bad.reapCalls, 1);
  });

  it("refuses to reap before quiescence (not closed) and poisons", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(5));
    registerRoot(reg, new FakeRoot("provider"));
    const reap = await reg.reapProcesses(DEADLINE, 5); // still "open"
    assert.equal(reap.kind, "incomplete");
    assert.equal(reg.state(), "poisoned");
  });

  it("hasLiveCommandRoot stays true until reapProcesses observes the command root", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(5));
    registerRoot(reg, new FakeRoot("command"));
    await reg.quiesceChildren(DEADLINE);
    assert.equal(reg.hasLiveCommandRoot(), true);
    await reg.reapProcesses(DEADLINE, 5);
    assert.equal(reg.hasLiveCommandRoot(), false);
  });
});

describe("ExecutionRegistry: callback reservation idempotency", () => {
  const key = { threadId: "t1", turnId: "u1", callId: "c1" };

  it("admits a fresh call and returns a cached terminal marker on replay", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const first = reg.reserveCallback({ ...key, fingerprint: "fp" });
    assert.equal(first.kind, "admitted");
    if (first.kind !== "admitted") return;
    reg.settleCallback(first.token, "ok");
    const replay = reg.reserveCallback({ ...key, fingerprint: "fp" });
    assert.equal(replay.kind, "replay");
    if (replay.kind === "replay") assert.equal(replay.marker.outcome, "ok");
  });

  it("returns a settled key's cached marker on replay AFTER a clean close (poisoned is distinct from cleanly-closed)", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const first = reg.reserveCallback({ ...key, fingerprint: "fp" });
    assert.equal(first.kind, "admitted");
    if (first.kind !== "admitted") return;
    reg.settleCallback(first.token, "ok");
    // A CLEAN close: nothing unsettled, so the epoch quiesces to "closed" and is NOT
    // poisoned. (poisoned and cleanly-closed are distinct terminal states.)
    const q = await reg.quiesceChildren(DEADLINE);
    assert.equal(q.kind, "quiescent");
    assert.equal(reg.state(), "closed");
    assert.equal(reg.isPoisoned(), false);
    // The settled key is still replayable: the existing-key block runs BEFORE the
    // admission gate, so a replay-after-clean-close returns its cached terminal
    // marker rather than being denied admission_closed.
    const replay = reg.reserveCallback({ ...key, fingerprint: "fp" });
    assert.equal(replay.kind, "replay");
    if (replay.kind === "replay") assert.equal(replay.marker.outcome, "ok");
    // Reading a cached marker never re-opens admission or poisons.
    assert.equal(reg.state(), "closed");
    assert.equal(reg.isPoisoned(), false);
  });

  it("denies a concurrent in-flight duplicate without running a second effect", () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    assert.equal(reg.reserveCallback({ ...key, fingerprint: "fp" }).kind, "admitted");
    const dup = reg.reserveCallback({ ...key, fingerprint: "fp" });
    assert.equal(dup.kind, "denied");
    if (dup.kind === "denied") assert.equal(dup.reason, "in_flight_duplicate");
    assert.notEqual(reg.state(), "poisoned"); // an honest replay must not poison
  });

  it("denies AND poisons call-id reuse with a changed payload/origin fingerprint", () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    assert.equal(reg.reserveCallback({ ...key, fingerprint: "fp-a" }).kind, "admitted");
    const changed = reg.reserveCallback({ ...key, fingerprint: "fp-b" });
    assert.equal(changed.kind, "denied");
    if (changed.kind === "denied") assert.equal(changed.reason, "changed_reuse");
    assert.equal(reg.state(), "poisoned");
  });

  it("treats a distinct (thread,turn,call) tuple as a separate reservation", () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    assert.equal(reg.reserveCallback({ ...key, fingerprint: "fp" }).kind, "admitted");
    const other = reg.reserveCallback({ threadId: "t1", turnId: "u2", callId: "c1", fingerprint: "fp" });
    assert.equal(other.kind, "admitted");
  });

  it("refuses new callbacks once admission is closed", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    await reg.quiesceChildren(DEADLINE);
    const denied = reg.reserveCallback({ ...key, fingerprint: "fp" });
    assert.equal(denied.kind, "denied");
    if (denied.kind === "denied") assert.equal(denied.reason, "admission_closed");
  });

  it("an unsettled callback at quiesce is incomplete AND poisons", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    reg.reserveCallback({ ...key, fingerprint: "fp" }); // admitted, never settled
    assert.equal(reg.inFlightCallbackCount(), 1);
    const q = await reg.quiesceChildren(DEADLINE);
    assert.equal(q.kind, "incomplete");
    assert.equal(reg.state(), "poisoned");
  });
});

describe("ExecutionRegistry: callback idle subscription", () => {
  it("keeps idle suspended until both admitted callbacks settle", () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const observedCounts: number[] = [];
    let idleArms = 0;
    const onCallbackChange = (): void => {
      const count = reg.inFlightCallbackCount();
      observedCounts.push(count);
      if (count === 0) idleArms++;
    };
    const unsubscribe = reg.subscribeCallbacks(onCallbackChange);
    try {
      const first = reg.reserveCallback({ threadId: "t", turnId: "u", callId: "first", fingerprint: "fp" });
      const second = reg.reserveCallback({ threadId: "t", turnId: "u", callId: "second", fingerprint: "fp" });
      assert.equal(first.kind, "admitted");
      assert.equal(second.kind, "admitted");
      if (first.kind !== "admitted" || second.kind !== "admitted") return;
      assert.equal(reg.inFlightCallbackCount(), 2, "both callbacks are admitted before either settles");
      assert.deepEqual(observedCounts, [1, 2]);
      assert.equal(idleArms, 0);

      reg.settleCallback(first.token, "ok");
      assert.equal(reg.inFlightCallbackCount(), 1);
      assert.deepEqual(observedCounts, [1, 2, 1]);
      assert.equal(idleArms, 0, "first settlement cannot rearm idle");

      reg.settleCallback(second.token, "ok");
      assert.equal(reg.inFlightCallbackCount(), 0);
      assert.deepEqual(observedCounts, [1, 2, 1, 0]);
      assert.equal(idleArms, 1, "final settlement rearms idle");
    } finally {
      unsubscribe();
    }
  });
});

describe("ExecutionRegistry: callback reservation ceiling (fail-closed bound)", () => {
  it("poisons+denies a NEW distinct key at the ceiling, yet a replay of an existing key past the ceiling still returns its cached marker without poisoning again", () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    // Fill the table to EXACTLY the ceiling with distinct keys, settling the first so
    // a replay of it can be exercised past the ceiling.
    for (let i = 0; i < MAX_CALLBACK_RESERVATIONS; i++) {
      const res = reg.reserveCallback({ threadId: "t", turnId: "u", callId: `c${i}`, fingerprint: "fp" });
      assert.equal(res.kind, "admitted");
      if (i === 0 && res.kind === "admitted") reg.settleCallback(res.token, "ok");
    }
    assert.equal(reg.inFlightCallbackCount(), MAX_CALLBACK_RESERVATIONS - 1); // one settled
    assert.equal(reg.state(), "open");

    // A NEW distinct key beyond the ceiling fails closed: poison + deny.
    const overflow = reg.reserveCallback({ threadId: "t", turnId: "u", callId: "overflow", fingerprint: "fp" });
    assert.equal(overflow.kind, "denied");
    if (overflow.kind === "denied") assert.equal(overflow.reason, "reservation_ceiling");
    assert.equal(reg.state(), "poisoned");
    const poisonCountAfterOverflow = reg.poisonErrors().length;
    assert.ok(poisonCountAfterOverflow >= 1);

    // A replay of the already-settled key #0 — now past the ceiling AND with the
    // epoch already poisoned — still returns its cached terminal marker (replays are
    // idempotent reads that never count against the ceiling) and does NOT poison a
    // second time.
    const replay = reg.reserveCallback({ threadId: "t", turnId: "u", callId: "c0", fingerprint: "fp" });
    assert.equal(replay.kind, "replay");
    if (replay.kind === "replay") assert.equal(replay.marker.outcome, "ok");
    assert.equal(reg.poisonErrors().length, poisonCountAfterOverflow); // no second poison
  });
});

describe("ExecutionRegistry: changed-fingerprint reuse flood stays bounded (availability)", () => {
  it("denies every changed-fingerprint reuse of one tuple, poisons once, and keeps poisonErrors() bounded (not linear in call count)", () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const tuple = { threadId: "t1", turnId: "u1", callId: "c1" };
    // Reserve the tuple ONCE with a baseline fingerprint.
    assert.equal(reg.reserveCallback({ ...tuple, fingerprint: "fp-0" }).kind, "admitted");

    // The audit's attack: flood the SAME (threadId,turnId,callId) tuple with ever-
    // varying fingerprints. Each is a changed_reuse: the first poisons, and every
    // subsequent one must deny WITHOUT re-entering poison(), so the poison list can
    // never grow linearly in the number of calls.
    const FLOOD = 5000;
    for (let i = 1; i <= FLOOD; i++) {
      const res = reg.reserveCallback({ ...tuple, fingerprint: `fp-${i}` });
      // (a) every reuse is denied changed_reuse.
      assert.equal(res.kind, "denied");
      if (res.kind === "denied") assert.equal(res.reason, "changed_reuse");
    }

    // (b) the registry is (and stays) poisoned.
    assert.equal(reg.isPoisoned(), true);
    assert.equal(reg.state(), "poisoned");

    // (c) the poison accumulator stayed BOUNDED — at most MAX_POISON_ERRORS, and in
    // particular NOT linear in FLOOD (the pre-fix regression measured one entry per
    // call). With the idempotent changed_reuse branch it is exactly one here.
    assert.ok(reg.poisonErrors().length <= MAX_POISON_ERRORS);
    assert.ok(reg.poisonErrors().length < FLOOD);
    assert.equal(reg.poisonErrors().length, 1);
  });

  it("caps poisonErrors() at MAX_POISON_ERRORS even under many DISTINCT poison() calls", () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    // A backstop independent of any single caller: poison() called far more than the
    // cap retains only the FIRST MAX_POISON_ERRORS reasons and never grows past it.
    for (let i = 0; i < MAX_POISON_ERRORS * 10; i++) {
      reg.poison({ category: "protocol", message: `poison-${i}` });
    }
    assert.equal(reg.isPoisoned(), true);
    assert.equal(reg.poisonErrors().length, MAX_POISON_ERRORS);
    // The retained reasons are the FIRST ones (most diagnostic), not the last.
    const retained = reg.poisonErrors();
    assert.equal(retained[0]?.message, "poison-0");
    assert.equal(retained[MAX_POISON_ERRORS - 1]?.message, `poison-${MAX_POISON_ERRORS - 1}`);
  });
});

describe("ExecutionRegistry: idempotent re-quiesce re-asserts emptiness", () => {
  it("a re-quiesce on a still-clean closed registry stays quiescent", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(2));
    registerRoot(reg, new FakeRoot("provider"));
    assert.equal((await reg.quiesceChildren(DEADLINE)).kind, "quiescent");
    const again = await reg.quiesceChildren(DEADLINE);
    assert.equal(again.kind, "quiescent");
    if (again.kind === "quiescent") assert.equal(again.epoch, 2);
  });

  it("re-quiesce reports incomplete+poison when a post-close boundary_action reservation is left unsettled", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(2));
    assert.equal((await reg.quiesceChildren(DEADLINE)).kind, "quiescent");
    assert.equal(reg.state(), "closed");
    // The trusted post-close lane admits a boundary_action reservation; leave it
    // unsettled (a checkpoint/git action that reserved but never registered/cancelled).
    const lane = reg.reserveLaunch("boundary_action");
    assert.equal(lane.kind, "reserved");
    assert.equal(reg.pendingLaunchCount(), 1);
    // The second quiesce MUST re-assert emptiness, not blindly report quiescent.
    const again = await reg.quiesceChildren(DEADLINE);
    assert.equal(again.kind, "incomplete");
    if (again.kind === "incomplete") assert.ok(again.errors.length >= 1);
    assert.equal(reg.state(), "poisoned");
    assert.equal(reg.isPoisoned(), true);
  });
});

describe("ExecutionRegistry: disposal and poison stickiness", () => {
  it("disposes every root once and is idempotent on a second call", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const a = new FakeRoot("provider");
    const b = new FakeRoot("command");
    registerRoot(reg, a);
    registerRoot(reg, b);
    const first = await reg.disposeTools(DEADLINE);
    assert.equal(first.kind, "disposed");
    assert.equal(reg.state(), "disposed");
    const second = await reg.disposeTools(DEADLINE);
    assert.equal(second.kind, "disposed");
    assert.equal(a.disposeCalls, 1);
    assert.equal(b.disposeCalls, 1);
  });

  it("a failing root dispose is incomplete and poisons", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    registerRoot(
      reg,
      new FakeRoot("provider", undefined, async () => {
        throw new Error("dispose boom");
      }),
    );
    const res = await reg.disposeTools(DEADLINE);
    assert.equal(res.kind, "incomplete");
    assert.equal(reg.state(), "poisoned");
  });

  it("retries one failed root disposal without clearing poison evidence", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    let calls = 0;
    registerRoot(
      reg,
      new FakeRoot("provider", async () => ({ ok: true }), async () => {
        calls += 1;
        if (calls === 1) throw new Error("first disposal failed");
      }),
    );

    const first = await reg.disposeTools(DEADLINE);
    assert.equal(first.kind, "incomplete");
    assert.equal(reg.isPoisoned(), true);
    const second = await reg.disposeTools(DEADLINE);
    assert.equal(second.kind, "disposed");
    assert.equal(calls, 2, "only the previously failed root is retried once");
    assert.equal(reg.isPoisoned(), true, "successful retry does not erase prior poison evidence");
  });

  it("isPoisoned() is sticky and survives disposal masking state() as 'disposed'", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    assert.equal(reg.isPoisoned(), false);
    reg.poison({ category: "unknown", message: "poison before disposal" });
    assert.equal(reg.isPoisoned(), true);
    assert.equal(reg.state(), "poisoned");
    const res = await reg.disposeTools(DEADLINE);
    assert.equal(res.kind, "disposed");
    // Disposal masks the enum: state() no longer reports "poisoned"...
    assert.equal(reg.state(), "disposed");
    // ...but the sticky witness stays true so a poisoned-then-disposed epoch can
    // never be mistaken for clean.
    assert.equal(reg.isPoisoned(), true);
  });

  it("poison is sticky and forces incomplete from quiesce and reap", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    reg.poison({ category: "unknown", message: "manual poison" });
    assert.equal(reg.state(), "poisoned");
    // A second poison keeps it poisoned and accumulates evidence.
    reg.poison({ category: "unknown", message: "second poison" });
    assert.equal(reg.state(), "poisoned");
    assert.ok(reg.poisonErrors().length >= 2);
    assert.equal((await reg.quiesceChildren(DEADLINE)).kind, "incomplete");
    assert.equal((await reg.reapProcesses(DEADLINE, 1)).kind, "incomplete");
  });
});

// Issue #1766 (M3a): settleForCapture, the repeat-until-settled drain a vault-locked deferral
// runs before a credential-free capture. Every root is a FakeRoot whose reap outcome the test
// controls; a dispose call is never counted as a reap.
describe("ExecutionRegistry: settleForCapture (issue #1766)", () => {
  const POLL = 10;
  const cb = (callId: string) => ({ threadId: "t", turnId: "u", callId, fingerprint: `fp-${callId}` });
  const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

  it("(a) reaps a root a pre-poison reservation registers DURING the drain, then waits for the callback that settles after that reap → observed_empty", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const launch = reg.reserveLaunch("command");
    assert.equal(launch.kind, "reserved");
    const admitted = reg.reserveCallback(cb("c1"));
    assert.equal(admitted.kind, "admitted");
    const order: string[] = [];
    const root = new FakeRoot("command", async () => {
      order.push("reaped");
      // The callback's reply is published only after its root reaped.
      setTimeout(() => {
        order.push("callback-settled");
        if (admitted.kind === "admitted") reg.settleCallback(admitted.token, "ok");
      }, 15);
      return { ok: true };
    });
    let done = false;
    const settle = reg.settleForCapture(2000, POLL).then((r) => {
      done = true;
      return r;
    });
    assert.equal(reg.state(), "poisoned", "poisoned synchronously on entry");
    await sleep(30);
    assert.equal(done, false, "an unregistered pre-poison reservation keeps it draining");
    // registerRoot does not check state: the pre-poison reservation still lands a root.
    if (launch.kind === "reserved") assert.deepEqual(reg.registerRoot(launch.reservation, root), { ok: true });
    const result = await settle;
    assert.deepEqual(result, { kind: "observed_empty" });
    assert.deepEqual(order, ["reaped", "callback-settled"]);
    assert.equal(root.reapCalls, 1);
    assert.equal(root.disposeCalls, 0, "settle reaps; it never disposes");
  });

  it("(b) an unresolved reservation that never registers is incomplete at the deadline, never empty", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    assert.equal(reg.reserveLaunch("provider").kind, "reserved");
    const result = await reg.settleForCapture(80, POLL);
    assert.equal(result.kind, "incomplete");
    if (result.kind === "incomplete") {
      assert.ok(result.errors.some((e) => /1 launch reservation\(s\) never settled/.test(e.message)));
    }
  });

  it("(c) a surviving writer whose root reap is unclean → incomplete; the root stays unreaped and is not retried", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const survivor = new FakeRoot("command", failReap);
    const clean = new FakeRoot("provider");
    registerRoot(reg, survivor);
    registerRoot(reg, clean);
    const result = await reg.settleForCapture(500, POLL);
    assert.equal(result.kind, "incomplete");
    if (result.kind === "incomplete") {
      assert.ok(result.errors.some((e) => /1 root\(s\) not cleanly reaped/.test(e.message)));
    }
    assert.equal(survivor.reapCalls, 1);
    assert.equal(clean.reapCalls, 1);
    assert.equal(survivor.disposeCalls, 0);
    assert.equal(reg.hasLiveCommandRoot(), true, "the unclean root is still counted as live");
  });

  it("(d) a callback that never settles → incomplete", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    assert.equal(reg.reserveCallback(cb("stuck")).kind, "admitted");
    registerRoot(reg, new FakeRoot("provider"));
    const result = await reg.settleForCapture(60, POLL);
    assert.equal(result.kind, "incomplete");
    if (result.kind === "incomplete") {
      assert.ok(result.errors.some((e) => /1 callback\/child-turn reservation\(s\) unsettled/.test(e.message)));
    }
  });

  it("(e) a disposed registry → incomplete (its cleared tables would read falsely empty)", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    assert.equal(reg.reserveCallback(cb("x")).kind, "admitted");
    assert.equal(reg.reserveLaunch("provider").kind, "reserved");
    assert.deepEqual(await reg.disposeTools(DEADLINE), { kind: "disposed" });
    assert.equal(reg.pendingLaunchCount(), 0);
    assert.equal(reg.inFlightCallbackCount(), 0);
    const result = await reg.settleForCapture(500, POLL);
    assert.equal(result.kind, "incomplete");
  });

  it("(f) an open registry is poisoned first; every further admission is denied", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    assert.equal(reg.state(), "open");
    const result = await reg.settleForCapture(500, POLL);
    assert.deepEqual(result, { kind: "observed_empty" });
    assert.equal(reg.state(), "poisoned");
    assert.ok(reg.poisonErrors().some((e) => e.message === "codex vault deferral: settling for capture"));
    assert.deepEqual(reg.reserveLaunch("provider"), { kind: "denied", reason: "admission_closed" });
    assert.deepEqual(reg.reserveLaunch("boundary_action"), { kind: "denied", reason: "admission_closed" });
    assert.deepEqual(reg.reserveCallback(cb("late")), { kind: "denied", reason: "admission_closed" });
    // A closed registry is poisoned too (no new state is introduced).
    const closed = new ExecutionRegistry(newLocalExecutionEpoch(2));
    assert.equal((await closed.quiesceChildren(DEADLINE)).kind, "quiescent");
    assert.deepEqual(await closed.settleForCapture(500, POLL), { kind: "observed_empty" });
    assert.equal(closed.state(), "poisoned");
  });

  it("(g) the whole loop respects ONE deadline, even across a hung root reap and polling", async () => {
    const deadline = 120;
    // A pending reservation keeps it polling until the deadline.
    const polling = new ExecutionRegistry(newLocalExecutionEpoch(1));
    assert.equal(polling.reserveLaunch("command").kind, "reserved");
    let started = Date.now();
    assert.equal((await polling.settleForCapture(deadline, 50)).kind, "incomplete");
    let elapsed = Date.now() - started;
    assert.ok(elapsed >= deadline - 5, `returned early: ${elapsed}ms`);
    assert.ok(elapsed <= deadline + 50 + 40, `overran the deadline by more than one poll: ${elapsed}ms`);

    // A root whose reap never resolves cannot stretch it either.
    const hung = new ExecutionRegistry(newLocalExecutionEpoch(1));
    registerRoot(hung, new FakeRoot("provider", () => new Promise<ReapOutcome>(() => {})));
    started = Date.now();
    assert.equal((await hung.settleForCapture(deadline, POLL)).kind, "incomplete");
    elapsed = Date.now() - started;
    assert.ok(elapsed <= deadline + POLL + 40, `a hung reap overran the deadline: ${elapsed}ms`);
  });
});
