# run-usage: the run page / run_usage cross-language contract (issue #195)

Two readers of the same result frames disagreed for the whole of PRD #40's life, and
nothing in the repo could have noticed:

| | reads | file |
|---|---|---|
| client (run page) | the frame's **top-level `usage`** | `web/src/lib/runUsage.ts` |
| server (`run_usage`, board, `uzi run list`, `/api/usage`, `/api/admin/usage`) | the frame's **`modelUsage`** | `foldRunUsage` in `api/internal/workersvc/service.go` |

`mapResult` (`agent/src/sdk-messages.ts`) forwards **both**, unguarded, and on the SDK
pin this repo ships against they are different populations. PRD #40 Decision 3 asserted
the two "cannot diverge"; its verification (M4/M5) was deferred for want of credentials
(`prds/done/40-token-usage-reporting.md:84`). This fixture is that deferred check.

## The two artifacts

```
fixtures/run-usage/result-frames-84b6a933.json   both real result frames of one run
fixtures/run-usage/run-usage-84b6a933.json       the run_usage rows the server folded from them
fixtures/run-usage/result-frames-02854d5e.json   four init + four result frames of one run (PRD #1079)
fixtures/run-usage/run-usage-02854d5e.json       the per-leg rows an independent jq reduction folds from them
fixtures/run-usage/README.md                     this file
```

