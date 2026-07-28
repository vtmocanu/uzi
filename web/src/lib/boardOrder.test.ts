import { describe, it, expect } from "vitest";
import {
  boardOrderAfterDrop,
  dropIntent,
  insertionEdgeFor,
  isSortMode,
  neighbourAnchor,
  sortCards,
  SORT_MODES,
  type SortMode,
} from "./boardOrder";
import { visibleCards } from "./boardCards";
import type { Card, LatestRun } from "./api";

// Fixtures are built so that under EVERY mode the expected order differs from iid
// order. That is PRD freeze-test 1's requirement generalised: where a mode's order and
// the iid order coincide, an implementation that quietly falls back to iid passes.

function aRun(updatedAt: string): LatestRun {
  return {
    id: "r1",
    status: "completed",
    mr_iid: null,
    mr_web_url: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    owner_name: "someone",
    worker_name: null,
    is_mine: true,
    run_count: 1,
    created_at: updatedAt,
    updated_at: updatedAt,
  } as LatestRun;
}

function aCard(over: Partial<Card> & { iid: number }): Card {
  return {
    title: `issue ${over.iid}`,
    state: "opened",
    labels: ["PRD"],
    web_url: `https://forge/x/-/issues/${over.iid}`,
    forge_type: "gitlab",
    author: null,
    has_prd_link: true,
    column: "",
    closed: false,
    conflict: false,
    forge_updated_at: "2026-01-01T00:00:00Z",
    latest_run: null,
    pipeline: null,
    ...over,
  } as Card;
}

const iidsOf = (cards: readonly Card[]) => cards.map((c) => c.iid);

describe("isSortMode", () => {
  it("accepts every mode the switcher offers", () => {
    for (const m of SORT_MODES) expect(isSortMode(m.value)).toBe(true);
  });

  it("rejects anything else, so a stale localStorage value degrades to Manual", () => {
    // The guard exists because an unrecognised value would otherwise reach sortCards
    // and select no comparator at all.
    for (const v of ["", "Manual", "position", null, undefined, 3, {}]) {
      expect(isSortMode(v)).toBe(false);
    }
  });
});

