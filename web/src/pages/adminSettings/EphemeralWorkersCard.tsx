import { useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, SectionTitle } from "../../components/ui";

// EphemeralWorkersCard is the admin surface for the ephemeral worker
// auto-provisioning instance kill-switch (PRD #529 / #649 M1). A single-bool card
// that saves independently of the label form, mirroring JudgeSettingsCard. It is an
// INSTANCE kill-switch: when off, no run ever auto-provisions a throwaway hosted
// worker regardless of a user's per-account opt-in; when on, users can still opt in
// individually on the Workers page. Web-only — the key already round-trips through
// /admin/settings.
export function EphemeralWorkersCard({
  settings,
  onSaved,
}: {
  settings: AppSettings;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [enabled, setEnabled] = useState(settings.ephemeral_workers_enabled === "true");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const dirty = (enabled ? "true" : "false") !== settings.ephemeral_workers_enabled;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    setBusy(true);
    try {
      const resp = await api.updateSettings({ ephemeral_workers_enabled: String(enabled) });
      onSaved(resp);
      setEnabled(resp.settings.ephemeral_workers_enabled === "true");
      setNotice("Ephemeral workers settings saved.");
    } catch (err) {
      setError(errorMessage(err, "Failed to save ephemeral workers settings"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Ephemeral workers</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          When on, a queued run that needs a capability no online worker has can auto-provision a
          run-bound, throwaway hosted worker on demand, reaped when the run finishes. This switch is the
          instance <strong className="text-fg">kill-switch</strong>: while it is off, no worker is ever
          auto-provisioned regardless of any per-user opt-in. With it on, users can still individually opt
          in per account on the Workers page. Off by default.
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
          Auto-provision workers on demand for this instance
        </label>

        <Button type="submit" disabled={busy || !dirty}>
          {busy ? "Saving…" : "Save ephemeral workers settings"}
        </Button>
      </form>
    </Card>
  );
}
