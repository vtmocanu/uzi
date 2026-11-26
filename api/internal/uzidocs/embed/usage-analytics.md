---
title: Usage analytics API
order: 47
audience: user
---

# Usage analytics API

A read-only API for run diagnostics over time: filter options, a Spend
document with daily (or weekly) buckets, ranked splits, two cost ratios, a
cache-read ratio, the ten most expensive runs and a per-group breakdown, and a
[Flow](#flow) document with live waits, activity, timing and delivery
evidence. Everything is computed on the server from one database snapshot. It
is API only: there is no web page or `uzi` command for it yet, and
`GET /api/usage` and `GET /api/admin/usage` are unchanged.

Every dollar figure is recorded spend (an estimate for comparison), not a
bill.

## Routes and authorization

| Scope | Routes | Who |
|---|---|---|
| Owner | `GET /api/analytics/options`, `GET /api/analytics/spend`, `GET /api/analytics/flow` | Any signed-in user (session or `uzc_` Bearer). Always the caller's own runs. |
| Admin | `GET /api/admin/analytics/options`, `GET /api/admin/analytics/spend`, `GET /api/admin/analytics/flow` | An admin session, or a Bearer token with the `admin_ro` scope. |

- The owner route never reads a user from the query. An admin calling it sees
  only their own runs, and a `user` parameter there is a 400.
- An ordinary Bearer token never reaches the admin routes, even when its owner
  is an admin (403).
- The paths are literal. Any other `/api/analytics/...` path, such as a section
  that does not exist yet, is a 404.

## Parameters and errors

| Parameter | Values | Default | Notes |
|---|---|---|---|
| `window` | `7d`, `30d`, `90d`, `all` | `30d` | Calendar dates, see [Calendar](#calendar). The admin all-users scope (`user` omitted or `user=all`) has no `all`: Options lists only `7d`, `30d`, `90d` there, and Spend and Flow reject `window=all` with a 400. Owner and admin `user=<uuid>` keep `all`. Options accepts and ignores `window`. |
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
- Spend and Flow on the admin all-users scope with `window=all` are a 400,
  `window all not available for all users`. It is answered before any
  analytics database work (authentication runs first) and is not a 503.
- Repeated and empty parameters are invalid (`invalid <name>`). An unknown
  parameter is a 400 `unknown parameter`. A query string that does not parse
  (a bad percent-encoding or a `;`) is a 400 `invalid query`; it is never
  silently replaced by defaults.
- `split` or `breakdown` on `/options` or `/flow`, and `split=user` or
  `breakdown=user` anywhere but an admin all-users Spend response, is
  `invalid split` or `invalid breakdown`. Flow takes only `window`, `tz`,
  `repo`, `harness`, `kind` and (admin) `user`; any other parameter is
  `unknown parameter`.
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
  `spend` or `flow`) may be in flight, and the owner and admin variants share that
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

## Flow

`GET /api/analytics/flow` (owner) and `GET /api/admin/analytics/flow` (admin
read group) answer where work is waiting now, how much finished over time and
where delay shows up in a creation cohort. Routes, authorization, parameters,
errors, admission, the snapshot and the [Calendar](#calendar) are the same as
for Spend, with the differences below. Flow has no `split` or `breakdown`, no
previous period and no cost figures. The response is the shared envelope, then:

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
  `applied`) and `legacy_unbound`. Percentiles are null with `no_observations`
  when `n` is 0.
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
  - `disclosure` counts runs with incomplete approval evidence:
    `historical_approval_only`, `ambiguous_current` and `legacy_evidence`.
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
  configuration. `unknown` is split by the first reason that applies:
  `ambiguous`, `legacy_unbound`, `historical_approval_only`, `no_evidence`.
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
  is ambiguous: it proves nothing and is reported as such. Receipts that are
  not eligible never make a pair ambiguous.
- **Current revision.** Human proof for a run is exactly one eligible receipt
  for the run's current gate revision. An eligible receipt only for an earlier
  revision is `historical_approval_only`. Revisions are never matched across
  runs.
- **Legacy and unbound.** A receipt written before gate binding (no binding) or
  recorded as `unbound` never proves an approval. A run with such a receipt and
  no unique current proof is `legacy_unbound`. In plan response a run with a
  no-binding receipt adds one unknown even when it also has timed pairs; a run
  with only `unbound` receipts adds one unknown only when it has no pair at
  all, and with pairs the unbound approval can never have acted on a gate, so
  the pair stays `not_approved`.

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
PostgreSQL 17, JIT off. These are statement-level medians; end-to-end request
latency was not measured.

| Request | Custom plan | Generic plan |
|---|---|---|
| Owner `30d` (largest owner) | about 85 ms | about 82 ms |
| Owner `window=all` (largest owner) | about 0.67 s | about 0.67 s |
| Admin all-users `30d` | about 0.40 s | about 0.39 s |
| Admin all-users `90d` | about 0.56 s | about 0.58 s |
| Admin `user=<uuid>` `window=all` | about 0.64 s | about 0.65 s |

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
list `all` for that scope and Spend answers 400 `window all not available for
all users` before any analytics database work (authentication runs first). The measured 10 s is over the 8 second
budget, so the request would otherwise have been a 503. All-time stays
available for the owner route and for admin `user=<uuid>`; owner `all` Spend
measured about 1.8 s (statement-level median, custom plan) on the same seed, and
end-to-end request latency was not measured. Admin all-users all-time is deferred to a follow-up that adds per-run
totals for finished runs (owner decision at landing).
