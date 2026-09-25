#!/usr/bin/env bash
# Hermetic tests of changelog-union.sh: a two-sided conflict becomes a union, a repeated
# `### Fixed` heading under [Unreleased] collapses into the first, a resolution that would
# lose a content line exits non-zero with the file untouched, and a marker-free file is a
# no-op success.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/changelog-union.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

# 1. Two-sided Fixed conflict (diff3 style, with a base section to drop) -> union, both kept.
cat > "$WORK/two.md" <<'EOF'
# Changelog

## [Unreleased]

### Fixed

- **base fix**
  base desc
<<<<<<< HEAD
- **main fix**
  main desc
||||||| parent of abc1234 (branch)
=======

- **branch fix**
  branch desc
>>>>>>> abc1234 (branch)

## [0.1.0] - 2026-01-01

### Fixed

- **old**
  old desc
EOF
cat > "$WORK/two.want" <<'EOF'
# Changelog

## [Unreleased]

### Fixed

- **base fix**
  base desc
- **main fix**
  main desc
- **branch fix**
  branch desc

## [0.1.0] - 2026-01-01

### Fixed

- **old**
  old desc
EOF
bash "$SCRIPT" "$WORK/two.md" > "$WORK/two.out" 2>&1 || fail "two-sided union failed: $(cat "$WORK/two.out")"
diff -u "$WORK/two.want" "$WORK/two.md" || fail "two-sided union produced the wrong file"

# 2. One side adds its own `### Fixed` section next to the existing one, the other side a
#    bare bullet: all collapse into the first `### Fixed` under [Unreleased]; released
#    sections are untouched.
cat > "$WORK/dup.md" <<'EOF'
## [Unreleased]

### Added

- **added**
  added desc

### Fixed

- **first fix**
  first desc

<<<<<<< HEAD
### Fixed

- **main fix**
  main desc
||||||| parent
=======
- **branch fix**
  branch desc
>>>>>>> def5678 (branch)

## [0.1.0] - 2026-01-01

### Fixed

- **old**

### Fixed

- **old two**
EOF
cat > "$WORK/dup.want" <<'EOF'
## [Unreleased]

### Added

- **added**
  added desc

### Fixed

- **first fix**
  first desc
- **main fix**
  main desc
- **branch fix**
  branch desc

## [0.1.0] - 2026-01-01

### Fixed

- **old**

### Fixed

- **old two**
EOF
bash "$SCRIPT" "$WORK/dup.md" > "$WORK/dup.out" 2>&1 || fail "duplicate collapse failed: $(cat "$WORK/dup.out")"
diff -u "$WORK/dup.want" "$WORK/dup.md" || fail "duplicate ### Fixed was not collapsed"
[ "$(grep -c '^### Fixed$' "$WORK/dup.md")" -eq 3 ] || fail "wrong ### Fixed count after collapse"

# 3. A resolution that would lose a content line: an awk shim drops one bullet from the
#    collapse pass's output. The helper must exit non-zero and leave the file byte-identical.
cat > "$WORK/lossy.md" <<'EOF'
## [Unreleased]

### Fixed

<<<<<<< HEAD
- **keep me**
||||||| parent
=======
- **branch fix**
>>>>>>> abc1234 (branch)
EOF
cp "$WORK/lossy.md" "$WORK/lossy.orig"
mkdir -p "$WORK/bin"
REAL_AWK=$(command -v awk)
cat > "$WORK/bin/awk" <<STUB
#!/usr/bin/env bash
case "\$*" in
  *emit_body*) "$REAL_AWK" "\$@" | "$REAL_AWK" '\$0 != "- **keep me**"';;
  *) exec "$REAL_AWK" "\$@";;
esac
STUB
chmod +x "$WORK/bin/awk"
set +e
PATH="$WORK/bin:$PATH" bash "$SCRIPT" "$WORK/lossy.md" > "$WORK/lossy.out" 2>&1
rc=$?
set -e
[ "$rc" -ne 0 ] || fail "a lossy resolution exited 0: $(cat "$WORK/lossy.out")"
cmp -s "$WORK/lossy.orig" "$WORK/lossy.md" || fail "a lossy resolution modified the file"
grep -qF 'keep me' "$WORK/lossy.out" || fail "the lost line was not named: $(cat "$WORK/lossy.out")"
# ...and an unterminated conflict block is refused the same way.
printf '## [Unreleased]\n\n<<<<<<< HEAD\n- a\n||||||| parent\n=======\n- b\n' > "$WORK/open.md"
cp "$WORK/open.md" "$WORK/open.orig"
set +e
bash "$SCRIPT" "$WORK/open.md" > "$WORK/open.out" 2>&1
rc=$?
set -e
[ "$rc" -ne 0 ] || fail "an unterminated conflict exited 0"
cmp -s "$WORK/open.orig" "$WORK/open.md" || fail "an unterminated conflict modified the file"

