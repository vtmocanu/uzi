import { useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingSource, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, SectionTitle } from "../../components/ui";
import { SettingControl } from "./SettingControl";
import type { NamedWorkerSize } from "../../lib/workerSizes";

// EphemeralWorkersCard is the admin surface for the ephemeral worker
// auto-provisioning instance kill-switch (PRD #529 / #649 M1). A single-bool card
// that saves independently of the label form, mirroring JudgeSettingsCard. It is an
// INSTANCE kill-switch: when off, no run ever auto-provisions a throwaway hosted
// worker regardless of a user's per-account opt-in; when on, users can still opt in
// individually on the Workers page. Below the switch are the per-key tuning controls
// (PRD #4212): per-user cap, default size and idle lease, each saved and reset on its own.
// Shown on Admin → Workers; the keys round-trip through the /admin/settings API.
export function EphemeralWorkersCard({
  settings,
  sources = {},
  defaults,
  sizeOptions,
  onSaved,
}: {
  settings: AppSettings;
  sources?: Record<string, SettingSource>;
  defaults?: Record<string, string>;
  /** The worker sizes the default-size selector offers (PRD #4213), lifted from Admin → Workers. */
  sizeOptions?: readonly NamedWorkerSize[];
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
          throwaway hosted worker on demand. It is removed once the run finishes, or, when the
          run qualifies for a lease, once that lease ends: during the lease it stays idle so a
          follow-up run on the same repository and branch can reuse it. The idle lease length is an
          admin setting below, default 2h; 0s disables leasing. An environment or chart value
          (<code className="font-mono text-fg">UZI_EPHEMERAL_LEASE</code>, <code className="font-mono text-fg">workers.ephemeralLease</code>)
          locks it. This switch is the
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

      <SettingControl
        settingKey="ephemeral_max_per_user"
        label="Per-user cap"
        kind="number"
        min={0}
        max={20}
        helper="Most ephemeral workers one user can have at once, from 0 to 20."
        value={settings.ephemeral_max_per_user ?? ""}
        source={sources["ephemeral_max_per_user"]}
        defaultValue={defaults?.["ephemeral_max_per_user"]}
        onSaved={onSaved}
      />
      <SettingControl
        settingKey="ephemeral_default_size"
        label="Ephemeral default size"
        kind="size"
        helper="Size every auto-provisioned ephemeral worker starts at."
        value={settings.ephemeral_default_size ?? ""}
        source={sources["ephemeral_default_size"]}
        defaultValue={defaults?.["ephemeral_default_size"]}
        sizeOptions={sizeOptions}
        onSaved={onSaved}
      />
      <SettingControl
        settingKey="ephemeral_lease"
        label="Idle lease"
        kind="text"
        helper="A Go duration such as 30m or 2h. 0s disables; maximum 2h."
        value={settings.ephemeral_lease ?? ""}
        source={sources["ephemeral_lease"]}
        defaultValue={defaults?.["ephemeral_lease"]}
        onSaved={onSaved}
      />
    </Card>
  );
}
