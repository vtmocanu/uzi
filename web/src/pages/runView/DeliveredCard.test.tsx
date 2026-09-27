// @vitest-environment jsdom
//
// PRD #1798 M7: the "Delivered" section. The description fields are UNTRUSTED and were
// sanitized for the FORGE's markdown, so this surface must (a) undo that encoding for display
// and (b) render the result as inert text: no element built from field content, ever.
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import cases from "../../../../fixtures/pr-size-line/cases.json";
import { DeliveredCard, displayPrText, prDescriptionOutcomeNote, prSizeLine } from "./DeliveredCard";
import { RunSummary } from "../RunView";
import type { PrDescriptionSize, Run, RunPrDescription } from "../../lib/api";

afterEach(cleanup);

// The Map-backed localStorage the summary collapse pref needs (jsdom here ships none).
beforeEach(() => {
  const m = new Map<string, string>();
  const storage = {
    getItem: (k: string) => (m.has(k) ? m.get(k)! : null),
    setItem: (k: string, v: string) => void m.set(k, String(v)),
    removeItem: (k: string) => void m.delete(k),
    clear: () => m.clear(),
    key: (i: number) => [...m.keys()][i] ?? null,
    get length() {
      return m.size;
    },
  } as Storage;
  Object.defineProperty(window, "localStorage", { configurable: true, value: storage });
});

const ZWSP = "\u200B";

function size(over: Partial<PrDescriptionSize> = {}): PrDescriptionSize {
  const z = { added: 0, deleted: 0 };
  return {
    unavailable: false,
    files: 2,
    code: { added: 1810, deleted: 12 },
    tests: { added: 40, deleted: 0 },
    docs: z,
    config: z,
    generated: z,
    vendored: z,
    ...over,
  };
}

function desc(over: Partial<RunPrDescription["fields"]> = {}, rest: Partial<RunPrDescription> = {}): RunPrDescription {
  return {
    mr_iid: 12,
    source: "generated",
    fields: {
      summary: "Adds a token-bucket rate limiter to the API.",
      changes: ["New middleware in the router."],
      scope_notes: [{ kind: "deferred", text: "Per-user quotas." }],
      review_pointers: ["Start with the bucket refill math."],
      verification: [{ command: "task gate:api", result: "pass", verified_at_sha: "abcdef0123456789" }],
      ...over,
    },
    size: size(),
    base_sha: "1111111",
    head_sha: "2222222",
    target_branch: "main",
    published_at: "2026-09-27T10:00:00Z",
    ...rest,
  };
}

function run(over: Partial<Run> = {}): Run {
  return { id: "r1", status: "completed", ...over } as Run;
}

describe("prSizeLine (cross-language fixture, twin of the CLI's prSizeBody)", () => {
  for (const c of cases) {
    it(c.name, () => {
      expect(prSizeLine(c.size as PrDescriptionSize)).toBe(c.expected);
    });
  }
  it("null size renders no line", () => {
    expect(prSizeLine(null)).toBeNull();
    expect(prSizeLine(undefined)).toBeNull();
  });
});

describe("displayPrText", () => {
  it("undoes the sanitizer's backslash escapes and entity encoding", () => {
    expect(displayPrText("\\[x\\](y)")).toBe("[x](y)");
    expect(displayPrText("&lt;b&gt;bold&lt;/b&gt;")).toBe("<b>bold</b>");
    expect(displayPrText("\\# heading")).toBe("# heading");
    expect(displayPrText("Map&lt;string, int&gt; &amp; more")).toBe("Map<string, int> & more");
    expect(displayPrText("&#39;q&#39; &quot;d&quot;")).toBe("'q' \"d\"");
    // One pass only: an encoded entity decodes to its literal text, never on to `<`.
    expect(displayPrText("&amp;lt;")).toBe("&lt;");
    // Left to right, as a CommonMark forge reads it: the lead's `a \< b` is stored as
    // `a \&lt; b`; the escaped `&` consumes the ampersand, so it reads `a &lt; b`, not `a \< b`.
    expect(displayPrText("a \\&lt; b")).toBe("a &lt; b");
    expect(displayPrText("\\&amp;")).toBe("&amp;");
    // A backslash before a non-punctuation character is literal in CommonMark: kept.
    expect(displayPrText("C:\\path")).toBe("C:\\path");
  });
  it("keeps the sanitizer's zero-width breakers and strips other format runes", () => {
    expect(displayPrText(`@${ZWSP}user`)).toBe(`@${ZWSP}user`);
    expect(displayPrText("a\u202Eb")).toBe("ab");
    expect(displayPrText(42)).toBe("");
  });
});

