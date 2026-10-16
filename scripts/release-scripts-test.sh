#!/usr/bin/env bash
# release-scripts-test.sh — offline fixture test for the release-train scripts.
#
# Builds a throwaway git repo with a pinned tag topology that exercises the
# RC-first release train (PRD 1265), then drives the real scripts against it and
# asserts their behaviour. It creates NO network traffic and touches no real tag.
#
# Topology built (see build_fixture):
#
#   main:  v0.1.0 -- feat#201 -- feat#202 -- [v0.2.0-rc.1]=X' -- feat#301 -- feat#302
#                                                                    \-- [v0.3.0-rc.1..rc.10]
#                                                                         -- featUncited -- [v0.3.0-rc.11]
#   release/0.2.0  (off X'):        promote -- [v0.2.0]
#   release/0.2.1  (off v0.2.0):    hotfix#210 -- [v0.2.1]
#
# Asserted (M1):
#   * section lookup strips the -rc.N suffix: a `0.2.0-rc.1` cut reads `## [0.2.0]`
#   * PREV is the highest STABLE tag strictly below the base version (D4), never
#     `git describe` ancestry and never a prerelease tag
#   * the hotfix tag v0.2.1 does not change the v0.3.0-rc.* coverage window
#     (same merge-base with main as v0.2.0)
#   * v0.3.0-rc.10 resolves to base 0.3.0 like rc.1
#   * an uncited shipping merge after the RC FAILs the next RC's oracle
#   * changelog-section.sh prints the FULL tag (v0.2.0-rc.1) in the title while
#     reading the `[0.2.0]` body
#
# M2a/M2b extend this file to drive release-cut.sh through its verbs.
# --shipping-paths-only runs the #1466 regressions without the full release suite.
# --identity-only [oracle-path] runs identity fixtures, optionally against a frozen oracle.
# --ghcr-only [baseline-verifier] runs #1464 M1 publication fixtures only.
# --frozen-rc-only runs the #1389 block-guard regressions without the full fixture suite.
# --fold-order-only runs canonical pending/open-RC ordering fixtures.
# --fragments-only runs pending fragment consumption and preflight regressions.
#
# Wired as `task test:release-scripts` (its own gate member, not gate:repo's
# sub-second band: it builds a multi-commit repo). Watch it fail on the
# unmodified scripts before M1 lands.
set -uo pipefail

SCRIPTS_DIR="$(cd "$(dirname "$0")" && pwd)"
TMPDIR="$(cd "$SCRIPTS_DIR/.." && pwd)/.uzi/scratch"
mkdir -p "$TMPDIR"
export TMPDIR
ORACLE="$SCRIPTS_DIR/assert-changelog-covers-release.sh"
[ "${1:-}" != "--identity-only" ] || ORACLE="${2:-$ORACLE}"
# Supply a frozen verifier explicitly for exact parity; otherwise assert current behavior.
GHCR_BASELINE="${GHCR_BASELINE_VERIFY:-}"
[ "${1:-}" != "--ghcr-only" ] || GHCR_BASELINE="${GHCR_BASELINE_VERIFY:-${2:-}}"
SECTION="$SCRIPTS_DIR/changelog-section.sh"
bash "$SCRIPTS_DIR/changelog-rc-bullets.test.sh" "$SECTION" || exit 1
[ "${1:-}" != "--frozen-rc-only" ] || exit 0

for s in "$ORACLE" "$SECTION"; do
  [ -x "$s" ] || { echo "release-scripts-test: $s not found/executable" >&2; exit 2; }
done

FAILS=0
PASSES=0
pass() { PASSES=$((PASSES + 1)); printf '  ok   %s\n' "$1"; }
fail() { FAILS=$((FAILS + 1)); printf '  FAIL %s\n' "$1"; }

# assert_eq <label> <expected> <actual>
assert_eq() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (expected '$2', got '$3')"; fi
}
# assert_contains <label> <needle> <haystack>
assert_contains() {
  case "$3" in
    *"$2"*) pass "$1" ;;
    *) fail "$1 (expected to contain '$2', got '$3')" ;;
  esac
}

# --- fixture repo -------------------------------------------------------------
REPO="$(mktemp -d)"
# A stub `gh` first on PATH keeps the oracle's closing-issues lookup offline: it fails
# unless GH_STUB_OUT is set, then prints it verbatim (the merge-commit OID line, then
# one owner/repo#N per closing issue) and logs the arguments it was called with.
GH_STUB="$(mktemp -d)"
trap 'rm -rf "$REPO" "$GH_STUB"' EXIT
cat > "$GH_STUB/gh" <<'STUB'
#!/usr/bin/env bash
# New release queries are isolated from the #1327 closing-issue response.
if [ "${GHCR_STUB:-}" = 1 ]; then
  [ -z "${GHCR_QUERY_LOG:-}" ] || printf '%s\n' "$*" >> "$GHCR_QUERY_LOG"
  endpoint=""
  for arg in "$@"; do
    case "$arg" in */packages/container/*/versions) endpoint="$arg" ;; esac
  done
  if [ -n "$endpoint" ]; then
    case "$endpoint" in
      *uzi%2Fagent-base/versions) out="${GHCR_BASE_OUT-}"; rc="${GHCR_BASE_RC:-1}" ;;
      *uzi%2Fagent-jvm/versions) out="${GHCR_JVM_OUT-}"; rc="${GHCR_JVM_RC:-1}" ;;
      *) out="${GHCR_OTHER_OUT-}"; rc="${GHCR_OTHER_RC:-1}" ;;
    esac
    printf '%s\n' "$out"; exit "$rc"
  fi
  case "$*" in
    'repo view --json owner,name '*)
      printf '%s\n' "${GHCR_IDENTITY-test uzi}"; exit "${GHCR_IDENTITY_RC:-1}" ;;
    'api users/test --jq .type')
      printf '%s\n' "${GHCR_KIND-User}"; exit "${GHCR_KIND_RC:-1}" ;;
    'run list '*) printf '123\n'; exit 0 ;;
    'run view 123 --log') printf 'Pushing signature to: fixture\n'; exit 0 ;;
    *'/releases/latest '*) printf 'v0.2.0\n'; exit 0 ;;
    *'/actions/runs/123/jobs'*)
      case "$*" in *'| length'*) printf '1\n' ;; *) printf 'success\n' ;; esac
      exit 0 ;;
    *) exit 1 ;; # Unconfigured release queries fail closed.
  esac
fi
[ -n "${GH_STUB_OUT:-}" ] || exit 1
[ -z "${GH_STUB_LOG:-}" ] || printf '%s\n' "$*" >> "$GH_STUB_LOG"
printf '%s\n' "$GH_STUB_OUT"
STUB
chmod +x "$GH_STUB/gh"
export PATH="$GH_STUB:$PATH"
unset GH_STUB_OUT GH_STUB_LOG GHCR_STUB GHCR_QUERY_LOG
export GH_REPO=test/uzi

DATE_N=0
tick() { DATE_N=$((DATE_N + 1)); printf '2026-09-%02dT00:00:00' "$DATE_N"; }

git_c() { git -C "$REPO" "$@"; }

commit() { # commit <msg> ; files already staged
  local d; d="$(tick)"
  GIT_AUTHOR_DATE="$d" GIT_COMMITTER_DATE="$d" \
    git_c commit -q -m "$1"
}

put() { # put <relpath> <<<content ; writes and stages
  local rel="$1"; shift
  mkdir -p "$REPO/$(dirname "$rel")"
  cat > "$REPO/$rel"
  git_c add -- "$rel"
}

chart() { # chart <version> -> stage Chart.yaml at that version
  put deploy/chart/Chart.yaml <<EOF
apiVersion: v2
name: uzi
version: ${1%%-*}
appVersion: "$1"
EOF
}

build_fixture() {
  git_c init -q
  git_c config user.email t@example.com
  git_c config user.name test
  git_c symbolic-ref HEAD refs/heads/main

  # 1. v0.1.0
  put api/base.go <<<'package main'
  chart 0.1.0
  put CHANGELOG.md <<'EOF'
# Changelog

## [Unreleased]

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
EOF
  git_c add -A
  commit "chore(release): v0.1.0"
  git_c tag v0.1.0

  # 2/3. two feature merges (no CHANGELOG touch — the oracle must demand a citation)
  put api/feature_a.go <<<'package main // a'
  commit "Feature A (#201)"
  put api/feature_b.go <<<'package main // b'
  commit "Feature B (#202)"

  # 4. v0.2.0-rc.1 — writes the [0.2.0] section citing #201/#202, bumps the chart
  put CHANGELOG.md <<'EOF'
# Changelog

## [Unreleased]

## [0.2.0] - 2026-09-04
### Added
- **Feature A** (#201)
- **Feature B** (#202)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
EOF
  chart 0.2.0-rc.1
  commit "chore(release): v0.2.0-rc.1"
  git_c tag v0.2.0-rc.1

  # release/0.2.0 off the RC: metadata-only promote (touches CHANGELOG + Chart)
  git_c checkout -q -b release/0.2.0 v0.2.0-rc.1
  put CHANGELOG.md <<'EOF'
# Changelog

## [Unreleased]

## [0.2.0] - 2026-09-05
### Added
- **Feature A** (#201)
- **Feature B** (#202)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
EOF
  chart 0.2.0
  commit "chore(release): v0.2.0 (promotes v0.2.0-rc.1)"
  git_c tag v0.2.0

  # release/0.2.1 off v0.2.0: hotfix
  git_c checkout -q -b release/0.2.1 v0.2.0
  put api/hotfix.go <<<'package main // fix'
  put CHANGELOG.md <<'EOF'
# Changelog

## [Unreleased]

## [0.2.1] - 2026-09-06
### Fixed
- **Hotfix** (#210)

## [0.2.0] - 2026-09-05
### Added
- **Feature A** (#201)
- **Feature B** (#202)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
EOF
  chart 0.2.1
  commit "chore(release): v0.2.1"
  git_c tag v0.2.1

  # back to main: two more feature merges, then v0.3.0-rc.1..rc.10 on one commit
  git_c checkout -q main
  put api/feature_c.go <<<'package main // c'
  commit "Feature C (#301)"
  put api/feature_d.go <<<'package main // d'
  commit "Feature D (#302)"
  put CHANGELOG.md <<'EOF'
# Changelog

## [Unreleased]

## [0.3.0] - 2026-09-10
### Added
- **Feature C** (#301)
- **Feature D** (#302)

## [0.2.0] - 2026-09-05
### Added
- **Feature A** (#201)
- **Feature B** (#202)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
EOF
  chart 0.3.0-rc.1
  commit "chore(release): v0.3.0-rc.1"
  local n
  for n in 1 2 3 4 5 6 7 8 9 10; do git_c tag "v0.3.0-rc.$n"; done

  # an uncited shipping merge, then v0.3.0-rc.11 that does NOT cite it
  put api/feature_uncited.go <<<'package main // uncited'
  commit "Uncited change"
  chart 0.3.0-rc.11
  commit "chore(release): v0.3.0-rc.11"
  git_c tag v0.3.0-rc.11

  # RC-body DELTA scenario (PRD 1265): 0.4.0-rc.1 opens the [0.4.0] section with X;
  # rc.2 appends Y (accumulation in the FILE); rc.3 re-spins the SAME commit as rc.2
  # (no new bullets). Drives the changelog-section.sh body delta assertions below.
  put api/feature_x.go <<<'package main // x'
  commit "Feature X (#401)"
  put CHANGELOG.md <<'EOF'
# Changelog

## [Unreleased]

## [0.4.0] - 2026-09-20
### Added
- **Feature X** (#401)
  Adds the X subsystem.
- **Feature Z** (#403)
  A stable helper.

## [0.2.0] - 2026-09-05
### Added
- **Feature A** (#201)
- **Feature B** (#202)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
EOF
  chart 0.4.0-rc.1
  commit "chore(release): v0.4.0-rc.1"
  git_c tag v0.4.0-rc.1

  put api/feature_y.go <<<'package main // y'
  commit "Feature Y (#402)"
  put CHANGELOG.md <<'EOF'
# Changelog

## [Unreleased]

## [0.4.0] - 2026-09-20
### Added
- **Feature X** (#401)
  Adds the X subsystem, now with caching.
- **Feature Z** (#403)
  A stable helper.
- **Feature Y** (#402)
  Adds the Y subsystem.

## [0.2.0] - 2026-09-05
### Added
- **Feature A** (#201)
- **Feature B** (#202)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
EOF
  chart 0.4.0-rc.2
  commit "chore(release): v0.4.0-rc.2"
  git_c tag v0.4.0-rc.2
  git_c tag v0.4.0-rc.3   # re-spin: same commit as rc.2, no new changelog bullets
}

# oracle_prev <ref> <version> — echo the PREV the oracle derives (VERSION passed,
# PREV left empty so the script derives it). Parsed from its "coverage for A..B" line.
oracle_prev() {
  ( cd "$REPO" && bash "$ORACLE" "$1" "" "$2" 2>&1 ) | sed -n 's/^Checking CHANGELOG coverage for \(.*\)\.\..*/\1/p' | head -1
}
# oracle_run <ref> <version> — run with derived PREV; echoes rc into $? via caller
oracle_run() { ( cd "$REPO" && bash "$ORACLE" "$1" "" "$2" >/dev/null 2>&1 ); }

# Identity extraction uses the real oracle, git history and the offline gh stub.
identity_fixture() {
  git_c init -q
  git_c config user.email t@example.com
  git_c config user.name test
  git_c symbolic-ref HEAD refs/heads/main
  put api/base.go <<<'package main'
  chart 0.1.0
  put CHANGELOG.md <<<'## [0.1.0]
- Initial'
  commit "chore(release): v0.1.0"
  git_c tag v0.1.0
}

identity_case() { # label subject body expected-rc expected-refs [escape]
  local label="$1" subject="$2" body="$3" expected="$4" refs="$5" escape="${6:-}"
  local short citation out rc diagnostic
  git_c reset -q --hard v0.1.0
  put api/change.go <<<'package main // change'
  if [ "$escape" = touched ]; then put CHANGELOG.md <<<'## [0.1.0]
- Changed'; fi
  commit "$subject

$body"
  short="$(git_c rev-parse --short HEAD)"
  citation="#1317"
  [ "$escape" != sha ] || citation="$short"
  put CHANGELOG.md <<<"## [0.2.0]
- Released ($citation)

## [0.1.0]
- Initial"
  chart 0.2.0
  commit "chore(release): v0.2.0"
  out="$(cd "$REPO" && bash "$ORACLE" HEAD v0.1.0 0.2.0 2>&1)"; rc=$?
  assert_eq "$label exit" "$expected" "$rc"
  if [ "$expected" = 1 ]; then
    if [ -z "$refs" ]; then
      diagnostic="        changed api/change.go but cites no issue number; cite #<issue> or the short SHA $short in the section"
    else
      diagnostic="        changed api/change.go; neither $refs nor the short SHA $short appears in the section"
    fi
    assert_eq "$label diagnostic" "$diagnostic" "$(printf '%s\n' "$out" | sed -n '/^        changed /p')"
    assert_contains "$label failure header" "FAIL: merges not accounted for in the [0.2.0] section:" "$out"
    assert_contains "$label commit identity" "  $short  $subject" "$out"
  fi
}

identity_regressions() {
  local REPO
  REPO="$(mktemp -d)"
  identity_fixture
  identity_case "unrelated issue prose" "Ship (#902)" "Unrelated issue #1317" 1 "#902"
  identity_case "unrelated issue prose without subject refs" "Ship" "Unrelated issue #1317" 1 ""
  identity_case "see prose without subject refs" "Ship" "See #1317" 1 ""
  local subject body keyword
  for body in "other/repo#1317" "Closes other/repo#1317" "Fixes test/uzi#1317" "agent/issue-1317" \
    "See api/1317/change.go" "See 2026/1317/11" "See https://example.com/issues/1317" \
    "* Prior change (#1317)" "encloses #1317" "prefixes #1317" "unresolved #1317" \
    "Fixes#1317" "ClosesSuffix #1317" "xFixes #1317" "Fixes #1317suffix" "Closes #1317x" "Resolved #1317suffix"; do
    identity_case "body rejected: $body" "Ship" "$body" 1 ""
  done
  for subject in "Ship api/1317/change.go" "Ship 2026/1317/11" \
    "Ship https://example.com/issues/1317" "Ship issue-1317suffix" \
    "Ship PRD 1317suffix" "Ship #1317suffix" "Ship tissue-1317" "Ship xPRD 1317"; do
    identity_case "subject rejected: $subject" "$subject" "" 1 ""
  done
  identity_case "zero refs" "Ship" "" 1 ""
  for subject in "Ship (#1317)" "Ship #1317" "Ship issue 1317" "Ship ISSUE-1317" \
    "Ship PRD 1317" "Ship pRd #1317" "Merge agent/issue-1317" \
    "Ship https://example.com/issue-1317" "Ship api/PRD 1317/file" \
    "Ship https://example.com/page#1317" "Ship api/file#1317" \
    "Ship #902,#1317" "Ship #902#1317" "Ship issue-902;issue-1317" "Ship PRD 902,PRD #1317"; do
    identity_case "subject accepted: $subject" "$subject" "" 0 ""
  done
  for keyword in close cLoSeS CLOSED fix FiXeS FIXED resolve ReSoLvEs RESOLVED; do
    identity_case "body keyword: $keyword" "Ship" "$keyword #1317" 0 ""
  done
  identity_case "body newline whitespace" "Ship" $'Fixes\n#1317' 0 ""
  identity_case "adjacent body references" "Ship" "Fixes #902;Closes #1317" 0 ""
  identity_case "space adjacent body references" "Ship" "Fixes #902 fixes #1317" 0 ""
  identity_case "squash subcommit subjects" "Ship (#902)" $'* First change (#1317)\n* Second change (#1318)' 1 "#902"
  identity_case "union and dedup diagnostic" "Ship #902,issue-903,#902" "Fixes #904;Closes #903" 1 "#902, #903, #904"
  identity_case "SHA without refs" "Ship" "" 0 "" sha
  identity_case "SHA with wrong refs" "Ship (#902)" "Fixes #903" 0 "" sha
  identity_case "Changelog none body" "Ship" "Changelog: none" 0 ""
  identity_case "Changelog none subject" "Changelog: none" "" 0 ""
  identity_case "CHANGELOG touched" "Ship" "" 0 "" touched
  rm -rf "$REPO"
}

if [ "${1:-}" = "--identity-only" ]; then
  identity_regressions
  echo "=== release-scripts-test: $PASSES passed, $FAILS failed ==="
  [ "$FAILS" -eq 0 ]
  exit $?
fi

if [ "${1:-}" != "--shipping-paths-only" ] && [ "${1:-}" != "--ghcr-only" ] && [ "${1:-}" != "--fold-order-only" ] && [ "${1:-}" != "--fragments-only" ]; then
identity_regressions
echo "=== building fixture in $REPO ==="
build_fixture

echo "=== M1: section lookup + PREV (D4) ==="

# section lookup strips -rc.N, and PREV is the highest stable tag below the base
assert_eq "PREV(v0.2.0-rc.1) = v0.1.0"      "v0.1.0" "$(oracle_prev v0.2.0-rc.1 0.2.0-rc.1)"
assert_eq "PREV(v0.2.0 stable) = v0.1.0"    "v0.1.0" "$(oracle_prev v0.2.0 0.2.0)"
assert_eq "PREV(v0.2.1 hotfix) = v0.2.0"    "v0.2.0" "$(oracle_prev v0.2.1 0.2.1)"
assert_eq "PREV(v0.3.0-rc.1) = v0.2.1"      "v0.2.1" "$(oracle_prev v0.3.0-rc.1 0.3.0-rc.1)"
assert_eq "PREV(v0.3.0-rc.10) = v0.2.1"     "v0.2.1" "$(oracle_prev v0.3.0-rc.10 0.3.0-rc.10)"

# the RC oracle PASSES (section [0.2.0] found by stripping -rc.1, merges cited)
if oracle_run v0.2.0-rc.1 0.2.0-rc.1; then pass "oracle PASS v0.2.0-rc.1"; else fail "oracle PASS v0.2.0-rc.1"; fi
if oracle_run v0.2.0 0.2.0;         then pass "oracle PASS v0.2.0";      else fail "oracle PASS v0.2.0"; fi
if oracle_run v0.2.1 0.2.1;         then pass "oracle PASS v0.2.1";      else fail "oracle PASS v0.2.1"; fi
if oracle_run v0.3.0-rc.1 0.3.0-rc.1;   then pass "oracle PASS v0.3.0-rc.1";  else fail "oracle PASS v0.3.0-rc.1"; fi
if oracle_run v0.3.0-rc.10 0.3.0-rc.10; then pass "oracle PASS v0.3.0-rc.10"; else fail "oracle PASS v0.3.0-rc.10"; fi

# the hotfix tag does not change the v0.3.0-rc.1 window (same merge-base as v0.2.0)
w_from_021="$(git_c log --first-parent --format=%H v0.2.1..v0.3.0-rc.1)"
w_from_020="$(git_c log --first-parent --format=%H v0.2.0..v0.3.0-rc.1)"
assert_eq "v0.2.1 does not change the v0.3.0-rc.1 window" "$w_from_020" "$w_from_021"

