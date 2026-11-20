import { useEffect, useState, type FormEvent } from "react";
import { api, type SettingSource, type SettingsResponse, type UpdateSettingsPayload } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Field, Input, Select } from "../../components/ui";
import { BUILTIN_WORKER_SIZE_NAMES, sizeLabel, sizeOptionLabel, type NamedWorkerSize } from "../../lib/workerSizes";

// SettingControl edits ONE app setting on its own: it PUTs only its key, "Reset to
// default" PUTs the empty string (the server's reset contract), and an env-sourced key
// is locked because the server refuses a write to it (409). It shows the effective
// value's provenance so an admin can tell a saved value from the compiled-in default
// (PRD #4212). Used by the worker cards, which keep their own switch/quota saves.

const SOURCE_LABEL: Record<SettingSource, string> = {
  env: "Set by environment",
  db: "Saved",
  default: "Default",
};

export type SettingControlKind = "number" | "text" | "size";

export function SettingControl({
  settingKey,
  label,
  kind,
  value,
  source,
  defaultValue,
  helper,
  min,
  max,
  sizeOptions,
  onSaved,
}: {
  settingKey: string;
  label: string;
  kind: SettingControlKind;
  /** The effective value from the settings response ("" when an older API omits the key). */
  value: string;
  /** Where the effective value comes from; absent is treated as the default. */
  source?: SettingSource;
  /** Compiled-in default, shown as a hint when the response carries `defaults`. */
  defaultValue?: string;
  helper?: string;
  min?: number;
  max?: number;
  /** kind "size": the sizes to offer, with their quantities, from GET /admin/worker-sizes
   *  (PRD #4213). Absent while that list is loading or after it failed: the builtin names
   *  are offered bare then, because a quantity this control cannot vouch for would mislead. */
  sizeOptions?: readonly NamedWorkerSize[];
  onSaved: (resp: SettingsResponse, sent: UpdateSettingsPayload) => void;
}) {
  const id = `setting-${settingKey}`;
  const [draft, setDraft] = useState(value);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  // Follow the saved value after a save, a reset, or a sibling card's save.
  useEffect(() => setDraft(value), [value]);

  const sizeNames: readonly string[] = sizeOptions ? sizeOptions.map((s) => s.name) : BUILTIN_WORKER_SIZE_NAMES;
  const effectiveSource: SettingSource = source ?? "default";
  const isEnv = effectiveSource === "env";
  const dirty = draft.trim() !== value;
  const invalid =
    kind === "number" && dirty && !(/^\d+$/.test(draft.trim()) && Number(draft) >= (min ?? 0) && Number(draft) <= (max ?? Infinity))
      ? `Must be a whole number from ${min ?? 0} to ${max}`
      : kind === "text" && dirty && draft.trim() === ""
        ? "Enter a value, or use Reset to default"
        : null;

  const put = async (next: string, done: string) => {
    setError("");
    setNotice("");
    setBusy(true);
    try {
      const payload: UpdateSettingsPayload = { [settingKey]: next };
      const resp = await api.updateSettings(payload);
      onSaved(resp, payload);
      // A reset whose saved value already equalled the default leaves `value` unchanged, so
      // the effect above would not drop a dirty draft; resync it from the response here.
      setDraft(resp.settings[settingKey as keyof typeof resp.settings] ?? value);
      setNotice(done);
    } catch (err) {
      setError(errorMessage(err, `Failed to save ${label.toLowerCase()}`));
    } finally {
      setBusy(false);
    }
  };

  const save = (e: FormEvent) => {
    e.preventDefault();
    if (busy || isEnv || !dirty || invalid) return;
    void put(draft.trim(), `${label} saved.`);
  };

  const describedBy = [helper ? `${id}-help` : "", `${id}-source`, isEnv ? `${id}-env` : "", invalid ? `${id}-error` : ""].filter(Boolean).join(" ");
  const common = {
    id,
    value: draft,
    disabled: isEnv || busy,
    "aria-describedby": describedBy,
    onChange: (e: { target: { value: string } }) => {
      setDraft(e.target.value);
      setError("");
      setNotice("");
    },
  };

  return (
    <form onSubmit={save} className="space-y-2 border-t border-edge pt-4" noValidate>
      <Field label={label} htmlFor={id}>
        {kind === "size" ? (
          <Select {...common}>
            {!sizeNames.includes(draft) && (
              <option value={draft} disabled>
                {draft === "" ? "Select a size" : draft}
              </option>
            )}
            {sizeOptions
              ? sizeOptions.map((s) => (
                  <option key={s.name} value={s.name}>
                    {sizeOptionLabel(s)}
                  </option>
                ))
              : BUILTIN_WORKER_SIZE_NAMES.map((name) => (
                  <option key={name} value={name}>
                    {sizeLabel(name)}
                  </option>
                ))}
          </Select>
        ) : (
          <Input
            {...common}
            type={kind === "number" ? "number" : "text"}
            min={kind === "number" ? min : undefined}
            max={kind === "number" ? max : undefined}
            step={kind === "number" ? 1 : undefined}
            aria-invalid={invalid !== null}
          />
        )}
      </Field>
      {helper && (
        <p id={`${id}-help`} className="text-xs text-muted">
          {helper}
        </p>
      )}
      <p id={`${id}-source`} className="text-xs text-muted">
        {SOURCE_LABEL[effectiveSource]}
        {defaultValue !== undefined && defaultValue !== "" ? ` · built-in default ${defaultValue}` : ""}
      </p>
      {isEnv && (
        <p id={`${id}-env`} className="text-xs text-muted">This setting is fixed by an environment variable and cannot be changed here.</p>
      )}
      {invalid && (
        <p id={`${id}-error`} className="text-xs text-danger">
          {invalid}
        </p>
      )}
      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}
      {!isEnv && (
        <div className="flex flex-wrap gap-2">
          <Button type="submit" aria-label={`Save ${label}`} disabled={busy || !dirty || invalid !== null}>
            {busy ? "Saving…" : "Save"}
          </Button>
          {effectiveSource === "db" && (
            <Button
              type="button"
              variant="secondary"
              aria-label={`Reset to default: ${label}`}
              disabled={busy}
              onClick={() => void put("", `${label} reset to default.`)}
            >
              Reset to default
            </Button>
          )}
        </div>
      )}
    </form>
  );
}
