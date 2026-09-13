#!/usr/bin/env bash
# release-cut.sh — deterministically PREPARE a release on main, RC-first (PRD 1265).
#
# The release train cuts release CANDIDATES by default and PROMOTES a candidate to
# stable in lockstep with cutting the next candidate. This script is the mechanical
# spine: discover the tag state, apply the CHANGELOG section, bump the chart, auto-bump
# the worker tag, refresh links, commit, and verify with the coverage oracle. It does
# NOT push. It tags only the promoted STABLE (locally, on a throwaway release branch);
# the RC tag on main is applied by the lead after ci.yml is green (the harness classifier
# blocks the agent/lead from pushing a tag anyway).
#
#   release-cut.sh <X.Y.Z> [VERB] [--changelog-file FILE] [--no-commit] [--prev-tag TAG]
#
#   <X.Y.Z>   the STABLE version a candidate is being cut FOR (leading v optional).
#             The script derives the tag: vX.Y.Z-rc.N by default, vX.Y.Z with --stable.
#
#   VERB (at most one; only meaningful with an RC in flight):
#     (none)          no RC in flight -> cut vX.Y.Z-rc.1. An RC in flight for THIS base
#                     (release-cut B while vB-rc.N exists) -> cut the next candidate
#                     vB-rc.(N+1) (requires [Unreleased] empty, D3). An RC in flight for
#                     a LOWER base -> REFUSE (exit 3) and print the facts, so the lead
#                     picks a verb.
#     --promote       promote the in-flight RC to stable from its own commit (D5), then
#                     cut vX.Y.Z-rc.1 on main. X.Y.Z must be > the in-flight base.
#     --skip-promote  abandon the in-flight RC: rename its open section to X.Y.Z, fold
#                     [Unreleased] in, cut vX.Y.Z-rc.1. The RC tags/Releases stay history.
#     --stable        cut a plain vX.Y.Z from main's tip (the old one-step model). An
#                     escape for a docs-only release; REFUSED while an RC is in flight.
#
#   --changelog-file  a drafted `## [X.Y.Z]` section that REPLACES the [Unreleased] fold.
#   --no-commit       stage every edit but do not commit (and so do not run the oracle).
#                     Applies to the main half only; rejected with --promote.
#   --prev-tag TAG    override the derived coverage-window base tag (a repair).
#
# The prerelease form is exactly vX.Y.Z-rc.N (N from 1); nothing else (D2). Tag discovery
# never sorts a mixed stable/prerelease list: stable tags are filtered first and the
# in-flight RC is found by grouping per base and comparing N numerically (D1).
#
# Exit codes: 0 ready;  1 a step/verify failed (tree left recoverable);  3 usage /
#             precondition failure, or a refusal that needs a verb.
#
# NEVER skip-ci a release commit (release.yml assumes ci.yml is green on it).
set -uo pipefail

VERSION=""; VERB=""; CL_FILE=""; DO_COMMIT=1; PREV_OVERRIDE=""
set_verb() {
  if [ -n "$VERB" ]; then echo "release-cut: only one of --promote/--skip-promote/--stable" >&2; exit 3; fi
  VERB="$1"
}
while [ $# -gt 0 ]; do
  case "$1" in
    --promote|--skip-promote|--stable) set_verb "$1"; shift;;
    --changelog-file) CL_FILE="${2:?}"; shift 2;;
    --no-commit) DO_COMMIT=0; shift;;
    --prev-tag) PREV_OVERRIDE="${2:?}"; shift 2;;
    -h|--help) sed -n '2,45p' "$0"; exit 3;;
    -*) echo "release-cut: unknown flag $1" >&2; exit 3;;
    *) if [ -z "$VERSION" ]; then VERSION="$1"; shift
       else echo "release-cut: unexpected arg: $1" >&2; exit 3; fi ;;
  esac
done
if [ -z "$VERSION" ]; then
  echo "usage: release-cut.sh <X.Y.Z> [--promote|--skip-promote|--stable] [--changelog-file FILE] [--no-commit] [--prev-tag TAG]" >&2
  exit 3
