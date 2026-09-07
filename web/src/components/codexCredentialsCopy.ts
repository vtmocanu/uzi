// Centralized "dark" copy for the Codex credentials card (issue #1174 item 6).
// While parent #1106 is dark, saved Codex credentials do NOT start runs and a
// `staging` login is NOT auto-verified in this build. The milestone that ENABLES
// Codex routing removes/updates these in ONE place (and updates the pinning test
// codexCredentialsCopy.test.ts). Do not inline these sentences elsewhere — import
// from here so retirement stays a one-line change.
export const CODEX_DARK_COPY = {
  notUsedForRuns:
    "Codex is not yet used to run agents — an Anthropic token is still what starts a run.",
  stagingNotAutoVerified:
    "This build does not verify Codex logins automatically.",
} as const;
