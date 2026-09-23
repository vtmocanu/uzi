# ADR-1562: the worker stamps which reading a Claude result frame carries, instead of the server guessing from an SDK version

**Status**: Accepted (implemented, issue #1562)
**Date**: 2026-09-23
**Deciders**: architect (design), coder (implementation), reviewer.
**Amends**: [ADR-1079](1079-run-usage-per-leg-fold.md) — its per-leg SUM rule (`run_usage_totals` MAXes within a leg, then SUMs across legs) is now the `per_leg` half of a two-basis fold; the leg key (`lineage_epoch`, one `init` frame per leg) is unchanged. [ADR-195](0195-run-usage-per-model-fold.md) is unchanged: the client still folds `modelUsage` per model, never the frame's top-level `usage`, and both readers still agree by mechanism, pinned by a third fixture pair.
**PRD**: none — this issue's spec lived in the issue body and its live measurement; the fixture at [`fixtures/run-usage/README.md`](../fixtures/run-usage/README.md) (section "The `cumulative` pair") carries the authoritative rule text this ADR restates.

## Decision (summary)

ADR-1079 fixed a real under-count on the premise that the Agent SDK reports cost
**per `query()` call**. From Claude Agent SDK 0.3.277 (pinned at 0.3.280 in this repo
since 2026-09-23, commit 6085c5c8, first shipped in v0.84.0-rc.8) that premise stopped
holding for a **resumed** session: a resumed leg's result frame now reports the
**running total of the whole session**, not just that leg. Summing legs — the ADR-1079
rule — then over-counts a multi-leg run several-fold: a live 5-leg run stored
42.725704 USD against a true 10.4907258 USD, a 4.1x inflation.

The fix does not sniff the SDK version at the server. It cannot: the only thing a
Claude result frame told the server, before this issue, was `{event, model}` on its
`init` frame and the raw token/cost numbers on its `result` frame — no field said which
reading applied. Instead, the **worker** (which knows which SDK it is running and what
it asked the SDK to do) stamps two explicit markers, and the server folds strictly by
what it is told:

| marker | on | meaning |
|---|---|---|
| `usage_basis: "session_cumulative"` | a Claude `result` frame | its `modelUsage` is the running total of the session the leg continued |
| `fresh_session: true` | an `init` frame | this SDK process did **not** continue the requested session |

A frame without `usage_basis` is `per_leg` — every frame recorded before this issue,
every Codex frame, and the stub executor. The server ignores the marker entirely on a
Codex run (Codex is never honoured, below).

## The explicit markers, and why not version-sniffing

`usage_basis` is stamped by the caller of `projectResult` (`agent/src/harness-messages.ts`),
never derived from the installed SDK's `package.json` at fold time. The run-lane reducer
sets it when the harness's terminal event reports `basis === "session"`
(`agent/src/claude-harness.ts`); the judge/chat/advice single-query path
(`mapResult` in `agent/src/sdk-messages.ts`) always marks its one-shot result, since a
lone `query()` call is trivially "the whole session". Both are hardcoded to the pinned
SDK's known behavior, not computed from a live version check — a
`sdk-version-usage-basis-guard.test.ts` guard instead pins the *installed* SDK at
`>= 0.3.277` and fails loudly if the dependency is ever rolled back below it, so a
rollback is caught at test time rather than silently mis-folding at runtime.

`fresh_session` marks a **session** boundary, not a leg boundary (`lineage_epoch`
already exists for that, unchanged from ADR-1079). It is `true` on an `init` frame when
either (a) the run did not request a resume for this turn, or (b) a resume was
requested but the SDK's own `init` frame reports a different `session_id` than the one
requested (`ClaudeHarness.decode`, `agent/src/claude-harness.ts`). An **absent** SDK
`session_id` is treated as **unknown, not fresh** — the worker cannot prove the SDK
started a different session, so it does not claim one did, and the flag is simply
omitted (never `false`; a pre-#1562 init frame stays byte-identical).

## `lineage_index`: which session a leg belongs to

`lineage_epoch` (ADR-1079) already counts the **leg** — one `init` frame, one `query()`
process. This issue adds `lineage_index`, the **session** a leg belongs to: the count of
`fresh_session: true` init frames with a lower `seq`, **excluding the run's own first
init** (`CountRunLineageRestartsBefore`, `api/internal/store/queries/runtime.sql`).
Lineage 0 is the run's initial session, whether or not its first init happens to carry
`fresh_session: true` — a fresh session that starts *before* any prior one is not a
restart. Each fresh session that starts *after* a prior one opens lineage 1, 2, ….

**Why exclude the run's first init.** Lineage 0 must be able to hold a run that is
in flight across the #1562 deploy: its earlier legs (persisted before the worker
shipped this stamp) are unmarked `per_leg`, and its next leg — the first one the
upgraded worker resumes after redeploy — is marked `session_cumulative`. Both must land
in the same `(model, lineage_index)` bucket so the high-water fold (below) can absorb
the transition correctly; splitting the run's very first session into its own
"restart" would instead open a spurious lineage 1 for the marked leg and leave lineage
0's unmarked legs permanently unfolded against it.

**Why not `resume_lineage_break`.** ADR-632's `resume_lineage_break` event (retired by
ADR-1079, but still persisted as a diagnostic) is emitted only when a *known* session id
fails to resolve on resume — the CLI reports an error resuming a specific, remembered
session. It is not emitted, and cannot be, when the run never had a session id to lose
in the first place, or when a resume silently starts fresh without an error the worker
can observe. A lost session-id report that starts fresh **silently** — no error, no
break event, just an `init` frame whose `session_id` does not match what was asked for
— is exactly the case `fresh_session` exists to catch and `resume_lineage_break` cannot.

