import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { api, ApiError, type Worker } from "../lib/api";
import { Alert, Badge, Button, Card, Field, Input } from "../components/ui";

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
    <div className="space-y-6">
      <div className="flex items-end justify-between">
        <div>
          <h1 className="text-2xl font-semibold">Workers</h1>
          <p className="mt-1 text-slate-400">
            A worker is your uzi-agent container. It claims your runs and streams them back.
          </p>
        </div>
        <Link to="/settings" className="text-sm text-indigo-400 hover:text-indigo-300">
          Settings
        </Link>
      </div>

      {error && <Alert message={error} />}

      {newToken && (
        <Card className="space-y-3 border-emerald-800/70">
          <h2 className="text-sm font-semibold uppercase tracking-wide text-emerald-300">
            Join token for “{newToken.worker}”
          </h2>
          <p className="text-sm text-slate-400">
            Copy it now — it is shown once and never again (only its hash is stored). Set it as{" "}
            <code className="rounded bg-slate-800 px-1 py-0.5 text-slate-200">UZI_WORKER_TOKEN</code>{" "}
            on the worker container.
          </p>
          <div className="flex items-center gap-2">
            <code className="flex-1 overflow-x-auto rounded-lg border border-slate-700 bg-slate-900 px-3 py-2 text-sm text-emerald-200">
              {newToken.token}
            </code>
            <Button variant="ghost" onClick={copy}>
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
        <h2 className="text-sm font-semibold uppercase tracking-wide text-slate-500">
          Register a worker
        </h2>
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
        <h2 className="text-sm font-semibold uppercase tracking-wide text-slate-500">
          Your workers
        </h2>
        {loading ? (
          <p className="text-sm text-slate-500">Loading…</p>
        ) : workers.length === 0 ? (
          <p className="text-sm text-slate-600">No workers yet. Generate a join token above.</p>
        ) : (
          <ul className="space-y-2">
            {workers.map((w) => (
              <li
                key={w.id}
                className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-slate-800 bg-slate-900/40 px-3 py-2 text-sm"
              >
                <div>
                  <span className="font-medium text-slate-100">{w.name}</span>
                  <div className="mt-0.5 flex flex-wrap items-center gap-x-2 text-xs text-slate-500">
                    {w.version && <span>v{w.version}</span>}
                    {w.last_heartbeat_at && (
                      <span>· last seen {new Date(w.last_heartbeat_at).toLocaleString()}</span>
                    )}
                  </div>
                </div>
                <div className="flex items-center gap-1.5">
                  <Badge tone={w.status === "online" ? "neutral" : "danger"}>{w.status}</Badge>
                  {w.busy && (
                    <Badge tone="warning" title="Holds an active run">
                      busy
                    </Badge>
                  )}
                  <Button variant="danger" onClick={() => remove(w.id)}>
                    Delete
                  </Button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </div>
  );
}
