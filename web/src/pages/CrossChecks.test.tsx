// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import { RunsHistory, RunsLayout, RunsList } from "./RunsList";
import { api } from "../lib/api";
import { ApiError } from "../lib/apiError";
import { useAuth } from "../auth/AuthContext";
import type { CrossCheckHistoryAttemptDTO as Attempt, CrossCheckHistoryDTO, CrossCheckHistoryGroupDTO as Group } from "../lib/apiTypes";
import full from "../../../fixtures/api-contract/cross_check_history.full.json";
import { crossChecksApi } from "../mocks/mockApi/crossChecks";

vi.mock("../mocks/mockApi/shared", async original => ({
  ...await original<typeof import("../mocks/mockApi/shared")>(),
  requireSession: () => ({ id: "owner" }),
  delay: <T,>(value: T) => Promise.resolve(value),
  mockScenario: () => "cross-check-history",
}));

vi.mock("../lib/api", async original => ({
  ...await original<typeof import("../lib/api")>(),
  api: {
    listRuns: vi.fn(), listSecrets: vi.fn(),
    crossCheckHistoryAvailability: vi.fn(), listCrossCheckHistory: vi.fn(),
  },
}));
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));
const mocked = vi.mocked(api);
function attempt(stage = "plan", round = 1, changes: Partial<Attempt> = {}): Attempt {
  return { ...full.groups[0].attempts[0], stage, round, active: false, historical: false, child_id: null,
    child_status: null, child_context: "No checker run available", execution: "couldnt_start",
    review: "none", reason: "Worker does not support this check", ...changes };
}
function group(changes: Partial<Group> = {}): Group {
  return { ...full.groups[0], parent_id: "00000000-0000-4000-8000-000000000001",
    parent_title: "Retained work", parent_status: "completed", active: true, refresh_needed: false,
    total_attempts: 1, matching_attempts: 1, active_attempts: 0, active_hidden: 0, active_context: undefined,
    older_cursor: undefined, omission: undefined, attempts: [attempt()], ...changes };
}
function Navigation() {
  const location = useLocation();
  const navigate = useNavigate();
  return <><output data-testid="location">{location.pathname}{location.search}</output>
    <button onClick={() => navigate("/runs?scope=cross-checks&stage=code")}>Change filter externally</button></>;
}
function Tree({ path = "/runs?scope=cross-checks" }: { path?: string }) {
  return <MemoryRouter initialEntries={[path]}><Navigation /><Routes><Route element={<RunsLayout />}>
    <Route path="/runs" element={<RunsList />} /><Route path="/runs/history" element={<RunsHistory />} />
  </Route></Routes></MemoryRouter>;
}
function account(id = "a") {
  vi.mocked(useAuth).mockReturnValue({ user: { id, is_admin: false, plan_cross_check_enabled: false, code_cross_check_enabled: false } } as ReturnType<typeof useAuth>);
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(r => { resolve = r; });
  return { promise, resolve };
}
beforeEach(() => {
  vi.resetAllMocks();
  localStorage.clear();
  account();
  mocked.listRuns.mockResolvedValue({ runs: [] });
  mocked.listSecrets.mockResolvedValue({ secrets: [] });
  mocked.crossCheckHistoryAvailability.mockResolvedValue({ available: true });
  mocked.listCrossCheckHistory.mockResolvedValue({ groups: [group()] });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it("retains disabled-setting history, separates execution/verdict/effect, and validates child links", async () => {
  mocked.listCrossCheckHistory.mockResolvedValue({ groups: [group({ attempts: [
    attempt("plan", 1, { historical: true, review: "approved", execution: "unknown", effect: "Effect not recorded", child_id: "javascript:bad" }),
    attempt("code", 1, { historical: true, execution: "invented", review: "invented" }),
  ], total_attempts: 2, matching_attempts: 2 })] });
  render(<Tree />);
  await screen.findByText("Retained work");
  fireEvent.click(screen.getByRole("button", { name: "Show attempt details" }));
  expect(screen.getByText("Historical approval of an earlier plan candidate.")).toBeTruthy();
  expect(screen.getByText("Historical attempt retained as evidence.")).toBeTruthy();
  expect(screen.getByText("Unknown verdict")).toBeTruthy();
  expect(screen.getAllByText("Unknown execution", { selector: "dd" })).toHaveLength(2);
  expect(screen.queryByRole("link", { name: "Open checker" })).toBeNull();
  expect(screen.getAllByText("Effect not recorded").length).toBeGreaterThan(0);
  expect(screen.getByText("Work run: completed")).toBeTruthy();
});
it("URL wins saved preference and explicit scope selection preserves lifecycle", async () => {
  localStorage.setItem("uzi.runs.scope.a", JSON.stringify("runs"));
  render(<Tree path="/runs/history?scope=cross-checks&stage=plan&outcome=passed" />);
  await screen.findByText("Retained work");
  expect(mocked.listCrossCheckHistory).toHaveBeenLastCalledWith({ lifecycle: "past", stage: "plan", outcome: "passed" });
  expect(localStorage.getItem("uzi.runs.scope.a")).toBe('"runs"');
  fireEvent.click(screen.getByRole("button", { name: "Runs" }));
  expect(screen.getByTestId("location").textContent).toContain("/runs/history?");
  expect(localStorage.getItem("uzi.runs.scope.a")).toBe('"runs"');
  fireEvent.click(screen.getByRole("button", { name: "Cross-checks" }));
  expect(localStorage.getItem("uzi.runs.scope.a")).toBe('"cross-checks"');
  fireEvent.click(screen.getByRole("link", { name: "Active" }));
  await waitFor(() => expect(mocked.listCrossCheckHistory).toHaveBeenLastCalledWith({ lifecycle: "active", stage: "plan" }));
  expect(mocked.crossCheckHistoryAvailability).toHaveBeenCalledTimes(1);
});
it.each(["null", "[]", "{}", '"bad"', "{"])("ignores malformed preference %s", async value => {
  localStorage.setItem("uzi.runs.scope.a", value);
  render(<Tree path="/runs" />);
  await screen.findByRole("button", { name: "Cross-checks" });
  expect(screen.getByRole("button", { name: "Runs" }).getAttribute("aria-pressed")).toBe("true");
  expect(mocked.listCrossCheckHistory).not.toHaveBeenCalled();
});
it("restores only the signed-in account preference on a URL without scope", async () => {
  localStorage.setItem("uzi.runs.scope.a", '"cross-checks"');
  const view = render(<Tree path="/runs/history" />);
  await screen.findByText("Retained work");
  account("b");
  view.rerender(<Tree path="/runs/history" />);
  await waitFor(() => expect(screen.getByRole("button", { name: "Runs" }).getAttribute("aria-pressed")).toBe("true"));
  expect(screen.queryByText("Retained work")).toBeNull();
  expect(localStorage.getItem("uzi.runs.scope.b")).toBeNull();
});
it("resets the page cursor on an account switch before requesting its history", async () => {
  const view = render(<Tree path="/runs?scope=cross-checks&cursor=account-a-page" />);
  await screen.findByText("Retained work");
  account("b");
  view.rerender(<Tree path="/runs?scope=cross-checks&cursor=account-a-page" />);
  await waitFor(() => expect(mocked.listCrossCheckHistory).toHaveBeenCalledTimes(2));
  expect(mocked.listCrossCheckHistory).toHaveBeenLastCalledWith({ lifecycle: "active", stage: "all" });
  expect(screen.getByTestId("location").textContent).not.toContain("cursor=");
});
it("survives blocked storage reads and writes", async () => {
  vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new Error("blocked"); });
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("blocked"); });
  render(<Tree path="/runs" />);
  fireEvent.click(await screen.findByRole("button", { name: "Cross-checks" }));
  await screen.findByText("Retained work");
});
it("falls back without overwriting the saved scope when no evidence exists", async () => {
  localStorage.setItem("uzi.runs.scope.a", '"cross-checks"');
  mocked.crossCheckHistoryAvailability.mockResolvedValue({ available: false });
  render(<Tree path="/runs" />);
  await screen.findByText("No retained cross-check evidence. Showing Runs.");
  expect(screen.queryByRole("group", { name: "What to list" })).toBeNull();
  expect(localStorage.getItem("uzi.runs.scope.a")).toBe('"cross-checks"');
  expect(mocked.listCrossCheckHistory).not.toHaveBeenCalled();
});
it("distinguishes failed availability, retry, and an older endpoint", async () => {
  mocked.crossCheckHistoryAvailability.mockRejectedValueOnce(new Error("hostile server error"))
    .mockRejectedValueOnce(new ApiError(404, "raw secret"));
  render(<Tree />);
  fireEvent.click(await screen.findByRole("button", { name: "Retry availability" }));
  await screen.findByText("Cross-check history is unavailable on this server version.");
  expect(screen.queryByText(/hostile|raw secret/)).toBeNull();
});
it("ignores old account availability and never copies its preference", async () => {
  localStorage.setItem("uzi.runs.scope.a", '"cross-checks"');
  const old = deferred<{ available: boolean }>();
  mocked.crossCheckHistoryAvailability.mockReturnValueOnce(old.promise);
  const view = render(<Tree path="/runs" />);
  account("b");
  view.rerender(<Tree path="/runs" />);
  await screen.findByRole("button", { name: "Cross-checks" });
  await act(async () => old.resolve({ available: false }));
  expect(screen.getByRole("button", { name: "Runs" }).getAttribute("aria-pressed")).toBe("true");
  expect(localStorage.getItem("uzi.runs.scope.b")).toBeNull();
});
it("ignores a stale list after filter changes and keeps availability independent", async () => {
  const old = deferred<CrossCheckHistoryDTO>();
  mocked.listCrossCheckHistory.mockReturnValueOnce(old.promise).mockResolvedValue({ groups: [group({ parent_title: "New filter" })] });
  render(<Tree />);
  await waitFor(() => expect(mocked.listCrossCheckHistory).toHaveBeenCalledTimes(1));
  fireEvent.click(screen.getByRole("button", { name: "Change filter externally" }));
  await screen.findByText("New filter");
  await act(async () => old.resolve({ groups: [group({ parent_title: "Stale filter" })] }));
  expect(screen.queryByText("Stale filter")).toBeNull();
  expect(mocked.crossCheckHistoryAvailability).toHaveBeenCalledTimes(1);
});
it("merges exact stage/round identities, prepends older history, and reports counts honestly", async () => {
  const latest = group({ total_attempts: 14, matching_attempts: 12, active_attempts: 3, active_hidden: 1, older_cursor: "older",
    attempts: [attempt("plan", 12, { active: true }), attempt("code", 1, { active: true })] });
  mocked.listCrossCheckHistory.mockResolvedValueOnce({ groups: [latest] }).mockResolvedValueOnce({ groups: [
    group({ attempts: [attempt("plan", 1, { active: true }), attempt("plan", 12), attempt("code", 1)], older_cursor: undefined }),
  ] });
  render(<Tree />);
  await screen.findByText("Earlier rounds omitted: load older attempts");
  expect(screen.getByText(/1 active attempts hidden/)).toBeTruthy();
  expect(screen.getByText("2 shown · 2 filtered · 10 remaining attempts")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Load older attempts" }));
  await waitFor(() => expect(screen.queryByText("Earlier rounds omitted: load older attempts")).toBeNull());
  expect(mocked.listCrossCheckHistory).toHaveBeenLastCalledWith({ lifecycle: "active", stage: "all", attempt_cursor: ["older"] });
  const ladder = within(screen.getByRole("group", { name: "Attempt ladder" }));
  expect(ladder.getAllByRole("button").map(b => b.textContent)).toEqual([
    "●Plan round 1 · Couldn't start", "●Plan round 12 · Couldn't start", "●Code round 1 · Couldn't start",
  ]);
  expect(screen.getByText("3 shown · 2 filtered · 9 remaining attempts")).toBeTruthy();
});
it("loads all 32 retained identities through bounded windows without hiding newest running evidence", async () => {
  mocked.listCrossCheckHistory.mockImplementation(crossChecksApi.listCrossCheckHistory);
  render(<Tree />);
  const card = within(await screen.findByRole("article", { name: "Synthetic cross-check work run 2" }));
  const ladder = within(card.getByRole("group", { name: "Attempt ladder" }));
  const titles = () => ladder.getAllByRole("button").map(button => button.getAttribute("title"));
  const recent = [
    ...Array.from({ length: 8 }, (_, i) => `Plan round ${23 + i}: Couldn't start`),
    "Plan round 31: Running", "Code round 1: Running",
  ];
  expect(titles()).toEqual(recent);
  expect(card.getByText("10 shown · 0 filtered · 22 remaining attempts")).toBeTruthy();
  expect(card.getByText(/1 active attempts hidden by filters or not loaded/)).toBeTruthy();
  expect(card.getByText("Earlier rounds omitted: load older attempts")).toBeTruthy();
  expect(card.queryByRole("button", { name: /Plan round 1 ·/ })).toBeNull();
  expect(mocked.listCrossCheckHistory).toHaveBeenCalledExactlyOnceWith({ lifecycle: "active", stage: "all" });
  fireEvent.click(ladder.getByRole("button", { name: "Plan round 31 · Running" }));
  expect(card.getByText("Checker: gpt-5.4 · high · running")).toBeTruthy();
  expect(card.getByRole("link", { name: "Open checker" }).getAttribute("href"))
    .toBe("/runs/10000000-0000-4000-8000-000000000231");

  // Each click requests the next bounded window for both visible groups.
  for (const [boundary, shown, remaining] of [[22, 20, 12], [12, 30, 2], [2, 32, 0]]) {
    fireEvent.click(card.getByRole("button", { name: "Load older attempts" }));
    await waitFor(() => expect(card.getByText(`${shown} shown · 0 filtered · ${remaining} remaining attempts`)).toBeTruthy());
    expect(mocked.listCrossCheckHistory).toHaveBeenLastCalledWith({
      lifecycle: "active", stage: "all", attempt_cursor: [
        `00000000-0000-4000-8000-000000000002:${boundary}`,
        `00000000-0000-4000-8000-000000000001:${boundary}`,
      ],
    });
    expect(ladder.getAllByRole("button")).toHaveLength(shown);
    if (remaining > 0) expect(card.getByText("Earlier rounds omitted: load older attempts")).toBeTruthy();
  }
  expect(mocked.listCrossCheckHistory).toHaveBeenCalledTimes(4);
  expect(titles()).toEqual([
    "Plan round 1: Running", "Plan round 2: Couldn't start", "Plan round 3: Replaced",
    "Plan round 4: Finished", "Plan round 5: Finished", "Plan round 6: Unknown execution",
    ...Array.from({ length: 24 }, (_, i) => `Plan round ${7 + i}: Couldn't start`),
    "Plan round 31: Running", "Code round 1: Running",
  ]);
  expect(new Set(titles()).size).toBe(32);
  expect(card.queryByText(/active attempts hidden by filters or not loaded/)).toBeNull();
  expect(card.queryByText("Earlier rounds omitted: load older attempts")).toBeNull();
  const exhausted = card.getByRole("button", { name: "All matching attempts loaded" });
  expect(exhausted.getAttribute("aria-disabled")).toBe("true");
  fireEvent.click(exhausted);
  expect(mocked.listCrossCheckHistory).toHaveBeenCalledTimes(4);
});

it("continues visible groups in one bounded vector request without the group-page cursor", async () => {
  const groups = Array.from({ length: 9 }, (_, i) => group({
    parent_id: `00000000-0000-4000-8000-${String(i + 1).padStart(12, "0")}`,
    parent_title: `Work ${i + 1}`, older_cursor: `older-${i + 1}`,
    total_attempts: 21, matching_attempts: 21,
    attempts: Array.from({ length: 10 }, (_, n) => attempt("plan", n + 12)),
  }));
  mocked.listCrossCheckHistory.mockResolvedValueOnce({ groups }).mockResolvedValueOnce({
    groups: groups.slice(0, 8).map(g => ({
      ...g, older_cursor: undefined, attempts: Array.from({ length: 10 }, (_, n) => attempt("plan", n + 2)),
    })),
  });
  render(<Tree path="/runs?scope=cross-checks&cursor=group-page" />);
  fireEvent.click(await screen.findByRole("button", { name: "Load older attempts for visible groups" }));
  await waitFor(() => expect(mocked.listCrossCheckHistory).toHaveBeenCalledTimes(2));
  expect(mocked.listCrossCheckHistory).toHaveBeenLastCalledWith({
    lifecycle: "active", stage: "all", attempt_cursor: Array.from({ length: 8 }, (_, i) => `older-${i + 1}`),
  });
  await waitFor(() => expect(within(screen.getByRole("article", { name: "Work 1" }))
    .getAllByRole("button", { name: /Plan round/ })).toHaveLength(20));
  expect(within(screen.getByRole("article", { name: "Work 9" }))
    .getAllByRole("button", { name: /Plan round/ })).toHaveLength(10);
});
it("resets group cursors on lifecycle and stage changes", async () => {
  render(<Tree path="/runs?scope=cross-checks&cursor=old-page" />);
  await screen.findByText("Retained work");
  fireEvent.click(screen.getByRole("link", { name: "Past checks" }));
  await waitFor(() => expect(mocked.listCrossCheckHistory).toHaveBeenLastCalledWith({
    lifecycle: "past", stage: "all", outcome: "any",
  }));
  fireEvent.click(within(screen.getByRole("group", { name: "Stage" })).getByRole("button", { name: "Code" }));
  await waitFor(() => expect(mocked.listCrossCheckHistory).toHaveBeenLastCalledWith({
    lifecycle: "past", stage: "code", outcome: "any",
  }));
  expect(screen.getByTestId("location").textContent).not.toContain("cursor=");
});
it("keeps last-good data on refresh failure and treats lifecycle invalidation distinctly", async () => {
  mocked.listCrossCheckHistory.mockResolvedValueOnce({ groups: [group({ older_cursor: "old" })] })
    .mockResolvedValueOnce({ groups: [group({ refresh_needed: true })] })
    .mockRejectedValueOnce(new Error("raw error"));
  render(<Tree />);
  fireEvent.click(await screen.findByRole("button", { name: "Load older attempts" }));
  await screen.findByText("Cross-check lifecycle changed. Refresh this page.");
  fireEvent.click(screen.getByRole("button", { name: "Refresh cross-checks" }));
  await screen.findByText("Cross-check history could not be loaded. Please retry.");
  expect(screen.getByText("Retained work")).toBeTruthy();
  expect(screen.queryByText("raw error")).toBeNull();
});
it("uses the server cursor for the next group page", async () => {
  mocked.listCrossCheckHistory.mockResolvedValueOnce({ groups: [group()], next_cursor: "page-two" })
    .mockResolvedValueOnce({ groups: [group({ parent_title: "Second page" })] });
  render(<Tree />);
  fireEvent.click(await screen.findByRole("button", { name: "Load more groups" }));
  await screen.findByText("Second page");
  expect(mocked.listCrossCheckHistory).toHaveBeenLastCalledWith({ lifecycle: "active", stage: "all", cursor: "page-two" });
  expect(screen.queryByText("Retained work")).toBeNull();
});
it("polls active checks only while visible and keeps last-good evidence on errors", async () => {
  render(<Tree />);
  await screen.findByText("Retained work");
  vi.spyOn(document, "hidden", "get").mockReturnValue(true);
  act(() => document.dispatchEvent(new Event("visibilitychange")));
  expect(mocked.listCrossCheckHistory).toHaveBeenCalledTimes(1);
  vi.spyOn(document, "hidden", "get").mockReturnValue(false);
  mocked.listCrossCheckHistory.mockRejectedValueOnce(new Error("private error"));
  act(() => document.dispatchEvent(new Event("visibilitychange")));
  await screen.findByText("Cross-check history could not be loaded. Please retry.");
  expect(screen.getByText("Retained work")).toBeTruthy();
  expect(mocked.crossCheckHistoryAvailability).toHaveBeenCalledTimes(1);
});
it("never polls past checks on visibility changes", async () => {
  render(<Tree path="/runs/history?scope=cross-checks" />);
  await screen.findByText("Retained work");
  act(() => document.dispatchEvent(new Event("visibilitychange")));
  await act(async () => {});
  expect(mocked.listCrossCheckHistory).toHaveBeenCalledTimes(1);
});
it("ignores pending old-account list data", async () => {
  const pending = deferred<CrossCheckHistoryDTO>();
  mocked.listCrossCheckHistory.mockReturnValueOnce(pending.promise).mockResolvedValue({ groups: [group({ parent_title: "Account B work" })] });
  const view = render(<Tree />);
  await waitFor(() => expect(mocked.listCrossCheckHistory).toHaveBeenCalledTimes(1));
  account("b");
  view.rerender(<Tree />);
  await screen.findByText("Account B work");
  await act(async () => pending.resolve({ groups: [group({ parent_title: "Account A private work" })] }));
  expect(screen.queryByText("Account A private work")).toBeNull();
});
it("distinguishes initial loading, empty, no match, and older-server history", async () => {
  const initial = deferred<CrossCheckHistoryDTO>();
  mocked.listCrossCheckHistory.mockReturnValueOnce(initial.promise).mockResolvedValueOnce({ groups: [] })
    .mockRejectedValueOnce(new ApiError(404, "server error"));
  render(<Tree />);
  await screen.findByText("Loading cross-checks...");
  await act(async () => initial.resolve({ groups: [] }));
  await screen.findByText("No cross-checks running right now.");
  fireEvent.click(within(screen.getByRole("group", { name: "Stage" })).getByRole("button", { name: "Plan" }));
  await screen.findByText("No attempts match these filters.");
  fireEvent.click(within(screen.getByRole("group", { name: "Stage" })).getByRole("button", { name: "Code" }));
  await screen.findByText("Cross-check history is unavailable on this server version.");
});
it("sanitizes text and independent title/aria-label sinks", async () => {
  const hostile = "Safe\u001b[31m\u202e\n title\tvalue";
  mocked.listCrossCheckHistory.mockResolvedValue({ groups: [group({ parent_title: hostile, attempts: [
    attempt("plan", 1, { reason: hostile, effect: hostile, next_action: hostile, model: hostile }),
  ] })] });
  render(<Tree />);
  const title = await screen.findByTitle("Safe title value");
  expect(title.textContent).toBe("Safe title value");
  expect(screen.getByRole("article").getAttribute("aria-label")).toBe("Safe title value");
  fireEvent.click(screen.getByRole("button", { name: "Show attempt details" }));
  expect(document.body.textContent).not.toMatch(/[\p{Cc}\p{Cf}]/u);
});
