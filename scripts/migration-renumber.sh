#!/bin/sh
# Landing-time migration RENUMBER helper (issue #1400 QW2).
#
# usage: scripts/migration-renumber.sh
#        scripts/migration-renumber.sh --rewrite-comments <mapfile> <sqlfile>
#
# 🔴 WHAT THIS IS FOR. A maintainer rebased their branch onto main; their new goose
# migration(s) are already COMMITTED, but a draft number now COLLIDES with a migration
# that landed on main in the meantime (the exact thing check-migration-numbering.sh
# reddens: goose panics `duplicate version N detected` at boot and in every *LiveDB
# test). This helper renames the branch's new migration(s) to CONSECUTIVE numbers above
# the live main head, rewrites the renamed migrations' OWN cross-reference comments, and
# REPORTS every other reference (Go, docs, sibling migrations) for a human to fix by
# hand. It is the mechanical form of CLAUDE.md's "numbers are assigned at merge time,
# renumbered above the live head".
#
# 🔴 IT IS A TREE-WRITER, NOT A GATE. It `git mv`s files and edits comment lines, so it
# is fail-CLOSED before any mutation (see the preconditions/preflight below) and, once it
# starts renaming, a failure is a hard non-zero exit. On success the tree is left DIRTY
# (renames staged, comment edits unstaged) for the maintainer to review and commit -- the
# helper never commits. A rerun then refuses on the clean-tree precondition, which is the
# intended fail-closed behaviour: a half-applied run is never silently resumed.
#
# 🔴 BY FILE PATH, NEVER BY NUMBER. The branch-new set is `git diff --diff-filter=A`
# (paths HEAD adds over origin/main), because the branch file's number may EQUAL a
# just-landed main migration's number -- that IS the collision to fix -- so a number-based
# diff would exclude exactly the file that needs renumbering.
#
# 🔴 COMMENT LINES ONLY, SIMULTANEOUS. The old->new remap is applied only to `--` comment
# lines (never SQL bodies, where a 5-digit literal could be data) and as ONE simultaneous
# token pass, because old and new ranges can overlap (draft 00231/00232 -> new 00232/00233
# aliases 00232) and a per-number sed in sequence would double-substitute.
#
# EXIT CODES:
#     2 = usage / bad subcommand / wrong subcommand argument count
#     1 = a precondition or preflight REFUSAL (nothing changed), OR a mid-transform
#         failure, OR a self-check failure, OR a --rewrite-comments file/rewrite failure
#     0 = success (mechanics done: rename + renamed-migration comment rewrite)
set -eu

MIGRATIONS_DIR="api/internal/store/migrations"
CANARY_DIR="scripts/migration-numbering-canary"

# A literal TAB, built from an escape so no raw control byte lives in this source.
TAB="$(printf '\t')"

die() {
  echo "migration-renumber: $*" >&2
  exit 1
}

usage() {
  echo "usage: scripts/migration-renumber.sh" >&2
  echo "       scripts/migration-renumber.sh --rewrite-comments <mapfile> <sqlfile>" >&2
}

# rewrite_comments <mapfile> <sqlfile>
# Rewrite <sqlfile> IN PLACE, applying the map (whitespace-separated `OLD NEW` per line)
# to its COMMENT LINES ONLY. Simultaneous, token-exact: matches a MAXIMAL run of digits
# and only remaps a run of EXACTLY 5 digits that is a map key -- so 100232 / 002321 never
# match, and a mapped value inserted into the output is never re-scanned (true
# simultaneous semantics). This is the SAME code path the main transform calls per renamed
# file, and the internal --rewrite-comments subcommand so the M3 harness can drive it on a
# fixture. (No literal single-quote and no `close` used as a name in the awk, deliberately.)
rewrite_comments() {
  _map="$1"
  _f="$2"
  _dir="$(dirname "$_f")"
  _tmp="$(mktemp "$_dir/.renum.XXXXXX")" || return 1
  if awk 'NR==FNR { m[$1]=$2; next }
    {
      probe=$0
      sub(/^[[:space:]]+/, "", probe)
      if (substr(probe,1,2) != "--") { print; next }
      out=""; rest=$0
      while (match(rest, /[0-9]+/)) {
        tok=substr(rest, RSTART, RLENGTH)
        out=out substr(rest, 1, RSTART-1)
        if (length(tok)==5 && (tok in m)) out=out m[tok]; else out=out tok
        rest=substr(rest, RSTART+RLENGTH)
      }
      print out rest
    }' "$_map" "$_f" > "$_tmp"; then
    mv "$_tmp" "$_f"
  else
    rm -f "$_tmp"
    return 1
  fi
}

