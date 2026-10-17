#!/usr/bin/env bash
# Hermetic exit-code contract for scripts/check-changelog-entry.sh: throwaway git repos with
# a local bare origin, no network. Each case builds a branch off origin/main and asserts the
# gate's status, so every exemption and the fail-closed paths are proven to fire. Prints a
# `cases=N passed=N` tally and fails below its case floor, so a gutted run cannot read green.
set -euo pipefail

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
mkdir -p "$ROOT/.uzi/scratch"
TMP="$(mktemp -d "$ROOT/.uzi/scratch/changelog-entry-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
FLOOR=80
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
  cp "$ROOT/scripts/lib/changelog-fragments.sh" "$ROOT/scripts/lib/changelog-fragments.awk" "$d/seed/scripts/lib/"
  mkdir -p "$d/seed/changelog.d"
  printf '### Fixed\n\n- **Inherited outcome (#12).**\n  Old description.\n' > "$d/seed/changelog.d/00000000-0000-4000-8000-000000000000.md"
  printf '# Changelog\n\n## [Unreleased]\n\n' > "$d/seed/CHANGELOG.md"
  if [ "$#" -gt 1 ]; then printf '%s\n\n' "$2" >> "$d/seed/CHANGELOG.md"; fi
  printf '## [0.1.0] - 2026-01-01\n\n- old\n' >> "$d/seed/CHANGELOG.md"
  echo 'FROM scratch' > "$d/seed/api/Dockerfile"
  echo 'package api' > "$d/seed/api/x.go"
  echo 'module x' > "$d/seed/api/go.mod"
  echo 'a: 1' > "$d/seed/deploy/chart/values.yaml"
  mkdir -p "$d/seed/agent/src" "$d/seed/agent/codex"
  echo '{"name":"agent"}' > "$d/seed/agent/package.json"
  echo '{"lockfileVersion":3}' > "$d/seed/agent/package-lock.json"
  echo 'export {};' > "$d/seed/agent/src/x.ts"
  echo 'CODEX_VERSION=0.1.0' > "$d/seed/agent/codex/codex-package.lock"
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

# Worker runtime assets need entries; excluded agent tests still do not.
for runtime in templates codex; do
  fresh "agent-$runtime-missing"
  mkdir -p "$W/agent/$runtime"
  echo 'runtime asset' > "$W/agent/$runtime/runtime.txt"
  commit "fix(agent): update $runtime"
  expect 1 "agent/$runtime change without an entry"
  unrel "- Update $runtime (#1466)"
  commit "docs: record $runtime change"
  expect 0 "agent/$runtime change with an added entry"
done
fresh agent-test-only
mkdir -p "$W/agent/test" "$W/agent/src"
echo 'test fixture' > "$W/agent/test/runtime.ts"
echo 'test fixture' > "$W/agent/src/foo.test.ts"
commit "test(agent): runtime coverage"
expect 0 "excluded agent tests need no entry"

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

fresh npm-lock; echo '{"lockfileVersion":3,"x":1}' > "$W/agent/package-lock.json"; commit "chore(deps): lock file maintenance"
expect 0 "agent npm lockfile only, dependency-typed"

fresh npm-manifest; echo '{"name":"agent","dependencies":{"y":"1"}}' > "$W/agent/package.json"; echo '{"lockfileVersion":3,"y":1}' > "$W/agent/package-lock.json"; commit "fix(deps): update dependency y to v1"
expect 0 "agent npm manifest and lockfile, dependency-typed"

fresh npm-fix-subject; echo '{"name":"agent","scripts":{"x":"y"}}' > "$W/agent/package.json"; commit "fix(agent): add a script"
expect 1 "an agent package.json edit under a non-deps subject is not exempt"

fresh npm-plus-source; echo '{"lockfileVersion":3,"y":1}' > "$W/agent/package-lock.json"; echo '// y' >> "$W/agent/src/x.ts"; commit "chore(deps): bump y"
expect 1 "npm lockfile plus agent source is not exempt"

fresh chart-npm; mkdir -p "$W/deploy/chart/files"; echo '{}' > "$W/deploy/chart/files/package.json"; commit "chore(deps): chart package"
expect 1 "a package.json under deploy/chart is not exempt"

fresh codex-lock; echo 'CODEX_VERSION=0.1.1' > "$W/agent/codex/codex-package.lock"; commit "chore(deps): update codex runtime"
expect 1 "the Codex runtime lock is never a dependency-exempt manifest"

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

