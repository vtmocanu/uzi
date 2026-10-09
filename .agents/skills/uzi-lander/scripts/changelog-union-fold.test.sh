#!/usr/bin/env bash
# Hermetic tests of changelog-union.sh's RELEASE FOLD: the base cut a release that folded its
# [Unreleased] body into a `## [x.y.z]` section while the branch added bullets under
# [Unreleased]. Each case builds a throwaway repo whose index is given the three conflict
# stages directly (no real rebase: a real one of these shapes can merge cleanly, which
# land-prep.sh's misplacement guard covers, tested in land-prep-changelog-placement.test.sh), plus a
# worktree file with diff3-style markers.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/changelog-union.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com

# mkconflict NAME: $WORK/NAME.{base,main,branch}.md hold the ancestor, ours (the base being
# rebased onto) and theirs (the branch commit). Builds a repo whose index holds stages 1, 2 and
# 3 of CHANGELOG.md, and a worktree file with one whole-file diff3 conflict block. (A
# real rebase of these shapes can merge CLEANLY, filing the bullets under the released
# section, so the conflict is built directly.)
mkconflict() {
  local d="$WORK/$1" s1 s2 s3
  git init -q -b main "$d"
  s1=$(git -C "$d" hash-object -w "$WORK/$1.base.md")
  s2=$(git -C "$d" hash-object -w "$WORK/$1.main.md")
  s3=$(git -C "$d" hash-object -w "$WORK/$1.branch.md")
  printf '100644 %s 1\tCHANGELOG.md\n100644 %s 2\tCHANGELOG.md\n100644 %s 3\tCHANGELOG.md\n' "$s1" "$s2" "$s3" |
    git -C "$d" update-index --index-info
  # a whole-file conflict block in diff3 style (marker content is not what the fold reads)
  { echo '<<<<<<< HEAD'; cat "$WORK/$1.main.md"; echo '||||||| parent'; cat "$WORK/$1.base.md"; echo '======='
    cat "$WORK/$1.branch.md"; echo '>>>>>>> branch'; } > "$d/CHANGELOG.md"
}
# run NAME: runs the script in the conflicted repo; sets RC and OUT.
run() {
  RC=0; OUT=$(cd "$WORK/$1" && bash "$SCRIPT" CHANGELOG.md 2>&1) || RC=$?
}
expect_ok() {
  run "$1"; [ "$RC" -eq 0 ] || fail "$1: expected exit 0, got $RC: $OUT"
  diff -u "$WORK/$1.want.md" "$WORK/$1/CHANGELOG.md" || fail "$1: wrong result"
}
expect_refuse() {  # NAME
  local before
  before=$(cat "$WORK/$1/CHANGELOG.md")
  run "$1"; [ "$RC" -eq 1 ] || fail "$1: expected exit 1, got $RC: $OUT"
  [ "$(cat "$WORK/$1/CHANGELOG.md")" = "$before" ] || fail "$1: a refusal must leave the file untouched"
}

HEAD_='# Changelog

'
REL_='## [0.2.0] - 2026-02-01

### Added

- **old added**
  old added desc

### Fixed

- **old fixed**
  old fixed desc

## [0.1.0] - 2026-01-01

### Added

- **first**
  first desc
'
# The ancestor: [Unreleased] holds two bullets (continuation lines) under two headings.
BASE_="${HEAD_}## [Unreleased]

### Added

- **old added**
  old added desc

### Fixed

- **old fixed**
  old fixed desc

## [0.1.0] - 2026-01-01

### Added

- **first**
  first desc
"

# 1. Happy path: branch bullets (with continuation lines) under two headings land under
#    those headings of the empty base [Unreleased]; headings are created in order; the
#    released sections stay byte-identical.
printf '%s' "$BASE_" > "$WORK/happy.base.md"
printf '%s## [Unreleased]\n\n%s' "$HEAD_" "$REL_" > "$WORK/happy.main.md"
printf '%s## [Unreleased]\n\n### Added\n\n- **old added**\n  old added desc\n\n- **new added**\n  new added line 1\n  new added line 2\n\n### Fixed\n\n- **old fixed**\n  old fixed desc\n\n- **new fixed**\n  new fixed desc\n\n## [0.1.0] - 2026-01-01\n\n### Added\n\n- **first**\n  first desc\n' "$HEAD_" > "$WORK/happy.branch.md"
printf '%s## [Unreleased]\n\n### Added\n\n- **new added**\n  new added line 1\n  new added line 2\n\n### Fixed\n\n- **new fixed**\n  new fixed desc\n\n%s' "$HEAD_" "$REL_" > "$WORK/happy.want.md"
mkconflict happy
expect_ok happy
grep -Fq 'release fold' <<<"$OUT" || fail "happy: the output should name the release fold: $OUT"