# an uncited shipping merge after the RC FAILs the next RC's oracle
if oracle_run v0.3.0-rc.11 0.3.0-rc.11; then fail "oracle FAIL v0.3.0-rc.11 (uncited merge)"; else pass "oracle FAIL v0.3.0-rc.11 (uncited merge)"; fi

echo "=== oracle: a section citing the issue a squash-merged PR closes ==="
# A squash merge whose subject names only its PR (#902) and whose body never names the
# issue (#901) it closes, released with a section citing #901 alone: the v0.84.0-rc.13
# shape. Accepted only via the PR's closing issues; offline it still FAILs.
SQ="$(mktemp -d)"
git -C "$SQ" init -q; git -C "$SQ" config user.email t@example.com; git -C "$SQ" config user.name test
git -C "$SQ" symbolic-ref HEAD refs/heads/main
mkdir -p "$SQ/api" "$SQ/deploy/chart"
printf 'package main\n' > "$SQ/api/base.go"
printf 'apiVersion: v2\nname: uzi\nversion: 0.9.0\nappVersion: "0.9.0"\n' > "$SQ/deploy/chart/Chart.yaml"
printf '# Changelog\n\n## [0.9.0] - 2026-09-01\n### Added\n- **Initial** (#900)\n' > "$SQ/CHANGELOG.md"
git -C "$SQ" add -A; git -C "$SQ" commit -q -m "chore(release): v0.9.0"; git -C "$SQ" tag v0.9.0
printf 'package main // fix\n' > "$SQ/api/fix.go"
git -C "$SQ" add -A; git -C "$SQ" commit -q -m "Codex idle watchdog fails a run (#899) (#902)" -m "* fix(agent): suspend idle while a command runs"
SQ_SHA="$(git -C "$SQ" rev-parse HEAD)"
printf 'apiVersion: v2\nname: uzi\nversion: 0.10.0\nappVersion: "0.10.0-rc.1"\n' > "$SQ/deploy/chart/Chart.yaml"
printf '# Changelog\n\n## [0.10.0] - 2026-09-02\n### Fixed\n- **Idle watchdog** ([#901](https://example.com/issues/901))\n\n## [0.9.0] - 2026-09-01\n### Added\n- **Initial** (#900)\n' > "$SQ/CHANGELOG.md"
git -C "$SQ" add -A; git -C "$SQ" commit -q -m "chore(release): v0.10.0-rc.1"
sq_run() { ( cd "$SQ" && bash "$ORACLE" HEAD "" 0.10.0-rc.1 >/dev/null 2>&1 ); }
if sq_run; then fail "oracle FAIL offline (gh unavailable): issue-only citation"; else pass "oracle FAIL offline (gh unavailable): issue-only citation"; fi
GH_STUB_LOG="$SQ.ghlog"; export GH_STUB_LOG
if GH_STUB_OUT="$SQ_SHA
test/uzi#901" sq_run; then pass "oracle PASS: section cites the issue PR #902 closes"; else fail "oracle PASS: section cites the issue PR #902 closes"; fi
assert_contains "a two-group subject looks up its LAST (#N), the PR" "pr view 902" "$(cat "$GH_STUB_LOG" 2>/dev/null)"
if GH_STUB_OUT="$SQ_SHA
test/uzi#903" sq_run; then fail "oracle FAIL: PR #902 closes an issue the section does not cite"; else pass "oracle FAIL: PR #902 closes an issue the section does not cite"; fi
if GH_STUB_OUT="0000000000000000000000000000000000000000
test/uzi#901" sq_run; then fail "oracle FAIL: the PR's merge commit is not this commit"; else pass "oracle FAIL: the PR's merge commit is not this commit"; fi
if GH_STUB_OUT="$SQ_SHA
other/repo#901" sq_run; then fail "oracle FAIL: the closing issue lives in another repo"; else pass "oracle FAIL: the closing issue lives in another repo"; fi
if GH_STUB_OUT="$SQ_SHA
TEST/UZI#901" sq_run; then pass "oracle PASS: repo match ignores case"; else fail "oracle PASS: repo match ignores case"; fi
unset GH_STUB_LOG
rm -rf "$SQ" "$SQ.ghlog"

echo "=== M1: changelog-section.sh title/body ==="
title="$( cd "$REPO" && bash "$SECTION" title 0.2.0-rc.1 2>&1 )"
assert_eq "section title is the full tag" "v0.2.0-rc.1" "$title"
body="$( cd "$REPO" && bash "$SECTION" body 0.2.0-rc.1 2>&1 )"
assert_contains "section body reads [0.2.0]" "Feature A" "$body"

echo "=== M1: changelog-section.sh body RC delta (PRD 1265, stable-only accumulation) ==="
# Two-physical-line bullets (title + indented description), matching the real
# CHANGELOG shape. rc.1 = full section; rc.2 = only its delta since rc.1: the NEW
# bullet (Y) and the AMENDED bullet (X, whose description gained a clause), while
# the UNCHANGED bullet (Z) is dropped. stable = the full accumulated section; a
# re-spin with no new bullets says so. Both directions, so an always-full-section
# regression, a dropped continuation line, or a missed amend all fail.
b_rc1="$( cd "$REPO" && bash "$SECTION" body 0.4.0-rc.1 2>&1 )"
assert_contains "rc.1 body is the full section (X present)" "Feature X" "$b_rc1"
assert_contains "rc.1 body is the full section (Z present)" "Feature Z" "$b_rc1"
b_rc2="$( cd "$REPO" && bash "$SECTION" body 0.4.0-rc.2 2>&1 )"
assert_contains "rc.2 body includes its new bullet (Y)" "Feature Y" "$b_rc2"
assert_contains "rc.2 body surfaces an AMENDED bullet (X's new clause)" "now with caching" "$b_rc2"
case "$b_rc2" in
  *"Feature Z"*) fail "rc.2 body drops the unchanged bullet (Z)" ;;
  *)             pass "rc.2 body drops the unchanged bullet (Z)" ;;
esac
# Structure fidelity: an emitted subsection keeps the blank line after its `### `
# header (the authored loose list), matching the full-section body.
if printf '%s\n' "$b_rc2" | awk 'prev == "### Added" && $0 == "" { ok = 1 } { prev = $0 } END { exit ok ? 0 : 1 }'; then
  pass "rc.2 delta keeps the blank line after a subsection header"
else
  fail "rc.2 delta keeps the blank line after a subsection header"
fi
b_stable="$( cd "$REPO" && bash "$SECTION" body 0.4.0 2>&1 )"
assert_contains "stable body accumulates rc.1 (X)" "Feature X" "$b_stable"
assert_contains "stable body accumulates the unchanged bullet (Z)" "Feature Z" "$b_stable"
assert_contains "stable body accumulates rc.2 (Y)" "Feature Y" "$b_stable"
b_rc3="$( cd "$REPO" && bash "$SECTION" body 0.4.0-rc.3 2>&1 )"
assert_contains "re-spin rc.3 with no new bullets says so" "No changelog changes since v0.4.0-rc.2" "$b_rc3"

fi

# =============================================================================
# M2: release-cut.sh state machine (D1) + lockstep promote (D5)
# =============================================================================
REPO_ROOT="$(cd "$SCRIPTS_DIR/.." && pwd)"
RC="$REPO_ROOT/.agents/skills/uzi-release/scripts/release-cut.sh"
[ -f "$RC" ] || { echo "release-scripts-test: $RC not found" >&2; exit 2; }

D2N=0
d2tick() { D2N=$((D2N + 1)); printf '2026-10-%02dT00:00:00' "$D2N"; }
gcommit() { # gcommit <dir> <msg>
  local when; when="$(d2tick)"
  GIT_AUTHOR_DATE="$when" GIT_COMMITTER_DATE="$when" git -C "$1" commit -q -m "$2"
}

# A fresh minimal repo carrying the sibling scripts release-cut orchestrates, plus a
# v0.1.0 stable baseline. UZI_CHANGELOG_REPO_URL is exported at call time so
# changelog-links needs no remote.
seed_repo() {
  local d="$1"
  mkdir -p "$d/scripts/lib" "$d/deploy/chart" "$d/api"
  local s
  for s in changelog-fragments worker-tag-autobump changelog-links assert-changelog-covers-release changelog-section assert-worker-tag-decoupled; do
    [ -f "$SCRIPTS_DIR/$s.sh" ] && { cp "$SCRIPTS_DIR/$s.sh" "$d/scripts/$s.sh"; chmod +x "$d/scripts/$s.sh"; }
  done
  # The shared shipping-path lib used by the oracle, release-cut and autobump.
  cp "$SCRIPTS_DIR/lib/shipping-paths.sh" "$d/scripts/lib/shipping-paths.sh"
  cp "$SCRIPTS_DIR/lib/dependency-bump.sh" "$d/scripts/lib/dependency-bump.sh"
  cp "$SCRIPTS_DIR/lib/changelog-fragments.sh" "$d/scripts/lib/changelog-fragments.sh"
  cp "$SCRIPTS_DIR/lib/changelog-fragments.awk" "$d/scripts/lib/changelog-fragments.awk"
  cp "$SCRIPTS_DIR/lib/release-mode.sh" "$d/scripts/lib/release-mode.sh"
  cp "$SCRIPTS_DIR/lib/changelog-bullets.awk" "$d/scripts/lib/changelog-bullets.awk"
  [ ! -f "$SCRIPTS_DIR/lib/ghcr.sh" ] || cp "$SCRIPTS_DIR/lib/ghcr.sh" "$d/scripts/lib/ghcr.sh"
  cat > "$d/deploy/chart/Chart.yaml" <<'YAML'
apiVersion: v2
name: uzi
version: 0.1.0
appVersion: "0.1.0"
YAML
  cat > "$d/deploy/chart/values.yaml" <<'YAML'
workers:
  image:
    repository: ghcr.io/x/worker
    tag: "0.1.0"
YAML
  cat > "$d/CHANGELOG.md" <<'MD'
# Changelog

## [Unreleased]

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
  echo 'package main' > "$d/api/base.go"
  git -C "$d" init -q
  git -C "$d" config user.email t@example.com
  git -C "$d" config user.name test
  git -C "$d" symbolic-ref HEAD refs/heads/main
  git -C "$d" add -A
  gcommit "$d" "chore(release): v0.1.0"
  git -C "$d" tag v0.1.0
}
add_feature() { # add_feature <dir> <issue>
  echo "package main // $2" > "$1/api/feature_$2.go"
  git -C "$1" add -A
  gcommit "$1" "Feature $2 (#$2)"
}
put_changelog() { # put_changelog <dir> ; stdin -> CHANGELOG.md, committed
  cat > "$1/CHANGELOG.md"
  git -C "$1" add CHANGELOG.md
  gcommit "$1" "docs: changelog"
}
run_rc() { # run_rc <dir> <args...>  -> sets RC_RC / RC_OUT
  local d="$1"; shift
  RC_OUT="$( cd "$d" && UZI_RELEASE_OFFLINE_FIXTURE=1 UZI_CHANGELOG_REPO_URL="https://github.com/vtmocanu/uzi" bash "$RC" "$@" 2>&1 )"; RC_RC=$?
}
chart_ver()   { awk '/^version:/{print $2; exit}' "$1/deploy/chart/Chart.yaml"; }
pin_tag()     { awk '/^workers:/{w=1} w&&/^    tag:/{gsub(/"/,"",$2);print $2;exit}' "$1/deploy/chart/values.yaml"; }
head_msg()    { git -C "$1" log -1 --format=%s; }
has_heading() { grep -qE "^## \[$2\]" "$1/CHANGELOG.md"; }
# add_origin <dir>: give <dir> a LOCAL bare origin (a real remote for `git ls-remote`)
# and push main, so release-cut's --promote remote-tag check can distinguish a pushed
# RC from a local-only one. push_tag_to_origin <dir> <tag> publishes a tag to it.
add_origin() {
  local d="$1"; local bare="$d.origin.git"
  git init -q --bare "$bare"
  git -C "$d" remote add origin "$bare"
  # main + the baseline tag (v0.1.0), so the pin's tag counts as PUBLISHED and the
  # origin-aware autobump does not treat the seed baseline as local-only. A later RC tag
  # is pushed explicitly (push_tag_to_origin) or deliberately withheld per test.
  git -C "$d" push -q origin main --tags
}
push_tag_to_origin() { git -C "$1" push -q origin "refs/tags/$2"; }

feature_201_changelog() { printf '# Changelog\n\n## [Unreleased]\n### Added\n- **Feature 201** (#201)\n\n## [0.1.0] - 2026-09-01\n### Added\n- **Initial** (#100)\n' | put_changelog "$1"; }

published_rc() {
  seed_repo "$1"; add_origin "$1"; add_feature "$1" 201
  feature_201_changelog "$1"
  run_rc "$1" 0.2.0
  assert_eq "listing fixture initial RC succeeds" 0 "$RC_RC"
  git -C "$1" tag v0.2.0-rc.1
  push_tag_to_origin "$1" v0.2.0-rc.1
}
# Real cuts preserve published blocks while grouping newly pending categories.
fold_order_regressions() {
  local d mode inventory version base before oldblock oldaddition section expected
  # Four finite, independent fixtures: assertion failures do not block sibling cases.
  for mode in next skip; do
    for inventory in legacy mixed; do
      d="$(mktemp -d)"; seed_repo "$d"; add_feature "$d" 201
      printf '# Changelog\n\n## [Unreleased]\n### Changed\n- **Published change** (#201)\n  Published continuation bytes.\n' > "$d/.git/notes"
      if [ "$mode" = skip ]; then
        printf '### Added\n- **Published addition** (#201)\n  Addition continuation bytes.\n' >> "$d/.git/notes"
      fi
      printf '\n## [0.1.0] - 2026-09-01\n### Added\n- **Initial** (#100)\n' >> "$d/.git/notes"
      put_changelog "$d" < "$d/.git/notes"
      run_rc "$d" 0.2.0
      assert_eq "$mode $inventory ordering fixture first cut" 0 "$RC_RC"
      git -C "$d" tag v0.2.0-rc.1
      before="$(awk '/^## \[0.1.0\]/{p=1} p&&/^\[Unreleased\]:/{exit} p' "$d/CHANGELOG.md")"
      oldblock="$(awk '/^- \*\*Published change/{p=1} p && NF==0{exit} p' "$d/CHANGELOG.md")"
      oldaddition="$(awk '/^- \*\*Published addition/{p=1} p && NF==0{exit} p' "$d/CHANGELOG.md")"
      awk '
        {print}
        /^## \[Unreleased\]$/ {
          print "### Fixed"; print "- **Pending fix** (#202)"; print "  Fix continuation bytes."
          print "### Added"; print "- **Pending addition** (#202)"; print "  Pending continuation bytes."
        }
      ' "$d/CHANGELOG.md" > "$d/.git/pending-notes"
      put_changelog "$d" < "$d/.git/pending-notes"
      if [ "$inventory" = mixed ]; then
        mkdir -p "$d/changelog.d"
        printf '### Added\n\n- **Pending fragment (#202).**\n  Fragment continuation bytes.\n' > "$d/changelog.d/01234567-89ab-4cde-8fab-0123456789ab.md"
        git -C "$d" add -- changelog.d/01234567-89ab-4cde-8fab-0123456789ab.md
        gcommit "$d" 'docs: ordering fragment'
      fi
      add_feature "$d" 202
      version=0.2.0; base=0.2.0
      if [ "$mode" = skip ]; then version=0.3.0; base=0.3.0; fi
      if [ "$mode" = skip ]; then run_rc "$d" "$version" --skip-promote; else run_rc "$d" "$version"; fi
      if [ "$RC_RC" -ne 0 ]; then printf '%s\n' "$RC_OUT"; fi
      assert_eq "$mode $inventory ordered cut succeeds" 0 "$RC_RC"
      section="$(awk -v base="$base" 'index($0, "## [" base "]")==1{s=1;next} s&&/^## \[/{exit} s' "$d/CHANGELOG.md")"
      expected=$'### Changed\n### Added\n### Fixed'
      if [ "$mode" = skip ]; then expected=$'### Added\n### Changed\n### Fixed'; fi
      assert_eq "$mode $inventory one heading per category in published/pending order" "$expected" "$(printf '%s\n' "$section" | awk '/^### /')"
      expected=$'- **Published change**\n- **Pending addition**\n- **Pending fix**'
      if [ "$mode" = skip ]; then expected=$'- **Published addition**\n- **Pending addition**\n- **Published change**\n- **Pending fix**'; fi
      if [ "$inventory" = mixed ]; then
        if [ "$mode" = skip ]; then expected=$'- **Published addition**\n- **Pending addition**\n- **Pending fragment\n- **Published change**\n- **Pending fix**'
        else expected=$'- **Published change**\n- **Pending addition**\n- **Pending fragment\n- **Pending fix**'; fi
      fi
      assert_eq "$mode $inventory new blocks append to matching categories" "$expected" "$(printf '%s\n' "$section" | awk '/^- /{sub(/ \(.*/, ""); print}')"
      assert_contains "$mode $inventory published block bytes survive" "$oldblock" "$section"
      if [ "$mode" = skip ]; then assert_contains "$mode $inventory published addition bytes survive" "$oldaddition" "$section"; fi
      assert_contains "$mode $inventory pending continuation survives" '  Pending continuation bytes.' "$section"
      assert_eq "$mode $inventory released section bytes unchanged" "$before" "$(awk '/^## \[0.1.0\]/{p=1} p&&/^\[Unreleased\]:/{exit} p' "$d/CHANGELOG.md")"
      rm -rf "$d"
    done
  done
}
if [ "${1:-}" != "--fragments-only" ]; then
if [ "$#" -eq 0 ] || [ "${1:-}" = "--fold-order-only" ]; then fold_order_regressions; fi
if [ "${1:-}" = "--fold-order-only" ]; then
  printf '=== fold ordering: %s passed, %s failed ===\n' "$PASSES" "$FAILS"
  [ "$FAILS" -eq 0 ]; exit $?
fi

