// @vitest-environment jsdom
//
// Issue #3181: the run view for the two token-switch holds. Each renders its own panel (not
// the owner-pause "Paused" card), the header pill names the hold, the always-mounted park
// region announces it, and the actions match what the server accepts. Settling keeps Resume
// enabled (the server lifts its fence on a lost worker with hold_reason unchanged) and shows
// the server's 409 refusal inline as an alert; unsettled offers Resume and Cancel. Both are
// owner decisions, gated on confirmed ownership like the provider hold.
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { api, type Run } from "../lib/api";
import { mockRuns } from "../mocks/data/runs";
import { ApiError } from "../lib/apiError";

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
  } };
});

const submit = vi.fn();
const refreshRun = vi.fn().mockResolvedValue(undefined);
let owner: boolean | undefined = true;

function fixture(id: "run-cred-switch-settling" | "run-cred-switch-unsettled"): Run {
  return { ...mockRuns.find((r) => r.id === id)!, id: "A" };
}

function mount(run: Run) {
  vi.mocked(useRunStream).mockImplementation(() => ({
    run, messages: [], connected: true, error: "", submit, refreshRun,
    inputs: [], refreshInputs: vi.fn(), canSteer: true, confirmedOwner: owner, pauseOriginReady: true,
  } as unknown as ReturnType<typeof useRunStream>));
  return render(<MemoryRouter initialEntries={["/runs/A"]}>
    <Routes><Route path="/runs/:id" element={<RunView />} /></Routes>
  </MemoryRouter>);
}

function panel(): HTMLElement {
  return screen.getByTestId("credential-switch-hold-panel");
}

beforeEach(() => {
  vi.clearAllMocks();
  owner = true;
  submit.mockReset().mockResolvedValue(undefined);
  vi.mocked(api.resumeRun).mockResolvedValue({ run: mockRuns[0] });
});
afterEach(cleanup);

const REFUSAL = "the worker is still finishing its own operations for this run; try Resume again shortly, or Cancel the run";
const WHY = "Resume may be refused until the worker finishes its own operations; then try again, or Cancel the run.";

it("settling: its own panel, Resume enabled with the server-authority note, Cancel works", async () => {
  mount(fixture("run-cred-switch-settling"));
  const p = panel();
  expect(within(p).getByRole("heading", { name: "Settling after a token switch" })).toBeTruthy();
  expect(within(p).getByText(/The worker is finishing its own operations for this run\./)).toBeTruthy();
  // No copy promises the processes are being stopped or that the worker will finish.
  expect(p.textContent).not.toMatch(/stopping them now|is stopping/);
  // The fixture's override names the token Resume will continue on.
  expect(within(p).getByText(/Resume continues it on “console-key”/)).toBeTruthy();
  const resume = within(p).getByRole("button", { name: /Resume/ }) as HTMLButtonElement;
  expect(resume.disabled).toBe(false);
  const why = document.getElementById(resume.getAttribute("aria-describedby") ?? "");
  expect(why?.textContent).toBe(WHY);
  // Not the owner-pause card, and no header Resume either.
  expect(screen.queryByText("Nothing runs and nothing is spent while it waits.")).toBeNull();
  expect(screen.getAllByRole("button", { name: /Resume/ })).toEqual([resume]);
  // The header pill names the hold.
  expect(screen.getByText("settling: token switch")).toBeTruthy();
  const cancel = within(p).getByRole("button", { name: "Cancel run" }) as HTMLButtonElement;
  expect(cancel.disabled).toBe(false);
  await act(async () => { fireEvent.click(cancel); });
  expect(submit).toHaveBeenCalledWith("cancel", "", undefined, undefined, false);
  expect(api.resumeRun).not.toHaveBeenCalled();
});

