import { useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingSource, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { ModelSelect } from "../../components/ModelSelect";
import { modelFieldWarning } from "../../lib/agentTemplates";
import { CODEX_ROLE_MODELS } from "../../lib/codexModels";
import { Alert, Button, Card, Field, SectionTitle } from "../../components/ui";

type SummaryKey = "summary_model" | "codex_summary_model";

// pinnedModel is the instance's own pin for one summary lane: "" when it follows the
// built-in (source "default"), else the stored value. The select's empty option is the
// reset, so an unpinned instance shows Default selected, not the resolved model.
function pinnedModel(key: SummaryKey, settings: AppSettings, sources: Record<string, SettingSource>): string {
  return sources[key] === "default" ? "" : (settings[key] ?? "");
}

// defaultOptionLabel names the built-in the reset follows. With no defaults from the
// server (an older API) it stays neutral rather than guessing a model.
function defaultOptionLabel(builtin: string | undefined): string {
  return builtin ? `Default · ${builtin} (built-in)` : "Default (built-in)";
}

const CUSTOM_EMPTY_MESSAGE = "Enter a model ID, or choose Default.";

// SummarySettingsCard is the admin surface for the run-summary model (PRD #362
// Decision 8, PRD #3248): the cheap model the inline plain-English run-summary
// generator runs on, one per harness (Claude | Codex). Each lane is an instance
// default with a Default option that follows the built-in; a per-user choice under
// Run defaults wins where set. The values ride the ISSUE-run claim. Saved
// independently through the same settings PUT, sending only the changed keys.
export function SummarySettingsCard({
  settings,
  sources,
  defaults,
  onSaved,
}: {
  settings: AppSettings;
  sources: Record<string, SettingSource>;
  // The compiled-in defaults from the admin GET, for the "Default · <model> (built-in)" label.
  defaults: Record<string, string>;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const pinned = pinnedModel("summary_model", settings, sources);
  const [model, setModel] = useState(pinned);
  const [modelCustom, setModelCustom] = useState(false);
  const codexPinned = pinnedModel("codex_summary_model", settings, sources);
  const [codexModel, setCodexModel] = useState(codexPinned);
  const [codexCustom, setCodexCustom] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  // PRD #2603: instance kill-switch for the model-written "Now" line. Absent (an older
  // server) reads as on, the server default.
  const savedNow = settings.now_summary_enabled !== "false";
  const [nowEnabled, setNowEnabled] = useState(savedNow);

  const modelDirty = model !== pinned;
  const codexDirty = codexModel !== codexPinned;
  const nowDirty = nowEnabled !== savedNow;
  const dirty = modelDirty || codexDirty || nowDirty;

  // "Other (custom model ID)" with an empty box would otherwise send "" (the reset) while
  // the picker still shows Other: refuse to save until the ID is filled or Default chosen.
  const claudeCustomEmpty = modelCustom && model.trim() === "";
  const codexCustomEmpty = codexCustom && codexModel.trim() === "";
  const claudeWarning = modelFieldWarning(model);
  const codexWarning = modelFieldWarning(codexModel);
  // The Claude lane refuses a Codex role id (the server does too); a Codex id belongs in
  // the Codex picker.
  const claudeIsCodexId = CODEX_ROLE_MODELS.includes(model.trim());
  // A malformed ID blocks only when it is the change being saved: an untouched stored
  // value is never sent, so it must not lock the other controls.
  const blocked =
    claudeCustomEmpty ||
    codexCustomEmpty ||
    (modelDirty && (claudeWarning !== "" || claudeIsCodexId)) ||
    (codexDirty && codexWarning !== "");
  // A stored instance Claude value that is a Codex id is skipped by Claude summaries in
  // favour of the built-in. Shown until the admin changes it.
  const legacyCodexInClaude = !modelDirty && claudeIsCodexId;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (blocked) return;
    setError("");
    setNotice("");
    setBusy(true);
    try {
      // Only the changed keys, so saving one control never rewrites the others. "" is the
      // reset to the built-in model.
      const resp = await api.updateSettings({
        ...(modelDirty ? { summary_model: model } : {}),
        ...(codexDirty ? { codex_summary_model: codexModel } : {}),
        ...(nowDirty ? { now_summary_enabled: String(nowEnabled) } : {}),
      });
      onSaved(resp);
      setModel(pinnedModel("summary_model", resp.settings, resp.sources));
      setCodexModel(pinnedModel("codex_summary_model", resp.settings, resp.sources));
      setNowEnabled(resp.settings.now_summary_enabled !== "false");
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
          <strong className="text-fg">the run owner&rsquo;s own credential for the run&rsquo;s harness</strong>{" "}
          (Anthropic or Codex). These set the instance-default models; a user can override them under their
          own Settings. Summaries are advisory and never block a run.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      <form onSubmit={save} className="space-y-4">
        <div className="space-y-2">
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Field label="Claude summary model" htmlFor="summary-model-admin">
                <ModelSelect
                  id="summary-model-admin"
                  harness="claude"
                  value={model}
                  onChange={setModel}
                  onCustomModeChange={setModelCustom}
                  defaultLabel={defaultOptionLabel(defaults.summary_model)}
                  customAriaLabel="Custom Claude summary model ID"
                  ariaDescribedBy="summary-model-admin-state"
                />
              </Field>
              <p id="summary-model-admin-state" className="text-xs text-faint">
                {model === "" ? "Follows the built-in model." : "Pinned. Choose Default to follow the built-in again."}
              </p>
              {legacyCodexInClaude && (
                <p className="text-xs text-warn" role="status">
                  Not a Claude model: Claude summaries use the built-in {defaults.summary_model ?? "model"}.
                  Choose Default to clear it.
                </p>
              )}
              {modelDirty && claudeIsCodexId && (
                <p className="text-xs text-danger" role="alert">
                  That is a Codex model. Choose it under Codex summary model instead.
                </p>
              )}
              {claudeCustomEmpty && (
                <p className="text-xs text-danger" role="alert">
                  {CUSTOM_EMPTY_MESSAGE}
                </p>
              )}
              {claudeWarning && <Alert tone="warning" message={claudeWarning} />}
            </div>
            <div className="space-y-1.5">
              <Field label="Codex summary model" htmlFor="codex-summary-model-admin">
                <ModelSelect
                  id="codex-summary-model-admin"
                  harness="codex"
                  models={CODEX_ROLE_MODELS}
                  value={codexModel}
                  onChange={setCodexModel}
                  onCustomModeChange={setCodexCustom}
                  defaultLabel={defaultOptionLabel(defaults.codex_summary_model)}
                  customAriaLabel="Custom Codex summary model ID"
                  ariaDescribedBy="codex-summary-model-admin-state"
                />
              </Field>
              <p id="codex-summary-model-admin-state" className="text-xs text-faint">
                {codexModel === "" ? "Follows the built-in model." : "Pinned. Choose Default to follow the built-in again."}
              </p>
              {codexCustomEmpty && (
                <p className="text-xs text-danger" role="alert">
                  {CUSTOM_EMPTY_MESSAGE}
                </p>
              )}
              {codexWarning && <Alert tone="warning" message={codexWarning} />}
            </div>
          </div>
          <p className="text-xs text-faint">
            A summary runs on the harness of the run it describes, so each harness has its own summary model.
            Default follows uzi&rsquo;s built-in model, including future changes to it. The built-ins are fast
            and near-free, since summaries are light and produced per run; pin{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">sonnet</code> or{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">opus</code> for Claude summaries for
            richer output. A user&rsquo;s own choice under Settings &rarr; Run defaults wins over these.
          </p>
        </div>

        <div className="space-y-1.5">
          <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={nowEnabled}
              onChange={(e) => setNowEnabled(e.target.checked)}
              className="h-4 w-4 rounded border-edge accent-brand"
            />
            Model-written Now line
          </label>
          <p className="text-xs text-faint">
            Instance kill-switch. While a run works, a small model writes a one-sentence summary of its
            current step on the progress card, on the run owner&rsquo;s own credential. Off stops it for
            every user, whatever their own setting. On by default.
          </p>
        </div>

        <Button type="submit" disabled={busy || !dirty || blocked}>
          {busy ? "Saving…" : "Save run summary settings"}
        </Button>
      </form>
    </Card>
  );
}
