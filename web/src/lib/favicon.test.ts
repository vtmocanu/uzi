import { describe, it, expect } from "vitest";
import { deriveFaviconState, failedRunIds, type FaviconRun } from "./favicon";
import type { StopKind } from "./api";

// run builds a minimal FaviconRun (just the id/status/stop_kind the derivation reads).
function run(status: string, over: { id?: string; stop_kind?: StopKind | null } = {}): FaviconRun {
  return { id: over.id ?? `run-${status}`, status, stop_kind: over.stop_kind ?? null };
}

const NONE = new Set<string>(); // empty baseline

describe("deriveFaviconState — the four states in isolation", () => {
  it("idle for no runs and no unread", () => {
    expect(deriveFaviconState([], 0, NONE)).toBe("idle");
  });
  it("failed for a fresh genuine failure", () => {
    expect(deriveFaviconState([run("failed")], 0, NONE)).toBe("failed");
  });
  it("attention for a run awaiting approval", () => {
    expect(deriveFaviconState([run("awaiting_approval")], 0, NONE)).toBe("attention");
  });
  it("running for a run in flight", () => {
    expect(deriveFaviconState([run("running")], 0, NONE)).toBe("running");
  });
});

describe("deriveFaviconState — priority ladder (first match wins)", () => {
  it("a fresh failure beats a concurrent awaiting_approval → failed", () => {
    expect(deriveFaviconState([run("awaiting_approval"), run("failed")], 0, NONE)).toBe("failed");
  });
  it("attention beats a concurrent running → attention", () => {
    expect(deriveFaviconState([run("running"), run("awaiting_approval")], 0, NONE)).toBe("attention");
  });
  it("unread > 0 alone → attention", () => {
    expect(deriveFaviconState([], 3, NONE)).toBe("attention");
  });
  it("unread > 0 outranks a concurrent running → attention", () => {
    expect(deriveFaviconState([run("running")], 1, NONE)).toBe("attention");
  });
});

describe("deriveFaviconState — a deliberate stop is not a failure", () => {
  it("a failed run carrying a stop_kind does not redden", () => {
    expect(deriveFaviconState([run("failed", { stop_kind: "plan_rejected" })], 0, NONE)).toBe("idle");
  });
  it("a cancelled run does not redden", () => {
    expect(deriveFaviconState([run("cancelled")], 0, NONE)).toBe("idle");
  });
});

describe("deriveFaviconState — fresh vs. baseline", () => {
  it("a failed run whose id is in the baseline does not redden", () => {
    const baseline = new Set(["run-failed"]);
    expect(deriveFaviconState([run("failed")], 0, baseline)).toBe("idle");
  });
  it("the same run id NOT in the baseline reddens", () => {
    expect(deriveFaviconState([run("failed")], 0, NONE)).toBe("failed");
  });
  it("a baselined failure still yields attention if another run awaits approval", () => {
    const baseline = new Set(["run-failed"]);
    expect(deriveFaviconState([run("failed"), run("awaiting_approval")], 0, baseline)).toBe("attention");
  });
});

describe("deriveFaviconState — every running status", () => {
  it("queued alone → running", () => {
    expect(deriveFaviconState([run("queued")], 0, NONE)).toBe("running");
  });
  it("claimed alone → running", () => {
    expect(deriveFaviconState([run("claimed")], 0, NONE)).toBe("running");
  });
  it("running alone → running", () => {
    expect(deriveFaviconState([run("running")], 0, NONE)).toBe("running");
  });
  it("a completed run alone → idle", () => {
    expect(deriveFaviconState([run("completed")], 0, NONE)).toBe("idle");
  });
});

describe("failedRunIds", () => {
  it("returns exactly the failed run ids", () => {
    const runs = [
      run("failed", { id: "a" }),
      run("running", { id: "b" }),
      run("failed", { id: "c", stop_kind: "cancelled" }),
      run("completed", { id: "d" }),
    ];
    expect(failedRunIds(runs)).toEqual(new Set(["a", "c"]));
  });
  it("is empty when nothing failed", () => {
    expect(failedRunIds([run("running"), run("completed")])).toEqual(new Set());
  });
});
