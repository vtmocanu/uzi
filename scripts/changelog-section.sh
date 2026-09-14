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
# RC-first release train (PRD 1265). The CHANGELOG keeps ONE accumulating
# `## [X.Y.Z]` section per stable version: the first RC folds `[Unreleased]` into
# it and later RCs append to it. Accumulation is a STABLE-only property of the
# *Release body*, not of every candidate:
#
#   * a stable tag  vX.Y.Z        -> the FULL section (the accumulated notes).
#   * an RC tag     vX.Y.Z-rc.N   -> only the DELTA since the previous RC of this
#                                    base (vX.Y.Z-rc.(N-1)), so each candidate
#                                    advertises just its own additions. rc.1 has no
#                                    prior RC, so it emits the full section (which
#                                    at rc.1 is only rc.1's content anyway).
#
# So on the stable Release the reader sees the combined rc.1+rc.2+… notes once,
# while each RC Release shows just what that candidate added. The CHANGELOG FILE
# is untouched by this (still one section), so the coverage oracle, the web
# changelog drawer and check-changelog.mjs all read the same accumulated section.
#
# The delta is computed by diffing this base's section against the previous RC
# tag's copy of it (bullet-level: a top-level `- ` list item plus its wrapped
# lines). A bullet is "new in this RC" when its exact text is absent from the
# previous RC's section, so an APPENDED bullet and an AMENDED one (a citation or a
# clause added to an existing bullet) both surface, and an untouched bullet is
# dropped. If the previous RC's content cannot be resolved (offline, tag absent,
# not a git tree) the body FALLS BACK to the full section rather than fail a
# release: the worst case is today's over-inclusive body, never a broken publish.
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
# A tag may be vX.Y.Z-rc.N (PRD 1265): the CHANGELOG section is keyed by the STABLE
# base X.Y.Z, but the Release TITLE keeps the full tag. base strips any -suffix.
base="${version%%-*}"
file="${UZI_CHANGELOG_FILE:-CHANGELOG.md}"

[ -f "$file" ] || { echo "changelog-section: $file not found" >&2; exit 2; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
section="$work/section.md"

# extract_section <version-base> <changelog-content-file> <out> : content between
# this base's heading and the next `## [` heading. Same extraction
# assert-changelog-covers-release.sh uses, so the two agree on what a "section" is.
extract_section() {
  awk -v v="$1" '
    $0 ~ "^## \\[" v "\\]" { inside = 1; next }
    inside && /^## \[/     { exit }
    inside                 { print }
  ' "$2" > "$3"
}

extract_section "$base" "$file" "$section"

# Reject an absent or blank-only section (a release must describe itself). awk
# reads the file to EOF, so no early-exit SIGPIPE.
if ! awk 'NF { found = 1 } END { exit found ? 0 : 1 }' "$section"; then
  echo "changelog-section: no non-empty '## [$base]' section in $file" >&2
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

# strip_marker <in> <out> : drop the release-title marker line.
strip_marker() {
  grep -vE '^<!--[[:space:]]*release-title:.*-->[[:space:]]*$' "$1" > "$2" || true
}

# trim_edges <file> : print the file with leading/trailing blank lines trimmed so
# the Release body starts and ends on content.
trim_edges() {
  awk '
    { lines[NR] = $0 }
    END {
      start = 1;  while (start <= NR && lines[start] ~ /^[[:space:]]*$/) start++
      end   = NR; while (end   >= 1  && lines[end]   ~ /^[[:space:]]*$/) end--
      for (i = start; i <= end; i++) print lines[i]
    }
  ' "$1"
}

# rc_number <version> <base> : echo N iff version is exactly <base>-rc.N (D2),
# else echo nothing. Keeps the "is this a candidate, and which" test in one place.
rc_number() {
  case "$1" in
    "$2"-rc.[0-9]*) printf '%s' "${1##*-rc.}" ;;
    *) : ;;
  esac
}

