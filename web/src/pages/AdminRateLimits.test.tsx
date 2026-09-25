// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminRateLimits } from "./AdminRateLimits";
import {
  api,
  type AdminRateLimitUser,
  type CodexAccountRateLimit,
  type CodexAdminRateLimitRow,
  type CodexRateLimitBucket,
  type CodexRateLimitStatus,
  type CodexRateLimitWindow,
  type MyRateLimits,
} from "../lib/api";
import { mockAdminCodexRateLimits } from "../mocks/data/codexRateLimits";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { getAdminRateLimits: vi.fn(), getAdminCodexRateLimits: vi.fn() } };
});

const mockApi = vi.mocked(api);

beforeEach(() => {
  // The page now also renders the Codex section, which reads /admin/codex-rate-limits.
  // Default to no linked Codex accounts so the section self-hides and these Claude-table
  // assertions are unaffected; the Codex-specific tests override this.
  mockApi.getAdminCodexRateLimits.mockResolvedValue({ users: [] });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.clearAllMocks();
});

const nowSecs = Math.floor(Date.now() / 1000);
function ok(pct5: number, pct7: number, over: Partial<Extract<MyRateLimits, { status: "ok" }>> = {}): MyRateLimits {
  return {
    status: "ok",
    five_hour: { pct: pct5, resets_at: nowSecs + 5000 },
    seven_day: { pct: pct7, resets_at: nowSecs + 200_000 },
    source: "usage_endpoint",
    synced_at: new Date(Date.now() - 60_000).toISOString(),
    stale: false,
    ...over,
  };
}
// One reading per TOKEN since PRD #104 M5; these tests exercise a user's single
// credential, so row() wraps it as their default and a "no_token" reading becomes
// the empty list the API sends for a token-less user.
function row(name: string, limits: MyRateLimits, vault_locked = false): AdminRateLimitUser {
  return {
    id: name,
    name,
    email: `${name}@example.com`,
    vault_locked,
    tokens:
      limits.status === "no_token"
        ? []
        : [
            {
              secret_id: `sec-${name}`,
              label: "default",
              is_default: true,
              // PRD #111 M2 rides every token row; these fixtures are about the
              // rate-limit classification, so they stay un-pooled.
              auto_eligible: false,
              auto_status: "not_pooled" as const,
              limits,
            },
          ],
  };
}

const USERS = [
  row("irina", { status: "no_token" }),
  row("vlad", ok(8, 27)),
  row("ana", ok(97, 71)),
  row("dana", { status: "unavailable" }),
  row("radu", ok(62, 83)),
  row("mihai", ok(31, 12, { stale: true }), true),
];

