import type { Worker } from "../lib/api";

/**
 * Per-worker upgrade badge and the Fleet upgrade summary (PRD #113 M5).
 *
 * Everything here is derived from the workers list the page already fetches. There is
 * deliberately NO second read path: the roll-health table carries no `user_id`, so it is
 * reachable only by joining through `workers`, and that join is what makes tenancy
 * unavoidable rather than remembered. A fleet-summary endpoint that queried the report
 * table directly would hand every user a count of other users' failing workers.
 */

/** The attention set (Decision 1): failed or behind. `upgrading` is expected and transient. */
export function needsAttention(w: Worker): boolean {
  return w.upgrade_status === "upgrade_failed" || w.upgrade_status === "outdated";
}

// The mute is NOT rendered here. Its storage and its release key landed with the api
// (including the fix that made the key correct for external workers, who are the
// population it exists for), but the UI action is not in this milestone's scope, and a
// badge branch for a state nothing can set would be dead code asserting a feature.
type Tone = "alert" | "warn" | "info" | "ok";

const PRESENTATION: Record<Worker["upgrade_status"], { label: string; tone: Tone } | null> = {
  // `unknown` renders NOTHING. An unstamped local image, an unparseable report and a
  // `dev` control plane all land here, and none of them is a finding — a badge on every
  // worker of every local stack is how a reader learns to stop looking at badges.
  unknown: null,
  up_to_date: { label: "up to date", tone: "ok" },
  outdated: { label: "outdated", tone: "warn" },
  upgrading: { label: "upgrading", tone: "info" },
  upgrade_failed: { label: "upgrade failed", tone: "alert" },
};

const TONE_CLASS: Record<Tone, string> = {
  alert: "border-danger/40 bg-danger/10 text-danger",
  warn: "border-warn/40 bg-warn/10 text-warn",
  info: "border-accent/40 bg-accent/10 text-accent",
  ok: "border-ok/40 bg-ok/10 text-ok",
};

export function WorkerUpgradeBadge({ worker }: { worker: Worker }) {
  const p = PRESENTATION[worker.upgrade_status];
  if (!p) return null;
  return (
    <span
      className={`inline-flex items-center gap-1 rounded border px-1.5 py-0.5 text-xs ${TONE_CLASS[p.tone]}`}
      // The detail is the sentence the api derived ("running 0.11.0, target 0.11.7"), so
      // the badge and its explanation cannot disagree — the api computed both.
      title={worker.upgrade_detail ?? undefined}
    >
      {p.label}
    </span>
  );
}

/**
 * A closed lookup from (container, reason) to a human cause.
 *
 * It lives in the WEB, and that placement is the point: it reads as product copy rather
 * than as something we measured on the cluster. The api forwards only the container name
 * and the k8s waiting REASON — never `message`, which is free text carrying paths and
 * Secret names — so a guess dressed up as a diagnosis would be a claim we cannot support.
 */
export function likelyCause(container: string | null, reason: string | null): string | null {
  if (reason === "ImagePullBackOff" || reason === "ErrImagePull") {
    return "The image tag may not exist in the registry, or the pull secret is missing.";
  }
  if (reason === "CreateContainerConfigError") {
    return "A referenced Secret or ConfigMap is missing — most often the worker's join token.";
  }
  if (reason === "CrashLoopBackOff" && container === "seed-nix") {
    return "The nix store reseed is failing. This is the shape of the v0.11.0 incident: a permissions error unpacking the browser closure.";
  }
  if (reason === "CrashLoopBackOff") {
    return "The container starts and exits repeatedly. Its last exit code is the best next clue.";
  }
  return null;
}

/**
 * The kubectl command an operator can copy. Read-only.
 *
 * `describe pod` rather than `logs`: worker logs carry agent output over a user's cloned
 * private repo, so `pods/log` is refused for the controller and offering it here would
 * imply access uzi does not grant itself. The label selector is the one the controller
 * actually stamps on the pod template, so this resolves the same pods roll health was
 * derived from.
 *
 * The namespace is NOT guessed. A docker-tier worker lives in a different namespace, and a
 * command naming the wrong one fails in a way that looks like the worker is gone — so the
 * placeholder is explicit and the copy says to fill it in.
 */
