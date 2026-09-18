#!/usr/bin/env bash
# Hermetic regressions for cr-rate-limit.sh: exact quota queries and reset formatting.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/cr-rate-limit.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin" "$WORK/state"
cat > "$WORK/bin/sleep" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu

if [ "${1:-}" = pr ] && [ "${2:-}" = view ]; then
  echo deadbeef
  exit 0
fi
if [ "${1:-}" = pr ] && [ "${2:-}" = comment ]; then
  body=""
  while [ $# -gt 0 ]; do
    [ "$1" = --body ] && { body="${2:-}"; break; }
    shift
  done
  printf '%s\n' "$body" >> "$POSTED"
  asked=$(jq -nr 'now|todate')
  replied=$(jq -nr 'now+2|todate')
  jq -n --arg a "$asked" --arg r "$replied" --arg n "${RESET_MIN:-12}" '[
    {user:{login:"tester"},body:"@coderabbitai rate limit",created_at:$a,updated_at:$a},
    {user:{login:"coderabbitai[bot]"},body:("More reviews will be available in " + $n + " minutes"),created_at:$r,updated_at:$r}
  ]' > "$COMMENTS"
  exit 0
fi
if [ "${1:-}" = api ]; then
  case "$*" in
    *'/issues/'*'/comments'*) cat "$COMMENTS"; exit 0 ;;
    *'/commits/'*'/status'*)
      if [ "$MODE" = query ]; then
        echo 'Review completed'
      else
        n=0; [ -f "$STATUS_COUNT" ] && n=$(cat "$STATUS_COUNT")
        n=$((n + 1)); echo "$n" > "$STATUS_COUNT"
        if [ "$n" -le 2 ]; then echo 'Review rate limited'; else echo 'Review completed'; fi
      fi
      exit 0 ;;
  esac
fi
echo "unexpected gh call: $*" >&2
exit 1
STUB
chmod +x "$WORK/bin/gh" "$WORK/bin/sleep"

export PATH="$WORK/bin:$PATH"
export UZI_LANDER_STATE_DIR="$WORK/state"
export COMMENTS="$WORK/comments.json"
export POSTED="$WORK/posted"
export STATUS_COUNT="$WORK/status-count"

# A stale success status must not suppress an explicitly requested exact quota query.
MODE="query"; export MODE
printf '[]\n' > "$COMMENTS"
set +e
bash "$SCRIPT" test/repo 42 --query > "$WORK/query.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "exact query returned rc=$rc, want 1: $(cat "$WORK/query.out")"
[ "$(cat "$POSTED")" = '@coderabbitai rate limit' ] || fail "exact query was not posted once"
grep -q '^CR_LIMITED=1$' "$WORK/query.out" || fail "fresh quota reply did not override stale success status"
grep -q '^CR_RESET_SOURCE=reply$' "$WORK/query.out" || fail "fresh reply was not authoritative"

# A wait progress line must print one RFC3339 value, never RFC3339+epoch concatenation.
MODE="wait"; export MODE
rm -f "$STATUS_COUNT"
now=$(jq -nr 'now|todate')
jq -n --arg t "$now" '[{
  user:{login:"coderabbitai[bot]"},
  body:"<!-- auto-generated comment: rate limited by coderabbit.ai -->\nNext included review available in 1 minutes\n<!-- end of auto-generated comment: rate limited -->",
  created_at:$t,
  updated_at:$t
}]' > "$COMMENTS"
bash "$SCRIPT" test/repo 42 --wait --interval 0 --max-wait-min 2 > "$WORK/wait.out" 2>&1
grep -Eq 'still limited; reset at [0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]+Z$' "$WORK/wait.out" \
  || fail "wait progress did not end in one RFC3339 timestamp: $(cat "$WORK/wait.out")"
if grep -Eq 'reset at .*Z[0-9]+' "$WORK/wait.out"; then
  fail "wait progress concatenated the epoch onto RFC3339: $(cat "$WORK/wait.out")"
fi

# In exact-query mode a stale Review completed status must not end the wait early.
MODE="query"; RESET_MIN=0; export MODE RESET_MIN
printf '[]\n' > "$COMMENTS"
rm -f "$POSTED"
bash "$SCRIPT" test/repo 42 --query --wait --interval 0 --max-wait-min 1 > "$WORK/exact-wait.out" 2>&1
grep -q '^CR_RESET_ELAPSED=1$' "$WORK/exact-wait.out" || fail "exact reset did not release the wait"
if grep -q '^CR_RESUMED=1$' "$WORK/exact-wait.out"; then fail "stale status ended exact wait early"; fi

echo "PASS cr-rate-limit: exact query, stale status, reset formatting"
