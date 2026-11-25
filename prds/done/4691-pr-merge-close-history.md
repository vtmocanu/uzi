# PRD #4691: Record pull request merge and close history

**Issue:** #4691.
**Status:** Done, 2026-11-25: provider timestamps, atomic per-PR history in `SetRunMRState`, LiveDB and mutation coverage, operator doc.
**Priority:** Normal.
**Blocked by:** none.
**Scope:** Neutral forge data, the three forge drivers and the forgesvc watcher test fakes, additive history migrations, watcher persistence, docs/specs/tests. No new API route, DTO, user interface or command. No changes under `.github/workflows/**`, including validation. Strict Goose with no allow-missing; migration numbers are assigned at merge time.
**Evidence baseline:** `591871c34b25348dd29becf57fa3928d83faf3ae`.

## Progress

- [x] M1 provider merge/close timestamps and durable per-PR history

## 1. Problem

`runs.mr_state` records only the current state a watcher last saw. When a PR actually merged or closed is not stored anywhere, several runs can reference one PR, and a later observation overwrites the earlier one. History that is not recorded at observation time cannot be reconstructed later: the forge's timestamps are not refetched once a watcher stops watching a PR, so every week without capture is history lost for good.

## 2. Outcome

The existing watcher fetches already return provider merge and close timestamps. They are kept, and every connection-owner state transition the watcher persists is written atomically into a per-PR head plus an append-only event table, with actual provider time and observation time kept apart. Nothing reads this history yet; it starts accumulating now so that later read APIs have real data.

Acceptance examples:

1. Two runs reference one PR merged at 14:02, polled at 14:05. The history holds exactly one first-merge event with provider time 14:02 and observation time 14:05.
2. A provider response reports merged state without a merge timestamp. The event is stored with source `observation_fallback` and no provider time.
3. A watcher first observes an already merged PR. The event carries the real provider timestamp and is marked as a bootstrap, not a witnessed transition.
4. Repeated observations of an unchanged state write no event.

## 3. Out of scope

- Any read API, DTO, page or command over this history (a later issue).
- Additional forge requests, broader watcher eligibility or rewatching terminal PRs.
- Changing board moves, rework cancellation ordering, run outcomes or forge permissions.
- Backfilling PRs no watcher observes, or claiming complete history before tracking started.
- Cross-owner global PR dedup, per-PR cost allocation, chat accounting or workflow changes.

## 4. Modules and seams

### Provider timestamps, verified offline

Extend neutral `forge.MergeRequest` (`api/internal/forge/codehost.go`, today `IID`, `State`, `WebURL`) with nullable `MergedAt` and `ClosedAt`, keeping absent distinct from zero. The existing fetches already return the objects:

| Driver | Existing fetch | Pinned SDK fields (verified in the local module cache at the pinned versions) |
|---|---|---|
| GitHub | `github_mr.go`, `PullRequests.Get` | go-github v92.0.0 `PullRequest.MergedAt`, `ClosedAt` (`*Timestamp`) |
| GitLab | `gitlab.go`, `MergeRequests.GetMergeRequest` | client-go/v3 v3.40.0 `BasicMergeRequest.MergedAt`, `ClosedAt` (`*time.Time`) |
| Forgejo | `forgejo_mr.go`, `GetPullRequest` | gitea SDK v0.25.1 `PullRequest.Merged` (`merged_at`), `Closed` (`closed_at`) (`*time.Time`) |

No upstream lookup is left to the worker. Map all three drivers; `forgetest.BaseFake` keeps its loud not-stubbed default (it has nothing to map) and the forgesvc watcher fakes carry the timestamps; method signatures need not change. Preserve merged-before-closed state classification: a merged PR's close timestamp is not an unmerged close.

Validate timestamps: absent or zero is unknown; a future or inverted timestamp is stored as unknown rather than producing a negative duration later. Keep provider time and observation time in separate columns with a fixed source marker (`provider` or `observation_fallback`). Never store observation time as if it were the provider's merge time.

### Atomic `SetRunMRState` persistence

`SetRunMRState` (`queries/runtime.sql`) stays the only SQL writer of `runs.mr_state`; its callers in `forgesvc/mr_watch.go` and `forgesvc/service.go` keep their behaviour. Extend its single statement with data-modifying CTEs, used by both watcher lanes:

