// PRD #1171 (M3, item 6 "Dark selection and secret registration") — the pure,
// fail-closed discriminator that turns a claim's optional Codex secret block into a
// run-time selection DECISION.
//
// The load-bearing rule (PRD §6, §Success 7): presence of a COMPLETE server-owned
// Codex binding selects Codex; a present-but-BROKEN binding fails closed BEFORE any
// model work and NEVER silently falls back to Claude; only true ABSENCE of the block
// takes the exact Claude path. This module is that decision and nothing else — it is
// pure (no I/O, no logging), and it never echoes the access token or capability into
// an error, since both are secrets.
//
// This pass delivers the TYPE + the DECISION only. Wiring the decision into
// makeExecutor / runner.ts / main.ts is a separate later unit.

// --- Branded validated binding ------------------------------------------------
// A module-private brand: the ONLY way to obtain a `CodexBinding` is through
// {@link selectCodexBinding}'s validator, so downstream credential-bridge code can
// never manufacture one from an unvalidated object literal. Branding is compile-time
// only; at runtime a binding is a plain object.
declare const codexBindingBrand: unique symbol;

/**
 * A validated Codex binding. Carries only the fields the credential bridge (M4)
 * needs, in the worker's own naming:
 *
 *   * `authMode` — the discriminator; "subscription" is the only mode that may
 *     refresh, "api_key" can never refresh.
 *   * `accessToken` / `capability` — non-empty, server-owned; SECRETS (never logged).
 *   * `generation` — present IFF subscription (the worker's initial observedGeneration
 *     for coordinated refresh); ABSENT for api_key.
 *
 * Obtainable only via {@link selectCodexBinding}; the brand blocks object-literal
 * forgery.
 */
export interface CodexBinding {
  readonly [codexBindingBrand]: true;
  readonly authMode: "subscription" | "api_key";
  readonly accessToken: string;
  readonly capability: string;
  readonly generation?: number;
}

/** The dark selection decision. `claude` is the exact legacy Claude path (absence);
 *  `codex` carries a validated binding (a complete server-owned Codex block). */
export type CodexSelection = { kind: "claude" } | { kind: "codex"; binding: CodexBinding };

// --- Bounded, secret-free failure ---------------------------------------------

/** The closed set of reasons a present Codex block is rejected. Programmatic and
 *  safe to surface; none of these ever carries a token/capability value. */
export type CodexSelectionErrorReason =
  | "not_an_object"
  | "invalid_auth_mode"
  | "invalid_access_token"
  | "invalid_capability"
  | "subscription_missing_generation"
  | "subscription_generation_not_finite"
  | "api_key_unexpected_generation";

/** Static, bounded messages. Deliberately NO interpolation of the offending value:
 *  the access token and capability are secrets, and even the auth_mode is untrusted
 *  attacker-shaped data, so nothing from the block is echoed back. */
const REASON_MESSAGES: Record<CodexSelectionErrorReason, string> = {
  not_an_object: "Codex claim block is present but is not an object",
  invalid_auth_mode: "Codex claim block has a missing or unrecognized auth_mode",
  invalid_access_token: "Codex claim block is missing a valid access_token",
  invalid_capability: "Codex claim block is missing a valid capability",
  subscription_missing_generation: "Codex subscription claim is missing its generation",
  subscription_generation_not_finite: "Codex subscription claim generation is not a finite number",
  api_key_unexpected_generation: "Codex api_key claim must not carry a generation",
};

/**
 * Thrown when a Codex block is PRESENT but not a complete, valid binding. This is the
 * fail-closed path: it fires before any model work and is never swallowed into a
 * Claude fallback. The message is a bounded static string; the machine-readable
 * {@link CodexSelectionErrorReason} is on `reason`. Neither carries the token or
 * capability value.
 */
export class CodexSelectionError extends Error {
  readonly reason: CodexSelectionErrorReason;
  constructor(reason: CodexSelectionErrorReason) {
    super(REASON_MESSAGES[reason]);
    this.name = "CodexSelectionError";
    this.reason = reason;
  }
}

// --- The decision -------------------------------------------------------------

/** The minimal claim shape this discriminator reads. The codex block is typed
 *  `unknown` on purpose: it arrives from untrusted JSON, so it is validated at
 *  run time rather than trusted from a compile-time type. A real
 *  {@link import("../protocol.js").ClaimSecrets} is assignable to this. */
export interface CodexClaimView {
  readonly codex?: unknown;
}

/**
 * The dark selection decision.
 *
 *   * codex block ABSENT (key missing / `undefined`) → `{ kind: "claude" }`. Absence
 *     is normal and is NEVER an error — this is the exact legacy Claude path.
 *   * codex block PRESENT and a complete, valid binding → `{ kind: "codex", binding }`.
 *   * codex block PRESENT but malformed/incomplete → THROW {@link CodexSelectionError}
 *     (fail closed; never a silent Claude fallback).
 */
export function selectCodexBinding(claim: CodexClaimView): CodexSelection {
  const raw = claim.codex;
  if (raw === undefined) {
    // True absence: the ordinary Claude claim. No error, ever.
    return { kind: "claude" };
  }
  // Present ⇒ it MUST validate to a complete binding, or we fail closed. Anything
  // that is not a complete server-owned binding (including a stray `null`, a
  // non-object, or an object missing/mistyping a field) throws here rather than
  // degrading to Claude.
  return { kind: "codex", binding: validateCodexBinding(raw) };
}

/** Validate an untrusted, present codex block into a branded {@link CodexBinding} or
 *  throw {@link CodexSelectionError}. The only mint point for the brand. */
function validateCodexBinding(raw: unknown): CodexBinding {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
    throw new CodexSelectionError("not_an_object");
  }
  const record = raw as Record<string, unknown>;

  const authMode = record.auth_mode;
  if (authMode !== "subscription" && authMode !== "api_key") {
    throw new CodexSelectionError("invalid_auth_mode");
  }

  const accessToken = record.access_token;
  if (typeof accessToken !== "string" || accessToken.length === 0) {
    throw new CodexSelectionError("invalid_access_token");
  }

  const capability = record.capability;
  if (typeof capability !== "string" || capability.length === 0) {
    throw new CodexSelectionError("invalid_capability");
  }

  if (authMode === "subscription") {
    const generation = record.generation;
    if (typeof generation !== "number") {
      throw new CodexSelectionError("subscription_missing_generation");
    }
    if (!Number.isFinite(generation)) {
      throw new CodexSelectionError("subscription_generation_not_finite");
    }
    return { authMode, accessToken, capability, generation } as CodexBinding;
  }

  // api_key: carries NO refresh generation. A present one is malformed (it can never
  // refresh), so we reject rather than silently ignore it.
  if (record.generation !== undefined) {
    throw new CodexSelectionError("api_key_unexpected_generation");
  }
  return { authMode, accessToken, capability } as CodexBinding;
}

// --- Structural accessors (for the M4 credential bridge) ----------------------

/** The validated binding's auth mode. */
export function mode(binding: CodexBinding): "subscription" | "api_key" {
  return binding.authMode;
}

/** True only for a subscription binding. The credential bridge (M4) uses this to
 *  enforce structurally that an api_key run never invokes refresh. */
export function canRefresh(binding: CodexBinding): boolean {
  return binding.authMode === "subscription";
}
