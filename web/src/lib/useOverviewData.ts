// The Overview's data loading (PRD #3878, #3877): the first load, the 10 s runs/workers poll
// and the 30 s usage poll, plus the opt-in admin reads. The page chooses which opt-in reads it
// wants; with none, the hook sends only the personal requests.

import { useCallback, useEffect, useRef, useState } from "react";
import { useAuth } from "../auth/AuthContext";
import {
  api,
  type AdminUsage,
  type AgentTemplate,
  type AgentSourceView,
  type RecoveryCustodyHolds,
  type RepoActivity,
  type RunSummaryItem,
  type SelfUsage,
  type User,
  type Worker,
} from "./api";
import { hasAnthropicToken } from "./hasToken";
import type { UsageWindow } from "./usageByUser";
import { usePollWhileVisible } from "./usePollWhileVisible";

export interface Overview {
  runs: RunSummaryItem[];
  // The full fleet (PRD #49), refreshed by the mount load and the 10s poll.
  workers: Worker[];
  reposEnabled: number;
  // The same core read's rows, for the Overview Agents chip (#4335); no extra request.
  agentTemplates: AgentTemplate[];
  hasToken: boolean;
  hasForge: boolean;
  // PRD #40: the caller's own usage (everyone) + the factory view (admins only).
  // Best-effort: a usage fetch failure leaves these null and simply hides the cards,
  // never blocking the rest of the Overview.
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

// The admin's user list: last good users (null before any success) plus a failure flag.
export interface AdminUsersState {
  users: User[] | null;
  failed: boolean;
}

const NO_ADMIN_USERS: AdminUsersState = { users: null, failed: false };

// The admin repo-activity read (Repos chip, #4334): last good rows (null before any success)
// plus a failure flag.
export interface AdminRepoActivityState {
  repos: RepoActivity[] | null;
  failed: boolean;
}

const NO_ADMIN_REPO_ACTIVITY: AdminRepoActivityState = { repos: null, failed: false };

export function useOverviewData(opts?: {
  includeAdminRuns?: boolean;
  includeRecoveryHolds?: boolean;
  includeAdminUsers?: boolean;
  includeAdminRepoActivity?: boolean;
  includeAdminAgentSource?: boolean;
}) {
  const { user } = useAuth();
  const [data, setData] = useState<Overview | null>(null);
  // A failed volatile snapshot poll keeps last-good rows for every existing card, but the
  // default Overview must not claim "nothing needs you" from those stale run decisions.
  const [runsRefreshFailed, setRunsRefreshFailed] = useState(false);
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
  // admin-gated endpoint, and a caller passing no option sends only the personal requests.
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
          reposEnabled: repos.filter((r) => r.enabled).length,
          agentTemplates: templates,
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

  // Admin runs poll on their own cadence, NOT inside `poll`: usePollWhileVisible skips
  // ticks while a callback's promise is pending, so coupling them would let one hung
  // personal request stop admin polling and its deadline handling. This callback returns
  // immediately (fetchAdminRuns is fire-and-forget), so its own in-flight gate never blocks.
  const pollAdmin = useCallback(() => {
    if (adminRunsEnabled) void fetchAdminRuns();
  }, [adminRunsEnabled, fetchAdminRuns]);
  usePollWhileVisible(pollAdmin, 10000);

  // Held-work (custody) holds are opt-in per page and ride their own request and poll, never
  // the personal Promise.all or `poll`: a hung runs/workers request must not stop holds, and a
  // holds failure must not touch `data`. A failure keeps the last good value (null before any
  // success). Without the opt-in no request is made.
  const holdsEnabled = opts?.includeRecoveryHolds === true;
  const [holds, setHolds] = useState<RecoveryCustodyHolds | null>(null);
  const [holdsRequestFailed, setHoldsRequestFailed] = useState(false);
  // The mount fetch is outside the poll's in-flight gate, so a slow first response can settle after
  // a newer one. A response applies when it is newer than the last APPLIED success, not only when it
  // is the latest issued: a newer request that hangs or fails must not discard good data that did
  // arrive, while an older response never overwrites a newer one.
  const holdsRequestIdRef = useRef(0);
  const holdsAppliedIdRef = useRef(0);
  const holdsLatestSettledIdRef = useRef(0);
  const loadHolds = useCallback(async () => {
    if (!holdsEnabled) return;
    const id = ++holdsRequestIdRef.current;
    try {
      const next = await api.getRecoveryHolds();
      if (!pollAliveRef.current) return;
      if (id > holdsAppliedIdRef.current) {
        holdsAppliedIdRef.current = id;
        setHolds(next);
      }
      // An older success may supply last-good data after a newer failure, but it cannot
      // make the latest request read as healthy for the calm notice.
      if (id > holdsLatestSettledIdRef.current) {
        holdsLatestSettledIdRef.current = id;
        setHoldsRequestFailed(false);
      }
    } catch {
      // keep the last-good holds
      if (!pollAliveRef.current || id <= holdsLatestSettledIdRef.current) return;
      holdsLatestSettledIdRef.current = id;
      setHoldsRequestFailed(true);
    }
  }, [holdsEnabled]);
  useEffect(() => {
    void loadHolds();
  }, [loadHolds]);
  usePollWhileVisible(loadHolds, 10000);

  // The admin user list (People chip, #4042) is opt-in per page AND admin-only, and rides its
  // own request: once on mount and on the 30 s usage poll, never the 10 s poll. A failure keeps
  // the last good users but flags `failed` (the page treats failed as unavailable) and never
  // touches `data`, runs or usage. The same applied-id guard as holds: an older response never
  // overwrites a newer one that already landed.
  const usersEnabled = opts?.includeAdminUsers === true && user?.is_admin === true;
  const [adminUsers, setAdminUsers] = useState<AdminUsersState>(NO_ADMIN_USERS);
  const usersRequestIdRef = useRef(0);
  const usersAppliedIdRef = useRef(0);
  const loadUsers = useCallback(async () => {
    if (!usersEnabled) return;
    const id = ++usersRequestIdRef.current;
    try {
      const { users } = await api.listUsers();
      if (!pollAliveRef.current || id <= usersAppliedIdRef.current) return;
      usersAppliedIdRef.current = id;
      setAdminUsers({ users, failed: false });
    } catch {
      if (!pollAliveRef.current || id <= usersAppliedIdRef.current) return;
      setAdminUsers((prev) => ({ ...prev, failed: true }));
    }
  }, [usersEnabled]);
  useEffect(() => {
    if (!usersEnabled) {
      // Admin revoked (or never granted): drop the list and orphan pending requests.
      usersAppliedIdRef.current = usersRequestIdRef.current;
      setAdminUsers(NO_ADMIN_USERS);
      return;
    }
    void loadUsers();
  }, [usersEnabled, loadUsers]);

  // The admin repo-activity read (Repos chip, #4334) is opt-in per page AND admin-only. It is
  // fetched once on mount (and again if the enabled flag flips), never on a poll: the
  // "last run" ages it feeds are day-scale. A failure keeps the last good rows but flags
  // `failed`. The same applied-id guard as users: an older response never overwrites a newer one.
  const repoActivityEnabled = opts?.includeAdminRepoActivity === true && user?.is_admin === true;
  const [adminRepoActivity, setAdminRepoActivity] = useState<AdminRepoActivityState>(NO_ADMIN_REPO_ACTIVITY);
  const repoActivityRequestIdRef = useRef(0);
  const repoActivityAppliedIdRef = useRef(0);
  const loadRepoActivity = useCallback(async () => {
    if (!repoActivityEnabled) return;
    const id = ++repoActivityRequestIdRef.current;
    try {
      const { repos } = await api.adminRepoActivity();
      if (!pollAliveRef.current || id <= repoActivityAppliedIdRef.current) return;
      repoActivityAppliedIdRef.current = id;
      setAdminRepoActivity({ repos, failed: false });
    } catch {
      if (!pollAliveRef.current || id <= repoActivityAppliedIdRef.current) return;
      setAdminRepoActivity((prev) => ({ ...prev, failed: true }));
    }
  }, [repoActivityEnabled]);
  useEffect(() => {
    if (!repoActivityEnabled) {
      // Admin revoked (or never granted): drop the rows and orphan pending requests.
      repoActivityAppliedIdRef.current = repoActivityRequestIdRef.current;
      setAdminRepoActivity(NO_ADMIN_REPO_ACTIVITY);
      return;
    }
    void loadRepoActivity();
  }, [repoActivityEnabled, loadRepoActivity]);

  // The Overview Agents chip reads the admin agent-source status once, independent of the
  // core load and polls. A missing admin read affects only its optional sync details.
  // The ref keeps StrictMode's effect replay to one request per viewer mount.
  const agentSourceEnabled = opts?.includeAdminAgentSource === true && user?.is_admin === true;
  const [adminAgentSource, setAdminAgentSource] = useState<AgentSourceView | null>(null);
  const agentSourceRequestRef = useRef<Promise<AgentSourceView | null> | null>(null);
  useEffect(() => {
    if (!agentSourceEnabled) {
      agentSourceRequestRef.current = null;
      setAdminAgentSource(null);
      return;
    }
    agentSourceRequestRef.current ??= Promise.resolve()
      .then(() => api.getAgentSource())
      .then((result) => result.agent_source, () => null);
    let cancelled = false;
    void agentSourceRequestRef.current.then((view) => {
      if (!cancelled) setAdminAgentSource(view);
    });
    return () => { cancelled = true; };
  }, [agentSourceEnabled]);

  const poll = useCallback(async () => {
    try {
      const [{ runs }, { workers }] = await Promise.all([api.listRuns({ view: "summary" }), api.listWorkers()]);
      if (!pollAliveRef.current) return;
      setData((prev) =>
        prev
          ? {
              ...prev,
              runs,
              workers,
            }
          : prev,
      );
      setRunsRefreshFailed(false);
    } catch {
      // keep the last-good data
      if (pollAliveRef.current) setRunsRefreshFailed(true);
    }
  }, []);
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
    void loadUsers();
  }, [user?.is_admin, loadUsers]);
  usePollWhileVisible(pollUsage, 30_000);

  // holdsFailed: a request failed and there is no good data to show (only meaningful with the opt-in).
  return {
    data,
    usageWindow,
    changeUsageWindow,
    adminRuns,
    adminUsers,
    adminRepoActivity,
    adminAgentSource,
    holds,
    holdsFailed: holdsRequestFailed && holds === null,
    holdsRequestFailed,
    runsRefreshFailed,
  };
}
