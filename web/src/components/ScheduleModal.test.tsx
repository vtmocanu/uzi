// @vitest-environment jsdom
//
// ScheduleModal (PRD #241 M5): the two segmented pickers (Target, Timing) must swap
// the fields below them, and the "Next fires" preview must render from the (mocked)
// POST /api/schedules/preview endpoint — never a client-side cron guess — so it
// always matches server truth (Decision 6).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ScheduleModal } from "./ScheduleModal";
import { api } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listRepos: vi.fn().mockResolvedValue({ repos: [] }),
      previewSchedule: vi.fn(),
      createSchedule: vi.fn(),
      updateSchedule: vi.fn(),
      deleteSchedule: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

function renderModal() {
  return render(
    <MemoryRouter>
      <ScheduleModal
        pinned={undefined}
        onClose={vi.fn()}
        onSaved={vi.fn()}
      />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ prdLabel: "PRD" } as unknown as ReturnType<typeof useAuth>);
  mockApi.previewSchedule.mockResolvedValue({ fires: [] });
  mockApi.listRepos.mockResolvedValue({ repos: [] });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("target switching swaps the fields", () => {
  it("issue → sweep → prompt reveals the right control each time", async () => {
    renderModal();

    // Default target = issue: an issue-number field, no label/prompt controls.
    expect(screen.getByLabelText("Issue number")).toBeTruthy();
    expect(screen.queryByPlaceholderText("add label…")).toBeNull();

    // Sweep reveals the label multiselect (Decision 9).
    fireEvent.click(screen.getByRole("radio", { name: /Label sweep/ }));
    expect(screen.getByPlaceholderText("add label…")).toBeTruthy();
    expect(screen.queryByLabelText("Issue number")).toBeNull();
    // The PRD-label default hint is stated.
    expect(screen.getByText(/Empty ⇒/i)).toBeTruthy();

    // Prompt reveals the textarea (Decision 10) and drops the label control.
    fireEvent.click(screen.getByRole("radio", { name: /Prompt/ }));
    expect(screen.getByPlaceholderText("hunt for flaky tests and open an MR")).toBeTruthy();
    expect(screen.queryByPlaceholderText("add label…")).toBeNull();
  });
});

describe("timing switching swaps the cadence fields", () => {
  it("recurring shows the preset/cron; once shows the datetime picker", async () => {
    renderModal();

    // Recurring (default): the Cadence preset select is present.
    expect(screen.getByLabelText("Cadence")).toBeTruthy();
    expect(screen.queryByLabelText("Fire at")).toBeNull();

    fireEvent.click(screen.getByRole("radio", { name: /Once/ }));
    expect(screen.getByLabelText("Fire at")).toBeTruthy();
    expect(screen.queryByLabelText("Cadence")).toBeNull();
  });
});

describe("editing a raw cron flips the preset to Custom", () => {
  it("keeps the cron string as the source of truth", () => {
    renderModal();
    // Open Advanced and hand-edit the cron to something no preset produces.
    fireEvent.click(screen.getByText(/Advanced/));
    const cron = screen.getByLabelText("Cron expression") as HTMLInputElement;
    fireEvent.change(cron, { target: { value: "0 2 1 * *" } });
    const preset = screen.getByLabelText("Cadence") as HTMLSelectElement;
    expect(preset.value).toBe("custom");
  });
});

describe("the Next fires preview renders from the mocked endpoint", () => {
  it("shows the server-computed fires, not a client guess", async () => {
    const future = new Date(Date.now() + 2 * 86_400_000 + 3 * 3_600_000).toISOString();
    mockApi.previewSchedule.mockResolvedValue({ fires: [future] });

    renderModal();

    // The debounced effect calls the preview endpoint and renders its result.
    await waitFor(() => expect(mockApi.previewSchedule).toHaveBeenCalled(), { timeout: 2000 });
    await waitFor(() => expect(screen.getByText("in 2d 3h")).toBeTruthy(), { timeout: 2000 });

    // It asked for a recurring preview with the default weekdays cron (canonical form).
    const calls = mockApi.previewSchedule.mock.calls;
    const call = calls[calls.length - 1]?.[0];
    expect(call?.timing).toBe("recurring");
    expect(call?.cron_expr).toBe("0 2 * * 1-5");
  });
});