describe("sortCards", () => {
  // U6
  it("manual is the identity and does not mutate the input", () => {
    // The payload already arrives in manual order (ListIssuesByRepo's ORDER BY), so
    // "render what the server sent" IS Manual. A sort here would break that contract.
    const cards = [aCard({ iid: 30 }), aCard({ iid: 10 }), aCard({ iid: 20 })];
    const before = iidsOf(cards);
    expect(iidsOf(sortCards(cards, "manual"))).toEqual([30, 10, 20]);
    expect(iidsOf(cards)).toEqual(before);
  });

  it("never mutates its input in any mode", () => {
    for (const { value } of SORT_MODES) {
      const cards = [aCard({ iid: 30 }), aCard({ iid: 10 })];
      sortCards(cards, value);
      expect(iidsOf(cards)).toEqual([30, 10]);
    }
  });

  // U1
  it("iid mode sorts ascending by issue number", () => {
    const cards = [aCard({ iid: 30 }), aCard({ iid: 10 }), aCard({ iid: 20 })];
    expect(iidsOf(sortCards(cards, "iid"))).toEqual([10, 20, 30]);
  });

  // U1 + U2
  it("run mode is newest-first and puts never-run cards LAST in iid order", () => {
    // latest_run is a pointer and is null on most cards of a real board. TWO never-run
    // cards, or the tiebreak among them is unpinned.
    const cards = [
      aCard({ iid: 40 }), // never run
      aCard({ iid: 10, latest_run: aRun("2026-01-01T10:00:00Z") }),
      aCard({ iid: 30 }), // never run
      aCard({ iid: 20, latest_run: aRun("2026-01-02T10:00:00Z") }),
    ];
    expect(iidsOf(sortCards(cards, "run"))).toEqual([20, 10, 30, 40]);
  });

  // U1
  it("updated mode is newest-first", () => {
    const cards = [
      aCard({ iid: 10, forge_updated_at: "2026-01-01T00:00:00Z" }),
      aCard({ iid: 20, forge_updated_at: "2026-03-01T00:00:00Z" }),
      aCard({ iid: 30, forge_updated_at: "2026-02-01T00:00:00Z" }),
    ];
    expect(iidsOf(sortCards(cards, "updated"))).toEqual([20, 30, 10]);
  });

  // U3 — the lexicographic-compare catcher.
  it("compares timestamps as instants, not as strings", () => {
    // Go trims trailing zeros from RFC3339 fractional seconds, so these two forms are
    // both emitted. As STRINGS "…00.5Z" > "…00Z", which happens to be right here; the
    // discriminating pair is the one below, where the string order is WRONG.
    const cards = [
      aCard({ iid: 10, forge_updated_at: "2026-01-01T10:00:00.5Z" }),
      aCard({ iid: 20, forge_updated_at: "2026-01-01T10:00:01Z" }),
    ];
    // 10:00:01 is later than 10:00:00.5, so 20 leads. String-compared, "10:00:00.5Z"
    // sorts after "10:00:01Z" is false — but ".5Z" vs "1Z" compares '.' (0x2E) against
    // '1' (0x31), so the string compare puts 20 first too. Assert via run mode's
    // never-run arm as well, below, and keep this as the instant check.
    expect(iidsOf(sortCards(cards, "updated"))).toEqual([20, 10]);
  });

  it("orders a sub-second pair the string compare gets WRONG", () => {
    // "2026-01-01T10:00:00Z" vs "2026-01-01T10:00:00.5Z": as strings the first is
    // SHORTER and a prefix of the second up to the 'Z' vs '.', and 'Z' (0x5A) > '.'
    // (0x2E), so a descending string sort puts the ZERO-fraction one first. As
    // instants the .5 one is later and must lead.
    const cards = [
      aCard({ iid: 10, forge_updated_at: "2026-01-01T10:00:00Z" }),
      aCard({ iid: 20, forge_updated_at: "2026-01-01T10:00:00.5Z" }),
    ];
    expect(iidsOf(sortCards(cards, "updated"))).toEqual([20, 10]);
  });

  it("sinks an unparseable timestamp instead of scrambling the board", () => {
    const cards = [
      aCard({ iid: 10, forge_updated_at: "not-a-date" }),
      aCard({ iid: 20, forge_updated_at: "2026-01-01T00:00:00Z" }),
      aCard({ iid: 30, forge_updated_at: "2026-02-01T00:00:00Z" }),
    ];
    expect(iidsOf(sortCards(cards, "updated"))).toEqual([30, 20, 10]);
  });

  // U4
  it("title mode sorts the DISPLAYED string, not the raw one", () => {
    // The board renders stripUnsafeChars(title) (#124). Sorting the raw title puts a
    // bidi-prefixed one in a visibly wrong place: U+202E sorts after every letter.
    const cards = [
      aCard({ iid: 10, title: "zebra" }),
      aCard({ iid: 20, title: "‮apple" }),
      aCard({ iid: 30, title: "mango" }),
    ];
    expect(iidsOf(sortCards(cards, "title"))).toEqual([20, 30, 10]);
  });

  it("title mode is case-insensitive and numeric-aware", () => {
    const cards = [
      aCard({ iid: 10, title: "beta" }),
      aCard({ iid: 20, title: "Alpha" }),
      aCard({ iid: 30, title: "item 10" }),
      aCard({ iid: 40, title: "item 9" }),
    ];
    // "Alpha" before "beta" needs case-insensitivity; "item 9" before "item 10" needs
    // numeric collation (a plain compare puts "10" first).
    expect(iidsOf(sortCards(cards, "title"))).toEqual([20, 10, 40, 30]);
  });

  // U5 — totality.
  it("every non-manual mode falls through to the iid tiebreak", () => {
    // A comparator that can return 0 leaves the render at the mercy of the engine's
    // sort. Each pair below is EQUAL on its mode's key, so only the tiebreak decides.
    const equalOn: Record<Exclude<SortMode, "manual" | "iid">, [Card, Card]> = {
      run: [
        aCard({ iid: 30, latest_run: aRun("2026-01-01T00:00:00Z") }),
        aCard({ iid: 10, latest_run: aRun("2026-01-01T00:00:00Z") }),
      ],
      updated: [
        aCard({ iid: 30, forge_updated_at: "2026-01-01T00:00:00Z" }),
        aCard({ iid: 10, forge_updated_at: "2026-01-01T00:00:00Z" }),
      ],
      title: [aCard({ iid: 30, title: "same" }), aCard({ iid: 10, title: "SAME" })],
    };
    for (const [mode, pair] of Object.entries(equalOn)) {
      expect(iidsOf(sortCards(pair, mode as SortMode))).toEqual([10, 30]);
    }
  });
});

