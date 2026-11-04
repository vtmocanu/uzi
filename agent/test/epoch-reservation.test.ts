import { it } from "node:test";
import assert from "node:assert/strict";
import { RunRunner } from "../src/runner.js";

function fixture() {
  const state = {
    executionTails: new Map<string, Promise<void>>(),
    inventoryReservations: new Map<string, Promise<void>>(),
    activeRuns: new Map<string, unknown>(),
    snapshotRegistry: new Map<string, unknown>(),
  };
  const reserve = <T>(action: (eligible: () => boolean) => Promise<T>) =>
    RunRunner.prototype.withSourceReservation.call(state as unknown as RunRunner, "run", action);
  return { state, reserve };
}
for (const replacement of ["inventory", "tail", "active", "snapshot"] as const)
  it(`reservation revocation is sticky for ${replacement} ownership`, async () => {
    const { state, reserve } = fixture();
    const successor = Promise.resolve();
    await reserve(async eligible => {
      assert.equal(eligible(), true);
      const old = state.inventoryReservations.get("run")!;
      if (replacement === "inventory") state.inventoryReservations.set("run", successor);
      if (replacement === "tail") state.executionTails.set("run", successor);
      if (replacement === "active") state.activeRuns.set("run", {});
      if (replacement === "snapshot") state.snapshotRegistry.set("run", {});
      assert.equal(eligible(), false);
      state.activeRuns.clear(); state.snapshotRegistry.clear();
      if (replacement === "inventory") state.inventoryReservations.set("run", old);
      assert.equal(eligible(), false);
      if (replacement === "inventory") {
        state.inventoryReservations.set("run", successor);
        state.executionTails.set("run", successor);
      }
    });
    assert.equal(state.inventoryReservations.has("run"), replacement === "inventory");
    assert.equal(state.executionTails.has("run"), replacement === "tail" || replacement === "inventory");
  });
for (const occupied of ["tail", "active", "snapshot"] as const)
  it(`refuses initially occupied ${occupied}`, async () => {
    const { state, reserve } = fixture();
    if (occupied === "tail") state.executionTails.set("run", Promise.resolve());
    if (occupied === "active") state.activeRuns.set("run", {});
    if (occupied === "snapshot") state.snapshotRegistry.set("run", {});
    let entered = false;
    await reserve(async () => { entered = true; });
    assert.equal(entered, false);
  });
