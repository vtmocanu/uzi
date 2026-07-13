// Admin → Instance settings (PRD #19 M1): the DB-backed app_settings surface.
// Ships the two configurable forge labels; future instance settings (registration
// policy, self-improvement toggle) slot in here without new plumbing. Admin-only
// (gated by AdminRoute + the admin-only API). Validation mirrors the server
// (Decision 8) for immediate feedback, but the server stays the source of truth.

import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  api,
  ApiError,
  type AppSettings,
  type Repo,
  type SelfimproveConfig,
  type SettingSource,
  type SettingsResponse,
  type UpdateSettingsPayload,
} from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import {
  Alert,
  Badge,
  type BadgeTone,
  Button,
  Card,
  Field,
  Input,
  PageHeader,
  SectionTitle,
  Select,
  Skeleton,
} from "../components/ui";
import { THEMES, THEME_LABELS } from "../lib/theme";

// slackStatusChip renders the live Slack connection state (PRD #25 M2) as a
// tone-coded Badge. Error states (error:auth | error:connection) collapse to a
// danger chip whose full class is in the title.
function slackStatusChip(status: string) {
  if (status.startsWith("error")) {
    return (
      <Badge tone="danger" dot title={`Slack connection: ${status}`}>
        {status.replace("error:", "error · ")}
      </Badge>
    );
  }
  const map: Record<string, { tone: BadgeTone; pulse?: boolean }> = {
    connected: { tone: "ok" },
    connecting: { tone: "warning", pulse: true },
    disabled: { tone: "neutral" },
  };
  const cfg = map[status] ?? { tone: "neutral" as BadgeTone };
  return (
    <Badge tone={cfg.tone} dot pulse={cfg.pulse} title={`Slack connection: ${status}`}>
      {status}
    </Badge>
  );
}

// oidcStatusChip renders OIDC SSO health (PRD #45, Nit6) as a tone-coded Badge:
// ok (green), degraded (warning — configured but discovery is failing), disabled
// (neutral). Env-configured, so this is read-only status, not a form.
function oidcStatusChip(status: string) {
  const map: Record<string, { tone: BadgeTone; pulse?: boolean }> = {
    ok: { tone: "ok" },
    degraded: { tone: "warning", pulse: true },
    disabled: { tone: "neutral" },
  };
  const cfg = map[status] ?? { tone: "neutral" as BadgeTone };
  return (
    <Badge tone={cfg.tone} dot pulse={cfg.pulse} title={`OIDC SSO: ${status}`}>
      {status}
    </Badge>
  );
}

// clientValidate reproduces the server's per-value + cross-key rules so an
// obviously-bad edit is caught before the round-trip. Returns an error message
// or null. The server re-checks regardless. The label triple must be
// pairwise-distinct — the PRDLESS label included, and regardless of its toggle
// state (PRD #22 Decision 7), matching the server's ValidateMerged.
function clientValidate(prdLabel: string, autopilotLabel: string, prdlessLabel: string): string | null {
  for (const [name, value] of [
    ["PRD label", prdLabel],
    ["Autopilot label", autopilotLabel],
    ["PRDLESS label", prdlessLabel],
  ] as const) {
    if (value.trim() === "") return `${name} must not be empty.`;
    if (value.length > 64) return `${name} must be at most 64 characters.`;
    if (value.includes(",")) return `${name} must not contain a comma.`;
  }
  if (prdLabel === autopilotLabel) return "The PRD and autopilot labels must differ.";
  if (prdlessLabel === prdLabel) return "The PRDLESS label must differ from the PRD label.";
  if (prdlessLabel === autopilotLabel) return "The PRDLESS label must differ from the autopilot label.";
  return null;
}

