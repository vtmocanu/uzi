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

// effectiveHarnessIsCodex is the show/hide gate for the Anthropic TOKEN controls, on a
// run/schedule that does NOT exist yet (PRD #1429 M4a review Fix 1/Fix 2): gating on the
// raw picker `selection` alone is a UX trap. A Codex-only user never sees the harness
// picker at all (bothHarnessesUsable is false), so `selection` stays "inherit" even
// though the run WILL resolve to Codex under D11 — and an Anthropic token/credential
// override control left visible for that user 422s the moment it is used (D5: a Codex
// run cannot carry an Anthropic override). Gate on the EFFECTIVE harness instead, which
// mirrors resolveHarness's (harness_resolver.go) explicit → usable-default → availability
// order exactly:
//
//   - an explicit "codex" pick is authoritative regardless of usability facts;
//   - an explicit "claude" pick is likewise authoritative (never hidden);
//   - "inherit" first checks the caller's usable default_harness (D11 rule 2): a
//     usable Codex default resolves to Codex even when Claude is ALSO usable (the
//     picker showing does not mean inherit resolves to Claude); a usable Claude
//     default resolves to Claude even when Codex is also usable;
//   - otherwise "inherit" falls through to availability — Codex only when it is the
//     SOLE usable harness (D11 rule 3); both-usable-with-no-usable-default and
//     sole-Claude both resolve to Claude (D11 rules 3/4).
//
// Used by every NOT-YET-CREATED start-dialog Anthropic TokenPicker gate (IssueView's
// start dialog, Board/IssueCard's per-card picker, ScheduleModal's per-schedule picker),
// each passing the caller's own usable default_harness (a user's for the two start
// sites, a schedule's owner's for ScheduleModal) alongside the picker's live selection.
// The EXISTING-run sites (RunView, PlanPanel, the "Switch token" action) do NOT call
// this helper — a run's harness is already frozen at creation, so they gate on the
// run's own actual `run.harness === "codex"` directly, with no resolver to mirror.
export function effectiveHarnessIsCodex(
  selection: HarnessSelection,
  claudeUsable: boolean,
  codexUsable: boolean,
  defaultHarness: Harness | null = null,
): boolean {
  if (selection !== "inherit") return selection === "codex";
  // Mirrors resolveHarness's rung 2: a USABLE user/schedule-owner default wins over
  // plain availability, in either direction — this is what a both-usable user's Codex
  // default (M4a's Run Defaults card) makes reachable on an untouched "inherit" pick.
  if (defaultHarness === "codex" && codexUsable) return true;
  if (defaultHarness === "claude" && claudeUsable) return false;
  // Rungs 3-4: no usable default (or none set) falls through to availability alone.
  return codexUsable && !claudeUsable;
}
