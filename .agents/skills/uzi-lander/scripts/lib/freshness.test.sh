#!/usr/bin/env bash
# freshness.test.sh — skill_scripts_stale reads 0 on a match, 1 behind or edited, unknown
# outside git or without origin/main. Hermetic: a throwaway repo and a bare "origin".
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/freshness.sh
. "$HERE/freshness.sh"
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
WORK=$(cd -P "$WORK" && pwd)
# A TMPDIR inside a checkout (a worker's .uzi/scratch) must not let the "non-git dir"
# case discover that parent repository: stop Git discovery above $WORK.
export GIT_CEILING_DIRECTORIES="${WORK%/*}"
fail() { echo "FAIL: $*" >&2; exit 1; }
g() { git -C "$WORK/repo" -c user.name=t -c user.email=t@example.com "$@"; }

[ "$(skill_scripts_stale "$WORK")" = unknown ] || fail "a non-git dir did not read unknown"

git init -q --bare -b main "$WORK/origin.git"
git init -q -b main "$WORK/repo"
S="$WORK/repo/.agents/skills/uzi-lander/scripts"
mkdir -p "$S"; echo v1 > "$S/x.sh"
g add -A; g commit -qm v1
[ "$(skill_scripts_stale "$S")" = unknown ] || fail "no origin/main did not read unknown"

g remote add origin "$WORK/origin.git"; g push -q origin main; g fetch -q origin
[ "$(skill_scripts_stale "$S")" = 0 ] || fail "a matching checkout did not read 0"

# origin/main moves on (another lander merged a script fix); this checkout did not pull.
git clone -q "$WORK/origin.git" "$WORK/other"
echo v2 > "$WORK/other/.agents/skills/uzi-lander/scripts/x.sh"
git -C "$WORK/other" -c user.name=t -c user.email=t@example.com commit -qam v2
git -C "$WORK/other" push -q origin main
g fetch -q origin
[ "$(skill_scripts_stale "$S")" = 1 ] || fail "a checkout behind origin/main did not read 1"

# A change outside the skill dir does not count.
g reset -q --hard origin/main; echo n > "$WORK/repo/README"; g add README; g commit -qm other
[ "$(skill_scripts_stale "$S")" = 0 ] || fail "a change outside the skill dir read as stale"

echo "PASS freshness: match 0, behind 1, outside-dir change 0, no git/origin unknown"
