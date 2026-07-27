import { useState } from "react";
import type { Worker } from "../lib/api";
import { stripUnsafeChars } from "../lib/safeText";

/**
 * Per-worker upgrade badge and the Fleet upgrade summary (PRD #113 M5).
 *
 * ⚠ ONLY USE COLOUR TOKENS THAT EXIST IN tailwind.config.js. This file shipped with
 * `accent`, `hair` and `base`, none of which are defined — the JIT scanned them and emitted
 * NOTHING, so the classes were inert in every theme (the defect is the config, not the CSS
 * variables, so no theme switch reveals it). Measured in a browser: the `upgrading` bar
 * segment laid out at its full percentage width with a transparent background, painting a
 * visible HOLE between the green and amber segments while the legend below counted it. The
 * real tokens are `info`, `edge`, `ink`. jsdom cannot catch this class of bug — a class name
 * is present and correct as a string either way.
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
  info: "border-info/40 bg-info/10 text-info",
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
      //
      // Issue #124: stripped HERE TOO, not only where the same field renders as text 175
      // lines down. An ATTRIBUTE is a sink a per-render-site sweep misses by construction —
      // `container.textContent` cannot see it, so no test asserting over rendered text can
      // either. A tooltip is drawn by the browser and honours bidi exactly like body text.
      title={worker.upgrade_detail ? stripUnsafeChars(worker.upgrade_detail) : undefined}
    >
      {p.label}
    </span>
  );
}

/**
 * A closed lookup from (container, reason, exit code) to a human cause.
 *
 * It lives in the WEB, and that placement is the point: it reads as product copy rather
 * than as something we measured on the cluster. The api forwards only the container name
 * and the k8s waiting REASON — never `message`, which is free text carrying paths and
 * Secret names.
 *
 * FEWER ENTRIES THAT ARE TRUE, rather than more that are plausible. An entry naming a
 * mechanism these pods cannot exhibit is worse than no entry: it arrives with authority
 * and sends an operator into the wrong subsystem during an incident. Two entries were
 * removed for exactly that reason — see the notes below — and `null` is a correct answer,
 * because `upgrade_detail` already states the container, the reason, the restart count and
 * the exit code.
 */