fi
VERSION="${VERSION#v}"
# The INPUT is always a stable base X.Y.Z (the version a candidate is cut FOR); the tag
# shape is derived. Reject a prerelease/other suffix on the input (D2).
if ! [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "release-cut: '$VERSION' is not a stable X.Y.Z version (pass the base; the RC suffix is derived)" >&2; exit 3
fi
if [ "$DO_COMMIT" -eq 0 ] && [ "$VERB" = "--promote" ]; then
  echo "release-cut: --no-commit is rejected with --promote (the promote half tags unconditionally)" >&2; exit 3
fi
TODAY="$(date +%F)"

ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || { echo "not in a git repo" >&2; exit 3; }
cd "$ROOT" || { echo "cannot cd to repo root $ROOT" >&2; exit 3; }

# --- version helpers ----------------------------------------------------------
# ver_cmp <a> <b> -> -1|0|1 for a<b|a==b|a>b, comparing the X.Y.Z base only.
ver_cmp() {
  awk -v a="${1%%-*}" -v b="${2%%-*}" 'BEGIN{
    split(a, x, "\\."); split(b, y, "\\.")
    for (i = 1; i <= 3; i++) { if (x[i]+0 < y[i]+0) { print -1; exit } if (x[i]+0 > y[i]+0) { print 1; exit } }
    print 0 }'
}
# highest_stable -> the highest vX.Y.Z tag (no prerelease), or empty.
highest_stable() {
  git tag -l 'v*' | awk '
    function gt(x, y,   ax, ay) {
      split(x, ax, "\\."); split(y, ay, "\\.")
      if (ax[1]+0 != ay[1]+0) return ax[1]+0 > ay[1]+0
      if (ax[2]+0 != ay[2]+0) return ax[2]+0 > ay[2]+0
      return ax[3]+0 > ay[3]+0
    }
    /^v[0-9]+\.[0-9]+\.[0-9]+$/ { v = $0; sub(/^v/, "", v); if (best == "" || gt(v, best)) best = v }
    END { if (best != "") print "v" best }'
}
# inflight_rc -> the in-flight RC tag: the highest-versioned vX.Y.Z-rc.N whose base has
# no stable tag. Bases with a stable tag are promoted and ignored (abandoned lower RCs of
# an abandoned base are ignored too, never an error). Grouped per base, N compared
# numerically, so git's prerelease-vs-base sort order never enters (D1).
inflight_rc() {
  local stables
  stables="$(git tag -l 'v*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sed 's/^v//' | tr '\n' ' ')"
  git tag -l 'v*-rc.*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+-rc\.[0-9]+$' | awk -v stables="$stables" '
    function gt(x, y,   ax, ay) {
      split(x, ax, "\\."); split(y, ay, "\\.")
      if (ax[1]+0 != ay[1]+0) return ax[1]+0 > ay[1]+0
      if (ax[2]+0 != ay[2]+0) return ax[2]+0 > ay[2]+0
      return ax[3]+0 > ay[3]+0
    }
    BEGIN { n = split(stables, arr, " "); for (i = 1; i <= n; i++) hasstable[arr[i]] = 1 }
    {
      tag = $0; b = tag; sub(/^v/, "", b); sub(/-rc\..*$/, "", b)
      rc = tag; sub(/^.*-rc\./, "", rc)
      if (b in hasstable) next
      if (bestb == "" || gt(b, bestb) || (b == bestb && rc+0 > bestn+0)) { bestb = b; bestn = rc+0; besttag = tag }
    }
    END { if (besttag != "") print besttag }'
}
# prev_stable_below <base> -> the highest STABLE tag strictly below base (the coverage
# window base, D4). Same rule the oracle uses.
prev_stable_below() {
  git tag -l 'v*' | awk -v base="$1" '
    function lt(x, y,   ax, ay) {
      split(x, ax, "\\."); split(y, ay, "\\.")
      if (ax[1]+0 != ay[1]+0) return ax[1]+0 < ay[1]+0
      if (ax[2]+0 != ay[2]+0) return ax[2]+0 < ay[2]+0
      return ax[3]+0 < ay[3]+0
    }
    /^v[0-9]+\.[0-9]+\.[0-9]+$/ { v = $0; sub(/^v/, "", v); if (lt(v, base) && (best == "" || lt(best, v))) best = v }
    END { if (best != "") print "v" best }'
}
# changelog_unreleased_body -> the body between `## [Unreleased]` and the next `## [`.
changelog_unreleased_body() {
  awk '/^## \[Unreleased\]/{f=1;next} f&&/^## \[/{exit} f{print}' CHANGELOG.md
}

