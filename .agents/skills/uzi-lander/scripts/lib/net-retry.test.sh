#!/usr/bin/env bash
# Hermetic regression for net_retry / net_retry_install: transport errors retry, client
# errors and unrelated failures do not, output is never concatenated across attempts.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin"
SLEEPS="$WORK/sleeps"; : > "$SLEEPS"
cat > "$WORK/bin/sleep" <<STUB
#!/usr/bin/env bash
echo "\$1" >> "$SLEEPS"
STUB
# fakecmd MODE: counts calls in \$FAKE_COUNT; behaviour keyed by \$FAKE_MODE.
cat > "$WORK/bin/fakecmd" <<'STUB'
#!/usr/bin/env bash
n=$(cat "$FAKE_COUNT" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > "$FAKE_COUNT"
case "$FAKE_MODE" in
  blip2) if [ "$n" -le 2 ]; then echo "error connecting to api.github.com" >&2; exit 1; fi
         printf 'ok-out'; exit 0 ;;
  partial) if [ "$n" -le 1 ]; then printf 'PARTIAL'; echo "dial tcp: i/o timeout" >&2; exit 1; fi
           printf 'ok-out'; exit 0 ;;
  persist) echo "attempt $n: connection reset by peer" >&2; exit 7 ;;
  h403) echo "HTTP 403: Forbidden" >&2; exit 1 ;;
  h429) echo "HTTP 429: rate limited" >&2; exit 1 ;;
  h404) echo "HTTP 404: Not Found" >&2; exit 1 ;;
  h404reset) echo "HTTP 404: Not Found (connection reset)" >&2; exit 1 ;;
  other) printf 'partial-out'; echo "boom: something else" >&2; exit 3 ;;
  malformed) printf '{not json'; exit 0 ;;
  ok) printf 'no-newline'; exit 0 ;;
  okwarn) printf 'body\n'; echo "warning: version skew" >&2; exit 0 ;;
  # once: the first call fails with $FAKE_ERR on stderr, the second succeeds.
  once) if [ "$n" -le 1 ]; then echo "$FAKE_ERR" >&2; exit 1; fi
        printf 'ok-out'; exit 0 ;;
esac
STUB
chmod +x "$WORK/bin/sleep" "$WORK/bin/fakecmd"
mkdir -p "$WORK/tmp"
# A private TMPDIR keeps the leak check exact: another process's net-retry files are not ours.
export PATH="$WORK/bin:$PATH" NET_RETRY_BASE_SLEEP=0 FAKE_COUNT="$WORK/count" TMPDIR="$WORK/tmp"
# shellcheck source=net-retry.sh
. "$HERE/net-retry.sh"

# run MODE -> $WORK/out, $WORK/err, RC, CALLS
run() {
  FAKE_MODE="$1"; export FAKE_MODE; : > "$FAKE_COUNT"; echo 0 > "$FAKE_COUNT"
  RC=0
  net_retry fakecmd a b > "$WORK/out" 2> "$WORK/err" || RC=$?
  CALLS=$(cat "$FAKE_COUNT")
}
expect() { [ "$RC" = "$1" ] || fail "$2: rc $RC, want $1"; [ "$CALLS" = "$3" ] || fail "$2: $CALLS calls, want $3"; }
cmp_str() { printf '%s' "$2" > "$WORK/want"; cmp -s "$1" "$WORK/want" || fail "$3: $1 differs from expected '$2'"; }

run blip2; expect 0 blip2 3
cmp_str "$WORK/out" 'ok-out' blip2-out
grep -q 'net-retry: fakecmd transport error, attempt 1/4' "$WORK/err" || fail "blip2: no retry log"

run partial; expect 0 partial 2
cmp_str "$WORK/out" 'ok-out' partial-out

run persist; expect 7 persist 4
grep -q 'attempt 4: connection reset' "$WORK/err" || fail "persist: last stderr missing"
grep -q 'attempt 3: connection reset' "$WORK/err" && fail "persist: earlier attempt stderr leaked"

