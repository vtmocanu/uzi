#!/bin/sh
# Gate specs/ai.md on its FREEZE (issue #1317): the file is read-only history,
# frozen at section 637, and this check is what makes "read-only" enforceable.
#
# usage: scripts/check-spec-numbering.sh <spec-file> <canary-file>
#   e.g. scripts/check-spec-numbering.sh specs/ai.md scripts/spec-numbering-canary.md
#
# Three arms, each read whole-file and fenced-code-block aware:
#   1. no section number appears twice   (catches an in-place renumber)
#   2. no section number exceeds the head (catches an append)
#   3. the section count equals the head  (catches a deleted or merged section;
#      today the numbers are exactly 1..637, so count == head)
# Order and gaps are NEVER checked: the file is appended in landing order and
# carries intentional out-of-order blocks. Both are now permanent.
#
# LIVENESS CANARY, BECAUSE A SILENT PASS IS THE FAILURE MODE. The canary plants a
# duplicate section number AND a section above the frozen head. Arms 1 and 2 must
# both fire on it, or the instrument is declared broken. A clean run prints what
# the canary tripped, so a green is a positive observation rather than "the
# detector never looked".
#
# NO SKIP BRANCH AND NO *_REQUIRED ENV VAR: this needs only sh/git/awk, which are
# always present, so there is nothing to fail open on.
#
# EXIT CODES (the convention lint-yaml.sh / scan-secrets.sh set):
#     2 = the instrument is broken (a file missing, extraction empty, canary did not fire)
#     1 = there are findings (a duplicate, a section above the head, a count mismatch)
#     0 = clean, and both canary arms were seen
# `task`'s own rc is 201 for all of them.
set -eu

FROZEN_HEAD=637

if [ "$#" -lt 2 ]; then
  echo "usage: scripts/check-spec-numbering.sh <spec-file> <canary-file>" >&2
  echo "  e.g. scripts/check-spec-numbering.sh specs/ai.md scripts/spec-numbering-canary.md" >&2
  exit 2
fi

SPEC="$1"
CANARY="$2"

ROOT="$(git rev-parse --show-toplevel)" || {
  echo "check-spec-numbering: not inside a git work tree (git rev-parse --show-toplevel failed)" >&2
  exit 2
}
cd "$ROOT" || {
  echo "check-spec-numbering: cannot cd to repo root: $ROOT" >&2
  exit 2
}

for f in "$SPEC" "$CANARY"; do
  if [ ! -f "$f" ]; then
    echo "check-spec-numbering: file not found: $f (instrument failure, nothing to check)" >&2
    exit 2
  fi
done

# Extraction is all awk (portable: no gawk-only 3-arg match, no ugrep negated-class or
# brace pitfalls). Toggle an "inside fence" flag on a line whose first non-space run is
# a triple backtick or triple tilde and skip while inside. Outside fences, a line
# matching `^## [0-9]+\.` yields `linenumber<TAB>number`; `+ 0` canonicalises the digits
# so `## 07.` and `## 7.` collide as the one section they both read as.
extract() {
  awk '
    {
      t = $0
      sub(/^[ \t]*/, "", t)
      if (t ~ /^```/ || t ~ /^~~~/) { infence = !infence; next }
      if (infence) next
      if ($0 ~ /^## [0-9]+\./) {
        s = substr($0, 4)
        i = index(s, ".")
        print NR "\t" (substr(s, 1, i - 1) + 0)
      }
    }
  ' "$1"
}

# stats <file> -> "dups max count", where dups counts numbers seen more than once.
stats() {
  extract "$1" | awk -F'\t' '
    { c[$2]++; if ($2 + 0 > max) max = $2 + 0 }
    END { d = 0; n = 0; for (k in c) { n++; if (c[k] > 1) d++ }; print d " " max + 0 " " n }'
}

# CANARY FIRST: prove both arms fire before trusting any spec verdict.
canary_stats="$(stats "$CANARY")"
canary_dups="${canary_stats%% *}"; canary_rest="${canary_stats#* }"; canary_max="${canary_rest%% *}"
if [ "$canary_dups" -lt 1 ] || [ "$canary_max" -le "$FROZEN_HEAD" ]; then
  echo "check-spec-numbering: INSTRUMENT BROKEN -- the canary ($CANARY) must carry a duplicate" >&2
  echo "check-spec-numbering: section number AND a section above $FROZEN_HEAD; detector saw" >&2
  echo "check-spec-numbering: duplicates=$canary_dups highest=$canary_max. Restore the canary or fix extract()." >&2
  exit 2
fi

spec_stats="$(stats "$SPEC")"
dups="${spec_stats%% *}"; spec_rest="${spec_stats#* }"; highest="${spec_rest%% *}"; count="${spec_rest#* }"

if [ "$count" -lt 1 ]; then
  echo "check-spec-numbering: INSTRUMENT BROKEN -- no section headings found in $SPEC." >&2
  echo "  Expected lines like '## 455. PRD #88 -- ...'. Extraction matched nothing." >&2
  exit 2
fi

rc=0
if [ "$dups" -gt 0 ]; then
  echo "check-spec-numbering: DUPLICATE section number(s) in $SPEC:" >&2
  extract "$SPEC" | awk -F'\t' '
    { lines[$2] = lines[$2] (lines[$2] == "" ? "" : ", ") $1; cnt[$2]++ }
    END { for (k in cnt) if (cnt[k] > 1) print k "\t" lines[k] }' | sort -n |
  while IFS='	' read -r num where; do
    echo "  section ${num} appears on lines: ${where}" >&2
  done
  rc=1
fi
if [ "$highest" -gt "$FROZEN_HEAD" ]; then
  echo "check-spec-numbering: $SPEC is FROZEN at section $FROZEN_HEAD (issue #1317) but carries section(s) above it:" >&2
  extract "$SPEC" | awk -F'\t' -v h="$FROZEN_HEAD" '$2 + 0 > h { print "  section " $2 " on line " $1 }' >&2
  echo "  Do not append to $SPEC. Record the design in the PRD Decision Log or an ADR." >&2
  rc=1
fi
if [ "$count" -ne "$FROZEN_HEAD" ]; then
  echo "check-spec-numbering: $SPEC is FROZEN with exactly $FROZEN_HEAD sections (issue #1317); found $count." >&2
  echo "  A section was removed or merged. Restore it: the file is read-only history." >&2
  rc=1
fi
[ "$rc" -eq 0 ] || exit "$rc"

echo "check-spec-numbering: clean -- $SPEC frozen at section $FROZEN_HEAD: $count sections, all distinct, none above the head."
echo "check-spec-numbering: canary tripped both arms in $CANARY (duplicates=$canary_dups, highest=$canary_max), so this"
echo "check-spec-numbering: green is a positive observation rather than a check that never looked."
exit 0
