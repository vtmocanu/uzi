// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Judge } from "./Judge";
import {
  api,
  ApiError,
  type JudgeBacklog,
  type JudgeRecommendationGroup,
  type JudgeDispositionResult,
} from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getJudgeBacklog: vi.fn(),
      bulkSetJudgeDisposition: vi.fn(),
      deleteDisposition: vi.fn(),
      listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

function occ(over: Partial<JudgeRecommendationGroup["occurrences"][number]> = {}) {
  return {
    run_id: "run-1",
    run_title: "A run",
    review_id: "rev-1",
    rec_id: "rec-1",
    verdict: "issues" as const,
    confidence: "" as const,
    bucket: "todo" as const,
    ...over,
  };
}

function group(over: Partial<JudgeRecommendationGroup> = {}): JudgeRecommendationGroup {
  return {
    category: "improve_uzi",
    target: "api/internal/poller",
    bucket: "todo",
    open_count: 3,
    run_count: 3,
    rationale_preview: "Queue-to-claim latency dominated the run.",
    occurrences: [
      occ({ run_id: "run-1", rec_id: "rec-1" }),
      occ({ run_id: "run-2", rec_id: "rec-2" }),
      occ({ run_id: "run-3", rec_id: "rec-3" }),
    ],
    ...over,
  };
}

// triage.todo (5) is DELIBERATELY larger than the number of open group rows (they dedup):
// the tab must show the canonical 5, not the 2 rows on screen.
function backlog(over: Partial<JudgeBacklog> = {}): JudgeBacklog {
  return {
    bucket: "todo",
    run: "",
    groups: [group(), group({ target: "shellcheck", category: "install_worker_tool", open_count: 1, run_count: 2 })],
    truncated: false,
    triage: { total: 11, todo: 5, filed: 2, done: 2, dismissed: 2, false_positives: 1 },
    ...over,
  };
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ user: { judge_enabled: true } } as unknown as ReturnType<typeof useAuth>);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderJudge(entries: string[] = ["/judge"]) {
  return render(
    <MemoryRouter initialEntries={entries}>
      <Judge />
    </MemoryRouter>,
  );
}

describe("Judge — bucket tabs read the canonical triage, not the groups on screen", () => {
  it("shows triage.todo on the To-triage tab even though fewer group rows are visible", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(backlog());
    renderJudge();

    await waitFor(() => expect(screen.getByText("shellcheck")).toBeTruthy());
    // Two deduped group rows…
    expect(screen.getAllByRole("checkbox")).toHaveLength(2);
    // …but the To-triage tab shows the canonical per-recommendation count (5), NOT 2.
    const tab = screen.getByRole("tab", { name: /To triage/ });
    expect(tab.textContent).toContain("5");
    // The default fetch is the todo bucket.
    expect(mockApi.getJudgeBacklog).toHaveBeenCalledWith("todo", undefined);
  });
});

describe("Judge — rationale_preview is escaped text, never HTML (PRD #98 auditor #1)", () => {
  it("renders markup in the preview as literal characters", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(
      backlog({
        groups: [group({ rationale_preview: "<img src=x onerror=alert(1)> **not bold**" })],
        triage: { total: 1, todo: 1, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
      }),
    );
    const { container } = renderJudge();

    expect(await screen.findByText(/<img src=x onerror=alert\(1\)> \*\*not bold\*\*/)).toBeTruthy();
    // The markup never became a real element (no dangerouslySetInnerHTML / markdown).
    expect(container.querySelector("img")).toBeNull();
  });
});

