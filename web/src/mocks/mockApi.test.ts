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
          run_eligible_labels: "Feature,bug",
          board_extra_labels: "bug",
          eligible_label_waives_prd_link: "true",
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

    const { review, pending_judge } = await api.getRunReview("run-done");
    expect(review).not.toBeNull();
    expect(review!.verdict).toBe("issues");
    expect(review!.recommendations.length).toBeGreaterThan(0);
    // run-done is the SETTLED fixture: a verdict and nothing in flight, so the panel's
    // enabled Re-run-judge state stays demoable.
    expect(pending_judge).toBeNull();

    // A terminal run with no seeded review reads as null (not judged yet) — but the
    // envelope's sibling key says WHY (PRD #119): run-failed has an auto-judge already
    // scheduled, which is the state review:null alone cannot express.
    const failed = await api.getRunReview("run-failed");
    expect(failed.review).toBeNull();
    expect(failed.pending_judge).toEqual({ state: "scheduled", enqueued_at: expect.any(String) });

    // A re-judge in flight over an existing verdict: both keys set at once.
    const closed = await api.getRunReview("run-closed");
    expect(closed.review).not.toBeNull();
    expect(closed.pending_judge).toEqual({ state: "running", enqueued_at: expect.any(String) });

    await expect(api.getRunReview("does-not-exist")).rejects.toMatchObject({ status: 404 });
  });

  // 🔴 The demo is the only place these four states are ever LOOKED at — the panel's own
  // tests mount JudgePanel against stubbed responses, so they stay green for a mock that
  // can no longer reach a state at all. That is exactly how #119 shipped with the
  // never-judged empty state (unchanged copy, ENABLED button) unreachable: run-failed was
  // the one terminal run with no review and the PRD gave it a scheduled auto-judge.
  // Asserted as a COVERAGE claim over the fixtures rather than by run id, so renaming or
  // re-purposing any one of them is free and dropping a state is not.
  it("keeps all four run-review panel states reachable from the fixtures (#119)", async () => {
    installStorage();
    const api = await reload();
    const { mockRuns } = await import("./data");

    const terminal = new Set(["completed", "failed", "cancelled"]);
    const judgeable = mockRuns.filter(
      (r) => terminal.has(r.status) && (r.kind === "issue" || r.kind === "ci_fix"),
    );
    const seen = new Set<string>();
    for (const r of judgeable) {
      const { review, pending_judge } = await api.getRunReview(r.id);
      seen.add(`${review === null ? "no-review" : "review"}/${pending_judge?.state ?? "none"}`);
    }

    // never judged (enabled Run judge) / auto-judge coming / re-judge in flight over a
    // verdict / settled verdict with a live Re-run judge.
    for (const state of ["no-review/none", "no-review/scheduled", "review/running", "review/none"]) {
      expect(
        seen.has(state),
        `no terminal, judge-eligible fixture renders the panel's "${state}" state — it ` +
          `cannot be browsed in mock mode, which is where this panel gets validated`,
      ).toBe(true);
    }
  });

  it("rerunJudge enqueues a judge run for a terminal issue run and rejects a non-terminal one", async () => {
    installStorage();
    const api = await reload();

    const { run } = await api.rerunJudge("run-done");
    expect(run.kind).toBe("judge");
    expect(run.status).toBe("queued");

    // …and it REGISTERS as the target's active judge, the way the real POST's inserted
    // row does. Without this the mock answered pending_judge:null on the next read, so
    // the panel's button stayed labelled "Re-run judge" where the server relabels it
    // "Judge scheduled" on the next poll — the mock disagreeing with the API it stands in
    // for, in exactly the state PRD #119 exists to render.
    const after = await api.getRunReview("run-done");
    expect(after.pending_judge).toEqual({ state: "scheduled", enqueued_at: expect.any(String) });

    // And that makes the index's refusal reachable FROM THE UI: a second click on the
    // same target 409s, which is the TOCTOU path the panel absorbs into a re-fetch. Only
    // pre-seeded targets could produce it before.
    await expect(api.rerunJudge("run-done")).rejects.toMatchObject({
      status: 409,
      message: "a judge run is already in progress for this run",
    });

    // A still-queued run is not terminal, so it cannot be judged.
    await expect(api.rerunJudge("run-queued")).rejects.toMatchObject({ status: 422 });

    // PRD #119: a target that already has an active judge 409s with the server's exact
    // message — the unique index's refusal, and the one the panel absorbs into a
    // re-fetch rather than an error banner.
    await expect(api.rerunJudge("run-failed")).rejects.toMatchObject({
      status: 409,
      message: "a judge run is already in progress for this run",
    });
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

  // The SINGLE-COORDINATE write path must clear the provenance too, and until now only the
  // bulk path was driven (PRD #98 review N-a). Measured: removing the clear from
  // setDisposition left typecheck clean and all 849 tests green — half-defended, which is
  // the state most likely to read as fully defended.
  //
  // It is the path RunView's per-recommendation Mark done / Dismiss drives, and it is
  // reachable on the very fixture B3 seeded: run-closed's enable_tool/ripgrep is an
  // issue-close auto-done, and that run's page renders per-rec controls. So without the
  // clear, hand-marking it from RunView leaves the Judge chip reading "Done via #91" for a
  // decision the user just made.
  it("the SINGLE-coordinate write path clears the provenance too, not just the bulk one", async () => {
    installStorage();
    const api = await reload();

    const backlog = await api.getJudgeBacklog("all");
    const group = backlog.groups.find((g) => g.category === "enable_tool" && g.target === "ripgrep")!;
    const occ = group.occurrences[0];
    expect(occ.set_via).toBe("issue_close"); // the fixture really is an auto-done

    // A human marks it done from the run page — the #94 per-recommendation route.
    await api.setDisposition(occ.run_id, occ.rec_id, "done");

    const after = (await api.getJudgeBacklog("all")).groups.find(
      (g) => g.category === "enable_tool" && g.target === "ripgrep",
    )!.occurrences[0];
    expect(after.bucket).toBe("done");
    expect(after.set_via).toBeUndefined();
  });

  // The mock must not invent wire fields. set_via is a mock-side extension of the STORED
  // disposition; the run-page DispositionDTO has no such field, so GET /runs/{id}/review
  // must not carry it (PRD #98 review N-b). A mock that ships more than the API does makes a
  // future RunView provenance feature work in demo mode and fail in production.
  it("does not leak set_via onto the run-page review DTO", async () => {
    installStorage();
    const api = await reload();

    // The endpoint answers an ENVELOPE, {review: ...|null, pending_judge: ...|null}.
    const { review } = await api.getRunReview("run-closed");
    const disp = review!.dispositions.find((d) => d.category === "enable_tool" && d.target === "ripgrep");
    expect(disp).toBeTruthy();
    expect("set_via" in (disp as object)).toBe(false);
  });
});

