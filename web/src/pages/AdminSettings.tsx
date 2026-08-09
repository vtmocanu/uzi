// Admin → Instance settings (PRD #19 M1): the DB-backed app_settings surface.
// Ships the two configurable forge labels; future instance settings (registration
// policy, self-improvement toggle) slot in here without new plumbing. Admin-only
// (gated by AdminRoute + the admin-only API). Validation mirrors the server
// (Decision 8) for immediate feedback, but the server stays the source of truth.

import { useCallback, useEffect, useState, type FormEvent, type ReactNode } from "react";
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

// parseLabelList splits a comma-separated settings value into a trimmed, non-empty
// token list (PRD #196 M2). joinLabelList is the inverse, de-duplicating so the
// pinned primary is never written twice.
function parseLabelList(value: string | undefined): string[] {
  return (value ?? "")
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
}
function joinLabelList(labels: string[]): string {
  return [...new Set(labels)].join(",");
}

// validateLabelLists mirrors the server's ValidateMerged cross-key rules for the two
// new label lists (PRD #196 M2), for immediate feedback; the server re-checks. The
// primary is auto-pinned into the eligible set by the caller, so "primary present" is
// structurally guaranteed and not re-checked here. `eligibleExtra` is the eligible
// set WITHOUT the primary; `boardExtras` is the visibility list.
function validateLabelLists(
  eligibleExtra: string[],
  boardExtras: string[],
  autopilotLabel: string,
  prdlessLabel: string,
): string | null {
  for (const [name, list] of [
    ["Run-eligible labels", eligibleExtra],
    ["Also-show-on-boards labels", boardExtras],
  ] as const) {
    const seen = new Set<string>();
    for (const l of list) {
      if (l.length > 64) return `${name}: each label must be at most 64 characters.`;
      if (l.includes(",")) return `${name}: a label must not contain a comma.`;
      if (seen.has(l)) return `${name}: "${l}" is listed twice.`;
      seen.add(l);
      if (l === autopilotLabel) return `${name} must not include the autopilot label.`;
      if (l === prdlessLabel) return `${name} must not include the PRDLESS label.`;
    }
  }
  return null;
}