// Issue #124: escaping (pinned directly above) leaves Unicode format characters alone, and
// the browser's bidi algorithm honours them — so a recommendation target can visually name
// a file it does not point at. The backlog is the surface where those targets are compared
// side by side, which is exactly where a lying one does the most damage.
describe("Judge — bidi/zero-width characters are stripped from backlog free text (#124)", () => {
  it("renders no format character anywhere in the backlog, expander included", async () => {
    const RLO = "\u202E";
    const ZWSP = "\u200B";
    mockApi.getJudgeBacklog.mockResolvedValue(
      backlog({
        groups: [
          group({
            target: `api/internal/${ZWSP}poller`,
            rationale_preview: `The judge ${RLO}approved this`,
            occurrences: [occ({ run_id: "run-1", rec_id: "rec-1", run_title: `A ${RLO}run` })],
            open_count: 1,
            run_count: 1,
          }),
        ],
        triage: { total: 1, todo: 1, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
      }),
    );
    const { container } = renderJudge();
    await screen.findByText("The judge approved this");

    // Expand the occurrences so run_title is on screen too — an un-expanded row would
    // let a missed strip pass unnoticed.
    fireEvent.click(screen.getByLabelText("Expand occurrences"));
    await screen.findByText("A run");

    const rendered = container.textContent ?? "";
    expect(rendered).not.toMatch(/[\p{Cf}]/u);
    // The target is the searchable string, and the checkbox label quotes it too, so the
    // accessible name cannot disagree with what is on screen.
    expect(screen.getByText("api/internal/poller")).toBeTruthy();
    expect(screen.getByLabelText(/Select .* api\/internal\/poller/)).toBeTruthy();
  });
});

describe("Judge — a dismiss RE-RENDERS the row from the response, never a client-side filter", () => {
  it("keeps the acted-on row (now Dismissed) and reports the response's Updated count", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(
      backlog({
        groups: [group()], // one group, open_count 3, visibly spanning 3 recommendations
        triage: { total: 3, todo: 3, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
      }),
    );
    // The response RE-READS at bucket=all: the just-dismissed group comes back with its new
    // rollup (dismissed, open 0). Updated is 2 — a (review, category, target) TRIPLE count,
    // deliberately LOWER than the 3 recommendations the group visibly spanned.
    const disposed: JudgeDispositionResult = {
      updated: 2,
      settled: [
        { run_id: "run-1", rec_id: "rec-1" },
        { run_id: "run-3", rec_id: "rec-3" },
      ],
      groups: [
        group({
          bucket: "dismissed",
          open_count: 0,
          occurrences: [
            occ({ run_id: "run-1", rec_id: "rec-1", bucket: "dismissed" }),
            occ({ run_id: "run-2", rec_id: "rec-2", bucket: "dismissed" }),
            occ({ run_id: "run-3", rec_id: "rec-3", bucket: "dismissed" }),
          ],
        }),
      ],
      truncated: false,
      triage: { total: 3, todo: 0, filed: 0, done: 0, dismissed: 3, false_positives: 0 },
    };
    mockApi.bulkSetJudgeDisposition.mockResolvedValue(disposed);

    renderJudge();
    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: /Dismiss/ }));
    fireEvent.click(screen.getByRole("menuitem", { name: /Won't do/ }));

    // The toast reports the RESPONSE's Updated (2), not the group's visible span (3), and
    // calls them COORDINATES: `updated` counts (review_id, category, target) triples, which
    // is not a recommendation count — two recs sharing a coordinate share one disposition.
    await waitFor(() => expect(screen.getByText(/2 coordinates dismissed/)).toBeTruthy());
    // The row did NOT vanish — it re-rendered in place at its new rollup.
    const row = screen.getByText("api/internal/poller").closest("li")!;
    expect(within(row).getByText("Dismissed")).toBeTruthy();

    // scope defaults to open; the coordinate is the group's (category, target).
    expect(mockApi.bulkSetJudgeDisposition).toHaveBeenCalledWith(
      [{ category: "improve_uzi", target: "api/internal/poller" }],
      "dismissed",
      "wont_do",
      "open",
    );
  });

  // The load-bearing undo test (PRD #98 review BLK-UNDO). Its previous form scripted a
  // response whose settled set MATCHED the page's own view of which occurrences were open,
  // so it could not tell a correct implementation from the destructive one — the fixture was
  // structurally incapable of observing the divergence, which is the whole failure.
  //
  // Here they DIVERGE, as they do in production: the page shows three `todo` occurrences,
  // but between its last load and the write, run-2's coordinate was settled by the M6
  // issue-close poller. scope=open is evaluated SERVER-SIDE at write time, so the server
  // settled only run-1 and run-3 and says so in `settled`. Undo must clear exactly those.
  //
  // Deleting run-2's disposition would destroy an auto-done IRREVERSIBLY: close_synced_at is
  // stamped, the poller is edge-triggered, so it never re-fires and the set_via='issue_close'
  // provenance is gone. That is why the negative assertion matters more than the count.
  it("Undo clears the members the RESPONSE settled, not the ones the page thought were open", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(
      backlog({
        groups: [group()], // three occurrences, all rendered `todo` by this (stale) read
        triage: { total: 3, todo: 3, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
      }),
    );
    mockApi.bulkSetJudgeDisposition.mockResolvedValue({
      updated: 2,
      // run-2 is ABSENT: settled elsewhere before this write, so scope=open skipped it.
      settled: [
        { run_id: "run-1", rec_id: "rec-1" },
        { run_id: "run-3", rec_id: "rec-3" },
      ],
      groups: [group({ bucket: "dismissed", open_count: 0 })],
      truncated: false,
      triage: { total: 3, todo: 0, filed: 0, done: 0, dismissed: 3, false_positives: 0 },
    });
    mockApi.deleteDisposition.mockResolvedValue(null);

    renderJudge();
    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: /Mark done/ }));
    const toast = await screen.findByRole("status");
    fireEvent.click(within(toast).getByText("Undo"));

    await waitFor(() => expect(mockApi.deleteDisposition).toHaveBeenCalledTimes(2));
    expect(mockApi.deleteDisposition).toHaveBeenCalledWith("run-1", "rec-1");
    expect(mockApi.deleteDisposition).toHaveBeenCalledWith("run-3", "rec-3");
    // The one that matters: the member this action never settled is never cleared.
    expect(mockApi.deleteDisposition).not.toHaveBeenCalledWith("run-2", "rec-2");
  });

  // A fan-out that settled nothing offers no Undo at all — there is nothing to revert, and
  // an Undo button that deletes someone else's settled dispositions is the bug above.
  it("offers no Undo when the response settled nothing", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(
      backlog({
        groups: [group()],
        triage: { total: 3, todo: 3, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
      }),
    );
    mockApi.bulkSetJudgeDisposition.mockResolvedValue({
      updated: 0,
      settled: [],
      groups: [group()],
      truncated: false,
      triage: { total: 3, todo: 3, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
    });

    renderJudge();
    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /Mark done/ }));

    const toast = await screen.findByRole("status");
    expect(within(toast).queryByText("Undo")).toBeNull();
  });

  // N4's OTHER half: the concurrency bound. Both validators found it unasserted —
  // `UNDO_CONCURRENCY = MAX_SAFE_INTEGER` left all 845 tests green — so the amplification
  // argument (undo re-expanding, one request per member, the fan-out M2 deliberately
  // collapsed into a single statement) was stated and not defended.
  //
  // Measuring peak in-flight is the only assertion that can see it: wrap the mock to count
  // on entry and decrement on resolve, and never let the peak exceed the bound. 24 members
  // against a bound of 6, so an unbounded implementation peaks at 24 and fails loudly.
  it("never exceeds the undo concurrency bound", async () => {
    const settled = Array.from({ length: 24 }, (_, i) => ({ run_id: `run-${i}`, rec_id: `rec-${i}` }));
    mockApi.getJudgeBacklog.mockResolvedValue(
      backlog({
        groups: [group()],
        triage: { total: 24, todo: 24, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
      }),
    );
    mockApi.bulkSetJudgeDisposition.mockResolvedValue({
      updated: 24,
      settled,
      groups: [group({ bucket: "done", open_count: 0 })],
      truncated: false,
      triage: { total: 24, todo: 0, filed: 0, done: 24, dismissed: 0, false_positives: 0 },
    });

    // DEFERRED PROMISES, NO TIMERS. Every call parks until this test releases it, so the
    // bound is observed structurally rather than by racing the clock: with all N calls
    // outstanding, the number the code has issued IS the concurrency limit.
    //
    // The earlier version resolved on a `setTimeout(…, 0)` and measured a peak counter. It
    // was correct and it survived four mutations, but it made a wall-clock-dependent
    // assertion out of a property that has nothing to do with elapsed time — and this suite
    // is where CI flakes get born. Nothing here can now be perturbed by a slow machine.
    const release: Array<() => void> = [];
    mockApi.deleteDisposition.mockImplementation(
      () => new Promise<null>((resolve) => release.push(() => resolve(null))),
    );

    renderJudge();
    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /Mark done/ }));
    const toast = await screen.findByRole("status");
    fireEvent.click(within(toast).getByText("Undo"));

    // Exactly UNDO_CONCURRENCY calls start, and no more, while all of them are outstanding.
    await waitFor(() => expect(mockApi.deleteDisposition).toHaveBeenCalledTimes(6));
    // Nothing may sneak a 7th through on a later microtask. Draining the queue repeatedly is
    // the deterministic equivalent of "wait and see": an unbounded implementation has already
    // issued all 24 by now, so this is where it fails.
    for (let i = 0; i < 20; i += 1) await Promise.resolve();
    expect(mockApi.deleteDisposition).toHaveBeenCalledTimes(6);

    // Control: the work really is concurrent, not serialised. A serial implementation would
    // have exactly ONE call outstanding here, so this discriminates a bound of 6 from a
    // bound of 1 — the failure mode that would make the assertion above pass vacuously.
    expect(release.length).toBe(6);

    // Drain: each release frees a worker to take the next member. Releasing a batch and then
    // waiting for the call count to GROW is what keeps this terminating — waiting on a
    // steady-state predicate would be satisfied instantly and spin.
    let seen = mockApi.deleteDisposition.mock.calls.length;
    while (seen < settled.length) {
      release.splice(0).forEach((r) => r());
      await waitFor(() => expect(mockApi.deleteDisposition.mock.calls.length).toBeGreaterThan(seen));
      seen = mockApi.deleteDisposition.mock.calls.length;
    }
    release.splice(0).forEach((r) => r()); // let the last batch settle
    expect(mockApi.deleteDisposition).toHaveBeenCalledTimes(settled.length);
  });

  // Partial failure is reported honestly (PRD #98 review N4). The previous Promise.all form
  // rejected on the FIRST failure with the rest still in flight, so the user was told
  // "Could not undo" while an unknown number of members HAD been reverted. Undo is
  // destructive: "some of it happened and I will not say which" is the one report it must
  // not give.
  it("reports a partial undo honestly instead of claiming total failure", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(
      backlog({
        groups: [group()],
        triage: { total: 3, todo: 3, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
      }),
    );
    mockApi.bulkSetJudgeDisposition.mockResolvedValue({
      updated: 3,
      settled: [
        { run_id: "run-1", rec_id: "rec-1" },
        { run_id: "run-2", rec_id: "rec-2" },
        { run_id: "run-3", rec_id: "rec-3" },
      ],
      groups: [group({ bucket: "done", open_count: 0 })],
      truncated: false,
      triage: { total: 3, todo: 0, filed: 0, done: 3, dismissed: 0, false_positives: 0 },
    });
    mockApi.deleteDisposition.mockImplementation((runId: string) =>
      runId === "run-2" ? Promise.reject(new ApiError(500, "boom")) : Promise.resolve(null),
    );

    renderJudge();
    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /Mark done/ }));
    const toast = await screen.findByRole("status");
    fireEvent.click(within(toast).getByText("Undo"));

    // Every member is attempted despite the failure in the middle — not abandoned.
    await waitFor(() => expect(mockApi.deleteDisposition).toHaveBeenCalledTimes(3));
    expect(await screen.findByText(/Partly undone: 2 of 3 reverted, 1 failed/)).toBeTruthy();
  });
});

