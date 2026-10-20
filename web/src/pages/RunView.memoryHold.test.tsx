// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { api, type Run } from "../lib/api";
import { mockRuns } from "../mocks/data/runs";
import { effectiveRunStatus, needsHumanAttention, runBadge } from "../lib/runBadge";
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
      supported: true, legacy: false, has_open_hold: false,
      counts: { preparing: 0, uploading: 0, available: 0, needs_action: 0, expired: 0, discarded: 0 },
      archives: [],
    }),
    resumeRun: vi.fn().mockResolvedValue({}),
  } };
});
const refreshRun = vi.fn().mockResolvedValue(undefined);
const submit = vi.fn().mockResolvedValue(undefined);
function held(over: Partial<Run> = {}): Run {
  return { ...mockRuns[0], id: "held", status: "recovery_wait",
    recovery_wait_cause: "worker_memory_pressure", outcome_pending: undefined,
    memory_hold: { episode: 4, used: 2, max: 2, remaining: 0 },
    requeue_count: 99, checkpoint_contains_latest: true,
    recovery_retry_not_before: "2020-01-01T00:00:00Z",
    worker_recovery: { episode: 8, automatic_requeue_limit: 17, episode_used: 16,
      episode_remaining: 1, evidence: { checkpoint_tip: "abc123", available_capture: true,
        publication_uncertain: false, capture_uncertain: false, custody_uncertain: true,
        unknown: false, recorded_at: "2026-10-07T04:00:00Z" } },
    ...over };
}
function view(run: Run, proof?: boolean, canSteer = true) {
  vi.mocked(useRunStream).mockReturnValue({ run, messages: [], connected: true, error: "",
    submit, refreshRun, inputs: [], refreshInputs: vi.fn(), canSteer,
    confirmedOwner: proof } as ReturnType<typeof useRunStream>);
  return <MemoryRouter initialEntries={["/runs/held"]}><Routes>
    <Route path="/runs/:id" element={<RunView />} />
  </Routes></MemoryRouter>;
}
beforeEach(() => { vi.clearAllMocks(); });
afterEach(cleanup);

it("renders memory allowance separately and keeps recorded source evidence historical", () => {
  render(view(held(), true));
  expect(screen.getByText("Worker memory pressure — waiting for owner")).toBeTruthy();
  expect(screen.getByText("Memory interventions: used 2. Maximum: 2. Remaining: 0.")).toBeTruthy();
  expect(screen.getByText("Memory episode: 4.")).toBeTruthy();
  expect(screen.getByText(/^Memory interventions have stopped\. Resume starts a fresh memory episode/)).toBeTruthy();
  expect(screen.getByText(/Current availability and the latest local edits are not verified/)).toBeTruthy();
  expect(screen.getByText(/It may later expire or be discarded/)).toBeTruthy();
  expect(screen.queryByText("The published checkpoint contains the latest work.")).toBeNull();
  expect(screen.queryByText(/Automatic recovery limit/)).toBeNull();
  expect(screen.queryByText(/Recovering and resuming automatically|Retry at/)).toBeNull();
  expect(api.resumeRun).not.toHaveBeenCalled();
  expect(submit).not.toHaveBeenCalled();
});
it("keeps zero as a known bound and source-only custody distinct from an archive", () => {
  render(view(held({
    memory_hold: { episode: 4, used: 0, max: 0, remaining: 0 },
    worker_recovery: undefined,
  }), true));
  expect(screen.getByText("Memory interventions: used 0. Maximum: 0. Remaining: 0.")).toBeTruthy();
  expect(screen.getByText(/Retained source may remain on the assigned worker/)).toBeTruthy();
  expect(screen.getByText(/Only a verified available recovery archive can restore captured work/)).toBeTruthy();
  expect(screen.queryByText(/A recovery capture was recorded as available/)).toBeNull();
  expect(api.resumeRun).not.toHaveBeenCalled();
});
it.each([undefined, { episode: 4, used: 2, max: null, remaining: null }])(
  "keeps unknown allowance unknown without a requeue fallback", (memory_hold) => {
    render(view(held({ memory_hold }), true));
    expect(screen.getByText(memory_hold
      ? "Memory interventions: used 2. Maximum: unknown. Remaining: unknown."
      : "Memory interventions: used unknown. Maximum: unknown. Remaining: unknown.")).toBeTruthy();
    expect(screen.queryByText(/Maximum: 17|unlimited/)).toBeNull();
  });
it.each([undefined, false])("proof %s exposes no owner actions", (proof) => {
  render(view(held(), proof));
  expect(screen.getByText("Only the run owner can resume or cancel this hold.")).toBeTruthy();
  expect(screen.queryByRole("button", { name: /^Resume$/ })).toBeNull();
  expect(screen.queryByRole("button", { name: /^(Cancel|Stop)/ })).toBeNull();
  expect(api.resumeRun).not.toHaveBeenCalled();
  expect(submit).not.toHaveBeenCalled();
});
it("also requires canSteer and responds to ownership changes", () => {
  const { rerender } = render(view(held(), true, false));
  expect(screen.queryByRole("button", { name: /^Resume$/ })).toBeNull();
  rerender(view(held(), true));
  expect(screen.getByRole("button", { name: /^Resume$/ })).toBeTruthy();
  rerender(view(held(), false));
  expect(screen.queryByRole("button", { name: /^Resume$/ })).toBeNull();
});
it("only clicks invoke the existing Resume and terminal Cancel paths", async () => {
  render(view(held(), true));
  expect(api.resumeRun).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: /^Resume$/ }));
  await waitFor(() => expect(api.resumeRun).toHaveBeenCalledWith("held"));
  await waitFor(() => expect(refreshRun).toHaveBeenCalledOnce());
  fireEvent.click(screen.getByRole("button", { name: /^Cancel$/ }));
  await waitFor(() => expect(submit).toHaveBeenCalledWith("cancel", "", undefined, undefined, false));
});
it("announces a cause-only change and preserves ordinary recovery", async () => {
  const { container, rerender } = render(view(held({ recovery_wait_cause: null })));
  const notice = container.querySelector('div.sr-only[role="status"]')!;
  await waitFor(() => expect(notice.textContent).toMatch(/resume automatically/));
  rerender(view(held()));
  await waitFor(() => expect(notice.textContent).toMatch(/memory pressure/i));
  expect(notice.textContent).toMatch(/Only the run owner/);
  expect(notice.textContent).not.toMatch(/automatically/);
});
it("uses an explicit warning badge and attention state, with honest missing-cause copy", () => {
  const run = held();
  const badge = runBadge({ ...run, worker_name: null, owner_name: "Demo", is_mine: true, run_count: 1 }, 0);
  expect(badge).toMatchObject({ label: "memory pressure: owner hold", tone: "warning", pulse: false });
  expect(badge.kind === "badge" && badge.title).toMatch(/fresh memory episode/);
  expect(needsHumanAttention(effectiveRunStatus(run))).toBe(true);
  render(<StatusPill status={effectiveRunStatus(run)} />);
  expect(screen.getByText("memory pressure: owner hold")).toBeTruthy();
  expect(effectiveRunStatus({ ...run, status: "running" })).toBe("running");
  const generic = runBadge({ ...run, recovery_wait_cause: undefined, worker_name: null, owner_name: "Demo", is_mine: true, run_count: 1 }, 0);
  expect(generic.kind === "badge" && generic.title).toMatch(/Open the run page/);
});
