import { describe, it, expect } from "vitest";
import { pipelineBadge, pipelineTitle, pipelineTone } from "./pipelineBadge";

describe("pipelineTone", () => {
  it("collapses the GitLab status set to five tones", () => {
    expect(pipelineTone("success")).toBe("passed");
    expect(pipelineTone("failed")).toBe("failed");
    // the whole in-flight family maps to running
    for (const s of ["created", "waiting_for_resource", "preparing", "pending", "running", "scheduled"]) {
      expect(pipelineTone(s)).toBe("running");
    }
    expect(pipelineTone("manual")).toBe("attention");
    expect(pipelineTone("canceled")).toBe("neutral");
    expect(pipelineTone("skipped")).toBe("neutral");
  });

  it("maps an unknown status to neutral instead of crashing", () => {
    expect(pipelineTone("some_future_status")).toBe("neutral");
    expect(pipelineTone("")).toBe("neutral");
  });
});

describe("pipelineBadge", () => {
  it("maps each tone to a shared Badge tone and pulse", () => {
    expect(pipelineBadge("success")).toEqual({ label: "passed", tone: "ok", pulse: false });
    expect(pipelineBadge("failed")).toEqual({ label: "failed", tone: "danger", pulse: false });
    // running is the only pulsing (live) state
    expect(pipelineBadge("running")).toEqual({ label: "running", tone: "info", pulse: true });
    expect(pipelineBadge("manual")).toEqual({ label: "manual", tone: "warning", pulse: false });
    expect(pipelineBadge("skipped")).toEqual({ label: "skipped", tone: "neutral", pulse: false });
  });

  it("humanizes underscored statuses in the label", () => {
    expect(pipelineBadge("waiting_for_resource").label).toBe("waiting for resource");
  });
});

describe("pipelineTitle", () => {
  const synced = "2026-07-06T12:00:00Z";
  it("names the exact status and how stale the badge is", () => {
    const now = Date.parse(synced) + 90_000; // 90s later
    expect(pipelineTitle("failed", synced, now)).toBe("Pipeline failed · synced 1m ago");
  });

  it("renders sub-minute, hour, and day granularities", () => {
    const base = Date.parse(synced);
    expect(pipelineTitle("success", synced, base + 5_000)).toContain("5s ago");
    expect(pipelineTitle("success", synced, base + 3 * 3_600_000)).toContain("3h ago");
    expect(pipelineTitle("success", synced, base + 2 * 86_400_000)).toContain("2d ago");
  });
});
