import { useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingSource, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, SectionTitle } from "../../components/ui";
import { DocLink } from "../../components/DocLink";
import { DOC_GITHUB_PROJECT_SYNC } from "../../lib/doclinks";

// GithubProjectSyncCard is the instance-wide kill-switch for GitHub Projects v2 sync
// (issue #534 M2). Default OFF — it initializes from the served value and never
// hard-codes true, because the feature is a rate-limit / cost lever that ships off
// until an admin arms it. When on, each run mirrors a card's board-column label onto a
// linked GitHub Projects Status field (GitHub-only; GitLab/Forgejo are untouched). It
// saves independently, sending only github_project_sync_enabled on change, and greys
// when the value is fixed by an environment variable (the server rejects a write, 409).
export function GithubProjectSyncCard({
  settings,
  sources,
  onSaved,
}: {
  settings: AppSettings;
  sources: Record<string, SettingSource>;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [enabled, setEnabled] = useState(settings.github_project_sync_enabled === "true");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const isEnv = sources["github_project_sync_enabled"] === "env";

  const dirty = (enabled ? "true" : "false") !== settings.github_project_sync_enabled;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    if (isEnv || !dirty) return;
    setBusy(true);
    try {
      const resp = await api.updateSettings({
        github_project_sync_enabled: enabled ? "true" : "false",
      });
      onSaved(resp);
      setEnabled(resp.settings.github_project_sync_enabled === "true");
      setNotice("GitHub Projects sync setting saved.");
    } catch (err) {
      setError(errorMessage(err, "Failed to save the GitHub Projects sync setting"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>GitHub Projects sync</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          When on, each run mirrors a board card&rsquo;s column label onto a linked GitHub
          Projects v2 Status field, so a team that prefers GitHub&rsquo;s native board stays in
          step. It is off by default because it spends GitHub API rate limit on every board
          move — an instance-wide cost lever. GitLab and Forgejo repos are unaffected. See the{" "}
          <DocLink slug={DOC_GITHUB_PROJECT_SYNC}>GitHub Projects v2 sync</DocLink> guide.
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
          Enable GitHub Projects sync
        </label>

        <Button type="submit" disabled={!dirty || busy || isEnv}>
          {busy ? "Saving…" : "Save GitHub Projects sync"}
        </Button>
      </form>
    </Card>
  );
}
