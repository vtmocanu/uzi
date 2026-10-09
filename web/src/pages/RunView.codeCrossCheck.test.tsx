// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { mockRuns } from "../mocks/data/runs";
vi.mock("../lib/useRunStream", () => ({ useRunStream: vi.fn() }));
vi.mock("../lib/api", async original => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api,
    getRunReview: vi.fn().mockResolvedValue({ review: null, pending_judge: null }),
    listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    listWorkers: vi.fn().mockResolvedValue({ workers: [] }),
    getMySettings: vi.fn().mockResolvedValue({ settings: {} }),
    getRunArchives: vi.fn().mockResolvedValue({ supported: true, legacy: false, has_open_hold: false,
      counts: { preparing: 0, uploading: 0, available: 0, needs_action: 0, expired: 0, discarded: 0 }, archives: [] }),
  } };
});
afterEach(cleanup);
it.each([true, false, undefined])("mounts code detail only with confirmed owner %s", confirmedOwner => {
  vi.mocked(useRunStream).mockReturnValue({
    run: { ...mockRuns[0], status: "queued", code_cross_check_required: true },
    messages: [], connected: true, error: "", submit: vi.fn(), refreshRun: vi.fn(),
    inputs: [], refreshInputs: vi.fn(), canSteer: true, confirmedOwner,
  } as ReturnType<typeof useRunStream>);
  render(<MemoryRouter initialEntries={["/runs/test"]}><Routes><Route path="/runs/:id" element={<RunView />} /></Routes></MemoryRouter>);
  expect(screen.queryByRole("region", { name: "Code cross-check" }) !== null).toBe(confirmedOwner === true);
});
