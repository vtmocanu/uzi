import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { api, ApiError, type SecretMeta } from "../lib/api";
import { Alert, Button, Card, Field, Input } from "../components/ui";

const DOC_URL =
  "https://gitlab.example.com/vtmocanu/uzi/-/blob/main/docs/anthropic-token.md";

export function Settings() {
  const [meta, setMeta] = useState<SecretMeta | null>(null);
  const [loading, setLoading] = useState(true);
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const load = useCallback(async () => {
    try {
      const { secrets } = await api.listSecrets();
      setMeta(secrets.find((s) => s.kind === "anthropic_token") ?? null);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load settings");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    setBusy(true);
    try {
      await api.putAnthropicToken(token);
      setToken("");
      setNotice("Token saved. It is encrypted at rest and validated on the first agent run.");
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save token");
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    setError("");
    setNotice("");
    setBusy(true);
    try {
      await api.deleteAnthropicToken();
      setNotice("Token removed. uzi is no longer connected to your Anthropic account.");
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to remove token");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-6">
      <div className="flex items-end justify-between">
        <div>
          <h1 className="text-2xl font-semibold">Settings</h1>
          <p className="mt-1 text-slate-400">Your personal uzi configuration.</p>
        </div>
        <Link to="/settings/workers" className="text-sm text-indigo-400 hover:text-indigo-300">
          Workers
        </Link>
      </div>

      {error && <Alert message={error} />}
      {notice && (
        <div className="rounded-lg border border-emerald-800 bg-emerald-950/60 px-3 py-2 text-sm text-emerald-200">
          {notice}
        </div>
      )}

      <Card className="space-y-5">
        <div>
          <h2 className="text-sm font-semibold uppercase tracking-wide text-slate-500">
            Anthropic token
          </h2>
          <p className="mt-2 text-sm text-slate-400">
            uzi runs your agents with your own Anthropic credentials. Paste an OAuth token from{" "}
            <code className="rounded bg-slate-800 px-1 py-0.5 text-slate-200">claude setup-token</code>{" "}
            or a Console API key. It is stored encrypted and validated on the first agent run.{" "}
            <a
              href={DOC_URL}
              target="_blank"
              rel="noreferrer"
              className="text-indigo-400 hover:text-indigo-300"
            >
              How to obtain a token
            </a>
            .
          </p>
        </div>

        <div className="flex items-center justify-between rounded-lg border border-slate-800 bg-slate-900/60 px-4 py-3 text-sm">
          {loading ? (
            <span className="text-slate-500">Loading…</span>
          ) : meta ? (
            <div>
              <span className="font-medium text-emerald-400">Set</span>
              <span className="ml-2 text-slate-500">
                updated {new Date(meta.updated_at).toLocaleString()}
              </span>
            </div>
          ) : (
            <span className="text-slate-400">Not set</span>
          )}
          {meta && !loading && (
            <Button variant="danger" disabled={busy} onClick={remove}>
              Delete
            </Button>
          )}
        </div>

        <form onSubmit={save} className="space-y-3">
          <Field label={meta ? "Replace token" : "Token"}>
            <Input
              type="password"
              autoComplete="off"
              placeholder="Paste your Anthropic token"
              value={token}
              onChange={(e) => setToken(e.target.value)}
            />
          </Field>
          <Button type="submit" disabled={busy || token.trim() === ""}>
            {meta ? "Save new token" : "Save token"}
          </Button>
        </form>
      </Card>
    </div>
  );
}
