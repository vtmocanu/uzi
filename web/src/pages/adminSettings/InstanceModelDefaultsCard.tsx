import { useLayoutEffect, useState, type FormEvent } from "react";
import {
  api,
  type AppSettings,
  type SettingSource,
  type SettingsResponse,
  type UpdateSettingsPayload,
} from "../../lib/api";
import { ApiError, errorMessage } from "../../lib/apiError";
import { ModelSelect } from "../../components/ModelSelect";
import { ClaudeIcon, OpenAIIcon } from "../../components/icons";
import { modelFieldWarning } from "../../lib/agentTemplates";
import { CODEX_CURATED_MODELS, CODEX_ROLE_MODELS } from "../../lib/codexModels";
import { Alert, Button, Card, Input, SectionTitle } from "../../components/ui";

type ModelKey = "judge_model" | "codex_judge_model" | "summary_model" | "codex_summary_model";
type Family = "Claude" | "Codex";

// One entry per grid cell: the single table that drives the four lanes. `models` is a
// closed list override for the picker (omitted = the harness's curated list); Claude lanes
// keep the picker's default of allowing a custom ID, Codex lanes stay closed.
interface Lane {
  key: ModelKey;
  family: Family;
  row: "Judge model" | "Summary model";
  // Plural noun for the legacy-Codex-id warning copy.
  plural: "judges" | "summaries";
  models?: readonly string[];
}

const LANES: readonly Lane[] = [
  { key: "judge_model", family: "Claude", row: "Judge model", plural: "judges" },
  { key: "codex_judge_model", family: "Codex", row: "Judge model", plural: "judges" },
  { key: "summary_model", family: "Claude", row: "Summary model", plural: "summaries" },
  { key: "codex_summary_model", family: "Codex", row: "Summary model", plural: "summaries", models: CODEX_ROLE_MODELS },
];

const ROWS = [
  {
    label: "Judge model" as const,
    note: "Judge advice feeds self-improvement, so its built-in is a strong model.",
  },
  {
    label: "Summary model" as const,
    note: "Two short summaries on every run, so its built-in is fast and near-free.",
  },
];

const KEYS = LANES.map((l) => l.key);
const CUSTOM_EMPTY_MESSAGE = "Enter a model ID, or choose Default.";
const FIELD_ERROR_RE = /^(judge_model|codex_judge_model|summary_model|codex_summary_model): ([\s\S]+)$/;

const laneId = (key: ModelKey) => `model-default-${key}`;
const laneName = (l: Lane) => `${l.row}, ${l.family}`;

type Values = Record<ModelKey, string>;
type View = { settings: AppSettings; sources: Record<string, SettingSource> };

// pinnedModel is the instance's own pin for one lane: "" when it follows the built-in
// (source "default"), else the stored value. The select's empty option is the reset, so an
// unpinned instance shows Default selected, not the resolved model.
function pinnedModel(key: ModelKey, view: View): string {
  return view.sources[key] === "default" ? "" : (view.settings[key] ?? "");
}

function pinnedValues(view: View): Values {
  return Object.fromEntries(KEYS.map((k) => [k, pinnedModel(k, view)])) as Values;
}

// defaultOptionLabel names the built-in the reset follows. With no defaults from the
// server (an older API) it stays neutral rather than guessing a model.
function defaultOptionLabel(builtin: string | undefined): string {
  return builtin ? `Default · ${builtin} (built-in)` : "Default (built-in)";
}

function FamilyLabel({ family }: { family: Family }) {
  return (
    <span className="inline-flex items-center gap-1.5 text-sm font-medium text-fg">
      {family === "Claude" ? <ClaudeIcon className="h-4 w-4" /> : <OpenAIIcon className="h-4 w-4 text-fg" />}
      {family}
    </span>
  );
}

