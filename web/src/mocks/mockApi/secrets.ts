import type {
  AutoStatus,
  CodexAccountRateLimit,
  SecretDependents,
  SecretMeta,
  TokenRateLimits,
} from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { isTerminalRun } from "../../lib/runStatus";
import { mockCodexAliasAccounts, mockMyTokenRateLimits, mockSecrets } from "../data";
import { patchRun, state } from "../store";
import { schedulesPinnedTo } from "./schedules";
import { delay, mockScenario, users } from "./shared";
// secrets ↔ workers is the one accepted import cycle (PRD #991 D4): deleteAnthropicTokenById
// unbinds workers pinned to the deleted token, and workers' setWorkerBindMode resolves a
// token label against this roster. Both reads are inside function bodies and no module-level
// initializer crosses the modules, so the cycle cannot throw at import.
import { workers } from "./workers";

export let secrets: SecretMeta[] = noDefaultScenario(mockSecrets.map((s) => ({ ...s })));

// PRD #1732 D4: the "no-default" demo scenario (?mock=no-default). Every Anthropic token
// is disabled, so the slot has NO default: the Settings card leads with the no-default
// notice (shown even with the Disabled shelf collapsed) and each shelf row offers
// "Enable and make default". Applied once at load, so enabling a token in the demo
// really fills the slot.
function noDefaultScenario(rows: SecretMeta[]): SecretMeta[] {
  if (mockScenario() !== "no-default") return rows;
  const at = new Date(Date.now() - 2 * 86_400_000).toISOString();
  return rows.map((s) =>
    s.kind === "anthropic_token"
      ? { ...s, is_default: false, enabled: false, disabled_at: s.disabled_at ?? at }
      : s,
  );
}

// ── Enablement read filters (PRD #1732 D1/D9/D13) ──────────────────────────────
// The server omits a disabled credential from every rate-limit read, owner and admin.
// The mock keeps each stored reading in its fixture and hides it here, so Enable can
// bring it back. A just-enabled token reads `unavailable` for a few seconds first: its
// pre-disable reading is never current (D13), and the delay is what lets the Settings
// row show "checking usage…" in the demo.
const FRESH_READING_DELAY_MS = 4_000;
const enabledAt = new Map<string, number>();

function isDisabledId(id: string): boolean {
  return secrets.some((s) => s.id === id && s.enabled === false);
}

// `own` marks the signed-in user's list: only there does the meter follow the owner's
// live default flag (a default hand-off on disable moves it). Admin rows only filter.
export function visibleTokenMeters(tokens: TokenRateLimits[], own = false): TokenRateLimits[] {
  const now = Date.now();
  return tokens
    .filter((t) => !isDisabledId(t.secret_id))
    .map((t) => {
      const since = enabledAt.get(t.secret_id);
      const row = own ? secrets.find((s) => s.id === t.secret_id) : undefined;
      const withDefault = row ? { ...t, is_default: row.is_default } : { ...t };
      if (since !== undefined && now - since < FRESH_READING_DELAY_MS) {
        return { ...withDefault, limits: { status: "unavailable" as const } };
      }
      return withDefault;
    });
}

// A Codex account's aliases drop disabled labels; an account left with no alias (every
// alias that named it is disabled) is not polled, so it is not listed at all (D6).
// promoteCredentialDisabledRuns is the mock's promoter (D14): a run held on
// credential_disabled goes back to the queue once the credential it names is enabled, or,
// for work that spends the default (it names none), once its slot has a default again.
function promoteCredentialDisabledRuns(): void {
  for (const r of state.runs.values()) {
    if (r.status !== "paused" || r.hold_reason !== "credential_disabled") continue;
    const pinnedLabel = r.credential_override?.mode === "pinned" ? r.credential_override.label : null;
    const needed =
      r.harness === "codex"
        ? secrets.find((s) => s.id === r.codex_secret_id)
        : pinnedLabel
          ? secrets.find((s) => s.kind === "anthropic_token" && s.label === pinnedLabel)
          : secrets.find((s) => s.id === r.anthropic_secret_id);
    const ready = needed
      ? needed.enabled !== false
      : secrets.some((s) => s.kind === "anthropic_token" && s.is_default && s.enabled !== false);
    if (ready) patchRun(r.id, { status: "queued", hold_reason: null, status_since: new Date().toISOString() });
  }
}

export function visibleCodexAccounts(accounts: CodexAccountRateLimit[]): CodexAccountRateLimit[] {
  const disabledLabels = new Set(
    secrets.filter((s) => s.kind === "codex_auth" && s.enabled === false).map((s) => s.label),
  );
  if (disabledLabels.size === 0) return accounts;
  return accounts
    .map((a) => ({ ...a, aliases: a.aliases.filter((l) => !disabledLabels.has(l)) }))
    .filter((a) => a.aliases.length > 0);
}
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

