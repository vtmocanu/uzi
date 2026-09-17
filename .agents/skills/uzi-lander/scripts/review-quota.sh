#!/usr/bin/env bash
# review-quota.sh — who else is about to consume CodeRabbit reviews on this repo?
#
# CodeRabbit's quota is per org and adaptive; every push to a CR-eligible PR is one review,
# and uzi's own runs open PRs and push reworks without asking you. Run this BEFORE you
# trigger `@coderabbitai review` or push a fix, so a batch of fixes goes out as ONE push per
# PR and you do not queue behind (or starve) a sibling PR's review.
#
# Usage: review-quota.sh OWNER/REPO
#
# Prints:
#   - every open, non-draft PR with its CodeRabbit status on the head, flagging the ones CR
#     will NOT review on its own (renovate author, a *(deps) / [skip-cr] title) since those
#     cost nothing;
#   - every non-terminal uzi run on the repo that will open a PR or push to one (issue,
#     prompt, self_improve, ci_fix, task runs; and mr_rework runs, which push to a named PR);
#   - a KEY=VALUE summary: CR_IN_PROGRESS, CR_LIMITED, CR_ELIGIBLE_OPEN, UPCOMING_PUSHES.
# Exit 0 always (informational); 3 on a gh error. A missing/failed `uzi` CLI is reported,
# not fatal.
set -uo pipefail

REPO=${1:?usage: review-quota.sh OWNER/REPO}

prs=$(gh pr list --repo "$REPO" --state open --limit 50 \
  --json number,title,author,isDraft,headRefOid,updatedAt 2>/dev/null) || { echo "gh pr list failed" >&2; exit 3; }

echo "== open PRs on $REPO (CodeRabbit status on head) =="
in_progress=0; limited=0; eligible=0
while IFS=$'\t' read -r num author draft head title; do
  [ -z "$num" ] && continue
  elig="yes"
  case "$author" in renovate\[bot\]) elig="no (renovate author)";; esac
  case "$title" in
    *"chore(deps)"*|*"fix(deps)"*|*"build(deps)"*) elig="no (deps title)";;
    *"[skip-cr]"*) elig="no ([skip-cr])";;
  esac
  [ "$draft" = "true" ] && elig="no (draft)"
  desc=$(gh api "repos/$REPO/commits/$head/status" \
    --jq '[.statuses[]|select(.context=="CodeRabbit")]|last|.description // empty' 2>/dev/null || true)
  case "$desc" in
    *"in progress"*) in_progress=$((in_progress+1));;
    *"rate limited"*) limited=$((limited+1));;
  esac
  case "$elig" in yes) eligible=$((eligible+1));; esac
  printf '  #%-5s cr=%-45s eligible=%-22s %s\n' "$num" "'${desc:-absent}'" "$elig" "${title:0:60}"
done < <(printf '%s' "$prs" | jq -r '.[]|[.number, .author.login, (.isDraft|tostring), .headRefOid, .title]|@tsv')

echo
echo "== uzi runs that will open or push to a PR =="
upcoming=0
if command -v uzi >/dev/null 2>&1; then
  repo_id=$(uzi repo list --json 2>/dev/null | jq -r --arg p "$REPO" '.[]|select(.path_with_namespace==$p)|.id' 2>/dev/null | head -1 || true)
  if [ -n "$repo_id" ]; then
    rows=$(uzi run list --json 2>/dev/null | jq -r --arg repo "$repo_id" '
      [.[]|select(.repo_id==$repo and ((.status|test("completed|failed|cancelled"))|not))
           |select(.kind=="issue" or .kind=="prompt" or .kind=="self_improve" or .kind=="ci_fix" or .kind=="task" or .kind=="mr_rework")]
      |.[]|"\(.id[0:8])\t\(.kind)\t\(.status)\t\(if .kind=="mr_rework" then "push to PR #\(.mr_iid)" elif .issue_iid!=null then "issue #\(.issue_iid)" else "-" end)"' 2>/dev/null || true)
    if [ -n "$rows" ]; then
      while IFS=$'\t' read -r rid kind st target; do
        [ -z "$rid" ] && continue
        upcoming=$((upcoming+1))
        printf '  %s  %-12s %-18s %s\n' "$rid" "$kind" "$st" "$target"
      done <<< "$rows"
    else
      echo "  (none)"
    fi
  else
    echo "  (repo not connected to uzi, or uzi repo list failed)"
  fi
else
  echo "  (uzi CLI not on PATH)"
fi

echo
echo "CR_IN_PROGRESS=$in_progress"
echo "CR_LIMITED=$limited"
echo "CR_ELIGIBLE_OPEN=$eligible"
echo "UPCOMING_PUSHES=$upcoming"
exit 0
