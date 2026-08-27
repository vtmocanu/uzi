# PRD #361: Per-repo Setup indicator + queued-run reason for un-allowlisted Docker repos

**Issue**: [#361](https://github.com/vtmocanu/uzi/issues/361)
**Status**: Complete (M1–M5 landed on agent/issue-361)
**Priority**: Medium (discoverability / operability; the failure is a silent stuck run, not data loss)

> This PRD is self-contained for an offline worker: every change is internal to this repo, needs no live cluster and no open-web access. Facts below were gathered from this repo's source (cited by symbol + path at `main`; line numbers are approximate, cite the symbol if the tree has moved) and reviewed by three agents (scope, code-fact accuracy, risk/design) whose corrections are folded in. **No database migration is required** (see M2). Acceptance is by Go unit / live-DB tests and web vitest, plus a rendered-UI check. Only standing non-local dependency: `sqlc generate` needs sqlc in the Go module cache (this repo's normal query-edit workflow).

## Problem

The Docker-worker repo allowlist (`docker_repo_allowlist`) is a global admin setting keyed by **repo id** (`api/internal/settings/settings.go:106`, `KeyDockerRepoAllowlist`). A repo re-added to uzi - e.g. after moving a fork from one forge to another - gets a **new** repo id and therefore silently drops out of the allowlist. This happened live: after migrating this dev repo from GitLab to GitHub and re-adding it, it was no longer Docker-allowed and nobody noticed.

Two gaps compound:

1. **The Repos page shows nothing about it.** A repo can read "Enabled" and "Trusted" and still be un-runnable by Docker workers, because allowlist membership lives on a separate admin surface (`AdminSettings` -> "Docker workers", `DockerAllowlistCard` in `web/src/pages/AdminSettings.tsx`) and never appears on the repo row. More generally, a repo's optional capabilities (skills, repo instructions, tool profile, Docker) are scattered across the Repos-page modals and the admin settings, with no at-a-glance readiness view.
2. **A blocked run gives no reason.** If the only online workers are Docker-capable, a run on a non-allowlisted repo can never be claimed (see the claim gate below), and it sits `queued` indefinitely with no explanation of why.

## Background: current behaviour (code facts)

**The claim gate.** `fn_worker_can_claim(is_docker, allowlist, run_repo_id, run_kind)` (migration `00113_fleet_aware_claim.sql:3-27`) is:

```
NOT is_docker OR (run_repo_id IS NULL AND run_kind = 'judge') OR run_repo_id = ANY(allowlist)
```

So a **non-Docker** worker can claim any repo (short-circuits `NOT is_docker`); a **Docker** worker can claim a repo-bearing run only if the repo id is in the allowlist; an empty allowlist is fail-closed; judge/repo-less runs are exempt. Consequence that shapes M2: **a run on a non-allowlisted repo is NOT unconditionally stuck** - a non-Docker worker would still take it. It is stuck only when **every online worker is a Docker worker that fails this predicate for the repo** (allowlist membership aside, no eligible worker exists at all - independent of whether any has a free slot). The full claim gate (`ClaimRun`, `runtime.sql:537-589`) adds resume affinity, the fleet-aware spread, and per-worker cap on top of this predicate; the reason resolver reuses only the **eligibility** predicate, so it is a best-effort reason, not a claim-gate-exact one.

**Allowlist read surface.** `settings.Cache.DockerRepoAllowlist(ctx) ([]uuid.UUID, error)` (`settings.go:739`) returns the parsed id set (always non-nil); `KeyDockerRepoAllowlist` at `settings.go:106`; default empty (`DefaultDockerRepoAllowlist`, `settings.go:190`). `workersvc` already reads it at claim time via the `DockerAllowlistReader` interface, wired on the `*Service` at `api/cmd/server/main.go:422` (`SetDockerAllowlist`, `workersvc/service.go:888`; reader iface `:760`; read at `:1061`). The `handler.Handler` carries `settings *settings.Cache` (`handler.go:56`), so `h.settings.DockerRepoAllowlist(ctx)` is reachable in the repo handlers. The list can hold repos owned by *other* admins, so `DockerAllowlistCard` never renders it as repo names to a non-owner - but a boolean "is THIS repo (the caller's own) allowlisted" leaks nothing, since the caller already owns the repo.

