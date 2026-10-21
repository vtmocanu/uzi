#!/bin/sh
# Hermetic fixtures for the actionlint/zizmor parity gate's 0/1/2 contract and the
# worker-version reader it shares with CI; no installs or network.
set -eu
ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
CHECK="$ROOT/scripts/check-actions-tool-parity.sh"
VERSION="$ROOT/scripts/worker-tool-version.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
cases=0
passed=0
FLOOR=24

workflow() {
  cat > "$TMP/ci.yml" <<'YML'
jobs:
  lint:
    steps:
      - name: Install actionlint (pinned) + zizmor (the worker lock's version)
        run: |
          # renovate-tarball: depName=rhysd/actionlint
          ACTIONLINT_VERSION="v1.7.12"          # v-prefixed git tag
          ACTIONLINT_V="${ACTIONLINT_VERSION#v}" # tarball filename is versionless
          tar -xzf /tmp/actionlint.tar.gz -C "${HOME}/.local/bin" actionlint
          ZIZMOR_VERSION="$(./scripts/worker-tool-version.sh zizmor)"
          pipx install "zizmor==${ZIZMOR_VERSION}"
YML
}
# lock <actionlint version> <zizmor version>
lock() {
  cat > "$TMP/devbox.lock" <<JSON
{"packages":{
  "actionlint":{"systems":{"x86_64-linux":{"outputs":[
    {"path":"/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-actionlint-$1","default":true}]}}},
  "zizmor":{"systems":{"x86_64-linux":{"outputs":[
    {"path":"/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-zizmor-$2","default":true}]}}},
  "zsh":{"systems":{"x86_64-linux":{"outputs":[
    {"path":"/nix/store/cccccccccccccccccccccccccccccccc-zsh-5.9.2","default":true},
    {"name":"man","path":"/nix/store/dddddddddddddddddddddddddddddddd-zsh-5.9.2-man","default":true}]}}}
}}
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
expect_version() {
  cases=$((cases + 1))
  rc=0
  got="$("$VERSION" "$1" "$TMP/devbox.lock" 2>"$TMP/err")" || rc=$?
  if [ "$rc" = "$2" ] && [ "$got" = "$3" ]; then
    passed=$((passed + 1))
  else
    echo "FAIL: reader $1: expected exit $2 and '$3', got $rc '$got'" >&2
    cat "$TMP/err" >&2
  fi
}
sub() { gsed -i "$1" "$TMP/ci.yml" 2>/dev/null || sed -i "$1" "$TMP/ci.yml"; }

workflow; lock 1.7.12 1.30.1
expect 0 'agree on actionlint 1.7.12 and zizmor 1.30.1' 'matching pins'

lock 1.7.12 1.31.1
expect 0 'zizmor 1.31.1' 'a lock zizmor refresh needs no CI edit'

expect_version actionlint 0 1.7.12
expect_version zizmor 0 1.31.1
expect_version zsh 0 5.9.2
expect_version missing 2 ''
expect_version 'zizmor.*' 2 ''
# A regex-meta name is refused even when the spliced pattern would match.
jq '.packages["a+"] = {"systems":{"x86_64-linux":{"outputs":[
  {"path":"/nix/store/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee-aaa-1.0.0","default":true}]}}}' \
  "$TMP/devbox.lock" > "$TMP/meta.lock"
mv "$TMP/meta.lock" "$TMP/devbox.lock"
expect_version 'a+' 2 ''

# actionlint drift, either direction.
lock 1.7.13 1.30.1
expect 1 'CI actionlint v1.7.12 differs from the worker lock v1.7.13' 'a lock refresh moved actionlint'

workflow; lock 1.7.12 1.30.1
sub 's/ACTIONLINT_VERSION="v1.7.12"/ACTIONLINT_VERSION="v1.7.13"/'
expect 1 'CI actionlint v1.7.13 differs' 'a Renovate actionlint bump without the lock'

