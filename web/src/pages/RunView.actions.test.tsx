// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
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
    getMySettings: vi.fn().mockResolvedValue({ settings: {} }),
    getRunArchives: vi.fn().mockResolvedValue({
      supported: true, legacy: false, has_open_hold: false,
      counts: { preparing: 0, uploading: 0, available: 0, needs_action: 0, expired: 0, discarded: 0 },
      archives: [],
    }),
    resumeRun: vi.fn(),
    resumeRunNow: vi.fn(),
  } };
});
const submit = vi.fn();
const refreshRun = vi.fn().mockResolvedValue(undefined);
type Mode = "ordinary" | "exhaustion" | "pool" | "gate";
function runFor(id: string, mode: Mode): Run {
  return { ...mockRuns[0], id, outcome_pending: undefined, hold_reason: null,
    status: mode === "ordinary" ? "paused" : mode === "exhaustion" ? "recovery_wait" : mode === "pool" ? "pool_wait" : "awaiting_approval",
    recovery_wait_cause: mode === "exhaustion" ? "worker_requeue_exhausted" : null,
    gate_revision: 2, plan_md: "# Plan two",
  };
}
function deferred() {
  let resolve!: () => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<Awaited<ReturnType<typeof api.resumeRun>>>((done, fail) => {
    resolve = () => done({ run: mockRuns[0] });
    reject = fail;
  });
  return { promise, resolve, reject };
}
function Controls() {
  const navigate = useNavigate();
  return <><button onClick={() => navigate("/runs/A")}>Go A</button>
    <button onClick={() => navigate("/runs/B")}>Go B</button></>;
}
function mount(modeA: Mode = "ordinary", modeB: Mode = modeA) {
  let delayedA = false;
  vi.mocked(useRunStream).mockImplementation((id) => ({
    run: runFor(delayedA ? "A" : id, id === "A" || delayedA ? modeA : modeB),
    messages: [], connected: true, error: "", submit, refreshRun,
    inputs: [], refreshInputs: vi.fn(), canSteer: true, confirmedOwner: true,
  } as ReturnType<typeof useRunStream>));
  const tree = <MemoryRouter initialEntries={["/runs/A"]}><Controls />
    <Routes><Route path="/runs/:id" element={<RunView />} /></Routes>
  </MemoryRouter>;
  const view = render(tree);
  return { ...view, delayA: () => { delayedA = true; } };
}
function click(name: RegExp | string) {
  const button = screen.getAllByRole("button", { name })[0] as HTMLButtonElement;
  expect(button.disabled).toBe(false);
  fireEvent.click(button);
}
function resumeBusy(expected: boolean, pool = false) {
  const buttons = screen.getAllByRole("button", { name: pool ? /^Resume now$/ : /Resume$/ });
  for (const button of buttons) expect((button as HTMLButtonElement).disabled).toBe(expected);
}
function navigate(roundTrip = false) {
  click("Go B");
  if (roundTrip) click("Go A");
}
const refusal = () => new ApiError(409, "Confirm old cancel", { reason: "outcome_pending_confirmation_required" });
const mismatch = () => new ApiError(409, "Old gate changed", { reason: "gate_revision_mismatch", current_gate_revision: 3 });
async function settle(request: ReturnType<typeof deferred>, outcome: string) {
  await act(async () => {
    if (outcome === "success") request.resolve();
    else request.reject(outcome === "confirmation" ? refusal() : outcome === "mismatch" ? mismatch() :
      outcome === "409" ? new ApiError(409, "Old pool conflict") : new ApiError(500, "Old action failed"));
  });
}
beforeEach(() => {
  vi.clearAllMocks();
  submit.mockReset().mockResolvedValue(undefined);
  vi.mocked(api.resumeRun).mockReset().mockResolvedValue({ run: mockRuns[0] });
  vi.mocked(api.resumeRunNow).mockReset().mockResolvedValue({ run: mockRuns[0] });
  refreshRun.mockReset().mockResolvedValue(undefined);
});
afterEach(cleanup);

