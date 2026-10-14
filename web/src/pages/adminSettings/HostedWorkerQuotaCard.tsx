import { useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, Field, Input, SectionTitle } from "../../components/ui";

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

export function HostedWorkerQuotaCard({ settings, onSaved }: {
  settings: AppSettings;
  onSaved: (resp: SettingsResponse) => void;
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
      const resp = await api.updateSettings({ hosted_worker_quota: value });
      onSaved(resp);
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
    </Card>
  );
}
