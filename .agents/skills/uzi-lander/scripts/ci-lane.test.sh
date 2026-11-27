#!/usr/bin/env bash
# Hermetic recorded-shape GitHub run, job, PR and cancellation fixtures.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir "$WORK/bin"
export PATH="$WORK/bin:$PATH" UZI_LANE_STATE="$WORK/slots" WORK
export TIP=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export OLD=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
export SLOT=cccccccccccccccccccccccccccccccccccccccc
export UZI_CI_SLOTS_MAX=2
export CALLS="$WORK/calls"
: > "$CALLS"

jq -cn --arg tip "$TIP" --arg old "$OLD" --arg slot "$SLOT" --arg recent "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '
  def run($id;$work;$branch;$event;$head;$pr;$message):
    {id:$id,name:(if $work=="CodeQL" then "Dynamic CodeQL display title" else $work end),
     path:(if $work=="CI" then ".github/workflows/ci.yml"
           elif $work=="KinD Smoke" then ".github/workflows/kind-smoke.yml"
           elif $work=="CodeQL" then "dynamic/github-code-scanning/codeql"
           elif $work=="Codex packaged proof" then ".github/workflows/codex-packaged-proof.yml"
           else ".github/workflows/unknown.yml" end),
     head_branch:$branch,event:$event,head_sha:$head,run_attempt:1,status:"queued",created_at:$recent,
     head_commit:{message:$message},pull_requests:(if $pr==null then [] else [{number:$pr}] end)};
  [
    run(101;"CI";"main";"push";$tip;null;"current"),
    run(102;"CI";"main";"push";$old;null;"ordinary"),
    run(103;"CI";"main";"push";$old;null;"chore(release): v1.2.3-rc.1"),
    run(104;"KinD Smoke";"main";"push";$old;null;"ordinary"),
    run(105;"CI";"fix/unslotted";"pull_request";$old;15;"ordinary"),
    run(106;"KinD Smoke";"fix/wrong-head";"pull_request";$old;16;"ordinary"),
    run(107;"CI";"fix/correct-head";"pull_request";$slot;16;"ordinary"),
    run(108;"CodeQL";"fix/untouched";"dynamic";$old;18;"ordinary"),
    run(109;"Codex packaged proof";"fix/proof";"pull_request";$old;19;"ordinary"),
    run(110;"CI";"fix/live-db";"pull_request";$old;20;"ordinary"),
    run(111;"Dynamic job";"main";"workflow_dispatch";$old;null;"ordinary"),
    run(112;"CI";"fix/unknown-pr";"pull_request";$old;null;"ordinary"),
    run(113;"CI";"main";"push";$slot;null;"unproven ancestry"),
    run(114;"CI";"fix/unknown-jobs";"pull_request";$old;23;"ordinary"),
    run(115;"CodeQL";"main";"dynamic";$tip;null;"current"),
    run(116;"CI";"main";"push";$old;null;"chore(release): v1.2.3-rc.2"),
    run(117;"CI";"main";"unknown";$old;null;"unknown event"),
    run(118;"CI";"fix/near-done-unslotted";"pull_request";$old;28;"ordinary"),
    run(119;"CI";"fix/stale";"pull_request";$old;29;"ordinary"),
    run(120;"E2E";"main";"workflow_dispatch";$old;null;"ordinary"),
    run(121;"CI";"fix/relied-on";"pull_request";$old;30;"ordinary"),
    run(122;"CI";"main";"push";$old;null;"ordinary"),
    run(123;"KinD Smoke";"main";"push";$old;null;"ordinary"),
    run(124;"KinD Smoke";"main";"push";$tip;null;"current")
  ] | map(if .id==107 or .id==110 or .id==118 or .id==121 or .id==122 or .id==123 then .status="in_progress"
          elif .id==114 or .id==119 or .id==120 then .created_at="2020-01-01T00:00:00Z" else . end)' > "$WORK/runs"
jq -nc --arg head "$TIP" '
  [ {number:31,changedFiles:3,additions:5}, {number:32,changedFiles:1,additions:2},
    {number:33,changedFiles:25,additions:200}, {number:34,changedFiles:7,additions:4},
    {number:35,changedFiles:2,additions:4}, {number:16,changedFiles:60,additions:100}]
  | map(.+{headRefOid:$head,mergeable:"MERGEABLE"})' > "$WORK/prs"
