# PRD #754: Auto lane never spends a non-pooled token — floor to the pool, hold on empty

**Issue**: [#754](https://github.com/vtmocanu/uzi/issues/754)
**Status**: Complete (all milestones M1–M7 shipped on this branch)
**Priority**: High
**Depends on**: PRD #111 (auto-select by headroom — shipped), PRD #217 (limit-park dead-credential exclusion — shipped), PRD #35 (usage-limit park — shipped)

> **Revised 2026-08-28 after a 3-reviewer pass** (scope/feasibility, milestones/testability, risks/dependencies). The reviews corrected the claim-path seam, found a second default-fallback branch (D14 `open_failed`), showed reactive promotion cannot live in SQL, and surfaced a poller-disabled regression. The design below folds all of that in and is materially simpler than the first draft: a **last-resort floor to the best pooled token** removes the need for a claim-time park on a non-empty pool, and a **distinct non-locking empty-pool hold state** doubles as the promotion discriminator. See the decision log for the resolved forks.

## Problem

For a user whose auto-select pool is a **single token**, resuming a run from an Anthropic usage-limit park spends the **owner default credential**, even when that default was deliberately kept **out of the pool** (`auto_eligible = false`). Taking a token out of the pool does **not** stop an `auto` worker from spending it.

### Observed live (`vtmocanu/uzi`, 2026-08-28)

Token config (`uzi token list --json`):

| label | is_default | auto_eligible | auto_status |
|---|---|---|---|
| meta | **true** | **false** (out of pool) | not_pooled |
| cristi | false | true (only pooled token) | stale (rate-limited) |
| personal | false | false | not_pooled |

Three active issue runs landed on **meta** with `anthropic_select_reason = pool_empty`, `limit_wait_count = 1` (`uzi run get --json`): `22ea0936`, `0a856d07`, `94379eec`. A run that never parked still runs on cristi correctly (`03817034`: cristi, reason `auto`).

`reason = pool_empty` is the decisive tell: cristi is the user's only `auto_eligible` token, so `Select` returning `!pooled` is reachable **only** when cristi is *excluded* — which happens only on a resume from a park on cristi (PRD #217's `limit_dead_secret_id`). A merely-stale cristi records `pool_stale` instead. So the runs parked on cristi, were re-claimed with cristi excluded, found no other pooled token, and fell back to the owner default.

### Root cause

The claim path is `ClaimRun → assembleClaim → claimSecretID → autoChoice → autoselect.Select`.

1. A run runs on cristi under `auto`. cristi hits a usage limit; with `wait_on_limit = true` it parks: `SetRunLimitWait` sets `limit_dead_secret_id = cristi`, `retry_not_before = <cristi reset>` (`api/internal/workersvc/limitwait.go`).
2. `PromoteLimitWaitRuns` promotes it once `retry_not_before` passes (`limit_wait → queued`, `api/internal/store/queries/runtime.sql`).
3. Re-claim runs `autoChoice` with `exclude = cristi`.
4. In `autoselect.Select` (`api/internal/autoselect/select.go`), cristi is skipped as excluded **before** `pooled` is set true, and meta is skipped as `!AutoEligible` → `pooled == false → ReasonPoolEmpty`, `Picked == false`.
5. The D7 fallback ("auto never fails a run") resolves `base = workerSecretID(wkr) = nil` to the **owner default = meta** via `GetDefaultUserSecretMeta`, with **no `auto_eligible` check**. `lowestAltCandidate` only fires when the resolved default equals the excluded credential (meta ≠ cristi), so it never runs; the code spends meta.
6. `recordRunCredential` records the spent credential **and clears `limit_dead_secret_id`** at that point (`SetRunAnthropicSecret`, `runtime.sql`), during claim assembly — **not** at `SetRunRunning`. Hence the observed `dead = null`.

### Two exact facts the fix hinges on (corrected in review)

