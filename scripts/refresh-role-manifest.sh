#!/usr/bin/env bash
# Refresh the vendored role manifest from the upstream skills library.
#
# PRD #85 vendors api/internal/agenttmpl/library/manifest.json: a distilled
# name->version snapshot of the 11 shipped builtin roles, plus the upstream
# commit it was distilled from. The drift test (TestBuiltinLibraryDrift) reddens
# when a builtin's version stamp falls behind that manifest. This script closes
# the other half of the loop (issue #601 item 2): it compares the manifest
# against a fresh upstream roles.yaml and rewrites it when a shipped role's
# version moved FORWARD, so the scheduled workflow can open a bump PR. The bump
# then reddens the drift test until a human ports the changed bodies.
#
# It NEVER adds an upstream-only role. Upstream ships 14 roles; uzi ships 11 by
# design, omitting release / tui-ux / skill-reviewer (PRD #85 M4). The manifest's
# roster is a deliberate product decision, so this bot only tracks forward version
# bumps for roles that already ship; a human adds a new builtin (and its manifest
# entry) by hand.
#
# Anything it cannot resolve with a forward bump (a shipped role gone from
# upstream, a non-integer version, or upstream moving BACKWARD) is reported as a
# WARNING so the workflow can surface it for a human, never silently applied.
#
# Parsing is a real YAML parse (yq -> JSON) fed to jq, NOT a line-oriented
# grep/awk: that is immune both to prompt_body prose that happens to start with a
# field name and to a cosmetic upstream re-indentation. yq (mikefarah) is
# preinstalled on GitHub-hosted runners; locally, `brew install yq`. jq is already
# a repo dependency.
#
# Usage (run from the repo root):
#   scripts/refresh-role-manifest.sh <path-to-roles.yaml> <upstream-sha>
#
# Rewrites the manifest in place when versions moved forward. Emits changed,
# warned, summary and warnings to $GITHUB_OUTPUT when set (consumed by the
# workflow), and prints the same to stderr. Exit codes: 0 ran (see the outputs),
# 2 a required input is missing, yq is absent, or the upstream file parses to no
# roles.
set -euo pipefail

roles_yaml="${1:?usage: refresh-role-manifest.sh <roles.yaml> <upstream-sha>}"
upstream_sha="${2:?upstream sha required}"
manifest="api/internal/agenttmpl/library/manifest.json"

[ -f "$roles_yaml" ] || { echo "roles.yaml not found: $roles_yaml" >&2; exit 2; }
[ -f "$manifest" ] || { echo "manifest not found: $manifest" >&2; exit 2; }
command -v yq >/dev/null 2>&1 || { echo "yq not found (preinstalled on GitHub runners; 'brew install yq' locally)" >&2; exit 2; }

# Upstream roles.yaml -> {name: version} via a real YAML parse.
upstream_json="$(yq -o=json '.roles' "$roles_yaml" | jq -c 'map({(.name): .version}) | add // {}')"
if [ "$(jq -n --argjson u "$upstream_json" '$u | length')" -eq 0 ]; then
  echo "parsed no roles from $roles_yaml; refusing to treat as an empty library" >&2
  exit 2
fi

manifest_roles="$(jq -c '.roles' "$manifest")"

# One pass over the manifest roster: forward bumps vs warnings. Iterating the
# MANIFEST (not upstream) is what keeps the 3 upstream-only roles out of scope.
plan="$(jq -cn --argjson up "$upstream_json" --argjson man "$manifest_roles" '
  reduce ($man | to_entries[]) as $e ({bumps: {}, warnings: []};
    ($up[$e.key]) as $u
    | if $u == null then
        .warnings += ["role \"\($e.key)\" (manifest v\($e.value)) is no longer in upstream roles.yaml; left unchanged for human review"]
      elif ($u | type) != "number" or ($u != ($u | floor)) then
        .warnings += ["role \"\($e.key)\" has a non-integer upstream version \($u | tostring); left unchanged for human review"]
      elif $u > $e.value then
        .bumps[$e.key] = {from: $e.value, to: $u}
      elif $u < $e.value then
        .warnings += ["role \"\($e.key)\" is v\($e.value) in the manifest but only v\($u) upstream (upstream moved backward?); left unchanged for human review"]
      else . end)')"

changed=false
warned=false
[ "$(jq -n --argjson p "$plan" '$p.bumps | length')" -gt 0 ] && changed=true
[ "$(jq -n --argjson p "$plan" '$p.warnings | length')" -gt 0 ] && warned=true

summary="$(jq -rn --argjson p "$plan" '$p.bumps | to_entries[] | "- \(.key): v\(.value.from) -> v\(.value.to)"')"
warnings="$(jq -rn --argjson p "$plan" '$p.warnings[] | "- WARNING: \(.)"')"

if [ "$changed" = true ]; then
  bumps_map="$(jq -cn --argjson p "$plan" '$p.bumps | map_values(.to)')"
  tmp="$(mktemp)"
  jq --argjson bumps "$bumps_map" \
     --arg sha "$upstream_sha" \
     --arg synced "$(date -u +%Y-%m-%d)" \
     '.roles += $bumps | .upstream_sha = $sha | .synced = $synced' \
     "$manifest" > "$tmp"
  mv "$tmp" "$manifest"
fi

{
  printf 'refresh: changed=%s warned=%s\n' "$changed" "$warned"
  [ -n "$summary" ] && printf '%s\n' "$summary"
  [ -n "$warnings" ] && printf '%s\n' "$warnings"
} >&2

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    printf 'changed=%s\n' "$changed"
    printf 'warned=%s\n' "$warned"
    printf 'summary<<REFRESH_EOF\n%s\nREFRESH_EOF\n' "$summary"
    printf 'warnings<<REFRESH_WARN_EOF\n%s\nREFRESH_WARN_EOF\n' "$warnings"
  } >> "$GITHUB_OUTPUT"
fi
