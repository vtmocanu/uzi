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
#
# Wired as `task test:release-scripts` (its own gate member, not gate:repo's
# sub-second band: it builds a multi-commit repo). Watch it fail on the
# unmodified scripts before M1 lands.
set -uo pipefail

SCRIPTS_DIR="$(cd "$(dirname "$0")" && pwd)"
ORACLE="$SCRIPTS_DIR/assert-changelog-covers-release.sh"
SECTION="$SCRIPTS_DIR/changelog-section.sh"

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
trap 'rm -rf "$REPO"' EXIT

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
}

# oracle_prev <ref> <version> — echo the PREV the oracle derives (VERSION passed,
# PREV left empty so the script derives it). Parsed from its "coverage for A..B" line.
oracle_prev() {
  ( cd "$REPO" && bash "$ORACLE" "$1" "" "$2" 2>&1 ) | sed -n 's/^Checking CHANGELOG coverage for \(.*\)\.\..*/\1/p' | head -1
}
# oracle_run <ref> <version> — run with derived PREV; echoes rc into $? via caller
oracle_run() { ( cd "$REPO" && bash "$ORACLE" "$1" "" "$2" >/dev/null 2>&1 ); }

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

echo "=== M1: changelog-section.sh title/body ==="
title="$( cd "$REPO" && bash "$SECTION" title 0.2.0-rc.1 2>&1 )"
assert_eq "section title is the full tag" "v0.2.0-rc.1" "$title"
body="$( cd "$REPO" && bash "$SECTION" body 0.2.0-rc.1 2>&1 )"
assert_contains "section body reads [0.2.0]" "Feature A" "$body"

echo
echo "=== release-scripts-test: $PASSES passed, $FAILS failed ==="
[ "$FAILS" -eq 0 ]
