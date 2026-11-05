import { useEffect, useRef, useState } from "react";
import type { Harness } from "../lib/api";
import { CODEX_CURATED_MODELS } from "../lib/codexModels";
import { Input, Select } from "./ui";

// The curated model aliases offered as first-class options, one closed list PER
// HARNESS (PRD #1429 D6): the two vocabularies are product-owned and do not merge.
// Claude keeps today's aliases (this is the single shared source for that set, PRD
// #17 risk: alias drift — reused by the agent-template editor and the per-user
// default-model setting) plus a free-text "Other…" custom escape hatch. Codex's
// picker has EXACTLY `gpt-6-astra`/`gpt-5.6-sol`/`gpt-6-sol`/`gpt-6.1-sol` as curated
// options, with no catalog discovery — matching D6's "the product-owned Codex picker
// remains exactly" rule. Custom entry defaults off for Codex; worker defaults and
// dedicated checker pins explicitly opt in.
const CLAUDE_MODEL_ALIASES = ["opus", "sonnet", "haiku", "fable"] as const;
// Mirror agent/src/codex/codex-executor.ts CODEX_PRODUCTION_PROVIDER.model.
export const DEFAULT_CODEX_MODEL = "gpt-6.1-sol";
// The curated Codex list lives in lib/codexModels.ts (PRD #3245), pinned to the shared
// fixtures/codex-models/models.json; it includes DEFAULT_CODEX_MODEL.
const CODEX_MODEL_ALIASES = CODEX_CURATED_MODELS;

// aliasesForHarness is the single place this component resolves which curated list
// applies. Defaults to Claude for an omitted/unrecognised harness — the safe,
// backward-compatible direction: a caller that never names a harness gets the Claude
// vocabulary, byte-identical to before PRD #1429 M4a.
function aliasesForHarness(harness: Harness | undefined): readonly string[] {
  return harness === "codex" ? CODEX_MODEL_ALIASES : CLAUDE_MODEL_ALIASES;
}

type Mode = "inherit" | string | "custom";

function deriveMode(value: string, aliases: readonly string[]): Mode {
  const v = value.trim();
  if (v === "") return "inherit";
  return aliases.includes(v) ? v : "custom";
}

