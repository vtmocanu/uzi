// The Overview's data loading, shared by Dashboard and the /overview/next preview
// (PRD #3878): the first load, the 10 s runs/workers poll and the 30 s usage poll that
// used to live in Dashboard.tsx, moved here unchanged so the two pages cannot drift in
// what they fetch or how they poll.

import { useCallback, useEffect, useRef, useState } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, type AdminUsage, type RunSummaryItem, type SelfUsage, type Worker } from "./api";
import { hasAnthropicToken } from "./hasToken";
import { usePollWhileVisible } from "./usePollWhileVisible";
import type { UsageWindow } from "../components/UsageCards";

export interface Overview {
  runs: RunSummaryItem[];
  // The full fleet drives both the online/total count and the per-worker load lines
  // (PRD #49); the mount load and the 10s poll both already fetch it.
  workers: Worker[];
  workersOnline: number;
  workersTotal: number;
  reposEnabled: number;
  templates: number;
  hasToken: boolean;
  hasForge: boolean;
  // PRD #40: the caller's own usage (everyone) + the factory view (admins only).
  // Best-effort: a usage fetch failure leaves these null and simply hides the cards,
  // never blocking the rest of the dashboard.
  selfUsage: SelfUsage | null;
  adminUsage: AdminUsage | null;
}

// The admin's all-users run list, loaded INDEPENDENTLY of the personal data above:
// a failure keeps the last good rows (null before any success) and flags `failed`, and
// never touches `data`.
export interface AdminRunsState {
  runs: RunSummaryItem[] | null;
  failed: boolean;
  lastOkAt: number | null;
}

// PRD #40: a browser preference only, never authoritative for usage or access.
const USAGE_WINDOW_KEY = "uzi.usageWindow";

function readUsageWindow(): UsageWindow {
  try {
    const stored = window.localStorage.getItem(USAGE_WINDOW_KEY);
    return stored === "lifetime" ? "lifetime" : "last_7_days";
  } catch {
    return "last_7_days";
  }
}

// How long an admin run-list request may stay pending before it counts as failed.
const ADMIN_RUNS_DEADLINE_MS = 30_000;

const NO_ADMIN_RUNS: AdminRunsState = { runs: null, failed: false, lastOkAt: null };

