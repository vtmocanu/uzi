#!/usr/bin/env bash
# Compatibility entry point. One Python engine owns the publication retry ledger.
# release-watch.sh <X.Y.Z> [--max-reruns N] [--interval S] [--max-ticks N]
# Exit: 0 published, 1 refused/exhausted, 2 pending timeout, 3 usage/instrument.
# Gates stay manual except opt-in --recover-tag-smoke with pinned identity.
# Opt-in requires --journal/--operation/--repository/--sha/--kind/--tag-oid.
# After a failed Brew proof, wait for the same tag's release.yml to succeed,
# then use: gh run rerun <brew-run> --failed.
set -uo pipefail
ROOT="$(git -c core.fsmonitor=false rev-parse --show-toplevel 2>/dev/null)" || {
  echo 'release-watch: not in a git checkout' >&2
  exit 3
}
exec python3 -I "$ROOT/scripts/release-watch.py" "$@"
