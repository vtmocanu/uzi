# PRD #593 — GitHub Projects sync completeness (design note, Part B PARKED)

**Issue**: [#593](https://github.com/vtmocanu/uzi/issues/593)
**Status**: Part A shipped (PR #595); **Part B PARKED** as a documented finding
**Priority**: Low (parked)
**Owner**: TBD

> This started as a two-part PRD. Review (two independent passes, 2026-08-22) reshaped it: Part A was already ~shipped, and Part B is a costlier, more speculative refactor than the first draft implied. Part A landed as the nudge (PR #595). Part B is **parked** here as an accurate design note — do not implement it until a concrete feature needs a truthful closed-issue-state cache. This file is the record so the analysis is not lost.

---

## Part A — DONE (PR #595)

The two panel-guidance gaps split as follows:

- **Done advisory** — *already shipped* in 0.56.0 (`web/src/pages/Repos.tsx:1428`, PRD #584 M4). The first draft wrongly claimed it was missing; both reviews caught this.
- **Column-by nudge** — genuinely missing; shipped in **PR #595** (`fix/593-column-by-nudge`): after provision/auto-create, the panel now tells the user to set the board's **Column by** to `uzi Status`. Frontend-only, mutation-tested.

## Part B — PARKED: decouple the issue-state cache from the board projection

### The problem (accurate)

The `issues` table is simultaneously (1) uzi's issue-state cache and (2) the source the board projects from. A **non-PRD** issue that closes is fetched by neither sync pass, so its cached `state` stays `'opened'` (a stale window) until the next `FullSync` evicts it — pinned by `api/internal/forgesvc/additive_sync_test.go:474` (`TestAClosedNonPRDIssueIsNeitherRefreshedNorKept`). Closed → Done (PRD #584) therefore works only for the PRD-labelled set, and no feature can trust `issues.state` for a non-PRD issue.

### Current architecture (verified — the parts a future implementer must not re-derive wrong)

- **`FullSync`** (`api/internal/forgesvc/service.go:428`) — unbounded, runs every Nth poller tick. Two fetches: PRD-labelled `StateAll` (`:430`) and all-open `StateOpened` (`:438`). It builds the eviction keep-set from the **union** of the two and calls `DeleteIssuesNotIn` (`:459`). This is the ONLY eviction path.
- **`IncrementalSync`** (`service.go:483`) — watermarked, runs the other ticks. Same two fetches, each bounded by its own `UpdatedAfter`. Never evicts. Returns the caller's marks unchanged on any error (`:495`, `:499`).
- **`Marks`** (`service.go:390`) — the watermark, a **pair** (`PRD`, `Open`) folded field-wise by `Advance`. It is held **in-memory** in the poller (`poller.go:107`), **deliberately not persisted** (`poller.go:99-105`: "Process-local by design… no stale mark to migrate or misread"). The pair split exists to fix issue #177 (a single shared mark stalls).
- **Board display** = `ListIssuesByRepo` (`api/internal/store/queries/forge.sql:311`), which is `SELECT * FROM issues WHERE repo_id=$1` — **no state filter**. So board membership == cache membership today; closed non-PRD issues stay off the board *only because they are evicted from the cache*.
- **`UpdatedAfter` / `StateAll` are already plumbed** at the forge layer (`forge.go:346` `ListIssuesOptions`; `github.go:319-320` maps `UpdatedAfter → Since`, and sends `state=all` for `StateAll`). No driver change is needed to fetch all-state incrementally.

### The three landmines a naive "track all issues" would hit

1. **Eviction (the sharpest).** `FullSync`'s keep-set is authoritative *only because both fetches are complete*. Make the additive fetch incremental/incomplete and `DeleteIssuesNotIn` deletes every stable open issue each reconcile. So you **cannot** simply watermark `FullSync`'s fetch.
2. **Cost.** The safe way to keep the keep-set complete is to widen `FullSync`'s additive fetch to `StateAll` **unbounded** — which pulls the full closed-issue history every reconcile, exactly the cost PRD #102 Decision 10 chose `StateOpened` to avoid (`forge.go:357-358`).
3. **Board would suddenly show closed issues.** Because `ListIssuesByRepo` is unfiltered `SELECT *`, keeping closed issues in the cache puts them on the board unless a display filter is added to preserve current UX.

### The two viable designs (for whenever this is un-parked)

- **Design A (simple, costlier).** Widen BOTH additive fetches to `StateAll`. `FullSync` stays unbounded/complete → keep-set safe (no landmine 1); `IncrementalSync` stays watermarked → cheap incremental catches. Add a state/label filter to `ListIssuesByRepo` so the board shows the same set as today (open ∪ PRD-labelled). Accepts landmine 2's cost on large repos.
- **Design B (harder, cheaper).** Decouple eviction from the fetch: evict only issues confirmed *deleted*, not those merely absent from an incremental fetch. Lets `IncrementalSync` (`StateAll` watermarked) carry the load without `FullSync` paying the full-history cost. Higher blast radius (reworks the keep-set invariant).

Either way: do **not** add a naive persistent `issues_synced_at` column — it reintroduces #177 (single mark) and the migration hazard the in-memory `Marks` deliberately avoids. If persistence is ever wanted, persist the **pair**. Autopilot is safe under both (its candidate query is guarded by `state='opened'` + both label predicates, `autopilot.sql:23-28`, so a larger cache adds zero candidates).

### Why parked

- **Value is speculative.** The user's intent is "UX unchanged, fix it behind the scenes" — so we would track closed non-PRD issues in the cache only to filter them back out of the board. The concrete symptom (#591-class staleness) is cosmetic; the real payoff is "a reliable issue-state source for a future feature" that does not exist yet.
- **Cost and blast radius are real** (landmines above; reworks `Marks`/keep-set/the #177 fix).
- **No consumer.** Best fit is to build this when a concrete feature needs truthful closed-issue state — e.g. extending closed → Done to all tracked issues as a *visible* feature, or issue-lifecycle analytics.

### Trigger to un-park

A concrete feature that needs the truthful closed-issue state of non-PRD issues. At that point, pick Design A (if the repos are small / cost is fine) or Design B (if the `FullSync` cost is unacceptable), and make the eviction-safety (landmine 1) the first, mutation-tested milestone.

## Provenance

- Reviews: two independent passes on 2026-08-22 (scope/milestones + fact-check of the baked citations). Both flagged the shipped Done advisory, the existing `Marks` mechanism, and (fact-check) the eviction landmine. All citations above re-verified against the code at that date.
- Part A (nudge) shipped as PR #595.
