#!/usr/bin/env bash
# Read-only M2 seam (run from the target repository root):
#   validate [changelog.d/<uuid>.md ...]  validates explicit paths, or all pending.
#   list                               validates all; emits incorporated paths.
#   render-body <legacy-body-file>     emits grouped pending body to stdout.
#   render-unreleased <CHANGELOG-file> extracts only [Unreleased], then renders.
# list's newline output is safe because all filenames have passed the UUID grammar.
# Release cuts capture list/render output ephemerally and own consumption; this CLI
# never edits CHANGELOG, fragments, or a tracked manifest. Full-file mode does not
# emit the changelog preamble or released sections; the caller owns their splice.
set -euo pipefail
export LC_ALL=C
SCRIPT_ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
CHANGELOG_FRAGMENT_PARSER="$SCRIPT_ROOT/scripts/lib/changelog-fragments.awk"
# shellcheck source=scripts/lib/changelog-fragments.sh
. "$SCRIPT_ROOT/scripts/lib/changelog-fragments.sh"

usage() { echo 'usage: changelog-fragments.sh validate [paths...] | list | render-body file | render-unreleased file' >&2; exit 2; }
[ "$#" -gt 0 ] || usage
mode=$1
shift
case "$mode" in
  validate)
    if [ "$#" -gt 0 ]; then
      for path in "$@"; do changelog_fragment_validate "$path"; done
    else
      changelog_fragment_inventory
    fi
    ;;
  list)
    [ "$#" -eq 0 ] || usage
    changelog_fragment_inventory
    for path in "${CHANGELOG_FRAGMENTS[@]}"; do printf '%s\n' "$path"; done
    ;;
  render-body|render-unreleased)
    [ "$#" -eq 1 ] || usage
    [ ! -L "$1" ] && [ -f "$1" ] || { changelog_fragment_error 'legacy input must be a regular file'; exit 1; }
    changelog_fragment_no_nul "$1"
    changelog_fragment_inventory
    mkdir -p .uzi/scratch
    work="$(mktemp -d .uzi/scratch/changelog-render.XXXXXX)"
    trap 'rm -rf "$work"' EXIT
    if [ "$mode" = render-unreleased ]; then
      awk '
        /^## \[Unreleased\]$/ { seen++; active = 1; next }
        /^## \[/ { active = 0 }
        active { print }
        END { if (seen != 1) exit 1 }
      ' < "$1" > "$work/legacy" || { changelog_fragment_error 'expected exactly one [Unreleased] section'; exit 1; }
    else
      cp -- "$1" "$work/legacy"
    fi
    # Prefix operands so awk cannot interpret a filename as an assignment.
    inputs=("$work/legacy")
    for path in "${CHANGELOG_FRAGMENTS[@]}"; do inputs+=("./$path"); done
    awk -v legacy="$work/legacy" -f "$CHANGELOG_FRAGMENT_PARSER" "${inputs[@]}"
    ;;
  *) usage ;;
esac