# Fragment coverage and validation precede every entry/exemption/nonshipping success.
fragment=changelog.d/11111111-1111-4111-8111-111111111111.md
inherited=changelog.d/00000000-0000-4000-8000-000000000000.md
write_fragment() { printf '### Fixed\n\n- **New outcome (#7).**\n  Description.\n' > "$W/$fragment"; }

for state in untracked staged committed; do
  fresh "fragment-$state"
  echo '// y' >> "$W/api/x.go"
  write_fragment
  case "$state" in
    staged) git -C "$W" add -- "$fragment" ;;
    committed) commit "fix(api): y" ;;
  esac
  expect 0 "$state new fragment satisfies shipping coverage"
done
fresh fragment-workingtree; write_fragment; commit "fix: fragment"
printf '### Added\n\n- **Updated outcome.**\n  Description.\n' > "$W/$fragment"
echo '// y' >> "$W/api/x.go"
expect 0 "workingtree edit to branch-new fragment counts"

fresh inherited-modified; echo '// y' >> "$W/api/x.go"
printf '### Fixed\n\n- **Changed inherited outcome.**\n  Description.\n' > "$W/$inherited"
expect 1 "inherited fragment edit does not count"
fresh inherited-deleted; echo '// y' >> "$W/api/x.go"; rm "$W/$inherited"
expect 1 "inherited deletion does not count"
fresh readme-only; echo '// y' >> "$W/api/x.go"; echo prose > "$W/changelog.d/README.md"
expect 1 "README is not entry evidence"
fresh readme-nonshipping; echo prose > "$W/changelog.d/README.md"
expect 0 "README content is not parsed"

for exemption in none docs deps legacy nonshipping valid; do
  fresh "malformed-$exemption"
  case "$exemption" in
    none) echo '// y' >> "$W/api/x.go"; commit $'fix: y\n\nChangelog: none' ;;
    docs) echo '// y' >> "$W/api/x.go"; commit "docs: y" ;;
    deps) echo 'require y v1' >> "$W/api/go.mod"; commit "chore(deps): y" ;;
    legacy) echo '// y' >> "$W/api/x.go"; unrel '- legacy' ;;
    valid) echo '// y' >> "$W/api/x.go"; write_fragment ;;
  esac
  echo malformed > "$W/changelog.d/22222222-2222-4222-8222-222222222222.md"
  expect 1 "malformed fragment rejects $exemption success"
done
for state in staged committed workingtree; do
  fresh "malformed-state-$state"
  write_fragment
  if [ "$state" = workingtree ]; then commit "docs: fragment"; fi
  echo malformed > "$W/$fragment"
  case "$state" in
    staged) git -C "$W" add -- "$fragment" ;;
    committed) commit "docs: malformed fragment" ;;
  esac
  expect 1 "$state malformed fragment without shipping change"
done
for name in bad.md 11111111-1111-5111-8111-111111111111.md 11111111-1111-4111-7111-111111111111.md A1111111-1111-4111-8111-111111111111.md $'bad\nname.md'; do
  fresh "invalid-name-$cases"
  echo '// y' >> "$W/api/x.go"
  write_fragment
  echo malformed > "$W/changelog.d/$name"
  expect 1 "invalid filename cannot escape validation"
done
fresh nul-fragment-entry
echo '// y' >> "$W/api/x.go"
printf '### Fixed\0\0- **Outcome.**\0  Description.\0' > "$W/$fragment"
expect 1 "NUL fragment cannot supply shipping entry"

fresh split-bold-title-entry
echo '// y' >> "$W/api/x.go"
printf '### Fixed\n\n- **Outcome.** unbold prose **Other.**\n  Description.\n' > "$W/$fragment"
expect 1 "split bold title cannot supply shipping entry"

fresh ignored-invalid-name
printf 'changelog.d/bad.md\n' > "$W/.gitignore"
echo malformed > "$W/changelog.d/bad.md"
expect 1 "ignored invalid filename rejects nonshipping success"

fresh ignored-readme
printf 'changelog.d/README.md\n' > "$W/.gitignore"
echo arbitrary > "$W/changelog.d/README.md"
expect 0 "ignored reserved README remains allowed"

fresh ignored-canonical-name
printf '%s\n' "$fragment" > "$W/.gitignore"
echo malformed > "$W/$fragment"
expect 0 "ignored canonical filename does not expand content validation"
echo '// y' >> "$W/api/x.go"
expect 1 "ignored canonical fragment supplies no shipping evidence"

