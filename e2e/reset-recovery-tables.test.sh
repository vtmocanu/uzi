#!/usr/bin/env bash
# Hermetic client contract: PATH contains only selected clients and required tools.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${RESET_SCRIPT:-$ROOT/e2e/reset-recovery-tables.sh}"
mkdir -p "$ROOT/.uzi/scratch"
TMP="$(mktemp -d "$ROOT/.uzi/scratch/reset-hermetic.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
mkdir "$TMP/native" "$TMP/docker"
for bin in dirname cat; do
  tool="$(command -v "$bin")"
  ln -s "$tool" "$TMP/native/$bin"
  ln -s "$tool" "$TMP/docker/$bin"
done
BASH_BIN="$(command -v bash)"
fail() { echo "FAIL: $*" >&2; exit 1; }
for mode in native docker; do
  client=docker
  if [ "$mode" = native ]; then client=psql; fi
  printf '#!%s\n' "$BASH_BIN" > "$TMP/$mode/$client"
  cat >> "$TMP/$mode/$client" <<'STUB'
printf '%s\0' "$@" > "$RECORD/argv"
cat > "$RECORD/sql"
echo 'client diagnostic' >&2
exit "${CLIENT_RC:-0}"
STUB
  chmod +x "$TMP/$mode/$client"
done
dsn='postgres://user:password@127.0.0.1:23456/test?sslmode=disable&application_name=two words'
run() {
  local mode="$1" rc_expected="$2"; shift 2
  rm -f "$TMP/argv" "$TMP/sql"
  local rc=0
  env -i PATH="$TMP/$mode" RECORD="$TMP" CLIENT_RC="${CLIENT_RC:-0}" \
    DATABASE_URL="$dsn" UZI_TEST_DATABASE_URL="${TARGET:-}" UZI_STORE_IT_PG_IMAGE="${TEST_IMAGE:-postgres:17}" \
    "$BASH_BIN" "$SCRIPT" "$@" > "$TMP/out" 2>&1 || rc=$?
  [ "$rc" -eq "$rc_expected" ] || { cat "$TMP/out"; echo "FAIL: exit $rc expected $rc_expected"; exit 1; }
}
for mode in native docker; do
  for source in explicit positional env; do
    TARGET=""
    case "$source" in
      explicit) run "$mode" 0 --dsn "$dsn" ;;
      positional) run "$mode" 0 "$dsn" ;;
      env) TARGET="$dsn"; run "$mode" 0 ;;
    esac
    python3 - "$TMP" "$mode" "$dsn" <<'PY'
import pathlib, sys
p, mode, dsn = sys.argv[1:]
p = pathlib.Path(p)
args = p.joinpath('argv').read_bytes().split(b'\0')[:-1]
expected = ['-X', dsn, '-v', 'ON_ERROR_STOP=1', '-q']
if mode == 'docker':
    expected = ['run', '--rm', '-i', '--network', 'host', 'postgres:17', 'psql'] + expected
assert args == [x.encode() for x in expected], args
assert 'clearing complete' in p.joinpath('out').read_text()
sql = p.joinpath('sql').read_bytes()
reference = p / 'reference'
if reference.exists():
    assert sql == reference.read_bytes()
else:
    reference.write_bytes(sql)
PY
    echo "PASS: $mode $source argv and stdin"
  done
  if [ "$mode" = docker ]; then
    TEST_IMAGE=postgres:test-owned run "$mode" 0 --dsn "$dsn"
    python3 - "$TMP/argv" <<'PYIMAGE'
import pathlib, sys
args = pathlib.Path(sys.argv[1]).read_bytes().split(b'\0')[:-1]
assert args[5] == b'postgres:test-owned', args
PYIMAGE
    echo 'PASS: Docker client uses selected image'
  fi
  CLIENT_RC=7 run "$mode" 7 --dsn "$dsn"
  if grep -Eq 'clearing complete|: done\.' "$TMP/out"; then
    fail "$mode: failed client printed a success banner"
  fi
  grep -Fq 'client diagnostic' "$TMP/out"
  echo "PASS: $mode client failure"
  TARGET=""
  for input in absent flag remote tls; do
    case "$input" in
      absent) run "$mode" 2 ;;
      flag) run "$mode" 2 --force ;;
      remote) run "$mode" 3 'postgres://db/test?sslmode=disable' ;;
      tls) run "$mode" 3 'postgres://localhost/test' ;;
    esac
    [ ! -e "$TMP/argv" ]
    if grep -Eq 'clearing complete|: done\.' "$TMP/out"; then
      fail "$mode: initial refusal $input printed a success banner"
    fi
    echo "PASS: $mode initial refusal $input (no client)"
  done
