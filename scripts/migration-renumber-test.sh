#!/bin/sh
# migration-renumber-test.sh -- offline fixture test for scripts/migration-renumber.sh
# (the landing-time RENUMBER helper, issue #1400).
#
# 🔴 WHAT THIS PROVES. It drives the REAL helper (scripts/migration-renumber.sh) against
# throwaway git repos and asserts its documented contract:
#   A. --rewrite-comments remaps `--` comment cross-references SIMULTANEOUSLY (no
#      double-substitution of an aliased number) and NEVER touches SQL body lines.
#   B. every precondition/preflight REFUSAL exits non-zero and CHANGES NOTHING.
#   C. the happy path renames the branch-new pair to consecutive numbers above the live
#      main head, rewrites the renamed migrations' own cross-references, REPORTS (never
#      edits) stale references in other branch-changed files, and passes its own numbering
#      self-check.
#   D. an OVERLAPPING-range renumber (a same-slug pair straddling the head) is renamed
#      correctly, and by IDENTITY, by the two-phase git mv -- where a single-phase mv
#      would collide with a still-present sibling and fail.
#
# 🔴 100% OFFLINE AND HERMETIC. Every git repo is built under `mktemp -d`; the "remote" is
# a LOCAL bare repo (`git init --bare`), so the helper's `git fetch origin main` is a local
# filesystem read, NEVER a network fetch. Nothing in the real repo tree and no real git
# remote is touched. Each temp repo sets user.email/user.name and commit.gpgsign=false
# locally so commits work with no ambient git identity.
#
# 🔴 POSIX sh, no bashisms (modelled on scripts/release-scripts-test.sh). Prints a visible
# `N passed, N failed` tally and, because a zero-case crash that still reached the end
# would otherwise read green, ABORTS (exit 2) if fewer than MIN_ASSERTIONS ran.
#
# EXIT CODES:
#     2 = the instrument is broken (helper/numbering-check/canary missing, or too few
#         assertions ran)
#     1 = at least one assertion FAILED
#     0 = every assertion passed
set -u

SCRIPTS_DIR="$(cd "$(dirname "$0")" && pwd)"
HELPER="$SCRIPTS_DIR/migration-renumber.sh"
NUMCHECK="$SCRIPTS_DIR/check-migration-numbering.sh"
CANARY="$SCRIPTS_DIR/migration-numbering-canary"

# The helper is the system under test: a missing/non-executable one is a broken instrument,
# not a test failure.
if [ ! -x "$HELPER" ]; then
  echo "migration-renumber-test: helper not found or not executable: $HELPER" >&2
  exit 2
fi
# Case C copies these into each temp repo so the helper's own numbering self-check runs;
# their absence here is an instrument failure, not a case failure.
if [ ! -f "$NUMCHECK" ]; then
  echo "migration-renumber-test: numbering check not found: $NUMCHECK" >&2
  exit 2
fi
if [ ! -d "$CANARY" ]; then
  echo "migration-renumber-test: numbering canary dir not found: $CANARY" >&2
  exit 2
fi

MIGDIR="api/internal/store/migrations"

FAILS=0
PASSES=0
pass() { PASSES=$((PASSES + 1)); printf '  ok   %s\n' "$1"; }
fail() { FAILS=$((FAILS + 1)); printf '  FAIL %s\n' "$1"; }

# assert_eq <label> <expected> <actual>
assert_eq() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (expected '$2', got '$3')"; fi
}
# assert_contains <label> <needle> <haystack>
assert_contains() {
  case "$3" in
    *"$2"*) pass "$1" ;;
    *) fail "$1 (expected to contain '$2')" ;;
  esac
}
# assert_absent <label> <needle> <haystack>  -- the OPPOSITE of assert_contains
assert_absent() {
  case "$3" in
    *"$2"*) fail "$1 (expected NOT to contain '$2')" ;;
    *) pass "$1" ;;
  esac
}
# assert_nonzero <label> <rc>
assert_nonzero() {
  if [ "$2" -ne 0 ]; then pass "$1 (refused, exit $2)"; else fail "$1 (expected non-zero exit, got 0)"; fi
}

ROOT="$(mktemp -d)"
trap 'rm -rf "$ROOT"' EXIT

# --- fixture builders --------------------------------------------------------

# wf <path> ; content on stdin -> written to <path> (parent dirs created).
wf() {
  mkdir -p "$(dirname "$1")"
  cat > "$1"
}

