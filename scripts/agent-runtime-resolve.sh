#!/usr/bin/env bash
# agent-runtime-resolve.sh -- decide whether a release can REUSE a published runtime base (#1720).
#
# Usage: scripts/agent-runtime-resolve.sh <template> <key> <platform>
# Prints GITHUB_OUTPUT lines on stdout:
#   found=true  + digest=sha256:...   a signed base with exactly this key exists: reuse it
#   found=false                       the registry CONFIRMED the tag is absent: build it
# Exits nonzero on everything else, so an auth/network/registry failure can never be read
# as "absent" (which would rebuild) or as "present" (which would reuse something unproven).
#
# Reuse requires, on the RESOLVED DIGEST (the one the release build then consumes):
#   * a keyless cosign signature from this repo's release.yml on a v* tag;
#   * labels naming exactly this key, template and platform (the signature proves who
#     published it, the labels prove it was published FOR this key).
# Absence is only the exact registry answer `ERROR: <ref>: not found`. Measured 2026-09-26:
# an AUTHENTICATED lookup returns that line for a missing tag AND for a repository that does
# not exist yet (the first publish); an anonymous lookup of a missing repository returns
# 403, which must fail, so run this after `docker login`.
set -euo pipefail

[ "$#" -eq 3 ] || { echo "usage: $0 <template> <key> <platform>" >&2; exit 2; }
template="$1" key="$2" platform="$3"
case "$template" in ''|*[!a-z0-9-]*) echo "bad template '$template'" >&2; exit 2;; esac
[[ "$key" =~ ^v[0-9]+-[0-9a-f]{32}$ ]] || { echo "bad key '$key'" >&2; exit 2; }

repo="ghcr.io/vtmocanu/uzi/agent-runtime-${template}"
ref="${repo}:${key}"
identity_re='^https://github\.com/vtmocanu/uzi/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$'
issuer='https://token.actions.githubusercontent.com'

errf="$(mktemp)"
trap 'rm -f "$errf"' EXIT

if digest="$(docker buildx imagetools inspect "$ref" --format '{{.Manifest.Digest}}' 2>"$errf")"; then
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "unexpected digest '$digest' for $ref" >&2; exit 1; }
  echo "found $ref -> $digest; verifying signature and labels" >&2
  cosign verify --certificate-identity-regexp "$identity_re" --certificate-oidc-issuer "$issuer" \
    "${repo}@${digest}" >/dev/null
  labels="$(docker buildx imagetools inspect "${repo}@${digest}" --format '{{json .Image.Config.Labels}}')"
  got="$(printf '%s' "$labels" | jq -r '[."io.github.vtmocanu.uzi.runtime-key", ."io.github.vtmocanu.uzi.runtime-template", ."io.github.vtmocanu.uzi.runtime-platform"] | @tsv')"
  want="$(printf '%s\t%s\t%s' "$key" "$template" "$platform")"
  [ "$got" = "$want" ] || { echo "label mismatch on ${repo}@${digest}: got '$got', want '$want'" >&2; exit 1; }
  printf 'found=true\ndigest=%s\n' "$digest"
elif [ "$(cat "$errf")" = "ERROR: ${ref}: not found" ]; then
  echo "absent: $ref (registry confirmed not found); it will be built" >&2
  echo "found=false"
else
  cat "$errf" >&2
  echo "could not resolve $ref (not a confirmed absence); failing closed" >&2
  exit 1
fi
