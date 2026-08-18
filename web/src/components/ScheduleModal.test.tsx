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
// The REAL mock (distinct from the vi.mock'd `api` above) — used to exercise the mock's
// own updateSchedule repoint branch directly. It ships seeded schedules/repos and starts
// signed in as admin, so no session setup is needed. ApiError survives the api vi.mock
// (importOriginal spreads the real one), so its 422/404 rejections match by `status`.
import { mockApi as realMockApi } from "../mocks/mockApi";

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
    last_fire: null,
    auto_approve: true,
    wait_on_limit: true,
    max_issues: 10,
    guidance: null,
    model: "fable",
    override_subagent_model: false,
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

describe('the "apply model also to agents" toggle (PRD #305)', () => {
  it("renders always enabled, including when Model is Inherit/empty", () => {
    renderModal();
    // A fresh create leaves Model on Inherit (empty). The toggle is first-class on
    // Inherit (Decision 3), so it must render and never be disabled.
    const model = screen.getByLabelText("Model (optional)") as HTMLSelectElement;
    expect(model.value).toBe("inherit");
    const toggle = screen.getByRole("switch", { name: "Apply model also to agents" });
    expect(toggle.hasAttribute("disabled")).toBe(false);
    expect(toggle.getAttribute("aria-checked")).toBe("false");
  });

  it("reflects the stored override_subagent_model when editing", () => {
    render(
      <MemoryRouter>
        <ScheduleModal
          editing={schedFixture({ override_subagent_model: true })}
          onClose={vi.fn()}
          onSaved={vi.fn()}
        />
      </MemoryRouter>,
    );
    const toggle = screen.getByRole("switch", { name: "Apply model also to agents" });
    expect(toggle.getAttribute("aria-checked")).toBe("true");
  });

  it("flows the flag into the submitted ScheduleInput", async () => {
    mockApi.updateSchedule.mockResolvedValue(schedFixture({ override_subagent_model: true }));
    render(
      <MemoryRouter>
        <ScheduleModal
          editing={schedFixture({ override_subagent_model: false })}
          onClose={vi.fn()}
          onSaved={vi.fn()}
        />
      </MemoryRouter>,
    );

    fireEvent.click(screen.getByRole("switch", { name: "Apply model also to agents" }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalled());
    expect(mockApi.updateSchedule.mock.calls[0]?.[1]?.override_subagent_model).toBe(true);
  });
});

describe("the create-only Enabled toggle (PRD #344 Feature B)", () => {
  it("renders the Enabled toggle in create mode", () => {
    renderModal();
    const toggle = screen.getByRole("switch", { name: "Enabled" });
    expect(toggle).toBeTruthy();
    // Default is on (a schedule is created active unless the user turns it off).
    expect(toggle.getAttribute("aria-checked")).toBe("true");
  });

  it("does NOT render the Enabled toggle in edit mode", () => {
    render(
      <MemoryRouter>
        <ScheduleModal editing={schedFixture()} onClose={vi.fn()} onSaved={vi.fn()} />
      </MemoryRouter>,
    );
    // Edit-mode enable/disable is the job of pause/resume, so the toggle is absent.
    expect(screen.queryByRole("switch", { name: "Enabled" })).toBeNull();
  });

  it("toggling it off and submitting a create sends enabled: false", async () => {
    mockApi.listRepos.mockResolvedValue({
      repos: [{ id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" }] as unknown as Awaited<
        ReturnType<typeof api.listRepos>
      >["repos"],
    });
    mockApi.createSchedule.mockResolvedValue(schedFixture({ enabled: false }));
    renderModal();

    // Wait for the repo picker to seed repoId from the loaded repos.
    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());

    // Default target = issue: give it a valid issue number so the form can submit.
    fireEvent.change(screen.getByLabelText("Issue number"), { target: { value: "142" } });

    // Turn Enabled off, then create.
    fireEvent.click(screen.getByRole("switch", { name: "Enabled" }));
    await waitFor(() =>
      expect(screen.getByRole("switch", { name: "Enabled" }).getAttribute("aria-checked")).toBe("false"),
    );

    const createBtn = screen.getByRole("button", { name: "Create schedule" });
    await waitFor(() => expect(createBtn.hasAttribute("disabled")).toBe(false));
    fireEvent.click(createBtn);

    await waitFor(() => expect(mockApi.createSchedule).toHaveBeenCalled());
    // createSchedule(repoId, input): the input is the second arg.
    expect(mockApi.createSchedule.mock.calls[0]?.[1]?.enabled).toBe(false);
  });

  it("leaves enabled default (true) in the created input when untouched", async () => {
    mockApi.listRepos.mockResolvedValue({
      repos: [{ id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" }] as unknown as Awaited<
        ReturnType<typeof api.listRepos>
      >["repos"],
    });
    mockApi.createSchedule.mockResolvedValue(schedFixture());
    renderModal();

    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    fireEvent.change(screen.getByLabelText("Issue number"), { target: { value: "142" } });

    const createBtn = screen.getByRole("button", { name: "Create schedule" });
    await waitFor(() => expect(createBtn.hasAttribute("disabled")).toBe(false));
    fireEvent.click(createBtn);

    await waitFor(() => expect(mockApi.createSchedule).toHaveBeenCalled());
    expect(mockApi.createSchedule.mock.calls[0]?.[1]?.enabled).toBe(true);
  });

  it("an EDIT submit never sends enabled (enable/disable stays pause/resume)", async () => {
    mockApi.updateSchedule.mockResolvedValue(schedFixture());
    render(
      <MemoryRouter>
        <ScheduleModal editing={schedFixture({ auto_approve: true })} onClose={vi.fn()} onSaved={vi.fn()} />
      </MemoryRouter>,
    );

    // Make a config change (flip a run option) so this is a real edit submit.
    fireEvent.click(screen.getByRole("switch", { name: "Auto-approve the plan" }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalled());
    // buildInput guards enabled to create-only, so an edit leaves it absent (undefined).
    expect(mockApi.updateSchedule.mock.calls[0]?.[1]?.enabled).toBeUndefined();
  });
});

describe("the edit-mode repo selector (PRD #344 Feature A)", () => {
  // Two repos so an edit can pick a different one than the schedule's current repo.
  function twoRepos() {
    return {
      repos: [
        { id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" },
        { id: "repo-other", path_with_namespace: "vtmocanu/other" },
      ] as unknown as Awaited<ReturnType<typeof api.listRepos>>["repos"],
    };
  }

  it("renders an enabled Repo select when editing a sweep-target schedule", async () => {
    mockApi.listRepos.mockResolvedValue(twoRepos());
    render(
      <MemoryRouter>
        <ScheduleModal editing={schedFixture()} onClose={vi.fn()} onSaved={vi.fn()} />
      </MemoryRouter>,
    );

    // Repos load in edit mode now, so the picker seeds with both options.
    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    const select = screen.getByLabelText("Repo") as HTMLSelectElement;
    expect(select).toBeTruthy();
    // A sweep schedule can be repointed, so the control is interactive.
    expect(select.hasAttribute("disabled")).toBe(false);
    // It prefills the schedule's current repo.
    await waitFor(() => expect(select.value).toBe("repo-uzi"));
  });

  it("disables the Repo select and shows a hint for an issue-target schedule", async () => {
    mockApi.listRepos.mockResolvedValue(twoRepos());
    render(
      <MemoryRouter>
        <ScheduleModal
          editing={schedFixture({ target: "issue", issue_iid: 142, labels: null })}
          onClose={vi.fn()}
          onSaved={vi.fn()}
        />
      </MemoryRouter>,
    );

    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    const select = screen.getByLabelText("Repo") as HTMLSelectElement;
    // An issue target is repo-relative and cannot be repointed (server 422).
    expect(select.hasAttribute("disabled")).toBe(true);
    expect(screen.getByText(/can't be repointed/i)).toBeTruthy();
  });

  it("flows a changed repo selection into repo_id on an edit submit", async () => {
    mockApi.listRepos.mockResolvedValue(twoRepos());
    mockApi.updateSchedule.mockResolvedValue(schedFixture({ repo_id: "repo-other", repo_path: "vtmocanu/other" }));
    render(
      <MemoryRouter>
        <ScheduleModal editing={schedFixture()} onClose={vi.fn()} onSaved={vi.fn()} />
      </MemoryRouter>,
    );

    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    const select = screen.getByLabelText("Repo") as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "repo-other" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalled());
    expect(mockApi.updateSchedule.mock.calls[0]?.[1]?.repo_id).toBe("repo-other");
  });

  it("does NOT send repo_id on create (the repo comes from the URL)", async () => {
    mockApi.listRepos.mockResolvedValue(twoRepos());
    mockApi.createSchedule.mockResolvedValue(schedFixture());
    renderModal();

    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    fireEvent.change(screen.getByLabelText("Issue number"), { target: { value: "142" } });

    const createBtn = screen.getByRole("button", { name: "Create schedule" });
    await waitFor(() => expect(createBtn.hasAttribute("disabled")).toBe(false));
    fireEvent.click(createBtn);

    await waitFor(() => expect(mockApi.createSchedule).toHaveBeenCalled());
    expect(mockApi.createSchedule.mock.calls[0]?.[1]?.repo_id).toBeUndefined();
  });

  it("does NOT send repo_id when editing an issue-target schedule (target-gate)", async () => {
    mockApi.listRepos.mockResolvedValue(twoRepos());
    mockApi.updateSchedule.mockResolvedValue(
      schedFixture({ target: "issue", issue_iid: 142, labels: null }),
    );
    render(
      <MemoryRouter>
        <ScheduleModal
          editing={schedFixture({ target: "issue", issue_iid: 142, labels: null })}
          onClose={vi.fn()}
          onSaved={vi.fn()}
        />
      </MemoryRouter>,
    );

    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    // An innocuous config change so the edit is a genuine submit.
    fireEvent.click(screen.getByRole("switch", { name: "Auto-approve the plan" }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalled());
    // buildInput gates repo_id to non-issue targets, so an issue-target edit never sends
    // it — closing the path where a user picks a new repo then switches target to issue
    // and provokes a server 422.
    expect(mockApi.updateSchedule.mock.calls[0]?.[1]?.repo_id).toBeUndefined();
  });
});

describe("the mock updateSchedule repoint branch (PRD #344 Feature A)", () => {
  // Exercises the REAL mock's repoint logic directly. The component tests above stub
  // updateSchedule via mockResolvedValue, so the mock's own 422/404/move branch is
  // otherwise uncovered. Seeded ids: sch-7kd2 (sweep, repo-uzi), sch-3bf1 (issue,
  // repo-uzi); repos repo-uzi (vtmocanu/uzi) and repo-atlas (vtmocanu/atlas-api).
  it("moves repo_path for a sweep schedule, and rejects issue-target (422) + unknown repo (404)", async () => {
    // An issue-target schedule is repo-relative and cannot be repointed.
    await expect(
      realMockApi.updateSchedule("sch-3bf1", {
        target: "issue",
        timing: "recurring",
        cron_expr: "0 3 * * *",
        repo_id: "repo-atlas",
      }),
    ).rejects.toMatchObject({ status: 422 });

    // An unknown/unowned repo on a repointable (sweep) schedule is a 404, checked before
    // the move so the schedule is left untouched.
    await expect(
      realMockApi.updateSchedule("sch-7kd2", {
        target: "sweep",
        timing: "recurring",
        cron_expr: "0 9 * * 1",
        repo_id: "repo-does-not-exist",
      }),
    ).rejects.toMatchObject({ status: 404 });

    // A valid repoint moves repo_id and refreshes repo_path (mirrors the server).
    const moved = await realMockApi.updateSchedule("sch-7kd2", {
      target: "sweep",
      timing: "recurring",
      cron_expr: "0 9 * * 1",
      repo_id: "repo-atlas",
    });
    expect(moved.repo_id).toBe("repo-atlas");
    expect(moved.repo_path).toBe("vtmocanu/atlas-api");
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
