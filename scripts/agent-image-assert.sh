#!/usr/bin/env bash
# agent-image-assert.sh -- prove a published agent release image IS the release it is tagged as (#1720).
#
# Usage: scripts/agent-image-assert.sh <template> <version> <short_sha> <commit_sha>
#   e.g. scripts/agent-image-assert.sh base 0.84.0 abc1234 abc1234<...40 hex>
#
# Reads the image config from the registry (no pull) and fails unless:
#   * :<version> and :<short_sha> resolve to the same digest;
#   * that digest carries a keyless cosign signature from this repo's release.yml;
#   * it is an index of exactly linux/amd64 + linux/arm64, each child signed the same way,
#     and each child passes every check below;
#   * exactly one UZI_AGENT_VERSION env entry, equal to `<version>+g<short_sha>` (the stamp
#     release.yml bakes; build metadata included, so an image built from the wrong commit
#     fails too);
#   * labels org.opencontainers.image.version == that stamp and .revision == <commit_sha>;
#   * io.github.vtmocanu.uzi.runtime-base names this template's runtime repo by full digest,
#     and that base is signed by the same workflow.
# This is the #1682 guard: a stable tag re-pointed at an RC's image reports the RC's stamp
# and fails here. BUILD_INFO is filesystem content, checked in the publish job right after
# the build; the revision label is its registry-side counterpart.
set -euo pipefail

[ "$#" -eq 4 ] || { echo "usage: $0 <template> <version> <short_sha> <commit_sha>" >&2; exit 2; }
template="$1" version="$2" short_sha="$3" commit="$4"
case "$template" in ''|*[!a-z0-9-]*) echo "bad template '$template'" >&2; exit 2;; esac
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]] || { echo "bad version '$version'" >&2; exit 2; }
[[ "$short_sha" =~ ^[0-9a-f]{7}$ ]] || { echo "bad short sha '$short_sha'" >&2; exit 2; }
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || { echo "bad commit '$commit'" >&2; exit 2; }
[ "${commit:0:7}" = "$short_sha" ] || { echo "short sha $short_sha is not a prefix of $commit" >&2; exit 2; }

repo="ghcr.io/vtmocanu/uzi/agent-${template}"
base_repo="ghcr.io/vtmocanu/uzi/agent-runtime-${template}"
identity_re='^https://github\.com/vtmocanu/uzi/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$'
issuer='https://token.actions.githubusercontent.com'
stamp="${version}+g${short_sha}"
fail=0
bad() { echo "FAIL ${repo}: $*" >&2; fail=1; }

d_version="$(docker buildx imagetools inspect "${repo}:${version}" --format '{{.Manifest.Digest}}')"
d_short="$(docker buildx imagetools inspect "${repo}:${short_sha}" --format '{{.Manifest.Digest}}')"
[[ "$d_version" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "FAIL ${repo}:${version}: no digest" >&2; exit 1; }
[ "$d_version" = "$d_short" ] || bad ":${version} ($d_version) and :${short_sha} ($d_short) differ"

cosign verify --certificate-identity-regexp "$identity_re" --certificate-oidc-issuer "$issuer" \
  "${repo}@${d_version}" >/dev/null || bad "release image ${d_version} is not signed by release.yml"

# The tag names a multi-arch index (publish-agent-index); every child must be the release.
children="$(docker buildx imagetools inspect "${repo}@${d_version}" --raw \
  | jq -r 'if .manifests then .manifests[] | "\(.platform.os)/\(.platform.architecture) \(.digest)" else empty end' | sort)"
[ "$(printf '%s\n' "$children" | cut -d' ' -f1 | tr '\n' ' ')" = 'linux/amd64 linux/arm64 ' ] \
  || { echo "FAIL ${repo}@${d_version}: want an index of exactly linux/amd64 + linux/arm64, got: $(printf '%s' "$children" | tr '\n' ' ')" >&2; exit 1; }

check_child() { # platform digest
  local ref="${repo}@$2" config stamps base_ref
  cosign verify --certificate-identity-regexp "$identity_re" --certificate-oidc-issuer "$issuer" \
    "$ref" >/dev/null || bad "$1 image $2 is not signed by release.yml"
  config="$(docker buildx imagetools inspect "$ref" --format '{{json .Image.Config}}')"
  stamps="$(printf '%s' "$config" | jq -r '[.Env[]? | select(startswith("UZI_AGENT_VERSION="))] | map(sub("^UZI_AGENT_VERSION=";"")) | .[]')"
  [ "$(printf '%s\n' "$stamps" | grep -c .)" -eq 1 ] || bad "$1: expected exactly one UZI_AGENT_VERSION, got: $(printf '%s' "$stamps" | tr '\n' ' ')"
  [ "$stamps" = "$stamp" ] || bad "$1: UZI_AGENT_VERSION='$stamps', want '$stamp'"

  label() { printf '%s' "$config" | jq -r --arg k "$1" '.Labels[$k] // ""'; }
  [ "$(label org.opencontainers.image.version)" = "$stamp" ] || bad "$1: version label '$(label org.opencontainers.image.version)', want '$stamp'"
  [ "$(label org.opencontainers.image.revision)" = "$commit" ] || bad "$1: revision label '$(label org.opencontainers.image.revision)', want '$commit'"

  base_ref="$(label io.github.vtmocanu.uzi.runtime-base)"
  if [[ "$base_ref" =~ ^${base_repo//./\\.}@sha256:[0-9a-f]{64}$ ]]; then
    cosign verify --certificate-identity-regexp "$identity_re" --certificate-oidc-issuer "$issuer" \
      "$base_ref" >/dev/null || bad "$1: runtime base $base_ref is not signed by release.yml"
  else
    bad "$1: runtime-base label '$base_ref' is not ${base_repo}@sha256:<digest>"
  fi
  bases+="$1 ${base_ref}; "
}

bases=""
while read -r platform digest; do check_child "$platform" "$digest"; done <<< "$children"

[ "$fail" -eq 0 ] || exit 1
echo "OK ${repo}:${version} @ ${d_version}: stamp ${stamp}, revision ${commit}, bases ${bases}"
