#!/usr/bin/env bash
# release-cut.sh — deterministically PREPARE a release on main, RC-first (PRD 1265).
#
# The release train cuts release CANDIDATES by default and PROMOTES a candidate to
# stable in lockstep with cutting the next candidate. This script is the mechanical
# spine: discover the tag state, apply the CHANGELOG section, bump the chart, auto-bump
# the worker tag, refresh links, commit, and verify with the coverage oracle. It does
# NOT push. It tags only the promoted STABLE (locally, on a throwaway release branch);
# the RC tag on main is applied by the lead after ci.yml is green. A tag push publishes
# unattended, so it needs the user's go-ahead (without one the harness classifier blocks it).
#
#   release-cut.sh <X.Y.Z> [VERB] [--changelog-file FILE] [--no-commit] [--prev-tag TAG]
#
#   <X.Y.Z>   the STABLE version a candidate is being cut FOR (leading v optional).
#             The script derives the tag: vX.Y.Z-rc.N by default, vX.Y.Z with --stable.
#
#   VERB (at most one; only meaningful with an RC in flight):
#     (none)          no RC in flight -> cut vX.Y.Z-rc.1. An RC in flight for THIS base
#                     (release-cut B while vB-rc.N exists) -> cut the next candidate
#                     vB-rc.(N+1), folding [Unreleased] into the open [B] section per
#                     subsection (guarded, D3). An RC in flight for
#                     a LOWER base -> REFUSE (exit 3) and print the facts, so the lead
#                     picks a verb.
#     --promote       promote the in-flight RC to stable from its own commit (D5), then
#                     cut vX.Y.Z-rc.1 on main. X.Y.Z must be > the in-flight base.
#     --promote-only  promote the in-flight RC to stable from its own commit (D5) and STOP:
#                     no next candidate, main untouched, whatever main has shipped since
#                     the RC (that work waits for the next plain cut). X.Y.Z must EQUAL
#                     the in-flight base: it names the stable being created. Takes none of
#                     --changelog-file / --no-commit / --prev-tag (there is no main half).
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
  if [ -n "$VERB" ]; then echo "release-cut: only one of --promote/--promote-only/--skip-promote/--stable" >&2; exit 3; fi
  VERB="$1"
}
while [ $# -gt 0 ]; do
  case "$1" in
    --promote|--promote-only|--skip-promote|--stable) set_verb "$1"; shift;;
    --changelog-file) CL_FILE="${2:?}"; shift 2;;
    --no-commit) DO_COMMIT=0; shift;;
    --prev-tag) PREV_OVERRIDE="${2:?}"; shift 2;;
    -h|--help) awk 'NR > 1 { if (/^#/) print; else exit }' "$0"; exit 3;;
    -*) echo "release-cut: unknown flag $1" >&2; exit 3;;
    *) if [ -z "$VERSION" ]; then VERSION="$1"; shift
       else echo "release-cut: unexpected arg: $1" >&2; exit 3; fi ;;
  esac
done
if [ -z "$VERSION" ]; then
  echo "usage: release-cut.sh <X.Y.Z> [--promote|--promote-only|--skip-promote|--stable] [--changelog-file FILE] [--no-commit] [--prev-tag TAG]" >&2
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
# --promote-only has no main half, so every main-half option is meaningless with it. Refuse
# rather than ignore: a lead passing a changelog draft expects a next candidate to be cut.
if [ "$VERB" = "--promote-only" ] && { [ -n "$CL_FILE" ] || [ "$DO_COMMIT" -eq 0 ] || [ -n "$PREV_OVERRIDE" ]; }; then
  echo "release-cut: --promote-only takes no --changelog-file/--no-commit/--prev-tag (it cuts no next candidate and never touches main; use --promote to cut one in lockstep)" >&2; exit 3