describe("AdminRateLimits", () => {
  it("renders the always-visible rate-limits guide link", async () => {
    mockApi.getAdminRateLimits.mockResolvedValue({ users: USERS });
    render(
    <MemoryRouter>
      <AdminRateLimits />
    </MemoryRouter>,
  );
    const link = await screen.findByRole("link", { name: "Claude rate limits" });
    expect(link.getAttribute("href")).toBe("/docs/rate-limits");
  });

  it("renders every user, badges each state, and sorts danger-first", async () => {
    mockApi.getAdminRateLimits.mockResolvedValue({ users: USERS });
    render(
    <MemoryRouter>
      <AdminRateLimits />
    </MemoryRouter>,
  );
    await screen.findByText("ana");

    // Sort: danger → warn → ok → stale → unavailable → no_token.
    const names = screen
      .getAllByRole("row")
      .slice(1)
      .map((r) => within(r).getAllByRole("cell")[0].querySelector("div")!.textContent);
    expect(names).toEqual(["ana", "radu", "vlad", "mihai", "dana", "irina"]);

    // Per-state badges (mockup frame C).
    expect(screen.getByText("5h nearly out")).toBeTruthy();
    expect(screen.getByText("🔒 vault locked")).toBeTruthy();
    expect(screen.getByText("no token")).toBeTruthy();
    expect(screen.getByText("no reading yet")).toBeTruthy();
    expect(screen.getAllByText("Live")).toHaveLength(2); // vlad + radu (warn stays Live)
  });

  it("paints an 85–94 danger-band row's bar red while its badge stays Live (PRD #115)", async () => {
    // sorin's worst window is 88%: danger TONE (≥85) but not ≥95, so the bar goes
    // red via the danger fill while the status pill stays a green "Live" — the
    // badge stays decoupled at ≥95.
    mockApi.getAdminRateLimits.mockResolvedValue({ users: [row("sorin", ok(88, 76))] });
    render(
    <MemoryRouter>
      <AdminRateLimits />
    </MemoryRouter>,
  );
    const sorin = (await screen.findByText("sorin")).closest("tr")!;
    // Forecast wrapper paints the fill LAST (opaque, over the ghost), so it is the
    // progressbar's lastChild (MeterTrack used directly has the fill as its only child).
    const bar5h = within(sorin).getByRole("progressbar", { name: "5-hour window" })
      .lastChild as HTMLElement;
    expect(bar5h.className).toMatch(/bg-danger/);
    expect(within(sorin).getByText("Live")).toBeTruthy();
    expect(within(sorin).queryByText(/nearly out/)).toBeNull();
  });

  it("badges a limit_report row as recorded at the usage limit, not other rows (PRD #217)", async () => {
    // nadia's five-hour window was recorded 100% at a usage-limit park (source
    // limit_report), synced_at deliberately older than the reading; vlad is an
    // ordinary poll. The park-time badge must sit on nadia's row only.
    mockApi.getAdminRateLimits.mockResolvedValue({
      users: [
        row("nadia", ok(100, 40, { source: "limit_report", synced_at: new Date(Date.now() - 14 * 60_000).toISOString() })),
        row("vlad", ok(8, 27)),
      ],
    });
    render(
    <MemoryRouter>
      <AdminRateLimits />
    </MemoryRouter>,
  );
    const nadia = (await screen.findByText("nadia")).closest("tr")!;
    expect(within(nadia).getByText("Recorded at usage limit")).toBeTruthy();
    // The updated line still renders alongside the disclosure.
    expect(within(nadia).getByText(/updated/)).toBeTruthy();

    const vlad = screen.getByText("vlad").closest("tr")!;
    expect(within(vlad).queryByText("Recorded at usage limit")).toBeNull();
  });

  it("renders a faint 'no name' placeholder for a user with an empty name (PRD #54)", async () => {
    mockApi.getAdminRateLimits.mockResolvedValue({ users: [row("", ok(8, 27))] });
    render(
    <MemoryRouter>
      <AdminRateLimits />
    </MemoryRouter>,
  );
    await screen.findByText("no name");
    // The placeholder must be the first div's textContent (the sort test reads the
    // name via querySelector("div")), so the email below never floats an empty line.
    const nameCell = screen.getAllByRole("cell")[0];
    expect(nameCell.querySelector("div")!.textContent).toBe("no name");
    expect(within(nameCell).getByText("no name").className).toMatch(/text-faint/);
  });

  it("shows a dim 'stale' reset on the vault-locked row and dashes for no-reading rows", async () => {
    mockApi.getAdminRateLimits.mockResolvedValue({ users: USERS });
    render(
    <MemoryRouter>
      <AdminRateLimits />
    </MemoryRouter>,
  );
    const mihai = (await screen.findByText("mihai")).closest("tr")!;
    expect(within(mihai).getAllByText("stale").length).toBe(2); // both windows read "stale"
    expect(within(mihai).getByText("31%")).toBeTruthy(); // pct still shown, just dimmed

    const irina = screen.getByText("irina").closest("tr")!;
    // no_token → em-dashes in the token cell and the single Utilization cell.
    // (Two since PRD #240 collapsed the two window columns into one and folded the
    // Updated column under the Status pill — a no_token row shows no "updated" line,
    // so its Updated dash is gone too. The badge keeps its own cell so the row stays
    // aligned with the live ones.)
    expect(within(irina).getAllByText("—").length).toBe(2);
  });

  it("renders the four-column header and no Updated/window columns (PRD #240 regression)", async () => {
    // Pin the fix's intent: the table is User · Token · Utilization & Forecast ·
    // Status. If a future change re-splits Utilization back into two window columns
    // (or re-adds a standalone Updated column), the horizontal-scroll bug this PRD
    // fixed returns — so this asserts the collapsed shape, not just that the headers
    // render. The Utilization column carries the anchored forecast (PRD #310), hence
    // its renamed header.
    mockApi.getAdminRateLimits.mockResolvedValue({ users: USERS });
    render(
    <MemoryRouter>
      <AdminRateLimits />
    </MemoryRouter>,
  );
    await screen.findByText("ana");

    const headers = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(headers).toEqual(["User", "Token", "Utilization & Forecast", "Status"]);
    expect(screen.queryByRole("columnheader", { name: "Updated" })).toBeNull();
    expect(screen.queryByRole("columnheader", { name: "5-hour window" })).toBeNull();
    expect(screen.queryByRole("columnheader", { name: "7-day window" })).toBeNull();
  });
});