# #1466: public release seams, with independent fixtures for each runtime surface.
shipping_path_regressions() {
  # shellcheck source=scripts/lib/shipping-paths.sh
  . "$SCRIPTS_DIR/lib/shipping-paths.sh"
  local expected="agent/src agent/package.json agent/package-lock.json agent/tsconfig.json agent/bin agent/templates agent/devbox-global agent/codex"
  assert_eq "shared runtime set stays exact" "$expected" "$AGENT_PATHS"
  local p d out status subject
  for p in $expected; do
    if is_shipping "$p"; then pass "runtime entry $p ships"; else fail "runtime entry $p ships"; fi
    if is_shipping "$p/nested/runtime.txt"; then pass "runtime descendant $p ships"; else fail "runtime descendant $p ships"; fi
    if is_shipping "${p}-near/runtime.txt"; then fail "runtime boundary $p excludes near miss"; else pass "runtime boundary $p excludes near miss"; fi
  done
  for p in api/x.go controller/x.go web/src/x.ts deploy/chart/values.yaml docs/cli.md; do
    if is_shipping "$p"; then pass "existing shipping $p"; else fail "existing shipping $p"; fi
  done
  for p in api/x_test.go agent/src/foo.test.ts web/src/foo.test.tsx api/testdata/x agent/test/x.ts agent/templates/test/x agent/codex/x_test.go e2e/x fixtures/x .agents/skills/x prds/x.md agent/README.md; do
    if is_shipping "$p"; then fail "excluded $p"; else pass "excluded $p"; fi
  done

  # Finite two-case loop: each failure is tallied and the sibling fixture still runs.
  for p in agent/templates/base/runtime.txt agent/codex/runtime.txt; do
    d="$(mktemp -d)"; seed_repo "$d"
    mkdir -p "$d/$(dirname "$p")"; echo 'runtime change' > "$d/$p"
    git -C "$d" add "$p"
    subject="Update $p (#1466)"
    gcommit "$d" "$subject"  # one-parent squash merge, no CHANGELOG in this commit
    put_changelog "$d" <<'MD'
# Changelog

## [Unreleased]

## [0.2.0] - 2026-10-01
### Fixed
- Unrelated fix (#999)

## [0.1.0] - 2026-09-01
### Added
- Initial (#100)
MD
    status=0
    out="$(cd "$d" && bash scripts/assert-changelog-covers-release.sh HEAD v0.1.0 0.2.0-rc.1 2>&1)" || status=$?
    assert_eq "uncited $p squash merge rejected" "1" "$status"
    assert_contains "oracle names $p subject" "$subject" "$out"
    assert_contains "oracle names $p path" "changed $p" "$out"
    sed 's/Unrelated fix (#999)/Runtime fix (#1466)/' "$d/CHANGELOG.md" > "$d/CHANGELOG.tmp"
    mv "$d/CHANGELOG.tmp" "$d/CHANGELOG.md"
    git -C "$d" add CHANGELOG.md; gcommit "$d" "docs: cite runtime fix"
    status=0
    out="$(cd "$d" && bash scripts/assert-changelog-covers-release.sh HEAD v0.1.0 0.2.0-rc.1 2>&1)" || status=$?
    assert_eq "cited $p squash merge accepted" "0" "$status"
    rm -rf "$d"
  done

  d="$(mktemp -d)"; seed_repo "$d"; add_origin "$d"; add_feature "$d" 201
  put_changelog "$d" <<'MD'
# Changelog

## [Unreleased]
### Added
- Feature 201 (#201)

## [0.1.0] - 2026-09-01
### Added
- Initial (#100)
MD
  run_rc "$d" 0.2.0
  assert_eq "templates promote fixture first RC succeeds" "0" "$RC_RC"
  git -C "$d" tag v0.2.0-rc.1; push_tag_to_origin "$d" v0.2.0-rc.1
  mkdir -p "$d/agent/templates/base"; echo 'runtime change' > "$d/agent/templates/base/runtime.txt"
  git -C "$d" add agent/templates/base/runtime.txt; gcommit "$d" "Update worker template (#1466)"
  run_rc "$d" 0.3.0 --promote
  assert_eq "templates-only empty Unreleased refuses next RC" "3" "$RC_RC"
  assert_contains "templates-only reaches main-half empty refusal" "pending notes (legacy [Unreleased] plus fragments) are empty and no --changelog-file given" "$RC_OUT"
  if git -C "$d" rev-parse -q --verify refs/tags/v0.2.0 >/dev/null; then
    fail "templates-only empty refusal leaves stable tag absent"
  else
    pass "templates-only empty refusal leaves stable tag absent"
  fi
  assert_eq "templates-only empty refusal leaves chart at prior RC" "0.2.0-rc.1" "$(chart_ver "$d")"
  awk '{print} /^## \[Unreleased\]/ {print "### Fixed"; print "- Runtime template (#1466)"}' "$d/CHANGELOG.md" > "$d/CHANGELOG.tmp"
  mv "$d/CHANGELOG.tmp" "$d/CHANGELOG.md"
  git -C "$d" add CHANGELOG.md; gcommit "$d" "docs: cite runtime template"
  run_rc "$d" 0.3.0 --promote
  assert_eq "cited templates-only promote succeeds" "0" "$RC_RC"
  assert_eq "cited templates-only cuts next RC" "0.3.0-rc.1" "$(chart_ver "$d")"
  if git -C "$d" rev-parse -q --verify refs/tags/v0.2.0 >/dev/null; then pass "cited templates-only creates stable"; else fail "cited templates-only creates stable"; fi
  rm -rf "$d" "$d.origin.git"

  if grep -qE '^[[:space:]]*AGENT_PATHS=' "$SCRIPTS_DIR/worker-tag-autobump.sh"; then
    fail "autobump has no private AGENT_PATHS assignment"
  else
    pass "autobump has no private AGENT_PATHS assignment"
  fi
  assert_contains "autobump sources shipping library" '/lib/shipping-paths.sh"' "$(cat "$SCRIPTS_DIR/worker-tag-autobump.sh")"
  # Override only the fixture-local library. A non-agent sentinel proves both actual
  # Git diff selection and changed-path reporting consume that library's pathspecs.
  d="$(mktemp -d)"; seed_repo "$d"
  printf '\nAGENT_PATHS="runtime-sentinel"\n' >> "$d/scripts/lib/shipping-paths.sh"
  mkdir -p "$d/agent/src"; echo 'raw runtime test' > "$d/agent/src/foo.test.ts"
  git -C "$d" add scripts/lib/shipping-paths.sh agent/src/foo.test.ts
  gcommit "$d" "test: fixture-local pathspec selection"
  status=0
  out="$(cd "$d" && bash scripts/worker-tag-autobump.sh --check 0.2.0 2>&1)" || status=$?
  assert_eq "autobump ignores paths outside sentinel set" "0" "$status"
  echo 'runtime change' > "$d/runtime-sentinel"
  git -C "$d" add runtime-sentinel; gcommit "$d" "test: selected sentinel"
  status=0
  out="$(cd "$d" && bash scripts/worker-tag-autobump.sh --check 0.2.0 2>&1)" || status=$?
  assert_eq "autobump check selects fixture sentinel" "1" "$status"
  assert_contains "autobump changed-path diff reports sentinel" "runtime-sentinel" "$out"
  status=0
  out="$(cd "$d" && bash scripts/worker-tag-autobump.sh 0.2.0 2>&1)" || status=$?
  assert_eq "autobump edit selects fixture sentinel" "0" "$status"
  assert_eq "sentinel change bumps worker pin" "0.2.0" "$(pin_tag "$d")"
  rm -rf "$d"
}

# #1464 M1: production remains untouched until M2. Named cases also form the
# priority/pin-selection mutation seams; do not mutate scripts in this fixture.
ghcr_snapshot() {
  git -C "$1" symbolic-ref HEAD
  git -C "$1" rev-parse HEAD
  git -C "$1" for-each-ref --format='%(refname) %(objectname)'
  git -C "$1" rev-list --all | sort
  git -C "$1" status --porcelain
  git -C "$1" diff HEAD
}
ghcr_absent() {
  case "$3" in *"$2"*) fail "$1 (unexpected '$2')" ;; *) pass "$1" ;; esac
}
ghcr_defaults() {
  export GHCR_STUB=1 GHCR_IDENTITY='test uzi' GHCR_IDENTITY_RC=0
  export GHCR_KIND=User GHCR_KIND_RC=0
  export GHCR_BASE_OUT=0.1.1 GHCR_JVM_OUT=0.1.1 GHCR_BASE_RC=0 GHCR_JVM_RC=0
  export GHCR_OTHER_OUT=0.2.0 GHCR_OTHER_RC=0
}
ghcr_cut_seed() {
  local d="$1" mode="$2"
  seed_repo "$d"
  # Keep autobump deterministic: the HEAD pin has a locally published Git tag,
  # while GHCR responses independently decide whether its images were published.
  git -C "$d" tag v0.1.1
  if [ "$mode" = promote ] || [ "$mode" = skip-promote ] || [ "$mode" = promote-only ]; then
    # A published RC with a tag-tree pin distinct from the current HEAD pin.
    # Publication requires its changelog section and normalized links in the tag.
    cat > "$d/CHANGELOG.md" <<'MD'
# Changelog

## [Unreleased]

## [0.2.0] - 2026-10-01
### Added
- Initial RC (#100)

## [0.1.0] - 2026-09-01
### Added
- Initial (#100)
MD
    (cd "$d" && UZI_CHANGELOG_REPO_URL=https://github.com/vtmocanu/uzi bash scripts/changelog-links.sh)
    sed 's/0.1.0/0.2.0-rc.1/g' "$d/deploy/chart/Chart.yaml" > "$d/chart.tmp"
    mv "$d/chart.tmp" "$d/deploy/chart/Chart.yaml"
    git -C "$d" add deploy/chart/Chart.yaml CHANGELOG.md
    gcommit "$d" "chore(release): v0.2.0-rc.1"
    git -C "$d" tag v0.2.0-rc.1
    add_origin "$d"; push_tag_to_origin "$d" v0.2.0-rc.1
  fi
  # Nested tags must not be selected, even when they precede workers.image.
  cat > "$d/deploy/chart/values.yaml" <<'YAML'
workers:
  docker:
    image:
      tag: "docker-decoy"
  controller:
    image:
      tag: "controller-decoy"
  image:
    repository: ghcr.io/test/uzi/agent-base
    tag: "0.1.1"
other:
  image:
    tag: "other-decoy"
YAML
  git -C "$d" add deploy/chart/values.yaml
  gcommit "$d" "test: current HEAD worker pin (#201)"
  if [ "$mode" != promote-only ] && [ "$mode" != auto-promote-only ]; then add_feature "$d" 201; fi
  put_changelog "$d" <<'MD'
# Changelog

## [Unreleased]
### Added
- Feature 201 (#201)

## [0.2.0] - 2026-10-01
### Added
- Initial RC (#100)

## [0.1.0] - 2026-09-01
### Added
- Initial (#100)
MD
  if [ "$mode" = auto-promote-only ]; then
    # The implicit promote-only route requires empty Unreleased as well as no
    # shipping commits since the RC.
    awk '/^## \[Unreleased\]/ {print; skip=1; next}
         skip && /^## \[/ {skip=0}
         !skip {print}' "$d/CHANGELOG.md" > "$d/changelog.tmp"
    mv "$d/changelog.tmp" "$d/CHANGELOG.md"
    (cd "$d" && UZI_CHANGELOG_REPO_URL=https://github.com/vtmocanu/uzi bash scripts/changelog-links.sh)
    git -C "$d" add CHANGELOG.md
    sed 's/0.1.0/0.2.0-rc.1/g' "$d/deploy/chart/Chart.yaml" > "$d/chart.tmp"
    mv "$d/chart.tmp" "$d/deploy/chart/Chart.yaml"
    git -C "$d" add deploy/chart/Chart.yaml
    gcommit "$d" "chore(release): v0.2.0-rc.1"
    git -C "$d" tag v0.2.0-rc.1
    add_origin "$d"; push_tag_to_origin "$d" v0.2.0-rc.1
  fi
}
ghcr_queries() {
  local label="$1" log="$2" owner="$3" text
  text="$(cat "$log")"
  assert_contains "$label identity lookup" 'repo view --json owner,name' "$text"
  assert_contains "$label owner type lookup" 'api users/test --jq .type' "$text"
  local image line endpoint matched count
  # Each worker endpoint must be queried exactly once; argument order is unrestricted.
  for image in agent-base agent-jvm; do
    endpoint="/${owner}/packages/container/uzi%2F${image}/versions"
    matched=""; count=0
    while IFS= read -r line; do
      case "$line" in *"$endpoint"*) matched="$line"; count=$((count + 1)) ;; esac
    done < "$log"
    assert_eq "$label $image queried once" 1 "$count"
    assert_contains "$label $image endpoint" "$endpoint" "$matched"
    assert_contains "$label $image pagination" --paginate "$matched"
    assert_contains "$label $image tag projection" '.[].metadata.container.tags[]' "$matched"
  done
  ghcr_absent "$label no unrelated package" 'uzi%2Fapi/' "$text"
}
ghcr_outcome() {
  local label="$1" image="$2" outcome="$3" output="$4" line found=""
  [ "$outcome" != error ] || outcome="query error"
  # Match image and outcome on one line, without prescribing punctuation or URI form.
  while IFS= read -r line; do
    case "$line" in *"$image"*) case "$line" in *"$outcome"*) found=1; break ;; esac ;; esac
  done <<< "$output"
  if [ -n "$found" ]; then pass "$label $image $outcome"; else fail "$label $image $outcome"; fi
}
ghcr_cut_case() {
  local name="$1" mode="$2" base="$3" jvm="$4" d log before head_before version verb owner out_lower
  local failures_before="$FAILS"
  # M2 mutation runners may select one named priority/pin case without changing fixtures.
  [ -z "${GHCR_CASE_FILTER:-}" ] || [ "$name" = "$GHCR_CASE_FILTER" ] || return 0
  d="$(mktemp -d)"; ghcr_cut_seed "$d" "$mode"; ghcr_defaults
  log="$(mktemp)"; export GHCR_QUERY_LOG="$log"
  case "$base" in absent) GHCR_BASE_OUT=0.2.0 ;; error) GHCR_BASE_RC=7 ;; esac
  case "$jvm" in absent) GHCR_JVM_OUT=0.2.0 ;; error) GHCR_JVM_RC=7 ;; esac
  owner=users/test
  case "$name" in
    org-present) GHCR_KIND=Organization; owner=orgs/test ;;
    kind-fallback-present) GHCR_KIND=''; GHCR_KIND_RC=7 ;;
    identity-rc-present) GHCR_IDENTITY_RC=7 ;;
    identity-rc-missing) GHCR_IDENTITY_RC=7; GHCR_BASE_OUT=0.2.0 ;;
    unresolved-owner) GHCR_IDENTITY=''; GHCR_IDENTITY_RC=7 ;;
    empty-head-pin)
      sed 's/tag: "0.1.1"/tag: ""/' "$d/deploy/chart/values.yaml" > "$d/values.tmp"
      mv "$d/values.tmp" "$d/deploy/chart/values.yaml"
      git -C "$d" add deploy/chart/values.yaml; gcommit "$d" "test: empty HEAD pin (#201)" ;;
    unreadable-head-pin)
      git -C "$d" rm -q deploy/chart/values.yaml; gcommit "$d" "test: missing HEAD pin (#201)" ;;
  esac
  before="$(ghcr_snapshot "$d")"
  head_before="$(git -C "$d" rev-parse HEAD)"
  version=0.2.0; verb=""
  case "$mode" in
    stable) verb=--stable ;;
    promote|skip-promote) version=0.3.0; verb="--$mode" ;;
    promote-only) verb=--promote-only ;;
    auto-promote-only) version=0.3.0; verb=--promote ;;
  esac
  if [ -n "$verb" ]; then run_rc "$d" "$version" "$verb"; else run_rc "$d" "$version"; fi
  out_lower="$(printf '%s' "$RC_OUT" | tr '[:upper:]' '[:lower:]')"
  if [ "$base" != absent ] && [ "$jvm" != absent ] &&
     [ "$name" != empty-head-pin ] && [ "$name" != unreadable-head-pin ]; then
    assert_eq "$name succeeds" 0 "$RC_RC"
    case "$name" in
      both-error|present-error|unresolved-owner|identity-rc-present|identity-rc-missing)
        assert_contains "$name warning diagnostic" 'warning:' "$out_lower"
        assert_contains "$name publish gate warning" 'publish gate' "$out_lower"
        assert_contains "$name warning selects HEAD pin" 0.1.1 "$RC_OUT"
        ghcr_absent "$name no publication repair on error" "the pinned tag's publication did not complete" "$RC_OUT" ;;
      *) ghcr_absent "$name present is silent" 'worker pin pre-flight' "$RC_OUT" ;;
    esac
    if [ "$mode" = promote-only ]; then
      assert_eq "$name no owner/package queries" "" "$(cat "$log")"
      assert_eq "$name keeps main chart" 0.2.0-rc.1 "$(chart_ver "$d")"
      assert_eq "$name clean tree" "" "$(git -C "$d" status --porcelain)"
    else
      if [ "$name" = unresolved-owner ] || [ "$name" = identity-rc-present ] || [ "$name" = identity-rc-missing ]; then
        assert_contains "$name identity attempted" 'repo view --json owner,name' "$(cat "$log")"
        ghcr_absent "$name no package queries" '/packages/container/' "$(cat "$log")"
      else
        ghcr_queries "$name" "$log" "$owner"
      fi
      assert_eq "$name committed clean tree" "" "$(git -C "$d" status --porcelain)"
      if [ "$mode" = auto-promote-only ]; then
        assert_eq "$name keeps main chart" 0.2.0-rc.1 "$(chart_ver "$d")"
        if git -C "$d" rev-parse -q --verify refs/tags/v0.2.0 >/dev/null; then pass "$name stable tag created"; else fail "$name stable tag created"; fi
      else
        assert_eq "$name cuts intended version" "$([ "$mode" = stable ] && echo "$version" || echo "$version-rc.1")" "$(chart_ver "$d")"
        if [ "$head_before" != "$(git -C "$d" rev-parse HEAD)" ]; then pass "$name committed cut"; else fail "$name committed cut"; fi
      fi
      if [ "$mode" = promote ]; then
        if git -C "$d" rev-parse -q --verify refs/tags/v0.2.0 >/dev/null; then pass "$name stable tag created"; else fail "$name stable tag created"; fi
      fi
    fi
  else
    assert_eq "$name refuses" 3 "$RC_RC"
    assert_contains "$name preflight diagnostic" 'worker pin pre-flight' "$RC_OUT"
    assert_eq "$name HEAD tags refs reachable commits and tree unchanged" "$before" "$(ghcr_snapshot "$d")"
    assert_eq "$name clean tree after refusal" "" "$(git -C "$d" status --porcelain)"
    case "$name" in
      empty-head-pin|unreadable-head-pin)
        ghcr_absent "$name no package queries" '/packages/container/' "$(cat "$log")"
        assert_contains "$name instrument failure diagnostic" 'instrument' "$out_lower"
        assert_contains "$name pin read failure diagnostic" 'read' "$out_lower" ;;
      *)
        ghcr_queries "$name" "$log" "$owner"
        ghcr_outcome "$name" agent-base "$base" "$RC_OUT"
        ghcr_outcome "$name" agent-jvm "$jvm" "$RC_OUT"
        # Absent outranks error regardless of image order.
        assert_contains "$name absent priority" absent "$RC_OUT"
        assert_contains "$name publication repair" "the pinned tag's publication did not complete" "$RC_OUT"
        assert_contains "$name release workflow repair" 'release.yml' "$RC_OUT"
        assert_contains "$name verify repair hint" 'verify' "$out_lower"
        assert_contains "$name rerun cut hint" 're-run' "$out_lower"
        assert_contains "$name cut repair hint" 'cut' "$out_lower"
        assert_contains "$name selects HEAD pin rather than requested version or RC tree" 0.1.1 "$RC_OUT" ;;
    esac
  fi
  printf 'GHCR_CASE=%s RELEASE_EXIT=%s ASSERTION_FAILURES=%s\n' "$name" "$RC_RC" "$((FAILS - failures_before))"
  rm -rf "$d" "$d.origin.git"; rm -f "$log"
}
ghcr_verify_case() {
  local name="$1" baseline="$2" record="$3" d old new oldrc newrc querylog owner want=1
  local failures_before="$FAILS"
  [ -z "${GHCR_CASE_FILTER:-}" ] || [ "verify-$name" = "$GHCR_CASE_FILTER" ] || return 0
  d="$(mktemp -d)"; seed_repo "$d"; ghcr_defaults
  export GHCR_BASE_OUT=$'0.2.0\n0.1.0' GHCR_JVM_OUT=$'0.2.0\n0.1.0'
  case "$name" in
    present) want=0 ;;
    org-present) want=0; GHCR_KIND=Organization ;;
    kind-fallback-present) want=0; GHCR_KIND=''; GHCR_KIND_RC=7 ;;
    identity-rc-present) want=0; GHCR_IDENTITY_RC=7 ;;
    unresolved-owner) want=3; GHCR_IDENTITY=''; GHCR_IDENTITY_RC=7 ;;
    image-absent) GHCR_BASE_OUT=0.1.0 ;;
    image-error) GHCR_BASE_RC=7 ;;
    pin-absent) GHCR_BASE_OUT=0.2.0 ;;
    pin-error) GHCR_JVM_RC=7 ;;
    empty-pin)
      sed 's/tag: "0.1.0"/tag: ""/' "$d/deploy/chart/values.yaml" > "$d/values.tmp"
      mv "$d/values.tmp" "$d/deploy/chart/values.yaml" ;;
    unreadable-pin) rm "$d/deploy/chart/values.yaml" ;;
    nested-pin)
      want=0
      cat > "$d/deploy/chart/values.yaml" <<'YAML'
workers:
  docker:
    image:
      tag: "docker-decoy"
  controller:
    image:
      tag: "controller-decoy"
  image:
    tag: "0.1.0"
