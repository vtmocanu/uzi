# PRD #33: Board–Run Follow-ups — MR-State Surfacing, Deliberate-Stop Signal, Multi-User Board Hardening, e2e Guard

**GitLab Issue**: [vtmocanu/uzi#33](https://github.com/vtmocanu/uzi/-/issues/33)
**Status**: Complete (2026-07-10) — all 5 milestones done. MR !33 merged to main (merge commit 99f00eb); issue #33 closed; supersedes issue #15 (closed). Migration renumbered 00045→00050 at landing (main's head had moved to 00049). Validation: pre-implementation review by 2 agents (design + fact-check); per-milestone reviewer+auditor rounds all clean; tester drove every success criterion live; web-ux browser pass with findings fixed and re-verified. Specs: ai.md §160-164.
**Priority**: Medium
**Created**: 2026-07-10
**Depends on**: PRD #12 (board–run lifecycle, done), PRD #24 (MR close/reopen watcher, done). Consolidates all four follow-up candidates from issue #15; supersedes it (close #15 when this PRD completes).

## Problem

Issue #15 recorded four self-contained gaps while shipping PRD #12. One of them has since narrowed: PRD #24 landed the MR close/reopen *automation* (poller watcher, `runs.mr_state`, migration `00029`), so "MR state tracking" is no longer a from-scratch follow-up PRD — but the state it maintains never leaves the store. The four gaps as they stand today:

1. **MR chips advertise stale state.** `runs.mr_state` is written by the watcher (`api/internal/forgesvc/mr_watch.go`) but is not exposed by any API payload: `LatestRun` (`web/src/lib/api.ts:223`) carries `mr_iid` and nothing else, so board cards, the issue view's run history (`web/src/pages/IssueView.tsx` `RunHistoryRow`), and `RunsList.tsx` all render an identical `MR !N` chip whether the MR is open, merged, or closed-unmerged. A reviewer scanning the board cannot tell a live review request from a finished one. (Auto-drain on merge is NOT part of this gap: the merge path already works end to end — agent MRs carry `Closes #N`, the merge closes the issue, the poller syncs it off the board; established in PRD #24's problem statement.)

2. **A deliberate stop can render as failure.** The client folds "deliberate human stop" into a calm neutral badge via an exact-string heuristic (`web/src/lib/runBadge.ts:25`, `STOPPED_FAILURE_REASONS = {"run cancelled", "plan rejected"}`). But a live-poller plan reject carries the user's *verbatim* reject reason (`agent/src/steering.ts:105` — `body?.trim() || "plan rejected"`), which no client-side match can recognize, so the run renders rose "failed" as if the agent broke. The server is the one that delivered the cancel/reject verdict; it should say so durably instead of the client guessing from a free-text string.

3. **Multi-user board leaks.** Latent today (all runs on a repo belong to the connection owner) but real once boards are shared:
   - `owner_name` falls back to the owner's **email** when `display_name` is empty (`api/internal/handler/board.go`, verified by `board_latestrun_test.go:63`), so a shared board would expose another user's email on every card badge.
   - `run_count` is a window count over **all** runs of the issue (`api/internal/store/queries/forge.sql:192`, `COUNT(*) OVER (PARTITION BY r.issue_iid)`), so the "×N" retry hint would count other users' runs.

4. **e2e harness breaks on a slashed project name.** `e2e/run-e2e.sh:41` passes `UZI_E2E_COMPOSE_PROJECT` verbatim to `docker compose -p`. The PID-based default is safe, but an explicitly exported value containing a slash (e.g. a branch-like name such as `feature/prd-33`) makes docker reject the project name mid-run, after setup work has begun.

## Solution Overview

