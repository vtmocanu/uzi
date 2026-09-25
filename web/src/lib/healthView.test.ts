import { describe, expect, it } from "vitest";

import { healthPipLabel } from "./healthView";

const zero = { ok: 10, warn: 0, danger: 0, unknown: 0, na: 2 };

// PRD #1648 D9: the attention pips' accessible name lists every non-zero severity, worst
// first, and never counts ok or na.
describe("healthPipLabel", () => {
  it("danger only", () => {
    expect(healthPipLabel({ ...zero, danger: 3 })).toBe("3 health checks need attention: 3 danger");
  });

  it("unknown only (not folded into warning)", () => {
    expect(healthPipLabel({ ...zero, unknown: 4 })).toBe("4 health checks need attention: 4 unknown");
  });

  it("mixed, in danger / unknown / warning order, skipping zeros", () => {
    expect(healthPipLabel({ ...zero, warn: 2, danger: 1, unknown: 5 })).toBe(
      "8 health checks need attention: 1 danger, 5 unknown, 2 warning",
    );
    expect(healthPipLabel({ ...zero, warn: 1, danger: 3 })).toBe("4 health checks need attention: 3 danger, 1 warning");
  });

  it("N = 1 reads in the singular", () => {
    expect(healthPipLabel({ ...zero, warn: 1 })).toBe("1 health check needs attention: 1 warning");
    expect(healthPipLabel({ ...zero, danger: 1 })).toBe("1 health check needs attention: 1 danger");
  });
});
