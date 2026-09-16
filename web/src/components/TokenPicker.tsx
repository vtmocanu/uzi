// TokenPicker — the shared, controlled Anthropic-credential picker (PRD #1247 M7).
//
// It exposes the FOUR explicit override states — inherit, default, auto, and pin (a
// specific token) — as a labeled <select>, and is reused at every write surface: the
// start-run flow (IssueView/Board), the plan gate (PlanPanel), the run header's
// "Switch token" action (RunView), and the schedule modal.
//
// Unlike the Workers-page rebind select (WorkersSettings), this picker ALWAYS renders
// all four states, even for a user with ZERO or ONE token — the point of the feature
// is choosing at run time, so hiding it when the pin list is short would defeat it.
// The Workers select is deliberately left untouched; this mirrors its mode/sentinel
// idea without sharing code.
//
// A token label is USER-AUTHORED text rendered through sanitizeLabel (React escaping
// does not neutralise a bidi override), the same rule RunCredential follows.

import { useEffect, useState } from "react";

import { api, type SecretMeta } from "../lib/api";
import type { CredentialSelection } from "../lib/credentialOverride";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { Select } from "./ui";

// The three account-level modes are encoded as printable sentinels a token id can never
// collide with: a token id is a UUID (hex + dashes only), which can never contain a
// colon. A pinned option's value is the token's own id. PINNED_UNAVAILABLE renders a
// pinned value whose token is missing from the fetched list (a deleted/unresolved seed)
// so the select shows it rather than silently snapping to the first option.
const INHERIT_VALUE = "mode:inherit";
const DEFAULT_VALUE = "mode:default";
const AUTO_VALUE = "mode:auto";
const PINNED_UNAVAILABLE = "mode:pinned-unavailable";

function selectionToValue(sel: CredentialSelection, tokens: SecretMeta[]): string {
  switch (sel.mode) {
    case "inherit":
      return INHERIT_VALUE;
    case "default":
      return DEFAULT_VALUE;
    case "auto":
      return AUTO_VALUE;
    case "pinned":
      return sel.secret_id && tokens.some((t) => t.id === sel.secret_id)
        ? sel.secret_id
        : PINNED_UNAVAILABLE;
  }
}

function valueToSelection(value: string): CredentialSelection {
  switch (value) {
    case INHERIT_VALUE:
      return { mode: "inherit" };
    case DEFAULT_VALUE:
      return { mode: "default" };
    case AUTO_VALUE:
      return { mode: "auto" };
    case PINNED_UNAVAILABLE:
      // Re-selecting the unavailable placeholder is a no-op: there is no valid id to
      // pin to. The caller keeps the prior selection (handled below), so this branch
      // only exists for exhaustiveness.
      return { mode: "pinned" };
    default:
      return { mode: "pinned", secret_id: value };
  }
}

export function TokenPicker({
  value,
  onChange,
  tokens,
  label = "Anthropic token",
  id,
  disabled = false,
  className = "",
}: {
  value: CredentialSelection;
  onChange: (next: CredentialSelection) => void;
  // Injected token list. When omitted the picker fetches its own (best-effort — a
  // failed fetch leaves the pin list empty, and the four states still render). Callers
  // that already hold the list (Board, ScheduleModal) pass it to avoid a second fetch.
  tokens?: SecretMeta[];
  // Accessible name for the <select>. A visible <label>/<Field> may wrap it instead;
  // this is the fallback so the control is never unlabeled.
  label?: string;
  id?: string;
  disabled?: boolean;
  className?: string;
}) {
  const [fetched, setFetched] = useState<SecretMeta[]>([]);
  const injected = tokens !== undefined;

  useEffect(() => {
    if (injected) return;
    let cancelled = false;
    // A synchronous throw here (e.g. a test whose `api` mock omits listSecrets) lands
    // in the catch, so the picker still renders its four base states rather than
    // crashing the surface that embeds it.
    void (async () => {
      try {
        const { secrets } = await api.listSecrets();
        if (!cancelled) setFetched(secrets.filter((s) => s.kind === "anthropic_token"));
      } catch {
        // keep the empty pin list
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [injected]);

  // Only Anthropic tokens are pinnable — a Codex credential is never spent by a run's
  // Anthropic selection (matching the Workers rebind's kind filter).
  const list = (injected ? tokens : fetched).filter((t) => t.kind === "anthropic_token");
  const current = selectionToValue(value, list);
  // Render the unavailable placeholder only when the selection actually needs it.
  const showUnavailable = current === PINNED_UNAVAILABLE;

  return (
    <Select
      id={id}
      aria-label={label}
      disabled={disabled}
      className={className}
      value={current}
      onChange={(e) => {
        const v = e.target.value;
        if (v === PINNED_UNAVAILABLE) return; // no valid id — keep the prior selection
        onChange(valueToSelection(v));
      }}
    >
      <option value={INHERIT_VALUE}>Inherit the worker's token</option>
      <option value={DEFAULT_VALUE}>Use my default token</option>
      <option value={AUTO_VALUE}>Auto-select from my pool</option>
      <optgroup label="Pin to a token">
        {list.map((t) => {
          // `?? ""` guards a token whose label is somehow absent (a minimal fixture, or a
          // rollout-skew DTO) — a missing label must never crash the picker.
          const safe = sanitizeLabel(t.label ?? "");
          // The visible text IS the option's label (already sanitized), so no aria-label —
          // one here would suppress the "(your default)" suffix from the accessible name.
          // `title` carries the sanitized label for a hover tooltip on a truncated option.
          return (
            <option key={t.id} value={t.id} title={safe}>
              {safe}
              {t.is_default ? " (your default)" : ""}
            </option>
          );
        })}
      </optgroup>
      {showUnavailable && (
        <option value={PINNED_UNAVAILABLE} disabled title={sanitizeLabel(value.label ?? "")}>
          {sanitizeLabel(value.label ?? "pinned token")} (unavailable)
        </option>
      )}
    </Select>
  );
}
