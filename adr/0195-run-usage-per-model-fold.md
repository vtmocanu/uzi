# ADR-195: Client and server fold the same wire field, by a mechanism rather than by assumption

**Status**: Accepted (implemented, issue #195)
**Date**: 2026-08-02
**Deciders**: team lead (investigation + design), coder (implementation), reviewer, auditor, tester (three independent validation passes, no blocking findings). Vlad approved the strict no-fallback rule and the recorded-fixture approach.
**Issue**: GitLab issue [vtmocanu/uzi#195](https://github.com/vtmocanu/uzi/issues/195) — there is no PRD. The issue carries the original measurement; this ADR carries the invariant, the mechanism that makes it hold, and the alternatives, because two of the alternatives are the first thing a competent reader reaches for and one of them is what the issue itself proposed.
**Numbering**: `0195` is an **issue** number, like `0106`. `0035`, `0042` and `0065` are PRD numbers. See ADR-106's numbering note.

## Decision (summary)

The run page's usage reduction reads the result frame's **`modelUsage`** map, per model,
never the frame's top-level `usage`. Per model, per column, it keeps a **running
high-water mark seeded at zero**, and the phase delta is the clamped difference against
that mark:

```
prev[model] = ZERO_MODEL                    // the seed is load-bearing, see below
delta       = max(0, cur - prev[model])     // per column, independently
prev[model] = mergeMax(prev[model], cur)    // column-wise max
```

Cost is quantized to microdollars **per model per frame**, mirroring where the server
applies it, before the running max.

The rule that generalises past this module, and the only sentence here worth memorising:

> **Two independent implementations folding the same wire field must be pinned by a
> fixture both of them read, not by a decision record asserting they agree.**

## Why the arithmetic works

The server's per-model value, composing the fold with the read:

- `UpsertRunUsage` merges with `GREATEST` per `(run_id, session_id, model)`, over values
  already clamped by `nonNegTokens`.
- `run_usage_totals` (migration `00063`) takes `MAX(...) GROUP BY run_id, model` in its
  inner query, then `SUM(...) GROUP BY run_id`. (PRD #632, migration `00176`, refined the
  inner grouping to `(run_id, model, lineage_epoch)`; single-lineage (epoch-0) output is
  unchanged, so this parity argument still holds for single-lineage runs but NOT for
  broken-lineage runs — see [ADR-632](0632-run-usage-lineage-epoch.md).)

So, **for a single-lineage run** (the case this derivation covers — see the note above;
for a broken-lineage run migration `00176` instead aggregates the per-epoch maxima and
SUMs them across epochs, so the server value is no longer a single `MAX` over all
frames), the server value is `MAX over ALL frames and sessions of max(0, v)`, then summed
across models. The client's fold telescopes to exactly that: with `mᵢ = max(mᵢ₋₁, vᵢ)`
and `m₀ = 0`, each term `max(0, vᵢ − mᵢ₋₁)` is identically `mᵢ − mᵢ₋₁`, so the sum is
`mₙ = max(0, max vᵢ)`. **The same expression, not an approximation of it.** Measured:
running-max disagreed with the server on 0 of 200,000 randomized sequences; last-seen
disagreed on 78,933.

**The zero seed is essential, and it is not the negative clamp.** `?? cur` makes every
model's first appearance difference against itself and contribute 0 — worth 326,462
tokens on the recorded fixture (2,162,533 → 1,836,071), and it fails `tsc` outright,
since the seed is the last reference to `ZERO_MODEL`. But the negative-clamp test stays
**green** under that mutation, which is the measurement that separates the two jobs:
`tokens()` floors every value at read, so a negative never reaches `prev`.

**Three places floor a negative, and they are NOT redundant — they guard different
surfaces.** This claim was stated wrongly several times before it was stated right, so it
is worth
stating precisely:

| guard | floors | so it protects |
|---|---|---|
| `tokens()` at read | the value | **both** surfaces, which is why removing it alone is unobservable |
| the `m₀ = 0` seed | the **mark**, via `mergeMax` | `modelTotals` (the per-model rows) |
| `clampDelta` | the **delta** | `total` and the phase rows |

Measured on a single frame carrying `inputTokens: -5`, all four variants:

```
baseline                        total.fresh 0    modelTotals.input  0
tokens() floor removed          total.fresh 0    modelTotals.input  0
?? cur only                     total.fresh 0    modelTotals.input  0
tokens() removed AND ?? cur     total.fresh 0    modelTotals.input -5   <- a negative lands
```

So no single-guard mutation reddens the negative-clamp test, and `total` never moves at
all — only `modelTotals` does, and only when `tokens()` and the seed are removed together.
Do **not** describe this as "guarded twice" (it drops the seed, which floors the mark
through `mergeMax`) and do **not** describe the three as "independent" or "redundant" (they
cover different surfaces, and `clampDelta` is additionally load-bearing for the high-water
identity itself, which is why removing its floor reddens 6 named tests rather than only
sign-related ones).

*Five statements of this were wrong before this one, and the coder's diagnosis of why is
the part worth keeping: **every wrong version asked HOW MANY — two, three, redundant,
independent — and the honest answer is WHICH.** The count was the defect. In order: the
module header's "three independent" (the failing word was "independent"); the auditor's
"the seed is not the negative clamp, `tokens()` is"; this ADR propagating that into
"guarded twice", dropping a real guard; the header's "TWICE, redundantly … the seed is NOT
one of them"; and the unscoped "no single-clamp mutation is observable", which is false
because removing `clampDelta`'s floor reddens 6 named tests.*

*One of those was not an error at all when it was written. The seed's flooring role is
observable **only on `modelTotals`** — a surface this very change introduced. So "the seed
is not a negative clamp" was TRUE of the only output that existed when the auditor first
wrote it, and became false when `modelTotals` landed. A claim outliving its context rather
than a claim that was ever wrong, which is the one shape in this list that no amount of
care at authoring time would have prevented.*

*Corrected 2026-08-02 from the auditor's four-variant matrix, reproduced independently by
the lead and the coder. It also exposed a coverage hole, now closed: no test caught the
double mutation, because the existing negative case uses two frames whose second is
positive, so the mark recovers either way. A lone negative frame is the only shape that
leaves the mark negative, and asserting `total` there pins nothing — `clampDelta` floors
it unconditionally. The new test asserts `modelTotals`.*

*This paragraph claimed "three mutually redundant clamps" including the seed until
2026-08-02, which is the very error the commit it documents was retiring. It was written
from a finding still in motion and corrected by the coder reading it.*

## Context: what was actually wrong

`agent/src/sdk-messages.ts` forwards both `usage` and `modelUsage` from the SDK result
frame, added in one commit (`9e8ae442`, PRD #40 M1). The server parsed only `modelUsage`;
the client read only top-level `usage`. PRD #40 Decision 3 asserted the two surfaces
"cannot diverge", resting on an M1 verdict flagged PROVISIONAL in its own text
(decompiled, no live run available). That verdict was taken at SDK `0.3.201`;
`agent/package.json` now pins `0.3.219`, eighteen patch versions on, and they did not
agree.

Measured on the cluster, run `84b6a933` seq 173:

| | input | output | cache_read | cache_write |
|---|---|---|---|---|
| top-level `usage` | 23 | 16,854 | 653,081 | 72,077 |
| Σ `modelUsage` | 5,259 | 41,763 | 2,134,247 | 183,504 |

Low on every column: 2.5x on output, 3.3x on cache_read, and **229x on input** — the
issue's own prose said "2.5x to 3.2x", which understates its worst column by two orders
of magnitude.

**The load-bearing discovery, which the issue did not have.** `modelUsage` is not a
model-stable map across frames. On that run `claude-haiku-4-5-20251001` appears in seq 173
and is **absent** from seq 2470, while the server retains it via `GREATEST`; `run_usage`
holds three models. This is not an artifact of one run: **17 runs in the live DB have a
model absent from their last result frame, 16 of them haiku.** All 48 result frames carry
both fields; none carries top-level `usage` alone.

## Alternatives considered and rejected

**A. Move the server onto top-level `usage`.** Rejected in the issue: `total_cost_usd`
already agrees with `modelUsage`, so the cost column and the token columns would then
disagree instead.

**B. Sum `modelUsage` per frame, then difference — the issue's own proposal.** Insufficient,
and this is the trap worth recording. It fixes the headline 2.5-3.3x and still disagrees
with the board: a per-frame sum telescopes to input **1,874** where the server has
**7,036**, losing exactly the vanished model. It is also *invisible to a fixture whose
models are stable across frames*, which is why the recorded fixture was chosen for its
vanishing entry.

**C. Keep `prev` as the last-seen value rather than a running max.** Overcounts every
non-monotonic run: `[5000, 1000, 3000]` gives 7000 where the server gives 5000. **Nothing
in the repo could see this** — measured by the tester at `cd68ed80` on 2026-08-02, when
the web suite held 1,595 tests (1,610 by the time this branch closed). The entire suite
passed identically
under both semantics, with the mutation applied and compiling. The first implementation
shipped this with a green gate and its own passing contract tests.

**D. Fall back to top-level `usage` when `modelUsage` is absent.** Rejected by Vlad on
2026-08-02: a fallback reintroduces exactly the divergence being fixed, in the case where
the board shows nothing and the run page shows numbers. Zero historical cost — no such
frame exists in the live DB.

## How the invariant is enforced

`fixtures/run-usage/` holds two recorded artifacts from a real run: the result frames, and
the `run_usage` rows the server folded from them. **Both sides read the same fixture with
their own production code** — `web/src/lib/runUsageContract.test.ts` and
`api/internal/workersvc/run_usage_contract_test.go` — and neither reads the other. Per
model, all four token columns plus cost, exact.

Two properties of that placement are deliberate:

- The fixture sits at the **repo root, above both Go module roots**, so `-count=1` is
  mandatory at both Go gates: a fixture edit changes nothing in any Go cache key.
  Reproduced on this contract (tester, independently at two SHAs; auditor confirmed the
  `cached=0` control on its own gate run): a gutted golden passes `go test` at `EXIT=0`
  with `(cached)`, and reddens only under `-count=1`. Both agents have since shut down, so
  this is recorded from their reports rather than re-derivable on request — the harness
  that produced it is a `git archive` extract plus a three-line gut of the rollup fixture,
  and it takes about a minute to rebuild.
- The fixture's discriminating power **is the vanished model specifically**. Demonstrated
  by counter-control (tester, run at both `cd68ed80` and `924c1cf1`; independently
  re-derived by the coder): re-add haiku to the second frame and the correct and naive
  implementations produce byte-identical pass/fail sets.

  **Scoped precisely, because a careless naive variant hides this.** It holds for a naive
  implementation that keeps per-model marks. One that folds the aggregate *into*
  `prevByModel` under a sentinel — arguably the more natural way to write it — is caught
  by the per-model assertion **independently of the vanished model**, because the sentinel
  pollutes `modelTotals`. So `modelTotals` gives the fixture a second, narrower catch, and
  the vanished model remains the only thing that catches the *clean* naive shape. The
  coder nearly reported this claim refuted on exactly that artifact, and found the cause
  before publishing.

## Consequences

**A post-reset phase row can render 0 tokens beside nonzero turns and duration.** The
server has no per-phase view, so parity cannot decide this; it is a deliberate choice that
the run total is right and the pathological path reads oddly. Total-correct wins because
the total is what every other surface shows.

**A frame whose `modelUsage` is empty produces no phase row at all**, and does not consume
an "Implement · iteration N" label. Server-consistent (the fold skips it wholesale) and
reachable — one such zero-work frame exists in the live DB. The same outcome has a second
trigger: a map whose only entry has an empty model id.

**Malformed input still diverges, by decision rather than omission, and the surviving set
is FIELD-level with one benign entry-level exception.** Every entry-level *shape* now
matches Go: a non-object entry poisons the frame on both sides, an array does too
(`typeof [] === "object"` made `rec()` accept one until an explicit `Array.isArray` guard
closed it), and `null` folds as an all-zero model on both sides because Go decodes it that
way.

**The exception is an empty model ID**, and it is an entry-level divergence kept
deliberately: a map whose only entry is keyed `""` makes Go fold the frame while writing
**no rows at all**, where the client skips the frame outright. The totals agree, because
neither side records anything; the only observable difference is that the client loses a
phase row, which is the same accepted outcome as an empty map. Uncoded on purpose.

What remains beyond that is Go's numeric strictness at the FIELD level: any field whose
JSON type Go cannot decode into its Go type — string, boolean, array, non-integer number,
out-of-range float — makes Go skip the whole frame while the client coerces to 0 and
folds. Both stay uncoded, and the two costs are **not** the same: `Number.isInteger` would catch
`1.5` in one call, as cheaply as the `Array.isArray` guard that closed the array row,
whereas int64/float64 **range** has no cheap JS equivalent at all — JS loses integer
precision above 2^53 and cannot distinguish an in-range int64 from an out-of-range one
near the boundary. The range argument is the strong one and covers `1e30`/`1e400`; it does
not cover `1.5`, which is a decision rather than a consequence.

**One of PRD #40's deferred residuals is closed; the other is not.** #40 recorded, as a
rider inside its **M6** bullet (not as an M4/M5 milestone), that the run-view strip total
should be checked against the runs-list total, and that it had never been performed for
want of credentials. The contract test is that check, now executable. **Still open:** #40's
M1/M2 live requeue-boundary check — a two-phase run crossing `docker compose down && up`,
reconciled against the sum of both legs. That needs a real requeue, and it is precisely
what Decision 3's divergence caveat was about. Do not read this ADR as discharging #40.

**`RunUsage.cacheHitRatio` is kept although no production code reads it**, and it is not
unguarded: `runUsage.test.ts` asserts it to 6 decimal places, so deleting it reddens a
test rather than merely contradicting a docstring. It is the unrounded truth, and the only
figure safe to do further arithmetic on — `cacheDisplayPct` is clamped into [1,99] for
display and must never be used as a value. PRD #194 M1 will want the exact one. Recorded
here because when PRD #103 M4 lands `knip`, an export whose only readers are tests is
precisely what gets flagged, and the next reader needs the reason in under a minute.

**PRD #194 M3 is unblocked.** It adds a cost column to this panel and was blocked because
adding a third population would have put two mislabelled ones beside it.

## What this does not cover

The four surfaces the issue names — header strip, cache-hit bar, per-phase table,
activity-feed finish lines — are pinned at the module level only. **The mock fixtures emit
`usage` and `modelUsage` with identical numbers**, so mock mode cannot surface this class
of bug and a browser pass against it validates rendering, not population. Making one mock
run diverge the way real frames do would close that.

`UpsertRunUsage`'s real SQL `GREATEST` is reimplemented in the Go contract test rather than
executed; real-Postgres behaviour is covered by the `*LiveDB` suite, which this work did
not run.
