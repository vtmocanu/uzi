# ADR-106: The plan-revision cap is enforced by a counter on the run row

**Status**: Accepted (implemented, issue #106)
**Date**: 2026-07-29
**Deciders**: architect (design), team lead, Vlad — who **requested this ADR over the architect's recorded recommendation not to write one** (the convention is one ADR per PRD, and this is an issue). The objection is recorded, not dropped; see the numbering note below. Implementation and the validation runs quoted throughout: coder, on branch `fix/106-revise-cap-atomic`.
**Issue**: GitLab issue [vtmocanu/uzi#106](https://github.com/vtmocanu/uzi/issues/106) — there is no PRD. The issue carries the reproduction, the file map and the test plan; this ADR carries **only** the decision and the alternatives, because each alternative will look attractive again to someone who has not measured, and three of them are the first thing a competent reader reaches for.
**Numbering**: `0035`, `0042` and `0065` are PRD numbers; `0106` is an **issue** number. ADRs are numbered by their tracking item, whichever kind it is. Noted because a reader who assumes "ADR number == PRD number" will go looking for `prds/106-*.md` and find nothing.

## Decision (summary)

The plan-revision cap moves its source of truth from `count(*)` over `run_user_inputs`
onto a **counter column on the `runs` row**, `runs.revise_count` (migration `00094`), and
the cap predicate moves into the **UPDATE's own `WHERE` qual**:

```
WITH bumped AS (
  UPDATE runs SET revise_count = runs.revise_count + 1
  WHERE runs.id = @run_id AND runs.revise_count < @max_revisions::int
  RETURNING runs.id AS run_id)
INSERT INTO run_user_inputs (run_id, kind, body)
SELECT bumped.run_id, 'revise_plan', @body FROM bumped
RETURNING *;
```

Refusal is still "zero rows returned", so `pgx.ErrNoRows → ErrReviseCapReached` and
every caller are unchanged. `count(*)` over `run_user_inputs` survives as
`CountRunReviseInputs`, demoted from enforcer to **auditor**.

The rule that generalises past this one query, and the only sentence here worth
memorising:

> **A cap predicate that must survive a concurrent writer may reference only columns of
> the row the statement locks. Any subquery in that qual reintroduces the defect.**

## Context

### What shipped, and why it read as atomic

PRD #41 closed a genuine count-then-insert TOCTOU by collapsing two round trips into one
statement: a leading CTE took the `runs` row `FOR UPDATE`, and the cap counted
`run_user_inputs` filtered on the locked id. Its comment claimed callers therefore
serialize — "a second caller blocks until the first commits, then counts including the
first's row."

The first half is true and the second is false. The lock is on **`runs`**; the count is
over **`run_user_inputs`**, a different table, read at the statement snapshot. The
statement is one statement and is *not atomic for this purpose*, which is a distinction
the word "atomic" actively hides. `workersvc.SubmitInput` calls it on the bare pool — no
transaction, no isolation setting anywhere in `api/` — so READ COMMITTED, and two
concurrent submits at N-1 both persist.

It is reachable from two production entry points that a single-owner gate genuinely
races: `handler/workers.go` (HTTP) and `slacksvc/replier.go` (Slack).

### The mechanism: EvalPlanQual re-reads the row, not the snapshot

Under READ COMMITTED, when an `UPDATE` blocks on a row another transaction is updating
and that transaction commits, Postgres does **not** abort and does not re-plan. It
re-evaluates the statement's qual against the **new version of the locked row**
(EvalPlanQual). Postgres documents the consequence plainly: such a command "can see the
effects of concurrent updating commands on the same rows it is trying to update, but it
does not see effects of those commands on **other rows** in the database."

A `run_user_inputs` row is an *other row*. So a subquery counting that table, sitting
inside the re-evaluated qual, still answers from the original statement snapshot and
still returns N-1. **The snapshot is the defect; the lock was never the defect.** That is
why strengthening the lock changes nothing (Option A′) and why moving the counted
quantity onto the locked row changes everything.

## The decision

### D1 — The cap predicate lives on the `runs` row, inside the UPDATE's own qual

`revise_count` is a column of the row EPQ re-reads, so the re-evaluation is authoritative
by construction rather than by care. The loser's UPDATE matches zero rows, the dependent
INSERT inserts zero rows, the caller gets no row, and the existing refusal mapping fires
unchanged.

Two halves of the shape already exist in this repo and neither is novel: the
data-modifying-CTE-feeding-an-INSERT is `CreateStopVerdictInput`, and the
counter-cap-in-the-qual is `RequeueRunsOfStaleWorkers` / `RequeueWorkerRuns`
(`requeue_count < @max_requeues`). Only their **combination** is new, which is why it was
gated on live-DB execution rather than on `sqlc generate` exiting 0.

The migration backfills `revise_count` from `count(*)` of existing `revise_plan` rows in
the same migration that adds the column. This is load-bearing, not tidy: without it every
existing run's budget silently resets to zero, and a run already at the cap gets a fresh
three.

#### Every column in the UPDATE's WHERE and RETURNING must be qualified `runs.`, and sqlc is why

Not a stylistic preference — the unqualified form **does not build**:

```
column reference "id" is ambiguous
```

Two teeth, both found by running it and neither guessable from reading:

- **Postgres itself accepts the unqualified statement.** The ambiguity is sqlc's own
  resolver, which sees `id` in scope from both `runs` and the INSERT's target
  `run_user_inputs`. So "it runs fine in psql" is not evidence the shape is legal here,
  and the constraint lives in the code generator rather than in the server.
- **The error is attributed to the NEXT query in the file**, not to this one. A reader
  who trusts the reported line number debugs an untouched, correct query. This costs an
  hour if you do not know it, which is the only reason it is recorded in an ADR.

**The SET target is necessarily bare**, and the scope above says "WHERE and RETURNING"
for that reason. Postgres rejects a qualified SET target outright — `UPDATE t SET t.n = …`
gives `column "t" of relation "t" does not exist` — so `revise_count = runs.revise_count + 1`
is the only legal spelling. *(Corrected 2026-07-29: this subsection was headed "every
column in the UPDATE", which is not a rule anyone could follow. Caught by the findings
round on `eb1bfe90`, which fixed the same wording on the query and the migration; the ADR
copy was missed. A rule stated one notch too broad is worse than a narrow one, because the
person who tries to obey it concludes the codebase is wrong.)*

Anyone editing this statement re-enters both traps. The query carries the same warning
inline, where the edit happens.

### D2 — The counter and `count(*)` are a deliberate duplication, and CASCADE is what makes it safe

Two representations of one fact now exist: `runs.revise_count` **enforces**, and
`count(*) … WHERE kind='revise_plan'` **records**. That is a contract, not a convenience,
and it was the open question this design had to close before the counter could be
recommended at all.

They cannot diverge, structurally:

- `run_user_inputs` has **exactly one** foreign key — `run_id → runs ON DELETE CASCADE`
  (`00020_workers_runs.sql`), and **no later migration touches it**: `00074` widens the
  `kind` CHECK, `00092` widens it again and adds a nullable `question_id` (PRD #88).
  *(Corrected 2026-07-29: this read "the two later migrations touching the table widen the
  `kind` CHECK". The count was wrong when written — it counted `00050`, which mentions the
  table only in a comment — and `00092` has since made the description wrong too. Naming
  the files and what each did is re-derivable; a count is a measurement with a shelf life,
  and this one expired twice without ever being read as false.)*
- There is **no `DELETE FROM run_user_inputs` anywhere in the repo**, in sqlc queries or
  in raw Go SQL.
- Deletes of whole `runs` rows **do** exist, and they are the safe case: no sqlc query has
  one, but `e2e/run-e2e.sh:4053` deletes runs directly as a PRD #98 fixture teardown, and
  `runs` rows also die by cascade from `users`, `repos`, or self-referentially from
  `runs.target_run_id`. *(Corrected 2026-07-29: this bullet read "and **no `DELETE FROM
  runs`** either", which is false. The invariant is unaffected — deleting a whole row
  deletes the counter with it — but the paragraph's value is that a reader can re-derive
  it, and a false line in an enumeration discredits the lines beside it.)*

So the only path that deletes a counted row is the cascade fired by deleting its `runs`
row — which deletes the counter in the same statement, because the counter is a column of
that row. **The counter cannot survive its own rows.** This is a property of where the
column lives, not of anyone maintaining an invariant.

**Ruling on the one future divergence source.** A later retention/pruning
`DELETE FROM run_user_inputs` (plausible: this is an append-only steering channel) would
decrement `count(*)` and not the counter. **That divergence would be correct.** The cap is
a *lifetime budget*, not a row inventory — the same argument that makes
`TestCountRunReviseInputsCountsConsumedRowsLiveDB` right about consumed rows. A pruned
revise was still spent. Do not "fix" it.

**The differential test that pins the two is not decoration, and this is measured rather
than asserted.** `TestReviseCountMatchesRowCountLiveDB` exists because the most likely
miswrite of D1 is to put the cap predicate on the **INSERT** instead of the UPDATE: still
one statement, still refuses at the cap, but the counter bumps on refusals too and the
run's remaining budget silently shrinks with every rejected attempt. Applied as a
mutation, that miswrite leaves **both the forced-interleave test and the sequential
control PASSING** — the concurrency behaviour it breaks is not the concurrency behaviour
they assert. Only the differential test catches it, and specifically only its case that a
**refused** insert must move neither representation.

### D3 — `CountRunReviseInputs` keeps reading `count(*)`, and is deliberately not repointed

Repointing it at the counter is the tempting tidy-up and it is the wrong one: it would
make the existing assertion in `TestCreateRunReviseInputIfUnderCapAtomicLiveDB` assert
that the counter equals itself — a vacuous green that reads as coverage. The query is the
only instrument positioned to catch the counter drifting from reality, and it can only do
that job while it computes the quantity independently.

Deleting it is wrong for the inverted reason: **its lack of a production call site is
exactly what makes it a good auditor.** Nothing can pull it toward the implementation.

### D4 — The counter UPDATE deliberately does **not** bump `updated_at`

This departs from the house style of every other `UPDATE runs` in the file, and the
departure is the decision.

`ListActiveRunsForHealth` includes `awaiting_approval` and selects `updated_at`;
`healthTargetFor` times the `approval_idle` health flag off it (default 1h). A revise
lands while the run is `awaiting_approval`. So bumping `updated_at` would silently change
when a user-visible health flag fires — a behaviour change riding inside a concurrency
fix, decided by a reflex rather than by a decision.

**The precedent that appears to authorise the bump does not.** `CreateStopVerdictInput`
bumps `updated_at` on an `awaiting_approval` run — but the verdict it carries is
cancel/reject_plan, which drives the run terminal, and `healthTargetFor`'s `default:` arm
returns healthy for terminal statuses. Its bump **can never change a flag outcome**. A
revise leaves the run `awaiting_approval`, where it can. The two writes look identical and
are not, and the asymmetry is invisible unless you enumerate the readers.

The method here is borrowed verbatim from `SetRunAnthropicCredential`, whose own comment
enumerates every reader of `runs.updated_at` to prove its bump is safe — and whose comment
records that naming only *one* of two readers "would have left a reader of the same column
unaccounted for while reading as though the set were complete." Running that method here
gives the opposite answer. That is the method working.

**D4 has a second consequence, found only afterwards, and recorded because nobody
reconstructs this kind of thing later.** The fix for issue #182 keys on
`run_user_inputs.created_at >= runs.updated_at` — "an input arrived at or after this gate
opened" — using `updated_at` as the **episode boundary** as well as the clock. Had this
decision gone the other way, a revise would have bumped `updated_at` to `now()` in the
same statement that inserts the row, producing `created_at == updated_at` **and** a
restarted clock: #182's predicate would have been moot for a full threshold period and its
headline case masked entirely. The two designs compose, and not by luck — both follow from
refusing to conflate *the human acted* with *the run is healthy*. That refusal is the
actual decision here; the untouched `updated_at` is just where it shows.

## Options considered, and why each fails

Recorded at length because each is what a careful person reaches for first.

### Option A′ — keep counting rows, but take a real `UPDATE` instead of `FOR UPDATE` (REJECTED — measured)

The obvious strengthening: if the lock is not doing its job, take a firmer one. **Measured
still over cap.** EPQ re-reads the locked row and the count subquery keeps the original
snapshot, so nothing about the answer changes. Recorded first because it is the cheapest
patch, it looks like it must work, and it does not.

### Option B — a DB constraint, following `00020`'s own precedent (REJECTED)

`uq_runs_one_active_per_issue` fixed the structurally identical check-then-insert TOCTOU
with a partial unique index — a constraint enforced outside snapshot visibility, the only
class that is *structurally* immune. It is the right instinct and the precedent **does not
transfer**: it works because "at most **1**" is a constant expressible in an index
predicate. Here N is `PLAN_MAX_REVISIONS`, a **runtime environment variable** (default 3,
also shipped to the worker in the claim payload). No CHECK and no index predicate can
reference a runtime config value. Expressing "at most N rows" as a constraint requires an
ordinal column, which is Option B′.

### Option B′ — an ordinal column plus `UNIQUE (run_id, revise_ordinal)` (REJECTED)

Enforces **uniqueness**, not **the cap**. The ordinal is still computed from the stale
snapshot, so the loser gets a `23505` rather than zero rows, and when the loser's ordinal
is legitimately under the cap it is a *correct* insert that merely collided — so it needs a
**retry loop**. That means `pgerrcode` handling in `SubmitInput`, changed error mapping at
both entry points, and an ordinal backfill over existing rows. Strictly more surface than
D1, for strictly less certainty.

### Option C — SERIALIZABLE with retry, and Option D — lock in statement 1, count in statement 2 (BOTH REJECTED, same seam)

Option D is the textbook fix and it genuinely works: under READ COMMITTED a *new*
statement takes a *new* snapshot, so a count issued after the lock is granted sees the
winner's committed row.

Both die on a constraint this repo already recorded deliberately, in
`queries/runtime.sql`'s `CreateStopVerdictInput` comment:

> "workersvc.Store exposes no transaction seam, so this single combined statement IS the
> atomicity."

`internal/workersvc` contains **zero** occurrences of `pgxpool`, `Begin(`, or `WithTx`
outside tests; `Store` is a pure Querier interface. C and D both require adding a
transaction seam to that boundary and leaking `pgx.Tx` into the two fakes that implement
it — a structural change to a deliberately narrow interface, shipped inside a defect fix.
C additionally imports 40001 retry handling into two callers for the benefit of one query.

Neither is wrong in the abstract. Both are the wrong size for this.

## What is measured and what is reasoned

Recorded separately because an ADR that flattens the two is how a reasoned claim gets
inherited as a measurement.

**Measured** — instrumented runs against a real Postgres:

- The shipped statement goes over cap **25/25** with the interleave forced, and again
  **100/100** under a second, independent instrument. The inverted sequential control is
  clean 25/25.
- The counter form with the predicate in the UPDATE's qual: **0/100** over cap under the
  identical interleave.
- Option A′ (a real `UPDATE` in place of `FOR UPDATE`): **still over cap**.
- **The promoted regression test's two assertions hold unchanged under the new statement**
  — `TestReviseCapForcedInterleaveLiveDB` reports `landed=1 persisted=3 cap=3` where the
  pre-fix statement reported `landed=2 persisted=4`, **10/10 runs**, each carrying its own
  lock confirmation, with the sequential control green. This was a *prediction* when the
  design was written and is now a measurement; it is listed here rather than below for
  exactly that reason.
- **The reproducer's forcing device transfers unchanged**, which was not guaranteed and
  matters to anyone maintaining the test: the blocked caller still parks on the same
  `runs` row lock — now taken by the query's own UPDATE rather than by a `FOR UPDATE` —
  so the `pg_stat_activity` confirmation still fires and still discriminates a real block
  from a slow run. Every one of the 10 runs above reported it.
- **A cap predicate moved onto the INSERT leaves both concurrency tests green** (D2). The
  differential test is the only instrument that sees it.
- Static facts re-derived in the tree: the FK topology and the total absence of any
  `DELETE` against either table; that `CreateRunReviseInputIfUnderCap` is the **sole
  writer** of `revise_plan` rows; that `workersvc` has no transaction seam; that
  `CountRunReviseInputs` has no production call site; that `approval_idle` is timed off
  `runs.updated_at`; and that no handler marshals `store.Run`, so adding a column is not
  an API contract change.

**Reasoned, not measured** — believe this one notch less:

- **The EPQ explanation itself.** It is consistent with every measurement above and with
  the documented READ COMMITTED semantics, but no experiment isolated it: the runs show
  *that* the counter form holds and the row-counting form does not, not *why*. A validated
  prediction does not promote its own explanation, and this list would be worthless if it
  let one.

## Consequences

- **The cap's source of truth moves off the rows onto the run.** `count(*)` is now an audit
  view, and its lifetime semantics are preserved exactly: a *consumed* revise still counts,
  because the counter never decrements either.
- **Accepted residual, stated honestly — a new failure mode is created.** Before this
  change, *any* writer of a `revise_plan` row was counted by construction. Now only writers
  that go through `CreateRunReviseInputIfUnderCap` are. A second writer added later would
  defeat the cap **silently**, and no type checks it. Today there is exactly one writer
  (measured) and the two entry points both funnel through `SubmitInput`'s dedicated branch.
  **What guards that is partial, and the parts were measured 2026-07-29 rather than
  reasoned about** — an earlier version of this bullet named
  `TestReviseCountMatchesRowCountLiveDB` as "the guard", which it cannot be: it lives in
  `store_test`, never imports `workersvc`, and writes only through the capped query, so a
  writer added in `workersvc` or `handler` is structurally invisible to it.

  | second-writer shape | caught by |
  |---|---|
  | added inside `SubmitInput`'s `revise_plan` branch, or replacing the capped call | `workersvc.TestSubmitInputRevisePlanEnqueuesPlain` (pre-existing; measured catching it) |
  | a new SQL query inserting a `'revise_plan'` literal | `store.TestOnlyOneQueryInsertsRevisePlanRows` (added with this fix; positive control measured) |
  | added elsewhere in Go, reusing the generic `CreateRunInput` query | **nothing** — measured leaving `go test -count=1 ./...` fully green (43 packages ok, 0 FAIL) |

  The third row is a genuine hole, recorded rather than papered over: `00074`'s CHECK
  permits `'revise_plan'` through `CreateRunInput`, whose `kind` is a bare parameter, so
  the only thing between the two writers is a Go `if` with an early return. This is a real
  cost of this option and it is the price of not needing a transaction seam.
- **`store.Run` grows a field; no REST contract changes** — every runs DTO is hand-built.
- **`approval_idle` behaviour is unchanged**, deliberately (D4). A run whose gate has been
  open past the threshold and which then receives a revision stays flagged until the worker
  re-reports and `SetRunAwaitingApproval` bumps `updated_at` and clears health. That is
  today's behaviour verbatim, not a regression introduced here, and issue #182 is where it
  is fixed.
- **`docs/configuration.md`'s description of `PLAN_MAX_REVISIONS` stays true** — it
  describes the behaviour (a lifetime cap counting all persisted revisions), and the counter
  equals that count by construction. No churn owed.
- **The migration backfill is one-shot and load-bearing**; a wrong one resets every existing
  run's budget and nothing else in the system would notice.

## Adjacent decisions, recorded elsewhere on purpose

- **`approval_idle` flags runs whose human already acted** — surfaced as an open question by
  this design, filed as issue **#182**, and designed separately on 2026-07-29. **Explicitly
  out of scope for this decision**, and D4 is written so that #182 lands without unpicking
  anything here (see D4's closing paragraph for why the two compose).

  Two things a reader needs, because **the issue as filed is not self-correcting**:

  1. The fix is *not* to reset the clock on a revise (which D4 declines to do) but to
     return the existing `waiting_worker` state when the run carries a **gate verdict
     submitted at or after the gate opened**. The included kinds are exactly the four the
     worker's steering `route()` turns into a gate event or a cancel — `approve_plan`,
     `reject_plan`, `cancel`, `revise_plan`. **`follow_up` is excluded**: a follow-up at an
     open gate is a message and not an answer, so the run is still waiting on the human, and
     including it would let a chatty user permanently silence their own approval nudge.
  2. **The mechanism #182 originally specified — a pending row, `consumed_at IS NULL` — was
     refuted by measurement and replaced.** The worker drains `GET /inputs` every 3s while
     parked at the gate and the sweeper samples every 15s, so that predicate is true for
     roughly three seconds in the healthy case: it usually misses the condition entirely,
     and when it does catch it the flag flips back a tick later while `healthSince`
     restamps, making an hours-old gate report "flagged 0m ago". The replacement,
     `created_at >= runs.updated_at`, is monotone within a gate episode and independent of
     poll timing. **A reader who finds #182 in its filed state must not implement the
     version it describes**; the correction is recorded in the issue's own discussion.
- **The four guardrail layers are untouched.** Nothing in this change affects what a worker
  may do, only whether a revision is accepted.
- **The mechanism and the subquery prohibition also live in the code**, in the migration and
  query comments, where someone editing the statement will actually be looking. This ADR
  carries the alternatives and the epistemics; the comments carry the rule. Neither is a copy
  of the other.

## Linked from ARCHITECTURE.md

Discharged in the same commit that added this file: the plan-approval-gate paragraph's
`PLAN_MAX_REVISIONS` sentence now links here, alongside its existing PRD #41 link, matching
how `0035`, `0042` and `0065` are reached. ARCHITECTURE.md's own claim — the cap is
"enforced both server- and worker-side" — stays true and needed no edit.