describe("prDescriptionOutcomeNote", () => {
  it("is silent for published or absent outcomes", () => {
    expect(prDescriptionOutcomeNote(null)).toBeNull();
    expect(prDescriptionOutcomeNote(undefined)).toBeNull();
    expect(prDescriptionOutcomeNote("published")).toBeNull();
  });
  it("maps every skip/failure outcome to its own sentence", () => {
    const outcomes = [
      "skipped_human_edit",
      "skipped_no_region",
      "skipped_malformed",
      "skipped_snapshot_moved",
      "write_failed",
    ];
    const notes = outcomes.map(prDescriptionOutcomeNote);
    for (const n of notes) expect(n).toMatch(/^Last PR update (skipped|failed): /);
    expect(new Set(notes).size).toBe(outcomes.length);
    expect(prDescriptionOutcomeNote("skipped_human_edit")).toBe(
      "Last PR update skipped: a human edited the description.",
    );
  });
  it("names an unknown future outcome instead of dropping it", () => {
    expect(prDescriptionOutcomeNote("skipped_new_thing")).toBe(
      "Last PR update was not published (skipped_new_thing).",
    );
  });
});

describe("DeliveredCard", () => {
  it("renders nothing when the run has no published description", () => {
    for (const pr of [null, undefined]) {
      const { container } = render(<DeliveredCard run={run({ pr_description: pr })} />);
      expect(container.innerHTML).toBe("");
      cleanup();
    }
  });

  it("renders summary, size line, changes, checks, scope notes and pointers", () => {
    render(<DeliveredCard run={run({ pr_description: desc(), pr_description_outcome: "published" })} />);
    const section = screen.getByRole("region", { name: "Delivered" });
    const text = section.textContent ?? "";
    expect(text).toContain("Adds a token-bucket rate limiter to the API.");
    expect(text).toContain("Size: code +1,810 \u221212 \u00b7 tests +40 \u22120 \u00b7 2 files");
    expect(within(section).getByText("What changed")).toBeTruthy();
    expect(text).toContain("New middleware in the router.");
    expect(text).toContain("task gate:api");
    expect(text).toContain("Reported by the agent at abcdef0");
    expect(text).not.toContain("abcdef01");
    expect(text).toContain("deferred");
    expect(text).toContain("Per-user quotas.");
    expect(text).toContain("Start with the bucket refill math.");
    // Positive half of the outcome-note pair: the note's lead-in is absent on "published".
    expect(text).not.toContain("Last PR update");
  });

  it("renders hostile, sanitizer-escaped fields as inert text", () => {
    const hostile = desc({
      summary: "&lt;img src=x onerror=alert(1)&gt; then \\[x\\](javascript:alert(1))",
      changes: [`@${ZWSP}user was pinged`, `C${ZWSP}loses #1`, "&lt;b&gt;bold&lt;/b&gt;"],
      scope_notes: [{ kind: "added", text: "&lt;script&gt;alert(1)&lt;/script&gt;" }],
      review_pointers: ["\\[link\\]\\[ref\\]"],
      verification: [{ command: "&lt;a href=x&gt;go&lt;/a&gt;", result: "fail", verified_at_sha: "0123456789" }],
    });
    const { container } = render(<DeliveredCard run={run({ pr_description: hostile })} />);
    const text = container.textContent ?? "";

    // Unescaped for display: the reader sees what the lead wrote, as characters.
    expect(text).toContain("<img src=x onerror=alert(1)> then [x](javascript:alert(1))");
    expect(text).toContain("<b>bold</b>");
    expect(text).toContain("<script>alert(1)</script>");
    expect(text).toContain("[link][ref]");
    expect(text).toContain("<a href=x>go</a>");
    expect(text).toContain(`@${ZWSP}user was pinged`);
    expect(text).toContain(`C${ZWSP}loses #1`);
    expect(text).not.toContain("&lt;");

    // ...and none of it became markup. Each selector is checked against a control first so a
    // query that could never match cannot pass vacuously: the card does render li and code.
    expect(container.querySelectorAll("li").length).toBeGreaterThan(0);
    expect(container.querySelectorAll("code").length).toBe(1);
    for (const sel of ["img", "a", "script", "b", "[onerror]", "[href]"]) {
      expect(container.querySelectorAll(sel).length, sel).toBe(0);
    }
  });

  it("shows a muted note for a skipped last update", () => {
    render(
      <DeliveredCard run={run({ pr_description: desc(), pr_description_outcome: "skipped_human_edit" })} />,
    );
    expect(screen.getByText("Last PR update skipped: a human edited the description.")).toBeTruthy();
  });

  it("renders the size line alone for a deterministic-only description", () => {
    const empty = desc(
      { summary: "", changes: [], scope_notes: [], review_pointers: [], verification: [] },
      { source: "deterministic_only", size: size({ unavailable: true, files: 0, code: { added: 0, deleted: 0 }, tests: { added: 0, deleted: 0 } }) },
    );
    render(<DeliveredCard run={run({ pr_description: empty })} />);
    const section = screen.getByRole("region", { name: "Delivered" });
    expect(section.textContent).toBe("DeliveredSize: unavailable");
  });
});

