import type { AutoStatus, SecretMeta } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { mockMyTokenRateLimits, mockSecrets } from "../data";
import { state } from "../store";
import { delay, users } from "./shared";
// secrets ↔ workers is the one accepted import cycle (PRD #991 D4): deleteAnthropicTokenById
// unbinds workers pinned to the deleted token, and workers' setWorkerBindMode resolves a
// token label against this roster. Both reads are inside function bodies and no module-level
// initializer crosses the modules, so the cycle cannot throw at import.
import { workers } from "./workers";

export let secrets: SecretMeta[] = mockSecrets.map((s) => ({ ...s }));

// requireUnlockedVault mirrors the real API: sealing a token needs the vault
// unlocked (PRD #32), so every create/rotate path throws the same 409 the SPA
// turns into an unlock prompt.
function requireUnlockedVault(): void {
  if (!state.vaultUnlocked) {
    throw new ApiError(409, "vault is locked; unlock it with your password, then save again", {
      code: "vault_locked",
    });
  }
}

// rejectInvisibleLabel mirrors the server's validateSecretLabel Cf rule (PRD #111).
//
// The mock is this repo's BROWSABLE SPEC, and until this existed it accepted labels
// production rejects — which is how a browser pass managed to store a bidi-override
// label and demonstrate F12 against a build that was supposed to make it impossible.
// A mock that disagrees with the API about what is valid teaches the wrong lesson and
// leaves the new error copy with nowhere to be seen.
//
// Control characters are not re-checked here: the real validator rejects them too,
// but they cannot be typed into the field, so the Cf half is the one a demo exercises.
function rejectInvisibleLabel(label: string): void {
  if (/\p{Cf}/u.test(label)) {
    throw new ApiError(
      400,
      "Label must not contain invisible formatting characters (zero-width spaces and joiners, bidirectional overrides, the byte-order mark): they let two different tokens look identical, or make a label read as a different account. This also rules out multi-part emoji such as 👨‍👩‍👧, which are joined by one of these characters, so use a plain name instead",
    );
  }
}

// pooledFixtureStatus is each demo token's eligibility WHEN POOLED, so toggling one
// off and on again returns it to the state its fixture describes instead of
// flattening every token to `eligible`.
const pooledFixtureStatus: Record<string, AutoStatus> = {
  "sec-never-polled": "no_reading",
  "sec-low": "below_threshold",
};

// unbindAnthropicSecret mirrors the schema cascade (migrations 00078/00079 hang composite
// FKs off user_secrets (user_id, id) with ON DELETE SET NULL): deleting a bound anthropic
// token unbinds its workers and the judge rather than orphaning them. Every delete path
// (both the by-id path and the D14 kind-path alias) must run this or the mock leaves a
// worker reading a token that no longer exists. Kept module-local (not exported) so knip
// does not flag it as an unused export.
function unbindAnthropicSecret(id: string): void {
  workers.forEach((w) => {
    if (w.anthropic_secret_id === id) {
      w.anthropic_secret_id = null;
      w.anthropic_secret_label = null;
    }
  });
  // `state.session` is a COPY, not a reference into `users`, so both have to be
  // swept or the cascade would be invisible to /me — which is the read every
  // judge surface actually uses.
  [...users, state.session].forEach((u) => {
    if (u && u.judge_anthropic_secret_id === id) {
      u.judge_anthropic_secret_id = null;
      u.judge_anthropic_secret_label = null;
    }
  });
}

