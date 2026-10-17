#!/usr/bin/env bash
# Hermetic branch-writer matching, terminal window and fail-closed metadata tests.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=ci-fix.sh
. "$HERE/ci-fix.sh"
fail() { echo "FAIL: $*" >&2; exit 1; }
now=1800000000
stamp=$(jq -rn --argjson n "$now" '$n|todateiso8601')

runs=$(jq -nc --arg at "$stamp" '[
 {id:"active",kind:"ci_fix",repo_id:"repo",pipeline_ref:"agent/x",status:"awaiting_input"},
 {id:"other-repo",kind:"ci_fix",repo_id:"elsewhere",pipeline_ref:"agent/x",status:"running"},
 {id:"other-branch",kind:"ci_fix",repo_id:"repo",pipeline_ref:"agent/xx",status:"running"},
 {id:"other-kind",kind:"issue",repo_id:"repo",pipeline_ref:"agent/x",status:"running"},
 {id:"recent",kind:"ci_fix",repo_id:"repo",pipeline_ref:"agent/x",status:"completed",finished_at:$at}]')
ci_fix_snapshot "$runs" repo agent/x "$now" || fail snapshot
[ "$CI_FIX_ACTIVE" = 'active awaiting_input' ] || fail "wrong active: $CI_FIX_ACTIVE"
[ "$CI_FIX_RECENT" = "recent completed $stamp" ] || fail "wrong recent: $CI_FIX_RECENT"
[ "$(ci_fix_active_ids "$runs" repo agent/x)" = active ] || fail 'active ids drifted'

runs=$(jq -nc --argjson n "$now" '[
 {id:"z",kind:"ci_fix",repo_id:"repo",pipeline_ref:"x",status:"running"},
 {id:"a",kind:"ci_fix",repo_id:"repo",pipeline_ref:"x",status:"queued"},
 {id:"done",kind:"ci_fix",repo_id:"repo",pipeline_ref:"x",status:"completed",finished_at:(($n-20)|todateiso8601)},
 {id:"cancel",kind:"ci_fix",repo_id:"repo",pipeline_ref:"x",status:"cancelled",finished_at:(($n-10)|todateiso8601)}]')
ci_fix_snapshot "$runs" repo x "$now" || fail multiple
[ "$CI_FIX_ACTIVE" = 'a queued; z running' ] && [ "$CI_FIX_ACTIVE_COUNT" = 2 ] || fail 'multiple writers hidden'
case "$CI_FIX_RECENT" in 'cancel cancelled '*) ;; *) fail 'newest terminal was not selected';; esac

for status in queued running awaiting_approval paused recovery_wait; do
  runs=$(jq -nc --arg s "$status" '[{id:"a",kind:"ci_fix",repo_id:"repo",pipeline_ref:"x",status:$s}]')
  ci_fix_snapshot "$runs" repo x "$now" || fail "$status parse"
  [ "$CI_FIX_ACTIVE" = "a $status" ] || fail "$status not active"
done
for status in completed failed cancelled; do
  for age in 86400 86401; do
    runs=$(jq -nc --arg s "$status" --argjson n "$now" --argjson age "$age" \
      '[{id:"r",kind:"ci_fix",repo_id:"repo",pipeline_ref:"x",status:$s,finished_at:(($n-$age)|todateiso8601)}]')
    ci_fix_snapshot "$runs" repo x "$now" || fail "$status/$age parse"
    [ "$CI_FIX_ACTIVE" = none ] || fail 'terminal treated as active'
    if [ "$age" = 86400 ]; then [ "$CI_FIX_RECENT" != none ] || fail 'window boundary excluded'
    else [ "$CI_FIX_RECENT" = none ] || fail 'old terminal included'; fi
  done
done
offset=$(jq -rn --argjson n "$now" '($n+3600)|strftime("%Y-%m-%dT%H:%M:%S.123+01:00")')
runs=$(jq -nc --arg at "$offset" '[{id:"r",kind:"ci_fix",repo_id:"repo",pipeline_ref:"x",status:"completed",finished_at:$at}]')
ci_fix_snapshot "$runs" repo x "$now" || fail 'fractional/offset timestamp rejected'
[ "$CI_FIX_RECENT" = "r completed $offset" ] || fail 'offset timestamp lost'

for bad in '{}' '[null]' \
  '[{"id":"r","kind":"ci_fix","repo_id":"repo","pipeline_ref":"x","status":null}]' \
  '[{"id":"r","kind":"ci_fix","repo_id":"repo","pipeline_ref":"x","status":"completed"}]' \
  '[{"id":"r","kind":"ci_fix","repo_id":"repo","pipeline_ref":"x","status":"completed","finished_at":"bad"}]'; do
  if ci_fix_snapshot "$bad" repo x "$now" 2>/dev/null; then fail "accepted $bad"; fi
  [ "$CI_FIX_ACTIVE" = unknown ] && [ "$CI_FIX_RECENT" = unknown ] || fail 'failed lookup masqueraded as none'
done
ci_fix_snapshot '[]' repo x "$now" || fail empty
[ "$CI_FIX_ACTIVE" = none ] && [ "$CI_FIX_RECENT" = none ] || fail 'empty is not none'
# Real run rows contain large payloads; never carry them in a jq argv entry
# (Linux limits each argument to 128 KiB, separately from total ARG_MAX).
runs=$(jq -nc '[range(4) | {id:("heavy-"+tostring),kind:"ci_fix",repo_id:"repo",pipeline_ref:"x",status:"running",payload:("x"*70000)}]')
ci_fix_snapshot "$runs" repo x "$now" || fail 'large active rows exceed the jq argument bound'
[ "$CI_FIX_ACTIVE_COUNT" = 4 ] || fail 'large active rows lost writers'
future=$(jq -nc --argjson n "$now" '[{id:"future",kind:"ci_fix",repo_id:"repo",pipeline_ref:"x",status:"completed",finished_at:(($n+301)|todateiso8601)}]')
if ci_fix_snapshot "$future" repo x "$now" 2>/dev/null; then fail 'accepted a finish beyond the clock-skew allowance'; fi
[ "$CI_FIX_ACTIVE" = unknown ] && [ "$CI_FIX_RECENT" = unknown ] || fail 'future finish became none'
# land-prep only needs active exclusion; historical terminal timestamps are irrelevant there.
[ -z "$(ci_fix_active_ids '[{"id":"r","kind":"ci_fix","repo_id":"repo","pipeline_ref":"x","status":"completed"}]' repo x)" ] || fail 'terminal blocked land-prep'
echo 'PASS ci-fix: exact branch/repo predicate, active/terminal split, 24h boundary, timestamps and unknowns'
