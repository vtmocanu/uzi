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

it("demo renders selected-window cost exclusions and all-time per-user totals", async () => {
  const [self, admin] = await Promise.all([mockApi.getUsage(), mockApi.getAdminUsage()]);
  const { getByRole, getByText } = render(createElement(MemoryRouter, null,
    createElement(UsageCard, { self, admin, window: "last_7_days", onWindowChange: () => {} }),
  ));
  const personal = within(getByRole("region", { name: "Your usage" }));
  expect(personal.getByText("excl. 2 subscription runs").getAttribute("title")).toBe("Cost excludes 2 Codex subscription runs");
  expect(getByText("Per-user figures are all time.")).toBeTruthy();
  const totalRow = getByText("uzi total").closest("tr")!;
  expect(within(totalRow).getByText("Cost excludes 20 Codex subscription runs")).toBeTruthy();
});