# --- preconditions ------------------------------------------------------------
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
if [ -n "$PREV_OVERRIDE" ] && ! git rev-parse -q --verify "refs/tags/$PREV_OVERRIDE" >/dev/null; then
  echo "release-cut: --prev-tag '$PREV_OVERRIDE' does not exist" >&2; exit 3
fi
# Best-effort tag refresh so discovery sees pushed tags; harmless (and skipped) with no
# reachable origin, e.g. an offline fixture repo.
git fetch --tags --quiet origin >/dev/null 2>&1 || true

# --- discovery ----------------------------------------------------------------
S="$(highest_stable)"
INFLIGHT="$(inflight_rc)"
IB=""; IN=""
if [ -n "$INFLIGHT" ]; then
  IB="${INFLIGHT#v}"; IB="${IB%%-rc.*}"
  IN="${INFLIGHT##*-rc.}"
fi

print_inflight_facts() {
  local cnt tagdate unrel
  cnt="$(git log --first-parent --format=%H "$INFLIGHT..HEAD" 2>/dev/null | grep -c . || true)"
  tagdate="$(git log -1 --format=%ci "$INFLIGHT" 2>/dev/null || echo '?')"
  unrel="$(changelog_unreleased_body | tr -d '[:space:]')"
  {
    echo "release-cut: an RC is in flight — $INFLIGHT (tagged $tagdate)."
    echo "  $cnt first-parent merge(s) on main since it; [Unreleased] is $([ -z "$unrel" ] && echo empty || echo non-empty)."
    echo "  Pick one:"
    echo "    promote it to stable and cut v$VERSION-rc.1:   release-cut $VERSION --promote"
    echo "    keep testing this base with another candidate:  release-cut $IB"
    echo "    abandon it and cut v$VERSION-rc.1 instead:      release-cut $VERSION --skip-promote"
  } >&2
}

# --- routing: decide OP -------------------------------------------------------
OP=""
if [ -n "$INFLIGHT" ]; then
  [ "$VERB" != "--stable" ] || { echo "release-cut: --stable is refused while $INFLIGHT is in flight. Promote or continue the RC train." >&2; exit 3; }
  cmp_ib="$(ver_cmp "$VERSION" "$IB")"
  case "$VERB" in
    "")
      if [ "$cmp_ib" = 0 ]; then OP=nextrc
      elif [ "$cmp_ib" = 1 ]; then print_inflight_facts; exit 3
      else echo "release-cut: $VERSION is below the in-flight candidate $INFLIGHT; refusing." >&2; exit 3; fi ;;
    --promote)      [ "$cmp_ib" = 1 ] || { echo "release-cut: --promote needs a version ABOVE the in-flight base $IB (got $VERSION)." >&2; exit 3; }; OP=promote ;;
    --skip-promote) [ "$cmp_ib" = 1 ] || { echo "release-cut: --skip-promote needs a version ABOVE the in-flight base $IB (got $VERSION)." >&2; exit 3; }; OP=skiprc ;;
  esac
else
  case "$VERB" in
    --promote|--skip-promote) echo "release-cut: no RC is in flight; nothing to ${VERB#--}." >&2; exit 3 ;;
    --stable) OP=stable ;;
    "") OP=rc1 ;;
  esac