describe("Judge — truncation is surfaced, never rendered as authoritative (PRD #98 auditor #2)", () => {
  it("shows the truncated banner when the backlog hit the cap", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(backlog({ truncated: true }));
    renderJudge();
    expect(await screen.findByText(/backlog is large and was truncated/i)).toBeTruthy();
  });

  // PRD #98 review N1. The reconcile's load-bearing half is the ABSENT case, and it was
  // undefended: mutating the reconcile to .filter() out coordinates missing from the
  // response left every Judge test green, because no fixture had a row the response omits.
  //
  // Past the row cap a settled coordinate can fall OUTSIDE the re-read window, so the
  // response carries no group for it. That is UNKNOWN, not "settled and gone" — dropping the
  // row makes it silently disappear mid-interaction, which is the exact confusion the
  // bucket=all re-read and the `truncated` flag exist to prevent. The row must stay,
  // rendered at its LAST KNOWN state.
  it("keeps a row the disposition response does not return — absent is UNKNOWN, not settled", async () => {
    const kept = group({ category: "install_worker_tool", target: "rg", open_count: 1, run_count: 1 });
    mockApi.getJudgeBacklog.mockResolvedValue(
      backlog({
        groups: [group(), kept],
        triage: { total: 4, todo: 4, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
      }),
    );
    // Truncated re-read: only the acted-on group comes back. `kept` is absent — NOT because
    // it was settled, but because it lies outside the window.
    mockApi.bulkSetJudgeDisposition.mockResolvedValue({
      updated: 2,
      settled: [
        { run_id: "run-1", rec_id: "rec-1" },
        { run_id: "run-3", rec_id: "rec-3" },
      ],
      groups: [group({ bucket: "dismissed", open_count: 0 })],
      truncated: true,
      triage: { total: 4, todo: 1, filed: 0, done: 0, dismissed: 3, false_positives: 0 },
    });

    renderJudge();
    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());
    expect(screen.getByText("rg")).toBeTruthy();

    fireEvent.click(screen.getAllByRole("button", { name: /Dismiss/ })[0]);
    fireEvent.click(screen.getAllByRole("menuitem", { name: /Won't do/ })[0]);

    // The acted-on row re-rendered at its new rollup...
    await waitFor(() => {
      const row = screen.getByText("api/internal/poller").closest("li")!;
      expect(within(row).getByText("Dismissed")).toBeTruthy();
    });
    // ...and the ABSENT row is still on screen, at its LAST KNOWN state. A .filter() on the
    // response's coordinates would have removed it here — this getByText is the assertion.
    const survivor = screen.getByText("rg").closest("li")!;
    // Still open, and NOT swept to the acted-on group's new rollup. (A todo group renders no
    // rollup badge — todo is the default state — so "still open" is the observable.)
    expect(within(survivor).getByText("1 open")).toBeTruthy();
    expect(within(survivor).queryByText("Dismissed")).toBeNull();
  });
});

// PRD #98 review N10. RunsList.test.tsx pins that the aggregate strip is GONE from /runs;
// nothing pinned that it ARRIVED here, so "the count is never homeless" was only half
// enforced — a removal test and an arrival test are different claims, and deleting this
// header would have left the first one green.
describe("Judge — the aggregate triage strip has a home here (PRD #98 Decision 7 / N10)", () => {
  it("renders the all-runs triage summary in the page header", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(
      backlog({
        groups: [group()],
        triage: { total: 12, todo: 5, filed: 2, done: 3, dismissed: 2, false_positives: 1 },
      }),
    );
    renderJudge();

    // The strip's own title, moved from /runs to here.
    expect(await screen.findByText(/Recommendations · all your runs/)).toBeTruthy();
    // And it renders the CANONICAL aggregate, not a tally of the rows on screen.
    expect(screen.getByText("all time")).toBeTruthy();
  });
});

describe("Judge — one grammar for the verdict fact (PRD #98 review N8)", () => {
  it("renders an occurrence verdict as `⚖ issues`, the same grammar /runs uses", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(
      backlog({ groups: [group()], triage: { total: 3, todo: 3, filed: 0, done: 0, dismissed: 0, false_positives: 0 } }),
    );
    renderJudge();
    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /Expand occurrences/ }));

    // The shared judgeBadge() grammar — the scale glyph plus the raw verdict enum.
    const badges = screen.getAllByText(/⚖ issues/);
    expect(badges.length).toBeGreaterThan(0);
    // NOT the second grammar this page had reintroduced. `verdictLabel` renders
    // "Issues found"; two grammars for one fact is the regression.
    expect(screen.queryByText("Issues found")).toBeNull();

    // One glyph, two inference rules (PRD #98 review N-b): on /runs a bare `⚖ issues` means
    // the backlog is CLEARED, because the count is always rendered when > 0. Here it means
    // no count is carried, on a row that is still open. The label is deliberately identical —
    // that shared grammar is the point of N8 — so the disambiguation must be in the TITLE,
    // and it must not claim anything about triage state.
    const title = badges[0].getAttribute("title") ?? "";
    expect(title).toContain("verdict");
    expect(title).not.toMatch(/to triage|still to triage|nothing left/i);
  });
});