export function diagnosticsCommand(workerId: string): string {
  return `kubectl -n <worker-namespace> describe pod -l uzi.dev/hosted-worker-id=${workerId}`;
}

/**
 * The detail strip for a worker in the ATTENTION SET — failed OR outdated.
 *
 * It covers both, and that is a correctness requirement rather than symmetry. The badge's
 * `title` is a hover tooltip: no keyboard path, no touch path, and inconsistent
 * screen-reader behaviour. So gating this strip on `upgrade_failed` alone left the OTHER
 * alert state — `outdated`, which `needsAttention` counts and the nav badge will count —
 * with its entire explanation reachable only by a mouse.
 *
 * Worth stating because a test cannot catch it: asserting the title's exact VALUE is a
 * stronger check than asserting its presence, and it is still satisfiable while the
 * information reaches nobody. jsdom can verify an attribute is correct; it cannot verify
 * anyone can reach it.
 *
 * The failure-specific parts (the likely cause, the kubectl affordance) stay gated on
 * `upgrade_failed` — there is no pod to inspect for a worker that is merely behind.
 */
export function WorkerUpgradeDetail({ worker }: { worker: Worker }) {
  if (!needsAttention(worker)) return null;
  const failed = worker.upgrade_status === "upgrade_failed";
  const cause = failed ? likelyCause(worker.upgrade_blocking_container ?? null, worker.upgrade_blocking_reason ?? null) : null;
  const tone = failed ? "border-danger/30 bg-danger/5" : "border-warn/30 bg-warn/5";
  return (
    <div className={`mt-2 rounded border p-2 text-xs ${tone}`}>
      <div className={`font-medium ${failed ? "text-danger" : "text-warn"}`}>
        {failed ? "Upgrade failed" : "Outdated"}
      </div>
      {/* The api's own sentence, as TEXT — the same string the badge carries as a title,
          rendered where a keyboard and a screen reader can both reach it. */}
      {worker.upgrade_detail && <div className="mt-1 text-fg">{worker.upgrade_detail}</div>}
      {cause && <div className="mt-1 text-faint">{cause}</div>}
      {failed && (
        <div className="mt-2">
          <div className="flex flex-wrap items-center gap-2">
            <button
              type="button"
              className="rounded border border-hair px-1.5 py-0.5 text-faint hover:text-fg"
              onClick={() => void navigator.clipboard?.writeText(diagnosticsCommand(worker.id))}
            >
              Copy kubectl command
            </button>
            {/* NOT truncated. `-n <worker-namespace>` is the PREFIX, so clipping would
                hide exactly the part the reader has to replace — handing them a command
                that looks complete and resolves the wrong namespace, or none. Wrapping is
                the lesser evil for a string meant to be read and edited. */}
            <code className="break-all text-faint">{diagnosticsCommand(worker.id)}</code>
          </div>
          {/* The substitution instruction. Without it the placeholder is a trap: a user
              pastes the command verbatim and kubectl answers "No resources found", which
              reads as "the worker is gone" rather than "fill in the namespace". MEASURED
              on dev-cluster: the hosted worker from the motivating incident lives in the
              DOCKER namespace, not the default one, so guessing is not an option either. */}
          <p className="mt-1 text-faint">
            Replace <code>&lt;worker-namespace&gt;</code> with the namespace this worker runs in — the docker-capable
            tier uses a separate one.
          </p>
        </div>
      )}
    </div>
  );
}

export type FleetSummary = {
  counts: Record<Worker["upgrade_status"], number>;
  attention: number;
  /**
   * The coordinate hosted workers are being rolled to, when it differs from the control
   * plane's own version. Non-null is NOT an error — see the panel copy.
   */
  divergentTarget: string | null;
};

