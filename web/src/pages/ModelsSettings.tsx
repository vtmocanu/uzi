// Settings → Models (PRD #3263): every model and effort default in one grid, Claude and Codex
// side by side, saved together by one PUT /me/model-settings. One parent owns the stored
// baseline, the draft, the semantic dirty set, the field errors and the save state
// (lib/modelsDraft.ts holds the meaning). Each cell shows the SAVED prediction from
// /me/effective-settings with its server source label; the browser never recomputes precedence.
//
// Everything is fenced to the viewer: the editor is keyed by the signed-in user, so a viewer
// change discards the previous draft, and every load and save checks a request token plus the
// live viewer before installing a response, so a late reply for an earlier request or viewer is
// dropped.

import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { useAuth } from "../auth/AuthContext";
import {
  api,
  ApiError,
  modelSettingsFailureReason,
  modelSettingsFieldErrors,
  type EffectiveSettings,
  type UserSettings,
} from "../lib/api";
import { errorMessage } from "../lib/apiError";
import {
  baselineFromSettings,
  buildModelsPatch,
  dirtyFields,
  emptyCustomFields,
  fieldLabel,
  mapFieldErrors,
  MODELS_FIELDS,
  type ModelsField,
  type ModelsValues,
} from "../lib/modelsDraft";
import { codexRuntimeDefaultLabel } from "../lib/modelSettings";
import { SettingsShell } from "../components/SettingsShell";
import { Alert, Button, Card, Select, Skeleton } from "../components/ui";
import { ModelSelect } from "../components/ModelSelect";
import { EffortSelect } from "../components/EffortSelect";
import { ClaudeIcon, OpenAIIcon } from "../components/icons";
import { ModelsCell, type ModelsCellIds } from "../components/models/ModelsCell";
import { ModelsSaveBar, type SaveOutcome } from "../components/models/ModelsSaveBar";
import { UnsavedModelsGuard } from "../components/models/UnsavedModelsGuard";
import {
  harnessReason,
  planningEffortDefaultLabel,
  planningModelDefaultLabel,
  predictionText,
  workerEffortDefaultLabel,
  workerModelDefaultLabel,
} from "../components/models/modelsLabels";

// The grid's three-track template, shared by the header and every row so the columns align.
const ROW_GRID = "md:grid md:grid-cols-[minmax(9rem,13rem)_minmax(0,1fr)_minmax(0,1fr)] md:gap-x-6";

interface RowDef {
  row: string;
  description: string;
}

const GROUPS: { id: string; title: string; note: string; rows: RowDef[] }[] = [
  {
    id: "worker",
    title: "Worker runs",
    note: "What a run uses once its plan is approved, and for runs that do not plan.",
    rows: [
      { row: "Worker model", description: "The lead agent, plus subagents without a model of their own." },
      { row: "Worker effort", description: "Higher levels reason longer and cost more. Runtime default sends no level." },
    ],
  },
  {
    id: "planning",
    title: "Planning",
    note: "Used while a run plans; it stays on the run's harness.",
    rows: [
      { row: "Planning model", description: "Same as the run follows the run's worker model." },
      { row: "Planning effort", description: "Same as the run follows the run's effort, including the lead template's." },
    ],
  },
];

const fieldFor = (row: string, family: "claude" | "codex" | null) =>
  MODELS_FIELDS.find((f) => f.row === row && f.family === family)!;

function idFor(field: ModelsField): string {
  return `models-${field.key.replace(/[^a-z0-9]+/gi, "-")}`;
}

interface Loaded {
  settings: UserSettings;
  effective: EffectiveSettings | null;
}

export function ModelsSettings() {
  const { user } = useAuth();
  return (
    <SettingsShell description="The models and reasoning effort your future runs use, per harness. Change anything below, then save once.">
      {/* Keyed by viewer: a different (or no) signed-in user gets a fresh editor, so the
          previous viewer's draft, errors and in-flight requests are gone with it. */}
      {user ? <ModelsEditor key={user.id} viewerId={user.id} /> : null}
    </SettingsShell>
  );
}

