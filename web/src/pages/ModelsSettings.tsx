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

import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent, type ReactNode } from "react";
import { Link } from "react-router-dom";
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
  localFieldWarnings,
  storedFieldWarnings,
  mapFieldErrors,
  MODELS_FIELDS,
  type ModelsField,
  type ModelsValues,
} from "../lib/modelsDraft";
import { codexRuntimeDefaultLabel } from "../lib/modelSettings";
import { CODEX_ROLE_MODELS } from "../lib/codexModels";
import { SettingsShell } from "../components/SettingsShell";
import { Alert, Button, Card, cx, Select, Skeleton } from "../components/ui";
import { ModelSelect } from "../components/ModelSelect";
import { EffortSelect } from "../components/EffortSelect";
import { ClaudeIcon, OpenAIIcon } from "../components/icons";
import { ModelsCell, type ModelsCellIds } from "../components/models/ModelsCell";
import { ModelsSaveBar, type SaveOutcome } from "../components/models/ModelsSaveBar";
import { UnsavedModelsGuard } from "../components/models/UnsavedModelsGuard";
import {
  CHECKER_EFFORT_DEFAULT_LABEL,
  checkerModelDefaultLabel,
  harnessName,
  harnessReason,
  helperModelDefaultLabel,
  judgeNotUsedReason,
  judgeRunsOnHelp,
  planningEffortDefaultLabel,
  planningModelDefaultLabel,
  predictionText,
  taskReviewEffortDefaultLabel,
  taskReviewModelDefaultLabel,
  workerEffortDefaultLabel,
  workerModelDefaultLabel,
} from "../components/models/modelsLabels";

// The grid template, shared by the headers and every row so the columns align. It answers the
// CARD's width, not the viewport's: the 15rem sidebar appears at lg, so a viewport breakpoint
// widened the grid exactly when the card narrowed and truncated select values. A select needs
// ~345px for its longest common value ("Same as the run · gpt-6-sol (lead template)"), so:
// below a 49rem card each family stacks under the row; from 49rem Claude and Codex sit side by
// side with the row name above them; from 60rem the name takes its own (narrow) track.
const ROW_GRID =
  "@[49rem]:grid @[49rem]:grid-cols-2 @[49rem]:gap-x-6 @[60rem]:grid-cols-[minmax(8rem,10rem)_minmax(0,1fr)_minmax(0,1fr)]";
// A row's name block: above the pair below 60rem, its own track from there.
const ROW_NAME = "mb-3 @[49rem]:col-span-2 @[60rem]:col-span-1 @[60rem]:mb-0";

// A row is a Claude/Codex pair, a routing choice spanning both columns (Judge runs on), or a
// read-only fact with no control (Summaries run on). A `sub` row belongs to the routing row
// above it: no rule between them, and a quieter name.
interface RowDef {
  row: string;
  description: ReactNode;
  type?: "pair" | "routing" | "fact";
  sub?: boolean;
}

const RUN_DEFAULTS_LINK = (
  <Link to="/settings/run-defaults" className="underline underline-offset-2 hover:text-fg">
    Run defaults
  </Link>
);

