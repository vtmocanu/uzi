// Per-run / per-schedule harness SELECTION model (PRD #1429 M4a). Distinct from a
// run's ACTUAL resolved harness (Run.harness): this is the CHOICE a human makes
// before a run starts, or a schedule pin. Mirrors lib/credentialOverride.ts's shape
// (the same reason: one vocabulary and one set of body-shaping rules shared by the
// start dialog, the two run-create sites, Run Defaults and the schedule modal).
//
// The server is authoritative (D3): the web only hides/explains controls and shapes
// request bodies. An "inherit" selection sends nothing and lets the server's D11
// resolver pick (the sole usable harness, the user's default, or Claude when both are
// usable and no default is set).

import type { Harness } from "./api";

// HarnessSelection is the picker's state: "inherit" means "let the server resolve it"
// — the common case for a single-harness user (the picker is hidden entirely) and the
// default state even when the picker IS shown (both harnesses usable). "claude"/
// "codex" are explicit selections.
export type HarnessSelection = "inherit" | Harness;

export const INHERIT_HARNESS: HarnessSelection = "inherit";

// runHarnessBody shapes the body for a RUN/CHAT-START create call: `inherit` sends
// nothing (the server's D11 resolves implicitly, byte-identical to a pre-M4a create);
// an explicit choice sends the bare harness string.
export function runHarnessBody(sel: HarnessSelection): Harness | undefined {
  return sel === "inherit" ? undefined : sel;
}

// scheduleHarnessPatch shapes the body for a SCHEDULE create/patch (presence-aware,
// mirroring credentialOverride.ts's scheduleCredentialPatch): the user never touched
// the control → undefined → OMIT the field, so the server's seed-and-keep preserves
// the stored pin on an unrelated edit; the user explicitly chose "inherit" (a
// deliberate clear) → null, which reaches the validator and nulls the stored pin; any
// other picked value → the bare harness string.
export function scheduleHarnessPatch(
  sel: HarnessSelection,
  touched: boolean,
): Harness | null | undefined {
  if (!touched) return undefined;
  return sel === "inherit" ? null : sel;
}

// selectionFromHarness derives a picker selection from a stored/actual harness value
// (a schedule's pin, or a user's default_harness) so an editing surface shows the
// current choice.
export function selectionFromHarness(harness: Harness | null | undefined): HarnessSelection {
  return harness ?? "inherit";
}

// bothHarnessesUsable is the show/hide gate for every harness control (D2): a picker
// (start dialog, Run Defaults, schedule modal) appears ONLY when the caller has a
// usable credential for BOTH harnesses. A single-harness user never sees it — the
// server's D11 sole-usable-harness rule already does the right thing implicitly.
export function bothHarnessesUsable(claudeUsable: boolean, codexUsable: boolean): boolean {
  return claudeUsable && codexUsable;
}

// effectiveHarnessIsCodex is the show/hide gate for the Anthropic TOKEN controls (PRD
// #1429 M4a review fix): gating on the raw picker `selection` alone is a UX trap. A
// Codex-only user never sees the harness picker at all (bothHarnessesUsable is false),
// so `selection` stays "inherit" even though the run WILL resolve to Codex under D11 —
// and an Anthropic token/credential-switch control left visible for that user 422s the
// moment it is used (D5: a Codex run cannot carry an Anthropic override). Gate on the
// EFFECTIVE harness instead:
//
//   - an explicit "codex" pick is authoritative regardless of usability facts;
//   - an explicit "claude" pick is likewise authoritative (never hidden);
//   - "inherit" resolves to Codex only when Codex is the SOLE usable harness (the
//     picker being hidden is exactly what makes this case reachable) — mirrors D11
//     rule 3, NOT rule 4 (both usable + no default ⇒ Claude, so inherit is Claude-ish
//     there and the token control stays visible).
//
// Used by every start-dialog Anthropic TokenPicker (IssueView, Board/IssueCard,
// ScheduleModal) and by the actual-harness checks for an in-flight run (RunView,
// PlanPanel, the "Switch token" action), which pass the run's real harness with
// selection="codex"/"claude" (never "inherit" — an actual run's harness is never
// unresolved) so this same helper decides both cases uniformly.
export function effectiveHarnessIsCodex(
  selection: HarnessSelection,
  claudeUsable: boolean,
  codexUsable: boolean,
): boolean {
  if (selection !== "inherit") return selection === "codex";
  return codexUsable && !claudeUsable;
}
