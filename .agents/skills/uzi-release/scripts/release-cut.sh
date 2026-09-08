#!/usr/bin/env bash
# release-cut.sh — deterministically PREPARE the release commit on main. One command
# for the mechanical spine of a cut: apply the CHANGELOG section, bump the chart,
# auto-bump the worker tag, refresh changelog links, commit, and verify with the
# coverage oracle. It does NOT push and does NOT tag — the lead reviews `git show
# HEAD`, pushes main, waits for CI (release-watch.sh / watch-run-ci.sh), then tags.
#
#   release-cut.sh <X.Y.Z> <prev-tag> [--changelog-file FILE] [--no-commit]
#
#   <X.Y.Z>            release version (leading v optional). Model B: chart
#                      version == appVersion == the tag.
#   <prev-tag>         the previous v* tag (e.g. v0.73.0), for the coverage oracle.
#   --changelog-file   a file whose content is the drafted `## [X.Y.Z] - <date>`
#                      section (heading + body); it is inserted below `## [Unreleased]`.
#                      Omit to FOLD the existing non-empty [Unreleased] into a dated
#                      `## [X.Y.Z]` section instead.
#   --no-commit        do every edit and stage them, but do not commit (and so do NOT
#                      run the oracle, which reads a commit). Escape hatch for review.
#
# What it does, in order:
#   1. Preconditions: clean tree, on the default branch, prev-tag exists.
#   2. CHANGELOG: insert the drafted section (or fold [Unreleased]); keep an empty
#      [Unreleased] on top.
#   3. Chart.yaml: version + appVersion -> X.Y.Z.
#   4. scripts/worker-tag-autobump.sh X.Y.Z (rolls the fleet IFF the agent runtime
#      surface changed; leaves it pinned otherwise — see that script / adr/0422).
#   5. scripts/changelog-links.sh (linkify bare #N, refresh compare-link footers).
#   6. Commit `chore(release): vX.Y.Z` (unless --no-commit).
#   7. Verify: scripts/assert-changelog-covers-release.sh HEAD <prev> X.Y.Z, and
#      changelog-links.sh --check. On failure, undo the commit (reset --soft) so the
#      tree is recoverable, print the reason, exit 1.
#
# Exit codes: 0 ready to push;  1 a step/verify failed (tree left recoverable);
#             3 usage / precondition failure.
#
# NEVER [skip ci] the release commit (release.yml assumes ci.yml is green on it).
set -uo pipefail

VERSION=""; PREV=""; CL_FILE=""; DO_COMMIT=1
while [ $# -gt 0 ]; do
  case "$1" in
    --changelog-file) CL_FILE="${2:?}"; shift 2;;
    --no-commit) DO_COMMIT=0; shift;;
    -h|--help) sed -n '2,38p' "$0"; exit 3;;
    *) if [ -z "$VERSION" ]; then VERSION="$1"; shift
       elif [ -z "$PREV" ]; then PREV="$1"; shift
       else echo "unexpected arg: $1" >&2; exit 3; fi ;;
  esac
done
if [ -z "$VERSION" ] || [ -z "$PREV" ]; then
  echo "usage: release-cut.sh <X.Y.Z> <prev-tag> [--changelog-file FILE] [--no-commit]" >&2
  exit 3
