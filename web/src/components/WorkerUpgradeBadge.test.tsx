// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import type { Worker } from "../lib/api";
// ?raw rather than node:fs — the web tsconfig has no node types, and adding @types/node to
// widen a browser project's type surface for one test would be a poor trade. Vite inlines
// these at build time, so the assertion runs against the real files in both tsc and vitest.
import tailwindConfigSource from "../../tailwind.config.js?raw";
import componentSource from "./WorkerUpgradeBadge.tsx?raw";
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
    upgrade_last_exit_code: null,
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

// Issue #124, item 7 addendum. This field is a strictly stronger primitive than a bare
// untrusted string: the api composes the worker's value INSIDE a sentence uzi wrote
// (upgrade.go:143 `"running %s, target %s"`), so a bidi override reorders UZI'S words
// around it — the sentence can read as its own opposite while every byte uzi contributed
// is intact. Ingest strips Cf now; this covers rows stored before that.
describe("WorkerUpgradeBadge — upgrade_detail carries no format characters (#124)", () => {
  it("strips bidi/zero-width characters out of the composed sentence", () => {
    // WorkerUpgradeDetail, not the badge: the badge renders the closed-enum status, the
    // detail panel renders the composed sentence.
    const { container } = render(
      <WorkerUpgradeDetail
        worker={aWorker({ upgrade_status: "outdated", upgrade_detail: "running 0.11.0\u202E, target 0.11.7\u200B" })}
      />,
    );
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(container.textContent).toContain("running 0.11.0, target 0.11.7");
  });

  it("strips the divergent upgrade_target in the fleet panel's sentence", () => {
    // Same value `upgrade_detail` composes six lines away, which 4a739bff already strips.
    const { container } = render(
      <FleetUpgradePanel
        workers={[aWorker({ kind: "hosted", upgrade_target: "0.11\u202E.7\u200B", upgrade_status: "outdated" })]}
        cpVersion="0.11.9"
      />,
    );
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(container.textContent).toContain("v0.11.7");
  });

  it("strips it in the badge's title ATTRIBUTE too, which textContent cannot see", () => {
    // The attribute is a sink a rendered-text sweep misses by construction, and it carries
    // the same field. Asserted on the attribute directly for that reason — the assertion
    // above would pass with this one broken.
    const { container } = render(
      <WorkerUpgradeBadge worker={aWorker({ upgrade_detail: "running 0.11.0\u202E, target 0.11.7\u200B" })} />,
    );
    const title = container.querySelector("span[title]")?.getAttribute("title") ?? "";
    expect(title).not.toMatch(/[\p{Cf}]/u);
    expect(title).toBe("running 0.11.0, target 0.11.7");
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
    // Scope to the divergence PARAGRAPH rather than reaching for getAllByText. The panel
    // heading also says "v0.11.7", so a page-wide query has to either tolerate multiple
    // matches or count them — and tolerating multiples throws away the "Found multiple
    // elements" error, which is precisely the signal that caught this test's first
    // version matching the heading instead of the divergence line.
    const line = screen.getByText(/not the control plane/).closest("p");
    expect(line).toBeTruthy();
    expect(within(line as HTMLElement).getByText("v0.11.0")).toBeTruthy();
    expect(within(line as HTMLElement).getByText("v0.11.7")).toBeTruthy();
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

describe("the detail strip", () => {
  it("renders for the WHOLE attention set, not just failures", () => {
    // `outdated` is an alert state that `needsAttention` counts and the nav badge will
    // count. Gating this strip on upgrade_failed left its entire explanation in a hover
    // `title`: no keyboard path, no touch path, inconsistent for screen readers.
    render(<WorkerUpgradeDetail worker={aWorker({ upgrade_status: "outdated" })} />);
    expect(screen.getByText("Outdated")).toBeTruthy();
    expect(screen.getByText("running 0.11.0, target 0.11.7")).toBeTruthy();
    // No pod to inspect for a worker that is merely behind, so no kubectl affordance.
    expect(screen.queryByRole("button", { name: /copy kubectl/i })).toBeNull();
  });

  it("renders nothing for states outside the attention set", () => {
    for (const status of ["up_to_date", "upgrading", "unknown"] as const) {
      const { container, unmount } = render(<WorkerUpgradeDetail worker={aWorker({ upgrade_status: status })} />);
      expect(container.innerHTML).toBe("");
      unmount();
    }
  });

  it("tells the reader to substitute the namespace, and does not clip the placeholder", () => {
    render(
      <WorkerUpgradeDetail
        worker={aWorker({ upgrade_status: "upgrade_failed", upgrade_blocking_reason: "CrashLoopBackOff" })}
      />,
    );
    // Without this the placeholder is a trap: pasted verbatim, kubectl answers "No
    // resources found", which reads as "the worker is gone".
    expect(screen.getByText(/Replace/)).toBeTruthy();
    const code = screen.getAllByText(/kubectl -n/)[0];
    // `-n <worker-namespace>` is the PREFIX, so a truncating class hides exactly the part
    // that must be edited.
    expect(code.className).not.toContain("truncate");
  });

  it("shows the api's detail, a likely cause, and a copyable command", () => {
    render(
      <WorkerUpgradeDetail
        worker={aWorker({
          upgrade_status: "upgrade_failed",
          upgrade_detail: "seed-nix: CrashLoopBackOff (6 restarts, last exit 2)",
          upgrade_blocking_container: "seed-nix",
          upgrade_blocking_reason: "CrashLoopBackOff",
          upgrade_last_exit_code: null,
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
    // Silence is the correct output. `upgrade_detail` already states the container, the
    // reason, the restart count and the exit code, so an ungrounded sentence adds nothing
    // and reads as a diagnosis.
    expect(likelyCause("worker", "SomeReasonWeDoNotModel")).toBeNull();
    expect(likelyCause(null, null)).toBeNull();
  });

  it("does NOT claim a missing Secret for CreateContainerConfigError", () => {
    // REFUTED and removed: that reason comes from kubelet's ENV resolution, and the
    // worker pod has no env sourced from a Secret or ConfigMap at all — the renderer has
    // zero ValueFrom/SecretKeyRef/EnvFrom, enforced by a test, because a secretKeyRef
    // token would reopen a /proc/<pid>/environ leak. The token is a VOLUME, and a missing
    // Secret behind a volume surfaces as FailedMount with the container still
    // ContainerCreating. So the old copy sent an operator hunting a token Secret for a
    // failure these pods cannot produce — with authority, mid-incident.
    expect(likelyCause("worker", "CreateContainerConfigError")).toBeNull();
  });

  it("names BOTH causes of a seed-nix crash-loop and uses the exit code to separate them", () => {
    const withCode = likelyCause("seed-nix", "CrashLoopBackOff", 2);
    expect(withCode).toMatch(/permissions error/);
    // The generalisation this replaced was false: under `set -eu -o pipefail` the tar
    // pipeline is the only failing step, so ENOSPC on /nix yields the IDENTICAL
    // (seed-nix, CrashLoopBackOff) pair, and the store is ~1.7 GB.
    expect(withCode).toMatch(/out of space/);
    expect(withCode).toMatch(/exit code was 2/);
    // Without a code it still names both, and claims no code it does not have.
    const noCode = likelyCause("seed-nix", "CrashLoopBackOff", null);
    expect(noCode).toMatch(/out of space/);
    expect(noCode).not.toMatch(/exit code/);
  });

  it("survives the reason FLICKERING between CrashLoopBackOff and Error (#146)", () => {
    // MEASURED on 0.11.8: a fast-failing seed-nix alternates between waiting:CrashLoopBackOff
    // and terminated:Error as kubelet cycles it, 71% / 29% in steady state. The badge stayed
    // `upgrade_failed` throughout; only this guidance dropped out, so an operator saw the
    // explanation for an unchanged failure appear and vanish on ~3 ticks in 10.
    //
    // Asserted as an INVARIANT ACROSS BOTH REASONS in one test, not as two independent
    // cases. The bug was never "Error returns null" on its own — it was that two
    // observations of the SAME incident disagreed, and only a comparison can see that.
    const crash = likelyCause("seed-nix", "CrashLoopBackOff", 2);
    const err = likelyCause("seed-nix", "Error", 2);
    expect(err).not.toBeNull();
    expect(err).toEqual(crash);

    // CONTROL 1, aimed at the specific way this could pass vacuously: a gate widened to
    // ignore the reason entirely would return the seed-nix copy for ANY reason on that
    // container, which is a confident wrong diagnosis rather than a missing one.
    expect(likelyCause("seed-nix", "FailedMount", 2)).toBeNull();
    expect(likelyCause("seed-nix", "PodInitializing", 2)).toBeNull();

    // CONTROL 2: `Error` is still scoped to seed-nix. Widening the REASON must not widen
    // the CONTAINER — the /nix copy is about the store reseed and says nothing true about
    // the agent container exiting non-zero.
    expect(likelyCause("worker", "Error", 2)).toBeNull();

    // CONTROL 3: an explicit exit 0 with reason `Error` is self-contradictory (k8s sets
    // `Error` for a non-zero termination), so it gets silence rather than a guess.
    expect(likelyCause("seed-nix", "Error", 0)).toBeNull();

    // ...but an ABSENT code is not a contradiction, and this is the asymmetry that keeps
    // the fix from being a regression: CrashLoopBackOff has never required a code, and the
    // test above pins that it still does not.
    expect(likelyCause("seed-nix", "Error", null)).toMatch(/out of space/);
    expect(likelyCause("seed-nix", "Error", null)).not.toMatch(/exit code/);
  });

  it("does not present the image-pull causes as an exhaustive pair", () => {
    // Worker images come from Harbor, so unreachable and rate-limited are as live as a
    // bad tag or a missing pull secret.
    expect(likelyCause("worker", "ImagePullBackOff")).toMatch(/unreachable or rate-limiting/);
  });

  it("says nothing for a generic crash-loop rather than restating the reason code", () => {
    // "The container starts and exits repeatedly" is the reason code in words, not a
    // cause — and `upgrade_detail` already says it with the numbers.
    expect(likelyCause("worker", "CrashLoopBackOff")).toBeNull();
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

// BLK-1 — every colour class this component emits must EXIST in tailwind.config.js.
//
// jsdom cannot catch a phantom token: `bg-accent` is a present, correct-looking string
// whether or not the JIT emitted a rule for it. This test reads the config and checks the
// tokens, which is the cheapest thing that can fail — the browser pass found the real
// consequence (the `upgrading` bar segment laid out at full width painting NOTHING, a
// visible hole between segments while the legend counted it).
describe("colour tokens exist in the Tailwind config (BLK-1)", () => {
  it("emits no class naming a token the config does not define", () => {
    // Only the code, not the comments — the header deliberately NAMES the dead tokens so
    // nobody reintroduces them, and matching those would make this test unfixable.
    const config = tailwindConfigSource;
    const code = componentSource.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
    const used = new Set<string>();
    for (const m of code.matchAll(/\b(?:bg|text|border)-([a-z][a-z-]*)(?:\/\d+)?\b/g)) {
      used.add(m[1]);
    }
    // Tailwind's own always-available colour names, plus the non-colour utilities that
    // share the `text-` prefix (font sizes and alignment). Excluding those by name rather
    // than by pattern keeps the check tight: a genuinely missing colour token cannot hide
    // behind a loose exclusion.
    const builtin = new Set([
      "white", "black", "transparent", "current", "inherit",
      "xs", "sm", "lg", "xl", "2xl", "3xl", "left", "right", "center", "justify",
    ]);
    const missing = [...used].filter((t) => !builtin.has(t) && !new RegExp(`["']?${t}["']?\\s*:`).test(config));
    expect(missing).toEqual([]);
  });
});

// BLK-2 — a mixed fleet must never be described categorically.
describe("the divergence line on a MIXED fleet (BLK-2)", () => {
  const hosted = (id: string, target: string) =>
    aWorker({ id, kind: "hosted", upgrade_target: target, upgrade_status: "up_to_date" });

  it("names the target when every divergent worker shares one", () => {
    render(<FleetUpgradePanel workers={[hosted("a", "0.11.0"), hosted("b", "0.11.0")]} cpVersion="0.11.7" />);
    const line = screen.getByText(/not the control plane/).closest("p") as HTMLElement;
    expect(within(line).getByText("v0.11.0")).toBeTruthy();
    expect(line.textContent).toContain("2 hosted workers target");
  });

  it("states a COUNT instead of a target when the fleet is partially pinned", () => {
    // The demo's own state, and the one the old code lied about: it kept the LAST divergent
    // target in list order and asserted "hosted workers target v0.11.0" on a screen that
    // also showed a hosted worker up to date against 0.11.7.
    render(
      <FleetUpgradePanel
        workers={[hosted("a", "0.11.0"), hosted("b", "0.11.5"), hosted("c", "0.11.7")]}
        cpVersion="0.11.7"
      />,
    );
    const line = screen.getByText(/pinned tag below/).closest("p") as HTMLElement;
    expect(line.textContent).toContain("2 hosted workers");
    // No single target may be named, because no single target is true of all of them.
    expect(within(line).queryByText("v0.11.0")).toBeNull();
    expect(within(line).queryByText("v0.11.5")).toBeNull();
  });

  it("says nothing while the control-plane version is still in flight", () => {
    // Previously the panel rendered a full bar and badges under "classification off",
    // because the version fetch loses the race with the workers load by ~400ms.
    render(<FleetUpgradePanel workers={[hosted("a", "0.11.0")]} cpVersion={null} />);
    expect(screen.queryByText(/classification off/)).toBeNull();
    expect(screen.queryByText(/target release/)).toBeNull();
  });
});

describe("copy feedback", () => {
  it("confirms the copy rather than leaving the reader guessing", async () => {
    render(<WorkerUpgradeDetail worker={aWorker({ upgrade_status: "upgrade_failed" })} />);
    const btn = screen.getByRole("button", { name: /copy kubectl/i });
    fireEvent.click(btn);
    expect(await screen.findByRole("button", { name: /copied/i })).toBeTruthy();
  });
});