YAML
      ;;
  esac
  git -C "$d" add deploy/chart/values.yaml
  git -C "$d" diff --cached --quiet || gcommit "$d" "test: verifier pin"
  git -C "$d" tag v0.2.0
  old="$(mktemp)"; new="$(mktemp)"; querylog="$(mktemp)"
  export GHCR_QUERY_LOG="$querylog"
  if [ -n "$baseline" ]; then
    oldrc=0; (cd "$d" && bash "$baseline" 0.2.0) > "$old" 2>&1 || oldrc=$?
    { printf '=== baseline %s ===\nEXIT=%s\n' "$name" "$oldrc"; cat "$old"; } >> "$record"
    assert_eq "verify $name baseline status" "$want" "$oldrc"
  fi
  : > "$querylog"
  newrc=0; (cd "$d" && bash "$REPO_ROOT/.agents/skills/uzi-release/scripts/release-verify.sh" 0.2.0) > "$new" 2>&1 || newrc=$?
  { printf '=== current %s ===\nEXIT=%s\n' "$name" "$newrc"; cat "$new"; } >> "$record"
  assert_eq "verify $name current status" "$want" "$newrc"
  if [ -n "$baseline" ]; then
    assert_eq "verify $name unchanged status" "$oldrc" "$newrc"
    if cmp -s "$old" "$new"; then pass "verify $name exact baseline output"; else fail "verify $name exact baseline output"; fi
  fi
  if [ "$name" = unresolved-owner ]; then
    ghcr_absent "verify $name no package queries" '/packages/container/' "$(cat "$querylog")"
  else
    owner=users/test; [ "$name" != org-present ] || owner=orgs/test
    # Verifier also checks the release artifacts; only inspect the worker endpoints here.
    local image endpoint matched line
    for image in agent-base agent-jvm; do
      endpoint="/${owner}/packages/container/uzi%2F${image}/versions"; matched=""
      while IFS= read -r line; do
        case "$line" in *"$endpoint"*) matched="$line"; break ;; esac
      done < "$querylog"
      assert_contains "verify $name $image owner endpoint" "$endpoint" "$matched"
      assert_contains "verify $name $image pagination" --paginate "$matched"
    done
  fi
  case "$name" in
    image-absent) assert_contains "verify $name membership assertion" 'agent-base:0.2.0 NOT found' "$(cat "$new")" ;;
    pin-absent) assert_contains "verify $name membership assertion" 'agent-base:0.1.0 NOT on GHCR' "$(cat "$new")" ;;
    image-error|pin-error) assert_contains "verify $name honest query error" 'could not query GHCR' "$(cat "$new")" ;;
    unresolved-owner) assert_contains "verify $name identity failure" 'could not resolve owner/repo via gh' "$(cat "$new")" ;;
    empty-pin|unreadable-pin) assert_contains "verify $name unreadable assertion" 'could not read workers.image.tag' "$(cat "$new")" ;;
  esac
  printf 'GHCR_CASE=verify-%s VERIFY_EXIT=%s ASSERTION_FAILURES=%s\n' "$name" "$newrc" "$((FAILS - failures_before))"
  rm -rf "$d"; rm -f "$old" "$new" "$querylog"
  unset GHCR_QUERY_LOG
}
# Direct helper contract: the verifier intentionally ignores the pin reader's status.
ghcr_reader_case() {
  local name="$1" record="$2" d out rc=0 failures_before="$FAILS" rev
  [ -z "${GHCR_CASE_FILTER:-}" ] || [ "reader-$name" = "$GHCR_CASE_FILTER" ] || return 0
  d="$(mktemp -d)"; seed_repo "$d"
  if [ "$name" = large-pin ]; then
    # Finite suffix larger than pipe buffers exposes early-reader SIGPIPE.
    awk 'BEGIN {for(i=0;i<100000;i++) print "# pin reader suffix"}' >> "$d/deploy/chart/values.yaml"
    git -C "$d" add deploy/chart/values.yaml; gcommit "$d" "test: large pin reader input"
    rev=HEAD
  else
    rev=refs/tags/missing-fixture-revision
  fi
  out="$(
    cd "$d" || exit 1
    set -o pipefail
    # shellcheck source=scripts/lib/ghcr.sh
    . "$d/scripts/lib/ghcr.sh" && ghcr_read_pin "$rev"
  )" || rc=$?
  { printf '=== reader %s ===\nEXIT=%s\n' "$name" "$rc"; printf '%s\n' "$out"; } >> "$record"
  if [ "$name" = large-pin ]; then
    assert_eq "reader $name exact pin" 0.1.0 "$out"
    assert_eq "reader $name status under pipefail" 0 "$rc"
  else
    if [ "$rc" -ne 0 ]; then pass "reader $name nonzero status"; else fail "reader $name nonzero status"; fi
    assert_eq "reader $name no pin" "" "$out"
  fi
  printf 'GHCR_CASE=reader-%s READER_EXIT=%s ASSERTION_FAILURES=%s\n' "$name" "$rc" "$((FAILS - failures_before))"
  rm -rf "$d"
}
ghcr_regressions() {
  local baseline="$GHCR_BASELINE" record name
  if [ -n "$baseline" ]; then
    [ -f "$baseline" ] || { fail "supplied frozen verifier unavailable: $baseline"; return; }
    # Verifier calls run inside fixtures, so resolve a supplied path first.
    baseline="$(cd "$(dirname "$baseline")" && pwd)/$(basename "$baseline")"
    record="$(mktemp "$TMPDIR/ghcr-baseline.XXXXXX")"
    echo "GHCR_BASELINE_LOG=$record"
  else
    record="$(mktemp "$TMPDIR/ghcr-current.XXXXXX")"
    echo "GHCR_CURRENT_LOG=$record"
    echo "  No frozen baseline supplied; asserting current verifier behavior only."
  fi
  # Finite scenario matrices: assertion failures never prevent sibling scenarios.
  ghcr_cut_case missing-base default absent present
  ghcr_cut_case missing-jvm default present absent
  ghcr_cut_case absent-error default absent error
  ghcr_cut_case error-absent default error absent
  ghcr_cut_case both-error default error error
  ghcr_cut_case present-error default present error
  ghcr_cut_case unresolved-owner default present present
  ghcr_cut_case empty-head-pin default present present
  ghcr_cut_case unreadable-head-pin default present present
  ghcr_cut_case present default present present
  ghcr_cut_case org-present default present present
  ghcr_cut_case kind-fallback-present default present present
  ghcr_cut_case identity-rc-present default present present
  ghcr_cut_case identity-rc-missing default present present
  ghcr_cut_case stable-refusal stable absent present
  ghcr_cut_case stable-present stable present present
  ghcr_cut_case skip-refusal skip-promote absent present
  ghcr_cut_case skip-present skip-promote present present
  ghcr_cut_case promote-refusal promote absent present
  ghcr_cut_case promote-present promote present present
  ghcr_cut_case promote-only-present promote-only present present
  ghcr_cut_case auto-promote-only-present auto-promote-only present present
  for name in present org-present kind-fallback-present identity-rc-present unresolved-owner image-absent image-error pin-absent pin-error empty-pin unreadable-pin nested-pin; do
    ghcr_verify_case "$name" "$baseline" "$record"
  done
  if [ -f "$SCRIPTS_DIR/lib/ghcr.sh" ]; then
    ghcr_reader_case large-pin "$record"
    ghcr_reader_case missing-revision "$record"
  else
    echo "  SKIP large-pin / missing-revision helper fixtures until scripts/lib/ghcr.sh exists"
  fi
  [ -z "${GHCR_CASE_FILTER:-}" ] || [ "$((PASSES + FAILS))" -gt 0 ] || fail "unknown GHCR_CASE_FILTER: $GHCR_CASE_FILTER"
  unset GHCR_STUB GHCR_QUERY_LOG GHCR_IDENTITY GHCR_IDENTITY_RC GHCR_KIND GHCR_KIND_RC
  unset GHCR_BASE_OUT GHCR_BASE_RC GHCR_JVM_OUT GHCR_JVM_RC GHCR_OTHER_OUT GHCR_OTHER_RC
}
if [ "${1:-}" != "--shipping-paths-only" ]; then ghcr_regressions; fi
if [ "${1:-}" = "--ghcr-only" ]; then
  echo "=== GHCR regressions: $PASSES passed, $FAILS failed ==="
  [ "$FAILS" -eq 0 ]
  exit $?
fi

shipping_path_regressions
if [ "${1:-}" = "--shipping-paths-only" ]; then
  echo "=== shipping-path regressions: $PASSES passed, $FAILS failed ==="
  [ "$FAILS" -eq 0 ]
  exit $?
fi

echo "=== M2: release-cut first RC ==="
S1="$(mktemp -d)"; seed_repo "$S1"; add_feature "$S1" 201
put_changelog "$S1" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S1" 0.2.0
assert_eq "first RC exits 0"                 "0"                          "$RC_RC"
assert_eq "first RC chart is 0.2.0-rc.1"     "0.2.0-rc.1"                 "$(chart_ver "$S1")"
assert_eq "first RC commit message"          "chore(release): v0.2.0-rc.1" "$(head_msg "$S1")"
if has_heading "$S1" 0.2.0; then pass "first RC opens [0.2.0] section"; else fail "first RC opens [0.2.0] section"; fi
if git -C "$S1" rev-parse -q --verify refs/tags/v0.2.0-rc.1 >/dev/null; then fail "script must NOT tag the RC"; else pass "script does NOT tag the RC (lead tags after CI)"; fi

echo "=== M2: next RC (empty [Unreleased]) ==="
git -C "$S1" tag v0.2.0-rc.1
run_rc "$S1" 0.2.0
assert_eq "next RC exits 0"                  "0"          "$RC_RC"
assert_eq "next RC chart is 0.2.0-rc.2"      "0.2.0-rc.2" "$(chart_ver "$S1")"

echo "=== M2: next RC folds a non-empty [Unreleased] into the open section ==="
# next_rc_fixture <dir> : first RC of 0.2.0 tagged, then CHANGELOG from stdin committed.
next_rc_fixture() {
  local cl; cl="$(cat)"   # read stdin first: the first-RC cut below must not consume it
  seed_repo "$1"; add_feature "$1" 201
  printf '# Changelog\n\n## [Unreleased]\n### Added\n- **Feature 201** (#201)\n\n## [0.1.0] - 2026-09-01\n### Added\n- **Initial** (#100)\n' | put_changelog "$1"
  run_rc "$1" 0.2.0
  git -C "$1" tag v0.2.0-rc.1
  add_feature "$1" 250
  printf '%s\n' "$cl" | put_changelog "$1"
}
open_section() { awk '/^## \[0\.2\.0\]/{s=1;next} s&&/^## \[/{exit} s' "$1/CHANGELOG.md"; }
S3="$(mktemp -d)"
next_rc_fixture "$S3" <<'MD'
# Changelog

