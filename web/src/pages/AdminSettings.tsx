// Admin → Instance settings (PRD #19 M1): the DB-backed app_settings surface.
// Ships the two configurable forge labels; future instance settings (registration
// policy, self-improvement toggle) slot in here without new plumbing. Admin-only
// (gated by AdminRoute + the admin-only API). Validation mirrors the server
// (Decision 8) for immediate feedback, but the server stays the source of truth.

import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  api,
  ApiError,
  type AppSettings,
  type SettingSource,
  type SettingsResponse,
  type UpdateSettingsPayload,
} from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import {
  Alert,
  Badge,
  type BadgeTone,
  Button,
  Card,
  Field,
  Input,
  PageHeader,
  SectionTitle,
  Select,
  Skeleton,
} from "../components/ui";
import { THEMES, THEME_LABELS } from "../lib/theme";

// slackStatusChip renders the live Slack connection state (PRD #25 M2) as a
// tone-coded Badge. Error states (error:auth | error:connection) collapse to a
// danger chip whose full class is in the title.
function slackStatusChip(status: string) {
  if (status.startsWith("error")) {
    return (
      <Badge tone="danger" dot title={`Slack connection: ${status}`}>
        {status.replace("error:", "error · ")}
      </Badge>
    );
  }
  const map: Record<string, { tone: BadgeTone; pulse?: boolean }> = {
    connected: { tone: "ok" },
    connecting: { tone: "warning", pulse: true },
    disabled: { tone: "neutral" },
  };
  const cfg = map[status] ?? { tone: "neutral" as BadgeTone };
  return (
    <Badge tone={cfg.tone} dot pulse={cfg.pulse} title={`Slack connection: ${status}`}>
      {status}
    </Badge>
  );
}

// oidcStatusChip renders OIDC SSO health (PRD #45, Nit6) as a tone-coded Badge:
// ok (green), degraded (warning — configured but discovery is failing), disabled
// (neutral). Env-configured, so this is read-only status, not a form.
function oidcStatusChip(status: string) {
  const map: Record<string, { tone: BadgeTone; pulse?: boolean }> = {
    ok: { tone: "ok" },
    degraded: { tone: "warning", pulse: true },
    disabled: { tone: "neutral" },
  };
  const cfg = map[status] ?? { tone: "neutral" as BadgeTone };
  return (
    <Badge tone={cfg.tone} dot pulse={cfg.pulse} title={`OIDC SSO: ${status}`}>
      {status}
    </Badge>
  );
}

// clientValidate reproduces the server's per-value + cross-key rules so an
// obviously-bad edit is caught before the round-trip. Returns an error message
// or null. The server re-checks regardless. The label triple must be
// pairwise-distinct — the PRDLESS label included, and regardless of its toggle
// state (PRD #22 Decision 7), matching the server's ValidateMerged.
function clientValidate(prdLabel: string, autopilotLabel: string, prdlessLabel: string): string | null {
  for (const [name, value] of [
    ["PRD label", prdLabel],
    ["Autopilot label", autopilotLabel],
    ["PRDLESS label", prdlessLabel],
  ] as const) {
    if (value.trim() === "") return `${name} must not be empty.`;
    if (value.length > 64) return `${name} must be at most 64 characters.`;
    if (value.includes(",")) return `${name} must not contain a comma.`;
  }
  if (prdLabel === autopilotLabel) return "The PRD and autopilot labels must differ.";
  if (prdlessLabel === prdLabel) return "The PRDLESS label must differ from the PRD label.";
  if (prdlessLabel === autopilotLabel) return "The PRDLESS label must differ from the autopilot label.";
  return null;
}

