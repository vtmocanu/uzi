import { describe, expect, it } from "vitest";
import { failOriginLabel } from "./failOriginLabel";

describe("failOriginLabel", () => {
  it("labels the plan_missing origin (issue #1593)", () => {
    expect(failOriginLabel("plan_missing")).toBe("plan missing");
  });

  it("labels the worker_residue_blocked origin (issue #1783)", () => {
    expect(failOriginLabel("worker_residue_blocked")).toBe("worker residue blocked");
  });

  it("labels a known origin", () => {
    expect(failOriginLabel("push_secret_blocked")).toBe("push secret blocked");
  });

  it("falls back to the raw name for an origin it does not know yet", () => {
    expect(failOriginLabel("some_future_origin")).toBe("some_future_origin");
  });
});
