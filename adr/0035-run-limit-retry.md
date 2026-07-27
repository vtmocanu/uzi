# ADR-35: Which credential a promoted run spends, and where the wait is computed

**Status**: Accepted (PRD #35 in flight — this ADR records the M0 design gate, not a merged implementation)
**Date**: 2026-07-27
**Deciders**: architect (M0 design gate), team lead, Vlad (open questions 1 and 2)
**PRD**: [prds/35-run-limit-retry.md](../prds/35-run-limit-retry.md) (GitLab issue [vtmocanu/uzi#35](https://gitlab.example.com/vtmocanu/uzi/-/issues/35)) — the PRD carries the milestones, the fourteen decisions and the decision log; this ADR carries **one** decision, because it is the one most likely to be re-litigated by someone reading the PRD's original three options and wondering why the recommended one was not built.

## Decision (summary)

When a run parks on an Anthropic usage limit (`limit_wait`) and is later promoted back
to `queued`, **it re-enters the ordinary claim path and PRD #111's auto-select runs
again, freely — the run pins nothing.** What changes instead is the **meaning of
`retry_not_before`**: it stops being *"the reset of the credential that was
exhausted"* and becomes *"the earliest moment this user could plausibly spend
**anything**"*, computed **at park time** over the user's whole opted-in pool.

The PRD's own recommended option — *re-select, then recompute `retry_not_before` from
the newly chosen credential* — **is not buildable as written**, and that is the part
of this decision that is not obvious from the outcome. See Context.

## Context

### The two mechanisms, and why their order is the whole problem

PRD #111 shipped `api/internal/autoselect`: at **claim** time, a worker in
`anthropic_bind_mode = 'auto'` has its user's opted-in Anthropic credentials ranked by
rate-limit headroom, and any below `MinHeadroom` is skipped. The chosen credential is
recorded on the run (`runs.anthropic_secret_id`, `00086`).

PRD #35 adds a park: a run that dies on a sustained usage limit becomes `limit_wait`
and is stamped with `retry_not_before`. A sweeper pass promotes it back to `queued`
once that clock passes.

So the sequence is:

```
park  ──►  [ retry_not_before gates PROMOTION ]  ──►  queued  ──►  claim  ──►  [ auto-select RE-SELECTS ]
             ▲                                                                    │
             └──────────────── the PRD asked for this to depend on ───────────────┘
```

**Promotion is strictly upstream of the claim where re-selection happens.** There is
no point in the lifecycle at which the newly chosen credential is known and the gate
has not already fired. A recomputation "from the newly chosen credential" therefore
has nothing to recompute at the moment it would need to run. The PRD's recommendation
was a coherent *goal* expressed as an uncomputable *mechanism*.

### What made the naive fix wrong too

The obvious repair — *stamp the earliest reset across the user's credentials* — is
also wrong, for a reason worth recording because it recurs: **a credential's reset is
not "when it becomes usable".** A token at 5% consumed has a `five_hour_resets_at`
three hours out and is spendable *right now*. `min(resets)` over the pool would still
delay a run that could run immediately, just by less.

The quantity actually wanted is *"when is each credential next spendable"*, which is
`now` for anything with headroom and a reset only for the ones that are exhausted.

## The decision

### D1 — Re-select freely; PRD #111's resolution path is untouched

A promoted run carries no credential override. `claimSecretID` keeps its three
existing modes and gains no fourth.

**Rejected: pin the original credential across the park.** It is correct by
construction — the gate and the spend would agree — and it loses anyway. It requires a
run-carried override that outranks the claiming worker's configured
`anthropic_bind_mode`, i.e. a change to PRD #111's resolution path that PRD #35's own
Out of Scope protects; and it produces the worst outcome for exactly the multi-token
users PRD #111 exists to serve (the run waits for token X's reset while token Y sits
idle).

**Rejected: re-select and accept a stale gate** (i.e. change nothing). This fails
PRD #35's Success Criterion 6 verbatim: *"a promoted run does not sit behind a
`retry_not_before` computed from a credential it is no longer going to spend."*

### D2 — The wait is computed at PARK time, over the pool, and the promotion gate stays a clock

At park time the server has the run, the user, and — via the existing
`ListAutoSelectCandidates` query — every candidate's gauge row. So:

```
base := crossCheckedReset(worker-reported reset, gauge row for the DEAD credential)
if alt, ok := autoselect.NextAvailable(cands, deadSecretID, policy, now); ok && alt < base {
        base = alt
}
retry_not_before = clamp(base + jitter(60..180s))
```

`NextAvailable` is **pure** and lives in `autoselect` — the package that already owns
credential classification (PRD #111 D21's "one classifier, not two"). Excluding the
credential that just died:

| candidate | contributes |
|---|---|
| classifies `eligible` | **`now`** — spendable immediately |
| `Measured` but below threshold | its **binding-window** reset (whichever window produced `headroom`'s `min`) |
| not pooled / no reading / unmeasured / stale | **nothing** |

An unknown contributes nothing in *either* direction: it must not pull the floor to
`now` (thrash) and must not push it out (a false wait). The binding-window rule is
reused, not reimplemented — a second copy of it is precisely the drift D21 exists to
prevent.

**Rejected: ask "does this user have any credential with headroom?" in the PROMOTION
pass**, i.e. drop the stamped clock entirely. Three costs, the third decisive:

1. It turns the only single-statement pass in `Sweep` into a per-user Go loop whose
   cost scales with parked runs × pooled credentials, on a ticker.
2. It is **broken outright when the usage poller is disabled**
   (`UZI_USAGE_POLL_INTERVAL=0` — which is what uzi's own e2e overlay sets): every
   candidate classifies `stale`, nothing ever has headroom, and **no parked run ever
   promotes**. Inverting it to fail-open promotes everything instantly instead.
3. The gauge is a percentage-consumed reading that lags the poll interval, and
   `blocking_limit` is a different signal from it. A run that just died on a limit can
   still read as having headroom, so it promotes, re-parks, and burns its
   `limit_wait_count` to the cap in minutes — strictly worse than a stamped clock.

The park-time formulation keeps the good half of that idea (an eligible alternative
means "promote on the next tick") while the gate itself stays
`retry_not_before <= now` in SQL.

**Rejected: gate the pool legs on `runs.anthropic_select_reason`** (apply them only
when the last claim went through the auto lane, so a pinned-worker user is never
promoted early). More precise, and rejected because it promotes a column that
`00086_run_anthropic_secret.sql` explicitly documents as *"display-only: nothing in
the state machine, the claim path or any sweep gate reads it"* into a load-bearing
one, to buy at most one wasted re-park. `Classify` already requires `auto_eligible`,
so the legs fire only for a user who **deliberately pooled** a second credential.

### D3 — The server's own clock beats the worker's, per window

The dead credential's gauge row is in the same fetch, so the cross-check costs
nothing. `rate_limit_type` selects the column (`five_hour` → `five_hour_resets_at`;
the four `seven_day*` members → `seven_day_resets_at`; `overage`/`unknown` → no
cross-check), and the answer is `max(worker reset, gauge reset)` when the reading is
fresh: both then describe the *same* window, and promoting before the later of them
guarantees a re-park. The per-window mapping is what stops `max` from over-delaying a
five-hour block by a seven-day rollover.

This is **accuracy, not security**. The security properties come from elsewhere and
are unchanged: `RUN_LIMIT_MAX_PARK` clamps an inflated reset, and a deflated one costs
the attacker's own user at most `RUN_LIMIT_MAX_WAITS` self-scoped retries.

### D4 — A promotion wave is spread by TIME, never by the load counter; the jitter is the mechanism, not a garnish

D2 manufactures something that did not exist before it: **a correlated wave.** N runs
parked against the same exhausted credential all become promotable at roughly the same
reset. Without D2 they would simply have failed, N times, independently.

`PromoteLimitWaitRuns` is a **single UPDATE that promotes every row whose clock has
passed**, so one sweeper tick releases the entire wave at once. The 60-180s jitter on
`retry_not_before` is therefore the only thing that separates them. **It is
load-bearing and must not be "simplified" away** on the theory that the sweeper tick
already spreads runs out — it does the exact opposite.

**How the spread actually works, and why it needs no new mechanism.** Claim ordering
is: `ClaimRun` (status → `claimed`) → `assembleClaim` → `claimSecretID` → `autoChoice`
→ `ListAutoSelectCandidates` → pick → `recordRunCredential` (stamps
`anthropic_secret_id`). So a run that claimed *earlier* has already **recorded** its
pick by the time a later run ranks, and the existing in-flight counter sees it and
penalizes that token. Jitter separates the promotions; claims then serialize; each
pick is recorded before the next ranking reads it. **The jitter and the counter are
one mechanism, not two.**

#### 🔴 Widening the load counter to include parked runs CANNOT work, and this is why

It is the obvious fix and it will be proposed again, so the refutation is recorded
rather than the conclusion:

**`runs.anthropic_secret_id` on a parked run names the credential the run
SPENT, not the one it is about to spend.** The park does not clear it, promotion does
not clear it, and only the *next* claim's `recordRunCredential` overwrites it. So
every parked run in the wave is recorded against **X — the exhausted credential** —
and counting them would pile phantom load onto the one token that is already excluded
for being empty. It adds **exactly zero** asymmetry between the candidates Y and Z
that the wave is actually going to converge on.

The general form, which is the part worth keeping: **the in-flight bias works because
it is per-token asymmetric.** A run that has not yet chosen a credential contributes
no asymmetry, and a load term applied equally to every candidate cannot change a
ranking. **Parked load is structurally unrankable.** No widening of that counter — by
`retry_not_before`, by status, by anything — can spread a wave, because the
information it would need (which token each parked run is *going* to pick) does not
exist until the run picks it.

The counter's exclusion of `limit_wait` is therefore not a gap that D2 opened. It is
**correct, and correct for D2's own reason**: a counted run is one that has chosen.

#### Rejected alternatives

- **Stagger the claim rather than the promotion.** This duplicates the jitter one
  layer down: a claim follows its promotion within one worker poll interval, so
  jittered promotion already *is* a staggered claim. Implementing it separately needs
  either a second not-before column or an in-process timer, and both are more
  machinery for the same effect.
- **`LIMIT` the promotion batch per tick** (considered here, not raised by the review;
  recorded because it is the next idea anyone has). It bounds the wave regardless of
  jitter collisions and costs one clause — and it converts a **rare** ranking collision
  into a **guaranteed** delay for the tail of every wave, since the sweeper tick can be
  minutes and the (N−K)th run then waits extra ticks. Bad trade against a race that
  self-corrects in one re-park.

#### Accepted residual, stated honestly

Two promotions closer together than one claim round-trip can still rank identically
and converge on the same token. **That race is not new** — it exists today for any two
concurrent claims, and PRD #111 accepted it in the in-flight bias' own comment
(*"several claims inside one interval read the SAME headroom and would pile onto the
same emptiest token"*). D2 makes it **more likely** by correlating the promotions, and
the jitter is what bounds it.

The cost is small and self-correcting: if the token they converge on has real
headroom, both runs simply run and nothing bad happened; only if it is near-empty does
the second re-park, costing one unit of `RUN_LIMIT_MAX_WAITS`. **If this ever bites,
the knob is the jitter RANGE, not the counter.** The observable signal is
`SweepResult.LimitPromoted > 1` on a single tick — already reported, so no new
instrumentation is needed to see it.

## Consequences

**The two no-regression properties are structural, not tested-in.** They are why this
shape was chosen over the alternatives, so they are the first thing to check if
someone later "simplifies" it:

- **A single-token user is bit-identical to the naive design.** The pool holds one
  candidate, it is the dead one, it is excluded, no leg contributes, and the stamp is
  that credential's cross-checked reset.
- **A poller-disabled deployment is bit-identical too.** Every candidate classifies
  `stale`, `Measured` is false, no leg contributes, and no fresh gauge row exists to
  cross-check against — so the stamp is the worker's reported reset.

**A run with no recorded credential** (`anthropic_secret_id IS NULL` — a pre-feature
run, or a claim whose recording failed) skips **both** legs. Without an exclusion id,
the eligible leg could fire on the dead credential's own stale-but-eligible reading
and promote instantly.

**The stamp is a floor, and floors can be wrong.** Another run may consume the
alternative between the park and the promotion; the cost is one re-park against the
`RUN_LIMIT_MAX_WAITS` budget. **Accepted residual**: a user with more pooled
credentials than that budget (default 5) can burn it cycling through them, after which
the run fails with *"usage-limit retry budget exhausted"* — which is honest. The user
ruled on this directly (2026-07-27): the default stays 5, because it is a **retry**
budget rather than a credential-count budget, and a large-pool operator raises it via
env.

**`self_improve` and `judge` runs route to the owner's judge binding, never to
`autoChoice`** (`service.go:1094-1100`), so the pool legs are inert for them. That
falls out of the existing code; no kind check was added for it, and none should be.

## Adjacent decisions, recorded elsewhere on purpose

This ADR is deliberately narrow. The other durable shapes of PRD #35 live in the PRD's
own decisions, and duplicating them here would create two records to keep in step:

- **Which run kinds park** (`issue`/`ci_fix`/`self_improve` yes; `judge`/`chat` no) —
  PRD Decision 14.
- **The park acknowledgement contract** (the worker skips its three filesystem
  cleanups **iff** the server's reply reports status `limit_wait`, never *iff*
  `applied`) — PRD Decision 4. It is a cross-component invariant whose failure mode is
  a silent unbounded disk leak, so read it before touching either side.
- **The four independent guardrail layers** are untouched by this PRD. Nothing here
  changes what a worker may do, only which credential it is handed and when.

## Note for whoever merges PRD #35

This ADR is **not yet linked from `ARCHITECTURE.md`**, deliberately: ARCHITECTURE.md
describes what uzi *is*, and PRD #35 is not built. Add the link in the same change
that archives the PRD to `prds/done/`, alongside the run-lifecycle paragraph that
describes the `limit_wait` state — not before, or ARCHITECTURE.md documents a status
the database does not have.
