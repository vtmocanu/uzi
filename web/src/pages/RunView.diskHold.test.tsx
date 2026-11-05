// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunView, RecoveryWaitPanel } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { api, ApiError, type Run } from "../lib/api";
import { mockRuns } from "../mocks/data/runs";
import { diskWaitMode } from "../lib/diskRecovery";
import { parkLabel, runBadge } from "../lib/runBadge";
import { StatusPill } from "../components/ui";

vi.mock("../lib/useRunStream", () => ({ useRunStream: vi.fn() }));
vi.mock("../lib/api", async (original) => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api,
    getRunReview: vi.fn().mockResolvedValue({ review: null, pending_judge: null }),
    listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    listWorkers: vi.fn().mockResolvedValue({ workers: [] }),
    getMySettings: vi.fn().mockResolvedValue({ settings: {} }),
    getRunArchives: vi.fn().mockResolvedValue({
      supported: false, legacy: true, has_open_hold: false,
      counts: { preparing: 0, uploading: 0, available: 0, needs_action: 0, expired: 0, discarded: 0 },
      archives: [],
    }),
    resumeRun: vi.fn().mockResolvedValue({}),
  } };
});
const refreshRun = vi.fn().mockResolvedValue(undefined);
const submit = vi.fn().mockResolvedValue(undefined);
function held(over: Partial<Run> = {}): Run {
  return { ...mockRuns[0], id: "disk", status: "recovery_wait",
    recovery_wait_cause: "data_volume_full", recovery_retry_not_before: null,
    disk_park_count: 4, outcome_pending: undefined, ...over };
}
function view(run: Run, proof?: boolean, canSteer = true) {
  vi.mocked(useRunStream).mockReturnValue({ run, messages: [], connected: true, error: "",
    submit, refreshRun, inputs: [], refreshInputs: vi.fn(), canSteer,
    confirmedOwner: proof } as unknown as ReturnType<typeof useRunStream>);
  return <MemoryRouter initialEntries={["/runs/disk"]}><Routes>
    <Route path="/runs/:id" element={<RunView />} />
  </Routes></MemoryRouter>;
}
beforeEach(() => {
  vi.clearAllMocks();
  refreshRun.mockReset().mockResolvedValue(undefined);
  vi.mocked(api.resumeRun).mockReset().mockResolvedValue({} as Awaited<ReturnType<typeof api.resumeRun>>);
});
afterEach(cleanup);

it.each([
  [null, "held", "Disk recovery needs your decision"],
  ["2099-01-01T00:00:00Z", "timed", "Waiting for disk space"],
  ["2020-01-01T00:00:00Z", "timed", "Waiting for disk space"],
  [undefined, "unknown", "Disk retry timing unknown"],
  ["", "unknown", "Disk retry timing unknown"],
  ["not-a-date", "unknown", "Disk retry timing unknown"],
] as const)("classifies timing %s consistently across disk readers", (timing, mode, label) => {
  const run = held({ recovery_retry_not_before: timing });
  expect(diskWaitMode(run)).toBe(mode);
  expect(parkLabel(run)).toBe(label);
  const badge = runBadge({ ...run, worker_name: null, owner_name: "Demo", is_mine: true, run_count: 1 }, 0);
  expect(badge).toMatchObject({ label, tone: "warning", pulse: false });
  const { container } = render(<>
    <StatusPill status={run.status} recovery_wait_cause={run.recovery_wait_cause} recovery_retry_not_before={timing} />
    <RecoveryWaitPanel run={run} ownerProof />
  </>);
  expect(screen.getAllByText(label)).toHaveLength(2);
  expect(container.textContent).toContain("Counted disk parks so far: 4.");
  if (mode === "held") {
    expect(screen.getByRole("button", { name: "Resume" })).toBeTruthy();
    expect(container.textContent).toContain("one bounded ordinary attempt");
    expect(container.textContent).toContain("Unpublished local work may be lost");
    expect(container.textContent).not.toContain("Retry at");
  } else {
    expect(screen.queryByRole("button", { name: "Resume" })).toBeNull();
    expect(container.textContent).toContain(mode === "timed" ? "retries automatically" : "neither an owner hold nor an automatic retry date");
  }
});
it("omitted timing and other causes do not prove a disk hold", () => {
  const run = held();
  delete run.recovery_retry_not_before;
  expect(diskWaitMode(run)).toBe("unknown");
  expect(diskWaitMode(held({ status: "running" }))).toBeNull();
  expect(diskWaitMode(held({ recovery_wait_cause: null }))).toBeNull();
});
it.each([[undefined, true], [false, true], [true, false]] as const)(
  "requires confirmed owner %s and steering %s for every destructive control", (proof, steering) => {
    render(view(held(), proof, steering));
    expect(screen.queryByRole("button", { name: /^Resume$/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^(Cancel|Stop)/ })).toBeNull();
    expect(api.resumeRun).not.toHaveBeenCalled();
    expect(submit).not.toHaveBeenCalled();
  });
