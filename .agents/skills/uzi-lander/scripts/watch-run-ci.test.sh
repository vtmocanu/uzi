#!/usr/bin/env bash
# Hermetic regressions for immutable-SHA workflow discovery and transient empty listings.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/watch-run-ci.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

FULL_SHA=3579b4f1869b848a53aaf7c941b86896cc5a9f3a
SHORT_SHA=${FULL_SHA:0:8}
export FULL_SHA
mkdir -p "$WORK/bin"
cat > "$WORK/bin/sleep" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$CALLS"
if [ "${1:-}" = run ] && [ "${2:-}" = list ]; then
  case "$MODE" in
    full)
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
      printf '104\tin_progress\t\tCI\n'
      ;;
    *) echo "unknown MODE=$MODE" >&2; exit 1 ;;
  esac
  exit 0
fi
if [ "${1:-}" = run ] && [ "${2:-}" = view ]; then
  if [ "$MODE" = failure ]; then
    printf 'completed\tfailure\tlint-repo\thttps://github.com/test/repo/actions/runs/104/job/999\t999\n'
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
export CALLS="$WORK/calls" LIST_COUNT="$WORK/list-count" VIEW_COUNT="$WORK/view-count"

: > "$CALLS"
MODE=full; export MODE
set +e
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/full.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "full SHA was not found through --commit, rc=$rc: $(cat "$WORK/full.out")"
grep -q -- "--commit $FULL_SHA" "$CALLS" || fail "full SHA did not use server-side --commit filtering"
grep -q -- '--branch main' "$CALLS" || fail "full SHA dropped the requested branch scope"

: > "$CALLS"
MODE=short; export MODE
set +e
bash "$SCRIPT" --sha "$SHORT_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/short.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "short SHA fallback failed, rc=$rc: $(cat "$WORK/short.out")"
grep -q -- '--branch main' "$CALLS" || fail "short SHA did not keep branch-list prefix fallback"
if grep -q -- '--commit' "$CALLS"; then fail "short SHA was passed to gh --commit, which does not resolve it"; fi

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
bash "$SCRIPT" --sha "$FULL_SHA" --repo test/repo --interval 0 --max-ticks 2 > "$WORK/failure.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "confirmed failed job did not exit 1, rc=$rc: $(cat "$WORK/failure.out")"
grep -Fq 'live log: gh api --allow-escape-sequences repos/test/repo/actions/jobs/999/logs' "$WORK/failure.out" \
  || fail "failed job omitted live-log command: $(cat "$WORK/failure.out")"
grep -Fq 'after run terminal: gh run view 104 --job 999 --log-failed' "$WORK/failure.out" \
  || fail "failed job omitted terminal log command: $(cat "$WORK/failure.out")"

echo "PASS watch-run-ci: exact SHA discovery, short fallback, transient empty recovery, live failed-job logs"
