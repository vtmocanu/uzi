import { useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingSource, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { ModelSelect } from "../../components/ModelSelect";
import { Alert, Button, Card, Field, Input, SectionTitle } from "../../components/ui";
import { CODEX_ROLE_MODELS } from "../../lib/codexModels";

type JudgeKey = "judge_model" | "codex_judge_model";

// pinnedModel is the instance's own pin for one judge lane: "" when it follows the
// built-in (source "default"), else the stored value. The select's empty option is the
// reset, so an unpinned instance shows Default selected, not the resolved model.
function pinnedModel(key: JudgeKey, settings: AppSettings, sources: Record<string, SettingSource>): string {
  return sources[key] === "default" ? "" : (settings[key] ?? "");
}

// defaultOptionLabel names the built-in the reset follows. With no defaults from the
// server (an older API) it stays neutral rather than guessing a model (PRD #3245 D8).
function defaultOptionLabel(builtin: string | undefined): string {
  return builtin ? `Default \u00b7 ${builtin} (built-in)` : "Default (built-in)";
}

const CUSTOM_EMPTY_MESSAGE = "Enter a model ID, or choose Default.";

// JudgeSettingsCard is the admin surface for the run judge (PRD #46): the global
// on/off kill-switch plus the cheap model the judge runs on. It saves independently
// of the label form (like the Slack card). A user still has to opt IN under their
// own Settings, and the judge always spends that user's tokens — this card only
// arms the feature instance-wide.
export function JudgeSettingsCard({
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
  const [enabled, setEnabled] = useState(settings.judge_enabled === "true");
  const pinned = pinnedModel("judge_model", settings, sources);
  const [model, setModel] = useState(pinned);
  const [modelCustom, setModelCustom] = useState(false);
  const codexPinned = pinnedModel("codex_judge_model", settings, sources);
  const [codexModel, setCodexModel] = useState(codexPinned);
  const [codexCustom, setCodexCustom] = useState(false);
  const [enforceAll, setEnforceAll] = useState(settings.judge_enforce_all === "true");
  const [cooldown, setCooldown] = useState(settings.judge_cooldown_seconds);
  const [budget, setBudget] = useState(settings.judge_daily_budget);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const dirty =
    (enabled ? "true" : "false") !== settings.judge_enabled ||
    model !== pinned ||
    codexModel !== codexPinned ||
    (enforceAll ? "true" : "false") !== settings.judge_enforce_all ||
    cooldown !== settings.judge_cooldown_seconds ||
    budget !== settings.judge_daily_budget;

  // "Other (custom model ID)" with an empty box would otherwise send "" (the reset) while
  // the picker still shows Other: refuse to save until the ID is filled or Default chosen.
  const claudeCustomEmpty = modelCustom && model.trim() === "";
  const codexCustomEmpty = codexCustom && codexModel.trim() === "";
  const blocked = claudeCustomEmpty || codexCustomEmpty;
  // PRD #3245 legacy warning: the stored instance Claude value is a Codex id, which
  // Claude judges skip in favour of the built-in. Shown until the admin changes it.
  const legacyCodexInClaude = pinned !== "" && model === pinned && CODEX_ROLE_MODELS.includes(pinned.trim());

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (blocked) return;
    setError("");
    setNotice("");
    setBusy(true);
    try {
      const resp = await api.updateSettings({
        judge_enabled: enabled ? "true" : "false",
        // Sent only when changed; "" is the reset to the built-in model.
        ...(model !== pinned ? { judge_model: model } : {}),
        ...(codexModel !== codexPinned ? { codex_judge_model: codexModel } : {}),
        // The kill-switch dominates: enforce-all is meaningless with the judge off, so
        // never send it as on when the feature is disabled (mirrors the Gate-2-wins
        // server semantics the /me consent surface also reflects).
        judge_enforce_all: enabled && enforceAll ? "true" : "false",
        judge_cooldown_seconds: cooldown.trim(),
        judge_daily_budget: budget.trim(),
      });
      onSaved(resp);
      setEnabled(resp.settings.judge_enabled === "true");
      setModel(pinnedModel("judge_model", resp.settings, resp.sources));
      setCodexModel(pinnedModel("codex_judge_model", resp.settings, resp.sources));
      setEnforceAll(resp.settings.judge_enforce_all === "true");
      setCooldown(resp.settings.judge_cooldown_seconds);
      setBudget(resp.settings.judge_daily_budget);
      setNotice("Run judge settings saved.");
    } catch (err) {
      setError(errorMessage(err, "Failed to save run judge settings"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Run judge</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          When on, every finished run of an opted-in user is reviewed by an LLM on{" "}
          <strong className="text-fg">that user&rsquo;s own credential for the run&rsquo;s provider</strong>{" "}
          (Anthropic or Codex), producing a verdict
          and recommendations on the Judge page and the run&rsquo;s page, plus a Slack DM when that user has
          linked Slack. This switch arms the feature instance-wide; each user still opts in under their own
          Settings. Off by default.
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
          Enable the run judge for this instance
        </label>

        {/* PRD #3245: one judge model per harness, side by side (Claude | Codex). A
            judge runs on the harness of the run it reviews, so each lane is its own
            instance default. */}
        <div className="space-y-2">
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Field label="Claude judge model" htmlFor="judge-model-admin">
                <ModelSelect
                  id="judge-model-admin"
                  harness="claude"
                  value={model}
                  onChange={setModel}
                  onCustomModeChange={setModelCustom}
                  defaultLabel={defaultOptionLabel(defaults.judge_model)}
                  customAriaLabel="Custom Claude judge model ID"
                  ariaDescribedBy="judge-model-admin-state"
                />
              </Field>
              <p id="judge-model-admin-state" className="text-xs text-faint">
                {model === "" ? "Follows the built-in model." : "Pinned. Choose Default to follow the built-in again."}
              </p>
              {legacyCodexInClaude && (
                <p className="text-xs text-warn" role="status">
                  Not a Claude model: Claude judges use the built-in {defaults.judge_model ?? "model"}. Choose
                  Default to clear it.
                </p>
              )}
              {claudeCustomEmpty && (
                <p className="text-xs text-danger" role="alert">
                  {CUSTOM_EMPTY_MESSAGE}
                </p>
              )}
            </div>
            <div className="space-y-1.5">
              <Field label="Codex judge model" htmlFor="codex-judge-model-admin">
                <ModelSelect
                  id="codex-judge-model-admin"
                  harness="codex"
                  value={codexModel}
                  onChange={setCodexModel}
                  onCustomModeChange={setCodexCustom}
                  defaultLabel={defaultOptionLabel(defaults.codex_judge_model)}
                  customAriaLabel="Custom Codex judge model ID"
                  ariaDescribedBy="codex-judge-model-admin-state"
                />
              </Field>
              <p id="codex-judge-model-admin-state" className="text-xs text-faint">
                {codexModel === "" ? "Follows the built-in model." : "Pinned. Choose Default to follow the built-in again."}
              </p>
              {codexCustomEmpty && (
                <p className="text-xs text-danger" role="alert">
                  {CUSTOM_EMPTY_MESSAGE}
                </p>
              )}
            </div>
          </div>
          <p className="text-xs text-faint">
            The judge runs on the harness of the run it reviews, so each harness has its own judge model.
            Default follows uzi&rsquo;s built-in model, including future changes to it; the built-ins are the
            strongest models because judge recommendations feed self-improvement. Pin a cheaper model, such
            as <code className="rounded bg-raised px-1 py-0.5 text-fg">sonnet</code> for Claude, to spend less.
            A user&rsquo;s own choice under Settings &rarr; Run defaults wins over these.
          </p>
        </div>

        {/* Enforce-all (PRD #69 M4). Greyed when the kill-switch is off: the kill-switch
            dominates, so an enforce toggle over a disabled judge would be a lie. */}
        <div className="space-y-1.5">
          <label
            className={`flex select-none items-center gap-2 text-sm ${
              enabled ? "cursor-pointer" : "cursor-not-allowed opacity-50"
            }`}
          >
            <input
              type="checkbox"
              checked={enabled && enforceAll}
              disabled={!enabled}
              onChange={(e) => setEnforceAll(e.target.checked)}
              className="h-4 w-4 rounded border-edge accent-brand"
            />
            Enforce the judge on every run (no per-user opt-in)
          </label>
          <p className="text-xs text-faint">
            With this on, EVERY user&rsquo;s finished runs are judged — bypassing their per-user opt-in — and
            each is spent on <strong className="text-fg">that user&rsquo;s own credential for the run&rsquo;s
            harness (Anthropic or Codex) without their opt-in</strong>. A user with no usable credential for
            that harness is skipped. On a subscription plan it also eats their rate-limit quota. Pin a
            cheaper judge model (the built-in default is the strongest) before enforcing. The per-user judge toggle on the Users page becomes inert
            while this is on.
          </p>
        </div>

        <div className="space-y-1.5">
          <Field label="Per-user cooldown (seconds)">
            <Input
              type="number"
              inputMode="numeric"
              min={0}
              value={cooldown}
              autoComplete="off"
              placeholder="60"
              onChange={(e) => setCooldown(e.target.value)}
            />
          </Field>
          <p className="text-xs text-faint">
            Minimum gap between one user&rsquo;s judge runs. <code className="rounded bg-raised px-1 py-0.5 text-fg">0</code>{" "}
            turns the cooldown off; otherwise a value from 60 up to 86400 seconds (24 hours).
          </p>
        </div>

        <div className="space-y-1.5">
          <Field label="Per-user daily budget (runs)">
            <Input
              type="number"
              inputMode="numeric"
              min={0}
              value={budget}
              autoComplete="off"
              placeholder="0"
              onChange={(e) => setBudget(e.target.value)}
            />
          </Field>
          <p className="text-xs text-faint">
            Most judge runs one user can spend per day. <code className="rounded bg-raised px-1 py-0.5 text-fg">0</code>{" "}
            means unlimited; otherwise a positive count.
          </p>
        </div>

        <Button type="submit" disabled={busy || !dirty || blocked}>
          {busy ? "Saving…" : "Save run judge settings"}
        </Button>
      </form>
    </Card>
  );
}
