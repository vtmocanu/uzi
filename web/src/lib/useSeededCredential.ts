// useSeededCredential — the shared re-seed + touched-ref for a credential picker that
// seeds its selection from a stored read-side override and must resolve a pinned
// label→id once the Anthropic token list is known (PRD #1247 M7). Factored out of the
// ScheduleModal pattern so the run-header "Switch token" action and the plan-gate token
// picker share ONE seeding rule instead of each re-deriving it (and mis-deriving it: the
// gate seeded inherit, and the switch action seeded against [], both showing an EXISTING
// pinned token as "(unavailable)").
//
// It lives OUTSIDE credentialOverride.ts (a deliberately React-free lib leaf) because it
// uses hooks. Behaviour, mirroring ScheduleModal's credentialTouchedRef effect:
//   - seed the selection from the override at mount (resolving a pin against any injected
//     list — so an injected list needs no async re-seed);
//   - self-fetch the user's Anthropic tokens when none are injected, then RE-SEED to
//     resolve a pinned label→id now that the list is known — UNLESS the user already
//     touched the picker, in which case their live edit stands (a ref, so the async
//     re-seed never clobbers it);
//   - `onSelectionChange` marks the picker touched and records the new selection.
// `enabled` (default true) gates the self-fetch so a surface that HIDES the picker for a
// non-owner (canSteer=false) fires no listSecrets for a viewer who cannot use it.

import { useCallback, useEffect, useRef, useState } from "react";

import { api, type CredentialOverride, type SecretMeta } from "./api";
import { selectionFromOverride, type CredentialSelection } from "./credentialOverride";

export interface SeededCredential {
  // The Anthropic token list backing the picker: the injected list when one was passed,
  // else the self-fetched one (empty until the fetch lands). Pass this to TokenPicker so
  // it renders the pin options WITHOUT a second fetch.
  tokens: SecretMeta[];
  selection: CredentialSelection;
  // Wire this to TokenPicker's onChange: it marks the picker touched (freezing the async
  // re-seed) and records the selection.
  onSelectionChange: (sel: CredentialSelection) => void;
  // True once the user changed the picker — the omit-vs-send signal a gate uses to avoid
  // re-sending an unchanged, seeded selection.
  touched: boolean;
}

export function useSeededCredential(
  override: CredentialOverride | null | undefined,
  opts?: { tokens?: SecretMeta[]; enabled?: boolean },
): SeededCredential {
  const injected = opts?.tokens !== undefined;
  const enabled = opts?.enabled ?? true;
  const injectedTokens = opts?.tokens;

  const [fetched, setFetched] = useState<SecretMeta[]>([]);
  const tokens = injected ? (injectedTokens as SecretMeta[]) : fetched;

  const [selection, setSelection] = useState<CredentialSelection>(() =>
    selectionFromOverride(override, injectedTokens ?? []),
  );
  const [touched, setTouched] = useState(false);
  const touchedRef = useRef(false);

  useEffect(() => {
    // An injected list is known at mount, so the initial seed already resolved a pin;
    // only self-fetch (and re-seed) when no list was injected and the picker is enabled.
    if (injected || !enabled) return;
    let cancelled = false;
    void (async () => {
      try {
        const { secrets } = await api.listSecrets();
        if (cancelled) return;
        const anthropic = secrets.filter((s) => s.kind === "anthropic_token");
        setFetched(anthropic);
        // Re-seed to resolve a pinned label→id now the list is known — unless the user
        // already changed the picker (then their choice stands).
        if (!touchedRef.current) setSelection(selectionFromOverride(override, anthropic));
      } catch {
        // keep the pre-load seed; the picker still renders its four base states
      }
    })();
    return () => {
      cancelled = true;
    };
    // `override`/`opts` are stable for the surface's lifetime; run once on mount, exactly
    // as ScheduleModal's credential seed effect does.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const onSelectionChange = useCallback((sel: CredentialSelection) => {
    touchedRef.current = true;
    setTouched(true);
    setSelection(sel);
  }, []);

  return { tokens, selection, onSelectionChange, touched };
}