fi
TODAY="$(date +%F)"

ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || { echo "not in a git repo" >&2; exit 3; }
cd "$ROOT" || { echo "cannot cd to repo root $ROOT" >&2; exit 3; }
# is_shipping: the shared release-train definition of "does this path ship" (same file the
# coverage oracle uses), so promote-only and the oracle can never disagree about "shipping".
# shellcheck source=scripts/lib/shipping-paths.sh
. "$ROOT/scripts/lib/shipping-paths.sh"

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
# no stable tag AND sits above the highest stable. Bases with a stable tag are promoted and
# ignored. A base at or below the highest stable with no stable of its own was ABANDONED
# (--skip-promote) and is ignored too: no verb can cut it any more (every new base must be
# above the highest stable), and under lockstep a newer candidate always masked it, but a
# promote that cuts no next candidate (--promote-only, or the implicit branch) leaves none,
# and the abandoned base would come back as "in flight" and wedge the next plain cut.
# Grouped per base, N compared numerically, so git's prerelease-vs-base sort order never
# enters (D1).
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
    BEGIN { n = split(stables, arr, " "); for (i = 1; i <= n; i++) { hasstable[arr[i]] = 1; if (hs == "" || gt(arr[i], hs)) hs = arr[i] } }
    {
      tag = $0; b = tag; sub(/^v/, "", b); sub(/-rc\..*$/, "", b)
      rc = tag; sub(/^.*-rc\./, "", rc)
      if (b in hasstable) next
      if (hs != "" && !gt(b, hs)) next   # abandoned: at or below the highest stable
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
# shipping_commits_since <ref> -> count of first-parent commits in <ref>..HEAD that touched
# a SHIPPING path (is_shipping, shared with the oracle). This is what promote-only keys on:
# a docs/skill/prd/build-only commit after the RC must NOT force a next candidate that ships
# nothing (it would leave an empty [X] section and, with an empty [Unreleased], fail the main
# half and roll the stable tag back). Counting raw `git log` commits conflated the two.
shipping_commits_since() {
  local c=0 sha f files
  while IFS= read -r sha; do
    [ -n "$sha" ] || continue
    files="$(git diff --name-only "$sha^1" "$sha" 2>/dev/null || git show --name-only --format= "$sha")"
    while IFS= read -r f; do
      [ -n "$f" ] || continue
      if is_shipping "$f"; then c=$((c + 1)); break; fi
    done <<EOF
$files
EOF
  done <<EOF
$(git log --first-parent --format=%H "$1..HEAD" 2>/dev/null)
EOF
  echo "$c"
}
# rc_tag_on_remote <tag> -> 0 present on origin, 2 absent from origin, 3 origin
# unreachable (network/auth). A promote re-tags the in-flight RC's PUBLISHED agent image
# and folds its notes, so promoting an RC that was never pushed (hence never built by
# release.yml) would ship a stable chart whose workers.image.tag names an image nobody
# built (the 0.83.0-rc.7 incident, 2026-09-19). A local-only tag is indistinguishable from
# a published one via `git tag -l`, so ask the remote directly. `--exit-code` separates
# "absent" (2) from a genuine remote error (anything else) so the two get distinct, honest
# messages -- both still refuse (fail-closed). The fixture's origin is a local path;
# ls-remote works on that too.
rc_tag_on_remote() {
  git ls-remote --exit-code --tags origin "refs/tags/$1" >/dev/null 2>&1
  case $? in
    0) return 0 ;;
    2) return 2 ;;
    *) return 3 ;;
  esac
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
# The cut reads the LOCAL HEAD. A main that is BEHIND origin/main miscounts what shipped since
# the RC (so --promote can take promote-only wrongly) and produces a release commit that can
# never fast-forward onto origin. Refuse here rather than at the push. Ahead is normal (an
# unpushed release commit); no origin/main ref (offline fixture, unreachable origin) skips it.
BEHIND="$(git rev-list --count HEAD..refs/remotes/origin/main 2>/dev/null || echo 0)"
if [ "${BEHIND:-0}" -gt 0 ]; then
  echo "release-cut: local main is $BEHIND commit(s) behind origin/main. Fast-forward first: git merge --ff-only origin/main" >&2; exit 3
fi

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
    echo "    promote it to stable, cut NO next candidate:    release-cut $IB --promote-only"
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
    # --promote-only names the stable it CREATES, so the version must be the in-flight base
    # itself: a next-version argument here means the lead expected a candidate to be cut.
    --promote-only) [ "$cmp_ib" = 0 ] || { echo "release-cut: --promote-only names the stable being created, so it needs the in-flight base $IB (got $VERSION). To also cut v$VERSION-rc.1: release-cut $VERSION --promote." >&2; exit 3; }; OP=promoteonly ;;
    --skip-promote) [ "$cmp_ib" = 1 ] || { echo "release-cut: --skip-promote needs a version ABOVE the in-flight base $IB (got $VERSION)." >&2; exit 3; }; OP=skiprc ;;
  esac
