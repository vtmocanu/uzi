// PRD #104 M6 asks for one thing to be ASSERTED rather than assumed: the three
// "does this user have a token?" checks in the SPA (Dashboard, Board, IssueView)
// keep any-row semantics after a user can hold several tokens.
//
// This file used to declare its OWN copy of the predicate while the real
// expression stayed inlined at all three call sites — so it asserted a copy, and
// narrowing any of those three to `is_default` would have left it green while the
// board silently gated on the wrong thing. Exactly the shape of "a test that
// passes both before and after is not testing the bug". The predicate now lives in
// `lib/hasToken.ts` and is imported by the three pages AND by this file, so the
// line below holds the shipping code.
//
// Verified by removal (2026-07-21): changing `hasAnthropicToken` to
// `s.kind === "anthropic_token" && s.is_default` turns the "NO token is flagged
// default" case red. Against the old local copy the same edit changed nothing.

import { describe, expect, it } from "vitest";
import type { SecretMeta } from "./api";
import {
  anthropicTokenCount,
  hasAnthropicToken as hasToken,
  hasAnyCodexCredential,
  hasUsableCredential,
  isCodexUsable,
} from "./hasToken";

function secret(over: Partial<SecretMeta> = {}): SecretMeta {
  return {
    id: "sec-1",
    kind: "anthropic_token",
    label: "default",
    is_default: true,
    // PRD #111 M2: the auto-selection pool opt-in, false unless a test says otherwise.
    auto_eligible: false,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

describe("hasToken (Dashboard / Board / IssueView gates)", () => {
  it("is false for a token-less user", () => {
    expect(hasToken([])).toBe(false);
  });

  it("is true for the single-token user (unchanged by PRD #104)", () => {
    expect(hasToken([secret()])).toBe(true);
  });

  it("is true for a multi-token user", () => {
    expect(hasToken([secret(), secret({ id: "sec-2", label: "console", is_default: false })])).toBe(true);
  });

  // The load-bearing case: a user whose tokens are all NON-default still has a
  // token. If any of these gates ever narrowed to the default, this is the state
  // that would wrongly read as "no token" — and under D6 it is unreachable today,
  // which is exactly why a test has to hold the line rather than the runtime.
  it("is true when NO token is flagged default", () => {
    expect(
      hasToken([
        secret({ id: "sec-a", label: "a", is_default: false }),
        secret({ id: "sec-b", label: "b", is_default: false }),
      ]),
    ).toBe(true);
  });

  it("ignores secrets of another kind", () => {
    expect(hasToken([secret({ kind: "openai_token" })])).toBe(false);
  });
});

// PRD #295: the Runs-list credential badge gates on ">1 Anthropic token", so the
// count is the shared, tested predicate rather than an inlined `.length > 1` at the
// call site. The `> 1` comparison lives in RunsList; this asserts the count itself.
describe("anthropicTokenCount (Runs-list credential-badge gate)", () => {
  it("is 0 for a token-less user", () => {
    expect(anthropicTokenCount([])).toBe(0);
  });

  it("is 1 for the single-token user (badge stays hidden — nothing to say)", () => {
    expect(anthropicTokenCount([secret()])).toBe(1);
  });

  it("counts every anthropic token for a multi-token user", () => {
    expect(
      anthropicTokenCount([
        secret(),
        secret({ id: "sec-2", label: "console", is_default: false }),
      ]),
    ).toBe(2);
  });

  // The gate must count only anthropic tokens: a user with one anthropic token and
  // some other secret kind is still a single-token user and must see no badge.
  it("ignores secrets of another kind", () => {
    expect(
      anthropicTokenCount([
        secret(),
        secret({ id: "sec-oai", kind: "openai_token" }),
      ]),
    ).toBe(1);
  });
});

// PRD #1429 M4a, D3: Codex usability mirrors the server's D11 rule EXACTLY — a
// subscription default is usable only when linked, an api_key default is usable by
// existence, and a failed/missing subscription default never falls through to a
// non-default api key.
describe("isCodexUsable (D3 Codex availability)", () => {
  it("is false with no Codex secrets at all", () => {
    expect(isCodexUsable([])).toBe(false);
  });

  it("is true for a LINKED subscription default", () => {
    expect(
      isCodexUsable([secret({ kind: "codex_auth", is_default: true, codex_status: "linked" })]),
    ).toBe(true);
  });

  it("is false for a subscription default that is NOT linked (staging/failed)", () => {
    expect(
      isCodexUsable([secret({ kind: "codex_auth", is_default: true, codex_status: "staging" })]),
    ).toBe(false);
  });

  // The load-bearing D3 case: an unusable subscription default must NOT fall through
  // to a non-default api key sitting behind it.
  it("does not fall through to a non-default api key behind an unusable subscription default", () => {
    expect(
      isCodexUsable([
        secret({ kind: "codex_auth", is_default: true, codex_status: "failed" }),
        secret({ id: "sec-key", kind: "openai_api_key", is_default: false }),
      ]),
    ).toBe(false);
  });

  it("is true for an api_key default (usable by existence)", () => {
    expect(isCodexUsable([secret({ kind: "openai_api_key", is_default: true })])).toBe(true);
  });

  it("is false for a non-default api_key with no default of either kind", () => {
    expect(isCodexUsable([secret({ kind: "openai_api_key", is_default: false })])).toBe(false);
  });

  it("ignores an anthropic token", () => {
    expect(isCodexUsable([secret({ kind: "anthropic_token", is_default: true })])).toBe(false);
  });
});

describe("hasAnyCodexCredential (harness-appropriate copy)", () => {
  it("is false with no Codex secrets", () => {
    expect(hasAnyCodexCredential([])).toBe(false);
  });
  it("is true for ANY Codex-kind secret, usable or not", () => {
    expect(
      hasAnyCodexCredential([secret({ kind: "codex_auth", is_default: false, codex_status: "staging" })]),
    ).toBe(true);
    expect(hasAnyCodexCredential([secret({ kind: "openai_api_key", is_default: false })])).toBe(true);
  });
  it("ignores an anthropic token", () => {
    expect(hasAnyCodexCredential([secret({ kind: "anthropic_token" })])).toBe(false);
  });
});

describe("hasUsableCredential (Dashboard onboarding step)", () => {
  it("is false with no usable credential of either harness", () => {
    expect(hasUsableCredential([])).toBe(false);
  });
  it("is true for a Claude-only user (today's behaviour, unchanged)", () => {
    expect(hasUsableCredential([secret()])).toBe(true);
  });
  // The Codex-only case this milestone adds: a user with zero Anthropic tokens but a
  // usable Codex credential has already completed the onboarding step.
  it("is true for a Codex-only user", () => {
    expect(hasUsableCredential([secret({ kind: "openai_api_key", is_default: true })])).toBe(true);
  });
});
