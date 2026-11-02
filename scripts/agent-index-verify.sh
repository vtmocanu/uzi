#!/usr/bin/env bash
# agent-index-verify.sh -- after release.yml's publish-agent-index runs `imagetools create`,
# prove :<version> and :<short_sha> both name ONE index whose children are exactly the
# per-arch digests this release pushed, and print that index digest (the one to sign).
#
# Usage: scripts/agent-index-verify.sh <repo> <version> <short_sha> <amd64-digest> <arm64-digest>
#
# GHCR's tag read-after-write can lag a push (issue #1102), and a tag that already existed can
# briefly still resolve to its previous index: :<short_sha> on a stable promoted from its RC's
# commit, or either tag when the publish-agent legs are rerun and push new digests. A tag whose
# index is not yet the expected one is retried with backoff, never failed on the first read.
# UZI_INDEX_DOCKER / UZI_INDEX_SLEEP accept executable paths for offline fixtures.
set -euo pipefail
[ "$#" -eq 5 ] || { echo "usage: $0 <repo> <version> <short_sha> <amd64-digest> <arm64-digest>" >&2; exit 2; }
repo="$1" version="$2" short_sha="$3" amd64="$4" arm64="$5"
for d in "$amd64" "$arm64"; do
  [[ "$d" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "bad digest '$d'" >&2; exit 2; }
done
docker_bin="${UZI_INDEX_DOCKER:-docker}"
sleep_bin="${UZI_INDEX_SLEEP:-sleep}"
want="$(printf 'linux/amd64=%s\nlinux/arm64=%s' "$amd64" "$arm64")"

children() {
  "$docker_bin" buildx imagetools inspect "$1" --raw \
    | jq -r '.manifests[]? | "\(.platform.os)/\(.platform.architecture)=\(.digest)"' | sort
}

index=""
for t in "$version" "$short_sha"; do
  got="" have="" ok=0
  for i in 1 2 3 4 5; do
    if got="$("$docker_bin" buildx imagetools inspect "${repo}:${t}" --format '{{.Manifest.Digest}}' 2>/dev/null)" \
       && [[ "$got" =~ ^sha256:[0-9a-f]{64}$ ]] \
       && have="$(children "${repo}@${got}" 2>/dev/null)" \
       && [ "$have" = "$want" ]; then
      ok=1; break
    fi
    [ "$i" -eq 5 ] || "$sleep_bin" "$((i * 2))"
  done
  [ "$ok" -eq 1 ] || { echo "${repo}:${t} never resolved to an index of exactly ${want//$'\n'/ } (last: ${got:-<not found>})" >&2; exit 1; }
  [ -z "$index" ] || [ "$got" = "$index" ] || { echo "${repo}: :${version} (${index}) and :${t} (${got}) differ" >&2; exit 1; }
  index="$got"
done
printf '%s\n' "$index"
