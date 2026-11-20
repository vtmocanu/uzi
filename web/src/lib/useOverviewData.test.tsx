// @vitest-environment jsdom
// PRD #3878: the admin run list's loading rules inside the shared Overview data hook.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { StrictMode } from "react";
import { act, cleanup, renderHook } from "@testing-library/react";
import { api, type RecoveryCustodyHolds, type RunSummaryItem } from "./api";
import { useAuth } from "../auth/AuthContext";
import { useOverviewData } from "./useOverviewData";
import { aRun, aUser, authValue, emptySelf } from "../pages/overviewFixtures";
import { mockAgentSource } from "../mocks/data";

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
      getRecoveryHolds: vi.fn(),
      listUsers: vi.fn(),
      adminRepoActivity: vi.fn(),
      getAgentSource: vi.fn(),
      getTemplateAllocations: vi.fn(),
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
  mockApi.getRecoveryHolds.mockResolvedValue(holdsWith(1));
  mockApi.listUsers.mockResolvedValue({ users: [] });
  mockApi.adminRepoActivity.mockResolvedValue({ repos: [] });
  mockApi.getAgentSource.mockResolvedValue({ agent_source: structuredClone(mockAgentSource) });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.clearAllMocks();
});

function holdsWith(open: number): RecoveryCustodyHolds {
  return {
    aggregate: { open_holds: open, admission_counted_holds: open, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0 },
    holds: [],
  };
}

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

  // Hung request A at mount; the tick at the 30 s deadline issues B (callbacks of both returned).
  function hungThenPending() {
    const a: { resolve: (v: { runs: ReturnType<typeof rows> }) => void; reject: (e: Error) => void } = {
      resolve: () => {}, reject: () => {},
    };
    const b = { ...a };
    mockApi.adminListRuns
      .mockImplementationOnce(() => new Promise((res, rej) => { a.resolve = res; a.reject = rej; }))
      .mockImplementationOnce(() => new Promise((res, rej) => { b.resolve = res; b.reject = rej; }));
    return { a, b };
  }

  it("keeps the replacement's in-flight marker when the abandoned request settles", async () => {
    const { a } = hungThenPending();
    const { result } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
    await settle(30_000); // A abandoned at the deadline, B issued and still pending
    expect(mockApi.adminListRuns).toHaveBeenCalledTimes(2);
    a.resolve({ runs: rows("stale") });
    await settle(10_000); // next poll tick: B is still pending, so no third request
    expect(mockApi.adminListRuns).toHaveBeenCalledTimes(2);
    expect(result.current.adminRuns.runs).toBeNull();
  });

  it("does not let an abandoned request's failure flag the replacement's success", async () => {
    const { a, b } = hungThenPending();
    const { result } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
    await settle(30_000);
    b.resolve({ runs: rows("new") });
    await settle();
    expect(ids(result.current.adminRuns.runs)).toEqual(["new"]);
    expect(result.current.adminRuns.failed).toBe(false);
    a.reject(new Error("late"));
    await settle();
    expect(result.current.adminRuns.failed).toBe(false);
    expect(ids(result.current.adminRuns.runs)).toEqual(["new"]);
  });

  it("orphans a pending request on revoke so a re-grant fetches immediately", async () => {
    const pending: { resolve: (v: { runs: ReturnType<typeof rows> }) => void } = { resolve: () => {} };
    mockApi.adminListRuns.mockImplementationOnce(() => new Promise((res) => { pending.resolve = res; }));
    const { result, rerender } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
    await settle();
    expect(mockApi.adminListRuns).toHaveBeenCalledTimes(1);
    vi.mocked(useAuth).mockReturnValue(authValue(aUser));
    rerender();
    await settle();
    pending.resolve({ runs: rows("stale") });
    await settle();
    expect(result.current.adminRuns).toEqual({ runs: null, failed: false, lastOkAt: null });

    vi.mocked(useAuth).mockReturnValue(authValue(admin));
    rerender();
    await settle();
    expect(mockApi.adminListRuns).toHaveBeenCalledTimes(2);
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

  describe("when a personal request hangs after the first load", () => {
    it("keeps applying new admin rows on the 10 s ticks", async () => {
      let n = 0;
      mockApi.adminListRuns.mockImplementation(async () => ({ runs: rows(`r${++n}`) }));
      const { result } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
      await settle();
      expect(ids(result.current.adminRuns.runs)).toEqual(["r1"]);
      mockApi.listRuns.mockImplementation(() => new Promise(() => {}));
      await settle(10_000); // the personal poll hangs from here on
      await settle(10_000);
      await settle(10_000);
      expect(mockApi.listRuns.mock.calls.length).toBe(2); // later personal ticks are suppressed
      expect(ids(result.current.adminRuns.runs)).toEqual(["r4"]);
      expect(result.current.adminRuns.failed).toBe(false);
    });

    it("still fails a hung admin request at the 30 s deadline", async () => {
      mockApi.adminListRuns.mockResolvedValueOnce({ runs: rows("a") });
      const { result } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
      await settle();
      mockApi.listWorkers.mockImplementation(() => new Promise(() => {}));
      mockApi.adminListRuns.mockImplementation(() => new Promise(() => {}));
      await settle(10_000);
      expect(result.current.adminRuns.failed).toBe(false);
      await settle(30_000);
      expect(result.current.adminRuns.failed).toBe(true);
      expect(ids(result.current.adminRuns.runs)).toEqual(["a"]);
    });
  });

  it("never calls the admin endpoint for a non-admin, even when asked", async () => {
    vi.mocked(useAuth).mockReturnValue(authValue(aUser));
    const { result } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
    await settle(30_000);
    expect(mockApi.adminListRuns).not.toHaveBeenCalled();
    expect(result.current.adminRuns).toEqual({ runs: null, failed: false, lastOkAt: null });
  });
});

