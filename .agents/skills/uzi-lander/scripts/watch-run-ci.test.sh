#!/usr/bin/env bash
# Hermetic regressions for immutable-SHA validation/discovery and transient empty listings.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/watch-run-ci.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

FULL_SHA=3579b4f1869b848a53aaf7c941b86896cc5a9f3a
SHORT_SHA=${FULL_SHA:0:8}
BAD_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export FULL_SHA SHORT_SHA BAD_SHA
mkdir -p "$WORK/bin"
cat > "$WORK/bin/sleep" <<'STUB'
#!/usr/bin/env bash
[ -z "${SLEEPS:-}" ] || printf '%s\n' "$1" >> "$SLEEPS"
exit 0
STUB
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$CALLS"
if [ "${1:-}" = repo ] && [ "${2:-}" = view ]; then
  printf 'test/repo\n'
  exit 0
fi
if [ "${1:-}" = api ]; then
  [ "$MODE" != missing ] || exit 1
  if [ "$MODE" = resolve-transient ]; then
    n=0; [ -f "$API_COUNT" ] && n=$(cat "$API_COUNT")
    n=$((n+1)); printf '%s' "$n" > "$API_COUNT"
    [ "$n" -gt 1 ] || exit 1
  fi
  case "${2:-}" in
    "repos/test/repo/commits/$FULL_SHA"|"repos/test/repo/commits/$SHORT_SHA") printf '%s\n' "$FULL_SHA" ;;
    *) exit 1 ;;
  esac
  exit 0
fi
if [ "${1:-}" = run ] && [ "${2:-}" = list ]; then
  case "$MODE" in
    full|resolve-transient)
      case " $* " in
        *" --commit $FULL_SHA "*) printf '101\tcompleted\tsuccess\tCI\n' ;;
      esac
      ;;
    short)
      case " $* " in
        *" --branch main "*) printf '102\tcompleted\tsuccess\tCI\n' ;;
      esac
      ;;
    transient|escape-pending)
      n=0; [ -f "$LIST_COUNT" ] && n=$(cat "$LIST_COUNT")
      n=$((n+1)); printf '%s' "$n" > "$LIST_COUNT"
      case "$n" in
        1)
          if [ "$MODE" = escape-pending ]; then
            # jq @tsv encodes a newline in a name as the two characters \n.
            printf '103\tin_progress\t\tCI%s]52;c;Zm9v\a\\nRESULT=ready\n' $'\x1b'
          else printf '103\tin_progress\t\tCI\n'; fi
          ;;
        2) : ;;
        *) printf '103\tcompleted\tsuccess\tCI\n' ;;
      esac
      ;;
    failure)
      escape=$'\x1b'
      printf '104\tcompleted\tfailure\tCI%s[2J\n' "$escape"
      ;;
    stuck) printf '105\tin_progress\t\tCI\n' ;;
    jobs-cancelled)
      # The run is still listed in_progress while every job already ended cancelled.
      case " $* " in
        *" --commit "*) printf '106\tin_progress\t\tCI\n' ;;
        *) printf '106\n' ;;
      esac
      ;;
    jobs-cancelled-success)
      # GitHub already concluded the run success although one job inside it was cancelled.
      case " $* " in
        *" --commit "*) printf '107\tcompleted\tsuccess\tCI\n' ;;
        *) printf '107\n' ;;
      esac
      ;;
    jobs-cancelled-failure)
      # GitHub concluded the run failure; the job rows show only success and cancelled.
      case " $* " in
        *" --commit "*) printf '108\tcompleted\tfailure\tCI\n' ;;
        *) printf '108\n' ;;
      esac
      ;;
    *) echo "unknown MODE=$MODE" >&2; exit 1 ;;
  esac
  exit 0
