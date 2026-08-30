// @vitest-environment node
//
// The offline mock's default-jobs surface (PRD #589 M6): listScheduleCatalog +
// enable/reset/clone + labels check/ensure, exercised directly against the real mock so
// its branches (idempotent enable, already-paused no-op, reset-to-catalog, clone-to-user,
// sweep-label check/ensure) are covered and the seeded Layout-A demo state is pinned. The
// mock ships seeded schedules/repos and starts signed in as admin, so no setup is needed.
import { describe, expect, it } from "vitest";
import { mockApi } from "./mockApi";

describe("mock default-jobs catalog (PRD #589)", () => {
  it("lists the 8 catalog entries and seeds a visible Layout-A demo state", async () => {
    const { entries, enablements } = await mockApi.listScheduleCatalog();
    expect(entries.map((e) => e.slug).sort()).toEqual(
      ["assigned-sweep", "bug-hunt", "bug-triage", "docs-hygiene", "feature-bingo", "planned-sweep", "self-improve", "test-improvement"].sort(),
    );
    // The assigned sweep selects by assignee, not by label, so its catalog labels stay null.
    expect(entries.find((e) => e.slug === "assigned-sweep")?.labels).toBeNull();
    // bug-triage is enabled on TWO repos (Layout A), one of them paused.
    const bt = enablements.filter((e) => e.slug === "bug-triage");
    expect(bt.map((e) => e.repo_id).sort()).toEqual(["repo-atlas", "repo-uzi"]);
    expect(bt.find((e) => e.repo_id === "repo-atlas")?.enabled).toBe(false);
    // docs-hygiene is enabled + customized on repo-uzi.
    expect(enablements.some((e) => e.slug === "docs-hygiene" && e.repo_id === "repo-uzi")).toBe(true);
  });

  it("enableCatalogSchedule materializes a default row and is idempotent (incl. an already-paused repo)", async () => {
    // A fresh repo materializes a new origin='default' row with the resolved catalog values.
    const created = await mockApi.enableCatalogSchedule("repo-payments", "bug-triage");
    expect(created.origin).toBe("default");
    expect(created.catalog_slug).toBe("bug-triage");
    expect(created.labels).toEqual(["bug"]);
    expect(created.enabled).toBe(true);

    // Re-enabling the same (repo, slug) returns the existing row, not a duplicate.
    const again = await mockApi.enableCatalogSchedule("repo-payments", "bug-triage");
    expect(again.id).toBe(created.id);

    // Re-enabling an already-PAUSED repo returns the paused row untouched (server no-op) —
    // the resume toggle, not a fresh enable, is the correct affordance.
    const paused = await mockApi.enableCatalogSchedule("repo-atlas", "bug-triage");
    expect(paused.enabled).toBe(false);
  });

  it("enableCatalogSchedule seeds the timezone override on a fresh row, ignores it on re-enable (issue #660)", async () => {
    // A fresh materialize with an explicit tz stores it on the created row (the browser
    // zone parity the create modal already has).
    const created = await mockApi.enableCatalogSchedule("repo-www", "bug-triage", "Europe/Bucharest");
    expect(created.timezone).toBe("Europe/Bucharest");

    // A plain enable (no tz) on a GENUINELY FRESH (repo, slug) — not an already-materialized
    // one, which would hit the idempotent re-enable branch and mask a wrong fresh default —
    // keeps the catalog/default zone. (repo-atlas, docs-hygiene) is unmaterialized in seed and
    // untouched by every other test here.
    const plain = await mockApi.enableCatalogSchedule("repo-atlas", "docs-hygiene");
    const entry = (await mockApi.listScheduleCatalog()).entries.find((e) => e.slug === "docs-hygiene")!;
    expect(plain.timezone).toBe(entry.timezone);

    // An invalid IANA name on a fresh enable rejects with a 400, mirroring the production
    // handler (before this the mock stored any string unchecked).
    await expect(mockApi.enableCatalogSchedule("repo-atlas", "planned-sweep", "Not/AZone")).rejects.toMatchObject({
      status: 400,
    });

    // The idempotent re-enable with a DIFFERENT tz does NOT clobber the stored zone
    // (mirrors the server's ON CONFLICT DO NOTHING — the AC in commit 265313a4).
    const again = await mockApi.enableCatalogSchedule("repo-www", "bug-triage", "America/New_York");
    expect(again.id).toBe(created.id);
    expect(again.timezone).toBe("Europe/Bucharest");
  });

  it("resetSchedule restores a customized default's cadence and clears customized", async () => {
    const before = (await mockApi.listSchedules()).find(
      (s) => s.origin === "default" && s.catalog_slug === "docs-hygiene" && s.repo_id === "repo-uzi",
    );
    expect(before?.customized).toBe(true);
    expect(before?.cron_expr).not.toBe("0 3 * * 1"); // shifted away from the catalog

    const reset = await mockApi.resetSchedule(before!.id);
    expect(reset.customized).toBe(false);
    expect(reset.cron_expr).toBe("0 3 * * 1"); // back to the catalog cron
  });

  it("cloneSchedule turns a default into an editable user row with the prompt baked in", async () => {
    const src = (await mockApi.listSchedules()).find(
      (s) => s.origin === "default" && s.catalog_slug === "docs-hygiene",
    );
    const clone = await mockApi.cloneSchedule(src!.id);
    expect(clone.origin).toBe("user");
    expect(clone.catalog_slug).toBeNull();
    // docs-hygiene is a prompt default, so the baked prompt carries over, now editable.
    expect(clone.prompt).toBe(src!.prompt);
    expect(clone.prompt.length).toBeGreaterThan(0);
    expect(clone.id).not.toBe(src!.id);
  });

  it("updateSchedule REJECTS a default patch carrying any catalog-owned field (mock == server 400)", async () => {
    // The server's patchDefaultScheduleConfig 400s ANY default patch whose body carries a
    // catalog-owned field (prompt/labels/target/timing/repo_id/issue_iid, plus guidance for a
    // non-prompt default). The mock must agree — the drift that let ScheduleModal ship a
    // `timing:"recurring"` default patch that passed CI but 400s the real backend. This test is
    // the regression fence. The `def` picked here is a SWEEP default (first seeded). Guidance
    // is NO LONGER catalog-owned for a sweep default (issue #675: it is the owner overlay,
    // owner-editable), so guidance is covered by its own accept test below, not here.
    const def = (await mockApi.listSchedules()).find(
      (s) => s.origin === "default" && !!s.catalog_slug && s.target === "sweep",
    );
    expect(def).toBeTruthy();

    // timing is the field that actually broke it, first.
    await expect(mockApi.updateSchedule(def!.id, { timing: "recurring" })).rejects.toMatchObject({ status: 400 });
    await expect(mockApi.updateSchedule(def!.id, { prompt: "x" })).rejects.toMatchObject({ status: 400 });
    await expect(mockApi.updateSchedule(def!.id, { target: "prompt" })).rejects.toMatchObject({ status: 400 });
    await expect(mockApi.updateSchedule(def!.id, { labels: ["bug"] })).rejects.toMatchObject({ status: 400 });
    await expect(mockApi.updateSchedule(def!.id, { issue_iid: 5 })).rejects.toMatchObject({ status: 400 });
    await expect(mockApi.updateSchedule(def!.id, { repo_id: "repo-atlas" })).rejects.toMatchObject({ status: 400 });

    // The editable-field path still works: cron/tz/model/flags/max_issues go through, and a
    // divergence from the catalog values flips `customized` (matching the server).
    const okUpdated = await mockApi.updateSchedule(def!.id, {
      cron_expr: "0 6 * * 3",
      timezone: "UTC",
      auto_approve: false,
      wait_on_limit: false,
      model: null,
    });
    expect(okUpdated.cron_expr).toBe("0 6 * * 3");
    expect(okUpdated.customized).toBe(true);
  });

  it("updateSchedule ACCEPTS + persists owner guidance on a PROMPT default and sets customized (issue #662)", async () => {
    // A prompt default is owner-editable for guidance (unlike sweep/issue defaults): the
    // server overlays it onto the catalog prompt at fire time. Materialize a fresh prompt
    // default (docs-hygiene) on a clean repo so it starts un-customized with no guidance.
    const def = await mockApi.enableCatalogSchedule("repo-payments", "docs-hygiene");
    expect(def.origin).toBe("default");
    expect(def.target).toBe("prompt");
    expect(def.customized).toBe(false);

    // The web always sends the full editable config on a default patch, so guidance rides
    // along with cadence/model/flags. A prompt catalog job carries empty guidance, so any
    // non-empty stored guidance flips customized (mirroring the server's guidance.Valid OR).
    const updated = await mockApi.updateSchedule(def.id, {
      cron_expr: def.cron_expr,
      timezone: def.timezone,
      auto_approve: def.auto_approve,
      wait_on_limit: def.wait_on_limit,
      model: null,
      guidance: "prefer the smallest safe change",
    });
    expect(updated.guidance).toBe("prefer the smallest safe change");
    expect(updated.customized).toBe(true);

    // Clearing guidance back to none (explicit null) restores it AND un-customizes, since
    // every editable field is back at the catalog values (exact-restore un-customizes).
    const cleared = await mockApi.updateSchedule(def.id, {
      cron_expr: def.cron_expr,
      timezone: def.timezone,
      auto_approve: def.auto_approve,
      wait_on_limit: def.wait_on_limit,
      model: null,
      guidance: null,
    });
    expect(cleared.guidance).toBeNull();
    expect(cleared.customized).toBe(false);
  });

  it("updateSchedule ACCEPTS + persists an owner guidance OVERLAY on a SWEEP default and sets customized (issue #675)", async () => {
    // Issue #675 splits a sweep default's guidance: the catalog value is the read-only
    // baked_guidance, and `guidance` is an owner-editable OVERLAY the server appends at fire
    // time (like the prompt-default overlay of #662). So a sweep-default patch carrying
    // guidance is ACCEPTED, and the OVERLAY persists while baked_guidance stays the catalog
    // value. Materialize a fresh planned-sweep default on a clean repo so it starts
    // un-customized with a null overlay and the catalog guidance baked in.
    const def = await mockApi.enableCatalogSchedule("repo-www", "planned-sweep");
    expect(def.origin).toBe("default");
    expect(def.target).toBe("sweep");
    expect(def.customized).toBe(false);
    expect(def.guidance).toBeNull(); // no owner overlay yet
    expect(def.baked_guidance).toBeTruthy(); // catalog guidance baked in, read-only
    const baked = def.baked_guidance;

    const updated = await mockApi.updateSchedule(def.id, {
      cron_expr: def.cron_expr,
      timezone: def.timezone,
      auto_approve: def.auto_approve,
      wait_on_limit: def.wait_on_limit,
      max_issues: def.max_issues,
      model: null,
      guidance: "prefer a failing test first, then the smallest fix",
    });
    expect(updated.guidance).toBe("prefer a failing test first, then the smallest fix");
    // The baked catalog guidance is untouched by the overlay patch (the two are independent).
    expect(updated.baked_guidance).toBe(baked);
    expect(updated.customized).toBe(true);

    // Clearing the overlay back to none (explicit null) restores it AND un-customizes, since
    // every editable field is back at the catalog values, and baked_guidance stays intact.
    const cleared = await mockApi.updateSchedule(def.id, {
      cron_expr: def.cron_expr,
      timezone: def.timezone,
      auto_approve: def.auto_approve,
      wait_on_limit: def.wait_on_limit,
      max_issues: def.max_issues,
      model: null,
      guidance: null,
    });
    expect(cleared.guidance).toBeNull();
    expect(cleared.baked_guidance).toBe(baked);
    expect(cleared.customized).toBe(false);
  });

  it("resetSchedule clears a SWEEP default's owner overlay, keeps baked_guidance, and un-customizes (issue #675)", async () => {
    // The RESET path (distinct from update-clear): resetSchedule re-materializes the default
    // from the catalog, so it drops the owner overlay (guidance -> null) while keeping the
    // read-only baked catalog guidance, and clears customized. Materialize a fresh
    // planned-sweep default on a clean repo (repo-www already carries one from the sweep-overlay
    // test above, so use a different owned repo) so it starts un-customized with a null overlay.
    const def = await mockApi.enableCatalogSchedule("repo-payments", "planned-sweep");
    expect(def.origin).toBe("default");
    expect(def.target).toBe("sweep");
    expect(def.customized).toBe(false);
    expect(def.guidance).toBeNull(); // no owner overlay yet
    expect(def.baked_guidance).toBeTruthy(); // catalog guidance baked in, read-only
    const baked = def.baked_guidance;

    const updated = await mockApi.updateSchedule(def.id, {
      cron_expr: def.cron_expr,
      timezone: def.timezone,
      auto_approve: def.auto_approve,
      wait_on_limit: def.wait_on_limit,
      max_issues: def.max_issues,
      model: null,
      guidance: "prefer a failing test first, then the smallest fix",
    });
    expect(updated.customized).toBe(true);

    // Resetting re-materializes from the catalog: the overlay is cleared, the baked catalog
    // guidance is unchanged, and the row is no longer customized.
    const reset = await mockApi.resetSchedule(def.id);
    expect(reset.guidance).toBeNull();
    expect(reset.baked_guidance).toBe(baked);
    expect(reset.customized).toBe(false);
  });

  it("REJECTS guidance over the 8 KiB cap by BYTES not chars, on both write paths (issue #662, mock == server 422)", async () => {
    // The server caps guidance at 8 KiB (MaxGuidanceBytes) and 422s an oversize value on EVERY
    // write path; the mock must reproduce that so mock-mode users hit the same error rather than
    // a silent success. Use MULTIBYTE boundaries so a regression to character-count validation is
    // caught: "é" is 2 UTF-8 bytes, so 4096 of them are exactly 8192 bytes (only 4096 chars), and
    // 8191 ASCII chars + one "é" is 8193 bytes (only 8192 chars).
    const atCap = "é".repeat(4096); // 8192 bytes, 4096 chars
    const overCap = "a".repeat(8191) + "é"; // 8193 bytes, 8192 chars
    expect(new TextEncoder().encode(atCap).length).toBe(8192);
    expect(new TextEncoder().encode(overCap).length).toBe(8193);

    // update path — a prompt default's owner guidance.
    const def = await mockApi.enableCatalogSchedule("repo-payments", "docs-hygiene");
    expect(def.target).toBe("prompt");
    const base = {
      cron_expr: def.cron_expr,
      timezone: def.timezone,
      auto_approve: def.auto_approve,
      wait_on_limit: def.wait_on_limit,
      model: null,
    };
    const updated = await mockApi.updateSchedule(def.id, { ...base, guidance: atCap });
    expect(updated.guidance).toBe(atCap);
    await expect(
      mockApi.updateSchedule(def.id, { ...base, guidance: overCap }),
    ).rejects.toMatchObject({ status: 422 });

    // create path — issue/sweep guidance is validated too (not only updates).
    const created = await mockApi.createSchedule("repo-payments", {
      target: "issue",
      issue_iid: 142,
      timing: "recurring",
      cron_expr: "0 3 * * 1",
      guidance: atCap,
    });
    expect(created.guidance).toBe(atCap);
    await expect(
      mockApi.createSchedule("repo-payments", {
        target: "issue",
        issue_iid: 143,
        timing: "recurring",
        cron_expr: "0 3 * * 1",
        guidance: overCap,
      }),
    ).rejects.toMatchObject({ status: 422 });
  });

  it("checkRepoLabels flags a missing selector label and ensureRepoLabels creates it", async () => {
    // repo-atlas is seeded without "bug", so a bug-triage sweep would match nothing there.
    expect((await mockApi.checkRepoLabels("repo-atlas", ["bug"])).missing).toEqual(["bug"]);
    expect((await mockApi.ensureRepoLabels("repo-atlas", ["bug"])).ensured).toEqual(["bug"]);
    // After ensuring, the label exists and the warn clears.
    expect((await mockApi.checkRepoLabels("repo-atlas", ["bug"])).missing).toEqual([]);
  });
});

