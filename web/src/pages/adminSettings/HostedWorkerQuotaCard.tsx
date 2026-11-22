import { useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingSource, type SettingsResponse, type UpdateSettingsPayload } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, Field, Input, SectionTitle } from "../../components/ui";
import { SettingControl } from "./SettingControl";
import type { NamedWorkerSize } from "../../lib/workerSizes";

// Mirrors validateHostedWorkerQuota in settings_hosted.go; the API rechecks every write.
function quotaError(value: string): string | null {
  if (!/^[+-]?\d+$/.test(value.trim())) return "Must be a whole number of workers";
  const n = Number(value.trim());
  return n < 0 || n > 20 ? "Must be 0 (self-service disabled) or between 1 and 20 workers" : null;
}

// The server stores accepted strings verbatim, but numeric inputs cannot display
// a leading plus or surrounding whitespace. Normalize valid saved values only.
function displayQuota(value: string): string {
  return quotaError(value) === null ? String(Number(value.trim())) : value;
}

export function HostedWorkerQuotaCard({ settings, sources = {}, defaults, sizeOptions, onSaved }: {
  settings: AppSettings;
  sources?: Record<string, SettingSource>;
  defaults?: Record<string, string>;
  /** The worker sizes the default-size selector offers (PRD #4213), lifted from Admin → Workers. */
  sizeOptions?: readonly NamedWorkerSize[];
  onSaved: (resp: SettingsResponse, sent: UpdateSettingsPayload) => void;
}) {
  const savedValue = displayQuota(settings.hosted_worker_quota);
  const [value, setValue] = useState(savedValue);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const dirty = value !== savedValue;
  const invalid = quotaError(value);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (busy || !dirty || invalid) return;
    setError("");
    setNotice("");
    setBusy(true);
    try {
      const payload: UpdateSettingsPayload = { hosted_worker_quota: value };
      const resp = await api.updateSettings(payload);
      onSaved(resp, payload);
      setValue(displayQuota(resp.settings.hosted_worker_quota));
      setNotice("Hosted worker quota saved.");
    } catch (err) {
      setError(errorMessage(err, "Failed to save hosted worker quota"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Hosted worker quota</SectionTitle>
        <p id="hosted-quota-description" className="mt-2 text-sm text-muted">
          Maximum persistent hosted workers each user may provision, from 0 to 20. Every size counts as one. Zero disables self-service provisioning without changing existing workers. Ephemeral workers have a separate cap.
        </p>
      </div>
      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}
      <form onSubmit={save} className="space-y-4" noValidate>
        <Field label="Hosted workers per user" htmlFor="hosted-worker-quota-input">
          <Input
            id="hosted-worker-quota-input"
            type="number"
            min={0}
            max={20}
            step={1}
            value={value}
            aria-invalid={invalid !== null}
            aria-describedby={`hosted-quota-description${invalid ? " hosted-quota-error" : ""}`}
            onChange={(e) => { setValue(e.target.value); setError(""); setNotice(""); }}
          />
        </Field>
        {invalid && <p id="hosted-quota-error" className="text-xs text-danger">{invalid}</p>}
        <Button type="submit" disabled={busy || !dirty || invalid !== null}>
          {busy ? "Saving…" : "Save hosted worker quota"}
        </Button>
      </form>
      <SettingControl
        settingKey="hosted_default_size"
        label="Default size for new workers"
        kind="size"
        helper="Size the provision form preselects for a new worker; a request still names its size. Existing workers keep their size."
        value={settings.hosted_default_size ?? ""}
        source={sources["hosted_default_size"]}
        defaultValue={defaults?.["hosted_default_size"]}
        sizeOptions={sizeOptions}
        onSaved={onSaved}
      />
    </Card>
  );
}