// Codex credentials (PRD #1147) share ONE default across BOTH kinds, so the
// helpers below scope set-default / first-credential over the union rather than a
// single kind. Kept module-local (not exported) so knip does not flag them.
const CODEX_KINDS = ["codex_auth", "openai_api_key"] as const;
type CodexKind = (typeof CODEX_KINDS)[number];
const isCodexKind = (kind: string): boolean =>
  (CODEX_KINDS as readonly string[]).includes(kind);

// codexOnlySecrets (PRD #1429 M4a): the "codex-only" mock scenario removes every
// anthropic_token row and guarantees a usable Codex default (D3: a linked codex_auth
// or an openai_api_key), so the harness picker/badges and the start/schedule/Run
// Defaults surfaces all see a Codex-only account — no Anthropic credential at all,
// one usable Codex credential. A no-op for every other scenario, and read-side only
// (mirrors credentialOverlay in runs.ts): the underlying `secrets` array — and every
// other mock domain that reads it directly (workers, settings) — is untouched.
function codexOnlySecrets(rows: SecretMeta[]): SecretMeta[] {
  if (mockScenario() !== "codex-only") return rows;
  const withoutAnthropic = rows.filter((s) => s.kind !== "anthropic_token");
  const usableDefault = withoutAnthropic.find(
    (s) => isCodexKind(s.kind) && s.is_default && (s.kind === "openai_api_key" || s.codex_status === "linked"),
  );
  if (usableDefault) return withoutAnthropic;
  // No usable Codex default among the seed rows: demote any existing Codex default
  // (a staging/failed one would otherwise still win the "is_default" lookup) and add
  // one linked subscription so the scenario always has a usable Codex credential.
  const now = new Date().toISOString();
  return [
    ...withoutAnthropic.map((s) => (isCodexKind(s.kind) ? { ...s, is_default: false } : s)),
    {
      id: "sec-codex-demo",
      kind: "codex_auth",
      label: "codex-demo",
      is_default: true,
      enabled: true,
      disabled_at: null,
      auto_eligible: false,
      codex_status: "linked",
      created_at: now,
      updated_at: now,
    },
  ];
}

function createCodexCredential(kind: CodexKind, label: string, isDefault: boolean) {
  requireUnlockedVault();
  const trimmed = label.trim();
  if (trimmed === "") throw new ApiError(400, "label must not be empty");
  rejectInvisibleLabel(trimmed);
  // Label uniqueness is per-kind, like the Anthropic path.
  if (secrets.some((s) => s.kind === kind && s.label.toLowerCase() === trimmed.toLowerCase())) {
    throw new ApiError(409, "a credential with that label already exists");
  }
  const codex = () => secrets.filter((s) => isCodexKind(s.kind));
  // FIRST codex credential across BOTH kinds is force-defaulted server-side, and so is a
  // new one while the shared slot has NO default (every credential disabled, PRD #1732
  // D4): the server's `req.Default || n == 0 || !hasDefault`.
  const first = codex().length === 0;
  const wantDefault = isDefault || first || !codex().some((s) => s.is_default);
  if (wantDefault) codex().forEach((s) => (s.is_default = false));
  const now = new Date().toISOString();
  const created: SecretMeta = {
    id: `sec-${Math.random().toString(36).slice(2, 8)}`,
    kind,
    label: trimmed,
    is_default: wantDefault,
    enabled: true,
    disabled_at: null,
    // No Codex auto-selection pool.
    auto_eligible: false,
    // The resolver-observed lifecycle state at birth: a login stages, a key is static.
    codex_status: kind === "codex_auth" ? "staging" : "static",
    created_at: now,
    updated_at: now,
  };
  secrets.push(created);
  return delay({ secret: { ...created } });
}

// PRD #1732: the server refuses to promote a disabled credential (make it the default, or
// opt it into the auto-select pool) with workersvc.ErrCredentialDisabled as a 409, before
// any other field of the same PATCH is written.
const CREDENTIAL_DISABLED = "credential is disabled; enable it in Settings";
function refuseIfDisabled(row: SecretMeta) {
  if (row.enabled === false) throw new ApiError(409, CREDENTIAL_DISABLED);
}

// refusesDefaultDelete mirrors the delete handlers' `n > 1 || (disabled && n > 0)` over
// CountEnabledSecretSlot: n counts the slot's ENABLED rows (the default itself included
// when it is enabled), so a disabled default is refused while any enabled row remains.
function refusesDefaultDelete(row: SecretMeta, slot: SecretMeta[]): boolean {
  const n = slot.filter((s) => s.enabled !== false).length;
  return n > 1 || (row.enabled === false && n > 0);
}

