#!/usr/bin/env bash
# Shared GHCR queries; sourcing this file only defines functions.

# Resolve checkout identity. Retain populated identity
# even on a failed gh call so callers can choose their compatibility policy.
ghcr_resolve_owner() {
  local identity identity_rc
  identity="$(gh repo view --json owner,name --jq '.owner.login + " " + .name' 2>/dev/null)"; identity_rc=$?
  OWNER=""; REPO=""
  read -r OWNER REPO <<< "$identity"
  [ "$identity_rc" -eq 0 ] && [ -n "$OWNER" ] && [ -n "$REPO" ]
}

# img_has_tag <img> [wanted-tag]: 0 present, 1 absent, 2 query error.
# Public OCI lookup: anonymous pull token, then an exact manifest HEAD.
img_has_tag() {
  local img="$1" want="${2:-${VERSION:-}}" name response token http_code
  name="$(printf '%s/%s/%s' "${OWNER:-}" "${REPO:-}" "$img" | LC_ALL=C tr '[:upper:]' '[:lower:]')"
  [[ "$name" =~ ^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*$ ]] || return 2
  [[ "$want" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]*$ ]] && [ "${#want}" -le 128 ] || return 2
  # -q must be first: personal curl configuration must not supply credentials.
  response="$(curl -q --silent --show-error --connect-timeout 5 --max-time 20 \
    --proto '=https' --max-filesize 65536 --get \
    --data-urlencode service=ghcr.io --data-urlencode "scope=repository:${name}:pull" \
    --write-out $'\n%{http_code}' https://ghcr.io/token)" || return 2
  [ "${response##*$'\n'}" = 200 ] && [ "${#response}" -le 65536 ] || return 2
  # Reject escapes before extracting quote-delimited token fields.
  case "$response" in *\\*) return 2 ;; esac
  token="$(printf '%s' "${response%$'\n'*}" | LC_ALL=C awk '
    { json = json $0 }
    END {
      if (json !~ /^[[:space:]]*\{/ || json !~ /\}[[:space:]]*$/) exit 1
      n = split(json, fields, "\"")
      for (i = 2; i + 2 <= n; i += 2) {
        if ((fields[i] == "token" || fields[i] == "access_token") && fields[i+1] ~ /^[[:space:]]*:[[:space:]]*$/) {
          value = fields[i+2]
          if (value !~ /^[A-Za-z0-9._~+\/-]+=*$/ || (seen && value != token)) exit 1
          token = value; seen++
        }
      }
      if (!seen) exit 1
      print token
    }
  ')" || return 2
  http_code="$(curl -q --silent --show-error --connect-timeout 5 --max-time 20 \
    --proto '=https' --head --output /dev/null --write-out '%{http_code}' \
    --header "Authorization: Bearer $token" \
    --header 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
    "https://ghcr.io/v2/${name}/manifests/${want}")" || return 2
  case "$http_code" in 200) return 0 ;; 404) return 1 ;; *) return 2 ;; esac
}

# ghcr_read_pin <rev>: first workers.image.tag in the revision's chart.
# Consume EOF before printing so large charts do not SIGPIPE git show.
# Callers enable pipefail to propagate git/awk read failures.
ghcr_read_pin() {
  git show "$1:deploy/chart/values.yaml" 2>/dev/null | awk '
    /^workers:/ { inw=1; next }
    inw && /^[^[:space:]]/ { inw=0 }
    inw && /^  image:/ { inimg=1; next }
    inw && inimg && /^  [^[:space:]]/ { inimg=0 }
    inw && inimg && /^    tag:[[:space:]]/ && !found { v=$2; gsub(/"/,"",v); found=1 }
    END { if (found) print v }
  '
}
