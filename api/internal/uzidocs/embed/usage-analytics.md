---
title: Usage analytics API
order: 47
audience: user
---

# Usage analytics API

A read-only API for spend, reliability, capacity and flow over time: filter
options, a Spend document with daily (or weekly) buckets, ranked splits, two
cost ratios, a cache-read ratio, the ten most expensive runs and a per-group
breakdown, a Reliability document with run outcomes, failure causes, a rolling
failure rate and a ranked breakdown, a [Capacity](#capacity) document with
approximate concurrent-run occupancy, a usage-wait share and current provider
gauges, and a [Flow](#flow) document with live waits, activity, timing and
delivery evidence. The historical figures are computed on the server from one
database snapshot. It is API only: there is no web page or `uzi` command for it
yet, and `GET /api/usage` and `GET /api/admin/usage` are unchanged.

Options and Spend share this boundary with another section, Quality (retained
rework, CI-fix, cross-check and judge coverage), at
`/api/analytics/quality` and `/api/admin/analytics/quality`. It is documented
in [Quality analytics API](quality-analytics.md); the parameters, errors,
admission and calendar below apply to it too, except where that page says
otherwise.

Every dollar figure is recorded spend (an estimate for comparison), not a
bill. Capacity reports no dollars.

## Routes and authorization

| Scope | Routes | Who |
|---|---|---|
| Owner | `GET /api/analytics/options`, `GET /api/analytics/spend`, `GET /api/analytics/reliability`, `GET /api/analytics/capacity`, `GET /api/analytics/quality`, `GET /api/analytics/flow` | Any signed-in user (session or `uzc_` Bearer). Always the caller's own runs. |
| Admin | `GET /api/admin/analytics/options`, `GET /api/admin/analytics/spend`, `GET /api/admin/analytics/reliability`, `GET /api/admin/analytics/capacity`, `GET /api/admin/analytics/quality`, `GET /api/admin/analytics/flow` | An admin session, or a Bearer token with the `admin_ro` scope. |

- The owner route never reads a user from the query. An admin calling it sees
  only their own runs, and a `user` parameter there is a 400.
- An ordinary Bearer token never reaches the admin routes, even when its owner
  is an admin (403).
- The paths are literal. Any other `/api/analytics/...` path, such as a section
  that does not exist yet, is a 404.
- Spend is the only section with `split` and `breakdown`; Quality rejects both.

## Parameters and errors

| Parameter | Values | Default | Notes |
|---|---|---|---|
| `window` | `7d`, `30d`, `90d`, `all` | `30d` | Calendar dates, see [Calendar](#calendar). The admin all-users scope (`user` omitted or `user=all`) has no `all`: Options lists only `7d`, `30d`, `90d` there, and Spend, Reliability, Capacity, Quality and Flow reject `window=all` with a 400. Owner and admin `user=<uuid>` keep `all`. Options accepts and ignores `window`. |
| `tz` | an IANA name | `UTC` | Must be known to both the database and the server. Empty, `Local` and names over 64 characters are invalid. |
| `repo` | a repo UUID | none | Hyphenated 36-character form only. |
| `harness` | `claude`, `codex` | none | |
| `kind` | a run kind, `delivery` or `supporting` | all kinds | In Spend, `chat` is accepted but only feeds `untracked_chat_runs`. On Capacity it gives empty measures and `excluded_kind_selected` `chat`. Reliability uses only the delivery kinds of the selection, see [Reliability](#reliability). In Flow, chat feeds `waiting_now`, `activity.created`, `first_start_delay` and `plan_response`, but not finished activity, the delivery funnel, approval or `issue_elapsed`. |
| `split` (spend) | `usage_model`, `harness`, `repo`, `kind`, plus `user` on an admin all-users response | `usage_model` | Top 5 groups plus Other. |
| `breakdown` (spend, reliability) | `primary_model`, `harness`, `repo`, `kind`, plus `user` on an admin all-users response | `repo` on Spend, `primary_model` on Reliability | Top 50 rows plus Other. |
| `user` (admin only) | `all` or a user UUID | `all` | Omitting it is byte-identical to `user=all`. |

- Every invalid value is a 400 naming its parameter (`invalid window`,
  `invalid tz`, ...). The message never echoes the value, SQL or other
  sensitive text.
- Spend, Reliability, Capacity, Quality and Flow on the admin all-users scope with `window=all` are a 400,
  `window all not available for all users`. It is answered before any
  analytics database work (authentication runs first) and is not a 503.
- Repeated and empty parameters are invalid (`invalid <name>`). An unknown
  parameter is a 400 `unknown parameter`. A query string that does not parse
  (a bad percent-encoding or a `;`) is a 400 `invalid query`; it is never
  silently replaced by defaults.
- `split` or `breakdown` on `/options`, `/capacity`, `/quality` or `/flow`, `split` on
  `/reliability` (it has a breakdown but no split), and `split=user` or
  `breakdown=user` anywhere but an admin all-users response, is `invalid split`
  or `invalid breakdown`. Flow takes only `window`, `tz`, `repo`, `harness`,
  `kind` and (admin) `user`; any other parameter is `unknown parameter`.
- A `repo` that does not exist and a `repo` the scope cannot see give the
  identical 400 `invalid repo`, so a repo id cannot be probed.
- `user` on the owner route is `user not allowed`. On the admin route an
  unknown UUID is `invalid user`. With `user=<uuid>` the statement is scoped to
  that user's runs, and `split=user` / `breakdown=user` are not offered.
- Unauthenticated callers get 401 and a non-admin caller of an admin route
  gets 403.

## Admission and timeouts

- Capacity is a quarter of the database pool (`max(1, MaxConns / 4)`; the
  default pool of 20 gives 5). One request per caller per section (`options`,
  `spend`, `reliability`, `capacity`, `quality` or `flow`) may be in flight, and the owner and admin variants share that
  slot. Nothing queues.
- A request that finds no slot answers 503 with `Retry-After: 1` before any
  analytics database work. Authentication runs earlier (bearer and cookie
  auth may read or update token metadata), so a busy request is not free of
  database access overall.
- An admitted request has an 8 second budget, enforced by a request deadline
  and `SET LOCAL statement_timeout = '8s'`. Hitting it also answers 503 with
  `Retry-After: 1`. Other database failures are a plain 500 with no detail.
- All analytics reads of one request, after authentication (user and repo
  checks, history start, the statement (Reliability: the cohort statement, then
  the latest-failure statement), and the timezone list on its first,
  cached load), run in a single `REPEATABLE READ` read-only transaction, so
  they agree with each other. JIT is off inside it. Authentication reads happen
  before it, and a warmed timezone cache comes from an earlier request, not
  from this snapshot.

## Calendar

Go computes every boundary and the SQL only tests half-open membership
(`start <= created_at < end`) against those instants. For Spend, buckets are by **run
creation time**, not by when cost was incurred. Quality measures have their
own time bases; see [Quality analytics](quality-analytics.md#response-shape).

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
`no_applicable_runs`, `no_input_tokens`, `no_finished_runs`, `not_applicable`
and `insufficient_runs`. `insufficient_runs` appears only on Reliability: on a
rolling point or a breakdown row over fewer than five finished runs. The
top-level Reliability `reason` is a separate field: `supporting_kinds_only` (or
null), see [Reliability](#reliability).

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

## Reliability

`GET /api/analytics/reliability` (the caller's own runs) and
`GET /api/admin/analytics/reliability` (admin read group) answer how often
finished work fails, why, and where. It takes `window`, `tz`, `repo`,
`harness`, `kind` and `breakdown` (plus `user` on the admin route). It has no
`split`: sending one is a 400 `invalid split`. The response carries the same
calendar envelope as Spend (`scope`, `generated_at`, `window`, `timezone`,
`from`, `to`, `previous_from`, `previous_to`, `history_start`,
`bucket_width_days`, `buckets`, `rolling`, `selected_user`), so
[Calendar](#calendar) applies unchanged. The admin all-users scope has no
`window=all` here either (400 `window all not available for all users`, as
for Spend).

- **Cohort.** Runs are chosen by **creation time**, like Spend: a run belongs
  to the window its `created_at` falls in, whenever it finished. Only delivery
  kinds count; `chat`, `judge` and `cross_check` are in no figure. A run that
  has not finished (in flight) is in no finished figure.
- **Not applicable.** When the `kind` filter selects only supporting kinds
  (`kind=supporting`, `kind=judge`, `kind=chat`, ...), no history, cohort or
  latest-failure statement runs (the user and repo checks still run, so an
  unknown `repo` is still a 400 `invalid repo`) and the response has `applicable: false` with `reason: "supporting_kinds_only"`. Every
  count is zero and every rate is null with reason `not_applicable`, which is
  not a zero failure rate. With `kind=delivery` or any delivery kind,
  `applicable` is true and `reason` is null.
- **Outcome classes.** `completed`, `cancelled`, `plan_rejected`,
  `base_diverged` and `failed`; `finished` is their sum. `failed` excludes
  `plan_rejected` and `base_diverged`, the same classes as the usage outcomes.
  A failed run with no recorded origin is `unknown` in the causes.
- **`totals`.** `current` and `previous` (omitted when the window has no
  previous period, as for `all`) each hold `finished`, `outcomes`,
  `failure_rate` and `requeued_runs`. `failure_rate` is failed over finished,
  never suppressed for a small sample; it is null with `no_finished_runs` only
  when nothing finished.
- **`series`.** One cell per bucket (`series[i]` is `buckets[i]`) with
  `finished` and the outcome counts, plus its own `index` (`series[].index`).
- **`causes`.** The window's failed runs by failure origin: the top five origins
  by runs, then Other. `causes.failed` is the window's failed count and
  `causes.items` (each with `rank`, `is_other`, `origin`, `runs` and `share`; no
  `origin` on Other) sum to it. `share` is runs over `failed`.
- **`rolling_failure_rate`.** `overall` (a list of points) and `harnesses`
  (`[{harness, points}]`, one entry for `claude` and one for `codex`), with one
  point per rolling range (same index as `rolling`). A point
  keeps its real `finished` and `failed` counts; `value` is null with
  `insufficient_runs` below five finished runs. A point is `partial` as in
  [Calendar](#calendar) (see the envelope's `rolling`).
- **`breakdown`.** Ranked by finished runs: ranks 1 to 50, then Other.
  `dimension` echoes the choice, `primary_model` by default; `harness`, `repo`
  and `kind` are the others, and `user` only on an admin all-users response.
  Each run is counted once, in exactly one row (under `primary_model`, the run's
  [primary model](#cost-fold-and-disclosure)). The rows are `breakdown.rows`,
  each with `rank`, `is_other`, `key`, `label`, `id`, `n`, `outcomes`,
  `failure_rate` and `requeued_runs`. `n` is the row's finished runs
  and `failure_rate` is null with `insufficient_runs` below five. On the owner
  route a repo outside the caller's connections is masked as in
  [Repo visibility](#repo-visibility): its row has key `inaccessible_repo` and
  null `id` and `label`, as do the other rows without one. A group whose runs
  are all still in flight appears with `n` = 0.
- **`requeued_runs`.** Runs whose retained requeue count is above zero. It is a
  count of runs, not of timestamped requeue events, and it counts cohort runs
  whether or not they finished, so it can exceed `finished`.
- **`needs_landing`.** `needs_landing.runs` counts the window's failed runs that left
  recoverable work: a failure origin a human can land, plus an available owner
  recovery capture or a preserved patch. `needs_landing.caveat` is always
  `work_may_already_have_landed`: the work may have landed by other means, so it
  is a recoverable-work figure, not a guaranteed landing backlog.
- **`latest_failure`.** The scope's most recent failure over its **lifetime**,
  not bounded by the window, restricted by the scope and the `repo`, `harness`
  and `kind` filters (delivery kinds only). It ignores `breakdown`, which never
  narrows it. It holds `failed_at` (finish time, else the last status time),
  `origin`, `run_id`, `user_id` and `completed_since_last_failure`, the
  completed runs of the same scope strictly after `failed_at`. It is `null` when
  the scope has no failure. Because it is unwindowed it can be a run created
  after the cohort's boundary at `generated_at`.
- **One snapshot.** The cohort and the latest-failure statements run in the
  same `REPEATABLE READ` transaction, so the whole response describes one
  database state. Admission is one request per caller for the `reliability`
  section, as described in [Admission and timeouts](#admission-and-timeouts).

## Options

`/options` returns the repos you can filter by, the users an admin can select
(admin only), and the allowed windows, kinds, harnesses, splits and breakdowns.
On the owner route `users` is null with reason `not_applicable`.

- Each list is capped at 200 and ordered by path (repos) or email (users);
  `truncated` is true when more exist. A valid repo that was cut off is still
  accepted as a filter.
- It reads no usage and no history start, so it is cheap.
- Admin `user=<uuid>` narrows the repo list to that user's connections and
  echoes `selected_user`. The Spend and Reliability responses carry the same `selected_user`
  (`all` or the user id; the caller's id on the owner route).

## Capacity

`GET /api/analytics/capacity` (owner) and `GET /api/admin/analytics/capacity`
(admin) answer how busy ordinary work was and what the providers' meters read
now. It takes `window`, `tz`, `repo`, `harness`, `kind` and, on the admin route,
`user`, with the same validation and errors as Spend. It takes no `split` or
`breakdown`. Authorization, the owner and admin scopes, admission (its own
`capacity` slot) and the 8 second budget are as above, and `window=all` on the
admin all-users scope is the same 400 `window all not available for all users`.
The 8 second budget covers the database snapshot only: the Codex vault
observation runs after it (see [Provider gauges](#provider-gauges)). The
response carries the same calendar envelope as Spend: `previous_from` and
`previous_to` are present for `7d`, `30d` and `90d` and absent for `window=all`,
and `rolling` is always present. Capacity itself reports no previous-period
figures and no rolling series. The response is sent with
`Cache-Control: private, no-store`.

### Occupancy

`occupancy` (`time_basis` `interval_overlap`, `unit` `average_concurrent_runs`)
is the time-weighted average number of concurrent ordinary runs. Each run
contributes one interval from `first_started_at` to its retained end
(`COALESCE(finished_at, status_since)`), or to `generated_at` while it is not
finished. It is an elapsed envelope: it includes time spent waiting and cannot
recover claim or resume history, so it is not exact execution time or worker
occupancy. There is no slot denominator, percentage or clamp, and no historical
worker count is read.

- Intervals are clipped to the range and integrated by one sort over their
  endpoints and the bucket edges. Runs that began before the window contribute
  only their overlap, and runs starting or ending at the same instant do not
  change the integral.
- Ordinary work only: chat runs and runs on a durable cross-check lane are
  excluded. A `kind=cross_check` run that is not on a lane is included.
  Retained lane assignment can be reset or absent, so the lane test is
  historical evidence, not a reconstruction.
- `window` and every bucket carry `run_seconds`, `duration_seconds`,
  `average_concurrent_runs` and `reason`. Each bucket is divided by its own
  actual duration (a 23 or 25 hour DST day, or the partial bucket for today),
  not by a fixed day. A zero-duration bucket (for day windows, the final
  `[now, now)` bucket when `generated_at` is exactly a local midnight; for
  `window=all`, see [Calendar](#calendar)) has `average_concurrent_runs` null with reason
  `zero_duration_bucket`; a zero-duration range has null with
  `zero_duration_range`.
- `population` counts the candidate runs: `candidates` is `with_interval` +
  `without_start` + `invalid_interval`. A terminal run is a candidate when its
  end is after the range start and it was created before `generated_at`; a
  nonterminal run when it was created before `generated_at`.

  | Field | Meaning |
  |---|---|
  | `with_interval` | Runs with a valid interval, which feed the integral. |
  | `without_start` | Runs with no `first_started_at`, excluded. |
  | `invalid_interval` | Runs whose end is before their start, or whose first start is at or after the request's captured clock (`generated_at`); excluded. |
  | `end_from_status_since` | Terminal runs with no `finished_at`, whose end falls back to `status_since`. |
  | `ongoing` | Every nonterminal candidate, including queued runs with no start (those are also `without_start` and add nothing to occupancy). A started one ends at `generated_at`. |
  | `cross_check_kind_included` | `cross_check` runs not on a lane that are included. |

- `limitations` is a fixed, ordered list of `{code, runs}`. Method codes have
  `runs` null: `elapsed_envelope_includes_waits`,
  `claim_and_resume_history_not_recovered`, `lane_assignment_history_limited`.
  Counted codes give the number of candidate runs they apply to:
  `missing_start_excluded`, `invalid_interval_excluded`,
  `end_from_status_since`, `ongoing_end_is_generated_at`,
  `cross_check_kind_not_on_lane_included`.

### Usage wait

`usage_wait` (`time_basis` `created_at`, `cohort` `creation`, `basis`
`limit_wait_count`) is the share of applicable runs created in the range whose
retained `limit_wait_count` is above zero. `current` is the whole range and
`buckets` are by creation time, each `{count, n, share, reason}`. It counts a
counter, not episodes: a run that waited twice counts once, whether or not it is
still parked, and no wait is placed in time. An empty cohort gives `share` null
with `no_applicable_runs`. Applicable runs are the same ordinary-work population
as occupancy, with no end-time condition.

### Not tracked, filters

- `not_tracked` lists measures this endpoint does not report, each with a null
  `value` and reason `not_tracked`: `historical_provider_headroom`,
  `slot_count`, `utilization`, `parked_duration`.
- `filters` echoes `repo` (null when absent), `harness` (null when absent) and
  `kinds`, the run kinds measured. `kinds` never lists `chat`. With `kind=chat`
  the measures are empty and `excluded_kind_selected` is `chat` (else null).
- `selected_user` is `all` or the user id, as in Spend.

### Provider gauges

`gauges.anthropic` and `gauges.codex` are the latest stored reading per
enabled identity, whatever the window (`time_basis` `current_observation`).
They are not part of the history. No provider is called, no credential is
refreshed, percentages are never summed, and the response has no secret values,
raw provider identifiers or poll-error text.

- Each gauge has `status` (`available`, `unavailable`, `not_selected`),
  `reason`, `effective_scope` and `identities`. `harness` selects the provider:
  `claude` shows only Anthropic and `codex` only Codex; the other is
  `not_selected` with reason `not_selected` and no identities.
- A shared quota cannot be split by repo or kind. When `repo` or `kind` is
  given, `effective_scope.unattributed_selectors` lists them (`repo` then
  `kind`) with reason `shared_quota_not_attributable`; the identities are
  unchanged. `effective_scope` also names the `user` (always equal to the response's
  `selected_user`) and `provider`.
- At most 200 identities per provider, in stable order. `truncated` is true when
  more exist, and `set_state` is `empty` (a proven empty set), `complete` or
  `truncated`; it is null unless the gauge is `available`. A Codex login with
  several enabled aliases is one identity (one reading). On an admin all-users
  response identities carry `user_id` and `email`; otherwise those are null.
- A missing or obsolete reading keeps the existing rate-limit statuses (for
  example `unavailable` for an Anthropic token with no current reading, `pending`
  or `stale` for Codex). Statuses are judged at the request's captured clock.
- A gauge read that fails with an ordinary database error marks that gauge
  `unavailable` with reason `gauge_unavailable`; occupancy, usage wait and the
  other gauge are kept. A gauge statement timeout or the snapshot deadline fails
  the whole request with 503 and `Retry-After: 1`, like any other analytics
  timeout.
- Codex vault state (`vault_state_basis` `live_observation`) is read live after
  the snapshot, outside its 8 second budget. The owner route reads the caller's
  own state with the same unbounded `RequiresUnlock` read as
  `GET /api/me/codex-rate-limits` and fails closed: a locked or unreadable vault
  reports `vault_locked` per identity (`polling_disabled` instead when Codex
  polling is turned off). The admin routes read each distinct owner without
  waiting for any user's vault lifecycle, under a 2 second budget that is
  checked between and after the lookups (reads run under the budget's context;
  a step that does not honour cancellation can still finish after it). If a state is contended, fails to read or
  the budget runs out, the Codex gauge is `unavailable` with reason `vault_status_unavailable`
  and occupancy is kept. This reason appears on the admin routes only.

## Flow

`GET /api/analytics/flow` (owner) and `GET /api/admin/analytics/flow` (admin
read group) answer where work is waiting now, how much finished over time and
where delay shows up in a creation cohort. Routes, authorization, parameters,
errors, admission, the snapshot and the [Calendar](#calendar) are the same as
for Spend, with the differences below. Flow has no `split` or `breakdown`, no
previous-period figures and no cost figures (the shared envelope still carries
`previous_from` and `previous_to` for bounded windows). The response is the shared envelope, then:

- `selected_user`: `all` for an admin all-users response, else the user id.
- `filters`: the echoed `repo` and `harness` (null when not given) and the
  effective `kinds` list.
- one object per block below.

Every block names its `time_basis` (`now`, `created_at` or `finished_at`) and a
`population` label. **Blocks are independent populations: do not add them to
each other or compare their counts as if they covered the same runs.** Bucket
series (`index` is the zero-based position in the response's `buckets`) always
contain every bucket, empty ones included. Durations are seconds. Lists are
never null.

### Blocks

| Block | `time_basis` | Population |
|---|---|---|
| `waiting_now` | `now` | Runs in the scope that are currently in a waiting status. The window is ignored; the repo, harness and kind filters still apply. |
| `activity.created` | `created_at` | Runs created in `[from, generated_at)`. Chat is included when the kind selector includes it. |
| `activity.finished` | `finished_at` | Runs that are terminal now and whose end time falls in `[from, generated_at)`, whatever their creation date. Chat is excluded. |
| `first_start_delay` | `created_at` | The creation cohort (all selected kinds). |
| `plan_response` | `created_at` | Gate presentation and approval pairs of runs in the creation cohort. |
| `delivery_funnel` | `created_at` | `issue` and `self_improve` runs in the creation cohort. |
| `approval` | `created_at` | The plan cohort inside the funnel cohort: runs with a gate revision above 0, `auto_approve` set, or any `approve_plan` receipt. |
| `issue_elapsed` | `created_at` | `issue` runs in the creation cohort. |

- **`waiting_now`** lists all eight states in a fixed order, zero rows
  included: `queued`, `awaiting_approval`, `awaiting_input`,
  `awaiting_followup`, `paused`, `limit_wait`, `pool_wait`, `recovery_wait`.
  Each has `count`, `oldest_status_since` and `oldest_age_seconds` (null when
  the count is 0; the age is measured to `generated_at` and never negative),
  and `total` sums them. An old run still awaiting approval counts here, not in
  the window.
- **`activity.created`** has `total` and a count per bucket. **`activity.finished`**
  has `total` and a per-bucket count by outcome class (`completed`, `cancelled`,
  `plan_rejected`, `base_diverged`, `failed`, and their `total`), the same
  classes as the usage outcomes. The end time is `finished_at`, else the last
  status time, so a run created before the window and finished inside it counts
  in the bucket of its end. `excluded_kinds` is `["chat"]`: chat has no outcome
  class and is never counted as finished.
- **`first_start_delay`** is **approximate** (`approximate: true`,
  `limitations: ["first_started_at_legacy_backfill"]`). It is
  `first_started_at - created_at` for runs where that is not negative, so holds
  before first execution are included. Migration 00280 backfilled
  `first_started_at` from a `started_at` that may belong to a later leg, so
  older runs can read longer than the true first start. `observed` counts valid
  pairs, `not_started` counts runs with no `first_started_at`, and `invalid`
  counts negative differences. `p50_seconds` and `p90_seconds` are null with
  `no_observations` when `observed` is 0.
- **`plan_response`** is **approximate** (`approximate: true`). It is the time
  from a gate presentation's `created_at` to the `created_at` of the single
  applied human approval for the same run and revision. Read it as the
  response to the observed gate evidence, not as a measure of when the human
  saw the plan:
  - A presentation row is stamped when the gate was recorded. A gate that had
    no id when it was published and was adopted later is stamped at adoption,
    so its measured response is shorter than the human's real wait.
  - `applied_at` is used only to prove that the approval was applied, never as
    the end of the interval, because the receipts of legacy workers may stamp
    `applied_at` when the input is drained rather than when it was approved.
  - There is no fallback to `run_messages`. A message carries no revision, so
    it cannot prove which gate it answered; without a retained presentation row
    the pair is unknown (`no_presentation`).

  `n` counts timed pairs. `not_approved` counts presentations with no applied
  approval; it is neither timed nor unknown. `unknown` has a total and these
  reasons: `ambiguous`, `no_presentation`, `invalid_chronology` (a receipt
  applied before it was created, or a presentation created after the input),
  `excluded_disposition` (only superseded, declined or advisory receipts),
  `reconciliation_only` (the single eligible receipt has disposition
  `applied`) and `legacy_unbound` (a run with any no-binding or `unbound`
  `approve_plan` receipt, counted once per run; see
  [Approval evidence](#approval-evidence)). Percentiles are null with
  `no_observations` when `n` is 0.

  Plan response covers every kind the selector includes, chat too: when `chat`
  is selected its runs are probed for gate evidence like any other run. The
  delivery funnel and the approval classification stay restricted to `issue`
  and `self_improve`.
- **`delivery_funnel`** has `applicable`; when the kind selector excludes both
  `issue` and `self_improve` it is false with reason `not_applicable` and every
  count is zero. Otherwise:
  - `stages` count runs: `created`, `plan_settled` (settled by a human or by
    auto-approval), `completed`, `pr_present` (a PR and a repo) and `merged`
    (the PR has a merged witness).
  - `planning` branches are exclusive and sum to `stages.created`. A run takes
    the first branch that applies, in this order: `rejected` (outcome
    `plan_rejected`), `settled_human` (exactly one eligible receipt for its
    current gate revision), `settled_auto` (`auto_approve` set, gate revision
    0, a plan that was not seeded), `skipped` (a seeded plan at gate revision
    0), `in_progress` (not terminal), `failed`, `cancelled`, `unknown`.
    `settled_auto` needs revision 0 because the human gate clears
    `auto_approve`. `unknown` means the evidence cannot show how planning
    ended, for example a completed run with no proof of approval and no seeded
    plan; it is never reported as skipped.
  - `outcomes` are exclusive and sum to `stages.created`: `completed`,
    `failed`, `cancelled`, `plan_rejected`, `base_diverged` and `in_progress`
    (no outcome yet).
  - `disclosure` counts runs with incomplete approval evidence.
    `historical_approval_only` counts runs with no eligible receipt for the
    current revision and at least one older revision with exactly one eligible
    receipt; it is a disclosure counter, independent of the `approval` ranking.
    `ambiguous_current` counts runs with more than one eligible receipt for the
    current revision. A run with no eligible current receipt whose only older
    evidence is ambiguous is counted in neither. `legacy_evidence` counts an `issue` or `self_improve` run with
    any no-binding or `unbound` `approve_plan` receipt, even when the run also
    has timed pairs or current human proof. These counts are not additive with
    each other, with `approval.unknown` or with plan response `unknown`: a run
    can appear in several.
  - `pr_units` counts **distinct PRs**, not runs. A PR is the pair (repo, PR
    number), as in the [merged-PR rules](#ratios); each one is attributed to
    the earliest run of the creation cohort that references it (by
    `created_at`, then id) and bucketed by that run's creation bucket, so a PR
    with several runs is counted once. It has `present`, `merged` and the same
    two counts per bucket. The merge witness is scoped to the response and
    ignores the window, harness, kind and repo filters, exactly as for
    `cost_per_merged_pr`: a merged run of the same user (owner and admin
    `user=<uuid>`) or of any user (admin all-users) proves the merge, and a
    run of another user never does on its own.
- **`approval`** classifies how the plan cohort's plans were approved, and
  `total = human + auto_approve_configured + unknown.total`. `human` is proven
  by an eligible applied receipt (below). `auto_approve_configured` has no
  human proof but the run has `auto_approve` set; it is configuration, not an
  observed event, and carries no timestamp. Human proof wins over
  configuration. `unknown` is split by the first reason that applies, in this
  order: `ambiguous` (more than one eligible current-revision receipt, or
  older-revision ambiguity with no uniquely approved older revision),
  `legacy_unbound`, `historical_approval_only` (at least one older revision
  with exactly one eligible receipt), `no_evidence`. See
  [Approval evidence](#approval-evidence) for the ranking.
  `applicable` follows the funnel.
- **`issue_elapsed`** is `COALESCE(finished_at, status_since) - first_started_at`
  for terminal `issue` runs in the creation cohort, so it includes held time.
  `applicable` is false (reason `not_applicable`, all counts zero) when the
  kind selector excludes `issue`.
  - `n` counts valid pairs. `unfinished` counts runs that are not terminal
    yet, and `incomplete` is true when any exist, even for an old bucket.
    `invalid` counts terminal runs with no valid pair (no `first_started_at`
    or a negative difference); it is kept apart from `unfinished`.
  - `histogram` has nine bins over valid pairs. Bin `i` is
    `[lower_seconds, upper_seconds)` and the last has a null upper. The lower
    edges are 0, 900, 1800, 3600, 7200, 14400, 28800, 86400 and 259200 seconds
    (15 minutes, 30 minutes, 1, 2, 4 and 8 hours, 1 day and 3 days).
  - `buckets` give `n`, `unfinished`, `invalid`, `incomplete` and percentiles
    per creation bucket, each computed from that bucket's own runs (also after
    all-time coarsening). Percentiles are suppressed below 5 valid
    observations: both are null with `insufficient_observations` while `n` is
    kept, and null with `no_observations` when `n` is 0. `partial` is the
    calendar's marker for the bucket that ends at `generated_at`; it is
    independent of `incomplete`. The overall percentiles are null with
    `no_observations` only when `n` is 0 and are not suppressed.

### Approval evidence

Plan response, the funnel and the approval classification share these rules:

- **Eligible receipt.** An `approve_plan` input is eligible only when it is
  bound to a gate revision above 0, was applied (`applied_at` is set), its
  disposition is empty or `applied`, and it was created no later than it was
  applied. A superseded, declined or advisory receipt is not eligible, and an
  input that was only acknowledged is not applied.
- **Ambiguity.** More than one eligible receipt for the same run and revision
  is ambiguous evidence for that revision: it proves nothing about it.
  Receipts that are not eligible never make a revision ambiguous. Reporting
  differs by surface: `plan_response` reports the pair as `ambiguous`.
  `approval` follows the [ranking](#approval-evidence) below: current-revision
  ambiguity, or older-revision ambiguity when no older revision has a unique
  receipt, meets the condition for the `ambiguous` reason, but the run takes
  the first reason in the ranking that applies, so current human proof and
  `auto_approve_configured` still rank ahead. The funnel's
  [`ambiguous_current`](#flow) disclosure counts only current-revision
  ambiguity.
- **Current revision.** Human proof for a run is exactly one eligible receipt
  for the run's current gate revision. This is evidence only: an older
  revision with exactly one eligible receipt is historical approval evidence,
  an older revision with more than one proves nothing, and the run's label is
  decided by the ranking below. Revisions are never matched across runs.
- **Legacy and unbound.** A receipt written before gate binding (no binding) or
  recorded as `unbound` never proves an approval. A run with such an
  `approve_plan` receipt is counted once in plan response
  `unknown.legacy_unbound`, which covers every selected kind (the gate-evidence
  cohort, chat included when selected). It is also counted once in the funnel
  `disclosure.legacy_evidence`, which covers only `issue` and `self_improve`
  runs (the funnel cohort). Both apply even when the run also has timed pairs
  or current human proof. An unbound receipt never approves a pair. A pair with
  no bound, applied receipt at all is `not_approved`; one whose bound, applied
  receipts all fail eligibility is `invalid_chronology` or
  `excluded_disposition`.
- **Approval classification ranks separately.** `approval` places a run by the
  first reason that applies: human (exactly one eligible current-revision
  receipt), `auto_approve_configured`, `ambiguous` (more than one eligible
  current receipt, or older-revision ambiguity with no uniquely approved older
  revision), `legacy_unbound`, `historical_approval_only` (at least one older
  revision with exactly one eligible receipt), then `no_evidence`. Current
  human proof and auto-approve configuration rank ahead of any older
  ambiguity, so a run with current human proof is `human` there while still
  counted in the disclosure and plan response legacy counts.
- **Disclosure counts are not additive.** `unknown.legacy_unbound`,
  `disclosure.legacy_evidence` and `approval.unknown.legacy_unbound` have
  different populations and precedence, and must not be summed or compared as
  one total.

### Null reasons

Flow's percentile and applicability reasons are a closed set:
`no_observations` (the population has no valid observation),
`insufficient_observations` (a creation bucket has fewer than 5, so its
percentiles are suppressed and its `n` is kept) and `not_applicable` (the kind
selector excludes the population). A null percentile always carries a reason.

### Masking

The Flow document names no repo id or path, so there is nothing to mask on
either route. The only repo that appears is the validated `repo` you passed,
echoed in `filters.repo`. For the scoped PR counts, see `pr_units` above.

### Flow performance

Measured on a synthetic 120,000-run seed (200 owners with skew, 1,250 repos,
373,446 run inputs, 71,336 gate presentations, 2.46 million messages),
PostgreSQL 17, JIT off. These are statement-level medians. End-to-end
production latency was not measured; HTTP, auth middleware, DTO JSON encoding
and network are excluded.

| Request | Custom plan | Generic plan |
|---|---|---|
| Owner `30d` (largest owner) | about 85 ms | about 82 ms |
| Owner `window=all` (largest owner) | about 0.67 s | about 0.67 s |
| Admin all-users `30d` | about 0.40 s | about 0.39 s |
| Admin all-users `90d` | about 0.56 s | about 0.58 s |
| Admin `user=<uuid>` `window=all` | about 0.64 s | about 0.65 s |

The DB-side request path (`store.RunAnalyticsSnapshot` with an 8 s statement
timeout around the history-start query, bounds computed in UTC rather than the
caller's zone, the Flow statement and decoding; 20 requests per workload on a
warm pooled connection with the default pgx statement cache) measured the same
on that seed, with no timeouts. The baseline phase had already run through the
same pool, so the Flow statement was already prepared and its plan choice
settled. The history-start statements run once per built workload (all eleven
are built before the workload filter): nine owner and two all-users executions
over the pool. The all-users plan could still switch inside the sample, and the
owner plan's state on the request connection was not checked. Any generic plan
in use was whatever Postgres auto mode chose. It excludes HTTP,
auth middleware, admission, in-snapshot authorization (tz-name check, user
lookup for an admin `user=<uuid>`, repo checks), the DTO mapping, JSON encoding
and the network. Medians in ms: owner `30d`
81.5, owner `window=all` 641.4, admin all-users `30d` 410.5, admin all-users
`90d` 589.1, admin `user=<uuid>` `window=all` 631.2; the slowest request of any
workload was 656.2 ms.

A median owner takes 1-15 ms. The statement reads approval evidence per run
through an index and never reads the run message log. No index was added for
Flow. The measurement write-up is in
[prds/done/4729-scoped-run-flow-api.md](../prds/done/4729-scoped-run-flow-api.md).

## Spend performance limit

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
list `all` for that scope, and Spend and Reliability answer 400 `window all not
available for all users` before any analytics database work (authentication runs first). The measured 10 s is over the 8 second
budget, so the request would otherwise have been a 503. All-time stays
available for the owner route and for admin `user=<uuid>`; owner `all` Spend
measured about 1.8 s (statement-level median, custom plan) on the same seed, and
end-to-end request latency was not measured. Admin all-users all-time is deferred to a follow-up that adds per-run
totals for finished runs (owner decision at landing).

**Reliability.** Measured on the same seed generator (120,000 runs, 846,668
usage legs, 200 owners, 1,250 repos; the #4685 re-seed, not byte-identical to
the Spend seed), PostgreSQL 17, JIT off as in production, warm cache, one client
and no concurrent load. The worst request, admin `user=<uuid>` all-time by
primary model, took about 1.3 s median, and no workload timed out against the
8 second budget. The per-workload tables and plans are in
[PRD #4685](../prds/done/4685-scoped-outcomes-reliability-api.md) section 8.
