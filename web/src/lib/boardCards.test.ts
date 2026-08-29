import { describe, expect, it } from "vitest";

import {
  canPromote,
  highlightSegments,
  isUziCard,
  isSelfImproveTracker,
  matchesQuery,
  SELF_IMPROVE_LABEL,
  visibleCards,
} from "./boardCards";

const card = (labels: string[], closed = false) => ({ labels, closed });

describe("isUziCard", () => {
  it("matches the CONFIGURED label, not the literal uzi", () => {
    // uzi_label is operator-configurable, which is the whole reason PRD #764 derives
    // this rather than storing a boolean: a stored flag goes stale on rename.
    expect(isUziCard(card(["runnable"]), "runnable")).toBe(true);
    expect(isUziCard(card(["uzi"]), "runnable")).toBe(false);
  });

  it("matches exactly, like the sync filter and the server gate", () => {
    expect(isUziCard(card(["Uzi"]), "uzi")).toBe(false);
    expect(isUziCard(card(["uzi-ish"]), "uzi")).toBe(false);
  });

  it("treats a card with no labels as non-uzi", () => {
    expect(isUziCard(card([]), "uzi")).toBe(false);
  });
});

describe("visibleCards", () => {
  const cards = [
    card(["uzi"]),
    card(["uzi", "bug"]),
    card(["bug"]),
    card([]),
    card([SELF_IMPROVE_LABEL]),
  ];

  it("with showAll OFF shows only the uzi (runnable) cards", () => {
    // Membership is the single `uzi` label: only the two `uzi` cards render; the
    // bug-only card, the unlabelled card and the tracker stay off the board.
    expect(visibleCards(cards, "uzi", false)).toEqual([card(["uzi"]), card(["uzi", "bug"])]);
  });

  it("with showAll ON adds every other open card", () => {
    expect(visibleCards(cards, "uzi", true)).toEqual([
      card(["uzi"]),
      card(["uzi", "bug"]),
      card(["bug"]),
      card([]),
    ]);
  });

  it("excludes the self-improve tracker even with showAll ON (Decision 13a)", () => {
    expect(visibleCards(cards, "uzi", true)).not.toContainEqual(card([SELF_IMPROVE_LABEL]));
  });

  it("still shows the tracker if it itself carries uzi", () => {
    // The exclusion is scoped to the "show all other issues" path. A tracker that
    // carries the `uzi` label is a member and hiding it would be a second, unstated rule.
    const promoted = card(["uzi", SELF_IMPROVE_LABEL]);
    expect(visibleCards([promoted], "uzi", false)).toEqual([promoted]);
    expect(visibleCards([promoted], "uzi", true)).toEqual([promoted]);
  });

  it("preserves input order, so the freeze's ordering is untouched", () => {
    // The filter must not reorder: renderCards feeds the lanes, and freeze-test 3
    // depends on the hidden cards' relative order being decided by the payload set.
    const ordered = [card(["uzi", "a"]), card(["bug"]), card(["uzi", "b"])];
    expect(visibleCards(ordered, "uzi", true)).toEqual(ordered);
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
  // PRD #764: canPromote keys off the single `uzi` label. Promote is offered when a
  // card is NOT runnable — Promote adds `uzi` and makes it runnable.
  const uzi = "uzi";

  it("offers Promote only on an open, non-uzi, non-tracker card", () => {
    // documentation does not carry `uzi`, so it is not runnable → Promote.
    expect(canPromote(card(["documentation"]), uzi)).toBe(true);
    expect(canPromote(card([]), uzi)).toBe(true);
    // A bug card without `uzi` is a selector-only issue: still not runnable → Promote.
    expect(canPromote(card(["bug"]), uzi)).toBe(true);
  });

  it("does not offer it on a card that already carries uzi (already runnable)", () => {
    expect(canPromote(card(["uzi"]), uzi)).toBe(false);
    expect(canPromote(card(["uzi", "bug"]), uzi)).toBe(false);
  });

  it("does not offer it on the self-improve tracker (Decision 13a)", () => {
    expect(canPromote(card([SELF_IMPROVE_LABEL]), uzi)).toBe(false);
  });

  it("STILL offers it on a non-uzi card whose forge issue closed but has not been evicted", () => {
    // During the un-evicted window issues.state stays 'opened' so cardDTO.Closed derives
    // FALSE, and the card behaves exactly as before it closed, Promote included.
    expect(canPromote(card(["documentation"], false), uzi)).toBe(true);
  });

  it("does not offer it on a closed card", () => {
    expect(canPromote(card(["documentation"], true), uzi)).toBe(false);
  });
});

describe("matchesQuery", () => {
  const qcard = (iid: number, title: string, labels: string[] = []) => ({ iid, title, labels });

  it("matches every card on an empty or whitespace query", () => {
    expect(matchesQuery(qcard(1, "anything"), "")).toBe(true);
    expect(matchesQuery(qcard(1, "anything"), "   ")).toBe(true);
  });

  it("matches a title substring, case-insensitively", () => {
    expect(matchesQuery(qcard(1, "Add dashboard"), "dash")).toBe(true);
    expect(matchesQuery(qcard(1, "Add dashboard"), "DASH")).toBe(true);
    expect(matchesQuery(qcard(1, "Add dashboard"), "widget")).toBe(false);
  });

  it("matches any label substring, case-insensitively", () => {
    expect(matchesQuery(qcard(1, "t", ["bug"]), "BUG")).toBe(true);
    expect(matchesQuery(qcard(1, "t", ["needs-triage"]), "triage")).toBe(true);
    expect(matchesQuery(qcard(1, "t", ["bug"]), "security")).toBe(false);
  });

  it("matches the iid, stripping a single leading '#', as a substring", () => {
    // #42 matches iid 429 and 142 — a partial issue number is a useful search.
    expect(matchesQuery(qcard(429, "t"), "#42")).toBe(true);
    expect(matchesQuery(qcard(142, "t"), "#42")).toBe(true);
    expect(matchesQuery(qcard(7, "t"), "#42")).toBe(false);
    // A bare number (no '#') hits the iid arm too.
    expect(matchesQuery(qcard(429, "t"), "42")).toBe(true);
  });

  it("does not treat a lone '#' as an iid query", () => {
    // Stripping the single '#' leaves an empty remainder, so the iid arm is skipped;
    // only the title/label substring test remains (which no plain title contains).
    expect(matchesQuery(qcard(429, "issue"), "#")).toBe(false);
  });

  it("returns false when nothing matches", () => {
    expect(matchesQuery(qcard(429, "Add dashboard", ["bug"]), "zzz")).toBe(false);
  });
});

describe("highlightSegments", () => {
  const concat = (segs: { text: string; hit: boolean }[]) => segs.map((s) => s.text).join("");

  it("returns one non-hit segment when there is no match", () => {
    expect(highlightSegments("Add dashboard", "widget")).toEqual([
      { text: "Add dashboard", hit: false },
    ]);
  });

  it("marks a match at the start", () => {
    expect(highlightSegments("dashboard", "dash")).toEqual([
      { text: "dash", hit: true },
      { text: "board", hit: false },
    ]);
  });

  it("marks a match in the middle", () => {
    expect(highlightSegments("Add dashboard now", "dashboard")).toEqual([
      { text: "Add ", hit: false },
      { text: "dashboard", hit: true },
      { text: " now", hit: false },
    ]);
  });

  it("marks a match at the end", () => {
    expect(highlightSegments("the bug", "bug")).toEqual([
      { text: "the ", hit: false },
      { text: "bug", hit: true },
    ]);
  });

  it("marks every occurrence and preserves the text's original casing", () => {
    // The match test is case-insensitive but the emitted text keeps the source casing.
    const segs = highlightSegments("Bug bug BUG", "bug");
    expect(segs).toEqual([
      { text: "Bug", hit: true },
      { text: " ", hit: false },
      { text: "bug", hit: true },
      { text: " ", hit: false },
      { text: "BUG", hit: true },
    ]);
    expect(concat(segs)).toBe("Bug bug BUG");
  });

  it("does not overlap matches (scans past each match)", () => {
    // "aa" in "aaa": one match at 0, then a trailing non-hit "a" — never two overlapping.
    expect(highlightSegments("aaa", "aa")).toEqual([
      { text: "aa", hit: true },
      { text: "a", hit: false },
    ]);
  });

  it("treats an empty or whitespace query as a single non-hit segment", () => {
    expect(highlightSegments("Add dashboard", "")).toEqual([
      { text: "Add dashboard", hit: false },
    ]);
    expect(highlightSegments("Add dashboard", "  ")).toEqual([
      { text: "Add dashboard", hit: false },
    ]);
  });

  it("returns [] for empty text", () => {
    expect(highlightSegments("", "")).toEqual([]);
    expect(highlightSegments("", "dash")).toEqual([]);
  });

  it("never interprets the query as a regular expression", () => {
    // A regex-special query is matched literally: ".*" only hits the literal ".*".
    expect(highlightSegments("a.*b and axyzb", ".*")).toEqual([
      { text: "a", hit: false },
      { text: ".*", hit: true },
      { text: "b and axyzb", hit: false },
    ]);
  });
});
