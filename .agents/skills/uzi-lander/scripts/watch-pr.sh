#!/usr/bin/env bash
#
# watch-pr.sh — poll a GitHub PR to merge-readiness for the uzi-lander flow.
#
# It combines the signals a merge decision here actually needs, so a session stops
# hand-rolling (and mis-writing) the same waiter each run:
#   1. required CI is settled and green on the PR's *current* head;
#   2. a reviewer bot (CodeRabbit and/or Greptile, per --reviewer) has reviewed that exact
#      head and left no live (unresolved) inline findings;
#   3. no in-flight uzi `mr_rework` run is reworking this MR (which would race a local
#      fix or a merge — see references/mr-rework.md); and
#   4. when no review can arrive on its own (CodeRabbit rate-limited, skipped or absent),
#      it says so with a distinct exit code instead of timing out, so the caller can
#      wait for the reset, trigger Greptile, or fall back to a local review.
#
# Usage: watch-pr.sh OWNER/REPO PR [interval_secs] [max_polls]
#                    [--reviewer any|coderabbit|greptile|none] [--reviewer-grace MIN]
#   interval_secs default 60, max_polls default 60.
#   --reviewer        which bot must have reviewed the head (default any: either one).
#                     `none` gates on CI + mr_rework only (the local-review fallback path;
#                     live findings from either bot still count).
#   --reviewer-grace  minutes to wait for an ABSENT reviewer before exit 6 (default 10,
#                     measured from the first poll). CodeRabbit's walkthrough normally
#                     lands a few minutes after the PR opens; Greptile's check-run appears
#                     ~12 s after `@greptileai review`. Pass 2 right after triggering.
#
# Exit codes (callers branch on these; keep them stable):
#   0  merge-ready — CI green on head, the required reviewer(s) reviewed this head with
#      0 live findings, no active mr_rework.
#   1  red — a required CI check failed on the head.
#   2  timeout — readiness not reached within max_polls, or the head never resolved.
#      NOTE: exit 0 is trustworthy; exit 2 means "inspect manually", never "merge".
#   3  findings — the head is reviewed but live inline findings remain to triage
#      (CodeRabbit's and Greptile's are both counted; the log line splits them).
#   4  mr_rework active — an mr_rework run is on this MR; defer, let it finish, re-run.
#   5  CodeRabbit rate-limited on this head, CI settled, and no Greptile review either.
#      Prints CR_RESET_MIN=<n> when the walkthrough names the reset window. Wait it out
#      (scripts/cr-rate-limit.sh --wait), then trigger `@coderabbitai review` ONCE.
#   6  no reviewer will come on its own: CodeRabbit skipped this head (its status names
#      the reason: base branch, >100 files, ignored title keyword) or is absent past the
#      grace, and Greptile has not reviewed it. Trigger a bot or use a local reviewer.
#   7  CodeRabbit rejected `@coderabbitai review` because it considers the last commit
#      already reviewed; post `@coderabbitai full review` once, then re-run this watcher.
#
# "CodeRabbit reviewed this head" is the union of three robust signals, because a
# zero-actionable incremental review can post NO new review object AND re-anchor no
# finding (references/review-signals.md): (a) a CodeRabbit review whose commit_id == the
# head SHA, (c) the walkthrough comment's final_review_risk marker naming the head SHA, or
# (d) an "equivalent head" — CodeRabbit reviewed an earlier commit A and the delta
# A..HEAD is only the merge-in of the PR base branch plus regenerated artifacts, so no
# new branch-authored code exists for it to review (a logic-free merge commit; #819).
# "Greptile reviewed this head" is its `Greptile Review` check-run on the head SHA at
# status completed (its summary carries "N files reviewed, M comments added"); with 0
# findings Greptile posts NO review object, so the check-run is the only per-head signal.
# The script errs toward timeout (exit 2) rather than a false "ready".
set -euo pipefail

usage() { echo "usage: watch-pr.sh OWNER/REPO PR [interval_secs] [max_polls] [--reviewer any|coderabbit|greptile|none] [--reviewer-grace MIN]" >&2; exit 2; }

REVIEWER="any"; GRACE_MIN=10
POS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --reviewer) REVIEWER="${2:?}"; shift 2;;
    --reviewer-grace) GRACE_MIN="${2:?}"; shift 2;;
    -h|--help) usage;;
    -*) echo "unknown flag: $1" >&2; usage;;
    *) POS+=("$1"); shift;;
  esac
