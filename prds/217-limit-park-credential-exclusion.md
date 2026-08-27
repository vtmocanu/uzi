# PRD #217 — Don't re-pick the just-exhausted Anthropic token after a usage-limit park

**Issue**: [#217](https://github.com/vtmocanu/uzi/-/issues/217) · **Label**: PRD · **Priority**: Medium
**Area**: `api/internal/workersvc/limitwait.go` (the park, M1) · `api/internal/store/queries/anthropic_rate_limits.sql` + a migration (M1) · `api/internal/autoselect/select.go` + `api/internal/workersvc/service.go` (the claim, M2) · `api/internal/anthropic/client.go` + `web/src/lib/api.ts` + the meter rendering (M3) · `docs/` (M5).
**Line references** are against `d367653b`.
**Status**: M1–M6 implemented, tested and reviewed (branch `agent/issue-217`). M7 (validate on dev-cluster) is not done — it needs a hosted k8s deploy outside this change's scope.
**Evidence**: measured 2026-08-03 against the live stack, run `a146df98` (issue #78), and read out of the code at that SHA. Adversarially reviewed the same day; the review refuted four claims in the first draft and every correction is recorded inline rather than silently applied.

## Problem

PRD #35 Decision 6e and PRD #111 already deliver "switch tokens and resume" for an
`auto` worker, and they work. Run `a146df98` is the proof:

| time (UTC) | event |
|---|---|
| 14:38:06.75 | `five_hour` limit; worker reports `limit_wait` (run feed seq 2576-2578) |
| — | `runs.limit_resets_at` stored as **16:20:00Z**; `retry_not_before` stamped **14:41:05.74** = `now` + 179s jitter |
| 14:41:12 | sweeper promoted, re-claimed: `anthropic_select_reason: auto`, token `meta`, 55% headroom |
| 14:41:14 | *"resuming an already-approved plan — skipping the planning turn and the approval gate"* |

**The inference from that stamp, stated with the alternative it has to exclude.**
`decideLimitPark` (`limitwait.go:359-386`) produces `now + jitter` in exactly two
ways: the pooled-alternative leg lowered the base to `now`
(`autoselect.NextAvailable`, `available.go:105`), or the floor clamp at
`limitwait.go:384-386` caught a base at or before `now`. The repo's own suite
asserts the same value from both mechanisms —
`limitpark_test.go:125-138` reaches it from a reset **30 minutes in the past**
with nil candidates, and `:241-258` reaches it from the pool leg — so the stamp
alone is not a tell. What closes it here is the **stored** `limit_resets_at` of
16:20:00Z (`queries/runtime.sql:832` ← `limitwait.go:544`, exposed at
`apitypes/run.go:201`): a base an hour and 42 minutes in the *future* cannot be
clamped down by the floor, and the gauge cross-check can only raise it. Only leg
3 lowers. So the run switched credential and resumed three minutes after the
limit instead of waiting an hour and 42 minutes.

*(The first draft argued this from the stamp alone and cited the worker-emitted
run feed rather than the stored column. The review refuted it: with a past-dated
report the floor produces an identical stamp. The conclusion survives on the
stored column; the reasoning did not survive as written. Note the distinction is
which ARTIFACT, not provenance — the stored value still originates in the
worker's report, validated server-side by `validReportedReset`. It is a base, not
a measurement.)*

### What is missing is the other half of that exclusion

The dead credential is excluded from the park's *timing* and is invisible to the
next claim's *selection*:

1. **`NextAvailable` takes an `exclude`** and refuses outright without one
   (`available.go:89-92`), for a stated reason: leg 1 "would fire on the dead
   credential's OWN stale-but-eligible reading and promote the run instantly into
   the window it just exhausted."
2. **`autoselect.Select` has no such parameter** (`select.go:157`), and
   `autoChoice` does not pass one — it fetches the pool, ranks it, and returns
   (`service.go:1223-1245`). The run's own `anthropic_secret_id` is right there in
   `claimSecretID`'s `run` argument (`service.go:1186`) and is never consulted.
3. **Nothing writes the dead credential's gauge at park time.** The only
   value-producing writer of `anthropic_rate_limits` is `usagepoller`
   (`engine.go:295`), on `UZI_USAGE_POLL_INTERVAL`, default **5 minutes**
   (`config.go:621`). (`DeleteRateLimits` exists with no production caller, the
   composite FK cascades on token delete, and e2e writes rows directly. None
   refreshes a reading.)
4. **The park jitter is 60-180 seconds** (`limitwait.go:155-158`), against a
   300-second default poll interval — so the maximum jitter is 1.67x shorter than
   the interval and the minimum is 5x shorter.

So a re-claim can easily land before the gauge learns anything. If the dead
token's last reading still showed at or above `UZI_AUTOSELECT_MIN_HEADROOM`
(default 15, `config.go:631`) it is still `StatusEligible`, and `Select` can hand
the resuming run the very credential that just refused it. The run resumes, dies
on the same window, and spends one of five `RUN_LIMIT_MAX_WAITS`
(`config.go:676`) to learn nothing.

**How wide that window really is, corrected.** The first draft said the jitter was
"strictly shorter than the poll interval's own 1-minute floor" and that the
re-claim "normally" lands first. Both were wrong: 60s is not shorter than the 60s
floor (`config.go:622-627`), and at the 5-minute default the race is roughly even
(mean 120s jitter plus sweeper and claim latency, against a ~150s expected wait
into a 300s cycle). What actually widens the window is not the jitter at all:
`usagepoller` arms a **15-minute refusal backoff** (`engine.go:47`, armed at
`:278` and `:289`) when the usage endpoint definitively refuses — and `:275-276`
names **429** as the observed refusal, which is precisely what a just-exhausted
credential returns. The token most likely to be re-picked is the token most
likely to be in a poll blackout.

### Why run #78 dodged it, and why that is not reassurance

The user's pool is two tokens: `personal` (`auto_status: below_threshold`) and
`meta` (`eligible`). The dead one already read below threshold, so the ranker
could not have picked it. That is a property of that reading at that minute, not
of the code. A token that goes from 70% consumed to a hard limit inside one poll
interval, which is the ordinary shape of a burst, leaves a fresh-and-eligible
reading behind and is fully re-pickable.

### It is not only the parked run

`recordRunCredential` writes the pick before the next claim ranks
(`limitwait.go:135-141`, re-derived from `service.go:1313`→`:1354`), so the
in-flight bias spreads *concurrent* runs. It does not encode "this credential is
exhausted". Every sibling claim in the same poll window ranks against the same
stale healthy reading, so a fleet can walk several runs onto a dead token one
after another.

## Solution

Two mechanisms, at two different layers, because neither covers the other's case.

**M1 — record the limit where the selector already looks.** At park time, mark
the dead credential's window 100% consumed in `anthropic_rate_limits`. Every
subsequent claim, for every run, then sees it through the one classifier
(`autoselect.Classify`) with no new concept.

**M2 — exclude the dead credential from the resuming run's own claim**, on *both*
of `autoChoice`'s exits. Narrow, per-run, one-claim-lived. It covers the three
cases M1 structurally cannot: the `overage` and `unknown` rate-limit types, which
name no gauge column at all (`limitwait.go:429`, the same limitation
`deadCredentialReset` already documents); the case where the dead credential is
the *sole* measurable candidate and `best_of_pool` picks it
(`select.go:198-213`); and the `pool_stale` / `pool_empty` fallback, which
bypasses the ranker's pick entirely.

### M1 writes the pct and NOTHING else

Not `synced_at`, not `five_hour_resets_at` / `seven_day_resets_at`, not the other
window. Each omission is load-bearing:

- **🔴 `synced_at` is the one that inverts the obvious implementation.** The
  reflex is to write `synced_at = now()` so the reading looks current. `Classify`
  reads a single `synced_at` for the whole row, so bumping it re-freshens the
  *other* window's stale reading — and worse, a freshened row is `Measured`, which
  promotes a dead token from "skipped as stale" to "ranked as below-threshold".
  Verified by execution against a byte-identical copy of the package: a fresh
  100%-consumed token beside a stale one yields `reason=best_of_pool
  picked=true id=<dead>`. Bumping `synced_at` can therefore *create* the re-pick
  this PRD exists to prevent. Leaving it alone is correct in both directions: a
  fresh row stays fresh and now reads headroom 0, so it falls under `MinHeadroom`
  and is filtered out of the ranking set whenever *any* token clears that bar
  (`select.go:206-213`); a stale row stays stale and contributes nothing to
  `Select` or `NextAvailable`, which is already the answer we want.

  **🔴 "Headroom 0 therefore loses to anything else" is FALSE, and the first
  draft said it.** Refuted by execution against a byte-identical copy of the
  package: once *no* token clears `MinHeadroom`, `best_of_pool` ranks the whole
  measured set on `Headroom − InflightPenalty × InFlight` (`select.go:186`,
  `:204-205`), so a 0-headroom dead token beats a 5-headroom live token carrying
  two in-flight runs (0 vs 5−6 = −1); and on a rank tie `tieLess`
  (`select.go:263-279`) prefers the **sooner binding-window reset**, which a
  just-exhausted five-hour token usually has. Both cases were reproduced picking
  the dead token. See SC3, which is scoped to what this actually delivers.
  *(Second precision from the same review: `best_of_pool` can pick a dead token
  with no `synced_at` bump at all when it is the sole measurable candidate. That
  case is D2's and M2's — the bump is not the sole cause, it is the one that turns
  a skipped row into a candidate.)*
- **The reset columns must stay the poller's.** Writing the worker-reported reset
  there would launder an untrusted value into a shared row that
  `deadCredentialReset` (`limitwait.go:412-438`) then reads back as its
  independent cross-check. For the same run that is merely circular; for a
  **sibling** run parking on the same credential it is not, because
  `validReportedReset` admits anything up to the year 2100
  (`limitwait.go:247-261`) — a far-future report would become the sibling's base
  and, past the 8-day `RUN_LIMIT_MAX_PARK` (`config.go:677`), **fail that run
  outright** at `limitwait.go:387-391` instead of parking it. Writing the pct
  alone keeps the cross-check reading a poller-measured timestamp:
  `deadCredentialReset` reads only `FiveResetsAt`/`SevenResetsAt`
  (`limitwait.go:426`, `:428`) and gates only on `Measured` (`:420`), and a pct
  write moves `Status` (Eligible → BelowThreshold) without moving `Measured` —
  both carry `Measured: true` (`autoselect.go:167`, `:169`).

  **One precondition, because `Measured` CAN flip.** If the window's pct was
  previously NULL the row classifies `StatusUnmeasured` (`Measured: false`), and
  writing 100 into it makes the row measured for the first time — verified by
  execution. The sole production writer cannot produce that shape
  (`anthropic.Window.Pct` is an `int`, not a pointer, `client.go:67`, and
  `engine.go:332` always sets `Valid: true`), but `e2e/run-e2e.sh:4939,:4996`
  INSERTs rows directly and can. M4 carries a NULL-pct case for it.

### Why M1 is an UPDATE and never an INSERT

If a token has no gauge row it classifies `StatusNoReading` (`autoselect.go:156-158`)
and `Select` skips it already (`select.go:179`). An INSERT would need to invent a
`synced_at` (NOT NULL, `00080:49`) for a reading that was never taken, to make a
token *less* pickable than it already is. So the statement is UPDATE-only, and a
zero-row result is a success.

### 🔴 The `pool_stale` fallback is NOT self-closing

The first draft argued it was, and declined to guard it. The review refuted that,
and the repo had already recorded the correction the draft reversed —
`adr/0035-run-limit-retry.md:230-244`, dated 2026-07-28:

> "the worker's reported reset" is only half the sentence, and the missing half
> matters more than the stated one. When the worker reports no reset either …
> the stamp is the exponential fallback (`15m << priorParks`, capped at 4h).
> **That makes this configuration the fallback's PRINCIPAL consumer rather than
> an edge**

The half that is true: when every candidate is stale, `Select` returns
`ReasonPoolStale` *and* `NextAvailable` returns false, structurally — both legs
require `Measured` (`available.go:104-114`, `autoselect.go:162-169`), which is
exactly what `pool_stale` says nothing has. The half that is false: the base does
**not** therefore fall back to the reported reset. **When the worker reports no
usable reset either**, `haveBase` is false at `limitwait.go:376` and the base is
`now + 15m` on the first park (`limitwait.go:216-228`), so `retry_not_before`
lands 16 to 18 minutes out. That is not "the window has reopened" for a five-hour
limit.

*(That conditional is load-bearing and an earlier version of this section dropped
it, asserting instead that the `terminal_reason` path carries no reset. It is
false: `agent/src/limit.ts:184` computes `futureReset` from the turn's latest
`rate_limit_event` and `:191` returns it **inside** the `terminal_reason` branch.
The field carries no timestamp; the report often does, off a frame stream
independent of the gauge (`limit.ts:104-136`). So "every candidate is stale" does
not imply "no reported reset" — and dropping the "when" is precisely the half-a-
sentence error `adr/0035:236` was written to correct, arriving from the other
side. The ruling below is unaffected: its second half is independent.)*

And in that same configuration the caller never uses `Select`'s pick: it resolves
`workerSecretID(wkr)`, nil for an auto worker, i.e. the owner's **default**
credential (`service.go:1233-1237`) — which for a single-default user is the dead
one. An `exclude` parameter inside `Select` cannot reach that path. **That is why
M2 covers `autoChoice`'s fallback branch and not only its ranking branch**, and
why Success Criterion 2 is deliverable at all.

## Milestones

- [x] **M1 — the park records the exhaustion.** `setLimitWait` marks the dead
      credential's named window at 100% consumed, via a new UPDATE-only query
      touching that one `*_pct` column and nothing else. Fires only for a window
      that has a column (`five_hour`, and the four seven-day spellings, per
      `limitwait.go:412-438`); `overage` and `unknown` are no-ops by
      construction. Includes the migration widening
      `anthropic_rate_limits.source` to admit `limit_report` (00080's CHECK is
      `('usage_endpoint','header_probe')` at `:48` and `:67`; number assigned at
      merge per CLAUDE.md). Two things to write into the query comment so nobody
      "fixes" them: `ListAutoSelectCandidates` does **not** project `source`
      (`queries/anthropic_rate_limits.sql:162-170`), so the new value never
      reaches the selector; and `setLimitWait` already fetches its candidates at
      `limitwait.go:509`, *before* `decideLimitPark` at `:519`, so this write
      cannot perturb the parking run's own decision wherever it is placed and no
      re-fetch is needed to "make it consistent".
- [x] **M2 — the claim excludes the dead credential, on both exits.**
      `autoselect.Select` gains an `exclude uuid.UUID` (the purity guard
      `TestPackageImportsStayPure`, `autoselect_test.go:230-231`, allows exactly
      `time` and `uuid`, and `uuid` is already imported at `select.go:6`), **and**
      `autoChoice`'s `!out.Picked` fallback refuses to resolve to the excluded
      credential. Needs a per-run signal: `runs.limit_wait_count > 0` is sticky
      and wrong (`queries/runtime.sql:835` is the only write and it only
      increments; `PromoteLimitWaitRuns` deliberately never clears it,
      `:863-865`), so a nullable `runs.limit_dead_secret_id` set by
      `SetRunLimitWait` and cleared by `recordRunCredential` is the shape.
      **State the honest lifetime in the column's own comment**: cleared on the
      first claim that successfully *records*, so it survives a claim that dies at
      `GetRunClaimContext` (`service.go:1294`), at `box.Open` (`:1302`) or in
      `openAnthropic` (`:1317-1349`) — "at least one claim", not "exactly one".
      The `self_improve` branch (`service.go:1187-1193`) returns before the auto
      lane and is deliberately unaffected.

      **The fallback exclusion is CONDITIONAL, and that is the whole subtlety.**
      `workerSecretID(wkr)` is nil for an auto worker (`service.go:1218-1222`,
      `:1237`) and nil resolves to the owner's default, which for a single-token
      user **is** the dead credential — so an unqualified "refuse the excluded
      credential" would leave nothing to resolve to and contradicts SC4. The rule
      is: exclude only when a different credential can actually be resolved;
      otherwise spend the dead one, because D7 outranks this feature and a run
      that cannot be placed must still run. Cost the review flagged and this
      milestone must budget: `secretChoice{secretID: nil}` does not *name* a
      credential, so the branch has to resolve the owner's default before it can
      compare it against the exclusion. It is a fetch, not an `if`.
- [x] **M3 — `source` becomes a value a human can actually read.** The first draft
      called this "update a rendering"; the review established it is **add** one.
      Nothing renders `source` today: `web/src/pages/AdminRateLimits.tsx` contains
      the string zero times, and `RateLimitMeters.tsx:132`'s only hit is the
      phrase "single-source-of-truth" in a comment. There are **four** homes for
      this vocabulary, not two — the migration CHECK,
      `api/internal/anthropic/client.go:73-74` (`SourceUsageEndpoint` /
      `SourceHeaderProbe`), the TS union at `web/src/lib/api.ts:1085` (used by the
      DTO at `:1097`), and **`web/src/mocks/data.ts:21,:159`**, which is the
      mock-bundle fixture module rather than a test file, so a `limit_report`
      scenario has to exist there for the new rendering to be visible under
      `VITE_UZI_MOCK=1` at all. Without a rendering, D6's justification for a new
      value is unrealised and M1 ships a meter showing 100% against an older
      `synced_at` with no disclosure.
- [x] **M4 — tests.** Unit: `Select` with an exclusion (including
      exclude-the-only-candidate and `uuid.Nil`); `autoChoice`'s fallback
      exclusion; the park's window mapping, including that `overage`/`unknown`
      write nothing; a `synced_at`-untouched assertion whose mutation is "bump it
      and watch `best_of_pool` pick the dead token"; a reset-columns-untouched
      assertion; and a **NULL-pct** case, the one shape where a pct write flips
      `Measured` (see D4). Live-DB: a `*LiveDB` case proving the one pct moved and
      `synced_at`, the other pct and both reset columns did not. A **`source`
      vocabulary drift test** parsing the CHECK, matching 00089's
      `TestSelectReasonVocabularyMatchesCheck` and 00091's
      `TestRateLimitTypeVocabularyMatchesCheck` — four homes and no drift test is
      the shape D21 exists to prevent — plus its TS counterpart, whose precedent
      is `web/src/lib/runCredential.test.ts:57-58` (`describe("the reason
      vocabulary is one vocabulary") > it("matches migration 00089's CHECK")`,
      parsing the migration at `:46`). **Do not grep for the identifier
      `api.ts:1118-1119` names** — `selectReasonMatchesMigration` exists nowhere
      in the tree; that comment is stale and M5 fixes it. Both Go gates already
      carry `-count=1`.
- [x] **M5 — docs.** `docs/rate-limits.md`, `docs/run-limit-wait.md` and
      `docs/anthropic-token.md:102` (headroom-based selection), plus
      `specs/ai.md`. State the new `source` value and that a park now writes the
      gauge. *(The draft claimed "only the poller writes this table" is stated in
      the query comments; it is not. `UpsertRateLimits`' comment at
      `queries/anthropic_rate_limits.sql:31-39` instead reasons that a **third
      caller** is safe by construction, because `user_secret_id` is the global
      primary key — which is good news for M1 and the opposite of what the draft
      said.)* Two fix-the-doc items this PRD's research surfaced and must not
      leave behind: `queries/anthropic_rate_limits.sql:222-224` says the
      token-delete path "**still runs**" `DeleteRateLimits` while
      `handler/secrets.go:249` says "no `DeleteRateLimits` call needed" (the
      handler is right); and `web/src/lib/api.ts:1118-1119` names a test symbol
      `selectReasonMatchesMigration` that does not exist (see M4).
- [x] **M6 — fix the "stale-but-eligible" phrase at every site.**
      `available.go:83` says leg 1 would fire on the dead credential's
      "**stale**-but-eligible reading"; a stale candidate classifies
      `StatusStale` and contributes nothing (`autoselect.go:162-164`, and it hits
      neither arm of `NextAvailable`'s switch at `available.go:104-114`), so the
      intended sense is "out-of-date but still inside `MaxStaleness`". A first
      version of this milestone said three sites; `git grep -F` finds **five**
      outside this PRD: `available.go:83`, `available_test.go:60`,
      `limitpark_test.go:296`, `specs/ai.md:13748` and `adr/0035:248`. The last is
      arguably a past-tense record, which is a call to make explicitly rather than
      by omission. Fix them in one commit — and note the miscount is itself the
      wording-drift failure CLAUDE.md documents: the sweep has to be the anchor
      token, not the phrasing anyone remembers.
- [ ] **M7 — validate on dev-cluster.** Per the k8s-first convention. Overlaps
      issue #168 (PRD #111 follow-up: validate auto-selection on hosted workers);
      run them together rather than twice.

## Success criteria

1. After a `five_hour` park, the dead credential's gauge row reads 100% for that
   window with `source = 'limit_report'`, and its `synced_at`, its other `*_pct`
   and **both** reset columns are byte-identical to before.
2. A run resuming from a park never claims the credential it just parked on —
   including when the gauge is stale, which is the case that requires M2 to cover
   `autoChoice`'s fallback branch and not only `Select`, and including
   `overage`/`unknown`, where M1 contributes nothing.
3. A sibling run claiming inside the same poll window does not pick the dead
   credential **whenever any other pooled token clears `MinHeadroom`** (this is
   M1's, and it is the half M2 cannot deliver, being per-run). **Scoped
   deliberately, not hedged**: a first draft asserted this absolutely and the
   review disproved it by execution — when *nothing* clears the bar,
   `best_of_pool` ranks the dead token against the rest and can still pick it, via
   the in-flight penalty or the sooner-reset tie-break (see D3). M1 is a strict
   improvement there rather than a guarantee, because today the dead token reads
   healthy and wins outright.
4. A user with exactly one pooled token behaves exactly as today: the park stamp
   still falls back to the reported reset or the exponential schedule, and the
   resume still spends that token. Auto never fails a run — this is what makes M2's
   fallback exclusion conditional rather than absolute.
5. `RUN_LIMIT_MAX_WAITS` is no longer consumed by a park-resume-repark cycle that
   learned nothing.

## Risks

- **R1 — a worker report now steers a SHARED row.** Today the report lands on the
  run row only; M1 widens its influence to the user's whole pool. Bounded three
  ways: the row is chosen by `runs.anthropic_secret_id` (the server's own record
  of what that claim opened, not anything the worker names), only the window the
  allowlisted `rate_limit_type` maps to is touched, and **no worker-supplied
  value is written** — the pct is the constant 100 and the reset columns are left
  alone. So the untrusted report selects *which column*, never *what value*.
- **R1b — the pct write can LENGTHEN a sibling's park, and in one shape convert
  it into a failure.** Measured: a candidate at `five_hour_pct=40` contributes
  `now` to `NextAvailable`, and at 100 it contributes its binding-window reset
  instead (`available.go:107-113`), so a sibling that would have promoted
  immediately now waits. `NextAvailable` only lowers the base
  (`limitwait.go:371-375`), so withdrawing a `now` contribution can only lengthen
  a park — and if the dead credential's binding-window reset is NULL,
  `BindingWindowReset` returns false (`available.go:24`, a one-line wrapper over
  `resetKey`, whose nil check is `select.go:314-316`), it contributes
  nothing at all, and a sibling whose own reported reset is beyond
  `RUN_LIMIT_MAX_PARK` **fails** at `limitwait.go:387-391` where it previously
  parked. This is D4's hazard reached through the column D4 permits, and it is
  the correct trade — the sibling is waiting because the credential really is
  exhausted — but it must be tested for, not discovered.
- **R2 — how long the write persists is NOT "until the next poll tick".** The
  draft asserted that mechanism and the review refuted it: `usagepoller`'s
  15-minute refusal backoff (`engine.go:47`, `:275-289`) is armed by exactly the
  429 a just-exhausted credential returns, so that token may not be re-polled for
  15 minutes. The bound that actually holds comes from D3 instead: with
  `synced_at` untouched, the write only steers for as long as the row's
  pre-existing freshness lasts, i.e. at most `AutoselectMaxStaleness`
  (`config.go:651`, 15m at defaults) from the *previous* poll. True conclusion,
  different mechanism.
- **R3 — the fix makes the wrong token unpickable.**
  `runs.anthropic_secret_id` is authoritative here: `SetRunAnthropicSecret`
  (`queries/runtime.sql:582`) is its only writer, and neither
  `PromoteLimitWaitRuns` (`:886-892`) nor `ClaimRun` (`:470-477`) touches it, so
  the credential named at park time is the one that was spending. M2's
  clear-on-record is what keeps the exclusion from outliving its claim.
- **R4 — sqlc's inference on the new statement.** CLAUDE.md records both traps: a
  green `sqlc generate` is not evidence the statement runs, and expression
  inference is weak. Mitigated by writing two plain single-column UPDATEs (one
  per window) rather than one `CASE`-driven statement, and by requiring the
  live-DB test in M4 before the milestone is ticked.
- **R5 — this looks like a fix for a bug nobody has seen fail.** Correct. The
  live evidence shows the *feature* working; the hole is reachable and cheap to
  close, not observed in production. Scope is deliberately two narrow mechanisms
  and no new user-facing control.

## Out of scope

- Changing the park jitter, `RUN_LIMIT_MAX_WAITS`, `RUN_LIMIT_MAX_PARK` or the
  poll interval. The jitter in particular is load-bearing for spreading a
  promoted wave (ADR-35 D4, `adr/0035-run-limit-retry.md:146`) and must not be
  shortened to "resume faster".
- Any new user-facing control. Switch-and-resume is already the behaviour of an
  `auto` worker; this PRD makes it reliable, it does not make it configurable.
- Polling a credential on demand at park time. It spends the user's token, and
  the report we already have says the same thing for free — and would in any case
  hit the same 15-minute refusal backoff R2 describes.

## Decision log

- **D1 — record the limit in the gauge rather than only excluding at claim.** The
  exclusion alone is per-run and leaves every sibling claim ranking against a
  stale healthy reading. The gauge write is where the selector already looks.
- **D2 — but keep the exclusion too, on both of `autoChoice`'s exits.** `overage`
  and `unknown` name no gauge column; the sole-measurable-candidate case reaches
  `best_of_pool` regardless of what the row says; and the `pool_stale` /
  `pool_empty` fallback never consults `Select`'s pick at all. Three cases M1
  cannot cover.
- **D3 — do not bump `synced_at`.** The obvious version of this write creates the
  failure it is meant to prevent. Verified by execution, not by reading.
- **D4 — write the pct only, never the reset columns.** Keeps
  `deadCredentialReset`'s cross-check reading a poller-measured timestamp, and
  keeps an untrusted far-future report from failing a sibling run outright. Note
  the one shape where a pct write still moves `Measured` (a previously NULL pct)
  and R1b's sibling-park lengthening, neither of which the reset columns cause.
- **D5 — the `pool_stale` fallback IS guarded, reversing the first draft.** It is
  not self-closing: when the worker reports no usable reset either, the base is
  `now + 15m` rather than the window's real reopening, and the fallback resolves
  to the owner default which may be the dead credential.
  `adr/0035-run-limit-retry.md:230-244` had already recorded the first half on
  2026-07-28 and the draft reversed it. The guard is conditional per M2, so D7
  still holds.
- **D6 — `source = 'limit_report'` rather than reusing `header_probe`.** An
  operator reading the row must be able to tell a measurement from an inference
  drawn off a refusal — and, given D3, must be able to tell that a 100% reading
  is newer than the `synced_at` beside it. Costs one migration, one Go constant,
  one TypeScript union member, one drift test and, per M3, the rendering that
  makes any of it visible.
- **D7 — UPDATE-only.** A missing gauge row already means "never picked".
