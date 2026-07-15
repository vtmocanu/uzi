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
function row(name: string, limits: MyRateLimits, vault_locked = false): AdminRateLimitUser {
  return { id: name, name, email: `${name}@example.com`, vault_locked, limits };
}

const USERS = [
  row("irina", { status: "no_token" }),
  row("vlad", ok(8, 47)),
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

  it("shows a dim 'stale' reset on the vault-locked row and dashes for no-reading rows", async () => {
    mockApi.getAdminRateLimits.mockResolvedValue({ users: USERS });
    render(<AdminRateLimits />);
    const mihai = (await screen.findByText("mihai")).closest("tr")!;
    expect(within(mihai).getAllByText("stale").length).toBe(2); // both windows read "stale"
    expect(within(mihai).getByText("31%")).toBeTruthy(); // pct still shown, just dimmed

    const irina = screen.getByText("irina").closest("tr")!;
    // no_token → em-dashes in both window cells and the Updated cell.
    expect(within(irina).getAllByText("—").length).toBe(3);
  });
});