// TagInput is a small multi-value label editor (PRD #196 M2, mock §7), following the
// add/remove pattern of Board.tsx's ColumnSettings: a row of removable tags plus an
// Input that adds on Enter or the Add button. An optional `pinned` tag renders first,
// marked "primary", and cannot be removed. It is deliberately NOT wrapped in <Field>
// (whose implicit <label> would bind the visible label to every control at once);
// the add input carries its own aria-label so it stays individually targetable.
function TagInput({
  label,
  hint,
  tags,
  pinned,
  disabled,
  onAdd,
  onRemove,
}: {
  label: string;
  hint?: ReactNode;
  tags: string[];
  pinned?: string;
  disabled?: boolean;
  onAdd: (name: string) => void;
  onRemove: (name: string) => void;
}) {
  const [draft, setDraft] = useState("");
  const add = () => {
    const n = draft.trim();
    if (n) onAdd(n);
    setDraft("");
  };
  return (
    <div className="space-y-1.5">
      <span className="block text-sm font-medium text-muted">{label}</span>
      <div className="flex flex-wrap items-center gap-1.5 rounded-lg border border-edge bg-raised p-1.5">
        {pinned && (
          <span className="inline-flex items-center gap-1.5 rounded-md border border-brand/60 px-2 py-1 text-xs text-fg">
            {pinned}
            <span className="text-[10px] uppercase tracking-wide text-faint">primary</span>
          </span>
        )}
        {tags.map((t) => (
          <span
            key={t}
            className="inline-flex items-center gap-1.5 rounded-md border border-edge px-2 py-1 text-xs text-fg"
          >
            {t}
            <button
              type="button"
              aria-label={`Remove ${t}`}
              disabled={disabled}
              onClick={() => onRemove(t)}
              className="text-faint transition-colors hover:text-fg disabled:opacity-50"
            >
              ×
            </button>
          </span>
        ))}
        <input
          aria-label={`Add to ${label}`}
          value={draft}
          maxLength={64}
          autoComplete="off"
          placeholder="add a label…"
          disabled={disabled}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              add();
            }
          }}
          className="min-w-32 flex-1 bg-transparent px-2 py-1 text-sm text-fg placeholder:text-faint outline-none disabled:opacity-50"
        />
        <Button type="button" variant="secondary" size="sm" onClick={add} disabled={disabled || !draft.trim()}>
          Add
        </Button>
      </div>
      {hint && <p className="text-xs text-faint">{hint}</p>}
    </div>
  );
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
  // PRD #196 M2. eligibleExtra is the run-eligible set WITHOUT the primary (the
  // primary is pinned and re-added on save); boardExtras is the "also show on boards"
  // default; waivesPrdLink is the PRD-link waiver, defaulting on.
  const [eligibleExtra, setEligibleExtra] = useState<string[]>([]);
  const [boardExtras, setBoardExtras] = useState<string[]>([]);
  const [waivesPrdLink, setWaivesPrdLink] = useState(true);
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
    // The eligible set always contains the primary; strip it here so the pinned tag
    // is the single source of it and the editable list holds only the extras.
    setEligibleExtra(parseLabelList(settings.run_eligible_labels).filter((l) => l !== settings.prd_label));
    setBoardExtras(parseLabelList(settings.board_extra_labels));
    setWaivesPrdLink(settings.eligible_label_waives_prd_link === "true");
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

  const isEnv = (key: string) => sources[key] === "env";
  // The eligible set persisted with the pinned primary re-added (deduped); the
  // board-extra list and the waiver as their raw settings strings (PRD #196 M2).
  const runEligibleValue = joinLabelList([prdLabel, ...eligibleExtra]);
  const boardExtraValue = joinLabelList(boardExtras);
  const waiverValue = waivesPrdLink ? "true" : "false";

  const dirty =
    saved !== null &&
    (prdLabel !== saved.prd_label ||
      autopilotLabel !== saved.autopilot_label ||
      defaultTheme !== saved.default_theme ||
      (prdlessEnabled ? "true" : "false") !== saved.prdless_enabled ||
      prdlessLabel !== saved.prdless_label ||
      runEligibleValue !== saved.run_eligible_labels ||
      boardExtraValue !== saved.board_extra_labels ||
      waiverValue !== saved.eligible_label_waives_prd_link);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    const invalid =
      clientValidate(prdLabel, autopilotLabel, prdlessLabel) ??
      validateLabelLists(eligibleExtra, boardExtras, autopilotLabel, prdlessLabel);
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
      const payload: UpdateSettingsPayload = {
        prd_label: prdLabel,
        autopilot_label: autopilotLabel,
        default_theme: defaultTheme,
        prdless_enabled: prdlessEnabled ? "true" : "false",
        prdless_label: prdlessLabel,
      };
      // Env-sourced keys are read-only (the server 409s a write); skip them, matching
      // the Slack/Health cards' env guard (PRD #196 M2).
      if (!isEnv("run_eligible_labels")) payload.run_eligible_labels = runEligibleValue;
      if (!isEnv("board_extra_labels")) payload.board_extra_labels = boardExtraValue;
      if (!isEnv("eligible_label_waives_prd_link")) {
        payload.eligible_label_waives_prd_link = waiverValue;
      }
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
            <div className="space-y-4 border-t border-edge pt-4">
              <TagInput
                label="Run-eligible labels"
                pinned={prdLabel}
                tags={eligibleExtra}
                disabled={isEnv("run_eligible_labels")}
                onAdd={(name) =>
                  setEligibleExtra((cur) =>
                    name === prdLabel || cur.includes(name) ? cur : [...cur, name],
                  )
                }
                onRemove={(name) => setEligibleExtra((cur) => cur.filter((l) => l !== name))}
                hint={
                  <>
                    An issue carrying any of these can be started by a user. The <b>primary</b> is the
                    one uzi <i>writes</i> — on Promote, and when the judge files an issue — and the only
                    one autopilot matches. It cannot be removed.
                  </>
                }
              />
              <TagInput
                label="Also show on boards"
                tags={boardExtras}
                disabled={isEnv("board_extra_labels")}
                onAdd={(name) =>
                  setBoardExtras((cur) => (cur.includes(name) ? cur : [...cur, name]))
                }
                onRemove={(name) => setBoardExtras((cur) => cur.filter((l) => l !== name))}
                hint={
                  <>
                    The default set of extra labels every board starts with. Issues carrying one appear
                    alongside PRD issues, but still cannot start a run until promoted. Run-eligible labels
                    are shown automatically and need no entry here. Each user can override this for
                    themselves per repo.
                  </>
                }
              />
              <div className="space-y-1.5">
                <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={waivesPrdLink}
                    disabled={isEnv("eligible_label_waives_prd_link")}
                    onChange={(e) => setWaivesPrdLink(e.target.checked)}
                    className="h-4 w-4 rounded border-edge accent-brand disabled:opacity-50"
                  />
                  A non-primary eligible label waives the PRD-link requirement
                </label>
                <p className="text-xs text-faint">
                  When on, an issue that is eligible by a non-primary label (e.g. <code>bug</code>) can
                  start a run with no <code>prds/*.md</code> link. Off: it needs a link or PRDLESS, so
                  the one-click Start becomes two clicks.
                </p>
              </div>
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

      {!loading && saved && (
        <DockerAllowlistCard settings={saved} sources={sources} onSaved={applyResponse} />
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
          autonomously opens (or extends) one merge/pull request on the chosen repo &mdash; picking a single top
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
