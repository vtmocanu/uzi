import { useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingSource, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { ModelSelect } from "../../components/ModelSelect";
import { Alert, Button, Card, Field, Input, SectionTitle } from "../../components/ui";

// pinnedJudgeModel is the instance's own pin: "" when the judge model follows the
// built-in (source "default"), else the stored value. The select's empty option is
// the reset, so an unpinned instance shows Default selected, not the resolved alias.
function pinnedJudgeModel(settings: AppSettings, sources: Record<string, SettingSource>): string {
  return sources.judge_model === "default" ? "" : settings.judge_model;
}

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
  const pinned = pinnedJudgeModel(settings, sources);
  const [model, setModel] = useState(pinned);
  const [enforceAll, setEnforceAll] = useState(settings.judge_enforce_all === "true");
  const [cooldown, setCooldown] = useState(settings.judge_cooldown_seconds);
  const [budget, setBudget] = useState(settings.judge_daily_budget);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const dirty =
    (enabled ? "true" : "false") !== settings.judge_enabled ||
    model !== pinned ||
    (enforceAll ? "true" : "false") !== settings.judge_enforce_all ||
    cooldown !== settings.judge_cooldown_seconds ||
    budget !== settings.judge_daily_budget;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    setBusy(true);
    try {
      const resp = await api.updateSettings({
        judge_enabled: enabled ? "true" : "false",
        // Sent only when changed; "" is the reset to the built-in model.
        ...(model !== pinned ? { judge_model: model } : {}),
        // The kill-switch dominates: enforce-all is meaningless with the judge off, so
        // never send it as on when the feature is disabled (mirrors the Gate-2-wins
        // server semantics the /me consent surface also reflects).
        judge_enforce_all: enabled && enforceAll ? "true" : "false",
        judge_cooldown_seconds: cooldown.trim(),
        judge_daily_budget: budget.trim(),
      });
      onSaved(resp);
      setEnabled(resp.settings.judge_enabled === "true");
      setModel(pinnedJudgeModel(resp.settings, resp.sources));
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
          <strong className="text-fg">that user&rsquo;s own Anthropic tokens</strong>, producing a verdict
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

        <div className="space-y-1.5">
          <Field label="Judge model" htmlFor="judge-model-admin">
            <ModelSelect
              id="judge-model-admin"
              value={model}
              onChange={setModel}
              defaultLabel={`Default · ${defaults.judge_model ?? "opus"} (built-in)`}
              customAriaLabel="Custom judge model ID"
            />
          </Field>
          <p className="text-xs text-faint">
            The Claude model the judge runs on. Choose Default to follow the built-in model, which is the
            strongest one since judge recommendations feed self-improvement and changes with future
            releases. Pin <code className="rounded bg-raised px-1 py-0.5 text-fg">haiku</code> or{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">sonnet</code> to spend less.
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
            each is spent on <strong className="text-fg">that user&rsquo;s own Anthropic token without their
            opt-in</strong>. On a subscription plan it also eats their rate-limit quota. Pin a
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

        <Button type="submit" disabled={busy || !dirty}>
          {busy ? "Saving…" : "Save run judge settings"}
        </Button>
      </form>
    </Card>
  );
}
