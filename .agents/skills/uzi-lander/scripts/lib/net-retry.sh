# shellcheck shell=bash
# net-retry.sh — bounded retry of transient transport errors for gh / uzi reads.
# Sourced by takeover.sh, land-prep.sh and watch-pr.sh; functions only (no `set`, no traps).
#
# `net_retry_install` shadows `gh` and `uzi` with shell functions that route every call
# through `net_retry`. EVERY call routed this way must be a READ: a retried write could be
# applied twice. A future write call must bypass the retry with `command gh ...` or
# `command uzi ...` (the `command` builtin skips the shadowing function).
#
# `net_retry CMD [ARGS...]` runs CMD with stdout and stderr buffered per attempt. On exit 0
# the buffered output is replayed unchanged. On a non-zero exit the stderr is classified
# (case-insensitive, first match wins):
#   1. `HTTP 4xx`                                   -> no retry (a client error is final,
#                                                      even beside a transport phrase)
#   2. a transport phrase (error connecting to, could not resolve host, no such host,
#      dial tcp, connection refused, connection reset, i/o timeout, tls handshake timeout,
#      context deadline exceeded, timed out) or `HTTP 500`-`HTTP 504`  -> retry
#   3. anything else                                -> no retry
# A retried attempt's output is discarded. The final attempt (or a no-retry failure) emits
# its own stdout and stderr unchanged and returns its own exit status.
#
# Env knobs (a non-integer value, or attempts < 1, falls back to the default):
#   NET_RETRY_ATTEMPTS    total attempts, default 4
#   NET_RETRY_BASE_SLEEP  seconds before the first retry, default 2; doubles each retry

net_retry() {
  local attempts="${NET_RETRY_ATTEMPTS:-4}" base="${NET_RETRY_BASE_SLEEP:-2}"
  case "$attempts" in ''|*[!0-9]*) attempts=4;; esac
  attempts=$((10#$attempts))
  [ "$attempts" -ge 1 ] || attempts=4
  case "$base" in ''|*[!0-9]*) base=2;; esac
  base=$((10#$base))
  local name="$1"
  [ "$name" = command ] && [ $# -gt 1 ] && name="$2"
  local tmp="${TMPDIR:-/tmp}" i=1 rc out err wait
  while :; do
    out=$(mktemp "$tmp/net-retry-out.XXXXXX" 2>/dev/null) || out=""
    err=$(mktemp "$tmp/net-retry-err.XXXXXX" 2>/dev/null) || err=""
    if [ -z "$out" ] || [ -z "$err" ]; then
      rm -f "$out" "$err"
      rc=0
      "$@" || rc=$?
      return "$rc"
    fi
    rc=0
    "$@" >"$out" 2>"$err" || rc=$?
    if [ "$rc" -eq 0 ]; then
      cat "$out"; cat "$err" >&2
      rm -f "$out" "$err"
      return 0
    fi
    if [ "$i" -lt "$attempts" ] \
      && ! grep -qiE 'HTTP 4[0-9][0-9]' "$err" \
      && grep -qiE 'error connecting to|could not resolve host|no such host|dial tcp|connection refused|connection reset|i/o timeout|tls handshake timeout|context deadline exceeded|timed out|HTTP 50[0-4]' "$err"; then
      rm -f "$out" "$err"
      wait=$((base * (1 << (i - 1))))
      echo "net-retry: $name transport error, attempt $i/$attempts, sleeping ${wait}s" >&2
      sleep "$wait"
      i=$((i + 1))
      continue
    fi
    cat "$out"; cat "$err" >&2
    rm -f "$out" "$err"
    return "$rc"
  done
}

# Shadow gh and uzi with retrying functions, each only when the executable resolves, so a
# missing uzi stays missing (`command -v uzi` keeps failing).
net_retry_install() {
  if command -v gh >/dev/null 2>&1; then
    gh() { net_retry command gh "$@"; }
  fi
  if command -v uzi >/dev/null 2>&1; then
    uzi() { net_retry command uzi "$@"; }
  fi
}