workflow
sub 's/^\( *\)\(ACTIONLINT_VERSION="v\)/\1# \2/'
expect 1 'found 0' 'a commented actionlint pin cannot pass'

workflow
printf '          ACTIONLINT_VERSION="v1.7.12"\n' >> "$TMP/ci.yml"
expect 1 'found 2' 'a duplicated actionlint pin is ambiguous'

workflow
printf '      - env:\n          ACTIONLINT_VERSION: v1.7.13\n' >> "$TMP/ci.yml"
expect 1 'other ACTIONLINT_VERSION assignment' 'an env-block actionlint version'

workflow
printf '      - run: go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.13\n' >> "$TMP/ci.yml"
expect 1 'actionlint install outside the pinned tarball' 'a second actionlint install'

# zizmor: the derive/install pair is the only version source.
workflow
printf '      - run: pipx install zizmor==1.31.1\n' >> "$TMP/ci.yml"
expect 1 'outside the lock-derived line' 'an extra literal zizmor pin (a Renovate bump)'

workflow
sub 's/^\( *\)ZIZMOR_VERSION=.*/\1true/'
sub 's/^\( *\)pipx install "zizmor==.*/\1pipx install zizmor==1.30.1/'
expect 1 'derive=0' 'a literal pin replacing the derivation'

workflow
printf '      - run: uvx zizmor@1.31.1 .github/workflows\n' >> "$TMP/ci.yml"
expect 1 'outside the lock-derived line' 'a uvx-pinned zizmor'

workflow
printf '          ZIZMOR_VERSION="$(./scripts/worker-tool-version.sh zizmor)"\n' >> "$TMP/ci.yml"
expect 1 'derive=2' 'a duplicated derivation is ambiguous'

workflow
sub 's/^\( *\)\(ZIZMOR_VERSION=\)/\1# \2/'
expect 1 'derive=0' 'a commented derivation cannot pass'

workflow
awk '{ print } /ZIZMOR_VERSION="/ { print "          ZIZMOR_VERSION=1.31.1" }' "$TMP/ci.yml" > "$TMP/ci2.yml" && mv "$TMP/ci2.yml" "$TMP/ci.yml"
expect 1 'paired=0' 'an override between derive and install'

workflow
printf '          pipx install "zizmor==${ZIZMOR_VERSION}"; pipx install zizmor==1.31.1\n' >> "$TMP/ci.yml"
expect 1 'outside the lock-derived line' 'a pin chained onto the approved install text'

workflow
printf '          pipx install zizmor==1.31.1 # pipx install "zizmor==${ZIZMOR_VERSION}"\n' >> "$TMP/ci.yml"
expect 1 'outside the lock-derived line' 'the approved text in a comment exempts nothing'

# Fail-closed inputs.
workflow
printf '{broken json\n' > "$TMP/devbox.lock"
expect 2 'INSTRUMENT BROKEN' 'invalid lock JSON'

printf '{"packages":{}}\n' > "$TMP/devbox.lock"
expect 2 'INSTRUMENT BROKEN' 'missing worker outputs'

lock 1.7.12 unknown
expect 2 'INSTRUMENT BROKEN' 'unparseable worker version'

lock 1.7.12 1.30.1
jq '.packages.zizmor.systems["x86_64-linux"].outputs += .packages.zizmor.systems["x86_64-linux"].outputs' \
  "$TMP/devbox.lock" > "$TMP/duplicate.lock"
mv "$TMP/duplicate.lock" "$TMP/devbox.lock"
expect 2 'INSTRUMENT BROKEN' 'multiple default worker outputs are ambiguous'

lock 1.7.12 1.30.1
rm -f "$TMP/ci.yml"
expect 2 'INSTRUMENT BROKEN' 'unreadable workflow'

echo "check-actions-tool-parity.test.sh: cases=$cases passed=$passed"
[ "$cases" -ge "$FLOOR" ] && [ "$passed" = "$cases" ]