describe("DeliveredCard with nothing published", () => {
  const emptyFields = { summary: "", changes: [], scope_notes: [], review_pointers: [], verification: [] };

  it("renders the outcome note alone when the first write failed (no description)", () => {
    const { container } = render(
      <DeliveredCard run={run({ pr_description: null, pr_description_outcome: "write_failed" })} />,
    );
    expect(container.textContent).toBe("Last PR update failed: the forge did not accept the new description.");
    expect(screen.queryByRole("region", { name: "Delivered" })).toBeNull();
    expect(container.querySelector("h3")).toBeNull();
  });

  it("renders the note alone for an empty description with a skipped outcome", () => {
    const { container } = render(
      <DeliveredCard run={run({ pr_description: desc(emptyFields, { size: null }), pr_description_outcome: "skipped_no_region" })} />,
    );
    expect(container.textContent).toBe("Last PR update skipped: the description no longer has a uzi section.");
    expect(container.querySelector("h3")).toBeNull();
  });

  it("renders nothing, not a bare heading, for an empty description and no note", () => {
    for (const outcome of [null, "published"]) {
      const { container } = render(
        <DeliveredCard run={run({ pr_description: desc(emptyFields, { size: null }), pr_description_outcome: outcome })} />,
      );
      expect(container.innerHTML).toBe("");
      cleanup();
    }
  });

  it("renders nothing with no description and no note", () => {
    const { container } = render(<DeliveredCard run={run({ pr_description: null, pr_description_outcome: null })} />);
    expect(container.innerHTML).toBe("");
  });
});

describe("RunSummary hosts the Delivered section", () => {
  it("shows Delivered even with no intent or plan summary, and collapses with the card", () => {
    render(<RunSummary run={run({ pr_description: desc() })} />);
    expect(screen.getByRole("region", { name: "Delivered" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Collapse" }));
    expect(screen.queryByRole("region", { name: "Delivered" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Expand" }));
    expect(screen.getByRole("region", { name: "Delivered" })).toBeTruthy();
  });

  it("still renders nothing with no summary and no description", () => {
    const { container } = render(<RunSummary run={run({ pr_description: null })} />);
    expect(container.innerHTML).toBe("");
  });

  it("renders nothing for an empty description with no note (no bare Delivered heading)", () => {
    const empty = desc({ summary: "", changes: [], scope_notes: [], review_pointers: [], verification: [] }, { size: null });
    const { container } = render(<RunSummary run={run({ pr_description: empty })} />);
    expect(container.innerHTML).toBe("");
  });

  it("shows the outcome note when nothing was ever published", () => {
    render(<RunSummary run={run({ pr_description: null, pr_description_outcome: "write_failed" })} />);
    expect(screen.getByText("Last PR update failed: the forge did not accept the new description.")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Delivered" })).toBeNull();
  });
});
