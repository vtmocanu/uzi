#!/usr/bin/env bash
# Refresh uzi's builtin roles and dev-team roster from the upstream skills library.
#
# Since PRD #1849 the 11 builtin subagent files in api/internal/agenttmpl/builtins/
# are byte-for-byte copies of the upstream library's product-agents/<role>.md at the
# commit api/internal/agenttmpl/library/manifest.json pins, and .claude/agents/ is
# this repo's dev-team roster synced from the same roles.yaml by the library's own
# sync.py. The weekly roles-manifest-refresh workflow checks out the latest STABLE
# upstream release and runs this script, which does the whole sync locally (#1851):
#
#   product half  copy every manifest role's product-agents file, move its version
#                 forward, pin upstream_sha to the release commit;
#   roster half   sync.py apply for every STALE or LEGACY dev-team agent, listing
#                 the body lines each apply dropped;
#   parity        byte-compare the builtins with upstream (api/cmd/builtinparity).
#
# ALL OR NOTHING. A manifest role missing upstream (in roles.yaml or product-agents/),
# a non-integer version, or a version that moved BACKWARD writes nothing at all and
# reports a warning: pinning a release while an old body stays would never reach
# parity. The roster is never widened: a library role uzi does not ship, or a
# dev-team agent the library lacks, is reported, never added or removed.
#
# Usage (from the repo root):
#   scripts/refresh-role-manifest.sh <upstream-checkout> <upstream-sha>
#   scripts/refresh-role-manifest.sh --latest-stable-tag <upstream-git-dir>
#
# The second form prints the newest vX.Y.Z tag (prereleases ignored), or exits 2.
#
# Outputs go to $GITHUB_OUTPUT when set and to stderr: changed, product_changed,
# roster_changed, warned, parity_ok, summary, warnings, roster_report, parity_report.
# Exit codes: 0 ran (read the outputs), 2 an input or tool is missing.
#
# Test seams (scripts/refresh-role-manifest-test.sh): ROLE_SYNC_PY overrides the
# upstream sync.py, ROLE_PARITY_CMD the parity command (run with the upstream
# product-agents dir as $1), ROLE_SYNC_ALLOWLIST the allowlist path.
set -euo pipefail

if [ "${1:-}" = "--latest-stable-tag" ]; then
  dir="${2:?usage: refresh-role-manifest.sh --latest-stable-tag <git-dir>}"
  tag="$(git -C "$dir" tag --list 'v*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n 1 || true)"
  [ -n "$tag" ] || { echo "no stable vX.Y.Z tag in $dir" >&2; exit 2; }
  printf '%s\n' "$tag"
  exit 0
fi

upstream="${1:?usage: refresh-role-manifest.sh <upstream-checkout> <upstream-sha>}"
upstream_sha="${2:?upstream sha required}"
roles_yaml="$upstream/skills/agent-kit/agent-team/roles.yaml"
product="$upstream/product-agents"
sync_py="${ROLE_SYNC_PY:-$upstream/skills/agent-kit/agent-team/scripts/sync.py}"
allowlist="${ROLE_SYNC_ALLOWLIST:-scripts/role-sync-allowlist.tsv}"
manifest="api/internal/agenttmpl/library/manifest.json"
# shellcheck disable=SC2016 # a literal Markdown code fence, not an expansion
fence='```'
builtins="api/internal/agenttmpl/builtins"
agents=".claude/agents"

[ -f "$roles_yaml" ] || { echo "roles.yaml not found: $roles_yaml" >&2; exit 2; }
[ -d "$product" ] || { echo "product-agents/ not found: $product" >&2; exit 2; }
[ -f "$manifest" ] || { echo "manifest not found: $manifest" >&2; exit 2; }
command -v yq >/dev/null 2>&1 || { echo "yq not found (preinstalled on GitHub runners; 'brew install yq' locally)" >&2; exit 2; }

# Upstream roles.yaml -> {name: version} via a real YAML parse.
upstream_json="$(yq -o=json '.roles' "$roles_yaml" | jq -c 'map({(.name): .version}) | add // {}')"
if [ "$(jq -n --argjson u "$upstream_json" '$u | length')" -eq 0 ]; then
  echo "parsed no roles from $roles_yaml; refusing to treat as an empty library" >&2
  exit 2
fi
manifest_roles="$(jq -c '.roles' "$manifest")"

