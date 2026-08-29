import { describe, it, expect } from "vitest";
import { boundedChips, chipLabels, hoistLabels, MAX_CARD_CHIPS, type LabelChipExclusions } from "./labelChips";

// The default settings values, so a case that renames one is visibly a rename.
const defaults: LabelChipExclusions = {
  autopilotLabel: "autopilot",
  columnLabels: ["Planned", "In Progress", "Human Review", "Later"],
};

describe("chipLabels", () => {
  it("keeps content labels and drops the autopilot label + columns", () => {
    // PRD #764: `uzi` and PRD are no longer special to chipLabels — they chip as
    // ordinary labels. Only the autopilot marker and the column labels are excluded.
    expect(
      chipLabels(["uzi", "bug", "In Progress", "autopilot", "security", "PRD"], defaults),
    ).toEqual(["uzi", "bug", "security", "PRD"]);
  });

  it("preserves input order rather than sorting", () => {
    // The board renders chips in the forge's label order; a sort here would silently
    // reorder every card's chips relative to the issue page.
    expect(chipLabels(["security", "bug", "tech-debt"], defaults)).toEqual([
      "security",
      "bug",
      "tech-debt",
    ]);
  });

  it("reads the autopilot label from settings, never from a hardcoded name", () => {
    // The autopilot label is operator-configurable. Under a renamed setting the DEFAULT
    // name becomes an ordinary content label and must chip; the configured one must not.
    const renamed: LabelChipExclusions = {
      autopilotLabel: "robot",
      columnLabels: ["Doing"],
    };
    expect(
      chipLabels(["autopilot", "In Progress", "robot", "Doing", "bug"], renamed),
    ).toEqual(["autopilot", "In Progress", "bug"]);
  });

  it("excludes whatever column set it is handed — the two callers pass different ones", () => {
    // The suggester's set is unsaved edit state, so a label just added as a column
    // must stop being offered even though board.columns has not caught up.
    const saved = chipLabels(["bug", "Triage"], defaults);
    const editing = chipLabels(["bug", "Triage"], { ...defaults, columnLabels: [...defaults.columnLabels, "Triage"] });
    expect(saved).toEqual(["bug", "Triage"]);
    expect(editing).toEqual(["bug"]);
  });

  it("drops the empty string so the implicit Backlog column never becomes a chip", () => {
    // Board cards in the implicit lane carry column === "", and IssueView passes
    // [issue.column] as the exclusion set — an empty entry must not turn into an
    // empty-looking chip from either side.
    expect(chipLabels(["", "bug"], { ...defaults, columnLabels: [""] })).toEqual(["bug"]);
  });

  it("returns nothing when every label is the autopilot marker or a column", () => {
    expect(chipLabels(["autopilot", "In Progress"], defaults)).toEqual([]);
  });
});

describe("hoistLabels", () => {
  it("moves matched labels to the front, keeping the rest in place", () => {
    expect(hoistLabels(["web", "bug", "k8s"], ["bug"])).toEqual(["bug", "web", "k8s"]);
  });

  it("preserves the input order among the hoisted labels", () => {
    // Two matches: they lead, but in the order they appeared in `labels`, not in the
    // order of `hoist`.
    expect(hoistLabels(["a", "bug", "b", "security", "c"], ["security", "bug"])).toEqual([
      "bug",
      "security",
      "a",
      "b",
      "c",
    ]);
  });

  it("is a no-op when nothing matches or hoist is empty", () => {
    expect(hoistLabels(["a", "b", "c"], ["bug"])).toEqual(["a", "b", "c"]);
    expect(hoistLabels(["a", "b", "c"], [])).toEqual(["a", "b", "c"]);
  });

  it("does not dedup — a repeated label survives, once per occurrence", () => {
    expect(hoistLabels(["a", "bug", "b", "bug"], ["bug"])).toEqual(["bug", "bug", "a", "b"]);
  });

  it("does not mutate its input", () => {
    const input = ["web", "bug", "k8s"];
    const out = hoistLabels(input, ["bug"]);
    out.push("mutated");
    expect(input).toEqual(["web", "bug", "k8s"]);
  });

  it("keeps a hoisted match inside the cap that would otherwise overflow it", () => {
    // Decision 11: the "why this card is here" chip must survive MAX_CARD_CHIPS.
    // Five chips at a cap of 4 drops the last one; hoisting bug first keeps it shown.
    const chips = ["a", "b", "c", "d", "bug"];
    expect(boundedChips(chips, 4).shown).not.toContain("bug");
    expect(boundedChips(hoistLabels(chips, ["bug"]), 4).shown).toContain("bug");
  });
});

describe("boundedChips", () => {
  it("shows everything and reports no overflow below the cap", () => {
    expect(boundedChips(["a", "b"], 4)).toEqual({ shown: ["a", "b"], overflow: 0, hidden: [] });
  });

  it("shows everything at exactly the cap — an overflow of 0 must not render a +0", () => {
    expect(boundedChips(["a", "b", "c", "d"], 4)).toEqual({
      shown: ["a", "b", "c", "d"],
      overflow: 0,
      hidden: [],
    });
  });

  it("splits above the cap and keeps the withheld labels for the tooltip", () => {
    expect(boundedChips(["a", "b", "c", "d", "e", "f"], 4)).toEqual({
      shown: ["a", "b", "c", "d"],
      overflow: 2,
      hidden: ["e", "f"],
    });
  });

  it("defaults to MAX_CARD_CHIPS", () => {
    const many = Array.from({ length: MAX_CARD_CHIPS + 3 }, (_, i) => `l${i}`);
    expect(boundedChips(many).shown).toHaveLength(MAX_CARD_CHIPS);
    expect(boundedChips(many).overflow).toBe(3);
  });

  it("does not mutate or alias its input", () => {
    const input = ["a", "b"];
    const out = boundedChips(input, 4);
    out.shown.push("c");
    expect(input).toEqual(["a", "b"]);
  });
});