function patchCodexCredential(
  kind: CodexKind,
  id: string,
  body: { label?: string; default?: boolean; token?: string },
) {
  const row = secrets.find((s) => s.id === id);
  // The real route is kind-scoped (PATCH /me/secrets/{kind}/{id}), so an id of the
  // wrong kind (e.g. an anthropic_token or the sibling codex kind) is a 404, not a hit.
  if (!row || row.kind !== kind) throw new ApiError(404, "credential not found");
  if (body.token !== undefined) requireUnlockedVault();
  if (body.default === false) {
    throw new ApiError(400, "cannot clear the default; set another credential as default instead");
  }
  if (body.default === true) refuseIfDisabled(row);
  if (body.label !== undefined) {
    const trimmed = body.label.trim();
    if (trimmed === "") throw new ApiError(400, "label must not be empty");
    rejectInvisibleLabel(trimmed);
    if (
      secrets.some(
        (s) => s.id !== id && s.kind === row.kind && s.label.toLowerCase() === trimmed.toLowerCase(),
      )
    ) {
      throw new ApiError(409, "a credential with that label already exists");
    }
    row.label = trimmed;
  }
  if (body.default === true) {
    // Single default across BOTH codex kinds.
    secrets.filter((s) => isCodexKind(s.kind)).forEach((s) => (s.is_default = false));
    row.is_default = true;
  }
  row.updated_at = new Date().toISOString();
  return delay({ secret: { ...row } });
}

function deleteCodexCredential(kind: CodexKind, id: string) {
  const row = secrets.find((s) => s.id === id);
  // The real route is kind-scoped (DELETE /me/secrets/{kind}/{id}), so an id of the
  // wrong kind (e.g. an anthropic_token or the sibling codex kind) is a 404, not a hit.
  if (!row || row.kind !== kind) throw new ApiError(404, "credential not found");
  // D12, as deleteCodexSecretByID: the default may not go while another ENABLED credential
  // in the shared Codex slot remains (CountEnabledSecretSlot spans both codex kinds).
  if (row.is_default && refusesDefaultDelete(row, secrets.filter((s) => isCodexKind(s.kind)))) {
    throw new ApiError(
      409,
      "cannot delete the default credential while other enabled credentials exist; set another credential as default first",
    );
  }
  secrets = secrets.filter((s) => s.id !== id);
  return delay(null);
}

