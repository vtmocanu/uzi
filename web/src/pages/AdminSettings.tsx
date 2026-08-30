// Admin → Instance settings (PRD #19 M1): the DB-backed app_settings surface.
// Ships the two configurable forge labels; future instance settings (registration
// policy, self-improvement toggle) slot in here without new plumbing. Admin-only
// (gated by AdminRoute + the admin-only API). Validation mirrors the server
// (Decision 8) for immediate feedback, but the server stays the source of truth.

import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  api,
  ApiError,
  isHttpsUrl,
  type AgentSourceStaged,
  type AgentSourceView,
  type AppSettings,
  type ReleaseCheckStatus,
  type Repo,
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
  SectionTitle,
  Select,
  Skeleton,
} from "../components/ui";
import { AdminShell } from "../components/AdminShell";
import { displayVersion, formatDay } from "../components/BuildInfoPopover";
import { formatAgo } from "../lib/rateLimits";
import { DocLink } from "../components/DocLink";
import { DOC_ADMIN_SETTINGS, DOC_GITHUB_PROJECT_SYNC } from "../lib/doclinks";
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
// obviously-bad edit is caught before the round-trip. Returns an error message or
// null. The server re-checks regardless. The `uzi` and autopilot labels must differ
// (PRD #764), matching the server's ValidateMerged.
function clientValidate(uziLabel: string, autopilotLabel: string): string | null {
  for (const [name, value] of [
    ["uzi label", uziLabel],
    ["Autopilot label", autopilotLabel],
  ] as const) {
    if (value.trim() === "") return `${name} must not be empty.`;
    if (value.length > 64) return `${name} must be at most 64 characters.`;
    if (value.includes(",")) return `${name} must not contain a comma.`;
  }
  if (uziLabel === autopilotLabel) return "The uzi and autopilot labels must differ.";
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
  const [uziLabel, setUziLabel] = useState("");
  const [autopilotLabel, setAutopilotLabel] = useState("");
  const [defaultTheme, setDefaultTheme] = useState("");
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
    setUziLabel(settings.uzi_label);
    setAutopilotLabel(settings.autopilot_label);
    setDefaultTheme(settings.default_theme);
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
    (uziLabel !== saved.uzi_label ||
      autopilotLabel !== saved.autopilot_label ||
      defaultTheme !== saved.default_theme);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    const invalid = clientValidate(uziLabel, autopilotLabel);
    if (invalid) {
      setError(invalid);
      return;
    }
    setBusy(true);
    // A label change (uzi or autopilot) triggers a repo resync; theme is
    // presentation-only (mirrors the server's settings.LabelChanged). Computed against
    // the pre-save `saved` so the notice only mentions propagation when it happens (N1).
    const labelChanged =
      saved !== null && (uziLabel !== saved.uzi_label || autopilotLabel !== saved.autopilot_label);
    try {
      const payload: UpdateSettingsPayload = {
        uzi_label: uziLabel,
        autopilot_label: autopilotLabel,
        default_theme: defaultTheme,
      };
      applyResponse(await api.updateSettings(payload));
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

  // The on-page section index. Built from the SAME conditions that render each
  // card below, so a hidden card never leaves a dead jump link. Plain hash
  // anchors (not router Links): the browser's own jump behavior is exactly what
  // an in-page index wants, and react-router does not intercept bare <a> tags.
  const sections: { id: string; label: string }[] = [
    ...(masterSealed !== null && masterSealed > 0
      ? [{ id: "vault-migration", label: "Vault migration" }]
      : []),
    { id: "forge-labels", label: "Forge labels" },
    ...(!loading && saved ? [{ id: "run-judge", label: "Run judge" }] : []),
    ...(!loading && oidcStatus !== "disabled"
      ? [{ id: "sso", label: "Single sign-on" }]
      : []),
    ...(!loading && saved
      ? [
          { id: "slack", label: "Slack" },
          { id: "run-health", label: "Run health" },
          { id: "docker-allowlist", label: "Docker workers" },
          { id: "ephemeral-workers", label: "Ephemeral workers" },
          { id: "capability-scheduling", label: "Capability scheduling" },
        ]
      : []),
  ];

  return (
    <AdminShell
      description={
        <>
          Configuration shared across every user of this uzi instance. See the{" "}
          <DocLink slug={DOC_ADMIN_SETTINGS}>admin settings</DocLink> guide.
        </>
      }
    >
      {/* The section index: this tab is eight cards deep, and without an index the
          only way to learn what it holds is to scroll all of it. Quiet pill links,
          not a second tab row — tabs switch content, these just scroll it. */}
      {!loading && sections.length > 1 && (
        <nav aria-label="Sections on this page" className="flex flex-wrap gap-1.5">
          {sections.map((s) => (
            <a
              key={s.id}
              href={`#${s.id}`}
              className="rounded-full border border-edge px-2.5 py-1 text-xs font-medium text-muted transition-colors hover:border-edge-strong hover:text-fg"
            >
              {s.label}
            </a>
          ))}
        </nav>
      )}

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      {masterSealed !== null && masterSealed > 0 && (
        <Card id="vault-migration" className="scroll-mt-6 space-y-2 border-warn/30">
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

      <Card id="forge-labels" className="scroll-mt-6 space-y-5">
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
              <Field label="uzi label">
                <Input
                  value={uziLabel}
                  maxLength={64}
                  autoComplete="off"
                  placeholder="uzi"
                  onChange={(e) => setUziLabel(e.target.value)}
                />
              </Field>
              <p className="text-xs text-faint">
                The single run-eligibility label. An issue carrying it is uzi&apos;s to run — a user can
                start it and the <code>Planned</code>/<code>bug</code> sweeps fire it. uzi writes it on
                Promote and on judge-filed issues.
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
                Adding this label to a <code>uzi</code> issue lets an opted-in user run it end to end,
                with no plan-approval step.
              </p>
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

      {!loading && saved && (
        <section id="run-judge" className="scroll-mt-6">
          <JudgeSettingsCard settings={saved} onSaved={applyResponse} />
        </section>
      )}

      {!loading && saved && (
        <section id="run-summaries" className="scroll-mt-6">
          <SummarySettingsCard settings={saved} onSaved={applyResponse} />
        </section>
      )}

      {!loading && (
        <section id="agent-source" className="scroll-mt-6">
          <AgentSourceSettingsCard />
        </section>
      )}

      {!loading && (
        <section id="updates" className="scroll-mt-6">
          <UpdatesSettingsCard />
        </section>
      )}

      {!loading && oidcStatus !== "disabled" && (
        <Card id="sso" className="scroll-mt-6 space-y-2">
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
        <section id="slack" className="scroll-mt-6">
          <SlackSettingsCard
            settings={saved}
            secrets={secrets}
            sources={sources}
            status={slackStatus}
            onSaved={applyResponse}
          />
        </section>
      )}

      {!loading && saved && (
        <section id="run-health" className="scroll-mt-6">
          <HealthSettingsCard settings={saved} sources={sources} onSaved={applyResponse} />
        </section>
      )}

      {!loading && saved && (
        <section id="docker-allowlist" className="scroll-mt-6">
          <DockerAllowlistCard settings={saved} sources={sources} onSaved={applyResponse} />
        </section>
      )}

      {!loading && saved && (
        <section id="ephemeral-workers" className="scroll-mt-6">
          <EphemeralWorkersCard settings={saved} onSaved={applyResponse} />
        </section>
      )}

      {!loading && saved && (
        <section id="capability-scheduling" className="scroll-mt-6">
          <CapabilitySchedulingCard settings={saved} sources={sources} onSaved={applyResponse} />
        </section>
      )}

      {!loading && saved && (
        <section id="github-project-sync" className="scroll-mt-6">
          <GithubProjectSyncCard settings={saved} sources={sources} onSaved={applyResponse} />
        </section>
      )}
    </AdminShell>
  );
}

// JudgeSettingsCard is the admin surface for the run judge (PRD #46): the global
// on/off kill-switch plus the cheap model the judge runs on. It saves independently
// of the label form (like the Slack card). A user still has to opt IN under their
// own Settings, and the judge always spends that user's tokens — this card only
// arms the feature instance-wide.
function JudgeSettingsCard({
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

// EphemeralWorkersCard is the admin surface for the ephemeral worker
// auto-provisioning instance kill-switch (PRD #529 / #649 M1). A single-bool card
// that saves independently of the label form, mirroring JudgeSettingsCard. It is an
// INSTANCE kill-switch: when off, no run ever auto-provisions a throwaway hosted
// worker regardless of a user's per-account opt-in; when on, users can still opt in
// individually on the Workers page. Web-only — the key already round-trips through
// /admin/settings.
function EphemeralWorkersCard({
  settings,
  onSaved,
}: {
  settings: AppSettings;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [enabled, setEnabled] = useState(settings.ephemeral_workers_enabled === "true");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const dirty = (enabled ? "true" : "false") !== settings.ephemeral_workers_enabled;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    setBusy(true);
    try {
      const resp = await api.updateSettings({ ephemeral_workers_enabled: String(enabled) });
      onSaved(resp);
      setEnabled(resp.settings.ephemeral_workers_enabled === "true");
      setNotice("Ephemeral workers settings saved.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save ephemeral workers settings");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Ephemeral workers</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          When on, a queued run that needs a capability no online worker has can auto-provision a
          run-bound, throwaway hosted worker on demand, reaped when the run finishes. This switch is the
          instance <strong className="text-fg">kill-switch</strong>: while it is off, no worker is ever
          auto-provisioned regardless of any per-user opt-in. With it on, users can still individually opt
          in per account on the Workers page. Off by default.
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
          Auto-provision workers on demand for this instance
        </label>

        <Button type="submit" disabled={busy || !dirty}>
          {busy ? "Saving…" : "Save ephemeral workers settings"}
        </Button>
      </form>
    </Card>
  );
}

// The copyable upgrade runbook (PRD #836 M5). Static, generic ops guidance — a helm
// upgrade for hosted/k8s and a compose pull+up for the laptop loop. Non-exported so
// knip's dead-export tier stays green (an exported const read only here reddens it).
const UPGRADE_RUNBOOK = [
  "# helm (hosted / k8s)",
  "helm upgrade uzi oci://ghcr.io/vtmocanu/uzi",
  "# compose (laptop)",
  "docker compose pull && docker compose up -d",
].join("\n");

// releaseExcerpt renders a short PLAIN-TEXT preview of the raw release markdown
// (never HTML — the body is admin-supplied markdown and must not be injected). It
// collapses to the first few non-empty lines, capped at ~300 chars, with an ellipsis
// when truncated. Each previewed line is tidied — a leading heading marker
// (`#`..`######` + space) and a leading list bullet (`-`/`*`/`+` + space) are
// stripped — so `### Added` shows as `Added` and `- Worker drain…` as `Worker
// drain…`. This is a pure string transform on already-plain text: it strips the
// markers, it does NOT render markdown.
function releaseExcerpt(body: string | undefined, max = 300): string {
  if (!body) return "";
  const trimmed = body.trim();
  const lines = trimmed
    .split("\n")
    .map((l) => l.trim().replace(/^#{1,6}\s+/, "").replace(/^[-*+]\s+/, ""))
    .filter((l) => l !== "")
    .slice(0, 6);
  const joined = lines.join("\n");
  if (joined.length <= max) return joined;
  return `${joined.slice(0, max).trimEnd()}…`;
}

// UpdatesSettingsCard is the admin surface for the upstream release check (PRD #836
// M5) — the destination the sidebar pip points at. It self-loads GET
// /admin/release-check (admin-only by virtue of living on the admin-guarded page):
// the current-vs-latest version delta, the release name + date, a plain-text notes
// excerpt with a link OUT to the full GitHub notes, the copyable upgrade runbook, a
// "Check now" button (POST /admin/release-check), and the two runtime toggles. It is
// the ONLY surface that states the "disabled" (air-gap) / "error" (rate-limited or
// unreachable) / "never" (no check yet) status in words. It renders NO release
// markdown as HTML — the excerpt is a plain React text node.
function UpdatesSettingsCard() {
  const [status, setStatus] = useState<ReleaseCheckStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [checking, setChecking] = useState(false);
  // Which toggle is mid-save; null when idle. A save disables BOTH toggle inputs
  // (deliberate concurrent-write protection — see the `disabled` guards below), while
  // this value still records which of the two rows is the one being saved.
  const [savingToggle, setSavingToggle] = useState<"enabled" | "banner" | null>(null);
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const { release_check } = await api.getReleaseCheck();
      setStatus(release_check);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load release-check status");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const checkNow = async () => {
    setError("");
    setChecking(true);
    try {
      const { release_check } = await api.checkReleaseNow();
      setStatus(release_check);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to run the release check");
    } finally {
      setChecking(false);
    }
  };

  // Persist a toggle through the settings string-space (release_check_enabled /
  // release_check_banner_enabled), then re-read the status so the derived
  // status/delta reflect the new master-toggle state (off ⇒ "disabled").
  const saveToggle = async (which: "enabled" | "banner", next: boolean) => {
    setError("");
    setSavingToggle(which);
    try {
      const key = which === "enabled" ? "release_check_enabled" : "release_check_banner_enabled";
      await api.updateSettings({ [key]: String(next) });
      // The write succeeded — reflect it immediately so a failed re-read below does not
      // leave the controlled checkbox showing the pre-save value.
      setStatus((prev) =>
        prev === null
          ? prev
          : which === "enabled"
            ? { ...prev, release_check_enabled: next }
            : { ...prev, release_check_banner_enabled: next },
      );
      const { release_check } = await api.getReleaseCheck();
      setStatus(release_check);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save update settings");
    } finally {
      setSavingToggle(null);
    }
  };

  const copyRunbook = async () => {
    try {
      await navigator.clipboard.writeText(UPGRADE_RUNBOOK);
      setCopied(true);
      setTimeout(() => setCopied(false), 1400);
    } catch {
      // Clipboard unavailable (insecure context): the runbook stays visible to copy by hand.
    }
  };

  const deltaBadge = (s: ReleaseCheckStatus) => {
    if (s.security) return <Badge tone="danger">Security release</Badge>;
    if (s.update_available) return <Badge tone="brand">Update available</Badge>;
    return <Badge tone="ok">Up to date</Badge>;
  };

  return (
    <Card className="space-y-5">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <SectionTitle>Updates</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            uzi periodically asks GitHub for the latest{" "}
            <span className="font-medium text-fg">vtmocanu/uzi</span> release and reports whether this
            instance has fallen behind. This card is the only place the check&rsquo;s status is spelled
            out; the sidebar pip points here.
          </p>
        </div>
        {status && (
          <Button
            variant="secondary"
            size="sm"
            onClick={checkNow}
            disabled={checking || !status.release_check_enabled}
          >
            {checking ? "Checking…" : "Check now"}
          </Button>
        )}
      </div>

      {error && <Alert message={error} />}

      {loading ? (
        <Skeleton className="h-24 w-full" />
      ) : !status ? (
        // Initial load failed (status never populated). Show the error (rendered
        // above) plus a retry path instead of a permanent skeleton — the "Check
        // now" button is gated on `status`, so this is the only way back.
        <div className="space-y-3">
          {!error && <Alert message="Failed to load release-check status." />}
          <Button variant="secondary" size="sm" onClick={() => void load()} disabled={loading}>
            Retry
          </Button>
        </div>
      ) : (
        <>
          {/* Security callout — the loudest thing this card does, per the mockup. */}
          {status.security && (
            <Alert
              tone="danger"
              message={`${displayVersion(status.latest_tag ?? "")} is flagged as a security release — update at the next opportunity.`}
            />
          )}

          {/* Status-in-words for the non-ok states — the air-gap / rate-limited / never-run cases. */}
          {status.status === "disabled" && (
            <div className="rounded-xl border border-edge bg-raised/40 p-4 text-sm text-muted">
              Update checks are turned off (master toggle below). While off, uzi never contacts
              github.com — the air-gap / privacy state.
            </div>
          )}
          {status.status === "never" && (
            <div className="rounded-xl border border-edge bg-raised/40 p-4 text-sm text-muted">
              No release check has run yet. Use <span className="font-medium text-fg">Check now</span> to
              run one.
            </div>
          )}
          {status.status === "error" && (
            <Alert
              tone="warning"
              message={`Release check unavailable${status.message ? ` — ${status.message}` : " — the last check failed (rate-limited or unreachable)."}`}
            />
          )}

          {/* Version delta — always shows the running version; the arrow + latest when a check ran. */}
          {status.status === "ok" && (
            <div className="space-y-3 rounded-xl border border-edge bg-raised/40 p-4">
              <div className="flex flex-wrap items-center gap-3">
                <span className="font-mono text-xl font-semibold text-fg">
                  {displayVersion(status.running_version)}
                </span>
                {status.update_available && status.latest_tag && (
                  <>
                    <span className="text-muted">&rarr;</span>
                    <span
                      className={`font-mono text-xl font-semibold ${
                        status.security ? "text-danger" : "text-brand"
                      }`}
                    >
                      {displayVersion(status.latest_tag)}
                    </span>
                  </>
                )}
                {deltaBadge(status)}
              </div>

              {status.update_available ? (
                <>
                  {(status.latest_name || status.published_at) && (
                    <p className="text-sm text-muted">
                      {status.latest_name && (
                        <span className="font-medium text-fg">{status.latest_name}</span>
                      )}
                      {status.latest_name && status.published_at && " · "}
                      {status.published_at && `released ${formatDay(status.published_at) ?? status.published_at}`}
                    </p>
                  )}

                  {releaseExcerpt(status.body) && (
                    <p className="whitespace-pre-wrap text-sm text-muted">{releaseExcerpt(status.body)}</p>
                  )}

                  {status.notes_url && isHttpsUrl(status.notes_url) && (
                    <a
                      href={status.notes_url}
                      target="_blank"
                      rel="noopener noreferrer"
                      className="text-sm text-info hover:underline"
                    >
                      Full notes on GitHub &#8599;
                    </a>
                  )}

                  {/* Copyable upgrade runbook. */}
                  <div className="relative rounded-lg border border-edge bg-ink p-3">
                    <button
                      type="button"
                      onClick={copyRunbook}
                      className="absolute right-2 top-2 rounded-md border border-edge bg-raised px-2 py-0.5 text-[11px] text-muted hover:text-fg"
                    >
                      {copied ? "Copied" : "Copy"}
                    </button>
                    <pre className="overflow-x-auto whitespace-pre font-mono text-xs leading-relaxed text-fg">
                      {UPGRADE_RUNBOOK}
                    </pre>
                  </div>
                </>
              ) : (
                <p className="text-sm text-muted">You&rsquo;re running the latest release.</p>
              )}
            </div>
          )}

          {/* aria-live region: a "Check now" / toggle refresh mutates the text
              inside this persistent container, so a screen reader is notified of
              the async update. Single visible copy — no duplicate info. */}
          <div aria-live="polite">
            {status.checked_at && (
              <p className="text-xs text-faint">Checked {formatAgo(status.checked_at)}.</p>
            )}
          </div>

          {/* The two runtime toggles — persisted through the settings string-space. */}
          <div className="space-y-3 border-t border-edge pt-4">
            <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={status.release_check_enabled}
                disabled={savingToggle !== null}
                onChange={(e) => void saveToggle("enabled", e.target.checked)}
                className="h-4 w-4 rounded border-edge accent-brand"
              />
              Enable update checks (contact github.com for the latest release)
            </label>
            <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={status.release_check_banner_enabled}
                disabled={savingToggle !== null}
                onChange={(e) => void saveToggle("banner", e.target.checked)}
                className="h-4 w-4 rounded border-edge accent-brand"
              />
              Show the escalation banner for far-behind / security releases
            </label>
          </div>
        </>
      )}
    </Card>
  );
}

// SummarySettingsCard is the admin surface for the run-summary model (PRD #362
// Decision 8): the cheap model the inline plain-English run-summary generator runs
// on. It mirrors the Judge model field exactly — a raw free-text model alias input —
// but the value rides the ISSUE-run claim, not the judge claim, and a per-user
// override wins where set. Saved independently through the same settings PUT.
function SummarySettingsCard({
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
      setError(err instanceof ApiError ? err.message : "Failed to save run summary settings");
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

// agentSourceActionMeta maps a staged-diff action to its review chip: a tone, a
// short label, and a one-line description of what Approve would do for that role
// (PRD #602 M5). The action set is the server's closed enum; an unknown action
// falls back to a neutral chip so a future action still renders rather than crashing.
function agentSourceActionMeta(action: string): { tone: BadgeTone; label: string; detail: string } {
  switch (action) {
    case "add":
      return { tone: "ok", label: "Add", detail: "New role — added as a shared template." };
    case "override":
      return { tone: "info", label: "Override", detail: "Replaces the shipped builtin body with the source's." };
    case "conflict":
      return {
        tone: "danger",
        label: "Conflict",
        detail: "Name collides with an admin template — skipped, never overwritten.",
      };
    case "remove":
      return {
        tone: "warning",
        label: "Remove",
        detail: "Gone from the source — its synced role is removed (an overridden builtin resets to shipped).",
      };
    case "unchanged":
      return { tone: "neutral", label: "Unchanged", detail: "Already matches — nothing to apply." };
    default:
      // No diff entry for this name (a role-only row: parsed/skipped but nothing to
      // apply). "not applied" is clearer than a bare "—" chip; a future non-empty
      // action string still renders as itself.
      return { tone: "neutral", label: action || "not applied", detail: "" };
  }
}

// AgentSourceStagedReview renders the pending staged snapshot: a count summary, the
// per-role diff (action chip + collapsible sanitized body), and the Approve gate.
// It is a child so the card body stays readable; the parent owns the apply action.
function AgentSourceStagedReview({
  staged,
  applying,
  onApprove,
}: {
  staged: AgentSourceStaged;
  applying: boolean;
  onApprove: () => void;
}) {
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const toggle = (name: string) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });

  // The diff carries the per-name action; the roles carry the parsed body. Join by
  // name across BOTH so each review row shows what it can: a "remove" has a diff
  // entry and no role, a skipped/failed role has a role and no diff entry, and both
  // must be visible. Diff order first (the applied actions), then any role-only names.
  const roleByName = new Map(staged.roles.map((r) => [r.name, r]));
  const actionByName = new Map(staged.diff.map((d) => [d.name, d]));
  const names = Array.from(new Set([...staged.diff.map((d) => d.name), ...staged.roles.map((r) => r.name)]));

  return (
    <div className="space-y-4 rounded-xl border border-plan/40 bg-plan/5 p-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <Badge tone="plan" dot pulse>
            review needed
          </Badge>
          <span className="text-sm font-medium text-fg">Staged changes from the source</span>
        </div>
        <div className="flex items-center gap-3 text-xs text-faint">
          <span>
            <strong className="text-fg">{staged.counts.staged}</strong> staged
          </span>
          <span>
            <strong className="text-fg">{staged.counts.changed}</strong> changed
          </span>
          <span className={staged.counts.failed > 0 ? "text-warn" : undefined}>
            <strong className={staged.counts.failed > 0 ? "text-warn" : "text-fg"}>{staged.counts.failed}</strong>{" "}
            failed
          </span>
        </div>
      </div>

      <p className="text-xs text-muted">
        Fetched{" "}
        {staged.fetched_at ? new Date(staged.fetched_at).toLocaleString() : "just now"} at{" "}
        <code className="rounded bg-raised px-1 py-0.5 font-mono text-fg">{staged.fetched_sha.slice(0, 10)}</code>.
        Nothing below is applied to your agents until you approve it.
      </p>

      <ul className="space-y-2">
        {names.map((name) => {
          const diff = actionByName.get(name);
          const role = roleByName.get(name);
          const meta = agentSourceActionMeta(diff?.action ?? "");
          const isOpen = expanded.has(name);
          const hasBody = Boolean(role?.prompt_body);
          return (
            <li key={name} className="rounded-lg border border-edge bg-surface">
              <div className="flex flex-wrap items-center gap-2 px-3 py-2">
                <Badge tone={meta.tone}>{meta.label}</Badge>
                <code className="font-mono text-sm text-fg">{name}</code>
                {role && !role.ok && (
                  <Badge tone="warning" title={`Skipped: ${role.reason ?? "unparseable"}`}>
                    skipped
                  </Badge>
                )}
                <span className="min-w-0 flex-1 truncate text-xs text-faint">
                  {diff?.detail || meta.detail || role?.reason}
                </span>
                {hasBody && (
                  <Button variant="ghost" onClick={() => toggle(name)} aria-expanded={isOpen}>
                    {isOpen ? "Hide body" : "Show body"}
                  </Button>
                )}
              </div>
              {/* Approval-surface honesty: when the server stripped control/bidi/format
                  chars for display, the preview under-represents the raw body. Flag it
                  ALWAYS (not only when expanded) so the admin never approves blind. */}
              {role?.body_sanitized && (
                <p className="flex items-start gap-1.5 border-t border-edge px-3 py-2 text-xs text-warn">
                  <span aria-hidden="true">⚠</span>
                  <span>
                    Hidden formatting characters were removed from this preview — the raw body is what will run.
                  </span>
                </p>
              )}
              {isOpen && role && (
                <div className="border-t border-edge px-3 py-2">
                  {(role.description || role.model || (role.tools && role.tools.length > 0)) && (
                    <dl className="mb-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
                      {role.description && (
                        <>
                          <dt className="text-faint">Description</dt>
                          <dd className="text-muted">{role.description}</dd>
                        </>
                      )}
                      <dt className="text-faint">Model</dt>
                      <dd className="text-muted">{role.model || "inherit"}</dd>
                      <dt className="text-faint">Tools</dt>
                      <dd className="font-mono text-muted">
                        {role.tools && role.tools.length > 0 ? role.tools.join(", ") : "inherit all"}
                      </dd>
                    </dl>
                  )}
                  {/* prompt_body is ALREADY server-sanitized (termsafe.SanitizeTTY) — a
                      plain text node, never dangerouslySetInnerHTML. */}
                  <pre className="max-h-64 overflow-auto whitespace-pre-wrap rounded-lg border border-edge bg-ink p-3 text-xs text-muted">
                    {role.prompt_body}
                  </pre>
                </div>
              )}
            </li>
          );
        })}
      </ul>

      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-xs text-muted">
          Approving applies every change above at once and records it as the last-applied snapshot.
        </p>
        <Button onClick={onApprove} disabled={applying}>
          {applying ? "Applying…" : "Approve & apply"}
        </Button>
      </div>
    </div>
  );
}

// AgentSourceSettingsCard is the admin surface for the configurable agent-source
// repo (PRD #602 M5). It self-loads a dedicated endpoint (config + last-sync status
// + a STAGED snapshot). The trust model is the whole
// point of the copy: a sync only STAGES what the source repo contains; nothing
// reaches a run until an admin reviews the diff and approves it. A fresh install
// has no URL and is disabled — the card reads as "nothing configured" in that state.
// The one-click preset that follows uzi's canonical roster (PRD #702 M3). The URL and
// folder are the literal published shape of the skills repo; the ref is NOT hardcoded —
// it is resolved to the latest tag at click time via the ls-remote endpoint. Enabling
// the source still needs both URL and ref non-empty, which the resolved tag satisfies.
const SKILLS_PRESET_URL = "https://github.com/vtmocanu/skills";
const SKILLS_PRESET_FOLDER = "product-agents/";

function AgentSourceSettingsCard() {
  const [view, setView] = useState<AgentSourceView | null>(null);
  const [loading, setLoading] = useState(true);
  const [url, setUrl] = useState("");
  const [ref, setRef] = useState("");
  // Repo-relative subfolder role files are read from (PRD #702 M1). The server
  // resolves empty/unset to ".claude/agents", so the input defaults to that.
  const [folder, setFolder] = useState(".claude/agents");
  const [enabled, setEnabled] = useState(false);
  const [interval, setIntervalValue] = useState("1h");
  // Write-only credential input: blank means "leave the stored one unchanged".
  const [credential, setCredential] = useState("");
  const [savingConfig, setSavingConfig] = useState(false);
  const [syncing, setSyncing] = useState(false);
  const [applying, setApplying] = useState(false);
  // True while an "update available" check ls-remotes the configured source (PRD #702 M4).
  const [checking, setChecking] = useState(false);
  // True while a "bump pin" write is in flight (PRD #702 M4).
  const [bumping, setBumping] = useState(false);
  // True while the Preset button is resolving the latest tag from the source (PRD #702 M3).
  const [resolving, setResolving] = useState(false);
  // Preset outcome rendered INLINE in the preset block, co-located with the button
  // (PRD #702 M3 UX fix). The card-level error/notice Alerts are pinned at the TOP of
  // this long card, ~793px above the Preset button — on a resolve failure their
  // explanation scrolls off-screen, so the preset's own feedback lives beside the action.
  const [presetMsg, setPresetMsg] = useState<{ tone: "success" | "warning" | "danger"; text: string } | null>(
    null,
  );
  // Check/Bump outcome rendered INLINE beside the Check-for-updates / Bump-pin controls
  // in the Sync-status block (PRD #702 M4 UX fix). Those controls live ~228px below the
  // card-TOP error/notice Alerts, so a "no update found" success or a bump confirmation
  // routed through the card-top banner would render off-screen — this local state keeps
  // their feedback co-located with the buttons. The shared Alert sets role=alert for
  // danger and role=status otherwise, so it stays announced.
  const [updateMsg, setUpdateMsg] = useState<{ tone: "success" | "warning" | "danger"; text: string } | null>(
    null,
  );
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  // resetForm mirrors the SAVED server config into the controlled inputs. It runs
  // ONLY on the initial load and after an explicit Save — never on a Sync-now or
  // Approve refresh, which act on the saved config and must preserve whatever the
  // admin has typed (a refresh that reset the inputs would silently discard edits).
  const resetForm = useCallback((v: AgentSourceView) => {
    setView(v);
    setUrl(v.config.url);
    setRef(v.config.ref);
    setFolder(v.config.folder || ".claude/agents");
    setEnabled(v.config.enabled);
    setIntervalValue(v.config.interval || "1h");
  }, []);

  // refreshView re-reads only the status + staged panels, leaving the form inputs
  // (and any unsaved edits) untouched. Used after Sync-now and Approve.
  const refreshView = useCallback(async () => {
    const { agent_source } = await api.getAgentSource();
    setView(agent_source);
  }, []);

  const load = useCallback(async () => {
    try {
      const { agent_source } = await api.getAgentSource();
      resetForm(agent_source);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load agent-source settings");
    } finally {
      setLoading(false);
    }
  }, [resetForm]);

  useEffect(() => {
    void load();
  }, [load]);

  // Preset: fill the URL + folder for uzi's canonical roster and resolve its latest tag
  // at click time (PRD #702 M3). The URL/folder are filled IMMEDIATELY so a resolve
  // failure (github.com not on this deployment's allowlist, or an unreachable source)
  // still leaves the admin a filled form to edit and Save. type="button" so it does not
  // submit the surrounding form. The admin still reviews and Saves.
  const fillPreset = async () => {
    // Preset feedback renders INLINE below (presetMsg), not through the card-level
    // error/notice Alerts at the top of the card — clear all three at the start so a
    // prior Save/Sync banner and a stale preset outcome both go away.
    setPresetMsg(null);
    setError("");
    setNotice("");
    setResolving(true);
    setUrl(SKILLS_PRESET_URL);
    setFolder(SKILLS_PRESET_FOLDER);
    try {
      const { latest_ref } = await api.resolveAgentSourceLatest(SKILLS_PRESET_URL);
      if (latest_ref.trim() === "") {
        // The source is reachable but publishes no semver tag yet. Leave URL+folder
        // filled and the ref as-is so the admin can Save or set a ref by hand.
        setPresetMsg({
          tone: "warning",
          text:
            "Preset filled the URL and folder, but the source publishes no semver tag yet — " +
            "it may not tag releases. Set a ref by hand or Save once it does.",
        });
      } else {
        setRef(latest_ref);
        setPresetMsg({
          tone: "success",
          text: `Preset filled — resolved latest tag ${latest_ref}. Review and Save to apply.`,
        });
      }
    } catch (err) {
      // Graceful: URL+folder stay filled so the admin can still edit/Save.
      setPresetMsg({
        tone: "danger",
        text:
          err instanceof ApiError
            ? err.message
            : "Could not resolve the latest tag. The URL and folder are filled — set a ref by hand or try again.",
      });
    } finally {
      setResolving(false);
    }
  };

  const saveConfig = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    if (enabled && url.trim() === "") {
      setError("Set a repository URL before enabling the agent source.");
      return;
    }
    setSavingConfig(true);
    try {
      const payload: UpdateSettingsPayload = {
        agent_source_repo_url: url.trim(),
        agent_source_ref: ref.trim(),
        agent_source_folder: folder.trim(),
        agent_source_enabled: String(enabled),
        agent_source_interval: interval.trim(),
      };
      // Send the credential only when the admin typed one — an empty field leaves the
      // stored credential untouched (write-only, like the Slack tokens).
      if (credential.trim() !== "") payload.agent_source_credential = credential;
      await api.updateSettings(payload);
      setCredential("");
      // updateSettings returns the settings envelope, not the agent-source view —
      // re-read so the status/credential-configured/staged panels reflect the save.
      await load();
      setNotice("Agent-source settings saved.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save agent-source settings");
    } finally {
      setSavingConfig(false);
    }
  };

  const syncNow = async () => {
    setError("");
    setNotice("");
    setSyncing(true);
    try {
      const { agent_source } = await api.syncAgentSource();
      // Refresh the status + staged panels only — the sync ran against the SAVED
      // config, so the admin's in-progress form edits must survive untouched.
      setView(agent_source);
      const st = agent_source.status;
      if (st.last_sync_status === "error") {
        setError(st.last_sync_error || "The sync failed. Check the repository URL, ref, and credential.");
      } else if (agent_source.staged?.pending) {
        setNotice("Sync complete. Review the staged changes below, then approve to apply.");
      } else {
        setNotice("Sync complete. The source matches what is applied — nothing to review.");
      }
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Sync failed");
    } finally {
      setSyncing(false);
    }
  };

  const approve = async () => {
    const staged = view?.staged;
    if (!staged) return;
    setError("");
    setNotice("");
    setApplying(true);
    try {
      const { result } = await api.applyAgentSource(staged.fetched_sha);
      await refreshView();
      setNotice(
        `Applied ${result.applied} change${result.applied === 1 ? "" : "s"}` +
          (result.deprovisioned > 0 ? `, removed ${result.deprovisioned}` : "") +
          (result.conflicts > 0 ? `, skipped ${result.conflicts} conflict${result.conflicts === 1 ? "" : "s"}` : "") +
          ".",
      );
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        // The staged snapshot changed since it was reviewed (a concurrent restage, or
        // it was already applied). Re-read so the admin reviews the current diff, and
        // say so plainly rather than retrying blind.
        await refreshView();
        setError("The staged snapshot changed since you reviewed it. Review the refreshed diff below, then approve again.");
      } else {
        setError(err instanceof ApiError ? err.message : "Failed to apply the staged changes");
      }
    } finally {
      setApplying(false);
    }
  };

  // Check for updates: ls-remote the SAVED configured source and refresh the derived
  // update-available signal (PRD #702 M4). Like Sync-now it acts on the saved config and
  // leaves the form inputs (and any unsaved edits) untouched. The update check reuses the
  // last_sync_error slot for its own error message. The outcome renders INLINE beside the
  // control (updateMsg), not through the card-top banner, so the "no update found" success
  // has a visible on-screen signal rather than one 228px above the button.
  const checkForUpdates = async () => {
    setError("");
    setNotice("");
    setUpdateMsg(null);
    setChecking(true);
    try {
      const { agent_source } = await api.updateCheckAgentSource();
      setView(agent_source);
      const st = agent_source.status;
      if (st.last_sync_status === "error") {
        setUpdateMsg({
          tone: "danger",
          text:
            st.last_sync_error || "The update check failed. Check the repository URL, ref, and credential.",
        });
      } else if (st.update_available === true) {
        setUpdateMsg({ tone: "success", text: "Update check complete." });
      } else {
        setUpdateMsg({ tone: "success", text: "No update available — you're on the latest." });
      }
    } catch (err) {
      setUpdateMsg({ tone: "danger", text: err instanceof ApiError ? err.message : "Update check failed" });
    } finally {
      setChecking(false);
    }
  };

  // Bump pin: write the newer tag as the saved ref via the generic settings PUT (PRD #702
  // M4). This ONLY moves the pin — the admin still Syncs → reviews → approves to apply.
  // After the write, refreshView() re-reads the status + staged panels (NOT the form) so
  // the admin's unsaved edits to the other fields survive, then only the ref input is set
  // to the bumped tag. refreshView's GET re-derives the badge against the now-saved ref
  // (the server sees the just-written ref == latest_ref), so the derived update signal
  // self-clears. The outcome renders INLINE beside the control (updateMsg), not through the
  // card-top banner, so the confirmation is on-screen next to the button.
  const bumpPin = async () => {
    const latest = view?.status.latest_ref;
    if (!latest) return;
    setError("");
    setNotice("");
    setUpdateMsg(null);
    setBumping(true);
    try {
      await api.updateSettings({ agent_source_ref: latest });
      await refreshView();
      setRef(latest);
      setUpdateMsg({
        tone: "success",
        text: `Pinned ref updated to ${latest}. Sync to stage it, then review and approve to apply.`,
      });
    } catch (err) {
      setUpdateMsg({
        tone: "danger",
        text: err instanceof ApiError ? err.message : "Failed to update the pinned ref",
      });
    } finally {
      setBumping(false);
    }
  };

  const status = view?.status;
  // Sync-now acts on the SAVED server config, so its enabled state reads that config
  // (view.config.url), never the local `url` input — otherwise typing a URL without
  // saving would arm a sync against the still-empty stored config.
  const configured = (view?.config.url ?? "").trim() !== "";
  // Whether the form holds unsaved edits vs. the saved config. Sync-now stays enabled
  // while dirty (a manual refresh shouldn't force saving unrelated edits first), but a
  // note makes clear it runs against the saved config, not the in-progress edits.
  const dirty =
    view !== null &&
    (url !== view.config.url ||
      ref !== view.config.ref ||
      folder !== (view.config.folder || ".claude/agents") ||
      enabled !== view.config.enabled ||
      interval !== (view.config.interval || "1h") ||
      credential.trim() !== "");
  const syncTone: BadgeTone =
    status?.last_sync_status === "ok" ? "ok" : status?.last_sync_status === "error" ? "danger" : "neutral";
  // Update-availability signal (PRD #702 M4), DERIVED server-side and read straight from
  // the status. `updateAvailable` gates the whole badge; a non-empty `latest_ref` is the
  // tag-pinned "newer tag" case (Bump pin applies), an empty one is the branch "moved"
  // signal (Decision 6). `stagedPending` distinguishes "source moved past what's running"
  // from "a change is already staged awaiting approval" (#602's staged state).
  const updateAvailable = status?.update_available === true;
  const latestRef = (status?.latest_ref ?? "").trim();
  const stagedPending = view?.staged?.pending === true;

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Agent source</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Point uzi at a Git repository of agent definitions to keep your team's agents in step with a shared,
          version-controlled source. A sync only <strong className="text-fg">stages</strong> what the source
          contains &mdash; you review the exact changes and approve them before anything reaches a run.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      {loading ? (
        <Skeleton className="h-24" />
      ) : (
        <>
          {/* Status panel — reads "Never synced" on a fresh install rather than empty. */}
          <div className="space-y-2 rounded-xl border border-edge bg-raised/40 p-4">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <span className="text-sm font-medium text-fg">Sync status</span>
              <Badge tone={syncTone} dot>
                {status?.last_sync_status === "ok"
                  ? "healthy"
                  : status?.last_sync_status === "error"
                    ? "error"
                    : "never synced"}
              </Badge>
            </div>
            {status?.last_sync_status === "error" && status.last_sync_error && (
              <p className="text-xs text-warn">{status.last_sync_error}</p>
            )}
            <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-xs">
              <dt className="text-faint">Last synced</dt>
              <dd className="text-muted">
                {status?.last_sync_at ? new Date(status.last_sync_at).toLocaleString() : "never"}
                {status?.last_sync_sha && (
                  <code className="ml-2 rounded bg-raised px-1 py-0.5 font-mono text-fg">
                    {status.last_sync_sha.slice(0, 10)}
                  </code>
                )}
              </dd>
              <dt className="text-faint">Last applied</dt>
              <dd className="text-muted">
                {status?.last_applied_at ? new Date(status.last_applied_at).toLocaleString() : "never"}
                {status?.last_applied_sha && (
                  <code className="ml-2 rounded bg-raised px-1 py-0.5 font-mono text-fg">
                    {status.last_applied_sha.slice(0, 10)}
                  </code>
                )}
              </dd>
              {status?.counts && (
                <>
                  <dt className="text-faint">Last sync counts</dt>
                  <dd className="text-muted">
                    {status.counts.staged} staged · {status.counts.changed} changed ·{" "}
                    <span className={status.counts.failed > 0 ? "text-warn" : undefined}>
                      {status.counts.failed} failed
                    </span>
                  </dd>
                </>
              )}
            </dl>
            {/* "Update available" signal (PRD #702 M4), derived server-side from the last
                update check. A non-empty latest_ref is the tag-pinned newer-tag case (Bump
                pin below applies it); an empty one is the branch "moved" signal. The copy
                never implies the update is already applied — it must still be Synced,
                reviewed, and approved. When a change is ALSO already staged, a separate note
                distinguishes "source moved past what's running" from "staged awaiting
                approval" (#602's staged state). */}
            {updateAvailable && (
              <div className="flex flex-wrap items-center gap-x-2 gap-y-1 pt-1">
                <Badge tone="warning" dot wrap>
                  {latestRef ? `Update available: ${latestRef}` : "Source moved"}
                </Badge>
                {status?.update_checked_at && (
                  <span className="text-xs text-faint">
                    checked {new Date(status.update_checked_at).toLocaleString()}
                  </span>
                )}
                {/* The branch "moved" case (empty latest_ref) has no Bump-pin control — a
                    branch pin is not a tag — so it needs a one-line what-next caption. */}
                {!latestRef && (
                  <span className="w-full text-xs text-faint">
                    The tracked branch advanced — Sync now to stage it.
                  </span>
                )}
                {stagedPending && (
                  <span className="w-full text-xs text-faint">
                    A change is staged for approval below — review and approve it to apply.
                  </span>
                )}
              </div>
            )}
            <div className="flex flex-wrap items-center gap-2 pt-1">
              <Button variant="ghost" onClick={syncNow} disabled={syncing || !configured}>
                {syncing ? "Syncing…" : "Sync now"}
              </Button>
              <Button variant="ghost" onClick={checkForUpdates} disabled={checking || !configured}>
                {checking ? "Checking…" : "Check for updates"}
              </Button>
              {updateAvailable && latestRef && (
                <Button variant="secondary" onClick={bumpPin} disabled={bumping}>
                  {bumping ? "Bumping…" : `Bump pin to ${latestRef}`}
                </Button>
              )}
              {!configured ? (
                <span className="ml-2 text-xs text-faint">Save a repository URL below to sync.</span>
              ) : dirty ? (
                <span className="ml-2 text-xs text-faint">
                  Sync now uses the saved configuration, not your unsaved edits.
                </span>
              ) : null}
            </div>
            {/* Check/Bump outcome, INLINE beside the controls (PRD #702 M4 UX fix). The
                shared Alert carries role="alert" for danger and role="status" otherwise,
                so a "no update found" success or a bump confirmation is announced here,
                not routed to the card-top banner ~228px above and off-screen. */}
            {updateMsg && <Alert tone={updateMsg.tone} message={updateMsg.text} />}
          </div>

          {/* Staged-diff review + approve — only when a snapshot is pending. */}
          {view?.staged?.pending && (
            <AgentSourceStagedReview staged={view.staged} applying={applying} onApprove={approve} />
          )}

          <form onSubmit={saveConfig} className="space-y-4">
            <div className="space-y-1.5 rounded-lg border border-edge bg-raised/40 p-3">
              <Button type="button" variant="secondary" size="sm" onClick={fillPreset} disabled={resolving}>
                {resolving ? "Resolving…" : "Use uzi skills preset"}
              </Button>
              <p className="text-xs text-faint">
                Points the source at{" "}
                <code className="rounded bg-raised px-1 py-0.5 font-mono text-fg">github.com/vtmocanu/skills</code>{" "}
                and folder{" "}
                <code className="rounded bg-raised px-1 py-0.5 font-mono text-fg">product-agents/</code>, resolving
                the latest tag now. Requires <code className="rounded bg-raised px-1 py-0.5 font-mono text-fg">github.com</code>{" "}
                on this deployment&rsquo;s agent-source allowlist and a public source. Review the filled values and
                Save to apply.
              </p>
              {/* Preset outcome, INLINE beside the button (PRD #702 M3 UX fix). The
                  shared Alert carries role="alert" for danger and role="status"
                  otherwise, so a resolve failure — off-allowlist or no semver tag — is
                  announced right here, not 793px up in the card-level banner. */}
              {presetMsg && <Alert tone={presetMsg.tone} message={presetMsg.text} />}
            </div>

            <div className="space-y-1.5">
              <Field label="Repository URL">
                <Input
                  value={url}
                  autoComplete="off"
                  placeholder="https://github.com/your-org/agents.git"
                  onChange={(e) => setUrl(e.target.value)}
                />
              </Field>
              <p className="text-xs text-faint">
                An https Git URL on the agent-source allowlist. Leave empty to disable the source entirely.
              </p>
            </div>

            <Field label="Ref">
              <Input
                value={ref}
                maxLength={255}
                autoComplete="off"
                placeholder="v1.0.0"
                onChange={(e) => setRef(e.target.value)}
              />
            </Field>

            <div className="space-y-1.5">
              <Field label="Source folder">
                <Input
                  value={folder}
                  maxLength={255}
                  autoComplete="off"
                  placeholder=".claude/agents"
                  onChange={(e) => setFolder(e.target.value)}
                />
              </Field>
              <p className="text-xs text-faint">
                The repo-relative subfolder to read role files from. Leave empty for the default{" "}
                <code className="rounded bg-raised px-1 py-0.5 font-mono text-fg">.claude/agents</code>.
              </p>
            </div>

            <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={enabled}
                onChange={(e) => setEnabled(e.target.checked)}
                className="h-4 w-4 rounded border-edge accent-brand"
              />
              Sync automatically on the interval below
            </label>

            <div className="space-y-1.5">
              <Field label="Sync interval">
                <Input
                  value={interval}
                  maxLength={32}
                  autoComplete="off"
                  placeholder="1h"
                  onChange={(e) => setIntervalValue(e.target.value)}
                />
              </Field>
              <p className="text-xs text-faint">
                How often the source is re-checked, as a Go duration (e.g.{" "}
                <code className="rounded bg-raised px-1 py-0.5 text-fg">1h</code>). Each auto-sync still only
                stages — approval stays manual.
              </p>
            </div>

            <div className="space-y-1.5">
              <Field label="Access credential">
                <Input
                  type="password"
                  value={credential}
                  autoComplete="off"
                  placeholder={
                    view?.config.credential_configured ? "Configured — type to replace" : "For a private repo"
                  }
                  onChange={(e) => setCredential(e.target.value)}
                />
              </Field>
              <p className="text-xs text-faint">
                {view?.config.credential_configured ? (
                  <>
                    A credential is <span className="font-medium text-fg">configured</span>. It is never shown;
                    type a new value to replace it, or leave this blank to keep it.
                  </>
                ) : (
                  <>
                    No credential is set. A public repo needs none; for a private repo, paste a token here (stored
                    sealed, never shown again).
                  </>
                )}
              </p>
            </div>

            <Button type="submit" disabled={savingConfig}>
              {savingConfig ? "Saving…" : "Save agent-source settings"}
            </Button>
          </form>
        </>
      )}
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

// parseAllowlist splits the comma-separated repo-id allowlist into a deduped id
// list, dropping empty tokens. normalizeAllowlist canonicalizes for comparison
// (deduped + sorted) so dirty-checking is order/dup-insensitive.
function parseAllowlist(value: string): string[] {
  return value
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
}
function normalizeAllowlist(ids: string[]): string {
  return [...new Set(ids)].sort().join(",");
}

// DockerAllowlistCard is the admin surface for the docker-worker repo allowlist
// (PRD #89 M-allow). A docker-capable worker reaches a root Docker daemon, so it may
// only claim runs for repos an admin has explicitly trusted; the gate binds at claim
// time. This is a security control: an EMPTY list is fail-closed (a docker worker
// then claims no repo-bearing run), and non-docker workers are entirely unaffected.
//
// The stored value is a comma-separated list of repo UUIDs, but admins pick repos by
// path — the card resolves paths from the repos API and writes the ids. The repos API
// (`listRepos`) is scoped to the CALLING admin's own repos, and docker_repo_allowlist
// is a GLOBAL setting that can hold repo ids from OTHER admins. So stored ids that do
// not resolve to a repo this admin can see are PRESERVED verbatim on save (surfaced as
// a labeled count), never dropped — otherwise admin A saving would silently clobber
// admin B's allowlisted repo just because A can't see it (auditor Low, PRD #89).
function DockerAllowlistCard({
  settings,
  sources,
  onSaved,
}: {
  settings: AppSettings;
  sources: Record<string, SettingSource>;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [repos, setRepos] = useState<Repo[]>([]);
  // reposLoaded gates the out-of-visibility indicator: until listRepos succeeds we
  // cannot know which stored ids are genuinely outside this admin's visibility vs
  // simply not fetched yet, so the count would be spurious (every id looks "unknown"
  // while repos is empty). reposError distinguishes a failed fetch from a genuinely
  // empty repo list so the copy can differ.
  const [reposLoaded, setReposLoaded] = useState(false);
  const [reposError, setReposError] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(
    () => new Set(parseAllowlist(settings.docker_repo_allowlist)),
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const isEnv = sources["docker_repo_allowlist"] === "env";

  useEffect(() => {
    api
      .listRepos()
      .then(({ repos }) => {
        setRepos(repos);
        setReposLoaded(true);
      })
      .catch(() => setReposError(true));
  }, []);

  const toggle = (id: string) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  // Selected ids that resolve to no repo THIS admin can see (listRepos is per-user):
  // another admin's allowlisted repo, or a deleted one. Kept in `selected` and written
  // back untouched on save so a global setting is never clobbered by an admin who
  // simply can't see the entry.
  const knownIds = new Set(repos.map((r) => r.id));
  const outsideVisibilityCount = [...selected].filter((id) => !knownIds.has(id)).length;

  const dirty =
    normalizeAllowlist([...selected]) !== normalizeAllowlist(parseAllowlist(settings.docker_repo_allowlist));

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    if (isEnv) return;
    // Persist EVERY selected id, including ones outside this admin's visibility — a
    // global setting must not be clobbered by an admin who cannot see another admin's
    // repos. Only the checkboxes this admin can see change; the rest ride through.
    const value = normalizeAllowlist([...selected]);
    setBusy(true);
    try {
      const resp = await api.updateSettings({ docker_repo_allowlist: value });
      onSaved(resp);
      setSelected(new Set(parseAllowlist(resp.settings.docker_repo_allowlist)));
      setNotice("Docker worker repo allowlist saved.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save the docker repo allowlist");
    } finally {
      setBusy(false);
    }
  };

  const selectedKnown = [...selected].filter((id) => knownIds.has(id)).length;

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Docker worker repo allowlist</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Docker-capable workers reach a root Docker daemon, so they may only run repos you
          explicitly trust here. Only the repos ticked below can be claimed by a docker worker;
          every other repo waits for a non-docker worker.
        </p>
        <p className="mt-2 text-sm text-warn">
          An <strong className="text-fg">empty list is fail-closed</strong> — a docker worker then
          claims no repo-bearing run. Non-docker workers are unaffected by this list.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}
      {isEnv && (
        <Alert tone="info" message="This setting is fixed by an environment variable and cannot be changed here." />
      )}

      <form onSubmit={save} className="space-y-4">
        <Field label={`Trusted repositories (${selectedKnown} selected)`}>
          {reposError ? (
            <p className="text-sm text-warn">
              Could not load repositories. The stored allowlist is preserved unchanged; reload to edit it.
            </p>
          ) : !reposLoaded ? (
            <p className="text-sm text-faint">Loading repositories…</p>
          ) : repos.length === 0 ? (
            <p className="text-sm text-faint">No connected repositories.</p>
          ) : (
            <div className="max-h-64 space-y-1 overflow-y-auto rounded border border-edge p-2">
              {repos.map((r) => (
                <label
                  key={r.id}
                  className="flex cursor-pointer select-none items-center gap-2 rounded px-1 py-1 text-sm hover:bg-raised"
                >
                  <input
                    type="checkbox"
                    checked={selected.has(r.id)}
                    disabled={isEnv}
                    onChange={() => toggle(r.id)}
                    className="h-4 w-4 rounded border-edge accent-brand"
                  />
                  <span className="truncate text-fg">{r.path_with_namespace}</span>
                </label>
              ))}
            </div>
          )}
        </Field>

        {/* Gated on reposLoaded: an unresolved id is only meaningfully "outside your
            visibility" once the repo list actually loaded — during loading or after a
            fetch failure every id looks unknown, which would be a false alarm. Purely
            informational (these ids are preserved on save), so it renders regardless of
            the dirty state without promising any removal. */}
        {reposLoaded && outsideVisibilityCount > 0 && (
          <p className="text-xs text-faint">
            {outsideVisibilityCount} allowlisted repo{outsideVisibilityCount === 1 ? "" : "s"} outside your
            visibility (preserved) — repos on other admins&rsquo; connections, or since removed. They stay in
            the allowlist when you save.
          </p>
        )}

        <Button type="submit" disabled={!dirty || busy || isEnv}>
          {busy ? "Saving…" : "Save repo allowlist"}
        </Button>
      </form>
    </Card>
  );
}

// CapabilitySchedulingCard is the admin kill-switch for capability-aware scheduling
// (PRD #84 M2, Decision 13). Default ON. It follows the bool-default-true toggle
// precedent (health_enabled) and sends only capability_aware_scheduling on change.
// Turning it OFF is an explicit, documented degraded mode: runs claim best-effort and
// a capability mismatch degrades to the existing mid-run failure. It does NOT disable
// the docker repo allowlist (that stays enforced regardless of this flag).
function CapabilitySchedulingCard({
  settings,
  sources,
  onSaved,
}: {
  settings: AppSettings;
  sources: Record<string, SettingSource>;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [enabled, setEnabled] = useState(settings.capability_aware_scheduling === "true");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const isEnv = sources["capability_aware_scheduling"] === "env";

  const dirty = (enabled ? "true" : "false") !== settings.capability_aware_scheduling;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    if (isEnv || !dirty) return;
    setBusy(true);
    try {
      const resp = await api.updateSettings({
        capability_aware_scheduling: enabled ? "true" : "false",
      });
      onSaved(resp);
      setEnabled(resp.settings.capability_aware_scheduling === "true");
      setNotice("Capability-aware scheduling setting saved.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save the capability scheduling setting");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Capability-aware scheduling</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Capability-aware scheduling routes each run to a worker that can run it (e.g. a
          Docker-needing run only to a Docker worker). Turning this OFF reverts to best-effort
          claiming: a run may be claimed by a worker that lacks a required capability and fail
          mid-run. This does NOT disable the Docker repo allowlist.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}
      {isEnv && (
        <Alert tone="info" message="This setting is fixed by an environment variable and cannot be changed here." />
      )}

      <form onSubmit={save} className="space-y-4">
        <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={enabled}
            disabled={isEnv}
            onChange={(e) => setEnabled(e.target.checked)}
            className="h-4 w-4 rounded border-edge accent-brand"
          />
          Enable capability-aware scheduling
        </label>

        <Button type="submit" disabled={!dirty || busy || isEnv}>
          {busy ? "Saving…" : "Save capability scheduling"}
        </Button>
      </form>
    </Card>
  );
}

// GithubProjectSyncCard is the instance-wide kill-switch for GitHub Projects v2 sync
// (issue #534 M2). Default OFF — it initializes from the served value and never
// hard-codes true, because the feature is a rate-limit / cost lever that ships off
// until an admin arms it. When on, each run mirrors a card's board-column label onto a
// linked GitHub Projects Status field (GitHub-only; GitLab/Forgejo are untouched). It
// saves independently, sending only github_project_sync_enabled on change, and greys
// when the value is fixed by an environment variable (the server rejects a write, 409).
function GithubProjectSyncCard({
  settings,
  sources,
  onSaved,
}: {
  settings: AppSettings;
  sources: Record<string, SettingSource>;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [enabled, setEnabled] = useState(settings.github_project_sync_enabled === "true");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const isEnv = sources["github_project_sync_enabled"] === "env";

  const dirty = (enabled ? "true" : "false") !== settings.github_project_sync_enabled;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    if (isEnv || !dirty) return;
    setBusy(true);
    try {
      const resp = await api.updateSettings({
        github_project_sync_enabled: enabled ? "true" : "false",
      });
      onSaved(resp);
      setEnabled(resp.settings.github_project_sync_enabled === "true");
      setNotice("GitHub Projects sync setting saved.");
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to save the GitHub Projects sync setting");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>GitHub Projects sync</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          When on, each run mirrors a board card&rsquo;s column label onto a linked GitHub
          Projects v2 Status field, so a team that prefers GitHub&rsquo;s native board stays in
          step. It is off by default because it spends GitHub API rate limit on every board
          move — an instance-wide cost lever. GitLab and Forgejo repos are unaffected. See the{" "}
          <DocLink slug={DOC_GITHUB_PROJECT_SYNC}>GitHub Projects v2 sync</DocLink> guide.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}
      {isEnv && (
        <Alert tone="info" message="This setting is fixed by an environment variable and cannot be changed here." />
      )}

      <form onSubmit={save} className="space-y-4">
        <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={enabled}
            disabled={isEnv}
            onChange={(e) => setEnabled(e.target.checked)}
            className="h-4 w-4 rounded border-edge accent-brand"
          />
          Enable GitHub Projects sync
        </label>

        <Button type="submit" disabled={!dirty || busy || isEnv}>
          {busy ? "Saving…" : "Save GitHub Projects sync"}
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