## [Unreleased]
### Fixed
- **Late fix** (#250)

### Added
- **Late thing** (#250)

## [0.2.0] - 2026-10-01
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S3" 0.2.0
assert_eq "next RC with non-empty [Unreleased] exits 0" "0" "$RC_RC"
assert_eq "next RC chart is 0.2.0-rc.2 after a fold" "0.2.0-rc.2" "$(chart_ver "$S3")"
assert_eq "[Unreleased] is empty after the fold" "" \
  "$(awk '/^## \[Unreleased\]/{f=1;next} f&&/^## \[/{exit} f' "$S3/CHANGELOG.md" | tr -d '[:space:]')"
sec="$(open_section "$S3")"
assert_eq "open section keeps ONE ### Added" "1" "$(printf '%s\n' "$sec" | grep -c '^### Added')"
assert_eq "open section gains ### Fixed" "1" "$(printf '%s\n' "$sec" | grep -c '^### Fixed')"
assert_eq "Late thing lands under the existing ### Added, after Feature 201" "### Added|- **Feature 201**|- **Late thing**|### Fixed|- **Late fix**" \
  "$(printf '%s\n' "$sec" | grep -v '^[[:space:]]*$' | sed 's/^\(- \*\*[^*]*\*\*\).*/\1/' | paste -sd'|' -)"

echo "=== M2: next RC fold refuses what it cannot place ==="
S3U="$(mktemp -d)"
next_rc_fixture "$S3U" <<'MD'
# Changelog

## [Unreleased]
### Improvements
- **Late thing** (#250)

## [0.2.0] - 2026-10-01
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
cl_before="$(cat "$S3U/CHANGELOG.md")"
run_rc "$S3U" 0.2.0
assert_eq "fold refuses an unknown subsection (exit 3)" "3" "$RC_RC"
assert_contains "refusal names the unknown subsection" "unknown heading" "$RC_OUT"
assert_eq "refused fold leaves CHANGELOG untouched" "$cl_before" "$(cat "$S3U/CHANGELOG.md")"
S3T="$(mktemp -d)"
next_rc_fixture "$S3T" <<'MD'
# Changelog

## [Unreleased]
- **Stray bullet** (#250)

## [0.2.0] - 2026-10-01
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S3T" 0.2.0
assert_eq "fold refuses text before the first ### (exit 3)" "3" "$RC_RC"
assert_contains "refusal quotes the stray line" "bullet outside a category" "$RC_OUT"
S3D="$(mktemp -d)"
next_rc_fixture "$S3D" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)

## [0.2.0] - 2026-10-01
### Added
- **Feature 201** (#201)

### Added
- **Older duplicate** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
cl_before="$(cat "$S3D/CHANGELOG.md")"
run_rc "$S3D" 0.2.0
assert_eq "fold refuses an open section that already repeats a ### (exit 3)" "3" "$RC_RC"
assert_contains "refusal names the duplicate subsection" "duplicate subsection in [0.2.0]: ### Added" "$RC_OUT"
assert_eq "duplicate-refused fold leaves CHANGELOG untouched" "$cl_before" "$(cat "$S3D/CHANGELOG.md")"

echo "=== M2: next RC fold collapses a repeated ### in [Unreleased] ==="
# A clean rebase can leave two `### Added` in [Unreleased] (changelog-union.sh --collapse).
S3R="$(mktemp -d)"
next_rc_fixture "$S3R" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Late one** (#250)

### Fixed
- **Late fix** (#250)

### Added
- **Late two** (#250)

## [0.2.0] - 2026-10-01
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S3R" 0.2.0
assert_eq "repeated ### in [Unreleased] folds (exit 0)" "0" "$RC_RC"
assert_eq "canonical pending view leaves one Added heading" 1 "$(open_section "$S3R" | awk '/^### Added$/{n++} END{print n+0}')"
assert_eq "both repeats land under the one ### Added, in order" "### Added|- **Feature 201**|- **Late one**|- **Late two**|### Fixed|- **Late fix**" \
  "$(open_section "$S3R" | grep -v '^[[:space:]]*$' | sed 's/^\(- \*\*[^*]*\*\*\).*/\1/' | paste -sd'|' -)"

echo "=== M2: next RC fold when the open section is the last in the file ==="
S3E="$(mktemp -d)"
next_rc_fixture "$S3E" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Late thing** (#250)

### Security
- **Late hardening** (#250)

## [0.2.0] - 2026-10-01
### Added
- **Feature 201** (#201)
MD
run_rc "$S3E" 0.2.0
assert_eq "fold with the open section last exits 0" "0" "$RC_RC"
assert_eq "last-section fold appends and opens subsections at EOF" "### Added|- **Feature 201**|- **Late thing**|### Security|- **Late hardening**" \
  "$(open_section "$S3E" | grep -v -e '^[[:space:]]*$' -e '^\[' | sed 's/^\(- \*\*[^*]*\*\*\).*/\1/' | paste -sd'|' -)"

echo "=== M2: refuse a new version with no verb + --stable refused in flight ==="
S4="$(mktemp -d)"; seed_repo "$S4"; add_feature "$S4" 201
put_changelog "$S4" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S4" 0.2.0; git -C "$S4" tag v0.2.0-rc.1
run_rc "$S4" 0.3.0
assert_eq "new version, no verb -> exit 3"      "3"          "$RC_RC"
assert_contains "refusal prints the in-flight facts" "in flight" "$RC_OUT"
run_rc "$S4" 0.3.0 --stable
assert_eq "--stable refused while RC in flight"  "3"         "$RC_RC"
assert_contains "--stable refusal names the RC"  "refused while" "$RC_OUT"

echo "=== M2: --skip-promote (abandon the RC) ==="
S6="$(mktemp -d)"; seed_repo "$S6"; add_feature "$S6" 201
put_changelog "$S6" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S6" 0.2.0; git -C "$S6" tag v0.2.0-rc.1
add_feature "$S6" 301
put_changelog "$S6" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 301** (#301)

## [0.2.0] - 2026-10-01
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S6" 0.3.0 --skip-promote
assert_eq "--skip-promote exits 0"           "0"          "$RC_RC"
assert_eq "--skip-promote chart is 0.3.0-rc.1" "0.3.0-rc.1" "$(chart_ver "$S6")"
if has_heading "$S6" 0.3.0; then pass "--skip-promote renames to [0.3.0]"; else fail "--skip-promote renames to [0.3.0]"; fi
if has_heading "$S6" 0.2.0; then fail "--skip-promote drops the old [0.2.0]"; else pass "--skip-promote drops the old [0.2.0]"; fi

echo "=== M2: --stable with no RC in flight ==="
S7="$(mktemp -d)"; seed_repo "$S7"; add_feature "$S7" 201
put_changelog "$S7" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S7" 0.2.0 --stable
assert_eq "--stable exits 0"          "0"       "$RC_RC"
assert_eq "--stable chart is 0.2.0 (no rc)" "0.2.0" "$(chart_ver "$S7")"

echo "=== M2b: --promote (lockstep) ==="
S8="$(mktemp -d)"; seed_repo "$S8"; add_origin "$S8"; add_feature "$S8" 201
put_changelog "$S8" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S8" 0.2.0; git -C "$S8" tag v0.2.0-rc.1; push_tag_to_origin "$S8" v0.2.0-rc.1
add_feature "$S8" 301
put_changelog "$S8" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 301** (#301)

## [0.2.0] - 2026-10-01
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
pin_before="$(pin_tag "$S8")"
run_rc "$S8" 0.3.0 --promote
assert_eq "--promote exits 0" "0" "$RC_RC"
if git -C "$S8" rev-parse -q --verify refs/tags/v0.2.0 >/dev/null; then pass "--promote creates stable tag v0.2.0"; else fail "--promote creates stable tag v0.2.0"; fi
assert_eq "v0.2.0 tag chart is stable 0.2.0" "0.2.0" "$(git -C "$S8" show v0.2.0:deploy/chart/Chart.yaml | awk '/^version:/{print $2;exit}')"
assert_eq "main chart is 0.3.0-rc.1"         "0.3.0-rc.1" "$(chart_ver "$S8")"
if has_heading "$S8" 0.3.0; then pass "--promote opens [0.3.0] on main"; else fail "--promote opens [0.3.0] on main"; fi
if git -C "$S8" rev-parse -q --verify refs/heads/release/0.2.0 >/dev/null; then fail "--promote leaves no release/0.2.0 branch"; else pass "--promote leaves no release/0.2.0 branch"; fi
assert_eq "--promote leaves the worker pin unchanged (D11)" "$pin_before" "$(pin_tag "$S8")"

echo "=== M2b: --promote takes promote-only when only a NON-shipping commit follows the RC ==="
# The cff43919 case: a docs/prd-only commit after the RC must NOT force an empty next RC.
# promote-only keys on SHIPPING commits (is_shipping), so it fires: tags the stable, cuts no
# next candidate, leaves main's Chart at the RC version. (On the raw-commit-count code this
# dropped to the main half and, with an empty [Unreleased], failed and rolled the stable back.)
S8P="$(mktemp -d)"; seed_repo "$S8P"; add_origin "$S8P"; add_feature "$S8P" 201
put_changelog "$S8P" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S8P" 0.2.0; git -C "$S8P" tag v0.2.0-rc.1; push_tag_to_origin "$S8P" v0.2.0-rc.1
mkdir -p "$S8P/prds"; echo 'anchor refresh' > "$S8P/prds/1359-x.md"   # NON-shipping, [Unreleased] stays empty
git -C "$S8P" add prds/1359-x.md; gcommit "$S8P" "docs(prd-1359): refresh anchors [skip ci]"
chart_before="$(chart_ver "$S8P")"
run_rc "$S8P" 0.3.0 --promote
assert_eq "--promote (docs-only follow) exits 0"                      "0"        "$RC_RC"
assert_contains "--promote (docs-only follow) is promote-only"        "promoted" "$RC_OUT"
assert_contains "--promote (docs-only follow) says nothing shipping"  "nothing shipping" "$RC_OUT"
if git -C "$S8P" rev-parse -q --verify refs/tags/v0.2.0 >/dev/null; then pass "promote-only creates stable tag v0.2.0"; else fail "promote-only creates stable tag v0.2.0"; fi
assert_eq "promote-only does NOT cut a next RC (main chart unchanged)" "$chart_before" "$(chart_ver "$S8P")"
if has_heading "$S8P" 0.3.0; then fail "promote-only opens NO [0.3.0] section"; else pass "promote-only opens NO [0.3.0] section"; fi

echo "=== M2b: --promote-only (stable from the RC commit, NO next candidate, main untouched) ==="
# The 0.83.1 case: SHIPPING work landed on main after the RC, it must not ride into the
# stable, and no next candidate is wanted yet. --promote would cut v0.3.0-rc.1 in lockstep
# (and, with empty [Unreleased] and no draft, refuse before tagging); --promote-only tags
# v0.2.0 from the RC commit and stops, whatever main carries. Same setup as S8.
S8O="$(mktemp -d)"; seed_repo "$S8O"; add_origin "$S8O"; add_feature "$S8O" 201
put_changelog "$S8O" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S8O" 0.2.0; git -C "$S8O" tag v0.2.0-rc.1; push_tag_to_origin "$S8O" v0.2.0-rc.1
add_feature "$S8O" 301
put_changelog "$S8O" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 301** (#301)

## [0.2.0] - 2020-01-01
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
head_before="$(git -C "$S8O" rev-parse HEAD)"; chart_before="$(chart_ver "$S8O")"
run_rc "$S8O" 0.2.0 --promote-only
assert_eq "--promote-only exits 0" "0" "$RC_RC"
if git -C "$S8O" rev-parse -q --verify refs/tags/v0.2.0 >/dev/null; then pass "--promote-only creates stable tag v0.2.0"; else fail "--promote-only creates stable tag v0.2.0"; fi
assert_eq "--promote-only tag chart is stable 0.2.0" "0.2.0" "$(git -C "$S8O" show v0.2.0:deploy/chart/Chart.yaml 2>/dev/null | awk '/^version:/{print $2;exit}')"
assert_eq "--promote-only stable sits directly on the RC commit" "$(git -C "$S8O" rev-parse 'v0.2.0-rc.1^{commit}')" "$(git -C "$S8O" rev-parse -q --verify 'v0.2.0^{commit}^' 2>/dev/null)"
if git -C "$S8O" cat-file -e v0.2.0:api/feature_301.go 2>/dev/null; then fail "--promote-only stable excludes main's later shipping work"; else pass "--promote-only stable excludes main's later shipping work"; fi
assert_eq "--promote-only leaves main's HEAD untouched"   "$head_before"  "$(git -C "$S8O" rev-parse HEAD)"
assert_eq "--promote-only leaves main's chart untouched"  "$chart_before" "$(chart_ver "$S8O")"
if has_heading "$S8O" 0.3.0; then fail "--promote-only opens NO next section"; else pass "--promote-only opens NO next section"; fi
if git -C "$S8O" rev-parse -q --verify refs/heads/release/0.2.0 >/dev/null; then fail "--promote-only leaves no release/0.2.0 branch"; else pass "--promote-only leaves no release/0.2.0 branch"; fi
assert_contains "--promote-only names what the stable leaves out" "1 shipping commit" "$RC_OUT"
# The follow-on: with the base promoted no RC is in flight, so the next plain cut is an
# ordinary rc.1 whose coverage window starts at the new stable (the oracle runs inside it).
run_rc "$S8O" 0.3.0
assert_eq "after --promote-only, the next plain cut exits 0"      "0"          "$RC_RC"
assert_eq "after --promote-only, the next plain cut is 0.3.0-rc.1" "0.3.0-rc.1" "$(chart_ver "$S8O")"
# ...and that cut reconciles main's [0.2.0] heading with the stable tag's copy: the promotion
# dated the section on the release branch only, main still carried the RC-cut date (D3).
heading_of() { awk -v h="## [$1]" 'index($0, h) == 1 { print; exit }'; }   # stdin -> the `## [X]` heading line
tag_heading="$(git -C "$S8O" show v0.2.0:CHANGELOG.md | heading_of 0.2.0)"
if [ "$tag_heading" != "## [0.2.0] - 2020-01-01" ] && [ -n "$tag_heading" ]; then pass "promotion dated the stable tag's [0.2.0] heading"; else fail "promotion dated the stable tag's [0.2.0] heading (got '$tag_heading')"; fi
assert_eq "next cut syncs main's [0.2.0] heading to the stable tag's copy" "$tag_heading" "$(heading_of 0.2.0 < "$S8O/CHANGELOG.md")"

echo "=== M2b: --promote-only refusals ==="
# It must NAME the in-flight base (the stable being created), never a next version; it takes
# no main-half options; it keeps --promote's published-RC guard; it needs an RC in flight.
S8R="$(mktemp -d)"; seed_repo "$S8R"; add_origin "$S8R"; add_feature "$S8R" 201
put_changelog "$S8R" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S8R" 0.2.0; git -C "$S8R" tag v0.2.0-rc.1
run_rc "$S8R" 0.2.0 --promote-only
assert_eq "--promote-only refused on a local-only RC exits 3" "3" "$RC_RC"
assert_contains "--promote-only local-only refusal names the cause" "not on origin" "$RC_OUT"
if git -C "$S8R" rev-parse -q --verify refs/tags/v0.2.0 >/dev/null; then fail "refused --promote-only (local-only RC) leaves NO v0.2.0 tag"; else pass "refused --promote-only (local-only RC) leaves NO v0.2.0 tag"; fi
push_tag_to_origin "$S8R" v0.2.0-rc.1
run_rc "$S8R" 0.3.0 --promote-only
assert_eq "--promote-only with a version other than the in-flight base exits 3" "3" "$RC_RC"
assert_contains "--promote-only wrong-version refusal names the base" "in-flight base 0.2.0" "$RC_OUT"
# A VALID draft and an EXISTING --prev-tag, so the generic "file not found" / "tag does not
# exist" preconditions cannot satisfy these; the specific refusal text must.
printf '## [0.2.0] - 2026-10-02\n### Added\n- **Feature 201** (#201)\n' > "$S8R.draft.md"
run_rc "$S8R" 0.2.0 --promote-only --changelog-file "$S8R.draft.md"
assert_eq "--promote-only with --changelog-file exits 3" "3" "$RC_RC"
assert_contains "--promote-only --changelog-file refusal is the specific one" "--promote-only takes no" "$RC_OUT"
run_rc "$S8R" 0.2.0 --promote-only --no-commit
assert_eq "--promote-only with --no-commit exits 3" "3" "$RC_RC"
assert_contains "--promote-only --no-commit refusal is the specific one" "--promote-only takes no" "$RC_OUT"
run_rc "$S8R" 0.2.0 --promote-only --prev-tag v0.1.0
assert_eq "--promote-only with --prev-tag exits 3" "3" "$RC_RC"
assert_contains "--promote-only --prev-tag refusal is the specific one" "--promote-only takes no" "$RC_OUT"
if git -C "$S8R" rev-parse -q --verify refs/tags/v0.2.0 >/dev/null; then fail "refused --promote-only leaves NO v0.2.0 tag"; else pass "refused --promote-only leaves NO v0.2.0 tag"; fi
S8N="$(mktemp -d)"; seed_repo "$S8N"
run_rc "$S8N" 0.2.0 --promote-only
assert_eq "--promote-only with no RC in flight exits 3" "3" "$RC_RC"
assert_contains "--promote-only with no RC in flight says so" "no RC is in flight" "$RC_OUT"

echo "=== M2b: an ABANDONED lower RC is not resurrected once a promote cuts no next candidate ==="
# v0.2.0-rc.1 is abandoned (--skip-promote), v0.3.0-rc.1 is promoted alone. No candidate is
# left to mask the abandoned base, so it used to come back as "in flight" and the next plain
# cut refused (exit 3) instead of cutting v0.4.0-rc.1. A base at or below the highest stable
# is never in flight.
S8A="$(mktemp -d)"; seed_repo "$S8A"; add_origin "$S8A"; add_feature "$S8A" 201
put_changelog "$S8A" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S8A" 0.2.0; git -C "$S8A" tag v0.2.0-rc.1; push_tag_to_origin "$S8A" v0.2.0-rc.1
add_feature "$S8A" 301
put_changelog "$S8A" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 301** (#301)

## [0.2.0] - 2026-10-01
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S8A" 0.3.0 --skip-promote
assert_eq "abandon v0.2.0-rc.1 (--skip-promote) exits 0" "0" "$RC_RC"
git -C "$S8A" tag v0.3.0-rc.1; push_tag_to_origin "$S8A" v0.3.0-rc.1
run_rc "$S8A" 0.3.0 --promote-only
assert_eq "--promote-only of v0.3.0-rc.1 (abandoned v0.2.0-rc.1 below it) exits 0" "0" "$RC_RC"
add_feature "$S8A" 401
awk '/^## \[Unreleased\]/ { print; print "### Added"; print "- **Feature 401** (#401)"; print ""; next } { print }' "$S8A/CHANGELOG.md" > "$S8A/CHANGELOG.new" \
  && mv "$S8A/CHANGELOG.new" "$S8A/CHANGELOG.md" && git -C "$S8A" add CHANGELOG.md && gcommit "$S8A" "docs: changelog"
run_rc "$S8A" 0.4.0
assert_eq "next plain cut ignores the abandoned v0.2.0-rc.1 and exits 0" "0" "$RC_RC"
assert_eq "next plain cut is 0.4.0-rc.1"                               "0.4.0-rc.1" "$(chart_ver "$S8A")"

echo "=== M2: a local main BEHIND origin/main is refused before anything is cut ==="
# release-cut reads the LOCAL HEAD: a stale main changes --promote's promote-only decision
# (fewer shipping commits since the RC) and cuts from a base whose release commit can never
# fast-forward onto origin. Refuse up front instead of failing at the push.
SBH="$(mktemp -d)"; seed_repo "$SBH"; add_origin "$SBH"; add_feature "$SBH" 201
put_changelog "$SBH" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
add_feature "$SBH" 202
git -C "$SBH" push -q origin main
git -C "$SBH" reset -q --hard HEAD~1          # local main is now 1 behind origin/main
run_rc "$SBH" 0.2.0
assert_eq "a main behind origin/main exits 3" "3" "$RC_RC"
assert_contains "the behind refusal names the cause" "behind origin/main" "$RC_OUT"
assert_eq "a refused stale cut leaves the chart untouched" "0.1.0" "$(chart_ver "$SBH")"
git -C "$SBH" merge -q --ff-only origin/main
put_changelog "$SBH" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)
- **Feature 202** (#202)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$SBH" 0.2.0
assert_eq "after the fast-forward the same cut exits 0" "0" "$RC_RC"

echo "=== M2b: --promote REFUSED when the in-flight RC is local-only (not on origin) ==="
# The 0.83.0-rc.7 guard: promotion re-tags the RC's PUBLISHED agent image, so an RC that
# was never pushed (never built by release.yml) must not promote. Same setup as S8 but the
# RC tag is deliberately NOT pushed to origin.
S8B="$(mktemp -d)"; seed_repo "$S8B"; add_origin "$S8B"; add_feature "$S8B" 201
put_changelog "$S8B" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S8B" 0.2.0; git -C "$S8B" tag v0.2.0-rc.1   # LOCAL only — deliberately NOT pushed
add_feature "$S8B" 301
put_changelog "$S8B" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 301** (#301)

## [0.2.0] - 2026-10-01
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S8B" 0.3.0 --promote
assert_eq "--promote refused on a local-only RC exits 3" "3" "$RC_RC"
assert_contains "refusal says the RC is not on origin" "not on origin" "$RC_OUT"
if git -C "$S8B" rev-parse -q --verify refs/tags/v0.2.0 >/dev/null; then fail "refused promote leaves NO v0.2.0 tag"; else pass "refused promote leaves NO v0.2.0 tag"; fi

echo "=== M2b: --promote REFUSED with a DISTINCT message when origin is unreachable ==="
# Finding 2: a network/auth failure must not be reported as an unpushed tag. Point origin
# at a path that does not exist so `git ls-remote` errors (not "absent").
S8C="$(mktemp -d)"; seed_repo "$S8C"; git -C "$S8C" remote add origin "$S8C.noexist.git"; add_feature "$S8C" 201
put_changelog "$S8C" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S8C" 0.2.0; git -C "$S8C" tag v0.2.0-rc.1
add_feature "$S8C" 301
put_changelog "$S8C" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 301** (#301)

## [0.2.0] - 2026-10-01
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S8C" 0.3.0 --promote
assert_eq "--promote refused when origin unreachable exits 3" "3" "$RC_RC"
assert_contains "refusal distinguishes an unreachable origin" "could not reach origin" "$RC_OUT"

echo "=== M2b: --promote allowlist abort ==="
S9="$(mktemp -d)"; seed_repo "$S9"; add_origin "$S9"; add_feature "$S9" 201
# Replace worker-tag-autobump.sh with a version-conditional stub and COMMIT it, so it
# rides into the v0.2.0-rc.1 promote worktree (a stub written after the RC tag would not
# exist there, and an uncommitted one would trip release-cut's clean-tree precondition
# before promote_inflight — CR review of PR #1322). When promote_inflight runs it for the
# stable base (arg 0.2.0) it creates AND stages a non-allowlisted file, so the promote
# commit's diff leaves the allowlist and the guard must abort BEFORE any tag exists. The
# bare-base condition leaves the first RC cut (which calls it with 0.2.0-rc.1) unaffected.
cat > "$S9/scripts/worker-tag-autobump.sh" <<'SH'
#!/usr/bin/env bash
if [ "${1#v}" = "0.2.0" ]; then
  echo 'package main // injected' > api/injected.go
  git add api/injected.go
fi
exit 0
SH
chmod +x "$S9/scripts/worker-tag-autobump.sh"
git -C "$S9" add scripts/worker-tag-autobump.sh; gcommit "$S9" "test: version-conditional injecting autobump stub"
put_changelog "$S9" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S9" 0.2.0; git -C "$S9" tag v0.2.0-rc.1; push_tag_to_origin "$S9" v0.2.0-rc.1
add_feature "$S9" 301
put_changelog "$S9" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 301** (#301)

## [0.2.0] - 2026-10-01
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S9" 0.3.0 --promote
if [ "$RC_RC" -ne 0 ]; then pass "--promote aborts on a non-allowlisted change"; else fail "--promote aborts on a non-allowlisted change"; fi
assert_contains "abort names the non-allowlisted file" "non-allowlisted file:" "$RC_OUT"
if git -C "$S9" rev-parse -q --verify refs/tags/v0.2.0 >/dev/null; then fail "aborted promote leaves NO v0.2.0 tag"; else pass "aborted promote leaves NO v0.2.0 tag"; fi
if git -C "$S9" rev-parse -q --verify refs/heads/release/0.2.0 >/dev/null; then fail "aborted promote leaves NO release/0.2.0 branch"; else pass "aborted promote leaves NO release/0.2.0 branch"; fi

echo "=== M2c: worker-tag-autobump — a pin naming a MISSING tag is repinned, never failed open ==="
# The 0.83.0-rc.7 forward trap: after the local-only rc.7 tag was deleted, the pin named a
# tag that no longer existed and the old autobump FAILED OPEN (left the dead pin, shipped
# it again). seed_repo copies the REAL worker-tag-autobump.sh + assert into scripts/ and
# tags v0.1.0; repin workers.image.tag to a tag that does NOT exist to hit that branch.
SA="$(mktemp -d)"; seed_repo "$SA"
cat > "$SA/deploy/chart/values.yaml" <<'YAML'
workers:
  image:
    repository: ghcr.io/x/worker
    tag: "0.9.9-rc.99"
YAML
git -C "$SA" add deploy/chart/values.yaml; gcommit "$SA" "test: pin a nonexistent worker tag"
( cd "$SA" && bash scripts/worker-tag-autobump.sh --check 0.2.0 >/dev/null 2>&1 ); ac_rc=$?
if [ "$ac_rc" -ne 0 ]; then pass "autobump --check flags a missing pin tag (exit $ac_rc)"; else fail "autobump --check flags a missing pin tag (got exit 0 — failed open)"; fi
( cd "$SA" && bash scripts/worker-tag-autobump.sh 0.2.0 >/dev/null 2>&1 ); ab_rc=$?
assert_eq "autobump edit on a missing pin tag exits 0"          "0"     "$ab_rc"
assert_eq "autobump repins the dead pin to the version cut"     "0.2.0" "$(pin_tag "$SA")"
assert_eq "autobump keeps PINNED_TAG in lockstep"               "0.2.0" "$(awk -F'"' '/^PINNED_TAG=/{print $2;exit}' "$SA/scripts/assert-worker-tag-decoupled.sh")"

echo "=== M2c: worker-tag-autobump — a VALID pin tag + unchanged surface still leaves the pin ==="
# The regression guard for guard 3: a present OLD tag must behave exactly as before.
SB="$(mktemp -d)"; seed_repo "$SB"   # pin 0.1.0, tag v0.1.0 present, agent surface unchanged
( cd "$SB" && bash scripts/worker-tag-autobump.sh 0.2.0 >/dev/null 2>&1 ); sb_rc=$?
assert_eq "autobump with a valid tag + unchanged surface exits 0" "0"     "$sb_rc"
assert_eq "autobump leaves the pin when the surface is unchanged"  "0.1.0" "$(pin_tag "$SB")"

echo "=== M2c: worker-tag-autobump — reads/bumps workers.image.tag past a deeper decoy tag ==="
# The pin reader/bumper (shared shape with release.yml's guard-1 job and release-verify's
# guard-4 check) must target the two-space workers.image.tag, not a deeper decoy.
SC="$(mktemp -d)"; seed_repo "$SC"
cat > "$SC/deploy/chart/values.yaml" <<'YAML'
workers:
  controller:
    image:
      repository: ghcr.io/x/controller
      tag: "9.9.9-decoy"
  image:
    repository: ghcr.io/x/worker
    tag: "0.1.0"
YAML
mkdir -p "$SC/agent/src"; echo 'export const x = 1;' > "$SC/agent/src/index.ts"
git -C "$SC" add -A; gcommit "$SC" "test: decoy nested tag + an agent-surface change"
( cd "$SC" && bash scripts/worker-tag-autobump.sh 0.2.0 >/dev/null 2>&1 ); sc_rc=$?
assert_eq "autobump (decoy) exits 0"                       "0"     "$sc_rc"
assert_eq "autobump bumps workers.image.tag, not the decoy" "0.2.0" "$(pin_tag "$SC")"
if grep -q '9.9.9-decoy' "$SC/deploy/chart/values.yaml"; then pass "autobump leaves the deeper controller decoy tag untouched"; else fail "autobump leaves the deeper controller decoy tag untouched"; fi

echo "=== M2c: worker-tag-autobump — a LOCAL-ONLY pin tag (origin reachable) is treated as unpublished ==="
# Finding 1: git rev-parse passes for a local-only tag, so without the origin check a pin
# naming an unpushed tag would diff clean and ship again. Origin here is reachable and has
# the baseline, but the pinned tag was never pushed.
SD="$(mktemp -d)"; seed_repo "$SD"; add_origin "$SD"
git -C "$SD" tag v0.5.0-rc.1                          # a LOCAL-only tag, never pushed to origin
cat > "$SD/deploy/chart/values.yaml" <<'YAML'
workers:
  image:
    repository: ghcr.io/x/worker
    tag: "0.5.0-rc.1"
YAML
git -C "$SD" add deploy/chart/values.yaml; gcommit "$SD" "test: pin a local-only tag"
( cd "$SD" && bash scripts/worker-tag-autobump.sh --check 0.6.0 >/dev/null 2>&1 ); sd_ac=$?
if [ "$sd_ac" -ne 0 ]; then pass "autobump --check flags a local-only pin tag (exit $sd_ac)"; else fail "autobump --check flags a local-only pin tag (got exit 0 — trusted the local tag)"; fi
( cd "$SD" && bash scripts/worker-tag-autobump.sh 0.6.0 >/dev/null 2>&1 ); sd_ab=$?
assert_eq "autobump edit on a local-only pin tag exits 0"        "0"     "$sd_ab"
assert_eq "autobump repins a local-only pin tag to the version cut" "0.6.0" "$(pin_tag "$SD")"

echo "=== M2c: worker-tag-autobump — a pin tag ON origin but missing locally is a broken instrument (exit 2) ==="
# The inverse of local-only: present on origin, absent from this clone (a standalone run
# with no tag fetch). It must NOT repin (wrong "not published" message + unneeded roll);
# it exits 2 and tells the operator to fetch.
SE="$(mktemp -d)"; seed_repo "$SE"; add_origin "$SE"
git -C "$SE" tag v0.7.0-rc.1; push_tag_to_origin "$SE" v0.7.0-rc.1   # on origin AND local
git -C "$SE" tag -d v0.7.0-rc.1 >/dev/null                            # delete locally; still on origin
cat > "$SE/deploy/chart/values.yaml" <<'YAML'
workers:
  image:
    repository: ghcr.io/x/worker
    tag: "0.7.0-rc.1"
YAML
git -C "$SE" add deploy/chart/values.yaml; gcommit "$SE" "test: pin a tag on origin but not local"
( cd "$SE" && bash scripts/worker-tag-autobump.sh 0.8.0 >/dev/null 2>&1 ); se_rc=$?
assert_eq "autobump exits 2 when the pin tag is on origin but not local" "2"          "$se_rc"
assert_eq "autobump does NOT repin on the broken-instrument path"        "0.7.0-rc.1" "$(pin_tag "$SE")"

echo "=== M2: release-cut cites dependency merges the per-PR check spared ==="
# add_dep <dir> <path> <subject>: a dependency-style merge touching only <path>.
add_dep() {
  mkdir -p "$(dirname "$1/$2")"
  echo "// bump $3" >> "$1/$2"
  git -C "$1" add -A
  gcommit "$1" "$3"
}
dep_bullets() { grep -c '^- \*\*Routine dependency updates' "$1/CHANGELOG.md" || true; }
dep_bullet()  { grep '^- \*\*Routine dependency updates' "$1/CHANGELOG.md" || true; }

# A Renovate-style go.mod bump alongside an entry-carrying feature: cited automatically.
SDA="$(mktemp -d)"; seed_repo "$SDA"; add_feature "$SDA" 201
add_dep "$SDA" api/go.mod "fix(deps): update module example.com/x to v1.2.0 (#300)"
feature_201_changelog "$SDA"
run_rc "$SDA" 0.2.0
assert_eq "dependency merge: first RC exits 0 (oracle satisfied)" "0" "$RC_RC"
assert_eq "dependency merge: one Routine bullet" "1" "$(dep_bullets "$SDA")"
assert_contains "dependency merge: bullet cites #300" "#300" "$(dep_bullet "$SDA")"
assert_eq "dependency merge: bullet sits under [0.2.0] ### Changed" "### Changed" \
  "$(awk '/^## \[0\.2\.0\]/{s=1;next} s&&/^## \[/{exit} s&&/^### /{h=$0} s&&/^- \*\*Routine dependency updates/{print h; exit}' "$SDA/CHANGELOG.md")"

# The next candidate adds a Dockerfile digest bump: the shipped bullet stays frozen.
git -C "$SDA" tag v0.2.0-rc.1
add_dep "$SDA" agent/src/Dockerfile "chore(deps): update node digest to abc123 (#302)"
run_rc "$SDA" 0.2.0
assert_eq "next RC with a new dependency merge exits 0" "0" "$RC_RC"
assert_eq "next RC appends a fresh bullet instead of editing the shipped one" "2" "$(dep_bullets "$SDA")"
assert_contains "original bullet still cites #300" "#300" "$(dep_bullet "$SDA")"
assert_contains "new bullet cites #302" "#302" "$(dep_bullet "$SDA")"
# Exercise the release-cut hook itself, not only the standalone block checker.
git -C "$SDA" tag v0.2.0-rc.2
frozen_head="$(git -C "$SDA" rev-parse HEAD)"
awk '{sub(/Dependency manifest and image bumps/, "Edited dependency work"); print}' "$SDA/CHANGELOG.md" > "$SDA/changed.md"
mv "$SDA/changed.md" "$SDA/CHANGELOG.md"
git -C "$SDA" add CHANGELOG.md
gcommit "$SDA" "docs: edit a shipped bullet"
before_rejected_cut="$(git -C "$SDA" rev-parse HEAD)"
run_rc "$SDA" 0.2.0
assert_eq "release cut rejects an edited shipped dependency block" "3" "$RC_RC"
assert_contains "release cut names the frozen block failure" "frozen published RC bullet" "$RC_OUT"
assert_eq "rejected frozen-block cut makes no commit" "$before_rejected_cut" "$(git -C "$SDA" rev-parse HEAD)"
git -C "$SDA" reset -q --hard "$frozen_head"

# A dependency-only window: [Unreleased] is empty, yet the cut has something to release.
SDB="$(mktemp -d)"; seed_repo "$SDB"
add_dep "$SDB" api/go.sum "build(deps)!: bump example.com/y (#310)"
run_rc "$SDB" 0.2.0
assert_eq "dependency-only window with empty [Unreleased] exits 0" "0" "$RC_RC"
assert_contains "dependency-only window cites #310" "#310" "$(dep_bullet "$SDB")"

# No PR number in the subject: the short SHA is the citation the oracle accepts.
SDC="$(mktemp -d)"; seed_repo "$SDC"; add_feature "$SDC" 201
add_dep "$SDC" api/devbox.lock "chore(deps): lock file maintenance"
sdc_short="$(git -C "$SDC" rev-parse --short HEAD)"
feature_201_changelog "$SDC"
run_rc "$SDC" 0.2.0
assert_eq "dependency merge without a PR number exits 0" "0" "$RC_RC"
assert_contains "dependency merge without a PR number is cited by short SHA" "$sdc_short" "$(dep_bullet "$SDC")"

# Already cited by hand: no bullet, nothing duplicated.
SDD="$(mktemp -d)"; seed_repo "$SDD"
add_dep "$SDD" api/go.mod "fix(deps): update module example.com/x to v1.2.0 (#300)"
printf '# Changelog\n\n## [Unreleased]\n### Changed\n- **Bumped x** (#300)\n\n## [0.1.0] - 2026-09-01\n### Added\n- **Initial** (#100)\n' | put_changelog "$SDD"
run_rc "$SDD" 0.2.0
assert_eq "hand-cited dependency merge exits 0" "0" "$RC_RC"
assert_eq "hand-cited dependency merge adds no bullet" "0" "$(dep_bullets "$SDD")"

# Already cited by short SHA: the PR number is the same merge, so no bullet either.
SDI="$(mktemp -d)"; seed_repo "$SDI"
add_dep "$SDI" api/go.mod "fix(deps): update module example.com/x to v1.2.0 (#300)"
sdi_short="$(git -C "$SDI" rev-parse --short HEAD)"
printf '# Changelog\n\n## [Unreleased]\n### Changed\n- **Bumped x** (%s)\n\n## [0.1.0] - 2026-09-01\n### Added\n- **Initial** (#100)\n' "$sdi_short" | put_changelog "$SDI"
run_rc "$SDI" 0.2.0
assert_eq "SHA-cited dependency merge exits 0" "0" "$RC_RC"
assert_eq "SHA-cited dependency merge is not re-cited by its PR number" "0" "$(dep_bullets "$SDI")"

# Not spared by the per-PR check, so not auto-cited: the oracle still refuses.
SDE="$(mktemp -d)"; seed_repo "$SDE"; add_feature "$SDE" 201
add_dep "$SDE" api/go.mod "fix(deps): update module example.com/x to v1.2.0 (#300)"
add_feature "$SDE" 320    # an ordinary shipping merge with no entry
feature_201_changelog "$SDE"
run_rc "$SDE" 0.2.0
assert_eq "uncited ordinary merge still fails the cut" "1" "$RC_RC"
assert_contains "oracle names the uncited ordinary merge" "Feature 320 (#320)" "$RC_OUT"
SDF="$(mktemp -d)"; seed_repo "$SDF"; add_feature "$SDF" 201
add_dep "$SDF" deploy/chart/Dockerfile "chore(deps): update chart image (#330)"
feature_201_changelog "$SDF"
run_rc "$SDF" 0.2.0
assert_eq "a chart path is never auto-cited (cut fails)" "1" "$RC_RC"
assert_contains "oracle names the chart dependency merge" "(#330)" "$RC_OUT"
SDG="$(mktemp -d)"; seed_repo "$SDG"; add_feature "$SDG" 201
add_dep "$SDG" api/go.mod "fix: hand-edited go.mod (#340)"
feature_201_changelog "$SDG"
run_rc "$SDG" 0.2.0
assert_eq "a manifest change under a non-dependency subject is not auto-cited (cut fails)" "1" "$RC_RC"
SDH="$(mktemp -d)"; seed_repo "$SDH"; add_feature "$SDH" 201
mkdir -p "$SDH/api"; echo 'package main // x' > "$SDH/api/x.go"; echo '// bump' >> "$SDH/api/go.mod"
git -C "$SDH" add -A; gcommit "$SDH" "fix(deps): bump x and adapt (#350)"
feature_201_changelog "$SDH"
run_rc "$SDH" 0.2.0
assert_eq "a dependency merge that also changes source is not auto-cited (cut fails)" "1" "$RC_RC"
# An agent npm bump (manifest + lockfile, Renovate-typed) is cited in bulk too; the Codex
# runtime lock is not a dependency manifest, so an uncited Codex bump still fails the cut.
SDJ="$(mktemp -d)"; seed_repo "$SDJ"; add_feature "$SDJ" 201
mkdir -p "$SDJ/agent"; echo '{"dependencies":{"y":"1"}}' > "$SDJ/agent/package.json"; echo '{"y":1}' > "$SDJ/agent/package-lock.json"
git -C "$SDJ" add -A; gcommit "$SDJ" "fix(deps): update dependency y to v1 (#360)"
feature_201_changelog "$SDJ"
run_rc "$SDJ" 0.2.0
assert_eq "agent npm dependency merge exits 0" "0" "$RC_RC"
assert_contains "agent npm dependency merge is auto-cited" "#360" "$(dep_bullet "$SDJ")"
SDK="$(mktemp -d)"; seed_repo "$SDK"; add_feature "$SDK" 201
add_dep "$SDK" agent/codex/codex-package.lock "chore(deps): update codex runtime (#370)"
feature_201_changelog "$SDK"
run_rc "$SDK" 0.2.0
assert_eq "a Codex runtime lock bump is not auto-cited (cut fails)" "1" "$RC_RC"
assert_contains "oracle names the Codex runtime merge" "(#370)" "$RC_OUT"
rm -rf "$SDA" "$SDB" "$SDC" "$SDD" "$SDE" "$SDF" "$SDG" "$SDH" "$SDI" "$SDJ" "$SDK"

rm -rf "$S1" "$S3" "$S4" "$S6" "$S7" "$S8" "$S8P" "$S8O" "$S8R" "$S8N" "$S8A" "$S8B" "$S8C" "$S9" "$SA" "$SB" "$SC" "$SD" "$SE" \
       "$S8.origin.git" "$S8P.origin.git" "$S8O.origin.git" "$S8R.origin.git" "$S8A.origin.git" "$S8B.origin.git" "$S9.origin.git" "$SD.origin.git" "$SE.origin.git" \
       "$S8R.draft.md" "$SBH" "$SBH.origin.git"

echo "=== #1468 M1: NUL paths and checked listing producers ==="
LIST_SHIM="$(mktemp -d)"
export LIST_REAL_GIT
LIST_REAL_GIT="$(command -v git)"
cat > "$LIST_SHIM/git" <<'STUB'
#!/usr/bin/env bash
# Record stable-tag creation attempts; delegate all non-listing operations unchanged.
if [ "${1:-}" = tag ] && [ "${2:-}" = -a ] && [ "${3:-}" = v0.2.0 ] && [ -n "${PROMOTE_ATTEMPT_LOG:-}" ]; then
  printf 'v0.2.0\n' >> "$PROMOTE_ATTEMPT_LOG"
fi
# Intercept only path listings for this fixture's selected commit.
listing=0
case "${1:-}" in
  diff|show)
    for arg in "$@"; do
      [ "$arg" != --name-only ] || listing=1
    done
    ;;
esac
if [ "$listing" = 1 ] && [ "${!#}" = "${LIST_SHA:-}" ]; then
  case "${LIST_MODE:-}" in
    fallback) [ "$1" != diff ] || exit 71 ;;
    fallback_prefix)
      if [ "$1" = diff ]; then printf 'prds/'; exit 71; fi
      ;;
    fallback_changelog)
      if [ "$1" = diff ]; then printf 'CHANGELOG.md\0'; exit 71; fi
      ;;
    fail) exit 72 ;;
    partial)
      printf 'api/go.mod\0api/é.go\0'
      exit 73
      ;;
    large)
      # 20,000 records exceed pipe capacity; success requires draining all of them.
      printf 'api/é.go\0'
      for ((i=0; i<20000; i++)); do
        printf 'prds/nonshipping-record-%08d-padding-padding-padding.md\0' "$i" || exit 74
      done
      printf 'drained\n' >> "$LIST_LOG"
      exit 0
      ;;
  esac
fi
exec "$LIST_REAL_GIT" "$@"
STUB
chmod +x "$LIST_SHIM/git"
export PATH="$LIST_SHIM:$PATH"
export LIST_SHA LIST_MODE LIST_LOG
listing_failure() {
  if [ "$2" -ne 0 ]; then pass "$1 refuses"; else fail "$1 refuses"; fi
  assert_contains "$1 names SHA" "$LIST_SHA" "$3"
  assert_contains "$1 explains listing failure" "list" "$3"
}
unusual_feature() {
  local d="$1" path="$2"
  printf 'package main\n' > "$d/$path"
  git -C "$d" add -- "$path"
  gcommit "$d" 'feat: unusual path (#1468)'
}
# Snapshot every tracked file, plus index/worktree status, to prove refusal is inert.
promote_snapshot() {
  ( cd "$1" && git ls-files -z | xargs -0 cksum && git status --porcelain )
}
no_promote_attempt() {
  assert_eq "$1 never attempts a stable tag" "" "$(cat "$PROMOTE_ATTEMPT_LOG")"
  if git -C "$2" rev-parse -q --verify refs/tags/v0.2.0 >/dev/null; then
    fail "$1 leaves stable tag absent"
  else
    pass "$1 leaves stable tag absent"
  fi
}
assert_absent() {
  case "$3" in
    *"$2"*) fail "$1 (unexpected '$2')" ;;
    *) pass "$1" ;;
  esac
}
echo "=== #1468 M2: empty-body promotion preflight ==="
export PROMOTE_ATTEMPT_LOG
for history in feature docs dependency; do
  S10="$(mktemp -d)"; published_rc "$S10"
  case "$history" in
    feature) add_feature "$S10" 301 ;;
    docs)
      mkdir -p "$S10/docs"
      printf 'CLI shipping documentation\n' > "$S10/docs/cli.md"
      git -C "$S10" add -- docs/cli.md
      gcommit "$S10" 'docs(cli): clarify usage (#1468)'
      ;;
    dependency) add_dep "$S10" api/go.mod 'fix(deps): bump x (#1468)' ;;
  esac
  shipping_sha="$(git -C "$S10" rev-parse HEAD)"
  before="$(promote_snapshot "$S10")"
  PROMOTE_ATTEMPT_LOG="$S10.attempt.log"; : > "$PROMOTE_ATTEMPT_LOG"
  run_rc "$S10" 0.3.0 --promote
  assert_eq "$history empty-body promote exits 3" 3 "$RC_RC"
  assert_contains "$history refusal names shipping SHA" "$shipping_sha" "$RC_OUT"
  assert_contains "$history refusal offers next candidate entries" 'next-candidate UUID-v4 fragments' "$RC_OUT"
  assert_contains "$history refusal offers fragment authoring" 'under changelog.d/ and commit them' "$RC_OUT"
  assert_contains "$history refusal offers draft" '--changelog-file' "$RC_OUT"
  assert_absent "$history refusal emits no promotion" 'promoted:' "$RC_OUT"
  assert_absent "$history refusal emits no rollback" 'rolled back' "$RC_OUT"
  assert_eq "$history refusal keeps HEAD" "$shipping_sha" "$(git -C "$S10" rev-parse HEAD)"
  assert_eq "$history refusal keeps files and index" "$before" "$(promote_snapshot "$S10")"
  no_promote_attempt "$history refusal" "$S10"
  case "$history" in
    feature)
      # Supplying a draft bypasses preflight, but later validation must still roll back.
      printf '## [0.4.0]\n### Added\n- Wrong base (#301)\n' > "$S10.draft.md"
      : > "$PROMOTE_ATTEMPT_LOG"
      run_rc "$S10" 0.3.0 --promote --changelog-file "$S10.draft.md"
      assert_eq "bad supplied draft fails later" 3 "$RC_RC"
      assert_eq "bad draft attempted stable tag" v0.2.0 "$(cat "$PROMOTE_ATTEMPT_LOG")"
      assert_contains "bad draft rolls back stable tag" 'rolled back the local promote tag v0.2.0' "$RC_OUT"
      if git -C "$S10" rev-parse -q --verify refs/tags/v0.2.0 >/dev/null; then
        fail "bad draft leaves stable tag absent"
      else
        pass "bad draft leaves stable tag absent"
      fi
      ;;
    docs)
      : > "$PROMOTE_ATTEMPT_LOG"
      run_rc "$S10" 0.2.0 --promote-only
      assert_eq "docs history explicit promote-only succeeds" 0 "$RC_RC"
      assert_eq "explicit promote-only keeps HEAD" "$shipping_sha" "$(git -C "$S10" rev-parse HEAD)"
      assert_eq "explicit promote-only keeps files and index" "$before" "$(promote_snapshot "$S10")"
      assert_eq "explicit promote-only attempts stable tag once" v0.2.0 "$(cat "$PROMOTE_ATTEMPT_LOG")"
      ;;
    dependency)
      # Use the same post-RC dependency history in an independent plain-cut fixture.
      SDP="$(mktemp -d)"; published_rc "$SDP"
      add_dep "$SDP" api/go.mod 'fix(deps): bump x (#1468)'
      PROMOTE_ATTEMPT_LOG="$SDP.attempt.log"; : > "$PROMOTE_ATTEMPT_LOG"
      run_rc "$SDP" 0.2.0
      assert_eq "paired plain dependency-only cut succeeds" 0 "$RC_RC"
      assert_contains "paired plain cut autocites dependency" '#1468' "$(dep_bullet "$SDP")"
      no_promote_attempt "paired plain cut" "$SDP"
      rm -rf "$SDP" "$SDP.origin.git" "$SDP.attempt.log"
      ;;
  esac
  rm -rf "$S10" "$S10.origin.git" "$S10.attempt.log" "$S10.draft.md"
done
# Empty Unreleased plus a correctly based draft may promote and cut the next candidate.
S10="$(mktemp -d)"; published_rc "$S10"
mkdir -p "$S10/docs"
printf 'CLI shipping documentation\n' > "$S10/docs/cli.md"
git -C "$S10" add -- docs/cli.md
gcommit "$S10" 'docs(cli): clarify usage (#1468)'
printf '## [0.3.0]\n### Changed\n- CLI usage (#1468)\n' > "$S10.draft.md"
PROMOTE_ATTEMPT_LOG="$S10.attempt.log"; : > "$PROMOTE_ATTEMPT_LOG"
run_rc "$S10" 0.3.0 --promote --changelog-file "$S10.draft.md"
assert_eq "empty-body promote with valid draft succeeds" 0 "$RC_RC"
assert_eq "valid draft advances main chart" 0.3.0-rc.1 "$(chart_ver "$S10")"
assert_eq "valid draft attempts stable tag once" v0.2.0 "$(cat "$PROMOTE_ATTEMPT_LOG")"
assert_contains "valid draft preserves citation" 'CLI usage ([#1468](https://github.com/vtmocanu/uzi/pull/1468))' "$(cat "$S10/CHANGELOG.md")"
rm -rf "$S10" "$S10.origin.git" "$S10.attempt.log" "$S10.draft.md"
unset PROMOTE_ATTEMPT_LOG

# Each oracle path is the ONLY shipping path on its commit.
for unusual in 'api/é.go' $'api/new\n"quote.go'; do
  LN="$(mktemp -d)"; seed_repo "$LN"
  unusual_feature "$LN" "$unusual"
  printf '# Changelog\n\n## [0.2.0]\n### Added\n- Other (#201)\n' | put_changelog "$LN"
  out="$(cd "$LN" && bash "$ORACLE" HEAD v0.1.0 0.2.0 2>&1)"; status=$?
  assert_eq "oracle flags uncited unusual path $unusual" 1 "$status"
  assert_contains "oracle reports unusual commit" 'feat: unusual path (#1468)' "$out"
  rm -rf "$LN"
done
for consumer in oracle shipping dependency; do
  for mode in fallback fallback_prefix fallback_changelog fail partial large; do
    # Large output is a shipping-drain probe, not another failure mode.
    [ "$mode" != large ] || [ "$consumer" = shipping ] || continue
    LN="$(mktemp -d)"
    LIST_SHA=""; LIST_MODE=""
    if [ "$consumer" = shipping ]; then
      published_rc "$LN"
      unusual_feature "$LN" 'api/é.go'
      LIST_SHA="$(git -C "$LN" rev-parse HEAD)"
      # Empty Unreleased exposes a mistaken promote-only classification.
    else
      seed_repo "$LN"
      if [ "$consumer" = dependency ]; then
        add_dep "$LN" api/go.mod 'fix(deps): bump x (#1468)'
      else
        unusual_feature "$LN" 'api/é.go'
      fi
      LIST_SHA="$(git -C "$LN" rev-parse HEAD)"
      if [ "$consumer" = dependency ]; then
        feature_201_changelog "$LN"
      elif [ "$mode" = fallback_prefix ] || [ "$mode" = fallback_changelog ]; then
        # Successful show lists an uncited shipping path; failed diff cannot exempt it.
        printf '# Changelog\n\n## [0.2.0]\n### Added\n- Other (#201)\n' | put_changelog "$LN"
      else
        printf '# Changelog\n\n## [Unreleased]\n### Added\n- Other (#201)\n\n## [0.2.0]\n### Added\n- Cited (#1468)\n' | put_changelog "$LN"
      fi
    fi
    before="$(git -C "$LN" rev-parse HEAD)"
    before_reflog="$(git -C "$LN" reflog --format=%H HEAD)"
    LIST_MODE="$mode"; LIST_LOG="$LN.producer.log"
    if [ "$consumer" = oracle ]; then
      out="$(cd "$LN" && bash "$ORACLE" HEAD v0.1.0 0.2.0 2>&1)"; status=$?
    else
      if [ "$consumer" = shipping ]; then
        run_rc "$LN" 0.2.0 --promote-only
      else
        run_rc "$LN" 0.2.0
      fi
      out="$RC_OUT"; status="$RC_RC"
    fi
    case "$consumer:$mode" in
      oracle:fallback_prefix|oracle:fallback_changelog)
        assert_eq "oracle $mode refuses uncited shipping path" 1 "$status"
        assert_contains "oracle $mode reports the real shipping commit" 'feat: unusual path (#1468)' "$out"
        ;;
      *:fallback|*:fallback_prefix|*:fallback_changelog|*:large)
        assert_eq "$consumer $mode succeeds" 0 "$status"
        if [ "$consumer" = shipping ]; then
          assert_contains "shipping $mode counts the unusual commit once" '1 shipping commit(s)' "$out"
        fi
        if [ "$consumer" = dependency ]; then
          assert_contains "dependency $mode retains manifest autocitation" '#1468' "$(dep_bullet "$LN")"
        fi
        if [ "$mode" = large ]; then
          assert_eq "large producer completed without SIGPIPE" drained "$(cat "$LIST_LOG")"
        fi
        ;;
      *)
        listing_failure "$consumer $mode" "$status" "$out"
        assert_eq "$consumer $mode creates no release commit" "$before" "$(git -C "$LN" rev-parse HEAD)"
        if [ "$consumer" = dependency ]; then
          assert_eq "dependency $mode does not create then roll back a release commit" "$before_reflog" "$(git -C "$LN" reflog --format=%H HEAD)"
        fi
        if git -C "$LN" rev-parse -q --verify refs/tags/v0.2.0 >/dev/null; then
          fail "$consumer $mode creates no stable tag"
        else
          pass "$consumer $mode creates no stable tag"
        fi
        ;;
    esac
    LIST_SHA=""; LIST_MODE=""
    rm -rf "$LN" "$LN.origin.git" "$LN.producer.log"
  done