export function AdminSettings() {
  const { refresh } = useAuth();
  const [saved, setSaved] = useState<AppSettings | null>(null);
  // Per-key secret-configured flags and sources (PRD #25), consumed by the Slack
  // card for its "configured ✓" and env-greying behavior.
  const [secrets, setSecrets] = useState<Record<string, boolean>>({});
  const [sources, setSources] = useState<Record<string, SettingSource>>({});
  // Live Slack socket state for the status chip (PRD #25 M2), polled separately
  // so it updates without clobbering in-progress form edits.
  const [slackStatus, setSlackStatus] = useState("disabled");
  // OIDC SSO health for the read-only status line (PRD #45, Nit6). Env-configured,
  // so there is no form — just a chip that surfaces a degraded (discovery-failing) IdP.
  const [oidcStatus, setOidcStatus] = useState("disabled");
  const [oidcProviderName, setOidcProviderName] = useState("SSO");
  const [prdLabel, setPrdLabel] = useState("");
  const [autopilotLabel, setAutopilotLabel] = useState("");
  const [defaultTheme, setDefaultTheme] = useState("");
  const [prdlessEnabled, setPrdlessEnabled] = useState(true);
  const [prdlessLabel, setPrdlessLabel] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  // Vault migration progress (PRD #32): stored secrets still using pre-vault
  // (master-key) encryption. Fetched separately so a failure never blocks the
  // settings form; null = unknown/not-yet-loaded, rendered only when > 0.
  const [masterSealed, setMasterSealed] = useState<number | null>(null);

  // applyResponse fans a GET/PUT response out to the label-form fields and the
  // shared secret/source state, so both the label form and the Slack card read a
  // consistent snapshot.
  const applyResponse = useCallback((resp: SettingsResponse) => {
    const { settings, secrets: sec, sources: src } = resp;
    setSaved(settings);
    setSecrets(sec ?? {});
    setSources(src ?? {});
    setSlackStatus(resp.slack_status ?? "disabled");
    setOidcStatus(resp.oidc_status ?? "disabled");
    setOidcProviderName(resp.oidc_provider_name || "SSO");
    setPrdLabel(settings.prd_label);
    setAutopilotLabel(settings.autopilot_label);
    setDefaultTheme(settings.default_theme);
    setPrdlessEnabled(settings.prdless_enabled === "true");
    setPrdlessLabel(settings.prdless_label);
  }, []);

  const load = useCallback(async () => {
    try {
      applyResponse(await api.getSettings());
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load settings");
    } finally {
      setLoading(false);
    }
    // Best-effort, independent of the settings load (PRD #32).
    api
      .vaultMigration()
      .then(({ master_sealed }) => setMasterSealed(master_sealed))
      .catch(() => setMasterSealed(null));
  }, [applyResponse]);

  useEffect(() => {
    load();
  }, [load]);

  // Poll only the live Slack connection state so the chip reflects connecting →
  // connected without a manual reload. Uses the dedicated status endpoint (not the
  // whole settings blob) and deliberately does NOT call applyResponse (which would
  // reset the form fields and clobber an in-progress edit).
  useEffect(() => {
    const id = setInterval(async () => {
      try {
        const { slack_status } = await api.getSlackStatus();
        setSlackStatus(slack_status ?? "disabled");
      } catch {
        // Best-effort: keep the last known status on a transient failure.
      }
    }, 5000);
    return () => clearInterval(id);
  }, []);

  const dirty =
    saved !== null &&
    (prdLabel !== saved.prd_label ||
      autopilotLabel !== saved.autopilot_label ||
      defaultTheme !== saved.default_theme ||
      (prdlessEnabled ? "true" : "false") !== saved.prdless_enabled ||
      prdlessLabel !== saved.prdless_label);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    const invalid = clientValidate(prdLabel, autopilotLabel, prdlessLabel);
    if (invalid) {
      setError(invalid);
      return;
    }
    setBusy(true);
    // Only a board-filtering label (PRD or autopilot) triggers a repo resync;
    // theme and the prdless keys are presentation-/gate-only (mirrors the server's
    // settings.LabelChanged). Computed against the pre-save `saved` so the notice
    // only mentions propagation when it actually happens (N1).
    const labelChanged =
      saved !== null && (prdLabel !== saved.prd_label || autopilotLabel !== saved.autopilot_label);
    try {
      applyResponse(
        await api.updateSettings({
          prd_label: prdLabel,
          autopilot_label: autopilotLabel,
          default_theme: defaultTheme,
          prdless_enabled: prdlessEnabled ? "true" : "false",
          prdless_label: prdlessLabel,
        }),
      );
      setNotice(
        labelChanged
          ? "Settings saved. Boards reflect the label change after the next sync."
          : "Settings saved.",
      );
      // Re-resolve this admin's own theme: with no personal override, a changed
      // instance default restyles their session live.
      await refresh();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save settings");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-6">
      <PageHeader
        title="Instance settings"
        description="Configuration shared across every user of this uzi instance."
      />
      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      {masterSealed !== null && masterSealed > 0 && (
        <Card className="space-y-2 border-warn/30">
          <SectionTitle>Vault migration</SectionTitle>
          <p className="text-sm text-muted">
            <strong className="text-fg">
              {masterSealed} stored secret{masterSealed === 1 ? "" : "s"}
            </strong>{" "}
            still use pre-vault encryption — their owners haven&rsquo;t logged in since password
            protection was enabled. Each re-seals under the owner&rsquo;s password automatically the
            next time they log in (or unlock). For full protection those owners should also rotate
            the affected token: sealing an existing value protects it from now on, not
            retroactively.
          </p>
        </Card>
      )}

      <Card className="space-y-5">
        <div>
          <SectionTitle>Forge labels</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            Which GitLab labels this factory reacts to. Changing a label never creates it on the
            forge — create the label in GitLab yourself. The labels must all differ.
          </p>
        </div>

        {loading ? (
          <div className="space-y-4">
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-16 w-full" />
          </div>
        ) : (
          <form onSubmit={save} className="space-y-4">
            <div className="space-y-1.5">
              <Field label="PRD label">
                <Input
                  value={prdLabel}
                  maxLength={64}
                  autoComplete="off"
                  placeholder="PRD"
                  onChange={(e) => setPrdLabel(e.target.value)}
                />
              </Field>
              <p className="text-xs text-faint">
                Marks an issue as factory work. The board only shows issues carrying this label.
              </p>
            </div>
            <div className="space-y-1.5">
              <Field label="Autopilot label">
                <Input
                  value={autopilotLabel}
                  maxLength={64}
                  autoComplete="off"
                  placeholder="autopilot"
                  onChange={(e) => setAutopilotLabel(e.target.value)}
                />
              </Field>
              <p className="text-xs text-faint">
                Adding this label to a PRD issue lets an opted-in user run it end to end, with no
                plan-approval step.
              </p>
            </div>
            <div className="space-y-3 border-t border-edge pt-4">
              <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  checked={prdlessEnabled}
                  onChange={(e) => setPrdlessEnabled(e.target.checked)}
                  className="h-4 w-4 rounded border-edge accent-brand"
                />
                Enable the PRDLESS escape hatch
              </label>
              <div className="space-y-1.5">
                <Field label="PRDLESS label">
                  <Input
                    value={prdlessLabel}
                    maxLength={64}
                    autoComplete="off"
                    placeholder="PRDLESS"
                    disabled={!prdlessEnabled}
                    onChange={(e) => setPrdlessLabel(e.target.value)}
                  />
                </Field>
                <p className="text-xs text-faint">
                  An issue carrying this label can start a run with no <code>prds/*.md</code> link.
                  Must differ from the PRD and autopilot labels; the name is editable only while the
                  feature is on.
                </p>
              </div>
            </div>
            <div className="space-y-1.5 border-t border-edge pt-4">
              <Field label="Default theme" htmlFor="default-theme">
                <Select
                  id="default-theme"
                  value={defaultTheme}
                  onChange={(e) => setDefaultTheme(e.target.value)}
                >
                  {THEMES.map((t) => (
                    <option key={t} value={t}>
                      {THEME_LABELS[t]}
                    </option>
                  ))}
                </Select>
              </Field>
              <p className="text-xs text-faint">
                The theme new users, and anyone without a personal choice, see. Each user can
                override it under Settings → Appearance.
              </p>
            </div>
            <Button type="submit" disabled={busy || !dirty}>
              {busy ? "Saving…" : "Save settings"}
            </Button>
          </form>
        )}
      </Card>

      {!loading && saved && <JudgeSettingsCard settings={saved} onSaved={applyResponse} />}

      {!loading && <SelfImproveSettingsCard />}

      {!loading && oidcStatus !== "disabled" && (
        <Card className="space-y-2">
          <div className="flex items-center justify-between gap-3">
            <div className="min-w-0">
              <SectionTitle>Single sign-on ({oidcProviderName})</SectionTitle>
              <p className="mt-2 text-sm text-muted">
                OIDC is configured via environment variables (restart to change). A{" "}
                <span className="font-medium text-warn">degraded</span> status means the provider is
                enabled but uzi could not reach its discovery document — the button still works and
                a sign-in attempt retries discovery.
              </p>
            </div>
            {oidcStatusChip(oidcStatus)}
          </div>
        </Card>
      )}

      {!loading && saved && (
        <SlackSettingsCard
          settings={saved}
          secrets={secrets}
          sources={sources}
          status={slackStatus}
          onSaved={applyResponse}
        />
      )}

      {!loading && saved && (
        <HealthSettingsCard settings={saved} sources={sources} onSaved={applyResponse} />
      )}
    </div>
  );
}

// JudgeSettingsCard is the admin surface for the run judge (PRD #46): the global
// on/off kill-switch plus the cheap model the judge runs on. It saves independently
// of the label form (like the Slack card). A user still has to opt IN under their
// own Settings, and the judge always spends that user's tokens — this card only
// arms the feature instance-wide. The self-improvement settings land in M5.
function JudgeSettingsCard({
  settings,
  onSaved,
}: {
  settings: AppSettings;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [enabled, setEnabled] = useState(settings.judge_enabled === "true");
  const [model, setModel] = useState(settings.judge_model);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const dirty = (enabled ? "true" : "false") !== settings.judge_enabled || model !== settings.judge_model;

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
      });
      onSaved(resp);
      setEnabled(resp.settings.judge_enabled === "true");
      setModel(resp.settings.judge_model);
      setNotice("Run judge settings saved.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save run judge settings");
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
              placeholder="haiku"
              onChange={(e) => setModel(e.target.value)}
            />
          </Field>
          <p className="text-xs text-faint">
            The Claude model the judge runs on. A retrospective is a single trace round-trip, so the cheap
            default (<code className="rounded bg-raised px-1 py-0.5 text-fg">haiku</code>) is usually right.
          </p>
        </div>

        <Button type="submit" disabled={busy || !dirty}>
          {busy ? "Saving…" : "Save run judge settings"}
        </Button>
      </form>
    </Card>
  );
}

// SelfImproveSettingsCard is the admin surface for the autonomous self-improvement
// job (PRD #46 M5). It self-loads its config (a dedicated endpoint, not the label
// form) and the admin's connected repos for the target picker. The consent copy is
// deliberately explicit: the job spends the ENABLING ADMIN'S OWN Anthropic token,
// on a standing basis (any logged-in window, not a one-time spend), producing
// autonomous MRs — the bot never merges to main.
function SelfImproveSettingsCard() {
  const [config, setConfig] = useState<SelfimproveConfig | null>(null);
  const [repos, setRepos] = useState<Repo[]>([]);
  const [enabled, setEnabled] = useState(false);
  const [interval, setIntervalValue] = useState("48h");
  const [repoId, setRepoId] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const apply = useCallback((c: SelfimproveConfig) => {
    setConfig(c);
    setEnabled(c.enabled);
    setIntervalValue(c.interval);
    setRepoId(c.repo_id ?? "");
  }, []);

  useEffect(() => {
    api
      .getSelfimprove()
      .then(({ selfimprove }) => apply(selfimprove))
      .catch(() => {});
    api
      .listRepos()
      .then(({ repos }) => setRepos(repos))
      .catch(() => setRepos([]));
  }, [apply]);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    if (enabled && repoId === "") {
      setError("Choose a repository for the self-improvement job to run against.");
      return;
    }
    setBusy(true);
    try {
      const { selfimprove } = await api.updateSelfimprove({
        enabled,
        interval,
        // repo_id is only meaningful when enabling; the server records the session
        // admin as the owner (never sent here).
        ...(enabled ? { repo_id: repoId } : {}),
      });
      apply(selfimprove);
      setNotice("Self-improvement settings saved.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save self-improvement settings");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Self-improvement</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          When on, uzi periodically reviews its own codebase and the accumulated{" "}
          <code className="rounded bg-raised px-1 py-0.5 text-fg">improve_uzi</code> recommendations, then
          autonomously opens (or extends) one merge request on the chosen repo &mdash; picking a single top
          improvement each cycle.
        </p>
        <p className="mt-2 text-sm text-warn">
          It runs on <strong className="text-fg">your own Anthropic token</strong>, on a standing basis: while
          you are logged in, the job runs unattended on its schedule and produces autonomous code changes
          &mdash; not a one-time spend. A human still reviews and merges; the bot never merges to{" "}
          <code className="rounded bg-raised px-1 py-0.5 text-fg">main</code>.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      {config?.active && <Alert tone="info" message="A self-improvement run is currently in progress." />}
      {config?.last_run_at && (
        <p className="text-xs text-faint">Last cycle: {new Date(config.last_run_at).toLocaleString()}</p>
      )}

      <form onSubmit={save} className="space-y-4">
        <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
            className="h-4 w-4 rounded border-edge accent-brand"
          />
          Enable the self-improvement job (uses your token)
        </label>

        <Field label="Repository">
          <Select value={repoId} onChange={(e) => setRepoId(e.target.value)}>
            <option value="">Select a connected repo…</option>
            {repos.map((r) => (
              <option key={r.id} value={r.id}>
                {r.path_with_namespace}
              </option>
            ))}
          </Select>
        </Field>

        <div className="space-y-1.5">
          <Field label="Interval">
            <Input
              value={interval}
              maxLength={32}
              autoComplete="off"
              placeholder="48h"
              onChange={(e) => setIntervalValue(e.target.value)}
            />
          </Field>
          <p className="text-xs text-faint">
            How often a cycle becomes due, as a Go duration (e.g.{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">48h</code>). The default is every two days.
          </p>
        </div>

        <Button type="submit" disabled={busy}>
          {busy ? "Saving…" : "Save self-improvement settings"}
        </Button>
      </form>
    </Card>
  );
}

const HEALTH_FIELDS: { key: keyof AppSettings; label: string; hint?: string }[] = [
  {
    key: "health_stall_seconds",
    label: "Stalled after (seconds of silence)",
    hint: "No new activity while no tool call is in flight.",
  },
  {
    key: "health_slow_seconds",
    label: "Slow after (seconds running)",
    hint: "Wall clock since the run started; clamped below RUN_TIMEOUT.",
  },
  { key: "health_queued_seconds", label: "Stuck queued after (seconds)" },
  { key: "health_approval_seconds", label: "Awaiting approval after (seconds)" },
  { key: "health_nudge_cooldown_seconds", label: "Slack nudge cooldown (seconds)" },
];

// validateHealthSeconds mirrors the server's write-time rule (Decision 5) for
// immediate feedback: 0 (disable) or an integer in [60, 86400]. The digit-only test
// keeps parity with the server's strconv.Atoi, which rejects the forms Number()
// would silently accept ("1e3", "0x10", "5.0"); the server stays the source of truth.
function validateHealthSeconds(value: string): string | null {
  const v = value.trim();
  if (!/^\d+$/.test(v)) return "Must be a whole number of seconds";
  const n = Number(v);
  if (n === 0) return null;
  if (n < 60 || n > 86400) return "Must be 0 (disabled) or between 60 and 86400 seconds";
  return null;
}

// HealthSettingsCard is the admin surface for the run-health detector (PRD #47): an
// enable toggle plus the five integer-seconds thresholds. It saves independently of
// the other cards, sending only the fields that changed. The health keys are never
// env-sourced (Decision 5: no env vars), but the env guard is kept for symmetry —
// the server rejects an env write anyway.
function HealthSettingsCard({
  settings,
  sources,
  onSaved,
}: {
  settings: AppSettings;
  sources: Record<string, SettingSource>;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [enabled, setEnabled] = useState(settings.health_enabled === "true");
  const [values, setValues] = useState<Record<string, string>>(() =>
    Object.fromEntries(HEALTH_FIELDS.map((f) => [f.key, settings[f.key]])),
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const isEnv = (key: string) => sources[key] === "env";

  const fieldError = HEALTH_FIELDS.map((f) => validateHealthSeconds(values[f.key])).find(Boolean) ?? null;

  const dirty =
    (enabled ? "true" : "false") !== settings.health_enabled ||
    HEALTH_FIELDS.some((f) => values[f.key] !== settings[f.key]);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    if (fieldError) {
      setError(fieldError);
      return;
    }
    // Send only what changed (and is not env-fixed), so an idempotent save is a no-op.
    const payload: UpdateSettingsPayload = {};
    if (!isEnv("health_enabled") && (enabled ? "true" : "false") !== settings.health_enabled) {
      payload.health_enabled = enabled ? "true" : "false";
    }
    for (const f of HEALTH_FIELDS) {
      if (!isEnv(f.key) && values[f.key].trim() !== settings[f.key]) {
        payload[f.key] = values[f.key].trim();
      }
    }
    if (Object.keys(payload).length === 0) return;

    setBusy(true);
    try {
      const resp = await api.updateSettings(payload);
      onSaved(resp);
      setEnabled(resp.settings.health_enabled === "true");
      setValues(Object.fromEntries(HEALTH_FIELDS.map((f) => [f.key, resp.settings[f.key]])));
      setNotice("Run-health settings saved.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save run-health settings");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Run health</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Flag runs that look slow, stuck, or looping on the board and in Slack. This is an early
          warning only — it never stops a run (RUN_TIMEOUT and the idle/iteration caps still do
          that). Set any threshold to 0 to disable that one signal.
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
          Enable run-health detection
        </label>

        <div className="grid gap-4 sm:grid-cols-2">
          {HEALTH_FIELDS.map((f) => {
            const err = validateHealthSeconds(values[f.key]);
            return (
              <div key={f.key} className="space-y-1">
                <Field label={f.label} htmlFor={f.key}>
                  <Input
                    id={f.key}
                    type="number"
                    min={0}
                    step={1}
                    inputMode="numeric"
                    value={values[f.key]}
                    disabled={isEnv(f.key)}
                    aria-invalid={err != null}
                    aria-describedby={err ? `${f.key}-error` : undefined}
                    onChange={(e) => setValues((v) => ({ ...v, [f.key]: e.target.value }))}
                  />
                </Field>
                {f.hint && <p className="text-xs text-faint">{f.hint}</p>}
                {err && (
                  <p id={`${f.key}-error`} className="text-xs text-warn">
                    {err}
                  </p>
                )}
              </div>
            );
          })}
        </div>

        <Button type="submit" disabled={!dirty || busy || fieldError != null}>
          {busy ? "Saving…" : "Save run health"}
        </Button>
      </form>
    </Card>
  );
}

// SlackSettingsCard is the admin surface for the Slack integration (PRD #25 M1):
// an enable toggle, write-only bot/app token fields (showing "configured ✓" when
// a value is already stored), the public base URL for deep links, and a
// connection-status chip (stubbed "disabled" until M2 wires live state). A field
// whose value is fixed by an environment variable renders disabled with a
// "set from environment" hint — the server rejects a write to it (409), so this
// greying reflects enforced policy. It saves independently of the label form,
// sending a token only when one was entered.
function SlackSettingsCard({
  settings,
  secrets,
  sources,
  status,
  onSaved,
}: {
  settings: AppSettings;
  secrets: Record<string, boolean>;
  sources: Record<string, SettingSource>;
  status: string;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [enabled, setEnabled] = useState(settings.slack_enabled === "true");
  const [publicBaseUrl, setPublicBaseUrl] = useState(settings.public_base_url);
  const [botToken, setBotToken] = useState("");
  const [appToken, setAppToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const isEnv = (key: string) => sources[key] === "env";
  const enabledEnv = isEnv("slack_enabled");
  const baseUrlEnv = isEnv("public_base_url");

  const dirty =
    (enabled ? "true" : "false") !== settings.slack_enabled ||
    publicBaseUrl !== settings.public_base_url ||
    botToken.trim() !== "" ||
    appToken.trim() !== "";

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    // Send only editable (non env-fixed) fields; a token only when one was typed
    // (the write-only fields start empty and empty means "leave unchanged").
    const payload: UpdateSettingsPayload = {};
    if (!enabledEnv) payload.slack_enabled = enabled ? "true" : "false";
    if (!baseUrlEnv) payload.public_base_url = publicBaseUrl;
    if (!isEnv("slack_bot_token") && botToken.trim() !== "") payload.slack_bot_token = botToken;
    if (!isEnv("slack_app_token") && appToken.trim() !== "") payload.slack_app_token = appToken;
    if (Object.keys(payload).length === 0) return;

    setBusy(true);
    try {
      const resp = await api.updateSettings(payload);
      onSaved(resp);
      setEnabled(resp.settings.slack_enabled === "true");
      setPublicBaseUrl(resp.settings.public_base_url);
      setBotToken("");
      setAppToken("");
      setNotice("Slack settings saved.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save Slack settings");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div className="flex items-start justify-between gap-4">
        <div>
          <SectionTitle>Slack</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            Send each user run notifications and plan-approval buttons as Slack DMs. The bot runs
            outbound-only (Socket Mode) — no public URL. Tokens are stored encrypted and never shown
            again.
          </p>
        </div>
        {slackStatusChip(status)}
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      <form onSubmit={save} className="space-y-4">
        <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={enabled}
            disabled={enabledEnv}
            onChange={(e) => setEnabled(e.target.checked)}
            className="h-4 w-4 rounded border-edge accent-brand disabled:opacity-50"
          />
          Enable Slack notifications
        </label>

        <SlackTokenField
          label="Bot token"
          placeholder="xoxb-…"
          value={botToken}
          configured={!!secrets.slack_bot_token}
          env={isEnv("slack_bot_token")}
          onChange={setBotToken}
          help="The xoxb- token from your Slack app (OAuth & Permissions). Validated against Slack when you save."
        />
        <SlackTokenField
          label="App-level token"
          placeholder="xapp-…"
          value={appToken}
          configured={!!secrets.slack_app_token}
          env={isEnv("slack_app_token")}
          onChange={setAppToken}
          help="The xapp- token with connections:write, for the Socket Mode connection."
        />

        <div className="space-y-1.5">
          <Field label="Public base URL">
            <Input
              value={publicBaseUrl}
              autoComplete="off"
              placeholder="http://127.0.0.1:8080"
              disabled={baseUrlEnv}
              onChange={(e) => setPublicBaseUrl(e.target.value)}
            />
          </Field>
          <p className="text-xs text-faint">
            {baseUrlEnv
              ? "Set from environment."
              : "Where Slack message links point. On a laptop the loopback default only resolves for you — set a Tailscale/LAN URL to share links."}
          </p>
        </div>

        <Button type="submit" disabled={busy || !dirty}>
          {busy ? "Saving…" : "Save Slack settings"}
        </Button>
      </form>
    </Card>
  );
}

// SlackTokenField is a write-only secret input: it never renders the stored
// token, shows a "configured ✓" affordance when one is stored, and greys itself
// out with a "set from environment" hint when the value is env-fixed.
function SlackTokenField({
  label,
  placeholder,
  value,
  configured,
  env,
  onChange,
  help,
}: {
  label: string;
  placeholder: string;
  value: string;
  configured: boolean;
  env: boolean;
  onChange: (v: string) => void;
  help: string;
}) {
  return (
    <div className="space-y-1.5">
      <Field label={label}>
        <Input
          type="password"
          value={value}
          autoComplete="off"
          placeholder={env ? "" : configured ? "configured ✓ — enter a new value to replace" : placeholder}
          disabled={env}
          onChange={(e) => onChange(e.target.value)}
        />
      </Field>
      <p className="text-xs text-faint">
        {env ? "Set from environment." : help}
        {!env && configured && " A value is stored; leave blank to keep it."}
      </p>
    </div>
  );
}
