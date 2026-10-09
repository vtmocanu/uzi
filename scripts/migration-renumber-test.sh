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
#   E. a binary file in the branch diff is skipped with a notice (not scanned, not
#      touched), and a non-UTF-8 text file is still scanned (issue 1581).
#   F. a reference-scan failure refuses BEFORE any rename, leaving the tree unchanged.
#   G. a colliding draft number rewrites only the branch filename/stem identity, leaving
#      main identities and ambiguous references unchanged and reporting them for review.
#   H. unresolved comments alone trigger the manual-review report and summary.
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

# Keep fixtures and every helper scratch directory inside the runtime scratch area.
SCRATCH="$(cd "$SCRIPTS_DIR/.." && pwd)/.uzi/scratch"
mkdir -p "$SCRATCH"
TMPDIR="$SCRATCH"
export TMPDIR
# Ambient mode must not change the default-mode cases.
unset MIGRATION_RENUMBER_NO_FETCH
ROOT="$(mktemp -d "$TMPDIR/migration-renumber-test.XXXXXX")"
TMPDIR="$ROOT"
export TMPDIR
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

# run_refusal <label> <repo> <expected-message-fragment> [path] [mode] -- snapshot the tree, run
# the helper (cwd in the repo), and assert it exits non-zero for the EXPECTED reason and
# leaves the tree byte-identical. Checking only the status would let an earlier unrelated
# refusal make a later case read green (CodeRabbit review on PR #1410). The optional PATH
# lets cases inject status/fetch failures; mode defaults to 0.
run_refusal() {
  _label="$1"
  _repo="$2"
  _needle="$3"
  _path="${4:-$PATH}"
  _before="$(tree_state "$_repo")"
  _out="$( ( cd "$_repo" && PATH="$_path" MIGRATION_RENUMBER_NO_FETCH="${5-0}" sh "$HELPER" ) 2>&1 )"
  _rc=$?
  _after="$(tree_state "$_repo")"
  assert_nonzero "$_label" "$_rc"
  assert_contains "$_label: refused for expected reason" "$_needle" "$_out"
  assert_eq "$_label: tree UNCHANGED (nothing renamed/edited)" "$_before" "$_after"
}

# fetch_failing_wrapper <case-dir> -- delegate all git except fetch, which records an
# attempt outside the fixture repo and fails. An empty marker proves no fetch was tried.
fetch_failing_wrapper() {
  _wrapdir="$1/bin"
  _real_git="$(command -v git)"
  mkdir -p "$_wrapdir"
  : > "$1/fetch-attempts"
  wf "$_wrapdir/git" <<EOF
#!/bin/sh
if [ "\${1:-}" = "fetch" ]; then
  printf '%s\\n' "\$*" >> "$1/fetch-attempts"
  exit 73
fi
exec "$_real_git" "\$@"
EOF
  chmod +x "$_wrapdir/git"
}