export const secretsApi = {
  // ── Secrets ─────────────────────────────────────────────────────────────────
  listSecrets: async () =>
    delay({
      // Default first, then by label — the order the server's query returns.
      secrets: [...secrets]
        .sort((a, b) =>
          a.is_default === b.is_default ? a.label.localeCompare(b.label) : a.is_default ? -1 : 1,
        )
        .map((s) => ({ ...s })),
    }),
  putAnthropicToken: async (_token: string) => {
    // Mirror the real API: a locked vault cannot seal a new token (PRD #32).
    requireUnlockedVault();
    const now = new Date().toISOString();
    // The D14 alias rotates the DEFAULT, or creates the first one labelled
    // "default" — exactly what UpsertDefaultUserSecret does server-side.
    const existing = secrets.find((s) => s.kind === "anthropic_token" && s.is_default);
    if (existing) {
      existing.updated_at = now;
      return delay({ secret: { ...existing } });
    }
    const created: SecretMeta = {
      id: `sec-${Math.random().toString(36).slice(2, 8)}`,
      kind: "anthropic_token",
      label: "default",
      is_default: true,
      // The user's FIRST/SOLE anthropic_token is born pooled (issue #804) so a
      // single-token user has a non-empty auto-select pool; token #2+ stays
      // opt-in. Compute it faithfully off the "no existing anthropic_token" rule,
      // evaluated BEFORE pushing this row, or the mock teaches the wrong lesson.
      auto_eligible: secrets.filter((s) => s.kind === "anthropic_token").length === 0,
      created_at: now,
      updated_at: now,
    };
    secrets.push(created);
    return delay({ secret: { ...created } });
  },

  // ── Token CRUD (PRD #104 M2) ───────────────────────────────────────────────
  createAnthropicToken: async (_token: string, label: string, isDefault: boolean) => {
    requireUnlockedVault();
    const trimmed = label.trim();
    if (trimmed === "") throw new ApiError(400, "label must not be empty");
    rejectInvisibleLabel(trimmed);
    const anthropic = () => secrets.filter((s) => s.kind === "anthropic_token");
    if (anthropic().some((s) => s.label.toLowerCase() === trimmed.toLowerCase())) {
      throw new ApiError(409, "a token with that label already exists");
    }
    // The server FORCES a user's first token to be the default whatever the body
    // asks (the invisible-token hazard); mirror that here or the mock teaches the
    // wrong lesson.
    const first = anthropic().length === 0;
    const wantDefault = isDefault || first;
    if (wantDefault) anthropic().forEach((s) => (s.is_default = false));
    const now = new Date().toISOString();
    const created: SecretMeta = {
      id: `sec-${Math.random().toString(36).slice(2, 8)}`,
      kind: "anthropic_token",
      label: trimmed,
      is_default: wantDefault,
      // The user's FIRST/SOLE anthropic_token is born pooled (issue #804); token
      // #2+ stays opt-in (auto_eligible false). Mirror the server or the mock
      // teaches the wrong lesson.
      auto_eligible: first,
      created_at: now,
      updated_at: now,
    };
    secrets.push(created);
    return delay({ secret: { ...created } });
  },
  patchAnthropicToken: async (
    id: string,
    body: { label?: string; default?: boolean; token?: string },
  ) => {
    const row = secrets.find((s) => s.id === id);
    if (!row) throw new ApiError(404, "token not found");
    if (body.token !== undefined) requireUnlockedVault();
    if (body.default === false) {
      throw new ApiError(400, "cannot clear the default; set another token as default instead");
    }
    if (body.label !== undefined) {
      const trimmed = body.label.trim();
      if (trimmed === "") throw new ApiError(400, "label must not be empty");
      rejectInvisibleLabel(trimmed);
      if (
        secrets.some(
          (s) => s.id !== id && s.kind === row.kind && s.label.toLowerCase() === trimmed.toLowerCase(),
        )
      ) {
        throw new ApiError(409, "a token with that label already exists");
      }
      row.label = trimmed;
    }
    if (body.default === true) {
      secrets.filter((s) => s.kind === row.kind).forEach((s) => (s.is_default = false));
      row.is_default = true;
    }
    row.updated_at = new Date().toISOString();
    return delay({ secret: { ...row } });
  },
  // The auto-selection pool toggle (PRD #111 M2). It also re-derives the token's
  // live eligibility, because in the mock that is the only way the chip beside the
  // toggle can move — and a toggle whose visible consequence never changes is the
  // silent no-op the real feature exists to make visible.
  setTokenAutoEligible: async (id: string, autoEligible: boolean) => {
    const row = secrets.find((s) => s.id === id);
    if (!row) throw new ApiError(404, "token not found");
    row.auto_eligible = autoEligible;
    row.updated_at = new Date().toISOString();
    const meter = mockMyTokenRateLimits.find((t) => t.secret_id === id);
    if (meter) {
      meter.auto_eligible = autoEligible;
      // Opting OUT is always `not_pooled` — that gate comes first server-side too.
      // Opting IN restores the token's OWN fixture state rather than hard-coding
      // `eligible` (web-ux F2): the four states the feature exists for — never
      // polled, stale, no usage data, low headroom — were unreachable in the demo
      // because this line asserted every pooled token is pickable, which is the very
      // thing the chip exists to disprove.
      //
      // This does NOT re-implement the gate. The real status is autoselect.Classify's
      // answer, computed server-side; this restores a fixture value, which is why it
      // lives here and not in lib/rateLimits.ts.
      meter.auto_status = autoEligible ? (pooledFixtureStatus[id] ?? "eligible") : "not_pooled";
    }
    return delay({ secret: { ...row } });
  },
  deleteAnthropicTokenById: async (id: string) => {
    const row = secrets.find((s) => s.id === id);
    if (!row) throw new ApiError(404, "token not found");
    const siblings = secrets.filter((s) => s.kind === row.kind);
    // D6: the default may not be deleted while others exist — promote first.
    if (row.is_default && siblings.length > 1) {
      throw new ApiError(
        409,
        "cannot delete the default token while other tokens exist; set another token as default first",
      );
    }
    secrets = secrets.filter((s) => s.id !== id);
    // The real schema CASCADES: deleting a bound token unbinds its workers and the judge
    // rather than orphaning them. Without this the mock left workers reading "spends
    // console-key" forever — and with one token left the picker is hidden, so there was
    // no way to correct it. Two reasons that matters beyond tidiness: the shipped
    // Dockerfile.mock demo was showing D5's own promise being broken, and D5's cascade
    // otherwise has schema-level evidence only. Mirrored here so a browser can prove the
    // behaviour end to end.
    unbindAnthropicSecret(id);
    return delay(null);
  },

  // ── Vault (PRD #32) ───────────────────────────────────────────────────────────
  // Any non-empty password unlocks in the demo (there is no real crypto); an empty
  // password is treated as the "wrong password" 403 so the banner's error path is
  // browsable.
  vaultUnlock: async (password: string) => {
    if (password.trim() === "") throw new ApiError(403, "incorrect password");
    state.vaultUnlocked = true;
    return delay(null, 150);
  },
  // Passphrase-create (PRD #45): min length 12, then the demo vault is unlocked.
  vaultCreatePassphrase: async (passphrase: string) => {
    if (passphrase.length < 12) throw new ApiError(400, "passphrase must be at least 12 characters");
    state.vaultUnlocked = true;
    return delay(null, 150);
  },
  vaultLock: async () => {
    state.vaultUnlocked = false;
    return delay(null, 100);
  },
  vaultStatus: async () => delay({ unlocked: state.vaultUnlocked }, 40),
  deleteAnthropicToken: async () => {
    // D14: the kind-path alias 409s for a multi-token user — they delete by id.
    const anthropic = secrets.filter((s) => s.kind === "anthropic_token");
    if (anthropic.length > 1) {
      throw new ApiError(
        409,
        "you have multiple tokens; delete a specific one by id (DELETE /api/me/secrets/anthropic_token/{id})",
      );
    }
    // Capture the sole token's id BEFORE filtering it out (at most one survives the 409
    // guard above), then run the same unbind cascade the by-id path does. No token row is
    // a no-op.
    const tokenId = anthropic[0]?.id;
    secrets = secrets.filter((s) => s.kind !== "anthropic_token");
    if (tokenId) unbindAnthropicSecret(tokenId);
    return delay(null);
  },
};
