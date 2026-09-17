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
