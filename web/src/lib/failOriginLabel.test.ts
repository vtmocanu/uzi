import { describe, expect, it } from "vitest";
import { failOriginLabel } from "./failOriginLabel";

describe("failOriginLabel", () => {
  it("labels the plan_missing origin (issue #1593)", () => {
    expect(failOriginLabel("plan_missing")).toBe("plan missing");
  });

  it("labels the data_volume_full origin (PRD #1809)", () => {
    expect(failOriginLabel("data_volume_full")).toBe("data volume full");
  });

  it("labels the task_undispatched origin (issue #1367)", () => {
    expect(failOriginLabel("task_undispatched")).toBe("task undispatched");
  });

  it("labels a known origin", () => {
    expect(failOriginLabel("push_secret_blocked")).toBe("push secret blocked");
  });

  it("falls back to the raw name for an origin it does not know yet", () => {
    expect(failOriginLabel("some_future_origin")).toBe("some_future_origin");
  });
});
