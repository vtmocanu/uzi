---
title: Usage analytics API
order: 47
audience: user
---

# Usage analytics API

A read-only API for spend over time: filter options, then a Spend document
with daily (or weekly) buckets, ranked splits, two cost ratios, a cache-read
ratio, the ten most expensive runs and a per-group breakdown. Everything is
computed on the server from one database snapshot. It is API only: there is no
web page or `uzi` command for it yet, and `GET /api/usage` and
`GET /api/admin/usage` are unchanged.

Every dollar figure is recorded spend (an estimate for comparison), not a
bill.

## Routes and authorization

| Scope | Routes | Who |
|---|---|---|
| Owner | `GET /api/analytics/options`, `GET /api/analytics/spend` | Any signed-in user (session or `uzc_` Bearer). Always the caller's own runs. |
| Admin | `GET /api/admin/analytics/options`, `GET /api/admin/analytics/spend` | An admin session, or a Bearer token with the `admin_ro` scope. |

- The owner route never reads a user from the query. An admin calling it sees
  only their own runs, and a `user` parameter there is a 400.
- An ordinary Bearer token never reaches the admin routes, even when its owner
  is an admin (403).
- The paths are literal. Any other `/api/analytics/...` path, such as a section
  that does not exist yet, is a 404.

## Parameters and errors

