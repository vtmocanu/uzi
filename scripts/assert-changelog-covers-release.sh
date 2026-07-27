#!/usr/bin/env bash
# Assert the CHANGELOG section being released actually accounts for every merge
# since the previous tag.
#
# WHY THIS EXISTS. Twice the CHANGELOG has shipped a release that did not describe
# what the release contained:
#
#   - 0.11.7 and 0.11.8 were both tagged with their entries still under
#     `[Unreleased]`, so 0.11.9 had to be restructured to repair two releases of drift.
#   - `agent/issue-60` merged in the window BETWEEN the 0.11.10 release MR being
#     opened and it being merged. It rode into the release with no entry at all,
#     and no review of the release MR could have seen it: it was never in that diff.
#
# THE OBVIOUS CHECK DOES NOT CATCH THIS. "Is `[Unreleased]` empty at tag time?" was
# TRUE in the second case -- nobody wrote an entry, so nothing was left over to
# notice. An empty `[Unreleased]` is equally consistent with "everything is
# recorded" and "someone forgot", so it cannot tell them apart. Coverage has to be
# measured against what actually merged.
#
# NOR DOES "EVERY MERGE MUST TOUCH CHANGELOG.md". That was the first version of
# this script and it was wrong for this repo: entries are written in the RELEASE
# MR, citing issue numbers, not in each feature MR. It passed the case it was
# written for and failed #149, whose entry existed and was simply written
# elsewhere. A gate that cries wolf on the normal workflow gets switched off.
#
# WHAT IT CHECKS. For each first-parent merge in <prev-tag>..<ref> that changed
# shipping code, at least one issue/PRD number from its branch name or commit body
# must appear in the CHANGELOG section for the version being released. That matches
# how entries are actually written here ("(issue #148)", "(PRD #121)", "(#60)").
#
# ESCAPE HATCHES, both deliberate. A merge is exempt if it touched CHANGELOG.md
# itself, or if its message carries `Changelog: none`. Internal refactors are real,
# and a gate with no way to say "genuinely nothing to announce" gets satisfied with
# a junk entry, which is worse than silence.
set -euo pipefail

REF="${1:-HEAD}"
PREV="${2:-}"
VERSION="${3:-}"

if [ -z "$PREV" ]; then
  exact="$(git describe --tags --exact-match "$REF" 2>/dev/null || echo '')"
  if [ -n "$exact" ]; then
    PREV="$(git describe --tags --abbrev=0 --exclude="$exact" "$REF" 2>/dev/null || true)"
  else
    PREV="$(git describe --tags --abbrev=0 "$REF" 2>/dev/null || true)"
  fi
fi

if [ -z "$PREV" ]; then
  echo "assert-changelog-covers-release: no previous tag before $REF; nothing to compare."
  exit 0
fi

# The version being released: the chart's appVersion at REF (Model B keeps chart
# version == appVersion == tag, and publish:assert-version already enforces that).
if [ -z "$VERSION" ]; then
  VERSION="$(git show "$REF:deploy/chart/Chart.yaml" | awk '/^appVersion:/ {gsub(/[":]/,"",$2); print $2; exit}')"
fi

echo "Checking CHANGELOG coverage for $PREV..$REF (version $VERSION)"

# The body of this version's section, so a number cited under an OLDER version
# cannot satisfy a merge that landed in this one.
SECTION="$(git show "$REF:CHANGELOG.md" | awk -v v="$VERSION" '
  $0 ~ "^## \\[" v "\\]" { inside = 1; next }
  inside && /^## \[/     { exit }
  inside                 { print }
')"

if [ -z "$SECTION" ]; then
  echo "FAIL: CHANGELOG.md at $REF has no '## [$VERSION]' section, or it is empty."
  echo "      A release must describe itself before it is tagged."
  exit 1
fi

is_shipping() {
  case "$1" in
    *_test.go|*.test.ts|*.test.tsx|*/testdata/*|*/test/*|e2e/*|fixtures/*) return 1 ;;
    api/*|agent/src/*|controller/*|web/src/*|deploy/chart/*|docs/*) return 0 ;;
    *) return 1 ;;
  esac
}

missing=0
while read -r sha; do
  [ -n "$sha" ] || continue

  subject="$(git log -1 --format=%s "$sha")"
  body="$(git log -1 --format=%B "$sha")"

  printf '%s' "$body" | grep -qiE '^Changelog:[[:space:]]*none' && continue

  # A `docs:`-typed commit declares itself as changing no behaviour, and this repo
  # uses conventional commits consistently. Without this the gate trips on every
  # comments-only edit to a .go file -- `is_shipping` matches on PATH and cannot
  # tell a comment from a statement. Measured against v0.11.8..v0.11.9, where a
  # comments-only commit to upgrade.go was one of two hits.
  printf '%s' "$subject" | grep -qE '^docs(\([^)]*\))?:' && continue

  files="$(git diff --name-only "$sha^1" "$sha" 2>/dev/null || git show --name-only --format= "$sha")"

  touched_changelog=0
  touched_shipping=0
  shipping_example=""
  while read -r f; do
    [ -n "$f" ] || continue
    if [ "$f" = "CHANGELOG.md" ]; then
      touched_changelog=1
    elif is_shipping "$f"; then
      touched_shipping=1
      [ -n "$shipping_example" ] || shipping_example="$f"
    fi
  done <<EOF
$files
EOF

  [ "$touched_shipping" = 1 ] || continue
  [ "$touched_changelog" = 0 ] || continue

  # Issue/PRD numbers this merge claims, from the branch name and the message.
  # `chore/release-0.11.10` yields nothing, by design: the `/<digits>-` form needs
  # digits immediately after the slash.
  refs="$( { printf '%s\n%s\n' "$subject" "$body" | grep -oiE '(issue[-/ ]?|prd[ -]?#?|#|/)[0-9]+' || true; } \
           | grep -oE '[0-9]+' | sort -un )"

  if [ -z "$refs" ]; then
    if [ "$missing" = 0 ]; then printf '\nFAIL: merges not accounted for in the [%s] section:\n\n' "$VERSION"; fi
    printf '  %s  %s\n' "$(git rev-parse --short "$sha")" "$subject"
    printf '        changed %s but cites no issue number, so coverage cannot be verified\n' "$shipping_example"
    missing=$((missing + 1))
    continue
  fi

  found=0
  for n in $refs; do
    if printf '%s' "$SECTION" | grep -qE "#0*$n([^0-9]|$)"; then found=1; break; fi
  done

  if [ "$found" = 0 ]; then
    if [ "$missing" = 0 ]; then printf '\nFAIL: merges not accounted for in the [%s] section:\n\n' "$VERSION"; fi
    printf '  %s  %s\n' "$(git rev-parse --short "$sha")" "$subject"
    printf '        changed %s; none of #%s appears in the section\n' \
      "$shipping_example" "$(printf '%s' "$refs" | tr '\n' ',' | sed 's/,$//; s/,/, #/g')"
    missing=$((missing + 1))
  fi
done <<EOF
$(git log --first-parent --format=%H "$PREV..$REF")
EOF

if [ "$missing" -gt 0 ]; then
  cat <<MSG

Add these to the [$VERSION] section, then re-tag. If a merge genuinely has nothing
to announce, put

    Changelog: none    # and why

in its commit message.

The window this closes: an MR merging WHILE a release MR is open never appears in
that MR's diff, so no amount of reviewing the release MR can surface it.
MSG
  exit 1
fi

echo "OK: every shipping merge since $PREV is cited in the [$VERSION] section"
