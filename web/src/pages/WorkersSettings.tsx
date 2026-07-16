// Settings → Workers: register workers, show the one-time join token, and list
// the fleet with live status. Inside SettingsShell.

import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { api, ApiError, type Worker } from "../lib/api";
import { Alert, Badge, Button, Card, EmptyState, Field, Input, SectionTitle, Select, Skeleton } from "../components/ui";
import { SettingsShell } from "../components/SettingsShell";
import { ServerIcon } from "../components/icons";
import { DEFAULT_WORKER_TEMPLATE, WORKER_TEMPLATES, hasTemplateDrift } from "../lib/workerTemplates";
import { sizeLabel } from "../lib/workerSizes";
import { HostedWorkers } from "../components/HostedWorkers";
import { WorkerRunBadge } from "../components/WorkerRunBadge";
import { WorkerStatGauges } from "../components/WorkerStats";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";

export function WorkersSettings() {
  const [workers, setWorkers] = useState<Worker[]>([]);
  const [loading, setLoading] = useState(true);
  const [name, setName] = useState("");
  // The declared worker template (PRD #18): what image the user says this worker
  // will run. Defaults to the base image; the worker later self-reports its
  // actual template and any mismatch shows as a drift badge.
  const [template, setTemplate] = useState<string>(DEFAULT_WORKER_TEMPLATE);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // newToken holds the plaintext join token for the just-created worker. It is
  // shown exactly once (only its hash is stored server-side) and cleared on the
  // next action.
  const [newToken, setNewToken] = useState<{ worker: string; token: string } | null>(null);
  const [copied, setCopied] = useState(false);
  // Which hosted worker is armed for deletion (PRD #58). Hosted only — see the Delete
  // button below.
  const [confirmingDelete, setConfirmingDelete] = useState<string | null>(null);
  const confirmRef = useRef<HTMLDivElement>(null);

  const load = useCallback(async () => {
    try {
      const { workers } = await api.listWorkers();
      setWorkers(workers);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load workers");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // Liveness (PRD #49): re-fetch the fleet every 10s while the tab is visible — the
  // same rhythm the Dashboard uses — so live CPU/memory gauges refresh without a
  // reload (heartbeat cadence 15s × this 10s poll). A poll error keeps the last-good
  // list (it must never blank the fleet or flash an error), unlike the first load.
  const poll = useCallback(async () => {
    try {
      const { workers } = await api.listWorkers();
      setWorkers(workers);
    } catch {
      // keep the last-good list
    }
  }, []);
  usePollWhileVisible(poll, 10000);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setBusy(true);
    try {
      const { worker, token } = await api.createWorker(name.trim(), template);
      setNewToken({ worker: worker.name, token });
      setCopied(false);
      setName("");
      setTemplate(DEFAULT_WORKER_TEMPLATE);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to create worker");
    } finally {
      setBusy(false);
    }
  };

  const remove = async (id: string) => {
    setError("");
    setConfirmingDelete(null);
    try {
      await api.deleteWorker(id);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to delete worker");
    }
  };

  // Focus the confirmation when it arms, so a keyboard user meets the warning instead
  // of hunting for it, and a screen reader reads it rather than announcing nothing.
  // Focus lands on the WARNING, never on "Delete anyway": auto-focusing the
  // destructive control is how a confirmation becomes a formality.
  useEffect(() => {
    if (confirmingDelete) confirmRef.current?.focus();
  }, [confirmingDelete]);

  const copy = async () => {
    if (!newToken) return;
    try {
      await navigator.clipboard.writeText(newToken.token);
      setCopied(true);
    } catch {
      // Clipboard may be unavailable (insecure context); the token stays visible
      // to copy manually.
    }
  };

  return (
    <SettingsShell description="Workers are your uzi-agent containers: they claim your runs and stream them back.">
      {error && <Alert message={error} />}

      {newToken && (
        <Card className="space-y-3 border-ok/40">
          <SectionTitle className="text-ok">Join token for “{newToken.worker}”</SectionTitle>
          <p className="text-sm text-muted">
            Copy it now — it is shown once and never again (only its hash is stored). Set it as{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">UZI_WORKER_TOKEN</code> on the
            worker container.
          </p>
          <div className="flex items-center gap-2">
            <code className="flex-1 overflow-x-auto rounded-lg border border-edge bg-ink px-3 py-2 font-mono text-sm text-ok">
              {newToken.token}
            </code>
            <Button variant="secondary" onClick={copy}>
              {copied ? "Copied" : "Copy"}
            </Button>
          </div>
          <div>
            <Button variant="ghost" onClick={() => setNewToken(null)}>
              Done
            </Button>
          </div>
        </Card>
      )}

      <Card className="space-y-4">
        <SectionTitle>Register a worker</SectionTitle>
        <form onSubmit={create} className="flex flex-wrap items-end gap-3">
          <div className="min-w-[16rem] flex-1">
            <Field label="Name">
              <Input
                placeholder="e.g. laptop, ci-runner-1"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </Field>
          </div>
          <div className="min-w-[10rem]">
            <Field label="Template">
              <Select
                aria-label="Worker template"
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
          <Button type="submit" disabled={busy || name.trim() === ""}>
            {busy ? "Creating…" : "Generate join token"}
          </Button>
        </form>
        <p className="text-xs text-muted">
          The template is the worker image to build (<code className="rounded bg-raised px-1 py-0.5 text-fg">base</code> plus
          heavy-dependency variants like <code className="rounded bg-raised px-1 py-0.5 text-fg">jvm</code>). Build the
          worker with a matching{" "}
          <code className="rounded bg-raised px-1 py-0.5 text-fg">WORKER_TEMPLATE</code>; if the worker reports a different
          one it is flagged below, never rejected.
        </p>
      </Card>

      {/* Hosted workers (PRD #58): renders itself only when the instance has hosting
          on and self-service quota left. One list below, not two — a hosted worker is
          an ordinary worker whose container the controller runs, so it keeps the same
          status, gauges, run badge and delete rule, and only its origin differs. */}
      <HostedWorkers
        hostedCount={workers.filter((w) => w.kind === "hosted").length}
        onProvisioned={load}
      />

      <Card className="space-y-3">
        <SectionTitle>Your workers</SectionTitle>
        {loading ? (
          <div className="space-y-2">
            <Skeleton className="h-12" />
            <Skeleton className="h-12" />
          </div>
        ) : workers.length === 0 ? (
          <EmptyState
            icon={<ServerIcon />}
            title="No workers yet"
            description="Generate a join token above, then start the uzi-agent container with it — the worker shows up here when it heartbeats."
          />
        ) : (
          <ul className="space-y-2">
            {workers.map((w) => (
              <li
                key={w.id}
                className="flex flex-col gap-2 rounded-lg border border-edge bg-raised/40 px-3 py-2.5 text-sm"
              >
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <div>
                    <span className="font-medium text-fg">{w.name}</span>
                    <div className="mt-0.5 flex flex-wrap items-center gap-x-2 text-xs text-faint">
                      {w.template_reported ? (
                        <span>template {w.template_reported}</span>
                      ) : (
                        w.template_declared && <span>template {w.template_declared} (awaiting report)</span>
                      )}
                      {w.version && <span>· v{w.version}</span>}
                      {w.last_heartbeat_at && (
                        <span>· last seen {new Date(w.last_heartbeat_at).toLocaleString()}</span>
                      )}
                    </div>
                  </div>
                  <div className="flex items-center gap-1.5">
                    {/* Marked by the row's own data, not by the hosting config: if an
                        admin turns hosting off while a user still holds hosted rows,
                        the rows stay listed (nothing may strand them) and must stay
                        honest about what they are. Hiding the badge there would leave
                        them looking like workers the user forgot to start. */}
                    {w.kind === "hosted" && (
                      <>
                        <Badge tone="info" title="Runs in the cluster: the controller starts and stops its container, not you.">
                          hosted
                        </Badge>
                        {/* Names-only means no quantity appears anywhere, so this chip
                            is the ONLY trace of the size the worker was provisioned
                            at — a bare "M" announces as "M" and explains nothing.
                            The title says what the letter is; what it BUYS is M6's,
                            once the controller's table exists to read it from. */}
                        {w.hosted_size && (
                          <Badge title={`Size ${sizeLabel(w.hosted_size)}`}>{sizeLabel(w.hosted_size)}</Badge>
                        )}
                      </>
                    )}
                    {hasTemplateDrift(w.template_declared, w.template_reported) && (
                      <Badge
                        tone="warning"
                        title={`Declared ${w.template_declared}, but the worker reports ${w.template_reported}. Rebuild it with WORKER_TEMPLATE=${w.template_declared} to match.`}
                      >
                        template drift
                      </Badge>
                    )}
                    <Badge tone={w.status === "online" ? "ok" : "neutral"} dot>
                      {w.status}
                    </Badge>
                    <WorkerRunBadge worker={w} />
                    {/* Hosted deletes confirm; external ones stay one click, and that
                        asymmetry is the whole point. Deleting an EXTERNAL worker
                        revokes a token — the container keeps running and the user
                        re-registers to recover. Deleting a HOSTED one takes its disks
                        with it (PRD: "PVCs accrue cost — deleted with the worker"),
                        which nothing undoes. Same button, wildly different blast
                        radius, so only the expensive one asks. */}
                    {confirmingDelete !== w.id && (
                      <Button
                        variant="danger"
                        size="sm"
                        onClick={() => (w.kind === "hosted" ? setConfirmingDelete(w.id) : remove(w.id))}
                      >
                        Delete
                      </Button>
                    )}
                  </div>
                </div>
                {/* Confirm-in-place, not a modal: same ruling as the provision form —
                    there is no modal primitive in web/ and a row action is not where to
                    invent one. It names what is destroyed rather than asking a
                    content-free "are you sure", because the whole reason it exists is
                    that the cost is invisible at the moment of the click.

                    "Delete is not a restart" leads deliberately: v1 ships no restart
                    endpoint (PRD non-goals: "no restart endpoint (delete +
                    reprovision)"), so Delete is the ONLY lifecycle control a hosted
                    user has, and they will reach for it on a stuck worker. Without
                    this they would silently pay the full /nix re-fetch from
                    cache.nixos.org — the exact off-LAN cost that volume exists to
                    prevent (Decision 7).

                    On today's build no controller exists (M3), so nothing is reaped
                    and there are no disks to lose. The copy states the RULE ("its
                    disks go with it"), which holds either way and becomes operative
                    the moment M3 ships — which is when a real user meets it. Warning
                    early costs a click; warning late costs someone their /nix. */}
                {confirmingDelete === w.id && (
                  <div
                    ref={confirmRef}
                    tabIndex={-1}
                    role="group"
                    aria-label={`Confirm deleting ${w.name}`}
                    onKeyDown={(e) => {
                      if (e.key === "Escape") setConfirmingDelete(null);
                    }}
                    className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2 outline-none"
                  >
                    <p className="text-xs text-warn">
                      Delete is not a restart: a hosted worker’s disks go with it, permanently —{" "}
                      <code className="rounded bg-raised px-1 py-0.5">/data</code> (its workspace) and{" "}
                      <code className="rounded bg-raised px-1 py-0.5">/nix</code> (its cached tools). A replacement
                      re-downloads its tools from the internet.
                    </p>
                    <div className="flex items-center gap-1.5">
                      <Button variant="danger" size="sm" onClick={() => remove(w.id)}>
                        Delete anyway
                      </Button>
                      <Button variant="ghost" size="sm" onClick={() => setConfirmingDelete(null)}>
                        Cancel
                      </Button>
                    </div>
                  </div>
                )}
                {/* Live resource gauges (PRD #49): renders only once the worker has
                    reported a sample, so a worker without stats keeps its old row. */}
                <WorkerStatGauges worker={w} />
              </li>
            ))}
          </ul>
        )}
      </Card>
    </SettingsShell>
  );
}
