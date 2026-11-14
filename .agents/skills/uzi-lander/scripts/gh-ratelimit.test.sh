#!/usr/bin/env bash
# Primary quota exhaustion is a bounded handback, never unknown-persistent or ready.
# SCRIPTS_DIR override lets the identical integration cases demonstrate red on the base.
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS_DIR=${GH_RATELIMIT_SCRIPTS_DIR:-$HERE}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
mkdir -p "$WORK/bin" "$WORK/state"
REAL_DATE=$(command -v date)
export REAL_DATE CLOCK_FILE="$WORK/clock" STATUS_COUNT="$WORK/count" CALL_LOG="$WORK/calls" SLEEP_LOG="$WORK/sleeps"
export UZI_LANDER_STATE_DIR="$WORK/state"
cat > "$WORK/bin/date" <<'STUB'
#!/usr/bin/env bash
if [ "$*" = '+%s' ]; then
  n=$(cat "$CLOCK_FILE"); echo "$n"; echo "$((n+1))" > "$CLOCK_FILE"
elif [ "$*" = '-u +%Y-%m-%dT%H:%M:%SZ' ]; then
  jq -nr --argjson e "$(cat "$CLOCK_FILE")" '$e|todate'
else exec "$REAL_DATE" "$@"; fi
STUB
cat > "$WORK/bin/sleep" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "$SLEEP_LOG"
STUB
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
echo '[]'
STUB
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
echo "$*" >> "$CALL_LOG"
if [ "$*" = 'api rate_limit' ]; then
  case "$RATE_MODE" in
    unavailable) exit 1 ;;
    malformed) echo '{}' ;;
    core) echo '{"resources":{"core":{"limit":5000,"remaining":0,"reset":2000000010},"graphql":{"limit":5000,"remaining":10,"reset":2000000020}}}' ;;
    graphql) echo '{"resources":{"core":{"limit":5000,"remaining":10,"reset":2000000010},"graphql":{"limit":5000,"remaining":0,"reset":2000000020}}}' ;;
    both) echo '{"resources":{"core":{"limit":5000,"remaining":0,"reset":2000000010},"graphql":{"limit":5000,"remaining":0,"reset":2000000020}}}' ;;
    unallocated) echo '{"resources":{"core":{"limit":5000,"remaining":10,"reset":2000000010},"graphql":{"limit":0,"remaining":0,"reset":2000000020}}}' ;;
    *) echo '{"resources":{"core":{"limit":5000,"remaining":10,"reset":2000000010},"graphql":{"limit":5000,"remaining":10,"reset":2000000020}}}' ;;
  esac
  exit 0
fi
if [ "$APP_MODE" = green ] && [ "${1:-}" = run ] && [ "${2:-}" = view ]; then
  printf 'completed\tsuccess\tunit\thttps://example.test/job\t1\n'; exit 0
fi
if [ "$APP_MODE" = cr_trigger ]; then
  case "$*" in
    'pr view '*)
      n=$(cat "$STATUS_COUNT"); echo "$((n+1))" > "$STATUS_COUNT"
      if [ "$n" -eq 0 ]; then echo deadbeef; exit 0; fi ;;
    *'/status') echo '{"statuses":[{"context":"CodeRabbit","description":"Review completed","updated_at":"2026-11-13T00:00:00Z"}]}'; exit 0 ;;
    *'/check-runs'*) echo '{"check_runs":[]}'; exit 0 ;;
    *'/comments'*)
      if [ -f "$STATUS_COUNT.reply" ]; then
        jq -nc --argjson e "$(cat "$CLOCK_FILE")" '[{user:{login:"coderabbitai[bot]"},body:"Reviews are available now",created_at:(($e+1)|todate)}]'
      else echo '[]'; fi
      exit 0 ;;
    'pr comment '*'@coderabbitai rate limit') echo 1 > "$STATUS_COUNT.reply"; exit 0 ;;
  esac
fi
if [ "$APP_MODE" = cr_wait ]; then
  case "$*" in
    'pr view '*) echo deadbeef; exit 0 ;;
    *'/status')
      n=$(cat "$STATUS_COUNT"); echo "$((n+1))" > "$STATUS_COUNT"
      if [ "$n" -eq 0 ]; then
        echo '{"statuses":[{"context":"CodeRabbit","description":"Review rate limited","updated_at":"2026-11-13T00:00:00Z"}]}'; exit 0
      fi ;;
    *'/check-runs'*) echo '{"check_runs":[]}'; exit 0 ;;
    *'/comments'*) echo '[]'; exit 0 ;;
  esac
