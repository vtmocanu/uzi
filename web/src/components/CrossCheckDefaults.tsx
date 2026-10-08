import { useState } from "react";
import { api } from "../lib/api";
import type { Harness, UserSettings, UserSettingsPatch } from "../lib/apiTypes";
import { checkerModelWarning, normalizeCheckerValue } from "../lib/crossCheckSettings";
import { errorMessage } from "../lib/apiError";
import { Alert, Button } from "./ui";
import { ModelSelect } from "./ModelSelect";
import { EffortSelect } from "./EffortSelect";

const families = ["claude", "codex"] as const;
type Draft = Record<Harness, { model: string; effort: string }>;
function stored(settings: UserSettings): Draft {
  return Object.fromEntries(families.map(harness => {
    const pin = settings.cross_check_pins?.find(p => p.stage === "plan" && p.harness === harness);
    return [harness, { model: pin?.model ?? "", effort: pin?.effort ?? "" }];
  })) as Draft;
}

export function CrossCheckDefaults({ settings, onSaved }: {
  settings: UserSettings; onSaved?: () => void;
}) {
  const [draft, setDraft] = useState(() => stored(settings));
  const [saved, setSaved] = useState(() => stored(settings));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const warnings = families.map(h => checkerModelWarning(draft[h].model, h)).filter(Boolean);
  const dirty = families.some(h => normalizeCheckerValue(draft[h].model) !== saved[h].model || draft[h].effort !== saved[h].effort);
  const save = async () => {
    setError("");
    setBusy(true);
    const cross_check_pins: NonNullable<UserSettingsPatch["cross_check_pins"]> = [];
    for (const harness of families) {
      const cell = draft[harness], previous = saved[harness];
      const patch: NonNullable<UserSettingsPatch["cross_check_pins"]>[number] = { stage: "plan", harness };
      if (normalizeCheckerValue(cell.model) !== previous.model) patch.model = normalizeCheckerValue(cell.model) || null;
      if (cell.effort !== previous.effort) patch.effort = cell.effort || null;
      if ("model" in patch || "effort" in patch) cross_check_pins.push(patch);
    }
    try {
      const { settings: response } = await api.putMySettings({ cross_check_pins });
      setDraft(stored(response));
      setSaved(stored(response));
      onSaved?.();
    } catch (err) {
      setError(errorMessage(err, "Failed to save cross-check defaults"));
    } finally { setBusy(false); }
  };

  return <div className="space-y-3">
    <p id="checker-default-help" className="text-sm text-muted">Checks leads running on the other model family</p>
    <fieldset disabled={busy} className="min-w-0 overflow-x-auto">
      <table aria-label="Cross-check defaults" aria-describedby="checker-default-help" className="w-full text-sm">
        <thead><tr><th scope="col">Cross-check</th><th scope="col">Claude cross-checker</th><th scope="col">Codex cross-checker</th></tr></thead>
        <tbody><tr><th scope="row">Plan cross-check</th>{families.map(harness => {
          const workerModel = settings.cross_check_pins?.find(p => p.stage === "plan" && p.harness === harness)?.worker_default_model;
          const workerEffort = (harness === "claude" ? settings.default_effort : settings.default_codex_effort) || "medium";
          const change = (field: "model" | "effort", value: string) => setDraft(d => ({ ...d, [harness]: { ...d[harness], [field]: value } }));
          return <td key={harness} className="min-w-56 align-top p-2">
            <label htmlFor={`checker-${harness}-model`}>{harness === "claude" ? "Claude" : "Codex"} checker model</label>
            <ModelSelect id={`checker-${harness}-model`} harness={harness} allowCustom value={draft[harness].model}
              customAriaLabel={`Custom ${harness} checker model ID`} onChange={v => change("model", v)}
              defaultLabel={`Default · ${workerModel === undefined || (workerModel === null && harness === "codex") ? "Unavailable" : workerModel ?? "SDK/account default"} (worker default)`} />
            <label htmlFor={`checker-${harness}-effort`}>{harness === "claude" ? "Claude" : "Codex"} checker effort</label>
            <EffortSelect id={`checker-${harness}-effort`} value={draft[harness].effort} onChange={v => change("effort", v)}
              defaultLabel={`Default · ${workerEffort} (worker default)`} />
            {harness === "claude" && <p className="text-muted">Used once Codex-lead runs are cross-checked</p>}
          </td>;
        })}</tr></tbody>
      </table>
    </fieldset>
    {error && <Alert message={error} />}
    {warnings.map((warning, i) => <Alert key={i} tone="warning" message={warning} />)}
    <Button onClick={save} disabled={busy || !dirty || warnings.length > 0}>
      {busy ? "Saving…" : "Save cross-check defaults"}
    </Button>
  </div>;
}
