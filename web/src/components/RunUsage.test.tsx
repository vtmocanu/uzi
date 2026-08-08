// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { RunUsagePanel } from "./RunUsage";
import { deriveRunUsage } from "../lib/runUsage";
import type { RunMessage } from "../lib/api";

afterEach(cleanup);

let seq = 0;
function m(kind: string, agent: string | null, payload: unknown): RunMessage {
  return { seq: ++seq, kind, agent, agent_instance: null, agent_label: null, payload, created_at: "2026-07-12T00:00:00Z" };
}

// A cumulative result frame. `modelUsage` is what the panel's figures come from
// (issue #195) — the top-level `usage` beside it is a real frame's second, DISAGREEING
// reading, kept here so this fixture cannot be satisfied by the field the run page
// used to read. Do not tidy the two into agreement.
function result(
  cum: { input: number; cacheRead: number; cacheCreation?: number; output: number; cost: number },
  meta: { turns: number; durationMs: number },
): RunMessage {
  return m("status", "lead", {
    event: "result",
    subtype: "success",
    num_turns: meta.turns,
    duration_ms: meta.durationMs,
    total_cost_usd: 9.99, // sentinel: never read
    usage: { input_tokens: 1, cache_read_input_tokens: 1, cache_creation_input_tokens: 1, output_tokens: 1 },
    modelUsage: {
      "claude-sonnet-5": {
        inputTokens: cum.input,
        outputTokens: cum.output,
        cacheReadInputTokens: cum.cacheRead,
        cacheCreationInputTokens: cum.cacheCreation ?? 0,
        costUSD: cum.cost,
      },
    },
  });
}

// A two-phase run: plan + one implement iteration, with per-agent assistant usage.
function twoPhase(): RunMessage[] {
  seq = 0;
  return [
    m("status", "lead", { event: "init", model: "claude-sonnet-5" }),
    m("text", "lead", { text: "planning", usage: { input_tokens: 20_000, cache_read_input_tokens: 180_000, cache_creation_input_tokens: 1_000, output_tokens: 6_000 } }),
    result({ input: 21_400, cacheRead: 188_000, output: 6_100, cost: 0.24 }, { turns: 9, durationMs: 100_000 }),
    m("text", "coder", { text: "implementing", usage: { input_tokens: 50_000, cache_read_input_tokens: 600_000, cache_creation_input_tokens: 0, output_tokens: 28_000 } }),
    result({ input: 80_300, cacheRead: 800_300, output: 34_800, cost: 1.26 }, { turns: 34, durationMs: 200_000 }),
  ];
}

