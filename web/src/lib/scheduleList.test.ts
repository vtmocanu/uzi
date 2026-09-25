// scheduleList (PRD #1645): row naming (D4), the D3 sort bands, D5 filters, the D6 fold.
import { describe, expect, it } from "vitest";
import type { CatalogEntry, Schedule } from "./api";
import {
  NO_FILTER,
  clearHidingFilters,
  foldRefs,
  isFoldedOnce,
  jobEnablement,
  matchesFilter,
  nextFireOf,
  pruneFilter,
  repoOptions,
  scheduleDisplayName,
  sortSchedules,
  sourceCounts,
  splitFold,
  targetTitle,
} from "./scheduleList";

const H = 3_600_000;
const at = (ms: number) => new Date(Date.UTC(2026, 8, 25) + ms).toISOString();

function sched(over: Partial<Schedule>): Schedule {
  return {
    id: "s1",
    repo_id: "r",
    repo_path: "o/r",
    target: "sweep",
    issue_iid: null,
    labels: null,
    prompt: "",
    timing: "recurring",
    cron_expr: "0 2 * * *",
    run_at: null,
    timezone: "UTC",
    next_fire_at: null,
    last_fired_at: null,
    last_fire: null,
    auto_approve: true,
    wait_on_limit: true,
    max_issues: null,
    guidance: null,
    baked_guidance: null,
    model: null,
    output_mode: null,
    override_subagent_model: false,
    enabled: true,
    status: "active",
    origin: "user",
    catalog_slug: null,
    customized: false,
    sibling_group_id: null,
    created_at: at(0),
    updated_at: at(0),
    next_fires: [],
    ...over,
  };
}

const byName = (s: Schedule) => s.prompt || s.id;

describe("nextFireOf", () => {
  it("prefers the live preview's first fire, then next_fire_at, else null", () => {
    expect(nextFireOf(sched({ next_fires: [at(H), at(2 * H)], next_fire_at: at(5 * H) }))).toBe(at(H));
    expect(nextFireOf(sched({ next_fires: null, next_fire_at: at(5 * H) }))).toBe(at(5 * H));
    expect(nextFireOf(sched({ next_fires: [], next_fire_at: null }))).toBeNull();
  });
});

describe("scheduleDisplayName", () => {
  const entries = new Map([["bug-triage", { slug: "bug-triage", name: "Bug triage sweep" } as CatalogEntry]]);

  it("names a default row after its catalog entry, a user row by its target", () => {
    expect(scheduleDisplayName(sched({ origin: "default", catalog_slug: "bug-triage", labels: ["bug"] }), entries)).toBe(
      "Bug triage sweep",
    );
    expect(scheduleDisplayName(sched({ labels: ["bug"] }), entries)).toBe("Sweep · label bug");
  });

  it("falls back to the slug for a default whose entry left the catalog", () => {
    expect(scheduleDisplayName(sched({ origin: "default", catalog_slug: "retired-job" }), entries)).toBe("retired-job");
  });

  it("targetTitle truncates a long prompt", () => {
    const t = targetTitle(sched({ target: "prompt", prompt: "x".repeat(60) }));
    expect(t.startsWith("Prompt: ")).toBe(true);
    expect(t.endsWith("…")).toBe(true);
    expect(t.length).toBe("Prompt: ".length + 42);
  });
});