# One pass over the MANIFEST roster (never upstream's), so upstream-only roles stay out.
plan="$(jq -cn --argjson up "$upstream_json" --argjson man "$manifest_roles" '
  reduce ($man | to_entries[]) as $e ({bumps: {}, warnings: []};
    ($up[$e.key]) as $u
    | if $u == null then
        .warnings += ["role \"\($e.key)\" (manifest v\($e.value)) is no longer in upstream roles.yaml"]
      elif ($u | type) != "number" or ($u != ($u | floor)) then
        .warnings += ["role \"\($e.key)\" has a non-integer upstream version \($u | tostring)"]
      elif $u > $e.value then
        .bumps[$e.key] = {from: $e.value, to: $u}
      elif $u < $e.value then
        .warnings += ["role \"\($e.key)\" is v\($e.value) in the manifest but only v\($u) upstream (moved backward?)"]
      else . end)')"
while IFS= read -r role; do
  [ -n "$role" ] || continue
  [ -f "$product/$role.md" ] || plan="$(jq -c --arg r "$role" '.warnings += ["role \"\($r)\" has no upstream product-agents/\($r).md"]' <<<"$plan")"
done < <(jq -r 'keys[]' <<<"$manifest_roles")

warnings="$(jq -r '.warnings[] | "- WARNING: \(.)"' <<<"$plan")"
product_changed=false
roster_changed=false
parity_ok=false
summary=""
roster_report=""
parity_report=""

if [ -n "$warnings" ]; then
  warned=true
  warnings="$(printf '%s\n- Nothing was copied or pinned: a partial sync would never reach parity. Fix the upstream release, or change the manifest by hand.' "$warnings")"
