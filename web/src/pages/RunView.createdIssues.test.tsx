// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { mockRuns } from "../mocks/data/runs";
import type { Run } from "../lib/apiTypes";

vi.mock("../lib/useRunStream", () => ({ useRunStream: vi.fn() }));
vi.mock("../lib/api", async original => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api,
    getRunReview: vi.fn().mockResolvedValue({ review: null, pending_judge: null }),
    listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    listWorkers: vi.fn().mockResolvedValue({ workers: [] }),
    listSecrets: vi.fn().mockResolvedValue({ secrets: [] }),
    getMySettings: vi.fn().mockResolvedValue({ settings: {} }),
    getRunArchives: vi.fn().mockResolvedValue({
      supported: true, legacy: false, has_open_hold: false,
      counts: { preparing: 0, uploading: 0, available: 0, needs_action: 0, expired: 0, discarded: 0 }, archives: [],
    }),
  } };
});
afterEach(cleanup);

it.each(["running", "completed"] as const)("shows outputs on a %s prompt without a source issue or MR", async status => {
  const run: Run = {
    ...mockRuns[0], id: "prompt-output", kind: "prompt", status,
    issue_iid: null, issue_title: "Scheduled idea", issue_web_url: null,
    mr_iid: null, mr_state: null, branch: null,
    created_issues: [{ iid: 51, title: "Proposed work", web_url: "https://github.com/org/repo/issues/51" }],
  };
  vi.mocked(useRunStream).mockReturnValue({
    run, messages: [], connected: true, error: "", submit: vi.fn(),
    refreshRun: vi.fn().mockResolvedValue(undefined), inputs: [], refreshInputs: vi.fn(),
    canSteer: false, confirmedOwner: false,
  } as unknown as ReturnType<typeof useRunStream>);
  render(<MemoryRouter initialEntries={["/runs/prompt-output"]}>
    <Routes><Route path="/runs/:id" element={<RunView />} /></Routes>
  </MemoryRouter>);
  await act(async () => {});
  expect(screen.getByRole("link", { name: "Created issue #51" }).getAttribute("href"))
    .toBe("https://github.com/org/repo/issues/51");
  expect(screen.queryByRole("link", { name: /Open issue #/ })).toBeNull();
});

it("shows created issues independently of source issue, status and MR/branch displays", async () => {
  const run: Run = {
    ...mockRuns[0], id: "created-issues", kind: "issue", status: "completed",
    issue_iid: 12, issue_title: "Original source issue",
    issue_web_url: "https://github.com/org/repo/issues/12", forge_type: "github",
    branch: "agent/source-12", mr_iid: 99, mr_state: "opened",
    outcome_pending: undefined, hold_reason: null,
    created_issues: [
      { iid: 41, title: "First follow-up", web_url: "https://github.com/org/repo/issues/41" },
      { iid: 42, title: "Second follow-up", web_url: "" },
    ],
  };
  vi.mocked(useRunStream).mockReturnValue({
    run, messages: [], connected: true, error: "", submit: vi.fn(),
    refreshRun: vi.fn().mockResolvedValue(undefined), inputs: [], refreshInputs: vi.fn(),
    canSteer: false, confirmedOwner: false,
  } as unknown as ReturnType<typeof useRunStream>);
  render(<MemoryRouter initialEntries={["/runs/created-issues"]}>
    <Routes><Route path="/runs/:id" element={<RunView />} /></Routes>
  </MemoryRouter>);
  await act(async () => {});
  expect(screen.getByRole("heading", { name: "Original source issue" })).toBeTruthy();
  expect(screen.getByRole("link", { name: "Open issue #12 on GitHub" }).getAttribute("href"))
    .toBe("https://github.com/org/repo/issues/12");
  const block = screen.getByRole("group", { name: "Created issues" });
  expect(within(block).getByRole("link", { name: "Created issue #41" }).getAttribute("href"))
    .toBe("https://github.com/org/repo/issues/41");
  expect(within(block).getByText("Created issue #42").tagName).toBe("SPAN");
  expect(within(block).getByText("First follow-up")).toBeTruthy();
  expect(within(block).getByText("Second follow-up")).toBeTruthy();
  expect(screen.getAllByText("agent/source-12").length).toBeGreaterThan(0);
  expect(screen.getByTitle("Open the pull request on GitHub").textContent).toContain("#99");
  expect(screen.getByText("Run completed")).toBeTruthy();
});
