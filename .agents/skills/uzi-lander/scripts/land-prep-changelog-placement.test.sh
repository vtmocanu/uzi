#!/usr/bin/env bash
# Hermetic regression with REAL `git rebase`s: when the base cuts a release that folds
# [Unreleased] into a new version section, a conflict-free rebase files the branch's new
# bullets under the RELEASED section (changelog-union.sh never runs). land-prep must stop with
# exit 11 and RESULT=changelog_misplaced, naming the bullets and leaving them where they are;
# a clean rebase that keeps them under [Unreleased] must still pass.
set -eu
export NET_RETRY_BASE_SLEEP=0

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/land-prep.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
mkdir -p "$WORK/bin"
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -eu
if [ "${1:-}" = pr ] && [ "${2:-}" = view ]; then printf '%s\n' "$PR_JSON"; exit 0; fi
echo "unexpected gh call: $*" >&2
exit 1
STUB
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
set -eu
if [ "${1:-}" = repo ] && [ "${2:-}" = list ]; then echo '[]'; exit 0; fi
echo "unexpected uzi call: $*" >&2
exit 1
STUB
chmod +x "$WORK/bin/gh" "$WORK/bin/uzi"

ANC='# Changelog

## [Unreleased]

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
BRANCH='# Changelog

## [Unreleased]

### Added

- **old added**
  old added desc

- **new added**
  new added line 1
  new added line 2

### Fixed

- **old fixed**
  old fixed desc

- **new fixed**
  new fixed desc

## [0.1.0] - 2026-01-01

### Added

- **first**
  first desc
'
FOLDED='# Changelog

## [Unreleased]

## [0.2.0] - 2026-02-01

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
# Control: main only edits a released section, far from the branch's bullets.
UNRELATED=${ANC/first desc/first desc, reworded}

# scenario NAME MAINCHANGELOG: bare origin, branch "feature" adding bullets, then main
# changes CHANGELOG.md to MAINCHANGELOG. Leaves $WORK/NAME/{root,wt} ready for land-prep.
scenario() {
  local d="$WORK/$1"
  mkdir -p "$d"
  git init -q --bare "$d/origin.git"
  git init -q -b main "$d/seed"
  git -C "$d/seed" config user.name test
  git -C "$d/seed" config user.email test@example.com
  printf '%s' "$ANC" > "$d/seed/CHANGELOG.md"
  git -C "$d/seed" add CHANGELOG.md
  git -C "$d/seed" commit -qm base
  git -C "$d/seed" remote add origin "$d/origin.git"
  git -C "$d/seed" push -q -u origin main
  git --git-dir="$d/origin.git" symbolic-ref HEAD refs/heads/main
  git -C "$d/seed" switch -qc feature
  printf '%s' "$BRANCH" > "$d/seed/CHANGELOG.md"
  git -C "$d/seed" commit -qam feature
  git -C "$d/seed" push -q -u origin feature
  git -C "$d/seed" switch -q main
  printf '%s' "$2" > "$d/seed/CHANGELOG.md"
  git -C "$d/seed" commit -qam "main moves"
  git -C "$d/seed" push -q origin main
  git clone -q "$d/origin.git" "$d/root"
  git -C "$d/root" config user.name test
  git -C "$d/root" config user.email test@example.com
  PR_JSON=$(jq -cn --arg h "$(git -C "$d/seed" rev-parse feature)" '{state:"OPEN",headRefName:"feature",baseRefName:"main",headRefOid:$h,headRepository:{name:"uzi"},headRepositoryOwner:{login:"test"}}')
  export PR_JSON
}
land() {  # NAME; sets RC, OUT
  RC=0
  OUT=$(PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi 77 --repo-root "$WORK/$1/root" --worktree "$WORK/$1/wt" --no-push --gate none 2>&1) || RC=$?
}

# 1. The fold: the rebase is conflict-free, the new bullets end up in [0.2.0], land-prep stops.
scenario fold "$FOLDED"
land fold
[ "$RC" -eq 11 ] || fail "fold: expected exit 11, got $RC: $OUT"
grep -Fq 'RESULT=changelog_misplaced' <<<"$OUT" || fail "fold: no RESULT line: $OUT"
grep -Fq -- '- **new added**' <<<"$OUT" || fail "fold: the misplaced bullet was not named: $OUT"
grep -Fq -- '- **new fixed**' <<<"$OUT" || fail "fold: the second misplaced bullet was not named: $OUT"
grep -Fq 'REBASE CONFLICT' <<<"$OUT" && fail "fold: the rebase was expected to be conflict-free: $OUT"
# the bullets really are under the released section (nothing was relocated)
awk '/^## \[Unreleased\]/ {u=1; next} /^## / {u=0} u && /new added/ {bad=1} END {exit bad}' "$WORK/fold/wt/CHANGELOG.md" \
  || fail "fold: bullets unexpectedly under [Unreleased]"

