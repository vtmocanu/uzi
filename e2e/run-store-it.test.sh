#!/usr/bin/env bash
# Behavioral regression test for run-store-it.sh's throwaway-Postgres teardown,
# driven by a FAKE `docker` on PATH. Proves the EXIT-trap cleanup removes the
# container's ANONYMOUS DATA VOLUME (docker rm -v), not just the container -- the
# leak that filled the worker's Docker data-root before this fix. No real docker
# daemon, no Postgres and no `go test`: the fake makes pg_isready never ready, so
# the script exits down its readiness-timeout path straight into cleanup.
# Exits non-zero on any failed assertion.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/run-store-it.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAKEBIN="$TMP/bin"
mkdir -p "$FAKEBIN"
LOG="$TMP/docker.log"

# Fake docker. Every invocation appends its argv to $LOG (a FILE, so it survives
# the real cleanup's `>/dev/null 2>&1` redirect), then returns per-subcommand:
#   image -> 0  (image present; the pull block is skipped)
#   run   -> 0  (container "started")
#   exec  -> 1  (pg_isready never ready -> readiness timeout -> exit 1 -> trap)
#   rm    -> 0  (record the teardown)
# UNquoted heredoc: $LOG expands now (test time); the fake's own \$* / \$1 stay
# literal for run time.
cat > "$FAKEBIN/docker" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >> '$LOG'
case "\$1" in
  image) exit 0 ;;
  run)   exit 0 ;;
  exec)  exit 1 ;;
  rm)    exit 0 ;;
  *)     exit 0 ;;
esac
EOF
chmod +x "$FAKEBIN/docker"

PATH="$FAKEBIN:$PATH"
export PATH

fails=0

# Drive the real script: readiness times out in ~1s (PGWAIT=1), so it exits 1 down
# the infrastructure-failure path, firing the EXIT trap (cleanup). A non-zero exit
# is expected; `|| true` keeps `set -e` from aborting the test on it.
UZI_STORE_IT_PG_WAIT_SECS=1 bash "$SCRIPT" >/dev/null 2>&1 || true

rm_count="$(grep -c '^rm ' "$LOG" || true)"
rm_line="$(grep -m1 '^rm ' "$LOG" || true)"

if [ "$rm_count" = "1" ]; then
  printf 'PASS: cleanup ran exactly one `docker rm`\n'
else
  printf 'FAIL: expected exactly one `docker rm`, got %s\n' "$rm_count"
  fails=$((fails + 1))
fi

# THE REGRESSION: the teardown must remove the anonymous volume (docker rm -v),
# not just the container. RED on the pre-fix `docker rm -f "$NAME"`.
case " $rm_line " in
  *" -v "*) printf 'PASS: `docker rm` carries -v (anonymous volume removed)\n' ;;
  *)        printf 'FAIL: `docker rm` is missing -v -> anonymous volume leaks: %s\n' "$rm_line"
            fails=$((fails + 1)) ;;
esac

case "$rm_line" in
  *uzi-store-it-*) printf 'PASS: teardown targets the throwaway container by name\n' ;;
  *)               printf 'FAIL: teardown did not target uzi-store-it-<pid>: %s\n' "$rm_line"
                   fails=$((fails + 1)) ;;
esac

if [ "$fails" -eq 0 ]; then
  printf '\nrun-store-it teardown test: all assertions passed\n'
  exit 0
fi
printf '\nrun-store-it teardown test: %s assertion(s) failed\n' "$fails" >&2
exit 1
