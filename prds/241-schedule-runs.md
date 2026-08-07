# PRD #241: Scheduled runs (one-time + recurring; pinned issue, label sweep, or ad-hoc prompt) from web + CLI

**GitLab Issue**: [#241](https://gitlab.example.com/vtmocanu/uzi/-/issues/241)
**Status**: Draft (created 2026-08-07; mock approved by owner same day; reviewed same day by an architect agent — concurrency/replica claim corrected to single-instance, the "inherited for free" seam boundary narrowed, sweep eligibility/consent fixed. **Expanded same day at owner's direction**: the sweep target generalized to a label selector (Decision 9), and a third target added — an issue-less repo→MR **ad-hoc prompt**, a new run kind (Decision 10). Re-reviewed by an architect agent, which corrected the prompt kind from the `self_improve` shape to the **`ci_fix` shape** — repo-ful + issue-less — and caught both kind CHECKs, the schedule-keyed dedup NULL gap, the FK delete semantics, and the `#null` MR hazard. See the Decision Log.)
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
- **Target** is one of three:
  - a **pinned issue** (one repo + IID);
  - a **label sweep** — at fire time, every open issue on the repo matching a label selector
    that *also* passes the run-creation gate; the selector defaults to the PRD label (Decision 9);
  - an **ad-hoc prompt** — a stored prompt run against the repo with **no issue**, opening an
    MR; a new run kind on the `self_improve` shape (Decision 10).

Schedules are managed from a new web **Schedules** surface (list page + create/edit modal
+ an issue-view "Schedule…" entry point) and a `uzi schedule …` CLI, each a peer consumer
of one set of API endpoints. A **scheduler goroutine** — built on the exact shape the
`selfimprove` engine already uses (a wake ticker over a durable "due" gate) — claims due
schedules with `FOR UPDATE SKIP LOCKED` and fires each. **Issue** and **label-sweep** targets
fire **through the same shared run-creation seam autopilot uses** (`workersvc.CreateRun` /
`CreateAutopilotRun`), inheriting every existing gate — PRDLESS bypass, fresh forge issue
fetch, active-run dedup (`HasActiveRunForIssue`), the usage-limit park — so for those two,
**a schedule can do nothing a manual start cannot.** The **ad-hoc prompt** target is the
deliberate exception: it has no issue, so it cannot use that issue-anchored seam; it uses a
**dedicated INSERT on the `self_improve` precedent** (`CreateSelfImproveRun`, which exists
precisely "because the normal path requires the issue to be in the poller cache and to carry
a PRD link"), and thereby **bypasses the PRD-issue sanction gate by design** (Decision 10).
The four main-protection guardrail layers (`main` never touched) hold for all three targets.

One new Go dependency (a cron parser, Decision 3), one new table (`run_schedules`), a nullable
`runs.schedule_id` for provenance/dedup, and one new `runs.kind` (the ad-hoc prompt, Decision
10). No new service, no new trust boundary — schedules fire as their owner, on the owner's
Anthropic token and forge PAT, subject to the same secrets and main-protection guardrails a
manual run is.

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
- **Label sweep.** "Every Monday 09:00, run every issue labeled `bug`" (Decision 9). The
  selector picks candidates; the gate still picks what runs, so this pairs with PRDLESS for
  raw bug reports.
- **Ad-hoc prompt, no issue filed.** "Every Monday 09:00, run this prompt against `repo`:
  *hunt for flaky tests and open an MR*." The prompt lives on the schedule; each fire is an
  issue-less repo→MR run (Decision 10) — the "standing prompt" use case without the throwaway
  forge issue, at the cost of a new run kind that bypasses the PRD-issue gate.
- **No-Renovate dependency sweep.** "We don't have Renovate — find outdated dependencies, bump
  them, refactor for any breaking changes, and make sure CI passes." A recurring ad-hoc prompt
  (Decision 10) against the repo, opening an MR; CI runs on the MR branch, and the existing
  `ci_fix` autopilot can even pick up any failures. Exactly the issue-less standing-work case
  the prompt target exists for.

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
| `target` (`issue` \| `sweep` \| `prompt`) | pinned issue vs label sweep vs ad-hoc prompt |
| `issue_iid` (bigint, only for `issue`) | pinned issue |
| `labels` (jsonb, only for `sweep`) | label selector; empty ⇒ the PRD label (Decision 9) |
| `prompt` (text, only for `prompt`) | stored task text copied into each fired run (Decision 10) |
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

Plus a nullable **`runs.schedule_id`** (FK to `run_schedules`) on the `runs` table itself —
provenance ("which schedule fired this run") and the dedup key for the prompt target, whose
runs have no issue to key `HasActiveRunForIssue` on (Decision 10).

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

### Decision 9 — The sweep target carries a label selector; default is the PRD label (recommended)

Owner request: also schedule "run every issue labeled `bug`", weekly. This is **not** a new
target — it is the **sweep** target with a label filter. `run_schedules.labels` (jsonb) holds
zero or more label names; the sweep-candidate query (Decision 7) filters `open ∧ has(all
selected labels)` on the cached `issues.labels` (predicate `labels ?& @labels` / `@>`).
**Empty selector ⇒ the PRD label**, i.e. exactly today's PRD-label sweep, so there is one code
path and the default is unchanged. Note (review): there is **no GIN index on `issues.labels`**,
but a sweep scans one repo's cached issues (a board is hundreds of rows), so it is a bounded
seq-scan — no new index needed; do not claim label-indexing.

The per-issue run-creation gate is **unchanged** and still applies: a label sweep only *fires*
on matching issues that also pass the gate (PRD-labeled with a PRD link, or carrying the
PRDLESS label). So:
- A `bug` sweep over issues that are also PRD issues → fires directly.
- A `bug` sweep over plain bug reports (no PRD) → fires **only if PRDLESS is enabled** and
  those issues carry the PRDLESS label; otherwise each is skipped at the gate.

State this in the UI: a label selector chooses *candidates*; the gate chooses *what actually
runs*. The natural pairing for a raw-bug-report workflow is label-sweep + PRDLESS. (If you want
a label-driven run with **no** gate at all, that is the ad-hoc prompt target, Decision 10, not
a sweep.)

### Decision 10 — Ad-hoc prompt target: an issue-less repo→MR run, a new kind on the `ci_fix` shape (owner-requested; guardrail-adjacent)

Owner request: schedule a **specific prompt** with **no issue** — e.g. weekly "find bugs and
open an MR." The codebase proves this is feasible: two run kinds already create work from a
stored prompt without going through the PRD-issue seam.

- **`kind='chat'`** (`CreateChatRun`, `chat.sql`) stores a raw prompt as `issue_description`
  with `repo_id`/`issue_iid` **NULL** — a prompt with no issue, but conversational and
  repo-less (no clone, no MR).
- **`kind='self_improve'`** (`CreateSelfImproveRun`, `selfimprove.sql`) is repo-ful,
  prompt-as-`issue_description`, `auto_approve`, opens an MR, and uses a **dedicated INSERT,
  not `createRun`** — its own comment: "because the normal path requires the issue to be in
  the poller cache and to carry a PRD link, neither of which [it] has." It is hardcoded to
  uzi's own repo with a global one-active guard.

The ad-hoc prompt target is a new `runs.kind` (working name `prompt`) with `repo_id` set,
`issue_iid` NULL, `issue_description` = the schedule's stored prompt, created by a dedicated
INSERT. **Get the analogy right (review):** it is repo-ful *and issue-less*, which is the
**`ci_fix` shape, not the `self_improve` shape** — `self_improve` carries a real tracking
issue (`issue_iid NOT NULL` under `runs_kind_shape`). So the two sides model on different
precedents:

- **API side** — model the dedicated INSERT **plus the `GetRepoForUser` consent wrapper** on
  `self_improve.go` (the consent check lives in the Go wrapper at `self_improve.go:31`, *not*
  the SQL; the prompt path bypasses `createRun`, where `GetRepoForUser` normally sits, so M3
  must replicate that call or ownership is silently dropped).
- **Worker side** — model kind handling on **`ci_fix`**, the genuinely issue-less precedent.
  With `issue_iid` NULL the current worker throws (`"issue run claim is missing issue_iid"`,
  `runner.ts:1360`) and would otherwise emit `Resolve issue #null` / `Closes #null` MR text
  (`runner.ts:1691,1741`). M8 must add a real `prompt` case to `runnerCloneForClaim`,
  `mrTitle`, and `mrDescription` — this is "add a kind," **not** "reuse the issue path with a
  synthetic task."

Concretely this needs:

1. **Both kind constraints edited** (review): `runs_kind_check` (`kind IN (…)`) **and**
   `runs_kind_shape` (per-kind field shape), both in `00058_run_judge_self_improve_kinds.sql`;
   plus the `RunKind` type (`agent/src/protocol.ts`) and an API kind constant beside
   `RunKindJudge`/`RunKindSelfImprove` (`workersvc/judge.go`). Editing only the shape CHECK
   fails the domain CHECK on first insert.
2. **`runs.schedule_id`** nullable FK **`ON DELETE SET NULL`** (never CASCADE — deleting a
   schedule must not delete run history).
3. **Dedup keyed by schedule.** A partial unique index `ON runs (schedule_id) WHERE
   kind='prompt' AND status NOT IN (terminal)` (the `uq_runs_one_active_self_improve` pattern,
   which keys on `(kind)`), plus a `HasActiveRunForSchedule` early-skip check. Postgres treats
   NULLs as distinct, so the shape CHECK must force `schedule_id IS NOT NULL` for `kind='prompt'`
   or the index is a no-op.
4. **`issue_title`** must be set (truncated prompt or the schedule's label) or the run-view
   header renders blank (`self_improve` borrows its tracking-issue title; a prompt run has none).
5. **Board move is already null-safe for free**: `runlifecycle` skips the card move + terminal
   comment when `issue_iid` is NULL (`lifecycle.go:236-240`, keyed on `IssueIid.Valid`) — the
   `ci_fix` carve-out, inherited, no work needed. **This is the one target that touches the
   `agent` module** (M8), not just `api`.

**Guardrail analysis (the reason this is flagged; verified in review).** This target
**deliberately bypasses the PRD-issue sanction gate** (`isPRDIssue`/`HasPrdLink` inside
`createRun`, `service.go:3108`/`:3115`). That gate is **not** one of the four main-protection
guardrail layers (Developer-role/protected-branch, worker-holds-PAT, `PreToolUse` deny-hook
`agent/src/guardrails.ts`, `settingSources:[]`) — all **untouched**, so `main` is still never
written. `ci_fix` (pipeline failure, no PRD) and `self_improve` (prompt, no PRD) already bypass
this same gate via dedicated INSERTs, so non-PRD runs are established, not new. Consent is repo
ownership (`GetRepoForUser`). What changes is *policy*: the owner can run arbitrary auto-approved
prompts against a repo they own. **One extra sub-risk (review):** `self_improve` MRs get
guard-critical-path flagging (`GUARD_CRITICAL_PATTERNS`, `self-improve.ts`) because they target
uzi's own repo; a `prompt` run pointed at the uzi repo would get **no** such flag — so either
add that flag for prompt runs on the uzi repo, or gate the prompt target away from it. **Owner
sign-off wanted** on the bypass, and on whether the prompt target is admin-gated like
`self_improve` (default-off) or a normal user capability.

## Milestones

### M1 — Schema + store queries (`api`)
- [ ] Migration (draft `00100_run_schedules.sql`; **renamed to the next free number at
  merge** per the goose convention) creating `run_schedules` with the columns in Decision 1
  (incl. `labels` jsonb and `prompt` text), an index on `(enabled, status, next_fire_at)` for
  the claim, and a CHECK enforcing the `target`/`timing` field-presence invariants (e.g.
  `issue_iid` non-null iff `target='issue'`; `prompt` non-null iff `target='prompt'`).
- [ ] Migration adding nullable **`runs.schedule_id`** FK (**`ON DELETE SET NULL`**) and
  editing **both** kind constraints in `00058` — `runs_kind_check` (`kind IN …`) **and**
  `runs_kind_shape` — for the new `prompt` kind (repo-ful, issue-less, `schedule_id` non-null,
  `issue_title` set). Also the `RunKind` type (`agent/src/protocol.ts`) and an API kind constant
  (`workersvc/judge.go` neighborhood) — Decision 10.
- [ ] sqlc queries in a new `queries/schedules.sql`: create, get, list-for-user,
  update (edit + pause/resume), delete, **ClaimDueSchedules** (`FOR UPDATE SKIP LOCKED`,
  single-instance defense-in-depth per Decision 1), and **AdvanceSchedule** (a *separate*
  statement — set `last_fired_at`, recompute or terminate — with retry-without-advance on a
  transient error). Plus a **label-parameterized sweep-candidate query** (Decisions 7, 9), a
  sibling to `ListAutopilotCandidateIssues` without the autopilot-label filter, taking the
  selector labels (default = PRD label), and a **`HasActiveRunForSchedule`** dedup query for the
  prompt target (Decision 10). `sqlc generate` (verify the generated const moved, per
  `.claude/rules/go.md`).
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
- [ ] Sweep path: the **label-parameterized** candidate query (Decisions 7, 9) +
  `HasActiveRunForIssue` dedup; pinned path: single issue with the same dedup. Do **not** wire
  autopilot's `eligible()` consent gate.
- [ ] Prompt path (Decision 10): a dedicated INSERT **+ `GetRepoForUser` consent wrapper**
  modeled on `self_improve.go` (`kind='prompt'`, repo-ful, prompt-as-description, `schedule_id`
  set, `issue_title` derived), dedup via `HasActiveRunForSchedule`. Does **not** touch the issue
  seam or the forge issue APIs.
- [ ] **Skip-vs-advance rule** (review): when a fire is skipped because the prior run is still
  live, `AdvanceSchedule` still advances `next_fire_at` to the next cadence and drops this fire
  (per Decision 8) — do **not** hold a past `next_fire_at` (tick-storm) nor queue a second run
  (double-fire). Applies to all targets; sharpest for prompt (schedule-keyed dedup).
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
  owner-scoped; create validates cron/tz and the per-target fields (issue_iid / labels /
  prompt) via M2 + Decisions 9-10.
- [ ] DTOs in `apitypes` (schedule row + next-fires preview), and a `runsForLimiter`-style
  decision on whether create should sit behind the forge per-user limiter (it does a forge
  read on `run-now`/pinned validation) — match `CreateRun`'s limiter posture.
- [ ] Handler tests: auth (owner-only), validation (bad cron → 400), shape.

### M5 — Web: Schedules surface (`web`)
- [ ] `/schedules` page (list table per the mock §1) + a `NavItem` in the **Work** group
  with an optional enabled-count badge (mock §4), reusing the existing `NavItem`/badge
  mechanism.
- [ ] Create/edit modal (mock §2): Target (issue / label sweep / prompt) + Timing segmented
  pickers — the label sweep reveals a **label multiselect** (Decision 9), the prompt target a
  **prompt textarea** (Decision 10); preset dropdown + time, Advanced (raw cron + tz), options
  (wait-on-limit, auto-approve), and a **live "Next fires" preview** computed from M2's logic.
- [ ] Issue-view **"Schedule…"** action beside "Start run" (mock §3), opening the modal
  pre-pinned to that issue.
- [ ] `api.*` methods in `web/src/lib/api.ts` + matching `web/src/mocks/mockApi.ts` (mock
  parity, typechecked against `realApi`), so `VITE_UZI_MOCK=1` demos the surface from
  fixtures. Vitest coverage for the modal's mode/target switching and the preview.

### M6 — CLI parity (`api/cmd/uzi`)
- [ ] `uzi schedule` command group: `create` (`--repo`, one of `--issue`|`--sweep`|`--prompt`,
  `--label` (repeatable, with `--sweep`), `--at`|`--cron` `--tz`, `--auto-approve`,
  `--wait-on-limit`), `list`, `get`, `pause`, `resume`, `run-now`, `delete` — with `--json` and
  documented exit codes, matching the existing `uzi run` command's conventions. (Mandatory per
  the "new functionality ⇒ check `api/cmd/uzi/`" convention.)
- [ ] Command tests mirroring `run.go`'s (`commands_test.go` style).

### M7 — Docs, specs, gates
- [ ] `docs/` page (audience: user) for scheduling, and a `docs/cli.md` update for the
  `uzi schedule` group; `web/scripts/check-docs.mjs` stays green.
- [ ] `specs/ai.md` records Decisions 1–10; `specs/human.md` only with owner approval.
- [ ] ARCHITECTURE.md: add the scheduler as the time-driven run origin (a peer of autopilot
  in "Agent runtime") and the `prompt` run kind beside `chat`/`ci_fix`/`self_improve`.
  **Consider an ADR** (`adr/0241-…`) only if the scheduler seam or the prompt-kind gate-bypass
  proves to be a durable invariant other code must respect — the ADR set is deliberately small;
  default to the PRD Decision Log otherwise. (The Decision 10 gate-bypass is a plausible ADR
  candidate.)
- [ ] All gates green: `task gate:api` (incl. `-race`, ratcheted lint, deadcode-at-zero),
  `task gate:web`, `task gate:agent` (M8 touches it), `task gate:repo`.

### M8 — Worker support for the ad-hoc prompt kind (`agent`)
- [ ] The worker claims and executes `kind='prompt'` runs (Decision 10): task = the run's
  `issue_description`, no issue reference. **Model on the issue-less `ci_fix` path, not
  `self_improve`** (which carries a tracking issue). Add a real `prompt` case to
  `runnerCloneForClaim` (branch derivation), `mrTitle`, and `mrDescription` (`runner.ts`) — the
  issue defaults produce `#null` MR text otherwise — and add `prompt` to the `RunKind` type.
- [ ] Guardrails unchanged: the `PreToolUse` deny-hook and PAT/branch protections apply
  identically — this kind adds a *trigger*, not a new capability. Consider the guard-critical-
  path flag (`GUARD_CRITICAL_PATTERNS`) if a prompt run targets the uzi repo. `task gate:agent`
  green.

## Success Criteria

1. A user can create, from **both** web and CLI: one-time and recurring schedules targeting a
   **pinned issue**, a **label sweep** (selector defaulting to the PRD label), or an **ad-hoc
   prompt** (issue-less repo→MR run).
2. Due schedules fire through the shared run-creation seam, so PRDLESS gating, fresh-label
   forge fetch, active-run dedup, and usage-limit park behave exactly as for a manual run.
3. Recurring schedules survive an api restart and never double-fire or backfill missed
   cadences (Decision 8); one-time schedules fire exactly once.
4. The Schedules page and `uzi schedule list` show the same schedules with correct
   next/last-fire times; the modal's "next fires" preview matches what actually fires.
5. Auto-approve is opt-in and, when off, a scheduled run stops at the plan gate like a
   manual run; `main` is never written under any path.
6. A label sweep fires only on matching issues that also pass the run-creation gate
   (Decision 9); an ad-hoc prompt run opens an MR with no issue, keys dedup on the schedule,
   and has `PreToolUse`/PAT/branch guardrails identical to any other run (Decision 10).
7. `VITE_UZI_MOCK=1` demos the full surface from fixtures; all gates green.

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
- **Ad-hoc prompt bypasses the PRD-issue sanction gate (Decision 10).** Mitigated: the four
  main-protection guardrail layers are untouched (`main` safe), consent is repo ownership, and
  non-PRD runs already exist (`ci_fix`, `self_improve`). Residual *policy* risk flagged for
  owner sign-off; consider admin-gating the prompt target like `self_improve` (default-off).
- **Label sweep over-promises.** A selector picks candidates but the gate picks what runs
  (Decision 9); mitigated by stating it in the UI, so a `bug` sweep over non-PRD issues without
  PRDLESS does not look broken when it fires nothing.
- **New run kind in `runs_kind_shape` + worker.** Mitigated by modeling exactly on
  `self_improve`/`chat` (both already issue-less-shaped) and by the M8 worker milestone.

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
- New: one Go dependency (`robfig/cron/v3`); one table (`run_schedules`) + migrations; a
  nullable `runs.schedule_id` FK (`ON DELETE SET NULL`); one new `runs.kind` (`prompt`) editing
  **both** `runs_kind_check` and `runs_kind_shape` plus the `RunKind` type; one background
  actor; and worker support for the prompt kind modeled on `ci_fix` (`agent`, M8).
- No new service, trust boundary, or secret. Fires as the schedule's owner on existing
  credentials; the prompt target bypasses the PRD-issue *sanction* gate (Decision 10) but no
  main-protection guardrail.

## Parallelization

- **Phase 1 (parallel):** M1 (schema/store) and M2 (cron engine — pure, no DB) are
  independent and touch different files.
- **Phase 2:** M3 (scheduler, needs M1+M2) and M4 (endpoints, needs M1) can overlap once M1
  lands; different files (poller-sibling vs handler).
- **Phase 3 (parallel):** M5 (web) against the mock + DTO contract; M6 (CLI) after M4; M8
  (agent worker for the prompt kind) is independent of web/CLI and runs alongside — it needs
  only the `kind='prompt'` contract from M1/M3.
- **Phase 4:** M7 (docs/specs/ARCHITECTURE) after the surface stabilizes.

| Phase | Milestone | Repo/module | Depends on | Files (primary) |
|---|---|---|---|---|
| 1 | M1 | api | — | `store/migrations/00100_*` (+ `runs.schedule_id`/kind CHECK), `store/queries/schedules.sql` |
| 1 | M2 | api | — | `internal/schedsvc/*` (+ `go.mod`) |
| 2 | M3 | api | M1, M2 | `internal/schedsvc` actor (issue/sweep/prompt fire paths), process startup |
| 2 | M4 | api | M1 | `internal/handler/*`, `handler.go` routes, `apitypes` |
| 3 | M5 | web | contract/mock | `pages/Schedules.tsx`, `components/AppShell.tsx`, `pages/IssueView.tsx`, `lib/api.ts`, `mocks/mockApi.ts` |
| 3 | M6 | api (cmd/uzi) | M4 | `cmd/uzi/schedule.go` |
| 3 | M8 | agent | M1, M3 (kind contract) | `agent/src/*` (claim + execute `prompt` kind) |
| 4 | M7 | docs + specs | M3–M6, M8 | `docs/`, `specs/ai.md`, `ARCHITECTURE.md` |