# mk_goose <path> <comment> -- a minimal valid goose migration (no cross-reference).
mk_goose() {
  printf '%s\n' '-- +goose Up' "-- $2" 'SELECT 1;' '-- +goose Down' 'SELECT 1;' > "$1"
}

# body_lines <sqlfile> -- print only the NON-comment lines, using the same "starts with
# `--` after leading whitespace" rule the helper's rewrite_comments applies. Used to prove
# a --rewrite-comments pass left every SQL body line byte-identical.
body_lines() {
  awk '{ p = $0; sub(/^[[:space:]]+/, "", p); if (substr(p, 1, 2) != "--") print }' "$1"
}

# tree_state <repo> -- a stable snapshot of what a refusal must leave UNCHANGED: status,
# tracked migration paths, and their full binary diff against HEAD. The diff is load-bearing
# for B1, whose tree starts dirty: porcelain alone would stay ` M file` if a broken helper
# changed that already-dirty file again and would falsely report byte-identical state.
tree_state() {
  git -C "$1" status --porcelain
  echo "--migrations--"
  git -C "$1" ls-files -- "$MIGDIR"
  echo "--migration-diff--"
  git -C "$1" diff --no-ext-diff --binary HEAD -- "$MIGDIR"
}

# build_base <casedir> -- create <casedir>/origin.git (bare) + <casedir>/repo, commit a
# main state (base migration + a landed 00230/00231 pair + the numbering check & canary),
# push it to the local bare origin, and check out branch `feature` off that tip. The result
# is a clean, rebased branch onto which a case adds its own branch-new migrations.
build_base() {
  _cd="$1"
  _bare="$_cd/origin.git"
  _repo="$_cd/repo"
  mkdir -p "$_cd"
  git init -q --bare -b main "$_bare"
  git init -q -b main "$_repo"
  git -C "$_repo" config user.email test@example.com
  git -C "$_repo" config user.name "renumber-test"
  git -C "$_repo" config commit.gpgsign false
  git -C "$_repo" remote add origin "$_bare"
  _md="$_repo/$MIGDIR"
  mkdir -p "$_md"
  mk_goose "$_md/00100_base.sql" "base migration"
  mk_goose "$_md/00230_run_branch_moved.sql" "landed pair, main side"
  mk_goose "$_md/00231_validate_run_branch_moved.sql" "landed pair validate, main side"
  mkdir -p "$_repo/scripts"
  cp "$NUMCHECK" "$_repo/scripts/check-migration-numbering.sh"
  chmod +x "$_repo/scripts/check-migration-numbering.sh"
  cp -R "$CANARY" "$_repo/scripts/migration-numbering-canary"
  git -C "$_repo" add -A
  git -C "$_repo" commit -q -m "main: base + landed 00230/00231 pair + numbering check"
  git -C "$_repo" push -q origin main
  git -C "$_repo" checkout -q -b feature
}

# add_forge_pair <repo> -- add and COMMIT a colliding branch-new pair at DISTINCT paths but
# the SAME draft numbers as main's landed pair (00230/00231). 00230_forge_x's header
# cross-references its sibling 00231; 00231_validate_forge_x's header cross-references 00230.
add_forge_pair() {
  _r="$1"
  _m="$_r/$MIGDIR"
  wf "$_m/00230_forge_x.sql" <<'SQL'
-- +goose Up
-- Forge-park branch draft. Its validate companion is 00231_validate_forge_x, which
-- VALIDATEs the constraint this migration adds NOT VALID (draft pair 00230/00231).
ALTER TABLE forge ADD COLUMN parked boolean;
-- +goose Down
ALTER TABLE forge DROP COLUMN parked;
SQL
  wf "$_m/00231_validate_forge_x.sql" <<'SQL'
-- +goose Up
-- Validate the constraint added NOT VALID by 00230_forge_x (draft pair 00230/00231).
ALTER TABLE forge VALIDATE CONSTRAINT forge_parked_check;
-- +goose Down
SELECT 1;
SQL
  git -C "$_r" add -A
  git -C "$_r" commit -q -m "branch: colliding forge pair (draft 00230/00231)"
}

