#!/usr/bin/env bash
# Hermetic CLI grammar, rendering and independent-merge contract.
set -euo pipefail
ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
mkdir -p "$ROOT/.uzi/scratch"
TMP="$(mktemp -d "$ROOT/.uzi/scratch/changelog-fragments-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
CLI="$ROOT/scripts/changelog-fragments.sh"
W="$TMP/work"
mkdir -p "$W/changelog.d"
A=changelog.d/11111111-1111-4111-8111-111111111111.md
B=changelog.d/22222222-2222-4222-a222-222222222222.md
cases=0
passed=0
expect() {
  local want=$1 label=$2 rc=0
  shift 2
  cases=$((cases + 1))
  (cd "$W" && "$CLI" "$@") > "$TMP/out" 2> "$TMP/err" || rc=$?
  if [ "$rc" -eq "$want" ]; then passed=$((passed + 1))
  else echo "FAIL: $label: wanted $want got $rc" >&2; cat "$TMP/err" >&2; fi
}
equal() {
  cases=$((cases + 1))
  if cmp -s "$1" "$2"; then passed=$((passed + 1))
  else echo "FAIL: $3" >&2; diff -u "$1" "$2" >&2 || true; fi
}
valid() { printf '### %s\n\n- **%s.**\n  Description.\n' "$1" "$2" > "$W/$3"; }
# Safety regressions also run alone against a frozen source export.
empty_output() {
  cases=$((cases + 1))
  if [ ! -s "$TMP/out" ]; then passed=$((passed + 1)); else echo "FAIL: $1 emitted stdout" >&2; fi
}
# A failed or truncated directory producer must never look like an empty inventory.
valid Fixed 'Enumeration original' "$A"
: > "$W/enumeration-body"
mkdir "$TMP/find-failure"
cat > "$TMP/find-failure/find" <<'SH'
#!/usr/bin/env bash
set -e
printf '%s\n' "$FIXTURE_ENUM_MODE" > "$FIXTURE_ENUM_MARKER"
printf '%s\0' "$FIXTURE_FRAGMENT"
case "$FIXTURE_ENUM_MODE" in
  failure) printf '\033[2J\nFORGED: success\n' >&2; exit 23 ;;
  truncated) printf 'partial'; exit 0 ;;
  too-many)
    for ((entry=0; entry<100000; entry++)); do printf '%s\0' "$FIXTURE_FRAGMENT"; done
    ;;
esac
SH
chmod +x "$TMP/find-failure/find"
for enumeration in failure truncated too-many; do
  for mode in validate list render-body; do
    args=(); [ "$mode" != render-body ] || args=(enumeration-body)
    PATH="$TMP/find-failure:$PATH" FIXTURE_ENUM_MODE="$enumeration" FIXTURE_ENUM_MARKER="$TMP/enumeration-reached" FIXTURE_FRAGMENT="$A" expect 1 "$enumeration enumeration fails closed in $mode" "$mode" "${args[@]}"
    empty_output "$enumeration enumeration"
    printf '%s\n' "$enumeration" > "$TMP/expected-marker"
    equal "$TMP/expected-marker" "$TMP/enumeration-reached" 'enumeration fault reached'
    diagnostic='cannot enumerate changelog.d'
    [ "$enumeration" != too-many ] || diagnostic='more than 1024 fragments'
    printf 'changelog-fragments: %s\n' "$diagnostic" > "$TMP/expected-error"
    equal "$TMP/expected-error" "$TMP/err" 'enumeration diagnostic excludes raw producer stderr'
  done
done
rm "$W/$A"
if [ "${1:-}" = --enumeration-only ]; then
  echo "changelog-fragments enumeration: cases=$cases passed=$passed"
  [ "$cases" -eq "$passed" ]; exit
fi
bidi_path=$(python3 -c "print('changelog.d/' + chr(0x202e) + 'bad' + chr(0x202c) + '.md')")
for bad in $'changelog.d/\033[2J\nFORGED: success.md' "$bidi_path"; do
  expect 1 'unsafe path diagnostic' validate "$bad"
  LC_ALL=C printf 'changelog-fragments: invalid fragment path: %q\n' "$bad" > "$TMP/expected-error"
  equal "$TMP/expected-error" "$TMP/err" 'exact quoted diagnostic bytes'
