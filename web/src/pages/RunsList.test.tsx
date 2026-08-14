// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunsList } from "./RunsList";
import { api, type RunListItem, type SecretMeta } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// Keep the real module (isTerminalRun etc.) and mock only the network + auth so the
// list renders offline. A non-admin viewer skips the admin fetches.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listRuns: vi.fn(),
      adminListRuns: vi.fn(),
      adminListWorkers: vi.fn(),
      // PRD #295: RunsList.load() now fetches the viewer's secrets to compute the
      // ">1 Anthropic token" gate. Default to no tokens so every pre-#295 test keeps
      // its no-badge expectation; the #295 cases override it per test. vi.clearAllMocks
      // in afterEach clears call history but not this implementation (same as
      // getJudgeStats above).
      listSecrets: vi.fn().mockResolvedValue({ secrets: [] }),
      // Defined but never expected to fire. PRD #98 Decision 7 removed the aggregate strip
      // from this page, and the strip's fetch went with it — see the removal test below,
      // which can only assert that against a mock that EXISTS.
      getJudgeStats: vi.fn().mockResolvedValue({
        total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0,
      }),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

// Issue #256 M3: useNow is the single clock the duration token reads. Pin it so the
// live buckets ("running 1h 30m") are deterministic — the real hook returns Date.now().
const FIXED_NOW = Date.parse("2026-07-05T13:30:00Z");
vi.mock("../lib/rateLimits", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/rateLimits")>();
  return { ...actual, useNow: () => FIXED_NOW };
});

const mockApi = vi.mocked(api);

function aRun(over: Partial<RunListItem> = {}): RunListItem {
  return {
    id: "run-1",
    repo_id: "repo-1",
    forge_type: "gitlab",
    mr_web_url: null,
    kind: "issue",
    issue_iid: 7,
    issue_title: "A run",
    issue_description: "",
    title: null,
    resume_of_run_id: null,
    status: "running",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w1",
    branch: null,
    model: null,
    override_subagent_model: false,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_select_reason: null,
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: "2026-07-05T12:00:00Z",
    updated_at: "2026-07-05T12:00:00Z",
    repo_path: "grp/proj",
    worker_name: "w1",
    // PRD #98 M4: unjudged by default, so existing assertions are unchanged.
    judge_verdict: null,
    judge_todo_count: 0,
    ...over,
  };
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ user: { is_admin: false } } as unknown as ReturnType<typeof useAuth>);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// Issue #124 / item 7. The run title IS the forge issue title, writable by anyone who can
// open an issue on the target repo. It was stripped on /judge and rendered raw on the pages
// people actually live on, which is worse than uniformly unprotected: it makes the one
// surface look authoritative while the others lie.
//
// The await anchors on the STATUS pill, not on the title — awaiting the cleaned title would
// make a mutation red at a findByText timeout instead of at the assertion, which is the
// blind-instrument trap this batch already hit twice.
// Issue #124, and the only CROSS-PRINCIPAL case in the batch: the admin fleet list renders
// worker names their OWNER chose, beside that owner's email. A bidi override there is one
// user spoofing a row in an admin's view, not spoofing their own.
describe("RunsList — the admin fleet list carries no format characters (#124)", () => {
  it("strips bidi/zero-width characters out of another user's worker name", async () => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: true },
      vaultUnlocked: true,
    } as unknown as ReturnType<typeof useAuth>);
    mockApi.listRuns.mockResolvedValue({ runs: [] });
    mockApi.adminListRuns.mockResolvedValue({ runs: [] });
    mockApi.adminListWorkers.mockResolvedValue({
      workers: [
        {
          id: "w1", name: "prod\u202Ebox\u200B", status: "online", owner_email: "someone@else.test",
          kind: "external", hosted_size: null, busy: false, active_runs: 0, max_concurrent_runs: null,
          template_declared: null, template_reported: null, version: null, last_heartbeat_at: null,
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
    } as never);

    const { container } = render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );
    // Anchored on the owner email, which the mutation cannot move.
    await waitFor(() => expect(screen.getByText("someone@else.test")).toBeTruthy());
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(screen.getByText("prodbox")).toBeTruthy();
  });
});

describe("RunsList — the run title carries no format characters (#124)", () => {
  it("strips bidi/zero-width characters out of the forge-supplied title", async () => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: false },
      vaultUnlocked: true,
    } as unknown as ReturnType<typeof useAuth>);
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "r", issue_title: "Fix the \u202Eparser\u200B bug", status: "running" })],
    });

    const { container } = render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("running")).toBeTruthy());
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(screen.getByText("Fix the parser bug")).toBeTruthy();
  });
});

