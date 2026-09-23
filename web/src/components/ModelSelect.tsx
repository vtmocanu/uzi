import { useEffect, useRef, useState } from "react";
import type { Harness } from "../lib/api";
import { Input, Select } from "./ui";

// The curated model aliases offered as first-class options, one closed list PER
// HARNESS (PRD #1429 D6): the two vocabularies are product-owned and do not merge.
// Claude keeps today's aliases (this is the single shared source for that set, PRD
// #17 risk: alias drift — reused by the agent-template editor and the per-user
// default-model setting) plus a free-text "Other…" custom escape hatch. Codex's
// picker is EXACTLY `gpt-6-astra`/`gpt-5.6-sol`/`gpt-6-sol` — no custom option, no catalog
// discovery — matching D6's "the product-owned Codex picker remains exactly" rule.
const CLAUDE_MODEL_ALIASES = ["opus", "sonnet", "haiku", "fable"] as const;
const CODEX_MODEL_ALIASES = ["gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol"] as const;

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
// an "Inherit" (empty) option and — when custom entry is allowed — an "Other…"
// custom free-text model ID. The effective model string is surfaced through onChange
// ("" = inherit). An incoming value that is not one of the CURRENT harness's aliases
// initializes into the custom state with its text prefilled — never silently reset to
// inherit (PRD #17 §2) — even when custom entry is disabled, so a value that predates a
// harness switch is still visible rather than vanishing. Submit gating stays with the
// caller: it keeps passing the emitted value through modelFieldWarning, so an
// injection-suspect custom ID still blocks the form.
//
// PRD #1551 M3: `allowCustom` is an explicit opt-in that overrides the harness default.
// Historically custom entry was "Claude yes, Codex no" (PRD #1429 D6). Codex's picker
// stayed closed for schedule/template callers, but the per-user Codex worker-model lane
// on Run Defaults now needs an "Other…" escape hatch, so ONLY that caller passes
// allowCustom. Every other caller omits it and keeps the harness-derived default.
export function ModelSelect({
  value,
  onChange,
  id,
  customAriaLabel = "Custom model ID",
  harness,
  allowCustom,
}: {
  value: string;
  onChange: (model: string) => void;
  id?: string;
  customAriaLabel?: string;
  // The harness this selection is validated against (PRD #1429 D6). Omitted ⇒
  // Claude, so every pre-M4a caller (the agent-template editor, the schedule modal
  // before its own harness picker lands) renders exactly as before.
  harness?: Harness;
  // Whether the "Other… (custom model ID)" menu entry is offered. Omitted ⇒ the
  // historical harness default (Claude allows custom, Codex does not), so
  // ScheduleModal / AgentTemplateEditor render unchanged. Run Defaults' Codex lane
  // passes `allowCustom` to open the escape hatch there and there only (PRD #1551 D5).
  allowCustom?: boolean;
}) {
  const aliases = aliasesForHarness(harness);
  const customAllowed = allowCustom ?? harness !== "codex";
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
  // The harness this instance last re-derived against, so a harness switch (which
  // moves the SAME value between "curated" and "custom" under the two vocabularies,
  // e.g. "opus" is curated under Claude but custom under Codex) is never missed.
  const lastHarness = useRef(harness);

  // ONE effect keyed on BOTH [value, harness], not two separate effects each keyed
  // on one (PRD #1429 M4a). A harness switch and an externally-driven value change can
  // land in the same render (an async settings load seeds both at once). Two
  // independent single-dependency effects would each see only
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
        {(customAllowed || mode === "custom") && <option value="custom">Other (custom model ID)…</option>}
      </Select>
      {mode === "custom" && (
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