| Parameter | Values | Default | Notes |
|---|---|---|---|
| `window` | `7d`, `30d`, `90d`, `all` | `30d` | Calendar dates, see [Calendar](#calendar). The admin all-users scope (`user` omitted or `user=all`) has no `all`: Options lists only `7d`, `30d`, `90d` there, and Spend rejects `window=all` with a 400. Owner and admin `user=<uuid>` keep `all`. Options accepts and ignores `window`. |
| `tz` | an IANA name | `UTC` | Must be known to both the database and the server. Empty, `Local` and names over 64 characters are invalid. |
| `repo` | a repo UUID | none | Hyphenated 36-character form only. |
| `harness` | `claude`, `codex` | none | |
| `kind` | a run kind, `delivery` or `supporting` | all kinds | `chat` is accepted but only feeds `untracked_chat_runs`. |
| `split` (spend) | `usage_model`, `harness`, `repo`, `kind`, plus `user` on an admin all-users response | `usage_model` | Top 5 groups plus Other. |
| `breakdown` (spend) | `primary_model`, `harness`, `repo`, `kind`, plus `user` on an admin all-users response | `repo` | Top 50 rows plus Other. |
| `user` (admin only) | `all` or a user UUID | `all` | Omitting it is byte-identical to `user=all`. |

- Every invalid value is a 400 naming its parameter (`invalid window`,
  `invalid tz`, ...). The message never echoes the value, SQL or other
  sensitive text.
- Spend on the admin all-users scope with `window=all` is a 400,
  `window all not available for all users`. It is answered before any
  analytics database work (authentication runs first) and is not a 503.
- Repeated and empty parameters are invalid (`invalid <name>`). An unknown
  parameter is a 400 `unknown parameter`. A query string that does not parse
  (a bad percent-encoding or a `;`) is a 400 `invalid query`; it is never
  silently replaced by defaults.
- `split` or `breakdown` on `/options`, and `split=user` or `breakdown=user`
  anywhere but an admin all-users response, is `invalid split` or
  `invalid breakdown`.
- A `repo` that does not exist and a `repo` the scope cannot see give the
  identical 400 `invalid repo`, so a repo id cannot be probed.
- `user` on the owner route is `user not allowed`. On the admin route an
  unknown UUID is `invalid user`. With `user=<uuid>` the statement is scoped to
  that user's runs, and `split=user` / `breakdown=user` are not offered.
- Unauthenticated callers get 401 and a non-admin caller of an admin route
  gets 403.

## Admission and timeouts

- Capacity is a quarter of the database pool (`max(1, MaxConns / 4)`; the
  default pool of 20 gives 5). One request per caller per section (`options`
  or `spend`) may be in flight, and the owner and admin variants share that
  slot. Nothing queues.
- A request that finds no slot answers 503 with `Retry-After: 1` before any
  analytics database work. Authentication runs earlier (bearer and cookie
  auth may read or update token metadata), so a busy request is not free of
  database access overall.
- An admitted request has an 8 second budget, enforced by a request deadline
  and `SET LOCAL statement_timeout = '8s'`. Hitting it also answers 503 with
  `Retry-After: 1`. Other database failures are a plain 500 with no detail.
- All analytics reads of one request, after authentication (user and repo
  checks, history start, the statement, and the timezone list on its first,
  cached load), run in a single `REPEATABLE READ` read-only transaction, so
  they agree with each other. JIT is off inside it. Authentication reads happen
  before it, and a warmed timezone cache comes from an earlier request, not
  from this snapshot.

## Calendar

Go computes every boundary and the SQL only tests half-open membership
(`start <= created_at < end`) against those instants. Buckets are by **run
creation time**, not by when cost was incurred.

- Buckets are contiguous: bucket D runs from the first instant whose local date
  is D (the first midnight at or after the previous bucket) to the first
  instant of D+1. They tile `[from, generated_at)` with no gaps.
- A local date skipped by a clock change has no instants, so that window has
  one bucket fewer and none is invented. When a clock rolls back across
  midnight, the instants after the rollback that read as the earlier date again
  fall in the later bucket, because a bucket is the interval between two first
  midnights.
- The last bucket ends at `generated_at` and is `partial`. For day buckets,
  right at a local midnight it is `[now, now)`: empty but still present, all
  zeros, with null ratios and their reasons. For `all`, whose buckets are
  Monday-anchored weeks, the final bucket is empty only when `now` is exactly
  at its own boundary (the start of a bucket), not at every midnight.
- `7d`, `30d` and `90d` are the last N local calendar dates including today
  (daily buckets, `bucket_width_days` 1). `previous_from` / `previous_to` are
  the immediately preceding N dates. `all` has no previous period.
- `all` starts at the Monday of the earliest matching run's week
  (`history_start`) and uses Monday-anchored week buckets of width 1, 2, 4, 8
  ... weeks: the smallest width that gives at most 104 buckets
  (`bucket_width_days` is 7 times that). A scope with no runs has no buckets, and for `all` it still reports
  `bucket_width_days` 7 (weekly grain).
- `rolling` has one point per bucket (same index): the seven local dates
  ending at the last date that bucket covers (today for the last bucket),
  whatever the bucket width. It uses real runs before `from` where they exist,
  and is clipped at `history_start`. A point is `partial` when it reaches
  before `history_start` or belongs to today's bucket.

## Cost fold and disclosure

- Cost uses the same fold as the existing usage totals (per-leg, session and
  lineage aware), summed per run and per model. Sums over models reconcile with
  the run's total.
- Each run has one status: `metered`, `subscription` (no estimate recorded),
  `unreported`, or `missing` (no usage rows at all). A run mixing metered and
  subscription legs is `unreported`. Recorded dollars are always kept, so
  partial cost still counts.
- Every block carries `disclosure`: distinct run counts for the four statuses
  plus `incomplete`, true when any run is not `metered` (subscription
  included). Treat dollars as a lower bound when it is true.
- `coverage` is metered runs over applicable non-chat runs. A run with missing
  usage stays in the denominator. Empty population: null with
  `no_applicable_runs`.
- Chat runs are untracked, not zero: they appear only in
  `untracked_chat_runs` (created in the current window) and in no cost, token,
  coverage, ratio or top-run figure.
- `split=usage_model` places a run in every model it used, so its group counts
  and disclosures are not additive (`non_additive: true`); a run with no usage
  is under `unknown`. Other splits count each run once.
- Primary model is the model with the most folded output tokens for the run,
  ties broken by model id order, `unknown` when there is no usage. It is not the
  run's configured model.

## Repo visibility

- On the **owner route** a repo outside the caller's connections is masked: its
  cost is kept under the key `inaccessible_repo` with null id, label and path.
  A run with no repo is `no_repo`.
- On the **admin routes** every repo in the result shows its id and path. With
  `user=<uuid>` that includes a repo the selected user's run references but
  that is on someone else's connection. The `repo` filter and the repo options
  stay limited to the selected user's own connections, so an admin can see such
  a repo but not filter by it.
- Which of the two applies comes from the route that was mounted, never from a
  query parameter.

## Ratios

Every ratio returns `numerator_usd`, `denominator`, `value` and a `disclosure`
for the numerator's runs. A zero denominator gives `value: null` with a reason.

| Ratio | Numerator | Denominator |
|---|---|---|
| `cost_per_completed_run` | Recorded non-chat cost of runs created in the range, any kind that passes the filters. | Completed Delivery runs created in the range. In-flight runs count in none of the finished or completed denominators. |
| `cost_per_merged_pr` | Recorded cost of Delivery runs that carry a PR, created in the range (merged or not). | Distinct PRs in the filtered cohort that have a merged witness (see the rules below). |

Delivery kinds are issue, mr_rework, ci_fix, prompt, self_improve, task and
job. Supporting kinds are judge, cross_check and chat.

Null reasons are a closed set: `no_completed_delivery_runs`, `no_merged_prs`,
`no_applicable_runs`, `no_input_tokens`, `no_finished_runs`, `not_applicable`.

Merged-PR rules:

- A PR is the pair (repo, PR number), and a repo belongs to one forge
  connection. Identity is connection-local, so instance totals can count one
  external PR twice when two owners connected it.
- A PR is merged when any run **in the response's scope** with that identity has
  state `merged`, even a run created outside the window. Owner routes and
  `user=<uuid>` use only that user's runs; admin all-users uses every run.
- The harness and kind filters choose which PRs are counted, not which runs may
  prove a merge, and the witness also ignores the repo and window filters. A
  same-owner `mr_rework` run or a run of another harness can therefore prove a
  PR merged without adding its cost to the filtered cohort.
- The merge state is the last one uzi saw. Board-free watchers stop at terminal
  states (see `api/internal/forgesvc/board_free_mr_watch.go`), so a PR merged
  or reopened after that is not seen.
- Each PR is attributed to one place, with all of its in-range Delivery cost.
  The headline and bucket panel (`[from, generated_at)`) use the bucket of the
  PR's earliest run in the window. The rolling series uses the local day of its
  earliest run in the fold range (`previous_from`, or `history_start` for
  `all`), so it can reach back before `from`.
- Two consequences: a rolling point depends on the window (a PR with runs 20
  and 3 days ago lands on day 3 under `7d` and day 20 under `30d`), and a point
  can include cost from runs created after its own range.
- This differs from the PR counts in `/api/usage`, which count a PR once for
  its earliest run's user and creation time, whether merged or not.

Other measures:

- `cache_read` is `read / (input + read + creation)` tokens over the window's
  non-chat runs. It is a recorded ratio, not proven savings. Null with
  `no_input_tokens` when the denominator is zero.
- `top_runs` are the ten most expensive current-window runs, most expensive
  first. Each carries its own usage status and the same repo masking as above.
- Breakdown `elapsed_p50_seconds` and `elapsed_p90_seconds` cover finished runs
  and measure `finished_at` (else the last status time) minus creation, so they
  include time spent queued or waiting for approval. Other is computed from its
  own runs, not from the rows above it.
- Breakdown `finished` is completed + cancelled + plan_rejected + base_diverged
  + failed, the same classes as the usage outcomes; `failed` excludes the
  other four.

## Options

`/options` returns the repos you can filter by, the users an admin can select
(admin only), and the allowed windows, kinds, harnesses, splits and breakdowns.
On the owner route `users` is null with reason `not_applicable`.

- Each list is capped at 200 and ordered by path (repos) or email (users);
  `truncated` is true when more exist. A valid repo that was cut off is still
  accepted as a filter.
- It reads no usage and no history start, so it is cheap.
- Admin `user=<uuid>` narrows the repo list to that user's connections and
  echoes `selected_user`. The Spend response carries the same `selected_user`
  (`all` or the user id; the caller's id on the owner route).

## Performance limit

Measured on a synthetic 120,000-run seed (845,988 usage legs, 200 owners with
skew, 1,250 repos, three years of history), PostgreSQL 17, default settings,
JIT off. Custom-plan medians (generic plans, the steady state, are within
about 20%: owner `30d` about 0.29 s, owner `90d` about 0.51 s):

| Request | Time |
|---|---|
| Owner `30d` | about 0.25 s |
| Owner `90d` | about 0.43 s |
| Admin all-users `30d` | about 1.3 s |
| Admin all-users `window=all` (measured, not offered) | about 10 s |

**Decision:** admin all-users `window=all` is not offered. Options does not
list `all` for that scope and Spend answers 400 `window all not available for
all users` before any analytics database work (authentication runs first). The measured 10 s is over the 8 second
budget, so the request would otherwise have been a 503. All-time stays
available for the owner route and for admin `user=<uuid>`; owner `all` Spend
measured about 1.8 s (statement-level median, custom plan) on the same seed, and
end-to-end request latency was not measured. Per-run, per-model totals for finished runs now exist (#4700) and make
admin all-users `7d`, `30d` and `90d` about 36 to 43% faster on the same seed once the backfill is complete (with nothing stored they were up to about 4 to 5% slower). All-time still misses the 8 second
budget in the steady state, even with every run stored, because the cost is in building the Spend document over every
run, not in reading the totals. It stays unavailable (owner decision, 2026-11-27); see
[ADR-4700](../adr/4700-run-usage-model-folds.md).
