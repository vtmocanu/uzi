import { useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingSource, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, Field, Input, SectionTitle } from "../../components/ui";

type JudgeKey = "judge_model" | "codex_judge_model";

// instanceModelText renders one lane of the read-only "Instance default" line. A pinned lane
// (source other than "default") shows its stored value, marked when the environment set it;
// an unpinned lane names the built-in it follows, or stays neutral when the server sent none.
function instanceModelText(
  key: JudgeKey,
  settings: AppSettings,
  sources: Record<string, SettingSource>,
  defaults: Record<string, string>,
): string {
  if (sources[key] !== "default") {
    const value = settings[key] ?? "";
    return sources[key] === "env" ? `${value} (set from the environment)` : value;
  }
  const builtin = defaults[key];
  return builtin ? `Default · ${builtin} (built-in)` : "Default (built-in)";
}

// JudgeSettingsCard is the admin surface for the run judge policy (PRD #46): the global
// on/off kill-switch, enforce-all, the per-user cooldown and the daily budget. It saves
// independently of the label form (like the Slack card). A user still has to opt IN under
// their own Settings, and the judge always spends that user's tokens: this card only arms
// the feature instance-wide.
//
// The judge models moved to InstanceModelDefaultsCard (#3259), the one grid for the
// per-harness judge and summary models. This card sends no model key; it only shows the
// current instance defaults read-only, derived from its props on every render so it
// follows a save made in that grid, and links to it.
export function JudgeSettingsCard({
  settings,
  sources,
  defaults,
  onSaved,
}: {
  settings: AppSettings;
  sources: Record<string, SettingSource>;
  // The compiled-in defaults from the admin GET, for the read-only "Default · <model> (built-in)" text.
  defaults: Record<string, string>;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [enabled, setEnabled] = useState(settings.judge_enabled === "true");
  const [enforceAll, setEnforceAll] = useState(settings.judge_enforce_all === "true");
  const [cooldown, setCooldown] = useState(settings.judge_cooldown_seconds);
  const [budget, setBudget] = useState(settings.judge_daily_budget);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const dirty =
    (enabled ? "true" : "false") !== settings.judge_enabled ||
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
        // The kill-switch dominates: enforce-all is meaningless with the judge off, so
        // never send it as on when the feature is disabled (mirrors the Gate-2-wins
        // server semantics the /me consent surface also reflects).
        judge_enforce_all: enabled && enforceAll ? "true" : "false",
        judge_cooldown_seconds: cooldown.trim(),
        judge_daily_budget: budget.trim(),
      });
      onSaved(resp);
      setEnabled(resp.settings.judge_enabled === "true");
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
          <strong className="text-fg">that user&rsquo;s own credential</strong> for the harness their
          &ldquo;Judge runs on&rdquo; choice picks, by default the run&rsquo;s own, producing a verdict
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

        <p className="text-xs text-faint" data-testid="judge-instance-default">
          Instance default: {instanceModelText("judge_model", settings, sources, defaults)} (Claude) &middot;{" "}
          {instanceModelText("codex_judge_model", settings, sources, defaults)} (Codex) &middot; users may
          override. <a href="#model-defaults">Model defaults</a>
        </p>

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
            each is spent on <strong className="text-fg">that user&rsquo;s own Claude or Codex credential,
            following their &ldquo;Judge runs on&rdquo; choice, without their opt-in</strong>. A user with no
            usable credential for that harness is skipped, with a reason shown on the run, and is never charged
            to the other provider. On a subscription plan it also eats their rate-limit quota. Consider a
            cheaper judge model in <a href="#model-defaults">Model defaults</a> before enforcing. The per-user
            judge toggle on the Users page becomes inert while this is on.
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