function ModelsEditor({ viewerId }: { viewerId: string }) {
  const { user } = useAuth();
  const liveViewer = useRef<string | null>(user?.id ?? null);
  liveViewer.current = user?.id ?? null;
  const mounted = useRef(false);
  const requestSeq = useRef(0);

  const [loaded, setLoaded] = useState<Loaded | null>(null);
  const [loadError, setLoadError] = useState("");
  const [baseline, setBaseline] = useState<ModelsValues>({});
  const [draft, setDraft] = useState<ModelsValues>({});
  const [saving, setSaving] = useState(false);
  const [reloading, setReloading] = useState(false);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [outcome, setOutcome] = useState<SaveOutcome | null>(null);
  // An earlier save may have committed without confirmation. Kept apart from `outcome`, which a
  // later failed reload or failed retry replaces: only a successful save, a successful reload or
  // Discard reconciles it, so the guard never offers "Discard changes" while a commit is unconfirmed.
  const [uncertainty, setUncertainty] = useState<"unknown" | "saved_unreadable" | null>(null);
  const [customMode, setCustomMode] = useState<Record<string, boolean>>({});
  // Bumped by Discard to remount the model pickers, so one left in "Other" with an empty box
  // (whose value never changed, so it would not re-derive) returns to its stored option.
  const [pickerGeneration, setPickerGeneration] = useState(0);
  // Save and Reload never overlap: while either is in flight, both are disabled, so neither
  // request can supersede the other's token.
  const busy = saving || reloading;

  // A response is installed only if it answers the newest request, for this viewer, while
  // this editor is still mounted.
  const isCurrent = useCallback(
    (seq: number) => mounted.current && seq === requestSeq.current && liveViewer.current === viewerId,
    [viewerId],
  );

  const fetchSaved = useCallback(async () => {
    const [settings, effective] = await Promise.allSettled([api.getMySettings(), api.getEffectiveSettings()]);
    if (settings.status === "rejected") throw settings.reason;
    // A prediction failure is "unavailable", never a guessed fallback; the settings still load.
    return { settings: settings.value.settings, effective: effective.status === "fulfilled" ? effective.value : null };
  }, []);

  const load = useCallback(async () => {
    const seq = ++requestSeq.current;
    setLoadError("");
    try {
      const next = await fetchSaved();
      if (!isCurrent(seq)) return;
      const values = baselineFromSettings(next.settings);
      setLoaded(next);
      setBaseline(values);
      setDraft(values);
    } catch (err) {
      if (isCurrent(seq)) setLoadError(errorMessage(err, "Failed to load your model settings"));
    }
  }, [fetchSaved, isCurrent]);

  useEffect(() => {
    mounted.current = true;
    void load();
    return () => {
      mounted.current = false;
    };
  }, [load]);

  const dirty = useMemo(() => dirtyFields(baseline, draft), [baseline, draft]);
  const dirtyKeys = useMemo(() => new Set(dirty.map((f) => f.key)), [dirty]);
  // "Other (custom model ID)" with an empty box would otherwise save as Default while the
  // picker still shows Other, so any cell in that state, dirty or not, blocks Save with its own error.
  const customEmpty = useMemo(() => new Set(emptyCustomFields(draft, customMode).map((f) => f.key)), [customMode, draft]);

  const setValue = (key: string, value: string) => {
    setDraft((d) => ({ ...d, [key]: value }));
    setFieldErrors((errs) => {
      if (!(key in errs)) return errs;
      const next = { ...errs };
      delete next[key];
      return next;
    });
    setOutcome((o) => (o?.kind === "saved" || o?.kind === "reloaded" ? null : o));
  };

  const discard = useCallback(() => {
    setDraft(baseline);
    setFieldErrors({});
    setOutcome(null);
    setUncertainty(null);
    setPickerGeneration((g) => g + 1);
  }, [baseline]);

  const save = async (e?: FormEvent) => {
    e?.preventDefault();
    if (busy || dirty.length === 0 || customEmpty.size > 0) return;
    const seq = ++requestSeq.current;
    const count = dirty.length;
    setSaving(true);
    setOutcome(null);
    setFieldErrors({});
    try {
      const res = await api.putMyModelSettings(buildModelsPatch(baseline, draft));
      if (!isCurrent(seq)) return;
      // The canonical post-commit settings become the clean baseline AND the draft.
      const values = baselineFromSettings(res.settings);
      setLoaded({ settings: res.settings, effective: res.effective ?? null });
      setBaseline(values);
      setDraft(values);
      setUncertainty(null);
      setOutcome(res.effective ? { kind: "saved", count } : { kind: "saved_no_prediction" });
    } catch (err) {
      if (!isCurrent(seq)) return;
      // Every failure keeps the draft: nothing is rolled back in the browser.
      if (err instanceof ApiError && err.status === 400) {
        const { byKey, unmatched } = mapFieldErrors(modelSettingsFieldErrors(err));
        setFieldErrors(byKey);
        setOutcome({ kind: "invalid", count: Object.keys(byKey).length + unmatched.length, unmatched });
      } else if (modelSettingsFailureReason(err) === "settings_commit_unknown") {
        setOutcome({ kind: "unknown" });
        setUncertainty("unknown");
      } else if (modelSettingsFailureReason(err) === "settings_saved_readback_failed") {
        setOutcome({ kind: "saved_unreadable" });
        setUncertainty((u) => u ?? "saved_unreadable");
      } else if (err instanceof ApiError && err.status <= 500) {
        // The handler answers a plain 500 only for a failure before commit (rolled back),
        // and other 4xx before writing anything.
        setOutcome({ kind: "not_saved", message: err.message });
      } else {
        // A network failure or a gateway error: the request may have reached the server.
        setOutcome({ kind: "unknown" });
        setUncertainty("unknown");
      }
    } finally {
      // Cleared whenever still mounted, even if a newer request token was issued meanwhile
      // (e.g. the viewer-fenced load): a saving flag left set would disable Save for good.
      if (mounted.current) setSaving(false);
    }
  };

  // Reconciliation after an uncertain save: re-read what is stored and make it the baseline,
  // keeping the draft, so the dirty set shows exactly which edits are not stored. No retry.
  const reload = async () => {
    if (busy) return;
    const seq = ++requestSeq.current;
    setReloading(true);
    try {
      const next = await fetchSaved();
      if (!isCurrent(seq)) return;
      const values = baselineFromSettings(next.settings);
      setLoaded(next);
      setBaseline(values);
      setFieldErrors({});
      setUncertainty(null);
      setOutcome({ kind: "reloaded", stillDirty: dirtyFields(values, draft).length });
    } catch (err) {
      if (isCurrent(seq)) setOutcome({ kind: "reload_failed", message: errorMessage(err, "request failed") });
    } finally {
      if (mounted.current) setReloading(false);
    }
  };

  if (loadError) {
    return (
      <div className="space-y-3">
        <Alert message={loadError} />
        <Button type="button" variant="secondary" onClick={() => void load()}>
          Try again
        </Button>
      </div>
    );
  }

  if (!loaded) {
    return (
      <Card className="space-y-4">
        <span className="sr-only" role="status">Loading your model settings{"…"}</span>
        {[0, 1, 2, 3, 4].map((i) => (
          <Skeleton key={i} className="h-12 w-full" />
        ))}
      </Card>
    );
  }

  const { settings, effective } = loaded;
  const workerModelCell = (h: "claude" | "codex") =>
    effective?.models?.find((c) => c.setting === "worker_model" && c.harness === h);
  const effortCell = (h: "claude" | "codex") =>
    settings.effort_settings?.find((c) => c.harness === h) ??
    effective?.efforts?.find((c) => c.setting === "worker_effort" && c.harness === h);

  const cellProps = (field: ModelsField) => ({
    field,
    idBase: idFor(field),
    dirty: dirtyKeys.has(field.key),
    prediction: predictionText(field, effective),
    reason: field.family ? harnessReason(field.family, effective) : null,
    error:
      fieldErrors[field.key] ??
      (customEmpty.has(field.key)
        ? field.row.startsWith("Planning")
          ? "Enter a custom model ID, or choose Same as the run."
          : "Enter a custom model ID, or choose Default."
        : null),
  });

  const modelControl = (field: ModelsField, ids: ModelsCellIds) => {
    const codex = field.family === "codex";
    const planning = field.row.startsWith("Planning");
    const cell = planning ? settings.planning_models?.find((c) => c.harness === field.family) : workerModelCell(field.family!);
    return (
      <ModelSelect
        key={pickerGeneration}
        id={ids.control}
        value={draft[field.key] ?? ""}
        onChange={(v) => setValue(field.key, v)}
        harness={field.family!}
        allowCustom={codex ? true : undefined}
        defaultLabel={planning ? planningModelDefaultLabel(cell) : workerModelDefaultLabel(field.family!, cell)}
        customAriaLabel={`${fieldLabel(field)}, custom model ID`}
        ariaDescribedBy={ids.describedBy}
        onCustomModeChange={(custom) => setCustomMode((m) => (m[field.key] === custom ? m : { ...m, [field.key]: custom }))}
      />
    );
  };

  const effortControl = (field: ModelsField, ids: ModelsCellIds) => {
    const planning = field.row.startsWith("Planning");
    const h = field.family!;
    return (
      <EffortSelect
        id={ids.control}
        value={draft[field.key] ?? ""}
        onChange={(v) => setValue(field.key, v)}
        ariaDescribedBy={ids.describedBy}
        defaultLabel={
          planning
            ? planningEffortDefaultLabel(settings.planning_efforts?.find((c) => c.harness === h))
            : workerEffortDefaultLabel(effortCell(h))
        }
        // Planning effort has no Runtime default (the server refuses it); the worker lanes
        // keep it distinct from Default.
        runtimeLabel={
          planning ? undefined : h === "codex" ? codexRuntimeDefaultLabel(settings.effort_settings?.find((c) => c.harness === "codex")) : "Runtime default · value not reported"
        }
      />
    );
  };

  const harnessField = fieldFor("Default harness", null);
  const dirtyRows = dirty.map(fieldLabel);

  return (
    <form onSubmit={(e) => void save(e)} className="space-y-4 pb-6" noValidate>
      <UnsavedModelsGuard
        dirtyCount={dirty.length}
        saving={saving}
        // The PUT may have committed: leaving must not read as a discard (PRD: explain the
        // uncertainty, never claim a rollback). A reload replaces this outcome.
        saveUncertainty={uncertainty}
        viewerId={viewerId}
        onDiscard={discard}
      />
      <fieldset disabled={busy} className="min-w-0 space-y-4 disabled:opacity-80">
        <legend className="sr-only">Model settings</legend>
        <Card className="space-y-1">
          <p id="models-precedence" className="max-w-prose text-sm text-muted">
            Explicit run choices win in both phases, and a compatible schedule model pin overrides these
            defaults. Each cell shows the saved effective value and where it comes from; an edit
            changes it only after you save.
          </p>

          {/* Default harness: one choice for both families. */}
          <div className={`border-t border-edge pt-4 mt-4 ${ROW_GRID}`}>
            <div className="mb-3 md:mb-0">
              <h3 className="text-sm font-medium text-fg">Default harness</h3>
              <p className="text-xs text-faint">
                Used when a run or schedule doesn't choose one. Switching keeps both families' choices below.
              </p>
            </div>
            <ModelsCell {...cellProps(harnessField)} reason={null} wide>
              {(ids) => (
                <Select
                  id={ids.control}
                  aria-describedby={ids.describedBy}
                  className="max-w-sm"
                  value={draft[harnessField.key] || "inherit"}
                  onChange={(e) => setValue(harnessField.key, e.target.value === "inherit" ? "" : e.target.value)}
                >
                  <option value="inherit">Default {"·"} automatic</option>
                  <option value="claude">Claude</option>
                  <option value="codex">Codex</option>
                </Select>
              )}
            </ModelsCell>
          </div>

          {/* Column headers: visual only (each control names its family itself), and hidden
              where the cells stack and carry their own family label. */}
          <div aria-hidden="true" className={`hidden pt-6 pb-2 ${ROW_GRID}`}>
            <span />
            {(["claude", "codex"] as const).map((h) => (
              <span key={h} className="flex items-center gap-2 border-l-2 border-transparent pl-3 text-sm font-semibold text-fg">
                {h === "claude" ? <ClaudeIcon className="h-4 w-4 text-brand-claude" /> : <OpenAIIcon className="h-4 w-4" />}
                {h === "claude" ? "Claude" : "Codex"}
              </span>
            ))}
          </div>

          {GROUPS.map((group) => (
            <div key={group.title} role="group" aria-labelledby={`models-group-${group.id}`} className="pt-4">
              <div className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 pb-2">
                <h3 id={`models-group-${group.id}`} className="text-sm font-semibold text-fg">
                  {group.title}
                </h3>
                <p className="text-xs text-faint">{group.note}</p>
              </div>
              {group.rows.map(({ row, description }) => (
                <div key={row} data-row={row} className={`border-t border-edge py-4 ${ROW_GRID}`}>
                  <div className="mb-3 md:mb-0">
                    <h4 className="text-sm font-medium text-fg">{row}</h4>
                    <p className="text-xs text-faint">{description}</p>
                  </div>
                  <div className="space-y-4 md:contents">
                    {(["claude", "codex"] as const).map((h) => {
                      const field = fieldFor(row, h);
                      return (
                        <ModelsCell key={field.key} {...cellProps(field)}>
                          {(ids) => (field.kind === "model" ? modelControl(field, ids) : effortControl(field, ids))}
                        </ModelsCell>
                      );
                    })}
                  </div>
                </div>
              ))}
            </div>
          ))}
        </Card>
      </fieldset>
      <ModelsSaveBar
        dirtyRows={dirtyRows}
        saving={saving}
        busy={busy}
        blockedCount={customEmpty.size}
        outcome={outcome}
        reloading={reloading}
        onDiscard={discard}
        onReload={() => void reload()}
      />
    </form>
  );
}
