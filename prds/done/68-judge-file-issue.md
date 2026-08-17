# PRD #68: File a forge issue from a judge recommendation

**GitLab Issue**: [#68](https://gitlab.example.com/vtmocanu/uzi/-/issues/68)
**Status**: Complete (2026-07-19) — merged to main via MR !71. Migration landed as `00071` (`00070` was taken by PRD #83's `worker_docker_enabled` on the landing rebase).
**Priority**: Medium
**Mockup**: [`prds/mockups/68-judge-file-issue-mock.html`](../mockups/68-judge-file-issue-mock.html) (5 states)
**Depends on**: PRD #46 (the judge, `run_reviews` + `review_recommendations`). Related: PRD #39 (`ProposalCard`, the human-gated issue-draft shape this reuses), PRD #22 (the `PRDLESS` bypass this relies on), PRD #19 (`app_settings`, `selfimprove_repo`).

## Problem

The judge's recommendations are a dead end. Every judged run (a completed or
failed `issue`/`ci_fix` run of an opted-in user, when the judge is enabled)
produces a verdict plus structured recommendations (`review_recommendations`, six
categories), the run page renders them (`RunView.tsx:714-733`), and there the
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
  (`forge/forge.go:316`, impl `gitlab.go:285`) with three callers
  (`handler/issues.go:63`, `handler/chat.go:237`, `selfimprove/engine.go:321`).
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
   persists the link. A **server** GET (rather than rendering the draft in the
   browser from review data the panel already holds) is chosen because three
   things have to happen server-side and want a single Go source of truth: the
   draft-time secret-shape scan (Decision 10), the category→repo default
   resolution (Decision 4, which reads settings + the caller's connected repos),
   and the code-fence/`/`-strip templating. Payload size is a secondary reason,
   not the primary one: the draft is deliberately NOT folded into the existing
   `GET /api/runs/{id}/review` response, because a body is up to ~13 KB (rationale
   4 KB + summary 8 KB + framing) and up to 50 recommendations exist per review
   (`judge_review.go:32-38`), so inlining would inflate every run-page load in the
   worst case to serve a draft that is usually never opened. **Note the render/write
   asymmetry (see Decision 10):** the sanitizing template here produces the *draft*;
   it is not the security boundary. The body that reaches the forge is the client's
   POST `description`, so the load-bearing controls re-run server-side at the POST.

3. **Labels are `PRD` + `PRDLESS`, never `autopilot` — server-side, and the
   autopilot omission is the real guard.** The three labels do three different
   jobs and the distinction is easy to get wrong:
   - `PRD` (`settings.KeyPRDLabel`) is **visibility**: the sync filters on it
     (`forgesvc/service.go:262`), so an unlabelled issue is invisible to uzi
     entirely — no board card, no run, nothing.
   - `PRDLESS` (`settings.KeyPrdlessLabel`) is the **PRD-link bypass** (PRD #22
     Decision 3). Without it `createRun` rejects with `ErrNoPRDLink`
     (`workersvc/service.go:1302`) unless the description matches
     `prds/*.md` (`forgesvc/service.go:49`). A recommendation has no PRD file,
     so `PRD` alone would put a card on the board that cannot start. **Caveat
     (audit):** the bypass also requires the instance-wide `prdless_enabled`
     setting (read at `handler/workers.go:378`, default `true` at
     `settings/settings.go:97`); an admin who turned it
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
   `autopilot`. **One residual, out of uzi's hands (audit Low-1, upgraded on
   re-review):** the issue *description* is human-edited, and GitLab executes
   quick-action slash-commands embedded in a description at creation (verified
   against GitLab's extractor: commands are anchored `^\/`, slash at column 0, and
   the Banzai `quick_action` pipeline *excludes* fenced code blocks — so a
   correctly fenced block neutralizes both quick-actions and markdown beacons).
   `/label ~autopilot` is only the headline case: `/labels`, `/relabel`,
   `/assign`, `/confidential`, `/move <project>`, `/close` all change label/state
   too, so a strip keyed on the literal `/label` is a **blocklist that misses
   siblings**. The controls, in order of strength:
   - **Primary: the breakout-proof fence (Decision 10).** Every untrusted field
     is fenced with a delimiter longer than the longest backtick run in the
     content, so it cannot break out to become a column-0 `/`-line in the first
     place. This is what actually holds.
   - **Belt-and-braces: strip every line whose first non-fenced character is `/`**
     (not just `/label`), applied server-side to the body being filed, over the
     fully-exposed text. This is a backstop *behind* the fence, not the primary
     control.

   Even if a command slipped through both, the autopilot self-trigger still needs
   the PAT≠human condition above to *not* hold, so it is a multi-fault chain, not
   a one-click one. **Where these controls live matters (audit Medium):** the body
   that reaches the forge is the client's POST `description` (Decision 2), not the
   GET draft, so the fence-and-strip must be a **server-side invariant at the POST
   handler** (Decision 10 / M3), never a property the client is trusted to have
   preserved from the draft.

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

   **Accepted limitation — the `target=''` collapse (audit blocking, resolved
   here).** `target` is `NOT NULL DEFAULT ''` (`00059:50`) and ingest does not
   force it non-empty (`validateAndScrubReview`, `judge_worker.go:330`, only
   enum-checks category/confidence and caps `target` at 255 bytes). So a judge
   can emit several `improve_uzi`/`enable_tool` recommendations that all carry
   `target=''`, and they **collapse to one coordinate**: only the first is
   fileable, and (Decision 12) filing it drops *every* empty-target `improve_uzi`
   in that review from the self-improve backlog. This is **inherent to the
   survive-re-judge key, not a fixable defect**: nothing per-row survives the
   delete-reinsert except `(category, target)`, so folding a row-id or a
   rationale-hash into the key — the rejected alternative — would reintroduce
   exactly the carry-forward problem this table exists to avoid (and a
   rationale-hash would also break the "keep the link when rationale changes"
   semantics above). We accept the collapse: the panel says "this
   category/target already has an issue" on the blocked siblings, and for
   `improve_uzi` specifically the collapse is *benign* because the
   self-improvement engine already aggregates all `improve_uzi` rows into a
   single tracking issue anyway (`selfimprove/engine.go:230`) — the coordinate is
   the right grain for that category. A judge that populates distinct `target`
   values (which the prompt already asks for) avoids the collapse entirely; the
   limitation only bites when the judge leaves `target` empty on genuinely
   distinct recommendations.

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
   bounded timeout.

   **The sweep timeout must be clamped, and this path is harsher than the
   proposal precedent (audit should-fix).** The proposal sweeper clamps
   `PROPOSAL_CONFIRM_STUCK_TIMEOUT` to `≥ 2×ForgeHTTPTimeout` at boot
   (`config.go:551` + the clamp note at `config.go:558`) precisely so a slow-but-
   *alive* forge write is never reverted mid-flight. This flow needs the same
   floor, and needs it more: the proposal revert is `confirming → pending` (a
   state flip), whereas this revert is a **DELETE of the claim row**, so a
   premature sweep during a live `CreateIssue` deletes the claim, a retry or a
   concurrent POST re-`INSERT`s, and you get **two forge issues** — the exact
   duplicate the claim-first design exists to prevent. Requirements: (a) the
   filing sweep timeout clamps to `≥ 2×ForgeHTTPTimeout` like the precedent;
   (b) settle is by row-id and must treat "0 rows updated" (the row was swept out
   from under a slow-but-successful `CreateIssue`) as **created-with-warning**
   (Decision 9), never as an error that would retry the forge write.

   Deleting the filed issue on the forge does not un-file the coordinate (uzi does
   not poll for it) — accepted: the link points at a deleted issue, honest and one
   re-judge from resetting.

8. **Authorization: owner-or-admin to read, caller-owns-repo to write; admin
   filing kept, provenance surfaced (user decision).** The draft endpoint is
   owner-or-admin scoped like `GetRunReview` (`GetRunForViewer`,
   `workersvc/service.go:1385`, non-owner → not-found). The write additionally
   requires the *caller* to own the target repo, exactly as `CreateIssue` does
   today (`repoForRequest` → `GetRepoForUser` keyed by the session user,
   `board.go:800`, the `GetRepoForUser` call at `board.go:811`; the query itself
   joins `repos → forge_connections WHERE r.id = $1 AND c.user_id = $2`,
   `queries/forge.sql`). A plain user therefore cannot file against a repo they do
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
   forge-writing route (the `forgeLimiter.PerUserMiddleware` mounts are at
   `handler.go:518-566` and `652` — e.g. create-issue at `:566`, move at `:560`,
   sync at `:564`, create-run at `:543`; **not** `handler.go:493-500`, which is
   the cookie-only `RequireAdmin` write group — mounting there would be a bug).
   Note this bucket is *shared* with move/sync/create-run/create-issue — filing
   competes there, acceptable. Order
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
   - **Swept-out claim (0 rows settled), a slow-but-successful `CreateIssue` whose
     `filing_since` was reaped before settle** → same created-with-warning stance,
     never a retry of the forge write (Decision 7).

   `EnsureLabels` is intentionally **not** called on this path (like `issues.go`'s
   create): if a project lacks a pre-existing `PRDLESS` label GitLab auto-creates
   it on `CreateIssue` with a random colour rather than `PrdlessLabelColor`
   (`forgesvc/service.go`). Harmless — the Start gate re-reads *live* forge labels
   (`workers.go:378-380`) and keys the bypass on the label *name*, so the card
   still starts on the first click regardless of colour.

10. **Untrusted text is fenced, stripped, and re-scanned at the write boundary
    before it crosses into the forge.** `rationale_md`/`summary_md` are LLM output
    over untrusted traces from a user-controlled worker (PRD #46 Decision 5).
    **The controls run server-side in the POST handler (M3), on the body being
    filed — never only in the GET draft (M2).** This is the load-bearing
    correction (audit Medium/structural): the draft the renderer produces is a UX
    convenience; the bytes that reach the forge are the client's POST
    `description`, which the client may have edited or replaced, so there is no
    "the draft was inert" invariant unless M3 re-applies it. Idempotent controls
    (the `/`-strip and secret-scan) re-run at POST; the fence is render-only
    (re-fencing arbitrary human edits would mangle them), so the write-boundary
    guarantee rests on strip + scan over the exposed text. Three distinct
    exposures:
    - **Agent-instruction injection is already covered.** A downstream agent
      reads a filed issue through `UNTRUSTED_FRAME` (`buildPlanPrompt`,
      `prompt.ts:16,112`), so it is exactly as untrusted to an agent as any
      forge issue. Unchanged, unweakened.
    - **Human-facing markdown / link / image injection, and forge quick-actions,
      are NOT covered by a blockquote — and a naïve fence is defeated by
      fence-breakout (audit HIGH).** `> [x](https://evil)` /
      `> ![](https://evil/p.png)` render as a live link / auto-loaded beacon when
      GitLab renders the issue for a human — an IP-leak/exfil vector the ingest
      scrub does not touch (`RunView.tsx:706-710`), and on a self-managed GitLab
      the asset/image proxy (camo) is **off by default**, so the beacon loads from
      the viewer's browser directly. A plain triple-backtick fence does not fix
      this: ingest (`sanitizeReviewText`, `judge_worker.go:371`) strips control
      chars but **preserves backticks**, so hostile `rationale_md` containing its
      own ``` run closes the fence early and re-exposes the trailing lines as live
      markdown + column-0 `/`-command text — a single fault. Fix: the templated
      body fences every untrusted field (`summary_md`, `rationale_md`, `target`)
      with a **breakout-proof fence — a backtick delimiter strictly longer than
      the longest backtick run in the content** (or an indented code block) — so
      it cannot break out; GitLab's Banzai `quick_action` pipeline also excludes
      fenced blocks, so a correct fence neutralizes quick-actions in the same
      stroke. Behind the fence, **strip every line whose first non-fenced
      character is `/`** (Decision 3, not just `/label`). No blockquote. **Title
      caveat:** the title is derived from worker-controlled `target` and cannot
      hold a fence; on GitLab this is low-risk (titles are not markdown-rendered
      and do not process quick-actions, newlines stripped), but the title is still
      run through the `/`-strip + secret-scan and the point is noted for a future
      Forgejo driver.
    - **Third-party secret leakage across projects — best-effort, not the primary
      control.** The ingest `ScrubSecrets` matches only four families
      (`slacksvc/redact.go:53`: `sk-ant-`, `glpat-`, `xoxb-`/`xapp-`, and the uzi
      token `uz[caw]_`); a foreign secret (AWS `AKIA…`, `ghp_…`, DB URL with
      password, PEM `-----BEGIN … PRIVATE KEY-----`, `Authorization: Bearer <jwt>`)
      quoted into the trace is not scrubbed, and this flow can write it into an
      issue on a *different* project than the run's repo. Mitigation, **ordered
      honestly (audit)**: the **human gate is the primary control**, a broader
      secret-shape scan at the write boundary is **best-effort defense-in-depth**
      (a regex/entropy scan has a large false-negative surface — dictionary-word
      DB passwords, low-entropy blobs — and false-positives on git SHAs/UUIDs, so
      it is not a credible standalone control), and a judge-prompt "never quote
      verbatim" instruction is the upstream backstop. Note the human cannot
      reliably eyeball an AWS key as a secret, which is exactly why the scan
      exists despite its limits. The docs state the cross-project egress plainly.
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
    recommendation. The predicate gains a `NOT EXISTS` against
    `recommendation_filed_issues` for the coordinate. Two refinements from review:
    - **Match on row-existence, not `filed_at IS NOT NULL` (audit should-fix).**
      An in-flight claim (`filing_since` set, `filed_at` still NULL) must also
      exclude the coordinate, or a self-improve cycle running concurrently with a
      file-in-progress folds a mid-filing `improve_uzi` into its tracking issue —
      the same check-then-act race the claim-first design avoids everywhere else.
      So the `NOT EXISTS` keys on the coordinate row existing at all; a reverted
      (deleted) claim re-includes it next cycle.
    - **The `NOT EXISTS` is the only mechanism; "extend the partial index" is
      infeasible** — a partial index on `review_recommendations` cannot reference
      another table (`recommendation_filed_issues`). The filed table's own unique
      coordinate index is what serves the `NOT EXISTS`.

    Decided now, not deferred, because it changes the backlog query — folding it
    into M1 avoids a second migration touching the same path. The engine stamps
    `addressed_by_run_id` at cycle time, so a row can be both filed and
    engine-addressed; the panel's display precedence is addressed-then-filed.

**Interactions (audit N6), for completeness:** the **notifications inbox** needs
nothing — the actor is the user themselves, and self-notifying is noise. **Run
deletion** cascades the review and its recommendations away, and (Decision 6) the
`recommendation_filed_issues` rows with them; the forge issue survives with no
back-link, consistent with the honesty stance elsewhere. **Hosted workers** are
untouched — this is an API + browser feature; no worker, claim, or agent code
changes. The **board cache** is handled by the Decision 9 upsert. **The `uzi`
CLI (`api/cmd/uzi/`, CLAUDE.md's "second consumer" check):** the judge-review
*read* is already CLI-reachable, so an agent driving the CLI can see
recommendations but not act on them. Filing is **out of scope for the CLI in this
PRD** (browser-only), stated so a future reader does not mistake the omission for
an oversight. Consequently the two endpoints do not both sit on `RequireUser`:
the draft **GET** mounts on `RequireUser` (session or admin-scoped CLI token,
CSRF-safe, mirroring the review read), and the file **POST** mounts on the
cookie+CSRF `RequireAuth` path behind `forgeLimiter.PerUserMiddleware`, mirroring
`ConfirmProposal` (`handler.go:652`) — pin this so the coder does not guess the
CSRF/CLI posture.

## Milestones

- [ ] **M1 — Schema + store**: migration (draft `00070` — the live head is
      `00069`, so `00067` from the original draft is already taken; renumber above
      the live head at merge per `CLAUDE.md`) creating `recommendation_filed_issues`
      keyed `(review_id, category, target)` with `filed_repo_id`,
      `filed_issue_iid`, `filed_issue_url`, `filed_by_user_id`, `filed_at`,
      `filing_since` (Decision 6/7) and the FK delete rules (review→CASCADE,
      repo/user→SET NULL); the same migration extends the `improve_uzi` backlog
      predicate to a **claimed-or-filed** `NOT EXISTS` (Decision 12); sqlc +
      queries for claim / revert / settle-by-id / read / sweep. A store integration
      test proves: the filed link survives a re-judge
      (`UpsertRunReviewWithRecommendations` delete-reinsert leaves the link table
      untouched), a claim on an already-claimed coordinate is rejected, and
      **duplicate `(category, target)` coordinates collapse to one claim** (the
      accepted `target=''` collapse, Decision 6 — assert one issue per coordinate,
      not fan-out).
- [ ] **M2 — Draft templating**: a pure, unit-tested renderer (recommendation +
      review + judged run → title, breakout-proof-fenced-and-stripped body,
      default repo, provenance line) covering the category→repo map (Decision 4)
      and its empty-picker cases, the breakout-proof code-fence + `/`-strip of
      untrusted fields, and the secret-shape scan (Decision 10); `GET
      .../issue-draft` on `RequireUser` (Interactions). **Non-authoritative for
      security:** this renderer produces the draft only; the load-bearing controls
      re-run at M3 (below). Its own handler file (e.g.
      `handler/review_issue_draft.go`) so it does not collide with M3 in the
      router block.
- [ ] **M3 — Filing endpoint**: `POST .../issue` on the cookie+CSRF `RequireAuth`
      path (Interactions) — caller-owns-repo via `GetRepoForUser`, server-side
      `[]string{PRD, PRDLESS}` labels, **claim-first** then `CreateIssue` then
      settle-by-id+cache in one tx (Decision 7/9), created-with-warning on tx
      failure **and on a swept-out claim (0 rows settled)**, 409 on an
      already-claimed coordinate, per-user forge limiter, PAT-redacted errors.
      **Re-applies the write-boundary sanitizer server-side on the POST body
      before `CreateIssue`** (breakout-proof fence semantics + `/`-strip + secret
      scan, Decision 10 — the client is never trusted to have preserved the
      draft's inertness). A stranded-`filing_since` sweeper on a bounded timeout
      **clamped `≥ 2×ForgeHTTPTimeout`** (Decision 7, `config.go` clamp
      precedent). Its own handler file (e.g. `handler/review_issue_file.go`); the
      single shared edit to the `handler.go` route block is expected and trivial.
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
      test **run against the POST handler, not just the renderer** — markdown
      link/image + `/label`/`/relabel` line + a **fence-breakout attempt** (a ```
      run inside `rationale_md`) → all inert in the filed body, asserting the
      breakout-proof fence and the `/`-strip hold on the client-supplied body;
      vitest for the panel states; an e2e leg that files from a stubbed review and
      asserts labels are `PRD`+`PRDLESS` and **no run is enqueued**.
- [ ] **M6 — Docs**: `docs/judge.md` gains the filing flow (label meaning, the
      `prdless_enabled` precondition, that nothing auto-starts, that the bot is
      the author, the cross-project egress note); `specs/ai.md` records the
      decisions.

**Dependency graph** (house convention): M1 → { M2, M3 } run in parallel (M2 is
a pure renderer + a read endpoint; M3 is the write path — they share the M1
schema and a **one-line edit each to the `handler.go` route block plus response
DTOs**, so each ships its own `handler/review_issue_*.go` file and the router
edit is expected to merge trivially — they are otherwise independent) → M4
(consumes both endpoints) → { M5, M6 } in parallel. Single repo, so no cross-repo
phase.

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
  image beacon, or executed quick-action) for a human GitLab viewer — including
  when the untrusted text contains its own backtick run (breakout-proof fence),
  and enforced on the **client-supplied POST body**, not merely the draft.

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
  and cross-project secret leakage are new and mitigated here (breakout-proof
  code-fence + `/`-strip **re-applied server-side at the POST boundary**,
  best-effort secret scan, and the human gate as the primary control). The naïve
  fence was defeated by fence-breakout on review (backticks survive ingest), and
  the controls were originally placed on the draft-render path rather than the
  write boundary — both corrected in Decision 10. Reviewers should keep attacking
  this framing specifically.
- **Confused-deputy on the admin path** (Decision 8), accepted by user decision,
  mitigated by prominent provenance rather than removed.

## Resolved Questions

- **Draft shows raw markdown, not a rendered preview.** The editable textarea is
  honest about what gets written and needs no renderer; a preview would mean
  running `Markdown.tsx` over untrusted text, which `RunView.tsx:706-710`
  refuses. (Decision 10 makes the *filed* body inert independently.)
- **A filed `improve_uzi` recommendation is excluded from the self-improvement
  backlog** (Decision 12): `ListOpenImproveUziRecommendations`
  (`selfimprove/engine.go:225`, predicate `queries/selfimprove.sql:38`) gains a
  **claimed-or-filed** `NOT EXISTS` so a hand-filed (or mid-filing) issue and the
  self-improve tracking issue do not cover the same recommendation twice. Decided
  pre-M1 because it changes the backlog query in the same migration; the `NOT
  EXISTS` (not a partial-index extension, which cannot cross tables) is the
  mechanism. A recommendation can be both filed and engine-addressed; display
  precedence is addressed-then-filed.