for unsafe in symlink dangling fifo directory nested readme-link parent-link parent-file; do
  fresh "unsafe-$unsafe"
  case "$unsafe" in
    symlink) ln -s "$inherited" "$W/$fragment" ;;
    dangling) ln -s nowhere "$W/$fragment" ;;
    fifo) mkfifo "$W/$fragment" ;;
    directory) mkdir "$W/$fragment" ;;
    nested) mkdir "$W/changelog.d/nested"; echo x > "$W/changelog.d/nested/x.md" ;;
    readme-link) ln -s nowhere "$W/changelog.d/README.md" ;;
    parent-link) mv "$W/changelog.d" "$W/elsewhere"; ln -s elsewhere "$W/changelog.d" ;;
    parent-file) rm -r "$W/changelog.d"; echo x > "$W/changelog.d" ;;
  esac
  expect 1 "$unsafe rejected before nonshipping success"
done
fresh inherited-malformed; echo malformed > "$W/$inherited"
expect 1 "malformed edit to inherited fragment fails without shipping"
fresh staged-inherited-malformed; echo malformed > "$W/$inherited"; git -C "$W" add -- "$inherited"
expect 1 "staged malformed inherited edit fails without shipping"
fresh deleted-new-fragment; echo '// y' >> "$W/api/x.go"; write_fragment; commit "fix: new fragment"; rm "$W/$fragment"
expect 1 "branch-new fragment deleted in workingtree cannot supply entry"
fresh shipping-newline-path; mkdir -p "$W/api"; echo x > "$W/api/"$'new\nfile.go'
expect 1 "shipping filename with newline is not lost"

# Legacy limits still apply during migration when the fragment directory is absent.
for budget in pending full; do
  fresh "missing-directory-$budget-budget"
  rm -rf "$W/changelog.d"
  python3 - "$W/CHANGELOG.md" "$budget" <<'PYFIX'
from pathlib import Path
import sys
if sys.argv[2] == 'pending':
    content = b'## [Unreleased]\n### Fixed\n- ' + b'x' * 4194304 + b'\n'
else:
    content = b'## [Unreleased]\n\n## [0.1.0]\n' + b'x' * 16777216 + b'\n'
Path(sys.argv[1]).write_bytes(content)
PYFIX
  expect 1 "missing fragment directory cannot bypass $budget legacy budget"
done

# Limits precede all exemptions and cover actual bounded fragment reads.
for exemption in none docs deps; do
  fresh "safety-limit-$exemption"
  case "$exemption" in
    none) echo '// change' >> "$W/api/x.go"; commit $'fix: change\n\nChangelog: none' ;;
    docs) echo '// docs' >> "$W/api/x.go"; commit 'docs: change' ;;
    deps) echo 'require y v1' >> "$W/api/go.mod"; commit 'chore(deps): change' ;;
  esac
  python3 - "$W/$fragment" <<'PYFIX'
from pathlib import Path
import sys
Path(sys.argv[1]).write_bytes(b'### Fixed\n\n- **Outcome.**\n  ' + b'x' * 65536 + b'\n')
PYFIX
  expect 1 "$exemption cannot exempt oversized fragment"
done
fresh bidi-fragment
printf '### Fixed\n\n- **Outcome.**\n  Unsafe \342\200\256text\342\200\254.\n' > "$W/$fragment"
expect 1 'PR check rejects bidi in changed fragment'
fresh count-limit
python3 - "$W/changelog.d" <<'PYFIX'
from pathlib import Path
import sys
for i in range(1025):
    (Path(sys.argv[1]) / f'{i:08x}-1111-4111-8111-111111111111.md').write_text('### Fixed\n\n- **Outcome.**\n  Description.\n')
PYFIX
expect 1 'PR check rejects inventory count before nonshipping exemption'
fresh aggregate-limit
python3 - "$W/changelog.d" <<'PYFIX'
from pathlib import Path
import sys
prefix = b'### Fixed\n\n- **Outcome.**\n  '
for i in range(65):
    (Path(sys.argv[1]) / f'{i:08x}-1111-4111-8111-111111111111.md').write_bytes(prefix + b'x' * (65536-len(prefix)-1) + b'\n')
PYFIX
expect 1 'PR check rejects fragment aggregate before nonshipping exemption'
fresh combined-limit
write_fragment
python3 - "$W/CHANGELOG.md" <<'PYFIX'
from pathlib import Path
import sys
Path(sys.argv[1]).write_bytes(b'## [Unreleased]\n### Fixed\n- ' + b'x' * 4194280 + b'\n')
PYFIX
expect 1 'PR check rejects combined body and fragments'

echo "check-changelog-entry.test.sh: cases=$cases passed=$passed"
[ "$cases" = "$passed" ] || exit 1
[ "$cases" -ge "$FLOOR" ] || { echo "check-changelog-entry.test.sh: only $cases cases, floor $FLOOR" >&2; exit 1; }
