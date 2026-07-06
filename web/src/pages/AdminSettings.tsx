// Admin → Instance settings (PRD #19 M1): the DB-backed app_settings surface.
// Ships the two configurable forge labels; future instance settings (registration
// policy, self-improvement toggle) slot in here without new plumbing. Admin-only
// (gated by AdminRoute + the admin-only API). Validation mirrors the server
// (Decision 8) for immediate feedback, but the server stays the source of truth.

import { useCallback, useEffect, useState, type FormEvent } from "react";
import { api, ApiError, type AppSettings } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { Alert, Button, Card, Field, Input, PageHeader, SectionTitle, Select, Skeleton } from "../components/ui";
import { THEMES, THEME_LABELS } from "../lib/theme";

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
  const [prdLabel, setPrdLabel] = useState("");
  const [autopilotLabel, setAutopilotLabel] = useState("");
  const [defaultTheme, setDefaultTheme] = useState("");
  const [prdlessEnabled, setPrdlessEnabled] = useState(true);
  const [prdlessLabel, setPrdlessLabel] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const load = useCallback(async () => {
    try {
      const { settings } = await api.getSettings();
      setSaved(settings);
      setPrdLabel(settings.prd_label);
      setAutopilotLabel(settings.autopilot_label);
      setDefaultTheme(settings.default_theme);
      setPrdlessEnabled(settings.prdless_enabled === "true");
      setPrdlessLabel(settings.prdless_label);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load settings");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

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
      const { settings } = await api.updateSettings({
        prd_label: prdLabel,
        autopilot_label: autopilotLabel,
        default_theme: defaultTheme,
        prdless_enabled: prdlessEnabled ? "true" : "false",
        prdless_label: prdlessLabel,
      });
      setSaved(settings);
      setPrdLabel(settings.prd_label);
      setAutopilotLabel(settings.autopilot_label);
      setDefaultTheme(settings.default_theme);
      setPrdlessEnabled(settings.prdless_enabled === "true");
      setPrdlessLabel(settings.prdless_label);
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
    </div>
  );
}