done
for control in 061c 200e 200f 202a 202b 202c 202d 202e 2066 2067 2068 2069; do
  direction=$(python3 -c 'import sys; print(chr(int(sys.argv[1], 16)), end="")' "$control")
  printf '### Fixed\n\n- **Outcome.**\n  Unsafe %s text.\n' "$direction" > "$W/$A"
  expect 1 "fragment direction control $control" validate "$A"
  empty_output 'direction control validation'
  printf '### Fixed\n\n- Legacy %s text\n' "$direction" > "$W/safety-body"
  rm "$W/$A"
  expect 1 "legacy direction control $control" render-body safety-body
  empty_output 'direction control render'
  printf '%s\n' 'changelog-fragments: .uzi/scratch/PLACEHOLDER/legacy:3: direction control in input' > "$TMP/expected-error"
  # Normalize only the random scratch directory; message and byte safety stay exact.
  sed -E 's@changelog-render\.[^/]+@PLACEHOLDER@' "$TMP/err" > "$TMP/normalized-error"
  equal "$TMP/expected-error" "$TMP/normalized-error" 'direction diagnostic has no raw author bytes'
done
printf '### Fixed\n\n- **Café 😀.**\n  Déjà vu 🎉.\n' > "$W/$A"
printf '### Fixed\n\n- Café 😀\n  Déjà vu 🎉.\n' > "$W/safety-body"
expect 0 'normal UTF-8 validation' validate "$A"
expect 0 'normal UTF-8 render' render-body safety-body
cat > "$TMP/utf8-expected" <<'UTF8'
### Fixed

- Café 😀
  Déjà vu 🎉.

- **Café 😀.**
  Déjà vu 🎉.
UTF8
equal "$TMP/utf8-expected" "$TMP/out" 'UTF-8 bytes preserved'
python3 - "$W/$A" <<'PYFIX'
from pathlib import Path
import sys
prefix = b'### Fixed\n\n- **Outcome.**\n  '
Path(sys.argv[1]).write_bytes(prefix + b'x' * (65536 - len(prefix) - 1) + b'\n')
PYFIX
expect 0 '65536 byte fragment boundary' validate "$A"
printf x >> "$W/$A"
for mode in validate list render-body; do
  args=(); [ "$mode" != render-body ] || args=(safety-body)
  expect 1 '65537 byte fragment fails closed' "$mode" "${args[@]}"
  empty_output 'oversized fragment'
  printf 'changelog-fragments: input exceeds 65536 bytes\n' > "$TMP/expected-error"
  equal "$TMP/expected-error" "$TMP/err" 'exact oversize diagnostic'
done
rm "$W/$A"
python3 - "$W" <<'PYFIX'
from pathlib import Path
import sys
w = Path(sys.argv[1])
prefix = b'### Fixed\n\n- '
(w / 'safety-body').write_bytes(prefix + b'x' * (4194304 - len(prefix) - 1) + b'\n')
(w / 'safety-full').write_bytes(b'## [Unreleased]\n\n## [1.0.0]\n' + b'x' * (16777216 - 29))
PYFIX
expect 0 '4MiB pending body boundary' render-body safety-body
printf x >> "$W/safety-body"
expect 1 'pending body over 4MiB' render-body safety-body
empty_output 'oversized body'
# Full legacy history is capped separately and never buffered as pending entries.
python3 - "$W/safety-full" <<'PYFIX'
from pathlib import Path
import sys
p = Path(sys.argv[1])
prefix = b'## [Unreleased]\n\n## [1.0.0]\n'
p.write_bytes(prefix + b'x' * (16777216 - len(prefix)))
PYFIX
expect 0 '16MiB full legacy boundary' render-unreleased safety-full
empty_output 'released history not pending'
printf x >> "$W/safety-full"
expect 1 'full legacy over 16MiB' render-unreleased safety-full
empty_output 'oversized full legacy'
# Combined budget catches individually valid fragments plus a valid body.
python3 - "$W" "$A" <<'PYFIX'
from pathlib import Path
import sys
w = Path(sys.argv[1])
fragment = b'### Fixed\n\n- **Outcome.**\n  Description.\n'
(w / sys.argv[2]).write_bytes(fragment)
prefix = b'### Fixed\n\n- '
(w / 'safety-body').write_bytes(prefix + b'x' * (4194304 - len(fragment) - len(prefix) - 1) + b'\n')
PYFIX
expect 0 'combined pending boundary' render-body safety-body
printf x >> "$W/safety-body"
expect 1 'combined pending over limit' render-body safety-body
empty_output 'combined pending over limit'
rm "$W/$A"
python3 - "$W/changelog.d" <<'PYFIX'
from pathlib import Path
import sys
for i in range(1024):
    (Path(sys.argv[1]) / f'{i:08x}-1111-4111-8111-111111111111.md').write_text('### Fixed\n\n- **Outcome.**\n  Description.\n')
