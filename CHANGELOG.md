# Changelog

Notable changes to uzi, loosely following [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions are release git tags (`deploy/chart/Chart.yaml`'s `version`/`appVersion`, Model B) — this
file is not bumped per-commit; `[Unreleased]` collects everything since the last tag.

## [Unreleased]

### Added

- A run whose updates cannot be saved is now flagged **looping** with a reason that names the cause ("the agent's updates can't be saved, so it keeps resending them") instead of falling through to the 45-minute "taking longer than usual" wall clock. The signal is the API's own count of failed writes for that run, so unlike the existing loop detector it is not blind to a broken message stream (PRD #108).
- A confirmed per-run write loop is **stopped automatically**, in about a minute, under a conjunction of guards that must all hold — including "other runs on this instance are successfully saving messages", so an API-wide outage stops nothing. Such a run ends `failed` with the new stop kind `auto_stopped`, which `uzi run get` shows as a `STOP_KIND` row and the web styles as breakage rather than as a stop you asked for. Operators can turn the stop off with `UZI_AUTOSTOP_ENABLED=false`, which leaves the flag working (PRD #108). See [docs/run-auto-stopped.md](docs/run-auto-stopped.md).
- `GET /api/admin/cli-tokens` / `uzi admin cli-tokens` lists every CLI token in the factory with its owner — closing a gap `workers` hasn't had since PRD #42, where a token was visible to its own holder and nobody else. Read-only (no admin revoke) and never returns the token value or its hash, at four independent layers (query projection, generated row type, DTO, handler). The row does carry `last_used_ip`, so an `admin_ro` holder can see every user's source IPs — deliberate, since `admin_ro` is factory-wide read, but worth knowing before minting one (PRD #98). See [docs/cli.md](docs/cli.md#commands).

### Known limitations

- Auto-stop needs at least one *other* run saving messages at the same time to act. On an instance running a single run at a time it will flag and notify but never stop — the correct behaviour on insufficient evidence, and the reason the flag, not the stop, is the value on that deployment shape (PRD #108).
- A run failing because the **worker itself** is sending malformed requests is flagged but never auto-stopped, because a worker defect affects every run that image touches and hiding them one at a time would hide the pattern. The remedy is to roll the worker image; the log line carries `failure_class=invalid` (PRD #108).

## [0.11.1] - 2026-07-22

Re-ships the PRD #87 browser prebake + `web-ux` builtin (v0.11.0, rolled back to v0.10.1 after live testing on dev-cluster caught three cluster-only bugs). Fixes all three (issue #114).

### Fixed

- Docker-tier workers no longer CrashLoop at `seed-nix`: the browser build guard, running as root, created a `root:root 0700` directory in the prebaked Chromium nix closure that the non-root (uid 10001) seed tar could not read; `/nix` store permissions are now normalized after the guard in both worker Dockerfiles (BUG 1).
- The prebaked browser now launches under the hardened worker: the `agent-browser` shim's `XDG_CONFIG_HOME` is uid-scoped so the Crashpad database directory (previously baked `root:root` by the root build guard) is writable by uid 10001 at runtime. This resolves the `Chrome exited early without writing DevToolsActivePort` / Crashpad `recvmsg` reset failure, which is not caused by seccomp: a non-writable XDG is the sole determinant, confirmed on-cluster under both RuntimeDefault and Unconfined (BUG 2a and 2b, one root cause).
- The `uzi-hosted-workers-docker` ResourceQuota no longer over-counts storage: the controller now skips re-creating PVCs it already observes as present, ending the per-tick admitted-then-`AlreadyExists` creates that inflated `used.requests.storage` without decrement (k8s #119593) and pinned the quota at its limit, blocking new workers (BUG 3).

## [0.10.1] - 2026-07-22

### Fixed

- Worker message batches carrying a NUL byte, an unpaired UTF-16 surrogate, or invalid UTF-8 are sanitized server-side instead of wedging the run in a silent retry loop (PRD #108).
- A permanently-unstorable message batch now returns 400 (never retried) instead of 500 (retried forever); an oversize batch returns 413 and is split, never treated as poison (PRD #108).
- The worker's message batcher bounds batch size, backs off exponentially, and bisects a rejected batch down to the single poisoned message instead of growing the retry body past the API's 1 MiB cap (PRD #108).
- A poisoned message is tombstoned in place (visible in the run's history) rather than dropped, so the live run view no longer freezes at a permanent gap (PRD #108).
- `/api/worker/runs/:id/state` text fields (`failure_reason`, `session_id`, `plan_md`, `branch`, `mr_web_url`) are NUL-stripped, closing the same poison-pill class on the sibling route (PRD #108).
- `run_usage`'s `session_id`/`model` are length-capped before the upsert, closing a second poison-pill route (an oversized composite-key index entry, Postgres 54000) (PRD #108).
- Run HOME cleanup now restores write permission before removing a tree, so a Go module cache's read-only directories no longer strand disk on every Go-touching run; a one-off startup sweep (`UZI_HOME_RECLAIM`) reclaims HOMEs already stranded on existing volumes (PRD #108).

### Security

- Worker-side redaction now covers the `agent` and `kind` message fields, not just the payload and `agent_instance`/`agent_label`, closing a gap where a secret placed in either field reached the API, the WebSocket frame, the browser, and `uzi run logs` unscrubbed (PRD #108).