// InstanceModelDefaultsCard is the one admin surface for the per-harness judge and summary
// model defaults (issue #3259): a Setting x (Claude | Codex) grid saved by a single PUT that
// carries only the changed keys. Default (the empty value) follows uzi's built-in model; a
// user's own choice under Run defaults wins over these. Worker models have no instance default.
export function InstanceModelDefaultsCard({
  settings,
  sources,
  defaults,
  onSaved,
}: {
  settings: AppSettings;
  sources: Record<string, SettingSource>;
  // The compiled-in defaults from the admin GET, for the "Default · <model> (built-in)" label.
  defaults: Record<string, string>;
  onSaved: (resp: SettingsResponse, sent: UpdateSettingsPayload) => void;
}) {
  // The last server view this card has seen: the baseline each lane's dirtiness is measured
  // against, replaced only by a successful save.
  const [view, setView] = useState<View>({ settings, sources });
  const baseline = pinnedValues(view);
  const [values, setValues] = useState<Values>(baseline);
  const [custom, setCustom] = useState<Record<ModelKey, boolean>>({
    judge_model: false,
    codex_judge_model: false,
    summary_model: false,
    codex_summary_model: false,
  });
  const [fieldErrors, setFieldErrors] = useState<Partial<Record<ModelKey, string>>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  const isEnv = (key: ModelKey) => view.sources[key] === "env";
  const dirtyOf = (key: ModelKey) => !isEnv(key) && values[key] !== baseline[key];
  const isCodexIdInClaude = (l: Lane) => l.family === "Claude" && CODEX_ROLE_MODELS.includes(values[l.key].trim());
  // The Codex judge lane is a closed list (the server accepts curated ids only), yet a stored
  // non-curated value still renders as itself so it never vanishes: flag it, and refuse to
  // save one as a change.
  const notCuratedCodexJudge = (l: Lane) =>
    l.key === "codex_judge_model" && values[l.key].trim() !== "" && !CODEX_CURATED_MODELS.includes(values[l.key].trim());
  const customEmpty = (l: Lane) => !isEnv(l.key) && custom[l.key] && values[l.key].trim() === "";

  const dirty = LANES.some((l) => dirtyOf(l.key));
  // A malformed stored ID blocks only when it is the change being saved: an untouched stored
  // value is never sent, so it must not lock the other lanes.
  const blocked = LANES.some(
    (l) =>
      customEmpty(l) ||
      (dirtyOf(l.key) &&
        (modelFieldWarning(values[l.key]) !== "" || isCodexIdInClaude(l) || notCuratedCodexJudge(l))),
  );

  // ModelSelect has no invalid prop, so mirror the field error onto its select by id.
  useLayoutEffect(() => {
    for (const l of LANES) {
      const el = document.getElementById(laneId(l.key));
      if (!el) continue;
      if (fieldErrors[l.key]) el.setAttribute("aria-invalid", "true");
      else el.removeAttribute("aria-invalid");
    }
  }, [fieldErrors, view]);

  const setLane = (key: ModelKey, v: string) => {
    setValues((prev) => (prev[key] === v ? prev : { ...prev, [key]: v }));
    setFieldErrors((prev) => {
      if (!prev[key]) return prev;
      const { [key]: _gone, ...rest } = prev;
      return rest;
    });
  };

  const setLaneCustom = (key: ModelKey, c: boolean) =>
    setCustom((prev) => (prev[key] === c ? prev : { ...prev, [key]: c }));

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (busy || !dirty || blocked) return;
    setError("");
    setNotice("");
    setFieldErrors({});
    setBusy(true);
    try {
      // Only the changed, non-env keys, in one request; "" is the reset to the built-in.
      const payload: Partial<Record<ModelKey, string>> = {};
      for (const l of LANES) if (dirtyOf(l.key)) payload[l.key] = values[l.key];
      const resp = await api.updateSettings(payload);
      onSaved(resp, payload);
      const next: View = { settings: resp.settings, sources: resp.sources };
      setView(next);
      setValues(pinnedValues(next));
      setNotice("Model defaults saved.");
    } catch (err) {
      if (err instanceof ApiError && err.status === 400) {
        const m = FIELD_ERROR_RE.exec(err.message);
        if (m) {
          setFieldErrors({ [m[1] as ModelKey]: m[2] });
          return;
        }
      }
      if (err instanceof ApiError && (err.status === 400 || err.status === 409)) {
        setError(err.message);
      } else {
        setError(
          `${errorMessage(err, "Failed to save model defaults")} The save may or may not have been applied. Reload the page to see the current values.`,
        );
      }
    } finally {
      setBusy(false);
    }
  };

  const renderCell = (l: Lane) => {
    const id = laneId(l.key);
    const stateId = `${id}-state`;
    const errId = `${id}-error`;
    const fieldError = fieldErrors[l.key];
    const value = values[l.key];
    const builtin = defaults[l.key];
    const warning = modelFieldWarning(value);
    const legacyCodexInClaude = !dirtyOf(l.key) && isCodexIdInClaude(l);

    return (
      <div key={l.key} className="min-w-0 space-y-1.5">
        {/* Below sm the header row is hidden, so each cell names its own family. */}
        <div className="sm:hidden" aria-hidden="true">
          <FamilyLabel family={l.family} />
        </div>
        <label htmlFor={id} className="sr-only">
          {laneName(l)}
        </label>
        {isEnv(l.key) ? (
          <>
            <Input id={id} readOnly value={view.settings[l.key] ?? ""} aria-describedby={stateId} />
            <p id={stateId} className="text-xs text-faint">
              Set from the environment
            </p>
          </>
        ) : (
          <>
            <ModelSelect
              id={id}
              harness={l.family === "Claude" ? "claude" : "codex"}
              models={l.models}
              value={value}
              onChange={(v) => setLane(l.key, v)}
              onCustomModeChange={(c) => setLaneCustom(l.key, c)}
              defaultLabel={defaultOptionLabel(builtin)}
              customAriaLabel={`Custom ${l.row.toLowerCase()} ID, ${l.family}`}
              ariaDescribedBy={fieldError ? `${stateId} ${errId}` : stateId}
            />
            <p id={stateId} className="text-xs text-faint">
              {value === ""
                ? "Follows the built-in model."
                : builtin
                  ? `Pinned · built-in is ${builtin}; choose Default to follow it again.`
                  : "Pinned. Choose Default to follow the built-in again."}
            </p>
            {legacyCodexInClaude && (
              <p className="text-xs text-warn" role="status">
                Not a Claude model: Claude {l.plural} use the built-in {builtin ?? "model"}. Choose Default to clear
                it.
              </p>
            )}
            {dirtyOf(l.key) && isCodexIdInClaude(l) && (
              <p className="text-xs text-danger" role="alert">
                That is a Codex model. Choose it under the Codex column instead.
              </p>
            )}
            {notCuratedCodexJudge(l) && !isEnv(l.key) && (
              <p className="text-xs text-warn" role="status">
                Not a listed Codex judge model: Codex judges accept only the models in this list. Choose one, or
                Default.
              </p>
            )}
            {customEmpty(l) && (
              <p className="text-xs text-danger" role="alert">
                {CUSTOM_EMPTY_MESSAGE}
              </p>
            )}
            {warning && <Alert tone="warning" message={warning} />}
            {fieldError && (
              <p id={errId} className="text-xs text-danger" role="alert">
                {fieldError}
              </p>
            )}
          </>
        )}
      </div>
    );
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Instance model defaults</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Used when a user leaves a model on Default; each user can pick their own under Settings &rarr; Run
          defaults.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      <form onSubmit={save} className="space-y-4">
        {/* Locked while a save is in flight so an edit made then is not silently replaced by the response. */}
        <fieldset disabled={busy} className="min-w-0 space-y-3 border-0 p-0">
          <div className="hidden gap-4 border-b border-edge pb-2 text-sm font-medium text-muted sm:grid sm:grid-cols-[180px_1fr_1fr]">
            <span>Setting</span>
            <FamilyLabel family="Claude" />
            <FamilyLabel family="Codex" />
          </div>
          {ROWS.map((row, i) => (
            <div
              key={row.label}
              className={`grid gap-3 sm:grid-cols-[180px_1fr_1fr] sm:gap-4 ${i > 0 ? "border-t border-edge pt-3" : ""}`}
            >
              <div className="min-w-0">
                <div className="text-sm font-medium text-fg">{row.label}</div>
                <p className="mt-0.5 text-xs text-faint">{row.note}</p>
              </div>
              {LANES.filter((l) => l.row === row.label).map(renderCell)}
            </div>
          ))}
        </fieldset>

        <p className="border-t border-edge pt-3 text-xs text-faint">
          Default follows uzi&rsquo;s built-in model, including future changes to it; picking a model pins it.
          Each column offers only that harness&rsquo;s models, with the same validation users get. A user&rsquo;s
          own choice under Settings &rarr; Run defaults wins over these. Worker models have no instance default.
        </p>

        <Button type="submit" disabled={busy || !dirty || blocked}>
          {busy ? "Saving…" : "Save model defaults"}
        </Button>
      </form>
    </Card>
  );
}
