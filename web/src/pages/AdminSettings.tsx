// Admin → Instance settings (PRD #19 M1): the DB-backed app_settings surface.
// Ships the two configurable forge labels; future instance settings (registration
// policy, self-improvement toggle) slot in here without new plumbing. Admin-only
// (gated by AdminRoute + the admin-only API). Validation mirrors the server
// (Decision 8) for immediate feedback, but the server stays the source of truth.

import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  api,
  type AppSettings,
  type SettingSource,
  type SettingsResponse,
  type UpdateSettingsPayload,
} from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
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
import { DocLink } from "../components/DocLink";
import { DOC_ADMIN_SETTINGS } from "../lib/doclinks";
import { THEMES, THEME_LABELS } from "../lib/theme";
import { JudgeSettingsCard } from "./adminSettings/JudgeSettingsCard";
import { EphemeralWorkersCard } from "./adminSettings/EphemeralWorkersCard";
import { UpdatesSettingsCard } from "./adminSettings/UpdatesSettingsCard";
import { SummarySettingsCard } from "./adminSettings/SummarySettingsCard";
import { AgentSourceSettingsCard } from "./adminSettings/AgentSourceSettingsCard";
import { HealthSettingsCard } from "./adminSettings/HealthSettingsCard";
import { DockerAllowlistCard } from "./adminSettings/DockerAllowlistCard";
import { CapabilitySchedulingCard } from "./adminSettings/CapabilitySchedulingCard";
import { GithubProjectSyncCard } from "./adminSettings/GithubProjectSyncCard";
import { SlackSettingsCard } from "./adminSettings/SlackSettingsCard";

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

  // The settings form fields are seeded by applyResponse as a fetcher side effect
  // (they are editable and also written by the save handler). The vaultMigration probe
  // runs in a `finally` so it fires regardless of the settings-load outcome, exactly as
  // the old load ran it after its own try/catch/finally (PRD #32). The hook owns
  // loading and the load error (surfaced as loadError, unioned with the save error at
  // the Alert below); masterSealed stays local, set as a side effect.
  const { loading, error: loadError } = useAsyncData(
    async ({ isCurrent }) => {
      try {
        const resp = await api.getSettings();
        if (isCurrent()) applyResponse(resp);
      } finally {
        api
          .vaultMigration()
          .then(({ master_sealed }) => {
            if (isCurrent()) setMasterSealed(master_sealed);
          })
          .catch(() => {
            if (isCurrent()) setMasterSealed(null);
          });
      }
    },
    [applyResponse],
    { fallback: "Failed to load settings" },
  );

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
      setError(errorMessage(err, "Failed to save settings"));
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

      {(error || loadError) && <Alert message={error || loadError} />}
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
