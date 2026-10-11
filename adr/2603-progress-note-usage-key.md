# ADR-2603: A Now-summary call's usage is stored under its own key, never the run's model row

**Status**: Accepted (implemented, PRD #2603)
**Date**: 2026-10-10
**PRD**: [prds/done/2603-run-now-summary.md](../prds/done/2603-run-now-summary.md)
**Numbering**: `2603` is a **PRD** number, like `0035`, `0042` and `0065`.

## Decision (summary)

The usage of the small-model call that writes a "Now" summary rides on its `progress_note`
run message (`payload.model_usage`) and is folded into `run_usage` under a key no result
frame can produce:

```
model         = "progress_note:<model>"     // the provider-reported model id, e.g. progress_note:claude-haiku-4-5-20251001, progress_note:gpt-6-luna
lineage_epoch = the note's own seq
usage_basis   = per_leg, lineage_index 0
```

The invariant, and the only sentence here worth memorising:

> **A summary call's usage must never share a `(run, model, lineage_epoch)` group with the run's own usage, and no two notes may share one with each other.**

Implementation: `foldProgressNoteUsage` in `api/internal/workersvc/progress_note.go`, called
from `api/internal/workersvc/usage_fold.go`.

## Why a bare model key is wrong

`run_usage` is upserted with `GREATEST` per `(run_id, session_id, model, lineage_epoch)`
(`UpsertRunUsage`), and `run_usage_totals` takes a `MAX` per `(run, model, lineage_epoch)`
before it sums (see [ADR-195](0195-run-usage-per-model-fold.md) and
[ADR-632](0632-run-usage-lineage-epoch.md)). Both are idempotent high-water marks, which is
what makes a re-delivered or re-folded result frame a no-op. They are also why a summary
call cannot be folded under the bare model:

- A run's own result frames often carry the same model the Claude summary uses
  (`claude-haiku-4-5-20251001`, from a role pinned to haiku or from the SDK's own internal
  calls, ADR-0195). Under that bare key the note's tokens are compared with the run's
  cumulative leg figure by `GREATEST`/`MAX`, so a small note is absorbed by the larger value and its spend
  disappears from the total, or a larger one replaces the run's figure and the run's
  spend disappears. Either way the run total is wrong.
- Two notes in one leg would collapse into each other for the same reason, since each is a
  separate small call and not a cumulative reading of one session.

A key per note is exact: the seq epoch is a pure function of the frame, so a re-delivered
batch and a full refold land on the same row, `GREATEST` makes the repeat a no-op, and the
`MAX` per group then sums to the true spend of every call.

## What readers must handle

- **`ListRecentUnpricedCodexModels`** (the `pricing.codex` health check) strips the
  `progress_note:` prefix before grouping, so a model used only by summaries is named under
  its real ID and counted with the run's plain rows. Without it the health page would list
  `progress_note:gpt-6-luna` as an unknown model. See
  [Codex price coverage](../docs/admin-health.md#codex-price-coverage).
- **`ListRunUsageFrames`** (the refold) includes `progress_note` frames alongside `status` and
  `error`, so a full refold of a run equals the incremental fold. Dropping the kind would make
  a refold silently discard the summary spend the incremental fold had recorded.
- **The web usage fold** (`web/src/lib/runUsage.ts`) keys notes the same way and shows them as
  a separate "Now summaries" row, so the panel's rows add up to the Run total. The fixtures
  pin the server and client folds to the same rollup, per
  [ADR-195](0195-run-usage-per-model-fold.md)'s rule that two folds are pinned by a shared
  fixture and not by a decision record: `fixtures/run-usage/result-frames-notes.json` (the
  worker frames) and `run-usage-notes.json` (the rollup) pin the server fold, and
  `stored-frames-notes.json` (those frames as the api stores them, itself pinned to the
  normaliser) pins the client fold.
- **Cost.** A Claude entry with no provider `costUSD` is priced from the standard Anthropic
  table; one the table cannot price stores `cost_status = 'unreported'` with its tokens, never
  a metered `$0`. Codex entries go through the same cost derivation as result frames.
- **The stored note carries the resolved cost.** On ingest the api writes each entry's
  resolved `costStatus` (`metered`, `subscription` or `unreported`, derived from the run's
  harness, never from the worker's claim alone) into the stored payload, and `costUSD` only
  when metered, quantized as `run_usage` stores it, through the same resolver the fold uses.
  Readers (the web fold) show a note's cost only when it is metered and otherwise show it as
  unavailable; they carry no price table. A refold of a stored entry gives the same row,
  except that an unreported Claude entry is re-priced and would change if a later price table
  learned its model.

## The payload must not look like a result frame

The api rebuilds every `progress_note` payload on ingest from only `{text, milestone_id,
model_usage}` (`normalizeProgressNotePayload`). In particular a note must never carry:

- **`event: "result"`**: defense in depth. Every leg-end reader gates on the message kind
  (`status` or `error`) before it looks at `event`: `collectUsageStamps` in `usage_tail.go`,
  `foldUsageFrames` in `usage_fold.go` and the web `isResultFrame`. A `progress_note` is
  neither, so none of them would read the key as a leg end today; ingest still drops it so
  that does not depend on every future reader keeping the gate.
- **a top-level `usage` key**: the web fold would count it a second time, beside the
  `model_usage` it already folds under the `progress_note:` key.

Dropping every other key at ingest keeps both from reaching the stored row, whatever a worker
sends.

## Summary passes (issue #2686)

The other SummaryRunner model passes (the intent and plan summaries on Claude, which use the
resolved `summary_model` (default Haiku), and the PR-description summary on Claude or Codex,
`CODEX_PR_DESCRIPTION_MODEL` on Codex) also run on the run's credential, and their spend used
to never reach `run_usage`. They follow the same invariant, under their own key and carrier:

```
model         = "summary_pass:<model>"
lineage_epoch = the summary_usage message's own seq
usage_basis   = per_leg
```

The worker emits one `summary_usage` run message per pass that observed usage, with payload
`{pass: intent|plan|pr_description, model_usage}`; a pass that finishes after the claim is fenced emits none. The api
rebuilds the payload at ingest (`api/internal/workersvc/summary_usage.go`) and folds it
(`foldSummaryUsage`, called from `usage_fold.go`). Each pass is its own row, so the rows sum.

**Why a new kind and not an existing one.** `progress_note` is the Now line, so its readers
would show a summary pass as a Now note. A status or error frame is read as a leg end. A new
kind is neither. A batch holding only usage messages does not count as run activity.

**Readers.**

- The refold (`ListRunUsageFrames`) includes the kind.
- The unpriced-Codex health check (`ListRecentUnpricedCodexModels`) strips the `summary_pass:`
  prefix, as it does `progress_note:`.
- The web usage fold counts it as a separate "Intent, plan & PR summaries" row that adds up to
  the Run total. It never enters a phase or per-agent row, which matters because the per-agent
  branch reads a top-level `usage` key without checking the kind.
- The web transcript and activity feed, `uzi run logs`, and the TUI hide the kind; `--json`
  still carries it. The run judge's input drops it.

The `summary` fixture pair in `fixtures/run-usage/` pins the server and client folds.

## Alternative rejected

Folding under the bare model, "as one more model entry", which is how the PRD first worded it.
It collapses with the run's own rows under `GREATEST`/`MAX`, as above, which is why the PRD's
Usage accounting bullet now points here.

## Verification

- `api/internal/workersvc/progress_note_livedb_test.go`: usage counted once, Codex usage,
  refold equivalence, a note-only batch leaving the stall clock alone, worker-only acceptance.
- `api/internal/workersvc/run_usage_contract_test.go` (`TestRunUsageNotesFoldMatchesAuthoredRollup`
  and its discriminating twin) with the `fixtures/run-usage/*-notes.json` pair.
- `api/internal/store/codex_pricing_livedb_test.go` (`TestRecentUnpricedCodexModelsProgressNoteLiveDB`).
- `api/internal/workersvc/run_usage_contract_test.go` (`TestRunUsageSummaryFoldMatchesAuthoredRollup`) and `web/src/lib/runUsageContract.test.ts` with the `fixtures/run-usage/*-summary.json` pair (issue #2686).
