// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import type { Worker } from "../lib/api";
import {
  FleetUpgradePanel,
  WorkerUpgradeBadge,
  WorkerUpgradeDetail,
  diagnosticsCommand,
  fleetSummary,
  likelyCause,
  needsAttention,
} from "./WorkerUpgradeBadge";

afterEach(cleanup);

function aWorker(over: Partial<Worker> = {}): Worker {
  return {
    id: "w1",
    name: "alpha",
    status: "online",
    kind: "external",
    hosted_size: null,
    busy: false,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: null,
    template_reported: null,
    version: "0.11.0",
    upgrade_status: "outdated",
    upgrade_detail: "running 0.11.0, target 0.11.7",
    upgrade_target: "0.11.7",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    last_heartbeat_at: null,
    created_at: new Date().toISOString(),
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    ...over,
  };
}

describe("WorkerUpgradeBadge", () => {
  it("renders NOTHING for unknown", () => {
    // An unstamped local image, an unparseable report and a `dev` control plane all
    // classify unknown. A badge on every worker of every local stack is how a reader
    // learns to stop reading badges.
    const { container } = render(<WorkerUpgradeBadge worker={aWorker({ upgrade_status: "unknown" })} />);
    expect(container.innerHTML).toBe("");
  });

  it("renders each real state with its own label", () => {
    for (const [status, label] of [
      ["up_to_date", "up to date"],
      ["outdated", "outdated"],
      ["upgrading", "upgrading"],
      ["upgrade_failed", "upgrade failed"],
    ] as const) {
      const { unmount } = render(<WorkerUpgradeBadge worker={aWorker({ upgrade_status: status })} />);
      expect(screen.getByText(label)).toBeTruthy();
      unmount();
    }
  });

  it("carries the api's own sentence as the title rather than composing its own", () => {
    render(<WorkerUpgradeBadge worker={aWorker()} />);
    // The badge and its explanation must not be able to disagree, so the detail is the
    // string the api derived — not one this component rebuilt from the same parts.
    expect(screen.getByText("outdated").getAttribute("title")).toBe("running 0.11.0, target 0.11.7");
  });
});

describe("the attention set (Decision 1)", () => {
  it("counts failed and behind, never upgrading", () => {
    expect(needsAttention(aWorker({ upgrade_status: "upgrade_failed" }))).toBe(true);
    expect(needsAttention(aWorker({ upgrade_status: "outdated" }))).toBe(true);
    // A roll in progress is expected, transient and self-resolving. Counting it is the
    // cry-wolf this PRD exists to avoid.
    expect(needsAttention(aWorker({ upgrade_status: "upgrading" }))).toBe(false);
    expect(needsAttention(aWorker({ upgrade_status: "up_to_date" }))).toBe(false);
    expect(needsAttention(aWorker({ upgrade_status: "unknown" }))).toBe(false);
  });
});

describe("FleetUpgradePanel — B-1, the target divergence", () => {
  it("states BOTH coordinates when a hosted target sits below the control plane", () => {
    render(
      <FleetUpgradePanel
        workers={[aWorker({ kind: "hosted", upgrade_target: "0.11.0", upgrade_status: "up_to_date" })]}
        cpVersion="0.11.7"
      />,
    );
    // The whole point of this panel: a controller reporting the fleet's own stale version
    // as the target makes every hosted worker read up_to_date indefinitely, and
    // `up_to_date` renders nothing — so without this line the suppression is invisible.
    // Assert the divergence SENTENCE, not just the word "target" — the panel heading
    // also says "target release", so a loose match here would pass with the divergence
    // line entirely absent.
    expect(screen.getByText(/not the control plane/)).toBeTruthy();
    expect(screen.getByText("v0.11.0")).toBeTruthy();
    expect(screen.getAllByText("v0.11.7").length).toBeGreaterThan(0);
  });

  it("says nothing when the hosted target matches the control plane", () => {
    render(
      <FleetUpgradePanel
        workers={[aWorker({ kind: "hosted", upgrade_target: "0.11.7", upgrade_status: "up_to_date" })]}
        cpVersion="0.11.7"
      />,
    );
    expect(screen.queryByText(/not the control plane/)).toBeNull();
  });

  it("does not treat an EXTERNAL worker's target as a fleet divergence", () => {
    // External workers are compared against the api's own version by construction, so
    // they can never diverge — and reporting a divergence for one would be a false claim
    // about how the hosted fleet is pinned.
    render(<FleetUpgradePanel workers={[aWorker({ kind: "external", upgrade_target: "0.11.0" })]} cpVersion="0.11.7" />);
    expect(screen.queryByText(/not the control plane/)).toBeNull();
  });

  it("counts the attention set and pluralises", () => {
    render(
      <FleetUpgradePanel
        workers={[
          aWorker({ id: "a", upgrade_status: "outdated" }),
          aWorker({ id: "b", upgrade_status: "upgrade_failed" }),
          aWorker({ id: "c", upgrade_status: "upgrading" }),
          aWorker({ id: "d", upgrade_status: "up_to_date" }),
        ]}
        cpVersion="0.11.7"
      />,
    );
    expect(screen.getByText("2 workers need attention.")).toBeTruthy();
  });

  it("says classification is off rather than inventing a target when the api is unstamped", () => {
    render(<FleetUpgradePanel workers={[aWorker({ upgrade_status: "unknown" })]} cpVersion="" />);
    expect(screen.getByText(/classification off/)).toBeTruthy();
  });
});

