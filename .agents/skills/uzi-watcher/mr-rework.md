# uzi may fix the CodeRabbit findings ITSELF (mr_rework) — coordinate, don't collide

This is the full runbook split out of `SKILL.md` (which carries a short pointer stub under
the same heading). Paths like `scripts/...` and `resume-recipe.md` are relative to this
skill directory (`.agents/skills/uzi-watcher/`), unchanged by the split.

**Before you fix a finding locally or merge, check whether uzi is already reworking the
MR.** The **MR review-watcher** (`mr_rework`, `docs/mr-review-watcher.md`) is **on by
default** for every opted-in user (opt-in is itself the default), with a per-user opt-out
and an admin `mr_rework_enabled` kill-switch. On the same poll tick that watches MRs, uzi
checks every open MR of one of your **completed issue runs** and — when the head pipeline
is **green**, the review has **settled** (newest comment a few minutes old, written against
the current head), there is **≥1 review comment it has not acted on**, and the MR is under
its per-MR rework cap — queues an **auto-approved `mr_rework` run**. That run reads the
review comments (**CodeRabbit's included**; human reviewers too; uzi's own status notes
filtered out), reworks the branch **in place** on the same `agent/issue-*` branch, replies
in-thread and resolves threads, and **pushes a fix commit**. It **never merges** (the four
guardrail layers still hold), so the merge stays THIS session's job.

The hazard is a **double-fix collision**: if this session also amends the same branch, the
two pushes race and conflict, and merging under an in-flight rework throws its work away
(or fails it against a closed MR). **Measured 2026-08-29** on PR #792 (issue #676): an
`mr_rework` run (`3374fbf4`, `mr_iid:792`) fixed a CodeRabbit test-scoping finding in place
while this session was about to merge — caught only by listing `kind=mr_rework` runs, not
by anything on the PR itself.

**The coordination rule, folded into the flow:**

1. **When ANY review finding lands, check for an `mr_rework` run on this MR before touching
   anything.** `mr_rework` consumes every unacted review comment — CodeRabbit, a **human
   reviewer**, and any **third-party review bot** — not just CodeRabbit, so gate the check
   on *any* finding, or a human/other-bot finding slips into the local-fix path while a
   rework is being queued and recreates the double-push collision:
   ```sh
   uzi run list --json | jq -r --arg repo REPO_ID --argjson pr PR \
     '.[]|select(.kind=="mr_rework" and .repo_id==$repo and .mr_iid==$pr)|{id,status}'
   ```
   **Filter on `repo_id` too, not `mr_iid` alone:** `mr_iid` is a per-repo MR number, so
   across the several repos this skill may drive, two repos can each have an MR with the
   same number — an `mr_iid`-only match can point at another repo's rework. `mr_rework`
   runs carry **`repo_id` and `mr_iid`, never `issue_iid`** (their `branch`/`mr_web_url`/
   `source_run_id` may read null while running — do not key on those). A non-terminal one
   means uzi is on it.

   **Handing a finding to `mr_rework` yourself: post it as a TOP-LEVEL PR comment, never an
   inline one.** The trigger keys on ONE scalar high-water (`mr_rework_ledger.high_water`)
   over GitHub's DISJOINT comment id sequences (top-level/issue vs inline-review vs
   review-summary, `github_mr.go` Sources A/B/C). An inline finding is silently skipped when
   a prior rework already consumed a higher-id top-level/issue comment (classically your own
   `@coderabbitai review` nudges): its id sits below the mark, so GATE 3 in
   `mr_review_watch.go` never fires and no run appears though the finding is the newest,
   actionable comment. A fresh TOP-LEVEL PR comment lands above the mark and fires it.
   Fail-safe (a skipped comment just falls back to human review), so the tell is silence, not
   an error. Durable per-sequence-high-water fix tracked in #1199.
