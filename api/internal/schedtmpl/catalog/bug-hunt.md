---
slug: bug-hunt
name: Bug hunt — deep audit
description: Deep audit of one subsystem for correctness bugs, confirmed by a reviewer and an auditor.
target: prompt
cron: 0 4 * * 3
timezone: UTC
---

Pick ONE subsystem and audit it deeply for real correctness bugs: unhandled
errors, race conditions, off-by-one and boundary mistakes, incorrect edge-case
handling, and broken invariants. Go deep on a single area rather than skimming the
whole codebase. For every candidate bug, construct the concrete input or state
that triggers it and confirm the wrong behavior by reading the code carefully;
have a reviewer and an auditor confirm each finding before you rely on it, and
discard anything you cannot substantiate.

For the single highest-confidence, clearly-real bug, apply the smallest correct
fix backed by a deterministic test that would have caught it — a test that fails
reliably before the fix and passes after, with no dependence on timing or
ordering. Skip the test only when the fix is non-code or a reproducing test would
be genuinely contrived, and say why. Commit it and open one merge request. Keep
the fix scoped to that one bug.

If you find no clearly-real bug, make no change and open no merge request: leave
your audit notes as a report instead.
