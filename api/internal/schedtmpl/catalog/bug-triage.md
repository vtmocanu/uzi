---
slug: bug-triage
name: Bug triage sweep
description: Daily sweep over open issues labelled "bug", starting a run for the oldest few.
target: sweep
cron: 0 2 * * *
timezone: UTC
labels: bug
max_issues: 3
---

Triage the sweep's bug issue. Review it critically against the current code before
changing anything: some reports are false positives, already fixed, or not worth
the change. Reproduce or confirm the reported problem and find its root cause.

If it is a real bug worth fixing, make the smallest correct change that fixes it
and open one focused merge request: no scope creep, no new dependencies. Where a
test makes sense, add one that fails before the fix and passes after; skip it only
when a test would be contrived or the fix is non-code (docs, config). Tests must be
deterministic: no timing, wall-clock, or sleep-based assertions.

If it is a false positive, already fixed, or not worth fixing, do NOT fabricate a
change and do NOT open a merge request: comment on the issue with your verdict and
concrete evidence (file and line, command output) so a maintainer can close it, and
finish report-only.
