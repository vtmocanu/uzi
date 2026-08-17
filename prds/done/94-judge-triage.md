# PRD #94: Triage judge recommendations — resolve, dismiss, and count

**GitLab Issue**: [#94](https://github.com/vtmocanu/uzi/-/issues/94)
**Status**: Complete (2026-07-20; merged via MR [!81](https://github.com/vtmocanu/uzi/-/merge_requests/81))
**Priority**: Medium
**Mockup**: [`prds/mockups/94-judge-triage-mock.html`](../mockups/94-judge-triage-mock.html) (global strip + per-review triage bar + row states + CLI)
**Depends on**: PRD #46 (the judge, `run_reviews` + `review_recommendations`), PRD #68 (`recommendation_filed_issues`, the coordinate-keyed side-table pattern this reuses verbatim, and the `improve_uzi` backlog `NOT EXISTS` this extends). Related: PRD #64 (the `uzi` CLI, second consumer), PRD #19 (`selfimprove` engine backlog).
**Review**: fable adversarial pass folded in (2026-07-20). Load-bearing corrections: owner-**only** authz (a uza_ `admin_ro` token keeps `IsAdmin` on `RequireUser`, so owner-or-admin would break the read-only ceiling); the ladder is computed **once in Go** for both counters (a Go helper cannot back a SQL `CASE`); "filed" means a **settled** link (`filed_at IS NOT NULL`), since claim rows are nullable/in-flight; the stale flag keys on a **rationale hash**, not `set_at < updated_at` (which fires on every re-judge). The CSRF worry on the `RequireUser` mount was checked and is a non-issue (presence-dispatch: a Bearer request never reads the cookie; the cookie path is the unmodified CSRF-enforcing `RequireAuth`).

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
- **Two counters, one ladder — in one place.** A per-review triage bar (`to do /
  filed / done / dismissed`, with the false-positive sub-count) and a global
  "across all your runs" strip, both bucketed by **one Go helper** so the numbers
  cannot drift.
- **CLI parity in the same MR.** A `uzi review` command group (`show / resolve /
  dismiss / undo / stats`) drives the same endpoints — the CLAUDE.md "second
  consumer" obligation, and the reason the write endpoint is `RequireUser`
  (CLI-reachable), owner-only, not cookie-only.
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
   `recommendation_filed_issues` is the wrong host because its lifecycle is
   entangled with the claim-first filing flow and its row exists in states a
   disposition has no business sharing: a row is present both for an **in-flight
   claim** and a **settled link** (`filed_issue_iid`/`filed_at` are **nullable**,
   `00071:42,45`; only `filed_issue_url` is `NOT NULL DEFAULT ''`, `00071:43`),
   while a disposition routinely exists with **no issue at all**
   (dismiss-without-filing and mark-done-without-filing are the common cases —
   most of the six categories are settings toggles or template edits the admin
   does directly, never a GitLab issue). Folding disposition onto that row would
   nullable-taint nothing new but would entangle a plain state field with the
   claim-first concurrency machinery (D#68 Decision 7) for no gain.

   New table `recommendation_dispositions`, UNIQUE `(review_id, category,
   target)`:
   - `status TEXT NOT NULL CHECK (status IN ('done','dismissed'))`
   - `dismiss_reason TEXT CHECK (dismiss_reason IN ('wont_do','not_an_issue'))`,
     with a table CHECK that it is non-NULL **iff** `status='dismissed'`
   - `rationale_hash TEXT NOT NULL` — sha256 of the recommendation's
     `rationale_md` at the moment the disposition was set (Decision 3)
   - `set_by_user_id`, `set_at`, `updated_at`
   - FKs: `review_id` **ON DELETE CASCADE** (the disposition dies with the
     review, correct), `set_by_user_id` **ON DELETE SET NULL** (deleting an
     unrelated user must not delete the row).

   It survives the recommendation delete-reinsert because the review row is
   **stable across a re-judge** (same `target_run_id`, upserted not replaced) —
   the identical guarantee the filed table relies on.

2. **Two axes on the coordinate, one precedence ladder — bucketed once, in Go.**
   A recommendation carries two independent facts on its coordinate: **filed**
   (D#68) and **disposition** (this PRD). They compose freely — a row can be
   filed and then marked done, or dismissed without ever being filed. The single
   bucket a row (and every count) falls into, highest wins:

   > **dismissed  >  done  >  filed  >  to-do (open)**

   Two hard rules make the two counters agree:
   - **The ladder is one Go function** — `bucketOf(dispositionStatus,
     filedSettled bool) → bucket` — consumed by **both** the per-review DTO
     (Decision 7) and the global stats handler (Decision 8). There is **no SQL
     `CASE`**: the global query is a plain join that returns, per recommendation
     row, its coordinate's disposition status and a `filed_settled` boolean, and
     Go buckets it. (An earlier draft had the global aggregate bucket in SQL — a
     Go helper cannot back a SQL `CASE`, so that would have re-implemented the
     ladder twice and reintroduced exactly the drift this decision prevents.)
   - **"filed" means a *settled* link** (`filed_at IS NOT NULL`), everywhere — the
     per-review path already skips unsettled claims (`reviewToDTO`,
     `handler/judge.go:100-104`, `!f.FiledAt.Valid → continue`), and the global
     join must match, or an in-flight/stranded claim would count as "filed"
     globally while the panel shows "to do".

3. **Sticky across re-judge; the stale flag keys on a rationale hash, not a
   timestamp.** Because the disposition is coordinate-keyed and the review row is
   stable, a done/dismissed survives a re-judge untouched — which is the whole
   point: dismiss a false positive, re-run the judge, and it does **not** come
   back as open to be re-triaged. To warn the human when the underlying
   recommendation has genuinely changed *under* their disposition, each row
   stores `rationale_hash` at set-time; the panel flags the disposition **stale
   iff the current recommendation's `rationale_md` hashes differently**. A naïve
   `set_at < review.updated_at` was rejected on review: `UpsertRunReview...` sets
   `updated_at = now()` on **every** conflict, byte-identical re-judge included
   (`queries/judge.sql:56-62`), so that predicate means "any re-judge has
   happened", firing a false stale badge on every quietly-dismissed row after any
   re-run — which would gut the flagship "dismiss a false positive, re-judge, it
   stays quietly dismissed" behaviour. The hash makes an unchanged re-judge
   produce **no** stale flag and a changed rationale produce one, so the flag
   means what it says and the success criterion is testable. Inherits the
   **`target=''` collapse** (D#68 Decision 6): several empty-target recs share one
   coordinate and thus one disposition and one hash (of the first such row);
   accepted, identical to the filed case, and inherent to the survive-re-judge
   key.

4. **Enum-only reasons; no free-text note in v1.** Dismiss carries a reason enum:
   `wont_do` ("valid, but not worth acting on") or `not_an_issue` ("false
   positive — the judge got it wrong"). No free-text field. A note would be a new
   **untrusted-text-storage-and-render** surface — and this panel deliberately
   refuses to render even judge text as anything but escaped plain text
   (`RunView.tsx:749-756`); an enum needs no sanitization, renders as a fixed
   label, and answers the user's ask ("ignored / false positive") precisely. The
   `not_an_issue` tally is the **"false positives"** number in both counters.
   Revisit only if users ask to annotate *why* they dismissed.

5. **Set/clear is `RequireUser` + OWNER-ONLY — a local, non-spend, non-forge
   mutation.** `PUT /api/runs/{id}/review/recommendations/{recID}/disposition`
   `{status, reason?}` and `DELETE .../disposition` (undo) mount on
   **`RequireUser`**, mirroring `POST /{id}/inputs` (`CreateRunInput`,
   `handler.go:644`) — the established posture for a run mutation that is
   CLI-reachable and neither spends tokens nor writes a forge. This is
   deliberately **not** the cookie-only `RequireAuth` path that `rejudge`
   (`handler.go:661`) and `FileIssue` (`handler.go:666`) sit on: those are
   cookie-only because they mint a token-spending run / write to a forge, and the
   disposition write does neither. Mounting on `RequireUser` is what makes `uzi
   review resolve/dismiss` work from a CLI token (Decision 10).

   **Authorization is owner-ONLY, not owner-or-admin** — this is the review's
   load-bearing authz correction. `RequireUser` hands a non-`admin_ro` CLI token a
   copy of the user row with `IsAdmin` cleared, degrading owner-or-admin handlers
   to owner-only for free — **but a uza_ `admin_ro` token keeps `IsAdmin`**
   (`middleware/cli_auth.go`, `if row.Scope != ScopeAdminRO { user.IsAdmin =
   false }`), and uza_ is documented **read-only across the whole factory**
   (`clitoken/clitoken.go`, `docs/cli.md`). So an owner-**or-admin** mutation here
   would be the first admin-reaching write on the `RequireUser` path and would let
   a read-only uza_ token mutate any user's triage. The claimed precedent is in
   fact owner-only (`SubmitInput(r.Context(), user.ID, …)`, `workers.go:541`,
   guarded by `TestCreateRunInputIsOwnerOnly`, `runs_test.go:243`) — `IsAdmin` is
   never consulted. So the disposition handlers must resolve the review by
   **strict caller-ownership** (the run's `user_id == caller`, `IsAdmin` never
   consulted — the `SubmitInput(ctx, user.ID, …)` pattern), **NOT** the
   owner-**or-admin** viewer helpers (`GetRunForViewer`/`GetReviewForTarget`,
   `handler.go:646`) that back the review *read* and `FileIssue`: those admit an
   `IsAdmin=true` caller to *any* user's run, so reusing them for the write would
   readmit exactly the cross-user write a uza_ token could otherwise make. Only
   after the review is owner-resolved is `recID → coordinate` looked up **within
   it** (404 if the recID is not in the current review, re-judged away). **Stated
   precisely (the practical consequence, per code review):** because there is no
   scope gate a handler can read, a uza_ token is refused (404) only on
   **another** user's review — on **its own** review `caller == owner`, so it is
   allowed to write, exactly like `CreateRunInput` today. uza_'s "read-only" reach
   is therefore *cross-user*, not "cannot write anything"; an admin can always
   triage their **own** judge runs (web session or any of their own tokens), and
   is blocked only from triaging **other** users' reviews. No forge limiter, no
   caller-owns-repo, no new CSRF posture.

6. **PUT is an idempotent upsert on the coordinate; DELETE is undo — no
   claim-first dance, no actor display.** Setting done, switching done→dismissed,
   or changing the reason is the same `PUT` re-run (upsert on the unique
   coordinate; it re-stamps `rationale_hash`), so re-clicking is safe and
   last-writer-wins. Undo is a `DELETE` of the coordinate row, returning it to
   whatever the **settled-filed** axis says (settled link → "Filed", else "To
   do"). None of PRD #68's claim-first machinery is needed: there is **no external
   side effect** to make exactly-once, so two concurrent PUTs converge on the last
   value and a DELETE is idempotent. The row keeps `set_by_user_id`/`set_at` for
   forensics, but the panel shows only a relative time ("resolved 2h ago"), **not
   an actor** — under owner-only the setter is always the owner, so a name is
   redundant, and an admin *reading* another user's review must not see a
   misleading "resolved by you".

7. **Per-review counts ride the review DTO; the panel never re-derives them.**
   `ReviewDTO` (`apitypes/review.go:52`) gains:
   - `dispositions []DispositionDTO` (`{category, target, status, reason, set_at,
     stale bool}`, alongside `filed_issues`) so the panel renders each row's chip,
     its Undo control, and the (server-computed) stale flag — a `dispByCoord` map
     mirroring the existing `filedByCoord` at `RunView.tsx:665`. `stale` is
     computed server-side (the hash compare, Decision 3) so the browser never sees
     a hash;
   - `triage TriageDTO` (`{total, todo, filed, done, dismissed,
     false_positives}`), computed in `reviewToDTO` (`handler/judge.go:88`) by the
     shared `bucketOf` helper (Decision 2), with `filed` = settled only.

   The web renders `triage` **directly** — it does not re-derive counts in TS — so
   the bar and `uzi review show` display identical numbers.

8. **The global strip is a server aggregate over the caller's reviews, bucketed
   in Go, per-row grain.** `GET /api/me/judge/stats` (`RequireUser`, owner-scoped
   — mirrors `/me/memory` at `handler.go:345`, so `uzi review stats` works)
   joins `run_reviews` (where `user_id = caller` — the column exists, `00059:24`,
   `idx_run_reviews_user`) → `review_recommendations` → LEFT JOIN the two
   side-tables on the coordinate, returning **one flat row per recommendation**
   with `(disposition_status, filed_settled = filed_at IS NOT NULL)`; the handler
   buckets those rows through the **same `bucketOf` helper** and returns a
   `TriageDTO`. The denominator is **recommendation rows** (what the user sees on
   screen), NOT coordinates — the `target=''` collapse means shared-coordinate
   rows share a status, accepted exactly as in the per-review count. The two
   side-table joins cannot fan out (both UNIQUE on the coordinate). **All-time,
   not windowed**: "to do" and "filed" are a true backlog an old row still belongs
   to; a rolling window is a later refinement. Owner-scoped and ≤50 recs/review
   means the flat-row pull is small; bucketing in Go (not SQL) is what keeps the
   ladder single-source. Renders on the **RunsList** header
   (`web/src/pages/RunsList.tsx`); it is a global backlog and deliberately does
   **not** respect the list's current filters (wiring it to the filtered query
   would be a bug).

9. **A disposed recommendation drops out of the self-improvement backlog — the
   same exclusion the filed link already gets.** `ListOpenImproveUziRecommendations`
   (`queries/selfimprove.sql:44`, predicate `category='improve_uzi' AND
   addressed_by_run_id IS NULL AND NOT EXISTS(filed)`) gains a **second
   `NOT EXISTS`** against `recommendation_dispositions` on the coordinate. A
   `dismissed` improve_uzi is a human "no"; a `done` one is handled — either way
   the engine must not fold it into its aggregated tracking issue, exactly as
   PRD #68 Decision 12 kept a hand-filed one out. **Row-existence is the
   exclusion** (any disposition excludes, regardless of status); Undo (row
   deleted) re-includes it next cycle — no claim-first machinery needed, since the
   upsert is atomic and there is no forge write to make exactly-once. Folds into
   **M1** because it changes the backlog query in the same migration that adds the
   table; the disposition table's own UNIQUE coordinate index serves the
   correlated lookup (a partial index cannot cross tables — same reasoning as #68).
   `addressed_by_run_id` stays an independent internal marker; the panel never
   shows it, and its chip precedence is the Decision-2 ladder.

10. **CLI: a `uzi review` command group, absorbing the existing read; rec IDs in
    the human output.** Today the only judge verb is `uzi run review <run-id>`
    (`api/cmd/uzi/run.go:163`, a `run` subcommand) and its human formatter prints
    **no** recommendation IDs (`renderReview`, `run.go:474-517`) — so a naïve
    top-level `uzi review resolve <run> <rec>` would both collide with the noun
    tree and be unusable without `--json`. Resolution:
    - Introduce a top-level **`uzi review` group**: `show <run>`,
      `resolve <run> <rec>`, `dismiss <run> <rec> --reason wont-do|not-an-issue`,
      `undo <run> <rec>`, `stats [--json]`. `uzi review show` **absorbs** today's
      read (verdict + recommendations + a new triage line). `uzi run review` is
      kept as a **hidden deprecated alias** → `uzi review show`, removable once no
      script depends on it (the CLI is young, PRD #64).
    - `uzi review show` prints a **short rec id** (first 8 hex of the rec UUID,
      git-style) per recommendation plus its status; the mutation verbs accept that
      short id (unambiguous-prefix match against the run's *current* review, else
      "refresh — the review changed"), so the verbs are usable straight from the
      human output. `--json` still carries the full `rec.id`.
    - Endpoints: `resolve`→`PUT status=done`, `dismiss`→`PUT status=dismissed`,
      `undo`→`DELETE`, `stats`→`GET /me/judge/stats`. The `RequireUser` +
      owner-only mount (Decision 5) is exactly what lets a uzc_ token drive them;
      a uza_ read-only token can `show`/`stats` across the factory, and its writes
      are owner-only (Decision 5) — refused (404) on another user's review.

**Interactions (for completeness):** the **notifications inbox** needs nothing —
disposition is a self-action. **Run deletion** cascades the review and its
recommendations away, and the `recommendation_dispositions` rows with them
(`review_id` CASCADE). **Hosted workers** are untouched — API + browser + CLI
only, no worker/claim/agent code. The **board cache** is untouched — no forge
write. **Orphaned ("zombie") dispositions:** when a re-judge stops emitting a
coordinate, its disposition row persists (review-keyed, the review survives) but
joins to no recommendation — so it is **inert while orphaned**: absent from the
panel, absent from both counters (per-row denominator), suppressing nothing (the
self-improve exclusion only bites when a rec row exists), and not undoable (both
endpoints are recID-addressed and no recID resolves to it). If a later re-judge
re-emits the coordinate, the old disposition reappears — and the rationale-hash
stale flag (Decision 3) fires **iff** the re-emitted rationale differs, warning
the human rather than silently re-suppressing a possibly-different concern; a
byte-identical re-emission correctly keeps the disposition. Orphans are cleaned by
the review-deletion cascade. Accepted as a documented limitation (same shape as
#68's filed-link orphan, but the hash flag covers the resurrection case #68 could
not).

## Milestones

- [x] **M1 — Schema + store**: migration (draft `00073` — the live head is
      `00072`; renumber above the live head at merge per `CLAUDE.md`) creating
      `recommendation_dispositions` keyed `(review_id, category, target)` with the
      status/reason CHECKs, the `rationale_hash` column (Decision 3), and FK rules
      (review→CASCADE, user→SET NULL); the **same** migration extends
      `ListOpenImproveUziRecommendations` with the disposition `NOT EXISTS`
      (Decision 9). sqlc + queries: upsert-disposition (re-stamps the hash),
      delete-disposition, list-for-review, and the **global flat-join** feeding
      the Go bucketer (Decision 8, returns per-rec `status` + `filed_settled`, no
      SQL `CASE`). A store integration test proves: a disposition **survives a
      re-judge** delete-reinsert with its stored `rationale_hash` **untouched** (the
      stale *flag* is the API-layer hash compare — Decision 3 keeps hashing out of
      the store — so the both-ways stale assertion lives in M5's
      `TestGetRunReviewStaleFlag`, not here); the ladder buckets
      `dismissed > done > filed(settled) > open` and treats an unsettled claim as
      not-filed; a disposed `improve_uzi` **leaves the backlog** and Undo
      re-includes it.
- [x] **M2 — API**: `PUT`/`DELETE .../disposition` on `RequireUser`, **owner-only**
      (Decision 5) — enum validation (bad `status`/`reason` → 400), recID→coordinate
      resolve, idempotent upsert / undo (Decision 6); `GET /me/judge/stats`
      (Decision 8); `ReviewDTO` gains `dispositions` (with server-computed `stale`)
      + `triage`, both via the shared **`bucketOf`** helper (Decisions 2/7). Its own
      handler file (e.g. `handler/review_disposition.go`) plus the shared helper;
      the single `handler.go` route-block edit and the DTO additions are expected.
- [x] **M3 — Web**: the JudgePanel gains a per-row **status chip** +
      `Mark done` / `Dismiss ▾ (Won't do / Not an issue)` controls + **Undo** +
      the **collapse-dismissed** toggle + the **stale-disposition** flag (rendered
      from the DTO's `stale`, "recommendation changed since you resolved"); a
      **triage bar** (counts + segmented meter) at the panel top; the **global
      strip** on the RunsList header; counts read the server `triage` (never
      re-derived in TS). `mockApi.ts` + `data.ts` extended so the mock stack
      renders every state (to-do / filed / done / dismissed×2 / stale).
- [x] **M4 — CLI**: the `uzi review` group + the `uzi run review` deprecated alias
      (Decision 10), the short-rec-id column + triage line in `renderReview`, and
      the four mutating/stats verbs; `api/cmd/uzi/commands_test.go` covers the verbs,
      the `--reason` enum mapping, the short-id resolution, and that a uza_ token is
      refused on a mutation.
- [x] **M5 — Tests**: Go handler tests — the **owner-only** authz matrix (owner
      sets; non-owner session → 404; a **uza_ `admin_ro` token → 404 mutating
      *another* user's review** — but 200 on `show`/`stats`, and allowed to write
      its **own** review, matching `CreateRunInput`; assert the write path uses
      strict caller-ownership, not the owner-or-admin viewer helper), enum
      validation, an idempotent
      double-PUT, undo, the **global-stats aggregate** (precedence ladder,
      unsettled-claim-is-not-filed, the self-improve exclusion); vitest for the
      panel states + the strip; an **e2e leg** that dismisses a recommendation from
      a stubbed review and asserts it drops out of the `improve_uzi` backlog (and
      re-includes on undo), that **no run is enqueued**, and that **no forge write**
      happens.
- [x] **M6 — Docs**: `docs/judge.md` gains the triage lifecycle (the four states,
      sticky-across-re-judge, the hash-based stale flag, the false-positive count,
      the self-improve exclusion, the orphan behaviour); `docs/cli.md` gains the
      `uzi review` group and states the uza_ read-only ceiling is preserved (mutations
      owner-only); `specs/ai.md` records the decisions.

**Dependency graph** (house convention): **M1 → M2 → { M3, M4 } in parallel →
{ M5, M6 } in parallel.** M2 is one migration-dependent API layer; M3 (web) and
M4 (CLI) are independent consumers of the same endpoints touching separate files
(`web/` vs `api/cmd/uzi/`); M5/M6 close out. Single repo, so no cross-repo phase.

## Success Criteria

- From a judged run, a user marks a recommendation **Done** or **Dismissed** (with
  a reason) in one click, and the panel's triage bar updates to match.
- A dismissal **survives a re-run of the judge**: an unchanged re-judge leaves it
  quietly dismissed with **no** stale flag; a re-judge that changes the
  recommendation's rationale flags it stale (hash-based, testable both ways) —
  never silently reopening it.
- The **false-positive** count reflects `not_an_issue` dismissals, per review and
  across all runs; the per-review and global counts agree with each other and with
  the on-screen rows (one `bucketOf` helper, `filed` = settled).
- A dismissed **or** done `improve_uzi` recommendation no longer appears in the
  self-improvement engine's backlog; **Undo** re-includes it.
- `uzi review resolve/dismiss/undo/stats` drive the **same state** as the web,
  from a uzc_ CLI token; a read-only uza_ token can `show`/`stats` but is refused
  (404) on a mutation.
- **No Anthropic token is spent and no forge write happens** anywhere in the flow
  — proven by the M5 e2e leg.

## Risks

- **Per-row vs per-coordinate denominator.** The `target=''` collapse (D#68
  Decision 6) makes shared-coordinate rows share a status, so per-row counts can
  look off when the judge emits empty targets. Accepted, identical to PRD #68; a
  judge that populates distinct `target` values (which the prompt already asks
  for) avoids it. Documented, not fixed — fixing reintroduces the carry-forward
  problem the coordinate key exists to avoid.
- **Orphaned dispositions accumulate as inert cruft** until the review is deleted
  (Interactions). They affect nothing while orphaned and the hash flag guards
  their resurrection, so this is a storage-tidiness cost, not a correctness one.
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
- **Mutations are owner-only, not owner-or-admin.** The `RequireUser` mount is
  what makes the CLI work, but a uza_ `admin_ro` token keeps `IsAdmin` there;
  owner-only (matching the `CreateRunInput` precedent) both fixes that and needs no
  scope inspection in the handler. **"Owner-only" here means strict
  caller-ownership** (the `SubmitInput(user.ID)` pattern) — **not** the
  owner-or-admin viewer helper (`GetRunForViewer`/`GetReviewForTarget`) that backs
  the review read and `FileIssue`; reusing that helper for the write would readmit
  the cross-user hole. Admins keep read access to others' reviews and can triage
  their **own** judge runs; they cannot triage **other** users' reviews.
- **All existing reviews get triage with no backfill or re-judge.**
  `recommendation_dispositions` starts empty ⇒ every recommendation reads "to
  do"; the controls appear on every recommendation already on screen, old or new,
  spending no token.
- **Global stats are all-time, owner-scoped, per-row.** A window is a later
  refinement; the backlog framing wants totals.
