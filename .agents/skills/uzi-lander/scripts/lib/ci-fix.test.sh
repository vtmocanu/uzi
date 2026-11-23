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
# Only exact terminal tokens establish that a writer has stopped.
[ "$(ci_fix_active_ids '[{"id":"r","kind":"ci_fix","repo_id":"repo","pipeline_ref":"x","status":"not_completed"}]' repo x)" = r ] || fail 'substring status concealed a writer'
helpers='[{"id":"fix","kind":"ci_fix","repo_id":"repo","pipeline_ref":"x","status":"running"},{"id":"rework","kind":"mr_rework","repo_id":"repo","mr_iid":42,"status":"paused"},{"id":"other","kind":"mr_rework","repo_id":"repo","mr_iid":43,"status":"running"}]'
[ "$(branch_helper_active_runs "$helpers" repo x 42 | jq -r 'map(.id)|join(" ")')" = 'fix rework' ] || fail 'shared helper predicate lost or widened writers'
if branch_helper_active_runs '[{"id":null,"kind":"mr_rework","repo_id":"repo","mr_iid":42,"status":"running"}]' repo x 42 2>/dev/null; then fail 'malformed matching rework accepted'; fi
for docs in '[][]' '[] []' ''; do
  if branch_helper_active_runs "$docs" repo x 42 >/dev/null 2>&1; then fail "run list '$docs' (not one document) accepted"; fi
done
# Whole-list validation: a malformed row that does NOT match the target still fails closed.
for bad in '[{}]' '[1]' '[{"id":"o","kind":"issue","repo_id":"elsewhere","status":"Running"}]' \
  '[{"id":"o","kind":7,"repo_id":"repo","status":"running"}]' '[{"id":"o","kind":"issue","repo_id":3,"status":"running"}]' \
  '[{"id":"o","kind":"issue","repo_id":"repo","status":"running","mr_iid":"7"}]' \
  '[{"id":"o","kind":"issue","repo_id":"repo","status":"running","pipeline_ref":5}]'; do
  if branch_helper_active_runs "$bad" repo x 7 >/dev/null 2>&1; then fail "accepted malformed list $bad"; fi
  if ci_fix_snapshot "$bad" repo x "$now" 2>/dev/null; then fail "snapshot accepted $bad"; fi
done
# Absent pipeline_ref/mr_iid are null (the CLI may omit empty fields).
branch_helper_active_runs '[{"id":"o","kind":"issue","repo_id":null,"status":"running"}]' repo x 7 >/dev/null || fail 'null repo_id/absent keys refused'

# branch_writers_read against a fake uzi that models cobra: unknown subcommands print the parent help (rc 0).
ADMIN_RW=1; WRITERS_RC=0; WRITERS_OUT='{"writers":[],"complete":true}'; RUNS_OUT='[]'; RUNS_RC=0
uzi() {
  case "$1 $2" in
    'admin --help') printf 'Available Commands:\n  review   x\n'; [ "$ADMIN_RW" != 1 ] || printf '  run-writers  y\n' ;;
    'admin run-writers') [ "$WRITERS_RC" -eq 0 ] || return "$WRITERS_RC"; printf '%s' "$WRITERS_OUT" ;;
    'run list') [ "$RUNS_RC" -eq 0 ] || return "$RUNS_RC"; printf '%s' "$RUNS_OUT" ;;
    *) return 1 ;;
  esac
}
w='{"run_id":"w1","kind":"ci_fix","status":"running","owner_id":"o","repo_id":"repo","pipeline_ref":"x","mr_iid":null,"created_at":"t"}'
WRITERS_OUT="{\"writers\":[$w],\"complete\":true}"; RUNS_OUT='[]'
branch_writers_read repo x 7 || fail 'authorized read refused'
[ "$WRITERS_VISIBILITY" = all_owners ] && [ "$(printf '%s' "$WRITERS" | jq -c .)" = '[{"id":"w1","kind":"ci_fix","status":"running"}]' ] || fail 'authorized writers not mapped'
writers_summarize; [ "$CI_FIX_ACTIVE" = 'w1 running' ] && [ "$CI_FIX_ACTIVE_COUNT" = 1 ] && [ "$WRITERS_MRW_COUNT" = 0 ] || fail 'summary wrong'
for rc in 1 2 6; do WRITERS_RC=$rc; if branch_writers_read repo x 7; then fail "rc $rc accepted"; fi; [ -n "$WRITERS_ERR" ] || fail 'no reason'; done
WRITERS_RC=0
for body in 'junk' '{"writers":[],"complete":false}' '{"writers":[],"complete":"true"}' '{"writers":[{"run_id":"a","kind":"ci_fix","status":"running","pipeline_ref":"other"}],"complete":true}' \
  '{"writers":[{"run_id":"a","kind":"mr_rework","status":"running","mr_iid":8}],"complete":true}'; do
  WRITERS_OUT=$body; if branch_writers_read repo x 7; then fail "accepted $body"; fi
