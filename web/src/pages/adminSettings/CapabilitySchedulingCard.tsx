import { useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingSource, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, SectionTitle } from "../../components/ui";

// CapabilitySchedulingCard is the admin kill-switch for capability-aware scheduling
// (PRD #84 M2, Decision 13). Default ON. It follows the bool-default-true toggle
// precedent (health_enabled) and sends only capability_aware_scheduling on change.
// Turning it OFF is an explicit, documented degraded mode: runs claim best-effort and
// a capability mismatch degrades to the existing mid-run failure. It does NOT disable
// the docker repo allowlist (that stays enforced regardless of this flag).
export function CapabilitySchedulingCard({
  settings,
  sources,
  onSaved,
}: {
  settings: AppSettings;
  sources: Record<string, SettingSource>;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [enabled, setEnabled] = useState(settings.capability_aware_scheduling === "true");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const isEnv = sources["capability_aware_scheduling"] === "env";

  const dirty = (enabled ? "true" : "false") !== settings.capability_aware_scheduling;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    if (isEnv || !dirty) return;
    setBusy(true);
    try {
      const resp = await api.updateSettings({
        capability_aware_scheduling: enabled ? "true" : "false",
      });
      onSaved(resp);
      setEnabled(resp.settings.capability_aware_scheduling === "true");
      setNotice("Capability-aware scheduling setting saved.");
    } catch (err) {
      setError(errorMessage(err, "Failed to save the capability scheduling setting"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Capability-aware scheduling</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Capability-aware scheduling routes each run to a worker that can run it (e.g. a
          Docker-needing run only to a Docker worker). Turning this OFF reverts to best-effort
          claiming: a run may be claimed by a worker that lacks a required capability and fail
          mid-run. This does NOT disable the Docker repo allowlist.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}
      {isEnv && (
        <Alert tone="info" message="This setting is fixed by an environment variable and cannot be changed here." />
      )}

      <form onSubmit={save} className="space-y-4">
        <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={enabled}
            disabled={isEnv}
            onChange={(e) => setEnabled(e.target.checked)}
            className="h-4 w-4 rounded border-edge accent-brand"
          />
          Enable capability-aware scheduling
        </label>

        <Button type="submit" disabled={!dirty || busy || isEnv}>
          {busy ? "Saving…" : "Save capability scheduling"}
        </Button>
      </form>
    </Card>
  );
}
