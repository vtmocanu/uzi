// HarnessPicker — the shared, controlled harness-choice picker (PRD #1429 M4a).
//
// Modeled on TokenPicker: a labeled <select> exposing the explicit states a human can
// pick — "inherit" (let the server's D11 resolver choose, the true default) plus
// "claude"/"codex". Reused at the start-run flow (IssueView/Board) and the schedule
// modal; the default harness itself is chosen in Settings → Models.
//
// The CALLER decides whether to render this at all: it appears only when BOTH
// harnesses are usable (lib/harnessSelection.ts's bothHarnessesUsable) — a
// single-harness user never sees it, matching D2's "no redundant picker" rule.

import type { HarnessSelection } from "../lib/harnessSelection";
import { Select } from "./ui";

export function HarnessPicker({
  value,
  onChange,
  label = "Harness",
  id,
  disabled = false,
  className = "",
  inheritLabel = "Use my default",
}: {
  value: HarnessSelection;
  onChange: (next: HarnessSelection) => void;
  // Accessible name for the <select>. A visible <label>/<Field> may wrap it instead;
  // this is the fallback so the control is never unlabeled.
  label?: string;
  id?: string;
  disabled?: boolean;
  className?: string;
  // Text of the "inherit" option; callers pass their own wording of what inherit resolves to.
  inheritLabel?: string;
}) {
  return (
    <Select
      id={id}
      aria-label={label}
      disabled={disabled}
      className={className}
      value={value}
      onChange={(e) => onChange(e.target.value as HarnessSelection)}
    >
      <option value="inherit">{inheritLabel}</option>
      <option value="claude">Claude</option>
      <option value="codex">Codex</option>
    </Select>
  );
}
