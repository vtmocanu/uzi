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
import { hasAnthropicToken as hasToken } from "./hasToken";

function secret(over: Partial<SecretMeta> = {}): SecretMeta {
  return {
    id: "sec-1",
    kind: "anthropic_token",
    label: "default",
    is_default: true,
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
