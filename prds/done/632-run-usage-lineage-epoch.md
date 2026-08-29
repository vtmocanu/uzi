# PRD #632 — run_usage: fix broken-resume-lineage undercount via `lineage_epoch`

**Issue**: #632 (supersedes #332, the design tracker)
**Status**: Implemented (M1–M6 landed; committed ADR is `adr/0632-run-usage-lineage-epoch.md`)
**Priority**: Medium
**Effort**: medium (5-6 milestones)
**Design of record**: ADR 0332 (in #332's pinned comment). This PRD restates the
load-bearing contract inline so an offline worker needs no external fetch.

---

## Problem

`run_usage` is the per-run token/cost ledger (PRD #40). Each delivered **result
frame's** cumulative `modelUsage` is folded into a row keyed `(run_id, session_id,
model)` and merged with `GREATEST`; the `run_usage_totals` view rolls up **MAX per
`(run_id, model)` across session rows → SUM across models**. MAX-across-sessions is
deliberate under PRD #40 Decision 3 verdict-b: within one unbroken accumulator
lineage the result frame reports cumulative-to-date, so session rows are *snapshots*
of one accumulator and a plain SUM would multiply them.

**The bug.** MAX-across-sessions is correct only while every session row belongs to
one unbroken lineage. When a worker restart **fails to resume** (SDK transcript not
resolvable on this worker → a **fresh** SDK session starts), the fresh session's
accumulator restarts at 0, its folds land in a **new** `(run_id, new_session_id,
model)` row accumulating from 0, and the view's `MAX per (run_id, model)` silently
**masks the smaller leg** — the run undercounts by up to a full leg. Both legs have
distinct `session_id`s, so `session_id` alone cannot tell an independent leg (should
SUM) from a within-lineage evolved-session snapshot (should MAX): only the worker
knows a resume was dropped.

**Measured, live (2026-08-23).** Run `0a0ea841-a46f-4b38-a286-3d69c52c343b` (issue
#602) hit the Anthropic five-hour session limit, parked (`limit_wait`), was re-claimed
by a **different** worker before the reset, could not resolve the prior session
(`resume_lineage_break` emitted), and latched a fresh session. Ledger `usage.cost_usd`
= **$64.74**; sum of per-turn result-frame `total_cost_usd` across the full run =
**$321.66**. Two distinct SDK sessions from the cross-worker break; `MAX` masked a leg.

**Frequency (why now).** #334 shipped a counter (`payload.event =
'resume_lineage_break'` in `run_messages`) on 2026-08-16. As of 2026-08-23: **5
break events across 5 distinct runs / 332 runs** created since the counter went live
(~1.5%), spread across the whole window (Aug 17 ×2, 21, 22, 23), and correlated with
the highest-cost runs (5-hour usage-limit parks that outlive worker affinity, not
rare crash edges). This clears the "is it material" gate that #332 was deferred on.

**Scope of the win.** Only *cost/token totals* are affected, and only the achievable
part: full resume **legs**. A never-completed turn's cost is permanently
unrecoverable (cost exists only at result-frame boundaries in the CLI's process-global
accumulator `qt`; there is no mid-query cost signal — per-call `message.usage` is
tokens-only and a different scale, 2.5-3.2× per #195, so it cannot be merged). This
PRD captures whole legs, not sub-turn tails. `run_usage` is advisory telemetry, **not
billing** — nobody is charged from it.

## Solution — Option B (defaulted `lineage_epoch`, keep `GREATEST`, MAX-within-epoch then SUM-across-epochs)

Add a per-leg marker so the view can SUM independent legs while still MAX-collapsing
snapshots inside one lineage:

- The worker already knows at `agent/src/runner.ts:657-684` that it dropped the
  resume and emits a `status` message with `payload.event = 'resume_lineage_break'`
  (`RESUME_LINEAGE_BREAK_EVENT`, `runner.ts:2762`, landed by #334). **No new endpoint
  and no new worker signal** — Option B reuses this existing emission.
- The **API** increments the run's epoch when it ingests that event, and stamps the
  current epoch onto every folded `run_usage` row.
- The **merge** is unchanged: `GREATEST` per `(run_id, session_id, model)` stays, so
  at-least-once re-delivery stays trivially idempotent.
- The **view** rewrites to `MAX within (run_id, model, lineage_epoch)`, then `SUM
  across epochs and models`. Snapshots within one lineage (same epoch, possibly
  evolved `session_id`) still MAX-collapse; independent legs (distinct epoch) now SUM.
- **Existing rows** default to `epoch = 0`, so single-lineage runs (the vast majority)
  are byte-for-byte unchanged → **zero historical restatement**. Historical
  resume-leg runs keep their (undercounted) numbers — forward-only and honest, since
  the marker was never recorded for them.

Option A (additive-delta ledger, immutable events, SUM rollup) was **rejected** in the
ADR: it still needs this same marker, plus a fragile delta/reorder idempotency story,
plus a retroactive restatement of past totals it cannot cleanly decompose.

### Load-bearing precondition — the two legs MUST persist under distinct `session_id`s

The fix works **only** because a dropped-resume fresh leg lands in a *separate*
`run_usage` row from leg 1, and that separation comes from `session_id`, **not** from
the epoch: the `UpsertRunUsage` conflict key is `(run_id, session_id, model)` and does
**not** include `lineage_epoch`. So `session_id` is the **row-splitter**; the epoch
only prevents *over-counting within* a lineage (see below). If two legs ever shared a
`session_id`, `GREATEST` would collapse them at the **row** level before the view ever
runs, and the epoch stamp would be inert.

They do get distinct session_ids, via a structural chain (verified 2026-08-23 at HEAD;
re-derive before relying on it):

- The break is emitted at `runner.ts:657-684` inside the per-claim handler, **before**
  `executor.run(ctx)` (`runner.ts:1131`), and only when the prior transcript is not
  resolvable here — i.e. by construction a fresh `executor.run()` with a fresh
  `RunDrive` state.
- `onSessionId` fires `reportState({status:"running", session_id: B})` only
  `if (!state.reportedSessionId)` (`sdk-executor.ts:2129-2130`); `state` is created
  once per `run()` and `reportedSessionId` is **never reset**, so a fresh `run()`
  reports the fresh id B.
- `SetRunRunning` writes `session_id = COALESCE(narg, session_id)` (`runtime.sql:845`),
  which **overwrites** to B (the runs row is not write-once at the SQL layer).

So break ⟹ fresh `run()` ⟹ `onSessionId(B)` fires ⟹ `runs.session_id` re-latches to B;
leg 1 sits under `(run, A, model)`, leg 2 under `(run, B, model)`. **M5 must assert the
two legs actually persist under distinct `session_id`s in `run_usage`** — not only
distinct epochs — so a future change to the `onSessionId`-once latch or the COALESCE
cannot silently break the fix with every epoch-only test still green.

### Why `lineage_epoch` and not just "SUM across session rows"

Given the two legs are already distinct rows, why not just SUM across session rows and
drop the epoch? Because verdict-b allows `session_id` to evolve turn-to-turn *within*
one unbroken lineage while carrying the cumulative forward — so two rows can be
snapshots of ONE accumulator. A plain SUM across session rows would double-count those
within-lineage snapshots (the exact multiply the current MAX exists to prevent). The
epoch is what distinguishes "reset to 0" (new epoch → SUM across) from "carried
forward" (same epoch → MAX within). `session_id` splits rows; the epoch groups them
correctly.

## Confirmed code anchors (verified 2026-08-23 at HEAD; re-derive before editing)

| What | Location |
|---|---|
| Totals view (MAX→SUM rollup to rewrite) | `api/internal/store/migrations/00063_run_usage_totals_view.sql` |
| `UpsertRunUsage` (GREATEST merge, gains the column) | `api/internal/store/queries/runtime.sql` (`-- name: UpsertRunUsage`, ~line 1899) |
| Fold (stamps the epoch) | `api/internal/workersvc/service.go` `func (s *Service) foldRunUsage` (~line 2921) |
| Worker break emission (already exists, reused) | `agent/src/runner.ts:657-684`; const `RESUME_LINEAGE_BREAK_EVENT` at `:2762` |
| Live-DB contract test to extend | `api/internal/store/run_usage_integration_test.go` `TestUpsertRunUsageMergeLiveDB` (broken-lineage case near the `sess-2` fold, ~line 121) |
| Migration head at drafting | `00158_self_improve_dedup_per_repo.sql` → draft new migrations as **00159+**, rename to the next free number above the applied head at landing (strict goose; `check:migration-numbering` gate enforces no duplicate prefix) |

## Milestones

- [x] **M1 — Schema: `lineage_epoch` columns + migration(s).** Add
  `run_usage.lineage_epoch INT NOT NULL DEFAULT 0` and a per-run epoch source
  `runs.lineage_epoch INT NOT NULL DEFAULT 0` (a counter bumped on each break; a `runs`
  column avoids a per-fold subquery in the hot path). Draft `00159+`; **both this and
  M4's view migration renumber above the applied head at landing, and their order is
  load-bearing — the `run_usage.lineage_epoch` column must exist before the view that
  groups on it** (put the column in an earlier-numbered migration than the view
  rewrite). Regenerate sqlc (`sqlc generate`, pinned v1.30.0). Existing rows default to
  0 → no historical restatement. `gate:repo` `check:migration-numbering` green.

- [x] **M2 — Server: bump the run epoch on the break signal (new ingestion code).**
  Today `resume_lineage_break` is parsed **nowhere** server-side (the #334 counter is
  an offline SQL scan, not an ingestion hook), so this milestone adds **new
  parse-and-act logic in the `appendMessages` hot path**: scan for a status message
  with `payload.event == 'resume_lineage_break'` and increment `runs.lineage_epoch`
  once per event via a **new sqlc query** (regen). No new endpoint / no new worker
  signal, but this is genuinely new server code, not "free." Specifics the code
  demands:
  - **Dedupe on `inserted`, NOT the `msgs` slice.** The append loop already tracks
    genuinely-new `(run_id, seq)` rows in `inserted` (rows>0, `service.go:2801-2805`),
    while `foldRunUsage` deliberately runs over **all** `msgs` so re-deliveries re-fold
    idempotently. The epoch bump is the opposite: bump **only for break events present
    in `inserted`**, or an at-least-once re-delivery double-bumps. Name `inserted`
    explicitly — do not share the fold's `msgs` pass.
  - **Extend the `appendMessages` audit** (`service.go:2631-2671`): its 🔴 comment
    requires any new store call that writes a worker-influenced value on this path to
    be revisited there in the same change. The bump takes only `run_id`+`int` (no
    worker-controlled text), so it clears the audit — but M2 must record it as a
    cleared suspect, in the same change.

- [x] **M3 — Fold: stamp the current epoch (and pin it on conflict).** `foldRunUsage`
  reads the run's `lineage_epoch` and passes it to `UpsertRunUsage`; the query gains a
  `lineage_epoch` column in the INSERT (regen). The `GREATEST` merge and the
  `(run_id, session_id, model)` conflict key are **unchanged** — the epoch is a stamped
  attribute, not part of the key (the fresh leg's distinct `session_id` is the
  row-splitter, per the precondition above). **Conflict behavior for `lineage_epoch`
  must be specified: OMIT it from the `DO UPDATE SET` clause (pin-to-first-insert).**
  Leg-1 frames are not re-delivered across a break (that leg's worker/batcher is gone),
  so pin-to-first is safe and cannot migrate a row's epoch back into a colliding group;
  `lineage_epoch = EXCLUDED.lineage_epoch` is the wrong choice (a late re-fold could
  re-collapse two legs). **Epoch visibility:** `foldRunUsage` receives the `run` struct
  fetched once at `appendMessages` top (`service.go:2673`), so a bump made *within the
  same call* is invisible to that fold. This is safe **only** because the break message
  arrives in an *earlier* append batch than the fresh leg's result frames (break
  emitted at `run()` start; result frames only at phase boundaries), so the later batch
  re-fetches `run` with the epoch already committed. State this temporal invariant in
  the code; do not describe a same-pass interleaving that does not exist. (A defensive
  alternative — re-read the run's epoch inside `foldRunUsage` — is acceptable if the
  implementer prefers not to rely on the batch-ordering invariant.)

- [x] **M4 — View rewrite: MAX-within-epoch then SUM-across-epochs.** New migration
  (a view cannot be `ALTER`ed; DROP + CREATE — numbered *after* M1's column migration)
  rewriting `run_usage_totals` so the inner grouping is `(run_id, model,
  lineage_epoch)` with `MAX`, and the outer stays `SUM` per `run_id` across the
  per-`(model, epoch)` maxima. Single-lineage runs (all epoch 0) produce identical
  output to today. Update the view's header comment to state the epoch rule.
  Regenerate sqlc and confirm every dependent read query still compiles against the
  recreated view (`validate:api` asserts regen is a no-op).

- [x] **M5 — Tests (live-DB + fake-store + worker).**
  - **View-read broken-lineage assertion (the one that proves the fix) belongs in
    `TestUsageRollupsLiveDB`, which reads `run_usage_totals` via `GetRunUsageTotal` —
    NOT in `TestUpsertRunUsageMergeLiveDB`, which reads raw `run_usage` rows and
    recomputes MAX/SUM in its own in-test SQL** (that test would prove nothing about
    the rewritten view, and a green in-test SQL could mask a wrong view). Seed leg 2
    under a distinct `session_id` **and** distinct `lineage_epoch`, starting *below*
    leg 1; assert the view **SUMs both legs**. Its `insUsage` helper needs the
    `lineage_epoch` column threaded through. Positive controls mandatory: `--- PASS`,
    `RUN>0`, **zero SKIP**; store-IT runner only (`test:api-store-it`).
  - **`TestUpsertRunUsageMergeLiveDB`**: extend only for the raw-row merge with the new
    column, and **assert the two legs persist under distinct `session_id`s** (guards the
    row-splitter precondition, not just epochs). Its `fold()` helper gains an epoch
    param.
  - **Name `TestUsageRollupsLiveDB` staying green unchanged (it seeds only epoch-0
    rows, exact totals) as the byte-identity / no-restatement regression guard** — that
    is the clean proof of Success Criterion 3, not the merge-test extension.
  - Fake-store service test: `foldRunUsage` stamps the run's current epoch onto
    upserts, and the epoch bumps exactly once on a newly-inserted `resume_lineage_break`
    message (and NOT on a re-delivered one).
  - Worker `node --test` (`agent/test/runner-resume-preflight.test.ts`): assert the
    #334 emission still fires and carries `event: 'resume_lineage_break'` (guards the
    signal Option B now depends on).

- [x] **M6 — Read surfaces, ADR, docs.** Confirm **every** total read path goes
  through `run_usage_totals` — `ListRunsForUser`, `GetRunUsageTotal`, `SelfUsage`,
  `AdminUsagePerUser`/`AdminUsageTotals`, and `judge.sql`'s LEFT JOIN (needs no change:
  the view's output columns are unchanged) — so all consumers, incl. `api/cmd/uzi`
  `uzi run get` (transitively via the run-detail DTO), get the corrected total with no
  per-caller change. `lineage_epoch` stays internal (not surfaced in DTOs). **Write a
  real committed ADR adr/0632-run-usage-lineage-epoch.md** capturing the shipped
  contract (there is no `adr/0332` file — #332's "ADR" is a GitHub issue comment, not a
  committed ADR; per convention ADRs are `adr/NNNN-slug.md` numbered by originating
  issue). Update `docs` if any user-facing usage page describes the rollup.
  *(Marking #332's Option B "implemented" is a forge write to another issue — a
  maintainer follow-up, out of an offline worker's branch diff; see below.)*

## Out of scope (and why — an offline worker cannot do it)

- **Validating the non-dropped-resume reseed assumption** (PRD #40 Decision 3
  verdict-b, PROVISIONAL). Option B trusts "worker did **not** drop the resume ⇒ the
  CLI accumulator `qt` reseeded, so those rows are one lineage (same epoch)." If `qt`
  silently fails to reseed on a *non-dropped* resume (home present but session
  mismatch/fork), that leg is mis-marked as a continuation and still undercounts.
  Confirming this needs a **live two-phase-across-requeue run with a real Anthropic
  session** (the PRD #40 Decision 3 procedure) — which needs open Anthropic egress and
  a live cluster, **not something an offline sweep worker can perform**. The
  **dropped-resume path this PRD implements and tests is safe regardless** (it is the
  detectable, dominant, measured case). The reseed validation is a separate
  maintainer live-run follow-up; do not add a milestone that depends on it.
- **Reducing the frequency of lineage breaks** (making cross-worker resumes actually
  resume). That is #628's job and is orthogonal: Option B makes the ledger correct
  *when* a break happens, whatever the rate.
- **Recovering a never-completed turn's cost** — permanently unrecoverable (no
  mid-query cost signal exists). Not attempted.
- **Marking #332's Option B "implemented"** is a forge write to another issue, not a
  repo-file change, so it is outside an offline sweep worker's branch diff — a
  maintainer follow-up once this lands, alongside the reseed-validation live run.
- **The web client-side usage fold stays epoch-unaware (discovered during M6, deferred).**
  `web/src/lib/runUsage.ts` (ADR-195) derives the run-page usage strip from the message
  stream with a per-model running high-water max; the stream carries no `session_id`/
  `lineage_epoch`, so on a broken-lineage run it still MAX-masks leg 2. Because #632 fixes
  only the *server* rollup, the fixed server total (`GetRunUsageTotal`) now **diverges**
  from that client strip on broken-lineage runs — a new inconsistency (the two agreed
  before #632 because both undercounted), and ADR-195's "cannot diverge by mechanism"
  invariant no longer holds for that case. Left as a maintainer follow-up (a feasible fix:
  reset the client's per-model baseline on the `resume_lineage_break` event it already
  sees, plus a broken-lineage contract fixture and a `runUsage.ts` comment fix). Recorded
  as an incidental finding and documented as a known limitation in `adr/0632`.

## Success criteria

1. A run with a genuine broken lineage (two legs under **distinct `session_id`s** and
   distinct epochs) reports, **read through `run_usage_totals`**, the **SUM** of both
   legs' per-model maxima; a single-lineage run's total is **value-identical** to today
   (all epoch 0, MAX-then-SUM over the typed view columns). Proven by the view-read
   assertion in `TestUsageRollupsLiveDB` with positive controls.
2. Re-delivering any result frame or the `resume_lineage_break` message changes no
   total (idempotency preserved: `GREATEST` untouched; epoch bump fires only for break
   events in `inserted`, never on re-delivery).
3. No historical restatement: existing `run_usage` rows are untouched; every past
   single-lineage run's total is unchanged — guarded by `TestUsageRollupsLiveDB`
   staying green unchanged (it seeds only epoch-0 rows and asserts exact totals).
4. Full `task gate:api` green (incl. `-race`, live-DB store-IT where applicable) and
   `gate:agent` green; migration numbering gate green.

## Risks

- **Stale-snapshot epoch visibility** (M3): `foldRunUsage` reads `run.LineageEpoch`
  from the `run` struct fetched once at `appendMessages` top, so an epoch bump made
  *within the same call* is invisible to that fold. It is safe **only** because the
  break message arrives in an *earlier* append batch than the fresh leg's result frames
  (break emitted at `run()` start; result frames only at phase boundaries). State this
  temporal invariant in code, or defensively re-read the epoch inside `foldRunUsage`.
  Do not describe a same-pass interleaving — the fold is a single terminal pass, not
  interleaved per message.
- **Idempotency regression** (M2): a naive "increment on every break message" double-
  counts a re-delivered break under at-least-once delivery. Bump only for break events
  in `inserted` (rows>0), never over the fold's `msgs` slice.
- **`ON CONFLICT` epoch handling** (M3): setting `lineage_epoch = EXCLUDED.lineage_epoch`
  in the upsert would let a late re-fold migrate a row's epoch and re-collapse two legs
  into one group (masking returns). Omit `lineage_epoch` from `DO UPDATE SET`
  (pin-to-first-insert); leg-1 frames are not re-delivered across a break, so this is
  safe. Load-bearing, must be explicit.
- **Row-splitter precondition** (design): the fix depends on the two legs persisting
  under distinct `session_id`s (break ⟹ fresh `run()` ⟹ `onSessionId` re-latch ⟹
  COALESCE overwrite). A future change to the `reportedSessionId`-once latch or the
  `SetRunRunning` COALESCE would silently break it — hence the distinct-`session_id`
  test assertion in M5, not just an epoch check.
- **Session-id-reporting timing** (pre-existing, inherited): `reportState(session_id)`
  and the fresh leg's first result-frame append are separate async calls; if the id
  write were delayed past that frame's append, the frame would fold under the old
  session/epoch and mask. A full phase separates them so the window is negligible, but
  it is not transactionally closed — inherited from today's session-id reporting, not
  introduced here.
- **View migration** must DROP + CREATE (no ALTER for a view), numbered after M1's
  column migration — verify all dependent read queries still compile against the
  recreated view (sqlc + gate).
- **Riskiest assumption** is the non-dropped reseed (see Out of scope) — explicitly
  deferred to a maintainer live run; the shipped path does not depend on it.

## Workflow-scope note

This PRD touches **no** `.github/workflows/**` file, so it is safe for a uzi sweep
worker (the worker PAT lacks `workflow` scope; any workflow-file touch in the branch
diff is an atomic push rejection). Keep it that way — no CI-workflow edits in the
implementation or validation.
