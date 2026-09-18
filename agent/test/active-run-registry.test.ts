import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { PendingTerminal } from "../src/outbox.js";

// PRD #1390 M2a / #1391 Run B M4 — the worker-side active-run registry: the source of every
// ActiveSnapshot the worker sends on the heartbeat, the run-lane claim, and the register request.
// These prove the snapshot shape (terminal_pending / pending_overflow), the real pending-terminal
// producer + its deterministic overflow rotation, the ONE monotonic epoch counter shared by every
// build site, and the production claim gate.

const RUN_A = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const RUN_B = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";

/** Build a PendingTerminal fixture. `since` orders the round-robin (oldest-first); `blocked`
 *  promotes an entry into the fixed listing slots. */
function pt(
  run_id: string,
  claim_generation: number,
  opts: { blocked?: boolean; since?: number } = {},
): PendingTerminal {
  return {
    run_id,
    claim_generation,
    phase: "running",
    blocked: opts.blocked ?? false,
    since: opts.since ?? 1,
  };
}

describe("ActiveRunRegistry — base snapshot shape (PRD #1390 M2a)", () => {
  it("with NO pending terminals, every entry is terminal_pending:false and pending_overflow:false", () => {
    // A #1390 worker (no pending-lister wired) never journals a terminal outcome, so this is the
    // pre-#1391 shape — the invariant M4 preserves for an ordinary run.
    const reg = new ActiveRunRegistry();
    reg.add(RUN_A, 3);
    reg.add(RUN_B, 7);

    const snap = reg.build();
    assert.strictEqual(snap.pending_overflow, false, "no pending terminal ⇒ no overflow");
    assert.strictEqual(snap.active.length, 2);
    for (const e of snap.active) {
      assert.strictEqual(e.terminal_pending, false, "no pending terminal ⇒ every entry terminal_pending:false");
    }
    const a = snap.active.find((e) => e.run_id === RUN_A);
    const b = snap.active.find((e) => e.run_id === RUN_B);
    assert.deepStrictEqual(a, { run_id: RUN_A, claim_generation: 3, phase: "running", terminal_pending: false });
    assert.deepStrictEqual(b, { run_id: RUN_B, claim_generation: 7, phase: "running", terminal_pending: false });
  });

  it("a freshly-added run starts at `running`; the epoch begins at 1", () => {
    const reg = new ActiveRunRegistry();
    reg.add(RUN_A, 1);
    const snap = reg.build();
    assert.strictEqual(snap.snapshot_epoch, 1, "the first build is epoch 1");
    assert.strictEqual(snap.active[0]!.phase, "running");
  });

  it("epoch is monotonic across INTERLEAVED builds (the heartbeat loop + the claim loop draw from ONE counter)", () => {
    const reg = new ActiveRunRegistry();
    reg.add(RUN_A, 1);
    const epochs = [reg.build(), reg.build(), reg.build(), reg.build()].map((s) => s.snapshot_epoch);
    assert.deepStrictEqual(epochs, [1, 2, 3, 4], "one shared monotonic counter, no gaps, no repeats");
    for (let i = 1; i < epochs.length; i++) {
      assert.ok(epochs[i]! > epochs[i - 1]!, "strictly increasing");
    }
  });

  it("a parked run reports its HELD phase, and resuming restores `running`", () => {
    const reg = new ActiveRunRegistry();
    reg.add(RUN_A, 5);

    reg.setPhase(RUN_A, "awaiting_approval");
    assert.strictEqual(reg.build().active[0]!.phase, "awaiting_approval", "the plan-gate park is reported");

    reg.setPhase(RUN_A, "awaiting_input");
    assert.strictEqual(reg.build().active[0]!.phase, "awaiting_input", "the question park is reported");

    reg.setPhase(RUN_A, "awaiting_followup");
    assert.strictEqual(reg.build().active[0]!.phase, "awaiting_followup", "the interactive followup park is reported");

    reg.setPhase(RUN_A, "running");
    assert.strictEqual(reg.build().active[0]!.phase, "running", "resuming restores running");
  });

  it("setPhase on an UNREGISTERED run is a no-op (a late transition after removal never revives an entry)", () => {
    const reg = new ActiveRunRegistry();
    reg.setPhase(RUN_A, "awaiting_approval");
    assert.strictEqual(reg.build().active.length, 0, "no entry was created");
    assert.strictEqual(reg.size, 0);
  });

  it("remove drops a run so it stops being listed (a terminal / requeued run leaves the snapshot)", () => {
    const reg = new ActiveRunRegistry();
    reg.add(RUN_A, 1);
    reg.add(RUN_B, 2);
    assert.strictEqual(reg.size, 2);

    reg.remove(RUN_A);
    const snap = reg.build();
    assert.strictEqual(snap.active.length, 1);
    assert.strictEqual(snap.active[0]!.run_id, RUN_B, "only the still-live run remains");
    assert.strictEqual(reg.size, 1);
  });

  it("re-adding an existing run id resets it to `running` at the new generation (a promoted re-claim)", () => {
    const reg = new ActiveRunRegistry();
    reg.add(RUN_A, 1);
    reg.setPhase(RUN_A, "awaiting_approval");
    reg.add(RUN_A, 2);
    const e = reg.build().active[0]!;
    assert.strictEqual(e.claim_generation, 2, "the new generation replaces the old");
    assert.strictEqual(e.phase, "running", "the re-claim resets to running");
    assert.strictEqual(reg.size, 1, "still exactly one entry for the run");
  });

  it("an empty registry builds an empty snapshot (still a valid, epoch-bearing snapshot)", () => {
    const reg = new ActiveRunRegistry();
    const snap = reg.build();
    assert.deepStrictEqual(snap.active, []);
    assert.strictEqual(snap.pending_overflow, false);
    assert.strictEqual(snap.snapshot_epoch, 1);
  });
});

