#!/usr/bin/env bash
# Read-only snapshot, never a merge grant. JSON null means unknown.
# Usage: fast-batch.sh [--evidence FILE] [--active-jobs N --peak-jobs N] [PR ...]
# PR arguments give the lane owner's proposed order. FILE is a JSON array of
# {pr, head, review, trust_boundary}; review is a receipt for an approval of head.
# Supply account-wide active jobs and a conservative additional peak per push.
set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd) || exit 2
# shellcheck source=lib/required-checks.sh
. "$HERE/lib/required-checks.sh"
# shellcheck source=lib/ci-fix.sh
. "$HERE/lib/ci-fix.sh"
# shellcheck source=lib/migrations.sh
. "$HERE/lib/migrations.sh"

repo=vtmocanu/uzi
order='[]'
evidence='[]'
active=null
peak=null
die() { printf 'fast-batch: %s\n' "$*" >&2; exit 2; }

while [ "$#" -gt 0 ]; do
  case "$1" in
    --evidence)
      [ "$#" -ge 2 ] || die 'missing evidence file'
      evidence=$(jq -cse 'if length == 1 and (.[0]|type) == "array" then .[0] else error("evidence") end
        | if all(.[]; (.pr|type)=="number" and (.head|type)=="string"
            and (.head|test("^[a-f0-9]{40}$"))
            and (.review==null or ((.review|type)=="string" and (.review|length)>0))
            and (.trust_boundary==null or (.trust_boundary|type)=="boolean"))
            and (map([.pr,.head])|unique|length)==length then . else error("evidence") end' < "$2") ||
        die 'unreadable or ambiguous review evidence'
      shift ;;
    --active-jobs|--peak-jobs)
      [ "$#" -ge 2 ] && [[ "$2" =~ ^(0|[1-9][0-9]?)$ ]] || die "invalid $1"
      if [ "$1" = --active-jobs ]; then active=$2; else
        [ "$2" -gt 0 ] || die 'peak must be positive'
        peak=$2
      fi
      shift ;;
    *)
      [[ "$1" =~ ^[1-9][0-9]*$ ]] || die "invalid PR number: $1"
      order=$(jq -ce --argjson n "$1" 'if index($n) then error("duplicate PR") else .+[$n] end' <<< "$order") ||
        die 'duplicate PR in landing order' ;;
  esac
  shift
done

read_main() {
  gh api "repos/$repo/git/ref/heads/main" --jq .object.sha |
    jq -Rse 'rtrimstr("\n") | select(test("^[a-f0-9]{40}$"))' -r
}

main=$(read_main 2>/dev/null) || main=null
base=null
if [ "$main" != null ]; then
  base=$(gh api "repos/$repo/contents/api/internal/store/migrations?ref=$main" 2>/dev/null) || base=null
