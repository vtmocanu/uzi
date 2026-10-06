import { describe, expect, it } from "vitest";

import { healthPipLabel, healthVerdict } from "./healthView";

// Use complete server-shaped documents through the public presentation seam.
import { incidentDoc, ownerOnlyDoc, degradedDoc, unknownDoc } from "../mocks/data/health";
import type { HealthDoc } from "./api";

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


describe("healthVerdict", () => {
  it("counts only instance danger and selects it after an owner danger", () => {
    const doc = incidentDoc();
    doc.checks = [...doc.checks.slice(1), doc.checks[0]];
    expect(healthVerdict(doc)).toMatchObject({
      blocking: true, title: "uzi cannot run work: 1 blocking check", cause: { id: "fleet.roll" },
    });
  });
  it("honors explicit false and keeps every attention item in the owner headline", () => {
    expect(healthVerdict(ownerOnlyDoc())).toMatchObject({
      blocking: false, title: "3 checks need attention; no instance-wide blocker detected",
    });
    const doc = { ...incidentDoc(), blocking: false };
    expect(healthVerdict(doc).blocking).toBe(false);
    expect(healthVerdict(doc).title).toBe("4 checks need attention; no instance-wide blocker detected");
    doc.checks = doc.checks.filter((c) => c.id === "fleet.capacity");
    expect(healthVerdict(doc).title).toBe("1 check needs attention; no instance-wide blocker detected");
  });
  it("preserves the old contract tally and first danger without scope inference", () => {
    const { blocking: _blocking, ...old } = ownerOnlyDoc();
    expect(healthVerdict(old as HealthDoc)).toMatchObject({
      blocking: true, title: "uzi cannot run work: 2 blocking checks", cause: { id: "fleet.capacity" },
    });
  });
  it("uses neutral attention copy for warnings and unknowns", () => {
    expect(healthVerdict(degradedDoc()).title).toBe("2 checks need attention");
    expect(healthVerdict(unknownDoc()).title).toBe("4 checks need attention");
  });
});
