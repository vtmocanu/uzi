// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { AdminRateLimits } from "./AdminRateLimits";
import { api, type AdminRateLimitUser, type MyRateLimits } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { getAdminRateLimits: vi.fn() } };
});

const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
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
  it("renders every user, badges each state, and sorts danger-first", async () => {
    mockApi.getAdminRateLimits.mockResolvedValue({ users: USERS });
    render(<AdminRateLimits />);
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
    render(<AdminRateLimits />);
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
    render(<AdminRateLimits />);
    const nadia = (await screen.findByText("nadia")).closest("tr")!;
    expect(within(nadia).getByText("Recorded at usage limit")).toBeTruthy();
    // The updated line still renders alongside the disclosure.
    expect(within(nadia).getByText(/updated/)).toBeTruthy();

    const vlad = screen.getByText("vlad").closest("tr")!;
    expect(within(vlad).queryByText("Recorded at usage limit")).toBeNull();
  });

  it("renders a faint 'no name' placeholder for a user with an empty name (PRD #54)", async () => {
    mockApi.getAdminRateLimits.mockResolvedValue({ users: [row("", ok(8, 27))] });
    render(<AdminRateLimits />);
    await screen.findByText("no name");
    // The placeholder must be the first div's textContent (the sort test reads the
    // name via querySelector("div")), so the email below never floats an empty line.
    const nameCell = screen.getAllByRole("cell")[0];
    expect(nameCell.querySelector("div")!.textContent).toBe("no name");
    expect(within(nameCell).getByText("no name").className).toMatch(/text-faint/);
  });

  it("shows a dim 'stale' reset on the vault-locked row and dashes for no-reading rows", async () => {
    mockApi.getAdminRateLimits.mockResolvedValue({ users: USERS });
    render(<AdminRateLimits />);
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
    render(<AdminRateLimits />);
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
    render(<AdminRateLimits />);

    const ana = (await screen.findByText("ana")).closest("tr")!;
    const bar5h = within(ana).getByRole("progressbar", { name: "5-hour window" });
    expect(bar5h.getAttribute("aria-valuetext")).toMatch(/projected \d+% by reset, over$/);
    expect(within(ana).getByTestId("forecast-overflow-marker")).toBeTruthy();
  });

  it("stays a plain bar on a low reading with headroom", async () => {
    mockApi.getAdminRateLimits.mockResolvedValue({ users: [row("vlad", ok(20, 20))] });
    render(<AdminRateLimits />);

    const vlad = (await screen.findByText("vlad")).closest("tr")!;
    const bar5h = within(vlad).getByRole("progressbar", { name: "5-hour window" });
    expect(bar5h.getAttribute("aria-valuetext")).not.toMatch(/projected/);
    expect(within(vlad).queryByTestId("forecast-overflow-marker")).toBeNull();
  });

  it("draws no forecast on a stale row heading past the cap", async () => {
    mockApi.getAdminRateLimits.mockResolvedValue({ users: [row("mihai", ok(90, 20, { stale: true }))] });
    render(<AdminRateLimits />);

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
    render(<AdminRateLimits />);
    const ana = (await screen.findByText("ana")).closest("tr")!;
    expect(ana.textContent).toMatch(RESET_LABEL);
  });

  it("omits the label when the 7-day resets_at is null", async () => {
    mockApi.getAdminRateLimits.mockResolvedValue({
      users: [row("vlad", ok(20, 20, { seven_day: { pct: 20, resets_at: null } }))],
    });
    render(<AdminRateLimits />);
    const vlad = (await screen.findByText("vlad")).closest("tr")!;
    expect(vlad.textContent).not.toMatch(RESET_LABEL);
  });
});