fi
base_info=$(migration_report '[]' "$base") || base_info=null
next=$(jq 'if .==null then null else .head+1 end' <<< "$base_info")
required=$(required_contexts "$repo" main) || required=null
claims=$("$HERE/claims.sh" list --json) || claims=null
repos=$(uzi repo list --json 2>/dev/null) || repos=null
repo_id=$(jq -er --arg repo "$repo" '[.[] | select(.path_with_namespace==$repo) | .id]
  | select(length==1) | first | select(type=="string" and length>0)' <<< "$repos" 2>/dev/null) || repo_id=""

prs=$(gh pr list --repo "$repo" --state open --limit 101 --json number,headRefOid,headRefName,baseRefName 2>/dev/null) ||
  die 'open PRs unknown'
prs=$(jq -ce --argjson order "$order" '
  if type=="array" and length<101 and all(.[]; (.number|type)=="number"
    and (.headRefOid|test("^[a-f0-9]{40}$")) and (.headRefName|type)=="string"
    and (.baseRefName|type)=="string") then . else error("incomplete PR list") end
  | sort_by(.number as $n | $order|index($n) // 1000000)' <<< "$prs") || die 'open PR list is incomplete'
jq -e --argjson order "$order" '[.[].number] as $seen | all($order[]; . as $n | $seen | index($n)!=null)'   <<< "$prs" >/dev/null || die 'an ordered PR is absent from the open list'

# Use the latest run's job IDs, with current-attempt completed-success evidence.
livedb() {
  local head=$1 pr_no=$2 run jobs name id line result='{}'
  run=$(gh run list --repo "$repo" --workflow ci.yml --event pull_request --commit "$head" --limit 1     --json databaseId,headSha,conclusion --jq '.[0]' 2>/dev/null) || return 1
  id=$(jq -er --arg head "$head" 'select(.headSha==$head and .conclusion=="success") | .databaseId |
    select(type=="number" and .>0 and .==floor)' <<< "$run") || return 1
  run=$(gh api "repos/$repo/actions/runs/$id" 2>/dev/null) || return 1
  run=$(jq -er --arg head "$head" --argjson pr "$pr_no" '
    select(.head_sha==$head and .event=="pull_request" and .conclusion=="success"
      and any(.pull_requests[]; .number==$pr and .head.sha==$head and .base.ref=="main"))
    | .run_attempt | select(type=="number" and .>0 and .==floor)' <<< "$run") || return 1
  jobs=$(gh api --paginate --slurp "repos/$repo/actions/runs/$id/attempts/$run/jobs?per_page=100" 2>/dev/null) || return 1
  for name in test-livedb-workersvc test-codex-refresh-lostreply-e2e; do
    id=$(jq -er --arg name "$name" '
      [.[] | .jobs[] | select(.name==$name)] | select(length==1)
      | .[0] | select(.status=="completed" and .conclusion=="success") | .id
      | select(type=="number" and .>0 and .==floor)' <<< "$jobs") || id=""
    line=null
    if [ -n "$id" ]; then
      # Read the entire log so a partial download cannot certify a match.
      line=$(gh api "repos/$repo/actions/jobs/$id/logs" 2>/dev/null |
        awk 'match($0,/LiveDB: [1-9][0-9]* passed, 0 skipped([[:space:]]|$)/) { text=substr($0,RSTART,RLENGTH); sub(/[[:space:]]+$/, "", text) }
          END { if (text=="") exit 1; print text }' | jq -R) || line=null
    fi
    result=$(jq -c --arg name "$name" --argjson line "$line" '.+{($name):$line}' <<< "$result") || return 1
  done
  printf '%s\n' "$result"
}

rows=''
while IFS= read -r pr; do
  pr_no=$(jq -r .number <<< "$pr")
  head=$(jq -r .headRefOid <<< "$pr")
  branch=$(jq -r .headRefName <<< "$pr")
  target=$(jq -r .baseRefName <<< "$pr")
  reviewed=$(jq -c --argjson n "$pr_no" --arg head "$head" 'map(select(.pr==$n and .head==$head)) | first' <<< "$evidence")
  claim=$(jq -c --arg key "#$pr_no" 'if type=="array" then map(select(.key==$key) | {owner,live,depends_on}) | first else null end' <<< "$claims")
  ci=unknown
  checks=null
  if [ "$target" = main ] && [ "$required" != null ]; then
    rc=0
    checks=$(pr_ci_checks "$repo" "$pr_no" "$required" "$head" "$target" 2>/dev/null) || rc=$?
    if { [ "$rc" -eq 0 ] || [ "$rc" -eq 1 ] || [ "$rc" -eq 8 ]; } && ci_checks_valid "$checks"; then
      missing=$(missing_required "$required" "$checks") || missing=null
      ci=$(jq -r --argjson missing "$missing" '
        if any(.[]; .bucket=="fail") then "red"
        elif $missing==null then "unknown"
        elif $missing>0 or any(.[]; .bucket=="pending" or .bucket=="cancel") then "pending"
        elif any(.[]; .bucket=="pass") then "green" else "unknown" end' <<< "$checks")
    else checks=null
    fi
  fi
  writers=null
  visibility=null
  if [ -n "$repo_id" ] && branch_writers_read "$repo_id" "$branch" "$pr_no"; then
    writers=$WRITERS
    visibility=$(jq -cn --arg value "$WRITERS_VISIBILITY" '$value')
  fi
  migration=null
  proofs=null
  slot=null
  ordered=$(jq -r --argjson n "$pr_no" 'index($n)!=null' <<< "$order")
  if [ "$target" = main ] && [ "$base_info" != null ]; then
    if files=$(migration_pr_files "$repo" "$pr_no") && migration=$(migration_report "$files" "$base"); then
      count=$(jq '.added|length' <<< "$migration")
      if [ "$count" -gt 0 ]; then
        if [ "$ordered" = true ] && [ "$next" != null ]; then
          slot=$(jq -cn --argjson first "$next" --argjson count "$count" '{first:$first,last:($first+$count-1)}')
          next=$((next + count))
        fi
        [ "$ci" != green ] || proofs=$(livedb "$head" "$pr_no") || proofs=null
      fi
    else
      migration=null
    fi
  fi
  if [ "$ordered" = true ] && [ "$migration" = null ]; then next=null; fi
  # Detect a head/base change during the GitHub reads. Discard stale evidence.
  pv=$(gh pr view "$pr_no" --repo "$repo" --json state,headRefOid,baseRefName,mergeable 2>/dev/null) || pv=null
  mergeable=$(jq -er --arg head "$head" --arg base "$target" '
    select(.state=="OPEN" and .headRefOid==$head and .baseRefName==$base)
    | .mergeable | select(.=="MERGEABLE" or .=="CONFLICTING")' <<< "$pv") || mergeable=unknown
  if ! jq -e --arg head "$head" --arg base "$target" '.state=="OPEN" and .headRefOid==$head and .baseRefName==$base'     <<< "$pv" >/dev/null; then
    ci=unknown; checks=null; reviewed=null; migration=null; proofs=null; slot=null; writers=null; visibility=null
    if [ "$ordered" = true ]; then next=null; fi
  fi
  row=$(jq -c --arg ci "$ci" --arg mergeable "$mergeable" --argjson checks "$checks" --argjson evidence "$reviewed"     --argjson claim "$claim" --argjson writers "$writers" --argjson visibility "$visibility" --argjson migration "$migration"     --argjson proofs "$proofs" --argjson slot "$slot" --argjson ordered "$ordered" '
    . + {ci:$ci,required_checks:$checks,mergeable:$mergeable,review:$evidence.review,
         trust_boundary:$evidence.trust_boundary,claim:$claim,writers:$writers,writer_visibility:$visibility,
         migrations:$migration,livedb:$proofs,proposed_slot:$slot,ordered:$ordered}' <<< "$pr") || die "cannot format PR #$pr_no"
  rows+="$row"$'\n'
done < <(jq -c '.[]' <<< "$prs")

end_main=$(read_main 2>/dev/null) || end_main=null
jq -s --arg main "$main" --arg end "$end_main" --argjson base "$base_info"   --argjson active "$active" --argjson peak "$peak" --argjson order "$order" --argjson claims "$claims" '
  {main:(if $main=="null" then null else $main end), main_unchanged:($main!="null" and $main==$end),
   migration_head:$base.head, proposed_order:$order, claims_readable:($claims|type=="array"), job_budget:{cap:20,active:$active,peak_per_push:$peak,
     max_pushes:(if $active==null or $peak==null or $main=="null" or $main!=$end then null
                 else ([0,((20-$active)/$peak|floor)]|max) end)}, prs:.}
  | if .main_unchanged then . else .prs |= map(.proposed_slot=null) end' <<< "$rows"
