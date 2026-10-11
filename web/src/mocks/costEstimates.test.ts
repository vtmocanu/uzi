import { expect, it } from "vitest";
import { mockReviews } from "./data/judge";
import { mockRuns } from "./data/runs";
import { runListItem } from "./data/runHistories";
import { mockApi } from "./mockApi";

it("Claude judge fixtures record SDK API-equivalent estimates", () => {
  const usage = mockReviews.find((r) => r.judge_run?.judge_run_id === "judge-run-closed")!.judge_run!.usage!;
  expect(usage.cost_status).toBe("metered");
  expect(usage.cost_usd).toBe(0.12);
});

it("priced Codex checker fixture matches the independently expected DTO", () => {
  expect(mockRuns.find((r) => r.id === "run-awaiting")!.plan_cross_check_summary!.usage).toEqual({
    input_tokens: 300, output_tokens: 200, cache_read_tokens: 600, cache_creation_tokens: 100,
    cost_usd: 0.00291, cost_status: "metered",
  });
});

it("keeps a visible legacy row and an unavailable mixed row with recorded partial dollars", () => {
  const legacy = runListItem(mockRuns.find((r) => r.id === "run-closed")!);
  expect(legacy.usage).toMatchObject({ cost_status: "subscription", cost_usd: 0 });
  const mixed = runListItem(mockRuns.find((r) => r.id === "run-failed")!);
  expect(mixed.usage).toMatchObject({ cost_status: "unreported", cost_usd: 0.61 });
});

it("detail fixtures retain the list cost status and partial amounts", async () => {
  for (const id of ["run-closed", "run-failed"]) {
    const listed = runListItem(mockRuns.find((r) => r.id === id)!);
    const { run } = await mockApi.getRun(id);
    expect(run.usage).toEqual(listed.usage);
  }
});

it("aggregate fixtures retain the mixed run's partial contribution and separate counts", async () => {
  const [self, admin] = await Promise.all([mockApi.getUsage(), mockApi.getAdminUsage()]);
  expect(self.lifetime.cost_usd).toBe(27.01);
  expect(self.last_7_days.cost_usd).toBe(5.16);
  expect(self.lifetime_unreported_run_count).toBe(1);
  expect(self.last7_unreported_run_count).toBe(1);
  expect(admin.factory.lifetime.cost_usd).toBe(88.76);
  expect(admin.factory.last_7_days.cost_usd).toBe(15.51);
  expect(admin.factory.lifetime_unreported_run_count).toBe(1);
  expect(admin.factory.last7_unreported_run_count).toBe(1);
  expect(admin.users.find((u) => u.user_id === "u-vlad")!.usage.cost_usd).toBe(27.01);
});
