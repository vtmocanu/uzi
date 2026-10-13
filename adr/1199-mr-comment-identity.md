# ADR-1199: MR review comment identity is (namespace, id)

**Status**: Accepted
**Date**: 2026-10-11
**Issue**: [vtmocanu/uzi#1199](https://github.com/vtmocanu/uzi/issues/1199)

## Context

The MR review watcher and the on-demand rework decide whether a comment is new
by comparing its id with one scalar, `mr_rework_ledger.high_water`. On GitHub
and Forgejo that is wrong: PR conversation comments, inline review comments and
review-summary bodies draw ids from separate sequences, so their numbers are not
comparable. Once a high-numbered top-level comment was consumed, a newer inline
finding with a lower id looked already seen and was skipped. The same scalar was
the filter for the pending set of permission-unknown comments
([ADR-2347](2347-review-comment-author-trust.md)), so a pending id could be
dropped or kept for the wrong reason. Migration 00168 recorded this as a KNOWN
LIMITATION and the watcher's GATE 3 comment accepted it.

## Decision

**Identity.** A comment is identified by `(namespace, id)`; an id is compared
only with ids in the same namespace. The forge driver sets exactly one
namespace per comment (`forge.CommentNS*`):

- GitHub and Forgejo: `issue_comment` (PR conversation comments),
  `review_comment` (inline), `review` (review-summary bodies).
- GitLab: `note` (one notes sequence covers inline and top-level notes).

**Ledger state.** Migration 00323 adds two columns to `mr_rework_ledger`:

- `comment_marks` (jsonb): the highest consumed id per namespace, advanced per
  key only, never backward.
- `pending_unknown_keys` (text[]): one `<namespace>:<id>` key per unverified
  author, capped at 10,000 and kept in admission order. On overflow the
  oldest-admitted keys are kept, so a later flood cannot displace an earlier
  finding, including across namespaces whose ids are not comparable.

`high_water` is still advanced so the column stays readable and a rollback has
a sane legacy mark, but it is not compared on a non-legacy row.
`pending_unknown_ids` is frozen after bootstrap. The rest of the pending-set
behaviour in ADR-2347 (one entry per unverified author, the cap, the "gone"
rule) is unchanged, applied per key.

**Legacy bootstrap.** A row with `comment_marks` NULL was written before the
upgrade or by an old binary. It is bootstrapped on the first successful comment
listing, by the watcher or the on-demand rework. A listing counts as
successful only when complete: drivers return an error rather than a partial
list, and a failed listing leaves the row legacy (the tick does nothing, as
before). The bootstrap runs under the branch lock as a guarded update that
applies only if the row is unchanged since it was read:

- each namespace's mark is the highest id observed in that namespace at or
  below the legacy scalar (0 when there is none);
- comments above the scalar keep today's eligibility;
- each legacy pending id becomes a key in every namespace whose listing has a
  comment with that id. An ambiguous id stays pending in all matching
  namespaces; in a rare collision this can replay one consumed comment once,
  within the cap;
- a legacy pending id with no comment in the complete listing is dropped (the
  existing "gone" rule).

A concurrent change makes the watcher skip the tick (it retries on the next
one) and the on-demand path answer 409 "the review comment state changed while
this request ran; try again".

**Overflow.** If qualifying the legacy pending ids would exceed 10,000 keys,
nothing is written: no partial bootstrap, no truncation, and the cap is not
raised. Run creation is refused. The watcher logs one Warn per MR per tick with
reason `bootstrap_overflow` and the counts; the on-demand path answers 409
"this merge request's review ledger is too large to upgrade automatically; an
operator must reset it (see docs/mr-review-watcher.md)".

### Operator recovery for a bootstrap overflow

Both statements are run by an operator against the database. `<repo uuid>` is
the repository's id and `<branch>` the MR's source branch (`agent/issue-N`).

1. Clear the legacy pending set (keeps the cycle count):

   ```sql
   UPDATE mr_rework_ledger SET pending_unknown_ids = '{}'
   WHERE repo_id = '<repo uuid>' AND ref = '<branch>' AND comment_marks IS NULL;
   ```

   The next watcher tick that lists the comments (green head pipeline, MR
   not halted at the cap), or the next on-demand rework, bootstraps with marks
   only. The previously pending comments
   fall back to human review (the owner can run `uzi run rework` with
   guidance). The automatic cycle count is kept.

2. Delete the row:

   ```sql
   DELETE FROM mr_rework_ledger WHERE repo_id = '<repo uuid>' AND ref = '<branch>';
   ```

   This resets the automatic cycle count and every mark to zero, so every
   eligible actionable comment on the MR counts as new once, bounded by the
   cap. It also gives up the pending set and the halt latch state.

### Rollout and rollback

During a mixed-version rollout an old binary ignores the new columns and
updates only the legacy columns (`high_water`, `pending_unknown_ids`,
`attempt_count`, `halt_notified`). After bootstrap the legacy
`pending_unknown_ids` column is frozen, and a rolled-back or mixed-version old
binary still treats those frozen ids as pending, so it may replay a comment
the new code already consumed, once within the cap (a bare id can also match a
comment in another namespace). A comment it consumed on a non-legacy row may
therefore replay once within the cap, and its legacy-column writes can leave the
namespace marks and pending keys stale relative to the legacy state.

A binary rollback without a database downgrade leaves `comment_marks` and
`pending_unknown_keys` stored. The old binary ignores them, so pending keys
added since bootstrap are not acted on by it: those comments fall back to human
review while it runs. They are not physically lost merely by changing binaries.
On re-upgrade, an already-bootstrapped row stays non-legacy and is not
re-bootstrapped; it resumes from its retained, possibly stale, marks and keys.

Explicitly running the migration Down is different: it drops the new columns,
so marks and keys are gone and a later re-upgrade bootstraps again from the
legacy scalar (and the stays-skipped consequence below applies again).

## Alternatives rejected

- **Order by `CreatedAt`.** Timestamp ties and trust in forge and author clock
  values make "newer" unreliable.
- **An unbounded set of consumed ids.** The set grows with every comment on the
  MR for its whole life.
- **Infer the source from `ReviewState`.** Inline versus summary is not enough:
  two GitHub sources (inline comments and review bodies) share the same summary
  shape, and the issue comment source has neither.
- **Copy the scalar into every namespace at bootstrap.** It would keep
  suppressing new lower-id findings in the namespaces the scalar never
  described, which is the bug being fixed.

## Consequences and residuals

- Supersedes the KNOWN LIMITATION in migration 00168, the watcher's GATE 3
  comment, and the scalar-mark statements in ADR-2347 (the pending-set
  paragraph and the known-residuals bullet).
- A finding skipped before the upgrade, and at or below its namespace's
  bootstrap mark, stays skipped. The owner can use `uzi run rework`.
- Ambiguous legacy pending ids can replay one consumed comment once (above).
- Mixed-version and rollback staleness is bounded by the cap and falls back to
  human review, never to a wrong automatic fix of an unverified author.
- New code never advances a legacy row. The automatic advance is guarded twice
  (the ledger read under the branch lock, and an upsert that writes nothing on a
  legacy row); the on-demand create relies on the read under the branch lock
  alone. Old binaries take the same lock, so a legacy row cannot appear inside
  that window; this is an accepted boundary, not a second guard.
