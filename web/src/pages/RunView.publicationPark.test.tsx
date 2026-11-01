// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { api, type Run } from "../lib/api";
import { mockRuns } from "../mocks/data/runs";
import { runBadge, parkLabel } from "../lib/runBadge";
import { StatusPill } from "../components/ui";
import { RunRow } from "./RunsList";

vi.mock("../lib/useRunStream", () => ({ useRunStream: vi.fn() }));
vi.mock("../lib/api", async (original) => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api,
    getRunReview: vi.fn().mockResolvedValue({ review: null, pending_judge: null }),
    listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    listWorkers: vi.fn().mockResolvedValue({ workers: [] }),
    getMySettings: vi.fn().mockResolvedValue({ settings: {} }),
    getRunArchives: vi.fn().mockResolvedValue({
      supported: true, legacy: false, has_open_hold: true,
      counts: { preparing: 0, uploading: 0, available: 0, needs_action: 0, expired: 0, discarded: 0 },
      archives: [],
    }),
    resumeRun: vi.fn().mockResolvedValue({}),
  } };
});
const refreshRun = vi.fn().mockResolvedValue(undefined);
const submit = vi.fn().mockResolvedValue(undefined);
const minsAgoIso = (m: number) => new Date(Date.now() - m * 60_000).toISOString();

function base(): Run {
  return { ...mockRuns[0], id: "pub", status: "recovery_wait", forge_type: "gitlab",
    recovery_wait_cause: "forge_unreachable", recovery_wait_phase: null,
    recovery_retry_not_before: null, forge_park_count: 5, forge_park_max: 5,
    health: "ok", health_reason: null, forge_origin_cooldown: undefined };
}
function publication(over: Partial<Run> = {}): Run {
  return { ...base(), recovery_wait_phase: "publication",
    publication_intent: { branch: "agent/issue-9", mr_iid: 42, original_head: "aaaa", candidate: "bbbb",
      state: "prepared", enrollment_seq: 1 }, ...over };
}
const timed = (over: Partial<Run> = {}) =>
  publication({ recovery_retry_not_before: "2099-01-01T00:00:00Z",
    publication_wait_started_at: minsAgoIso(20), publication_wait_limit_seconds: 7200, ...over });
const held = (reason: string | undefined, over: Partial<Run> = {}) =>
  publication({ recovery_retry_not_before: null, publication_hold_reason: reason,
    publication_wait_limit_seconds: 7200, ...over });

function view(run: Run, proof?: boolean) {
  vi.mocked(useRunStream).mockReturnValue({ run, messages: [], connected: true, error: "",
    submit, refreshRun, inputs: [], refreshInputs: vi.fn(), canSteer: true,
    confirmedOwner: proof } as unknown as ReturnType<typeof useRunStream>);
  return <MemoryRouter initialEntries={["/runs/pub"]}><Routes>
    <Route path="/runs/:id" element={<RunView />} />
  </Routes></MemoryRouter>;
}
beforeEach(() => { vi.clearAllMocks(); });
afterEach(cleanup);

// Badges, pills, park labels and list rows agree on one word per state.
it.each([
  ["timed", "2099-01-01T00:00:00Z", "Publishing paused"],
  ["held", null, "Publishing held: needs your decision"],
  ["unknown", undefined, "Publishing paused: retry timing unknown"],
] as const)("publication %s park reads the same on badge, pill, park label and list row", (_n, retry, label) => {
  const run = publication({ recovery_retry_not_before: retry });
  const badge = runBadge({ ...run, owner_name: "", worker_name: null, is_mine: true, run_count: 1 }, Date.now());
  expect(badge).toMatchObject({ label, tone: "warning", pulse: false });
  expect(parkLabel(run)).toBe(label);
  const { unmount } = render(<StatusPill status={run.status} recovery_wait_cause={run.recovery_wait_cause}
    recovery_retry_not_before={retry} recovery_wait_phase={run.recovery_wait_phase} />);
  expect(screen.getByText(label)).toBeTruthy();
  unmount();
  render(<MemoryRouter><ul><RunRow run={{ ...run, repo_path: "demo/repo", worker_name: null, judge_verdict: null, judge_todo_count: 0 }} now={Date.now()} /></ul></MemoryRouter>);
  expect(screen.getByText(label)).toBeTruthy();
});
it("never labels a publication park with the pre-clone forge wording or the park count", () => {
  const badge = runBadge({ ...publication(), owner_name: "", worker_name: null, is_mine: true, run_count: 1 }, Date.now());
  expect(badge).toMatchObject({ label: "Publishing held: needs your decision" });
  expect(String((badge as { title?: string }).title)).toContain("publication-only attempt");
  expect(String((badge as { label: string }).label)).not.toMatch(/Forge|\b5\b/);
});
it("keeps the pre-clone forge labels when the phase is absent or unrecognised", () => {
  for (const phase of [null, undefined, "", "future_phase"]) {
    const run = base();
    run.recovery_wait_phase = phase;
    expect(parkLabel(run)).toBe("Forge recovery needs your decision");
  }
});

