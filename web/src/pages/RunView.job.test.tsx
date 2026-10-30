// @vitest-environment jsdom
// PRD #1908 M7: the job gates in the RunView PAGE. A repo-less job run mounts the job result
// panel, has no wait-on-limit / MR-rework toggle row, reads "Runs / Job / Run" in the breadcrumb
// (no Board link) and fires no repo lookup. Each negative is paired with an issue-run
// positive control rendered by the same harness, so an absence here is never vacuous.
import { afterEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { api, ApiError, type Run } from "../lib/api";
import { mockJobRuns, mockRuns } from "../mocks/data";
import { setDemoMode } from "../lib/demoMode";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getRunReview: vi.fn().mockResolvedValue({ review: null, pending_judge: null }),
      rerunJudge: vi.fn(),
      pauseRun: vi.fn().mockResolvedValue({ run: null }),
      listSecrets: vi.fn().mockResolvedValue({ secrets: [] }),
      listRepos: vi.fn().mockResolvedValue({ repos: [] }),
      listFindings: vi.fn().mockResolvedValue({ findings: [] }),
      listWorkers: vi.fn().mockResolvedValue({ workers: [] }),
      getMySettings: vi.fn().mockResolvedValue({ settings: { mr_rework_enabled: null } }),
      setRunMrRework: vi.fn().mockResolvedValue({ run: null }),
      getRunArchives: vi.fn().mockResolvedValue({
        supported: false,
        legacy: true,
        has_open_hold: false,
        counts: { preparing: 0, uploading: 0, available: 0, needs_action: 0, expired: 0, discarded: 0 },
        archives: [],
      }),
    },
  };
});
vi.mock("../lib/useRunStream", () => ({ useRunStream: vi.fn() }));
const mockUseRunStream = vi.mocked(useRunStream);
const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  setDemoMode(false);
  vi.clearAllMocks();
  submit.mockReset().mockResolvedValue(undefined);
});

// Both running and owned, so the toggle row's own conditions (non-terminal status; canSteer
// with no MR yet) hold for either kind: only the kind gate can hide it.
const queuedJob: Run = mockJobRuns.find((r) => r.id === "run-job-queued")!;
const runningJob: Run = { ...queuedJob, status: "running", worker_id: "w-laptop" };
const runningIssue: Run = {
  ...mockRuns.find((r) => r.id === "run-queued")!,
  status: "running",
  harness: "claude",
  worker_id: "w-laptop",
  mr_iid: null,
  mr_state: null,
  repo_path: "vtmocanu/uzi",
};

const submit = vi.fn().mockResolvedValue(undefined);
async function renderPage(run: Run, canSteer = true, confirmedOwner = true) {
  mockUseRunStream.mockReturnValue({
    run,
    messages: [],
    connected: true,
    error: "",
    submit,
    refreshRun: vi.fn(),
    inputs: [],
    canSteer, confirmedOwner,
  } as unknown as ReturnType<typeof useRunStream>);
  const utils = render(
    <MemoryRouter initialEntries={[`/runs/${run.id}`]}>
      <RunView />
    </MemoryRouter>,
  );
  // Let the mount effects (repo lookup, settings, archives) settle.
  await act(async () => {
    await new Promise((r) => setTimeout(r, 0));
  });
  return utils;
}

const WAIT_LABEL = "Wait out future usage limits on this run";
const REWORK_LABEL = /Auto-rework this MR.s review comments/;
const breadcrumb = (container: HTMLElement) => container.querySelector("nav")!;

describe("RunView read-only capability sources", () => {
  it.each([
    ["running", false], ["completed", false], ["running", true], ["completed", true],
  ] as const)("shows sources for status=%s autopilot=%s outside the approval gate", async (status, auto_approve) => {
    const { container } = await renderPage({ ...runningIssue, status, auto_approve,
      required_capabilities: [], required_tools: [], capability_provenance: {
        hints_known: true, inference_known: true, capabilities: [{ capability: "jvm", sources: ["inferred"],
          required: false, inference_ignored: true,
          triggers: [{ path: "app/modules/x/android/build.gradle", reason: "jvm_manifest" }], truncated: false }],
      },
    });
    const sources = within(container).getByRole("group", { name: "Capability sources" });
    expect(within(sources).getByText("app/modules/x/android/build.gradle")).toBeTruthy();
    expect(within(sources).getByText(/inferred; inference ignored; not currently required/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Approve plan/ })).toBeNull();
  });

  it("hides read-only source paths in demo mode", async () => {
    setDemoMode(true);
    const { container } = await renderPage({ ...runningIssue, capability_provenance: {
      hints_known: true, inference_known: true, capabilities: [{ capability: "jvm", sources: ["inferred"],
        required: false, inference_ignored: true,
        triggers: [{ path: "private/customer/build.gradle", reason: "jvm_manifest" }], truncated: false }],
    } });
    expect(within(container).getByRole("group", { name: "Capability sources" })).toBeTruthy();
    expect(container.innerHTML).not.toContain("private/customer/build.gradle");
  });
});

