#!/usr/bin/env bash
# Hermetic exit-code contract for scripts/check-changelog-entry.sh: throwaway git repos with
# a local bare origin, no network. Each case builds a branch off origin/main and asserts the
# gate's status, so every exemption and the fail-closed paths are proven to fire. Prints a
# `cases=N passed=N` tally and fails below its case floor, so a gutted run cannot read green.
set -euo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FLOOR=29
cases=0
passed=0

export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

# fresh <name>: a clone of a bare origin whose main holds the check, its lib and a CHANGELOG.
fresh() {
  local d="$TMP/$1"
  mkdir -p "$d/seed/scripts/lib" "$d/seed/api" "$d/seed/deploy/chart"
  cp "$ROOT/scripts/check-changelog-entry.sh" "$d/seed/scripts/"
  cp "$ROOT/scripts/lib/shipping-paths.sh" "$d/seed/scripts/lib/"
  cp "$ROOT/scripts/lib/dependency-bump.sh" "$d/seed/scripts/lib/"
  printf '# Changelog\n\n## [Unreleased]\n\n' > "$d/seed/CHANGELOG.md"
  if [ "$#" -gt 1 ]; then printf '%s\n\n' "$2" >> "$d/seed/CHANGELOG.md"; fi
  printf '## [0.1.0] - 2026-01-01\n\n- old\n' >> "$d/seed/CHANGELOG.md"
  echo 'FROM scratch' > "$d/seed/api/Dockerfile"
  echo 'package api' > "$d/seed/api/x.go"
  echo 'module x' > "$d/seed/api/go.mod"
  echo 'a: 1' > "$d/seed/deploy/chart/values.yaml"
  git -C "$d/seed" init -q -b main
  git -C "$d/seed" add -A
  git -C "$d/seed" commit -qm init
  git clone -q --bare "$d/seed" "$d/origin.git"
  git clone -q "$d/origin.git" "$d/work"
  git -C "$d/work" checkout -qb feature
  W="$d/work"
}
commit() { git -C "$W" add -A; git -C "$W" commit -qm "$1"; }
# unrel <line>: insert a line directly under `## [Unreleased]`.
unrel() {
  awk -v l="$1" '{print} /^## \[Unreleased\]/ {print ""; print l}' "$W/CHANGELOG.md" > "$W/CHANGELOG.tmp"
  mv "$W/CHANGELOG.tmp" "$W/CHANGELOG.md"
}

# expect <want-rc> <label>
expect() {
  cases=$((cases + 1))
  local rc=0
  (cd "$W" && ./scripts/check-changelog-entry.sh) > "$TMP/out" 2>&1 || rc=$?
  if [ "$rc" = "$1" ]; then
    passed=$((passed + 1))
  else
    echo "FAIL: $2: want exit $1, got $rc" >&2
    sed 's/^/    /' "$TMP/out" >&2
  fi
}

fresh on-main; git -C "$W" checkout -q main
expect 0 "HEAD at origin/main: empty diff"

fresh non-shipping; echo x > "$W/README.md"; commit "chore: readme"
expect 0 "non-shipping change only"

fresh bare-fix; echo '// y' >> "$W/api/x.go"; commit "fix(api): y"
expect 1 "shipping change, no entry"

fresh with-entry; echo '// y' >> "$W/api/x.go"; unrel '- y (#1)'; commit "fix(api): y"
expect 0 "shipping change with an [Unreleased] line"

# A PATH-first shim models BusyBox unified headers. Its added record corresponds to
# the real new line in the fixture; the headers alone must never count as an entry.
mkdir -p "$TMP/diff-shim"
cat > "$TMP/diff-shim/diff" <<'SH'
#!/usr/bin/env bash
printf '%s\n' '--- unreleased.base' '+++ unreleased.head' '@@ -0,0 +1 @@' '+- y (#1)'
exit 1
SH
chmod +x "$TMP/diff-shim/diff"
fresh busybox-unified; echo '// y' >> "$W/api/x.go"; unrel '- y (#1)'; commit "fix(api): y"
PATH="$TMP/diff-shim:$PATH" expect 0 "BusyBox-style unified diff accepts a real entry"

fresh duplicate-entry '- existing'; echo '// y' >> "$W/api/x.go"; unrel '- existing'; commit "fix(api): y"
expect 0 "duplicating an existing [Unreleased] line counts"

fresh reordered-entries $'- first\n- second'; echo '// y' >> "$W/api/x.go"
printf '# Changelog\n\n## [Unreleased]\n\n- second\n- first\n\n## [0.1.0] - 2026-01-01\n\n- old\n' > "$W/CHANGELOG.md"
commit "fix(api): y"
expect 0 "reordering [Unreleased] lines counts as an added diff record"

fresh plus-prefixed; echo '// y' >> "$W/api/x.go"; unrel '+ prefixed entry'; commit "fix(api): y"
expect 0 "added content beginning with + counts"

