import { describe, expect, it } from "vitest";
import { failOriginLabel } from "./failOriginLabel";

describe("failOriginLabel", () => {
  it("labels the plan_missing origin (issue #1593)", () => {
    expect(failOriginLabel("plan_missing")).toBe("plan missing");
  });

  it("labels the worker_residue_blocked origin (issue #1783)", () => {
    expect(failOriginLabel("worker_residue_blocked")).toBe("worker residue blocked");
  });

  it("labels the skills_plugin_load_failed origin (issue #1888)", () => {
    expect(failOriginLabel("skills_plugin_load_failed")).toBe("skills plugin load failed");
  });

  it("labels the repo-less job origins (PRD #1908)", () => {
    expect(failOriginLabel("no_job_capable_worker")).toBe("no job-capable worker");
    expect(failOriginLabel("ephemeral_worker_never_registered")).toBe("ephemeral worker never registered");
    expect(failOriginLabel("job_no_result")).toBe("job reported no result");
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
