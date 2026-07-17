# PRD #68: File a forge issue from a judge recommendation

**GitLab Issue**: [#68](https://gitlab.example.com/vtmocanu/uzi/-/issues/68)
**Status**: Draft (created 2026-07-17)
**Priority**: Medium
**Mockup**: [`prds/mockups/68-judge-file-issue-mock.html`](mockups/68-judge-file-issue-mock.html) (5 states)
**Depends on**: PRD #46 (the judge, `run_reviews` + `review_recommendations`). Related: PRD #39 (`ProposalCard`, the human-gated issue-draft shape this reuses), PRD #22 (the `PRDLESS` bypass this relies on), PRD #19 (`app_settings`, `selfimprove_repo`).

## Problem

The judge's recommendations are a dead end. Every judged run (a completed or
failed `issue`/`ci_fix` run of an opted-in user, when the judge is enabled)
produces a verdict plus structured recommendations (`review_recommendations`, six
categories), the run page renders them (`RunView.tsx:696-716`), and there the
trail stops. Acting on one means reading it, copying the text into GitLab by hand,
and re-deriving the context (which run, which agent, what the judge actually saw)
that uzi already has in the row next to it.

Only the `improve_uzi` subset ever reaches a forge, and only as the
self-improvement engine's aggregated tracking issue (`selfimprove/engine.go:321`)
— an admin-only path, gated on a scheduled job most instances never enable. The
other five categories have no path to a forge at all. uzi produces the analysis
and then makes the human do the transcription.

## Solution Overview

Every recommendation gets a **File issue** button. It opens an editable draft the
API templates from rows it already holds; the human reviews it and clicks Create;
uzi files the issue on the forge and persists the link back onto the
recommendation.

- **No new token spend.** The body is templated deterministically from the
  recommendation, the review, and the judged run. The judge prompt, the worker,
  and the claim are untouched.
- **No new forge capability.** `CreateIssue` already exists
  (`forge/forge.go:225`) with three callers.
- **No new forge identity.** The issue is authored by the connection's bot,
  exactly like every other write uzi makes.
- **Nothing auto-starts.** The issue carries `PRD` + `PRDLESS` — visible on the
  board and startable in one click — but never `autopilot`. Filing an issue and
  spending tokens on a run stay separate human decisions.

## Design Decisions

1. **The API templates the body; no second LLM call.** Everything a useful issue
   needs is already stored: `category`, `target`, `rationale_md`, `confidence`
   from the recommendation; `verdict`, `summary_md`, `judge_model` from the
   review; kind/repo/issue-iid/status from the judged run. A deterministic
   template renders them into a body a coding agent can act on. Rejected: a
   fresh LLM call at click time (a new token-spending path, a new worker
   round-trip, and a new failure mode behind a button that should feel instant)
   and judge-authored `issue_title`/`issue_body` at review time (every judged
   run would pay for bodies that are mostly never filed, plus a migration and a
   judge-prompt change — PRD #46 Decision 5's caps exist precisely to keep these
   rows small).

2. **The draft is fetched, then filed — two endpoints, both user-scoped.**
   `GET /api/runs/{id}/review/recommendations/{recID}/issue-draft` returns
   `{default_repo_id, title, description, labels}`; the user edits; `POST
   .../issue` with `{repo_id, title, description}` writes to the forge and
   persists the link. The draft is NOT folded into the existing
   `GET /api/runs/{id}/review` response: a body is up to ~13 KB (rationale 4 KB
   + summary 8 KB + framing) and up to 50 recommendations exist per review
   (`judge_review.go:32-38`), so inlining would inflate every run-page load by
   a worst case of ~650 KB to serve a draft that is usually never opened.

