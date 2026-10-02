#!/bin/sh
# Hermetic fixtures for the parity gate's 0/1/2 contract; no installs or network.
set -eu
ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
CHECK="$ROOT/scripts/check-semgrep-parity.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
cases=0
passed=0
FLOOR=10

workflow() {
  printf 'jobs:\n  lint:\n    steps:\n      - name: Install semgrep\n        run: pipx install semgrep==%s\n' "$1" > "$TMP/ci.yml"
}
lock() {
  cat > "$TMP/devbox.lock" <<EOF
{"packages":{"semgrep":{"systems":{"x86_64-linux":{"outputs":[
  {"path":"/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-python3.14-semgrep-$1","default":true}
]}}}}}
EOF
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

workflow 1.172.0; lock 1.172.0
expect 0 'OK: CI and worker lock use semgrep 1.172.0' 'matching pins'

workflow 1.178.0
expect 1 'CI 1.178.0 != worker lock 1.172.0' 'independent CI bump'

workflow 1.172.0; lock 1.173.0
expect 1 'CI 1.172.0 != worker lock 1.173.0' 'worker lock bump needs CI update'

workflow 1.172.0; lock 1.172.0
printf '        run: pipx install semgrep==1.172.0\n' >> "$TMP/ci.yml"
expect 2 'INSTRUMENT BROKEN' 'duplicate CI pins are ambiguous'

printf '# run: pipx install semgrep==1.172.0\n' > "$TMP/ci.yml"
expect 2 'INSTRUMENT BROKEN' 'commented pin cannot pass'

workflow latest
expect 2 'INSTRUMENT BROKEN' 'unparseable CI version'

workflow 1.172.0
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

echo "check-semgrep-parity.test.sh: cases=$cases passed=$passed"
[ "$cases" -ge "$FLOOR" ] && [ "$passed" = "$cases" ]