done
[ "${#POS[@]}" -ge 2 ] || usage
REPO=${POS[0]}
PR=${POS[1]}
INTERVAL=${POS[2]:-60}
MAX=${POS[3]:-60}
case "$REVIEWER" in any|coderabbit|greptile|none) ;; *) echo "bad --reviewer: $REVIEWER" >&2; usage;; esac

# A request that SUCCEEDED but returned a payload jq cannot read must count as UNKNOWN,
# never as "zero findings" — that is a route to a false ready. `gh api --paginate` emits
# one document per page, so the checks slurp and test every page's shape.
pages_are_arrays() { printf '%s' "$1" | jq -es 'length>0 and all(.[]; type=="array")' >/dev/null 2>&1; }
pages_are_checkruns() { printf '%s' "$1" | jq -es 'length>0 and all(.[]; type=="object" and has("check_runs"))' >/dev/null 2>&1; }
is_array() { printf '%s' "$1" | jq -e 'type=="array"' >/dev/null 2>&1; }

# uzi repo_id for the mr_rework check, resolved lazily inside the loop. THREE states are
# kept distinct so a failed lookup never masquerades as "not connected" (which would skip
# the rework check and risk a false ready): `repo_known=1` + non-empty id = connected, run
# the check; `repo_known=1` + empty id = genuinely not connected, safe to skip; `repo_known=0`
# = the `uzi repo list` never succeeded, an UNKNOWN that blocks readiness. mr_iid is per-repo,
# so the run filter matches on repo_id AND mr_iid.
repo_id=""
repo_known=0
start_ts=$(date +%s)