# 2. The base [Unreleased] already holds NEWER bullets under existing headings: kept, the
#    branch bullet is added alongside; a heading the base lacks (Changed) is created in
#    conventional order, between Added and Fixed.
printf '%s## [Unreleased]\n\n### Added\n\n- **newer on main**\n  newer desc\n\n### Fixed\n\n- **newer fix**\n  fix desc\n\n%s' "$HEAD_" "$REL_" > "$WORK/newer.main.md"
cp "$WORK/happy.base.md" "$WORK/newer.base.md"
printf '%s## [Unreleased]\n\n### Added\n\n- **old added**\n  old added desc\n\n- **new added**\n  new added desc\n\n### Changed\n\n- **new changed**\n  new changed desc\n\n### Fixed\n\n- **old fixed**\n  old fixed desc\n\n## [0.1.0] - 2026-01-01\n\n### Added\n\n- **first**\n  first desc\n' "$HEAD_" > "$WORK/newer.branch.md"
printf '%s## [Unreleased]\n\n### Added\n\n- **newer on main**\n  newer desc\n\n- **new added**\n  new added desc\n\n### Changed\n\n- **new changed**\n  new changed desc\n\n### Fixed\n\n- **newer fix**\n  fix desc\n\n%s' "$HEAD_" "$REL_" > "$WORK/newer.want.md"
mkconflict newer
expect_ok newer

# 3. A heading the base [Unreleased] lacks and that sorts last (Fixed after Added): appended
#    at the end of [Unreleased], before the first released section.
printf '%s## [Unreleased]\n\n### Added\n\n- **newer on main**\n  newer desc\n\n%s' "$HEAD_" "$REL_" > "$WORK/tail.main.md"
cp "$WORK/happy.base.md" "$WORK/tail.base.md"
printf '%s## [Unreleased]\n\n### Added\n\n- **old added**\n  old added desc\n\n### Fixed\n\n- **old fixed**\n  old fixed desc\n\n- **tail fix**\n  tail desc\n\n## [0.1.0] - 2026-01-01\n\n### Added\n\n- **first**\n  first desc\n' "$HEAD_" > "$WORK/tail.branch.md"
printf '%s## [Unreleased]\n\n### Added\n\n- **newer on main**\n  newer desc\n\n### Fixed\n\n- **tail fix**\n  tail desc\n\n%s' "$HEAD_" "$REL_" > "$WORK/tail.want.md"
mkconflict tail
expect_ok tail

# 4. Refusal: the branch REWORDED an ancestor [Unreleased] bullet.
cp "$WORK/happy.base.md" "$WORK/edit.base.md"; cp "$WORK/happy.main.md" "$WORK/edit.main.md"
printf '%s## [Unreleased]\n\n### Added\n\n- **old added**\n  old added REWORDED\n\n- **new added**\n  new\n\n### Fixed\n\n- **old fixed**\n  old fixed desc\n\n## [0.1.0] - 2026-01-01\n\n### Added\n\n- **first**\n  first desc\n' "$HEAD_" > "$WORK/edit.branch.md"
mkconflict edit; expect_refuse edit
grep -Fq 'moved or deleted' <<<"$OUT" || fail "edit: wrong refusal reason: $OUT"

# 5. Refusal: the branch DELETED an ancestor [Unreleased] bullet.
cp "$WORK/happy.base.md" "$WORK/del.base.md"; cp "$WORK/happy.main.md" "$WORK/del.main.md"
printf '%s## [Unreleased]\n\n### Added\n\n- **new added**\n  new\n\n### Fixed\n\n- **old fixed**\n  old fixed desc\n\n## [0.1.0] - 2026-01-01\n\n### Added\n\n- **first**\n  first desc\n' "$HEAD_" > "$WORK/del.branch.md"
mkconflict del; expect_refuse del
grep -Fq 'moved or deleted' <<<"$OUT" || fail "del: wrong refusal reason: $OUT"

# 6. Refusal: no fold to prove (the base released something else; the ancestor bullets are
#    in no released section of the base).
cp "$WORK/happy.base.md" "$WORK/nofold.base.md"
printf '%s## [Unreleased]\n\n### Added\n\n- **unrelated**\n  unrelated desc\n\n## [0.1.0] - 2026-01-01\n\n### Added\n\n- **first**\n  first desc\n' "$HEAD_" > "$WORK/nofold.main.md"
cp "$WORK/happy.branch.md" "$WORK/nofold.branch.md"
mkconflict nofold; expect_refuse nofold
grep -Fq 'fold unproven' <<<"$OUT" || fail "nofold: wrong refusal reason: $OUT"

