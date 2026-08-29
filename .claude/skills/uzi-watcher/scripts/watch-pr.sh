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

# uzi repo_id for the mr_rework check. Best-effort: empty when uzi is not configured or
# the repo is not connected, in which case the mr_rework check is skipped — never a false
# "active". mr_iid is per-repo, so the run filter matches on repo_id AND mr_iid.
repo_id=$(uzi repo list --json 2>/dev/null \
  | jq -r --arg p "$REPO" '.[]|select(.path_with_namespace==$p)|.id' 2>/dev/null | head -1 || true)

i=0
while [ "$i" -lt "$MAX" ]; do
  i=$((i + 1))

  head=$(gh pr view "$PR" --repo "$REPO" --json headRefOid -q .headRefOid 2>/dev/null || true)
  if [ -z "$head" ]; then
    echo "try $i: head unresolved"
    sleep "$INTERVAL"
    continue
  fi

  checks=$(gh pr checks "$PR" --repo "$REPO" 2>/dev/null || true)
  fail=$(printf '%s\n' "$checks" | awk -F'\t' '$2=="fail"{n++} END{print n + 0}')
  # CodeRabbit's own check is excluded from the pending count — it settles on its own axis.
  pend=$(printf '%s\n' "$checks" | grep -v -i coderabbit | awk -F'\t' '$2=="pending"{n++} END{print n + 0}')

  mrw_active=0
  if [ -n "$repo_id" ]; then
    mrw_active=$(uzi run list --json 2>/dev/null | jq -r \
      --arg repo "$repo_id" --argjson pr "$PR" \
      '[.[]|select(.kind=="mr_rework" and .repo_id==$repo and .mr_iid==$pr
                   and ((.status|test("completed|failed|cancelled"))|not))]|length' \
      2>/dev/null || echo 0)
  fi

  # Signal (a): a CodeRabbit review object on this exact head. Two gotchas handled here:
  # `gh api --jq` does NOT accept jq's --arg (so the head SHA is passed to standalone jq),
  # and `gh api --paginate --jq length` counts PER PAGE (so pages are slurped with `-s` and
  # `.[][]` before counting). The constant bot login is inlined into the filter.
  # shellcheck disable=SC2016  # $h is a jq var (--arg), must stay single-quoted
  rev_on_head=$(gh api --paginate "repos/$REPO/pulls/$PR/reviews" 2>/dev/null \
    | jq -rs --arg h "$head" \
      '[.[][]|select(.user.login=="coderabbitai[bot]" and .commit_id==$h)]|length' \
      2>/dev/null || echo 0)
  # Signal (c): the walkthrough recent_review range ending at this head. gh's built-in --jq
  # is fine here — no shell var in the filter, and per-page emission only feeds the grep.
  wt_head=$(gh api --paginate "repos/$REPO/issues/$PR/comments" \
    --jq '.[]|select(.user.login=="coderabbitai[bot]")|select(.body|contains("<!-- walkthrough_start -->"))|.body' \
    2>/dev/null | grep -oE 'and [0-9a-f]{7,40}' | tail -1 | awk '{print $2}' || true)
  reviewed_head=0
  [ "${rev_on_head:-0}" -gt 0 ] && reviewed_head=1
  if [ -n "$wt_head" ] && printf '%s' "$head" | grep -q "^$wt_head"; then reviewed_head=1; fi

  # Live inline findings: CodeRabbit comments still anchored to current code (line != null);
  # an addressed finding re-anchors to line == null (outdated). Slurp pages, then count once.
  live=$(gh api --paginate "repos/$REPO/pulls/$PR/comments" 2>/dev/null \
    | jq -rs '[.[][]|select(.user.login=="coderabbitai[bot]" and .line!=null)]|length' \
      2>/dev/null || echo 0)

  echo "try $i: head=${head:0:8} ci_fail=$fail ci_pend=$pend mrw_active=$mrw_active reviewed_head=$reviewed_head live_findings=$live"

  if [ "$fail" -gt 0 ]; then echo "RESULT=red"; exit 1; fi
  if [ "$mrw_active" -gt 0 ]; then echo "RESULT=mr_rework_active"; exit 4; fi
  if [ "$pend" -eq 0 ] && [ "$reviewed_head" -eq 1 ]; then
    if [ "$live" -eq 0 ]; then echo "RESULT=ready"; exit 0; fi
    echo "RESULT=findings live=$live"; exit 3
  fi

  sleep "$INTERVAL"
done

echo "RESULT=timeout"
exit 2
