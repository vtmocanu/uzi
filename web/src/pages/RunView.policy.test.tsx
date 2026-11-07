// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { api, ApiError, type Run } from "../lib/api";
import { mockRuns } from "../mocks/data/runs";
import wording from "../../../fixtures/policy-hold/wording.json";

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
    resumeRunNow: vi.fn().mockResolvedValue({}),
    resumePolicyHold: vi.fn(),
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

// Issue #3607: the owner's resume choice on a confirmed policy hold.
const sameConversation = { ...hold, hold_cause: "refused", resume_modes: ["preserve", "fresh_one_claim"] };
const attemptPath = { ...hold, hold_cause: "attempt_path", continuation_mode: "fresh_from_saved_work" as const,
  resume_modes: ["fresh_from_saved_work"] };
const modeButtons = () => screen.queryAllByRole("button", { name: new RegExp(
  `^(${Object.values(wording.modes).map(m => m.label).join("|")})$`) });
function bodyless() {
  expect(api.resumeRun).not.toHaveBeenCalled();
  expect(api.resumeRunNow).not.toHaveBeenCalled();
}

it("attempt-path episode offers only the fresh-from-saved-work action, with its consequence in the confirm", async () => {
  render(view(held({ policy_hold: attemptPath })));
  expect(screen.getByText(wording.causes.attempt_path)).toBeTruthy();
  expect(modeButtons().map(b => b.textContent)).toEqual([wording.modes.fresh_from_saved_work.label]);
  fireEvent.click(screen.getByRole("button", { name: wording.modes.fresh_from_saved_work.label }));
  const confirm = screen.getByRole("group", { name: wording.modes.fresh_from_saved_work.label });
  await waitFor(() => expect(document.activeElement).toBe(confirm));
  expect(confirm.textContent).toMatch(/fresh conversation from the saved work/i);
  const described = document.getElementById(confirm.getAttribute("aria-describedby")!);
  expect(described?.textContent).toBe(wording.modes.fresh_from_saved_work.consequence);
  expect(described?.textContent).toMatch(/ends for the rest of the run/);
  expect(screen.getByRole("button", { name: "Cancel run" })).toBeTruthy();
  bodyless();
});

it("same-conversation episode offers preserve and one-pickup fresh, nothing else", () => {
  render(view(held({ policy_hold: sameConversation })));
  expect(modeButtons().map(b => b.textContent)).toEqual([wording.modes.preserve.label, wording.modes.fresh_one_claim.label]);
  expect(screen.queryByRole("button", { name: /Stop|Revise/ })).toBeNull();
  const panel = screen.getByRole("heading", { name: "Codex policy hold" }).parentElement!;
  expect(within(panel).getByText(/Stop is not available for a policy hold/)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Cancel run" })).toBeTruthy();
});

it.each([
  ["empty", { ...hold, resume_modes: [] }],
  ["absent (older API)", hold],
  ["unknown modes only", { ...hold, resume_modes: ["resume", "toString"] }],
])("%s resume_modes offers no resume action", (_label, policy_hold) => {
  render(view(held({ policy_hold })));
  expect(modeButtons()).toHaveLength(0);
  noContinuation();
  expect(screen.getByRole("button", { name: "Cancel run" })).toBeTruthy();
});

it("offers no action once the episode is resolved and the run left the hold", () => {
  render(view(held({ status: "queued", hold_reason: null, policy_hold: { ...sameConversation, resolved_at: "2026-11-07T10:00:00Z", resume_modes: [] } })));
  expect(screen.queryByRole("heading", { name: "Codex policy hold" })).toBeNull();
  expect(modeButtons()).toHaveLength(0);
});

it("a non-owner sees the hold but no resume action", () => {
  render(view(held({ policy_hold: sameConversation }), { owner: false }));
  expect(modeButtons()).toHaveLength(0);
  expect(screen.getByText(/The run's owner can choose how to continue or Cancel/)).toBeTruthy();
});

it("posts the exact body, refreshes and reports success", async () => {
  vi.mocked(api.resumePolicyHold).mockResolvedValue({ run: held() });
  render(view(held({ policy_hold: sameConversation })));
  fireEvent.click(screen.getByRole("button", { name: wording.modes.preserve.label }));
  fireEvent.change(screen.getByLabelText("Guidance for the agent (optional)"), { target: { value: "Skip the exploit PoC; patch only." } });
  fireEvent.click(screen.getByRole("button", { name: "Confirm and resume" }));
  await waitFor(() => expect(api.resumePolicyHold).toHaveBeenCalledExactlyOnceWith("held",
    { mode: "preserve", expected_episode_id: "private-episode", guidance: "Skip the exploit PoC; patch only." }));
  await waitFor(() => expect(refreshRun).toHaveBeenCalled());
  expect(screen.getByText(/Resume the preserved conversation: resumed/)).toBeTruthy();
  bodyless();
});

it("omits blank guidance from the body", async () => {
  vi.mocked(api.resumePolicyHold).mockResolvedValue({ run: held() });
  render(view(held({ policy_hold: attemptPath })));
  fireEvent.click(screen.getByRole("button", { name: wording.modes.fresh_from_saved_work.label }));
  fireEvent.change(screen.getByLabelText("Guidance for the agent (optional)"), { target: { value: "   " } });
  fireEvent.click(screen.getByRole("button", { name: "Confirm and resume" }));
  await waitFor(() => expect(api.resumePolicyHold).toHaveBeenCalledExactlyOnceWith("held",
    { mode: "fresh_from_saved_work", expected_episode_id: "private-episode" }));
});

it("shows a replayed request as already resumed, and keeps it after the hold ends", async () => {
  vi.mocked(api.resumePolicyHold).mockResolvedValue({ run: held(), replay: true });
  const rendered = render(view(held({ policy_hold: sameConversation })));
  fireEvent.click(screen.getByRole("button", { name: wording.modes.fresh_one_claim.label }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm and resume" }));
  const note = await screen.findByText("Already resumed (this request was applied earlier).");
  expect(note.getAttribute("role")).toBe("status");
  rendered.rerender(view(held({ status: "queued", hold_reason: null, policy_hold: { ...sameConversation, resume_modes: [] } })));
  expect(screen.getByText("Already resumed (this request was applied earlier).")).toBeTruthy();
  await waitFor(() => expect(document.activeElement).toBe(
    screen.getByText("Already resumed (this request was applied earlier).").parentElement));
});

it.each([
  "policy hold changed or does not match this mode; refresh the run and choose again",
  "already resumed with different guidance; the new guidance was not recorded",
])("a 409 shows the server message and offers a refresh: %s", async (message) => {
  vi.mocked(api.resumePolicyHold).mockRejectedValue(new ApiError(409, message));
  render(view(held({ policy_hold: sameConversation })));
  fireEvent.click(screen.getByRole("button", { name: wording.modes.preserve.label }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm and resume" }));
  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toBe(message);
  refreshRun.mockClear();
  fireEvent.click(screen.getByRole("button", { name: "Refresh run" }));
  expect(refreshRun).toHaveBeenCalledTimes(1);
});

it("counts guidance in UTF-8 bytes and blocks submit above 16,000", () => {
  render(view(held({ policy_hold: sameConversation })));
  fireEvent.click(screen.getByRole("button", { name: wording.modes.preserve.label }));
  const box = screen.getByLabelText("Guidance for the agent (optional)");
  const submitButton = () => screen.getByRole("button", { name: "Confirm and resume" }) as HTMLButtonElement;
  // "é" is 2 bytes: 8,000 of them sit exactly on the cap.
  fireEvent.change(box, { target: { value: "é".repeat(8000) } });
  expect(screen.getByText("16,000 / 16,000 bytes")).toBeTruthy();
  expect(submitButton().disabled).toBe(false);
  expect(screen.queryByRole("alert")).toBeNull();
  fireEvent.change(box, { target: { value: "é".repeat(8000) + "a" } });
  expect(screen.getByText("16,001 / 16,000 bytes")).toBeTruthy();
  expect(submitButton().disabled).toBe(true);
  expect(screen.getByRole("alert").textContent).toMatch(/over the 16,000-byte limit/);
  expect(box.getAttribute("aria-describedby")).toBe(screen.getByText("16,001 / 16,000 bytes").id);
  fireEvent.click(submitButton());
  expect(api.resumePolicyHold).not.toHaveBeenCalled();
});

it("Escape and Back close the confirm and return focus to the action", async () => {
  render(view(held({ policy_hold: sameConversation })));
  fireEvent.click(screen.getByRole("button", { name: wording.modes.fresh_one_claim.label }));
  const confirm = screen.getByRole("group", { name: wording.modes.fresh_one_claim.label });
  fireEvent.keyDown(confirm, { key: "Escape" });
  await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: wording.modes.fresh_one_claim.label })));
  fireEvent.click(screen.getByRole("button", { name: wording.modes.preserve.label }));
  fireEvent.click(screen.getByRole("button", { name: "Back" }));
  await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: wording.modes.preserve.label })));
  expect(api.resumePolicyHold).not.toHaveBeenCalled();
});

