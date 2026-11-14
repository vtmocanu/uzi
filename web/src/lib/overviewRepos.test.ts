import { describe, expect, it } from "vitest";
import type { RepoActivity, RunStatus, RunSummaryItem } from "./api";
import { ADMIN_RUNS_CAP } from "./overviewPeople";
import { repoActivityModel, repoIdleText } from "./overviewRepos";

const NOW = Date.parse("2026-11-08T12:00:00Z");
const HOUR = 3600 * 1000;
const DAY = 24 * HOUR;
const ME = "me@x.io";

let seq = 0;
function run(over: Partial<RunSummaryItem> & { status: RunStatus }): RunSummaryItem {
  return { id: `r-${++seq}`, health: "ok", repo_id: "repo-a", repo_path: "org/alpha", ...over } as RunSummaryItem;
}

function repo(id: string, path: string, over: Partial<RepoActivity> = {}): RepoActivity {
  return { id, path_with_namespace: path, enabled: true, last_run_at: null, ...over };
}

function ago(ms: number): string {
  return new Date(NOW - ms).toISOString();
}

type Input = Parameters<typeof repoActivityModel>[0];

function model(over: Partial<Input> = {}) {
  return repoActivityModel({
    ownRuns: [],
    adminRuns: { runs: [], failed: false, lastOkAt: null },
    viewerEmail: ME,
    repoActivity: { repos: [], failed: false },
    now: NOW,
    demo: false,
    ...over,
  });
}

function admin(runs: RunSummaryItem[]) {
  return { runs, failed: false, lastOkAt: NOW };
}

function activeOf(m: ReturnType<typeof model>) {
  if (m.active.state !== "ok") throw new Error(`active is ${m.active.state}`);
  return m.active;
}

describe("active repos", () => {
  it("follows runPart: terminal runs and rows without a repo are skipped", () => {
    const m = model({
      adminRuns: admin([
        run({ status: "running" }),
        run({ status: "completed", repo_id: "repo-b", repo_path: "org/beta" }),
        run({ status: "failed", repo_id: "repo-b", repo_path: "org/beta" }),
        run({ status: "running", repo_id: null, repo_path: "" }),
      ]),
      repoActivity: { repos: [repo("repo-a", "org/alpha"), repo("repo-b", "org/beta")], failed: false },
    });
    const a = activeOf(m);
    expect(a.count).toBe(1);
    expect(a.repos.map((r) => r.key)).toEqual(["repo-a"]);
  });

  it("counts the viewer's own runs and admin runs, each id once", () => {
    const own = run({ status: "running", id: "same" });
    const m = model({
      ownRuns: [own],
      adminRuns: admin([{ ...own }, run({ status: "queued" })]),
    });
    const a = activeOf(m);
    expect(a.repos).toHaveLength(1);
    expect(a.repos[0].parts).toEqual({ running: 1, queued: 1, waiting: 0, owner: 0 });
    expect(a.repos[0].pill).toBe("1 running · 1 queued");
    expect(a.repos[0].tone).toBe("ok");
  });

  it("orders by most running, then by path", () => {
    const m = model({
      adminRuns: admin([
        run({ status: "running", repo_id: "r-z", repo_path: "org/zed" }),
        run({ status: "queued", repo_id: "r-b", repo_path: "org/bravo" }),
        run({ status: "running", repo_id: "r-a", repo_path: "org/alpha" }),
        run({ status: "running", repo_id: "r-z", repo_path: "org/zed" }),
        run({ status: "queued", repo_id: "r-c", repo_path: "org/able" }),
      ]),
    });
    expect(activeOf(m).repos.map((r) => r.path)).toEqual(["org/zed", "org/alpha", "org/able", "org/bravo"]);
  });

  it("derives a two-character badge from the last path segment, case kept", () => {
    const m = model({
      adminRuns: admin([
        run({ status: "running", repo_id: "r-1", repo_path: "Org/Uzi-api" }),
        run({ status: "queued", repo_id: "r-2", repo_path: "org/--" }),
      ]),
    });
    const byKey = Object.fromEntries(activeOf(m).repos.map((r) => [r.key, r.badge]));
    expect(byKey["r-1"]).toBe("Uz");
    expect(byKey["r-2"]).toBe("?");
  });

  it("prefers the read's path over the run's repo_path", () => {
    const m = model({
      adminRuns: admin([run({ status: "running", repo_path: "old/name" })]),
      repoActivity: { repos: [repo("repo-a", "org/renamed")], failed: false },
    });
    expect(activeOf(m).repos[0].path).toBe("org/renamed");
  });
});

