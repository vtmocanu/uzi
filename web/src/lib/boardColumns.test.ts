import { describe, it, expect } from "vitest";
import { lanePaging, visibleColumns, type BoardColumn } from "./boardColumns";

// The implicit column keeps its key ("") and gains the display label "Backlog"
// (PRD #102 M1). visibleColumns keys off `key`, never `label`, so the rename is
// inert here — the fixture tracks the real board so a reader is not told the
// first lane is still called Open.
const cols: BoardColumn[] = [
  { key: "", label: "Backlog", droppable: true, accent: "bg-faint" },
  { key: "review", label: "review", droppable: true, accent: "bg-info" },
  { key: "__closed__", label: "Closed", droppable: false, accent: "bg-edge-strong" },
];

// Backlog holds 2 cards, review is empty, Closed holds 1.
const counts: Record<string, number> = { "": 2, review: 0, __closed__: 1 };
const count = (key: string) => counts[key] ?? 0;

describe("visibleColumns", () => {
  it("returns every column when hideEmpty is off", () => {
    expect(visibleColumns(cols, count, false, false)).toEqual(cols);
  });

  it("drops empty columns when hideEmpty is on and no drag is active", () => {
    expect(visibleColumns(cols, count, true, false).map((c) => c.key)).toEqual(["", "__closed__"]);
  });

  it("exempts no column — an empty Backlog or Closed lane hides too", () => {
    const allEmpty = () => 0;
    expect(visibleColumns(cols, allEmpty, true, false)).toEqual([]);
  });

  it("reveals every column during a drag so all lanes stay drop targets", () => {
    expect(visibleColumns(cols, count, true, true)).toEqual(cols);
  });

  it("is derived, not stored: a column hides the moment it empties and returns when it repopulates", () => {
    const emptyReview = (k: string) => (k === "review" ? 0 : 1);
    expect(visibleColumns(cols, emptyReview, true, false).map((c) => c.key)).toEqual([
      "",
      "__closed__",
    ]);
    const populated = () => 1;
    expect(visibleColumns(cols, populated, true, false).map((c) => c.key)).toEqual([
      "",
      "review",
      "__closed__",
    ]);
  });
});

describe("lanePaging", () => {
  // The caller owns cap (default 10) and page (50); the helper never hardcodes them.
  const CAP = 10;
  const PAGE = 50;
  const paging = (total: number, shownCount: number, searchActive = false) =>
    lanePaging({ total, shownCount, cap: CAP, page: PAGE, searchActive });

  it("shows a lane in full with no expander when total is within the baseline", () => {
    // total <= cap, no search: everything renders, nothing left, no affordances.
    expect(paging(7, CAP)).toEqual({
      render: 7,
      showMoreBy: 0,
      remaining: 0,
      canCollapse: false,
      nudgeSearch: false,
      countLabel: "",
    });
  });

  it("caps a deep lane at the default and offers a page-sized Show more", () => {
    // total > cap, shownCount at the default cap: render=cap, next reveal is a page
    // (bounded by what's left), and the lane shows an N/M count but no Collapse yet.
    expect(paging(200, CAP)).toEqual({
      render: 10,
      showMoreBy: 50,
      remaining: 190,
      canCollapse: false,
      nudgeSearch: true,
      countLabel: "10/200",
    });
  });

  it("bounds the next reveal by the remainder, not always a full page", () => {
    // 30 left with a page of 50 -> Show more reveals only 30.
    const p = paging(CAP + 30, CAP);
    expect(p.remaining).toBe(30);
    expect(p.showMoreBy).toBe(30);
  });

  it("after one Show more, renders cap+page, shrinks the remainder, and can collapse", () => {
    // shownCount = cap + page, total between cap+page and cap+2page.
    const total = CAP + PAGE + 20; // 80
    const p = paging(total, CAP + PAGE);
    expect(p.render).toBe(CAP + PAGE); // 60
    expect(p.remaining).toBe(20);
    expect(p.canCollapse).toBe(true);
    expect(p.countLabel).toBe(`60/${total}`);
  });

  it("when shownCount reaches total, everything renders and Show more disappears", () => {
    const p = paging(40, 40);
    expect(p).toEqual({
      render: 40,
      showMoreBy: 0,
      remaining: 0,
      canCollapse: true, // total (40) > baseline (cap 10): the lane was expanded
      nudgeSearch: false,
      countLabel: "",
    });
  });

  it("cannot collapse once fully shown if the lane never exceeded the baseline", () => {
    // total <= cap: render == total <= baseline, so there is nothing to collapse from.
    expect(paging(10, 10).canCollapse).toBe(false);
  });

  it("nudges toward search only past two pages of remainder", () => {
    // remaining exactly 2*page is NOT past it; one more card tips the nudge on.
    expect(paging(CAP + 2 * PAGE, CAP).nudgeSearch).toBe(false); // remaining 100
    expect(paging(CAP + 2 * PAGE + 1, CAP).nudgeSearch).toBe(true); // remaining 101
  });

  it("with search active lifts the cap: the baseline becomes one page", () => {
    // A small queried lane (total <= page) is fully shown with no expander...
    expect(paging(30, 0, true)).toEqual({
      render: 30,
      showMoreBy: 0,
      remaining: 0,
      canCollapse: false,
      nudgeSearch: false,
      countLabel: "",
    });
    // ...and a large one starts at a page, not at the cap, then pages from there.
    const big = paging(140, 0, true);
    expect(big.render).toBe(PAGE); // 50, not 10
    expect(big.remaining).toBe(90);
    expect(big.countLabel).toBe("50/140");
  });

  it("clamps a shownCount below the baseline up to the baseline", () => {
    // A stale shownCount of 0 must not render 0 cards; it snaps back to the cap.
    expect(paging(200, 0).render).toBe(CAP);
    // ...and to the page baseline while searching.
    expect(paging(200, 0, true).render).toBe(PAGE);
  });

  it("clamps negative inputs rather than producing negative counts", () => {
    const p = lanePaging({ total: -5, shownCount: -9, cap: -1, page: -2, searchActive: false });
    expect(p.render).toBe(0);
    expect(p.remaining).toBe(0);
    expect(p.showMoreBy).toBe(0);
    expect(p.canCollapse).toBe(false);
    expect(p.countLabel).toBe("");
  });
});
