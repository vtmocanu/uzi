// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import type { Worker } from "../lib/api";
import { WorkerCordonBadge } from "./WorkerCordonBadge";

afterEach(cleanup);

function aWorker(over: Partial<Worker> = {}): Worker {
  return {
    id: "w1",
    name: "alpha",
    status: "online",
    kind: "hosted",
    hosted_size: "m",
    busy: false,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: null,
    template_reported: null,
    version: "0.11.0",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.11.0",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: null,
    created_at: new Date().toISOString(),
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    stats_disk_nix_bytes: null,
    stats_disk_nix_total_bytes: null,
    stats_disk_data_bytes: null,
    stats_disk_data_total_bytes: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    draining_since: null,
    ...over,
  };
}

describe("WorkerCordonBadge", () => {
  // The absence and a positive together, in ONE test, so the "renders nothing" half is
  // anchored to the current pill wording — a copy/label change that broke the pill would
  // strand a standalone negative passing forever against something that no longer renders.
  it("renders nothing without draining_since, and the pill once it is set", () => {
    const { container } = render(<WorkerCordonBadge worker={aWorker({ draining_since: null })} />);
    expect(container.innerHTML).toBe("");

    render(<WorkerCordonBadge worker={aWorker({ draining_since: "2026-08-21T12:00:00Z", active_runs: 0 })} />);
    expect(screen.getByText("cordoned")).toBeTruthy();
  });

  describe("splits the label on active_runs, not busy", () => {
    it("reads 'draining' while it still holds runs", () => {
      render(<WorkerCordonBadge worker={aWorker({ draining_since: "2026-08-21T12:00:00Z", active_runs: 1 })} />);
      expect(screen.getByText("draining")).toBeTruthy();
      expect(screen.queryByText("cordoned")).toBeNull();
    });

    it("reads 'cordoned' once its runs are drained", () => {
      render(<WorkerCordonBadge worker={aWorker({ draining_since: "2026-08-21T12:00:00Z", active_runs: 0 })} />);
      expect(screen.getByText("cordoned")).toBeTruthy();
      expect(screen.queryByText("draining")).toBeNull();
    });

    // The whole reason the label keys on active_runs and not busy: a chat-only cordoned
    // worker holds a run of a non-run kind, so `busy` is true while `active_runs` is 0.
    // Keying on `busy` would mislabel it "draining" / "finishing its current runs" while
    // it holds zero runs — this case is exactly the one that mutation reddens.
    it("reads 'cordoned' for a chat-only worker (busy:true, active_runs:0)", () => {
      render(
        <WorkerCordonBadge worker={aWorker({ draining_since: "2026-08-21T12:00:00Z", busy: true, active_runs: 0 })} />,
      );
      expect(screen.getByText("cordoned")).toBeTruthy();
      expect(screen.queryByText("draining")).toBeNull();
    });
  });

  describe("carries the Decision-4 title copy verbatim", () => {
    // jsdom reads the title ATTRIBUTE, which a browser/screenshot pass structurally
    // cannot: a native `title` tooltip is invisible until hover, so its VALUE — em dash
    // and all — is only ever checkable here, never in a visual pass (mirrors
    // WorkerUpgradeBadge.test.tsx's title-attribute note).
    it("says it is finishing its current runs while draining", () => {
      render(<WorkerCordonBadge worker={aWorker({ draining_since: "2026-08-21T12:00:00Z", active_runs: 2 })} />);
      expect(screen.getByText("draining").getAttribute("title")).toBe(
        "Cordoned — finishing its current runs, not claiming new ones.",
      );
    });

    it("says only that it claims no new runs once drained", () => {
      render(<WorkerCordonBadge worker={aWorker({ draining_since: "2026-08-21T12:00:00Z", active_runs: 0 })} />);
      expect(screen.getByText("cordoned").getAttribute("title")).toBe("Cordoned — not claiming new runs.");
    });
  });
});
