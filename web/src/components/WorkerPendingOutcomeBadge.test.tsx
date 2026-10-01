// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { WorkerPendingOutcomeBadge } from "./WorkerPendingOutcomeBadge";
import type { WorkerReportedRun } from "../lib/api";

afterEach(cleanup);

// Issue #1994: a reported_runs entry flagged terminal_pending is an outcome journaled on the
// worker but not yet delivered, not a live run. The pill must appear only for such entries,
// count them, and explain each in its title.

const NOW = Date.parse("2026-10-01T12:00:00Z");

function entry(over: Partial<WorkerReportedRun> = {}): WorkerReportedRun {
  return { run_id: "r-live", phase: "running", claim_generation: 1, ...over };
}

describe("WorkerPendingOutcomeBadge", () => {
  it("renders nothing when reported_runs is absent (older api / mock)", () => {
    const { container } = render(<WorkerPendingOutcomeBadge worker={{}} nowMs={NOW} />);
    expect(container.innerHTML).toBe("");
  });

  it("renders nothing for an empty snapshot", () => {
    const { container } = render(<WorkerPendingOutcomeBadge worker={{ reported_runs: [] }} nowMs={NOW} />);
    expect(container.innerHTML).toBe("");
  });

  it("renders nothing when every entry is live (false or omitted flag)", () => {
    const { container } = render(
      <WorkerPendingOutcomeBadge
        worker={{
          reported_runs: [
            entry({ run_id: "a", terminal_pending: false, terminal_pending_since: null }),
            entry({ run_id: "b" }),
          ],
        }}
        nowMs={NOW}
      />,
    );
    expect(container.innerHTML).toBe("");
  });

  it("renders the singular label with run id, generation and pending age in the title", () => {
    render(
      <WorkerPendingOutcomeBadge
        worker={{
          reported_runs: [
            entry({ run_id: "live-1" }),
            entry({
              run_id: "11111111-2222-3333-4444-555555555555",
              claim_generation: 3,
              terminal_pending: true,
              terminal_pending_since: "2026-10-01T11:48:00Z",
            }),
          ],
        }}
        nowMs={NOW}
      />,
    );
    const pill = screen.getByText("1 pending outcome");
    const title = pill.getAttribute("title") ?? "";
    expect(title).toContain("Outcome journaled on this worker but not yet delivered to the api");
    expect(title).toContain("run 11111111-2222-3333-4444-555555555555 (generation 3), pending 12m");
    // The live entry is not listed.
    expect(title).not.toContain("live-1");
  });

  it("renders the plural label and omits the age when terminal_pending_since is null", () => {
    render(
      <WorkerPendingOutcomeBadge
        worker={{
          reported_runs: [
            entry({ run_id: "p1", claim_generation: 2, terminal_pending: true, terminal_pending_since: "2026-10-01T09:30:00Z" }),
            entry({ run_id: "p2", claim_generation: 5, terminal_pending: true, terminal_pending_since: null }),
          ],
        }}
        nowMs={NOW}
      />,
    );
    const title = screen.getByText("2 pending outcomes").getAttribute("title") ?? "";
    expect(title).toContain("run p1 (generation 2), pending 2h 30m");
    const p2 = title.split("\n").find((l) => l.startsWith("run p2"));
    expect(p2).toBe("run p2 (generation 5)");
  });

  it("strips format characters from a run id", () => {
    render(
      <WorkerPendingOutcomeBadge
        worker={{ reported_runs: [entry({ run_id: "ab‮cd", terminal_pending: true, terminal_pending_since: null })] }}
        nowMs={NOW}
      />,
    );
    const title = screen.getByText("1 pending outcome").getAttribute("title") ?? "";
    expect(title).toContain("run abcd (generation 1)");
    expect(title).not.toMatch(/[\p{Cf}]/u);
  });
});
