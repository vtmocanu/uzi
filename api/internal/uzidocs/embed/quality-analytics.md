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
deleted is not counted, so a zero is a zero of what is still stored, not proof
that nothing happened. Only totals of retained rows (runs, attempts, targets)
are lower bounds. Class counts are not: deleting a rework run can move its PR
into the `zero` group, and deleting a review moves a target from `complete` to
`unreviewed`, which raises that count.

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
  window or the harness, kind and repo filters. On the owner route and an
  admin-selected `user=<uuid>`, a witness from another user never establishes a
  merge. Admin all-users accepts any user's witness. Cohort PRs without a
  witness are not in the groups: they are `unwitnessed_prs`. `applicable_prs` is the witnessed cohort
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
  PRs, which means one external PR connected more than once. On the owner route
  and an admin-selected user, detection uses only the scoped user's own
  connections, and `shared_detection_unavailable` is the selected PRs whose
  connection is missing or not the scoped user's, so they were left out of it.
  Admin all-users uses every user's connections, so there
  `shared_detection_unavailable` is the selected PRs whose repo has no
  connection. In practice it is always 0 there, because `repos.connection_id`
  is `NOT NULL` and cascades on connection deletion.
- **`buckets`.** The witnessed PRs per earliest-cohort-run bucket, with the
  same four groups.
- **`retention`** is always `retained_runs_only`: a deleted rework run lowers
  its PR's group (a PR can fall into `zero`) and is not a zero-rework proof, and
  a deleted witness run can move a PR into `unwitnessed_prs`. The association is
  only as complete as the retained links; legacy rework runs that lack them are
  not counted.
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
(`claude`, `codex`, `unknown`). There is no time restriction on the lead run,
and none on the attempt's creation time: time evidence is classified as dated
or undated. Two kinds of attempt are still counted nowhere, and neither is
disclosed in the response: a dated attempt with a future `decided_at` (one
decided before `from` is simply outside the window; see `undated` below), and
an attempt whose stage cannot apply to
its lead kind (see the population rule below).

The population is per stage, so counts always agree with `applicable`: a plan
attempt counts only when its lead kind is plan cross-checkable, and a code
attempt only when its lead kind is code cross-checkable (the kinds in
[Applicability](#applicability)). For `kind=task`, plan is
`applicable: false` with zero counts, and plan attempts retained on other lead
kinds are not counted. Plan attempts on task leads, and any attempt on a chat,
judge or job lead, are excluded under every filter and are not disclosed.

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
- **`undated`** counts every non-pending attempt that is not dated. An attempt
  is dated when `decided_at` is present and finite, `created_at` is finite
  (`-infinity` and `+infinity` are not), and `decided_at` is not earlier than
  `created_at`. A far-future `created_at` makes an attempt undated only when
  `decided_at` is earlier than it; with `decided_at >= created_at` the attempt
  is dated. Undated attempts are in no bucket and the window is not applied to
  them (`window: "not_applied"`); they have no creation-time cutoff. The dated
  series is bounded by `decided_at` in `[from, now)`. The dated rule has no
  upper bound, so a non-pending attempt with a finite future `decided_at` (or a
  far-future `created_at` with `decided_at >= created_at`) is dated but outside
  `[from, now)`, and it is counted nowhere: not in a bucket, not in `undated`.
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
evidence: any user's merge witness and every user's forge connections count
there. The same-owner checks still apply: a review counts only if its user is
the target's owner, and a rework link is verified only when the target and the
rework share the owner.

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
- Rework counts retained runs only. Deleted rework runs and missing links
  lower a PR's group (a PR can fall into `zero`); an unverified link is
  reported and not counted.
- Merge-day activity and run-end-to-merge duration are not tracked (null with
  `not_tracked`).
- Shared external PR detection covers the scoped user's own connections on the
  owner route and an admin-selected user, and every user's on admin all-users.
- Pending is a live count of any age and is not tied to the window.

## Performance limit

Measured on a synthetic seed of 140,169 runs (120,000 base plus 20,169 rework
runs; 200 owners with skew, the largest owner 19.9% of runs), 92,547
cross-checks and 35,818 reviews, PostgreSQL 17.12 with `work_mem` 4MB. The
request times below ran through the production router with JIT off and warm
caches (sequential requests, so they do not exercise admission):

| Request | p50 | p95 |
|---|---|---|
| Owner `30d` (largest owner) | about 0.20 s | about 0.21 s |
| Owner `window=all` (largest owner) | about 0.53 s | about 0.54 s |
| Admin all-users `30d` | about 0.60 s | about 0.74 s |
| Admin `user=<uuid>` `30d` (largest owner) | about 0.19 s | about 0.20 s |

The all-users all-time scope was measured only at the statement level, with
the statement timeout disabled and JIT on: about 2.2 s median on one synthetic
seed (the `EXPLAIN ANALYZE` runs reached about 2.3 s). It is not offered, and that timing is an observation on this seed, not a
bound on smaller windows or on HTTP latency. No index was added. With JIT on,
a candidate index appeared to cut all-users 30d custom p95 by 23.5%; the cause
is undetermined (an index-driven plan change or a different statistics
sample). The decision rests on a production-like JIT-off check, where the best
candidate changed the p95 by between -6.1% and +5.6% in both plan modes, so
the 20% bar is not met. That check is one run at n=20 on a fresh reseed whose
counts differ slightly from the main run (140,113 runs against 140,169). The evidence and the seed's limits are in the PRD
(`prds/done/4730-scoped-run-quality-api.md`, section 8).