1. Capture the target run's previous state and repository/PR identity under its existing write lock, then `UPDATE runs ... RETURNING` the identity and observed data.
2. Upsert one per-PR head keyed by `(repo_id, mr_iid)`. On conflict, advance its observation watermark for any strictly newer observation (even when the state is unchanged) and change its state only when the known state differs; return the previous head state through an explicit previous-state column (PostgreSQL 16 and 17 both run in production and compose: do not use `RETURNING OLD/NEW` aliases).
3. Insert the event from the returned head transition. For a new head, the previous state comes from the run's pre-update `mr_state`. Mark bootstrap provenance explicitly.

The upsert serializes several referencing runs. A unique partial index permits at most one first-merge event per PR, and the head keeps first-merge knowledge, so a later observation cannot create a second first merge. Head and events cascade with the repo (`ON DELETE CASCADE`). `(repo_id, mr_iid)` means forge connection + repository + PR number; the same external PR connected by two owners is two identities.

**Observation ordering.** Two runs referencing one PR can commit forge observations out of order. Capture the observation time in Go when the forge fetch returns and pass it into the statement. The head keeps an observation watermark: the time of the newest observation that reached it. Every strictly newer observation that reaches `SetRunMRState` advances the watermark, including one that only confirms the head's current state; an event is inserted only when the state changes. An observation older than the watermark still updates its own run's `mr_state` exactly as today, but changes no head and writes no event. A first head insert for a run that already carried a pre-tracking `mr_state` is a bootstrap from that state. The watchers' existing same-state early return means a missing provider timestamp is never enriched by a later identical observation; document that coverage limit rather than changing watcher control flow.

**Tenant boundary.** Runs carry independent owner and repo ids, so a shared `(repo_id, mr_iid)` head could let one owner's observation shape another owner's history. Record history only for observations from runs whose owner is the owner of the repo's forge connection; any other observation updates `runs.mr_state` as today and is outside history coverage (documented). This keeps one owner per head and needs no owner-scoped dedup.

State, head and event commit together. A failure keeps the existing watcher log-and-retry behaviour. Do not advance the head if a prerequisite cancellation failed. Unknown states write nothing. Do not add a forge request to enrich history; board-free terminal self-eviction stays a documented coverage limit.

### Docs and specs

Document the recorded history (fields, sources, bootstrap, coverage limits, no expiry) in an operator- or contributor-audience doc page with valid frontmatter and a unique order, run `task docs:sync`, and add a terse `specs/human.md` entry tagged `(AI-synced YYYY-MM-DD)`. Never edit frozen `specs/ai.md`.

## 5. Testing decisions

