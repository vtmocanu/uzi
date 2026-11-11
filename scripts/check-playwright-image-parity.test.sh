#!/bin/sh
# Hermetic contract tests for scripts/check-playwright-image-parity.sh: synthetic
# workflow and package.json files prove 0 (parity), 1 (version drift) and 2 (image
# line or npm pin missing, duplicated or malformed), so a gutted check cannot read
# green. Ends with the real repo files, which must agree.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
SUT="$ROOT/scripts/check-playwright-image-parity.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  echo "check-playwright-image-parity.test.sh: FAIL: $*" >&2
  exit 1
}

D=2f05f8a734509a7b3f26a912639488ee08f58a7b292d14d62271bcff9038768f

# check <case> <expected-rc> <ci container lines> <package.json devDependency lines>
check() {
  printf 'jobs:\n  t:\n    container:\n%s\n' "$3" > "$TMP/$1.ci.yml"
  printf '{\n  "devDependencies": {\n%s\n  }\n}\n' "$4" > "$TMP/$1.package.json"
  rc=0
  "$SUT" "$TMP/$1.ci.yml" "$TMP/$1.package.json" > "$TMP/$1.out" 2>&1 || rc=$?
  [ "$rc" = "$2" ] || fail "$1: exit $rc, want $2 ($(cat "$TMP/$1.out"))"
}

IMG="      image: mcr.microsoft.com/playwright:v1.64.1-noble@sha256:$D"
PW='    "playwright": "1.64.1"'

check same     0 "$IMG" "$PW"
check drift    1 "$IMG" '    "playwright": "1.65.0"'
check imgdrift 1 "      image: mcr.microsoft.com/playwright:v1.63.0-noble@sha256:$D" "$PW"
check nodig    2 "      image: mcr.microsoft.com/playwright:v1.64.1-noble" "$PW"
check short    2 "      image: mcr.microsoft.com/playwright:v1.64.1-noble@sha256:${D%?}" "$PW"
check noimg    2 "      image: ubuntu:24.04" "$PW"
check twice    2 "$IMG
$IMG" "$PW"
check range    2 "$IMG" '    "playwright": "^1.64.1"'
check nopw     2 "$IMG" '    "vitest": "4.1.12"'
check twopw    2 "$IMG" "$PW,
$PW"
# @vitest/browser-playwright must not count as the playwright pin.
check vitestpw 0 "$IMG" '    "@vitest/browser-playwright": "4.1.12",
    "playwright": "1.64.1"'

"$SUT" "$ROOT/.github/workflows/ci.yml" "$ROOT/web/package.json" > "$TMP/real.out" 2>&1 \
  || fail "real repo files: $(cat "$TMP/real.out")"
echo "check-playwright-image-parity.test.sh: OK"