describe("degradation", () => {
  it("is unavailable when the admin run list failed, loading while it is null", () => {
    expect(model({ adminRuns: { runs: null, failed: true, lastOkAt: null } }).active).toEqual({ state: "unavailable" });
    const loading = model({ adminRuns: { runs: null, failed: false, lastOkAt: null } });
    expect(loading.active).toEqual({ state: "loading" });
    expect(loading.recent).toBeNull();
    expect(loading.idle).toBeNull();
  });

  it("keeps recent and idle null when the runs are unavailable", () => {
    const m = model({
      adminRuns: { runs: null, failed: true, lastOkAt: null },
      repoActivity: { repos: [repo("repo-a", "org/alpha")], failed: false },
    });
    expect(m.recent).toBeNull();
    expect(m.idle).toBeNull();
    expect(m.total).toBe(1);
  });

  it("has no total and no groups while the read is loading", () => {
    const m = model({
      adminRuns: admin([run({ status: "running" })]),
      repoActivity: { repos: null, failed: false },
    });
    expect(activeOf(m).count).toBe(1);
    expect(m.total).toBeNull();
    expect(m.recent).toBeNull();
    expect(m.idle).toBeNull();
    expect(m.listFailed).toBe(false);
  });

  it("has no total and no groups when the read failed, even with last-good rows", () => {
    const m = model({
      adminRuns: admin([run({ status: "running" })]),
      repoActivity: { repos: [repo("repo-a", "org/alpha"), repo("repo-b", "org/beta")], failed: true },
    });
    expect(activeOf(m).count).toBe(1);
    expect(m.total).toBeNull();
    expect(m.recent).toBeNull();
    expect(m.idle).toBeNull();
    expect(m.listFailed).toBe(true);
  });
});

describe("total", () => {
  it("unions the read's rows with an active repo the read lacks", () => {
    const m = model({
      adminRuns: admin([run({ status: "running", repo_id: "repo-off", repo_path: "org/disabled" })]),
      repoActivity: { repos: [repo("repo-a", "org/alpha"), repo("repo-b", "org/beta")], failed: false },
    });
    const a = activeOf(m);
    expect(a.count).toBe(1);
    expect(m.total).toBe(3);
    expect(a.repos[0].path).toBe("org/disabled");
  });

  it("counts an active repo the read already has only once", () => {
    const m = model({
      adminRuns: admin([run({ status: "running" })]),
      repoActivity: { repos: [repo("repo-a", "org/alpha"), repo("repo-b", "org/beta")], failed: false },
    });
    expect(m.total).toBe(2);
  });

  it("is an empty-read total of zero, not null", () => {
    expect(model().total).toBe(0);
  });
});

