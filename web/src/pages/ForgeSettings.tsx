import { useCallback, useEffect, useState } from "react";
import { api, ApiError, type ForgeConnection } from "../lib/api";
import { Alert, Badge, Button, Card, Field, Input, Select } from "../components/ui";

export function ForgeSettings() {
  const [connections, setConnections] = useState<ForgeConnection[]>([]);
  const [allowedUrls, setAllowedUrls] = useState<string[]>([]);
  const [baseUrl, setBaseUrl] = useState("");
  const [token, setToken] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    try {
      const [cfg, conns] = await Promise.all([api.forgeConfig(), api.listConnections()]);
      setAllowedUrls(cfg.allowed_base_urls);
      setBaseUrl((prev) => prev || cfg.allowed_base_urls[0] || "");
      setConnections(conns.connections);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load forge settings");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const connect = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    setBusy(true);
    try {
      const { connection } = await api.createConnection(baseUrl, token);
      setToken("");
      setNotice(`Connected as ${connection.bot_username}.`);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Connect failed");
    } finally {
      setBusy(false);
    }
  };

  const verify = async (id: string) => {
    setError("");
    setNotice("");
    setBusy(true);
    try {
      const { connection } = await api.verifyConnection(id);
      setNotice(`Verified ${connection.bot_username}.`);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Verify failed");
    } finally {
      setBusy(false);
    }
  };

  const remove = async (id: string) => {
    setError("");
    setNotice("");
    setBusy(true);
    try {
      await api.deleteConnection(id);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Delete failed");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold">Forge</h1>
        <p className="mt-1 text-slate-400">
          Connect your GitLab bot account. Create a bot, give it a personal access token with the{" "}
          <code className="text-slate-300">api</code> scope, and add it as Developer to the projects
          uzi should see. The token is stored encrypted and never shown again.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && (
        <div className="rounded-lg border border-emerald-800 bg-emerald-950/50 px-3 py-2 text-sm text-emerald-200">
          {notice}
        </div>
      )}

      <Card>
        <h2 className="text-sm font-semibold uppercase tracking-wide text-slate-500">
          Connect a bot PAT
        </h2>
        <form className="mt-4 space-y-4" onSubmit={connect}>
          <Field label="Forge base URL">
            <Select value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)}>
              {allowedUrls.map((u) => (
                <option key={u} value={u}>
                  {u}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="Bot personal access token (scope: api)">
            <Input
              type="password"
              autoComplete="off"
              placeholder="glpat-…"
              value={token}
              onChange={(e) => setToken(e.target.value)}
            />
          </Field>
          <Button type="submit" disabled={busy || !baseUrl || !token}>
            {busy ? "Verifying…" : "Connect"}
          </Button>
        </form>
      </Card>

      <Card className="p-0">
        <div className="overflow-x-auto">
          <table className="w-full text-left text-sm">
            <thead className="border-b border-slate-800 text-slate-400">
              <tr>
                <th className="px-4 py-3 font-medium">Bot</th>
                <th className="px-4 py-3 font-medium">Base URL</th>
                <th className="px-4 py-3 font-medium">Last verified</th>
                <th className="px-4 py-3 text-right font-medium">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-800">
              {loading ? (
                <tr>
                  <td colSpan={4} className="px-4 py-6 text-center text-slate-500">
                    Loading…
                  </td>
                </tr>
              ) : connections.length === 0 ? (
                <tr>
                  <td colSpan={4} className="px-4 py-6 text-center text-slate-500">
                    No connections yet.
                  </td>
                </tr>
              ) : (
                connections.map((c) => (
                  <tr key={c.id}>
                    <td className="px-4 py-3">
                      <span className="font-medium text-slate-100">{c.bot_username}</span>{" "}
                      <Badge>{c.forge_type}</Badge>
                    </td>
                    <td className="px-4 py-3 text-slate-400">{c.base_url}</td>
                    <td className="px-4 py-3 text-slate-400">
                      {c.last_verified_at ? new Date(c.last_verified_at).toLocaleString() : "—"}
                    </td>
                    <td className="px-4 py-3 text-right">
                      <div className="flex justify-end gap-2">
                        <Button variant="ghost" disabled={busy} onClick={() => verify(c.id)}>
                          Verify
                        </Button>
                        <Button variant="danger" disabled={busy} onClick={() => remove(c.id)}>
                          Delete
                        </Button>
                      </div>
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </Card>
      <p className="text-xs text-slate-600">
        To rotate a token, connect again with the same base URL — the new PAT replaces the old one.
      </p>
    </div>
  );
}