fi
VERSION="${VERSION#v}"
if ! [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "release-cut: '$VERSION' is not X.Y.Z" >&2; exit 3
fi
TAG="v$VERSION"; TODAY="$(date +%F)"

ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || { echo "not in a git repo" >&2; exit 3; }
cd "$ROOT" || { echo "cannot cd to repo root $ROOT" >&2; exit 3; }

# --- 1. preconditions ---------------------------------------------------------
if ! git rev-parse -q --verify "refs/tags/$PREV" >/dev/null; then
  echo "release-cut: prev-tag '$PREV' does not exist" >&2; exit 3
fi
# Tracked changes only: untracked files (new scripts, scratch) do not enter the
# release commit, which stages known paths, so they must not block a cut.
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  echo "release-cut: tracked working-tree changes present — commit or stash first" >&2
  git status --short --untracked-files=no >&2; exit 3
fi
DEFBRANCH="$(git symbolic-ref -q --short HEAD || echo DETACHED)"
if [ "$DEFBRANCH" != "main" ]; then
  echo "release-cut: HEAD is on '$DEFBRANCH', expected 'main'. Cut on main." >&2; exit 3
fi
if [ -n "$CL_FILE" ] && [ ! -f "$CL_FILE" ]; then
  echo "release-cut: --changelog-file '$CL_FILE' not found" >&2; exit 3
fi

echo "=== release-cut $TAG (prev $PREV) on $DEFBRANCH ==="

# --- 2. CHANGELOG -------------------------------------------------------------
CL="CHANGELOG.md"; TMP="$(mktemp)"
if [ -n "$CL_FILE" ]; then
  if ! grep -qF "[$VERSION]" "$CL_FILE"; then
    echo "release-cut: --changelog-file does not contain a '## [$VERSION]' heading" >&2
    rm -f "$TMP"; exit 3
  fi
  # If [Unreleased] already has entries, --changelog-file REPLACES them with the draft
  # (the draft is expected to have folded them in), so surface what was there. Shipping
  # merges dropped by mistake are still caught by the coverage oracle below.
  oldbody="$(awk '/^## \[Unreleased\]/{f=1;next} f&&/^## \[/{exit} f{print}' "$CL")"
  if [ -n "$(printf '%s' "$oldbody" | tr -d '[:space:]')" ]; then
    echo "  NOTE: [Unreleased] had entries; the draft REPLACES them (confirm the draft folds them in). Was:" >&2
    printf '%s\n' "$oldbody" | sed 's/^/    | /' >&2
  fi
  awk -v f="$CL_FILE" '
    BEGIN{ buf=""; while((getline line < f)>0) buf=buf line "\n" }
    /^## \[Unreleased\]/ && !done { print; print ""; printf "%s", buf; skip=1; done=1; next }
    skip && /^## \[/ { skip=0; print "" }   # reached the next section: stop dropping, re-add one blank
    skip { next }                            # drop the old [Unreleased] body so it is not duplicated
    { print }
    END{ if(!done){ print "release-cut: no [Unreleased] heading found" > "/dev/stderr"; exit 7 } }
  ' "$CL" > "$TMP" || { echo "release-cut: CHANGELOG insert failed" >&2; rm -f "$TMP"; exit 3; }
else
  # Fold: require non-empty [Unreleased], then rename it and prepend a fresh empty one.
  body="$(awk '/^## \[Unreleased\]/{f=1;next} f&&/^## \[/{exit} f{print}' "$CL" | tr -d '[:space:]')"
  if [ -z "$body" ]; then
    echo "release-cut: [Unreleased] is empty and no --changelog-file given — nothing to release" >&2
    rm -f "$TMP"; exit 3
  fi
  awk -v ver="$VERSION" -v d="$TODAY" '
    /^## \[Unreleased\]/ && !done { print "## [Unreleased]"; print ""; print "## [" ver "] - " d; done=1; next }
    { print }
  ' "$CL" > "$TMP" || { echo "release-cut: CHANGELOG fold failed" >&2; rm -f "$TMP"; exit 3; }
fi
mv "$TMP" "$CL"
echo "  CHANGELOG: [$VERSION] section applied"

# --- 3. Chart.yaml ------------------------------------------------------------
CHART="deploy/chart/Chart.yaml"; TMP="$(mktemp)"
if ! awk -v v="$VERSION" '
  /^version:/    { print "version: " v; next }
  /^appVersion:/ { print "appVersion: \"" v "\""; next }
  { print }
' "$CHART" > "$TMP"; then
  echo "release-cut: Chart.yaml rewrite (awk) failed" >&2; rm -f "$TMP"; exit 1
fi
mv "$TMP" "$CHART"
# awk exits 0 even if NEITHER line matched (e.g. the Chart.yaml format changed), which
# would silently leave the chart unbumped and only surface at tag time via
# release.yml's assert-version, after main is already pushed. Confirm the bump took.
if ! grep -qxF "version: $VERSION" "$CHART" || ! grep -qxF "appVersion: \"$VERSION\"" "$CHART"; then
  echo "release-cut: Chart.yaml bump did not take — version/appVersion not set to $VERSION" >&2
  exit 1
fi
echo "  Chart.yaml: version + appVersion -> $VERSION"

# --- 4. worker-tag autobump ---------------------------------------------------
if ! bash scripts/worker-tag-autobump.sh "$VERSION"; then
  echo "release-cut: worker-tag-autobump.sh failed" >&2; exit 1
fi
# PINNED_TAG is the worker pin, kept in lockstep with values.yaml's workers.image.tag
# by worker-tag-autobump.sh — an unambiguous single line, unlike the several `tag:`
# keys in values.yaml.
WT="$(awk -F'"' '/^PINNED_TAG=/{print $2; exit}' scripts/assert-worker-tag-decoupled.sh 2>/dev/null)"
echo "  worker tag now: ${WT:-<unreadable>} (rolls the fleet iff it changed from the prior pin)"

# --- 5. changelog links -------------------------------------------------------
if ! bash scripts/changelog-links.sh; then
  echo "release-cut: changelog-links.sh failed" >&2; exit 1
fi
echo "  changelog-links: refreshed"

# --- 6/7. commit + verify -----------------------------------------------------
git add -- CHANGELOG.md deploy/chart/Chart.yaml deploy/chart/values.yaml scripts/assert-worker-tag-decoupled.sh 2>/dev/null || true

if [ "$DO_COMMIT" -eq 0 ]; then
  echo
  echo "=== --no-commit: staged, NOT committed (oracle not run) ==="
  git --no-pager diff --cached --stat
  echo "Review, commit as 'chore(release): $TAG', then run the oracle:"
  echo "  bash scripts/assert-changelog-covers-release.sh HEAD $PREV $VERSION"
  exit 0
fi

git commit -q -m "chore(release): $TAG" || { echo "release-cut: commit failed" >&2; exit 1; }

verify_fail() {
  echo "release-cut: VERIFY FAILED — $1" >&2
  echo "  undoing the release commit (git reset --soft HEAD~1); your edits are kept staged." >&2
  git reset --soft HEAD~1
  exit 1
}
if ! bash scripts/assert-changelog-covers-release.sh HEAD "$PREV" "$VERSION"; then
  verify_fail "changelog coverage oracle rejected the release commit"
fi
if ! bash scripts/changelog-links.sh --check; then
  verify_fail "changelog-links --check found stale links (release.yml would reject the tag)"
fi

echo
echo "=== $TAG cut and verified on main (NOT pushed) ==="
echo "Next:"
echo "  git show HEAD                 # review"
echo "  git push origin main         # triggers ci.yml"
echo "  .../watch-run-ci.sh --branch main --workflow ci.yml   # wait for green"
echo "  git tag -a $TAG -m $TAG HEAD  # then: ! git push origin $TAG  (classifier blocks the agent/lead)"
echo "  .../release-watch.sh $VERSION && .../release-verify.sh $VERSION"
exit 0