# =============================================================================
echo "=== G. identity collision through public main entry ==="
# =============================================================================
CG="$ROOT/caseG"; build_base "$CG"; RG="$CG/repo"
git -C "$RG" checkout -q main
mk_goose "$RG/$MIGDIR/00260_main_change.sql" "main identity"
git -C "$RG" add -- "$MIGDIR/00260_main_change.sql"
git -C "$RG" commit -q -m "main: migration 260"
git -C "$RG" push -q origin main
git -C "$RG" checkout -q feature
git -C "$RG" merge -q --ff-only main
wf "$RG/$MIGDIR/00260_branch_change.sql" <<'SQL'
-- +goose Up
-- Branch: 00260_branch_change.sql, 00260_branch_change; api/internal/store/migrations/00260_branch_change.sql.
-- Punctuation: (00260_branch_change.sql), [00260_branch_change]; 00260_branch_change.
-- Main: 00260_main_change.sql and 00260_main_change; bare 00260.
-- Ambiguous: 00260_branch_change.sql.bak, 00260_branch_change_extra, x00260_branch_change.
SELECT '00260_branch_change.sql', 00260;
-- +goose Down
SELECT 1;
SQL
wf "$CG/expected.sql" <<'SQL'
-- +goose Up
-- Branch: 00261_branch_change.sql, 00261_branch_change; api/internal/store/migrations/00261_branch_change.sql.
-- Punctuation: (00261_branch_change.sql), [00261_branch_change]; 00261_branch_change.
-- Main: 00260_main_change.sql and 00260_main_change; bare 00260.
-- Ambiguous: 00260_branch_change.sql.bak, 00260_branch_change_extra, x00260_branch_change.
SELECT '00260_branch_change.sql', 00260;
-- +goose Down
SELECT 1;
SQL
git -C "$RG" add -- "$MIGDIR/00260_branch_change.sql"
git -C "$RG" commit -q -m "branch: colliding identity"
cp "$RG/$MIGDIR/00260_main_change.sql" "$CG/main-before.sql"
G_BODY="$(body_lines "$RG/$MIGDIR/00260_branch_change.sql")"
G_OUT="$( ( cd "$RG" && sh "$HELPER" ) 2>&1 )"; G_RC=$?
G_NEW="$RG/$MIGDIR/00261_branch_change.sql"
assert_eq "G: public collision entry succeeds" "0" "$G_RC"
if cmp -s "$CG/expected.sql" "$G_NEW"; then pass "G: mixed identity whole-file comparison"; else fail "G: mixed identity whole-file comparison"; fi
assert_contains "G: main slug and bare number unchanged" "-- Main: 00260_main_change.sql and 00260_main_change; bare 00260." "$(cat "$G_NEW")"
assert_contains "G: ambiguous boundaries unchanged" "-- Ambiguous: 00260_branch_change.sql.bak, 00260_branch_change_extra, x00260_branch_change." "$(cat "$G_NEW")"
assert_eq "G: body line content unchanged" "$G_BODY" "$(body_lines "$G_NEW")"
assert_contains "G: unresolved comment path and line" "$MIGDIR/00261_branch_change.sql:4" "$G_OUT"
assert_contains "G: ambiguous comment path and line" "$MIGDIR/00261_branch_change.sql:5" "$G_OUT"
assert_contains "G: ambiguous comment content" "-- Ambiguous: 00260_branch_change.sql.bak, 00260_branch_change_extra, x00260_branch_change." "$G_OUT"
assert_contains "G: unresolved comment content" "-- Main: 00260_main_change.sql and 00260_main_change; bare 00260." "$G_OUT"
assert_contains "G: ambiguous-only summary requires manual review" "manual" "$G_OUT"
if cmp -s "$CG/main-before.sql" "$RG/$MIGDIR/00260_main_change.sql"; then pass "G: main file byte-identical"; else fail "G: main file byte-identical"; fi

# No identity cross-reference and no other-file hit: the unresolved comment alone
# must still appear in postflight and make the final summary request manual review.
CH="$ROOT/caseH"; build_base "$CH"; RH="$CH/repo"
mk_goose "$RH/$MIGDIR/00230_ambiguous.sql" "bare draft 00230; main 00230_run_branch_moved.sql"
git -C "$RH" add -- "$MIGDIR/00230_ambiguous.sql"
git -C "$RH" commit -q -m "branch: ambiguous references only"
cp "$RH/$MIGDIR/00230_ambiguous.sql" "$CH/expected.sql"
H_OUT="$( ( cd "$RH" && sh "$HELPER" ) 2>&1 )"; H_RC=$?
assert_eq "H: ambiguous-only renumber exits 0" "0" "$H_RC"
if cmp -s "$CH/expected.sql" "$RH/$MIGDIR/00232_ambiguous.sql"; then pass "H: unresolved references unchanged"; else fail "H: unresolved references unchanged"; fi
assert_contains "H: unresolved path/line/content reported" "$MIGDIR/00232_ambiguous.sql:2  | -- bare draft 00230; main 00230_run_branch_moved.sql" "$H_OUT"
assert_contains "H: summary requests manual review without other-file hits" "auto-edited and still need manual review" "$H_OUT"

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
-- Pair header: this migration 00231_pair validates the constraint that base migration 00232_validate_forge adds.
-- Bare numbers 00231/00232 and other slug 00231_other remain unresolved.
-- Its own validate companion step is 00232_validate_forge (a sibling draft).
CREATE TABLE demo (id bigint, code integer);
INSERT INTO demo (id, code) VALUES (1, 00231);
INSERT INTO demo (id, code) VALUES (2, 100232);
-- +goose Down
DROP TABLE demo;
SQL
printf '00231\t00232\t00231_pair.sql\n00232\t00233\t00232_validate_forge.sql\n' > "$MAP"
BODY_BEFORE="$(body_lines "$FIX")"
A_OUT="$( sh "$HELPER" --rewrite-comments "$MAP" "$FIX" 2>&1 )"
A_RC=$?
: "$A_OUT"
assert_eq "A: --rewrite-comments exits 0" "0" "$A_RC"
A_AFTER="$(cat "$FIX")"
wf "$CASE_A/expected.sql" <<'SQL'
-- +goose Up
-- Pair header: this migration 00232_pair validates the constraint that base migration 00233_validate_forge adds.
-- Bare numbers 00231/00232 and other slug 00231_other remain unresolved.
-- Its own validate companion step is 00233_validate_forge (a sibling draft).
CREATE TABLE demo (id bigint, code integer);
INSERT INTO demo (id, code) VALUES (1, 00231);
INSERT INTO demo (id, code) VALUES (2, 100232);
-- +goose Down
DROP TABLE demo;
SQL
if cmp -s "$CASE_A/expected.sql" "$FIX"; then pass "A: mixed overlapping references whole-file comparison"; else fail "A: mixed overlapping references whole-file comparison"; fi
assert_contains "A: comment 00231 -> 00232"                 "this migration 00232_pair validates"  "$A_AFTER"
assert_contains "A: comment 00232 -> 00233"                 "base migration 00233_validate_forge adds"       "$A_AFTER"
assert_contains "A: aliased 00232 companion -> 00233"       "00233_validate_forge"            "$A_AFTER"
assert_absent   "A: no double-substitution of 00231->00233" "this migration 00233_pair"            "$A_AFTER"
BODY_AFTER="$(body_lines "$FIX")"
assert_eq       "A: NO SQL body line changed"               "$BODY_BEFORE"                    "$BODY_AFTER"
assert_contains "A: body 5-digit map-key literal preserved" "VALUES (1, 00231);"              "$BODY_AFTER"
assert_contains "A: body 6-digit literal preserved"         "VALUES (2, 100232);"             "$BODY_AFTER"

