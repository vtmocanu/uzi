// @vitest-environment jsdom
//
// JudgeUsageStrip cost tile (PRD #1429 D7 completeness): the judge run's OWN cost/time
// strip has the exact same "$0 ⇒ em-dash" ambiguity RunUsagePanel already fixed — it
// must go through the SAME shared costDisplay/costHeadline helpers (lib/costStatus.ts)
// rather than re-deriving "cost_usd > 0" itself. Tested directly against the exported
// JudgeUsageStrip (mirrors RunUsage.test.tsx's direct-component pattern) rather than
// mounting the whole JudgePanel, which needs a live review fetch to reach this tile.
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { JudgeUsageStrip } from "./JudgePanel";
import type { CostStatus, RunReview, RunUsage } from "../../lib/api";

afterEach(cleanup);

function usage(over: Partial<RunUsage> = {}): RunUsage {
  return {
    input_tokens: 12_000,
    cache_read_tokens: 0,
    cache_creation_tokens: 0,
    output_tokens: 3_000,
    cost_usd: 0,
    cost_status: "metered",
    ...over,
  };
}

function judgeRun(over: Partial<NonNullable<RunReview["judge_run"]>> = {}): NonNullable<RunReview["judge_run"]> {
  return {
    judge_run_id: "jr-1",
    claimed_at: "2026-01-01T00:00:00Z",
    started_at: "2026-01-01T00:00:00Z",
    finished_at: "2026-01-01T00:05:00Z",
    usage: usage(),
    ...over,
  };
}

describe("JudgeUsageStrip", () => {
  it("renders NOTHING when the judge posted no usage row (pre-feature judge)", () => {
    const { container } = render(<JudgeUsageStrip judgeRun={judgeRun({ usage: null })} />);
    expect(container.firstChild).toBeNull();
  });

  it("metered: shows the real dollar figure — even a genuine $0.00 — never a bare dash", () => {
    const { container } = render(
      <JudgeUsageStrip judgeRun={judgeRun({ usage: usage({ cost_status: "metered", cost_usd: 0 }) })} />,
    );
    // Positive: the real metered figure renders as an actual dollar amount.
    expect(container.textContent).toContain("$0.00");
    // Negative, paired with the above: never the retired ambiguous dash standing in
    // for a real $0, and never a non-metered label leaking onto a metered run.
    expect(container.textContent).not.toContain("—");
    expect(container.textContent).not.toContain("Subscription");
    expect(container.textContent).not.toContain("Unavailable");
  });

  it("metered: a nonzero cost still renders its exact dollar figure", () => {
    const { container } = render(
      <JudgeUsageStrip judgeRun={judgeRun({ usage: usage({ cost_status: "metered", cost_usd: 1.23 }) })} />,
    );
    expect(container.textContent).toContain("$1.23");
  });

  it("subscription: never a dollar figure even with real tokens spent — labelled, not hidden", () => {
    const { container } = render(
      <JudgeUsageStrip judgeRun={judgeRun({ usage: usage({ cost_status: "subscription", cost_usd: 0 }) })} />,
    );
    // Positive: explicitly labelled as subscription usage, and its real tokens still show.
    expect(container.textContent).toContain("Subscription");
    expect(container.textContent).toContain("Tokens in");
    // Negative, paired with the above: no dollar figure anywhere in the strip.
    expect(container.textContent).not.toContain("$0.00");
    expect(container.textContent).not.toMatch(/\$\d/);
  });

  it("unreported: tokens stay visible, cost is explicitly unavailable — never a bare $0", () => {
    const { container } = render(
      <JudgeUsageStrip judgeRun={judgeRun({ usage: usage({ cost_status: "unreported", cost_usd: 0 }) })} />,
    );
    expect(container.textContent).toContain("Unavailable");
    expect(container.textContent).toContain("Tokens in");
    expect(container.textContent).not.toContain("$0.00");
    expect(container.textContent).not.toMatch(/\$\d/);
  });

  it("a hostile/unknown future cost_status value renders as unavailable, never a complete $0", () => {
    // A newer server ships a status this build has never heard of, with a real nonzero
    // cost_usd riding along — that figure must never surface.
    const { container } = render(
      <JudgeUsageStrip
        judgeRun={judgeRun({ usage: usage({ cost_status: "something_new" as CostStatus, cost_usd: 5 }) })}
      />,
    );
    expect(container.textContent).toContain("Unavailable");
    expect(container.textContent).toContain("Tokens in");
    expect(container.textContent).not.toMatch(/\$\d/);
  });

  it("the empty-string legacy cost_status (a pre-M1 judge run) renders as unavailable, not a metered zero", () => {
    const { container } = render(
      <JudgeUsageStrip judgeRun={judgeRun({ usage: usage({ cost_status: "", cost_usd: 0 }) })} />,
    );
    expect(container.textContent).toContain("Unavailable");
    expect(container.textContent).not.toMatch(/\$\d/);
  });
});
