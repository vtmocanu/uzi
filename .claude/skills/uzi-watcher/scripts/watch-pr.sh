#!/usr/bin/env bash
#
# watch-pr.sh — poll a GitHub PR to merge-readiness for the uzi-watcher Auto flow.
#
# It combines the three signals a merge decision here actually needs, so a session
# stops hand-rolling (and mis-writing) the same waiter each run:
#   1. CI is settled and green on the PR's *current* head;
#   2. CodeRabbit has reviewed that exact head and left no live (unresolved) inline
#      findings; and
#   3. no in-flight uzi `mr_rework` run is reworking this MR (which would race a local
#      fix or a merge — see SKILL.md, "uzi may fix the CodeRabbit findings ITSELF").
#
# Usage: watch-pr.sh OWNER/REPO PR [interval_secs] [max_polls]
#   interval_secs default 60, max_polls default 60.
#
# Exit codes (callers branch on these; keep them stable):
#   0  merge-ready — CI green on head, CodeRabbit reviewed head with 0 live findings,
#      and no active mr_rework.
#   1  red — a required CI check failed on the head.
#   2  timeout — readiness not reached within max_polls, or the head never resolved.
#      NOTE: exit 0 is trustworthy; exit 2 means "inspect manually", never "merge".
#   3  findings — CodeRabbit reviewed the head but left live inline findings to triage.
#   4  mr_rework active — an mr_rework run is on this MR; defer, let it finish, re-run.
#
# "CodeRabbit reviewed this head" is the union of three robust signals, because a
# zero-actionable incremental review can post NO new review object AND re-anchor no
# finding (SKILL.md signal (c)): (a) a CodeRabbit review whose commit_id == the head
# SHA, (c) the walkthrough comment's final_review_risk marker naming the head SHA, or
# (d) an "equivalent head" — CodeRabbit reviewed an earlier commit A and the delta
# A..HEAD is only the merge-in of the PR base branch plus regenerated artifacts, so no
# new branch-authored code exists for it to review (a logic-free merge commit; #819).
# The script errs toward timeout (exit 2) rather than a false "ready".
set -euo pipefail

REPO=${1:?usage: watch-pr.sh OWNER/REPO PR [interval_secs] [max_polls]}
PR=${2:?usage: watch-pr.sh OWNER/REPO PR [interval_secs] [max_polls]}
INTERVAL=${3:-60}
MAX=${4:-60}

# uzi repo_id for the mr_rework check, resolved lazily inside the loop. THREE states are
# kept distinct so a failed lookup never masquerades as "not connected" (which would skip
# the rework check and risk a false ready): `repo_known=1` + non-empty id = connected, run
# the check; `repo_known=1` + empty id = genuinely not connected, safe to skip; `repo_known=0`
# = the `uzi repo list` never succeeded, an UNKNOWN that blocks readiness. mr_iid is per-repo,
# so the run filter matches on repo_id AND mr_iid.
repo_id=""
repo_known=0