describe("mockApi judge category stats (PRD #270)", () => {
  // getJudgeCategoryStats is a bucket-keyed MATRIX now (PRD #270): bucket → category → GROUP
  // count, with `all` the whole-backlog per-category count and, by construction,
  // todo+filed+done+dismissed == all per category. It rolls up the SAME ladder the backlog
  // groups do (todo if any open member, else the group's highest settled rung), so a
  // meaningful differential test needs a fixture that is genuinely multi-bucket AND exercises
  // both the highest-settled-rung spread and open-over-settled promotion. A degenerate
  // single-bucket fixture would make the sum invariant vacuous, so guards below fail loudly if
  // the fixture ever collapses to one.
  const SETTLED_BUCKETS = ["filed", "done", "dismissed"] as const;

  // bucketSum totals the four disposition buckets for one category — the value that must equal
  // that category's `all` slice.
  function bucketSum(matrix: Record<string, Record<string, number>>, cat: string): number {
    return (["todo", "filed", "done", "dismissed"] as const).reduce(
      (n, b) => n + (matrix[b]?.[cat] ?? 0),
      0,
    );
  }

  it("is a bucket matrix whose per-category buckets sum to `all`, over a genuinely multi-bucket fixture", async () => {
    installStorage();
    const api = await reload();
    const { mockReviews } = await import("./data");

    // Force a category to span TWO distinct non-todo settled buckets: improve_uzi seeds one
    // dismissed group (run-timeout) and one open group (api/internal/poller). Mark the open one
    // done and improve_uzi now lands groups in BOTH done and dismissed — the highest-settled-rung
    // spread a single-bucket fixture never exercises. install_worker_tool/shellcheck is left
    // untouched: its group has a done member (first review) AND an open member (another run), so
    // it must still promote to todo — the open+settled case.
    await api.bulkSetJudgeDisposition(
      [{ category: "improve_uzi", target: "api/internal/poller" }],
      "done",
      undefined,
      "open",
    );

    const { counts_by_bucket } = await api.getJudgeCategoryStats();

    // Shape: exactly the five bucket keys, each an object (possibly empty).
    expect(Object.keys(counts_by_bucket).sort()).toEqual(["all", "dismissed", "done", "filed", "todo"]);
    for (const b of Object.keys(counts_by_bucket)) {
      expect(counts_by_bucket[b] && typeof counts_by_bucket[b]).toBe("object");
    }

    // Core rollup invariant: every group is counted once in its own bucket and once in `all`,
    // so the four disposition buckets sum to `all` for every category.
    for (const cat of Object.keys(counts_by_bucket.all)) {
      expect(bucketSum(counts_by_bucket, cat), `todo+filed+done+dismissed must equal all for ${cat}`).toBe(
        counts_by_bucket.all[cat],
      );
    }
    // …and no category appears in a per-bucket slice without appearing in `all`.
    for (const b of ["todo", "filed", "done", "dismissed"] as const) {
      for (const cat of Object.keys(counts_by_bucket[b])) {
        expect(counts_by_bucket.all[cat] ?? 0, `${cat} is in ${b} but missing from all`).toBeGreaterThan(0);
      }
    }

    // DISCRIMINATION GUARD 1 — highest-rung spread: improve_uzi's groups must span ≥2 DISTINCT
    // settled buckets. Without it the matrix could be degenerate (every group in one bucket) and
    // the sum invariant above would be vacuous.
    const improveUziSettled = SETTLED_BUCKETS.filter((b) => (counts_by_bucket[b].improve_uzi ?? 0) > 0);
    expect(
      improveUziSettled.length,
      "fixture not discriminating: improve_uzi must have groups in ≥2 distinct settled buckets",
    ).toBeGreaterThanOrEqual(2);
    expect(counts_by_bucket.done.improve_uzi).toBe(1); // poller, just marked done
    expect(counts_by_bucket.dismissed.improve_uzi).toBe(1); // run-timeout, dismissed in seed

    // DISCRIMINATION GUARD 2 — open-over-settled promotion: install_worker_tool/shellcheck has a
    // SETTLED member in the seed yet recurs open in another run, so its group must roll up to
    // todo. Prove the seed really is mixed (else "in todo" proves nothing), then assert placement.
    const shellcheckSettled = mockReviews.some((r) =>
      r.dispositions.some((d) => d.category === "install_worker_tool" && d.target === "shellcheck"),
    );
    expect(
      shellcheckSettled,
      "fixture not discriminating: install_worker_tool/shellcheck has no settled member, so its " +
        "presence in todo cannot demonstrate open-over-settled promotion",
    ).toBe(true);
    expect(counts_by_bucket.todo.install_worker_tool).toBe(1);
    expect(counts_by_bucket.all.install_worker_tool).toBe(1);

    // GROUPS, not rows: improve_uzi is 2 groups (poller, run-timeout) while its rows number 4
    // (poller recurs in three reviews). A rows-counter would read 4 in `all`.
    const improveUziRows = mockReviews.reduce(
      (n, r) => n + r.recommendations.filter((x) => x.category === "improve_uzi").length,
      0,
    );
    expect(counts_by_bucket.all.improve_uzi).toBe(2);
    expect(improveUziRows).toBeGreaterThan(counts_by_bucket.all.improve_uzi);
  });

  it("is triage-VARIANT: a disposition moves a group between buckets (PRD #270 reverses #244)", async () => {
    installStorage();
    const api = await reload();

    // install_worker_tool/shellcheck seeds as a todo group (an open member in a second run).
    // Dismiss its open member and the group moves todo → dismissed: the matrix is no longer the
    // fetch-once, triage-invariant aggregate #244 shipped.
    const before = (await api.getJudgeCategoryStats()).counts_by_bucket;
    expect(before.todo.install_worker_tool).toBe(1);
    expect(before.dismissed.install_worker_tool ?? 0).toBe(0);

    await api.bulkSetJudgeDisposition(
      [{ category: "install_worker_tool", target: "shellcheck" }],
      "dismissed",
      "wont_do",
      "open",
    );

    const after = (await api.getJudgeCategoryStats()).counts_by_bucket;
    expect(after.todo.install_worker_tool ?? 0).toBe(0);
    expect(after.dismissed.install_worker_tool).toBe(1);
    // The whole-backlog `all` slice is unchanged — one group, still one group.
    expect(after.all.install_worker_tool).toBe(before.all.install_worker_tool);
  });

  it("scopes the matrix to the ?run= anchor (deep-link parity with the backlog)", async () => {
    installStorage();
    const api = await reload();

    // The anchored matrix must be a subset of the unanchored one: a run anchor keeps only groups
    // recurring in that run. Read the backlog at the same anchor to learn which categories are in
    // scope, then assert the matrix's `all` slice matches that set.
    const whole = (await api.getJudgeCategoryStats()).counts_by_bucket;
    const anchoredBacklog = await api.getJudgeBacklog("all", "run-closed");
    const anchoredCats = new Set(anchoredBacklog.groups.map((g) => g.category));

    const anchored = (await api.getJudgeCategoryStats("run-closed")).counts_by_bucket;
    expect(new Set(Object.keys(anchored.all))).toEqual(anchoredCats);
    // Anchoring can only narrow: every anchored `all` count is ≤ the whole-backlog one.
    for (const cat of Object.keys(anchored.all)) {
      expect(anchored.all[cat]).toBeLessThanOrEqual(whole.all[cat]);
    }
  });
});

