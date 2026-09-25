// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { CodexRateLimitCard } from "./CodexRateLimitMeters";
import { api, type CodexAccountRateLimit, type CodexRateLimitBucket, type CodexRateLimitWindow, type CodexRateLimitStatus, type UserSettings } from "../lib/api";

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

  it("shows the provider-rejected guidance visibly under polling_disabled, with and without buckets (#1594)", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({
      accounts: [
        { ...acct("cdx-off-empty", ["off-empty"], false, "polling_disabled", []), reason: "provider_rejected" },
        {
          ...acct("cdx-off-buckets", ["off-buckets"], false, "polling_disabled", [
            bucket("requests", "Requests", win(52, 18000, null)),
          ]),
          reason: "provider_rejected",
          stale: true,
        },
      ],
    });
    render(<CodexRateLimitCard sidebarAccountIds={[]} onToggleSidebarAccount={noop} />);
    await screen.findByText("Codex limits");
    const rejected = "The saved Codex login was rejected by the provider; add a login used only by uzi.";
    // Each account carries the sentence twice: its sr-only badge description and one VISIBLE
    // paragraph (in place of the meters, or under the kept buckets).
    const visible = screen.getAllByText(rejected).filter((el) => !el.classList.contains("sr-only"));
    expect(visible).toHaveLength(2);
    // The bucketed account still draws its stored meter.
    expect(screen.getByText("52%")).toBeTruthy();
    expect(screen.getAllByText("Polling off")).toHaveLength(2);
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

  // PRD #1653 D-W4: the OpenAI logo leads the card title (aria-hidden, so the heading's
  // accessible name stays "Codex limits"), and it draws in the text colour.
  it("carries the OpenAI logo in the card title", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({ accounts: [freshDefault] });
    render(<CodexRateLimitCard sidebarAccountIds={[]} onToggleSidebarAccount={noop} />);
    const heading = await screen.findByRole("heading", { name: "Codex limits" });
    const logo = heading.querySelector("svg");
    expect(logo).not.toBeNull();
    expect(logo?.getAttribute("viewBox")).toBe("0 0 16 16");
    expect(logo?.getAttribute("aria-hidden")).toBe("true");
    // No per-account logo inside the card: the title's OpenAI mark is the only one.
    const marks = [...document.querySelectorAll("svg")].filter((svg) => svg.getAttribute("viewBox") === "0 0 16 16");
    expect(marks).toHaveLength(1);
  });

  // PRD #1653 D-W3: the main bucket (id "codex", empty display name, as the api builds the
  // top-level rate_limit) draws no caption; every other bucket keeps its name. Both keep
  // the bucket name in their meters' accessible names.
  it("hides the main 'codex' bucket's caption and keeps an additional bucket's", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({
      accounts: [
        acct("cdx-main", ["personal-codex"], true, "fresh", [
          bucket("codex", "", win(28, 18000, 5000), win(46, 604800, 200_000)),
          bucket("code", "Code (3-hour)", win(63, 10800, 2400)),
        ]),
      ],
    });
    render(<CodexRateLimitCard sidebarAccountIds={[]} onToggleSidebarAccount={noop} />);
    await screen.findByText("Codex limits");
    expect(screen.queryByText("codex")).toBeNull();
    expect(screen.getByText("Code (3-hour)")).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "codex 5h window" })).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "codex 7d window" })).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "Code (3-hour) 3h window" })).toBeTruthy();
    // The account is still named and the default badge stays.
    expect(screen.getByText("personal-codex")).toBeTruthy();
    expect(screen.getByText("default")).toBeTruthy();
  });

  it("keeps the limit-reached flag on a main bucket that has no caption", async () => {
    mockApi.getMyCodexRateLimits.mockResolvedValue({
      accounts: [
        acct("cdx-main", ["personal-codex"], true, "fresh", [
          bucket("codex", "", win(100, 604800, 200_000), null, { limit_reached: true, allowed: false }),
        ]),
      ],
    });
    render(<CodexRateLimitCard sidebarAccountIds={[]} onToggleSidebarAccount={noop} />);
    await screen.findByText("Codex limits");
    expect(screen.getByText("limit reached")).toBeTruthy();
    expect(screen.queryByText("codex")).toBeNull();
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

// The sidebar micro-meters moved into the combined account list (PRD #1653 D-W2);
// their tests live in SidebarUsageLimits.test.tsx.
