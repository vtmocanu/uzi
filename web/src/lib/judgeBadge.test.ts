import { describe, it, expect } from "vitest";
import { judgeBadge } from "./judgeBadge";

// PRD #98 M4, Decision 7. The single-grammar rule is the point of these tests: the
// concept mock shipped two grammars (a verdict badge AND a separate count badge) and that
// was called out as a bug, so "one badge, verdict-first, count only when > 0" is the
// contract rather than a styling preference.
describe("judgeBadge", () => {
  it("renders nothing for an unjudged run", () => {
    // Not a neutral pill: "never judged" and "judged and fine" are different facts, and a
    // placeholder would assert the second when only the first is true.
    expect(judgeBadge({ judge_verdict: null, judge_todo_count: 0 })).toBeNull();
    // Even if a count somehow arrived without a verdict, there is no verdict to show.
    expect(judgeBadge({ judge_verdict: null, judge_todo_count: 5 })).toBeNull();
  });

  it("is verdict-only when nothing is left to triage", () => {
    expect(judgeBadge({ judge_verdict: "ideal", judge_todo_count: 0 })?.label).toBe("⚖ ideal");
    expect(judgeBadge({ judge_verdict: "ok", judge_todo_count: 0 })?.label).toBe("⚖ ok");
    expect(judgeBadge({ judge_verdict: "issues", judge_todo_count: 0 })?.label).toBe("⚖ issues");
  });

  it("appends the count only when it is > 0, in ONE badge", () => {
    const badge = judgeBadge({ judge_verdict: "issues", judge_todo_count: 2 });
    expect(badge?.label).toBe("⚖ issues · 2");
    // One string, one badge — the verdict and the count are never two pills.
    expect(badge?.label.match(/⚖/g)).toHaveLength(1);
  });

  it("keeps a fully-triaged run's verdict visible", () => {
    // The badge reports the JUDGE'S verdict, not the triage state, so clearing the
    // backlog must not erase the fact that the judge flagged issues.
    expect(judgeBadge({ judge_verdict: "issues", judge_todo_count: 0 })?.label).toBe("⚖ issues");
  });

  it("tones the verdict without claiming the run failed", () => {
    // `issues` is a warning, not a danger: the judge found things worth doing, not a
    // broken run — the status pill owns failure.
    expect(judgeBadge({ judge_verdict: "ideal", judge_todo_count: 0 })?.tone).toBe("ok");
    expect(judgeBadge({ judge_verdict: "ok", judge_todo_count: 0 })?.tone).toBe("info");
    expect(judgeBadge({ judge_verdict: "issues", judge_todo_count: 3 })?.tone).toBe("warning");
  });

  it("explains itself in the title, including the count", () => {
    expect(judgeBadge({ judge_verdict: "issues", judge_todo_count: 3 })?.title).toContain("3 still to triage");
    expect(judgeBadge({ judge_verdict: "ideal", judge_todo_count: 0 })?.title).not.toContain("triage");
  });
});