done
# Drive live-harness setup with fake Docker only. Failed setup must never remove
# objects it did not create, including existing names and partial initialization.
mkdir "$TMP/ownership-bin"
cat > "$TMP/ownership-bin/docker" <<'STUB'
#!/usr/bin/env bash
FIXTURE="$(cd "$(dirname "$0")/.." && pwd)"
OWNERSHIP_CASE="$(cat "$FIXTURE/ownership.case")"
OWNERSHIP_ROOT="$(cat "$FIXTURE/ownership.root")"
OWNERSHIP_LOG="$FIXTURE/ownership.log"
printf '%s\0' "$@" >> "$OWNERSHIP_LOG"
printf '\n' >> "$OWNERSHIP_LOG"
case "${1:-}:${2:-}" in
  info:*) exit 0 ;;
  image:inspect) [ "$OWNERSHIP_CASE" != pull-fail ]; exit $? ;;
  pull:*) exit 1 ;;
  container:inspect) [ "$OWNERSHIP_CASE" = container-exists ]; exit $? ;;
  volume:inspect)
    case "$OWNERSHIP_CASE:${3:-}" in
      old-volume-exists:*-old-data) exit 0 ;;
      volume-exists:*-old-data) exit 1 ;;
      volume-exists:*) exit 0 ;;
      *) exit 1 ;;
    esac ;;
  volume:create)
    case "$OWNERSHIP_CASE:${3:-}" in second-volume-fail:*-old-data) exit 1 ;; esac
    exit 0 ;;
  compose:*)
    [ "$OWNERSHIP_CASE" = run-fail ] || [ "$OWNERSHIP_CASE" = run-success ] || exit 1
    python3 - "$OWNERSHIP_ROOT" <<'PYCONFIG'
import json, sys
print(json.dumps({'services': {'db': {'volumes': [
    {'type': 'bind', 'source': sys.argv[1] + '/e2e/test-database-marker.sql',
     'target': '/docker-entrypoint-initdb.d/10-uzi-test-database-marker.sql', 'read_only': True},
    {'type': 'volume', 'source': 'pgdata', 'target': '/var/lib/postgresql/data'}]}}}))
PYCONFIG
    ;;
  run:*) [ "$OWNERSHIP_CASE" != run-fail ] || exit 125; echo owned-container-id ;;
  exec:*) exit 0 ;;
  port:*) echo '127.0.0.1:23456' ;;
  rm:*|volume:rm) exit 0 ;;
  *) echo "unexpected fake Docker call: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "$TMP/ownership-bin/docker"
for scenario in pull-fail container-exists volume-exists old-volume-exists second-volume-fail compose-fail run-fail run-success; do
  : > "$TMP/ownership.log"
  printf '%s\n' "$scenario" > "$TMP/ownership.case"
  printf '%s\n' "$ROOT" > "$TMP/ownership.root"
  rc=0
  PATH="$TMP/ownership-bin:$PATH" OWNERSHIP_CASE="$scenario" OWNERSHIP_ROOT="$ROOT" \
    OWNERSHIP_LOG="$TMP/ownership.log" RESET_CLIENT_MODE=docker \
    bash "$ROOT/e2e/reset-recovery-tables.live.test.sh" > "$TMP/ownership.out" 2>&1 || rc=$?
  [ "$rc" -ne 0 ] || fail "$scenario: fake setup failure unexpectedly passed"
  python3 - "$TMP" "$scenario" <<'PYOWNERSHIP'
from pathlib import Path
import sys
root, scenario = Path(sys.argv[1]), sys.argv[2]
calls = [[part.decode() for part in row.split(b'\0')[:-1]]
         for row in root.joinpath('ownership.log').read_bytes().splitlines()]
names = [call[2] for call in calls if call[:2] in (['container', 'inspect'], ['rm', '-f'])]
assert names, calls
name = names[0]
assert name.startswith('rrt-live-'), name
creates = [call for call in calls if call[:2] == ['volume', 'create'] or call[:1] == ['run']]
removes = [call for call in calls if call[:1] == ['rm'] or call[:2] == ['volume', 'rm']]
expected = []
if scenario == 'second-volume-fail':
    expected = [['volume', 'rm', name + '-data']]
elif scenario in ('compose-fail', 'run-fail', 'run-success'):
    if scenario == 'run-success':
        expected.append(['rm', '-f', name])
    expected += [['volume', 'rm', name + '-data'], ['volume', 'rm', name + '-old-data']]
assert removes == expected, (scenario, removes, expected)
if scenario in ('pull-fail', 'container-exists', 'volume-exists', 'old-volume-exists'):
    assert creates == [], (scenario, creates)
if scenario.endswith('exists'):
    assert 'already exists' in root.joinpath('ownership.out').read_text()
PYOWNERSHIP
  echo "PASS: live ownership $scenario (fake Docker)"
done
echo 'PASS: hermetic reset contract'
