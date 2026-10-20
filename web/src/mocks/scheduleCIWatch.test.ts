// @vitest-environment node
//
// The offline mock's ci_watch surface (PRD #2865): the ci-repair catalog entry, the seeded
// ci_watch default with two pages of episode state, and create/patch mirroring the
// server's ci_watch validator (blank prompt -> catalog body, auto_approve defaults off,
// issue fields and stray ci_workflows rejected).
import { describe, expect, it } from "vitest";
import { validArtifactSelectors, invalidArtifactSelectors } from "../lib/ciArtifacts.testVectors";
import { mockApi } from "./mockApi";

describe("mock ci_watch schedules (PRD #2865)", () => {
  it("ships the ci-repair catalog entry with plan review on", async () => {
    const { entries } = await mockApi.listScheduleCatalog();
    const ci = entries.find((e) => e.slug === "ci-repair");
    expect(ci).toMatchObject({ target: "ci_watch", auto_approve: false, cron: "0 * * * *" });
    expect(ci?.prompt).toMatch(/^Treat all forge evidence/);
  });

  it("carries episodes only on the detail read, paged two pages deep", async () => {
    const listed = (await mockApi.listSchedules()).find((s) => s.id === "sch-def-ci-uzi");
    expect(listed?.target).toBe("ci_watch");
    expect(listed?.ci_episodes).toBeUndefined();

    const detail = await mockApi.getSchedule("sch-def-ci-uzi");
    const first = detail.ci_episodes!;
    expect(first.episodes.map((e) => e.workflow_key)).toEqual(["ci.yml", "codeql.yml", "docs.yml"]);
    expect(first.next_cursor).not.toBeNull();

    const second = await mockApi.listScheduleCIEpisodes("sch-def-ci-uzi", first.next_cursor!);
    expect(second.episodes.map((e) => e.workflow_key)).toEqual(["kind-smoke.yml", "release.yml"]);
    expect(second.next_cursor).toBeNull();
  });

  it("seeds only episode shapes the server writes", async () => {
    const first = (await mockApi.getSchedule("sch-def-ci-uzi")).ci_episodes!;
    const rest = await mockApi.listScheduleCIEpisodes("sch-def-ci-uzi", first.next_cursor!);
    const rows = [...first.episodes, ...rest.episodes];
    for (const e of rows) {
      // Start, halt and reset all set the watermark to the observed run.
      expect(e.observed_run_id).not.toBeNull();
      expect(e.last_handled_run_id).toBe(e.observed_run_id);
      const isReset = e.attempts === 0 && e.last_signature === null && e.halted_reason === null;
      const isStartOrHalt = e.last_signature !== null && (e.attempts > 0 || e.halted_reason !== null);
      expect(isReset || isStartOrHalt, e.workflow_key).toBe(true);
    }
    // kind-smoke.yml is reset-shaped, matching the last fire's ci_green skip.
    expect(rows.find((e) => e.workflow_key === "kind-smoke.yml")).toMatchObject({
      attempts: 0, last_signature: null, halted_reason: null,
    });
    const detail = await mockApi.getSchedule("sch-def-ci-uzi");
    expect(detail.last_fire?.skips).toContainEqual(expect.objectContaining({ title: "kind-smoke.yml", reason: "ci_green" }));
  });

  it("rejects a malformed cursor with a 400", async () => {
    await expect(mockApi.listScheduleCIEpisodes("sch-def-ci-uzi", "%%%")).rejects.toMatchObject({ status: 400 });
  });

  it("creates a ci_watch schedule with the catalog prompt and manual approve by default", async () => {
    const s = await mockApi.createSchedule("repo-uzi", {
      target: "ci_watch", timing: "recurring", cron_expr: "0 * * * *", ci_workflows: [" ci.yml "],
    });
    expect(s.auto_approve).toBe(false);
    expect(s.prompt).toMatch(/^Treat all forge evidence/);
    expect(s.ci_workflows).toEqual(["ci.yml"]);
    // An explicit choice still wins, and other targets keep the auto-approve default.
    const on = await mockApi.createSchedule("repo-uzi", { target: "ci_watch", cron_expr: "0 * * * *", auto_approve: true });
    expect(on.auto_approve).toBe(true);
    const prompt = await mockApi.createSchedule("repo-uzi", { target: "prompt", prompt: "x", cron_expr: "0 * * * *" });
    expect(prompt.auto_approve).toBe(true);
  });

  it("rejects issue fields on ci_watch and ci_workflows elsewhere", async () => {
    await expect(
      mockApi.createSchedule("repo-uzi", { target: "ci_watch", cron_expr: "0 * * * *", issue_iid: 3 }),
    ).rejects.toMatchObject({ status: 400 });
    await expect(
      mockApi.createSchedule("repo-uzi", { target: "sweep", cron_expr: "0 * * * *", ci_workflows: ["ci.yml"] }),
    ).rejects.toMatchObject({ status: 400 });
    await expect(
      mockApi.createSchedule("repo-uzi", { target: "ci_watch", cron_expr: "0 * * * *", ci_workflows: ["a", "a"] }),
    ).rejects.toMatchObject({ status: 400 });
  });

  it("clears a default's workflow selector with [] back to all workflows", async () => {
    const s = await mockApi.updateSchedule("sch-def-ci-uzi", { ci_workflows: [] });
    expect(s.ci_workflows).toBeUndefined();
  });
});


