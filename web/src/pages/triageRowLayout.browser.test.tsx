// Real-browser layout regression for the two triage rows (Judge GroupRow, Findings FindingRow).
//
// Both rows are `flex flex-wrap`: checkbox, a `min-w-0 flex-1` text column, then a `shrink-0`
// action group (File issue · Mark done · Dismiss ▾ · chevron, ~300px). With a flex-basis of 0 the
// line never wraps, so at phone width the text column was squeezed to ~30px: one word per line,
// and a Findings location one character per line. `basis-64` on the text column makes the action
// group wrap below instead. jsdom has no layout, hence a browser test.
import "../index.css";
import { afterEach, describe, expect, it } from "vitest";
import { page } from "vitest/browser";
import { cleanup, render, screen } from "@testing-library/react";
import { GroupRow } from "./judge/GroupRow";
import { FindingRow } from "./Findings";
import type { IncidentalFinding, JudgeRecommendationGroup } from "../lib/api";

afterEach(cleanup);

const noop = () => {};

// Long, unbroken target/location so the test also covers overflow, not only the squeeze.
const LONG_TARGET = "api/internal/workersvc/guardrail_read_only_access_to_agent_produced_artifacts";
const LONG_LOCATION = "api/internal/store/sweeper_background_reconciliation.go#sweepLoopWithTickerShutdown";

function judgeGroup(): JudgeRecommendationGroup {
  return {
    category: "improve_uzi",
    target: LONG_TARGET,
    bucket: "todo",
    open_count: 3,
    run_count: 3,
    rationale_preview: "The run waited about four minutes for the first poll tick after the label was applied.",
    occurrences: [
      { run_id: "run-1", run_title: "A run", review_id: "rev-1", rec_id: "rec-1", verdict: "issues", confidence: "", bucket: "todo" },
    ],
  };
}

function finding(): IncidentalFinding {
  return {
    finding_id: "f-1",
    location: LONG_LOCATION,
    repo_id: "repo-1",
    repo_path: "owner/repo",
    status: "open",
    last_title: "Leaked ticker in sweepLoop never stopped on shutdown",
    seen_in_runs: 2,
    evidence_preview: "ticker := time.NewTicker(d) with no Stop",
  };
}

function renderRows() {
  // p-4 mirrors the page gutter, so the row width matches what a phone actually gets.
  render(
    <ul className="space-y-2 p-4">
      <GroupRow group={judgeGroup()} selected={false} onToggleSelect={noop} onDispose={noop} repos={[]} onFiled={noop} />
      <FindingRow
        finding={finding()}
        selectable
        selected={false}
        onToggleSelect={noop}
        repoLabel="owner/repo"
        resolved={false}
        onFile={async () => {}}
        onDismiss={noop}
        onMarkDone={noop}
      />
    </ul>,
  );
  const rows = [...document.querySelectorAll("li")].filter((li) => li.parentElement?.tagName === "UL");
  const fileButtons = screen.getAllByRole("button", { name: "File issue" });
  return {
    judge: {
      row: rows[0],
      text: screen.getByText(/waited about four minutes/),
      file: fileButtons[0],
    },
    finding: {
      row: rows[1],
      text: screen.getByText(/Leaked ticker in sweepLoop/),
      code: screen.getByText(LONG_LOCATION),
      file: fileButtons[1],
    },
  };
}

const rect = (el: Element) => el.getBoundingClientRect();

describe("triage rows at phone width (390px)", () => {
  it("wraps the actions below the text instead of squeezing the text column", async () => {
    await page.viewport(390, 844);
    const { judge, finding: f } = renderRows();

    for (const { row, text, file } of [judge, f]) {
      // The squeezed column measured ~30px; a readable one spans most of the ~340px row.
      expect(rect(text).width).toBeGreaterThan(200);
      // The action group sits on its own line under the text.
      expect(rect(file).top).toBeGreaterThanOrEqual(rect(text).bottom);
      // Nothing pushes the row wider than its box.
      expect(row.scrollWidth).toBeLessThanOrEqual(row.clientWidth);
    }
    // The long location breaks across a few lines, not one character per line.
    expect(rect(f.code).width).toBeGreaterThan(200);
  });
});

describe("triage rows at desktop width (1280px)", () => {
  it("keeps the actions on the same line as the text", async () => {
    await page.viewport(1280, 900);
    const { judge, finding: f } = renderRows();

    for (const { row, text, file } of [judge, f]) {
      expect(rect(file).top).toBeLessThan(rect(text).bottom);
      expect(row.scrollWidth).toBeLessThanOrEqual(row.clientWidth);
    }
  });
});
