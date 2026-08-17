# PRD #194 — Live cost estimate on the run page (cost is frozen between SDK phases)

**Issue**: [#194](https://github.com/vtmocanu/uzi/-/issues/194) · **Label**: PRD · **Priority**: Medium
**Area**: `web/src/lib/runUsage.ts` + `web/src/components/RunUsage.tsx` + `web/src/pages/RunView.tsx` (client-derived, PRD #40 lineage) + a new price table module + one nullable `bills_usage` column on the Anthropic token row (M2, open question 1).
**Line references** are against `28d7238e`.
**Status**: **M0 is a go/no-go gate**, and it reduces to one question: can
per-call `output_tokens` be made whole? With them the estimate lands within 3.9%
of billed; without them the cost is −33% low *and* the panel contradicts itself
in public (estimated tokens-out for the whole run reads lower than confirmed
tokens-out for one phase). Both [open questions](#open-questions) are now
answered: the headline estimate is approved, and uzi cannot distinguish a
subscription token today, so M2 gains one storage change to remember it.
~~**M3 is blocked on [#195](https://github.com/vtmocanu/uzi/-/issues/195).**~~
**UNBLOCKED 2026-08-02**: #195 landed. The strip and the per-phase table now fold
`modelUsage` per model, pinned to the server's `run_usage` rollup from both sides by a
recorded fixture (`fixtures/run-usage/`, `adr/0195-run-usage-per-model-fold.md`). The
three-populations problem below is now a two-populations problem, and the two that remain
are the ones M3 is designed to label.

## Problem

Cost is written exactly once per SDK query. `UpsertRunUsage`
(`api/internal/store/queries/runtime.sql:1223`) folds usage from a **delivered
result frame**, and the client's own reduction takes `costUsd` from
`payload.total_cost_usd` on a result frame only (`web/src/lib/runUsage.ts:187`).
A result frame arrives when a phase ends: once for planning, once per
implement/review iteration.

For a multi-agent PRD run that means the cost figure is stale for as long as a
phase lasts. Measured on run `84b6a933` (PRD #119) on 2026-07-29:

| Time | Event |
|---|---|
| 09:01:27Z | run starts |
| 09:11:04Z | planning result frame, `total_cost_usd = 3.533714` |
| 10:33:19Z | still running, still reporting **$3.53** |

There is exactly **one** result frame in the whole 86-minute log, so the
staleness window is open-ended rather than 82 minutes. The live-derived estimate
at 10:17:27Z was **$31.51** across 598 usage-carrying frames (484 distinct API
calls — see M0 on why those differ). That is not a rounding difference: it is the
difference between "this run was cheap" and "this run cost thirty dollars", and
the page cannot express it.

The whole panel is additionally gated on a result frame having arrived at all:
`hasUsage: phases.length > 0` (`runUsage.ts:243`), consumed at
`RunUsage.tsx:110` **and at `RunView.tsx:683`**. So a run that has not finished
its first phase shows **no usage surface whatsoever**, even though token counts
for every call so far are already on the wire.

### The data is already there

Every assistant frame carries per-call usage, attached by the worker
**exactly once per SDK frame**, executor-side, behind the `usageAttached` latch
at `agent/src/sdk-executor.ts:1385-1397` (the docblock at
`agent/src/sdk-messages.ts:315-317` states why); the per-call model rides the
same latch (`:323` `assistantUsageOf`, `:351` `assistantModelOf`).
On run `84b6a933`, 598 frames carried it (575 Opus 5, 23 Sonnet 5) — 484 of them
distinct, because the SDK emits several frames per API call and each repeats that
call's usage. M0 covers the consequences.
`runUsage.ts` already sums those per agent and renders them in the per-agent
table, live. What it does not carry is **cost**, because per-call usage has no
cost field.

So the missing piece is arithmetic, not data collection.

## Solution

Price the per-call usage the client already sums, against a static rate table,
and render it as an **estimate** that is visually and semantically distinct from
the **confirmed** billed figure. Never let the estimate overwrite or stand in for
`total_cost_usd`.

Design mock (built from the real measurements above):
<https://claude.ai/code/artifact/ea356b26-6679-435d-9318-76f3a70dae1e>
Repo copy: `prds/mockups/194-live-cost-mock.html`.

### What this supersedes in PRD #40

Two of #40's decisions are reversed here, deliberately and by name, because an
unnamed reversal is how a future reader concludes one of the two documents is
wrong:

- **#40 Decision 8, "Tokens are the headline, cost is secondary."** This PRD
  makes an estimated cost the lead figure on the run page. The reversal is
  conditional on [open question 1](#open-questions): if uzi cannot distinguish a
  billed token from a subscription one, Decision 8 stands and this PRD reverts to
  a secondary cost line.
- **#40 Decision 11 caveat (b), "tokens only, no per-agent cost."** That caveat
  reasoned from "per-call usage has no cost field", which is true and does not
  imply cost cannot be *derived*. `web/src/components/RunUsage.tsx:262-264`
  carries the resulting user-facing footnote ("tokens only — per-agent cost is
  not available"), which M3 must retire.

**#40 Decision 11 caveat (a) is NOT superseded and is load-bearing here.** It
states that per-agent figures are *attribution* and "must not be forced to
reconcile with the usage strip", and it names four divergence mechanisms:
signal-only frames whose every mapped message is filtered; frames with only
unmapped blocks; compaction/summarization API calls; and refusal-fallback
`supersedes` replacements where both frames persist with usage. The first two
undercount, the last overcounts. **This PRD proposes putting a dollar sign on
exactly that non-reconciling sum**, which is why M0 exists and why it can kill
the PRD.

### Why a static table: there is no pricing endpoint

Verified against the live API reference on 2026-07-29. `GET /v1/models` returns
`id`, `type`, `display_name`, `created_at`, `max_input_tokens`, `max_tokens`, and
a `capabilities` tree. **No price field**, and no separate pricing route. Prices
are published as documentation only. A hand-maintained table is not a shortcut
here, it is the only option, and the PRD treats its staleness as a first-class
risk rather than an afterthought.

Prior art per the inspiration-first convention:
`inspiration/multica/packages/views/runtimes/utils.ts:158` (`MODEL_PRICING`),
four rates per model (`input`/`output`/`cacheRead`/`cacheWrite`), with
`resolvePricing` at `:298`. It is **not** stale in the way first assumed — it has
`claude-sonnet-5` and `claude-fable-5` rows; what it lacks is a `claude-opus-5`
row, i.e. the model this run actually used. Its resolver already does
exact-match-then-strip-trailing-date and explicitly refuses `startsWith`
(`:143-147`), which is the resolution rule proposed below **verbatim** — so that
is adopted prior art, not an improvement. The two genuine deltas are the fifth
rate and effective dating, and multica's own comment at `:164-166` flags the
missing future-dating as a known gap. Its single `cacheWrite` is the 5m rate,
which is the concrete precedent for the 7.7% error described below.

### The arithmetic is exact, and that was measured

Applying the published rates to the SDK's own token counts reproduces the SDK's
own cost to floating point. On the planning-phase result frame of `84b6a933`:

| Leg | Priced by hand | SDK `modelUsage.costUSD` |
|---|---|---|
| `claude-opus-5` | 3.528472250 | 3.528472249999999 |
| `claude-haiku-4-5` | 0.005242 | 0.005242 |
| Sum | 3.533714250 | `total_cost_usd` 3.533714249999999 |

Delta 8.9e-16. **The pricing formula is not the risk.** The risk lives entirely
in which token counts the live path sees, which is what M0 exists to settle.

**The attribution rule that made this work, recorded because the test cannot be
written without it.** `ModelUsage` carries `cacheCreationInputTokens` but **no
5m/1h split** — the split exists only on the frame's top-level `usage`. On seq
173 those two do not even agree: `modelUsage["claude-opus-5"]
.cacheCreationInputTokens` is 183,504, while the frame's
`usage.cache_creation` is `{1h: 72,077, 5m: 0}` with
`cache_creation_input_tokens: 72,077`. The reconciliation closes only by taking
**72,077 as the 1h portion and 183,504 − 72,077 = 111,427 as 5m** — and 111,427
appears nowhere on the wire, while the frame literally reports zero 5m writes.
A test author cannot derive that; it has to be written down, and it is only
unambiguous here because Haiku's `cacheCreationInputTokens` is 0. **There is no
general rule**, so M1's reconciliation fixture must be single-model, and the
rate-difference assertion belongs in M2's mixed fixture instead.

### Five rates per model, not two, and a residual bucket

Reaching for `{input, output}` produces a wrong number. The wire carries
`cache_creation.ephemeral_1h_input_tokens` and `ephemeral_5m_input_tokens`
separately, and they are priced differently (1.25x base for 5m, 2x for 1h; cache
reads are 0.1x, which is what makes Opus 5's $0.50 read rate consistent). On the
planning phase of `84b6a933`, 72,077 of 183,504 cache-write tokens were 1h
writes; pricing them all at the 5m rate gives $3.258183 against $3.528472, an
**underprice of 7.66%**. Both categories are genuinely populated on the live
path, not merely present in the types: across the run, 717 frames carry a nonzero
5m figure and 67 a nonzero 1h figure.

The table needs, per model: `input`, `output`, `cacheRead`, `cacheWrite5m`,
`cacheWrite1h`.

**The reader must partition, not sum two named fields.** In the SDK types
`cache_creation` is `BetaCacheCreation | null` while
`cache_creation_input_tokens` is the authoritative total, so
`cacheWrite = 5m + 1h` prices cache writes at **zero** whenever the object is
null, silently and in the underprice direction. The same shape would miss a
future third TTL tier. So: `cacheWriteOther = cache_creation_input_tokens −
(5m + 1h)`, priced at the 5m rate, plus a differential test (below).
*Measured caveat, so nobody over-weights this: across all 598 frames of
`84b6a933`, `cache_creation` was never null while the aggregate was nonzero, and
`5m + 1h == cache_creation_input_tokens` exactly, residual 0. This is a
type-level hazard, not an observed defect.*

**`readUsage` (`runUsage.ts:100`) cannot feed this as written.** It collapses
usage into `{fresh, cached, out}`, where `fresh = input_tokens +
cache_creation_input_tokens` — it merges two differently-priced categories and
discards the 5m/1h split entirely. Pricing needs a parallel reader that keeps
five buckets plus the residual. The existing three-bucket shape stays for the
token tables, which are correct as they are.

### Approach chosen (and rejected)

**Chosen: client-derived, in `runUsage.ts`.** This follows PRD #40 Decision 5/11
verbatim — the run view already builds its usage surfaces from the message
stream rather than from `run_usage`, precisely because a pure reduction over the
seq-deduped message list is safe against the ws→reconnect→REST-replay overlap
that an incremental accumulator would double-count.

**The strongest argument for the client seam is mutability, and it applies
against every alternative below.** A price computed on the client is recomputed
on every page load from the currently-deployed bundle, so a wrong rate is fixed
by a web deploy and every historical run is corrected retroactively. A price
computed anywhere upstream is frozen at the moment it was written, by whatever
table shipped in that build, and can never be corrected for runs already
recorded.

**Rejected: the worker prices tokens and emits a cost.** It plainly could. But
the result is immutable in `run_messages`, per the paragraph above, and it would
require a new agent image to fix a rate.

**Rejected: an ingest-time fold into `run_usage`.** This is the serious
alternative — the API already sees every message through `AppendMessages`, so
folding assistant-frame tokens into new columns at write time is O(1) per
message and would light up the board, `uzi run list` and admin usage for free.
Rejected because `foldRunUsage` is deliberately called with **all delivered**
messages rather than newly-inserted ones (PRD #40 Decision 2: the message insert
and the usage write are separate statements with no transaction seam, so folding
only new inserts would skip the fold forever on a crash-retry). That makes the
existing fold idempotent by construction, and an *additive* accumulator over the
same delivery is not — it would need a new exactly-once ingest path, a change to
a load-bearing contract. Plus a second price table in Go, plus immutability.

**Rejected: a read-time server scan**, which would re-read a run's whole message
history per request. Named only to be dismissed; the ingest-time fold above is
the version worth arguing with.

### The panel already shows three populations, and that shapes the design

Captured from the live run page at 10:57Z on run `84b6a933`, all three visible in
one screen at one moment:

| Surface in the panel | Tokens in | Reads |
|---|---|---|
| Stat strip + per-phase table | ~~**725.2k**~~ → **2.32M** | ~~result frame's top-level `usage`~~ → `modelUsage`, per model |
| Board / `uzi run list` / admin (not in the panel) | **2.32M** | `modelUsage` |
| Per-agent breakdown | **78.15M** | raw assistant frames, whole run |

*Captured at 10:57Z on run `84b6a933`, before #195. The first row is struck because
#195 landed on 2026-08-02: the strip and the table now read the same population as the
board and agree with it exactly, per model, on all four token columns plus cost. The
measurement is left in place rather than deleted, because the design consequence below
was derived from it.*

The gap was 108x between smallest and largest, and closing #195 removes the strip-vs-board
half of it. **What remains is the per-agent inflation** — the frame-vs-call duplication M0
identified. The existing footnote ("may not sum to the run total") attributed all of it to
attribution, which was only partly true then and is now the whole story for the one row
still divergent.

**The design consequence: each dollar figure owns a table.** Rather than one
blended total, the panel pairs each figure with the breakdown that derives it —
**Confirmed** with the per-phase table (result frames), **Estimate** with the
per-agent table (assistant frames). The gap between them is then legible as work
in flight since the last phase boundary, rather than as an unexplained
discrepancy. Priced against live data that is $3.53 confirmed beside $45.11
estimated, which is coherent when labelled and absurd when not.

**Sequencing follows from this**, and it is why M3 is blocked: land #195 so the
top of the panel stops disagreeing with the board, then dedupe so the per-agent
rows count calls rather than frames, then add the cost column. Adding money
first would ship a $45 next to a $3.53 with a footnote between them. A token
discrepancy reads as attribution noise, which is how that footnote survived; a
dollar discrepancy reads as a billing error, and no footnote absorbs one.

## User journey

Today, on a run past its first phase: the user sees one dollar figure with no
indication of age, and it does not move. On a run before its first phase: no
usage surface at all.

After:

1. A run starts. Within the first few model calls the Cost panel appears with a
   live estimate, marked as an estimate.
2. The figure climbs as the run streams. Per-model and per-agent breakdowns climb
   with it, so a runaway subagent is visible while it is running rather than in
   hindsight.
3. A phase ends. A **Confirmed** line appears with the billed total and the phase
   it covers, e.g. "billed total through Plan, reported 09:11:04Z". The estimate
   keeps climbing above it.
4. The run finishes. Confirmed catches up to the final total; the estimate has
   served its purpose and the confirmed figure is what the user quotes.

At no point does the user have to know which of the two numbers is authoritative:
the confirmed one says so, and the estimate carries a tilde and a band.

## Open questions

Both answered 2026-07-29. Recorded with their resolutions rather than deleted,
because the reasoning constrains M2 and M3.

### 1. Can uzi distinguish a billed token from a subscription one? **No, by design.**

Investigated 2026-07-29. Three independent confirmations that uzi does not, and
deliberately does not:

- `api/internal/handler/secrets.go:766` — `validateAnthropicToken`'s own comment:
  it "makes no assumption about the token's prefix or format (**Anthropic
  prefixes are not a documented contract**)". It checks non-empty, length bound,
  no whitespace or control characters. Nothing else.
- `docs/anthropic-token.md:27` says it to users: *"Both paste into the same
  field; uzi doesn't check for a particular prefix, so either kind is accepted."*
- `user_secrets.kind` is `CHECK (kind IN ('anthropic_token'))` (migration
  `00010`) — one kind. The label (`subscription`, `console-key`) is user-chosen
  free text, suggested in the docs, never read by uzi.

And this is not an edge case: `docs/anthropic-token.md:23` calls the
subscription OAuth token the **recommended** option, and both `docs/judge.md:30`
and `web/src/pages/Settings.tsx:375` actively encourage holding one of each
(subscription for runs, console key for retrospectives).

**Resolution — remember it per token, do not inspect it.** Three options were
weighed:

| | Approach | Verdict |
|---|---|---|
| a | Prefix inspection (`sk-ant-api03-` vs `sk-ant-oat01-`) | **Rejected.** Contradicts a documented decision, and the validator's comment gives the reason: prefixes are not a contract, so a format change silently misclassifies. Misclassifying puts a dollar figure on a free run. |
| b | `total_cost_usd > 0` on the first result frame | **Correct but too late.** Already what #40 Decision 8 and `RunUsage.tsx:142` use. Only available after the first phase — the window this PRD exists to fill. |
| c | **Persist the outcome of (b) on the token row** | **Chosen.** |

(c): the first time a run on token X reports a nonzero `total_cost_usd`, record
`bills_usage` on that token. Every later run on X headlines an estimate from the
first model call; the first run on a new token shows tokens only and degrades to
(b). No format assumptions, self-correcting, one nullable boolean written from
the fold that already parses `total_cost_usd`. **This is the one storage change
in the PRD** and it lands in M2.

### 2. Is a headline dollar estimate acceptable? **Yes — approved 2026-07-29.**

Confirmed by the user. #40 Decision 8's tokens-lead principle is superseded for
the run page, conditional on (1)'s resolution being implemented so a subscription
run never headlines a price. The `hasEstimate` gate is therefore
`hasEstimate && token.bills_usage`.

### Superseded framing, kept for the record

1. **Can uzi distinguish a billed token from a subscription one, at claim time?**
   `SdkEnv` (`agent/src/sdk-env.ts`) hands the SDK `CLAUDE_CODE_OAUTH_TOKEN` and
   pins `ANTHROPIC_API_KEY: undefined` / `ANTHROPIC_AUTH_TOKEN: undefined`, so
   every run authenticates through the OAuth path and the stored secret may be
   either a console key or a subscription token. PRD #40 Decision 8 records the
   consequence: "subscription-auth users may see $0", and
   `RunUsage.tsx:142` renders exactly that today
   (`total.costUsd > 0 ? "your Anthropic token" : "subscription auth · no cost"`).
   **This PRD makes the estimate the lead figure and removes the gate that today
   suppresses it, so a subscription run would headline `~$31.51` for a spend of
   $0** — the PRD's own failure mode, running the other way, in exactly the
   pre-first-phase window it exists to fill. If the answer is no, the honest
   options are (a) suppress the estimate until a confirmed nonzero frame proves
   the run is billed, which removes most of the value, or (b) keep #40 Decision 8
   and show the estimate as a secondary line. *Run `84b6a933` bills normally
   ($3.53), so this is not hypothetical-only in one direction: both kinds exist.*
2. **Is a headline dollar estimate acceptable at all**, given #40 Decision 8's
   tokens-lead principle and Decision 11 caveat (a)'s "must not be forced to
   reconcile"? This is a product call, not a technical one.

## Technical scope

### New: `web/src/lib/modelPricing.ts`

- `interface ModelRates { input, output, cacheRead, cacheWrite5m, cacheWrite1h }`
  in dollars per million tokens.
- `ratesFor(model: string, at: Date): ModelRates | null` — **null, never a zero
  rate**, for an unknown model. A zero silently underprices; a null lets the UI
  say "not priced".
- Effective-dated entries, because at least one known rate change is already
  scheduled: Sonnet 5 is on introductory pricing through 2026-08-31 and reverts
  on 2026-09-01. Encode **all five rates for both rows from the published table**
  (intro 2.00 / 10.00 / 0.20 / 2.50 / 4.00; after 3.00 / 15.00 / 0.30 / 3.75 /
  6.00) rather than deriving the cache rates from multipliers, which invites a
  rounding disagreement.
- Resolution is **exact-match first**, then a documented normalisation (strip a
  trailing date suffix such as `claude-haiku-4-5-20251001`). No `startsWith`
  matching — `claude-opus-5` must not silently match a future
  `claude-opus-5-something` priced differently. This is multica's rule, adopted.
  Note the dated form appears **only in `modelUsage`**; every per-call assistant
  frame on this run carried an undated id, so the normalisation is exercised by
  M1's reconciliation fixture rather than by the live reduction path.

### Changed: `web/src/lib/runUsage.ts`

- A five-bucket-plus-residual usage reader alongside the existing `readUsage`.
- A **non-standard-rate guard**: the reader returns `null` (unpriced) for any
  frame whose `service_tier` is not `"standard"` or whose `inference_geo` is a
  billed-different value, reusing the null-not-zero principle. *Measured: all 598
  frames carried `service_tier: "standard"` and `inference_geo: "not_available"`.
  `speed` is **null on assistant frames** — the `"standard"` value appears on the
  result frame only — so fast mode cannot be gated from the per-call path and
  stays a documented gap rather than a guard.*
- `AgentUsage` and a new per-model rollup gain `estCostUsd: number | null` and a
  `unpricedModels: string[]`, so the UI can render a partial total honestly.
- `hasUsage` splits: `hasConfirmed` (a result frame arrived, today's gate) and
  `hasEstimate` (any assistant frame carried usage).
- The per-agent `model` derivation from PRD #93 (`modelCounts`, `runUsage.ts:214`)
  is reused as-is for attribution; pricing groups by the model on each frame, not
  by the lane's primary model, because a lane can span models.

### Changed: `web/src/components/RunUsage.tsx` and `web/src/pages/RunView.tsx`

**Both** gate on `usage.hasUsage` today (`RunUsage.tsx:110`, `RunView.tsx:683`)
and both must move to the split flag; `web/src/lib/runUsage.test.ts:106` asserts
`hasUsage === false` and must be **repointed, not deleted** (it is the paired
case CLAUDE.md describes).

- Estimate as the lead figure with a tilde and an "Estimate" pill; confirmed
  figure beneath it in the `ok` tone with the phase and timestamp it covers.
- Per-model table with an estimate column; per-agent table gains one too, and the
  "tokens only — per-agent cost is not available" footnote at `:262-264` retires.
- An explicit "not priced" cell for any model or frame with no rate, and the
  total labelled as partial when one is present.

### CLI (second API consumer — CLAUDE.md convention)

The check was performed: `uzi run get` reads the run DTO, whose `usage` comes
from `run_usage` (`api/internal/apitypes/usage.go`, `api/internal/handler/usage.go`),
i.e. the same result-frame path. To show an estimate the CLI would have to pull
the message stream and run the same reduction. **Deferred with the reason
recorded**, to follow M0 so the two consumers cannot diverge. Same for the board,
`uzi run list` and the admin usage view — all inherit the same staleness for
in-flight runs, all out of scope for a run-page problem.

## Milestones

Dependency: **M0 gates everything.** It is an investigation over already-recorded
data, it is the cheapest milestone, and it is allowed to kill the PRD — so
nothing runs beside it. M1 in particular is *file*-safe against M0 but 100% waste
if M0 says no, and M0's outcome changes M1's fixture (see the attribution rule
above).

| Phase | Milestone | Depends on | Files (repo: uzi) |
|---|---|---|---|
| 1 | M0 — Is the residual bias bounded? | — | investigation; no code |
| 2 | M1 — Price table + reconciliation test | M0 | `web/src/lib/modelPricing.ts` (+test) |
| 3 | M2 — Cost in the usage reduction | M1 | `web/src/lib/runUsage.ts` (+test) |
| 4 | M3 — Run-page panel | M2 | `web/src/components/RunUsage.tsx`, `web/src/pages/RunView.tsx`, `web/src/mocks/*` |
| 5 | M4 — Docs + ADR | M3 | `docs/`, `adr/0194-live-cost-estimate.md` |
| 5 | M5 — Verify on a live run | M3 | k8s-first per CLAUDE.md |

- [ ] **M0 — Can per-call `output_tokens` be made whole? Everything hinges on
  that one question.** The review round already did most of the divergence
  analysis, and it collapsed the problem to a single unknown. Recorded here so
  M0 starts where that left off rather than repeating it.

  **What is already settled.** Two hypotheses are closed. (i) "Usage is attached
  to more than one message per SDK frame" is refuted in code by the
  `usageAttached` latch (`agent/src/sdk-executor.ts:1385-1397`). (ii) But the
  *duplication is real one layer up*: measured on the planning phase, **34
  groups covering 69 of 84 usage-carrying frames** share a byte-identical
  `(agent_instance, usage)` signature — e.g. seq 6 (`text`), 7 and 9
  (`tool_use`), all reporting `cache_read=13874, output=1`. Since the latch
  guarantees one attach per SDK frame, those are **separate SDK frames repeating
  one API call's `message.usage`**. So the seam is the SDK emitting several
  frames per API call, not the worker attaching several times, and the fix is a
  dedupe by `(agent_instance, usage)` rather than anything in the worker.

  **What deduping does, and why it is not the answer on its own.** Measured on
  the planning phase against `modelUsage["claude-opus-5"]` (single-model, so
  apples-to-apples; **not** the frame's top-level `usage`):

  | | cache reads | output | priced | vs billed $3.5337 |
  |---|---|---|---|---|
  | Raw sum (84 frames) | 3,004,689 (+41%) | 855 (−98%) | $4.1290 | **+16.8%** |
  | Deduped (49 unique) | 1,940,508 (−9%) | 504 (−99%) | $2.3659 | **−33.0%** |
  | Deduped + true output | — | 41,747 | $3.3970 | **−3.9%** |

  Deduping fixes the input side (+41% → −9%) and makes the **priced** answer
  worse, because the raw figure was only close by two large errors cancelling.
  The third row is the finding: **the missing output tokens are worth $1.03, or
  29% of the billed phase, and with them the estimate lands within 3.9%.**

  **So M0's question is narrow.** Is per-call `output_tokens` a `message_start`
  snapshot (p50 = 3, max = 73 across all 598 frames — not a plausible final count
  for a message emitting a large tool-use payload), and if so can a final count
  be obtained on the live path at all? If yes, this PRD is worth building and the
  target accuracy is single-digit. If no, the honest ceiling is either +17% by
  luck or −33% by construction, and the PRD should be closed.

  **It is a coherence gate, not just an accuracy one — and that is the finding
  that raises its priority.** Rendering the proposal against live data at 10:57Z
  exposed something the percentages hide: the strip's estimated **Tokens out**
  reads **~18.1k for the whole run** while the confirmed figure for the **Plan
  phase alone** reads **41.8k**. A live number covering strictly more work cannot
  legitimately be smaller than a settled number covering less. So an unresolved
  output count does not merely make the cost wrong by a third — it makes the
  panel **visibly self-contradictory to a user who knows nothing about our data
  paths**. The first question would not be "is this estimate accurate", it would
  be "why does your own panel disagree with itself". That is unshippable in a way
  a −33% cost error is not.

  Secondary, only if output is solvable: bound the residual across at least three
  completed runs of different shapes, using PRD #40 Decision 11 caveat (a)'s four
  named mechanisms (signal-only frames, unmapped-block frames,
  compaction/summarization calls, refusal-fallback `supersedes` double-counting)
  to attribute what dedupe does not explain.

  **Deliverable**: a written finding on the issue answering the output-token
  question, the residual bound if it proceeds, and a go/no-go. **Dedupe and any
  correction factor land in `runUsage.ts` (M2), not in `modelPricing.ts`**, which
  is frozen by then and prices tokens rather than correcting them.
- [ ] **M1 — Price table module.** `ratesFor(model, at)` with five rates per
  model, effective-dated entries (Sonnet 5 intro and post-intro both present),
  exact-match-then-documented-normalisation resolution, and `null` for unknown.
  **Tests**: (a) a **reconciliation test** against a recorded **single-model**
  result-frame fixture — the SDK's own `modelUsage` token counts plus that
  frame's 1h/5m split must reproduce its `total_cost_usd` to within 1e-9 (the
  measured delta was 8.9e-16, so this is a tight assertion, and it is the test
  that makes the table trustworthy). Single-model because `ModelUsage` carries no
  1h/5m split and there is no general rule for attributing a frame-level split
  across models; (b) an unknown model returns `null`, not zero; (c) a date before
  and after 2026-09-01 returns different Sonnet 5 rates; (d) a dated model id
  (`claude-haiku-4-5-20251001`) resolves. `npm test` + `npm run typecheck` green.
- [ ] **M2 — Cost in the usage reduction, plus the `bills_usage` flag.** The one
  storage change in this PRD: a nullable `bills_usage boolean` on the Anthropic
  token row, set the first time a run on that token reports a nonzero
  `total_cost_usd`, written from the existing `foldRunUsage` path. The headline
  estimate gates on `hasEstimate && token.bills_usage`, so a subscription run
  never headlines a price and a new token shows tokens only until its first
  phase reports (open question 1). Then: five-bucket-plus-residual reader;
  **dedupe by `(agent_instance, usage)` per M0**; non-standard-rate guard;
  per-model and per-agent `estCostUsd`; `unpricedModels`; `hasUsage` split. Pure
  reduction, no accumulator, per PRD #40 Decision 11 — and note the dedupe is
  itself a reduction over the seq-deduped list, so it stays idempotent under
  replay. **Tests**: (f) three SDK frames sharing one `(agent_instance, usage)`
  signature are priced once, not three times; (a) a **differential test** asserting
  `readUsage(u).fresh === input + 5m + 1h + residual` on every fixture, which is
  the instrument that keeps the two readers from drifting; (b) a fixture where
  `cache_creation` is null but `cache_creation_input_tokens` is nonzero prices
  the residual rather than zero; (c) a fixture mixing 1h and 5m writes prices
  them at different rates — the discriminating case, since a 5m-only fixture
  passes against a split-blind implementation; (d) two models sum per-model and
  per-agent correctly; (e) an unpriced model yields a partial total plus a
  populated `unpricedModels` rather than a silently low number.
- [ ] **M3 — Run-page panel. ~~BLOCKED ON [#195](https://github.com/vtmocanu/uzi/-/issues/195)~~ — UNBLOCKED 2026-08-02, #195 landed.**
  That issue found, during this PRD's review, that the run page's existing token
  columns already read a different population from every rollup surface: the
  client read the result frame's top-level `usage` while the
  server folds only `modelUsage`, and on run `84b6a933` seq 173 they differed by
  2.5-3.2x. Adding an estimate column on top would have put three populations in one
  panel with two of them mislabelled.

  **What #195 actually settled, and two things M3 should take from it.** The
  divergence was worse than stated here: **229x on input**, not 2.5-3.2x, which was
  the ratio on output and cache_read only. And the fix was not the one that issue
  proposed — summing `modelUsage` per frame is insufficient, because a model can be
  present in one result frame and absent from a later one while the server retains it
  via `GREATEST`. The client now keeps a per-model running high-water mark per column.
  See `adr/0195-run-usage-per-model-fold.md`.

  Two consequences for M3's own design:
  - `RunUsage` now exposes **`modelTotals`** (one row per model, four token columns
    plus quantized cost). M3's per-model estimate column should consume that rather
    than re-deriving a per-model split, and it is currently read only by tests.
  - **PREREQUISITE: fix the mock fixtures before building the panel against them.**
    Four defects, all in `web/src/mocks/{data,engine}.ts`, all surfaced during #195's
    review by validators rather than by anyone reading the mocks:

    1. **The two populations carry identical numbers.** Result frames emit
       `usage: {input_tokens: 21_400, cache_read_input_tokens: 188_000, output_tokens:
       6_100}` beside a `modelUsage` saying exactly the same. Real frames diverge 2.5x
       to 229x. So mock mode **structurally cannot reproduce the divergence class**
       #195 was about, and a demo build shows M3 nothing real.
    2. **Every result frame has exactly one model key** (all five: `data.ts` ×3,
       `engine.ts` ×2). So the **per-model** dimension is unexercised in a browser,
       which is the dimension M3's per-model cost column is built on.
    3. **The fixtures contradict themselves about models**: `modelUsage` names
       `claude-sonnet-5` while the per-agent table for that same run shows the run used
       `claude-opus-4-8` **and** `claude-sonnet-5`.
    4. **`engine.ts`'s turns read as cumulative when they are not.** Its second result
       frame goes 9 → 61, which looks like a running total; real `num_turns` is
       per-invocation, settled against the live DB (several runs go 13 → 2, and a
       cumulative counter cannot decrease). **This one already cost us**: the browser
       validator read the panel's "70 turns" against a declared 61, correctly concluded
       something was double-counted, and filed a should-fix against `deriveRunUsage`.
       There was no bug — the fixture manufactured it. web-ux suggests `num_turns: 52`
       for that frame; treat that as its inference about the author's intent, not as
       established.

    **The fix is fixture data only**: one mock run with two model keys, a genuinely low
    top-level `usage`, per-agent models consistent with `modelUsage`, and unambiguous
    per-invocation turns. Do it first, or M3's cost column gets designed, demoed and
    reviewed against data that cannot exhibit the conditions it must handle.
  Estimate-led figure with confirmed beneath it,
  panel renders before the first result frame, per-model and per-agent estimate
  columns, explicit not-priced state, the `:262-264` footnote retired, mock
  scenarios for: pre-first-phase (estimate only), mid-run (both), unpriced model
  present. Both `hasUsage` call sites migrated. **Tests**: the panel renders with
  zero result frames; the confirmed line names its phase and timestamp; an
  unpriced model renders the not-priced cell and marks the total partial;
  `runUsage.test.ts:106` repointed. **On the copy change, grep the retired
  strings across the test tree** per the CLAUDE.md rule on vacuous negative
  assertions. `npm test` + `npm run typecheck` + `npm run build` green.
- [ ] **M4 — Docs + ADR.** A `docs/` page (or a section on the existing usage
  page) explaining the two figures, why the estimate is an estimate, and where
  the rate table lives and when it must be updated. Frontmatter per
  `docs/README.md`; `npm run build` runs `check-docs`. **Plus
  `adr/0194-live-cost-estimate.md`** (numbered by originating issue per the
  CLAUDE.md convention) recording the durable contract, not the table: a
  client-derived estimate may never be persisted, never merged into `run_usage`,
  never presented as billed; and any second consumer (CLI, board, Slack) must
  duplicate the table in its own language under a differential test against a
  recorded `total_cost_usd`. If M0 kills the PRD, no ADR.
- [ ] **M5 — Verify on a live run.** k8s-first per CLAUDE.md conventions. Start a
  real multi-agent run and confirm: the panel appears before the first result
  frame; the estimate climbs; the confirmed line appears at the phase boundary
  naming that phase; a reload mid-run reproduces both figures from replay (the
  reduction is pure, so this is the test that it really is). Record the estimate
  and the final billed total for the same run and note the error, extending M0's
  calibration to a whole run.

### Parallelisation

There is none, and that is the finding rather than an omission. M0 is a go/no-go
gate whose outcome changes M1's fixture, so phase 1 runs M0 alone; every later
milestone consumes the previous one's output in a single file. M4 and M5 could
overlap, but they are one milestone's worth of work between them and both need a
human. A PRD that admits it is serial is more useful than one that invents
phases.

## Risks & mitigations

- **The estimate is wrong in a way that erodes trust.** The measured single-phase
  error is +16.8% raw and −33.0% deduped, and PRD #40 Decision 11 caveat (a) says
  the underlying sum is not expected to reconcile at all. **Neither figure is
  shippable**; only the −3.9% that dedupe plus a real output count would give is.
  Mitigations: M0 gates on exactly that; the figure is labelled an estimate,
  carries a tilde and a band, and is never presented as billed; the confirmed
  figure keeps its own line and its own tone. If output tokens cannot be made
  whole, closing this PRD is the correct outcome.
- **Subscription-auth runs would headline a price for a free run.** See
  [open question 1](#open-questions). This is a correctness risk, not a polish
  one, and it is unresolved.
- **The rate table goes stale on a date, not on a deploy.** No CI signal will
  catch it and no test can assert against a live price. Mitigations: effective-
  dated entries so the one known upcoming change (Sonnet 5, 2026-09-01) is
  already encoded; `null` rather than zero for an unknown model; the M4 doc names
  the table as a thing that must be updated and where the published rates live.
- **`readUsage` looks reusable and is not**, and the obvious replacement is also
  wrong. It merges `input_tokens` with `cache_creation_input_tokens` and discards
  the split; the naive fix (`5m + 1h`) prices writes at zero whenever
  `cache_creation` is null. Mitigation: the residual bucket plus the M2
  differential test, which is what makes two readers of one payload safe rather
  than merely convenient.
- **Fast mode is an unguardable gap from the per-call path.** Fast mode ($10/$50
  on Opus 5) would make the estimate underprice by 2x, and `speed` is null on
  assistant frames, so the reader cannot see it. uzi does not currently set
  `speed: "fast"`. Mitigation: `service_tier` and `inference_geo` *are* on every
  frame and are guarded; fast mode is documented as a known gap in M4, and the
  guard would have to read the last result frame if uzi ever adopts it.
- **Estimate and confirmed are read as competing numbers.** Mitigation: they are
  not peers in the layout — the confirmed figure is smaller, tonally separated,
  and its caption names the phase and timestamp it covers, so it reads as a
  checkpoint rather than a rival.

## Success criteria

- On a run that has not finished its first phase, the Cost panel renders a live
  estimate. Today it renders nothing.
- On a run mid-phase, the estimate moves as the run streams, and the confirmed
  figure is visibly a checkpoint with a named phase and timestamp rather than a
  current total.
- The reconciliation test in M1 reproduces a recorded `total_cost_usd` to within
  1e-9, and fails if any of the five rates is wrong.
- The differential test in M2 fails if the two readers of the same payload
  disagree on totals.
- A model or frame with no applicable rate renders as not priced and marks the
  total partial. No configuration produces a silently-low total.
- On the run verified in M5, the whole-run estimate and the final billed total
  are recorded together, and the error is stated in the PRD rather than assumed.

## Decision log

- **2026-07-29 — Client-derived, not server-side, and mutability is the reason.**
  A client-side price is recomputed from the deployed bundle on every page load,
  so a wrong rate is fixed by a web deploy and corrects history. Anything written
  upstream is frozen at write time. This outranks the performance argument.
- **2026-07-29 — The ingest-time fold is the serious alternative, and it is
  rejected on idempotency.** `foldRunUsage` runs over all *delivered* messages by
  design (#40 Decision 2); an additive accumulator over that same delivery is not
  idempotent and would need a new exactly-once ingest path.
- **2026-07-29 — Static rate table, because there is no alternative.** `GET
  /v1/models` returns no price field and there is no pricing endpoint.
- **2026-07-29 — Five rates per model plus a residual, not two.** The 5m/1h split
  is worth ~7% on the planning phase of `84b6a933`; the residual exists because
  `cache_creation` is nullable in the SDK types even though it was never null in
  598 measured frames.
- **2026-07-29 — Client-only pricing makes the table TypeScript-only.** Any
  future estimate on the CLI, board, admin usage or Slack needs a Go
  reimplementation. That duplication is a contract, which is what the M4 ADR
  exists to record.
- **2026-07-29 — M0 is allowed to kill the PRD**, and nothing runs beside it.
- **2026-07-29 — Supersedes #40 Decision 8 (conditionally) and Decision 11
  caveat (b).** Named above rather than left implicit.
- **2026-07-29 — Headline estimate approved** by the user, conditional on the
  `bills_usage` gate so a subscription run never headlines a price.
- **2026-07-29 — Do not inspect token format; remember the outcome instead.**
  uzi's non-inspection is deliberate and documented in two places; a prefix check
  would trade a documented contract for an undocumented one, and misclassifying
  puts a price on a free run.
- **2026-07-29 — Each dollar figure owns a table.** Confirmed pairs with the
  per-phase breakdown, estimate with the per-agent one. The gap between them is
  then legible as work in flight rather than as a discrepancy, which is what lets
  $3.53 and $45.11 coexist in one panel.
- **2026-07-29 — M0 is a coherence gate, not only an accuracy gate.** Estimated
  tokens-out for the whole run currently reads lower than confirmed tokens-out
  for one phase. That is visible without any knowledge of our data paths.

## Review

Reviewed 2026-07-29 by an architect and a fact-checker before first commit, and
every measured figure was re-derived independently by both.

**Folded in**: the mutability argument for the client seam; the ingest-time fold
as the real alternative to argue with; the refuted attach hypothesis and the
duplication one layer up; the three-way dedupe calibration that reframed M0; the
`RunView.tsx:683` consumer; the nullable `cache_creation` residual and its
differential test; the single-model reconciliation fixture and the 111,427
subtraction; the ADR; the superseded #40 decisions; the subscription-auth open
question; and corrections to the multica characterisation, the 7.66% figure, the
`sdk-messages.ts:315-317` citation, and "frames" vs "calls".

**One finding was refuted on measurement.** The architect held that M0 had
compared the live sum against the result frame's top-level `usage` rather than
`modelUsage`. It had not: the planning phase is single-model and the cited
2,134,247 / 41,747 are `modelUsage["claude-opus-5"]` figures (top-level `usage`
reads 653,081 / 16,854). The +41% / −98% stand as written, and the fact-checker
independently reached the same conclusion.

**Two findings were narrowed by measurement.** The nullable-`cache_creation` bug
is a type-level hazard, not an observed defect: across all 598 frames the key was
always present and `5m + 1h == cache_creation_input_tokens` exactly, residual 0.
The residual bucket is kept anyway, because the invariant demonstrably does *not*
hold on the result frame. And a fast-mode guard cannot key off `speed` from the
per-call path: `speed` is null on every assistant frame and carries `"standard"`
on the result frame only.