done
# Source + manifest in ONE dependency-typed commit must never gain an autocitation.
LN="$(mktemp -d)"; seed_repo "$LN"; add_feature "$LN" 201
printf '// bump\n' > "$LN/api/go.mod"
printf 'package main\n' > "$LN/api/é.go"
git -C "$LN" add -- api/go.mod api/é.go
gcommit "$LN" 'fix(deps): bump and adapt (#1468)'
feature_201_changelog "$LN"
run_rc "$LN" 0.2.0
assert_eq "Unicode source plus manifest is not autocited" 1 "$RC_RC"
assert_eq "Unicode source plus manifest has no Routine bullet" 0 "$(dep_bullets "$LN")"
rm -rf "$LN"
# With entries, Unicode work after a published RC reaches the next main-half cut.
LN="$(mktemp -d)"; published_rc "$LN"
unusual_feature "$LN" 'api/é.go'
printf '# Changelog\n\n## [Unreleased]\n### Added\n- Unicode (#1468)\n\n## [0.2.0]\n### Added\n- Feature (#201)\n\n## [0.1.0]\n- Initial (#100)\n' | put_changelog "$LN"
run_rc "$LN" 0.3.0 --promote
assert_eq "Unicode work with entries promotes and cuts main" 0 "$RC_RC"
assert_eq "Unicode work advances main chart" 0.3.0-rc.1 "$(chart_ver "$LN")"
export PATH="${PATH#"$LIST_SHIM:"}"
rm -rf "$LN" "$LN.origin.git" "$LIST_SHIM"
unset LIST_SHA LIST_MODE LIST_LOG LIST_REAL_GIT

