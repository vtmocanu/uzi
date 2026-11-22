# PRD #4580: Codex usage attributed per model segment with exact per-column conservation

**Issue:** #4580 (number assigned after draft review).
**Parent:** #1703; split from #3253.
**Status:** Implemented, 2026-11-21 (M1 complete on branch `agent/issue-4580`; the landing obligations below remain for exact-head PR CI and landing). Draft baseline origin/main `10876dee680859e4e468ae5f004475f44b31a80a`.
**Labels:** `enhancement`, `PRD`, `area::runtime`, `priority::normal`.
**Blocked by:** none. The accountant and its tests already exist on this base. #3253 is a downstream consumer, not a prerequisite.
**Delivery:** one implementation run, one PR. Accounting is a billing-integrity seam: include reviewer, tester and auditor. No implementation or validation writes under `.github/workflows/`.

## Problem

`CodexUsageAccountant` binds a thread to its first registered model for a whole claim. A caller cannot represent a model transition without attributing later work to the first model. Its regression check also omits `reasoningOutputTokens` and `totalTokens`, allowing malformed cumulative sequences to dip in those columns.

## Outcome and scope gate

One independently valuable outcome: a conservative accountant capable of attributing successive model segments while conserving every token column. It rejects the two previously unchecked dips and exposes a tested segment boundary for #3253. Settings, claiming, model switching and runtime integration are outside this child.

Safe alone: no production caller invokes the new cut yet. For well-formed sequences, totals, cost and `usage_record` events remain byte-identical, pinned by existing accounting tests. Only malformed reasoning/total-token regressions change behavior: refuse them with `advancing_regression` and mark usage `unreported`, retaining proven tokens. This is the independent accounting slice approved by the owner on 2026-11-21.

Acceptance examples:

1. For a synthetic monotone cumulative sequence split A -> B -> A, every column's segment sum equals the claim delta exactly; response attribution and pricing use the model of each segment.
2. A resumed thread with no proved baseline rebinds its empty segment, charges no replay, then accounts only proved new work. Quarantine remains sticky across cuts.
3. A reasoning or total-token dip is refused and conservatively makes all affected segment models `unreported`, retaining proven tokens. Valid single-model sequences retain today's output.

## Out of scope

- Planning settings, claim fields, capability mirrors, API/web/CLI changes and migrations.
- Production calls to `beginModelSegment`, approval-epoch recreation, hold behavior or runtime model verification. These belong to #3253 after this child merges.
- New accountants per epoch, changed thread identities, guessed response attribution or padding missing usage.
- Advice, judge, cross-check and summary accountancy behavior outside the existing accountant's shared validation invariants.

## Modules and seams

### Accountant (`agent/src/codex/token-accounting.ts`)

**Caller-facing interface:** one new method, `beginModelSegment(threadId: string, model: string): void`. Everything else is internal.

**Regression gate (R2-6).** `record`'s gate gains `reasoningOutputTokens` and `totalTokens`. A note that lowers any of the seven columns below the last adopted note is refused with the existing `advancing_regression` cause. Every adopted cumulative is then monotone in every column, so segment deltas telescope exactly.

This is a behaviour change, but only for malformed note sequences. Such a thread is now marked incomplete, so its model is `unreported` with proven tokens retained. Today such a note would be adopted with a dip. A test pins the change.

**ThreadAccount.** The readonly `model` becomes an ordered `segments` list. Each segment has:
- `model`;
- `start`, the cumulative where it begins (`undefined` for segment 0, meaning the thread baseline);
- its own `responses`;
- its own `pricingEvidenceComplete`.

The current segment is the last one. `registerThread` is unchanged: the first registration wins and creates segment 0.

**`beginModelSegment`:**

| Thread state | Effect |
|---|---|
| Unknown thread | No-op |
| Current segment model equals `model` | No-op |
| No adopted usage this claim (`maxTotal === undefined`), including a resumed thread still awaiting proved replay | Rebind the current segment's model in place. It has no responses. A later proved replay baselines as today and is never charged. |
| Adopted usage exists | Append a segment whose `start` is the current `maxTotal`. |
| `attributionQuarantined` thread | The cut or rebind is recorded, and the thread stays quarantined. Nothing is attributed. |

