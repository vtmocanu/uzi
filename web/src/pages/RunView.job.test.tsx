// @vitest-environment jsdom
// PRD #1908 M7: the job gates in the RunView PAGE. A repo-less job run mounts the job result
// panel, has no wait-on-limit / MR-rework toggle row, reads "Runs / Job / Run" in the breadcrumb
// (no Board link) and fires no repo lookup. Each negative is paired with an issue-run
// positive control rendered by the same harness, so an absence here is never vacuous.
import { afterEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { api, type Run } from "../lib/api";
import { mockJobRuns, mockRuns } from "../mocks/data";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getRunReview: vi.fn().mockResolvedValue({ review: null, pending_judge: null }),
      rerunJudge: vi.fn(),
      listRepos: vi.fn().mockResolvedValue({ repos: [] }),
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
  vi.clearAllMocks();
});

// Both running and owned, so the toggle row's own conditions (non-terminal status; canSteer
// with no MR yet) hold for either kind: only the kind gate can hide it.
const runningJob: Run = { ...mockJobRuns.find((r) => r.id === "run-job-queued")!, status: "running", worker_id: "w-laptop" };
const runningIssue: Run = {
  ...mockRuns.find((r) => r.id === "run-queued")!,
  status: "running",
  worker_id: "w-laptop",
  mr_iid: null,
  mr_state: null,
};

async function renderPage(run: Run) {
  mockUseRunStream.mockReturnValue({
    run,
    messages: [],
    connected: true,
    error: "",
    submit: vi.fn(),
    refreshRun: vi.fn(),
    inputs: [],
    canSteer: true,
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

describe("RunView job gates (PRD #1908)", () => {
  it("control: a running issue run has the toggle row, a Board crumb, no job panel, and looks up its repo", async () => {
    const { container } = await renderPage(runningIssue);
    expect(screen.getByLabelText(WAIT_LABEL)).toBeTruthy();
    expect(screen.getByLabelText(REWORK_LABEL)).toBeTruthy();
    expect(within(breadcrumb(container)).getByRole("link", { name: "Board" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: /job$/ })).toBeNull();
    expect(mockApi.listRepos).toHaveBeenCalled();
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