fi # non-fragment fixture slices

echo "=== M2 fragments: consumption, isolation and recovery ==="
fragment_path=changelog.d/01234567-89ab-4cde-8fab-0123456789ab.md
fragment_second=changelog.d/11234567-89ab-4cde-8fab-0123456789ab.md
fragment_owned=(CHANGELOG.md deploy/chart/Chart.yaml deploy/chart/values.yaml scripts/assert-worker-tag-decoupled.sh "$fragment_path")
write_fragment() {
  mkdir -p "$1/changelog.d"
  printf '### Added\n\n- **Fragment %s (#%s).**\n  Preserved description.\n' "$2" "$2" > "$1/$fragment_path"
}
commit_fragment() {
  write_fragment "$1" "$2"
  git -C "$1" add -- "$fragment_path"
  gcommit "$1" "docs: fragment $2"
}
fragment_count() { awk -v n="$2" 'index($0, "- **Fragment " n " (") == 1 { nlines++ } END { print nlines+0 }' "$1/CHANGELOG.md"; }
assert_fragment_staged() {
  local d="$1" label="$2"
  assert_eq "$label deletion staged" "D" "$(git -C "$d" diff --cached --name-status -- "$fragment_path" | awk '{print $1}')"
  assert_eq "$label no unstaged edits" "" "$(git -C "$d" diff --name-only)"
  assert_eq "$label only owned paths staged" "" "$(git -C "$d" diff --cached --name-only | awk '$0 != "CHANGELOG.md" && $0 != "deploy/chart/Chart.yaml" && $0 != "deploy/chart/values.yaml" && $0 != "scripts/assert-worker-tag-decoupled.sh" && $0 != "changelog.d/01234567-89ab-4cde-8fab-0123456789ab.md"')"
}
# Every case owns an isolated repository; failures are tallied without blocking siblings.
for mode in rc stable no-commit commit-failure oracle-failure; do
  MF="$(mktemp -d "$REPO_ROOT/.uzi/scratch/m2.XXXXXX")"; seed_repo "$MF"; add_feature "$MF" 201; commit_fragment "$MF" 201
  case "$mode" in
    commit-failure)
      printf '#!/bin/sh\nexit 1\n' > "$MF/.git/hooks/pre-commit"; chmod +x "$MF/.git/hooks/pre-commit" ;;
    oracle-failure)
      # A valid fragment touch in the shipping first-parent commit is no exemption.
      write_fragment "$MF" 999
      echo 'package main // shipping fragment with wrong identity' > "$MF/api/fragment.go"
      git -C "$MF" add -- "$fragment_path" api/fragment.go; gcommit "$MF" 'Feature (#202)' ;;
  esac
  start="$(git -C "$MF" rev-parse HEAD)"
  args=(); [ "$mode" != stable ] || args=(--stable)
  [ "$mode" != no-commit ] || args=(--no-commit)
  run_rc "$MF" 0.2.0 "${args[@]}"
  assert_contains "$mode prints starting SHA" "$start" "$RC_OUT"
  assert_contains "$mode explicit restore" "git restore --source=$start --staged --worktree -- CHANGELOG.md deploy/chart/Chart.yaml deploy/chart/values.yaml scripts/assert-worker-tag-decoupled.sh $fragment_path" "$RC_OUT"
  assert_contains "$mode explicit postcommit reset" "git reset --soft $start" "$RC_OUT"
  assert_contains "$mode provisional recovery warning" "Inspect/remove any leftover provisional stable tag deliberately" "$RC_OUT"
  case "$mode" in
    rc|stable)
      assert_eq "$mode success" 0 "$RC_RC"
      assert_eq "$mode fragment once" 1 "$(fragment_count "$MF" 201)"
      assert_contains "$mode fragment citation identity" '- **Fragment 201 ([#201](https://github.com/vtmocanu/uzi/pull/201)).**' "$(cat "$MF/CHANGELOG.md")"
      assert_eq "$mode fragment consumed" no "$([ -f "$MF/$fragment_path" ] && echo yes || echo no)"
      assert_eq "$mode clean tracked tree" "" "$(git -C "$MF" status --porcelain --untracked-files=no)"
      assert_eq "$mode deletion committed" D "$(git -C "$MF" diff HEAD~1 HEAD --name-status -- "$fragment_path" | awk '{print $1}')"
      if [ "$mode" = rc ]; then
        repeat_start="$(git -C "$MF" rev-parse HEAD)"
        cp "$MF/CHANGELOG.md" "$MF/.git/repeat-notes"
        repeat_index="$(git -C "$MF" write-tree)"
        repeat_tree="$(git -C "$MF" diff HEAD --binary)"
        repeat_tags="$(git -C "$MF" show-ref --tags)"
        awk '/^## \[0.1.0\]/{p=1} p && /^\[Unreleased\]:/{exit} p{print}' "$MF/CHANGELOG.md" > "$MF/.git/history-before"
        run_rc "$MF" 0.2.0
        assert_eq "repeat first RC before tagging refuses empty pending" 3 "$RC_RC"
        assert_eq "repeat before tag retains HEAD" "$repeat_start" "$(git -C "$MF" rev-parse HEAD)"
        if cmp -s "$MF/.git/repeat-notes" "$MF/CHANGELOG.md"; then pass "repeat before tag retains complete CHANGELOG bytes"; else fail "repeat before tag retains complete CHANGELOG bytes"; fi
        assert_eq "repeat before tag retains index" "$repeat_index" "$(git -C "$MF" write-tree)"
        assert_eq "repeat before tag retains worktree" "$repeat_tree" "$(git -C "$MF" diff HEAD --binary)"
        assert_eq "repeat before tag retains tags" "$repeat_tags" "$(git -C "$MF" show-ref --tags)"
        awk '/^## \[0.1.0\]/{p=1} p && /^\[Unreleased\]:/{exit} p{print}' "$MF/CHANGELOG.md" > "$MF/.git/history-after"
        if cmp -s "$MF/.git/history-before" "$MF/.git/history-after"; then pass 'repeat historical bytes including blanks'; else fail 'repeat historical bytes including blanks'; fi
        assert_eq "repeat before tag never replays consumed fragment" 1 "$(fragment_count "$MF" 201)"
        assert_eq "repeat before tag fragment stays absent" no "$([ -f "$MF/$fragment_path" ] && echo yes || echo no)"
        git -C "$MF" tag v0.2.0-rc.1
        run_rc "$MF" 0.2.0
        assert_eq "empty RC respin succeeds" 0 "$RC_RC"
        assert_eq "empty respin never replays" 1 "$(fragment_count "$MF" 201)"
        git -C "$MF" tag v0.2.0-rc.2
        commit_fragment "$MF" 202; add_feature "$MF" 202
        run_rc "$MF" 0.2.0
        assert_eq "later RC fresh fragment succeeds" 0 "$RC_RC"
        assert_eq "later RC retains old once" 1 "$(fragment_count "$MF" 201)"
        assert_eq "later RC adds fresh once" 1 "$(fragment_count "$MF" 202)"
      fi ;;
    *)
      expected=1; [ "$mode" != no-commit ] || expected=0
      assert_eq "$mode status" "$expected" "$RC_RC"
      assert_eq "$mode HEAD retained" "$start" "$(git -C "$MF" rev-parse HEAD)"
      assert_fragment_staged "$MF" "$mode"
      if [ "$mode" = no-commit ]; then assert_contains "no-commit explicit successful prep" 'oracle not run' "$RC_OUT"; fi
      run_rc "$MF" 0.2.0
      assert_eq "$mode dirty retry refused" 3 "$RC_RC"
      git -C "$MF" restore --source="$start" --staged --worktree -- "${fragment_owned[@]}"
      assert_eq "$mode deliberate restore clean" "" "$(git -C "$MF" status --porcelain --untracked-files=no)"
      rm -f "$MF/.git/hooks/pre-commit"
      if [ "$mode" = oracle-failure ]; then
        commit_fragment "$MF" 201
        sed 's/(#201)/(#201, #202)/' "$MF/$fragment_path" > "$MF/.git/note"
        mv "$MF/.git/note" "$MF/$fragment_path"
        git -C "$MF" add -- "$fragment_path"; gcommit "$MF" 'docs: correct identities'
      fi
      run_rc "$MF" 0.2.0
      assert_eq "$mode restored retry succeeds" 0 "$RC_RC"
      assert_eq "$mode restored retry consumes once" 1 "$(fragment_count "$MF" 201)"
      if [ "$mode" = oracle-failure ]; then
        assert_contains "corrected retry retains both citation identities" '- **Fragment 201 ([#201](https://github.com/vtmocanu/uzi/pull/201), [#202](https://github.com/vtmocanu/uzi/pull/202)).**' "$(cat "$MF/CHANGELOG.md")"
      fi ;;
  esac
  rm -rf "$MF"
done

# Empty first cuts and shipping promotion refuse before changing any tracked bytes.
# Four bounded independent cases; an assertion failure does not stop siblings.
for mode in rc stable promote promote-dependency; do
  MF="$(mktemp -d "$REPO_ROOT/.uzi/scratch/m2.XXXXXX")"
  args=(); version=0.2.0
  case "$mode" in
    rc|stable) seed_repo "$MF"; [ "$mode" != stable ] || args=(--stable) ;;
    promote*)
      published_rc "$MF"; version=0.3.0; args=(--promote)
      if [ "$mode" = promote-dependency ]; then
        printf 'module example\n' > "$MF/api/go.mod"
        git -C "$MF" add -- api/go.mod; gcommit "$MF" 'chore(deps): bump (#202)'
      else add_feature "$MF" 202; fi ;;
  esac
  start="$(git -C "$MF" rev-parse HEAD)"
  index="$(git -C "$MF" write-tree)"; tags="$(git -C "$MF" show-ref --tags)"
  cp "$MF/CHANGELOG.md" "$MF/.git/before-notes"
  run_rc "$MF" "$version" "${args[@]}"
  assert_eq "$mode empty preflight refuses" 3 "$RC_RC"
  assert_contains "$mode empty refusal reason" 'empty and no --changelog-file given' "$RC_OUT"
  assert_eq "$mode empty HEAD retained" "$start" "$(git -C "$MF" rev-parse HEAD)"
  assert_eq "$mode empty index retained" "$index" "$(git -C "$MF" write-tree)"
  assert_eq "$mode empty tree retained" '' "$(git -C "$MF" diff HEAD --binary)"
  assert_eq "$mode empty tags retained" "$tags" "$(git -C "$MF" show-ref --tags)"
  if cmp -s "$MF/.git/before-notes" "$MF/CHANGELOG.md"; then pass "$mode empty complete notes retained"; else fail "$mode empty complete notes retained"; fi
  rm -rf "$MF" "$MF.origin.git"
done

# Only explicit promotion and fragment-free manual replacement bypass legacy grammar.
for mode in promote-only draft ordinary draft-fragment; do
  MF="$(mktemp -d "$REPO_ROOT/.uzi/scratch/m2.XXXXXX")"
  if [ "$mode" = promote-only ]; then published_rc "$MF"; add_feature "$MF" 202
  else seed_repo "$MF"; add_feature "$MF" 201; fi
  # Orphan prose is invalid pending grammar; released candidate notes remain valid.
  awk '/^## \[Unreleased\]$/ {print; print "Malformed orphan legacy prose"; next} {print}' "$MF/CHANGELOG.md" > "$MF/.git/notes"
  put_changelog "$MF" < "$MF/.git/notes"
  if [ "$mode" = promote-only ] || [ "$mode" = draft-fragment ]; then commit_fragment "$MF" 202; fi
  start="$(git -C "$MF" rev-parse HEAD)"; index="$(git -C "$MF" write-tree)"
  tags="$(git -C "$MF" show-ref --tags)"
  cp "$MF/CHANGELOG.md" "$MF/.git/before-notes"
  [ ! -f "$MF/$fragment_path" ] || cp "$MF/$fragment_path" "$MF/.git/before-fragment"
  printf '## [0.2.0]\n\n### Added\n\n- **Draft shipping feature (#201).**\n  Manual replacement description.\n' > "$MF/.git/draft"
  args=(); case "$mode" in promote-only) args=(--promote-only);; draft*) args=(--changelog-file "$MF/.git/draft");; esac
  run_rc "$MF" 0.2.0 "${args[@]}"
  case "$mode" in
    draft)
      assert_eq 'malformed legacy draft succeeds with oracle' 0 "$RC_RC"
      assert_contains 'draft retains shipping identity' '- **Draft shipping feature ([#201](https://github.com/vtmocanu/uzi/pull/201)).**' "$(cat "$MF/CHANGELOG.md")"
      assert_eq 'draft replacement removes malformed legacy' 0 "$(awk '/Malformed orphan/{n++} END{print n+0}' "$MF/CHANGELOG.md")"
      assert_eq 'draft committed tree clean' '' "$(git -C "$MF" status --porcelain --untracked-files=no)" ;;
    *)
      expected=3; [ "$mode" != promote-only ] || expected=0
      assert_eq "$mode malformed legacy result" "$expected" "$RC_RC"
      assert_eq "$mode malformed legacy HEAD preserved" "$start" "$(git -C "$MF" rev-parse HEAD)"
      assert_eq "$mode malformed legacy index preserved" "$index" "$(git -C "$MF" write-tree)"
      assert_eq "$mode malformed legacy tree preserved" '' "$(git -C "$MF" diff HEAD --binary)"
      if cmp -s "$MF/.git/before-notes" "$MF/CHANGELOG.md"; then pass "$mode malformed main notes exact"; else fail "$mode malformed main notes exact"; fi
      if [ "$mode" = promote-only ]; then
        assert_eq 'malformed explicit promotion parent is candidate' "$(git -C "$MF" rev-parse 'v0.2.0-rc.1^{commit}')" "$(git -C "$MF" rev-parse -q --verify 'v0.2.0^{commit}^' 2>/dev/null)"
        assert_eq 'malformed explicit promotion excludes later shipping' '' "$(git -C "$MF" ls-tree v0.2.0 -- api/feature_202.go "$fragment_path" 2>/dev/null)"
        assert_eq 'malformed explicit promotion uses candidate notes' 0 "$(git -C "$MF" show v0.2.0:CHANGELOG.md 2>/dev/null | awk '/Malformed orphan|Fragment 202/{n++} END{print n+0}')"
      else
        assert_eq "$mode malformed tags preserved" "$tags" "$(git -C "$MF" show-ref --tags)"
        reason='unknown or misplaced legacy content'; [ "$mode" != draft-fragment ] || reason='--changelog-file is refused with pending fragments'
        assert_contains "$mode malformed refusal reason" "$reason" "$RC_OUT"
      fi
      if [ -f "$MF/.git/before-fragment" ]; then
        if cmp -s "$MF/.git/before-fragment" "$MF/$fragment_path"; then pass "$mode fragment bytes exact"; else fail "$mode fragment bytes exact"; fi
      fi ;;
  esac
  rm -rf "$MF" "$MF.origin.git"
done

# A draft flag does not bypass grammar on routes that still consume legacy notes.
for route in nextrc skiprc; do
  MF="$(mktemp -d "$REPO_ROOT/.uzi/scratch/m2.XXXXXX")"; published_rc "$MF"; add_feature "$MF" 202
  awk '
    { print }
    /^## \[Unreleased\]$/ {
      print "### Added"; print "- **Feature 202 (#202).**"; print "  Description."
      print ""; print "Orphan prose after a completed bullet"
    }
  ' "$MF/CHANGELOG.md" > "$MF/.git/notes"
  put_changelog "$MF" < "$MF/.git/notes"
  start="$(git -C "$MF" rev-parse HEAD)"; index="$(git -C "$MF" write-tree)"
  tags="$(git -C "$MF" show-ref --tags)"; cp "$MF/CHANGELOG.md" "$MF/.git/before-notes"
  printf '## [0.3.0]\n### Added\n- Draft (#202)\n' > "$MF/.git/draft"
  version=0.2.0; args=(--changelog-file "$MF/.git/draft")
  if [ "$route" = skiprc ]; then version=0.3.0; args+=(--skip-promote); fi
  run_rc "$MF" "$version" "${args[@]}"
  assert_eq "$route draft legacy consumer refuses" 3 "$RC_RC"
  assert_contains "$route draft legacy consumer grammar" 'unknown or misplaced legacy content' "$RC_OUT"
  assert_eq "$route draft refusal HEAD unchanged" "$start" "$(git -C "$MF" rev-parse HEAD)"
  assert_eq "$route draft refusal index unchanged" "$index" "$(git -C "$MF" write-tree)"
  assert_eq "$route draft refusal tree unchanged" '' "$(git -C "$MF" diff HEAD --binary)"
  assert_eq "$route draft refusal tags unchanged" "$tags" "$(git -C "$MF" show-ref --tags)"
  if cmp -s "$MF/.git/before-notes" "$MF/CHANGELOG.md"; then pass "$route draft refusal bytes exact"; else fail "$route draft refusal bytes exact"; fi
  rm -rf "$MF" "$MF.origin.git"
done

# Grammar bypasses retain bounded input and fragment validation before routing.
# Each of these finite cases must fail at input validation, not later discovery.
for route in draft promote-only; do
  for fault in fragment nul control bidi pending-limit full-limit duplicate; do
    MF="$(mktemp -d "$REPO_ROOT/.uzi/scratch/m2.XXXXXX")"; seed_repo "$MF"
    reason='control byte in input'
    case "$fault" in
      fragment)
        mkdir -p "$MF/changelog.d"; printf 'invalid\n' > "$MF/$fragment_path"
        git -C "$MF" add -- "$fragment_path"; reason='expected one canonical category' ;;
      nul|control|bidi)
        byte='\000'; [ "$fault" != control ] || byte='\001'; [ "$fault" != bidi ] || byte='\342\200\256'
        { printf '# Changelog\n\n## [Unreleased]\n'; printf '%b\n' "$byte"; } > "$MF/CHANGELOG.md"
        [ "$fault" != nul ] || reason='NUL byte'
        [ "$fault" != bidi ] || reason='direction control in input' ;;
      pending-limit|full-limit)
        size=4194305; [ "$fault" != full-limit ] || size=16777217
        { printf '# Changelog\n\n'; [ "$fault" != pending-limit ] || printf '## [Unreleased]\n'; head -c "$size" /dev/zero | tr '\000' x; } > "$MF/CHANGELOG.md"
        reason='bytes' ;;
      duplicate) printf '# Changelog\n\n## [Unreleased]\n\n## [Unreleased]\n' > "$MF/CHANGELOG.md"; reason='expected exactly one [Unreleased] section' ;;
    esac
    git -C "$MF" add -- CHANGELOG.md; gcommit "$MF" 'docs: unsafe pending input'
    start="$(git -C "$MF" rev-parse HEAD)"; index="$(git -C "$MF" write-tree)"
    printf '## [0.2.0]\n### Added\n- Draft (#201)\n' > "$MF/.git/draft"
    args=(--promote-only); [ "$route" != draft ] || args=(--changelog-file "$MF/.git/draft")
    run_rc "$MF" 0.2.0 "${args[@]}"
    assert_eq "$route $fault input refuses" 3 "$RC_RC"
    assert_contains "$route $fault input diagnostic" "$reason" "$RC_OUT"
    assert_eq "$route $fault HEAD retained" "$start" "$(git -C "$MF" rev-parse HEAD)"
    assert_eq "$route $fault index retained" "$index" "$(git -C "$MF" write-tree)"
    assert_eq "$route $fault tree retained" '' "$(git -C "$MF" diff HEAD --binary)"
    rm -rf "$MF"
  done
done