# run_refusal <label> <repo> <expected-message-fragment> -- snapshot the tree, run the
# helper (cwd in the repo), and assert it exits non-zero for the EXPECTED reason and leaves
# the tree byte-identical. Checking only the status would let an earlier unrelated refusal
# make a later case read green (CodeRabbit review on PR #1410).
run_refusal() {
  _label="$1"
  _repo="$2"
  _needle="$3"
  _before="$(tree_state "$_repo")"
  _out="$( ( cd "$_repo" && sh "$HELPER" ) 2>&1 )"
  _rc=$?
  _after="$(tree_state "$_repo")"
  assert_nonzero "$_label" "$_rc"
  assert_contains "$_label: refused for expected reason" "$_needle" "$_out"
  assert_eq "$_label: tree UNCHANGED (nothing renamed/edited)" "$_before" "$_after"
}

# =============================================================================
echo "=== A. --rewrite-comments unit (simultaneous comment remap; body untouched) ==="
# =============================================================================
CASE_A="$ROOT/caseA"
mkdir -p "$CASE_A"
FIX="$CASE_A/00231_pair.sql"
MAP="$CASE_A/map.txt"
# Comment cross-references: draft 00231's header names its sibling 00232, and it references
# base migration 00232 -- 00232 is BOTH a map value (from 00231) and a map key (-> 00233),
# the aliasing that a per-number sed would double-substitute. Body lines carry a 5-digit
# map-key literal (00231) and a 6-digit literal (100232) that MUST survive byte-identical.
wf "$FIX" <<'SQL'
-- +goose Up
-- Pair header: this migration 00231 validates the constraint that base migration 00232 adds.
-- Its own validate companion step is 00232_validate_forge (a sibling draft).
CREATE TABLE demo (id bigint, code integer);
INSERT INTO demo (id, code) VALUES (1, 00231);
INSERT INTO demo (id, code) VALUES (2, 100232);
-- +goose Down
DROP TABLE demo;
SQL
wf "$MAP" <<'MAP'
00231 00232
00232 00233
MAP
BODY_BEFORE="$(body_lines "$FIX")"
A_OUT="$( sh "$HELPER" --rewrite-comments "$MAP" "$FIX" 2>&1 )"
A_RC=$?
: "$A_OUT"
assert_eq "A: --rewrite-comments exits 0" "0" "$A_RC"
A_AFTER="$(cat "$FIX")"
assert_contains "A: comment 00231 -> 00232"                 "this migration 00232 validates"  "$A_AFTER"
assert_contains "A: comment 00232 -> 00233"                 "base migration 00233 adds"       "$A_AFTER"
assert_contains "A: aliased 00232 companion -> 00233"       "00233_validate_forge"            "$A_AFTER"
assert_absent   "A: no double-substitution of 00231->00233" "this migration 00233"            "$A_AFTER"
BODY_AFTER="$(body_lines "$FIX")"
assert_eq       "A: NO SQL body line changed"               "$BODY_BEFORE"                    "$BODY_AFTER"
assert_contains "A: body 5-digit map-key literal preserved" "VALUES (1, 00231);"              "$BODY_AFTER"
assert_contains "A: body 6-digit literal preserved"         "VALUES (2, 100232);"             "$BODY_AFTER"

# An empty or whitespace-only map must fail without truncating the SQL file. With the old
# NR==FNR map detection, an empty first input made every SQL line look like a map record and
# the helper replaced the migration with an empty file (CodeRabbit review on PR #1410).
for _map_kind in empty whitespace; do
  EMPTY_FIX="$CASE_A/${_map_kind}_map.sql"
  EMPTY_MAP="$CASE_A/${_map_kind}_map.txt"
  wf "$EMPTY_FIX" <<'SQL'
-- +goose Up
-- Migration comment 00231 must survive a rejected rewrite.
SELECT 00231;
-- +goose Down
SELECT 1;
SQL
  if [ "$_map_kind" = "empty" ]; then
    : > "$EMPTY_MAP"
  else
    printf '   \n\t\n' > "$EMPTY_MAP"
  fi
  EMPTY_BEFORE="$(cat "$EMPTY_FIX")"
  EMPTY_OUT="$(sh "$HELPER" --rewrite-comments "$EMPTY_MAP" "$EMPTY_FIX" 2>&1)"
  EMPTY_RC=$?
  assert_nonzero "A: $_map_kind map refused" "$EMPTY_RC"
  assert_contains "A: $_map_kind map names expected reason" "map file is empty or whitespace-only" "$EMPTY_OUT"
  assert_eq "A: $_map_kind map leaves SQL byte-identical" "$EMPTY_BEFORE" "$(cat "$EMPTY_FIX")"
