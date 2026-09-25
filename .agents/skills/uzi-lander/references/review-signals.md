# Review-bot signals: what is pollable, per bot, per head

Everything here is measured on this repo (PRs #1421, #1426, #1428, 2026-09-16/17, plus
the earlier CodeRabbit measurements cited in `coderabbit-triage.md`). `scripts/watch-pr.sh`,
`scripts/pr-findings.sh`, `scripts/takeover.sh` and `scripts/cr-rate-limit.sh` implement
these; read this when you must hand-roll a check or extend a script.

## CodeRabbit (auto on eligible PRs against `main`; `.coderabbit.yaml`)

| Surface | Endpoint | What it tells you |
|---|---|---|
| Commit status, context `CodeRabbit` | `repos/O/R/commits/<head>/status` → `.statuses[]` | The **only place CR says why it did not review**: `Review in progress`, `Review completed`, `Review rate limited`, `Review skipped: reviews are disabled for this base branch` / `147 files exceed the limit of 100` / `ignored keyword in the PR title`. `state` is `success` even when rate-limited: read the description. Absent = CR never touched this head. |
| Review objects | `repos/O/R/pulls/N/reviews` | Signal (a): a `coderabbitai[bot]` review with `commit_id == head` proves a review of this head. The tally `Actionable comments posted: N` lives in the review body (a clean pass is an APPROVED review with an empty body; a COMMENTED tally-less review is *unconfirmed*, read its body). |
| Walkthrough comment | `repos/O/R/issues/N/comments`, exactly one `coderabbitai[bot]` comment containing `<!-- walkthrough_start -->` | Signal (c): the `final_review_risk` block's ``up to `<short-sha>` `` names the last reviewed head; a zero-finding incremental posts no review object, so this is the signal that always fires. Parse only inside the block, fail closed on 0 or >1 comments. **Signal (e):** on some PRs CodeRabbit drops `final_review_risk` entirely (0 occurrences on #1502, 2026-09-21) and names the assessed commit in a machine-readable HTML comment `<!-- change_assessment_commit:"<full-40-char-sha>" -->`, beside a `recent_review_start … recent_review_end` block reading "No actionable comments were generated in the recent review" when clean. Match the FULL head in quotes; a mid-review/post-push walkthrough still names the OLDER commit, so an exact-head match cannot forge a ready. `watch-pr.sh`/`pr-findings.sh` treat (c) OR (e) as the reviewed-head walkthrough signal. The same comment carries the **rate-limit block** (`rate limited by coderabbit.ai` … `Next included review available in N minute(s)`), relative to the comment's `updated_at`. |
| Command replies | `repos/O/R/issues/N/comments`, newest bot reply after the matching user command | A normal `@coderabbitai review` can terminate with “Already reviewed the last commit. Use `@coderabbitai full review`” while status stays stale `Review completed`. This is actionable, not pending: `watch-pr.sh` exits 7 unless a later full-review command already exists. |
| Review threads | GraphQL `pullRequest.reviewThreads` | CodeRabbit finding liveness: count only threads with `isResolved == false`, `isOutdated == false`, and a CodeRabbit comment. REST `line != null` survives human thread resolution and is not authoritative. Refuse truncated thread/comment pagination. |
| Inline comments | `repos/O/R/pulls/N/comments` | Finding bodies and anchors for display; not CodeRabbit resolution state. |
| Equivalent head | two `compare` calls | Signal (d): a logic-free merge-in of the base plus regenerated artifacts needs no fresh review (#819); `watch-pr.sh` computes it fail-closed. |
| Check-run `CodeRabbit` in `gh pr checks` | | Do not key on it: it reads `pass` for an earlier commit while the new head is unreviewed (PR #756). |

**Rate limit, the three places the reset appears:** the walkthrough block above (free);
the reply to the exact two-word `@coderabbitai rate limit` (`More reviews will be available
in N minute(s)`, relative to the reply's `created_at`, or `Reviews are available now` for an
immediate reset); nowhere on a bare `@coderabbitai review` while limited (it replies `Review
rate limited`, no time). When timing matters, the exact query is authoritative: never infer
from a review timestamp or nominal hourly rate.
`@coderabbitai ratelimits` and plain English get "I cannot view the quota". Refill is adaptive.
Renovate-authored PRs and `*(deps)` / `[skip-cr]` titles are not auto-reviewed and consume
nothing; every push to any other open PR is one review, including uzi's `mr_rework` pushes.

## Greptile (on demand only; `greptile.json` has `autoReview: []`)

Trigger: a `@greptileai review` comment (also `@greptile review`), or the `Retrigger` link
Greptile puts in the PR body. One credit per review. The check-run appears ~12 s later.
Only the COMMENT is visible to the scripts: the `Retrigger` link posts none, so a review
started that way is not seen until its check-run exists.

| Surface | Endpoint | What it tells you |
|---|---|---|
| Check-run `Greptile Review`, app `greptile-apps` | `repos/O/R/commits/<head>/check-runs` | **The per-head signal**, unless a push raced the trigger: then the run sits on the OLDER commit while Greptile reviewed the head (see PR body edits). `in_progress` while reviewing; `completed` with `output.summary` = `Greptile has reviewed the Pull Request.\n\nN files reviewed, M comments added`. Conclusion is `success` even with a P1 finding: read M. Absent = not triggered on this head. The listing is newest first and a re-trigger adds a second run on the same commit: take the max `id`, never the last. The live summary ends with a period. Durations seen: 35 files 2.5 min, 107 files 6 min. |
| Review object | `pulls/N/reviews` | Only when the pass adds INLINE comments (M minus its outside-diff bullets > 0): a `greptile-apps[bot]` COMMENTED review, empty body, `commit_id` = reviewed head. **Zero findings posts no review at all**, so the reviews endpoint reads clean-as-absent. |
| Inline comments | `pulls/N/comments` | Findings carry a `<img alt="P1">` / `P2` badge. Scope them to the latest current-head review id; when the current-head check reports `M > 0`, wait until M scoped comments are readable. An explicit `0 comments added` makes older still-anchored comments non-live. A push re-anchors every older comment onto the new head. When the head carries NO Greptile evidence (no check-run in any state, no review object), scope them to the newest EARLIER verdict within the last 20 PR commits (`scripts/lib/greptile-verdict.sh`; clean → none live, else that review id only; none found → all anchored stay live). A Greptile run still queued or in progress on a newer commit than that verdict defers instead, and so does a `@greptileai review` comment newer than it (the check-run takes ~12 s to appear) for up to 10 min. A head listing that could not be READ is not "no evidence": it keeps every anchored comment live. Liveness only, never the head-reviewed gate. Premise: each pass reviews the whole PR diff, not the delta (both passes on one PR reported the PR's full changed-file count). |
| PR body edits | GraphQL `pullRequest.userContentEdits{editedAt editor{login} diff}` | Greptile rewrites the description between `<!-- greptile_comment -->` markers (`Confidence Score`, a summary, `<sub>Reviews (K) · Last reviewed commit: [...](…/commit/<full-sha>)</sub>`). Not on every PR. The live `pulls/N` `.body` is user-editable: never a verdict. The edit history is authenticated: `editor.login` `greptile-apps`, and `diff` has been observed to hold the full body snapshot (documented only as a change summary, so anything else reads as no marker). With no completed review run on the head, `greptile_paired_verdict` (`scripts/lib/greptile-verdict.sh`) takes the newest Greptile edit by `editedAt` (else a head review object's `submitted_at`) naming the head (full 40 hex, inside the block), binds the ONE completed `Greptile Review` run on a PR commit finishing within 120 s after it (measured +3 to +6 s) and not before the head's committer date, and requires that run to have started after the newest non-Bot `@greptile(ai) review` comment. A newer trigger = pending; no trigger, no Greptile edit or no run = not reviewed; two runs in the window, a further edit page, or an unreadable response = unknown. It overrides a stale `in_progress` duplicate on the head. The bound run's M is the tally. |
| Issue comments | `issues/N/comments` | Findings on lines the diff does not cover: ONE `greptile-apps[bot]` comment marked `<!-- greptile_outside_diff -->`, one `- ` bullet per finding (`<img alt="P1">`, `**title**`, `` `path:line` ``, a `/blob/<sha>/` link), edited in place; a bullet leaves once its file changes. M counts these bullets, so a pass whose findings are ALL outside the diff posts no review object. Scripts read it via `greptile_outside_diff` (`scripts/lib/greptile-verdict.sh`): every bullet is live (cleared by an explicit `0 comments added`), and bullets linking the head are subtracted from M before scoping inline comments (PR #1671, 2026-09-25). |

## Poll recipe (what the scripts do)

1. Head SHA from `gh pr view --json headRefOid`; every signal is tested against it.
2. CR: status description → pending / limited / skipped / absent; reviewed = (a) or (c) or (e) or (d); command reply can require the agent to decide on one full review.
3. Greptile: check-run on head → absent / in_progress / completed(+M); else the run bound to Greptile's edit or head review → completed(+M), or pending on a newer trigger.
4. If a COUNTED bot is active, defer finding output; the set is incomplete even when the other bot already satisfies the gate. A bot an explicit `--reviewer coderabbit|greptile` ignores is NOT waited on.
5. Live findings = the COUNTED bots' live findings. `--reviewer any`/`none` count both; an explicit `--reviewer coderabbit|greptile` counts only the selected bot (the other bot's in-flight state and findings are both ignored). Greptile live = current-head review, else its newest earlier verdict. The poll log always prints the raw per-bot counts, counted or not.
6. Ready only when required CI is settled green, every COUNTED active review settled, the required reviewer(s) reviewed this exact head, counted live = 0, no `mr_rework` active, and the head re-reads unchanged (TOCTOU).
