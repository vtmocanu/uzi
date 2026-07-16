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

import { useEffect, useState, type FormEvent } from "react";
import { api, ApiError, type HostedConfig } from "../lib/api";
import { Alert, Button, Card, Field, SectionTitle, Select } from "./ui";
import { DEFAULT_WORKER_TEMPLATE, WORKER_TEMPLATES } from "../lib/workerTemplates";
import { DEFAULT_WORKER_SIZE, WORKER_SIZES, sizeLabel } from "../lib/workerSizes";

export function HostedWorkers({
  hostedCount,
  onProvisioned,
}: {
  /** How many hosted workers the user already holds, counted from the fleet list the
   *  page polls. There is no count endpoint and none is wanted. */
  hostedCount: number;
  /** Refresh the fleet so the new row appears. */
  onProvisioned: () => void | Promise<void>;
}) {
  const [config, setConfig] = useState<HostedConfig | null>(null);
  const [template, setTemplate] = useState<string>(DEFAULT_WORKER_TEMPLATE);
  const [size, setSize] = useState<string>(DEFAULT_WORKER_SIZE);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  // Fetched once, on mount, and never polled: enabled/quota are operator-set POLICY,
  // which changes on a deploy or an admin edit, not on the 10s liveness rhythm the
  // fleet list needs. A failed read fails CLOSED — config stays null and the card
  // stays hidden, indistinguishable from hosting being off. That is the honest
  // failure: the alternative is an alert about a capability probe for a feature the
  // user may not even have, on every blip.
  useEffect(() => {
    let live = true;
    api
      .hostedConfig()
      .then((cfg) => {
        if (live) setConfig(cfg);
      })
      .catch(() => {
        // Fail closed; see above.
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
      await api.provisionHostedWorker(template, size);
      setTemplate(DEFAULT_WORKER_TEMPLATE);
      setSize(DEFAULT_WORKER_SIZE);
      await onProvisioned();
    } catch (err) {
      // The server's message is what the user reads, verbatim: it distinguishes the
      // quota being reached (409 — delete one and retry) from provisioning being
      // switched off underneath us (403 — this card should not have been shown, so
      // our config read is stale) and from the rate limiter (429).
      setError(err instanceof ApiError ? err.message : "Failed to provision worker");
    } finally {
      setBusy(false);
    }
  };

  // Nothing hosted renders until the config says so: not loaded, failed, or disabled
  // all look the same on purpose.
  if (!config?.enabled) return null;
  // quota 0 is POLICY — the admin turned self-service off — so the form goes, and no
  // amount of deleting brings it back. The user's existing hosted rows keep rendering
  // and stay deletable: they are the page's, not this card's, which is what stops a
  // quota change from stranding workers someone already owns.
  if (config.quota <= 0) return null;

  // A HINT, not the gate. The server holds an advisory lock and counts under it; that
  // is the enforcement. This only spares the user a click that would 409 — and a
  // client working from a stale list simply gets the 409 and shows it.
  const atQuota = hostedCount >= config.quota;

  return (
    <Card className="space-y-4">
      <SectionTitle>Provision a hosted worker</SectionTitle>
      {error && <Alert message={error} />}
      <form onSubmit={provision} className="flex flex-wrap items-end gap-3">
        <div className="min-w-[10rem]">
          <Field label="Template">
            <Select
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
        <div className="min-w-[8rem]">
          <Field label="Size">
            <Select aria-label="Hosted worker size" value={size} onChange={(e) => setSize(e.target.value)}>
              {WORKER_SIZES.map((s) => (
                // Upper-cased for reading only — the value stays the lowercase wire
                // spelling, which is the one the api accepts.
                <option key={s} value={s}>
                  {sizeLabel(s)}
                </option>
              ))}
            </Select>
          </Field>
        </div>
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
        delete it there like any other worker.
      </p>
    </Card>
  );
}