describe("RunView job gates (PRD #1908)", () => {
  it("control: a running issue run names its repo in the breadcrumb and has the toggle row", async () => {
    const { container } = await renderPage(runningIssue);
    expect(screen.getByLabelText(WAIT_LABEL)).toBeTruthy();
    expect(screen.getByLabelText(REWORK_LABEL)).toBeTruthy();
    const repo = within(breadcrumb(container)).getByRole("link", { name: "uzi" });
    expect(repo.getAttribute("href")).toBe(`/repos/${runningIssue.repo_id}/board`);
    expect(repo.getAttribute("title")).toBe("vtmocanu/uzi");
    expect(screen.queryByRole("region", { name: /job$/ })).toBeNull();
    expect(mockApi.listRepos).toHaveBeenCalled();
  });

  it.each([undefined, ""])("falls back to Board for an unavailable repo path (%s)", async (repo_path) => {
    const { container } = await renderPage({ ...runningIssue, repo_path });
    expect(within(breadcrumb(container)).getByRole("link", { name: "Board" }).getAttribute("href"))
      .toBe(`/repos/${runningIssue.repo_id}/board`);
  });

  it("masks the repo breadcrumb tooltip in demo mode", async () => {
    setDemoMode(true);
    const { container } = await renderPage(runningIssue);
    const repo = within(breadcrumb(container)).getByRole("link", { name: "uzi" });
    expect(repo.getAttribute("title")).toBe("demo/uzi");
  });

  it("strips unsafe characters from the repo label and tooltip", async () => {
    const { container } = await renderPage({ ...runningIssue, repo_path: "team\u202e/uz\u001bi" });
    const repo = within(breadcrumb(container)).getByRole("link", { name: "uzi" });
    expect(repo.getAttribute("title")).toBe("team/uzi");
  });

  it("keeps No repository for a repo-less non-job run", async () => {
    const { container } = await renderPage({ ...runningJob, kind: "chat" });
    expect(within(breadcrumb(container)).getByText("No repository")).toBeTruthy();
  });

  it("a running job mounts the result panel and has no wait-on-limit / MR-rework row", async () => {
    await renderPage(runningJob);
    const panel = screen.getByRole("region", { name: "Research job" });
    expect(within(panel).getByText("incident-notes.md")).toBeTruthy();
    expect(within(panel).getByText("The report and findings appear here when the job finishes.")).toBeTruthy();
    expect(screen.queryByLabelText(WAIT_LABEL)).toBeNull();
    expect(screen.queryByLabelText(REWORK_LABEL)).toBeNull();
  });

  it("a job's breadcrumb reads Runs / Job / Run with no Board or issue link, and no repo lookup fires", async () => {
    const { container } = await renderPage(runningJob);
    const nav = breadcrumb(container);
    expect(nav.textContent?.replace(/\s+/g, "")).toBe("Runs/Job/Run");
    expect(within(nav).getAllByRole("link").map((a) => a.textContent)).toEqual(["Runs"]);
    expect(mockApi.listRepos).not.toHaveBeenCalled();
  });
});

// M1 retains the header stage for any run without a progress card.
describe("header stage fallback", () => {
  it.each(["job", "chat"] as const)("keeps the live stage for a %s run without a card", async kind => {
    const {container} = await renderPage({...runningJob, kind, milestones: null, progress: null, current_activity: null, usage: null, usage_estimated_tail: null});
    expect(container.querySelector("[data-run-progress-card]")).toBeNull();
    expect(screen.getByText("Starting up")).toBeTruthy();
  });
});


