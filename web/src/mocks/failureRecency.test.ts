// @vitest-environment jsdom
import { afterEach, expect, it } from "vitest";
import { createElement } from "react";
import { cleanup, render, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { UsageCard } from "../components/UsageCards";
import { mockApi } from "./mockApi";

afterEach(cleanup);

it("demo shows matching failure recency, a valid drill-in, and a never-failed user", async () => {
  const [self, admin] = await Promise.all([mockApi.getUsage(), mockApi.getAdminUsage()]);
  const factory = admin.factory.outcomes.lifetime;
  const mine = admin.users.find((u) => u.user_id === "u-vlad")!;
  expect(self.outcomes.lifetime.last_failed_at).toEqual(mine.outcomes.last_failed_at);
  expect(typeof factory.last_failed_at).toBe("string");
  expect(factory.last_failed_at).toEqual(admin.users.map((u) => u.outcomes.last_failed_at).filter((at): at is string => at != null).sort().slice(-1)[0]);
  expect(admin.users.find((u) => u.outcomes.last_failed_at === factory.last_failed_at)?.user_id).toBe(factory.last_failed_user_id);
  const clean = admin.users.find((u) => u.outcomes.finished > 0 && u.outcomes.failed === 0)!;
  expect(clean).toBeDefined();
  expect(clean.outcomes.last_failed_at).toBeNull();
  expect(clean.outcomes.completed_since_last_failure).toBeNull();
  const { run } = await mockApi.getRun(factory.last_failed_run_id!);
  expect(run.status).toBe("failed");
  expect(run.finished_at).toBe(factory.last_failed_at);
  expect(factory.last_failed_origin).toBe("workflow_scope_missing");
  expect(Object.values(factory.fail_origins).reduce((sum, count) => sum + count, 0)).toBe(factory.failed);
  expect(admin.users.reduce((sum, u) => sum + u.outcomes.completed, 0)).toBe(factory.completed);
  expect(self.outcomes.last_7_days.last_failed_at).toBeNull();
  // Billing populations remain consistent across self, factory, and user rows.
  expect(self.lifetime_subscription_run_count).toBe(mine.subscription_run_count);
  expect(self.last7_subscription_run_count).toBeGreaterThan(0);
  expect(self.last7_subscription_run_count).toBeLessThanOrEqual(self.lifetime_subscription_run_count);
  expect(admin.factory.lifetime_subscription_run_count).toBe(admin.users.reduce((sum, user) => sum + user.subscription_run_count, 0));
  expect(admin.factory.last7_subscription_run_count).toBeGreaterThan(0);
  expect(admin.factory.last7_subscription_run_count).toBeLessThanOrEqual(admin.factory.lifetime_subscription_run_count);
  expect(admin.users.filter((user) => user.subscription_run_count > 0).length).toBe(2);
  for (const user of admin.users) {
    expect(user.subscription_run_count).toBeLessThanOrEqual(user.run_count);
    expect(user.unreported_run_count).toBe(0);
  }
});

it("demo users partition every factory metric in both windows", async () => {
  const [self, admin] = await Promise.all([mockApi.getUsage(), mockApi.getAdminUsage()]);
  const sum = (values: number[]) => values.reduce((a, b) => a + b, 0);
  const mine = admin.users.find((u) => u.user_id === "u-vlad")!;
  expect(mine.last_7_days).toEqual(self.last_7_days);
  expect(mine.last7_outcomes).toEqual(self.outcomes.last_7_days);
  expect(mine.last7_subscription_run_count).toBe(self.last7_subscription_run_count);
  for (const field of ["input_tokens", "cache_read_tokens", "cache_creation_tokens", "output_tokens", "cost_usd"] as const) {
    expect(sum(admin.users.map((u) => u.usage[field]))).toBeCloseTo(admin.factory.lifetime[field], 8);
    expect(sum(admin.users.map((u) => u.last_7_days![field]))).toBeCloseTo(admin.factory.last_7_days[field], 8);
  }
  for (const field of ["finished", "completed", "cancelled", "plan_rejected", "failed", "needs_landing"] as const) {
    expect(sum(admin.users.map((u) => u.outcomes[field]))).toBe(admin.factory.outcomes.lifetime[field]);
    expect(sum(admin.users.map((u) => u.last7_outcomes![field]))).toBe(admin.factory.outcomes.last_7_days[field]);
  }
  expect(sum(admin.users.map((u) => u.run_count))).toBe(admin.factory.run_count);
  expect(sum(admin.users.map((u) => u.subscription_run_count))).toBe(admin.factory.lifetime_subscription_run_count);
  expect(sum(admin.users.map((u) => u.unreported_run_count))).toBe(admin.factory.lifetime_unreported_run_count);
  expect(sum(admin.users.map((u) => u.last7_subscription_run_count!))).toBe(admin.factory.last7_subscription_run_count);
  expect(sum(admin.users.map((u) => u.last7_unreported_run_count!))).toBe(admin.factory.last7_unreported_run_count);
  const origins: Record<string, number> = {};
  const lifetimeOrigins: Record<string, number> = {};
  for (const u of admin.users) {
    expect(u.last7_run_count).toBeGreaterThanOrEqual(u.last7_subscription_run_count! + u.last7_unreported_run_count!);
    expect(u.last7_run_count).toBeLessThanOrEqual(u.run_count);
    const o = u.last7_outcomes!;
    expect(o.finished).toBe(o.completed + o.cancelled + o.plan_rejected + o.failed);
    expect(sum(Object.values(o.fail_origins))).toBe(o.failed);
    for (const [origin, count] of Object.entries(o.fail_origins)) origins[origin] = (origins[origin] ?? 0) + count;
    for (const [origin, count] of Object.entries(u.outcomes.fail_origins)) lifetimeOrigins[origin] = (lifetimeOrigins[origin] ?? 0) + count;
  }
  expect(origins).toEqual(admin.factory.outcomes.last_7_days.fail_origins);
  expect(lifetimeOrigins).toEqual(admin.factory.outcomes.lifetime.fail_origins);
});

it("demo renders selected-window cost exclusions and seven-day per-user totals", async () => {
  const [self, admin] = await Promise.all([mockApi.getUsage(), mockApi.getAdminUsage()]);
  const { getByRole, getByText } = render(createElement(MemoryRouter, null,
    createElement(UsageCard, { self, admin, window: "last_7_days", onWindowChange: () => {} }),
  ));
  const personal = within(getByRole("region", { name: "Your usage" }));
  expect(personal.getByText("excl. 2 subscription runs").getAttribute("title")).toBe("Cost excludes 2 Codex subscription runs");
  expect(getByRole("heading", { name: "Per user · last 7 days" })).toBeTruthy();
  const totalRow = getByText("uzi total").closest("tr")!;
  expect(within(totalRow).getByText("Cost excludes 5 Codex subscription runs")).toBeTruthy();
});