jq -nc --arg head "$TIP" '
  [{pr:31,head:$head,blocks:["#4000"]},
   {pr:32,head:$head,blocks:["#4001","#4002"]},
   {pr:34,head:$head,red_main_or_release_fix:true},
   {pr:35,head:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",red_main_or_release_fix:true},
   {pr:30,head:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",relied_on:true}]' > "$WORK/evidence"

cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$CALLS"
case "$*" in
  'pr list '*) cat "$WORK/prs"; exit ;;
  *'/git/ref/heads/main '*) printf '%s\n' "$TIP"; exit ;;
  *"/actions/runs?branch=main&head_sha=$TIP&per_page=100")
    mode=${TIP_RUNS:-normal}
    if [ "$mode" = lost-on-apply ] && grep -q '/actions/runs/102$' "$CALLS"; then
      mode=cancelled-ci
    fi
    jq -c --arg head "$TIP" --arg mode "$mode" '
      map(select(.head_sha==$head))
      | if $mode=="no-ci" then map(select(.id!=101))
        elif $mode=="cancelled-ci" then map(if .id==101 then .status="completed"|.conclusion="cancelled" else . end)
        else . end
      | [{total_count:length,workflow_runs:.}]' "$WORK/runs"
    exit ;;
  *'/actions/runs?status='*)
    endpoint=${!#}
    run_state=${endpoint#*status=}; run_state=${run_state%%&*}
    if [ "$run_state" = queued ] && { [ "${TORN_LIST:-0}" = permanent ] ||
         { [ "${TORN_LIST:-0}" = once ] && [ ! -e "$WORK/torn-once" ]; }; }; then
      touch "$WORK/torn-once"
      echo '[{"total_count":1,"workflow_runs":[]}]'
    else
      jq -c --arg run_state "$run_state" --arg mode "${TIP_RUNS:-normal}" '
        map(select(.status==$run_state and (($mode|IN("lost-on-apply","started-on-apply")|not) or .id==102)))
        | [{total_count:length,workflow_runs:.}]' "$WORK/runs"
    fi
    exit ;;
  *'/releases/tags/v1.2.3-rc.1') exit 1 ;;
  *'/releases/tags/v1.2.3-rc.2') echo '{"tag_name":"v1.2.3-rc.2","draft":false,"published_at":"2026-11-26T00:00:00Z"}'; exit ;;
  *"/compare/$OLD...$TIP") jq -cn --arg sha "$OLD" '{status:"ahead",merge_base_commit:{sha:$sha}}'; exit ;;
  *"/compare/$SLOT...$TIP") echo '{"status":"diverged","merge_base_commit":{}}'; exit ;;
  *'/jobs?filter=latest&per_page=100')
    endpoint=${!#}
    id=${endpoint#*/actions/runs/}; id=${id%%/*}
    case "$id" in
      104) jq -cn '[{total_count:7,jobs:[range(7)|{id:(. + 1),name:"job",status:(if .<6 then "completed" else "in_progress" end)}]}]' ;;
      107|110|121|122) echo '[{"total_count":1,"jobs":[{"id":1,"name":"test-codex-refresh-lostreply-e2e","status":"queued"}]}]' ;;
      114) exit 1 ;;
      118) jq -cn '[{total_count:20,jobs:[range(20)|{id:(. + 1),name:"job",status:(if .<18 then "completed" else "in_progress" end)}]}]' ;;
      *) echo '[{"total_count":0,"jobs":[]}]' ;;
    esac
    exit ;;
  *'/force-cancel')
    endpoint=${!#}
    id=${endpoint#*/actions/runs/}; id=${id%%/*}
    case "$id" in 102|105|106|110|116|118|122|123) ;; *) exit 99 ;; esac
    [ "${NO_VERIFY:-0}" = 1 ] || touch "$WORK/cancelled-$id"
    : ;;
  *'/actions/runs/'*)
    endpoint=${!#}
    id=${endpoint##*/}
    if [ -e "$WORK/cancelled-$id" ]; then
      jq -ce --argjson id "$id" '.[]|select(.id==$id)|.status="completed"|.conclusion="cancelled"' "$WORK/runs"
    elif [ "${TIP_RUNS:-normal}" = started-on-apply ] && [ "$id" = 102 ]; then
      jq -ce --argjson id "$id" '.[]|select(.id==$id)|.status="in_progress"' "$WORK/runs"
    elif [ "${MOVED_ATTEMPT:-0}" = 1 ] && [ "$id" = 102 ]; then
      jq -ce --argjson id "$id" '.[]|select(.id==$id)|.run_attempt=2' "$WORK/runs"
    else jq -ce --argjson id "$id" '.[]|select(.id==$id)' "$WORK/runs"; fi ;;
  *) echo "unexpected gh: $*" >&2; exit 99 ;;
esac
STUB
cat > "$WORK/bin/sleep" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$WORK/bin/gh" "$WORK/bin/sleep"
lane="$HERE/ci-lane.sh"
"$lane" list | jq -e 'length==0' >/dev/null
[ ! -e "$UZI_LANE_STATE" ]
"$lane" grant 16 "$SLOT" --owner session-one --size 3/+4 --blocks '#17' --obligation yes
"$lane" grant 17 "$TIP" --owner session-two
if "$lane" grant 99 "$TIP" --owner unexpected; then echo 'FAILED: exceeded slot cap' >&2; exit 1; fi
"$lane" list | jq -e 'map(.pr)==[16,17] and .[0].obligation=="yes" and .[0].blocks=="#17"' >/dev/null
"$lane" release 17

"$lane" plan --json --evidence "$WORK/evidence" > "$WORK/plan"
jq -e '[.runs[]|select(.id==122 or .id==123)|.decision] == ["KEEP","KEEP"]' "$WORK/plan" >/dev/null ||
  { echo 'FAILED: plan must keep running main-push CI and KinD despite queued replacements' >&2; exit 1; }
jq -e '
  .available_slots==1
  and ([.runs[]|select(.decision=="CANCEL")|.id] == [102,105,106,110,116,118])
  and ([.runs[]|select(.decision=="KEEP")|.id] == [101,103,104,107,108,109,115,121,122,123,124])
  and ([.runs[]|select(.decision=="UNKNOWN")|.id] == [111,112,113,114,117])
  and ([.stale_runs[].id]==[119,120])
  and ([.next_slot_candidates[].pr] == [34,32,31,33,35])
  and (.next_slot_candidates[-1].blocks==null)
' "$WORK/plan" >/dev/null || { cat "$WORK/plan" >&2; exit 1; }
! grep -q -- 'force-cancel' "$CALLS"

for mode in no-ci cancelled-ci; do
  TIP_RUNS=$mode "$lane" plan --json > "$WORK/tip-plan"
  jq -e '[.runs[] | select(.id==102) | .decision] == ["KEEP"]' "$WORK/tip-plan" >/dev/null ||
    { echo "FAILED: main CI cancelled with tip fixture $mode" >&2; exit 1; }
done

: > "$CALLS"
TIP_RUNS=lost-on-apply "$lane" apply --log "$WORK/lost-replacement" > "$WORK/lost-replacement.out"
grep -qF 'SKIP run 102: KEEP' "$WORK/lost-replacement.out"
! grep -q -- 'force-cancel' "$CALLS"

: > "$CALLS"
TIP_RUNS=started-on-apply "$lane" apply --log "$WORK/started-replacement" > "$WORK/started-replacement.out"
grep -qF 'SKIP run 102: KEEP' "$WORK/started-replacement.out"
! grep -q -- 'force-cancel' "$CALLS"
[ ! -s "$WORK/started-replacement" ]

"$lane" plan --evidence "$WORK/evidence" > "$WORK/human"
grep -Fq 'size: 1/+2; blocks: #4001, #4002' "$WORK/human"

grep -q '^STALE (queued over 24h' "$WORK/human"
: > "$CALLS"
TORN_LIST=once "$lane" plan --json > "$WORK/recovered"
[ "$(grep -cF 'actions/runs?status=queued&per_page=100' "$CALLS")" = 2 ]
: > "$CALLS"
if TORN_LIST=permanent "$lane" apply --log "$WORK/torn.log"; then echo 'FAILED: accepted a torn listing' >&2; exit 1; fi
[ "$(grep -cF 'actions/runs?status=queued&per_page=100' "$CALLS")" = 4 ]
! grep -q -- 'force-cancel' "$CALLS"
if NO_VERIFY=1 "$lane" apply --evidence "$WORK/evidence" --log "$WORK/unverified"; then echo 'FAILED: accepted an unverified cancel' >&2; exit 1; fi
[ "$(grep -c -- '/102/force-cancel' "$CALLS")" = 2 ]
jq -es 'length==1 and .[0].id==102 and .[0].result=="unverified"' "$WORK/unverified" >/dev/null

: > "$CALLS"
MOVED_ATTEMPT=1 "$lane" apply --evidence "$WORK/evidence" --log "$WORK/verified" > "$WORK/notify"
! grep -q -- '/102/force-cancel' "$CALLS"
[ "$(grep -c -- 'force-cancel' "$CALLS")" = 5 ]
jq -es 'map(.id)==[105,106,110,116,118] and all(.[]; .result=="cancelled" and (.at|endswith("Z")))' "$WORK/verified" >/dev/null
grep -qF 'NOTIFY pr=16 branch=fix/wrong-head run=106 owner=session-one' "$WORK/notify"
echo 'PASS ci-lane: slot protection, stale runs, main replacements, read retry, force-cancel verification and attempt change'