// PRD #104: the mock must CASCADE a token delete the way the schema does.
// Migrations 00078/00079 hang composite FKs off user_secrets (user_id, id) with
// ON DELETE SET NULL, so deleting a bound token unbinds its workers and the judge.
// The mock previously only dropped the secret row, which left the shipped
// Dockerfile.mock demo showing D5's own promise being broken — and with one token
// left the picker hides, so there was no way to correct it in the UI. This is the
// only place D5's cascade is provable above the schema layer inside the SPA.
describe("mockApi token delete cascades like ON DELETE SET NULL (PRD #104 D5)", () => {
  it("unbinds every worker bound to the deleted token, and leaves the others alone", async () => {
    installStorage();
    const api = await reload();
    await api.login("admin@uzi.local", "whatever");

    const { secret } = await api.createAnthropicToken("sk-ant-mock-second", "cascade-key", false);
    const { workers } = await api.listWorkers();
    const target = workers[0];
    const other = workers[1];
    await api.setWorkerBindMode(target.id, "pinned", "cascade-key");

    const beforeList = (await api.listWorkers()).workers;
    const beforeOther = beforeList.find((w) => w.id === other?.id);
    expect(beforeList.find((w) => w.id === target.id)?.anthropic_secret_label).toBe("cascade-key");

    await api.deleteAnthropicTokenById(secret.id);

    const after = (await api.listWorkers()).workers.find((w) => w.id === target.id)!;
    expect(after.anthropic_secret_id).toBeNull();
    expect(after.anthropic_secret_label).toBeNull();
    // Deleting a token must not disturb a worker that was never bound to it.
    if (beforeOther) {
      const afterOther = (await api.listWorkers()).workers.find((w) => w.id === other.id)!;
      expect(afterOther.anthropic_secret_id).toBe(beforeOther.anthropic_secret_id);
    }
  });

  it("clears the judge binding when the judge's token is deleted", async () => {
    installStorage();
    const api = await reload();
    await api.login("admin@uzi.local", "whatever");

    const { secret } = await api.createAnthropicToken("sk-ant-mock-judge", "judge-only-key", false);
    const { user } = await api.setJudgeEnabled(true, "judge-only-key");
    expect(user.judge_anthropic_secret_label).toBe("judge-only-key");

    await api.deleteAnthropicTokenById(secret.id);

    const me = await api.me();
    expect(me.user.judge_anthropic_secret_id).toBeNull();
    expect(me.user.judge_anthropic_secret_label).toBeNull();
  });
});
