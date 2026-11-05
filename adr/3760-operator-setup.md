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
  is read with a 16 KiB bound, and every message is scrubbed of it before it
  reaches stdout or stderr.
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
  usage, configuration, schema or owner precondition with nothing written.

## Consequences

The command runs in another process than the API, so it cannot call the API's
in-process repo-sync invalidation. The report marks issue sync `pending`; the
poller imports issues on its next interval.

The worker step and exit code 3 follow in a later milestone of the PRD.

## Rejected

- More `UZI_SEED_*` variables: seeds run on every boot, swallow forge failures
  and give a script neither an exit code nor a safe delivery channel for a
  one-time token.
- A setup-scoped bearer token, `uzi worker create` over `uzc_`, or `uza_`
  writes: each widens what a stolen token can do.
- A `--user` flag acting for arbitrary users.
- Relaxing `config.Load`, or changing the shared connection upsert.
