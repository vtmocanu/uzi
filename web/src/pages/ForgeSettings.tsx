// Settings → Forge: connect/verify/delete the GitLab bot PAT. Inside
// SettingsShell; per-row busy state instead of one page-wide busy flag.

import { useCallback, useEffect, useState } from "react";
import { api, ApiError, type ForgeConnection } from "../lib/api";
import { Alert, Badge, Button, Card, EmptyState, Field, Input, SectionTitle, Select, Skeleton } from "../components/ui";
import { SettingsShell } from "../components/SettingsShell";
import { BranchIcon } from "../components/icons";

export function ForgeSettings() {
  const [connections, setConnections] = useState<ForgeConnection[]>([]);
  const [allowedUrls, setAllowedUrls] = useState<string[]>([]);
  const [baseUrl, setBaseUrl] = useState("");
  const [token, setToken] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [warning, setWarning] = useState("");
  const [connecting, setConnecting] = useState(false);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  // Per-connection draft of the user's own forge username (PRD #19 M3). Seeded from
  // the stored value on load; keyed by connection id so multiple connections edit
  // independently.
  const [usernameDrafts, setUsernameDrafts] = useState<Record<string, string>>({});

  const load = useCallback(async () => {
    try {
      const [cfg, conns] = await Promise.all([api.forgeConfig(), api.listConnections()]);
      setAllowedUrls(cfg.allowed_base_urls);
      setBaseUrl((prev) => prev || cfg.allowed_base_urls[0] || "");
      setConnections(conns.connections);
      setUsernameDrafts(
        Object.fromEntries(conns.connections.map((c) => [c.id, c.human_username ?? ""])),
      );
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
    setConnecting(true);
    try {
      const { connection } = await api.createConnection(baseUrl, token);
      setToken("");
      setNotice(`Connected as ${connection.bot_username}.`);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Connect failed");
    } finally {
      setConnecting(false);
    }
  };

  const verify = async (id: string) => {
    setError("");
    setNotice("");
    setBusyId(id);
    try {
      const { connection } = await api.verifyConnection(id);
      setNotice(`Verified ${connection.bot_username}.`);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Verify failed");
    } finally {
      setBusyId(null);
    }
  };

  const remove = async (id: string) => {
    setError("");
    setNotice("");
    setWarning("");
    setBusyId(id);
    try {
      await api.deleteConnection(id);
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Delete failed");
    } finally {
      setBusyId(null);
    }
  };

  const saveUsername = async (id: string) => {
    setError("");
    setNotice("");
    setWarning("");
    setBusyId(id);
    try {
      const { connection, warning: w } = await api.updateConnection(id, usernameDrafts[id] ?? "");
      if (w) {
        setWarning(w);
      } else {
        setNotice(
          connection.human_username
            ? `Saved your forge username: ${connection.human_username}.`
            : "Forge username cleared.",
        );
      }
      await load();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Save failed");
    } finally {
      setBusyId(null);
    }
  };

  return (
    <SettingsShell description="Connect the GitLab bot account uzi acts through.">
      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}
      {warning && <Alert tone="warning" message={warning} />}

      <Card>
        <SectionTitle>Connect a bot PAT</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Create a bot account, give it a personal access token with the{" "}
          <code className="rounded bg-raised px-1 py-0.5 text-fg">api</code> scope, and add it as
          Developer to the projects uzi should see. The token is stored encrypted and never shown
          again.
        </p>
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
          <Button type="submit" disabled={connecting || !baseUrl || !token}>
            {connecting ? "Verifying…" : "Connect"}
          </Button>
        </form>
      </Card>

      {loading ? (
        <Card>
          <Skeleton className="h-5 w-full" />
          <Skeleton className="mt-3 h-5 w-2/3" />
        </Card>
      ) : connections.length === 0 ? (
        <EmptyState
          icon={<BranchIcon />}
          title="No forge connection yet"
          description="Connect a bot PAT above — repos the bot can see become boards."
        />
      ) : (
        <Card className="p-0">
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-edge text-muted">
                <tr>
                  <th className="px-4 py-3 font-medium">Bot</th>
                  <th className="px-4 py-3 font-medium">Base URL</th>
                  <th className="px-4 py-3 font-medium">Last verified</th>
                  <th className="px-4 py-3 text-right font-medium">Actions</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-edge">
                {connections.map((c) => (
                  <tr key={c.id}>
                    <td className="px-4 py-3">
                      <span className="font-medium text-fg">{c.bot_username}</span>{" "}
                      <Badge>{c.forge_type}</Badge>
                    </td>
                    <td className="px-4 py-3 text-muted">{c.base_url}</td>
                    <td className="px-4 py-3 text-muted">
                      {c.last_verified_at ? new Date(c.last_verified_at).toLocaleString() : "—"}
                    </td>
                    <td className="px-4 py-3 text-right">
                      <div className="flex justify-end gap-2">
                        <Button
                          variant="secondary"
                          size="sm"
                          disabled={busyId === c.id}
                          onClick={() => verify(c.id)}
                        >
                          Verify
                        </Button>
                        <Button
                          variant="danger"
                          size="sm"
                          disabled={busyId === c.id}
                          onClick={() => remove(c.id)}
                        >
                          Delete
                        </Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
      {connections.length > 0 && (
        <Card>
          <SectionTitle>Your forge identity (for autopilot)</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            From the bot token, uzi only knows the bot account — not you. To let autopilot attribute an{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">autopilot</code>-labeled issue to you and
            run it under your Anthropic token, tell uzi your own forge username. It is checked against the
            forge on save; a username that does not resolve is still saved, with a warning. Autopilot itself
            stays off until you opt in on your <span className="text-fg">Account &amp; token</span> settings.
          </p>
          <div className="mt-4 space-y-5">
            {connections.map((c) => (
              <div key={c.id} className="space-y-3">
                <Field label={`Your username on ${c.base_url}`}>
                  <Input
                    autoComplete="off"
                    placeholder="your-forge-username"
                    value={usernameDrafts[c.id] ?? ""}
                    onChange={(e) => setUsernameDrafts((d) => ({ ...d, [c.id]: e.target.value }))}
                  />
                </Field>
                <Button
                  variant="secondary"
                  size="sm"
                  disabled={busyId === c.id || (usernameDrafts[c.id] ?? "") === (c.human_username ?? "")}
                  onClick={() => saveUsername(c.id)}
                >
                  {busyId === c.id ? "Saving…" : "Save username"}
                </Button>
              </div>
            ))}
          </div>
        </Card>
      )}

      <p className="text-xs text-faint">
        To rotate a token, connect again with the same base URL — the new PAT replaces the old one.
      </p>
    </SettingsShell>
  );
}