// PRD #310 — the anchored forecast wired through the admin table (its primary
// target). The projection comes from a SINGLE reading, so one render of a row
// heading past the cap already shows the ghost/marker — no sample warm-up.
describe("AdminRateLimits forecast (PRD #310 — anchored, always-on)", () => {
  it("draws a forecast ghost + » on a row heading past the cap, with no warm-up", async () => {
    // ana's 5h window at 90% with a near reset (elapsed ≈ 13000s ⇒ projected ≈ 125).
    mockApi.getAdminRateLimits.mockResolvedValue({ users: [row("ana", ok(90, 20))] });
    render(
    <MemoryRouter>
      <AdminRateLimits />
    </MemoryRouter>,
  );

    const ana = (await screen.findByText("ana")).closest("tr")!;
    const bar5h = within(ana).getByRole("progressbar", { name: "5-hour window" });
    expect(bar5h.getAttribute("aria-valuetext")).toMatch(/projected \d+% by reset, over$/);
    expect(within(ana).getByTestId("forecast-overflow-marker")).toBeTruthy();
  });

  it("stays a plain bar on a low reading with headroom", async () => {
    mockApi.getAdminRateLimits.mockResolvedValue({ users: [row("vlad", ok(20, 20))] });
    render(
    <MemoryRouter>
      <AdminRateLimits />
    </MemoryRouter>,
  );

    const vlad = (await screen.findByText("vlad")).closest("tr")!;
    const bar5h = within(vlad).getByRole("progressbar", { name: "5-hour window" });
    expect(bar5h.getAttribute("aria-valuetext")).not.toMatch(/projected/);
    expect(within(vlad).queryByTestId("forecast-overflow-marker")).toBeNull();
  });

  it("draws no forecast on a stale row heading past the cap", async () => {
    mockApi.getAdminRateLimits.mockResolvedValue({ users: [row("mihai", ok(90, 20, { stale: true }))] });
    render(
    <MemoryRouter>
      <AdminRateLimits />
    </MemoryRouter>,
  );

    const mihai = (await screen.findByText("mihai")).closest("tr")!;
    expect(within(mihai).queryByTestId("forecast-overflow-marker")).toBeNull();
  });
});