describe("ActiveRunRegistry — pending terminals (PRD #1391 Run B M4)", () => {
  it("lists a pending run as terminal_pending:true, using the journal's generation + phase", () => {
    let pending: PendingTerminal[] = [pt(RUN_A, 4)];
    const reg = new ActiveRunRegistry(() => pending, () => 32);
    const snap = reg.build();
    assert.strictEqual(snap.pending_overflow, false, "one pending, well under the cap ⇒ no overflow");
    assert.deepStrictEqual(snap.active, [
      { run_id: RUN_A, claim_generation: 4, phase: "running", terminal_pending: true },
    ]);
    pending = [];
    assert.deepStrictEqual(reg.build().active, [], "once the journal retires, the run leaves the snapshot");
  });

  it("pending_overflow is set when pendingCount exceeds the cap", () => {
    const pending = [pt(RUN_A, 1, { since: 1 }), pt(RUN_B, 1, { since: 2 })];
    // Cap 1: two pending > 1 ⇒ overflow, and only ONE pending entry is ever listed (rotating).
    const reg = new ActiveRunRegistry(() => pending, () => 1);
    const snap = reg.build();
    assert.strictEqual(snap.pending_overflow, true);
    assert.strictEqual(snap.active.filter((e) => e.terminal_pending).length, 1, "cap 1 ⇒ at most one pending listed");
    assert.strictEqual(reg.claimsPausedByPendingOverflow(), true, "the production claim gate agrees with build()");
  });

  it("cap 0 / undefined: an EMPTY pending subset + overflow (no snapshot rejection, cap-independent floor)", () => {
    const pending = [pt(RUN_A, 1), pt(RUN_B, 1)];
    for (const cap of [undefined, 0] as const) {
      const reg = new ActiveRunRegistry(() => pending, () => cap);
      const snap = reg.build();
      assert.strictEqual(
        snap.active.filter((e) => e.terminal_pending).length,
        0,
        `cap ${cap}: NO pending entry may be listed (a non-empty subset would be rejected whole)`,
      );
      assert.strictEqual(snap.pending_overflow, true, `cap ${cap}: overflow whenever anything is pending`);
      assert.strictEqual(reg.claimsPausedByPendingOverflow(), true, `cap ${cap}: the claim gate stays closed`);
    }
  });

  it("no pending ⇒ never overflow, and the claim gate stays open, regardless of cap", () => {
    const reg = new ActiveRunRegistry(() => [], () => 0);
    assert.strictEqual(reg.build().pending_overflow, false);
    assert.strictEqual(reg.claimsPausedByPendingOverflow(), false);
  });

  it("the register snapshot is an EMPTY subset + overflow whenever anything is pending", () => {
    let pending: PendingTerminal[] = [pt(RUN_A, 2), pt(RUN_B, 3)];
    const reg = new ActiveRunRegistry(() => pending, () => 32);
    const snap = reg.buildRegisterSnapshot();
    assert.deepStrictEqual(snap.active, [], "the register snapshot never lists a subset (cap-independent)");
    assert.strictEqual(snap.pending_overflow, true, "pending outcomes ⇒ overflow so the api leases them all");
    assert.ok(snap.snapshot_epoch >= 1, "draws the shared monotonic epoch");
    pending = [];
    assert.strictEqual(reg.buildRegisterSnapshot().pending_overflow, false, "no pending ⇒ no overflow");
  });

  it("live entries and pending entries coexist; the cap governs only the pending subset", () => {
    const pending = [pt(RUN_B, 9)];
    const reg = new ActiveRunRegistry(() => pending, () => 32);
    reg.add(RUN_A, 1); // a live execution
    const snap = reg.build();
    const a = snap.active.find((e) => e.run_id === RUN_A);
    const b = snap.active.find((e) => e.run_id === RUN_B);
    assert.deepStrictEqual(a, { run_id: RUN_A, claim_generation: 1, phase: "running", terminal_pending: false });
    assert.deepStrictEqual(b, { run_id: RUN_B, claim_generation: 9, phase: "running", terminal_pending: true });
  });
});