describe("sortSchedules (D3)", () => {
  it("orders parked, then enabled by next fire, then enabled with none, then paused", () => {
    const rows = [
      sched({ id: "paused", prompt: "a-paused", enabled: false, next_fire_at: at(H) }),
      sched({ id: "nofire", prompt: "b-nofire", next_fire_at: null }),
      sched({ id: "late", prompt: "c-late", next_fires: [at(5 * H)] }),
      sched({ id: "parked", prompt: "z-parked", status: "error", next_fire_at: at(9 * H) }),
      sched({ id: "soon", prompt: "d-soon", next_fire_at: at(H) }),
    ];
    expect(sortSchedules(rows, byName).map((s) => s.id)).toEqual(["parked", "soon", "late", "nofire", "paused"]);
  });

  it("a parked row leads even when it is also paused", () => {
    const rows = [
      sched({ id: "soon", prompt: "a", next_fire_at: at(H) }),
      sched({ id: "parked-off", prompt: "b", status: "error", enabled: false }),
    ];
    expect(sortSchedules(rows, byName).map((s) => s.id)).toEqual(["parked-off", "soon"]);
  });

  it("breaks ties by display name, then repo path, then id", () => {
    const same = at(2 * H);
    const rows = [
      sched({ id: "3", prompt: "beta", repo_path: "o/a", next_fire_at: same }),
      sched({ id: "2", prompt: "alpha", repo_path: "o/b", next_fire_at: same }),
      sched({ id: "1", prompt: "alpha", repo_path: "o/b", next_fire_at: same }),
      sched({ id: "0", prompt: "alpha", repo_path: "o/a", next_fire_at: same }),
    ];
    expect(sortSchedules(rows, byName).map((s) => s.id)).toEqual(["0", "1", "2", "3"]);
  });

  it("does not mutate its input", () => {
    const rows = [sched({ id: "b", prompt: "b", enabled: false }), sched({ id: "a", prompt: "a", next_fire_at: at(H) })];
    sortSchedules(rows, byName);
    expect(rows.map((s) => s.id)).toEqual(["b", "a"]);
  });
});

describe("filters (D5)", () => {
  const def = sched({ id: "d", origin: "default", catalog_slug: "bug-triage", repo_id: "r1" });
  const mine = sched({ id: "m", origin: "user", repo_id: "r2", repo_path: "o/b" });
  const off = sched({ id: "o", origin: "user", enabled: false, repo_id: "r1" });

  it("matches each source, the repo and the job, intersected", () => {
    expect(matchesFilter(def, { ...NO_FILTER, source: "catalog" })).toBe(true);
    expect(matchesFilter(mine, { ...NO_FILTER, source: "catalog" })).toBe(false);
    expect(matchesFilter(mine, { ...NO_FILTER, source: "mine" })).toBe(true);
    expect(matchesFilter(off, { ...NO_FILTER, source: "paused" })).toBe(true);
    expect(matchesFilter(mine, { ...NO_FILTER, source: "paused" })).toBe(false);
    expect(matchesFilter(def, { ...NO_FILTER, repoId: "r2" })).toBe(false);
    expect(matchesFilter(def, { ...NO_FILTER, jobSlug: "bug-triage" })).toBe(true);
    // A user clone of the job keeps no catalog_slug, so the Job filter never matches it.
    expect(matchesFilter(mine, { ...NO_FILTER, jobSlug: "bug-triage" })).toBe(false);
    expect(matchesFilter(def, { source: "catalog", repoId: "r1", jobSlug: "other" })).toBe(false);
  });

  it("counts each chip over the whole set", () => {
    expect(sourceCounts([def, mine, off])).toEqual({ all: 3, catalog: 1, mine: 2, paused: 1 });
  });

  it("lists the distinct repos by path", () => {
    expect(repoOptions([off, mine, def])).toEqual([
      { id: "r2", path: "o/b" },
      { id: "r1", path: "o/r" },
    ]);
  });

  it("clearHidingFilters resets only the dimensions hiding the row, and returns the same filter when none do", () => {
    const f = { source: "catalog" as const, repoId: "r1", jobSlug: "bug-triage" };
    expect(clearHidingFilters(f, def)).toBe(f);
    expect(clearHidingFilters(f, mine)).toEqual(NO_FILTER);
    expect(clearHidingFilters({ ...NO_FILTER, source: "mine", repoId: "r1" }, mine)).toEqual({ ...NO_FILTER, source: "mine" });
    expect(clearHidingFilters({ ...NO_FILTER, source: "paused" }, def)).toEqual(NO_FILTER);
  });
});