describe("RunsList — waiting for vault unlock (PRD #32)", () => {
  it("renders own queued runs as waiting for vault unlock while locked", async () => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: false },
      vaultUnlocked: false,
    } as unknown as ReturnType<typeof useAuth>);
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "q", issue_title: "Queued run", status: "queued" })],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Queued run")).toBeTruthy());
    expect(screen.getByText(/waiting for vault unlock/)).toBeTruthy();
    // The bare "queued" pill must not also render for that run.
    expect(screen.queryByText("queued")).toBeNull();
  });

  // Amendment 2026-08-14 (2): the factory card hides the admin's own runs (they are
  // already the Active section above), so the waiting-for-unlock badge — which only
  // ever applied to the admin's OWN queued rows — cannot appear on this list at all,
  // and another owner's queued row stays a plain "queued" pill (their vault state is
  // unknown here, PRD #32).
  it("admin factory list hides the admin's own runs; other users' queued rows read plain", async () => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: true, email: "me@uzi.test" },
      vaultUnlocked: false,
    } as unknown as ReturnType<typeof useAuth>);
    mockApi.listRuns.mockResolvedValue({ runs: [] });
    mockApi.adminListWorkers.mockResolvedValue({ workers: [] });
    mockApi.adminListRuns.mockResolvedValue({
      runs: [
        aRun({ id: "mine", issue_title: "My queued", status: "queued", owner_email: "me@uzi.test" }),
        aRun({ id: "theirs", issue_title: "Their queued", status: "queued", owner_email: "other@uzi.test" }),
      ],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Their queued")).toBeTruthy());
    // The heading says what the list now is; the old all-users copy is retired.
    expect(screen.getByText("Active runs · other users")).toBeTruthy();
    expect(screen.queryByText("Active runs · all users")).toBeNull();
    // The admin's own row is gone from the card (it lives in Active above)…
    expect(screen.queryByText("My queued")).toBeNull();
    // …and no waiting badge can render here; the other owner's row reads plain.
    expect(screen.queryByText(/waiting for vault unlock/)).toBeNull();
    expect(screen.getByText("queued")).toBeTruthy();
  });

  it("renders a plain queued pill when the vault is unlocked", async () => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: false },
      vaultUnlocked: true,
    } as unknown as ReturnType<typeof useAuth>);
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "q", issue_title: "Queued run", status: "queued" })],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Queued run")).toBeTruthy());
    expect(screen.getByText("queued")).toBeTruthy();
    expect(screen.queryByText(/waiting for vault unlock/)).toBeNull();
  });
});

describe("RunsList — autopilot badge", () => {
  it("shows the autopilot badge only for an auto_approve run", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({ id: "auto", issue_title: "Autopilot run", auto_approve: true }),
        aRun({ id: "manual", issue_title: "Manual run", auto_approve: false }),
      ],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Autopilot run")).toBeTruthy());
    expect(screen.getByText("Manual run")).toBeTruthy();
    // Exactly one badge — the manual run must not carry it.
    expect(screen.getAllByText("autopilot")).toHaveLength(1);
  });
});

describe("RunsList — usage meta line (PRD #40)", () => {
  it("adds tokens + cost to a run with usage (running → 'so far'), nothing to a run without", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({
          id: "with",
          issue_title: "Has usage",
          status: "running",
          usage: { input_tokens: 114_400, cache_read_tokens: 1_170_000, cache_creation_tokens: 0, output_tokens: 48_200, cost_usd: 1.87 },
        }),
        aRun({ id: "without", issue_title: "No usage", status: "running" }),
      ],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Has usage")).toBeTruthy());
    // total = 114.4k + 1.17M + 0 + 48.2k = 1,332,600 → "1.33M tok so far" (running).
    expect(screen.getByText(/1\.33M tok so far/)).toBeTruthy();
    expect(screen.getByText(/\$1\.87/)).toBeTruthy();
    // The no-usage run contributes no "tok" figure — exactly one run shows usage.
    expect(screen.queryAllByText(/tok/).length).toBe(1);
  });
})

