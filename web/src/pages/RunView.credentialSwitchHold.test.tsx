// @vitest-environment jsdom
//
// Issue #3181: the run view for the two token-switch holds. Each renders its own panel (not
// the owner-pause "Paused" card), the header pill names the hold, the always-mounted park
// region announces it, and the actions match what the server accepts: settling offers Cancel
// with Resume disabled and explained (the server 409s Resume), unsettled offers Resume and
// Cancel. Both are owner decisions, gated on confirmed ownership like the provider hold.
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { api, type Run } from "../lib/api";
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

it("settling: its own panel, Cancel works, Resume disabled with the reason as its description", async () => {
  mount(fixture("run-cred-switch-settling"));
  const p = panel();
  expect(within(p).getByRole("heading", { name: "Stopping processes after a token switch" })).toBeTruthy();
  expect(within(p).getByText(/the worker is stopping them now/)).toBeTruthy();
  // The fixture's override names the token Resume will continue on.
  expect(within(p).getByText(/Resume continues it on “console-key”/)).toBeTruthy();
  const resume = within(p).getByRole("button", { name: /Resume/ }) as HTMLButtonElement;
  expect(resume.disabled).toBe(true);
  const why = document.getElementById(resume.getAttribute("aria-describedby") ?? "");
  expect(why?.textContent).toBe("Resume and Switch token become available when the worker finishes. Cancel works now.");
  // Not the owner-pause card, and no header Resume either.
  expect(screen.queryByText("Nothing runs and nothing is spent while it waits.")).toBeNull();
  expect(screen.getAllByRole("button", { name: /Resume/ })).toEqual([resume]);
  // The header pill names the hold.
  expect(screen.getByText("stopping processes: token switch")).toBeTruthy();
  const cancel = within(p).getByRole("button", { name: "Cancel run" }) as HTMLButtonElement;
  expect(cancel.disabled).toBe(false);
  await act(async () => { fireEvent.click(cancel); });
  expect(submit).toHaveBeenCalledWith("cancel", "", undefined, undefined, false);
  expect(api.resumeRun).not.toHaveBeenCalled();
});

it("unsettled: the brief's copy, Resume requeues on the new token", async () => {
  mount(fixture("run-cred-switch-unsettled"));
  const p = panel();
  expect(within(p).getByRole("heading", { name: "Held: token switch" })).toBeTruthy();
  expect(within(p).getByText(
    "Processes could not be proven stopped; work is kept on its worker. Resume to continue on the new token, or Cancel.",
  )).toBeTruthy();
  expect(within(p).getByText(/Resume requeues this run on “console-key”/)).toBeTruthy();
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
  ["run-cred-switch-settling", /stopping this run's processes\. Resume becomes available when it finishes\. Cancel works now\./],
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
    expect(within(p).getByText(/Only the run owner can/)).toBeTruthy();
    expect(screen.queryAllByRole("button", { name: /Resume|Cancel run|Stop run/ })).toHaveLength(0);
  },
);

it("an ordinary owner pause still renders the Paused card and no token-switch panel (control)", () => {
  mount({ ...fixture("run-cred-switch-unsettled"), hold_reason: null });
  expect(screen.queryByTestId("credential-switch-hold-panel")).toBeNull();
  expect(screen.getByText("Nothing runs and nothing is spent while it waits.")).toBeTruthy();
});
