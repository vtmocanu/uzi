// @vitest-environment node
import { afterEach, describe, expect, it } from "vitest";
import { mockApi } from "./mockApi";
import { state } from "./store";
import { appSettings } from "./mockApi/settings";
import type { ScheduleInput } from "../lib/api";

const base: ScheduleInput = { target: "sweep", timing: "recurring", cron_expr: "0 2 * * *", labels: ["on-deck"] };
const originalLabel = appSettings.uzi_label;
const originalRuns = new Map(state.runs);
afterEach(() => { appSettings.uzi_label = originalLabel; state.runs = new Map(originalRuns); });

describe("mock selector removal contract", () => {
  it("defaults false, preserves omitted PATCH and clears explicit false", async () => {
    expect(await mockApi.createSchedule("repo-uzi", base)).toMatchObject({ remove_label_on_dispatch: false });
    const row = await mockApi.createSchedule("repo-uzi", { ...base, remove_label_on_dispatch: true });
    expect(await mockApi.updateSchedule(row.id, { enabled: false })).toMatchObject({ remove_label_on_dispatch: true });
    expect(await mockApi.updateSchedule(row.id, { remove_label_on_dispatch: false })).toMatchObject({ remove_label_on_dispatch: false });
    expect(await mockApi.updateSchedule(row.id, { enabled: true })).toMatchObject({ remove_label_on_dispatch: false });
  });
  it.each([
    { target: "issue" as const }, { target: "prompt" as const }, { timing: "once" as const },
    { labels: [] }, { labels: ["bug", "other"] }, { labels: [" uzi "] }, { labels: [" "] },
  ])("refuses create and merged PATCH %j atomically", async (patch) => {
    const before = (await mockApi.listSchedules()).length;
    await expect(mockApi.createSchedule("repo-uzi", { ...base, ...patch, remove_label_on_dispatch: true })).rejects.toMatchObject({ status: 400 });
    expect((await mockApi.listSchedules()).length).toBe(before);
    const row = await mockApi.createSchedule("repo-uzi", { ...base, remove_label_on_dispatch: true });
    await expect(mockApi.updateSchedule(row.id, { ...patch, enabled: false })).rejects.toMatchObject({ status: 400 });
    expect(await mockApi.getSchedule(row.id)).toMatchObject({ labels: ["on-deck"], target: "sweep", timing: "recurring", enabled: true, remove_label_on_dispatch: true });
    expect(await mockApi.updateSchedule(row.id, { ...patch, remove_label_on_dispatch: false })).toMatchObject({ remove_label_on_dispatch: false });
  });
  it("normalizes labels and refuses live custom eligibility labels, clone and add-repo alike", async () => {
    const row = await mockApi.createSchedule("repo-uzi", { ...base, labels: [" on-deck ", ""], remove_label_on_dispatch: true });
    expect(row.labels).toEqual(["on-deck"]);
    expect(await mockApi.cloneSchedule(row.id)).toMatchObject({ remove_label_on_dispatch: true });
    expect(await mockApi.addScheduleRepo(row.id, "repo-atlas")).toMatchObject({ remove_label_on_dispatch: true });
    appSettings.uzi_label = "on-deck";
    expect((await mockApi.runScheduleNow(row.id)).started?.[0]).toMatchObject({ selector_label: "on-deck", label_removed: false, label_remove_failed: false });
    await expect(mockApi.cloneSchedule(row.id)).rejects.toMatchObject({ status: 400 });
    await expect(mockApi.addScheduleRepo(row.id, "repo-payments")).rejects.toMatchObject({ status: 400 });
    expect(await mockApi.updateSchedule(row.id, { enabled: false })).toMatchObject({ enabled: false, remove_label_on_dispatch: true });
    expect(await mockApi.updateSchedule(row.id, { enabled: true })).toMatchObject({ enabled: true, remove_label_on_dispatch: true });
    expect(await mockApi.getSchedule(row.id)).toMatchObject({ enabled: true, remove_label_on_dispatch: true });
    await expect(mockApi.updateSchedule(row.id, { cron_expr: "0 3 * * *" })).rejects.toMatchObject({ status: 400 });
    expect(await mockApi.getSchedule(row.id)).toMatchObject({ cron_expr: base.cron_expr, enabled: true, remove_label_on_dispatch: true });
    expect(await mockApi.updateSchedule(row.id, { remove_label_on_dispatch: false, enabled: false })).toMatchObject({ enabled: false, remove_label_on_dispatch: false });
    await expect(mockApi.createSchedule("repo-uzi", { ...base, remove_label_on_dispatch: true })).rejects.toMatchObject({ status: 400 });
    expect(await mockApi.createSchedule("repo-uzi", { ...base, labels: ["uzi"], remove_label_on_dispatch: true })).toMatchObject({ remove_label_on_dispatch: true });
  });
  it("uses catalog effective selectors and older entries’ false enable/reset baselines", async () => {
    const row = await mockApi.enableCatalogSchedule("repo-payments", "bug-triage");
    expect(row.remove_label_on_dispatch).toBe(false);
    for (const enabled of [false, true]) {
      expect(await mockApi.updateSchedule(row.id, { enabled })).toMatchObject({ enabled, remove_label_on_dispatch: false, customized: false });
    }
    expect(await mockApi.updateSchedule(row.id, { remove_label_on_dispatch: true })).toMatchObject({ remove_label_on_dispatch: true, customized: true });
    expect(await mockApi.updateSchedule(row.id, { enabled: false })).toMatchObject({ remove_label_on_dispatch: true, customized: true });
    expect(await mockApi.cloneSchedule(row.id)).toMatchObject({ origin: "user", labels: ["bug"], remove_label_on_dispatch: true });
    expect(await mockApi.updateSchedule(row.id, { remove_label_on_dispatch: false })).toMatchObject({ customized: false });
    await mockApi.updateSchedule(row.id, { remove_label_on_dispatch: true });
    expect(await mockApi.resetSchedule(row.id)).toMatchObject({ remove_label_on_dispatch: false, customized: false, enabled: false });
    for (const slug of ["assigned-sweep", "docs-hygiene"]) {
      const unsupported = await mockApi.enableCatalogSchedule("repo-payments", slug);
      await expect(mockApi.updateSchedule(unsupported.id, { remove_label_on_dispatch: true })).rejects.toMatchObject({ status: 400 });
      expect(await mockApi.updateSchedule(unsupported.id, { remove_label_on_dispatch: false })).toMatchObject({ remove_label_on_dispatch: false });
    }
    await mockApi.updateSchedule(row.id, { remove_label_on_dispatch: true });
    appSettings.uzi_label = "bug";
    for (const enabled of [false, true]) {
      expect(await mockApi.updateSchedule(row.id, { enabled })).toMatchObject({ enabled, remove_label_on_dispatch: true, customized: true });
      expect(await mockApi.getSchedule(row.id)).toMatchObject({ enabled, remove_label_on_dispatch: true, customized: true });
    }
    await expect(mockApi.updateSchedule(row.id, { cron_expr: "0 3 * * *" })).rejects.toMatchObject({ status: 400 });
    expect(await mockApi.getSchedule(row.id)).toMatchObject({ enabled: true, remove_label_on_dispatch: true, customized: true });
    expect(await mockApi.updateSchedule(row.id, { remove_label_on_dispatch: false, enabled: false })).toMatchObject({ enabled: false, remove_label_on_dispatch: false, customized: false });
    await expect(mockApi.updateSchedule(row.id, { remove_label_on_dispatch: true })).rejects.toMatchObject({ status: 400 });
  });
  it("returns snapshots and success/failure demo flags without claiming forge writes or persisting manual fires", async () => {
    for (const failed of [false, true]) {
      const row = await mockApi.getSchedule(failed ? "sch-removal-failed" : "sch-removal-success");
      const expected = { selector_label: "on-deck", label_removed: !failed, label_remove_failed: failed };
      expect(row.last_fire?.started[0]).toMatchObject(expected);
      const result = await mockApi.runScheduleNow(row.id);
      expect(result.started?.[0]).toMatchObject(expected);
      expect((await mockApi.getSchedule(row.id)).last_fire).toEqual(row.last_fire);
    }
    const row = await mockApi.createSchedule("repo-uzi", base);
    expect((await mockApi.runScheduleNow(row.id)).started?.[0]).not.toHaveProperty("selector_label");
    expect((await mockApi.getSchedule(row.id)).last_fire).toBeNull();
  });
});


