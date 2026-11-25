# PRD #4691: Record pull request merge and close history

**Issue:** #4691.
**Status:** Ready.
**Priority:** Normal.
**Blocked by:** none.
**Scope:** Neutral forge data, the three forge drivers and the shared test fake, additive history migrations, watcher persistence, one bounded retention pass, docs/specs/tests. No new API route, DTO, user interface or command. No changes under `.github/workflows/**`, including validation. Strict Goose with no allow-missing; migration numbers are assigned at merge time.
**Evidence baseline:** `591871c34b25348dd29becf57fa3928d83faf3ae`.

## Progress

- [ ] M1 provider merge/close timestamps and durable per-PR history

## 1. Problem

`runs.mr_state` records only the current state a watcher last saw. When a PR actually merged or closed is not stored anywhere, several runs can reference one PR, and a later observation overwrites the earlier one. History that is not recorded at observation time cannot be reconstructed later: the forge's timestamps are not refetched once a watcher stops watching a PR, so every week without capture is history lost for good.

## 2. Outcome

The existing watcher fetches already return provider merge and close timestamps. They are kept, and every state transition the watcher observes is written atomically into a per-PR head plus an append-only event table, with actual provider time and observation time kept apart. Nothing reads this history yet; it starts accumulating now so that later read APIs have real data. Bounded retention keeps storage finite.

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

No upstream lookup is left to the worker. Map all three drivers and update `forgetest.BaseFake` and timestamp-carrying test stubs; method signatures need not change. Preserve merged-before-closed state classification: a merged PR's close timestamp is not an unmerged close.

Validate timestamps: absent or zero is unknown; a future or inverted timestamp is stored as unknown rather than producing a negative duration later. Keep provider time and observation time in separate columns with a fixed source marker (`provider` or `observation_fallback`). Never store observation time as if it were the provider's merge time.

### Atomic `SetRunMRState` persistence

`SetRunMRState` (`queries/runtime.sql`) stays the only SQL writer of `runs.mr_state`; its callers in `forgesvc/mr_watch.go` and `forgesvc/service.go` keep their behaviour. Extend its single statement with data-modifying CTEs, used by both watcher lanes:

1. Capture the target run's previous state and repository/PR identity under its existing write lock, then `UPDATE runs ... RETURNING` the identity and observed data.
2. Upsert one per-PR head keyed by `(repo_id, mr_iid)`. On conflict, advance its observation watermark for any strictly newer observation (even when the state is unchanged) and change its state only when the known state differs; return the previous head state through an explicit previous-state column (PostgreSQL 16 and 17 both run in production and compose: do not use `RETURNING OLD/NEW` aliases).
3. Insert the event from the returned head transition. For a new head, the previous state comes from the run's pre-update `mr_state`. Mark bootstrap provenance explicitly.

The upsert serializes several referencing runs. A unique partial index permits at most one first-merge event per PR, and the head keeps first-merge knowledge after event retention, so a later observation cannot create a second first merge. Head and events cascade with the repo (`ON DELETE CASCADE`). `(repo_id, mr_iid)` means forge connection + repository + PR number; the same external PR connected by two owners is two identities.

**Observation ordering.** Two runs referencing one PR can commit forge observations out of order. Capture the observation time in Go when the forge fetch returns and pass it into the statement. The head keeps an observation watermark: the time of the newest observation that reached it. Every strictly newer observation that reaches `SetRunMRState` advances the watermark, including one that only confirms the head's current state; an event is inserted only when the state changes. An observation older than the watermark still updates its own run's `mr_state` exactly as today, but changes no head and writes no event. A first head insert for a run that already carried a pre-tracking `mr_state` is a bootstrap from that state. The watchers' existing same-state early return means a missing provider timestamp is never enriched by a later identical observation; document that coverage limit rather than changing watcher control flow.

**Tenant boundary.** Runs carry independent owner and repo ids, so a shared `(repo_id, mr_iid)` head could let one owner's observation shape another owner's history. Record history only for observations from runs whose owner is the owner of the repo's forge connection; any other observation updates `runs.mr_state` as today and is outside history coverage (documented). This keeps one owner per head and needs no owner-scoped dedup.