i=0
while [ "$i" -lt "$MAX" ]; do
  i=$((i + 1))
  # Any lookup that FAILS (network/API error) sets unknown=1 for this iteration, so the
  # decision is deferred to the next poll rather than made on a masked zero. A failed lookup
  # is not the same as a zero result.
  unknown=0

  if [ "$repo_known" -eq 0 ]; then
    if rl=$(uzi repo list --json 2>/dev/null); then
      repo_id=$(printf '%s' "$rl" | jq -r --arg p "$REPO" '.[]|select(.path_with_namespace==$p)|.id' 2>/dev/null | head -1 || true)
      repo_known=1
    fi
  fi

  head=$(gh pr view "$PR" --repo "$REPO" --json headRefOid -q .headRefOid 2>/dev/null || true)
  if [ -z "$head" ]; then
    echo "try $i: head unresolved (retrying)"
    sleep "$INTERVAL"
    continue
  fi

  # CI: only REQUIRED checks gate (an optional failure must not force red), and `cancel` is
  # a non-ready state (a cancelled required check = supersession, not green). Parse the JSON
  # by validity, NOT by gh's exit code — `gh pr checks` exits non-zero merely for pending.
  fail=0; pend=0; cancel=0
  cj=$(gh pr checks "$PR" --repo "$REPO" --required --json bucket 2>/dev/null || true)
  if printf '%s' "$cj" | jq -e . >/dev/null 2>&1; then
    fail=$(printf '%s' "$cj" | jq '[.[]|select(.bucket=="fail")]|length')
    pend=$(printf '%s' "$cj" | jq '[.[]|select(.bucket=="pending")]|length')
    cancel=$(printf '%s' "$cj" | jq '[.[]|select(.bucket=="cancel")]|length')
  else
    unknown=1
  fi

  # mr_rework: an active (non-terminal) run on this (repo_id, mr_iid). Only skip the check on
  # a KNOWN-not-connected repo; an unresolved repo_id or a failed run-list is unknown.
  mrw_active=0
  if [ "$repo_known" -eq 0 ]; then
    unknown=1
  elif [ -n "$repo_id" ]; then
    if rl2=$(uzi run list --json 2>/dev/null); then
      mrw_active=$(printf '%s' "$rl2" | jq -r --arg repo "$repo_id" --argjson pr "$PR" \
        '[.[]|select(.kind=="mr_rework" and .repo_id==$repo and .mr_iid==$pr
                     and ((.status|test("completed|failed|cancelled"))|not))]|length' 2>/dev/null || echo 0)
    else
      unknown=1
    fi
  fi

  # Signal (a): a CodeRabbit review object on this exact head. Two gotchas handled here:
  # `gh api --jq` does NOT accept jq's --arg (so the head SHA is passed to standalone jq),
  # and `gh api --paginate` emits one array PER PAGE (so pages are slurped with `-s`/`.[][]`
  # before counting). The constant bot login is inlined into the filter.
  reviewed_head=0
  cr_a=""
  if rev_raw=$(gh api --paginate "repos/$REPO/pulls/$PR/reviews" 2>/dev/null); then
    # shellcheck disable=SC2016  # $h is a jq var (--arg), must stay single-quoted
    rev_on_head=$(printf '%s' "$rev_raw" | jq -rs --arg h "$head" \
      '[.[][]|select(.user.login=="coderabbitai[bot]" and .commit_id==$h)]|length' 2>/dev/null || echo 0)
    [ "${rev_on_head:-0}" -gt 0 ] && reviewed_head=1
    # cr_a: the commit CodeRabbit reviewed MOST RECENTLY (any head), for the equivalent-head
    # signal (d) below. Reviews come back oldest-first, so the last coderabbit entry is its
    # newest verdict. Empty when CodeRabbit has posted no review object yet.
    cr_a=$(printf '%s' "$rev_raw" | jq -rs \
      '[.[][]|select(.user.login=="coderabbitai[bot]")]|last|.commit_id // empty' 2>/dev/null || true)
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
  # signal (c) at all, leave reviewed_head to signal (a); and (2) the SHA is parsed only INSIDE
  # the final_review_risk block, so an unrelated "up to `<sha>`" phrase elsewhere in the body
  # cannot forge a match. The exact bot login is matched so a crafted comment can't spoof it.
  if issue_c=$(gh api --paginate "repos/$REPO/issues/$PR/comments" 2>/dev/null); then
    wt_count=$(printf '%s' "$issue_c" | jq -rs '[.[][]|select(.user.login=="coderabbitai[bot]")|select(.body|contains("<!-- walkthrough_start -->"))]|length' 2>/dev/null || echo 0)
    if [ "${wt_count:-0}" -eq 1 ]; then
      wt_body=$(printf '%s' "$issue_c" | jq -rs '.[][]|select(.user.login=="coderabbitai[bot]")|select(.body|contains("<!-- walkthrough_start -->"))|.body' 2>/dev/null || true)
      # final_review_risk marker: "up to `<sha>`" parsed ONLY within its own block.
      fr_block=$(printf '%s' "$wt_body" | awk '/final_review_risk_start/{f=1} f{print} /final_review_risk_end/{f=0}')
      # shellcheck disable=SC2016  # the backticks are LITERAL text in CodeRabbit's marker, not a subshell
      fr_head=$(printf '%s' "$fr_block" | grep -oE 'up to `[0-9a-f]{5,40}`' | tail -1 | grep -oE '[0-9a-f]{5,40}' || true)
      if [ -n "$fr_head" ] && printf '%s' "$head" | grep -q "^$fr_head"; then reviewed_head=1; fi
    fi
  else
    unknown=1
  fi

  # Live inline findings: CodeRabbit comments still anchored to current code (line != null)
  # AND not self-marked resolved. Two ways a finding stops being live, and only the first was
  # handled before: an outdated finding re-anchors to line == null; but a finding CodeRabbit
  # judged FIXED by a later commit keeps line != null and instead appends a "✅ Addressed in
  # commit <sha>" line to its body (measured 2026-08-29 on PR #807, where two such addressed
  # findings were mis-counted as live=2 and produced a false exit-3 after a clean rework).
  # Exclude both, so an addressed finding does not read as a live one.
  live=0
  if pull_c=$(gh api --paginate "repos/$REPO/pulls/$PR/comments" 2>/dev/null); then
    live=$(printf '%s' "$pull_c" | jq -rs '[.[][]|select(.user.login=="coderabbitai[bot]" and .line!=null and ((.body|contains("Addressed in commit"))|not))]|length' 2>/dev/null || echo 0)
  else
    unknown=1
  fi

  # Signal (d): "equivalent head" — a logic-free merge commit CodeRabbit did not re-review
  # (issue #819). When the head is a merge that only brings in the PR base branch plus
  # regenerated artifacts, CodeRabbit posts no fresh review (nothing to review), so signals
  # (a)/(c) never fire and a genuinely merge-ready PR times out. Recognize ONLY the provably
  # safe case, fail closed on everything else: HEAD is reviewed-equivalent to CodeRabbit's
  # last-reviewed commit cr_a when EVERY path that changed between cr_a and HEAD is either
  #   - absent from the PR's diff vs its base branch (HEAD's version equals base's, so the
  #     change came in with the merge — the branch did not author it), or
  #   - a regenerated/mirror artifact (*.sql.go, api/internal/uzidocs/embed/**) derived from
  #     already-reviewed sources.
  # Any changed path that IS in the PR diff and is NOT such an artifact is real branch work
  # CodeRabbit has not seen — leave reviewed_head=0 (→ timeout, never a false "ready").
  # Two GitHub compare calls, no local git (keeps this script cwd-independent). Attempted only
  # when we would otherwise be ready but for the missing review, to bound the cost. The compare
  # API caps .files at 300; a truncated list could hide an unreviewed path and forge
  # equivalence, so we refuse to judge at/above the cap.
  equiv=0
  if [ "$reviewed_head" -eq 0 ] && [ "$unknown" -eq 0 ] && [ "$fail" -eq 0 ] \
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
          case "$f" in
            *.sql.go) continue ;;
            api/internal/uzidocs/embed/*) continue ;;
          esac
          # Otherwise: branch-authored change CodeRabbit has not reviewed. Not equivalent.
          equiv=0
          break
        done < <(printf '%s\n' "$changed_ah")
      fi
      [ "$equiv" -eq 1 ] && reviewed_head=1
    else
      unknown=1
    fi
  fi

  eqnote=""
  [ "$equiv" -eq 1 ] && eqnote=" equiv=1"
  echo "try $i: head=${head:0:8} req_fail=$fail req_pend=$pend req_cancel=$cancel mrw_active=$mrw_active reviewed_head=$reviewed_head${eqnote} live_findings=$live${unknown:+ unknown=$unknown}"

  # A failed lookup this iteration: defer, do not decide on masked values.
  if [ "$unknown" -ne 0 ]; then sleep "$INTERVAL"; continue; fi

  if [ "$fail" -gt 0 ]; then echo "RESULT=red"; exit 1; fi
  if [ "$mrw_active" -gt 0 ]; then echo "RESULT=mr_rework_active"; exit 4; fi
  if [ "$pend" -eq 0 ] && [ "$cancel" -eq 0 ] && [ "$reviewed_head" -eq 1 ]; then
    # Revalidate the head right before deciding (TOCTOU): a push during this iteration would
    # otherwise let an exit 0 describe an unreviewed head. Proceed to ready ONLY when the
    # re-read succeeds AND matches; an empty (failed) re-read is unknown, not a match — defer.
    head2=$(gh pr view "$PR" --repo "$REPO" --json headRefOid -q .headRefOid 2>/dev/null || true)
    if [ -z "$head2" ] || [ "$head2" != "$head" ]; then
      echo "try $i: head unconfirmed (${head:0:8} -> ${head2:0:8}), re-polling"
      sleep "$INTERVAL"; continue
    fi
    if [ "$live" -eq 0 ]; then echo "RESULT=ready"; exit 0; fi
    echo "RESULT=findings live=$live"; exit 3
  fi

  sleep "$INTERVAL"
done

echo "RESULT=timeout"
exit 2
