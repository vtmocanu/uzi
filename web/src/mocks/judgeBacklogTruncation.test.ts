// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { capBacklogRows, mockApi, MOCK_BACKLOG_MAX_ROWS, type JudgeBacklogRow } from "./mockApi";
import type { JudgeRecommendationGroup } from "../lib/api";

// The demo-mode truncation toggle (PRD #98, design note A8). Truncation was unreachable in
// demo mode -- `truncated` was hardcoded false at both sites -- so the one state in which the
// screen is NOT the truth was the one state a person could never see, and its CLI remedy was
// separately measured to have been outright false.
//
// What these tests are actually defending, and it is not the flag: the cut must remove ROWS
// BEFORE GROUPING. A mock that set `truncated: true` over complete data would pass any
// assertion about the flag while demoing the opposite of what the banner means, so every
// assertion below that matters is about a SURVIVING group's counts, never about the boolean.

const SCENARIO = "truncated-backlog";

function setScenario(v: string): void {
  window.history.replaceState({}, "", v ? `/judge?mock=${v}` : "/judge");
}

afterEach(() => setScenario(""));

const find = (groups: JudgeRecommendationGroup[], category: string, target: string) =>
  groups.find((g) => g.category === category && g.target === target);

const rowCount = (groups: JudgeRecommendationGroup[]) =>
  groups.reduce((n, g) => n + g.occurrences.length, 0);

