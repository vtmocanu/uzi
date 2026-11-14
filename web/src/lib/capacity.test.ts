import { describe, expect, it } from "vitest";
import { capacitySlots, rollNote, workerTakesNoNewRuns } from "./capacity";
import { aWorker } from "../pages/overviewFixtures";

const online = (active: number, cap: number | null, over = {}) =>
  aWorker({ status: "online", active_runs: active, max_concurrent_runs: cap, ...over });

describe("capacitySlots", () => {
  it("counts in-use and open slots for plain online workers", () => {
    expect(capacitySlots([online(2, 2), online(1, 4)])).toEqual({
      inUse: 3, closed: 0, open: 3, total: 6, overCap: 0, excludedOffline: 0, excludedHibernated: 0, excludedUnknownCap: 0,
    });
  });

  it("closes the spare slots of a draining worker", () => {
    const s = capacitySlots([online(1, 2, { draining_since: "2026-07-05T12:00:00Z" })]);
    expect(s).toMatchObject({ inUse: 1, closed: 1, open: 0, total: 2 });
  });

  it("closes spare slots for a manual cordon and for an effective cordon", () => {
    expect(capacitySlots([online(0, 1, { manual_cordoned_at: "2026-07-05T12:00:00Z" })])).toMatchObject({ closed: 1, open: 0 });
    expect(capacitySlots([online(0, 3, { effective_cordoned: true })])).toMatchObject({ closed: 3, open: 0 });
  });

  it("closes spare slots for a hosted upgrade but not an external one", () => {
    expect(capacitySlots([online(0, 2, { kind: "hosted", upgrade_status: "upgrading" })])).toMatchObject({ closed: 2, open: 0 });
    expect(capacitySlots([online(0, 2, { kind: "external", upgrade_status: "upgrading" })])).toMatchObject({ closed: 0, open: 2 });
  });

  it("excludes an offline worker whatever its cap", () => {
    const s = capacitySlots([aWorker({ status: "offline", active_runs: 1, max_concurrent_runs: 4 })]);
    expect(s).toEqual({ inUse: 0, closed: 0, open: 0, total: 0, overCap: 0, excludedOffline: 1, excludedHibernated: 0, excludedUnknownCap: 0 });
  });

  it("excludes hibernated and waking workers from capacity and counts them apart from offline", () => {
    const s = capacitySlots([
      online(0, 4, { status: "offline", hibernation_state: "hibernated" }),
      online(0, 3, { status: "online", hibernation_state: "waking" }),
      online(1, 2),
    ]);
    expect(s).toEqual({ inUse: 1, closed: 0, open: 1, total: 2, overCap: 0, excludedOffline: 0, excludedHibernated: 2, excludedUnknownCap: 0 });
  });

  it("closes the spare slots of a worker that is still preparing to hibernate", () => {
    const s = capacitySlots([online(0, 2, { hibernation_state: "preparing" })]);
    expect(s).toMatchObject({ closed: 2, open: 0, total: 2, excludedHibernated: 0 });
    expect(workerTakesNoNewRuns(online(0, 2, { hibernation_state: "preparing" }))).toBe(true);
  });

  it("reports runs beyond the cap as over-cap, never as a segment", () => {
    const s = capacitySlots([online(5, 2)]);
    expect(s).toMatchObject({ inUse: 2, open: 0, closed: 0, total: 2, overCap: 3 });
  });

  it("excludes an online worker with a null, non-finite or negative cap", () => {
    const s = capacitySlots([online(1, null), online(1, Number.NaN), online(1, -2), online(1, Infinity)]);
    expect(s).toEqual({ inUse: 0, closed: 0, open: 0, total: 0, overCap: 0, excludedOffline: 0, excludedHibernated: 0, excludedUnknownCap: 4 });
  });

  it("ignores dedicated cross-check lanes but counts legacy ones already inside active_runs", () => {
    const dedicated = online(1, 2, { active_cross_checks: 2, max_cross_check_slots: 4 });
    expect(capacitySlots([dedicated])).toMatchObject({ inUse: 1, open: 1, total: 2 });
    // A legacy cross-check shares the run lane: it is already part of active_runs.
    const legacy = online(2, 2, { active_cross_checks: 1, max_cross_check_slots: null });
    expect(capacitySlots([legacy])).toMatchObject({ inUse: 2, open: 0, total: 2 });
  });

  it("is all zero for no workers", () => {
    expect(capacitySlots([])).toEqual({ inUse: 0, closed: 0, open: 0, total: 0, overCap: 0, excludedOffline: 0, excludedHibernated: 0, excludedUnknownCap: 0 });
  });

  it("never yields a negative figure, even for negative active_runs", () => {
    const s = capacitySlots([online(-3, 2)]);
    expect(s).toMatchObject({ inUse: 0, open: 2, total: 2, overCap: 0 });
    for (const v of Object.values(s)) expect(v).toBeGreaterThanOrEqual(0);
  });

  it("treats absent cordon fields as not cordoned", () => {
    const w = online(0, 1);
    expect(w.manual_cordoned_at).toBeUndefined();
    expect(w.effective_cordoned).toBeUndefined();
    expect(workerTakesNoNewRuns(w)).toBe(false);
    expect(capacitySlots([w])).toMatchObject({ open: 1, closed: 0 });
  });
});

describe("rollNote", () => {
  const rolling = { kind: "hosted" as const, upgrade_status: "upgrading" as const, upgrade_target: "v0.86.1", draining_since: "2026-07-05T12:00:00Z" };

  it("names the target only with hosted, upgrading, a target and a drain", () => {
    expect(rollNote(aWorker(rolling))).toBe("Rolls to v0.86.1 once fully idle");
  });

  it("is null for a manual cordon alone", () => {
    expect(rollNote(aWorker({ manual_cordoned_at: "2026-07-05T12:00:00Z", effective_cordoned: true }))).toBeNull();
  });

  it("is null when only outdated, for an external worker, with a blank target, or without a drain", () => {
    expect(rollNote(aWorker({ ...rolling, upgrade_status: "outdated" }))).toBeNull();
    expect(rollNote(aWorker({ ...rolling, kind: "external" }))).toBeNull();
    expect(rollNote(aWorker({ ...rolling, upgrade_target: "  " }))).toBeNull();
    expect(rollNote(aWorker({ ...rolling, upgrade_target: "‮​" }))).toBeNull();
    expect(rollNote(aWorker({ ...rolling, draining_since: null }))).toBeNull();
  });

  it("sanitizes a hostile target", () => {
    expect(rollNote(aWorker({ ...rolling, upgrade_target: "v1‮<script>" }))).toBe("Rolls to v1<script> once fully idle");
  });
});
