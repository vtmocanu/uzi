# PRD #3760: Scriptable first setup, an operator command for the forge, the repos and the first worker

**Issue**: [#3760](https://github.com/vtmocanu/uzi/issues/3760)
**Status**: Draft, design proposed by the maintainers for the reporter to review and implement
**Priority**: Medium
**Created**: 2026-11-02
**Related**: #1173 (VPS / Compose deployment guide), PRD #64 (no `worker create` over a CLI token), PRD #65 (forge seed, D11 explicit forge type), PRD #66 (repo-enable guard), PRD #5 (least-privilege token gate)

## Problem

An operator who installs uzi on a fresh host (Compose on a VPS, or the Helm chart) cannot finish first setup from a script. After the stack boots, three steps still need the web UI:

1. **Connect the forge.** The CLI has no forge command. The startup seed (`UZI_SEED_FORGE_PAT`) is pinned to GitLab (`api/internal/seed/seed.go`, `seedForgeType`), so a GitHub or Forgejo install cannot use it.
2. **Enable repos.** `uzi repo` offers `list` and `remove`, not `enable`.
3. **Create the first worker and get its join token.** `worker create` is deliberately absent from the CLI (`docs/cli.md`: a join token reads decrypted secrets, so a stolen `uzc_` must not be able to mint one).

Today a setup script therefore logs in with the admin password and drives the cookie-plus-CSRF web API. That stores a password more powerful than the join token it fetches, and it couples the script to internal routes.

The forge seed also differs from the web path in two safety checks: it skips the least-privilege token check (`handler/forge.go`, `CreateConnection` calls `pcheck.CheckToken`) and it enables repos with `SetRepoEnabledForUser` directly, skipping the live `GuardRepo` enable gate (`handler/forge.go`, `SetRepoEnabled`). Claim time still runs `GuardRepo` (`workersvc/claim_assembly.go`), so this is not an open hole, but it is the wrong model to copy.

A repo enabled without the web UI also has no board. The board columns are created only on the first `GET /api/repos/<id>/board` (`handler/board.go`, `seedBoard`), which only the web page sends. With 0 columns, `board.ResolveColumn` returns no column, the run lifecycle treats the card as moved by a person and drops the automated move (`runlifecycle/lifecycle.go`), the card stays on `In Progress` after the run opens its PR, the PR watcher (it selects `Human Review` cards) never records the merge request, and `uzi run rework` answers that the merge request is not open. The startup seed has the same gap. Found in a headless install on 2026-11-01; one board read repaired it.

## Outcome

A one-shot command in the API image, `/server setup`, run on the deployment through `docker compose exec` or `kubectl exec`, connects the forge, enables the named repos and creates the first worker for the configured seed admin. It applies the same checks as the web UI, reports every step in machine-readable form, and exits with a code a script can branch on. No new token class, no new listener, and no change to what `uzc_` or `uza_` tokens can do.

Acceptance examples:

1. **Fresh Compose install, GitHub.** After `docker compose up -d`, the operator runs
   `printf '%s' "$PAT" | docker compose exec -T api /server setup --forge-type github --forge-base-url https://api.github.com --forge-token-file - --repos acme/app,acme/lib --worker-name vps-1`.
   It prints one JSON report on stdout: forge `connected`, both repos `enabled` with their board columns created, worker `created` with its join token, and exits 0. The operator writes the token into `UZI_WORKER_TOKEN` and starts the `agent` profile.
2. **Unprotected default branch.** Same command, but `acme/lib` lets the bot push to `main`. The report shows `acme/app` `enabled`, `acme/lib` `refused` with the same reasons the web UI shows, and the worker `skipped_dependency` with no join token minted; the command exits 1. The forge connection and `acme/app` stay in place; nothing is rolled back.
3. **Re-run after success.** The same command again reports forge `present`, repos `enabled` (re-checked, unchanged), worker `exists` with no token, and exits 3, telling the script that a worker already exists and its token cannot be recovered.

## Out of scope

- **Codex login import.** Part of the requested setup path, delivered as a follow-up PRD. It must go through the shared credential path (vault sealing and fencing, the default-slot lock, credential state, identity reconciliation); inserting ciphertext alone is wrong. Until it lands, this PRD is a partial delivery of #3760.
- **Explicit forge type for the startup seed** (`UZI_SEED_FORGE_TYPE`, GitLab default, plus fixing the stale `seedForgeType` comment and documenting it in `.env.example`). A separate small issue, independent of this PRD.
- **Worker token rotation or replacement.** Rotation can invalidate a running worker and needs its own lifecycle design.
- **Hosted (Kubernetes) worker provisioning.** On the Helm chart the primary worker path is hosted workers created through the controller. `setup` creates a self-hosted (join-token) worker only; on Kubernetes an operator may skip the worker step.
- **Acting for any user other than the seed admin**, promoting an account, or resetting a password or vault.
- **Waiting for the first issue sync.** The report marks it pending; a bounded wait option can come later.
- **CLI ergonomics from the same report**: showing the pending question in `uzi run get`, and `uzi run create` refusing an issue until the next forge poll. Separate issues.
- **Any change to `uzc_`, `uza_`, or the cookie-only routes.**

## Modules and seams

### 1. Shared provisioning service (prefactor)

Extract the decision logic of three handlers into one service that both the handlers and `setup` call, so there is one set of security rules:

- `ConnectForge(ctx, userID, forgeType, baseURL, token) (conn, error)`: allowlist check (`cfg.ForgeBaseURLAllowed`), base URL normalisation, client build, `VerifyToken`, `pcheck.CheckToken` (over-privileged token refused with its violations), encrypt, upsert. Same order and errors as `CreateConnection` today.
- `DiscoverRepos(ctx, userID, connID) ([]repo, error)`: the forge project listing and `UpsertRepo` loop from `ListProjects`.
- `EnableRepo(ctx, userID, repoID) (repo, GuardResult, error)`: `GuardRepo` before the flip, honouring the live override, then the board columns, then `SetRepoEnabledForUser` and the pending-override settlement. Same 422 findings as `SetRepoEnabled`. When the repo has no board columns, it creates the default column labels on the forge (`forgesvc.DefaultColumns`) and their `board_columns` rows, the same columns `seedBoard` creates on first board read; a repo that already has columns keeps them. If the forge refuses the labels, the repo stays disabled and the step fails (the web enable answers 502). No issue import here: the poller imports issues on its next pass. The first board read keeps creating columns for repos enabled before this change, and the startup seed calls the same column step after it enables a repo.

Worker creation already lives in `workersvc.(*Service).CreateWorker`; `setup` calls it with the same name validation the handler applies (`termsafe.Validate`, the 200-byte limit, template validity).

Invariant: the handlers' HTTP behaviour (status codes, bodies, side effects) is unchanged, except that enabling a repo now creates its board columns and answers 502 when the forge refuses the column labels. The handler-local `invalidateRepoSync` hook stays in the handler; `setup` runs in another process and cannot reach it (see Decision 6).

### 2. `/server setup` mode

- **Entry**: `api/cmd/server` gains a `setup` subcommand beside `-health`, dispatched before normal server initialization. It uses a setup-specific configuration loader that shares the existing database, `UZI_SECRET_KEY`, forge-allowlist and email validators. It resolves `UZI_SEED_EMAIL` without requiring or reading `UZI_SEED_PASSWORD` (`config.Load` refuses a seed email without a valid password, `loadSeedAdmin`), and it runs no startup seeds. Normal server configuration validation stays unchanged. The command opens no listener and starts no background service.
- **Schema precondition**: it never migrates. It calls `store.SchemaVersionStatus` and refuses (exit 2) unless the schema is at this binary's head, so it only runs against a database the API has already migrated.
- **Owner**: the user named by `UZI_SEED_EMAIL`. Missing, or not `is_admin`, is a refusal (exit 2). It never creates, promotes or changes that user.
- **Inputs**: non-secrets as flags: `--forge-type` required, no default and no detection, per PRD #65 D11; `--forge-base-url`; `--repos` as comma-separated `path_with_namespace`. `--worker-name` is optional: omitting it skips worker provisioning and reports `not_requested`; supplying it requests creation. `--worker-template` requires `--worker-name`. All flags, repo entries, worker names and templates are validated and normalized before any write; invalid input exits 2. The one secret, the forge token, comes only from `--forge-token-file PATH`, where `-` means stdin. Never from a flag value or an environment variable.
- **Steps, in order**: connect or reuse the forge, discover repos, enable each requested repo, then optionally create the worker. A connection or discovery failure skips the dependent steps. All requested repos are evaluated independently; any missing or refused repo prevents worker creation. The JSON report includes every requested repo and step, using `skipped_dependency` where execution was prevented. Completed writes remain. Exit 1 takes precedence over exit 3; exit 3 applies only when every preceding requested step succeeded and worker creation found an existing worker.
- **Existing state**: a connection for the same forge type and base URL is reused untouched (`present`, not re-verified, never overwritten; a different token is never silently rotated). Setup's connection persistence is atomically create-only: on a uniqueness conflict it reads and reuses the winning connection without changing its token or identity. `UpsertForgeConnection` updates the ciphertext on conflict (`queries/forge.sql`), so setup needs its own create-only write; validation is shared with the web path, and the web path keeps its existing reconnect/upsert behaviour. Repos are idempotent: enabling an enabled repo re-runs the guard and reports `enabled`. The worker step reports `exists` with no token when the owner already has any worker. Setup's worker existence check and creation are serialized per owner, so two concurrent setup invocations cannot both mint a first worker. Concurrent live-DB tests prove both guarantees.
- **Output**: exactly one JSON document on stdout, schema-versioned, one entry per step and per repo with `status` and, for refusals, the findings. The join token appears only in the worker entry of that one document. Diagnostics go to stderr; neither stream ever carries the forge token, and stderr never carries the join token.
- **Exit codes**: 0 all requested steps done or already present; 1 a step failed or was refused (report says which); 2 usage, configuration, schema or owner precondition, nothing written; 3 everything else succeeded but the worker already exists, so no token was issued.

**Resource bounds** (inputs are operator-local but still bounded): read at most 16 KiB plus one byte from the token source, rejecting oversized input before any write. Accept at most 200 nonempty repo paths and 64 KiB of aggregate repo-path input; the worker-name limit does not apply to repository paths. One database pool of at most two connections. Forge calls inherit the existing driver timeouts and pagination caps. A five-minute deadline covers input acquisition and everything after it; cancellation also interrupts a blocked stdin read. Bounded inputs are validated before writes.

## Testing decisions

- **Prefactor**: the existing handler tests for `CreateConnection`, `ListProjects` and `SetRepoEnabled` must pass; the only allowed change is a fake-forge `labels` route, which the new column step calls; add service-level tests on a fake forge (embedding `forgetest.BaseFake`) for an over-privileged token refusal and a `GuardRepo` block.
- **Setup command**: a live-DB test (the repo's store-it pattern, see `.claude/rules/go.md`) drives `setup` in-process against a fake forge for: the three acceptance examples; a missing or non-admin seed owner (exit 2, nothing written); a schema behind head (exit 2); setup succeeding with `UZI_SEED_PASSWORD` absent once the owner exists; oversized token and repo input; invalid worker input caught before any write; cancellation of a blocked stdin read, without real-time waits; concurrent invocations that must not rotate a connection or mint two workers; and a check that the forge token appears in neither stream and the join token appears only once, in stdout.
- **Regression for the bypass**: a test that fails if `setup` enables a repo whose guard blocks, the bug class the forge seed has.
- **Regression for the missing board**: a test that fails if enabling a repo with no board columns (web enable, startup seed or `setup`) leaves it with 0 columns, and a test that a repo with existing columns keeps them unchanged.
- **Execution paths**: document the Compose and Kubernetes invocations, and exercise the binary's `setup` entry in the existing compose smoke if the harness change is small. Otherwise cover it with an `acceptance` issue (below).
- Follow the existing seed tests (`api/internal/seed/seed_test.go`) for the fake-store pattern.

## Milestones

### M1: Prefactor, shared provisioning service
Extract `ConnectForge`, `DiscoverRepos` and `EnableRepo` from the handlers; handlers call the service. No behaviour change, except that enabling a repo creates its board columns (web enable and startup seed).
- **Blocked by**: none
- **Acceptance**: handler tests unchanged and green; new service tests cover the token-privilege refusal, the enable guard and a refused column label; the missing-board regression test passes; `task gate:api` green.

### M2: `/server setup` connects the forge and enables repos
The subcommand, preconditions, inputs, JSON report and exit codes 0/1/2 for the forge and repo steps, with tests and operator docs (Compose and Kubernetes invocations). The ADR for the on-box setup boundary lands here.
- **Blocked by**: M1
- **Acceptance**: acceptance examples 1 (without the worker) and 2 pass in the live-DB test; preconditions refuse with exit 2 and write nothing; the forge token never appears in output.; `task gate:api` green.

### M3: `setup` creates the first worker
The worker step via `workersvc.CreateWorker`, create-only, exit code 3, token in the single stdout report, and docs for wiring the token into `UZI_WORKER_TOKEN` (Compose) and skipping the step on Kubernetes hosted-worker installs.
- **Blocked by**: M2
- **Acceptance**: acceptance examples 1 and 3 pass; the join token appears exactly once and only on stdout.; `task gate:api` green.

### Landing obligations
- `task docs:sync` after any `docs/*.md` edit; CHANGELOG fragment.
- An `acceptance` issue owned by the maintainer for a live run on a fresh Compose host and on a Kubernetes install via `kubectl exec -i`.

## Decision Log

1. **A one-shot command in the API image, not more startup seeds.** Seeds run on every boot, log and swallow forge failures so a forge outage never blocks boot, and give a script no exit code; a script cannot tell "API is up" from "setup succeeded", and a one-time join token has no safe delivery channel from a boot log. Rejected: extending `UZI_SEED_*` with forge type, worker and Codex entries.
2. **No new token class and no CLI write path.** The boundary is "can execute in the deployment" (Compose host access, or Kubernetes `pods/exec` RBAC), which already reaches `UZI_SECRET_KEY` and the database. A stolen `uzc_` still cannot mint a worker (PRD #64), and `uza_` stays admin read-only. Rejected: a setup-scoped bearer token, `uzi worker create` over `uzc_`, or `uza_` writes.
3. **Protected vaults stay protected.** Host access plus `UZI_SECRET_KEY` does not decrypt a user's protected vault, so `setup` never reads, resets or bypasses one. Nothing in M1 to M3 touches vault-protected credentials; the Codex follow-up must take the vault passphrase where one is set.
4. **Reuse the web handlers' rules through one service, not the forge seed's.** The seed skips `CheckToken` and the live `GuardRepo` enable gate. Rejected: calling `seed.ForgeConnection` from `setup`.
5. **Seed admin only.** It is the one identity the operator already configured out of band. Rejected: a `--user` flag acting for arbitrary users.
6. **No waits; report pending.** The new repos reach the poller on its next interval, since `setup` cannot call the API's in-process sync invalidation. The report marks issue sync as pending. Rejected: waiting for the first sync, or a cross-process notification channel.
7. **Create-only worker, exit 3 on an existing one.** A join token is stored as a hash and shown once, so an existing worker's token is unrecoverable; a distinct exit code lets an idempotent script tell "already set up" from a failure. "Any worker owned by the seed admin" is the test, so no new marker column is needed. Rejected: rotation in this PRD, and a name-based "setup-created" marker.
8. **Explicit forge type, no detection.** The driver is chosen before verification (PRD #65 D11).
9. **Partial progress is kept, not rolled back.** Each step is independently valid (a connection without repos, an enabled repo without a worker), matching how the web UI builds the same state step by step; the report says exactly what exists.
10. **Secrets from a file or stdin only.** Flag values and environment variables leak through process listings and container inspection.
11. **A setup-specific config loader.** `config.Load` refuses a seed email without a valid seed password, so reusing it would force operators to keep the admin password in the environment, the very thing this PRD removes. Rejected: relaxing `config.Load` for the server.
12. **Atomic create-only writes in setup only.** A lookup followed by the existing upsert can rotate a concurrently created connection, and a lookup followed by worker creation can mint two first workers. Setup gets a create-only connection write and a per-owner serialized worker step; the web path keeps its reconnect/upsert semantics. Rejected: changing the shared upsert.
13. **Enabling a repo creates its board columns.** A repo enabled without a board read had 0 columns, and the run lifecycle then dropped every automated card move (Problem). Creating the columns in the shared enable step fixes the web enable, the startup seed and `setup` in one place. A refused label keeps the repo disabled, because a repo without columns breaks runs silently later. The startup seed is the one exception: it turns the repo on first and only logs a refused label, because a seed never blocks boot (Decision 1); the first board read then repairs that repo. Rejected: keeping the creation on first board read only (a headless install never sends one), and a poller repair for enabled repos with 0 columns (more moving parts; the first board read already repairs older repos).
