// scheduleList (PRD #1645): row naming (D4) and the D3 sort bands.
import { describe, expect, it } from "vitest";
import type { CatalogEntry, Schedule } from "./api";
import { nextFireOf, scheduleDisplayName, sortSchedules, targetTitle } from "./scheduleList";

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
