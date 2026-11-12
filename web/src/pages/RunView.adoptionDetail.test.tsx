// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
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

function adoption(detail?: unknown): Run {
  return { ...mockRuns[0], id: "held", status: "recovery_wait",
    recovery_wait_cause: "retained_recovery_adoption_failed", recovery_retry_not_before: null,
    recovery_wait_detail: detail, requeue_count: 0, outcome_pending: undefined } as unknown as Run;
}
function view(run: Run) {
  vi.mocked(useRunStream).mockReturnValue({ run, messages: [], connected: true, error: "",
    submit, refreshRun, inputs: [], refreshInputs: vi.fn(), canSteer: true,
    confirmedOwner: true } as unknown as ReturnType<typeof useRunStream>);
  return <MemoryRouter initialEntries={["/runs/held"]}><Routes>
    <Route path="/runs/:id" element={<RunView />} />
  </Routes></MemoryRouter>;
}
const resumeBtn = () => screen.getByRole("button", { name: /^Resume$/ }) as HTMLButtonElement;
const GENERIC = /Resume starts another recovery episode\. If the underlying block persists/;

beforeEach(() => {
  vi.clearAllMocks();
  submit.mockReset().mockResolvedValue(undefined);
});
afterEach(() => cleanup());

it("active secret remediation disables Resume with an accessible reason and shows both routes and Cancel", () => {
  render(view(adoption("secret_remediation_active")));
  const resume = resumeBtn();
  expect(resume.disabled).toBe(true);
  const reasonId = resume.getAttribute("aria-describedby");
  expect(reasonId).toBeTruthy();
  const reason = document.getElementById(reasonId!)!;
  const text = reason.textContent ?? "";
  expect(text).toMatch(/active secret-scan remediation/);
  expect(text).toMatch(/uzi run export <run-id> --output <path>/);
  expect(text).toMatch(/may omit later work kept on the worker/);
  expect(text).toMatch(/operator to recover the retained work from worker storage/);
  expect(text).toMatch(/Cancel ends the run/);
  expect(text).not.toMatch(/latest|complete/i);
  expect((screen.getByRole("button", { name: /^Cancel$/ }) as HTMLButtonElement).disabled).toBe(false);
  expect(screen.queryByText(GENERIC)).toBeNull();
});

it("active secret remediation shows both routes even when an archive is listed", async () => {
  vi.mocked(api.getRunArchives).mockResolvedValue({
    supported: true, legacy: false, has_open_hold: true,
    counts: { preparing: 0, uploading: 0, available: 1, needs_action: 0, expired: 0, discarded: 0 },
    archives: [],
  } as unknown as Awaited<ReturnType<typeof api.getRunArchives>>);
  render(view(adoption("secret_remediation_active")));
  await waitFor(() => expect(api.getRunArchives).toHaveBeenCalled());
  expect(screen.getByText(/operator to recover the retained work from worker storage/)).toBeTruthy();
  expect(screen.getByText(/uzi run export <run-id> --output <path>/)).toBeTruthy();
});

it("remediation_proof_unknown says a retry may help and keeps Resume enabled", async () => {
  render(view(adoption("remediation_proof_unknown")));
  expect(screen.getByText(/remediation proof could not be verified, so a retry may help/)).toBeTruthy();
  expect(resumeBtn().disabled).toBe(false);
  expect(resumeBtn().hasAttribute("aria-describedby")).toBe(false);
  expect(screen.queryByText(/active secret-scan remediation/)).toBeNull();
  fireEvent.click(resumeBtn());
  await waitFor(() => expect(api.resumeRun).toHaveBeenCalledWith("held"));
});

it.each([null, undefined])("detail %s keeps today's copy with Resume enabled", (detail) => {
  render(view(adoption(detail)));
  expect(screen.getByText("Integrity hold needs your decision")).toBeTruthy();
  expect(screen.getByText(GENERIC)).toBeTruthy();
  expect(resumeBtn().disabled).toBe(false);
  expect(screen.queryByText(/retry may help|active secret-scan remediation/)).toBeNull();
});

it("an unrecognised detail is never rendered and behaves like null", () => {
  const { container } = render(view(adoption("brand_new_value_from_newer_server")));
  expect(screen.getByText("Integrity hold needs your decision")).toBeTruthy();
  expect(screen.getByText(GENERIC)).toBeTruthy();
  expect(resumeBtn().disabled).toBe(false);
  expect(container.textContent).not.toMatch(/brand_new_value_from_newer_server/);
  expect(screen.queryByText(/retry may help|active secret-scan remediation/)).toBeNull();
});

it("a detail on a different cause is ignored", () => {
  render(view({ ...adoption("secret_remediation_active"), recovery_wait_cause: "retained_continuation_unwitnessed" } as Run));
  expect(screen.getByText("Unwitnessed continuation hold needs your decision")).toBeTruthy();
  expect(resumeBtn().disabled).toBe(false);
  expect(screen.queryByText(/active secret-scan remediation/)).toBeNull();
});

it("a 409 on Resume shows the server message", async () => {
  const message = "The retained source is held under an active secret-scan remediation; Resume cannot succeed unchanged.";
  vi.mocked(api.resumeRun).mockRejectedValueOnce(new ApiError(409, message));
  render(view(adoption("remediation_proof_unknown")));
  fireEvent.click(resumeBtn());
  await waitFor(() => expect(screen.getByText(message)).toBeTruthy());
  expect(resumeBtn().disabled).toBe(false);
});