describe("RunUsagePanel", () => {
  it("renders the strip totals, per-phase deltas, and per-agent attribution", () => {
    const { getByText, getAllByText } = render(<RunUsagePanel usage={deriveRunUsage(twoPhase())} />);

    // Strip: tokens-in = fresh(80.3k) + cached(800.3k) = 880.6k, model, cache bar.
    expect(getByText("Tokens in")).toBeTruthy();
    expect(getByText("880.6k")).toBeTruthy();
    expect(getByText(/from cache/)).toBeTruthy();
    expect(getByText("claude-sonnet-5")).toBeTruthy();
    // The run total cost shows in both the strip and the per-phase total row (they
    // agree by construction — the strip is the sum of the phase deltas).
    expect(getAllByText("$1.26").length).toBeGreaterThanOrEqual(1);

    // Per-phase table: labelled rows + the differenced iteration + a run total.
    expect(getByText("Plan")).toBeTruthy();
    expect(getByText("Implement · iteration 1")).toBeTruthy();
    expect(getByText("Run total")).toBeTruthy();
    // The iteration-1 delta fresh = 80.3k − 21.4k = 58.9k (unique to that row).
    expect(getByText("58.9k")).toBeTruthy();

    // Per-agent table: one row per agent + the attribution footnote.
    expect(getByText("lead")).toBeTruthy();
    expect(getByText("coder")).toBeTruthy();
    expect(getByText("Attributed total")).toBeTruthy();
    expect(getByText(/may not sum to the run total/)).toBeTruthy();
  });

  it("renders nothing for a run with no usage (pre-feature)", () => {
    const { container } = render(<RunUsagePanel usage={deriveRunUsage([m("text", "lead", { text: "hi" })])} />);
    expect(container.firstChild).toBeNull();
  });

  it("names the strip with role=group, so the aria-label is actually exposed", () => {
    // A bare div defaults to role `generic`, on which ARIA does not permit a name:
    // Chrome exposes it anyway, NVDA/VoiceOver generally do not. Asserting the ROLE
    // rather than just the label is the point — the label was already there and was
    // unreliable, so a getByLabelText alone would have passed over the defect.
    seq = 0;
    const { container } = render(<RunUsagePanel usage={deriveRunUsage(twoPhase())} />);
    const strip = container.querySelector('[aria-label="Run usage totals"]');
    expect(strip).toBeTruthy();
    expect(strip?.getAttribute("role")).toBe("group");
  });

  it("never labels the strip 100% from cache while fresh tokens exist", () => {
    // 99.6% cache — the band real runs actually sit in. Math.round put "100% from
    // cache" on the strip beside a zero-width warn segment while 4k fresh tokens were
    // rendered two cells away.
    seq = 0;
    const { container } = render(
      <RunUsagePanel
        usage={deriveRunUsage([
          result({ input: 4_000, cacheRead: 996_000, output: 100, cost: 1 }, { turns: 1, durationMs: 1 }),
        ])}
      />,
    );
    expect(container.textContent).toContain("99% from cache");
    expect(container.textContent).not.toContain("100% from cache");
    // And the bar still spans the full width: the two segments sum to 100%.
    const widths = [...container.querySelectorAll<HTMLElement>('[role="img"] > span')].map((s) => s.style.width);
    expect(widths).toEqual(["99%", "1%"]);
  });
});

// PRD #93: the per-agent Model column. `model` rides the same frame as `usage`.
function assistantFrame(agent: string, out: number, model?: string): RunMessage {
  return m("text", agent, {
    text: "…",
    ...(model ? { model } : {}),
    usage: { input_tokens: 1_000, cache_read_input_tokens: 0, cache_creation_input_tokens: 0, output_tokens: out },
  });
}
// Only here to make hasConfirmed true so the confirmed surfaces render; its modelUsage
// model is never displayed (the per-agent Model column reads assistant frames, the strip
// reads init).
function resultFrame(): RunMessage {
  return result({ input: 3_000, cacheRead: 0, output: 300, cost: 0.5 }, { turns: 3, durationMs: 5_000 });
}

// The panel renders two tables; the per-agent one is identified by its first header
// cell ("Agent" vs the per-phase table's "Phase"), so these helpers stay correct
// even if the per-phase table changes shape.
function agentTable(container: HTMLElement): HTMLTableElement {
  const table = [...container.querySelectorAll("table")].find(
    (t) => t.querySelector("thead th")?.textContent?.trim() === "Agent",
  );
  if (!table) throw new Error("per-agent table not found");
  return table;
}
function headerTexts(table: HTMLTableElement): string[] {
  return [...table.querySelectorAll("thead th")].map((th) => th.textContent?.trim() ?? "");
}
/** Every body row's cell at column `col`, in row order (last row is the total). */
function columnTexts(table: HTMLTableElement, col: number): string[] {
  return [...table.tBodies[0].rows].map((r) => r.cells[col]?.textContent?.trim() ?? "");
}

