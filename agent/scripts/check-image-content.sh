#!/usr/bin/env bash
# Image-content check for the baked uzi source (PRD #39 M2, Decision 5).
#
# Asserts, against the worker image's filesystem, the invariants the milestone names:
#   1. /opt/uzi-src/api exists  (the API source is baked in)
#   2. /opt/uzi-src/web exists  (the web source is baked in)
#   3. NO file matching .env* anywhere under /opt/uzi-src  (no secret leaked in)
#   4. NO inspiration/ under /opt/uzi-src  (the large submodules are excluded)
#   5. /opt/uzi-src is entirely ROOT-OWNED (Decision 5 — the non-root agent can't write it)
#   6. read-only-to-agent: a write as the image's non-root user is DENIED
# plus /opt/uzi-src/BUILD_INFO is present.
# Invariants 5 + 6 are FULL-mode only (they need the real image's `uzi` user + USER
# switch; the light busybox reproduction runs as root and would false-pass).
#
# Every template (base, jvm, …) shares the COPY-as-root + USER-uzi invariant, so the
# full-mode ownership assertion must cover EACH — pass `all` (or list templates) so CI
# does not verify only the default.
#
# Two modes (the content invariants depend only on the bake COPY + the per-Dockerfile
# ignore, NOT on the heavy nix/devbox layers):
#   * light (default): reproduce `COPY . /opt/uzi-src` + the REAL per-template
#     Dockerfile.dockerignore on a busybox base. Same context + same ignore ⇒ the
#     /opt/uzi-src tree is byte-identical to the real image, but the build is a few
#     seconds and needs only the tiny busybox pull. This is the CI-cheap path.
#   * full (UZI_CHECK_MODE=full): build the actual template image. Faithful, but the
#     FIRST build pulls nix + devbox (minutes, network); later builds are cached.
#
# Usage:
#   agent/scripts/check-image-content.sh                     # light, template=base
#   agent/scripts/check-image-content.sh all                 # light, every template
#   UZI_CHECK_MODE=full agent/scripts/check-image-content.sh all      # ownership on base+jvm
#   UZI_CHECK_MODE=full agent/scripts/check-image-content.sh base jvm # explicit list
set -uo pipefail

MODE="${UZI_CHECK_MODE:-light}"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TEMPLATES_DIR="$REPO_ROOT/agent/templates"

# Resolve the template list: none → base (back-compat); `all` → every template dir; else
# the names given.
if [ "$#" -eq 0 ]; then
  TEMPLATES=(base)