describe("useOverviewData admin users", () => {
  const user = (id: string) => ({ ...aUser, id, email: `${id}@uzi.local` });
  const emails = (users: { email: string }[] | null) => (users ?? []).map((u) => u.email);

  it("loads once on mount, skips the 10 s poll and reloads on the 30 s usage poll", async () => {
    mockApi.listUsers.mockResolvedValue({ users: [user("a")] });
    const { result } = renderHook(() => useOverviewData({ includeAdminUsers: true }));
    await settle();
    expect(mockApi.listUsers).toHaveBeenCalledTimes(1);
    expect(emails(result.current.adminUsers.users)).toEqual(["a@uzi.local"]);
    await settle(10_000);
    expect(mockApi.listUsers).toHaveBeenCalledTimes(1);
    mockApi.listUsers.mockResolvedValue({ users: [user("a"), user("b")] });
    await settle(20_000);
    expect(mockApi.listUsers).toHaveBeenCalledTimes(2);
    expect(emails(result.current.adminUsers.users)).toEqual(["a@uzi.local", "b@uzi.local"]);
  });

  it("never calls listUsers without the opt-in, or for a non-admin who asks", async () => {
    renderHook(() => useOverviewData({ includeAdminRuns: true }));
    renderHook(() => useOverviewData());
    await settle(30_000);
    expect(mockApi.listUsers).not.toHaveBeenCalled();
    vi.mocked(useAuth).mockReturnValue(authValue(aUser));
    const { result } = renderHook(() => useOverviewData({ includeAdminUsers: true }));
    await settle(30_000);
    expect(mockApi.listUsers).not.toHaveBeenCalled();
    expect(result.current.adminUsers).toEqual({ users: null, failed: false });
  });

  it("flags failed, keeps the last good users and leaves the other data alone", async () => {
    mockApi.listUsers.mockResolvedValueOnce({ users: [user("a")] });
    const { result } = renderHook(() => useOverviewData({ includeAdminUsers: true, includeAdminRuns: true }));
    await settle();
    const data = result.current.data;
    const adminRuns = result.current.adminRuns;
    expect(data).not.toBeNull();
    mockApi.listUsers.mockRejectedValue(new Error("down"));
    await settle(30_000);
    expect(result.current.adminUsers.failed).toBe(true);
    expect(emails(result.current.adminUsers.users)).toEqual(["a@uzi.local"]);
    expect(result.current.data?.selfUsage).not.toBeNull();
    expect(result.current.data?.runs).toEqual(data?.runs);
    expect(result.current.adminRuns.failed).toBe(false);
    expect(result.current.adminRuns.runs).toEqual(adminRuns.runs);
    mockApi.listUsers.mockResolvedValue({ users: [user("c")] });
    await settle(30_000);
    expect(result.current.adminUsers).toEqual({ users: [user("c")], failed: false });
  });

  it("does not let an older response overwrite a newer one", async () => {
    const first: { resolve: (v: { users: ReturnType<typeof user>[] }) => void } = { resolve: () => {} };
    mockApi.listUsers.mockImplementationOnce(() => new Promise((res) => { first.resolve = res; }));
    const { result } = renderHook(() => useOverviewData({ includeAdminUsers: true }));
    await settle();
    mockApi.listUsers.mockResolvedValue({ users: [user("new")] });
    await settle(30_000);
    expect(emails(result.current.adminUsers.users)).toEqual(["new@uzi.local"]);
    first.resolve({ users: [user("old")] });
    await settle();
    expect(emails(result.current.adminUsers.users)).toEqual(["new@uzi.local"]);
  });
});

