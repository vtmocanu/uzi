// @vitest-environment jsdom
//
// ScheduleModal's ci_watch target (PRD #2865): the target is offered, the fields that
// do not apply are hidden, auto-approve starts OFF for ci_watch only (D10) with the
// explanatory note, and the request carries ci_workflows and no issue fields.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ScheduleModal } from "./ScheduleModal";
import { api, type Schedule } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { browserTimezone } from "../lib/timezone";
import { mockApi as realMockApi } from "../mocks/mockApi";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listScheduleCatalog: vi.fn(),
      listRepos: vi.fn(),
      previewSchedule: vi.fn(),
      createSchedule: vi.fn(),
      updateSchedule: vi.fn(),
      deleteSchedule: vi.fn(),
      checkRepoLabels: vi.fn(),
      ensureRepoLabels: vi.fn(),
      listSecrets: vi.fn(),
      getMySettings: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));
vi.mock("../lib/timezone", () => ({ browserTimezone: vi.fn() }));

const mockApi = vi.mocked(api);

const NOTE =
  "Off by default: this job reads CI logs, which can contain text from dependencies, tests or outside contributors. Review the plan before the run changes code. You can turn it on.";

beforeEach(async () => {
  vi.mocked(browserTimezone).mockReturnValue("UTC");
  vi.mocked(useAuth).mockReturnValue({ uziLabel: "uzi" } as unknown as ReturnType<typeof useAuth>);
  mockApi.listScheduleCatalog.mockResolvedValue(await realMockApi.listScheduleCatalog());
  mockApi.previewSchedule.mockResolvedValue({ fires: [] });
  mockApi.listRepos.mockResolvedValue({
    repos: [{ id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" }] as unknown as Awaited<
      ReturnType<typeof api.listRepos>
    >["repos"],
  });
  mockApi.checkRepoLabels.mockResolvedValue({ missing: [] });
  mockApi.ensureRepoLabels.mockResolvedValue({ ensured: [] });
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.getMySettings.mockResolvedValue({
    settings: { default_harness: null } as unknown as Awaited<ReturnType<typeof api.getMySettings>>["settings"],
  });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderCreate() {
  return render(
    <MemoryRouter>
      <ScheduleModal onClose={vi.fn()} onSaved={vi.fn()} />
    </MemoryRouter>,
  );
}

const approveSwitch = () => screen.getByRole("switch", { name: "Auto-approve the plan" });
const pickTarget = (name: RegExp) => fireEvent.click(screen.getByRole("radio", { name }));

describe("ScheduleModal — ci_watch target (PRD #2865)", () => {
  it("offers the CI repair target and swaps in the workflow field, hiding issue-only fields", async () => {
    renderCreate();
    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    // The issue target (the create default) shows its issue field first.
    expect(screen.getByLabelText("Issue number")).toBeTruthy();

    pickTarget(/CI repair/);

    expect(screen.getByLabelText("Workflows to watch (optional)")).toBeTruthy();
    expect(screen.getByLabelText("Prompt (optional)")).toBeTruthy();
    expect(screen.getByText(/Leave blank to watch all workflows in the listed window/)).toBeTruthy();
    // Fields the server rejects on ci_watch are not offered.
    expect(screen.queryByLabelText("Issue number")).toBeNull();
    expect(screen.queryByLabelText("Issues to send at a time")).toBeNull();
    expect(screen.queryByLabelText("Output mode (optional)")).toBeNull();
    expect(screen.queryByLabelText("Guidance (optional)")).toBeNull();
    expect(screen.queryByPlaceholderText("add label…")).toBeNull();
  });

  it("shows the approval note for ci_watch and not for another target", async () => {
    renderCreate();
    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    expect(screen.queryByText(NOTE)).toBeNull();
    pickTarget(/CI repair/);
    expect(screen.getByText(NOTE)).toBeTruthy();
    pickTarget(/Label sweep/);
    expect(screen.queryByText(NOTE)).toBeNull();
  });

  it("defaults auto-approve OFF for ci_watch and ON for every other target", async () => {
    renderCreate();
    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    expect(approveSwitch().getAttribute("aria-checked")).toBe("true");
    pickTarget(/CI repair/);
    expect(approveSwitch().getAttribute("aria-checked")).toBe("false");
    pickTarget(/Label sweep/);
    expect(approveSwitch().getAttribute("aria-checked")).toBe("true");
    pickTarget(/^Prompt/);
    expect(approveSwitch().getAttribute("aria-checked")).toBe("true");
  });

  it("keeps an explicit auto-approve choice when the target changes afterwards", async () => {
    renderCreate();
    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    pickTarget(/CI repair/);
    fireEvent.click(approveSwitch());
    expect(approveSwitch().getAttribute("aria-checked")).toBe("true");
    pickTarget(/Label sweep/);
    pickTarget(/CI repair/);
    expect(approveSwitch().getAttribute("aria-checked")).toBe("true");
  });

  it("sends ci_workflows, a manual-approve flag and no issue fields", async () => {
    mockApi.createSchedule.mockResolvedValue({} as Schedule);
    renderCreate();
    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    // Fill the issue field first so a leak of the issue target's state would show.
    fireEvent.change(screen.getByLabelText("Issue number"), { target: { value: "142" } });
    pickTarget(/CI repair/);
    fireEvent.change(screen.getByLabelText("Workflows to watch (optional)"), {
      target: { value: " ci.yml \n\nKinD smoke\n" },
    });

    const create = screen.getByRole("button", { name: "Create schedule" });
    await waitFor(() => expect(create.hasAttribute("disabled")).toBe(false));
    fireEvent.click(create);
    await waitFor(() => expect(mockApi.createSchedule).toHaveBeenCalledTimes(1));

    const [repoId, input] = mockApi.createSchedule.mock.calls[0]!;
    expect(repoId).toBe("repo-uzi");
    expect(input.target).toBe("ci_watch");
    expect(input.ci_workflows).toEqual(["ci.yml", "KinD smoke"]);
    expect(input.auto_approve).toBe(false);
    // A blank prompt is allowed: the server fills in the CI repair instructions.
    expect(input.prompt).toBe("");
    for (const key of ["issue_iid", "labels", "max_issues", "output_mode", "guidance"] as const) {
      expect(input[key], key).toBeUndefined();
    }
  });

  it("sends an empty workflow list when the field is blank (all workflows)", async () => {
    mockApi.createSchedule.mockResolvedValue({} as Schedule);
    renderCreate();
    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    pickTarget(/CI repair/);
    const create = screen.getByRole("button", { name: "Create schedule" });
    await waitFor(() => expect(create.hasAttribute("disabled")).toBe(false));
    fireEvent.click(create);
    await waitFor(() => expect(mockApi.createSchedule).toHaveBeenCalledTimes(1));
    expect(mockApi.createSchedule.mock.calls[0]![1].ci_workflows).toEqual([]);
  });

  it("blocks submit on a duplicate workflow and says why", async () => {
    renderCreate();
    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    pickTarget(/CI repair/);
    fireEvent.change(screen.getByLabelText("Workflows to watch (optional)"), {
      target: { value: "ci.yml\nci.yml" },
    });
    expect(screen.getByText("Each workflow can be listed only once.")).toBeTruthy();
    expect(screen.getByLabelText("Workflows to watch (optional)").getAttribute("aria-invalid")).toBe("true");
    expect(screen.getByRole("button", { name: "Create schedule" }).hasAttribute("disabled")).toBe(true);
  });

  it("keeps a sweep create on auto-approve with no ci_workflows key (control)", async () => {
    mockApi.createSchedule.mockResolvedValue({} as Schedule);
    renderCreate();
    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());
    pickTarget(/Label sweep/);
    const create = screen.getByRole("button", { name: "Create schedule" });
    await waitFor(() => expect(create.hasAttribute("disabled")).toBe(false));
    fireEvent.click(create);
    await waitFor(() => expect(mockApi.createSchedule).toHaveBeenCalledTimes(1));
    const input = mockApi.createSchedule.mock.calls[0]![1];
    expect(input.auto_approve).toBe(true);
    expect("ci_workflows" in input && input.ci_workflows !== undefined).toBe(false);
  });

  it("turns auto-approve off when an edit converts an auto-approved schedule into ci_watch", async () => {
    const editing = await realMockApi.getSchedule("sch-pr0m");
    expect(editing.auto_approve).toBe(true);
    render(
      <MemoryRouter>
        <ScheduleModal editing={editing} onClose={vi.fn()} onSaved={vi.fn()} />
      </MemoryRouter>,
    );
    expect(approveSwitch().getAttribute("aria-checked")).toBe("true");
    pickTarget(/CI repair/);
    expect(approveSwitch().getAttribute("aria-checked")).toBe("false");
    // Converting back restores the stored setting.
    pickTarget(/^Prompt/);
    expect(approveSwitch().getAttribute("aria-checked")).toBe("true");
  });

  it("edits a CI repair default: workflows and guidance editable, catalog fields omitted", async () => {
    const editing = { ...(await realMockApi.getSchedule("sch-def-ci-uzi")), ci_workflows: ["ci.yml", "kind-smoke.yml"] };
    mockApi.updateSchedule.mockResolvedValue(editing);
    render(
      <MemoryRouter>
        <ScheduleModal editing={editing} onClose={vi.fn()} onSaved={vi.fn()} />
      </MemoryRouter>,
    );
    expect(screen.getByText("Default-branch CI repair")).toBeTruthy();
    expect(screen.getByText(NOTE)).toBeTruthy();
    // The stored value wins on edit (the seeded default is manual approve).
    expect(approveSwitch().getAttribute("aria-checked")).toBe("false");
    const field = screen.getByLabelText("Workflows to watch (optional)") as HTMLTextAreaElement;
    expect(field.value).toBe("ci.yml\nkind-smoke.yml");
    fireEvent.change(field, { target: { value: "ci.yml" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalledTimes(1));
    const [id, input] = mockApi.updateSchedule.mock.calls[0]!;
    expect(id).toBe("sch-def-ci-uzi");
    expect(input.ci_workflows).toEqual(["ci.yml"]);
    for (const key of ["target", "prompt", "labels", "timing", "issue_iid"] as const) {
      expect(input[key], key).toBeUndefined();
    }
    // The payload is one the mock (mirroring the server's default-patch lock) accepts.
    await expect(realMockApi.updateSchedule(id, input)).resolves.toMatchObject({ ci_workflows: ["ci.yml"] });
  });
});