## The fold rule

Per `(run_id, model, lineage_index)`, legs taken in `lineage_epoch` order, a running
total `R` starts at 0, per column:

- a `per_leg` leg does `R += v` (ADR-1079's rule, unchanged);
- a `session_cumulative` leg does `R = max(R, v)`.

The model's total is the SUM of `R` over that model's lineages; the run's total is the
SUM over models — both unchanged from ADR-1079. This is a **high-water mark**, not
"subtract the previous leg's value": a column that goes down, or a model missing from a
later leg, adds nothing to `R`.

`run_usage_totals` (rewritten in migration `00244_run_usage_session_cumulative.sql`)
implements this as a closed form rather than a literal running fold, per lineage:

```
R = (Σ over per_leg legs of v)  +  GREATEST(0, MAX over cumulative legs of (v − s_prefix))
```

where `s_prefix` is the running SUM of the *per_leg* values strictly before that leg.
The two are equal by induction on the legs in epoch order (a per_leg leg advances both
sides identically; a cumulative leg's `max(R_{i-1}, v_i)` equals `S + max(M, v_i − S)`
with `S` the per_leg prefix sum and `M` the running max term, which is exactly the next
step of the closed form). `GREATEST(0, NULL) = 0` in PostgreSQL, so a lineage with no
cumulative leg at all reduces to the plain per_leg SUM — every pre-#1562 run (all
`per_leg`, lineage 0 throughout) folds to byte-identical totals under the new view.

**Why high-water, not previous-leg subtraction.** A resumed leg's running total can be
non-monotonic against the *immediately preceding* leg for reasons unrelated to actual
spend (a dropped and silently-restarted turn inside the same lineage, a model whose
usage the SDK omits from one frame and restates in the next) — ADR-1079 itself measured
exactly this non-monotonicity across legs 3 and 4 of a live run, in the *other*
direction (a genuine new leg reporting less than the leg before it). Subtracting the
previous leg's value can go negative and either clamp away real spend or, worse, invert
the sign silently; taking the running maximum can only ever grow, matching what a
session-cumulative total actually is: the most complete figure any leg of that session
has reported so far.

