import { useEffect, useRef, useState } from "react";
import type { Harness } from "../lib/api";
import { Input, Select } from "./ui";

// The curated model aliases offered as first-class options, one closed list PER
// HARNESS (PRD #1429 D6): the two vocabularies are product-owned and do not merge.
// Claude keeps today's aliases (this is the single shared source for that set, PRD
// #17 risk: alias drift — reused by the agent-template editor and the per-user
// default-model setting) plus a free-text "Other…" custom escape hatch. Codex's
// picker is EXACTLY `gpt-6-astra`/`gpt-5.6-sol` — no custom option, no catalog
// discovery — matching D6's "the product-owned Codex picker remains exactly" rule.
export const CLAUDE_MODEL_ALIASES = ["opus", "sonnet", "haiku", "fable"] as const;
export const CODEX_MODEL_ALIASES = ["gpt-6-astra", "gpt-5.6-sol"] as const;

// Back-compat alias: every pre-M4a caller that never named a harness gets the
// Claude vocabulary, byte-identical to before this milestone.
export const MODEL_ALIASES = CLAUDE_MODEL_ALIASES;

// aliasesForHarness is the single place a caller (this component, or a harness-aware
// summary) resolves which curated list applies. Defaults to Claude for an omitted/
// unrecognised harness — the safe, backward-compatible direction.
export function aliasesForHarness(harness: Harness | undefined): readonly string[] {
  return harness === "codex" ? CODEX_MODEL_ALIASES : CLAUDE_MODEL_ALIASES;
}

// modelCompatibleWithHarness is the D6 compatibility rule Run Defaults uses to decide
// whether SWITCHING the default harness must reset a stored model to inherit rather
// than persist a knowingly-invalid pair (PRD #1429 D6): blank/inherit is always
// compatible; a Codex target accepts only its two curated aliases (Codex has no
// custom escape hatch); a Claude target accepts anything EXCEPT a known Codex-only
// alias (Claude keeps its curated-plus-custom freedom, but must never inherit a
// Codex-specific model id it cannot run).
export function modelCompatibleWithHarness(model: string, harness: Harness): boolean {
  const trimmed = model.trim();
  if (trimmed === "") return true;
  if (harness === "codex") return (CODEX_MODEL_ALIASES as readonly string[]).includes(trimmed);
  return !(CODEX_MODEL_ALIASES as readonly string[]).includes(trimmed);
}

type Mode = "inherit" | string | "custom";

function deriveMode(value: string, aliases: readonly string[]): Mode {
  const v = value.trim();
  if (v === "") return "inherit";
  return aliases.includes(v) ? v : "custom";
}

// ModelSelect is the shared model picker: a dropdown of the curated aliases plus
// an "Inherit" (empty) option and — for Claude only — an "Other…" custom free-text
// model ID (PRD #1429 D6: Codex's picker is closed, no custom escape hatch). The
// effective model string is surfaced through onChange ("" = inherit). An incoming
// value that is not one of the CURRENT harness's aliases initializes into the custom
// state with its text prefilled — never silently reset to inherit (PRD #17 §2) —
// even for Codex, so a value that predates a harness switch is still visible rather
// than vanishing; the caller (Run Defaults) is what resets an incompatible stored
// model to inherit on an explicit harness switch (D6). Submit gating stays with the
// caller: it keeps passing the emitted value through frontmatterFieldWarning, so an
// injection-suspect custom ID still blocks the form.
export function ModelSelect({
  value,
  onChange,
  id,
  customAriaLabel = "Custom model ID",
  harness,
}: {
  value: string;
  onChange: (model: string) => void;
  id?: string;
  customAriaLabel?: string;
  // The harness this selection is validated against (PRD #1429 D6). Omitted ⇒
  // Claude, so every pre-M4a caller (the agent-template editor, the schedule modal
  // before its own harness picker lands) renders exactly as before.
  harness?: Harness;
}) {
  const aliases = aliasesForHarness(harness);
  const allowCustom = harness !== "codex";
  const [mode, setMode] = useState<Mode>(() => deriveMode(value, aliases));
  const [custom, setCustom] = useState(() => (deriveMode(value, aliases) === "custom" ? value : ""));
  // What we last emitted, so a parent echoing our own value back does not
  // re-derive state — that would flip an intentionally-emptied custom field
  // back to inherit. Only an externally-driven change (async load, reset)
  // re-syncs the internal state.
  const lastEmitted = useRef(value);
  // The harness this instance last re-derived against, so a harness switch (which
  // moves the SAME value between "curated" and "custom" under the two vocabularies,
  // e.g. "opus" is curated under Claude but custom under Codex) is never missed.
  const lastHarness = useRef(harness);

  // ONE effect keyed on BOTH [value, harness], not two separate effects each keyed
  // on one (PRD #1429 M4a). Run Defaults' saveHarness PATCHes default_harness and
  // default_model TOGETHER (D6), so a harness switch and a value reset can land in
  // the same render. Two independent single-dependency effects would each see only
  // ITS OWN dependency change and recompute mode/custom from a value/harness pair
  // that was current when THAT effect last fired — reading the other, unchanged
  // half from a potentially stale closure. A single effect that always recomputes
  // from the CURRENT render's value+harness+aliases together, and fires whenever
  // either changes, cannot go stale relative to itself no matter how renders batch.
  // (RunDefaults.test.tsx's flake under `vitest --coverage` that surfaced this design
  // turned out to be a separate, test-side timing gap — asserting on the derived
  // <select> value before its OWN effect had flushed, fixed there with a `waitFor`
  // — not proof this exact race fired; this merge is still the more defensible
  // design and worth keeping on its own correctness grounds.)
  useEffect(() => {
    const harnessChanged = harness !== lastHarness.current;
    lastHarness.current = harness;
    // Skip re-derivation only when NEITHER the harness changed NOR the value is a
    // genuinely external change (differs from what this instance last emitted).
    if (!harnessChanged && value === lastEmitted.current) return;
    lastEmitted.current = value;
    const m = deriveMode(value, aliases);
    setMode(m);
    setCustom(m === "custom" ? value : "");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [value, harness]);

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
        {aliases.map((m) => (
          <option key={m} value={m}>
            {m}
          </option>
        ))}
        {/* Rendered whenever Claude (always allows custom) OR the picker is ALREADY in
            custom mode (Codex: no NEW custom entry is offered from the menu, but an
            existing custom value that predates a harness switch must still have a
            matching <option value="custom"> — otherwise a controlled <select> whose
            value names an absent option silently falls back to the first option
            (selectedIndex 0), which would show "Inherit" while still emitting the
            stale custom string underneath it. */}
        {(allowCustom || mode === "custom") && <option value="custom">Other (custom model ID)…</option>}
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
