import "../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page } from "vitest/browser";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import type { Run } from "../lib/api";
import { mockRuns } from "../mocks/data";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getRunReview: vi.fn().mockResolvedValue({ review: null, pending_judge: null }),
      rerunJudge: vi.fn(),
      resumeRun: actual.api.resumeRun,
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


afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

const diskIssue: Run = {
  ...mockRuns.find((run) => run.id === "run-queued")!,
  status: "recovery_wait",
  recovery_wait_cause: "data_volume_full",
  recovery_retry_not_before: null,
  disk_park_count: 4,
  outcome_pending: undefined,
};
function diskView(proof?: boolean, refreshRun = vi.fn(), submit = vi.fn().mockResolvedValue(undefined)) {
  mockUseRunStream.mockReturnValue({
    run: diskIssue, messages: [], connected: true, error: "",
    submit, refreshRun, inputs: [], canSteer: true, confirmedOwner: proof,
  } as unknown as ReturnType<typeof useRunStream>);
  return <MemoryRouter initialEntries={[`/runs/${diskIssue.id}`]}>
    <div className="p-4"><Routes><Route path="/runs/:id" element={<RunView />} /></Routes></div>
  </MemoryRouter>;
}

it.each([320, 390, 768])("keeps disk owner actions visible at %ipx and posts once through refetch", async width => {
  await page.viewport(width, 1000);
  let respond!: (response: Response) => void;
  let refreshed!: () => void;
  // Intercept the real binding before rendering. Unexpected requests fail locally;
  // none can reach the live Vite API proxy.
  const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    if (String(input) !== `/api/runs/${diskIssue.id}/resume-now` || init?.method !== "POST") {
      return Promise.reject(new Error(`Unexpected network request: ${String(input)}`));
    }
    return new Promise<Response>(resolve => { respond = resolve; });
  });
  vi.stubGlobal("fetch", fetchMock);
  const refreshRun = vi.fn(() => new Promise<void>(resolve => { refreshed = resolve; }));
  const submit = vi.fn().mockResolvedValue(undefined);
  render(diskView(true, refreshRun, submit));
  await act(async () => { await Promise.resolve(); });
  await document.fonts.ready;
  const resume = screen.getByRole("button", { name: "Resume" }) as HTMLButtonElement;
  const cancel = screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement;
  const panel = resume.closest(".rounded-xl")!;
  const bounds = panel.getBoundingClientRect();
  expect(bounds.width).toBeGreaterThan(0);
  expect(bounds.left).toBeGreaterThanOrEqual(0);
  expect(bounds.right).toBeLessThanOrEqual(width);
  expect(panel.scrollWidth).toBeLessThanOrEqual(panel.clientWidth);
  for (const button of [resume, cancel]) {
    const rect = button.getBoundingClientRect();
    expect(rect.width).toBeGreaterThan(0);
    expect(rect.height).toBeGreaterThan(0);
    expect(rect.left).toBeGreaterThanOrEqual(bounds.left);
    expect(rect.right).toBeLessThanOrEqual(bounds.right);
  }
  const resumeBounds = resume.getBoundingClientRect();
  const cancelBounds = cancel.getBoundingClientRect();
  expect(resumeBounds.right <= cancelBounds.left || resumeBounds.bottom <= cancelBounds.top).toBe(true);
  await page.getByRole("button", { name: "Resume", exact: true }).click();
  expect(resume.disabled).toBe(true);
  expect(cancel.disabled).toBe(true);
  // Even programmatically queued clicks cannot race the in-flight request.
  act(() => { resume.click(); cancel.click(); });
  expect(fetchMock).toHaveBeenCalledOnce();
  const [url, request] = fetchMock.mock.calls[0];
  expect(String(url)).toBe(`/api/runs/${diskIssue.id}/resume-now`);
  expect(request?.method).toBe("POST");
  expect(request?.body).toBeUndefined();
  expect(submit).not.toHaveBeenCalled();
  await act(async () => { respond(new Response(JSON.stringify({ run: diskIssue }), {
    status: 200, headers: { "Content-Type": "application/json" },
  })); });
  await waitFor(() => expect(refreshRun).toHaveBeenCalledOnce());
  expect(resume.disabled).toBe(true);
  expect(cancel.disabled).toBe(true);
  await act(async () => { refreshed(); });
  expect(resume.disabled).toBe(false);
  expect(screen.getByText(/Counted disk parks so far: 4/)).toBeTruthy();
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await waitFor(() => expect(submit).toHaveBeenCalledWith("cancel", "", undefined, undefined, false));
  expect(fetchMock).toHaveBeenCalledOnce();
});