**Why not a run-wide MAX.** A run-wide maximum across every leg regardless of lineage
would silently collapse a genuine session restart (a `fresh_session` leg starting a new,
smaller-looking accumulation) into the previous session's larger figure, under-counting
the restarted session's own spend. Partitioning by `lineage_index` first, then applying
the high-water rule only within a lineage, is what lets a restart correctly begin a new
running total at zero rather than being swallowed by the lineage before it.

## The one-way basis upgrade

`UpsertRunUsage` (`api/internal/store/queries/runtime.sql`) upgrades `usage_basis`
**one-way** on conflict: once either the existing row or the incoming frame reads
`session_cumulative`, the row stays `session_cumulative` for good. This is what makes
the in-flight-across-deploy case (above) safe without a special code path — an unmarked
`per_leg` frame for a leg that has already been marked `session_cumulative` by a later
delivery (a re-delivered batch, an at-least-once retry) never downgrades the row back to
a summed reading. Both-`per_leg` stays `per_leg`; `lineage_index` is a pure function of
`(run_id, seq)` like `lineage_epoch`, so on conflict both sides are already equal and the
existing value is simply kept.

## Codex is never honoured

The marker is Claude-only by construction, at three independent layers: the server gate
(`foldUsageFrames` in `api/internal/workersvc/usage_fold.go` only honours
`usage_basis: "session_cumulative"` when `run.Harness != harnessCodex`, regardless of
what a frame's payload claims), the agent (the Codex harness never sets `usageBasis` or
`freshSession` on its own emitted frames — those fields are Claude-harness- and
mapResult-specific), and the client (the run page's fold, `web/src/lib/runUsage.ts`,
applies the identical basis/lineage rule but only Claude frames ever carry the marker to
begin with). A Codex run's `modelUsage` is per `query()` call today and stays `per_leg`
regardless of any future Codex SDK behavior change; extending the marker to Codex is a
distinct, unshipped decision this ADR does not make.

## `run_usage_totals` is now the only authoritative total

**A `SUM` over raw `run_usage` rows is no longer a valid total for any run that has ever
carried a `session_cumulative` leg.** Before this issue, summing `run_usage` rows
directly (skipping the view) merely repeated work the view already did, for the same
per-leg answer. Now the two can disagree by exactly the amount this issue fixes: the raw
rows still store each leg's own reported figures (a `session_cumulative` leg's row holds
that leg's *own* running-total snapshot, not its marginal contribution), so summing them
reproduces the old over-count. Every reader of run cost — `GetRunUsageTotal`,
`ListRunsForUser`, `SelfUsage`, `AdminUsageTotals`, `AdminUsagePerUser`, the judge join,
the client's `deriveRunUsage` — must go through the view (or its client-side mirror of
the same rule); a future ad hoc query against `run_usage` directly will silently
reproduce the bug this ADR fixes.

## Backfill: accepted, not performed