describe("M2 header and settings", () => {
  const settings = () => screen.getByRole("button", { name: "Run settings ▾" });
  const head = (container: HTMLElement) => container.querySelector<HTMLElement>(".run-head")!;
  it.each(["requested", "released", "unknown", null])("keeps one actual token and only a valid switch chip: %s", async credential_switch => {
    const { container } = await renderPage({ ...runningIssue, anthropic_secret_label: "Actual account",
      credential_override: { mode: "pinned", label: "Chosen account" }, credential_switch });
    const header = within(head(container));
    expect(header.getByText(/Actual account/)).toBeTruthy();
    if (credential_switch === "requested" || credential_switch === "released") {
      expect(header.getByText("token override: pinned “Chosen account”")).toBeTruthy();
      expect(header.getByText(credential_switch === "requested" ? "switch pending" : "switch released")).toBeTruthy();
    } else {
      expect(header.queryByText(/token override:/)).toBeNull();
      expect(header.queryByText(/^switch (pending|released)$/)).toBeNull();
    }
  });

  it("places the only lifecycle controls above the real composer, including pending-read optimism", async () => {
    const { container } = await renderPage(runningIssue, true, false);
    const stop = screen.getByRole("button", { name: "Stop run" });
    const pause = screen.getByRole("button", { name: /pause ▾/i });
    expect(screen.getAllByRole("button", { name: "Stop run" })).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: /pause ▾/i })).toHaveLength(1);
    expect(container.querySelector<HTMLElement>(".run-head")!.parentElement!.parentElement!.contains(stop)).toBe(true);
    expect(container.querySelector<HTMLElement>(".run-head")!.parentElement!.parentElement!.contains(pause)).toBe(true);
    const pointer = screen.getByText("Pause or stop this run from the header.");
    expect(pointer.parentElement!.contains(screen.getByRole("textbox"))).toBe(true);
    expect(within(pointer.parentElement!).queryByRole("button", { name: "Stop run" })).toBeNull();
    expect(within(pointer.parentElement!).queryByRole("button", { name: /pause ▾/i })).toBeNull();
    fireEvent.click(pause);
    await act(async () => fireEvent.click(screen.getByRole("menuitem", { name: /Pause now/ })));
    expect(api.pauseRun).toHaveBeenCalledWith(runningIssue.id, "now");
  });

  it.each([queuedJob, runningJob])("names the real $status job Stop exactly and retains its pointer without a composer", async (job) => {
    await renderPage(job);
    const stop = screen.getByRole("button", { name: "Stop this job" });
    expect(stop.textContent).toBe("Stop this job");
    expect(stop.getAttribute("aria-label")).toBe("Stop this job");
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("button", { name: "Stop run" })).toBeNull();
    expect(screen.getByText("A job takes no follow-ups. Stopping it cancels the job.")).toBeTruthy();
    expect(screen.getByText("Stop this job from the header.")).toBeTruthy();
    await act(async () => fireEvent.click(stop));
    expect(submit).toHaveBeenCalledWith("cancel", "", undefined, undefined, false);
  });

  it.each([[false, "running"], [true, "completed"]] as const)("gates real page controls: owner %s status %s", async (owner, status) => {
    await renderPage({ ...runningIssue, status }, owner);
    expect(screen.queryByRole("button", { name: "Stop run" })).toBeNull();
    expect(screen.queryByRole("button", { name: /pause ▾/i })).toBeNull();
  });

  it("header Stop keeps the existing outcome confirmation and busy gate", async () => {
    submit.mockRejectedValueOnce(new ApiError(409, "Confirm cancel", { reason: "outcome_pending_confirmation_required" }));
    await renderPage(runningIssue);
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Stop run" })));
    expect(screen.getByRole("dialog", { name: "Discard the held outcome?" })).toBeTruthy();
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Discard and cancel" })));
    expect(submit).toHaveBeenLastCalledWith("cancel", "", undefined, undefined, true);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("opens with Enter/Space and returns focus on Escape/trigger close; includes epoch one pin and time", async () => {
    const applied_at = "2026-10-01T12:00:00Z";
    await renderPage({ ...runningIssue, credential_epochs: [{ claim_generation: 1, label: "First account",
      secret_id: null, select_reason: "override_pinned", applied_at }, { claim_generation: 2, label: "Second account",
        secret_id: null, select_reason: "auto", applied_at }] });
    const trigger = settings();
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(document.getElementById("run-settings")!.classList.contains("hidden")).toBe(true);
    expect(screen.queryByRole("button", { name: "Switch token" })).toBeNull();
    fireEvent.keyDown(trigger, { key: "Enter" });
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByRole("button", { name: "Switch token" })).toBeTruthy();
    const history = screen.getByText("Token history").parentElement!;
    expect(within(history).getByText("First account")).toBeTruthy();
    expect(within(history).getByText("Second account")).toBeTruthy();
    expect(within(history).getAllByRole("listitem")).toHaveLength(2);
    expect(within(history).getByText("· pinned by override")).toBeTruthy();
    expect(history.textContent).toContain(new Date(applied_at).toLocaleString());
    const checkbox = screen.getByLabelText(WAIT_LABEL);
    checkbox.focus();
    fireEvent.keyDown(checkbox, { key: "Escape" });
    expect(document.activeElement).toBe(trigger);
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    fireEvent.keyDown(trigger, { key: " " });
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    checkbox.focus();
    fireEvent.click(trigger);
    expect(document.activeElement).toBe(trigger);
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
  });

  it("keeps a single recorded pin and its time in Run settings", async () => {
    const applied_at = "2026-10-01T12:00:00Z";
    await renderPage({ ...runningIssue, credential_epochs: [{ claim_generation: 1, label: "Only pin",
      secret_id: null, select_reason: "pinned", applied_at }] });
    fireEvent.click(settings());
    const history = screen.getByText("Token history").parentElement!;
    expect(within(history).getAllByRole("listitem")).toHaveLength(1);
    expect(within(history).getByText("Only pin")).toBeTruthy();
    expect(history.textContent).toContain(new Date(applied_at).toLocaleString());
  });

  it.each(["running", "limit_wait"] as const)("places wait setting correctly for %s; effective neutral chips", async status => {
    const { container } = await renderPage({ ...runningIssue, status, wait_on_limit: true, mr_rework_enabled: false });
    const panel = container.querySelector("#run-settings")!;
    expect(panel.contains(screen.getByLabelText(WAIT_LABEL))).toBe(status !== "limit_wait");
    expect(panel.contains(screen.getByLabelText(REWORK_LABEL))).toBe(true);
    expect(within(head(container)).getByText("waits on limits").className).toContain("text-neutral-fg");
    expect(within(head(container)).getByText("auto-rework off").className).toContain("text-neutral-fg");
  });

  it("omits default toggle chips and keeps stage in the card", async () => {
    const { container } = await renderPage({ ...runningIssue, wait_on_limit: false, mr_rework_enabled: null,
      milestones: [{ id: "m1", title: "First" }], milestones_completed: [] });
    const header = within(head(container));
    expect(header.queryByText("waits on limits")).toBeNull();
    expect(header.queryByText("auto-rework off")).toBeNull();
    expect(header.queryByText("Starting up")).toBeNull();
    expect(container.querySelector("[data-run-progress-card]")!.textContent).toContain("Starting up");
  });
});