for m in h403:'HTTP 403: Forbidden' h429:'HTTP 429: rate limited' h404:'HTTP 404: Not Found'; do
  run "${m%%:*}"; expect 1 "${m%%:*}" 1
  cmp_str "$WORK/err" "${m#*:}
" "${m%%:*}-err"
  [ ! -s "$WORK/out" ] || fail "${m%%:*}: stdout not empty"
done

run h404reset; expect 1 h404reset 1
run other; expect 3 other 1
cmp_str "$WORK/out" 'partial-out' other-out
cmp_str "$WORK/err" 'boom: something else
' other-err

run malformed; expect 0 malformed 1
cmp_str "$WORK/out" '{not json' malformed-out

run ok; expect 0 ok 1
cmp_str "$WORK/out" 'no-newline' ok-out
[ ! -s "$WORK/err" ] || fail "ok: stderr not empty"

run okwarn; expect 0 okwarn 1
cmp_str "$WORK/out" 'body
' okwarn-out
cmp_str "$WORK/err" 'warning: version skew
' okwarn-err

# Each transport class retries, matched case-insensitively.
for e in 'HTTP 502: Bad Gateway' 'gh: HTTP 504' 'dial tcp: Connection Refused' \
  'Could Not Resolve Host: api.github.com' 'lookup api.github.com: no such host' \
  'request Timed Out' 'net/http: TLS handshake timeout' 'context deadline exceeded'; do
  FAKE_ERR="$e" run once; expect 0 "once($e)" 2
done
# A 5xx outside 500-504 and a lowercase 4xx are not transport errors.
FAKE_ERR='HTTP 505: Version Not Supported' run once; expect 1 http505 1
FAKE_ERR='http 404 (connection refused)' run once; expect 1 lc404 1

# Leading `command` word is skipped in the log name.
FAKE_MODE=blip2; echo 0 > "$FAKE_COUNT"
net_retry command fakecmd > /dev/null 2> "$WORK/err"
grep -q 'net-retry: fakecmd transport error' "$WORK/err" || fail "command word not skipped in log"

# Invalid env falls back to defaults (4 attempts).
NET_RETRY_ATTEMPTS=abc run persist; expect 7 bad-attempts 4
NET_RETRY_ATTEMPTS=0 run persist; expect 7 zero-attempts 4
NET_RETRY_ATTEMPTS=2 run persist; expect 7 two-attempts 2

# No temp files leak.
for leaked in "${TMPDIR:-/tmp}"/net-retry-*; do [ ! -e "$leaked" ] || fail "temp file leaked: $leaked"; done

# Retries slept, and only zero seconds.
[ -s "$SLEEPS" ] || fail "retries never called sleep"
grep -qvx 0 "$SLEEPS" && fail "non-zero sleep recorded: $(sort -u "$SLEEPS" | tr '\n' ' ')"

# Backoff doubles when the base is non-zero.
: > "$SLEEPS"
NET_RETRY_BASE_SLEEP=2 run persist
[ "$(tr '\n' ' ' < "$SLEEPS")" = "2 4 8 " ] || fail "backoff: $(tr '\n' ' ' < "$SLEEPS")"

# net_retry_install: no uzi on PATH -> uzi stays undefined; gh gets wrapped.
mkdir -p "$WORK/mini"
for t in bash cat rm mktemp grep; do ln -s "$(command -v "$t")" "$WORK/mini/$t"; done
printf '#!/bin/sh\necho gh-real\n' > "$WORK/mini/gh"; chmod +x "$WORK/mini/gh"
(
  export PATH="$WORK/mini"
  # shellcheck source=net-retry.sh
  . "$HERE/net-retry.sh"
  net_retry_install
  declare -F gh > /dev/null || exit 11
  if declare -F uzi > /dev/null; then exit 12; fi
  if command -v uzi > /dev/null 2>&1; then exit 13; fi
  [ "$(gh x)" = gh-real ] || exit 14
) || fail "net_retry_install without uzi (subshell rc $?)"

echo "net-retry.test.sh: ok"
