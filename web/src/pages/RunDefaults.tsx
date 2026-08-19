// Settings → Run defaults: how your runs behave before any run exists — the
// autopilot opt-in, the usage-limit park default, the run judge and its token
// binding, automatic CI fixes, and the worker model. Split out of the old
// "Account & token" tab, which had grown to carry credentials AND run behavior
// in one scroll; the discriminator is that everything here shapes future runs,
// while everything on Account is who you are and what you hold.

import { useCallback, useEffect, useState } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, type SecretMeta } from "../lib/api";
import { Alert, Button, Card, Field, SectionTitle, Select, Skeleton } from "../components/ui";
import { ModelSelect } from "../components/ModelSelect";
import { modelFieldWarning } from "../lib/agentTemplates";
import { SettingsShell } from "../components/SettingsShell";

export function RunDefaults() {
  const { user, refresh, judgeEnforcedByAdmin, effectiveJudgeModel } = useAuth();
  const [secrets, setSecrets] = useState<SecretMeta[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
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
      setAutopilotError(err instanceof ApiError ? err.message : "Failed to update autopilot");
    } finally {
      setAutopilotBusy(false);
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
      setWaitLimitError(err instanceof ApiError ? err.message : "Failed to update the usage-limit default");
    } finally {
      setWaitLimitBusy(false);
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
      setJudgeError(err instanceof ApiError ? err.message : "Failed to update run judge");
    } finally {
      setJudgeBusy(false);
    }
  };

  // setJudgeToken points the JUDGE lane at one of the user's tokens, or clears it
  // back to the default. Separate from the opt-in above so each sends only what it
  // changes — the whole point of the three-way token field.
  const setJudgeToken = async (label: string) => {
    setJudgeError("");
    setJudgeBusy(true);
    try {
      await api.setJudgeEnabled(user?.judge_enabled ?? false, label === "" ? null : label);
      await refresh();
    } catch (err) {
      setJudgeError(err instanceof ApiError ? err.message : "Failed to change the judge's token");
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
      setCiAutofixError(err instanceof ApiError ? err.message : "Failed to update CI autofix");
    } finally {
      setCiAutofixBusy(false);
    }
  };

  // Worker model: "" = inherit. savedModel is the persisted value, so Save is
  // only offered when the picker differs from what is stored.
  const [defaultModel, setDefaultModel] = useState("");
  const [savedModel, setSavedModel] = useState("");
  const [modelBusy, setModelBusy] = useState(false);

  // Per-user judge model (PRD #69 M2): "" = inherit the instance judge_model. Its own
  // saved/busy pair, independent of the worker model above — they write the same
  // /me/settings endpoint but each sends only its own field.
  const [judgeModel, setJudgeModel] = useState("");
  const [savedJudgeModel, setSavedJudgeModel] = useState("");
  const [judgeModelBusy, setJudgeModelBusy] = useState(false);

  // Per-user run-summary model (PRD #362 M2): "" = inherit the instance summary_model.
  // Its own saved/busy pair, independent of the judge and worker models above — they
  // write the same /me/settings endpoint but each sends only its own field.
  const [summaryModel, setSummaryModel] = useState("");
  const [savedSummaryModel, setSavedSummaryModel] = useState("");
  const [summaryModelBusy, setSummaryModelBusy] = useState(false);

  // secrets feed the judge token picker; settings carry the saved worker model.
  const load = useCallback(async () => {
    try {
      const [{ secrets: rows }, { settings }] = await Promise.all([
        api.listSecrets(),
        api.getMySettings(),
      ]);
      setSecrets(rows.filter((s) => s.kind === "anthropic_token"));
      const model = settings.default_model ?? "";
      setDefaultModel(model);
      setSavedModel(model);
      const jm = settings.judge_model ?? "";
      setJudgeModel(jm);
      setSavedJudgeModel(jm);
      const sm = settings.summary_model ?? "";
      setSummaryModel(sm);
      setSavedSummaryModel(sm);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load settings");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const modelWarning = modelFieldWarning(defaultModel);
  const modelDirty = defaultModel.trim() !== savedModel;

  const saveModel = async () => {
    setError("");
    setNotice("");
    setModelBusy(true);
    try {
      const { settings } = await api.putMySettings({ default_model: defaultModel.trim() || null });
      const model = settings.default_model ?? "";
      setDefaultModel(model);
      setSavedModel(model);
      setNotice(
        model === ""
          ? "Worker model cleared. Your runs now use the lead template's model."
          : `Worker model set to ${model}. It applies to your next run.`,
      );
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save worker model");
    } finally {
      setModelBusy(false);
    }
  };

  // Same validation path as the worker model (PRD #69 M2): the shared modelFieldWarning
  // gates Save, so a per-user judge model can't be saved to a shape the server rejects.
  const judgeModelWarning = modelFieldWarning(judgeModel);
  const judgeModelDirty = judgeModel.trim() !== savedJudgeModel;

  const saveJudgeModel = async () => {
    setError("");
    setNotice("");
    setJudgeModelBusy(true);
    try {
      const { settings } = await api.putMySettings({ judge_model: judgeModel.trim() || null });
      const model = settings.judge_model ?? "";
      setJudgeModel(model);
      setSavedJudgeModel(model);
      setNotice(
        model === ""
          ? "Judge model cleared. Your runs are judged on the instance default model."
          : `Judge model set to ${model}. It applies to your next judged run.`,
      );
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save judge model");
    } finally {
      setJudgeModelBusy(false);
    }
  };

  // Same validation path as the judge and worker models (PRD #362 M2): the shared
  // modelFieldWarning gates Save, so a per-user summary model can't be saved to a
  // shape the server rejects.
  const summaryModelWarning = modelFieldWarning(summaryModel);
  const summaryModelDirty = summaryModel.trim() !== savedSummaryModel;

  const saveSummaryModel = async () => {
    setError("");
    setNotice("");
    setSummaryModelBusy(true);
    try {
      const { settings } = await api.putMySettings({ summary_model: summaryModel.trim() || null });
      const model = settings.summary_model ?? "";
      setSummaryModel(model);
      setSavedSummaryModel(model);
      setNotice(
        model === ""
          ? "Summary model cleared. Your run summaries use the instance default model."
          : `Summary model set to ${model}. It applies to your next run's summaries.`,
      );
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save summary model");
    } finally {
      setSummaryModelBusy(false);
    }
  };

  return (
    <SettingsShell description="How your runs behave: autopilot, usage limits, the judge, CI fixes, and the model.">
      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      <Card className="space-y-4">
        <div>
          <SectionTitle>Autopilot</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            With autopilot on, adding the{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">autopilot</code> label alongside{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">PRD</code> on an issue in GitLab starts a
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

      {/* PRD #35. Placed after Autopilot on purpose: the two compose, and this is the
          only place the composition is visible. An autopilot run has no start
          affordance, so for that kind — and for CI-fix and self-improve runs — this
          default is the ONLY way the opt-in can ever be expressed. */}
      <Card className="space-y-4">
        <div>
          <SectionTitle>Anthropic usage limits</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            When a run exhausts your Anthropic usage window it normally{" "}
            <strong className="text-fg">fails</strong> and its work is lost. With this on, a run{" "}
            <strong className="text-fg">pauses</strong> instead and resumes by itself when the window
            reopens — it keeps its branch, its history and its place, and picks up where it left off. Runs
            you did not start by hand (autopilot, CI fixes, self-improvement) have no other way to opt in,
            so this setting is what covers them. Off by default.
          </p>
          <p className="mt-2 text-sm text-muted">
            This is the default for <strong className="text-fg">new</strong> runs. It does not change runs
            that already exist, including one that is paused right now — each run carries its own setting,
            which you can flip on the run's page.
          </p>
          <p className="mt-2 text-sm text-faint">
            A paused run holds onto its checkout and its cached dependencies while it waits, so several at
            once cost real disk on the worker. There is a cap on how many times one run will wait before it
            gives up and fails.
          </p>
        </div>

        {waitLimitError && <Alert message={waitLimitError} />}

        <label className="flex items-center gap-3 text-sm">
          <input
            type="checkbox"
            className="h-4 w-4 accent-brand"
            checked={user?.wait_on_limit ?? false}
            disabled={waitLimitBusy}
            onChange={(e) => toggleWaitOnLimit(e.target.checked)}
          />
          <span className="text-fg">Pause my new runs on a usage limit instead of failing them</span>
        </label>
      </Card>

      <Card className="space-y-4">
        <div>
          <SectionTitle>Run judge</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            With the run judge on, each of your <strong className="text-fg">finished</strong> runs is
            reviewed by an LLM on <strong className="text-fg">your own Anthropic tokens</strong>. It reads
            the run trace and produces a verdict plus recommendations (a missing worker tool, an agent or
            template to improve, and so on) in your inbox — it only recommends, and never changes code. Your
            instance admin also has to enable the feature globally for anything to run. Off by default.
          </p>
        </div>

        {/* Enforced-mode banner (PRD #69 M4). When the admin turns on enforce-all, the
            judge runs on EVERY finished run regardless of the opt-in below, still on the
            user's own token — so the honest opt-out is removing the token, not this
            toggle. effective_judge_model names the model that token will actually run. */}
        {judgeEnforcedByAdmin && (
          <Alert
            tone="warning"
            message={
              `Your admin has ENFORCED the run judge: every one of your finished runs is judged on ` +
              `your own Anthropic token${effectiveJudgeModel ? ` (model: ${effectiveJudgeModel})` : ""}, ` +
              `whether or not the opt-in below is on. The only way to opt out is to remove your Anthropic token.`
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

        {/* The judge token picker (PRD #104 M4/M6). Without it "the judge lane can
            burn a different token, set from the web UI" is unreachable, which is
            why it is required and not a nicety. Shown only with more than one
            token — with a single credential there is nothing to choose. */}
        {secrets.length > 1 && (
          <Field label="Token the judge spends">
            <Select
              aria-label="Token the judge spends"
              value={user?.judge_anthropic_secret_label ?? ""}
              disabled={judgeBusy}
              onChange={(e) => setJudgeToken(e.target.value)}
            >
              <option value="">your default token</option>
              {secrets.map((s) => (
                <option key={s.id} value={s.label}>
                  {s.label}
                  {s.is_default ? " (default)" : ""}
                </option>
              ))}
            </Select>
            <p className="mt-1.5 text-xs text-faint">
              Retrospectives can bill a different account from the runs they review — point them at
              a cheaper console key while your runs stay on a subscription.
            </p>
          </Field>
        )}

        {/* Per-user judge model (PRD #69 M2). Leave on Inherit to use the instance
            judge model the admin picked; pinning one here overrides it for YOUR judged
            runs only, and applies in enforced mode too. Same picker + validation as the
            worker model, so a bad model blocks Save rather than failing on the next run. */}
        <div className="space-y-3">
          <Field label="Judge model" htmlFor="judge-model">
            <ModelSelect id="judge-model" value={judgeModel} onChange={setJudgeModel} />
          </Field>
          <p className="text-xs text-faint">
            The Claude model your finished runs are judged on. Leave it on <em>Inherit</em> to use the
            instance default (opus unless your admin changed it); pin a cheaper alias to spend less.
          </p>
          {judgeModelWarning && <Alert message={judgeModelWarning} tone="warning" />}
          <Button
            type="button"
            disabled={judgeModelBusy || !judgeModelDirty || judgeModelWarning !== ""}
            onClick={saveJudgeModel}
          >
            Save judge model
          </Button>
        </div>
      </Card>

      {/* Per-user run-summary model (PRD #362 M2). Each run generates short
          plain-English summaries on the run owner's own token; leave this on Inherit
          to use the instance summary model the admin picked, or pin one here to
          override it for YOUR runs only. Same picker + validation as the judge and
          worker models, so a bad model blocks Save rather than failing on the next run. */}
      <Card className="space-y-4">
        <div>
          <SectionTitle>Run summaries</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            Each of your runs generates two short plain-English summaries — what it will implement, and
            what the proposed plan will do — on <strong className="text-fg">your own Anthropic token</strong>.
            Pinning a model here overrides the instance default for your own runs; other users are
            unaffected. Summaries are advisory and never block a run.
          </p>
        </div>

        <div className="space-y-3">
          <Field label="Summary model" htmlFor="summary-model">
            <ModelSelect id="summary-model" value={summaryModel} onChange={setSummaryModel} />
          </Field>
          <p className="text-xs text-faint">
            The Claude model your run summaries are generated on. Leave it on <em>Inherit</em> to use the
            instance default (haiku unless your admin changed it); pin another alias to trade cost for depth.
          </p>
          {summaryModelWarning && <Alert message={summaryModelWarning} tone="warning" />}
          <Button
            type="button"
            disabled={summaryModelBusy || !summaryModelDirty || summaryModelWarning !== ""}
            onClick={saveSummaryModel}
          >
            Save summary model
          </Button>
        </div>
      </Card>

      <Card className="space-y-4">
        <div>
          <SectionTitle>Automatic CI fixes</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            With this on, when a pipeline fails on one of your agent merge-request branches, uzi spends
            your <strong className="text-fg">own Anthropic tokens</strong> to attempt a fix automatically.
            Off by default.
          </p>
        </div>

        {ciAutofixError && <Alert message={ciAutofixError} />}

        <label className="flex items-center gap-3 text-sm">
          <input
            type="checkbox"
            className="h-4 w-4 accent-brand"
            checked={user?.ci_autofix_enabled ?? false}
            disabled={ciAutofixBusy}
            onChange={(e) => toggleCIAutofix(e.target.checked)}
          />
          <span className="text-fg">Automatically fix my failed CI pipelines</span>
        </label>
      </Card>

      <Card className="space-y-5">
        <div>
          <SectionTitle>Worker model</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            The Claude model your runs use — the lead orchestrator and its subagents that
            inherit the model. Picking one here overrides the lead template's model for your
            own runs; other users are unaffected. Leave it on <em>Inherit</em> to use the lead
            template's model (opus by default). An unrecognized custom ID only fails on the
            first run.
          </p>
        </div>

        {loading ? (
          <Skeleton className="h-9 w-full max-w-sm" />
        ) : (
          <div className="space-y-3">
            <Field label="Model" htmlFor="worker-model">
              <ModelSelect id="worker-model" value={defaultModel} onChange={setDefaultModel} />
            </Field>
            {modelWarning && <Alert message={modelWarning} tone="warning" />}
            <Button
              type="button"
              disabled={modelBusy || !modelDirty || modelWarning !== ""}
              onClick={saveModel}
            >
              Save model
            </Button>
          </div>
        )}
      </Card>
    </SettingsShell>
  );
}
