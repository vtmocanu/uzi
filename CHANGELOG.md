# Changelog

Notable changes to uzi, loosely following [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions are release git tags (`deploy/chart/Chart.yaml`'s `version`/`appVersion`, Model B) — this
file is not bumped per-commit; `[Unreleased]` collects everything since the last tag.

## [Unreleased]

### Fixed

- **Browser launches on hosted k8s workers get `--no-sandbox` again.** The worker's `CMD` is `npm run start`, and npm's run-script prepends `/app/node_modules/.bin` to `PATH` — so on the non-root (single-uid) start the real `agent-browser` CLI shadowed the PRD #87 shim, and every launch silently lost the flags the shim injects. Chromium then aborted on the setuid sandbox that the worker hardening makes impossible. The entrypoint now pins the runner PATH on both start modes, so the shim resolves first on k8s as it always did on compose. Runner children also stop resolving the worker's own `node_modules/.bin` (`tsx`, `tsc`, `esbuild`, …), which is the intended boundary (PRD #120, issue #120).

### Added

- `uzi tui`: a full-screen terminal UI — a live board of your runs, a run detail view with a per-agent lane rail and live transcript, and in-place steering (follow-up, approve/reject, cancel) and judge-review triage, all without leaving the keyboard (PRD #112).
- **Builtin skills now ship allocated.** Each builtin carries a default shared allocation, seeded the first time uzi inserts the skill, so a fresh instance no longer needs an admin to click allocate before a builtin can reach a run. That gap was total rather than partial: allocations are what build the run's skill union, so an unallocated builtin reached nobody, not the subagents it was meant for and not the lead either. Seeding happens only on that first insert, so an allocation an admin removes afterwards stays removed; `ci-cd-norms` (whose row predates the mechanism everywhere, so its insert can never fire again) is backfilled onto existing instances by a migration, allocated to `coder` and `reviewer` (PRD #72 M2). See [docs/skills.md](docs/skills.md).
- **A `prd-lifecycle` builtin skill**, allocated to `lead` and `reviewer` by default: the playbook for updating the issue's linked `prds/*.md` file at the end of an issue run, ticking items only on direct evidence, and moving the file to `prds/done/` when (and only when) every item is complete. The lead's own instructions carry a short version of the same step so it does not depend on the skill being allocated, conditional in its wording on the issue actually linking a PRD, and the plan prompt now asks the submitted plan to name that step, so a human approving a plan sees that the repo's spec file changes too. Both layers are instructions to a model rather than enforcement: the merge request stays the check on whether a PRD update is honest (PRD #72 M3). See [docs/skills.md](docs/skills.md).
- **A run started with repo agents now carries the run's delivered skills.** Allocations key on your agent templates and a repo's `.claude/agents/` roster has none, so repo subagents previously received no delivered skill at all: a run started with agents from git silently lost every skill its owner had allocated. Each repo subagent now receives exactly the run's materialized skill union, which is the same set the lead already receives and no superset of it. Per-template scoping is unchanged on template runs. The trade, recorded rather than discovered later: a repo-authored subagent can now read every delivered skill body and could write it into the branch, accepted because skill bodies are user data and never secrets by product policy (PRD #72 M1). See [docs/repo-agents.md](docs/repo-agents.md).
- **The issue's own PRD link is corrected after the merge.** When an issue run reports that it moved its PRD to `prds/done/`, uzi rewrites that link in the issue description once the run's merge request has merged, so the link a human clicks still resolves against the default branch. It is edge-triggered (once, then never again, so it does not fight a later human edit) and only ever repoints a `prds/*.md` link the description already carried, matched on the moved file's name, so a link to a different PRD is untouched. The bound is that a run cannot introduce a link, not that it cannot pick among the ones already there: an issue linking several PRDs can have the wrong one repointed if the run declares a matching filename, which costs description integrity on that one issue and nothing wider. `ci_fix` and `self_improve` runs are excluded at both the write and the read side. Adds `UpdateIssueDescription` to the forge interface and both drivers (PRD #72 M5). See [docs/autopilot.md](docs/autopilot.md).

### Changed

- `/api/ws` now accepts a Bearer CLI token (`uzc_`/`uza_`) as well as a browser session cookie, so a headless client can subscribe to a run's live event stream; per-run authorization and the socket's origin check are unchanged (PRD #112 M1).
- **The `uzi` CLI strips more from untrusted text before printing it, which changes the output of existing commands** — not only the new `uzi tui`. `uzi run logs`, `uzi run get`, `uzi review show`, `uzi review backlog` and the disposition tables now also remove DEL (`0x7f`) and every Unicode format character (category `Cf`: the bidi overrides `U+202A`–`U+202E`, the isolates `U+2066`–`U+2069`, `U+200F`, zero-width spaces and joiners, the BOM, and the soft hyphen). Previously only C0 (except tab and newline) and C1 were removed, so a bidi override could visually reorder a judge's `target` or an agent's label into something it is not, and zero-width runes could silently consume a table column's width budget while drawing nothing. Printable text is unaffected, and `--json` output is byte-exact as before. If you script against the human tables, this removes characters that were previously passed through (PRD #112 M3).
- **Consequence of the above, stated because it is visible:** `U+200D` (zero-width joiner) is itself a format character, so **emoji ZWJ sequences decompose** in CLI output — a family emoji renders as its component people, a profession emoji as its parts. This affects all of the commands listed above, not only `uzi tui`. It is the accepted cost of rejecting the whole `Cf` category rather than an allowlist: an allowlist of "safe" format characters is a list somebody has to keep correct, and getting it wrong reopens the bidi-override spoof (PRD #112 M3).

## [0.11.6] - 2026-07-25

### Added

- A run whose updates cannot be saved is now flagged **looping** with a reason that names the cause ("the agent's updates can't be saved, so it keeps resending them") instead of falling through to the 45-minute "taking longer than usual" wall clock. The signal is the API's own count of failed writes for that run, so unlike the existing loop detector it is not blind to a broken message stream (PRD #108).
- A confirmed per-run write loop is **stopped automatically**, in about a minute, under a conjunction of guards that must all hold — including "other runs on this instance are successfully saving messages", so an API-wide outage stops nothing. Such a run ends `failed` with the new stop kind `auto_stopped`, which `uzi run get` shows as a `STOP_KIND` row and the web styles as breakage rather than as a stop you asked for. Operators can turn the stop off with `UZI_AUTOSTOP_ENABLED=false`, which leaves the flag working (PRD #108). See [docs/run-auto-stopped.md](docs/run-auto-stopped.md).
- `GET /api/admin/cli-tokens` / `uzi admin cli-tokens` lists every CLI token in the factory with its owner — closing a gap `workers` hasn't had since PRD #42, where a token was visible to its own holder and nobody else. Read-only (no admin revoke) and never returns the token value or its hash, at four independent layers (query projection, generated row type, DTO, handler). The row does carry `last_used_ip`, so an `admin_ro` holder can see every user's source IPs — deliberate, since `admin_ro` is factory-wide read, but worth knowing before minting one (PRD #98). See [docs/cli.md](docs/cli.md#commands).
- The Slack health nudge now forks on cause: a persistence loop reads "⚠ This run's updates aren't being saved, so it keeps re-sending them", distinct from the pre-existing "⚠ This run looks like it's repeating the same step" (PRD #108).

### Changed

- `uzi worker list` gains a **VERSION** column. The operator page's first remedy for an auto-stopped run is "check the worker's version", and the web has rendered it since PRD #42 — so the page shipped a remedy one of its two audiences could not follow (PRD #108).

### Fixed

- The **docker worker tier** now gets the same values-gated egress allow to uzi's own in-cluster `web` Service that the restricted tier already had. PRD #87 M5 landed the rule in `worker-networkpolicy.yaml` only, so `web-ux` on a docker-capable worker had no target to drive — found by running PRD #87's M7 gate for real, where the browser launched correctly but every request to the UI timed out. Off by default; `dev-cluster` opts in (PRD #87 M5, MR !105).

### Known limitations

- Auto-stop needs at least one *other* run saving messages at the same time to act. On an instance running a single run at a time it will flag and notify but never stop — the correct behaviour on insufficient evidence, and the reason the flag, not the stop, is the value on that deployment shape (PRD #108).
- A run failing because the **worker itself** is sending malformed requests is flagged but never auto-stopped, because a worker defect affects every run that image touches and hiding them one at a time would hide the pattern. The remedy is to roll the worker image; the log line carries `failure_class=invalid` (PRD #108).

## [0.11.5] - 2026-07-25 [NOT RELEASED]

Version burned; nothing was published under it. The tag was pushed against the wrong commit (a bare-clone root's stale detached `HEAD`, carrying `Chart.yaml` 0.9.0). The tag pipeline's `publish:assert-version` job compares chart `version`/`appVersion` against the tag and blocks **all** pushes on a mismatch, so no image and no chart reached Harbor — the guard did exactly what it exists for. The tag is protected and could be neither deleted nor moved, so 0.11.5 is skipped rather than reused. Its intended payload shipped as 0.11.6.

## [0.11.4] - 2026-07-24

### Changed

- The agent worker's Claude Agent SDK moves 0.3.201 → 0.3.219, so `opus` resolves to **Opus 5** rather than the previous generation. The builtin agent templates pin `model: opus` (ten of the eleven roles; `documenter` is sonnet) and the alias is resolved by the `claude` binary the SDK bundles — so runs on a role that pins `opus` change model with this release, without any template edit.
- `openssl` is baked into the default worker toolchain instead of being a per-repo tier-2 package. It is a broadly useful base crypto/TLS CLI rather than a repo-specific extra, so every worker gets it without a repo opting in. (Specifically `openssl.bin` — the bare `openssl` attribute installs no CLI, which failed the image build guard with exit 127.)

## [0.11.3] - 2026-07-22

### Fixed

- The `Select` UI primitive no longer discards a caller's `className`, so the per-worker Anthropic token picker on the Workers page is styled like every other field instead of rendering as an unstyled native `<select>`. `Input` and `Textarea` already merged correctly; only `Select` was broken (issue #118).
- The your-usage card's "see per-run detail →" link no longer orphans its arrow onto a line of its own at narrow widths (issue #117).

## [0.11.2] - 2026-07-22

### Changed

- Meter colour thresholds move to **warn ≥40%**, **danger ≥85%** (from 80/95), so a rate-limit budget reads amber well before it is nearly gone. The status pill is decoupled from the bar: only a ≥95% window escalates it to "nearly out", and an 85–94% row keeps a green "Live" pill while the bar carries the red. A dedicated ≥95% screen-reader announcement keeps that escalation audible now that the danger tone steps at 85 (PRD #115). Busy workers now sit amber or red as their steady state — an accepted trade for the earlier warning.

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