done

# =============================================================================
echo "=== B. precondition/preflight refusals (each exits non-zero, changes nothing) ==="
# =============================================================================

# B1. dirty tree: a valid rebased colliding branch with an uncommitted working-tree edit.
CB1="$ROOT/caseB1"; build_base "$CB1"; RB1="$CB1/repo"
add_forge_pair "$RB1"
printf '%s\n' '-- uncommitted edit' >> "$RB1/$MIGDIR/00100_base.sql"
run_refusal "B1 dirty tree" "$RB1" "working tree is not clean"

# B2. unresolvable base: no usable origin, so `git fetch origin main` fails.
CB2="$ROOT/caseB2"; build_base "$CB2"; RB2="$CB2/repo"
add_forge_pair "$RB2"
git -C "$RB2" remote remove origin
run_refusal "B2 unresolvable base (no origin remote)" "$RB2" "git fetch origin main failed"

# B3. not rebased: origin/main advances to a commit that is NOT an ancestor of HEAD.
CB3="$ROOT/caseB3"; build_base "$CB3"; RB3="$CB3/repo"
add_forge_pair "$RB3"
git -C "$RB3" checkout -q main
printf '%s\n' 'main advanced after the branch was cut' > "$RB3/MAIN_ADVANCED.txt"
git -C "$RB3" add -A
git -C "$RB3" commit -q -m "main advances after branch cut"
git -C "$RB3" push -q origin main
git -C "$RB3" checkout -q feature
run_refusal "B3 branch not rebased onto origin/main" "$RB3" "HEAD is not rebased onto current origin/main"

# B4. empty branch-new set: a clean rebased branch that adds NO migration.
CB4="$ROOT/caseB4"; build_base "$CB4"; RB4="$CB4/repo"
wf "$RB4/docs/note.md" <<'MD'
# Note
This branch adds no migration at all.
MD
git -C "$RB4" add -A
git -C "$RB4" commit -q -m "branch: docs only, no migration"
run_refusal "B4 empty branch-new set" "$RB4" "no migrations added by HEAD"

# B5. malformed name: a branch-new migration whose basename is not NNNNN_slug.sql.
CB5="$ROOT/caseB5"; build_base "$CB5"; RB5="$CB5/repo"
wf "$RB5/$MIGDIR/9999_x.sql" <<'SQL'
-- +goose Up
SELECT 1;
-- +goose Down
SELECT 1;
SQL
git -C "$RB5" add -A
git -C "$RB5" commit -q -m "branch: malformed migration name (4-digit prefix)"
run_refusal "B5 malformed migration name" "$RB5" "malformed migration name"

# B6. duplicate old number: two branch-new migrations sharing one 5-digit prefix.
CB6="$ROOT/caseB6"; build_base "$CB6"; RB6="$CB6/repo"
mk_goose "$RB6/$MIGDIR/00240_a.sql" "duplicate prefix a"
mk_goose "$RB6/$MIGDIR/00240_b.sql" "duplicate prefix b"
git -C "$RB6" add -A
git -C "$RB6" commit -q -m "branch: two migrations share prefix 00240"
run_refusal "B6 duplicate old number" "$RB6" "two branch-new migrations share the same 5-digit prefix"

# B7. vacuous main head: the branch adds a valid migration, but origin/main has no numbered
# migrations. The helper must not silently treat that as head 0 and renumber from 00001.
CB7="$ROOT/caseB7"; BARE7="$CB7/origin.git"; RB7="$CB7/repo"
mkdir -p "$CB7"
git init -q --bare -b main "$BARE7"
git init -q -b main "$RB7"
git -C "$RB7" config user.email test@example.com
git -C "$RB7" config user.name "renumber-test"
git -C "$RB7" config commit.gpgsign false
git -C "$RB7" remote add origin "$BARE7"
wf "$RB7/docs/base.md" <<'MD'
# Base without migrations
MD
git -C "$RB7" add -A
git -C "$RB7" commit -q -m "main: no migrations"
git -C "$RB7" push -q origin main
git -C "$RB7" checkout -q -b feature
mkdir -p "$RB7/$MIGDIR"
mk_goose "$RB7/$MIGDIR/00240_new.sql" "branch migration over a vacuous main head"
git -C "$RB7" add -A
git -C "$RB7" commit -q -m "branch: first migration"
run_refusal "B7 vacuous main migration head" "$RB7" "origin/main has no numbered migrations"