2. **If uzi is (or is about to be) reworking, DEFER — do not fix locally, do not merge.**
   The trigger needs a green pipeline + settled review, so the run may not have spawned yet
   even though it will; if the findings are uzi-fixable (below) and the owner is opted in,
   give it a beat and re-check rather than racing in.

   **How long is "a beat"? Longer than the trigger wording implies — mr_rework firing LAGS
   the settled review badly, and the measured lag is what to plan around.** The trigger
   reads as "the next poll tick after the review settles," but on a busy instance the run
   has been observed firing **30-40+ minutes** after CodeRabbit's review landed and CI went
   green (measured 2026-08-30: findings on PRs #847/#848 settled ~14:10, the `mr_rework`
   runs were created ~14:52-14:55, roughly 40 min later), while on a quiet instance it fired
   in ~4 min (#843, same day). So a 20-minute "it hasn't fired, I'll just fix it myself"
   conclusion is **premature**: you do the whole local fix and mr_rework then fires on top of
   it, duplicating the work (not a data-loss collision if your pre-push re-check is clean,
   but wasted effort and a confusing double set of fix commits). **Budget at least ~40 min
   of polling for the run to APPEAR before falling back to a local fix, and poll for it
   rather than eyeballing** — `scripts/wait-mrrework.sh OWNER/REPO PR [max] [int] [since_utc]`
   waits through the fire→terminal lifecycle and exits when the run lands (or the budget
   elapses). **Capture `since_utc` BEFORE the wait and pass it**, so the poller anchors to the
   CURRENT rework cycle and cannot report a PRIOR cycle's already-terminal run as "done" (the
   exact failure it exists to prevent, one cycle down): `SINCE=$(date -u +%Y-%m-%dT%H:%M:%SZ)`
   the moment you first see the findings, then `wait-mrrework.sh OWNER/REPO PR 45 60 "$SINCE"`.
   Reuse the SAME `$SINCE` on every re-run — a run for any later comment is still created after
   it, so one baseline catches every cycle for these findings.

   **That ~40-min budget is measured from the NEWEST review comment, so it RESETS every time
   a new one lands — it is not a fixed timer off the first finding.** mr_rework's quietPeriod
   debounce runs from the latest review comment on the MR, so a later CodeRabbit incremental,
   a human note, or another bot posting mid-wait pushes the fire window forward by another
   full debounce. A fixed "~40 min from when I first saw findings" countdown can therefore
   expire while the run is still legitimately pending against a newer comment. So restart the
   budget whenever a new review comment arrives, and conclude "it will not fire" only once a
   full quiet period has elapsed with NO new review comment AND no run has appeared (re-running
   `wait-mrrework.sh` after any new comment does exactly this).

   Fall back to a local fix **only on `wait-mrrework.sh` exit 2** — a CONFIRMED empty result
   (no current-cycle run appeared across reliable polls) — or when mr_rework structurally
   cannot help (owner opted out, the admin kill-switch is on, or a `.github/workflows` finding
   — the "when this session still fixes locally" list below). **Its other exits do NOT
   authorize a local fix:** exit 3 means a run is STILL reworking (re-run to keep waiting),
   exit 5 means the polls were unreliable so "never fired" is unproven, and exit 4 means the
   repo did not resolve — treating any of these as "budget spent, fix locally" re-opens the
   double-push collision. **Either way**, the
   pre-push guard (re-list `kind=mr_rework` for this MR AND re-fetch the branch head, step 1)
   stays mandatory right before any local push, because the run can fire during your edit.
3. **Let it finish, then decide from whether the head ACTUALLY moved — and read the ref
   authoritatively at BOTH ends.** With `BR` the PR's `agent/issue-*` branch, **`git fetch
   origin "$BR"` before recording** the pre-rework head (`before=$(git rev-parse
   origin/"$BR")`) — the local remote-tracking ref can already be stale, so an unfetched
   `before` is as unreliable as an unfetched `after`. Then, after the run reaches terminal,
   **`git fetch origin "$BR"` AGAIN** and re-read the head: a rework-worker push does not
   update your local remote-tracking ref on its own, so without the second fetch a real push
   reads as "no commit". Then compare `before` to the new head:
   - **Head advanced** → REVIEW the new commit like any other diff (`git show <sha>`). uzi
     *acting* is not uzi being *right*: confirm it addressed the finding, added no
     regression, and did not "fix" a deliberate behavior. A bad rework is a
     `revise`/`follow-up` to that run or a local correction — never an automatic merge. Then
     go to step 4 (its push retriggered CodeRabbit).
   - **Head unchanged** → the rework pushed **no commit** (it judged every finding invalid
     or already handled and only replied/resolved threads). There is no new head, so **no
     re-review was triggered — do NOT wait for one** (step 4 does not apply). Instead read
     the run's own outcome (its in-thread replies / `uzi run logs`) and confirm every
     finding was *explicitly* skipped or resolved-without-code; if so, proceed to merge on
     the existing green state, otherwise return it to triage or local handling.
4. **When it pushed a commit, that push retriggers CodeRabbit.** Wait for the re-review on
   the **new head** (signal (c), the walkthrough `recent_review` range covering the new SHA
   — see the *Triaging CodeRabbit findings* runbook in `coderabbit-triage.md`
   (this skill dir)),
   confirm no active `mr_rework` remains and CI is green on that head, THEN merge.

   **`scripts/watch-pr.sh OWNER/REPO PR [interval] [max]` runs this whole readiness poll**
   so you do not hand-roll it each time: it exits **0** merge-ready (CI green on the head,
   CodeRabbit reviewed that exact head with zero live inline findings, no active
   `mr_rework`), **1** on red CI, **3** when CodeRabbit reviewed the head but left live
   findings to triage, **4** when an `mr_rework` run is active on the MR (defer, then re-run
   it), and **2** on timeout — where **exit 0 is trustworthy but exit 2 means inspect
   manually, never merge**. "Reviewed this head" is the union of a review whose `commit_id`
   is the head SHA and the walkthrough range ending at it, because a zero-actionable
   incremental posts no new review object.

**When this session still fixes locally (mr_rework will not or cannot):**
- the owner is **opted out**, or the admin **kill-switch** is engaged
  (`mr_rework_enabled=false`), so no rework fires;
- a **`.github/workflows` finding** — the worker lacks `workflow` scope, so uzi cannot
  touch it (nor can a re-run); that is yours, on a CI-only PR;
- a **base-realignment inherited finding** (a workflow file already on `main`) — not the
  PR's to fix at all;
- `mr_rework` **failed or hit its per-MR cap** with findings still open. Its pre-0.68.0
  failure signature is `failure_reason: "issue run claim is missing issue_iid"` (the #784
  branch-derivation bug, fixed in 0.68.0); on an older server every rework fails this way
  and the old self-fix flow is the only path.

This is the **first** fork of *Reviewing the diff* (the section in `SKILL.md`) and
*Triaging CodeRabbit findings* (the runbook in `coderabbit-triage.md`, this skill dir):
read those for how to verify and label a finding, but decide **who fixes** it here first.

