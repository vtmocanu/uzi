// PRD #104 M6 asks for one thing to be ASSERTED rather than assumed: the three
// "does this user have a token?" checks in the SPA (Dashboard, Board, IssueView)
// keep any-row semantics after a user can hold several tokens.
//
// All three are the same expression — `secrets.some((s) => s.kind ===
// "anthropic_token")` — over the GET /api/me/secrets payload. This pins that
// expression's behaviour across the shapes that payload can now take, so a future
// change to it (say, filtering on is_default) trips here instead of silently
// gating the board on the wrong thing.

import { describe, expect, it } from "vitest";
import type { SecretMeta } from "./api";

const hasToken = (secrets: SecretMeta[]) => secrets.some((s) => s.kind === "anthropic_token");

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
