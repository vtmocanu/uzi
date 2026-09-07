#!/usr/bin/env bash
# PRD #1156 M1 — real base × jvm × amd64 × arm64 package matrix.
# Each cell builds the actual Dockerfile (therefore executing its positive runner-uid
# guard), then runs the same guard after three independent corruptions: checksum,
# missing code-mode host, and broken Codex executable. Every container is named outside
# the uzi- namespace and cleanup targets exact names only.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
LOCK="$REPO/agent/codex/codex-package.lock"
TIMEOUT="${UZI_M3A_MATRIX_TIMEOUT:-1800}"
read -r -a ARCHES <<< "${UZI_M3A_MATRIX_ARCHES:-amd64 arm64}"
read -r -a TEMPLATES <<< "${UZI_M3A_MATRIX_TEMPLATES:-base jvm}"
RUN_DIR="$(mktemp -d "${TMPDIR:-/tmp}/codex-m3a-matrix.XXXXXX")"
OWNED_CONTAINERS=()
OWNED_IMAGES=()
CELLS=0

cleanup() {
  for name in "${OWNED_CONTAINERS[@]}"; do docker rm -f "$name" >/dev/null 2>&1 || true; done
  for image in "${OWNED_IMAGES[@]}"; do docker image rm "$image" >/dev/null 2>&1 || true; done
  rm -rf "$RUN_DIR"
}
trap cleanup EXIT

lock_value() {
  local key=$1
  awk -F= -v key="$key" '$1 == key { print substr($0, index($0, "=") + 1); found=1; exit } END { if (!found) exit 1 }' "$LOCK"
}

for arch in "${ARCHES[@]}"; do
  case "$arch" in amd64|arm64) ;; *) echo "unsupported matrix arch: $arch" >&2; exit 2 ;; esac
  artifact="$(lock_value "CODEX_ARTIFACT_${arch}")"
  tag="$(lock_value CODEX_TAG)"
  base_url="$(lock_value CODEX_BASE_URL)"
  curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 \
    -o "$RUN_DIR/$artifact" "$base_url/$tag/$artifact"
  cp "$RUN_DIR/$artifact" "$RUN_DIR/$artifact.corrupt"
  printf 'corrupt-checksum-control\n' >> "$RUN_DIR/$artifact.corrupt"
done

run_negative() {
  local name=$1 image=$2 arch=$3 body=$4
  OWNED_CONTAINERS+=("$name")
  timeout "$TIMEOUT" docker run --rm --platform "linux/$arch" --name "$name" \
    --entrypoint /bin/sh -v "$REPO/agent/codex:/m3a:ro" "$image" -ceu "$body"
  docker rm -f "$name" >/dev/null 2>&1 || true
}

for template in "${TEMPLATES[@]}"; do
  case "$template" in base|jvm) ;; *) echo "unsupported matrix template: $template" >&2; exit 2 ;; esac
  for arch in "${ARCHES[@]}"; do
    image="codex-m3a-matrix-${template}-${arch}:$$"
    OWNED_IMAGES+=("$image")
    printf '\n==> build %s × linux/%s (positive guard runs in Dockerfile)\n' "$template" "$arch"
    timeout "$TIMEOUT" docker buildx build --load --platform "linux/$arch" \
      -f "$REPO/agent/templates/$template/Dockerfile" -t "$image" \
      --build-arg "UZI_SRC_SHA=$(git -C "$REPO" rev-parse HEAD)" "$REPO"

    artifact="$(lock_value "CODEX_ARTIFACT_${arch}")"
    checksum_name="codex-m3a-checksum-${template}-${arch}-$$"
    OWNED_CONTAINERS+=("$checksum_name")
    printf '==> negative checksum %s × %s\n' "$template" "$arch"
    timeout "$TIMEOUT" docker run --rm --platform "linux/$arch" --name "$checksum_name" \
      --entrypoint /bin/sh \
      -v "$REPO/agent/codex:/m3a:ro" \
      -v "$RUN_DIR/$artifact.corrupt:/artifact.tgz:ro" \
      "$image" -ceu 'if UZI_CODEX_ARTIFACT=/artifact.tgz UZI_CODEX_PREFIX=/tmp/codex-bad bash /m3a/install-codex.sh "$1"; then echo "corrupt checksum unexpectedly installed" >&2; exit 1; fi' sh "$arch"
    docker rm -f "$checksum_name" >/dev/null 2>&1 || true

    printf '==> negative missing host %s × %s\n' "$template" "$arch"
    run_negative "codex-m3a-host-${template}-${arch}-$$" "$image" "$arch" \
      'host=/opt/uzi-codex/0.153.2/bin/codex-code-mode-host; mv "$host" "$host.missing"; if bash /m3a/assert-codex.sh; then echo "missing host unexpectedly passed" >&2; exit 1; fi'

    printf '==> negative broken binary %s × %s\n' "$template" "$arch"
    run_negative "codex-m3a-binary-${template}-${arch}-$$" "$image" "$arch" \
      'codex=/opt/uzi-codex/0.153.2/bin/codex; printf "not-an-elf\n" > "$codex"; chmod 0755 "$codex"; if bash /m3a/assert-codex.sh; then echo "broken binary unexpectedly passed" >&2; exit 1; fi'

    printf 'PASS %s × linux/%s: positive + checksum + missing-host + broken-binary\n' "$template" "$arch"
    CELLS=$((CELLS + 1))
  done
done

printf '\nCodex package matrix passed: cells=%s templates=[%s] arches=[%s], three negatives per cell.\n' \
  "$CELLS" "${TEMPLATES[*]}" "${ARCHES[*]}"