export function AdminSettings() {
  const { refresh } = useAuth();
  const [saved, setSaved] = useState<AppSettings | null>(null);
  // Per-key secret-configured flags and sources (PRD #25), consumed by the Slack
  // card for its "configured ✓" and env-greying behavior.
  const [secrets, setSecrets] = useState<Record<string, boolean>>({});
  const [sources, setSources] = useState<Record<string, SettingSource>>({});
  // Live Slack socket state for the status chip (PRD #25 M2), polled separately
  // so it updates without clobbering in-progress form edits.
  const [slackStatus, setSlackStatus] = useState("disabled");
  // OIDC SSO health for the read-only status line (PRD #45, Nit6). Env-configured,
  // so there is no form — just a chip that surfaces a degraded (discovery-failing) IdP.
  const [oidcStatus, setOidcStatus] = useState("disabled");
  const [oidcProviderName, setOidcProviderName] = useState("SSO");
  const [prdLabel, setPrdLabel] = useState("");
  const [autopilotLabel, setAutopilotLabel] = useState("");
  const [defaultTheme, setDefaultTheme] = useState("");
  const [prdlessEnabled, setPrdlessEnabled] = useState(true);
  const [prdlessLabel, setPrdlessLabel] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  // Vault migration progress (PRD #32): stored secrets still using pre-vault
  // (master-key) encryption. Fetched separately so a failure never blocks the
  // settings form; null = unknown/not-yet-loaded, rendered only when > 0.
  const [masterSealed, setMasterSealed] = useState<number | null>(null);

  // applyResponse fans a GET/PUT response out to the label-form fields and the
  // shared secret/source state, so both the label form and the Slack card read a
  // consistent snapshot.
  const applyResponse = useCallback((resp: SettingsResponse) => {
    const { settings, secrets: sec, sources: src } = resp;
    setSaved(settings);
    setSecrets(sec ?? {});
    setSources(src ?? {});
    setSlackStatus(resp.slack_status ?? "disabled");
    setOidcStatus(resp.oidc_status ?? "disabled");
    setOidcProviderName(resp.oidc_provider_name || "SSO");
    setPrdLabel(settings.prd_label);
    setAutopilotLabel(settings.autopilot_label);
    setDefaultTheme(settings.default_theme);
    setPrdlessEnabled(settings.prdless_enabled === "true");
    setPrdlessLabel(settings.prdless_label);
  }, []);

  const load = useCallback(async () => {
    try {
      applyResponse(await api.getSettings());
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load settings");
    } finally {
      setLoading(false);
    }
    // Best-effort, independent of the settings load (PRD #32).
    api
      .vaultMigration()
      .then(({ master_sealed }) => setMasterSealed(master_sealed))
      .catch(() => setMasterSealed(null));
  }, [applyResponse]);

  useEffect(() => {
    load();
  }, [load]);

  // Poll only the live Slack connection state so the chip reflects connecting →
  // connected without a manual reload. Uses the dedicated status endpoint (not the
  // whole settings blob) and deliberately does NOT call applyResponse (which would
  // reset the form fields and clobber an in-progress edit).
  useEffect(() => {
    const id = setInterval(async () => {
      try {
        const { slack_status } = await api.getSlackStatus();
        setSlackStatus(slack_status ?? "disabled");
      } catch {
        // Best-effort: keep the last known status on a transient failure.
      }
    }, 5000);
    return () => clearInterval(id);
  }, []);

  const dirty =
    saved !== null &&
    (prdLabel !== saved.prd_label ||
      autopilotLabel !== saved.autopilot_label ||
      defaultTheme !== saved.default_theme ||
      (prdlessEnabled ? "true" : "false") !== saved.prdless_enabled ||
      prdlessLabel !== saved.prdless_label);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    const invalid = clientValidate(prdLabel, autopilotLabel, prdlessLabel);
    if (invalid) {
      setError(invalid);
      return;
    }
    setBusy(true);
    // Only a board-filtering label (PRD or autopilot) triggers a repo resync;
    // theme and the prdless keys are presentation-/gate-only (mirrors the server's
    // settings.LabelChanged). Computed against the pre-save `saved` so the notice
    // only mentions propagation when it actually happens (N1).
    const labelChanged =
      saved !== null && (prdLabel !== saved.prd_label || autopilotLabel !== saved.autopilot_label);
    try {
      applyResponse(
        await api.updateSettings({
          prd_label: prdLabel,
          autopilot_label: autopilotLabel,
          default_theme: defaultTheme,
          prdless_enabled: prdlessEnabled ? "true" : "false",
          prdless_label: prdlessLabel,
        }),
      );
      setNotice(
        labelChanged
          ? "Settings saved. Boards reflect the label change after the next sync."
          : "Settings saved.",
      );
      // Re-resolve this admin's own theme: with no personal override, a changed
      // instance default restyles their session live.
      await refresh();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save settings");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-6">
      <PageHeader
        title="Instance settings"
        description="Configuration shared across every user of this uzi instance."
      />
      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      {masterSealed !== null && masterSealed > 0 && (
        <Card className="space-y-2 border-warn/30">
          <SectionTitle>Vault migration</SectionTitle>
          <p className="text-sm text-muted">
            <strong className="text-fg">
              {masterSealed} stored secret{masterSealed === 1 ? "" : "s"}
            </strong>{" "}
            still use pre-vault encryption — their owners haven&rsquo;t logged in since password
            protection was enabled. Each re-seals under the owner&rsquo;s password automatically the
            next time they log in (or unlock). For full protection those owners should also rotate
            the affected token: sealing an existing value protects it from now on, not
            retroactively.
          </p>
        </Card>
      )}

      <Card className="space-y-5">
        <div>
          <SectionTitle>Forge labels</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            Which GitLab labels this factory reacts to. Changing a label never creates it on the
            forge — create the label in GitLab yourself. The labels must all differ.
          </p>
        </div>

        {loading ? (
          <div className="space-y-4">
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-16 w-full" />
          </div>
        ) : (
          <form onSubmit={save} className="space-y-4">
            <div className="space-y-1.5">
              <Field label="PRD label">
                <Input
                  value={prdLabel}
                  maxLength={64}
                  autoComplete="off"
                  placeholder="PRD"
                  onChange={(e) => setPrdLabel(e.target.value)}
                />
              </Field>
              <p className="text-xs text-faint">
                Marks an issue as factory work. The board only shows issues carrying this label.
              </p>
            </div>
            <div className="space-y-1.5">
              <Field label="Autopilot label">
                <Input
                  value={autopilotLabel}
                  maxLength={64}
                  autoComplete="off"
                  placeholder="autopilot"
                  onChange={(e) => setAutopilotLabel(e.target.value)}
                />
              </Field>
              <p className="text-xs text-faint">
                Adding this label to a PRD issue lets an opted-in user run it end to end, with no
                plan-approval step.
              </p>
            </div>
            <div className="space-y-3 border-t border-edge pt-4">
              <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  checked={prdlessEnabled}
                  onChange={(e) => setPrdlessEnabled(e.target.checked)}
                  className="h-4 w-4 rounded border-edge accent-brand"
                />
                Enable the PRDLESS escape hatch
              </label>
              <div className="space-y-1.5">
                <Field label="PRDLESS label">
                  <Input
                    value={prdlessLabel}
                    maxLength={64}
                    autoComplete="off"
                    placeholder="PRDLESS"
                    disabled={!prdlessEnabled}
                    onChange={(e) => setPrdlessLabel(e.target.value)}
                  />
                </Field>
                <p className="text-xs text-faint">
                  An issue carrying this label can start a run with no <code>prds/*.md</code> link.
                  Must differ from the PRD and autopilot labels; the name is editable only while the
                  feature is on.
                </p>
              </div>
            </div>
            <div className="space-y-1.5 border-t border-edge pt-4">
              <Field label="Default theme" htmlFor="default-theme">
                <Select
                  id="default-theme"
                  value={defaultTheme}
                  onChange={(e) => setDefaultTheme(e.target.value)}
                >
                  {THEMES.map((t) => (
                    <option key={t} value={t}>
                      {THEME_LABELS[t]}
                    </option>
                  ))}
                </Select>
              </Field>
              <p className="text-xs text-faint">
                The theme new users, and anyone without a personal choice, see. Each user can
                override it under Settings → Appearance.
              </p>
            </div>
            <Button type="submit" disabled={busy || !dirty}>
              {busy ? "Saving…" : "Save settings"}
            </Button>
          </form>
        )}
      </Card>

      {!loading && oidcStatus !== "disabled" && (
        <Card className="space-y-2">
          <div className="flex items-center justify-between gap-3">
            <div className="min-w-0">
              <SectionTitle>Single sign-on ({oidcProviderName})</SectionTitle>
              <p className="mt-2 text-sm text-muted">
                OIDC is configured via environment variables (restart to change). A{" "}
                <span className="font-medium text-warn">degraded</span> status means the provider is
                enabled but uzi could not reach its discovery document — the button still works and
                a sign-in attempt retries discovery.
              </p>
            </div>
            {oidcStatusChip(oidcStatus)}
          </div>
        </Card>
      )}

      {!loading && saved && (
        <SlackSettingsCard
          settings={saved}
          secrets={secrets}
          sources={sources}
          status={slackStatus}
          onSaved={applyResponse}
        />
      )}
    </div>
  );
}

// SlackSettingsCard is the admin surface for the Slack integration (PRD #25 M1):
// an enable toggle, write-only bot/app token fields (showing "configured ✓" when
// a value is already stored), the public base URL for deep links, and a
// connection-status chip (stubbed "disabled" until M2 wires live state). A field
// whose value is fixed by an environment variable renders disabled with a
// "set from environment" hint — the server rejects a write to it (409), so this
// greying reflects enforced policy. It saves independently of the label form,
// sending a token only when one was entered.
function SlackSettingsCard({
  settings,
  secrets,
  sources,
  status,
  onSaved,
}: {
  settings: AppSettings;
  secrets: Record<string, boolean>;
  sources: Record<string, SettingSource>;
  status: string;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [enabled, setEnabled] = useState(settings.slack_enabled === "true");
  const [publicBaseUrl, setPublicBaseUrl] = useState(settings.public_base_url);
  const [botToken, setBotToken] = useState("");
  const [appToken, setAppToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const isEnv = (key: string) => sources[key] === "env";
  const enabledEnv = isEnv("slack_enabled");
  const baseUrlEnv = isEnv("public_base_url");

  const dirty =
    (enabled ? "true" : "false") !== settings.slack_enabled ||
    publicBaseUrl !== settings.public_base_url ||
    botToken.trim() !== "" ||
    appToken.trim() !== "";

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    // Send only editable (non env-fixed) fields; a token only when one was typed
    // (the write-only fields start empty and empty means "leave unchanged").
    const payload: UpdateSettingsPayload = {};
    if (!enabledEnv) payload.slack_enabled = enabled ? "true" : "false";
    if (!baseUrlEnv) payload.public_base_url = publicBaseUrl;
    if (!isEnv("slack_bot_token") && botToken.trim() !== "") payload.slack_bot_token = botToken;
    if (!isEnv("slack_app_token") && appToken.trim() !== "") payload.slack_app_token = appToken;
    if (Object.keys(payload).length === 0) return;

    setBusy(true);
    try {
      const resp = await api.updateSettings(payload);
      onSaved(resp);
      setEnabled(resp.settings.slack_enabled === "true");
      setPublicBaseUrl(resp.settings.public_base_url);
      setBotToken("");
      setAppToken("");
      setNotice("Slack settings saved.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save Slack settings");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div className="flex items-start justify-between gap-4">
        <div>
          <SectionTitle>Slack</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            Send each user run notifications and plan-approval buttons as Slack DMs. The bot runs
            outbound-only (Socket Mode) — no public URL. Tokens are stored encrypted and never shown
            again.
          </p>
        </div>
        {slackStatusChip(status)}
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      <form onSubmit={save} className="space-y-4">
        <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={enabled}
            disabled={enabledEnv}
            onChange={(e) => setEnabled(e.target.checked)}
            className="h-4 w-4 rounded border-edge accent-brand disabled:opacity-50"
          />
          Enable Slack notifications
        </label>

        <SlackTokenField
          label="Bot token"
          placeholder="xoxb-…"
          value={botToken}
          configured={!!secrets.slack_bot_token}
          env={isEnv("slack_bot_token")}
          onChange={setBotToken}
          help="The xoxb- token from your Slack app (OAuth & Permissions). Validated against Slack when you save."
        />
        <SlackTokenField
          label="App-level token"
          placeholder="xapp-…"
          value={appToken}
          configured={!!secrets.slack_app_token}
          env={isEnv("slack_app_token")}
          onChange={setAppToken}
          help="The xapp- token with connections:write, for the Socket Mode connection."
        />

        <div className="space-y-1.5">
          <Field label="Public base URL">
            <Input
              value={publicBaseUrl}
              autoComplete="off"
              placeholder="http://127.0.0.1:8080"
              disabled={baseUrlEnv}
              onChange={(e) => setPublicBaseUrl(e.target.value)}
            />
          </Field>
          <p className="text-xs text-faint">
            {baseUrlEnv
              ? "Set from environment."
              : "Where Slack message links point. On a laptop the loopback default only resolves for you — set a Tailscale/LAN URL to share links."}
          </p>
        </div>

        <Button type="submit" disabled={busy || !dirty}>
          {busy ? "Saving…" : "Save Slack settings"}
        </Button>
      </form>
    </Card>
  );
}

// SlackTokenField is a write-only secret input: it never renders the stored
// token, shows a "configured ✓" affordance when one is stored, and greys itself
// out with a "set from environment" hint when the value is env-fixed.
function SlackTokenField({
  label,
  placeholder,
  value,
  configured,
  env,
  onChange,
  help,
}: {
  label: string;
  placeholder: string;
  value: string;
  configured: boolean;
  env: boolean;
  onChange: (v: string) => void;
  help: string;
}) {
  return (
    <div className="space-y-1.5">
      <Field label={label}>
        <Input
          type="password"
          value={value}
          autoComplete="off"
          placeholder={env ? "" : configured ? "configured ✓ — enter a new value to replace" : placeholder}
          disabled={env}
          onChange={(e) => onChange(e.target.value)}
        />
      </Field>
      <p className="text-xs text-faint">
        {env ? "Set from environment." : help}
        {!env && configured && " A value is stored; leave blank to keep it."}
      </p>
    </div>
  );
}