it("renders a timed wait against its limit", () => {
  render(view(timed(), true));
  expect(screen.getByText(/Publishing paused/, { selector: "p[role=status]" })).toBeTruthy();
  expect(screen.getByText(/Waiting 20m of a 2h 0m limit\./)).toBeTruthy();
  expect(screen.getByText(/Publication retries automatically/)).toBeTruthy();
  // A timed wait is not an owner hold: no actions.
  expect(screen.queryByRole("button", { name: /^Resume$/ })).toBeNull();
  expect(screen.queryByRole("button", { name: /^Cancel$/ })).toBeNull();
});
it("renders a timed wait with no limit when the elapsed hold is disabled", () => {
  render(view(timed({ publication_wait_limit_seconds: undefined }), true));
  expect(screen.getByText(/Waiting 20m of no limit\./)).toBeTruthy();
  expect(screen.queryByText(/of a .* limit/)).toBeNull();
});
it("shows the merge request when one exists and the branch otherwise", () => {
  const { unmount } = render(view(timed()));
  expect(screen.getByText(/Publishing to: Merge request !42\./)).toBeTruthy();
  unmount();
  render(view(timed({ publication_intent: { branch: "agent/issue-9", mr_iid: null, original_head: "aaaa",
    candidate: null, state: "prepared", enrollment_seq: 1 } })));
  expect(screen.getByText(/Publishing to: Branch agent\/issue-9\./)).toBeTruthy();
  expect(screen.queryByText(/Merge request/)).toBeNull();
});
it("does not present the forge park count as a publication budget", () => {
  const { unmount } = render(view(timed(), true));
  expect(screen.getByText(/Waiting 20m/)).toBeTruthy();
  expect(screen.queryByText(/Forge parks/)).toBeNull();
  expect(screen.queryByText(/5 of 5/)).toBeNull();
  unmount();
  // Positive control: the pre-clone park still shows its count, so the negatives above can fail.
  render(view(base(), true));
  expect(screen.getByText(/Forge parks: 5 of 5/)).toBeTruthy();
});