describe("the 500-run cap", () => {
  function capped(extra: RunSummaryItem[]) {
    const rows = [...extra];
    while (rows.length < ADMIN_RUNS_CAP) rows.push(run({ status: "completed", repo_id: "repo-a" }));
    return rows;
  }

  it("says at least, and keeps M exact, including a disabled repo in the read but off the capped list", () => {
    const m = model({
      adminRuns: admin(capped([run({ status: "running", repo_id: "repo-a" })])),
      repoActivity: {
        repos: [repo("repo-a", "org/alpha"), repo("repo-b", "org/beta"), repo("repo-off", "org/off", { enabled: false })],
        failed: false,
      },
    });
    const a = activeOf(m);
    expect(a.atLeast).toBe(true);
    expect(a.count).toBe(1);
    expect(m.total).toBe(3);
  });

  it("does not put a repo in recent or idle when its active run falls past the cap", () => {
    const rows = capped([]).map((r) => ({ ...r, status: "running" as RunStatus }));
    const m = model({
      adminRuns: admin(rows),
      repoActivity: {
        repos: [repo("repo-a", "org/alpha"), repo("repo-b", "org/beta", { last_run_at: ago(60_000) })],
        failed: false,
      },
    });
    expect(activeOf(m).atLeast).toBe(true);
    expect(activeOf(m).count).toBe(1);
    expect(m.recent).toBeNull();
    expect(m.idle).toBeNull();
    expect(m.total).toBe(2);
  });

  it("is not at least one run short of the cap", () => {
    const rows = capped([run({ status: "running" })]).slice(0, ADMIN_RUNS_CAP - 1);
    expect(activeOf(model({ adminRuns: admin(rows) })).atLeast).toBe(false);
  });

  it("is at least with no total while the read is loading, and again when it failed", () => {
    for (const repoActivity of [
      { repos: null, failed: false },
      { repos: null, failed: true },
    ]) {
      const m = model({ adminRuns: admin(capped([run({ status: "running" })])), repoActivity });
      expect(activeOf(m).atLeast).toBe(true);
      expect(m.total).toBeNull();
    }
  });
});

describe("recent and idle", () => {
  it("splits at exactly 7 days: under is recent, exactly 7 is idle", () => {
    const m = model({
      repoActivity: {
        repos: [
          repo("r-under", "org/under", { last_run_at: ago(7 * DAY - 1000) }),
          repo("r-exact", "org/exact", { last_run_at: ago(7 * DAY) }),
        ],
        failed: false,
      },
    });
    expect(m.recent?.map((r) => r.key)).toEqual(["r-under"]);
    expect(m.idle).toEqual({ count: 1, minDays: 7, maxDays: 7, never: 0 });
  });

  it("lists recent repos newest first with an age, and skips active repos", () => {
    const m = model({
      adminRuns: admin([run({ status: "running", repo_id: "r-busy", repo_path: "org/busy" })]),
      repoActivity: {
        repos: [
          repo("r-old", "org/old", { last_run_at: ago(3 * DAY) }),
          repo("r-new", "org/new", { last_run_at: ago(2 * HOUR) }),
          repo("r-busy", "org/busy", { last_run_at: ago(HOUR) }),
        ],
        failed: false,
      },
    });
    expect(m.recent).toEqual([
      { key: "r-new", path: "org/new", badge: "ne", ago: "2h ago" },
      { key: "r-old", path: "org/old", badge: "ol", ago: "3d ago" },
    ]);
    expect(m.idle).toEqual({ count: 0, minDays: null, maxDays: null, never: 0 });
  });

  it("counts a repo that never ran, and tracks the day range of the rest", () => {
    const m = model({
      repoActivity: {
        repos: [
          repo("r-1", "org/one"),
          repo("r-2", "org/two", { last_run_at: ago(9 * DAY + HOUR) }),
          repo("r-3", "org/three", { last_run_at: ago(30 * DAY) }),
          repo("r-4", "org/four", { last_run_at: ago(12 * DAY) }),
        ],
        failed: false,
      },
    });
    expect(m.idle).toEqual({ count: 3, minDays: 9, maxDays: 30, never: 1 });
  });

  it("counts an unparseable date as idle without a day figure", () => {
    const m = model({ repoActivity: { repos: [repo("r-1", "org/one", { last_run_at: "not a date" })], failed: false } });
    expect(m.idle).toEqual({ count: 1, minDays: null, maxDays: null, never: 0 });
  });
});