else
  warned=false

  # ---- product half ----------------------------------------------------------
  copied=""
  while IFS= read -r role; do
    [ -n "$role" ] || continue
    if ! cmp -s "$product/$role.md" "$builtins/$role.md"; then
      cp "$product/$role.md" "$builtins/$role.md"
      copied="$copied$role "
    fi
  done < <(jq -r 'keys[]' <<<"$manifest_roles")
  old_sha="$(jq -r '.upstream_sha' "$manifest")"
  bumps="$(jq -c '.bumps | map_values(.to)' <<<"$plan")"
  if [ -n "$copied" ] || [ "$bumps" != "{}" ] || [ "$old_sha" != "$upstream_sha" ]; then
    product_changed=true
    tmp="$(mktemp)"
    jq --argjson bumps "$bumps" --arg sha "$upstream_sha" --arg synced "$(date -u +%Y-%m-%d)" \
      '.roles += $bumps | .upstream_sha = $sha | .synced = $synced' "$manifest" > "$tmp"
    mv "$tmp" "$manifest"
    summary="$(jq -r '.bumps | to_entries[] | "- \(.key): v\(.value.from) -> v\(.value.to)"' <<<"$plan")"
    [ -n "$copied" ] && summary="$(printf '%s\n- bodies copied: %s' "$summary" "${copied% }")"
    [ "$old_sha" != "$upstream_sha" ] && summary="$(printf '%s\n- pin: %s -> %s' "$summary" "${old_sha:0:12}" "${upstream_sha:0:12}")"
    summary="${summary#$'\n'}"
  fi

  # ---- roster half -----------------------------------------------------------
  if [ -d "$agents" ] && [ -f "$sync_py" ]; then
    rc=0
    check_out="$(python3 "$sync_py" --library "$roles_yaml" --agents "$agents" check 2>&1)" || rc=$?
    if [ "$rc" -ge 2 ] || grep -qE '^[a-z0-9-]+ +.*[[:space:]](BAD-FM|ERROR)([[:space:]]|$)' <<<"$check_out"; then
      roster_report="$(printf 'Roster sync stopped: sync.py check reported an unreadable file or failed (exit %s):\n\n%s\n%s\n%s' "$rc" "$fence" "$check_out" "$fence")"
    else
      to_apply="$(awk '$0 ~ /[[:space:]](STALE|LEGACY)([[:space:]]|$)/ {print $1}' <<<"$check_out" | tr '\n' ' ')"
      unexpected=""
      while IFS= read -r line; do
        [ -n "$line" ] || continue
        role="$(awk '{print $1}' <<<"$line")"
        detail="$(sed -E 's/^.*[[:space:]]MODIFIED[[:space:]]+//' <<<"$line")"
        if [ -f "$allowlist" ] && grep -qxF "$(printf '%s\t%s' "$role" "$detail")" "$allowlist"; then
          continue
        fi
        unexpected="$(printf '%s\n- %s: %s' "$unexpected" "$role" "$detail")"
      done < <(grep -E '[[:space:]]MODIFIED([[:space:]]|$)' <<<"$check_out" || true)
      [ -n "$unexpected" ] && roster_report="$(printf 'Left unchanged (edited locally at the library version; review by hand):%s' "$unexpected")"
      extra="$(grep -E '[[:space:]]CUSTOM([[:space:]]|$)|in the library, no file here' <<<"$check_out" || true)"
      [ -n "$extra" ] && roster_report="$(printf '%s\n\nRoster differences reported, not changed:\n\n%s\n%s\n%s' "$roster_report" "$fence" "$extra" "$fence")"
      if [ -n "${to_apply// /}" ]; then
        # shellcheck disable=SC2086 # to_apply is a space-separated list of role names
        if python3 "$sync_py" --library "$roles_yaml" --agents "$agents" apply ${to_apply} >/dev/null 2>&1; then
          dropped=""
          for backup in "$agents"/*.md.pre-sync; do
            [ -e "$backup" ] || continue
            lines="$(diff "$backup" "${backup%.pre-sync}" | grep -E '^< ' | grep -vE '^< version: ' || true)"
            [ -n "$lines" ] && dropped="$(printf '%s\n%s:\n%s' "$dropped" "$(basename "${backup%.pre-sync}")" "$lines")"
            rm -f "$backup"
          done
          roster_changed=true
          applied="$(printf -- '- synced: %s' "${to_apply% }")"
          if [ -n "$dropped" ]; then
            applied="$(printf '%s\n\nBody lines the sync dropped (confirm each is superseded library text, not a uzi edit):\n\n%s\n%s\n%s' "$applied" "$fence" "${dropped#$'\n'}" "$fence")"
          fi
          roster_report="$(printf '%s\n\n%s' "$applied" "$roster_report")"
        else
          git checkout -q -- "$agents" 2>/dev/null || true
          rm -f "$agents"/*.md.pre-sync
          roster_report="$(printf 'Roster sync stopped: sync.py apply failed for %s; nothing changed.\n\n%s' "${to_apply% }" "$roster_report")"
        fi
      fi
    fi
  fi

  # ---- parity ----------------------------------------------------------------
  prc=0
  if [ -n "${ROLE_PARITY_CMD:-}" ]; then
    parity_report="$($ROLE_PARITY_CMD "$product" 2>&1)" || prc=$?
  else
    parity_report="$(cd api && go run ./cmd/builtinparity -upstream "$product" \
      -builtins internal/agenttmpl/builtins -manifest internal/agenttmpl/library/manifest.json 2>&1)" || prc=$?
  fi
  if [ "$prc" -eq 0 ] && grep -q '^builtins match upstream' <<<"$parity_report"; then
    parity_ok=true
  fi
fi

changed=false
{ [ "$product_changed" = true ] || [ "$roster_changed" = true ]; } && changed=true

{
  printf 'refresh: changed=%s product_changed=%s roster_changed=%s warned=%s parity_ok=%s\n' \
    "$changed" "$product_changed" "$roster_changed" "$warned" "$parity_ok"
  [ -n "$summary" ] && printf '%s\n' "$summary"
  [ -n "$warnings" ] && printf '%s\n' "$warnings"
  [ -n "$roster_report" ] && printf '%s\n' "$roster_report"
  [ -n "$parity_report" ] && printf 'parity: %s\n' "$parity_report"
} >&2

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    printf 'changed=%s\nproduct_changed=%s\nroster_changed=%s\nwarned=%s\nparity_ok=%s\n' \
      "$changed" "$product_changed" "$roster_changed" "$warned" "$parity_ok"
    printf 'summary<<REFRESH_EOF\n%s\nREFRESH_EOF\n' "$summary"
    printf 'warnings<<REFRESH_WARN_EOF\n%s\nREFRESH_WARN_EOF\n' "$warnings"
    printf 'roster_report<<REFRESH_ROSTER_EOF\n%s\nREFRESH_ROSTER_EOF\n' "$roster_report"
    printf 'parity_report<<REFRESH_PARITY_EOF\n%s\nREFRESH_PARITY_EOF\n' "$parity_report"
  } >> "$GITHUB_OUTPUT"
fi
