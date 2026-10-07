# ADR-2347: Author trust for MR review comments

**Status**: Accepted, with one open point: the conditional delay bound departs from the issue's zero-delay acceptance criterion and awaits maintainer confirmation (see below)
**Date**: 2026-10-07
**Issue**: [#2347](https://github.com/vtmocanu/uzi/issues/2347)

## Context

The `mr_rework` lane feeds an MR's review comments to an agent that can push
code. Until now any comment on the MR reached the prompt, whoever wrote it, so
a drive-by commenter on a public repository could steer a run. The issue lane
closed the same hole in #2345: a comment body is only read when its author has
repository access, decided by the forge driver's `RepositoryAuthorEligibility`.
This ADR applies that rule to the review lane, which has three extra problems:
most review bots (CodeRabbit, Greptile) are not collaborators yet are wanted;
reply/resolve tools act on threads and must not touch withheld ones; and an
outsider can flood an MR with comments to starve the one eligible finding that
matters, because each author check costs a forge call.

## Decision

**Eligibility.** One `ReviewAssessor` serves the poller's watcher and the
on-demand rework endpoint. A comment is eligible when its author passes the
shared repository-access check, or is on the admin allowlist. Everything else
is `author_not_eligible` (including user id 0 and cached negative verdicts) or
`permission_unknown` (lookup error, timeout, deadline, budget, or not reached
this tick). Unknown never triggers.

**Allowlist identity and scope.** The admin-only instance setting
`mr_review_trusted_bots` holds `<base_url>#<forge_user_id>` entries. A bot is
identified by forge instance (normalized connection base URL; the default
https port `:443` is treated as absent on both sides, and
`https://api.github.com` counts as `https://github.com`) plus numeric user id,
never by login or a `[bot]` suffix, which are not stable identities. The list
is instance-scoped and admin-only because it widens what any user's agent
reads; it defaults to empty. Allowlisting authorizes ingestion only: bot text
stays untrusted data, and a trusted bot's summary or walkthrough note never
triggers. The web card edits it; the CLI (`uzi admin review-bots`) is
read-only because the write endpoint is cookie-only.

**Withheld, not placeholder.** The issue lane replaces withheld comments with a
placeholder carrying author and time. The review lane omits them: a thread
anchor, author name or timestamp would still be attacker-shaped bytes in the
prompt, and the lane's caps are then spent on eligible comments only. The
snapshot (`version: 2`) carries `withheld_not_eligible` and `withheld_unknown`
counts, rendered as a fixed note outside the nonce fence, so the agent knows
something was held back without any of its content.

**Thread rule.** The existing server-side rule stands: reply/resolve is allowed
only for a thread id present on an included comment of the run's stored
snapshot. A wholly withheld thread is therefore a 403. A mixed thread is
allowed through its eligible comment; the accepted consequence is that
resolving a mixed GitLab discussion resolves the outsider's notes in it too.
Forgejo keeps its reply-only, inline-only contract.

**Legacy snapshots.** A snapshot whose version is not 2 predates assessment and
may hold unvetted bodies. It is replayed as empty with a fixed note and
authorizes no thread. Runs in flight across the upgrade lose their comments and
reply/resolve. The version is the review lane's own constant, not an alias of
the issue lane's, so bumping one lane never re-versions the other.

**Pending set.** Unknown actionable comments at or below a new high-water mark
would be skipped forever once the mark moves. Their ids are kept in
`mr_rework_ledger.pending_unknown_ids` (at most 200), merged in SQL, a consumed
id never re-added, and trigger once their author is eligible. An id the
snapshot caps evict is dropped (human review is the fallback), including on a
tick that creates no run.

**Verdict cache.** Not-eligible answers are cached per repository for 6 hours
(`mr_review_author_verdicts`). Eligible and unknown answers are always
re-asked, so a promoted collaborator is recognized within 6 hours and a revoked
one never rides a cached yes.

**FIFO queue with sequence and lock.** Authors awaiting a lookup wait in a
per-(repo, ref) queue (`mr_review_author_queue`). New authors join behind
waiting ones; unknown or not-eligible results re-queue to the back; eligible
results keep their place. Positions come from a database sequence
(`mr_review_author_queue_seq`, bigint, `CACHE 1 NO CYCLE`), never from
`max(queue_seq)`, and every mutation runs in a short transaction under a
per-(repo, ref) advisory lock that is never held across a forge call, with
`UNIQUE (repo_id, ref, queue_seq)` as a backstop. The prune bound is read
before the comment fetch; a pruner may only delete rows at or below the
sequence value it observed. Because the sequence never reuses a value, a row
admitted after the observation always sorts above it, even if every earlier row
was deleted in between. A `max()`-based scheme would restart at 1 after such a
delete and let a stale pruner reset a waiting author's position. `CACHE 1`
keeps values monotonic in lock order across sessions, which the guard needs;
`NO CYCLE` stops a wrapped value repeating.

**Per-lookup timeout.** Each author lookup is cut off after 5 seconds inside the
30-second, 200-author assessment, so one hanging forge call costs one slot, not
the tick.

**Shared-evidence condition.** Eligibility evidence such as the GitHub
collaborator list is shared across lookups. A tick only
makes progress for the queue when that shared evidence answered within the
per-lookup timeout; a tick where it did not is uncounted in the bound below.

**Conditional delay bound.** For a waiting author X with R_0 not-eligible
entries ahead, let A_t be the lookups actually attempted on tick t (logged; only
A_t <= 200 is unconditional) and E_t the eligible authors ahead. A tick counts
if shared evidence answered in time and A_t - E_t >= 1. X is attempted on the
first counted tick k where the sum of (A_t - E_t) over counted ticks reaches
R_0 + 1; with a constant p = A_t - E_t that is ceil((R_0 + 1) / p) counted
ticks (R_0 = 2, A = 2: tick 2). X fires on that tick if the other gates pass,
otherwise keeps its front place. Uncounted ticks never move anyone ahead of X.
Assumptions: forge calls honor context cancellation; locked queue writes
succeed; shared evidence arrives within the per-lookup timeout; the connection
token's rate limit (shared with other MRs) is not exhausted; other MRs don't
touch this MR's queue (they share only the verdict cache, the rate limit and the
serial detect loop, about 30 seconds plus one timeout per flooded MR per tick);
eligible authors are few.