# A dotted slug is a basename too; validation must not invent an alphanumeric-only
# restriction beyond the existing migration-name contract.
printf '00231\t00232\t00231_pair.v2.sql\n' > "$CASE_A/dotted-map.txt"
printf '%s\n' '-- See 00231_pair.v2.sql and 00231_pair.v2.' 'SELECT 00231;' > "$CASE_A/dotted.sql"
printf '%s\n' '-- See 00232_pair.v2.sql and 00232_pair.v2.' 'SELECT 00231;' > "$CASE_A/dotted-expected.sql"
DOTTED_OUT="$(sh "$HELPER" --rewrite-comments "$CASE_A/dotted-map.txt" "$CASE_A/dotted.sql" 2>&1)"
DOTTED_RC=$?
assert_eq "A: path-free dotted basename accepted" "0" "$DOTTED_RC"
if cmp -s "$CASE_A/dotted-expected.sql" "$CASE_A/dotted.sql"; then pass "A: dotted filename/stem identities rewritten"; else fail "A: dotted filename/stem identities rewritten: $DOTTED_OUT"; fi

# A-dot-tail: longer dotted identities must stay unchanged and be reported, even
# when the character after the dot is a hyphen or underscore.
printf '00231\t00233\t00231_pair.sql\n' > "$CASE_A/dot-tail-map.txt"
wf "$CASE_A/dot-tail.sql" <<'SQL'
-- See 00231_pair.-other.sql
-- See 00231_pair._other.sql
-- See 00231_pair.sql.-other.sql
-- See 00231_pair.sql._other.sql
SELECT '00231_pair.-other.sql';
SQL
cp "$CASE_A/dot-tail.sql" "$CASE_A/dot-tail-expected.sql"
DOT_TAIL_OUT="$(sh "$HELPER" --rewrite-comments "$CASE_A/dot-tail-map.txt" "$CASE_A/dot-tail.sql" 2>&1)"
DOT_TAIL_RC=$?
assert_eq "A-dot-tail: rewrite succeeds" "0" "$DOT_TAIL_RC"
if cmp -s "$CASE_A/dot-tail-expected.sql" "$CASE_A/dot-tail.sql"; then pass "A-dot-tail: longer dotted identities whole-file comparison"; else fail "A-dot-tail: longer dotted identities whole-file comparison"; fi
DOT_TAIL_EXPECTED="$(printf '%s:1\t%s\n%s:2\t%s\n%s:3\t%s\n%s:4\t%s\n' "$CASE_A/dot-tail.sql" '-- See 00231_pair.-other.sql' "$CASE_A/dot-tail.sql" '-- See 00231_pair._other.sql' "$CASE_A/dot-tail.sql" '-- See 00231_pair.sql.-other.sql' "$CASE_A/dot-tail.sql" '-- See 00231_pair.sql._other.sql')"
assert_eq "A-dot-tail: unresolved path/line/content diagnostics" "$DOT_TAIL_EXPECTED" "$DOT_TAIL_OUT"