fresh blank-line; echo '// y' >> "$W/api/x.go"; unrel ''; commit "fix(api): y"
expect 1 "a blank [Unreleased] line is not an entry"

fresh released-section; echo '// y' >> "$W/api/x.go"; printf -- '- y (#1)\n' >> "$W/CHANGELOG.md"; commit "fix(api): y"
expect 1 "a line under a released version is not an entry"

fresh deletion-only; echo '// y' >> "$W/api/x.go"; printf '# Changelog\n' > "$W/CHANGELOG.md"; commit "fix(api): y"
expect 1 "CHANGELOG removal alone is not an entry"

fresh uncommitted-entry; echo '// y' >> "$W/api/x.go"; commit "fix(api): y"; unrel '- y (#1)'
expect 0 "uncommitted CHANGELOG line counts (pre-commit gate)"

fresh untracked-shipping; echo 'package api' > "$W/api/new.go"
expect 1 "untracked shipping file with no entry"

fresh changelog-none; echo '// y' >> "$W/api/x.go"; commit "$(printf 'refactor(api): y\n\nChangelog: none (internal rename)')"
expect 0 "Changelog: none in a branch commit"

fresh docs-only; echo '// comment' >> "$W/api/x.go"; commit "docs(api): comment"
expect 0 "every shipping commit docs-typed"

fresh docs-then-fix; echo '// c' >> "$W/api/x.go"; commit "docs(api): c"; echo '// d' >> "$W/api/x.go"; commit "fix(api): d"
expect 1 "docs commit plus a code commit is not exempt"

fresh docs-plus-uncommitted; echo '// c' >> "$W/api/x.go"; commit "docs(api): c"; echo '// d' >> "$W/api/x.go"
expect 1 "docs commit plus uncommitted shipping change is not exempt"

fresh manifest-only; echo 'require y v1' >> "$W/api/go.mod"; commit "chore(deps): bump y"
expect 0 "dependency manifest only"

fresh dockerfile-deps; echo 'FROM scratch@sha256:abc' > "$W/api/Dockerfile"; commit "chore(deps): update scratch digest"
expect 0 "dependency-typed Dockerfile bump"

fresh dockerfile-fix; echo 'USER 0' >> "$W/api/Dockerfile"; commit "fix(api): run as root"
expect 1 "a fix-titled Dockerfile edit is not a dependency bump"

fresh manifest-uncommitted; echo 'require y v1' >> "$W/api/go.mod"; commit "chore(deps): bump y"; echo 'USER 0' >> "$W/api/Dockerfile"
expect 1 "an uncommitted manifest edit is not covered by the deps subject"

fresh manifest-plus-code; echo 'require y v1' >> "$W/api/go.mod"; echo '// y' >> "$W/api/x.go"; commit "chore(deps): bump y"
expect 1 "manifest plus source is not exempt"

fresh chart; echo 'a: 2' > "$W/deploy/chart/values.yaml"; commit "chore(deps): chart"
expect 1 "deploy/chart change is never exempt"

# A pull_request checkout: HEAD is a merge of the PR head into main, whose subject must
# not break the docs exemption.
fresh synthetic-merge; echo '// c' >> "$W/api/x.go"; commit "docs(api): c"
git -C "$W" checkout -q -B pr-merge origin/main
git -C "$W" merge -q --no-ff -m "Merge abc into def" feature
expect 0 "synthetic merge commit ignored for messages"

fresh merge-message; echo '// y' >> "$W/api/x.go"; commit "fix(api): y"
git -C "$W" checkout -q -B pr-merge origin/main
git -C "$W" merge -q --no-ff -m "$(printf 'Merge abc into def\n\nChangelog: none')" feature
expect 1 "a merge commit's message cannot exempt the branch"

fresh chart-dockerfile; mkdir -p "$W/deploy/chart/files"; echo 'FROM x' > "$W/deploy/chart/files/Dockerfile"; commit "chore(deps): chart image"
expect 1 "a manifest basename under deploy/chart is not exempt"

fresh no-base; git -C "$W" remote remove origin; git -C "$W" update-ref -d refs/remotes/origin/main 2>/dev/null || true
echo '// y' >> "$W/api/x.go"; commit "fix(api): y"
expect 2 "origin/main unresolvable fails closed"

fresh unrelated; git -C "$W" checkout -q --orphan lone; git -C "$W" rm -rq --cached . ; echo '// y' >> "$W/api/x.go"; commit "fix(api): y"
expect 2 "no merge-base fails closed"

fresh refetch; git -C "$W" update-ref -d refs/remotes/origin/main; echo '// y' >> "$W/api/x.go"; commit "fix(api): y"
expect 1 "missing origin/main is fetched, then judged"

echo "check-changelog-entry.test.sh: cases=$cases passed=$passed"
[ "$cases" = "$passed" ] || exit 1
[ "$cases" -ge "$FLOOR" ] || { echo "check-changelog-entry.test.sh: only $cases cases, floor $FLOOR" >&2; exit 1; }