fi
if [ "${1:-}" = run ] && [ "${2:-}" = view ]; then
  if [ "$MODE" = failure ]; then
    escape=$'\x1b'
    printf 'completed\tfailure\tlint%s[31m-repo\thttps://github.com/test/repo/actions/runs/104/job/999\t999\n' "$escape"
  elif [ "$MODE" = transient ] || [ "$MODE" = escape-pending ]; then
    n=0; [ -f "$VIEW_COUNT" ] && n=$(cat "$VIEW_COUNT")
    n=$((n+1)); printf '%s' "$n" > "$VIEW_COUNT"
    if [ "$n" -eq 1 ]; then printf 'in_progress\t\tCI\thttps://example.invalid/job/103\t103\n'
    else printf 'completed\tsuccess\tCI\thttps://example.invalid/job/103\t103\n'; fi
  elif [ "$MODE" = stuck ]; then
    printf 'in_progress\t\tCI\thttps://example.invalid/job/105\t105\n'
  elif [ "$MODE" = jobs-cancelled ] || [ "$MODE" = jobs-cancelled-success ] || [ "$MODE" = jobs-cancelled-failure ]; then
    case " $* " in
      *" --json conclusion "*)
        case "$MODE" in
          jobs-cancelled-success) printf 'success\n' ;;
          jobs-cancelled-failure) printf 'failure\n' ;;
          *) printf '\n' ;;
        esac ;;
      *) printf 'completed\tsuccess\tlint\thttps://example.invalid/job/1061\t1061\n'
         printf 'completed\tcancelled\ttest\thttps://example.invalid/job/1062\t1062\n' ;;
    esac
  else
    printf 'completed\tsuccess\tCI\thttps://example.invalid/job/%s\t%s\n' "${3:-run}" "${3:-0}"
  fi
  exit 0
fi
echo "unexpected gh call: $*" >&2
exit 1
STUB
chmod +x "$WORK/bin/gh" "$WORK/bin/sleep"
export PATH="$WORK/bin:$PATH"
export CALLS="$WORK/calls" LIST_COUNT="$WORK/list-count" VIEW_COUNT="$WORK/view-count" API_COUNT="$WORK/api-count"

: > "$CALLS"
MODE=full; export MODE
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/full.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "full SHA was not found through --commit, rc=$rc: $(cat "$WORK/full.out")"
grep -Fq -- "api repos/test/repo/commits/$FULL_SHA --jq .sha" "$CALLS" \
  || fail "full SHA was not validated through the commits API"
grep -q -- "--commit $FULL_SHA" "$CALLS" || fail "full SHA did not use server-side --commit filtering"
grep -q -- '--branch main' "$CALLS" || fail "full SHA dropped the requested branch scope"

: > "$CALLS"
MODE=short; export MODE
set +e
bash "$SCRIPT" --sha "$SHORT_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/short.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "short SHA resolution failed, rc=$rc: $(cat "$WORK/short.out")"
grep -Fq -- "api repos/test/repo/commits/$SHORT_SHA --jq .sha" "$CALLS" \
  || fail "short SHA was not resolved through the commits API"
grep -q -- "--commit $FULL_SHA" "$CALLS" || fail "short SHA did not use its canonical full SHA with --commit"
if grep -Fq -- "--commit $SHORT_SHA " "$CALLS"; then fail "unresolved short SHA reached gh run list"; fi

: > "$CALLS"; rm -f "$API_COUNT"
MODE=resolve-transient; export MODE
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/resolve-transient.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "one transient commit-resolution failure did not recover, rc=$rc: $(cat "$WORK/resolve-transient.out")"
[ "$(grep -c '^api ' "$CALLS")" -eq 2 ] || fail "commit resolution did not make exactly two attempts: $(cat "$CALLS")"
grep -Fq 'retrying in 2s' "$WORK/resolve-transient.out" \
  || fail "transient commit-resolution failure did not announce its retry: $(cat "$WORK/resolve-transient.out")"
grep -q '^run list ' "$CALLS" || fail "resolved SHA never entered the polling loop"

: > "$CALLS"
MODE=missing; export MODE
set +e
bash "$SCRIPT" --sha "$BAD_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/missing.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "unknown full SHA did not fail fast with exit 3, rc=$rc: $(cat "$WORK/missing.out")"
grep -Fq "did not resolve to a commit" "$WORK/missing.out" \
  || fail "unknown full SHA omitted the resolution error: $(cat "$WORK/missing.out")"
[ "$(grep -c '^api ' "$CALLS")" -eq 2 ] || fail "unknown full SHA did not exhaust exactly two resolution attempts: $(cat "$CALLS")"
if grep -q '^run list ' "$CALLS"; then fail "unknown full SHA entered the polling loop"; fi