export function useOverviewData(opts?: { includeAdminRuns?: boolean }) {
  const { user } = useAuth();
  const [data, setData] = useState<Overview | null>(null);
  const [adminRuns, setAdminRuns] = useState<AdminRunsState>(NO_ADMIN_RUNS);
  const [usageWindow, setUsageWindow] = useState<UsageWindow>(readUsageWindow);
  const changeUsageWindow = useCallback((value: UsageWindow) => {
    setUsageWindow(value);
    try {
      window.localStorage.setItem(USAGE_WINDOW_KEY, value);
    } catch {
      // Storage restrictions must not prevent the current view from switching.
    }
  }, []);
  // Admin runs are opt-in per page AND admin-only: a non-admin never calls the
  // admin-gated endpoint, and Dashboard (no option) sends exactly its historic requests.
  const adminRunsEnabled = opts?.includeAdminRuns === true && user?.is_admin === true;

  // First load fetches everything — volatile tiles plus the rarely-changing
  // fields the onboarding checklist reads. An error here keeps `data` null so the
  // skeletons stay (skeletons only ever show pre-first-load); the background poll
  // below never reaches this path.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const [{ runs }, { workers }, { repos }, { templates }, { secrets }, { connections }, selfUsage, adminUsage] =
          await Promise.all([
            api.listRuns({ view: "summary" }),
            api.listWorkers(),
            api.listRepos(),
            api.listAgentTemplates(),
            api.listSecrets(),
            api.listConnections(),
            // Usage is best-effort (.catch → null) so it can never fail the core load,
            // and the admin view is fetched ONLY for an admin — a non-admin never hits
            // the admin-gated endpoint.
            api.getUsage().catch(() => null),
            user?.is_admin ? api.getAdminUsage().catch(() => null) : Promise.resolve(null),
          ]);
        if (cancelled) return;
        setData({
          runs,
          workers,
          workersOnline: workers.filter((w) => w.status === "online").length,
          workersTotal: workers.length,
          reposEnabled: repos.filter((r) => r.enabled).length,
          templates: templates.length,
          hasToken: hasAnthropicToken(secrets),
          hasForge: connections.length > 0,
          selfUsage,
          adminUsage,
        });
      } catch {
        if (!cancelled) setData(null);
      }
    })();
    return () => {
      cancelled = true;
    };
    // Deps list `user?.is_admin` (the value the effect branches on at the
    // getAdminUsage call above), NOT the whole `user` object. ProtectedRoute renders
    // this page only once auth has resolved to a non-null user, so is_admin holds its
    // final value at mount. On the ordinary focus/vault-lock refresh path it does not
    // flip: those replace the user object but re-read the same is_admin. React compares
    // deps with Object.is per element, so listing the boolean means the effect re-runs
    // ONLY if is_admin actually changes value — which on the normal path is never, so
    // it still runs exactly once. (The one case it would change — an admin grant/revoke
    // by another admin, then a refresh — correctly re-loads the dashboard rather than
    // showing stale admin/non-admin data, so the fix is safe there too.) Listing the
    // whole `user` object instead WOULD churn on every refresh; that is why it is the
    // boolean, not the object. Resolved #200 (the M3 exhaustive-deps review).
  }, [user?.is_admin]);

  // Liveness: re-fetch only the volatile endpoints (runs, workers) every 10s while
  // the tab is visible, so a run moving to awaiting_approval or a worker dropping
  // offline shows without a reload. Repos/templates/secrets/connections change
  // rarely and stay mount-only. A poll failure keeps the last-good data — unlike
  // the first load, a transient re-poll error must NOT blank the page back to
  // skeletons.
  const pollAliveRef = useRef(false);
  useEffect(() => {
    pollAliveRef.current = true;
    return () => { pollAliveRef.current = false; };
  }, []);

  // The admin list rides its own request on both the first load and the 10 s poll, caught
  // separately so its failure can never fail the personal load. A failure keeps the last
  // good rows and flags `failed`; the next success clears the flag and stamps the read time.
  // Normally one request at a time: a tick that finds one pending skips, so a response
  // slower than the poll interval is still applied (never starved by newer requests).
  // The api client has no request timeout, so a request that never settles must not pin
  // the marker forever: a tick that finds one older than ADMIN_RUNS_DEADLINE_MS counts it
  // as failed and issues a new request. The stuck one is abandoned, not aborted (the api
  // client has no abort signal), so two requests can then be in flight; only the latest
  // issued id may write state or clear the marker, so the abandoned one settling later
  // changes nothing. A server consistently slower than the deadline therefore keeps
  // `failed` true.
  const adminInFlightRef = useRef<{ id: number; startedAt: number } | null>(null);
  const adminRequestIdRef = useRef(0);
  const fetchAdminRuns = useCallback(async () => {
    const now = Date.now();
    const pending = adminInFlightRef.current;
    if (pending) {
      if (now - pending.startedAt < ADMIN_RUNS_DEADLINE_MS) return;
      if (pollAliveRef.current) setAdminRuns((prev) => ({ ...prev, failed: true }));
    }
    const id = ++adminRequestIdRef.current;
    adminInFlightRef.current = { id, startedAt: now };
    try {
      const { runs } = await api.adminListRuns({ view: "summary" });
      if (!pollAliveRef.current || adminInFlightRef.current?.id !== id) return;
      setAdminRuns({ runs, failed: false, lastOkAt: Date.now() });
    } catch {
      if (!pollAliveRef.current || adminInFlightRef.current?.id !== id) return;
      setAdminRuns((prev) => ({ ...prev, failed: true }));
    } finally {
      if (adminInFlightRef.current?.id === id) adminInFlightRef.current = null;
    }
  }, []);
  useEffect(() => {
    if (!adminRunsEnabled) {
      // Admin revoked (or never granted): drop any rows and orphan a pending request so a
      // later re-grant starts empty instead of showing stale rows as live.
      adminInFlightRef.current = null;
      setAdminRuns(NO_ADMIN_RUNS);
      return;
    }
    void fetchAdminRuns();
  }, [adminRunsEnabled, fetchAdminRuns]);

  const poll = useCallback(async () => {
    if (adminRunsEnabled) void fetchAdminRuns();
    try {
      const [{ runs }, { workers }] = await Promise.all([api.listRuns({ view: "summary" }), api.listWorkers()]);
      if (!pollAliveRef.current) return;
      setData((prev) =>
        prev
          ? {
              ...prev,
              runs,
              workers,
              workersOnline: workers.filter((w) => w.status === "online").length,
              workersTotal: workers.length,
            }
          : prev,
      );
    } catch {
      // keep the last-good data
    }
  }, [adminRunsEnabled, fetchAdminRuns]);
  usePollWhileVisible(poll, 10000);
  // #2399: usage has its own cadence and failure handling. Each successful
  // endpoint updates immediately; a failure retains only its own last-good data.
  const pollUsage = useCallback(() => {
    void api.getUsage().then((selfUsage) => {
      setData((prev) => prev ? { ...prev, selfUsage } : prev);
    }).catch(() => {});
    if (user?.is_admin) {
      void api.getAdminUsage().then((adminUsage) => {
        setData((prev) => prev ? { ...prev, adminUsage } : prev);
      }).catch(() => {});
    }
  }, [user?.is_admin]);
  usePollWhileVisible(pollUsage, 30_000);

  return { data, usageWindow, changeUsageWindow, adminRuns };
}
