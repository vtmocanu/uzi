// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { api, ApiError, type Run } from "../lib/api";
import { mockRuns } from "../mocks/data/runs";
import { effectiveRunStatus, runBadge } from "../lib/runBadge";
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
function held(limit = 3): Run {
  return { ...mockRuns[0], id: "held", status: "recovery_wait",
    recovery_wait_cause: "worker_requeue_exhausted", requeue_count: 8,
    checkpoint_contains_latest: true, outcome_pending: undefined,
    worker_recovery: { episode: 2, automatic_requeue_limit: limit, episode_used: limit,
      episode_remaining: 0, evidence: { checkpoint_tip: "abc123", available_capture: true,
        publication_uncertain: true, capture_uncertain: true, custody_uncertain: true,
        unknown: true, recorded_at: "2026-10-07T04:00:00Z" } } };
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

it.each([0, 3])("renders limit %s and historical evidence even after captures disappear", (limit) => {
  render(view(held(limit), true));
  expect(screen.getByText("Worker recovery needs your decision")).toBeTruthy();
  expect(screen.getByText(`Automatic recovery limit: ${limit}. Used: ${limit}. Remaining: 0.`)).toBeTruthy();
  expect(screen.getByText("Lifetime automatic requeues: 8.")).toBeTruthy();
  expect(screen.getByText("The server recorded checkpoint abc123 before this hold. Current availability and the latest local edits are not verified.")).toBeTruthy();
  expect(screen.getByText("A recovery capture was recorded as available at 2026-10-07T04:00:00Z. It may later expire or be discarded; it may not contain the latest local edits.")).toBeTruthy();
  expect(screen.getByText(/Publication is pending or uncertain/)).toBeTruthy();
  expect(screen.getByText(/Capture is pending or uncertain/)).toBeTruthy();
  expect(screen.getByText(/Retained source custody is uncertain/)).toBeTruthy();
  expect(screen.getByText(/Server recovery evidence is incomplete/)).toBeTruthy();
  expect(screen.queryByText("The published checkpoint contains the latest work.")).toBeNull();
});
it.each([undefined, false])("pending or non-owner proof %s cannot resume or cancel", (proof) => {
  render(view(held(), proof));
  expect(screen.getByText("Only the run owner can resume or cancel this hold.")).toBeTruthy();
  expect(screen.queryByRole("button", { name: /^Resume$/ })).toBeNull();
  expect(screen.queryByRole("button", { name: /^(Cancel|Stop)/ })).toBeNull();
});
it("requires canSteer as well as proof", () => {
  render(view(held(), true, false));
  expect(screen.queryByRole("button", { name: /^Resume$/ })).toBeNull();
  expect(screen.queryByRole("button", { name: /^(Cancel|Stop)/ })).toBeNull();
});
it("enables actions only after owner proof and removes them after a non-owner reply", () => {
  const { rerender } = render(view(held()));
  expect(screen.queryByRole("button", { name: /^Resume$/ })).toBeNull();
  rerender(view(held(), true));
  expect((screen.getByRole("button", { name: /^Resume$/ }) as HTMLButtonElement).disabled).toBe(false);
  expect((screen.getByRole("button", { name: /^Cancel$/ }) as HTMLButtonElement).disabled).toBe(false);
  rerender(view(held(), false, false));
  expect(screen.queryByRole("button", { name: /^Resume$/ })).toBeNull();
  expect(screen.queryByRole("button", { name: /^(Cancel|Stop)/ })).toBeNull();
});
it("uses normal resume refresh and cancel handlers", async () => {
  render(view(held(), true));
  fireEvent.click(screen.getByRole("button", { name: /^Resume$/ }));
  await waitFor(() => expect(api.resumeRun).toHaveBeenCalledWith("held"));
  await waitFor(() => expect(refreshRun).toHaveBeenCalledOnce());
  fireEvent.click(screen.getByRole("button", { name: /^Cancel$/ }));
  await waitFor(() => expect(submit).toHaveBeenCalledWith("cancel", "", undefined, undefined, false));
});
it("cancel retains the held-outcome confirmation before discarding", async () => {
  submit.mockRejectedValueOnce(new ApiError(409, "Confirmation required", {
    reason: "outcome_pending_confirmation_required",
  }));
  render(view(held(), true));
  fireEvent.click(screen.getByRole("button", { name: /^Cancel$/ }));
  await waitFor(() => expect(screen.getByRole("button", { name: "Discard and cancel" })).toBeTruthy());
  expect(submit).toHaveBeenCalledWith("cancel", "", undefined, undefined, false);
  fireEvent.click(screen.getByRole("button", { name: "Discard and cancel" }));
  await waitFor(() => expect(submit).toHaveBeenLastCalledWith("cancel", "", undefined, undefined, true));
});
it("past retry timestamps do not add an exhaustion countdown", () => {
  render(view({ ...held(), recovery_retry_not_before: "2020-01-01T00:00:00Z" }, true));
  expect(screen.getByText(/This hold has no scheduled retry/)).toBeTruthy();
  expect(screen.queryByText(/resuming automatically|retrying in|retry in/i)).toBeNull();
});
it("keeps actions busy and surfaces resume errors", async () => {
  let reject!: (e: Error) => void;
  vi.mocked(api.resumeRun).mockImplementationOnce(() => new Promise((_, fail) => { reject = fail; }));
  render(view(held(), true));
  fireEvent.click(screen.getByRole("button", { name: /^Resume$/ }));
  expect((screen.getByRole("button", { name: /^Cancel$/ }) as HTMLButtonElement).disabled).toBe(true);
  reject(new ApiError(409, "Recovery refused"));
  await waitFor(() => expect(screen.getByText("Recovery refused")).toBeTruthy());
  expect((screen.getByRole("button", { name: /^Resume$/ }) as HTMLButtonElement).disabled).toBe(false);
});
it("uses the normal cancel busy and error path", async () => {
  let reject!: (e: Error) => void;
  submit.mockImplementationOnce(() => new Promise((_, fail) => { reject = fail; }));
  render(view(held(), true));
  fireEvent.click(screen.getByRole("button", { name: /^Cancel$/ }));
  expect((screen.getByRole("button", { name: /^Resume$/ }) as HTMLButtonElement).disabled).toBe(true);
  reject(new ApiError(500, "Cancel refused"));
  await waitFor(() => expect(screen.getByText("Cancel refused")).toBeTruthy());
  expect((screen.getByRole("button", { name: /^Cancel$/ }) as HTMLButtonElement).disabled).toBe(false);
});
it("missing recovery data stays unknown", () => {
  render(view({ ...held(), worker_recovery: undefined }, true));
  expect(screen.getByText("Automatic recovery limit, used and remaining: unknown.")).toBeTruthy();
  expect(screen.getByText("Server recovery evidence is unavailable.")).toBeTruthy();
});
it("empty evidence flags remain incomplete rather than implying archival guarantees", () => {
  const run = held();
  run.worker_recovery!.evidence = {
    checkpoint_tip: null, available_capture: false, publication_uncertain: false,
    capture_uncertain: false, custody_uncertain: false, unknown: false, recorded_at: "",
  };
  render(view(run, true));
  expect(screen.getByText("Server recovery evidence is incomplete.")).toBeTruthy();
});
it("announces a cause change without a status change and preserves ordinary recovery", async () => {
  const ordinary = { ...held(), recovery_wait_cause: null };
  const { container, rerender } = render(view(ordinary));
  const notice = container.querySelector('div.sr-only[role="status"]')!;
  await waitFor(() => expect(notice.textContent).toMatch(/resume automatically/));
  expect(screen.getByText("Recovering and resuming automatically")).toBeTruthy();
  rerender(view(held()));
  await waitFor(() => expect(notice.textContent).toMatch(/Only the run owner can resume or cancel/));
  expect(notice.textContent).not.toMatch(/automatically/);
});
it("badge and shared pill describe the owner decision without automatic retry", () => {
  const run = held();
  const badge = runBadge({ ...run, worker_name: run.worker_name ?? null, owner_name: "Demo", is_mine: true, run_count: 1 }, 0);
  expect(badge).toMatchObject({ label: "recovery needs decision", tone: "warning", pulse: false });
  expect(badge.kind === "badge" && badge.title).toMatch(/owner/);
  render(<StatusPill status={effectiveRunStatus(run)} />);
  expect(screen.getByText("recovery needs decision")).toBeTruthy();
});
