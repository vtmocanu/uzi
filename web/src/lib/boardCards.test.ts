import { describe, expect, it } from "vitest";

import {
  canPromote,
  DEFAULT_BOARD_EXTRA_LABELS,
  isEligibleCard,
  isMemberCard,
  isPRDCard,
  isSelfImproveTracker,
  SELF_IMPROVE_LABEL,
  visibleCards,
} from "./boardCards";

const card = (labels: string[], closed = false) => ({ labels, closed });

describe("isPRDCard", () => {
  it("matches the CONFIGURED label, not the literal PRD", () => {
    // prd_label is operator-configurable, which is the whole reason Decision 12
    // derives this rather than storing a boolean: a stored flag goes stale on rename.
    expect(isPRDCard(card(["Feature"]), "Feature")).toBe(true);
    expect(isPRDCard(card(["PRD"]), "Feature")).toBe(false);
  });

  it("matches exactly, like the sync filter and the server gate", () => {
    expect(isPRDCard(card(["prd"]), "PRD")).toBe(false);
    expect(isPRDCard(card(["PRD-ish"]), "PRD")).toBe(false);
  });

  it("treats a card with no labels as non-PRD", () => {
    expect(isPRDCard(card([]), "PRD")).toBe(false);
  });
});

describe("isMemberCard", () => {
  it("matches a card carrying ANY of the membership labels, exactly", () => {
    expect(isMemberCard(card(["bug"]), ["PRD", "bug"])).toBe(true);
    expect(isMemberCard(card(["PRD"]), ["PRD", "bug"])).toBe(true);
    expect(isMemberCard(card(["security"]), ["PRD", "bug"])).toBe(false);
    expect(isMemberCard(card([]), ["PRD", "bug"])).toBe(false);
  });

  it("is an exact match, like isPRDCard — no case-folding, no prefix", () => {
    expect(isMemberCard(card(["Bug"]), ["PRD", "bug"])).toBe(false);
    expect(isMemberCard(card(["bug-ish"]), ["PRD", "bug"])).toBe(false);
  });

  it("matches nothing when the membership set is empty", () => {
    expect(isMemberCard(card(["PRD"]), [])).toBe(false);
  });
});

describe("isEligibleCard", () => {
  it("matches a card carrying ANY eligible label, exactly (any-of)", () => {
    // The M4 eligible set ships as ["PRD", "bug"], so a bug card is runnable...
    expect(isEligibleCard(card(["bug"]), ["PRD", "bug"])).toBe(true);
    // ...and so is a primary card (the primary is always in the set).
    expect(isEligibleCard(card(["PRD"]), ["PRD", "bug"])).toBe(true);
    // A documentation card is a possible board MEMBER but is not eligible unless the
    // admin adds it — visibility and eligibility are orthogonal (Decision 1/2).
    expect(isEligibleCard(card(["documentation"]), ["PRD", "bug"])).toBe(false);
    expect(isEligibleCard(card([]), ["PRD", "bug"])).toBe(false);
  });

  it("is an exact match, no case-folding, no prefix", () => {
    expect(isEligibleCard(card(["Bug"]), ["PRD", "bug"])).toBe(false);
    expect(isEligibleCard(card(["bug-ish"]), ["PRD", "bug"])).toBe(false);
  });

  it("matches nothing when the eligible set is empty", () => {
    expect(isEligibleCard(card(["PRD"]), [])).toBe(false);
  });
});

describe("visibleCards", () => {
  const cards = [
    card(["PRD"]),
    card(["PRD", "bug"]),
    card(["bug"]),
    card([]),
    card([SELF_IMPROVE_LABEL]),
  ];

  it("with showAll OFF shows only the membership cards (primary ∪ extras)", () => {
    // Membership is primary ∪ extras. With extras=["bug"] the bug-only card joins the
    // two PRD cards; the unlabelled card and the tracker stay off the board.
    expect(visibleCards(cards, ["PRD", "bug"], false)).toEqual([
      card(["PRD"]),
      card(["PRD", "bug"]),
      card(["bug"]),
    ]);
  });

  it("with a primary-only membership set behaves like the old PRD-only default", () => {
    // No extras: only the primary is a member, so this is exactly today's board.
    expect(visibleCards(cards, ["PRD"], false)).toEqual([card(["PRD"]), card(["PRD", "bug"])]);
  });

  it("honours a multi-label membership set", () => {
    const multi = [card(["PRD"]), card(["bug"]), card(["security"]), card(["docs"])];
    expect(visibleCards(multi, ["PRD", "bug", "security"], false)).toEqual([
      card(["PRD"]),
      card(["bug"]),
      card(["security"]),
    ]);
  });

  it("with showAll ON adds every other open card", () => {
    expect(visibleCards(cards, ["PRD"], true)).toEqual([
      card(["PRD"]),
      card(["PRD", "bug"]),
      card(["bug"]),
      card([]),
    ]);
  });

  it("excludes the self-improve tracker even with showAll ON (Decision 13a)", () => {
    expect(visibleCards(cards, ["PRD"], true)).not.toContainEqual(card([SELF_IMPROVE_LABEL]));
    // …and still excludes it when extras are configured but it is not one of them.
    expect(visibleCards(cards, ["PRD", "bug"], true)).not.toContainEqual(card([SELF_IMPROVE_LABEL]));
  });

  it("still shows the tracker if it is itself a member", () => {
    // The exclusion is scoped to the "show all other issues" path. A tracker that
    // carries a membership label is a member and hiding it would be a second,
    // unstated rule — this mirrors the old PRD-label carve-out, generalised.
    const promotedPrimary = card(["PRD", SELF_IMPROVE_LABEL]);
    expect(visibleCards([promotedPrimary], ["PRD"], false)).toEqual([promotedPrimary]);
    expect(visibleCards([promotedPrimary], ["PRD"], true)).toEqual([promotedPrimary]);

    const memberByExtra = card(["bug", SELF_IMPROVE_LABEL]);
    expect(visibleCards([memberByExtra], ["PRD", "bug"], false)).toEqual([memberByExtra]);
    expect(visibleCards([memberByExtra], ["PRD", "bug"], true)).toEqual([memberByExtra]);
  });

  it("preserves input order, so the freeze's ordering is untouched", () => {
    // The filter must not reorder: renderCards feeds the lanes, and freeze-test 3
    // depends on the hidden cards' relative order being decided by the payload set.
    const ordered = [card(["PRD", "a"]), card(["bug"]), card(["PRD", "b"])];
    expect(visibleCards(ordered, ["PRD"], true)).toEqual(ordered);
  });
});