it.each([
  ["publication_retry_exhausted", "The publication wait limit was reached."],
  ["publication_custodian_unavailable", "The worker holding the unpublished work is unavailable."],
  ["publication_model_required", "Publishing needs model work, which a publication-only resume never does."],
  ["publication_source_unavailable", "The unpublished work is not available."],
  ["publication_intent_mismatch", "The publication record did not match."],
] as const)("owner hold %s shows its fixed blocker and Resume/Cancel for the owner", async (reason, sentence) => {
  render(view(held(reason), true));
  expect(screen.getByText(sentence)).toBeTruthy();
  expect(screen.getByText(/Publishing held: needs your decision/, { selector: "p[role=status]" })).toBeTruthy();
  expect(screen.getByText(/Resume starts a new publication-only attempt with no model work and no other budget/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: /^Resume$/ }));
  await waitFor(() => expect(api.resumeRun).toHaveBeenCalledWith("pub"));
  fireEvent.click(screen.getByRole("button", { name: /^Cancel$/ }));
  await waitFor(() => expect(submit).toHaveBeenCalledWith("cancel", "", undefined, undefined, false));
});
const KEPT_LINE = /The work is kept on its worker\. Publication resumes without running the model again\./;
it("shows the kept-on-worker line for a timed wait and a retry-exhausted hold", () => {
  const { unmount } = render(view(timed(), true));
  expect(screen.getByText(KEPT_LINE)).toBeTruthy();
  unmount();
  render(view(held("publication_retry_exhausted"), true));
  expect(screen.getByText(KEPT_LINE)).toBeTruthy();
});
it.each([
  "publication_source_unavailable",
  "publication_custodian_unavailable",
  "publication_model_required",
] as const)("omits the kept-on-worker line for hold %s", (reason) => {
  render(view(held(reason), true));
  // Positive control: the hold panel rendered with its reason, so the absence below can fail.
  expect(screen.getByText(/Publishing held: needs your decision/, { selector: "p[role=status]" })).toBeTruthy();
  expect(screen.getByRole("button", { name: /^Resume$/ })).toBeTruthy();
  expect(screen.queryByText(KEPT_LINE)).toBeNull();
  expect(screen.queryByText(/kept on its worker/)).toBeNull();
});
it.each([undefined, false])("hides hold actions without confirmed owner proof (%s)", (proof) => {
  render(view(held("publication_retry_exhausted"), proof));
  expect(screen.getByText(/Only the confirmed run owner can Resume or Cancel/)).toBeTruthy();
  expect(screen.queryByRole("button", { name: /^Resume$/ })).toBeNull();
  expect(screen.queryByRole("button", { name: /^Cancel$/ })).toBeNull();
});
it("renders an unrecognised hold reason honestly without mapping it to a known one", () => {
  render(view(held("publication_from_the_future"), true));
  // The screen-reader announcer repeats the sentence; count the visible copy only.
  expect(screen.getAllByText(/Unrecognised hold reason: publication_from_the_future\./)
    .filter((n) => !n.closest(".sr-only"))).toHaveLength(1);
  expect(screen.queryByText(/wait limit was reached/)).toBeNull();
  expect(screen.getByRole("button", { name: /^Resume$/ })).toBeTruthy();
});
it("renders an owner hold without a reason and never promises execution or export", () => {
  render(view(held(undefined), true));
  expect(screen.getByText(/This owner hold has no scheduled retry/)).toBeTruthy();
  expect(screen.getAllByText(/does not promise execution/).filter((n) => !n.closest(".sr-only"))).toHaveLength(1);
  expect(screen.queryByText(/Export requires/)).toBeNull();
  expect(screen.queryByRole("link", { name: /download|export/i })).toBeNull();
});

it("names an unrecognised recovery phase on an otherwise ordinary forge park", () => {
  render(view({ ...base(), recovery_wait_phase: "future_phase" }, true));
  expect(screen.getByText(/Recovery phase: future_phase/)).toBeTruthy();
  expect(screen.getByText(/Forge parks: 5 of 5/)).toBeTruthy();
});
it("leaves the pre-clone forge park unchanged", () => {
  render(view(base(), true));
  expect(screen.getByText(/This owner hold has no scheduled retry/)).toBeTruthy();
  expect(screen.getByText(/Forge recovery needs your decision/, { selector: "p[role=status]" })).toBeTruthy();
  expect(screen.getByText(/Export requires a verified available recovery archive/)).toBeTruthy();
  expect(screen.getByRole("button", { name: /^Resume$/ })).toBeTruthy();
  expect(screen.queryByText(/Publishing|publication-only/)).toBeNull();
});

it("announces the publication state, replacing it when the hold reason changes", async () => {
  const { container, rerender } = render(view(timed(), true));
  const notice = container.querySelector('div.sr-only[role="status"]')!;
  await waitFor(() => expect(notice.textContent).toContain("Publishing is paused because the forge was unreachable"));
  expect(notice.textContent).toContain("no model work runs");
  rerender(view(held("publication_source_unavailable"), true));
  await waitFor(() => expect(notice.textContent).toContain("Publishing is held and needs your decision"));
  expect(notice.textContent).toContain("The unpublished work is not available.");
  expect(notice.textContent).not.toContain("automatically");
});
