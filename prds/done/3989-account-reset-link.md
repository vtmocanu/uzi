# PRD #3989: Admin-issued account reset link for a forgotten password or vault passphrase

**Status**: Done
**Priority**: Normal
**Area**: security

## Problem

A user who forgets the secret that protects their vault has no way back:

- **Password accounts.** The vault key-encryption key is derived from the login password (`docs/vault-threat-model.md`, "What the vault protects"). Forgetting the password means the user can neither log in nor unlock the vault. There is no "forgot password" flow (`docs/auth-design.md`, "Accepted limitations"), no admin endpoint that sets another user's password (the `UpdatePassword` query has no HTTP or CLI caller; `Vault.UpdatePassword` in `api/internal/vault/auth_lifecycle.go` is an unused seam), and `UZI_SEED_PASSWORD` never touches an existing user.
- **OIDC-only accounts.** They can still log in through SSO, but the vault passphrase is create-only (`adr/3507-optional-credential-vault.md`: "Passphrase change and recovery remain deferred"). A forgotten passphrase leaves every protected credential permanently locked, and the create-only endpoint refuses a new one while the old wrapper exists.

Today the only exit is an operator editing Postgres by hand, which is unsupported and skips session revocation.

PRD #32 already recorded the constraint any reset must honour (`prds/done/32-user-vault-password-wrapped-secrets.md`, "Password change / reset"): the DEK is unrecoverable by design, so a reset deletes the wrapper and every DEK-sealed row and tells the user to re-enter tokens; "an admin reset that silently kept unreadable ciphertext would be a worse bug than the data loss". This PRD implements that reset.

## Outcome

An admin can issue a **one-time reset link** for another active, non-seed user. The admin hands it over out of band (uzi sends no email). Redeeming it:

- deletes **all** of the user's model credentials (every `user_secrets` row of every kind and every `codex_provider_account` row, whatever their sealing marker), the vault wrapper and all browser re-unlock slots, in one transaction;
- for a password account, sets the new password the user types on the redemption page;
- ends every browser session (`token_version` bump) and denies the user's approved, unconsumed CLI browser-login requests (a pending request has no user yet and stays pending, but the old sessions can no longer approve it), for password and OIDC-only accounts alike;
- leaves the account **protected** with no wrapper, ready to enroll a fresh vault through the existing create path (next password login, `POST /api/vault/enable` from an SSO session, or passphrase creation for OIDC-only accounts);
- makes the user's affected runs wait instead of failing: a run claimed while the user has no credential for it parks as `paused` with `hold_reason = 'credentials_reset'` and is promoted back to queued automatically once a usable credential exists.

Forge connections, repos, schedules, run history, settings, CLI tokens and product OAuth grants are untouched.

Acceptance examples:

1. **Password user.** Admin opens Admin → Users, chooses "Reset link" on user A, copies the link shown once. User A opens it within the hour, reads the warning (what is deleted; that a run pinned to a deleted credential will use the default credential once new ones exist), types and confirms a new password, submits. User A's old browser sessions now return 401; logging in with the new password succeeds and enrolls a new protected vault; Settings shows no Anthropic, OpenAI or Codex credentials. User A's queued issue run, once claimed, shows paused with "credentials were reset"; after user A adds an Anthropic token it returns to queued on its own.
2. **OIDC-only user.** Same flow; the redemption page asks for no password. User B signs in through SSO, creates a new vault passphrase from the app-wide banner shown after SSO sign-in (the create-only endpoint now accepts it), and re-adds credentials.
3. **Misuse.** Reusing a link, an expired link, a link superseded by a newer one, a link whose target or issuer has since been deactivated, or whose issuer is no longer an admin, all show one generic "this link is invalid or expired" page and change nothing. An admin cannot issue a link for themselves or for the seed admin.

## Out of scope