# 2. Control: a clean rebase that keeps the bullets under [Unreleased] is not stopped.
scenario keep "$UNRELATED"
land keep
[ "$RC" -eq 0 ] || fail "keep: expected exit 0, got $RC: $OUT"
grep -Fq 'RESULT=prepared' <<<"$OUT" || fail "keep: not prepared: $OUT"

# 3. A branch-added block that is MISSING everywhere (hand fix after exit 11 that dropped one
#    bullet instead of moving it): re-entry stops again, naming it as missing, not as misplaced.
printf '%s' "${FOLDED/## \[Unreleased\]/## [Unreleased]

### Added

- **new added**
  new added line 1
  new added line 2}" > "$WORK/fold/wt/CHANGELOG.md"
git -C "$WORK/fold/wt" config user.name test
git -C "$WORK/fold/wt" config user.email test@example.com
git -C "$WORK/fold/wt" commit -qam 'hand fix that drops a bullet'
RC=0
OUT=$(PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi 77 --repo-root "$WORK/fold/root" --worktree "$WORK/fold/wt" --skip-rebase --no-push --gate none 2>&1) || RC=$?
[ "$RC" -eq 11 ] || fail "missing: expected exit 11, got $RC: $OUT"
grep -Fq 'missing' <<<"$OUT" && grep -Fq -- '- **new fixed**' <<<"$OUT" || fail "missing: the dropped bullet was not named as missing: $OUT"
if grep -F 'misplaced (' <<<"$OUT" | grep -Fq 'new added'; then fail "missing: a correctly placed bullet was flagged: $OUT"; fi

# 4-6. A migration renumber may change the migration number inside the branch's OWN bullet
#      (the renumber helper asks for that hand fix, #2429 / #2407); any other edit still stops.
#      mig_scenario NAME: the branch adds 00002_foo.sql and a bullet naming "migration 00002";
#      after a clean land, the worktree renames it to 00003 and edits the bullet to NEWNUM.
mig_scenario() {  # NAME NEWNUM [EXTRA_REWORD]
  local d="$WORK/$1" newnum="$2" reword="${3:-}"
  local branch_m=${BRANCH/new fixed desc/new fixed desc (migration 00002)}
  mkdir -p "$d"
  git init -q --bare "$d/origin.git"
  git init -q -b main "$d/seed"
  git -C "$d/seed" config user.name test
  git -C "$d/seed" config user.email test@example.com
  printf '%s' "$ANC" > "$d/seed/CHANGELOG.md"
  mkdir -p "$d/seed/api/internal/store/migrations"
  printf -- '-- base\n' > "$d/seed/api/internal/store/migrations/00001_base.sql"
  git -C "$d/seed" add -A
  git -C "$d/seed" commit -qm base
  git -C "$d/seed" remote add origin "$d/origin.git"
  git -C "$d/seed" push -q -u origin main
  git --git-dir="$d/origin.git" symbolic-ref HEAD refs/heads/main
  git -C "$d/seed" switch -qc feature
  printf '%s' "$branch_m" > "$d/seed/CHANGELOG.md"
  printf -- '-- foo\n' > "$d/seed/api/internal/store/migrations/00002_foo.sql"
  git -C "$d/seed" add -A
  git -C "$d/seed" commit -qm feature
  git -C "$d/seed" push -q -u origin feature
  git clone -q "$d/origin.git" "$d/root"
  git -C "$d/root" config user.name test
  git -C "$d/root" config user.email test@example.com
  PR_JSON=$(jq -cn --arg h "$(git -C "$d/seed" rev-parse feature)" '{state:"OPEN",headRefName:"feature",baseRefName:"main",headRefOid:$h,headRepository:{name:"uzi"},headRepositoryOwner:{login:"test"}}')
  export PR_JSON
  land "$1"
  [ "$RC" -eq 0 ] || fail "$1: initial land expected exit 0, got $RC: $OUT"
  git -C "$d/wt" config user.name test
  git -C "$d/wt" config user.email test@example.com
  git -C "$d/wt" mv api/internal/store/migrations/00002_foo.sql api/internal/store/migrations/00003_foo.sql
  sed "s/(migration 00002)/(migration $newnum)$reword/" "$d/wt/CHANGELOG.md" > "$d/wt/CHANGELOG.new"
  mv "$d/wt/CHANGELOG.new" "$d/wt/CHANGELOG.md"
  git -C "$d/wt" commit -qam 'renumber 00002 -> 00003 and fix the bullet'
  RC=0
  OUT=$(PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi 77 --repo-root "$d/root" --worktree "$d/wt" --skip-rebase --no-push --gate none 2>&1) || RC=$?
}
# 4. Exactly the renumber substitution: passes.
mig_scenario renum 00003
[ "$RC" -eq 0 ] || fail "renum: the renumber-only bullet edit should pass, got $RC: $OUT"
# 5. The renumber plus an unrelated reword of the same bullet: still stops as missing.
mig_scenario renum_reword 00003 ' and more'
[ "$RC" -eq 11 ] || fail "renum_reword: an extra reword must still stop with exit 11, got $RC: $OUT"
grep -Fq 'missing' <<<"$OUT" || fail "renum_reword: not named as missing: $OUT"
# 6. A number change that is not the recorded rename (00002 -> 00009 while the file became 00003).
mig_scenario renum_wrong 00009
[ "$RC" -eq 11 ] || fail "renum_wrong: a non-rename number change must stop with exit 11, got $RC: $OUT"