PYFIX
expect 0 '1024 fragments boundary' list
valid Fixed Extra "$B"
expect 1 '1025 fragments rejected' list
empty_output 'fragment count limit'
rm "$W/changelog.d/"*.md
# 65 valid 64KiB fragments exceed the aggregate before any output.
python3 - "$W/changelog.d" <<'PYFIX'
from pathlib import Path
import sys
prefix = b'### Fixed\n\n- **Outcome.**\n  '
for i in range(65):
    (Path(sys.argv[1]) / f'{i:08x}-1111-4111-8111-111111111111.md').write_bytes(prefix + b'x' * (65536-len(prefix)-1) + b'\n')
PYFIX
expect 1 'fragment inventory aggregate limit' list
empty_output 'inventory aggregate limit'
rm "$W/changelog.d/"*.md
# Change the tracked source immediately after validation. Rendering must consume
# the validated snapshot, never an unbounded second read of the source path.
valid Fixed 'Snapshot original' "$A"
printf '### Fixed\n\n- **Snapshot original.**\n  Description.\n' > "$TMP/snapshot-expected"
: > "$W/safety-body"
mkdir "$TMP/awk-swap"
cat > "$TMP/awk-swap/awk" <<'SH'
#!/usr/bin/env bash
"$FIXTURE_AWK" "$@" || exit
if [ "${1:-}" = -v ] && [ "${2:-}" = mode=validate ]; then
  printf '### Fixed\n\n- **Source replaced.**\n  Replacement.\n' > "$FIXTURE_FRAGMENT"
  : > "$FIXTURE_MARKER"
fi
SH
chmod +x "$TMP/awk-swap/awk"
real_awk=$(command -v awk)
PATH="$TMP/awk-swap:$PATH" FIXTURE_AWK="$real_awk" FIXTURE_FRAGMENT="$W/$A" FIXTURE_MARKER="$TMP/swap-reached" expect 0 'render uses validated bytes after source replacement' render-body safety-body
equal "$TMP/snapshot-expected" "$TMP/out" 'rendered exact snapshot bytes'
cases=$((cases + 1))
if [ -f "$TMP/swap-reached" ]; then passed=$((passed + 1)); else echo 'FAIL: source replacement boundary not reached' >&2; fi
rm "$W/$A"
if [ "${1:-}" = --safety-only ]; then
  echo "changelog-fragments safety: cases=$cases passed=$passed"
  [ "$cases" -eq "$passed" ]; exit
fi
for category in Added Changed Deprecated Removed Fixed Security; do
  valid "$category" 'Outcome (#99)' "$A"
  expect 0 "category $category" validate
done
valid Fixed Outcome "$A"
expect 0 "identity belongs to oracle" validate "$A"
printf '\n\n' >> "$W/$A"
expect 0 "optional trailing blanks" validate
echo 'arbitrary README prose' > "$W/changelog.d/README.md"
expect 0 "README excluded" validate
expect 1 "README is not an explicit fragment" validate changelog.d/README.md

