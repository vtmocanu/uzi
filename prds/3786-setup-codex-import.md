# PRD #3786: Operator setup imports an existing Codex login

**Issue**: [#3786](https://github.com/vtmocanu/uzi/issues/3786)
**Status**: Draft, design proposed by the maintainers for the reporter to review and implement
**Priority**: Normal
**Created**: 2026-11-02
**Depends on**: PRD #3760 (the `/server setup` command, design in PR #3782). This PRD adds one step to that command and cannot ship before it.
**Related**: #3760 (original ask), PRD #1147 (Codex credentials), PRD #32 (per-user vault), `docs/codex-credentials.md`, `docs/vault-threat-model.md`

## Problem

PRD #3760 lets an operator script first setup (forge, repos, first worker) without a browser. The fourth piece #3760 asked for, saving a Codex login so the seed admin can run Codex-harness work, still needs the web UI: the only write path is the cookie-only `POST /api/me/secrets/codex_auth` (`api/internal/handler/routes_me.go`), deliberately kept off Bearer tokens so a stolen `uzc_` cannot replace a user's credentials.

Saving a Codex credential is not a plain row insert. The web path (`createCodexSecret` in `api/internal/handler/secrets.go`) validates the value, seals it through the vault (`Vault.Seal(..., vault.SealForUse)`), and inside a READ COMMITTED transaction takes the per-user secret-mutation lock, fences the vault mode and epoch it sealed under (`FenceVaultWrite`), computes the single shared default across both Codex kinds, then inserts the credential and its `staging` credential-state row together. Fresh empty databases start with Optional vault policy, so newly seeded admins start Off (`seed.Admin` in `api/internal/seed/admin.go`). Existing or Required-policy installations can have protected seed admins, whose existing vault wrapper must be unlocked before sealing.

## Outcome

`/server setup` gains an optional, create-only step importing an existing Codex subscription login for the seed admin. It goes through the same credential logic the web UI uses, makes no provider calls, and reports the credential as saved and pending verification.

Acceptance examples:

1. **Protected seed admin with an existing wrapper.** Following `docs/codex-credentials.md`, the operator has obtained a dedicated, isolated uzi login in `UZI_LOGIN_DIR` (a directory used only for this uzi login, never the operator's everyday Codex home). The API has booted and initialized the protected seed admin's wrapper. The operator projects the isolated file's tokens:
   `jq -c '{access_token: .tokens.access_token, refresh_token: .tokens.refresh_token}' "$UZI_LOGIN_DIR/auth.json" | docker compose exec -T api /server setup … --codex-auth-file -`
   with the forge token from `--forge-token-file /run/secrets/forge_pat`. The report shows the Codex step `saved_pending_verification` with label `default`, and the command exits 0. No provider request left the setup process.
2. **Wrong or missing password.** Same command with `UZI_SEED_PASSWORD` absent or wrong while the vault is protected. Setup refuses before any requested provisioning write: exit 2, no forge, repo, worker or credential write, no default change.
3. **Re-run.** Same command again. The Codex step finds a credential with label `default` and reports `present` without touching its material, state or default flag; exit 0.

## Out of scope

- **Importing an OpenAI API key.** A follow-up if wanted: its state is `static`, the staged-login reconciler does not verify it, and it would need its own report semantics.
- **Running a provider login flow** (device code or browser OAuth). Setup imports login material the operator already has.
- **Replacing, rotating or deleting** an existing Codex credential, or switching which credential is the default. Web UI actions.
- **Parsing a whole `auth.json`.** The accepted format stays the flat `{access_token, refresh_token}` object the web UI takes; `docs/codex-credentials.md` already documents the `jq` projection.
- **Unlocking the running API's vault cache.** Setup unlocks only its own process; see Decision 5.
- **Forcing identity verification.** Linking stays asynchronous in the API (Decision 6).
- **Any change to `uzc_`, `uza_` or the cookie-only routes.**

## Modules and seams

### 1. Shared Codex credential creation (prefactor)

Extract the transactional core of `createCodexSecret` into a service operation, for example `CreateCodexCredential(ctx, userID, kind, label, value, wantDefault) (row, status, error)`, called by both the handler and setup. It stays generic over both Codex kinds; setup uses only `codex_auth`.

- Inputs are already validated (`validateCodexValue`, `validateSecretLabel`) by the caller through shared validators.
- Seals with `Vault.Seal(..., vault.SealForUse)` before the transaction, then runs the existing lock, fence, default computation across both Codex kinds, `InsertCodexSecret` and `InsertCodexCredentialState` as one transaction.
- **Create-only option for setup**: under the secret-mutation lock, a label collision returns a distinct "exists" result without changing material, state or defaults. The web handler keeps its existing 409 mapping.
- **Stays in the handler**: HTTP status mapping, and the post-commit API-process hooks (the Codex usage poller poke and credential promotion request). Setup runs in another process and cannot reach them.

Invariant: the web endpoints' status codes, bodies and side effects are unchanged.

### 2. The setup Codex step

- **Flags**: `--codex-auth-file PATH|-`; optional `--codex-label`, default `default`. No default-switch flag.
- **One stdin source per invocation**: at most one of the forge token and Codex inputs may be `-`; violating that exits 2 before any write.
- **Vault**: setup constructs its own vault instance over the same database and `UZI_SECRET_KEY`. For a protected vault it calls `UnlockExisting` with `UZI_SEED_PASSWORD` (the same password the API's boot unlock uses, `seed.UnlockAdminVault`). Never `Unlock` or `Enable`: a missing wrapper is a refusal, never an enrollment. An unprotected (Off) vault needs no password and still seals through the policy-aware `Seal`.
- **Key cleanup**: never `Vault.Lock`, `LockWithContext` or `LockAndAcknowledge`; they durably revoke the user's browser slots (`api/internal/vault/lifecycle.go`). Discard the process-local key through a narrowly scoped cache-destruction helper, or by process exit.
- **Preflight and exit codes**: before any requested provisioning write, read and validate all inputs and, when a Codex import targets a protected vault, complete `UnlockExisting`. Missing or wrong password and missing wrapper exit 2. This follow-up qualifies PRD #3760's exit-2 guarantee: no requested forge, repo, worker or credential provisioning writes occur, but existing vault-unlock housekeeping may occur (`UnlockExisting` can clear the lock notice and lazily migrate legacy master-sealed material). Failures during credential sealing or transactional persistence exit 1. The report distinguishes these cases without secret bytes.
- **Ordering**: the Codex step runs after the forge and repo steps and before the worker step. It needs no forge, so a repo refusal does not skip it. Worker creation retains PRD #3760's forge and repo prerequisites; a Codex-only failure does not add another worker prerequisite.
- **Report**: `saved_pending_verification` (credential state `staging`), `present`, `refused` (with a reason that never includes secret bytes), or `not_requested`. Never "ready".

**Resource bounds**: the Codex input is read up to the existing 64 KiB cap (`maxCodexBlobBytes`) plus one byte and refused if larger. The vault unlock runs under the existing `LifecycleTimeout` inside setup's overall five-minute deadline. No provider calls, so no new network spend.

## Testing decisions

- **Prefactor**: the existing `createCodexSecret` handler tests pass unchanged; service tests cover the default rule across both kinds and rollback of the credential plus its state row together.
- **Setup step** (live-DB, the same harness as PRD #3760): protected and Off vaults; wrong and missing password; missing wrapper; each of those three refused before any forge or repo write; label collision and two concurrent imports with the same label (exactly one saved, the other `present`); an existing `openai_api_key` default left unchanged by a non-default import; a vault mode or epoch change between seal and write (fenced, no write); secret-safe stdout and stderr; and proof of zero provider calls (a fake forge and no network transport).
- **Unlock side effects**: tests assert the provisioning guarantees (no requested write, no credential insert, no default change on failure), not "no database writes at all".
- Follow `api/internal/seed/anthropic_test.go` for the fake sealer pattern.

## Milestones

### M1: Prefactor, shared Codex credential creation
Extract the operation; the handler calls it. No behaviour change.
- **Blocked by**: none (can land before PRD #3760's setup command)
- **Acceptance**: handler tests unchanged and green; new service tests for default selection, create-only collision and transactional rollback; `task gate:api` green.

### M2: `setup` saves a Codex login
The flags, single-stdin rule, preflight unlock, report states, docs (operator setup page and `docs/codex-credentials.md`), for Compose and Kubernetes.
- **Blocked by**: M1, and PRD #3760's setup command merged
- **Acceptance**: the three acceptance examples pass in the live-DB test, plus the regression list above; `task gate:api` green.

### Landing obligations
- `task docs:sync` after `docs/*.md` edits; CHANGELOG fragment.
- Add the Codex step to the live `acceptance` issue filed for PRD #3760 (fresh Compose host and Kubernetes via `kubectl exec -i`), including that the login links once the API's poller runs.

## Decision Log

1. **Save-only, no provider calls in setup.** Setup only stores the staged alias and does not wait for provider verification. The API's existing reconciler performs nonrotating identity discovery and canonical account linking (`CodexReconciler.ReconcileCodexAuthIdentity` via `codexusagepoller`); setup does not invoke it. Rejected: verifying the login inside setup.
2. **Reuse the web path's transactional core, not a direct insert.** The lock, fence and two-kind default rule are what keep credentials consistent under concurrency and vault changes. Rejected: a seed-style insert.
3. **`UZI_SEED_PASSWORD` unlocks a protected vault** (maintainer decision 2026-11-02). A headless protected seed admin already keeps it in the API environment for the boot unlock, so setup adds no new secret. Rejected: a separate `--vault-password-file`, which only helps when the operator also disables boot unlock.
4. **`UnlockExisting`, never `Unlock` or `Enable`.** Setup must not enroll or create a vault wrapper; the API's boot unlock owns that. Never `Lock*` for cleanup, because it revokes browser slots durably.
5. **Process boundary accepted.** Unlocking in setup does not warm the API's cache. On a protected vault the API's own boot unlock already holds the key, so the API's reconciler can open the new credential; where that unlock is not present, the credential stays pending until the owner unlocks through the normal path.
6. **No promise of eventual linking in every configuration.** With the Codex usage poller disabled (interval 0), staged reconciliation does not run. The report and docs say "pending verification", never "will link".
7. **Flat input format only.** One parser (`validateCodexAuthBlob`); the documented `jq` projection turns a Codex `auth.json` into it. Rejected: accepting nested `auth.json`.
8. **Create-only by label, default label `default`.** It matches the startup Anthropic seed's `default` naming. A collision is `present`, never a replacement.
9. **At most one stdin source.** Several secret inputs cannot share stdin; refusing up front keeps the parse unambiguous.
10. **Unlock in preflight.** A wrong password or missing wrapper must not surface after forge and repo writes. Rejected: unlocking lazily at the Codex step.
11. **Codex subscription login only.** An API key has no staged verification, so `saved_pending_verification` would misdescribe it. Rejected: importing `openai_api_key` in this PRD.