fi
# Any cut of a NEW base must be above the highest stable (nextrc stays on its own base).
if [ "$OP" != "nextrc" ] && [ -n "$S" ] && [ "$(ver_cmp "$VERSION" "${S#v}")" != 1 ]; then
  echo "release-cut: $VERSION must be above the highest stable tag $S." >&2; exit 3
fi

# --- resolve TAG / chart version / base / prev --------------------------------
case "$OP" in
  rc1|skiprc|promote) TAG="v$VERSION-rc.1"; CHARTVER="$VERSION-rc.1"; BASE="$VERSION" ;;
  nextrc)             RCN=$((IN + 1)); TAG="v$IB-rc.$RCN"; CHARTVER="$IB-rc.$RCN"; BASE="$IB" ;;
  stable)             TAG="v$VERSION"; CHARTVER="$VERSION"; BASE="$VERSION" ;;
esac

# --- shared step helpers ------------------------------------------------------
fold_or_insert() { # writes the [BASE] section into CHANGELOG (fold [Unreleased] or --changelog-file)
  local base="$1" tmp; tmp="$(mktemp)"
  if [ -n "$CL_FILE" ]; then
    if ! grep -qF "[$base]" "$CL_FILE"; then
      echo "release-cut: --changelog-file does not contain a '## [$base]' heading" >&2; rm -f "$tmp"; exit 3
    fi
    local oldbody; oldbody="$(changelog_unreleased_body)"
    if [ -n "$(printf '%s' "$oldbody" | tr -d '[:space:]')" ]; then
      echo "  NOTE: [Unreleased] had entries; the draft REPLACES them (confirm the draft folds them in)." >&2
    fi
    awk -v f="$CL_FILE" '
      BEGIN{ buf=""; while((getline line < f)>0) buf=buf line "\n" }
      /^## \[Unreleased\]/ && !done { print; print ""; printf "%s", buf; skip=1; done=1; next }
      skip && /^## \[/ { skip=0; print "" }
      skip { next }
      { print }
      END{ if(!done){ print "release-cut: no [Unreleased] heading found" > "/dev/stderr"; exit 7 } }
    ' CHANGELOG.md > "$tmp" || { echo "release-cut: CHANGELOG insert failed" >&2; rm -f "$tmp"; exit 3; }
  else
    local body; body="$(changelog_unreleased_body | tr -d '[:space:]')"
    if [ -z "$body" ]; then
      echo "release-cut: [Unreleased] is empty and no --changelog-file given — nothing to release" >&2; rm -f "$tmp"; exit 3
    fi
    awk -v ver="$base" -v d="$TODAY" '
      /^## \[Unreleased\]/ && !done { print "## [Unreleased]"; print ""; print "## [" ver "] - " d; done=1; next }
      { print }
    ' CHANGELOG.md > "$tmp" || { echo "release-cut: CHANGELOG fold failed" >&2; rm -f "$tmp"; exit 3; }
  fi
  mv "$tmp" CHANGELOG.md
}

rename_and_fold() { # skiprc: rename `## [IB]`->`## [NEW]` (date today), move [Unreleased] body under it
  local from="$1" to="$2" tmp bodyf; tmp="$(mktemp)"; bodyf="$(mktemp)"
  # The [Unreleased] body goes through a FILE, never `awk -v`: BSD awk rejects a newline
  # in a -v value ("newline in string"). getline reads it when the renamed heading lands.
  changelog_unreleased_body > "$bodyf"
  awk -v from="$from" -v to="$to" -v d="$TODAY" -v bodyf="$bodyf" '
    /^## \[Unreleased\]/ && !u { print "## [Unreleased]"; print ""; u=1; skipu=1; next }
    skipu && /^## \[/ { skipu=0 }
    skipu { next }
    $0 ~ "^## \\[" from "\\]" && !r {
      print "## [" to "] - " d
      nb = 0; hasbody = 0
      while ((getline line < bodyf) > 0) { buf[++nb] = line; if (line ~ /[^[:space:]]/) hasbody = 1 }
      close(bodyf)
      while (nb > 0 && buf[nb] ~ /^[[:space:]]*$/) nb--   # trim trailing blanks
      if (hasbody) { print ""; for (i = 1; i <= nb; i++) print buf[i] }
      r = 1; next
    }
    { print }
  ' CHANGELOG.md > "$tmp" || { rm -f "$tmp" "$bodyf"; echo "release-cut: skip-promote rename/fold failed" >&2; exit 1; }
  mv "$tmp" CHANGELOG.md; rm -f "$bodyf"
}

