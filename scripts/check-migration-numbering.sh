#!/bin/sh
# Gate goose migrations on number-prefix UNIQUENESS, whole-directory (PRD #500 M1).
#
# usage: scripts/check-migration-numbering.sh <canary-dir> [migrations-dir]
#   e.g. scripts/check-migration-numbering.sh scripts/migration-numbering-canary \
#          api/internal/store/migrations
#
# 🔴 WHY THIS EXISTS. goose (v3.27.3, api/go.mod:25) PANICS
# `goose: duplicate version N detected` the moment two migrations share a number
# prefix — at API boot AND in every *LiveDB test that calls store.Migrate. Migration
# numbers are "assigned at merge time, renumbered above the live head" (CLAUDE.md),
# which is prose enforced nowhere until now. This check makes the collision mechanical:
# it reports any number carried by more than one *.sql file and instructs a renumber
# above the head.
#
# 🔴 UNIQUENESS ONLY -- NEVER ORDER, GAPS OR CONTIGUITY. The migrations dir is
# DELIBERATELY not contiguous: intentional gaps exist (e.g. 00002→00010, 00021→00029)
# because numbers are assigned at merge time above the live head and nothing backfills
# a hole. So this check asserts one thing: that no number appears on two files. A check
# that flagged a gap or an out-of-order landing would be a PERMANENT false positive on
# that intentional layout, which is why the extraction below reports duplicates and
# nothing else — never "close the gap" or "reorder".
#
# 🔴 THE NUMBER IS CANONICALISED TO ITS ARITHMETIC VALUE (`+ 0`), NOT A FIXED WIDTH.
# The extraction grabs the leading run of digits before the first `_` in the BASENAME
# and reads it as an integer, so a mis-padded `0109_x.sql` collides with `00109_y.sql`
# (both are 109) and `00140` reads as 140. Grabbing a fixed-width `[0-9]{5}` would let a
# mis-padded number slip past the very collision this gate exists to catch.
#
# 🔴 A LIVENESS CANARY, BECAUSE A SILENT PASS IS THE FAILURE MODE. If the awk
# extraction ever matches nothing (a pattern change, a bad edit), the corpus would read
# "0 duplicates" and this gate would pass VACUOUSLY. The canary dir ($1) carries two
# deliberately colliding-numbered stubs (00001_a.sql, 00001_b.sql); the identical
# extraction is run over it FIRST and MUST find that duplicate, or the instrument is
# declared broken. A clean run therefore PRINTS that the canary duplicate was detected
# -- a positive observation, so a green here is not "the detector never looked".
#
# NO SKIP BRANCH AND NO *_REQUIRED ENV VAR, deliberately -- the asymmetry with
# lint-yaml.sh / lint-formula.sh is that those wrap brew/pip tools a contributor may
# lack, whereas this check needs only sh/git/awk/sort, which are always present.
#
# EXIT CODES (the check-spec-numbering.sh / check-migration-additive.sh convention):
#     2 = the instrument is broken (a dir missing, extraction empty, or the canary
#         duplicate did not fire)
#     1 = there are findings (a number prefix appears on more than one file)
#     0 = clean, and the canary duplicate was seen
# `task`'s own rc is 201 for all of them.
set -eu

if [ "$#" -lt 1 ]; then
  echo "usage: scripts/check-migration-numbering.sh <canary-dir> [migrations-dir]" >&2
  echo "  e.g. scripts/check-migration-numbering.sh scripts/migration-numbering-canary api/internal/store/migrations" >&2
  exit 2
fi

CANARY_DIR="$1"
MIGRATIONS_DIR="${2:-api/internal/store/migrations}"

# Run from the repo root whatever the caller's directory, like the sibling scripts.
ROOT="$(git rev-parse --show-toplevel)" || {
  echo "check-migration-numbering: not inside a git work tree (git rev-parse --show-toplevel failed)" >&2
  exit 2
}
cd "$ROOT" || {
  echo "check-migration-numbering: cannot cd to repo root: $ROOT" >&2
  exit 2
}

if [ ! -d "$CANARY_DIR" ]; then
  echo "check-migration-numbering: canary dir not found: $CANARY_DIR" >&2
  echo "  The canary is what proves the duplicate detector fires; without it a" >&2
  echo "  clean corpus cannot be told from a detector that matched nothing." >&2
  exit 2
fi
if [ ! -d "$MIGRATIONS_DIR" ]; then
  echo "check-migration-numbering: migrations dir not found: $MIGRATIONS_DIR" >&2
  exit 2
fi