# =============================================================================
echo "=== C. integration -- the happy path ==="
# =============================================================================
CASE_C="$ROOT/caseC"; build_base "$CASE_C"; REPOC="$CASE_C/repo"
add_forge_pair "$REPOC"
# A non-migration file the branch adds, referencing OLD draft numbers -> must be REPORTED
# by postflight, never auto-edited.
wf "$REPOC/docs/x.md" <<'MD'
# Forge park notes
See migration 00230 for the forge-park change and 00231 for its validation step.
MD
# A file referencing NO old number -> must be left completely alone.
wf "$REPOC/docs/unrelated.md" <<'MD'
# Unrelated notes
This file mentions no migration number and must be left alone.
MD
git -C "$REPOC" add -A
git -C "$REPOC" commit -q -m "branch: docs referencing old draft numbers"

DOCS_X_BEFORE="$(cat "$REPOC/docs/x.md")"
UNREL_BEFORE="$(cat "$REPOC/docs/unrelated.md")"

C_OUT="$( ( cd "$REPOC" && sh "$HELPER" ) 2>&1 )"
C_RC=$?

MC="$REPOC/$MIGDIR"
assert_eq "C: happy path exits 0" "0" "$C_RC"

# renamed to consecutive slots ABOVE the live head (231): 00232 / 00233 exist ...
if [ -f "$MC/00232_forge_x.sql" ]; then pass "C: 00232_forge_x.sql created"; else fail "C: 00232_forge_x.sql created"; fi
if [ -f "$MC/00233_validate_forge_x.sql" ]; then pass "C: 00233_validate_forge_x.sql created"; else fail "C: 00233_validate_forge_x.sql created"; fi
# ... and the old draft names are gone ...
if [ -f "$MC/00230_forge_x.sql" ]; then fail "C: old 00230_forge_x.sql removed"; else pass "C: old 00230_forge_x.sql removed"; fi
if [ -f "$MC/00231_validate_forge_x.sql" ]; then fail "C: old 00231_validate_forge_x.sql removed"; else pass "C: old 00231_validate_forge_x.sql removed"; fi
# ... while main's landed pair is UNTOUCHED.
if [ -f "$MC/00230_run_branch_moved.sql" ]; then pass "C: main 00230_run_branch_moved.sql untouched"; else fail "C: main 00230_run_branch_moved.sql untouched"; fi
if [ -f "$MC/00231_validate_run_branch_moved.sql" ]; then pass "C: main 00231_validate_run_branch_moved.sql untouched"; else fail "C: main 00231_validate_run_branch_moved.sql untouched"; fi

# the renamed pair's OWN cross-references were rewritten (simultaneous old->new).
NEW232="$(cat "$MC/00232_forge_x.sql")"
assert_contains "C: 00232 header now names 00233" "00233_validate_forge_x" "$NEW232"
NEW233="$(cat "$MC/00233_validate_forge_x.sql")"
assert_contains "C: 00233 header now names 00232" "00232_forge_x" "$NEW233"

# postflight REPORTED the stale reference in the non-migration file (path + OLD -> NEW) ...
assert_contains "C: postflight reports docs/x.md"      "docs/x.md"     "$C_OUT"
assert_contains "C: postflight suggests 00230 -> 00232" "00230 -> 00232" "$C_OUT"
assert_contains "C: postflight suggests 00231 -> 00233" "00231 -> 00233" "$C_OUT"
# ... but did NOT auto-edit it, and never mentioned the unrelated file.
assert_eq     "C: docs/x.md content UNCHANGED (not auto-edited)" "$DOCS_X_BEFORE" "$(cat "$REPOC/docs/x.md")"
assert_eq     "C: unrelated file UNCHANGED"                      "$UNREL_BEFORE"  "$(cat "$REPOC/docs/unrelated.md")"
assert_absent "C: unrelated file NOT in the postflight report"   "unrelated.md"   "$C_OUT"

# the numbering self-check is GREEN in the renumbered tree (rc 0).
( cd "$REPOC" && ./scripts/check-migration-numbering.sh scripts/migration-numbering-canary "$MIGDIR" ) >/dev/null 2>&1
C_NUMRC=$?
assert_eq "C: check-migration-numbering.sh green after renumber" "0" "$C_NUMRC"