it("keeps typed guidance when Escape closes the confirm and the same mode is reopened", () => {
  render(view(held({ policy_hold: sameConversation })));
  const label = wording.modes.preserve.label;
  fireEvent.click(screen.getByRole("button", { name: label }));
  const box = screen.getByLabelText("Guidance for the agent (optional)") as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "Keep the patch minimal." } });
  fireEvent.keyDown(box, { key: "Escape" });
  expect(screen.queryByRole("group", { name: label })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: label }));
  expect((screen.getByLabelText("Guidance for the agent (optional)") as HTMLTextAreaElement).value).toBe("Keep the patch minimal.");
});

it("closes the confirm when the episode changes under it", () => {
  const rendered = render(view(held({ policy_hold: sameConversation })));
  fireEvent.click(screen.getByRole("button", { name: wording.modes.preserve.label }));
  expect(screen.getByRole("group", { name: wording.modes.preserve.label })).toBeTruthy();
  rendered.rerender(view(held({ policy_hold: { ...sameConversation, outcome_id: "next-episode" } })));
  expect(screen.queryByRole("group", { name: wording.modes.preserve.label })).toBeNull();
  expect(screen.queryByRole("button", { name: "Confirm and resume" })).toBeNull();
});

it.each([
  [null, wording.unknown_cause],
  [undefined, wording.unknown_cause],
  ["cause_from_the_future", wording.unknown_cause],
  ["fresh_start_uncertain", wording.causes.fresh_start_uncertain],
])("cause %s reads its table wording", (hold_cause, text) => {
  render(view(held({ policy_hold: { ...sameConversation, hold_cause } })));
  expect(screen.getByText(text)).toBeTruthy();
  expect(screen.queryByText(/no provider (thread|conversation) (was )?started/i)).toBeNull();
});

it("states a pending one-pickup fresh start and recovery verification", () => {
  render(view(held({ policy_hold: { ...sameConversation, pending_fresh: true, recovery_required: true, recovery_verified_this_claim: false } })));
  expect(screen.getByText(/next pickup may start one new conversation/)).toBeTruthy();
  expect(screen.getByText(/required recovery checkpoint is recorded; the current pickup has not verified it/)).toBeTruthy();
});
