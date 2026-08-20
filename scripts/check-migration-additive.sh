#!/bin/sh
# Gate every migration's `-- +goose Up` section against a DESTRUCTIVE schema change on
# a WORKER-FACING table (PRD #422 Decision 2 / M6).
#
# usage: scripts/check-migration-additive.sh <canary-file> [migrations-dir]
#   e.g. scripts/check-migration-additive.sh scripts/migration-additive-canary.sql \
#          api/internal/store/migrations
#
# 🔴 WHY THIS EXISTS. During a rolling release the api rolls AHEAD of the worker fleet,
# so a worker still on the previous image (N-1) keeps talking to the NEW api and the NEW
# schema. That old worker still READS the columns/tables it always did. A migration that
# DROPS, RENAMES, or RETYPES one of those out from under it breaks the old worker mid
# release — the exact skew TestOldWorkerSkewLiveDB proves the api tolerates. This check
# is the schema-side half: it makes an additive-only release-window discipline mechanical
# rather than a review reminder. An additive change (ADD COLUMN, new table, new index,
# relaxing NOT NULL / a default / a constraint) is always safe for an old worker and is
# NOT flagged; a removal/rename/retype is.
#
# 🔴 SCOPE — UP SECTIONS ONLY. A destructive statement in a `-- +goose Down` section is
# legitimate: Down is the rollback of an additive Up (drop the column the Up added), and
# it only runs on an explicit `goose down`, never during a forward rolling release. The
# scanner reads the text between `-- +goose Up` and the next `-- +goose Down` (or EOF)
# and nothing else. The canary proves this directionality (see below).
#
# 🔴 WORKER-FACING TABLE SET. Only the tables an old worker reads over the protocol are
# in scope. A destructive change to a table no worker touches (e.g. anthropic_rate_limits)
# is out of scope here — it cannot break the worker skew. The set is listed once, below.
#
# 🔴 NOT flagged, deliberately: DROP CONSTRAINT, DROP NOT NULL, DROP DEFAULT, DROP INDEX.
# Those RELAX a table and are additive-compatible for a reader (there are ~25 legit ones
# in history). Only column/table removal, rename, or retype breaks an old reader.
#
# 🔴 SELF-CHECK (canary), the check-spec-numbering convention. A silent pass is the
# failure mode: if the awk parse ever matches nothing, every migration reads "clean" and
# this gate passes vacuously. The canary ($1) carries a deliberately planted worker-facing
# destructive statement in its Up section AND a destructive statement in its Down section.
# The scanner is run over it first and MUST report EXACTLY ONE finding — the Up one. Zero
# means the detector is blind (instrument broken); two means it wrongly scanned the Down
# section (a false positive the canary is built to expose). Either way: exit 2.
#
# EXIT CODES (the lint-yaml.sh / scan-secrets.sh / check-spec-numbering.sh convention):
#     2 = the instrument is broken (a file/dir missing, or the canary did not detect
#         exactly its one planted Up-section statement)
#     1 = there are findings (a destructive change to a worker-facing table in an Up section)
#     0 = clean, and the canary's planted Up-section statement was detected
# `task`'s own rc is 201 for all of them.
set -eu

# The tables an N-1 worker reads over the protocol (claim payload, /state, /messages,
# forge read surface). A destructive Up-section change to one of these breaks the old
# worker during a rolling release. Keep in sync with the worker protocol, not with the
# whole schema.
WORKER_TABLES="workers runs run_messages issues repos forge_connections"

if [ "$#" -lt 1 ]; then
  echo "usage: scripts/check-migration-additive.sh <canary-file> [migrations-dir]" >&2
  echo "  e.g. scripts/check-migration-additive.sh scripts/migration-additive-canary.sql api/internal/store/migrations" >&2
  exit 2
fi

CANARY="$1"
MIGRATIONS_DIR="${2:-api/internal/store/migrations}"

# Run from the repo root whatever the caller's directory, like the sibling scripts.
ROOT="$(git rev-parse --show-toplevel)" || {
  echo "check-migration-additive: not inside a git work tree (git rev-parse --show-toplevel failed)" >&2
  exit 2
}
cd "$ROOT" || {
  echo "check-migration-additive: cannot cd to repo root: $ROOT" >&2
  exit 2
}

if [ ! -f "$CANARY" ]; then
  echo "check-migration-additive: canary file not found: $CANARY" >&2
  echo "  The canary is what proves the destructive-statement detector fires; without it a" >&2
  echo "  clean corpus cannot be told from a detector that matched nothing." >&2
  exit 2
fi
if [ ! -d "$MIGRATIONS_DIR" ]; then
  echo "check-migration-additive: migrations dir not found: $MIGRATIONS_DIR" >&2
  exit 2
fi

