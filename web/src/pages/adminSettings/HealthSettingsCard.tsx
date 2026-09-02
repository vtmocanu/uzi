import { useState, type FormEvent } from "react";
import {
  api,
  type AppSettings,
  type SettingSource,
  type SettingsResponse,
  type UpdateSettingsPayload,
} from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, Field, Input, SectionTitle } from "../../components/ui";

const HEALTH_FIELDS: { key: keyof AppSettings; label: string; hint?: string }[] = [
  {
    key: "health_stall_seconds",
    label: "Stalled after (seconds of silence)",
    hint: "No new activity while no tool call is in flight.",
  },
  {
    key: "health_slow_seconds",
    label: "Slow after (seconds running)",
    hint: "Wall clock since the run started; clamped below RUN_TIMEOUT.",
  },
  { key: "health_queued_seconds", label: "Stuck queued after (seconds)" },
  { key: "health_approval_seconds", label: "Awaiting approval after (seconds)" },
  { key: "health_nudge_cooldown_seconds", label: "Slack nudge cooldown (seconds)" },
];

// validateHealthSeconds mirrors the server's write-time rule (Decision 5) for
// immediate feedback: 0 (disable) or an integer in [60, 86400]. The digit-only test
// keeps parity with the server's strconv.Atoi, which rejects the forms Number()
// would silently accept ("1e3", "0x10", "5.0"); the server stays the source of truth.
function validateHealthSeconds(value: string): string | null {
  const v = value.trim();
  if (!/^\d+$/.test(v)) return "Must be a whole number of seconds";
  const n = Number(v);
  if (n === 0) return null;
  if (n < 60 || n > 86400) return "Must be 0 (disabled) or between 60 and 86400 seconds";
  return null;
}

// HealthSettingsCard is the admin surface for the run-health detector (PRD #47): an
// enable toggle plus the five integer-seconds thresholds. It saves independently of
// the other cards, sending only the fields that changed. The health keys are never
// env-sourced (Decision 5: no env vars), but the env guard is kept for symmetry —
// the server rejects an env write anyway.
export function HealthSettingsCard({
  settings,
  sources,
  onSaved,
}: {
  settings: AppSettings;
  sources: Record<string, SettingSource>;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [enabled, setEnabled] = useState(settings.health_enabled === "true");
  const [values, setValues] = useState<Record<string, string>>(() =>
    Object.fromEntries(HEALTH_FIELDS.map((f) => [f.key, settings[f.key]])),
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const isEnv = (key: string) => sources[key] === "env";

  const fieldError = HEALTH_FIELDS.map((f) => validateHealthSeconds(values[f.key])).find(Boolean) ?? null;

  const dirty =
    (enabled ? "true" : "false") !== settings.health_enabled ||
    HEALTH_FIELDS.some((f) => values[f.key] !== settings[f.key]);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    if (fieldError) {
      setError(fieldError);
      return;
    }
    // Send only what changed (and is not env-fixed), so an idempotent save is a no-op.
    const payload: UpdateSettingsPayload = {};
    if (!isEnv("health_enabled") && (enabled ? "true" : "false") !== settings.health_enabled) {
      payload.health_enabled = enabled ? "true" : "false";
    }
    for (const f of HEALTH_FIELDS) {
      if (!isEnv(f.key) && values[f.key].trim() !== settings[f.key]) {
        payload[f.key] = values[f.key].trim();
      }
    }
    if (Object.keys(payload).length === 0) return;

    setBusy(true);
    try {
      const resp = await api.updateSettings(payload);
      onSaved(resp);
      setEnabled(resp.settings.health_enabled === "true");
      setValues(Object.fromEntries(HEALTH_FIELDS.map((f) => [f.key, resp.settings[f.key]])));
      setNotice("Run-health settings saved.");
    } catch (err) {
      setError(errorMessage(err, "Failed to save run-health settings"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Run health</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Flag runs that look slow, stuck, or looping on the board and in Slack. This is an early
          warning only — it never stops a run (RUN_TIMEOUT and the idle/iteration caps still do
          that). Set any threshold to 0 to disable that one signal.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      <form onSubmit={save} className="space-y-4">
        <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
            className="h-4 w-4 rounded border-edge accent-brand"
          />
          Enable run-health detection
        </label>

        <div className="grid gap-4 sm:grid-cols-2">
          {HEALTH_FIELDS.map((f) => {
            const err = validateHealthSeconds(values[f.key]);
            return (
              <div key={f.key} className="space-y-1">
                <Field label={f.label} htmlFor={f.key}>
                  <Input
                    id={f.key}
                    type="number"
                    min={0}
                    step={1}
                    inputMode="numeric"
                    value={values[f.key]}
                    disabled={isEnv(f.key)}
                    aria-invalid={err != null}
                    aria-describedby={err ? `${f.key}-error` : undefined}
                    onChange={(e) => setValues((v) => ({ ...v, [f.key]: e.target.value }))}
                  />
                </Field>
                {f.hint && <p className="text-xs text-faint">{f.hint}</p>}
                {err && (
                  <p id={`${f.key}-error`} className="text-xs text-warn">
                    {err}
                  </p>
                )}
              </div>
            );
          })}
        </div>

        <Button type="submit" disabled={!dirty || busy || fieldError != null}>
          {busy ? "Saving…" : "Save run health"}
        </Button>
      </form>
    </Card>
  );
}