export function likelyCause(container: string | null, reason: string | null, lastExitCode?: number | null): string | null {
  if (reason === "ImagePullBackOff" || reason === "ErrImagePull") {
    // NOT an exhaustive two-item list, which is what this said before. Worker images come
    // from Harbor, so an unreachable registry and a pull rate-limit are as live as a bad
    // tag or a missing pull secret.
    return "The image could not be pulled: the tag may not exist, the pull secret may be missing, or the registry may be unreachable or rate-limiting.";
  }
  // TWO REASONS, ONE FAILURE (issue #146). A fast-failing seed-nix ALTERNATES between
  // `waiting: CrashLoopBackOff` and `terminated: Error` as kubelet cycles it — MEASURED
  // steady-state split 71% / 29% on 0.11.8. Gating on CrashLoopBackOff alone made this
  // guidance appear and vanish on roughly three ticks in ten while the badge correctly
  // stayed `upgrade_failed`, so an operator watched the explanation for an unchanged
  // failure flicker out. `Error` is the SAME incident observed a moment later, not a
  // different one.
  //
  // The exit-code requirement applies to the `Error` arm ONLY, and the asymmetry is
  // deliberate rather than an oversight. `CrashLoopBackOff` on an INIT container is
  // already self-evidently a failure (a successful init container exits 0 and is never
  // restarted), and requiring a code there would REMOVE guidance that ships today for a
  // row whose exit code was never recorded — the test below pins that case. `Error` is the
  // weaker signal, so it is required not to contradict itself: an explicit exit 0 with
  // reason `Error` is incoherent and gets silence. An ABSENT code is not a contradiction
  // and still qualifies, which is what `!== 0` says about null and undefined.
  //
  // NOT added to the controller's blockingReasons. `Error` is not a state k8s refuses to
  // leave — that set's entry criterion — and it does not need to be: on every `Error` tick
  // the restart arm already holds the `stuck` verdict. See stuckRestartThreshold's comment,
  // which measures exactly this.
  const seedNixFailing =
    container === "seed-nix" && (reason === "CrashLoopBackOff" || (reason === "Error" && lastExitCode !== 0));
  if (seedNixFailing) {
    // Faithful to the v0.11.0 incident WITHOUT generalising from it. The earlier version
    // asserted a permissions error as the cause, which is false as a rule: under
    // `set -eu -o pipefail` the tar pipeline is the only failing step, so ENOSPC on the
    // /nix PVC produces the IDENTICAL (seed-nix, CrashLoopBackOff) pair — and the store is
    // ~1.7 GB, so that is not a remote possibility.
    //
    // AND THE EXIT CODE DOES NOT SEPARATE THEM, which is what this comment claimed next.
    // Measured in the worker base image (node:22-alpine@sha256:16e22a55…, `apk add tar` →
    // GNU tar 1.35, extracting as uid 10001): permission denied gives "Cannot open:
    // Permission denied", ENOSPC gives "Wrote only 8704 of 10240 bytes" (e.g. — the byte
    // counts vary per run, only the shape is stable), and BOTH exit 2 —
    // GNU tar uses 2 for every fatal error. (Busybox tar, if GNU tar were ever absent,
    // exits 1 for both; the conclusion does not depend on which one runs.) What separates
    // them is tar's stderr LINE, which is exactly what this architecture refuses to carry:
    // pods/log is not offered and k8s `message` is never forwarded.
    //
    // So the string below names both causes and reports the code as a fact to quote into a
    // bug report — it must not be reworded into a claim that the code chooses between them.
    const code = typeof lastExitCode === "number" ? ` Its last exit code was ${lastExitCode}.` : "";
    return `The nix store reseed is failing. Two causes produce this exact signature: a permissions error unpacking the store (the shape of the v0.11.0 incident), or the /nix volume running out of space.${code}`;
  }
  // DELIBERATELY ABSENT — do not add back without grounding them:
  //
  //   * CreateContainerConfigError, previously "a referenced Secret or ConfigMap is
  //     missing, most often the join token". REFUTED: this reason comes from kubelet's
  //     ENV resolution, and the worker pod has no env sourced from a Secret or ConfigMap
  //     at all — verified zero ValueFrom/SecretKeyRef/EnvFrom in the renderer, which a
  //     test actively enforces because a secretKeyRef token would reopen a
  //     /proc/<pid>/environ leak. The token is a VOLUME, and a missing Secret behind a
  //     volume surfaces as FailedMount with the container still ContainerCreating — which
  //     the controller classifies on its reasonless arm, not here. So the most specific
  //     and most actionable line in this table pointed at a failure mode these pods
  //     cannot produce.
  //
  //   * a generic CrashLoopBackOff line. It restated the reason code ("the container
  //     starts and exits repeatedly") without naming a cause, which is what "likely
  //     cause" promises. `upgrade_detail` already says that, with the numbers.
  //
  //   * CreateContainerError and InvalidImageName are in the controller's blocking set
  //     with no copy here. That is the honest state: nobody has grounded a cause for them
  //     on these pods.
  return null;
}

