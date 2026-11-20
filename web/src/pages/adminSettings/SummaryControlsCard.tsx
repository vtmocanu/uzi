import { useState, type FormEvent } from "react";
import {
  api,
  type AppSettings,
  type SettingSource,
  type SettingsResponse,
  type UpdateSettingsPayload,
} from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, SectionTitle } from "../../components/ui";

// SummaryControlsCard is the admin surface for the run-summary controls that are not
// models (PRD #2603): the instance kill-switch for the model-written "Now" line. The
// summary models themselves live in InstanceModelDefaultsCard (#3259). Saved on its own,
// sending only now_summary_enabled.
export function SummaryControlsCard({
  settings,
  sources,
  onSaved,
}: {
  settings: AppSettings;
  sources: Record<string, SettingSource>;
  onSaved: (resp: SettingsResponse, sent: UpdateSettingsPayload) => void;
}) {
  // Absent (an older server) reads as on, the server default.
  const saved = settings.now_summary_enabled !== "false";
  const [nowEnabled, setNowEnabled] = useState(saved);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const fromEnv = sources.now_summary_enabled === "env";
  const dirty = !fromEnv && nowEnabled !== saved;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (busy || !dirty) return;
    setError("");
    setNotice("");
    setBusy(true);
    try {
      const payload: UpdateSettingsPayload = { now_summary_enabled: String(nowEnabled) };
      const resp = await api.updateSettings(payload);
      onSaved(resp, payload);
      setNowEnabled(resp.settings.now_summary_enabled !== "false");
      setNotice("Run summary controls saved.");
    } catch (err) {
      setError(errorMessage(err, "Failed to save run summary controls"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Run summaries</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Runs write short plain-English summaries on{" "}
          <strong className="text-fg">the run owner&rsquo;s own credential</strong>. Summaries are advisory and
          never block a run. Pick the summary models in{" "}
          <a href="#model-defaults" className="text-brand underline underline-offset-2">
            Model defaults
          </a>
          .
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      <form onSubmit={save} className="space-y-4">
        <div className="space-y-1.5">
          <label
            className={`flex select-none items-center gap-2 text-sm ${
              fromEnv ? "cursor-not-allowed opacity-50" : "cursor-pointer"
            }`}
          >
            <input
              type="checkbox"
              checked={nowEnabled}
              disabled={fromEnv}
              onChange={(e) => setNowEnabled(e.target.checked)}
              className="h-4 w-4 rounded border-edge accent-brand"
            />
            Model-written Now line
          </label>
          {fromEnv && <p className="text-xs text-faint">Set from the environment</p>}
          <p className="text-xs text-faint">
            Instance kill-switch. While a run works, a small model writes a one-sentence summary of its
            current step on the progress card, on the run owner&rsquo;s own credential. Off stops it for
            every user, whatever their own setting. On by default.
          </p>
        </div>

        <Button type="submit" disabled={busy || !dirty}>
          {busy ? "Saving…" : "Save run summary controls"}
        </Button>
      </form>
    </Card>
  );
}