for malformed in empty unknown leading no-blank plain-title split-bold-title no-period no-description blank-description one-space three-space nested-bullet nested-heading numbered-bullet wrapped prose extra-bullet extra-heading crlf; do
  valid Fixed Outcome "$A"
  case "$malformed" in
    empty) : > "$W/$A" ;;
    unknown) printf '### Other\n\n- **Outcome.**\n  Description.\n' > "$W/$A" ;;
    leading) printf '\n### Fixed\n\n- **Outcome.**\n  Description.\n' > "$W/$A" ;;
    no-blank) printf '### Fixed\n- **Outcome.**\n  Description.\n' > "$W/$A" ;;
    plain-title) printf '### Fixed\n\n- Outcome.\n  Description.\n' > "$W/$A" ;;
    split-bold-title) printf '### Fixed\n\n- **Outcome.** unbold prose **Other.**\n  Description.\n' > "$W/$A" ;;
    no-period) printf '### Fixed\n\n- **Outcome**\n  Description.\n' > "$W/$A" ;;
    no-description) printf '### Fixed\n\n- **Outcome.**\n' > "$W/$A" ;;
    blank-description) printf '### Fixed\n\n- **Outcome.**\n  \n' > "$W/$A" ;;
    one-space) printf '### Fixed\n\n- **Outcome.**\n Description.\n' > "$W/$A" ;;
    three-space) printf '### Fixed\n\n- **Outcome.**\n   Description.\n' > "$W/$A" ;;
    nested-bullet) printf '### Fixed\n\n- **Outcome.**\n  - extra bullet\n' > "$W/$A" ;;
    nested-heading) printf '### Fixed\n\n- **Outcome.**\n  ### Added\n' > "$W/$A" ;;
    numbered-bullet) printf '### Fixed\n\n- **Outcome.**\n  1. extra bullet\n' > "$W/$A" ;;
    wrapped) echo '  second physical line' >> "$W/$A" ;;
    prose) echo prose >> "$W/$A" ;;
    extra-bullet) echo '- extra' >> "$W/$A" ;;
    extra-heading) echo '### Added' >> "$W/$A" ;;
    crlf) printf '### Fixed\r\n\r\n- **Outcome.**\r\n  Description.\r\n' > "$W/$A" ;;
  esac
  expect 1 "malformed $malformed" validate
done
# Exercise the raw-byte guard with both the host awk and BusyBox record splitting.
mkdir -p "$TMP/busybox-bin"
printf '#!/usr/bin/env bash\nexec busybox awk "$@"\n' > "$TMP/busybox-bin/awk"
chmod +x "$TMP/busybox-bin/awk"
host_path=$PATH
implementations=(host)
if command -v busybox > /dev/null; then implementations+=(busybox); fi
for implementation in "${implementations[@]}"; do
  if [ "$implementation" = busybox ]; then export PATH="$TMP/busybox-bin:$host_path"; fi
  printf '### Fixed\0\0- **Outcome.**\0  Description.\0' > "$W/$A"
  expect 1 "$implementation rejects NUL fragment" validate "$A"
  equal /dev/null "$TMP/out" "$implementation NUL validation has empty stdout"
  : > "$W/body-nul"
  expect 1 "$implementation NUL fragment cannot render" render-body body-nul
  equal /dev/null "$TMP/out" "$implementation NUL fragment render has empty stdout"
  valid Fixed Outcome "$A"
  printf '### Fixed\0\0- existing\0  Description.\0' > "$W/body-nul"
  expect 1 "$implementation rejects NUL legacy body" render-body body-nul
  equal /dev/null "$TMP/out" "$implementation NUL body has empty stdout"
  printf '## [Unreleased]\n\n### Fixed\0\0- existing\0  Description.\0\n## [1.0.0]\n\n- released\n' > "$W/full-nul"
  expect 1 "$implementation rejects NUL before extraction" render-unreleased full-nul
  equal /dev/null "$TMP/out" "$implementation NUL fullfile has empty stdout"
  export PATH=$host_path
done
printf '\0' > "$W/-"
expect 1 "NUL-only literal dash file" render-body -
equal /dev/null "$TMP/out" "literal dash NUL rejection has empty stdout"
valid Fixed Outcome "$A"
for name in bad.md 11111111-1111-3111-8111-111111111111.md 11111111-1111-4111-c111-111111111111.md A1111111-1111-4111-8111-111111111111.md .hidden.md $'bad\nname.md'; do
  cp "$W/$A" "$W/changelog.d/$name"
  expect 1 "invalid path $name" list
  rm "$W/changelog.d/$name"
done
expect 1 "path traversal spelling" validate changelog.d/../changelog.d/11111111-1111-4111-8111-111111111111.md
expect 1 "absent explicit fragment" validate "$B"
for unsafe in symlink fifo directory; do
  case "$unsafe" in
    symlink) ln -s "${A##*/}" "$W/$B" ;;
    fifo) mkfifo "$W/$B" ;;
    directory) mkdir "$W/$B" ;;
  esac
  expect 1 "unsafe $unsafe" validate
  rm -rf "${W:?}/${B:?}"
