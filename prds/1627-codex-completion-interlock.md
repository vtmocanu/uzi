# PRD 1627: Completion interlock for Codex runs

- **Issue**: #1627 (split from #1626, parent epic #1225)
- **Status**: Ready (peer-reviewed, 3 rounds)
- **Priority**: Medium
- **Surfaces**: agent (`agent/src/codex/codex-executor.ts`, a new shared completion-attempt module, `agent/src/sdk-executor.ts` refactor only, `agent/src/runner.ts` harness-aware resume, capability advertisement), api (`api/internal/workersvc/service.go` create-time stamp, `api/internal/workersvc/health.go` queued reason, `api/internal/capability/capability.go`, the claim clauses in `api/internal/store/queries/runtime.sql` + `sqlc generate`, the queued-run health reason), web (`web/src/pages/adminSettings/CompletionInterlockCard.tsx` copy), CLI comments (`api/cmd/uzi/run_decide.go`, `api/cmd/uzi/run_render.go`), docs (`docs/admin-settings.md`, `docs/run-completion-hold.md`, then `task docs:sync`), ADR 1225 addendum, `specs/human.md`.
- **No migration.** The new protocol capability is a value in the existing `workers.protocol_capabilities` column; the claim change is a query predicate.
- **Guardrail**: no `.github/workflows/**` change in implementation or validation (`.claude/rules/prds.md`).
- **Offline-resolvable**: every fact below was read from the tree at `main` @ `5758d96e`; the implementer re-verifies each by reading the cited file. Nothing here needs the open internet.

## Problem

