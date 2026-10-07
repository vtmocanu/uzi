import { describe, expect, it } from "vitest";
import { mockAdminWorkers, mockWorkers } from "./workers";

describe("mock worker custody decisions", () => {
  it.each([
    ["owner", mockWorkers],
    ["admin", mockAdminWorkers],
  ] as const)("includes healthy retention and an owner decision in the %s list", (_scope, workers) => {
    const healthy = workers.find((w) => w.id === "w-laptop");
    expect(healthy).toMatchObject({
      status: "online", busy: true, active_runs: 1,
      retaining_unpublished_work: true, custody_decisions_needed: 0,
    });
    const decision = workers.find((w) => w.id === "w-nas");
    expect(decision).toMatchObject({
      status: "online", busy: false, active_runs: 0,
      retaining_unpublished_work: true, custody_decisions_needed: 1,
    });
  });
});
