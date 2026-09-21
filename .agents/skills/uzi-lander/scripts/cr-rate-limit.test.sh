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
  if [ "${SINGULAR_MINUTE:-0}" = 1 ]; then
    /bin/sleep 1
    replied=$(jq -nr 'now|todate')
    reply='<!-- This is an auto-generated reply by CodeRabbit -->
Your [plan](https://docs.coderabbit.ai/management/plans#fair-usage-limits-policy) includes PR reviews subject to [rate limits](https://docs.coderabbit.ai/management/plans#rate-limits). More reviews will be available in 1 minute.'
  elif [ "${AVAILABLE_NOW:-0}" = 1 ]; then
    /bin/sleep 1
    replied=$(jq -nr 'now|todate')
    reply='<!-- This is an auto-generated reply by CodeRabbit -->
Your [plan](https://docs.coderabbit.ai/management/plans#fair-usage-limits-policy) includes PR reviews subject to [rate limits](https://docs.coderabbit.ai/management/plans#rate-limits). Reviews are available now.'
  else
    replied=$(jq -nr 'now+2|todate')
    reply="More reviews will be available in ${RESET_MIN:-12} minutes"
  fi
  # A large body AFTER the match phrase forces the `printf … | grep -qF` SIGPIPE the fix
  # removed: grep -q matches near the top and closes the pipe before printf finishes writing
  # the (>64 KiB pipe-buffer) tail, so under pipefail the old match test returned 141 (false).
  # The big body is fed to jq via --rawfile, NOT --arg: a 500 KB value passed as one argv
  # element exceeds Linux MAX_ARG_STRLEN (~128 KiB) and execve(jq) fails there (it passed only
  # on macOS). The file still yields a >pipe-buffer body, so it exercises the same SIGPIPE.
  if [ "${BIG_BODY:-0}" = 1 ]; then
    { printf '%s\n' "$reply"; head -c 500000 </dev/zero | tr '\0' x; } > "$COMMENTS.big"
    jq -n --arg a "$asked" --arg r "$replied" --rawfile reply "$COMMENTS.big" '[
      {user:{login:"tester"},body:"@coderabbitai rate limit",created_at:$a,updated_at:$a},
      {user:{login:"coderabbitai[bot]"},body:$reply,created_at:$r,updated_at:$r}
    ]' > "$COMMENTS"
  else
    jq -n --arg a "$asked" --arg r "$replied" --arg reply "$reply" '[
      {user:{login:"tester"},body:"@coderabbitai rate limit",created_at:$a,updated_at:$a},
      {user:{login:"coderabbitai[bot]"},body:$reply,created_at:$r,updated_at:$r}
    ]' > "$COMMENTS"
  fi
  if [ "${LATER_WALKTHROUGH:-0}" = 1 ]; then
    later=$(jq -nr 'now+4|todate')
    jq --arg t "$later" '. + [{
      user:{login:"coderabbitai[bot]"},
      body:"<!-- auto-generated comment: rate limited by coderabbit.ai -->\nNext included review available in 60 minutes\n<!-- end of auto-generated comment: rate limited -->",
      created_at:$t,
      updated_at:$t
    }]' "$COMMENTS" > "$COMMENTS.next"
    mv "$COMMENTS.next" "$COMMENTS"
  fi
  exit 0
fi
if [ "${1:-}" = api ]; then
  case "$*" in
    *'/issues/'*'/comments'*) cat "$COMMENTS"; exit 0 ;;
    *'/commits/'*'/status'*)
      if [ "$MODE" = query ]; then
        echo 'Review completed'
      elif [ "$MODE" = available ]; then
        echo 'Review rate limited'
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

# A stale success status and later-edited walkthrough must not suppress or override the exact reply.
MODE="query"; LATER_WALKTHROUGH=1; export MODE LATER_WALKTHROUGH
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

# "Reviews are available now" is an authoritative zero-minute reply, even with stale limited status.
MODE="available"; AVAILABLE_NOW=1; export MODE AVAILABLE_NOW
printf '[]\n' > "$COMMENTS"
rm -f "$POSTED"
set +e
bash "$SCRIPT" test/repo 42 --query > "$WORK/available.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "available-now reply returned rc=$rc, want 0: $(cat "$WORK/available.out")"
grep -q '^CR_LIMITED=0$' "$WORK/available.out" || fail "available-now reply left stale limited state"
grep -q '^CR_RESET_ELAPSED=1$' "$WORK/available.out" || fail "available-now reply did not release the query"

# CodeRabbit grammatically uses singular "1 minute"; it is the same authoritative countdown.
MODE="singular"; SINGULAR_MINUTE=1; unset AVAILABLE_NOW; export MODE SINGULAR_MINUTE
printf '[]\n' > "$COMMENTS"
rm -f "$POSTED"
set +e
bash "$SCRIPT" test/repo 42 --query > "$WORK/singular.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "singular-minute reply returned rc=$rc, want 1: $(cat "$WORK/singular.out")"
grep -q '^CR_LIMITED=1$' "$WORK/singular.out" || fail "singular-minute reply did not keep the active limit"
grep -q '^CR_RESET_MIN=1$' "$WORK/singular.out" || fail "singular-minute reply did not produce a one-minute reset"
grep -q '^CR_RESET_SOURCE=reply$' "$WORK/singular.out" || fail "singular-minute reply was not authoritative"

# A LARGE available-now reply must still be recognized: the old `printf … | grep -qF` match
# test returned 141 (SIGPIPE) under pipefail on a body past the pipe buffer, silently missing a
# real immediate reset (observed on #1504). The no-pipeline `case` reads it deterministically.
MODE="available"; AVAILABLE_NOW=1; BIG_BODY=1; unset SINGULAR_MINUTE; export MODE AVAILABLE_NOW BIG_BODY
printf '[]\n' > "$COMMENTS"
rm -f "$POSTED"
set +e
bash "$SCRIPT" test/repo 42 --query > "$WORK/available-big.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 0 ] || fail "large available-now reply returned rc=$rc, want 0 (SIGPIPE miss?): $(cat "$WORK/available-big.out")"
grep -q '^CR_LIMITED=0$' "$WORK/available-big.out" || fail "large available-now reply left stale limited state: $(cat "$WORK/available-big.out")"
grep -q '^CR_RESET_ELAPSED=1$' "$WORK/available-big.out" || fail "large available-now reply did not release the query: $(cat "$WORK/available-big.out")"
unset BIG_BODY

echo "PASS cr-rate-limit: exact query, singular minute, available-now (small + large), stale status, reset formatting"
