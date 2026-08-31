---
title: Vault threat model
order: 40
audience: operator
---

# Vault threat model (per-user password-wrapped secrets)

The per-user vault (PRD #32) exists to make one class of attack materially
harder: a **passive operator read** of a user's personal Anthropic token. Before
the vault, every user's token was sealed under one master key
(`UZI_SECRET_KEY`) that lives in the API's environment — so anyone who could read
both the env (`kubectl describe`/exec, the Secret, Infisical, etcd) and the DB
had every token in plaintext. The vault removes the token's decryption key from
everywhere at rest.

Design rationale lives in the PRD
([../prds/done/32-user-vault-password-wrapped-secrets.md](../prds/done/32-user-vault-password-wrapped-secrets.md));
this page is the as-built residual-risk list and the operator hardening steps.

## What the vault protects — and what it does not

- **Protected**: each user's `user_secrets` rows (today the Anthropic token). They
  are sealed with a per-user 256-bit DEK. The DEK is stored only wrapped:
  `secretbox(KEK, DEK)` where `KEK = Argon2id(login password, per-user salt)`. The
  KEK is derived at unlock, used once, and discarded; the plaintext DEK lives only
  in the API process's memory while the vault is unlocked, and is gone on lock or
  restart. Neither the KEK nor the plaintext DEK is ever written anywhere.
- **Not protected (by design)**: the forge **bot PAT**
  (`forge_connections.token_ciphertext`). The poller syncs issues 24/7 with no
  user present, so its credential cannot be password-wrapped. It stays sealed
  under `UZI_SECRET_KEY`, which therefore remains — for connection-level secrets
  only. An operator can still recover the bot PAT; its blast radius is bounded by
  PRD #5's least-privilege checks (a Developer-role bot on protected repos).

## OIDC-only users: the vault passphrase (PRD #45)

An OIDC-only user (JIT-provisioned or linked via SSO with no password ever
set) has no login password for the KEK above to derive from. Such a user
instead sets a dedicated **vault passphrase** through a create-only endpoint,
running the exact same Argon2id derivation path (`api/internal/vault/vault.go`)
and enforcing the same 12-character floor as a login password — the KEK's
real strength (residual risk 1, below) is unchanged by which secret feeds it.
The DEK hierarchy underneath is identical; the vault has no notion of whether
the string it was handed at creation was a password or a passphrase. Changing
or recovering a lost passphrase is deferred (deleting and re-creating the
vault would lose already-sealed secrets) — a documented limitation, not an
oversight. See [auth-design.md](auth-design.md#oidc-single-sign-on) for how
this fits into the OIDC login flow.

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
   token directly or derives the KEK from the seeded password. The seed admin's
   vault is also **boot-unlocked** on every start (so a headless deployment runs
   overnight autopilot from first boot), which means its DEK is in memory
   unconditionally. See the hardening steps below.
3. **A token that ever existed master-sealed may already be leaked.** Lazy rewrap
   (on the owner's first unlock) improves the at-rest posture going forward, but an
   operator could have snapshotted the DB before the rewrap. Rewrap is **not
   retroactive** — the only real fix for a previously master-sealed token is to
   **rotate it**. The UI says so (Settings → token notice) and the admin migration
   count surfaces how many rows are still legacy-sealed.
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
7. **The DEK cache is per-process.** uzi runs single-replica today (compose). If a
   multi-replica API is ever deployed, **pin the API to `replicas: 1`** or accept
   that each pod must be unlocked independently. Do **not** replicate DEKs across
   pods.
8. **The AAD no longer pins a ciphertext to a *row*, only to `(user, kind)`
   (PRD #104).** Each sealed secret is bound to `user_id || 0x00 || kind` as
   additional authenticated data, so a DB-write operator who moves a ciphertext
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
   base64-encoded, not encrypted, at that layer. Anyone who can read it can
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

1. Change the seed admin's password (this rewraps the DEK). Until a
   change-password endpoint exists, re-register or rotate out of the seeded
   account. **Note:** once a change-password flow lands, a stale `UZI_SEED_PASSWORD`
   left in the environment would make the next boot-unlock fail — and boot-unlock
   is fatal when seeding is configured. Remove `UZI_SEED_*` after the account is
   self-sufficient.
2. Rotate `UZI_SEED_ANTHROPIC_TOKEN` (it was readable from the env) and re-save it
   through the UI so it is DEK-sealed under the changed password. See
   [anthropic-token.md](anthropic-token.md).
3. Remove `UZI_SEED_PASSWORD` and `UZI_SEED_ANTHROPIC_TOKEN` from the deployed
   environment.

## Rollout note

The deploy that ships the vault has a **one-time rollout stall**: every existing
user's runs stop claiming until they next log in. Their tokens are still
master-sealed and the claim gate requires an unlocked vault, so nothing runs for a
user until their first post-deploy login (which unlocks the vault and lazily
rewraps their token). Communicate this before deploying. The seed admin is
unaffected (boot-unlocked). Migration progress is visible to admins under
Settings → Instance settings as a count of still-master-sealed secrets.

## Vault-lock Slack notice (PRD #890)

Every restart is also an every-vault-lock event (the DEK cache is dropped, see
residual #7), which is precisely the stall this section describes — not just
the one-time first-vault-rollout, but any later restart too. A boot
reconciler now closes the "nothing tells the user" gap directly: for each
vault-having, Slack-linked user whose locked vault is blocking `queued`/
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

**Residual #7 interaction.** Because "locked" is per-process, an unlock on
one pod re-arms the marker, so a sibling pod still locked in its own process
can re-notify on its next tick. This is bounded — **at most one redundant DM
per cross-pod unlock event**, never N — and single-replica deployments (the
norm today) never hit it at all.
