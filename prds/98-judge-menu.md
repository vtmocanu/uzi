# PRD #98: Judge menu — a dedicated cross-run recommendation workbench

**GitLab Issue**: [#98](https://gitlab.example.com/vtmocanu/uzi/-/issues/98)
**Status**: In progress (2026-07-20) — branch `feature/prd-98-judge-menu`. **M8's e2e leg is deferred** until PRD #97 (e2e suite hardening) merges: it rewrites ~450 lines of `e2e/run-e2e.sh` and this PRD's e2e leg must be written against its `create_run` / `retry_read` / positive-control conventions, not the pre-#97 ones.

**Progress (2026-07-21, session end)** — 6 of 8 milestones landed. **NO MR opened yet.** Work resumes in THIS PRD, not a follow-up. Branch `feature/prd-98-judge-menu` @ `a5b65617`, 35 commits.

### RESUME HERE — the four things to do next, in order

1. **Finish the in-flight fixture fix (BLOCKING, partly written and uncommitted).** Two test files are dirty in the worktree: `api/internal/handler/judge_bulk_disposition_livedb_test.go` and `api/internal/store/judge_recommendations_integration_test.go`. **Three parts:** (a) give `bulkFixture` **two recommendations with different rationale texts** — today it writes `'because'` on every row, so folding `rr.rationale_md → 'because'::text` passes **and nothing in the live-DB suite catches it**; (b) add an **unfiled coordinate inside `autoRev`** — today `autoRev`/`handRev` are different reviews, so `f.review_id = rv.id` alone separates them and the filed-issues join's **coordinate predicate is unpinned by anything**; (c) **fix the assertion wording** — `judge_recommendations_integration_test.go:514` fails with *"the column is not row-scoped"* but fires on a wrong **value**; real row-scoping stays green. Without (c) the fix closes the holes and leaves the misleading label pointing at them. **Acceptance: both folds must redden after the fix. NOTE — parts of this fix may already be written (`bulkFixture`'s hardcoded `'because'` is now a bound parameter), but NO VALIDATOR HAS FOLDED THE CORRECTED FIXTURE. Status is "fix written, unverified", NOT closed — do not read "Blocking found" next to "fix landed" as closed; that is a commit proxying for the property. The fold is still owed and must prove three things: (i) the `rationale_md` pin reddens under `→ 'because'::text`; (ii) the join predicate becomes observable when BOTH coordinate halves are dropped, not just the target half — a weaker mutation passing while a stronger one fails is what made this worth checking, and stopping at the obvious one is how it was missed the first time; (iii) the `filed_at` assertion's message matches what it now actually checks.**
2. **M5** — the notification retarget. Land the **third `triage.todo` consumer** as it is written: the notification must read the canonical count, not re-derive it, and one test must mount nav badge + tab + notification **together** (mounted apart they have always all been correct — that is how the earlier badge/tab drift survived a whole milestone).
3. **M8a docs** — `docs/judge.md`, `docs/cli.md`, `specs/ai.md`. **Do not describe deferred work as shipped**: no e2e coverage exists, mock↔server fidelity is not asserted, and `OccurrenceFileIssue` has no tests.
4. **Then the MR** — after a review pass, and with the **migration re-check run fresh** (see the PRE-MR GATE in the checkpoint; it expires and its failure mode is every upgraded instance refusing to boot).

**Handoff document:** `.claude/agent-team-tasks/prd-98-m3-checkpoint.md` — branch state, standing rules with the incident behind each, the docker safety rule about the user's real stack, the projection-pin criterion, and the pre-MR gate.