it.each([false, true])("ordinary Resume retires busy before navigation, round trip %s", async (roundTrip) => {
  const old = deferred();
  vi.mocked(api.resumeRun).mockReturnValueOnce(old.promise as ReturnType<typeof api.resumeRun>);
  mount();
  click(/Resume$/);
  navigate(roundTrip);
  const inheritedBusy = (screen.getAllByRole("button", { name: /Resume$/ })[0] as HTMLButtonElement).disabled;
  await settle(old, "failure");
  expect(screen.queryByText("Old action failed")).toBeNull();
  expect(inheritedBusy).toBe(false);
  resumeBusy(false);
});

it.each([
  ["ordinary", "ordinary", false], ["ordinary", "ordinary", true], ["ordinary", "exhaustion", false],
  ["exhaustion", "ordinary", false], ["ordinary", "cancel", false], ["cancel", "exhaustion", false],
] as const)("shared ownership from %s to %s preserves the newer action, round trip %s", async (first, second, roundTrip) => {
  const old = deferred();
  const newer = deferred();
  const firstApi = first === "cancel" ? submit : vi.mocked(api.resumeRun);
  const secondApi = second === "cancel" ? submit : vi.mocked(api.resumeRun);
  firstApi.mockReturnValueOnce(old.promise);
  secondApi.mockReturnValueOnce(newer.promise);
  mount(first === "cancel" ? "exhaustion" : first, second === "cancel" ? "exhaustion" : second);
  click(first === "cancel" ? /^Cancel$/ : /Resume$/);
  navigate(roundTrip);
  click(second === "cancel" ? /(Stop run|Cancel)$/ : /Resume$/);
  expect(secondApi).toHaveBeenCalled();
  await settle(old, "failure");
  expect(screen.queryByText("Old action failed")).toBeNull();
  resumeBusy(true);
  await settle(newer, "failure");
  expect(screen.getByText("Old action failed")).toBeTruthy();
  resumeBusy(false);
});

it.each(["success", "failure", "confirmation"])("old cancel %s after a round trip cannot touch a newer Resume", async (outcome) => {
  const old = deferred();
  const newer = deferred();
  submit.mockReturnValueOnce(old.promise);
  vi.mocked(api.resumeRun).mockReturnValueOnce(newer.promise as ReturnType<typeof api.resumeRun>);
  mount("exhaustion");
  click(/^Cancel$/);
  navigate(true);
  click(/Resume$/);
  await settle(old, outcome);
  expect(screen.queryByText("Old action failed")).toBeNull();
  expect(screen.queryByRole("button", { name: "Discard and cancel" })).toBeNull();
  resumeBusy(true);
  await settle(newer, "success");
  resumeBusy(false);
});

it.each([
  ["success", false], ["failure", false], ["409", false],
  ["success", true], ["failure", true], ["409", true],
] as const)("old pool %s cannot affect a newer resume, round trip %s", async (outcome, roundTrip) => {
  const old = deferred();
  const newer = deferred();
  vi.mocked(api.resumeRunNow)
    .mockReturnValueOnce(old.promise as ReturnType<typeof api.resumeRunNow>)
    .mockReturnValueOnce(newer.promise as ReturnType<typeof api.resumeRunNow>);
  mount("pool");
  click("Resume now");
  navigate(roundTrip);
  const inheritedBusy = (screen.getByRole("button", { name: "Resume now" }) as HTMLButtonElement).disabled;
  // Record retirement independently of whether the old request later settles.
  expect(inheritedBusy).toBe(false);
  click("Resume now");
  expect(api.resumeRunNow).toHaveBeenCalledTimes(2);
  await settle(old, outcome);
  expect(refreshRun).not.toHaveBeenCalled();
  expect(screen.queryByText("Old action failed")).toBeNull();
  expect(screen.queryByText("This run is no longer waiting.")).toBeNull();
  resumeBusy(true, true);
  await settle(newer, "success");
  expect(refreshRun).toHaveBeenCalledOnce();
  resumeBusy(false, true);
});

