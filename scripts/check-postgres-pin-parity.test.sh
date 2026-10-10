#!/bin/sh
# Hermetic contract tests for scripts/check-postgres-pin-parity.sh: synthetic compose
# workflow and store-it files prove 0 (parity), 1 (pin drift in digest or tag, or a
# floating postgres tag off the pin) and 2 (a pin missing, duplicated or malformed), so
# a gutted check cannot read green. Ends with the real repo files, which must agree.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
SUT="$ROOT/scripts/check-postgres-pin-parity.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  echo "check-postgres-pin-parity.test.sh: FAIL: $*" >&2
  exit 1
}

A=5c855ad7b85e68e48a62f34662853f38b57c1c1d80f3a927ab58034fd6d31c5e
B=2d2b8998d31037bf721cfdf764d76ba74171b4fab3431b7f72c27c56ddbdf9e3

# check <case> <expected-rc> <compose image line> <ci image line> [<ci extra> [<store-it default>]]
check() {
  printf 'services:\n  db:\n%s\n' "$3" > "$TMP/$1.compose.yml"
  printf 'jobs:\n  t:\n    services:\n      postgres:\n%s\n    steps:\n      - run: ./scripts/docker-pull-fallback.sh %s\n' \
    "$4" "${5:-postgres:17}" > "$TMP/$1.ci.yml"
  printf 'PGIMAGE="${UZI_STORE_IT_PG_IMAGE:-%s}"\n' "${6:-postgres:17}" > "$TMP/$1.storeit.sh"
  rc=0
  "$SUT" "$TMP/$1.compose.yml" "$TMP/$1.ci.yml" "$TMP/$1.storeit.sh" > "$TMP/$1.out" 2>&1 || rc=$?
  [ "$rc" = "$2" ] || fail "$1: exit $rc, want $2 ($(cat "$TMP/$1.out"))"
}

check same    0 "    image: postgres:17@sha256:$A" "        image: mirror.gcr.io/library/postgres:17@sha256:$A"
check digest  1 "    image: postgres:17@sha256:$A" "        image: mirror.gcr.io/library/postgres:17@sha256:$B"
check tag     1 "    image: postgres:18@sha256:$A" "        image: mirror.gcr.io/library/postgres:17@sha256:$A"
check nodig   2 "    image: postgres:17"           "        image: mirror.gcr.io/library/postgres:17@sha256:$A"
check short   2 "    image: postgres:17@sha256:${A%?}" "        image: mirror.gcr.io/library/postgres:17@sha256:$A"
check hubci   2 "    image: postgres:17@sha256:$A" "        image: postgres:17@sha256:$A"
check twice   2 "    image: postgres:17@sha256:$A
    image: postgres:17@sha256:$B" "        image: mirror.gcr.io/library/postgres:17@sha256:$A"

check extrac  2 "    image: postgres:17@sha256:$A
    image: postgres:18" "        image: mirror.gcr.io/library/postgres:17@sha256:$A"
check extraci 2 "    image: postgres:17@sha256:$A" "        image: mirror.gcr.io/library/postgres:17@sha256:$A
        image: mirror.gcr.io/library/postgres:17@sha256:${A%?}"
check floatci 1 "    image: postgres:17@sha256:$A" "        image: mirror.gcr.io/library/postgres:17@sha256:$A" postgres:18
check floatsi 1 "    image: postgres:17@sha256:$A" "        image: mirror.gcr.io/library/postgres:17@sha256:$A" postgres:17 postgres:16
check notname 0 "    image: postgres:17@sha256:$A" "        image: mirror.gcr.io/library/postgres:17@sha256:$A" my-postgres:9

rc=0
(cd "$ROOT" && "$SUT") > "$TMP/repo.out" 2>&1 || rc=$?
[ "$rc" = 0 ] || fail "repo: docker-compose.yml and ci.yml postgres pins differ ($(cat "$TMP/repo.out"))"

echo "check-postgres-pin-parity.test.sh: PASS (12 synthetic cases + real files)"
