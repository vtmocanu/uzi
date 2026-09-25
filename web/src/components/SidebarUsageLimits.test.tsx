// @vitest-environment jsdom
//
// SidebarUsageLimits (PRD #1653 D-W2/D-W3): the sidebar's ONE account list, Claude tokens
// then Codex accounts, each a role="group" named "<Provider> account <name>" under a logo
// + name header. Also carries the cases ported from the retired per-provider sidebar
// blocks (SidebarRateLimits / SidebarCodexRateLimits), so no coverage is lost.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { SidebarUsageLimits } from "./SidebarUsageLimits";
import { emitSidebarTokensChanged } from "../lib/sidebarTokens";
import {
  api,
  type CodexAccountRateLimit,
  type CodexRateLimitBucket,
  type CodexRateLimitStatus,
  type CodexRateLimitWindow,
  type MyRateLimits,
  type TokenRateLimits,
  type UserSettings,
} from "../lib/api";
import { MICRO_METER_GRID_COLS } from "../lib/rateLimitLayout";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: { getMyRateLimits: vi.fn(), getMyCodexRateLimits: vi.fn(), getMySettings: vi.fn() },
  };
});

const mockApi = vi.mocked(api);

const settings = (tokenIds: string[], codexIds: string[]): { settings: UserSettings } => ({
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
    sidebar_token_ids: tokenIds,
    sidebar_codex_account_ids: codexIds,
  },
});

const nowSecs = Math.floor(Date.now() / 1000);

const okReading: MyRateLimits = {
  status: "ok",
  five_hour: { pct: 8, resets_at: nowSecs + 5000 },
  seven_day: { pct: 27, resets_at: nowSecs + 200_000 },
  source: "usage_endpoint",
  synced_at: new Date(Date.now() - 2 * 60_000).toISOString(),
  stale: false,
};
const warnReading: MyRateLimits = {
  ...okReading,
  five_hour: { pct: 62, resets_at: nowSecs + 5000 },
  seven_day: { pct: 83, resets_at: nowSecs + 200_000 },
};
const staleReading: MyRateLimits = {
  status: "ok",
  five_hour: { pct: 31, resets_at: null },
  seven_day: { pct: 12, resets_at: null },
  source: "header_probe",
  synced_at: new Date(Date.now() - 3 * 3600_000).toISOString(),
  stale: true,
};