done
mv "$W/changelog.d" "$W/real"
ln -s real "$W/changelog.d"
expect 1 "symlink parent" validate "$A"
rm "$W/changelog.d"
mv "$W/real" "$W/changelog.d"

# Literal expected blocks deliberately include older formats and duplicate prose.
cat > "$W/body" <<'BODY'
<!-- release-title: Keep this title -->

### Fixed

- old one-line (#1)

- **Old title.**
  wrapped description line one
  wrapped description line two with trailing spaces<TRAILING>
old unindented continuation

### Added

- existing added

### Fixed

- duplicate
- duplicate
BODY
valid Fixed 'Second fragment (#2)' "$B"
valid Added 'First fragment (#1)' "$A"
cat > "$TMP/expected" <<'BODY'
<!-- release-title: Keep this title -->

### Added

- existing added

- **First fragment (#1).**
  Description.

### Fixed

- old one-line (#1)

- **Old title.**
  wrapped description line one
  wrapped description line two with trailing spaces<TRAILING>
old unindented continuation

- duplicate

- duplicate

- **Second fragment (#2).**
  Description.
BODY
# Assemble significant trailing spaces at runtime so the test source has no drift.
for fixture in "$W/body" "$TMP/expected"; do
  awk '{ sub(/<TRAILING>$/, "  "); print }' "$fixture" > "$TMP/spaces"
  mv "$TMP/spaces" "$fixture"
done
expect 0 "render legacy and fragments" render-body body
equal "$TMP/expected" "$TMP/out" "canonical grouping and exact legacy block bytes"
cp "$W/body" "$W/-"
expect 0 "literal dash legacy file" render-body -
equal "$TMP/expected" "$TMP/out" "literal dash input preserves legacy bytes"
printf '%s\n%s\n' "$A" "$B" > "$TMP/list"
expect 0 "list" list
equal "$TMP/list" "$TMP/out" "deterministic incorporated paths"
{ printf '# Changelog\n\n## [Unreleased]\n\n'; cat "$W/body"; printf '\n## [1.0.0]\n\n- released\n'; } > "$W/CHANGELOG.md"
expect 0 "full changelog interface" render-unreleased CHANGELOG.md
equal "$TMP/expected" "$TMP/out" "full changelog and body agree"
expect 0 "absolute legacy input" render-unreleased "$W/CHANGELOG.md"
equal "$TMP/expected" "$TMP/out" "absolute file operand"
for malformed in unknown orphan malformed-marker malformed-marker-continuation indented-marker-continuation canonical-marker-continuation malformed-marker-fullfile unexpected-heading-fullfile nul-body nul-fullfile misplaced-marker duplicate-marker out-of-category missing-unreleased duplicate-unreleased; do
  case "$malformed" in
    unknown) printf '### Other\n\n- old\n' > "$W/bad" ;;
    orphan) printf '### Fixed\n\nprose\n' > "$W/bad" ;;
    malformed-marker) printf '<!-- release-title: Missing close\n' > "$W/bad" ;;
    malformed-marker-continuation) printf '### Fixed\n\n- existing\n<!-- release-title: Missing close\n' > "$W/bad" ;;
    indented-marker-continuation) printf '### Fixed\n\n- existing\n  <!-- release-title: Missing close\n' > "$W/bad" ;;
    canonical-marker-continuation) printf '### Fixed\n\n- existing\n<!-- release-title: misplaced -->\n' > "$W/bad" ;;
    malformed-marker-fullfile) printf '## [Unreleased]\n\n### Fixed\n\n- existing\n<!-- release-title: Missing close\n\n## [1.0.0]\n\n- released\n' > "$W/bad" ;;
    unexpected-heading-fullfile) printf '## [Unreleased]\n\n### Fixed\n\n- keep\n\n## unexpected\n\n- lost\n\n## [1.0.0]\n\n- released\n' > "$W/bad" ;;
    nul-body) printf '### Fixed\0\0- existing\0  Description.\0' > "$W/bad" ;;
    nul-fullfile) printf '## [Unreleased]\n\n### Fixed\0\0- existing\0  Description.\0\n## [1.0.0]\n\n- released\n' > "$W/bad" ;;
    misplaced-marker) printf '### Fixed\n\n<!-- release-title: misplaced -->\n' > "$W/bad" ;;
    duplicate-marker) printf '<!-- release-title: one -->\n<!-- release-title: two -->\n' > "$W/bad" ;;
    out-of-category) printf -- '- old\n' > "$W/bad" ;;
    missing-unreleased) printf '# Changelog\n' > "$W/bad" ;;
    duplicate-unreleased) printf '## [Unreleased]\n\n## [Unreleased]\n' > "$W/bad" ;;
  esac
  mode=render-body
  case "$malformed" in *unreleased|*fullfile) mode=render-unreleased ;; esac
  expect 1 "fail closed on $malformed" "$mode" bad
  cases=$((cases + 1))
  if [ ! -s "$TMP/out" ]; then passed=$((passed + 1)); else echo "FAIL: partial output on $malformed" >&2; fi
