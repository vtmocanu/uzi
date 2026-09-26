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
#   4. The GitHub Release channel is correct. Stable: the Release is marked latest
#      (releases/latest tag_name == vX.Y.Z). RC (vX.Y.Z-rc.N): the Release is flagged
#      pre-release and releases/latest is unchanged (a lower stable, never the RC). Uses
#      the API, not `gh release view --json isLatest`, which errored on the installed gh
#      cutting v0.59.0 (release.md).
#   5. The released chart's worker pin (workers.image.tag, read from the tag's tree)
#      resolves on GHCR for each worker template image. A stable chart may pin an RC
#      worker (D11), but only a PUBLISHED one -- an unpublished pin ImagePullBackOffs
#      every new hosted worker (the 0.83.0-rc.7 incident).
#   6. The release.yml run's assert-agent-version job succeeded: every agent image
#      reports exactly the version it is tagged with, on a signed runtime base (#1720;
#      the #1682 re-tagged-RC mismatch). A release cut before #1720 has no such job and
#      FAILs this check by design.
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

# Release channel (PRD 1265 M3): a stable tag must be marked latest; an RC tag
# (vX.Y.Z-rc.N) must be flagged pre-release and must NOT be latest (latest stays the
# previous stable). Checks 1-3 (images, chart, cosign) are identical either way. The
# channel comes from the shared helper, so watch and verify never disagree.
ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || { echo "release-verify: not in a git checkout" >&2; exit 3; }
# shellcheck source=scripts/lib/release-mode.sh
. "$ROOT/scripts/lib/release-mode.sh"
MODE="$(release_mode "$VERSION")"

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
# img_has_tag <img> [wanted-tag] -> membership of <wanted-tag> (default $VERSION) among
# <img>'s GHCR tags. The optional second arg lets check 5 reuse this for the worker pin,
# which is NOT $VERSION (it may name an older published tag, or an RC inside a stable).
img_has_tag() {
  local img="$1" want="${2:-$VERSION}" enc tags rc
  enc="${REPO}%2F${img}"
  tags="$(gh api --paginate "/${PKG_OWNER_PATH}/packages/container/${enc}/versions" \
        --jq '.[].metadata.container.tags[]' 2>/dev/null)"; rc=$?
  [ "$rc" -ne 0 ] && return 2
  case $'\n'"$tags"$'\n' in (*$'\n'"$want"$'\n'*) return 0 ;; (*) return 1 ;; esac
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
  # count. Every release signs api/web/controller/chart and both agent release images (6);
  # a publish-agent job ALSO signs a runtime base when its input key was absent (#1720), so
  # a correct release signs 6 to 8. Match the two signing-step
  # names DIRECTLY ("Sign image (cosign keyless)" and "Package + push + sign chart").
  # Do NOT match on a bare "sign" and exclude "installer": the cosign install step is
  # named "Run ./.github/actions/install-cosign", which contains "cosign" (so it matches
  # "sign") but NOT "installer", so that exclusion missed it and double-counted every job
  # (12 vs 6, false FAIL — measured v0.75.0, 2026-09-02). Require the log's "Pushing
  # signature to:" lines to match. (No --paginate: a release run has far fewer than one
  # page of jobs.)
  expected="$(gh api "repos/${OWNER}/${REPO}/actions/runs/${RELRUN}/jobs" \
    --jq '[.jobs[].steps[] | select((.name|test("sign image|sign chart";"i")) and .conclusion=="success")] | length' 2>/dev/null)"
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

# --- 4. GitHub Release channel ------------------------------------------------
# Stable: releases/latest == the tag. RC: the tag's Release is prerelease AND
# releases/latest is unchanged (some LOWER stable, never the RC). releases/latest is the
# newest non-prerelease Release, so on an RC it correctly returns the previous stable.
latest="$(gh api "repos/${OWNER}/${REPO}/releases/latest" --jq '.tag_name' 2>/dev/null)"; latest_rc=$?
# Fail CLOSED on a lookup error, same as the GHCR image checks above. An empty $latest
# from a rate-limited or auth-failed query must NOT silently pass the RC check below
# (`"" != $TAG` would read as "latest unchanged" without ever confirming it). A 404 here
# means no stable release exists yet; the train always cuts above a prior stable, so that
# is not a normal RC state — re-run verify.
if [ "$latest_rc" -ne 0 ]; then
  fail "could not query releases/latest (gh/network error) — re-run verify"
elif [ "$MODE" = stable ]; then
  if [ "$latest" = "$TAG" ]; then pass "GitHub Release $TAG is marked latest"
  else fail "releases/latest is '${latest:-<none>}', expected $TAG"; fi
else
  pre="$(gh api "repos/${OWNER}/${REPO}/releases/tags/${TAG}" --jq '.prerelease' 2>/dev/null || true)"
  if [ "$pre" = "true" ]; then pass "GitHub Release $TAG is flagged pre-release"
  else fail "Release $TAG prerelease flag is '${pre:-<none>}', expected true (an RC must never be latest)"; fi
  if [ "$latest" != "$TAG" ]; then pass "releases/latest is '${latest:-<none>}' — unchanged, not the RC"
  else fail "releases/latest is the RC $TAG; an RC must never be marked latest"; fi
fi

# --- 5. the released chart's worker pin names a PUBLISHED image ----------------
# Read workers.image.tag from the RELEASED tree (the tag), not the working tree, so this
# proves exactly what shipped. A stable chart may legitimately pin an RC worker (D11) --
# but only a PUBLISHED one; an unpublished pin ImagePullBackOffs every new hosted worker
# (the 0.83.0-rc.7 incident, 2026-09-19). The pin reader is the same shape as
# scripts/worker-tag-autobump.sh's read_pin (workers.docker/controller image tags sit
# deeper and must not match). Worker template images: keep equal to
# api/internal/workertmpl.Names.
PIN="$(git show "$TAG:deploy/chart/values.yaml" 2>/dev/null | awk '
  /^workers:/ { inw=1; next }
  inw && /^[^[:space:]]/ { inw=0 }
  inw && /^  image:/ { inimg=1; next }
  inw && inimg && /^  [^[:space:]]/ { inimg=0 }
  inw && inimg && /^    tag:[[:space:]]/ { v=$2; gsub(/"/,"",v); print v; exit }
')"
if [ -z "$PIN" ]; then
  fail "could not read workers.image.tag from ${TAG}:deploy/chart/values.yaml (is the tag present locally? run: git fetch --tags origin)"
else
  for img in agent-base agent-jvm; do  # worker images agent-<name>; workertmpl.Names (pinned by template_sources_test.go)
    img_has_tag "$img" "$PIN"; r=$?
    case $r in
      0) pass "worker pin ${REPO}/${img}:${PIN} on GHCR" ;;
      2) fail "worker image ${REPO}/${img}: could not query GHCR (gh/network error) -- re-run verify" ;;
      *) fail "chart pins workers.image.tag=${PIN} but ${REPO}/${img}:${PIN} NOT on GHCR (unpublished worker image -- new hosted workers would ImagePullBackOff)" ;;
    esac
  done
fi

# --- 6. agent image identity gate ran green (#1720) ----------------------------
if [ -z "${RELRUN:-}" ]; then
  fail "no release.yml run found for $TAG (cannot prove the agent identity check ran)"
else
  concl="$(gh api "repos/${OWNER}/${REPO}/actions/runs/${RELRUN}/jobs" \
    --jq '[.jobs[] | select(.name == "assert-agent-version") | .conclusion] | first // empty' 2>/dev/null)"
  case "$concl" in
    success) pass "assert-agent-version green: agent images report the version they are tagged with (run $RELRUN)" ;;
    '') fail "no assert-agent-version job in release.yml run $RELRUN (a release cut before #1720?)" ;;
    *) fail "assert-agent-version concluded '$concl' in release.yml run $RELRUN -- an agent image does not report its tagged version" ;;
  esac
fi

echo
if [ "$fails" -eq 0 ]; then
  echo "=== $TAG: all publish checks PASSED ==="
  exit 0
fi
echo "=== $TAG: $fails check(s) FAILED ==="
exit 1