refresh_section_date() { # set `## [SEC] - <today>` for the first [SEC] heading
  local sec="$1" tmp; tmp="$(mktemp)"
  awk -v sec="$sec" -v d="$TODAY" '
    $0 ~ "^## \\[" sec "\\]" && !done { print "## [" sec "] - " d; done=1; next }
    { print }
  ' CHANGELOG.md > "$tmp" && mv "$tmp" CHANGELOG.md
}

bump_chart() { # set version + appVersion to $1 and verify the bump took
  local ver="$1" tmp; tmp="$(mktemp)"
  if ! awk -v v="$ver" '
    /^version:/    { print "version: " v; next }
    /^appVersion:/ { print "appVersion: \"" v "\""; next }
    { print }
  ' deploy/chart/Chart.yaml > "$tmp"; then
    echo "release-cut: Chart.yaml rewrite failed" >&2; rm -f "$tmp"; exit 1
  fi
  mv "$tmp" deploy/chart/Chart.yaml
  if ! grep -qxF "version: $ver" deploy/chart/Chart.yaml || ! grep -qxF "appVersion: \"$ver\"" deploy/chart/Chart.yaml; then
    echo "release-cut: Chart.yaml bump did not take — version/appVersion not set to $ver" >&2; exit 1
  fi
}

run_autobump() { # $1 = version being cut (full, incl -rc.N)
  bash scripts/worker-tag-autobump.sh "$1" || { echo "release-cut: worker-tag-autobump.sh failed" >&2; exit 1; }
}
run_links() {
  bash scripts/changelog-links.sh || { echo "release-cut: changelog-links.sh failed" >&2; exit 1; }
}

# --- promote (D5): tag a stable vIB from a throwaway release branch -----------
promote_inflight() {
  local relbranch="release/$IB" tagB="v$IB" wt
  if git rev-parse -q --verify "refs/heads/$relbranch" >/dev/null; then
    echo "release-cut: branch $relbranch already exists (aborted prior promotion?). Remove it deliberately and re-run." >&2; exit 3
  fi
  if git rev-parse -q --verify "refs/tags/$tagB" >/dev/null; then
    echo "release-cut: tag $tagB already exists; the in-flight base is already promoted." >&2; exit 3
  fi
  wt="$(mktemp -d)"
  if ! git worktree add --quiet -b "$relbranch" "$wt" "$INFLIGHT"; then
    echo "release-cut: could not create promote worktree at $INFLIGHT" >&2; rm -rf "$wt"; exit 1
  fi
  cleanup_promote() { git worktree remove --force "$wt" >/dev/null 2>&1 || true; git branch -D "$relbranch" >/dev/null 2>&1 || true; rm -rf "$wt"; }
  (
    cd "$wt" || exit 1
    awk -v v="$IB" '/^version:/{print "version: " v;next} /^appVersion:/{print "appVersion: \"" v "\"";next} {print}' deploy/chart/Chart.yaml > c.tmp && mv c.tmp deploy/chart/Chart.yaml
    grep -qxF "version: $IB" deploy/chart/Chart.yaml || { echo "promote: Chart.yaml bump did not take" >&2; exit 1; }
    awk -v sec="$IB" -v d="$TODAY" '$0 ~ "^## \\[" sec "\\]" && !done { print "## [" sec "] - " d; done=1; next } {print}' CHANGELOG.md > cl.tmp && mv cl.tmp CHANGELOG.md
    bash scripts/worker-tag-autobump.sh "$IB" || exit 1
    bash scripts/changelog-links.sh || exit 1
    git add -- CHANGELOG.md deploy/chart/Chart.yaml deploy/chart/values.yaml scripts/assert-worker-tag-decoupled.sh >/dev/null 2>&1 || true
    git commit -q -m "chore(release): $tagB (promotes $INFLIGHT)" || exit 1
  ) || { echo "release-cut: promote commit failed" >&2; cleanup_promote; exit 1; }

  # Assert the promote commit changed ONLY allowlisted files, BEFORE any tag exists.
  local changed f
  changed="$(git -C "$wt" diff --name-only "$INFLIGHT..HEAD")"
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    case "$f" in
      CHANGELOG.md|deploy/chart/Chart.yaml|deploy/chart/values.yaml|scripts/assert-worker-tag-decoupled.sh) ;;
      *) echo "release-cut: promote commit changed a non-allowlisted file: $f — aborting before tagging." >&2; cleanup_promote; exit 1 ;;
    esac
  done <<EOF
