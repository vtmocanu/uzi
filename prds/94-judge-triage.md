# PRD #94: Triage judge recommendations — resolve, dismiss, and count

**GitLab Issue**: [#94](https://gitlab.example.com/vtmocanu/uzi/-/issues/94)
**Status**: Draft
**Priority**: Medium
**Mockup**: [`prds/mockups/94-judge-triage-mock.html`](mockups/94-judge-triage-mock.html) (global strip + per-review triage bar + five row states + CLI)
**Depends on**: PRD #46 (the judge, `run_reviews` + `review_recommendations`), PRD #68 (`recommendation_filed_issues`, the coordinate-keyed side-table pattern this reuses verbatim, and the `improve_uzi` backlog `NOT EXISTS` this extends). Related: PRD #64 (the `uzi` CLI, second consumer), PRD #19 (`selfimprove` engine backlog).

## Problem

A judge recommendation is a one-way street with one exit. Every judged run
produces a verdict plus structured recommendations (`review_recommendations`,
six categories), the panel renders them (`RunView.tsx:757-780`), and the only
thing a user can do to one is **File issue** (PRD #68). There is no way to say "I
handled this", "I'm not going to do this", or "the judge is wrong — this is a
false positive". So a review never reaches a settled state: a recommendation the
user has decided about looks identical to one they have not, filed-or-not is the
only distinction, and there is no count of how many are still open versus dealt
with — not for a single run, and not across all their runs. The user reads the
list, acts (or not), and the list forgets.

## Solution Overview

Give each recommendation a **disposition** the user sets in one click — **Done**,
or **Dismissed** with a reason (**Won't do** / **Not an issue** = false positive)
— and surface the tallies both in the panel and across all runs.

- **Coordinate-keyed, exactly like the filed link.** The disposition lives in a
  new side-table keyed on the same stable `(review_id, category, target)`
  coordinate PRD #68 introduced, so it survives a re-judge untouched. Dismiss a
  false positive, re-run the judge, it stays dismissed.
- **No token spend, no forge write.** Setting a disposition is a single local
  row upsert. Nothing is enqueued, nothing is written to GitLab.
- **Two counters, one ladder.** A per-review triage bar (`to do / filed / done /
  dismissed`, with the false-positive sub-count) and a global "across all your
  runs" strip, both computed from one precedence ladder so the numbers agree.
- **CLI parity in the same MR.** `uzi review resolve / dismiss / undo / stats`
  drive the same endpoints — the CLAUDE.md "second consumer" obligation, and the
  reason the write endpoint is `RequireUser` (CLI-reachable), not cookie-only.
- **Folds into self-improvement.** A dismissed or done `improve_uzi`
  recommendation drops out of the engine's backlog, the same way a filed one
  already does.

## Design Decisions

1. **Disposition is a NEW coordinate-keyed side-table — not columns on
   `review_recommendations`, and not columns on `recommendation_filed_issues`.**
   Two hosts are rejected for two different reasons. `review_recommendations`
   rows are **deleted-and-reinserted on every re-judge**
   (`UpsertRunReviewWithRecommendations`, `queries/judge.sql:45`), so a per-row
   status is wiped on re-run — the exact trap PRD #68 Decision 6 hit.
   `recommendation_filed_issues` is the wrong host because its row exists **only
   when an issue is filed** (`filed_issue_iid`/`filed_issue_url`/`filed_at` are
   NOT NULL, `00071:36-50`), whereas a disposition routinely exists with **no
   issue** — dismiss-without-filing and mark-done-without-filing are the common
   cases (most of the six categories are settings toggles or template edits the
   admin does directly, never a GitLab issue). Folding disposition onto that row
   would force those columns nullable and entangle the disposition lifecycle with
   the claim-first filing concurrency (D#68 Decision 7), for no gain.

   New table `recommendation_dispositions`, UNIQUE `(review_id, category,
   target)`:
   - `status TEXT NOT NULL CHECK (status IN ('done','dismissed'))`
   - `dismiss_reason TEXT CHECK (dismiss_reason IN ('wont_do','not_an_issue'))`,
     with a table CHECK that it is non-NULL **iff** `status='dismissed'`
   - `set_by_user_id`, `set_at`, `updated_at`
   - FKs: `review_id` **ON DELETE CASCADE** (the disposition dies with the
     review, correct), `set_by_user_id` **ON DELETE SET NULL** (deleting an
     unrelated user must not delete the row; keep the verdict, drop the
     attribution — the `filed_by_user_id` SET-NULL shape).

   It survives the recommendation delete-reinsert because the review row is
   **stable across a re-judge** (same `target_run_id`, upserted not replaced) —
   the identical guarantee the filed table relies on.

2. **Two axes on the coordinate, one precedence ladder.** A recommendation now
   carries two independent facts on its coordinate: **filed** (D#68) and
   **disposition** (this PRD). They compose freely — a row can be filed and then
   marked done, or dismissed without ever being filed. The single bucket a row
   (and every count) falls into, highest wins:

   > **dismissed  >  done  >  filed  >  to-do (open)**

   Rationale: a dismissal is a human "no" and overrides everything; done is
   resolved; filed is in-progress; open is untouched. The row chip, the
   per-review bar, and the global strip all use this one ladder, defined **once
   in a Go helper** consumed by both the per-review DTO (Decision 7) and the
   global aggregate (Decision 8) so the two counters cannot drift.

3. **Sticky across re-judge; stale-flagged, never auto-reopened.** Because the
   disposition is coordinate-keyed and the review row is stable, a done/dismissed
   survives a re-judge untouched — which is the whole point: dismiss a false
   positive, re-run the judge, and it does **not** come back as open to be
   re-triaged. When a re-judge re-emits the same coordinate with a materially
   changed rationale (`set_at < review.updated_at`), the disposition is **kept**
   but the panel flags it "set for an earlier version of this recommendation" so
   the human can Undo if the change matters — mirroring PRD #68's stale-filed
   flag exactly. Inherits the same **`target=''` collapse** (D#68 Decision 6):
   several empty-target recs share one coordinate and therefore one disposition;
   accepted, identical to the filed case, and **inherent to the survive-re-judge
   key** (folding a row-id or rationale-hash into the key would reintroduce the
   carry-forward problem the coordinate key exists to avoid).

4. **Enum-only reasons; no free-text note in v1.** Dismiss carries a reason enum:
   `wont_do` ("valid, but not worth acting on") or `not_an_issue` ("false
   positive — the judge got it wrong"). No free-text field. A note would be a new
   **untrusted-text-storage-and-render** surface — and this panel deliberately
   refuses to render even judge text as anything but escaped plain text
   (`RunView.tsx:749-756`); an enum needs no sanitization, renders as a fixed
   label, and answers the user's ask ("ignored / false positive") precisely. The
   `not_an_issue` tally is the **"false positives"** number in both counters.
   Revisit only if users ask to annotate *why* they dismissed.

5. **Set/clear is `RequireUser` (session OR user-scoped CLI token) — a local,
   non-spend, non-forge mutation.** `PUT
   /api/runs/{id}/review/recommendations/{recID}/disposition` `{status,
   reason?}` and `DELETE .../disposition` (undo) mount on **`RequireUser`**,
   mirroring `POST /{id}/inputs` (`CreateRunInput`, `handler.go:644`) — the
   established posture for a run mutation that is CLI-reachable and neither spends
   tokens nor writes a forge. This is deliberately **not** the cookie-only
   `RequireAuth` path that `rejudge` (`handler.go:661`) and `FileIssue`
   (`handler.go:666`) sit on: those are cookie-only precisely because they mint a
   token-spending run / write to a forge, and the disposition write does neither.
   Mounting it on `RequireUser` is exactly what makes `uzi review resolve/dismiss`
   work from a CLI token (Decision 10). Authorization: **owner-or-admin** to
   resolve the recommendation (the same `GetReviewForTarget` / viewer scoping the
   review read and `FileIssue` use) — and **no** caller-owns-repo check, because
   there is no forge repo in play. recID → coordinate is resolved server-side as
   `FileIssue` does (`handler/review_issue_file.go`); a recID not present in the
   current review returns 404 ("refresh — the review changed"). No forge limiter,
   no CSRF posture beyond whatever `RequireUser` already applies to `inputs` — do
   not invent a new one.

6. **PUT is an idempotent upsert on the coordinate; DELETE is undo — no
   claim-first dance.** Setting done, switching done→dismissed, or changing the
   reason is the same `PUT` re-run (upsert on the unique coordinate), so
   re-clicking is safe and last-writer-wins. Undo is a `DELETE` of the coordinate
   row, returning the row to whatever the **filed** axis says (filed → "Filed",
   else "To do"). None of PRD #68's claim-first concurrency machinery is needed:
   there is **no external side effect** to make exactly-once (no forge issue is
   created), so two concurrent PUTs simply converge on the last value and a DELETE
   is idempotent. The row records `set_by_user_id`/`set_at` for provenance
   ("resolved by X, 2h ago").

7. **Per-review counts ride the review DTO; the ladder is computed server-side,
   once.** `ReviewDTO` (`apitypes/review.go:52`) gains:
   - `dispositions []DispositionDTO` (`{category, target, status, reason,
     set_at}`), alongside `filed_issues`, so the panel renders each row's chip and
     Undo control (a `dispByCoord` map, mirroring the existing `filedByCoord` at
     `RunView.tsx:665`);
   - `triage TriageDTO` (`{total, todo, filed, done, dismissed,
     false_positives}`), computed in `reviewToDTO` (`handler/judge.go:88`) by the
     shared ladder helper (Decision 2).

   The web renders `triage` **directly** — it does not re-derive counts in TS —
   so the per-review bar and `uzi review show` display identical numbers.

8. **The global strip is a server aggregate over the caller's reviews, per-row
   grain, same ladder.** `GET /api/me/judge/stats` (`RequireUser`, owner-scoped —
   mirrors `/me/memory` at `handler.go:346`, so `uzi review stats` works) returns
   the same `TriageDTO` aggregated across every `run_reviews` row where
   `user_id = caller`, counted **per `review_recommendations` row** with its
   coordinate's disposition/filed status — a `LEFT JOIN` of recommendations to
   the two side-tables, a `CASE` expressing the Decision-2 ladder, `GROUP BY`
   bucket. The denominator is **recommendation rows** (what the user actually sees
   on screen), NOT coordinates — the `target=''` collapse means shared-coordinate
   rows share a status, accepted exactly as in the per-review count. **All-time,
   not windowed**: "to do" and "filed" are a true backlog an old row still belongs
   to; a rolling window is a later refinement, not v1. Renders on the
   **RunsList** header (`web/src/pages/RunsList.tsx`, the "your runs" page); it is
   a global backlog and deliberately does **not** respect the list's current
   filters (wiring it to the filtered query would be a bug).

9. **A disposed recommendation drops out of the self-improvement backlog — the
   same exclusion the filed link already gets.** `ListOpenImproveUziRecommendations`
   (`queries/selfimprove.sql:44`, predicate `category='improve_uzi' AND
   addressed_by_run_id IS NULL AND NOT EXISTS(filed)`) gains a **second
   `NOT EXISTS`** against `recommendation_dispositions` on the coordinate. A
   `dismissed` improve_uzi is a human "no"; a `done` one is handled — either way
   the engine must not fold it into its aggregated tracking issue, exactly as
   PRD #68 Decision 12 kept a hand-filed one out. **Row-existence is the
   exclusion** (any disposition excludes, regardless of status); Undo (row
   deleted) re-includes it next cycle. Folds into **M1** because it changes the
   backlog query in the same migration that adds the table; the disposition
   table's own UNIQUE coordinate index serves the correlated lookup (a partial
   index cannot cross tables — same reasoning as #68). `addressed_by_run_id`
   stays an independent, internal marker: a row can be both engine-addressed and
   user-disposed; the panel never shows `addressed`, and its chip precedence is
   the Decision-2 ladder.

10. **CLI: four verbs on the same endpoints, plus a status column on `review
    show`.** Discharges the CLAUDE.md "second consumer" check in this MR:
    - `uzi review resolve <run> <rec>` → `PUT` `status=done`
    - `uzi review dismiss <run> <rec> --reason wont-do|not-an-issue` → `PUT`
      `status=dismissed`
    - `uzi review undo <run> <rec>` → `DELETE`
    - `uzi review stats [--json]` → `GET /me/judge/stats`

    `uzi review show` (`api/cmd/uzi/run.go:162`) gains a per-recommendation
    **status column** and a per-review **triage line**, read from the DTO's
    `dispositions`/`triage` (`renderReview`, `run.go:474`). `<rec>` is the
    recommendation id the `show --json` envelope already carries (`rec.id`); the
    verbs act by recID exactly as the web does. The endpoints being `RequireUser`
    (Decision 5) is precisely what lets the CLI token reach them — `dismiss`/
    `resolve` are the first CLI-driven mutations on the judge surface (today the
    CLI can only *read* the review, PRD #68 left filing browser-only).

**Interactions (for completeness):** the **notifications inbox** needs nothing —
disposition is a self-action, self-notifying is noise. **Run deletion** cascades
the review and its recommendations away, and the `recommendation_dispositions`
rows with them (`review_id` CASCADE); no forge issue is involved. **Hosted
workers** are untouched — this is an API + browser + CLI feature with no worker,
claim, or agent code. The **board cache** is untouched — no forge write. **The
`improve_uzi` self-improve engine** is touched only through the Decision-9
backlog predicate.

## Milestones

- [ ] **M1 — Schema + store**: migration (draft `00073` — the live head is
      `00072`; renumber above the live head at merge per `CLAUDE.md`) creating
      `recommendation_dispositions` keyed `(review_id, category, target)` with the
      status/reason CHECKs (Decision 1) and FK rules (review→CASCADE,
      user→SET NULL); the **same** migration extends
      `ListOpenImproveUziRecommendations` with the disposition `NOT EXISTS`
      (Decision 9). sqlc + queries: upsert-disposition, delete-disposition,
      list-for-review, and the **global per-row aggregate** (Decision 8). A store
      integration test proves: a disposition **survives a re-judge** (the
      `UpsertRunReviewWithRecommendations` delete-reinsert leaves the disposition
      table untouched); the ladder buckets correctly including the
      **dismissed > done > filed > open** precedence; a disposed `improve_uzi`
      **leaves the backlog** and Undo re-includes it.
- [ ] **M2 — API**: `PUT`/`DELETE .../disposition` on `RequireUser` (Decision 5)
      — enum validation (bad `status`/`reason` → 400), owner-or-admin,
      recID→coordinate resolve, idempotent upsert / undo (Decision 6); `GET
      /me/judge/stats` (Decision 8); `ReviewDTO` gains `dispositions` + `triage`
      via the **shared ladder helper** (Decisions 2/7). Its own handler file (e.g.
      `handler/review_disposition.go`) plus the shared ladder helper; the single
      `handler.go` route-block edit and the DTO additions are expected and
      trivial.
- [ ] **M3 — Web**: the JudgePanel gains a per-row **status chip** +
      `Mark done` / `Dismiss ▾ (Won't do / Not an issue)` controls + **Undo** +
      the **collapse-dismissed** toggle + the **stale-disposition** flag
      (`set_at` < review `updated_at`); a **triage bar** (counts + segmented
      meter) at the panel top; the **global strip** on the RunsList header; the
      counts read the server `triage` (never re-derived in TS). `mockApi.ts` +
      `data.ts` extended so the mock stack renders every state
      (to-do/filed/done/dismissed×2/stale).
- [ ] **M4 — CLI**: `uzi review resolve/dismiss/undo/stats` and the `review show`
      status column + triage line (Decision 10); `api/cmd/uzi/commands_test.go`
      coverage of the verbs, the `--reason` enum mapping, and the `stats` output.
- [ ] **M5 — Tests**: Go handler tests — authz matrix (owner sets; admin sets on
      another user's review; non-owner-non-admin → 404), enum validation, an
      idempotent double-PUT, undo, the **global-stats aggregate** including the
      precedence ladder and the **self-improve exclusion**; vitest for the panel
      states + the strip; CLI tests (M4); an **e2e leg** that dismisses a
      recommendation from a stubbed review and asserts it drops out of the
      `improve_uzi` backlog (and re-includes on undo), that **no run is enqueued**,
      and that **no forge write** happens.
- [ ] **M6 — Docs**: `docs/judge.md` gains the triage lifecycle (the four states,
      sticky-across-re-judge, the false-positive count, the self-improve
      exclusion); `docs/cli.md` gains the four verbs; `specs/ai.md` records the
      decisions.

**Dependency graph** (house convention): **M1 → M2 → { M3, M4 } in parallel →
{ M5, M6 } in parallel.** M2 is one migration-dependent API layer; M3 (web) and
M4 (CLI) are independent consumers of the same endpoints touching separate files
(`web/` vs `api/cmd/uzi/`); M5/M6 close out. Single repo, so no cross-repo phase.

## Success Criteria

- From a judged run, a user marks a recommendation **Done** or **Dismissed** (with
  a reason) in one click, and the panel's triage bar updates to match.
- A dismissal **survives a re-run of the judge** for an unchanged coordinate — the
  recommendation stays dismissed, not re-surfaced as open; a materially changed
  rationale flags it stale rather than silently reopening.
- The **false-positive** count reflects `not_an_issue` dismissals, per review and
  across all runs, and the per-review and global counts agree with each other and
  with the on-screen rows (one shared ladder).
- A dismissed **or** done `improve_uzi` recommendation no longer appears in the
  self-improvement engine's backlog; **Undo** re-includes it.
- `uzi review resolve/dismiss/undo/stats` drive the **same state** as the web,
  from a CLI token, with no cookie/CSRF.
- **No Anthropic token is spent and no forge write happens** anywhere in the flow
  — proven by the M5 e2e leg.

## Risks

- **Per-row vs per-coordinate denominator.** The `target=''` collapse (D#68
  Decision 6) makes shared-coordinate rows share a status, so per-row counts can
  look off when the judge emits empty targets. Accepted, identical to PRD #68; a
  judge that populates distinct `target` values (which the prompt already asks
  for) avoids it. Documented, not fixed — fixing reintroduces the carry-forward
  problem the coordinate key exists to avoid.
- **Sticky dispositions can mask a materially changed recommendation.** A re-judge
  that meaningfully rewrites a dismissed rec keeps it hidden; mitigated by the
  stale flag, not by auto-reopening (auto-reopen would re-nag exactly the false
  positives the user silenced).
- **Two exclusion sources on the self-improve backlog** (filed + disposition)
  widen the ways an `improve_uzi` silently leaves the engine's view. Intended, but
  the backlog query comment must name **both** `NOT EXISTS` so a future reader is
  not surprised by an empty tracking issue.

## Resolved Questions

- **Done and Filed are separate states, not merged.** Filing an issue is an
  action (a forge write, D#68); marking done is a triage verdict. You can file
  then later mark done; the ladder ranks done above filed, so a filed-and-done row
  counts as done. Collapsing them loses "I filed an issue but haven't finished
  it."
- **False positive is a *reason* under Dismiss, not a top-level action.** Keeps
  each row to three actions (File / Done / Dismiss ▾); the strip still surfaces
  the FP count separately (the `not_an_issue` sub-count).
- **All existing reviews get triage with no backfill or re-judge.**
  `recommendation_dispositions` starts empty ⇒ every recommendation reads "to
  do"; the controls appear on every recommendation already on screen, old or new,
  spending no token.
- **Global stats are all-time, owner-scoped, per-row.** A window is a later
  refinement; the backlog framing wants totals.