describe("demo-mode backlog truncation: the cut", () => {
  it("is OFF by default, so an ordinary demo visitor never lands in this state", async () => {
    setScenario("");
    const b = await mockApi.getJudgeBacklog("all");
    expect(b.truncated, "the demo backlog truncates with no scenario set: this state must be opt-in").toBe(false);
    expect(b.groups.length, "the uncapped demo backlog no longer returns all 8 groups").toBe(8);
  });

  it("only demos something if the demo data outgrows the cap", async () => {
    setScenario("");
    const b = await mockApi.getJudgeBacklog("all");
    // Not a tautology: data.ts is free to shrink, and if it ever drops to MOCK_BACKLOG_MAX_ROWS
    // rows or fewer the toggle silently stops truncating and every assertion below still
    // passes for the wrong reason.
    expect(
      rowCount(b.groups),
      `fixture broken: the uncapped demo backlog has ${rowCount(b.groups)} rows and the cap is ` +
        `${MOCK_BACKLOG_MAX_ROWS} -- with no rows to remove the toggle demos nothing, and the ` +
        `assertions below would pass against an untruncated page`,
    ).toBeGreaterThan(MOCK_BACKLOG_MAX_ROWS);
  });

  it("cuts ROWS before grouping: a SURVIVING group under-reports run_count", async () => {
    setScenario("");
    const whole = (await mockApi.getJudgeBacklog("all")).groups;
    setScenario(SCENARIO);
    const cut = (await mockApi.getJudgeBacklog("all")).groups;

    // This is the assertion a flipped boolean cannot satisfy. improve_uzi/api/internal/poller
    // occurs in all three demo reviews; the cap keeps only the first, so the group SURVIVES
    // while its evidence chip drops from "seen in 3 runs" to "seen in 1". That understatement
    // is what the banner is warning about, and it exists only because rows were cut first.
    expect(
      find(whole, "improve_uzi", "api/internal/poller")?.run_count,
      "precondition: the uncapped coordinate must recur in 3 runs, or the cut below removes nothing",
    ).toBe(3);
    expect(
      find(cut, "improve_uzi", "api/internal/poller")?.run_count,
      "a SURVIVING group did not lose occurrences to the cut -- the rows were not cut before grouping. " +
        "Flipping `truncated` instead of cutting produces exactly this: the flag is right and the page is complete",
    ).toBe(1);

    // A second one, so the first is not a single lucky coordinate.
    expect(find(whole, "add_agent", "deploy-agent")?.run_count, "precondition: this coordinate must recur in 2 runs").toBe(2);
    expect(
      find(cut, "add_agent", "deploy-agent")?.run_count,
      "a second surviving group kept its full run_count -- see above; one coordinate could be luck, two cannot",
    ).toBe(1);

    // The control: a coordinate whose rows both survive the cut must be UNCHANGED. Without
    // this, "run_count went down" would also be satisfied by a cap that damaged everything.
    expect(find(whole, "install_worker_tool", "shellcheck")?.run_count, "precondition: this coordinate must recur in 2 runs").toBe(2);
    expect(
      find(cut, "install_worker_tool", "shellcheck")?.run_count,
      "a coordinate whose rows BOTH survive the cut lost count anyway -- the cap is damaging groups it did not cut, " +
        "and 'run_count went down' would then be satisfied by a cap that mangles everything",
    ).toBe(2);
  });

  it("drops whole groups too, and a dropped coordinate is UNKNOWN rather than settled", async () => {
    setScenario("");
    const whole = (await mockApi.getJudgeBacklog("all")).groups;
    setScenario(SCENARIO);
    const cut = (await mockApi.getJudgeBacklog("all")).groups;

    expect(whole.length, "precondition: the uncapped demo backlog must hold 8 groups").toBe(8);
    expect(cut.length, "the truncated page returns a different number of groups than the cut accounts for").toBe(5);
    // enable_tool/ripgrep is the demo's auto-done (set_via=issue_close). It vanishes entirely
    // from the truncated page -- which is precisely the case the banner exists for: absent is
    // not the same claim as settled, even though this particular one happens to be settled.
    expect(find(whole, "enable_tool", "ripgrep"), "precondition: this coordinate must exist uncapped").toBeTruthy();
    expect(
      find(cut, "enable_tool", "ripgrep"),
      "a coordinate whose only row was cut still came back -- the page is not actually missing anything, " +
        "so the banner's 'a few groups may be missing' would be a claim about nothing",
    ).toBeUndefined();
    expect(find(cut, "adjust_template", "coder"), "a second wholly-cut coordinate still came back").toBeUndefined();
  });

  it("leaves triage at the canonical aggregate, which is what makes the state visible", async () => {
    setScenario("");
    const whole = await mockApi.getJudgeBacklog("all");
    setScenario(SCENARIO);
    const cut = await mockApi.getJudgeBacklog("all");

    // triage comes from the separate stats query on the server and has no LIMIT, so it does
    // NOT move when the page is truncated. That is not an inconsistency to reconcile: it IS
    // the truncated state. The nav badge keeps reporting every open coordinate while the page
    // shows fewer, and a reader who trusted the page would be wrong.
    expect(
      cut.triage,
      "triage moved with the cut -- it must stay the canonical aggregate, because the badge disagreeing with " +
        "the page IS the truncated state and a reader who trusted the page would be wrong",
    ).toEqual(whole.triage);
    expect(cut.truncated, "the cap removed rows without reporting truncated").toBe(true);
    expect(
      rowCount(cut.groups),
      "the truncated page carries a different number of occurrences than the cap allows",
    ).toBe(MOCK_BACKLOG_MAX_ROWS);
  });

  it("reports truncated=false for a backlog of exactly the cap, and true one row over", () => {
    // The server buys this distinction by reading `max + 1` rows so a full page is
    // distinguishable from an exactly-full one without a second COUNT. Spelled out here
    // because an off-by-one would show the banner over a page that is complete.
    const row = (i: number) => ({ rec_id: String(i) }) as unknown as JudgeBacklogRow;
    const three = [row(1), row(2), row(3)];
    expect(
      capBacklogRows(three, 3),
      "a backlog of EXACTLY the cap was reported truncated -- an off-by-one here shows the banner over a page " +
        "that is complete, which is the same lie as flipping the flag",
    ).toEqual({ rows: three, truncated: false });
    const cut = capBacklogRows(three, 2);
    expect(cut.truncated, "a backlog one row OVER the cap was not reported truncated").toBe(true);
    expect(cut.rows, "the cut kept the wrong rows: it must keep the FIRST max, in query order").toEqual([row(1), row(2)]);
    expect(capBacklogRows([], 0), "an empty backlog at cap 0 must not be truncated").toEqual({ rows: [], truncated: false });
  });

  // PRD #235 M2: the ?category= filter cuts ROWS before the cap, mirroring the SQL predicate
  // that runs before the LIMIT. This rides the truncation leg (not the golden fidelity
  // fixture) because it is a pre-cap row operation — exactly like ?run= — and the fidelity
  // fixture deliberately excludes those.
  it("filters category ROWS before the cap: a category whose only rows sit past the cap still returns", async () => {
    setScenario(SCENARIO);
    // Unfiltered, the cap cuts enable_tool/ripgrep off-page entirely — its only rows sit past
    // the 6-row cut. The truncation suite above pins this same disappearance.
    const unfiltered = (await mockApi.getJudgeBacklog("all")).groups;
    expect(
      find(unfiltered, "enable_tool", "ripgrep"),
      "precondition: enable_tool/ripgrep must be truncated off the unfiltered page, or this test proves nothing",
    ).toBeUndefined();

    // Filtered to that category, it comes back. The ONLY way it can is if the category
    // predicate ran BEFORE the cap. Filtering the grouped output AFTER the cap would return
    // nothing here — the exact off-page bug the server's predicate-before-LIMIT avoids, now
    // reproduced in the mock if the order is wrong.
    const filtered = await mockApi.getJudgeBacklog("all", undefined, ["enable_tool"]);
    expect(
      find(filtered.groups, "enable_tool", "ripgrep"),
      "the category filter ran AFTER the cap: enable_tool/ripgrep's rows were cut before the filter saw them, " +
        "which is the off-page bug the server's predicate-before-LIMIT exists to avoid",
    ).toBeTruthy();
    // And the filtered set fits under the cap, so nothing is truncated — a second signal that
    // the cap runs on the already-narrowed rows, not the whole backlog.
    expect(
      filtered.truncated,
      "the selected category's rows fit under the cap, so the filtered page must not report truncated",
    ).toBe(false);
  });

  // Mutating: keep last. bulkSetJudgeDisposition writes to the shared demo state.
  it("carries truncated through the bulk-disposition re-read", async () => {
    setScenario(SCENARIO);
    const res = await mockApi.bulkSetJudgeDisposition(
      [{ category: "improve_uzi", target: "api/internal/poller" }],
      "dismissed",
      "wont_do",
      "open",
    );
    // The server sets this from the re-read (`Truncated: backlog.Truncated`), not from a
    // second computation. The page ORs it into its own state and appends "(backlog partial --
    // some may be off-page)" to the toast, so a false here would tell the user their bulk
    // action covered a complete view when it did not.
    expect(
      res.truncated,
      "the bulk-disposition response dropped the truncated flag -- the toast then tells the user their action " +
        "covered a complete view when it did not, and the page ORs this into its own state",
    ).toBe(true);
    expect(res.triage.total, "the re-read returned an empty triage aggregate").toBeGreaterThan(0);
  });
});