describe("boardOrderAfterDrop", () => {
  const columnKeys = ["", "Planned", "In Progress"];
  // Displayed order deliberately differs from iid order, so a fold that reconstructs
  // the list from iids rather than from position is caught.
  const openCards = [
    aCard({ iid: 30, column: "" }),
    aCard({ iid: 10, column: "" }),
    aCard({ iid: 20, column: "Planned" }),
    aCard({ iid: 40, column: "Planned" }),
    aCard({ iid: 50, column: "In Progress" }),
  ];

  // U7
  it("moves a card above an anchor within its own column", () => {
    expect(boardOrderAfterDrop(openCards, columnKeys, 10, "", { iid: 30, before: true })).toEqual([
      10, 30, 20, 40, 50,
    ]);
  });

  it("moves a card below an anchor within its own column", () => {
    expect(boardOrderAfterDrop(openCards, columnKeys, 30, "", { iid: 10, before: false })).toEqual([
      10, 30, 20, 40, 50,
    ]);
  });

  it("moves a card into another column at the anchor", () => {
    expect(
      boardOrderAfterDrop(openCards, columnKeys, 30, "Planned", { iid: 40, before: true }),
    ).toEqual([10, 20, 30, 40, 50]);
  });

  it("appends to the destination column when there is no anchor", () => {
    // A drop on lane whitespace: "append to the end of this column", which is what the
    // pre-M5 lane-level drop already effectively did.
    expect(boardOrderAfterDrop(openCards, columnKeys, 30, "In Progress", null)).toEqual([
      10, 20, 40, 50, 30,
    ]);
  });

  it("treats a drop on the dragged card itself as a freeze with no positional change", () => {
    // 30 is the FIRST card of its lane, deliberately. The design doc claimed this case
    // "falls through to end of bucket — harmless and correct"; it is not. Removing the
    // card and then appending it moves it to the BOTTOM, which only looks like a no-op
    // when the fixture happens to have picked the last card. This assertion is the one
    // that caught it.
    expect(boardOrderAfterDrop(openCards, columnKeys, 30, "", { iid: 30, before: true })).toEqual(
      iidsOf(openCards),
    );
    expect(boardOrderAfterDrop(openCards, columnKeys, 30, "", { iid: 30, before: false })).toEqual(
      iidsOf(openCards),
    );
    // …and the last card of a lane, which is the case that passes either way — kept so
    // the pair shows the discrimination rather than just asserting the fix.
    expect(boardOrderAfterDrop(openCards, columnKeys, 40, "Planned", { iid: 40, before: true })).toEqual(
      iidsOf(openCards),
    );
  });

  it("returns the plain grouped order when the dragged iid is not present", () => {
    expect(boardOrderAfterDrop(openCards, columnKeys, 999, "", null)).toEqual(iidsOf(openCards));
  });

  it("groups by column in render order even when the input is interleaved", () => {
    const interleaved = [
      aCard({ iid: 50, column: "In Progress" }),
      aCard({ iid: 30, column: "" }),
      aCard({ iid: 20, column: "Planned" }),
      aCard({ iid: 10, column: "" }),
    ];
    expect(boardOrderAfterDrop(interleaved, columnKeys, 999, "", null)).toEqual([30, 10, 20, 50]);
  });

  it("keeps a card whose column is not a configured lane instead of dropping it", () => {
    // Should be impossible (ResolveColumn only yields "" or a configured label), but a
    // silently dropped card would be cleared to NULL by ClearBoardOrderExcept and
    // relocate to the bottom of its column — a data-losing bug that reads as a render
    // glitch.
    const withStray = [...openCards, aCard({ iid: 60, column: "Ghost" })];
    const out = boardOrderAfterDrop(withStray, columnKeys, 999, "", null);
    expect(out).toContain(60);
    expect(out).toHaveLength(withStray.length);
  });

  // U8 — THE invariant. Runs over every case above.
  it("always returns every input card exactly once", () => {
    const cases: Array<[number, string, Parameters<typeof boardOrderAfterDrop>[4]]> = [
      [10, "", { iid: 30, before: true }],
      [30, "", { iid: 10, before: false }],
      [30, "Planned", { iid: 40, before: true }],
      [30, "In Progress", null],
      [30, "", { iid: 30, before: true }],
      [999, "", null],
      [50, "", null],
      [20, "In Progress", { iid: 50, before: false }],
    ];
    for (const [dragIid, dest, anchor] of cases) {
      const out = boardOrderAfterDrop(openCards, columnKeys, dragIid, dest, anchor);
      expect(out).toHaveLength(openCards.length);
      expect([...out].sort((a, b) => a - b)).toEqual([...iidsOf(openCards)].sort((a, b) => a - b));
    }
  });
});