// ── PRD #636 M3: addScheduleRepo server-parity (findings review follow-up) ─────
describe("mock addScheduleRepo — server parity (PRD #636)", () => {
  it("copies the SOURCE's enabled onto the sibling: a paused source yields a paused sibling", async () => {
    // sch-zt88 is a seeded PAUSED (enabled:false) user schedule on repo-atlas. The server
    // copies cur.Enabled onto the new sibling, so adding a repo from it must yield a PAUSED
    // sibling — not an unconditionally-enabled one (the pre-fix mock hardcoded enabled:true).
    const src = (await mockApi.listSchedules()).find((s) => s.id === "sch-zt88");
    expect(src?.origin).toBe("user");
    expect(src?.enabled).toBe(false);

    const sibling = await mockApi.addScheduleRepo("sch-zt88", "repo-payments");
    expect(sibling.enabled).toBe(false); // inherited from the paused source
    expect(sibling.status).toBe("active"); // fresh-row status default (server + clone parity)
    expect(sibling.repo_id).toBe("repo-payments");
    expect(sibling.sibling_group_id).toBeTruthy(); // a group id was coalesced
  });

  it("returns 409 (not 404) for a non-user (default-origin) source", async () => {
    // The server returns 409 ("only a custom schedule can add a repo; clone a default first")
    // for a default-origin source, mirroring ResetSchedule's origin-mismatch conflict — the
    // pre-fix mock wrongly folded this into the owner-scope 404.
    const def = (await mockApi.listSchedules()).find((s) => s.origin === "default" && !!s.catalog_slug);
    expect(def).toBeTruthy();
    await expect(mockApi.addScheduleRepo(def!.id, "repo-payments")).rejects.toMatchObject({ status: 409 });
  });

  it("keeps the owner-scope 404 for an absent source and the duplicate-repo 409", async () => {
    // An unknown source id is still a 404 (owner scope), distinct from the origin 409 above.
    await expect(mockApi.addScheduleRepo("nope-does-not-exist", "repo-payments")).rejects.toMatchObject({
      status: 404,
    });
    // Adding the same repo twice from a user source hits the partial unique index → 409.
    const first = await mockApi.addScheduleRepo("sch-pr0m", "repo-atlas");
    expect(first.sibling_group_id).toBeTruthy();
    await expect(mockApi.addScheduleRepo("sch-pr0m", "repo-atlas")).rejects.toMatchObject({ status: 409 });
  });
});
