# PRD #503 — Run termination classification + cancel/reject reason capture

**Issue**: [#503](https://github.com/vtmocanu/uzi/issues/503)
**Priority**: Medium
**Status**: Draft — ready for implementation

> **Path convention**: every path is relative to the repo root. The change lives in the **api** module's termination/steering lane — `api/internal/workersvc/service.go`, `api/internal/workersvc/judge_enqueue.go`, `api/internal/store/queries/runtime.sql` (+ regenerated `store/*.sql.go`), `api/cmd/uzi/run.go`, `api/internal/handler/workers.go` — plus **one** new nullable-column migration (M3). No web, no controller, no agent (`agent/`) code change (the worker-side threading already works). Load `.claude/rules/go.md` before starting.
>
> **This PRD is destined for an offline uzi worker.** Every fact below was verified on 2026-08-21 with its `file:line`. The unit tests (`failorigin_test.go`, a new `judge_enqueue` test, `run.go` cobra tests) are in-process and offline; the SQL-transition coverage runs on the local throwaway Postgres via `./e2e/run-store-it.sh` (Docker, no internet). It touches **no** file under `.github/workflows/**` (`.claude/rules/prds.md`).
>
> **User-facing behavior change** → **adds a CHANGELOG `[Unreleased]` line**. The new migration number is a **draft** (`00141` at time of writing); rename it to the next free number above the live head at the landing rebase (goose numbers are assigned at merge time — CLAUDE.md convention; and note PRD #500, if it lands first, adds a `check:migration-numbering` gate that will flag a collision).

## Problem

Three defects in how a run's **termination** is classified and how **steering reasons** are captured:

- **REC A (seen in 4 runs) — cancel/reject on the live path is mis-classified as `agent_failure`.** A worker **cannot report a `cancelled` terminal status** (`Service.SetState`'s switch has no `cancelled` case), so when a **live worker** consumes a cancel or reject verdict the run ends via the **`failed`** arm, which defaults `fail_origin` to **`agent_failure`**. So an operator cancellation during planning ends `failed` + `agent_failure`, gets judged (contradicting the "cancelled runs are not judged" decision), and is attributed to the agent — polluting per-agent reliability signal. The server-side reject path correctly stamps `plan_rejected`, but the **live** reject path does not: the same default hits both cancel and reject.
- **REC B (seen in 4 runs) — cancel reason is never captured.** `uzi run cancel` has **no `-m/--message` flag** (hardcoded empty body), the TUI fires a bare y/n confirm, and even a supplied body is dropped by `CancelRunServerSide` (takes only id + user_id); there is **no `runs` column** for a cancel reason. So a cancel preserves no signal about why the operator stopped the run. (The rec's second half — "persist reconnaissance before teardown" — is **already covered** by PRD #333's incidental findings, so it is scoped out here.)
- **REC C (seen in 1 run, high confidence) — reject rationale is optional and dropped.** `uzi run reject`'s `-m/--message` is **optional** (an empty reason passes), and the **server-side** reject path drops it entirely (`RejectRunServerSide` is called with a hardcoded `"plan rejected"`). So a rejection can terminate a run at 0 iterations with no learnable signal. (The rec's "thread it back so the agent can revise" is **already** `uzi run revise` — an in-place re-plan without stopping — so REC C reduces to: require a reject reason, and stop dropping it server-side.)

## Solution

One PRD, three milestones over the shared termination/steering lane:

- **M1 (REC A)**: in `SetState`'s `failed` arm, consult the run's already-loaded `stop_kind`. When `stop_kind='cancelled'`, route the worker's `failed` report to a **`cancelled` transition** (status `cancelled`, `fail_origin` NULL) instead of `SetRunFailed` — converging the live path with the server-side path and making "cancelled runs are not judged" true uniformly, with **no new `fail_origin` vocabulary and no migration** (Option 2, recommended). When `stop_kind='plan_rejected'`, stamp `fail_origin='plan_rejected'` to match the server-side path.
- **M2 (REC C)**: require a non-empty reason on `uzi run reject` (like `revise`/`follow-up`), and pass `body` through to `RejectRunServerSide` so it is persisted as `failure_reason` instead of the hardcoded literal. **No new column.**
- **M3 (REC B)**: add `-m/--message` to `uzi run cancel`, add a new **nullable `runs.stop_reason text`** column (a cancel is not a failure, so overloading `failure_reason` is a smell), and persist the reason on both the server-side (`CancelRunServerSide`) and live (`CreateStopVerdictInput`) cancel paths.

---

## Background — current state (resolved facts)

### The central mechanism: a worker cannot report `cancelled`

`Service.SetState`'s switch (`api/internal/workersvc/service.go`, `func (s *Service) SetState`, ~`:2985`) accepts exactly `running`, `awaiting_approval`, `awaiting_input`, `completed`, `limit_wait`, `failed` — **no `cancelled` case**. So a run whose **live** worker consumes a cancel verdict ends via the `failed` arm:

- `agent/src/executor.ts:711,822` / `sdk-executor.ts:136` (`REASON_CANCELLED="run cancelled"`): a consumed cancel `throw`s.
- `agent/src/runner.ts:~1756-1780`: `rawReason = err instanceof PlanRejectedError ? err.reason : errMessage(err)`; `failOrigin = failOriginForReason(rawReason)`.
- `failOriginForReason` (`runner.ts:74`) matches only `REASON_PROVISION_FAILED`/`REASON_NO_TOKEN`; `"run cancelled"` and any plan-reject reason → **`undefined`**.
- Server `SetState` `case "failed":` (`service.go:3071-3095`): `failOrigin := "agent_failure"` (`:3078`); `CoerceFailOrigin(nil)` → nil, so the default **`agent_failure`** is written via `SetRunFailed`.

Terminal state therefore depends on whether a **live poller** exists (`SubmitInput`, `service.go:4545`, gated by `hasLivePoller`, `:4807`):

| Path | Trigger | terminal `status` | `fail_origin` | `stop_kind` |
|---|---|---|---|---|
| **Live-poller cancel** | worker running/at gate ("during planning") | **`failed`** | **`agent_failure`** | `cancelled` |
| Server-side cancel | run `queued`/`limit_wait`/stale | `cancelled` | NULL | `cancelled` |
| **Live-poller reject** | worker at gate | **`failed`** | **`agent_failure`** | `plan_rejected` |
| Server-side reject | no live poller | `failed` | `plan_rejected` | `plan_rejected` |

The `stop_kind` is stamped by `CreateStopVerdictInput` (`runtime.sql`, CTE ~`:2005`) **before** the worker's failed report, so it is already on the run row when `SetState` runs. REC A's "cancel during planning" is the **live-poller cancel** row: `failed` + `agent_failure`. **REC A's claim is code-confirmed.** The server-side writers stamp in SQL: `RejectRunServerSide` (`runtime.sql:1457`, `fail_origin='plan_rejected'`), `FailRunAutoStop` (`runtime.sql:1412-1450`, `auto_stopped`), `CancelRunServerSide` (`runtime.sql:1374`, `status='cancelled'`, no `fail_origin`).

### Judge impact (REC A)

`judge_enqueue.go`: Gate 0 (`maybeEnqueueJudge`, `:61-64`) judges only `completed`/`failed` — **`cancelled` is never judged** (comment `:55-56`, "Decision 2"). Gate 4b (`:134-135`) skips judge only for `preStartInfraFailOrigins` (provisioning/credential/guardrail) at iter 0. A live-poller cancel is `failed`+`agent_failure`+iter 0, and `agent_failure` is **not** in that set → it **passes and gets judged**, contradicting Decision 2. `claim.go:186` hands the run's `fail_origin` to the judge as `FailureClass`, so the judge is literally told `"agent_failure"` for a user cancellation.

### `failorigin.go` vocabulary

`failOrigins` (`:35-54`, 11 values) + `workerReportableFailOrigins` (`:92`, 6). **No operator/cancel origin.** Cancel/reject are server-authoritative (the worker must not forge them). A vocabulary change is guarded by: `TestCoerceFailOrigin`'s partition invariant (`failorigin_test.go:46-49`, `len(workerReportable)+len(serverOnly)==len(failOrigins)`, so a new server-only value must be added to `serverOnly` at `:33`), and `TestFailOriginVocabularyMatchesCheck` (`:66`) which parses the CHECK out of a **hard-coded** migration path (`const path = "../store/migrations/00139_run_finalize_base_align_conflict.sql"`) and diffs it against `AllFailOrigins()`. **Option 2 avoids all of this** — routing to `cancelled` status changes no vocabulary.

### Cancel reason (REC B) — not captured anywhere

- `api/cmd/uzi/run.go` `cancel` (`:479-490`): calls `submitInput(..., kindCancel, "", nil)` — **hardcoded empty body, no `-m` flag** (unlike `reject`/`revise`/`follow-up`).
- Chat `EndChat` (`handler/chat.go:246`) sends `"cancel", ""`; the TUI (`cmd/uzi/tui_steer.go:269`) is a bare y/n.
- Even a supplied body: the live path stores it in `run_user_inputs.body`, but **server-side `CancelRunServerSide` (`runtime.sql:1374`) takes only id + user_id and drops it**. There is **no `runs` column** for a cancel reason (`00020_workers_runs.sql:44` has only `failure_reason text`, a *failure* field).
- The "persist reconnaissance" half is **already built**: PRD #333 (`00129_incidental_findings.sql`) gives per-run `findings` + cross-run `finding_dispositions`, persisted as reported (not at teardown). So scope REC B to the **cancel-reason gap only**.

### Reject reason (REC C) — optional, dropped server-side

- `uzi run reject` (`run.go:464-477`): `-m/--message` exists but help says "(optional)"; empty is allowed and passed (contrast `revise`/`follow-up` at `:503`/`:532`, which reject empty with `ExitUsage`).
- Handler `CreateRunInput` (`workers.go:1203`, `:1219-1224`) does no reason validation.
- Live path **does** preserve a non-empty reason: `SubmitInput` reject → `CreateStopVerdictInput(Body=body)` → `PlanRejectedError(verdict.reason)` (`executor.ts:821`) → `failure_reason=reason` (`runner.ts:1758,1774`).
- **Server-side reject DROPS it**: `RejectRunServerSide` is called with `FailureReason: pgText("plan rejected")` — a **hardcoded** string (`service.go:4557`), ignoring `body`.
- "Thread it back so the agent can revise" is **already** `uzi run revise` (`run.go:511-538`, `kindRevisePlan`; re-plan in place, requires a message). So REC C = require a reject reason + stop dropping it server-side.

### Gates and tests

`task gate:api` = fmt-check + vet + build + lint (ratcheted `.golangci.yml`) + deadcode + `test:api` (`-race -count=1`), run with `UZI_TEST_DATABASE_URL` **unset**. Vocabulary/classification guards (`TestCoerceFailOrigin`, `TestFailOriginVocabularyMatchesCheck`, `TestSetStateFailed*`) are in-process, **offline, no DB**. The SQL transitions (`CancelRunServerSide`/`RejectRunServerSide`/`SetRunFailed`) are covered by `*LiveDB` tests via `./e2e/run-store-it.sh` (local throwaway Postgres, **offline**, needs the `postgres:17` image cached; require a positive control per `.claude/rules/go.md` — `RUN>0`, named `--- PASS`, zero `--- SKIP`). After editing `queries/*.sql` or a migration: `cd api && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0 generate` (offline once cached), and a mutation must target the generated `store/*.sql.go` const to actually execute.

---

## Design decisions

1. **M1 uses Option 2 (route to `cancelled` status), not a new `operator_cancelled` origin.** Option 2 converges the live cancel path with the server-side path, makes Decision 2 (cancelled → not judged) uniformly true, and needs **no** `fail_origin` vocabulary change, **no** migration, and **no** `failorigin_test.go`/CHECK churn — it removes the special case rather than adding one. Concretely: in `SetState`'s `failed` arm (`service.go:3071-3095`), before defaulting to `agent_failure`, branch on the run's `stop_kind` (already on the loaded row): `'cancelled'` → perform a cancel transition (status `cancelled`, `fail_origin` NULL); `'plan_rejected'` → `SetRunFailed` with `fail_origin='plan_rejected'` (matching `RejectRunServerSide`); otherwise the existing `agent_failure` default. The rejected alternative (Option 1: add `operator_cancelled` to the vocabulary + migration + update the hard-coded `failorigin_test.go:66` path) is documented but not taken.
2. **M2 reuses `failure_reason`, no new column.** A rejected plan is a failure-class terminal, so `failure_reason` is the right home; pass `body` to `RejectRunServerSide` instead of the hardcoded literal, and make the CLI require a non-empty reason (mirroring `revise`/`follow-up`'s `ExitUsage`).
3. **M3 adds a nullable `runs.stop_reason text`, no CHECK.** A cancel is not a failure, so a dedicated nullable column is cleaner than overloading `failure_reason`. Persist it on both the server-side (`CancelRunServerSide` gains a `stop_reason` param) and live (`CreateStopVerdictInput` already stores the body in `run_user_inputs`; stamp `stop_reason` on the run at the same CTE) cancel paths — server-side stamping avoids trusting the worker. Add `-m/--message` to `uzi run cancel` (optional here — a cancel reason is helpful, not mandatory, unlike a reject).
4. **Web/TUI parity is out of scope.** The recs are about the CLI/steering-capture and server classification. Adding a "reason" prompt to the web cancel/reject surfaces and the TUI is a follow-up; the column + server plumbing this PRD lands is what a later web change would build on.
5. **One PRD, three milestones.** All three touch the same narrow lane (`SubmitInput`, the `SetState` failed arm, the three server-side transition queries, `cmd/uzi/run.go`), so splitting would cause repeated merge friction on `service.go`/`runtime.sql`. M1/M2 are tightest (shared failed arm + `RejectRunServerSide`); M3 is the most independent (adds a column).

## Scope

**In scope**:
- **M1**: `service.go` SetState failed-arm branch on `stop_kind` (Option 2); `judge_enqueue.go` consistency (a live cancel now reaches `cancelled`, so Gate 0 already excludes it — add a regression test rather than new gate logic if the routing makes 4b moot); tests `TestSetStateFailed*` + a new `judge_enqueue` test.
- **M2**: `run.go` reject requires non-empty reason; `service.go:4557` passes `body` to `RejectRunServerSide`; `runtime.sql` `RejectRunServerSide` stores the passed reason as `failure_reason` (regenerate sql.go); tests.
- **M3**: new migration adding nullable `runs.stop_reason text` (draft `00141`, renumber at land); `runtime.sql` `CancelRunServerSide` + `CreateStopVerdictInput` persist `stop_reason` (regenerate sql.go); `run.go` cancel gains `-m/--message`; tests.
- CHANGELOG `[Unreleased]` entry.

**Out of scope**:
- A new `operator_cancelled` `fail_origin` (Option 1, rejected).
- Web and TUI "reason" prompts (Decision 4).
- The "persist reconnaissance" half of REC B (already PRD #333).
- Any agent (`agent/`) code change (the worker-side threading already works).

## Milestones

- [x] **M1 — Classify cancel/reject off `stop_kind` (Option 2).** In `service.go`'s `SetState` `failed` arm, branch on the loaded run's `stop_kind`: `'cancelled'` → cancel transition (status `cancelled`, `fail_origin` NULL); `'plan_rejected'` → `SetRunFailed` `fail_origin='plan_rejected'`; else the existing `agent_failure` default. Add/extend `TestSetStateFailed*` (offline, no DB) asserting: a `failed` report with `stop_kind='cancelled'` yields terminal `cancelled` + NULL `fail_origin`; with `stop_kind='plan_rejected'` yields `failed` + `plan_rejected`; with neither, `agent_failure`. Add a `judge_enqueue` test that a live-cancelled run is **not** enqueued for judging. Prove each non-vacuous by a call-site fold (`.claude/agent-team.md`). If any SQL/transition is added, regenerate sql.go and run the relevant `*LiveDB` test via `./e2e/run-store-it.sh` (positive control required). **Validate**: `git fetch origin main` (ratchet), then `task gate:api`; the vocabulary tests (`TestCoerceFailOrigin`, `TestFailOriginVocabularyMatchesCheck`) stay green **unchanged** (Option 2 adds no vocabulary).
- [x] **M2 — Require + persist the reject reason.** Make `uzi run reject` reject an empty `-m/--message` with `ExitUsage` (mirror `revise`/`follow-up` at `run.go:503`/`:532`). **The fix is Go-caller-only, NOT a query change**: `RejectRunServerSide` already parameterizes the reason (`runtime.sql:1460`, `failure_reason = @failure_reason`) — the hardcoded literal lives in the **caller** at `service.go:4557` (`FailureReason: pgText("plan rejected")`). Change `service.go:4557` to pass the request `body` (falling back to a sensible default only if empty, though M2's CLI change makes empty unreachable from the CLI). **No `runtime.sql` edit and no `sqlc generate` is needed** — do not "helpfully" edit the query or regenerate; the generated const is unchanged. Tests: `run.go` cobra test that empty `-m` exits usage; a `*LiveDB` test that a server-side reject persists the supplied reason (the non-vacuity mutation targets the **existing** `RejectRunServerSide` const / the `service.go:4557` call site, not a new one). **Validate**: `task gate:api`; the reject `*LiveDB` transition via `./e2e/run-store-it.sh` (positive control).
- [x] **M3 — Cancel reason column + CLI + persistence.** Add a migration (draft `00141_run_stop_reason.sql`, renumber above the live head at land) adding nullable `runs.stop_reason text` (no CHECK). Add `-m/--message` to `uzi run cancel` (`run.go:479-490`), thread `body` through `SubmitInput`'s cancel branch. Persist `stop_reason` on both cancel paths: `CancelRunServerSide` gains a reason param (server-side), and the live path stamps it at `CreateStopVerdictInput` (regenerate sql.go for both). **🔴 `CreateStopVerdictInput` is a SHARED CTE** — it is called for `cancel`, `reject_plan`, AND auto-stop (`service.go:4545`/`:4580-4581`; auto-stop uses `kind='cancel'` per `runtime.sql:1988-1989`). So the `stop_reason` stamp must be conditional on the verdict being a **cancel**: pass `stop_reason = NULL` for `reject_plan` (its reason belongs in `failure_reason` via M2, and double-writing would contradict Decision 3's clean split), and decide auto-stop's value deliberately (leave NULL, or a fixed `"auto-stopped"` marker — state which). Tests: `run.go` cancel with `-m` sends the body; `*LiveDB` tests that a cancel persists `stop_reason`, **a live reject leaves `stop_reason` NULL** (guarding the shared-CTE split), and the reason survives on both cancel paths (positive control). **Validate**: `task gate:api`; the cancel/reject `*LiveDB` transitions via `./e2e/run-store-it.sh`; if PRD #500 has landed, `task check:migration-numbering` passes for the renumbered migration.
- [x] **M4 — CHANGELOG.** Add an `[Unreleased]` entry: operator cancellations are no longer classified as agent failures or judged; `uzi run reject` now requires a reason and persists it; `uzi run cancel` accepts an optional `-m` reason, stored on the run. **Validate**: CHANGELOG format matches existing `[Unreleased]` entries.

## Success criteria

1. A run cancelled while a live worker holds it ends with terminal status `cancelled` and NULL `fail_origin` (not `failed`/`agent_failure`), and is **not** enqueued for judging — matching the server-side cancel path.
2. A plan rejected while a live worker holds it ends `failed` with `fail_origin='plan_rejected'` (not `agent_failure`), matching the server-side reject path.
3. `uzi run reject` refuses an empty reason (exit usage), and a supplied reject reason is persisted server-side as `failure_reason` (no longer the hardcoded literal).
4. `uzi run cancel` accepts `-m/--message`, and the reason is persisted in the new nullable `runs.stop_reason` column on both the server-side and live cancel paths.
5. The `fail_origin` vocabulary and its CHECK/partition tests are **unchanged** (Option 2 adds no new origin); `task gate:api` passes with new offline unit tests and the relevant `*LiveDB` transitions green (positive control), and the mutations target generated `store/*.sql.go` consts.
6. A CHANGELOG `[Unreleased]` entry is added; the new migration is renumbered above the live head at land; no web/controller/agent change; no `.github/workflows/**` touched; `main` is never touched; delivered on a branch + PR.

## Risks & mitigations

- **Choosing Option 1 (new `operator_cancelled` origin)** drags in a migration, the `serverOnly` partition update, and the hard-coded `failorigin_test.go:66` migration-path update — easy to miss the last one. Mitigation: Decision 1 takes Option 2 (no vocabulary change); the vocabulary tests must stay green unchanged as a guard against accidental scope creep.
- **A `*LiveDB` mutation that targets the `.sql` source, not the generated `store/*.sql.go` const**, so it never executes. Mitigation: regenerate with pinned sqlc and target the generated const (`.claude/rules/go.md` mutation discipline); require a positive control (`RUN>0`, `--- PASS`, zero `--- SKIP`).
- **Overloading `failure_reason` for a cancel.** Mitigation: Decision 3 adds a dedicated nullable `stop_reason` for cancels; `failure_reason` stays for the reject/failure class (M2).
- **Migration number collision at land.** Mitigation: renumber above the live head at the landing rebase (CLAUDE.md); if PRD #500 landed first, its `check:migration-numbering` gate catches a stale number.
- **Ratchet false-red on `service.go`/`judge_enqueue.go`.** Mitigation: `git fetch origin main` before the gate (`.golangci.yml` `new-from-merge-base` + `whole-files`).
- **Web/TUI still send an empty cancel/reason** after this lands. Mitigation: Decision 4 scopes them out explicitly; the column + server plumbing is the foundation a later web/TUI change builds on, and the server-side default stays valid (nullable).

## Dependencies

- **No external / internet dependency.** Unit tests are in-process/offline; the SQL transitions run on the local throwaway Postgres (`run-store-it.sh`, needs `postgres:17` cached, no internet). No new Go dependency.
- **`stop_kind` must be on the run row before `SetState`'s failed report** — it is, stamped by `CreateStopVerdictInput` before the worker reports (Background); M1 relies on this ordering.
- **Shared-file note**: within this batch, only #503 touches `service.go`/`failorigin.go`/`judge_enqueue.go`, `runtime.sql`, `run.go`, and migrations (PRD #499 touches `workersvc/judge.go`, a different file in the same package — no textual conflict). **The real entanglement is with PRD #84 (capability-aware scheduling), in flight in a peer session**, which also edits `service.go`, `runtime.sql`, and adds migrations — but in **disjoint regions**: #84's work is in `ClaimRun`/`fn_worker_can_claim` (`service.go:~1098-1127`, `runtime.sql` ClaimRun call sites), `ListActiveRunsForHealth`, and worker/caps DTOs in `handler/worker_protocol.go`; #503's is in the SetState failed arm (`service.go:~3071`), `SubmitInput` (`:4557`), and the Cancel/Reject/StopVerdict queries (`runtime.sql:~1368-2006`). **No worker-DTO collision** — #503's `handler/workers.go` touch is only the steering handler `CreateRunInput`/`apitypes.RunInputRequest` (`workers.go:~1200-1224`), not the caps DTO. So the overlap is merge-friction on distinct regions plus a goose renumber, not a logical conflict. **Sequence #503 after #84 M1-M3 lands** (per the peer coordination) and renumber the `stop_reason` migration above #84's migrations, or rebase the disjoint hunks. **CHANGELOG `[Unreleased]`** is also touched by #501 — an append-only 2-way merge resolved at land time.

## Decision log

- **2026-08-21**: Bundled three `improve_uzi` recommendations on the termination/steering lane (cancel misclassified as agent_failure; cancel reason not captured; reject reason optional+dropped) into one PRD — they share `service.go`/`runtime.sql`, so separate PRDs would merge-conflict.
- **2026-08-21**: **REC A confirmed against code** — a live-poller cancel/reject ends `failed`+`agent_failure` because `SetState` has no `cancelled` case; the same default hits the live reject path while the server-side reject path correctly stamps `plan_rejected`.
- **2026-08-21**: **Chose Option 2 (route to `cancelled` status) over Option 1 (new `operator_cancelled` origin)** — Option 2 converges the two paths, needs no vocabulary/migration/CHECK-test change, and removes the special case.
- **2026-08-21**: **REC B scoped to the cancel-reason gap only** — its "persist reconnaissance before teardown" half is already delivered by PRD #333's incidental findings.
- **2026-08-21**: **REC C reduced to require-a-reason + stop-dropping-it-server-side** — the "thread it back so the agent can revise" half already exists as `uzi run revise`.
- **2026-08-21**: **Web/TUI reason prompts deferred** — the recs are about CLI/steering capture and server classification; the column + server plumbing here is the foundation for a later web/TUI change.
- **2026-08-21**: Next step = send to uzi (Auto). PRD authored fully internet-independent and workflow-file-free; CHANGELOG line included; new migration renumbered above head at land.
- **2026-08-21 (implementation)**: Landed as M1–M4. The `stop_reason` migration was renumbered from the draft `00141` to **`00143_run_stop_reason.sql`** (live head at land was `00142`). Auto-stop passes `stop_reason = NULL` on the shared `CreateStopVerdictInput` CTE (its identity is `stop_kind='auto_stopped'`). Review-driven hardening beyond the milestones: both the cancel reason (`stop_reason`) and the server-side reject reason (`failure_reason`) are now NUL-stripped and length-capped to `maxFailureReasonRunes` (2048), matching `sanitizeFailureReason` — a NUL in a text column raises Postgres 22021 and would silently abort the cancel/reject; the live path additionally NUL-strips the body co-written to `run_user_inputs.body` in the same INSERT.
