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
  ! grep -Eq 'clearing complete|: done\.' "$TMP/out"
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
    ! grep -Eq 'clearing complete|: done\.' "$TMP/out"
    echo "PASS: $mode initial refusal $input (no client)"
  done
done
echo 'PASS: hermetic reset contract'
