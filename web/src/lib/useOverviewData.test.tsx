// @vitest-environment jsdom
// PRD #3878: the admin run list's loading rules inside the shared Overview data hook.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, renderHook } from "@testing-library/react";
import { api, type RunSummaryItem } from "./api";
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

  // A response slower than the 10 s poll must still be applied: one request at a time.
  function slowAdminApi(failFrom = Infinity) {
    let inFlight = 0;
    let maxInFlight = 0;
    let calls = 0;
    mockApi.adminListRuns.mockImplementation(() => {
      const n = ++calls;
      inFlight++;
      maxInFlight = Math.max(maxInFlight, inFlight);
      return new Promise((resolve, reject) => {
        setTimeout(() => {
          inFlight--;
          if (n >= failFrom) reject(new Error("slow failure"));
          else resolve({ runs: rows(`r${n}`) });
        }, 15_000);
      });
    });
    return { max: () => maxInFlight };
  }

  it("applies rows from responses slower than the poll, never overlapping requests", async () => {
    const probe = slowAdminApi();
    const { result } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
    await settle(16_000);
    expect(ids(result.current.adminRuns.runs)).toEqual(["r1"]);
    await settle(45_000);
    expect(probe.max()).toBe(1);
    expect(ids(result.current.adminRuns.runs).length).toBe(1);
    expect(result.current.adminRuns.runs?.[0].id).not.toBe("r1");
    expect(result.current.adminRuns.failed).toBe(false);
  });

  it("flags a slow failing response failed and keeps the last good rows", async () => {
    slowAdminApi(2);
    const { result } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
    await settle(16_000);
    expect(ids(result.current.adminRuns.runs)).toEqual(["r1"]);
    await settle(30_000);
    expect(ids(result.current.adminRuns.runs)).toEqual(["r1"]);
    expect(result.current.adminRuns.failed).toBe(true);
  });

  it("treats a request that never settles as failed, retries, and ignores the abandoned one", async () => {
    let resolveFirst: (v: { runs: ReturnType<typeof rows> }) => void = () => {};
    mockApi.adminListRuns.mockResolvedValueOnce({ runs: rows("a") });
    const { result } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
    await settle();
    expect(ids(result.current.adminRuns.runs)).toEqual(["a"]);
    const lastOkAt = result.current.adminRuns.lastOkAt;

    mockApi.adminListRuns.mockImplementationOnce(
      () => new Promise((resolve) => { resolveFirst = resolve; }),
    );
    await settle(10_000); // tick issues the request that never settles
    expect(mockApi.adminListRuns).toHaveBeenCalledTimes(2);
    expect(result.current.adminRuns.failed).toBe(false);

    mockApi.adminListRuns.mockResolvedValueOnce({ runs: rows("new") });
    await settle(30_000); // the tick at exactly the deadline retries
    expect(mockApi.adminListRuns.mock.calls.length).toBeGreaterThanOrEqual(3);
    // The replacement request succeeded, so failed is cleared and its rows applied.
    expect(ids(result.current.adminRuns.runs)).toEqual(["new"]);
    expect(result.current.adminRuns.failed).toBe(false);

    resolveFirst({ runs: rows("stale") });
    await settle();
    expect(ids(result.current.adminRuns.runs)).toEqual(["new"]);
    expect(result.current.adminRuns.lastOkAt).not.toBe(lastOkAt);
  });

  it("flags failed with last good rows retained while the replacement is still pending", async () => {
    mockApi.adminListRuns.mockResolvedValueOnce({ runs: rows("a") });
    const { result } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
    await settle();
    const lastOkAt = result.current.adminRuns.lastOkAt;
    mockApi.adminListRuns.mockImplementation(() => new Promise(() => {}));
    await settle(10_000);
    expect(result.current.adminRuns.failed).toBe(false);
    const callsBefore = mockApi.adminListRuns.mock.calls.length;
    await settle(30_000);
    expect(result.current.adminRuns.failed).toBe(true);
    expect(ids(result.current.adminRuns.runs)).toEqual(["a"]);
    expect(result.current.adminRuns.lastOkAt).toBe(lastOkAt);
    expect(mockApi.adminListRuns.mock.calls.length).toBeGreaterThan(callsBefore);
  });

  it("resets admin runs when admin access is revoked", async () => {
    mockApi.adminListRuns.mockResolvedValueOnce({ runs: rows("a") });
    const { result, rerender } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
    await settle();
    expect(ids(result.current.adminRuns.runs)).toEqual(["a"]);
    vi.mocked(useAuth).mockReturnValue(authValue(aUser));
    rerender();
    await settle();
    expect(result.current.adminRuns).toEqual({ runs: null, failed: false, lastOkAt: null });
  });

  it("never calls the admin endpoint for a non-admin, even when asked", async () => {
    vi.mocked(useAuth).mockReturnValue(authValue(aUser));
    const { result } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
    await settle(30_000);
    expect(mockApi.adminListRuns).not.toHaveBeenCalled();
    expect(result.current.adminRuns).toEqual({ runs: null, failed: false, lastOkAt: null });
  });
});