- **Done, reviewed + audited**: M1 (`0874d3f6`), M2 (`30204a61`, later collapsed to one atomic statement), M6 (`d6a8545c`), M4 (`1da5ac32`), M3 (`c629ce28`), M7 (`de2d8de3`). Plus a merge of `origin/main` (`ad5abca1`, 38 commits — PRD #97 landed, which unblocked M8b).
- **In this MR**: M5, M8a docs, and the last open Blocking (a measured-false CLI instruction).
- **Still open IN THIS PRD after the MR** — see "Remaining work" below. Four items, all with recorded evidence.
- **Handoff notes**: `.claude/agent-team-tasks/prd-98-m3-checkpoint.md` is the resume document — branch state, standing rules with the incident behind each, the docker safety rule, and the pre-MR migration gate.

**What the review loop cost and bought** (recorded because it shaped the design): 3 Blocking in M3's first wave, then **every Blocking after M3 was about evidence rather than behaviour** — seven SQL projections no test executed, tests asserting properties of `encoding/json` rather than of the service, assertions credited for gates they sit behind, comments crediting guards in other files, and two printed CLI instructions that had never been run and were false. The implementation was sound throughout; the layer certifying it was not.

**Superseded progress note (2026-07-20, end of day)** — 5 of 8 milestones landed on `feature/prd-98-judge-menu`, no MR yet:
- **Done + reviewed + audited**: M1 (`0874d3f6`), M2 (`30204a61`, rewritten to one atomic statement in `082d8651`/`c962435d`), M6 (`d6a8545c`), M4 (`1da5ac32`). Full Go + web + live-DB gates pass at the tip.
- **Done, review PENDING**: M3 (Judge page + nav, `c629ce28`) — the largest milestone, first substantial `web/` surface. Gates green (web typecheck + 837 tests + build; api build/vet/test), the four validator pre-flags built in and test-pinned, but it has **not yet had a review/audit wave** — that is the first task next session. Six implementation decisions are flagged for confirmation in the M3 checkpoint; one (anchored deep-link defaulting to `bucket=all`, not `todo`) is a product-behaviour call touching M5.
- **Not started**: M7 (CLI), M5 (notification retarget — its `/judge` + `?run=` route dependency is now satisfied), M8a (vitest + docs + specs). **M8b (e2e) stays blocked** on #97.
- **One Blocking bug** found and fixed before merge: a duplicate-coordinate `SQLSTATE 21000` crash on legal judge output, invisible to fakes (the M2 fan-out collapse; fixed with `DISTINCT ON` on the resolved member set, `c962435d`).
- **Open follow-ups**: **AK** — a bucket-literal→constant sweep is partial; the producer side (`BucketOf` in `triage.go`, #94's shared helper) is deliberately left as a cross-PRD change, with `TestBucketConstantsMatchTheLadder` pinning the coupling meanwhile. **AL** — a comment-precision split in `judge_bulk_disposition.go`. Both tracked in the M3 checkpoint.
- The Design Decisions and Risks below were **corrected repeatedly against the code during implementation** and are current; several load-bearing claims (the `?run=` semi-join, the `EXPLAIN`-measured backlog cost, the `set_via` provenance mirror, the two-layer dedup asymmetry) were rewritten after being disproved by execution rather than review.
**Priority**: Medium
**Mockup**: static concept mock (ember shell + buckets + worklist + the three deltas) at the design artifact; **note** it renders the worklist grouped *by run* — a precursor. This PRD supersedes that with **group-by-target + dedup** (Decision 2); a revised in-repo mock lands with M3 as `prds/mockups/98-judge-menu-mock.html`.
**Depends on**: PRD #46 (the judge: `run_reviews` + `review_recommendations`, `users.judge_enabled`), PRD #68 (`recommendation_filed_issues`, the coordinate-keyed claim-first file flow), PRD #94 (`recommendation_dispositions`, the `bucketOf` ladder, `GET /me/judge/stats`, the global RunsList strip this promotes). Related: PRD #64 (the `uzi` CLI, second consumer), PRD #69 (the judge **control plane** — mode/model/spend/accuracy/consent; this PRD is the complementary output workbench, cleanly separable — see Decision 5's digest-scope note), PRD #47 (RunHealth badge — the per-row badge grammar this mirrors).
**Review**: fable adversarial pass on the concept mock folded in (2026-07-20). Load-bearing corrections adopted: **group by target with cross-run dedup**, not by run (the same recommendation recurs across runs and must be one triage decision, with frequency as the priority signal); **one canonical to-triage number** single-sourced from `triage.todo` so the nav badge, the notification, and the page tab cannot drift; the **/runs badge uses one verdict-first grammar** (`⚖ issues · 2`), not the mock's two grammars; the **triage state machine is already closed by #94** (the mock's flat three-button row under-represented it — Done / Dismiss ▾ Won't-do|Not-an-issue already exist). Deferred by review + user scoping: keyboard triage and target-file staleness → Future Work.
**PRD review**: a second fable adversarial pass, this one on the PRD itself and verified against the code (2026-07-20). Corrections folded in: **Filed→Done must be edge-triggered** — the poller holds a synced `issues.state` *snapshot*, not transition events, so a naïve "upsert done while closed" re-fires every tick and would make Undo impossible and overwrite a `dismissed` member; fixed with a `close_synced_at` edge marker + `INSERT … ON CONFLICT DO NOTHING` (Decision 6) — **so this PRD is NOT migration-free** (Decision 6 ships one migration). **Bulk file-as-one-issue is descoped to a follow-up** — it needs a repo pick, a human draft gate + the #68 sanitizer, `forgeLimiter`, and reverses #68's cookie+CSRF posture; v1 keeps clean bulk *disposition* + per-rec browser filing (Decision 3 + Open Questions). **M5 re-sequenced after M3** (it deep-links `/judge`, which M3 creates). The **/runs todo count buckets in Go, not a second SQL ladder** (#94 Decision 2 forbids a SQL `CASE`; Decision 7). Honesty edits: the grouped read is a **new, wider query** (same join *shape* as `/stats`, but adds a `runs` join + the filed row — Decision 1); **filed issues are NOT owner-scoped** (#68 Decision 8 lets an admin file on another user's review — Decision 6).

## Problem

The judge produces good output with no cross-run home. Every judged run gets a
verdict plus structured recommendations (`review_recommendations`, six
categories), and today that output surfaces in exactly three places: a
`judge_review` notification in the inbox, a **count-only** strip on the `/runs`
header (PRD #94, "Judge recommendations · all your runs"), and the full
`JudgePanel` on each run's detail page. What is missing is the place you actually
*work* the backlog:

- **There is no cross-run worklist.** To act on a recommendation you deep-link
  into one run at a time. The strip tells you *how many* are open; it gives you
  nowhere to *do* them.
- **The same recommendation is re-triaged in every run it appears in.** "Add
  `rg` to the worker image" or "coder re-ran a failing test without reading the
  error" recurs across many runs. Each occurrence is an independent coordinate
  with independent triage state, so the user files or dismisses the *same idea*
  N times. Nothing surfaces that a recommendation is recurring — which is
  precisely the signal that it is worth acting on.
- **Recommendations are about improving the factory, not reviewing a run.** The
  six categories (`enable_tool`, `install_worker_tool`, `adjust_template`,
  `improve_agent`, `add_agent`, `improve_uzi`) are all "make the factory better"
  actions. Anchoring them to the run that happened to surface them is the wrong
  spine for the work.

## Solution Overview

A dedicated **Judge** menu — a top-level destination in the **Factory** nav
group — that is the cross-run recommendation workbench. It reads the exact
owner-scoped aggregate PRD #94 already computes, but **dedups by
`(category, target)` across all your runs** and lets you triage a whole group in
one action.

- **Group by target, dedup across runs; frequency is the priority.** One row per
  `(category, target)`, with a "seen in N runs" evidence chip and an expander
  listing each run occurrence (run title, verdict, per-run triage state).
  Recurrence, not the judge's self-reported confidence, is the trustworthy
  ranking signal.
- **A group *disposition* action fans out to its member coordinates — no new
  storage.** The group is a *display* construct over #94's per-coordinate rows.
  "Dismiss" / "Mark done" applies the disposition to every open member (`bucket ==
  todo`); bulk multi-select does the same across several groups. **Filing** in v1
  stays the existing #68 per-recommendation browser draft (reachable from a
  group's occurrence expander); **bulk "file N as one issue" is a follow-up** — it
  needs a repo pick, a human draft gate, and a forge limiter #68 already imposes
  (Decision 3, Open Questions).
- **The notification stays a ping — its deep-link just retargets here.** No
  event is removed from the inbox; the `judge_review` row and its Slack DM now
  deep-link to `/judge?run={id}` instead of `/runs/{id}`. Mirror, don't move.
- **The strip leaves `/runs`; each run row gains a verdict badge.** The global
  count strip becomes the Judge page's bucket header. In its place, every run row
  gets a one-grammar `⚖ verdict · N` badge — a per-run glance it never had.
- **Inbox grouping + Filed→Done sync close the loop at scale.** The in-app inbox
  groups consecutive judge rows (no Slack digest — Slack DMs stay one-per-review);
  and when a filed issue *closes*, its recommendation auto-moves to Done — once,
  edge-triggered off the poller's synced issue state, never overwriting a human's
  own verdict.
- **One canonical number.** The nav badge, the notification, and the page's "To
  triage" tab all read `triage.todo` from #94's shared `bucketOf` helper. "Seen
  in N runs" communicates the grouping without minting a second count.
- **CLI parity in the same MR** (`uzi review backlog` + bulk *disposition* verbs;
  filing stays browser-only), per the CLAUDE.md second-consumer rule.

## Design Decisions

1. **The Judge page is a new READ endpoint — the *same join shape* as #94's
   `/stats`, but a genuinely new, wider query; the read model itself needs no
   migration.** `GET /api/me/judge/recommendations` (`RequireUser`, owner-scoped,
   all-time). #94's `ListJudgeTriageRowsForUser` returns only three columns
   (`queries/dispositions.sql`, the flat per-rec `(disposition_status,
   filed_settled, category)` the Go `BucketOf` needs). This endpoint keeps that
   join's spine — `run_reviews` where `user_id = caller` → `review_recommendations`
   → LEFT JOIN `recommendation_filed_issues` + `recommendation_dispositions` on the
   `(review_id, category, target)` coordinate — but **additionally joins `runs`**
   (for `issue_title` → `run_title`) and selects `run_reviews.verdict`, the rec's
   `confidence`/`rationale_md`, `rec_id`, and the filed row's `filed_issue_iid`/URL.
   So it is a **new query**, not a reuse of `ListJudgeTriageRowsForUser` verbatim —
   same shape, wider projection. It returns the rows **grouped by `(category,
   target)`**. Per group: `{category, target, occurrences: [{run_id, run_title,
   review_id, rec_id, verdict, confidence, bucket, filed_issue?}], open_count,
   run_count, rationale_preview}` where `rationale_preview` is the most-recent
   occurrence's `rationale_md`, truncated and length-capped, shipped as **plain
   text — NOT server-side HTML-escaped**. (Corrected 2026-07-20 against the code,
   which this PRD had wrong: the no-raw-render guarantee is **client-side**.
   `RunView.tsx:959` renders these fields "as escaped plain text (React's default
   + whitespace-pre-wrap), never markdown/HTML", and `apitypes/review.go:8` ships
   the scrubbed free text raw; secrets and control chars are already stripped at
   the review-POST ingest (`workersvc/judge_review.go`). Escaping server-side
   would double-escape in the SPA and print HTML entities into the terminal from
   `uzi review backlog`.) Each `bucket` comes from the shared **`bucketOf`** (PRD #94
   Decision 2 — same helper, no re-implementation), so the page's tab totals and
   the nav badge equal the existing strip exactly. A
   `?bucket=todo|filed|done|dismissed|all` filter (default `todo`) and a `?run=`
   anchor (for the notification deep-link, Decision 4) bound/scope the pull.
   **Response shape, settled at implementation (2026-07-20) — M2/M3/M7 build on
   this, they do not re-derive it:** the response is an envelope `{bucket, run,
   groups[], triage}`, not a bare array, and **`triage` comes from #94's own
   `/me/judge/stats` query, called directly — not tallied off the page rows.**
   (Revised 2026-07-20 once the pull became bounded: with a `LIMIT` in play,
   tallying `triage` off the returned rows would make the canonical number *wrong*
   on exactly the heavy accounts that need it most. Sourcing it from the stats query
   makes "nav badge == notification == To-triage tab" **literally the same query**
   rather than equal-by-construction, and it survives both filters and truncation.
   Cost: one extra cheap 3-column query per request.) Each
   group carries its explicit rollup `bucket` alongside `open_count`/`run_count`, so
   neither the page nor the CLI recomputes the ladder. `?run=` filters **which
   groups** return (those with ≥1 occurrence in that run) but never trims a kept
   group's occurrence list — arriving from a notification must still show that the
   recommendation recurs elsewhere, since that recurrence is the priority signal.
   **`?run=` is therefore a coordinate-level SEMI-JOIN in SQL, not an equality
   predicate and not a Go post-filter** (settled 2026-07-20; this note previously
   said "applied in Go, do not fix it into the WHERE clause" and was superseded
   within the hour — see below). The naive reading is a false dilemma: an equality
   `WHERE rv.target_run_id = @run` does trim exactly the occurrences Decision 4
   exists to preserve and corrupts `run_count`, but a Go post-filter is not the only
   alternative. The shipped form selects *coordinates* —
   `EXISTS (… WHERE rv2.user_id = rv.user_id AND rv2.target_run_id = @run_anchor
   AND rr2.category = rr.category AND rr2.target = rr.target)` — so a group opened
   from a notification keeps its other-run occurrences while coordinates absent from
   the anchor run drop out, and the bound applies in the database rather than after
   a full materialization. The subquery is scoped to the caller's own reviews, so an
   anchor naming another user's run matches nothing (no oracle).
   **Once the row cap exists, a Go post-filter is not merely inferior — it is a
   defect.** The `LIMIT` would apply *before* the anchor filter, so an anchored
   request whose coordinates fall outside the newest `JudgeBacklogMaxRows` rows
   would return empty while reporting `truncated: true` — the notification
   deep-link's worst case, silently, on exactly the heavy accounts the cap exists
   for. In SQL the bound applies after the anchor. The two designs were equivalent
   before the cap and are not equivalent after it, which is why this note is
   load-bearing rather than stylistic.
   **`nullableUUID`, not the shared `pgUUID`, carries the "no anchor" case**: `pgUUID`
   always sets `Valid=true`, so passing `uuid.Nil` would have sent the all-zero uuid
   as a *real* anchor, the query's `IS NULL` escape hatch would never fire, and
   **every unanchored backlog request would have returned nothing**. Caught by the
   live-DB test, not by a fake.
   A malformed uuid or unknown bucket is a **400**; a well-formed unknown/foreign
   run uuid is an **empty list, not a 404** (no existence oracle) — so a CLI typo
   can never look like an empty backlog.
   **Exception, user-decided 2026-07-21: an ANCHORED `/judge?run={id}` defaults to
   `?bucket=all`, not `todo`.** Un-anchored `/judge` still defaults to `todo` as
   above. The reason is a direct consequence of Decision 2's cross-run dedup: a
   notified run's coordinate may already be **settled via a different run**, so a
   `todo` default would deep-link a fresh `judge_review` notification to an
   apparently-empty page, with no hint the item exists under another bucket — the
   worst possible first impression for the feature, and precisely the "row vanished"
   confusion the `bucket=all` re-read exists to prevent elsewhere. M5's notification
   deep-link therefore lands on `all`. Do not "correct" this back to `todo` for
   consistency with the un-anchored default; the two defaults differ deliberately. `rationale_preview` is capped at
   `RationalePreviewMaxRunes = 280` **runes** (never bytes — a byte cut splits
   UTF-8), ellipsis appended only on an actual cut. Occurrence order is
   `rv.updated_at DESC` (most-recently-**judged** first, so a re-judge counts as
   recent), not `created_at` — a re-judge upserts in place, bumping `updated_at` and
   leaving `created_at`, so `created_at` ordering shows staler preview text than the
   group actually contains. **This depends on a property a future PRD could silently
   break:** `judge.sql`'s `ON CONFLICT (target_run_id) DO UPDATE … updated_at =
   now()` is currently the **only** writer of `run_reviews.updated_at` anywhere in
   `queries/` (verified 2026-07-20), so the column moves only on a re-judge. If any
   later change adds a status-change or bookkeeping write to that column, this
   ordering silently stops meaning "freshest rationale" and the preview regresses
   without a test failing. **The
   read is migration-free; the PRD as a whole is not — Decision 6 ships one
   migration.**

2. **Grouping grain is `(category, target)`; occurrences stay per-run; triage
   state stays per-coordinate.** Because #68/#94 key filed/disposition on
   `(review_id, category, target)` — **per review** — the "same" recommendation in
   two runs is two coordinates with independent state. The menu groups them for
   display and priority; a group is **not** a stored entity and needs no new
   table. Consequences, stated so the counts are unambiguous:
   - The **nav badge and the "To triage" tab count are per-recommendation** (the
     existing `triage.todo`), NOT per-group — so they equal PRD #94's strip and
     `uzi review stats` to the digit. "Seen in N runs" carries the grouping.
     (This is the deliberate resolution of the review's competing-numbers worry:
     one canonical to-triage number; the group count is never a second badge.)
   - **"Open" means `bucket == todo`** (a *filed* member is not open — it is on the
     ladder's `filed` rung). `open_count` = members with `bucket == todo`; a group
     is under **To triage** iff `open_count ≥ 1`. A fully-settled group rolls up to
     the highest state among members via the #94 ladder (`dismissed > done > filed
     > to-do`) — so a group of 3 done + 1 dismissed shows under **Dismissed**
     (highest wins; a display quirk, documented). The occurrence expander always
     shows the per-run truth, so a mixed group (2 dismissed, 2 open) is never
     misrepresented.

3. **A group *disposition* action FANS OUT to member coordinates — reusing #94's
   mutation semantics unchanged, owner-only. Bulk *filing* is descoped to a
   follow-up.** The one new bulk endpoint in v1:
   - `PUT /api/me/judge/recommendations/disposition`
     `{items: [{category, target}], status, reason?, scope: open|all}` — resolves
     the caller's member coordinates for each item and upserts a disposition on
     each (idempotent per #94 Decision 6; re-stamps the `rationale_hash` per its
     Decision 3). `scope=open` (default) touches only members with `bucket == todo`
     (Decision 2's definition — a filed member is left filed); `all` re-asserts.
     Returns the updated groups. **Owner-only by construction**: every member is
     caller-owned — the resolve is scoped `user_id = caller` (the
     `SubmitInput(user.ID)` / #94 Decision 5 strict-ownership pattern, verified in
     `handler/review_disposition.go`), so a uza_ `admin_ro` token can only ever
     dispose its **own** rows and `IsAdmin` is never consulted. This is the clean
     half: a local, non-forge, non-spend upsert applied N times.
   - **Bulk "file N as one issue" is NOT in v1** — fable's PRD pass showed it is a
     mini-PRD, not a #68 reuse. #68's `FileIssue` (`handler.go:680-687`) files into
     a **user-picked repo** (`GetRepoForUser`, caller-owns-repo — and #68 Decision 4
     notes the default repo is *unresolvable* for 4 of 6 categories for
     non-admins), behind a **human-editable draft** it calls "the primary control"
     for cross-project secret leakage (#68 Decision 10's fence/`/`-strip/secret
     scan), on the **cookie+CSRF `RequireAuth` path with `forgeLimiter`**, and is
     deliberately **browser-only** (filing is out of scope for the CLI, #68
     Interactions). A bulk endpoint on `RequireUser` with a server-templated
     aggregate body would drop the repo pick, the draft gate, and the limiter, and
     reverse the auth posture (letting a uza_ token drive a forge *issue* write). So
     **v1 keeps per-recommendation filing on the existing #68 browser draft**,
     reachable from a group's occurrence expander; **bulk file-as-one-issue (repo
     pick + aggregated draft + limiter + posture decision) is a follow-up PRD.**
     (Recorded as an Open Question — the one scope item to confirm.)
   - **Multi-select** across groups (the checkbox bar) is a multi-item call to the
     disposition endpoint — the UI batches selection, the API takes a list of
     `(category, target)` coordinates in one round-trip.
   - **Settled at implementation (2026-07-20), both deliberate:** the N upserts are
     **not wrapped in a transaction**. Each is local, side-effect-free and
     last-writer-wins, with no forge write and no spend to make exactly-once — #94
     Decision 6's own reasoning — so a partial failure is safely retried and
     converges. **A mid-fan-out failure surfaces as a plain 500 and the partial
     apply is NOT reported** (measured, not reasoned, in the M2 review: with the
     2nd of 3 upserts failing, one upsert had landed, the service discarded the
     `updated` counter and returned a generic 500, and the re-read never ran).
     That is accepted: a 500 makes no false claim of success, the landed subset is
     visible on the next read, and a retry converges because every upsert is
     idempotent. What is NOT accepted is describing it otherwise — the requirement
     is that the endpoint never *claims* completeness it does not have. Moving to a
     partial-success report (207, or 200 with a `partial` flag) would be a design
     change, deliberately not taken in v1. The response is `{updated, groups, triage}` with **`groups` re-read at
     `bucket=all`** — the subtle part: a just-dismissed group has left To triage but
     must still come back so M3 can re-render the row instead of having it vanish
     mid-interaction. **M3 must not "optimize" that re-read down to the active
     filter**; doing so reintroduces exactly that flicker. `items` is capped at
     `JudgeDispositionMaxItems = 100`, deduplicated **before** the cap check so the
     cap counts distinct work rather than body length.
   - **The item cap does NOT bound the fan-out — members-per-coordinate does, and
     it is unbounded** (M2 audit, 2026-07-20). One coordinate matches *every*
     occurrence across *all* the caller's reviews, and the resolve carries no
     `LIMIT`, so ≤100 coordinates in a ~4 KB body can drive tens of thousands of
     sequential upserts, each its own round-trip, holding a pool connection, on a
     mount with no rate limiter. Self-inflicted, own-data and idempotent, so not a
     vulnerability — but it made M2 materially less bounded than M1, which caps at
     2000 rows. **Resolution: collapse the N upserts into ONE multi-row `INSERT …
     ON CONFLICT` driven by `unnest` of the RESOLVED coordinates.** That removes the
     round-trip amplification and the partial-failure window in a single move while
     keeping the resolved-not-body invariant intact — and it supersedes the
     "fail-fast, partial apply surfaces as a 500" note above, since one statement
     cannot partially apply. **That collapse REQUIRES a `DISTINCT` on the
     coordinate** (verified against live Postgres, 2026-07-20, before the code was
     written): `review_recommendations` has **no unique constraint on `(review_id,
     category, target)`** — only `pkey(id)`, the partial `improve_uzi` index, and
     `idx(review_id)` — because a judge may legitimately emit the same coordinate
     twice in one review. So the resolve can return that coordinate twice, and a
     multi-row upsert over it raises `ON CONFLICT DO UPDATE command cannot affect
     row a second time` (SQLSTATE 21000) at runtime: rare, data-dependent, and
     invisible to any fake. `dedupeCoords` does **not** cover this — it dedupes the
     *request* coordinates, while the duplication arises inside the *resolved member
     set*. **The member set stays deliberately UNBOUNDED in v1, and a hard cap was
     considered and rejected** (2026-07-20). Collapsing to one statement removed the
     round-trip amplification and the partial-apply window, but not the bound: the
     item cap bounds **coordinates** (100), not **members**, since one coordinate
     matches every occurrence across all the caller's reviews. What the collapse
     traded is many short autocommit statements for **one statement whose parameter
     arrays scale with member count, in a single transaction holding row locks on
     every affected row for its duration** — better on round trips, longer on lock
     hold under concurrency. A cap returning 400 was drafted and **withdrawn**: it
     would make a large group *permanently un-dispositionable*, which is precisely
     the failure the SQLSTATE 21000 crash caused and which this PRD just fixed —
     reintroducing it by policy rather than by bug is not an improvement. If it is
     ever bounded, the shape is a `LIMIT` on the resolve paired with a `truncated`
     signal so the client repeats (mirroring M1), **never** a rejection. The
     operation is own-data, idempotent and authenticated; the honest
     characterisation lives in the query comment where the next reader will find it.
     Two arguments settled it, both stronger than "the heaviest user is
     inconvenienced". **A single group can exceed any cap on its own** — members
     expand per coordinate, so one coordinate recurring across a long history blows
     the cap by itself, and there is nothing smaller for the user to select. The
     action becomes permanently impossible for *precisely* the recommendation this
     feature exists to surface, since frequency is the priority signal and the
     most-recurring group is both the most valuable and the first to exceed. And
     **members are invisible to the client** — the UI shows groups and `run_count`,
     and nothing anywhere tells a user that three selected groups expand to 4,000
     members — so "select fewer" is guess-and-check against a quantity they cannot
     observe. An unactionable error is bad; one that is unactionable *in principle*
     is a dead end.
     **If protection is ever wanted, the instrument is a per-user rate limiter on
     the `/me/judge` group, not a member cap.** Re-derived at `415d08bb`: that group
     mounts `RequireUser` and nothing else — no limiter — and carries all three
     routes. The real exposure there is **repetition, not any single request's
     size**: `GET /me/judge/stats` is completely unbounded and, per the measured
     plan above, seq-scans `review_recommendations` in full on every call. A member
     cap would harden the one route that is already atomic and self-limiting while
     leaving the two cheaper-to-abuse reads open. The codebase already has the
     pattern (`judgeLimiter`, `forgeLimiter`, `hostedLimiter` are per-user
     middleware). Deliberately **not** done here: it should be triggered by an
     observation rather than a theory.
     The per-member loop was immune only because each upsert was its own
     statement, which is a reason nobody had written down until changing the shape
     removed it. **This was missed on the first attempt and shipped a hard 500** —
     worth recording *why*, because the reason is structural rather than careless.
     The bulk suite's fake `memberRow` helper mints `ReviewID: uuid.New()` per
     member, so every member the fake can produce carries a **distinct**
     `review_id` — the fake is incapable of constructing the colliding triple. The
     headline test of the new statement (`TestBulkDispositionIsOneRoundTrip`, 500
     members on one coordinate) passes precisely because those 500 members all have
     different review ids, and no live-DB test seeded a duplicate. **The one shape
     that breaks the write was the one shape no existing test could construct.** The
     `DISTINCT` must therefore be keyed `(review_id, category, target)` on the
     RESOLVED member set, and the fixture helper must be able to mint members
     sharing a `review_id` — otherwise the next change hits the same wall. A test
     helper that cannot construct the failing input silently bounds every test built
     on it.
     **The dedup key is the full `(review_id, category, target)` triple and MUST NOT
     be reduced to `(category, target)`** — caught during implementation, before it
     shipped. Keying on the pair looks natural, because the pair is what the request
     carries and what the group is named by. But members legitimately repeat a
     coordinate **across different reviews**, and that recurrence is the entire
     premise of this PRD. Deduping on the pair would have silently disposed **one
     run per group instead of all of them** — the fan-out this endpoint exists to
     perform. It is the same shape as the crash it was fixing: a guard whose
     correctness depends on a distinction nobody had written down. The dedup test
     therefore carries a **negative control** — reducing the key to the pair must
     fail with "wrote 1 members, want 2".
     *(This note originally said the pair-keyed version would have shipped "with
     every existing test still green". **Measured false** in the `e4934c2c` review:
     re-keying to the pair is caught in two independent places — the fake-backed
     control, and three live-DB tests led by `TestBulkDispositionFansOutAcrossRunsLiveDB`
     with `updated = 1, want 3 (one per run the coordinate recurs in)`. So the suite
     would have caught it; the coder self-caught it earlier, which is better, but the
     safety net was real. The claim was the implementer's, relayed by the lead
     without checking — the same inherited-assertion failure this PRD keeps finding,
     this time in the PRD's own prose. The audit added the symmetry that explains
     it: those pre-existing multi-member fixtures build members with `memberRow`,
     which mints a **fresh `ReviewID` per member**, so every one of them is
     implicitly a *cross-review* fixture — exactly what a pair-keyed dedup destroys.
     **The same fixture limitation that made the same-review duplicate
     unconstructible, and so hid the 21000 crash, is what would have exposed the
     pair-key mistake.** One property, two opposite effects, depending on which bug
     you are hunting.)*
   - **The Go dedup layer is deliberate defence-in-depth, and adding it was not
     free — state both halves.** On the live path it is **dead code by
     construction**: the resolve is `SELECT DISTINCT ON (…)`, so it cannot return a
     duplicate triple and `seen[key]` can never fire against a real database. It is
     exercised only by fake-backed tests — which is the point, since a fake cannot
     model SQL and a fake-backed duplicate test would otherwise be theatre. It earns
     its place the day someone relaxes the `DISTINCT ON` or adds a second caller of
     the write. **But divergence between the two layers is asymmetric, and the
     dangerous direction is the new one:** a wrong *SQL* key is masked by the Go
     layer (which still keys on the conflict key, so the statement stays legal —
     degraded, not broken), while a wrong *Go* key is **not** masked by SQL, because
     the Go pass runs downstream and can *remove* members the SQL correctly kept —
     silently under-disposing. So the second layer added a way to be wrong that did
     not exist before it, which is why the Go layer is the one that must carry the
     test. "Add a second layer" is not automatically free. (Implementation detail
     that is safe *for a reason* rather than by luck: `seen[key]` is marked **before**
     the scope switch, so a member excluded by scope still consumes its key — fine
     only because duplicates of one triple always carry identical
     `disposition_status`/`filed_settled`, since those joins are on the coordinate
     and not on `rr.id`, so both copies take the same scope branch anyway.)
   - **Why "write the resolved row, never the body" is defence-in-depth rather than
     the mechanism** (recorded so a future refactor does not undo it): the resolve
     matches by *equality* (`want.category = rr.category AND want.target =
     rr.target`), so for any row that matches, the resolved values are
     byte-identical to the body values. The actual security mechanism is the JOIN
     plus the owner predicate yielding **zero rows** for anything bogus. The two
     become observably different only if the match is ever loosened
     (case-insensitive, `LIKE`, trimming) — at which point writing from the body
     would start writing attacker-shaped text. So the rule stands even though no
     test can distinguish it today; that limit is inherent to the design, not a
     coverage gap.

4. **The notification is KEPT as a ping; only its deep-link retargets to the
   Judge menu.** The `judge_review` payload already anchors `run_id` + `review_id`
   (#46/#94) and the inbox is a generic surface (`notifysvc` untouched — "the
   judge is simply tenant #1"). No payload change; two link changes:
   - Slack DM: `reviewDeepLink` (`handler/judge_worker.go:318`, today
     `baseURL + "/runs/" + targetID`) → `baseURL + "/judge?run=" + targetID` (a
     true one-liner).
   - Web inbox: **kind-conditional, not a one-liner.** `Notifications.tsx` links
     `/runs/${n.run_id}` generically for **any** kind carrying a `run_id`
     (`Notifications.tsx:59`), so the retarget is a `kind === 'judge_review'` guard
     that routes to `/judge?run={id}` while every other kind keeps `/runs/{id}`.
   `/judge?run={id}` opens the menu scrolled/filtered to that run's occurrences —
   which requires the M1 endpoint's `?run=` anchor (Decision 1) or a client-side
   occurrence filter, so **M5 depends on M3** (the route + the filter), not Phase
   1. The inbox row itself stays — the ping's job ("a review landed while you were
   away") is preserved; only its destination changes.

5. **Digest is web-inbox only; NO Slack digest.** At factory throughput a stream
   of one `judge_review` row per finished run floods the *in-app inbox*, so
   `Notifications.tsx` groups consecutive `judge_review` rows under one expandable
   "N reviews ready" header, keyed on `kind` + a time window. Rows are still
   individually persisted (their `run_id`/`review_id` anchors and read-state
   survive) — grouping is render-only, no new storage, no scheduler.
   - **Slack DMs are left exactly as they are — one DM per review, un-throttled,
     un-batched** (the existing #46/#94 best-effort judge DM). This PRD changes
     only its **deep-link** (Decision 4), never its cadence. No throttle, no
     roll-up, no `judge_dm_throttle` state. [user-decided 2026-07-20]
   - **Scoped to `kind == judge_review`.** The inbox grouping keys strictly on the
     `judge_review` kind, so it never groups a *different* judge notification — in
     particular PRD #69 M7a's deterministic pre-start **infra-skip** notification
     (a distinct kind) renders as its own row. And because there is no Slack digest
     at all, nothing can delay that infra DM — the #56 (Slack notifications UX)
     seam evaporates.

6. **Filed→Done sync — edge-triggered off the poller's synced state, never
   overwriting a human verdict. This is the PRD's one migration.** #68's
   `recommendation_filed_issues` links a coordinate to a forge issue
   (`filed_issue_iid` + `filed_repo_id`). Crucially, **the poller has no transition
   events** — `issues.state` is a synced *snapshot* (`store/migrations/00002_forge.sql`;
   `forgesvc` FullSync/IncrementalSync upsert the cache). A naïve "upsert `done`
   while the linked issue's cached state is closed" is **level-triggered**: it
   re-fires every tick, so a human **Undo** is silently re-applied on the next
   sync, and reusing #94's `ON CONFLICT DO UPDATE` upsert would **overwrite a
   member the user had already `dismissed`** with `done`. Both are wrong. So:
   - **Edge marker:** add `close_synced_at TIMESTAMPTZ` (nullable) to
     `recommendation_filed_issues`. The post-sync pass acts on a linked issue only
     on the open→closed **edge** (cached `state = closed` AND `close_synced_at IS
     NULL`), then stamps `close_synced_at` — exactly once per close.
   - **Never overwrite:** the disposition is written `INSERT … ON CONFLICT DO
     NOTHING` (NOT #94's DO-UPDATE upsert), so a coordinate the user already
     dismissed/marked keeps their verdict, and after Undo deletes the row the edge
     is already consumed — Undo **sticks**. A reopen does not re-open (no
     auto-reopen; flapping avoided); a re-close does nothing (`close_synced_at`
     set).
   - **Provenance + ownership:** the row carries `set_via='issue_close'` (a nullable
     column on `recommendation_dispositions`, default `NULL`) so the UI labels
     "done via #IID"; `set_by_user_id = NULL` marks the system action.
     **A human write MUST clear `set_via` back to `NULL`** (found in the M6 review,
     2026-07-20, and measured against live Postgres). The tests covered
     dismiss-then-close; the **reverse order was wrong**. #94's
     `UpsertRecommendationDisposition` DO-UPDATE sets status, reason, hash,
     `set_by_user_id`, `set_at` and `updated_at` but **never touches `set_via`** —
     the column did not exist when that query was written. So a human overriding an
     auto-done leaves a row claiming `set_by_user_id = <the human>` **and**
     `set_via = 'issue_close'` simultaneously, and the UI would render that human's
     `dismissed` verdict with system provenance — destroying exactly the
     auto-vs-human distinction this decision exists to preserve. It is the precise
     mirror of PF-4: that stops a system action being attributed to a human, this
     attributes a human action to the system. **Fix: `set_via = NULL` in that
     DO-UPDATE**, plus a live-DB test for the auto-done→human-override ordering
     beside the existing reverse-order one. **This must land before M3 renders the
     label**; M6 created the interaction by adding the column, so M6 owns it.
     *(Both validators found this independently and proposed different one-liners:
     `set_via = NULL` versus `set_via = EXCLUDED.set_via`. They are equivalent
     today only because the INSERT column list omits `set_via`, so `EXCLUDED` is
     NULL. `NULL` is chosen deliberately: it states the invariant — a human write
     always means human provenance — instead of depending on a column list
     elsewhere in the same statement staying as it is. If someone later adds
     `set_via` to that INSERT list, the `EXCLUDED` form would silently start
     carrying system provenance through a human write, with no edit to the line
     that guarantees it. That is exactly the class of latent breakage this PRD has
     been finding all run.)*
     **Postscript (2026-07-21): the visible half is what makes the invisible half
     checkable.** `set_via` reached the wire only at M3's B3 fix — before that it
     lived entirely inside `api/internal/store`, so no consumer could distinguish an
     auto-done from a hand-marked one and the PRD documented a label that did not
     exist. The moment the field became visible, the **mock reproduced this exact
     misattribution**: `mockApi`'s disposition upsert used `Object.assign(existing,
     next)`, which copies only the keys `next` carries, so a human overriding an
     auto-done **kept** `set_via='issue_close'` and the chip went on reading "Done
     via #91" after the user had overridden it. That is precisely what the literal
     `NULL` above prevents server-side, re-created client-side the instant the field
     had a reader — and it was uncatchable before, because nothing could observe the
     value. Fixed in both mock write paths with an end-to-end override test. The
     general lesson: **a provenance field no consumer reads cannot be tested, only
     asserted** — descoping the visible half would have left the invisible half
     permanently unverifiable. The
     disposition lands on the **review owner's** coordinate regardless of who
     filed — **filed issues are NOT owner-scoped** (#68 Decision 8 keeps admin
     filing on another user's review; `filed_by_user_id` may be an admin).
   - **Join the issue cache on `(repo_id, forge_issue_iid)` — never on iid alone —
     and skip rows whose `filed_repo_id IS NULL`** (audit requirement, 2026-07-20,
     verified against the code). `issues` is keyed `ON CONFLICT (repo_id,
     forge_issue_iid)` (`queries/forge.sql:174`): an iid is **per-project, not
     global**. Since `filed_repo_id` is `ON DELETE SET NULL`, a NULL-repo row joined
     on iid alone would match any repo's issue with that number — closing issue #7
     in repo X would auto-Done a recommendation filed as #7 into repo Y, cross-repo
     and possibly cross-user. Excluding NULL-repo rows makes the documented
     disabled-repo no-op below a **safe** no-op, not just a silent one.
   - **Preconditions (documented limits):** the issue cache only holds
     **PRD-labeled issues of enabled repos** (`forgesvc/service.go` — reconcile
     evicts de-labeled issues), and `filed_repo_id` is `ON DELETE SET NULL`. So a
     filed issue that loses its PRD label, or whose repo is disabled/disconnected,
     is no longer observable and **won't auto-Done** — a silent no-op, called out
     here and in `docs/judge.md`, not a bug.
   The #94 ladder then buckets an auto-done as **done** — it leaves To triage and,
   for `improve_uzi`, the self-improvement backlog (#94 Decision 9). Rides the
   existing poll loop; no new worker. **Migration:** `set_via` on
   `recommendation_dispositions` + `close_synced_at` on `recommendation_filed_issues`
   — one migration, draft-numbered above the live head and renumbered at merge per
   CLAUDE.md.

7. **Per-run verdict badge on `/runs` — verdict via a safe single join, the count
   bucketed in Go (never a second SQL ladder); the strip moves out.** `RunListItem`
   (`web/src/lib/api.ts` — today carries `mr_state`, `health`, … but **no** judge
   field) gains `judge_verdict` (`ideal|ok|issues|null`) and `judge_todo_count`.
   Two different mechanisms, deliberately:
   - `judge_verdict`: a **safe LEFT JOIN `run_reviews` ON `target_run_id = run.id`**
     in the list query (`handler/runs.go` / `ListRunsForUser`). `target_run_id` is
     UNIQUE, so this stays strictly one-row-per-run — no fan-out.
   - `judge_todo_count`: **NOT** computed in SQL. Joining through
     `review_recommendations` would fan the run list out (≤50 recs/review → up to
     50 duplicate run rows, breaking `ListRunsForUser`'s one-row-per-run
     contract), and counting `todo` in SQL means re-implementing the ladder's
     bottom rung (`disposition IS NULL AND filed_at IS NULL`) — which #94 Decision
     2 categorically forbids (no SQL `CASE`, one Go `BucketOf`). Instead the
     handler fetches the per-rec rows **for the runs on the page** and buckets them
     with the shared `BucketOf`, attaching `judge_todo_count` per run in Go.
   The row renders **one** compact badge, verdict-first with the count appended
   only when `> 0` (`⚖ issues · 2`, `⚖ ideal`) — a single grammar, fixing the
   mock's two-grammar bug, mirroring the RunHealth badge (#47). Click → the run's
   `JudgePanel` (unchanged). The global `TriageSummary` strip is **removed** from
   `RunsList.tsx` (PRD #94 Decision 8's header render + its `getJudgeStats` call) —
   but that removal lands **with M3** (the Judge page header is its new home), so
   the aggregate count is never homeless. `GET /me/judge/stats` stays.

8. **The empty / inbox-zero state is a first-class view — because to-triage = 0
   is the goal.** When `triage.todo == 0`, the Judge page is not blank: it shows a
   recent-verdict trend (last N runs' verdicts, from the same join), recently
   Filed / Done groups, and — if the user has not opted into the judge
   (`users.judge_enabled`, #46/#69) — an opt-in card linking Settings. A
   badge-less nav item most of the week is expected; the zero state is what keeps
   the destination worth opening and earns the top-level slot.

9. **Nav placement: the Factory group, after Workers.** `<NavGroup label="Factory">`
   (`AppShell.tsx:304`) today holds Agents / Skills / Workers; Judge joins as
   `<NavItem to="/judge" label="Judge" badge={judgeTodo} />`. The categories are
   all factory-improvement actions, so the group is thematically exact. The badge
   is the existing `NavItem` unread-count pill (`AppShell.tsx:106-145`, `>99 →
   99+`), fed by a poll of `/me/judge/stats.todo` owned by `AppShell` alongside
   the notifications-unread poll (`AppShell.tsx` M2 poll). Name is **Judge** (not
   "Reviews"): it matches the run kind (`RunKindJudge`), the `JudgePanel`, the
   `uzi review` CLI group, and the Settings opt-in; "Reviews" collides with
   MR/code-review connotations. The page subtitle ("Recommendations across all
   your runs") carries the backlog framing.

10. **CLI parity — `uzi review backlog` + bulk *disposition* verbs**
    (`api/cmd/uzi/review.go`, the #94 group). `uzi review backlog [--bucket
    todo|filed|done|dismissed|all] [--run <run-id>] [--json]` prints the deduped groups (`category ·
    target · seen in N runs · open N`) from the M1 endpoint; `uzi review
    resolve|dismiss --category C --target T [--reason wont-do|not-an-issue]` drives
    the M2 bulk **disposition** endpoint (group fan-out). **No `uzi review file`** —
    filing stays browser-only (#68 Interactions kept it out of the CLI, and bulk
    file-as-one-issue is a follow-up, Decision 3). The existing per-run `uzi review
    show/resolve/dismiss/undo/stats` stay. The web-only surfaces (nav badge, inbox
    grouping, per-row `/runs` badge) have no CLI analogue and are called out as
    such.
    **Correction 2026-07-21: `--run` was added, and this decision's flag list did not
    originally name it.** The omission was treated as deliberate during M7 and the flag
    was left out; the review then measured why that was wrong. The truncation warning
    told the user to "re-check with `uzi review backlog --bucket all`", and **no bucket
    value can reach what the cap cut**: `truncated` is computed and the rows sliced
    *before* `filterGroups` runs, so every bucket truncates identically (measured with
    the cap lowered to 2 against a 9-row fixture — `all`, `todo`, `filed`, `done` and
    `dismissed` all returned `truncated=true`, and `all` returned the same surviving
    groups as `todo`). The `?run=` anchor is the **only** predicate pushed into SQL
    *before* the `LIMIT` (Decision 1's semi-join), so it is the only parameter that can
    change what gets cut — which made it the only possible remedy for a warning the CLI
    was already printing. The flag list described the surface being specified, not a
    prohibition on the endpoint's other parameter, and CLAUDE.md's second-consumer rule
    points the same way: a route capability only the web drives is the "CLI silently
    stale" case. Both truncation warnings now name `--run` (and `--json` on the original
    call, which is the only complete record of what a write did).

**Interactions (for completeness):** **Hosted workers / claim / agent code** are
untouched — API + web + CLI + poller only. **Run deletion** cascades the review,
its recommendations, dispositions (#94 CASCADE) and filed links (#68) away; a
deleted run simply drops out of the owner-scoped join, so its groups shrink or
vanish with no orphan work. **`improve_uzi` self-improve backlog** (#94 Decision
9) is unchanged in mechanism: a group dismiss/done or a Filed→Done sync writes the
same disposition rows the engine already excludes on. **Board cache** is
untouched by triage (no forge write); only the *file* path writes a forge issue,
exactly as #68 already does.

## Milestones

- [x] **M1 — Grouped read model (api)** — DONE `0874d3f6`; review wave dispatched.: `GET /api/me/judge/recommendations`
      (`RequireUser`, owner-scoped, `?bucket=` filter + `?run=` anchor) — the new
      **wider** query (the #94 join shape plus the `runs` join for `issue_title` and
      the verdict/confidence/filed projection, Decision 1), returning groups keyed
      `(category, target)` with the occurrence list, `open_count` (= `bucket==todo`),
      `run_count`, and a plain-text (NOT server-escaped) `rationale_preview`.
      **Read is migration-free.**
      Store/handler test: dedup groups the same `(category, target)` across ≥2 runs
      into one group with a correct occurrence list; the bucketed totals equal `GET
      /me/judge/stats` for the same fixture (shared `BucketOf`, no re-implementation).
- [x] **M2 — Bulk disposition (api)** — DONE `30204a61`; review wave dispatched.: `PUT .../recommendations/disposition`
      (Decision 3) — coordinate fan-out reusing #94's idempotent disposition upsert,
      `scope=open|all` (`open` = `bucket==todo`). Owner-only authz matrix (owner fans
      out; non-owner → 404; uza_ `admin_ro` → 404 on another user's rows, allowed on
      its own; `IsAdmin` never consulted); idempotent double-call; a partial group
      (some members already settled) dismisses/marks only the open ones. Depends on
      M1 (shared coordinate-resolve helper + DTO). **Bulk filing is NOT here** — see
      the follow-up note below. **Audit requirements folded in 2026-07-20** (each
      re-derived from the code): this endpoint is the **first place a
      `category`/`target` arrives from a request body**, and `00071`/`00073` both
      carry a verbatim comment that they omit a category CHECK *on purpose* because
      "the handler never accepts a category from the request body" — so the
      disposition row must be written off the **resolved** recommendation, never
      echoed from the body (a bogus coordinate then resolves to zero members and
      writes nothing); `len(items)` is **capped** (400 above it) since this is N
      resolves + N upserts on a no-CSRF token path; and the response carries **no
      per-item existence oracle** — "absent" and "another user's" both yield zero
      members (#94 Decision 5's one-404 rule). The `admin_ro`-on-another-user's-rows
      test asserts at the **DB level** (their row unchanged), not on HTTP status:
      with coordinates there is no id to 404 on, so a status-only assertion is
      vacuous.
- [x] **M3 — Judge page + nav (web)** — DONE `c629ce28`; reviewed + audited. Web gates green
      (`typecheck` + 837 vitest). **One interpretation to confirm:** an anchored
      deep-link (`/judge?run={id}`) with no explicit `?bucket=` defaults to the `all`
      bucket, not `todo`, so a notification reliably shows that run's recs even when its
      coordinates rolled up settled elsewhere (dedup); un-anchored still defaults `todo`.
      The AL comment-precision fix in `judge_bulk_disposition.go` is folded into this
      commit.: route `/judge` (with the `?run=` filter for
      the notification deep-link); `<NavItem>` in the Factory group with the
      `triage.todo` badge poll (Decisions 8/9); bucket tabs from `triage`; the
      deduped worklist (group rows + "seen in N runs" + occurrence expander +
      per-group overflow **Mark done / Dismiss ▾**, and **per-recommendation File
      issue via the existing #68 browser draft** from the occurrence expander); the
      multi-select checkbox bar → bulk **disposition** M2 calls with an undo toast;
      the **inbox-zero** state (Decision 8). **Also removes the `TriageSummary` strip
      from `RunsList.tsx`** (its aggregate moves to this page's header — Decision 7).
      `mockApi.ts` + `data.ts` render every state. A revised in-repo mock
      `prds/mockups/98-judge-menu-mock.html` (by-target). Depends on M1 + M2.
- [x] **M4 — `/runs` per-row badge (web + api)** — DONE `1da5ac32`; review dispatched.
      **Three behaviours settled at implementation, recorded so M3 does not re-decide
      them differently:** (a) the `judge_todo_count` read is **best-effort** — a
      failure logs and leaves counts at 0 rather than 500-ing the whole run list,
      because a badge is decoration and an ornament must not cause an outage;
      (b) an **unjudged run renders no badge at all**, not a neutral pill — "never
      judged" and "judged and fine" are different claims and a placeholder asserts
      the second (the same reason the DTO field is nullable rather than defaulted);
      (c) the **verdict survives a cleared backlog** (`⚖ issues` with count 0),
      because the badge reports the judge's finding, not the triage state. All three
      are pinned by tests so the intent is visible to the next reader.
      **Trap for anyone adding a field here:** the Go and TS type structures are NOT
      symmetric — Go has separate `RunDTO`/`RunListItemDTO`, while TS has
      `RunListItem extends Run`. A field added to the TS `Run` therefore *inherits*
      into `RunListItem` instead of erroring, silently telling the client that
      `GET /runs/{id}` returns something the API only ever sends on the list. Caught
      here by `tsc` via the run-view fixtures, and only by that.
      **The verdict join is safe by INVARIANT, not by predicate** (M4 audit): `LEFT
      JOIN run_reviews rv ON rv.target_run_id = r.id` carries no user predicate, and
      is correct only because *two* things both hold — the outer query filters
      `r.user_id = @user_id`, **and** a review's `user_id` always equals its target
      run's owner (`PostReview` binds `UserID: target.UserID`). The count query, by
      contrast, is scoped in its own right (`WHERE rv.user_id = @user_id AND
      rv.target_run_id = ANY(@run_ids)`), so a spoofed run id yields nothing. The
      cheap hardening is `AND rv.user_id = r.user_id` on the join — free (the planner
      has both columns) and self-standing, exactly as `ee834583` made the `?run=`
      semi-join self-standing.
      **The count has TWO silent-understatement paths, both indistinguishable from a
      genuine 0:** a failed count read (best-effort → renders `⚖ issues` instead of
      `⚖ issues · 3`) and truncation, if the page cap and `JudgeRunTodoMaxRows` ever
      diverge. Acceptable for decoration — and in both cases the load-bearing half,
      the verdict, is unaffected because it rides the join rather than the count.: `judge_verdict` via the safe
      single `run_reviews` join + `judge_todo_count` **bucketed in Go** (never a SQL
      ladder; Decision 7); `RunListItem` type + the one-grammar per-row badge.
      Independent of the endpoints (own join) — starts immediately. (The strip
      *removal* is M3's, so the aggregate is never homeless.)
- [x] **M5 — Notification retarget + inbox grouping (web + api)** — DONE: `reviewDeepLink`
      → `/judge?run=` (one-liner); the inbox link retarget as a
      **`kind==='judge_review'` guard** (not a one-liner — `Notifications.tsx` linked
      `/runs/${run_id}` for any kind, Decision 4); web inbox grouping of
      consecutive judge rows (Decision 5). **No Slack digest** — Slack DMs keep their
      one-per-review cadence, only the link changes. **Depends on M3** (deep-links
      `/judge` + its `?run=` filter) — Phase 3, not Phase 1.
      **What landed, and the reasoning that is not visible from the diff:**
      - The link lives in ONE pure function, `notificationLink(kind, runID)` in
        `web/src/lib/notifications.ts`, so the guard is unit-testable away from a render.
        **The gate is the NON-judge case**, and the test enumerates five kinds rather than
        one: the judge row lands on `/judge` under both the correct guard and the
        unconditional URL change, so a test that checks only the judge row passes under
        exactly the bug the guard exists for. A single non-judge case would also be
        satisfied by a guard spelt `kind !== 'run_failed'`.
      - **The API side is a plain URL change and that asymmetry is deliberate**, recorded at
        `reviewDeepLink` itself: it is called from exactly one place, the judge review
        notification, so it is judge-only by construction. The web surface renders EVERY
        kind, which is what makes the same edit a guard there. `reviewDeepLink` had **no
        test at all** before this; it has one now, covering the anchor, the trailing-slash
        base and the empty base (no link rather than a broken one).
      - **The third `triage.todo` consumer reads the value, it does not poll for it.**
        `JudgeTodoContext` gained a READ side (`JudgeTodoValueContext` / `useJudgeTodo`).
        Polling `/me/judge/stats` from the inbox is a defensible reading of "read the
        canonical count" and it is wrong for the BLK-BADGE reason: a shared SOURCE without
        shared PROPAGATION is exactly the configuration in which the nav badge read 3 while
        the tab read 0. The value is `number | null`; `null` (no provider) renders **no
        number**, because a displayed 0 is the claim "nothing left to triage" and a
        provider-less component has not been told that.
      - **Grouping is a pure partition** (`groupNotifications`), asserted as such: every row
        appears exactly once, in order, so read-state, ids and offset paging are untouched.
        A run of ONE stays a plain row. An unparseable timestamp **breaks** the run — `NaN >
        window` is false, so the arithmetic alone would fold an unknown-age row in silently.
      - **Demo fixture: one non-judge row added to `data.ts`, placed BETWEEN two judge rows.**
        It renders all three inbox states at once (an ungrouped judge row with its `/judge`
        link, a non-judge row with its `/runs` link, and a grouped pair). Before it, demo
        mode showed only the group — and the grouping is precisely what would have hidden
        the retarget from anyone looking at the demo.
      - **`mockApi.notifications.test.ts` was rewritten to DERIVE from the fixture.** Adding
        that one demo row turned five of its six tests red, none for a reason about mockApi:
        they had snapshotted the fixture's ids and counts. The property is the mock's
        semantics (own-scoping, newest-first, offset paging, unread bookkeeping); the
        fixture's shape never was, and pinning it made `data.ts` unable to gain a row.
      - **Two existing tests changed meaning and were moved off `judge_review`**: the inbox's
        "renders a run deep link" case and its paging case. The first had become a test of
        the judge path wearing the generic path's name; the second was asserting on 30
        rows that now collapse into one header, so it was measuring the grouping instead of
        the paging.
      - **`navBadgeText()`'s selector had to be anchored** (`/^Judge/`): the group header
        carries its own "Open Judge" link, so the old substring match found two links and
        threw. Same shape as the repo's `role="status"` ambiguity trap, in a new place.
      **Negative controls, each RED then restored green:** (1) header renders `items.length`
      instead of the canonical count → 3 of the together-mount tests fail; (2) the
      notification holds a frozen copy (the own-poll shape) → the post-dispose agreement test
      fails while the other two pass, which is what proves that assertion is load-bearing
      rather than riding its neighbours; (3) `notificationLink` written as the unconditional
      `/judge?run=` URL change → the non-judge gate fails in both the lib and the page suite,
      and **no judge-row assertion moves**.
      **Also corrected in the same commit:** `specs/ai.md` said the recommendation free text
      "stays on the run page behind the deep link" — true until this milestone moved the
      link. And `judge_notify_test.go` asserted the old `/runs/` URL; it was found by the
      suite, not by a grep for `reviewDeepLink`, because it pins the literal string.
- [x] **M6 — Filed→Done sync (api/poller) — the migration** — DONE `d6a8545c`
      (migration `00075`, draft number — renumber on the landing rebase); review wave
      dispatched.: add `set_via` on
      `recommendation_dispositions` + `close_synced_at` on
      `recommendation_filed_issues` (draft number above the live head, renumber at
      merge); the post-sync **edge** pass (cached `state=closed` AND `close_synced_at
      IS NULL` → `INSERT … ON CONFLICT DO NOTHING` a `set_via='issue_close'`,
      `set_by_user_id=NULL` done → stamp `close_synced_at`), Decision 6. Test: a
      close drops the rec from To triage and (for `improve_uzi`) the self-improve
      backlog; **Undo sticks** (next tick does not re-apply); a coordinate the user
      **dismissed** is **not** overwritten; a reopen does not re-open. Builds on
      #68/#94 + the existing poll loop — parallel from the start.
- [x] **M7 — CLI (`api/cmd/uzi/review.go`)** — DONE `de2d8de3`; reviewed + audited: `uzi review backlog` (grouped,
      `--bucket`, `--json`) + `resolve`/`dismiss --category/--target` (Decision 10);
      **no `file` verb** (filing stays browser-only). Tests cover the grouped output
      and the bulk disposition fan-out. **Correction, measured 2026-07-21 while
      implementing M7: this milestone used to also ask for "a uza_ token refused on a
      bulk disposition mutation", and that test cannot honestly exist.** There is no
      refusal to assert on this route: it is owner-only BY CONSTRUCTION (the service
      resolves members under `user_id = caller`) and coordinates are not ids, so a uza_
      token aimed at another user's coordinate gets **200 `updated: 0`** — identical to a
      misspelt or already-settled coordinate, which is exactly Decision 5's
      no-existence-oracle rule working. The branch's own
      `judge_bulk_disposition_livedb_test.go` had already written this down: "the authz
      case the PRD calls for has no id to 404 on … a status-only assertion is therefore
      VACUOUS." A CLI test scripting a 404 here would have gone green only because the
      fake returned an error the real server never sends. What the CLI *can* get wrong is
      presenting that silence as a completed action, so
      `TestReviewGroupZeroUpdatedIsNotReportedAsSuccess` pins that instead — and the
      genuine 404 refusal stays covered where it genuinely exists, on the per-run route
      (`TestReviewMutationRefusedForReadOnlyToken`). Depends on M1 + M2.
## What this PRD learned about pinning SQL, and the evidence for it (2026-07-21)

Recorded here rather than in `.claude/agent-team-tasks/` because **that directory is
gitignored** (`.gitignore:27`) — rules written there die with the worktree, which is the
session's own thesis landing on the session. The short, findable form of the two most
reusable facts is in `CLAUDE.md`'s api section; this is the rationale and the evidence.
Agent-process rules (citing across a moving tree, instructions expiring) are in
`.claude/agent-team.md`.

**THE RULE, stated so it names a query SHAPE rather than a file list:** *any LEFT JOIN onto
a coordinate-keyed side table needs a fixture where two rows in one review share EXACTLY ONE
half of the coordinate.* A file list catches two of the four sites below; the shape catches
all four. The failure it prevents is specific and it recurred four times: a fixture whose
rows differ in *both* halves at once makes every half individually inert, so the weaker
mutation passes while only the both-halves mutation fails — and a passing weaker mutation
reads as coverage.

| site | state before this PRD's sweep |
|---|---|
| `ListJudgeRecommendationRowsForUser` (backlog) | coordinate halves pinned at `45381961`; **review halves were not** |
| `ListJudgeTriageRowsForRuns` (`/runs` badge) | **nothing pinned at all** until `2e941ced` |
| `ListOwnedRecommendationsForCoords` (bulk resolve) | review half pinned; coordinate halves inert; the `f` join never exercised |
| `ListJudgeTriageRowsForUser` (#94 stats) | every half individually inert; only the both-halves fold caught anything |

**All four are now pinned**, each half reddening its own named assertion, every fold run on a
fresh database with a positive control asserted. The `/runs` badge query and the #94 stats
query needed new tests; the backlog query needed new fixture rows (a cross-review coordinate,
a cross-category one, a claimed-but-not-filed one, a second tenant, and a run the caller owns
but does not request).

**THE POSITIVE CONTROL HAS THREE CLAUSES BECAUSE THREE DIFFERENT MECHANISMS PRODUCE "no
failures observed" WITH NOTHING EXECUTED**, and no single weaker check catches all three:

| mechanism | what it looks like | what misses it |
|---|---|---|
| the suite skipped (no DSN) | `rc=0`, both packages `ok`, `RUN=108 PASS=0 SKIP=108` | exit code, "no FAIL lines" |
| the mutation silently did not apply | a real, fully green run of **unmutated** code | everything except diffing the file |
| the run never happened | `run-store-it.sh` exits 1, log holds only "postgres never became ready", `PASS=0 SKIP=0 FAIL=0` | "no FAIL lines" |

The third was observed twice on 2026-07-21 by two agents on two different commands.

**THE POSITIVE CONTROL CATCHES TWO OF THESE THREE, NOT ALL THREE, AND THE DISTINCTION IS
LOAD-BEARING.** `PASS > 0` **and** `SKIP == 0` **and** the named test appearing as
`--- PASS`/`--- FAIL` catches the skipped suite and the run that never happened — which is why
the rule is not "check the exit code". It **cannot** catch the silently-unapplied mutation, and
no property of the *run* can: the suite genuinely runs, every assertion genuinely executes, the
control passes cleanly, and the result is green because the code under test was never mutated.
Only comparing the TREE sees that one, which is why `.claude/agent-team.md`'s "assert the
mutation actually applied — not just that the test ran" is a **separate** rule and must stay
one. A reader who believes the control covers all three will drop the tree comparison as
duplicated effort, and that is the one of the three that has already produced a false green on
this branch. (Corrected 2026-07-21: the lead originally wrote "one check catches all three",
which the table above already contradicted.)

Note also that the control was written before the third mechanism was seen and caught it
anyway: that is the argument for a mechanism over an enumeration of known failure modes.

**The tenant boundary, and the precise claim.** Neither `recommendation_filed_issues`
(00071) nor `recommendation_dispositions` (00073) has an owner column. `filed_by_user_id`
and `set_by_user_id` are `ON DELETE SET NULL` **attribution** pointers — nullable, and NULL
by design for every M6 auto-done — not ownership. Ownership reaches both tables *only*
through `review_id → run_reviews.user_id`, and `WHERE rv.user_id = @user_id` scopes `rv`,
not the joined side table. **The production code is correct**; what was missing was any test
that could observe a break in it. Do not summarise this as a shipped leak.
Corollary worth keeping: the natural "hardening", `AND d.set_by_user_id = @user_id`, would
**silently drop every auto-done**.

**A PRD that makes another PRD's query load-bearing inherits its coverage.** Decision 1 calls
#94's stats query directly so the nav badge, the page tab and M5's notification are literally
the same query rather than equal-by-construction. That choice buys the guarantee *and* the
risk: a broken coordinate half there makes all three consumers read the same wrong number and
agree perfectly, so the cross-check the design relies on cannot fire. The decision that buys
the guarantee inherits the obligation to cover it.

**Two mechanisms that turned out to matter more than any individual finding.**
- **A positive control on every mutation run** — assert the named test appears as
  `--- PASS`/`--- FAIL` and that `SKIP` is 0. Three agents independently leaned on weaker
  evidence (exit code, "no failures printed", a contention argument) before this was
  measured; see `CLAUDE.md` for the measurement. It caught a real dead fold in this sweep,
  *and* it caught its own regex being wrong first.
- **Compile the mutation before believing it.** Four separately-prescribed folds this session
  did not build, one of them prescribed inside the correction of another.

**A fold must be SELECTIVE, not merely discriminating.** A fold that reddens a spread of
assertions manufactures several confidently-wrong diagnoses along with the true one. Measured:
`f.filed_at -> now()::timestamptz` reddened every assertion ORing `FiledAt.Valid` in with
other fields, several blaming join predicates that were never mutated;
`f.filed_at -> d.set_at` reddened exactly one.

**Cite the assertion, not the line — and not the tally either.** Line numbers drifted three
times in one session from comment edits alone; an assertion *count* drifted the same way
(one agent said three, another five, both right for their own tree).

## Remaining work — OPEN IN THIS PRD after the first MR (2026-07-21)

Five items. All found by execution, all with evidence recorded here or in the M3 checkpoint.
**These are PRD #98 work, not a follow-up PRD** — resume here.

- [ ] **An unscoped assertion in an M6 test — a landmine with a measured detonation.**
      `ListOpenImproveUziRecommendations` (selfimprove.sql) selects
      `WHERE rr.category = 'improve_uzi'` across the **whole table** — no user scope, no
      review scope — and `TestFiledIssueCloseAutoDonesOnceLiveDB`
      (`judge_issue_close_livedb_test.go`) iterates that result **filtering only by target**.
      So any future fixture, in any package, that seeds an *open* `improve_uzi` row on target
      `rg` fails that test for reasons entirely unrelated to it. **This already cost time on
      2026-07-21**: it failed the first baseline run of the M4 badge fixture, whose only sin
      was using `improve_uzi` as its second category. That is the branch's own
      "scope live-DB assertions to the fixture, never the whole table" rule, violated inside a
      reviewed-and-audited test. **The fix is to assert on the recommendation's ID**, since
      the returned row carries no `review_id` to scope by. Not fixed at the time because scope
      was frozen and the test was a validated artifact; the M4 fixture was moved to an inert
      category instead, with the reason recorded at the constant. Related, unfixed, same
      shape: `crossCat` in `TestJudgeBacklogProjectsEveryColumnLiveDB` also seeds an open
      `improve_uzi` row and avoids the collision **only** because its target is `rg-auto`.

- [ ] **M8b — the e2e leg.** Unblocked since PRD #97 merged and this branch merged `main`
      (`create_run`, `retry_read`, positive controls are all in the tree). Its value is
      **entirely in assertions fakes structurally cannot make** — a happy-path walkthrough
      would duplicate coverage that already exists three times over. It is the natural home
      for the two mechanisms below.
- [ ] **The printed-instruction backstop.** Three instructions existed in the CLI, **none had
      ever been executed, and two were false** (the revert hint, fixed; the truncation
      remedy, fixed). A string that tells a user what to do is an assertion nothing
      typechecks. The mechanism: a table of `arrange → produce → extract → assert` rows that
      **executes the printed text verbatim** (never a hand-written copy — both false
      instructions parsed perfectly), asserts an **outcome** rather than an exit code, and
      **asserts its own precondition** or is vacuous. **The piece that makes it a class
      mechanism and not three patches**: a backstop scanning `api/cmd/uzi/` for printed
      backticked `uzi …` commands that **fails if any has no row in the table** — so the
      *fourth* instruction, the one nobody has written yet, fails the build until someone
      runs it. That half needs **no stack** (a grep and a set difference) and can land
      independently. Constraint: each row must bind to the command that **emits** it —
      running an instruction against the wrong command manufactures a false finding exactly
      as reading it manufactures a false pass.
- [ ] **Seam 6 — mock↔server fidelity. MEASURED 2026-07-21: no divergence found, but the
      demo fixture cannot reach the two riskiest behaviours.** A differential harness dumped
      the shipped `mockReviews`, ran the real `GroupJudgeRecommendations` over rows built in
      `rv.updated_at DESC` order, and structurally diffed against `mockApi.getJudgeBacklog`:
      **7 groups, 0 field diffs, identical ordering.** Detection power proven (swapping the JS
      `BUCKET_RANK` produced 4 immediate divergences), and **sort stability is genuinely
      exercised** — the fixture contains a four-way tie at `(run_count=1, open_count=0)` and
      both sides order it identically, so `sort.SliceStable` vs JS `.sort()` is covered by
      data rather than by reading.
      **The gap: the demo fixture contains ZERO instances of `occurrences > run_count`** (the
      same coordinate twice in one review — the Go grouper's own comment calls it out, and it
      is the shape that crashed the endpoint with SQLSTATE 21000) **and ZERO fully-settled
      groups with disagreeing members** — so `topRung` never has to *choose*, and the
      `dismissed > done > filed` precedence ladder, the single most-duplicated logic across
      the two implementations, is **never exercised**. Extending the fixture with both showed
      the implementations agree (9 groups, 0 diffs), so this is a **coverage gap, not a
      defect** — but the fixture *is* the demo, so the blind spot is shared by the demo and by
      every mock-backed vitest.
      **Second finding: truncation is unreachable in demo mode.** `mockApi.ts:381` and `:1812`
      hardcode `truncated: false`, so the banner cannot render. M3 requires `mockApi.ts` +
      `data.ts` to "render every state"; truncation is a state, it is subtle, and seam 5
      showed its CLI remedy was outright false — making it the state you would most want
      demoable.
      **Boundary of what was measured, stated not implied:** the harness compares the Go
      **grouper**, a pure function over rows. The `?run=` anchor and the row cap live in
      **SQL** and were *not* executed — they read as equivalent (SQL's coordinate-level
      `EXISTS` filtering rows pre-grouping vs the mock's `occurrences.some(...)` filtering
      groups post-grouping, both retaining other-run occurrences) but that comparison needs a
      live DB and belongs in M8b.
      **Constraint on the golden-fixture mechanism, demonstrated by this run:** the fixture
      must be **authored to discriminate, not snapshotted from the demo** — a golden file
      derived from `mockReviews` would lock in exactly the blind spot above, agree on
      everything it covers, and *read as full coverage*. One case per reimplemented behaviour
      (dedup; occurrences>runs; partial settle; each rollup precedence pair; anchor;
      scope=open; truncation) **plus an assertion that the fixture actually exercises each**,
      in the shape of the Go grouper test's own "fixture broken … otherwise this test proves
      nothing" guard. Without that self-check the golden file rots into a snapshot the moment
      someone regenerates it.
      *Original framing:* `mockApi` is no longer a stub: it reimplements
      dedup, the rollup ranks, `run_count`, the `?run=` semi-join, the `scope=open` fan-out,
      `updated` triples, truncation and `set_via` provenance. Its agreement with Go was
      verified **only by reading** — the mode that failed repeatedly on this branch. If it
      drifts, **the demo lies AND ~860 mock-backed vitests pass while asserting a fiction**:
      two failures, one cause, nothing announces either. Settling execution: a golden fixture
      — one input through Go's `GroupJudgeRecommendations` and through `mockApi`, asserting
      identical output. Attack the cases the mock had to *reimplement* (cross-run recurrence,
      a partially-settled group, the `dismissed > done > filed > todo` rollup, the anchor
      keeping other-run occurrences, truncation cutting **before** grouping), and check
      ordering explicitly — Go uses `sort.SliceStable(run_count DESC, open_count DESC)` and a
      JS `.sort()` is not stable the same way for equal keys.
- [ ] **The filed-issues join's coordinate half is asserted but NOT exercised** (measured
      2026-07-21). Dropping `AND f.target = rr.target` from the `LEFT JOIN` leaves
      `TestJudgeBacklogProjectsEveryColumnLiveDB` **green**, because the fixture's `autoRev`
      and `handRev` are **different reviews** — so `f.review_id = rv.id` alone separates the
      filed row from the unfiled one and the coordinate half never carries weight. **This
      covers four columns**: `filed_settled`, `filed_issue_iid`, `filed_issue_url`,
      `filed_at`. Drift would leak a filed link onto **sibling coordinates of the same
      review** — a never-filed coordinate rendering "Filed #4242", and `filed_settled`
      flipping it to the `filed` rung so the ladder hides it from To triage. Silent and
      user-visible. **One-line fix: add an unfiled coordinate INSIDE `autoRev`**, so dropping
      the predicate cross-matches and the assertion becomes load-bearing. (Caveat from the
      measurement: only the *minimal type-preserving* fold is a valid test here — two earlier
      attempts changed the generated Go type or perturbed an unrelated test, so a green from
      a non-minimal fold proves nothing.)
      **Honest count for the MR: 15 of 16 projections pinned with VERIFIED isolation, plus
      one unpinned JOIN predicate that is not a projection at all.** The `filed_at`
      *projection* IS isolated (folding `f.filed_at → now()` reddens via the unfiled-row
      absence check); what is unpinned is the filed-issues join's **coordinate predicate**,
      which affects the row-scoping of the four columns riding it. The one unisolated
      projection is `rationale_md`, and **nothing in the live-DB suite catches its fold** —
      no incidental coverage. **Both remaining gaps share one root cause: every fixture row
      carries identical values.** `bulkFixture` hardcodes `'because'` as the rationale on
      every row, so folding `rr.rationale_md → 'because'::text` collapses the stored hash and
      the read-back value to the same thing and the test **goes green** (measured). And
      `autoRev`/`handRev` put the filed and unfiled coordinates in *different reviews*, so
      `f.review_id = rv.id` alone separates them and the join's coordinate half never works.
      **One fixture principle fixes both — distinct values per fixture row**, which is the
      `memberRowIn` lesson from `136acb53` never applied to the rationale text or the
      filed/unfiled split. The fix is in the fixture, not the assertions.
      **STATUS CORRECTION, 2026-07-21 (later the same day): the `rationale_md` half of the
      paragraph above is STALE and is left visible rather than deleted, because the staleness
      is itself the lesson.** `bulkFixture` no longer hardcodes `'because'` — it writes
      `"rationale for %s/%s in run %d of %s"`, distinct per row, and the fixture's own comment
      records the measurement (GREEN under `rr.rationale_md → 'because'::text` before the
      change, RED after, on a fresh database, at both the per-coordinate hash assertion and
      the two-hashes-must-differ one). So "15 of 16 with one unisolated projection" understates
      the tree it is now read against. **What remains open in this item is only the JOIN
      predicate**, and only on the M1 read query (`judge_recommendations.sql`) — see the
      distinction recorded immediately below.
      **The BULK query carries its OWN copy of that join, and it is now PINNED (2026-07-21).**
      `judge_bulk_disposition.sql`'s `ListOwnedRecommendationsForCoords` has a second,
      independent `LEFT JOIN recommendation_filed_issues` — a different query body, so the M1
      fix above would not have covered it. It was worse than unexercised: measured at
      `a2b554a6`, `grep -c "recommendation_filed_issues\|filed_settled"` over
      `api/internal/handler/judge_bulk_disposition_livedb_test.go` returned **0**. No live bulk
      fixture had ever inserted a filed row, so `filed_settled` was FALSE on every row of every
      live exercise and Decision 2's "a FILED member is not open" rung was pinned only by a
      fake — which takes the boolean as a **parameter** and therefore cannot be wrong about
      where the boolean comes from.
      `TestBulkDispositionFiledMemberIsNotOpenLiveDB` closes it with one review holding TWO
      coordinates and a filed row on exactly ONE, which is what makes it discriminating in
      both directions. Measured, each fold on a fresh throwaway Postgres with the positive
      control passing (control RUN/PASS lines = 2, `SKIP == 0`) and the restore verified by
      `sqlc generate` giving a zero diff:
      **baseline GREEN** (129 PASS / 0 FAIL / 0 SKIP);
      **`ON f.review_id = rv.id AND f.category = rr.category AND f.target = rr.target` →
      `ON f.review_id = rv.id`** ⇒ **RED**, `updated = 0` (the one filed row cross-matches its
      sibling coordinate, so BOTH members bucket `filed` and neither is open);
      **`(f.filed_at IS NOT NULL)::bool` → `false::bool`** ⇒ **RED**, `updated = 2` (nothing
      reads as filed, so both members are open). Each fold reddened **exactly this one test**.
      A one-coordinate fixture would have caught only the second. Both folds were compiled
      (`sqlc generate` + `go vet`) before being believed; `false::bool` is type-preserving here
      because the projection is already NOT NULL via the cast — the nullability trap in
      `CLAUDE.md` applies to folding a nullable LEFT-JOIN column, which this is not.
- [ ] **`ListRunInputsForRun` has NO live exercise — found by the query inventory, 2026-07-21.**
      `judge.sql`'s `ListRunInputsForRun` is called from exactly one place in production
      (`workersvc/judge_trace.go:89`) and from **no test that touches a database**. Every test
      reaching `JudgeTrace` runs against workersvc's `fakeStore`
      (`workersvc/service_test.go:393`), which returns a canned slice, so the SQL text has
      never executed under test. The judge's oldest-first input cap rides this query
      (`follow_up_inputs_integration_test.go:21` describes the shape). Recorded rather than
      fixed because scope is frozen; it is declared `UNPINNED` with this reason in
      `api/internal/store/judge_query_inventory_test.go`, which is the only reason it is
      visible at all.

- [ ] **Widen the query inventory beyond the judge family.** The declaration test landed for
      the five judge-family `.sql` files (**17 queries**). Repo-wide is **276 queries across
      28 files**, and a repo-wide table written in one sitting would be mostly `UNPINNED` rows
      authored by someone who had not investigated any of them — which is a worse artifact
      than none, because it reads as an audit. Widen one file at a time, with the call sites
      opened.
      **What the mechanism is and is not, so a later reader does not over-credit it:** it
      proves only that someone has DECLARED where a query is pinned, **not that the pin is
      good**. Nothing in it executes a query or folds a predicate; a row naming a test that
      merely touches the query is exactly as green as a row naming a test that reddens under
      mutation. What it does catch is the case that currently produces silence — a query
      **arriving or being renamed with nobody having thought about coverage** — and it fails
      the build until a human writes a row.
      **`UNPINNED` is a legal, green, permanent state, and that is a design constraint rather
      than a concession:** a mechanism that fails the build for an honestly-declared gap gets
      deleted the first time someone is in a hurry, taking the arriving-query check with it.
      **Declared, not inferred, and the inference was measured wrong in BOTH directions on
      these same 17 queries** (the auditor's prototype, `Test*LiveDB` body scan): (a) the two
      `judge_bulk_disposition.sql` queries appear in **no test source at all** — reached only
      through workersvc from a handler test — which is the mechanism behind the 48 repo-wide
      queries it classified as "named in tests but no LiveDB caller"; (b) **a second, distinct
      false-negative mechanism found while writing the table**: `ListDispositionsForReview` is
      called at `recommendation_dispositions_integration_test.go:262`, inside the
      package-level helper `listDispositions` (`:260`) that the test calls at `:225` — in the
      file, but in no test function's body, so a body scan misses it and a whole-file scan
      would instead credit every test in the file; (c) false positive: `CreateJudgeRun`'s
      first inferred pinner is `TestClaimRunDockerRepoAllowlistLiveDB`, which uses it as
      fixture setup for an unrelated property.
      **Negative controls run at the tip, all four RED then restored green:** an undeclared
      new query in `judge.sql`; a pin renamed to a test that does not exist; a row renamed so
      it names no query (fires BOTH the missing and the stale check); an `UNPINNED` row whose
      reason is blank. Plus two self-checks against a vacuous green — a per-file "0 query
      names parsed" abort and a whole-scan "0 queries total" abort — because the two ways this
      test passes for any tree are "the glob found nothing" and "the regex matched nothing",
      and each check catches only its own.

- [ ] **N2 — `OccurrenceFileIssue` tests.** 236 lines, **zero tests**, and M3's **only
      forge-writing web path**; no test ever opens the occurrence expander. Its security
      controls were verified by line-by-line diff against RunView's filer (same CSRF path,
      `forgeLimiter`, draft gate, provenance box, `isHttpsUrl`) — but that duplication also
      duplicated away its coverage. A test on the duplicate is wanted, **not** a refactor.
      Also still open: its stale-filed-link warning is absent because `JudgeOccurrenceDTO`
      carries no review `updated_at`; the comment now says so plainly rather than implying
      there is nothing to guard.

**Known limitation, accepted deliberately (2026-07-21):** the `⚖ issues` badge is
**byte-identical on `/runs` and on Judge occurrence rows**, but means different things. On
`/runs` the count is always rendered when > 0, so a bare `⚖ issues` there means *"nothing
left to triage"* (M4 behaviour (c)); on the Judge page an occurrence carries no count at all
— and those rows are by construction still open. **Only the tooltip distinguishes them**; a
user who does not hover reads the `/runs` meaning. Splitting the *label* was rejected because
it reintroduces the two-grammar problem the fable review removed and N8 closed, so the title
carries the distinction alone. Revisit only with a design that keeps one visual grammar.

**Also unresolved, lower priority:** the live-DB harness's intermittent *"postgres never became
ready"* — **mechanism unknown**, with two confident explanations already disproved (a "fixed
container name" that does not exist, and load contention measured at 3-6× headroom for one
concurrent suite). Recorded in the checkpoint as unknown. Sequencing is the mitigation and it
costs nothing; do not attribute it.

- [ ] **M8a — Tests + Docs (docs half in the first MR)**: e2e leg (dedup grouping; a group **Dismiss** fans out
      across runs and drops an `improve_uzi` rec from the backlog; **issue-close →
      Done, edge-once, Undo sticks, dismissed not overwritten**; the notification
      deep-links to `/judge?run=`; **no token spend** on any disposition); vitest for
      the page/tabs/zero-state + the `/runs` badge; `docs/judge.md` (the menu, dedup
      grain, group disposition fan-out, inbox grouping, filed-sync incl. its
      PRD-label/enabled-repo preconditions) + `docs/cli.md` (`review backlog` +
      disposition verbs); `specs/ai.md` records the decisions.

**Follow-up (not in this PRD): bulk file-as-one-issue.** A `POST
.../recommendations/file` that files one aggregated GitLab issue linking N members
across a group (or several groups) needs its own decisions — per-item `repo_id`
selection (+ caller-owns-repo), an aggregated **human draft** through #68's
sanitizer, `forgeLimiter`, and a `RequireAuth`→`RequireUser` posture change (or a
deliberate browser-only stance). Tracked separately; v1 files per-recommendation
via the existing #68 flow.

**Dependency graph** (house convention):

| Phase | Milestones | Depends on | Touches |
|---|---|---|---|
| 1 (parallel) | **M1** grouped read · **M4** /runs badge · **M6** filed→Done (migration) | existing #46/#68/#94 | new judge-recs query/handler · `runs.go`+`RunListItem` · migration + `poller`/`forgesvc` |
| 2 | **M2** bulk disposition | M1 | new `handler/judge_bulk.go` + `handler.go` route |
| 3 (parallel) | **M3** Judge page+nav (+ strip removal) · **M5** notif retarget+inbox grouping · **M7** CLI | M1 + M2 (M5 needs M3's route) | `web/` · `judge_worker`+`Notifications.tsx` · `api/cmd/uzi/` |
| 4 | **M8** tests + docs | all | e2e/vitest/docs |

M4 and M6 are independent of the API core (own join / own poller pass) and run in
parallel with M1 from day one; M2 gates the mutating consumers. **M5 moved to
Phase 3** — it deep-links `/judge` and needs the `?run=` filter, both of which land
in M3; shipping it in Phase 1 would point every judge notification at a 404. The
`TriageSummary` **strip removal is folded into M3** (its aggregate's new home is the
Judge page header). Single repo, so no cross-repo phase.

## Success Criteria

- From the **Judge** menu a user sees every open recommendation **deduped by
  `(category, target)` across all their runs**, each with a "seen in N runs"
  count and an expander to the per-run occurrences.
- **One group *disposition* action settles every occurrence**: a group **Dismiss**
  / **Mark done** dispositions all open (`bucket==todo`) members across runs, in
  one call; **filing** is per-recommendation via the existing #68 browser draft
  (bulk file-as-one-issue is a follow-up).
- The **nav badge, the notification, and the page's To-triage tab show the same
  number** (`triage.todo` via the shared `BucketOf`); "seen in N runs" never
  appears as a competing count.
- The **judge notification is unchanged as an event** but deep-links to
  `/judge?run={id}` (web and Slack); the in-app inbox groups consecutive judge
  rows (Slack DMs keep their one-per-review cadence — no Slack digest).
- The `/runs` list shows a **one-grammar per-row judge badge** (`⚖ verdict · N`)
  and **no longer** carries the global strip.
- A **filed issue closing auto-moves its recommendation to Done exactly once**
  (edge-triggered), dropping it from To triage and, for `improve_uzi`, the
  self-improve backlog; a human **Undo sticks** (the next poll tick does not
  re-apply); a member the user **dismissed** is never overwritten; a reopen does
  not re-open.
- `uzi review backlog` + the disposition verbs drive the **same state** as the web
  from a uzc_ token; a uza_ read-only token can `backlog` but is refused (404) on a
  bulk disposition mutation.
- **No Anthropic token is spent** by any disposition/backlog action (proven by
  the M8 e2e leg).

## Risks

- **Payload growth on an all-time backlog.** Owner-scoped and ≤50 recs/review,
  but many runs → many groups. **Corrected 2026-07-20 (audit of M1 at `0874d3f6`):
  this originally read "bounded by default", and that was false.** The default
  `?bucket=todo` filter bounds only the **response body** — the query is always the
  caller's entire all-time row set, every row is materialized into occurrence DTOs,
  and grouping runs over all of them. That is not an oversight to fix by narrowing
  the query: `triage` is *deliberately* computed over the unfiltered set so it
  equals `/me/judge/stats` whatever the filter (Decision 1), and that design is
  right. So the real bound is an explicit **hard `LIMIT` plus a `truncated` flag**
  in the response, not the filter. Own-data amplification only (no cross-tenant
  exposure), on a route group with no rate limiter. Pagination remains the fast
  follow if a heavy user's To-triage exceeds one screenful of groups.
  **What `JudgeBacklogMaxRows = 2000` does and does not bound** (audit of
  `d701a388`, 2026-07-20 — state this precisely, because the loose version is
  already circulating): it bounds rows on the wire, the Go materialization, the
  grouping pass, and the response. It does **not** bound Postgres's own work. The
  `ORDER BY` spans two tables (`rv.created_at, rv.id, rr.created_at, rr.id`), so no
  single index supplies that ordering and the server must produce the caller's full
  join result and top-N sort it before `LIMIT` applies. `?bucket=all` on a heavy
  account still walks all of *that caller's* rows server-side. **`EXPLAIN (ANALYZE,
  BUFFERS)` was subsequently run against a seeded multi-tenant fixture (120 users,
  7,278 runs/reviews, 145,222 recommendations, caller owns 1,200) and one clause of
  this paragraph came back FALSE — corrected here rather than left standing.** The
  top-N-sort-above-the-join claim is confirmed (`Limit → Sort → Hash Left Join`,
  the sort sits above the entire join), and `run_reviews` is index-bounded as
  claimed (`Bitmap Index Scan on idx_run_reviews_user` → 60 rows). But **"never a
  full-table scan" is wrong**: the plan carries `Seq Scan on review_recommendations
  (rows=145222)` and `Seq Scan on runs (rows=7278)` — both read in full to return
  1,200 rows. `idx_review_recommendations_review` exists and the planner does not
  choose it, preferring hash joins, and the plan shape is identical at 24k and at
  145k rows, so this is not a small-data artifact. **The read therefore scales with
  the TOTAL size of `review_recommendations`/`runs` across all tenants, not with the
  caller's backlog.** This is inherited from #94's join spine, not introduced by
  this PRD — the companion `GET /me/judge/stats` shows the identical seq scan — but
  #98 makes the judge page run **two** such scans per request, and the nav badge
  polls one of them. Indexing it is a **#94-scoped** change needing its own
  measurement, deliberately not attempted here. So
  the claim "an anchored pull reads only the rows it returns" is true of the API and
  **false of the database** — the `EXISTS` is still evaluated per candidate row.
  Do not promote that sentence into `docs/judge.md`.
  Relatedly the request is **half-capped**: the wide read stops at 2000 rows, but
  the `triage` stats query has no `LIMIT` (one row per recommendation, all-time).
  Acceptable and non-blocking — it is 3 narrow columns rather than 15 wide ones
  carrying up to 4 KiB of `rationale_md` each, and it is the identical query
  `/me/judge/stats` already serves unbounded on the same `RequireUser` mount, so it
  adds no exposure that is not already shipped.
- **Truncation understates SURVIVING groups, not merely missing ones — M3 and M7
  must not render a cut page as authoritative** (found in the `d701a388` review,
  2026-07-20). The `LIMIT` cuts **rows before grouping**, and the cut is by review
  recency, so a group that survives the cut can still lose its older occurrences.
  That understates its `run_count` and `open_count` and **can change its rollup
  bucket**: a group whose only open occurrence sat among the cut rows rolls up
  `done`/`dismissed` instead of `todo` and is then filtered *out* of the default
  `?bucket=todo` view. `truncated: true` honestly flags the page as partial and at
  2000 rows this is a heavy-user edge case, so the behavior is acceptable — but the
  consumer contract is "when `truncated` is true, surviving groups' counts and
  rollup may be understated", NOT "only the oldest occurrences are missing".
- **Cross-user duplication for factory-wide categories.** `install_worker_tool`
  and `improve_uzi` affect everyone, but reviews are **owner-scoped** by #46
  design, so two users can independently see and file the same recommendation →
  duplicate GitLab issues. The menu stays owner-scoped in v1; cross-user
  visibility ("also recommended for 2 other users") or admin-scoped routing for
  factory-wide categories is **Future Work**. Called out so it is a known
  limitation, not a surprise.
- **Per-coordinate divergence inside a group.** Members can hold different
  dispositions; the group rollup is display-only and group actions default to
  `scope=open`, so nothing silently overwrites a member's prior decision. The
  occurrence expander is the per-run source of truth. Documented.
- **Filed→Done provenance + observability.** An auto-done (`set_via='issue_close'`,
  `set_by_user_id=NULL`) must be visibly distinct from a hand-marked done; the
  column + "done via #IID" label carry it. And because the sync reads the
  **PRD-labeled, enabled-repo** issue cache, a filed issue that loses its label or
  whose repo is disabled won't auto-Done — a documented silent no-op, not a bug
  (Decision 6).
- **Filed→Done correctness hinges on the edge marker.** Without `close_synced_at`
  the level-triggered pass would re-apply after Undo and could overwrite a
  `dismissed` verdict (fable's finding). The marker + `ON CONFLICT DO NOTHING`
  make it fire once and never clobber a human verdict — but this is the subtle
  part of M6 and must be the test that gates it.

## Resolved / Open Questions

- **Name is Judge** (not Reviews) — matches `RunKindJudge`, `JudgePanel`, the
  `uzi review` CLI, and the Settings opt-in; "Reviews" collides with
  MR/code-review. [user-decided 2026-07-20]
- **Worklist is by-target + dedup**, run as evidence — not by-run. Frequency, not
  self-reported confidence, ranks the backlog. A by-run *toggle* is **Future
  Work**, not v1. [user-decided 2026-07-20]
- **The triage state machine is already closed by PRD #94** — Done, and Dismiss ▾
  (Won't-do / Not-an-issue = false positive). The menu reuses those verbs via
  bulk fan-out; it introduces **no new disposition states**. (The concept mock's
  flat three-button row under-represented what already ships.)
- **The notification is kept, not removed** — the original "move notifications
  into the menu" idea is resolved as *retarget the ping's deep-link*, keeping the
  generic inbox and the "review landed" signal. [discussion-decided]
- **v1 scope includes** bulk multi-select **disposition** (dismiss / mark-done),
  **web-inbox grouping** of judge rows, and Filed→Done issue sync. **No Slack
  digest** — judge Slack DMs stay one-per-review. **Keyboard triage (j/k)** and a
  **target-file staleness marker** (distinct from #94's rationale-hash stale flag)
  are **Future Work**. [user-decided 2026-07-20]
- **RESOLVED — bulk "file N as one issue" is descoped to a follow-up.**
  [user-decided 2026-07-20] fable's PRD pass showed it is a mini-PRD, not a #68
  reuse (repo pick + aggregated human draft + `forgeLimiter` + a
  `RequireAuth`→`RequireUser` posture change). v1 ships bulk *disposition* +
  per-recommendation browser filing as written; bulk file-as-one-issue becomes a
  separate PRD. M2/M3/M7 therefore do **not** grow to carry those four concerns,
  and the CLI gains **no** `file` verb (#68's browser-only stance stands).
- **This PRD ships exactly one migration** (Decision 6: `set_via` +
  `close_synced_at`); everything else is a new query, new endpoints, and web. The
  read model (M1) is migration-free.
