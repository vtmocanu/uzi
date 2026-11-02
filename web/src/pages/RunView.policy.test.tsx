// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { api, ApiError, type Run } from "../lib/api";
import { mockRuns } from "../mocks/data/runs";

vi.mock("../lib/useRunStream", () => ({ useRunStream: vi.fn() }));
vi.mock("../lib/api", async (original) => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api,
    getRunReview: vi.fn().mockResolvedValue({ review: null, pending_judge: null }),
    listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    listWorkers: vi.fn().mockResolvedValue({ workers: [] }),
    listSecrets: vi.fn().mockResolvedValue({ secrets: [] }),
    listFindings: vi.fn().mockResolvedValue({ findings: [] }),
    getMySettings: vi.fn().mockResolvedValue({ settings: {} }),
    getRunArchives: vi.fn().mockResolvedValue({
      supported: true, legacy: false, has_open_hold: false,
      counts: { preparing: 0, uploading: 0, available: 0, needs_action: 0, expired: 0, discarded: 0 },
      archives: [],
    }),
    resumeRun: vi.fn().mockResolvedValue({}),
    submitRunInput: vi.fn().mockResolvedValue({}),
  } };
});
const submit = vi.fn();
const refreshRun = vi.fn().mockResolvedValue(undefined);
const hold: NonNullable<Run["policy_hold"]> = {
  preserve_required: true, pending_fresh: false, outcome_id: "private-episode",
  continuation_mode: "same_conversation", resolved_at: null, applying_worker_id: null,
  applying_generation: 1, source_generation: 1, refusal_count: 1,
  policy_tag: "cyberPolicy", phase: "implementation", correlation_id: "private-correlation",
  published: true, captured_head: "abc123",
};
function held(overrides: Partial<Run> = {}): Run {
  return { ...mockRuns[0], id: "held", harness: "codex", status: "paused",
    hold_reason: "codex_policy_hold", policy_hold: hold, outcome_pending: undefined,
    completion_phase: "blocked", kind: "issue",
    milestones: [{ id: "M1", title: "Saved work" }], milestones_completed: ["M1"],
    can_stop_at_provider_hold: true, ...overrides };
}
function view(run: Run, ownership: { owner?: boolean } = { owner: true }) {
  vi.mocked(useRunStream).mockReturnValue({ run, messages: [], connected: true, error: "",
    submit, refreshRun, inputs: [], refreshInputs: vi.fn(), canSteer: true,
    confirmedOwner: ownership.owner, pauseOriginReady: true } as unknown as ReturnType<typeof useRunStream>);
  return <MemoryRouter initialEntries={["/runs/held"]}><Routes>
    <Route path="/runs/:id" element={<RunView />} />
  </Routes></MemoryRouter>;
}
function noContinuation() {
  expect(screen.queryAllByRole("button", { name: /Resume|Continue|Stop|Pause/ })).toHaveLength(0);
  expect(api.resumeRun).not.toHaveBeenCalled();
  expect(api.submitRunInput).not.toHaveBeenCalled();
}
beforeEach(() => {
  vi.clearAllMocks();
  submit.mockReset().mockResolvedValue(undefined);
});
afterEach(cleanup);

