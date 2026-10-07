// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { CustodyBoardAlert } from "./CustodyBoardAlert";
import { api, type RecoveryCustodyHolds } from "../lib/api";

// Mock the aggregate fetch only: rendering uses the real custodyAlertView rules.
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
  it.each([8, 9])("renders capacity-only holds at %i as a polite warning without action", async (open_holds) => {
    await renderAlert(holds({ open_holds }));
    const region = screen.getByRole("status");
    expect(region.getAttribute("aria-live")).toBe("polite");
    expect(region.classList.contains("bg-warn/10")).toBe(true);
    expect(region.classList.contains("bg-danger/10")).toBe(false);
    expect(screen.getByText("Custody capacity reached").classList.contains("text-warn")).toBe(true);
    expect(screen.getByText(`${open_holds} / 8 custody slots used`)).toBeTruthy();
    expect(screen.getByText("Custody capacity is reached. New code runs will wait; no action is needed. Admission resumes when counted holds settle.")).toBeTruthy();
    expect(screen.queryByRole("link", { name: /Review held work/ })).toBeNull();
    expect(screen.queryByText(/needs? a decision/)).toBeNull();
    expect(screen.queryByText(/runs? blocked/)).toBeNull();
    expect(screen.queryByText(/Review the holds that need a decision/)).toBeNull();
    expect(screen.queryByText(/Queued code runs are waiting/)).toBeNull();
  });

  it.each([
    { open_holds: 3 },
    { open_holds: 8, custody_hold_limit: 0 },
    { open_holds: 8, custody_hold_limit: -1 },
    { open_holds: 0, decision_needed: 1, blocked_runs: 1 },
  ])("self-hides with aggregate %j despite recovery wait diagnostics", async (aggregate) => {
    const { container } = await renderAlert(holds(aggregate), 5);
    expect(container.innerHTML).toBe("");
  });

  it.each([4, 8, 9])("renders decision-only holds at %i as a polite warning with targeted advice", async (open_holds) => {
    await renderAlert(holds({ open_holds, decision_needed: 2 }), 5);
    const region = screen.getByRole("status");
    expect(region.getAttribute("aria-live")).toBe("polite");
    expect(region.classList.contains("bg-warn/10")).toBe(true);
    expect(region.classList.contains("bg-danger/10")).toBe(false);
    expect(screen.getByText("Held work needs your attention").classList.contains("text-warn")).toBe(true);
    expect(screen.getByText("Review the holds that need a decision and choose how to preserve their work.")).toBeTruthy();
    expect(screen.getByText(/holds need a decision/)).toBeTruthy();
    expect(screen.getByText(/runs waiting to recover/)).toBeTruthy();
    expect(screen.queryByText(/runs? blocked/)).toBeNull();
    expect(screen.queryByText(/Queued code runs are waiting/)).toBeNull();
    expect(screen.queryByText(/New code runs will wait/)).toBeNull();
    const link = screen.getByRole("link", { name: /Review held work/ });
    expect(link.getAttribute("href")).toBe("/workers?tab=workers#recovery-holds");
    expect(link.classList.contains("bg-raised")).toBe(true);
  });

  it.each([4, 8, 9])("renders blocked runs without decisions at %i as danger with queued-wait advice", async (open_holds) => {
    await renderAlert(holds({ open_holds, blocked_runs: 2 }));
    const region = screen.getByRole("alert");
    expect(region.getAttribute("aria-live")).toBe("assertive");
    expect(region.classList.contains("bg-danger/10")).toBe(true);
    expect(region.classList.contains("bg-warn/10")).toBe(false);
    expect(screen.getByText("Held work is blocking new runs").classList.contains("text-danger")).toBe(true);
    expect(screen.getByText("Queued code runs are waiting for custody admission; no action is needed. Admission resumes when counted holds settle.")).toBeTruthy();
    expect(screen.getByText(/runs blocked/)).toBeTruthy();
    expect(screen.queryByText(/needs? a decision/)).toBeNull();
    expect(screen.queryByRole("link", { name: /Review held work/ })).toBeNull();
    expect(screen.queryByText(/Review the holds that need a decision/)).toBeNull();
  });

  it.each([4, 8])("prioritizes blocked runs over decisions at %i and links to targeted review", async (open_holds) => {
    await renderAlert(holds({ open_holds, decision_needed: 1, blocked_runs: 2 }));
    const region = screen.getByRole("alert");
    expect(region.getAttribute("aria-live")).toBe("assertive");
    expect(region.classList.contains("bg-danger/10")).toBe(true);
    expect(screen.getByText("Held work is blocking new runs")).toBeTruthy();
    expect(screen.getByText("Queued code runs are waiting for custody admission. Review the holds that need a decision and choose how to preserve their work.")).toBeTruthy();
    expect(screen.getByText(/hold needs a decision/)).toBeTruthy();
    expect(screen.getByText(/runs blocked/)).toBeTruthy();
    const link = screen.getByRole("link", { name: /Review held work/ });
    expect(link.getAttribute("href")).toBe("/workers?tab=workers#recovery-holds");
    expect(link.classList.contains("bg-brand")).toBe(true);
    // A single anchor, with no nested button or second tab stop.
    expect(link.tagName).toBe("A");
    expect(link.querySelector("button")).toBeNull();
    expect(screen.queryByRole("button", { name: /Review held work/ })).toBeNull();
  });

  it("updates through hidden, capacity, blocked, decision-only, and hidden polling states", async () => {
    vi.useFakeTimers();
    mockApi.getRecoveryHolds
      .mockResolvedValueOnce(holds({ open_holds: 2 }))
      .mockResolvedValueOnce(holds({ open_holds: 8 }))
      .mockResolvedValueOnce(holds({ open_holds: 4, blocked_runs: 2 }))
      .mockResolvedValueOnce(holds({ open_holds: 8, decision_needed: 1 }))
      .mockResolvedValue(holds({ open_holds: 3 }));
    const { container } = render(
      <MemoryRouter>
        <CustodyBoardAlert recoveryWaitCount={0} />
      </MemoryRouter>,
    );
    await act(async () => { await Promise.resolve(); });
    expect(container.innerHTML).toBe("");

    await act(async () => { await vi.advanceTimersByTimeAsync(10000); });
    expect(screen.getByRole("status").getAttribute("aria-live")).toBe("polite");
    expect(screen.getByText("Custody capacity reached")).toBeTruthy();
    expect(screen.queryByRole("link", { name: /Review held work/ })).toBeNull();

    await act(async () => { await vi.advanceTimersByTimeAsync(10000); });
    expect(screen.getByRole("alert").getAttribute("aria-live")).toBe("assertive");
    expect(screen.getByText("Held work is blocking new runs")).toBeTruthy();
    expect(screen.queryByRole("status")).toBeNull();

    await act(async () => { await vi.advanceTimersByTimeAsync(10000); });
    expect(screen.getByRole("status").getAttribute("aria-live")).toBe("polite");
    expect(screen.getByText("Held work needs your attention")).toBeTruthy();
    expect(screen.getByRole("link", { name: /Review held work/ })).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText(/Queued code runs are waiting/)).toBeNull();

    await act(async () => { await vi.advanceTimersByTimeAsync(10000); });
    expect(container.innerHTML).toBe("");
    expect(mockApi.getRecoveryHolds).toHaveBeenCalledTimes(5);
  });
});