export const secretsApi = {
  // ── Secrets ─────────────────────────────────────────────────────────────────
  // The demo has no provider connection, so an explicit Test stays inconclusive.
  testSecret: async (_kind: "anthropic_token" | "codex_auth" | "openai_api_key", _id: string) =>
    delay({ status: "inconclusive" as const, reason: "generic" as const }),
  listSecrets: async () =>
    delay({
      // Default first, then by label — the order the server's query returns.
      // PRD #1429 M4a: codexOnlySecrets overlays the "codex-only" scenario read-side.
      secrets: codexOnlySecrets([...secrets])
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
      enabled: true,
      disabled_at: null,
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
    // ...and the same query forces it while the slot has NO default row (every token
    // disabled, PRD #1732 D4): InsertUserSecret's `NOT EXISTS (… AND is_default)`. The
    // pool opt-in below stays first-token-only, as on the server.
    const wantDefault = isDefault || !anthropic().some((s) => s.is_default);
    if (wantDefault) anthropic().forEach((s) => (s.is_default = false));
    const now = new Date().toISOString();
    const created: SecretMeta = {
      id: `sec-${Math.random().toString(36).slice(2, 8)}`,
      kind: "anthropic_token",
      label: trimmed,
      is_default: wantDefault,
      enabled: true,
      disabled_at: null,
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
    if (body.default === true) refuseIfDisabled(row);
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
    if (autoEligible) refuseIfDisabled(row);
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
    // D6/D12: the default may not be deleted while another ENABLED token exists — promote
    // first. Disabled siblings do not count (DeleteAnthropicTokenByID's enabled-slot count).
    if (row.is_default && refusesDefaultDelete(row, secrets.filter((s) => s.kind === row.kind))) {
      throw new ApiError(
        409,
        "cannot delete the default token while other enabled tokens exist; set another token as default first",
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

  // ── Codex / OpenAI credentials (PRD #1147 M3) ──────────────────────────────
  // Two kinds share ONE default and ONE card: `codex_auth` (a Codex login, born
  // "staging" — a resolver later moves it to "linked"/"failed") and `openai_api_key`
  // (a static Console key, born "static"). The user's FIRST codex credential across
  // BOTH kinds is force-defaulted server-side, and set-default is single across both
  // kinds. No auto-selection pool — an `auto` worker never spends a Codex credential.
  createCodexAuth: async (_token: string, label: string, isDefault: boolean) =>
    createCodexCredential("codex_auth", label, isDefault),
  patchCodexAuth: async (
    id: string,
    body: { label?: string; default?: boolean; token?: string },
  ) => patchCodexCredential("codex_auth", id, body),
  deleteCodexAuthById: async (id: string) => deleteCodexCredential("codex_auth", id),
  createOpenAIApiKey: async (_token: string, label: string, isDefault: boolean) =>
    createCodexCredential("openai_api_key", label, isDefault),
  patchOpenAIApiKey: async (
    id: string,
    body: { label?: string; default?: boolean; token?: string },
  ) => patchCodexCredential("openai_api_key", id, body),
  deleteOpenAIApiKeyById: async (id: string) => deleteCodexCredential("openai_api_key", id),

  // ── Disable / Enable (PRD #1732 D4, D11) ─────────────────────────────────────
  // Mirrors PatchSecretEnabled: kind-scoped 404, idempotent repeat (the original
  // disabled_at is kept), the REQUIRED enabled replacement when disabling a default while
  // another enabled credential exists (409), the atomic default clear on the slot's last
  // enabled credential, and Enable filling an empty slot's default.
  setSecretEnabled: async (kind: string, id: string, enabled: boolean, newDefaultId?: string) => {
    const row = secrets.find((s) => s.id === id);
    if (!row || row.kind !== kind) throw new ApiError(404, "secret not found");
    const inSlot = (s: SecretMeta) =>
      isCodexKind(row.kind) ? isCodexKind(s.kind) : s.kind === row.kind;
    const slot = secrets.filter(inSlot);
    if ((row.enabled !== false) === enabled) return delay({ secret: { ...row } });
    if (!enabled) {
      if (row.is_default) {
        const enabledCount = slot.filter((s) => s.enabled !== false).length;
        if (enabledCount > 1) {
          const replacement = slot.find(
            (s) => s.id === newDefaultId && s.id !== id && s.enabled !== false,
          );
          if (!replacement) throw new ApiError(409, "choose an enabled replacement in Settings");
          slot.forEach((s) => (s.is_default = false));
          replacement.is_default = true;
        } else {
          row.is_default = false;
        }
      }
      row.enabled = false;
      row.disabled_at = new Date().toISOString();
      enabledAt.delete(id);
    } else {
      row.enabled = true;
      row.disabled_at = null;
      if (!slot.some((s) => s.is_default && s.enabled !== false)) {
        slot.forEach((s) => (s.is_default = false));
        row.is_default = true;
      }
      enabledAt.set(id, Date.now());
      // A held run waiting on this credential resumes by itself (D14): the mock promotes it.
      promoteCredentialDisabledRuns();
    }
    row.updated_at = new Date().toISOString();
    return delay({ secret: { ...row } });
  },
  getSecretDependents: async (kind: string, id: string): Promise<SecretDependents> => {
    const row = secrets.find((s) => s.id === id);
    if (!row || row.kind !== kind) throw new ApiError(404, "secret not found");
    const page = <T,>(items: T[]) => ({ items: items.slice(0, 20), total: items.length });
    const boundWorkers = workers
      .filter((w) => w.anthropic_secret_id === id)
      .map((w) => ({ id: w.id, name: w.name }));
    const pinnedSchedules = row.kind === "anthropic_token" ? schedulesPinnedTo(row.label) : [];
    const liveRuns = [...state.runs.values()]
      .filter(
        (r) =>
          !isTerminalRun(r.status) &&
          (r.anthropic_secret_id === id ||
            r.codex_secret_id === id ||
            (r.credential_override?.mode === "pinned" &&
              row.kind === "anthropic_token" &&
              r.credential_override.label === row.label)),
      )
      .map((r) => ({ id: r.id, status: r.status }));
    const account = mockCodexAliasAccounts[id];
    const siblings = account
      ? secrets
          .filter((s) => s.id !== id && s.enabled !== false && mockCodexAliasAccounts[s.id] === account)
          .map((s) => ({ id: s.id, label: s.label }))
      : [];
    return delay({
      default: row.is_default,
      judge: state.session?.judge_anthropic_secret_id === id,
      workers: page(boundWorkers),
      schedules: page(pinnedSchedules),
      runs: page(liveRuns),
      enabled_siblings: page(siblings),
    });
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
