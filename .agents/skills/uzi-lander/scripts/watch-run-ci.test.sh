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
    transient)
      n=0; [ -f "$LIST_COUNT" ] && n=$(cat "$LIST_COUNT")
      n=$((n+1)); printf '%s' "$n" > "$LIST_COUNT"
      case "$n" in
        1) printf '103\tin_progress\t\tCI\n' ;;
        2) : ;;
        *) printf '103\tcompleted\tsuccess\tCI\n' ;;
      esac
      ;;
    failure)
      escape=$'\x1b'
      printf '104\tcompleted\tfailure\tCI%s[2J\n' "$escape"
      ;;
    *) echo "unknown MODE=$MODE" >&2; exit 1 ;;
  esac
  exit 0
fi
if [ "${1:-}" = run ] && [ "${2:-}" = view ]; then
  if [ "$MODE" = failure ]; then
    escape=$'\x1b'
    printf 'completed\tfailure\tlint%s[31m-repo\thttps://github.com/test/repo/actions/runs/104/job/999\t999\n' "$escape"
  elif [ "$MODE" = transient ]; then
    n=0; [ -f "$VIEW_COUNT" ] && n=$(cat "$VIEW_COUNT")
    n=$((n+1)); printf '%s' "$n" > "$VIEW_COUNT"
    if [ "$n" -eq 1 ]; then printf 'in_progress\t\tCI\thttps://example.invalid/job/103\t103\n'
    else printf 'completed\tsuccess\tCI\thttps://example.invalid/job/103\t103\n'; fi
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

echo "PASS watch-run-ci: SHA validation/retry/canonicalization, transient empty recovery, live failed-job logs"