# 🔴 THE LOAD-BEARING PARSE, ALL IN awk (portable; no ugrep negated-class/brace pitfalls).
# SQL statements span multiple lines and end at `;`, so a line grep would miss
#   ALTER TABLE runs
#     DROP COLUMN status;
# Per file: toggle an "in Up section" flag on the goose markers; while inside Up, strip
# line comments, lowercase, accumulate into a buffer, and cut the buffer into statements
# on `;`. For each statement, find the target table (the token after `alter table` /
# `drop table [if exists]`, past an optional `if exists`/`only` and a `public.` schema
# qualifier) and whether it carries a destructive verb. Prints one TAB-separated finding
# line per hit: file<TAB>table<TAB>verb<TAB>statement.
scan() {
  awk -v tables="$WORKER_TABLES" '
    function norm(s){ gsub(/[ \t]+/, " ", s); sub(/^ /, "", s); sub(/ $/, "", s); return s }
    function cleantbl(s){ gsub(/"/, "", s); sub(/^public\./, "", s); gsub(/[(),;]/, "", s); return s }
    function report(tbl, verb, stmt){ print FILENAME "\t" tbl "\t" verb "\t" norm(stmt) }
    function check(raw,   stmt, rest, w, tbl, verb){
      stmt = norm(raw)
      if (stmt == "") return
      if (stmt ~ /^drop table /){
        rest = stmt; sub(/^drop table /, "", rest); sub(/^if exists /, "", rest)
        split(rest, w, " "); tbl = cleantbl(w[1])
        if (tbl in wf) report(tbl, "DROP TABLE", stmt)
        return
      }
      if (stmt ~ /^alter table /){
        rest = stmt; sub(/^alter table /, "", rest); sub(/^if exists /, "", rest); sub(/^only /, "", rest)
        split(rest, w, " "); tbl = cleantbl(w[1])
        verb = ""
        if (rest ~ /(^| )drop column /)               verb = "DROP COLUMN"
        else if (rest ~ /(^| )rename column /)         verb = "RENAME COLUMN"
        else if (rest ~ /(^| )rename to /)             verb = "RENAME TO"
        else if (rest ~ /(^| )alter column .* type /)  verb = "ALTER COLUMN ... TYPE"
        if (verb != "" && (tbl in wf)) report(tbl, verb, stmt)
        return
      }
    }
    BEGIN { n = split(tables, a, " "); for (i = 1; i <= n; i++) wf[a[i]] = 1 }
    FNR == 1 { state = "pre"; buf = "" }
    {
      lc = tolower($0)
      # Detect goose section markers BEFORE stripping comments (the marker IS a comment).
      if (lc ~ /\+goose[ \t]+up/)   { state = "up";   buf = ""; next }
      if (lc ~ /\+goose[ \t]+down/) { state = "down"; buf = ""; next }
      if (state != "up") next
      line = $0
      sub(/--.*/, "", line)          # strip an end-of-line SQL comment
      buf = buf " " tolower(line)
      while ((p = index(buf, ";")) > 0){
        check(substr(buf, 1, p - 1))
        buf = substr(buf, p + 1)
      }
    }
  ' "$@"
}

# 🔴 CANARY FIRST: prove the detector fires (and fires ONLY on the Up section) before
# trusting any verdict over the real corpus.
canary_hits="$(scan "$CANARY")"
canary_count="$(printf '%s' "$canary_hits" | awk 'NF{n++} END{print n+0}')"
if [ "$canary_count" -ne 1 ]; then
  echo "check-migration-additive: ================================================================" >&2
  echo "check-migration-additive: INSTRUMENT BROKEN -- the canary ($CANARY) did not yield exactly" >&2
  echo "check-migration-additive: one finding (got $canary_count)." >&2
  echo "check-migration-additive:" >&2
  echo "check-migration-additive: The canary plants ONE worker-facing destructive statement in its" >&2
  echo "check-migration-additive: Up section and one in its Down section. Zero findings means the awk" >&2
  echo "check-migration-additive: parse matched nothing (a changed pattern, a mangled canary), so a" >&2
  echo "check-migration-additive: \"clean\" corpus would mean nothing. Two means the scanner wrongly" >&2
  echo "check-migration-additive: read the Down section, which would false-positive on every legit" >&2
  echo "check-migration-additive: additive rollback. Fix scan() or the canary." >&2
  echo "check-migration-additive: ================================================================" >&2
  exit 2
fi

# Scan the real corpus. A glob with no match would leave the literal pattern; guard it.
any_file=0
for mig in "$MIGRATIONS_DIR"/*.sql; do
  [ -f "$mig" ] || continue
  any_file=1
done
if [ "$any_file" -eq 0 ]; then
  echo "check-migration-additive: no *.sql migrations under $MIGRATIONS_DIR -- nothing to scan." >&2
  echo "  Extraction over an empty corpus is vacuous; treating as an instrument failure." >&2
  exit 2
fi

hits="$(scan "$MIGRATIONS_DIR"/*.sql)"
if [ -n "$hits" ]; then
  echo "check-migration-additive: DESTRUCTIVE change to a worker-facing table in a migration Up section:" >&2
  printf '%s\n' "$hits" | while IFS='	' read -r file tbl verb stmt; do
    [ -n "$file" ] || continue
    echo "" >&2
    echo "  $file" >&2
    echo "    table:     $tbl (worker-facing)" >&2
    echo "    change:    $verb" >&2
    echo "    statement: $stmt" >&2
  done
  echo "" >&2
  echo "  An N-1 worker (previous image) still READS this column/table during a rolling" >&2
  echo "  release. Removing, renaming, or retyping it breaks that old worker mid-release" >&2
  echo "  (PRD #422 Decision 2 / M6). Releases must be ADDITIVE: add the new column/table" >&2
  echo "  alongside the old, migrate readers first, and drop the old one only a release" >&2
  echo "  LATER, once no N-1 worker remains. If this rollback truly belongs in a forward" >&2
  echo "  migration, it belongs in the Down section (which this check does not scan)." >&2
  exit 1
fi

echo "check-migration-additive: clean -- no destructive change to a worker-facing table in any"
echo "check-migration-additive: Up section under $MIGRATIONS_DIR."
echo "check-migration-additive: worker-facing tables checked: $WORKER_TABLES"
echo "check-migration-additive: canary destructive statement DETECTED in $CANARY (exactly one, Up-section"
echo "check-migration-additive: only) -- the detector is live, so this green is a positive observation."
exit 0
