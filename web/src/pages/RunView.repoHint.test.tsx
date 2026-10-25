// @vitest-environment jsdom
// Issue #3391: RunView feeds the plan gate the run's repo (capability hint, Docker allowlist).
// The repo is fetched per run; a late or stale answer for one run's repo must never reach the
// run now on screen.
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { api, type Repo, type Run } from "../lib/api";
import { mockRuns } from "../mocks/data/runs";

vi.mock("../lib/useRunStream", () => ({ useRunStream: vi.fn() }));
vi.mock("../lib/api", async (original) => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api,
    getRunReview: vi.fn().mockResolvedValue({ review: null, pending_judge: null }),
    listRepos: vi.fn(),
    listWorkers: vi.fn().mockResolvedValue({ workers: [] }),
    listSecrets: vi.fn().mockResolvedValue({ secrets: [] }),
    getMySettings: vi.fn().mockResolvedValue({ settings: {} }),
    setRepoRequiredCapabilities: vi.fn(),
    getRunArchives: vi.fn().mockResolvedValue({
      supported: true, legacy: false, has_open_hold: false,
      counts: { preparing: 0, uploading: 0, available: 0, needs_action: 0, expired: 0, discarded: 0 }, archives: [],
    }),
  } };
});

const HINT = /This repo doesn't require/;
const ALLOWLIST = /isn't on the Docker worker repo allowlist/;
const REQUIRE = { name: "Require on repo" };

function repo(id: string, over: Partial<Repo> = {}): Repo {
  return { id, web_url: `https://example.test/${id}`, required_capabilities: [], docker_allowlisted: true, ...over } as unknown as Repo;
}
function runFor(id: string, repoId: string | null): Run {
  return { ...mockRuns[0], id, repo_id: repoId, outcome_pending: undefined, hold_reason: null, status: "awaiting_approval",
    gate_revision: 1, plan_md: "# Plan", required_capabilities: ["docker"], worker_id: null } as Run;
}
const RUNS: Record<string, Run> = { A: runFor("A", "X"), B: runFor("B", "Y"), N: runFor("N", null) };

function Controls() {
  const navigate = useNavigate();
  return <>{["A", "B", "N"].map((id) => <button key={id} onClick={() => navigate(`/runs/${id}`)}>{`Go ${id}`}</button>)}</>;
}
function mount() {
  vi.mocked(useRunStream).mockImplementation((id) => ({
    run: RUNS[id as string], messages: [], connected: true, error: "", submit: vi.fn(), refreshRun: vi.fn().mockResolvedValue(undefined),
    inputs: [], refreshInputs: vi.fn(), canSteer: true, confirmedOwner: true,
  } as unknown as ReturnType<typeof useRunStream>));
  render(<MemoryRouter initialEntries={["/runs/A"]}><Controls />
    <Routes><Route path="/runs/:id" element={<RunView />} /></Routes></MemoryRouter>);
}
async function go(id: string) {
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: `Go ${id}` })); });
}
function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => { resolve = r; });
  return { promise, resolve };
}

beforeEach(() => {
  vi.mocked(api.listRepos).mockReset();
  vi.mocked(api.setRepoRequiredCapabilities).mockReset();
});
afterEach(() => { cleanup(); });

const X_BAD = () => repo("X", { docker_allowlisted: false });
const Y_OK = () => repo("Y", { required_capabilities: ["docker"] });

it("never shows one run's repo lines on another run or on a repo-less run", async () => {
  vi.mocked(api.listRepos).mockResolvedValue({ repos: [X_BAD(), Y_OK()] });
  mount();
  await act(async () => {});
  expect(screen.getByText(HINT)).toBeTruthy();
  expect(screen.getByText(ALLOWLIST)).toBeTruthy();
  // Straight from X's run to the repo-less run, so a leftover X repo would show here.
  await go("N");
  expect(screen.getByText(/Run requirements/)).toBeTruthy();
  expect(screen.queryByText(HINT)).toBeNull();
  expect(screen.queryByText(ALLOWLIST)).toBeNull();
  await go("A");
  expect(screen.getByText(ALLOWLIST)).toBeTruthy();
  await go("B");
  expect(screen.getByText(/Run requirements/)).toBeTruthy();
  expect(screen.queryByText(HINT)).toBeNull();
  expect(screen.queryByText(ALLOWLIST)).toBeNull();
});

it("ignores a late repo list for the previous run", async () => {
  const late = deferred<{ repos: Repo[] }>();
  vi.mocked(api.listRepos).mockReturnValueOnce(late.promise as ReturnType<typeof api.listRepos>);
  vi.mocked(api.listRepos).mockResolvedValue({ repos: [repo("Y")] });
  mount();
  await go("B");
  expect(screen.getByText(HINT)).toBeTruthy();
  await act(async () => { late.resolve({ repos: [X_BAD(), repo("Y")] }); });
  expect(screen.getByText(HINT)).toBeTruthy();
  expect(screen.queryByText(ALLOWLIST)).toBeNull();
});

it("ignores a late save for the previous run's repo", async () => {
  vi.mocked(api.listRepos).mockResolvedValue({ repos: [repo("X"), repo("Y")] });
  const patch = deferred<{ repo: Repo }>();
  vi.mocked(api.setRepoRequiredCapabilities).mockReturnValue(patch.promise as ReturnType<typeof api.setRepoRequiredCapabilities>);
  mount();
  await act(async () => {});
  await act(async () => { fireEvent.click(screen.getByRole("button", REQUIRE)); });
  expect(api.setRepoRequiredCapabilities).toHaveBeenCalledWith("X", ["docker"]);
  await go("B");
  expect(screen.getByText(HINT)).toBeTruthy();
  await act(async () => { patch.resolve({ repo: repo("X", { required_capabilities: ["docker"] }) }); });
  expect(screen.getByText(HINT)).toBeTruthy();
  expect(screen.getByRole("button", REQUIRE)).toBeTruthy();
  expect(api.setRepoRequiredCapabilities).toHaveBeenCalledTimes(1);
});

it("keeps the allowlist state when the save response omits the computed fields", async () => {
  vi.mocked(api.listRepos).mockResolvedValue({ repos: [repo("X")] });
  vi.mocked(api.setRepoRequiredCapabilities).mockResolvedValue({
    repo: { id: "X", required_capabilities: ["docker"] } as unknown as Repo,
  });
  mount();
  await act(async () => {});
  expect(screen.getByText(HINT)).toBeTruthy();
  await act(async () => { fireEvent.click(screen.getByRole("button", REQUIRE)); });
  expect(screen.queryByText(HINT)).toBeNull();
  expect(screen.queryByText(ALLOWLIST)).toBeNull();
  expect(api.setRepoRequiredCapabilities).toHaveBeenCalledWith("X", ["docker"]);
});

it("keeps a real allowlist gap when the save response omits the computed fields", async () => {
  vi.mocked(api.listRepos).mockResolvedValue({ repos: [X_BAD()] });
  vi.mocked(api.setRepoRequiredCapabilities).mockResolvedValue({
    repo: { id: "X", required_capabilities: ["docker"] } as unknown as Repo,
  });
  mount();
  await act(async () => {});
  expect(screen.getByText(ALLOWLIST)).toBeTruthy();
  await act(async () => { fireEvent.click(screen.getByRole("button", REQUIRE)); });
  expect(screen.queryByText(HINT)).toBeNull();
  expect(screen.getByText(ALLOWLIST)).toBeTruthy();
});
