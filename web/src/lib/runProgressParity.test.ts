// PRD #2602 parity: the server's progress.milestone_done/total must equal what the web's
// milestoneBadge counts for the same run, so the M2/3 badge and the progress cell can never
// disagree. The fixture is recorded by the Go handler test running runToDTO and read
// across the module boundary by design (the TUI asserts the same cases in api/cmd/uzi).
import { describe, expect, it } from "vitest";
import parity from "../../../fixtures/run-progress/parity.json";
import { milestoneBadge } from "./runBadge";
import type { Milestone, RunProgress } from "./apiTypes";

type ParityCase = {
  name: string;
  milestones: Milestone[] | null;
  milestones_completed: string[] | null;
  progress: RunProgress | null;
};

const cases = parity as ParityCase[];
const withProgress = cases.filter((c) => c.progress != null);

describe("run progress parity with milestoneBadge (PRD #2602)", () => {
  it("the fixture is non-trivial (guards an empty or unread file)", () => {
    expect(withProgress.length).toBeGreaterThanOrEqual(6);
    // Positive control: at least one case with a non-zero done count, so a parity that
    // only ever compares 0 with 0 cannot pass.
    expect(withProgress.some((c) => (c.progress?.milestone_done ?? 0) > 0)).toBe(true);
  });

  it.each(withProgress.map((c) => [c.name, c] as const))("%s", (_name, c) => {
    const badge = milestoneBadge(c);
    // milestoneBadge returns null for a nil or empty frozen list: that is 0 of 0.
    expect(badge?.done ?? 0).toBe(c.progress!.milestone_done);
    expect(badge?.total ?? 0).toBe(c.progress!.milestone_total);
  });
});