- Unit tests for each driver's timestamp mapping (nullable and real values) and for watcher behaviour through fakes: both lanes, cancellation and write failures with retry, unknown states, locked-to-terminal, supported reopen handling.
- LiveDB tests only in `store`, executing the real statement on PostgreSQL 17: CTE pre-state correctness, concurrent duplicate observations of one PR from two runs, a delayed older `opened` observation arriving after `merged` (no head change, no event), a genuine `closed → opened → closed` cycle (three events), `opened` at T1 then an `opened` confirmation at T3 from another run then a delayed `closed` at T2 (watermark at T3, no close event), first-merge uniqueness, rollback, bootstrap from a pre-tracking `mr_state`, fallback source, stable observations, a run whose owner is not the connection owner (no history written, `mr_state` still updated).
- Observe each regression-like mutation fail and the restored code pass (dedup, source selection, pre-update capture), per `.claude/rules/go.md`.
- **In-run checks:** `task check:api`; `go test -race -count=1` on the affected packages, one package per command; the new LiveDB cases run by name against PostgreSQL 17 with positive RUN/PASS counts and no SKIP (Docker required); `task check:migration-numbering` and `task check:migration-additive`; `task docs:sync` plus `task check-docs:web`; `check:changelog-entry` with one lowercase UUID-v4 changelog fragment. The e2e forge-fake is unchanged (e2e records `observation_fallback`); still sweep changed identifiers with `git grep -F` across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh`, `e2e/forge-fake/`, run `task test:e2e-lib` and `task test:forge-fake` when anything there changes, and name unrun phases.

## 6. Milestones

### M1: Provider merge/close timestamps and durable per-PR history

**Blocked by:** none.

Deliver the neutral fields and all three driver mappings, the forgesvc watcher fake updates, additive head/event migrations, the atomic `SetRunMRState` capture, first-merge uniqueness, docs/specs/tests.

Acceptance: examples 1 to 4 hold; out-of-order observations never move the head backwards; only connection-owner observations enter history; actual, fallback and bootstrap sources are distinct; several referencing runs yield one merge; no extra forge call or watcher expansion; real PostgreSQL CTE and concurrency tests pass. Attach query plans for the extended statement, its measured latency change, WAL bytes per event and the storage growth measurement per event and head row.

### Landing obligations (not milestones)

Before merge, the exact PR head must pass full `test:api`, `gate:repo`, `scan:secrets`, `sast:semgrep` and the LiveDB sweep in PR CI. Report the measured change in CI job time.

## 7. Decision Log

| Decision | Reason; rejected alternative |
|---|---|
| Capture now, read later | History not recorded at observation time cannot be recovered; reject waiting for a read API before recording. |
| Provider times with labelled observation fallback | Existing fetches already carry timestamps; reject treating poll time as merge time. |
| One atomic `SetRunMRState` statement | Preserves the sole-writer and retry invariants; reject a separately committed history write. |
| Per-PR head plus first-merge uniqueness | Serializes references and remembers prior state; reject per-run duplicate events. |
| Observation-time ordering on the head | Out-of-order commits from several runs must not rewind history; reject trusting commit order. |
| Connection-owner observations only | One owner per head, no cross-owner influence; reject a shared head with later owner filtering, which cannot undo dedup. |
| Preserve existing watcher coverage | Avoids unbounded terminal polling; disclose missing cycles rather than claim full history. |
| Existing `sweeper.Pass` (superseded by the no-retention row below) | No new scheduler or loop. |
| No retention for now | No retention for now: storage grows by one event row per PR state change plus one head row per PR; a retention pass is a later additive change if growth ever matters. Rejected: a 400-day pass (adds a sweeper pass, lock class and deadline handling for negligible storage, and deletes the history the analytics work exists to keep). Supersedes the "Existing `sweeper.Pass`" row. |
| Decline both fake changes | `forgetest.BaseFake` keeps its loud not-stubbed default because it has nothing to map, and the forgesvc watcher fakes carry the timestamps; the e2e forge-fake stays unchanged because e2e records `observation_fallback`. Rejected: stubbing timestamps in the shared fake and the e2e forge-fake, which adds surface for no coverage. |
| `FOR NO KEY UPDATE OF r` on the run row | Serializes writers of the run without blocking FK `KEY SHARE` from referencing rows. Rejected: `FOR UPDATE`, which would block them. |
| SQL-owned timestamp normalisation | A future or inverted provider time is nulled in SQL, where the observation time and head watermark are known; Go maps only nil, zero and out-of-range times. Rejected: duplicating the comparison in Go. |
| Bootstrap provenance | A fresh head insert carries `from_state` = the run's prior known `mr_state`, even when equal to the new state, and the events CHECK is `bootstrap OR from_state IS DISTINCT FROM to_state`. Rejected: dropping the prior state or forbidding equal bootstrap events. |
| Accept the cross_check-child delete deadlock | A repo or connection delete racing the watcher on a cross_check child run with an MR can deadlock; either side may get 40P01; both outcomes pinned for the repo delete by `TestPRHistoryCrossCheckChildDeleteDeadlockLiveDB` (connection delete and retry untested); a losing delete returns the handler's generic HTTP 500 and can be retried. Rejected: new locking in the delete queries for a rare, retryable race. |
| Measurements (synthetic, PG17) | Synthetic data, local PG 17.12 container on tmpfs, single client, 10,000 PRs per phase (method and summaries in the PR description). Latency per call, old UPDATE vs new statement (mean): bootstrap 1102 vs 1278 µs (+16%), state change 1085 vs 1296 µs (+19%), first merge 1086 vs 1297 µs (+19%), no-change confirmation 1070 vs 1244 µs (+16%); absolute values include a lookup harness shared by both. Extra WAL per call: about +650 B bootstrap, +870 B state change, +1190 B first merge (extra partial-index entry), +485 B no-change (head watermark update). EXPLAIN (ANALYZE, BUFFERS, WAL): one runs_pkey index scan, a head upsert on pr_history_heads_pkey and an event insert (0 rows when unchanged). Storage after 10,000 PRs x 3 events: about 223 B per event row and about 206 B per head row in total relation size (heap 117 / 157 B, indexes 104 / 44 B, the rest free-space and visibility maps), i.e. a few hundred bytes per PR state change. Not measured: concurrency, fsync on real disk, production-size runs rows. |

**UNVERIFIED before implementation:** mapping behaviour against every live forge deployment. Write latency, WAL bytes and storage were measured on synthetic data only (see the Measurements row); concurrency, real-disk fsync and production-size rows are unmeasured. SDK field availability is verified locally; no external lookup is left to the worker.
