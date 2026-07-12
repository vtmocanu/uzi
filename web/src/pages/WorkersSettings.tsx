// Settings → Workers: register workers, show the one-time join token, and list
// the fleet with live status. Inside SettingsShell.

import { useCallback, useEffect, useState, type FormEvent } from "react";
import { api, ApiError, type Worker } from "../lib/api";
import { Alert, Badge, Button, Card, EmptyState, Field, Input, SectionTitle, Select, Skeleton } from "../components/ui";
import { SettingsShell } from "../components/SettingsShell";
import { ServerIcon } from "../components/icons";
import { DEFAULT_WORKER_TEMPLATE, WORKER_TEMPLATES, hasTemplateDrift } from "../lib/workerTemplates";
import { workerRunBadge } from "../lib/workerRuns";

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
    try {
      await api.deleteWorker(id);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to delete worker");
    }
  };

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
                className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-edge bg-raised/40 px-3 py-2.5 text-sm"
              >
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
                  {(() => {
                    const runBadge = workerRunBadge(w);
                    return (
                      runBadge && (
                        <Badge tone={runBadge.tone} title={runBadge.title}>
                          {runBadge.label}
                        </Badge>
                      )
                    );
                  })()}
                  <Button variant="danger" size="sm" onClick={() => remove(w.id)}>
                    Delete
                  </Button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </SettingsShell>
  );
}
