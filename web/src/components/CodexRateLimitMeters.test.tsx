// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { CodexRateLimitCard, SidebarCodexRateLimits } from "./CodexRateLimitMeters";
import { emitSidebarTokensChanged } from "../lib/sidebarTokens";
import { api, type CodexAccountRateLimit, type CodexRateLimitBucket, type CodexRateLimitWindow, type CodexRateLimitStatus, type UserSettings } from "../lib/api";
import { MICRO_METER_GRID_COLS } from "../lib/rateLimitLayout";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { getMyCodexRateLimits: vi.fn(), getMySettings: vi.fn() } };
});

const mockApi = vi.mocked(api);

const settings = (codexIds: string[]): { settings: UserSettings } => ({
  settings: {
    default_harness: null,
    default_model: null,
    default_effort: null,
    judge_model: null,
    summary_model: null,
    appearance_mode: null,
    light_theme: null,
    dark_theme: null,
    typeface: null,
    theme: null,
    sidebar_token_ids: [],
    sidebar_codex_account_ids: codexIds,
  },
});

beforeEach(() => {
  mockApi.getMySettings.mockResolvedValue(settings([]));
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const nowSecs = Math.floor(Date.now() / 1000);

function win(
  usedPct: number | null,
  windowSecs: number | null,
  resetIn: number | null,
): CodexRateLimitWindow {
  return {
    used_percent: usedPct,
    limit_window_seconds: windowSecs,
    reset_after_seconds: resetIn,
    reset_at: resetIn == null ? null : nowSecs + resetIn,
  };
}

function bucket(
  id: string,
  displayName: string,
  primary: CodexRateLimitWindow | null,
  secondary: CodexRateLimitWindow | null = null,
  extra: Partial<CodexRateLimitBucket> = {},
): CodexRateLimitBucket {
  return { id, display_name: displayName, allowed: true, limit_reached: false, primary, secondary, ...extra };
}

function acct(
  account_id: string,
  aliases: string[],
  is_default: boolean,
  status: CodexRateLimitStatus,
  buckets: CodexRateLimitBucket[],
  over: Partial<CodexAccountRateLimit> = {},
): CodexAccountRateLimit {
  return { account_id, aliases, is_default, status, buckets, ...over };
}

// A rich fresh default: a standard 5h/7d bucket AND a NONSTANDARD 3-hour bucket that is
// on pace to overshoot (proving both the "3h" chip and a reported-duration forecast).
const freshDefault = acct(
  "cdx-default",
  ["personal-codex"],
  true,
  "fresh",
  [
    bucket("requests", "Requests", win(28, 18000, 5000), win(46, 604800, 200_000)),
    bucket("code", "Code (3-hour)", win(90, 10800, 2400)),
  ],
  { last_success_at: new Date(Date.now() - 2 * 60_000).toISOString() },
);

describe("CodexRateLimitCard (Settings)", () => {
  const noop = () => {};

  it("renders a fresh account's meters, the reported window chips and a default badge", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({ accounts: [freshDefault] });
    render(<CodexRateLimitCard sidebarAccountIds={[]} onToggleSidebarAccount={noop} />);
    await screen.findByText("Codex limits");
    expect(screen.getByText("personal-codex")).toBeTruthy();
    expect(screen.getByText("default")).toBeTruthy();
    expect(screen.getByText("Live")).toBeTruthy();
    expect(screen.getByText("28%")).toBeTruthy();
    expect(screen.getByText("46%")).toBeTruthy();
    expect(screen.getByText("90%")).toBeTruthy();
    // The window chips are derived from the reported length: a 3-hour bucket reads "3h",
    // never a hardcoded 5h/7d. The accessible name also carries the bucket name so two
    // same-duration windows stay distinguishable to a screen reader.
    expect(screen.getByRole("progressbar", { name: "Requests 5h window" })).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "Requests 7d window" })).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "Code (3-hour) 3h window" })).toBeTruthy();
  });

  it("disambiguates two same-duration windows of a 2-bucket account by bucket name", async () => {
    // A team account with a Requests 7d window and a Tokens 7d window: same duration, so
    // the chip alone ("7d window") would give both the SAME accessible name. The bucket
    // name must qualify the meter's aria-label so a screen-reader user can tell them apart.
    mockApi.getMyCodexRateLimits.mockResolvedValue({
      accounts: [
        acct("cdx-team", ["team-codex"], true, "fresh", [
          bucket("requests", "Requests", win(40, 604800, 200_000)),
          bucket("tokens", "Tokens", win(72, 604800, 200_000)),
        ]),
      ],
    });
    render(<CodexRateLimitCard sidebarAccountIds={[]} onToggleSidebarAccount={noop} />);
    await screen.findByText("Codex limits");
    // Both 7d windows resolve to distinct, bucket-qualified accessible names.
    expect(screen.getByRole("progressbar", { name: "Requests 7d window" })).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "Tokens 7d window" })).toBeTruthy();
    // The visible chip stays the short "7d window" (rendered once per bucket).
    expect(screen.getAllByText("7d window")).toHaveLength(2);
  });

  it("draws the forecast on the 3-hour bucket from its OWN reported duration", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({ accounts: [freshDefault] });
    render(<CodexRateLimitCard sidebarAccountIds={[]} onToggleSidebarAccount={noop} />);
    await screen.findByText("Codex limits");
    // The 3h window at 90% with a near reset heads past the cap → a projected value on the
    // bar's aria-valuetext. The 5h window at 28% has headroom → no projection.
    const bar3h = screen.getByRole("progressbar", { name: "Code (3-hour) 3h window" });
    expect(bar3h.getAttribute("aria-valuetext")).toMatch(/projected \d+% by reset/);
    const bar5h = screen.getByRole("progressbar", { name: "Requests 5h window" });
    expect(bar5h.getAttribute("aria-valuetext")).not.toMatch(/projected/);
  });

  it("shows an explicit state for each non-reading status", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({
      accounts: [
        acct("cdx-p", ["p"], false, "pending", []),
        acct("cdx-n", ["n"], false, "no_reading", []),
        acct("cdx-r", ["r"], false, "credential_action_required", []),
        acct("cdx-s", ["s"], false, "stale", [bucket("requests", "Requests", win(52, 18000, null))], { stale: true }),
      ],
    });
    render(<CodexRateLimitCard sidebarAccountIds={[]} onToggleSidebarAccount={noop} />);
    await screen.findByText("Codex limits");
    expect(screen.getByText("Pending")).toBeTruthy();
    expect(screen.getByText("No reading yet")).toBeTruthy();
    expect(screen.getByText("Action required")).toBeTruthy();
    expect(screen.getByText("Stale")).toBeTruthy();
    // The stale account still shows its aged number, dimmed.
    expect(screen.getByText("52%")).toBeTruthy();
    const bar = screen.getByRole("progressbar", { name: "Requests 5h window" }).lastChild as HTMLElement;
    expect(bar.className).toMatch(/opacity-40/);
  });

  it("renders the provider-rejected hint for that account and the generic hint for the other (#1594)", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({
      accounts: [
        { ...acct("cdx-rej", ["rej"], false, "credential_action_required", []), reason: "provider_rejected" },
        acct("cdx-gen", ["gen"], false, "credential_action_required", []),
      ],
    });
    render(<CodexRateLimitCard sidebarAccountIds={[]} onToggleSidebarAccount={noop} />);
    await screen.findByText("Codex limits");
    const rejected = "The saved Codex login was rejected by the provider; add a login used only by uzi.";
    const generic =
      "This Codex login needs re-authentication before uzi can read its usage again. Replace the login in your credentials.";
    // The rejected account's sr-only description and its badge tooltip carry the rejection
    // sentence; the other account keeps the generic re-auth hint.
    expect(document.getElementById("codex-acct-status-cdx-rej")?.textContent).toBe(rejected);
    expect(document.getElementById("codex-acct-status-cdx-gen")?.textContent).toBe(generic);
    const badges = screen.getAllByText("Action required");
    expect(badges.map((b) => b.getAttribute("title")).sort()).toEqual([generic, rejected].sort());
  });

  it("shows 'no reading yet' for a partial window beside a real one", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({
      accounts: [
        acct("cdx-x", ["x"], true, "fresh", [
          bucket("requests", "Requests", win(null, 18000, 3000), win(60, 604800, 200_000)),
        ]),
      ],
    });
    render(<CodexRateLimitCard sidebarAccountIds={[]} onToggleSidebarAccount={noop} />);
    await screen.findByText("Codex limits");
    expect(screen.getByText("no reading yet")).toBeTruthy();
    expect(screen.getByText("60%")).toBeTruthy();
  });

  it("flags an exhausted bucket as limit reached at 100%", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({
      accounts: [
        acct("cdx-e", ["e"], true, "fresh", [
          bucket("tokens", "Tokens", win(100, 604800, 200_000), null, { limit_reached: true, allowed: false }),
        ]),
      ],
    });
    render(<CodexRateLimitCard sidebarAccountIds={[]} onToggleSidebarAccount={noop} />);
    await screen.findByText("Codex limits");
    expect(screen.getByText("limit reached")).toBeTruthy();
    expect(screen.getByText("100%")).toBeTruthy();
  });

  it("renders ONE account block for an account with duplicate aliases", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({
      accounts: [
        acct("cdx-team", ["team-codex", "team-codex"], true, "fresh", [
          bucket("requests", "Requests", win(30, 18000, 5000)),
        ]),
      ],
    });
    render(<CodexRateLimitCard sidebarAccountIds={[]} onToggleSidebarAccount={noop} />);
    await screen.findByText("Codex limits");
    // The duplicate aliases collapse: the label appears exactly once, one budget.
    expect(screen.getAllByText("team-codex")).toHaveLength(1);
    // One "Show in sidebar and TUI" checkbox for the one account.
    expect(screen.getAllByRole("checkbox")).toHaveLength(1);
  });

  it("renders nothing when the user has no linked subscription account (API-key-only default)", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({ accounts: [] });
    render(<CodexRateLimitCard sidebarAccountIds={[]} onToggleSidebarAccount={noop} />);
    await waitFor(() => expect(mockApi.getMyCodexRateLimits).toHaveBeenCalled());
    await Promise.resolve();
    expect(screen.queryByText("Codex limits")).toBeNull();
  });

  it("pins the default account's checkbox checked+disabled and toggles a non-default one", async () => {
    const extra = acct("cdx-extra", ["extra-codex"], false, "fresh", [
      bucket("requests", "Requests", win(77, 18000, 5000)),
    ]);
    const onToggle = vi.fn();
    mockApi.getMyCodexRateLimits.mockResolvedValue({ accounts: [freshDefault, extra] });
    render(<CodexRateLimitCard sidebarAccountIds={["cdx-extra"]} onToggleSidebarAccount={onToggle} />);
    await screen.findByText("Codex limits");

    const defaultBox = screen.getByRole("checkbox", { name: "Show personal-codex in the sidebar and TUI" }) as HTMLInputElement;
    expect(defaultBox.checked).toBe(true);
    expect(defaultBox.disabled).toBe(true);

    const extraBox = screen.getByRole("checkbox", { name: "Show extra-codex in the sidebar and TUI" }) as HTMLInputElement;
    expect(extraBox.checked).toBe(true);
    expect(extraBox.disabled).toBe(false);
    // Unchecking a shown extra writes the removal.
    fireEvent.click(extraBox);
    expect(onToggle).toHaveBeenCalledWith("cdx-extra", false);
  });

  it("checks an unselected non-default account when its box is clicked", async () => {
    const extra = acct("cdx-extra", ["extra-codex"], false, "fresh", [
      bucket("requests", "Requests", win(77, 18000, 5000)),
    ]);
    const onToggle = vi.fn();
    mockApi.getMyCodexRateLimits.mockResolvedValue({ accounts: [freshDefault, extra] });
    render(<CodexRateLimitCard sidebarAccountIds={[]} onToggleSidebarAccount={onToggle} />);
    await screen.findByText("Codex limits");
    const extraBox = screen.getByRole("checkbox", { name: "Show extra-codex in the sidebar and TUI" }) as HTMLInputElement;
    expect(extraBox.checked).toBe(false);
    fireEvent.click(extraBox);
    expect(onToggle).toHaveBeenCalledWith("cdx-extra", true);
  });
});

