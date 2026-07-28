// Settings → Account & tokens: the account card (moved here from the old
// dashboard) plus the Anthropic token lifecycle. Lives inside SettingsShell so
// tokens/forge/workers are one discoverable area. The token LIST itself is
// AnthropicTokens (PRD #104 M6) — this page owns the fetch so the list and the
// rate-limit meters refresh together.

import { useCallback, useEffect, useState } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, type SecretMeta } from "../lib/api";
import { Alert, Button, Card, Field, SectionTitle, Select, Skeleton } from "../components/ui";
import { AnthropicTokens } from "../components/AnthropicTokens";
import { ModelSelect } from "../components/ModelSelect";
import { modelFieldWarning } from "../lib/agentTemplates";
import { SettingsShell } from "../components/SettingsShell";
import { RateLimitCard } from "../components/RateLimitMeters";
import { VaultBadge, useVaultLock } from "../components/VaultControls";
import { SlackNotifications } from "../components/SlackNotifications";
import { prefs } from "../lib/prefs";
import { applyTheme, resolveTheme, THEMES, THEME_LABELS, isTheme } from "../lib/theme";

// One-time dismissal (per browser) of the rotate-your-legacy-token reminder.
const ROTATE_NOTICE_KEY = "uzi.vault.rotateNoticeDismissed";

