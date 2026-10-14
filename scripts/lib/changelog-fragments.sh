#!/usr/bin/env bash
# Shared filesystem and grammar checks. Caller runs at the repository root.
# Loops visit each supplied file once; the first error aborts the operation.
changelog_fragment_error() { printf 'changelog-fragments: %s\n' "$*" >&2; return 1; }

changelog_fragment_directory() {
  if [ -L changelog.d ] || { [ -e changelog.d ] && [ ! -d changelog.d ]; }; then
    changelog_fragment_error 'changelog.d must be a real directory'; return 1
  fi
}

changelog_fragment_validate() {
  local path=$1
  changelog_fragment_directory || return 1
  if [[ ! $path =~ ^changelog\.d/[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.md$ ]]; then
    changelog_fragment_error "invalid fragment path: $path"; return 1
  fi
  if [ -L "$path" ] || [ ! -f "$path" ]; then
    changelog_fragment_error "fragment must be a regular file: $path"; return 1
  fi
  awk -v mode=validate -f "$CHANGELOG_FRAGMENT_PARSER" "./$path"
}

# Git does not enumerate FIFOs or empty directories; inspect the directory too.
changelog_fragment_safe_entries() {
  local path
  changelog_fragment_directory || return 1
  [ -d changelog.d ] || return 0
  for path in changelog.d/* changelog.d/.[!.]* changelog.d/..?*; do
    [ -e "$path" ] || [ -L "$path" ] || continue
    if [ -L "$path" ] || [ ! -f "$path" ]; then
      changelog_fragment_error "unsafe entry: $path"; return 1
    fi
  done
}

# Sets CHANGELOG_FRAGMENTS in LC_ALL=C filename order, excluding the reserved README.
# Globbing (including dotfiles) preserves path boundaries without newline parsing.
changelog_fragment_inventory() {
  local path
  changelog_fragment_directory || return 1
  CHANGELOG_FRAGMENTS=()
  [ -d changelog.d ] || return 0
  for path in changelog.d/* changelog.d/.[!.]* changelog.d/..?*; do
    [ -e "$path" ] || [ -L "$path" ] || continue
    if [ -L "$path" ] || [ ! -f "$path" ]; then
      changelog_fragment_error "unsafe entry: $path"; return 1
    fi
    [ "$path" != changelog.d/README.md ] || continue
    changelog_fragment_validate "$path" || return 1
    CHANGELOG_FRAGMENTS+=("$path")
  done
}
