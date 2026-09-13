# Triaging CodeRabbit findings (assess ALL first, then execute unattended)

This is the full runbook split out of `SKILL.md` (which carries a short pointer stub under
the same heading). Paths like `scripts/...` and `resume-recipe.md` are relative to this
skill directory (`.agents/skills/uzi-watcher/`), unchanged by the split.

When findings exist across one or more in-flight PRs, do NOT fix them piecemeal and do NOT
decide them yourself. **Gather every finding across every PR, verify each, present them as
one batch, collect the user's decision on each, THEN do all the work unattended.** One
decision gate, then hands-off — that is the whole point of batching, and it is the flow the
user asked for here (2026-08-24): "first we assess all, gather all answers, then work".

Present each finding with: PR, file:line, CodeRabbit's severity, your **real / inherited /
deliberate / mock-only** label, and a one-line recommendation. For each, the user picks one:

- **Fix locally** — amend the PR's own `agent/issue-*` branch with your own credentials (an
  isolated worktree; `git add` only your files), re-run CI, then merge. Keeps it in the one
  PR / review / CI cycle. The right default for small, localized findings — **but first
  confirm uzi's own `mr_rework` is not already fixing this MR** (see the *uzi may fix the
  CodeRabbit findings ITSELF* runbook in `mr-rework.md`, this skill dir); on a default-enabled
  instance it usually is, and a
  local amend then collides with its push. Defer to it, review its fix, and reserve the
  local amend for the cases it cannot handle.
- **Skip** — record the reason (deliberate behavior, a false positive, a base-realignment
  artifact, not worth it). A skip is a legitimate outcome, not a failure. **But a
  pre-existing / inherited finding that is a REAL bug is NOT a free skip:** fix it (in the PR
  when that keeps the diff clean, or a separate PR when the PR is pure-motion and an inline
  fix would break that contract), or **at minimum file it** as a tracked follow-up issue — do
  not skip a real bug just because it predates the diff. "Inherited, so not the PR's to fix"
  is true only for base-realignment artifacts (a workflow file already on `main`) and false
  positives; it does NOT cover a genuine defect the review happened to surface in code the PR
  merely moved or touched. Verify it against the current code first (a mock-only divergence
  where the real backend is correct is still a real finding; a CodeQL/analyzer alert on demo
  data with no actual sensitive value is a false positive). Instance (2026-09-02, PR #1011,
  a pure-motion mockApi split): three CodeRabbit "Major" findings were real mock↔backend
  divergences moved verbatim from the old file — filed as follow-up #1013 rather than skipped,
  keeping the split pure — while the HIGH CodeQL "clear-text storage" alert beside them was a
  true false positive (non-secret demo settings in `localStorage`), dismissed not fixed.