$changed
EOF

  if ! git tag -a "$tagB" -m "$tagB (promotes $INFLIGHT)" "$relbranch"; then
    echo "release-cut: could not create tag $tagB" >&2; cleanup_promote; exit 1
  fi
  cleanup_promote
  echo "  promoted: $tagB tagged locally from $INFLIGHT (release branch removed; tag holds the commit)"
  PROMOTED_TAG="$tagB"
}

# --- verify + commit the main half --------------------------------------------
verify_fail() {
  echo "release-cut: VERIFY FAILED — $1" >&2
  echo "  undoing the release commit (git reset --soft HEAD~1); your edits are kept staged." >&2
  git reset --soft HEAD~1
  exit 1
}
commit_and_verify() { # $1 tag  $2 prev  $3 base
  git commit -q -m "chore(release): $1" || { echo "release-cut: commit failed" >&2; exit 1; }
  if ! bash scripts/assert-changelog-covers-release.sh HEAD "$2" "$3"; then
    verify_fail "changelog coverage oracle rejected the release commit"
  fi
  if ! bash scripts/changelog-links.sh --check; then
    verify_fail "changelog-links --check found stale links (release.yml would reject the tag)"
  fi
}

echo "=== release-cut $TAG (op=$OP) on $DEFBRANCH ==="

# --- promote half (before touching main) --------------------------------------
PROMOTED_TAG=""; PROMOTE_FINALIZED=0
# The promote half tags a LOCAL (unpushed) stable vB before the main half runs. If the
# main half then fails (an empty [Unreleased] with no --changelog-file, a bad oracle,
# anything), roll that tag back so a re-run starts clean instead of refusing with "tag
# vB already exists". The tag is not pushed, so deleting it loses nothing; a successful
# run sets PROMOTE_FINALIZED=1 first so the guard is a no-op.
promote_guard() {
  local rc=$?
  if [ "$rc" -ne 0 ] && [ -n "$PROMOTED_TAG" ] && [ "$PROMOTE_FINALIZED" -eq 0 ]; then
    git tag -d "$PROMOTED_TAG" >/dev/null 2>&1 \
      && echo "release-cut: rolled back the local promote tag $PROMOTED_TAG (the RC cut did not complete); fix the cause and re-run." >&2
  fi
}
if [ "$OP" = promote ]; then
  # If main has not moved since the RC and [Unreleased] is empty, there is nothing to
  # cut: promote only (D1). main's Chart.yaml then stays at the RC version, harmless.
  merges="$(git log --first-parent --format=%H "$INFLIGHT..HEAD" 2>/dev/null | grep -c . || true)"
  unrel="$(changelog_unreleased_body | tr -d '[:space:]')"
  trap promote_guard EXIT
  promote_inflight
  if [ "$merges" -eq 0 ] && [ -z "$unrel" ]; then
    PROMOTE_FINALIZED=1
    echo
    echo "=== promoted $PROMOTED_TAG only (nothing shipped since $INFLIGHT, [Unreleased] empty) ==="
    echo "Next:  git push origin $PROMOTED_TAG"
    exit 0
  fi
