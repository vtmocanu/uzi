// Per-run / per-schedule Anthropic credential OVERRIDE model (PRD #1247 M7, the
// write side). Distinct from lib/runCredential (which describes the credential a
// claim already SPENT): this is the choice a human makes BEFORE or DURING a run —
// inherit the worker binding, spend the default token, auto-select from the pool,
// or pin one specific token.
//
// Kept in a lib leaf (no React) so the picker component, the two run-create sites,
// the run view, and the schedule modal all share one vocabulary and one set of
// body-shaping rules — and so a unit test can pin those rules without a DOM.

import type { CredentialOverride, Run, SecretMeta } from "./api";

// The four explicit states the picker exposes. `inherit` is the true default: the
// run/schedule follows the worker binding (today's behaviour), and NOTHING is sent.
// `default`/`auto` name an account-level choice; `pinned` names one token by id.
export type CredentialMode = "inherit" | "default" | "auto" | "pinned";

export interface CredentialSelection {
  mode: CredentialMode;
  // Set ONLY for `pinned` — the chosen token's id. Absent for the other three.
  secret_id?: string;
  // Display-only hint for a `pinned` value whose token is not in the fetched list
  // (e.g. a schedule seeded from a stored override whose token was later deleted).
  // NEVER sent on the wire — the server keys pinned on secret_id.
  label?: string;
}

export const INHERIT_SELECTION: CredentialSelection = { mode: "inherit" };

// runCredentialBody shapes the body for a RUN create / set-token call: `inherit`
// means "send nothing, inherit the worker binding", so it returns undefined and the
// caller OMITS the field. Every other mode is sent as {mode, secret_id?}. secret_id
// rides only a pinned choice (the server 400s a pinned mode with no secret_id, and
// 400s a secret_id on a non-pinned mode).
export function runCredentialBody(
  sel: CredentialSelection,
): { mode: string; secret_id?: string } | undefined {
  if (sel.mode === "inherit") return undefined;
  if (sel.mode === "pinned") return { mode: "pinned", secret_id: sel.secret_id };
  return { mode: sel.mode };
}

// setTokenBody shapes the body for the SWITCH action (POST /runs/{id}/credential),
// which ALWAYS sends a body — unlike a run create, an explicit `inherit` here is a
// meaningful action (revert to the worker binding), so it is sent as {mode:"inherit"}
// rather than omitted. secret_id rides only a pinned choice.
export function setTokenBody(sel: CredentialSelection): { mode: string; secret_id?: string } {
  if (sel.mode === "pinned") return { mode: "pinned", secret_id: sel.secret_id };
  return { mode: sel.mode };
}

// scheduleCredentialPatch shapes the body for a SCHEDULE create/patch, where the
// rules differ from a run (PRD #1247 M6, presence-aware seed-and-keep):
//   - the user never TOUCHED the picker → undefined → OMIT the field, so the
//     server's seed-and-keep preserves the stored override on an unrelated edit;
//   - the user set it to inherit → {mode:"inherit"} — an EXPLICIT clear that reaches
//     the validator (NOT null, NOT omitted), which nulls the stored override;
//   - any other picked value → {mode, secret_id?}.
export function scheduleCredentialPatch(
  sel: CredentialSelection,
  touched: boolean,
): { mode: string; secret_id?: string } | undefined {
  if (!touched) return undefined;
  if (sel.mode === "inherit") return { mode: "inherit" };
  if (sel.mode === "pinned") return { mode: "pinned", secret_id: sel.secret_id };
  return { mode: sel.mode };
}

// selectionFromOverride derives a picker selection from a stored READ-side override
// ({mode, label}) so an editing surface shows the current choice. A pinned override
// carries only a LABEL (the snapshot taken at write time), so it is resolved back to
// an id by matching the loaded token list — best-effort: an unmatched label (deleted
// token, or a list not yet loaded) keeps the label as a display hint and leaves
// secret_id undefined, so the picker renders it as unavailable rather than silently
// dropping it or fabricating an id.
export function selectionFromOverride(
  override: CredentialOverride | null | undefined,
  tokens: SecretMeta[],
): CredentialSelection {
  if (!override) return { mode: "inherit" };
  switch (override.mode) {
    case "pinned": {
      const match = override.label
        ? tokens.find((t) => t.label === override.label)
        : undefined;
      return { mode: "pinned", secret_id: match?.id, label: override.label ?? undefined };
    }
    case "auto":
      return { mode: "auto" };
    case "default":
      return { mode: "default" };
    default:
      // An unknown stored mode reads as inherit rather than fabricating a state the
      // picker cannot round-trip — the same fail-safe the read-side renderer uses.
      return { mode: "inherit" };
  }
}

// isCredentialSwitchRefusedLane names the run lanes the server REFUSES a credential
// switch on (PRD #1247 M7, D6/D12) — a task_review run (trigger_source), and the
// chat / judge / self_improve kinds. These 409 server-side, so the UI must never
// render a switch control that would fail: it hides the action entirely for them.
export function isCredentialSwitchRefusedLane(
  run: Pick<Run, "kind" | "trigger_source">,
): boolean {
  if (run.trigger_source === "task_review") return true;
  return run.kind === "chat" || run.kind === "judge" || run.kind === "self_improve";
}
