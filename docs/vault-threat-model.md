---
title: Vault threat model
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
