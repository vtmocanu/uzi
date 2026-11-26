#!/usr/bin/env bash
# Usage: ci-lane.sh grant PR FULL_SHA --owner NAME [--size TEXT] [--blocks TEXT] [--obligation yes|no]
#        ci-lane.sh release PR | list
#        ci-lane.sh plan [--json] [--evidence FILE]
#        ci-lane.sh apply --log FILE [--evidence FILE]
# Evidence is an array of {pr,head,blocks?,red_main_or_release_fix?,conflict_prone?}.
set -uo pipefail
umask 077

repo=vtmocanu/uzi
state=${UZI_LANE_STATE:-"$HOME/.local/state/uzi-lander"}
slot_file="$state/ci-slots.json"
slot_max=${UZI_CI_SLOTS_MAX:-2}
lock="$state/ci-slots.lock"
command=${1:-}; [ "$#" -eq 0 ] || shift
slots='[]'
evidence='[]'
log_file=''
json=0

die() { printf 'ci-lane: %s\n' "$*" >&2; exit 2; }
valid_pr() { [[ "$1" =~ ^[1-9][0-9]{0,8}$ ]]; }
valid_head() { [[ "$1" =~ ^[0-9a-f]{40}$ ]]; }
[[ "$state" = /* && "$slot_max" =~ ^[1-9][0-9]?$ ]] || die 'invalid state path or slot maximum'

load_slots() {
  [ ! -L "$slot_file" ] || return 1
  if [ ! -e "$slot_file" ]; then slots='[]'; return 0; fi
  [ -f "$slot_file" ] || return 1
  slots=$(jq -cse '
    if length==1 and (.[0]|type)=="array" then .[0] else error("slots") end
    | select(all(.[]; type=="object" and (.pr | type=="number" and .==floor and .>0)
        and (.head | type=="string" and test("^[0-9a-f]{40}$"))
        and (.owner | type=="string" and length>0) and (.granted_at|type)=="string"
        and (.size | type=="string" or .==null) and (.blocks | type=="string" or .==null)
        and (.obligation|IN("yes","no",null))) and (map(.pr)|unique|length)==length)
    ' < "$slot_file" 2>/dev/null)
}

take_lock() {
  mkdir -p "$state" && mkdir "$lock" 2>/dev/null || return 1
  trap 'rmdir "$lock" 2>/dev/null || true' EXIT
}
drop_lock() { rmdir "$lock" || die 'cannot release the state lock'; trap - EXIT; }

save_slots() {
  local tmp
  tmp=$(mktemp "$state/.ci-slots.XXXXXX") || return 1
  if ! printf '%s\n' "$slots" > "$tmp" || ! mv -f "$tmp" "$slot_file"; then
    rm -f "$tmp"
    return 1
  fi
}

case "$command" in
  grant)
    pr=${1:-}; head=${2:-}
    valid_pr "$pr" && valid_head "$head" || die 'grant requires a PR and full lowercase SHA'
    shift 2
    owner=''; size=''; blocks=''; obligation=''
    while [ "$#" -gt 0 ]; do
      [ "$#" -ge 2 ] || die "missing $1 value"
      case "$1" in
        --owner) owner=$2 ;; --size) size=$2 ;; --blocks) blocks=$2 ;;
        --obligation) case "$2" in yes|no) obligation=$2 ;; *) die 'obligation must be yes or no' ;; esac ;;
        *) die "unknown grant option $1" ;;
      esac
      shift 2
    done
    [ -n "$owner" ] || die 'grant requires --owner'
    take_lock || die 'state busy or unavailable'
    load_slots || die 'unreadable slots'
    grant_time=$(date -u +%Y-%m-%dT%H:%M:%SZ) || die 'clock unavailable'
    slots=$(jq -ce --argjson pr "$pr" --arg head "$head" --arg owner "$owner" \
      --arg time "$grant_time" --arg size "$size" --arg blocks "$blocks" --arg obligation "$obligation" \
      --argjson cap "$slot_max" '
      def optional: if .=="" then null else . end;
      if any(.[]; .pr==$pr) then error("release the existing grant first")
      elif length >= $cap then error("CI slots full")
      else .+[{pr:$pr,head:$head,owner:$owner,granted_at:$time,size:($size|optional),
               blocks:($blocks|optional),obligation:($obligation|optional)}] end | sort_by(.pr)
      ' <<< "$slots") || die 'grant refused'
    save_slots || die 'cannot save slots'
    drop_lock
    printf 'GRANTED #%s %s\n' "$pr" "$head"
    exit ;;
  release)
    [ "$#" -eq 1 ] && valid_pr "$1" || die 'release requires one PR'
    take_lock || die 'state busy or unavailable'
    load_slots || die 'unreadable slots'
    slots=$(jq -c --argjson pr "$1" 'map(select(.pr!=$pr))' <<< "$slots") || die 'cannot update slots'
    save_slots || die 'cannot save slots'
    drop_lock
    printf 'RELEASED #%s\n' "$1"
    exit ;;
  list)
    [ "$#" -eq 0 ] || die 'list takes no arguments'
    load_slots || die 'unreadable slots'
    jq . <<< "$slots"
    exit ;;
  plan|apply)
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --json) [ "$command" = plan ] || die '--json is for plan'; json=1; shift ;;
        --log)
          [ "$command" = apply ] && [ "$#" -ge 2 ] && [ -n "$2" ] || die 'apply needs a log path'
          log_file=$2; shift 2 ;;
        --evidence)
          [ "$#" -ge 2 ] || die 'missing evidence path'
          evidence=$(jq -cse '
            if length==1 and (.[0]|type)=="array" then .[0] else error("evidence") end
            | select(all(.[]; type=="object" and (.pr | type=="number" and .==floor and .>0)
              and (.head|type=="string" and test("^[0-9a-f]{40}$"))
              and (.blocks==null or (.blocks|type=="array" and all(.[]; type=="string" and length>0)))
              and (.red_main_or_release_fix|type|IN("null","boolean"))
              and (.conflict_prone|type|IN("null","boolean")))
              and (map([.pr,.head])|unique|length)==length)
            ' < "$2") || die 'unreadable or duplicate scheduling evidence'
          shift 2 ;;
        *) die "unknown $command option $1" ;;
      esac
    done
    [ "$command" != apply ] || [ -n "$log_file" ] || die 'apply requires --log'
    ;;
  *) die 'usage: ci-lane.sh grant|release|list|plan|apply' ;;
esac

# An incomplete pagination result is never taken as a zero count.
page_rows() { # endpoint, array field
  local pages
  pages=$(gh api --paginate --slurp "$1" 2>/dev/null) || return 1
  jq -ce --arg field "$2" '
    select(length>0 and all(.[]; type=="object"
      and (.total_count|type=="number" and .==floor and .>=0)
      and (.[$field]|type)=="array"))
    | .[0].total_count as $count
    | select(all(.[]; .total_count==$count))
    | [ .[].[$field][] ] | select(length==$count)
    ' <<< "$pages" 2>/dev/null
}

main_head() {
  local sha
  sha=$(gh api "repos/$repo/git/ref/heads/main" --jq .object.sha 2>/dev/null) && valid_head "$sha" || return 1
  printf '%s\n' "$sha"
}

# Match a published release, rather than guessing from a failed tag lookup.
release_published() {
  local first=$1 tag
  if ! [[ "$first" =~ ^chore\(release\):[[:space:]]+(v[0-9]+\.[0-9]+\.[0-9]+[-.a-zA-Z0-9]*)[[:space:]]*$ ]]; then
    return 1
  fi
  tag=${BASH_REMATCH[1]}
  gh api "repos/$repo/releases/tags/$tag" 2>/dev/null |
    jq -e --arg tag "$tag" '.tag_name==$tag and .draft==false and (.published_at|type=="string" and length>0)' >/dev/null
}

decision() { judgement=$1; reason=$2; }

classify() {
  local run=$1 tip=$2 id head branch workflow workflow_path event first job_rows related run_state
  judgement=UNKNOWN; reason='unclassified workflow or missing evidence'; pr=null; owner=null
  id=$(jq -er '.id | select(type=="number" and .>0 and .==floor)' <<< "$run") || return
  head=$(jq -r '.head_sha // empty' <<< "$run")
  branch=$(jq -r '.head_branch // empty' <<< "$run")
  workflow_path=$(jq -r '.path // empty' <<< "$run")
  case "$workflow_path" in
    .github/workflows/ci.yml) workflow=CI ;;
    .github/workflows/kind-smoke.yml) workflow='KinD Smoke' ;;
    .github/workflows/codex-packaged-proof.yml) workflow='Codex packaged proof' ;;
    dynamic/github-code-scanning/codeql|.github/workflows/codeql.yml) workflow=CodeQL ;;
    *) workflow='' ;;
  esac
  event=$(jq -r '.event // empty' <<< "$run")
  run_state=$(jq -r '.status // empty' <<< "$run")
  case "$run_state" in queued|requested|waiting|pending|in_progress) ;; *) return ;; esac
  valid_head "$head" && [ -n "$branch" ] || return
  if [ "$event" = pull_request ]; then
    pr=$(jq -c '
      .pull_requests
      | if type=="array" and all(.[]; (.number|type=="number" and .>0 and .==floor))
        then map(.number)|unique|if length==1 then .[0] else null end else null end' <<< "$run") || pr=null
    if [ "$pr" != null ]; then
      related=$(jq -c --argjson pr "$pr" 'map(select(.pr==$pr)) | first' <<< "$slots") || return
      owner=$(jq -c .owner <<< "$related") || return
      if jq -e --arg head "$head" '.head==$head' <<< "$related" >/dev/null; then
        decision KEEP 'granted PR head'; return
      fi
    fi
  fi
  case "$workflow" in
    'Codex packaged proof') decision KEEP 'packaged proof'; return ;;
    CodeQL) if [ "$event" = pull_request ] || [ "$branch" != main ]; then decision KEEP 'non-main CodeQL'; return; fi ;;
  esac
  if [ "$branch" = main ] && [ "$event" != pull_request ]; then
    case "$workflow" in
      CI|'KinD Smoke'|CodeQL)
        if [ "$head" = "$tip" ]; then decision KEEP 'current main head'; return; fi ;;
    esac
    first=$(jq -er '.head_commit.message | select(type=="string" and length>0) | split("\n")[0]' <<< "$run") ||
      { decision UNKNOWN 'main commit subject unreadable'; return; }
    if [[ "$first" == 'chore(release)'* ]] && ! release_published "$first"; then
      decision KEEP 'release publication not proven'; return
    fi
  fi

  job_rows=$(page_rows "repos/$repo/actions/runs/$id/jobs?filter=latest&per_page=100" jobs) ||
    { decision UNKNOWN 'jobs unreadable or incomplete'; return; }
  if ! jq -e 'all(.[]; (.name|type=="string" and length>0) and
     (.id|type=="number" and .>0 and .==floor)
     and (.status|IN("queued","requested","waiting","pending","in_progress","completed")))
     and (map(.id)|unique|length)==length' <<< "$job_rows" >/dev/null; then
    decision UNKNOWN 'invalid job record'; return
  fi
  if jq -e 'any(.[]; .name | test("(^|[^A-Za-z])livedb([^A-Za-z]|$)|^test-codex-refresh-lostreply-e2e$";"i"))' <<< "$job_rows" >/dev/null; then
    decision KEEP 'LiveDB job present'; return
  fi
  if jq -e 'length as $total | ([.[]|select(.status=="completed")]|length)*100 > $total*85' <<< "$job_rows" >/dev/null; then
    decision KEEP 'more than 85% complete'; return
  fi
  if [ "$branch" = main ] && [ "$event" != pull_request ]; then
    case "$event" in push|schedule|workflow_dispatch|dynamic) ;; *) return ;; esac
    case "$workflow" in
      CI|'KinD Smoke'|CodeQL)
        if gh api "repos/$repo/compare/$head...$tip" 2>/dev/null |
          jq -e --arg base "$head" '.status=="ahead" and .merge_base_commit.sha==$base' >/dev/null; then
          decision CANCEL 'superseded by a descendant main push'
        else
          decision UNKNOWN 'main ancestry unproven'
        fi ;;
    esac
  elif [ "$event" = pull_request ] && [ "$pr" != null ]; then
    case "$workflow" in
      CI|'KinD Smoke')
        if [ "$related" = null ]; then decision CANCEL 'PR has no slot'
        else decision CANCEL 'head differs from slot'; fi ;;
    esac
  fi
}

open_prs() {
  local raw
  raw=$(gh pr list -R "$repo" --state open -L 101 \
    --json number,headRefOid,mergeable,changedFiles,additions 2>/dev/null) || return 1
  jq -ce --argjson slots "$slots" --argjson ev "$evidence" '
    select(type=="array" and length<101 and all(.[]; (.number|type)=="number"
      and (.headRefOid|type=="string" and test("^[0-9a-f]{40}$"))
      and (.changedFiles|type)=="number" and (.additions|type)=="number"
      and (.mergeable|IN("MERGEABLE","CONFLICTING","UNKNOWN"))))
    | [ .[] | select(.mergeable=="MERGEABLE") | . as $p
       | select(all($slots[]; .pr!=$p.number))
       | ([$ev[] | select(.pr==$p.number and .head==$p.headRefOid)] | first) as $e
       | {pr:.number,head:.headRefOid,files:.changedFiles,additions:.additions,
          blocks:($e.blocks | if .==null then null else unique end),
          red_main_or_release_fix:$e.red_main_or_release_fix,conflict_prone:$e.conflict_prone} ]
    | sort_by((if .red_main_or_release_fix==true then 0 elif ((.blocks // [])|length)>0 then 1
                else 2 end), -((.blocks // [])|length),
              (if .conflict_prone==true then 0 else 1 end), -.files, -.additions, .pr)
    ' <<< "$raw" 2>/dev/null
}

make_plan() {
  local tip active='' page list run row rows='' run_state proposals end
  load_slots || die 'unreadable slots'
  tip=$(main_head) || die 'cannot pin main'
  for run_state in queued in_progress waiting requested pending; do
    page=$(page_rows "repos/$repo/actions/runs?status=$run_state&per_page=100" workflow_runs) ||
      die "incomplete $run_state run listing"
    active+="$page"$'\n'
  done
  list=$(jq -cse '
    add | select(all(.[]; type=="object" and (.id|type=="number" and .>0 and .==floor)))
    | group_by(.id) | map(if length==1 then .[0] else .[0]+{unstable:true} end)
    ' <<< "$active") || die 'invalid workflow run list'
  while IFS= read -r run; do
    if jq -e '.unstable' <<< "$run" >/dev/null; then
      decision UNKNOWN 'run moved between status listings'; pr=null; owner=null
    else
      classify "$run" "$tip"
    fi
    row=$(jq -c --arg result "$judgement" --arg reason "$reason" --argjson pr "$pr" --argjson owner "$owner" '
      {id,attempt:.run_attempt,head:.head_sha,branch:.head_branch,workflow:.name,status,pr:$pr,
       owner:$owner,decision:$result,reason:$reason}' <<< "$run") || die 'cannot format run decision'
    rows+="$row"$'\n'
  done < <(jq -c '.[]' <<< "$list")
  proposals=$(open_prs) || proposals=null
  end=$(main_head) || die 'cannot recheck main'
  [ "$end" = "$tip" ] || die 'main moved during planning; retry'
  jq -s --arg main "$tip" --argjson slots "$slots" --argjson max "$slot_max" --argjson next "$proposals" '
    {main:$main,slots:$slots,available_slots:([0,($max-($slots|length))]|max),
     runs:.,next_slot_candidates:$next}' <<< "$rows"
}

if [ "$command" = plan ]; then
  plan=$(make_plan) || exit $?
  if [ "$json" -eq 1 ]; then printf '%s\n' "$plan"; else
    jq -r '.runs[] | [.decision,.id,.workflow,.branch,.head,.reason] | @tsv' <<< "$plan"
    jq -r '"available slots: \(.available_slots)", (.next_slot_candidates |
      if .==null then "next slots: UNKNOWN" else .[] | "next slot: #\(.pr) \(.head) size: \(.files)/+\(.additions); blocks: \(
        if .blocks==null then "unknown" elif (.blocks|length)==0 then "none" else .blocks|join(", ") end)" end)' <<< "$plan"
  fi
  exit
fi

: >> "$log_file" || die 'cannot open cancellation log'
plan=$(make_plan) || exit $?
while IFS= read -r candidate; do
  id=$(jq -r .id <<< "$candidate")
  take_lock || die 'state busy or unavailable'
  load_slots || die 'unreadable slots'
  tip=$(main_head) || die 'cannot recheck main'
  live=$(gh api "repos/$repo/actions/runs/$id" 2>/dev/null) || die "cannot refresh run $id"
  if ! jq -e --argjson expected "$candidate" '
    .id==$expected.id and .head_sha==$expected.head and .run_attempt==$expected.attempt
    and (.run_attempt|type=="number") and .status!="completed"' <<< "$live" >/dev/null; then
    printf 'SKIP run %s: attempt/head changed or already complete\n' "$id"
    drop_lock
    continue
  fi
  classify "$live" "$tip"
  planned_pr=$(jq -c .pr <<< "$candidate")
  if [ "$pr" != "$planned_pr" ]; then decision UNKNOWN 'PR association changed'; fi
  if [ "$judgement" != CANCEL ]; then
    printf 'SKIP run %s: %s (%s)\n' "$id" "$judgement" "$reason"
    drop_lock
    continue
  fi
  verified=0
  for attempt in 1 2; do
    # The second request still needs a fresh eligible, unretargeted attempt.
    if [ "$attempt" -eq 2 ]; then
      tip=$(main_head) || die 'cannot recheck main'
      live=$(gh api "repos/$repo/actions/runs/$id" 2>/dev/null) || die "cannot refresh run $id"
      if jq -e --argjson expected "$candidate" '
          .id==$expected.id and .head_sha==$expected.head and .run_attempt==$expected.attempt
          and .status=="completed" and .conclusion=="cancelled"' <<< "$live" >/dev/null; then
        verified=1; break
      fi
      jq -e --argjson expected "$candidate" '
        .id==$expected.id and .head_sha==$expected.head and .run_attempt==$expected.attempt and .status!="completed"
        ' <<< "$live" >/dev/null || break
      classify "$live" "$tip"
      [ "$judgement" = CANCEL ] && [ "$pr" = "$planned_pr" ] || break
    fi
    gh api -X POST "repos/$repo/actions/runs/$id/force-cancel" >/dev/null 2>&1 || true
    for tick in 1 2 3; do
      if gh api "repos/$repo/actions/runs/$id" 2>/dev/null |
        jq -e --argjson expected "$candidate" '
          .id==$expected.id and .head_sha==$expected.head and .run_attempt==$expected.attempt
          and .status=="completed" and .conclusion=="cancelled"' >/dev/null; then
        verified=1; break
      fi
      [ "$tick" -eq 3 ] || sleep 2
    done
    [ "$verified" -eq 0 ] || break
  done
  at=$(date -u +%Y-%m-%dT%H:%M:%SZ) || die 'clock unavailable'
  jq -c --arg at "$at" --argjson ok "$verified" --argjson owner "$owner" '
    {at:$at,result:(if $ok==1 then "cancelled" else "unverified" end),pr,branch,head,id,owner:$owner}
    ' <<< "$candidate" >> "$log_file" || die 'cannot append cancellation log'
  [ "$verified" -eq 1 ] || die "force-cancel of run $id was not verified"
  jq -r --argjson owner "$owner" '"NOTIFY pr=\(.pr // "main") branch=\(.branch) run=\(.id) owner=\($owner // "unknown")"' <<< "$candidate"
  drop_lock
done < <(jq -c '.runs[] | select(.decision=="CANCEL")' <<< "$plan")
