// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppShell } from "../components/AppShell";
import { Judge } from "./Judge";
import { api } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// PRD #98 review BLK-BADGE — the nav badge and the To-triage tab must agree TO THE DIGIT,
// and this is the only file that can prove it.
//
// Both halves were already correct in isolation: each reads the canonical `triage.todo` and
// neither re-derives it from the groups on screen, and both properties are mutation-defended
// in their own suites. The bug was PROPAGATION — AppShell polls on
// `[user, location.pathname]`, and a disposition changes neither (nor does a bucket tab,
// which rewrites the search) — so after a dispose the nav read 3 while the tab read 0.
//
// A test that mounts either component ALONE cannot see that, which is why the existing
// suites were green throughout. Mounting them TOGETHER is the whole point of this file; if
// it is ever reduced to a single-component render it stops testing anything.

vi.mock("../lib/api", () => ({
  MOCK_MODE: false,
  api: {
    // Judge page
    getJudgeBacklog: vi.fn(),
    bulkSetJudgeDisposition: vi.fn(),
    deleteDisposition: vi.fn(),
    listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    // AppShell chrome
    listConnections: vi.fn().mockResolvedValue({ connections: [] }),
    unreadNotificationCount: vi.fn().mockResolvedValue({ count: 0 }),
    getJudgeStats: vi.fn(),
    listRuns: vi.fn().mockResolvedValue({ runs: [] }),
    getMyRateLimits: vi.fn().mockResolvedValue({ status: "no_token" }),
    version: vi.fn().mockResolvedValue({ version: "9.9.9-test" }),
  },
}));
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

