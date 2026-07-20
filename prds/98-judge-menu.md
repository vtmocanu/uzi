# PRD #98: Judge menu — a dedicated cross-run recommendation workbench

**GitLab Issue**: [#98](https://gitlab.example.com/vtmocanu/uzi/-/issues/98)
**Status**: Draft (2026-07-20)
**Priority**: Medium
**Mockup**: static concept mock (ember shell + buckets + worklist + the three deltas) at the design artifact; **note** it renders the worklist grouped *by run* — a precursor. This PRD supersedes that with **group-by-target + dedup** (Decision 2); a revised in-repo mock lands with M3 as `prds/mockups/98-judge-menu-mock.html`.
**Depends on**: PRD #46 (the judge: `run_reviews` + `review_recommendations`, `users.judge_enabled`), PRD #68 (`recommendation_filed_issues`, the coordinate-keyed claim-first file flow), PRD #94 (`recommendation_dispositions`, the `bucketOf` ladder, `GET /me/judge/stats`, the global RunsList strip this promotes). Related: PRD #64 (the `uzi` CLI, second consumer), PRD #69 (the judge **control plane** — mode/model/spend/accuracy/consent; this PRD is the complementary output workbench, cleanly separable — see Decision 5's digest-scope note), PRD #47 (RunHealth badge — the per-row badge grammar this mirrors).
**Review**: fable adversarial pass on the concept mock folded in (2026-07-20). Load-bearing corrections adopted: **group by target with cross-run dedup**, not by run (the same recommendation recurs across runs and must be one triage decision, with frequency as the priority signal); **one canonical to-triage number** single-sourced from `triage.todo` so the nav badge, the notification, and the page tab cannot drift; the **/runs badge uses one verdict-first grammar** (`⚖ issues · 2`), not the mock's two grammars; the **triage state machine is already closed by #94** (the mock's flat three-button row under-represented it — Done / Dismiss ▾ Won't-do|Not-an-issue already exist). Deferred by review + user scoping: keyboard triage and target-file staleness → Future Work.

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
- **A group action fans out to its member coordinates — no new storage.** The
  group is a *display* construct over #94's per-coordinate rows. "Dismiss" /
  "Mark done" applies the disposition to every open member; "File issue" opens
  **one** GitLab issue aggregating the occurrences and links every open member to
  it. Bulk multi-select does the same across several groups.
- **The notification stays a ping — its deep-link just retargets here.** No
  event is removed from the inbox; the `judge_review` row and its Slack DM now
  deep-link to `/judge?run={id}` instead of `/runs/{id}`. Mirror, don't move.
- **The strip leaves `/runs`; each run row gains a verdict badge.** The global
  count strip becomes the Judge page's bucket header. In its place, every run row
  gets a one-grammar `⚖ verdict · N` badge — a per-run glance it never had.
- **Inbox grouping + Filed→Done sync close the loop at scale.** The in-app inbox
  groups consecutive judge rows (no Slack digest — Slack DMs stay one-per-review);
  and when a filed issue *closes*, its recommendation auto-moves to Done.
- **One canonical number.** The nav badge, the notification, and the page's "To
  triage" tab all read `triage.todo` from #94's shared `bucketOf` helper. "Seen
  in N runs" communicates the grouping without minting a second count.
- **CLI parity in the same MR** (`uzi review backlog` + bulk verbs), per the
  CLAUDE.md second-consumer rule.

## Design Decisions

1. **The Judge page is a new READ endpoint over #94's existing flat-join — no
   migration, no new storage.** `GET /api/me/judge/recommendations`
   (`RequireUser`, owner-scoped, all-time) runs the *same* join `GET
   /me/judge/stats` runs today (`handler/judge_stats.go`, `h.JudgeStats` at
   `handler.go:357`): `run_reviews` where `user_id = caller` → `review_recommendations`
   → LEFT JOIN `recommendation_filed_issues` and `recommendation_dispositions` on
   the `(review_id, category, target)` coordinate, one flat row per current
   recommendation. Where `/stats` buckets those rows into a `TriageDTO` and throws
   the rows away, this endpoint **returns them grouped by `(category, target)`**.
   The grouped DTO per group: `{category, target, occurrences: [{run_id,
   run_title, review_id, rec_id, verdict, confidence, bucket, filed_issue?}],
   open_count, run_count, rationale_preview}` where `rationale_preview` is the
   most-recent occurrence's escaped `rationale_md` (the panel's no-raw-render
   rule, `RunView.tsx`). Counts still come from the shared **`bucketOf`** (PRD #94
   Decision 2), so the page's tab totals and the nav badge equal the existing
   strip exactly. A `?bucket=todo|filed|done|dismissed|all` filter (default
   `todo`) bounds the initial pull.

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
   - A group's bucket **for the tab filter** is derived from its members, display
     only: it appears under **To triage** iff `open_count ≥ 1`; a fully-settled
     group rolls up to the highest state among members via the #94 ladder
     (`dismissed > done > filed > to-do`). The occurrence expander always shows
     the per-run truth, so a mixed group (2 dismissed, 2 open) is never
     misrepresented.