# ---- subcommand dispatch -------------------------------------------------------------
# The --rewrite-comments subcommand is a pure file operation: it deliberately runs BEFORE
# any git/cd/precondition work so it can be unit-tested in isolation on an arbitrary
# fixture, from any cwd, without a repo or a clean tree.
case "${1:-}" in
  --rewrite-comments)
    if [ "$#" -ne 3 ]; then
      usage
      exit 2
    fi
    _sub_map="$2"
    _sub_f="$3"
    [ -f "$_sub_map" ] || die "--rewrite-comments: map file not found: $_sub_map"
    [ -f "$_sub_f" ] || die "--rewrite-comments: sql file not found: $_sub_f"
    if rewrite_comments "$_sub_map" "$_sub_f"; then
      exit 0
    else
      die "--rewrite-comments: rewrite failed for $_sub_f"
    fi
    ;;
  "")
    : # main transform, below
    ;;
  *)
    echo "migration-renumber: unknown argument: $1" >&2
    usage
    exit 2
    ;;
esac

# ---- main transform ------------------------------------------------------------------

# Run from the repo root whatever the caller's cwd, like check-migration-numbering.sh.
ROOT="$(git rev-parse --show-toplevel)" || die "not inside a git work tree (git rev-parse --show-toplevel failed)"
cd "$ROOT" || die "cannot cd to repo root: $ROOT"

# Scratch lives OUTSIDE the repo so it is invisible to git status and never staged; the
# trap removes it on every exit path (including a precondition refusal).
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/migrenum.XXXXXX")" || die "cannot create a temp work dir"
trap 'rm -rf "$WORKDIR"' EXIT

# ---- preconditions: fail closed, change nothing, check in order ----------------------

# 1. Clean tree: the helper git mv's files, so the branch's migrations must be committed.
if [ -n "$(git status --porcelain)" ]; then
  die "working tree is not clean. Commit (or stash) the branch's migrations first -- this
  helper git mv's them and rewrites comments, so it refuses to touch a dirty tree."
fi

# 2. Resolvable base.
git fetch origin main || die "git fetch origin main failed; cannot resolve the landing base"
git rev-parse --verify origin/main >/dev/null 2>&1 || die "origin/main is not resolvable after fetch"

# 3. Rebased onto current main.
if ! git merge-base --is-ancestor origin/main HEAD; then
  die "HEAD is not rebased onto current origin/main. Rebase the branch first, then rerun."
fi

# ---- live main head (max arithmetic value of the leading digit-run of the BASENAMES) --
# Read from origin/main's tree, NOT the working tree: the working tree's highest already
# counts the branch's own draft, so it is the wrong base to number above.
git ls-tree -r --name-only origin/main -- "$MIGRATIONS_DIR" > "$WORKDIR/main_migs" ||
  die "git ls-tree origin/main failed for $MIGRATIONS_DIR"
