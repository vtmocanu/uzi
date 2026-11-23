# ADR-2555: Persist Codex response attribution separately from activity

**Status**: Accepted (implemented, issue #2555)
**Date**: 2026-10-09
**Issue**: [#2555](https://github.com/vtmocanu/uzi/issues/2555)

## Decision

Persist adopted Codex response usage as an accounting status record. It is evidence for
estimated agent attribution, separate from result-frame billing and visible activity.

The neutral harness event is `usage_record`. The reducer writes the existing status
message shape with payload `{ event: "codex_response_usage", usage_response_id, usage, model }`
and the existing agent role, instance and label fields. No API endpoint, DTO or migration
is added. The original records survive persistence and REST/WebSocket replay.

`usage_response_id` is a worker-generated UUID, 36 ASCII characters, generated once for
each adopted complete response. It is neither provider text nor a hash of token values.
Replay retains that identity; two genuine responses with identical buckets have distinct
identities and both count in confirmed attribution. The web validates the bounded UUID
and uses it in a namespace separate from Claude signatures. Missing or malformed
identities on this dedicated event are omitted, never downgraded to signature dedup.

## Adoption, validity and lane ownership

The existing Codex accountant remains the sole adoption gate. Unknown threads,
unproved resumed baselines, duplicates, stale/out-of-order notifications and proved
historical replays produce no current-response measurement. Its existing response
adoption sites return immutable measurements; totals, reconciliation, modelUsage,
pricing and cost behavior remain unchanged.

Attribution requires complete pricing evidence and
`cachedInputTokens + cacheWriteInputTokens <= inputTokens`. Complete individual
numeric buckets do not prove that cache-split invariant. Incomplete evidence or an
impossible split emits no complete attribution record, without changing accountant
adoption or pricing. Missing responses are never padded from cumulative totals.

The emitted Claude-shaped buckets are:

- `input_tokens = max(0, inputTokens - cachedInputTokens - cacheWriteInputTokens)`
- `cache_read_input_tokens = cachedInputTokens`
- `cache_creation_input_tokens = cacheWriteInputTokens`
- `output_tokens = outputTokens`

The root uses the lead lane and its registered model. A child uses its admitted dispatch
role, instance and label, and its registered model. Usage-lane bindings survive normal
display-sink closure so late adopted child notes keep their lane. Already-adopted child
measurements awaiting a proved binding are retained only for the current turn and
flushed on successful binding. Failed/rejected/ambiguous startup never fabricates lead
attribution. Pending measurements and usage-lane state are disposed on termination,
cancellation or stream failure; accountant history is preserved.

Usage is emitted independently of assistant text and child text caps. Already-adopted
attributable records are drained before the first terminal event, which is where the
executor stops consuming. The reducer persists these events directly, without frame
signal/context processing. They do not establish model-processing evidence, lead text,
subagent activity or empty-turn behavior. They are not Claude usage-journal leg/ordinal
records and must not enter Claude uncovered-tail accounting.

## Web presentation and attribution limits

The full message stream remains the usage reducer and sequence/replay source.
Accounting statuses are excluded before feed caps, grouping, counts, summaries,
recency, active-speaker selection and accessibility announcements. A direct event-row
guard also hides them. Hiding just the row is insufficient: it would leave empty lanes
and inflated counts.

Claude confirmed attribution and live usage share the existing signature:
`JSON.stringify([agent_instance, input_tokens, cache_read_input_tokens, cache_creation_input_tokens, output_tokens])`,
with the existing finite-number-or-zero coercion. Confirmed attribution deduplicates
all token buckets and model-frequency counts together. Distinct instances remain
separate; identical signatures on one lane may collapse genuine calls, including across
legs. This accepted heuristic makes the table an estimate. Context remains latest-wins
outside attribution dedup. Existing grouping by agent role is retained.

Codex confirmed attribution uses response identity instead. Live usage retains its
existing signature semantics for both harnesses, so identical Codex buckets on one lane
can still collapse on the live surface. Result-frame billing totals and phase folds
remain independent and unchanged. The per-agent Out column is omitted for both
harnesses because Claude assistant output snapshots cannot support complete output
attribution; the derived output bucket is retained and deduplicated. Per-agent cost
remains unavailable. The footer discloses estimated and potentially incomplete attribution.

Phase turns and duration are nullable finite measurements; a run total is unknown if
any included phase lacks that field. Measured zero stays zero. Codex duration measures
wall-clock processing time from each root turn/start dispatch, including RPC time, and
is emitted as duration_ms. Turns and wire total_cost_usd remain absent, preserving run
lifecycle behavior. This processing-duration sum is distinct from elapsed run age,
which includes parks.

## CLI/TUI presentation contract

Human run logs omit these accounting statuses after the whole-DTO JSON branch, so
JSON logs retain the complete records. The shared Go-local predicate requires kind
status and the exact top-level event string. TUI rebuild filters before real and ALL
lanes; active-lane selection and rail activity use the same filter. Transcript caches,
counts, suffixes, context and recency consume the filtered lanes. Empty/loading checks
use visible lanes, so an accounting-only loaded page has the normal empty presentation.

Raw received frames, dedup state, sequence bounds, pagination and follow cursors remain
complete, including accounting-only pages. Human log cursors advance from the highest
received sequence, even when nothing renders. Transports, question lookup, plan freshness
and the demo's raw sequence allocation are unchanged.

The backfill badge uses the count-free phrase "⇡ loading earlier". Raw sequence-derived
held/total counts cannot describe visible transcript entries once accounting statuses
are hidden. Deterministic Update/View tests cover this boundary, including light/dark TrueColor and
Ascii/NoTTY profiles. The shipped offline generator includes a lead/child accounting
scene with the loading badge for fresh visual review.

## Consequences

Attribution may be lower than billing totals when evidence is missing or invalid.
Claude attribution can undercount distinct calls with identical signatures. These
limits are disclosed rather than repaired with invented measurements. Accounting
statuses still advance the server's existing raw sequence and last_activity_at path;
presentation exclusion does not claim those backend timestamps are unchanged.

Future consumers must distinguish accounting evidence from visible activity and
preserve raw cursor progress. Changing adoption or pricing to improve presentation,
using token values as Codex response identity, or attributing an unbound child to lead
would violate this contract.

## Amendment (#4580, 2026-11-21)

The decision above stands. This amendment lets the Codex accountant attribute one
thread's usage to more than one model. For well-formed usage nothing recorded or billed
changes; malformed reasoning or total-token dips are now refused (see Admission).

- **Segments.** A thread's usage is split into model segments. Root response
  attribution uses the root thread's current segment model, which qualifies "The root
  uses the lead lane and its registered model": the registered model is the current
  segment's, not necessarily the first. Child lane and identity rules are unchanged,
  and attribution stays distinct from result-frame billing.
- **Cuts.** A cut is made only after the caller has recorded the historical replay
  boundary, so replayed history stays in the earlier segment or is never charged. The
  caller contract: before cutting, it must have recorded every usage note belonging to
  the earlier segment, historical replay and live notes of earlier turns. A note
  delivered after the cut that advances the running max is attributed to the new
  segment and its model. The #3253 CodexHarness root-boundary caller enforces
  this ordering as described below. The first registration of
  a thread still wins and claim-shared deduplication is kept. Segment differences are
  clamped at zero as a defensive floor, identical to the plain difference for
  non-negative buckets under the monotone gate.
- **Admission.** The monotone gate covers seven columns: the six wire buckets plus
  clamped uncached input. A malformed reasoning or `totalTokens` dip on an advancing
  note is refused, the model is reported `unreported`, and proven tokens are retained.
- **Reconciliation and pricing** run per segment on that segment's own responses and
  evidence flag. A segment that misses a response degrades its own model. When the
  dropped note is the last one before a cut, its tokens appear in the next segment's
  delta, so the next model degrades and the earlier model may stay metered while
  understated per model; the run-level cost is still unreported. Incompleteness of the
  thread taints every segment model.
- **Rebinding an incomplete thread** keeps the replaced model as a zero-token
  `unreported` entry (PRD D5). Healthy rebinds drop the old model.
- **Uncached input** for threads with two or more segments is the difference of
  clamped cumulative uncached values per segment, so it is conserved per thread under
  malformed cache splits. A malformed split at any boundary marks the thread
  incomplete, every segment model `unreported`, tokens retained (PRD D6). Single-segment
  output is unchanged. Incompleteness also sets the claim-wide `usageIncomplete`,
  which an uncut thread with the same data does not; this asymmetry is accepted.
- **Production caller (#3253).** `CodexHarness.recordUsageNote` records buffered
  replay into the planning segment before calling `beginModelSegment` once at the
  first matching root `turn_started`. Thread preparation and completion without
  a matching start do not cut. Old-harness disposal and cleanup proof precede fresh
  provider preparation/publication; its closed fence rejects late old notes.
  Pending transition intent is claim-shared across replacements, while verification
  binds to the new harness. Child threads retain their own registrations and are
  not cut by the root transition. Per-response `usage_record` attribution uses the
  adopted response's segment model. The seven-column admission gate remains in
  `record()`; the integration does not replace it.
- **Crash boundary.** A process crash loses the in-memory accountant, unflushed
  frames and pending cut. The next claim creates a new accountant for its current
  phase and baselines proved history, without reconstructing missing spend.
  Delivered frames retain the existing idempotent, additive per-leg server fold.
  This is not an fsync or storage-crash recovery guarantee. The cold switch and
  verification contract are recorded in [ADR-3252](./3252-phase-model-switch.md).

Rationale and rejected alternatives are in the PRD Decision Log
([PRD #4580](../prds/done/4580-codex-usage-model-segments.md)).