# 3b. FAIL-CLOSED: a bullet on BOTH sides of one conflict block (each side also has its own)
#     would be written twice: refuse, file untouched.
cat > "$WORK/shared.md" <<'EOF'
## [Unreleased]

### Fixed

<<<<<<< HEAD
- **shared fix**
- **main only**
||||||| parent
=======
- **shared fix**
- **branch only**
>>>>>>> abc1234 (branch)
EOF
cp "$WORK/shared.md" "$WORK/shared.orig"
set +e
bash "$SCRIPT" "$WORK/shared.md" > "$WORK/shared.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "a line on both sides returned rc=$rc, want 1: $(cat "$WORK/shared.out")"
cmp -s "$WORK/shared.orig" "$WORK/shared.md" || fail "a line on both sides modified the file"
grep -qF 'line on both sides of the conflict ending at line 12: - **shared fix**' "$WORK/shared.out" || fail "the shared line was not named: $(cat "$WORK/shared.out")"
# 3c. ...the same for HEADINGS: each side carries `## [Unreleased]` / `### Fixed` / its own
#     bullet, which a union would turn into two [Unreleased] sections.
cat > "$WORK/twosec.md" <<'EOF'
# Changelog

<<<<<<< HEAD
## [Unreleased]

### Fixed

- **main only**
||||||| parent
=======
## [Unreleased]

### Fixed

- **branch only**
>>>>>>> abc1234 (branch)

## [0.1.0] - 2026-01-01

- **old**
EOF
cp "$WORK/twosec.md" "$WORK/twosec.orig"
set +e
bash "$SCRIPT" "$WORK/twosec.md" > "$WORK/twosec.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "a two-sided [Unreleased] section returned rc=$rc, want 1: $(cat "$WORK/twosec.out")"
cmp -s "$WORK/twosec.orig" "$WORK/twosec.md" || fail "a two-sided [Unreleased] section modified the file"
# 3d. Sides that share no line but would repeat a version heading (same version, different
#     dates) are refused: a `## ` heading inside a block always refuses (the repeated-version
#     post-check stays as a second line of defence behind it).
cat > "$WORK/twover.md" <<'EOF'
## [Unreleased]

<<<<<<< HEAD
## [0.2.0] - 2026-02-01

- **main only**
||||||| parent
=======
## [0.2.0] - 2026-02-02

- **branch only**
>>>>>>> abc1234 (branch)
EOF
cp "$WORK/twover.md" "$WORK/twover.orig"
set +e
bash "$SCRIPT" "$WORK/twover.md" > "$WORK/twover.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "a repeated version heading returned rc=$rc, want 1: $(cat "$WORK/twover.out")"
cmp -s "$WORK/twover.orig" "$WORK/twover.md" || fail "a repeated version heading modified the file"
grep -qF 'a `## ` heading inside the conflict' "$WORK/twover.out" || fail "the version-heading conflict was not named: $(cat "$WORK/twover.out")"
# 3e. Positive: distinct bullets on the two sides still union.
printf '## [Unreleased]\n\n### Fixed\n\n<<<<<<< HEAD\n- **main only**\n||||||| parent\n=======\n- **branch only**\n>>>>>>> abc (b)\n' > "$WORK/distinct.md"
bash "$SCRIPT" "$WORK/distinct.md" > "$WORK/distinct.out" 2>&1 || fail "distinct bullets did not union: $(cat "$WORK/distinct.out")"
[ "$(printf '## [Unreleased]\n\n### Fixed\n\n- **main only**\n- **branch only**\n\n')" = "$(cat "$WORK/distinct.md"; echo)" ] || fail "distinct union wrong: $(cat "$WORK/distinct.md")"

# 3f. The common real conflict (#1644): each side brings its own `### Fixed` section, one
#     side also a `### Changed`. A shared `### ` subsection heading inside [Unreleased] is
#     allowed: one `### Fixed` remains and every bullet stays under its own section.
cat > "$WORK/sub.md" <<'EOF'
## [Unreleased]