it.each([undefined, false])("keeps unconfirmed/nonowner disk holds inert at 320px (proof %s)", async proof => {
  await page.viewport(320, 1000);
  const fetchMock = vi.fn().mockRejectedValue(new Error("Unexpected network request"));
  vi.stubGlobal("fetch", fetchMock);
  const submit = vi.fn();
  render(diskView(proof, vi.fn(), submit));
  await act(async () => { await Promise.resolve(); });
  expect(screen.getAllByText("Disk recovery needs your decision").length).toBeGreaterThan(0);
  expect(screen.queryByRole("button", { name: /^Resume$/ })).toBeNull();
  expect(screen.queryByRole("button", { name: /^(Cancel|Stop)/ })).toBeNull();
  const notice = screen.getByText("Only the confirmed run owner can Resume or Cancel.");
  const bounds = notice.getBoundingClientRect();
  expect(bounds.width).toBeGreaterThan(0);
  expect(bounds.left).toBeGreaterThanOrEqual(0);
  expect(bounds.right).toBeLessThanOrEqual(320);
  expect(fetchMock).not.toHaveBeenCalled();
  expect(submit).not.toHaveBeenCalled();
});

const runningIssue: Run = {
  ...mockRuns.find((run) => run.id === "run-queued")!,
  status: "running",
  harness: "claude",
  worker_id: "w-laptop",
  mr_iid: null,
  mr_state: null,
};

it.each([320, 390, 768, 1024, 1280])("keeps production RunView disclosures within the viewport at %ipx", async width => {
  await page.viewport(width, 1000);
  mockUseRunStream.mockReturnValue({
    run: runningIssue,
    messages: [],
    connected: true,
    error: "",
    submit: vi.fn().mockResolvedValue(undefined),
    refreshRun: vi.fn(),
    inputs: [],
    canSteer: true,
    confirmedOwner: true,
  } as unknown as ReturnType<typeof useRunStream>);
  render(
    <MemoryRouter initialEntries={[`/runs/${runningIssue.id}`]}>
      <div className="p-4"><Routes><Route path="/runs/:id" element={<RunView />} /></Routes></div>
    </MemoryRouter>,
  );
  await act(async () => { await Promise.resolve(); });
  await document.fonts.ready;

  const pause = screen.getByRole("button", { name: /pause ▾/i });
  expect(screen.getAllByRole("button", { name: "Stop run" })).toHaveLength(1);
  fireEvent.click(pause);
  const menu = screen.getByRole("menu");
  expect(screen.getByRole("menuitem", { name: /Pause now/ })).toBeTruthy();
  const pauseBounds = menu.getBoundingClientRect();
  console.info(JSON.stringify({ width, pause: { left: pauseBounds.left, right: pauseBounds.right } }));
  expect(pauseBounds.width).toBeGreaterThan(0);
  expect(pauseBounds.left).toBeGreaterThanOrEqual(0);
  expect(pauseBounds.right).toBeLessThanOrEqual(width);
  fireEvent.click(pause);

  const settings = screen.getByRole("button", { name: "Run settings ▾" });
  fireEvent.click(settings);
  const panel = document.getElementById("run-settings")!;
  expect(settings.getAttribute("aria-expanded")).toBe("true");
  expect(screen.getByRole("button", { name: "Switch token" })).toBeTruthy();
  const settingsBounds = panel.getBoundingClientRect();
  console.info(JSON.stringify({ width, settings: { left: settingsBounds.left, right: settingsBounds.right } }));
  expect(settingsBounds.width).toBeGreaterThan(0);
  expect(settingsBounds.left).toBeGreaterThanOrEqual(0);
  expect(settingsBounds.right).toBeLessThanOrEqual(width);
  fireEvent.click(screen.getByRole("button", { name: "Switch token" }));
  const picker = panel.querySelector<HTMLElement>(".w-72")!;
  const pickerBounds = picker.getBoundingClientRect();
  expect(pickerBounds.width).toBeGreaterThan(0);
  expect(pickerBounds.left).toBeGreaterThanOrEqual(settingsBounds.left);
  expect(pickerBounds.right).toBeLessThanOrEqual(settingsBounds.right);
  for (const control of picker.querySelectorAll("button, select, input")) {
    const bounds = control.getBoundingClientRect();
    expect(bounds.left).toBeGreaterThanOrEqual(pickerBounds.left);
    expect(bounds.right).toBeLessThanOrEqual(pickerBounds.right);
  }
  fireEvent.keyDown(settings, { key: "Escape" });
  expect(document.activeElement).toBe(settings);
  expect(settings.getAttribute("aria-expanded")).toBe("false");
});