# 🔴 THE EXTRACTION, ALL IN awk (portable; no ugrep negated-class/brace pitfalls).
# For every *.sql file in the given dir, take the BASENAME and grab its leading run of
# digits (the number prefix before the first `_`), then print `number<TAB>basename`
# with the number canonicalised to its arithmetic value. Files whose basename does not
# begin with a digit run are not numbered migrations and are skipped.
extract() {
  dir="$1"
  for f in "$dir"/*.sql; do
    [ -f "$f" ] || continue
    printf '%s\n' "${f##*/}"
  done | awk '
    {
      base = $0
      if (match(base, /^[0-9]+/)) {
        # `+ 0` canonicalises the digits to their NUMERIC value so `0109` and `00109`
        # collide as the one number 109 they both read as -- a duplicate detector must
        # not go blind on a mis-padded prefix.
        print (substr(base, RSTART, RLENGTH) + 0) "\t" base
      }
    }
  '
}

# 🔴 CANARY FIRST: prove the instrument fires before trusting any corpus verdict. A
# broken extraction would report the corpus "clean" AND the canary "no duplicate", so
# checking the canary up front closes the vacuous-pass hole for both paths.
canary_dups="$(extract "$CANARY_DIR" | awk -F'\t' '{ c[$1]++ } END { d = 0; for (k in c) if (c[k] > 1) d++; print d }')"
if [ "$canary_dups" -lt 1 ]; then
  echo "check-migration-numbering: ================================================================" >&2
  echo "check-migration-numbering: INSTRUMENT BROKEN -- the duplicate detector did not fire on the" >&2
  echo "check-migration-numbering: canary ($CANARY_DIR)." >&2
  echo "check-migration-numbering:" >&2
  echo "check-migration-numbering: That fixture carries two deliberately colliding-numbered stubs." >&2
  echo "check-migration-numbering: Finding no duplicate means the awk extraction matched nothing" >&2
  echo "check-migration-numbering: (a changed pattern, a mangled canary), so a \"clean\" corpus would" >&2
  echo "check-migration-numbering: mean nothing. Restore the canary's collision or fix extract()." >&2
  echo "check-migration-numbering: ================================================================" >&2
  exit 2
fi

# Scan the real corpus. A glob with no match would leave the literal pattern; guard it.
any_file=0
for mig in "$MIGRATIONS_DIR"/*.sql; do
  [ -f "$mig" ] || continue
  any_file=1
  break
done
if [ "$any_file" -eq 0 ]; then
  echo "check-migration-numbering: no *.sql migrations under $MIGRATIONS_DIR -- nothing to scan." >&2
  echo "  Extraction over an empty corpus is vacuous; treating as an instrument failure." >&2
  exit 2
fi

# Duplicate numbers in the corpus, each with the filenames that carry it, sorted
# numerically. Non-empty means findings.
dup_report="$(extract "$MIGRATIONS_DIR" | awk -F'\t' '
  { files[$1] = files[$1] (files[$1] == "" ? "" : ", ") $2; cnt[$1]++ }
  END { for (k in cnt) if (cnt[k] > 1) print k "\t" files[k] }
' | sort -n)"

# Whole-directory stats: unique-number count and the highest number seen (the head).
stats="$(extract "$MIGRATIONS_DIR" | awk -F'\t' '
  { c[$1]++; if ($1 + 0 > max) max = $1 + 0 }
  END { n = 0; for (k in c) n++; print n " " max }')"
unique_count="${stats% *}"
highest="${stats#* }"

if [ "$unique_count" -lt 1 ]; then
  echo "check-migration-numbering: INSTRUMENT BROKEN -- no numbered migrations found in $MIGRATIONS_DIR." >&2
  echo "  Expected files like '00140_github_project_sync.sql'. Extraction matched nothing," >&2
  echo "  so a 'clean' verdict would be vacuous. Check the dir and extract()." >&2
  exit 2
fi

if [ -n "$dup_report" ]; then
  echo "check-migration-numbering: DUPLICATE migration number(s) in $MIGRATIONS_DIR:" >&2
  printf '%s\n' "$dup_report" | while IFS='	' read -r num files; do
    echo "  number ${num} is used by: ${files}" >&2
  done
  echo "" >&2
  echo "  goose panics 'duplicate version detected' on two migrations sharing a number," >&2
  echo "  at API boot and in every *LiveDB test. Numbers are UNIQUE, assigned at merge" >&2
  echo "  time; order and gaps are fine (numbers are landed above the live head, never" >&2
  echo "  backfilled). The fix is to RENUMBER one of the collisions above to the next" >&2
  echo "  free number above the live head, never to close a gap or reorder." >&2
  exit 1
fi

next=$((highest + 1))
echo "check-migration-numbering: clean -- $unique_count unique migration number(s) in $MIGRATIONS_DIR, all distinct."
echo "check-migration-numbering: head (highest) number = $highest; next landing number = $next"
echo "check-migration-numbering:   (renumber a new migration to this above the live head; never close a gap)."
echo "check-migration-numbering: canary duplicate DETECTED in $CANARY_DIR -- the detector is live, so this"
echo "check-migration-numbering: green is a positive observation rather than a check that never looked."
exit 0