// PRD #310 M3 — the absolute "resets <Day HH:MM>" line under the token name in the
// Token cell, from the 7-day reset. The utilization column shows a bare countdown
// ("1h 23m"), not a "resets …" string, so the label regex is unambiguous.
describe("AdminRateLimits reset label (PRD #310 M3)", () => {
  const RESET_LABEL = /resets [A-Za-z]{3} \d{2}:\d{2}/;

  it("renders the 7-day reset label under the token name", async () => {
    mockApi.getAdminRateLimits.mockResolvedValue({ users: [row("ana", ok(90, 20))] }); // 7d set
    render(
    <MemoryRouter>
      <AdminRateLimits />
    </MemoryRouter>,
  );
    const ana = (await screen.findByText("ana")).closest("tr")!;
    expect(ana.textContent).toMatch(RESET_LABEL);
  });

  it("omits the label when the 7-day resets_at is null", async () => {
    mockApi.getAdminRateLimits.mockResolvedValue({
      users: [row("vlad", ok(20, 20, { seven_day: { pct: 20, resets_at: null } }))],
    });
    render(
    <MemoryRouter>
      <AdminRateLimits />
    </MemoryRouter>,
  );
    const vlad = (await screen.findByText("vlad")).closest("tr")!;
    expect(vlad.textContent).not.toMatch(RESET_LABEL);
  });
});

// ── Codex accounts section (PRD #1209 M3) ────────────────────────────────────
const cnowSecs = Math.floor(Date.now() / 1000);
function cwin(usedPct: number | null, windowSecs: number | null, resetIn: number | null): CodexRateLimitWindow {
  return {
    used_percent: usedPct,
    limit_window_seconds: windowSecs,
    reset_after_seconds: resetIn,
    reset_at: resetIn == null ? null : cnowSecs + resetIn,
  };
}
function cbucket(id: string, name: string, primary: CodexRateLimitWindow | null, secondary: CodexRateLimitWindow | null = null): CodexRateLimitBucket {
  return { id, display_name: name, allowed: true, limit_reached: false, primary, secondary };
}
function cacct(account_id: string, aliases: string[], is_default: boolean, status: CodexRateLimitStatus, buckets: CodexRateLimitBucket[], over: Partial<CodexAccountRateLimit> = {}): CodexAccountRateLimit {
  return { account_id, aliases, is_default, status, buckets, ...over };
}
function crow(name: string, accounts: CodexAccountRateLimit[], vault_locked = false): CodexAdminRateLimitRow {
  return { id: name, name, email: `${name}@example.com`, vault_locked, accounts };
}