# A-atomic-dotted: consume the whole matched identity so a mapped number inside
# its slug is preserved, for both full basename and extensionless stem.
printf '00231\t00233\t00231_x.00232_y.sql\n00232\t00234\t00232_y.sql\n' > "$CASE_A/atomic-dotted-map.txt"
wf "$CASE_A/atomic-dotted.sql" <<'SQL'
-- See 00231_x.00232_y.sql
-- See 00231_x.00232_y.
-- Path: api/internal/store/migrations/00231_x.00232_y.sql, (00232_y).
SELECT '00231_x.00232_y.sql';
SQL
wf "$CASE_A/atomic-dotted-expected.sql" <<'SQL'
-- See 00233_x.00232_y.sql
-- See 00233_x.00232_y.
-- Path: api/internal/store/migrations/00233_x.00232_y.sql, (00234_y).
SELECT '00231_x.00232_y.sql';
SQL
ATOMIC_OUT="$(sh "$HELPER" --rewrite-comments "$CASE_A/atomic-dotted-map.txt" "$CASE_A/atomic-dotted.sql" 2>&1)"
ATOMIC_RC=$?
assert_eq "A-atomic-dotted: rewrite succeeds" "0" "$ATOMIC_RC"
if cmp -s "$CASE_A/atomic-dotted-expected.sql" "$CASE_A/atomic-dotted.sql"; then pass "A-atomic-dotted: filename/stem whole-file comparison"; else fail "A-atomic-dotted: filename/stem whole-file comparison"; fi
assert_eq "A-atomic-dotted: no internal-number diagnostic" "" "$ATOMIC_OUT"

# A-dot-left-boundary: the suffix of an unknown dotted filename or stem is not
# a standalone mapped identity.
wf "$CASE_A/dot-left.sql" <<'SQL'
-- See unknown.00232_y.sql
-- See unknown.00232_y.
-- See 00231_unknown.00232_y.sql
-- See 00231_unknown.00232_y.
SELECT 'unknown.00232_y.sql';
SQL
cp "$CASE_A/dot-left.sql" "$CASE_A/dot-left-expected.sql"
DOT_LEFT_OUT="$(sh "$HELPER" --rewrite-comments "$CASE_A/atomic-dotted-map.txt" "$CASE_A/dot-left.sql" 2>&1)"
DOT_LEFT_RC=$?
assert_eq "A-dot-left-boundary: rewrite succeeds" "0" "$DOT_LEFT_RC"
if cmp -s "$CASE_A/dot-left-expected.sql" "$CASE_A/dot-left.sql"; then pass "A-dot-left-boundary: unknown filename/stem whole-file comparison"; else fail "A-dot-left-boundary: unknown filename/stem whole-file comparison"; fi
DOT_LEFT_EXPECTED="$(printf '%s:1\t%s\n%s:2\t%s\n%s:3\t%s\n%s:4\t%s\n' "$CASE_A/dot-left.sql" '-- See unknown.00232_y.sql' "$CASE_A/dot-left.sql" '-- See unknown.00232_y.' "$CASE_A/dot-left.sql" '-- See 00231_unknown.00232_y.sql' "$CASE_A/dot-left.sql" '-- See 00231_unknown.00232_y.')"
assert_eq "A-dot-left-boundary: unresolved path/line/content diagnostics" "$DOT_LEFT_EXPECTED" "$DOT_LEFT_OUT"