- **Send back to uzi** — as a follow-up **issue** (a full gated run → a *separate* PR to
  review/merge/CI-watch; right for a substantial change) or a **handoff task**
  (`uzi handoff`). Know the tradeoffs before recommending it: a handoff pushes to a throwaway
  `uzi/task/<id>` branch and so **cannot amend the PR under review**; an issue is a whole
  extra PR cycle; and a `.github/workflows` finding **cannot go to uzi at all** (worker lacks
  `workflow` scope). For tiny, well-localized findings a uzi round-trip is usually
  disproportionate — say so.

  **A filed issue meant for UNATTENDED pickup must be sweepable AND self-contained**, or the
  nightly sweep silently never fires it (the sweep table is in `CLAUDE.local.md`; the failure
  mode and the fix are the `issue-triage` skill's whole subject). Three requirements:
  1. **A sweep selector label** — `Planned` for feature/change work, `bug` for a defect.
  2. **Eligibility** — the `uzi` label, or the issue **assigned to the uzi-bot account**
     (a second, label-less way to be eligible, PRD #767); no PRD link, no PRD file, no
     waiver needed. A selector label alone does **not** fire an issue that is neither
     `uzi`-labelled nor bot-assigned. For an issue you're filing fresh here, the `uzi`
     label is simpler than assigning the bot account — assignment matters mainly for an
     issue a human already assigned to the bot before you got to it.
  3. **A cold-readable body** — a swept worker reads ONLY the issue text (no chat, no memory
     of this session), so name the exact files and `file:line` anchors, the precise change,
     and acceptance criteria (a failing-first test where sensible). CodeRabbit's own finding
     text is a good seed, but paste the concrete context — do not link to a PR comment the
     worker cannot see.

  **The inverse is just as deliberate:** an issue you intend a *human* to triage later must be
  left **non-sweepable** — no `uzi`, no `Planned`/`bug` — so an unattended 02:00 run cannot
  auto-implement a half-formed idea. Choose the labels for the outcome you want, every time.

A finding on a **workflow file inherited from `main`** (base-realignment artifact) is not the
PR's to fix at all — if it is worth doing, it is a separate CI-only PR (your token carries
`workflow` scope), correctly attributed to the change that introduced it.

**After you push a fix commit, CodeRabbit RE-REVIEWS the branch — wait for that re-review
before merging.** Every push to an `agent/issue-*` branch retriggers CodeRabbit's `auto_review`,
which posts an *incremental* review of just the new commits (async, a few minutes; the
`CodeRabbit` PR check flips to pending then back to "Review completed"). So the merge sequence
per fixed PR is: push fix → **wait for the incremental review of THAT commit to land** → confirm
it is clean or only acknowledgements → then merge. Do not merge a PR whose CodeRabbit re-review is
still pending after a fix push — the re-review can surface a defect in the fix itself, and merging
first defeats the point of fixing. A re-review that raises something new re-enters this same triage
flow (assess → decide → fix/skip), not an automatic merge.

**Do NOT treat the `CodeRabbit` PR check flipping back to "Review completed" (`gh pr checks PR`
showing CodeRabbit `pass`) as proof the re-review landed.** Measured 2026-08-28 on PR#756: the
check went green and every CI job was green while the latest CodeRabbit *review object* still only
covered the PREVIOUS commit — a poller keyed on the check reported "re-review done" when the
incremental review of the just-pushed fix had not posted. Confirm the re-review landed one of two
robust ways instead: (a) a new CodeRabbit review whose **Commits** range covers your latest SHA
(`gh api repos/OWNER/REPO/pulls/PR/reviews` — compare its `submitted_at`/range to your push time),
or (b) the specific findings you fixed now render **outdated** — their inline comments report
`line: null` with `original_line` set (`gh api repos/OWNER/REPO/pulls/PR/comments`), i.e. CodeRabbit
no longer anchors them to live code. And know that **CodeRabbit often does NOT post a fresh APPROVED
for a trivial fix**, so `reviewDecision` can stay `CHANGES_REQUESTED` even after every finding is
resolved; once (a) or (b) plus green CI confirm the current head is clean, `--admin` merges past
that stale verdict rather than waiting for a flip that never comes.

**(a) and (b) both fail on a CLEAN re-review, and that is the common case — so know signal
(c), the one that ALWAYS fires.** Measured 2026-08-28 on PR#763: after a fix push, a poller
keyed on (a) timed out and (b) never fired, because a **zero-actionable incremental review
posts NO new review object and re-anchors no prior finding** — CodeRabbit had finished and
found nothing, which is exactly the merge-ready state, yet both robust-looking checks above
stayed silent. The signal that fires on *every* incremental pass is the **walkthrough
comment's `recent_review` block**: CodeRabbit edits that one issue comment each pass to state
either the new actionable count or `No actionable comments were generated in the recent
review 🎉`, and — the load-bearing part — the exact range `Reviewing files that changed ...
between BASE_SHA and HEAD_SHA` (two full commit SHAs). Read it from the **issue** comments, not
the pulls comments — with `--paginate`, and select the ONE walkthrough comment CodeRabbit edits
in place by its stable marker rather than trusting the first CodeRabbit body you find. Two
reasons this must be deterministic: the issue-comments endpoint defaults to 30 per page (ascending
by id), so on a PR with more than 30 comments the walkthrough can fall off the default first page;
and CodeRabbit posts several issue comments (walkthrough, status, tips), so a bare login filter
returns more than one body. The `<!-- walkthrough_start -->` marker uniquely identifies it. Because
this is the signal an unattended merge keys on, match the **exact** bot login `coderabbitai[bot]`
(id `136622811`), not `test("coderabbit";"i")` which any login *containing* "coderabbit" would
satisfy — a spoofable author lets a crafted comment forge a clean range. Expect exactly one match
and fail closed on zero or more than one:
`gh api --paginate repos/OWNER/REPO/issues/PR/comments --jq '.[]|select(.user.login=="coderabbitai[bot]")|select(.body|contains("<!-- walkthrough_start -->"))|.body'`
and confirm the range's second SHA is your latest push. So the reliable order is: **(c) the
`recent_review` range covers your head SHA AND reports its actionable count** (0 → clean, merge
on green CI; >0 → triage); (a)/(b) are corroborating detail only when actionable comments
existed. A poller must key on (c), never on the `CodeRabbit` PR check nor on the presence of a
new review object.

**Two updates to signal (c) and the live-findings count, both measured 2026-08-29 driving a
6-PR batch, both now fixed in `scripts/watch-pr.sh`:**

- **The `recent_review` range is GONE; signal (c) now keys on the `final_review_risk` block.**
  The long treatment above describes the `recent_review` "between BASE and HEAD" range as the
  signal that "ALWAYS fires" — that format is **retired**: 0 occurrences across PRs #807 / #809 /
  #812 on 2026-08-29, all of which carry a `final_review_risk` block instead. That block reads
  `**Merge Risk:** _🟡 Moderate_ · up to` then the head short-sha in backticks (e.g. `up to
  280ac`), sits between `<!-- final_review_risk_start -->` / `<!-- final_review_risk_end -->`,
  and states a merge-readiness verdict in prose ("no actionable merge-blocking risk remaining;
  it is merge-ready" on a clean pass). `watch-pr.sh` confirms "reviewed this head" from that `up
  to` short-sha marker (**parsed only inside the `final_review_risk` block**, and only when
  **exactly one** walkthrough comment exists — fail closed on 0 or >1), plus signal (a), a review
  object whose `commit_id` is the head. It no longer parses `recent_review`, which is a retired
  format. Parsing the `final_review_risk` SHA over the whole body could false-match an unrelated
  "up to `<sha>`" phrase and, with zero live findings, forge a merge; the parser therefore scopes
  that SHA to its marker block and fails closed unless exactly one walkthrough comment exists (the
  "exactly one match, fail closed" contract above). A poller you hand-roll must key on `final_review_risk` (block-scoped)
  + signal (a); if `recent_review` ever returns, signal (a) still covers a real review, so the
  loss is fail-closed (a timeout, never a false ready).
- **An ADDRESSED finding keeps `line != null`; it is NOT outdated — do not count it as live.**
  The `line: null`/`original_line` "outdated" signal in (b) above is only ONE of the two ways a
  finding stops being live. When CodeRabbit judges a finding FIXED by a later commit it leaves
  the inline comment anchored to live code (`line != null`) and instead appends a
  `✅ Addressed in commit <sha>` line to the comment **body**. A live-findings count that filters
  only on `line != null` therefore counts a resolved finding as open: measured on PR #807, where
  two addressed findings produced a false "2 live findings" (`watch-pr.sh` exit 3) after a clean
  rework, stalling a merge-ready PR. Exclude any comment whose body `contains("Addressed in
  commit")` — the fix now in `watch-pr.sh`'s `live` count. This is also why, when you read
  findings by hand (e.g. via `pr-findings.sh`), a finding tagged `Addressed in commit` is done;
  verify against the body marker, not the line anchor.

**A third fix, 2026-08-30 (#819): signal (d), the "equivalent head".** A merge commit whose only
content is the merge-in of the PR base branch plus regenerated artifacts (e.g. resolving a
`*.sql.go` sqlc conflict by re-running `sqlc generate`) carries **no branch-authored logic change**,
so CodeRabbit posts no fresh review and moves its `final_review_risk` marker to no new SHA — signals
(a) and (c) both stay silent and a genuinely merge-ready PR times out (exit 2). `watch-pr.sh` now
recognizes this **fail-closed**: it treats the head as reviewed-equivalent to CodeRabbit's
last-reviewed commit `A` only when every path that changed between `A` and HEAD is either absent from
the PR's diff vs its base branch (so HEAD == base for that path — a pure merge-in the branch did not
author) or a regenerated/mirror artifact (`api/internal/store/*.sql.go`, `api/internal/uzidocs/embed/*.md`
— each scoped to the exact dir a generate/sync check covers, so a broader glob can't forgive an
unchecked branch-added file). It is computed
from two GitHub `compare` calls (no local git, so the script stays cwd-independent) and **refuses to
judge** when either compare's `files` list reaches the API's 300-file cap, since a truncated list could
hide an unreviewed path and forge equivalence. Any changed path that IS in the PR diff and is NOT such
an artifact is unreviewed branch work, so the signal does not fire → timeout, never a false "ready". A
poller you hand-roll should either key on (a)/(c) only (accepting the logic-free-merge timeout) or
reproduce this exact fail-closed equivalence — never relax the head-match, which is what forges a merge.

**The shared `main` worktree is a multi-writer tree — never assert it is clean.** Other
sessions leave modified files in it and advance `main` mid-review (measured 2026-08-20:
two reviewers found unrelated `tui_*` edits and `main` moving `6fc6c5eb`→`2007cbf4` under
them). This never blocks a merge — `gh pr merge` is a GitHub-side op that reads the PR
branch, not your local tree — but it means a local commit here (e.g. a docs/skill fix) must
`git add` **only** its own file(s), never `git add -A`, and you must leave the foreign dirty
files and any `wt-*` ghost worktrees alone.

