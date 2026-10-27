---
title: Vault threat model
order: 40
audience: operator
---

# Vault threat model (per-user password-wrapped secrets)

The per-user vault (PRD #32) exists to make one class of attack materially
harder for **protected** users: a **passive operator read** of personal
Anthropic, OpenAI and Codex credentials. Before
the vault, every user's token was sealed under one master key
(`UZI_SECRET_KEY`) that lives in the API's environment — so anyone who could read
both the env (`kubectl describe`/exec, the Secret, Infisical, etcd) and the DB
could decrypt those tokens. Protected mode removes the personal credential's
DEK from persistent storage in plaintext. **Unprotected (Off)** mode deliberately
uses server-key sealing instead: DB plus server-key theft exposes those values,
as with forge bot tokens. A compromised running server can read credentials
while a protected vault is unlocked.

Design rationale lives in the PRD
([../prds/done/32-user-vault-password-wrapped-secrets.md](../prds/done/32-user-vault-password-wrapped-secrets.md));
this page is the as-built residual-risk list and the operator hardening steps.
[ADR-3507](../adr/3507-optional-credential-vault.md) records optional protection,
conversion and compatibility boundaries; users should start at
[Vault choice](./anthropic-token.md#vault-choice).

## What the vault protects — and what it does not

- **Protected mode**: the user's `user_secrets` rows — three kinds today
  (`anthropic_token`, `openai_api_key`, `codex_auth`; the latter two share one
  default slot per user, separate from the Anthropic token's own default) —
  plus the per-account Codex login material in `codex_provider_account`. They
  are sealed with a per-user 256-bit DEK under normal operation. The DEK is
  stored only wrapped: `secretbox(KEK, DEK)` where `KEK = Argon2id(login
  password, per-user salt)`. The KEK is derived at unlock, used once, and
  discarded; the plaintext DEK lives only in the API process's memory while
  the vault is unlocked, and is gone on lock or restart. Neither the KEK nor
  the plaintext DEK is ever written anywhere. **One narrow exception**: a
  Codex account's *recovery* slot (a successful provider result retained while
  its live commit cannot finish) is sealed under `UZI_SECRET_KEY` with the
  owner/kind-bound `master_aad` marker instead, but only when the owner's vault
  is locked at the exact moment a refresh needs to protect newly-rotated material — never the live,
  in-use login. A later reconcile pass re-seals that recovery copy under the
  user's DEK once the vault unlocks. This same protected login is what the
  Codex account-meter poller (PRD #1209 — see
  [rate-limits.md](rate-limits.md#codex-account-limits)) opens to read usage;
  it hands back only a sanitized reading and never the token itself, so it
  adds no new decrypted-secret surface beyond the one already covered here.
- **Not protected (by design)**: the forge **bot PAT**
  (`forge_connections.token_ciphertext`). The poller syncs issues 24/7 with no
  user present, so its credential cannot be password-wrapped. It stays sealed
  under `UZI_SECRET_KEY`, which therefore remains — for connection-level secrets
  and unprotected personal credentials. An operator can still recover the bot
  PAT; its blast radius is bounded by
  PRD #5's least-privilege checks (a Developer-role bot on protected repos).

## OIDC-only users: the vault passphrase (PRD #45)

An OIDC-only user (JIT-provisioned or linked via SSO with no password ever
set) has no login password for the KEK above to derive from. When protected
or explicitly enabling protection, this user sets a dedicated **vault
passphrase** through a create-only endpoint,
running the exact same Argon2id derivation path (`api/internal/vault/vault.go`)
and enforcing 12–1024 UTF-8 bytes — the KEK's
real strength (residual risk 1, below) is unchanged by which secret feeds it.
The DEK hierarchy underneath is identical; the vault has no notion of whether
the string it was handed at creation was a password or a passphrase. Changing
or recovering a lost passphrase is deferred (deleting and re-creating the
vault would lose already-sealed secrets) — a documented limitation, not an
oversight. See [auth-design.md](auth-design.md#oidc-single-sign-on) for how
this fits into the OIDC login flow. Off users need no passphrase or wrapper.
Linking SSO to an existing password account retains its mode and password
semantics.

## Residual risks (accepted)

1. **`users.password_hash` is an offline brute-force oracle — the dominant
   residual.** An operator with the DB can crack the login password against the
   stored Argon2id hash; recovering it yields KEK → DEK → tokens. The vault's real
   strength is therefore **password entropy + Argon2 cost**, not the 256-bit DEK.
   As shipped, the KEK derivation uses the same Argon2id cost as the login hash
   (t=2, 19 MiB, p=1) and the password floor is 12 characters. Raising the KEK
   cost for a vault-protected deployment is a one-line change in
   `api/internal/vault/vault.go` (the `kek*` consts); the no-vault unlock
   timing-equalization burn derives with those same params, so it tracks any bump
   automatically. Consider also raising the password floor.
2. **The seed admin is exempt.** `UZI_SEED_PASSWORD` and
   `UZI_SEED_ANTHROPIC_TOKEN` are environment variables: an operator reads the
   token directly or derives the KEK from the seeded password. A **protected**
   seed admin is **boot-unlocked** when the seed password remains configured,
   keeping its DEK in memory. An Off seed admin skips boot unlock and is not
   implicitly enrolled. See the hardening steps below.
3. **A credential that ever existed master-sealed may remain exposed in backups.**
   This includes values stored Off, legacy material and the protected Codex
   recovery exception. Enabling protection or lazy rewrap improves current
   storage but cannot erase earlier DB snapshots readable with the server key.
   **Rotate credentials** to address that exposure; enabling displays rotation
   advice. The admin migration count reports remaining master-sealed secrets.
4. **A live-pod memory dump captures cached DEKs and in-flight plaintext.** With
   until-restart caching and overnight autopilot, DEK-in-RAM is the *common* state.
   `kubectl debug` + `SYS_PTRACE` on `/proc/1/mem` wins. This is an active, noisy,
   auditable attack (unlike the passive read the vault closes). Environmental
   mitigations: admission policy blocking ephemeral/privileged containers, and
   audit alerts on `pods/exec` and `pods/ephemeralcontainers`. An optional
   idle-timeout auto-lock is a possible future knob (off by default — it would
   break overnight runs).
5. **A trojaned uzi image** that logs passwords or tokens at next use defeats
   everything. Mitigations are environmental: GitOps drift detection and image
   signing.
6. **The worker holds the plaintext token for a run's duration** (delivered in the
   claim payload). Unchanged by this PRD; short-lived per-run tokens are a future
   PRD.
7. **The DEK cache is per-process.** A singleton API with coordinated
   **Recreate** replacement is a required compatibility boundary. Mixed-version
   API operation is unsupported; independently unlocking replicas is not a
   supported alternative. Do not replicate DEKs across pods. The chart already
   enforces this deployment shape; see [ADR-3507](../adr/3507-optional-credential-vault.md).
8. **The AAD no longer pins a ciphertext to a *row*, only to `(user, kind)`
   (PRD #104).** Each sealed secret is bound to `user_id || 0x00 || kind` as
   additional authenticated data (the raw 16-byte UUID, one zero byte, then
   the kind bytes), for both DEK and new `master_aad` personal material. Legacy
   `master` remains readable with nil AAD; unknown markers fail closed. Thus a
   DB-write operator who moves a ciphertext
   between users or between kinds gets a GCM authentication failure rather than a
   working key. That guarantee is intact. What changed is that a user may now hold
   several `anthropic_token` rows, and all of them share one AAD — so swapping two
   of the *same user's own* tokens between rows authenticates cleanly. The
   consequence is a mislabeled credential, not disclosure: nothing is decrypted
   that the operator could not already decrypt, and the swap is confined to one
   account. Accepted deliberately: widening the AAD to include `user_secrets.id`
   would be strictly better, but it is not backward compatible — every existing
   ciphertext was sealed without the id and would stop opening — so it needs a
   re-seal migration of its own. This residual sits inside the DB-write threat
   model, which is already outside the passive-read model the vault was built for.

## Hosted worker join tokens (PRD #58)

[Hosted workers](hosted-workers.md) (k8s only) introduce a second secret this
vault does not touch: the worker's own join token. Two residuals, both
accepted and bounded rather than closed:

1. **The token is plaintext in etcd for the worker's lifetime.** The
   controller delivers it as a file-mounted k8s Secret (never an env var —
   the same `/proc/<pid>/environ` leak class [proc-hardening.md](proc-hardening.md)
   closes for the worker's own credentials), and a k8s Secret is
   base64-encoded, not encrypted, at that layer by default — even with etcd
   encryption-at-rest enabled, protection needs an encrypting provider
   configured for the `secrets` resource specifically (an
   `EncryptionConfiguration` with the resource merely listed is not enough),
   and it is not retroactive: a Secret written before that provider was
   configured stays plaintext in etcd until something rewrites it, so a
   worker join token already delivered is not automatically protected just
   because the cluster operator turns on encryption-at-rest later. Anyone
   who can read it can
   impersonate that worker: claim its owner's runs, and receive their
   decrypted forge PAT and Anthropic token in the claim response. Bounded by
   the worker namespace holding nothing else and the controller's own RBAC
   being create/delete-only on Secrets (it never reads one back, so a
   controller compromise cannot harvest the whole fleet's tokens in one
   call). Per-worker rotation is a future PRD.
2. **While a token is pending delivery, its sealed copy sits in Postgres
   under `UZI_SECRET_KEY`** — the very key this vault exists to stop relying
   on for the Anthropic token. This is temporary: the api destroys the
   sealed copy the moment the worker actually authenticates with it, proving
   delivery. `WORKER_HOSTING_PENDING_TOKEN_TTL` (default 1h) bounds the
   worst case — a controller that never picks the token up (chart not
   deployed, controller down, hosting disabled mid-flight) — by expiring the
   buffer instead of leaving it at rest indefinitely. The expiry sweep runs
   regardless of whether hosting is currently enabled, since a stack that
   provisioned workers and then turned hosting off is exactly the case that
   would otherwise strand ciphertext.

See `ARCHITECTURE.md`'s [worker controller](../ARCHITECTURE.md#worker-controller-k8s-only)
section for the RBAC this leans on, and the PRD's Decision 3 for the full
reasoning.

## Operator hardening

For the seed admin, after first boot:

1. Confirm **Protect my credentials with my password (vault)** is On in
   Settings; enable it if the account is Off, then confirm the protected vault
   is unlocked. Fresh seed accounts start Off, so re-saving a token alone does
   not add password protection. Use a strong account password: this feature
   adds no change-password endpoint, and re-registering does not rewrap an
   existing account's DEK. Protected accounts unlock with their current password.
2. Rotate `UZI_SEED_ANTHROPIC_TOKEN` (it was readable from the env) and save the
   replacement only after confirming protected/unlocked status. That save is
   DEK-sealed under the protected vault. If the account remains Off, the
   replacement remains server-key-sealed and does not meet this hardening
   objective. See [Vault choice](anthropic-token.md#vault-choice).
3. Once the account and credentials are self-sufficient, remove `UZI_SEED_*`
   (including `UZI_SEED_PASSWORD` and `UZI_SEED_ANTHROPIC_TOKEN`) from the
   deployed environment. This stops seed boot-unlock; a protected account
   then needs an interactive unlock after a restart.

## Rollout note

The original PRD #32 rollout required existing users to unlock before work
could claim. Optional protection preserves that behavior for protected users;
Off users need no unlock after restart and receive no vault-lock notice.

Migration 00358 sets existing users to protected, including wrapperless users,
without migrating ciphertext. If any users exist, it seeds Required policy;
an empty database gets Optional before seed-admin creation. New enrollment
chooses an explicit mode under current policy; login, seed boot unlock and
ordinary unlock do not enroll Off users. Admin policy changes neither convert
nor re-seal existing material. Required leaves existing Off users runnable with
a persistent enrollment prompt.

Enabling/disabling atomically converts personal secrets and both live/recovery
Codex account slots. Limits are **10,000 material rows, 64 MiB ciphertext per
user**, with **64-row/1 MiB batches** and a **two-minute lifecycle deadline**.
Recovery slots count separately; an account's live/recovery pair must fit
1 MiB together. Disabled secrets and unused/orphan accounts count. Resource-limit
refusal reports measured totals and limits and leaves material and mode
unchanged. Use existing credential-management removal controls before retrying;
removing aliases need not free canonical accounts. Orphan-account remediation,
including a dedicated deletion API, index or UI, remains deferred; there is no
orphan cleanup workflow to follow.

A confirmed abort rolls back the conversion. A lost commit acknowledgment is
**uncertain**, not proof that the database is unchanged: the unpublished key
is discarded, reconciliation uses a fresh connection and applicable locks,
and the user must refresh status and unlock if protected. Do not compensate
by deleting a wrapper.

Keep the API singleton and replace it with Recreate; do not run mixed API
versions. Additive columns use constant defaults to avoid user/ciphertext
rewrites, but ALTER TABLE locks and immediate CHECK validation scans have
production-dependent duration. Plan maintenance and backups; no production
size or migration-duration claim has been established. Down migration refuses
any unprotected user or `master_aad` in secrets or either account slot. Use
the **new binary** to convert safely before down; simply starting an older
binary is unsafe because it rejects `master_aad`. See
[ADR-3507](../adr/3507-optional-credential-vault.md) for the protocol and measured
fixture evidence.

## Vault-lock Slack notice (PRD #890)

A restart drops protected users' cached DEKs (see residual #7), causing the
stall this section describes — not just
the one-time first-vault-rollout, but any later restart too. A boot
reconciler now closes the "nothing tells the user" gap directly: for each
protected, vault-having, Slack-linked user whose locked vault is blocking `queued`/
`awaiting_approval`/`awaiting_input` work or a due schedule, it DMs them to
unlock, once per lock-episode — deduped on `user_vaults.lock_notified_at`
(atomic across replicas) and cleared on their next successful unlock. It is
gated by `UZI_VAULT_LOCK_NOTICE_ENABLED` (default on) and the same per-user
`slack_notify` gate every other DM uses.

**Nothing new is leaked.** The DM goes only to the owner's own confirmed
Slack DM, and the owner already sees their own lock state in-app (the
`VaultLockedBanner` and `GET /api/vault/status` / `/api/me`) — lock/unlock
state is not a protected secret; the indistinguishability requirement above
is about an unauthenticated or other-user probe, not about telling the
authenticated owner their own state. The DM body is a fixed, cause-neutral
title and text plus an in-app deep link — never the password, KEK, or DEK.

**Residual #7 interaction.** Lock state is per-process; the singleton/Recreate
requirement avoids cross-pod cache and notice inconsistencies. Off users have
no wrapper or lock-notice episode.
