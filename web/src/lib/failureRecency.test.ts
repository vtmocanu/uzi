import { describe, expect, it } from "vitest";
import { failureRecency } from "./failureRecency";
import type { RunOutcomes } from "./apiTypes";

const empty: RunOutcomes = {
  finished: 0, completed: 0, cancelled: 0, plan_rejected: 0, failed: 0,
  needs_landing: 0, fail_origins: {}, last_failed_at: null,
  last_failed_run_id: null, last_failed_origin: null, last_failed_user_id: null,
  completed_since_last_failure: null,
};
describe("failure recency", () => {
  const now = Date.parse("2026-01-02T03:04:05Z");
  it.each([[-1, "<1m"], [0, "<1m"], [59, "<1m"], [60, "1m"], [2280, "38m"], [3600, "1h"], [21600, "6h"], [86400, "1d"], [259200, "3d"]])(
    "formats %s seconds elapsed as %s", (seconds, expected) => {
      expect(failureRecency({ ...empty, last_failed_at: new Date(now - Number(seconds) * 1000).toISOString() }, now)).toBe(expected);
    },
  );
  it("distinguishes idle, never-failed, and unavailable history", () => {
    expect(failureRecency(empty, now)).toBe("–");
    expect(failureRecency({ ...empty, finished: 3, completed: 3 }, now)).toBe("no failures");
    expect(failureRecency({ ...empty, failed: 1 }, now)).toBe("Unavailable");
    expect(failureRecency({ ...empty, last_failed_at: "bad date" }, now)).toBe("Unavailable");
  });
});
