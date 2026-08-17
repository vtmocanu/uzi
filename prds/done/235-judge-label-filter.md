# PRD #235 — Filter the Judge backlog by recommendation label

**Issue**: [#235](https://github.com/vtmocanu/uzi/-/issues/235) · **Label**: PRD · **Priority**: Medium
**Area**: `web/src/pages/Judge.tsx` (the chip row + the `?category=` URL param) · `web/src/lib/api.ts` (`getJudgeBacklog` gains a categories arg) · `web/src/mocks/mockApi.ts` (mock-mode parity) · `api/internal/handler/judge_recommendations.go` (param parse + validation) · `api/internal/workersvc/judge_backlog.go` (signature + a `ValidRecommendationCategory` helper) · `api/internal/store/queries/judge_recommendations.sql` (one optional predicate) · `api/cmd/uzi/review.go` (`--category` on `uzi review backlog`).
**Mockup**: [`prds/mockups/235-judge-label-filter-mock.html`](../mockups/235-judge-label-filter-mock.html) — interactive; the label chips filter a sample backlog live.
**Line references** are against `dd342cb7`.
**Status**: complete (2026-08-07) — all four milestones implemented, tested, and reviewed on branch `agent/issue-235`.

**Reviewed 2026-08-07** by a verification subagent that opened every code citation against
`dd342cb7`. Verdict: the design is sound (category is a raw stored column filtered pre-`LIMIT`
in SQL exactly like `run_anchor`; bucket correctly stays in Go; validation reuses the ingest
enum; `triage` is untouched). Fixes are folded in below — the pre-cap proof test was
**relocated to a live-DB test** (the handler's fake store cannot prove SQL ordering), the
DTO-echo's `wire_test.go` cost was added to scope, empty-param normalization and the CLI
`--category` overload were called out, and four citation nits were corrected. See
[Review findings](#review-findings).

## Problem

The Judge page (`web/src/pages/Judge.tsx`) is the cross-run recommendation workbench:
it reads every recommendation the caller owns, deduped by `(category, target)` into
groups (`GET /me/judge/recommendations`), and lets a user triage a whole group in one
action. It already narrows the list two ways — a **bucket** tab (`?bucket=`, the #94
todo/filed/done/dismissed/all ladder) and a **run** anchor (`?run=`, from a
notification deep-link). It does **not** let a user narrow by the recommendation's
*label* — the six-value taxonomy `RECOMMENDATION_LABELS` maps
(`web/src/lib/judge.ts:40-47`):

| category | label |
|---|---|
| `enable_tool` | Enable a tool or skill |
| `install_worker_tool` | Install a worker tool |
| `adjust_template` | Adjust an agent template |
| `improve_agent` | Improve an agent |
| `add_agent` | Add a missing agent |
| `improve_uzi` | Improve uzi |

A user who wants to act on one *kind* of recommendation — "show me only the worker-tool
installs I keep getting asked for", or "just the `improve_uzi` backlog before I open a
self-improve run" — has to eyeball the whole list. Every group already renders its label
as a `Badge` (`Judge.tsx:497`), so the label is a visible, meaningful axis with no way
to filter on it.

## Solution

Add a **recommendation-label filter** to the Judge page: a row of toggle chips, one per
label, above the bucket tabs. It is **multi-select** (OR within the set — show groups in
*any* selected label), it lives in the URL as `?category=`, and it is enforced
**server-side**, the same as `?bucket=` and `?run=`.

### Why server-side, and not a client-side render filter

The obvious cheap version — fetch the whole backlog and hide the non-matching groups in
React — is **wrong here specifically**, and the reason is the row cap.

The backlog read is bounded by `JudgeBacklogMaxRows = 2000`
(`api/internal/workersvc/judge_backlog.go:136`), and the cap is applied **before
grouping**: `JudgeRecommendationBacklog` pulls `Lim: JudgeBacklogMaxRows + 1`
(`:162`), then `rows = rows[:JudgeBacklogMaxRows]` (`:169`), and only *then*
`GroupJudgeRecommendations` dedups what survived. So on a truncated backlog a whole
category can be **entirely off-page**: if the 2001st-onward rows are the only
`improve_uzi` recommendations, a client-side filter for `improve_uzi` renders *nothing*
while the true answer is "several, past the cap". A render filter would therefore be
correct only until the cap bites, which is exactly the moment a filter is most useful.

The server already makes this argument for the `run` anchor, in the query itself: the
anchor is "pushed DOWN here rather than post-filtered in Go, so an anchored pull reads
only the rows it will return" (`judge_recommendations.sql:71-74`), and the CLI's `--run`
help calls it "the one filter applied BEFORE the server's row cap"
(`api/cmd/uzi/review.go:72`). Category is the same shape as run: it is a stored column
(`rr.category`, `judge_recommendations.sql:43`), not a computed rollup, so it belongs in
the same place — an optional SQL predicate, applied **before** the `LIMIT`. Then the cap
bounds the *selected* categories, the surviving groups are complete up to 2000 rows of
what was asked for, and narrowing the filter makes truncation *less* likely to bite, not
more.

### Category composes with bucket; it does not replace either filter's home

`bucket` stays exactly where it is — filtered **in Go**, in `filterGroups`
(`judge_backlog.go:303-319`) — and must not move into SQL. The bucket value matches the
GROUP **rollup**, which is computed from the shared `BucketOf` ladder; a SQL bucket
filter would be "the second ladder #94 Decision 2 forbids"
(`judge_recommendations.sql:13-15` comment / `judge_backlog.go:149-150,299-302`). So the two filters live in
two layers by design and compose cleanly: **SQL narrows the rows to the selected
categories → the cap applies → Go groups them → Go bucket-filters the rollups**. Adding
category to SQL touches neither the cap logic nor the bucket ladder.

### Validation: a typo is a 400, never a silent empty list

The handler already rejects an unknown `bucket` with a 400
(`judge_recommendations.go:48-51`) and an unparseable `run` with a 400 (`:53-59`), on the
stated principle that "a typo in a CLI flag can never look like an empty backlog"
(`:27-30`). `?category=` gets the same treatment: each value is validated against the
**existing server-side source of truth**, the `RecommendationCategories` map
(`api/internal/workersvc/judge_review.go:23-26`) — the same closed set the review-ingest
path already enforces — exposed through a new `ValidRecommendationCategory` helper beside
`ValidJudgeBacklogBucket` (`judge_backlog.go:44`). An unknown category is a 400; an empty
`?category=` (or its absence) means "all labels", the current behaviour.

### What deliberately does not change

- **`triage` / the nav badge.** The bucket tab counts and the nav badge come from the
  separate `/me/judge/stats` aggregate (`JudgeTriageStats`), never tallied off the groups
  on screen (`api.ts:1450-1461`, `Judge.tsx:123-127`). The category filter narrows the
  **group list only**. The bucket-tab counts and the nav badge keep counting the *whole*
  backlog, unchanged — the same way they already ignore the `run` anchor and truncation.
  A category filter must not silently start driving the badge off a filtered view.
- **The `triage` summary strip** (`Judge.tsx:353-355`) likewise reflects all-time
  totals, not the filtered slice. (See open question 4 on whether the strip should gain a
  "filtered to N of M" line.)
- **The stats query and the row cap.** No new aggregate and no cap change. Per-label
  *counts on the chips* are explicitly **out of the core scope** — see open question 3.

## User journey

1. A user opens **Judge** and sees the same to-triage backlog as today, now with a
   **Filter by label** row above the bucket tabs. No chip is selected; the list is
   unchanged.
2. They click **Install a worker tool**. The URL gains `?category=install_worker_tool`,
   the page re-fetches, and the list narrows to worker-tool groups in the current bucket.
   The bucket tabs and nav badge still show whole-backlog counts.
3. They also click **Improve uzi**. Now `?category=install_worker_tool,improve_uzi`; the
   list shows groups in either label.
4. They switch the bucket tab to **All**; the label filter persists (both are URL
   params). They copy the URL to a teammate, who opens the exact same filtered view.
5. They click **Clear**; `?category=` drops and the full backlog returns.
6. On a **truncated** backlog, the existing truncation banner
   (`Judge.tsx:384-389`) still shows when the cap bit *for the selected categories* —
   and biting is now less likely, because the SQL predicate runs before the cap.

## Open questions

### 1. Multi-select (OR) or single-select? **Multi-select, OR.** (recommended)
A user triaging "the two kinds of tool asks" wants both at once, and OR is what the chips
in the mock do. AND across labels is meaningless (a recommendation has exactly one
category), so multi-select can only mean OR. Ship multi.

### 2. Where does the filter live — Judge only, or RunView too?
The RunView triage panel already lists one run's recommendations and could carry the same
chips. **Recommendation: Judge only for v1.** RunView shows a single run's recs, usually
few, where a label filter earns little; the cross-run backlog is where the axis pays off.
Revisit if asked.

### 3. Do the chips show a per-label count?
The mock shows a count on each chip. **There is no canonical per-label aggregate today** —
`/me/judge/stats` is bucketed (todo/filed/done/dismissed), not per-category. The only
ways to populate a count are (a) tally it off the delivered groups, which is the anti-
pattern the codebase forbids by name — triage is "NEVER tallied from `groups`"
(`api.ts:1452`) and would be wrong under truncation and under the bucket filter — or
(b) add a per-category aggregate to the stats query, which carries the same full-table
seq-scan cost the cap comment measures (`judge_backlog.go:117-135`).
**Recommendation: ship v1 chips WITHOUT counts.** A count is a deliberate follow-up
(its own stats aggregate), not something to fake off the filtered list. The mock's counts
are illustrative and are dropped from the shipped chips unless (b) is scheduled.

### 4. Should the `triage` strip gain a "filtered to N of M groups" line?
The strip and tabs stay whole-backlog by design (above). A small "showing N of M" line
**below the tabs** (reading the length of the returned `groups`, clearly labelled as the
filtered view) would orient the user without touching the canonical counts.
**Recommendation: yes, a plain result line**, not a restyled strip — it is a view hint,
not a second count that could be mistaken for triage.

### 5. Does the CLI get `--category`?
`uzi review backlog` forwards `--bucket` and `--run` verbatim to this endpoint
(`api/cmd/uzi/review.go:64,72`), and CLAUDE.md's rule is "New uzi functionality ⇒ check
whether `api/cmd/uzi/` needs a matching CLI change." **Recommendation: yes** — a
`--category` flag is one line plus forwarding, validated server-side identically (an
unknown value is the server's 400 → exit 2, never a silent empty list, exactly as
`--bucket` already behaves per `review.go:199-205`). Scoped as its own milestone (M3) so
the web change is not held up by it.

The flag name is already used in the same command group, with **different semantics**:
`uzi review resolve|dismiss` add `--category`/`--target` via `addCoordFlags`
(`review.go:211-212`) as a **single literal coordinate** (the group to act on), not enum-
validated. `backlog --category` is a distinct animal — multi-value, enum-validated,
400-on-unknown. There is no cobra collision (flags are per-subcommand), but the overload
inside one `uzi review` group is worth one line in `docs/cli.md`. Decide too whether
`backlog --category` gets a pinned usage-string test like `--bucket`'s
(`TestBacklogBucketUsageMatchesServerEnum`-style, `review.go:199-205`), so the CLI's help
text can never silently drift from the server enum.

### 6. A category present in the data but absent from the client label map?
`recommendationLabel` has a fallback that humanizes an unknown slug "so the panel never
renders a raw enum" (`judge.ts:37-53`), anticipating a future category. The **filter
chips are built from the fixed `RECOMMENDATION_LABELS` keys**, so a not-yet-known category
would render in group badges but have no chip to filter by until the client map learns
it. This is acceptable (the map and the server enum are edited together when a category is
added) and is called out so it is a known limitation, not a surprise.

## Technical scope

### SQL — one optional predicate (`api/internal/store/queries/judge_recommendations.sql`)
Add a `sqlc.narg('categories')::text[]` predicate to `ListJudgeRecommendationRowsForUser`,
alongside the existing `run_anchor` escape-hatch pattern:

```sql
AND (
    sqlc.narg('categories')::text[] IS NULL
    OR rr.category = ANY(sqlc.narg('categories')::text[])
)
```

NULL → the predicate is a no-op (all labels), matching how `run_anchor IS NULL` disables
the anchor. It sits **before** the `ORDER BY … LIMIT`, so the cap bounds the filtered set.
No `GROUP BY`, no `CASE` — the file's invariant (grouping/bucketing stay in Go) is
preserved; category is a raw-column WHERE, the one kind of filter this query already does
(for `run_anchor`). Regenerate with sqlc.

### Handler (`api/internal/handler/judge_recommendations.go`)
Parse `?category=` (repeated param or comma-separated — pick one and pin it in a test),
validate each value with `workersvc.ValidRecommendationCategory`, 400 on any unknown value
(mirroring the bucket/run 400s at `:48-59`). Pass the validated slice to
`JudgeRecommendationBacklog`.

**Normalize before validating.** A present-but-empty `?category=` comma-splits to `[""]`,
and `""` is not a valid category, so trim and drop empty tokens *first* and treat a
resulting empty slice as "all labels" (the absent-param behaviour). Without this step,
`?category=` with no value would 400 — contradicting the "empty means all" contract in the
Solution. Pin both `?category=` (empty → all) and `?category=,improve_uzi` (empty token
dropped, not a 400) in the handler test.

### Service (`api/internal/workersvc/judge_backlog.go`)
- New `ValidRecommendationCategory(s string) bool`, reading `RecommendationCategories`
  (moved/kept as the single source of truth), beside `ValidJudgeBacklogBucket` (`:44`).
- `JudgeRecommendationBacklog` gains a `categories []string` param, threaded into the
  query params (`:157-163`). Everything after — the cap, grouping, bucket filter, triage —
  is unchanged. Echo the applied categories back on the DTO (like `Bucket`/`Run`) so the
  response is self-describing. **This is not free**: `JudgeBacklogDTO`
  (`api/internal/apitypes/review.go:174-180`) has its JSON tag set pinned by
  `TestJudgeBacklogDTOTags` in `api/internal/apitypes/wire_test.go`, and `assertTags` is a
  **strict full-set equality** (`wire_test.go:41`) — adding any field reddens the build
  until the expected tag list is updated. Add `wire_test.go` to the touch-points, or drop
  the echo and have the client rely on the URL it already owns (a defensible v1
  simplification — see Decision 9).

### Web — the filter UI (`web/src/pages/Judge.tsx`)
- A `LabelFilter` chip row above the bucket tabs (`:359-382`), built from
  `RECOMMENDATION_LABELS`, multi-select, with a Clear control.
- `?category=` in the URL via `useSearchParams`, read/written exactly like `bucket`
  (`:96-101`, `:170-174`); validate against the known keys (drop unknowns) the way
  `isBucket` guards `?bucket=` (`:456-458`).
- `api.getJudgeBacklog` (`api.ts:2397-2406`) gains a `categories?: string[]` arg appended
  to the query string; `JudgeBacklog` DTO gains the echoed `category` field
  (`api.ts:1456-1461`).
- Empty-state copy for "no groups match these labels in this bucket", parallel to the
  run-anchor empty copy (`Judge.tsx:398-406`).
- A "showing N of M" result line (open question 4).

### Mock parity (`web/src/mocks/mockApi.ts`)
`mockApi.getJudgeBacklog` (`:2427`) → `computeBacklog` (`:568`) must honour `?category=`, or
the mock-mode build diverges from the real endpoint. Two specifics, or the mock lies in the
exact way the server design avoids:
- **Filter rows BEFORE the cap.** Thread category into `backlogRowsFromReviews` so it runs
  before `capBacklogRows` (`:570`), mirroring the SQL predicate-before-`LIMIT`. Filtering
  groups *after* the cap reintroduces the off-page bug inside the mock.
- **It rides with the truncation/e2e leg, not the golden fidelity fixture.** The fidelity
  fixture deliberately excludes pre-cap row operations — `?run=` is excluded for exactly this
  reason (`:577-579`, "belongs to the e2e leg instead") — and category is the same kind of
  pre-cap row filter. So the parity check belongs with `judgeBacklogTruncation.test.ts` / the
  e2e leg, not `judgeBacklogFidelity.test.ts`.

### Tests
- **Server, the decisive one — a LIVE-DB test.** Seed > 2000 rows where the *only* rows of
  one category sit past the cap; assert `?category=<that>` returns those groups (no longer
  truncated off), i.e. the SQL predicate runs before the `LIMIT`. This MUST be a live-DB test
  in `api/internal/store/judge_recommendations_integration_test.go`, modelled on
  `TestJudgeBacklogRunAnchorLiveDB` (`:30`) and its "the hard row cap must bind in SQL"
  assertion (`:159-165`). It **cannot** live on the handler's fake `backlogStore`: that
  double returns `s.rows` verbatim and ignores every param
  (`judge_recommendations_test.go:39-43`), so a category filter — a SQL operation — is
  invisible to it, and a fake-store test would slice the past-cap rows off in Go and *fail*.
- **Handler, the weaker twin**: assert the parsed `categories` slice reaches the query args,
  modelled on `TestJudgeRecommendationsPushesRunAnchorToTheQuery`
  (`judge_recommendations_test.go:335`) — it inspects `backlogArg`, which the fake store
  *does* record. This pins threading, not pre-cap ordering; the two are not substitutes.
- Handler: unknown `?category=` → 400; present-but-empty and absent → all (see the
  empty-normalization note); multi-value → OR; composition with `?bucket=` and `?run=` (all
  three at once).
- Web: chip toggle writes/reads `?category=`; unknown URL value ignored; empty state; the
  `api.ts` query-string build.
- Mock: the truncation/e2e leg, not the golden fidelity fixture — see the mock-parity note.

### Docs & changelog
`docs/cli.md` (the `--category` flag, M3), any Judge-page user doc, and a `CHANGELOG.md`
entry. No `specs/human.md` change without user approval; a `specs/ai.md` note records the
server-side-filter decision.

## Milestones

- [x] **M1 — Server: `?category=` end to end.** The SQL predicate (regenerated),
      `ValidRecommendationCategory`, the handler parse+validate+400 (with empty-token
      normalization), the `JudgeRecommendationBacklog` signature, and the DTO echo (which
      also updates `wire_test.go`'s pinned tag set — or is dropped per Decision 9). Ships
      with the **live-DB pre-cap proof** test, the handler-level threading assertion, the
      unknown-category 400, and the bucket/run/category composition tests. No web change
      yet; the endpoint is exercised by tests and `curl`.
- [x] **M2 — Web: the label filter.** The chip row, `?category=` in the URL, the Clear
      control, the empty state, the result line (open question 4), `api.getJudgeBacklog`
      and the DTO, and `mockApi` parity. Unit tests + the fidelity test. The Judge page can
      filter to any subset of labels, shareable by URL.
- [x] **M3 — CLI parity.** `uzi review backlog --category` forwarded verbatim and
      validated server-side (open question 5), `docs/cli.md`. Independent of M2.
- [x] **M4 — Docs, changelog, mock parity check.** The user-facing docs and the changelog
      entry, and a browser check that the shipped filter matches
      `prds/mockups/235-judge-label-filter-mock.html` against a mock-mode build
      (`VITE_UZI_MOCK=1`, never a live-proxying `vite dev`/`preview`, per CLAUDE.md).

### Parallelisation
M1 is the dependency: M2 consumes the new param and DTO field, and M3 forwards it. Agree
the `api.ts` `category` field shape up front and M2/M3 can start against it in parallel
with M1's server work, accepting one merge on the DTO. M4 is last (it verifies the shipped
UI). M3 is otherwise independent of M2.

## Risks & mitigations

| Risk | Mitigation |
|---|---|
| A category filter implemented in Go/React post-cap silently hides categories that truncated off-page | M1's pre-cap proof test; the predicate is in SQL before the `LIMIT`, the same place `run_anchor` already is. |
| The category filter starts driving the nav badge / bucket counts off the filtered view | `triage` stays the `/me/judge/stats` aggregate, untouched; a test asserts the badge/tab counts are invariant to `?category=`. |
| A typo in `?category=` (or `--category`) returns an empty list that reads as "nothing to triage" | Server 400 on any unknown value, validated against the ingest-time `RecommendationCategories` set; matches the existing bucket/run 400s. |
| `mockApi` not updated, so mock-mode build and fidelity tests diverge from the real endpoint | M2 includes `mockApi` + the fidelity test; called out as the second consumer. |
| A future category exists in data but has no chip | Documented limitation (open question 6); the map and server enum are edited together when a category is added. |
| Faked per-label counts on chips mislead under truncation/bucket | Counts are out of core scope (open question 3); v1 ships countless chips rather than a tally the codebase forbids. |

## Success criteria

- Selecting one or more labels narrows the Judge group list to those labels, in the
  current bucket, with the URL carrying `?category=` and the view reproducible from that
  URL alone.
- On a truncated backlog, `?category=X` returns X's groups even when X's rows sit past the
  2000-row cap — proven by a **live-DB** test (a fake store cannot show SQL pre-cap
  ordering), not asserted.
- An unknown `?category=` value is a 400 (web and CLI); a present-but-empty `?category=` is
  "all labels", never a silent empty list.
- The bucket-tab counts and the nav badge are byte-identical with and without a category
  filter applied.
- `uzi review backlog --category=<label>` returns the same filtered set as the web view.
- The mock-mode build honours `?category=` (filtering rows before the cap), so the mock and
  the real endpoint agree.

## Decision log

1. **Server-side `?category=`, not a client render filter.** The 2000-row cap is applied
   before grouping, so a post-cap filter hides categories that truncated off-page. Category
   is a stored column, so it belongs in SQL before the `LIMIT`, where `run_anchor` already
   is.
2. **Category in SQL, bucket stays in Go.** Bucket matches a computed rollup and a SQL
   bucket filter would be the forbidden second `BucketOf` ladder (#94 Decision 2). The two
   filters compose across two layers: SQL narrows rows, the cap bounds them, Go groups and
   bucket-filters.
3. **Validate against the ingest-time `RecommendationCategories` set**, exposed as
   `ValidRecommendationCategory`. One source of truth; a typo is a 400, mirroring
   bucket/run.
4. **Multi-select, OR semantics.** A recommendation has one category, so AND is
   meaningless; OR is the only sensible multi-select and is what the mock does.
5. **`triage`, the nav badge, and the strip stay whole-backlog.** The filter narrows the
   group list only. Triage is the `/me/judge/stats` aggregate and must never be driven off
   a filtered view.
6. **No per-label chip counts in v1.** There is no canonical per-category aggregate, and
   tallying off the (truncated, bucket-filtered) groups is the anti-pattern the codebase
   forbids. A count is a deliberate follow-up with its own aggregate (open question 3).
7. **Judge only for v1, not RunView.** The cross-run backlog is where a label axis pays
   off; a single run's short rec list does not (open question 2).
8. **CLI `--category` ships as its own milestone.** Same endpoint, verbatim forward,
   server-side validation; parallel to `--bucket`/`--run`. Keeps the web change unblocked.
9. **The DTO echo is optional.** Echoing the applied categories back on `JudgeBacklogDTO`
   costs a `wire_test.go` pinned-tag update (strict full-set equality). Keep it for parity
   with `bucket`/`run`, or drop it and let the client read its own `?category=` URL — decided
   at M1. Either way the tag test is a known touch-point, not a surprise build break.

## Review findings

Reviewed 2026-08-07 by a verification subagent instructed to open every code citation and
assume some were wrong. Verdict: **sound-with-fixes** — the core approach (SQL pre-`LIMIT`
category predicate, bucket staying in Go, ingest-enum validation, untouched `triage`) was
confirmed against the code. No design error. What changed:

- **The pre-cap proof test was relocated.** The draft cited the handler truncation test
  (`judge_recommendations_test.go:359-373`) as the extension point, but that runs against a
  fake `backlogStore` whose `ListJudgeRecommendationRowsForUser` returns `s.rows` verbatim
  and ignores every param (`:39-43`). A category filter is a SQL operation, so a fake-store
  test would slice the past-cap rows off in Go and *fail*. Moved to a **live-DB** test
  modelled on `TestJudgeBacklogRunAnchorLiveDB`, with a separate handler-level threading
  assertion kept as the weaker twin. This is the one finding that touched a load-bearing
  claim rather than a citation.
- **The DTO echo's `wire_test.go` cost was added to scope** (Decision 9). `TestJudgeBacklogDTOTags`
  pins the tag set with strict equality (`wire_test.go:41`), so the echo field reddens the
  build until the expected list is updated — a touch-point the draft omitted.
- **Empty-param normalization** (`?category=` → all, not a `[""]` 400) was made explicit in
  the handler scope and success criteria, reconciling the "empty means all" contract with the
  comma-split parse.
- **The CLI `--category` overload was called out**: the flag already exists on
  `uzi review resolve|dismiss` (`review.go:211-212`) as a single literal coordinate, unlike
  `backlog`'s multi-value enum-validated form. No cobra collision, but a `docs/cli.md` note
  and a possible pinned-usage test.
- **The mock must filter rows before the cap** and ride the truncation/e2e leg, not the
  golden fidelity fixture (which excludes pre-cap row ops, as `?run=` already is).

Citation nits corrected: the phantom `judge_backlog.sql` → `judge_recommendations.sql:13-15`;
`rr.category` is `:43` not `:42`; `getJudgeBacklog` is `api.ts:2397-2406` not `-2410`.
