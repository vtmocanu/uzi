import "../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page } from "vitest/browser";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
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