The structural completion interlock (PRD #1226, on by default since #1626 / v0.84.0-rc.12) turns a Claude run's `signal_done` into a completion attempt: unmet frozen milestones earn a same-session nudge, a no-progress streak earns a timed owner question, then a recoverable owner hold. A Codex run gets none of it. `createRun` stamps `completion_contract_version` only when the resolved harness is Claude, because `CodexExecutor` breaks its implement loop on `done` without recording an attempt. A Codex run can therefore finish with frozen milestones unmet and open its PR as if complete.

Stamping Codex runs today would be worse than leaving them unstamped: the permit check at finalize would deny, and every incomplete Codex run would go straight to an owner hold with no nudge.

## Solution

Port the completion-attempt loop to `CodexExecutor` so a Codex issue run behaves like a Claude one: attempt on done, nudge in the same Codex thread, owner question at the stall limit, hold after, wall-park race resolved the same way. Gate claiming an interlocked Codex run on a new worker protocol capability so an older worker never receives one. Then drop the Claude-only filter at run create.

## Resolved facts (current code)

### The SDK loop being ported (`agent/src/sdk-executor.ts`)

- Guard: `ctx.completionInterlock && isIssueRun && !ctx.interactive && ctx.recordCompletionAttempt` (~L2583).
- Attempt, in order (~L2583-2710): (1) `ctx.checkpoint({ reap: true, progress })` BEFORE any feedback; (2) `ctx.worktreeFingerprint()`, head = its first line; (3) `ctx.recordCompletionAttempt({ declared: turn.milestonesCompleted ?? [], head, worktreeFingerprint })` returns `{ unmet }` (server-authoritative union); latch `completionAttempted = true` only after it returns; (4) `unmet` empty → break to finalize; (5) fingerprint `JSON.stringify([sorted unmet, head, worktreeFingerprint])`, identical consecutive fingerprint advances `completionStallStreak`, else resets to 1.
- `STALL_LIMIT = 3` (~L165), shared with the #281 prose stall.
- At the stall limit: `ctx.askCompletionQuestion(unmet)` when wired. `continue` → follow-up with owner guidance, reset streak and fingerprint, `turn.done = false`, continue; iteration and wall budgets unchanged. `expired` → fall through to `routeCompletionHold(ctx, REASON_COMPLETION_NO_PROGRESS, completionAttempted)`; `true` → latch `completionHeld` and break; `false` → throw the reason.
- Below the stall limit: `buildCompletionReworkFollowUp(unmet, frozenMilestones)` (~L3761) as an autonomous same-session follow-up; `resetStallState()`; continue.
- Post-attempt iteration exhaustion routes to the hold, not `REASON_MAX_ITERATIONS` (~L2715).
- Server budget steer: at the loop top, `served?.budgetExhausted` after an attempt routes to the hold with `REASON_COMPLETION_BUDGET_EXHAUSTED` (~L2146-2160).
- D14 wall race (~L2320): a `wall` pause after the first attempt tries `routeCompletionHold(ctx, REASON_WALL, …)` first; only `false` falls through to `parkForWall`. The hold's server write settles the wall columns.
- `routeCompletionHold` (~L3742) calls `ctx.enterCompletionHold(reason)` only when `attempted`.

### The Codex side (`agent/src/codex/codex-executor.ts`)

- Header (~L33) and `driveTurnWithWallPark` doc (~L2403): "Codex does NOT enter the completion-attempt interlock, so its wall trip ALWAYS parks (no D14 race)". Both become false after this PRD.
- Implement loop (~L1859-1920): `maxIterations = positiveOr(ctx.config?.max_iterations, DEFAULT_MAX_ITERATIONS)`; per iteration `ctx.reportIteration` (lifts the wall), safety-steer prefix, then the turn via `driveTurnWithWallPark`; `if (result.done) break;` (~L1916); `if (iteration >= maxIterations) throw new Error(REASON_MAX_ITERATIONS)` (~L1917).
- Cooperative checkpoint pattern already exists (~L1907-1914): `epoch.persistSession()` → `ctx.checkpoint({ reap: true })` → `startProviderEpoch(ctx, shared, lastSessionId, ++epochIndex)` → dispose old. A `reap: true` checkpoint kills the provider root, so the next turn needs a new epoch resuming `lastSessionId`.
- Wall park: `tryCodexWallPark` (~L2458) routes wall trips to `ctx.parkForWall`; no hold branch.
- The declaration already flows: `signal_done` schema has `milestones_completed` (`agent/src/codex/dynamic-tools.ts` ~L97); the reducer copies it to `ReducedTurnResult.milestonesCompleted` (`agent/src/harness-reducer.ts` ~L187, `agent/src/harness.ts` ~L406).

### Runner seams are harness-neutral (`agent/src/runner.ts`)

- The `RunContext` built at ~L6440-6520 wires `completionInterlock` (from `claim.config.completion_contract_version`), `recordCompletionAttempt`, `enterCompletionHold`, `askCompletionQuestion` for every executor. Codex receives them today and ignores them.
- `enterCompletionHold` (~L8754) captures through the shared `captureHoldContext` (~L9142), which is already harness-aware (Codex boundary handling ~L9204) and is what the Codex wall park uses.
- `phasePublish` reads `ExecutorResult.completionHeld` to skip finalize (`agent/src/runner.ts` ~L3164; the field is declared in `agent/src/executor.ts` ~L689).
- Resume drops a non-Claude session: `phaseResume` (`agent/src/runner.ts` ~L5441) clears `claim.session_id` when `sessionTranscriptResolvable` is false, and that helper looks only under `<HOME>/.claude/projects` (`agent/src/sdk-session.ts` ~L138). A held Codex run's thread id would be discarded before `CodexExecutor` could adopt it.
- The SDK routes post-attempt WALL **and IDLE** trips to the hold (`agent/src/sdk-executor.ts` ~L2363); pre-attempt they rethrow. Codex's wall wrapper (`tryCodexWallPark`) handles wall only; an idle trip rethrows and fails the run.

### api

- Create-time filter: `api/internal/workersvc/service.go` ~L5553, `if interlockOn && resolved.Harness == HarnessClaude`. `interlockOn` (~L5503) already excludes seeded runs.
- Claim clauses in `api/internal/store/queries/runtime.sql`: completion (`completion_interlock_v1`, ~L825-836), Codex harness (`codex_harness_v1`, ~L837-853), custom Codex model (`codex_custom_model_v1`, ~L855-878); peer fleet-spread mirrors ~L985-1020.
- Queued-run health reason: `api/internal/workersvc/health.go` ~L706-720 (`CountOnlineWorkersSatisfyingProtocol` → `reasonNoCompletionCapableWorker`); the query is in `runtime.sql` ~L5930.
- Precedent for a Codex sub-capability: `CodexCustomModelV1` (`api/internal/capability/capability.go` ~L76-91), a strict addition on top of `codex_harness_v1`, gated by a dedicated clause outside `fn_worker_can_claim`, mirrored for the peer, re-checked post-claim in `claim_assembly.go` ~L293.
- Stale "Codex is not checked" copy: `web/src/pages/adminSettings/CompletionInterlockCard.tsx` L9 and L60; `docs/admin-settings.md` ~L239; `docs/run-completion-hold.md` ~L18; comments in `api/internal/settings/keys.go` (~L102, ~L326), `api/internal/workersvc/service.go` (~L1589, ~L5546-5552), `api/cmd/uzi/run_decide.go` ~L29-30, `api/cmd/uzi/run_render.go` ~L237.

## Design

### D1: One shared completion-attempt core, two drivers

Extract the harness-neutral parts of the SDK loop into one module (for example `agent/src/completion-attempt.ts`): the attempt fingerprint, the streak update, the follow-up text (`buildCompletionReworkFollowUp`, moved verbatim), `routeCompletionHold`, and the reason constants. Both executors call it; each keeps its own turn plumbing. The SDK refactor is behaviour-preserving: its existing tests (`agent/test/runner-completion-*.test.ts` and the SDK executor tests) pass unmodified. Duplicating the loop in Codex was rejected: two copies of a fail-closed state machine drift.

### D2: Same-thread re-entry through the existing new-root resume

On a Codex `done` for an interlocked, non-interactive issue run, run the attempt in the SDK order. The checkpoint-first step is `reap: true`, which kills the provider root, so the Codex attempt reuses the cooperative-checkpoint sequence already in the loop: `persistSession` → `ctx.checkpoint({ reap: true })` → record the attempt → on unmet, `startProviderEpoch(…, lastSessionId, …)` → dispose the old epoch → drive the next implement turn with the rework follow-up as its prompt. The session id carries the thread, so the lead keeps its context. Same guard as the SDK path, including `!ctx.interactive`.

### D3: Budgets are the SDK's

`STALL_LIMIT` and the owner-question window (`completion_hold_window_seconds`, runner-side) are shared, with no Codex-specific constants. Codex keeps its own `max_iterations` and wall. After the first attempt, Codex iteration exhaustion and the server's `budgetExhausted` steer route to the hold (`REASON_MAX_ITERATIONS` / `REASON_COMPLETION_BUDGET_EXHAUSTED`) exactly as the SDK does; before the first attempt they behave as today.

### D4: Post-attempt wall and idle trips resolve hold-first, as the SDK does

In the Codex turn wrapper, when `completionAttempted` is true:

- **Wall** (timer or `wall` pause): try `routeCompletionHold(ctx, REASON_WALL, true)` first; `true` → a held outcome that ends the run with `completionHeld`; `false` → today's `parkForWall` path. Before the first attempt the wall always parks, as now.
- **Idle**: try `routeCompletionHold(ctx, REASON_IDLE, true)`; `true` → held; `false` → rethrow as today. Before the first attempt idle rethrows, as now.

Cancel and every other error rethrow unchanged. Update both stale "no D14 race" comments.

### D5: Owner question and hold reuse the runner seams

`ctx.askCompletionQuestion` and `ctx.enterCompletionHold` are already harness-neutral, so the question and the hold entry need no runner change (the resume does, D6). `continue` resumes the same Codex thread (D2 re-entry) with the owner guidance in the follow-up and iteration/wall budgets unchanged. The executor returns `completionHeld` in its `ExecutorResult` so `phasePublish` skips finalize.

### D6: Make resume harness-aware so a held Codex run keeps its thread

This is a required runner change, not an open question. `phaseResume` validates the resume session only against the Claude transcript store (see *Resolved facts*), so a held Codex run's thread id is cleared and the resume starts a fresh thread. Make the check harness-aware: for a Codex claim, validate against the Codex session store (`CodexSessionStore` in `agent/src/codex/session-state.ts`, the credential-free store `persistSession` writes) instead of `.claude/projects`; the Claude path is unchanged. The check must be **specific to the claimed thread**: `CodexSessionStore.inspect` (`agent/src/codex/session-state.ts` ~L1032) returns `present` when the current generation holds *any* session artifact, so it cannot be the check as-is. Add an id-specific lookup (the claimed `session_id` exists in the current generation) and use it. Keep both fail-safes: a store lacking the claimed thread falls back to a fresh thread with the same warning, and if the Codex resume itself rejects the thread at turn start, the executor also falls back to a fresh thread with that warning rather than failing the run.

The test must prove the thread is actually adopted (the resumed turn runs on the persisted Codex thread), not only that the id reaches the executor.

### D7: A new protocol capability gates the claim

Add `codex_completion_interlock_v1`, a strict addition on top of `codex_harness_v1`, modelled on `CodexCustomModelV1`:

- The agent advertises it whenever it advertises `codex_harness_v1` and carries this loop.
- `capability.go`: add it to the protocol vocabulary and `protocolOrder`, never to `vocabulary` / `required_capabilities`.
- `runtime.sql`: a dedicated, non-bypassable clause: an interlocked (`completion_contract_version IS NOT NULL`) Codex-indicating run (same three-fact test as the Codex clause) is claimable only by a worker advertising it. Mirror it in the peer fleet-spread predicate. Run `sqlc generate`.
- The queued-run health reason (`health.go`) names the missing capability when no online worker can claim the run. Its capable-worker count must require the intersection of every protocol capability the claim needs for this run, including the new one and, for a custom-root Codex run, `codex_custom_model_v1`, not each capability separately.

Without it, an older worker in a mixed fleet would claim a stamped Codex run, skip the nudge and hold on every incomplete completion.

### D8: Then stamp Codex runs

Remove `&& resolved.Harness == HarnessClaude` from the create-time stamp. Seeded runs stay legacy (`interlockOn` already excludes them). The admin switch stays the one kill-switch for both harnesses. Order matters: this change lands in the same PR as D7, never before it.

### D9: Docs and copy follow the behaviour

Remove "Codex runs are not checked" everywhere listed under *Resolved facts*; run `task docs:sync`. Add an addendum to ADR 1225 (the invariant now covers both harnesses, plus the new capability) and an `(AI-synced …)` line under Feature #1226 in `specs/human.md`. Check `api/cmd/uzi/` for any user-facing string that says Codex is exempt (today only comments).

## Milestones

| # | Milestone | Validation (offline) |
|---|---|---|
| M1 | Shared completion-attempt core extracted (D1); SDK behaviour unchanged | `task gate:agent`; existing completion tests pass unmodified |
| M2 | Codex attempt loop: record on done, same-thread nudge via new-root resume, stall → owner question → hold, post-attempt budget routing (D2, D3, D5) | new `codex-executor` tests: unmet → nudge in the same thread → completes once declared; three no-progress attempts → question; `continue` resumes the same thread with budgets unchanged; `expired` → hold, never `failed`; post-attempt iteration exhaustion → hold; non-interlocked and interactive runs unchanged |
| M3 | Post-attempt wall and idle trips (D4) | tests: wall after first attempt → hold wins; wall before any attempt → parks; hold refused → falls through to the wall park; idle after first attempt → hold; idle before any attempt → rethrows as today |
| M4 | Harness-aware resume: a completion-held Codex run adopts its thread (D6) | tests: held Codex run resumes on the persisted Codex thread (adoption proven on the resumed turn); a present store that lacks the claimed thread id → fresh thread + warning; empty/absent store → fresh thread + warning; Codex resume rejected at turn start → fresh thread + warning, run not failed; Claude resume unchanged |
| M5 | `codex_completion_interlock_v1` capability: advertise, vocabulary, claim clause, peer mirror, health reason in `health.go` (D7) | agent test for advertisement; live-DB claim tests: capable worker claims, `codex_harness_v1`-only worker does not, legacy Codex run unaffected, peer mirror; health test: the capable-worker count checks the intersection of every protocol capability the claim requires (completion + Codex harness + the new one), so a worker with only some of them does not count, including a custom-root Codex run that also needs `codex_custom_model_v1`; watch each fail on the unfixed code |
| M6 | Stamp Codex runs; remove the filter (D8) | live-DB `createRun` test: unseeded Codex issue run stamped when on; seeded stays legacy; switch off stamps neither harness |
| M7 | Docs, copy, ADR addendum, spec line (D9) | `task gate:web`, `task check-docs:web`, `task docs:sync` committed, `task gate:api` (embedded-docs equality) |

M1 → M2 → (M3, M4) → M5 → M6 → M7. M5 and M6 ship in one PR (D8).

### What runs where

- **On the worker**: `task gate:agent`, `task gate:web`, `task check-docs:web`, `task docs:sync`, `sqlc generate` and `task gate:api`. `go run` fetches the pinned sqlc through the Go module proxy, which is package-cache egress the worker allows; if it cannot, report exit 2 from `check:sqlc-drift` in the PR rather than skipping it.
- **Live-DB tests** (M5 claim/health, M6 `createRun`) are skipped by the plain `gate:api` without a database (`.claude/rules/go.md`). Run them against a throwaway Postgres if the worker can start one; otherwise say so explicitly in the PR, and CI's `test-api-store-it` lane is the live-DB evidence. Never report a live-DB test as passed when it was skipped.

## Success criteria

- An interlocked Codex issue run that signals done with an unmet frozen milestone gets a same-thread nudge and completes once the milestone is declared.
- A no-progress Codex run gets the timed owner question first, then the owner hold; it never ends `failed` on the interlock.
- After an attempt, wall and idle trips resolve to the hold; before one, the wall parks and idle behaves as today.
- No worker without `codex_completion_interlock_v1` ever claims an interlocked Codex run.
- Claude-run behaviour is unchanged (all existing completion tests green, untouched).
- No UI, doc or CLI text says Codex runs are exempt.

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| The SDK refactor changes Claude behaviour | M1 is refactor-only, gated on unmodified existing tests |
| The attempt's `reap: true` checkpoint leaves Codex without a live root | D2 reuses the existing persist → reap → new-epoch resume sequence, already proven for cooperative checkpoints |
| Mixed fleet sends a stamped Codex run to an old worker | D7 claim clause, landed with D8 in one PR |
| Hold resume starts a fresh Codex thread and loses context | D6 makes resume harness-aware; M4 proves adoption on the resumed turn |
| A post-attempt idle trip fails the Codex run | D4 routes it to the hold, M3 tests it |
| The fleet has no capable worker right after deploy | The worker pin rolls with the release (autobump); until then the health reason explains why an interlocked Codex run is queued, and the admin kill-switch is the escape |

## Dependencies

- #1626 merged (v0.84.0-rc.12). Done.
- The #1225 structural checkpoint that #1627 named as a precondition: waived by the maintainer on 2026-09-25 (Decision log).

## Decision log

- **2026-09-25**: Proceed before the ~2-week #1225 structural checkpoint the issue listed as a dependency. Maintainer decision.
- **2026-09-25**: Share the SDK budgets (D3) rather than Codex-specific constants: one owner-facing contract across harnesses.
- **2026-09-25**: Gate on a new protocol capability (D7) rather than relying on the fleet rolling, following the `codex_custom_model_v1` precedent.
- **2026-09-25**: Extract a shared core (D1) rather than duplicating the loop in the Codex executor.