# The main preflight accepts spaces in slugs. Drive the generated identity map through
# the public entry and compare filename/stem rewrites and preserved SQL bytes together.
CASPACE="$ROOT/caseAspace"; build_base "$CASPACE"; RASPACE="$CASPACE/repo"
wf "$RASPACE/$MIGDIR/00230_x y.sql" <<'SQL'
-- +goose Up
-- See 00230_x y.sql and 00230_x y.
SELECT '00230_x y.sql', 00230;
-- +goose Down
SELECT 1;
SQL
wf "$CASPACE/expected.sql" <<'SQL'
-- +goose Up
-- See 00232_x y.sql and 00232_x y.
SELECT '00230_x y.sql', 00230;
-- +goose Down
SELECT 1;
SQL
git -C "$RASPACE" add -- "$MIGDIR/00230_x y.sql"
git -C "$RASPACE" commit -q -m "branch: space-containing migration slug"
SPACE_OUT="$( ( cd "$RASPACE" && sh "$HELPER" ) 2>&1 )"
SPACE_RC=$?
assert_eq "A: generated map accepts space-containing basename" "0" "$SPACE_RC"
if cmp -s "$CASPACE/expected.sql" "$RASPACE/$MIGDIR/00232_x y.sql"; then pass "A: space filename/stem identities whole-file comparison"; else fail "A: space filename/stem identities whole-file comparison: $SPACE_OUT"; fi
if [ ! -e "$RASPACE/$MIGDIR/00230_x y.sql" ]; then pass "A: old space-containing filename removed"; else fail "A: old space-containing filename removed"; fi

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
  cp "$EMPTY_FIX" "$CASE_A/rejected-before.sql"
  EMPTY_OUT="$(sh "$HELPER" --rewrite-comments "$EMPTY_MAP" "$EMPTY_FIX" 2>&1)"
  EMPTY_RC=$?
  assert_nonzero "A: $_map_kind map refused" "$EMPTY_RC"
  assert_contains "A: $_map_kind map names expected reason" "map file is empty, whitespace-only, or malformed" "$EMPTY_OUT"
  if cmp -s "$CASE_A/rejected-before.sql" "$EMPTY_FIX"; then pass "A: $_map_kind map leaves SQL byte-identical"; else fail "A: $_map_kind map leaves SQL byte-identical"; fi
done

# A nonempty malformed map must also fail closed. A missing NEW value used to create an
# empty mapping and silently delete the OLD token; duplicate OLD keys are ambiguous.
for _bad_kind in missing-value duplicate-old duplicate-new numeric-only non-tab extra-field wrong-prefix path backslash-path bad-old bad-new bad-basename blank-row; do
  BAD_FIX="$CASE_A/${_bad_kind}_map.sql"
  BAD_MAP="$CASE_A/${_bad_kind}_map.txt"
  wf "$BAD_FIX" <<'SQL'
-- +goose Up
-- Companion migration is 00231_validate_x.
SELECT 00231;
-- +goose Down
SELECT 1;
SQL
  case "$_bad_kind" in
    missing-value) printf '00231\n' > "$BAD_MAP" ;;
    duplicate-old) printf '00231\t00232\t00231_x.sql\n00231\t00233\t00231_y.sql\n' > "$BAD_MAP" ;;
    duplicate-new) printf '00231\t00233\t00231_x.sql\n00232\t00233\t00232_y.sql\n' > "$BAD_MAP" ;;
    numeric-only) printf '00231\t00232\n' > "$BAD_MAP" ;;
    non-tab) printf '00231 00232 00231_x.sql\n' > "$BAD_MAP" ;;
    extra-field) printf '00231\t00232\t00231_x.sql\textra\n' > "$BAD_MAP" ;;
    wrong-prefix) printf '00231\t00232\t00230_x.sql\n' > "$BAD_MAP" ;;
    path) printf '00231\t00232\tdir/00231_x.sql\n' > "$BAD_MAP" ;;
    backslash-path) printf '00231\t00232\tdir\\\\00231_x.sql\n' > "$BAD_MAP" ;;
    bad-old) printf '0231\t00232\t00231_x.sql\n' > "$BAD_MAP" ;;
    bad-new) printf '00231\t100232\t00231_x.sql\n' > "$BAD_MAP" ;;
    bad-basename) printf '00231\t00232\t00231_x.sql.bak\n' > "$BAD_MAP" ;;
    blank-row) printf '00231\t00232\t00231_x.sql\n\n' > "$BAD_MAP" ;;
  esac
  cp "$BAD_FIX" "$CASE_A/rejected-before.sql"
  BAD_OUT="$(sh "$HELPER" --rewrite-comments "$BAD_MAP" "$BAD_FIX" 2>&1)"
  BAD_RC=$?
  assert_nonzero "A: $_bad_kind map refused" "$BAD_RC"
  assert_contains "A: $_bad_kind map names expected reason" "map file is empty, whitespace-only, or malformed" "$BAD_OUT"
  if cmp -s "$CASE_A/rejected-before.sql" "$BAD_FIX"; then pass "A: $_bad_kind map leaves SQL byte-identical"; else fail "A: $_bad_kind map leaves SQL byte-identical"; fi
done

# =============================================================================
echo "=== B. precondition/preflight refusals (each exits non-zero, changes nothing) ==="
# =============================================================================

