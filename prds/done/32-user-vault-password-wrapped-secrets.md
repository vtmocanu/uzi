# PRD #32: Per-User Vault — Password-Wrapped Secrets

**GitLab Issue**: [vtmocanu/uzi#32](https://gitlab.example.com/vtmocanu/uzi/-/issues/32)
**Status**: Complete (2026-07-10, merged via [MR !35](https://gitlab.example.com/vtmocanu/uzi/-/merge_requests/35))
**Priority**: High
**Created**: 2026-07-10
**Depends on**: PRD #3 (user secrets + secretbox, done); PRD #19 (autopilot, done — this PRD gates its claims).

## Problem

A user's Anthropic subscription token is sealed with AES-256-GCM (`api/internal/secretbox`) under one master key, `UZI_SECRET_KEY`, loaded from an env var. In the k8s deployment that key is visible to anyone with cluster access: `kubectl describe pod` / exec / reading the Secret, the Infisical project that feeds it, or etcd. Same people can read the DB. Env + DB = every user's personal Anthropic token in plaintext. etcd encryption at rest and Infisical do not change the threat model — the same operators administer those too.

Goal (stated by the user, 2026-07-10): make it materially harder — the decryption key for a user's secrets must **never exist at rest anywhere an operator can read** (env, Infisical, etcd, DB, disk). Perfection is explicitly not the goal: an admin who memory-dumps a live pod or ships a trojaned image can still win; those attacks are active, noisy, and auditable, unlike today's passive read.

## Solution Overview

The Bitwarden model, scoped to per-user secrets:

- Each user gets a random 32-byte **DEK** (data encryption key). Their secrets (`user_secrets` rows — today `anthropic_token`) are sealed with the DEK via the existing `secretbox.Box` construction.
- The DEK is stored **only wrapped**: `wrap = secretbox(KEK, DEK)`, where **KEK = Argon2id(login password, per-user vault salt)**. The KEK is derived at login, used to unwrap the DEK, and discarded. Neither KEK nor plaintext DEK is ever written anywhere.
- The unwrapped DEK lives in an in-memory cache in the API process ("the vault is **unlocked**"), from login until pod restart or an explicit **Lock vault** action. While unlocked, agents/autopilot for that user work normally, including overnight — the key lives in the server, not the browser session.
- While **locked** (after a deploy/restart, or manual lock), the user's runs stay queued as "waiting for vault unlock" instead of claiming or failing; the UI shows a 🔒 badge and a re-enter-password unlock prompt (no full re-login needed — the JWT cookie survives restarts, the DEK cache does not).

**Scope boundary — forge bot PAT stays under the master key.** The poller must sync issues 24/7 with no user present; the connection PAT (`forge_connections.token_ciphertext`, opened in `forgesvc` and `workersvc.assembleClaim`) cannot be password-wrapped without breaking sync. `UZI_SECRET_KEY` therefore **remains**, but only for connection-level secrets. Residual risk accepted and documented: an operator can still recover the bot PAT (a Developer-role bot on protected repos, blast radius bounded by PRD #5's least-privilege checks) — but no longer any user's personal Anthropic token.

### Why not the alternatives (Decision Log)

1. **Key as mounted file / fetched from Infisical at boot** — rejected: same operators read Infisical; exec into pod reads the file.
2. **Vault transit / KMS envelope encryption** — rejected for this deployment: cluster operators also administer Vault/Infisical, so it only adds audit, not confidentiality.
3. **Global manual unseal at boot (Vault-style, Shamir shards)** — viable, but user chose per-user unlock: no ops ceremony on every deploy, per-user granularity, and the unlocker is the secret's owner. A global unseal can be layered later for connection PATs if wanted.
4. **Per-run short-lived Anthropic access tokens** (worker never sees a long-lived credential) — out of scope here; depends on what the Anthropic OAuth flow permits. Noted as a future PRD; composes with this one.

### Inspiration check

bottega stores tokens plaintext-equivalent (host ambient creds); multica stores plaintext creds; dot-agent-deck delegates to ambient credentials. None does password-wrapped envelope encryption. Prior art is Bitwarden/1Password key hierarchy (master-password-derived KEK → DEK → data), which this follows.

## Technical Design

### Key hierarchy & crypto

- `DEK`: 32 random bytes (`crypto/rand`), per user, generated on vault creation.
- `KEK = Argon2id(password, vault_salt, params)` — reuse the cost params from `api/internal/auth/argon2.go`, but:
  - **`vault_salt` is a fresh random 16-byte salt, independent of the auth-hash salt.** This is load-bearing: the auth flow **stores** its Argon2 output in `users.password_hash`. If the KEK used the same salt+params, the DB itself would contain the KEK. Distinct salt ⇒ distinct output; the KEK derivation result is never persisted.
  - Domain-separate for safety: derive with `Argon2id(password, vault_salt)` and feed through HKDF or simply keep params/salt distinct — implementer's choice, documented in code.
- `wrapped_dek = secretbox.Seal_KEK(DEK)` — reuse `secretbox.Box` keyed by the KEK (GCM gives integrity: a wrong password fails authentication cleanly, which is also the unlock-verification mechanism).
- User secrets: `ciphertext = secretbox.Seal_DEK(secret)` — same construction as today, different key.

### Storage (migration, draft `00044` — renumber to the live head at merge per repo convention)

```sql
CREATE TABLE user_vaults (
    user_id     UUID PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    kek_salt    BYTEA NOT NULL,          -- 16 bytes, random, never reused
    wrapped_dek BYTEA NOT NULL,          -- secretbox(KEK, DEK)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE user_secrets ADD COLUMN sealed_with TEXT NOT NULL DEFAULT 'master'
    CHECK (sealed_with IN ('master', 'dek'));
```

**Lazy migration of existing rows — with honest limits.** Existing `user_secrets.ciphertext` is master-key-sealed and cannot be rewrapped without the user's password. On each successful unlock, any of that user's rows still marked `sealed_with = 'master'` are opened with the master box, resealed with the DEK, and flipped to `'dek'` in one transaction. Two truths the UI and docs must not paper over (audit finding, 2026-07-10):

- **Rewrap does not un-leak.** An operator could have snapshotted the DB before the rewrap; a token that ever existed master-sealed must be treated as potentially compromised. Real protection for existing users comes from **rotating the token**, not rewrapping it: after the first post-deploy unlock, the UI shows a one-time notice "Your token is now protected by your password. For full protection, rotate it (it was previously recoverable by operators) and re-save." Rewrap still happens (better at-rest posture going forward), but the claim is "protected from now on", never "protected retroactively".
- **Legacy `'master'` rows don't need the DEK to open.** Until rewrap, the claim gate (in-memory unlocked check) is the only thing withholding them while locked — a runtime control, not a cryptographic one.

Dormant accounts never rewrap; add an admin-visible count of still-`master`-sealed rows (Settings → admin) so the operator can see migration progress. New saves always seal with the DEK.

### New package `api/internal/vault`

```go
type Vault struct { /* master box for lazy rewrap, store, in-memory cache */ }

// Unlock derives the KEK from password, unwraps (or on first unlock creates)
// the DEK, caches it, and lazily rewraps master-sealed secrets.
// GCM auth failure ⇒ wrong password ⇒ ErrWrongPassword (no oracle beyond
// what login already is; endpoint sits behind the auth rate limiter).
func (v *Vault) Unlock(ctx context.Context, userID uuid.UUID, password string) error
func (v *Vault) Lock(userID uuid.UUID)                    // wipe from cache (best-effort zeroize)
func (v *Vault) Unlocked(userID uuid.UUID) bool           // gate before ClaimRun
func (v *Vault) Seal(userID uuid.UUID, plaintext []byte) ([]byte, error)   // ErrLocked when locked
func (v *Vault) Open(userID uuid.UUID, ciphertext []byte, sealedWith string) ([]byte, error)  // ErrLocked
```

Required sqlc query changes (M1, and a hard dependency of M3 — enumerate so phases don't stall): `GetUserSecretCiphertext` must also return `sealed_with` (vault.Open needs it); `UpsertUserSecret` writes `sealed_with`; new `RewrapUserSecret` update (ciphertext + flip to `'dek'`) for the lazy migration; new `user_vaults` CRUD.

Optional defense-in-depth (cheap, do it in M1): add Seal/Open variants taking AAD and bind `user_id || kind` — today `secretbox` passes nil AAD, so a DB-*write* operator could swap rows between users. Out of the passive threat model (a swap just fails GCM or hands user A user B's token, not disclosure to the operator), but binding is a few lines.

Cache: `map[uuid.UUID][]byte` + `sync.RWMutex`. DEKs never logged (the existing redactor discipline applies), never serialized, wiped on Lock. Best-effort `for i := range k { k[i] = 0 }` on eviction — Go gives no guarantees, stated in a comment, not oversold.

### Wire-in points

- **Login** (`handler/auth.go` `Login`): after `VerifyPassword` succeeds, call `vault.Unlock` with the same plaintext password before it goes out of scope. First-ever unlock for a user with no `user_vaults` row creates DEK + wrap. Unlock failure after successful login should be impossible (same password) except for a corrupted row — log loudly, return the session anyway with vault locked. Note: login now runs Argon2id twice (verify + KEK, 2×19 MiB) — keep the login rate limiter strict.
- **Register** (`handler/auth.go` `Register`): create the vault row at registration (we hold the password). **Derive the KEK before the transaction**, like `password_hash` already is (auth.go:129) — registration holds a `pg_advisory_xact_lock` (auth.go:175) and a second Argon2id inside it would serialize all registrations. Only the cheap row insert goes in the tx (atomic with user creation); a crash between the two is recovered by Unlock's create-on-first-login path anyway.
- **New endpoints** (inside `RequireAuth` — so unlock is not a pre-auth oracle and CSRF applies; rate-limit unlock with the **per-user** limiter (`middleware.PerUserMiddleware`), not the per-IP auth limiter, since it's authenticated and a stolen JWT would otherwise make it an online password-guessing oracle sharing a NAT bucket with other users' logins):
  - `POST /api/vault/unlock` `{password}` → 204 / 403 wrong password
  - `POST /api/vault/lock` → 204
  - `GET /api/vault/status` → `{"unlocked": bool}` (or fold into the existing `/api/me` payload — implementer's choice, keep it one round-trip for the SPA shell)
- **Secrets save** (`handler/secrets.go`): seal with `vault.Seal(userID, ...)`; 409 + `vault_locked` error code when locked (only reachable if the pod restarted mid-session).
- **Claim path** (`workersvc`): claims are already single-user — `ClaimRun` is scoped `r.user_id = @user_id` from the worker's own identity (service.go:252-256, runtime.sql:157) — so **no SQL change**: a one-line Go gate `if !s.vault.Unlocked(wkr.UserID) { return nil, nil /* idle */ }` before `ClaimRun` keeps locked-owner runs `queued`. In `assembleClaim` (service.go:314-327) the anthropic secret opens via `vault.Open(run.UserID, ...)`. **The lock race (lock lands after `ClaimRun`, before `assembleClaim`) must NOT reuse `errCredentialUnavailable`** — that path is terminal (`MarkRunFailedByID`, service.go:268-276), which would fail the run and violate success criteria 3 and 5. Add a distinct `errVaultLocked` sentinel handled by resetting the just-claimed run back to `queued` (mirror `SweepClaimedNeverStarted`, runtime.sql:272-276; new query) and reporting idle. Bot PAT decryption (service.go:308) is unchanged (master box).
  - Wire the vault into `workersvc` via the existing optional-dependency seam (`SetBroadcaster`/`SetLifecycle`, service.go:184-189): add `SetVault(*vault.Vault)`, called additively from `main.go` — keeps M3's files disjoint from M2's `main.go`/constructor edits.
- **Autopilot / poller**: no special code — autopilot only labels/queues; the claim gate above is the single enforcement point. Sweeper is safe **as verified 2026-07-10**: no sweep query touches `status='queued'` (`SweepClaimedNeverStarted` keys `claimed`, `SweepRunningTimeout` keys `running`, stale-worker sweeps key claimed/running/awaiting_approval — runtime.sql:272-305); there is no queued-age timeout, so locked-owner runs sit indefinitely by design.
- **Resume affinity**: a resumed run re-enters claim → same gate applies; a locked owner's resumable run waits, it does not fail.
- **Seed path** (`seed.AnthropicToken`, `api/internal/seed/anthropic.go`): boot holds the seed admin's plaintext password (`loadSeedAdmin`), so seeding creates the vault row, seals the seeded token with the DEK, **and populates the DEK cache (unlocks the seed admin at boot)** — otherwise a fresh headless deployment would sit locked until the first interactive login, defeating overnight autopilot for exactly the bootstrap case. The `Sealer` interface grows into a small vault-aware variant.
  - **The seed admin is explicitly exempt from this PRD's protection** (audit finding): `UZI_SEED_PASSWORD` and `UZI_SEED_ANTHROPIC_TOKEN` are env vars — an operator reads the token directly, or derives the KEK from the seeded password. The vault is only as strong as what's in env. Docs must say: for real protection, after first boot the seed admin changes their password (rewraps the DEK, once the change endpoint exists — until then: re-register/rotate), rotates the token, and the operator removes `UZI_SEED_*` from the deployed env.

### Web UI

- **Vault badge** in the header (next to the user menu): 🔓 unlocked / 🔒 locked, with tooltip. State rides on `/api/me` (`sessionPayload`, auth.go:280). **There is no global WS to hook** — the only socket is per-run (`/api/ws?run=<id>`) — so refresh via `AuthContext.refresh()` (AuthContext.tsx:83) on window focus, on any 409 `vault_locked` API response, and after unlock/lock calls.
- **Locked banner** when authenticated-but-locked: "Vault locked — enter your password to resume agents" → password field → `POST /api/vault/unlock`. Full re-login also unlocks (login handler does it) — the banner is the cheaper path.
- **Lock vault** action in the user/settings menu → `POST /api/vault/lock`, badge flips, confirmation toast explaining runs will queue.
- **Board/runs**: `queued` runs whose owner is locked render as *waiting for vault unlock* (distinguish via the run owner's vault status — the current user's own runs are the only ones whose state matters to them; an `unlocked` boolean on the run list payload for own runs, or derive client-side from vault status).
- **Token save notice** (Settings → Secrets): "Encrypted with your login password. If you forget your password, this token cannot be recovered and must be re-entered."

### Password change / reset

No password-change or reset endpoint exists today (`handler/auth.go` has register/login only). Design constraints recorded for when they land:

- **Change** (user present, old password known): unwrap DEK with old-password KEK (or take it from the cache — user is unlocked), rewrap with new-password KEK, update `user_vaults` in the same transaction as `users.password_hash`. Transparent to the user.
- **Reset** (password lost): DEK unrecoverable **by design**. Reset must delete `user_vaults` + all `sealed_with='dek'` rows for the user and tell them to re-enter tokens. An admin reset that silently kept unreadable ciphertext would be a worse bug than the data loss.

### Explicitly accepted residual risks (document in `docs/`, threat-model section)

1. **`users.password_hash` is an offline brute-force oracle — the dominant residual.** An operator with the DB can crack the login password against the stored Argon2id hash; recovering it yields KEK → DEK → tokens. The vault's real strength is password entropy + Argon2 cost (t=2, 19 MiB), not the 256-bit DEK, and `MinPasswordLen` is 12. Mitigations: document loudly; consider a higher Argon2 cost for the KEK derivation than the login hash, and a higher password floor for vault-protected deployments.
2. **The seed admin is exempt** — token and password sit in env (`UZI_SEED_*`); see Seed path for the post-boot hardening steps.
3. **Tokens that ever existed master-sealed are potentially already leaked** — rewrap protects going forward only; rotation is the real fix (see Lazy migration).
4. Memory dump of a live pod (`kubectl debug` + `SYS_PTRACE` on `/proc/1/mem`) captures cached DEKs and in-flight plaintext. With until-restart caching and overnight autopilot, DEK-in-RAM is the *common* state, not a corner case — an optional idle-timeout auto-lock is a documented future knob (off by default; it would break overnight runs). Environmental mitigations: admission policy blocking ephemeral/privileged containers + audit alerts on `pods/exec` / `pods/ephemeralcontainers`.
5. A trojaned uzi image logs passwords or tokens at next use. Mitigation: GitOps drift detection + image signing (environmental).
6. The worker receives the plaintext Anthropic token at claim (`claim.go` `anthropic_oauth_token`) and holds it for the run's duration — unchanged by this PRD; short-lived per-run tokens are the future-PRD answer.
7. Bot PAT remains master-key-sealed (see Scope boundary).
8. Multi-replica API: the DEK cache is per-process; uzi runs single-replica today (compose; no k8s manifest exists in-repo yet — when one lands, pin the API to `replicas: 1` or accept per-pod unlock). Do **not** replicate DEKs across pods.
9. **One-time rollout stall**: on the deploy that ships this, every existing user's runs stop claiming until they next log in (tokens still master-sealed but the gate requires an unlocked vault). Call out in release notes/ops doc.

### Out of scope

- Vaulting the forge connection PAT (needs global unseal or a service-ownership model).
- SSO/OIDC (no password to derive from — would need a separate master passphrase, Bitwarden-style).
- Shamir-sharded global unseal, HSMs, per-run short-lived Anthropic tokens.
- Password change/reset endpoints themselves (constraints recorded above).

## Milestones

- [ ] **M1: vault core** — `api/internal/vault` package + migration + sqlc queries, unit-tested (wrap/unwrap roundtrip, wrong password fails GCM auth, master key cannot open DEK-sealed rows)
- [ ] **M2: auth + endpoints wire-in** — unlock at login/register, lazy rewrap, vault endpoints, secrets-save via vault, seed path; `go test ./...` green
- [ ] **M3: claim gating** — locked-owner runs never claim, unlock resumes claiming, lock-race requeues (never fails) a run, resume path safe
- [ ] **M4: web UI** — vault badge, locked banner + unlock, lock action, "waiting for vault unlock" run state, irrecoverability notice on token save; typecheck + vitest green
- [ ] **M5: integration + docs + specs** — e2e lock/unlock/restart scenarios, threat-model doc, `specs/ai.md` updated (`specs/human.md` only with user approval)

Phase layout per the parallel-analysis convention (files disjoint within a phase):

| Phase | Milestone | Depends on | Files touched |
|---|---|---|---|
| 1 | **M1: vault core** — `api/internal/vault` (KDF w/ dedicated salt, wrap/unwrap, cache, errors, AAD variant), migration `00044` (draft), sqlc queries: `user_vaults` CRUD, `GetUserSecretCiphertext`+`sealed_with`, `UpsertUserSecret`+`sealed_with`, `RewrapUserSecret`, requeue-claimed-run | — | `api/internal/vault/*`, `api/internal/store/{migrations,queries}` |
| 1 | **M4: web UI against the contract** — badge, locked banner + unlock call, lock action, run-state rendering, save/rotate-notice copy (API mocked) | — (contract in this PRD) | `web/src/*` |
| 2 | **M2: auth + endpoints wire-in** — login/register unlock, lazy rewrap, `/api/vault/{unlock,lock,status}` + per-user limiter, `/api/me` vault status, secrets-save via vault, seed path incl. boot unlock, `main.go` `SetVault` call | M1 | `api/internal/handler/{auth,secrets,handler}.go`, `api/internal/seed/anthropic.go`, `api/cmd/server/main.go` |
| 2 | **M3: claim gating** — `Unlocked` gate before `ClaimRun`, `assembleClaim` via vault, `errVaultLocked` → back-to-queued (never failed), `SetVault` seam, resume path | M1 | `api/internal/workersvc/*` (no shared query file) |
| 3 | **M5: integration + docs + specs** — e2e (lock → run queues → unlock → run claims; restart → locked → unlock → lazy rewrap flips `sealed_with`), threat-model doc, `specs/ai.md` (auto), `specs/human.md` (user approval required) | M2, M3, M4 | `e2e/*`, `docs/*`, `specs/*` |

## Success criteria

1. With a DB dump + every env var + Infisical contents, an operator cannot recover any user's Anthropic token whose row was **born** `sealed_with='dek'` — **except the seed admin's** (derivable from `UZI_SEED_PASSWORD`; exempt by design) and modulo offline password cracking (residual #1). Test: decrypt attempt with the master key fails GCM auth.
2. `UZI_SECRET_KEY` decrypts only connection PATs and legacy not-yet-rewrapped rows.
3. Restart the API → all vaults locked → queued/new runs for a user do not claim; unlock → they claim within one poll cycle; the user's `sealed_with` flips to `dek` on that unlock.
4. Wrong password on `/api/vault/unlock` → 403, rate-limited like login, no state change.
5. Lock vault mid-run → in-flight run completes; next run queues.
6. `./e2e/run-e2e.sh`, `go test ./...`, `npm test`, `npm run typecheck` green.