it("settling: a refused Resume shows the server's 409 inline as an alert and refetches", async () => {
  vi.mocked(api.resumeRun).mockRejectedValue(new ApiError(409, REFUSAL));
  mount(fixture("run-cred-switch-settling"));
  const resume = within(panel()).getByRole("button", { name: /Resume/ });
  await act(async () => { fireEvent.click(resume); });
  expect(api.resumeRun).toHaveBeenCalledWith("A");
  expect(refreshRun).toHaveBeenCalled();
  // Announced (role=alert) inside the panel, beside the button it answers.
  const alerts = within(panel()).getAllByRole("alert");
  expect(alerts.map((a) => a.textContent)).toEqual([`Resume refused: ${REFUSAL}`]);
  const ids = (resume.getAttribute("aria-describedby") ?? "").split(" ");
  expect(ids.map((i) => document.getElementById(i)?.textContent)).toEqual([WHY, `Resume refused: ${REFUSAL}`]);
  // Not duplicated on the page banner.
  expect(screen.getAllByRole("alert")).toEqual(alerts);
  // A successful retry clears the refusal.
  vi.mocked(api.resumeRun).mockResolvedValueOnce({ run: mockRuns[0] });
  await act(async () => { fireEvent.click(resume); });
  expect(within(panel()).queryAllByRole("alert")).toHaveLength(0);
});

it("settling: a non-409 Resume failure stays on the page banner (control)", async () => {
  vi.mocked(api.resumeRun).mockRejectedValue(new ApiError(500, "boom"));
  mount(fixture("run-cred-switch-settling"));
  await act(async () => { fireEvent.click(within(panel()).getByRole("button", { name: /Resume/ })); });
  expect(within(panel()).queryAllByRole("alert")).toHaveLength(0);
  expect(screen.getByRole("alert").textContent).toBe("boom");
});

function rerenderWith(view: ReturnType<typeof mount>, run: Run) {
  vi.mocked(useRunStream).mockImplementation(() => ({
    run, messages: [], connected: true, error: "", submit, refreshRun,
    inputs: [], refreshInputs: vi.fn(), canSteer: true, confirmedOwner: owner, pauseOriginReady: true,
  } as unknown as ReturnType<typeof useRunStream>));
  view.rerender(<MemoryRouter initialEntries={["/runs/A"]}>
    <Routes><Route path="/runs/:id" element={<RunView />} /></Routes>
  </MemoryRouter>);
}

it("unsettled: a 409 Resume refusal is page-banner feedback, not a settling refusal", async () => {
  vi.mocked(api.resumeRun).mockRejectedValue(new ApiError(409, "run is no longer paused"));
  mount(fixture("run-cred-switch-unsettled"));
  await act(async () => { fireEvent.click(within(panel()).getByRole("button", { name: /Resume/ })); });
  expect(api.resumeRun).toHaveBeenCalledWith("A");
  expect(within(panel()).queryAllByRole("alert")).toHaveLength(0);
  expect(screen.getByRole("alert").textContent).toBe("run is no longer paused");
});

it("a refusal from an earlier settling hold does not reappear on a later settling hold", async () => {
  vi.mocked(api.resumeRun).mockRejectedValue(new ApiError(409, REFUSAL));
  const view = mount(fixture("run-cred-switch-settling"));
  await act(async () => { fireEvent.click(within(panel()).getByRole("button", { name: /Resume/ })); });
  expect(within(panel()).getAllByRole("alert")).toHaveLength(1);
  // A refetch that leaves the run settling keeps the refusal.
  rerenderWith(view, { ...fixture("run-cred-switch-settling") });
  expect(within(panel()).getAllByRole("alert")).toHaveLength(1);
  // The hold moves on (here to a running run), then a fresh settling hold arrives.
  rerenderWith(view, { ...fixture("run-cred-switch-settling"), status: "running", hold_reason: null });
  expect(screen.queryByTestId("credential-switch-hold-panel")).toBeNull();
  rerenderWith(view, fixture("run-cred-switch-settling"));
  expect(within(panel()).getByRole("heading", { name: "Settling after a token switch" })).toBeTruthy();
  expect(within(panel()).queryAllByRole("alert")).toHaveLength(0);
  const resume = within(panel()).getByRole("button", { name: /Resume/ });
  expect((resume.getAttribute("aria-describedby") ?? "").split(" ")).toHaveLength(1);
});