### Added

- **added**

<<<<<<< HEAD
### Fixed

- **main fix**
||||||| parent
=======
### Changed

- **branch changed**

### Fixed

- **branch fix**
>>>>>>> abc1234 (branch)

## [0.1.0] - 2026-01-01

- **old**
EOF
cat > "$WORK/sub.want" <<'EOF'
## [Unreleased]

### Added

- **added**

### Fixed

- **main fix**
- **branch fix**

### Changed

- **branch changed**

## [0.1.0] - 2026-01-01

- **old**
EOF
cp "$WORK/sub.md" "$WORK/sub.orig"
bash "$SCRIPT" "$WORK/sub.md" > "$WORK/sub.out" 2>&1 || fail "a shared ### subsection heading was refused: $(cat "$WORK/sub.out")"
diff -u "$WORK/sub.want" "$WORK/sub.md" || fail "shared-subsection union produced the wrong file"
# 3h. The verification is section-aware: an awk shim moves `- **branch changed**` under
#     `### Fixed` in the collapse output (same lines, wrong section). Refused, file untouched.
cp "$WORK/sub.orig" "$WORK/move.md"
REAL_AWK2=$(command -v awk)
mkdir -p "$WORK/bin2"
cat > "$WORK/bin2/awk" <<STUB
#!/usr/bin/env bash
case "\$*" in
  *emit_body*) "$REAL_AWK2" "\$@" | "$REAL_AWK2" '\$0 == "- **branch changed**" { next } { print } \$0 == "- **main fix**" { print "- **branch changed**" }';;
  *) exec "$REAL_AWK2" "\$@";;
esac
STUB
chmod +x "$WORK/bin2/awk"
set +e
PATH="$WORK/bin2:$PATH" bash "$SCRIPT" "$WORK/move.md" > "$WORK/move.out" 2>&1
rc=$?
set -e
[ "$rc" -ne 0 ] || fail "a line moved to another section was accepted: $(cat "$WORK/move.out")"
cmp -s "$WORK/sub.orig" "$WORK/move.md" || fail "a section-changing resolution modified the file"

# 3g. ...but a shared `### ` heading OUTSIDE [Unreleased] (a released section, which the
#     collapse never touches) is still refused, file untouched.
cat > "$WORK/relsub.md" <<'EOF'
## [Unreleased]

- **u**

## [0.1.0] - 2026-01-01

<<<<<<< HEAD
### Fixed

- **main old**
||||||| parent
=======
### Fixed

- **branch old**
>>>>>>> abc1234 (branch)
EOF
cp "$WORK/relsub.md" "$WORK/relsub.orig"
set +e
bash "$SCRIPT" "$WORK/relsub.md" > "$WORK/relsub.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "a shared ### heading in a released section returned rc=$rc, want 1: $(cat "$WORK/relsub.out")"
cmp -s "$WORK/relsub.orig" "$WORK/relsub.md" || fail "a shared released-section heading modified the file"

# 3i. A later commit REWORDS its own earlier bullet (#1644): the diff3 base holds the old
#     wording, the commit's side the new. A union would keep both: refused, file untouched.
cat > "$WORK/reword.md" <<'EOF'
## [Unreleased]

### Fixed

<<<<<<< HEAD
- **main fix**
- **note, first wording**
||||||| parent of abc1234 (branch)
- **note, first wording**
=======
- **note, second wording**
>>>>>>> abc1234 (branch)
EOF
cp "$WORK/reword.md" "$WORK/reword.orig"
set +e
bash "$SCRIPT" "$WORK/reword.md" > "$WORK/reword.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "a reworded bullet returned rc=$rc, want 1: $(cat "$WORK/reword.out")"
cmp -s "$WORK/reword.orig" "$WORK/reword.md" || fail "a reworded bullet modified the file"
grep -qF 'a side deletes or rewords a line of the conflict ending at line 12: - **note, first wording**' "$WORK/reword.out" || fail "the reworded line was not named: $(cat "$WORK/reword.out")"
# 3j. A block with no diff3 base section cannot be checked for that: refused, file untouched.
printf '## [Unreleased]\n\n<<<<<<< HEAD\n- **a**\n=======\n- **b**\n>>>>>>> x (y)\n' > "$WORK/nobase.md"
cp "$WORK/nobase.md" "$WORK/nobase.orig"
set +e
bash "$SCRIPT" "$WORK/nobase.md" > "$WORK/nobase.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "a block without a base section returned rc=$rc, want 1"
cmp -s "$WORK/nobase.orig" "$WORK/nobase.md" || fail "a block without a base section modified the file"
grep -qF 'no diff3 base section' "$WORK/nobase.out" || fail "missing base not named: $(cat "$WORK/nobase.out")"