i=0
while [ "$i" -lt "$MAX" ]; do
  i=$((i + 1))
  # Any lookup that FAILS (network/API error) sets unknown=1 for this iteration, so the
  # decision is deferred to the next poll rather than made on a masked zero. A failed lookup
  # is not the same as a zero result.
  unknown=0

  if [ "$repo_known" -eq 0 ]; then
    # Only a listing that parses as an array is a KNOWN answer; malformed output must not
    # become "known, not connected" (which would skip the rework check on a false empty id).
    if rl=$(uzi repo list --json 2>/dev/null) && is_array "$rl"; then
      if repo_id=$(printf '%s' "$rl" | jq -r --arg p "$REPO" '[.[]|select(.path_with_namespace==$p)|.id]|first // ""' 2>/dev/null); then
        repo_known=1
      fi
    fi
  fi

  pv=$(gh pr view "$PR" --repo "$REPO" --json headRefOid,state 2>/dev/null || true)
  head=$(printf '%s' "$pv" | jq -r '.headRefOid // empty' 2>/dev/null || true)
  pstate=$(printf '%s' "$pv" | jq -r '.state // empty' 2>/dev/null || true)
  if [ -z "$head" ]; then
    echo "try $i: head unresolved (retrying)"
    sleep "$INTERVAL"
    continue
  fi
  if [ "$pstate" != "OPEN" ]; then echo "RESULT=pr_not_open state=$pstate"; exit 2; fi

  # CI: only REQUIRED checks gate (an optional failure must not force red), and `cancel` is
  # a non-ready state (a cancelled required check = supersession, not green). Parse the JSON
  # by validity, NOT by gh's exit code — `gh pr checks` exits non-zero merely for pending.
  # The payload must be a NON-EMPTY array: `{}` or `[]` (a PR whose checks have not
  # registered yet, or a malformed reply) would count as zero failing / zero pending and
  # forge a green, so both are unknown.
  fail=0; pend=0; cancel=0
  cj=$(gh pr checks "$PR" --repo "$REPO" --required --json bucket 2>/dev/null || true)
  if printf '%s' "$cj" | jq -e 'type=="array" and length>0' >/dev/null 2>&1; then
    fail=$(printf '%s' "$cj" | jq '[.[]|select(.bucket=="fail")]|length') || unknown=1
    pend=$(printf '%s' "$cj" | jq '[.[]|select(.bucket=="pending")]|length') || unknown=1
    cancel=$(printf '%s' "$cj" | jq '[.[]|select(.bucket=="cancel")]|length') || unknown=1
  else
    unknown=1
  fi

  # mr_rework: an active (non-terminal) run on this (repo_id, mr_iid). Only skip the check on
  # a KNOWN-not-connected repo; an unresolved repo_id or a failed run-list is unknown.
  mrw_active=0
  if [ "$repo_known" -eq 0 ]; then
    unknown=1
  elif [ -n "$repo_id" ]; then
    if rl2=$(uzi run list --json 2>/dev/null) && is_array "$rl2"; then
      mrw_active=$(printf '%s' "$rl2" | jq -r --arg repo "$repo_id" --argjson pr "$PR" \
        '[.[]|select(.kind=="mr_rework" and .repo_id==$repo and .mr_iid==$pr
                     and ((.status|test("completed|failed|cancelled"))|not))]|length' 2>/dev/null) || unknown=1
    else
      unknown=1
    fi
  fi

  # CodeRabbit's commit STATUS on the head (context "CodeRabbit"): its description is the
  # one place CR states why it did NOT review — "Review rate limited", "Review skipped:
  # reviews are disabled for this base branch" / "147 files exceed the limit of 100" /
  # "ignored keyword in the PR title" — and "Review in progress" while it works. The
  # `state` is `success` even when rate-limited, so only the description is read. Absent
  # (no context) = CR has not touched this head at all.
  cr_desc=""; cr_pending=0; cr_limited=0; cr_skipped=0; cr_absent=0
  if st=$(gh api "repos/$REPO/commits/$head/status" 2>/dev/null) && printf '%s' "$st" | jq -e 'has("statuses")' >/dev/null 2>&1; then
    cr_desc=$(printf '%s' "$st" | jq -r '[.statuses[]|select(.context=="CodeRabbit")]|last|.description // empty' 2>/dev/null) || unknown=1
    case "$cr_desc" in
      "") cr_absent=1 ;;
      *"in progress"*) cr_pending=1 ;;
      *"rate limited"*) cr_limited=1 ;;
      *"skipped"*) cr_skipped=1 ;;
    esac
  else
    unknown=1
  fi

  # Signal (a): a CodeRabbit review object on this exact head that is a VERDICT — APPROVED
  # (its clean pass: empty, tally-less body) or one carrying the "Actionable comments
  # posted: N" tally. A tally-less COMMENTED/CHANGES_REQUESTED review on the head is NOT a
  # verdict: CodeRabbit puts grouped / outside-diff findings in that review BODY, which the
  # inline count below never sees, so it is counted as an UNCONFIRMED finding instead
  # (pr-findings.sh classifies it the same way). Two gotchas handled here: `gh api --jq`
  # does NOT accept jq's --arg (so the head SHA is passed to standalone jq), and `gh api
  # --paginate` emits one array PER PAGE (so pages are slurped with `-s`/`.[][]`).
  cr_reviewed=0; cr_unconfirmed=0
  cr_a=""
  if rev_raw=$(gh api --paginate "repos/$REPO/pulls/$PR/reviews" 2>/dev/null) && pages_are_arrays "$rev_raw"; then
    # shellcheck disable=SC2016  # $h is a jq var (--arg), must stay single-quoted
    rev_on_head=$(printf '%s' "$rev_raw" | jq -rs --arg h "$head" \
      '[.[][]|select(.user.login=="coderabbitai[bot]" and .commit_id==$h
                     and (.state=="APPROVED" or ((.body // "")|test("Actionable comments posted: [0-9]+"))))]|length' 2>/dev/null) || unknown=1
    [ "${rev_on_head:-0}" -gt 0 ] && cr_reviewed=1
    # shellcheck disable=SC2016
    cr_unconfirmed=$(printf '%s' "$rev_raw" | jq -rs --arg h "$head" \
      '[.[][]|select(.user.login=="coderabbitai[bot]" and .commit_id==$h
                     and .state!="APPROVED" and (((.body // "")|test("Actionable comments posted: [0-9]+"))|not))]|length' 2>/dev/null) || unknown=1
    # cr_a: the commit CodeRabbit reviewed MOST RECENTLY (any head), for the equivalent-head
    # signal (d) below. Reviews come back oldest-first, so the last coderabbit entry is its
    # newest verdict. Empty when CodeRabbit has posted no review object yet.
    cr_a=$(printf '%s' "$rev_raw" | jq -rs \
      '[.[][]|select(.user.login=="coderabbitai[bot]")]|last|.commit_id // empty' 2>/dev/null) || unknown=1
  else
    unknown=1
  fi
  # Signal (c): the walkthrough comment, which CodeRabbit edits in place each pass and which
  # covers the zero-actionable incremental case (that posts no review object). It keys on the
  # `final_review_risk` block — "**Merge Risk:** ... · up to `<short-sha>`" between
  # `<!-- final_review_risk_start -->` / `<!-- final_review_risk_end -->`. (The older
  # `recent_review` "between BASE and HEAD" range is gone: 0 occurrences across PRs #807/#809/
  # #812 on 2026-08-29 — CodeRabbit migrated to final_review_risk. Parsing it was dead-format
  # matching over the whole body, which could false-match an unrelated "between … and <sha>"
  # phrase and forge a ready; removed. If it ever returns, signal (a) still covers a real review.)
  #
  # FAIL CLOSED, because reviewed_head=1 + live=0 is the auto-merge trigger: (1) exactly ONE
  # walkthrough comment is expected (CodeRabbit edits it in place) — 0 or >1 means don't trust
  # signal (c) at all, leave cr_reviewed to signal (a); and (2) the SHA is parsed only INSIDE
  # the final_review_risk block, so an unrelated "up to `<sha>`" phrase elsewhere in the body
  # cannot forge a match. The exact bot login is matched so a crafted comment can't spoof it.
  #
  # The rate-limit block ("Next included review available in N minutes") lives in the
  # comment CR edits in place when it refused this head (on #1428 the same comment as the
  # summary); N is relative to that comment's updated_at, so it is converted to a
  # REMAINING figure here. Read from the newest such comment, independently of the
  # exactly-one walkthrough rule (it is informational, not a merge signal).
  cr_reset_min=""; cr_full_required=0
  if issue_c=$(gh api --paginate "repos/$REPO/issues/$PR/comments" 2>/dev/null) && pages_are_arrays "$issue_c"; then
    wt_count=$(printf '%s' "$issue_c" | jq -rs '[.[][]|select(.user.login=="coderabbitai[bot]")|select(.body|contains("<!-- walkthrough_start -->"))]|length' 2>/dev/null) || unknown=1
    if [ "${wt_count:-0}" -eq 1 ]; then
      wt_body=$(printf '%s' "$issue_c" | jq -rs '.[][]|select(.user.login=="coderabbitai[bot]")|select(.body|contains("<!-- walkthrough_start -->"))|.body' 2>/dev/null || true)
      # final_review_risk marker: "up to `<sha>`" parsed ONLY within its own block.
      fr_block=$(printf '%s' "$wt_body" | awk '/final_review_risk_start/{f=1} f{print} /final_review_risk_end/{f=0}')
      # shellcheck disable=SC2016  # the backticks are LITERAL text in CodeRabbit's marker, not a subshell
      fr_head=$(printf '%s' "$fr_block" | grep -oE 'up to `[0-9a-f]{5,40}`' | tail -1 | grep -oE '[0-9a-f]{5,40}' || true)
      if [ -n "$fr_head" ] && printf '%s' "$head" | grep -q "^$fr_head"; then cr_reviewed=1; fi
    fi
    rl_row=$(printf '%s' "$issue_c" | jq -rs '[.[][]|select(.user.login=="coderabbitai[bot]")|select(.body|contains("rate limited by coderabbit.ai"))]|last|select(.!=null)|"\(.updated_at)\t\(.body)"' 2>/dev/null || true)
    if [ -n "$rl_row" ]; then
      rl_ts=$(printf '%s' "$rl_row" | head -1 | cut -f1)
      rl_n=$(printf '%s' "$rl_row" | awk '/auto-generated comment: rate limited by coderabbit.ai/{f=1} f{print} /end of auto-generated comment: rate limited/{f=0}' \
             | grep -oE 'available in [0-9]+ minutes' | tail -1 | grep -oE '[0-9]+' || true)
      if [ -n "$rl_n" ] && [ -n "$rl_ts" ]; then
        rl_epoch=$(jq -rn --arg t "$rl_ts" '$t|sub("\\.[0-9]+";"")|fromdateiso8601' 2>/dev/null || true)
        if [ -n "$rl_epoch" ]; then
          cr_reset_min=$(( ( rl_epoch + rl_n*60 - $(date +%s) + 59 ) / 60 ))
          [ "$cr_reset_min" -lt 0 ] && cr_reset_min=0
        fi
      fi
    fi
    # A normal review command can receive a terminal "already reviewed" reply while the
    # status remains the stale Review completed from an older head. Surface the prescribed
    # full-review command immediately, but only once: a later user full-review command
    # suppresses this state while CodeRabbit starts it.
    cr_full_required=$(printf '%s' "$issue_c" | jq -rs '
      ([.[][]|select(((.user.login|test("\\[bot\\]$"))|not)
                      and (((.body // "")|gsub("^\\s+|\\s+$";""))=="@coderabbitai review"))]|last) as $ask
      | if $ask==null then 0 else
          ([.[][]|select(.user.login=="coderabbitai[bot]" and .created_at>$ask.created_at
                         and ((.body // "")|contains("Already reviewed the last commit"))
                         and ((.body // "")|contains("@coderabbitai full review")))]|last) as $reply
          | if $reply==null then 0 else
              ([.[][]|select(((.user.login|test("\\[bot\\]$"))|not)
                             and .created_at>$reply.created_at
                             and (((.body // "")|gsub("^\\s+|\\s+$";""))=="@coderabbitai full review"))]|length) as $full
              | if $full==0 then 1 else 0 end
            end
        end' 2>/dev/null) || unknown=1
  else
    unknown=1
  fi

  # Live inline findings: bot comments still anchored to current code (line != null) AND
  # not self-marked resolved. Two ways a CodeRabbit finding stops being live: an outdated
  # finding re-anchors to line == null; a finding CodeRabbit judged FIXED by a later commit
  # keeps line != null and instead appends a "✅ Addressed in commit <sha>" line to its body
  # (measured 2026-08-29 on PR #807, where two such addressed findings were mis-counted as
  # live=2 and produced a false exit-3 after a clean rework). Greptile findings are counted
  # by the same line != null anchor (no addressed-marker convention has been observed).
  cr_live=0; gr_live=0
  if pull_c=$(gh api --paginate "repos/$REPO/pulls/$PR/comments" 2>/dev/null) && pages_are_arrays "$pull_c"; then
    cr_live=$(printf '%s' "$pull_c" | jq -rs '[.[][]|select(.user.login=="coderabbitai[bot]" and .line!=null and ((.body|contains("Addressed in commit"))|not))]|length' 2>/dev/null) || unknown=1
    gr_live=$(printf '%s' "$pull_c" | jq -rs '[.[][]|select(.user.login=="greptile-apps[bot]" and .line!=null)]|length' 2>/dev/null) || unknown=1
  else
    unknown=1
  fi
  # An unconfirmed CodeRabbit review (grouped findings in its body) counts as live: it
  # blocks a ready and surfaces as exit 3 for a human to read the body.
  live=$((cr_live + gr_live + cr_unconfirmed))

  # Greptile: its `Greptile Review` check-run on the head (app slug greptile-apps). Absent =
  # not triggered on this head (Greptile is on-demand here: greptile.json autoReview []).
  # "Reviewed" needs ALL of: status completed, conclusion success, and the "N files
  # reviewed, M comments added" summary — a completed check that failed, was cancelled or
  # skipped is not a review. `gh api --paginate` emits one object per page, so slurp first.
  gr_state="absent"; gr_summary=""; gr_concl=""
  if cr_raw=$(gh api --paginate "repos/$REPO/commits/$head/check-runs" 2>/dev/null) && pages_are_checkruns "$cr_raw"; then
    gr_json=$(printf '%s' "$cr_raw" | jq -s '[.[].check_runs[]?|select(.app.slug=="greptile-apps" and .name=="Greptile Review")]|last // empty' 2>/dev/null) || unknown=1
    if [ -n "$gr_json" ]; then
      gr_state=$(printf '%s' "$gr_json" | jq -r '.status // "absent"' 2>/dev/null) || unknown=1
      gr_concl=$(printf '%s' "$gr_json" | jq -r '.conclusion // ""' 2>/dev/null) || unknown=1
      gr_summary=$(printf '%s' "$gr_json" | jq -r '.output.summary // ""' 2>/dev/null | grep -oE '[0-9]+ files reviewed, [0-9]+ comments added' || true)
    fi
  else
    unknown=1
  fi
  gr_reviewed=0
  if [ "$gr_state" = "completed" ] && [ "$gr_concl" = "success" ] && [ -n "$gr_summary" ]; then gr_reviewed=1; fi
  [ "$gr_state" = "completed" ] && [ "$gr_reviewed" -eq 0 ] && gr_state="completed(${gr_concl:-no-conclusion}, no summary)"

  # Signal (d): "equivalent head" — a logic-free merge commit CodeRabbit did not re-review
  # (issue #819). When the head is a merge that only brings in the PR base branch plus
  # regenerated artifacts, CodeRabbit posts no fresh review (nothing to review), so signals
  # (a)/(c) never fire and a genuinely merge-ready PR times out. Recognize ONLY the provably
  # safe case, fail closed on everything else: HEAD is reviewed-equivalent to CodeRabbit's
  # last-reviewed commit cr_a when EVERY path that changed between cr_a and HEAD is either
  #   - absent from the PR's diff vs its base branch (HEAD's version equals base's, so the
  #     change came in with the merge — the branch did not author it), or
  #   - a regenerated/mirror artifact (api/internal/store/*.sql.go,
  #     api/internal/uzidocs/embed/*.md) derived from already-reviewed sources.
  # Any changed path that IS in the PR diff and is NOT such an artifact is real branch work
  # CodeRabbit has not seen — leave cr_reviewed=0 (→ timeout, never a false "ready").
  # Two GitHub compare calls, no local git (keeps this script cwd-independent). Attempted only
  # when we would otherwise be ready but for the missing review, to bound the cost. The compare
  # API caps .files at 300; a truncated list could hide an unreviewed path and forge
  # equivalence, so we refuse to judge at/above the cap.
  equiv=0
  if [ "$cr_reviewed" -eq 0 ] && [ "$unknown" -eq 0 ] && [ "$fail" -eq 0 ] \
     && [ "$pend" -eq 0 ] && [ "$cancel" -eq 0 ] && [ -n "$cr_a" ] && [ "$cr_a" != "$head" ]; then
    base=$(gh pr view "$PR" --repo "$REPO" --json baseRefName -q .baseRefName 2>/dev/null || true)
    if [ -n "$base" ] \
       && cmp_ah=$(gh api "repos/$REPO/compare/$cr_a...$head" 2>/dev/null) \
       && cmp_mh=$(gh api "repos/$REPO/compare/$base...$head" 2>/dev/null); then
      n_ah=$(printf '%s' "$cmp_ah" | jq '.files|length' 2>/dev/null || echo 999)
      n_mh=$(printf '%s' "$cmp_mh" | jq '.files|length' 2>/dev/null || echo 999)
      changed_ah=$(printf '%s' "$cmp_ah" | jq -r '.files[]?.filename' 2>/dev/null || true)
      pr_diff=$(printf '%s' "$cmp_mh" | jq -r '.files[]?.filename' 2>/dev/null || true)
      if [ -n "$changed_ah" ] && [ "$n_ah" -lt 300 ] && [ "$n_mh" -lt 300 ]; then
        equiv=1
        while IFS= read -r f; do
          [ -z "$f" ] && continue
          # Absent from the PR's diff vs base ⇒ HEAD matches base for this path ⇒ a merge-in.
          if ! printf '%s\n' "$pr_diff" | grep -qxF "$f"; then continue; fi
          # In the PR diff but a regenerated/mirror artifact derived from reviewed sources.
          # Scope each pattern to the exact dir a generator/sync check covers: validate:api
          # regenerates only api/internal/store, and docs:sync mirrors only *.md into embed.
          # A broader glob (*.sql.go anywhere, any file under embed/) would forgive a
          # branch-added file no check regenerates — a fail-open hole in a fail-closed gate.
          case "$f" in
            api/internal/store/*.sql.go) continue ;;
            api/internal/uzidocs/embed/*.md) continue ;;
          esac
          # Otherwise: branch-authored change CodeRabbit has not reviewed. Not equivalent.
          equiv=0
          break
        done < <(printf '%s\n' "$changed_ah")
      fi
      [ "$equiv" -eq 1 ] && cr_reviewed=1
    else
      unknown=1
    fi
  fi

  # The reviewer gate, per --reviewer.
  case "$REVIEWER" in
    any)        reviewed_head=$(( cr_reviewed || gr_reviewed )) ;;
    coderabbit) reviewed_head=$cr_reviewed ;;
    greptile)   reviewed_head=$gr_reviewed ;;
    none)       reviewed_head=1 ;;
  esac

  eqnote=""
  [ "$equiv" -eq 1 ] && eqnote=" equiv=1"
  echo "try $i: head=${head:0:8} req_fail=$fail req_pend=$pend req_cancel=$cancel mrw_active=$mrw_active cr_reviewed=$cr_reviewed${eqnote} cr_status='${cr_desc:-absent}' cr_full_required=$cr_full_required greptile=$gr_state${gr_summary:+ ($gr_summary)} live=$live (cr=$cr_live gr=$gr_live cr_unconfirmed=$cr_unconfirmed)${unknown:+ unknown=$unknown}"

  # A failed lookup this iteration: defer, do not decide on masked values.
  if [ "$unknown" -ne 0 ]; then sleep "$INTERVAL"; continue; fi

  if [ "$fail" -gt 0 ]; then echo "RESULT=red"; exit 1; fi
  if [ "$mrw_active" -gt 0 ]; then echo "RESULT=mr_rework_active"; exit 4; fi
  if [ "$pend" -eq 0 ] && [ "$cancel" -eq 0 ]; then
    if [ "$reviewed_head" -eq 1 ]; then
      # Revalidate the head right before deciding (TOCTOU): a push during this iteration would
      # otherwise let an exit 0 describe an unreviewed head. Proceed to ready ONLY when the
      # re-read succeeds AND matches; an empty (failed) re-read is unknown, not a match — defer.
      head2=$(gh pr view "$PR" --repo "$REPO" --json headRefOid -q .headRefOid 2>/dev/null || true)
      if [ -z "$head2" ] || [ "$head2" != "$head" ]; then
        echo "try $i: head unconfirmed (${head:0:8} -> ${head2:0:8}), re-polling"
        sleep "$INTERVAL"; continue
      fi
      if [ "$live" -eq 0 ]; then echo "RESULT=ready"; exit 0; fi
      echo "RESULT=findings live=$live cr=$cr_live gr=$gr_live cr_unconfirmed=$cr_unconfirmed"; exit 3
    fi
    # CI settled, no review on this head. Is one coming, or must the caller act?
    if [ "$cr_full_required" -eq 1 ] && [ "$REVIEWER" != "greptile" ] && [ "$REVIEWER" != "none" ]; then
      echo "RESULT=cr_full_review_required command='@coderabbitai full review'"
      exit 7
    elif [ "$cr_pending" -eq 1 ] || [ "$gr_state" = "in_progress" ] || [ "$gr_state" = "queued" ]; then
      : # a review is in flight; keep polling
    elif [ "$cr_limited" -eq 1 ] && [ "$REVIEWER" != "greptile" ] && [ "$gr_reviewed" -eq 0 ]; then
      echo "RESULT=cr_rate_limited${cr_reset_min:+ CR_RESET_MIN=$cr_reset_min}"
      [ -n "$cr_reset_min" ] && echo "CR_RESET_MIN=$cr_reset_min"
      exit 5
    else
      elapsed_min=$(( ( $(date +%s) - start_ts ) / 60 ))
      # A skipped head is deterministic: CR will never review it on its own. An ABSENT
      # status (CR has not touched the head) or an untriggered Greptile gets the grace,
      # since the walkthrough lands minutes after the PR opens and a Greptile check-run
      # appears ~12 s after its trigger.
      if [ "$cr_skipped" -eq 1 ] && [ "$REVIEWER" != "greptile" ]; then
        echo "RESULT=no_reviewer reason='$cr_desc'"; exit 6
      fi
      if { [ "$cr_absent" -eq 1 ] || [ "$cr_limited" -eq 1 ] || [ "$REVIEWER" = "greptile" ]; } && [ "$elapsed_min" -ge "$GRACE_MIN" ]; then
        echo "RESULT=no_reviewer reason='cr=${cr_desc:-absent} greptile=$gr_state after ${elapsed_min}m grace'"; exit 6
      fi
    fi
  fi

  sleep "$INTERVAL"
done

echo "RESULT=timeout"
exit 2