3. **A group action FANS OUT to member coordinates — reusing #68/#94 mutation
   semantics unchanged, owner-only.** Two new bulk endpoints, both `RequireUser`
   and owner-only by construction (every member is caller-owned — the join is
   `user_id = caller`, so this is PRD #94 Decision 5's strict-caller-ownership
   posture applied N times; a uza_ `admin_ro` token is refused a mutation on
   another user's rows exactly as in #94, and `IsAdmin` is never consulted):
   - `PUT /api/me/judge/recommendations/disposition`
     `{items: [{category, target}], status, reason?, scope: open|all}` — resolves
     the caller's member coordinates for each item and upserts a disposition on
     each (idempotent per #94 Decision 6; re-stamps the `rationale_hash` per its
     Decision 3). `scope=open` (default) skips already-settled members; `all`
     re-asserts. Returns the updated groups.
   - `POST /api/me/judge/recommendations/file`
     `{items: [{category, target}], single_issue: bool}` — files GitLab
     issue(s) via #68's claim-first flow and links **every open member** of each
     item. `single_issue=false` (default) = one issue per group; `single_issue=true`
     merges the selected groups into **one** issue whose body enumerates every
     occurrence (run links / issue IIDs) so the maintainer sees the frequency —
     this is the review's "file several recs into one issue". Each linked member
     gets a `recommendation_filed_issues` row on its coordinate (#68), so the
     per-run panels light up "Filed" consistently.
   - **Multi-select** across different groups (the checkbox bar) is just a
     multi-item call to these two endpoints — the UI batches selection, the API
     contract stays a list of `(category, target)` coordinates. A true
     single-round-trip multi-coordinate batch is exactly what these endpoints
     already are; no per-group round-trips.

4. **The notification is KEPT as a ping; only its deep-link retargets to the
   Judge menu.** The `judge_review` payload already anchors `run_id` + `review_id`
   (#46/#94) and the inbox is a generic surface (`notifysvc` untouched — "the
   judge is simply tenant #1"). Two one-line link changes, no payload change:
   - Slack DM: `reviewDeepLink` (`handler/judge_worker.go:318`, today
     `baseURL + "/runs/" + targetID`) → `baseURL + "/judge?run=" + targetID`.
   - Web inbox: the `judge_review` row deep-links to the in-app `/judge?run={id}`
     (its `run_id` anchor) instead of `/runs/{id}`.
   `/judge?run={id}` opens the menu filtered/scrolled to that run's occurrences,
   so the ping lands you *on the work*, not on a run page you then have to leave.
   The inbox row itself stays — the ping's job ("a review landed while you were
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

6. **Filed→Done sync — the poller closes the loop.** #68's
   `recommendation_filed_issues` links a coordinate to a forge issue
   (`filed_issue_iid`); the poller (`api/internal/poller` + `forgesvc`) already
   syncs issue state (forge = source of truth). Extend that sync: when a linked
   filed issue transitions to **closed**, upsert a `done` disposition (#94's
   idempotent upsert) on **every** coordinate linked to that issue, with a
   provenance marker (`set_via='issue_close'`, a nullable column on
   `recommendation_dispositions`, default `NULL`) so the UI can label "done via
   #IID" and distinguish it from a hand-marked done. The #94 ladder then buckets
   it as **done** — it leaves To triage and, for `improve_uzi`, the
   self-improvement backlog (#94 Decision 9). **No auto-reopen** on issue reopen
   (avoids flapping); a human Undo (#94 `DELETE .../disposition`) restores it.
   Owner is the review owner (filed issues are owner-scoped). Rides the existing
   poll loop; no new worker.

7. **Per-run verdict badge on `/runs` — a denormalized read on the list query, no
   new store; the strip moves out.** The runs-list query (`handler/runs.go`) and
   `RunListItem` (`web/src/lib/api.ts` — today it carries `mr_state`, `health`,
   … but **no** judge field) gain `judge_verdict` (`ideal|ok|issues|null`) and
   `judge_todo_count` via a LEFT JOIN `run_reviews` (+ bucketed recs) on
   `target_run_id = run.id`, owner-scoped like every other column. The row renders
   **one** compact badge, verdict-first with the count appended only when `> 0`
   (`⚖ issues · 2`, `⚖ ideal`) — a single grammar, fixing the mock's two-grammar
   bug, and mirroring the RunHealth badge (#47) placement. Click → the run's
   `JudgePanel` (unchanged). The global `TriageSummary` strip is **removed** from
   `RunsList.tsx` (PRD #94 Decision 8's header render + its `getJudgeStats` call);
   `GET /me/judge/stats` stays — the Judge page header and the nav badge now
   consume it.

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

10. **CLI parity — `uzi review backlog` + bulk verbs** (`api/cmd/uzi/review.go`,
    the #94 group). `uzi review backlog [--bucket todo|filed|done|dismissed|all]
    [--json]` prints the deduped groups (`category · target · seen in N runs ·
    open N`) from the M1 endpoint; `uzi review file --category C --target T
    [--single-issue]` and `uzi review resolve|dismiss --category C --target T
    [--reason wont-do|not-an-issue]` drive the M2 bulk endpoints (group fan-out).
    The existing per-run `uzi review show/resolve/dismiss/undo/stats` stay. The
    web-only surfaces (nav badge, inbox grouping, per-row `/runs` badge)
    have no CLI analogue and are called out as such.

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

- [ ] **M1 — Grouped read model (api)**: `GET /api/me/judge/recommendations`
      (`RequireUser`, owner-scoped, `?bucket=` filter) over #94's flat-join,
      returning groups keyed `(category, target)` with the occurrence list,
      `open_count`, `run_count`, and escaped `rationale_preview` (Decision 1). No
      migration. Store/handler test: dedup groups the same `(category, target)`
      across ≥2 runs into one group with a correct occurrence list; the bucketed
      totals equal `GET /me/judge/stats` for the same fixture (shared `bucketOf`).
- [ ] **M2 — Bulk mutation (api)**: `PUT .../recommendations/disposition` and
      `POST .../recommendations/file` (Decision 3) — coordinate fan-out reusing
      #94's idempotent disposition upsert and #68's claim-first file, `single_issue`
      aggregation, `scope=open|all`. Owner-only authz matrix (owner fans out;
      non-owner → 404; uza_ `admin_ro` → 404 on another user's rows, allowed on its
      own; `IsAdmin` never consulted); idempotent double-call; a partial group
      (some members already settled) files/dismisses only the open ones. Depends on
      M1 (shared coordinate-resolve helper + DTO).
- [ ] **M3 — Judge page + nav (web)**: route `/judge`; `<NavItem>` in the Factory
      group with the `triage.todo` badge poll (Decisions 8/9); bucket tabs from
      `triage`; the deduped worklist (group rows + "seen in N runs" + occurrence
      expander + per-group primary **File issue** and overflow **Mark done /
      Dismiss ▾**); the multi-select checkbox bar → bulk M2 calls with an undo
      toast; the **inbox-zero** state (Decision 8). `mockApi.ts` + `data.ts` render
      every state. A revised in-repo mock `prds/mockups/98-judge-menu-mock.html`
      (by-target). Depends on M1 + M2. Parallel with M6/M7.
- [ ] **M4 — `/runs` badge + strip removal (web + api)**: runs-list query gains
      `judge_verdict` + `judge_todo_count` (Decision 7); `RunListItem` type + the
      one-grammar per-row badge; **remove** the global `TriageSummary` strip and its
      `getJudgeStats` call from `RunsList.tsx`. Independent of M1/M2 (own join, own
      files) — starts immediately.
- [ ] **M5 — Notification retarget + inbox grouping (web + api)**: `reviewDeepLink`
      → `/judge?run=` and the inbox `judge_review` link → `/judge?run=` (Decision 4);
      web inbox grouping of consecutive judge rows (Decision 5). **No Slack digest** —
      Slack DMs keep their current one-per-review cadence, only the link changes.
      Files: `judge_worker.go` (the link), `Notifications.tsx` (grouping) — parallel
      from the start.
- [ ] **M6 — Filed→Done sync (api/poller)**: on a linked filed issue closing,
      upsert `done` on every linked coordinate with `set_via='issue_close'` (nullable
      column, Decision 6); no auto-reopen; test that a close drops the rec from To
      triage and (for `improve_uzi`) from the self-improve backlog, and that Undo
      restores it. Builds on #68/#94 + the existing poll loop — parallel from the
      start.
- [ ] **M7 — CLI (`api/cmd/uzi/review.go`)**: `uzi review backlog` (grouped,
      `--bucket`, `--json`) + `file`/`resolve`/`dismiss --category/--target`
      (Decision 10); `commands_test.go` covers the grouped output, the bulk fan-out,
      and a uza_ token refused on a bulk mutation. Depends on M1 + M2.
- [ ] **M8 — Tests + Docs**: e2e leg (dedup grouping; a group **Dismiss** fans out
      across runs and drops an `improve_uzi` rec from the backlog; `single_issue`
      file opens ONE issue linking all open members; issue-close → Done; the
      notification deep-links to `/judge?run=`; **no token spend** on any triage);
      vitest for the page/tabs/zero-state + the `/runs` badge; `docs/judge.md` (the
      menu, dedup grain, group fan-out, inbox grouping, filed-sync) + `docs/cli.md`
      (`review backlog` + bulk verbs); `specs/ai.md` records the decisions.

**Dependency graph** (house convention):

| Phase | Milestones | Depends on | Touches |
|---|---|---|---|
| 1 (parallel) | **M1** grouped read · **M4** /runs badge+strip · **M5** notif retarget+inbox grouping · **M6** filed→Done | existing #46/#68/#94 | `judge_stats`-adjacent · `runs.go`+`RunsList.tsx` · `judge_worker`+`Notifications.tsx` · `poller`/`forgesvc` |
| 2 | **M2** bulk mutation | M1 | new `handler/judge_bulk.go` + `handler.go` routes |
| 3 (parallel) | **M3** Judge page+nav · **M7** CLI | M1 + M2 | `web/` · `api/cmd/uzi/` |
| 4 | **M8** tests + docs | all | e2e/vitest/docs |

M4, M5, M6 are independent of the new API core (own joins/files) and run in
parallel with M1 from day one; M2 gates only the two consumers (M3 web, M7 CLI)
that mutate. Single repo, so no cross-repo phase.

## Success Criteria

- From the **Judge** menu a user sees every open recommendation **deduped by
  `(category, target)` across all their runs**, each with a "seen in N runs"
  count and an expander to the per-run occurrences.
- **One group action settles every occurrence**: a group **Dismiss** / **Mark
  done** dispositions all open members across runs; **File issue** opens one
  GitLab issue that links all open members and (with multi-select) can merge
  several groups into a single issue.
- The **nav badge, the notification, and the page's To-triage tab show the same
  number** (`triage.todo` via the shared `bucketOf`); "seen in N runs" never
  appears as a competing count.
- The **judge notification is unchanged as an event** but deep-links to
  `/judge?run={id}` (web and Slack); the in-app inbox groups consecutive judge
  rows (Slack DMs keep their one-per-review cadence — no Slack digest).
- The `/runs` list shows a **one-grammar per-row judge badge** (`⚖ verdict · N`)
  and **no longer** carries the global strip.
- A **filed issue closing auto-moves its recommendation to Done** (dropping it
  from To triage and, for `improve_uzi`, the self-improve backlog); a human Undo
  restores it; a reopen does not.
- `uzi review backlog` + the bulk verbs drive the **same state** as the web from a
  uzc_ token; a uza_ read-only token can `backlog` but is refused (404) on a bulk
  mutation.
- **No Anthropic token is spent** by any triage/file/dispose action (proven by
  the M8 e2e leg).

## Risks

- **Payload growth on an all-time backlog.** Owner-scoped and ≤50 recs/review,
  but many runs → many groups. Mitigated by the default `?bucket=todo` filter and
  server-side grouping; pagination is a fast follow if a heavy user's To-triage
  ever exceeds one screenful of groups. Documented, bounded by default.
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
- **Filed→Done provenance.** An auto-done (`set_via='issue_close'`) must be
  visibly distinct from a hand-marked done so a user is not confused by a
  disposition they did not set; the column + "done via #IID" label carry it.

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
- **v1 scope includes** bulk multi-select triage/file, **web-inbox grouping** of
  judge rows, and Filed→Done issue sync. **No Slack digest** — judge Slack DMs stay
  one-per-review. **Keyboard triage (j/k)** and a **target-file staleness marker**
  (distinct from #94's rationale-hash stale flag) are **Future Work**.
  [user-decided 2026-07-20]
- **Bulk file default = one issue per group**; multi-select can merge selected
  groups into a **single** issue (`single_issue=true`).
