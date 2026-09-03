# ADR-1079: `run_usage` keys each leg by its position-absolute `init`-frame count, not by a mutable counter

**Status**: Accepted (implemented, issue #1079, M1–M3 landed)
**Date**: 2026-09-03
**Deciders**: architect + fact-check pass over the first PRD draft (2026-09-03), coder (implementation), reviewer (M3 poison-batch finding addressed).
**Supersedes**: [ADR-632](0632-run-usage-lineage-epoch.md) — its within-lineage MAX rationale, its `resume_lineage_break`-driven epoch bump, and the `runs.lineage_epoch` counter. [ADR-195](0195-run-usage-per-model-fold.md) is **unchanged**: the client still folds `modelUsage` per model, and parity between the two readers is still pinned by a fixture both halves read (this ADR adds a second such fixture pair).
**PRD**: [prds/done/1079-run-usage-per-leg-fold.md](../prds/done/1079-run-usage-per-leg-fold.md) — carries the full milestone breakdown, code anchors and Decision Log D1–D10; this ADR restates the load-bearing shipped contract and the two things a future reader most needs and will not re-derive from the diff: why ADR-632's design was wrong in a way its own tests couldn't see, and what the backfill does and does not reach.

## Decision (summary)

`run_usage` under-reported the cost of every multi-iteration run, because the Agent SDK reports cost **per `query()` call**, not cumulatively over a resumed session, while `run_usage` keyed rows `(run_id, session_id, model)` and merged with `GREATEST` on the assumption that later frames carried the earlier ones forward. The worker starts one `query()` per turn (planning, then each implement iteration), all resumed under the same `runs.session_id`, so every turn's result frame is one **leg** reporting only that leg's cost — and the `GREATEST` merge silently kept only the largest leg.

The fix: **a leg is one SDK `query()` process, and it is marked by the `system/init` status frame the worker persists at the start of every `query()` call, resumed or not.** A result frame's leg index (`lineage_epoch`) is now derived at fold time as the count of persisted `init` frames of the same run with a lower `seq` — a pure function of `(run_id, seq)`, computed by `CountRunInitFramesBefore` against `run_messages` (backed by a new partial index, migration `00188_run_usage_leg_key.sql`). `lineage_epoch` moved from a mutable per-run counter into the `run_usage` **primary key** `(run_id, session_id, model, lineage_epoch)`; `GREATEST` now only de-dups re-delivery of the *same* frame. The `run_usage_totals` view (`00177`, MAX within `(run_id, model, lineage_epoch)` then SUM across) is **unchanged** — it already computed exactly the per-leg rule, it just never had a leg-keyed input to run it over.

## Context

`run_usage` is the per-run token/cost ledger (PRD #40); every rollup surface reads it — `GetRunUsageTotal`, `ListRunsForUser`, `SelfUsage`, `AdminUsageTotals`, `AdminUsagePerUser`, the judge join, and the run page's client-side fold (`web/src/lib/runUsage.ts`, ADR-195). ADR-632 (issue #632) found and fixed a *different* bug — a dropped-resume fresh SDK session starting a new accumulator from zero under a fresh `session_id` — with a `lineage_epoch` marker bumped once per `resume_lineage_break` event. It recorded its within-lineage MAX-carry-forward assumption as explicitly **PROVISIONAL**, unvalidated for want of a live two-phase run. This PRD is that validation, and the answer is that the assumption is false in the *common* case, not just the dropped-resume case ADR-632 targeted.

**The SDK's own documentation says so directly**: *"When using sessions, the cost reported is limited to the individual query call rather than the entire session"* and *"Because the SDK does not maintain a session-level total, developers are responsible for manually accumulating these values."* (https://code.claude.com/docs/en/agent-sdk/cost-tracking, resolved 2026-09-03; a uzi worker has no open-web egress, so this is restated here rather than only linked).

**Measured, live, sum of `payload.total_cost_usd` over each run's `result` frames against the stored `usage.cost_usd`:**

| run | legs | stored | true (sum of legs) | ratio |
|---|---|---|---|---|
| `02854d5e` #1064 | plan + 3 | $77.19 | $153.58 (6.33 / 44.12 / 75.03 / 28.10) | 2.0x |
| `d8648b31` #966 | 7 | $43.02 | $153.95 | 3.6x |
| `55977a4f` #1030 | 7 | $26.70 | $100.63 | 3.8x |
| `011433d0` #991 | 6 | $49.31 | $68.51 | 1.4x |

For `02854d5e` the stored value matched a max-per-model fold **to the cent** (opus 75.029070 from leg 3 + haiku 0.010111 from leg 1 + sonnet 2.146358 from leg 4 = 77.185539; `output_tokens` 514572 = 488268 leg-3-opus + 26292 leg-4-sonnet + 12 leg-1-haiku), confirming the bug was exactly the `GREATEST` collapse, not a different discrepancy. The proof that frames are per-leg rather than cumulative is that **leg 4 is strictly smaller than leg 3 in every opus column** (input 438 < 1285, output 158427 < 488268, cache_read 25620187 < 96109126, cache_creation 1285948 < 2223283, cost 25.955935 < 75.029070) while `context.used` kept growing across that same transition (165188 → 246422) and no `resume_lineage_break` was emitted — a cumulative reading is structurally impossible for a non-monotonic sequence with no break in between.

## What ADR-632 got right, and what it got wrong

**Right**: `session_id` is the row-splitter. Legs from a genuinely fresh SDK process (dropped resume) land in a different `run_usage` row because that process also emits a fresh `init` and (per the worker's `onSessionId` latch + `SetRunRunning`'s `COALESCE`) a fresh `session_id`. This PRD keeps `session_id` in the key unchanged (zero churn) precisely because the two designs agree on that split.

**Wrong**: two assumptions, both load-bearing for ADR-632's fix and both false.

1. **Cumulative carry-forward.** ADR-632 assumed a resumed-but-not-dropped `query()` reports a session-level running total, so MAX-within-lineage was "obviously" correct. It is not: every `query()` call, resumed or not, reports only that call's own usage. MAX-within-a-leg is still correct (a leg's own result frame, or a multi-turn process's several cumulative frames, should be MAX'd) — the assumption failed at the *lineage* boundary, not within it.
2. **A mutable counter as the leg id.** `runs.lineage_epoch`, bumped once per persisted `resume_lineage_break`, breaks the moment the epoch sits in a primary key: a re-delivered batch whose co-batched `init` was already seq-deduped stamps at the *current* counter value and lands in a *different* row than its first delivery — verified on the fixture (replaying leg 2 after leg 4 had landed raised opus@epoch-4 from $25.96 to $44.12, a real over-count from a design meant to fix an under-count). A **position-absolute** count of `(run_id, seq)` recomputes identically on every delivery, so idempotency holds by construction; the refold (M3) and the incremental fold (M1) are the same computation over different-sized histories, and the lost-bump failure class ADR-632 documents (a `BumpRunLineageEpoch` statement failing after its triggering row already committed) cannot exist because there is no bump statement to fail.

## The backfill: reach and its boundary

Every result frame of every still-existing run remains in `run_messages` (nothing prunes it; the only deletion is the `runs → run_messages ON DELETE CASCADE` on repo removal). A boot-time one-shot (`RefoldRunUsage`, `api/internal/workersvc/usage_refold.go`) re-folds every **pre-migration, non-chat, terminal** run through the *same* fold function used for live incremental writes, in one transaction per run: delete the run's old `run_usage` rows, re-fold its full `status`/`error` history, mark it refolded, commit. Scoped by `runs.usage_refolded` (`boolean NOT NULL DEFAULT true`; migration `00188` sets it `false` for every non-chat row that exists at migration time, so new rows are born refolded and the incremental fold stays the only writer for live runs). Knobs `UZI_USAGE_REFOLD_ENABLED` (default `true`) and `UZI_USAGE_REFOLD_BATCH` (default `50`), documented in `docs/configuration.md`. A review pass added a same-cycle skip-set so a batch of runs that fail to refold cannot starve newer, healthy runs from ever being listed again.

**The backfill recovers exactly the runs whose `init` frames were persisted** — a run without any `init` frame refolds to the same collapsed epoch-0 value it has today, never worse, because `CountRunInitFramesBefore` then answers zero for every result frame and the fold behaves exactly as the pre-fix code did. This PRD does not claim, and a reader should not assume, that every historical run in a given deployment's database carries `init` frames all the way back — that is a maintainer measurement after merge (`SELECT count(*) FROM runs r WHERE NOT EXISTS (SELECT 1 FROM run_messages m WHERE m.run_id = r.id AND m.kind='status' AND m.payload->>'event'='init')`), not a claim shipped code makes about itself.

## Migration lock window

`00188_run_usage_leg_key.sql` runs as a **single goose transaction**: the `run_usage` primary-key rebuild, the new partial index `idx_run_messages_init` on `run_messages (run_id, seq) WHERE kind = 'status' AND payload->>'event' = 'init'`, and the `UPDATE runs SET usage_refolded = false WHERE kind <> 'chat'` all commit or roll back together. At uzi's current scale (932 runs at the time this PRD was measured) that lock window is acceptable. A maintainer scaling to a very large `run_messages` table should reconsider building the partial index concurrently (outside the transaction, with its own migration step and a retry-on-failure story) before this migration becomes slow enough to matter.

## Consequences

- Every displayed run cost, user total, and factory total changes (upward) on the next boot after this ships, for any run with more than one leg. That is the fix, not a regression — see the CHANGELOG entry and `docs/run-cost.md`.
- The client (`web/src/lib/runUsage.ts`, M2) now resets its per-model high-water marks at every persisted `init` frame, so the run page's per-phase table shows each leg's own figures rather than the old "post-reset phase reads 0" masking; both folds count the same persisted `init` frames, so they continue to agree by mechanism, not by decision-record assertion — the same rule ADR-195 established, now pinned by a second recorded fixture pair (`fixtures/run-usage/README.md`).
- `runs.lineage_epoch`, `BumpRunLineageEpoch`, and the `resume_lineage_break`-driven bump loop are retired. The `resume_lineage_break` event itself is unaffected and stays persisted as a dropped-resume diagnostic — a break is always followed by a fresh `init`, which the position-absolute count already handles without needing to know a break happened.
- ADR-195 stands unchanged: the client still reads `modelUsage` per model, never the frame's top-level `usage`, and that half of the contract was never in question here.

