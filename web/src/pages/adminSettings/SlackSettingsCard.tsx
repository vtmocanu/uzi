import { useState, type FormEvent } from "react";
import {
  api,
  type AppSettings,
  type SettingSource,
  type SettingsResponse,
  type UpdateSettingsPayload,
} from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Badge, type BadgeTone, Button, Card, Field, Input, SectionTitle } from "../../components/ui";

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

// SlackSettingsCard is the admin surface for the Slack integration (PRD #25 M1):
// an enable toggle, write-only bot/app token fields (showing "configured ✓" when
// a value is already stored), the public base URL for deep links, and a
// connection-status chip (stubbed "disabled" until M2 wires live state). A field
// whose value is fixed by an environment variable renders disabled with a
// "set from environment" hint — the server rejects a write to it (409), so this
// greying reflects enforced policy. It saves independently of the label form,
// sending a token only when one was entered.
export function SlackSettingsCard({
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
      setError(errorMessage(err, "Failed to save Slack settings"));
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
