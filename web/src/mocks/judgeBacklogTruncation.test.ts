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
    expect(b.truncated).toBe(false);
    expect(b.groups.length).toBe(7);
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
    expect(find(whole, "improve_uzi", "api/internal/poller")?.run_count).toBe(3);
    expect(find(cut, "improve_uzi", "api/internal/poller")?.run_count).toBe(1);

    // A second one, so the first is not a single lucky coordinate.
    expect(find(whole, "add_agent", "deploy-agent")?.run_count).toBe(2);
    expect(find(cut, "add_agent", "deploy-agent")?.run_count).toBe(1);

    // The control: a coordinate whose rows both survive the cut must be UNCHANGED. Without
    // this, "run_count went down" would also be satisfied by a cap that damaged everything.
    expect(find(whole, "install_worker_tool", "shellcheck")?.run_count).toBe(2);
    expect(find(cut, "install_worker_tool", "shellcheck")?.run_count).toBe(2);
  });

  it("drops whole groups too, and a dropped coordinate is UNKNOWN rather than settled", async () => {
    setScenario("");
    const whole = (await mockApi.getJudgeBacklog("all")).groups;
    setScenario(SCENARIO);
    const cut = (await mockApi.getJudgeBacklog("all")).groups;

    expect(whole.length).toBe(7);
    expect(cut.length).toBe(5);
    // enable_tool/ripgrep is the demo's auto-done (set_via=issue_close). It vanishes entirely
    // from the truncated page -- which is precisely the case the banner exists for: absent is
    // not the same claim as settled, even though this particular one happens to be settled.
    expect(find(whole, "enable_tool", "ripgrep")).toBeTruthy();
    expect(find(cut, "enable_tool", "ripgrep")).toBeUndefined();
    expect(find(cut, "adjust_template", "coder")).toBeUndefined();
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
    expect(cut.triage).toEqual(whole.triage);
    expect(cut.truncated).toBe(true);
    expect(rowCount(cut.groups)).toBe(MOCK_BACKLOG_MAX_ROWS);
  });

  it("reports truncated=false for a backlog of exactly the cap, and true one row over", () => {
    // The server buys this distinction by reading `max + 1` rows so a full page is
    // distinguishable from an exactly-full one without a second COUNT. Spelled out here
    // because an off-by-one would show the banner over a page that is complete.
    const row = (i: number) => ({ rec_id: String(i) }) as unknown as JudgeBacklogRow;
    const three = [row(1), row(2), row(3)];
    expect(capBacklogRows(three, 3)).toEqual({ rows: three, truncated: false });
    const cut = capBacklogRows(three, 2);
    expect(cut.truncated).toBe(true);
    expect(cut.rows).toEqual([row(1), row(2)]);
    expect(capBacklogRows([], 0)).toEqual({ rows: [], truncated: false });
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
    expect(res.truncated).toBe(true);
    expect(res.triage.total).toBeGreaterThan(0);
  });
});
