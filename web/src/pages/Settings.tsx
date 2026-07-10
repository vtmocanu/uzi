// Settings → Account & token: the account card (moved here from the old
// dashboard) plus the Anthropic token lifecycle. Lives inside SettingsShell so
// token/forge/workers are one discoverable area.

import { useCallback, useEffect, useState, type FormEvent } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, isVaultLocked, type SecretMeta } from "../lib/api";
import { Alert, Badge, Button, Card, Field, Input, SectionTitle, Select, Skeleton } from "../components/ui";
import { ModelSelect } from "../components/ModelSelect";
import { modelFieldWarning } from "../lib/agentTemplates";
import { SettingsShell } from "../components/SettingsShell";
import { VaultBadge, useVaultLock } from "../components/VaultControls";
import { prefs } from "../lib/prefs";
import { applyTheme, resolveTheme, THEMES, THEME_LABELS, isTheme } from "../lib/theme";

// One-time dismissal (per browser) of the rotate-your-legacy-token reminder.
const ROTATE_NOTICE_KEY = "uzi.vault.rotateNoticeDismissed";

const DOC_URL =
  "https://gitlab.example.com/vtmocanu/uzi/-/blob/main/docs/anthropic-token.md";

export function Settings() {
  const { user, refresh, themeOverride, defaultTheme, vaultUnlocked } = useAuth();
  const [vaultNotice, setVaultNotice] = useState("");
  const { lock, locking } = useVaultLock(() =>
    setVaultNotice(
      "Vault locked. Runs already in flight finish; your new runs wait as “waiting for vault unlock” until you unlock again.",
    ),
  );
  const [rotateDismissed, setRotateDismissed] = useState(() => prefs.get(ROTATE_NOTICE_KEY, false));
  const dismissRotate = () => {
    prefs.set(ROTATE_NOTICE_KEY, true);
    setRotateDismissed(true);
  };
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

  // Worker model: "" = inherit. savedModel is the persisted value, so Save is
  // only offered when the picker differs from what is stored.
  const [defaultModel, setDefaultModel] = useState("");
  const [savedModel, setSavedModel] = useState("");
  const [modelBusy, setModelBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const [{ secrets }, { settings }] = await Promise.all([api.listSecrets(), api.getMySettings()]);
      setMeta(secrets.find((s) => s.kind === "anthropic_token") ?? null);
      const model = settings.default_model ?? "";
      setDefaultModel(model);
      setSavedModel(model);
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
      setNotice(
        "Token saved. It is sealed with your login password and validated on the first agent run.",
      );
      await load();
    } catch (err) {
      // A mid-session pod restart locks the vault; the global handler already
      // refreshed the session (the unlock banner is now showing), so point there.
      setError(
        isVaultLocked(err)
          ? "Your vault is locked — unlock it with the banner above, then save again."
          : err instanceof ApiError
            ? err.message
            : "Failed to save token",
      );
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

  const modelWarning = modelFieldWarning(defaultModel);
  const modelDirty = defaultModel.trim() !== savedModel;

  // Appearance: the per-user theme override. "" = use the instance default. The
  // change is applied live (optimistic) then persisted; a failed save re-syncs
  // from the server, reverting the optimistic stamp.
  const [themeBusy, setThemeBusy] = useState(false);
  const [themeError, setThemeError] = useState("");

  const changeTheme = async (value: string) => {
    setThemeError("");
    const override = value === "" ? null : value;
    // Apply immediately so the switch feels live; the server value reconciles.
    applyTheme(resolveTheme(override, defaultTheme));
    setThemeBusy(true);
    try {
      await api.putMySettings({ theme: override });
      await refresh(); // sync themeOverride + re-apply the authoritative theme
    } catch (err) {
      setThemeError(err instanceof ApiError ? err.message : "Failed to save theme");
      await refresh(); // revert the optimistic stamp to the server's truth
    } finally {
      setThemeBusy(false);
    }
  };

  const saveModel = async () => {
    setError("");
    setNotice("");
    setModelBusy(true);
    try {
      const { settings } = await api.putMySettings({ default_model: defaultModel.trim() || null });
      const model = settings.default_model ?? "";
      setDefaultModel(model);
      setSavedModel(model);
      setNotice(
        model === ""
          ? "Worker model cleared. Your runs now use the lead template's model."
          : `Worker model set to ${model}. It applies to your next run.`,
      );
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save worker model");
    } finally {
      setModelBusy(false);
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
          <p className="text-xs text-faint">
            Encrypted with your login password. If you forget your password this token cannot be
            recovered and must be re-entered.
          </p>
          <Button type="submit" disabled={busy || token.trim() === ""}>
            {meta ? "Save new token" : "Save token"}
          </Button>
        </form>

        {meta && !rotateDismissed && (
          <div className="rounded-lg border border-info/40 bg-info/10 px-4 py-3 text-sm text-info">
            <p className="text-fg">
              <strong className="font-semibold">Protecting an older token?</strong> If you first saved
              this token before password-protection was enabled, an operator could have read it. The
              protection applies from the moment you save, not retroactively — for full protection,
              rotate the token in the Anthropic console and re-save it above.
            </p>
            <button
              type="button"
              onClick={dismissRotate}
              className="mt-2 text-xs font-medium text-brand hover:text-brand-hover"
            >
              Got it, dismiss
            </button>
          </div>
        )}
      </Card>

      <Card className="space-y-4">
        <div>
          <SectionTitle>Vault</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            Your Anthropic token is sealed with a key derived from your login password, so it is
            readable only while your vault is unlocked in this session. It unlocks
            automatically when you log in and stays unlocked — including overnight — until you lock it
            or the server restarts. While locked, your agents pause: new runs wait as{" "}
            <em>waiting for vault unlock</em> rather than failing.
          </p>
        </div>

        {/* Info (not success/green): locking flips the badge amber, so a green
            "success" toast would clash with the color language. */}
        {vaultNotice && <Alert tone="info" message={vaultNotice} />}

        <div className="flex items-center justify-between rounded-lg border border-edge bg-raised/60 px-4 py-3">
          <VaultBadge />
          {vaultUnlocked ? (
            <Button variant="secondary" size="sm" disabled={locking} onClick={lock}>
              {locking ? "Locking…" : "Lock vault"}
            </Button>
          ) : (
            <span className="text-sm text-muted">Locked — unlock from the banner above.</span>
          )}
        </div>
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

      <Card className="space-y-5">
        <div>
          <SectionTitle>Worker model</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            The Claude model your runs use — the lead orchestrator and its subagents that
            inherit the model. Picking one here overrides the lead template's model for your
            own runs; other users are unaffected. Leave it on <em>Inherit</em> to use the lead
            template's model (opus by default). An unrecognized custom ID only fails on the
            first run.
          </p>
        </div>

        {loading ? (
          <Skeleton className="h-9 w-full max-w-sm" />
        ) : (
          <div className="space-y-3">
            <Field label="Model" htmlFor="worker-model">
              <ModelSelect id="worker-model" value={defaultModel} onChange={setDefaultModel} />
            </Field>
            {modelWarning && <Alert message={modelWarning} tone="warning" />}
            <Button
              type="button"
              disabled={modelBusy || !modelDirty || modelWarning !== ""}
              onClick={saveModel}
            >
              Save model
            </Button>
          </div>
        )}
      </Card>

      <Card className="space-y-5">
        <div>
          <SectionTitle>Appearance</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            The theme uzi renders for you. It follows you across browsers. Leave it on{" "}
            <em>Use default</em> to track the instance default your admin sets.
          </p>
        </div>

        {themeError && <Alert message={themeError} />}

        <div className="max-w-sm">
          <Field label="Theme" htmlFor="appearance-theme">
            <Select
              id="appearance-theme"
              value={isTheme(themeOverride) ? themeOverride : ""}
              disabled={themeBusy}
              onChange={(e) => changeTheme(e.target.value)}
            >
              <option value="">Use default ({THEME_LABELS[defaultTheme]})</option>
              {THEMES.map((t) => (
                <option key={t} value={t}>
                  {THEME_LABELS[t]}
                </option>
              ))}
            </Select>
          </Field>
        </div>
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
