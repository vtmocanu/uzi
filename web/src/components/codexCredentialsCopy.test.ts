// Pins the "dark" copy for the Codex credentials card (issue #1174 item 6). These
// exact-equality assertions are the tripwire for the future milestone that ENABLES
// Codex routing: flipping the flag in codexCredentialsCopy.ts must be a deliberate
// edit that also updates this test, never an accidental drift. The card component
// imports the same const, so a single source of truth backs both the UI and the
// guard.

import { describe, it, expect } from "vitest";
import { CODEX_DARK_COPY } from "./codexCredentialsCopy";

describe("CODEX_DARK_COPY (the one Codex-dark retirement flag)", () => {
  it("pins the not-used-for-runs sentence", () => {
    expect(CODEX_DARK_COPY.notUsedForRuns).toBe(
      "Codex is not yet used to run agents — an Anthropic token is still what starts a run.",
    );
  });

  it("pins the staging-not-auto-verified sentence", () => {
    expect(CODEX_DARK_COPY.stagingNotAutoVerified).toBe(
      "This build does not verify Codex logins automatically.",
    );
  });
});