describe("repoIdleText", () => {
  const idle = (count: number, min: number | null, max: number | null, never: number) => ({
    count,
    minDays: min,
    maxDays: max,
    never,
  });

  it("gives a range, a single value, singular and plural days", () => {
    expect(repoIdleText(idle(3, 9, 30, 1))).toBe("3 last ran 9 to 30 days ago, 1 never ran");
    expect(repoIdleText(idle(2, 9, 9, 0))).toBe("2 last ran 9 days ago");
    expect(repoIdleText(idle(1, 1, 1, 0))).toBe("1 last ran 1 day ago");
    expect(repoIdleText(idle(2, 1, 4, 0))).toBe("2 last ran 1 to 4 days ago");
  });

  it("says only never ran when nothing else is idle", () => {
    expect(repoIdleText(idle(0, null, null, 2))).toBe("2 never ran");
  });

  it("falls back when the dates did not parse", () => {
    expect(repoIdleText(idle(2, null, null, 0))).toBe("2 last ran more than 7 days ago");
    expect(repoIdleText(idle(2, null, null, 1))).toBe("2 last ran more than 7 days ago, 1 never ran");
  });

  it("is null when no repo is idle", () => {
    expect(repoIdleText(idle(0, null, null, 0))).toBeNull();
  });
});

describe("demo masking and unsafe paths", () => {
  const hasUnsafe = (s: string) => /evil/.test(s) || [...s].some((c) => c.charCodeAt(0) < 32 || c === "\u202E");

  it("masks the namespace of a read path and of a run path", () => {
    const m = model({
      demo: true,
      adminRuns: admin([
        run({ status: "running", repo_id: "r-1", repo_path: "secret-org/inner/app" }),
        run({ status: "queued", repo_id: "r-2", repo_path: "other/thing" }),
      ]),
      repoActivity: {
        repos: [
          repo("r-1", "secret-org/inner/app"),
          repo("r-3", "acme/billing", { last_run_at: ago(HOUR) }),
        ],
        failed: false,
      },
    });
    expect(activeOf(m).repos.map((r) => r.path).sort()).toEqual(["demo/app", "demo/thing"]);
    expect(m.recent?.[0]).toMatchObject({ path: "demo/billing", badge: "bi" });
  });

  it("sanitizes an unsafe run repo_path before masking, so the badge follows the masked path", () => {
    const evil = "evil\u202Eorg/\u001bx\u0007repo";
    const m = model({
      demo: true,
      adminRuns: admin([run({ status: "running", repo_id: "r-evil", repo_path: evil })]),
      repoActivity: { repos: [], failed: false },
    });
    const row = activeOf(m).repos[0];
    expect(row.path).toBe("demo/xrepo");
    expect(row.badge).toBe("xr");
    expect(hasUnsafe(row.path)).toBe(false);
  });

  it("falls back to Unknown when the last segment is empty after stripping, so masking cannot promote the namespace", () => {
    const m = model({
      demo: true,
      adminRuns: admin([run({ status: "running", repo_id: "r-1", repo_path: "org/\u202E" })]),
      repoActivity: { repos: [], failed: false },
    });
    expect(activeOf(m).repos[0]).toMatchObject({ path: "Unknown", badge: "Un" });
  });

  it("strips control characters with demo off and falls back to Unknown when nothing is left", () => {
    const m = model({
      adminRuns: admin([
        run({ status: "running", repo_id: "r-1", repo_path: "org/a\u202Eb" }),
        run({ status: "queued", repo_id: "r-2", repo_path: "\u202E" }),
      ]),
    });
    const byKey = Object.fromEntries(activeOf(m).repos.map((r) => [r.key, r]));
    expect(byKey["r-1"].path).toBe("org/ab");
    expect(byKey["r-2"]).toMatchObject({ path: "Unknown", badge: "Un" });
  });
});
