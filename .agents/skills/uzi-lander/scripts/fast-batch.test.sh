#!/usr/bin/env bash
# Offline batch report contract. Network or write calls outside the stubs fail.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir "$WORK/bin"
export PATH="$WORK/bin:$PATH" UZI_LANDER_STATE_DIR="$WORK/state"
export MAIN=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export ONE=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
export TWO=cccccccccccccccccccccccccccccccccccccccc
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
case "$*" in
  *'/git/ref/heads/main'*) [ "${NO_MAIN:-0}" = 0 ] && printf '%s\n' "$MAIN" ;;
  *"/contents/api/internal/store/migrations?ref=$MAIN") echo '[{"name":"00541_top.sql"}]' ;;
  *'/rules/branches/main'*) echo '[[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"CI"}]}}]]' ;;
  *'/protection/required_status_checks') printf 'HTTP/2.0 404\r\n\r\n{"message":"Branch not protected"}\n'; exit 1 ;;
  'pr list '*) jq -cn --arg a "$ONE" --arg b "$TWO" '[
    {number:1,headRefOid:$a,headRefName:"fix/one",baseRefName:"main"},
    {number:2,headRefOid:$b,headRefName:"fix/two",baseRefName:"main"}]' ;;
  'pr checks 1 '*) [ "${CHECKS_MISSING:-0}" = 0 ] && echo '[{"name":"CI","bucket":"pass"}]' || echo '[]' ;;
  'pr checks 2 '*) echo '[{"name":"CI","bucket":"pending"}]'; exit 8 ;;
  'pr view 1 '*) jq -cn --arg h "${MOVED:-$ONE}" '{state:"OPEN",headRefOid:$h,baseRefName:"main",mergeable:"MERGEABLE"}' ;;
  'pr view 2 '*) jq -cn --arg h "$TWO" '{state:"OPEN",headRefOid:$h,baseRefName:"main",mergeable:"CONFLICTING"}' ;;
  *'/pulls/1/files') [ "${NO_FILES:-0}" = 0 ] && echo '[{"filename":"api/internal/store/migrations/00540_gap.sql","status":"added"}]' ;;
  *'/pulls/2/files') echo '[{"filename":"api/internal/store/migrations/00543_later.sql","status":"added"}]' ;;
  'run list '*) jq -cn --arg h "$ONE" '{databaseId:81,headSha:$h,conclusion:"success"}' ;;
  *'/actions/runs/81') jq -cn --arg h "$ONE" --argjson pr "${LINKED_PR:-1}" '
    {head_sha:$h,event:"pull_request",conclusion:"success",run_attempt:2,pull_requests:[{number:$pr,head:{sha:$h},base:{ref:"main"}}]}'  ;;
  *'/actions/runs/81/attempts/2/jobs?per_page=100')
    echo '[{"jobs":[{"name":"test-livedb-workersvc","id":91,"status":"completed","conclusion":"success"},
      {"name":"test-codex-refresh-lostreply-e2e","id":92,"status":"completed","conclusion":"success"}]}]' ;;
  *'/actions/jobs/91/logs') printf 'CI log\nLiveDB: 12 passed, 0 skipped\n' ;;
  *'/actions/jobs/92/logs') printf 'CI log\nLiveDB: 10 passed, %s skipped\n' "${SKIPPED:-0}" ;;
  *) echo "unexpected gh: $*" >&2; exit 99 ;;
esac
STUB
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
set -eu
case "$*" in
  'repo list --json') echo '[{"id":"repo-1","path_with_namespace":"vtmocanu/uzi"}]' ;;
  'admin --help') echo '  run-writers list all writers' ;;
  'admin run-writers '*) echo '{"writers":[],"complete":true}' ;;
  *) echo "unexpected uzi: $*" >&2; exit 99 ;;
esac
STUB
chmod +x "$WORK/bin/gh" "$WORK/bin/uzi"
jq -cn --arg head "$ONE" '[{pr:1,head:$head,review:"journal:review-1",trust_boundary:false}]' > "$WORK/evidence"
run() {
  "$HERE/fast-batch.sh" --evidence "$WORK/evidence" --active-jobs 11 --peak-jobs 3 1 2 > "$WORK/report"
}
assert() {
  if ! jq -e "$1" < "$WORK/report" >/dev/null; then cat "$WORK/report" >&2; exit 1; fi
}
run
assert '.main_unchanged and .migration_head==541 and .job_budget.max_pushes==3
  and .prs[0].review=="journal:review-1" and .prs[0].trust_boundary==false
  and .prs[0].ci=="green" and .prs[0].writer_visibility=="all_owners"
  and .prs[0].mergeable=="MERGEABLE" and .prs[0].migrations.collisions==["00540_gap.sql"]
  and .prs[0].proposed_slot=={first:542,last:542}
  and .prs[0].livedb["test-livedb-workersvc"]=="LiveDB: 12 passed, 0 skipped"
  and .prs[0].livedb["test-codex-refresh-lostreply-e2e"]=="LiveDB: 10 passed, 0 skipped"
  and .prs[1].ci=="pending" and .prs[1].mergeable=="CONFLICTING"
  and .prs[1].trust_boundary==null and .prs[1].proposed_slot.first==543'
LINKED_PR=2 run
assert '.prs[0].ci=="green" and .prs[0].livedb==null'
SKIPPED=1 run
assert '.prs[0].livedb["test-codex-refresh-lostreply-e2e"]==null'
CHECKS_MISSING=1 run
assert '.prs[0].ci=="pending" and .prs[0].livedb==null'
MOVED=$TWO run
assert '.prs[0].ci=="unknown" and .prs[0].review==null and .prs[0].livedb==null
  and .prs[0].proposed_slot==null and .prs[1].proposed_slot==null'
NO_FILES=1 run
assert '.prs[0].migrations==null and .prs[1].proposed_slot==null'
NO_MAIN=1 run
assert '(.main_unchanged|not) and .job_budget.max_pushes==null and all(.prs[]; .proposed_slot==null)'
echo 'PASS fast-batch: pinned evidence, LiveDB proof, ordered slots, job budget, and unknowns'