describe("On-deck catalog baseline", () => {
  const baseline = { remove_label_on_dispatch: true, capacity_limit: 4,
    capacity_room_needed: 2, max_issues: 1, cron_expr: "*/10 * * * *", customized: false };
  it("enables, edits, restores exactly, resets, clones and copies the stored values", async () => {
    const row = await mockApi.enableCatalogSchedule("repo-ledger", "ondeck-sweep");
    expect(row).toMatchObject(baseline);
    expect(row.labels).toEqual(["on-deck"]);
    expect(await mockApi.updateSchedule(row.id, { capacity_limit: 5 })).toMatchObject({
      capacity_limit: 5, capacity_room_needed: 2, customized: true });
    expect(await mockApi.updateSchedule(row.id, { capacity_limit: 4 })).toMatchObject(baseline);
    expect(await mockApi.updateSchedule(row.id, { remove_label_on_dispatch: false })).toMatchObject({ customized: true });
    expect(await mockApi.updateSchedule(row.id, { remove_label_on_dispatch: true })).toMatchObject(baseline);
    await mockApi.updateSchedule(row.id, { capacity_limit: null, capacity_room_needed: null, remove_label_on_dispatch: false });
    await mockApi.updateSchedule(row.id, { enabled: false });
    const reset = await mockApi.resetSchedule(row.id);
    expect(reset).toMatchObject({ ...baseline, enabled: false });
    const clone = await mockApi.cloneSchedule(row.id);
    expect(clone).toMatchObject({ ...baseline, origin: "user", catalog_slug: null, labels: ["on-deck"] });
    expect(await mockApi.addScheduleRepo(clone.id, "repo-atlas")).toMatchObject({
      ...baseline, origin: "user", labels: ["on-deck"] });
    expect((await mockApi.runScheduleNow(row.id)).capacity?.blocked).toBe(true);
    state.runs.clear();
    expect((await mockApi.runScheduleNow(row.id)).started?.[0]).toMatchObject({
      selector_label: "on-deck", label_removed: true, label_remove_failed: false });
  });
  it("checks the live uzi label on fresh enable/reset but returns existing rows first", async () => {
    const row = await mockApi.enableCatalogSchedule("repo-payments", "ondeck-sweep");
    appSettings.uzi_label = "on-deck";
    expect(await mockApi.enableCatalogSchedule("repo-payments", "ondeck-sweep", "Europe/Berlin")).toMatchObject({ id: row.id });
    await expect(mockApi.enableCatalogSchedule("repo-uzi", "ondeck-sweep")).rejects.toMatchObject({ status: 400 });
    await expect(mockApi.resetSchedule(row.id)).rejects.toMatchObject({ status: 400 });
    expect(await mockApi.getSchedule(row.id)).toMatchObject(baseline);
    expect(await mockApi.updateSchedule(row.id, { enabled: false })).toMatchObject({ ...baseline, enabled: false });
    expect(await mockApi.updateSchedule(row.id, { enabled: true })).toMatchObject({ ...baseline, enabled: true });
    state.runs.clear();
    expect((await mockApi.runScheduleNow(row.id)).started?.[0]).toMatchObject({
      selector_label: "on-deck", label_removed: false, label_remove_failed: false });
  });
});
