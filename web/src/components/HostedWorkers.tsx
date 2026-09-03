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
  onAvailability,
}: {
  /** How many hosted workers the user already holds, counted from the fleet list the
   *  page polls. There is no count endpoint and none is wanted. */
  hostedCount: number;
  /** Hand the new worker to the page: it owns the announcement slot (a delete has to
   *  be able to replace a provision's message, and deletes are the page's) and the
   *  fleet refresh. */
  onProvisioned: (worker: Worker) => void | Promise<void>;
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
  const { user, refresh } = useAuth();
  const [config, setConfig] = useState<HostedConfig | null>(null);
  const [template, setTemplate] = useState<string>(DEFAULT_WORKER_TEMPLATE);
  const [size, setSize] = useState<string>(DEFAULT_WORKER_SIZE);
  // Opt into a rootless Docker-in-Docker sidecar (PRD #83 M3). Off by default: the
  // plain worker is the common case, docker is the extra one you ask for.
  const [docker, setDocker] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // The ephemeral opt-in write is in flight: disable the toggle so a double-flip cannot
  // race the refresh.
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

  // Flip this user's ephemeral auto-provision opt-in. Mirrors the RunDefaults
  // judge/autopilot pattern: write, then refresh() so useAuth().user reflects the new
  // value everywhere. On error we surface it in the toggle's OWN alert (beside the
  // toggle) and leave it showing user.ephemeral_workers_enabled — which refresh() never
  // advanced — so it never shows a state that did not persist.
  const toggleEphemeral = async (next: boolean) => {
    setEphemeralError("");
    setEphemeralBusy(true);
    try {
      await api.setEphemeralWorkersEnabled(next);
      await refresh();
    } catch (err) {
      setEphemeralError(
        errorMessage(err, "Failed to update auto-provisioning"),
      );
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
                    // "M — up to 2 CPU / 4Gi RAM / 10Gi disk". The quantities are IN the
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
            {/* This stays a plain checkbox on purpose: it is a FORM FIELD submitted with
                the Provision button below, not a persisted setting. Its semantics differ
                from the ephemeral Toggle further down (which persists on flip), so the
                two controls are deliberately different primitives. The label wraps the
                input so the whole thing is one click target with no htmlFor to keep in
                sync. */}
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
              {atQuota && " — delete one to provision another"}
            </span>
          </form>
          <p className="text-xs text-muted">
            A hosted worker runs in the cluster, not on your machine: there is no join token to
            copy and no container to start. It appears below and comes online on its own, and you
            delete it there like any other worker. Tick <em>Docker-capable</em> to give its agent
            a rootless, isolated Docker daemon (for docker and docker&nbsp;compose); it costs extra
            CPU and storage and needs an instance that offers the docker tier.
          </p>
        </>
      )}
      {showEphemeral && (
        <div className="space-y-1">
          {/* Uses the app's Toggle primitive (a role="switch" button) rather than a
              checkbox because this is a PERSISTED, consequential opt-in written the
              moment it flips — the exact analog of the "Trusted repo" master switch in
              Repos.tsx, and the app convention for such settings. (The Docker-capable
              control above stays a checkbox: it is a form field, not a persisted
              setting.) `aria-describedby` links the caveat copy below so a screen reader
              reads the informed-consent caveat with the switch. */}
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
          <p id="ephemeral-toggle-desc" className="text-xs text-muted">
            When on, uzi spins up a run-bound throwaway hosted worker on demand when one of
            your runs needs a capability no online worker has, and reaps it when the run
            finishes. This is <em>experimental</em>, and each ephemeral worker pays a one-time
            ~2.6&nbsp;GiB tool-cache cold start.
          </p>
          {/* The ephemeral write's own error slot, next to the toggle where the user
              acted — not the manual form's alert at the top of the card. */}
          {ephemeralError && <Alert message={ephemeralError} />}
        </div>
      )}
    </Card>
  );
}
