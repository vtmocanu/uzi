# ADR-2014: record the estimated usage tail of an interrupted Claude session, apart from the metered total

**Status**: Accepted (issue #2014)
**Date**: 2026-10-03
**Deciders**: architect (design), lead (plan approval), coder (implementation), reviewer.
**Amends**: [ADR-1562](1562-run-usage-session-cumulative-basis.md) — its metered fold (`run_usage_totals`, the `per_leg` / `session_cumulative` basis rule) is unchanged and stays the only authoritative total. This ADR adds a second, separate figure beside it: the usage of model calls that no SDK `result` frame ever covered.
**PRD**: none — the spec lives in the issue body; this ADR is the design record.

## Decision (summary)

The metered fold reads only Claude `result` frames (`foldUsageFrames` in
`api/internal/workersvc/usage_fold.go`). A leg interrupted before its `result` frame
(a disk park, a worker restart, a crash, a limit-wait cut) leaves every model call it
made since its last `result` unmetered: the spend happened, the ledger never sees it.

uzi now records that tail on a side channel and shows it **apart from** the metered
total, labelled **estimated**, priced from a recorded price table, with an explicit
**coverage** indicator:

- The worker captures per-message usage, keyed by Anthropic `message.id`, **before**
  projection, and posts it to a new route `POST /api/worker/runs/{id}/usage`.
- The server stores it in three new tables and decides, per message, whether some
  `result` frame already covered it. Uncovered messages are the **tail**.
- The tail is priced at read time from `api/internal/anthropicprice`. It counts toward
  no budget and never enters `run_usage_totals`.
- Codex and chat runs are excluded.

## D1 — Capture before projection, on a side channel

`ClaudeHarness` (run lane only) owns a `UsageRecorder` (`agent/src/usage-recorder.ts`)
per **leg** (one `query()` call, one `init` frame). Records are keyed by Anthropic
`message.id` and created or updated in `decode`, **before** projection or signal
filtering, from three sources:

1. every `assistant` frame (`message.id`, `message.usage`, model);
2. `stream_event` `message_start` (id, model, input and cache usage), which creates a
   record for a message that never arrives as an assistant frame;
3. `stream_event` `message_delta` (the final usage; sets `output_final = true`).

Stream events are attributed per lane (`parent_tool_use_id ?? "main"`) to that lane's
current `message_start` id. Token columns merge with GREATEST, because consecutive
assistant frames share one `message.id` and the assistant-frame usage is a
`message_start` snapshot, not the final figure.

Each record carries `message_id`, `leg_id` (a UUID per `query()`), `ordinal` (dense
1..n per distinct `message_id` within the leg, in SDK stream order), `model`,
`subagent`, `input_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`
(plus the `ephemeral_5m` / `ephemeral_1h` split when present), `output_tokens`,
`output_final`, `service_tier` (when present), and the frame `session_id` (diagnostic
only).

Text-only, thinking-only, signal-only and subagent messages all count. Displayed usage
and the web live surface are unchanged.

**Why a side channel, not `run_messages`.** `run_messages` has many unfiltered readers
(the client fold in `web/src/lib/runUsage.ts` sums `payload.usage` on any non-result
frame), and the worker owns its gapless `seq`. Adding usage-only frames there would
change what every reader sees. The new route touches none of them.

**Subagent coverage.** The implementation verifies whether subagent `stream_event`s are
forwarded with `includePartialMessages`. If not, the fallback is `forwardSubagentText:
true` with the reducer dropping subagent text and thinking items from projection.
Either way a test pins that a text-only subagent message is counted.

## D2 — Partial messages in the run lane, with no change to liveness or projection

`ClaudeHarness` sets `includePartialMessages: true`. Every other lane stays `false`:
chat (`agent/src/chat-executor.ts`), advice and judge
(`agent/src/claude-advice-harness.ts`), jobs (`agent/src/job-runner.ts`) and isolated
runs (`agent/src/isolated-executor.ts`).

`decode` maps a `stream_event` either to a new neutral `usage` harness event or drops
it (pings, content deltas). It is **never** an `activity` or `frame` event, so:

- it does not re-arm the idle timer: a ping-only stall still trips `REASON_IDLE`;
- it is excluded from `orphanInstanceKind` and from both session-id latches;
- it is never projected and never persisted.

**Projection equivalence.** A test replays one SDK transcript through
`ClaudeHarness.decode`, the reducer and the executor emit, once with partials off and
once with partials on, and asserts the projected and persisted message sequence is
identical (kinds, deep-equal payloads, seq count). The input is a redacted real
recording if cheap to obtain, otherwise one built from the richest existing raw SDK
frame sequences in agent tests, including a subagent and a signal-only frame, with the
documented stream events (`message_start`, `content_block_start` / `_delta` / `_stop`,
`message_delta`, `message_stop`, `ping`) interleaved. The PR states which input it used.

## D3 — Durable, incremental and bounded capture on the worker

| constant | value | meaning |
|---|---|---|
| `USAGE_POST_MAX_RECORDS` | 500 | records per post |
| `USAGE_PENDING_MAX` | 2000 | distinct messages pending in memory |
| `USAGE_DROPPED_IDS_MAX` | 2000 | dropped message ids remembered per leg; past it drops are still counted |
| `USAGE_DRAIN_DEADLINE_MS` | 3000 | total drain wait at a flush or close |

- **Prompt posting.** About 250 ms debounce, at most 500 records per post, bounded
  backoff, a per-request timeout, the claim generation stamped on every post.
- **Memory bound.** Past 2000 pending messages, new records are dropped and counted in
  `dropped_records`, which is sent on the next post and on the close marker. The server
  persists it and coverage reports `records_dropped`.
- **Leg end.** The `ClaudeHarness` events iterator's `finally` enqueues a close marker
  (`closed_through = n`, `dropped_records`).
- **Drain owner.** The recorder registers with the run's `MessageBatcher`;
  `batcher.flush()` and `close()` drain it first, so usage lands before the
  claim-releasing state report (`handlePausePark` in `agent/src/runner.ts` already
  flushes before that report). The limit-wait, credential-switch and wall-park paths
  are audited for report-before-flush order.
- **Drain bound.** The total wait is at most 3000 ms, or less when the batcher's own
  boundary deadline is shorter. Past it the recorder abandons unsent records **and
  aborts the pending HTTP request** (an `AbortController`), not merely stops waiting on
  it; the batcher proceeds. Abandoned records surface as `leg_not_closed` or
  `ordinal_gap`. A retryable post failure also ends the drain early; its records stay pending
  for the backoff-paced retry and, if still unsent when the batcher closes, are abandoned the same way
  (review of !2214, `runner.ts` park path).
- **Typed errors.** Three answers stop only that run's recorder: the typed `stale` 404
  (run not owned), the fence 409 `{disposition: "stale_claim"}`, and the
  `ErrMissingClaimGeneration` 409. An untyped 404 or 405 streak disables the route for the whole process, the same pattern
  as `INCLUSION_ROUTE_MISSING_LIMIT` in `agent/src/inclusion-reporter.ts`, so a new
  worker against an old api does not retry forever.
- **No disk spill.** A hard kill loses unsent records; coverage says so.

## D4 — Reconciliation: which messages a `result` frame covered

**Leg identity and order.** The worker stamps `leg_id` and the SDK `session_id` (as
`sdk_session_id`) into the leg's persisted `init` frame (`projectInit`).
The incremental fold (`foldRunUsage`, the wrapper `appendMessages` calls; never the
shared body `RefoldRunUsage` replays) upserts `run_usage_legs` with `init_seq = COALESCE(existing, frame
seq)` and `sdk_session_id = COALESCE(existing, stamped)`. Leg order is `init_seq`.

**Result stamp.** Both `projectResult` emit sites in `agent/src/harness-messages.ts`
(the `status` and the `error` result) stamp `leg_id` and `usage_through`, the leg's
highest ordinal at that point. The incremental wrapper `foldRunUsage` (not the shared
`foldUsageFrames` body), **after** the unchanged metered fold and only for a frame the metered
fold accepts (it decodes as `resultUsagePayload`, is a `result` event, and has at least one
non-empty model key), sets:

```sql
covered_through    = GREATEST(covered_through, usage_through)
covered_cumulative = covered_cumulative OR (basis = 'session_cumulative')
```

**Untrusted stamps are skipped, never fatal.** A stamp is used only when `leg_id`
parses as a UUID, `usage_through` is within `0..MaxInt32`, and `sdk_session_id` is
non-empty and rune-capped. Otherwise it is ignored and the append and metering still
land.

**The coverage predicate.** Message `m` in leg `L` is covered iff some leg `C` of the
same run has `C.covered_through IS NOT NULL` and either:

```sql
-- arm (a): the same leg's own result covered it
(C.leg_id = L.leg_id AND m.ordinal <= C.covered_through)
OR
-- arm (b): a LATER leg of the SAME SDK session reported a session-cumulative total
(C.covered_cumulative AND C.sdk_session_id = L.sdk_session_id AND C.init_seq > L.init_seq)
```

**A fresh-session restart is covered by arm (a) only.** When the worker restarts the
lead in a **fresh** SDK session (run 2dc4d842: disk parks restarted the lead in a
fresh session), the new leg has a different `sdk_session_id`, so arm (b)
never applies across that boundary. The new leg's cumulative total does not include
the interrupted leg's spend, so the interrupted leg's uncovered messages stay in the
tail and are counted. Only a resume of the **same** session lets a later cumulative
result absorb an earlier leg.

**Conservative supersession.** When arm (b) supersedes leg `L` and `L` either has no
close marker or holds any message with `output_final = false`, coverage is `partial`
with reason `superseded_uncertain`, even when the tail from `L` is empty. The rule
exists because the claim "a resumed cumulative total includes the interrupted turn" is
a premise, not something uzi can verify. A real interrupt-then-resume fixture is
optional; the rule is mandatory.

**What enters the tail.** Uncovered messages whose leg has a non-null `init_seq` and
`sdk_session_id`. Messages in a leg missing either, and messages with conflicting
re-posts, are `unresolved` and excluded: the tail may under-count, never over-count.

Delivery order does not matter: a result folded before its leg's records arrive, or
after, gives the same answer. The metered fold is untouched.

## D5 — Coverage indicator

`coverage` is `"complete"` or `"partial"`; `coverage_reasons` is a closed set:

| reason | when |
|---|---|
| `leg_not_closed` | a leg not fully superseded has no close marker |
| `ordinal_gap` | for a non-superseded leg, `COUNT(DISTINCT ordinal) < max(closed_through, covered_through, max ordinal)` |
| `output_not_final` | a tail message never received its `message_delta` |
| `superseded_uncertain` | see D4, conservative supersession |
| `records_dropped` | the worker reported `dropped_records > 0` |
| `record_cap_reached` | the api dropped records or legs past a per-run cap (D11) |
| `unresolved` | see D4, what enters the tail |

`complete` iff the set is empty. No other reason string is emitted.

## D6 — Cost provenance

A new Go package, `api/internal/anthropicprice`, holds the price table with
`AnthropicPriceTableVersion`, `AnthropicPriceSourceURL` and `AnthropicPriceFetchedAt`.

- **Where the rates come from.** Every rate (input, output, cache read, 5-minute cache
  write, 1-hour cache write, long-context premium) is transcribed from Anthropic's
  official pricing page, fetched during implementation. The source URL and fetch date
  are recorded beside `AnthropicPriceTableVersion`. Rates are never written from
  memory. If the page cannot be fetched, the table ships with **no model rows** and
  every tail is `unpriced`; the PR says so. This ADR records no price figures.
- **When pricing happens.** At read time, per message, in exact microdollars. No dollar
  figure is stored, so a corrected table re-prices history.
- **Unpriced when** the model is unknown; `service_tier` is present and not `standard`;
  cache-creation tokens arrive without the 5m / 1h split; or the message is above a
  long-context threshold with no recorded premium.
- **Status.** `estimated` (with `cost_usd` and `price_table_version`) only when every
  tail message is priced. Otherwise `unpriced`, with `cost_usd` null, **never 0**.
  Per-model rows carry their own cost or null. The `metered` status of the existing
  total is unchanged.

## D7 — Budget semantics

No usage-based budget exists: run budgets are iterations and wall-clock seconds, and
no cost bound exists: the SDK's `maxBudgetUsd` option is unset in `agent/src`, and
`maxCostUSD` in `api/internal/workersvc/service.go` is only the `numeric(12,6)` storage
clamp. The tail therefore counts toward
nothing. It is **not** added to `run_usage_totals`, and every metered reader stays
byte-identical: `GetRunUsageTotal`, `ListRunUsageTotalsForRuns`, `SelfUsage`,
`AdminUsageTotals`, `AdminUsagePerUser`, `GetJudgeRunUsageForTarget`.

## D8 — Schema

One migration, DDL only, inline CHECKs. Its number is a draft until merge (goose numbers
are assigned at merge time).

- **`run_usage_legs`**: `run_id` (FK `runs` ON DELETE CASCADE), `leg_id uuid`,
  `init_seq bigint NULL`, `sdk_session_id text NULL`, `closed_through int NULL`
  (`>= 0`), `covered_through int NULL` (`>= 0`), `covered_cumulative boolean NOT NULL
  DEFAULT false`, `dropped_records bigint NOT NULL DEFAULT 0` (`>= 0`),
  `claim_generation`, `created_at`. PK `(run_id, leg_id)`.
- **`run_usage_messages`**: `run_id`, `message_id text` (capped), `leg_id`, `ordinal int
  NOT NULL` (`>= 1`), `frame_session_id`, `model` (capped), `subagent`, six token
  columns `bigint` (`>= 0`; the cache split nullable), `output_final`, `service_tier`,
  `conflict boolean NOT NULL DEFAULT false`, `claim_generation`, `created_at`. PK
  `(run_id, message_id)`; FK `(run_id, leg_id)` to `run_usage_legs` ON DELETE CASCADE;
  non-unique index `(run_id, leg_id, ordinal)`.
- **`run_usage_tail_state`**: `run_id` PK (FK `runs` ON DELETE CASCADE),
  `record_cap_reached boolean NOT NULL DEFAULT false`, `capped_records bigint NOT NULL
  DEFAULT 0`, `capped_legs bigint NOT NULL DEFAULT 0`.

Upserts are monotone and touch only their own columns; rows are written in
`(leg_id, message_id)` order. The tail and its coverage are read in one REPEATABLE READ
snapshot. The boot refold (`RefoldRunUsage`) is unaffected: the leg extension lives in
the incremental wrapper `foldRunUsage`, not in the shared `foldUsageFrames` body the
refold runs inside its transaction after `DeleteRunUsage`, so the refold never takes the
advisory lock after a row write.

## D9 — The `/usage` route matches `/messages`

The route is registered beside `/runs/{id}/messages` and matches its claim fence:
worker auth, `ErrMissingClaimGeneration` (409), the
`InsertRunMessage` claim-generation fence inside the route's own transaction (a miss is
a `{disposition: "stale_claim"}` 409), `httpx.DecodeJSONLimited` (413) with
`DisallowUnknownFields`, NUL stripping and rune caps (400 for bad input, 500 for
server faults), and a row in `route_limiter_mounts_test.go`. It **deviates** from
`/messages` on two points, deliberately: it answers `ErrRunNotOwned` with a typed
`stale` 404 (`httpx.ErrorReason`, as the inclusion receipt route does) where
`WorkerRunMessages` answers an untyped 404, so an ownership change never counts toward
the recorder's route-missing streak; and it is not in `laneWorkerAllowlist`, so a lane
worker gets 403 (`/messages` is allowlisted). A request carries at most 500 records and 16 leg
markers; more is a 400.