State, head and event commit together. A failure keeps the existing watcher log-and-retry behaviour. Do not advance the head if a prerequisite cancellation failed. Unknown states write nothing. Do not add a forge request to enrich history; board-free terminal self-eviction stays a documented coverage limit.

### Bounded retention pass

Use the existing `sweeper.Pass` (`api/internal/sweeper/sweeper.go`) on the existing sweep cadence (default 15 s, operator setting preserved), with a dedicated session advisory lock constant registered beside the existing ones in `store/migrate.go`, reusing `workersvc/checkpoint_retention.go`'s pinned-connection, nonblocking try-lock and proven-unlock/connection-destruction pattern. No new loop or scheduler.

Retain events for 400 days by observation time. Delete in id-ordered batches of at most 1,000 rows, at most four batches and a two-second deadline per pass, one transaction per batch. Keep one fixed-size head per retained PR identity (O(distinct PRs), cascading with the repo).

Measure drain at the effective sweep cadence against a stated peak event inflow, the added delay before the liveness sweep, WAL/row/index footprint and head/event cardinality. If drain cannot keep up within these bounds, stop for a design revision.

### Docs and specs

Document the recorded history (fields, sources, bootstrap, coverage limits, retention) in an operator- or contributor-audience doc page with valid frontmatter and a unique order, run `task docs:sync`, and add a terse `specs/human.md` entry tagged `(AI-synced YYYY-MM-DD)`. Never edit frozen `specs/ai.md`.

## 5. Testing decisions

- Unit tests for each driver's timestamp mapping (nullable and real values) and for watcher behaviour through fakes: both lanes, cancellation and write failures with retry, unknown states, locked-to-terminal, supported reopen handling.
- LiveDB tests only in `store`, executing the real statement on PostgreSQL 17: CTE pre-state correctness, concurrent duplicate observations of one PR from two runs, a delayed older `opened` observation arriving after `merged` (no head change, no event), a genuine `closed → opened → closed` cycle (three events), `opened` at T1 then an `opened` confirmation at T3 from another run then a delayed `closed` at T2 (watermark at T3, no close event), first-merge uniqueness, rollback, bootstrap from a pre-tracking `mr_state`, fallback source, stable observations, a run whose owner is not the connection owner (no history written, `mr_state` still updated), retention batches and deadline.
- Observe each regression-like mutation fail and the restored code pass (dedup, source selection, pre-update capture), per `.claude/rules/go.md`.
- **In-run checks:** `task check:api`; `go test -race -count=1` on the affected packages, one package per command; the new LiveDB cases run by name against PostgreSQL 17 with positive RUN/PASS counts and no SKIP (Docker required); `task check:migration-numbering` and `task check:migration-additive`; `task docs:sync` plus `task check-docs:web`; `check:changelog-entry` with one lowercase UUID-v4 changelog fragment. The forge interface change touches `e2e/forge-fake/`: sweep changed identifiers with `git grep -F` across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh`, `e2e/forge-fake/`, run `task test:e2e-lib` and `task test:forge-fake` when anything there changes, and name unrun phases.

## 6. Milestones

### M1: Provider merge/close timestamps and durable per-PR history

**Blocked by:** none.

Deliver the neutral fields and all three driver mappings, the fake updates, additive head/event migrations, the atomic `SetRunMRState` capture, first-merge uniqueness, the bounded retention pass, docs/specs/tests.

Acceptance: examples 1 to 4 hold; out-of-order observations never move the head backwards; only connection-owner observations enter history; actual, fallback and bootstrap sources are distinct; several referencing runs yield one merge; no extra forge call or watcher expansion; real PostgreSQL CTE and concurrency tests pass. Attach query plans for the extended statement, its measured latency change, WAL bytes per event and the retention-drain measurement.

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
| Existing `sweeper.Pass` | No new scheduler or loop. |

**UNVERIFIED before implementation:** mapping behaviour against every live forge deployment, write latency, WAL bytes and retention drain. SDK field availability is verified locally; no external lookup is left to the worker.