elif [ "$1" = "all" ]; then
  TEMPLATES=()
  for d in "$TEMPLATES_DIR"/*/; do
    [ -f "${d}Dockerfile" ] && TEMPLATES+=("$(basename "$d")")
  done
else
  TEMPLATES=("$@")
fi

# Track built images + temp dirs so a trap can always clean up, even on a hard failure.
IMAGES=()
TMPS=()
cleanup() {
  local i
  for i in "${TMPS[@]:-}"; do [ -n "$i" ] && rm -rf "$i"; done
  for i in "${IMAGES[@]:-}"; do [ -n "$i" ] && docker image rm -f "$i" >/dev/null 2>&1 || true; done
}
trap cleanup EXIT

# have <regex> — grep the current $LISTING (a caller local; bash dynamic scoping).
have() { printf '%s\n' "$LISTING" | grep -qE "$1"; }

# check_one <template> — returns 0 pass / 1 fail.
check_one() {
  local TEMPLATE="$1"
  local TDIR="$TEMPLATES_DIR/$TEMPLATE"
  [ -f "$TDIR/Dockerfile" ] || { echo "no such template: $TEMPLATE (expected $TDIR/Dockerfile)"; return 1; }
  [ -f "$TDIR/Dockerfile.dockerignore" ] || { echo "missing $TDIR/Dockerfile.dockerignore"; return 1; }

  local IMAGE="uzi-imgcheck-${TEMPLATE}-$$"
  IMAGES+=("$IMAGE")
  local DOCKERFILE
  if [ "$MODE" = "full" ]; then
    echo "== full build of the real $TEMPLATE image (first build pulls nix + devbox) =="
    DOCKERFILE="$TDIR/Dockerfile"
  else
    echo "== light build: bake + real dockerignore on busybox ($TEMPLATE) =="
    local TMP
    TMP="$(mktemp -d)"
    TMPS+=("$TMP")
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

  if ! DOCKER_BUILDKIT=1 docker build -f "$DOCKERFILE" -t "$IMAGE" "$REPO_ROOT" >/dev/null; then
    echo "FAIL docker build failed for $TEMPLATE"
    return 1
  fi

  # Enumerate the baked source from the RUNNING container. `docker export "$CID" | tar`
  # returns an EMPTY stream for a BuildKit image carrying a provenance/attestation
  # manifest on this host (Docker 29) — so every presence check below would silently
  # misread as "missing" and full mode false-FAILs. Reading the live filesystem with
  # `find` is immune (busybox + alpine both ship find + sh). Paths are absolute
  # (/opt/uzi-src/…), and scoping the listing to /opt/uzi-src keeps the .env probe from
  # false-positiving on an OS/nix dotfile elsewhere in the real image.
  local LISTING
  LISTING="$(docker run --rm --entrypoint sh "$IMAGE" -c 'find /opt/uzi-src' 2>/dev/null || true)"

  local fail=0
  # find has no trailing slash on dirs, so match "<path>" or "<path>/…" via (/|$).
  if have '^/opt/uzi-src/api(/|$)';    then echo "ok   /opt/uzi-src/api present"; else echo "FAIL /opt/uzi-src/api missing"; fail=1; fi
  if have '^/opt/uzi-src/web(/|$)';    then echo "ok   /opt/uzi-src/web present"; else echo "FAIL /opt/uzi-src/web missing"; fail=1; fi
  if have '^/opt/uzi-src/BUILD_INFO$'; then echo "ok   /opt/uzi-src/BUILD_INFO present"; else echo "FAIL BUILD_INFO missing"; fail=1; fi

  # A .env* path component anywhere under /opt/uzi-src (the subtree scope is implicit —
  # the listing is only that subtree).
  if have '/\.env'; then
    echo "FAIL a .env* file leaked into the baked source:"
    printf '%s\n' "$LISTING" | grep -E '/\.env' | sed 's/^/       /'
    fail=1
  else
    echo "ok   no .env* anywhere under the baked source"
  fi

  if have '^/opt/uzi-src/inspiration(/|$)'; then echo "FAIL inspiration/ leaked into the baked source"; fail=1; else echo "ok   no inspiration/ under the baked source"; fi

  # Ownership + read-only-to-agent (Decision 5) — FULL MODE ONLY, and per template. The
  # light check builds its OWN busybox Dockerfile.check with no USER switch (runs as
  # root, no uzi user), so `find ! -user root` there is a false-pass and a write would
  # succeed — it asserts nothing. So gate both probes on full mode against the REAL image.
  if [ "$MODE" = "full" ]; then
    local NONROOT PROBE
    NONROOT="$(docker run --rm --entrypoint sh "$IMAGE" -c 'find /opt/uzi-src ! -user root 2>/dev/null' 2>/dev/null || true)"
    if [ -z "$NONROOT" ]; then
      echo "ok   /opt/uzi-src is entirely root-owned"
    else
      echo "FAIL non-root-owned paths under /opt/uzi-src:"
      printf '%s\n' "$NONROOT" | sed 's/^/       /'
      fail=1
    fi
    PROBE="$(docker run --rm --entrypoint sh "$IMAGE" -c 'touch /opt/uzi-src/UZI_WRITE_PROBE 2>/dev/null && echo WROTE || echo denied')"
    if [ "$PROBE" = "denied" ]; then
      echo "ok   the agent user cannot write /opt/uzi-src (read-only)"
    else
      echo "FAIL the agent user was able to write /opt/uzi-src (expected read-only)"
      fail=1
    fi
  else
    echo "note ownership/read-only assertion is full-mode only (light image has no non-root user);"
    echo "     run UZI_CHECK_MODE=full agent/scripts/check-image-content.sh all to verify base+jvm"
  fi

  if [ "$fail" -ne 0 ]; then echo "IMAGE CONTENT CHECK FAILED ($MODE, $TEMPLATE)"; return 1; fi
  echo "IMAGE CONTENT CHECK PASSED ($MODE, $TEMPLATE)"
  return 0
}

overall=0
for t in "${TEMPLATES[@]}"; do
  echo "=================== template: $t ==================="
  check_one "$t" || overall=1
done
exit "$overall"