describe("AdminRateLimits — Codex section", () => {
  // Keep the Claude side empty so its table does not interfere; the Codex section is
  // scoped by its own aria-label region in every assertion.
  beforeEach(() => {
    mockApi.getAdminRateLimits.mockResolvedValue({ users: [] });
  });

  const codexUsers: CodexAdminRateLimitRow[] = [
    // A warn-tone user (60%) — should sort BELOW the danger user.
    crow("warmian", [cacct("cdx-w", ["warm-codex"], true, "fresh", [cbucket("requests", "Requests", cwin(60, 18000, 5000))])]),
    // A danger-tone user (96%) with a NONSTANDARD 3-hour bucket — should lead.
    crow("hotpat", [cacct("cdx-h", ["hot-codex"], true, "fresh", [cbucket("code", "Code", cwin(96, 10800, 2000))])]),
    // A non-reading user — sinks to the bottom, utilization em-dash.
    crow("pendra", [cacct("cdx-p", ["pend-codex"], true, "pending", [])]),
  ];

  const section = () => screen.getByLabelText("Codex accounts");

  it("renders a separate Codex section with its own four-column table", async () => {
    mockApi.getAdminCodexRateLimits.mockResolvedValue({ users: codexUsers });
    render(
      <MemoryRouter>
        <AdminRateLimits />
      </MemoryRouter>,
    );
    await screen.findByText("hot-codex");
    const headers = within(section()).getAllByRole("columnheader").map((h) => h.textContent);
    expect(headers).toEqual(["User", "Account", "Utilization & Forecast", "Status"]);
  });

  it("sorts danger-first and sinks the non-reading user, with the reported 3h chip", async () => {
    mockApi.getAdminCodexRateLimits.mockResolvedValue({ users: codexUsers });
    render(
      <MemoryRouter>
        <AdminRateLimits />
      </MemoryRouter>,
    );
    await screen.findByText("hot-codex");
    const names = within(section())
      .getAllByRole("row")
      .slice(1)
      .map((r) => within(r).getAllByRole("cell")[0].querySelector("div")!.textContent);
    expect(names).toEqual(["hotpat", "warmian", "pendra"]);
    // The 3-hour bucket's chip is derived from the reported length, not hardcoded 5h/7d.
    expect(within(section()).getByRole("progressbar", { name: "Code 3h window" })).toBeTruthy();
    expect(within(section()).getByText("96%")).toBeTruthy();
  });

  it("collapses a non-reading account to an em-dash and badges its status", async () => {
    mockApi.getAdminCodexRateLimits.mockResolvedValue({ users: [crow("pendra", [cacct("cdx-p", ["pend-codex"], true, "pending", [])])] });
    render(
      <MemoryRouter>
        <AdminRateLimits />
      </MemoryRouter>,
    );
    await screen.findByText("pend-codex");
    const pendra = within(section()).getByText("pend-codex").closest("tr")!;
    expect(within(pendra).getByText("—")).toBeTruthy();
    expect(within(pendra).getByText("Pending")).toBeTruthy();
  });

  it("names a provider-rejected login in the status hint, from the account's reason (#1594)", async () => {
    // The shared mock set: carmen's account carries reason "provider_rejected", so the
    // page must pass the WHOLE account (status AND reason) to codexStatusBadge.
    mockApi.getAdminCodexRateLimits.mockResolvedValue({ users: mockAdminCodexRateLimits });
    render(
      <MemoryRouter>
        <AdminRateLimits />
      </MemoryRouter>,
    );
    await screen.findByText("carmen-codex");
    const carmen = within(section()).getByText("carmen-codex").closest("tr")!;
    const badge = within(carmen).getByText("Action required");
    expect(badge.getAttribute("title")).toBe(
      "The saved Codex login was rejected by the provider; add a login used only by uzi.",
    );
  });

  it("self-hides when no user has a linked Codex account", async () => {
    mockApi.getAdminCodexRateLimits.mockResolvedValue({ users: [] });
    render(
      <MemoryRouter>
        <AdminRateLimits />
      </MemoryRouter>,
    );
    // The Claude side settles to its empty state; the Codex section never appears.
    await screen.findByText("No users yet");
    expect(screen.queryByLabelText("Codex accounts")).toBeNull();
  });

  it("stays self-hidden on a transient poll error after a confirmed-empty load (no dead chrome on a non-Codex instance)", async () => {
    // data === [] (a confirmed-empty first load) must self-hide even when a later poll
    // fails: a non-Codex instance must never flash a "Codex accounts / Failed to load"
    // banner on a routine poll blip. Only data === null (never-loaded) shows the alert.
    vi.useFakeTimers();
    mockApi.getAdminCodexRateLimits.mockResolvedValue({ users: [] });
    render(
      <MemoryRouter>
        <AdminRateLimits />
      </MemoryRouter>,
    );
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.queryByLabelText("Codex accounts")).toBeNull();

    mockApi.getAdminCodexRateLimits.mockRejectedValueOnce(new Error("poll blip"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(mockApi.getAdminCodexRateLimits).toHaveBeenCalledTimes(2); // confirmed-empty load + one failed poll
    // Still hidden — the transient error over a confirmed-empty list draws nothing.
    expect(screen.queryByLabelText("Codex accounts")).toBeNull();
  });

  it("keeps the last-good Codex table when a poll fails, showing the alert above it", async () => {
    // A single transient poll failure must NOT blank the whole capacity table for a
    // poll interval: useAsyncData keeps last-good data on a failed reload, so the rows
    // stay and the alert renders above them (mirroring the Claude table).
    vi.useFakeTimers();
    mockApi.getAdminCodexRateLimits.mockResolvedValue({ users: codexUsers });
    render(
      <MemoryRouter>
        <AdminRateLimits />
      </MemoryRouter>,
    );
    // Flush the successful first load: the table renders with a known account + percentage.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(within(section()).getByText("hot-codex")).toBeTruthy();
    expect(within(section()).getByText("96%")).toBeTruthy();

    // The next 60s poll rejects; the section must swallow it, keep the last-good rows,
    // and surface the error above the table rather than collapsing to an alert-only view.
    mockApi.getAdminCodexRateLimits.mockRejectedValueOnce(new Error("poll blip"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(mockApi.getAdminCodexRateLimits).toHaveBeenCalledTimes(2); // first load + one poll

    // Last-good rows persist (assert the actual account row + percentage + its meter)...
    expect(within(section()).getByText("hot-codex")).toBeTruthy();
    expect(within(section()).getByText("96%")).toBeTruthy();
    expect(within(section()).getByRole("progressbar", { name: "Code 3h window" })).toBeTruthy();
    // ...and the transient error is shown (the fallback message, since it is not an ApiError).
    expect(within(section()).getByText("Failed to load Codex rate limits")).toBeTruthy();
  });
});

// ── Flush Card outer-edge padding vs rowspan (PRD #1648 D8) ──────────────────
// A flush Card pads each row's first td to 20px. On a rowspan continuation row the
// first td in the DOM is the token/account cell, which sits right of the spanning
// user cell, so it must carry data-flush-inner and fall outside that selector. The
// selector is derived from the Card's own class token, so this pins the real rule.
describe("AdminRateLimits — flush Card first-cell selector", () => {
  function flushCard(el: Element): Element {
    let cur: Element | null = el;
    while (cur) {
      if (Array.from(cur.classList).some((c) => c.startsWith("[&_td:first-child"))) return cur;
      cur = cur.parentElement;
    }
    throw new Error("no flush Card ancestor");
  }
  function firstCellSelector(card: Element): string {
    const token = Array.from(card.classList).find((c) => c.startsWith("[&_td:first-child"));
    if (!token) throw new Error("flush Card has no first-cell token");
    const inner = token.replace(/^\[/, "").replace(/\]:pl-5$/, "");
    return inner.replace(/^&/, ":scope").replace(/_/g, " ");
  }
  const td = (text: string) => {
    const cell = screen.getByText(text).closest("td");
    if (!cell) throw new Error(`no td for ${text}`);
    return cell;
  };

  function twoTokenUser(): AdminRateLimitUser {
    const base = row("multitok", ok(40, 30));
    const t = base.tokens[0];
    return {
      ...base,
      tokens: [
        { ...t, secret_id: "sec-a", label: "tok-alpha" },
        { ...t, secret_id: "sec-b", label: "tok-beta", is_default: false },
      ],
    };
  }

  it("matches first-row user cells and skips rowspan continuation cells", async () => {
    mockApi.getAdminRateLimits.mockResolvedValue({ users: [twoTokenUser()] });
    mockApi.getAdminCodexRateLimits.mockResolvedValue({
      users: [
        crow("multiacct", [
          cacct("cdx-a", ["acct-alpha"], true, "fresh", [cbucket("requests", "Requests", cwin(20, 18000, 5000))]),
          cacct("cdx-b", ["acct-beta"], false, "fresh", [cbucket("requests", "Requests", cwin(10, 18000, 5000))]),
        ]),
      ],
    });
    render(
      <MemoryRouter>
        <AdminRateLimits />
      </MemoryRouter>,
    );
    await screen.findByText("tok-beta");
    await screen.findByText("acct-beta");

    const cases = [
      { user: "multitok", first: "tok-alpha", cont: "tok-beta" },
      { user: "multiacct", first: "acct-alpha", cont: "acct-beta" },
    ];
    for (const c of cases) {
      const userCell = td(c.user);
      const card = flushCard(userCell);
      const matched = Array.from(card.querySelectorAll(firstCellSelector(card)));
      expect(matched).toContain(userCell);
      expect(matched).not.toContain(td(c.first));
      const cont = td(c.cont);
      expect(matched).not.toContain(cont);
      expect(cont.hasAttribute("data-flush-inner")).toBe(true);
    }
  });
});