function token(
  secret_id: string,
  label: string,
  is_default: boolean,
  limits: MyRateLimits,
): TokenRateLimits {
  return { secret_id, label, is_default, auto_eligible: false, auto_status: "not_pooled", limits };
}

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
): CodexRateLimitBucket {
  return { id, display_name: displayName, allowed: true, limit_reached: false, primary, secondary };
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

// The Claude default token and a second, unchecked one (hotter, to prove the rail shows
// the user's choice rather than the most constrained token).
const team = token("sec-1", "team", true, okReading);
const consoleKey = token("sec-2", "console-key", false, warnReading);

// The Codex default account shaped like the api builds it (codexauth/usage.go): the main
// limit is the "codex" bucket with an EMPTY display name.
const codexDefault = acct("cdx-default", ["personal-codex"], true, "fresh", [
  bucket("codex", "", win(28, 18000, 5000), win(46, 604800, 200_000)),
]);
const codexExtra = acct("cdx-extra", ["extra-codex"], false, "fresh", [
  bucket("codex", "", win(77, 18000, 5000)),
]);

function mockData(tokens: TokenRateLimits[], accounts: CodexAccountRateLimit[]) {
  mockApi.getMyRateLimits.mockResolvedValue({ tokens });
  mockApi.getMyCodexRateLimits.mockResolvedValue({ accounts });
}

function renderList() {
  return render(
    <MemoryRouter>
      <SidebarUsageLimits />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockApi.getMySettings.mockResolvedValue(settings([], []));
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("SidebarUsageLimits: one account list (PRD #1653 D-W2)", () => {
  it("renders one group per provider, Claude first, each named by provider and account", async () => {
    mockData([team], [codexDefault]);
    renderList();
    await screen.findByLabelText("Usage limits");
    const claude = await screen.findByRole("group", { name: "Claude account team" });
    const codex = await screen.findByRole("group", { name: "Codex account personal-codex" });
    expect(screen.getAllByRole("group")).toHaveLength(2);
    // Claude precedes Codex in document order.
    expect(claude.compareDocumentPosition(codex) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // Each group holds its own meters.
    expect(within(claude).getByText("8%")).toBeTruthy();
    expect(within(codex).getByText("28%")).toBeTruthy();
  });

  it("names a lone Claude token in a visible header", async () => {
    mockData([team], []);
    renderList();
    const group = await screen.findByRole("group", { name: "Claude account team" });
    expect(within(group).getByText("team")).toBeTruthy();
  });

  it("names a lone Codex account in a visible header", async () => {
    mockData([], [codexDefault]);
    renderList();
    const group = await screen.findByRole("group", { name: "Codex account personal-codex" });
    expect(within(group).getByText("personal-codex")).toBeTruthy();
  });

  it("draws no provider word: no 'Codex' eyebrow, the logo titles name the providers", async () => {
    mockData([team], [codexDefault]);
    const { container } = renderList();
    await screen.findByRole("group", { name: "Codex account personal-codex" });
    // The retired Codex eyebrow read exactly "Codex"; no visible text names a provider.
    expect(screen.queryByText("Codex")).toBeNull();
    expect(container.textContent).not.toMatch(/Codex|Claude/);
    // The provider is carried by the logos' hover titles, beside aria-hidden svgs.
    const claudeMark = screen.getByTitle("Claude");
    const codexMark = screen.getByTitle("Codex");
    for (const mark of [claudeMark, codexMark]) {
      expect(mark.querySelector("svg")?.getAttribute("aria-hidden")).toBe("true");
    }
  });

  it("combines hidden Claude tokens and Codex accounts into one '+N more accounts' link", async () => {
    mockData([team, consoleKey], [codexDefault, codexExtra]);
    renderList();
    const more = await screen.findByRole("link", { name: "+2 more accounts in Settings" });
    expect(more.getAttribute("href")).toBe("/settings");
    expect(screen.getAllByRole("link")).toHaveLength(1);
    // The unchecked extras' numbers do not render.
    expect(screen.queryByText("62%")).toBeNull();
    expect(screen.queryByText("77%")).toBeNull();
  });

  it("uses the singular for one hidden account", async () => {
    mockData([team], [codexDefault, codexExtra]);
    renderList();
    const more = await screen.findByRole("link", { name: "+1 more account in Settings" });
    expect(more.getAttribute("href")).toBe("/settings");
  });

  it("renders nothing when neither provider has a readable account", async () => {
    mockData(
      [token("sec-1", "team", true, { status: "unavailable" })],
      [acct("cdx-default", ["personal-codex"], true, "pending", [])],
    );
    renderList();
    await waitFor(() => expect(mockApi.getMyRateLimits).toHaveBeenCalled());
    await waitFor(() => expect(mockApi.getMyCodexRateLimits).toHaveBeenCalled());
    await Promise.resolve();
    expect(screen.queryByLabelText("Usage limits")).toBeNull();
    expect(screen.queryByRole("group")).toBeNull();
  });

  it("renders nothing for a user with no credential at all", async () => {
    mockData([], []);
    renderList();
    await waitFor(() => expect(mockApi.getMyCodexRateLimits).toHaveBeenCalled());
    await Promise.resolve();
    expect(screen.queryByLabelText("Usage limits")).toBeNull();
  });

  it("draws no empty group for a readable Codex account whose windows carry no percentage", async () => {
    const noRows = acct("cdx-default", ["personal-codex"], true, "fresh", [
      bucket("codex", "", win(null, 18000, 5000), win(null, 604800, 200_000)),
    ]);
    mockData([team], [noRows]);
    renderList();
    await screen.findByRole("group", { name: "Claude account team" });
    expect(screen.queryByRole("group", { name: /^Codex account/ })).toBeNull();
    expect(screen.getAllByRole("group")).toHaveLength(1);
  });

  it("renders nothing when the only readable account has no drawable rows", async () => {
    const noRows = acct("cdx-default", ["personal-codex"], true, "fresh", [
      bucket("codex", "", win(null, 18000, 5000)),
    ]);
    mockData([], [noRows]);
    renderList();
    await waitFor(() => expect(mockApi.getMyCodexRateLimits).toHaveBeenCalled());
    await Promise.resolve();
    expect(screen.queryByLabelText("Usage limits")).toBeNull();
  });
});

describe("SidebarUsageLimits: Codex bucket captions (PRD #1653 D-W3)", () => {
  it("shows no caption for an account whose only bucket is the main 'codex' one", async () => {
    mockData([], [codexDefault]);
    renderList();
    const group = await screen.findByRole("group", { name: "Codex account personal-codex" });
    expect(within(group).queryByText("codex")).toBeNull();
    // The window rows keep the bucket name in their accessible names.
    expect(within(group).getByRole("progressbar", { name: "codex 5h window" })).toBeTruthy();
    expect(within(group).getByRole("progressbar", { name: "codex 7d window" })).toBeTruthy();
  });

  it("captions an additional bucket by its name, still not the main one", async () => {
    const withExtra = acct("cdx-default", ["personal-codex"], true, "fresh", [
      bucket("codex", "", win(28, 18000, 5000)),
      bucket("code", "Code (3-hour)", win(63, 10800, 2400)),
    ]);
    mockData([], [withExtra]);
    renderList();
    const group = await screen.findByRole("group", { name: "Codex account personal-codex" });
    expect(within(group).getByText("Code (3-hour)")).toBeTruthy();
    expect(within(group).queryByText("codex")).toBeNull();
    expect(within(group).getByRole("progressbar", { name: "Code (3-hour) 3h window" })).toBeTruthy();
    expect(within(group).getByText("3h")).toBeTruthy();
  });
});

// Ported from the retired SidebarRateLimits tests (RateLimitMeters.test.tsx).
describe("SidebarUsageLimits: Claude rows", () => {
  it("shows the 5h/7d micro-bars on an ok reading", async () => {
    mockData([team], []);
    renderList();
    await screen.findByLabelText("Usage limits");
    expect(screen.getByText("5h")).toBeTruthy();
    expect(screen.getByText("7d")).toBeTruthy();
    expect(screen.getByText("8%")).toBeTruthy();
  });

  // PRD #1519 M2: both providers' micro-rows use the shared grid-template constant so
  // their 1fr meter tracks line up. jsdom does no layout, so this asserts the class.
  it("lays each Claude and Codex micro-row out on the shared grid-template constant", async () => {
    mockData([team], [codexDefault]);
    renderList();
    const claude = await screen.findByRole("group", { name: "Claude account team" });
    const codex = await screen.findByRole("group", { name: "Codex account personal-codex" });
    const claudeRow = within(claude).getByText("5h").parentElement as HTMLElement;
    const codexRow = within(codex).getByText("5h").parentElement as HTMLElement;
    expect(claudeRow.className).toContain(MICRO_METER_GRID_COLS);
    expect(codexRow.className).toContain(MICRO_METER_GRID_COLS);
    // The constant must be the complete arbitrary-value literal (Tailwind content-scan).
    expect(MICRO_METER_GRID_COLS).toBe("grid-cols-[1.4rem_1fr_2.6rem]");
  });

  // The forecast rows carry the "resets in …" countdown, folded into MicroRow's valueText
  // so it flows into RateLimitForecast's tooltip: a 5h window at 90% with a near reset
  // projects past the cap, so the title holds both "resets in" and "projected".
  it("carries the resets-in countdown into a forecast row's title", async () => {
    const forecastReading: MyRateLimits = {
      status: "ok",
      five_hour: { pct: 90, resets_at: nowSecs + 5000 },
      seven_day: { pct: 20, resets_at: nowSecs + 200_000 },
      source: "usage_endpoint",
      synced_at: new Date().toISOString(),
      stale: false,
    };
    mockData([token("sec-1", "team", true, forecastReading)], []);
    renderList();
    const bar5h = await screen.findByRole("progressbar", { name: "5h window" });
    const title = (bar5h.closest("[title]") as HTMLElement).getAttribute("title") ?? "";
    expect(title).toMatch(/resets in/);
    expect(title).toMatch(/projected/);
  });

  it("dims both micro-bars on a stale reading", async () => {
    mockData([token("sec-1", "team", true, staleReading)], []);
    renderList();
    await screen.findByLabelText("Usage limits");
    const fills = screen.getAllByRole("progressbar").map((b) => b.lastChild as HTMLElement);
    expect(fills).toHaveLength(2);
    for (const fill of fills) expect(fill.className).toMatch(/opacity-40/);
  });

  // The rail shows the USER'S chosen set: the default token always, plus checked extras,
  // never an automatic pick. Nothing checked: only the default renders, however hot.
  it("shows only the default token plus a '+N more' link when nothing else is checked", async () => {
    mockData([team, consoleKey], []);
    renderList();
    await screen.findByRole("group", { name: "Claude account team" });
    expect(screen.getAllByRole("progressbar")).toHaveLength(2);
    expect(screen.getByText("8%")).toBeTruthy();
    expect(screen.queryByText("62%")).toBeNull();
    expect(screen.queryByRole("group", { name: "Claude account console-key" })).toBeNull();
    const more = await screen.findByRole("link", { name: "+1 more account in Settings" });
    expect(more.getAttribute("href")).toBe("/settings");
  });

  it("also shows a checked extra token, and drops the link when nothing is hidden", async () => {
    mockApi.getMySettings.mockResolvedValue(settings(["sec-2"], []));
    mockData([team, consoleKey], []);
    renderList();
    await screen.findByRole("group", { name: "Claude account console-key" });
    await waitFor(() => expect(screen.getAllByRole("progressbar")).toHaveLength(4));
    expect(screen.getByRole("group", { name: "Claude account team" })).toBeTruthy();
    // Server order: the default first.
    const groups = screen.getAllByRole("group").map((g) => g.getAttribute("aria-label"));
    expect(groups).toEqual(["Claude account team", "Claude account console-key"]);
    expect(screen.queryByRole("link")).toBeNull();
  });
});

// Ported from the retired SidebarCodexRateLimits tests (CodexRateLimitMeters.test.tsx).
describe("SidebarUsageLimits: Codex rows", () => {
  it("shows only the default account plus a '+N more' link when nothing is checked", async () => {
    mockData([], [codexDefault, codexExtra]);
    renderList();
    await screen.findByRole("group", { name: "Codex account personal-codex" });
    expect(screen.getByText("28%")).toBeTruthy();
    expect(screen.queryByText("77%")).toBeNull();
    const more = await screen.findByRole("link", { name: "+1 more account in Settings" });
    expect(more.getAttribute("href")).toBe("/settings");
  });

  it("also shows a checked extra account, and drops the link when nothing is hidden", async () => {
    mockApi.getMySettings.mockResolvedValue(settings([], ["cdx-extra"]));
    mockData([], [codexDefault, codexExtra]);
    renderList();
    await screen.findByRole("group", { name: "Codex account extra-codex" });
    expect(screen.getByText("77%")).toBeTruthy();
    expect(screen.getByText("28%")).toBeTruthy();
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("dims a stale account's bars", async () => {
    mockData(
      [],
      [
        acct("cdx-default", ["personal-codex"], true, "stale", [
          bucket("codex", "", win(41, 18000, 5000), win(33, 604800, 200_000)),
        ], { stale: true }),
      ],
    );
    renderList();
    await screen.findByRole("group", { name: "Codex account personal-codex" });
    const fills = screen.getAllByRole("progressbar").map((b) => b.lastChild as HTMLElement);
    expect(fills.length).toBeGreaterThan(0);
    for (const fill of fills) expect(fill.className).toMatch(/opacity-40/);
  });
});

describe("SidebarUsageLimits: selection refetch", () => {
  it("refetches both providers' selection on the shared sidebar-changed event", async () => {
    mockData([team, consoleKey], [codexDefault, codexExtra]);
    renderList();
    await screen.findByRole("link", { name: "+2 more accounts in Settings" });
    expect(screen.queryByText("62%")).toBeNull();
    expect(screen.queryByText("77%")).toBeNull();
    // A Settings save now includes both extras and fires the shared event.
    mockApi.getMySettings.mockResolvedValue(settings(["sec-2"], ["cdx-extra"]));
    emitSidebarTokensChanged();
    await waitFor(() => expect(screen.getByText("77%")).toBeTruthy());
    expect(screen.getByText("62%")).toBeTruthy();
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("keeps the last known selection when a refetch fails", async () => {
    mockApi.getMySettings.mockResolvedValue(settings([], ["cdx-extra"]));
    mockData([team], [codexDefault, codexExtra]);
    renderList();
    await screen.findByRole("group", { name: "Codex account extra-codex" });
    mockApi.getMySettings.mockRejectedValue(new Error("network"));
    emitSidebarTokensChanged();
    await waitFor(() => expect(mockApi.getMySettings).toHaveBeenCalledTimes(2));
    await Promise.resolve();
    expect(screen.getByRole("group", { name: "Codex account extra-codex" })).toBeTruthy();
  });
});
