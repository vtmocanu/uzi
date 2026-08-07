# PRD #241: Scheduled runs (one-time + recurring; pinned issue or eligible sweep) from web + CLI

**GitLab Issue**: [#241](https://gitlab.example.com/vtmocanu/uzi/-/issues/241)
**Status**: Draft (created 2026-08-07; mock approved by owner same day; reviewed same day by an architect agent against the codebase — concurrency/replica claim corrected to single-instance, the "inherited for free" seam boundary narrowed to what the seam actually does, and sweep eligibility/consent fixed. See the Decision Log.)
**Priority**: Medium
**Mock**: `prds/mockups/241-schedule-runs-mock.html` (approved 2026-08-07)

## Problem

A run can only start **now**. There are exactly three ways to create one today, and all
three are immediate or event-driven:

- The board / issue-view **"Start run"** button (`web/src/pages/IssueView.tsx` →
  `api.createRun` → `POST /api/repos/{id}/runs`).
- The CLI **`uzi run create --repo --issue`** (`api/cmd/uzi/run.go`).
- **Autopilot** (PRD #19), which is *label-driven*: when the autopilot label appears on
  an eligible issue, the post-sync detector (`api/internal/poller/autopilot.go`) starts a
  run. It reacts to a forge event, not to the clock.

Nothing is **time-driven**. An operator cannot say "run this issue tomorrow at 09:00" or
"every weekday at 02:00, start runs on all eligible PRD issues on this repo." For a dark
factory whose whole premise is unattended AI labor, the off-hours window — nights,
weekends, the gap between rate-limit resets — is exactly when you'd most want work to
start, and it is the one window the product cannot currently use.

## Solution Overview

Add a **time-driven** run origin alongside manual and autopilot. A **schedule** is a
persisted, owner-scoped intent to create run(s) at future time(s):

- **Timing** is either **one-time** (fires once at a timestamp, then goes terminal) or
  **recurring** (a cron cadence in a named timezone).
- **Target** is either a **pinned issue** (one repo + IID) or a **sweep** (at fire time,
  every currently-eligible PRD issue on the repo — the same eligibility autopilot uses).

Schedules are managed from a new web **Schedules** surface (list page + create/edit modal
+ an issue-view "Schedule…" entry point) and a `uzi schedule …` CLI, each a peer consumer
of one set of API endpoints. A **scheduler goroutine** — built on the exact shape the
`selfimprove` engine already uses (a wake ticker over a durable "due" gate) — claims due
schedules with `FOR UPDATE SKIP LOCKED` and fires each **through the same shared
run-creation seam autopilot uses** (`workersvc.CreateRun` / `CreateAutopilotRun`). That
seam is the load-bearing reuse: every existing gate — PRDLESS bypass, fresh forge issue
fetch, active-run dedup (`HasActiveRunForIssue`), and the usage-limit park — behaves
identically no matter what triggered the run. **A schedule can never do something a manual
start cannot.**

One new Go dependency (a cron parser, Decision 3) and one new table. No new service, no
new trust boundary — schedules fire as their owner, on the owner's Anthropic token and
forge PAT, subject to the same secrets and guardrails a manual run is.

### Motivating use cases

- **Standing "find bugs" / "improve tests" runs.** Put the prompt in a long-lived issue
  ("hunt for bugs across the poller package and open MRs", "raise coverage in `forgesvc`")
  and attach a *recurring, pinned-issue* schedule that **never closes the issue**. Each fire
  opens an MR (never touching `main`); the active-run dedup (Decision 2) skips a fire while
  the previous run is still working, so a nightly hunt never piles up. These want
  auto-approve on (Decision 4) to proceed unattended. Caveat: the issue must still satisfy
  the run-creation gate — a PRD-labeled issue with a PRD link, or one carrying the PRDLESS
  label — exactly as a manual start requires.
- **Overnight backlog sweep.** "Every weekday 02:00, start runs on every eligible PRD issue"
  — burn down the backlog in the off-hours / rate-limit-reset window.
- **Deferred one-shot.** "Run this issue tomorrow 09:00" when you want it started, but not now.

## Design Decisions

### Decision 1 — A dedicated `run_schedules` table + a scheduler goroutine (recommended)

Rejected alternative: overload `runs` with a `scheduled_for` column and let the sweeper
promote future rows to `queued`. That conflates two lifecycles (a *schedule* recurs and
outlives any run it spawns; a *run* is a single execution) and would put recurring/cron
state on a table whose state machine has no room for it.

**Recommended:** a new `run_schedules` table, and a scheduler that is a sibling background
actor to the poller/sweeper/selfimprove engine. Proposed columns (final names/types at
implementation):

| Column | Purpose |
|---|---|
| `id` (uuid) | schedule id |
| `user_id` (uuid) | owner; the run fires as this user (token + PAT) |
| `repo_id` (uuid) | target repo (forge connection) |
| `target` (`issue` \| `sweep`) | pinned issue vs eligible-issue sweep |
| `issue_iid` (bigint, null for sweep) | pinned issue |
| `timing` (`once` \| `recurring`) | one-shot vs cron |
| `cron_expr` (text, null for once) | 5-field cron (recurring) |
| `run_at` (timestamptz, null for recurring) | the single fire time (once) |
| `timezone` (text) | IANA tz the cron is interpreted in (DST-correct) |
| `next_fire_at` (timestamptz) | **the durable due-gate** — indexed, drives claiming |
| `last_fired_at` (timestamptz, null) | last time it fired |
| `auto_approve` (bool) | Decision 4 |
| `wait_on_limit` (bool) | park on usage limit instead of failing (mirrors the run flag) |
| `enabled` (bool) | pause/resume without deleting |
| `status` (`active` \| `fired` \| `error`) | Decision 5 |
| `created_at` / `updated_at` | audit |

The scheduler loop mirrors `selfimprove.Engine.Run` (`api/internal/selfimprove/engine.go`):
a `time.Ticker` wake cadence, and on each tick a claim of rows where
`enabled AND status='active' AND next_fire_at <= now()`, firing each then advancing
`next_fire_at`. Like **every** existing background actor here (poller, sweeper, selfimprove,
privcheck, usage — all wired single-instance in `cmd/server/main.go:465-535`), the scheduler
runs **single-instance**, not behind leader election. `FOR UPDATE SKIP LOCKED` on the claim
is defense-in-depth (and lets AdvanceSchedule be a separate statement with
retry-without-advance on a transient error, mirroring selfimprove's `skip` path at
`engine.go:274-297`); it is **not** what makes the design multi-replica-safe. The real
backstop against a *duplicate run* — should two ticks or two replicas ever race — is the
seam's **one-active-run-per-issue unique index** (`ErrActiveRunExists`, `service.go:3158`)
plus `HasActiveRunForIssue`, not the row lock. Genuine multi-replica scheduling would need a
lease column (`claimed_by`/`claimed_until`), machinery that exists nowhere else in this
codebase; out of scope unless the owner requires it.

### Decision 2 — Fire through the shared run-creation seam, replicating the poller's caller-side scaffolding (recommended)

The scheduler must **not** re-implement run creation. It calls the same
`*workersvc.Service` methods autopilot calls (`api/internal/workersvc/service.go:2952`,
`:2975`): `CreateAutopilotRun(ctx, userID, repoID, issueIID, description, allowWithoutPRD)`
when the schedule is auto-approve, and
`CreateRun(ctx, userID, repoID, issueIID, description, allowWithoutPRD, waitOnLimit, nil)`
when it is not.

But be precise about the seam boundary — the review found the first draft overclaimed it.
The seam does **not** fetch from the forge or compute PRDLESS; those are *caller-side*, and
the scheduler must replicate exactly what the poller's autopilot detector does before it
calls the seam (`poller/autopilot.go:198-210`). Per pinned/swept issue the scheduler must:

1. Load the connection with `GetRepoForUser(repoID, userID)` — which *also* enforces that the
   schedule owner owns the repo (our consent check, Decision 7).
2. Build a forge driver from the stored connection (`forgesvc.ForgeForConnection`,
   `forgesvc/service.go:168`); the scheduler takes a `ForgeBuilder` dependency, exactly as
   the selfimprove engine already does (`selfimprove/engine.go:83-85`).
3. `f.GetIssue(...)` for the fresh title/body/labels.
4. Compute `allowWithoutPRD` from settings + the fresh labels (PRDLESS bypass, PRD #22
   Decision 3), as the handler does at `handler/workers.go:756-779`.
5. Call the seam.

What the seam then **genuinely inherits** (the real reuse): the PRD-label gate (read from
the *cache*), the PRD-link gate (via the `allowWithoutPRD` bool), the
**one-active-run-per-issue unique index** and `HasActiveRunForIssue` dedup — so a schedule
never double-starts an issue with a live run (critical for sweeps and for a recurring
schedule whose previous run is still going) — the description cap, the
`auto_approve`/`wait_on_limit` stamping, and the queued→in-progress notify. That is the
architectural move autopilot's `RunStarter` interface documents: "keeping run creation on the
workersvc side is what makes an autopilot run and a manual run share one state machine and one
set of gates." A scheduled run becomes a third caller of that one seam.

Narrow phrasing (review N4): the PRD-**label** gate reads *cached* labels, so a just-added PRD
label is not seen until the next cache refresh (same as the manual button); only the *PRDLESS*
label is read fresh. Acceptable, but the PRD does not claim more.

### Decision 3 — Add `github.com/robfig/cron/v3` for parsing + next-fire computation (recommended)

There is **no** cron/scheduling library in `api/go.mod` today. Recurring schedules need to
(a) validate a cron string, (b) compute `next_fire_at` in a given IANA timezone (DST-correct),
and (c) render "next N fires" for the modal preview and CLI. Hand-rolling a cron parser is
a classic underestimate (DST, `L`/`#`/step semantics, leap handling).

**Recommended:** `github.com/robfig/cron/v3` — the de-facto Go cron library, maintained,
`cron.ParseStandard` for 5-field expressions, and `Schedule.Next(t)` for the next fire. We
use only its **parser/schedule** types (`cron.ParseStandard`, `Schedule.Next`), *not* its
in-process `cron.Cron` runner — our durable `next_fire_at` gate is the runner, so the
schedule survives restarts (replica-dedup is covered in Decision 1, not by the library).
Timezone/DST mechanism (review N3): construct the probe time in the target IANA location
(`time.LoadLocation`) before calling `Next`, then persist `next_fire_at` as UTC — or use
robfig's `CRON_TZ=` spec prefix; the M2 DST test guards whichever is chosen. This is the one
new dependency; flag for review. (Presets in the UI are a thin translation layer to/from cron
strings, Decision 6.)

### Decision 4 — Auto-approve is opt-in per schedule, reusing autopilot's `auto_approve` (the one guardrail-adjacent choice)

`runs.auto_approve` already exists (true for autopilot/selfimprove runs, false for manual).
A scheduled run that fires at 02:00 and **stops at the plan-approval gate** waits until a
human shows up — which defeats "work overnight." But **auto-approving a run nobody is
watching** is a trust decision, not a default to make silently.

**Recommended:** a per-schedule `auto_approve` toggle, **defaulting off** (safe: the run
plans and then waits at the gate, exactly like a manual run), with the modal and docs
making explicit that turning it on is what makes unattended runs actually proceed, and that
it reuses the *same* semantics autopilot already runs under (so it is not a new privilege,
just a new trigger for an existing one). The primary directive is untouched either way:
`main` is never written, and all four guardrail layers (PRD guardrails section) still apply
— auto-approve only skips the *plan* gate, not any guardrail. **Owner sign-off wanted on the
default.**

### Decision 5 — One-time schedules go terminal, not hard-deleted (recommended)

The mock's legend says a one-time schedule "auto-removes" after firing. Interpreted as:
after a `once` schedule fires, it moves to `status='fired'` and drops out of the default
(active) list view — **not** a row delete. Keeping the row preserves provenance (which run
came from which schedule, `last_fired_at`) and lets the UI offer a "past / fired" filter.
Hard-delete loses that link the moment it would be most useful (debugging an unexpected
overnight run). A "Clear fired" bulk action can prune later. **Recommended: soft-terminal.**

### Decision 6 — Presets are a translation layer over cron; the raw field is the source of truth (recommended)

The modal shows friendly presets (Weekdays / Every day / Every week / Every N hours) + a
time picker, and an **Advanced** disclosure with the raw cron string + timezone. Presets
map **to** a cron string on selection; editing the raw field that no longer matches a preset
flips the dropdown to "Custom." Storage is always the cron string + tz (never the preset
label), so the CLI (`--cron`) and web agree byte-for-byte and there is one canonical form to
parse and to compute `next_fire_at` from. This is the "presets + advanced cron" shape the
owner chose.

### Decision 7 — Sweep eligibility: a PRD-label-only sibling query; consent is repo ownership, NOT autopilot's gate (recommended)

Two corrections the review forced (the first draft got both wrong):

- **The query.** `ListAutopilotCandidateIssues` (`store/queries/autopilot.sql:6-28`) filters
  on the **autopilot label AND the PRD label** — `open ∧ has(autopilot_label) ∧
  has(prd_label)`, with **no** user mapping in the query itself. Reusing it verbatim would
  make a sweep fire only on issues that *also* carry the autopilot label — nearly redundant
  with autopilot (clock vs. label-event). Since a sweep means "every **eligible PRD**
  issue," it needs a **PRD-label-only sibling** query (`open ∧ has(prd_label)`), then
  `HasActiveRunForIssue` dedup per issue. One canonical eligibility, expressed as the
  PRD-label predicate — not the autopilot one.
- **Consent.** Do **not** reuse autopilot's `eligible()` / `GetAutopilotConnectionContext`
  path (`poller/autopilot.go:340-352`): it infers consent from the label-adder/author
  matching the repo owner's `human_username`, which is meaningless for a schedule. A
  schedule's consent is direct and already enforced — the owner created it and must own the
  repo, checked by `GetRepoForUser(repoID, userID)` inside the seam (`service.go:3080`,
  returns `ErrRepoNotFound` otherwise). No attribution inference.

### Decision 8 — Missed fires: fire once on the next wake, never backfill (recommended)

If the api is down across a fire time (or a tick is slow), the durable `next_fire_at` is
already in the past on the next wake, so the schedule fires **once**, promptly — mirroring
selfimprove's "a cycle that came due while the process was down fires promptly instead of
one wake-cadence later." A recurring schedule does **not** replay every cadence it missed
during an outage (no thundering backfill); it fires once and computes the next future
`next_fire_at`. A `once` schedule long overdue still fires once, then goes terminal.

For "promptly" to actually hold after a restart, the scheduler must run one **immediate tick
on boot** — a `Boot()` step wired in `main.go`, exactly as `selfimprove.Engine.Boot` is at
`engine.go:132-135` (wired `main.go:513`); without it, "promptly" degrades to
up-to-one-wake-cadence latency after a restart (review N2).

## Milestones

### M1 — Schema + store queries (`api`)
- [ ] Migration (draft `00100_run_schedules.sql`; **renamed to the next free number at
  merge** per the goose convention) creating `run_schedules` with the columns in Decision 1,
  an index on `(enabled, status, next_fire_at)` for the claim, and a CHECK enforcing the
  `target`/`timing` field-presence invariants (e.g. `issue_iid` non-null iff `target='issue'`).
- [ ] sqlc queries in a new `queries/schedules.sql`: create, get, list-for-user,
  update (edit + pause/resume), delete, **ClaimDueSchedules** (`FOR UPDATE SKIP LOCKED`,
  single-instance defense-in-depth per Decision 1), and **AdvanceSchedule** (a *separate*
  statement — set `last_fired_at`, recompute or terminate — with retry-without-advance on a
  transient error). Plus a **PRD-label-only sweep-candidate query** (Decision 7), a sibling
  to `ListAutopilotCandidateIssues` without the autopilot-label filter. `sqlc generate`
  (verify the generated const moved, per `.claude/rules/go.md`).
- [ ] Live-DB test (`*LiveDB`) for the claim/advance round-trip across `once` and
  `recurring`, proving the query runs against real Postgres.

### M2 — Cron / next-fire engine (`api`, pure)
- [ ] New `api/internal/schedsvc` (name TBD): add `robfig/cron/v3` (Decision 3); functions
  to validate a cron string, compute `next_fire_at` for `(cron, tz)` and for a `once`
  `run_at`, and render the next N fires.
- [ ] Preset↔cron translation helpers (Decision 6), shared so web/CLI/api agree.
- [ ] Unit tests including a **DST boundary** case (a 02:30 daily cron across a spring-forward
  day in `Europe/Bucharest`) and an invalid-cron rejection. No DB, fast.

### M3 — Scheduler goroutine (`api`)
- [ ] A **single-instance** background actor modeled on `selfimprove.Engine` (ticker +
  durable gate + a `Boot()` immediate tick, Decision 8), taking a `ForgeBuilder` dependency
  like the selfimprove engine: claim due schedules, fire each, then AdvanceSchedule.
- [ ] Per-issue firing replicates the poller's caller-side steps (Decision 2): `GetRepoForUser`
  (also enforces owner-owns-repo, Decision 7) → `ForgeForConnection` → `f.GetIssue` for fresh
  labels/body → compute `allowWithoutPRD` → call the seam. Cite `poller/autopilot.go:198-210`
  as the template.
- [ ] Sweep path: the **PRD-label-only** candidate query (Decision 7) + `HasActiveRunForIssue`
  dedup; pinned path: single issue with the same dedup. Do **not** wire autopilot's
  `eligible()` consent gate.
- [ ] Error handling: a schedule whose owner/token/repo is gone goes `status='error'`
  (surfaced, not silently dropped); a transient forge/DB error is logged and retried next
  tick without advancing.
- [ ] Wire the actor into `cmd/server/main.go` beside the poller/sweeper/selfimprove
  (single-instance, matching them).
- [ ] Live-DB test: seed a due schedule, run one tick, assert a run was created via the seam
  and `next_fire_at` advanced (recurring) / `status='fired'` (once).

### M4 — API endpoints + DTOs (`api`)
- [ ] `GET /api/me/schedules` (list, owner-scoped), `POST /api/repos/{id}/schedules`
  (create), `GET/PATCH/DELETE /api/schedules/{id}`, and `POST /api/schedules/{id}/run-now`
  (fire immediately through the seam). Mounted on `RequireUser` (so a CLI token works),
  owner-scoped; create validates cron/tz/target via M2.
- [ ] DTOs in `apitypes` (schedule row + next-fires preview), and a `runsForLimiter`-style
  decision on whether create should sit behind the forge per-user limiter (it does a forge
  read on `run-now`/pinned validation) — match `CreateRun`'s limiter posture.
- [ ] Handler tests: auth (owner-only), validation (bad cron → 400), shape.

### M5 — Web: Schedules surface (`web`)
- [ ] `/schedules` page (list table per the mock §1) + a `NavItem` in the **Work** group
  with an optional enabled-count badge (mock §4), reusing the existing `NavItem`/badge
  mechanism.
- [ ] Create/edit modal (mock §2): Target + Timing segmented pickers, preset dropdown +
  time, Advanced (raw cron + tz), options (wait-on-limit, auto-approve), and a **live "Next
  fires" preview** computed from M2's logic (client-side mirror or a small preview call).
- [ ] Issue-view **"Schedule…"** action beside "Start run" (mock §3), opening the modal
  pre-pinned to that issue.
- [ ] `api.*` methods in `web/src/lib/api.ts` + matching `web/src/mocks/mockApi.ts` (mock
  parity, typechecked against `realApi`), so `VITE_UZI_MOCK=1` demos the surface from
  fixtures. Vitest coverage for the modal's mode/target switching and the preview.

### M6 — CLI parity (`api/cmd/uzi`)
- [ ] `uzi schedule` command group: `create` (`--repo`, `--issue`|`--sweep`,
  `--at`|`--cron` `--tz`, `--auto-approve`, `--wait-on-limit`), `list`, `get`, `pause`,
  `resume`, `run-now`, `delete` — with `--json` and documented exit codes, matching the
  existing `uzi run` command's conventions. (Mandatory per the "new functionality ⇒ check
  `api/cmd/uzi/`" convention.)
- [ ] Command tests mirroring `run.go`'s (`commands_test.go` style).

### M7 — Docs, specs, gates
- [ ] `docs/` page (audience: user) for scheduling, and a `docs/cli.md` update for the
  `uzi schedule` group; `web/scripts/check-docs.mjs` stays green.
- [ ] `specs/ai.md` records Decisions 1–8; `specs/human.md` only with owner approval.
- [ ] ARCHITECTURE.md: add the scheduler as the time-driven run origin (a peer of autopilot
  in "Agent runtime"). **Consider an ADR** (`adr/0241-…`) only if the scheduler seam proves
  to be a durable invariant other code must respect — the ADR set is deliberately small;
  default to the PRD Decision Log otherwise.
- [ ] All gates green: `task gate:api` (incl. `-race`, ratcheted lint, deadcode-at-zero),
  `task gate:web`, `task gate:agent` (untouched), `task gate:repo`.

## Success Criteria

1. A user can create, from **both** web and CLI: a one-time schedule (fires once at a
   timestamp then goes terminal) and a recurring schedule (cron cadence in a chosen tz),
   targeting either a pinned issue or an eligible-issue sweep.
2. Due schedules fire through the shared run-creation seam, so PRDLESS gating, fresh-label
   forge fetch, active-run dedup, and usage-limit park behave exactly as for a manual run.
3. Recurring schedules survive an api restart and never double-fire or backfill missed
   cadences (Decision 8); one-time schedules fire exactly once.
4. The Schedules page and `uzi schedule list` show the same schedules with correct
   next/last-fire times; the modal's "next fires" preview matches what actually fires.
5. Auto-approve is opt-in and, when off, a scheduled run stops at the plan gate like a
   manual run; `main` is never written under any path.
6. `VITE_UZI_MOCK=1` demos the full surface from fixtures; all gates green.

## Risks & Mitigations

- **New cron dependency.** Mitigated by using only the parser/schedule types of a
  well-established library (Decision 3) and pinning it; our own durable gate is the runner.
- **Sweep thundering herd.** A 02:00 sweep can enqueue many runs at once. Mitigated because
  runs land in `queued` and workers claim with `SKIP LOCKED` (worker capacity is the natural
  limiter) and dedup prevents duplicates; an optional per-sweep cap or per-user schedule cap
  is a deliberate NICE follow-up, not M-scope.
- **Sweep forge fan-out.** A sweep issues N `GetIssue` calls at fire time, and the scheduler
  (a background actor) is not behind the HTTP forge per-user limiter. Mitigate with per-tick
  pacing and note the rate-limit exposure (review N1).
- **Unattended auto-approve.** Mitigated by opt-in default-off (Decision 4) and by the
  guardrail layers being independent of the plan gate; called out for owner sign-off.
- **Timezone/DST correctness.** Mitigated by storing IANA tz + delegating next-fire to the
  cron library, with an explicit DST unit test (M2).
- **Second definition of "eligible."** Mitigated by reusing autopilot's candidate predicate
  (Decision 7) instead of inventing one.

## Testing Strategy

- **api**: `*LiveDB` tests for claim/advance (M1) and one full scheduler tick creating a run
  via the seam (M3), run through `./e2e/run-store-it.sh`; pure unit tests incl. DST for the
  cron engine (M2); handler tests for auth/validation/shape (M4).
- **web**: vitest on modal mode/target switching + preview and mockApi parity (M5).
- **cli**: command tests mirroring `run.go` (M6).
- **manual**: `VITE_UZI_MOCK=1 npm run dev` to walk the surface against the approved mock.

## Dependencies

- Reuses: the run-creation seam (`workersvc.CreateRun`/`CreateAutopilotRun`), autopilot's
  dedup + candidate predicate (`HasActiveRunForIssue`, `ListAutopilotCandidateIssues`), the
  selfimprove engine's ticker + durable-gate pattern, the `NavItem`/badge mechanism, and the
  `RequireUser` endpoint pattern.
- New: one Go dependency (`robfig/cron/v3`), one table + migration, one background actor.
- No new service, trust boundary, or secret. Fires as the schedule's owner on existing
  credentials.

## Parallelization

- **Phase 1 (parallel):** M1 (schema/store) and M2 (cron engine — pure, no DB) are
  independent and touch different files.
- **Phase 2:** M3 (scheduler, needs M1+M2) and M4 (endpoints, needs M1) can overlap once M1
  lands; different files (poller-sibling vs handler).
- **Phase 3 (parallel):** M5 (web) can start against the mock + the agreed DTO contract; M6
  (CLI) needs M4's endpoints. Separate toolchains.
- **Phase 4:** M7 (docs/specs/ARCHITECTURE) after the surface stabilizes.

| Phase | Milestone | Repo/module | Depends on | Files (primary) |
|---|---|---|---|---|
| 1 | M1 | api | — | `store/migrations/00100_*`, `store/queries/schedules.sql` |
| 1 | M2 | api | — | `internal/schedsvc/*` (+ `go.mod`) |
| 2 | M3 | api | M1, M2 | `internal/schedsvc` actor, process startup |
| 2 | M4 | api | M1 | `internal/handler/*`, `handler.go` routes, `apitypes` |
| 3 | M5 | web | contract/mock | `pages/Schedules.tsx`, `components/AppShell.tsx`, `pages/IssueView.tsx`, `lib/api.ts`, `mocks/mockApi.ts` |
| 3 | M6 | api (cmd/uzi) | M4 | `cmd/uzi/schedule.go` |
| 4 | M7 | docs + specs | M3–M6 | `docs/`, `specs/ai.md`, `ARCHITECTURE.md` |