3. **Labels are `PRD` + `PRDLESS`, never `autopilot` — server-side, and the
   autopilot omission is the real guard.** The three labels do three different
   jobs and the distinction is easy to get wrong:
   - `PRD` (`settings.KeyPRDLabel`) is **visibility**: the sync filters on it
     (`forgesvc/service.go:262`), so an unlabelled issue is invisible to uzi
     entirely — no board card, no run, nothing.
   - `PRDLESS` (`settings.KeyPrdlessLabel`) is the **PRD-link bypass** (PRD #22
     Decision 3). Without it `createRun` rejects with `ErrNoPRDLink`
     (`workersvc/service.go:1295`) unless the description matches
     `prds/*.md` (`forgesvc/service.go:49`). A recommendation has no PRD file,
     so `PRD` alone would put a card on the board that cannot start. **Caveat
     (audit):** the bypass also requires the instance-wide `prdless_enabled`
     setting (`handler/workers.go:509`, default `true`); an admin who turned it
     off makes even a PRDLESS-labelled issue fail `ErrNoPRDLink` on Start. The
     docs must name this precondition, and the success criterion below is scoped
     to `prdless_enabled` being on.
   - `autopilot` (`settings.KeyAutopilotLabel`) is the **trigger**, deliberately
     omitted — and this is the actual guard, not belt-and-braces. `detectOne`
     attributes the label event to its adder, then the issue author, matched
     against the connection's declared `human_username`
     (`poller/autopilot.go:112,292`). **That guard is unenforced when the
     connection PAT's own account equals the declared `human_username`** — a
     fully supported setup (a personal PAT, username declared to match), and
     uzi nowhere requires a distinct bot account (`handler/forge.go:298` only
     verifies the username *exists*). In that case a bot-filed
     autopilot-labelled issue *would* self-trigger. So the safety comes from not
     attaching the label, not from an attribution invariant that may not hold.

   Labels are `[]string{PRD, PRDLESS}` assembled server-side from settings, never
   from the request body — the uzi API surface gives the client no way to attach
   `autopilot`. **One residual, out of uzi's hands (audit Low-1):** the issue
   *description* is human-edited, and GitLab executes quick-action slash-commands
   (`/label ~autopilot`) embedded in a description at creation. So the body can
   influence labels at the *forge* layer. Mitigation: strip leading `/`-command
   lines from the templated body before filing (Decision 10). Even if one slips
   through, the autopilot self-trigger still needs the PAT≠human condition above
   to *not* hold, so this is a two-fault chain, not a one-click one.

