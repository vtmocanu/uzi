#!/usr/bin/env bash
# Behavioral test for reclaim-leaked-e2e.sh, driven by a FAKE `docker` on PATH.
# Proves the safety contract without touching real containers or volumes:
#   - a definitely-dead `uzi-e2e-<pid>` project IS reclaimed;
#   - an EPERM pid (init, pid 1), the current run's own project, the real dev
#     stack `uzi`, and store-it's `uzi-store-it-*` are all SKIPPED;
#   - UZI_E2E_NO_RECLAIM=1 reclaims nothing.
#   - the normal entrypoint preserves that opt-out across env -i while scrubbing
#     compose configuration; absent the opt-out, reclaim still runs.
# Exits non-zero on any failed assertion.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/reclaim-leaked-e2e.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAKEBIN="$TMP/bin"
mkdir -p "$FAKEBIN"
LOG="$TMP/down.log"
CURRENT="uzi-e2e-$$"

# A pid that is provably dead at test time: spawn a trivial child and reap it,
# rather than hardcoding a high number (which could be a live process on a host
# with a raised kernel.pid_max, spuriously reddening this gate).
sleep 0 & DEAD_PID=$!
wait "$DEAD_PID" 2>/dev/null || true
DEAD="uzi-e2e-$DEAD_PID"

# Fake docker. The candidate list is emitted for `docker ps`; `docker compose -p
# <proj> down ...` appends <proj> to the log so we can assert exactly what got
# torn down. UNquoted heredoc: $LOG, $CURRENT and $DEAD expand now (test time),
# while the fake's own positional args (\$1, \$3) stay literal for run time.
cat > "$FAKEBIN/docker" <<EOF
#!/usr/bin/env bash
case "\$1" in
  ps)
    printf '%s\n' '$DEAD' 'uzi-e2e-1' '$CURRENT' 'uzi' 'uzi-store-it-12345'
    ;;
  volume)
    : ;;
  compose)
    printf '%s\n' "\$3" >> '$LOG'
    ;;
esac
EOF
chmod +x "$FAKEBIN/docker"

PATH="$FAKEBIN:$PATH"
export PATH

fails=0

check_absent() {
  if grep -qx "$1" "$LOG"; then
    printf 'FAIL: %s was torn down but must be skipped\n' "$1"
    fails=$((fails + 1))
  fi
}

# --- Sub-test A: normal reclaim -------------------------------------------
: > "$LOG"
bash "$SCRIPT" "$CURRENT" >/dev/null

if grep -qx "$DEAD" "$LOG"; then
  printf 'PASS: dead-pid project %s was reclaimed\n' "$DEAD"
else
  printf 'FAIL: dead-pid project %s was NOT reclaimed\n' "$DEAD"
  fails=$((fails + 1))
fi

check_absent 'uzi-e2e-1'         # pid 1: EPERM (non-root) or signal-delivered (root) -> alive
check_absent "$CURRENT"          # own run: alive + name-excluded
check_absent 'uzi'               # regex miss (real dev stack)
check_absent 'uzi-store-it-12345' # regex miss (store-it)

lines="$(grep -c . "$LOG" || true)"
if [ "$lines" = "1" ]; then
  printf 'PASS: exactly one project torn down\n'
else
  printf 'FAIL: expected exactly 1 teardown, got %s\n' "$lines"
  fails=$((fails + 1))
fi

# --- Sub-test B: opt-out --------------------------------------------------
: > "$LOG"
UZI_E2E_NO_RECLAIM=1 bash "$SCRIPT" "$CURRENT" >/dev/null
if [ -s "$LOG" ]; then
  printf 'FAIL: UZI_E2E_NO_RECLAIM=1 still tore down projects\n'
  fails=$((fails + 1))
else
  printf 'PASS: UZI_E2E_NO_RECLAIM=1 tore down nothing\n'
fi

# Drive the real entrypoint's re-exec through its shift, then stop before stack
# provisioning. Keep the actual reclaim script and fake Docker from this test.
# This pins the opt-out across env -i without running any real stack operation.
awk '{ print } /^shift / { found=1; exit } END { if (!found) exit 1 }' \
  "$HERE/run-e2e.sh" > "$TMP/run-e2e.sh"
cat >> "$TMP/run-e2e.sh" <<'EOF'
[ -z "${TRUSTED_PROXIES+x}" ] && [ -z "${JWT_SECRET+x}" ] \
  || { echo 'FAIL: entrypoint leaked a compose configuration variable'; exit 1; }
bash "$(dirname "${BASH_SOURCE[0]}")/reclaim-leaked-e2e.sh" "$UZI_E2E_COMPOSE_PROJECT"
EOF
cp "$SCRIPT" "$TMP/reclaim-leaked-e2e.sh"

: > "$LOG"
if UZI_E2E_NO_RECLAIM=1 UZI_E2E_COMPOSE_PROJECT="$CURRENT" \
  TRUSTED_PROXIES=must-be-scrubbed JWT_SECRET=must-be-scrubbed \
  bash "$TMP/run-e2e.sh" > "$TMP/sanitized.log" && \
  [ ! -s "$LOG" ] && grep -qF '[reclaim] skipped (UZI_E2E_NO_RECLAIM set)' "$TMP/sanitized.log"; then
  printf 'PASS: normal entrypoint preserves no-reclaim and scrubs compose configuration\n'
else
  printf 'FAIL: normal entrypoint lost no-reclaim or leaked compose configuration\n'
  fails=$((fails + 1))
fi

: > "$LOG"
if env -u UZI_E2E_NO_RECLAIM UZI_E2E_COMPOSE_PROJECT="$CURRENT" \
  TRUSTED_PROXIES=must-be-scrubbed JWT_SECRET=must-be-scrubbed \
  bash "$TMP/run-e2e.sh" >/dev/null && [ "$(cat "$LOG")" = "$DEAD" ]; then
  printf 'PASS: normal entrypoint still reclaims with no opt-out\n'
else
  printf 'FAIL: normal entrypoint changed default reclaim behavior\n'
  fails=$((fails + 1))
fi

if [ "$fails" -ne 0 ]; then
  printf 'FAIL: %d check(s) failed\n' "$fails"
  exit 1
fi
printf 'PASS: all checks passed\n'