The 84b6a933 pair pins the vanished-model property (below); the 02854d5e pair pins a
different, independent bug and is documented in its own section, ["The `02854d5e`
pair"](#the-02854d5e-pair-per-leg-sum-vs-cumulative-max), further down.

Repo root, **owned by neither runtime** — same placement and the same reason as
`fixtures/judge-fidelity/`. Not `api/internal/workersvc/testdata/`, which is where a
`go test -update` flag gets added; not `web/src/lib/`, which is where a
`toMatchSnapshot()` gets added.

Two readers, and **neither generates either file**:

| | |
|---|---|
| Go | `api/internal/workersvc/run_usage_contract_test.go` (relative path; `go:embed` cannot escape the `api/` module) |
| vitest | `web/src/lib/runUsageContract.test.ts` (plain `fs`, resolved against `import.meta.url`) |

**Each runtime folds the frames with its OWN production code and compares against the
SAME recorded rollup.** Never Go against JS directly: a direct diff can report only
*that* they disagree, never *which one drifted*, and it would make `npm test` depend on
a Go toolchain.

A missing or unreadable fixture is a **fatal**, never a skip, on both sides.

### 🔴 The two halves are NOT symmetric, and the Go half needs `-count=1`

This directory sits above `api/`, so every byte of it is outside that module and
contributes **nothing** to `internal/workersvc`'s cache key — cmd/go's own rule is *"Do
not recheck files outside the module, GOPATH, or GOROOT root"*. A fixture-only edit
therefore leaves `go test` printing `ok (cached)` over a gutted fixture. The vitest half
has no such cache and needs no flag. So "Go red plus vitest green means Go drifted" has
a third explanation: *Go never ran.*

```sh
cd api && go test -count=1 ./internal/workersvc/
cd web && npx vitest run src/lib/runUsageContract.test.ts
```

## 🔴 This fixture is RECORDED, not authored — do not regenerate it

Both files were read out of the dev-cluster database on 2026-08-02. `result-frames-…json`
holds two genuine result frames of run `84b6a933-ceea-4f6e-9f29-75143555ec0f`, verbatim
payloads; `run-usage-…json` holds what the shipped server actually folded from them
(`session_id` redacted, everything else as stored).

That is the opposite of `judge-fidelity/`, which is hand-authored *because* a golden
derived from the implementation locks in its blind spots. Here the third artifact is not
a hand-computed expectation at all — it is **the production server's own output**, which
is exactly what the client has to agree with. Recomputing it from either reader would
turn the contract into a tautology.

There is deliberately no `-update` flag and no `toMatchSnapshot()`. If a test here goes
red, one of the two readers changed; fix the reader.

## What makes it discriminate

The naive reading of the issue — "sum `modelUsage` per result frame instead of reading
top-level `usage`" — closes the headline gap and **still disagrees with the server**,
which is why both test files carry a self-check over the fixture's own shape.

`modelUsage` is not a model-stable map across frames. A model present in an earlier
frame can be **absent** from a later one, while the server retains it: `UpsertRunUsage`
merges with `GREATEST` per `(run_id, session_id, model)`, and
`00063_run_usage_totals_view.sql` then takes `MAX` over that model's sessions before
summing across models. Per column, the server's run total is therefore

    Σ over models of  MAX over ALL frames of  nonNegTokens(value)

```
seq  173 :: claude-haiku-4-5-20251001  in=5162  out=16      cr=0         cw=0
seq  173 :: claude-opus-5              in=97    out=41747   cr=2134247   cw=183504
seq 2470 :: claude-opus-5              in=1770  out=592895  cr=78119788  cw=2017902
seq 2470 :: claude-sonnet-5            in=104   out=28108   cr=2529269   cw=137595
```

haiku is gone at seq 2470; `run_usage` holds **three** rows. So:

| | input | output |
|---|---|---|
| naive per-frame sum, telescoped | 1,874 | 621,003 |
| server MAX-per-model-then-SUM | **7,036** | **621,019** |

low by exactly the vanished model. `total_cost_usd` has the identical hole: seq 2470
carries `69.1557442` where the rollup sums to `69.160986` — a `0.005242` gap that is
haiku's cost exactly, which is why nobody caught this by reconciling cost.

The shape is not a one-run artifact: **17 live runs** have a model present in an earlier
result frame and absent from that run's last one; 16 of the 17 are haiku. (Measured on
dev-cluster on 2026-08-02 by read-only SQL, over the 48 result frames the DB then held;
a first pass at that query reported "20 across 15" by numbering `(frame, model)` pairs
rather than frames. Re-derive the count if you need it — the SHAPE is what this fixture
pins, and it is pinned by the fixture's own self-check rather than by that number.)

The self-checks (`TestRunUsageFixtureDiscriminates`, `describe("run-usage fixture
discriminates")`) assert all of that over the fixture itself, so a "tidied" fixture
fatals instead of quietly agreeing with the bug.

### 🔴 The `cached` column is BLIND on this fixture

haiku — the model that vanishes, and the only source of disagreement here — carried
`cacheReadInputTokens: 0` **and** `cacheCreationInputTokens: 0`. So the naive per-frame
sum, the correct per-model fold and the server rollup all answer 80,649,057 on
`cache_read`. Measured, naive minus server:

| field | delta | verdict |
|---|---|---|
| `fresh` (input + cache_creation) | −5,162 | discriminating (haiku's `inputTokens`) |
| `cached` (cache_read) | **0** | **BLIND** |
| `out` | −16 | discriminating |
| `cost` | −0.0052418 | discriminating (haiku's `costUSD` exactly) |

A control that comes back with `cached` green and the rest red is a **working** control,
not partial coverage. Never cite a passing `cache_read` here as evidence of anything.

One more thing the cost column does NOT do: it does not separate the status quo from the
naive fix. Frame 2's `total_cost_usd` happens to equal its own Σ `modelUsage` `costUSD`,
so both read 69.1557442. Cost separates {status quo, naive} from {correct}; three other
discriminators do the rest.

### Detection power, MEASURED before landing

Six controls, all against the fixed tree, each typechecked before its result was read and
each restored by copy-aside (never `git checkout --`, which reverts to HEAD and would
have wiped the uncommitted fix). Restores were verified by `shasum`, not by grep.

| control | how it was obtained | result |
|---|---|---|
| naive per-frame `modelUsage` sum (the issue's own suggested fix) | hand-written fold | contract **RED** ×2: `expected [ '__all__' ] to deeply equal [ 'claude-haiku-4-5-20251001', …(2) ]`, and `input+cache_creation disagrees with run_usage: expected 2157371 to be 2162533` — short by 5,162, haiku's input exactly |
| the real pre-fix reader (top-level `usage`) | `git show e0472a88:web/src/lib/runUsage.ts` — retrieved, not reconstructed | contract **RED** ×3: `expected 255068 to be 2162533`, the named "does not read the frames' top-level usage" assertion, and the per-model test crashing on the absent field |
| running MAX → last-seen value | one-line fold on `prevByModel.set` | 4 unit tests **RED**: `expected 7000 to be 5000`, `expected [5000, 0, 2000] to deeply equal [5000, 0, 0]`, `expected 170 to be 150`, `expected { input: 50 } to match { input: 100, cacheCreation: 200 }` |
| per-column max → one combined `fresh` column | fold on the delta accumulation | 1 unit test **RED**, exactly the one named for it: `expected 250 to be 300` |
| microdollar quantization removed | fold on `quantizeCost`'s return | 2 **RED**: `claude-opus-5 cost_usd: expected 67.45905024999998 to be 67.45905` (contract, per model) and the unit test |
| `Map` accumulator → plain `{}` | shim class behind the same surface | 1 unit test **RED**: `expected [ 'claude-opus-5', 'constructor' ] to deeply equal [ '__proto__', 'claude-opus-5', …(1) ]`. A direct probe on the mutated build measured the second failure mode too: `total.fresh = NaN`, `out = NaN` |
| a case deleted from the fixture (haiku removed from seq 173) | edit the JSON | bare `go test` → `ok (cached)`; `go test -count=1` → **RED** ×2; vitest → **RED** ×2, no flag needed |

The first control is the one that matters most. A per-frame sum passes any fixture whose
models are stable across frames, which is most of them — that is precisely why this bug
survived review, and why a fixture that cannot fail against it is worthless.

**The last two rows are the ones to read next.** Before the running-max and quantization
tests existed, the tester measured the whole 1,595-test web suite as green under BOTH
semantics — the mutation applied textually and compiled, and nothing anywhere in the repo
could tell them apart. A fixture whose totals happen to be monotonic cannot see that
difference, which is why those pins are synthetic unit tests rather than fixture cases.

## Do not tidy

- **Both frames must stay.** With one frame every implementation agrees, and nothing
  here could tell a cumulative reader from a summing one.
- **haiku must stay absent from seq 2470.** It is the only thing separating the correct
  fold from the naive one. `TestRunUsageFixtureDiscriminates` fatals if it returns.
- **The top-level `usage` blocks must stay, and must stay wrong.** They are the field
  the run page used to read; removing them, or "correcting" them into agreement with
  `modelUsage`, deletes the ability to tell the two readings apart.
- **`run-usage-…json`'s costs are the DB's `numeric(12,6)` values** (`67.459050`, not
  `67.45905024999998`). The client mirrors `numericUSD`'s microdollar quantization at the
  point the server applies it, so the PER-MODEL costs are compared exactly and only their
  sum carries a tolerance. Do not "restore precision" from the frames, and do not relax
  the per-model assertions to `toBeCloseTo` — that would hide a drift in the
  quantization itself.
- **Do not widen the total-level cost tolerance to `toBeCloseTo(_, 6)`.** Its threshold
  is a flat 5e-7, and the quantization bound is half a microdollar PER ROW — 1.5e-6 at
  three models. It passes here only because the three roundings went a favourable
  direction, and would redden a CORRECT implementation on a run with more models.

## What this fixture CANNOT catch

- **Anything the run page does with the numbers.** It pins `deriveRunUsage`'s totals,
  not the strip, the per-phase table, the cache-hit bar or the finish lines.
- **The per-agent table.** That path reads assistant frames' per-call `usage`, a third
  population this fixture contains no frames for.
- **The wire.** Both halves start from a recorded payload; that `mapResult` still emits
  this shape is pinned by `agent/`'s own tests, not here.
- **A frame with top-level `usage` and no `modelUsage`.** None existed in the live DB
  when this was recorded (48/48 carried both, dev-cluster, 2026-08-02), so the
  skip-not-fallback decision is covered by a unit test in `web/src/lib/runUsage.test.ts`
  rather than by recorded data.
- **The `GREATEST` merge itself.** The Go half reimplements it over the captured
  upserts; the SQL is pinned by the `*LiveDB` suite.
- **Anything non-monotonic.** Both recorded frames are monotonic per model, so the
  running-max fold and a last-seen fold produce identical numbers here. The distinction
  is pinned by synthetic unit tests in `web/src/lib/runUsage.test.ts`, not by this
  fixture — do not read a green contract as evidence about it.
- **Hostile model ids.** `__proto__` / `constructor` keys and the 200-code-point cap are
  likewise unit-tested, not represented here. The recorded run used three ordinary ids.
- **`+Infinity` in `costUSD`.** The client's `num` drops every non-finite value to 0
  where `numericUSD` clamps `+Inf` to the column ceiling. Documented at `quantizeCost`
  rather than coded, since no SDK emits it.

## The `02854d5e` pair: per-leg SUM vs cumulative MAX (issue #1079)

`result-frames-02854d5e.json` / `run-usage-02854d5e.json` is a second, independent
contract pair, added for PRD #1079. It pins a **different** bug from the 84b6a933 pair
above: not which field each reader consumes, but whether a result frame's `modelUsage`
is cumulative over the resumed session (the assumption `run_usage` shipped with) or
reports only its own SDK `query()` call (what the Agent SDK's own docs say, and what
this fixture proves). Both readers must now fold **per leg and SUM the legs**, where a
leg is one `query()` call marked by its own persisted `init` frame.

### What it pins

`result-frames-02854d5e.json` holds all four `init` + four `result` frames of run
`02854d5e` (plan turn + three implement iterations), verbatim, in seq order.
`run-usage-02854d5e.json` holds six rows, one per `(model, lineage_epoch)` — haiku@1,
opus@1..4, sonnet@4 — and the run totals: input 12785, cache_read 187880173,
cache_creation 5500712, output 1021240, cost_usd 153.582776.

The discriminator: the pre-fix, MAX-collapsed reading of these same eight frames
answers **77.185539** USD / **514572** output tokens — the exact numbers the shipped
(buggy) server stored for this run before the fix. A fold that regresses to
cumulative-MAX reproduces that pair exactly, so both contract test files assert the
correct totals **and** assert that a MAX-per-model fold of the same frames lands on
77.185539 / 514572, not merely that the correct numbers differ from *something*.

### Why its rollup is AUTHORED, not recorded (D9)

The 84b6a933 pair above is recorded from the live database on both sides — frames and
rollup — because a golden derived from an implementation locks in that implementation's
blind spots, and 84b6a933 exists precisely to catch a blind spot neither implementation
had noticed yet. That reasoning does not apply here: for 02854d5e, **the shipped
server's own collapsed output (77.185539 / 514572) is the defect being fixed**, so it
cannot be the golden — recording "what the server answered" would pin the bug. Instead:

- `result-frames-02854d5e.json`'s frames are verbatim, recorded from the dev-cluster run
  (same as 84b6a933's frames).
- `run-usage-02854d5e.json`'s `rows` and `totals` are **authored** — an independent jq
  reduction over the frames (one row per `(model, lineage_epoch)`, tokens floored at 0,
  cost quantized to microdollars exactly as `numericUSD` does), not read back off either
  production fold. The file's own `note` field documents the reduction rule.

Both production folds (`foldRunUsage` in Go, `deriveRunUsage` in TS) must reproduce this
authored rollup from the frames; if either doesn't, one of the two folds has drifted
from the specified per-leg rule, not from each other.

### Do not tidy

- **All four legs must stay.** Three legs (or fewer) can't rule out "the last frame is
  cumulative and the fix only needed a bigger buffer" — four legs, with a non-monotonic
  transition between two of them (below), are what make the per-leg reading provable
  from the fixture's own shape.
- **Leg 4 must stay strictly SMALLER than leg 3 for opus, in every column.** That
  inversion (input 438 < 1285, output 158427 < 488268, cache_read 25620187 < 96109126,
  cache_creation 1285948 < 2223283, cost 25.955935 < 75.029070) is what makes a
  cumulative reading of these frames structurally impossible — a running total cannot
  shrink absent a break event, and none is present between legs 3 and 4. Rounding leg 4
  up, dropping it, or reordering the frames destroys that proof.
- **`run-usage-02854d5e.json`'s costs are the DB's `numeric(12,6)` microdollar
  quantization** (`75.02907`, not a longer float), mirroring the 84b6a933 pair's
  precision rule above — do not "restore precision" from the frames.

### The Go half needs `-count=1`

Same rule as the 84b6a933 pair, for the same reason (this directory is outside the `api`
Go module, so a fixture-only edit is invisible to `go test`'s cache key):

```sh
cd api && go test -count=1 ./internal/workersvc/
cd web && npx vitest run src/lib/runUsageContract.test.ts
```