export function Settings() {
  const { user, refresh, themeOverride, defaultTheme, vaultUnlocked } = useAuth();
  const [vaultNotice, setVaultNotice] = useState("");
  const { lock, locking } = useVaultLock(() =>
    setVaultNotice(
      "Vault locked. Runs already in flight finish; your new runs wait as “waiting for vault unlock” until you unlock again.",
    ),
  );
  const [rotateDismissed, setRotateDismissed] = useState(() => prefs.get(ROTATE_NOTICE_KEY, false));
  const dismissRotate = () => {
    prefs.set(ROTATE_NOTICE_KEY, true);
    setRotateDismissed(true);
  };
  const [secrets, setSecrets] = useState<SecretMeta[]>([]);
  const [loading, setLoading] = useState(true);
  // busy is the page-level guard the token card also respects, so a model save and
  // a token mutation cannot race each other's reloads.
  const [busy] = useState(false);
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

  // Worker model: "" = inherit. savedModel is the persisted value, so Save is
  // only offered when the picker differs from what is stored.
  const [defaultModel, setDefaultModel] = useState("");
  const [savedModel, setSavedModel] = useState("");
  const [modelBusy, setModelBusy] = useState(false);

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

  // Appearance: the per-user theme override. "" = use the instance default. The
  // change is applied live (optimistic) then persisted; a failed save re-syncs
  // from the server, reverting the optimistic stamp.
  const [themeBusy, setThemeBusy] = useState(false);
  const [themeError, setThemeError] = useState("");

  const changeTheme = async (value: string) => {
    setThemeError("");
    const override = value === "" ? null : value;
    // Apply immediately so the switch feels live; the server value reconciles.
    applyTheme(resolveTheme(override, defaultTheme));
    setThemeBusy(true);
    try {
      await api.putMySettings({ theme: override });
      await refresh(); // sync themeOverride + re-apply the authoritative theme
    } catch (err) {
      setThemeError(err instanceof ApiError ? err.message : "Failed to save theme");
      await refresh(); // revert the optimistic stamp to the server's truth
    } finally {
      setThemeBusy(false);
    }
  };

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

  return (
    <SettingsShell description="Your personal uzi configuration.">
      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      <AnthropicTokens
        secrets={secrets}
        loading={loading}
        busy={busy}
        reload={load}
        onError={setError}
        onNotice={setNotice}
        judgeSecretId={user?.judge_anthropic_secret_id ?? null}
      />

      {/* The rotate-your-legacy-token reminder (PRD #32): password protection
          applies from the save forward, never retroactively. */}
      {secrets.length > 0 && !rotateDismissed && (
        <div className="rounded-lg border border-info/40 bg-info/10 px-4 py-3 text-sm text-info">
          <p className="text-fg">
            <strong className="font-semibold">Protecting an older token?</strong> If you first saved
            a token before password-protection was enabled, an operator could have read it. The
            protection applies from the moment you save, not retroactively — for full protection,
            rotate the token in the Anthropic console and replace its value above.
          </p>
          <button
            type="button"
            onClick={dismissRotate}
            className="mt-2 text-xs font-medium text-brand hover:text-brand-hover"
          >
            Got it, dismiss
          </button>
        </div>
      )}

      {/* Claude rate-limit meters (PRD #53). Self-gates: hidden when no token is
          set, greyed on "unavailable", live meters once a reading lands. */}
      <RateLimitCard />

      <Card className="space-y-4">
        <div>
          <SectionTitle>Vault</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            Your Anthropic token is sealed with a key derived from your login password, so it is
            readable only while your vault is unlocked in this session. It unlocks
            automatically when you log in and stays unlocked — including overnight — until you lock it
            or the server restarts. While locked, your agents pause: new runs wait as{" "}
            <em>waiting for vault unlock</em> rather than failing.
          </p>
        </div>

        {/* Info (not success/green): locking flips the badge amber, so a green
            "success" toast would clash with the color language. */}
        {vaultNotice && <Alert tone="info" message={vaultNotice} />}

        <div className="flex items-center justify-between rounded-lg border border-edge bg-raised/60 px-4 py-3">
          <VaultBadge />
          {vaultUnlocked ? (
            <Button variant="secondary" size="sm" disabled={locking} onClick={lock}>
              {locking ? "Locking…" : "Lock vault"}
            </Button>
          ) : (
            <span className="text-sm text-muted">Locked — unlock from the banner above.</span>
          )}
        </div>
      </Card>

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

        {judgeError && <Alert message={judgeError} />}

        <label className="flex items-center gap-3 text-sm">
          <input
            type="checkbox"
            className="h-4 w-4 accent-brand"
            checked={user?.judge_enabled ?? false}
            disabled={judgeBusy}
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

      <Card className="space-y-5">
        <div>
          <SectionTitle>Appearance</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            The theme uzi renders for you. It follows you across browsers. Leave it on{" "}
            <em>Use default</em> to track the instance default your admin sets.
          </p>
        </div>

        {themeError && <Alert message={themeError} />}

        <div className="max-w-sm">
          <Field label="Theme" htmlFor="appearance-theme">
            <Select
              id="appearance-theme"
              value={isTheme(themeOverride) ? themeOverride : ""}
              disabled={themeBusy}
              onChange={(e) => changeTheme(e.target.value)}
            >
              <option value="">Use default ({THEME_LABELS[defaultTheme]})</option>
              {THEMES.map((t) => (
                <option key={t} value={t}>
                  {THEME_LABELS[t]}
                </option>
              ))}
            </Select>
          </Field>
        </div>
      </Card>

      <SlackNotifications />

      {user && (
        <Card>
          <SectionTitle>Your account</SectionTitle>
          <dl className="mt-3 divide-y divide-edge">
            {(
              [
                ["Email", user.email],
                ["Display name", user.display_name ?? "—"],
                ["Role", user.is_admin ? "Administrator" : "User"],
                ["Account status", user.is_active ? "Active" : "Deactivated"],
                ["Joined", new Date(user.created_at).toLocaleString()],
                ["Last login", user.last_login ? new Date(user.last_login).toLocaleString() : "—"],
              ] as [string, string][]
            ).map(([k, v]) => (
              <div key={k} className="flex justify-between py-2 text-sm">
                <dt className="text-muted">{k}</dt>
                <dd className="text-fg">{v}</dd>
              </div>
            ))}
          </dl>
        </Card>
      )}
    </SettingsShell>
  );
}
