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

  // PRD #65 R5: the merged map must fold in Forgejo's status strings without a
  // Forgejo failure or cancellation ever rendering as a benign badge.
  it("maps Forgejo Actions/commit statuses without rendering a red build benign", () => {
    // The two R5 traps: `failure` (not GitLab's `failed`) must be failed, and the
    // two-L `cancelled` must be recognised distinctly from GitLab's `canceled`.
    expect(pipelineTone("failure")).toBe("failed");
    expect(pipelineTone("cancelled")).toBe("neutral");
    expect(pipelineTone("canceled")).toBe("neutral"); // GitLab spelling still works
    // a commit-status error is a failure, never benign
    expect(pipelineTone("error")).toBe("failed");
    // Forgejo in-flight statuses read as running (live), not neutral
    for (const s of ["waiting", "blocked"]) {
      expect(pipelineTone(s)).toBe("running");
    }
    // shared strings mean the same thing on both forges
    expect(pipelineTone("success")).toBe("passed");
    expect(pipelineTone("skipped")).toBe("neutral");
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

  it("renders a failed Forgejo build as danger, not neutral (R5)", () => {
    expect(pipelineBadge("failure")).toEqual({ label: "failure", tone: "danger", pulse: false });
    expect(pipelineBadge("error")).toEqual({ label: "error", tone: "danger", pulse: false });
    expect(pipelineBadge("cancelled")).toEqual({ label: "cancelled", tone: "neutral", pulse: false });
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