# B1. dirty tree: a valid rebased colliding branch with an uncommitted working-tree edit.
CB1="$ROOT/caseB1"; build_base "$CB1"; RB1="$CB1/repo"
add_forge_pair "$RB1"
printf '%s\n' '-- uncommitted edit' >> "$RB1/$MIGDIR/00100_base.sql"
run_refusal "B1 dirty tree" "$RB1" "working tree is not clean"

# B1b. git status itself fails: an unknown tree state must be a refusal, never misread as
# clean. A PATH-local wrapper fails only that command and delegates every other git call.
CB1B="$ROOT/caseB1b"; build_base "$CB1B"; RB1B="$CB1B/repo"
add_forge_pair "$RB1B"
FAKEBIN1B="$CB1B/bin"
REAL_GIT1B="$(command -v git)"
mkdir -p "$FAKEBIN1B"
wf "$FAKEBIN1B/git" <<EOF
#!/bin/sh
if [ "\${1:-}" = "status" ] && [ "\${2:-}" = "--porcelain" ]; then
  exit 73
fi
exec "$REAL_GIT1B" "\$@"
EOF
chmod +x "$FAKEBIN1B/git"
run_refusal "B1b git status failure" "$RB1B" "git status --porcelain failed" "$FAKEBIN1B:$PATH"

# B2. unresolvable base: no usable origin, so `git fetch origin main` fails.
CB2="$ROOT/caseB2"; build_base "$CB2"; RB2="$CB2/repo"
add_forge_pair "$RB2"
git -C "$RB2" remote remove origin
run_refusal "B2 unresolvable base (no origin remote)" "$RB2" "git fetch origin main failed"

# B3. missing ancestry: origin/main advances to a commit that is NOT an ancestor of HEAD.
CB3="$ROOT/caseB3"; build_base "$CB3"; RB3="$CB3/repo"
add_forge_pair "$RB3"
git -C "$RB3" checkout -q main
printf '%s\n' 'main advanced after the branch was cut' > "$RB3/MAIN_ADVANCED.txt"
git -C "$RB3" add -A
git -C "$RB3" commit -q -m "main advances after branch cut"
git -C "$RB3" push -q origin main
git -C "$RB3" checkout -q feature
run_refusal "B3 branch missing origin/main ancestry" "$RB3" "origin/main is not an ancestor of HEAD"

# B3b. Unset and explicit 0 both attempt fetch, even with a valid local ref.
CB3B="$ROOT/caseB3b"; build_base "$CB3B"; RB3B="$CB3B/repo"
add_forge_pair "$RB3B"
fetch_failing_wrapper "$CB3B"
B3B_BEFORE="$(tree_state "$RB3B")"
B3B_OUT="$( ( cd "$RB3B" && PATH="$CB3B/bin:$PATH" sh "$HELPER" ) 2>&1 )"
B3B_RC=$?
assert_nonzero "B3b unset mode refuses failed fetch" "$B3B_RC"
assert_contains "B3b unset mode fetch refusal" "git fetch origin main failed" "$B3B_OUT"
assert_eq "B3b unset mode tree unchanged" "$B3B_BEFORE" "$(tree_state "$RB3B")"
assert_eq "B3b unset mode attempts fetch" "fetch origin main" "$(cat "$CB3B/fetch-attempts")"
: > "$CB3B/fetch-attempts"
run_refusal "B3b explicit 0 fetch refusal" "$RB3B" "git fetch origin main failed" "$CB3B/bin:$PATH" 0
assert_eq "B3b explicit 0 attempts fetch" "fetch origin main" "$(cat "$CB3B/fetch-attempts")"

# B3c. Offline mode refuses an absent tracking ref without attempting fetch.
CB3C="$ROOT/caseB3c"; build_base "$CB3C"; RB3C="$CB3C/repo"
add_forge_pair "$RB3C"
git -C "$RB3C" update-ref -d refs/remotes/origin/main
fetch_failing_wrapper "$CB3C"
run_refusal "B3c offline missing local ref" "$RB3C" "local origin/main is not resolvable" "$CB3C/bin:$PATH" 1
assert_eq "B3c no fetch attempted" "" "$(cat "$CB3C/fetch-attempts")"

# B3d. Strict validation rejects empty, numeric aliases, and text before mutation/fetch.
CB3D="$ROOT/caseB3d"; build_base "$CB3D"; RB3D="$CB3D/repo"
add_forge_pair "$RB3D"
fetch_failing_wrapper "$CB3D"
for BAD_MODE in "" 2 01 true; do
  run_refusal "B3d invalid mode '$BAD_MODE'" "$RB3D" "MIGRATION_RENUMBER_NO_FETCH must be unset, 0, or 1" "$CB3D/bin:$PATH" "$BAD_MODE"
