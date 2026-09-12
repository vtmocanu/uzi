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

// Each health field carries its OWN validator (PRD #1170): the four thresholds are in
// seconds (validateHealthSeconds), while health_near_timeout_pct is a percent of the
// run's wall-clock budget (validateHealthPercent) — the field types diverged when the
// wall-clock "slow" timer became a budget-relative "near timeout" flag.
const HEALTH_FIELDS: {
  key: keyof AppSettings;
  label: string;
  hint?: string;
  validate: (value: string) => string | null;
}[] = [
  {
    key: "health_stall_seconds",
    label: "Stalled after (seconds of silence)",
    hint: "No new activity while no tool call is in flight.",
    validate: validateHealthSeconds,
  },
  {
    key: "health_near_timeout_pct",
    label: "Near timeout at (% of wall-clock budget)",
    hint: "Active running time, excluding time parked at a human gate, as a share of RUN_TIMEOUT or the run's frozen budget. 0 disables.",
    validate: validateHealthPercent,
  },
  {
    // PRD #1189: the per-run wall-clock extension allowance. It lives in this card (below the
    // near-timeout percent) because it governs the SAME near-timeout moment, but it is NOT a
    // health threshold — its bounds are {0} ∪ [3600, 604800], so it carries its own validator.
    key: "run_extension_cap_seconds",
    label: "Extension allowance per run (seconds)",
    hint: "Total extra wall-clock time an owner may grant a run through Extend; 57600 = 16h. 0 turns extending off.",
    validate: validateExtensionCapSeconds,
  },
  { key: "health_queued_seconds", label: "Stuck queued after (seconds)", validate: validateHealthSeconds },
  { key: "health_approval_seconds", label: "Awaiting approval after (seconds)", validate: validateHealthSeconds },
  { key: "health_nudge_cooldown_seconds", label: "Slack nudge cooldown (seconds)", validate: validateHealthSeconds },
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

// validateHealthPercent mirrors the server's write-time rule (PRD #1170) for immediate
// feedback: a base-10 integer that is 0 (disable) or in [50, 99]. The floor keeps an
// operator from recreating this flag's old noise with a low threshold; 100 is excluded
// because the sweeper fires the timeout at 100%. The message matches the server's; the
// server stays the source of truth.
function validateHealthPercent(value: string): string | null {
  const v = value.trim();
  const msg = "must be 0 (disabled) or between 50 and 99 percent";
  if (!/^-?\d+$/.test(v)) return msg;
  const n = Number(v);
  if (n === 0) return null;
  if (n < 50 || n > 99) return msg;
  return null;
}

// validateExtensionCapSeconds mirrors the server's validateExtensionCapSeconds (PRD #1189):
// 0 (disable) or a whole number in the inclusive range [3600, 604800] (1h to 7d). The bounds
// differ from the health-seconds fields, so this validator is distinct; the message matches
// the server's, which stays the source of truth. Digit-only for strconv.Atoi parity.
function validateExtensionCapSeconds(value: string): string | null {
  const v = value.trim();
  const msg = "must be 0 (disabled) or between 3600 and 604800 seconds";
  if (!/^\d+$/.test(v)) return msg;
  const n = Number(v);
  if (n === 0) return null;
  if (n < 3600 || n > 604800) return msg;
  return null;
}

// HealthSettingsCard is the admin surface for the run-health detector (PRD #47, #1170):
// an enable toggle plus five thresholds — four in seconds and one (near timeout) a
// percent of the run's wall-clock budget. It saves independently of
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
    // `?? ""` guards a mid-deploy api pod that predates a key (e.g. run_extension_cap_seconds):
    // an undefined value would make the input uncontrolled; empty reads honestly and the field's
    // validator then flags it.
    Object.fromEntries(HEALTH_FIELDS.map((f) => [f.key, settings[f.key] ?? ""])),
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const isEnv = (key: string) => sources[key] === "env";

  const fieldError = HEALTH_FIELDS.map((f) => f.validate(values[f.key])).find(Boolean) ?? null;

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
      setValues(Object.fromEntries(HEALTH_FIELDS.map((f) => [f.key, resp.settings[f.key] ?? ""])));
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
          Flag runs that look stuck, looping, or close to their timeout on the board and in Slack. This is an early
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
            const err = f.validate(values[f.key]);
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
