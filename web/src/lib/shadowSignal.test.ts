// shadowSignal unit tests (PRD #1167 M4). shadowSignal is the pure derivation the
// Shadow theme keys its data-live / data-attention surfaces off — live when a machine
// is working, attention when a human owes the run a decision or it is in review, else
// null. A NEW file rather than appended to runBadge.test.ts (repo convention when the
// target file already carries findings), covering every arm the switch below folds in.
//
// Runs under the vitest `node` project (src/lib/**) — no DOM needed, it is a pure fn.
import { describe, it, expect } from "vitest";
import { shadowSignal } from "./runBadge";

// A minimal run-like object; shadowSignal reads only these five fields.
type RunLike = {
  status: string;
  is_planning?: boolean;
  is_revising?: boolean;
  mr_iid?: number | null;
  mr_state?: string | null;
};
function run(over: Partial<RunLike> & { status: string }): RunLike {
  return { mr_iid: null, mr_state: null, ...over };
}

describe("shadowSignal", () => {
  it("returns null for a null or undefined run", () => {
    expect(shadowSignal(null)).toBeNull();
    expect(shadowSignal(undefined)).toBeNull();
  });

  describe("live — a machine is working", () => {
    it("claimed is live", () => {
      expect(shadowSignal(run({ status: "claimed" }))).toBe("live");
    });
    it("running is live", () => {
      expect(shadowSignal(run({ status: "running" }))).toBe("live");
    });
    it("planning (running + is_planning) is live — planning IS a running run", () => {
      expect(shadowSignal(run({ status: "running", is_planning: true }))).toBe("live");
    });
    it("a live run with an MR is still live (live wins over the review check)", () => {
      expect(shadowSignal(run({ status: "running", mr_iid: 7, mr_state: "opened" }))).toBe("live");
    });
  });

  describe("attention — a human owes the run a decision, or it is in review", () => {
    it("awaiting_approval is attention", () => {
      expect(shadowSignal(run({ status: "awaiting_approval" }))).toBe("attention");
    });
    it("revising (awaiting_approval + is_revising) is attention", () => {
      expect(
        shadowSignal(run({ status: "awaiting_approval", is_revising: true })),
      ).toBe("attention");
    });
    it("completed with an OPEN mr (null mr_state) is attention — in review", () => {
      expect(shadowSignal(run({ status: "completed", mr_iid: 42, mr_state: null }))).toBe(
        "attention",
      );
    });
    it("completed with an OPEN mr (opened mr_state) is attention", () => {
      expect(
        shadowSignal(run({ status: "completed", mr_iid: 42, mr_state: "opened" })),
      ).toBe("attention");
    });
  });

  describe("null — quiet, terminal, or no longer in review", () => {
    it("completed with a MERGED mr is null", () => {
      expect(shadowSignal(run({ status: "completed", mr_iid: 42, mr_state: "merged" }))).toBeNull();
    });
    it("completed with a CLOSED mr is null", () => {
      expect(shadowSignal(run({ status: "completed", mr_iid: 42, mr_state: "closed" }))).toBeNull();
    });
    it("completed with NO mr is null", () => {
      expect(shadowSignal(run({ status: "completed", mr_iid: null }))).toBeNull();
    });
    it("queued is null", () => {
      expect(shadowSignal(run({ status: "queued" }))).toBeNull();
    });
    it("failed is null", () => {
      expect(shadowSignal(run({ status: "failed" }))).toBeNull();
    });
    it("cancelled is null", () => {
      expect(shadowSignal(run({ status: "cancelled" }))).toBeNull();
    });
    it("limit_wait is null", () => {
      expect(shadowSignal(run({ status: "limit_wait" }))).toBeNull();
    });
    // Pin the deliberate non-attention states from PRD #1167's enumeration: these are
    // human-adjacent waits that shadowSignal intentionally does NOT flag as attention.
    // They currently fall through to null; lock it so a future change is caught.
    it("awaiting_input is null — NOT attention per PRD #1167", () => {
      expect(shadowSignal(run({ status: "awaiting_input" }))).toBeNull();
    });
    it("awaiting_followup is null — NOT attention per PRD #1167", () => {
      expect(shadowSignal(run({ status: "awaiting_followup" }))).toBeNull();
    });
    it("pool_wait is null — NOT attention per PRD #1167", () => {
      expect(shadowSignal(run({ status: "pool_wait" }))).toBeNull();
    });
  });
});
