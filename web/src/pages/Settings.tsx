// Settings → Account & token: the account card (moved here from the old
// dashboard) plus the Anthropic token lifecycle. Lives inside SettingsShell so
// token/forge/workers are one discoverable area.

import { useCallback, useEffect, useState, type FormEvent } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, type SecretMeta } from "../lib/api";
import { Alert, Badge, Button, Card, Field, Input, SectionTitle, Skeleton } from "../components/ui";
import { SettingsShell } from "../components/SettingsShell";

const DOC_URL =
  "https://gitlab.example.com/vtmocanu/uzi/-/blob/main/docs/anthropic-token.md";

export function Settings() {
  const { user, refresh } = useAuth();
  const [meta, setMeta] = useState<SecretMeta | null>(null);
  const [loading, setLoading] = useState(true);
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [autopilotBusy, setAutopilotBusy] = useState(false);
  const [autopilotError, setAutopilotError] = useState("");

  const toggleAutopilot = async (enabled: boolean) => {
    setAutopilotError("");
    setAutopilotBusy(true);
    try {
      await api.setAutopilotEnabled(enabled);
      // Re-fetch the session so useAuth().user reflects the new opt-in everywhere.
      await refresh();
    } catch (err) {
      setAutopilotError(err instanceof ApiError ? err.message : "Failed to update autopilot");
    } finally {
      setAutopilotBusy(false);
    }
  };

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
    <SettingsShell description="Your personal uzi configuration.">
      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      <Card className="space-y-5">
        <div>
          <SectionTitle>Anthropic token</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            uzi runs your agents with your own Anthropic credentials. Paste an OAuth token from{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">claude setup-token</code> or a
            Console API key. It is stored encrypted and validated on the first agent run.{" "}
            <a
              href={DOC_URL}
              target="_blank"
              rel="noreferrer"
              className="text-brand hover:text-brand-hover"
            >
              How to obtain a token
            </a>
            .
          </p>
        </div>

        <div className="flex items-center justify-between rounded-lg border border-edge bg-raised/60 px-4 py-3 text-sm">
          {loading ? (
            <Skeleton className="h-5 w-40" />
          ) : meta ? (
            <div className="flex items-center gap-2">
              <Badge tone="ok" dot>
                Set
              </Badge>
              <span className="text-faint">updated {new Date(meta.updated_at).toLocaleString()}</span>
            </div>
          ) : (
            <Badge tone="neutral">Not set</Badge>
          )}
          {meta && !loading && (
            <Button variant="danger" size="sm" disabled={busy} onClick={remove}>
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

      <Card className="space-y-4">
        <div>
          <SectionTitle>Autopilot</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            With autopilot on, adding the{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">autopilot</code> label alongside{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">PRD</code> on an issue in GitLab starts a
            run <strong className="text-fg">unattended</strong>: it skips the pre-execution plan review and
            spends <strong className="text-fg">your own Anthropic tokens</strong>. The plan is still recorded
            for the audit trail, and the merge-request review stays your human gate. Attribution uses the
            forge username you set under Forge, so only issues that trace back to you can spend your tokens.
            Off by default.
          </p>
        </div>

        {autopilotError && <Alert message={autopilotError} />}

        <label className="flex items-center gap-3 text-sm">
          <input
            type="checkbox"
            className="h-4 w-4 accent-brand"
            checked={user?.autopilot_enabled ?? false}
            disabled={autopilotBusy}
            onChange={(e) => toggleAutopilot(e.target.checked)}
          />
          <span className="text-fg">Enable autopilot for my account</span>
        </label>
      </Card>

      {user && (
        <Card>
          <SectionTitle>Your account</SectionTitle>
          <dl className="mt-3 divide-y divide-edge">
            {(
              [
                ["Email", user.email],
                ["Display name", user.display_name ?? "—"],
                ["Role", user.is_admin ? "Administrator" : "User"],
                ["Account status", user.is_active ? "Active" : "Deactivated"],
                ["Joined", new Date(user.created_at).toLocaleString()],
                ["Last login", user.last_login ? new Date(user.last_login).toLocaleString() : "—"],
              ] as [string, string][]
            ).map(([k, v]) => (
              <div key={k} className="flex justify-between py-2 text-sm">
                <dt className="text-muted">{k}</dt>
                <dd className="text-fg">{v}</dd>
              </div>
            ))}
          </dl>
        </Card>
      )}
    </SettingsShell>
  );
}