describe("RunsList — global judge-triage strip removed (PRD #98 Decision 7)", () => {
  it("no longer renders the aggregate strip (its home is now the Judge page header)", async () => {
    mockApi.listRuns.mockResolvedValue({ runs: [aRun({ issue_title: "A run" })] });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("A run")).toBeTruthy());
    expect(screen.queryByText("Judge recommendations · all your runs")).toBeNull();
    // The strip's fetch is gone entirely — RunsList must not CALL getJudgeStats.
    //
    // This used to assert `getJudgeStats` was `undefined`, which was a property of THIS
    // FILE'S mock factory rather than of RunsList: it passed whatever the component did
    // (PRD #98 review N9). The behaviour was in fact defended — re-adding the call blew up
    // six tests with a TypeError on an undefined mock — but by accident, and with a failure
    // that named the wrong cause. The mock is now defined, so this asserts the real thing
    // and a regression reads as "it was called" instead of "undefined is not a function".
    expect(mockApi.getJudgeStats).not.toHaveBeenCalled();
  });
})

describe("RunsList milestone badge (PRD #122)", () => {
  beforeEach(() => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: false },
      vaultUnlocked: true,
    } as unknown as ReturnType<typeof useAuth>);
  });

  it("adds a compact M{done}/{total} badge to a milestone-structured run row", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({
          id: "m",
          issue_title: "Milestone run",
          status: "running",
          milestones: [
            { id: "a", title: "A" },
            { id: "b", title: "B" },
            { id: "c", title: "C" },
          ],
          milestones_completed: ["a", "b"],
        }),
      ],
    });
    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByText("Milestone run")).toBeTruthy());
    expect(screen.getByText("M2/3")).toBeTruthy();
  });

  it("adds no milestone badge to a run with no milestones (the row had none before)", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "n", issue_title: "Plain run", status: "running", milestones: null })],
    });
    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByText("Plain run")).toBeTruthy());
    expect(screen.queryByText(/^M\d+\/\d+$/)).toBeNull();
  });
});

// Issue #256 M3: each row carries a live, per-state duration token derived client-side
// from the timestamps the run already has, driven by the pinned useNow clock above.
describe("RunsList — live duration token (issue #256 M3)", () => {
  beforeEach(() => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: false },
      vaultUnlocked: true,
    } as unknown as ReturnType<typeof useAuth>);
  });

  it("shows a running run's live age (started_at 90m before the fixed now)", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({
          id: "act",
          issue_title: "Active run",
          status: "running",
          // 90 minutes before FIXED_NOW (2026-07-05T13:30:00Z) → "running 1h 30m".
          started_at: "2026-07-05T12:00:00Z",
        }),
      ],
    });

    const { container } = render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Active run")).toBeTruthy());
    expect(screen.getByText(/running 1h 30m/)).toBeTruthy();
    // The updated_at timestamp is kept alongside the token, not replaced (owner decision).
    expect(container.textContent ?? "").toContain(new Date("2026-07-05T12:00:00Z").toLocaleString());
  });

  it("shows a terminal run's static ran-span (finished_at − started_at = 42m)", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({
          id: "term",
          issue_title: "Terminal run",
          status: "completed",
          started_at: "2026-07-05T12:00:00Z",
          finished_at: "2026-07-05T12:42:00Z",
        }),
      ],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    // Terminal runs render in the always-visible, date-grouped past section.
    await waitFor(() => expect(screen.getByText("Terminal run")).toBeTruthy());
    expect(screen.getByText(/ran 42m/)).toBeTruthy();
  });

  it("adds no token to a run whose anchor is missing (never a fabricated 0s)", async () => {
    mockApi.listRuns.mockResolvedValue({
      // A completed run that never started: no started_at, so the static span is "".
      runs: [aRun({ id: "none", issue_title: "Never started", status: "completed", started_at: null, finished_at: null })],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Never started")).toBeTruthy());
    expect(screen.queryByText(/\bran\b/)).toBeNull();
  });
});

