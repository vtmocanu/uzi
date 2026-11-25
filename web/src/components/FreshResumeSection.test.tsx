// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { FreshResumeSection } from "./FreshResumeSection";
import { api, type FreshResumePreview, type Run } from "../lib/api";
import { ApiError } from "../lib/apiError";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return { ...actual, api: { getFreshResumePreview: vi.fn(), startFreshResume: vi.fn() } };
});
const mockApi = vi.mocked(api);
const run = { id: "r1", status: "recovery_wait" } as Run;

function preview(over: Partial<FreshResumePreview> = {}): FreshResumePreview {
  return {
    status: "available", selection_token: "tok1", evidence_generation: 3, claim_generation: 5,
    kind: "capture", capture_id: "cap1", sha: "abc123def", uncovered_generations: [4, 5], notes: [], ...over,
  };
}
const started = { id: "t1", run_id: "r1", request_id: "x", state: "pending", replay: false, evidence_generation: 3, kind: "capture", sha: "abc", admission_deadline: "" };

beforeEach(() => { mockApi.getFreshResumePreview.mockReset(); mockApi.startFreshResume.mockReset(); });
afterEach(cleanup);

async function openPreview(p: FreshResumePreview = preview()) {
  mockApi.getFreshResumePreview.mockResolvedValue(p);
  render(<FreshResumeSection run={run} busy={false} />);
  fireEvent.click(screen.getByRole("button", { name: /Fresh resume on another worker/ }));
  await waitFor(() => expect(mockApi.getFreshResumePreview).toHaveBeenCalledWith("r1"));
}

describe("FreshResumeSection", () => {
  it("shows the evidence facts, the cutoff text, the S6 text and the extra admission unit sentence", async () => {
    await openPreview();
    await screen.findByText("Evidence generation (Ge)");
    expect(screen.getByText("abc123def")).toBeTruthy();
    expect(screen.getByText("recovery capture")).toBeTruthy();
    for (const re of [
      /Edits made after the saved evidence may be missing/,
      /original worker's source and custody are kept/,
      /Unsaved edits may be lost if the original worker's volume is gone/,
      /Each attempt keeps its own custody hold/,
      /counts one extra admission unit until its new hold is released or the run ends/,
      /only a third eligible hosted worker can take a repeat fresh resume/,
      /the original worker never takes this run again/,
      /only two workers whose new worker is lost must add a worker or cancel/,
      /No saved evidence exists for generation 4, 5; any edits made in it are not included\./,
    ]) expect(screen.getByText(re)).toBeTruthy();
  });

  it("starts with the selection token and a UUID request id, and says started", async () => {
    await openPreview();
    mockApi.startFreshResume.mockResolvedValue(started);
    fireEvent.click(await screen.findByRole("button", { name: "Confirm fresh resume" }));
    await screen.findByText(/Fresh resume started/);
    const [id, body] = mockApi.startFreshResume.mock.calls[0];
    expect(id).toBe("r1");
    expect(body.selection_token).toBe("tok1");
    expect(body.request_id).toMatch(/^[0-9a-f-]{36}$/);
  });

  it("reuses the request id on a retry of the same confirm after an unknown outcome", async () => {
    await openPreview();
    mockApi.startFreshResume.mockRejectedValueOnce(new ApiError(502, "bad gateway")).mockResolvedValue(started);
    fireEvent.click(await screen.findByRole("button", { name: "Confirm fresh resume" }));
    fireEvent.click(await screen.findByRole("button", { name: "Retry fresh resume" }));
    await screen.findByText(/Fresh resume started/);
    const [a, b] = mockApi.startFreshResume.mock.calls.map((c) => c[1].request_id);
    expect(a).toBe(b);
  });

  it.each([400, 401, 404])("does not offer Retry after a %s and spends the request id", async (status) => {
    await openPreview();
    mockApi.startFreshResume.mockRejectedValue(new ApiError(status, "nope"));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm fresh resume" }));
    await screen.findByText(/start was not accepted/);
    expect(screen.queryByRole("button", { name: "Retry fresh resume" })).toBeNull();
  });

  it("offers Retry after a network failure", async () => {
    await openPreview();
    mockApi.startFreshResume.mockRejectedValue(new TypeError("Failed to fetch"));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm fresh resume" }));
    await screen.findByRole("button", { name: "Retry fresh resume" });
  });

  it("makes a new request id for a new confirm after a definite refusal and a fresh preview", async () => {
    await openPreview();
    mockApi.startFreshResume.mockRejectedValueOnce(new ApiError(409, "stale", { reason: "stale_selection" })).mockResolvedValue(started);
    fireEvent.click(await screen.findByRole("button", { name: "Confirm fresh resume" }));
    await screen.findByText(/selection changed since the preview/);
    fireEvent.click(screen.getByRole("button", { name: /Fresh resume on another worker/ }));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm fresh resume" }));
    await screen.findByText(/Fresh resume started/);
    const [a, b] = mockApi.startFreshResume.mock.calls.map((c) => c[1].request_id);
    expect(a).not.toBe(b);
  });

  it.each([
    ["stale_selection", /selection changed since the preview/],
    ["request_completed", /already finished/],
    ["fresh_resume_in_progress", /already in progress/],
    ["custody_limit", /custody hold limit/],
    ["something_new", /does not know/],
  ])("renders the 409 reason %s", async (reason, text) => {
    await openPreview();
    mockApi.startFreshResume.mockRejectedValue(new ApiError(409, "x", { reason }));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm fresh resume" }));
    await screen.findByText(text);
    expect(screen.queryByText(/something_new/)).toBeNull();
    expect(mockApi.startFreshResume).toHaveBeenCalledTimes(1);
  });

  it.each([
    ["no_destination", /No other persistent hosted worker/],
    ["custody_limit", /custody hold limit/],
    ["no_evidence", /no saved evidence/],
    ["expired", /expired/],
    ["disabled", /not enabled/],
    ["ambiguous", /none contains the others/],
  ] as const)("explains the non-available status %s and offers no confirm", async (status, text) => {
    await openPreview({ status, uncovered_generations: [], notes: [] });
    await screen.findByText(text);
    expect(screen.queryByRole("button", { name: /Confirm fresh resume/ })).toBeNull();
  });
});
