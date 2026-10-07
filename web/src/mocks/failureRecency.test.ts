// @vitest-environment jsdom
import { expect, it } from "vitest";
import { mockApi } from "./mockApi";

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
});
