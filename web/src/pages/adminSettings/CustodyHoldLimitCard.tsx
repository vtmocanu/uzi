import { useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, Field, Input, SectionTitle } from "../../components/ui";

// Mirrors validateCustodyHoldLimit in settings_custody.go; the API rechecks every write.
const LIMIT_MESSAGE = "must be a whole number between 1 and 1000";

function limitError(value: string): string | null {
  const t = value.trim();
  if (!/^[0-9]+$/.test(t)) return LIMIT_MESSAGE;
  const n = Number(t);
  return n < 1 || n > 1000 ? LIMIT_MESSAGE : null;
}

// Saved values are stored verbatim; show the trimmed form so the number input can render it.
function displayLimit(value: string): string {
  return limitError(value) === null ? String(Number(value.trim())) : value;
}

export function CustodyHoldLimitCard({ settings, onSaved }: {
  settings: AppSettings;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const savedValue = displayLimit(settings.custody_hold_limit);
  const [value, setValue] = useState(savedValue);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const dirty = value !== savedValue;
  const invalid = limitError(value);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (busy || !dirty || invalid) return;
    setError("");
    setNotice("");
    setBusy(true);
    try {
      const resp = await api.updateSettings({ custody_hold_limit: value });
      onSaved(resp);
      setValue(displayLimit(resp.settings.custody_hold_limit));
      setNotice("Custody hold limit saved.");
    } catch (err) {
      setError(errorMessage(err, "Failed to save custody hold limit"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Custody hold limit</SectionTitle>
        <p id="custody-limit-description" className="mt-2 text-sm text-muted">
          Per-owner limit on open recovery holds that count toward admission, from 1 to 1000 (default 8). Once an owner reaches it, their new code runs wait. Raising it admits more work. Lowering it pauses new runs only; it never releases holds or cancels runs. One value applies to the whole deployment, to each owner separately.
        </p>
      </div>
      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}
      <form onSubmit={save} className="space-y-4" noValidate>
        <Field label="Recovery holds per owner" htmlFor="custody-hold-limit-input">
          <Input
            id="custody-hold-limit-input"
            type="number"
            min={1}
            max={1000}
            step={1}
            value={value}
            aria-invalid={invalid !== null}
            aria-describedby={`custody-limit-description${invalid ? " custody-limit-error" : ""}`}
            onChange={(e) => { setValue(e.target.value); setError(""); setNotice(""); }}
          />
        </Field>
        {invalid && <p id="custody-limit-error" className="text-xs text-danger">{invalid}</p>}
        <Button type="submit" disabled={busy || !dirty || invalid !== null}>
          {busy ? "Saving…" : "Save custody hold limit"}
        </Button>
      </form>
    </Card>
  );
}