**The queued-run reason resolver already exists.** The PRD #47 run-health detector runs once per sweep tick and, for a queued run past its threshold, computes a human reason via `queuedReason(ctx, userID)` (`api/internal/workersvc/health.go:480`), most-actionable-first:

1. owner vault locked -> `reasonVaultLocked` (`:481`)
2. no online worker at all (`CountOnlineWorkersForUser` == 0) -> `reasonNoWorker` (`:489`)
3. every online worker at its cap (`CountOnlineWorkersWithFreeSlotForUser` == 0) -> `reasonAllWorkersBusy` (`:497`)
4. else -> `reasonWaitingWorker` (`:500`)

All queued arms of `healthTargetFor` return the **same** `healthWaitingWorker` flag enum (`health.go:243,245,247`; enum `="waiting_worker"` at `:48`); only the free-text `health_reason` string differs (reason constants `health.go:57-107`). `runs.health_reason` has **no CHECK constraint** - migration `00057_run_health.sql:33` constrains only `runs.health`; `health_reason` is bare `text` (`:34`), and no later migration adds a CHECK. So adding a new reason string owes **no migration** - the precedent `reasonAllWorkersBusy` (PRD #216) and `reasonDeprioritized`/`reasonRestored` (PRD #320) set. The queued arm is `case "queued":` at `health.go:224`. Worker-count queries: `runtime.sql:2151` (`CountOnlineWorkersForUser`) and `:2157` (`CountOnlineWorkersWithFreeSlotForUser`, whose "online + NULL-cap-or-under-cap, run-lane status" shape M2 clones).

**But the health row lacks the two columns M2 needs.** `ListActiveRunsForHealth` (`runtime.sql:2109-2112`) selects `id, user_id, status, auto_approve, started_at, last_activity_at, updated_at, health, health_reason, health_since, health_notified_at, budget_wall_seconds` - **no `repo_id`, no `kind`**. The new branch needs both, so M2 must grow that query by two columns and regenerate (it changes the row struct the whole health pass reads).

**The reason already reaches every consumer.** `health_reason` is an owner-gated field on the run DTO (`apitypes/run.go:126`, `json:"health_reason"`; `web/src/lib/api.ts` `health_reason: string | null`). It surfaces on all five run views, though via two mechanisms: `RunsList`, `Dashboard` and `ActivityFeed` render the `RunHealthBadge` component (`web/src/components/RunHealthBadge.tsx`); `Board` renders it as a badge tooltip through `runBadge()` -> `badgeTitle(health_reason)` (`runBadge.ts:288,345`; `Board.tsx:1673`); `RunView` renders it inline (`RunView.tsx:154`). Crucially the badge/CLI switch on **no vocabulary** (`badgeTitle` just strips + returns; `cmd/uzi/run.go:1178` comment "switching on no vocabulary"), so a new reason string flows through with no consumer change. The CLI already prints it: `uzi run` shows a `HEALTH_REASON` detail row (`api/cmd/uzi/run.go:1190`) and a stderr warning while watching (`run.go:1045`). **So a new reason string surfaces on web and CLI with zero consumer-side change.**

**The Repo DTO and the handlers that build it.** `RepoDTO` (`api/internal/apitypes/repo.go:6`) carries `Enabled`, `RepoSkillsEnabled`, `RepoClaudemdEnabled` (`:19`), `RepoDevboxOptIn` (`:23`), `GuardrailOverride` (`:31`), `GuardrailBlocked` (`:41`). The pure mapper `repoToDTO(store.Repo)` (`forge.go:124`) maps stored fields only; it does **not** set `GuardrailBlocked` - that is computed by `guardrailBlockedForRepo` (`forge.go:110`, from the stored privilege report + the per-repo `GuardrailOverrideReason` flag; **no settings-cache read**) and merged in the handlers. Two handlers build the merged DTO:

- **`ListProjects`** (`forge.go:486`, per-repo loop `:546-551`, sets `GuardrailBlocked` at `:549`) - this is the handler the **Repos page** calls (`web/src/pages/Repos.tsx:95`, `api.listProjects(connId)`), so it feeds the M4 chip. **All computed fields must be added here or the chip gets nothing.**
- **`ListRepos`** (`forge.go:558`, loop `:591-596`, sets `GuardrailBlocked` at `:594`) - `GET /api/repos`, **enabled-only** (`ListEnabledReposForUser`), feeds the sidebar picker and the CLI `uzi repo list` (`uzicli/client.go:902`).

There is **no `GetRepo` handler**. `PatchRepo` (`forge.go:732`) returns a bare `repoToDTO` carrying neither `guardrail_blocked` nor the new fields (pre-existing), so a row refreshed via PATCH will not carry them - acceptable, the page refetches via `ListProjects`. The web `Repo` type is at `api.ts:346` (mirrors the fields + `pipeline`, `guardrail_override`, `guardrail_blocked`). Repos page: `isTrusted` at `Repos.tsx:123`; the Enabled/guardrail badge `<td>` at `Repos.tsx:404`; Trusted + Tools modals follow. CLI `uzi repo list` prints `ID`, `PATH`, `ENABLED` only (`repo.go:35,37`).

## Solution

Two independent additions, both small:

- **A queued-run reason** (backend only): a new branch in `queuedReason` that fires when a run's repo is not on the Docker allowlist and **no online worker is eligible to claim it** (every online worker is a Docker worker failing `fn_worker_can_claim` for the repo), producing a fixed, owner-only reason string. Reuses the `healthWaitingWorker` enum (no migration) and the `fn_worker_can_claim` eligibility predicate. It reaches web + CLI for free.
- **A per-repo Setup indicator** (web + computed DTO fields): a neutral "Setup" chip on each enabled repo row whose hover/click popover lists the four optional capabilities (repo skills, repo instructions, tool profile, Docker workers) as on/off with a one-line explainer and where each is set, framed as *optional, off by default for safety* - never as "missing". The chip stays neutral by default and escalates to an **info** tone (sky, not a red warning) only when a run on that repo is actually blocked (`docker_blocked`, computed from the same eligibility logic, not from a config gap), so a repo sitting happily on defaults never nags. A "Warning on anything not maxed out" design is explicitly rejected (see Decision log): it produces alarm fatigue and pressures users to flip security opt-ins to silence it.

Design reference: the approved interactive mock (ember-themed Repos page, neutral-vs-warning toggle, stuck-run strip) produced during brainstorming; this PRD implements its "Neutral chip + escalate only when blocked" behaviour.

## Milestones

Milestone numbers follow **build order**. Dependency / parallelization map:

| Phase | Milestone | Depends on | Files touched (shared files annotated) |
|---|---|---|---|
| 1 (parallel) | M1 RepoDTO `docker_allowlisted` | - | `apitypes/repo.go`, `handler/forge.go` (**shares with M3**), `web/src/lib/api.ts` (**shares with M3**), `cmd/uzi/repo.go`, `apitypes/wire_test.go` |
| 1 (parallel) | M2 queued-run Docker reason | - | `workersvc/health.go`, `store/queries/runtime.sql` (**shares with M3**) |
| 2 | M3 `docker_blocked` escalation field | M2 | `handler/forge.go`, `store/queries/runtime.sql`, `web/src/lib/api.ts` |
| 3 | M4 Setup chip (web) | M1, M3 | `web/src/pages/Repos.tsx`, new `web/src/components/RepoSetupChip.tsx` |
| 4 | M5 Docs + integration pass | M1-M4 | `ARCHITECTURE.md`, `docs/` |

M1 and M2 are genuinely parallel-safe (fully file-disjoint). M3 re-touches M1's exact lines in `handler/forge.go` and `web/src/lib/api.ts` and M2's `runtime.sql`, so it is sequenced after both, not run beside them. Each milestone owns its own tests in its acceptance clause; M5 is docs + a final cross-cutting green-gate/live-DB pass, **not** where the unit tests live (so a partial delivery of M1/M2 ships *with* its tests, never gated behind a later phase).

### M1 - RepoDTO gains `docker_allowlisted` (backend + wire + web type + CLI)
- [x] Add `DockerAllowlisted bool` with JSON tag `docker_allowlisted` to `RepoDTO` (`apitypes/repo.go`), commented as a computed, caller-scoped boolean (not the list).
- [x] Compute it in **both** `ListProjects` (`forge.go:549`) and `ListRepos` (`forge.go:594`) loops, alongside `GuardrailBlocked`, by membership-testing the repo id against `h.settings.DockerRepoAllowlist(ctx)` read **once per request** (not per repo). Leave `repoToDTO` and `PatchRepo` unchanged (they omit computed fields, matching `guardrail_blocked`).
- [x] Add the JSON-tag assertion for the new field to `apitypes/wire_test.go` (sibling of `TestGuardrailOverrideDTOTags`, `:449`).
- [x] Add `docker_allowlisted: boolean` to the web `Repo` type (`api.ts:346`) and to the repo mock fixtures (`web/src/mocks/`).
- [x] CLI parity (repo convention): add a `DOCKER` column to `uzi repo list` (`cmd/uzi/repo.go:35`) rendering `boolStr(r.DockerAllowlisted)` (gate behind `--wide` if the row gets too wide).
- **Acceptance / tests**: an allowlisted repo returns `docker_allowlisted:true` and a non-allowlisted one `false` from `ListProjects` (handler or live-DB test); wire-tag test green; a one-line test asserts a non-owner never receives the field for another user's repo (owner-scoping); `uzi repo list` shows the column.

### M2 - Queued-run reason: "repo not on the Docker worker allowlist" (backend health detector)
- [x] Add a fixed, server-controlled reason constant near `health.go:57-107`, e.g. `reasonRepoNotDockerAllowed = "this repo isn't on the Docker worker allowlist, so no Docker worker can run it"` - no repo content, no tool name, no live duration; maps to the existing `healthWaitingWorker` enum, **no migration**.
- [x] Grow `ListActiveRunsForHealth` (`runtime.sql:2109`) by `repo_id, kind`; `sqlc generate`; thread them through `healthTargetFor`'s queued arm (`health.go:224`). A NULL `repo_id` (judge/repo-less run) must fall straight through and never trip the new branch.
- [x] Add one store query counting the caller's **online, eligible** workers for a repo, **ignoring free slots**: shape on `CountOnlineWorkersForUser` (`runtime.sql:2151`) plus `AND fn_worker_can_claim(COALESCE(w.docker_enabled,false), @allowlist::uuid[], @repo_id::uuid, @kind::text)`. Cast the params exactly as `ClaimRun` passes them (`runtime.sql:546`) so a green `sqlc generate` is not mistaken for a working query. Read the allowlist from `s.dockerAllowlist`/settings (already on the `*Service`, `main.go:422`); nil-guard and degrade to the generic wait reason on nil or read error (matching the sibling resolvers).
- [x] Add the branch: fire `reasonRepoNotDockerAllowed` when `CountOnlineWorkersForUser > 0` AND the repo is not allowlisted AND the **eligible-worker count (ignoring slots) is 0**. Order it **after** `reasonNoWorker`, **before** `reasonAllWorkersBusy`: only when zero workers are eligible at all is the run genuinely unrunnable without allowlisting (a busy-but-eligible non-Docker worker means the honest reason is still "all workers busy", so that case must fall through - this is the corrected predicate, distinct from "eligible AND free-slot == 0").
- **Acceptance / tests**: a `*LiveDB` test (the only proof the new query runs - a green `sqlc generate` is not evidence, see `.claude/rules/go.md`) covering: (a) Docker-only fleet + non-allowlisted repo -> gets the reason; (b) **mixed busy fleet** - a busy eligible non-Docker worker + a free Docker worker, repo not allowlisted -> does **not** get the reason (falls to all-busy); (c) repo allowlisted -> falls through; (d) judge/repo-less run -> never trips it.

### M3 - `docker_blocked` escalation field (backend, per-repo) [depends on M2]
- [x] Add a computed `DockerBlocked bool` (`docker_blocked`) to `RepoDTO` + web `Repo` type, computed **from eligibility directly** - NOT by matching the `workersvc`-private reason string (which would duplicate an unexported const across packages: the retire-a-string trap, and it would inherit the sweeper's `health_enabled`/600s-threshold gating, so the chip would stay neutral for the first 10 minutes of a real block or whenever health is disabled).
- [x] Define `docker_blocked` = repo is enabled AND not allowlisted AND the caller has >= 1 **queued** run on this repo AND **zero online workers are eligible** for the repo (the same eligibility notion as M2, reused). Implement as one aggregate query returning the caller's docker-blocked repo ids; mark the field in the same `ListProjects`/`ListRepos` loops as M1.
- **Acceptance / tests**: `docker_blocked` is `true` exactly for a repo with a live queued run and zero eligible workers and no allowlist entry; `false` when a run is claimable (eligible worker exists), when no run is queued, or when allowlisted. If this milestone is deferred, M4 must degrade to a neutral-only chip (never escalate) rather than break.

### M4 - Repos page Setup chip + popover (web) [depends on M1, M3]
- [x] New `web/src/components/RepoSetupChip.tsx`: a focusable chip with a custom popover (hover + focus-within + click-to-pin; **not** a native `title` tooltip). It lists four capabilities with on/off marks, a one-line explainer, and where each is set: **Repo skills** (`repo_skills_enabled`, Trusted repo settings), **Repo instructions** (`repo_claudemd_enabled`, Trusted repo settings), **Tool profile** (`repo_devbox_opt_in`, Tools modal), **Docker workers** (`docker_allowlisted`, Admin -> Docker workers, tagged `admin`). Footer: "Defaults are off on purpose; each one widens what an agent may do."
- [x] Tone rules: neutral/muted by default; **info** (sky) when `docker_blocked`; a quiet **ok** "Ready" when all four are on. Never red/amber. Copy frames off capabilities as *optional*, never "missing". The Docker row, when off and `docker_blocked`, says a queued run is waiting and points to Admin -> Docker workers.
- [x] Wire into the Repos row badge cell (`Repos.tsx:404`), only for `enabled` repos (matching the existing `!r.enabled` guards). Respect `prefers-reduced-motion`; visible keyboard focus; use ember tokens / the `Badge` tone system.
- **Acceptance / tests**: vitest for `RepoSetupChip` covering the three tones and the on/off combinations; a `Repos.tsx` test that the chip appears only for enabled repos and reflects the flags. Guard against a vacuous negative assertion (see `.claude/rules/web.md`): pair any "warning tone absent" check with a positive assertion on the neutral/info tone actually present. Validate in a real browser under `VITE_UZI_MOCK=1` (rendering only).

### M5 - Documentation + integration pass
- [x] Note the Setup indicator and the new queued reason where the repo/admin surfaces are described (`ARCHITECTURE.md` forge/board section and/or the relevant `docs/*.md`), including the re-add-loses-allowlist-seat failure mode. Respect `docs/` frontmatter rules (`web/scripts/check-docs.mjs` runs in the web build).
- [x] Final cross-cutting pass: `task gate:api`, `task gate:web`, `task gate:controller` (unaffected, must stay green), and the store-it live-DB sweep (`./e2e/run-store-it.sh`) all green together.
- **Acceptance**: docs build passes; all gates green.

## Success criteria

1. A queued run that no Docker worker can claim because its repo is not allowlisted shows a clear, owner-only reason on the board, runs list, run view, and `uzi run`, distinct from "no worker online" and "all workers busy", and does **not** fire when an eligible (non-Docker) worker exists but is merely busy.
2. Every enabled repo row shows a neutral Setup chip whose popover accurately reflects the four capability states and where to change each; the chip escalates to info only when a run is actually blocked, and never renders a red/amber warning.
3. No database migration is introduced; the new reason reuses the `waiting_worker` health enum.
4. The reason and `docker_blocked` reuse the `fn_worker_can_claim` **eligibility** predicate rather than re-implementing it (best-effort on the full claim gate, exact on eligibility).
5. All gates green: `task gate:api`, `task gate:web`, `task gate:controller`, and the live-DB store-it sweep.

## Out of scope (explicitly)

- **Auto-adding a re-added repo back to the allowlist.** Membership is an admin security decision (Docker workers reach a root Docker daemon); the indicator surfaces the gap, it does not silently widen the allowlist.
- **Reworking the admin Docker-workers allowlist UI** (`DockerAllowlistCard`). Unchanged; the chip only links to it.
- **A general capability-readiness score or a repos-wide "N repos need setup" banner.** The chip is per-row and per-capability; no aggregate nag.
- **Turning any default-off capability on by default.** The skills/instructions/devbox/Docker opt-ins keep their deliberate off defaults.
- **Making the reason claim-gate-exact** (reproducing affinity/spread/cap). It is a best-effort eligibility-based reason.
- **A single-repo DTO endpoint / `PatchRepo` carrying computed fields.** Pre-existing gap for `guardrail_blocked`; not widened here.

## Risks and mitigations

- **Risk**: exposing allowlist membership leaks another admin's repos. **Mitigation**: `docker_allowlisted` is a boolean about the *caller's own* repo, computed server-side in owner-scoped handlers (`ListProjects`/`ListRepos` resolve repos via `…ForUser` queries); the list itself is never sent. Covered by the M1 non-owner test.
- **Risk**: the new `queuedReason` branch mislabels an ordinary wait (the mixed-busy-fleet case). **Mitigation**: the corrected predicate keys on zero *eligible* workers ignoring slots, so a busy-but-eligible worker falls through to all-busy; covered by M2 negative test (b). Gated behind the existing queued-threshold guard (evaluates for ~zero runs/tick); read errors degrade to the generic wait reason.
- **Risk**: `docker_blocked` coupling to the sweeper's written string or thresholds. **Mitigation**: M3 computes it from eligibility directly, independent of `health_reason` text and `health_enabled`/`health_queued_seconds`.
- **Risk**: the chip becomes an alarm users learn to ignore or that pushes them to flip security toggles. **Mitigation**: neutral by default, escalate only on a real block, info-not-warning tone, optional-framed copy. The rejected "warning on everything" variant is recorded.
- **Risk**: the M2/M3 queries pass `sqlc generate` but Postgres rejects them at prepare time (parameter-type deduction). **Mitigation**: cast params as `ClaimRun` does; mandatory `*LiveDB` test executes them.
- **Risk (handoff)**: this is a multi-milestone feature; a bug-triage sweep is a poor fit. **Mitigation**: routed to the general **Night-Shift** sweep (empty guidance) instead; M1 and M2 are each independently valuable and shippable alone, so a partial delivery still lands a coherent unit (M2, backend-only, is a natural first landing).

## Decision log

- **2026-08-18**: Filed after re-adding this dev repo (migrated GitLab -> GitHub) left it off the Docker allowlist unnoticed, and a would-be Docker run had no way to say why it was stuck. Brainstormed a per-repo indicator; built and approved an interactive ember-themed mock.
- **2026-08-18**: Chose a **neutral info chip that escalates only on a real block** over the original "exclamation mark on anything not fully set up." Rationale: three of the four capabilities are deliberately-off security opt-ins, so a warning icon on every fresh repo produces alarm fatigue and nudges users to weaken the default posture; the mock demonstrated this by toggling to a "Warning" variant that lit up a perfectly healthy repo. Scope set to indicator + stuck-run reason.
- **2026-08-18**: Reason rides the existing PRD #47 `queuedReason` resolver and the `healthWaitingWorker` enum, so **no migration** and the reason reaches web + CLI with no consumer change (the badge/CLI switch on no vocabulary). Eligibility reuses `fn_worker_can_claim` rather than re-deriving it.
- **2026-08-18**: `docker_allowlisted` exposed as a caller-scoped boolean on `RepoDTO` (not the list), computed like `guardrail_blocked`. CLI `uzi repo list` gains a Docker column.
- **2026-08-18 (review pass)**: Three-agent review corrected four load-bearing points folded into this revision: (1) the Repos page is served by **`ListProjects`** (not `ListRepos`, and there is no `GetRepo`), so the computed fields must land there or the chip gets nothing; (2) the M2 predicate must key on **zero eligible workers ignoring free slots**, else a busy non-Docker worker mislabels a run as Docker-blocked; (3) `ListActiveRunsForHealth` lacks `repo_id`/`kind` and must grow them; (4) `docker_blocked` is computed from eligibility directly, not by matching the `workersvc`-private reason string. Also softened "cannot disagree with the claim gate" to eligibility-only/best-effort. Routed the handoff to the general **Night-Shift** sweep rather than the bug-triage sweep.