# 7. Refusal: the base still holds an ancestor bullet under [Unreleased] AND released it
#    (not a clean fold; ambiguous).
cp "$WORK/happy.base.md" "$WORK/still.base.md"
printf '%s## [Unreleased]\n\n### Added\n\n- **old added**\n  old added desc\n\n%s' "$HEAD_" "$REL_" > "$WORK/still.main.md"
cp "$WORK/happy.branch.md" "$WORK/still.branch.md"
mkconflict still; expect_refuse still
grep -Fq 'no fold' <<<"$OUT" || fail "still: wrong refusal reason: $OUT"

# 8. Refusal: a branch-added bullet already exists in the base (released there).
cp "$WORK/happy.base.md" "$WORK/dup.base.md"
printf '%s## [Unreleased]\n\n%s' "$HEAD_" "$REL_" | awk '{print} /old fixed desc/ && !d {print ""; print "- **new added**"; print "  new added line 1"; print "  new added line 2"; d=1}' > "$WORK/dup.main.md"
cp "$WORK/happy.branch.md" "$WORK/dup.branch.md"
mkconflict dup; expect_refuse dup
grep -Fq 'already exists' <<<"$OUT" || fail "dup: wrong refusal reason: $OUT"

# 9. Refusal: the branch also changed a released section.
cp "$WORK/happy.base.md" "$WORK/rel.base.md"; cp "$WORK/happy.main.md" "$WORK/rel.main.md"
sed 's/first desc/first desc CHANGED/' "$WORK/happy.branch.md" > "$WORK/rel.branch.md"
mkconflict rel; expect_refuse rel
grep -Fq 'outside [Unreleased]' <<<"$OUT" || fail "rel: wrong refusal reason: $OUT"

# 10. Refusal: a non-bullet line under the branch's [Unreleased] (cannot place it).
cp "$WORK/happy.base.md" "$WORK/prose.base.md"; cp "$WORK/happy.main.md" "$WORK/prose.main.md"
sed 's/^- \*\*new added\*\*/Some prose\n\n- **new added**/' "$WORK/happy.branch.md" > "$WORK/prose.branch.md"
mkconflict prose; expect_refuse prose
grep -Fq 'non-bullet line' <<<"$OUT" || fail "prose: wrong refusal reason: $OUT"

# 11. Refusal (historical match): main merely DELETED the Unreleased entry; an identical bullet
#     already sat in an OLD release and no new release section exists. Not a fold.
printf '%s## [Unreleased]\n\n### Added\n\n- **old added**\n  old added desc\n\n## [0.1.0] - 2026-01-01\n\n### Added\n\n- **old added**\n  old added desc\n' "$HEAD_" > "$WORK/hist.base.md"
printf '%s## [Unreleased]\n\n## [0.1.0] - 2026-01-01\n\n### Added\n\n- **old added**\n  old added desc\n' "$HEAD_" > "$WORK/hist.main.md"
printf '%s## [Unreleased]\n\n### Added\n\n- **old added**\n  old added desc\n\n- **new added**\n  new\n\n## [0.1.0] - 2026-01-01\n\n### Added\n\n- **old added**\n  old added desc\n' "$HEAD_" > "$WORK/hist.branch.md"
mkconflict hist; expect_refuse hist
grep -Fq 'fold unproven' <<<"$OUT" || fail "hist: wrong refusal reason: $OUT"

# 12. Refusal (subsection identity): the branch MOVED an ancestor bullet from Added to Changed.
cp "$WORK/happy.base.md" "$WORK/moved.base.md"; cp "$WORK/happy.main.md" "$WORK/moved.main.md"
printf '%s## [Unreleased]\n\n### Added\n\n- **new added**\n  new\n\n### Changed\n\n- **old added**\n  old added desc\n\n### Fixed\n\n- **old fixed**\n  old fixed desc\n\n## [0.1.0] - 2026-01-01\n\n### Added\n\n- **first**\n  first desc\n' "$HEAD_" > "$WORK/moved.branch.md"
mkconflict moved; expect_refuse moved
grep -Fq 'moved or deleted' <<<"$OUT" || fail "moved: wrong refusal reason: $OUT"

# 13. Refusal (multiplicity): the ancestor held the same bullet twice, the branch dropped one.
printf '%s## [Unreleased]\n\n### Added\n\n- **twin**\n  twin desc\n\n- **twin**\n  twin desc\n' "$HEAD_" > "$WORK/twin.base.md"
printf '%s## [Unreleased]\n\n## [0.2.0] - 2026-02-01\n\n### Added\n\n- **twin**\n  twin desc\n\n- **twin**\n  twin desc\n' "$HEAD_" > "$WORK/twin.main.md"
printf '%s## [Unreleased]\n\n### Added\n\n- **twin**\n  twin desc\n\n- **new added**\n  new\n' "$HEAD_" > "$WORK/twin.branch.md"
mkconflict twin; expect_refuse twin
grep -Fq 'moved or deleted' <<<"$OUT" || fail "twin: wrong refusal reason: $OUT"

