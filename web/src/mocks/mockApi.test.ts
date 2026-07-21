// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";

// Enhancement B: the mock persists ONLY the settings maps to localStorage so the
// demo survives a hard reload. A "reload" is simulated by resetting the module
// registry and re-importing mockApi with localStorage primed — its top-level
// loadSettings() then runs against the stored blob, exactly as a fresh page load
// would. This jsdom build does not expose window.localStorage, so back it with a
// Map-based Storage stub (same approach as prefs.test.ts / theme.test.ts).

const KEY = "uzi.mock.v2";

function installStorage(initial: Record<string, string> = {}): void {
  const m = new Map<string, string>(Object.entries(initial));
  const storage = {
    getItem: (k: string) => (m.has(k) ? m.get(k)! : null),
    setItem: (k: string, v: string) => void m.set(k, String(v)),
    removeItem: (k: string) => void m.delete(k),
    clear: () => m.clear(),
    key: (i: number) => [...m.keys()][i] ?? null,
    get length() {
      return m.size;
    },
  } as Storage;
  Object.defineProperty(window, "localStorage", { configurable: true, value: storage });
}

async function reload() {
  // A fresh module init: re-runs mockApi's top-level loadSettings() against the
  // current (primed or write-through-updated) localStorage.
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

afterEach(() => {
  vi.resetModules();
});

describe("mockApi settings persistence (demo survives reload)", () => {
  it("restores a persisted theme override, worker model, and default_theme", async () => {
    installStorage({
      [KEY]: JSON.stringify({
        v: 1,
        userSettings: { default_model: "opus", theme: "mission" },
        appSettings: {
          prd_label: "Feature",
          autopilot_label: "autopilot",
          default_theme: "mission",
          prdless_enabled: "false",
          prdless_label: "NOSPEC",
          slack_enabled: "true",
          public_base_url: "https://uzi.example",
          judge_enabled: "false",
          judge_model: "haiku",
          health_enabled: "false",
          health_stall_seconds: "120",
          health_slow_seconds: "2700",
          health_queued_seconds: "600",
          health_approval_seconds: "3600",
          health_nudge_cooldown_seconds: "1800",
          docker_repo_allowlist: "",
        },
      }),
    });
    const api = await reload();

    const user = (await api.getMySettings()).settings;
    expect(user.theme).toBe("mission");
    expect(user.default_model).toBe("opus");

    const app = (await api.getSettings()).settings;
    expect(app.default_theme).toBe("mission");
    expect(app.prd_label).toBe("Feature");
    // The prdless keys round-trip too (PRD #22 M1 brought them into app_settings).
    expect(app.prdless_enabled).toBe("false");
    expect(app.prdless_label).toBe("NOSPEC");
    // The Slack non-secret keys round-trip too (PRD #25 M1).
    expect(app.slack_enabled).toBe("true");
    expect(app.public_base_url).toBe("https://uzi.example");
    // The run-health keys round-trip too (PRD #47).
    expect(app.health_enabled).toBe("false");
    expect(app.health_stall_seconds).toBe("120");
  });

  it("falls back to seed on a corrupt blob", async () => {
    installStorage({ [KEY]: "{ not valid json" });
    const api = await reload();

    expect((await api.getMySettings()).settings.theme).toBeNull();
    expect((await api.getSettings()).settings.default_theme).toBe("ember");
  });

  it("falls back to seed on a version/shape mismatch (stale demo state is discarded)", async () => {
    installStorage({
      [KEY]: JSON.stringify({
        v: 99, // a different seed-schema version: must be discarded, not served
        userSettings: { default_model: null, theme: "mission" },
        appSettings: { prd_label: "x", autopilot_label: "y", default_theme: "mission" },
      }),
    });
    const api = await reload();

    expect((await api.getMySettings()).settings.theme).toBeNull();
    expect((await api.getSettings()).settings.default_theme).toBe("ember");
  });

  it("write-throughs a settings change so the next reload restores it", async () => {
    installStorage(); // empty store → seeds
    let api = await reload();
    await api.putMySettings({ theme: "mission" });
    await api.updateSettings({ default_theme: "mission" });

    // Reload: the write-through values, not the seed, come back.
    api = await reload();
    expect((await api.getMySettings()).settings.theme).toBe("mission");
    expect((await api.getSettings()).settings.default_theme).toBe("mission");
  });

  it("does not persist non-settings state (runs stay seed-only across a reload)", async () => {
    installStorage();
    const api = await reload();
    // A sanity check that only settings round-trip: the seeded runs are always
    // present from the seed, never from persisted state.
    const runs = (await api.listRuns()).runs;
    expect(runs.length).toBeGreaterThan(0);
  });
});

describe("mockApi run judge review (PRD #46 M4)", () => {
  it("returns the seeded review for a judged run, null for an unjudged one, 404 for unknown", async () => {
    installStorage();
    const api = await reload();

    const { review } = await api.getRunReview("run-done");
    expect(review).not.toBeNull();
    expect(review!.verdict).toBe("issues");
    expect(review!.recommendations.length).toBeGreaterThan(0);

    // A terminal run with no seeded review reads as null (not judged yet).
    expect((await api.getRunReview("run-failed")).review).toBeNull();

    await expect(api.getRunReview("does-not-exist")).rejects.toMatchObject({ status: 404 });
  });

  it("rerunJudge enqueues a judge run for a terminal issue run and rejects a non-terminal one", async () => {
    installStorage();
    const api = await reload();

    const { run } = await api.rerunJudge("run-done");
    expect(run.kind).toBe("judge");
    expect(run.status).toBe("queued");

    // A still-queued run is not terminal, so it cannot be judged.
    await expect(api.rerunJudge("run-queued")).rejects.toMatchObject({ status: 422 });
  });
});

describe("mockApi judge backlog (PRD #98 M3)", () => {
  it("dedups by (category, target) across runs, ranks by frequency, and can express a PARTIALLY-settled group", async () => {
    installStorage();
    const api = await reload();

    const all = await api.getJudgeBacklog("all");
    const byCoord = (c: string, t: string) => all.groups.find((g) => g.category === c && g.target === t);

    // poller recurs in all three seeded runs → the top group by frequency.
    const poller = byCoord("improve_uzi", "api/internal/poller")!;
    expect(poller.run_count).toBe(3);
    expect(poller.open_count).toBe(3);
    expect(poller.bucket).toBe("todo");
    expect(all.groups[0]).toBe(poller); // ranked first (run_count desc)

    // shellcheck is the PARTIALLY-settled group: DONE in run-done, TODO in run-closed. The
    // fixture must be able to construct this before any test leans on it (checkpoint).
    const shellcheck = byCoord("install_worker_tool", "shellcheck")!;
    expect(shellcheck.run_count).toBe(2);
    expect(shellcheck.open_count).toBe(1); // one open member among two
    expect(shellcheck.bucket).toBe("todo"); // any open member → the group rolls up To triage
    const buckets = shellcheck.occurrences.map((o) => o.bucket).sort();
    expect(buckets).toEqual(["done", "todo"]);

    // The canonical triage is the SAME query getJudgeStats serves — the two cannot drift.
    const stats = await api.getJudgeStats();
    expect(all.triage).toEqual(stats);
    // Per-recommendation, and still > the open group rows. It was 5 before PRD #98 review
    // B3 seeded run-closed's ripgrep as an issue-close AUTO-DONE (filed #91, then closed):
    // that moved one recommendation off the todo rung. The number changed because the
    // fixture gained a state, not because the tally drifted.
    expect(stats.todo).toBe(4);
  });

  it("the ?run= anchor keeps only groups recurring in that run but preserves their other-run occurrences", async () => {
    installStorage();
    const api = await reload();

    const anchored = await api.getJudgeBacklog("all", "run-closed");
    const coords = anchored.groups.map((g) => `${g.category}/${g.target}`).sort();
    // run-closed's review carries poller, shellcheck, ripgrep — and nothing else survives.
    expect(coords).toEqual([
      "enable_tool/ripgrep",
      "improve_uzi/api/internal/poller",
      "install_worker_tool/shellcheck",
    ]);
    // poller still shows ALL three of its occurrences (the recurrence is the whole point).
    const poller = anchored.groups.find((g) => g.target === "api/internal/poller")!;
    expect(poller.run_count).toBe(3);

    // A foreign / unknown run matches nothing (no existence oracle) — never a 404.
    const foreign = await api.getJudgeBacklog("all", "does-not-exist");
    expect(foreign.groups).toEqual([]);
  });

  it("scope=open dispositions ONLY the open members; `updated` counts triples, lower than the visible span", async () => {
    installStorage();
    const api = await reload();

    // shellcheck spans 2 occurrences (done + todo). scope=open touches only the open one,
    // so `updated` is 1 — deliberately lower than the 2 the group visibly spans.
    const res = await api.bulkSetJudgeDisposition(
      [{ category: "install_worker_tool", target: "shellcheck" }],
      "dismissed",
      "wont_do",
      "open",
    );
    expect(res.updated).toBe(1);
    // The group is RE-READ at bucket=all and comes back with its new rollup: the done member
    // remains, the once-open member is now dismissed → highest rung (dismissed) wins.
    const g = res.groups.find((x) => x.target === "shellcheck")!;
    expect(g.bucket).toBe("dismissed");
    expect(g.open_count).toBe(0);
    // The done member was NOT overwritten (scope=open left it).
    expect(g.occurrences.map((o) => o.bucket).sort()).toEqual(["dismissed", "done"]);

    // Canonical counts moved: one left To triage, one joined Dismissed. (Baseline todo is 4,
    // not 5, since B3's seeded auto-done — see the dedup test above.)
    expect(res.triage.todo).toBe(3);
    expect(res.triage.dismissed).toBe(3);
  });

  it("surfaces an issue-close auto-done distinctly, and a human override CLEARS the provenance", async () => {
    installStorage();
    const api = await reload();

    const ripgrep = (b: Awaited<ReturnType<typeof api.getJudgeBacklog>>) =>
      b.groups.find((g) => g.category === "enable_tool" && g.target === "ripgrep")!.occurrences[0];

    // Seeded state: filed as #91, that issue closed, so the M6 sync marked it done. The
    // ladder puts done above filed, so the bucket is `done` and the provenance is what
    // distinguishes it from a hand-marked one.
    const before = ripgrep(await api.getJudgeBacklog("all"));
    expect(before.bucket).toBe("done");
    expect(before.set_via).toBe("issue_close");
    expect(before.filed_issue?.issue_iid).toBe(91);

    // A HUMAN now dismisses it. set_via must go back to "a person decided this" — otherwise
    // the chip would keep reading "Done via #91" after the user overrode it, attributing
    // their decision to the system. The server guarantees this with a literal NULL rather
    // than EXCLUDED.set_via (dispositions.sql); the mock must not diverge, and its
    // Object.assign upsert makes the omission a live bug rather than a tidiness nit.
    await api.bulkSetJudgeDisposition(
      [{ category: "enable_tool", target: "ripgrep" }],
      "dismissed",
      "not_an_issue",
      "all",
    );
    const after = ripgrep(await api.getJudgeBacklog("all"));
    expect(after.bucket).toBe("dismissed");
    expect(after.set_via).toBeUndefined();
  });
});
