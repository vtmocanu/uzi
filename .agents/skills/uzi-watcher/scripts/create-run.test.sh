#!/usr/bin/env bash
# Hermetic regression for create-run.sh: retries only the label-sync refusals.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/create-run.sh"
BASH_BIN="${BASH_BIN:-/bin/bash}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail(){ echo "FAIL: $*" >&2; exit 1; }

# Fake uzi: $MODE picks the behaviour; each call appends its argv to $CALLS.
cat > "$WORK/uzi" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$CALLS"
n="$(wc -l < "$CALLS" | tr -d ' ')"
echo "uzi: CLI v0.1.0 is behind server 0.2.0; some fields may be missing." >&2
case "$MODE" in
  sync-then-ok)
    if [ "$n" -le 2 ]; then
      echo "uzi: this issue is not marked as uzi's work; add the uzi label" >&2; exit 2
    fi
    echo '{"run":{"id":"r1","status":"queued"}}' ;;
  new-issue-then-ok)
    if [ "$n" -le 1 ]; then echo "uzi: issue not found on this repo's board" >&2; exit 4; fi
    echo '{"run":{"id":"r2","status":"queued"}}' ;;
  conflict) echo "uzi: a run is already in progress for this issue" >&2; exit 5 ;;
  never) echo "uzi: this issue is not marked as uzi's work" >&2; exit 2 ;;
esac
STUB
chmod +x "$WORK/uzi"

run() { # run <mode> <limit> -> sets RC, OUT, ERR
  : > "$WORK/calls"
  RC=0
  MODE="$1" CALLS="$WORK/calls" UZI_BIN="$WORK/uzi" CREATE_RETRY_SECS="$2" CREATE_RETRY_INTERVAL=1 \
    "$BASH_BIN" "$SCRIPT" repo-1 42 --mr-rework > "$WORK/out" 2> "$WORK/err" || RC=$?
  CALLS_N="$(wc -l < "$WORK/calls" | tr -d ' ')"
}

run sync-then-ok 30
[ "$RC" = 0 ] || fail "sync-then-ok exit $RC: $(cat "$WORK/err")"
[ "$CALLS_N" = 3 ] || fail "sync-then-ok made $CALLS_N calls, want 3"
jq -e '.run.id == "r1"' "$WORK/out" >/dev/null || fail "stdout is not the clean JSON: $(cat "$WORK/out")"
[ "$(head -1 "$WORK/calls")" = "run create --repo repo-1 --issue 42 --mr-rework --json" ] \
  || fail "argv not passed through: $(head -1 "$WORK/calls")"

run new-issue-then-ok 30
[ "$RC" = 0 ] && [ "$CALLS_N" = 2 ] || fail "new-issue-then-ok rc=$RC calls=$CALLS_N"

run conflict 30
[ "$RC" = 5 ] || fail "conflict exit $RC, want uzi's 5"
[ "$CALLS_N" = 1 ] || fail "conflict retried ($CALLS_N calls): a generic error must stop at once"

run never 3
[ "$RC" = 2 ] || fail "never exit $RC, want uzi's 2"
grep -q RETRY_EXHAUSTED "$WORK/err" || fail "exhausted window not reported"
[ "$CALLS_N" -ge 2 ] && [ "$CALLS_N" -le 4 ] || fail "never made $CALLS_N calls in a 3s window"

# No extra flags: the empty-argument expansion must work under macOS bash 3.2 set -u.
: > "$WORK/calls"
MODE=sync-then-ok CALLS="$WORK/calls" UZI_BIN="$WORK/uzi" CREATE_RETRY_SECS=30 CREATE_RETRY_INTERVAL=1 \
  "$BASH_BIN" "$SCRIPT" repo-1 42 > "$WORK/out" 2> "$WORK/err" || fail "no-flags create failed: $(cat "$WORK/err")"
[ "$(head -1 "$WORK/calls")" = "run create --repo repo-1 --issue 42 --json" ] || fail "no-flags argv: $(head -1 "$WORK/calls")"

echo "create-run.test.sh: ok"