describe("useOverviewData admin repo activity", () => {
  const row = (id: string) => ({ id, path_with_namespace: `org/${id}`, enabled: true, last_run_at: null });

  it("loads once on mount and on no poll", async () => {
    mockApi.adminRepoActivity.mockResolvedValue({ repos: [row("a")] });
    const { result } = renderHook(() => useOverviewData({ includeAdminRepoActivity: true }));
    await settle();
    expect(mockApi.adminRepoActivity).toHaveBeenCalledTimes(1);
    expect(result.current.adminRepoActivity).toEqual({ repos: [row("a")], failed: false });
    await settle(65_000);
    expect(mockApi.adminRepoActivity).toHaveBeenCalledTimes(1);
  });

  it("never calls the endpoint without the opt-in, or for a non-admin who asks", async () => {
    renderHook(() => useOverviewData({ includeAdminRuns: true, includeAdminUsers: true }));
    renderHook(() => useOverviewData());
    await settle(30_000);
    expect(mockApi.adminRepoActivity).not.toHaveBeenCalled();
    vi.mocked(useAuth).mockReturnValue(authValue(aUser));
    const { result } = renderHook(() => useOverviewData({ includeAdminRepoActivity: true }));
    await settle(30_000);
    expect(mockApi.adminRepoActivity).not.toHaveBeenCalled();
    expect(result.current.adminRepoActivity).toEqual({ repos: null, failed: false });
  });

  it("flags failed with no rows, and leaves the other data alone", async () => {
    mockApi.adminRepoActivity.mockRejectedValue(new Error("down"));
    const { result } = renderHook(() => useOverviewData({ includeAdminRepoActivity: true, includeAdminRuns: true }));
    await settle();
    expect(result.current.adminRepoActivity).toEqual({ repos: null, failed: true });
    expect(result.current.data).not.toBeNull();
    expect(result.current.adminRuns.failed).toBe(false);
  });

  it("drops the rows on revoke and fetches again on a re-grant", async () => {
    mockApi.adminRepoActivity.mockResolvedValue({ repos: [row("a")] });
    const { result, rerender } = renderHook(() => useOverviewData({ includeAdminRepoActivity: true }));
    await settle();
    expect(result.current.adminRepoActivity.repos).toEqual([row("a")]);
    vi.mocked(useAuth).mockReturnValue(authValue(aUser));
    rerender();
    await settle();
    expect(result.current.adminRepoActivity).toEqual({ repos: null, failed: false });
    vi.mocked(useAuth).mockReturnValue(authValue(admin));
    rerender();
    await settle();
    expect(mockApi.adminRepoActivity).toHaveBeenCalledTimes(2);
    expect(result.current.adminRepoActivity.repos).toEqual([row("a")]);
  });
});

describe("useOverviewData admin agent source", () => {
  it("reads once under StrictMode, independently of both polls", async () => {
    const { result } = renderHook(() => useOverviewData({ includeAdminAgentSource: true }), { wrapper: StrictMode });
    await settle();
    expect(mockApi.getAgentSource).toHaveBeenCalledTimes(1);
    expect(result.current.adminAgentSource?.config.enabled).toBe(true);
    await settle(65_000);
    expect(mockApi.getAgentSource).toHaveBeenCalledTimes(1);
  });

  it("never calls the admin read without the opt-in or for a non-admin", async () => {
    renderHook(() => useOverviewData());
    await settle();
    expect(mockApi.getAgentSource).not.toHaveBeenCalled();
    vi.mocked(useAuth).mockReturnValue(authValue(aUser));
    const { result } = renderHook(() => useOverviewData({ includeAdminAgentSource: true }));
    await settle();
    expect(mockApi.getAgentSource).not.toHaveBeenCalled();
    expect(result.current.adminAgentSource).toBeNull();
  });

  it("fails without blanking the core Overview data", async () => {
    mockApi.getAgentSource.mockRejectedValue(new Error("down"));
    const { result } = renderHook(() => useOverviewData({ includeAdminAgentSource: true }));
    await settle();
    expect(result.current.adminAgentSource).toBeNull();
    expect(result.current.data).not.toBeNull();
  });
});