# 7-8. Chained renames (00002 -> 00003 and 00003 -> 00004) map each ORIGINAL number once:
#      the correct "00003, 00004" passes; "00004, 00004" (a cascading substitution) stops.
chain_scenario() {  # NAME NEWREFS
  local d="$WORK/$1" refs="$2"
  local branch_m=${BRANCH/new fixed desc/new fixed desc (migrations 00002, 00003)}
  mkdir -p "$d"
  git init -q --bare "$d/origin.git"
  git init -q -b main "$d/seed"
  git -C "$d/seed" config user.name test
  git -C "$d/seed" config user.email test@example.com
  printf '%s' "$ANC" > "$d/seed/CHANGELOG.md"
  mkdir -p "$d/seed/api/internal/store/migrations"
  printf -- '-- base\n' > "$d/seed/api/internal/store/migrations/00001_base.sql"
  git -C "$d/seed" add -A
  git -C "$d/seed" commit -qm base
  git -C "$d/seed" remote add origin "$d/origin.git"
  git -C "$d/seed" push -q -u origin main
  git --git-dir="$d/origin.git" symbolic-ref HEAD refs/heads/main
  git -C "$d/seed" switch -qc feature
  printf '%s' "$branch_m" > "$d/seed/CHANGELOG.md"
  printf -- '-- foo\n' > "$d/seed/api/internal/store/migrations/00002_foo.sql"
  printf -- '-- bar\n' > "$d/seed/api/internal/store/migrations/00003_bar.sql"
  git -C "$d/seed" add -A
  git -C "$d/seed" commit -qm feature
  git -C "$d/seed" push -q -u origin feature
  git clone -q "$d/origin.git" "$d/root"
  git -C "$d/root" config user.name test
  git -C "$d/root" config user.email test@example.com
  PR_JSON=$(jq -cn --arg h "$(git -C "$d/seed" rev-parse feature)" '{state:"OPEN",headRefName:"feature",baseRefName:"main",headRefOid:$h,headRepository:{name:"uzi"},headRepositoryOwner:{login:"test"}}')
  export PR_JSON
  land "$1"
  [ "$RC" -eq 0 ] || fail "$1: initial land expected exit 0, got $RC: $OUT"
  git -C "$d/wt" config user.name test
  git -C "$d/wt" config user.email test@example.com
  git -C "$d/wt" mv api/internal/store/migrations/00003_bar.sql api/internal/store/migrations/00004_bar.sql
  git -C "$d/wt" mv api/internal/store/migrations/00002_foo.sql api/internal/store/migrations/00003_foo.sql
  sed "s/(migrations 00002, 00003)/(migrations $refs)/" "$d/wt/CHANGELOG.md" > "$d/wt/CHANGELOG.new"
  mv "$d/wt/CHANGELOG.new" "$d/wt/CHANGELOG.md"
  git -C "$d/wt" commit -qam 'renumber 00002 -> 00003, 00003 -> 00004'
  RC=0
  OUT=$(PATH="$WORK/bin:$PATH" bash "$SCRIPT" test/uzi 77 --repo-root "$d/root" --worktree "$d/wt" --skip-rebase --no-push --gate none 2>&1) || RC=$?
}
chain_scenario chain_ok '00003, 00004'
[ "$RC" -eq 0 ] || fail "chain_ok: correct chained renumber should pass, got $RC: $OUT"
chain_scenario chain_cascade '00004, 00004'
[ "$RC" -eq 11 ] || fail "chain_cascade: a cascaded substitution must stop with exit 11, got $RC: $OUT"

echo "PASS land-prep-changelog-placement: real conflict-free rebase after a release fold stops with exit 11 naming the misplaced bullets; a rebase keeping them under [Unreleased] passes; a bullet changed only by the recorded migration renumber passes (chained renames map each original once), any other edit stops"