it("invokes Resume once without arguments beyond the ID and stays busy through refetch", async () => {
  let finishRequest!: (value: Awaited<ReturnType<typeof api.resumeRun>>) => void;
  let finishRefresh!: () => void;
  vi.mocked(api.resumeRun).mockImplementationOnce(() => new Promise(resolve => { finishRequest = resolve; }));
  refreshRun.mockImplementationOnce(() => new Promise<void>(resolve => { finishRefresh = resolve; }));
  render(view(held(), true));
  const resume = screen.getByRole("button", { name: "Resume" }) as HTMLButtonElement;
  const cancel = screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement;
  act(() => { resume.click(); resume.click(); cancel.click(); });
  expect(vi.mocked(api.resumeRun).mock.calls).toEqual([["disk"]]);
  expect(submit).not.toHaveBeenCalled();
  expect(resume.disabled).toBe(true);
  expect(cancel.disabled).toBe(true);
  await act(async () => { finishRequest({} as Awaited<ReturnType<typeof api.resumeRun>>); });
  expect(refreshRun).toHaveBeenCalledOnce();
  expect(resume.disabled).toBe(true);
  await act(async () => { finishRefresh(); });
  expect(resume.disabled).toBe(false);
  expect(screen.getByText(/Counted disk parks so far: 4/)).toBeTruthy();
  fireEvent.click(cancel);
  await waitFor(() => expect(submit).toHaveBeenCalledWith("cancel", "", undefined, undefined, false));
});
it("does not overlap Resume with an in-flight Cancel", async () => {
  let finishCancel!: () => void;
  submit.mockImplementationOnce(() => new Promise<void>(resolve => { finishCancel = resolve; }));
  render(view(held(), true));
  const resume = screen.getByRole("button", { name: "Resume" }) as HTMLButtonElement;
  const cancel = screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement;
  act(() => { cancel.click(); resume.click(); cancel.click(); });
  expect(submit).toHaveBeenCalledOnce();
  expect(api.resumeRun).not.toHaveBeenCalled();
  expect(resume.disabled).toBe(true);
  expect(cancel.disabled).toBe(true);
  await act(async () => { finishCancel(); });
  expect(cancel.disabled).toBe(false);
});
it("updates actions when owner proof changes without sending a request", () => {
  const { rerender } = render(view(held()));
  expect(screen.queryByRole("button", { name: "Resume" })).toBeNull();
  rerender(view(held(), true));
  expect(screen.getByRole("button", { name: "Resume" })).toBeTruthy();
  rerender(view(held(), false));
  expect(screen.queryByRole("button", { name: "Resume" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Cancel" })).toBeNull();
  expect(api.resumeRun).not.toHaveBeenCalled();
  expect(submit).not.toHaveBeenCalled();
});
it.each([409, 500])("surfaces %s without retrying or polling for capacity", async status => {
  vi.mocked(api.resumeRun).mockRejectedValueOnce(new ApiError(status, "Disk admission refused"));
  render(view(held(), true));
  fireEvent.click(screen.getByRole("button", { name: "Resume" }));
  await waitFor(() => expect(screen.getByText("Disk admission refused")).toBeTruthy());
  expect(vi.mocked(api.resumeRun).mock.calls).toEqual([["disk"]]);
  expect(refreshRun).not.toHaveBeenCalled();
  expect((screen.getByRole("button", { name: "Resume" }) as HTMLButtonElement).disabled).toBe(false);
  expect(screen.getByText(/Counted disk parks so far: 4/)).toBeTruthy();
});
it("does not retry a failed refetch", async () => {
  refreshRun.mockRejectedValueOnce(new Error("Refresh failed"));
  render(view(held(), true));
  fireEvent.click(screen.getByRole("button", { name: "Resume" }));
  await waitFor(() => expect(screen.getByText("Refresh failed")).toBeTruthy());
  expect(vi.mocked(api.resumeRun).mock.calls).toEqual([["disk"]]);
  expect(refreshRun).toHaveBeenCalledOnce();
});
it("announces timed to held once and keeps counters stable", async () => {
  const { container, rerender } = render(view(held({ recovery_retry_not_before: "2020-01-01T00:00:00Z" }), true));
  const notice = container.querySelector('div.sr-only[role="status"]')!;
  await waitFor(() => expect(notice.textContent).toContain("retries automatically"));
  rerender(view(held(), true));
  await waitFor(() => expect(notice.textContent).toContain("Automatic disk recovery has stopped"));
  const announcement = notice.textContent;
  rerender(view(held({ disk_park_count: 5 }), true));
  expect(notice.textContent).toBe(announcement);
  expect(screen.getByText(/Counted disk parks so far: 5/)).toBeTruthy();
  expect(api.resumeRun).not.toHaveBeenCalled();
});
