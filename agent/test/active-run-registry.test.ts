import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { ActiveRunRegistry } from "../src/active-run-registry.js";

// PRD #1390 M2a — the worker-side active-run registry: the source of every ActiveSnapshot
// the worker sends on the heartbeat and the run-lane claim. These prove the snapshot shape
// (terminal_pending / pending_overflow), the ONE monotonic epoch counter shared by the two
// build sites, and that a parked run reports its held phase.

const RUN_A = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const RUN_B = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";

describe("ActiveRunRegistry (PRD #1390 M2a)", () => {
  it("projects the live registry with terminal_pending:false on every entry and pending_overflow:false", () => {
    const reg = new ActiveRunRegistry();
    reg.add(RUN_A, 3);
    reg.add(RUN_B, 7);

    const snap = reg.build();
    assert.strictEqual(snap.pending_overflow, false, "a #1390 worker never overflows");
    assert.strictEqual(snap.active.length, 2);
    for (const e of snap.active) {
      assert.strictEqual(e.terminal_pending, false, "a #1390 worker journals no terminal outcome");
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
    // Simulate the heartbeat loop and the claim loop each building independently from the
    // same registry: the epochs must strictly increase across both.
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
    // A promoted re-claim (serialised behind the old park) re-registers the same run id.
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
