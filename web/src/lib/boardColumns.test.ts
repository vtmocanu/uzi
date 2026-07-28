import { describe, it, expect } from "vitest";
import { visibleColumns, type BoardColumn } from "./boardColumns";

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
