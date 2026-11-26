---
title: Quality analytics API
order: 48
audience: user
---

# Quality analytics API

A read-only API for how finished work held up: how often merged PRs needed
rework, how CI-fix runs ended, how plan and code cross-checks came out, and how
much of the judge-eligible work has a completed review. Everything is computed
on the server from one database snapshot. It is API only: there is no web page
or `uzi` command for it yet, and the existing run, review and cross-check
routes are unchanged.

It is built on the same boundary as the [Usage analytics API](usage-analytics.md)
(Options and Spend): the same routes shape, selectors, validation, errors,
admission, calendar and filter options. Read that page for the shared rules;
this page covers what is specific to Quality.

Quality reports **retained** evidence. A run, review or cross-check that was
deleted is not counted, so a count is a lower bound and a zero is a zero of
what is still stored, not proof that nothing happened.

## Routes and authorization

| Scope | Route | Who |
|---|---|---|
| Owner | `GET /api/analytics/quality` | Any signed-in user (session or `uzc_` Bearer). Always the caller's own runs. |
| Admin | `GET /api/admin/analytics/quality` | An admin session, or a Bearer token with the `admin_ro` scope. |

- On the admin route `user=<uuid>` selects one user. `user` omitted or
  `user=all` (byte-identical) spans every user.
- The owner route never reads a user from the query: `user` there is a 400. An
  ordinary Bearer token never reaches the admin route, even when its owner is
  an admin (403). Unauthenticated callers get 401.
- The filter options (repos, users, windows, kinds, harnesses) come from the
  existing `/options` endpoints; Quality adds no options of its own.

## Parameters and errors