else
  case "$VERB" in
    --promote|--promote-only|--skip-promote) echo "release-cut: no RC is in flight; nothing to ${VERB#--}." >&2; exit 3 ;;
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
  promoteonly)        TAG="v$IB"; CHARTVER="$IB"; BASE="$IB" ;;   # the stable itself; no main half runs
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

# fold_into_open <base>: nextrc: move the [Unreleased] body into the open `## [base]`
# section, subsection by subsection (D3, amended): each `### X` bucket is appended to the
# end of the section's own `### X`, or opens that subsection at the section's end. PRs
# land their entries in [Unreleased], so a re-spin nearly always has some. Guarded, because
# a malformed merge is invisible to the oracle and changelog-links --check: refuses (exit 3,
# CHANGELOG untouched) on text before the first `###`, a subsection name outside Keep a
# Changelog's six, a duplicate `###` in the result, or any line lost or invented.
fold_into_open() {
  local base="$1" tmp; tmp="$(mktemp)"
  awk -v base="$base" '
    function flush(   n, i, lines) {   # append the bucket for the current subsection
      if (sub_ != "" && (sub_ in bucket)) {
        n = split(bucket[sub_], lines, "\n")
        print ""; for (i = 1; i < n; i++) print lines[i]
        used[sub_] = 1
      }
    }
    function trim(s) { sub(/^\n+/, "", s); sub(/\n+$/, "\n", s); return s }
    FNR == NR {
      if ($0 ~ /^## \[Unreleased\]/) { inU = 1; next }
      if (inU && /^## \[/) inU = 0
      if (!inU) next
      if ($0 ~ /^### /) {
        cur = substr($0, 5); sub(/[[:space:]]+$/, "", cur)
        if (cur !~ /^(Added|Changed|Deprecated|Removed|Fixed|Security)$/) { print "release-cut: [Unreleased] has an unknown subsection: ### " cur > "/dev/stderr"; bad = 1 }
        if (!(cur in bucket)) { order[++no] = cur; bucket[cur] = "" }
        next
      }
      if (cur == "") { if ($0 ~ /[^[:space:]]/) { print "release-cut: [Unreleased] has text before its first ### subsection: " $0 > "/dev/stderr"; bad = 1 }; next }
      bucket[cur] = bucket[cur] $0 "\n"
      next
    }
    FNR == 1 {
      if (bad) exit 3
      for (k in bucket) { bucket[k] = trim(bucket[k]); if (bucket[k] !~ /[^[:space:]]/) delete bucket[k] }
    }
    /^## \[Unreleased\]/ && !u { print; print ""; u = 1; skipU = 1; next }
    skipU && /^## \[/ { skipU = 0 }
    skipU { next }
    index($0, "## [" base "]") == 1 && !inB { inB = 1; found = 1; sub_ = ""; print; next }
    inB && /^[[:space:]]*$/ { blanks = blanks $0 "\n"; next }
    inB && (/^### / || /^## \[/) {
      flush()
      if (/^## \[/) {
        for (i = 1; i <= no; i++) if ((order[i] in bucket) && !(order[i] in used)) { print ""; print "### " order[i]; n = split(bucket[order[i]], L, "\n"); print ""; for (j = 1; j < n; j++) print L[j]; used[order[i]] = 1 }
        print ""; blanks = ""; inB = 0; print; next
      }
      printf "%s", blanks; blanks = ""
      sub_ = substr($0, 5); sub(/[[:space:]]+$/, "", sub_); print; next
    }
    inB { printf "%s", blanks; blanks = ""; print; next }
    { print }
    END {
      if (bad) exit 3
      if (!found) { print "release-cut: no open ## [" base "] section" > "/dev/stderr"; exit 3 }
      if (inB) {   # the open section was the last one in the file
        flush()
        for (i = 1; i <= no; i++) if ((order[i] in bucket) && !(order[i] in used)) { print ""; print "### " order[i]; n = split(bucket[order[i]], L, "\n"); print ""; for (j = 1; j < n; j++) print L[j] }
      }
    }
  ' CHANGELOG.md CHANGELOG.md > "$tmp" || { rm -f "$tmp"; echo "release-cut: [Unreleased] fold into [$base] refused; CHANGELOG untouched" >&2; exit 3; }

  # Guard 1: no subsection appears twice in the open section.
  local dups
  dups="$(awk -v base="$base" 'index($0, "## [" base "]") == 1 { s = 1; next } s && /^## \[/ { exit } s && /^### / { print }' "$tmp" | sort | uniq -d)"
  # Guard 2: every non-blank line survives exactly once, nothing invented. `###` lines are
  # excluded: folding a bucket into an existing subsection drops its heading by design.
  local before after
  before="$(grep -v -e '^[[:space:]]*$' -e '^### ' CHANGELOG.md | sort)"
  after="$(grep -v -e '^[[:space:]]*$' -e '^### ' "$tmp" | sort)"
  if [ -n "$dups" ] || [ "$before" != "$after" ] || [ -n "$(awk '/^## \[Unreleased\]/{f=1;next} f&&/^## \[/{exit} f' "$tmp" | tr -d '[:space:]')" ]; then
    echo "release-cut: [Unreleased] fold into [$base] failed its guard (duplicate subsection: ${dups:-none}; line set changed: $([ "$before" = "$after" ] && echo no || echo yes)); CHANGELOG untouched" >&2
    rm -f "$tmp"; exit 3
  fi
  mv "$tmp" CHANGELOG.md
}

# sync_stable_heading <stable-tag>: make main's `## [S] - <date>` heading match the copy in
# the stable tag. Promotion dates the section on the release branch (D3/D5); a promote that
# never touches main (--promote-only, or the implicit promote-only branch) leaves main at the
# RC-cut date, so the next cut of a new base reconciles it here. No-op when the two agree or
# either side lacks the heading. index(), not a regex: the version's dots stay literal.
sync_stable_heading() {
  local tag="$1" sec="${1#v}" want have tmp
  [ -n "$tag" ] || return 0
  want="$(git show "$tag:CHANGELOG.md" 2>/dev/null | awk -v h="## [$sec]" 'index($0, h) == 1 { print; exit }')"
  have="$(awk -v h="## [$sec]" 'index($0, h) == 1 { print; exit }' CHANGELOG.md)"
  if [ -z "$want" ] || [ -z "$have" ] || [ "$want" = "$have" ]; then return 0; fi
  tmp="$(mktemp)"
  if awk -v h="## [$sec]" -v w="$want" 'index($0, h) == 1 && !done { print w; done = 1; next } { print }' CHANGELOG.md > "$tmp"; then
    mv "$tmp" CHANGELOG.md
    echo "  CHANGELOG: [$sec] heading synced to the $tag tag's copy (was '$have')"
  else
    rm -f "$tmp"; echo "release-cut: could not sync the [$sec] heading from $tag" >&2; exit 1
  fi
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
      && echo "release-cut: rolled back the local promote tag $PROMOTED_TAG (the RC cut did not complete); fix the cause and re-run. To promote WITHOUT cutting a next candidate: release-cut $IB --promote-only." >&2
  fi
}
if [ "$OP" = promote ] || [ "$OP" = promoteonly ]; then
  # REFUSE to promote an RC that is not confirmed PUBLISHED on origin. Promotion re-tags
  # the RC's published agent image and folds its notes; an unpushed RC was never built by
  # release.yml, so a promote of it ships a stable chart whose worker pin names an image
  # nobody built (the 0.83.0-rc.7 incident). Fail closed on both "absent" and "cannot
  # reach origin", with distinct messages so a network/auth blip is not read as an unpushed
  # tag. Push the RC, let it publish, THEN promote.
  rc_tag_on_remote "$INFLIGHT"; rtr=$?
  if [ "$rtr" -eq 2 ]; then
    {
      echo "release-cut: $VERB REFUSED -- the in-flight RC $INFLIGHT is not on origin."
      echo "  Promotion re-tags the RC's PUBLISHED agent image and folds its notes; an RC that was never pushed was never built by release.yml, so promoting it would ship a stable chart whose workers.image.tag names an image nobody built (the 0.83.0-rc.7 incident, 2026-09-19)."
      echo "  Push the RC tag, let it publish, THEN promote:"
      echo "    ! git push origin $INFLIGHT"
      echo "    .../release-watch.sh ${INFLIGHT#v} && .../release-verify.sh ${INFLIGHT#v}"
      echo "    release-cut $VERSION $VERB"
    } >&2
    exit 3
  elif [ "$rtr" -ne 0 ]; then
    {
      echo "release-cut: $VERB REFUSED -- could not reach origin to confirm $INFLIGHT is published (git ls-remote failed)."
      echo "  Promoting without confirming the RC is published risks shipping a stable chart pinned to an unbuilt worker image (the 0.83.0-rc.7 incident). Fix connectivity/auth and re-run."
    } >&2
    exit 3
  fi
  # If NOTHING SHIPPING has landed since the RC and [Unreleased] is empty, there is nothing
  # to cut: promote only (D1). main's Chart.yaml then stays at the RC version, harmless. We
  # count SHIPPING first-parent commits, not raw commits: a docs/skill/prd/build-only commit
  # after the RC (e.g. a `docs(...) [skip ci]` anchor refresh) must still take promote-only,
  # not drop through to the main half and fail on an empty [Unreleased] (which would roll the
  # stable tag back). "Shipping" is is_shipping, the same predicate the coverage oracle uses.
  merges="$(shipping_commits_since "$INFLIGHT")"
  unrel="$(changelog_unreleased_body | tr -d '[:space:]')"
  trap promote_guard EXIT
  promote_inflight
  # --promote-only: the lead asked for the stable alone. Stop here whatever main carries:
  # shipping work landed after the RC is deliberately NOT in this stable (it is tagged from
  # the RC commit) and waits for the next plain cut, whose coverage window starts at this
  # stable. main is untouched, so the same two warts as the implicit branch below apply.
  if [ "$OP" = promoteonly ]; then
    PROMOTE_FINALIZED=1
    echo
    echo "=== promoted $PROMOTED_TAG only (--promote-only: no next candidate cut, main untouched) ==="
    if [ "$merges" -gt 0 ] || [ -n "$unrel" ]; then
      echo "  NOT in $PROMOTED_TAG: $merges shipping commit(s) on main since $INFLIGHT; [Unreleased] is $([ -z "$unrel" ] && echo empty || echo non-empty). They ship with the next candidate (release-cut <next X.Y.Z>)."
    fi
    echo "  main is untouched: until the next cut its Chart.yaml stays at the RC version and its [$IB] section keeps the RC-cut date (the next cut syncs it from $PROMOTED_TAG)."
    echo "Next:  git push origin $PROMOTED_TAG"
    exit 0
  fi
  if [ "$merges" -eq 0 ] && [ -z "$unrel" ]; then
    PROMOTE_FINALIZED=1
    echo
    echo "=== promoted $PROMOTED_TAG only (nothing shipping since $INFLIGHT, [Unreleased] empty) ==="
    echo "Next:  git push origin $PROMOTED_TAG"
    exit 0
  fi
fi

# --- CHANGELOG (main half) ----------------------------------------------------
case "$OP" in
  rc1|stable)
    fold_or_insert "$BASE" ;;
  nextrc)
    if ! grep -qE "^## \[$BASE\]" CHANGELOG.md; then
      echo "release-cut: no open '## [$BASE]' section for the next candidate; expected it from the first RC." >&2; exit 3
    fi
    if [ -n "$(changelog_unreleased_body | tr -d '[:space:]')" ]; then
      fold_into_open "$BASE"
      echo "  CHANGELOG: [Unreleased] folded into the open [$BASE] section"
    fi ;;
  skiprc)
    rename_and_fold "$IB" "$BASE" ;;
  promote)
    refresh_section_date "$IB"
    fold_or_insert "$BASE" ;;
esac
echo "  CHANGELOG: [$BASE] section applied ($OP)"
# A cut of a NEW base also reconciles the previous stable's heading date (a no-op unless that
# stable was promoted without touching main). nextrc stays CHANGELOG-free by contract (D3).
[ "$OP" = nextrc ] || sync_stable_heading "$(prev_stable_below "$BASE")"

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
  echo "  .../watch-run-ci.sh --sha \"\$(git rev-parse HEAD)\" --branch main   # validate exact release HEAD, then wait for green"
  echo "  git tag -a $TAG -m $TAG HEAD  # then push it with the user's go-ahead; if the classifier blocks you, hand them: ! git push origin $TAG"
else
  echo "  git show HEAD                 # review"
  echo "  git push origin main         # triggers ci.yml"
  echo "  .../watch-run-ci.sh --sha \"\$(git rev-parse HEAD)\" --branch main   # validate exact release HEAD, then wait for green"
  echo "  git tag -a $TAG -m $TAG HEAD  # then push it with the user's go-ahead; if the classifier blocks you, hand them: ! git push origin $TAG"
fi
echo "  .../release-watch.sh $CHARTVER && .../release-verify.sh $CHARTVER"
exit 0