// ux-tweaks item 3: the past section is searchable, date-grouped (lib/runGroups.ts)
// and render-sliced through the board's lanePaging (cap 10, page 50).
describe("RunsList — past section search, grouping and reveal (ux-tweaks)", () => {
  beforeEach(() => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: false },
      vaultUnlocked: true,
    } as unknown as ReturnType<typeof useAuth>);
  });

  // Timestamps are chosen relative to FIXED_NOW (2026-07-05T13:30:00Z) with margins
  // wide enough that the local-time bucketing lands the same in any test timezone.
  const pastRun = (id: string, title: string, finishedAt: string) =>
    aRun({ id, issue_title: title, status: "completed", started_at: finishedAt, finished_at: finishedAt });

  it("groups past runs under date headers (Today vs an older month)", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        pastRun("t", "Fresh run", "2026-07-05T12:42:00Z"),
        pastRun("old", "Ancient run", "2026-05-20T10:00:00Z"),
      ],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Fresh run")).toBeTruthy());
    expect(screen.getByText("Today")).toBeTruthy();
    // The month label is locale-derived, so derive the expectation the same way.
    const monthLabel = new Date("2026-05-20T10:00:00Z").toLocaleDateString(undefined, {
      month: "long",
      year: "numeric",
    });
    expect(screen.getByText(monthLabel)).toBeTruthy();
  });

  it("filters past runs by the search box and reports the match count", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        pastRun("a", "Fix the parser", "2026-07-05T12:00:00Z"),
        pastRun("b", "Ship the exporter", "2026-07-05T11:00:00Z"),
      ],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Fix the parser")).toBeTruthy());
    fireEvent.change(screen.getByLabelText("Search past runs"), { target: { value: "parser" } });
    expect(screen.getByText("Fix the parser")).toBeTruthy();
    expect(screen.queryByText("Ship the exporter")).toBeNull();
    expect(screen.getByText(/1 result/)).toBeTruthy();

    // No match: an explanatory empty state with a working clear affordance.
    fireEvent.change(screen.getByLabelText("Search past runs"), { target: { value: "zzz" } });
    expect(screen.getByText(/No past runs match/)).toBeTruthy();
    fireEvent.click(screen.getByText("Clear search"));
    expect(screen.getByText("Ship the exporter")).toBeTruthy();
  });

  it("caps the initial render at 10 and reveals the remainder via Show more / Collapse", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: Array.from({ length: 12 }, (_, i) =>
        // Distinct minutes keep the sort deterministic; all land in one Today bucket.
        pastRun(`p${i}`, `Past run ${i}`, `2026-07-05T12:${String(59 - i).padStart(2, "0")}:00Z`),
      ),
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Past run 0")).toBeTruthy());
    // 10 of 12 render; the two newest-minus-nine are cut, the count label says so.
    expect(screen.getByText("Past run 9")).toBeTruthy();
    expect(screen.queryByText("Past run 10")).toBeNull();
    expect(screen.getByText("10/12")).toBeTruthy();

    fireEvent.click(screen.getByText(/Show 2 more/));
    expect(screen.getByText("Past run 11")).toBeTruthy();

    fireEvent.click(screen.getByText("Collapse"));
    expect(screen.queryByText("Past run 10")).toBeNull();
  });

  // Page order (amendment 2026-08-14): the admin Factory card sits ABOVE the past
  // archive, so the one section that grows (Show 50 more) is always the page's tail.
  it("renders the admin Factory status card before the Past runs archive", async () => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: true, email: "me@uzi.test" },
      vaultUnlocked: true,
    } as unknown as ReturnType<typeof useAuth>);
    mockApi.listRuns.mockResolvedValue({
      runs: [pastRun("t", "Finished run", "2026-07-05T12:00:00Z")],
    });
    mockApi.adminListRuns.mockResolvedValue({ runs: [] });
    mockApi.adminListWorkers.mockResolvedValue({ workers: [] });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Finished run")).toBeTruthy());
    const factory = screen.getByText("Factory status (admin)");
    const pastHeading = screen.getByText("Past runs");
    // DOCUMENT_POSITION_FOLLOWING (4): pastHeading comes after factory in the DOM.
    expect(factory.compareDocumentPosition(pastHeading) & Node.DOCUMENT_POSITION_FOLLOWING).toBe(4);
  });
});

