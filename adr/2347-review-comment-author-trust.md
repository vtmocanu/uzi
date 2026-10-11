# ADR-2347: Author trust for MR review comments

**Status**: Accepted (the maintainer accepted the conditional delay bound on 2026-10-07, a departure from the issue's zero-delay acceptance criterion; see below)
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
resolving a mixed thread resolves the outsider's notes in it too (the whole
discussion on GitLab, the whole review thread on GitHub).
Forgejo keeps its reply-only, inline-only contract.

**Legacy snapshots.** A snapshot whose version is not 2 predates assessment and
may hold unvetted bodies. It is replayed as empty with a fixed note and
authorizes no thread. Runs in flight across the upgrade lose their comments and
reply/resolve; one that already carries a session or a stored plan (context
written after reading the unvetted bodies) is refused terminally at claim
(`guardrail_blocked`) rather than resumed. The version is the review lane's own constant, not an alias of
the issue lane's, so bumping one lane never re-versions the other.

**Pending set.** Unknown actionable comments at or below a new high-water mark
would be skipped forever once the mark moves. Their ids are kept in
`mr_rework_ledger.pending_unknown_ids`, merged in SQL, a consumed id never
re-added, and trigger once their author is eligible. The set holds one id per
unverified author, that author's newest unknown actionable comment id that is
above the previous mark (and at or below the new one) or already pending, capped at 10,000 entries; on overflow the oldest ids are kept,
so a flood arriving after a finding cannot displace it. Near the cap, when superseding could not be guaranteed to fit, an author's older id is kept (so an author may briefly hold two entries) rather than risk losing both. An author's older id is dropped only when its newer representative is retained in the same atomic merge (a stale writer whose add is rejected, or whose replacement another writer removed, leaves the older id in place); a retained replacement takes the older id's place in the cap order within that merge. Both the watcher and the on-demand path read the ledger before listing the comments, so a pending id missing from the listing is treated as deleted. The set accumulates across fires, so displacement needs it to exceed 10,000
entries (one per unverified author; documented residual). A concurrent ledger
writer that moves the mark past a new representative id can drop it, but the older id then stays pending (narrow; overlaps the scalar-mark limitation). If an author deletes their own representative (newest) comment while
older unknown ones remain, those older ones fall back to human review. An id the
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

**Per-lookup timeout.** Author-specific identity and permission requests keep
one 5-second child deadline per lookup inside the 200-author assessment budget:
30 seconds for the background watcher, 5 seconds for on-demand rework across
Begin and Snapshot (#2372). Shared repository evidence uses the remaining
assessment context, not that child deadline; it can consume the remaining
assessment time. The total deadline can cut a lookup short. In the background
watcher, one hanging author-specific call costs one slot, not the tick.

**Shared-evidence condition.** GitHub's repository-wide collaborator list and
Forgejo's shared repository, ownership and direct-collaborator evidence use the
remaining assessment deadline. GitHub can finish from shared results even after
the author's child deadline expires. Forgejo's author-specific permission
fallback retains the original child context: if it has expired, the fallback
yields permission-unknown, while a later author with a fresh child can succeed.
Shared reads remain subject to assessment cancellation and existing pagination
checks; a listing is not guaranteed to succeed. A tick counts in the bound
below only when the required shared evidence answers within the remaining
assessment time. GitLab lookups are per-user calls with no shared evidence; each
is bounded by its child timeout and the assessment deadline.

**Conditional delay bound.** For a waiting author X with R_0 entries ahead that
are not eligible (not-eligible or permission-unknown), let A_t be the lookups actually attempted on tick t (logged; only
A_t <= 200 is unconditional) and E_t the eligible authors ahead. A tick counts
if shared evidence answered in time and A_t - E_t >= 1. X is attempted on the
first counted tick k where the sum of (A_t - E_t) over counted ticks reaches
R_0 + 1; with a constant p = A_t - E_t that is ceil((R_0 + 1) / p) counted
ticks (R_0 = 2, A = 2: tick 2). X fires on that tick if the other gates pass,
otherwise keeps its front place. Uncounted ticks never move anyone ahead of X.
Assumptions: X's author-specific requests answer within the original per-lookup
child deadline and its verdict arrives within the remaining assessment time on
the counted tick (otherwise X becomes permission-unknown, re-queues at the back,
and the bound restarts from its new position); forge calls honor context cancellation; locked queue writes
succeed; required shared evidence arrives within the remaining assessment time; the connection
token's rate limit (shared with other MRs) is not exhausted; other MRs don't
touch this MR's queue (they share only the verdict cache, the rate limit and the
repository's serial detect loop; each MR's assessment has its own 30-second
deadline, so a flooded or hanging MR can cost up to about 30 seconds of that
loop per tick, and the poll interval defaults to 1 minute, `FORGE_POLL_INTERVAL`);
eligible authors are few.

## Departure from the issue's zero-delay acceptance criterion: accepted by the maintainer

The issue asks that an outsider flood not delay an eligible finding. This design
does not meet that literally. It guarantees an eligible finding is **not suppressed by an outsider flood unless
the pending set exceeds 10,000 entries (one per unverified author, accumulated across fires)**, never displaced from the
snapshot's context by outsider comments, and **delayed only by the conditional
bound above**, whose assumptions (forge availability, rate
limit, a few eligible authors ahead) are not under uzi's control. Whether the
conditional bound is acceptable was a maintainer decision: on 2026-10-07 the
maintainer accepted it (#2347). Under a flood an eligible finding may be delayed by
this bound but is never dropped or suppressed.

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
- Resolved in #2372: the on-demand handler selects a 5-second total assessment
  deadline while the background watcher retains 30 seconds. Queue and verdict
  writes and Snapshot keep the original parent context. This leaves headroom in
  the 15-second HTTP write budget when earlier work is prompt; it does not enforce
  an overall request or creation deadline or provide idempotency.
- Not shipped: the CLI cannot edit the allowlist (cookie-only endpoint).