**Decision: do not backfill.** The affected set is narrow and shrinking on its own: it
is exactly the Claude legs of runs that ran on SDK 0.3.280 (workers built from
`v0.84.0-rc.8` onward) **before** this fix ships, whose `run_usage` rows were written
with the old per-leg-SUM view and no `usage_basis` marker to distinguish them. Once this
migrates in, `RefoldRunUsage` (ADR-1079's boot-time refolder, `usage_refold.go`) does
**not** pick these runs up — it selects only `runs.usage_refolded = false`, and every one
of these runs is already marked refolded (they postdate ADR-1079's migration entirely).
Re-running the fold changes nothing for them anyway: `foldUsageFrames` derives
`usage_basis` from each frame's *persisted* payload, and a frame from that window never
carried the `usage_basis` key at all (the worker only started stamping it in this
issue's agent-side commit), so a re-fold of the same `run_messages` history reproduces
the identical `per_leg` rows and the identical over-counted total.

There is also **no per-claim worker/SDK-version provenance** to retroactively re-derive
the correct basis from: `run_usage.claim_generation` records which claim epoch produced
a row, not which SDK build the worker was running. A correct backfill would need to
re-derive, per leg, whether the worker that produced it happened to be running SDK
`>= 0.3.277`, which nothing in the persisted history records.

A maintainer wanting to enumerate the affected runs (to report their true cost by hand,
or to decide whether to intervene manually) can list candidates with:

```sql
-- Runs with >= 2 Claude result frames created since the SDK 0.3.280 pin (2026-09-23),
-- none of which carry usage_basis: these ran the affected SDK before the worker
-- started stamping the marker, so their run_usage total is potentially over-counted.
-- Adjust the date to the actual worker-fleet rollout time of v0.84.0-rc.8 if known.
SELECT r.id, r.created_at, r.harness
FROM runs r
JOIN run_messages m
    ON m.run_id = r.id
   AND m.kind IN ('status', 'error')
   AND m.payload->>'event' = 'result'
GROUP BY r.id, r.created_at, r.harness
HAVING COUNT(*) FILTER (WHERE m.payload->>'event' = 'result') >= 2
   AND bool_and(m.payload->'usage_basis' IS NULL)
   AND r.created_at >= '2026-09-23'
   AND r.harness = 'claude';
```

## Known limits

- **(a) The conditional-transition assumption.** A run whose earlier legs ran on SDK
  0.3.273 (pre-cumulative, `per_leg`) followed by later legs on SDK 0.3.280
  (`session_cumulative`, marked) folds exactly only if the 0.3.280 leg's resumed total
  actually includes every earlier turn's spend. If the SDK's running total instead
  starts accumulating fresh from the point of the SDK upgrade rather than from the true
  session start, the closed-form fold under-counts the turns before the upgrade — those
  are absorbed into neither the per_leg sum (they were summed correctly) nor visible in
  the cumulative leg's own reported figure.
- **(b) An undetected session reset under-counts.** The high-water rule assumes a
  `session_cumulative` leg's value only ever grows within a lineage. If the SDK silently
  resets its own internal accumulator without emitting a differing `init` `session_id`
  (so `fresh_session` is never set), the fold has no signal to open a new lineage, and
  the smaller post-reset figure is simply absorbed as a no-op by `GREATEST` — the reset
  segment's spend is lost from the total rather than double-counted.
- **(c) A legacy restart before this deploy gets no lineage split.** `lineage_index` is
  derived only from persisted `fresh_session` flags, which did not exist before this
  issue. A run that restarted its session pre-deploy, with no `fresh_session` marker on
  that restart's `init` frame, stays in lineage 0 for its whole history — correct for the
  `per_leg` legs either side of the restart (ADR-1079's rule already handled that), but
  it means a *future* `session_cumulative` leg resumed from that pre-deploy restart
  cannot be distinguished from one resumed from the run's true start.
- **(d) Chat runs are stamped but not folded.** `foldUsageFrames` excludes
  `runs.kind = 'chat'` entirely (ADR-1079's chat exclusion, unchanged). A chat run's
  Claude frames still carry `usage_basis` and `fresh_session` (the agent-side stamping
  is harness-driven, not run-kind-driven), but nothing ever reads those markers for a
  chat run — they are inert payload bytes for a run kind PRD #40 always excluded from
  the ledger.

## Fixture

[`fixtures/run-usage/README.md`](../fixtures/run-usage/README.md), section
["The `cumulative` pair"](../fixtures/run-usage/README.md#the-cumulative-pair-session-cumulative-resumed-legs-issue-1562),
carries the authoritative rule text (restated above), the five pinned scenarios
(including the live 5-leg run this ADR's numbers come from), and why the rollup is
authored rather than recorded — the shipped server's own answer for these frames is
exactly the defect being fixed, so it cannot be the golden.
