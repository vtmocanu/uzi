# PRD #102: Board v2 — column rename, label chips, manual ordering, non-PRD issues

**GitLab Issue**: [#102](https://gitlab.example.com/vtmocanu/uzi/-/issues/102)
**Status**: Draft (created 2026-07-20)
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
first. `Board.tsx:313` buckets by column preserving that order. There is
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
- **Manual ordering**: uzi-owned drag-to-reorder within a column, shared
  across users.
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

8. **Ordering is shared; visibility is per-browser.** Board state today
   is split: `board_columns` is per-repo and shared, `hideEmpty` is
   per-browser in `localStorage` (`Board.tsx:78`, `prefs.ts`). A manual
   priority order is a team statement about what to work on next, so it
   goes in the DB, shared. The non-PRD toggle is one person choosing what
   to look at, so it uses `prefs`, keyed per repo, like Hide-empty — which
   is per-browser, not truly per-user (the same user on two browsers sees
   two states); acceptable for a view preference.
   - Accepted cost: with a per-browser toggle, `In Progress` can show a
     different card count in different browsers. "How many are in flight"
     stops being a shared number. Revisit only if it causes real
     confusion.

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
    `DeleteIssuesNotIn` call at `service.go:279`; the keep-set is built at
    `:274-278`):

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

- [ ] **M1 — `Open` → `Backlog` in the web app**: column header, auto-move
      toast text, and the issue detail page all read `Backlog`;
      `npm run typecheck` + `npm test` green. Sites:
      `web/src/pages/Board.tsx:6,55,299`, `web/src/pages/IssueView.tsx:17`,
      `web/src/lib/boardColumns.test.ts:5,10,23` (fixture + comments).

- [ ] **M2 — `Planned` seeded first**: `forgesvc.DefaultColumns` reads
      `{Planned, In Progress, Human Review, Later}`, `Planned` keeping the
      old `Upcoming` color (`#6699cc`); leading comment explains the new
      order per Decision 2; `go build ./...` + `go test ./...` green. A
      newly connected repo seeds
      `Backlog | Planned | In Progress | Human Review | Later | Closed`
      and gets a `Planned` label on its GitLab project.

- [ ] **M3 — Docs correct and migration documented**: `docs/board.md`
      (`:17` implicit-column entry, `:19` seeded list, `:25` and `:58`
      backlog-bucket prose, `:57` restore-target list) and
      `docs/configuration.md:104` reflect the new names and order.
      `configuration.md` gains the manual procedure for an existing board:
      rename the label in GitLab **first**, then repoint the uzi column —
      the reverse order orphans the label and drops its cards into
      `Backlog`.

- [ ] **M4 — Label chips on cards**: each card renders its non-workflow,
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

**Phase 2 — manual ordering**

- [ ] **M5 — Manual ordering within a column**: a nullable ordering column
      on `issues` (migration number assigned at merge time, above the live
      head — currently `00074`), a reorder endpoint, and drag-to-reorder in
      the board. Cards with no explicit order fall back to
      `forge_issue_iid ASC`, so an existing board is unchanged until
      someone drags, and newly synced issues take the fallback rather than
      jumping to the top. The drag gesture must distinguish "move to
      another column" (existing, writes a label forge-first) from
      "reposition within this column" (new, uzi-only); settle the drop
      semantics before implementing. **Sync-clobber trap** (review S6):
      `UpsertIssue`'s `ON CONFLICT DO UPDATE SET` (`forge.sql:169-183`)
      wholesale-sets every listed column on every poll. The new ordering
      column must be **excluded** from that SET list (it is uzi-owned,
      never in the forge payload), or each 1-minute poll resets manual
      order. Guard it with a test that reorders, syncs, and asserts the
      order survives.

**Phase 3 — non-PRD issues**

- [ ] **M6 — Non-PRD issues visible, off by default**: `State` added to
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

- [ ] **M6-docs — Stale-doc sweep** (review S10): M6 invalidates prose
      and code comments beyond `Board.tsx:368`. Correct, in the same MR:
      `docs/board.md` intro + the whole "Why only some issues show up"
      section (`:128-135`) + the "Staying in sync" prose (`:120-122`,
      wrong for non-PRD cards per Decision-18 staleness),
      `docs/gitlab-bot-setup.md:38`; and the code comments that assert the
      cache holds only PRD issues — `handler/board.go:184-187`,
      `store/queries/forge.sql:275-276`, `Board.tsx:1` header, and the
      "state=all is mandatory" driver comments (`gitlab.go:249-250`,
      `forgejo.go:303-304`) that Decision 10 invalidates.

- [ ] **M7 — Verified end to end**: a fresh repo connection seeds the new
      columns and labels; `vtmocanu/uzi`'s own board is migrated by hand per
      M3 with cards keeping their placement; ordering, the non-PRD toggle,
      and Promote behave on a real board **on k8s** (per CLAUDE.md, k8s is
      the primary validation target, not compose). **e2e** (review S7):
      `./e2e/run-e2e.sh` stays the local pre-merge gate per CLAUDE.md, so
      the fake forge (`e2e/forge-fake/forge-fake.mjs`) must implement the
      `state` list param and the harness must tolerate two list calls per
      sync cycle (its eviction/race assumptions at `run-e2e.sh:239-256`
      are sensitive to sync shape).

- [ ] **M-specs — specs/ai.md updated** (review S1): the repo's specs
      contract requires an AI-decisions record. M2 falsifies
      `specs/ai.md:1514` ("Board order everywhere: In Progress, Human
      Review, Upcoming, Later"); `:342-343` lists the seeded set and is
      **already stale** (three labels, missing Human Review — the same
      PRD-#12 bug caught in `configuration.md:104`); `:326` names the
      implicit `Open`; `:2011` records manual ordering as "not built",
      which M5 changes. Add ai.md items for M2 (rename + reorder + the
      dashed-border/State/eviction/hwm design), M5 (ordering is uzi-owned
      board state), and M6, and fix the two stale lines. Can land per
      milestone rather than all at once.

## Parallelization

| Phase | Milestones | Depends on | Files touched |
|---|---|---|---|
| 1 | M1, M2, M3, M4 | — | `web/src/pages/Board.tsx`, `IssueView.tsx`, `web/src/lib/`, `forgesvc/service.go`, `docs/` |
| 2 | M5 | — (conflicts with Phase 1 in `Board.tsx`) | `Board.tsx`, `handler/board.go`, `store/` + migration |
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
  and is visible to a second user.
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
  move, and where a card lands when dragged into a manually ordered column
  from another one.
- **M6 scale.** Not a concern at the expected size (user, 2026-07-20:
  thousands of issues are not expected, and the additive fetch is
  open-only). The tripwire if that changes: the non-PRD fetch is unbounded
  and runs every `FullSync`, so a repo with a large *open* issue count
  would want a cap or a per-repo opt-in.

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
- Sort-by options (updated-at, run state) — a plausible follow-up once
  manual ordering exists, but a separate feature.
