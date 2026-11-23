// Settings → Run defaults: how your runs behave before any run exists — the
// autopilot opt-in, the cross-check enables, the usage-limit park default, the run
// judge and its token binding, the Now line, automatic CI fixes, attribution, MR
// review rework and harness overflow. Split out of the old "Account & token" tab,
// which had grown to carry credentials AND run behavior in one scroll; the
// discriminator is that everything here shapes future runs, while everything on
// Account is who you are and what you hold.
//
// PRD #3263 M3: every model and effort choice (default harness, worker, planning,
// judge routing and models, summary and task-review models) is edited and saved in
// Settings → Models. This page keeps only behavior, plus a read-only summary of what
// the judge runs on and a link to change it there.

import { useState } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api } from "../lib/api";
import { UsageReserveSetting } from "../components/UsageReserveSetting";
import { WeeklyWarningMarginSetting } from "../components/WeeklyWarningMarginSetting";
import type { UserSettings, BindMode, Harness, ModelSettingCell, SecretMeta } from "../lib/apiTypes";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { Alert, Card, Field, SectionTitle, Select, Skeleton, Toggle } from "../components/ui";
import { modelSettingSourceLabel } from "../lib/modelSettings";
import { harnessName, judgeRunsOnHelp } from "../components/models/modelsLabels";
import { SettingsShell } from "../components/SettingsShell";
import { hasAnthropicToken, isCodexUsable } from "../lib/hasToken";
import { bothHarnessesUsable } from "../lib/harnessSelection";

// The read-only judge summary's routing line. An explicit Claude or Codex choice shares
// Models' copy; "Same as the run" drops Models' "Pick Claude or Codex" prompt, since
// nothing is pickable here, and points at Models instead.
function judgeSummaryHelp(choice: string): string {
  if (choice === "claude" || choice === "codex") return judgeRunsOnHelp(choice);
  return "Claude runs use the Claude judge model, Codex runs the Codex model. To judge every run on one harness, change it in Models.";
}

// The judge picker's "Auto-select from the pool" sentinel — the SAME value
// WorkersSettings.tsx uses for its worker picker, deliberately a string no
// user-authored token label can collide with. Module scope, not inside the
// component, so it is one stable <option> identity rather than a new string per render.
const AUTO_OPTION = "\u0000auto";

const MODELS_PATH = "/settings/models";

// The judge summary's model line: the server-resolved judge model for one harness and where it
// comes from. An older server without judge_models cells falls back to the session's effective
// (Claude) judge model; with neither the value is honestly unavailable, never guessed.
function judgeModelSummary(cell: ModelSettingCell | undefined, fallback: string | undefined): string {
  if (cell) return `${cell.resolved_model} · ${modelSettingSourceLabel("model", cell.source)}`;
  return fallback || "Unavailable";
}

// The judge summary's Claude credential line, from the judge lane's OWN binding (bind mode and
// pinned token) and the owned-token metadata the picker below already shows. It is never a
// prediction of the lead's credential: the judge lane is bound separately (PRD #104).
function claudeJudgeCredential(
  user: ReturnType<typeof useAuth>["user"],
  tokens: SecretMeta[],
  pooledCount: number,
  usable: boolean,
): string {
  if (!usable) return "No usable Anthropic token, so Claude judges are skipped";
  const mode = user?.judge_anthropic_bind_mode;
  if (mode === "auto") {
    return pooledCount === 0
      ? "Auto-select from the pool (empty, so your default token)"
      : "Auto-select from the pool";
  }
  if (mode === "pinned") {
    const pinned = tokens.find((s) => s.id === user?.judge_anthropic_secret_id);
    const label = pinned?.label ?? user?.judge_anthropic_secret_label;
    if (label) return `Pinned to ${label}${pinned?.enabled === false ? " (disabled)" : ""}`;
  }
  const fallback = tokens.find((s) => s.is_default)?.label;
  return fallback ? `Your default token (${fallback})` : "Your default token";
}

export function RunDefaults() {
  const { user } = useAuth();
  return <RunDefaultsEditor key={user?.id} />;
}

