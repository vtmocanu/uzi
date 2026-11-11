// @vitest-environment jsdom
// Issue #3767: the owner-held `anthropic_spend_limit` recovery_wait hold.
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { api, type Run } from "../lib/api";
import { mockRuns } from "../mocks/data/runs";
import { effectiveRunStatus, needsHumanAttention, runBadge } from "../lib/runBadge";
import { parkDisposition } from "../lib/overview";
import { StatusPill } from "../components/ui";

vi.mock("../lib/useRunStream", () => ({ useRunStream: vi.fn() }));
vi.mock("../lib/api", async (original) => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api,
    getRunReview: vi.fn().mockResolvedValue({ review: null, pending_judge: null }),
    listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    listWorkers: vi.fn().mockResolvedValue({ workers: [] }),
    listSecrets: vi.fn().mockResolvedValue({ secrets: [] }),
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
  return { ...mockRuns[0], id: "held", kind: "issue", harness: "claude", trigger_source: undefined,
    status: "recovery_wait", recovery_wait_cause: "anthropic_spend_limit",
    recovery_retry_not_before: null, outcome_pending: undefined, ...over } as Run;
}
function view(run: Run, proof?: boolean) {
  vi.mocked(useRunStream).mockReturnValue({ run, messages: [], connected: true, error: "",
    submit, refreshRun, inputs: [], refreshInputs: vi.fn(), canSteer: true,
    confirmedOwner: proof } as unknown as ReturnType<typeof useRunStream>);
  return <MemoryRouter initialEntries={["/runs/held"]}><Routes>
    <Route path="/runs/:id" element={<RunView />} />
  </Routes></MemoryRouter>;
}
beforeEach(() => { vi.clearAllMocks(); });
afterEach(cleanup);

const WORDING_HEAD = /Resume uses this run.s current credential selection\. If it still selects the capped token, raise the limit first\./;

it("renders the spend-limit panel with the owner wording, Resume and Cancel, not the generic copy", async () => {
  render(view(held(), true));
  expect(screen.getByText("Anthropic API spend limit reached")).toBeTruthy();
  expect(screen.getByText(WORDING_HEAD)).toBeTruthy();
  expect(screen.getByText(/To choose another token now, use Switch token or/)).toBeTruthy();
  expect(screen.getByText("uzi run set-token").tagName).toBe("CODE");
  expect(screen.queryByText(/Recovering and resuming automatically|no action is needed/)).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: /^Resume$/ }));
  await waitFor(() => expect(api.resumeRun).toHaveBeenCalledWith("held"));
  fireEvent.click(screen.getByRole("button", { name: /^Cancel$/ }));
  await waitFor(() => expect(submit).toHaveBeenCalledWith("cancel", "", undefined, undefined, false));
});

it.each(["issue", "task"] as const)("a switchable %s run offers another token and a Settings link", async (kind) => {
  render(view(held({ kind }), true));
  expect(await screen.findByRole("button", { name: "Run with another token" })).toBeTruthy();
  const link = screen.getByRole("link", { name: "Manage tokens in Settings" });
  expect(link.getAttribute("href")).toBe("/settings");
});

it("a self_improve run offers neither and drops the choose-another-token sentence", () => {
  render(view(held({ kind: "self_improve" }), true));
  expect(screen.getByText(WORDING_HEAD)).toBeTruthy();
  expect(screen.getByRole("button", { name: /^Resume$/ })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Run with another token" })).toBeNull();
  expect(screen.queryByRole("link", { name: "Manage tokens in Settings" })).toBeNull();
  expect(screen.queryByText(/To choose another token now/)).toBeNull();
});

it("a non-owner sees no actions", () => {
  render(view(held(), false));
  expect(screen.getByText("Only the run owner can resume or cancel this hold.")).toBeTruthy();
  expect(screen.queryByRole("button", { name: /^Resume$/ })).toBeNull();
  expect(screen.queryByRole("button", { name: "Run with another token" })).toBeNull();
});

it("a queued run with a stale cause renders no panel and no hold badge", () => {
  const stale = held({ status: "queued" });
  render(view(stale, true));
  expect(screen.queryByText("Anthropic API spend limit reached")).toBeNull();
  expect(effectiveRunStatus(stale)).toBe("queued");
  const badge = runBadge({ ...stale, worker_name: null, owner_name: "Demo", is_mine: true, run_count: 1 }, 0);
  expect(badge.kind === "badge" && badge.label).not.toBe("spend limit: owner hold");
  // Control: the same run in recovery_wait does render the panel.
  cleanup();
  render(view(held(), true));
  expect(screen.getByText("Anthropic API spend limit reached")).toBeTruthy();
});

it("announces the hold and keeps the owner-hold badge, attention and disposition", async () => {
  const { container } = render(view(held(), true));
  const notice = container.querySelector('div.sr-only[role="status"]')!;
  await waitFor(() => expect(notice.textContent).toMatch(/Anthropic API spend limit reached/));
  const run = held();
  expect(effectiveRunStatus(run)).toBe("anthropic_spend_limit");
  expect(parkDisposition(run)).toBe("owner");
  expect(needsHumanAttention(effectiveRunStatus(run))).toBe(true);
  const badge = runBadge({ ...run, worker_name: null, owner_name: "Demo", is_mine: true, run_count: 1 }, 0);
  expect(badge).toMatchObject({ label: "spend limit: owner hold", tone: "warning", pulse: false });
  cleanup();
  render(<StatusPill status={effectiveRunStatus(run)} />);
  expect(screen.getByText("spend limit: owner hold")).toBeTruthy();
});
