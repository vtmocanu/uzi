import { describe, it, expect } from "vitest";
import { isTerminalRun, TERMINAL_RUN_STATUSES } from "./runStatus";

// TERMINAL_RUN_STATUSES mirrors the DB CHECK's terminal set. Its membership is a
// negative-space contract every non-terminal status depends on, so each park has an
// explicit absence test rather than trusting the list to stay right.
describe("TERMINAL_RUN_STATUSES", () => {
  it("is exactly the three terminal statuses", () => {
    expect(TERMINAL_RUN_STATUSES).toEqual(["completed", "failed", "cancelled"]);
  });

  // PRD #1190: `paused` is a NON-terminal owner hold (In Progress on the board, resumes on
  // demand), so it must NOT be terminal — a paused run that read as terminal would drop off
  // the active surfaces and never resume. This is the web twin of the worker's
  // TERMINAL_RUN_STATUSES 🔴 note (a park must never join it).
  it("does NOT contain `paused` (PRD #1190) — it is a non-terminal hold", () => {
    expect(TERMINAL_RUN_STATUSES).not.toContain("paused");
    expect(isTerminalRun("paused")).toBe(false);
  });

  // The other two non-terminal holds, pinned the same way so the set cannot silently grow.
  it("does NOT contain the involuntary holds either", () => {
    expect(isTerminalRun("limit_wait")).toBe(false);
    expect(isTerminalRun("pool_wait")).toBe(false);
  });
});
