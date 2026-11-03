#!/usr/bin/env bash
# Print the pinned fleet's exact baked version, verified on immutable signed images.
# Only stdout is the stamp; registry/verifier diagnostics stay on stderr.
# UZI_ROLL_DOCKER / UZI_ROLL_COSIGN accept executable paths for offline fixtures.
set -euo pipefail
[ "$#" -eq 1 ] || { echo "usage: $0 <worker-pin>" >&2; exit 2; }
pin="$1"
[[ "$pin" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]] || { echo "invalid worker pin" >&2; exit 2; }
docker_bin="${UZI_ROLL_DOCKER:-docker}"
cosign_bin="${UZI_ROLL_COSIGN:-cosign}"
identity="https://github.com/vtmocanu/uzi/.github/workflows/release.yml@refs/tags/v${pin}"
stamp=''
for t in base jvm; do  # worker templates; workertmpl.Names (pinned by template_sources_test.go)
  repo="ghcr.io/vtmocanu/uzi/agent-${t}"
  digest="$("$docker_bin" buildx imagetools inspect "${repo}:${pin}" --format '{{.Manifest.Digest}}')" || {
    echo "ERROR: ${repo}:${pin} could not be resolved (unpublished pin or registry/auth error). A stable chart may pin an RC worker (D11), but only a PUBLISHED one; see .agents/skills/uzi-release" >&2
    exit 1
  }
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "invalid image digest: $repo" >&2; exit 1; }
  ref="${repo}@${digest}"
  "$cosign_bin" verify --certificate-identity "$identity" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com "$ref" >/dev/null || {
      echo "ERROR: $ref signature verification failed; expected identity $identity and GitHub Actions OIDC issuer" >&2
      exit 1
    }
  # A multi-arch pin names an index; its children are bound by digest under the verified
  # signature, and each must carry the same stamp. An older single-arch pin is its own child.
  children="$("$docker_bin" buildx imagetools inspect "$ref" --raw | jq -er 'if .manifests then .manifests[].digest else "" end')" || {
    echo "invalid image manifest: $ref" >&2; exit 1; }
  [ -n "$children" ] || children="$digest"
  for child in $children; do
    [[ "$child" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "invalid child digest: $ref" >&2; exit 1; }
    config="$("$docker_bin" buildx imagetools inspect "${repo}@${child}" --format '{{json .Image.Config}}')"
    current="$(printf '%s' "$config" | jq -er --arg pin "$pin" '
      . as $config
      |
      [.Env[]? | select(startswith("UZI_AGENT_VERSION=")) | ltrimstr("UZI_AGENT_VERSION=")] as $versions
      | (if ($versions | length) != 1 then error("expected one UZI_AGENT_VERSION") else $versions[0] end) as $stamp
      | ($stamp | ltrimstr($pin + "+g")) as $sha
      | if $stamp != ($pin + "+g" + $sha) or ($sha | length) != 7
           or ($sha | test("^[0-9a-f]+$") | not)
        then error("stamp does not match pin")
        elif $config.Labels["org.opencontainers.image.version"] != $stamp
        then error("image env/label mismatch") else $stamp end
    ')"
    [ -z "$stamp" ] || [ "$stamp" = "$current" ] || { echo "worker images disagree" >&2; exit 1; }
    stamp="$current"
  done
done
printf '%s\n' "$stamp"
