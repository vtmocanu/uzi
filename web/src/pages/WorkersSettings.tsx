// Settings → Workers: register workers, show the one-time join token, and list
// the fleet with live status. Inside SettingsShell.

import { useCallback, useEffect, useState, type FormEvent } from "react";
import { api, ApiError, type Worker } from "../lib/api";
import { Alert, Badge, Button, Card, EmptyState, Field, Input, SectionTitle, Skeleton } from "../components/ui";
import { SettingsShell } from "../components/SettingsShell";
import { ServerIcon } from "../components/icons";

export function WorkersSettings() {
  const [workers, setWorkers] = useState<Worker[]>([]);
  const [loading, setLoading] = useState(true);
  const [name, setName] = useState("");
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
      const { worker, token } = await api.createWorker(name.trim());
      setNewToken({ worker: worker.name, token });
      setCopied(false);
      setName("");
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
          <Button type="submit" disabled={busy || name.trim() === ""}>
            {busy ? "Creating…" : "Generate join token"}
          </Button>
        </form>
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
                    {w.version && <span>v{w.version}</span>}
                    {w.last_heartbeat_at && (
                      <span>· last seen {new Date(w.last_heartbeat_at).toLocaleString()}</span>
                    )}
                  </div>
                </div>
                <div className="flex items-center gap-1.5">
                  <Badge tone={w.status === "online" ? "ok" : "neutral"} dot>
                    {w.status}
                  </Badge>
                  {w.busy && (
                    <Badge tone="warning" title="Holds an active run">
                      busy
                    </Badge>
                  )}
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