HEAD_NUM="$(awk '
  {
    base = $0
    sub(/.*\//, "", base)
    if (match(base, /^[0-9]+/)) {
      v = substr(base, RSTART, RLENGTH) + 0
      if (v > max) max = v
    }
  }
  END { print max + 0 }' "$WORKDIR/main_migs")"

# ---- branch-new set BY FILE PATH -----------------------------------------------------
git diff --no-renames --name-only --diff-filter=A origin/main HEAD -- "$MIGRATIONS_DIR" \
  > "$WORKDIR/branch_new" || die "git diff (branch-new set) failed"

# ---- preflight validation: fail BEFORE any mutation ----------------------------------

# Branch-new set empty -> refuse (nothing to renumber; likely the wrong branch/dir).
if [ ! -s "$WORKDIR/branch_new" ]; then
  die "no migrations added by HEAD over origin/main under $MIGRATIONS_DIR -- nothing to
  renumber. (Are you on the right branch, and is it rebased onto current main?)"
fi

# Validate each basename is exactly NNNNN_slug.sql, and build a `oldnum<TAB>path` list.
# Explicit 5-char digit classes (not a {5} brace interval) keep this portable across awks
# and clear of any ugrep interval pitfalls.
: > "$WORKDIR/sorted"
while IFS= read -r p; do
  [ -n "$p" ] || continue
  bn="${p##*/}"
  if ! printf '%s\n' "$bn" | awk '/^[0-9][0-9][0-9][0-9][0-9]_.+\.sql$/ { ok=1 } END { exit ok ? 0 : 1 }'; then
    die "malformed migration name (need exactly NNNNN_slug.sql): $bn"
  fi
  oldnum="$(printf '%s\n' "$bn" | awk '{ if (match($0, /^[0-9]+/)) print substr($0, RSTART, RLENGTH) }')"
  printf '%s%s%s\n' "$oldnum" "$TAB" "$p" >> "$WORKDIR/sorted"
done < "$WORKDIR/branch_new"

# Sort by draft number ascending (5-digit zero-padded prefixes sort lexically == numerically).
sort "$WORKDIR/sorted" -o "$WORKDIR/sorted"

# Duplicate old number -> refuse (the old->new map would be ambiguous).
dup="$(awk -F"$TAB" '{ c[$1]++ } END { for (k in c) if (c[k] > 1) print k }' "$WORKDIR/sorted")"
if [ -n "$dup" ]; then
  die "two branch-new migrations share the same 5-digit prefix ($dup) -- the old->new map
  would be ambiguous. Give them distinct draft numbers, then rerun."
fi

N="$(awk 'END { print NR }' "$WORKDIR/sorted")"

# ---- assign consecutive numbers above the head, build the rename plan -----------------
# Preserving ascending draft order keeps a base migration and its validate_* companion
# adjacent and ordered. Plan columns (TAB-separated): oldnum newnum oldpath newpath tmppath.
awk -F"$TAB" -v head="$HEAD_NUM" '
  {
    oldnum = $1
    oldpath = $2
    base = oldpath
    sub(/.*\//, "", base)
    slug = base
    sub(/^[0-9]+_/, "", slug)
    newnum = sprintf("%05d", head + NR)
    dir = oldpath
    if (dir ~ /\//) sub(/\/[^\/]*$/, "", dir); else dir = "."
    newpath = dir "/" newnum "_" slug
    tmppath = dir "/.renumber-tmp-" NR "_" slug
    print oldnum "\t" newnum "\t" oldpath "\t" newpath "\t" tmppath
  }' "$WORKDIR/sorted" > "$WORKDIR/plan"

# Derived views of the plan (each two-column form keeps every read var used).
awk -F"$TAB" '{ print $1 " " $2 }'         "$WORKDIR/plan" > "$WORKDIR/map"      # OLD NEW
awk -F"$TAB" '{ print $3 "\t" $5 }'        "$WORKDIR/plan" > "$WORKDIR/phase1"   # oldpath -> tmppath
awk -F"$TAB" '{ print $5 "\t" $4 }'        "$WORKDIR/plan" > "$WORKDIR/phase2"   # tmppath -> newpath
awk -F"$TAB" '{ print $4 }'                "$WORKDIR/plan" > "$WORKDIR/newpaths"

# ---- rename: TWO-PHASE so overlapping ranges never collide ---------------------------
# Draft 00231/00232 -> new 00232/00233 means a new path can equal another file's OLD path,
# so a direct git mv would collide with a still-present file. Phase 1 moves every old path
# to a unique temp name; phase 2 moves each temp to its final new path.
while IFS="$TAB" read -r src dst; do
  git mv -- "$src" "$dst"
done < "$WORKDIR/phase1"
while IFS="$TAB" read -r src dst; do
  git mv -- "$src" "$dst"
done < "$WORKDIR/phase2"

# ---- comment rewrite: one SIMULTANEOUS old->new pass over each renamed file ----------
while IFS= read -r np; do
  [ -n "$np" ] || continue
  rewrite_comments "$WORKDIR/map" "$np" || die "comment rewrite failed for $np"
done < "$WORKDIR/newpaths"

# ---- postflight report: scan OTHER branch-changed files, never auto-edit, never abort --
# Other = (all files HEAD changed over origin/main) MINUS the renamed migrations (their OLD
# paths, i.e. the branch-new set). Token-exact 5-digit scan in awk (this host's ugrep
# mishandles negated bracket classes in -E mode, so awk digit-run scanning is used).
git diff --no-renames --name-only origin/main HEAD > "$WORKDIR/all_changed" ||
  die "git diff (all changed files) failed"
awk 'NR==FNR { skip[$0]=1; next } !($0 in skip)' "$WORKDIR/branch_new" "$WORKDIR/all_changed" \
  > "$WORKDIR/other"

report=""
while IFS= read -r f; do
  [ -n "$f" ] || continue
  [ -f "$f" ] || continue
  hits="$(awk 'NR==FNR { m[$1]=$2; next }
    {
      rest=$0
      while (match(rest, /[0-9]+/)) {
        tok=substr(rest, RSTART, RLENGTH)
        if (length(tok)==5 && (tok in m))
          print FILENAME ":" FNR "\t" tok " -> " m[tok] "\t" $0
        rest=substr(rest, RSTART+RLENGTH)
      }
    }' "$WORKDIR/map" "$f")"
  if [ -n "$hits" ]; then
    report="$report$hits
"
  fi
done < "$WORKDIR/other"

if [ -n "$report" ]; then
  echo ""
  echo "migration-renumber: POSTFLIGHT -- other files reference an OLD migration number."
  echo "  These were NOT auto-edited: a token-equal number elsewhere may reference a"
  echo "  DIFFERENT migration or be data. Review and fix BY HAND. Format:"
  echo "    file:line  OLD -> NEW  | line content"
  printf '%s' "$report" | while IFS="$TAB" read -r loc sugg content; do
    [ -n "$loc" ] || continue
    echo "  $loc  $sugg  | $content"
  done
fi

# ---- self-check: fail loudly if the mechanics did not hold ---------------------------

# INLINE (mandatory): the resulting new FILENAMES' numbers are contiguous head+1..head+N,
# all strictly > the live main head, and all distinct. Verified from the files ON DISK.
: > "$WORKDIR/newnums"
while IFS= read -r np; do
  [ -n "$np" ] || continue
  [ -f "$np" ] || die "self-check FAILED: expected renamed file is missing: $np"
  printf '%s\n' "${np##*/}" >> "$WORKDIR/newnums"
done < "$WORKDIR/newpaths"

selfok="$(awk -v head="$HEAD_NUM" -v n="$N" '
  { if (match($0, /^[0-9]+/)) v[++c] = substr($0, RSTART, RLENGTH) + 0 }
  END {
    if (c != n) { print "0"; exit }
    for (i = 1; i <= c; i++)
      for (j = i + 1; j <= c; j++)
        if (v[j] < v[i]) { t = v[i]; v[i] = v[j]; v[j] = t }
    for (i = 1; i <= c; i++)
      if (v[i] != head + i) { print "0"; exit }
    print "1"
  }' "$WORKDIR/newnums")"
if [ "$selfok" != "1" ]; then
  die "self-check FAILED: new numbers are not contiguous head+1..head+$N above $HEAD_NUM"
fi

# Numbering check: reuse the committed uniqueness gate if it is present.
if [ -x scripts/check-migration-numbering.sh ]; then
  if ! ./scripts/check-migration-numbering.sh "$CANARY_DIR" "$MIGRATIONS_DIR"; then
    die "self-check FAILED: check-migration-numbering.sh reported a problem after renumber"
  fi
else
  echo "migration-renumber: NOTE -- scripts/check-migration-numbering.sh not found or not"
  echo "  executable; skipping the numbering self-check (this is not a failure)."
fi

# NOTE: the renamed migrations are deliberately NOT grepped for residual OLD numbers as a
# pass/fail signal -- under old/new overlap a residual token is LEGITIMATE (draft
# 00231/00232 -> new 00232/00233 keeps a 00232 token for the file that was 00231).

# ---- final summary -------------------------------------------------------------------
echo ""
echo "migration-renumber: DONE -- renamed $N migration(s) above live main head $HEAD_NUM:"
awk -F"$TAB" '{ print "  " $3 " -> " $4 }' "$WORKDIR/plan"
echo ""
if [ -n "$report" ]; then
  echo "migration-renumber: postflight found OTHER references (listed above). They were NOT"
  echo "  auto-edited and still need manual review. SUCCESS here means the MECHANICS are done"
  echo "  (rename + renamed-migration comment rewrite), NOT that those references were reviewed."
else
  echo "migration-renumber: postflight found no other references to the old numbers."
  echo "  SUCCESS = mechanics done (rename + renamed-migration comment rewrite)."
fi
exit 0
