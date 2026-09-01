#!/usr/bin/env bash
# release-verify.sh — PROVE a published release, the deterministic form of the
# publish-proof checks in .claude/agents/release.md. Run it AFTER release.yml and
# brew.yml are green (release-watch.sh exits 0), to confirm the artifacts are
# actually live rather than trusting that the run "said success".
#
#   release-verify.sh <X.Y.Z>
#
# <X.Y.Z> is the release version (leading v optional). Owner/repo are derived from
# the checkout via `gh`, so a fork under any owner works unedited.
#
# Checks (each prints PASS/FAIL; the script exits nonzero if any FAILs):
#   1. GHCR image version tag present on all five images (api, web, controller,
#      agent-base, agent-jvm).
#   2. GHCR chart version tag present (the OCI chart <repo>/<repo>).
#   3. cosign signing happened: the release.yml run's publish jobs each logged a
#      "Pushing signature to:" line. cosign 3.x signs via the OCI referrers API,
#      NOT as a `sha256-<digest>.sig` TAG — so we prove signing from the JOB LOG,
#      never by grepping the tag list for `.sig` (that reads empty on a correctly
#      signed image; see release.md).
#   4. The GitHub Release exists and is marked latest (tag_name == vX.Y.Z). Uses
#      the API, not `gh release view --json isLatest`, which errored on the
#      installed gh cutting v0.59.0 (release.md).
#
# Exit codes:
#   0  every check passed
#   1  a check FAILed (which one is printed)
#   3  usage / gh error
#
# Design: literals are matched with `grep -F` and fields parsed with awk, never a
# bare grep pattern — this host's grep is ugrep, whose POSIX modes mishandle
# negated classes and brace intervals (repo CLAUDE.md).
set -uo pipefail

VERSION="${1:-}"
if [ -z "$VERSION" ] || [ "$VERSION" = "-h" ] || [ "$VERSION" = "--help" ]; then
  sed -n '2,26p' "$0"; exit 3
fi
VERSION="${VERSION#v}"          # normalize: accept v0.74.0 or 0.74.0
TAG="v$VERSION"

# Owner/repo from the checkout so a fork works unedited.
read -r OWNER REPO < <(gh repo view --json owner,name --jq '.owner.login + " " + .name' 2>/dev/null)
if [ -z "${OWNER:-}" ] || [ -z "${REPO:-}" ]; then
  echo "release-verify: could not resolve owner/repo via gh (are you in the checkout, authed?)" >&2
  exit 3
fi

fails=0
pass() { printf '  PASS  %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; fails=$((fails+1)); }

echo "=== verifying $TAG on $OWNER/$REPO ==="

# --- 1. image version tags on GHCR --------------------------------------------
# A user's container package: /users/<owner>/packages/container/<repo>%2F<img>/versions
img_has_tag() {   # $1=image short name -> rc 0 if $VERSION is among its tags
  local img="$1" enc out
  enc="${REPO}%2F${img}"
  out="$(gh api "/users/${OWNER}/packages/container/${enc}/versions" \
        --jq "[.[].metadata.container.tags[]] | map(select(. == \"${VERSION}\")) | length" 2>/dev/null)"
  [ "${out:-0}" -gt 0 ] 2>/dev/null
}
for img in api web controller agent-base agent-jvm; do
  if img_has_tag "$img"; then pass "image ${REPO}/${img}:${VERSION} on GHCR"
  else fail "image ${REPO}/${img}:${VERSION} NOT found on GHCR"; fi
done

# --- 2. chart version tag on GHCR ---------------------------------------------
# The OCI Helm chart is <repo>/<repo> (ghcr.io/<owner>/<repo>/<repo>).
if img_has_tag "$REPO"; then pass "chart ${REPO}/${REPO}:${VERSION} on GHCR"
else fail "chart ${REPO}/${REPO}:${VERSION} NOT found on GHCR"; fi

# --- 3. cosign signing lines in the release.yml run --------------------------
RELRUN="$(gh run list --workflow release.yml --branch "$TAG" --limit 1 \
          --json databaseId --jq '.[0].databaseId // empty' 2>/dev/null)"
if [ -z "$RELRUN" ]; then
  fail "no release.yml run found for $TAG (cannot prove signing)"
else
  # cosign prints "Pushing signature to: <ref>" once per signed artifact. Six
  # publish jobs sign (5 images + chart), so expect at least 6 lines.
  sigs="$(gh run view "$RELRUN" --log 2>/dev/null | grep -cF 'Pushing signature to:')"
  sigs="${sigs:-0}"
  if [ "$sigs" -ge 6 ]; then pass "cosign signed $sigs artifacts (release.yml run $RELRUN)"
  elif [ "$sigs" -gt 0 ]; then fail "cosign signed only $sigs artifacts (expected >=6) in run $RELRUN"
  else fail "no 'Pushing signature to:' lines in release.yml run $RELRUN — signing unproven"; fi
fi

# --- 4. GitHub Release exists and is latest ----------------------------------
latest="$(gh api "repos/${OWNER}/${REPO}/releases/latest" --jq '.tag_name' 2>/dev/null || true)"
if [ "$latest" = "$TAG" ]; then pass "GitHub Release $TAG is marked latest"
else fail "releases/latest is '${latest:-<none>}', expected $TAG"; fi

echo
if [ "$fails" -eq 0 ]; then
  echo "=== $TAG: all publish checks PASSED ==="
  exit 0
fi
echo "=== $TAG: $fails check(s) FAILED ==="
exit 1