describe("useOverviewData recovery holds", () => {
  it("makes no holds request and returns null without the opt-in", async () => {
    const { result } = renderHook(() => useOverviewData({ includeAdminRuns: true }));
    await settle(30_000);
    expect(mockApi.getRecoveryHolds).not.toHaveBeenCalled();
    expect(result.current.holds).toBeNull();
    const bare = renderHook(() => useOverviewData());
    await settle(30_000);
    expect(mockApi.getRecoveryHolds).not.toHaveBeenCalled();
    expect(bare.result.current.holds).toBeNull();
  });

  it("loads holds once on mount and again each 10 s tick", async () => {
    const { result } = renderHook(() => useOverviewData({ includeRecoveryHolds: true }));
    await settle();
    expect(mockApi.getRecoveryHolds).toHaveBeenCalledTimes(1);
    expect(result.current.holds?.aggregate.open_holds).toBe(1);
    mockApi.getRecoveryHolds.mockResolvedValue(holdsWith(4));
    await settle(10_000);
    expect(mockApi.getRecoveryHolds).toHaveBeenCalledTimes(2);
    expect(result.current.holds?.aggregate.open_holds).toBe(4);
  });

  it("loads holds even while the personal requests never settle", async () => {
    mockApi.listRuns.mockImplementation(() => new Promise(() => {}));
    const { result } = renderHook(() => useOverviewData({ includeRecoveryHolds: true }));
    await settle();
    expect(result.current.data).toBeNull();
    expect(result.current.holds?.aggregate.open_holds).toBe(1);
    mockApi.getRecoveryHolds.mockResolvedValue(holdsWith(2));
    await settle(10_000);
    expect(result.current.holds?.aggregate.open_holds).toBe(2);
  });

  it("keeps the last good holds after a failed poll, and recovers on the next success", async () => {
    const { result } = renderHook(() => useOverviewData({ includeRecoveryHolds: true }));
    await settle();
    mockApi.getRecoveryHolds.mockRejectedValueOnce(new Error("blip"));
    await settle(10_000);
    expect(mockApi.getRecoveryHolds).toHaveBeenCalledTimes(2);
    expect(result.current.holds?.aggregate.open_holds).toBe(1);
    expect(result.current.holdsRequestFailed).toBe(true);
    mockApi.getRecoveryHolds.mockResolvedValue(holdsWith(3));
    await settle(10_000);
    expect(result.current.holds?.aggregate.open_holds).toBe(3);
    expect(result.current.holdsRequestFailed).toBe(false);
  });

  it("stays null when the first load fails, without touching the personal data", async () => {
    mockApi.getRecoveryHolds.mockRejectedValue(new Error("down"));
    const { result } = renderHook(() => useOverviewData({ includeRecoveryHolds: true }));
    await settle();
    expect(result.current.holds).toBeNull();
    expect(result.current.data).not.toBeNull();
  });

  it("does not let a hung holds request stop the runs poll", async () => {
    mockApi.getRecoveryHolds.mockImplementation(() => new Promise(() => {}));
    renderHook(() => useOverviewData({ includeRecoveryHolds: true }));
    await settle();
    await settle(10_000);
    expect(mockApi.listRuns).toHaveBeenCalledTimes(2);
  });

  it("keeps the newer holds when an older request settles after a newer one", async () => {
    const first: { resolve: (v: RecoveryCustodyHolds) => void } = { resolve: () => {} };
    mockApi.getRecoveryHolds.mockImplementationOnce(() => new Promise((res) => { first.resolve = res; }));
    const { result } = renderHook(() => useOverviewData({ includeRecoveryHolds: true }));
    await settle();
    // The mount request is still pending; a later tick issues a newer one that settles first.
    mockApi.getRecoveryHolds.mockResolvedValue(holdsWith(4));
    await settle(10_000);
    expect(mockApi.getRecoveryHolds).toHaveBeenCalledTimes(2);
    expect(result.current.holds?.aggregate.open_holds).toBe(4);
    first.resolve(holdsWith(1));
    await settle();
    expect(result.current.holds?.aggregate.open_holds).toBe(4);
  });

  it("ignores a superseded request's failure", async () => {
    const first: { reject: (e: Error) => void } = { reject: () => {} };
    mockApi.getRecoveryHolds.mockImplementationOnce(() => new Promise((_, rej) => { first.reject = rej; }));
    const { result } = renderHook(() => useOverviewData({ includeRecoveryHolds: true }));
    await settle();
    mockApi.getRecoveryHolds.mockResolvedValue(holdsWith(2));
    await settle(10_000);
    first.reject(new Error("late"));
    await settle();
    expect(result.current.holds?.aggregate.open_holds).toBe(2);
    expect(result.current.holdsFailed).toBe(false);
  });

  it("applies an older success while a newer request is still pending", async () => {
    const first: { resolve: (v: RecoveryCustodyHolds) => void } = { resolve: () => {} };
    mockApi.getRecoveryHolds.mockImplementationOnce(() => new Promise((res) => { first.resolve = res; }));
    const { result } = renderHook(() => useOverviewData({ includeRecoveryHolds: true }));
    await settle();
    // The poll's newer request never settles; the slow mount request then succeeds.
    mockApi.getRecoveryHolds.mockImplementation(() => new Promise(() => {}));
    await settle(10_000);
    expect(mockApi.getRecoveryHolds).toHaveBeenCalledTimes(2);
    first.resolve(holdsWith(3));
    await settle();
    expect(result.current.holds?.aggregate.open_holds).toBe(3);
    expect(result.current.holdsFailed).toBe(false);
    expect(result.current.holdsRequestFailed).toBe(false); // newer request is pending, not failed
  });

  it("applies an older success that lands after a newer request failed, with no holds yet", async () => {
    const first: { resolve: (v: RecoveryCustodyHolds) => void } = { resolve: () => {} };
    mockApi.getRecoveryHolds.mockImplementationOnce(() => new Promise((res) => { first.resolve = res; }));
    const { result } = renderHook(() => useOverviewData({ includeRecoveryHolds: true }));
    await settle();
    mockApi.getRecoveryHolds.mockRejectedValueOnce(new Error("502"));
    await settle(10_000);
    expect(result.current.holds).toBeNull();
    expect(result.current.holdsFailed).toBe(true);
    first.resolve(holdsWith(3));
    await settle();
    expect(result.current.holds?.aggregate.open_holds).toBe(3);
    expect(result.current.holdsFailed).toBe(false);
    expect(result.current.holdsRequestFailed).toBe(true); // newer failed request remains latest
  });

  it("ignores an older failure that lands after a newer success", async () => {
    const first: { reject: (e: Error) => void } = { reject: () => {} };
    mockApi.getRecoveryHolds.mockImplementationOnce(() => new Promise((_, rej) => { first.reject = rej; }));
    const { result } = renderHook(() => useOverviewData({ includeRecoveryHolds: true }));
    await settle();
    // A newer request is pending too, so holds are still empty when the older one fails.
    const second: { resolve: (v: RecoveryCustodyHolds) => void } = { resolve: () => {} };
    mockApi.getRecoveryHolds.mockImplementationOnce(() => new Promise((res) => { second.resolve = res; }));
    await settle(10_000);
    second.resolve(holdsWith(2));
    await settle();
    first.reject(new Error("late"));
    await settle();
    expect(result.current.holds?.aggregate.open_holds).toBe(2);
    expect(result.current.holdsFailed).toBe(false);
  });

  it("reports holdsFailed only when a request failed and no holds exist, and clears it on success", async () => {
    mockApi.getRecoveryHolds.mockRejectedValueOnce(new Error("down"));
    const { result } = renderHook(() => useOverviewData({ includeRecoveryHolds: true }));
    expect(result.current.holdsFailed).toBe(false); // before the first response
    await settle();
    expect(result.current.holds).toBeNull();
    expect(result.current.holdsFailed).toBe(true);
    await settle(10_000); // default mock resolves
    expect(result.current.holds?.aggregate.open_holds).toBe(1);
    expect(result.current.holdsFailed).toBe(false);
    mockApi.getRecoveryHolds.mockRejectedValueOnce(new Error("blip"));
    await settle(10_000);
    expect(result.current.holds?.aggregate.open_holds).toBe(1); // last good kept
    expect(result.current.holdsFailed).toBe(false);
  });

  it("keeps holds intact and refreshing when the runs/workers poll rejects", async () => {
    const { result } = renderHook(() => useOverviewData({ includeRecoveryHolds: true }));
    await settle();
    expect(result.current.holds?.aggregate.open_holds).toBe(1);
    mockApi.listRuns.mockRejectedValue(new Error("runs down"));
    mockApi.listWorkers.mockRejectedValue(new Error("workers down"));
    await settle(10_000);
    expect(result.current.data).not.toBeNull();
    expect(result.current.holds?.aggregate.open_holds).toBe(1);
    expect(result.current.runsRefreshFailed).toBe(true);
    mockApi.getRecoveryHolds.mockResolvedValue(holdsWith(6));
    await settle(10_000);
    expect(result.current.holds?.aggregate.open_holds).toBe(6);
  });
});