describe("ActiveRunRegistry — deterministic overflow rotation (PRD #1391 Run B M4)", () => {
  it("blocked-first fills cap-1, the last slot round-robins the omitted oldest-first, never more than cap", () => {
    // 5 pending, cap 3: 2 blocked (A,B) fill the fixed cap-1=2 slots; the last slot round-robins over
    // the 3 omitted (C,D,E) oldest-first. Every pending run is leased within omitted+1 = 4 builds.
    const A = "a0000000-0000-0000-0000-000000000000";
    const B = "b0000000-0000-0000-0000-000000000000";
    const C = "c0000000-0000-0000-0000-000000000000";
    const D = "d0000000-0000-0000-0000-000000000000";
    const E = "e0000000-0000-0000-0000-000000000000";
    const pending = [
      pt(C, 1, { since: 3 }),
      pt(A, 1, { blocked: true, since: 1 }),
      pt(E, 1, { since: 5 }),
      pt(B, 1, { blocked: true, since: 2 }),
      pt(D, 1, { since: 4 }),
    ];
    const reg = new ActiveRunRegistry(() => pending, () => 3);

    const listedPerBuild: string[][] = [];
    for (let i = 0; i < 4; i++) {
      const snap = reg.build();
      assert.strictEqual(snap.pending_overflow, true, "5 > cap 3 ⇒ overflow");
      const ids = snap.active.filter((e) => e.terminal_pending).map((e) => e.run_id);
      assert.ok(ids.length <= 3, "never more than cap pending entries listed");
      assert.strictEqual(ids.length, 3, "the fixed cap-1 + the one rotating slot");
      assert.ok(ids.includes(A) && ids.includes(B), "the two blocked journals always fill the fixed slots");
      listedPerBuild.push(ids);
    }
    // The rotating last slot walks the omitted oldest-first: C, D, E, then back to C.
    const rotating = listedPerBuild.map((ids) => ids.find((id) => id !== A && id !== B)!);
    assert.deepStrictEqual(rotating, [C, D, E, C], "round-robin over the omitted, oldest-first, wrapping");

    // Every one of the 5 pending runs was listed within omitted+1 = 4 builds.
    const everListed = new Set(listedPerBuild.flat());
    assert.deepStrictEqual([...everListed].sort(), [A, B, C, D, E].sort(), "all pending leased within omitted+1 builds");
  });

  it("cap 1: the single slot round-robins over EVERY pending entry oldest-first", () => {
    const X = "10000000-0000-0000-0000-000000000000";
    const Y = "20000000-0000-0000-0000-000000000000";
    const Z = "30000000-0000-0000-0000-000000000000";
    const pending = [pt(Z, 1, { since: 3 }), pt(X, 1, { since: 1 }), pt(Y, 1, { since: 2 })];
    const reg = new ActiveRunRegistry(() => pending, () => 1);
    const picks: string[] = [];
    for (let i = 0; i < 3; i++) {
      const listed = reg.build().active.filter((e) => e.terminal_pending).map((e) => e.run_id);
      assert.strictEqual(listed.length, 1, "cap 1 ⇒ exactly one pending listed");
      picks.push(listed[0]!);
    }
    assert.deepStrictEqual(picks, [X, Y, Z], "oldest-first over every pending entry");
  });
});