// U9
describe("insertionEdgeFor", () => {
  it("returns top above the midpoint and bottom below it", () => {
    expect(insertionEdgeFor(100, 40, 105)).toBe("top");
    expect(insertionEdgeFor(100, 40, 135)).toBe("bottom");
  });

  it("pins the boundary: exactly on the midpoint is bottom", () => {
    // Arbitrary but asserted — this is the only line here where an off-by-one is
    // invisible in use.
    expect(insertionEdgeFor(100, 40, 120)).toBe("bottom");
    expect(insertionEdgeFor(100, 40, 119.9)).toBe("top");
  });

  it("handles a zero-height rect without throwing", () => {
    expect(insertionEdgeFor(0, 0, 0)).toBe("bottom");
  });
});

// U10 — the payload-set rule (Decision 7b), asserted at the only layer that can assert
// it before M6's card-level filter exists.
describe("dropIntent", () => {
  const columnKeys = ["", "Planned"];
  const payloadCards = [
    aCard({ iid: 30, column: "" }),
    aCard({ iid: 10, column: "" }),
    aCard({ iid: 20, column: "Planned" }),
    aCard({ iid: 99, column: "", closed: true }),
  ];

  it("flags a cross-column drop and not a within-column one", () => {
    expect(dropIntent({ payloadCards, columnKeys, sortMode: "manual", dragIid: 30, destColumnKey: "Planned", anchor: null })!.columnChanged).toBe(true);
    expect(dropIntent({ payloadCards, columnKeys, sortMode: "manual", dragIid: 30, destColumnKey: "", anchor: null })!.columnChanged).toBe(false);
  });

  it("EXCLUDES closed cards from the freeze", () => {
    // A frozen position on a closed card would ride an issue that later reopens and
    // drop it at an arbitrary rank in its new column.
    const out = dropIntent({ payloadCards, columnKeys, sortMode: "manual", dragIid: 30, destColumnKey: "", anchor: null })!;
    expect(out.iids).not.toContain(99);
    expect(out.iids).toHaveLength(3);
  });

  it("returns every card it was GIVEN, so a filtered input yields a shorter order", () => {
    // RENAMED from "covers the UNFILTERED payload, not the subset a viewer can see",
    // which oversold it. dropIntent has no filter of its own and cannot: it orders
    // whatever array it is handed. So this pins its INPUT-OUTPUT relation — the same
    // set out as in — and nothing about which array the caller chooses.
    //
    // The `partial` half is a fixture check, not a behavioural one: the test removed
    // 20 from the input, so 20 being absent from the output is arithmetic. It stays
    // because it makes the relation legible in both directions, but nobody should
    // read it as the payload-set gate.
    //
    // THE ACTUAL GATE IS ELSEWHERE, in two places, because dropIntent's contract and
    // Board.tsx's choice of argument are different things and only the second can
    // regress: "freeze-test 3" below (contract) and Board.test.tsx's
    // "freezes the cards the viewer cannot see" (wiring). Mutating Board.tsx to pass
    // the rendered set left all 1316 tests green while this one passed.
    const rendered = payloadCards.filter((c) => c.iid !== 20);
    const full = dropIntent({ payloadCards, columnKeys, sortMode: "manual", dragIid: 30, destColumnKey: "", anchor: null })!;
    const partial = dropIntent({ payloadCards: rendered, columnKeys, sortMode: "manual", dragIid: 30, destColumnKey: "", anchor: null })!;
    expect(full.iids).toContain(20);
    expect(partial.iids).not.toContain(20);
    expect(full.iids.length).toBe(partial.iids.length + 1);
  });

  it("freezes the order of the CURRENT MODE, not the iid order", () => {
    // The trap Decision 7b exists for: sorted by something else, a freeze that fell
    // back to iid would re-sort the whole board under the user's hand.
    const cards = [
      aCard({ iid: 10, column: "", forge_updated_at: "2026-01-01T00:00:00Z" }),
      aCard({ iid: 20, column: "", forge_updated_at: "2026-03-01T00:00:00Z" }),
      aCard({ iid: 30, column: "", forge_updated_at: "2026-02-01T00:00:00Z" }),
    ];
    const out = dropIntent({ payloadCards: cards, columnKeys: [""], sortMode: "updated", dragIid: 20, destColumnKey: "", anchor: { iid: 10, before: false } })!;
    // Displayed under `updated` is 20,30,10. Move 20 below 10 → 30,10,20.
    expect(out.iids).toEqual([30, 10, 20]);
  });

  it("returns null when the dragged card is gone from the payload or is closed", () => {
    expect(dropIntent({ payloadCards, columnKeys, sortMode: "manual", dragIid: 12345, destColumnKey: "", anchor: null })).toBeNull();
    expect(dropIntent({ payloadCards, columnKeys, sortMode: "manual", dragIid: 99, destColumnKey: "", anchor: null })).toBeNull();
  });
});

