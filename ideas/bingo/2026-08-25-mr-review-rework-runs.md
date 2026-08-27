# MR review feedback → rework run: let the worker read the review it is being asked to address

Line references are against ae8e54b9.

## Rationale

A completed issue run parks its card in Human Review with an open MR, and then the
review happens **on the MR** — human comments, and on this repo a bot review on every
PR. uzi can read none of it. The forge seam's MR read is state-only: `type MergeRequest
struct { IID int64; State string; WebURL string }` (`api/internal/forge/forge.go:273-277`),
reached through the single method `GetMergeRequest(ctx context.Context, projectID, mrIID
int64) (MergeRequest, error)` (`forge.go:437`). The worker's own tool says so in its
description — `"Read one forge merge request by its number (iid): its state."`
(`agent/src/forge-tools.ts:136`), served by `WorkerForgeGetMergeRequest`
(`api/internal/handler/worker_forge.go:359`, mounted at
`api/internal/handler/handler.go:1520`). There is no `ListMergeRequestComments` anywhere
in the interface.

The asymmetry is deliberate and recorded: PRD #381 built the whole untrusted-comment
pipeline for **issues** — `ListIssueComments` (`forge.go:428`), the best-effort snapshot
`fetchIssueCommentsSnapshot` (`api/internal/workersvc/service.go:4677`, which calls
`buildIssueCommentsSnapshot(comments, row.BotForgeUserID)` at `:4688`), the
`runs.issue_comments jsonb` column (`api/internal/store/migrations/00133_run_issue_comments.sql:4`,
written by the `CreateRun` INSERT at `api/internal/store/queries/runtime.sql:373`), the
claim field ``IssueComments *IssueCommentsSnapshot `json:"issue_comments,omitempty"` ``
(`api/internal/workersvc/claim.go:36`), and the nonce fence that renders it
(`agent/src/prompt.ts:385`: ``const openTag = `<issue_comments_${nonce}>`;``, consumed at
`:781`). Its non-goals list then names exactly this gap:
"**Reading MR/PR comments or CI-comment threads** — this PRD is issue comments feeding an
issue-backed run." (`prds/done/381-worker-reads-issue-comments.md:87`).

So today the only automated reaction to a review is the close edge — `case stored ==
forge.MRStateOpened && observed == forge.MRStateClosed:` (`api/internal/forgesvc/mr_watch.go:94`),
documented as "Closing a completed run's merge request without merging it is treated as
'rework needed'" (`docs/board.md:194`). That moves the card and tells the next run
*nothing about why*: the human must close the MR (burying the review threads), then
hand-copy the findings into an issue comment, because `uzi run follow-up`
(`api/cmd/uzi/run.go:593`) only reaches a live run — a completed one cannot be steered.
That copy-paste step is the same one PRD #381 removed for issues, still standing for MRs,
and it sits on the factory's slowest path (the human review loop).

The cheap part is that **no new machinery is needed to act on the feedback**: successive
runs on an issue already share the branch — "If the branch already exists at origin (a
resume, or a prior run on the same issue … successive runs build on prior work"
(`agent/src/git.ts:355-357`), `createOrAttachRunnerClone` →
``this.runnerCloneForBranch(barePath, `agent/issue-${issueIid}`, …)`` (`git.ts:359-360`) —
and the worker's `createMr` folds a duplicate onto the existing MR. Note the reuse is
conditional on `agent/issue-<iid>` still existing at origin (else the clone starts from
the default branch); in the rework case the MR was closed *without* merging, where the
branch normally survives — the likely path, not a guaranteed one. A rework run is a
normal `issue` run with a different *instruction*, so it is plan-gated like every other
run and needs no new `runs.kind`, no claim-wire kind branch, and no worker state-machine
change.

## Sketch

- **Forge seam (the bulk of the work).** Add
  `ListMergeRequestComments(ctx, projectID, mrIID int64) ([]MRComment, error)` to
  `api/internal/forge/forge.go`, with a neutral `MRComment` modeled on `IssueComment`
  (`forge.go:252-257`) plus the two fields review threads need: `Path`/`Line` (nil for a
  top-level note) and `ReviewState` (a review-summary body vs an inline note). Three
  drivers (`gitlab.go`, `github.go`, `forgejo.go`); GitHub is the unequal one — top-level
  notes, inline review comments, and review bodies are three separate endpoints. Per
  #381's D2/D8, filter forge system notes driver-side and normalize to oldest-first.
  Verify each SDK signature against the Go module cache the way #381's "Resolved facts"
  section did — this idea asserts none of them.
- **Snapshot + carry, as a sibling of #381, not a fork of it.** `fetchReviewCommentsSnapshot`
  beside `fetchIssueCommentsSnapshot` (`workersvc/service.go:4677`), reusing the same
  caps and fail-safes from `api/internal/workersvc/issue_comments.go`
  (`maxIssueCommentsBytes = 32768` at `:12`, the D1 bot self-filter and D9
  unknown-bot-id bail at `:44`); a nullable `runs.review_comments jsonb` column
  (migration number assigned at merge — live head is
  `api/internal/store/migrations/00163_run_status_since.sql`); one more nullable field
  on the `CreateRun` INSERT (`runtime.sql:373`) and on `ClaimPayload` (`claim.go:36`).
  Best-effort context, never a reason to fail creation, exactly as the doc comment at
  `service.go:4674-4676` states.
- **Prompt.** A `buildReviewCommentsContext` beside `buildIssueCommentsContext`
  (`prompt.ts:781`) using `fenceNonce()` (defined `prompt.ts:1355`, called at `:384`) —
  a `<review_comments_{nonce}>` block whose per-entry `author` / `path:line` labels are
  uzi's and whose bodies are data — plus a short rework framing note in the issue-run
  lifecycle slot (`prompt.ts:111-123`): the branch and MR already exist, address the
  feedback, do not re-run the approved milestones.
- **Trigger: explicit, human, one click.** `POST /api/runs/{id}/rework` on a *completed*
  run with a non-NULL `mr_iid`, creating a new issue run on the same issue via the
  existing `s.CreateRun(...)` seam (`api/internal/workersvc/composite.go:218`). Mount it
  in the **`RequireUser`** group beside `r.Post("/{id}/dispatch", …)`
  (`handler.go:1318`) and `r.Patch("/{id}/priority", …)` (`:1357`) so a CLI token reaches
  it — see the open question below. Dedup is free: `uq_runs_one_active_per_issue … WHERE
  kind = 'issue' AND status NOT IN ('completed','failed','cancelled')`
  (`migrations/00043_ci_fix_runs.sql:27-29`) makes a second rework a 23505 → 409.
- **CLI:** `uzi run rework <run-id>` in `api/cmd/uzi/run.go` (the verb list at `:205-752`),
  plus `api/internal/uzicli/client.go`, the `FakeClient`, `docs/cli.md`, and the embedded
  `api/internal/uzicli/skill/SKILL.md` the command-tree drift test pins.
- **Web:** an "Address review" action on a completed run's card/detail
  (`web/src/pages/IssueView.tsx`, `web/src/pages/RunView.tsx`) shown only when the run is
  completed with an `mr_web_url` (`web/src/lib/api.ts:529`).
- **Mock parity is a contract, not a convenience.** `web/src/mocks/mockApi.ts` gets a
  second implementation of the rework precondition (completed + `mr_iid` present), so it
  needs a differential fixture, not a snapshot of the demo data: one case per branch —
  completed-with-MR (action offered), completed-without-MR (hidden), non-terminal
  (hidden), plus an assertion that the fixture actually contains all three, so the test
  cannot pass by covering only the happy row.
- **Optional last milestone, easy to cut:** a review-comment *count* on the Human Review
  card. `SyncMRStates` (invoked from the poller tick at `api/internal/poller/poller.go:352`)
  already does one `GetMergeRequest` per watched candidate per tick — the candidate loop
  at `api/internal/forgesvc/mr_watch.go:29-40`, the fetch at `:60` — over a deliberately
  tiny Lane-A set (`ListMRWatchCandidates`, `api/internal/store/queries/forge.sql:456`),
  so a count read rides that loop — at the cost of doubling its forge calls.
- **Docs:** extend the rework paragraph at `docs/board.md:194` (close-the-MR is now the
  coarse path; rework-with-feedback is the fine one) and `docs/cli.md`.

## Caveats / out of scope

- **This is the worst prompt-injection input uzi ingests, and it must be named as such.**
  MR review bodies are multi-author attacker-influenceable text, and a review bot's
  comment can literally contain an agent-addressed imperative block. The nonce fence
  (`prompt.ts:1355`) and #381's D5 rationale are the mitigation, and a breakout test is
  mandatory. Note the deliberate asymmetry: the bot filter keys on the connection's own
  `bot_forge_user_id`, so uzi's own notes are dropped while a *third-party* review bot
  stays readable — that is the point of the feature, not an oversight.
- **Open question for the PRD (needs the maintainer's call): which auth group.** `rejudge`
  is cookie-only precisely because it "MINTS a token-spending judge run"
  (`handler.go:1297-1298`, `:1365`), and a rework run also spends. But a cookie-only mount
  makes the CLI verb 401 on a `uzc_` Bearer. Recommendation: `RequireUser` like
  `/dispatch`, whose comment records the same "no token spend, no forge write" reasoning
  it does *not* have (`handler.go:1315-1318`) — settle it explicitly rather than by
  copy-paste.
- **Out of scope: auto-firing a rework when new comments appear.** PRD #6's
  inspiration-check table already records the rejection of the generic "event → run with
  no human gate" shape (`prds/done/6-ci-status-integration.md:25`); if it is ever wanted,
  the template is the `ci_autofix` loop-guard ledger (`api/internal/poller/ci_autofix.go:26-31`,
  `maxAttempts` at `:66`) plus a consumed-comment high-water mark, not a level trigger.
- **Also out of scope:** writing back to the forge (resolving threads, replying,
  approving), auto-merge, MR mergeability/conflict state (still absent from
  `MergeRequest` — a separate idea), and feeding MR comments to `chat`/`judge`/
  `self_improve` runs, which have no MR thread of their own.
- **Interface-change spread is the known cost**: three drivers plus the forge fakes #381
  counted at six (including `poller/ci_autofix_test.go`'s `cfForge`). Mechanical, but it
  is a milestone, not a footnote.
- The rework run re-plans and re-gates, which is correct but not free: on a
  three-line-nit review the plan turn costs more than the fix. Accepted; a "seeded plan"
  shortcut (`prds/done/209-seeded-plan-runs.md`) is the natural follow-on if that bites.

## Dedup checks performed

Ruled out as already-shipped or already-recorded: run queue priority
(`ideas/bingo/2026-08-11-run-queue-priority.md` — since shipped,
`prds/done/320-run-queue-priority.md`, verb live at `api/cmd/uzi/run.go:752`); worker
pause/quiesce (`ideas/bingo/2026-08-18-worker-pause-quiesce.md`, plus the adjacent
`prds/done/496-worker-cordon-pill.md`); issue-comment ingestion
(`prds/done/381-worker-reads-issue-comments.md`, which names MR comments out of scope);
MR-close rework (`prds/done/24-mr-close-rework.md`, `forgesvc/mr_watch.go`);
merged-state recording (`prds/done/527-mr-merged-state-recording.md`); CI autofix
(`prds/done/71-ci-autofix.md`, `poller/ci_autofix.go`); worker forge read tools
(`prds/done/158-forge-read-tools.md` — no comment tool); mid-run steering and scope
control (`adr/0634-run-scope-steering.md`, `prds/done/517-interactive-task-runs.md`,
`prds/done/322-chat-slack-run-control.md` — all live-run only); judge/self-improve
(`prds/done/46-run-judge-self-improvement.md`, `prds/done/590-selfimprove-as-default-job.md`);
finalize-time rebase (`adr/0456-rebase-before-finalize-push.md`, which covers the push,
not the post-MR review loop). Grepped `prds/`, `prds/done/`, `adr/`, `docs/`, `specs/` for
`review comment|mr comment|mr note|discussion thread|unresolved thread|rework|address
review` — the only hits are #381's out-of-scope bullet and the PRD #24 close-edge prose,
both cited above.
