// Admin → Instance settings (PRD #19 M1): the DB-backed app_settings surface.
// Ships the two configurable forge labels; future instance settings (registration
// policy, self-improvement toggle) slot in here without new plumbing. Admin-only
// (gated by AdminRoute + the admin-only API). Validation mirrors the server
// (Decision 8) for immediate feedback, but the server stays the source of truth.

import { useCallback, useEffect, useState, type FormEvent } from "react";
import { api, ApiError, type AppSettings } from "../lib/api";
import { Alert, Button, Card, Field, Input, PageHeader, SectionTitle, Skeleton } from "../components/ui";

// clientValidate reproduces the server's per-value + cross-key rules so an
// obviously-bad edit is caught before the round-trip. Returns an error message
// or null. The server re-checks regardless.
function clientValidate(prdLabel: string, autopilotLabel: string): string | null {
  for (const [name, value] of [
    ["PRD label", prdLabel],
    ["Autopilot label", autopilotLabel],
  ] as const) {
    if (value.trim() === "") return `${name} must not be empty.`;
    if (value.length > 64) return `${name} must be at most 64 characters.`;
    if (value.includes(",")) return `${name} must not contain a comma.`;
  }
  if (prdLabel === autopilotLabel) return "The PRD and autopilot labels must differ.";
  return null;
}

export function AdminSettings() {
  const [saved, setSaved] = useState<AppSettings | null>(null);
  const [prdLabel, setPrdLabel] = useState("");
  const [autopilotLabel, setAutopilotLabel] = useState("");
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
    saved !== null && (prdLabel !== saved.prd_label || autopilotLabel !== saved.autopilot_label);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    const invalid = clientValidate(prdLabel, autopilotLabel);
    if (invalid) {
      setError(invalid);
      return;
    }
    setBusy(true);
    try {
      const { settings } = await api.updateSettings({
        prd_label: prdLabel,
        autopilot_label: autopilotLabel,
      });
      setSaved(settings);
      setPrdLabel(settings.prd_label);
      setAutopilotLabel(settings.autopilot_label);
      setNotice("Settings saved. Boards reflect a changed PRD label after the next sync.");
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
            forge — create the label in GitLab yourself. The two labels must differ.
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
            <Button type="submit" disabled={busy || !dirty}>
              {busy ? "Saving…" : "Save settings"}
            </Button>
          </form>
        )}
      </Card>
    </div>
  );
}
