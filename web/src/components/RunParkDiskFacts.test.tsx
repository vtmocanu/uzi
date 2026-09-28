// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { RunParkDiskFacts, checkpointDurabilityText, runDiskSizeText } from "./RunParkDiskFacts";
import type { Run, RunStatus } from "../lib/api";

afterEach(cleanup);

// Only the fields these helpers read; the rest of Run is irrelevant here.
function aRun(over: Partial<Run>): Run {
  return { id: "r1", status: "paused", ...over } as Run;
}

describe("checkpointDurabilityText (PRD #1809 M6)", () => {
  it("states both report values on every park status", () => {
    for (const status of ["limit_wait", "recovery_wait", "paused"] as RunStatus[]) {
      expect(checkpointDurabilityText(aRun({ status, checkpoint_contains_latest: true }))).toBe(
        "The published checkpoint contains the latest work.",
      );
      expect(checkpointDurabilityText(aRun({ status, checkpoint_contains_latest: false }))).toMatch(
        /does not contain the latest committed work; the worker keeps that work/,
      );
    }
  });

  it("says nothing when not parked (the flag survives a resume) or not reported", () => {
    expect(checkpointDurabilityText(aRun({ status: "running", checkpoint_contains_latest: false }))).toBeNull();
    expect(checkpointDurabilityText(aRun({ status: "paused" }))).toBeNull();
  });
});

describe("runDiskSizeText (PRD #1809 M6)", () => {
  const GIB = 1073741824;
  it("renders home and cache sizes, and a lower bound when truncated", () => {
    expect(runDiskSizeText(aRun({ home_bytes: 4 * GIB, cache_bytes: 3 * GIB }))).toBe(
      "Home on the worker: 4 GiB, including 3 GiB of rebuildable caches.",
    );
    expect(runDiskSizeText(aRun({ home_bytes: 4 * GIB, cache_bytes: GIB, disk_truncated: true }))).toBe(
      "Home on the worker: at least 4 GiB, including 1 GiB of rebuildable caches.",
    );
  });

  it("renders nothing without a reported size", () => {
    expect(runDiskSizeText(aRun({}))).toBeNull();
  });
});

describe("RunParkDiskFacts", () => {
  it("renders both lines for a parked run with a report and a size", () => {
    render(<RunParkDiskFacts run={aRun({ status: "recovery_wait", checkpoint_contains_latest: false, home_bytes: 1024 })} />);
    expect(screen.getByText(/does not contain the latest committed work/)).toBeTruthy();
    expect(screen.getByText("Home on the worker: 1 KiB.")).toBeTruthy();
  });

  it("renders nothing when there is nothing to say", () => {
    const { container } = render(<RunParkDiskFacts run={aRun({ status: "running" })} />);
    expect(container.firstChild).toBeNull();
  });
});