describe("the fired one-shot fold (D6)", () => {
  const fired = (over: Partial<Schedule>) => sched({ target: "issue", timing: "once", status: "fired", ...over });

  it("folds only fired one-shots, never a parked or a pending one", () => {
    expect(isFoldedOnce(fired({}))).toBe(true);
    expect(isFoldedOnce(fired({ status: "error" }))).toBe(false);
    expect(isFoldedOnce(fired({ status: "active" }))).toBe(false);
    expect(isFoldedOnce(sched({ status: "fired" }))).toBe(false); // recurring
  });

  it("filters first, then folds within the filtered set, then sorts each part", () => {
    const rows = [
      fired({ id: "f2", issue_iid: 2, repo_id: "r1" }),
      sched({ id: "b", prompt: "b", repo_id: "r1" }),
      fired({ id: "f1", issue_iid: 1, repo_id: "r1" }),
      fired({ id: "fx", issue_iid: 9, repo_id: "r2" }),
      sched({ id: "a", prompt: "a", repo_id: "r1" }),
    ];
    const { shown, folded } = splitFold(rows, { ...NO_FILTER, repoId: "r1" }, (s) => s.prompt || s.id);
    expect(shown.map((s) => s.id)).toEqual(["a", "b"]);
    expect(folded.map((s) => s.id)).toEqual(["f1", "f2"]);
  });

  it("foldRefs lists issue refs once, skipping rows with no issue", () => {
    expect(
      foldRefs([fired({ issue_iid: 158 }), fired({ issue_iid: 158 }), fired({ target: "prompt" }), fired({ issue_iid: 7 })]),
    ).toEqual(["#158", "#7"]);
  });
});

describe("pruneFilter (M2 review note 1)", () => {
  const rows = [
    sched({ id: "a", repo_id: "r1", origin: "default", catalog_slug: "bug-triage" }),
    sched({ id: "b", repo_id: "r2" }),
  ];
  const slugs = new Set(["bug-triage", "docs-hygiene"]);

  it("returns the same filter when every dimension still names something", () => {
    const f = { source: "all" as const, repoId: "r2", jobSlug: "docs-hygiene" };
    expect(pruneFilter(f, rows, slugs)).toBe(f);
  });

  it("drops a repo no schedule is on any more, keeping the rest", () => {
    const f = { source: "paused" as const, repoId: "r9", jobSlug: "bug-triage" };
    expect(pruneFilter(f, rows, slugs)).toEqual({ source: "paused", repoId: null, jobSlug: "bug-triage" });
  });

  it("keeps a job with no row while the catalog carries it; drops one known to neither", () => {
    const keep = { ...NO_FILTER, jobSlug: "docs-hygiene" };
    expect(pruneFilter(keep, rows, slugs)).toBe(keep);
    // A retired slug a row still carries is kept too (the rows are what it filters).
    const retired = [...rows, sched({ id: "c", origin: "default", catalog_slug: "retired" })];
    const onRow = { ...NO_FILTER, jobSlug: "retired" };
    expect(pruneFilter(onRow, retired, slugs)).toBe(onRow);
    expect(pruneFilter({ ...NO_FILTER, jobSlug: "gone" }, rows, slugs)).toEqual(NO_FILTER);
  });
});

describe("jobEnablement (D7)", () => {
  it("counts distinct repos with a default row for the slug, and the paused ones", () => {
    const rows = [
      sched({ id: "a", repo_id: "r1", origin: "default", catalog_slug: "bug-triage" }),
      sched({ id: "b", repo_id: "r2", origin: "default", catalog_slug: "bug-triage", enabled: false }),
      // A user row or another slug on a third repo never counts.
      sched({ id: "c", repo_id: "r3", origin: "user", catalog_slug: null }),
      sched({ id: "d", repo_id: "r3", origin: "default", catalog_slug: "docs-hygiene" }),
    ];
    const got = jobEnablement(rows, "bug-triage");
    expect([...got.repoIds].sort()).toEqual(["r1", "r2"]);
    expect(got.pausedRepos).toBe(1);
    expect(jobEnablement(rows, "nope")).toEqual({ repoIds: new Set(), pausedRepos: 0 });
  });
});