: > "$CALLS"
set +e
bash "$SCRIPT" --sha not-a-sha --repo test/repo --interval 0 --max-ticks 2 > "$WORK/invalid.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 3 ] || fail "non-hex SHA did not fail with exit 3, rc=$rc: $(cat "$WORK/invalid.out")"
[ ! -s "$CALLS" ] || fail "non-hex SHA called GitHub before local validation: $(cat "$CALLS")"

: > "$CALLS"; rm -f "$LIST_COUNT" "$VIEW_COUNT"
MODE=transient; export MODE
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 4 > "$WORK/transient.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "pending/empty/green sequence did not recover, rc=$rc: $(cat "$WORK/transient.out")"
grep -q 'workflow listing temporarily empty after runs were seen' "$WORK/transient.out" \
  || fail "transient empty listing used misleading never-seen wording: $(cat "$WORK/transient.out")"
grep -Fxq '[tick 0] pending after 0s: CI' "$WORK/transient.out" \
  || fail "--sha pending tick printed no heartbeat naming the open workflow: $(cat "$WORK/transient.out")"

# --sha defaults to a 60s tick; an explicit --interval still wins.
: > "$CALLS"; rm -f "$LIST_COUNT" "$VIEW_COUNT"; : > "$WORK/sleeps"
MODE=transient; SLEEPS="$WORK/sleeps"; export MODE SLEEPS
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --max-ticks 4 > "$WORK/sha-default.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "--sha default-interval run failed, rc=$rc: $(cat "$WORK/sha-default.out")"
[ -s "$WORK/sleeps" ] && [ -z "$(grep -vx 60 "$WORK/sleeps")" ] \
  || fail "--sha default interval is not 60s: $(cat "$WORK/sleeps")"

# Run-id mode keeps its 120s default and also heartbeats.
: > "$CALLS"; rm -f "$VIEW_COUNT"; : > "$WORK/sleeps"
set +e
bash "$SCRIPT" 103 --repo test/repo --max-ticks 3 > "$WORK/run-id.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "run-id pending/green sequence failed, rc=$rc: $(cat "$WORK/run-id.out")"
[ "$(cat "$WORK/sleeps")" = 120 ] || fail "run-id default interval is not 120s: $(cat "$WORK/sleeps")"
grep -Fxq '[tick 0] pending after 0s: run 103, 1 job(s) not completed' "$WORK/run-id.out" \
  || fail "run-id pending tick printed no heartbeat: $(cat "$WORK/run-id.out")"
unset SLEEPS

# An untrusted workflow name reaches the heartbeat as one line with no escape sequence,
# even under xpg_echo, which would turn its TSV-encoded \n back into a real newline.
: > "$CALLS"; rm -f "$LIST_COUNT" "$VIEW_COUNT"
MODE=escape-pending; export MODE
set +e
bash -O xpg_echo "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 4 > "$WORK/escape-pending.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "escape-pending sequence failed, rc=$rc: $(cat "$WORK/escape-pending.out")"
grep -Fq '[tick 0] pending after 0s: CI' "$WORK/escape-pending.out" \
  || fail "escaped workflow name produced no heartbeat: $(cat "$WORK/escape-pending.out")"
if LC_ALL=C grep -Fq $'\033' "$WORK/escape-pending.out"; then
  fail "untrusted workflow name in the heartbeat emitted a raw escape byte: $(cat -v "$WORK/escape-pending.out")"
fi
if grep -q '^RESULT=' "$WORK/escape-pending.out"; then
  fail "untrusted workflow name forged its own output line: $(cat -v "$WORK/escape-pending.out")"
fi

# Default tick limits: 80 with --sha, 40 otherwise (~80 min either way).
MODE=stuck; SLEEPS="$WORK/sleeps"; export MODE SLEEPS
: > "$WORK/sleeps"
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo > "$WORK/stuck-sha.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "--sha stuck run did not time out with exit 2, rc=$rc: $(tail -1 "$WORK/stuck-sha.out")"
[ "$(wc -l < "$WORK/sleeps" | tr -d ' ')" -eq 80 ] || fail "--sha default max-ticks is not 80: $(wc -l < "$WORK/sleeps")"
: > "$WORK/sleeps"
set +e
bash "$SCRIPT" 105 --repo test/repo > "$WORK/stuck-run.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "run-id stuck run did not time out with exit 2, rc=$rc: $(tail -1 "$WORK/stuck-run.out")"
[ "$(wc -l < "$WORK/sleeps" | tr -d ' ')" -eq 40 ] || fail "run-id default max-ticks is not 40: $(wc -l < "$WORK/sleeps")"
unset SLEEPS