fi

# --- CHANGELOG (main half) ----------------------------------------------------
case "$OP" in
  rc1|stable)
    fold_or_insert "$BASE" ;;
  nextrc)
    body="$(changelog_unreleased_body | tr -d '[:space:]')"
    if [ -n "$body" ]; then
      echo "release-cut: [Unreleased] must be EMPTY to cut $TAG (write re-spin entries into the open ## [$BASE] section). Was:" >&2
      changelog_unreleased_body | sed 's/^/    | /' >&2
      exit 3
    fi
    if ! grep -qE "^## \[$BASE\]" CHANGELOG.md; then
      echo "release-cut: no open '## [$BASE]' section for the next candidate; expected it from the first RC." >&2; exit 3
    fi ;;
  skiprc)
    rename_and_fold "$IB" "$BASE" ;;
  promote)
    refresh_section_date "$IB"
    fold_or_insert "$BASE" ;;
esac
echo "  CHANGELOG: [$BASE] section applied ($OP)"

# --- Chart.yaml + autobump + links --------------------------------------------
bump_chart "$CHARTVER"
echo "  Chart.yaml: version + appVersion -> $CHARTVER"
run_autobump "$CHARTVER"
WT="$(awk -F'"' '/^PINNED_TAG=/{print $2; exit}' scripts/assert-worker-tag-decoupled.sh 2>/dev/null)"
echo "  worker tag now: ${WT:-<unreadable>} (rolls the fleet iff it changed from the prior pin)"
run_links
echo "  changelog-links: refreshed"

# --- PREV for the oracle ------------------------------------------------------
if [ -n "$PREV_OVERRIDE" ]; then
  PREV="$PREV_OVERRIDE"
else
  PREV="$(prev_stable_below "$BASE")"
fi

git add -- CHANGELOG.md deploy/chart/Chart.yaml deploy/chart/values.yaml scripts/assert-worker-tag-decoupled.sh 2>/dev/null || true

if [ "$DO_COMMIT" -eq 0 ]; then
  echo
  echo "=== --no-commit: staged, NOT committed (oracle not run) ==="
  git --no-pager diff --cached --stat
  echo "Review, commit as 'chore(release): $TAG', then run the oracle:"
  echo "  bash scripts/assert-changelog-covers-release.sh HEAD ${PREV:-<prev>} $BASE"
  exit 0
fi

if [ -z "$PREV" ]; then
  # First release ever: the oracle self-skips with no previous tag. Commit without it.
  git commit -q -m "chore(release): $TAG" || { echo "release-cut: commit failed" >&2; exit 1; }
  if ! bash scripts/changelog-links.sh --check; then verify_fail "changelog-links --check found stale links"; fi
else
  commit_and_verify "$TAG" "$PREV" "$BASE"
fi

PROMOTE_FINALIZED=1   # the RC cut committed and verified; the promote tag (if any) stands
echo
echo "=== $TAG cut and verified on main (NOT pushed) ==="
echo "Next:"
if [ -n "$PROMOTED_TAG" ]; then
  echo "  git show HEAD                       # review the RC commit"
  echo "  # push order (D5): STABLE tag, then main, then the RC tag —"
  echo "  git push origin $PROMOTED_TAG"
  echo "  git push origin main               # triggers ci.yml"
  echo "  .../watch-run-ci.sh --branch main --workflow ci.yml   # wait for green"
  echo "  git tag -a $TAG -m $TAG HEAD  # then: ! git push origin $TAG  (classifier blocks the agent/lead)"
else
  echo "  git show HEAD                 # review"
  echo "  git push origin main         # triggers ci.yml"
  echo "  .../watch-run-ci.sh --branch main --workflow ci.yml   # wait for green"
  echo "  git tag -a $TAG -m $TAG HEAD  # then: ! git push origin $TAG  (classifier blocks the agent/lead)"
fi
echo "  .../release-watch.sh $CHARTVER && .../release-verify.sh $CHARTVER"
exit 0
