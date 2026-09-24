# ADR-1543: filter sweep candidates by eligibility in SQL, before the scan window

**Status**: Accepted
**Date**: 2026-09-24
**Deciders**: uzi-agent (M3 of issue #1543) + team lead
**Issue**: [vtmocanu/uzi#1543](https://github.com/vtmocanu/uzi/issues/1543)
**Supersedes**: the eligibility-out-of-SQL decision in [PRD #416](../prds/done/416-sweep-backfill-skipped-slots.md) (Sweep backfill for skipped slots).

## Context

A label sweep's candidate query matched a selector (e.g. the `bug` label) and
ordered oldest-issue-first, but did not know which of those matches were
**eligible** to run (carrying the configured `uzi` label, or assigned to the
uzi-bot account) until each candidate was fetched and passed through
`createRun`'s per-row gate. PRD #416 gave a skipped candidate's cap slot back
to the next candidate ("backfill"), bounded by a scan window (the cap plus a
fixed headroom), but that walk still had to spend window slots *fetching*
every ineligible match before reaching an eligible one.

When the selector's oldest matches were overwhelmingly ineligible — sixteen
old `bug` issues with no `uzi` label ahead of a scan window of thirteen — the
window was exhausted entirely on candidates that could never start, and every
eligible issue behind them was permanently unreachable: raising `max_issues`
only pushed the same wall further out, and the fire looked healthy (it "ran"
every night) while starting nothing.

PRD #416 deliberately kept eligibility out of SQL, because eligibility then
required fetching each issue's PRD-link body check — not answerable from a
`WHERE` clause on the cached `issues` table. That constraint no longer holds:
PRD #764 replaced the PRD-link check with a cached-label check, and PRD #767
added cached bot assignment, so eligibility is now a predicate over columns
the `issues` cache already carries.

## Decision

Filter sweep candidates by eligibility in SQL, **before** `ORDER BY`/`LIMIT`
cuts the scan window, so an ineligible match is never fetched and never
occupies a window slot:

- **`ListSweepCandidateIssues`** gains an eligibility predicate — the
  configured `@uzi_label` (string membership via `jsonb_exists`) OR
  assignment to the bot (`assignee_ids @> to_jsonb(@bot_id)`, guarded by
  `@bot_id > 0`) — mirroring the predicate `ListAutopilotCandidateIssues`
  already uses for autopilot. The assigned-selector kind is eligible by
  construction and short-circuits the check. The two predicates are kept
  byte-identical so "eligible" means the same thing to both.
- **`CountSweepCandidateIssues`** returns two columns from one statement:
  `eligible` (selector matches that also pass the eligibility predicate,
  unlimited) and `matched` (every open selector match, eligible or not).
  `Capped` compares the fetched candidate count against `eligible`, so it
  keeps meaning "more eligible issues exist than the scan window reached."
  `matched - eligible` is the new aggregate diagnostic, `ineligible_matched`:
  how many open issues match the selector but aren't eligible, over the
  sweep's whole backlog rather than just the scan window.
- **`createRun` stays the authoritative per-row gate.** The SQL filter is an
  optimization over the candidate set, not a new source of truth: a race
  between candidate selection and run creation (eligibility changes in
  between) still falls through to the existing `not_eligible` skip.

## Consequences

- A per-issue `not_eligible` skip on a label sweep becomes rare — it fires
  only on the create-time race, or on a pinned-issue fire (which has no
  upfront selector to filter). It is no longer the mechanism a maintainer
  reads to diagnose starvation.
- `ineligible_matched` is the new diagnostic for that case: absent (nil) on a
  fire from before this change, or on a non-label-sweep fire (assigned
  sweeps, pinned issues, prompts), meaning unknown, never zero. Callers
  (`uzi schedule get`, `run-now`, `--json`, the web Last fire panel) must
  treat a missing key as unknown rather than defaulting it to zero.
- The `--max-issues`-raising advice in the CLI's skip-reason and capped-fire
  hints no longer applies and was removed: raising the cap does not help a
  fire starved by a thin eligible backlog, and it also raises how many runs
  a single fire can start per night, which is not the fix for this problem.
- The eligibility rule now lives in **two** places that must move together:
  the SQL predicate (`ListSweepCandidateIssues`/`CountSweepCandidateIssues`)
  and `createRun`'s per-row gate. A future change to what makes an issue
  eligible (a third eligibility path, say) must update both, or the SQL
  filter and the authoritative gate will silently disagree — the SQL side
  would either wrongly exclude an eligible issue from ever becoming a
  candidate, or wrongly admit one that `createRun` then skips anyway.
