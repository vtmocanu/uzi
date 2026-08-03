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

# MATCH ONLY RELEASE TAGS. `git describe --abbrev=0` returns the nearest tag by
# ANCESTRY, not the nearest release, so any scratch tag between the last release and
# HEAD silently narrows the window this gate checks. Measured 2026-07-28: with
# `prd111-premerge-backup` sitting between v0.11.12 and main, the default run reported
# "OK ... since prd111-premerge-backup" while the release actually spanned 15
# first-parent commits from v0.11.12. It passed anyway that time, which is the point:
# a gate that silently checks less than it claims fails open, and the failure is
# invisible in its own output. `--match 'v[0-9]*'` binds it to the release tags Model B
# defines. Pass $2 explicitly to override.
if [ -z "$PREV" ]; then
  exact="$(git describe --tags --exact-match --match 'v[0-9]*' "$REF" 2>/dev/null || echo '')"
  if [ -n "$exact" ]; then
    PREV="$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude="$exact" "$REF" 2>/dev/null || true)"
  else
    PREV="$(git describe --tags --abbrev=0 --match 'v[0-9]*' "$REF" 2>/dev/null || true)"
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
  #
  # BOTH greps need the `|| true`, and the second one is the easy half to miss:
  # a merge citing no issue number is the case this gate exists to REPORT, and
  # under `set -euo pipefail` an unguarded `grep` returning 1 makes the whole
  # command substitution fail, which aborts an ASSIGNMENT (the POSIX distinction
  # -- a substitution inside a simple command would not). Measured 2026-08-03
  # cutting v0.14.0: the run died at `fix/agent-test-timeout-knife-edge`, whose
  # message carries `!161` and `120000` but no `#`-prefixed number, printing the
  # "Checking ..." header and then exiting 1 with nothing else -- so every merge
  # after it in the window went unchecked and the operator was told nothing. It
  # fails CLOSED, which is why it was survivable, but a gate that reports less
  # than it claims is the exact defect the `--match 'v[0-9]*'` comment above
  # exists to prevent, arriving through the shell instead of through git.
  refs="$( { printf '%s\n%s\n' "$subject" "$body" | grep -oiE '(issue[-/ ]?|prd[ -]?#?|#|/)[0-9]+' || true; } \
           | { grep -oE '[0-9]+' || true; } | sort -un )"

  short="$(git rev-parse --short "$sha")"

  # THE SHORT SHA IS A SECOND, RETROACTIVE WAY TO ACCOUNT FOR A MERGE, and it is
  # the only one that works after the fact. `Changelog: none` lives in the commit
  # MESSAGE, so reaching for it once a merge has landed means amending -- a
  # force-push, which this repo forbids on `main`. That made some release windows
  # simply unreleasable, which contradicts the window this gate exists to close:
  # merges land while a release MR is open, and those are exactly the ones nobody
  # thought to tag with an issue number. Citing the short SHA in the section still
  # forces a human to write down what shipped, which is the whole property; it just
  # does not require having predicted the need before merging.
  #
  # Checked BEFORE the issue-number path so it also rescues a merge that cites the
  # wrong number, and matched with `-F` -- a SHA is a literal, and this file's
  # neighbours are full of patterns that were read as regexes.
  if printf '%s' "$SECTION" | grep -qF "$short"; then continue; fi

  if [ -z "$refs" ]; then
    if [ "$missing" = 0 ]; then printf '\nFAIL: merges not accounted for in the [%s] section:\n\n' "$VERSION"; fi
    printf '  %s  %s\n' "$short" "$subject"
    printf '        changed %s but cites no issue number; cite #<issue> or the short SHA %s in the section\n' \
      "$shipping_example" "$short"
    missing=$((missing + 1))
    continue
  fi

  found=0
  for n in $refs; do
    if printf '%s' "$SECTION" | grep -qE "#0*$n([^0-9]|$)"; then found=1; break; fi
  done

  if [ "$found" = 0 ]; then
    if [ "$missing" = 0 ]; then printf '\nFAIL: merges not accounted for in the [%s] section:\n\n' "$VERSION"; fi
    printf '  %s  %s\n' "$short" "$subject"
    printf '        changed %s; neither #%s nor the short SHA %s appears in the section\n' \
      "$shipping_example" "$(printf '%s' "$refs" | tr '\n' ',' | sed 's/,$//; s/,/, #/g')" "$short"
    missing=$((missing + 1))
  fi
done <<EOF
$(git log --first-parent --format=%H "$PREV..$REF")
EOF

if [ "$missing" -gt 0 ]; then
  cat <<MSG

Add these to the [$VERSION] section, then re-tag, citing either the issue number
(#NNN) or the short SHA printed above. If a merge genuinely has nothing to
announce, put

    Changelog: none    # and why

in its commit message -- but note that only works BEFORE it merges; afterwards the
short-SHA citation is the escape, since amending a merged message needs a
force-push this repo does not allow.

The window this closes: an MR merging WHILE a release MR is open never appears in
that MR's diff, so no amount of reviewing the release MR can surface it.
MSG
  exit 1
fi

echo "OK: every shipping merge since $PREV is cited in the [$VERSION] section"