# 3k. Two blocks: the first carries DISTINCT `## [0.1.0]` / `## [0.2.0]` headings, so the
#     second sits in a released section; its shared `### Fixed` must not be treated as
#     [Unreleased]. Refused, file untouched.
cat > "$WORK/twoblk.md" <<'EOF'
## [Unreleased]

- **u**

<<<<<<< HEAD
## [0.1.0] - 2026-01-01
||||||| parent
=======
## [0.2.0] - 2026-02-01
>>>>>>> abc1234 (branch)

<<<<<<< HEAD
### Fixed

- **main old**
||||||| parent
=======
### Fixed

- **branch old**
>>>>>>> abc1234 (branch)
EOF
cp "$WORK/twoblk.md" "$WORK/twoblk.orig"
set +e
bash "$SCRIPT" "$WORK/twoblk.md" > "$WORK/twoblk.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "a block after a version-heading block returned rc=$rc, want 1: $(cat "$WORK/twoblk.out")"
cmp -s "$WORK/twoblk.orig" "$WORK/twoblk.md" || fail "a version-heading block modified the file"

# 4. No markers: success, file untouched (even with a duplicate heading).
printf '## [Unreleased]\n\n### Fixed\n\n- a\n\n### Fixed\n\n- b\n' > "$WORK/clean.md"
cp "$WORK/clean.md" "$WORK/clean.orig"
bash "$SCRIPT" "$WORK/clean.md" > "$WORK/clean.out" 2>&1 || fail "a marker-free file failed: $(cat "$WORK/clean.out")"
cmp -s "$WORK/clean.orig" "$WORK/clean.md" || fail "a marker-free file was modified"

# 5. --collapse on a clean file: a repeated `### Fixed` under [Unreleased] (non-adjacent too)
#    folds into the first; every other line stays byte-identical, released sections included.
cat > "$WORK/col.md" <<'EOF'
## [Unreleased]

### Fixed

- **branch fix**
  branch desc

### Fixed

- **base fix**

- **spaced base**

### Changed

- **changed**

### Fixed

- **late fix**

## [0.1.0] - 2026-01-01

### Fixed

- **old**

### Fixed

- **old two**
EOF
cat > "$WORK/col.want" <<'EOF'
## [Unreleased]

### Fixed

- **branch fix**
  branch desc
- **base fix**

- **spaced base**
- **late fix**

### Changed

- **changed**

## [0.1.0] - 2026-01-01

### Fixed

- **old**

### Fixed

- **old two**
EOF
bash "$SCRIPT" --collapse "$WORK/col.md" > "$WORK/col.out" 2>&1 || fail "--collapse failed: $(cat "$WORK/col.out")"
diff -u "$WORK/col.want" "$WORK/col.md" || fail "--collapse produced the wrong file"
# 6. --collapse with no repeat: untouched (blank lines between bullets included).
printf '## [Unreleased]\n\n### Fixed\n\n- a\n\n- b\n\n### Added\n\n- c\n' > "$WORK/nodup.md"
cp "$WORK/nodup.md" "$WORK/nodup.orig"
bash "$SCRIPT" --collapse "$WORK/nodup.md" > "$WORK/nodup.out" 2>&1 || fail "--collapse on a clean file failed"
cmp -s "$WORK/nodup.orig" "$WORK/nodup.md" || fail "--collapse changed a file with no repeated heading"
# 7. --collapse refuses conflict markers, file untouched.
cp "$WORK/lossy.orig" "$WORK/mk.md"
set +e
bash "$SCRIPT" --collapse "$WORK/mk.md" > "$WORK/mk.out" 2>&1
rc=$?
set -e
[ "$rc" -eq 1 ] || fail "--collapse on a conflicted file returned rc=$rc, want 1"
cmp -s "$WORK/lossy.orig" "$WORK/mk.md" || fail "--collapse modified a conflicted file"

echo "PASS changelog-union: union, duplicate-heading collapse, lossy refusal, both-sides and repeated-version refusals, shared ### subsections, reword, no-base and version-heading refusals, no-op, --collapse"