MF="$(mktemp -d "$REPO_ROOT/.uzi/scratch/m2.XXXXXX")"; seed_repo "$MF"
printf 'module example\n' > "$MF/api/go.mod"
git -C "$MF" add -- api/go.mod; gcommit "$MF" 'chore(deps): bump (#203)'
run_rc "$MF" 0.2.0
assert_eq 'empty legacy dependency-only plain cut succeeds' 0 "$RC_RC"
assert_contains 'empty legacy dependency-only cut autocites' '- **Routine dependency updates ([#203](https://github.com/vtmocanu/uzi/pull/203)).**' "$(cat "$MF/CHANGELOG.md")"
rm -rf "$MF"

# Refusals preserve HEAD, files, index and tag set, including combined promotion.
for mode in malformed untracked draft promote-draft; do
  MF="$(mktemp -d "$REPO_ROOT/.uzi/scratch/m2.XXXXXX")"
  if [ "$mode" = promote-draft ]; then published_rc "$MF"; else seed_repo "$MF"; fi
  commit_fragment "$MF" 201
  case "$mode" in
    malformed) printf 'invalid\n' > "$MF/$fragment_path"; git -C "$MF" add -- "$fragment_path"; gcommit "$MF" 'docs: malformed' ;;
    untracked) git -C "$MF" rm -q --cached -- "$fragment_path"; gcommit "$MF" 'docs: untrack' ;;
  esac
  start="$(git -C "$MF" rev-parse HEAD)"; before="$(git -C "$MF" status --porcelain)"; tags="$(git -C "$MF" tag)"
  cp "$MF/CHANGELOG.md" "$MF/.git/before-notes"
  cp "$MF/$fragment_path" "$MF/.git/before-fragment"
  printf '## [0.3.0]\n### Added\n- Draft (#201)\n' > "$MF/.git/draft"
  args=(); version=0.2.0
  case "$mode" in draft) args=(--changelog-file "$MF/.git/draft");; promote-draft) args=(--promote --changelog-file "$MF/.git/draft"); version=0.3.0;; esac
  run_rc "$MF" "$version" "${args[@]}"
  assert_eq "$mode refuses" 3 "$RC_RC"
  assert_eq "$mode HEAD untouched" "$start" "$(git -C "$MF" rev-parse HEAD)"
  assert_eq "$mode index/worktree untouched" "$before" "$(git -C "$MF" status --porcelain)"
  assert_eq "$mode tags untouched" "$tags" "$(git -C "$MF" tag)"
  if cmp -s "$MF/CHANGELOG.md" "$MF/.git/before-notes" && cmp -s "$MF/$fragment_path" "$MF/.git/before-fragment"; then pass "$mode bytes untouched"; else fail "$mode bytes untouched"; fi
  rm -rf "$MF" "$MF.origin.git"
done

for mode in promote-only promote skip; do
  MF="$(mktemp -d "$REPO_ROOT/.uzi/scratch/m2.XXXXXX")"; published_rc "$MF"; commit_fragment "$MF" 202
  start="$(git -C "$MF" rev-parse HEAD)"
  cp "$MF/$fragment_path" "$MF/.git/promotion-fragment-before"
  if [ "$mode" = promote-only ]; then run_rc "$MF" 0.2.0 --promote-only; else run_rc "$MF" 0.3.0 "--$([ "$mode" = skip ] && echo skip-promote || echo promote)"; fi
  assert_eq "$mode pending fragment success" 0 "$RC_RC"
  if [ "$mode" = promote-only ]; then
    assert_eq "explicit promotion main unchanged" "$start" "$(git -C "$MF" rev-parse HEAD)"
    if cmp -s "$MF/.git/promotion-fragment-before" "$MF/$fragment_path"; then pass 'explicit promotion fragment bytes unchanged'; else fail 'explicit promotion fragment bytes unchanged'; fi
    assert_eq "explicit promotion notes not on main" 0 "$(fragment_count "$MF" 202)"
  else
    assert_eq "$mode fragment folded once" 1 "$(fragment_count "$MF" 202)"
    assert_eq "$mode candidate advanced despite notes-only work" 0.3.0-rc.1 "$(chart_ver "$MF")"
  fi
  if [ "$mode" != skip ]; then
    assert_eq "$mode stable isolates pending main fragment" "" "$(git -C "$MF" ls-tree v0.2.0 -- "$fragment_path")"
    assert_eq "$mode stable excludes pending note" 0 "$(git -C "$MF" show v0.2.0:CHANGELOG.md | awk '/Fragment 202/{n++} END{print n+0}')"
  fi
  rm -rf "$MF" "$MF.origin.git"
done

# Duplicate physical blocks remain distinct, including legacy duplicate categories.
MF="$(mktemp -d "$REPO_ROOT/.uzi/scratch/m2.XXXXXX")"; seed_repo "$MF"; commit_fragment "$MF" 201
cp "$MF/$fragment_path" "$MF/$fragment_second"
printf '# Changelog\n\n## [Unreleased]\n### Added\n- Legacy one (#201)\n### Added\n- **Legacy two (#201).**\n  Legacy continuation bytes.\n\n## [0.1.0]\n- Initial (#100)\n' | put_changelog "$MF"
git -C "$MF" add -- "$fragment_second"; gcommit "$MF" 'docs: duplicate fragment'
printf 'module example\n' > "$MF/api/go.mod"; git -C "$MF" add -- api/go.mod; gcommit "$MF" 'chore(deps): bump (#203)'
run_rc "$MF" 0.2.0
assert_eq "legacy plus duplicate fragments succeeds" 0 "$RC_RC"
assert_eq "identical fragment blocks remain duplicate" 2 "$(fragment_count "$MF" 201)"
assert_eq "one grouped Added section" 1 "$(awk '/^### Added$/{n++} END{print n+0}' "$MF/CHANGELOG.md")"
assert_contains "legacy one-line preserved" '- Legacy one ([#201](https://github.com/vtmocanu/uzi/pull/201))' "$(cat "$MF/CHANGELOG.md")"
assert_contains "legacy continuation preserved" '  Legacy continuation bytes.' "$(cat "$MF/CHANGELOG.md")"
assert_contains "fragment cut dependency autocitation" '- **Routine dependency updates ([#203](https://github.com/vtmocanu/uzi/pull/203)).**' "$(cat "$MF/CHANGELOG.md")"
assert_eq "duplicate inputs both consumed" "" "$(git -C "$MF" ls-files -- changelog.d)"
rm -rf "$MF"

# Canonical category order applies to legacy-only and mixed pending notes.
for inventory in legacy mixed; do
  MF="$(mktemp -d "$REPO_ROOT/.uzi/scratch/m2.XXXXXX")"; seed_repo "$MF"
  printf '# Changelog\n\n## [Unreleased]\n### Fixed\n- Fixed legacy (#201)\n### Added\n- Added legacy (#201)\n### Fixed\n- Fixed duplicate (#201)\n\n## [0.1.0]\n- Initial (#100)\n' | put_changelog "$MF"
  if [ "$inventory" = mixed ]; then
    write_fragment "$MF" 201
    mkdir -p "$MF/changelog.d"
    printf '### Changed\n\n- **Changed fragment (#201).**\n  Changed fragment description.\n' > "$MF/$fragment_second"
    git -C "$MF" add -- "$fragment_path" "$fragment_second"
    gcommit "$MF" 'docs: mixed pending notes'
  fi
  run_rc "$MF" 0.2.0
  assert_eq "$inventory canonical cut succeeds" 0 "$RC_RC"
  expected=$'### Added\n### Fixed'
  [ "$inventory" != mixed ] || expected=$'### Added\n### Changed\n### Fixed'
  assert_eq "$inventory canonical grouped categories" "$expected" "$(awk '/^### /{print}' "$MF/CHANGELOG.md")"
  expected=$'- Added legacy\n- Fixed legacy\n- Fixed duplicate'
  [ "$inventory" != mixed ] || expected=$'- Added legacy\n- **Fragment 201\n- **Changed fragment\n- Fixed legacy\n- Fixed duplicate'
  assert_eq "$inventory deterministic block order and multiplicity" "$expected" "$(awk '/^- / && !/Initial/ {sub(/ \(.*/, ""); print}' "$MF/CHANGELOG.md")"
  assert_eq "$inventory released section stays at original version" $'## [0.1.0] - 2026-09-01\n- Initial ([#100](https://github.com/vtmocanu/uzi/pull/100))' "$(awk '/^## \[0.1.0\]/{p=1} p && /^\[Unreleased\]:/{exit} p && NF{print}' "$MF/CHANGELOG.md")"
  rm -rf "$MF"
done

# Already maintained historical content needs no linking/date reconciliation.
# Bound this section by another released heading so blank bytes are significant.
MF="$(mktemp -d "$REPO_ROOT/.uzi/scratch/m2.XXXXXX")"; seed_repo "$MF"; commit_fragment "$MF" 201
cat <<'MD' | put_changelog "$MF"
# Changelog

## [Unreleased]

## [0.1.0] - 2026-09-01

### Added

- **Initial** ([#100](https://github.com/vtmocanu/uzi/pull/100))


## [0.0.9] - 2026-08-01

- Older already maintained history.
MD
history_section() { awk '/^## \[0.0.9\]/{exit} /^## \[0.1.0\]/{p=1} p{print}' "$1"; }
history_section "$MF/CHANGELOG.md" > "$MF/.git/history-before"
# Exercise the read-only assembly seam before the cutter's splice and full cut.
(cd "$MF" && bash scripts/changelog-fragments.sh render-unreleased CHANGELOG.md) > "$MF/.git/assembled"
history_section "$MF/CHANGELOG.md" > "$MF/.git/history-after"
if cmp -s "$MF/.git/history-before" "$MF/.git/history-after"; then pass 'assembly leaves exact history bytes'; else fail 'assembly leaves exact history bytes'; fi
run_rc "$MF" 0.2.0
assert_eq 'maintained historical fixture cut succeeds' 0 "$RC_RC"
history_section "$MF/CHANGELOG.md" > "$MF/.git/history-after"
if cmp -s "$MF/.git/history-before" "$MF/.git/history-after"; then pass 'full cut splice preserves exact historical bytes including blanks'; else fail 'full cut splice preserves exact historical bytes including blanks'; fi
rm -rf "$MF"

# Deterministic two-input failures: remove the first input then fail, or refuse
# final staging after both removals. Each independent fixture restores and retries.
for fault in first-deletion staging; do
  MF="$(mktemp -d "$REPO_ROOT/.uzi/scratch/m2.XXXXXX")"; seed_repo "$MF"; commit_fragment "$MF" 201
  write_fragment "$MF" 202; mv "$MF/$fragment_path" "$MF/$fragment_second"
  git -C "$MF" restore -- "$fragment_path"
  git -C "$MF" add -- "$fragment_second"; gcommit "$MF" 'docs: second fragment'
  start="$(git -C "$MF" rev-parse HEAD)"
  cp "$MF/$fragment_path" "$MF/.git/original-first"
  cp "$MF/$fragment_second" "$MF/.git/original-second"
  mkdir "$MF/.git/bin"
  real_rm="$(command -v rm)"; real_git="$(command -v git)"
  cat > "$MF/.git/bin/rm" <<'STUB'
#!/usr/bin/env bash
if [ "$FIXTURE_FAULT" = first-deletion ] && [ "$#" = 2 ] && [ "$1" = -- ] && [ "$2" = "$FIXTURE_FIRST" ]; then
  "$FIXTURE_RM" "$@" || exit
  printf 'first-deletion\n' > .git/fault-reached
  exit 1
fi
exec "$FIXTURE_RM" "$@"
STUB
  cat > "$MF/.git/bin/git" <<'STUB'
#!/usr/bin/env bash
if [ "$FIXTURE_FAULT" = staging ] && [ "${1:-}" = add ] && [ "${2:-}" = -- ]; then
  for arg in "$@"; do
    if [ "$arg" = "$FIXTURE_FIRST" ]; then
      printf 'staging\n' > .git/fault-reached
      exit 1
    fi
  done
fi
exec "$FIXTURE_GIT" "$@"
STUB
  chmod +x "$MF/.git/bin/rm" "$MF/.git/bin/git"
  PATH="$MF/.git/bin:$PATH" FIXTURE_FAULT="$fault" FIXTURE_FIRST="$fragment_path" FIXTURE_RM="$real_rm" FIXTURE_GIT="$real_git" run_rc "$MF" 0.2.0
  assert_eq "$fault reached intended boundary" "$fault" "$(cat "$MF/.git/fault-reached")"
  assert_eq "$fault cut fails" 1 "$RC_RC"
  assert_eq "$fault HEAD retained" "$start" "$(git -C "$MF" rev-parse HEAD)"
  assert_eq "$fault first input removed" no "$([ -f "$MF/$fragment_path" ] && echo yes || echo no)"
  expected=no; [ "$fault" != first-deletion ] || expected=yes
  assert_eq "$fault second input at boundary" "$expected" "$([ -f "$MF/$fragment_second" ] && echo yes || echo no)"
  for number in 201 202; do assert_eq "$fault assembled $number once" 1 "$(fragment_count "$MF" "$number")"; done
  assert_contains "$fault restore includes both inputs" "git restore --source=$start --staged --worktree -- CHANGELOG.md deploy/chart/Chart.yaml deploy/chart/values.yaml scripts/assert-worker-tag-decoupled.sh $fragment_path $fragment_second" "$RC_OUT"
  git -C "$MF" restore --source="$start" --staged --worktree -- "${fragment_owned[@]}" "$fragment_second"
  for pair in "original-first:$fragment_path" "original-second:$fragment_second"; do
    original=${pair%%:*}; input=${pair#*:}
    git -C "$MF" show ":$input" > "$MF/.git/restored-index"
    if cmp -s "$MF/.git/$original" "$MF/$input" && cmp -s "$MF/.git/$original" "$MF/.git/restored-index"; then pass "$fault $input original index/worktree bytes"; else fail "$fault $input original index/worktree bytes"; fi
  done
  assert_eq "$fault restored tree and index clean" '' "$(git -C "$MF" status --porcelain --untracked-files=no)"
  run_rc "$MF" 0.2.0
  assert_eq "$fault restored retry succeeds" 0 "$RC_RC"
  for number in 201 202; do assert_eq "$fault retry $number exactly once" 1 "$(fragment_count "$MF" "$number")"; done
  assert_eq "$fault retry consumes both inputs" '' "$(git -C "$MF" ls-files -- "$fragment_path" "$fragment_second")"
  rm -rf "$MF"
done

# A fragment-touch shipping commit needs the right identity in the assembled section.
for identity in missing wrong correct; do
  MF="$(mktemp -d "$REPO_ROOT/.uzi/scratch/m2.XXXXXX")"; seed_repo "$MF"; write_fragment "$MF" 999
  case "$identity" in
    missing) sed 's/ (#999)//' "$MF/$fragment_path" > "$MF/.git/note"; mv "$MF/.git/note" "$MF/$fragment_path" ;;
    correct) write_fragment "$MF" 201 ;;
  esac
  echo 'package main // ships' > "$MF/api/feature.go"
  git -C "$MF" add -- api/feature.go "$fragment_path"; gcommit "$MF" 'Feature (#201)'
  run_rc "$MF" 0.2.0
  expected=1; [ "$identity" != correct ] || expected=0
  assert_eq "shipping fragment $identity identity" "$expected" "$RC_RC"
  if [ "$identity" != correct ]; then
    assert_contains "$identity identity oracle refusal" 'VERIFY FAILED' "$RC_OUT"
    assert_fragment_staged "$MF" "$identity identity"
  else
    assert_eq "correct identity rendered once" 1 "$(fragment_count "$MF" 201)"
    assert_contains "correct identity citation retained" '- **Fragment 201 ([#201](https://github.com/vtmocanu/uzi/pull/201)).**' "$(cat "$MF/CHANGELOG.md")"
  fi
  rm -rf "$MF"
done

# Deterministic interruption at git commit: the fixture wrapper stops its known
# parent, reports readiness over a pipe, then exits. The owner kills only Popen.pid.
for phase in before after; do
  MF="$(mktemp -d "$REPO_ROOT/.uzi/scratch/m2.XXXXXX")"; seed_repo "$MF"; commit_fragment "$MF" 201
  start="$(git -C "$MF" rev-parse HEAD)"
  mkdir "$MF/.git/bin"
  real_git="$(command -v git)"
  cat > "$MF/.git/bin/git" <<'STUB'
#!/usr/bin/env python3
import os
import signal
import subprocess
import sys
if sys.argv[1:2] == ['commit']:
    if os.environ['INTERRUPT_PHASE'] == 'after':
        result = subprocess.run([os.environ['FIXTURE_GIT'], *sys.argv[1:]], check=False)
        if result.returncode:
            raise SystemExit(result.returncode)
    os.kill(os.getppid(), signal.SIGSTOP)
    os.write(int(os.environ['READY_FD']), b'R')
    raise SystemExit(1)
os.execv(os.environ['FIXTURE_GIT'], [os.environ['FIXTURE_GIT'], *sys.argv[1:]])
STUB
  chmod +x "$MF/.git/bin/git"
  python3 - "$MF" "$RC" "$phase" "$real_git" <<'PY'
import os
from pathlib import Path
import select
import subprocess
import sys
repo, cutter, phase, git = sys.argv[1:]
reader, writer = os.pipe()
env = {**os.environ, 'PATH': str(Path(repo) / '.git/bin') + ':' + os.environ['PATH'],
       'READY_FD': str(writer), 'INTERRUPT_PHASE': phase, 'FIXTURE_GIT': git,
       'UZI_RELEASE_OFFLINE_FIXTURE': '1', 'UZI_CHANGELOG_REPO_URL': 'https://github.com/vtmocanu/uzi'}
with (Path(repo) / '.git/interruption.log').open('w') as log:
    process = subprocess.Popen(['bash', cutter, '0.2.0'], cwd=repo, env=env,
                               stdout=log, stderr=subprocess.STDOUT, pass_fds=(writer,))
    os.close(writer)
    try:
        if not select.select([reader], [], [], 15)[0] or os.read(reader, 1) != b'R':
            raise RuntimeError('fixture did not reach commit boundary')
    finally:
        process.kill()
        process.wait(timeout=5)
        os.close(reader)
PY
  interrupt_rc=$?
  assert_eq "$phase interruption reached deterministic boundary" 0 "$interrupt_rc"
  RC_OUT="$(cat "$MF/.git/interruption.log")"
  assert_contains "$phase interruption printed starting SHA" "$start" "$RC_OUT"
  assert_contains "$phase interruption printed owned restore" "git restore --source=$start --staged --worktree -- CHANGELOG.md deploy/chart/Chart.yaml deploy/chart/values.yaml scripts/assert-worker-tag-decoupled.sh $fragment_path" "$RC_OUT"
  assert_eq "$phase interrupted notes exactly once" 1 "$(fragment_count "$MF" 201)"
  if [ "$phase" = before ]; then
    assert_eq "before interruption HEAD unchanged" "$start" "$(git -C "$MF" rev-parse HEAD)"
    assert_fragment_staged "$MF" "before interruption"
    run_rc "$MF" 0.2.0
    assert_eq "interrupted dirty retry refuses" 3 "$RC_RC"
  else
    assert_eq "after interruption release committed" 'chore(release): v0.2.0-rc.1' "$(head_msg "$MF")"
    assert_eq "after interruption may be clean" "" "$(git -C "$MF" status --porcelain --untracked-files=no)"
    git -C "$MF" reset --soft "$start"
    assert_fragment_staged "$MF" "after deliberate soft reset"
  fi
  git -C "$MF" restore --source="$start" --staged --worktree -- "${fragment_owned[@]}"
  assert_eq "$phase interrupted restore clean" "" "$(git -C "$MF" status --porcelain --untracked-files=no)"
  assert_eq "$phase interrupted fragment restored" yes "$([ -f "$MF/$fragment_path" ] && echo yes || echo no)"
  run_rc "$MF" 0.2.0
  assert_eq "$phase interrupted restore retry succeeds" 0 "$RC_RC"
  assert_eq "$phase interrupted retry exactly once" 1 "$(fragment_count "$MF" 201)"
  rm -rf "$MF"
done

if [ "${1:-}" = "--fragments-only" ]; then
  echo "=== release-scripts-test: $PASSES passed, $FAILS failed ==="
  [ "$FAILS" -eq 0 ]; exit $?
fi

echo "=== M3: release-mode lib (shared by watch + verify) ==="
# shellcheck source=scripts/lib/release-mode.sh
. "$REPO_ROOT/scripts/lib/release-mode.sh"
assert_eq "release_mode rc"        "rc"     "$(release_mode 0.83.0-rc.1)"
assert_eq "release_mode rc (v)"    "rc"     "$(release_mode v0.83.0-rc.2)"
assert_eq "release_mode stable"    "stable" "$(release_mode 0.83.0)"
assert_eq "release_mode stable(v)" "stable" "$(release_mode v0.83.0)"
assert_eq "release_base rc"        "0.83.0" "$(release_base 0.83.0-rc.10)"
assert_eq "release_base stable"    "0.83.0" "$(release_base v0.83.0)"

echo
echo "=== release-scripts-test: $PASSES passed, $FAILS failed ==="
[ "$FAILS" -eq 0 ]