- **Self-service OIDC passphrase reset.** It needs proof of a fresh IdP authentication; the OIDC flow sends no `prompt=login` or `max_age` and checks no `auth_time` (`api/internal/oidc`). Parked until that exists.
- **Self-service password change** (old password known, rewrap the DEK; PRD #32 "Change"). A separate feature.
- **Email delivery** of the link.
- **Stopping or fencing runs that are already executing** (user decision: a forgotten password is not a compromise response). A running run keeps the plaintext its worker already holds until it ends; its Codex refresh or rate-limit writes against a deleted account row fail closed. Documented residual.
- **Revoking CLI tokens (`uzc_` / `uza_`) and product OAuth grants.** Neither opens the vault without the password; revoking them stays the user's or admin's existing choice. In-flight CLI login requests are denied (above) because they would otherwise mint a new token after the reset.
- **CLI issuance.** Admin writes are cookie-only by design (`api/internal/handler/routes_admin.go`, PRD #64). CLI read surfaces must render the new hold reason; no new CLI command.
- **Resetting the seed admin.** Its password is the operator's `UZI_SEED_PASSWORD`, and boot unlock (`api/internal/seed/admin.go`, `api/cmd/server/main.go`) refuses to start when that no longer matches, so issuance refuses the seed admin rather than changing boot behaviour.
- **A general audit-log facility.** The link row is the audit record for this feature.
- **Changing missing-credential behaviour for users who were never reset.** Their runs keep today's terminal `credential_unavailable`.

## Modules and seams

### 1. `vault.Reset` (api/internal/vault)

- **Interface**: `func (v *Vault) Reset(ctx context.Context, owner uuid.UUID, fn func(ctx context.Context, q <tx store>) error) error`, built like `authMutation` in `auth_lifecycle.go`: the vault takes the per-owner lifecycle mutex (`acquireLifecycle`), then opens its own transaction (`v.transact`), runs `lockLifecycleState` (which takes `LockSecretMutation`), runs the caller's `fn` (link consumption, auth changes, CLI-request denial), then deletes the material and calls `advanceMode` to protected, which increments the epoch; the users-row trigger bumps the browser-slot generation (`00379`). After commit it evicts the cached DEK; uncertain commits go through `finishLifecycle` like every other lifecycle operation. Never call it from inside a caller-opened transaction.
- **Deletes**: `user_vaults`; every `user_secrets` row and every `codex_provider_account` row of the owner (no query deletes Codex accounts today; add one). Existing FKs set run, worker and judge bindings to NULL and cascade rate-limit and Codex state rows (verify each). `users.sidebar_codex_account_ids` has no FK: readers must ignore ids that no longer resolve.
- **Fencing**: every writer of `user_secrets` and `codex_provider_account` must already fence mode and epoch inside its write transaction; a seal started before the reset is refused after it.
- **Invariants**: never rewraps, reuses or reads a cached DEK, even while unlocked. Never leaves a ciphertext row the user can no longer open. Idempotent on an owner with nothing to delete.
- **Tests**: fake-store unit tests in `api/internal/vault` (follow `lifecycle_test.go`, `lifecycle_contention_test.go`) for ordering, cache eviction and contention with unlock; live-DB tests in a new `api/internal/store/vault_reset_livedb_test.go` (`Test…LiveDB`, follow `vault_lifecycle_livedb_test.go`): rows deleted including `master_aad` ones, FK side effects, a Codex refresh after reset fails closed and recreates no row (`workersvc/codexrefresh.go` recovery-slot update), a sealed insert started before the reset is refused by the epoch fence.

### 2. Reset links (store + handler)

- **Storage**: new table, e.g. `account_reset_links(id, user_id, token_hash UNIQUE, kind, target_is_admin, issued_by, created_at, expires_at, redeemed_at, superseded_at)`; `issued_by` with an explicit `ON DELETE` rule (reject or cascade-supersede; record the choice). Token: 256 random bits, only its sha256 stored, plaintext shown once (the worker join-token pattern). Migration rides in M1; number assigned at merge time.
- **Issue**: `POST /api/admin/users/{id}/reset-link` in the cookie-only admin write group (RequireAuth + RequireAdmin + CSRF), target from the path. Locks the target `users` row `FOR UPDATE`, then supersedes any live link for that target and inserts the new one (1 hour TTL), so concurrent issues leave one live link. Refuses: unknown or inactive target, self (400), the seed admin (`UZI_SEED_EMAIL`). Returns `{token, expires_at, kind, target_is_admin}`; the web composes the URL from its own origin.
- **Preview**: `POST /api/auth/reset/preview` with `{token}`, unauthenticated, behind the auth limiter. Returns `{kind, expires_at}` only, `kind` derived from the current users row. Every failure cause returns one byte-identical 404.
- **Redeem**: `POST /api/auth/reset/redeem` with `{token, password?}`, unauthenticated, behind the auth limiter. Unlocked lookup of the link to learn `user_id` and `kind`; for `kind=password` validate the new password with the registration policy (`auth.MinPasswordLen` / `MaxPasswordLen`) and hash it with the registration Argon2id parameters **before** any lock; then `vault.Reset(owner, fn)` where `fn` locks the link row `FOR UPDATE` and re-checks: unredeemed, unexpired, unsuperseded, target active, issuer active and still an admin, `kind` still matching the users row (else the generic 404 and rollback); sets `password_hash` and bumps `token_version` (`vault_only`: bump only); denies the user's approved, unconsumed `cli_auth_requests` (pending ones have no `user_id`; see Correction 4); sets `redeemed_at` and the user's credentials-reset marker (module 3). Exactly one concurrent redemption succeeds. Redemption does not log the user in.
- **Lock order** everywhere: lifecycle mutex, then the lifecycle state locks, then the link row. Issue takes only the users row.
- **Untrusted input bounds**: token must be the exact fixed length before hashing; password bounded by the registration limits before Argon2; both endpoints ride the existing per-IP auth limiter, so redeem's single Argon2 adds nothing beyond login's budget. Nothing irreversible happens before the locked re-check passes.
- **Revocation on admin changes**: `PatchUser` deactivation or demotion supersedes every live link the user issued and every live link targeting them.
- **Token placement**: the link carries the token in the URL fragment (`/reset#<token>`), never path or query; the SPA reads it, clears it with `history.replaceState`, and sends it only in POST bodies.
- **Audit**: the link row plus `slog` lines on issue and redeem with user ids and `target_is_admin`, never the token. Docs state that an admin who redeems a link they issued controls the account's non-vault data, and that issuing for another admin allows taking over that admin account.
- **Tests**: handler live-DB tests (follow `cli_auth_flow_livedb_test.go`, `vault_toggle_livedb_test.go`): issue needs admin cookie + CSRF and a `uza_` bearer gets 401; refusals (self, seed, inactive); single use under concurrent redemption; concurrent issue leaves one live link; expiry; supersession; issuer demoted or deactivated; target deactivated then reactivated; `kind` drift; byte-identical 404 bodies; old session cookie 401s; an approved, unconsumed CLI login request cannot mint a token after redeem; limiter mounted on both routes (`route_limiter_mounts_test.go`).

### 3. Runs park instead of failing after a reset (workersvc)

- **Mechanism**: extend the existing `credential_disabled` park (PRD #1732; `api/internal/store/queries/credential_disabled.sql`, `api/internal/workersvc/credential_disabled.go`, `claimCredentialDisabled` in `claim_recovery.go`, `parkChatCredentialDisabled` in `chat.go`) with a new `hold_reason = 'credentials_reset'`. Redemption writes a per-user reset marker (e.g. `users.credentials_reset_at`). At claim, where assembly would today end in terminal `credential_unavailable` because the user holds no model credential for the run's harness, a run created before the marker parks through the same exact-claim park queries with `credentials_reset`, classified in `finishRunClaimTx` next to the existing `credDisabled` decision (`claim_recovery.go`). Users with no marker, or runs created after it, keep today's behaviour.
- **Typed sentinel**: `errCredentialUnavailable` also covers forge-origin refusal, repo-owner mismatch and undecryptable bot PAT or token (`claim_assembly.go`, `secretchoice.go`). Add a narrower sentinel (e.g. `errNoModelCredential`, wrapping `errCredentialUnavailable` so existing handling is unchanged) emitted only where the user has no model credential: the "no Anthropic token configured" sites in `secretchoice.go` and the Codex no-alias path in `claim_assembly.go`. Classify with `errors.Is` on the new sentinel plus the marker, never on `errCredentialUnavailable`.
- **Promotion**: the existing promoter (`promoteCredentialDisabledRuns` and its sweep fallback) also handles `credentials_reset`, under the lock order documented in `credential_disabled.go`, but with its own requirement rule: `credentialRequirementMet` deliberately treats an unresolvable requirement (NULL binding, deleted credential) as met, and after a reset every binding is NULL, so reusing it would promote and re-park every sweep and burn claim slots. A `credentials_reset` run is promoted only when the harness resolver (`ErrNoCredentialForHarness`, `workersvc/harness_create.go`) finds an enabled credential for the run's harness. Credential creation and vault unlock already request promotion (`withSecretLock` in `handler/secrets.go`, `handler/vault.go`). If the credential vanishes again before the next claim, the claim-time check parks the run again, so promotion never causes a terminal failure.
- **No manual resume**: like `credential_disabled` (PRD #1732 D12), owner Resume refuses a `credentials_reset` hold. Add it to both `hold_reason IS DISTINCT FROM 'credential_disabled'` predicates of `ResumePausedRun` in `api/internal/store/queries/runtime.sql` and to the named refusal in `api/internal/handler/resume.go`, which tells the user to add a credential. Only the promoter releases it.
- **Kinds**: exactly the kinds the `credential_disabled` park already covers (the run claim lane and the chat twin). `judge`, `job` and `cross_check` keep today's behaviour; state that in docs.
- **Pinned overrides**: a run pinned to a deleted credential resolves to "inherit" exactly as a token delete does today (`00233_per_run_credential_override.sql`, PRD #1247 D1); the redemption warning says so.
- **Schema and SQL predicates**: add `credentials_reset` to the `hold_reason` CHECK (latest definition in `00391_publication_resume.sql`), plus the users marker column. `hold_reason = 'credential_disabled'` is hard-coded as a SQL predicate in 11 places in `credential_disabled.sql` and in the partial index from `00258_secret_enablement.sql`: each list, promote and lock query that must also see the new hold, and the index, must include it (a new index if needed). Also update every Go, agent and web mirror of the hold reasons. `git grep -F credential_disabled` lists all of these sites.
- **Rendering**: web run views and `CredentialDisabledPanel.tsx` (or a sibling) explain the reset hold; CLI run renderers in `api/cmd/uzi/` that print `hold_reason` show it.
- **Tests**: workersvc live-DB tests: a reset user's queued run parks at claim instead of failing; a never-reset user's run still fails `credential_unavailable`; a forge-origin refusal still fails terminally for a user with a reset marker; with no credential the run stays parked across two promoter sweeps; promotion on credential creation; re-park when the credential disappears between promotion and claim; owner Resume of a `credentials_reset` hold is refused with the named message and changes nothing; chat twin.

### 4. Web

- **Admin → Users** (`web/src/pages/AdminUsers.tsx`): "Reset link" per eligible user; a confirmation dialog stating what redemption deletes and, for an admin target, that this can take over an admin account; then the link once with copy button, expiry and kind.
- **Redemption page**: new **public** route `/reset` (`web/src/App.tsx` public routes, not protected, so the fragment survives). Reads and clears the fragment, calls preview, shows the warning, a new-password plus confirm form for `kind=password`, a confirmation control, then points to login (password) or SSO login (`vault_only`). When password login is disabled (SSO-only instance), it tells a `kind=password` user to sign in through SSO and enable the vault in Settings with the new password.
- **Tests**: Vitest for both pages (follow `Login.test.tsx`, `AdminUsers.test.tsx`): the token never appears in a request URL, the route is public, the SSO-only hint.

## File map (entry points)

- `api/internal/vault/` (Reset), `api/internal/store/migrations/` and `queries/` (link table, marker, hold reason, Codex account delete), `api/internal/handler/routes_admin.go`, `routes_auth.go`, a new handler file for reset links, `api/internal/handler/admin.go` (`PatchUser` supersession), `api/internal/handler/cli_auth_flow.go` (request denial), `api/internal/workersvc/credential_disabled.go`, `claim_recovery.go`, `chat.go`, `api/cmd/uzi/` run renderers, `web/src/pages/AdminUsers.tsx`, a new reset page, `web/src/App.tsx`, `web/src/lib/apiTypes.ts` plus `fixtures/api-contract/` for any DTO change, docs listed under the milestones.

## Testing decisions

- The destructive core at `vault.Reset` (fake-store unit + store live-DB); security properties at the handler (live-DB); parking at workersvc (live-DB).
- Every bug-class property gets a test watched red then green (`.claude/rules/go.md` mutation discipline).
- All new live-DB tests sit in packages already in `PKGS` of `e2e/run-store-it.sh` (`store`, `handler`, `workersvc`); do not add `api/internal/vault` to `PKGS` (it has no live-DB tests and the script rejects such a package).
- E2E harness: new literals only; still `git grep -F` the touched hold and claim literals across `e2e/` per the repo rule and name any affected phase left unrun in the PR.

## Milestones

### M1: Admin issues a reset link; a password user redeems it and starts with an empty, protected vault

- Blocked by: none.
- Scope: modules 1 and 2 for `kind=password`, Admin → Users action, `/reset` page with the password form and the SSO-only hint, session and CLI-request revocation, docs.
- Acceptance: example 1 except the run-parking part; example 3; module 1 and module 2 tests for the password path; a password user on an SSO-only instance enrolls through `POST /api/vault/enable` from an SSO session after reset (live-DB test). Docs: `docs/auth-design.md` "No password reset" replaced with the admin link; `docs/vault-threat-model.md` and `docs/anthropic-token.md` (Vault choice) describe the reset, what it deletes, the running-run residual, the admin-takeover statements; `docs/oidc.md` break-glass notes the seed admin is excluded; `adr/3507-optional-credential-vault.md` deferral note updated; `task docs:sync` mirror committed; a changelog fragment.

### M2: OIDC-only users redeem a vault-only reset and can create a new passphrase

- Blocked by: M1.
- Scope: `kind=vault_only` issue, preview and redeem, page variant, docs.
- Acceptance: example 2; no password asked, `token_version` bumped; the create-only passphrase endpoint accepts a passphrase after reset and still refuses while a wrapper exists; a linked SSO + password account is `kind=password`.

### M3: A reset user's runs park as `credentials_reset` and resume when a credential exists

- Blocked by: M1.
- Scope: module 3, web and CLI rendering, `ARCHITECTURE.md` run-lifecycle note.
- Acceptance: the run part of example 1; never-reset users unchanged; module 3 tests.

## In-run validation boundary

- `task check:api` plus `go test -race -count=1` on touched packages with focused regression tests watched red then green; `task check:web` plus touched Vitest files; `task docs:sync` + `task check-docs:web`; `task check:changelog-entry`; `task check:migration-numbering`; sqlc regenerate is a no-op (`task check:sqlc-drift`). Full `test:api`, `test:web`, `test:web-browser`, the live-DB sweep, `gate:repo`, `scan:secrets` and `sast:semgrep` stay pending exact-head PR CI.
- The diff must not touch `.github/workflows/**`.

## Decision Log

- **D1. Destructive reset, no rescue.** Per PRD #32. Rejected: rewrapping a DEK cached at reset time (gives the reset initiator a path to the user's credentials).
- **D2. Delete all model credentials, not only DEK-sealed rows.** Predictable "start empty"; whole Codex account rows avoid split live and recovery slots. Rejected: deleting only `sealed_with='dek'` rows.
- **D3. One-time link, not an admin-chosen temporary password.** The admin never learns the new password. Rejected: temporary password.
- **D4. One mechanism for both account kinds.** No fresh-auth proof for OIDC today. Rejected: self-service reset on a plain SSO redirect.
- **D5. Account stays protected after reset under both policies.** Recovery must not silently lower protection. Rejected: dropping to Off under Optional.
- **D6. Running runs are not stopped.** User decision. Rejected: cancelling or fencing active claims.
- **D7. Reuse the `credential_disabled` park with a new hold reason, triggered at claim and scoped by a reset marker.** Gives automatic promotion and a claim-time re-check, touches no `recovery_wait` cause (several carry their own custody, publication or backoff rules) and needs no run updates in the reset transaction. Rejected: rewriting queued and `recovery_wait` runs to a new `recovery_wait` cause in the reset transaction (overwrites causes other code depends on; queued to `recovery_wait` has no path today), and changing missing-credential behaviour for all users (alters existing functionality).
- **D8. No admin re-authentication to issue a link.** User decision; single-use token, issuer re-check at redeem and the audit row are the controls.
- **D9. Token in the URL fragment, POST bodies only.** Keeps it out of access logs and Referer.
- **D10. 1 hour TTL; issuing supersedes, under a users-row lock.** Re-issuing reliably kills a link sent to the wrong place.
- **D11. Redemption does not log in.** The existing create-on-unlock path enrolls the new vault.
- **D12. CLI tokens and product OAuth grants survive; approved, unconsumed CLI login requests are denied.** Tokens and grants cannot open the vault; an approved request would mint a fresh token after the reset. A pending request has no user until approved and stays pending, but the old sessions cannot approve it (Correction 4).
- **D13. Seed admin and self excluded.** The seed admin's password is operator config and boot unlock fails on a mismatch; self-issue would let any holder of an admin session set a new password without the old one.
- **D14. `vault.Reset` owns the transaction.** Every lifecycle operation takes the in-process mutex before its transaction; a caller-opened transaction would invert that order and could deadlock with `Unlock`.
- **D15. Issuer authority re-checked at redeem; admin deactivation or demotion supersedes links.** A demoted admin must not keep pre-issued takeover links.
- **D16. Codex runs are not parked (owner decision 2026-11-06).** The PRD #1147 write-once binding freeze (`FreezeRunCodexBinding`) stays. A Codex run created before the reset whose frozen alias is gone fails terminally with "the bound Codex login is unavailable after your account reset; start a new run". Rejected: relaxing the freeze, and holds that could never be released.
- **D17. M2: the passphrase is set from the app-wide banner, not Settings; a `vault_only` link grants no access, so takeover warnings are scoped to `password` links.**
- **D18. M3 as built.** The `credentials_reset` park keys on the narrow `errNoModelCredential` sentinel (no Anthropic token), never on `errCredentialUnavailable` as a whole, so forge-origin refusal, repo-owner mismatch and an undecryptable token stay terminal. Judge, job and cross_check runs are excluded explicitly. The promoter releases the hold when the harness resolver finds an enabled credential for the run's harness, not through `credentialRequirementMet` (every binding is NULL after a reset, so that rule would promote and re-park every pass). Owner Resume and `run set-token` on the hold are refused with 409 before any write. Accepted residual (with D6): a Claude claim whose token was already opened when the reset committed is delivered on that token. Deferred: stamping the marker with `clock_timestamp()` at the end of the reset transaction (a run created during the reset window fails as today instead of parking).
- **Correction 1 (module 3, schema).** `runs.hold_reason` has no CHECK constraint: `00215_completion_hold.sql` adds it as plain text and `00328_provider_park_streak.sql` notes "free text, so no CHECK". The CHECK in `00391_publication_resume.sql` belongs to `run_publication_resume`. So there is no CHECK change; only the promoter index is widened.
- **Correction 2 (module 3, sentinel).** The Codex "no alias" refusal is `ErrCodexRunNotBound` (`codexauthz.go`), also raised at lock time by `classifyLockedCodexClaim` in `claim_recovery.go`.
- **Correction 3 (module 2, revocation).** Demotion happens only via `oidcSyncAdmin` -> `SetUserAdmin`, not `PatchUser`. Supersession is attached to the activation and admin writes themselves, as a separate statement after the users update in the same transaction.
- **Correction 4 (D12 amended).** Pending CLI login requests have no `user_id` until approved, so redeem denies only approved, unconsumed requests. A pending request stays pending, but the old sessions cannot approve it, because CLI approve re-checks `token_version` under a users-row share lock.
- **Correction 5 (lock order).** A CLI poll locks the request's user row (KEY SHARE) before claiming, so poll and redeem lock users before `cli_auth_requests`.