describe("useOverviewData personal data and usage", () => {
  const zeroOutcomes = () => ({ finished: 0, completed: 0, cancelled: 0, plan_rejected: 0, base_diverged: 0, failed: 0, needs_landing: 0, last_failed_at: null, last_failed_run_id: null, last_failed_origin: null, last_failed_user_id: null, completed_since_last_failure: null, fail_origins: {} });
  // A self-usage payload told apart by its lifetime completed count.
  const usageWith = (completed: number) => {
    const base = emptySelf();
    return { ...base, outcomes: { ...base.outcomes, lifetime: { ...zeroOutcomes(), finished: completed, completed } } };
  };
  const completedOf = (u: { outcomes: { lifetime: { completed: number } } } | null | undefined) => u?.outcomes.lifetime.completed;

  it("keeps data null when the first load fails, and keeps the last good data when a re-poll fails (#2661 split)", async () => {
    mockApi.listRuns.mockRejectedValueOnce(new Error("api down"));
    const { result } = renderHook(() => useOverviewData());
    await settle();
    expect(result.current.data).toBeNull();
    cleanup();
    const second = renderHook(() => useOverviewData());
    await settle();
    expect(second.result.current.data?.runs).toEqual([]);
    mockApi.listRuns.mockRejectedValueOnce(new Error("poll blip"));
    await settle(10_000);
    expect(second.result.current.data?.runs).toEqual([]);
    expect(second.result.current.runsRefreshFailed).toBe(true);
  });

  it("requests the summary projection on the first load and on every poll tick (#2661)", async () => {
    renderHook(() => useOverviewData());
    await settle();
    await settle(10_000);
    expect(mockApi.listRuns.mock.calls.length).toBeGreaterThanOrEqual(2);
    for (const call of mockApi.listRuns.mock.calls) expect(call[0]).toEqual({ view: "summary" });
  });

  it("sends exactly the personal first-load requests for a non-admin, with no admin request", async () => {
    vi.mocked(useAuth).mockReturnValue(authValue(aUser));
    renderHook(() => useOverviewData());
    await settle();
    for (const call of [mockApi.listRuns, mockApi.listWorkers, mockApi.listRepos, mockApi.listAgentTemplates, mockApi.listSecrets, mockApi.listConnections, mockApi.getUsage]) {
      expect(call).toHaveBeenCalledTimes(1);
    }
    for (const call of [mockApi.getAdminUsage, mockApi.adminListRuns, mockApi.listUsers, mockApi.getRecoveryHolds, mockApi.adminRepoActivity, mockApi.getAgentSource]) {
      expect(call).not.toHaveBeenCalled();
    }
  });

  it("loads admin usage for an admin and never the admin run list without the opt-in", async () => {
    renderHook(() => useOverviewData());
    await settle();
    await settle(10_000);
    expect(mockApi.getAdminUsage).toHaveBeenCalledTimes(1);
    expect(mockApi.listRuns).toHaveBeenCalledTimes(2);
    expect(mockApi.listWorkers).toHaveBeenCalledTimes(2);
    expect(mockApi.listRepos).toHaveBeenCalledTimes(1);
    expect(mockApi.adminListRuns).not.toHaveBeenCalled();
  });

  it("never reads the template allocations, on first load or on a poll (#4335)", async () => {
    renderHook(() => useOverviewData());
    await settle(30_000);
    expect(mockApi.listRuns.mock.calls.length).toBeGreaterThan(1);
    expect(mockApi.getTemplateAllocations).not.toHaveBeenCalled();
  });

  it("refreshes usage on its own 30 s cadence even when the runs poll fails (#2399)", async () => {
    mockApi.getUsage.mockResolvedValue(usageWith(14));
    const { result } = renderHook(() => useOverviewData());
    await settle();
    expect(completedOf(result.current.data?.selfUsage)).toBe(14);
    mockApi.listRuns.mockRejectedValue(new Error("runs unavailable"));
    mockApi.getUsage.mockResolvedValue(usageWith(0));
    await settle(30_000);
    expect(completedOf(result.current.data?.selfUsage)).toBe(0);
    expect(mockApi.getUsage).toHaveBeenCalledTimes(2);
  });

  it.each(["self", "admin"] as const)("retains the failed %s usage payload while refreshing the other scope (#2399)", async (failed) => {
    mockApi.getUsage.mockResolvedValue(usageWith(14));
    mockApi.getAdminUsage.mockResolvedValue({ factory: usageWith(31), users: [], earliest_run: null });
    const { result } = renderHook(() => useOverviewData());
    await settle();
    if (failed === "self") {
      mockApi.getUsage.mockRejectedValue(new Error("self blip"));
      mockApi.getAdminUsage.mockResolvedValue({ factory: usageWith(0), users: [], earliest_run: null });
    } else {
      mockApi.getAdminUsage.mockRejectedValue(new Error("admin blip"));
      mockApi.getUsage.mockResolvedValue(usageWith(0));
    }
    await settle(30_000);
    expect(completedOf(result.current.data?.selfUsage)).toBe(failed === "self" ? 14 : 0);
    expect(completedOf(result.current.data?.adminUsage?.factory)).toBe(failed === "admin" ? 31 : 0);
    expect(result.current.data?.runs).toEqual([]);
  });

  it("skips hidden usage ticks and refreshes when the tab becomes visible (#2399)", async () => {
    renderHook(() => useOverviewData());
    await settle();
    Object.defineProperty(document, "hidden", { configurable: true, get: () => true });
    await settle(30_000);
    expect(mockApi.getUsage).toHaveBeenCalledTimes(1);
    Object.defineProperty(document, "hidden", { configurable: true, get: () => false });
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    expect(mockApi.getUsage).toHaveBeenCalledTimes(2);
  });
});

