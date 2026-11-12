// @vitest-environment jsdom
//
// PRD #3257 M2: the run-review panel's skip state. When the automatic judge was skipped because
// the judge's harness had no usable credential, the panel says when and why from the STORED row
// (fixed copy per reason, viewer-neutral), links to Run defaults, and keeps the Run judge button.
// The skip only renders when there is no review and no pending judge (read precedence review >
// pending > skip), and a rerun refusal (422) shows inline.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { JudgePanel } from "./JudgePanel";
import { api, ApiError, type JudgeSkip, type Run, type RunReview } from "../../lib/api";
import { mockRuns } from "../../mocks/data";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return {
    ...actual,
    api: {
      getRunReview: vi.fn(),
      rerunJudge: vi.fn(),
      listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    },
  };
});
const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const RUN: Run = { ...mockRuns.find((r) => r.id === "run-unjudged")!, id: "r-skip" };
const AT = "2026-11-10T09:30:00Z";

function skip(over: Partial<JudgeSkip> = {}): JudgeSkip {
  return { harness: "codex", reason: "no_usable_credential", at: AT, ...over };
}

function review(): RunReview {
  return {
    id: "rev1",
    target_run_id: "r-skip",
    verdict: "ideal",
    summary_md: "Smooth run.",
    judge_model: "haiku",
    status: "complete",
    created_at: AT,
    updated_at: AT,
    recommendations: [],
    filed_issues: [],
    dispositions: [],
    triage: { total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
  };
}

function renderPanel() {
  return render(
    <MemoryRouter>
      <JudgePanel run={RUN} />
    </MemoryRouter>,
  );
}

// The sentence is "The judge was skipped on <locale date>: <cause>"; the date is locale-
// formatted, so the assertions pin the prefix, the cause and the machine-readable <time>.
async function skipSentence(): Promise<HTMLElement> {
  return screen.findByText((_, el) => el?.tagName === "P" && (el.textContent ?? "").startsWith("The judge was skipped"));
}

describe("JudgePanel skip state (#3257 M2)", () => {
  it.each([
    [skip({ harness: "codex", reason: "no_usable_credential" }), ": Codex had no usable credential."],
    [skip({ harness: "claude", reason: "no_usable_credential" }), ": Claude had no usable credential."],
    [skip({ harness: "codex", reason: "credential_disabled" }), ": the Codex credential was disabled."],
    [skip({ harness: "claude", reason: "credential_disabled" }), ": the Claude credential was disabled."],
    [skip({ harness: "codex", reason: "codex_pool_unavailable" }), ": no pooled Codex account was available."],
  ])("renders fixed copy for %o", async (s, cause) => {
    mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: null, skip: s });
    renderPanel();
    const p = await skipSentence();
    expect(p.textContent).toMatch(/^The judge was skipped on .+: /);
    expect(p.textContent?.endsWith(cause)).toBe(true);
    expect(p.querySelector("time")?.getAttribute("dateTime")).toBe(AT);
    // Viewer-neutral: an admin reading another user's run is never told about "your" credential.
    expect(p.textContent).not.toMatch(/\byour\b/i);
    // The never-judged copy is replaced, not stacked under the skip.
    expect(screen.queryByText(/hasn't been judged yet/)).toBeNull();
    // Run defaults link + the existing, enabled Run judge button.
    const link = screen.getByRole("link", { name: "Run defaults" });
    expect(link.getAttribute("href")).toBe("/settings/run-defaults");
    const button = screen.getByRole("button", { name: "Run judge" }) as HTMLButtonElement;
    expect(button.disabled).toBe(false);
    expect(screen.getByText("judge skipped")).toBeTruthy();
  });

  it.each([
    skip({ harness: "gemini", reason: "no_usable_credential" }),
    skip({ harness: "codex", reason: "quota_<b>exceeded</b>" }),
  ])("an unknown harness or reason gets generic fixed copy, never the raw code: %o", async (s) => {
    mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: null, skip: s });
    renderPanel();
    const p = await skipSentence();
    expect(p.textContent?.endsWith(": the judge's harness had no usable credential.")).toBe(true);
    expect(document.body.textContent).not.toContain("gemini");
    expect(document.body.textContent).not.toContain("quota_");
  });

  it("drops the date clause rather than printing Invalid Date", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: null, skip: skip({ at: "nope" }) });
    renderPanel();
    const p = await skipSentence();
    expect(p.textContent).toBe("The judge was skipped: Codex had no usable credential.");
  });

  it("precedence: a review wins over a skip", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: null, skip: skip() });
    renderPanel();
    expect(await screen.findByText("Smooth run.")).toBeTruthy();
    expect(screen.queryByText(/The judge was skipped/)).toBeNull();
    expect(screen.queryByText("judge skipped")).toBeNull();
  });

  it("precedence: a pending judge wins over a skip", async () => {
    mockApi.getRunReview.mockResolvedValue({
      review: null,
      pending_judge: { state: "scheduled", enqueued_at: AT },
      skip: skip(),
    });
    renderPanel();
    expect(await screen.findByRole("button", { name: "Judge scheduled" })).toBeTruthy();
    expect(screen.queryByText(/The judge was skipped/)).toBeNull();
    expect(screen.queryByText("judge skipped")).toBeNull();
  });

  it("an absent skip key (older api) renders the never-judged state", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: null });
    renderPanel();
    expect(await screen.findByText(/hasn't been judged yet/)).toBeTruthy();
    expect(screen.queryByText(/The judge was skipped/)).toBeNull();
  });

  it("shows a rerun refusal (422 no_credential_for_harness) inline and keeps the skip", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: null, skip: skip() });
    const message = "the judge's harness (codex) has no usable credential";
    mockApi.rerunJudge.mockRejectedValue(
      new ApiError(422, message, { error: message, code: "no_credential_for_harness" }),
    );
    renderPanel();
    await skipSentence();
    fireEvent.click(screen.getByRole("button", { name: "Run judge" }));
    expect(await screen.findByText(message)).toBeTruthy();
    expect(screen.getByText(/The judge was skipped/)).toBeTruthy();
    expect((screen.getByRole("button", { name: "Run judge" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("shows the Codex pool refusal body inline", async () => {
    mockApi.getRunReview.mockResolvedValue({
      review: null,
      pending_judge: null,
      skip: skip({ reason: "codex_pool_unavailable" }),
    });
    const message = "no pooled Codex account is available; link, re-authenticate or pool a Codex account in Settings";
    mockApi.rerunJudge.mockRejectedValue(
      new ApiError(422, message, { error: message, code: "no_credential_for_harness", reason: "codex_pool_unavailable" }),
    );
    renderPanel();
    await skipSentence();
    fireEvent.click(screen.getByRole("button", { name: "Run judge" }));
    expect(await screen.findByText(message)).toBeTruthy();
  });

  it("a successful rerun clears the skip at once (the creation deleted the row)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: null, skip: skip() });
    mockApi.rerunJudge.mockResolvedValue({ run: { ...RUN, kind: "judge", status: "queued" } });
    renderPanel();
    await skipSentence();
    fireEvent.click(screen.getByRole("button", { name: "Run judge" }));
    await waitFor(() => expect(screen.queryByText(/The judge was skipped/)).toBeNull());
    expect(screen.getAllByText(/Judge re-queued/).length).toBeGreaterThan(0);
  });
});