4. **The repo is category-derived and always overridable.** The category already
   says where the change lives:

   | Category | Default repo | Why |
   |---|---|---|
   | `improve_agent`, `add_agent` | the judged run's repo | repo agents live in that repo's `.claude/agents/` (PRD #37) |
   | `improve_uzi`, `install_worker_tool` | `selfimprove_repo` | uzi's own code; the worker image is built from it |
   | `adjust_template` | `selfimprove_repo` | builtin templates are `go:embed`-shipped from uzi (`agenttmpl/builtins/`) |
   | `enable_tool` | `selfimprove_repo` | weakest fit — see Decision 5 |

   The default is a pre-selection, not a constraint: the picker lists every repo
   the user has connected and Create is enabled once any is chosen. **Honest
   framing (audit):** `selfimprove_repo` is doubly weak as a default — usually
   unset (self-improvement is off by default), and even when set it is one
   admin's connected repo (`forge_connections.user_id`-scoped), so per Decision 8
   it resolves only for that admin. For a non-admin, **4 of the 6 categories are
   expected to open with an empty picker**; the map's real payload is the
   `improve_agent`/`add_agent` → judged-run's-repo default, which is cheap and
   usually resolvable. When the default cannot be resolved — `selfimprove_repo`
   unset, or set to a repo this caller does not own — the draft opens with the
   picker empty and says why, rather than guessing (mock state D). There is no
   "judged run's repo is gone" case: judge-eligible kinds are `issue`/`ci_fix`
   only, both `repo_id NOT NULL`, and `runs.repo_id` is `ON DELETE CASCADE`
   (`00020:32`) with the review cascading too — a live recommendation always has
   a live repo.

5. **Every category gets the button, including the two that fit badly.**
   `enable_tool` is usually an admin settings toggle, not a code change, and
   `adjust_template` splits (builtins are uzi code; custom templates are DB
   rows) — for those, a filed issue can be a category error. Accepted anyway:
   the user's ask is a button on *each* improvement, the human reads the draft
   before anything is written, and special-casing two of six categories buys a
   rule users must learn in exchange for a mis-click they can already see and
   cancel. Revisit if the filed-issue mix shows these two dominating and getting
   closed as invalid.

6. **The filed link lives in its own table, not on the recommendation row
   (revised after review).** The draft was to add `filed_*` columns to
   `review_recommendations`, but that row is deleted-and-reinserted on every
   re-judge (`UpsertRunReviewWithRecommendations`, `queries/judge.sql:45`), so
   the link would need carrying forward — and matching on `(category, target)`
   *fans out*: those coordinates are not unique (the judge can emit two
   `improve_agent` rows for the same agent, cap 50), so a carry `LEFT JOIN`
   either duplicates inserted rows or stamps one link onto several. Instead, a
   small table `recommendation_filed_issues` keyed **`(review_id, category,
   target)`** holds `filed_repo_id`, `filed_issue_iid`, `filed_issue_url`,
   `filed_by_user_id`, `filed_at`. It survives the recommendation
   delete-reinsert untouched (the review row is stable across a re-judge — same
   `target_run_id`, upserted not replaced), makes the fan-out impossible by
   construction, and enforces the arguably-correct guard: **one issue per
   coordinate per review**, not per row. FKs: `review_id` `ON DELETE CASCADE`
   (the link dies with the review, correct), `filed_repo_id` and
   `filed_by_user_id` **`ON DELETE SET NULL`** — disconnecting an *unrelated*
   repo must never delete another run's filed link; the `filed_issue_url` stays
   as the durable pointer (matches `produced_by_user_id`'s existing
   SET-NULL shape). On semantics: when a re-judge re-emits the same
   `(category, target)` with a *different* rationale, the link is kept (a second
   issue would be a forge-side duplicate about the same agent/tool), but the UI
   flags it — `filed_at` predates the review's `updated_at` → "filed for an
   earlier version of this recommendation" — so the human can judge staleness.

7. **One issue per coordinate, enforced claim-first — not read-then-write.**
   A naïve "check unfiled → `CreateIssue`" is check-then-act: two concurrent
   POSTs both pass the check and file two issues (the `409` would only catch
   *sequential* re-clicks). The repo already hit and fixed exactly this in the
   chat-proposal flow (PRD #39 audit): `ConfirmProposal`
   (`handler/chat.go:190-262` + `00054_proposal_confirming.sql`) does an atomic
   `pending → confirming` claim *before* the forge call, so of two concurrent
   confirms exactly one reaches `CreateIssue`. This PRD mirrors it: a row in
   `recommendation_filed_issues` is claimed with a transient `filing_since` (an
   atomic `INSERT … ON CONFLICT DO NOTHING` on the coordinate key, or an
   `UPDATE … WHERE filed_at IS NULL AND filing_since IS NULL`) *before*
   `CreateIssue`; the loser 409s. On forge failure the claim reverts (row
   deleted / `filing_since` cleared); on success it settles with the issue iid.
   A crash between claim and settle strands a `filing_since` row — swept by a
   bounded timeout like `PROPOSAL_CONFIRM_STUCK_TIMEOUT`. Deleting the filed
   issue on the forge does not un-file the coordinate (uzi does not poll for it)
   — accepted: the link points at a deleted issue, honest and one re-judge from
   resetting.

8. **Authorization: owner-or-admin to read, caller-owns-repo to write; admin
   filing kept, provenance surfaced (user decision).** The draft endpoint is
   owner-or-admin scoped like `GetRunReview` (`GetRunForViewer`,
   `workersvc/service.go:1379`, non-owner → not-found). The write additionally
   requires the *caller* to own the target repo, exactly as `CreateIssue` does
   today (`repoForRequest` → `GetRepoForUser` keyed by the session user,
   `board.go:787`). A plain user therefore cannot file against a repo they do
   not own. The residual is a **confused-deputy** (audit Medium-3): recommendation
   text is attacker-controllable (worker-forged, PRD #46 threat model), and an
   admin browsing another user's review could file that text as the **admin's**
   bot into the **admin's** repo, PRD+PRDLESS-labelled. The user chose to keep
   admin filing rather than restrict to the owner, **conditioned on provenance
   being prominent**: the draft shows "from user X's worker, run <id>"
   (`produced_by_user_id`/`produced_by_run_id` already stored, PRD #46) so the
   admin sees whose text they are about to publish. There is no cross-user
   *write* and no per-user forge identity anywhere in uzi — a repo has one
   connection, one PAT, and the bot authors every issue, note, label and MR uzi
   creates.

9. **Forge-first, then link+cache in one tx; settle-failure reports, never
   reverts.** The route mounts behind `forgeLimiter.PerUserMiddleware` like every
   forge-writing route (`handler.go:493-500`; note this bucket is *shared* with
   move/sync/create-run/create-issue — filing competes there, acceptable). Order
   is forge-first (`CLAUDE.md`: "the forge is the source of truth"): claim (D7) →
   `CreateIssue` → in one tx, settle the `recommendation_filed_issues` row and
   upsert the issue into the `issues` cache so the board card appears without a
   poll (the same write `handler/issues.go:70-93` performs). Failure modes:
   - **Forge rejects** → claim reverts, nothing persisted, draft stays open
     (mock state E). This is the *only* error mock E shows.
   - **Crash after `CreateIssue`, before the tx** → the issue exists on the forge
     carrying PRD+PRDLESS, so the next poll syncs it onto the board regardless;
     only the local link is missing, and the stranded `filing_since` is swept.
     The in-tx cache upsert is a latency optimization the sync self-heals.
   - **The tx itself fails after a successful `CreateIssue`** → report
     created-with-warning and do **not** revert (reverting would orphan or
     invite a duplicate of the real issue) — the exact stance `issues.go:94-101`
     already takes.

10. **Untrusted text is fenced, stripped, and re-scanned before it crosses into
    the forge.** `rationale_md`/`summary_md` are LLM output over untrusted traces
    from a user-controlled worker (PRD #46 Decision 5). Two distinct exposures,
    and the original draft under-covered both (audit Medium-1/2):
    - **Agent-instruction injection is already covered.** A downstream agent
      reads a filed issue through `UNTRUSTED_FRAME` (`buildPlanPrompt`,
      `prompt.ts:16,112`), so it is exactly as untrusted to an agent as any
      forge issue. Unchanged, unweakened.
    - **Human-facing markdown / link / image injection is NOT covered by a
      blockquote.** `> [x](https://evil)` and `> ![](https://evil/p.png)` still
      render as a live link / auto-loaded beacon when GitLab renders the issue
      for a human — an IP-leak/exfil vector the ingest scrub explicitly does not
      touch (`RunView.tsx:688-692`). Fix: the templated body puts every untrusted
      field (`summary_md`, `rationale_md`, `target`) inside a **fenced code
      block**, which renders inert, and **strips leading `/`-command lines**
      first (Decision 3's quick-action residual). No blockquote.
    - **Third-party secret leakage across projects.** The ingest `ScrubSecrets`
      matches only four families (`slacksvc/redact.go:27`: `sk-ant-`, `glpat-`,
      `xoxb-`/`xapp-`); a foreign secret (AWS key, DB password, PEM, generic
      bearer) quoted into the trace is not scrubbed, and this flow can write it
      into an issue on a *different* project than the run's repo. Mitigation: run
      the draft body through a **broader secret-shape scan at draft-render time**
      (high-entropy / known-format families beyond the four) and redact hits; the
      human gate and a judge-prompt "never quote verbatim" instruction are the
      backstops, not the primary control. The docs state the cross-project egress
      plainly.
    The body carries a footer naming uzi, the requesting user, and the producing
    run/user, so neither a human on GitLab nor a later reader mistakes
    bot-authorship for uzi vouching for the content (audit Low-3).

11. **Works on every existing recommendation, no backfill, no re-judge (user
    question).** The draft is templated at click time from rows already stored
    (`review_recommendations` + `run_reviews` + the judged run), and
    `recommendation_filed_issues` starts empty, which reads as "nothing filed
    yet." So the button appears in its idle state on every recommendation from
    every run judged before this ships, and filing spends no Anthropic token on
    old runs any more than new ones. The panel already renders old reviews
    (`JudgePanel` fetches by target-run id today); this only adds a button to a
    list already on screen. Nothing about the flow depends on *when* the run was
    judged — the category→repo default reads current settings and the run's repo,
    both independent of the run's age.

12. **A filed `improve_uzi` recommendation drops out of the self-improvement
    backlog.** The engine composes its tracking issue from
    `ListOpenImproveUziRecommendations` (`selfimprove/engine.go:225`, predicate
    `category = 'improve_uzi' AND addressed_by_run_id IS NULL`,
    `queries/selfimprove.sql:38`). Without this, a hand-filed `improve_uzi` issue
    and the engine's aggregated tracking issue would both carry the same
    recommendation. The predicate gains a "no filed link for this coordinate"
    condition (a `NOT EXISTS` against `recommendation_filed_issues`, or the
    partial index extended to match). Decided now, not deferred, because it
    changes the backlog query and its index — folding it into M1's migration
    avoids a second migration touching the same index. The engine stamps
    `addressed_by_run_id` at cycle time, so a row can be both filed and
    engine-addressed; the panel's display precedence is addressed-then-filed.

**Interactions (audit N6), for completeness:** the **notifications inbox** needs
nothing — the actor is the user themselves, and self-notifying is noise. **Run
deletion** cascades the review and its recommendations away, and (Decision 6) the
`recommendation_filed_issues` rows with them; the forge issue survives with no
back-link, consistent with the honesty stance elsewhere. **Hosted workers** are
untouched — this is an API + browser feature; no worker, claim, or agent code
changes. The **board cache** is handled by the Decision 9 upsert.

## Milestones

- [ ] **M1 — Schema + store**: migration (draft `00067`, renumber above the live
      head at merge per `CLAUDE.md`) creating `recommendation_filed_issues`
      keyed `(review_id, category, target)` with `filed_repo_id`,
      `filed_issue_iid`, `filed_issue_url`, `filed_by_user_id`, `filed_at`,
      `filing_since` (Decision 6/7) and the FK delete rules (review→CASCADE,
      repo/user→SET NULL); the same migration extends the `improve_uzi` backlog
      predicate (Decision 12); sqlc + queries for claim / revert / settle / read
      / sweep. A store integration test proves: the filed link survives a
      re-judge (`UpsertRunReviewWithRecommendations` delete-reinsert leaves the
      link table untouched), a claim on an already-claimed coordinate is
      rejected, and **duplicate `(category, target)` coordinates do not fan out**.
- [ ] **M2 — Draft templating**: a pure, unit-tested renderer (recommendation +
      review + judged run → title, fenced-and-stripped body, default repo,
      provenance line) covering the category→repo map (Decision 4) and its
      empty-picker cases, the code-fence + `/`-strip of untrusted fields, and the
      draft-time secret-shape scan (Decision 10); `GET .../issue-draft` behind
      the review's owner-or-admin scope.
- [ ] **M3 — Filing endpoint**: `POST .../issue` — caller-owns-repo via
      `GetRepoForUser`, server-side `[]string{PRD, PRDLESS}` labels, **claim-first**
      then `CreateIssue` then settle+cache in one tx (Decision 7/9), created-with-warning
      on tx failure, 409 on an already-claimed coordinate, per-user forge limiter,
      PAT-redacted errors; a stranded-`filing_since` sweeper on a bounded timeout.
- [ ] **M4 — Web**: the JudgePanel gains the per-row button, the
      `ProposalCard`-shaped inline draft (repo picker, editable
      title/description, server-label badges, **provenance line** "from user X's
      worker"), the filed state with an issue link, the **stale-filed** flag
      (`filed_at` < review `updated_at`), and the no-default and forge-error
      states; `mockApi.ts` + `data.ts` extended so the mock stack renders all.
- [ ] **M5 — Tests**: Go handler tests for both endpoints — authz matrix (owner,
      admin-reads-other-user, non-owner-non-admin → not-found; write against a
      non-owned repo → not-found), the **concurrent double-POST** files exactly
      one issue, 409, forge failure, tx-failure-reports-created; a body-injection
      test (markdown link/image + `/label` line in `rationale_md` → inert in the
      filed body); vitest for the panel states; an e2e leg that files from a
      stubbed review and asserts labels are `PRD`+`PRDLESS` and **no run is
      enqueued**.
- [ ] **M6 — Docs**: `docs/judge.md` gains the filing flow (label meaning, the
      `prdless_enabled` precondition, that nothing auto-starts, that the bot is
      the author, the cross-project egress note); `specs/ai.md` records the
      decisions.

**Dependency graph** (house convention): M1 → { M2, M3 } run in parallel (M2 is
a pure renderer + a read endpoint; M3 is the write path — they share only the M1
schema) → M4 (consumes both endpoints) → { M5, M6 } in parallel. Single repo, so
no cross-repo phase.

## Success Criteria

- From a judged run with recommendations, a user files a GitLab issue in two
  clicks, and the issue contains enough context that an agent started on it
  needs no further explanation.
- On an instance with `prdless_enabled` on, the filed issue lands on the board
  and starts on the first click of Start — no `ErrNoPRDLink`, no hand-added
  labels.
- No run is ever enqueued as a side effect of filing — proven by the M5 e2e leg.
- Two concurrent file attempts on one recommendation create exactly one issue.
- Re-running the judge does not re-arm a duplicate file for an unchanged
  `(category, target)`; a stale-filed recommendation is flagged, not silently
  refiled.
- No new Anthropic token is spent anywhere in the flow, on new or existing runs.
- Untrusted recommendation text in a filed body renders inert (no live link,
  image beacon, or executed quick-action) for a human GitLab viewer.

## Risks

- **Cheap filing invites board noise.** Six categories × every judged run, and a
  low-confidence recommendation is as one-click as a high-confidence one. Human
  gate only. If the board fills, the next lever is a confidence filter, not
  removing the button.
- **`enable_tool`/`adjust_template` mis-files** (Decision 5), accepted with a
  revisit trigger.
- **New egress of untrusted text to a forge** (Decision 10). This is the first
  path that writes judge text to a forge issue: agent-instruction injection is
  fenced by the pre-existing `UNTRUSTED_FRAME`, but human-facing markdown/beacons
  and cross-project secret leakage are new and mitigated here (code-fence,
  `/`-strip, draft-time secret scan, human gate). Reviewers should attack this
  framing specifically.
- **Confused-deputy on the admin path** (Decision 8), accepted by user decision,
  mitigated by prominent provenance rather than removed.

## Resolved Questions

- **Draft shows raw markdown, not a rendered preview.** The editable textarea is
  honest about what gets written and needs no renderer; a preview would mean
  running `Markdown.tsx` over untrusted text, which `RunView.tsx:688-692`
  refuses. (Decision 10 makes the *filed* body inert independently.)
- **A filed `improve_uzi` recommendation is excluded from the self-improvement
  backlog** (Decision 12): `ListOpenImproveUziRecommendations`
  (`selfimprove/engine.go:225`, predicate `queries/selfimprove.sql:38`) gains a
  "not filed" condition so a hand-filed issue and the self-improve tracking issue
  do not cover the same recommendation twice. Decided pre-M1 because it changes
  the backlog query and its partial index in the same migration. A recommendation
  can be both filed and engine-addressed; display precedence is
  addressed-then-filed.