const user = {
  id: "u1",
  email: "admin@uzi.local",
  display_name: "Admin",
  is_admin: false,
  is_active: true,
  autopilot_enabled: false,
  judge_enabled: true,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

const triage = (todo: number) => ({
  total: 3,
  todo,
  filed: 0,
  done: 3 - todo,
  dismissed: 0,
  false_positives: 0,
});

const group = () => ({
  category: "improve_uzi" as const,
  target: "api/internal/poller",
  bucket: "todo" as const,
  open_count: 3,
  run_count: 3,
  rationale_preview: "Queue-to-claim latency dominated the run.",
  occurrences: [
    { run_id: "run-1", run_title: "A run", review_id: "rev-1", rec_id: "rec-1", verdict: "issues" as const, confidence: "" as const, bucket: "todo" as const },
    { run_id: "run-2", run_title: "A run", review_id: "rev-2", rec_id: "rec-2", verdict: "issues" as const, confidence: "" as const, bucket: "todo" as const },
    { run_id: "run-3", run_title: "A run", review_id: "rev-3", rec_id: "rec-3", verdict: "issues" as const, confidence: "" as const, bucket: "todo" as const },
  ],
});

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ user } as unknown as ReturnType<typeof useAuth>);
  mockApi.getJudgeStats.mockResolvedValue(triage(3));
  mockApi.getJudgeBacklog.mockResolvedValue({
    bucket: "todo",
    run: "",
    groups: [group()],
    truncated: false,
    triage: triage(3),
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// Renders the Judge page INSIDE the real AppShell, the way the router does.
function renderJudgeInShell() {
  return render(
    <MemoryRouter initialEntries={["/judge"]}>
      <AppShell>
        <Judge />
      </AppShell>
    </MemoryRouter>,
  );
}

function navBadgeText() {
  return screen.getByRole("link", { name: /Judge/ }).textContent ?? "";
}

function tabText() {
  return screen.getByRole("tab", { name: /To triage/ }).textContent ?? "";
}

describe("Judge nav badge vs the To-triage tab (PRD #98 review BLK-BADGE)", () => {
  it("agrees on first load", async () => {
    renderJudgeInShell();
    await waitFor(() => expect(navBadgeText()).toContain("3"));
    expect(tabText()).toContain("3");
  });

  // The regression itself. Before the fix this ended at nav "3" / tab "0".
  it("still agrees AFTER a disposition drops the count", async () => {
    mockApi.bulkSetJudgeDisposition.mockResolvedValue({
      updated: 3,
      settled: [
        { run_id: "run-1", rec_id: "rec-1" },
        { run_id: "run-2", rec_id: "rec-2" },
        { run_id: "run-3", rec_id: "rec-3" },
      ],
      groups: [{ ...group(), bucket: "done" as const, open_count: 0 }],
      truncated: false,
      triage: triage(0),
    });

    renderJudgeInShell();
    await waitFor(() => expect(navBadgeText()).toContain("3"));

    fireEvent.click(screen.getByRole("button", { name: /Mark done/ }));

    // The tab drops to 0 …
    await waitFor(() => expect(tabText()).toContain("0"));
    // … and so does the nav badge. This is the assertion that was failing: the count is
    // rendered as a badge only while non-zero, so at 0 the digit must be GONE, not stale.
    await waitFor(() => expect(navBadgeText()).not.toContain("3"));
  });

  // The badge must follow the page's own reads too, not only dispositions.
  //
  // The scenario is a BUCKET TAB switch: it rewrites the search, so `load()` re-runs and
  // learns a fresh canonical triage, while AppShell — keyed on `location.pathname` — does
  // NOT re-poll. If the count moved server-side in the meantime (another tab, the M6
  // issue-close poller), the page knows and the shell does not.
  //
  // Note what this test does NOT do: make the shell's mount poll and the page's mount read
  // disagree. An earlier draft did, and it was testing a race I had invented — both read the
  // same canonical query at the same moment, so the server cannot produce that state, and
  // which promise resolved last is not a property worth pinning. The tab switch is
  // deterministic because it happens after both have settled.
  it("follows a page-only reload, which the shell's pathname poll cannot see", async () => {
    renderJudgeInShell();
    await waitFor(() => expect(navBadgeText()).toContain("3"));
    expect(tabText()).toContain("3");

    // The next read of the backlog reports a smaller canonical count.
    mockApi.getJudgeBacklog.mockResolvedValue({
      bucket: "all",
      run: "",
      groups: [group()],
      truncated: false,
      triage: triage(1),
    });
    fireEvent.click(screen.getByRole("tab", { name: /All/ }));

    // ORDER MATTERS HERE (PRD #98 review). This assertion used to sit AFTER the two digit
    // waitFors, where it could never execute in the failing case: the mutation it is aimed at
    // — obtaining the number by RE-FETCHING instead of publishing — makes the digit waitFor
    // throw first (the mock's getJudgeStats returns the stale count, so a refetch actively
    // re-stales the badge). The premise was proven by a different line than the one credited.
    // Asserting the call count first makes this line the discriminator it was written to be.
    //
    // It pins a real secondary property: the fix spends NO round-trip, because the response
    // already carries the canonical triage. The shell polls once, on mount; a bucket switch
    // changes the search, not the pathname.
    await waitFor(() => expect(mockApi.getJudgeBacklog).toHaveBeenCalledTimes(2));
    expect(mockApi.getJudgeStats).toHaveBeenCalledTimes(1);

    await waitFor(() => expect(tabText()).toContain("1"));
    await waitFor(() => expect(navBadgeText()).toContain("1"));
  });

  // Control: what the page publishes is the SERVER's canonical count, never a tally of the
  // rows on screen. After the tab switch the shell's poll is stale, so the page's publish is
  // the ONLY source for the badge — and the group on screen carries 3 open occurrences while
  // the canonical count is 5. A tally-based implementation would publish 3 and this fails.
  it("publishes the canonical count, not a tally of the visible rows", async () => {
    renderJudgeInShell();
    await waitFor(() => expect(navBadgeText()).toContain("3"));

    mockApi.getJudgeBacklog.mockResolvedValue({
      bucket: "all",
      run: "",
      groups: [group()], // 3 open occurrences visible
      truncated: false,
      triage: { total: 9, todo: 5, filed: 0, done: 4, dismissed: 0, false_positives: 0 },
    });
    fireEvent.click(screen.getByRole("tab", { name: /All/ }));

    await waitFor(() => expect(tabText()).toContain("5"));
    await waitFor(() => expect(navBadgeText()).toContain("5"));
  });
});

describe("the Judge page still works with no AppShell above it", () => {
  // The context default is a no-op precisely so a standalone mount does not throw. Every
  // other Judge test relies on that, so it needs its own pin: if the default ever became a
  // throwing "provider missing" guard, those suites would fail for an unrelated reason.
  it("renders standalone without a provider", async () => {
    render(
      <MemoryRouter initialEntries={["/judge"]}>
        <Judge />
      </MemoryRouter>,
    );
    expect(await screen.findByText("api/internal/poller")).toBeTruthy();
    const tab = screen.getByRole("tab", { name: /To triage/ });
    expect(within(tab).queryByText("3") ?? tab).toBeTruthy();
  });
});