# =============================================================================
echo "=== D. integration -- two-phase rename over an OVERLAPPING range ==="
# =============================================================================
# The whole reason migration-renumber.sh renames in TWO phases (old -> temp -> new) is to
# survive a rename whose NEW path equals another branch file's still-present OLD path. That
# happens when a SAME-SLUG pair straddles the head: draft 00231_dup / 00232_dup with live
# head 00231 -> new 00232_dup / 00233_dup, so a direct `git mv 00231_dup -> 00232_dup` would
# collide with the still-present 00232_dup. A single-phase helper FAILS this (git mv refuses
# to overwrite); the two-phase rename renames it correctly. Distinguishing body markers prove
# IDENTITY is preserved (no clobber/swap), not merely that the new numbers now exist. Case C
# (distinct slugs, no path overlap) does NOT exercise this: single-phase would pass it.
CASE_D="$ROOT/caseD"; build_base "$CASE_D"; REPOD="$CASE_D/repo"
MD_D="$REPOD/$MIGDIR"
printf '%s\n' '-- +goose Up' '-- MARK_A: this file was drafted as 00231_dup' 'SELECT 1;' '-- +goose Down' 'SELECT 1;' > "$MD_D/00231_dup.sql"
printf '%s\n' '-- +goose Up' '-- MARK_B: this file was drafted as 00232_dup' 'SELECT 1;' '-- +goose Down' 'SELECT 1;' > "$MD_D/00232_dup.sql"
git -C "$REPOD" add -A
git -C "$REPOD" commit -q -m "branch: same-slug pair 00231_dup/00232_dup straddling head 00231"

D_OUT="$( ( cd "$REPOD" && sh "$HELPER" ) 2>&1 )"
D_RC=$?
: "$D_OUT"
assert_eq "D: overlapping-range renumber exits 0 (single-phase git mv would collide)" "0" "$D_RC"
if [ -f "$MD_D/00232_dup.sql" ]; then pass "D: 00232_dup.sql created"; else fail "D: 00232_dup.sql created"; fi
if [ -f "$MD_D/00233_dup.sql" ]; then pass "D: 00233_dup.sql created"; else fail "D: 00233_dup.sql created"; fi
if [ -f "$MD_D/00231_dup.sql" ]; then fail "D: old 00231_dup.sql removed"; else pass "D: old 00231_dup.sql removed"; fi
# Identity preserved by the two-phase rename: the file drafted 00231_dup is now 00232_dup
# (MARK_A), the file drafted 00232_dup is now 00233_dup (MARK_B) -- not clobbered or swapped.
# (MARK_A/MARK_B are number-free, so the comment number-remap cannot perturb them.)
assert_contains "D: 00232_dup carries MARK_A (was 00231_dup)" "MARK_A" "$(cat "$MD_D/00232_dup.sql" 2>/dev/null)"
assert_contains "D: 00233_dup carries MARK_B (was 00232_dup)" "MARK_B" "$(cat "$MD_D/00233_dup.sql" 2>/dev/null)"
# No stray two-phase temp file left behind (POSIX glob: an explicit leading dot matches
# dotfiles; a no-match glob stays literal, so the -e test is false and leftover stays empty).
_leftover=""
for _t in "$MD_D"/.renumber-tmp-*; do
  [ -e "$_t" ] && _leftover="$_leftover $_t"
done
assert_eq "D: no leftover .renumber-tmp-* file after success" "" "$_leftover"
# Numbering check green after the overlapping renumber.
( cd "$REPOD" && ./scripts/check-migration-numbering.sh scripts/migration-numbering-canary "$MIGDIR" ) >/dev/null 2>&1
D_NUMRC=$?
assert_eq "D: check-migration-numbering.sh green after overlapping renumber" "0" "$D_NUMRC"

# --- tally -------------------------------------------------------------------
TOTAL=$((PASSES + FAILS))
# A visible tally so a zero-case crash that still reached here cannot read green.
MIN_ASSERTIONS=30
echo
echo "=== migration-renumber-test: $PASSES passed, $FAILS failed ($TOTAL assertions) ==="
if [ "$TOTAL" -lt "$MIN_ASSERTIONS" ]; then
  echo "migration-renumber-test: only $TOTAL assertions ran (< $MIN_ASSERTIONS) -- the harness" >&2
  echo "  did not run to completion; treating as an instrument failure." >&2
  exit 2
fi
[ "$FAILS" -eq 0 ]
