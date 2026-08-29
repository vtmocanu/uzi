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
import { api, ApiError, type Schedule } from "../lib/api";
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
      // The sweep branch mounts SweepLabelWarn, which checks labels against the repo.
      checkRepoLabels: vi.fn(),
      ensureRepoLabels: vi.fn(),
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
  vi.mocked(useAuth).mockReturnValue({ uziLabel: "uzi" } as unknown as ReturnType<typeof useAuth>);
  mockApi.previewSchedule.mockResolvedValue({ fires: [] });
  mockApi.listRepos.mockResolvedValue({ repos: [] });
  mockApi.checkRepoLabels.mockResolvedValue({ missing: [] });
  mockApi.ensureRepoLabels.mockResolvedValue({ ensured: [] });
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
    baked_guidance: null,
    model: "fable",
    override_subagent_model: false,
    enabled: true,
    status: "active",
    origin: "user",
    catalog_slug: null,
    customized: false,
    sibling_group_id: null,
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

describe("auto-approve is hidden for a self_improve schedule (PRD #590 follow-up 2)", () => {
  it("hides the Auto-approve toggle and drops the approve segment from the footer", () => {
    render(
      <MemoryRouter>
        <ScheduleModal
          editing={schedFixture({ target: "self_improve", origin: "user", auto_approve: true })}
          onClose={vi.fn()}
          onSaved={vi.fn()}
        />
      </MemoryRouter>,
    );

    // A self_improve run is always server-forced to auto_approve, so the toggle must not
    // render (showing it would misrepresent a fixed value as a user choice).
    expect(screen.queryByRole("switch", { name: "Auto-approve the plan" })).toBeNull();

    // The footer summary drops the approve segment entirely but still names the target.
    const footer = screen.getByText(/self-improve/);
    expect(footer.textContent).not.toMatch(/auto-approve/);
    expect(footer.textContent).not.toMatch(/manual approve/);
  });

  it("still renders the Auto-approve toggle for a non-self_improve schedule (control)", () => {
    render(
      <MemoryRouter>
        <ScheduleModal
          editing={schedFixture({ target: "sweep", auto_approve: true })}
          onClose={vi.fn()}
          onSaved={vi.fn()}
        />
      </MemoryRouter>,
    );

    expect(screen.getByRole("switch", { name: "Auto-approve the plan" })).toBeTruthy();
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

// ── PRD #589 M6: a catalog default renders read-only; a clone is editable ──────
describe("editing a catalog default (PRD #589)", () => {
  // A prompt-target default: its prompt is catalog-owned (baked), so the modal shows
  // it read-only with a Clone-to-edit affordance and no editable prompt textarea.
  function defaultFixture(over: Partial<Schedule> = {}): Schedule {
    return schedFixture({
      id: "sch-def-1",
      origin: "default",
      catalog_slug: "docs-hygiene",
      target: "prompt",
      prompt: "Audit the docs for dead links and stale references.",
      labels: null,
      guidance: null,
      model: "",
      customized: false,
      ...over,
    });
  }

  it("shows the DEFAULT chip and the locked, read-only baked prompt (not an editable textarea)", () => {
    render(
      <MemoryRouter>
        <ScheduleModal editing={defaultFixture()} onClose={vi.fn()} onSaved={vi.fn()} onCloneToEdit={vi.fn()} />
      </MemoryRouter>,
    );

    // The DEFAULT chip marks it as catalog-owned.
    expect(screen.getByText("DEFAULT")).toBeTruthy();
    // The baked prompt renders (read-only display)...
    expect(screen.getByText("Audit the docs for dead links and stale references.")).toBeTruthy();
    // ...and crucially NOT as the editable prompt textarea a user schedule shows. The
    // placeholder is unique to the editable control, so its absence is non-vacuous.
    expect(screen.queryByPlaceholderText("hunt for flaky tests and open an MR")).toBeNull();
    // The read-only label states it plainly.
    expect(screen.getByText(/Baked prompt/)).toBeTruthy();
  });

  it("offers Clone to edit, which hands the schedule to onCloneToEdit", () => {
    const onCloneToEdit = vi.fn();
    const fixture = defaultFixture();
    render(
      <MemoryRouter>
        <ScheduleModal editing={fixture} onClose={vi.fn()} onSaved={vi.fn()} onCloneToEdit={onCloneToEdit} />
      </MemoryRouter>,
    );
    fireEvent.click(screen.getByRole("button", { name: /Clone to edit/ }));
    expect(onCloneToEdit).toHaveBeenCalledWith(fixture);
  });

  it("saving a default sends only the editable fields — not the catalog-owned prompt/target/subagent flag", async () => {
    mockApi.updateSchedule.mockResolvedValue(defaultFixture());
    render(
      <MemoryRouter>
        <ScheduleModal editing={defaultFixture({ cron_expr: "0 3 * * 1" })} onClose={vi.fn()} onSaved={vi.fn()} onCloneToEdit={vi.fn()} />
      </MemoryRouter>,
    );
    // Change the cadence via the raw cron (Advanced), the one thing editable on a default.
    fireEvent.click(screen.getByText(/Advanced/));
    fireEvent.change(screen.getByLabelText("Cron expression"), { target: { value: "0 5 * * 1" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalled());
    const input = mockApi.updateSchedule.mock.calls[0]?.[1];
    // The edited cadence flows through...
    expect(input?.cron_expr).toBe("0 5 * * 1");
    // ...while the catalog-owned fields are omitted entirely (server rejects editing them).
    expect(input?.prompt).toBeUndefined();
    expect(input?.target).toBeUndefined();
    expect(input?.labels).toBeUndefined();
    // Guidance is the ONE editable exception on a PROMPT default (issue #662): it IS sent,
    // with replace-semantics — explicit null here since the fixture's guidance is empty
    // (a blank textarea clears owner guidance back to none). Not omitted like the rest.
    expect(input && "guidance" in input).toBe(true);
    expect(input?.guidance).toBeNull();
    expect(input?.override_subagent_model).toBeUndefined();
    // timing is catalog-owned too: the server keeps timing="recurring" itself and 400s ANY
    // default patch that carries it, so buildDefaultInput must NOT send it. This is the field
    // that actually broke the real backend — assert its key is absent, not merely its value.
    expect(input?.timing).toBeUndefined();
    expect(input && "timing" in input).toBe(false);
    // repo_id / issue_iid / run_at are catalog-owned on a default and never sent either.
    expect(input?.repo_id).toBeUndefined();
    expect(input?.issue_iid).toBeUndefined();
    expect(input?.run_at).toBeUndefined();
  });
});

// ── issue #662 M3: a prompt DEFAULT gets an editable owner-guidance field ───────
describe("owner guidance on a prompt default (issue #662)", () => {
  function promptDefault(over: Partial<Schedule> = {}): Schedule {
    return schedFixture({
      id: "sch-def-prompt",
      origin: "default",
      catalog_slug: "docs-hygiene",
      target: "prompt",
      prompt: "Audit the docs for dead links and stale references.",
      labels: null,
      guidance: null,
      model: "",
      customized: false,
      ...over,
    });
  }

  it("shows an EDITABLE guidance textarea alongside the read-only baked prompt", () => {
    render(
      <MemoryRouter>
        <ScheduleModal editing={promptDefault()} onClose={vi.fn()} onSaved={vi.fn()} onCloneToEdit={vi.fn()} />
      </MemoryRouter>,
    );
    // The baked prompt is still read-only (shown, but never in the editable prompt textarea)...
    expect(screen.getByText("Audit the docs for dead links and stale references.")).toBeTruthy();
    expect(screen.getByText(/Baked prompt/)).toBeTruthy();
    expect(screen.queryByPlaceholderText("hunt for flaky tests and open an MR")).toBeNull();
    // ...while guidance IS editable: the guidance textarea is present and writable.
    const guidance = screen.getByPlaceholderText("always add a failing test first") as HTMLTextAreaElement;
    expect(guidance.hasAttribute("readonly")).toBe(false);
    expect(guidance.hasAttribute("disabled")).toBe(false);
  });

  it("seeds the guidance textarea from the stored owner guidance", () => {
    render(
      <MemoryRouter>
        <ScheduleModal
          editing={promptDefault({ guidance: "prefer the smallest safe change" })}
          onClose={vi.fn()}
          onSaved={vi.fn()}
          onCloneToEdit={vi.fn()}
        />
      </MemoryRouter>,
    );
    const guidance = screen.getByPlaceholderText("always add a failing test first") as HTMLTextAreaElement;
    expect(guidance.value).toBe("prefer the smallest safe change");
  });

  it("submitting sends the edited guidance in the patch input", async () => {
    mockApi.updateSchedule.mockResolvedValue(promptDefault());
    render(
      <MemoryRouter>
        <ScheduleModal editing={promptDefault()} onClose={vi.fn()} onSaved={vi.fn()} onCloneToEdit={vi.fn()} />
      </MemoryRouter>,
    );
    fireEvent.change(screen.getByPlaceholderText("always add a failing test first"), {
      target: { value: "keep the diff tiny" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalled());
    const input = mockApi.updateSchedule.mock.calls[0]?.[1];
    expect(input?.guidance).toBe("keep the diff tiny");
    // The baked prompt itself is still catalog-owned and never sent.
    expect(input?.prompt).toBeUndefined();
    expect(input?.target).toBeUndefined();
  });

});

// ── issue #675 M2: a sweep DEFAULT gets an editable owner-guidance OVERLAY, shown
// ── alongside its read-only baked catalog guidance (the two are separate DTO fields) ──
describe("owner guidance overlay on a sweep default (issue #675)", () => {
  // The baked catalog guidance (read-only) and the owner overlay (editable) MUST be
  // DIFFERENT values here: with the old single-field shape both the read-only panel and
  // the editable textarea were fed the same `guidance`, so equal values would let this
  // test pass vacuously and hide that exact collision. Keeping them distinct is essential.
  const BAKED = "Triage the bug: reproduce, root-cause, and fix if small.";
  const OVERLAY = "prefer a failing test first, then the smallest fix";

  function sweepDefault(over: Partial<Schedule> = {}): Schedule {
    return schedFixture({
      id: "sch-def-sweep",
      origin: "default",
      catalog_slug: "bug-triage",
      target: "sweep",
      labels: ["bug"],
      prompt: "",
      baked_guidance: BAKED,
      guidance: OVERLAY,
      model: "",
      customized: true,
      ...over,
    });
  }

  it("shows the baked catalog guidance read-only (never the overlay) in the Baked guidance panel", () => {
    render(
      <MemoryRouter>
        <ScheduleModal editing={sweepDefault()} onClose={vi.fn()} onSaved={vi.fn()} onCloneToEdit={vi.fn()} />
      </MemoryRouter>,
    );
    expect(screen.getByText(/Baked guidance/)).toBeTruthy();
    // The read-only panel shows the BAKED text (as static text, not a textarea), never
    // the owner overlay — the collision the old single-field shape would have produced.
    const bakedEl = screen.getByText(BAKED);
    expect(bakedEl.tagName).not.toBe("TEXTAREA");
    // The overlay does appear on screen, but ONLY in the editable textarea (not the panel).
    const overlayEl = screen.getByText(OVERLAY);
    expect(overlayEl.tagName).toBe("TEXTAREA");
    expect(overlayEl).not.toBe(bakedEl);
  });

  it("shows an EDITABLE guidance textarea seeded from the owner overlay (not the baked value)", () => {
    render(
      <MemoryRouter>
        <ScheduleModal editing={sweepDefault()} onClose={vi.fn()} onSaved={vi.fn()} onCloneToEdit={vi.fn()} />
      </MemoryRouter>,
    );
    const guidance = screen.getByPlaceholderText("always add a failing test first") as HTMLTextAreaElement;
    expect(guidance.hasAttribute("readonly")).toBe(false);
    expect(guidance.hasAttribute("disabled")).toBe(false);
    // Seeded from the OVERLAY, never the baked catalog guidance.
    expect(guidance.value).toBe(OVERLAY);
    expect(guidance.value).not.toBe(BAKED);
  });

  it("submitting sends the edited overlay in the patch input, never the baked value", async () => {
    mockApi.updateSchedule.mockResolvedValue(sweepDefault());
    render(
      <MemoryRouter>
        <ScheduleModal editing={sweepDefault()} onClose={vi.fn()} onSaved={vi.fn()} onCloneToEdit={vi.fn()} />
      </MemoryRouter>,
    );
    fireEvent.change(screen.getByPlaceholderText("always add a failing test first"), {
      target: { value: "keep the change tiny and reversible" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalled());
    const input = mockApi.updateSchedule.mock.calls[0]?.[1];
    expect(input?.guidance).toBe("keep the change tiny and reversible");
    // The baked catalog guidance is never round-tripped through the overlay patch.
    expect(input?.guidance).not.toBe(BAKED);
    // Labels/target stay catalog-owned and are never sent.
    expect(input?.labels).toBeUndefined();
    expect(input?.target).toBeUndefined();
  });

  it("clearing the guidance textarea sends guidance:null (explicit clear), never baked_guidance", async () => {
    mockApi.updateSchedule.mockResolvedValue(sweepDefault({ guidance: null, customized: false }));
    render(
      <MemoryRouter>
        <ScheduleModal editing={sweepDefault()} onClose={vi.fn()} onSaved={vi.fn()} onCloneToEdit={vi.fn()} />
      </MemoryRouter>,
    );
    fireEvent.change(screen.getByPlaceholderText("always add a failing test first"), {
      target: { value: "" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalled());
    const input = mockApi.updateSchedule.mock.calls[0]?.[1];
    // A cleared overlay is sent as an EXPLICIT null, not undefined, so the server drops it.
    expect(input?.guidance).toBeNull();
    // The read-only baked catalog guidance is never round-tripped through the overlay patch.
    expect(input?.baked_guidance).toBeUndefined();
  });
});

describe("a cloned/user schedule keeps its prompt editable (PRD #589)", () => {
  it("renders the prompt in the editable textarea with no Clone-to-edit lock", () => {
    render(
      <MemoryRouter>
        <ScheduleModal
          editing={schedFixture({
            origin: "user",
            catalog_slug: null,
            target: "prompt",
            prompt: "cloned prompt, now editable",
            labels: null,
          })}
          onClose={vi.fn()}
          onSaved={vi.fn()}
          onCloneToEdit={vi.fn()}
        />
      </MemoryRouter>,
    );
    // The prompt is in the editable textarea, prefilled and writable...
    const textarea = screen.getByPlaceholderText("hunt for flaky tests and open an MR") as HTMLTextAreaElement;
    expect(textarea.value).toBe("cloned prompt, now editable");
    expect(textarea.hasAttribute("readonly")).toBe(false);
    expect(textarea.hasAttribute("disabled")).toBe(false);
    // ...and a user row is not catalog-owned, so there is no Clone-to-edit lock here.
    expect(screen.queryByRole("button", { name: /Clone to edit/ })).toBeNull();
    expect(screen.queryByText("DEFAULT")).toBeNull();
  });
});

// ── PRD #636 M2: multi-repo create fan-out ─────────────────────────────────────
describe("multi-repo create (PRD #636 M2)", () => {
  // Two repos so a create can fan out over both.
  function twoRepos() {
    return {
      repos: [
        { id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" },
        { id: "repo-other", path_with_namespace: "vtmocanu/other" },
      ] as unknown as Awaited<ReturnType<typeof api.listRepos>>["repos"],
    };
  }

  it("fans out 2 createSchedule calls carrying the SAME sibling_group_id", async () => {
    mockApi.listRepos.mockResolvedValue(twoRepos());
    mockApi.createSchedule.mockResolvedValue(schedFixture());
    renderModal();

    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    // The first repo is seeded; select the second so the fan-out spans both.
    fireEvent.click(screen.getByLabelText("vtmocanu/other"));
    // Default target = issue: give a valid number so the form submits.
    fireEvent.change(screen.getByLabelText("Issue number"), { target: { value: "142" } });

    const createBtn = screen.getByRole("button", { name: "Create schedule" });
    await waitFor(() => expect(createBtn.hasAttribute("disabled")).toBe(false));
    fireEvent.click(createBtn);

    await waitFor(() => expect(mockApi.createSchedule).toHaveBeenCalledTimes(2));
    const g0 = mockApi.createSchedule.mock.calls[0]?.[1]?.sibling_group_id;
    const g1 = mockApi.createSchedule.mock.calls[1]?.[1]?.sibling_group_id;
    // Both siblings share ONE client-generated id (assert cross-call equality, not a fixed
    // uuid), and it is a non-empty string — the group tag that renders them as one group.
    expect(typeof g0).toBe("string");
    expect(g0).not.toBe("");
    expect(g0).toBe(g1);
  });

  it("a single-repo create carries NO sibling_group_id (standalone)", async () => {
    mockApi.listRepos.mockResolvedValue(twoRepos());
    mockApi.createSchedule.mockResolvedValue(schedFixture());
    renderModal();

    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    // Leave the seeded single repo selected (N==1).
    fireEvent.change(screen.getByLabelText("Issue number"), { target: { value: "142" } });

    const createBtn = screen.getByRole("button", { name: "Create schedule" });
    await waitFor(() => expect(createBtn.hasAttribute("disabled")).toBe(false));
    fireEvent.click(createBtn);

    await waitFor(() => expect(mockApi.createSchedule).toHaveBeenCalledTimes(1));
    // A standalone create must not carry the group tag (→ NULL server-side).
    expect(mockApi.createSchedule.mock.calls[0]?.[1]?.sibling_group_id).toBeUndefined();
  });

  it("create mode renders RepoMultiSelect; edit mode renders the single repoint Select", async () => {
    // Create mode: the multi-select summary is present, the single "Repo" select is not.
    mockApi.listRepos.mockResolvedValue(twoRepos());
    const { unmount } = renderModal();
    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    expect(screen.queryByLabelText(/Repos — \d+ selected/)).toBeTruthy();
    // The per-checkbox controls of the multi-select are present (one per repo).
    expect(screen.getByLabelText("vtmocanu/other")).toBeTruthy();
    unmount();

    // Edit mode: the single repoint <Select> is present and the multi-select is absent.
    mockApi.listRepos.mockResolvedValue(twoRepos());
    render(
      <MemoryRouter>
        <ScheduleModal editing={schedFixture()} onClose={vi.fn()} onSaved={vi.fn()} />
      </MemoryRouter>,
    );
    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    expect(screen.getByLabelText("Repo")).toBeTruthy();
    expect(screen.queryByLabelText(/Repos — \d+ selected/)).toBeNull();
  });

  it("surfaces a 'created N of M, K failed' message on partial failure and keeps the modal open", async () => {
    mockApi.listRepos.mockResolvedValue(twoRepos());
    // First create lands, second rejects — Promise.allSettled keeps the landed row.
    mockApi.createSchedule
      .mockResolvedValueOnce(schedFixture())
      .mockRejectedValueOnce(new ApiError(500, "boom"));
    const onSaved = vi.fn();
    render(
      <MemoryRouter>
        <ScheduleModal pinned={undefined} onClose={vi.fn()} onSaved={onSaved} />
      </MemoryRouter>,
    );

    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    fireEvent.click(screen.getByLabelText("vtmocanu/other"));
    fireEvent.change(screen.getByLabelText("Issue number"), { target: { value: "142" } });

    const createBtn = screen.getByRole("button", { name: "Create schedule" });
    await waitFor(() => expect(createBtn.hasAttribute("disabled")).toBe(false));
    fireEvent.click(createBtn);

    await waitFor(() => expect(mockApi.createSchedule).toHaveBeenCalledTimes(2));
    // Partial failure does not roll back; the message names what landed and what failed.
    await waitFor(() => expect(screen.getByText(/Created 1 of 2 schedules; 1 failed\./)).toBeTruthy());
    // The modal stays open (no close/refresh) so the user can read the message.
    expect(onSaved).not.toHaveBeenCalled();
  });

  it("shows a per-repo sweep-warn when a selector label is missing on a selected repo", async () => {
    mockApi.listRepos.mockResolvedValue({
      repos: [{ id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" }] as unknown as Awaited<
        ReturnType<typeof api.listRepos>
      >["repos"],
    });
    // The selector label does not exist on the repo → advisory warn (never blocks create).
    mockApi.checkRepoLabels.mockResolvedValue({ missing: ["bug"] });
    renderModal();

    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    // Switch to the sweep target and add the missing selector label.
    fireEvent.click(screen.getByRole("radio", { name: /Label sweep/ }));
    fireEvent.change(screen.getByPlaceholderText("add label…"), { target: { value: "bug" } });
    fireEvent.keyDown(screen.getByPlaceholderText("add label…"), { key: "Enter" });

    // The per-repo SweepLabelWarn checks the label and warns it matches nothing.
    await waitFor(
      () => expect(screen.getByText(/doesn't exist on vtmocanu\/uzi/)).toBeTruthy(),
      { timeout: 2000 },
    );
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