done
# Two concatenated documents (even individually valid) and an empty body are refused.
WRITERS_OUT='{"writers":[],"complete":true}{"writers":[],"complete":true}'
if branch_writers_read repo x 7; then fail 'two concatenated documents accepted'; fi
WRITERS_OUT=''
if branch_writers_read repo x 7; then fail 'empty body accepted'; fi
WRITERS='[][]'; if writers_summarize; then fail 'summarize accepted two documents'; fi
# Without an MR number an mr_rework entry cannot match.
WRITERS_OUT='{"writers":[{"run_id":"a","kind":"mr_rework","status":"running","mr_iid":7}],"complete":true}'
if branch_writers_read repo x; then fail 'mr_rework accepted without an MR'; fi
# rc 3/4 and an older CLI fall back to the owner list (owner_only); a failed fallback refuses.
WRITERS_OUT='{"writers":[],"complete":true}'
RUNS_OUT='[{"id":"o1","kind":"ci_fix","repo_id":"repo","pipeline_ref":"x","status":"queued"}]'
for rc in 3 4; do
  WRITERS_RC=$rc; branch_writers_read repo x 7 || fail "rc $rc did not fall back"
  [ "$WRITERS_VISIBILITY" = owner_only ] && [ "$(printf '%s' "$WRITERS" | jq -r '.[0].id')" = o1 ] && [ -n "$WRITERS_RUNS" ] || fail "rc $rc fallback result"
done
WRITERS_RC=0; ADMIN_RW=0; branch_writers_read repo x 7 || fail 'older CLI did not fall back'
[ "$WRITERS_VISIBILITY" = owner_only ] || fail 'older CLI visibility'
RUNS_RC=1; if branch_writers_read repo x 7; then fail 'failed fallback accepted'; fi
RUNS_RC=0; RUNS_OUT='[{}]'; if branch_writers_read repo x 7; then fail 'malformed fallback accepted'; fi
unset -f uzi
WRITERS='[{"id":"foreign","kind":"ci_fix","status":"claimed"}]'
CI_FIX_RECENT=unchanged
owner='[{"id":"owner","kind":"ci_fix","repo_id":"repo","pipeline_ref":"x","status":"running"}]'
writers_include_owner_active "$owner" repo x 7 || fail 'union observations'
[ "$CI_FIX_ACTIVE_COUNT" -eq 2 ] || fail 'union lost an active observation'
[ "$CI_FIX_ACTIVE" = 'foreign claimed; owner running' ] || fail 'union IDs missing'
[ "$CI_FIX_RECENT" = unchanged ] || fail 'union changed the recent advisory'
WRITERS='[{"id":"owner","kind":"ci_fix","status":"claimed"}]'
writers_include_owner_active "$owner" repo x 7 || fail 'deduplicate observations'
[ "$CI_FIX_ACTIVE_COUNT" -eq 1 ] && [ "$CI_FIX_ACTIVE" = 'owner running' ] || fail 'union duplicated an ID or lost its later status'
WRITERS='[]'
writers_include_owner_active '[]' repo x 7 || fail 'empty union'
[ "$CI_FIX_ACTIVE_COUNT" -eq 0 ] && [ "$CI_FIX_ACTIVE" = none ] || fail 'empty union invented a writer'
echo 'PASS ci-fix: exact branch/repo predicate, active/terminal split, 24h boundary, timestamps and unknowns'
