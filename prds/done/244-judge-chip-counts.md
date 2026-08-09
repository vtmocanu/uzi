# PRD #244 — Per-label counts on the Judge filter chips

**Issue**: [#244](https://gitlab.example.com/vtmocanu/uzi/-/issues/244) · **Label**: PRD · **Priority**: Medium
**Parent**: [#235](https://gitlab.example.com/vtmocanu/uzi/-/issues/235) (the label filter itself) — this is the follow-up its Decision 6 named.
**Area**: a new `/me/judge/category-stats` aggregate — `api/internal/store/queries/dispositions.sql` (a sibling of `ListJudgeTriageRowsForUser`), `api/internal/workersvc/judge_disposition.go` (a `JudgeCategoryStats` method beside `JudgeTriageStats`), `api/internal/handler/judge_stats.go` (a sibling handler) + route (`handler.go` `/me/judge` group), `api/internal/apitypes/review.go` (a new DTO) + `api/internal/apitypes/wire_test.go` (its pinned tag test) · `web/src/lib/api.ts` (`getJudgeCategoryStats` + TS type), `web/src/pages/Judge.tsx` (render the count on each chip), `web/src/mocks/mockApi.ts` (parity).
**Mockup**: [`prds/mockups/235-judge-label-filter-mock.html`](mockups/235-judge-label-filter-mock.html) — the parent's mockup already draws the count badge (`.chip .n`, `countFor(cat)` at `:275`). This PRD ships what that badge illustrated.
**Line references** are against `f9306c8e`.
**Status**: done — implemented 2026-08-08 (#244, all milestones M1–M3 complete). Design reviewed 2026-08-08 (see [Review findings](#review-findings)); verdict sound-with-fixes, fixes folded in.

## Problem

PRD #235 shipped the Judge page's **filter-by-label** chips — a row of toggle
chips, one per recommendation category, that narrows the cross-run backlog by
label (`web/src/pages/Judge.tsx`, `LabelFilter` at `:546-596`). The chips carry
**no count**. A user scanning the backlog cannot see how many groups are behind
each label without clicking each chip in turn: "how many worker-tool installs am
I actually being asked for?" takes six clicks to answer across six labels.

The design mockup drew a count on every chip from the start
(`prds/mockups/235-judge-label-filter-mock.html`: the `.chip .n` badge, populated
by `countFor(cat)` at `:275`, `:286`). #235 **deliberately shipped without it** —
not an oversight, a sequencing decision. Its Decision 6 (`prds/done/235-judge-label-filter.md:359-361`):

> **No per-label chip counts in v1.** There is no canonical per-category
> aggregate, and tallying off the (truncated, bucket-filtered) groups is the
> anti-pattern the codebase forbids. A count is a deliberate follow-up with its
> own aggregate.

This PRD is that follow-up: add the canonical per-category aggregate #235 said a
count must have, and render it on the chips.

## Why a count cannot be tallied off the delivered groups

This is the whole reason #235 deferred it, and it is the load-bearing constraint
here, so it is stated first. The backlog the page already holds is the **wrong
source** for a count, in two independent ways:

1. **It is capped before grouping.** `JudgeRecommendationBacklog` pulls at most
   `JudgeBacklogMaxRows = 2000` rows (`api/internal/workersvc/judge_backlog.go:143`),
   applied **before** `GroupJudgeRecommendations` dedups them — the cap comment
   spells out that it "applies BEFORE grouping" (`judge_backlog.go:120`). So on a
   truncated backlog a whole category can sit past the cap and never appear in the
   delivered groups. A count tallied from `groups` would read low, or zero, exactly
   when the backlog is large enough to matter.
2. **It is bucket-filtered.** The delivered groups are already narrowed to the
   active `?bucket=` (`filterGroups`, `judge_backlog.go`), so a tally off them
   would silently become a per-bucket count, not a per-label one.

This is the same prohibition the triage counts already obey: the canonical triage
tally is the separate `/me/judge/stats` aggregate and is "NEVER tallied from
`groups`" (`web/src/lib/api.ts:1452`, the comment on `JudgeBacklog`'s `triage`
field; the `TriageCounts` type itself is at `:1317`). A per-label count must be a
real aggregate over the whole backlog, computed server-side, the same way
`/me/judge/stats` is — never derived from the on-screen list.

## Solution

Add a **canonical per-category count aggregate** and render it on the chips.

### What the count means: whole-backlog **group** count, all triage states

The count on each chip is the number of **recommendation groups** in that
category across the caller's whole backlog — i.e. distinct `(category, target)`
coordinates, the same unit the chip filters and the same unit the list renders as
cards. It is **not** a raw-occurrence (row) count, and it is **not**
bucket-scoped. Three consequences, each deliberate:

- **Groups, not rows.** The chip filters the *group* list, and each group is one
  card. A count of groups predicts "how many cards has this label", which is what a
  user reads a chip count to mean. This matches the mockup exactly
  (`countFor(cat) = GROUPS.filter(g => g.cat === cat).length` — a group count) and
  differs from the existing `/me/judge/stats` aggregate, which counts **rows**
  (`BucketTriage`, `api/internal/workersvc/triage.go`: `out.Total++` per
  recommendation row). We are adding a new aggregate, not reusing that one, and the
  unit is the reason.
- **Whole-backlog, ignoring the bucket tab.** The count is the same regardless of
  which bucket tab is active, exactly as the bucket-tab counts and the triage strip
  are whole-backlog and "already ignore the `run` anchor and truncation"
  (`prds/done/235-judge-label-filter.md:106-107`). Making the count *bucket-aware*
  would require computing each group's rollup bucket in SQL — "the second ladder #94
  Decision 2 forbids" (`prds/done/235-judge-label-filter.md:82-83`). So the count
  stays a flat per-category aggregate, disambiguated from the visible list by the
  "showing N of M" result line #235 already ships (open question 4 there).
- **All triage states, so the count is triage-invariant.** Because a group is a
  `(category, target)` coordinate, marking it done or dismissing it does **not**
  change the count — triage moves a coordinate between buckets, it does not add or
  remove the coordinate. This is verifiable at the schema: triage writes only
  `recommendation_dispositions`, filing writes only `recommendation_filed_issues`,
  and the only writers of the counted `review_recommendations` table are
  `UpsertRunReviewWithRecommendations` (`api/internal/store/queries/judge.sql`,
  the re-judge clear+insert) and one guarded `UPDATE addressed_by_run_id` in
  `selfimprove.sql` — which stamps a run link and never touches `category`/`target`,
  so it too cannot move a coordinate. The count therefore changes only when the
  recommendation set itself does (a new run's review lands, a review is deleted, a
  re-judge adds/drops a finding). This is a feature: the chip counts do not flicker
  as the user works the list, so they need no refetch after a triage action (unlike
  the nav badge, which reads the `todo` row-count and does refetch on navigation).

### It is exact and uncapped, which is the entire point

The aggregate is a `COUNT(DISTINCT target) … GROUP BY category` with **no
`LIMIT`** — like `/me/judge/stats`, which "has no `LIMIT` at all"
(`judge_backlog.go:141`). So the count is the *true* count even when the backlog
list is truncated: a chip can correctly read `6` while the capped list shows `4`
cards under the existing truncation banner (`Judge.tsx`). That divergence is not a
bug, it is the aggregate doing the job the delivered groups cannot — it is exactly
the case Decision 6 was protecting against.

### A separate endpoint, so the nav badge stays isolated

The count is served from a **new** endpoint, `GET /me/judge/category-stats`,
returning a **new** DTO — not folded into `TriageDTO` / the `/me/judge/stats`
response. Two reasons:

- **The nav badge must not be reachable from category data.** The sidebar badge
  reads exactly one field, `TriageCounts.todo`, polled from `/me/judge/stats`
  (`web/src/components/AppShell.tsx`, `setJudgeTodo(stats.todo)`). A separate DTO
  with per-label fields has no path to `setJudgeTodo` and cannot drive the badge.
  Widening `TriageDTO` would put a category dimension into the per-review DTO and the
  polled badge payload, which neither needs.
- **The badge fetch must not carry category work.** `AppShell` fetches
  `/me/judge/stats` for the badge on navigation — a `useEffect` keyed on
  `[user, location.pathname]` (`web/src/components/AppShell.tsx:733-748`), not a
  wall-clock timer — and also takes a push from the Judge page's own `triage.todo`
  via `JudgeTodoContext` after a triage action (`AppShell.tsx:690,883`). The category
  counts belong to the Judge page only and are fetched **once on mount** of that page.
  Keeping them on their own endpoint keeps the badge fetch untouched.

### Cost, stated honestly

The new aggregate runs over the same #94 join spine as `/me/judge/stats` and
inherits the same full-table seq scan the cap comment measured against a
145k-recommendation database (`judge_backlog.go:124-142`). It adds **one** such
read per Judge-page mount (not per filter toggle — the counts are
bucket/category-invariant — and not on the badge fetch, which is a separate
endpoint). A backlog request already runs two of these reads
(`judge_backlog.go:139-141`); this adds a third on Judge-page load only. As with the existing aggregates, an index is a #94-scoped
change with its own measurement and is **not** in this PRD's scope.

## User journey

1. A user opens **Judge**. The filter-by-label row now shows a count on each
   chip: `Install a worker tool 3`, `Improve uzi 6`, and so on — the number of
   groups behind each label across all their runs.
2. They see `Improve uzi 6` and click it. The list narrows to the `improve_uzi`
   groups in the current bucket (unchanged #235 behaviour); the chip counts do not
   move.
3. They switch the bucket tab to **Done**. The list changes; the chip counts stay
   put (whole-backlog), and the "showing N of M" line reconciles what is visible.
4. On a **truncated** backlog, a chip reads `Improve uzi 6` even though only 4
   `improve_uzi` cards render under the truncation banner — the count is the true
   figure, the list is capped.
5. They mark two `improve_uzi` groups done. The list shrinks in the To-triage
   bucket; the chip still reads `6` (a done group is still a group). The nav badge's
   to-triage count drops, as it always did.

## Open questions

### 1. Group count or occurrence (row) count? **Group count.** (recommended)
The chip filters groups and each group is a card, so a group count predicts what
clicking does; it is also what the mockup draws. An occurrence count would answer a
different question ("how many times was this raised") that the group's own "seen in
N runs" line already answers per row. Ship group counts.

### 2. Whole-backlog, or scoped to the active bucket? **Whole-backlog.** (recommended)
A bucket-scoped count would need each group's rollup bucket computed in SQL — the
forbidden second `BucketOf` ladder (#94 Decision 2) — or an uncapped Go grouping
pass per bucket. Whole-backlog is consistent with the bucket tabs and triage strip
(both whole-backlog), triage-invariant (no post-action refetch), and matches the
mockup. The "showing N of M" result line already tells the user what the current
bucket+filter actually shows. **Alternative for a later iteration:** a per-category
*to-triage* count (count only coordinates whose rollup bucket is `todo`), which is
more actionable and reuses the existing Go `BucketOf` ladder (no new SQL ladder) at
the cost of a Go grouping pass over the uncapped rows and a refetch after each
triage action. Deferred, not rejected — see Decision 5.

### 3. Show a count of 0, hide the chip, or dim it? **Keep all six chips, show the count, dim a true-zero chip.** (recommended)
#235 renders all six chips from the fixed `RECOMMENDATION_LABELS` keys
(`web/src/lib/judge.ts`), so the filter set is stable and learnable. The mockup
*hides* zero-count chips (`if (n === 0) return;` at `:280`), but hiding makes the
row jump as data changes and drops a learnable control. Recommendation: keep all
six visible, render the count including `0`, and **dim (and optionally disable) a
chip whose whole-backlog count is 0** so it reads as "none of this kind" rather than
inviting a click into the empty state. (A non-zero chip whose groups are all in
another bucket is not zero whole-backlog, so it will not dim — the dim is honest.)

### 4. Does the count respect the `?run=` deep-link anchor? **No.** (recommended)
When the page is anchored to one run (`?run=`, from a notification deep-link), the
triage strip and bucket tabs already stay whole-backlog and ignore the anchor
(`prds/done/235-judge-label-filter.md:106-107`). The category counts are the same
kind of aggregate and follow the same rule: whole-backlog, anchor-independent. Keeps
the aggregate a single flat query with no run parameter.

### 5. Does the CLI get per-category counts? **Defer, check recorded.** (recommended)
Per CLAUDE.md ("New uzi functionality ⇒ check whether `api/cmd/uzi/` needs a
matching CLI change"), the check is done here. `uzi review backlog` already filters
by `--category` (#235 M3) and prints the triage tally; surfacing per-category counts
in its header is a nice-to-have, not parity-critical, and the terminal user can
already pass `--category` to see a filtered list. Recommendation: **defer to a
follow-up**; the web chips are the feature the parent issue is about. If added later,
it is a header line off the same new endpoint.

### 6. A category present in data but absent from the client label map?
Same limitation #235 documented (its open question 6): the chips are built from the
fixed `RECOMMENDATION_LABELS` keys, so a not-yet-known category has no chip and its
count is simply not shown until the client map learns it. The aggregate keys by the
raw `rr.category` column, so it *would* return a count for an unknown category; the
client renders counts only for the categories it has chips for. Using a **map** DTO
(`counts: {category: n}`) rather than a fixed-field struct means the wire format does
not break when the taxonomy grows — the client reads `counts[cat] ?? 0` per chip.

## Technical scope

### SQL — a new aggregate (`api/internal/store/queries/dispositions.sql`)
Add `CountJudgeGroupsByCategoryForUser` beside `ListJudgeTriageRowsForUser`
(`:62-82`). It is the same owner-scoped join spine, aggregated:

```sql
-- name: CountJudgeGroupsByCategoryForUser :many
-- Per-category GROUP count (distinct (category, target) coordinates) across the
-- caller's whole backlog, for the Judge filter-chip counts (#244). Uncapped by
-- design: this is the canonical per-category aggregate #235 Decision 6 required,
-- and it is exact even when the backlog list truncates. COUNT(DISTINCT rr.target)
-- within each category == one per group, matching GroupJudgeRecommendations'
-- (category, target) coordinate. No disposition/filed joins: the count is
-- whole-backlog, all triage states (a group stays a group once triaged), so this
-- query does not touch the ladder. It also drops the backlog query's inner
-- `JOIN runs r ON r.id = rv.target_run_id`: run_reviews.target_run_id is NOT NULL
-- UNIQUE (00059_run_reviews.sql:22), a lossless 1:1 join that adds nothing to a
-- per-category count — do NOT re-add it "for consistency".
SELECT rr.category AS category, COUNT(DISTINCT rr.target)::bigint AS group_count
FROM run_reviews rv
JOIN review_recommendations rr ON rr.review_id = rv.id
WHERE rv.user_id = @user_id
GROUP BY rr.category;
```

sqlc already types a top-level `COUNT` aggregate as `int64`; the explicit `::bigint`
is belt-and-braces (in-repo precedent: `judge_issue_close.sql:132-133`), not a fix
for the expression-inference trap `.claude/rules/go.md` records — that trap is about
*boolean/CASE* expressions typed `interface{}`, which this is not. Regenerate with
sqlc; confirm the generated const in `api/internal/store/*.sql.go` moved. Per
`.claude/rules/go.md`, a green `sqlc generate` is **not** proof the query runs — the
M1 live-DB test is what executes it.

### Service (`api/internal/workersvc/judge_disposition.go`)
A `JudgeCategoryStats(ctx, ownerUserID uuid.UUID) (apitypes.JudgeCategoryStatsDTO, error)`
beside `JudgeTriageStats` (`:106-123`): run the new query, fold the rows into a
`map[string]int` keyed by category (`int(row.GroupCount)` — the query returns
`int64` from the `bigint`, so the narrowing is explicit, not automatic), return the
DTO. No grouping in Go — the `COUNT(DISTINCT)` does it in SQL — and no bucket ladder.

### DTO (`api/internal/apitypes/review.go`) + wire test
A new struct, a map so the taxonomy can grow without a wire break:

```go
type JudgeCategoryStatsDTO struct {
    Counts map[string]int `json:"counts"`
}
```

Add `TestJudgeCategoryStatsDTOTags` to `api/internal/apitypes/wire_test.go`
pinning the single key `counts` (the `assertTags` strict-equality convention at
`wire_test.go:36-45`; `TestTriageDTOTags` at `:173-176` is the model). A new DTO
gets its own tag test rather than widening `TriageDTO`, keeping the six-bucket
triage contract untouched.

### Handler + route (`api/internal/handler/judge_stats.go`, `handler.go`)
A `JudgeCategoryStats` handler beside `JudgeStats` (`judge_stats.go:15-28`),
owner-scoped via `RequireUser`, mounted at `GET /me/judge/category-stats` in the
`/me/judge` route group (`handler.go`, beside `r.Get("/stats", h.JudgeStats)`).

### Web — render the count (`web/src/pages/Judge.tsx`, `web/src/lib/api.ts`)
- `api.getJudgeCategoryStats(): Promise<JudgeCategoryStats>` and the TS type
  `JudgeCategoryStats = { counts: Record<string, number> }` in `api.ts`, a sibling of
  `getJudgeStats` (`api.ts:2389`).
- Fetch it **once on Judge-page mount**, not on every bucket/category change
  (the counts are invariant to both). Hold it in page state.
- `LabelFilter` (`Judge.tsx:546-596`) renders `counts[cat] ?? 0` in a badge on each
  chip; a chip with a `0` whole-backlog count is dimmed (open question 3).
- The comment at `Judge.tsx:401-405` / `:540-545` that says there are "deliberately
  NO per-chip counts (Decision 6)" is updated — this PRD is the aggregate Decision 6
  required, so the counts are now sourced correctly, not tallied off the groups. The
  new comment must say *where* the count comes from (the aggregate endpoint), so the
  next reader does not re-derive the ban.

### Mock parity (`web/src/mocks/mockApi.ts`)
A `getJudgeCategoryStats` mock that recomputes distinct `(category, target)`
coordinates per category from the caller's fixtures, independently — the way
`computeTriage` (`mockApi.ts`) recomputes the triage aggregate from fixtures rather
than sharing the Go helper. It must count **distinct coordinates**, not rows, or the
mock diverges from the `COUNT(DISTINCT)` server semantics.

### Tests
- **Server, the decisive one — a LIVE-DB test.** Seed a backlog where one
  category's groups sit **past** the 2000-row cap, and where the same
  `(category, target)` recurs across several reviews/runs. Assert
  `/me/judge/category-stats` returns that category's **true** group count (dedupe
  across runs correct, and the past-cap groups **counted** — the aggregate is
  uncapped), while the **unfiltered** (`?bucket=all`, no `?category=`) backlog list
  truncates and drops that category's oldest groups off-page. **Truncation must be
  shown on the unfiltered list, not a `?category=`-filtered one:** #235 pushed the
  category predicate *below* the `LIMIT`
  (`api/internal/store/queries/judge_recommendations.sql:100-105`), so a
  category-filtered request cannot truncate that label off-page — the contrast that
  proves the aggregate is doing work the list cannot is against the *all-labels*
  backlog. This proves the two properties Decision 6 required: uncapped and
  deduped-to-groups. It MUST be a live-DB test
  (`api/internal/store/*_integration_test.go`), modelled on
  `TestJudgeBacklogRunAnchorLiveDB`; the handler's fake store returns rows verbatim
  and cannot show a SQL `COUNT(DISTINCT)`.
- **Handler**: owner-scoping (a second user's recommendations are not counted); the
  response shape is the `counts` map.
- **wire_test**: `TestJudgeCategoryStatsDTOTags`.
- **Web**: the chip renders the count from `getJudgeCategoryStats`; the count is
  **not** refetched on a bucket or category toggle (fetched once on mount); a
  `0`-count chip is dimmed; the nav badge is byte-identical with the feature on/off
  (it reads `/me/judge/stats.todo`, never the category endpoint).
- **Mock**: `mockApi.getJudgeCategoryStats` counts distinct coordinates and agrees
  with the server semantics. The differential is only meaningful over a
  **discriminating fixture** — one where a rows-counter and a distinct-coordinate
  counter *disagree*: a coordinate that recurs across ≥2 reviews (today's fixtures
  already have this, e.g. the ripgrep/shellcheck cross-run recurrences in
  `mockApi.test.ts`), and ideally the same `target` string under two categories (which
  a missing `GROUP BY category` would miscount). State the requirement in the test so
  a future fixture edit cannot silently make it vacuous.

### Docs, changelog, specs
`docs/judge-menu.md` (or the Judge page user doc) gains a line on the chip counts;
`CHANGELOG.md` entry; a `specs/ai.md` note recording the new canonical per-category
aggregate and the whole-backlog / group-count / separate-endpoint decisions. No
`specs/human.md` change without user approval.

## Milestones

- [x] **M1 — Server: the per-category count aggregate end to end.** The new SQL
      `COUNT(DISTINCT)` query (regenerated, const confirmed moved),
      `JudgeCategoryStats`, the `JudgeCategoryStatsDTO` + `wire_test` tag test, the
      handler and route. Ships with the **live-DB uncapped-and-deduped proof** and the
      handler owner-scoping test. No web change yet; exercised by tests and `curl`.
- [x] **M2 — Web: render the count on each chip.** `getJudgeCategoryStats` + the TS
      type, fetch-once-on-mount, the chip count badge, the dim-on-zero treatment,
      `mockApi` parity, and the updated Decision-6 comment. Unit tests: count render,
      no-refetch-on-toggle, dim-on-zero, nav-badge isolation.
- [x] **M3 — Docs, changelog, specs, parity check.** The user doc line, the changelog
      entry, the `specs/ai.md` note, and a browser check that the shipped chip counts
      match the mockup against a mock-mode build (`VITE_UZI_MOCK=1`, never a
      live-proxying `vite dev`/`preview`, per `.claude/rules/web.md`).

### Parallelisation
M1 is the dependency: M2 consumes the new endpoint and DTO. Agree the
`{ counts: Record<string, number> }` shape up front and M2 can build its rendering
and mock against it in parallel with M1's server work, accepting one merge on the DTO.
M3 is last (it verifies the shipped UI). CLI is deferred (open question 5).

## Risks & mitigations

| Risk | Mitigation |
|---|---|
| A count tallied off the delivered groups undercounts under truncation / bucket filtering | The count is a dedicated uncapped SQL aggregate, never derived from `groups`; the live-DB test seeds past-cap groups and asserts they are counted. |
| The category counts start driving the nav badge | Separate endpoint + separate DTO with no `todo` field; a test asserts the badge is byte-identical with the feature on/off. |
| Rows-vs-groups mismatch: the chip count disagrees with the number of cards | Count distinct `(category, target)` coordinates (groups), the same unit the chip filters and the list renders; `COUNT(DISTINCT target)` in SQL. |
| The count flickers as the user triages | Whole-backlog, all-states count is triage-invariant by construction (a coordinate stays a coordinate); no post-action refetch. |
| A new category exists in data but has no chip | Map DTO (`counts`) does not break the wire format; the client renders counts only for known chips (documented, open question 6). |
| The extra aggregate adds DB load | One read per Judge-page mount only (not per toggle, not on the polled badge); same seq-scan cost as the existing `/me/judge/stats`, indexing is a #94-scoped follow-up. |

## Success criteria

- Each Judge filter chip shows the number of recommendation groups in its category
  across the caller's whole backlog, sourced from `/me/judge/category-stats`, never
  tallied from the on-screen groups.
- On a truncated backlog, a chip shows the **true** category group count even when
  that category's groups sit past the 2000-row cap — proven by a **live-DB** test.
- The count is byte-stable across bucket switches and across triage actions
  (whole-backlog, group count); it changes only when the underlying recommendation
  set changes.
- The nav badge and bucket-tab counts are byte-identical with the chip counts
  feature on and off.
- The mock-mode build shows chip counts that match the server semantics (distinct
  coordinates per category).

## Decision log

1. **A dedicated uncapped aggregate, never a tally off `groups`.** The delivered
   backlog is capped-before-grouping and bucket-filtered, so a tally is wrong under
   truncation — the exact anti-pattern #235 Decision 6 named. The count is a real
   `COUNT(DISTINCT)` aggregate, like `/me/judge/stats`.
2. **Group count (distinct `(category, target)`), not occurrence rows.** The chip
   filters groups and each group is a card, so the count predicts the click; matches
   the mockup. Differs from `/me/judge/stats`, which counts rows — hence a new
   aggregate, not a reuse.
3. **Whole-backlog, bucket-independent.** A bucket-aware count needs the forbidden
   second `BucketOf` ladder in SQL (#94 Decision 2); whole-backlog is consistent with
   the tabs and strip and is triage-invariant. The "showing N of M" line reconciles
   the visible slice.
4. **A separate endpoint and DTO, not a widening of `TriageDTO`.** Keeps the
   category dimension out of the per-review DTO and the polled badge payload, so the
   nav badge (which reads only `todo`) is structurally unreachable from category data.
5. **To-triage / bucket-aware counts deferred, not rejected.** A per-category
   to-triage count is more actionable and reuses the Go `BucketOf` ladder, but costs a
   Go grouping pass over uncapped rows and a post-triage refetch; revisit if the
   whole-backlog count proves confusing in use (open question 2).
6. **Fetched once on mount, not polled and not per-toggle.** The count is invariant
   to bucket, category, and triage state, so a single fetch on Judge-page mount is
   correct; the badge poll stays on `/me/judge/stats` and carries no category work.
7. **Map DTO, so the taxonomy can grow.** `counts: {category: n}` does not break the
   wire format when a category is added (#235 open question 6); the client reads
   `counts[cat] ?? 0` per chip.
8. **CLI deferred, check recorded.** `uzi review backlog` already filters by
   `--category`; surfacing counts there is a nice-to-have off the same endpoint, not
   parity-critical, and is left to a follow-up (open question 5).

## Review findings

Reviewed 2026-08-08 by an architect subagent instructed to open every code citation
and assume some were wrong, and to trace the two load-bearing correctness claims
against the schema. Verdict: **sound-with-fixes** — the core design is correct on
every axis checked, and the citations were accurate to an unusual degree. What was
confirmed, and what changed:

**Confirmed against code (not merely asserted):**
- **`COUNT(DISTINCT rr.target) GROUP BY rr.category` equals the group count.** The
  group key is exactly `coord{category, target}` (`judge_backlog.go:107,221`), and
  `GROUP BY category` correctly separates the same-`target`-across-categories case.
- **Triage-invariance holds.** An exhaustive grep for writers of
  `review_recommendations` found only the re-judge upsert and a guarded
  `addressed_by_run_id` stamp — neither moves a coordinate. Dispositions and filed
  issues write only their own side tables.
- **The separate endpoint has no isolation hole.** The only production reader of
  `/me/judge/stats` is the `AppShell` nav badge; the Judge page reads triage from the
  embedded `JudgeBacklog.triage`, not `getJudgeStats`. The new endpoint is purely
  additive and structurally unreachable from `setJudgeTodo`.
- **Dropping the inner `JOIN runs` is safe** because `target_run_id` is a `NOT NULL
  UNIQUE` FK (`00059_run_reviews.sql:22`) — a lossless 1:1 join.

**Fixes folded in:**
- **The live-DB test now demonstrates truncation on the *unfiltered* backlog.** The
  draft said "the backlog list for that category truncates", but #235 pushes the
  category predicate below the `LIMIT`, so a category-filtered request cannot truncate
  that label off-page — the contrast must be against the all-labels list. (Tests section.)
- **The mock differential test now requires a discriminating fixture** (a coordinate
  recurring across ≥2 reviews; ideally one `target` under two categories), or the
  rows-vs-groups differential is vacuous. (Tests section.)
- **The `int(row.GroupCount)` narrowing is explicit** (Service section); the `::bigint`
  note was corrected — it is belt-and-braces, not a fix for the `interface{}`
  expression-inference trap, which concerns boolean/CASE expressions.
- **The AppShell badge is fetched on navigation, not on a timer** — the "polls on a
  timer" phrasing was wrong; corrected in the Solution and cost sections.
- **`selfimprove.sql`'s `addressed_by_run_id` write is named** as the fourth writer of
  the counted table in the triage-invariance argument (it strengthens the claim).

Citation nits corrected: the "NEVER tallied from `groups`" comment documents
`JudgeBacklog`'s `triage` field, not the `TriageCounts` type (`api.ts:1317`).