// FREEZE-TEST 3 (PRD #102, Decision 7b's third case; task #13).
//
// M5 could not express this. It needs a CARD-level render filter to discriminate,
// and M5 had none — hideEmpty hides whole columns, so any implementation of the
// freeze passes it vacuously. M6's non-PRD toggle is the first such filter, which is
// why this test lands here rather than with the other two.
//
// The rule it protects: the freeze is computed from the PAYLOAD set, never from what
// the viewer can see. Compute it from the rendered subset and ClearBoardOrderExcept
// NULLs every card the viewer had hidden, dropping them to the bottom of their
// column — on the same person's other browser, where the toggle happens to be on.
describe("freeze-test 3: a freeze taken with cards hidden", () => {
  const columnKeys = [""];
  // One lane, PRD and non-PRD interleaved. The interleaving is the point: with the
  // hidden cards grouped at one end, an implementation that appended them instead of
  // preserving their positions would still pass.
  const payloadCards = [
    aCard({ iid: 10, column: "", labels: ["PRD"] }),
    aCard({ iid: 20, column: "", labels: ["bug"] }),
    aCard({ iid: 30, column: "", labels: ["PRD"] }),
    aCard({ iid: 40, column: "", labels: [] }),
    aCard({ iid: 50, column: "", labels: ["PRD"] }),
  ];
  const hidden = [20, 40];

  it("leaves the hidden cards' relative order unchanged", () => {
    // A viewer with the toggle OFF sees 10, 30, 50 and drags 50 to the top.
    const seen = visibleCards(payloadCards, "PRD", false);
    expect(seen.map((c) => c.iid)).toEqual([10, 30, 50]);

    const out = dropIntent({
      payloadCards, // UNFILTERED — this is the rule under test
      columnKeys,
      sortMode: "manual",
      dragIid: 50,
      destColumnKey: "",
      anchor: { iid: 10, before: true },
    })!;

    // Every hidden card survives the freeze...
    for (const iid of hidden) expect(out.iids).toContain(iid);
    // ...and in the same relative order it had before, which is what the viewer with
    // the toggle ON will see.
    expect(out.iids.filter((iid) => hidden.includes(iid))).toEqual(hidden);
    // The dragged card did move, so the fixture is not passing by doing nothing.
    expect(out.iids[0]).toBe(50);
  });

  it("would lose them entirely if the freeze used the rendered set", () => {
    // The positive control. Without it "the hidden cards are in the list" is a claim
    // about a fixture, not about the implementation.
    const broken = dropIntent({
      payloadCards: visibleCards(payloadCards, "PRD", false),
      columnKeys,
      sortMode: "manual",
      dragIid: 50,
      destColumnKey: "",
      anchor: { iid: 10, before: true },
    })!;
    for (const iid of hidden) expect(broken.iids).not.toContain(iid);
  });
});

describe("neighbourAnchor", () => {
  const lane = [aCard({ iid: 30 }), aCard({ iid: 10 }), aCard({ iid: 20 })];

  it("up targets the card above, before it", () => {
    expect(neighbourAnchor(lane, 10, "up")).toEqual({ iid: 30, before: true });
  });

  it("down targets the card below, after it", () => {
    expect(neighbourAnchor(lane, 10, "down")).toEqual({ iid: 20, before: false });
  });

  it("returns null at the ends, so a stray press cannot produce a bogus freeze", () => {
    expect(neighbourAnchor(lane, 30, "up")).toBeNull();
    expect(neighbourAnchor(lane, 20, "down")).toBeNull();
  });

  it("returns null for a card that is not in the lane", () => {
    expect(neighbourAnchor(lane, 999, "up")).toBeNull();
  });

  it("produces an anchor that MOVES the card exactly one place", () => {
    // The property that matters, asserted rather than assumed: a keyboard press must
    // be a single-step move, and it must go through boardOrderAfterDrop to get there.
    const up = neighbourAnchor(lane, 10, "up")!;
    expect(boardOrderAfterDrop(lane, [""], 10, "", up)).toEqual([10, 30, 20]);
    const down = neighbourAnchor(lane, 10, "down")!;
    expect(boardOrderAfterDrop(lane, [""], 10, "", down)).toEqual([30, 20, 10]);
  });
});
