#!/usr/bin/env bash
# builtin-parity.sh: fetch the upstream role library at the commit pinned in
# api/internal/agenttmpl/library/manifest.json and compare its published
# product-agents/ files with uzi's builtins (PRD #1849 M4). A NUDGE: exits 0 on
# any drift; exits 2 when it cannot fetch or read the upstream (never reads as
# parity). Needs network access to the upstream repo.
set -euo pipefail

root=$(git rev-parse --show-toplevel)
manifest="$root/api/internal/agenttmpl/library/manifest.json"
repo=$(jq -r '.upstream_repo' "$manifest")
sha=$(jq -r '.upstream_sha' "$manifest")
if [ -z "$repo" ] || [ "$repo" = null ] || [ -z "$sha" ] || [ "$sha" = null ]; then
  echo "builtin-parity: manifest lacks upstream_repo or upstream_sha" >&2
  exit 2
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if ! git -C "$tmp" init -q \
  || ! git -C "$tmp" fetch -q --depth=1 "https://$repo" "$sha" \
  || ! git -C "$tmp" checkout -q FETCH_HEAD -- product-agents; then
  echo "builtin-parity: could not fetch product-agents/ from $repo at $sha" >&2
  exit 2
fi

echo "upstream: $repo @ ${sha:0:12}"
cd "$root/api"
go run ./cmd/builtinparity -upstream "$tmp/product-agents" -builtins internal/agenttmpl/builtins
