#!/bin/sh
# Gate specs/ai.md on section-number UNIQUENESS, whole-file (issue #181).
#
# usage: scripts/check-spec-numbering.sh <spec-file> <canary-file>
#   e.g. scripts/check-spec-numbering.sh specs/ai.md scripts/spec-numbering-canary.md
#
# 🔴 UNIQUENESS ONLY -- NEVER ORDER, GAPS OR CONTIGUITY. specs/ai.md is
# append-numbered and DELIBERATELY NOT SORTED BY SECTION NUMBER: the file says so
# at specs/ai.md:15084 ("New sections are APPENDED AT THE TAIL, in LANDING order
# ... which is not a mistake and must not be 'tidied'"), and its gaps (e.g. the
# buried §403-416 / §447-454 blocks) are intentional too. So this check asserts one
# thing: that no section number appears twice. A check that flagged out-of-order
# placement or a gap would be a PERMANENT false positive on that intentional layout,
# which is why the extraction below reports duplicates and nothing else.
#
# 🔴 A LIVENESS CANARY, BECAUSE A SILENT PASS IS THE FAILURE MODE. If the awk
# extraction ever matches nothing (a pattern change, a bad edit), the spec would
# read "0 duplicates" and this gate would pass VACUOUSLY. The canary file carries a
# deliberately planted duplicate; the identical extraction is run over it and MUST
# find that duplicate, or the instrument is declared broken. A clean run therefore
# PRINTS the canary duplicate it detected -- a positive observation, so a green here
# is not "the detector never looked".
#
# NO SKIP BRANCH AND NO *_REQUIRED ENV VAR, deliberately -- the asymmetry with
# lint-yaml.sh / lint-formula.sh is that those wrap brew/pip tools a contributor may
# lack, whereas this check needs only sh/git/awk/sort, which are always present.
# There is nothing to fail open on, so there is no fail-open branch to guard.
#
# EXIT CODES (the convention lint-yaml.sh / scan-secrets.sh set):
#     2 = the instrument is broken (a file missing, spec extraction empty, or the
#         canary duplicate did not fire)
#     1 = there are findings (a section number appears more than once)
#     0 = clean, and the canary duplicate was seen
# `task`'s own rc is 201 for all of them.
set -eu

if [ "$#" -lt 2 ]; then
  echo "usage: scripts/check-spec-numbering.sh <spec-file> <canary-file>" >&2
  echo "  e.g. scripts/check-spec-numbering.sh specs/ai.md scripts/spec-numbering-canary.md" >&2
  exit 2
fi

SPEC="$1"
CANARY="$2"

# Run from the repo root whatever the caller's directory, like the sibling scripts.
ROOT="$(git rev-parse --show-toplevel)" || {
  echo "check-spec-numbering: not inside a git work tree (git rev-parse --show-toplevel failed)" >&2
  exit 2
}
cd "$ROOT" || {
  echo "check-spec-numbering: cannot cd to repo root: $ROOT" >&2
  exit 2
}

if [ ! -f "$SPEC" ]; then
  echo "check-spec-numbering: spec file not found: $SPEC" >&2
  echo "  Without it there is nothing to check; this is an instrument failure." >&2
  exit 2
fi
if [ ! -f "$CANARY" ]; then
  echo "check-spec-numbering: canary file not found: $CANARY" >&2
  echo "  The canary is what proves the duplicate detector fires; without it a" >&2
  echo "  clean spec cannot be told from a detector that matched nothing." >&2
  exit 2
fi

