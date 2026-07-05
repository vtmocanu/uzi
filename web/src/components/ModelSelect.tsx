import { useEffect, useRef, useState } from "react";
import { Input, Select } from "./ui";

// The curated model aliases offered as first-class options. This is the single
// shared source for the alias set (PRD #17 risk: alias drift), reused by the
// agent-template editor and the per-user default-model setting. Anything not on
// this list is entered through the "Other…" custom free-text ID.
export const MODEL_ALIASES = ["opus", "sonnet", "haiku", "fable"] as const;

type Mode = "inherit" | (typeof MODEL_ALIASES)[number] | "custom";

function deriveMode(value: string): Mode {
  const v = value.trim();
  if (v === "") return "inherit";
  return (MODEL_ALIASES as readonly string[]).includes(v) ? (v as Mode) : "custom";
}

// ModelSelect is the shared model picker: a dropdown of the curated aliases plus
// an "Inherit" (empty) option and an "Other…" custom free-text model ID. The
// effective model string is surfaced through onChange ("" = inherit). An
// incoming value that is not one of the aliases initializes into the custom
// state with its text prefilled — never silently reset to inherit (PRD #17 §2).
// Submit gating stays with the caller: it keeps passing the emitted value
// through frontmatterFieldWarning, so an injection-suspect custom ID still
// blocks the form.
export function ModelSelect({
  value,
  onChange,
  id,
  customAriaLabel = "Custom model ID",
}: {
  value: string;
  onChange: (model: string) => void;
  id?: string;
  customAriaLabel?: string;
}) {
  const [mode, setMode] = useState<Mode>(() => deriveMode(value));
  const [custom, setCustom] = useState(() => (deriveMode(value) === "custom" ? value : ""));
  // What we last emitted, so a parent echoing our own value back does not
  // re-derive state — that would flip an intentionally-emptied custom field
  // back to inherit. Only an externally-driven change (async load, reset)
  // re-syncs the internal state.
  const lastEmitted = useRef(value);

  useEffect(() => {
    if (value === lastEmitted.current) return;
    lastEmitted.current = value;
    const m = deriveMode(value);
    setMode(m);
    setCustom(m === "custom" ? value : "");
  }, [value]);

  const emit = (v: string) => {
    lastEmitted.current = v;
    onChange(v);
  };

  const onSelect = (next: Mode) => {
    setMode(next);
    if (next === "inherit") emit("");
    else if (next === "custom") emit(custom);
    else emit(next);
  };

  const onCustom = (text: string) => {
    setCustom(text);
    emit(text);
  };

  return (
    <div className="space-y-2">
      <Select id={id} value={mode} onChange={(e) => onSelect(e.target.value as Mode)}>
        <option value="inherit">Inherit (account default)</option>
        {MODEL_ALIASES.map((m) => (
          <option key={m} value={m}>
            {m}
          </option>
        ))}
        <option value="custom">Other (custom model ID)…</option>
      </Select>
      {mode === "custom" && (
        <Input
          value={custom}
          onChange={(e) => onCustom(e.target.value)}
          placeholder="claude-…"
          aria-label={customAriaLabel}
          autoCapitalize="off"
          autoCorrect="off"
          spellCheck={false}
        />
      )}
    </div>
  );
}
