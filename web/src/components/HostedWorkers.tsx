// Settings → Workers: provision a HOSTED worker (PRD #58 M5) — one whose container
// the controller runs in the cluster, rather than one the user starts by hand.
//
// It is an inline Card form, not a modal, and deliberately: the sibling "Register a
// worker" card on this same page is the same interaction with the same shape of
// input, and this repo has no modal primitive at all. A two-field form is not the
// place to invent one (focus trap, escape, aria-modal, scroll lock); if the app wants
// modals later, that is a primitive for the whole app and its own piece of work.
//
// The whole card is hidden — never disabled-with-explanation — when hosting is off.
// A user on an instance without hosting has no use for the concept (Decision 12).

import { useEffect, useRef, useState, type FormEvent } from "react";
import { api, type HostedConfig, type Worker } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAuth } from "../auth/AuthContext";
import { Alert, Button, Card, Field, SectionTitle, Select, Toggle } from "./ui";
import { DEFAULT_WORKER_TEMPLATE, WORKER_TEMPLATES } from "../lib/workerTemplates";
import { DEFAULT_WORKER_SIZE, WORKER_SIZES, sizeOptionLabel } from "../lib/workerSizes";

export function HostedWorkers({
  hostedCount,
  onProvisioned,
  onShowWorkers,
  onAvailability,
}: {
  /** How many PERSISTENT hosted workers the user already holds, counted from the fleet
   *  list the page polls (ephemeral rows excluded, as the server's quota count excludes
   *  them). There is no count endpoint and none is wanted. */
  hostedCount: number;
  /** Hand the new worker to the page: it owns the announcement slot (a delete has to
   *  be able to replace a provision's message, and deletes are the page's) and the
   *  fleet refresh. */
  onProvisioned: (worker: Worker) => void | Promise<void>;
  /** Cross the tabs back to Your workers so the user can delete a hosted worker; the
   *  page selects that tab and focuses the first hosted row. Fired by the at-quota
   *  "delete one to provision another" link (D10). */
  onShowWorkers: () => void;
  /** Tell the page whether MANUAL hosted provisioning is available, so its empty state
   *  can lead with a hosted CTA (D8). It is fired FROM the config-fetch effect below,
   *  exactly once per mount, and BEFORE the render-time early `return null` gates: the
   *  effect runs on mount whatever those gates render, so the page learns hosting is off
   *  (or on) even while this component's own card renders nothing and even while the add
   *  panel is `hidden` — which works only because D4 keeps this component mounted. Lifting
   *  the fetch into the page was rejected (D8) to keep the one-shot fetch where its tests
   *  pin it; this callback is the one-prop alternative. `manual = enabled && quota > 0`;
   *  `{ manual: false }` when hosting is disabled, quota is 0, or the config read rejects. */
  onAvailability?: (a: { manual: boolean }) => void;
}) {
  const { user, updateUser } = useAuth();
  const [config, setConfig] = useState<HostedConfig | null>(null);
  const [template, setTemplate] = useState<string>(DEFAULT_WORKER_TEMPLATE);
  const [size, setSize] = useState<string>(DEFAULT_WORKER_SIZE);
  // Opt into a Docker-in-Docker sidecar (PRD #83 M3). Off by default: the
  // plain worker is the common case, docker is the extra one you ask for.
  const [docker, setDocker] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // Both ephemeral preference controls stay disabled until the saved user is applied.
  const [ephemeralBusy, setEphemeralBusy] = useState(false);
  // The ephemeral toggle carries its OWN error slot, rendered beside the toggle at the
  // bottom of the card, so a failed write is visible where the user acted — not in the
  // manual form's alert at the top (likely off-screen). Kept separate from `error` so
  // the two surfaces never clobber each other.
  const [ephemeralError, setEphemeralError] = useState("");
  // Read the latest onAvailability via a ref so the fetch effect can stay `[]`-deps (a
  // one-shot fetch its tests pin) without an inline callback re-running it. The latch ref
  // makes onAvailability fire exactly once per mount, even under a StrictMode double-invoke.
  const onAvailabilityRef = useRef(onAvailability);
  onAvailabilityRef.current = onAvailability;
  const availabilityReported = useRef(false);

  // Fetched once, on mount, and never polled: enabled/quota are operator-set POLICY,
  // which changes on a deploy or an admin edit, not on the 10s liveness rhythm the
  // fleet list needs. A failed read fails CLOSED — config stays null and the card
  // stays hidden, indistinguishable from hosting being off. That is the honest
  // failure: the alternative is an alert about a capability probe for a feature the
  // user may not even have, on every blip.
  useEffect(() => {
    let live = true;
    // Report availability from INSIDE this effect (D8), so it fires before the render-time
    // `return null` gates below and the page learns hosting is off even while this card
    // renders nothing. Guarded to exactly once per mount.
    const reportAvailability = (manual: boolean) => {
      if (availabilityReported.current) return;
      availabilityReported.current = true;
      onAvailabilityRef.current?.({ manual });
    };
    api
      .hostedConfig()
      .then((cfg) => {
        if (live) setConfig(cfg);
        // manual = enabled AND self-service quota left; anything else is { manual: false }.
        reportAvailability(!!cfg.enabled && cfg.quota > 0);
      })
      .catch(() => {
        // Fail closed; see above. The page still learns hosting is unavailable.
        reportAvailability(false);
      });
    return () => {
      live = false;
    };
  }, []);

  const provision = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setBusy(true);
    try {
      // No token comes back and none is rendered — unlike createWorker on this page.
      // The controller collects a hosted worker's token from its poll; the user is
      // never in that path (Decision 3). Success is: the row shows up in the list.
      const { worker } = await api.provisionHostedWorker(template, size, docker);
      setTemplate(DEFAULT_WORKER_TEMPLATE);
      setSize(DEFAULT_WORKER_SIZE);
      setDocker(false);
      // The page announces it, naming the worker the SERVER created (it derives a name
      // from template + size when the form sends none, and naming the row is how the
      // user finds it below).
      await onProvisioned(worker);
    } catch (err) {
      // The server's message is what the user reads, verbatim: it distinguishes the
      // quota being reached (409 — delete one and retry) from provisioning being
      // switched off underneath us (403 — this card should not have been shown, so
      // our config read is stale) and from the rate limiter (429).
      setError(errorMessage(err, "Failed to provision worker"));
    } finally {
      setBusy(false);
    }
  };

  // Both preferences share a pending lock. Keep rendering confirmed auth values;
  // a rejected write therefore restores the control without a speculative value.
  // Apply the saved user directly so an older session probe cannot undo the write.
  const toggleEphemeral = async (prefs: boolean | { docker: boolean }) => {
    if (ephemeralBusy) return;
    setEphemeralError("");
    setEphemeralBusy(true);
    try {
      const { user: savedUser } = await api.setEphemeralWorkersEnabled(prefs);
      updateUser(savedUser);
    } catch (err) {
      setEphemeralError(errorMessage(err, "Failed to update ephemeral workers"));
    } finally {
      setEphemeralBusy(false);
    }
  };

  // Nothing hosted renders until the config says so: not loaded, failed, or disabled
  // all look the same on purpose. Ephemeral needs hosting too, so this gate is shared.
  if (!config?.enabled) return null;

  // The manual provision form is gated by quota; the ephemeral opt-in is gated by the
  // admin instance gate. They are INDEPENDENT: the ephemeral per-user cap
  // (UZI_EPHEMERAL_MAX_PER_USER) has nothing to do with HostedWorkerQuota, so "manual
  // quota 0 + auto-provision on demand" is a coherent policy and the toggle must
  // survive quota <= 0. If neither surface has anything to offer, render nothing.
  const showManual = config.quota > 0;
  const showEphemeral = config.ephemeral_enabled;
  if (!showManual && !showEphemeral) return null;

  // A HINT, not the gate. The server holds an advisory lock and counts under it; that
  // is the enforcement. This only spares the user a click that would 409 — and a
  // client working from a stale list simply gets the 409 and shows it. Only meaningful
  // when the manual form renders.
  const atQuota = showManual && hostedCount >= config.quota;

  return (
    <Card className="space-y-4">
      {/* One heading for the whole card, rendered whichever surface(s) show — the manual
          form, the ephemeral toggle, or both — so the card is never heading-less (a
          bare toggle with no title is disorienting). It reads correctly above the
          manual form and equally well when the toggle is the only content. */}
      <SectionTitle>Hosted workers</SectionTitle>
      {showManual && (
        <>
          <h3 className="text-sm font-medium">Persistent worker</h3>
          {/* The manual form's own error stays with the form. The success notice is NOT
              here: the page owns one announcement slot for both provisioning and
              deleting, because a delete must be able to replace a provision's message
              ("it appears in your workers below" is a lie once the row is gone) and
              deletes are the page's. Errors stay local — they belong to this form and
              only it can retry them. */}
          {error && <Alert message={error} />}
          <form onSubmit={provision} className="flex flex-wrap items-end gap-3">
            <div className="min-w-[10rem]">
              <Field label="Template">
                <Select
                  // Stable id so the page can move focus here across the component boundary
                  // (D10): the header "Add a worker" button and the empty-state "Provision a
                  // hosted worker" CTA focus this select as the add panel's first control.
                  id="hosted-worker-template"
                  aria-label="Hosted worker template"
                  value={template}
                  onChange={(e) => setTemplate(e.target.value)}
                >
                  {WORKER_TEMPLATES.map((t) => (
                    <option key={t} value={t}>
                      {t}
                    </option>
                  ))}
                </Select>
              </Field>
            </div>
            <div className="min-w-[18rem]">
              <Field label="Size">
                <Select aria-label="Hosted worker size" value={size} onChange={(e) => setSize(e.target.value)}>
                  {WORKER_SIZES.map((s) => (
                    // "M — up to 4 CPU / 12Gi RAM / 25Gi disk". The quantities are IN the
                    // option, not in a table elsewhere, because the point is to inform the
                    // choice at the moment it is made — before M6 this select offered three
                    // bare letters and a user picking one was picking blind.
                    //
                    // Upper-cased for reading only — the value stays the lowercase wire
                    // spelling, which is the one the api accepts.
                    <option key={s} value={s}>
                      {sizeOptionLabel(s)}
                    </option>
                  ))}
                </Select>
              </Field>
            </div>
            {/* This checkbox is submitted with the provision form. The ephemeral
                checkbox below is a persisted preference, saved on change. */}
            <label className="flex items-center gap-2 pb-2 text-sm">
              <input
                type="checkbox"
                aria-label="Docker-capable worker"
                checked={docker}
                onChange={(e) => setDocker(e.target.checked)}
              />
              Docker-capable
            </label>
            <Button type="submit" disabled={busy || atQuota}>
              {busy ? "Provisioning…" : "Provision"}
            </Button>
            <span className="pb-2 text-xs text-muted">
              {hostedCount} of {config.quota} used
              {atQuota && (
                <>
                  {" — "}
                  {/* A link-styled button, not plain text (D "At quota"): it crosses the
                      tabs back to Your workers so the user can delete a hosted worker. The
                      styling mirrors the inline link-buttons elsewhere (e.g. JudgePanel's
                      Undo): the surrounding muted colour, underlined, brightening to text-fg
                      on hover — no new palette class. */}
                  <button
                    type="button"
                    onClick={onShowWorkers}
                    className="text-muted underline underline-offset-2 transition-colors hover:text-fg"
                  >
                    delete one to provision another
                  </button>
                </>
              )}
            </span>
          </form>
          <p className="text-xs text-muted">
            Runs in the cluster, not on your machine: no join token, no container to start.
            It shows up under <strong>Your workers</strong> and you delete it there.{" "}
            <em>Docker-capable</em> gives it a Docker daemon for container builds and tests,
            at extra CPU and storage.
          </p>
        </>
      )}
      {showEphemeral && (
        <div className={showManual ? "space-y-1 border-t border-edge pt-4" : "space-y-1"}>
          <h3 className="text-sm font-medium">Ephemeral workers</h3>
          {/* The agreed UI uses a switch for auto-provision and a persisted Docker
              checkbox on the same row. Both save on change, even with auto-provision off. */}
          <div className="flex flex-wrap items-center gap-4">
            <div className="flex items-center gap-2">
              <Toggle
                label="Auto-provision on demand"
                checked={user?.ephemeral_workers_enabled ?? false}
                disabled={ephemeralBusy}
                aria-describedby="ephemeral-toggle-desc"
                onChange={(next) => toggleEphemeral(next)}
              />
              <span className="text-sm">Auto-provision on demand</span>
            </div>
            {config.docker_enabled && (
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  aria-label="Docker-capable ephemeral workers"
                  aria-describedby="ephemeral-toggle-desc"
                  checked={user?.ephemeral_docker_enabled ?? false}
                  disabled={ephemeralBusy}
                  onChange={(e) => toggleEphemeral({ docker: e.target.checked })}
                />
                Docker-capable
              </label>
            )}
          </div>
          <p id="ephemeral-toggle-desc" className="text-xs text-muted">
            uzi spins up a throwaway hosted worker when a run needs a capability no online
            worker has, or when every capable worker stays busy. It may be kept warm for up
            to 2 hours for a follow-up run on the same branch, and counts toward your
            ephemeral limit meanwhile.
            {config.docker_enabled && " Docker-capable includes Docker for repositories your admin allows, at extra CPU and storage."}
          </p>
          {/* The ephemeral write's own error slot, next to the toggle where the user
              acted — not the manual form's alert at the top of the card. */}
          {ephemeralError && <Alert message={ephemeralError} />}
        </div>
      )}
    </Card>
  );
}
