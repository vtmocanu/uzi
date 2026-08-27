# PRD #102: Board v2 — column rename, label chips, sorting + manual ordering, non-PRD issues

**GitLab Issue**: [#102](https://github.com/vtmocanu/uzi/-/issues/102)
**Status**: Complete (merged 2026-07-28 via MR !142, `7b02b1a0`; released as `v0.12.0` and deployed to dev-cluster. M7's interactive board walkthrough deferred to the user.)
**Priority**: Medium

Four board changes, bundled because they all land in `Board.tsx` and the
board's data path and would otherwise conflict. Phased so the zero-risk
cosmetic work (M1–M4) ships as its own MR without waiting on the sync
change in M6.

## Problem

**1. Two column names misdescribe what they hold.**

`Open` is the implicit column for issues carrying none of the configured
column labels (`OPEN_KEY = ""`, `web/src/pages/Board.tsx:40`). The name
reads as an issue *state*, and GitLab's own board uses `Open` for its
built-in default list, so the same word means an issue-state list there
and a triage lane here. What it holds is untriaged work: every issue uzi
creates gets only the `PRD` label (`api/internal/handler/issues.go:63`),
so it lands there by construction.

`Upcoming` (a real forge label from `forgesvc.DefaultColumns`) is
documented as a "plain backlog bucket" the automation never touches. It
does not record the decision the team actually wants between "somebody
filed it" and "an agent is working it": *selected for development*.

There is an ordering defect too. `DefaultColumns`
(`api/internal/forgesvc/service.go:38`) seeds `In Progress → Human Review
→ Upcoming → Later`, per that file's explicit "the two workflow columns
lead, the backlog buckets follow" convention. A fresh board therefore
reads `Open | In Progress | Human Review | Upcoming | Later | Closed` —
the backlog bucket sits *after* the review column, wrong for a column
meaning "picked, not yet started".

**2. Cards hide their non-workflow labels.** A card shows title, meta and
run badges but never its labels, so `bug`, `security`, `tech-debt` are
invisible. The data already ships: `cardDTO.Labels []string`
(`api/internal/handler/board.go:46`) carries the full set on every card.

**3. Cards cannot be ordered.** Within a column, cards render in the order
the API returned, which is `ORDER BY forge_issue_iid ASC`
(`api/internal/store/queries/forge.sql:186`) — issue number, oldest
first. `Board.tsx:314` buckets by column preserving that order. There is
no way to say "this one first".

**4. Non-PRD issues are invisible, and not by a render filter.** The
`PRD`-label filter is applied at the *sync* layer, in both paths:

```go
// forgesvc/service.go:262 (FullSync) and :291 (IncrementalSync)
f.ListIssues(ctx, forgeProjectID, forge.ListIssuesOptions{Labels: []string{s.prdLabel(ctx)}})
```

Non-PRD issues never enter the `issues` cache, so there is nothing local
for a UI toggle to reveal, and the board cannot show the team what it is
not already tracking.

## Solution Overview

- **`Open` → `Backlog`**: display-only. The implicit column has no label
  behind it; nothing is written to the forge, `OPEN_KEY` stays `""`.
- **`Upcoming` → `Planned`, seeded first**: renames the seeded default
  label and moves it ahead of `In Progress`, giving
  `Backlog | Planned | In Progress | Human Review | Later | Closed`.
- **Label chips on cards**: render each card's labels minus workflow
  markers and minus the configured column labels.
- **Manual ordering + sort modes**: uzi-owned drag-to-reorder within a
  column, durable and cross-device for the board's owner (**not** shared
  across users — see Decision 8's correction), offered alongside a
  board-wide sort-mode switcher (issue number, run activity, last updated,
  title) whose default reproduces today's order exactly. Dragging
  auto-switches the board into manual mode.
- **Show non-PRD issues**: an *additive* second fetch for open issues
  without the `PRD` label, gated by a per-user toggle, with a `Promote`
  action to turn one into a real PRD card.

## Key Distinction (the thing to not get wrong)

`Backlog` and `Planned` are not the same kind of object:

| Column | Backed by | uzi writes to forge? | Visible in GitLab? |
|---|---|---|---|
| `Backlog` | nothing (implicit, `column == ""`) | never | no — GitLab shows these in its own built-in `Open` list |
| `Planned` | a real GitLab label | yes, on every drag (forge-first) | yes, in the project's label list |

Intentional: `Backlog` means "nobody has decided anything yet", so there
is nothing worth recording on the forge. `Planned` is an explicit
decision, so it earns a real label that is queryable in GitLab itself.

Consequence: uzi says `Backlog` where GitLab's board says `Open`, and
they cannot be reconciled. GitLab's `Open` list is built-in and, per
GitLab's docs, cannot be renamed or moved — only hidden. Hiding it is
worse than the mismatch, because untriaged issues would then be invisible
on the GitLab board rather than relocated. We accept the divergence.

`PRD` and `PRDLESS` are likewise different objects, and conflating them
is the single easiest mistake here:

| Label | Means | Enforced at |
|---|---|---|
| `PRD` | board membership — no label, no card | sync filter, `forgesvc/service.go:262` |
| `PRDLESS` | escape hatch for the PRD *link* requirement | run start, `handler/workers.go:393` |

They stack, they do not substitute. Promoting a raw issue adds `PRD`; it
still needs a `prds/*.md` link **or** `PRDLESS` before a run can start.

## Design Decisions

1. **`Backlog` is a display rename, not a new label** (user, 2026-07-20).
   A real `Backlog` label would not remove the implicit column — anything
   created outside uzi, or with its labels stripped, still has to render
   somewhere — so we would end up with both `Backlog` and `Open`. The
   implicit column is structural; renaming its display string is the fix.

2. **`Planned` seeds at position 0, ahead of `In Progress`** (user,
   2026-07-20). This deliberately breaks the "workflow columns lead,
   backlog buckets follow" convention stated in `service.go`'s comment,
   because reading order should match the flow: intake → selected →
   working → review. `humanReviewPlacement` (`handler/board.go:276`)
   anchors `Human Review` relative to `In Progress` rather than an
   absolute index, so the retrofit path is unaffected. Update that
   comment to say why the convention changed.

3. **No retrofit for already-connected repos.** `ensureHumanReviewColumn`
   sets a precedent for back-filling a column onto older boards, but a
   *rename* is a different act: it would either orphan the operator's
   existing `Upcoming` label or rename a real GitLab label out from under
   them. Both destructive, neither undoable from uzi. Repos already on
   `Upcoming` keep it; the manual procedure is documented in M3.

4. **Do not touch `Upcoming` in Go test fixtures.** The string appears
   across `board/board_test.go`, `forgesvc/mr_watch_test.go:27,183`,
   `runlifecycle/lifecycle_test.go`, `forge/gitlab_test.go`,
   `forge/forgejo_test.go:409,496,505-506`, and
   `handler/board_retrofit_test.go:28,34,40,127,132`, but in every case it
   is an arbitrary operator-defined column name chosen to prove the code
   does *not* special-case anything outside
   `ColumnInProgress`/`ColumnHumanReview`. No test asserts on
   `DefaultColumns`' contents (verified: `rg DefaultColumns api --glob
   '*_test.go'` is empty). Renaming them would add churn and weaken the
   "any label works" signal.

5. **`Later` stays.** `Backlog` = "not yet looked at"; `Later` = "looked
   at, deliberately deferred".

6. **Label chips exclude four things, not two.** The predicate already
   exists — `Board.tsx:822` computes exactly it for column suggestions:

   ```
   .filter((l) => l !== prdLabel && l !== autopilotLabel && l !== prdlessLabel && !names.includes(l))
   ```

   Beyond `PRD` and `PRDLESS` there is the **autopilot** label
   (`settings.DefaultAutopilotLabel = "autopilot"`) and the **configured
   column labels** — a card in `Planned` must not also wear a `Planned`
   chip. All four are operator-configurable, so the filter reads them from
   settings, never hardcoded. M4 extracts the predicate so the chip
   renderer and the column suggester share one implementation.

7. **Manual ordering is uzi-owned, not forge-derived** (user,
   2026-07-20, after evaluating the forge-synced alternative). GitLab
   supports it — `PUT /projects/:id/issues/:issue_iid/reorder` with
   `move_after_id`/`move_before_id`, plus `order_by=relative_position` on
   list-issues, so it round-trips even though GitLab never returns the
   field. **Forgejo cannot.** Its ordering lives on *project boards*, a
   separate entity with its own columns and card records, and that API is
   unreleased (Forgejo PR #9384 closed Feb 2026, split into smaller PRs
   still in development mid-2026). Using it would require uzi to maintain
   a shadow Forgejo project per repo mirrored onto the label columns — a
   second source of truth, every drag needing two writes that can
   diverge. A forge-synced order would be GitLab-only, exactly the
   driver-specific behavior the neutral `Forge` interface
   (`api/internal/forge/forge.go:297`) exists to prevent. A uzi-owned
   order behaves identically on both drivers. It is board *presentation*
   state, not forge data — document it so nobody reports it as a sync bug.

7a. **Manual order is one of several sort modes, and the default mode
    reproduces today's board exactly** (user, 2026-07-27). The modes:
    `Manual` (default), `Issue number`, `Recent run activity`,
    `Last updated`, `Title`.

    `Manual` is not "the manual order or nothing": its tiebreak for cards
    nobody has dragged is `forge_issue_iid ASC`, so on an untouched board
    it renders byte-for-byte what ships today. That is what makes it safe
    as the default — today's behavior is what you get without choosing
    anything, rather than something you have to go and pick. `Issue
    number` is therefore *not* redundant with it: it is the escape hatch
    that ignores the manual order entirely, which is the only way back to
    plain issue-number order once someone has dragged.

    **Note what "the current sort" is not.** Today's order is
    `ORDER BY forge_issue_iid ASC` (`store/queries/forge.sql:186`) — issue
    number, which is monotonic per project on **both** drivers and
    therefore exactly creation order on both (GitLab's iid; Forgejo's
    `Index`, mapped at `forge/forgejo.go:679`, which is monotonic but
    shared with pull requests, so an issue-only board sees creation order
    with gaps in the numbering). It is **not** GitLab's board order: uzi
    never reads `relative_position`, and per Decision 7 it never will, so
    "sort the way GitLab's board does" is not an offerable mode on both
    drivers.

    Sorting is a pure client-side function. The board payload already
    ships every card for the repo in one response and `Board.tsx:314`
    buckets them in memory, so no query change and no new read endpoint.
    Verified 2026-07-27: there is no cap and no pagination anywhere on
    that payload — `buildBoard` (`handler/board.go:329-376`) calls
    `ListIssuesByRepo` with no `LIMIT`, the web client is a single
    `GET /repos/${repoId}/board` (`api.ts:1728`) with no page param, and
    `maxBoardColumns = 10` caps columns only. Both drivers paginate
    upstream to exhaustion (`gitlab.go:246-274`, `forgejo.go:293-335`).
    Cost by mode: `Issue number`, `Title` and `Recent run activity` are
    free (`iid`, `title`, and `latest_run.updated_at` — pinned below —
    already ride `cardDTO`); `Last updated` is the one API change —
    `issues.forge_updated_at` exists on the row
    (`migrations/00002_forge.sql:57`) but is not on `cardDTO`
    (`handler/board.go:42-71`), so the field is added.

    **`Recent run activity` needs two things pinned that "free" hides**
    (review, 2026-07-27). `cardDTO.LatestRun` is a **pointer**, null on
    every card that has never run (`handler/board.go:66`, web mirror
    `api.ts:358`) — and on a real board most cards have never run. Sort
    key: **`latest_run.updated_at`**, matching the mode's name, descending.
    Null placement: never-run cards last, `forge_issue_iid ASC` among
    themselves, mirroring the NULLS-LAST rule in 7b. Note also that
    `ListLatestRunsForRepo` picks the latest run by `created_at DESC`
    (`forge.sql:216`), so the chosen row's `updated_at` is the newest
    run's, not the max across the issue's runs — intended, and stated here
    so nobody "fixes" it later.

7b. **Dragging auto-switches the board to `Manual`, and EVERY drop freezes
    the visible order before it moves anything** (user, 2026-07-27).
    Reposition-drag stays live in *every* mode; there is no "switch to
    Manual first" dead state and no disabled-drag affordance to explain.

    The trap that makes this more than a one-line pref write: sorted by
    `Last updated`, drag card #7 to the top. If the drop writes only #7's
    position and then flips the mode, every *other* card falls back to the
    iid tiebreak and the whole board re-sorts under the user's hand — the
    drag reads as having scrambled the board rather than moved one card.
    So a drop **materializes the currently displayed order of every
    non-closed card into the ordering column, then applies the move**:
    freeze, then move. One bulk write, and boards are small (see Open
    Questions, M6 scale).

    **The freeze fires on every drop, including one already in `Manual`
    mode — gating it on "mode != Manual" reintroduces the exact bug this
    decision exists to prevent** (review, 2026-07-27). On an untouched
    board every ordering value is NULL and the default mode is already
    `Manual`, so a mode-gated freeze would not fire; the drop writes one
    position, and that single non-NULL card sorts ahead of every NULL
    under `NULLS LAST`. Drag a card to the *bottom* of a column and it
    renders at the *top*. Once every card is non-NULL the freeze changes
    no ordering — though it still rewrites every non-closed row — so
    making it unconditional removes the only state in which the gate is
    wrong, at no cost to the result.

    The freeze is board-wide, not per-column, because the mode is
    board-wide (7a): flipping to `Manual` re-sorts the untouched columns
    too, and those must not move either.

    **Freeze the PAYLOAD set, not the rendered set** (review, 2026-07-27).
    "Currently displayed" is the wrong scope once M6's non-PRD toggle
    exists: a viewer with the toggle off would freeze only the PRD cards,
    leaving every non-PRD card NULL and therefore relocated to the bottom
    of its column on the viewer's *other* browser, where the toggle is on.
    Same hazard, smaller blast radius, for Decision 13a's excluded
    self-improve issue. So positions are computed for every non-closed
    card in the board payload using the same pure sort function, not from
    visual position — hidden cards included.

    **Closed cards are excluded from the freeze** and keep a NULL order.
    Closed is not a drop target (`Board.tsx:310` `droppable: false`,
    `:468-472`, `:597`, and `move()` returns early at `:253`), so a
    position there is unreachable by drag — but a frozen one would ride an
    issue that later reopens on the forge and drop it at an arbitrary
    point in its new column, contradicting the newly-synced-goes-to-the-
    bottom rule below.

    **Concurrency: the reorder write is board-wide and last-writer-wins
    across the owner's own tabs and devices. Accepted** (review,
    2026-07-27). Today's `move()` is per-card and forge-first
    (`Board.tsx:252`); a whole-board write is a new conflict class, and
    the board polls every 10s (`Board.tsx:208`), so two tabs can each
    submit an order computed from a different snapshot and the second
    discards all of the first. A draft of this paragraph specified a
    board-version check that 409s a stale write — **dropped**, on two
    grounds: `boardDTO` carries no version, etag or generation field
    (`handler/board.go:165-179`, web mirror `api.ts:365-378`), so the
    client has nothing to send and adding one is exactly the "no other API
    read change" M5 forbids; and per Decision 8's correction the conflict
    is now one person's two tabs rather than two people, which the
    existing refetch-on-poll already resolves within 10s. Revisit if
    cross-user boards ever ship.

    Unknown iids — evicted by `DeleteIssuesNotIn`
    (`forgesvc/service.go:287`, keep-set built at `:283-286`) between
    render and submit — are a per-iid no-op, never a 404. One stale card
    must not fail the whole freeze. This rule is independent of the
    dropped version check and stands.

    **Positions are one board-global gapped-integer sequence**, not a
    per-column sequence restarting at each lane (review, 2026-07-27).
    Global is what makes a cross-column drag well-defined — a card moved
    between columns carries a number that still means something in its
    destination, where a per-column sequence would hand it a colliding
    one. Gaps leave room to insert without rewriting neighbours. This also
    settles what freeze test 3 compares, and it is the missing piece the
    M5-drop-semantics Open Question needs in order to close.

    Cards synced *after* a freeze have a NULL order. `ORDER BY <order>
    NULLS LAST, forge_issue_iid ASC` lands them at the bottom of their
    column, ordered by iid among themselves — the concrete reading of M5's
    "newly synced issues take the fallback rather than jumping to the top".

8. **Ordering is durable and cross-device; visibility is per-browser.**

   > **Corrected 2026-07-27 (review).** This decision read *"Ordering is
   > shared … A manual priority order is a team statement about what to
   > work on next, so it goes in the DB, shared"*, and the word `shared`
   > was false in the sense it was being used. **There is no second viewer
   > of a uzi board today.** `issues.repo_id → repos.connection_id →
   > forge_connections.user_id` (`migrations/00002_forge.sql:49`, `:25`,
   > `:8` respectively), so
   > two people who each connect `vtmocanu/uzi` get two `forge_connections`
   > rows, two `repos` rows, and two independent `issues` caches under
   > different `repo_id`s. Every board route resolves through
   > `GetRepoForUser` — `WHERE r.id = $1 AND c.user_id = $2`
   > (`store/queries/forge.sql:80-88`), called from `repoForRequest`
   > (`handler/board.go:800-817`) by all five board handlers (`:189`,
   > `:467`, `:549`, `:662`, `:775`); run creation is scoped the same way
   > (`workersvc/service.go:2244`). An ordering column on `issues` is
   > therefore **per-owner state**, and putting it in the DB buys
   > durability and cross-device consistency for its one owner, not team
   > sharing.
   >
   > The `IsMine` / "a shared board must not leak another user's email"
   > language on `cardDTO` (`handler/board.go:65,75,93,126,146`) is PRD
   > #33 designing *defensively* for a shared board, not evidence of one:
   > `assembleCards` passes `repo.UserID` as the viewer (`:362-365`), and
   > that is the connection owner by construction.
   >
   > One genuine cross-user `repos` path exists and does **not** weaken
   > this: admin `PatchRepo` (`handler/forge.go:589`) branches to
   > `SetRepoSkillsEnabled` / `SetRepoDevboxOptIn` (`forge.sql:111-115`,
   > `:124-128`, both commented "not scoped to the owning user") for an
   > admin caller. It writes two opt-in booleans and reads no board, card,
   > issue or ordering state. Noted so the next reader who greps for
   > unscoped `repos` access does not think this correction missed it.

   Board state today is split: `board_columns` is per-repo (and, per the
   correction above, therefore also per-owner) in the DB, while
   `hideEmpty` is per-browser in `localStorage` (`Board.tsx:79-80`,
   `prefs.ts`). A manual priority order is a durable statement about what
   to work on next rather than a glance-level view choice, so it goes in
   the DB. The non-PRD toggle is one person choosing what to look at right
   now, so it uses `prefs`, keyed per repo, like Hide-empty — which is
   per-browser, not truly per-user (the same user on two browsers sees two
   states); acceptable for a view preference.
   - Accepted cost: with a per-browser toggle, `In Progress` can show a
     different card count in different browsers. "How many are in flight"
     stops being a shared number. Revisit only if it causes real
     confusion.
   - **The sort MODE (Decision 7a) is per-browser too, and its default is
     `Manual`.** The order is durable, the choice of whether to honor it
     is a view preference, so the mode goes in `prefs` keyed per repo like
     Hide-empty. The default matters for a plainer reason than the one
     first written here: `Manual` on an untouched board *is*
     `forge_issue_iid ASC`, so defaulting to it is what makes the feature
     invisible until someone wants it (7a). An earlier draft justified the
     default by "otherwise a second user cannot see the shared order" —
     struck, because per the correction above there is no second user.
     Same default, sound reason.
   - **Accepted cost of the board-wide freeze (7b), now that it is
     per-owner:** dragging one card while sorted by `Title` rewrites your
     own stored order for the whole board, on every device you use. Within
     one person's own board that is a defensible read of "I want it to
     look like this"; it would not have been if boards were shared. If
     cross-user boards ever ship, revisit 7b before they do — see the open
     question below.

9. **The non-PRD fetch is ADDITIVE; today's PRD sync is untouched**
   (user, 2026-07-20). Not a widening of the existing filter — a second,
   independent fetch: `state=opened`, no label filter, discarding rows
   that carry the `PRD` label (the existing path owns those). Today's
   `FullSync`/`IncrementalSync` PRD behavior, including `state=all` so
   closed PRD cards reach the Closed column, is unchanged.

10. **`State` belongs in `ListIssuesOptions`.** The driver currently
    hardcodes `state=all` and does not expose state at all
    (`forge.go:282-284`: *"State is always queried as 'all' by the driver
    (the Closed column requires it), so it is not exposed here"*). The
    additive fetch wants open-only; without a `State` option it would pull
    the entire closed history on every `FullSync` (the reconcile pass, not
    a one-time import) purely to discard it. Since Decision 9 adds a
    second fetch regardless, the interface addition is the *cheaper*
    option, not the more expensive one. Add `State` to
    `ListIssuesOptions`, defaulting to today's `all` so every existing
    caller is unaffected, and implement in both drivers.

11. **Eviction becomes a union, and must fail closed.** `FullSync`
    deletes everything absent from its keep-set (the
    `DeleteIssuesNotIn` call at `service.go:287`; the keep-set is built at
    `:283-286` — both anchors read `:279` / `:274-278` until 2026-07-27
    and had drifted about eight lines; `:279` is `upsertIssues`' error
    return):

    ```go
    s.q.DeleteIssuesNotIn(ctx, ...{RepoID: repoID, KeepIids: keep})
    ```

    Built from the PRD fetch alone, it would wipe the non-PRD rows the
    second fetch just wrote, every poll. The keep-set becomes the union of
    both fetches. Critically: **if either fetch fails, no eviction runs at
    all** — a union missing one half is not authoritative, and treating it
    as such deletes the entire backlog on a transient error. This extends
    the discipline already stated at `service.go:264` to the second fetch.
    This is the highest-risk change in the PRD and wants a dedicated test.

11a. **The shared high-water-mark must fail closed too — eviction is not
     the only place the two fetches share state** (review B2). The poller
     holds ONE `hwm` per repo (`poller/poller.go:61-64`, `repoState.hwm`)
     and both sync paths feed it; it is preserved across a poll only when
     the sync returns an error (`poller.go:228-233`). Two hazards Decision
     11 does not cover:
     - **Asymmetric failure.** If the PRD fetch succeeds and the non-PRD
       fetch fails (or vice versa), returning `nil` with an advanced `hwm`
       makes the next `IncrementalSync` (`UpdatedAfter=hwm`,
       `service.go:291-293`) permanently skip the failed path's window
       until the next full reconcile (~10 min at shipped defaults:
       `FORGE_POLL_INTERVAL=1m` × `FORGE_RECONCILE_EVERY=10`). Rule:
       **either fetch failing makes the whole sync return an error** — no
       "the extra fetch is best-effort, log and continue" soft-fail. Not
       merely "no eviction".
     - **Two-fetch window race (both succeed).** PRD fetch at tA, non-PRD
       fetch at tB > tA; a PRD issue updated in (tA, tB) appears in neither
       result, and if a non-PRD row has a higher `updated_at`,
       `max(maxA, maxB)` advances `hwm` past the missed update, which then
       self-heals only at the next reconcile. Fix: advance the shared
       `hwm` by `min(maxA, maxB)`, or keep a per-path `hwm`. The Decision
       11 test must cover `hwm` semantics, not just eviction.

11b. **Autopilot's candidate query rests on the invariant M6 deletes —
     this is not optional** (review B1). `ListAutopilotCandidateIssues`
     (`store/queries/autopilot.sql:11-16`) selects on `state='opened' AND
     jsonb_exists(labels, @label)` ONLY, and its own comment states the
     load-bearing assumption: *"The cache holds only PRD-labelled issues
     (the sync filter), so a match here already carries BOTH the PRD and
     autopilot labels."* M6 makes that false. After the additive fetch, a
     non-PRD open issue carrying the autopilot label becomes a candidate,
     and the poller's detector (`poller`/autopilot path) then:
     - if it has a `prds/*.md` link (`has_prd_link` **is** computed for
       non-PRD rows, `service.go:337`) or carries `PRDLESS`, **starts an
       unattended run on an issue that was never uzi's**;
     - otherwise posts a comment on the forge issue (an outward-facing
       write to someone else's issue);
     - and if Decision 14's gate is added as a new `createRun` error,
       autopilot's error switch has no case for it, hits `default:`,
       records nothing, and re-evaluates every tick — one
       `ListIssueLabelEvents` forge call per issue per minute, forever.
     M6 must therefore add a `PRD`-label predicate to
     `ListAutopilotCandidateIssues` (a new label param — autopilot's
     `SettingsReader` has no `PRDLabel` accessor today, so that is added
     too), **rewrite that SQL comment** (it is now false, and CLAUDE.md
     requires fixing it in the same change), and add an explicit case for
     Decision 14's rejection to autopilot's error switch.

12. **"Is this a PRD card" is derived, not stored.** It is computable from
    the `labels` jsonb the row already holds, so no migration and no new
    DTO field beyond what `cardDTO.Labels` ships. Deriving at read time is
    also *more correct* than a stored boolean, because `prd_label` is
    operator-configurable (`settings.KeyPRDLabel`) — a stored flag goes
    stale the moment someone renames it. One predicate serves the render
    filter, the Start-run gate, and the Promote affordance.

13. **Non-PRD cards flow through columns normally** (user, 2026-07-20).
    `ResolveColumn` (`board/board.go:23`) is untouched and knows nothing
    about `PRD`; a non-PRD card carrying `In Progress` renders there, and
    dragging one writes the column label forge-first like any other card.
    Simpler and more honest than pinning them to `Backlog`.

13a. **The self-improvement tracking issue will surface as a non-PRD
     card** (review S4). `selfimprove/engine.go:33` marks it
     `TrackingLabel = "uzi-self-improve"`, deliberately NOT a PRD or
     autopilot label; it is open, on uzi's own repo. The additive fetch
     caches it and toggle-on renders it, and a stray Promote would slap
     the `PRD` label onto internal machinery. Decision: the non-PRD render
     path (and Promote's eligibility) excludes issues carrying
     `TrackingLabel`. Cheap, and keeps a self-improve run from ever being
     started by hand off the board.

14a. **`Backlog`↔`Open` wire constant must not be renamed** (review nit).
     The move handler sends the literal string `"open"` for the implicit
     column (`Board.tsx:259`) and the server matches it case-insensitively
     (`handler/board.go:564`, `EqualFold`). The M1 rename is display-only:
     it must not touch these two strings or the drag-to-`Backlog` move
     breaks. Grep-replacing `Open`→`Backlog` blindly is the accident to
     avoid.

14. **Non-PRD cards cannot start runs.** Start-run today checks for a PRD
    *link* or `PRDLESS`, **not** the `PRD` label (`handler/workers.go:393`,
    error at `:411`). Every board card having `PRD` made that sufficient;
    once non-PRD cards are visible it is not, and a non-PRD issue that
    happens to contain a `prds/*.md` link would become runnable by
    accident. Add an explicit `PRD`-label check to run eligibility. The
    gate lives in the shared `workersvc` run-create path (both the manual
    handler switch at `workers.go:398-424` and autopilot, per 11b, map its
    rejection), and `workersvc`'s settings interface
    (`workersvc/service.go:360-372`) is judge-scoped today, so a
    `PRDLabel` accessor is added there. The CLI starts runs through the
    same endpoint (`uzicli/client.go:608`), so it is covered server-side
    and needs no CLI change — recorded here per CLAUDE.md's
    check-the-CLI rule.

15. **Promote adds the `PRD` label.** One click makes a non-PRD card a
    normal board citizen. Mechanically the same shape as the existing
    PRDLESS toggle (`Board.tsx:277-290`): an endpoint that adds one label
    forge-first and returns the updated card. No demote — removing a label
    in GitLab is easy, and nobody has asked. **Color caveat** (review S9):
    the PRDLESS apply path hardcodes `EnsureLabels(..., PrdlessLabelColor)`
    (`service.go:214`), so a naive reuse would auto-create a missing `PRD`
    label in amber. Promote must parametrize the color, or skip the
    ensure entirely (the `PRD` label already exists on any repo whose
    board is in use).

16. **The PRDLESS toggle is gated to PRD cards.** Once non-PRD cards
    render, today's Mark/Remove PRDLESS button (`Board.tsx:733`) would
    appear on them, where it grants nothing (run eligibility also requires
    the `PRD` label per Decision 14). A button that looks like it does
    something and does not is worse than no button.

17. **Non-PRD cards are marked with a dashed border, not dimming.** Both
    obvious alternatives are already taken and would collide:
    - `opacity-40` means "this card is being dragged right now"
      (`Board.tsx:526,623`). A permanently dim card reads as perpetually
      mid-drag — and dimming fights the purpose, since the toggle exists
      to make these readable.
    - `border-warn/60 ring-2 ring-warn/40` is `loud`, reserved for
      `awaiting_approval`, "a human is the blocker"
      (`Board.tsx:600-602,621`). A non-PRD card is the least urgent thing
      on the board; ring treatment inverts the hierarchy.

    `border-dashed border-edge` is unclaimed at card scope and already
    means provisional in this codebase at column scope (`:485` the
    non-droppable lane, `:530` the empty placeholder). Full text opacity;
    composes with the drag and approval states because it is a border
    change and those are opacity/ring changes. Verify legibility in both
    ember and mission, where `border-edge` differs. No explicit "no PRD"
    chip — the border plus Promote-instead-of-Start-run carries it, and
    chip space is contended by M4.

## Milestones

**Phase 1 — rename + chips (independent; ships as one MR)**

- [x] **M1 — `Open` → `Backlog` in the web app**: column header, auto-move
      toast text, and the issue detail page all read `Backlog`;
      `npm run typecheck` + `npm test` green. Sites:
      `web/src/pages/Board.tsx:6,55,299`, `web/src/pages/IssueView.tsx:17`,
      `web/src/lib/boardColumns.test.ts:5,10,23` (fixture + comments).

- [x] **M2 — `Planned` seeded first**: `forgesvc.DefaultColumns` reads
      `{Planned, In Progress, Human Review, Later}`, `Planned` keeping the
      old `Upcoming` color (`#6699cc`); leading comment explains the new
      order per Decision 2; `go build ./...` + `go test ./...` green. A
      newly connected repo seeds
      `Backlog | Planned | In Progress | Human Review | Later | Closed`
      and gets a `Planned` label on its GitLab project.

- [x] **M3 — Docs correct and migration documented**: `docs/board.md`
      (`:17` implicit-column entry, `:19` seeded list, `:25` and `:58`
      backlog-bucket prose, `:57` restore-target list) and
      `docs/configuration.md:104` reflect the new names and order.
      `configuration.md` gains the manual procedure for an existing board:
      rename the label in GitLab **first**, then repoint the uzi column —
      the reverse order orphans the label and drops its cards into
      `Backlog`.

- [x] **M4 — Label chips on cards**: each card renders its non-workflow,
      non-column labels as chips. The Decision 6 predicate is extracted to
      a pure, unit-tested helper in `web/src/lib/` (the `runBadge.ts` /
      `boardColumns.ts` discipline). Because the two callers exclude
      against different sets — the suggester at `Board.tsx:822` filters
      against `names` (edit state), the chip renderer against
      `board.columns` — the helper takes the exclusion sets as parameters
      rather than reaching for one global. The issue detail page already
      renders label chips (`IssueView.tsx:138-147`, filtered only by the
      current column and conditionally PRDLESS, so `PRD`/`autopilot` chips
      show there today); M4 adopts the same extracted predicate at
      `IssueView:139` so board and issue view agree. No API change. Chips
      must not compete with the run/pipeline badges, and a card with many
      labels must not blow out the column width.

**Phase 2 — sort modes + manual ordering**

- [x] **M5 — Sort modes and manual ordering within a column**: a nullable
      ordering column on `issues` (migration number assigned at merge time,
      above the live head — currently `00085`), a reorder endpoint,
      drag-to-reorder in the board, and a board-wide sort-mode switcher.
      The five modes of Decision 7a, default `Manual`, whose tiebreak is
      `forge_issue_iid ASC` — so an existing board renders exactly as it
      does today until someone drags, and newly synced issues sort
      `NULLS LAST` to the bottom rather than jumping to the top. Every
      drop, `Manual` included, freezes the board-wide order before it
      moves anything and leaves the board in `Manual` (Decision 7b).
      `cardDTO` gains `forge_updated_at` for the `Last updated` mode; no
      other API read change. The sort is a pure, unit-tested helper in
      `web/src/lib/` (the `runBadge.ts` / `boardColumns.ts` discipline)
      taking cards + mode and returning the ordered list, so every mode is
      testable without a DOM. `npm run typecheck` + `npm test` +
      `go test -count=1 ./...` green.
    - **There is no card-level drop target today.** `onDrop` is on the lane
      (`Board.tsx:474`); cards get `draggable` at `:617`, gated by
      `const draggable = !card.closed` (`:597`), but are not drop targets,
      so reposition needs a new insertion affordance built from scratch.
      The gesture must distinguish "move to another column" (existing,
      writes a label forge-first via `move()` at `:252`) from "reposition
      within this column" (new, uzi-only, no forge write); settle the drop
      semantics before implementing.
    - **Docs** (`docs/board.md`, `audience: user`, renders in-app): the
      page says nothing about ordering or sorting today (`rg 'order|sort'`
      hits only the `order: 30` frontmatter), and M3 is rename-only while
      M6-docs is M6-only, so without a bullet here a visible feature ships
      undocumented — `web/scripts/check-docs.mjs` validates frontmatter and
      links, never coverage. Document the five modes, the `Manual` default,
      that dragging rewrites the stored order for the whole board, and that
      newly synced issues land at the bottom.
    - **CLI**: no change needed, recorded per CLAUDE.md's check-the-CLI
      rule and the precedent Decision 14 sets. `api/cmd/uzi`'s `board` is
      the runs TUI (`tui_*.go`) and `uzicli` has no board client method, so
      the reorder endpoint and the new `cardDTO` field have no CLI
      consumer.
    - **Sync-clobber trap** (review S6): `UpsertIssue`'s `ON CONFLICT DO
      UPDATE SET` (`forge.sql:169-183`) wholesale-sets every listed column
      on every poll. The new ordering column must be **excluded** from that
      SET list (it is uzi-owned, never in the forge payload), or each
      1-minute poll resets manual order. Guard it with a test that
      reorders, syncs, and asserts the order survives.
    - **Freeze tests** (Decision 7b), three, because each catches a
      different way the freeze goes wrong:
      1. A drop taken while sorted by something other than `Manual` leaves
         every card *except* the dragged one in the position it visually
         occupied. The fixture has to be one where the mode order and the
         iid order genuinely differ — with a fixture where they coincide,
         the broken implementation (write one position, let the rest fall
         back to iid) passes.
      2. **On an untouched board in the default `Manual` mode, dragging a
         card to the bottom of its column leaves it at the bottom.** This
         is the one a mode-gated freeze fails: all-NULL ordering plus one
         written position puts the dragged card first under `NULLS LAST`.
      3. A freeze performed with cards hidden from the viewer (M6's
         non-PRD toggle off) leaves those cards' relative order unchanged
         for a viewer with the toggle on — the payload-set rule.

**Phase 3 — non-PRD issues**

- [x] **M6 — Non-PRD issues visible, off by default**: `State` added to
      `ListIssuesOptions` and both drivers, defaulting to today's `all`
      (Decision 10); an additive `state=opened`, no-label fetch in
      `FullSync`/`IncrementalSync` (Decision 9); eviction keep-set unioned
      and failing closed (Decision 11); the shared `hwm` made fail-closed
      and race-safe (Decision 11a); **the autopilot candidate query gains
      a `PRD`-label predicate, its stale SQL comment is rewritten, and
      autopilot's error switch handles the new run-create rejection**
      (Decision 11b — without this, M6 spawns unattended runs on non-PRD
      issues); a per-browser, per-repo **Show non-PRD issues** toggle
      filtering at render, default off so today's behavior is preserved,
      excluding the self-improve tracking issue (Decision 13a). Start-run
      gains the explicit `PRD`-label check in `workersvc` (Decision 14),
      non-PRD cards render dashed (Decision 17) and offer **Promote**
      (Decision 15) in place of Start-run, and the PRDLESS toggle is
      gated to PRD cards (Decision 16). Decisions 11/11a/11b share one
      dedicated test covering eviction, `hwm`, and the autopilot gate
      under each fetch failing independently. The stale-doc sweep
      (below) lands here.

- [x] **M6-docs — Stale-doc sweep** (review S10): M6 invalidates prose
      and code comments beyond `Board.tsx:368`. Correct, in the same MR:
      `docs/board.md` intro + the whole "Why only some issues show up"
      section (`:128-135`) + the "Staying in sync" prose (`:120-122`,
      wrong for non-PRD cards per Decision-18 staleness),
      `docs/gitlab-bot-setup.md:38`; and the code comments that assert the
      cache holds only PRD issues — `handler/board.go:184-187`,
      `store/queries/forge.sql:275-276`, `Board.tsx:1` header, and the
      "state=all is mandatory" driver comments (`gitlab.go:249-250`,
      `forgejo.go:303-304`) that Decision 10 invalidates.

- [~] **M7 — Verified end to end**: a fresh repo connection seeds the new
      columns and labels; `vtmocanu/uzi`'s own board is migrated by hand per
      M3 with cards keeping their placement; ordering, the non-PRD toggle,
      and Promote behave on a real board **on k8s** (per CLAUDE.md, k8s is
      the primary validation target, not compose). **e2e** (review S7):
      `./e2e/run-e2e.sh` stays the local pre-merge gate per CLAUDE.md, so
      the fake forge (`e2e/forge-fake/forge-fake.mjs`) must implement the
      `state` list param and the harness must tolerate two list calls per
      sync cycle (its eviction/race assumptions at `run-e2e.sh:239-256`
      are sensitive to sync shape).
    - **CLOSED 2026-07-28 with the interactive half DEFERRED TO THE USER, at
      their direction.** The PRD is complete; this milestone's remaining work
      is a manual board walkthrough the user will do later. Recorded here
      rather than silently ticked, so nobody reads M7 as fully exercised.

    - **Verified after release (`v0.12.0`, deployed to dev-cluster):**
      - `uzi-api`, `uzi-controller`, `uzi-web` all rolled to `0.12.0` and are
        healthy. The api pod reaching `Running` is itself evidence: migrations
        run at boot under strict goose, so a numbering problem would surface as
        `CrashLoopBackOff`.
      - `goose: successfully migrated database to version: 91` against the real
        12-day-old database, so `00090_issue_board_position` and PRD #35's
        `00091_run_limit_wait` both applied in order. Api listening on `:8080`
        and `:8443`; privilege sweeper 1 checked / 0 violations.
      - `e2e:kind-smoke` green on the tag pipeline — a real KinD cluster,
        `helm install` of this chart, and `scripts/smoke.sh` against it. It only
        runs on protected refs, so the tag is one of the few places it executes.
      - Every milestone's code confirmed present in the **served** bundle at
        `uzi.example.com`: `Backlog`, `Show other issues`, `Promote to `,
        `Move issue #`, `Still saving the previous move`, the five sort-mode
        names, and the chip-overflow fragment.
        **Presence in the bundle is not efficacy** — it shows the code shipped,
        not that it renders or behaves.

    - **STILL UNVERIFIED, and it is the interactive half:** a fresh repo
      connection seeding `Planned` ahead of `In Progress`; the `Planned` label
      appearing in that GitLab project; chips on a real card; drag and keyboard
      reorder; the Decision 7b freeze (drag while sorted by `Last updated` and
      confirm every *other* card holds); the non-PRD toggle, dashed treatment,
      and Promote. Also the M3 hand-migration of `vtmocanu/uzi`'s own board,
      which renames a live GitLab label and was never started.

    - **Why it stopped here, recorded because the reason is not "we forgot":**
      the deployment authenticates via Keycloak OIDC with password login also
      enabled, and the cluster secret holds only service credentials
      (`JWT_SECRET`, controller tokens, the OIDC client secret) — no seeded
      admin. There was no credential available to drive an authenticated
      session, and minting a real user on the instance was not something to do
      unprompted.
    - **Done**: the fake forge now honours `state` **and `labels`** — it
      ignored *both*, and its comment claiming "the caller filters by label"
      was false, which mattered because against it the Decision 11 union
      keep-set was trivially correct (both fetches returned the same set).
      `./e2e/run-e2e.sh` green at `fb9d89da`: exit 0, the terminal
      `tearing down (down -v)` line, 204 PASS / 0 FAIL, **8m02s**.
      `scripts/smoke.sh` green in an isolated project.
    - **Not done**: validation on dev-cluster. Deployment there is GitOps
      via ArgoCD from a `v*` tag, so it cannot happen until this branch
      merges and is released. It is a post-merge step, not a skipped one.
    - **Also not covered**, stated so the green is not read as wider than it
      is: e2e never calls `PUT /repos/{id}/board/order` and holds no
      `board_position` reference, and every board assertion selects by
      `.iid==$iid` — order-independent. **A wrong `ORDER BY` would not have
      failed that run.** e2e proves M5 broke nothing; the freeze itself is
      covered by the live-DB sweep and vitest, both mutation-verified. The
      harness also seeds no non-PRD issue (`makeIssue` defaults `labels` to
      `["PRD"]`), so the union's *discriminating* case rests on the Go
      tests. A board-order phase and a non-PRD fixture are the two things
      that would close it.
    - **Citation drift**: the `run-e2e.sh:239-256` anchor above is stale —
      that range is the cleanup/`KEEP_STACK` block. The real sites are
      ~2205 (reconcile cadence) and ~2462-2472 (the FullSync-eviction
      dedup assertion).

- [x] **M-specs — specs/ai.md updated** (review S1): the repo's specs
      contract requires an AI-decisions record. M2 falsifies
      `specs/ai.md:1528` ("Board order everywhere: In Progress, Human
      Review, Upcoming, Later" — cited as `:1514` until 2026-07-27, which
      is MR-watcher prose); `:342-343` lists the seeded set and is
      **already stale** (three labels, missing Human Review — the same
      PRD-#12 bug caught in `configuration.md:104`); `:326` names the
      implicit `Open`. **M5 falsifies `specs/ai.md:328-329` hardest** —
      *"`issues` — a cache, never authoritative. uzi's only owned board
      state is column config; every issue field is overwritten from the
      forge each sync"* — because M5 puts a uzi-owned ordering column on
      `issues` and mandates excluding it from that overwrite. (An earlier
      draft cited `:2011` as recording manual ordering "not built"; struck
      2026-07-27 — the sole "not built" in the file is `:2025` and it is
      about `mr_state`. Nothing in ai.md records ordering as not built.)
      Add ai.md items for M2 (rename + reorder + the
      dashed-border/State/eviction/hwm design), M5 (ordering is uzi-owned
      board state; sort modes are per-browser with a `Manual` default),
      and M6, and fix the stale lines. Can land per
      milestone rather than all at once.

## Parallelization

| Phase | Milestones | Depends on | Files touched |
|---|---|---|---|
| 1 | M1, M2, M3, M4 | — | `web/src/pages/Board.tsx`, `IssueView.tsx`, `web/src/lib/`, `forgesvc/service.go`, `docs/` |
| 2 | M5 | — (conflicts with Phase 1 in `Board.tsx`) | `Board.tsx`, `web/src/lib/`, `handler/board.go`, `store/` + migration |
| 3 | M6 | M4 (chip helper) | `forge/`, `forgesvc/service.go`, `handler/workers.go`, `handler/board.go`, `Board.tsx`, `docs/` |

M1–M4 are one agent's work and should land first as a single MR. M5 and
M6 both re-touch `Board.tsx` and the board data path, so running them
concurrently against Phase 1 will conflict; sequence them.

## Success Criteria

- The board's first column reads `Backlog`; no user-facing surface in
  `web/` renders the implicit column as `Open`.
- A newly connected repo seeds `Planned, In Progress, Human Review, Later`
  and a `Planned` label appears in that GitLab project's label list.
- Dragging a card to `Planned` writes the label forge-first; dragging to
  `Backlog` removes every column label.
- An already-connected repo's board is unchanged by the deploy (no label
  created, none removed, no card moved).
- A card carrying `bug` shows a `bug` chip; a card carrying `PRD`,
  `PRDLESS`, `autopilot`, or its own column's label shows no chip for
  those.
- Reordering a card within a column survives a reload and a poll cycle,
  and is still there in a different browser on a different device (the
  order is per-owner and durable, not shared across users — Decision 8's
  correction; the criterion here previously said "visible to a second
  user", which no board route can produce).
- On a board nobody has dragged, the default mode renders exactly today's
  `forge_issue_iid ASC` order.
- **On that same untouched board, dragging a card to the bottom of its
  column leaves it at the bottom.** This is the criterion that
  discriminates; the two either side of it pass against a mode-gated
  freeze, which is the defect (Decision 7b).
- Dragging a card while the board is sorted by `Last updated` leaves every
  *other* card in the position it visually occupied, and leaves the board
  in `Manual` mode (Decision 7b).
- An issue synced after a manual reorder appears at the bottom of its
  column, not the top.
- With the toggle off, the board shows exactly the PRD-labeled issues it
  shows today, and closed PRD cards still reach the Closed column.
- With the toggle on, open non-PRD issues appear, render dashed, offer
  Promote and not Start-run, and show no PRDLESS toggle.
- Promote adds the `PRD` label forge-first and the card becomes ordinary.
- **A forge error on either fetch evicts nothing AND does not advance the
  `hwm`** (Decisions 11, 11a) — covered by a test that fails one fetch and
  asserts both the other path's rows survive and the next sync re-fetches
  the failed window.
- **No unattended run or forge comment is ever produced for a non-PRD
  issue** (Decision 11b) — covered by a test that adds the autopilot label
  to a non-PRD open issue with a `prds/*.md` link and asserts no run is
  created and no case runs in the error switch loop.
- `rg '"Upcoming"' api/internal/forgesvc --glob '!*_test.go'` returns
  nothing (the `mr_watch_test.go` fixtures at `:27,183` are kept by
  Decision 4 and are deliberately excluded from this grep).

## Documentation Corrections Folded In

- `docs/configuration.md:104` states uzi "ensures three labels exist on
  that GitLab project — `In Progress`, `Upcoming`, `Later`". Wrong since
  PRD #12 added `Human Review` to `DefaultColumns`: there are four. Found
  2026-07-20 while scoping this PRD; corrected in M3.

## Open Questions

- **M5 drop semantics.** The exact gesture separating reposition from
  move. (The other half of this question — where a card lands when dragged
  into a manually ordered column from another one — was closed 2026-07-27
  by 7b's board-global gapped-integer sequence: the card keeps a number
  that is meaningful in its destination.)
- **M6 scale.** Not a concern at the expected size (user, 2026-07-20:
  thousands of issues are not expected, and the additive fetch is
  open-only). The tripwire if that changes: the non-PRD fetch is unbounded
  and runs every `FullSync`, so a repo with a large *open* issue count
  would want a cap or a per-repo opt-in.
- **Is a per-owner manual order the feature we want?** (raised by the
  2026-07-27 review.) This PRD was written believing the order would be a
  team-visible priority statement; the schema does not support that (see
  Decision 8's correction), so what M5 actually ships is "arrange *your*
  board how you like it, durably, on every device you use". That is still
  worth having and the milestone stands, but the pitch changes and two
  design choices were argued from the wrong premise — the board-wide
  freeze (7b) and the `Manual` default (7a/8), both of which survive on
  other grounds. If a genuinely shared board is wanted, it is a separate
  and much larger PRD: one `repos`/`issues` row per project rather than
  per connection, which touches run ownership, PAT selection, and every
  `*ForUser` query. **Do not bolt a cross-user order onto M5.**

## Out of Scope

- Any change to GitLab's own issue board lists. Uzi never reads them (no
  `/boards` calls in `api/internal/forge/gitlab.go`); the two boards are
  independent views over the same labels.
- Forge-synced card ordering (Decision 7).
- Automatic migration of existing boards (Decision 3).
- Closed non-PRD issues. The additive fetch is open-only; they never enter
  the cache and never reach the Closed column. Consequence to document
  (review S8): a non-PRD card that gets closed on the forge is invisible to
  `IncrementalSync` (which fetches `state=opened`), so it lingers
  open-looking until the next full reconcile (~10 min at shipped defaults)
  evicts it — it never animates into Closed the way a PRD card does. State
  the window in `docs/board.md` so it does not read as a bug.
- Demote (Decision 15).
- Renaming `Later`, or collapsing it into `Backlog` (Decision 5).
- Renaming the fixture columns in the Go test suite (Decision 4).
- Any change to `ColumnInProgress` / `ColumnHumanReview`, the two names
  the run automation is coupled to.
- ~~Sort-by options (updated-at, run state) — a plausible follow-up once
  manual ordering exists, but a separate feature.~~ **Pulled INTO scope
  2026-07-27** (Decisions 7a/7b, M5). Deferring them would have meant
  re-touching the same `cardsByColumn` memo and the same drag gate a
  second time, and M5 has to settle the ordering seam either way. What
  stays out: **per-column** sort modes (the mode is board-wide, Decision
  7a), and a mode stored per-user in the DB rather than per-browser
  (Decision 8).