# 🔴 EXTRACTION IS FENCED-CODE-BLOCK AWARE, AND IT IS ALL DONE IN awk (portable --
# no gawk-only 3-arg match, no ugrep negated-class/brace pitfalls). Toggle an
# "inside fence" flag on any line whose first non-space run is a triple backtick or
# triple tilde; while inside a fence, skip. Outside fences, a line matching
# `^## [0-9]+\.` yields its leading integer (strip the `## ` prefix, then cut at the
# first `.`). Emit `linenumber<TAB>number` so a duplicate can be reported with the
# lines it occurs on.
extract() {
  awk '
    {
      t = $0
      sub(/^[ \t]*/, "", t)
      if (t ~ /^```/ || t ~ /^~~~/) { infence = !infence; next }
      if (infence) next
      if ($0 ~ /^## [0-9]+\./) {
        s = substr($0, 4)          # strip the "## " prefix
        i = index(s, ".")
        print NR "\t" substr(s, 1, i - 1)
      }
    }
  ' "$1"
}

# 🔴 CANARY FIRST: prove the instrument fires before trusting any spec verdict. A
# broken extraction would report the spec "clean" AND the canary "no duplicate", so
# checking the canary up front closes the vacuous-pass hole for both paths.
canary_dups="$(extract "$CANARY" | awk -F'\t' '{ c[$2]++ } END { d = 0; for (k in c) if (c[k] > 1) d++; print d }')"
if [ "$canary_dups" -lt 1 ]; then
  echo "check-spec-numbering: ================================================================" >&2
  echo "check-spec-numbering: INSTRUMENT BROKEN -- the duplicate detector did not fire on the" >&2
  echo "check-spec-numbering: canary ($CANARY)." >&2
  echo "check-spec-numbering:" >&2
  echo "check-spec-numbering: That fixture carries a deliberately planted duplicate section" >&2
  echo "check-spec-numbering: number. Finding none means the awk extraction matched nothing" >&2
  echo "check-spec-numbering: (a changed pattern, a mangled canary), so a \"clean\" spec would" >&2
  echo "check-spec-numbering: mean nothing. Restore the canary's duplicate or fix extract()." >&2
  echo "check-spec-numbering: ================================================================" >&2
  exit 2
fi

# Duplicate numbers in the spec, each with the line numbers it appears on, sorted
# numerically. Non-empty means findings.
dup_report="$(extract "$SPEC" | awk -F'\t' '
  { lines[$2] = lines[$2] (lines[$2] == "" ? "" : ", ") $1; cnt[$2]++ }
  END { for (k in cnt) if (cnt[k] > 1) print k "\t" lines[k] }
' | sort -n)"

# Whole-file stats: unique-section count and the highest number seen.
stats="$(extract "$SPEC" | awk -F'\t' '
  { c[$2]++; if ($2 + 0 > max) max = $2 + 0 }
  END { n = 0; for (k in c) n++; print n " " max }')"
unique_count="${stats% *}"
highest="${stats#* }"

if [ "$unique_count" -lt 1 ]; then
  echo "check-spec-numbering: INSTRUMENT BROKEN -- no section headings found in $SPEC." >&2
  echo "  Expected lines like '## 455. PRD #88 -- ...'. Extraction matched nothing," >&2
  echo "  so a 'clean' verdict would be vacuous. Check the file and extract()." >&2
  exit 2
fi

if [ -n "$dup_report" ]; then
  echo "check-spec-numbering: DUPLICATE section number(s) in $SPEC:" >&2
  printf '%s\n' "$dup_report" | while IFS='	' read -r num where; do
    echo "  §${num} appears on lines: ${where}" >&2
  done
  echo "" >&2
  echo "  Section numbers must be UNIQUE. Order and gaps are fine (this file is" >&2
  echo "  append-numbered and not sorted -- see $SPEC:15084), so the fix is to" >&2
  echo "  RENUMBER one of the collisions above the highest number in the whole file," >&2
  echo "  never to reorder or close a gap." >&2
  exit 1
fi

next=$((highest + 1))
echo "check-spec-numbering: clean -- $unique_count unique section number(s) in $SPEC, all distinct."
echo "check-spec-numbering: highest section = $highest; next landing number = $next"
echo "check-spec-numbering:   (read across the WHOLE file, not the tail: the tail is not the max)."
echo "check-spec-numbering: canary duplicate DETECTED in $CANARY -- the detector is live, so this"
echo "check-spec-numbering: green is a positive observation rather than a check that never looked."
exit 0
