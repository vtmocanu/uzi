import { describe, expect, it } from "vitest";
import { hibernationCleanupText } from "./workerHibernation";
import type { WorkerHibernationCleanup } from "./apiTypes";

const text = (c: WorkerHibernationCleanup | null | undefined) => hibernationCleanupText({ hibernation_cleanup: c });

describe("hibernationCleanupText (PRD #4206)", () => {
  it("maps each state and outcome to its exact copy", () => {
    expect(text({ state: "settled", outcome: "pruned", reason: null })).toBe("cache pruned");
    expect(text({ state: "settled", outcome: "failed", reason: "interrupted" })).toBe("cleanup interrupted");
    expect(text({ state: "cleaning", outcome: null, reason: null })).toBe("cleaning up");
    expect(text({ state: "unknown", outcome: null, reason: null })).toBe("cleanup settlement unknown");
    expect(text({ state: "settled", outcome: "skipped", reason: "unsupported" })).toBe(
      "cleanup skipped: this worker cannot prune its Docker cache",
    );
  });

  it("an in-progress state wins over a stale outcome", () => {
    expect(text({ state: "unknown", outcome: "skipped", reason: "cancelled" })).toBe("cleanup settlement unknown");
  });

  it("is null without an attempt or outcome", () => {
    expect(text(null)).toBeNull();
    expect(text(undefined)).toBeNull();
    expect(text({ state: "none", outcome: null, reason: null })).toBeNull();
  });

  it("sanitizes an unknown reason and omits an empty one", () => {
    expect(text({ state: "settled", outcome: "skipped", reason: "new‮_code\u0007" })).toBe("cleanup skipped: new_code");
    expect(text({ state: "settled", outcome: "skipped", reason: null })).toBe("cleanup skipped");
  });
});