describe("useOverviewData usage window (PRD #40)", () => {
  it("defaults to seven days, persists a change, and reads it back on the next mount", async () => {
    const first = renderHook(() => useOverviewData());
    await settle();
    expect(first.result.current.usageWindow).toBe("last_7_days");
    act(() => first.result.current.changeUsageWindow("lifetime"));
    expect(first.result.current.usageWindow).toBe("lifetime");
    expect(localStorage.getItem("uzi.usageWindow")).toBe("lifetime");
    first.unmount();
    const second = renderHook(() => useOverviewData());
    await settle();
    expect(second.result.current.usageWindow).toBe("lifetime");
    act(() => second.result.current.changeUsageWindow("last_7_days"));
    expect(localStorage.getItem("uzi.usageWindow")).toBe("last_7_days");
  });

  it("falls back to seven days on an invalid stored value", async () => {
    localStorage.setItem("uzi.usageWindow", "unsupported");
    const { result } = renderHook(() => useOverviewData());
    await settle();
    expect(result.current.usageWindow).toBe("last_7_days");
  });

  it("survives blocked storage reads and writes without blocking the switch", async () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("storage blocked");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("storage blocked");
    });
    const { result } = renderHook(() => useOverviewData());
    await settle();
    expect(result.current.usageWindow).toBe("last_7_days");
    act(() => result.current.changeUsageWindow("lifetime"));
    expect(result.current.usageWindow).toBe("lifetime");
    vi.restoreAllMocks();
  });
});
