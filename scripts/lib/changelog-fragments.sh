#!/usr/bin/env bash
# Shared filesystem and grammar checks. Caller runs at the repository root.
# Loops visit each supplied file once; the first error aborts the operation.
changelog_fragment_error() { printf 'changelog-fragments: %s\n' "$*" >&2; return 1; }

# Path diagnostics use C-locale quoting, including UTF-8 direction-control bytes.
changelog_fragment_path_error() {
  local quoted
  local LC_ALL=C
  printf -v quoted '%q' "$2"
  changelog_fragment_error "$1: $quoted"
}

# Consume at most limit+1 bytes once, then parse only the bounded snapshot.
# Limits are literals at call sites, never environment overrides.
changelog_fragment_snapshot() {
  local input=$1 output=$2 limit=$3 bytes
  if ! { head -c "$((limit + 1))" < "$input" > "$output"; } 2>/dev/null; then
    changelog_fragment_path_error 'unreadable input' "$input"; return 1
  fi
  bytes=$(wc -c < "$output")
  if [ "$bytes" -gt "$limit" ]; then
    changelog_fragment_error "input exceeds $limit bytes"; return 1
  fi
}

changelog_fragment_directory() {
  if [ -L changelog.d ] || { [ -e changelog.d ] && [ ! -d changelog.d ]; }; then
    changelog_fragment_error 'changelog.d must be a real directory'; return 1
  fi
}

changelog_fragment_path() {
  if [[ ! $1 =~ ^changelog\.d/[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.md$ ]]; then
    changelog_fragment_path_error 'invalid fragment path' "$1"; return 1
  fi
}

# Check bytes before awk can interpret NUL as a record separator.
changelog_fragment_no_nul() {
  local input=$1
  # cmp reserves '-' for stdin even after --; make relative operands literal files.
  case "$input" in /*) ;; *) input="./$input" ;; esac
  if ! tr -d '\000' < "$input" | cmp -s -- "$input" -; then
    changelog_fragment_path_error 'NUL byte or unreadable input' "$1"; return 1
  fi
}

changelog_fragment_validate() (
  export LC_ALL=C
  local path=$1 work
  changelog_fragment_directory || return 1
  changelog_fragment_path "$path" || return 1
  if [ -L "$path" ] || [ ! -f "$path" ]; then
    changelog_fragment_path_error 'fragment must be a regular file' "$path"; return 1
  fi
  mkdir -p .uzi/scratch
  work=$(mktemp -d .uzi/scratch/changelog-validate.XXXXXX) || return 1
  trap 'rm -rf "$work"' EXIT
  changelog_fragment_snapshot "$path" "$work/input" 65536 || return 1
  changelog_fragment_no_nul "$work/input" || return 1
  awk -v mode=validate -f "$CHANGELOG_FRAGMENT_PARSER" "$work/input" || return 1
  if [ "$#" -gt 1 ]; then cp -- "$work/input" "$2"; fi
)

# Git does not enumerate FIFOs or empty directories; inspect the directory too.
changelog_fragment_safe_entries() (
  local path work count=0 total=0 bytes
  mkdir -p .uzi/scratch
  work=$(mktemp -d .uzi/scratch/changelog-sizes.XXXXXX) || return 1
  trap 'rm -rf "$work"' EXIT
  changelog_fragment_directory || return 1
  [ -d changelog.d ] || return 0
  for path in changelog.d/* changelog.d/.[!.]* changelog.d/..?*; do
    [ -e "$path" ] || [ -L "$path" ] || continue
    if [ -L "$path" ] || [ ! -f "$path" ]; then
      changelog_fragment_path_error 'unsafe entry' "$path"; return 1
    fi
    [ "$path" != changelog.d/README.md ] || continue
    changelog_fragment_path "$path" || return 1
    count=$((count + 1))
    [ "$count" -le 1024 ] || { changelog_fragment_error 'more than 1024 fragments'; return 1; }
    changelog_fragment_snapshot "$path" "$work/input" 65536 || return 1
    total=$((total + $(wc -c < "$work/input")))
    [ "$total" -le 4194304 ] || { changelog_fragment_error 'pending input exceeds 4194304 bytes'; return 1; }
  done
  # PR checks share the full-file and combined pending budget, without parsing
  # released history as entries or changing inherited legacy acceptance.
  if [ -f CHANGELOG.md ] && [ ! -L CHANGELOG.md ]; then
    changelog_fragment_snapshot CHANGELOG.md "$work/changelog" 16777216 || return 1
    bytes=$(LC_ALL=C awk '/^## \[Unreleased\]$/ { active=1; next } /^## \[/ { active=0 } active { n += length($0)+1 } END { print n+0 }' "$work/changelog")
    total=$((total + bytes))
    [ "$total" -le 4194304 ] || { changelog_fragment_error 'pending input exceeds 4194304 bytes'; return 1; }
  fi
)

# Sets CHANGELOG_FRAGMENTS in LC_ALL=C filename order, excluding the reserved README.
# Globbing (including dotfiles) preserves path boundaries without newline parsing.
changelog_fragment_inventory() {
  local work=${1:-} owned=0 rc=0
  if [ -z "$work" ]; then
    mkdir -p .uzi/scratch
    work=$(mktemp -d .uzi/scratch/changelog-inventory.XXXXXX) || return 1
    owned=1
  fi
  changelog_fragment_inventory_into "$work" || rc=$?
  if [ "$owned" -eq 1 ]; then rm -rf "$work"; fi
  return "$rc"
}

changelog_fragment_inventory_into() {
  local path count=0 total=0 work=${1:-} snapshot
  changelog_fragment_safe_entries || return 1
  CHANGELOG_FRAGMENTS=()
  CHANGELOG_FRAGMENT_INPUTS=()
  [ -d changelog.d ] || return 0
  for path in changelog.d/* changelog.d/.[!.]* changelog.d/..?*; do
    [ -e "$path" ] || [ -L "$path" ] || continue
    if [ -L "$path" ] || [ ! -f "$path" ]; then
      changelog_fragment_path_error 'unsafe entry' "$path"; return 1
    fi
    [ "$path" != changelog.d/README.md ] || continue
    snapshot="$work/fragment-$count"
    changelog_fragment_validate "$path" "$snapshot" || return 1
    total=$((total + $(wc -c < "$snapshot")))
    [ "$total" -le 4194304 ] || { changelog_fragment_error 'pending input exceeds 4194304 bytes'; return 1; }
    CHANGELOG_FRAGMENT_INPUTS+=("$snapshot")
    count=$((count + 1))
    [ "$count" -le 1024 ] || { changelog_fragment_error 'more than 1024 fragments'; return 1; }
    CHANGELOG_FRAGMENTS+=("$path")
  done
}