fi
echo 'HTTP 403: API rate limit exceeded' >&2
exit 1
STUB
chmod +x "$WORK/bin/"*
export PATH="$WORK/bin:$PATH" RATE_MODE=core APP_MODE=fail
reset_case() {
  echo 1800000000 > "$CLOCK_FILE"; echo 0 > "$STATUS_COUNT"
  : > "$CALL_LOG"; : > "$SLEEP_LOG"
  rm -f "$STATUS_COUNT.reply"
  APP_MODE=fail; RATE_MODE=core
}
run_case() {
  local rc
  if "$@" > "$WORK/out" 2>&1; then rc=0; else rc=$?; fi
  [ "$rc" -eq 12 ] || fail "expected quota exit 12, got $rc: $(cat "$WORK/out")"
  grep -qx 'GH_RATE_LIMITED=1' "$WORK/out" || fail 'missing quota diagnosis'
  grep -qx 'GH_RATE_LIMIT_RESOURCE=core' "$WORK/out" || fail 'wrong resource'
  grep -qx 'GH_RATE_LIMIT_RESET=2000000010' "$WORK/out" || fail 'wrong reset'
  grep -qx 'GH_RETRY_AT=2000000015' "$WORK/out" || fail 'missing reset margin'
  grep -q '^RESULT=gh_rate_limited' "$WORK/out" || fail 'missing handback result'
  if grep -q 'RESULT=unknown_persistent\|RESULT=ready' "$WORK/out"; then fail 'quota consumed unknown budget or became ready'; fi
  grep -qx 'api rate_limit' "$CALL_LOG" || fail 'quota endpoint not queried'
}
selected=${GH_RATELIMIT_CASE:-all}
if [ "$selected" = all ] || [ "$selected" = watch-pr ]; then
  reset_case
  run_case bash "$SCRIPTS_DIR/watch-pr.sh" test/repo 42 0 2 --max-unknown 1
  [ ! -s "$SLEEP_LOG" ] || fail 'terminal handback slept or kept polling'
  # On re-arm after a real reset, a normal unknown keeps its original bounded result.
  RATE_MODE=available
  if bash "$SCRIPTS_DIR/watch-pr.sh" test/repo 42 0 2 --max-unknown 1 > "$WORK/recovered" 2>&1; then rc=0; else rc=$?; fi
  [ "$rc" -eq 9 ] || fail "re-armed normal unknown did not retain exit 9: $rc"
  echo 'PASS gh quota: watch-pr handback before max-unknown, re-arm restores normal unknown handling'
fi
if [ "$selected" = all ] || [ "$selected" = watch-run-ci ]; then
  reset_case
  run_case bash "$SCRIPTS_DIR/watch-run-ci.sh" 42 --repo test/repo --interval 0 --max-ticks 2
  [ ! -s "$SLEEP_LOG" ] || fail 'CI handback slept'
  RATE_MODE=available; APP_MODE=green
  bash "$SCRIPTS_DIR/watch-run-ci.sh" 42 --repo test/repo --interval 0 --max-ticks 2 > "$WORK/recovered" 2>&1 || fail 'CI failed after quota recovery'
  grep -q 'all jobs terminal, none failed' "$WORK/recovered" || fail 'CI did not resume its normal verdict'
  echo 'PASS gh quota: watch-run-ci handback and green after recovery'
fi
if [ "$selected" = all ] || [ "$selected" = watch-prs-ci ]; then
  reset_case
  run_case bash "$SCRIPTS_DIR/watch-prs-ci.sh" 42 --interval 0 --max-ticks 2
  echo 'PASS gh quota: batch CI handback'
fi
if [ "$selected" = all ] || [ "$selected" = cr-rate-limit ]; then
  reset_case; APP_MODE=cr_wait
  run_case bash "$SCRIPTS_DIR/cr-rate-limit.sh" test/repo 42 --wait --interval 0 --max-wait-min 1
  [ "$(cat "$STATUS_COUNT")" -eq 2 ] || fail 'CR wait did not stop on first failed status poll'
  echo 'PASS gh quota: CR wait diagnoses GitHub exhaustion separately from CR quota'
fi
if [ "$selected" = all ] || [ "$selected" = cr-trigger ]; then
  reset_case; APP_MODE=cr_trigger
  run_case bash "$SCRIPTS_DIR/cr-rate-limit.sh" test/repo 42 --trigger-review --interval 0 --max-wait-min 1
  [ "$(cat "$STATUS_COUNT")" -eq 2 ] || fail 'trigger failure was not at the locked head recheck'
  if grep -qF '@coderabbitai review' "$CALL_LOG"; then fail 'review was posted after unreadable head'; fi
  echo 'PASS gh quota: trigger subshell preserves quota exit and posts no review after a failed read'
fi
if [ "$selected" = all ] || [ "$selected" = helper ]; then
  # shellcheck source=lib/gh-ratelimit.sh
  . "$SCRIPTS_DIR/lib/gh-ratelimit.sh"
  for mode in core graphql both; do
    reset_case; RATE_MODE=$mode
    report=$(gh_rate_limit_report) || fail "$mode exhaustion not recognized"
    case "$mode" in core) resource=core; reset=2000000010 ;; graphql) resource=graphql; reset=2000000020 ;; both) resource=core,graphql; reset=2000000020 ;; esac
    grep -qx "GH_RATE_LIMIT_RESOURCE=$resource" <<< "$report" || fail "$mode resource"
    grep -qx "GH_RATE_LIMIT_RESET=$reset" <<< "$report" || fail "$mode reset"
  done
  for mode in available unavailable malformed unallocated; do
    RATE_MODE=$mode
    if gh_rate_limit_report > "$WORK/report"; then fail "$mode became exhausted"; fi
    [ ! -s "$WORK/report" ] || fail "$mode leaked partial quota evidence"
  done
  echo 'PASS gh quota: core/graphql/both, invalid/unavailable/unallocated preserve unknown'
fi