# resolve_prev_rc_section <prev-tag> <base> <out> : write the previous RC tag's
# `## [base]` section to <out>. Best-effort: fetches the single tag if the tree
# does not already have it, and returns non-zero on ANY failure (not a git tree,
# tag unresolvable, file/section absent) so the caller falls back to the full
# section. Invoked in an `if` condition, so set -e is suspended inside it.
resolve_prev_rc_section() {
  local prev_tag="$1" b="$2" out="$3"
  git rev-parse --is-inside-work-tree >/dev/null 2>&1 || return 1
  if ! git rev-parse -q --verify "refs/tags/$prev_tag^{commit}" >/dev/null 2>&1; then
    git fetch --depth=1 --quiet origin "refs/tags/$prev_tag:refs/tags/$prev_tag" >/dev/null 2>&1 || true
  fi
  git rev-parse -q --verify "refs/tags/$prev_tag^{commit}" >/dev/null 2>&1 || return 1
  git show "$prev_tag:$file" > "$work/prevfile.md" 2>/dev/null || return 1
  extract_section "$b" "$work/prevfile.md" "$out"
  awk 'NF { f = 1 } END { exit f ? 0 : 1 }' "$out" || return 1
  return 0
}

# emit_delta <prev-section> <cur-section> : print the bullets in <cur-section>
# whose exact text is not in <prev-section>, under the `### ` subsection headers
# they fall in (a header is printed once, only when a bullet under it survives).
# A "bullet" is a top-level `- ` item plus its wrapped continuation lines, ended
# by the next `- `/`### `/`## ` line or a blank line. Blocks are keyed by their
# joined lines using a control-char separator built with sprintf (portable across
# BSD awk on macOS and gawk on the runner; no \xNN escape, no regex dialect).
emit_delta() {
  local prev="$1" cur="$2"
  awk '
    BEGIN { SEP = sprintf("%c", 30) }        # 0x1E, cannot appear in CHANGELOG text
    function block_key(   k, i) {
      if (n == 0) return ""
      k = b[1]
      for (i = 2; i <= n; i++) k = k SEP b[i]
      return k
    }
    # pass 1: previous RC section -> record every bullet block in seen[]
    FNR == NR {
      if ($0 ~ /^- /)                       { kk = block_key(); if (kk != "") seen[kk] = 1; n = 1; b[1] = $0 }
      else if ($0 ~ /^### / || $0 ~ /^## /) { kk = block_key(); if (kk != "") seen[kk] = 1; n = 0 }
      else if ($0 ~ /^[[:space:]]*$/)       { kk = block_key(); if (kk != "") seen[kk] = 1; n = 0 }
      else if (n > 0)            { b[++n] = $0 }
      next
    }
    # boundary into the second file: flush the last prev block
    FNR == 1 { kk = block_key(); if (kk != "") seen[kk] = 1; n = 0 }
    # pass 2: current section -> emit surviving bullets under lazy headers
    function flush_cur(   kk, i) {
      if (n == 0) return
      kk = block_key()
      if (!(kk in seen)) {
        # Blank before the header when it is not the first output, and a blank
        # AFTER it, so an emitted subsection reads as the authored loose list
        # (`### X` / blank / bullets), matching the full-section body exactly.
        if (pending != "") { if (emitted) print ""; print pending; print ""; pending = ""; emitted = 1 }
        for (i = 1; i <= n; i++) { print b[i]; emitted = 1 }
      }
      n = 0
    }
    /^### / { flush_cur(); pending = $0; next }
    /^## /  { flush_cur(); pending = ""; next }
    /^- /   { flush_cur(); n = 1; b[1] = $0; next }
    /^[[:space:]]*$/ { flush_cur(); next }
    { if (n > 0) b[++n] = $0 }
    END { flush_cur() }
  ' "$prev" "$cur"
}

case "$mode" in
  title)
    if [ -n "$marker" ]; then
      printf 'v%s: %s\n' "$version" "$marker"
    else
      printf 'v%s\n' "$version"
    fi
    ;;
  body)
    strip_marker "$section" "$work/body.md"

    rcn="$(rc_number "$version" "$base")"
    if [ -n "$rcn" ] && [ "$rcn" -ge 2 ]; then
      prev_tag="v$base-rc.$((rcn - 1))"
      if resolve_prev_rc_section "$prev_tag" "$base" "$work/prev.md"; then
        strip_marker "$work/prev.md" "$work/prev-clean.md"
        delta="$(emit_delta "$work/prev-clean.md" "$work/body.md")"
        if [ -n "$(printf '%s' "$delta" | tr -d '[:space:]')" ]; then
          printf '%s\n' "$delta"
        else
          # A re-spin candidate with no new changelog bullets: say so plainly
          # rather than publish an empty Release body.
          printf '_No changelog changes since %s._\n' "$prev_tag"
        fi
        exit 0
      fi
      # fall through to the full section when the previous RC cannot be resolved
    fi

    # Stable, rc.1, or graceful fallback: the full section, blank edges trimmed.
    trim_edges "$work/body.md"
    ;;
  *)
    usage
    ;;
esac
