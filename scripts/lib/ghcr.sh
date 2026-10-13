#!/usr/bin/env bash
# Shared GHCR queries; sourcing this file only defines functions.

# Resolve checkout identity and the GHCR owner path. Retain populated identity
# even on a failed gh call so callers can choose their compatibility policy.
ghcr_resolve_owner() {
  local identity identity_rc kind_rc
  identity="$(gh repo view --json owner,name --jq '.owner.login + " " + .name' 2>/dev/null)"; identity_rc=$?
  OWNER=""; REPO=""; PKG_OWNER_PATH=""
  read -r OWNER REPO <<< "$identity"
  if [ -n "$OWNER" ] && [ -n "$REPO" ]; then
    OWNER_KIND="$(gh api "users/${OWNER}" --jq '.type' 2>/dev/null)"; kind_rc=$?
    # Historically unreadable type falls back to User; retain stdout semantics
    # even if gh returns a nonzero status alongside a type.
    : "$kind_rc"
    case "$OWNER_KIND" in
      Organization) PKG_OWNER_PATH="orgs/${OWNER}" ;;
      *)            PKG_OWNER_PATH="users/${OWNER}" ;;
    esac
  fi
  [ "$identity_rc" -eq 0 ] && [ -n "$OWNER" ] && [ -n "$REPO" ]
}

# img_has_tag <img> [wanted-tag]: 0 present, 1 absent, 2 query error.
# Paginate and match exact newline-delimited tags without an early pipe reader.
img_has_tag() {
  local img="$1" want="${2:-$VERSION}" enc tags rc
  enc="${REPO}%2F${img}"
  tags="$(gh api --paginate "/${PKG_OWNER_PATH}/packages/container/${enc}/versions" \
        --jq '.[].metadata.container.tags[]' 2>/dev/null)"; rc=$?
  [ "$rc" -ne 0 ] && return 2
  case $'\n'"$tags"$'\n' in (*$'\n'"$want"$'\n'*) return 0 ;; (*) return 1 ;; esac
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