Four independent fixes under one PRD (shared theme: finishing PRD #12's board–run story), one MR:

1. Expose `mr_state` through the run API and render MR-chip state in the web (open = today's chip; merged / closed get a distinct visual + label).
2. Add a server-stamped deliberate-stop signal (`runs.stop_kind`) written at the verdict sites, exposed in run payloads; the client heuristic is replaced, with a one-time backfill for the two exact literals.
3. Stop the email fallback (display-name-or-generic), keep `run_count` issue-scoped but document the decision.
4. Reject an invalid explicit `UZI_E2E_COMPOSE_PROJECT` at the top of the script with a clear error.

## Design Decisions

1. **`mr_state` is display-only and best-effort in the web; NULL renders as today's plain chip.** The watcher only maintains `mr_state` for its watch candidates (latest completed run, card in Human Review or reopen-watch — PRD #24 Decisions 4/10), so for unwatched cards the column is NULL or stale by design. The chip therefore treats `mr_state` as a *hint*: `merged`/`closed` get distinct rendering; NULL/`opened`/`locked`/unknown render the existing chip. No new polling is added just for display — widening the watcher's candidate set would break PRD #24's cost bound (Decision 10) for cosmetic freshness. Freshness holds only for the **board card** (the issue's latest run, kept watched while in Human Review or `mr_state='closed'`); per-run history rows (issue view, runs list, run view, dashboard) render each run's *frozen* `mr_state` — once a rework run supersedes a completed run, the watcher never updates the old run again, so a superseded run's chip can say `closed` after that MR was reopened (review finding: only stale `closed` misleads; `merged` is terminal). Chip `title` says "as of last sync"; docs scope the freshness claim to the board card and do not oversell the `merged` variant (a merge usually closes the issue and drops it from candidates before `merged` is ever observed).
2. **Use a derived-status pattern for the chip.** Precompute a small display enum server-side or in one web helper (`mrChipState(mr_state)`), never scatter raw forge state strings through components — already flagged as the pattern to use in PRD #24 §4.
3. **Deliberate-stop is a new nullable column `runs.stop_kind` (`cancelled` | `plan_rejected`), NOT a new run status.** A new status would touch the state machine, the sweeper, the claim gate, terminal-status checks, and every status switch in api/web/agent for what is presentation semantics; the status stays `failed`/`cancelled` exactly as today. The column is stamped **server-side at the moment the server knows the intent** — it never depends on what reason string the worker later reports:
   - `SubmitInput` reject/cancel verdict branches (`api/internal/workersvc/service.go`). The **live-poller path has no status write** — it enqueues the verdict via `CreateRunInput` (:861), an insert shared by approve/follow_up/cancel/reject_plan; the stamp is conditional on `kind ∈ {cancel, reject_plan}` and MUST land in the same transaction/statement as that insert (a new `SetRunStopKind`-style query or a combined insert — never a second non-transactional statement, whose loss would reintroduce the failed-vs-stopped bug). The server-side no-poller path (:847) stamps alongside its existing `FailureReason` write.
   - The existing server-side cancel path (status `cancelled`) stamps `stop_kind = 'cancelled'` for uniformity, though the client already treats status `cancelled` as stopped.
   - The sweeper's timeout/requeue failures do **not** stamp it — a timeout is not a deliberate human stop.
4. **Backfill only the two exact literals; historical verbatim-reason rejects stay "failed".** The migration backfills `stop_kind` where `failure_reason IN ('run cancelled', 'plan rejected')` (the only literals deliberate stops ever persisted: `plan rejected` is server-written at `service.go:847`, `run cancelled` originates agent-side — `executor.ts:205`, `sdk-executor.ts:51` — and lands via the worker's failure path). Old runs whose reject carried a verbatim reason are indistinguishable from real failures after the fact — accepted; the set is small and shrinking. With the column exposed, `isStoppedRun` becomes `status === 'cancelled' || (status === 'failed' && stop_kind != null)` and `STOPPED_FAILURE_REASONS` is deleted, replacing the string heuristic entirely (issue #15's stated bar), including the `runBadge.ts` known-limitation comment block. **The `failed` guard is load-bearing** (review blocking finding): on the live path `stop_kind` is stamped at verdict *enqueue*, while the run is still `awaiting_approval`/`running`, and a reject-then-approve race (`agent/src/steering.ts:94-96`, latest verdict wins) can even *complete* a run that carries `stop_kind` — without the guard those render "stopped" prematurely or suppress a completed run's MR chip. Backfill note: server-side cancels (`CancelRunServerSide`) write status `cancelled` with NO `failure_reason` — deliberately not backfilled; `isStoppedRun`'s `cancelled` branch covers them.
5. **`owner_name` never contains an email.** Fallback chain becomes display-name → empty string; the web already renders a no-owner badge for empty (`board_latestrun_test.go:77` asserts empty is legal). No sanitized email local-part — deriving a handle from the email still leaks the identifier it was meant to hide. `owner_email` stays admin-list-only (`handler/runs.go`, already gated). **The board latest-run DTO's `failure_reason` is owner-gated the same way (auditor pre-flag, 2026-07-10): it can carry a user's verbatim typed reject reason or a raw agent error, so a non-owner viewer of a shared board gets `null` (owners keep it for the failed-badge tooltip). `stop_kind` — a non-sensitive enum — stays exposed to everyone, so stopped-vs-failed classification never needs the free-text field.**
6. **`run_count` stays issue-scoped (counts all users' runs) — documented, not per-viewer.** The "×N" hint answers "how many times has this issue been run", which is a board-level fact a shared board legitimately shows; it exposes only a count, no identity. Per-viewer scoping would make the same card show different histories to different users and complicate the `DISTINCT ON` query for no confidentiality gain. Recorded in `specs/ai.md`; revisit only if a real multi-tenant deployment objects.
7. **e2e guard rejects, never rewrites.** An explicitly exported `UZI_E2E_COMPOSE_PROJECT` is user intent; silently sanitizing it hides the mismatch (logs, teardown hints, and any concurrently running stack would reference a name the user never set). Validate the **resolved** `PROJECT` unconditionally against compose's project-name rule (lowercase alphanumerics, `-`, `_`, must start alphanumeric: `^[a-z0-9][a-z0-9_-]*$`; verified against Compose v5.1.2's own error message) and exit with a clear message before any setup work — no provenance check needed, the PID default `uzi-e2e-$$` always passes (review nit).
8. **Migration number is a draft.** Drafted as `00045`; **landed as `00050`** at merge time — above main's new head `00049` (PRDs #18/#25 landed `00044`–`00049`), per the CLAUDE.md convention.

## Technical Design

### 1. MR-state surfacing (api handler + web)

- Store: no changes — `runs.mr_state` exists (migration `00029`); add it to the run-returning queries the handlers use (`ListLatestRunsForRepo`, the single-issue latest-run query, `ListRuns`/issue-history query in `forge.sql` / `runtime.sql`) and regenerate sqlc.
- API: add `mr_state: string | null` to the latest-run DTO (`handler/board.go`) and the per-run DTO — which lives in `handler/workers.go:88,119` (`runDTO`/`runToDTO`; `runs.go` only consumes it, review finding 3) — mirrored into `LatestRun` and the run row types in `web/src/lib/api.ts`.
- Web: one pure helper in `runBadge.ts` (unit-tested like the rest of that file) mapping `mr_iid` + `mr_state` to the chip variant: `merged` → check-mark/ok-toned "MR !N merged"; `closed` → muted/struck "MR !N closed"; anything else → today's chip. Applied at **all five** MR-chip surfaces (review finding 4): board card chip `Board.tsx:636`, `IssueView.tsx` `RunHistoryRow` (`mr_iid` render at :263), `RunsList.tsx:45`, `Dashboard.tsx:244`, and `RunView.tsx:234/264/268`.

### 2. Deliberate-stop signal (api + web, migration `00050`)

- Migration: `ALTER TABLE runs ADD COLUMN stop_kind text CHECK (stop_kind IN ('cancelled','plan_rejected'));` + backfill `UPDATE runs SET stop_kind = CASE failure_reason WHEN 'run cancelled' THEN 'cancelled' WHEN 'plan rejected' THEN 'plan_rejected' END WHERE failure_reason IN ('run cancelled','plan rejected');` + goose Down.
- Writers (all in `workersvc`): the `SubmitInput` verdict branches per Decision 3 — live-poller reject/cancel stamp transactionally with the `CreateRunInput` insert (:861), conditional on the verdict kind; server-side no-poller reject at `service.go:847` stamps alongside `FailureReason`; the server-side cancel path stamps `'cancelled'`. `SetRunFailed`-style worker-reported failures never stamp it — if the run had a pending stop verdict, the verdict site already did.
- API/web: expose `stop_kind` in the same DTOs as §1; `isStoppedRun(status, stopKind)` drops the `failureReason` parameter and the `STOPPED_FAILURE_REASONS` set (formula per Decision 4, terminal-guarded); callers updated: `runBadge`, `runStatusTone`, `Board.tsx`, `RunsList.tsx`, `IssueView.tsx`, and `RunView.tsx:114` (review finding 4); `runBadge.test.ts` cases rewritten around `stop_kind`, including the previously-impossible verbatim-reason reject now rendering neutral "stopped", plus the two Decision 4 guard cases (stamped-but-still-running renders by status; stamped-but-completed renders the MR chip).
- Agent: no protocol change — the worker keeps reporting whatever `failure_reason` it has; the server signal is authoritative.

### 3. Multi-user board hardening (api)

- `handler/board.go` owner-name fallback: display-name or empty (Decision 5); update `board_latestrun_test.go:63` to assert the email is NOT used.
- Sweep for other email fallbacks feeding non-admin payloads (`grep`-level check; `handler/runs.go` owner display on the user-scoped list).
- `run_count`: no code change; add the Decision 6 rationale as a comment at the window definition (`forge.sql:192`) and to `specs/ai.md`.

### 4. e2e guard (e2e/run-e2e.sh)

- After `PROJECT=` is resolved (`run-e2e.sh:41`): if the resolved value fails `^[a-z0-9][a-z0-9_-]*$`, print the offending value + the rule and exit 2 before any scratch-dir/compose work (unconditional — the PID default always passes, Decision 7).

### 5. Docs + specs

- `docs/board.md`: MR-chip states (merged/closed rendering; freshness claim scoped to the board card per Decision 1, history rows "as of last sync"), stopped-vs-failed badge semantics now server-driven.
- `specs/ai.md`: Decisions 1–8 recorded; `specs/human.md` untouched (no new user-stated requirements — issue #15 items were AI-surfaced follow-ups; the consolidation into one PRD is the user's call and is recorded on issue #33).

## Out of Scope

- Widening the MR watcher's candidate set or adding MR polling for display freshness (PRD #24's cost bound stands).
- Per-viewer `run_count` scoping and any broader shared-board/multi-tenant model.
- New run statuses or worker-protocol changes.
- WebSocket board push, issue comments in-app (unchanged from PRD #12's out-of-scope list).

## Milestones

- [ ] **M1 — Deliberate-stop signal end to end**: migration (`00050`) + backfill, `workersvc` stamp sites, DTO exposure, `runBadge.ts` heuristic replaced, api+web tests green. *(Files: `api/internal/store/{migrations,queries}`, `api/internal/workersvc/service.go`, `api/internal/handler/{board,workers}.go`, `web/src/lib/{api,runBadge}.ts`, `web/src/pages/RunView.tsx` + tests)*
- [ ] **M2 — MR-state surfacing**: queries + DTOs expose `mr_state`, chip variants rendered at all five surfaces, web unit tests. *(Files: `api/internal/store/queries/forge.sql`, `api/internal/handler/{board,workers}.go`, `web/src/lib/{api,runBadge}.ts`, `web/src/pages/{Board,IssueView,RunsList,Dashboard,RunView}.tsx`)*
- [ ] **M3 — Multi-user hardening + e2e guard**: owner-name fallback fix + test flip, email-fallback sweep, `run_count` decision comment, `run-e2e.sh` project-name guard. *(Files: `api/internal/handler/board.go` + tests, `api/internal/store/queries/forge.sql` comment, `e2e/run-e2e.sh`)*
- [ ] **M4 — Docs + specs sync**: `docs/board.md`, `specs/ai.md`; `check-docs` green.
- [ ] **M5 — Validation wave**: full gates (`go test ./...`, `npm test`/`typecheck` in web, agent tests untouched-but-run, `./e2e/run-e2e.sh` including a negative check of the new guard), review/audit findings resolved, issue #15 closed as superseded by #33.

Milestone dependency note: M1, M2, M3 touch disjoint behavior but overlapping files (`runBadge.ts`, `board.go`, `forge.sql`, DTOs) — run them **sequentially on one branch** (single coder), not as parallel worktrees; the per-milestone cost is small.

## Success Criteria

1. A run stopped via live-poller plan reject **with a verbatim reason** renders as neutral "stopped" on the board, runs list, and issue view — the exact case issue #15 item 3 names.
2. A board card whose completed run's MR was merged or closed shows that state on the chip; an open MR renders exactly as today.
3. No non-admin API payload ever contains another user's email; `owner_name` is display-name or empty.
4. `UZI_E2E_COMPOSE_PROJECT=feature/x ./e2e/run-e2e.sh` exits immediately with a clear error; the default PID-based run is unchanged and green.
5. `STOPPED_FAILURE_REASONS` no longer exists in the codebase.
