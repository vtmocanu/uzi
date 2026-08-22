#!/usr/bin/env bash
# Behavioral test for reclaim-leaked-e2e.sh, driven by a FAKE `docker` on PATH.
# Proves the safety contract without touching real containers or volumes:
#   - a definitely-dead `uzi-e2e-<pid>` project IS reclaimed;
#   - an EPERM pid (init, pid 1), the current run's own project, the real dev
#     stack `uzi`, and store-it's `uzi-store-it-*` are all SKIPPED;
#   - UZI_E2E_NO_RECLAIM=1 reclaims nothing.
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

if [ "$fails" -ne 0 ]; then
  printf 'FAIL: %d check(s) failed\n' "$fails"
  exit 1
fi
printf 'PASS: all checks passed\n'
