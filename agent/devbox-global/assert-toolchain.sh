#!/usr/bin/env bash
# Fail-closed guard for the baked worker toolchain (replaces the hardcoded
# `command -v python3 && ...` line that PRD #92 M1 introduced in both Dockerfiles).
#
# WHY THIS EXISTS RATHER THAN A LONGER HARDCODED LINE. The old guard named five
# binaries and never changed again. devbox.json kept growing -- chromium, fontconfig
# and dejavu_fonts for PRD #87, openssl, then file/perl/coreutils/kubernetes-helm/
# kubeconform in 0.11.11 -- and none of them were ever added to it. So by 0.11.11 the
# guard covered 5 of 13 packages while reporting success, and `publish:agent` going
# green said nothing whatsoever about whether `helm` resolved. A guard that silently
# stops covering what it is supposed to cover is worse than no guard: it is a green
# light nobody re-derives. This script closes that by making COVERAGE ITSELF the
# thing that is checked.
#
# Run as: assert-toolchain.sh <toolchain-guard.tsv>
# Requires: devbox on PATH, HOME pointing at the global devbox config, and the
# toolchain already on PATH (i.e. run AFTER `devbox global install` and the
# /opt/uzi-toolchain symlink + ENV PATH).

set -euo pipefail

GUARD="${1:?usage: assert-toolchain.sh <toolchain-guard.tsv>}"
[ -r "$GUARD" ] || { echo "FAIL: cannot read guard map at $GUARD" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# --- 1. COVERAGE -------------------------------------------------------------
# `devbox global list` is the authority on what was installed. Parsing devbox.json
# here would mean parsing HuJSON (it carries `//` comments), and a comment-stripping
# regex over a file full of prose comments is its own bug factory. The list output is
# `* <package>@<version>` plus an unrelated "not activated" warning block, so match
# the starred lines strictly and ignore everything else.
devbox global list 2>/dev/null | sed -n 's/^\* \([^@]*\)@.*/\1/p' | sort > "$tmp/installed"
grep -v '^[[:space:]]*#' "$GUARD" | grep -v '^[[:space:]]*$' | awk -F'\t' '{print $1}' | sort > "$tmp/guarded"

if ! cmp -s "$tmp/installed" "$tmp/guarded"; then
  echo "FAIL: the toolchain guard map has drifted from devbox.json." >&2
  echo "" >&2
  if [ -s "$tmp/installed" ]; then
    unguarded="$(comm -23 "$tmp/installed" "$tmp/guarded")"
    [ -n "$unguarded" ] && { echo "  INSTALLED BUT NOT GUARDED (add a row to $(basename "$GUARD")):" >&2
                             echo "$unguarded" | sed 's/^/    /' >&2; }
  else
    echo "  (devbox global list returned nothing -- is HOME set to the global config?)" >&2
  fi
  stale="$(comm -13 "$tmp/installed" "$tmp/guarded")"
  [ -n "$stale" ] && { echo "  GUARDED BUT NOT INSTALLED (drop the row, or fix devbox.json):" >&2
                       echo "$stale" | sed 's/^/    /' >&2; }
  exit 1
fi

# --- 2. RESOLVE + SMOKE ------------------------------------------------------
# Read with IFS=tab so column 3 keeps its spaces (`helm version --short`).
failed=0
while IFS=$'\t' read -r pkg bin smoke; do
  case "$pkg" in ''|\#*) continue ;; esac
  [ "$bin" = "-" ] && continue

  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "FAIL: $pkg -> \`$bin\` does not resolve on PATH." >&2
    echo "      A nix package can install its libs and man pages while its CLI never" >&2
    echo "      appears -- check meta.outputsToInstall (this is the openssl.bin case)." >&2
    failed=1
    continue
  fi

  [ "$smoke" = "-" ] && continue
  if ! $smoke >/dev/null 2>&1; then
    echo "FAIL: $pkg -> \`$bin\` resolves but \`$smoke\` did not run cleanly." >&2
    failed=1
  fi
done < <(grep -v '^[[:space:]]*#' "$GUARD" | grep -v '^[[:space:]]*$')

[ "$failed" -eq 0 ] || exit 1

echo "OK: toolchain guard -- $(wc -l < "$tmp/guarded" | tr -d ' ') packages, all covered, all resolving."
