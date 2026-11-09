# ADR-3760: The on-box operator setup command

Status: accepted. Date: 2026-11-05. Issue: #3760. PRD: `prds/3760-operator-setup.md`.

## Decision

`/server setup` is a one-shot subcommand of the API binary. An operator runs it
inside the deployment (`docker compose exec` or `kubectl exec -i`) to connect a
forge and enable repos for the seed admin, with a machine-readable report and an
exit code a script can branch on.

The trust boundary is "can execute in the deployment". Compose host access, or
Kubernetes `pods/exec` RBAC, already reaches `UZI_SECRET_KEY` and the database,
so the command adds no new authority. It adds no token class, no listener and no
CLI write path. A stolen `uzc_` token still cannot mint a worker; `uza_` stays
admin read-only.

Rules the implementation keeps:

- It reuses `provisionsvc`, the service the web handlers call. The token
  least-privilege check, the live repo-enable guard and the board-column
  creation therefore apply exactly as in the web UI. The forge startup seed
  skips the first two and is not a model for this command.
- It acts only for the user named by `UZI_SEED_EMAIL`, who must already exist
  and be an admin. It never creates, promotes or changes that user, and it
  never reads or resets a user's protected vault.
- It never migrates. It refuses a database that is not at the binary's schema
  head (exit 2).
- Its configuration loader is separate from `config.Load`. `config.Load`
  refuses a seed email without a seed password, and setup must not need the
  admin password in the environment. `config.Load` is unchanged.
- The one secret, the forge token, comes only from `--forge-token-file`
  (`-` = stdin). Never from a flag value or an environment variable. The token
  is read with a 16 KiB bound and must be a regular file or stdin; setup
  refuses a FIFO or a device without blocking. Every string in the stdout
  report (the intended `join_token` aside), every stderr diagnostic, and every
  `slog` line the shared services write while setup runs pass the same scrubber,
  which replaces the token. A database open error is replaced by a fixed
  message, since the raw error can carry the password.
- Inputs are bounded (200 repo paths, 64 KiB of repo input), the pool holds at
  most two connections, and a five-minute deadline covers input acquisition and
  everything after it; cancellation interrupts a blocked stdin read.
- An existing connection for the same forge type and base URL is reused
  untouched: not re-verified, never overwritten. The connection write is
  create-only (`CreateForgeConnectionIfAbsent`); on a uniqueness conflict the
  winner is read and reused. The shared upsert would rotate the stored
  ciphertext, so the web path keeps it and setup does not use it.
- Completed writes are kept. Each step is independently valid, as when the web
  UI builds the same state step by step. The report says what exists.
- Exit codes: 0 done or already present, 1 a step failed or was refused, 2
  usage, configuration, schema or owner precondition with nothing written, 3
  everything succeeded but the worker already exists (M3).

## Consequences

The command runs in another process than the API, so it cannot call the API's
in-process repo-sync invalidation. The report marks issue sync `pending`; the
poller imports issues on its next interval.

## Worker step (M3)

- `--worker-name` creates the owner's first self-hosted worker through
  `workersvc.CreateWorker`, the call the web handler makes. The step is
  create-only: if the owner owns any worker, setup reports `exists`, prints no
  token and exits 3.
- The existence check and the creation run in one transaction behind a
  per-owner advisory lock (`poolWorkers` in `setupcmd/prod.go`). A second
  concurrent run waits, then sees the first run's worker. The web path takes no
  such lock and keeps its behaviour. The lock is a transaction-scoped
  advisory lock, so a crash releases it and no schema change is needed.
- The worker step runs only when the forge and every requested repo succeeded.
  Otherwise it is `skipped_dependency` and no token is minted. Exit 1 wins over
  exit 3.
- The join token is a field of the worker entry in the one stdout document. The
  worker error text is never copied into the report or to stderr, so a failure
  message cannot carry a token. If the report write itself fails, the token is
  lost; the operator deletes the worker and runs setup again.
- Hosted (Kubernetes) workers are out of scope: setup creates join-token
  workers only, and an operator on the Helm chart omits `--worker-name`.

## Rejected

- More `UZI_SEED_*` variables: seeds run on every boot, swallow forge failures
  and give a script neither an exit code nor a safe delivery channel for a
  one-time token.
- A setup-scoped bearer token, `uzi worker create` over `uzc_`, or `uza_`
  writes: each widens what a stolen token can do.
- A `--user` flag acting for arbitrary users.
- Relaxing `config.Load`, or changing the shared connection upsert.