function RunDefaultsEditor() {
  const { user, refresh, judgeEnforcedByAdmin, effectiveJudgeModel, uziLabel } = useAuth();
  // The loaded settings: the judge summary reads the saved routing and judge model cells, and
  // harness overflow the saved default harness. The toggles below keep their own state, so
  // their saves never write back here (none of those fields is read from it).
  const [committedSettings, setCommittedSettings] = useState<UserSettings | null>(null);
  const [autopilotBusy, setAutopilotBusy] = useState(false);
  const [autopilotError, setAutopilotError] = useState("");

  const toggleAutopilot = async (enabled: boolean) => {
    setAutopilotError("");
    setAutopilotBusy(true);
    try {
      await api.setAutopilotEnabled(enabled);
      // Re-fetch the session so useAuth().user reflects the new opt-in everywhere.
      await refresh();
    } catch (err) {
      setAutopilotError(errorMessage(err, "Failed to update autopilot"));
    } finally {
      setAutopilotBusy(false);
    }
  };

  const [crossCheckBusy, setCrossCheckBusy] = useState(false);
  const [crossCheckError, setCrossCheckError] = useState("");
  const [crossCheckWarning, setCrossCheckWarning] = useState("");

  const toggleCodeCrossCheck = async (code: boolean) => {
    setCrossCheckError("");
    setCrossCheckWarning("");
    setCrossCheckBusy(true);
    try {
      const response = await api.setCodeCrossCheckEnabled(code);
      await refresh();
      setCrossCheckWarning(response.warning ?? "");
    } catch (err) {
      setCrossCheckError(errorMessage(err, "Failed to update code cross-check"));
    } finally {
      setCrossCheckBusy(false);
    }
  };

  const togglePlanCrossCheck = async (plan: boolean) => {
    setCrossCheckError("");
    setCrossCheckWarning("");
    setCrossCheckBusy(true);
    try {
      const response = await api.setPlanCrossCheckEnabled(plan);
      await refresh();
      setCrossCheckWarning(response.warning ?? "");
    } catch (err) {
      setCrossCheckError(errorMessage(err, "Failed to update plan cross-check"));
    } finally {
      setCrossCheckBusy(false);
    }
  };

  const [waitLimitBusy, setWaitLimitBusy] = useState(false);
  const [waitLimitError, setWaitLimitError] = useState("");

  // PRD #35: the per-user DEFAULT for the usage-limit park. Deliberately its own
  // busy/error pair rather than sharing autopilot's — they are independent writes to
  // independent endpoints, and one failing must not disable or blame the other.
  const toggleWaitOnLimit = async (enabled: boolean) => {
    setWaitLimitError("");
    setWaitLimitBusy(true);
    try {
      await api.setWaitOnLimit(enabled);
      // Same reason as autopilot: re-read the session so useAuth().user carries the
      // new default everywhere it is read.
      await refresh();
    } catch (err) {
      setWaitLimitError(errorMessage(err, "Failed to update the usage-limit default"));
    } finally {
      setWaitLimitBusy(false);
    }
  };

  const [earlyResetBusy, setEarlyResetBusy] = useState(false);
  const [earlyResetError, setEarlyResetError] = useState("");

  // PRD #1020: the per-user opt-in for the early-limit-reset Slack alert. Its own
  // busy/error pair rather than sharing wait-on-limit's — independent writes to
  // independent endpoints, and one failing must not disable or blame the other.
  const toggleNotifyEarlyReset = async (enabled: boolean) => {
    setEarlyResetError("");
    setEarlyResetBusy(true);
    try {
      await api.setNotifyEarlyReset(enabled);
      // Same reason as wait-on-limit: re-read the session so useAuth().user carries
      // the new default everywhere it is read.
      await refresh();
    } catch (err) {
      setEarlyResetError(errorMessage(err, "Failed to update the early-reset alert"));
    } finally {
      setEarlyResetBusy(false);
    }
  };

  const [judgeBusy, setJudgeBusy] = useState(false);
  const [judgeError, setJudgeError] = useState("");

  const toggleJudge = async (enabled: boolean) => {
    setJudgeError("");
    setJudgeBusy(true);
    try {
      // The token field is OMITTED, not sent as null: omitted leaves the judge
      // binding alone, and toggling the opt-in must never silently unbind the
      // credential the user chose (PRD #104 M4).
      await api.setJudgeEnabled(enabled);
      await refresh();
    } catch (err) {
      setJudgeError(errorMessage(err, "Failed to update run judge"));
    } finally {
      setJudgeBusy(false);
    }
  };

  // setJudgeBinding maps the picker choice to the judge lane's (mode, token): the
  // auto sentinel -> auto (pool), "" -> default, a label -> pinned. Sends the mode
  // so clearing means "your default token", never the pool (PRD #1140). Separate from
  // the opt-in above so each sends only what it changes.
  const setJudgeBinding = async (value: string) => {
    setJudgeError("");
    setJudgeBusy(true);
    try {
      const mode: BindMode = value === AUTO_OPTION ? "auto" : value === "" ? "default" : "pinned";
      await api.setJudgeEnabled(user?.judge_enabled ?? false, mode === "pinned" ? value : null, mode);
      await refresh();
    } catch (err) {
      setJudgeError(errorMessage(err, "Failed to change the judge's token"));
    } finally {
      setJudgeBusy(false);
    }
  };

  const [ciAutofixBusy, setCiAutofixBusy] = useState(false);
  const [ciAutofixError, setCiAutofixError] = useState("");

  const toggleCIAutofix = async (enabled: boolean) => {
    setCiAutofixError("");
    setCiAutofixBusy(true);
    try {
      await api.setCIAutofixEnabled(enabled);
      await refresh();
    } catch (err) {
      setCiAutofixError(errorMessage(err, "Failed to update CI autofix"));
    } finally {
      setCiAutofixBusy(false);
    }
  };

  // AI-attribution opt-out (issue #916). DEFAULT ON, unlike CI-autofix above: when on
  // (the default) the worker keeps the Co-Authored-By: Claude trailer on its commits;
  // turning it off suppresses that trailer on the user's next run. Its own busy/error
  // pair, independent of CI-autofix — separate endpoints, and one failing must not
  // disable or blame the other.
  const [attributionBusy, setAttributionBusy] = useState(false);
  const [attributionError, setAttributionError] = useState("");

  const toggleAttribution = async (enabled: boolean) => {
    setAttributionError("");
    setAttributionBusy(true);
    try {
      await api.setAttributionEnabled(enabled);
      await refresh();
    } catch (err) {
      setAttributionError(errorMessage(err, "Failed to update AI attribution"));
    } finally {
      setAttributionBusy(false);
    }
  };

  // MR review rework opt-in (PRD #700 M6). Default ON: null/absent reads as
  // enabled, only an explicit false opts the account out. Stored as the raw
  // tri-state so the checkbox reflects "!== false". Unlike autopilot/judge/CI-autofix
  // — which live on the User session and write dedicated endpoints — this is a
  // UserSettings field, so it loads from settings below and PATCHes via putMySettings.
  const [mrReworkEnabled, setMrReworkEnabled] = useState<boolean | null>(null);
  const [mrReworkBusy, setMrReworkBusy] = useState(false);
  const [mrReworkError, setMrReworkError] = useState("");

  // Persists the boolean through PUT /me/settings; a failed save leaves the
  // last-loaded value untouched.
  const toggleMrRework = async (next: boolean) => {
    setMrReworkError("");
    setMrReworkBusy(true);
    try {
      const { settings } = await api.putMySettings({ mr_rework_enabled: next });
      setMrReworkEnabled(settings.mr_rework_enabled ?? null);
    } catch (err) {
      setMrReworkError(
        errorMessage(err, "Failed to update the MR review setting"),
      );
    } finally {
      setMrReworkBusy(false);
    }
  };

  // PRD #2603: the per-user switch for the model-written Now line. A UserSettings field like
  // mr_rework_enabled: null/absent reads as ON, only an explicit false opts out.
  const [nowSummaryEnabled, setNowSummaryEnabled] = useState<boolean | null>(null);
  const [nowSummaryBusy, setNowSummaryBusy] = useState(false);
  const [nowSummaryError, setNowSummaryError] = useState("");

  const toggleNowSummary = async (next: boolean) => {
    setNowSummaryError("");
    setNowSummaryBusy(true);
    try {
      const { settings } = await api.putMySettings({ now_summary_enabled: next });
      setNowSummaryEnabled(settings.now_summary_enabled ?? null);
    } catch (err) {
      setNowSummaryError(errorMessage(err, "Failed to update the Now line setting"));
    } finally {
      setNowSummaryBusy(false);
    }
  };

  // Harness overflow (saved on its own endpoint, the moment it changes). Its default harness
  // is chosen in Models; this page only reads the saved one, because overflow applies only
  // to an explicit Claude or Codex default.
  const [overflowEnabled, setOverflowEnabled] = useState(false);
  const [overflowBusy, setOverflowBusy] = useState(false);
  const [overflowError, setOverflowError] = useState("");
  const toggleOverflow = async (enabled: boolean) => {
    setOverflowBusy(true);
    setOverflowError("");
    try {
      const result = await api.setHarnessOverflow(enabled);
      setOverflowEnabled(result.harness_overflow_enabled);
    } catch (err) {
      setOverflowError(errorMessage(err, "Failed to save harness overflow"));
    } finally {
      setOverflowBusy(false);
    }
  };
  // Harness-aware credential facts (PRD #1429 M4a, D3), computed on ALL secrets: they decide
  // which judge lanes the summary shows and what each lane's credential line says.
  const [claudeUsable, setClaudeUsable] = useState(false);
  const [codexUsable, setCodexUsable] = useState(false);

  // secrets feed the judge token picker; settings carry the behavior toggles above and the
  // read-only judge summary. secrets is read-only display, so it rides in the hook's data
  // bundle; the settings seed state the toggles also write, so they stay local.
  const { data, error: loadError } = useAsyncData(
    async () => {
      const [{ secrets: rows }, { settings }] = await Promise.all([
        api.listSecrets(),
        api.getMySettings(),
      ]);
      setCommittedSettings(settings);
      setOverflowEnabled(settings.harness_overflow_enabled);
      setMrReworkEnabled(settings.mr_rework_enabled ?? null);
      setNowSummaryEnabled(settings.now_summary_enabled ?? null);
      setClaudeUsable(hasAnthropicToken(rows));
      setCodexUsable(isCodexUsable(rows, settings.codex_account_mode));
      const aliases = rows.filter((s) => s.kind === "codex_auth");
      const pooled = settings.codex_account_mode === "auto" ? aliases.filter((s) => s.auto_eligible) : [];
      const judgeAliases = pooled.length > 0 ? pooled : aliases.filter((s) => s.is_default);
      return {
        secrets: rows.filter((s) => s.kind === "anthropic_token"),
        hasLinkedJudgeAlias: judgeAliases.some((s) => s.enabled !== false && s.codex_status === "linked"),
      };
    },
    [],
    { fallback: "Failed to load settings" },
  );
  const secrets = data?.secrets ?? [];
  // The credential facts and saved settings arrive together; until they do (or when the load
  // failed) the judge summary and overflow availability make no claim from the pre-load defaults.
  const loaded = data !== null;
  // How many of the user's tokens are opted into the auto-selection pool. A judge
  // lane in "auto" with ZERO pooled tokens spends the DEFAULT token (D4), unlike a
  // worker which holds — so the empty-pool warning below is worded for that fallback.
  // Derived rather than fetched: auto_eligible already rides SecretMeta.
  // PRD #1732 D8: effective pool membership requires enabled.
  const pooledCount = secrets.filter((s) => s.auto_eligible && s.enabled !== false).length;

  // The judge summary reads the SAVED routing and the server-resolved judge cells (user >
  // instance > built-in, per harness); both are edited in Models.
  const judgeRunsOn: "" | Harness = committedSettings?.judge_runs_on ?? "";
  const claudeJudgeCell = committedSettings?.judge_models?.find((c) => c.harness === "claude");
  const codexJudgeCell = committedSettings?.judge_models?.find((c) => c.harness === "codex");

  // Which judge lanes the summary shows: each usable harness (the Claude lane also when
  // neither is usable), plus the harness the judge explicitly runs on even without a usable
  // credential (PRD #3257), so the summary can say that judge is skipped.
  const showClaudeJudgeLane = claudeUsable || !codexUsable || judgeRunsOn === "claude";
  const showCodexJudgeLane = codexUsable || judgeRunsOn === "codex";
  const judgeOn = judgeRunsOn === "" ? null : harnessName(judgeRunsOn);
  const claudeJudgeNotUsed = judgeRunsOn === "codex";
  const codexJudgeNotUsed = judgeRunsOn === "claude";
  const judgeProviderRestricted = Boolean(committedSettings?.claude_provider_id);
  const providerRestriction = "Judge runs are skipped while a custom Claude provider is selected";
  // Alias metadata does not expose the account state the queue-time resolver checks.
  const hasCodexAlias = data?.hasLinkedJudgeAlias ?? false;
  const judgeHarnessUnusable = !judgeProviderRestricted &&
    ((judgeRunsOn === "claude" && !claudeUsable) || (judgeRunsOn === "codex" && !codexUsable && !hasCodexAlias));
  const notUsedCopy = judgeOn ? `Not used: the judge runs on ${judgeOn}` : "";

  // The enforced banner's model clause: the one model in play when the judge runs on a fixed
  // harness, both lanes when it follows the run. An older server without judge_models falls
  // back to the session's effective (Claude) model.
  const claudeJudgeResolved = claudeJudgeCell?.resolved_model ?? effectiveJudgeModel;
  let enforcedJudgeModels = "";
  if (judgeRunsOn === "codex") {
    if (codexJudgeCell) enforcedJudgeModels = ` (Codex, with ${codexJudgeCell.resolved_model})`;
  } else if (judgeRunsOn === "claude") {
    if (claudeJudgeResolved) enforcedJudgeModels = ` (Claude, with ${claudeJudgeResolved})`;
  } else if (claudeJudgeCell && codexJudgeCell) {
    enforcedJudgeModels = ` (Claude runs on ${claudeJudgeCell.resolved_model}, Codex runs on ${codexJudgeCell.resolved_model})`;
  } else if (effectiveJudgeModel) {
    enforcedJudgeModels = ` (Claude runs on ${effectiveJudgeModel})`;
  }

  // Overflow only applies to an explicit saved default (the server ignores it when automatic).
  const bothUsable = bothHarnessesUsable(claudeUsable, codexUsable);
  const overflowNeedsExplicitDefault = loaded && !committedSettings?.default_harness;
  const overflowUnavailable = !loaded && loadError !== "";

  // One summary row per lane in play: the model in effect and the credential it spends. A lane
  // the saved routing leaves unused says so in full contrast rather than fading out.
  const judgeLanes: { harness: Harness; model: string; credential: string; notUsed: boolean }[] = [];
  if (showClaudeJudgeLane) {
    judgeLanes.push({
      harness: "claude",
      model: judgeModelSummary(claudeJudgeCell, effectiveJudgeModel),
      credential: judgeProviderRestricted ? providerRestriction : claudeJudgeCredential(user, secrets, pooledCount, claudeUsable),
      notUsed: claudeJudgeNotUsed,
    });
  }
  if (showCodexJudgeLane) {
    judgeLanes.push({
      harness: "codex",
      model: judgeModelSummary(codexJudgeCell, undefined),
      // Docs/judge.md: a Codex judge re-resolves your usable Codex credential when it is
      // queued (subscription default first); there is no separate judge-lane pick.
      credential: judgeProviderRestricted
        ? providerRestriction
        : hasCodexAlias
        ? "Codex credential availability is checked when the judge is queued"
        : codexUsable
        ? "Your usable Codex credential when the judge is queued"
        : "No usable Codex credential, so Codex judges are skipped",
      notUsed: codexJudgeNotUsed,
    });
  }

  return (
    <SettingsShell description="How your runs behave: autopilot, cross-check, usage limits, the judge, summaries, CI fixes, attribution, MR rework and harness overflow. Models and reasoning effort are set in Models.">
      {loadError && <Alert message={loadError} />}

      <Card className="space-y-4">
        <div>
          <SectionTitle>Autopilot</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            With autopilot on, adding the{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">autopilot</code> label alongside{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">{uziLabel}</code> on an issue in GitLab starts a
            run <strong className="text-fg">unattended</strong>: it skips the pre-execution plan review and
            spends <strong className="text-fg">your own Anthropic tokens</strong>. The plan is still recorded
            for the audit trail, and the merge-request review stays your human gate. Attribution uses the
            forge username you set under Forge, so only issues that trace back to you can spend your tokens.
            Off by default.
          </p>
        </div>

        {autopilotError && <Alert message={autopilotError} />}

        <label className="flex items-center gap-3 text-sm">
          <input
            type="checkbox"
            className="h-4 w-4 accent-brand"
            checked={user?.autopilot_enabled ?? false}
            disabled={autopilotBusy}
            onChange={(e) => toggleAutopilot(e.target.checked)}
          />
          <span className="text-fg">Enable autopilot for my account</span>
        </label>
      </Card>

      <Card className="space-y-4">
        <div>
          <SectionTitle>Cross-check</SectionTitle>
          <p id="plan-cross-check-help" className="mt-2 text-sm text-muted">
            A second opinion from the other model family. For new auto-approved runs,
            the plan must pass before implementation. With auto-approve off, the check
            also runs before your review; you still approve the plan.
          </p>
        </div>
        {crossCheckError && <Alert message={crossCheckError} />}
        {crossCheckWarning && <Alert tone="warning" message={crossCheckWarning} />}
        <label className="flex items-center gap-3 text-sm">
          <input
            type="checkbox"
            className="h-4 w-4 accent-brand"
            aria-describedby="plan-cross-check-help"
            checked={user?.plan_cross_check_enabled ?? false}
            disabled={crossCheckBusy}
            onChange={(e) => togglePlanCrossCheck(e.target.checked)}
          />
          <span className="text-fg">Plan cross-check · Required before implementation</span>
        </label>
        <label className="flex items-center gap-3 text-sm">
          <input type="checkbox" className="h-4 w-4 accent-brand"
            aria-describedby="code-cross-check-help"
            checked={user?.code_cross_check_enabled ?? false}
            disabled={crossCheckBusy}
            onChange={(e) => toggleCodeCrossCheck(e.target.checked)} />
          <span className="text-fg">Code cross-check · Advisory before publication</span>
        </label>
        <p id="code-cross-check-help" className="text-sm text-muted">Adds the other model family's findings to the merge request; never blocks it</p>
        {/* PRD #3263 M2: the checker model and effort for each stage are saved with every other
            model choice in Models; only the enables stay here. */}
        <p className="text-sm text-muted">
          Model and effort for each cross-check stage are set in{" "}
          <Link to={MODELS_PATH} className="underline underline-offset-2">Models</Link>.
        </p>
      </Card>

      {/* PRD #35. Placed after Autopilot on purpose: the two compose, and this is the
          only place the composition is visible. An autopilot run has no start
          affordance, so for that kind — and for CI-fix and self-improve runs — this
          default is the ONLY way the opt-in can ever be expressed. */}
      <Card className="space-y-4">
        <div>
          <SectionTitle>Usage limits</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            Claude runs and Codex subscription runs can{" "}
            <strong className="text-fg">pause</strong> on a usage-window limit and resume by themselves
            when the window reopens. Recovered work and an approved plan carry forward. Runs you did
            not start by hand (autopilot, CI fixes, self-improvement) inherit this setting too. On by default.
          </p>
          <p className="mt-2 text-sm text-muted">
            This is the default for <strong className="text-fg">new</strong> runs. It does not change runs
            that already exist, including one that is paused right now — each run carries its own setting,
            which you can flip on the run's page.
          </p>
          <p className="mt-2 text-sm text-faint">
            A parked run keeps its issue lock and retains session and recovery data on the worker.
            Several parked runs cost real disk. Limits on the number of waits and the length of each
            wait bound how long a run can keep waiting.
          </p>
        </div>

        {waitLimitError && <Alert message={waitLimitError} />}

        <label className="flex items-center gap-3 text-sm">
          <input
            type="checkbox"
            className="h-4 w-4 accent-brand"
            checked={user?.wait_on_limit ?? true}
            disabled={waitLimitBusy}
            onChange={(e) => toggleWaitOnLimit(e.target.checked)}
          />
          <span className="text-fg">Pause my new runs on a usage limit instead of failing them</span>
        </label>

        {/* PRD #1020: the early-limit-reset Slack alert opt-in. On the SAME usage-limits
            surface because it is the other thing that happens around your Anthropic or
            Codex weekly window. Default ON — a field-absent user renders CHECKED, matching the DB
            default rather than assuming OFF. Independent write from the pause default
            above, so its own busy/error pair. */}
        {earlyResetError && <Alert message={earlyResetError} />}

        <label className="flex items-center gap-3 text-sm">
          <input
            type="checkbox"
            className="h-4 w-4 accent-brand"
            checked={user?.notify_early_limit_reset ?? true}
            disabled={earlyResetBusy}
            onChange={(e) => toggleNotifyEarlyReset(e.target.checked)}
          />
          <span className="text-fg">Alert me when my 7-day limit resets early</span>
        </label>
        <p className="text-sm text-faint">
          Sends a Slack DM when your weekly Anthropic or Codex limit reopens more than 8 hours
          before its expected reset. On by default; needs a linked Slack account to reach you.
        </p>

        {/* PRD #3522: the weekly usage reserve. The third thing on this card that is
            about your 7-day window; its own component and endpoint, so its busy/error
            state never touches the two toggles above. */}
        <UsageReserveSetting />

        {/* PRD #4123: the early-warning margin, counted back from the reserve above (or
            100%). Its own endpoint and state; hidden until the settings read says what is
            stored, and on a server that predates the setting. */}
        {committedSettings?.weekly_warning_margin_pts !== undefined && (
          <WeeklyWarningMarginSetting
            saved={committedSettings.weekly_warning_margin_pts}
            onSaved={(marginPts) =>
              setCommittedSettings((current) => current && { ...current, weekly_warning_margin_pts: marginPts })
            }
          />
        )}
      </Card>

      <Card className="space-y-4">
        <div>
          <SectionTitle>Run judge</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            With the run judge on, each of your <strong className="text-fg">finished</strong> runs is
            reviewed by an LLM using <strong className="text-fg">your own credential for the harness it runs on</strong>,
            shown below: your Anthropic token on Claude, your Codex credential on Codex. It reads
            the run trace and produces a verdict plus recommendations (a missing worker tool, an agent or
            template to improve, and so on). Results appear on the Judge page and on the run&rsquo;s page,
            plus a Slack DM when your Slack account is linked. It only recommends and never changes code. Your
            instance admin also has to enable the feature globally for anything to run. Off by default.
          </p>
        </div>

        {/* Enforced-mode banner (PRD #69 M4, PRD #3245, PRD #3257). When the admin turns on
            enforce-all, the judge runs on EVERY finished run regardless of the opt-in
            below, still on the user's own credential for the harness the judge runs on — so
            the honest opt-out is removing that credential, not this toggle. It names the
            resolved judge model(s) in play; an older server without judge_models falls back
            to the Claude lane's effective_judge_model. */}
        {judgeEnforcedByAdmin && (
          <Alert
            tone="warning"
            message={
              `Your admin has ENFORCED the run judge: every one of your finished runs is judged on ` +
              `your own credential for the harness the judge runs on` +
              enforcedJudgeModels +
              `, whether or not the opt-in below is on. If the judge runs on Claude or Codex for every run ` +
              `and that harness has no usable credential, the judge is skipped. The only way to opt out is ` +
              `to remove your credential for that harness.`
            }
          />
        )}

        {judgeError && <Alert message={judgeError} />}

        {/* In enforced mode the per-user opt-in is bypassed at enqueue (Decision 3), so
            this toggle is INERT — grey and disable it (matching AdminUsers.tsx), rather
            than leave a live control that contradicts the banner above it. */}
        <label
          className={`flex items-center gap-3 text-sm${judgeEnforcedByAdmin ? " opacity-60" : ""}`}
        >
          <input
            type="checkbox"
            className="h-4 w-4 accent-brand"
            checked={user?.judge_enabled ?? false}
            disabled={judgeBusy || judgeEnforcedByAdmin}
            onChange={(e) => toggleJudge(e.target.checked)}
          />
          <span className="text-fg">Judge my finished runs</span>
        </label>


        {/* PRD #3263 M3: what the judge runs on, read-only. Routing and the judge models are
            edited and saved with every other model choice in Models; the credential lines come
            from the judge lane's own binding below (Claude) and the owned Codex credentials,
            never from a prediction of the lead's credential. */}
        <section
          aria-labelledby="judge-summary-title"
          className="space-y-3 rounded-lg border border-edge bg-raised/40 p-4"
        >
          <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
            <h3 id="judge-summary-title" className="text-sm font-semibold text-fg">
              Where your judge runs
            </h3>
            <Link to={MODELS_PATH} className="text-sm font-medium text-brand hover:text-brand-hover">
              Change in Models
            </Link>
          </div>
          {!loaded && !loadError && (
            <div className="space-y-2" aria-busy="true">
              <span className="sr-only">Loading where your judge runs</span>
              <Skeleton className="h-4 w-full max-w-sm" />
              <Skeleton className="h-4 w-full max-w-xs" />
              <Skeleton className="h-4 w-full max-w-sm" />
            </div>
          )}
          {!loaded && loadError && (
            <p className="text-sm text-muted">Unavailable until your settings load. Reload the page to try again.</p>
          )}
          {loaded && (
            <dl className="grid gap-x-6 gap-y-0.5 text-sm sm:grid-cols-[minmax(9rem,auto)_1fr] sm:gap-y-2">
              <div className="contents">
                <dt className="text-muted">Runs on</dt>
                <dd className="mb-2 text-fg sm:mb-0">
                  {judgeOn ?? "Same as the run"}
                  <span className="mt-0.5 block text-xs text-faint">{judgeSummaryHelp(judgeRunsOn)}</span>
                </dd>
              </div>
              {/* One wrapper per dt/dd pair: a <dl> may group only a name and its values. */}
              {judgeLanes.map((lane) => [
                <div key={`${lane.harness}-model`} className="contents">
                  <dt className="text-muted">{harnessName(lane.harness)} judge model</dt>
                  <dd className="mb-2 text-fg sm:mb-0">
                    <span className="break-words">{lane.model}</span>
                    {lane.notUsed && <span className="mt-0.5 block text-xs text-muted">{notUsedCopy}</span>}
                  </dd>
                </div>,
                <div key={`${lane.harness}-credential`} className="contents">
                  <dt className="text-muted">{harnessName(lane.harness)} judge credential</dt>
                  <dd className="mb-2 text-fg sm:mb-0">{lane.credential}</dd>
                </div>,
              ])}
            </dl>
          )}
          {loaded && judgeOn && judgeHarnessUnusable && (
            <p id="judge-runs-on-unusable" className="text-xs text-warn" role="status">
              {judgeOn} has no usable credential, so the judge is skipped on every run until you add one
              in <Link to="/settings" className="underline underline-offset-2">Account &amp; tokens</Link>.
              Your choice stays saved.
            </p>
          )}
        </section>


        {/* The judge token picker (PRD #104 M4/M6, #1140 M3). Without it "the judge
            lane can burn a different token, set from the web UI" is unreachable, which
            is why it is required and not a nicety. Shown with one OR more tokens: with
            a single token the pool and the default coincide, but the MODE is now worth
            showing since auto is the default. Hidden only at zero tokens. */}
        {secrets.length >= 1 && (
          <Field label="Token the judge spends">
            {/* PRD #3257: when every judge runs on Codex this Anthropic token is never spent,
                so the picker is dimmed (still operable, so the binding can be set ahead of a
                switch back) and says why, in full contrast, beneath it. */}
            <div className={claudeJudgeNotUsed ? "opacity-45" : undefined}>
              <Select
                aria-label="Token the judge spends"
                aria-describedby={claudeJudgeNotUsed ? "judge-token-not-used" : undefined}
                // Driven by the MODE first (mirroring the worker picker): an auto judge
                // lane selects the sentinel; a pinned one selects its token's label; and
                // a pinned binding whose token was deleted arrives here as effective mode
                // "default" with a null pointer — so it shows the default-token option.
                // The label is resolved from `secrets` by id BEFORE the DTO's own label:
                // GET /api/auth/me never fills judge_anthropic_secret_label (only the
                // PUT /me/judge response does, handler.go toDTO), so on a fresh page load a
                // pinned lane would otherwise render as "Use my default token", and the
                // next picker change would silently replace the pin.
                value={
                  user?.judge_anthropic_bind_mode === "auto"
                    ? AUTO_OPTION
                    : user?.judge_anthropic_bind_mode === "pinned"
                      ? (secrets.find((s) => s.id === user.judge_anthropic_secret_id)?.label ??
                        user.judge_anthropic_secret_label ??
                        "")
                      : ""
                }
                disabled={judgeBusy}
                onChange={(e) => setJudgeBinding(e.target.value)}
              >
                {/* Copy mirrors the worker picker (WorkersSettings.tsx, F15): the two
                    account-level choices read as sentences, and a token NAMED "default"
                    shows "(your default)" so "default (default)" never appears. */}
                <option value="">Use my default token</option>
                <option value={AUTO_OPTION}>Auto-select from the pool</option>
                <optgroup label="Pin to a token">
                  {/* PRD #1732 D5: a disabled token cannot be newly bound, so it is not
                      offered; the one the judge is ALREADY pinned to stays visible and
                      marked (D2 keeps the binding; retrospectives wait on it). */}
                  {secrets
                    .filter((s) => s.enabled !== false || s.id === user?.judge_anthropic_secret_id)
                    .map((s) => (
                      <option key={s.id} value={s.label} disabled={s.enabled === false}>
                        {s.label}
                        {s.is_default ? " (your default)" : ""}
                        {s.enabled === false ? " (disabled)" : ""}
                      </option>
                    ))}
                </optgroup>
              </Select>
            </div>
            {claudeJudgeNotUsed && (
              <p id="judge-token-not-used" className="mt-1.5 text-xs text-muted">
                {notUsedCopy}
              </p>
            )}
            <p className="mt-1.5 text-xs text-faint">
              Used whenever the judge runs on Claude. Retrospectives can bill a different account from
              the runs they review — point them at a cheaper console key while your runs stay on a
              subscription.
            </p>
            {/* Empty-pool warning, D4 wording: unlike a worker (which holds), an auto
                judge lane with an empty pool spends the default token. role=status /
                aria-live mirrors the worker picker's announce() so a screen-reader user
                who selects Auto with an empty pool hears the fallback. */}
            {user?.judge_anthropic_bind_mode === "auto" && pooledCount === 0 && (
              <p className="mt-1.5 text-xs text-warn" role="status" aria-live="polite">
                Your pool is empty, so retrospectives spend your default token until you add a token to the pool.
              </p>
            )}
          </Field>
        )}
        <p className="border-t border-edge pt-3 text-xs text-faint">
          If the judge&rsquo;s harness can&rsquo;t run, the judge is skipped, never charged to the other
          provider.
        </p>
      </Card>

      {/* Run summaries (PRD #362, #3248, #2603). The summary model for each harness is set in
          Models; the Now line switch is behavior, so it stays here. */}
      <Card className="space-y-4">
        <div>
          <SectionTitle>Run summaries</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            Each of your runs generates two short plain-English summaries — what it will implement, and
            what the proposed plan will do — on the same harness the run uses, with your own credential
            for that harness. Summaries are advisory and never block a run.
          </p>
          <p className="mt-2 text-sm text-muted">
            The summary model for each harness is set in{" "}
            <Link to={MODELS_PATH} className="underline underline-offset-2">Models</Link>.
          </p>
        </div>

        <div className="space-y-2">
          {nowSummaryError && <Alert message={nowSummaryError} />}
          <label className="flex items-center gap-3 text-sm">
            <input
              type="checkbox"
              className="h-4 w-4 accent-brand"
              checked={nowSummaryEnabled !== false}
              disabled={nowSummaryBusy || !committedSettings}
              aria-describedby="now-summary-help"
              onChange={(e) => toggleNowSummary(e.target.checked)}
            />
            <span className="text-fg">Model-written Now line</span>
          </label>
          <p id="now-summary-help" className="text-xs text-faint">
            A small model summarises the current step of a running run, on your own credential. On by
            default; your admin can also turn it off for the whole instance.
          </p>
        </div>
      </Card>

      {/* Task review (PRD #1570): only a model and effort, both edited in Models, so the card
          is a pointer for anyone who looks for it here. */}
      <Card className="space-y-2">
        <SectionTitle>Task review</SectionTitle>
        <p className="text-sm text-muted">
          The model and effort the optional <code>uzi handoff --review</code> pass uses are set in{" "}
          <Link to={MODELS_PATH} className="underline underline-offset-2">Models</Link>.
        </p>
      </Card>


      <Card className="space-y-4">
        <div>
          <SectionTitle>Automatic CI fixes</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            When a pipeline fails on one of your agent merge-request branches, uzi spends your{" "}
            <strong className="text-fg">own Anthropic tokens</strong> to attempt a fix automatically.
            On by default. Opting out stops autofix on{" "}
            <strong className="text-fg">your</strong> MRs — the admin kill-switch that turns the
            feature off for the whole instance is separate.
          </p>
        </div>

        {ciAutofixError && <Alert message={ciAutofixError} />}

        <label className="flex items-center gap-3 text-sm">
          <input
            type="checkbox"
            className="h-4 w-4 accent-brand"
            checked={user?.ci_autofix_enabled !== false}
            disabled={ciAutofixBusy}
            onChange={(e) => toggleCIAutofix(e.target.checked)}
          />
          <span className="text-fg">Automatically fix my failed CI pipelines</span>
        </label>
      </Card>

      {/* AI attribution in commits (issue #916). Placed beside Automatic CI fixes, but
          DEFAULT ON (opt-out) rather than default-off — turning it off suppresses the
          Co-Authored-By: Claude trailer on the worker's commits. This affects only the
          commit trailer; uzi's merge-request bodies are worker-built and already carry
          no AI attribution, so the copy must not claim it changes MR descriptions. */}
      <Card className="space-y-4">
        <div>
          <SectionTitle>AI attribution in commits</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            Every commit uzi's worker makes includes a{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">Co-Authored-By: Claude</code>{" "}
            trailer. Turn this off if your organization's policy requires no AI attribution in git
            history. On by default; the change takes effect on your next run. (Merge-request
            descriptions are unaffected — they already carry no AI attribution.)
          </p>
        </div>

        {attributionError && <Alert message={attributionError} />}

        <label className="flex items-center gap-3 text-sm">
          <input
            type="checkbox"
            className="h-4 w-4 accent-brand"
            checked={user?.attribution_enabled ?? true}
            disabled={attributionBusy}
            onChange={(e) => toggleAttribution(e.target.checked)}
          />
          <span className="text-fg">Include the Co-Authored-By: Claude trailer in my worker commits</span>
        </label>
      </Card>

      {/* MR review rework opt-in (PRD #700 M6). A run-behavior default, so it lives
          here beside autopilot/judge/CI-autofix rather than on Account & tokens. */}
      <Card className="space-y-4">
        <div>
          <SectionTitle>MR review rework</SectionTitle>
          <p id="mr-rework-help" className="mt-2 text-sm text-muted">
            When a completed run's merge request gets new review comments on a green pipeline, uzi
            can automatically rework the branch to address them, then reply to each thread, and
            resolve it on forges that support resolvable threads (GitLab, GitHub). On by default.
            Opting out stops the watcher from auto-reworking{" "}
            <strong className="text-fg">your</strong> MRs — the admin kill-switch that turns the
            feature off for the whole instance is separate.
          </p>
        </div>

        {mrReworkError && <Alert message={mrReworkError} />}

        <label className="flex items-center gap-3 text-sm">
          <input
            type="checkbox"
            className="h-4 w-4 accent-brand"
            checked={mrReworkEnabled !== false}
            disabled={mrReworkBusy}
            aria-describedby="mr-rework-help"
            onChange={(e) => toggleMrRework(e.target.checked)}
          />
          <span className="text-fg">Auto-rework MR review comments on my runs</span>
        </label>
      </Card>

      {/* Harness overflow (saved on its own endpoint). Behavior, so it stays here when the
          default harness and worker models it builds on moved to Models (PRD #3263 M3). */}
      <Card className="space-y-4">
        <div>
          <SectionTitle>Harness overflow</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            When your default harness is near its usage limits, a new run can start on the other
            harness instead. Your default harness and worker models are set in{" "}
            <Link to={MODELS_PATH} className="underline underline-offset-2">Models</Link>.
          </p>
        </div>

        <div className="space-y-1.5">
          <label className="flex items-center gap-3 text-sm text-fg">
            <Toggle
              checked={overflowEnabled}
              disabled={overflowBusy || !loaded || overflowNeedsExplicitDefault}
              focusableWhenDisabled
              onChange={enabled => void toggleOverflow(enabled)}
              label="Overflow to the other harness when my default is near its limits"
              aria-describedby={
                overflowNeedsExplicitDefault
                  ? "harness-overflow-scope harness-overflow-automatic"
                  : overflowUnavailable
                    ? "harness-overflow-scope harness-overflow-unavailable"
                    : !loaded
                      ? "harness-overflow-scope harness-overflow-loading"
                      : "harness-overflow-scope"
              }
            />
            Overflow to the other harness when my default is near its limits
          </label>
          {!loaded && !loadError && (
            <div aria-busy="true">
              {/* Named by the toggle's aria-describedby: it is disabled until the load
                  settles, and this says why (as the judge summary's loading row does). */}
              <span id="harness-overflow-loading" className="sr-only">Loading your saved default harness</span>
              <Skeleton className="h-3.5 w-full max-w-md" />
            </div>
          )}
          {overflowUnavailable && (
            <p id="harness-overflow-unavailable" className="text-xs text-faint">
              Unavailable until your settings load. Reload the page to try again.
            </p>
          )}
          {overflowNeedsExplicitDefault && (
            <p id="harness-overflow-automatic" className="text-xs text-faint">
              Overflow applies only when a Claude or Codex default harness is saved in Models.
              {bothUsable && " With Default · automatic, uzi already picks by weekly limits."}
            </p>
          )}
          <p id="harness-overflow-scope" className="text-xs text-faint">
            Saves automatically. Applies only to new issue runs started manually, by autopilot
            or by a schedule, when both harnesses are usable. Explicit harnesses, run model pins,
            Anthropic credential overrides and enabled plan or code cross-checks exclude overflow.
            Existing runs and resumes never switch. Chat and other run kinds do not use overflow.
            Stored usage and capacity holds are compared at creation time; unknown comparison
            scores keep the preferred harness.
          </p>
          {overflowError && <Alert message={overflowError} />}
        </div>
      </Card>
    </SettingsShell>
  );
}
