// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { ApiError } from "../lib/apiError";
import { mockApi } from "./mockApi";

afterEach(async () => {
  // Restore the seed so the shared settings blob does not leak into other tests.
  await mockApi.setWeeklyWarningMargin(15);
});

// PRD #4123 module 6: the mock twin of PUT /api/me/weekly-warning-margin
// (handler/weekly_warning_margin.go) and its GET /me/settings read-back.
describe("mockApi.setWeeklyWarningMargin (PRD #4123)", () => {
  it("seeds the server's default of 15 on the settings read", async () => {
    const { settings } = await mockApi.getMySettings();
    expect(settings.weekly_warning_margin_pts).toBe(15);
  });

  it("stores 0..50 and reads it back", async () => {
    expect(await mockApi.setWeeklyWarningMargin(0)).toEqual({ weekly_warning_margin_pts: 0 });
    expect((await mockApi.getMySettings()).settings.weekly_warning_margin_pts).toBe(0);
    expect((await mockApi.setWeeklyWarningMargin(50)).weekly_warning_margin_pts).toBe(50);
    expect((await mockApi.getMySettings()).settings.weekly_warning_margin_pts).toBe(50);
  });

  it("refuses what the server refuses, with its 400 message, and writes nothing", async () => {
    await mockApi.setWeeklyWarningMargin(20);
    for (const bad of [51, -1, 1.5, Number.NaN, "15" as unknown as number, null as unknown as number]) {
      const err = await mockApi.setWeeklyWarningMargin(bad).catch((e: unknown) => e);
      expect(err, String(bad)).toBeInstanceOf(ApiError);
      expect((err as ApiError).status).toBe(400);
      expect((err as ApiError).message).toBe("margin_pts must be an integer from 0 to 50");
    }
    const missing = await mockApi.setWeeklyWarningMargin(undefined as unknown as number).catch((e: unknown) => e);
    expect((missing as ApiError).message).toBe("margin_pts is required (an integer 0..50)");
    expect((await mockApi.getMySettings()).settings.weekly_warning_margin_pts).toBe(20);
  });

  // Example 1's running near-limit run, and the seven_day park beside it.
  it("margin 0 drops the near-limit block but keeps the park explanation", async () => {
    const near = () => mockApi.getRun("run-weekly-near").then((r) => r.run.weekly_limit);
    const park = () => mockApi.getRun("run-limit-wait-due").then((r) => r.run.weekly_limit);
    const summaryOf = async (id: string) =>
      (await mockApi.listRuns({ view: "summary" })).runs.find((r) => r.id === id)?.weekly_limit;

    expect((await near())?.tier).toBe("warning");
    expect(await summaryOf("run-weekly-near")).toEqual({
      tier: "warning",
      credential_key: "anthropic:sec-low",
      credential_label: "nearly-spent",
    });

    await mockApi.setWeeklyWarningMargin(0);
    expect(await near()).toBeUndefined();
    expect(await summaryOf("run-weekly-near")).toBeUndefined();
    expect((await park())?.tier).toBe("warning");
    expect((await summaryOf("run-limit-wait-due"))?.credential_key).toBe("anthropic:sec-console");
  });
});