| Parameter | Values | Default | Notes |
|---|---|---|---|
| `window` | `7d`, `30d`, `90d`, `all` | `30d` | Calendar dates in `tz`, see [Calendar](usage-analytics.md#calendar). |
| `tz` | an IANA name | `UTC` | Same rules as Spend. |
| `repo` | a repo UUID | none | Hyphenated 36-character form only. |
| `harness` | `claude`, `codex` | none | Filters the run (the cross-check lead run), not the checker. |
| `kind` | a run kind, `delivery` or `supporting` | all kinds | Decides which measures apply, see [Applicability](#applicability). |
| `user` (admin only) | `all` or a user UUID | `all` | |

- `split` and `breakdown` are rejected: Quality has no grouping controls. Any
  other parameter is a 400 `unknown parameter`. Repeated and empty parameters
  are invalid, and a query string that does not parse is a 400 `invalid query`,
  exactly as for Spend.
- Every invalid value is a 400 naming its parameter (`invalid window`,
  `invalid tz`, `invalid repo`, `invalid user`, ...) and never echoes the value.
  A `repo` that does not exist and one the scope cannot see give the identical
  `invalid repo`, so a repo id cannot be probed.
- **Admin all-users `window=all` is refused.** With `user` omitted or
  `user=all`, `window=all` is a 400 `window all not available for all users`.
  The answer is identical for both spellings, is given before any analytics
  database work (authentication runs first) and is not a 503. Options does not
  list `all` for that scope. The owner route and admin `user=<uuid>` accept
  `window=all`. This follows the owner decision recorded for Spend.

## Admission and timeouts

Same as [Spend](usage-analytics.md#admission-and-timeouts), with section key
`quality`:

- One request per caller per section may be in flight (the owner and admin
  variants of `quality` share the slot), and capacity is a quarter of the
  database pool. Nothing queues. A request that finds no slot answers 503 with
  `Retry-After: 1` before any analytics database work.
- An admitted request has an 8 second budget. Hitting the statement timeout
  also answers 503 with `Retry-After: 1`. Other database failures are a plain
  500 with no detail.

## Clock and snapshot

- The request clock (`now`) is captured in Go when the request arrives. It is
  never part of the database snapshot. It fixes today's local date, and so the
  window start and the bucket boundaries (the calendar), and it is the cutoff
  for the response's `to` and `generated_at`, the end of the last bucket, and
  the upper bound on stored times.
- Every value comes from one `REPEATABLE READ` read-only snapshot. The snapshot
  is fixed by the first query inside the transaction, not by the clock.
- A write committed after the clock was captured but before the first query is
  visible, and counts when its stored time is before `now`. A write committed
  after the first query is not visible and appears on the next request.
- Reviews, the pending cross-check count and the rework merge witness have no
  `now` bound: they read whatever the snapshot holds, so a merge visible in the
  snapshot counts regardless of time. See the measures below.

The calendar (half-open Go-computed buckets, `all` as Monday-anchored weeks
capped at 104 buckets, partial last bucket) is described in
[Usage analytics: Calendar](usage-analytics.md#calendar). Quality has no
previous period and no rolling series: `previous_from`, `previous_to` are
absent and `rolling` is an empty array. Every calendar bucket is present, empty
ones included, and `index` in a measure's bucket is the zero-based position in
the response's `buckets` array.

## Response shape

The response is the shared envelope (`scope`, `generated_at`, `window`,
`timezone`, `from`, `to`, `history_start`, `bucket_width_days`, `buckets`,
`rolling`), `selected_user` (`all`, or the user id; the caller on the owner
route) and four measures: `rework`, `ci_fix`, `cross_check` (`plan`, `code`,
`pending`) and `judge`. The Go fixtures pin the shape:
`fixtures/api-contract/analytics_quality.full.json` (every field present) and
`analytics_quality.zero.json` (the marshalled zero-value DTO, which pins the
nullability surface with null `rolling`, `limitations` and `kinds`; it is not a
real response, which always emits `[]`, the three limitation strings and the
effective kinds). Every count is a number of
distinct items of its section's population, every class map has a fixed key
list where an absent class is `0`, and arrays are never null.

Every measure carries `applicable`, `reason` and `time_basis`.

| Measure | `time_basis` | Buckets are cut on |
|---|---|---|
| `rework` | `pr_first_cohort_created_at` | The creation time of the PR's earliest run that matches the filters. |
| `ci_fix` | `created_at` | Run creation time. |
| `cross_check.plan`, `cross_check.code` | `decided_at` | When the attempt was decided, independent of when its lead run was created. |
| `cross_check.pending` | `live` | Not bucketed: a current count. |
| `judge` | `target_created_at` | Creation time of the reviewed run. |

### Applicability

A measure whose population cannot occur under the `kind` filter reports
`applicable: false`, `reason: "not_applicable"` and zero counts. Otherwise
`reason` is null.

| Measure | Applicable when the kinds include |
|---|---|
| `rework` | any delivery kind |
| `ci_fix` | `ci_fix` |
| `cross_check.plan` | a plan cross-checkable kind: `issue`, `prompt`, `self_improve`, `ci_fix`, `mr_rework` |
| `cross_check.code` | a code cross-checkable kind: `issue`, `prompt`, `self_improve`, `ci_fix`, `mr_rework`, `task` |
| `cross_check.pending` | either of the two above |
| `judge` | a judge-eligible kind: `issue`, `ci_fix` |

### Rework

Merged PRs grouped by how many retained `mr_rework` runs they have.

- **Population.** A PR is the pair (repo, PR number); identity is
  connection-local, as in Spend. The cohort is the distinct PRs of runs created
  in the window that pass the repo, harness and kind filters (delivery kinds
  only). It is one row per PR before anything else is joined, so a PR is
  counted once however many runs it has.
- **Merged.** A PR counts only when a merge witness exists: any run of the
  scoped user with that identity and state `merged`, even one outside the
  window or the harness, kind and repo filters. A witness from another user
  never establishes a merge. Cohort PRs without a witness are not in the
  groups: they are `unwitnessed_prs`. `applicable_prs` is the witnessed cohort
  and is the total of `groups`.
- **Groups.** `zero`, `one`, `two`, `three_plus`: the number of distinct
  retained `mr_rework` runs associated with the PR through its identity and a
  verified link, manual and out-of-window reworks included (rework runs are not
  limited by the window, harness or kind filters, only by the ownership boundary
  and `now`). A rework is verified when its target run carries the same repo, PR
  number and owner. `zero` means zero matched retained runs.
- **`unverified_links`.** Retained reworks on a selected PR whose target run is
  missing, deleted or does not match. They are in no group.
- **`shared_external_prs`.** Selected PRs whose external identity (forge type,
  connection base URL, project, PR number) is shared by two or more selected
  PRs, which means one external PR connected more than once. The count is
  within the scoped user's own connections. `shared_detection_unavailable` is
  the selected PRs whose connection is missing or not the scoped user's, so
  they were left out of that detection.
- **`buckets`.** The witnessed PRs per earliest-cohort-run bucket, with the
  same four groups.
- **`retention`** is always `retained_runs_only`: a deleted rework run is a
  lower count and not a zero-rework proof. The association is only as complete
  as the retained links; legacy rework runs that lack them are not counted.
- **`merge_day_activity` and `run_end_to_merge_seconds`** are always null with
  reason `not_tracked`. The merge state is the last one uzi saw, and the merge
  moment itself is not recorded.

### CI-fix activity

`ci_fix` runs created in the window, bucketed by creation time, with the repo
and harness filters applied. `runs` is `finished + in_flight`. A finished run
has one outcome class (`completed`, `cancelled`, `plan_rejected`,
`base_diverged`, `failed`, the same classes as the Spend outcomes); an
in-flight run has none.

### Cross-check

Attempts are selected through their **lead run**: the lead's repo, harness and
kind (any kind except `cross_check`) decide whether an attempt is in scope.
The checker's harness is separate metadata, reported as `checker_harness`
(`claude`, `codex`, `unknown`). There is no time restriction on the lead run.

Plan and code are separate measures with separate stored fields. Plan reports
the stored verdict (`approve`, `revise`, `block`, `failed`). Code reports the
stored outcome (`completed`, `failed`); the legacy code verdict field is
inert and never exposed. Raw findings and prompts are never returned.

- **`decided_in_window`** counts attempts decided in the window, by
  `decided_at`, however old their lead run. Replaced and interrupted attempts
  are decided attempts too and appear in `classes.replaced`, except a plan
  attempt whose verdict (`approve`, `revise` or `block`, with `reason_class`
  equal to that verdict) was recorded before the interruption (`decided_at <=
  interrupted_at`): it keeps its class (`passed` or `raised_issues`).
- **`classes`** classify each decided attempt the way the cross-check history
  does: `passed`, `raised_issues`, `didnt_finish`, `replaced`, `unclassified`.
  For code, a `completed` attempt is `raised_issues` when it has findings and
  `passed` when it has none; findings that cannot be counted are
  `unclassified`.
- **`undated`** counts decided attempts with no usable decision time (missing,
  non-finite, or earlier than the attempt's creation) or a non-finite creation
  time (for example a `-infinity` `created_at`). They are in no bucket
  and the window is not applied to them (`window: "not_applied"`).
- **`pending`** is a live count of attempts that are undecided and not
  interrupted, of any age, split into `plan` and `code`. The window is not
  applied and there is no `now` bound. It discloses the filters it applied in
  `effective_filters` (`repo`, `harness`, `kinds`). A code checker still
  winding down after a decision is not pending.

### Judge coverage

Of the runs the judge could review, how many have a completed review. Targets
are `issue` and `ci_fix` runs created in the window (after repo, harness and
kind filters), grouped by the target's creation time.

- **`candidates`**: targets that reached `completed` or `failed`.
- **`eligible`**: candidates minus those the judge skips by their stable
  failure origin. `total = unreviewed + failed + complete.total`.
  `unreviewed` has no review (`unreviewed_skipped` is the part with a recorded
  judge skip), `failed` has a review whose own status is `failed`, and
  `complete` splits into the verdicts `ideal`, `ok` and `issues`. Only a
  `complete` review counts toward a verdict.
- **`excluded`**: `cancelled`, `in_flight` (not yet completed or failed),
  `skip_origin` (candidates removed by failure origin) and `reviewed_excluded`
  (the part of `skip_origin` that nevertheless has a review).
- **`coverage`** is `complete.total / eligible.total`, null with reason
  `no_applicable_runs` when nothing is eligible. Each bucket carries the same
  ratio.
- Reviews have no `now` bound and are the retained latest: the store keeps one
  review per target, so a rejudge overwrites it and can change the verdict of
  an older cohort. Historical review versions cannot be reconstructed.
- `selection` is `retained_target_properties`. The eligible denominator is
  derived from properties the target run still carries, so `limitations` names
  what it cannot know historically: `credential_availability_not_historical`,
  `consent_not_historical` and `spend_guards_not_historical`. A run that
  could not be reviewed then, for one of those reasons, sits in `unreviewed`.

## Ownership boundary

The owner route and an admin-selected `user=<uuid>` count only that user's
evidence. This covers every source, not only the starting cohort:

- the cohort and the merge witnesses (a witness from another user never counts);
- the rework runs and their links (the target must belong to the same user);
- the forge connection used to detect a shared external PR;
- the cross-check leads;
- the judge reviews (a review counts only for its target's owner).

Admin all-users applies no scope-user filter, so it reads every user's
evidence, but the same-owner checks still apply: a review counts only if its
user is the target's owner, and a rework link is verified only when the target
and the rework share the owner.

## Repo visibility

Quality has no per-repo rows, so there are no repo ids or paths to mask. The
only repo id in a response is the caller's own `repo` filter, echoed in
`cross_check.pending.effective_filters.repo` after the route has confirmed the
scope may see it. Which repos a scope may filter by is the same as for Spend.

## Limitations

- Judge denominators are derived from retained target properties:
  credential availability, consent and spend guards are not historical (see
  [Judge coverage](#judge-coverage)).
- Judge verdicts are the retained latest review. A rejudge changes the verdict
  of the cohort the target was created in.
- Rework counts retained runs only. Deleted runs and missing links lower the
  counts; an unverified link is reported and not counted.
- Merge-day activity and run-end-to-merge duration are not tracked (null with
  `not_tracked`).
- Shared external PR detection covers the scoped user's own connections only.
- Pending is a live count of any age and is not tied to the window.

## Performance limit

Measured on a synthetic seed of 140,018 runs (120,000 base plus 20,018 rework
runs; 200 owners with skew, the largest owner 20% of runs), 92,420 cross-checks
and 35,722 reviews, PostgreSQL 17, default settings. The request times below
ran through the production router with JIT off and warm caches (sequential
requests, so they do not exercise admission):

| Request | p50 | p95 |
|---|---|---|
| Owner `30d` (largest owner) | about 0.21 s | about 0.21 s |
| Owner `window=all` (largest owner) | about 0.54 s | about 0.55 s |
| Admin all-users `30d` | about 0.63 s | about 0.65 s |
| Admin `user=<uuid>` `30d` (largest owner) | about 0.21 s | about 0.22 s |

The all-users all-time scope was measured only at the statement level
(about 2.4 s); it is not offered, so that figure is an upper bound for the
windows a request can reach. No index was added: the candidates changed the p95
by at most 7%. The evidence and the seed's limits are in the PRD
(`prds/done/4730-scoped-run-quality-api.md`, section 8).
