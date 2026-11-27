---
title: Pull request merge and close history
order: 71
audience: operator
---

# Pull request merge and close history

uzi's MR-state watcher records the pull request (PR) or merge request state
changes it persists, with the forge's own merge/close time kept apart from the
time uzi noticed. Not every fetched change is persisted: a failed prerequisite
(such as a board move or rework cancel), a run whose owner does not own the
forge connection, and an older or tied observation all leave history unchanged
(see Coverage limits). Nothing reads this history yet: no API route, page or command
exposes it (PRD #4691). It accumulates so that later read features have real
data, because the forge's timestamps are not refetched once a watcher stops
watching a PR. History that was not captured when the watcher saw it cannot be
reconstructed.

## What is recorded

Two tables, written by the same SQL statement that writes `runs.mr_state`
(`SetRunMRState`), so the history commits in one transaction (one autocommit
statement) with `runs.mr_state` and has exactly its durability.

**`pr_history_heads`**: one row per PR, keyed by `(repo_id, mr_iid)`.

| Column | Meaning |
|---|---|
| `state` | Current known state: `opened`, `closed`, `merged` or `locked`. |
| `observed_at` | Observation watermark: the newest observation time that reached the head. |
| `last_provider_at` | Newest provider-reported merge/close time seen. Never decreases. |
| `first_merged_at`, `first_merged_source` | The first merge uzi recorded for the PR: the provider's merge time when `first_merged_source` is `provider`, otherwise (`observation_fallback`) the time uzi first recorded the PR as merged. Set once. |
| `last_changed`, `last_from_state`, `last_to_state`, `last_event_provider_at`, `last_time_source`, `last_bootstrap`, `last_first_merge` | Transport-only: they carry the latest transition to the event insert inside the statement. They are not history; do not read them. |

**`pr_history_events`**: append-only transitions.

| Column | Meaning |
|---|---|
| `from_state`, `to_state` | The transition. `from_state` is NULL when no prior state is known. |
| `provider_at` | The forge's own merge/close time, or NULL. |
| `time_source` | `provider` (`provider_at` is set) or `observation_fallback` (`provider_at` is NULL). |
| `observed_at` | When uzi observed the transition (captured in Go right after the forge fetch returns). |
| `bootstrap` | True for the first observation of a PR, which is not a witnessed transition. |
| `first_merge` | True on the one event where uzi first recorded the PR as merged. A unique partial index allows at most one per PR. |

`(repo_id, mr_iid)` means forge connection, repository and PR number: the same
external PR connected by two owners is two identities. Unchanged repeated
observations write no event. Observation time is never stored as if it were
the provider's time: when the provider sent none, the event is
`observation_fallback` with `provider_at` NULL.

## Provider timestamps per driver

| Driver | Source fields |
|---|---|
| GitHub | `merged_at`, `closed_at` of the pull request |
| GitLab | `merged_at`, `closed_at` of the merge request |
| Forgejo | `merged` (`merged_at`) and `closed` (`closed_at`) of the pull request |

For a `merged` observation the merge time is used and the close time is
ignored (a merged PR's close time is not a separate event); for `closed` the
close time is used. The mapping is verified against the pinned SDK field
definitions only. **Mapping against live forge deployments is UNVERIFIED.**

### Zero and absent timestamps

In Go, a nil, zero, or outside [1970-01-01, 10000-01-01) (UTC) provider time is
treated as unknown. In SQL, a provider time after the observation time, or
older than the newest provider time already on the head, is also dropped to
NULL, so the event is `observation_fallback` rather than carrying an inverted
or backwards time.

## Bootstrap

The first observation of a PR (no head row yet) writes a bootstrap event.
Its `from_state` is the run's last recorded `mr_state`; NULL means no known
prior state. SetRunCompleted assigns `mr_iid`, so if a run's `mr_iid` was
rewritten after a state was recorded, that prior state may belong to the
run's earlier MR rather than the current one.

## Retention

There is no age-based expiry. History rows are deleted only by the repository
cascade (`ON DELETE CASCADE` from `repos`). Each PR state change inserts one event row (a few hundred bytes) and upserts
the PR's single head row.

## Delivery semantics

Delivery is best effort, never exactly-once. A lost reply (the write
committed but the caller saw an error) leaves no duplicate: the observation
watermark and the changed-state guard stop a repeat from writing an event, and
the first-merge unique index plus `first_merged_at` on the head stop a second
first merge. A write that really failed is re-observed on the next eligible
tick, with a later `observed_at`.

Isolation is READ COMMITTED. The statement locks the run row with
`FOR NO KEY UPDATE`, then upserts the head; observations of one PR from several
runs serialize on the head, and one older than or tied with the watermark
changes neither the head nor the events (the run's own `mr_state` is still
updated).

## Coverage limits

History is what the watcher saw, not a complete record of every PR.

- **Same-state early return.** The watcher skips an observation whose state
  equals the stored `mr_state`. A transition that is reverted before the next
  observation is not seen, and a provider time missing from the first recorded
  event is never back-filled by a later identical observation.
- **Terminal states stored before tracking.** A PR whose terminal state was
  already stored in `mr_state` before history existed may go uncaptured; later
  eligible transitions are recorded.
- **Non-owner observations.** History is recorded only when the run's user owns
  the repository's forge connection. Any other observation still updates
  `mr_state` but writes no history.
- **Issue-lane watch rules.** On a repository without an external issue
  tracker, the watcher observes only an issue's latest run, and only while that
  run is `completed`. For an open issue it also needs the card in Human Review
  or a stored `mr_state` of `closed`; for a closed issue it stops once the run's
  `mr_state` is merged or closed, so a PR reopen while the issue stays closed is not recorded. An MR
  outside those conditions is not observed, so its close or merge is not
  recorded (the close-edge gap below is one case of this). Repositories linked
  to an external issue tracker are watched by the board-free lane instead
  (next item).
- **Board-free self-eviction.** Board-free runs leave the watch candidate set
  once their `mr_state` is terminal, so they are no longer observed. One
  exception: on a repository linked to an external issue tracker, an issue's
  latest run with `mr_state` `closed` stays watched while the issue is open, so
  a PR reopen is recorded; a merged run still leaves the set.
- **Close-edge gap.** On the opened-to-closed edge the board card move runs
  first and the record second. If the move succeeds and the record fails, that
  close can go unrecorded, as it can for `mr_state` already.
- **Older, tied and clock-skewed observations.** An observation older than or
  tied with the head's watermark is dropped. Observation times come from each
  API replica's clock, so clock differences between replicas can drop or
  misorder a transition.
- **Rolling deploy.** Pods still on the old version write `mr_state` without
  history while an upgrade rolls out.
- **Down migration.** Rolling the migration back drops both tables and the
  history is irrecoverable; the transition times and observation provenance
  exist only here.
- **Cross-check child delete deadlock.** A repository or forge-connection
  delete racing the watcher on a `cross_check` child run that carries an MR
  can deadlock (SQLSTATE 40P01): neither delete prelocks a `cross_check`
  child. If the watcher loses, it retries on the next tick and the repo is
  gone. If the delete loses, the handler returns its generic error (HTTP 500)
  and the user can retry the delete. The watcher's write commits independently
  of that response and may still be in flight when the error arrives, so an
  immediate retry can race it again. This is accepted without extra locking. Both outcomes
  are pinned for the repository delete by
  `TestPRHistoryCrossCheckChildDeleteDeadlockLiveDB` in
  `api/internal/store/pr_history_livedb_test.go`; the connection-delete case
  and the retry are not covered by a test.

## See also

- [PRD #4691](../prds/done/4691-pr-merge-close-history.md) for the design and Decision Log.
