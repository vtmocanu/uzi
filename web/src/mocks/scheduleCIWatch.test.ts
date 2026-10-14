// @vitest-environment node
//
// The offline mock's ci_watch surface (PRD #2865): the ci-repair catalog entry, the seeded
// ci_watch default with two pages of episode state, and create/patch mirroring the
// server's ci_watch validator (blank prompt -> catalog body, auto_approve defaults off,
// issue fields and stray ci_workflows rejected).
import { describe, expect, it } from "vitest";
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
