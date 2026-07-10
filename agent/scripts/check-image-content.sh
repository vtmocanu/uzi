#!/usr/bin/env bash
# Image-content check for the baked uzi source (PRD #39 M2, Decision 5).
#
# Asserts, against the worker image's filesystem, the four content invariants the
# milestone names:
#   1. /opt/uzi-src/api exists  (the API source is baked in)
#   2. /opt/uzi-src/web exists  (the web source is baked in)
#   3. NO file matching .env* anywhere under /opt/uzi-src  (no secret leaked in)
#   4. NO inspiration/ under /opt/uzi-src  (the large submodules are excluded)
# plus /opt/uzi-src/BUILD_INFO is present.
#
# Two modes (the four invariants depend only on the bake COPY + the per-Dockerfile
# ignore, NOT on the heavy nix/devbox layers):
#   * light (default): reproduce `COPY . /opt/uzi-src` + the REAL per-template
#     Dockerfile.dockerignore on a busybox base. Same context + same ignore ⇒ the
#     /opt/uzi-src tree is byte-identical to the real image, but the build is a few
#     seconds and needs only the tiny busybox pull. This is the CI-cheap path.
#   * full (UZI_CHECK_MODE=full): build the actual template image. Faithful, but the
#     FIRST build pulls nix + devbox (minutes, network); later builds are cached.
#
# Usage:
#   agent/scripts/check-image-content.sh [template]        # light, default template=base
#   UZI_CHECK_MODE=full agent/scripts/check-image-content.sh jvm
set -euo pipefail

TEMPLATE="${1:-base}"
MODE="${UZI_CHECK_MODE:-light}"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TDIR="$REPO_ROOT/agent/templates/$TEMPLATE"
[ -f "$TDIR/Dockerfile" ] || { echo "no such template: $TEMPLATE (expected $TDIR/Dockerfile)"; exit 2; }
[ -f "$TDIR/Dockerfile.dockerignore" ] || { echo "missing $TDIR/Dockerfile.dockerignore"; exit 2; }

IMAGE="uzi-imgcheck-${TEMPLATE}-$$"
TMP=""
cleanup() {
  [ -n "$TMP" ] && rm -rf "$TMP"
  docker image rm -f "$IMAGE" >/dev/null 2>&1 || true
}
trap cleanup EXIT

if [ "$MODE" = "full" ]; then
  echo "== full build of the real $TEMPLATE image (first build pulls nix + devbox) =="
  DOCKERFILE="$TDIR/Dockerfile"
else
  echo "== light build: bake + real dockerignore on busybox ($TEMPLATE) =="
  TMP="$(mktemp -d)"
  cat > "$TMP/Dockerfile.check" <<'EOF'
# syntax=docker/dockerfile:1
FROM busybox:latest
ARG UZI_SRC_SHA=
COPY . /opt/uzi-src
RUN printf 'uzi source baked into the worker image (PRD #39 chat)\ncommit: %s\n' "${UZI_SRC_SHA:-unknown}" > /opt/uzi-src/BUILD_INFO
EOF
  # Faithfully apply the REAL per-Dockerfile ignore: BuildKit keys it to the
  # Dockerfile's name, so copy it beside the check Dockerfile under the matching name.
  cp "$TDIR/Dockerfile.dockerignore" "$TMP/Dockerfile.check.dockerignore"
  DOCKERFILE="$TMP/Dockerfile.check"
fi

DOCKER_BUILDKIT=1 docker build -f "$DOCKERFILE" -t "$IMAGE" "$REPO_ROOT" >/dev/null

# Enumerate the image filesystem (all paths, incl. dotfiles at any depth).
CID="$(docker create "$IMAGE")"
LISTING="$(docker export "$CID" | tar -tf -)"
docker rm -f "$CID" >/dev/null 2>&1 || true

fail=0
have() { printf '%s\n' "$LISTING" | grep -qE "$1"; }

if have '^opt/uzi-src/api/';      then echo "ok   /opt/uzi-src/api present";  else echo "FAIL /opt/uzi-src/api missing";  fail=1; fi
if have '^opt/uzi-src/web/';      then echo "ok   /opt/uzi-src/web present";  else echo "FAIL /opt/uzi-src/web missing";  fail=1; fi
if have '^opt/uzi-src/BUILD_INFO'; then echo "ok   /opt/uzi-src/BUILD_INFO present"; else echo "FAIL BUILD_INFO missing"; fail=1; fi

if have '(^|/)\.env'; then
  echo "FAIL a .env* file leaked into the baked source:"
  printf '%s\n' "$LISTING" | grep -E '(^|/)\.env' | sed 's/^/       /'
  fail=1
else
  echo "ok   no .env* anywhere under the baked source"
fi

if have '^opt/uzi-src/inspiration/'; then echo "FAIL inspiration/ leaked into the baked source"; fail=1; else echo "ok   no inspiration/ under the baked source"; fi

if [ "$fail" -ne 0 ]; then echo "IMAGE CONTENT CHECK FAILED"; exit 1; fi
echo "IMAGE CONTENT CHECK PASSED ($MODE, $TEMPLATE)"