it("retires pool ownership on route B even while the stream still shows A", async () => {
  const old = deferred();
  vi.mocked(api.resumeRunNow).mockReturnValueOnce(old.promise as ReturnType<typeof api.resumeRunNow>);
  const view = mount("pool");
  click("Resume now");
  view.delayA();
  navigate();
  await settle(old, "409");
  expect(refreshRun).not.toHaveBeenCalled();
  expect(screen.queryByText("This run is no longer waiting.")).toBeNull();
  const button = screen.getByRole("button", { name: "Resume now" }) as HTMLButtonElement;
  expect(button.disabled).toBe(true);
  expect(api.resumeRunNow).toHaveBeenCalledOnce();
});

it.each(["success", "failure", "confirmation"])("old confirmed cancel %s cannot change a newer confirmation modal", async (outcome) => {
  const old = deferred();
  const newer = deferred();
  submit.mockRejectedValueOnce(refusal()).mockReturnValueOnce(old.promise)
    .mockRejectedValueOnce(refusal()).mockReturnValueOnce(newer.promise);
  mount("exhaustion");
  await act(async () => click(/^Cancel$/));
  click("Discard and cancel");
  expect(submit).toHaveBeenLastCalledWith("cancel", "", undefined, undefined, true);
  navigate(true);
  expect(screen.queryByRole("dialog")).toBeNull();
  await act(async () => click(/^Cancel$/));
  click("Discard and cancel");
  await settle(old, outcome);
  expect(screen.getByRole("dialog")).toBeTruthy();
  expect(screen.queryByText("Old action failed")).toBeNull();
  expect((screen.getByRole("button", { name: "Discarding…" }) as HTMLButtonElement).disabled).toBe(true);
  await settle(newer, "success");
  expect(screen.queryByRole("dialog")).toBeNull();
  resumeBusy(false);
});

it.each(["ordinary", "exhaustion", "pool"] as const)("retires %s resume on unmount", async (mode) => {
  const old = deferred();
  const resume = mode === "pool" ? vi.mocked(api.resumeRunNow) : vi.mocked(api.resumeRun);
  resume.mockReturnValueOnce(old.promise);
  const view = mount(mode);
  click(mode === "pool" ? "Resume now" : /Resume$/);
  view.unmount();
  await settle(old, "success");
  expect(refreshRun).not.toHaveBeenCalled();
});

it("keeps a current pool conflict gentle and resets its note on navigation", async () => {
  const current = deferred();
  vi.mocked(api.resumeRunNow).mockReturnValueOnce(current.promise);
  mount("pool");
  click("Resume now");
  await settle(current, "409");
  expect(screen.getByText("This run is no longer waiting.")).toBeTruthy();
  expect(refreshRun).toHaveBeenCalledOnce();
  resumeBusy(false, true);
  navigate();
  expect(screen.queryByText("This run is no longer waiting.")).toBeNull();
  resumeBusy(false, true);
});

it("old gate mismatch after a round trip cannot resync or clear newer action busy", async () => {
  const old = deferred();
  const newer = deferred();
  submit.mockReturnValueOnce(old.promise).mockReturnValueOnce(newer.promise);
  mount("gate");
  click(/Approve plan/);
  navigate(true);
  click(/Approve plan/);
  await settle(old, "mismatch");
  expect(refreshRun).not.toHaveBeenCalled();
  expect(screen.queryByText(/Your decision was not applied/)).toBeNull();
  expect((screen.getByRole("button", { name: /Approve plan/ }) as HTMLButtonElement).disabled).toBe(true);
  await settle(newer, "success");
  expect((screen.getByRole("button", { name: /Approve plan/ }) as HTMLButtonElement).disabled).toBe(false);
});