// ModelSelect is the shared model picker: a dropdown of the curated aliases plus
// a caller-labelled Default (empty) option (omitted when defaultLabel is null) and —
// when custom entry is allowed — an "Other…" custom free-text model ID. The effective
// model string is surfaced through onChange ("" = default). An incoming value that is not one of the CURRENT harness's aliases
// initializes into the custom state with its text prefilled — never silently reset to
// inherit (PRD #17 §2) — even when custom entry is disabled, so a value that predates a
// harness switch is still visible rather than vanishing. Submit gating stays with the
// caller: it keeps passing the emitted value through modelFieldWarning, so an
// injection-suspect custom ID still blocks the form.
//
// PRD #1551 M3: `allowCustom` is an explicit opt-in that overrides the harness default.
// Historically custom entry was "Claude yes, Codex no" (PRD #1429 D6). Codex's picker
// stays closed for schedule/template callers. The per-user Codex worker-model lane
// and dedicated plan cross-check pins on Run Defaults opt into the "Other…" escape
// hatch. Callers that omit allowCustom keep the harness-derived default.
export function ModelSelect({
  value,
  onChange,
  id,
  defaultLabel,
  customAriaLabel = "Custom model ID",
  harness,
  allowCustom,
  models,
  ariaDescribedBy,
  onCustomModeChange,
}: {
  value: string;
  onChange: (model: string) => void;
  id?: string;
  // Text of the empty-value option, naming where the default really comes from at
  // this call site. Required: there is no generic fallback. `null` means the surface
  // has no Default option (the value must be chosen); an empty selection then renders
  // a disabled "Choose a model" placeholder.
  defaultLabel: string | null;
  customAriaLabel?: string;
  // The harness this selection is validated against (PRD #1429 D6). Omitted ⇒
  // Claude, so every pre-M4a caller (the agent-template editor, the schedule modal
  // before its own harness picker lands) renders exactly as before.
  harness?: Harness;
  // Whether the "Other… (custom model ID)" menu entry is offered. Omitted ⇒ the
  // historical harness default (Claude allows custom, Codex does not), so
  // ScheduleModal / AgentTemplateEditor render unchanged. Run Defaults' Codex lane
  // and dedicated plan cross-check pins pass `allowCustom` to open that escape hatch
  // (PRD #1551 D5 and #2151).
  allowCustom?: boolean;
  // Closed curated-list override (PRD #3248). Custom entry requires explicit
  // allowCustom when provided; omission preserves the harness defaults.
  models?: readonly string[];
  // Id(s) of a hint that describes the select (e.g. an "In effect" line), PRD #3245.
  ariaDescribedBy?: string;
  // Reports whether the picker is in "Other (custom model ID)" mode, so a caller can
  // refuse to save an empty custom ID instead of silently sending "" (= Default).
  onCustomModeChange?: (custom: boolean) => void;
}) {
  const aliases = models ?? aliasesForHarness(harness);
  const customAllowed = allowCustom ?? (models === undefined && harness !== "codex");
  // Compare list contents rather than array identity: callers may build a fresh
  // array each render without changing the vocabulary.
  const aliasesKey = JSON.stringify(aliases);
  const lastAliasesKey = useRef(aliasesKey);
  // A provider-specific placeholder example, so the custom input hints at the right
  // vocabulary (a Codex id looks like gpt-…, a Claude id like claude-…).
  const customPlaceholder = harness === "codex" ? "gpt-…" : "claude-…";
  const [mode, setMode] = useState<Mode>(() => deriveMode(value, aliases));
  const [custom, setCustom] = useState(() => (deriveMode(value, aliases) === "custom" ? value : ""));
  // What we last emitted, so a parent echoing our own value back does not
  // re-derive state — that would flip an intentionally-emptied custom field
  // back to inherit. Only an externally-driven change (async load, reset)
  // re-syncs the internal state.
  const lastEmitted = useRef(value);
  // Pair this render's value with its emitted snapshot. An event between commit
  // and the passive effect may already have advanced lastEmitted.current.
  const emittedAtRender = lastEmitted.current;
  // The harness this instance last re-derived against, so a harness switch (which
  // moves the SAME value between "curated" and "custom" under the two vocabularies,
  // e.g. "opus" is curated under Claude but custom under Codex) is never missed.
  const lastHarness = useRef(harness);

  // Re-derive from the current value, harness and vocabulary together so async
  // loads and list changes cannot leave a selection from the previous vocabulary.
  useEffect(() => {
    const harnessChanged = harness !== lastHarness.current;
    lastHarness.current = harness;
    const aliasesChanged = aliasesKey !== lastAliasesKey.current;
    lastAliasesKey.current = aliasesKey;
    // An equal new array must preserve an intentionally empty custom input.
    if (!harnessChanged && !aliasesChanged && value === emittedAtRender) return;
    lastEmitted.current = value;
    const m = deriveMode(value, aliases);
    setMode(m);
    setCustom(m === "custom" ? value : "");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [value, harness, aliasesKey]);

  const isCustom = mode === "custom";
  const customVisible = isCustom && (models === undefined || customAllowed);
  useEffect(() => {
    onCustomModeChange?.(isCustom);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isCustom]);

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
      <Select id={id} aria-describedby={ariaDescribedBy} value={mode} onChange={(e) => onSelect(e.target.value as Mode)}>
        {defaultLabel !== null ? (
          <option value="inherit">{defaultLabel}</option>
        ) : (
          mode === "inherit" && (
            <option value="inherit" disabled>
              Choose a model
            </option>
          )
        )}
        {aliases.map((m) => (
          <option key={m} value={m}>
            {m}
          </option>
        ))}
        {/* Preserve existing values without opening custom entry on a closed list.
            A disabled current value prevents the select from displaying Default
            while the parent still holds an unsupported pin. */}
        {isCustom && !customVisible && <option value="custom" disabled>{value}</option>}
        {(customAllowed || customVisible) && <option value="custom">Other (custom model ID)…</option>}
      </Select>
      {customVisible && (
        <Input
          value={custom}
          onChange={(e) => onCustom(e.target.value)}
          placeholder={customPlaceholder}
          aria-label={customAriaLabel}
          autoCapitalize="off"
          autoCorrect="off"
          spellCheck={false}
        />
      )}
    </div>
  );
}
