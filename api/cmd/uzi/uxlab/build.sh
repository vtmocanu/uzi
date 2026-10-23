#!/usr/bin/env bash
# Generate and render uxlab frames, then verify every screenshot is fresh.
set -euo pipefail

if ! command -v freeze >/dev/null 2>&1; then
  echo 'uxlab: freeze is missing. Check that devbox opt-in is enabled for this repo in uzi and that run provisioning reported no package warnings, or run devbox run build locally.' >&2
  exit 1
fi

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"
# Drop prior frames so a generator that writes nothing cannot pass on stale ones.
rm -f frames/*.ansi
./gen.sh
./render.sh

shopt -s nullglob
frames=(frames/*.ansi)
if (( ${#frames[@]} == 0 )); then
  echo 'uxlab: no ANSI frames generated' >&2
  exit 1
fi
# One pass over the generated inventory; any failed frame rejects the build.
for frame in "${frames[@]}"; do
  png="png/${frame##*/}"
  png="${png%.ansi}.png"
  if [[ ! -f "$png" || ! -s "$png" || ! "$png" -nt "$frame" ]]; then
    echo "uxlab: missing, empty or stale PNG for $frame" >&2
    exit 1
  fi
done
echo "uxlab: verified ${#frames[@]} fresh PNG(s)"
