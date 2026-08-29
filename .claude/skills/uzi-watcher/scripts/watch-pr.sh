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
# "CodeRabbit reviewed this head" is the union of two robust signals, because a
# zero-actionable incremental review can post NO new review object AND re-anchor no
# finding (SKILL.md signal (c)): (a) a CodeRabbit review whose commit_id == the head
# SHA, or (c) the walkthrough comment's recent_review range ending at the head SHA.
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
  if rev_raw=$(gh api --paginate "repos/$REPO/pulls/$PR/reviews" 2>/dev/null); then
    # shellcheck disable=SC2016  # $h is a jq var (--arg), must stay single-quoted
    rev_on_head=$(printf '%s' "$rev_raw" | jq -rs --arg h "$head" \
      '[.[][]|select(.user.login=="coderabbitai[bot]" and .commit_id==$h)]|length' 2>/dev/null || echo 0)
    [ "${rev_on_head:-0}" -gt 0 ] && reviewed_head=1
  else
    unknown=1
  fi
  # Signal (c): the walkthrough recent_review range ending at this head — covers the
  # zero-actionable incremental case, which posts no review object.
  if issue_c=$(gh api --paginate "repos/$REPO/issues/$PR/comments" 2>/dev/null); then
    wt_head=$(printf '%s' "$issue_c" | jq -rs '.[][]|select(.user.login=="coderabbitai[bot]")|select(.body|contains("<!-- walkthrough_start -->"))|.body' 2>/dev/null \
      | grep -oE 'and [0-9a-f]{7,40}' | tail -1 | awk '{print $2}' || true)
    if [ -n "$wt_head" ] && printf '%s' "$head" | grep -q "^$wt_head"; then reviewed_head=1; fi
  else
    unknown=1
  fi

  # Live inline findings: CodeRabbit comments still anchored to current code (line != null);
  # an addressed finding re-anchors to line == null (outdated).
  live=0
  if pull_c=$(gh api --paginate "repos/$REPO/pulls/$PR/comments" 2>/dev/null); then
    live=$(printf '%s' "$pull_c" | jq -rs '[.[][]|select(.user.login=="coderabbitai[bot]" and .line!=null)]|length' 2>/dev/null || echo 0)
  else
    unknown=1
  fi

  echo "try $i: head=${head:0:8} req_fail=$fail req_pend=$pend req_cancel=$cancel mrw_active=$mrw_active reviewed_head=$reviewed_head live_findings=$live${unknown:+ unknown=$unknown}"

  # A failed lookup this iteration: defer, do not decide on masked values.
  if [ "$unknown" -ne 0 ]; then sleep "$INTERVAL"; continue; fi

  if [ "$fail" -gt 0 ]; then echo "RESULT=red"; exit 1; fi
  if [ "$mrw_active" -gt 0 ]; then echo "RESULT=mr_rework_active"; exit 4; fi
  if [ "$pend" -eq 0 ] && [ "$cancel" -eq 0 ] && [ "$reviewed_head" -eq 1 ]; then
    # Revalidate the head right before deciding (TOCTOU): a push during this iteration would
    # otherwise let an exit 0 describe an unreviewed head.
    head2=$(gh pr view "$PR" --repo "$REPO" --json headRefOid -q .headRefOid 2>/dev/null || true)
    if [ -n "$head2" ] && [ "$head2" != "$head" ]; then
      echo "try $i: head moved ${head:0:8} -> ${head2:0:8}, re-polling"
      sleep "$INTERVAL"; continue
    fi
    if [ "$live" -eq 0 ]; then echo "RESULT=ready"; exit 0; fi
    echo "RESULT=findings live=$live"; exit 3
  fi

  sleep "$INTERVAL"
done

echo "RESULT=timeout"
exit 2