it.each([
  [true, "abc123", /Worker-reported published checkpoint/],
  [false, "abc123", /Verified local checkpoint; publication is not confirmed/],
  [null, "abc123", /Verified local checkpoint; publication is not confirmed/],
  [false, null, /Local-only retention; checkpoint capture is unverified/],
  [true, null, /Preservation is unverified/],
  [null, undefined, /Preservation is unverified/],
] as const)("presents preservation evidence: published=%s head=%s", (published, captured_head, copy) => {
  render(view(held({ policy_hold: { ...hold, published, captured_head } })));
  expect(screen.getByRole("heading", { name: "Codex policy hold" })).toBeTruthy();
  expect(screen.queryByText("Completion blocked")).toBeNull();
  expect(screen.getByText(copy)).toBeTruthy();
  expect(screen.getByText(/Continuation requires preserving the provider session/)).toBeTruthy();
  expect(screen.queryByText(/private-episode|private-correlation|cyberPolicy/)).toBeNull();
  expect(screen.getByRole("button", { name: "Cancel run" })).toBeTruthy();
  noContinuation();
});
it("handles absent policy data without claiming preservation or session continuity", () => {
  render(view(held({ policy_hold: undefined })));
  expect(screen.getByText(/Preservation is unverified\. Preservation evidence was last reported by the worker\./)).toBeTruthy();
  expect(screen.getByText(/Continuation requirements are unverified/)).toBeTruthy();
  noContinuation();
});
it("distinguishes future fresh conversation and escapes checkpoint content", () => {
  const head = "<img src=x onerror=alert(1)>";
  render(view(held({ policy_hold: { ...hold, captured_head: head, continuation_mode: "fresh_from_saved_work" } })));
  expect(screen.getByText(head)).toBeTruthy();
  expect(screen.getByText(/future continuation would use a fresh conversation from the saved work/)).toBeTruthy();
  expect(document.querySelector("img[src=x]")).toBeNull();
  noContinuation();
});
it.each(["confirmed", "pending"] as const)("%s Cancel keeps the existing explicit discard confirmation", async (state) => {
  submit.mockRejectedValueOnce(new ApiError(409, "Confirmation required", { reason: "outcome_pending_confirmation_required" }));
  const run = state === "confirmed" ? held() : held({ status: "running", hold_reason: null,
    outcome_pending: { reason: "codex_policy_hold_pending" } });
  render(view(run));
  fireEvent.click(screen.getByRole("button", { name: state === "pending" ? "Cancel run…" : "Cancel run" }));
  const confirm = await screen.findByRole("button", { name: "Discard and cancel" });
  expect(submit).toHaveBeenCalledExactlyOnceWith("cancel", "", undefined, undefined, false);
  const dialog = screen.getByRole("dialog");
  expect(dialog.textContent).not.toMatch(/finished on its worker|finished result/);
  fireEvent.click(confirm);
  await waitFor(() => expect(submit).toHaveBeenLastCalledWith("cancel", "", undefined, undefined, true));
  noContinuation();
});
it.each(["queued", "running", "paused", "awaiting_approval", "awaiting_followup", "awaiting_input", "limit_wait", "pool_wait", "recovery_wait"] as const)(
  "pending delivery at %s never becomes confirmed hold or offers continuation", (status) => {
    render(view(held({ status, hold_reason: null, policy_hold: undefined,
      outcome_pending: { reason: "codex_policy_hold_pending" },
      pause_requested_at: "2026-10-29T10:00:00Z", pause_mode: "milestone" })));
    const banner = screen.getByText("Policy report delivery pending").closest('[role="status"]')!;
    expect(banner.textContent).toMatch(/worker last reported a retained policy report/);
    expect(banner.textContent).toMatch(/server has not confirmed a policy hold/);
    expect(banner.textContent).toMatch(/Continuation is blocked pending delivery/);
    expect(banner.textContent).not.toMatch(/finished|Stopping|paused/);
    expect(screen.queryByRole("heading", { name: "Codex policy hold" })).toBeNull();
    expect(within(banner as HTMLElement).getByRole("button", { name: "Cancel run…" })).toBeTruthy();
    noContinuation();
  },
);
it.each(["completed", "failed", "cancelled"] as const)("hides stale pending delivery after %s", (status) => {
  const pending = held({ status: "running", hold_reason: null, completion_phase: "",
    outcome_pending: { reason: "codex_policy_hold_pending" } });
  const rendered = render(view(pending));
  expect(screen.getByText("Policy report delivery pending")).toBeTruthy();
  rendered.rerender(view({ ...pending, status }));
  expect(screen.queryByText("Policy report delivery pending")).toBeNull();
  expect(screen.queryByRole("button", { name: /Cancel run/ })).toBeNull();
});
it.each([false, undefined])("requires confirmed ownership %s for policy Cancel", (owner) => {
  render(view(held(), { owner }));
  expect(screen.queryByRole("button", { name: /Cancel run/ })).toBeNull();
  noContinuation();
});
it("retains ordinary provider hold Resume and completed-work Stop", async () => {
  render(view(held({ hold_reason: "provider_unavailable", policy_hold: undefined, completion_phase: "" })));
  const resume = screen.getByRole("button", { name: "Resume" });
  const stop = screen.getByRole("button", { name: "Stop completed work" }) as HTMLButtonElement;
  expect(stop.disabled).toBe(false);
  fireEvent.click(resume);
  await waitFor(() => expect(api.resumeRun).toHaveBeenCalledWith("held"));
  await waitFor(() => expect((resume as HTMLButtonElement).disabled).toBe(false));
  fireEvent.click(stop);
  await waitFor(() => expect(api.submitRunInput).toHaveBeenCalledWith("held", "scope", "1"));
  await waitFor(() => expect(api.resumeRun).toHaveBeenCalledTimes(2));
});
it("keeps permanent pending-outcome copy and discard action", () => {
  render(view(held({ status: "running", hold_reason: null, policy_hold: undefined, completion_phase: "",
    outcome_pending: { reason: "worker_auth_failed" } })));
  expect(screen.getByText(/The run finished on its worker, but that outcome has not reached uzi yet/)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Discard held outcome…" })).toBeTruthy();
});