/**
 * The kubectl command an operator can copy. Read-only.
 *
 * `describe pod` rather than `logs`, and the reason is about the DATA, not about RBAC.
 * This command runs under the operator's own kubeconfig, so the controller's Role does not
 * constrain it — an earlier version of this comment cited "pods/log is refused for the
 * controller", which is a true fact about the wrong subject. The actual reason to prefer
 * describe: worker logs carry agent output over a user's cloned private repo, so putting a
 * log command in the product UI invites reading it, and pod metadata answers the question
 * ("why is this pod not Ready?") without that exposure.
 *
 * The label selector is the one the controller actually stamps on the pod template, so
 * this resolves the same pods roll health was derived from.
 *
 * The namespace is NOT guessed. A docker-tier worker lives in a different namespace, and a
 * command naming the wrong one fails in a way that looks like the worker is gone — so the
 * placeholder is explicit and the copy says to fill it in.
 *
 * NO PLACEHOLDER CAN ANNOUNCE ITSELF, so do not go looking for a better one. kubectl does
 * not validate a namespace argument against RFC 1123 before querying: measured against a
 * real cluster, `-n '<worker-namespace>'` exits 0 with "No resources found", so every
 * candidate spelling — `<worker-namespace>`, `NAMESPACE`, `__FILL_ME_IN__` — reaches the
 * api server and comes back empty and cheerful. The only reason the current form fails
 * loudly is an accident of shell syntax (`<`/`>` are redirections, so a paste never
 * execs), which means the SUBSTITUTION REMINDER lives in the copy below and nowhere else.
 * Changing the placeholder without changing that copy silently removes the only warning.
 *
 * The way out is to stop needing a placeholder: the controller knows each worker's
 * namespace (it groups pods by it in observeNamespace) and could carry it on the wire.
 * Deliberately not done here — it reopens the M3/M4 protocol contract, both goldens and
 * the audit surface for a display string. See the MR description.
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
  // Copy feedback. Without it the button gives NO signal at all, so a reader cannot tell a
  // successful copy from a no-op clipboard — and this command is the one thing they are
  // meant to carry to a terminal.
  const [copied, setCopied] = useState(false);
  if (!needsAttention(worker)) return null;
  const failed = worker.upgrade_status === "upgrade_failed";
  const cause = failed
    ? likelyCause(worker.upgrade_blocking_container ?? null, worker.upgrade_blocking_reason ?? null, worker.upgrade_last_exit_code)
    : null;
  const tone = failed ? "border-danger/30 bg-danger/5" : "border-warn/30 bg-warn/5";
  return (
    <div className={`mt-2 rounded border p-2 text-xs ${tone}`}>
      <div className={`font-medium ${failed ? "text-danger" : "text-warn"}`}>
        {failed ? "Upgrade failed" : "Outdated"}
      </div>
      {/* The api's own sentence, as TEXT — the same string the badge carries as a title,
          rendered where a keyboard and a screen reader can both reach it. */}
      {/* Issue #124, and this shape is strictly worse than a bare field: the api composes
          the untrusted span INSIDE a sentence uzi wrote (upgrade.go:143 "running %s, target
          %s"; :462 "%s: %s" from blocking_container/blocking_reason). A bidi override there
          does not merely garble the attacker's own value — it reorders uzi's words around
          it, so the sentence can read as its own opposite while every byte uzi contributed
          is intact. Ingest strips Cf now; this covers rows written before that. */}
      {worker.upgrade_detail && (
        <div className="mt-1 text-fg">{stripUnsafeChars(worker.upgrade_detail)}</div>
      )}
      {cause && <div className="mt-1 text-faint">{cause}</div>}
      {failed && (
        <div className="mt-2">
          <div className="flex flex-wrap items-center gap-2">
            <button
              type="button"
              className="rounded border border-edge px-1.5 py-0.5 text-faint hover:text-fg"
              onClick={() => {
                void navigator.clipboard?.writeText(diagnosticsCommand(worker.id));
                setCopied(true);
                window.setTimeout(() => setCopied(false), 2000);
              }}
              aria-live="polite"
            >
              {copied ? "Copied" : "Copy kubectl command"}
            </button>
            {/* NOT truncated, and the reason is the opposite of what this comment first
                claimed. `text-overflow: ellipsis` elides the TAIL, so the placeholder
                survives and the WORKER ID is what disappears — measured at 420px. That is
                worse than losing the prefix: a reader who cannot see the id has no way to
                tell which worker the command is even for, and the visible part looks
                complete. Wrapping keeps both halves. */}
            <code className="break-all text-faint">{diagnosticsCommand(worker.id)}</code>
          </div>
          {/* The substitution instruction. Without it the placeholder is a trap — though not
              the trap this comment first described. Pasted into a shell the command never
              reaches kubectl at all: `<` and `>` are REDIRECTION OPERATORS, so bash and zsh
              both abort with "worker-namespace: No such file or directory" (exit 1, measured
              in both; no stray output file is created either, since the failed input
              redirection aborts before the output one). Loud and confusing, but it does say
              "you left something unsubstituted".

              The dangerous case is the substituted-but-WRONG namespace: kubectl accepts it,
              exits 0, and answers "No resources found", which reads as "the worker is gone".
              MEASURED on dev-cluster: the hosted worker from the motivating incident lives
              in the DOCKER namespace, not the default one, so guessing is not an option.

              DO NOT "fix" this by quoting the placeholder in diagnosticsCommand. kubectl
              does NOT reject `<worker-namespace>` for violating RFC 1123 — it exits 0 with
              "No resources found", so quoting converts the loud failure into the silent one
              this very paragraph warns about. */}
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
   * Hosted workers whose target differs from the control plane's version. Non-empty is NOT
   * an error — see the panel copy.
   *
   * A COUNT plus the distinct targets, not "the divergent target", because a partial pin is
   * a real fleet state: keeping only the last one in list order and stating it categorically
   * said "hosted workers target v0.4.1" on a screen that also showed a hosted worker badged
   * up to date against v0.4.2. The suppression scenario this line exists for has UNIFORM
   * targets, so it still names them; a mixed fleet gets a count instead of a false claim.
   */
  divergentCount: number;
  divergentTargets: string[];
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
  let divergentCount = 0;
  const divergentTargets = new Set<string>();
  for (const w of workers) {
    counts[w.upgrade_status] = (counts[w.upgrade_status] ?? 0) + 1;
    if (needsAttention(w)) attention++;
    // A hosted worker whose target is not the control plane's version means the worker
    // image is pinned — deliberately by an operator, or by a controller suppressing
    // alerts. The api cannot distinguish those (Decision 9 requires honouring the tag),
    // so the divergence is stated rather than judged.
    if (w.kind === "hosted" && w.upgrade_target && cpVersion && w.upgrade_target !== cpVersion) {
      divergentCount++;
      // Issue #124. Weaker provenance than the rest of the batch — `classifyWithTarget`
      // sets this from uzi's own CPVersion or the CONTROLLER's reported tag, not from a
      // worker — so this is for the ASYMMETRY as much as the tier: `upgrade_detail`
      // composes the same value into its own sentence six lines away and IS stripped.
      // Same value, two treatments, is what makes the next reader guess.
      divergentTargets.add(stripUnsafeChars(w.upgrade_target));
    }
  }
  return { counts, attention, divergentCount, divergentTargets: [...divergentTargets] };
}

export function FleetUpgradePanel({
  workers,
  cpVersion,
}: {
  workers: Worker[];
  // null while GET /api/version is still in flight; "" once it resolved with no stamp. The
  // DISTINCTION matters: they were conflated, and because the version fetch loses the race
  // with the workers load by ~400ms, the panel rendered a full bar and five badges under a
  // heading that said "no release stamp — classification off". Measured at T+270ms, flipping
  // at T+670ms. Self-contradictory copy on every first paint, and structural rather than a
  // demo artifact.
  cpVersion: string | null;
}) {
  const { counts, attention, divergentCount, divergentTargets } = fleetSummary(workers, cpVersion ?? "");
  const versionPending = cpVersion === null;
  const classified = workers.length - counts.unknown;
  if (workers.length === 0) return null;

  const segments: { key: Worker["upgrade_status"]; cls: string }[] = [
    { key: "up_to_date", cls: "bg-ok" },
    { key: "upgrading", cls: "bg-info" },
    { key: "outdated", cls: "bg-warn" },
    { key: "upgrade_failed", cls: "bg-danger" },
  ];

  return (
    <section className="mb-4 rounded border border-edge bg-raised p-3" aria-labelledby="fleet-upgrade-heading">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 id="fleet-upgrade-heading" className="text-sm font-medium text-fg">
          Fleet upgrade
        </h3>
        <span className="text-xs text-faint">
          {versionPending ? (
            // Say nothing rather than assert either state while the answer is in flight.
            <>&nbsp;</>
          ) : cpVersion ? (
            <>target release v{cpVersion}</>
          ) : (
            <>no release stamp — classification off</>
          )}
        </span>
      </div>

      {/* B-1: state the divergence. Neutral, because pinning workers.image.tag below the
          api's release is a supported operation — and because the same shape is how a
          compromised controller would suppress every alert in the fleet by reporting the
          fleet's own stale version as the target. A reader who pinned deliberately sees
          their pin confirmed; a reader who did not sees something to investigate. */}
      {divergentCount > 0 && (
        <p className="mt-2 rounded border border-edge bg-ink p-2 text-xs text-faint">
          {divergentTargets.length === 1 ? (
            <>
              {divergentCount === 1 ? "One hosted worker targets" : `${divergentCount} hosted workers target`}{" "}
              <span className="text-fg">v{divergentTargets[0]}</span>, not the control plane&rsquo;s{" "}
              <span className="text-fg">v{cpVersion}</span>.
            </>
          ) : (
            <>
              {divergentCount} hosted workers target a pinned tag below the control plane&rsquo;s{" "}
              <span className="text-fg">v{cpVersion}</span>.
            </>
          )}{" "}
          A hosted worker is compared against the tag the controller reports rolling to, so a pinned image reads as up
          to date at that tag. Each row shows its own target.
        </p>
      )}

      {classified > 0 && (
        <div className="mt-2 flex h-1.5 overflow-hidden rounded bg-ink" role="presentation">
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
