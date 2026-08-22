#!/usr/bin/env bash
# Extract one version's section from CHANGELOG.md, for the GitHub Release that
# release.yml's `publish-release` job creates from a `v*` tag.
#
#   changelog-section.sh body  <version>   -> section content (Added/Changed/...)
#                                             heading + release-title marker stripped;
#                                             this becomes the Release NOTES body.
#   changelog-section.sh title <version>   -> "vX.Y.Z: <marker>" if the section
#                                             carries a `<!-- release-title: … -->`
#                                             marker, else "vX.Y.Z".
#
# The CHANGELOG is the single source of truth (curated prose, already gated by
# assert-changelog-covers-release.sh); this script only re-surfaces a section.
#
# The one authored extra per release is an OPTIONAL terse title marker placed on
# the line under the heading, e.g.
#
#     ## [0.46.0] - 2026-08-19
#     <!-- release-title: readable run transcript + all-agents lane -->
#
# It renders invisibly in Markdown, is ignored by the coverage oracle (it carries
# no issue number), and defaults gracefully to the bare tag when absent.
#
# Every grep/awk below reads a FILE and none early-exit under a pipe, so the
# printf|grep -q SIGPIPE flake documented at length in
# assert-changelog-covers-release.sh cannot occur here.
set -euo pipefail

usage() {
  echo "usage: $0 <body|title> <version>" >&2
  exit 2
}

[ $# -eq 2 ] || usage
mode="$1"
version="$2"
file="${UZI_CHANGELOG_FILE:-CHANGELOG.md}"

[ -f "$file" ] || { echo "changelog-section: $file not found" >&2; exit 2; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
section="$work/section.md"

# Content between this version's heading and the next `## [` heading. Same
# extraction assert-changelog-covers-release.sh uses, so the two agree on what a
# "section" is.
awk -v v="$version" '
  $0 ~ "^## \\[" v "\\]" { inside = 1; next }
  inside && /^## \[/     { exit }
  inside                 { print }
' "$file" > "$section"

# Reject an absent or blank-only section (a release must describe itself). awk
# reads the file to EOF, so no early-exit SIGPIPE.
if ! awk 'NF { found = 1 } END { exit found ? 0 : 1 }' "$section"; then
  echo "changelog-section: no non-empty '## [$version]' section in $file" >&2
  exit 1
fi

# The optional terse title marker, if present (first one wins).
marker="$(awk '
  match($0, /^<!--[[:space:]]*release-title:[[:space:]]*/) {
    s = substr($0, RLENGTH + 1)
    sub(/[[:space:]]*-->.*$/, "", s)
    print s
    exit
  }
' "$section")"

case "$mode" in
  title)
    if [ -n "$marker" ]; then
      printf 'v%s: %s\n' "$version" "$marker"
    else
      printf 'v%s\n' "$version"
    fi
    ;;
  body)
    # Drop the marker line, then trim leading and trailing blank lines so the
    # Release body starts and ends on content.
    grep -vE '^<!--[[:space:]]*release-title:.*-->[[:space:]]*$' "$section" > "$work/body.md" || true
    awk '
      { lines[NR] = $0 }
      END {
        start = 1;  while (start <= NR && lines[start] ~ /^[[:space:]]*$/) start++
        end   = NR; while (end   >= 1  && lines[end]   ~ /^[[:space:]]*$/) end--
        for (i = start; i <= end; i++) print lines[i]
      }
    ' "$work/body.md"
    ;;
  *)
    usage
    ;;
esac
