import { describe, expect, it } from "vitest";

import { canPromote, isPRDCard, isSelfImproveTracker, SELF_IMPROVE_LABEL, visibleCards } from "./boardCards";

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

describe("visibleCards", () => {
  const cards = [
    card(["PRD"]),
    card(["PRD", "bug"]),
    card(["bug"]),
    card([]),
    card([SELF_IMPROVE_LABEL]),
  ];

  it("with the toggle OFF shows exactly today's board", () => {
    // The criterion that protects the default: nothing about the board changes for
    // a user who never touches the toggle.
    expect(visibleCards(cards, "PRD", false)).toEqual([card(["PRD"]), card(["PRD", "bug"])]);
  });

  it("with the toggle ON adds the open non-PRD cards", () => {
    expect(visibleCards(cards, "PRD", true)).toEqual([
      card(["PRD"]),
      card(["PRD", "bug"]),
      card(["bug"]),
      card([]),
    ]);
  });

  it("excludes the self-improve tracker even with the toggle ON (Decision 13a)", () => {
    expect(visibleCards(cards, "PRD", true)).not.toContainEqual(card([SELF_IMPROVE_LABEL]));
  });

  it("still shows the tracker if it somehow carries the PRD label", () => {
    // The exclusion is scoped to the NON-PRD render path. A tracker that has been
    // given the PRD label is a PRD card and hiding it would be a second, unstated
    // rule.
    const promoted = card(["PRD", SELF_IMPROVE_LABEL]);
    expect(visibleCards([promoted], "PRD", false)).toEqual([promoted]);
    expect(visibleCards([promoted], "PRD", true)).toEqual([promoted]);
  });

  it("preserves input order, so the freeze's ordering is untouched", () => {
    // The filter must not reorder: renderCards feeds the lanes, and freeze-test 3
    // depends on the hidden cards' relative order being decided by the payload set.
    const ordered = [card(["PRD", "a"]), card(["bug"]), card(["PRD", "b"])];
    expect(visibleCards(ordered, "PRD", true)).toEqual(ordered);
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
  it("offers Promote only on an open, non-PRD, non-tracker card", () => {
    expect(canPromote(card(["bug"]), "PRD")).toBe(true);
    expect(canPromote(card([]), "PRD")).toBe(true);
  });

  it("does not offer it on a card that is already uzi's work", () => {
    expect(canPromote(card(["PRD"]), "PRD")).toBe(false);
  });

  it("does not offer it on the self-improve tracker (Decision 13a)", () => {
    expect(canPromote(card([SELF_IMPROVE_LABEL]), "PRD")).toBe(false);
  });

  it("does not offer it on a closed card", () => {
    // Closed non-PRD cards never enter the cache at all (the additive fetch is
    // open-only), so this is belt: a card that closes between polls still lingers.
    expect(canPromote(card(["bug"], true), "PRD")).toBe(false);
  });
});
