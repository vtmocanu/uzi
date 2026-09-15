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
# 🔴 ALLOW-DROP MARKER (contract-phase exemption, issue #1087). A CONTRACT step of an
# expand/contract change legitimately drops a now-dead worker-facing column a release AFTER
# its last reader is gone. To let that one drop through without disarming the guard for
# everything else, put a marker in the SAME Up section naming the EXACT table.column:
#     -- migration-additive:allow-drop runs.lineage_epoch
#     ALTER TABLE runs DROP COLUMN lineage_epoch;
# The marker exempts ONLY that table.column: an unmarked drop still fails, a marker naming a
# DIFFERENT column does not exempt the actual drop, and a second unmarked drop in the same
# statement still fails. Only DROP COLUMN is markerable; RENAME / ALTER ... TYPE / DROP
# TABLE are never exempted. Mirrors the check-docs:ignore-path marker-comment convention.
# The marker/mismatch self-check canaries below prove the match stays exact (both directions).
#
# 🔴 STRING LITERALS (issue #1128). PostgreSQL dollar-quoted strings ($$ ... $$, or tagged
# $tag$ ... $tag$; multi-line, and several blocks per section) AND single-quoted strings
# ('...') are opaque text. scan() tracks literal state and, while inside one, reads NEITHER the
# allow-drop marker NOR a DROP: a marker on its own line inside a DO/function body is string
# content, not a comment, and cannot exempt a real drop; a DROP COLUMN-shaped fragment inside a
# body is not a statement; and a $$/$tag$ token inside a '...' string must not open a spurious
# literal that hides a real drop that follows it. The dollarquote and sqstring self-check
# canaries below prove each direction.
#
# 🔴 SELF-CHECK (canary), the check-spec-numbering convention. A silent pass is the
# failure mode: if the awk parse ever matches nothing, every migration reads "clean" and
# this gate passes vacuously. The canary ($1) carries a deliberately planted worker-facing
# destructive statement in its Up section AND a destructive statement in its Down section.
# The scanner is run over it first and MUST report EXACTLY ONE finding — the Up one. Zero
# means the detector is blind (instrument broken); two means it wrongly scanned the Down
# section (a false positive the canary is built to expose). Either way: exit 2.
#
# Six sibling fixtures beside $1 extend the self-check. For the allow-drop marker (#1087):
# migration-additive-marker-canary.sql (a worker-table drop WITH a matching STANDALONE
# marker) MUST yield 0; migration-additive-mismatch-canary.sql (a drop whose marker names a
# DIFFERENT column) MUST yield 1; and migration-additive-embedded-canary.sql (a marker
# trailing a statement or inside a single-line string literal, NOT a standalone comment) MUST
# yield 1. For string-literal awareness (#1128): migration-additive-dollarquote-marker-canary.sql
# (a standalone marker line INSIDE a $$ ... $$ body, next to a real worker-table drop that
# FOLLOWS the body) MUST yield 1; migration-additive-dollarquote-drop-canary.sql (a DROP
# COLUMN-shaped fragment INSIDE a $$ ... $$ body) MUST yield 0; and
# migration-additive-sqstring-canary.sql (a $$/$tag$ token INSIDE a single-quoted string,
# then a real worker-table drop) MUST yield 1. Together they prove the exemption is exact in
# every direction and that string content is never read as SQL: a broken marker match reddens
# the marker canary, an over-broad one reddens the mismatch canary, dropping the
# standalone-comment anchor reddens the embedded canary, and losing dollar-quote or
# single-quote state reddens one of the three string-literal canaries.
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
    # A column drop in Postgres is `DROP [COLUMN] <name>`; the COLUMN keyword is OPTIONAL.
    # Flag a `drop` whose following word is a real column name -- i.e. anything EXCEPT the
    # relaxations the guard deliberately ignores: `drop constraint`, `drop default`, and
    # `drop not null`. (`drop column <name>` also lands here: its next word "column" is not
    # a relaxation keyword, so it is treated as a column drop.) DROP INDEX / DROP TABLE are
    # never inside an ALTER TABLE statement, so they never reach this function.
    function col_drop(r,   tmp, tail, word){
      tmp = r
      while (match(tmp, /(^| )drop /)){
        tail = substr(tmp, RSTART + RLENGTH)
        if (match(tail, /^[a-z_"][a-z0-9_"]*/)){
          word = substr(tail, RSTART, RLENGTH); gsub(/"/, "", word)
          if (word != "constraint" && word != "default" && word != "not") return 1
        }
        tmp = tail
      }
      return 0
    }
    # Exact-match exemption for a DROP COLUMN (issue #1087). Returns 1 iff the statement
    # drops at least one column AND EVERY dropped column is named by an
    # `-- migration-additive:allow-drop <table>.<column>` marker seen earlier in this Up
    # section (recorded in allow[]). It mirrors col_drop`s notion of "a column drop"
    # (DROP [COLUMN] <name>, minus the drop-constraint/default/not-null relaxations), so an
    # exemption can never be broader than what col_drop flags. A marker naming a different
    # column, or a second unmarked drop in the same statement, leaves this 0 -> still flagged.
    function drop_exempted(rest, tbl,   tmp, tail, word, rem, col, anycol){
      tmp = rest; anycol = 0
      while (match(tmp, /(^| )drop /)){
        tail = substr(tmp, RSTART + RLENGTH)
        if (match(tail, /^[a-z_"][a-z0-9_"]*/)){
          word = substr(tail, RSTART, RLENGTH); gsub(/"/, "", word)
          if (word == "column"){
            rem = substr(tail, RLENGTH + 1); sub(/^[ \t]+/, "", rem); col = ""
            if (match(rem, /^[a-z_"][a-z0-9_"]*/)){ col = substr(rem, RSTART, RLENGTH); gsub(/"/, "", col) }
            anycol = 1
            if (!((tbl "." col) in allow)) return 0
          } else if (word != "constraint" && word != "default" && word != "not"){
            anycol = 1
            if (!((tbl "." word) in allow)) return 0
          }
        }
        tmp = tail
      }
      return anycol
    }
    # 🔴 STRING/LITERAL SANITIZER (issue #1128). PostgreSQL dollar-quoted string literals
    # ($$ ... $$, tagged $tag$ ... $tag$) AND single-quoted string literals (bounded by single
    # quotes) are opaque text: a `;`, a `--`, or an ALTER/DROP fragment INSIDE one is string
    # content, not SQL, and an allow-drop marker inside one is not a comment. Return the line
    # with every such span removed, keeping ordinary code and SQL line comments verbatim -- so
    # goose markers, the allow-drop anchor, and the end-of-line comment strip below all still
    # see what they should. Single-quoted strings are tracked too so a $$/$tag$ INSIDE one (or a
    # stray ; / --) cannot open a spurious dollar-literal or split a statement. Two persistent
    # flags carry state across lines ("" / 0 = outside): dqtag (open dollar delimiter) and insq
    # (inside a single-quoted string). They reset per file and per goose section, since neither
    # literal spans one. SQ (a lone single-quote char, built in BEGIN) stands in for the quote
    # glyph so this single-quoted awk program carries no literal quote. Walk by CHARACTER
    # POSITION (index/substr), not a per-line toggle: several blocks can open/close on one line
    # and a delimiter can sit mid-line. Portable awk only -- substr/index/length and 2-arg match
    # with RSTART/RLENGTH; no gawk-only 3-arg match(s,re,arr), no gensub, no \< / {n,m}.
    function strip_literals(s,   out, n, i, rest, cpos){
      out = ""; n = length(s); i = 1
      while (i <= n){
        if (dqtag != ""){
          # inside a dollar-quoted literal: drop through to the exact closing delimiter.
          rest = substr(s, i)
          cpos = index(rest, dqtag)          # exact closer (literal, case-sensitive)
          if (cpos > 0){
            i = i + cpos - 1 + length(dqtag)    # drop the literal up to and incl. the closer
            dqtag = ""
          } else {
            return out                          # literal runs to EOL; drop the rest of it
          }
        } else if (insq){
          # inside a single-quoted string: a doubled quote is an escaped quote (stays IN); a
          # lone quote ends it. SQ / SQ SQ stand in for the quote glyph (see BEGIN).
          if (substr(s, i, 2) == SQ SQ){ i = i + 2 }
          else if (substr(s, i, 1) == SQ){ insq = 0; i++ }
          else { i++ }                       # ordinary string content -- dropped, not code
        } else {
          # code state
          if (substr(s, i, 2) == "--"){      # rest of line is a SQL line comment: keep verbatim
            return out substr(s, i)            # (a $$/$1/quote in a comment is inert)
          }
          if (substr(s, i, 1) == SQ){ insq = 1; i++; continue }   # a single-quoted string opens
          if (substr(s, i, 1) == "$"){
            rest = substr(s, i)              # try to read a dollar-quote OPENER $tag$ here
            if (match(rest, /^\$([a-zA-Z_][a-zA-Z0-9_]*)?\$/)){
              dqtag = substr(rest, 1, RLENGTH)   # exact delimiter, e.g. "$$" or "$body$"
              i = i + RLENGTH                     # skip the opener; enter dollar-literal state
              continue
            }
          }
          out = out substr(s, i, 1); i++     # an ordinary code character
        }
      }
      return out
    }
    function check(raw,   stmt, rest, w, tbl, verb, m, parts, k, tw){
      stmt = norm(raw)
      if (stmt == "") return
      if (stmt ~ /^drop table /){
        # DROP TABLE takes a comma-separated LIST of targets; flag if ANY is worker-facing.
        rest = stmt; sub(/^drop table /, "", rest); sub(/^if exists /, "", rest); sub(/^only /, "", rest)
        m = split(rest, parts, ",")
        for (k = 1; k <= m; k++){
          split(parts[k], tw, " "); tbl = cleantbl(tw[1])   # first token drops a trailing cascade/restrict
          if (tbl in wf) report(tbl, "DROP TABLE", stmt)
        }
        return
      }
      if (stmt ~ /^alter table /){
        rest = stmt; sub(/^alter table /, "", rest); sub(/^if exists /, "", rest); sub(/^only /, "", rest)
        split(rest, w, " "); tbl = cleantbl(w[1])
        verb = ""
        if (col_drop(rest))                           verb = "DROP COLUMN"
        else if (rest ~ /(^| )rename column /)         verb = "RENAME COLUMN"
        else if (rest ~ /(^| )rename to /)             verb = "RENAME TO"
        else if (rest ~ /(^| )alter column .* type /)  verb = "ALTER COLUMN ... TYPE"
        if (verb != "" && (tbl in wf)){
          if (verb == "DROP COLUMN" && drop_exempted(rest, tbl)) return
          report(tbl, verb, stmt)
        }
        return
      }
    }
    BEGIN { n = split(tables, a, " "); for (i = 1; i <= n; i++) wf[a[i]] = 1; SQ = sprintf("%c", 39) }
    FNR == 1 { state = "pre"; buf = ""; split("", allow); dqtag = ""; insq = 0 }
    {
      lc = tolower($0)
      # Detect goose section markers on the RAW line, BEFORE any string sanitizing (the marker
      # IS a comment; a section marker never sits inside a literal, and reading the raw line
      # means a stray unclosed literal can never hide one). allow[] (the approved-drop set) AND
      # the literal-state flags dqtag/insq (#1128) all reset per Up section: a marker can never
      # leak across sections or in from a Down, and a literal never spans a section.
      if (lc ~ /\+goose[ \t]+up/)   { state = "up";   buf = ""; split("", allow); dqtag = ""; insq = 0; next }
      if (lc ~ /\+goose[ \t]+down/) { state = "down"; buf = ""; dqtag = ""; insq = 0; next }
      if (state != "up") next
      # Remove string-literal spans (dollar-quoted and single-quoted, issue #1128) BEFORE the
      # marker capture and the DROP scan below, so neither reads string content as SQL. SQL line
      # comments survive in sline, so the standalone allow-drop anchor and the end-of-line
      # comment strip still see them.
      sline = strip_literals($0)
      slc = tolower(sline)
      # Capture an approval marker BEFORE the comment strip below (the marker IS a comment).
      # `-- migration-additive:allow-drop <table>.<column>` exempts exactly that
      # table.column from the DROP COLUMN check (issue #1087), and nothing else.
      # The marker MUST be a STANDALONE comment line (`^\s*-- migration-additive:allow-drop
      # <tbl>.<col>\s*$`): a token trailing a destructive statement, or inside a string
      # literal, must NOT populate allow[] (it would let an embedded token bypass the guard).
      # A marker INSIDE a dollar-quoted literal has been blanked out of sline (#1128), so it
      # can never populate allow[] either.
      if (match(slc, /^[ \t]*--[ \t]*migration-additive:allow-drop[ \t]+[a-z0-9_".]+[ \t]*$/)){
        mk = slc; sub(/^.*allow-drop[ \t]+/, "", mk); sub(/[ \t]+$/, "", mk); gsub(/"/, "", mk)
        allow[mk] = 1
      }
      line = sline
      sub(/--.*/, "", line)          # strip an end-of-line SQL comment
      buf = buf " " tolower(line)
      while ((p = index(buf, ";")) > 0){
        check(substr(buf, 1, p - 1))
        buf = substr(buf, p + 1)
      }
    }
  ' "$@"
}

# 🔴 CANARIES FIRST: prove the detector fires (Up only), that the allow-drop marker exemption
# is EXACT, and that string-literal content is never read as SQL -- before trusting any verdict
# over the real corpus. Seven fixtures, all beside $CANARY, each run through the same scan():
#   canary             (no marker)                  -> MUST be 1  (detector live, Up-only)
#   marker canary      (matching allow-drop)        -> MUST be 0  (exact marker exempts)
#   mismatch canary    (allow-drop names OTHER col) -> MUST be 1  (marker cannot over-exempt)
#   embedded canary    (marker trailing a stmt /    -> MUST be 1  (only a STANDALONE comment
#                       inside a single-line string)                marker exempts)
#   dollarquote marker (standalone marker INSIDE a  -> MUST be 1  (a marker in string content
#                       $$ body, drop AFTER it)                     cannot exempt a real drop)
#   dollarquote drop   (DROP-shaped text INSIDE a   -> MUST be 0  (string content is not a
#                       $$ body)                                    statement)
#   sqstring           ($$/$tag$ INSIDE a '...'     -> MUST be 1  (a token in a single-quoted
#                       string, real drop AFTER it)                 string opens no literal)
# The marker/mismatch/embedded trio is the both-directions mutation guard on the exemption
# (#1087): delete the marker logic and the marker canary jumps to 1; loosen it to "any
# marker exempts any drop" and the mismatch canary falls to 0; drop the standalone-comment
# anchor and the embedded canary falls to 0. The three string-literal canaries guard #1128:
# lose dollar-quote state and the dollarquote-marker canary falls to 0 (an in-body marker
# wrongly exempts) or the dollarquote-drop canary rises to 1 (in-body text wrongly flagged);
# lose single-quote state and the sqstring canary falls to 0 (a $$ in a string opens a
# spurious literal that hides the real drop). Either way this self-check exits 2.
CANARY_DIR="$(dirname "$CANARY")"
MARKER_CANARY="$CANARY_DIR/migration-additive-marker-canary.sql"
MISMATCH_CANARY="$CANARY_DIR/migration-additive-mismatch-canary.sql"
EMBEDDED_CANARY="$CANARY_DIR/migration-additive-embedded-canary.sql"
DOLLAR_MARKER_CANARY="$CANARY_DIR/migration-additive-dollarquote-marker-canary.sql"
DOLLAR_DROP_CANARY="$CANARY_DIR/migration-additive-dollarquote-drop-canary.sql"
SQSTRING_CANARY="$CANARY_DIR/migration-additive-sqstring-canary.sql"
for f in "$MARKER_CANARY" "$MISMATCH_CANARY" "$EMBEDDED_CANARY" "$DOLLAR_MARKER_CANARY" "$DOLLAR_DROP_CANARY" "$SQSTRING_CANARY"; do
  if [ ! -f "$f" ]; then
    echo "check-migration-additive: self-check fixture not found: $f" >&2
    echo "  The marker/mismatch canaries prove the allow-drop exemption is exact; without" >&2
    echo "  them a broken or over-broad marker match could not be told from a working one." >&2
    exit 2
  fi
done

count_hits() { printf '%s' "$1" | awk 'NF{n++} END{print n+0}'; }
canary_count="$(count_hits "$(scan "$CANARY")")"
marker_count="$(count_hits "$(scan "$MARKER_CANARY")")"
mismatch_count="$(count_hits "$(scan "$MISMATCH_CANARY")")"
embedded_count="$(count_hits "$(scan "$EMBEDDED_CANARY")")"
dollar_marker_count="$(count_hits "$(scan "$DOLLAR_MARKER_CANARY")")"
dollar_drop_count="$(count_hits "$(scan "$DOLLAR_DROP_CANARY")")"
sqstring_count="$(count_hits "$(scan "$SQSTRING_CANARY")")"
if [ "$canary_count" -ne 1 ] || [ "$marker_count" -ne 0 ] || [ "$mismatch_count" -ne 1 ] || [ "$embedded_count" -ne 1 ] || [ "$dollar_marker_count" -ne 1 ] || [ "$dollar_drop_count" -ne 0 ] || [ "$sqstring_count" -ne 1 ]; then
  echo "check-migration-additive: ================================================================" >&2
  echo "check-migration-additive: INSTRUMENT BROKEN -- a self-check fixture did not yield its" >&2
  echo "check-migration-additive: expected count (canary=$canary_count want 1, marker=$marker_count want 0," >&2
  echo "check-migration-additive: mismatch=$mismatch_count want 1, embedded=$embedded_count want 1," >&2
  echo "check-migration-additive: dollar-marker=$dollar_marker_count want 1, dollar-drop=$dollar_drop_count want 0," >&2
  echo "check-migration-additive: sqstring=$sqstring_count want 1)." >&2
  echo "check-migration-additive:" >&2
  echo "check-migration-additive:   canary   ($CANARY): one Up + one Down destructive stmt;" >&2
  echo "check-migration-additive:            want 1 (0 = parse matched nothing; 2 = wrongly read Down)." >&2
  echo "check-migration-additive:   marker   ($MARKER_CANARY): a worker-table DROP" >&2
  echo "check-migration-additive:            COLUMN with a MATCHING standalone allow-drop marker; want 0." >&2
  echo "check-migration-additive:   mismatch ($MISMATCH_CANARY): a worker-table DROP" >&2
  echo "check-migration-additive:            COLUMN whose allow-drop marker names a DIFFERENT column;" >&2
  echo "check-migration-additive:            want 1 (a marker must never over-exempt)." >&2
  echo "check-migration-additive:   embedded ($EMBEDDED_CANARY): a worker-table DROP with the" >&2
  echo "check-migration-additive:            marker trailing the statement or inside a string literal;" >&2
  echo "check-migration-additive:            want 1 (only a STANDALONE comment marker exempts)." >&2
  echo "check-migration-additive:   dollar-marker ($DOLLAR_MARKER_CANARY):" >&2
  echo "check-migration-additive:            a standalone allow-drop marker INSIDE a \$\$ ... \$\$ body, next to a real" >&2
  echo "check-migration-additive:            worker-table DROP that FOLLOWS the body; want 1 (in-body string content" >&2
  echo "check-migration-additive:            is not a comment and cannot exempt the real drop, issue #1128)." >&2
  echo "check-migration-additive:   dollar-drop ($DOLLAR_DROP_CANARY):" >&2
  echo "check-migration-additive:            a DROP COLUMN-shaped fragment INSIDE a \$\$ ... \$\$ body; want 0 (string" >&2
  echo "check-migration-additive:            content is not a statement, issue #1128)." >&2
  echo "check-migration-additive:   sqstring ($SQSTRING_CANARY): a \$\$/\$tag\$ token INSIDE a" >&2
  echo "check-migration-additive:            single-quoted string, then a real worker-table DROP after it; want 1 (a" >&2
  echo "check-migration-additive:            token in a '...' string must not open a literal that hides the real drop," >&2
  echo "check-migration-additive:            issue #1128). Fix scan() or the fixtures." >&2
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