/**
 * Fold the workers list into the panel's numbers.
 *
 * `cpVersion` is the control plane's own release, already fetched for the footer.
 */
export function fleetSummary(workers: Worker[], cpVersion: string): FleetSummary {
  const counts: Record<Worker["upgrade_status"], number> = {
    up_to_date: 0,
    outdated: 0,
    upgrading: 0,
    upgrade_failed: 0,
    unknown: 0,
  };
  let attention = 0;
  let divergentTarget: string | null = null;
  for (const w of workers) {
    counts[w.upgrade_status] = (counts[w.upgrade_status] ?? 0) + 1;
    if (needsAttention(w)) attention++;
    // A hosted worker whose target is not the control plane's version means the worker
    // image is pinned — deliberately by an operator, or by a controller suppressing
    // alerts. The api cannot distinguish those (Decision 9 requires honouring the tag),
    // so the divergence is stated rather than judged.
    if (w.kind === "hosted" && w.upgrade_target && cpVersion && w.upgrade_target !== cpVersion) {
      divergentTarget = w.upgrade_target;
    }
  }
  return { counts, attention, divergentTarget };
}

export function FleetUpgradePanel({ workers, cpVersion }: { workers: Worker[]; cpVersion: string }) {
  const { counts, attention, divergentTarget } = fleetSummary(workers, cpVersion);
  const classified = workers.length - counts.unknown;
  if (workers.length === 0) return null;

  const segments: { key: Worker["upgrade_status"]; cls: string }[] = [
    { key: "up_to_date", cls: "bg-ok" },
    { key: "upgrading", cls: "bg-accent" },
    { key: "outdated", cls: "bg-warn" },
    { key: "upgrade_failed", cls: "bg-danger" },
  ];

  return (
    <section className="mb-4 rounded border border-hair bg-raised p-3" aria-labelledby="fleet-upgrade-heading">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 id="fleet-upgrade-heading" className="text-sm font-medium text-fg">
          Fleet upgrade
        </h3>
        <span className="text-xs text-faint">
          {cpVersion ? <>target release v{cpVersion}</> : <>no release stamp — classification off</>}
        </span>
      </div>

      {/* B-1: state the divergence. Neutral, because pinning workers.image.tag below the
          api's release is a supported operation — and because the same shape is how a
          compromised controller would suppress every alert in the fleet by reporting the
          fleet's own stale version as the target. A reader who pinned deliberately sees
          their pin confirmed; a reader who did not sees something to investigate. */}
      {divergentTarget && (
        <p className="mt-2 rounded border border-hair bg-base p-2 text-xs text-faint">
          Hosted workers target <span className="text-fg">v{divergentTarget}</span>, not the control plane&rsquo;s{" "}
          <span className="text-fg">v{cpVersion}</span>. Hosted workers are compared against the tag the controller
          reports rolling to, so a pinned worker image reads as up to date at that tag.
        </p>
      )}

      {classified > 0 && (
        <div className="mt-2 flex h-1.5 overflow-hidden rounded bg-base" role="presentation">
          {segments.map(({ key, cls }) =>
            counts[key] > 0 ? (
              <div key={key} className={cls} style={{ width: `${(counts[key] / classified) * 100}%` }} />
            ) : null,
          )}
        </div>
      )}

      <div className="mt-2 flex flex-wrap gap-x-3 gap-y-1 text-xs text-faint">
        {segments.map(({ key }) =>
          counts[key] > 0 ? (
            <span key={key}>
              {counts[key]} {PRESENTATION[key]?.label}
            </span>
          ) : null,
        )}
        {counts.unknown > 0 && <span>{counts.unknown} not reporting a version</span>}
      </div>

      {attention > 0 && (
        <p className="mt-2 text-xs text-warn">
          {attention} {attention === 1 ? "worker needs" : "workers need"} attention.
        </p>
      )}
    </section>
  );
}