describe("fleetSummary", () => {
  it("excludes unknown from the segmented bar's denominator", () => {
    const s = fleetSummary(
      [aWorker({ id: "a", upgrade_status: "unknown" }), aWorker({ id: "b", upgrade_status: "outdated" })],
      "0.11.7",
    );
    expect(s.counts.unknown).toBe(1);
    expect(s.counts.outdated).toBe(1);
    // A bar whose denominator counted unclassifiable workers would render a fleet of
    // local dev workers as "50% healthy", which is a number about nothing.
    expect(s.attention).toBe(1);
  });
});

describe("the failed-worker strip", () => {
  it("renders only for upgrade_failed", () => {
    const { container } = render(<WorkerUpgradeDetail worker={aWorker({ upgrade_status: "outdated" })} />);
    expect(container.innerHTML).toBe("");
  });

  it("shows the api's detail, a likely cause, and a copyable command", () => {
    render(
      <WorkerUpgradeDetail
        worker={aWorker({
          upgrade_status: "upgrade_failed",
          upgrade_detail: "seed-nix: CrashLoopBackOff (6 restarts, last exit 2)",
          upgrade_blocking_container: "seed-nix",
          upgrade_blocking_reason: "CrashLoopBackOff",
        })}
      />,
    );
    expect(screen.getByText(/seed-nix: CrashLoopBackOff/)).toBeTruthy();
    expect(screen.getByText(/nix store reseed/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /copy kubectl/i })).toBeTruthy();
  });
});

describe("likelyCause", () => {
  it("is a closed table and says nothing when it has nothing to say", () => {
    // Silence is the correct output for an unmapped reason. A generic sentence would read
    // as a diagnosis, and the api forwards only the k8s reason — never `message` — so
    // there is nothing to base one on.
    expect(likelyCause("worker", "SomeReasonWeDoNotModel")).toBeNull();
    expect(likelyCause(null, null)).toBeNull();
  });

  it("distinguishes the seed-nix crash-loop from a generic one", () => {
    expect(likelyCause("seed-nix", "CrashLoopBackOff")).toMatch(/nix store reseed/);
    expect(likelyCause("worker", "CrashLoopBackOff")).toMatch(/starts and exits repeatedly/);
  });
});

describe("diagnosticsCommand", () => {
  it("selects on the label the controller actually stamps, and never asks for logs", () => {
    const cmd = diagnosticsCommand("11111111-1111-1111-1111-111111111111");
    // The selector must match kube/render.go's objectLabels, or the command returns
    // "No resources found" and reads as though the worker is gone.
    expect(cmd).toContain("-l uzi.dev/hosted-worker-id=11111111-1111-1111-1111-111111111111");
    expect(cmd).toContain("describe pod");
    // pods/log is refused for the controller because worker logs carry agent output over
    // a user's cloned private repo. Offering it here would imply access uzi does not
    // grant itself.
    expect(cmd).not.toContain("logs");
    // The namespace is a visible placeholder, not a guess: a docker-tier worker lives in
    // a different namespace and naming the wrong one fails like a missing worker.
    expect(cmd).toContain("<worker-namespace>");
  });
});
