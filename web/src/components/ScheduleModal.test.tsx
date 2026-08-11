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
import { api, type Schedule } from "../lib/api";
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

describe("keyboard: Escape closes the dialog", () => {
  it("calls the close handler (same as the × button) on Escape", () => {
    const onClose = vi.fn();
    render(
      <MemoryRouter>
        <ScheduleModal pinned={undefined} onClose={onClose} onSaved={vi.fn()} />
      </MemoryRouter>,
    );
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});

// A recurring-sweep schedule fixture, submittable as-is (PRD #300 model round-trip).
function schedFixture(over: Partial<Schedule> = {}): Schedule {
  return {
    id: "sch-1",
    repo_id: "repo-uzi",
    repo_path: "vtmocanu/uzi",
    target: "sweep",
    issue_iid: null,
    labels: ["bug"],
    prompt: "",
    timing: "recurring",
    cron_expr: "0 2 * * 1-5",
    run_at: null,
    timezone: "UTC",
    next_fire_at: null,
    last_fired_at: null,
    auto_approve: true,
    wait_on_limit: true,
    max_issues: 10,
    guidance: null,
    model: "fable",
    enabled: true,
    status: "active",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    next_fires: [],
    ...over,
  };
}

describe("the per-schedule model control (PRD #300)", () => {
  it("shows the frozen model when editing and flows a change into the submitted input", async () => {
    mockApi.updateSchedule.mockResolvedValue(schedFixture());
    render(
      <MemoryRouter>
        <ScheduleModal editing={schedFixture({ model: "fable" })} onClose={vi.fn()} onSaved={vi.fn()} />
      </MemoryRouter>,
    );

    // The stored model prefills the shared ModelSelect (shown for every target).
    const select = screen.getByLabelText("Model (optional)") as HTMLSelectElement;
    expect(select.value).toBe("fable");

    // Changing it to another alias flows into ScheduleInput.model on save.
    fireEvent.change(select, { target: { value: "opus" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalled());
    expect(mockApi.updateSchedule.mock.calls[0]?.[1]?.model).toBe("opus");
  });

  it("clearing the control to Inherit sends explicit null (clear-to-inherit)", async () => {
    mockApi.updateSchedule.mockResolvedValue(schedFixture({ model: null }));
    render(
      <MemoryRouter>
        <ScheduleModal editing={schedFixture({ model: "fable" })} onClose={vi.fn()} onSaved={vi.fn()} />
      </MemoryRouter>,
    );

    const select = screen.getByLabelText("Model (optional)") as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "inherit" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalled());
    expect(mockApi.updateSchedule.mock.calls[0]?.[1]?.model).toBeNull();
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
