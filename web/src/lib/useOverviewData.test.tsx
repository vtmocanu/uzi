// @vitest-environment jsdom
// PRD #3878: the admin run list's loading rules inside the shared Overview data hook.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, renderHook } from "@testing-library/react";
import { api, type RunListItem, type RunSummaryItem } from "./api";
import { useAuth } from "../auth/AuthContext";
import { useOverviewData } from "./useOverviewData";
import { aRun, aUser, authValue, emptySelf } from "../pages/overviewFixtures";

vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return {
    ...actual,
    api: {
      listRuns: vi.fn(),
      listWorkers: vi.fn(),
      listRepos: vi.fn(),
      listAgentTemplates: vi.fn(),
      listSecrets: vi.fn(),
      listConnections: vi.fn(),
      getUsage: vi.fn(),
      getAdminUsage: vi.fn(),
      adminListRuns: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
const admin = { ...aUser, is_admin: true };

beforeEach(() => {
  vi.useFakeTimers();
  localStorage.clear();
  Object.defineProperty(document, "hidden", { configurable: true, get: () => false });
  vi.mocked(useAuth).mockReturnValue(authValue(admin));
  mockApi.listRuns.mockResolvedValue({ runs: [] });
  mockApi.listWorkers.mockResolvedValue({ workers: [] });
  mockApi.listRepos.mockResolvedValue({ repos: [] });
  mockApi.listAgentTemplates.mockResolvedValue({ templates: [] });
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.listConnections.mockResolvedValue({ connections: [] });
  mockApi.getUsage.mockResolvedValue(emptySelf());
  mockApi.getAdminUsage.mockResolvedValue({ factory: emptySelf(), users: [], earliest_run: null });
  mockApi.adminListRuns.mockResolvedValue({ runs: [] });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.clearAllMocks();
});

async function settle(ms = 0) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

// A request the test resolves or rejects by hand.
function control() {
  let resolve!: (v: { runs: RunListItem[] }) => void;
  let reject!: (e: Error) => void;
  const promise = new Promise<{ runs: RunListItem[] }>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const rows = (...ids: string[]) => ids.map((id) => aRun({ id, owner_email: "mira@uzi.local" }));
const ids = (runs: RunSummaryItem[] | null) => (runs ?? []).map((r) => r.id);

describe("useOverviewData admin runs", () => {
  it("keeps the last good rows and read time when a later read fails", async () => {
    mockApi.adminListRuns.mockResolvedValueOnce({ runs: rows("a", "b") });
    const { result } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
    await settle();
    expect(ids(result.current.adminRuns.runs)).toEqual(["a", "b"]);
    expect(result.current.adminRuns.failed).toBe(false);
    const lastOkAt = result.current.adminRuns.lastOkAt;
    expect(lastOkAt).not.toBeNull();

    mockApi.adminListRuns.mockRejectedValueOnce(new Error("blip"));
    await settle(10_000);
    expect(ids(result.current.adminRuns.runs)).toEqual(["a", "b"]);
    expect(result.current.adminRuns.failed).toBe(true);
    expect(result.current.adminRuns.lastOkAt).toBe(lastOkAt);
  });

  it("ignores an older request that resolves after a newer one", async () => {
    const first = control();
    mockApi.adminListRuns.mockReturnValueOnce(first.promise);
    mockApi.adminListRuns.mockResolvedValueOnce({ runs: rows("new") });
    const { result } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
    await settle();
    // The mount request is still pending; the 10 s poll issues the newer one.
    await settle(10_000);
    expect(mockApi.adminListRuns).toHaveBeenCalledTimes(2);
    expect(ids(result.current.adminRuns.runs)).toEqual(["new"]);

    await act(async () => {
      first.resolve({ runs: rows("stale") });
    });
    await settle();
    expect(ids(result.current.adminRuns.runs)).toEqual(["new"]);
    expect(result.current.adminRuns.failed).toBe(false);
  });

  it("ignores an older failure that arrives after a newer success", async () => {
    const first = control();
    mockApi.adminListRuns.mockReturnValueOnce(first.promise);
    mockApi.adminListRuns.mockResolvedValueOnce({ runs: rows("new") });
    const { result } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
    await settle();
    await settle(10_000);
    await act(async () => {
      first.reject(new Error("late failure"));
    });
    await settle();
    expect(ids(result.current.adminRuns.runs)).toEqual(["new"]);
    expect(result.current.adminRuns.failed).toBe(false);
  });

  it("never calls the admin endpoint for a non-admin, even when asked", async () => {
    vi.mocked(useAuth).mockReturnValue(authValue(aUser));
    const { result } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
    await settle(30_000);
    expect(mockApi.adminListRuns).not.toHaveBeenCalled();
    expect(result.current.adminRuns).toEqual({ runs: null, failed: false, lastOkAt: null });
  });
});
