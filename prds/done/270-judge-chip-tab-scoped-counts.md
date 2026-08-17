# PRD #270: Judge filter-chip counts scope to the selected triage tab

**GitLab Issue**: [#270](https://github.com/vtmocanu/uzi/-/issues/270)
**Status**: ✅ Done — implemented 2026-08-09 (all milestones M1–M6 landed and reviewed on branch `agent/issue-270`; backend matrix + uncapped SQL, frontend tab-scoped refetch, mock parity + differential test, docs + specs §506)
**Priority**: Medium

## Problem

On the Judge triage page (`web/src/pages/Judge.tsx`), each label filter chip carries a
count. Today that count is **whole-backlog and triage-invariant** (PRD #244): "Improve an
agent 12" means twelve `(category, target)` groups exist across *every* triage state — To
triage, Filed, Done, Dismissed — regardless of which tab is selected.

But the group list directly below the chips is scoped to the **selected tab**. So under
**To triage**, the chip reads `12` while the list reads "Showing **3** groups matching
Improve an agent" — the other 9 groups are in Done/Dismissed. The number next to a filter
does not predict how many results that filter yields in the current view, which is the one
thing a facet count is expected to do. The owner hit this and read it as a bug; it took
reading the source to confirm it was deliberate.

The whole-backlog design was a real choice — it is fetched once, never moves, and answers
"how much of this kind exists anywhere." But on a *triage* page, predictiveness beats the
portfolio signal, and the portfolio signal is still available on the **All** tab.

## Solution Overview

Make each chip count reflect the **currently selected bucket tab**, so the chip predicts
the visible group list:

| Tab | `Improve an agent` chip |
|---|---|
| To triage | 3 |
| Done | 9 |
| All | 12 |

`All` keeps today's whole-backlog number; the four settled/open buckets partition it
(`todo + filed + done + dismissed == all`, per category), because every group rolls up to
exactly one bucket.

**The load-bearing constraint is *how* the per-bucket count is computed.** A group's bucket
is not a column — it is a Go-side rollup (`GroupJudgeRecommendations`,
`judge_backlog.go:214`): "todo when any member is open, else the group's highest settled
rung." The codebase forbids re-expressing that ladder in SQL (PRD #94 Decision 2 — "the
second ladder"), and the current chip-count endpoint's cheap `COUNT(DISTINCT category,
target)` **cannot** produce per-bucket counts without reintroducing exactly that forbidden
second ladder. So the count must be derived from the **same grouping pass** the list uses,
guaranteeing chip and list agree by construction.

Shape of the change:

- **Backend** (`api/internal/workersvc/judge_disposition.go`, `judge_backlog.go`): replace
  the SQL `COUNT(DISTINCT)` aggregate with a pass that loads the caller's whole-backlog
  flat rows **uncapped**, runs `GroupJudgeRecommendations`, and tallies group rollups into a
  **bucket × category matrix**. It reuses the same query and the shared rollup as the backlog
  — but **not the backlog's row cap** (`JudgeBacklogMaxRows + 1 = 2001`, `judge_backlog.go:143`).
  The current chip count is deliberately **uncapped** so a chip reads the true count even when
  the list truncates, and a live-DB test pins exactly that
  (`TestJudgeCategoryStatsUncappedAndDedupedLiveDB` seeds a 50-group category past the 2000-row
  cut and asserts the chip reads 51 while the truncated list shows 0). That guarantee must
  survive: the matrix load runs with no `Lim`. No new SQL ladder.
- **Wire** (`api/internal/apitypes/review.go`): extend `JudgeCategoryStatsDTO` from a flat
  `{category: count}` map to a bucket-keyed `{bucket: {category: count}}` matrix (Decision 3).
- **Frontend** (`web/src/pages/Judge.tsx`, `lib/api.ts`): fetch the matrix, index it by the
  active bucket (`counts = matrix[bucket] ?? {}`), and pass that to `LabelFilter`. Because
  counts are **no longer triage-invariant**, refetch the matrix after disposition actions
  (mark done / dismiss / undo / file) and on run-anchor change — but **not** on bucket
  change (the matrix already holds every bucket) and **not** on category change (facet
  independence, Decision 4).
- **Mock** (`web/src/mocks/mockApi.ts`): `computeCategoryStats` recomputes the same matrix
  from mock data so mock-mode and live agree.
- **Docs** (`docs/judge-menu.md:132-142`): the paragraph that currently states the count is
  "your whole backlog — every bucket, every triage state" and "switching bucket tabs ...
  doesn't move it" is now false and must be rewritten.

This is a **display + aggregation** change: no run-lifecycle, sweeper, claim, or
disposition-write path is touched. The tab totals themselves (`To triage 45`, recommendation
level) are unchanged.

## Design Decisions

### Decision 1 — Chip count is scoped to the selected tab; `All` keeps today's number
Owner decision (2026-08-09), chosen over "show both (3 / 12)" and "keep whole-backlog +
clarify UI". Rationale: matches faceted-filter convention (a facet count predicts results
in the current scope) and removes the exact 12-vs-3 confusion. The portfolio view survives
on the `All` tab.

### Decision 2 — Counts come from the shared rollup, never a second SQL ladder
The per-bucket count MUST be computed by running `GroupJudgeRecommendations` over the
whole-backlog rows and tallying group rollups per bucket — the same function that produces
the rendered list. A SQL `GROUP BY disposition_status` would compute a *different* bucket
than the Go rollup (which promotes any group with an open member to `todo`), silently
disagreeing with the list. This is the invariant PRD #94 Decision 2 protects ("the strip and
the bar agree").

**Uncapped, so not "the backlog's load."** The backlog caps its row load at 2001
(`judge_backlog.go:143/178`); this matrix must load uncapped (see Solution Overview + Risks
→ Cost), because #244's pinned guarantee is that a chip is exact even when the list truncates
— and a capped load additionally produces *wrong* rollups (a group whose only open occurrence
fell past the cut mis-rolls to a settled bucket). So this is the shared query and the shared
rollup, at a different (unbounded) `Lim` than the backlog — an important distinction the cost
accounting below owns.

### Decision 3 — Wire shape: bucket-keyed matrix, still a map so the taxonomy can grow
Extend `JudgeCategoryStatsDTO`. Options: (a) `counts_by_bucket: {todo: {cat: n}, filed:
{...}, done, dismissed, all}`; (b) keep `counts` (= `all`, back-compat) and add
`counts_by_bucket`. **Recommend (a)** — there is exactly one consumer (the web page) and no
CLI consumer (`grep` of `api/cmd/uzi` for `category-stats` is empty), so a clean break is
cheap and avoids shipping two representations of the same number. Keep the inner and outer
levels as **maps** (not fixed-field structs) so a new category or bucket does not break the
wire, preserving PRD #244 Decision 7. `all` is included as a bucket key so the frontend
indexes uniformly (`matrix[bucket]`) with no special case.

### Decision 4 — Facet independence: a category chip's counts ignore the active `?category=`
The matrix is computed over the caller's whole backlog with **no** `?category=` filter, so
selecting "Improve an agent" does not zero out the other chips' counts. This is standard
OR-facet behaviour and is why the count cannot simply be folded into the backlog response
(the backlog pushes `?category=` into SQL, so its loaded rows exclude other categories).

### Decision 5 — Run anchor: the matrix respects `?run=`, the category filter it does not
**Resolved (architect-endorsed): anchor-aware.** When the page is anchored to a single run
(`?run=`), the whole view is scoped to that run; the chip counts scope with it. The count
endpoint takes the same optional `run` anchor as the backlog and applies it
(`RunAnchor: nullableUUID(runAnchor)`), but never applies `?category=` (`Categories: nil`,
Decision 4). The two compose cleanly because they are different kinds of scope: `?category=`
is the facet the chips drive (applying it would zero sibling chips), while `?run=` is an
orthogonal view anchor.

Subtlety to preserve: the backlog's run anchor is a **semi-join** that keeps *all* of a kept
group's occurrences, not just the anchor run's (`judge_backlog.go:153-160`, mirrored in the
mock). So an anchored group buckets by its **cross-run** rollup, exactly as the anchored list
renders it — chip and list stay in agreement, which is the argument for anchor-aware. It also
means the cost concern (Risks → Cost) applies only to the **unanchored** whole-backlog matrix;
an anchored load is a handful of rows.

### Decision 6 — Counts are now triage-variant; refresh them on disposition, not on tab
Today the matrix would be wrong the instant a bulk "mark done" moves a group from `todo` to
`done`. So the frontend must refetch the matrix on the same triggers that re-read the
backlog after a disposition action (and on run-anchor change), but **not** on bucket-tab
change (all buckets are already in hand) and **not** on category toggle (Decision 4). This
reverses PRD #244's "fetched once on mount, never refetched" property, which the doc and the
`Judge.tsx` comments currently assert.

Two consequences to own:
- **Transient chip/list disagreement after a bulk action.** The bulk-dispose path reconciles
  acted-on rows *in place* and deliberately keeps them visible at their new rollup rather than
  yanking them (`Judge.tsx:270-280`, header contract `:12-14`). So right after "mark done" on
  the To-triage tab, the done card is still on screen while a refetched `todo` chip has already
  decremented — chip and visible list disagree by exactly the acted-on count until the next
  navigation/`load` heals it. Acceptable and self-healing, but it is a real transient the
  premise ("chip predicts the visible list") does not literally hold across, and it must be
  called out, not discovered.
- **A reintroduced round-trip.** The bulk response was designed to update rows + badge with
  **no** follow-up GET (`review.go:213-216`); a separate matrix refetch adds one back. The
  alternative — folding the matrix into `JudgeDispositionResultDTO` — avoids the GET but runs
  the uncapped grouping on *every write* (worse per Risks → Cost). The separate refetch is the
  chosen tradeoff, stated as a decision, not a non-event.

### Decision 7 — Zero-count chips still dim rather than hide, per-tab
The existing rule (PRD #235 open question 3) stays: a chip whose count is 0 **in the current
tab** is dimmed but kept visible and clickable, so the chip row is a stable, learnable set
across tabs. On the `Filed` tab (often all-zero) every chip dims — accurate, not empty.

### Decision 8 — Out of scope: the tab-total unit (recommendations) vs chip unit (groups)
The bucket **tab** totals (`To triage 45`) are recommendation-level (`TriageDTO` via
`BucketTriage`), while chips are group-level. That pre-existing units difference is not what
this PRD fixes and is left as-is; noted so review does not expect chip sums to equal tab
totals.

## Milestones

- [x] **M1 — Backend matrix aggregate (uncapped).** New/extended service method returns the
  bucket × category group-count matrix via `GroupJudgeRecommendations` over an **uncapped**
  whole-backlog row load (no `Lim` — see Decision 2/Solution Overview; a capped load silently
  regresses #244 and mis-rolls groups); handler updated; owner-scoped. Tests: (a) carry
  forward `TestJudgeCategoryStatsUncappedAndDedupedLiveDB`, **extended to the matrix shape**,
  so a category seeded past the 2000-row cut still reads its true `all` count — this is the
  regression M1's other assertions do NOT catch; (b) `todo+filed+done+dismissed == all` per
  category; (c) a group with one open member counts under `todo` only, and a fully-settled
  group counts under its highest rung. Gate: `task gate:api`.
- [x] **M2 — Wire + DTO.** `JudgeCategoryStatsDTO` becomes the bucket-keyed matrix
  (Decision 3, clean break); update `api/internal/handler/judge_stats_test.go`
  (`TestJudgeCategoryStatsResponseShape` asserts the flat `counts` key + builds a
  `CountJudgeGroupsByCategoryForUserRow` fake — both change) and the `apitypes` wire test.
  Confirm no other Go/CLI consumer (grep of `api/cmd/uzi` is empty).
- [x] **M3 — Frontend rewire.** `Judge.tsx` indexes the matrix by active bucket; `LabelFilter`
  unchanged (still receives `{category: count}`); matrix refetches on disposition/undo/file
  and run-anchor change, not on bucket/category change (Decision 6); aria-label still honest.
  Rewrite the fetch-once tests that Decision 6 reverses: `Judge.test.tsx` (the
  `getJudgeCategoryStats … toHaveBeenCalledTimes(1)` assertions) to assert refetch-on-dispose;
  update the `getJudgeCategoryStats` stubs in `JudgeNavBadge.test.tsx` to the matrix shape.
  Gate: `task gate:web`.
- [x] **M4 — Mock parity + differential test.** `computeCategoryStats` recomputes the matrix
  (this adds a *second* implementation of the "any open ⇒ todo, else highest rung" rollup, on
  the mock side). Add a **differential test** with a fixture that actually discriminates it:
  at least one category with groups spanning ≥2 *non-todo* settled buckets (exercises
  "highest rung"), one group mixing open + settled members (exercises todo-promotion at tally
  level), asserting per-category `todo+filed+done+dismissed == all` **and** that the fixture
  contains multi-bucket categories. Extend `fixtures/judge-fidelity` to the matrix; do NOT
  snapshot the demo (a demo snapshot locks in whatever spread the demo happens to have).
- [x] **M5 — Docs + comment sweep.** Rewrite `docs/judge-menu.md` chip-count paragraph
  (~lines 133-142: no longer whole-backlog, no longer "doesn't move on tab switch or
  mark-done"); keep the truncation and dimmed-zero notes, which still hold. Same-commit comment
  fixes for the "whole-backlog / triage-invariant / fetched once" claim wherever it lives:
  `web/src/lib/api.ts:1482-1490`, `apitypes/review.go:78-92` (moves with the type), `Judge.tsx`,
  `judge_stats.go`, `judge_disposition.go`, and the mock comments `mockApi.ts:340-354`/`:2762-2764`.
  Gate: `npm run build` (`check-docs.mjs`).
- [x] **M6 — Full gate + manual check.** `task gate` green; manually verify in mock mode that
  the "Improve an agent" chip reads 3 on To triage, 12 on All, and updates after a mark-done
  (assert the 3/12 as a fixture in M4, not by eyeballing M6).

## Success Criteria

1. Selecting a bucket tab updates every chip count to the number of groups **in that bucket
   for that category, whole-backlog and uncapped** — so on a non-truncated page it equals the
   rendered group count, and under truncation it stays the true count (may exceed the rendered
   cards), the same exact-vs-truncated relationship the tab totals already have. On `All` the
   chip equals today's whole-backlog number.
2. For every category, `todo + filed + done + dismissed == all` (pinned by test).
3. Counts are computed through the shared `GroupJudgeRecommendations` rollup over an uncapped
   load — no SQL query buckets dispositions, and the uncapped-past-2000 guarantee survives
   (grep proof + the carried-forward uncapped live-DB test + test that a group with an open
   member counts as `todo`, not under its members' settled rungs).
4. A category chip's count is unaffected by which categories are currently selected (facet
   independence).
5. After a bulk mark-done/dismiss/undo, the chip counts move to reflect the new bucket
   membership without a page reload.
6. `docs/judge-menu.md` no longer claims the count is whole-backlog or tab-invariant.

## Risks & Mitigations

- **Reintroducing the forbidden second SQL ladder.** The tempting shortcut (extend the
  `COUNT(DISTINCT)` with `GROUP BY disposition_status`) computes the wrong bucket. Mitigation:
  Decision 2 + Success Criterion 3's grep/test; route counts through the same Go rollup as the
  list.
- **Stale counts after disposition.** Losing triage-invariance means the once-on-mount fetch
  is no longer safe. Mitigation: Decision 6 refetch triggers; M6 manual check.
- **Cost — stated honestly (architect S1).** The endpoint moves from a ~6-row SQL aggregate to
  loading **every** recommendation row across the wire (uncapped, per B1/Decision 2) plus
  grouping them in Go. The DB spine cost is roughly comparable (both seq-scan the join spine),
  but wire + app cost goes from O(categories) to O(recommendations) — on a large backlog that
  is not "one backlog fetch" (wire-capped at 2001), it is much larger. Decision 6 compounds it:
  today one fetch on mount; after this, an uncapped load + grouping on every
  disposition/undo/file/run-anchor change, reintroducing per-action the unboundedness the row
  cap was added to prevent. This is owned, not hidden. Mitigations: the anchored view is cheap
  (few rows); the unanchored whole-backlog matrix is the concern, so if a live profile shows it
  hurting, options are a debounce on the post-dispose refetch, a server-side memo per
  (user, mutation-generation), or a bounded-but-honest cap that reports truncation. Direction
  stays right — there is no correct SQL alternative.
- **Wire break for an unknown consumer.** Mitigation: M2 confirms the web page is the only
  consumer (no CLI usage); a clean-break DTO (Decision 3a) is safe.

## Dependencies

- Builds directly on PRD #244 (the chip counts) and PRD #94/#98 (the disposition rollup and
  `GroupJudgeRecommendations`). No new services, schema, or migration.

## Documentation Impact

- `docs/judge-menu.md` — chip-count paragraph rewrite (~lines 133-142) (M5).
- Inline comments/doc-strings that currently assert "whole-backlog / triage-invariant /
  fetched once" must be corrected in the same commits that change the behaviour (repo
  fix-the-doc rule): `Judge.tsx`, `judge_stats.go`, `judge_disposition.go`,
  `web/src/lib/api.ts:1482-1490`, `apitypes/review.go:78-92` (moves with the DTO type), and the
  mock comments `mockApi.ts:340-354` / `:2762-2764`.
