#!/usr/bin/env bash
# Make a Docker Hub image available locally under its mirror.gcr.io name, pulling
# through the mirror first and falling back to Docker Hub.
#
#   docker-pull-fallback.sh <dockerhub-ref>     e.g. postgres:17, moby/buildkit:buildx-stable-1
#
# CI pulls Docker Hub images through mirror.gcr.io (Google's public Docker Hub cache)
# because GitHub-hosted runners share Docker Hub's anonymous per-IP pull quota
# (toomanyrequests, 2026-10-09, PR #2621). The mirror is a best-effort cache: an
# image it has evicted fails to pull. This script covers that miss. It pulls
# mirror.gcr.io/<path>:<tag>; if that fails, it pulls the same ref from Docker Hub and
# tags it as mirror.gcr.io/<path>:<tag>, so the consumer keeps one name either way:
#   - run-store-it.sh (UZI_STORE_IT_PG_IMAGE) skips its own pull when the image is
#     present locally;
#   - buildx's docker-container driver always pulls its builder image, but uses a
#     local image of the same name when that pull fails (buildx driver.go, create()).
# A job's `services:` containers start before any step, so they cannot use this.
#
# Exit: 0 = image present under the mirror name; 1 = both pulls failed; 2 = usage.
set -euo pipefail

usage() { echo "usage: $0 <dockerhub-ref>   (name[:tag], no registry host, no @digest)" >&2; exit 2; }
[ "$#" -eq 1 ] || usage
ref=$1
case "$ref" in
  ''|*@*|*' '*) usage ;;
esac
name=${ref%%:*}
tag=${ref#"$name"}
tag=${tag#:}
[ -n "$tag" ] || tag=latest
# A first path component with a dot or colon is a registry host, not a Docker Hub repo.
case "${name%%/*}" in
  *.*|*:*) [ "$name" = "${name%%/*}" ] || usage ;;
esac
case "$name" in
  */*) path=$name ;;
  *) path=library/$name ;;
esac
mirror_ref="mirror.gcr.io/$path:$tag"
hub_ref="docker.io/$path:$tag"

if docker pull "$mirror_ref"; then
  echo "docker-pull-fallback: $mirror_ref pulled from the mirror"
  exit 0
fi
echo "docker-pull-fallback: mirror miss for $mirror_ref; falling back to Docker Hub ($hub_ref)" >&2
if docker pull "$hub_ref" && docker tag "$hub_ref" "$mirror_ref"; then
  echo "docker-pull-fallback: $mirror_ref tagged from Docker Hub"
  exit 0
fi
echo "docker-pull-fallback: could not pull $ref from mirror.gcr.io or Docker Hub" >&2
exit 1
