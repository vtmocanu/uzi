#!/usr/bin/env bash
# Keep CHANGELOG.md's links current: Keep-a-Changelog compare-link footers (each
# `## [X.Y.Z]` heading links to its `vPREV...vX.Y.Z` diff) and PRD-aware inline
# linkification of bare `#N` PR/issue refs.
#
#   changelog-links.sh            rewrite CHANGELOG.md in place (idempotent)
#   changelog-links.sh --check    exit 1 (and print the diff) if a rewrite WOULD
#                                 change the file; exit 0 if already current.
#
# Run the plain form in the release commit (before tagging); `--check` runs in
# release.yml's assert-changelog job so a tag whose CHANGELOG links are stale is
# rejected the same way a missing changelog section is.
#
# THE LINKIFY RULE IS AN ALLOW-LIST, SAFE BY DEFAULT. A bare `#N` is linked ONLY
# where the surrounding text makes it unambiguously a uzi PR/issue:
#   * a parenthetical citation — `(#402)` or `(#396, #397)` (uzi's own convention)
#   * an issue/PR label — `issue #114`, `issues #1, #2`, `PR #99`, `pull request #7`
# Everything else stays plain, on purpose. `PRD #400` points at a design doc under
# prds/ (its number is the ORIGINATING issue, not necessarily a mergeable PR), and
# a cross-repo ref like `k8s #119593` points at another project's tracker; linking
# either to /pull/N would be WRONG, and a wrong cross-repo link is worse than none
# — it silently sends the reader to an unrelated uzi PR. The handful of bare-prose
# uzi refs this leaves unlinked (e.g. "closing #221") are an accepted, harmless
# miss; do not widen the rule to catch them at the cost of mislinking foreign refs.
# Linkification is idempotent: an already-linked `[#N](…)` is skipped by the
# `(?<!\[)` lookbehind, and a linked ref no longer opens with `(#` or `label #`.
set -euo pipefail

check=0
if [ "${1:-}" = "--check" ]; then
  check=1
  shift
fi
[ $# -eq 0 ] || { echo "usage: $0 [--check]" >&2; exit 2; }

file="${UZI_CHANGELOG_FILE:-CHANGELOG.md}"
[ -f "$file" ] || { echo "changelog-links: $file not found" >&2; exit 2; }

# The canonical repo URL for the links. Derived from origin so a fork points at
# its own repo; override with UZI_CHANGELOG_REPO_URL (e.g. in a detached CI
# checkout). Only GitHub remotes are understood.
repo_url="${UZI_CHANGELOG_REPO_URL:-}"
if [ -z "$repo_url" ]; then
  origin="$(git remote get-url origin)"
  origin="${origin%.git}"
  case "$origin" in
    git@github.com:*)       slug="${origin#git@github.com:}" ;;
    ssh://git@github.com/*) slug="${origin#ssh://git@github.com/}" ;;
    https://github.com/*)   slug="${origin#https://github.com/}" ;;
    *)
      echo "changelog-links: cannot derive a GitHub repo from origin '$origin'." >&2
      echo "                 Set UZI_CHANGELOG_REPO_URL=https://github.com/OWNER/REPO." >&2
      exit 2
      ;;
  esac
  repo_url="https://github.com/$slug"
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# 1. Strip the existing reference-style footer block (any `[Unreleased]:` /
#    `[X.Y.Z]:` line — they only ever appear as footers) and any trailing blank
#    lines, so the block can be regenerated deterministically.
awk '
  /^\[Unreleased\]:[[:space:]]/             { next }
  /^\[[0-9]+\.[0-9]+\.[0-9]+\]:[[:space:]]/ { next }
  { lines[++n] = $0 }
  END {
    end = n
    while (end >= 1 && lines[end] ~ /^[[:space:]]*$/) end--
    for (i = 1; i <= end; i++) print lines[i]
  }
' "$file" > "$work/stripped.md"

# 2. Inline linkification (allow-list; see header). Slurp the whole file so a
#    citation run that wraps across a line is still recognised.
REPO_URL="$repo_url" perl -0777 -pe '
  my $u = $ENV{REPO_URL};
  my $link = sub { my $s = shift; $s =~ s{(?<!\[)#(\d+)\b}{[#$1]($u/pull/$1)}g; $s };
  s{\((#\d+(?:\s*,\s*#\d+)*)}{"(" . $link->($1)}ge;
  s{\b(issues?|pull\ requests?|PR)(\s+)(#\d+(?:\s*,\s*#\d+)*)}{$1 . $2 . $link->($3)}gie;
' "$work/stripped.md" > "$work/linked.md"

# 3. Regenerate the compare-footer block. Versions in file order (newest first);
#    each links to the diff since the next-older version, the oldest to its tag.
versions=()
while IFS= read -r v; do
  versions+=("$v")
done < <(awk '
  match($0, /^## \[[0-9]+\.[0-9]+\.[0-9]+\]/) {
    s = substr($0, RSTART, RLENGTH); sub(/^## \[/, "", s); sub(/\]$/, "", s)
    print s
  }
' "$work/linked.md")

has_unreleased=0
awk '/^## \[Unreleased\]/ { found = 1 } END { exit found ? 0 : 1 }' "$work/linked.md" && has_unreleased=1

{
  printf '\n'
  if [ "$has_unreleased" -eq 1 ]; then
    if [ "${#versions[@]}" -gt 0 ]; then
      printf '[Unreleased]: %s/compare/v%s...HEAD\n' "$repo_url" "${versions[0]}"
    else
      printf '[Unreleased]: %s/commits/HEAD\n' "$repo_url"
    fi
  fi
  n=${#versions[@]}
  for ((i = 0; i < n; i++)); do
    cur="${versions[$i]}"
    if [ "$((i + 1))" -lt "$n" ]; then
      prev="${versions[$((i + 1))]}"
      printf '[%s]: %s/compare/v%s...v%s\n' "$cur" "$repo_url" "$prev" "$cur"
    else
      printf '[%s]: %s/releases/tag/v%s\n' "$cur" "$repo_url" "$cur"
    fi
  done
} >> "$work/linked.md"

# 4. Apply or check.
if [ "$check" -eq 1 ]; then
  if diff -u "$file" "$work/linked.md" >/dev/null 2>&1; then
    echo "changelog-links: OK ($file inline links + compare footers are current)."
    exit 0
  fi
  echo "changelog-links: $file links/footers are STALE." >&2
  echo "                 Run: scripts/changelog-links.sh   then commit." >&2
  diff -u "$file" "$work/linked.md" >&2 || true
  exit 1
fi

if diff -u "$file" "$work/linked.md" >/dev/null 2>&1; then
  echo "changelog-links: $file already current; no change."
else
  cp "$work/linked.md" "$file"
  echo "changelog-links: updated $file (inline #N links + compare footers)."
fi
