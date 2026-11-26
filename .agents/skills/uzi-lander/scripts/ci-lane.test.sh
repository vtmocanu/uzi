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

jq -cn --arg tip "$TIP" --arg old "$OLD" --arg slot "$SLOT" '
  def run($id;$work;$branch;$event;$head;$pr;$message):
    {id:$id,name:(if $work=="CodeQL" then "Dynamic CodeQL display title" else $work end),
     path:(if $work=="CI" then ".github/workflows/ci.yml"
           elif $work=="KinD Smoke" then ".github/workflows/kind-smoke.yml"
           elif $work=="CodeQL" then "dynamic/github-code-scanning/codeql"
           elif $work=="Codex packaged proof" then ".github/workflows/codex-packaged-proof.yml"
           else ".github/workflows/unknown.yml" end),
     head_branch:$branch,event:$event,head_sha:$head,run_attempt:1,status:"queued",
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
    run(118;"CI";"fix/at-threshold";"pull_request";$old;28;"ordinary")
  ]' > "$WORK/runs"
jq -nc --arg head "$TIP" '
  [ {number:31,changedFiles:3,additions:5}, {number:32,changedFiles:1,additions:2},
    {number:33,changedFiles:25,additions:200}, {number:34,changedFiles:7,additions:4},
    {number:35,changedFiles:2,additions:4}, {number:16,changedFiles:60,additions:100}]
  | map(.+{headRefOid:$head,mergeable:"MERGEABLE"})' > "$WORK/prs"
jq -nc --arg head "$TIP" '
  [{pr:31,head:$head,blocks:["#4000"]},
   {pr:32,head:$head,blocks:["#4001","#4002"]},
   {pr:34,head:$head,red_main_or_release_fix:true},
   {pr:35,head:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",red_main_or_release_fix:true}]' > "$WORK/evidence"

cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$CALLS"
case "$*" in
  'pr list '*) cat "$WORK/prs"; exit ;;
  *'/git/ref/heads/main '*) printf '%s\n' "$TIP"; exit ;;
  *'/actions/runs?status=queued&per_page=100')
    if [ "${TORN_LIST:-0}" -eq 1 ]; then echo '[{"total_count":1,"workflow_runs":[]}]'
    else jq -c '[{total_count:length,workflow_runs:.}]' "$WORK/runs"; fi
    exit ;;
  *'/actions/runs?status='*) echo '[{"total_count":0,"workflow_runs":[]}]'; exit ;;
  *'/releases/tags/v1.2.3-rc.1') exit 1 ;;
  *'/releases/tags/v1.2.3-rc.2') echo '{"tag_name":"v1.2.3-rc.2","draft":false,"published_at":"2026-11-26T00:00:00Z"}'; exit ;;
  *"/compare/$OLD...$TIP") jq -cn --arg sha "$OLD" '{status:"ahead",merge_base_commit:{sha:$sha}}'; exit ;;
  *"/compare/$SLOT...$TIP") echo '{"status":"diverged","merge_base_commit":{}}'; exit ;;
  *'/jobs?filter=latest&per_page=100')
    endpoint=${!#}
    id=${endpoint#*/actions/runs/}; id=${id%%/*}
    case "$id" in
      104) jq -cn '[{total_count:7,jobs:[range(7)|{id:(. + 1),name:"job",status:(if .<6 then "completed" else "in_progress" end)}]}]' ;;
      110) echo '[{"total_count":1,"jobs":[{"id":1,"name":"test-codex-refresh-lostreply-e2e","status":"queued"}]}]' ;;
      114) exit 1 ;;
      118) jq -cn '[{total_count:20,jobs:[range(20)|{id:(. + 1),name:"job",status:(if .<17 then "completed" else "in_progress" end)}]}]' ;;
      *) echo '[{"total_count":0,"jobs":[]}]' ;;
    esac
    exit ;;
  *'/force-cancel')
    endpoint=${!#}
    id=${endpoint#*/actions/runs/}; id=${id%%/*}
    case "$id" in 102|105|106|116|118) ;; *) exit 99 ;; esac
    [ "${NO_VERIFY:-0}" = 1 ] || touch "$WORK/cancelled-$id"
    : ;;
  *'/actions/runs/'*)
    endpoint=${!#}
    id=${endpoint##*/}
    if [ -e "$WORK/cancelled-$id" ]; then
      jq -ce --argjson id "$id" '.[]|select(.id==$id)|.status="completed"|.conclusion="cancelled"' "$WORK/runs"
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
jq -e '
  .available_slots==1
  and ([.runs[]|select(.decision=="CANCEL")|.id] == [102,105,106,116,118])
  and ([.runs[]|select(.decision=="KEEP")|.id] == [101,103,104,107,108,109,110,115])
  and ([.runs[]|select(.decision=="UNKNOWN")|.id] == [111,112,113,114,117])
  and ([.next_slot_candidates[].pr] == [34,32,31,33,35])
  and (.next_slot_candidates[-1].blocks==null)
' "$WORK/plan" >/dev/null || { cat "$WORK/plan" >&2; exit 1; }
! grep -q -- 'force-cancel' "$CALLS"
"$lane" plan --evidence "$WORK/evidence" > "$WORK/human"
grep -Fq 'size: 1/+2; blocks: #4001, #4002' "$WORK/human"

if TORN_LIST=1 "$lane" apply --log "$WORK/torn.log"; then echo 'FAILED: accepted a torn listing' >&2; exit 1; fi
! grep -q -- 'force-cancel' "$CALLS"
if NO_VERIFY=1 "$lane" apply --log "$WORK/unverified"; then echo 'FAILED: accepted an unverified cancel' >&2; exit 1; fi
[ "$(grep -c -- '/102/force-cancel' "$CALLS")" = 2 ]
jq -es 'length==1 and .[0].id==102 and .[0].result=="unverified"' "$WORK/unverified" >/dev/null

: > "$CALLS"
MOVED_ATTEMPT=1 "$lane" apply --log "$WORK/verified" > "$WORK/notify"
! grep -q -- '/102/force-cancel' "$CALLS"
[ "$(grep -c -- 'force-cancel' "$CALLS")" = 4 ]
jq -es 'map(.id)==[105,106,116,118] and all(.[]; .result=="cancelled" and (.at|endswith("Z")))' "$WORK/verified" >/dev/null
grep -qF 'NOTIFY pr=16 branch=fix/wrong-head run=106 owner=session-one' "$WORK/notify"
echo 'PASS ci-lane: protected runs, supersession, slot cap, priority, retry verification and attempt change'
