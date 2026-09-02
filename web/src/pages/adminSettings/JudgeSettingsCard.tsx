import { useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, Field, Input, SectionTitle } from "../../components/ui";

// JudgeSettingsCard is the admin surface for the run judge (PRD #46): the global
// on/off kill-switch plus the cheap model the judge runs on. It saves independently
// of the label form (like the Slack card). A user still has to opt IN under their
// own Settings, and the judge always spends that user's tokens — this card only
// arms the feature instance-wide.
export function JudgeSettingsCard({
  settings,
  onSaved,
}: {
  settings: AppSettings;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [enabled, setEnabled] = useState(settings.judge_enabled === "true");
  const [model, setModel] = useState(settings.judge_model);
  const [enforceAll, setEnforceAll] = useState(settings.judge_enforce_all === "true");
  const [cooldown, setCooldown] = useState(settings.judge_cooldown_seconds);
  const [budget, setBudget] = useState(settings.judge_daily_budget);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const dirty =
    (enabled ? "true" : "false") !== settings.judge_enabled ||
    model !== settings.judge_model ||
    (enforceAll ? "true" : "false") !== settings.judge_enforce_all ||
    cooldown !== settings.judge_cooldown_seconds ||
    budget !== settings.judge_daily_budget;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    if (model.trim() === "") {
      setError("The judge model must not be empty.");
      return;
    }
    setBusy(true);
    try {
      const resp = await api.updateSettings({
        judge_enabled: enabled ? "true" : "false",
        judge_model: model,
        // The kill-switch dominates: enforce-all is meaningless with the judge off, so
        // never send it as on when the feature is disabled (mirrors the Gate-2-wins
        // server semantics the /me consent surface also reflects).
        judge_enforce_all: enabled && enforceAll ? "true" : "false",
        judge_cooldown_seconds: cooldown.trim(),
        judge_daily_budget: budget.trim(),
      });
      onSaved(resp);
      setEnabled(resp.settings.judge_enabled === "true");
      setModel(resp.settings.judge_model);
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
          and recommendations in their inbox. This switch arms the feature instance-wide; each user still
          opts in under their own Settings. Off by default.
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
          <Field label="Judge model">
            <Input
              value={model}
              maxLength={100}
              autoComplete="off"
              placeholder="opus"
              onChange={(e) => setModel(e.target.value)}
            />
          </Field>
          <p className="text-xs text-faint">
            The Claude model the judge runs on. The default is{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">opus</code> — the strongest model, since
            judge recommendations feed self-improvement. Admins and users can pin{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">haiku</code> or{" "}
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
            opt-in</strong>. On a subscription plan it also eats their rate-limit quota. Pin{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">judge_model</code> to a cheaper model
            (opus is the default) before enforcing. The per-user judge toggle on the Users page becomes inert
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