it.each(["paused", "limit_wait", "pool_wait", "recovery_wait"] as const)("stops the header elapsed for %s and retains status/harness/model", async status => {
  const { container } = await renderPage({ ...runningIssue, status, started_at: "2026-10-01T10:00:00Z",
    status_since: "2026-10-01T10:01:00Z", budget_wall_seconds: null, budget_used_seconds: null,
    model: "test-model", recovery_wait_cause: "transient" });
  const header = headForElapsed(container);
  expect(header.textContent).toContain("clock stopped");
  // PRD #3244 rule 3: a parked (claimed) lead-lane run with no recorded rows keeps the lead chip,
  // and its tooltip still carries the create-time pinned model.
  const chip = within(header).getByText("lead: not recorded");
  expect(chip.getAttribute("title")).toContain("test-model: fixed by schedule");
  expect(within(header).getByTitle("Runs on Claude")).toBeTruthy();
  expect(header.textContent).toContain(status === "pool_wait" ? "waiting for pool" : status.replace(/_/g, " "));
  expect(within(header).queryByText("Starting up")).toBeNull();
});
function headForElapsed(container: HTMLElement) { return container.querySelector<HTMLElement>(".run-head")!; }

it("disables both real header controls while Stop is pending, then restores them", async () => {
  let finish!: () => void;
  submit.mockReturnValueOnce(new Promise<void>(resolve => { finish = resolve; }));
  await renderPage(runningIssue);
  const stop = screen.getByRole("button", { name: "Stop run" }) as HTMLButtonElement;
  const pause = screen.getByRole("button", { name: /pause ▾/i }) as HTMLButtonElement;
  fireEvent.click(stop);
  expect(stop.disabled).toBe(true);
  expect(pause.disabled).toBe(true);
  fireEvent.click(screen.getByRole("button", { name: "Run settings ▾" }));
  expect((screen.getByLabelText(WAIT_LABEL) as HTMLInputElement).disabled).toBe(true);
  expect((screen.getByLabelText(REWORK_LABEL) as HTMLInputElement).disabled).toBe(true);
  await act(async () => finish());
  expect(stop.disabled).toBe(false);
  expect(pause.disabled).toBe(false);
});


it("keeps settings children owner-gated", async () => {
  await renderPage(runningIssue, false);
  fireEvent.click(screen.getByRole("button", { name: "Run settings ▾" }));
  expect(screen.queryByRole("button", { name: "Switch token" })).toBeNull();
  expect(screen.getByText(/Only the run.s owner can switch its token/)).toBeTruthy();
  expect(screen.queryByLabelText(WAIT_LABEL)).toBeNull();
  expect(screen.queryByLabelText(REWORK_LABEL)).toBeNull();
});