done

# Canonical ordering across all categories, filename ordering within a category,
# and equal prose from separate fragments (no deduplication).
saved_w=$W
W="$TMP/categories"
mkdir -p "$W/changelog.d"
: > "$W/body"
n=9
for category in Security Fixed Removed Deprecated Changed Added; do
  path="changelog.d/00000000-0000-4000-8000-00000000000$n.md"
  valid "$category" "$category outcome" "$path"
  n=$((n - 1))
done
valid Fixed 'A filename first' "$A"
valid Fixed 'A filename first' "$B"
cat > "$TMP/categories-expected" <<'BODY'
### Added

- **Added outcome.**
  Description.

### Changed

- **Changed outcome.**
  Description.

### Deprecated

- **Deprecated outcome.**
  Description.

### Removed

- **Removed outcome.**
  Description.

### Fixed

- **Fixed outcome.**
  Description.

- **A filename first.**
  Description.

- **A filename first.**
  Description.

### Security

- **Security outcome.**
  Description.
BODY
expect 0 "all categories, sorted fragments and equal prose" render-body body
equal "$TMP/categories-expected" "$TMP/out" "canonical category order without deduplicating equal prose"
valid Fixed 'Z filename last' "$B"
# An explicit expected last title distinguishes filename order from title sorting.
awk '/A filename first/ { seen++; if (seen == 2) sub(/A filename first/, "Z filename last") } { print }' "$TMP/categories-expected" > "$TMP/categories-sorted"
expect 0 "fragment sorting follows filenames" render-body body
equal "$TMP/categories-sorted" "$TMP/out" "filename order within Fixed"
W=$saved_w

# Independent branches add disjoint fragments, merged in both orders in one
# scratch fixture repo (no nested worktree). The real CHANGELOG is never edited.
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
rm "$W/$A" "$W/$B"
git -C "$W" init -q -b main
git -C "$W" add -- CHANGELOG.md body changelog.d/README.md
git -C "$W" commit -qm init
git -C "$W" checkout -qb first
valid Added 'First fragment (#1)' "$A"
git -C "$W" add -- "$A"
git -C "$W" commit -qm first
git -C "$W" checkout -qb second main
valid Fixed 'Second fragment (#2)' "$B"
git -C "$W" add -- "$B"
git -C "$W" commit -qm second
for order in ab ba; do
  git -C "$W" checkout -qb "merge-$order" main
  if [ "$order" = ab ]; then branches=(first second); else branches=(second first); fi
  for branch in "${branches[@]}"; do git -C "$W" merge -q --no-ff -m merge "$branch"; done
  expect 0 "render merged $order" render-unreleased CHANGELOG.md
  cp "$TMP/out" "$TMP/render-$order"
  git -C "$W" ls-files -z > "$TMP/files-$order"
  # Build a full candidate without touching the tracked fixture changelog.
  { printf '# Changelog\n\n## [Unreleased]\n\n'; cat "$TMP/out"; printf '\n## [1.0.0]\n\n- released\n'; } > "$TMP/CHANGELOG-$order"
done
equal "$TMP/files-ab" "$TMP/files-ba" "both merge orders have identical final file sets"
equal "$TMP/CHANGELOG-ab" "$TMP/CHANGELOG-ba" "both merge orders yield byte-identical CHANGELOG"
equal "$TMP/expected" "$TMP/render-ab" "no existing PR bullet moved within its category"
git -C "$W" show main:CHANGELOG.md > "$TMP/original"
equal "$TMP/original" "$W/CHANGELOG.md" "render never mutates actual changelog"

echo "changelog-fragments.test.sh: cases=$cases passed=$passed"
[ "$cases" -eq "$passed" ] && [ "$cases" -ge 101 ]
