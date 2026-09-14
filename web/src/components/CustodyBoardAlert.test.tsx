// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { CustodyBoardAlert } from "./CustodyBoardAlert";
import { api, type RecoveryCustodyHolds } from "../lib/api";

// The alert fetches its own aggregate via api.getRecoveryHolds and refreshes on a visible
// poll. Mock only that call; keep custodyAlertView real so these pins exercise the real
// self-hide / escalation decision.
vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return { ...actual, api: { getRecoveryHolds: vi.fn() } };
});
const mockApi = vi.mocked(api);

beforeEach(() => {
  Object.defineProperty(document, "hidden", { configurable: true, get: () => false });
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.clearAllMocks();
});

function holds(over: Partial<RecoveryCustodyHolds["aggregate"]> = {}): RecoveryCustodyHolds {
  return {
    aggregate: { open_holds: 0, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0, ...over },
    holds: [],
  };
}

async function renderAlert(resp: RecoveryCustodyHolds, recoveryWaitCount = 0) {
  mockApi.getRecoveryHolds.mockResolvedValue(resp);
  const utils = render(
    <MemoryRouter>
      <CustodyBoardAlert recoveryWaitCount={recoveryWaitCount} />
    </MemoryRouter>,
  );
  await waitFor(() => expect(mockApi.getRecoveryHolds).toHaveBeenCalled());
  await act(async () => {
    await Promise.resolve();
  });
  return utils;
}

describe("CustodyBoardAlert", () => {
  it("self-hides when nothing needs action", async () => {
    const { container } = await renderAlert(holds({ open_holds: 3 }));
    expect(container.innerHTML).toBe("");
  });

  it("renders a warning (role=status) with the slot line and counts when a decision is needed", async () => {
    await renderAlert(holds({ open_holds: 4, decision_needed: 2, blocked_runs: 1 }), 5);
    const region = screen.getByRole("status");
    expect(region.getAttribute("aria-live")).toBe("polite");
    expect(screen.getByText(/Held work needs your attention/)).toBeTruthy();
    expect(screen.getByText("4 / 8 custody slots used")).toBeTruthy();
    expect(screen.getByText(/holds need a decision/)).toBeTruthy();
    expect(screen.getByText(/run blocked/)).toBeTruthy();
    // recovery_wait_count is surfaced for diagnosis.
    expect(screen.getByText(/runs waiting to recover/)).toBeTruthy();
  });

  it("escalates to an assertive alert at the admission limit and links to the resolution surface", async () => {
    await renderAlert(holds({ open_holds: 8, custody_hold_limit: 8, decision_needed: 1, blocked_runs: 2 }));
    const region = screen.getByRole("alert");
    expect(region.getAttribute("aria-live")).toBe("assertive");
    expect(screen.getByText(/Held work is blocking new runs/)).toBeTruthy();
    const link = screen.getByRole("link", { name: /Review held work/ });
    expect(link.getAttribute("href")).toContain("/workers");
    // a11y: the action is a SINGLE anchor styled as a button — never a <Link> wrapping a
    // <Button>, which is two tab stops and a doubled screen-reader announcement. One
    // focusable element, no nested button.
    expect(link.tagName).toBe("A");
    expect(link.querySelector("button")).toBeNull();
    expect(screen.queryByRole("button", { name: /Review held work/ })).toBeNull();
  });

  it("gates zero-valued counts: no '0 holds need a decision' / '0 runs blocked' line", async () => {
    // At the admission limit with no decisions and no blocked runs, the danger alert still
    // shows (atLimit), but neither count line renders — matching how recoveryWaitCount is
    // already gated > 0.
    await renderAlert(holds({ open_holds: 8, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0 }));
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText(/Held work is blocking new runs/)).toBeTruthy();
    expect(screen.queryByText(/needs? a decision/)).toBeNull();
    expect(screen.queryByText(/runs? blocked/)).toBeNull();
  });

  it("updates live: a poll that crosses the limit turns a hidden alert into a danger alert", async () => {
    vi.useFakeTimers();
    // First fetch: healthy (hidden). Later poll: at the limit (danger).
    mockApi.getRecoveryHolds
      .mockResolvedValueOnce(holds({ open_holds: 2 }))
      .mockResolvedValue(holds({ open_holds: 8, custody_hold_limit: 8, decision_needed: 1, blocked_runs: 2 }));
    render(
      <MemoryRouter>
        <CustodyBoardAlert recoveryWaitCount={0} />
      </MemoryRouter>,
    );
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.queryByRole("alert")).toBeNull();
    // Advance one poll interval; the second fetch now reports the limit.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10000);
    });
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText(/blocking new runs/)).toBeTruthy();
  });
});
