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

# GHCR packages live under /users/<login> for a User owner and /orgs/<login> for an
# Organization; picking the wrong one 404s and would report a published artifact as
# missing. Select by owner type so a fork under an org verifies too.
OWNER_KIND="$(gh api "users/${OWNER}" --jq '.type' 2>/dev/null)"
case "$OWNER_KIND" in
  Organization) PKG_OWNER_PATH="orgs/${OWNER}" ;;
  *)            PKG_OWNER_PATH="users/${OWNER}" ;;   # User (the default; also the fallback if type is unreadable)
esac

fails=0
pass() { printf '  PASS  %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; fails=$((fails+1)); }

echo "=== verifying $TAG on $OWNER/$REPO ==="

# --- 1. image version tags on GHCR --------------------------------------------
# A user's container package: /users/<owner>/packages/container/<repo>%2F<img>/versions
# rc 0 = $VERSION is among the image's tags; 1 = not found; 2 = query error (a
# transient gh/network blip must not be misreported as "not published"). --paginate
# walks every version page (the wanted tag is newest so usually page 1, but do not
# rely on it). Membership is tested in pure bash — no `printf | grep -q`, which can
# SIGPIPE-flake under pipefail (see assert-changelog-covers-release.sh).
img_has_tag() {
  local img="$1" enc tags rc
  enc="${REPO}%2F${img}"
  tags="$(gh api --paginate "/${PKG_OWNER_PATH}/packages/container/${enc}/versions" \
        --jq '.[].metadata.container.tags[]' 2>/dev/null)"; rc=$?
  [ "$rc" -ne 0 ] && return 2
  case $'\n'"$tags"$'\n' in (*$'\n'"$VERSION"$'\n'*) return 0 ;; (*) return 1 ;; esac
}
for img in api web controller agent-base agent-jvm; do
  img_has_tag "$img"; r=$?
  case $r in
    0) pass "image ${REPO}/${img}:${VERSION} on GHCR" ;;
    2) fail "image ${REPO}/${img}: could not query GHCR (gh/network error) — re-run verify" ;;
    *) fail "image ${REPO}/${img}:${VERSION} NOT found on GHCR" ;;
  esac
done

# --- 2. chart version tag on GHCR ---------------------------------------------
# The OCI Helm chart is <repo>/<repo> (ghcr.io/<owner>/<repo>/<repo>).
img_has_tag "$REPO"; r=$?
case $r in
  0) pass "chart ${REPO}/${REPO}:${VERSION} on GHCR" ;;
  2) fail "chart ${REPO}/${REPO}: could not query GHCR (gh/network error) — re-run verify" ;;
  *) fail "chart ${REPO}/${REPO}:${VERSION} NOT found on GHCR" ;;
esac

# --- 3. cosign signing lines in the release.yml run --------------------------
RELRUN="$(gh run list --workflow release.yml --branch "$TAG" --limit 1 \
          --json databaseId --jq '.[0].databaseId // empty' 2>/dev/null)"
if [ -z "$RELRUN" ]; then
  fail "no release.yml run found for $TAG (cannot prove signing)"
else
  # cosign prints "Pushing signature to: <ref>" once per signed artifact. Six
  # publish jobs sign (5 images + chart), so expect at least 6 lines.
  # Expected signatures = the signing steps that actually ran (success), NOT a hardcoded
  # 6. On an app-only release the publish-agent jobs re-tag the prior signed digest and
  # SKIP signing (release.yml gates the Sign step `if reuse != 'true'`), so a correct
  # release can sign as few as 4 (api/web/controller/chart). Match both signing-step
  # names ("Sign image (cosign keyless)" and "Package + push + sign chart") while
  # EXCLUDING the "cosign-installer" setup step, which also contains "sign". Require the
  # log's "Pushing signature to:" lines to match. (No --paginate: a release run has far
  # fewer than one page of jobs.)
  expected="$(gh api "repos/${OWNER}/${REPO}/actions/runs/${RELRUN}/jobs" \
    --jq '[.jobs[].steps[] | select((.name|test("sign";"i")) and ((.name|test("installer";"i"))|not) and .conclusion=="success")] | length' 2>/dev/null)"
  expected="${expected:-0}"
  sigs="$(gh run view "$RELRUN" --log 2>/dev/null | grep -cF 'Pushing signature to:')"
  sigs="${sigs:-0}"
  if [ "$expected" -ge 1 ] && [ "$sigs" -ge "$expected" ]; then
    pass "cosign signed $sigs artifacts ($expected Sign steps ran; release.yml run $RELRUN)"
  elif [ "$expected" -ge 1 ]; then
    fail "cosign: $sigs 'Pushing signature to:' lines but $expected Sign steps ran in run $RELRUN — signing incomplete"
  elif [ "$sigs" -ge 1 ]; then
    pass "cosign signed $sigs artifacts (release.yml run $RELRUN; Sign-step count unavailable)"
  else
    fail "no 'Pushing signature to:' lines and no successful Sign steps in run $RELRUN — signing unproven"
  fi
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
