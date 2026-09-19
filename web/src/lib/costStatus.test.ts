import { describe, it, expect } from "vitest";
import type { CostStatus } from "./apiTypes";
import { costDisplay, costHeadline, costSubLabel, costCellText, aggregateDisclosure } from "./costStatus";

describe("costDisplay (PRD #1429 D7)", () => {
  it("metered: carries the real dollar figure", () => {
    const d = costDisplay("metered", 1.87);
    expect(d.kind).toBe("metered");
    expect(d.dollars).toBe("$1.87");
  });

  it("metered: a genuine $0 total still reports metered, not unavailable", () => {
    // A cache-only or negligible call can legitimately price at $0.00 — that is a
    // REAL metered reading, not a signal to fall back to "unavailable".
    const d = costDisplay("metered", 0);
    expect(d.kind).toBe("metered");
    expect(d.dollars).toBe("$0.00");
  });

  it("subscription: never carries a dollar figure, even with a nonzero cost_usd on the wire", () => {
    const d = costDisplay("subscription", 4.2);
    expect(d.kind).toBe("subscription");
    expect(d.dollars).toBeUndefined();
  });

  it("unreported: never carries a dollar figure", () => {
    const d = costDisplay("unreported", 0);
    expect(d.kind).toBe("unavailable");
    expect(d.dollars).toBeUndefined();
  });

  it("empty string (pre-M1/unset legacy value) folds to unavailable, not metered zero", () => {
    const d = costDisplay("", 0);
    expect(d.kind).toBe("unavailable");
    expect(d.dollars).toBeUndefined();
  });

  it("a hostile/future-unknown enum value folds to unavailable and drops its cost_usd, never a complete $0", () => {
    // Simulates a newer server shipping a status this build has never heard of — the
    // wire type is a compile-time hint only (see CostStatus's own doc comment), so a
    // real JSON payload can carry any string here despite the narrower TS type.
    const hostile = "something_new" as CostStatus;
    const d = costDisplay(hostile, 5);
    expect(d.kind).toBe("unavailable");
    expect(d.dollars).toBeUndefined();
  });
});

describe("costHeadline / costSubLabel / costCellText", () => {
  it("metered renders the dollar figure everywhere, and the Claude sub-label", () => {
    const d = costDisplay("metered", 2.5);
    expect(costHeadline(d)).toBe("$2.50");
    expect(costCellText(d)).toBe("$2.50");
    expect(costSubLabel(d, "claude")).toBe("your Anthropic token");
    expect(costSubLabel(d, "codex")).toBe("your OpenAI credential");
    expect(costSubLabel(d)).toBe("your token");
  });

  it("subscription never renders a dollar string anywhere, and names itself in the sub-label", () => {
    const d = costDisplay("subscription", 0);
    expect(costHeadline(d)).toBe("Subscription");
    expect(costHeadline(d)).not.toMatch(/\$/);
    expect(costCellText(d)).toBe("sub");
    expect(costCellText(d)).not.toMatch(/\$/);
    expect(costSubLabel(d)).toMatch(/subscription usage/);
  });

  it("unavailable (unreported) never renders a dollar string, and says cost is unavailable", () => {
    const d = costDisplay("unreported", 0);
    expect(costHeadline(d)).toBe("Unavailable");
    expect(costHeadline(d)).not.toMatch(/\$/);
    expect(costCellText(d)).toBe("n/a");
    expect(costSubLabel(d)).toMatch(/cost unavailable/);
  });

  it("headline is never the bare '—' placeholder for a non-metered run (that ambiguity is the retired bug)", () => {
    expect(costHeadline(costDisplay("subscription", 0))).not.toBe("—");
    expect(costHeadline(costDisplay("unreported", 0))).not.toBe("—");
    expect(costHeadline(costDisplay("something_new" as CostStatus, 9))).not.toBe("—");
  });
});

describe("aggregateDisclosure (PRD #1429 D7 mixed aggregate)", () => {
  it("an all-metered window (0/0) discloses nothing — byte-identical empty text", () => {
    const d = aggregateDisclosure(0, 0);
    expect(d.incomplete).toBe(false);
    expect(d.text).toBe("");
  });

  it("subscription-only mix discloses the count, singular run", () => {
    const d = aggregateDisclosure(1, 0);
    expect(d.incomplete).toBe(true);
    expect(d.text).toBe("1 subscription run excluded from the $ total");
  });

  it("unreported-only mix discloses the count, plural runs", () => {
    const d = aggregateDisclosure(0, 3);
    expect(d.incomplete).toBe(true);
    expect(d.text).toBe("3 unreported runs excluded from the $ total");
  });

  it("a MIXED aggregate (both subscription and unreported present) discloses both counts together", () => {
    const d = aggregateDisclosure(2, 3);
    expect(d.incomplete).toBe(true);
    expect(d.text).toBe("2 subscription + 3 unreported runs excluded from the $ total");
  });

  it("negative/hostile counts never crash and never disclose below zero", () => {
    const d = aggregateDisclosure(-5, -1);
    expect(d.incomplete).toBe(false);
    expect(d.text).toBe("");
  });
});