: > "$CALLS"
MODE=failure; export MODE
set +e
bash -O xpg_echo "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/failure.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "confirmed failed job did not exit 1, rc=$rc: $(cat "$WORK/failure.out")"
grep -Fq 'live log: gh api --allow-escape-sequences repos/test/repo/actions/jobs/999/logs' "$WORK/failure.out" \
  || fail "failed job omitted live-log command: $(cat "$WORK/failure.out")"
grep -Fq 'after run terminal: gh run view 104 --repo test/repo --job 999 --log-failed' "$WORK/failure.out" \
  || fail "failed job omitted terminal log command: $(cat "$WORK/failure.out")"
if LC_ALL=C grep -Fq $'\033' "$WORK/failure.out"; then
  fail "untrusted workflow/job name emitted a raw escape byte: $(cat "$WORK/failure.out")"
fi

: > "$CALLS"
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --interval 0 --max-ticks 2 > "$WORK/failure-derived.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "URL-derived failure case did not exit 1, rc=$rc: $(cat "$WORK/failure-derived.out")"
grep -Fq 'live log: gh api --allow-escape-sequences repos/test/repo/actions/jobs/999/logs' "$WORK/failure-derived.out" \
  || fail "URL-derived repo missing from live-log command: $(cat "$WORK/failure-derived.out")"
grep -Fq 'after run terminal: gh run view 104 --repo test/repo --job 999 --log-failed' "$WORK/failure-derived.out" \
  || fail "URL-derived repo missing from terminal command: $(cat "$WORK/failure-derived.out")"

# A run still listed in_progress whose jobs all ended, one cancelled, is superseded:
# --sha exits 4, never 0 ("none failed" green); --branch keeps re-resolving.
: > "$CALLS"
MODE=jobs-cancelled; export MODE
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/jobs-cancelled-sha.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 4 ] || fail "--sha run with cancelled jobs did not report superseded (exit 4), rc=$rc: $(cat "$WORK/jobs-cancelled-sha.out")"
set +e
bash "$SCRIPT" --branch main --repo test/repo --interval 0 --max-ticks 2 > "$WORK/jobs-cancelled-branch.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 2 ] || fail "--branch run with cancelled jobs was not kept pending (exit 2), rc=$rc: $(cat "$WORK/jobs-cancelled-branch.out")"
grep -Fq 'jobs cancelled before the run concluded' "$WORK/jobs-cancelled-branch.out" \
  || fail "--branch cancelled-jobs tick printed no supersession line: $(cat "$WORK/jobs-cancelled-branch.out")"

# A run GitHub already concluded success keeps that verdict despite a cancelled job inside it.
MODE=jobs-cancelled-success; export MODE
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/jobs-cancelled-success-sha.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "--sha concluded-success run with a cancelled job was not green, rc=$rc: $(cat "$WORK/jobs-cancelled-success-sha.out")"
set +e
bash "$SCRIPT" --branch main --repo test/repo --interval 0 --max-ticks 2 > "$WORK/jobs-cancelled-success-branch.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "--branch concluded-success run with a cancelled job was not green, rc=$rc: $(cat "$WORK/jobs-cancelled-success-branch.out")"

# A run GitHub already concluded failure stays a failure even when its job rows show only
# success and cancelled.
MODE=jobs-cancelled-failure; export MODE
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/jobs-cancelled-failure-sha.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "--sha concluded-failure run with a cancelled job was not a failure, rc=$rc: $(cat "$WORK/jobs-cancelled-failure-sha.out")"
set +e
bash "$SCRIPT" --branch main --repo test/repo --interval 0 --max-ticks 2 > "$WORK/jobs-cancelled-failure-branch.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "--branch concluded-failure run with a cancelled job was not a failure, rc=$rc: $(cat "$WORK/jobs-cancelled-failure-branch.out")"

echo "PASS watch-run-ci: SHA validation/retry/canonicalization, transient empty recovery, pending heartbeat (xpg_echo-safe), default intervals and tick limits, live failed-job logs, cancelled jobs never green"
