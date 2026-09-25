import { useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingSource, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, SectionTitle } from "../../components/ui";

// CompletionInterlockCard is the admin kill-switch for the completion interlock
// (issue #1626, PRD #1226). Default ON. Before an issue run opens a PR that closes its
// issue, the interlock checks every milestone of the approved plan is marked done.
// It applies to Claude runs only; Codex runs are not checked yet. Sends only
// completion_interlock_rollout on change, following the CapabilitySchedulingCard
// bool-default-true precedent.
export function CompletionInterlockCard({
  settings,
  sources,
  onSaved,
}: {
  settings: AppSettings;
  sources: Record<string, SettingSource>;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [enabled, setEnabled] = useState(settings.completion_interlock_rollout === "true");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const isEnv = sources["completion_interlock_rollout"] === "env";

  const dirty = (enabled ? "true" : "false") !== settings.completion_interlock_rollout;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    if (isEnv || !dirty) return;
    setBusy(true);
    try {
      const resp = await api.updateSettings({
        completion_interlock_rollout: enabled ? "true" : "false",
      });
      onSaved(resp);
      setEnabled(resp.settings.completion_interlock_rollout === "true");
      setNotice("Completion check setting saved.");
    } catch (err) {
      setError(errorMessage(err, "Failed to save the completion check setting"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Completion check</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Before an issue run opens a pull request that closes its issue, uzi checks that the agent
          has marked every milestone of the approved plan done. If some are missing, the agent is
          told which ones and keeps working. If it still does not finish them, the run pauses for
          its owner to continue it, ship only some milestones without closing the issue, or accept
          specific criteria as met. The check itself uses no model, though the extra work it
          triggers does. Applies to Claude runs; Codex runs are not checked yet. Turn off only if
          it blocks runs you trust.
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
          Enable the completion check
        </label>

        <Button type="submit" disabled={!dirty || busy || isEnv}>
          {busy ? "Saving…" : "Save completion check"}
        </Button>
      </form>
    </Card>
  );
}
