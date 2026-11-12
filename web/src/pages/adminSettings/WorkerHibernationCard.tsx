import { useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, SectionTitle } from "../../components/ui";

// WorkerHibernationCard is the admin surface for the worker hibernation instance switch
// (PRD #4205). A single-bool card that saves independently of the label form, mirroring
// EphemeralWorkersCard. The key round-trips through /admin/settings.
export function WorkerHibernationCard({
  settings,
  onSaved,
}: {
  settings: AppSettings;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const current = settings.worker_hibernation_enabled ?? "false";
  const [enabled, setEnabled] = useState(current === "true");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const dirty = (enabled ? "true" : "false") !== current;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    setBusy(true);
    try {
      const resp = await api.updateSettings({ worker_hibernation_enabled: String(enabled) });
      onSaved(resp);
      setEnabled(resp.settings.worker_hibernation_enabled === "true");
      setNotice("Worker hibernation settings saved.");
    } catch (err) {
      setError(errorMessage(err, "Failed to save worker hibernation settings"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Worker hibernation</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          When on, an admin can hibernate an idle persistent hosted worker: its pod is scaled down
          and its storage is kept, so it costs nothing while it sleeps. A hibernated worker takes no
          new runs and does not count as available capacity. Turning this off wakes{" "}
          <strong className="text-fg">every hibernated worker</strong> (at most 10 per pass, so a
          large fleet wakes over several passes). Waking a worker on demand never depends on this
          setting. Off by default.
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
          Allow hibernating persistent hosted workers
        </label>

        <Button type="submit" disabled={busy || !dirty}>
          {busy ? "Saving…" : "Save worker hibernation settings"}
        </Button>
      </form>
    </Card>
  );
}