describe("Judge — clearing the run filter keeps the bucket (PRD #98 review N5)", () => {
  it("does not silently snap an anchored `all` view back to To triage", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(
      backlog({
        bucket: "all",
        groups: [group({ bucket: "done", open_count: 0 })],
        triage: { total: 3, todo: 0, filed: 0, done: 3, dismissed: 0, false_positives: 0 },
      }),
    );
    // An anchored deep-link with no explicit bucket: the default derives to `all`
    // (Decision 1's deliberate exception for notification links).
    render(
      <MemoryRouter initialEntries={["/judge?run=run-1"]}>
        <Judge />
      </MemoryRouter>,
    );
    await waitFor(() => expect(mockApi.getJudgeBacklog).toHaveBeenCalledWith("all", "run-1"));

    fireEvent.click(screen.getByRole("button", { name: /Clear filter/ }));

    // Still `all`. Dropping the anchor used to re-derive the default to `todo`, so the rows
    // the user was looking at vanished — they asked to stop filtering by run, not to change
    // which rung they were on.
    await waitFor(() => expect(mockApi.getJudgeBacklog).toHaveBeenLastCalledWith("all", undefined));
  });
});

describe("Judge — an auto-done is visibly distinct from a hand-marked done (PRD #98 D6/B3)", () => {
  // The whole point of set_via is that "I decided this was done" and "the system inferred it
  // from a closed issue" are DIFFERENT claims. So the load-bearing assertion is that the two
  // chips differ — a test that only checked the auto-done renders would pass unchanged if
  // both rendered "✓ Done", which is exactly the state this ships to fix (the column existed
  // and never left the store, so every client rendered them identically).
  //
  // Both occurrences are bucket "done" on purpose: if the buckets differed, the test would
  // be proving something about the ladder rather than about provenance.
  async function expandedChips(occurrences: JudgeRecommendationGroup["occurrences"]) {
    mockApi.getJudgeBacklog.mockResolvedValue(
      backlog({
        groups: [group({ bucket: "done", open_count: 0, occurrences })],
        triage: { total: 2, todo: 0, filed: 0, done: 2, dismissed: 0, false_positives: 0 },
      }),
    );
    renderJudge();
    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /Expand occurrences/ }));

    // The same fixture-broken guard the Go grouper test carries. Every occurrence here must
    // bucket `done`: if a future edit to the `occ` default drifted one off `done`, these
    // tests would quietly become ladder tests — passing because the BUCKETS differ rather
    // than because the PROVENANCE does, which is the property they exist for.
    for (const o of occurrences) {
      if (o.bucket !== "done") {
        throw new Error(
          `fixture broken: every occurrence must bucket "done" (got ${o.bucket}) — otherwise this ` +
            "proves something about the ladder, not about set_via",
        );
      }
    }
    return screen.getAllByRole("listitem");
  }

  it("labels an issue-close auto-done 'Done via #IID' and a hand-marked one just 'Done'", async () => {
    await expandedChips([
      occ({
        run_id: "run-auto",
        rec_id: "rec-auto",
        bucket: "done",
        set_via: "issue_close",
        filed_issue: { issue_iid: 91, issue_url: "https://forge.example/issues/91", filed_at: "2026-07-20T10:00:00Z" },
      }),
      occ({ run_id: "run-hand", rec_id: "rec-hand", bucket: "done" }),
    ]);

    // The auto-done names the issue whose closure produced it...
    expect(screen.getByText(/Done via #91/)).toBeTruthy();
    // ...and the hand-marked one does NOT claim any issue provenance. queryByText with an
    // exact matcher, so "Done via #91" cannot satisfy it.
    const plainDone = screen.getAllByText((_, el) => el?.textContent?.trim() === "✓ Done");
    expect(plainDone.length).toBeGreaterThan(0);
    expect(screen.queryByText(/Done via #undefined/)).toBeNull();
  });

  it("renders the two DIFFERENTLY — the same chip for both is the bug this exists for", async () => {
    await expandedChips([
      occ({
        run_id: "run-auto",
        rec_id: "rec-auto",
        run_title: "auto run",
        bucket: "done",
        set_via: "issue_close",
        filed_issue: { issue_iid: 91, issue_url: "https://forge.example/issues/91", filed_at: "2026-07-20T10:00:00Z" },
      }),
      occ({ run_id: "run-hand", rec_id: "rec-hand", run_title: "hand run", bucket: "done" }),
    ]);

    // Scoped to the two OCCURRENCE rows by their run titles. An earlier version took
    // getAllByRole("listitem") and picked by textContent, which silently bound `auto` to the
    // enclosing GROUP row — first in document order and containing "Done via" through its
    // own descendants — so the comparison was parent-vs-child and `not.toEqual` could never
    // fail. Proven: under the label-every-done-as-auto mutation both chips render
    // identically and that version stayed GREEN. Scoping is what makes this assertion real.
    const auto = screen.getByText("auto run").closest("li")!;
    const hand = screen.getByText("hand run").closest("li")!;
    expect(auto).not.toBe(hand);
    expect(auto.contains(hand)).toBe(false); // neither may enclose the other
    expect(auto.textContent).not.toEqual(hand.textContent);
    // And specifically on the chip, so a difference in some unrelated cell cannot satisfy it.
    expect(auto.textContent).toContain("Done via #91");
    expect(hand.textContent).not.toContain("Done via");
  });

  // An auto-done whose filed link is gone still STATES its provenance rather than printing
  // "Done via #undefined". The sync always fires from a filed issue, so this is defensive —
  // but "defensive" is why it needs a test: nothing else would ever exercise it.
  it("states the provenance even without an issue iid, never '#undefined'", async () => {
    await expandedChips([occ({ bucket: "done", set_via: "issue_close" })]);
    expect(screen.getByText(/Done via issue close/)).toBeTruthy();
    expect(screen.queryByText(/#undefined/)).toBeNull();
  });
});

describe("Judge — inbox-zero is a first-class view (PRD #98 Decision 8)", () => {
  it("renders the zero-state (and the opt-in card when the judge is off) when triage.todo is 0", async () => {
    vi.mocked(useAuth).mockReturnValue({ user: { judge_enabled: false } } as unknown as ReturnType<typeof useAuth>);
    // The todo view is empty and triage.todo is 0 → the zero state, which fetches an
    // all-bucket snapshot for the recent-verdict trend + recently-handled groups.
    mockApi.getJudgeBacklog.mockImplementation(async (bucket) => {
      if (bucket === "all") {
        return backlog({
          bucket: "all",
          groups: [group({ bucket: "done", open_count: 0 })],
          triage: { total: 2, todo: 0, filed: 1, done: 1, dismissed: 0, false_positives: 0 },
        });
      }
      return backlog({
        groups: [],
        triage: { total: 2, todo: 0, filed: 1, done: 1, dismissed: 0, false_positives: 0 },
      });
    });

    renderJudge();

    expect(await screen.findByText(/Inbox zero/i)).toBeTruthy();
    expect(screen.getByText(/judge is off for your account/i)).toBeTruthy();
    // Recent-verdict trend rendered from the all-bucket snapshot.
    // Heading renamed by PRD #98 review N6: the tally is ALL TIME (no recency window
    // exists — the occurrence DTO carries no disposition timestamp), so the label may not
    // say "Recent".
    await waitFor(() => expect(screen.getByText("Verdicts across your judged runs")).toBeTruthy());
    expect(screen.queryByText(/^Recent/)).toBeNull();
  });
});

describe("Judge — the ?run= deep-link anchor (PRD #98 Decision 4)", () => {
  it("passes the run anchor to the backlog read and shows a clearable filter", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(backlog({ bucket: "all", run: "run-1" }));
    renderJudge(["/judge?run=run-1"]);

    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());
    // An anchored deep-link defaults to the `all` bucket so the run's recs always show.
    expect(mockApi.getJudgeBacklog).toHaveBeenCalledWith("all", "run-1");
    expect(screen.getByText(/Filtered to one run's recommendations/i)).toBeTruthy();
  });
});