describe("DEFAULT_BOARD_EXTRA_LABELS", () => {
  it("ships bug as the compiled-in extras fallback (open question 1)", () => {
    expect(DEFAULT_BOARD_EXTRA_LABELS).toEqual(["bug"]);
  });
});

describe("isSelfImproveTracker", () => {
  it("matches the tracking label exactly", () => {
    expect(isSelfImproveTracker(card([SELF_IMPROVE_LABEL]))).toBe(true);
    expect(isSelfImproveTracker(card(["UZI-SELF-IMPROVE"]))).toBe(false);
    expect(isSelfImproveTracker(card(["bug"]))).toBe(false);
  });

  it("uses the same string the API compiles in", () => {
    // If this constant drifts from selfimprove.TrackingLabel the failure is a card
    // shown that should be hidden, never a promote that should be refused (the
    // server enforces that independently).
    expect(SELF_IMPROVE_LABEL).toBe("uzi-self-improve");
  });
});

describe("canPromote", () => {
  // PRD #196 M4: canPromote keys off the ELIGIBLE set, not the primary alone. Promote is
  // offered when a card is NOT runnable — Promote adds the primary and makes it runnable.
  const eligible = ["PRD", "bug"];

  it("offers Promote only on an open, non-eligible, non-tracker card", () => {
    // documentation is not in the eligible set, so it is not runnable → Promote.
    expect(canPromote(card(["documentation"]), eligible)).toBe(true);
    expect(canPromote(card([]), eligible)).toBe(true);
  });

  it("does not offer it on a card that is already runnable (eligible)", () => {
    // Already carries the primary...
    expect(canPromote(card(["PRD"]), eligible)).toBe(false);
    // ...or a non-primary eligible label (`bug`) — it runs, so Promote is a no-op.
    expect(canPromote(card(["bug"]), eligible)).toBe(false);
  });

  it("DOES offer it on a bug card when bug is not in the eligible set", () => {
    // Eligibility is admin-configured: with only the primary eligible, a bug card is
    // not runnable and Promote is the way to make it so.
    expect(canPromote(card(["bug"]), ["PRD"])).toBe(true);
  });

  it("does not offer it on the self-improve tracker (Decision 13a)", () => {
    expect(canPromote(card([SELF_IMPROVE_LABEL]), eligible)).toBe(false);
  });

  it("STILL offers it on a card whose forge issue closed but has not been evicted", () => {
    // The window docs/board.md documents. During it the row is never re-upserted —
    // the PRD fetch is label-filtered and the additive fetch is StateOpened, so
    // neither returns a closed non-eligible issue — so issues.state stays 'opened' and
    // cardDTO.Closed derives FALSE. The card therefore looks and behaves exactly as
    // it did before it closed, Promote included.
    //
    // This test exists because a code comment asserted the opposite (no button at
    // all) and survived three readers, partly by citing docs/board.md as
    // corroboration while contradicting it.
    expect(canPromote(card(["documentation"], false), eligible)).toBe(true);
  });

  it("does not offer it on a closed card", () => {
    // Reachable only when a row was cached WHILE closed — which the state=all PRD
    // fetch does — and then stopped being eligible, i.e. an operator rename while
    // closed PRD cards are cached. Not reachable through the sync alone, and stated
    // that narrowly on purpose: the comment that used to claim this was the ordinary
    // post-close window was wrong.
    expect(canPromote(card(["documentation"], true), eligible)).toBe(false);
  });
});