**`record`:**
- The adoption gate is unchanged apart from the two added columns.
- An adopted note's `last` joins the current segment's responses.
- `adopted.model` is the current segment's model, so ADR-2555 `usage_record` events carry the right model.
- A note already recorded before the cut has magnitude at or below the segment start and is never adopted into the new segment. A note delivered after the cut that advances the max is attributed to the new segment, so the caller must record all earlier-segment notes before cutting (#3253; D7).

**`aggregateByModel`, per segment:**
- Charged usage is `segmentEnd - segmentStart`. The end is the next segment's `start`, or `maxTotal` for the last segment. Segment 0 starts at the baseline. With the monotone gate these differences are non-negative in every column; the clamp at zero is only a defensive floor (D7).
- Reconciliation (`responsesReconcileDelta`) and pricing (`priceCodexResponse(segment.model, …)`) run on each segment's own responses against its own delta.
- Dominance per model is unchanged.
- A thread's incompleteness, from any cause, taints every model it has a segment on. That keeps unattributable deltas conservative.

**Invariants, each pinned by a test:**
1. Conservation: in every column, the sum of segment deltas equals `maxTotal - baseline`.
2. No usage is counted twice across a cut.
3. Replay is never charged, before or after a rebind.
4. Quarantine and incompleteness spread to every model the thread has a segment on.
5. `usage_record` carries the model per response.
6. A boundary-replay advance stays in the segment before the cut.

### Ownership, bounds and compatibility

The existing claim-shared accountant remains the sole adoption authority. Keep existing thread admission, numeric validation, deduplication and claim-lifetime disposal. Do not introduce global history, persistence, background work, provider calls or new dependencies. Segment state is claim-local; duplicate/equal-model cuts and unknown-thread calls allocate no segment. This child adds no production cuts, so ordinary runtime segment count remains one per registered thread. #3253 must bound calls to the first root-turn boundary per epoch and keep child registrations unchanged.

The caller must apply any historical replay to the previous segment before cutting. Synthetic accountant tests establish that contract here; production lifecycle ordering is tested in #3253. Existing malformed/incomplete evidence stays fail-closed; declared model names do not establish response pricing evidence.

### ADR-2555 and release note

Amend [ADR-2555](../../adr/2555-run-usage-attribution.md): root response attribution uses the root thread's current segment model, and cuts occur only after the caller has recorded the historical replay boundary. State that this child has no production cut caller yet and #3253 supplies that integration. Preserve child lane/identity rules and the distinction between attribution evidence and billing. Add a lowercase UUID-v4 changelog fragment for the malformed-sequence correction and segment-capable accountant.

## Testing decisions

- **Accountant** (`agent/test/codex-token-accounting.test.ts`, `codex-cost-accounting.test.ts`). One case per `beginModelSegment` row and per invariant:
  - fresh-thread cut;
  - resumed-without-baseline rebind, then proved replay;
  - quarantined cut;
  - duplicate, stale and older-than-cut notes;
  - A→B→A;
  - per-segment reconciliation, both success and failure on one segment only;
  - per-segment pricing;
  - incompleteness spread;
  - `adopted.model`;
  - the extended regression gate, a `reasoningOutputTokens` dip and a `totalTokens` dip, each refused;
  - conservation over randomized monotone sequences in all seven columns, with randomized cut points;
  - boundary replay assigned to the earlier segment.

- Add an explicit single-segment compatibility control covering totals, cost and adopted response/model events. Keep deterministic seeds and bounded sequence lengths in randomized conservation tests.
- Watch new regression tests red on the base for reasoning/total dips. For newly introduced segment behavior, use named mutants that restore per-thread reconciliation, leave `acct.model` in adoption, or omit each added regression column. A replay-before-cut mutant belongs to the accountant caller fixture here and the real harness integration in #3253. Record behavioral assertion failures, not setup or compilation failures; unchanged compatibility controls may pass on the base.

## Milestone

### M1. Conservative per-model segment accounting

- Implement the accountant interface and invariants above, its regression/compatibility/mutation tests, ADR-2555 amendment and changelog fragment together.
- In-run validation: `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent`, the two touched test files with the repository's Node test timeout and current concurrency limits, `task scan:secrets`, `task sast:semgrep`, and `task check-docs:web`. Run heavy checks serially. No real credentials or provider access are needed.
- Sweep `usage_record`, `advancing_regression` and affected accounting literals across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh` and `e2e/forge-fake/`; update affected contracts and run `task test:e2e-lib` and `task test:forge-fake`. Name affected full E2E phases left to CI.
- Acceptance: examples and invariants pass; watched red/green and named mutation results are recorded; the single-model compatibility control passes; no production caller cuts segments in this PR.

## Landing obligations (outside milestones)

- full agent suite: exact-head PR CI
- Applicable exact-head repository checks, security scans, docs and E2E checks pass before landing. No full `gate:*` command is an in-run acceptance requirement.
- Report measured affected test/job durations before and after and critical-path impact; distinguish queue/cache effects and say when CI timings are unavailable. Avoid new real-time waits or CI jobs.

## Decision Log

- **D1. Split approved by owner, 2026-11-21.** Extract #3253's safe-alone M1 into this child. #3253 is blocked on this merge and owns production cuts and switching. Rejected: delivering the billing accountant and the end-to-end planning feature in one large PR.
- **D2. Segment rather than replace accountancy.** Preserve first-registration identity, replay baselines and claim-shared deduplication. A new accountant per epoch loses those guarantees; mutating the sole model retrospectively misattributes prior work.
- **D3. Monotone seven-column admission rather than clamping segment deltas.** Refuse malformed dips and preserve proven tokens conservatively; clamping masks nonconservation.
- **D4. Reconciliation and pricing belong to each segment.** Thread-level reconciliation mixes work from different models and incorrectly degrades split usage. Quarantine/incompleteness still taints every affected model.
- **D5. Rebinding an incomplete thread keeps the replaced model as `unreported` (`taintModels`).** A quarantined thread has no cumulative total, so `beginModelSegment` can only rebind it, and the literal rebind rule would drop the replaced model from the output. `prds/done/3253-codex-planning-model.md` example 4 requires both models to be `unreported`, so a rebind on an already-incomplete thread records the replaced model in a deduplicated per-thread list that aggregation emits as a zero-token `unreported` entry. Healthy rebinds behave as the table says: the old model's key is absent.
- **D6. Multi-segment threads take uncached input from clamped-cumulative differences, with a boundary check.** Arose from the automated plan check. Summing raw input, cached and cache-write deltas per segment and then clamping can attribute tokens to a later segment that the unsplit thread never had when a cache split is malformed (`cachedInputTokens + cacheWriteInputTokens > inputTokens`); the transport and the fresh/child harness paths do not reject such splits. Segments of a thread with two or more segments therefore contribute `U(end) - U(start)`, where `U` is the clamped uncached cumulative the admission gate already keeps non-decreasing, so per-thread uncached input telescopes exactly. Single-segment threads keep the raw-delta path and byte-identical output. A malformed split at the baseline, a cut or a newly adopted total of a multi-segment thread marks that thread incomplete through the existing `invalid_raw_usage` bucket-relationship cause (never quarantine): every segment model is `unreported` with tokens retained, and the note is still adopted. The conservation claim is per thread: a multi-segment thread does not absorb a single-segment sibling's negative raw uncached delta, so a per-model sum can exceed today's shared-clamp value only when a sibling carries a malformed split, whose response is already unpriceable. Known asymmetry, unreachable until #3253 adds a production cut: incompleteness also sets the claim-wide `usageIncomplete`, which drops the terminal frame's base-usage claim and emits the claim-wide notice; the same malformed data on an uncut thread produces neither.
- **D7. Cut ordering is a caller contract; clamped segment difference restored as a floor; exact conservation is for safe integers.** The caller must record every usage note of the earlier segment (historical replay and live notes of earlier turns) before cutting; a note delivered after the cut that advances the running max is attributed to the new segment and its model. #3253 owns enforcing this (its PRD already requires that no late old-epoch note crosses the cut). Rejected: turn-binding of notes inside the accountant, and a protocol change; either can be taken up by #3253 if needed. Per-segment differences are clamped at zero again as a defensive floor against negative buckets from direct callers; for non-negative buckets under the monotone gate this is identical to the plain difference, and D3's no-clamping intent concerns masking non-conservation, which the gate prevents. Exact conservation is stated for non-negative safe-integer buckets.
