import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { canRefresh, CodexSelectionError, mode, selectCodexBinding } from "../src/codex/select.js";
import type { CodexSelection, CodexSelectionErrorReason } from "../src/codex/select.js";
import type { ClaimCodexSecrets, ClaimSecrets } from "../src/protocol.js";

// PRD #1171 (M3, item 6 "Dark selection and secret registration") — the unit test for
// the pure, fail-closed dark-selection discriminator in src/codex/select.ts.
//
// The invariants this file pins:
//   * ABSENCE of the codex block takes the exact legacy Claude path (never an error).
//   * a COMPLETE, valid server-owned block selects Codex and mints a validated binding.
//   * a PRESENT-but-malformed block FAILS CLOSED — it THROWS a CodexSelectionError with
//     a programmatic `.reason`, and never silently degrades to the Claude path.
//   * the thrown message is a bounded static string that NEVER echoes the access token
//     or the capability (both are secrets), even when the offending block carries them.
//   * the CodexBinding brand is compile-time-private: only selectCodexBinding mints one.

// Recognizable secret-shaped values planted in the malformed inputs so an assertion can
// prove they are absent from the (static) error message — i.e. nothing from the block is
// ever interpolated back out.
const SECRET_TOKEN = "sk-codex-SECRET-ACCESS-TOKEN-abc123-do-not-leak";
const SECRET_CAP = "cap-SECRET-CAPABILITY-xyz789-do-not-leak";
const PRIVATE_ACCOUNT = "account-PRIVATE-IDENTITY-do-not-leak";

function validSubscription(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    auth_mode: "subscription",
    access_token: SECRET_TOKEN,
    capability: SECRET_CAP,
    generation: 7,
    chatgpt_account_id: PRIVATE_ACCOUNT,
    chatgpt_plan_type: null,
    ...overrides,
  };
}

describe("selectCodexBinding: absence takes the exact Claude path", () => {
  it("returns {kind:'claude'} when the codex block is a MISSING key", () => {
    assert.deepEqual(selectCodexBinding({}), { kind: "claude" });
  });

  it("returns {kind:'claude'} when codex is explicitly undefined", () => {
    assert.deepEqual(selectCodexBinding({ codex: undefined }), { kind: "claude" });
  });

  it("selects claude for a plain Claude ClaimSecrets with no codex field", () => {
    const claudeClaim: ClaimSecrets = { forge_pat: "pat-not-a-secret-under-test" };
    assert.deepEqual(selectCodexBinding(claudeClaim), { kind: "claude" });
  });
});

describe("selectCodexBinding: a complete, valid block selects Codex", () => {
  it("subscription (nonnegative safe-integer generation) → codex; authMode subscription; canRefresh true", () => {
    const subCodex: ClaimCodexSecrets = {
      auth_mode: "subscription",
      access_token: SECRET_TOKEN,
      capability: SECRET_CAP,
      generation: 7,
      chatgpt_account_id: PRIVATE_ACCOUNT,
      chatgpt_plan_type: null,
    };
    const claim: ClaimSecrets = { forge_pat: "pat", codex: subCodex };

    const sel = selectCodexBinding(claim);
    assert.equal(sel.kind, "codex");
    if (sel.kind !== "codex") return; // narrow for the compiler

    assert.equal(sel.binding.authMode, "subscription");
    assert.equal(sel.binding.accessToken, SECRET_TOKEN);
    assert.equal(sel.binding.capability, SECRET_CAP);
    assert.equal(sel.binding.generation, 7);
    assert.equal(sel.binding.chatgptAccountId, PRIVATE_ACCOUNT);
    assert.equal(sel.binding.chatgptPlanType, null);
    assert.equal(mode(sel.binding), "subscription");
    assert.equal(canRefresh(sel.binding), true);
  });

  it("accepts generation === 0 (a finite, falsy generation is valid)", () => {
    const sel = selectCodexBinding({
      codex: validSubscription({ generation: 0 }),
    });
    assert.equal(sel.kind, "codex");
    if (sel.kind !== "codex") return;
    assert.equal(sel.binding.generation, 0);
    assert.equal(canRefresh(sel.binding), true);
  });

  it("api_key (NO generation) → codex; authMode api_key; canRefresh false; mode api_key", () => {
    const apiCodex: ClaimCodexSecrets = {
      auth_mode: "api_key",
      access_token: SECRET_TOKEN,
      capability: SECRET_CAP,
    };
    const claim: ClaimSecrets = { forge_pat: "pat", codex: apiCodex };

    const sel = selectCodexBinding(claim);
    assert.equal(sel.kind, "codex");
    if (sel.kind !== "codex") return;

    assert.equal(sel.binding.authMode, "api_key");
    assert.equal(sel.binding.generation, undefined);
    assert.equal(mode(sel.binding), "api_key");
    assert.equal(canRefresh(sel.binding), false);
  });
});