The fence is rechecked with `FOR SHARE` (`RunUsageFenceLiveLocked`) as the **last statement before
Commit**, binding `worker_id` as well, so a release or reclaim that commits after the early check
still discards the whole post and one that starts later waits for our commit.

The route rejects chat and Codex runs, and the leg-coverage fold extension runs only
for Claude non-chat runs. **The Codex exclusion is new**, not parity:
`foldUsageFrames` does fold Codex result frames into the metered total. Codex has no
`message.id`-keyed per-call stream to capture, and extending the tail to it is a
separate decision.

## D10 — Surfaces

- `RunDTO.usage_estimated_tail` (omitempty): `input_tokens`, `cache_read_tokens`,
  `cache_creation_tokens`, `output_tokens`, `cost_usd` (nullable), `cost_status`
  (`estimated` or `unpriced`), `price_table_version`, `coverage`, `coverage_reasons`,
  `models[]`. Filled in `GetRun` beside the metered `Usage`.
- CLI: `uzi run get --json` emits it; the human view adds an `EST. TAIL` row beside
  COST.
- Web: the run page shows an "Estimated, not in the total" block apart from the metered
  total: tokens, the cost or "cost unknown", the price-table version and the coverage
  reasons. The current wording distinguishes this separate tail from recorded
  API-equivalent cost; it changes no fold or aggregate boundary (see
  [PRD #2559](../prds/done/2559-codex-subscription-cost.md)).

## D11 — Per-run caps, atomic, with one lock order

- `maxUsageMessagesPerRun = 20000`, `maxUsageLegsPerRun = 500`.
- **Route.** Each post is one transaction. Its **first** statement is
  `pg_advisory_xact_lock(<usage namespace>, hashtext(run_id))`, before any row write.
  It then counts existing rows, inserts new ids only up to the headroom (in ordinal
  order; updates to existing rows are always allowed), and upserts
  `run_usage_tail_state` with the dropped counts.
- **Fold.** The init and result leg upsert takes the **same** lock first, applies the
  same cap, and runs in its own short transaction after `UpsertRunUsage`. A cap hit sets
  `record_cap_reached` and skips the leg write; a cap hit never fails the append. A
  database error fails the append (500) so the worker re-delivers, the metered fold's contract;
  all writes are idempotent or monotone. Chosen over read-time derivation (review of !2214). The
  stamp transaction takes **no claim fence**: its stamps derive from frames already stored under
  `InsertRunMessage`'s `generation_live` fence, and a fence here would turn a valid stamp into a
  409 and lose it permanently.
- **Lock order.** The advisory lock is always taken before any row write, on both
  paths; no path takes them in the reverse order, so the two cannot deadlock. The route
  then row-locks the `runs` row (`FOR SHARE`, the fence recheck) last, while holding the
  advisory lock; no path that holds a `runs` row lock ever takes the usage advisory lock,
  so that adds no cycle.
- `record_cap_reached` always yields that coverage reason; a capped run is never
  `complete`.

## Rollout safety, both orders

- **api first, old worker.** No worker posts to `/usage` and no `init` or `result` frame
  carries the new stamps. No usage rows exist, so no tail is computed and the DTO
  field is omitted. Metering is unchanged.
- **New worker, old api.** `/usage` returns an untyped 404 or 405; after the streak the
  recorder disables the route for the process and stops posting. The extra stamps on
  `init` and `result` payloads are inert keys the old fold never reads. Metering is
  unchanged.
- **A delayed post after the claim moved on** fails the claim-generation fence with a
  `stale_claim` 409 and stops only that run's recorder. Records already committed under
  the old claim stay; they are usage that happened.

## Known limits

- **The resumed-cumulative premise.** Arm (b) assumes a same-session resumed result's
  cumulative total includes the interrupted turns. uzi cannot verify that; where it
  matters, the conservative supersession rule makes it visible as
  `superseded_uncertain` rather than hiding it.
- **SDK-internal model calls** that never surface as an `assistant` frame or a
  `stream_event` are not captured.
- **A hard kill before a post** loses those records (no disk spill); coverage reports
  `leg_not_closed` or `ordinal_gap`.
- **A stamp lost to a release.** If the stamp write fails and the claim is released before
  re-delivery, the fenced re-delivery is rejected and that stamp is lost: a lost result stamp makes
  the tail over-estimate, a lost init leaves the leg absent.
- **Stamp failures count like metered-fold failures** toward the auto-stop streak, and frames of a
  failed attempt are not broadcast live.
- **The tail is an estimate.** It is priced from a recorded public price table, not
  billed amounts, and is never merged into the metered total.

## Deferred

- Tail figures in aggregates (per-user, factory-wide, judge).
- A TUI badge for the tail.