- **`limit_dead_secret_id` clears at `SetRunAnthropicSecret` (i.e. at credential-record time inside claim assembly), not at `SetRunRunning`.** So on a resume that never records a credential (a hold), the exclusion **persists** — which is exactly what the exclude-relax must reason about.
- **The only case currently misreported is the excluded-sole-token case** (`!pooled → pool_empty`). A rate-limited/stale pooled token already classifies as the distinct `pool_stale`. So the model gap is narrow: "pooled tokens exist but the only one is excluded" is indistinguishable from "nothing is pooled." The fix does not need to reclassify stale.

### There is a SECOND default-fallback branch (D14), found in review

Dropping the `autoChoice` fallback is **not** the whole story. `assembleClaim`'s D14 open-failed retry (`api/internal/workersvc/service.go`, the `open_failed` arm) fires when `Select` **picks** a pooled token and `openAnthropic` then fails to decrypt it: it retries on `workerSecretID(wkr)` (nil → owner default). Under the pooled-only rule this spends the non-pooled default **on a pick branch**, defeating the headline criterion. M2 must close it too.

## Solution overview — one absolute invariant, a precedence ladder

**Invariant: an `auto` worker never spends a token whose `auto_eligible = false`, on any branch (pick, fallback, or open-failed retry).** The owner-default fallback (PRD #111 D7 "auto never fails a run") is removed from the auto lane. Liveness is preserved not by spending the wrong account but by falling back **within the pool** and, only when the pool is genuinely empty, holding.

`autoChoice` resolves a run's credential by this precedence (auto lane only):

1. **Pick.** `Select` returns a usable pooled token (eligible, or D10 best-of-pool below-threshold) → spend it (`reason = auto` / `best_of_pool`). Unchanged.
2. **Floor (last resort, never fail, never the default).** No usable pick, but the user has ≥1 pooled token that can be resolved (honoring the exclude-relax below) → spend the **best pooled token available, including a stale or below-threshold one**, with a deterministic tie-break — rather than fail or touch the non-pooled default. This is the resolved fork: *a stale pooled token beats the out-of-pool default, always.* It subsumes best-of-pool and extends it to unmeasured tokens as a genuine last resort.
3. **Empty-pool hold.** The user has **no** pooled token at all → the run **holds** in a distinct, non-locking state (below) with a reason like "auto pool is empty, add a token to the pool", and resumes automatically when the user opts a token in.

The waiting the user asked for ("continue on cristi") is delivered by the **existing** usage-limit park (a worker reporting a limit while running already parks the run until `retry_not_before`); this PRD's job is that the **resume** picks cristi, not meta. The **exclude-relax** makes that happen: once the just-parked credential's own window has reopened (its `retry_not_before` passed), it is no longer excluded, so the resume re-picks it (step 1) or floors onto it while its gauge is still catching up (step 2). Either way the run continues on cristi and never on meta.

### Why this shape (design rationale, resolved forks)

- **Floor to the pool instead of a claim-time park.** The first draft parked the just-claimed run into `limit_wait` on a non-empty exhausted pool. Review showed `SetRunLimitWait` cannot do `claimed → limit_wait` (its guard is `status = 'running'` and its header explicitly forbids that transition), so this would need a whole new park query + a claim-time park decision with no worker report to compute a reset from. The floor removes the need entirely: on a non-empty pool the run **runs** (on the best pooled token) rather than parks. Simpler, and it fixes the **poller-disabled regression** — where nothing is `Measured` (`UZI_USAGE_POLL_INTERVAL = 0`, or the ~15m post-limit backoff), so a hold could never resolve and would burn the wait budget to a failure. The floor spends the (stale) pooled token there, exactly as auto effectively does today, minus the non-pooled default.
- **Empty-pool gets its OWN state, not `limit_wait`.** A `limit_wait` hold counts as active in `uq_runs_one_active_per_issue` (the one-active-per-issue index, `WHERE status NOT IN (completed,failed,cancelled)`), so it would pin the issue and block any re-run — and it is reachable on a **fresh** claim, so an auto worker with an unpopulated pool would lock every issue. The resolved fork is a **distinct non-locking hold state** excluded from that index. As a bonus it is a clean **promotion discriminator**: reactive resume operates on this state alone and never touches the shared `PromoteLimitWaitRuns` path.

### The central tradeoff (stated, deliberate)

This removes PRD #111 D7 "auto never fails a run" **as a fallback to the owner default**. An auto run can now hold (empty pool) rather than spend an out-of-pool credential. It does **not** generally fail for want of a candidate: the floor keeps it alive on any pooled token. The only new terminal outcome is a genuinely empty pool that the user never populates (M4 bounds that with a surfaced reason, not a silent forever-pin). Record the reversal in the decision log and update every PRD #111 D7 reference (see M7 — the reference set is scoped, not a blanket `D7` grep).

## Scope / non-goals

- **In scope**: `api/internal/autoselect` (the narrow empty-vs-excluded split + the floor tier), `api/internal/workersvc` (`autoChoice` precedence ladder; the D14 `open_failed` arm in `assembleClaim`; the exclude-relax; the empty-pool hold state + its reactive resume; manual resume-now), `api/internal/store/queries/runtime.sql` + a new migration (the hold state, its promotion pass, the resume-now write), the select-reason / hold-reason vocabulary and its drift-guarded CHECK (a **new** migration, not an edit to the applied `00089`), the CLI (`api/cmd/uzi` + the embedded `api/internal/uzicli/skill/SKILL.md`), the web run view + copy + a resume-now control, live-DB tests, and docs.
- **All auto-lane run kinds are in scope.** `claimSecretID` routes every non-`self_improve` run on an `auto` worker through `autoChoice`: `issue`, `ci_fix`, `prompt`, `task`, `then_fix`. Each takes the new ladder; note the differing dedup/UX (a `ci_fix` hold blocks CI-autofix on that ref, a `prompt` hold blocks a schedule's next fire). `self_improve` and `judge` resolve the judge binding and never reach `autoChoice`; `chat` uses its own `assembleChatClaim` (always the default) — all three unchanged.
- **Out of scope — `.github/workflows/**`.** Neither implementation nor validation may create, modify, or commit any file under `.github/workflows/` (the uzi worker PAT lacks `workflow` scope; a workflow-file touch in the branch diff is an atomic push rejection that loses the whole branch — `.claude/rules/prds.md`). Before finalize, `git diff --name-only <base>..HEAD` must show **zero** entries there. This feature needs no CI-workflow change.
- **Out of scope**: the `default`/`pinned` bind modes (they resolve one named credential, never the pool), the poller/gauge freshness (cristi reading `stale` is the poller's correct D11 backoff), and controller/hosted-worker changes (bind mode and credential resolution are api-side).

## Key design decisions

1. **autoselect split is a boolean, kept pure (M1).** Track "at least one token is pooled" set right after the `!AutoEligible` guard and **before** the exclude `continue`, so `Select` can return "pooled tokens exist but none pickable" distinctly from "nothing pooled". This is a boolean OR across candidates — order-independence, totality, and purity (`time`+`uuid` only) all preserved, `exclude` contract untouched. The minimal internal change is moving the pooled-membership accounting above the exclude skip.

2. **The floor tier (M1).** Add a last-resort ranking that returns the best pooled candidate even when none is `Measured`/eligible — for stale/unmeasured tokens there is no headroom to rank, so the tie-break is deterministic on `(soonest binding-window reset, then lowest secret id)`, mirroring `tieLess`. Restricted to `AutoEligible` candidates (never the default). It is a **distinct outcome** the caller opts into as a floor, not something that changes the normal pick. Keep `Select` pure; the caller decides when to consult the floor.

3. **Reason vocabulary + a NEW migration (M1/M2).** `00089_run_select_reason_check.sql` is already applied on live instances, so a new `anthropic_select_reason` value needs a **new** drop-and-re-add migration (numbered at merge time), moved in the same commit as the Go `AllReasons()`/consts, `TestSelectReasonVocabularyMatchesCheck` (which parses the CHECK), the CLI switch (`api/cmd/uzi/run.go`), the `len(AllReasons()) == 8` assertion in `select_test.go`, and the web union (`web/src/lib/api.ts` / `runCredential.ts`) — the doc names three enumerating guards (workersvc, CLI, web) and they redden the ordinary gate if forgotten. Decide the minimal set of new values: at least one for the floor's provenance if it must be visible, and the empty-pool hold reason. Prefer surfacing the *human* sentence via the run's `health_reason` and keeping the enum change minimal — but note (Decision 9) that the park/promote statements currently reset health, so reusing `health_reason` requires not clearing it on the hold path.

4. **Never resolve the non-pooled default on the auto lane (M2).** Remove the `GetDefaultUserSecretMeta`/`lowestAltCandidate` fallback from `autoChoice`, replacing it with the floor (Decision 2) and, for a genuinely empty pool, the hold (Decision 6). **Also convert the D14 `open_failed` retry in `assembleClaim`**: when an auto-picked token will not open, retry onto another *pooled* token (or floor), never `workerSecretID(wkr)`'s nil→default. Rewrite the affected unit tests (`TestAutoFallsBackToTheOwnerDefaultAndSaysWhy`, `TestAutoRetriesOnceWhenThePickWillNotOpen`, `TestAutoRetryIsOnceOnly`) — they encode the old behavior and live in the ordinary gate.

5. **Exclude-relax is a CALLER decision, not a change to `Select`'s contract (M3).** M1 keeps `Select`'s `exclude` semantics intact; the relax lives in `autoChoice`/the resume path deciding whether to pass `exclude = uuid.Nil`. The just-parked credential stops being excluded once its own window has demonstrably reopened (`limit_dead_secret_id`'s run had `retry_not_before` = that reset, which has passed by promotion). Because `limit_dead_secret_id` persists until a credential is recorded (corrected fact above), this is readable at the resume claim. Keep the exclude in force while the token is still within its window.

6. **Empty-pool hold: a distinct non-locking state (M4).** Introduce a run state (e.g. `pool_wait`) that is **excluded from `uq_runs_one_active_per_issue`** so a held run does not pin the issue, with a `claimed → pool_wait` transition (a new query — it does not collide with `SetRunLimitWait`'s `running`-only guard) and a surfaced "add a token to the pool" reason. Reachable on both a fresh claim and a resume once the floor has nothing to floor to. Decide the interaction with issue dedup deliberately: a non-active hold means a second run could be created on the same issue — confirm that is acceptable (the held run is inert) or gate creation on it. Bound it: it resumes on populate (M5), and needs a clear cancel path; it must not fail on a `MAX_PARK`-style ceiling the way a usage park does (that would defeat "hold until you add a token").

7. **Reactive resume is a Go-side pass, not SQL (M5).** `Select` is Go and cannot run inside the set-based `PromoteLimitWaitRuns` `UPDATE`. Reactive resume is a **separate** sweeper pass: list `pool_wait` runs, for each read the owner's live `ListAutoSelectCandidates`, and promote the ones a token is now available for (a token opted into the pool; optionally a pooled token turning eligible). It operates on the `pool_wait` state only, so the shared `limit_wait` promotion is untouched (this is the discriminator BLOCKER-1 asked for). Bound the cost (index the `pool_wait` subset; batch per user). **Re-add stagger**: this pass bypasses the park-time jitter that is "the only mechanism spreading a promoted wave", so it must promote at most one held run per newly-available token per tick, or apply its own jitter, to avoid a stampede (the live case had 3 runs on one token).

8. **Manual resume-now is its own verb, `RequireUser` (M5).** Not an `expedite` flag — `expedite` bumps a *queued* run's priority without changing status, while resume-now transitions a *held* run. New route in the **`RequireUser`** group (Bearer `uzc_` or cookie — the CLI uses a `uzc_` Bearer and 401s on cookie-only `RequireAuth`; precedent: `expedite`/`SetRunPriority` is on `RequireUser` with a comment saying exactly this), with a **router-level auth test** (`cli_auth_livedb_test.go` is the pattern; a fake-client unit test bypasses the real router). A CLI verb (`uzi run resume-now <id>`) wired in `api/cmd/uzi` + the embedded `SKILL.md`; a web control. Owner-only; a non-held run is 409, unknown/foreign 404. M5's reactive pass already auto-resumes on populate, so this is the on-demand override.

9. **Observability + failure-sentence hygiene (M2/M4/M7).** A `pool_wait` hold is a *distinct status*, so it is already distinguishable from a `limit_wait` usage park in `run get`/`run list` (no marker column needed). If `health_reason` carries the "add a token" sentence, ensure the hold path does not clear health the way the park statements do. Do **not** route an empty-pool/pool-exhausted outcome through `limitFailureReason` — it hard-codes "Anthropic usage limit … reached", which is false for a pool/config cause. The wait budget (`RUN_LIMIT_MAX_WAITS`) and `limit_wait_count` are the usage-park's; the floor and the `pool_wait` hold must **not** consume them.

## Milestones

### Phase 1 — Stop spending the non-pooled default (the bug fix; each milestone independently shippable)

- [x] **M1 — autoselect: empty-vs-excluded split + the floor tier.** In `api/internal/autoselect`: (a) distinguish "pooled but all excluded" from "nothing pooled" (Decision 1); (b) add the last-resort floor ranking over pooled candidates including stale/below-threshold, deterministic tie-break (Decision 2), as a distinct outcome the caller opts into. Keep the package pure/total/order-independent (no new import); extend `select_test.go` over hand-written fixtures (no DB) — including the `AllReasons` count assertion if the enum changes. No claim-behavior change yet.

- [x] **M2 — Claim lane: never the non-pooled default; floor to the pool.** In `api/internal/workersvc`, implement the `autoChoice` precedence ladder (pick → floor), removing `GetDefaultUserSecretMeta`/`lowestAltCandidate` (Decision 4). **Convert the D14 `open_failed` retry in `assembleClaim`** to a pooled-only re-pick/floor, never `workerSecretID(wkr)`→default. Add the new reason value(s) + the new migration and move every vocabulary guard in the same commit (Decision 3). Rewrite the old-behavior unit tests. `default`/`pinned`/`chat`/`self_improve`/`judge` must be byte-unaffected. **This milestone alone stops meta from ever being spent** — the observed bug is fixed here. Live-DB test: single pooled token, resume from park with cristi still stale → the run floors onto **cristi** (never meta).

- [x] **M3 — Exclude-relax on window reopen (caller-side).** In `autoChoice`/the resume path (not `Select`), stop excluding the just-parked credential once its window has reopened (Decision 5), so the resume re-picks it or floors onto it. Anchor on the corrected clear point (`SetRunAnthropicSecret`, so `limit_dead_secret_id` persists across the hold). Live-DB test: resume after cristi's window reopens → picks cristi with `reason = auto` (fresh gauge) or floors onto cristi (stale gauge); the exclude stays in force while the window is still closed.

### Phase 2 — Empty-pool hold + reactive/manual resume

- [x] **M4 — Empty-pool distinct non-locking hold.** Add the `pool_wait` state (Decision 6): a new migration for the state + the CHECK, exclusion from `uq_runs_one_active_per_issue`, a `claimed → pool_wait` transition query, a surfaced "add a token to the pool" reason (not via `limitFailureReason`, Decision 9), and the sweeper/UI plumbing to render it. An auto claim with a genuinely empty pool holds here instead of spending the default or failing. Confirm the issue-dedup interaction (Decision 6). Live-DB test: empty pool → holds in `pool_wait`, does not pin the one-active index, spends nothing.

- [x] **M5 — Reactive resume (Go-side) + manual resume-now.** A sweeper pass that promotes `pool_wait` runs when a pooled token becomes available (Decision 7), Go-side (not an SQL predicate), scoped to `pool_wait`, with per-token stagger/jitter against a stampede, bounded/batched. Plus the manual `uzi run resume-now <id>` verb + `RequireUser` route + router-level auth test + web control (Decision 8). Because a new/edited query is unverified until executed (`.claude/rules/go.md`), the promotion query's live-DB coverage lands **in this milestone** (token opted into the pool → held run resumes; several held runs on one newly-eligible token → they do not stampede).

- [x] **M6 — Live-DB coverage sweep + gate.** Fill the remaining scenarios not already covered by M2/M3/M5, each with a flip-one-variable positive control, asserting on **content** (resulting state/reason/spent credential), never a bare count: `wait_on_limit=false` + pool exhausted → floors onto a pooled token (does **not** fail, does **not** spend the default); a picked token that will not open → floors/holds, never the default (the D14 path); a `default`/`pinned` worker unaffected; a multi-pooled-token user where one is exhausted and one eligible → picks the eligible one. **No wall-clock**: backdate `runs.retry_not_before` / `anthropic_rate_limits` rows via inline `now() ± interval` SQL (the `run_limit_wait_integration_test.go` convention — there is **no** existing run/gauge clock helper; `setWorkerClock` is worker-only, so this milestone adds its own setup). Run under `./e2e/run-store-it.sh` with a positive control (`RUN > 0`, zero `SKIP`); the ordinary gate runs with `UZI_TEST_DATABASE_URL` unset. `task gate:api` + touched `gate:web`/`gate:agent` green.

- [x] **M7 — Docs, copy, and D7/D14 reference updates.** Update `ARCHITECTURE.md`'s autoselect/bind-mode section and the autoselect docs to state the pooled-only invariant, the floor, and the empty-pool hold; document resume-now; record the D7 reversal in the decision log. Update the **scoped** PRD #111 D7 / #217 references — at least `specs/ai.md` (the PRD #217 M2 spec line that states a single-pooled-token user resumes on their token "because D7 outranks this feature" — the exact behavior this reverses), the CLI help in `api/cmd/uzi/worker.go` ("auto never fails a run for want of a candidate" becomes false), and the web copy in `web/src/pages/WorkersSettings.tsx` / `web/src/lib/runCredential.ts` that says auto "spends the default". CLI parity per repo convention; confirm no `.github/workflows/**` in the branch diff.

## Success criteria

1. An `auto` worker **never** spends a token whose `auto_eligible = false`, on any branch — pick, fallback, or the D14 open-failed retry (verified by live-DB tests, not inspection).
2. With a single pooled token that is rate-limited and the owner default out of the pool, a resuming run continues on the **pooled** token (picked fresh, or floored while its gauge lags), never on the out-of-pool default — the observed bug is gone.
3. A deployment with the poller disabled (or all pooled tokens stale) keeps auto runs alive on a **pooled** token via the floor — no regression to failure — and still never spends the non-pooled default.
4. A genuinely empty pool holds the run in a non-locking state with an "add a token to the pool" reason, does not pin the issue's one-active lock, and resumes automatically when a token is opted in; a user can also resume-now on demand.
5. Reactive resume does not stampede multiple held runs onto one newly-available token; the floor and the empty-pool hold do not consume the usage-limit wait budget.
6. `default`/`pinned` bind modes and `chat`/`self_improve`/`judge` runs are behaviorally unchanged; `Select` stays pure and order-independent.
7. `task gate:api` (and touched web/agent gates) green; no `.github/workflows/**` in the branch diff.

## Risks & mitigations

- **Poller-disabled / backoff regression.** Nothing `Measured` → a hold could never resolve → failure. *Mitigation*: the floor spends a stale pooled token rather than holding on a non-empty pool (Success Criterion 3), so the disabled-poller path behaves like today minus the non-pooled default. Resolved fork (Q1).
- **Empty-pool forever-pin / fresh-claim blast radius.** A `limit_wait` hold pins the issue and is reachable on a fresh claim. *Mitigation*: the distinct non-locking `pool_wait` state (Decision 6, resolved fork Q2), excluded from the one-active index, with a cancel path and no usage-park ceiling.
- **Reactive-resume stampede.** Bypassing park-time jitter re-groups a promoted wave. *Mitigation*: Decision 7 — Go-side pass, one held run per newly-available token per tick or its own jitter.
- **Wait-budget confusion.** Reusing park machinery would burn `RUN_LIMIT_MAX_WAITS`. *Mitigation*: the floor **runs** (no park) and the `pool_wait` hold is its own state (Decision 9) — neither touches `limit_wait_count`.
- **Second default-fallback branch (D14) missed.** *Mitigation*: M2 explicitly converts the `assembleClaim` `open_failed` arm and rewrites its test; Success Criterion 1 says "any branch".
- **Vocabulary/CHECK drift.** A new reason in Go but not in SQL is a failed run at claim. *Mitigation*: a **new** migration (00089 is applied), moved with `TestSelectReasonVocabularyMatchesCheck`, the CLI/web renderers, and the count assertion in one commit (Decision 3).
- **sqlc accepts a query Postgres rejects; a mutation must hit the generated const.** *Mitigation*: every new query has an executing live-DB test in its own milestone (M2/M3/M5), not deferred; regenerate with `sqlc generate` (v1.31.1) and confirm the generated const moved (`.claude/rules/go.md`).
- **Floor thrash.** Flooring onto a genuinely-still-exhausted token re-hits the limit. *Mitigation*: on a resume the exclude-relax only lifts the token once its window reopened, so the floor spends it after the reset; on a fresh claim the residual is one usage park, bounded and self-correcting.
- **Offline-worker constraint (this PRD goes to uzi in Auto mode).** Fully internal-codebase; every anchor, query, migration, and route is verifiable in a clone with no open web. The one empirical fact (the live behavior) is resolved in the Problem section from CLI output captured 2026-08-28; the worker re-derives it from the cited code. File:line anchors drift — the symbol names and files are the durable references; re-grep rather than trusting a line number.

## Decision log

- **2026-08-28**: An `auto` worker spends **pooled tokens only** — PRD #111 D7's owner-default fallback is removed. Liveness comes from a floor **within the pool**, not from spending an out-of-pool credential. (Solution invariant)
- **2026-08-28 (fork Q1, resolved)**: When no pooled token is pickable, spend the **best pooled token including a stale/below-threshold one** (last resort), never the non-pooled default and never a bare failure. This removes the need for a claim-time park on a non-empty pool and fixes the poller-disabled regression. (Decision 2, precedence step 2)
- **2026-08-28 (fork Q2, resolved)**: A genuinely empty pool holds in a **distinct non-locking state** (`pool_wait`), excluded from the one-active-per-issue index, resuming on populate — not a `limit_wait` hold (which pins the issue and burns the wait budget) and not a fallback to the default. The distinct state doubles as the reactive-promotion discriminator. (Decision 6)
- **2026-08-28**: `Select`'s empty-vs-excluded split is a pure boolean; the floor is a distinct opt-in outcome; the exclude-relax is a **caller** decision, so `Select`'s `exclude` contract is untouched. (Decisions 1, 2, 5)
- **2026-08-28**: The D14 `open_failed` retry in `assembleClaim` is a second default-fallback branch and must also be converted to pooled-only. (Decision 4, found in review)
- **2026-08-28**: Reactive resume is a **Go-side** sweeper pass over `pool_wait` (not an SQL predicate on the shared `PromoteLimitWaitRuns`), with per-token stagger against a stampede. (Decision 7, found in review)
- **2026-08-28**: Resume-now is its **own** `RequireUser` verb with a router-level auth test, not an `expedite` overload. (Decision 8)
- **2026-08-28**: Corrected facts from review — `limit_dead_secret_id` clears at `SetRunAnthropicSecret` (not `SetRunRunning`); the only misreported case is excluded-sole-token → `pool_empty` (stale already reads `pool_stale`); a new reason needs a **new** migration (00089 is applied). (Problem, Decision 3)
- **2026-08-28 (fork Q3, resolved)**: After this revision, the PRD is sent to uzi in **Auto** mode (gated; budget scales to the frozen milestones).
