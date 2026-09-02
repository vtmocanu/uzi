import { useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, Field, Input, SectionTitle } from "../../components/ui";

// SummarySettingsCard is the admin surface for the run-summary model (PRD #362
// Decision 8): the cheap model the inline plain-English run-summary generator runs
// on. It mirrors the Judge model field exactly — a raw free-text model alias input —
// but the value rides the ISSUE-run claim, not the judge claim, and a per-user
// override wins where set. Saved independently through the same settings PUT.
export function SummarySettingsCard({
  settings,
  onSaved,
}: {
  settings: AppSettings;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [model, setModel] = useState(settings.summary_model);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const dirty = model !== settings.summary_model;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    if (model.trim() === "") {
      setError("The summary model must not be empty.");
      return;
    }
    setBusy(true);
    try {
      const resp = await api.updateSettings({ summary_model: model });
      onSaved(resp);
      setModel(resp.settings.summary_model);
      setNotice("Run summary settings saved.");
    } catch (err) {
      setError(errorMessage(err, "Failed to save run summary settings"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Run summaries</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Each run generates two short plain-English summaries — what it will implement, and what the
          proposed plan will do — on{" "}
          <strong className="text-fg">the run owner&rsquo;s own Anthropic token</strong>. This sets the
          instance-default model; a user can override it under their own Settings. Summaries are advisory
          and never block a run.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      <form onSubmit={save} className="space-y-4">
        <div className="space-y-1.5">
          <Field label="Summary model">
            <Input
              value={model}
              maxLength={100}
              autoComplete="off"
              placeholder="haiku"
              onChange={(e) => setModel(e.target.value)}
            />
          </Field>
          <p className="text-xs text-faint">
            The Claude model the run-summary generator runs on. The default is{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">haiku</code> — fast and near-free, since
            summaries are light and produced per run. Pin{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">sonnet</code> or{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">opus</code> for richer summaries.
          </p>
        </div>

        <Button type="submit" disabled={busy || !dirty}>
          {busy ? "Saving…" : "Save run summary settings"}
        </Button>
      </form>
    </Card>
  );
}