// Every present-but-malformed block, its expected programmatic reason, and (where the
// field exists) a planted secret so the message can be proven secret-free.
const MALFORMED: { name: string; codex: unknown; reason: CodexSelectionErrorReason }[] = [
  {
    name: "unknown auth_mode",
    codex: { auth_mode: "oauth", access_token: SECRET_TOKEN, capability: SECRET_CAP, generation: 1 },
    reason: "invalid_auth_mode",
  },
  {
    name: "missing auth_mode",
    codex: { access_token: SECRET_TOKEN, capability: SECRET_CAP },
    reason: "invalid_auth_mode",
  },
  {
    name: "empty access_token",
    codex: validSubscription({ access_token: "" }),
    reason: "invalid_access_token",
  },
  {
    name: "missing access_token",
    codex: { ...validSubscription(), access_token: undefined },
    reason: "invalid_access_token",
  },
  {
    name: "empty capability",
    codex: validSubscription({ capability: "" }),
    reason: "invalid_capability",
  },
  {
    name: "subscription missing generation",
    codex: { ...validSubscription(), generation: undefined },
    reason: "subscription_missing_generation",
  },
  {
    name: "subscription generation is NaN",
    codex: validSubscription({ generation: Number.NaN }),
    reason: "subscription_generation_not_finite",
  },
  {
    name: "subscription generation is Infinity",
    codex: validSubscription({ generation: Number.POSITIVE_INFINITY }),
    reason: "subscription_generation_not_finite",
  },
  {
    name: "subscription generation is fractional",
    codex: validSubscription({ generation: 1.5 }),
    reason: "subscription_generation_not_safe_integer",
  },
  {
    name: "subscription generation exceeds the safe-integer range",
    codex: validSubscription({ generation: Number.MAX_SAFE_INTEGER + 1 }),
    reason: "subscription_generation_not_safe_integer",
  },
  {
    name: "subscription generation is negative",
    codex: validSubscription({ generation: -1 }),
    reason: "subscription_generation_negative",
  },
  {
    name: "subscription account id is missing",
    codex: { ...validSubscription(), chatgpt_account_id: undefined },
    reason: "subscription_invalid_account_id",
  },
  {
    name: "subscription account id is empty",
    codex: validSubscription({ chatgpt_account_id: "" }),
    reason: "subscription_invalid_account_id",
  },
  {
    name: "subscription plan type is missing",
    codex: { ...validSubscription(), chatgpt_plan_type: undefined },
    reason: "subscription_invalid_plan_type",
  },
  {
    name: "subscription plan type is provider text instead of null",
    codex: validSubscription({ chatgpt_plan_type: "plus" }),
    reason: "subscription_invalid_plan_type",
  },
  {
    name: "api_key WITH a generation",
    codex: { auth_mode: "api_key", access_token: SECRET_TOKEN, capability: SECRET_CAP, generation: 5 },
    reason: "api_key_unexpected_subscription_fields",
  },
  {
    name: "api_key WITH an account id",
    codex: { auth_mode: "api_key", access_token: SECRET_TOKEN, capability: SECRET_CAP, chatgpt_account_id: PRIVATE_ACCOUNT },
    reason: "api_key_unexpected_subscription_fields",
  },
  {
    name: "api_key WITH an explicit plan null",
    codex: { auth_mode: "api_key", access_token: SECRET_TOKEN, capability: SECRET_CAP, chatgpt_plan_type: null },
    reason: "api_key_unexpected_subscription_fields",
  },
  { name: "codex block is null", codex: null, reason: "not_an_object" },
  { name: "codex block is a string (secret-shaped)", codex: SECRET_TOKEN, reason: "not_an_object" },
  { name: "codex block is a number", codex: 42, reason: "not_an_object" },
  { name: "codex block is an array", codex: [SECRET_TOKEN, SECRET_CAP], reason: "not_an_object" },
];

describe("selectCodexBinding: a present-but-malformed block fails closed (throws, secret-free)", () => {
  for (const m of MALFORMED) {
    it(`${m.name} → throws CodexSelectionError(${m.reason}) with a secret-free message`, () => {
      assert.throws(
        () => selectCodexBinding({ codex: m.codex }),
        (e: unknown) => {
          assert.ok(e instanceof CodexSelectionError, `expected CodexSelectionError, got ${String(e)}`);
          assert.equal(e.reason, m.reason);
          // The message is a bounded, non-empty static string...
          assert.ok(e.message.length > 0);
          // ...that never echoes the access token or the capability, even when the
          // offending block carried them.
          assert.ok(!e.message.includes(SECRET_TOKEN), `message leaked the access token: ${e.message}`);
          assert.ok(!e.message.includes(SECRET_CAP), `message leaked the capability: ${e.message}`);
          assert.ok(!e.message.includes(PRIVATE_ACCOUNT), `message leaked the account id: ${e.message}`);
          return true;
        },
      );
    });
  }
});

describe("selectCodexBinding: a present-but-broken block NEVER falls back to claude", () => {
  it("throws for every broken block rather than returning {kind:'claude'}", () => {
    for (const m of MALFORMED) {
      // Sentinel-based, so a stray {kind:'claude'} (or any other return) is caught as a
      // failure instead of an assert.fail being swallowed by the catch.
      let outcome: CodexSelection | "threw";
      try {
        outcome = selectCodexBinding({ codex: m.codex });
      } catch (e) {
        assert.ok(e instanceof CodexSelectionError, `${m.name}: threw a non-CodexSelectionError: ${String(e)}`);
        outcome = "threw";
      }
      assert.equal(outcome, "threw", `${m.name}: a broken block must fail closed, not fall back to claude`);
    }
  });
});

describe("CodexBinding brand: only selectCodexBinding may mint a binding", () => {
  it("rejects a hand-built object literal where a CodexBinding is required (compile-time-private brand)", () => {
    // Shaped exactly like a subscription binding, but WITHOUT the module-private brand
    // symbol — so it is structurally rejected at compile time by mode()/canRefresh().
    const forged = {
      authMode: "subscription",
      accessToken: SECRET_TOKEN,
      capability: SECRET_CAP,
      generation: 1,
      chatgptAccountId: PRIVATE_ACCOUNT,
      chatgptPlanType: null,
    };

    // @ts-expect-error - the CodexBinding brand is a module-private unique symbol: only
    // selectCodexBinding's validator mints one, so a plain literal is not assignable here.
    assert.equal(mode(forged), "subscription");
    // @ts-expect-error - same brand barrier guards canRefresh against a forged binding.
    assert.equal(canRefresh(forged), true);
  });
});