done
assert_eq "B3d invalid modes never fetch" "" "$(cat "$CB3D/fetch-attempts")"

# B3e. Offline mode retains the dirty-tree and ancestry refusals.
fetch_failing_wrapper "$CB1"
run_refusal "B3e offline dirty tree" "$RB1" "working tree is not clean" "$CB1/bin:$PATH" 1
assert_eq "B3e dirty tree never fetches" "" "$(cat "$CB1/fetch-attempts")"
fetch_failing_wrapper "$CB3"
run_refusal "B3e offline missing ancestry" "$RB3" "origin/main is not an ancestor of HEAD" "$CB3/bin:$PATH" 1
assert_eq "B3e missing ancestry never fetches" "" "$(cat "$CB3/fetch-attempts")"

# B3f. A genuine merge of diverged main/feature histories satisfies ancestry. Main's
# extra migration advances the live head to 232; the colliding pair must become 233/234.
CB3F="$ROOT/caseB3f"; build_base "$CB3F"; RB3F="$CB3F/repo"
add_forge_pair "$RB3F"
git -C "$RB3F" checkout -q main
mk_goose "$RB3F/$MIGDIR/00232_landed.sql" "main advanced independently"
git -C "$RB3F" add -- "$MIGDIR/00232_landed.sql"
git -C "$RB3F" commit -q -m "main: independently landed migration"
git -C "$RB3F" push -q origin main
git -C "$RB3F" checkout -q feature
git -C "$RB3F" merge -q --no-ff main -m "feature: integrate main by merge"
assert_eq "B3f genuine merge has two parents" "3" "$(git -C "$RB3F" rev-list --parents -n 1 HEAD | awk '{ print NF }')"
git -C "$RB3F" merge-base --is-ancestor origin/main HEAD
assert_eq "B3f origin/main is ancestor" "0" "$?"
B3F_MAIN_BEFORE="$(cat "$RB3F/$MIGDIR/00232_landed.sql")"
B3F_BODY_BEFORE="$(body_lines "$RB3F/$MIGDIR/00230_forge_x.sql")"
fetch_failing_wrapper "$CB3F"
B3F_OUT="$( ( cd "$RB3F" && PATH="$CB3F/bin:$PATH" MIGRATION_RENUMBER_NO_FETCH=1 sh "$HELPER" ) 2>&1 )"
B3F_RC=$?
assert_eq "B3f offline merge renumber exits 0" "0" "$B3F_RC"
assert_contains "B3f summary uses local main head" "renamed 2 migration(s) above live main head 232" "$B3F_OUT"
assert_eq "B3f offline merge never fetches" "" "$(cat "$CB3F/fetch-attempts")"
assert_contains "B3f first migration renumbered" "00233_forge_x.sql" "$(git -C "$RB3F" ls-files -- "$MIGDIR")"
assert_contains "B3f second migration renumbered" "00234_validate_forge_x.sql" "$(git -C "$RB3F" ls-files -- "$MIGDIR")"
assert_absent "B3f old first path removed" "00230_forge_x.sql" "$(git -C "$RB3F" ls-files -- "$MIGDIR")"
assert_absent "B3f old second path removed" "00231_validate_forge_x.sql" "$(git -C "$RB3F" ls-files -- "$MIGDIR")"
assert_contains "B3f forward cross-reference rewritten" "00234_validate_forge_x" "$(cat "$RB3F/$MIGDIR/00233_forge_x.sql")"
assert_contains "B3f backward cross-reference rewritten" "00233_forge_x" "$(cat "$RB3F/$MIGDIR/00234_validate_forge_x.sql")"
assert_eq "B3f SQL body unchanged" "$B3F_BODY_BEFORE" "$(body_lines "$RB3F/$MIGDIR/00233_forge_x.sql")"
assert_eq "B3f main migration unchanged" "$B3F_MAIN_BEFORE" "$(cat "$RB3F/$MIGDIR/00232_landed.sql")"
( cd "$RB3F" && ./scripts/check-migration-numbering.sh scripts/migration-numbering-canary "$MIGDIR" ) >/dev/null 2>&1
assert_eq "B3f renumbered merged tree passes numbering check" "0" "$?"

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
assert_absent "D: clean identity-only summary has no manual review" "manual" "$D_OUT"
assert_absent "D: overlap does not report inserted old-range number" "unresolved renamed-migration" "$D_OUT"
assert_contains "D: first identity rewritten exactly once" "MARK_A: this file was drafted as 00232_dup" "$(cat "$MD_D/00232_dup.sql")"
assert_contains "D: second identity rewritten exactly once" "MARK_B: this file was drafted as 00233_dup" "$(cat "$MD_D/00233_dup.sql")"
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