# 14. Positive control for multiplicity: both copies kept and folded -> the addition lands.
printf '%s## [Unreleased]\n\n### Added\n\n- **twin**\n  twin desc\n\n- **twin**\n  twin desc\n\n- **new added**\n  new\n' "$HEAD_" > "$WORK/twin2.branch.md"
cp "$WORK/twin.base.md" "$WORK/twin2.base.md"; cp "$WORK/twin.main.md" "$WORK/twin2.main.md"
printf '%s## [Unreleased]\n\n### Added\n\n- **new added**\n  new\n\n## [0.2.0] - 2026-02-01\n\n### Added\n\n- **twin**\n  twin desc\n\n- **twin**\n  twin desc\n' "$HEAD_" > "$WORK/twin2.want.md"
mkconflict twin2; expect_ok twin2

# 14b. Refusal (release identity is the VERSION): main only re-dated an existing release heading
#      and deleted the Unreleased entry; no new release version exists.
sed 's/## \[0.1.0\] - 2026-01-01/## [0.1.0] - 2026-01-02/' "$WORK/hist.main.md" > "$WORK/redate.main.md"
cp "$WORK/hist.base.md" "$WORK/redate.base.md"
cp "$WORK/hist.branch.md" "$WORK/redate.branch.md"
printf '%s## [Unreleased]\n\n## [0.1.0] - 2026-01-02\n\n### Added\n\n- **old added**\n  old added desc\n' "$HEAD_" > "$WORK/redate.main.md"
mkconflict redate; expect_refuse redate

# 14c. RC-first: an rc.N cut folded [Unreleased] into the stable-keyed section an earlier
#      candidate created; the section gained the ancestor bullet, so the fold is proven.
RC_REL='## [0.2.0] - 2026-02-01

### Added

- **old added**
  old added desc
'
printf '%s## [Unreleased]\n\n### Fixed\n\n- **rc2 fix**\n  rc2 desc\n\n%s' "$HEAD_" "$RC_REL" > "$WORK/rcfold.base.md"
printf '%s## [Unreleased]\n\n%s\n### Fixed\n\n- **rc2 fix**\n  rc2 desc\n' "$HEAD_" "$RC_REL" > "$WORK/rcfold.main.md"
printf '%s## [Unreleased]\n\n### Fixed\n\n- **rc2 fix**\n  rc2 desc\n\n- **new fix**\n  new desc\n\n%s' "$HEAD_" "$RC_REL" > "$WORK/rcfold.branch.md"
printf '%s## [Unreleased]\n\n### Fixed\n\n- **new fix**\n  new desc\n\n%s\n### Fixed\n\n- **rc2 fix**\n  rc2 desc\n' "$HEAD_" "$RC_REL" > "$WORK/rcfold.want.md"
mkconflict rcfold; expect_ok rcfold

# 14d. Refusal (gain, not presence): the existing section already held the bullet in the
#      ancestor and main only deleted the Unreleased entry; the section gained nothing.
printf '%s## [Unreleased]\n\n### Added\n\n- **old added**\n  old added desc\n\n%s' "$HEAD_" "$RC_REL" > "$WORK/nogain.base.md"
printf '%s## [Unreleased]\n\n%s' "$HEAD_" "$RC_REL" > "$WORK/nogain.main.md"
printf '%s## [Unreleased]\n\n### Added\n\n- **old added**\n  old added desc\n\n- **new added**\n  new\n\n%s' "$HEAD_" "$RC_REL" > "$WORK/nogain.branch.md"
mkconflict nogain; expect_refuse nogain
grep -Fq 'fold unproven' <<<"$OUT" || fail "nogain: wrong refusal reason: $OUT"

# 15. Refusal: markers in a plain file with no index stages stay the old refusal.
printf '## [Unreleased]\n\n### Fixed\n\n<<<<<<< HEAD\n- **a**\n||||||| b\n- **old**\n=======\n- **b**\n>>>>>>> x\n' > "$WORK/plain.md"
cp "$WORK/plain.md" "$WORK/plain.before"
RC=0; OUT=$(bash "$SCRIPT" "$WORK/plain.md" 2>&1) || RC=$?
[ "$RC" -eq 1 ] || fail "plain: expected exit 1, got $RC: $OUT"
cmp -s "$WORK/plain.md" "$WORK/plain.before" || fail "plain: a refusal must leave the file untouched"

echo "PASS changelog-union-fold: release fold happy path, newer base bullets, created headings in order, appended heading, an RC-first fold into an existing release section, and the edited, deleted, unproven, no-gain, unfolded, duplicate, released-change, prose and no-stage refusals"
