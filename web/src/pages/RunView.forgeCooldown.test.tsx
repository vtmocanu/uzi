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
function forge(retry: string | null | undefined = null): Run {
  return { ...mockRuns[0], id: "forge", status: "recovery_wait",
    recovery_wait_cause: "forge_unreachable", recovery_retry_not_before: retry,
    forge_park_count: 5, forge_park_max: 5, health: "ok", health_reason: null,
    forge_origin_cooldown: undefined };
}
function view(run: Run, proof?: boolean, canSteer = true) {
  vi.mocked(useRunStream).mockReturnValue({ run, messages: [], connected: true, error: "",
    submit, refreshRun, inputs: [], refreshInputs: vi.fn(), canSteer,
    confirmedOwner: proof } as unknown as ReturnType<typeof useRunStream>);
  return <MemoryRouter initialEntries={["/runs/forge"]}><Routes>
    <Route path="/runs/:id" element={<RunView />} />
  </Routes></MemoryRouter>;
}
beforeEach(() => { vi.clearAllMocks(); });
afterEach(cleanup);

it.each([undefined, false, true])("only confirmed owner proof %s exposes held actions", (proof) => {
  render(view(forge(), proof));
  expect(screen.getByText(/This owner hold has no scheduled retry/)).toBeTruthy();
  expect(screen.queryByRole("button", { name: /^Resume$/ }) !== null).toBe(proof === true);
  expect(screen.queryByRole("button", { name: /^Cancel$/ }) !== null).toBe(proof === true);
  expect(screen.queryByText(/will resume automatically|Retries automatically/)).toBeNull();
  expect(screen.getByText(/Export requires a verified available recovery archive/)).toBeTruthy();
  expect(screen.queryByRole("link", { name: /download|export/i })).toBeNull();
});
it("requires canSteer even after owner proof and uses normal Resume and Cancel", async () => {
  const { rerender } = render(view(forge(), true, false));
  expect(screen.queryByRole("button", { name: /^Resume$/ })).toBeNull();
  rerender(view(forge(), true));
  fireEvent.click(screen.getByRole("button", { name: /^Resume$/ }));
  await waitFor(() => expect(api.resumeRun).toHaveBeenCalledWith("forge"));
  await waitFor(() => expect(refreshRun).toHaveBeenCalledOnce());
  fireEvent.click(screen.getByRole("button", { name: /^Cancel$/ }));
  await waitFor(() => expect(submit).toHaveBeenCalledWith("cancel", "", undefined, undefined, false));
});
it("reannounces timed to NULL to omitted without automatic promises on owner hold", async () => {
  const { container, rerender } = render(view(forge("2020-01-01T00:00:00Z"), true));
  const notice = container.querySelector('div.sr-only[role="status"]')!;
  await waitFor(() => expect(notice.textContent).toContain("Retries automatically after the scheduled time, including when already due"));
  expect(screen.queryByRole("button", { name: /^Resume$/ })).toBeNull();
  rerender(view(forge(), true));
  await waitFor(() => expect(notice.textContent).toContain("There is no scheduled retry"));
  expect(notice.textContent).not.toContain("automatically");
  const unknown = forge();
  delete unknown.recovery_retry_not_before;
  rerender(view(unknown, true));
  await waitFor(() => expect(notice.textContent).toContain("timing is unavailable"));
  expect(screen.queryByRole("button", { name: /^Resume$/ })).toBeNull();
  expect(screen.queryByText(/This owner hold has no scheduled retry/)).toBeNull();
});
it.each([
  [null, "Forge recovery needs your decision"],
  ["2020-01-01T00:00:00Z", "Forge retry scheduled"],
  ["2099-01-01T00:00:00Z", "Forge retry scheduled"],
  [undefined, "Forge retry timing unknown"],
  ["invalid", "Forge retry timing unknown"],
] as const)("keeps run badges, pills and list rows consistent for %s", (retry, label) => {
  const run = forge();
  run.recovery_retry_not_before = retry;
  const badge = runBadge({ ...run, owner_name: "", worker_name: null, is_mine: true, run_count: 1 }, Date.now());
  expect(badge).toMatchObject({ label, pulse: false });
  expect(parkLabel(run)).toBe(label);
  const { unmount } = render(<StatusPill status={run.status} recovery_wait_cause={run.recovery_wait_cause} recovery_retry_not_before={retry} />);
  expect(screen.getByText(label)).toBeTruthy();
  unmount();
  render(<MemoryRouter><ul><RunRow run={{ ...run, repo_path: "demo/repo", worker_name: null, judge_verdict: null, judge_todo_count: 0 }} now={Date.now()} /></ul></MemoryRouter>);
  expect(screen.getByText(label)).toBeTruthy();
});
it("shows queued admission advice even with OK health and keeps old projections unknown", () => {
  const run = { ...forge(), status: "queued" as const, health_reason: "Origin cooldown blocks admission; recovery unverified." };
  render(view(run, true));
  expect(screen.getAllByText(run.health_reason).some((node) => !node.classList.contains("sr-only"))).toBe(true);
  expect(screen.queryByText("Forge recovery needs your decision")).toBeNull();
});