describe("artifact selectors", () => {
  for (const { input, expected } of validArtifactSelectors) {
    it(`normalizes valid artifact selectors ${JSON.stringify(input).slice(0, 60)}`, async () => {
      const s = await mockApi.createSchedule("repo-uzi", { target: "ci_watch", ci_artifacts: input });
      expect(s.ci_artifacts).toEqual(expected);
    });
  }
  for (const input of invalidArtifactSelectors) {
    it(`rejects invalid artifact selectors ${JSON.stringify(input).slice(0, 60)}`, async () => {
      await expect(mockApi.createSchedule("repo-uzi", { target: "ci_watch", ci_artifacts: input })).rejects.toMatchObject({ status: 400 });
      await expect(mockApi.updateSchedule("sch-def-ci-uzi", { ci_artifacts: input })).rejects.toMatchObject({ status: 400 });
    });
  }
  it("preserves omitted PATCH, clears null/[], rejects other targets, and clears on conversion", async () => {
    for (const ci_artifacts of [undefined, null, []]) {
      const empty = await mockApi.createSchedule("repo-uzi", { target: "ci_watch", ci_artifacts });
      expect(empty.ci_artifacts ?? []).toEqual([]);
    }
    const s = await mockApi.createSchedule("repo-uzi", { target: "ci_watch", ci_artifacts: ["logs-*"] });
    expect((await mockApi.updateSchedule(s.id, { timezone: "UTC" })).ci_artifacts).toEqual(["logs-*"]);
    for (const clear of [null, []]) {
      await mockApi.updateSchedule(s.id, { ci_artifacts: ["logs-*"] });
      expect((await mockApi.updateSchedule(s.id, { ci_artifacts: clear })).ci_artifacts ?? []).toEqual([]);
    }
    await expect(mockApi.createSchedule("repo-uzi", { target: "prompt", prompt: "x", ci_artifacts: ["logs"] })).rejects.toMatchObject({ status: 400 });
    await mockApi.updateSchedule(s.id, { ci_artifacts: ["logs-*"] });
    await expect(mockApi.updateSchedule(s.id, { target: "prompt", prompt: "x", ci_artifacts: ["logs"] })).rejects.toMatchObject({ status: 400 });
    expect((await mockApi.updateSchedule(s.id, { target: "prompt", prompt: "x" })).ci_artifacts).toBeUndefined();
  });
  it("keeps selectors through clone, replication and repoint, and resets catalog customization", async () => {
    const def = await mockApi.enableCatalogSchedule("repo-atlas", "ci-repair");
    expect(def.ci_artifacts ?? []).toEqual([]);
    const custom = await mockApi.updateSchedule(def.id, { ci_artifacts: ["logs-*"] });
    expect(custom.customized).toBe(true);
    const clone = await mockApi.cloneSchedule(def.id);
    expect(clone.ci_artifacts).toEqual(["logs-*"]);
    const sibling = await mockApi.addScheduleRepo(clone.id, "repo-ledger");
    expect(sibling.ci_artifacts).toEqual(["logs-*"]);
    expect((await mockApi.updateSchedule(clone.id, { repo_id: "repo-uzi" })).ci_artifacts).toEqual(["logs-*"]);
    const reset = await mockApi.resetSchedule(def.id);
    expect(reset.ci_artifacts ?? []).toEqual([]);
    expect(reset.customized).toBe(false);
    expect((await mockApi.getSchedule(clone.id)).ci_artifacts).toEqual(["logs-*"]);
  });
});