# =============================================================================
echo "=== E. integration -- binary and non-UTF-8 files in the branch diff (issue 1581) ==="
# =============================================================================
# A PNG in the branch diff made the postflight awk scan die on its bytes under a UTF-8
# locale ("towc: multibyte conversion failure") AFTER the git mv, leaving a staged rename
# and a nonzero exit that a rerun then refused on the dirty tree. The helper must skip the
# binary file with a notice, and still scan a non-UTF-8 TEXT file byte-wise.
CASE_E="$ROOT/caseE"; build_base "$CASE_E"; REPOE="$CASE_E/repo"
add_forge_pair "$REPOE"
mkdir -p "$REPOE/docs/img"
# PNG signature + a NUL-bearing IHDR chunk and high bytes: git classifies it binary.
printf '\211PNG\r\n\032\n\000\000\000\rIHDR\000\000\000\001\377\376\351\000' > "$REPOE/docs/img/shot.png"
# Latin-1 text (0xE9, no NUL): git calls it text, and it references an OLD draft number.
printf 'Caf\351 notes: see migration 00230.\n' > "$REPOE/docs/latin1.txt"
# Paths git quotes by default: non-ASCII (must be scanned verbatim under core.quotePath=false)
# and a TAB (still quoted, so it must be named as unscanned, never silently dropped).
printf 'see migration 00231\n' > "$REPOE/docs/caf$(printf '\303\251').md"
printf 'see migration 00230\n' > "$REPOE/docs/tab$(printf '\t')name.md"
git -C "$REPOE" add -A
git -C "$REPOE" commit -q -m "branch: a PNG and a Latin-1 note"
PNG_BEFORE="$(od -An -tx1 "$REPOE/docs/img/shot.png")"

E_OUT="$( ( cd "$REPOE" && LC_ALL=en_US.UTF-8 sh "$HELPER" ) 2>&1 )"
E_RC=$?
ME="$REPOE/$MIGDIR"
assert_eq "E: renumber with a PNG in the diff exits 0" "0" "$E_RC"
if [ -f "$ME/00232_forge_x.sql" ]; then pass "E: 00232_forge_x.sql created"; else fail "E: 00232_forge_x.sql created"; fi
assert_eq       "E: PNG byte-identical"                     "$PNG_BEFORE" "$(od -An -tx1 "$REPOE/docs/img/shot.png")"
assert_contains "E: binary skip notice names the PNG"       "skipped binary file: docs/img/shot.png" "$E_OUT"
assert_contains "E: non-UTF-8 text file still scanned"      "docs/latin1.txt:1"                      "$E_OUT"
assert_contains "E: non-ASCII path scanned verbatim"        "$(printf 'docs/caf\303\251.md:1')"     "$E_OUT"
assert_contains "E: git-quoted path named as NOT scanned"   "NOT scanned (git-quoted path"           "$E_OUT"

# =============================================================================
echo "=== F. a reference-scan failure happens BEFORE any rename (issue 1581) ==="
# =============================================================================
# The scan is the last fallible read before the tree is written, so it runs first: a scan
# error must leave nothing staged, keeping the helper retryable. A PATH-local awk wrapper
# fails only on the poisoned file and delegates every other awk call.
CF="$ROOT/caseF"; build_base "$CF"; RF="$CF/repo"
add_forge_pair "$RF"
wf "$RF/docs/poison.txt" <<'TXT'
migration 00230
TXT
git -C "$RF" add -A
git -C "$RF" commit -q -m "branch: a file the wrapped awk refuses to scan"
FAKEBINF="$CF/bin"
REAL_AWKF="$(command -v awk)"
mkdir -p "$FAKEBINF"
wf "$FAKEBINF/awk" <<EOF
#!/bin/sh
for _a in "\$@"; do
  case "\$_a" in *poison.txt) echo "awk: injected scan failure" >&2; exit 71 ;; esac
done
exec "$REAL_AWKF" "\$@"
EOF
chmod +x "$FAKEBINF/awk"
run_refusal "F scan failure" "$RF" "reference scan failed" "$FAKEBINF:$PATH"

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