describe("SidebarCodexRateLimits", () => {
  const extra = acct("cdx-extra", ["extra-codex"], false, "fresh", [
    bucket("requests", "Requests", win(77, 18000, 5000)),
  ]);

  it("carries a 'Codex' provider label and shows only the default account plus a '+N more' link", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({ accounts: [freshDefault, extra] });
    mockApi.getMySettings.mockResolvedValue(settings([]));
    render(
      <MemoryRouter>
        <SidebarCodexRateLimits />
      </MemoryRouter>,
    );
    await screen.findByLabelText("Codex rate limits");
    expect(screen.getByText("Codex")).toBeTruthy();
    // The default account's numbers render; the unchecked extra's do not.
    expect(screen.getByText("28%")).toBeTruthy();
    expect(screen.queryByText("77%")).toBeNull();
    const more = await screen.findByRole("link", { name: "+1 more Codex account in Settings" });
    expect(more.getAttribute("href")).toBe("/settings");
  });

  it("also shows a checked extra account, and drops the link when nothing is hidden", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({ accounts: [freshDefault, extra] });
    mockApi.getMySettings.mockResolvedValue(settings(["cdx-extra"]));
    render(
      <MemoryRouter>
        <SidebarCodexRateLimits />
      </MemoryRouter>,
    );
    await screen.findByLabelText("Codex rate limits");
    await waitFor(() => expect(screen.getByText("77%")).toBeTruthy());
    expect(screen.getByText("28%")).toBeTruthy();
    expect(screen.queryByRole("link")).toBeNull();
  });

  // PRD #1519 M2: CodexMicroRow must use the SAME shared grid-template constant as the
  // Anthropic MicroRow (RateLimitMeters.test.tsx asserts the same constant on that side),
  // so the label column is 1.4rem on both and the 1fr meter tracks align. jsdom does no
  // layout, so this asserts the shared class, not pixel geometry.
  it("lays each Codex micro-row out on the shared grid-template constant", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({ accounts: [freshDefault] });
    mockApi.getMySettings.mockResolvedValue(settings([]));
    render(
      <MemoryRouter>
        <SidebarCodexRateLimits />
      </MemoryRouter>,
    );
    await screen.findByLabelText("Codex rate limits");
    const row = screen.getByText("5h").parentElement as HTMLElement;
    expect(row.className).toContain(MICRO_METER_GRID_COLS);
    // The constant must be the complete arbitrary-value literal (Tailwind content-scan).
    expect(MICRO_METER_GRID_COLS).toBe("grid-cols-[1.4rem_1fr_2.6rem]");
  });

  it("renders nothing when no surfaced account has a reading (pending default)", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({
      accounts: [acct("cdx-default", ["personal-codex"], true, "pending", [])],
    });
    render(
      <MemoryRouter>
        <SidebarCodexRateLimits />
      </MemoryRouter>,
    );
    await waitFor(() => expect(mockApi.getMyCodexRateLimits).toHaveBeenCalled());
    await Promise.resolve();
    expect(screen.queryByLabelText("Codex rate limits")).toBeNull();
  });

  it("dims a stale account's bars", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({
      accounts: [
        acct("cdx-default", ["personal-codex"], true, "stale", [
          bucket("requests", "Requests", win(41, 18000, 5000), win(33, 604800, 200_000)),
        ], { stale: true }),
      ],
    });
    render(
      <MemoryRouter>
        <SidebarCodexRateLimits />
      </MemoryRouter>,
    );
    await screen.findByLabelText("Codex rate limits");
    const fills = screen.getAllByRole("progressbar").map((b) => b.lastChild as HTMLElement);
    expect(fills.length).toBeGreaterThan(0);
    for (const fill of fills) expect(fill.className).toMatch(/opacity-40/);
  });

  it("refetches its selection on the shared sidebar-changed event", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({ accounts: [freshDefault, extra] });
    mockApi.getMySettings.mockResolvedValue(settings([]));
    render(
      <MemoryRouter>
        <SidebarCodexRateLimits />
      </MemoryRouter>,
    );
    await screen.findByLabelText("Codex rate limits");
    // Default only until the selection changes.
    expect(screen.queryByText("77%")).toBeNull();
    // A Settings save now includes the extra account and fires the shared event.
    mockApi.getMySettings.mockResolvedValue(settings(["cdx-extra"]));
    emitSidebarTokensChanged();
    await waitFor(() => expect(screen.getByText("77%")).toBeTruthy());
  });
});
