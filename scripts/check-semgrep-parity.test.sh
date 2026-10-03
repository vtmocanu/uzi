#!/bin/sh
# Hermetic fixtures for the parity gate's 0/1/2 contract and the worker-version
# reader it shares with CI; no installs or network.
set -eu
ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
CHECK="$ROOT/scripts/check-semgrep-parity.sh"
VERSION="$ROOT/scripts/semgrep-worker-version.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
cases=0
passed=0
FLOOR=18

workflow() {
  cat > "$TMP/ci.yml" <<'YML'
jobs:
  lint:
    steps:
      - name: Install semgrep
        run: |
          SEMGREP_VERSION="$(./scripts/semgrep-worker-version.sh)"
          pipx install "semgrep==${SEMGREP_VERSION}"
YML
}
lock() {
  cat > "$TMP/devbox.lock" <<JSON
{"packages":{"semgrep":{"systems":{"x86_64-linux":{"outputs":[
  {"path":"/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-python3.14-semgrep-$1","default":true}
]}}}}}
JSON
}
expect() {
  cases=$((cases + 1))
  rc=0
  "$CHECK" "$TMP/ci.yml" "$TMP/devbox.lock" > "$TMP/out" 2>&1 || rc=$?
  if [ "$rc" = "$1" ] && grep -Fq "$2" "$TMP/out"; then
    passed=$((passed + 1))
  else
    echo "FAIL: $3: expected exit $1 and '$2', got $rc" >&2
    cat "$TMP/out" >&2
  fi
}

workflow; lock 1.172.0
expect 0 "OK: CI installs the worker lock's semgrep 1.172.0" 'lock-derived install'

lock 1.178.0
expect 0 "OK: CI installs the worker lock's semgrep 1.178.0" 'a lock refresh needs no CI edit'

cases=$((cases + 1))
if [ "$("$VERSION" "$TMP/devbox.lock")" = 1.178.0 ]; then passed=$((passed + 1)); else echo "FAIL: version reader output" >&2; fi

workflow
printf '      - run: pipx install semgrep==1.178.0\n' >> "$TMP/ci.yml"
expect 1 'outside the lock-derived line' 'an extra literal pin (a Renovate bump)'

printf 'jobs:\n  lint:\n    steps:\n      - run: pipx install semgrep==1.172.0\n' > "$TMP/ci.yml"
expect 1 'derive=0' 'a literal pin replacing the derivation'

workflow
printf '      - run: pip install semgrep\n' >> "$TMP/ci.yml"
expect 1 'outside the lock-derived line' 'an unpinned pip install'

workflow
printf '          SEMGREP_VERSION="$(./scripts/semgrep-worker-version.sh)"\n' >> "$TMP/ci.yml"
expect 1 'derive=2' 'a duplicated derivation is ambiguous'

workflow
gsed -i 's/^\( *\)\(SEMGREP_VERSION=\)/\1# \2/' "$TMP/ci.yml" 2>/dev/null || sed -i 's/^\( *\)\(SEMGREP_VERSION=\)/\1# \2/' "$TMP/ci.yml"
expect 1 'derive=0' 'a commented derivation cannot pass'

# Each case below passed the earlier line-counting check (false greens).
workflow
awk '{ print } /SEMGREP_VERSION="/ { print "          SEMGREP_VERSION=1.178.0" }' "$TMP/ci.yml" > "$TMP/ci2.yml" && mv "$TMP/ci2.yml" "$TMP/ci.yml"
expect 1 'paired=0' 'an override between derive and install'

workflow
printf '      - env:\n          SEMGREP_VERSION: 1.178.0\n' >> "$TMP/ci.yml"
expect 1 'other SEMGREP_VERSION assignment' 'another SEMGREP_VERSION assignment'

workflow
printf '          pipx install "semgrep==${SEMGREP_VERSION}"; pipx install semgrep==1.178.0\n' >> "$TMP/ci.yml"
expect 1 'outside the lock-derived line' 'a pin chained onto the approved install text'

workflow
printf '          pipx install semgrep==1.178.0 # pipx install "semgrep==${SEMGREP_VERSION}"\n' >> "$TMP/ci.yml"
expect 1 'outside the lock-derived line' 'the approved text in a comment exempts nothing'

workflow
awk '/pipx install/ { print "          true" } { print }' "$TMP/ci.yml" > "$TMP/ci2.yml" && mv "$TMP/ci2.yml" "$TMP/ci.yml"
expect 1 'paired=0' 'derive and install not adjacent'

workflow
printf '{broken json\n' > "$TMP/devbox.lock"
expect 2 'INSTRUMENT BROKEN' 'invalid lock JSON'

printf '{"packages":{}}\n' > "$TMP/devbox.lock"
expect 2 'INSTRUMENT BROKEN' 'missing worker output'

lock unknown
expect 2 'INSTRUMENT BROKEN' 'unparseable worker version'

lock 1.172.0
jq '.packages.semgrep.systems["x86_64-linux"].outputs += .packages.semgrep.systems["x86_64-linux"].outputs' \
  "$TMP/devbox.lock" > "$TMP/duplicate.lock"
mv "$TMP/duplicate.lock" "$TMP/devbox.lock"
expect 2 'INSTRUMENT BROKEN' 'multiple default worker outputs are ambiguous'

lock 1.172.0
rm -f "$TMP/ci.yml"
expect 2 'INSTRUMENT BROKEN' 'unreadable workflow'

echo "check-semgrep-parity.test.sh: cases=$cases passed=$passed"
[ "$cases" -ge "$FLOOR" ] && [ "$passed" = "$cases" ]
