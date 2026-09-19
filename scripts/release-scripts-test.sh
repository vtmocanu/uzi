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
  mkdir -p "$d/scripts" "$d/deploy/chart" "$d/api"
  local s
  for s in worker-tag-autobump changelog-links assert-changelog-covers-release changelog-section assert-worker-tag-decoupled; do
    [ -f "$SCRIPTS_DIR/$s.sh" ] && { cp "$SCRIPTS_DIR/$s.sh" "$d/scripts/$s.sh"; chmod +x "$d/scripts/$s.sh"; }
  done
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
  RC_OUT="$( cd "$d" && UZI_CHANGELOG_REPO_URL="https://github.com/vtmocanu/uzi" bash "$RC" "$@" 2>&1 )"; RC_RC=$?
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

echo "=== M2: next RC refused on non-empty [Unreleased] ==="
S3="$(mktemp -d)"; seed_repo "$S3"; add_feature "$S3" 201
put_changelog "$S3" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S3" 0.2.0                # first RC
git -C "$S3" tag v0.2.0-rc.1
put_changelog "$S3" <<'MD'
# Changelog

## [Unreleased]
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
assert_eq "next RC refused (non-empty [Unreleased]) exits 3" "3" "$RC_RC"
assert_contains "next RC refusal explains EMPTY rule" "must be EMPTY" "$RC_OUT"

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

echo "=== M2b: --promote rolls back the local stable tag when the main half fails ==="
S10="$(mktemp -d)"; seed_repo "$S10"; add_origin "$S10"; add_feature "$S10" 201
put_changelog "$S10" <<'MD'
# Changelog

## [Unreleased]
### Added
- **Feature 201** (#201)

## [0.1.0] - 2026-09-01
### Added
- **Initial** (#100)
MD
run_rc "$S10" 0.2.0; git -C "$S10" tag v0.2.0-rc.1; push_tag_to_origin "$S10" v0.2.0-rc.1
add_feature "$S10" 301               # merges since the RC, so NOT the promote-only path
# but leave [Unreleased] EMPTY and pass no --changelog-file, so the main-half fold fails
# AFTER promote_inflight has already tagged v0.2.0. The trap must roll that tag back.
run_rc "$S10" 0.3.0 --promote
if [ "$RC_RC" -ne 0 ]; then pass "promote main-half failure exits nonzero"; else fail "promote main-half failure exits nonzero"; fi
if git -C "$S10" rev-parse -q --verify refs/tags/v0.2.0 >/dev/null; then fail "failed promote leaves NO local v0.2.0 tag (rolled back)"; else pass "failed promote leaves NO local v0.2.0 tag (rolled back)"; fi

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

rm -rf "$S1" "$S3" "$S4" "$S6" "$S7" "$S8" "$S8B" "$S8C" "$S9" "$S10" "$SA" "$SB" "$SC" "$SD" "$SE" \
       "$S8.origin.git" "$S8B.origin.git" "$S9.origin.git" "$S10.origin.git" "$SD.origin.git" "$SE.origin.git"

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