describe("RunUsagePanel model column (PRD #93)", () => {
  it("places Model immediately after Agent in the per-agent header row", () => {
    seq = 0;
    const { container } = render(
      <RunUsagePanel
        usage={deriveRunUsage([
          assistantFrame("lead", 100, "claude-opus-4-8"),
          assistantFrame("coder", 200, "claude-sonnet-5"),
          resultFrame(),
        ])}
      />,
    );
    // The PRD and the approved mock both place Model right after Agent; without
    // this, moving the column anywhere in the row leaves every other test green.
    expect(headerTexts(agentTable(container))).toEqual(["Agent", "Model", "In (fresh)", "In (cached)", "Out"]);
  });

  it("renders each agent's model and 'N models' on the total row for a mixed run", () => {
    seq = 0;
    const { getByText, getAllByText } = render(
      <RunUsagePanel
        usage={deriveRunUsage([
          assistantFrame("lead", 100, "claude-opus-4-8"),
          assistantFrame("coder", 200, "claude-sonnet-5"),
          resultFrame(),
        ])}
      />,
    );

    expect(getByText("Model")).toBeTruthy(); // the new per-agent header
    expect(getAllByText("claude-opus-4-8").length).toBe(1);
    expect(getByText("claude-sonnet-5")).toBeTruthy();
    expect(getByText("2 models")).toBeTruthy(); // the "Attributed total" cell
  });

  it("suffixes '+1' for an agent that ran on more than one model", () => {
    seq = 0;
    const { getByText } = render(
      <RunUsagePanel
        // The minority model is seen FIRST, so the cell cannot be satisfied by a
        // first-seen implementation — it must render the most frequent one.
        usage={deriveRunUsage([
          assistantFrame("coder", 100, "claude-opus-4-8"),
          assistantFrame("coder", 100, "claude-sonnet-5"),
          assistantFrame("coder", 100, "claude-sonnet-5"),
          resultFrame(),
        ])}
      />,
    );
    expect(getByText("claude-sonnet-5 +1")).toBeTruthy();
    // One agent, two distinct models → the total row still reads "2 models".
    expect(getByText("2 models")).toBeTruthy();
  });

  it("shows the single model string on the total row when the run used exactly one", () => {
    seq = 0;
    const { getAllByText } = render(
      <RunUsagePanel
        usage={deriveRunUsage([
          assistantFrame("lead", 100, "claude-opus-4-8"),
          assistantFrame("coder", 200, "claude-opus-4-8"),
          resultFrame(),
        ])}
      />,
    );
    // Two agent rows + the total row all read the same model string.
    expect(getAllByText("claude-opus-4-8").length).toBe(3);
  });

  it("clips a pathologically long model id instead of widening the table", () => {
    seq = 0;
    const long = `claude-${"x".repeat(200)}-4-8`;
    const { container, getAllByText } = render(
      <RunUsagePanel usage={deriveRunUsage([assistantFrame("lead", 100, long), resultFrame()])} />,
    );
    // The clip has to be on an inner block-level span: `max-w` on a bare <td> is not
    // reliably honored by table layout, so asserting it on the <td> would prove nothing.
    const span = agentTable(container).tBodies[0].rows[0].cells[1].querySelector("span");
    expect(span?.className).toContain("truncate");
    expect(span?.className).toMatch(/\bblock\b/);
    expect(span?.className).toMatch(/max-w-\[/);
    // Truncation is visual only — the value is intact in the DOM (agent row + the
    // single-model total row) and on hover.
    expect(getAllByText(long).length).toBe(2);
    expect(span?.getAttribute("title")).toBe(long);
    // Issue #163 (a11y): the clipped id must still be reachable and announced in full —
    // aria-label carries the whole id (the same value as title) and the span is a tab stop.
    expect(span?.getAttribute("aria-label")).toBe(long);
    expect(span?.getAttribute("tabindex")).toBe("0");
  });

  it("renders '—' in the Model column for a pre-feature run (usage, no models)", () => {
    seq = 0;
    const { container } = render(
      <RunUsagePanel usage={deriveRunUsage([assistantFrame("lead", 100), resultFrame()])} />,
    );
    const table = agentTable(container);
    expect(headerTexts(table)[1]).toBe("Model");
    // Read the Model cell BY COLUMN INDEX, so this is anchored to the column rather
    // than to "a dash exists somewhere in the panel" (money() emits dashes too):
    // the agent row and the "Attributed total" row must BOTH hold exactly the dash.
    expect(columnTexts(table, 0)).toEqual(["lead", "Attributed total"]);
    expect(columnTexts(table, 1)).toEqual(["—", "—"]);
  });
});

describe("RunUsagePanel $0 cost (Decision 8)", () => {
  it("renders a $0 cost as '—' in the strip and per-phase total, never '$0.00'", () => {
    seq = 0;
    // A subscription-auth run: real tokens, zero cost. Note it is `costUSD: 0` on the
    // model entry that matters now, not the frame's `total_cost_usd` (issue #195).
    const messages = [result({ input: 1000, cacheRead: 0, output: 200, cost: 0 }, { turns: 3, durationMs: 5000 })];
    const { container } = render(<RunUsagePanel usage={deriveRunUsage(messages)} />);
    expect(container.textContent).toContain("—");
    expect(container.textContent).not.toContain("$0.00");
  });
});

// Issue #152: `Td`'s colour branch was ADDITIVE — a left cell got `text-muted text-fg`
// both. Equal specificity, so stylesheet order decided it, and `.text-fg` precedes
// `.text-muted` in the built CSS (re-measured 2026-07-27 on `npm run build`: 24360 vs
// 24602; the issue measured 24294/24536, so the ORDER is the durable fact, not the
// offsets). `text-muted` therefore always won and the
// left-cell rule was dead. The Agent, Phase and Model columns therefore rendered dim
// against the approved mocks.
//
// Asserting BOTH halves is the point: `expect(classes).toContain("text-fg")` alone passes
// against the buggy code, because the buggy code emits `text-fg` too — it just also emits
// the class that beats it. The absence is the whole assertion.
function tableWithHeader(container: HTMLElement, first: string): HTMLTableElement {
  const table = [...container.querySelectorAll("table")].find(
    (t) => t.querySelector("thead th")?.textContent?.trim() === first,
  );
  if (!table) throw new Error(`table with first header "${first}" not found`);
  return table;
}
function cellClasses(table: HTMLTableElement, row: number, col: number): string[] {
  const cell = table.tBodies[0]!.rows[row]?.cells[col];
  if (!cell) throw new Error(`no cell at row ${row}, col ${col}`);
  return [...cell.classList];
}

describe("RunUsagePanel cell colour is exclusive (issue #152)", () => {
  it("gives a left-aligned body cell text-fg and NOT text-muted", () => {
    const { container } = render(<RunUsagePanel usage={deriveRunUsage(twoPhase())} />);

    for (const [label, table, col] of [
      ["phase", tableWithHeader(container, "Phase"), 0],
      ["agent", tableWithHeader(container, "Agent"), 0],
      ["model", tableWithHeader(container, "Agent"), 1], // PRD #93's Model column, `left mono`
    ] as const) {
      const classes = cellClasses(table, 0, col);
      expect(classes, `${label} column must be readable`).toContain("text-fg");
      expect(classes, `${label} column must NOT also carry the muted class that beats it`).not.toContain("text-muted");
    }
  });

  it("leaves the numeric columns muted, so the fix did not just brighten everything", () => {
    const { container } = render(<RunUsagePanel usage={deriveRunUsage(twoPhase())} />);
    // Right-aligned, non-total: still the dim treatment the mock asks for. Without this,
    // `text-fg` everywhere would pass the test above and lose the whole visual hierarchy.
    const classes = cellClasses(tableWithHeader(container, "Phase"), 0, 1);
    expect(classes).toContain("text-muted");
    expect(classes).not.toContain("text-fg");
  });

  it("keeps the total row emphasised and unmuted, left cell included", () => {
    const { container } = render(<RunUsagePanel usage={deriveRunUsage(twoPhase())} />);
    const table = tableWithHeader(container, "Phase");
    const last = table.tBodies[0]!.rows.length - 1;
    for (const col of [0, 1]) {
      const classes = cellClasses(table, last, col);
      expect(classes).toContain("font-semibold");
      expect(classes).toContain("text-fg");
      expect(classes).not.toContain("text-muted");
    }
  });
});

// Item 8: three mechanical a11y defects, all pre-existing, found by web-ux while measuring
// the #152 contrast fix. Attribute assertions, so the positive control is cheap — drop the
// attribute, confirm the case reds — and therefore still required.
describe("RunUsagePanel accessibility (item 8)", () => {
  function panel() {
    return render(<RunUsagePanel usage={deriveRunUsage(twoPhase())} />);
  }

  it("names both scroll regions, and adds NO tab stop (WCAG 2.1.1 is already satisfied)", () => {
    const { container } = panel();
    const wrappers = [...container.querySelectorAll("div.overflow-x-auto")];
    // Both breakdown tables, not one: the per-agent table overflows harder than the
    // per-phase one (600 vs 560 minimum width against a 301px viewport).
    expect(wrappers.length).toBe(2);
    for (const w of wrappers) {
      expect(w.getAttribute("role")).toBe("region");
      // A region role without an accessible name is not exposed as a landmark at all.
      // …and the name must not repeat the role the AT announces immediately after it:
      // "Per-phase usage table, scrollable" then the table role reads as
      // "…table, scrollable … table, Per-phase usage". `scrollable` stays — that is the
      // part with no other way to be announced, and the reason the region earns its place.
      const label = w.getAttribute("aria-label") ?? "";
      expect(label).toMatch(/scrollable$/);
      expect(label).not.toMatch(/\btable\b/);
      // …and NO tabIndex, asserted rather than merely omitted. Chrome focuses overflowing
      // scrollers natively (web-ux, Chrome 150 at 375px: Tab lands here, ArrowRight scrolls
      // scrollLeft 0 -> 299), and it
      // does so ONLY while they overflow — so an unconditional tab stop is dead weight at
      // every desktop width. This assertion stops it being re-added as an "improvement" on
      // the strength of the property read that produced the original finding.
      //
      // It encodes a CHROME-SCOPED measurement, not a universal rule: see RunUsage.tsx for
      // what a future engine measurement would have to show, and why the answer there would
      // be a CONDITIONAL tabIndex rather than deleting this case.
      expect(w.hasAttribute("tabindex")).toBe(false);
    }
    // The two names must DIFFER: two adjacent scrollable data regions with one name is
    // the case a screen-reader user cannot navigate between.
    expect(wrappers[0]!.getAttribute("aria-label")).not.toBe(wrappers[1]!.getAttribute("aria-label"));
  });

  it("names both tables and scopes every column header (WCAG 1.3.1)", () => {
    const { container } = panel();
    const tables = [...container.querySelectorAll("table")];
    expect(tables.length).toBe(2);
    expect(tables.map((t) => t.getAttribute("aria-label"))).toEqual(["Per-phase usage", "Per-agent usage"]);
    // Every th is a COLUMN header here; a data cell read without its column is a number
    // with no meaning. Asserted over all of them so a new column cannot skip it.
    const ths = [...container.querySelectorAll("th")];
    expect(ths.length).toBeGreaterThan(0);
    for (const th of ths) expect(th.getAttribute("scope")).toBe("col");
  });

  it("hides the decorative disclosure triangles from assistive tech", () => {
    const { container } = panel();
    // <details>/<summary> conveys expanded state natively, so an announced triangle is a
    // second and contradictory reading of the same fact.
    const glyphs = [...container.querySelectorAll("summary span")].filter((s) =>
      /[▸▾]/.test(s.textContent ?? ""),
    );
    expect(glyphs.length).toBe(4); // two per <details>, one for each open state
    for (const g of glyphs) expect(g.getAttribute("aria-hidden")).toBe("true");
  });
});

// Issue #237: the LIVE / in-flight surface. It appears from the first assistant-usage
// frame (before any result frame confirms a billed total) and hands over to the confirmed
// surfaces the moment one lands. Input tokens only — no Out column, no cost — because
// per-call output is a message_start snapshot (see runUsage.ts).
function liveFrame(
  agent: string,
  model: string,
  u: { input: number; cacheRead: number; cacheCreation?: number; output: number },
): RunMessage {
  return m("text", agent, {
    text: "…",
    model,
    usage: {
      input_tokens: u.input,
      cache_read_input_tokens: u.cacheRead,
      cache_creation_input_tokens: u.cacheCreation ?? 0,
      output_tokens: u.output,
    },
  });
}

describe("RunUsagePanel live in-flight surface (issue #237)", () => {
  it("shows the in-flight tokens with ZERO result frames, and none of the confirmed billed surfaces", () => {
    seq = 0;
    const { container, getByText, queryByText, getAllByText } = render(
      <RunUsagePanel
        usage={deriveRunUsage([
          m("status", "lead", { event: "init", model: "claude-opus-4-8" }),
          liveFrame("lead", "claude-opus-4-8", { input: 40_000, cacheRead: 300_000, output: 5_000 }),
          liveFrame("coder", "claude-sonnet-5", { input: 60_000, cacheRead: 500_000, output: 8_000 }),
        ])}
      />,
    );

    // The live heading is unmistakable that these are provisional, not the billed figure.
    expect(getByText(/In-flight tokens · live, not billed yet/)).toBeTruthy();

    // The CONFIRMED billed surfaces are all absent: no strip, no per-phase table, no cost.
    expect(queryByText("Tokens in")).toBeNull();
    expect(queryByText("Per-phase breakdown")).toBeNull();
    expect(queryByText("Run total")).toBeNull();

    // No live output and no live cost: no "Out" header anywhere, and no "$" figure.
    expect(queryByText("Out")).toBeNull();
    expect(container.textContent).not.toContain("$");

    // Deduped input figures render, split fresh/cached, per model and per agent.
    expect(getByText("claude-opus-4-8")).toBeTruthy();
    expect(getByText("claude-sonnet-5")).toBeTruthy();
    // liveTotal fresh = 40k + 60k = 100.0k, cached = 300k + 500k = 800.0k — each total row
    // appears once in the per-model table and once in the per-agent table.
    expect(getAllByText("100.0k").length).toBe(2);
    expect(getAllByText("800.0k").length).toBe(2);

    // Exactly two live tables in two named, distinct scroll regions.
    const wrappers = [...container.querySelectorAll("div.overflow-x-auto")];
    expect(wrappers.length).toBe(2);
    const labels = wrappers.map((w) => w.getAttribute("aria-label") ?? "");
    for (const label of labels) {
      expect(label).toMatch(/scrollable$/);
      expect(label).not.toMatch(/\btable\b/);
    }
    expect(labels[0]).not.toBe(labels[1]);
    // Names distinct from the confirmed tables' names (which never render here).
    for (const label of labels) expect(label).not.toMatch(/Per-(phase|agent) usage/);
    const tables = [...container.querySelectorAll("table")];
    expect(tables.length).toBe(2);
    for (const th of container.querySelectorAll("th")) expect(th.getAttribute("scope")).toBe("col");
  });

  it("collapses a duplicate (agent_instance, usage) live pair to a single figure", () => {
    seq = 0;
    // Two byte-identical calls on the same lead-less lane (agent_instance null) → one record.
    const { queryByText, getAllByText } = render(
      <RunUsagePanel
        usage={deriveRunUsage([
          liveFrame("coder", "claude-sonnet-5", { input: 60_000, cacheRead: 500_000, output: 8_000 }),
          liveFrame("coder", "claude-sonnet-5", { input: 60_000, cacheRead: 500_000, output: 8_000 }),
        ])}
      />,
    );
    // Deduped: the coder lane reads 60.0k fresh, not the doubled 120.0k.
    expect(queryByText("120.0k")).toBeNull();
    // 60.0k appears as the per-model row + per-model total + per-agent row + per-agent total.
    expect(getAllByText("60.0k").length).toBe(4);
    expect(getAllByText("500.0k").length).toBe(4);
  });

  it("hands over to the confirmed surfaces once a result frame lands, hiding the live section", () => {
    seq = 0;
    const { getByText, queryByText, getAllByText } = render(
      <RunUsagePanel
        usage={deriveRunUsage([
          m("status", "lead", { event: "init", model: "claude-sonnet-5" }),
          liveFrame("coder", "claude-sonnet-5", { input: 60_000, cacheRead: 500_000, output: 8_000 }),
          result({ input: 80_000, cacheRead: 500_000, output: 8_000, cost: 1.5 }, { turns: 5, durationMs: 10_000 }),
        ])}
      />,
    );
    // Confirmed strip + cost are back…
    expect(getByText("Tokens in")).toBeTruthy();
    expect(getAllByText("$1.50").length).toBeGreaterThanOrEqual(1);
    // …and the live in-flight section is gone.
    expect(queryByText(/In-flight tokens/)).toBeNull();
  });
});