const GROUPS: { id: string; title: string; note: ReactNode; rows: RowDef[] }[] = [
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
  {
    id: "helpers",
    title: "Helper runs",
    note: "Short jobs around a run, each on your own credential for the harness it runs on.",
    rows: [
      { row: "Judge runs on", type: "routing", description: "Where the judge reviews each finished run." },
      { row: "Judge model", sub: true, description: "The model for each harness the judge may run on." },
      { row: "Summaries run on", type: "fact", description: "The intent and plan summaries of every run." },
      { row: "Summary model", sub: true, description: "Claude summaries also write the pull request description." },
      { row: "Task review model", description: "The optional uzi handoff --review pass, on the harness of the task it reviews." },
      { row: "Task review effort", description: "Default follows your worker effort for that family." },
    ],
  },
  {
    id: "cross-check",
    title: "Cross-check",
    note: (
      <>
        A checker from the other family reviews the lead: the Claude column checks Codex runs, the Codex
        column Claude runs. Turn each check on or off in {RUN_DEFAULTS_LINK}.
      </>
    ),
    rows: [
      { row: "Plan cross-check model", description: "Reviews the plan before implementation starts." },
      { row: "Plan cross-check effort", description: "Default is the cross-check default, not your worker effort." },
      { row: "Code cross-check model", description: "Reviews the code before the merge request is published." },
      { row: "Code cross-check effort", description: "Default is the cross-check default, not your worker effort." },
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
  const { user, judgeEnforcedByAdmin } = useAuth();
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
  // later failed reload or failed retry replaces: a confirmed save or a read after a completed response
  // reconciles it, except after a lost response (`requestMayStillFinish`): that request may still
  // commit, so nothing clears it for the page's life. A dialog shown while a commit is unconfirmed
  // never offers "Discard changes".
  // Discard drops the local draft but keeps this (with Reload) because it cannot know what the
  // server stored; a clean draft is not guarded by the leave dialog.
  const [uncertainty, setUncertainty] = useState<"unknown" | "saved_unreadable" | null>(null);
  const [unconfirmedKeys, setUnconfirmedKeys] = useState<ReadonlySet<string>>(new Set());
  const [reaffirmed, setReaffirmed] = useState<ReadonlySet<string>>(new Set());
  const [requestMayStillFinish, setRequestMayStillFinish] = useState(false);
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

  const dirty = useMemo(() => dirtyFields(baseline, draft, MODELS_FIELDS, reaffirmed), [baseline, draft, reaffirmed]);
  const dirtyKeys = useMemo(() => new Set(dirty.map((f) => f.key)), [dirty]);
  // "Other (custom model ID)" with an empty box would otherwise save as Default while the
  // picker still shows Other, so any cell in that state, dirty or not, blocks Save with its own error.
  const customEmpty = useMemo(() => new Set(emptyCustomFields(draft, customMode).map((f) => f.key)), [customMode, draft]);
  // A dirty value its own validator refuses (a Codex id in a Claude helper lane, the other
  // family's model in a checker) blocks Save on its cell, dimmed or not.
  const localWarnings = useMemo(() => localFieldWarnings(baseline, draft, MODELS_FIELDS, reaffirmed), [baseline, draft, reaffirmed]);
  const blocked = useMemo(() => new Set([...customEmpty, ...Object.keys(localWarnings)]), [customEmpty, localWarnings]);
  // An UNCHANGED stored value the same validator refuses (a legacy Codex id in a Claude lane) is
  // shown on its cell as information; it never blocks Save.
  const storedWarnings = useMemo(() => storedFieldWarnings(baseline, draft, MODELS_FIELDS, reaffirmed), [baseline, draft, reaffirmed]);

  const setValue = (key: string, value: string) => {
    setDraft((d) => ({ ...d, [key]: value }));
    if (unconfirmedKeys.has(key)) setReaffirmed((keys) => new Set([...keys, key]));
    setFieldErrors((errs) => {
      if (!(key in errs)) return errs;
      const next = { ...errs };
      delete next[key];
      return next;
    });
    setOutcome((o) => (o?.kind === "saved" || o?.kind === "reloaded" ? null : o));
  };

  // Discard drops the local draft only. After an uncertain save it does not reconcile what the
  // server stored, so the uncertainty and its alert (with Reload saved settings) stay until a
  // confirmed save or a reload that is safe to reconcile; after a lost-response request
  // (`requestMayStillFinish`) they stay for the page's life.
  const discard = useCallback(() => {
    setDraft(baseline);
    setReaffirmed(new Set());
    setFieldErrors({});
    setOutcome(uncertainty ? { kind: "discarded_unconfirmed" } : null);
    setPickerGeneration((g) => g + 1);
  }, [baseline, uncertainty]);

  const save = async (e?: FormEvent) => {
    e?.preventDefault();
    if (busy || dirty.length === 0 || blocked.size > 0) return;
    const seq = ++requestSeq.current;
    const count = dirty.length;
    const earlierMayStillFinish = requestMayStillFinish;
    setSaving(true);
    setOutcome(null);
    setFieldErrors({});
    const rememberUnconfirmed = () => setUnconfirmedKeys((keys) => new Set([...keys, ...dirtyKeys]));
    try {
      const res = await api.putMyModelSettings(buildModelsPatch(baseline, draft, MODELS_FIELDS, reaffirmed));
      if (!isCurrent(seq)) return;
      // The canonical post-commit settings become the clean baseline AND the draft.
      const values = baselineFromSettings(res.settings);
      setLoaded({ settings: res.settings, effective: res.effective ?? null });
      setBaseline(values);
      setDraft(values);
      // A lost-response request may still commit after this retry and overwrite it, so this
      // confirmation covers only its own save: the earlier uncertainty stays.
      if (!earlierMayStillFinish) {
        setUncertainty(null);
        setUnconfirmedKeys(new Set());
      }
      setReaffirmed(new Set());
      setOutcome(
        res.effective
          ? { kind: "saved", count, ...(earlierMayStillFinish ? { earlierMayStillFinish: true } : {}) }
          : { kind: "saved_no_prediction" },
      );
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
        rememberUnconfirmed();
      } else if (modelSettingsFailureReason(err) === "settings_saved_readback_failed") {
        setOutcome({ kind: "saved_unreadable" });
        setUncertainty((u) => u ?? "saved_unreadable");
        rememberUnconfirmed();
      } else if (err instanceof ApiError && err.status <= 500) {
        // The handler answers a plain 500 only for a failure before commit (rolled back),
        // and other 4xx before writing anything.
        setOutcome({ kind: "not_saved", message: err.message });
      } else {
        // A network failure or a gateway error can arrive before the server finishes.
        setOutcome({ kind: "unknown" });
        setUncertainty("unknown");
        setRequestMayStillFinish(true);
        rememberUnconfirmed();
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
      // Keep only edits the viewer made against the previous baseline. Every
      // other field follows the fresh server value.
      const nextDraft = { ...values };
      for (const { key } of dirty) nextDraft[key] = draft[key];
      setLoaded(next);
      setBaseline(values);
      setDraft(nextDraft);
      if (dirty.length === 0) setPickerGeneration((g) => g + 1);
      setFieldErrors({});
      if (!requestMayStillFinish) {
        setUncertainty(null);
        setUnconfirmedKeys(new Set());
        setReaffirmed(new Set());
      }
      setOutcome({
        kind: "reloaded",
        adopted: dirty.length === 0,
        stillDirty: dirtyFields(values, nextDraft, MODELS_FIELDS, requestMayStillFinish ? reaffirmed : undefined).length,
        requestMayStillFinish,
      });
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
  // Default metadata comes from the effective worker-effort cell, which includes the lead
  // template's effort tier; the /me/settings preference cell always describes the built-in
  // default. Without predictions the label stays generic rather than claiming built-in medium.
  // (The Codex Runtime default label still reads catalog metadata from the settings cell.)
  const effortCell = (h: "claude" | "codex") =>
    effective?.efforts?.find((c) => c.setting === "worker_effort" && c.harness === h);

  // The judge's routing as DRAFTED: dimming follows the select before it is saved.
  const judgeRunsOn = draft.judge_runs_on ?? "";

  const cellProps = (field: ModelsField) => {
    const notUsed = judgeNotUsedReason(field, judgeRunsOn);
    return {
      field,
      idBase: idFor(field),
      dirty: dirtyKeys.has(field.key),
      // A cell showing its own Not used line drops the saved prediction's not_used note, so a
      // screen reader hears it once.
      prediction: predictionText(field, effective, { omitNotUsed: notUsed !== null }),
      reason: field.family ? harnessReason(field.family, effective) : null,
      notUsed,
      storedWarning: storedWarnings[field.key] ?? null,
      error:
        fieldErrors[field.key] ??
        localWarnings[field.key] ??
        (customEmpty.has(field.key)
          ? field.row.startsWith("Planning")
            ? "Enter a custom model ID, or choose Same as the run."
            : "Enter a custom model ID, or choose Default."
          : null),
    };
  };

  const pinOf = (field: ModelsField) =>
    "pin" in field.target
      ? settings.cross_check_pins?.find((p) => "pin" in field.target && p.stage === field.target.pin.stage && p.harness === field.target.pin.harness)
      : undefined;
  const cellFor = <T extends { harness: string }>(cells: readonly T[] | null | undefined, h: string) => cells?.find((c) => c.harness === h);

  // Each row keeps the catalog and Default its Run defaults card had: worker Codex and the
  // checkers accept a custom ID; the Codex judge is the curated set only; the Codex summary and
  // task-review lanes are the Codex role models; the Claude lanes keep the Claude aliases plus a
  // custom ID, guarded by the field's own validator.
  const modelPicker = (field: ModelsField): { defaultLabel: string; allowCustom?: boolean; models?: readonly string[] } => {
    const h = field.family!;
    const codex = h === "codex";
    if ("pin" in field.target) return { defaultLabel: checkerModelDefaultLabel(h, pinOf(field)), allowCustom: true };
    switch (field.row) {
      case "Planning model":
        return { defaultLabel: planningModelDefaultLabel(cellFor(settings.planning_models, h)), allowCustom: codex ? true : undefined };
      case "Judge model":
        return { defaultLabel: helperModelDefaultLabel(cellFor(settings.judge_models, h)), allowCustom: codex ? false : undefined };
      case "Summary model":
        return { defaultLabel: helperModelDefaultLabel(cellFor(settings.summary_models, h)), models: codex ? CODEX_ROLE_MODELS : undefined };
      case "Task review model":
        return { defaultLabel: taskReviewModelDefaultLabel(h, cellFor(settings.task_review_models, h)), models: codex ? CODEX_ROLE_MODELS : undefined };
      default:
        return { defaultLabel: workerModelDefaultLabel(h, workerModelCell(h)), allowCustom: codex ? true : undefined };
    }
  };

  const modelControl = (field: ModelsField, ids: ModelsCellIds) => (
    <ModelSelect
      key={pickerGeneration}
      id={ids.control}
      value={draft[field.key] ?? ""}
      onChange={(v) => setValue(field.key, v)}
      harness={field.family!}
      {...modelPicker(field)}
      customAriaLabel={`${fieldLabel(field)}, custom model ID`}
      ariaDescribedBy={ids.describedBy}
      onCustomModeChange={(custom) => setCustomMode((m) => (m[field.key] === custom ? m : { ...m, [field.key]: custom }))}
    />
  );

  // Effort Defaults: planning is Same as the run and has no Runtime default (the server refuses
  // it); task review follows the worker effort and keeps Runtime default; a checker's Default is
  // the cross-check default and, like planning, offers no Runtime default.
  const effortPicker = (field: ModelsField): { defaultLabel: string; runtimeLabel?: string } => {
    const h = field.family!;
    if ("pin" in field.target) return { defaultLabel: CHECKER_EFFORT_DEFAULT_LABEL };
    switch (field.row) {
      case "Planning effort":
        return { defaultLabel: planningEffortDefaultLabel(cellFor(settings.planning_efforts, h)) };
      case "Task review effort":
        return { defaultLabel: taskReviewEffortDefaultLabel(cellFor(settings.task_review_efforts, h)), runtimeLabel: "Runtime default" };
      default:
        return {
          defaultLabel: workerEffortDefaultLabel(effortCell(h)),
          runtimeLabel: h === "codex" ? codexRuntimeDefaultLabel(cellFor(settings.effort_settings, "codex")) : "Runtime default · value not reported",
        };
    }
  };

  const effortControl = (field: ModelsField, ids: ModelsCellIds) => (
    <EffortSelect
      id={ids.control}
      value={draft[field.key] ?? ""}
      onChange={(v) => setValue(field.key, v)}
      ariaDescribedBy={ids.describedBy}
      {...effortPicker(field)}
    />
  );

  // Judge runs on: what the drafted choice does, a credential fact for an explicit harness, and,
  // when the judge is off, where to turn it on. None of them disables the select.
  const judgeField = fieldFor("Judge runs on", null);
  const judgeOnHarness = judgeRunsOn === "claude" || judgeRunsOn === "codex" ? judgeRunsOn : null;
  const judgeUnusable = judgeOnHarness ? effective?.harnesses?.find((x) => x.harness === judgeOnHarness)?.usable === false : false;
  const judgeOff = user?.judge_enabled === false && !judgeEnforcedByAdmin;
  const routingCell = (
    <ModelsCell
      {...cellProps(judgeField)}
      reason={
        judgeUnusable && judgeOnHarness
          ? `${harnessName(judgeOnHarness)} has no usable credential, so the judge is skipped until you add one. You can still save this choice.`
          : null
      }
      note={
        <>
          {judgeRunsOnHelp(judgeRunsOn)}
          {judgeOff && <> The judge is off for your runs; turn it on in {RUN_DEFAULTS_LINK}.</>}
        </>
      }
      wide
    >
      {(ids) => (
        <Select
          id={ids.control}
          aria-describedby={ids.describedBy}
          className="max-w-sm"
          value={judgeRunsOn}
          onChange={(e) => setValue(judgeField.key, e.target.value)}
        >
          <option value="">Same as the run</option>
          <option value="claude">Claude</option>
          <option value="codex">Codex</option>
        </Select>
      )}
    </ModelsCell>
  );

  // Summaries run on is a fact, not a setting: summaries run inline on the run's own engine and
  // credential (#4156 tracks a choice), so it reads like a cell but offers no control.
  const summaryRoutingFact = (
    <div className="min-w-0 space-y-1.5 border-l-2 border-transparent pl-3 @[49rem]:col-span-2">
      <p className="text-sm text-fg">Same as the run</p>
      <p className="max-w-prose text-xs text-faint">
        Summaries run on the run&rsquo;s own engine and credential: Claude runs use the Claude summary model,
        Codex runs the Codex one. Choosing another engine is not available yet.
      </p>
    </div>
  );

  const pairCells = (row: string) =>
    (["claude", "codex"] as const).map((h) => {
      const field = fieldFor(row, h);
      return (
        <ModelsCell key={field.key} {...cellProps(field)}>
          {(ids) => (field.kind === "model" ? modelControl(field, ids) : effortControl(field, ids))}
        </ModelsCell>
      );
    });

  const harnessField = fieldFor("Default harness", null);
  const dirtyRows = dirty.map(fieldLabel);

  return (
    <form onSubmit={(e) => void save(e)} className="space-y-4 pb-6" noValidate>
      <UnsavedModelsGuard
        dirtyCount={dirty.length}
        saving={saving}
        // The PUT may have committed: leaving must not read as a discard (PRD: explain the
        // uncertainty, never claim a rollback). A reload after a lost response may still
        // predate the earlier write.
        saveUncertainty={uncertainty}
        requestMayStillFinish={requestMayStillFinish}
        viewerId={viewerId}
        onDiscard={discard}
      />
      <fieldset disabled={busy} className="min-w-0 space-y-4 disabled:opacity-80">
        <legend className="sr-only">Model settings</legend>
        {/* A little less side padding below sm, so a phone-width select fits its Default value. */}
        <Card className="@container space-y-1 max-sm:px-4">
          <p id="models-precedence" className="max-w-prose text-sm text-muted">
            Explicit run choices win in both phases, and a compatible schedule model pin overrides these
            defaults. Each cell shows the saved effective value and where it comes from; an edit
            changes it only after you save.
          </p>

          {/* Default harness: one choice for both families. */}
          <div className={`border-t border-edge pt-4 mt-4 ${ROW_GRID}`}>
            <div className={ROW_NAME}>
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

          {GROUPS.map((group) => (
            <div key={group.title} role="group" aria-labelledby={`models-group-${group.id}`} className="pt-6">
              <div className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 pb-2">
                <h3 id={`models-group-${group.id}`} className="text-sm font-semibold text-fg">
                  {group.title}
                </h3>
                <p className="max-w-prose text-xs text-faint">{group.note}</p>
              </div>
              {/* Column headers, repeated per group so a long grid never loses its columns:
                  visual only (each control names its family itself), and hidden where the
                  cells stack and carry their own family label. */}
              <div aria-hidden="true" className={`hidden pb-2 ${ROW_GRID}`}>
                <span className="hidden @[60rem]:block" />
                {(["claude", "codex"] as const).map((h) => (
                  <span key={h} className="flex items-center gap-2 border-l-2 border-transparent pl-3 text-xs font-semibold text-muted">
                    {h === "claude" ? <ClaudeIcon className="h-3.5 w-3.5 text-brand-claude" /> : <OpenAIIcon className="h-3.5 w-3.5" />}
                    {harnessName(h)}
                  </span>
                ))}
              </div>
              {group.rows.map(({ row, description, type = "pair", sub }) => (
                <div
                  key={row}
                  data-row={row}
                  className={`${sub ? "pt-1 pb-4" : type === "pair" ? "border-t border-edge py-4" : "border-t border-edge pt-4 pb-3"} ${ROW_GRID}`}
                >
                  <div className={cx(ROW_NAME, sub && "@[60rem]:pl-3")}>
                    <h4 className={cx("text-sm", sub ? "text-muted" : "font-medium text-fg")}>{row}</h4>
                    <p className="text-xs text-faint">{description}</p>
                  </div>
                  {type === "routing" ? (
                    routingCell
                  ) : type === "fact" ? (
                    summaryRoutingFact
                  ) : (
                    <div className="space-y-4 @[49rem]:contents">{pairCells(row)}</div>
                  )}
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
        blockedCount={blocked.size}
        outcome={outcome}
        reloading={reloading}
        uncertain={uncertainty !== null}
        requestMayStillFinish={requestMayStillFinish}
        onDiscard={discard}
        onReload={() => void reload()}
      />
    </form>
  );
}
