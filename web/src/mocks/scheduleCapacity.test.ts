// @vitest-environment node
import { afterEach, describe, expect, it } from "vitest";
import { mockApi } from "./mockApi";
import { state } from "./store";
import type { ScheduleInput } from "../lib/api";
const originalRuns = new Map(state.runs);
afterEach(() => { state.runs = new Map(originalRuns); });
const base: ScheduleInput = { target: "sweep", timing: "recurring", cron_expr: "0 2 * * *", labels: [] };
const gated = { ...base, capacity_limit: 4, capacity_room_needed: 2 };
describe("mock capacity contract", () => {
  it("creates omitted and explicit-null pairs ungated, preserves null N and fanout group", async () => {
    for (const input of [base, { ...base, capacity_limit: null, capacity_room_needed: null }]) {
      expect((await mockApi.createSchedule("repo-uzi", input)).capacity_limit).toBeNull();
    }
    const row = await mockApi.createSchedule("repo-uzi", { ...gated, max_issues: null, sibling_group_id: "capacity-group" });
    expect(row).toMatchObject({ capacity_limit: 4, capacity_room_needed: 2, max_issues: null, sibling_group_id: "capacity-group" });
  });
  it.each([
    { capacity_limit: 4 }, { capacity_room_needed: 2 },
    { capacity_limit: 4, capacity_room_needed: null },
    { capacity_limit: null, capacity_room_needed: 2 },
    { capacity_limit: 51, capacity_room_needed: 2 },
    { capacity_limit: 4.5, capacity_room_needed: 2 },
    { capacity_limit: 4, capacity_room_needed: 0 },
    { capacity_limit: 1, capacity_room_needed: 2 },
    { capacity_limit: 4, capacity_room_needed: 2, timing: "once" as const },
    { capacity_limit: 4, capacity_room_needed: 2, target: "prompt" as const },
  ])("rejects invalid create %j without inserting", async (patch) => {
    const before = (await mockApi.listSchedules()).length;
    await expect(mockApi.createSchedule("repo-uzi", { ...base, ...patch })).rejects.toMatchObject({ status: 400 });
    expect((await mockApi.listSchedules()).length).toBe(before);
  });
  it("merges omitted partners, retains omission, requires both null, and never mutates on error", async () => {
    const row = await mockApi.createSchedule("repo-uzi", gated);
    expect(await mockApi.updateSchedule(row.id, { capacity_limit: 5 })).toMatchObject({ capacity_limit: 5, capacity_room_needed: 2 });
    expect(await mockApi.updateSchedule(row.id, { enabled: false })).toMatchObject({ capacity_limit: 5, capacity_room_needed: 2 });
    for (const input of [{ capacity_limit: null }, { capacity_room_needed: null }, { capacity_limit: null, capacity_room_needed: 2 }, { capacity_limit: 1 }, { timing: "once" as const }, { target: "prompt" as const }]) {
      await expect(mockApi.updateSchedule(row.id, { ...input, enabled: true })).rejects.toMatchObject({ status: 400 });
      expect(await mockApi.getSchedule(row.id)).toMatchObject({ enabled: false, capacity_limit: 5, capacity_room_needed: 2, target: "sweep", timing: "recurring" });
    }
    expect(await mockApi.updateSchedule(row.id, { capacity_limit: null, capacity_room_needed: null })).toMatchObject({ capacity_limit: null, capacity_room_needed: null });
  });
  it("persists enabled with valid, omitted and cleared default capacity, retaining config on invalid writes", async () => {
    const row = await mockApi.enableCatalogSchedule("repo-uzi", "bug-triage");
    expect(row.origin).toBe("default");
    expect(await mockApi.updateSchedule(row.id, { enabled: false, capacity_limit: 4, capacity_room_needed: 2 })).toMatchObject({
      origin: "default", enabled: false, capacity_limit: 4, capacity_room_needed: 2, customized: true,
    });
    const retained = await mockApi.updateSchedule(row.id, { enabled: true });
    expect(retained).toMatchObject({ enabled: true, capacity_limit: 4, capacity_room_needed: 2, customized: true });
    for (const input of [
      { capacity_limit: null }, { capacity_room_needed: null },
      { capacity_limit: null, capacity_room_needed: 2 },
      { capacity_limit: 1 }, { capacity_limit: 51 }, { capacity_room_needed: 0 },
    ]) {
      await expect(mockApi.updateSchedule(row.id, {
        ...input, enabled: false, cron_expr: "0 3 * * *", max_issues: 1,
      })).rejects.toMatchObject({ status: 400 });
      expect(await mockApi.getSchedule(row.id)).toMatchObject({
        enabled: true, capacity_limit: 4, capacity_room_needed: 2, customized: true,
        target: retained.target, timing: retained.timing, labels: retained.labels,
        cron_expr: retained.cron_expr, max_issues: retained.max_issues, updated_at: retained.updated_at,
      });
    }
    expect(await mockApi.updateSchedule(row.id, {
      enabled: false, capacity_limit: null, capacity_room_needed: null,
    })).toMatchObject({ enabled: false, capacity_limit: null, capacity_room_needed: null, customized: false });
    expect(await mockApi.getSchedule(row.id)).toMatchObject({
      origin: "default", enabled: false, capacity_limit: null, capacity_room_needed: null, customized: false,
    });
  });
  it("validates default effective selectors, customizes, resets, clones and adds repos", async () => {
    const row = await mockApi.enableCatalogSchedule("repo-payments", "bug-triage");
    const updated = await mockApi.updateSchedule(row.id, { ...gated, target: undefined, timing: undefined, labels: undefined });
    expect(updated.customized).toBe(true);
    const clone = await mockApi.cloneSchedule(row.id);
    expect(clone).toMatchObject({ origin: "user", capacity_limit: 4, capacity_room_needed: 2 });
    expect(await mockApi.addScheduleRepo(clone.id, "repo-atlas")).toMatchObject({ capacity_limit: 4, capacity_room_needed: 2 });
    expect(await mockApi.resetSchedule(row.id)).toMatchObject({ capacity_limit: null, capacity_room_needed: null, customized: false });
    for (const slug of ["assigned-sweep", "docs-hygiene"]) {
      const unsupported = await mockApi.enableCatalogSchedule("repo-payments", slug);
      await expect(mockApi.updateSchedule(unsupported.id, { capacity_limit: 4, capacity_room_needed: 2 })).rejects.toMatchObject({ status: 400 });
    }
  });
  it("counts unfinished owner work across repos/kinds/statuses, clamps room and never persists Run Now", async () => {
    const sample = originalRuns.get("run-done")!;
    state.runs.clear();
    const cases = [
      ["a", "queued", "issue", "repo-uzi"],
      ["b", "awaiting_approval", "ci_fix", "repo-atlas"],
      ["c", "awaiting_input", "self_improve", null],
      ["job", "paused", "job", "repo-atlas"],
      ["chat", "running", "chat", null],
      ["judge", "running", "judge", null],
      ["done", "completed", "issue", "repo-uzi"],
      ["failed", "failed", "issue", "repo-uzi"],
      ["cancelled", "cancelled", "issue", "repo-uzi"],
    ] as const;
    for (const [id, status, kind, repo_id] of cases) state.runs.set(id, { ...sample, id, status, kind, repo_id });
    // The owner-attribution fixture excludes this active foreign run.
    const foreign = originalRuns.get("run-andrei-queued")!;
    expect(foreign.status).toBe("queued");
    state.runs.set(foreign.id, foreign);
    const row = await mockApi.createSchedule("repo-uzi", { ...gated, capacity_limit: 5, max_issues: null });
    const blocked = await mockApi.runScheduleNow(row.id);
    expect(blocked).toMatchObject({ created: 0, matched: 0, started: [], skips: [], capacity: { in_flight: 4, room: 1, blocked: true } });
    expect(await mockApi.getSchedule(row.id)).toMatchObject({ last_fire: null, last_fired_at: null });
    await mockApi.updateSchedule(row.id, { capacity_limit: 2 });
    expect((await mockApi.runScheduleNow(row.id)).capacity?.room).toBe(0);
    await mockApi.updateSchedule(row.id, { capacity_limit: 7 });
    const pass = await mockApi.runScheduleNow(row.id);
    expect(pass).toMatchObject({ created: 3, capacity: { in_flight: 4, room: 3, blocked: false } });
    await mockApi.updateSchedule(row.id, { max_issues: 10 });
    expect((await mockApi.runScheduleNow(row.id)).created).toBe(3);
    await mockApi.updateSchedule(row.id, { max_issues: 1 });
    expect((await mockApi.runScheduleNow(row.id)).created).toBe(1);
    expect(await mockApi.getSchedule(row.id)).toMatchObject({ last_fire: null, last_fired_at: null });
    expect(state.runs.size).toBe(cases.length + 1);
  });
});
