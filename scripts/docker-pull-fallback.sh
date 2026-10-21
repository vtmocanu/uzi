#!/usr/bin/env bash
# Make a Docker Hub image available locally under its mirror.gcr.io name, pulling
# through the mirror first and falling back to Docker Hub.
#
#   docker-pull-fallback.sh [--print-ref] <dockerhub-ref>
#     e.g. postgres:17, moby/buildkit:buildx-stable-1
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
# Digest refs require --print-ref: print the registry-qualified ref actually pulled,
# without retagging (Docker cannot tag a digest-qualified target). All diagnostics
# go to stderr in this mode so callers can capture stdout and run --pull=never.
# Pulls get three attempts per registry, waiting 5 then 15 seconds between failures.
#
# Exit: 0 = image available (selected ref in digest mode, mirror name in tag mode);
# 1 = acquisition or retag failed; 2 = usage.
set -euo pipefail

usage() { echo "usage: $0 [--print-ref] <dockerhub-ref> (name[:tag][@sha256:<64hex>], no registry host; digest requires --print-ref)" >&2; exit 2; }
print_ref=0
if [ "${1:-}" = --print-ref ]; then
  print_ref=1
  shift
fi
[ "$#" -eq 1 ] || usage
ref=$1
case "$ref" in
  ''|-*|*[[:space:]]*) usage ;;
esac
digest=""
if [[ "$ref" == *@* ]]; then
  [ "$print_ref" -eq 1 ] || usage
  digest=${ref#*@}
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || usage
  ref=${ref%%@*}
  [[ "$ref" == *:* ]] || usage
fi
# Docker's rule: a first path component holding a dot or a colon, or equal to
# `localhost`, is a registry host, not a Docker Hub repo. Check it before any tag split.
case "$ref" in
  */*)
    case "${ref%%/*}" in
      *.*|*:*|localhost) usage ;;
    esac ;;
esac
# The tag is whatever follows the last colon (no host is left to hold one).
case "$ref" in
  *:*) name=${ref%:*}; tag=${ref##*:} ;;
  *)   name=$ref; tag=latest ;;
esac
[ -n "$name" ] && [ -n "$tag" ] || usage
case "$name" in
  */*) path=$name ;;
  *) path=library/$name ;;
esac
mirror_ref="mirror.gcr.io/$path:$tag"
hub_ref="docker.io/$path:$tag"
if [ -n "$digest" ]; then
  mirror_ref+="@$digest"
  hub_ref+="@$digest"
fi

pull() {
  local image=$1 delay
  for delay in 5 15; do
    if docker pull "$image" >&2; then return 0; fi
    echo "docker-pull-fallback: pull failed for $image; retrying in ${delay}s" >&2
    sleep "$delay"
  done
  docker pull "$image" >&2
}

success() {
  echo "docker-pull-fallback: $1 available locally" >&2
  if [ "$print_ref" -eq 1 ]; then printf '%s\n' "$1"; fi
  return 0
}

if pull "$mirror_ref"; then
  success "$mirror_ref"
  exit 0
fi
echo "docker-pull-fallback: mirror miss for $mirror_ref; falling back to Docker Hub ($hub_ref)" >&2
if pull "$hub_ref"; then
  if [ -n "$digest" ]; then
    success "$hub_ref"
    exit 0
  elif docker tag "$hub_ref" "$mirror_ref" >&2; then
    success "$mirror_ref"
    exit 0
  fi
fi
echo "docker-pull-fallback: could not make $ref available from mirror.gcr.io or Docker Hub" >&2
exit 1
