# ADR-3507: Optional credential vault

**Status**: Accepted
**Date**: 2026-10-26
**Issue**: #3507

## Context

Password-wrapped personal credentials make passive theft of the database plus
server key harder, but require unlocking after an API restart. Operators need
an explicit instance policy and users need an informed choice without silent
conversion. This extends [PRD #32](../prds/done/32-user-vault-password-wrapped-secrets.md);
[ARCHITECTURE.md](../ARCHITECTURE.md#secrets-per-user-credentials-at-rest)
maps the service boundary. This ADR records the durable design.

## Decision

Persist `users.vault_mode` (`protected` or `unprotected`) and a monotonic
`vault_epoch`. Off users have no wrapper, unlock requirement or lock notice.
Protected users retain the password-derived KEK, wrapped random 256-bit DEK
and until-explicit-lock-or-server-restart cache. Password login unlocks a
protected vault. Login, seed startup and ordinary unlock leave Off users Off.

The database-only admin setting `vault_policy` accepts `optional` or
`required`; missing or invalid values resolve to Required. Production
enrollment explicitly chooses mode under current policy. Optional starts
new users Off; Required starts new users protected, including wrapperless
OIDC users awaiting enrollment. Policy changes do not convert or re-seal
material. Existing Off users remain runnable under Required and receive a
persistent enrollment prompt; protected users cannot disable under Required.

Migration 00373 uses constant-default additive mode/epoch columns: existing
users, including inactive/OIDC/wrapperless users, become protected without a
ciphertext migration. If any users exist, policy starts Required; an empty
database starts Optional. Migrations run before seed-admin creation, so a
fresh seed admin starts Off and newly seeded personal material is
`master_aad`. A protected seed admin still boot-unlocks when its environment
password is retained; an Off seed admin skips boot enrollment/unlock.

Settings uses **Protect my credentials with my password (vault)**. Enabling
verifies the current login password or creates an OIDC-only passphrase
(12–1024 UTF-8 bytes). Wrapper creation is create-only; it cannot overwrite
an existing protected wrapper. Disabling requires an unlocked protected
vault and Optional policy. Password + OIDC linking retains the user's mode
and password semantics. Passphrase change and recovery remain deferred.

User help targets [Vault choice](../docs/anthropic-token.md#vault-choice)
(`/docs/anthropic-token#vault-choice`), with a Codex credentials crosslink;
admin help targets [the threat model](../docs/vault-threat-model.md)
(`/docs/vault-threat-model`). Both CLI provider tables display `VAULT off`
for unprotected users, and the TUI omits their lock indicator. Missing mode
or unlocked fields retain older-server fallbacks. No CLI toggle or unlock
command is added.

## Sealing and threat boundary

Protected personal material uses DEK sealing; Off material uses
`UZI_SECRET_KEY`, equivalent in passive-read exposure to server-key-sealed
forge bot tokens. DB plus server-key theft exposes Off credentials. A live
compromised server can read protected credentials while unlocked. Password
entropy/offline cracking, seed environment secrets, worker plaintext and
historical backups remain residual risks; see the threat model.

The protected, locked Codex refresh recovery exception remains, now using
`master_aad` rather than legacy nil-AAD master sealing. It retains the
successful provider result for later reconciliation without replacing a
locked live login. Mode/epoch races and background persistence retries
re-seal that same result under current state without a second provider
exchange. Conversion includes both live and recovery account slots.

New user-material master writes use `master_aad`. Both DEK and
`master_aad` authenticate `user_id || 0x00 || kind`: the UUID's raw 16
bytes, one zero byte, then the kind bytes, as implemented by `secretAAD`
in [vault.go](../api/internal/vault/vault.go). This prevents cross-owner/kind
swaps, but accepts same-owner/same-kind row swaps (PRD #104 residual).
Legacy `master` remains readable with nil AAD; unknown markers fail closed,
without discriminator reinterpretation. An older binary rejects
`master_aad`.

Residual #3 applies to enabling as well as lazy migration: values ever
master-sealed may remain readable in backups even after enabling protection.
Advise credential rotation; re-sealing current rows does not erase copies.
Residual #2 remains for a protected seed account with its environment
password retained; Off seed accounts are not implicitly enrolled.

## Atomic conversion and writer fences

[The lifecycle implementation](../api/internal/vault/lifecycle.go) takes the
per-user lifecycle mutex before starting a transaction. Cache entries carry
the epoch; a key is published after commit. The cache mutex does not span
database or provider I/O. Monotonic epochs fence stale writers through
protected → Off → protected ABA transitions; comparing mode alone is
insufficient. Ordinary credential writers use transactional mode/epoch
checks rather than acquiring lifecycle mutexes.

The lock order is lifecycle mutex → shared `SettingsMutationLockKey`
admission for enrollment/disable → user-secret advisory guard → existing
worker/run locks where needed → users mode/epoch row → wrapper → accounts →
secrets → alias states, with row-ID ordering within material collections.
Admin policy edits take exclusive settings admission. Admission is followed
by an authoritative policy read in a separate READ COMMITTED statement,
so a waiter observes the policy committed by the previous holder.
[store/vault_tx.go](../api/internal/store/vault_tx.go) defines the writer
fence; [settings.sql](../api/internal/store/queries/settings.sql) defines the
fail-closed policy read. Registration takes policy admission before its work
and retains the first-user lock before inserting a user. Protected lazy
rewrap locks current account/secret material before reading or decrypting,
so it cannot overwrite a same-epoch credential replacement.

Keep the existing run → alias → account NOWAIT exception for claim finishers
and account promotion. They must not wait for lifecycle/advisory acquisition
inside an already-open claim-finishing transaction; see
[codex_account_promote.go](../api/internal/workersvc/codex_account_promote.go).
Provider I/O stays outside credential-write transactions. The result writer
binds current mode/epoch and holds its fence through commit; see
[codex_vault_write.go](../api/internal/workersvc/codex_vault_write.go).
The reconciler's existing install-and-link transaction receives seal
snapshots prepared outside it; it does not acquire lifecycle serialization
inside that transaction. Run promotions and usage-poller pokes occur after commit and lifecycle-mutex
release, so their failure cannot undo a committed conversion.

Conversion updates ciphertext and discriminators together while preserving
IDs, defaults, capabilities, alias bindings, material revisions, provider
generations and recovery metadata. Crypto-only conversion does not bump
provider revisions; see [vault_material.sql](../api/internal/store/queries/vault_material.sql).

[conversion.go](../api/internal/vault/conversion.go) enforces **10,000 material
rows and 64 MiB ciphertext per user**, with **64-material-row/1 MiB batches**.
An account's live/recovery pair travels together and must fit 1 MiB; populated
recovery slots count separately. Disabled secrets and unused/orphan accounts
count. Preflight refuses oversize totals/records before collecting ciphertext
batches; transactional checks refuse exceeding bounds without partial
conversion. The lifecycle context has a **two-minute deadline**, defined in
lifecycle.go; this is not a production performance promise.

Resource-limit refusal returns measured totals, largest record and limits,
confirms unchanged material/mode, and points to existing credential-management
removal controls. Removing aliases need not free canonical accounts.
Orphan-account remediation, including a dedicated deletion API, index or UI,
remains deferred PR scope; no orphan cleanup workflow is supplied here.

A confirmed abort rolls back conversion. Lost commit acknowledgment is
uncertain: discard the unpublished key, evict cached state and reconcile on a
fresh connection under applicable locks. Reconciliation has a fresh bounded
deadline; failure is reported rather than treated as rollback proof. The user
refreshes status before retrying and unlocks if protected. Do not claim an
unchanged database or compensate by deleting the wrapper after an uncertain
acknowledgment.

## Deployment and rollback

The supported API topology is **singleton with Recreate replacement**.
Mixed-version API operation is unsupported. The existing
[API deployment](../deploy/chart/templates/api-deployment.yaml) already states
HARD SINGLETON and sets `strategy.type: Recreate`; no chart change is needed.

Constant defaults avoid a user/ciphertext rewrite, but ALTER TABLE locks and
immediate CHECK validation scans expose production-dependent lock duration.
Plan maintenance and backups. Production database sizes, conversion times and
DDL lock durations are unknown; fixture timings below do not establish them.

The [down migration](../api/internal/store/migrations/00373_vault_mode.sql)
refuses any unprotected user or `master_aad` in personal secrets or either
Codex account slot. Operators must use the **new binary** to safely convert
affected users/material before down, quiesce credential mutations, verify
those refusal conditions are absent, then coordinate replacement. Simply starting an older binary is
unsafe: it cannot read the new marker or preserve Off semantics.

## Measured fixture evidence

Lead-owned evidence at commit `21cdb494`, recorded 2026-10-27, used isolated
`postgres:17` Docker and Go 1.27.3 linux/amd64. The fixture had the
repository test-database marker; its URL was scoped only to the filtered
serial invocation below (`fixture_dsn` names that owned fixture). Ordinary
unfiltered Go gates leave `UZI_TEST_DATABASE_URL` unset. From `api/`:

```sh
env UZI_TEST_DATABASE_URL="$fixture_dsn" go test -p 1 -count=1 -v ./internal/store -run '^TestVaultBudget(Enable|Disable)LiveDB$'
```

The local evidence log was `.uzi/scratch/gate-log.veq34n`: two top-level
tests plus twelve subcases, RUN 14 / PASS 14 / FAIL 0 / SKIP 0, EXIT 0.
The package took 33.939 s including migrations and fixtures; that is not a
migration-DDL timing. The conversion log brackets the enable/disable operation
itself. Fixtures exercise limits, rollback on refusal and preservation of
non-crypto metadata in [vault_budget_livedb_test.go](../api/internal/store/vault_budget_livedb_test.go).

| Fixture ciphertext | Result | Enable | Disable |
|---|---|---|---|
| 10,000 rows / 320,000 bytes (about 313 KiB) | Accepted row limit | 7.327 s | 6.324 s |
| 64 rows / 67,108,864 bytes (64 MiB) | Accepted total limit | 1.461 s | 1.305 s |
| 2 slots / 1,048,576 bytes (1 MiB account pair) | Accepted pair limit | 160 ms | 27 ms |
| 10,001 rows / 320,032 bytes | Refused row limit | 106 ms | 12 ms |
| 65 slots / 67,108,893 bytes (64 MiB + 29 bytes) | Refused total limit | 99 ms | 5 ms |
| 2 slots / 1,048,577 bytes (1 MiB + 1 byte pair) | Refused pair limit | 106 ms | 4 ms |

These are illustrative rounded local fixture measurements. The worker shared
CPU with CLI package tests; this was not a controlled production benchmark.
Production sizes/times are unknown, and no remote-CI timing claim is made.