// PRD #295: the compact credential badge on the Runs list, gated on the viewer
// holding more than one Anthropic token.
function aSecret(over: Partial<SecretMeta> = {}): SecretMeta {
  return {
    id: "sec-1",
    kind: "anthropic_token",
    label: "default",
    is_default: true,
    auto_eligible: false,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

// A run whose claim recorded a credential — the badge's visible text is the label.
function aCredentialedRun(over: Partial<RunListItem> = {}): RunListItem {
  return aRun({
    id: "cred",
    issue_title: "Billed run",
    status: "running",
    anthropic_secret_id: "sec-x",
    anthropic_secret_label: "console-key",
    anthropic_select_reason: "auto",
    anthropic_headroom_pct: 62,
    ...over,
  });
}

describe("RunsList — credential badge gate (PRD #295)", () => {
  beforeEach(() => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: false },
      vaultUnlocked: true,
    } as unknown as ReturnType<typeof useAuth>);
  });

  // 0 and 1 token: nothing to say (every run billed the one token), so no badge.
  // 2+: the badge names which token each run billed.
  it.each([
    [0, false],
    [1, false],
    [2, true],
  ])("with %i anthropic tokens the personal badge shown=%s", async (count, shown) => {
    mockApi.listSecrets.mockResolvedValue({
      secrets: Array.from({ length: count }, (_, i) => aSecret({ id: `sec-${i}`, is_default: i === 0 })),
    });
    mockApi.listRuns.mockResolvedValue({ runs: [aCredentialedRun()] });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Billed run")).toBeTruthy());
    if (shown) {
      expect(screen.getByText("console-key")).toBeTruthy();
    } else {
      expect(screen.queryByText("console-key")).toBeNull();
    }
  });

  // A run with no recorded credential label renders no badge, at any token count. The
  // compact badge wraps its label in a `max-w-[12rem]` span (the truncation clamp,
  // web-ux F20), which is unique to this badge — so its absence is the tell that
  // RunCredential self-hid, replacing the old sr-only hint id the compact path no
  // longer emits.
  it("shows no badge for a run with no credential label, even with two tokens", async () => {
    mockApi.listSecrets.mockResolvedValue({
      secrets: [aSecret({ id: "sec-0" }), aSecret({ id: "sec-1", is_default: false })],
    });
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "bare", issue_title: "Pre-#111 run", status: "running", anthropic_secret_label: null })],
    });

    const { container } = render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Pre-#111 run")).toBeTruthy());
    expect(container.querySelector('span.max-w-\\[12rem\\]')).toBeNull();
  });

  // The blocking-bug guard: the entire row is a <Link to="/runs/:id">, so the compact
  // badge must never introduce a nested <a> — not even on a non-neutral (pool_empty)
  // credential, which the old compact path wrapped in an inner <Link to="/settings">.
  it("a personal non-neutral credential row contains no nested /settings anchor", async () => {
    mockApi.listSecrets.mockResolvedValue({
      secrets: [aSecret({ id: "sec-0" }), aSecret({ id: "sec-1", is_default: false })],
    });
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aCredentialedRun({
          id: "warn",
          issue_title: "Pool-empty run",
          anthropic_secret_label: "nearly-spent",
          anthropic_select_reason: "pool_empty",
          anthropic_headroom_pct: null,
        }),
      ],
    });

    const { container } = render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Pool-empty run")).toBeTruthy());
    // The badge renders with its label…
    expect(screen.getByText("nearly-spent")).toBeTruthy();
    // …but there is no /settings anchor anywhere — the row's own /runs/:id link stands
    // alone, with no nested <a> inside it.
    expect(container.querySelector('a[href="/settings"]')).toBeNull();
  });

  // The admin factory list shows every run's credential (an admin auditing spend
  // wants provenance) regardless of the admin's own token count — here zero — and the
  // badge never links: it lives inside the row <Link>, so an inner /settings anchor
  // would be a nested <a> (Decision 2 also noted it would point the admin at their own
  // settings for another user's token). pool_empty is non-neutral, yet still no link.
  it("admin factory rows render the credential badge but do not link it", async () => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: true, email: "me@uzi.test" },
      vaultUnlocked: true,
    } as unknown as ReturnType<typeof useAuth>);
    mockApi.listSecrets.mockResolvedValue({ secrets: [] });
    mockApi.listRuns.mockResolvedValue({ runs: [] });
    mockApi.adminListWorkers.mockResolvedValue({ workers: [] });
    mockApi.adminListRuns.mockResolvedValue({
      runs: [
        aCredentialedRun({
          id: "other",
          issue_title: "Other's run",
          owner_email: "other@uzi.test",
          anthropic_secret_id: "sec-y",
          anthropic_secret_label: "their-key",
          anthropic_select_reason: "pool_empty",
          anthropic_headroom_pct: null,
        }),
      ],
    });

    const { container } = render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Other's run")).toBeTruthy());
    // The badge renders (admin still sees provenance despite holding zero tokens).
    expect(screen.getByText("their-key")).toBeTruthy();
    // …but its /settings link is stripped. The row's own /runs/:id link is unaffected.
    expect(container.querySelector('a[href="/settings"]')).toBeNull();
  });
});