it("unsettled never shows a settling refusal, even one left from the same run", async () => {
  vi.mocked(api.resumeRun).mockRejectedValue(new ApiError(409, REFUSAL));
  const view = mount(fixture("run-cred-switch-settling"));
  await act(async () => { fireEvent.click(within(panel()).getByRole("button", { name: /Resume/ })); });
  expect(within(panel()).getAllByRole("alert")).toHaveLength(1);
  const unsettled = fixture("run-cred-switch-unsettled");
  vi.mocked(useRunStream).mockImplementation(() => ({
    run: unsettled, messages: [], connected: true, error: "", submit, refreshRun,
    inputs: [], refreshInputs: vi.fn(), canSteer: true, confirmedOwner: owner, pauseOriginReady: true,
  } as unknown as ReturnType<typeof useRunStream>));
  view.rerender(<MemoryRouter initialEntries={["/runs/A"]}>
    <Routes><Route path="/runs/:id" element={<RunView />} /></Routes>
  </MemoryRouter>);
  expect(within(panel()).getByRole("heading", { name: "Held: token switch" })).toBeTruthy();
  expect(within(panel()).queryAllByRole("alert")).toHaveLength(0);
});

it("unsettled: the brief's copy, Resume requeues on the new token", async () => {
  mount(fixture("run-cred-switch-unsettled"));
  const p = panel();
  expect(within(p).getByRole("heading", { name: "Held: token switch" })).toBeTruthy();
  expect(within(p).getByText(
    "Processes could not be proven stopped; work is kept on its worker. Resume to continue on the new token, or Cancel.",
  )).toBeTruthy();
  expect(within(p).getByText("Resume requeues this run on “console-key”. No new work starts while it is held.")).toBeTruthy();
  // The acknowledgement does not prove the processes stopped, so nothing claims they are idle.
  expect(p.textContent).not.toMatch(/Nothing runs|nothing is spent/);
  expect(screen.getByText("held: token switch")).toBeTruthy();
  expect(screen.queryByText("Nothing runs and nothing is spent while it waits.")).toBeNull();
  const resumes = screen.getAllByRole("button", { name: /Resume/ });
  expect(resumes).toHaveLength(1);
  expect((resumes[0] as HTMLButtonElement).disabled).toBe(false);
  expect(resumes[0].hasAttribute("aria-describedby")).toBe(false);
  await act(async () => { fireEvent.click(resumes[0]); });
  expect(api.resumeRun).toHaveBeenCalledWith("A");
  expect(refreshRun).toHaveBeenCalled();
});

it.each([
  ["run-cred-switch-settling", /finishing its own operations for this run\. Resume may be refused until the worker finishes its own operations; then try again, or Cancel the run\./],
  ["run-cred-switch-unsettled", /Held after a token switch\. Processes could not be proven stopped/],
] as const)("%s is announced by the persistent park region", (id, text) => {
  mount(fixture(id));
  expect(screen.getAllByText(text).some((el) => el.closest("[aria-live]") !== null)).toBe(true);
});

it.each(["run-cred-switch-settling", "run-cred-switch-unsettled"] as const)(
  "%s without confirmed ownership offers no owner actions", (id) => {
    owner = false;
    mount(fixture(id));
    const p = panel();
    expect(within(p).queryAllByRole("button")).toHaveLength(0);
    expect(within(p).getByText("Only the run owner can resume or cancel this hold.")).toBeTruthy();
    expect(screen.queryAllByRole("button", { name: /Resume|Cancel run|Stop run/ })).toHaveLength(0);
  },
);

it("an ordinary owner pause still renders the Paused card and no token-switch panel (control)", () => {
  mount({ ...fixture("run-cred-switch-unsettled"), hold_reason: null });
  expect(screen.queryByTestId("credential-switch-hold-panel")).toBeNull();
  expect(screen.getByText("Nothing runs and nothing is spent while it waits.")).toBeTruthy();
});