## Departure from the issue's zero-delay acceptance criterion: pending maintainer confirmation

The issue asks that an outsider flood not delay an eligible finding. This design
does not meet that literally. It guarantees an eligible finding is **never
suppressed and never displaced from the snapshot's context**, and **delayed only
by the conditional bound above**, whose assumptions (forge availability, rate
limit, a few eligible authors ahead) are not under uzi's control. Whether the
conditional bound is acceptable is a maintainer decision; this change must not
merge as fully satisfying the criterion until it is confirmed.

## Alternatives rejected

- **Placeholders for withheld comments** (the issue lane's shape): leaves
  attacker-shaped author and time text in the prompt and spends caps on it.
- **Allowlisting by login or `[bot]` suffix**: both can be claimed or
  re-registered; only the numeric id on a named instance is stable.
- **`max(queue_seq) + 1` positions**: repeat after a delete, defeating the
  stale-prune guard (see above).

## Consequences and residuals

- After upgrade, CodeRabbit, Greptile and other non-collaborator bots stop
  triggering and stop reaching the prompt until an admin allowlists them.
- In-flight `mr_rework` runs from before the upgrade replay no comments and
  cannot reply or resolve.
- An outsider promoted to collaborator is recognized within 6 hours.
- The scalar high-water mark and GitHub's and Forgejo's distinct id sequences
  (existing limitation) also apply to pending-id capture.
- Two forges on one host under different paths share an allowlist key.
- A stale GitHub REST listing can cost a queue position; that affects fairness
  only, never who is trusted.
- The on-demand handler shares the 30-second assessment deadline while the API
  write timeout is 15 seconds (same mismatch as #2372); deferred.
- Not shipped: the CLI cannot edit the allowlist (cookie-only endpoint).
