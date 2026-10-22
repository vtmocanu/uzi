#!/usr/bin/env bash
# Opt-in M1 production provisioning canary. No provider/model credential is used.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
cd "$repo"
scratch=.uzi/scratch
image="${UXLAB_RENDER_IMAGE:-ghcr.io/vtmocanu/uzi/agent-base:0.86.0-rc.18}"
action="${1:-canary}"
case "$action" in canary|build) ;; *) echo "usage: $0 {canary|build}" >&2; exit 2 ;; esac
mkdir -p "$scratch"
log="$(mktemp "$scratch/uxlab-render.XXXXXX")"
name="uxlab-render-$(date +%s)-$$"
volume="$name-nix"
owner="$name"
started=$SECONDS
mount_args=()
bind_ro() {
  local source="$1" target="$2"
  case "$source" in /*) ;; *) echo "ERROR: bind source must be absolute: $source" >&2; exit 2 ;; esac
  case "$source" in *,*) echo "ERROR: comma in bind source: $source" >&2; exit 2 ;; esac
  [ -e "$source" ] || { echo "ERROR: missing bind source: $source" >&2; exit 2; }
  mount_args+=(--mount "type=bind,source=$source,target=$target,readonly")
}
if [ "$action" = canary ]; then
  bind_ro "$here" /work/uxlab-render
  manifest="${UXLAB_RENDER_MANIFEST:-$repo/devbox.json}"
  [ -f "$manifest" ] || { echo "ERROR: root manifest must be a regular file" >&2; exit 2; }
  bind_ro "$manifest" /work/root-devbox.json
  # A regular exported source directory is accepted; no Git metadata is needed.
  source_dir="${UXLAB_RENDER_SRC_DIR:-$repo/agent/src}"
  [ -d "$source_dir" ] || { echo "ERROR: source must be an exported agent/src directory" >&2; exit 2; }
  bind_ro "$source_dir" /app/src
  if [ -n "${UXLAB_RENDER_BIN_DIR:-}" ]; then
    for bin in uzi-codex-supervisor uzi-codex-fileop uzi-codex-command-sandbox; do
      [ -f "$UXLAB_RENDER_BIN_DIR/$bin" ] && [ -x "$UXLAB_RENDER_BIN_DIR/$bin" ] ||
        { echo "ERROR: missing executable helper $UXLAB_RENDER_BIN_DIR/$bin" >&2; exit 2; }
      bind_ro "$UXLAB_RENDER_BIN_DIR/$bin" "/usr/local/bin/$bin"
    done
  fi
fi
if ! command -v docker >/dev/null 2>&1 || ! timeout 10s docker info >/dev/null 2>&1; then
  echo "SKIP: Docker CLI or daemon unavailable; LOG=$log"
  exit 77
fi
if [ "$action" = build ]; then
  rc=0
  DOCKER_BUILDKIT=1 timeout --foreground --kill-after=10s "${UXLAB_RENDER_BUILD_TIMEOUT:-1800}" \
    docker build -f "$repo/agent/templates/base/Dockerfile" -t "$image" \
    --build-arg "UZI_SRC_SHA=$(git -C "$repo" rev-parse HEAD)" "$repo" \
    2>&1 | head -c 8388608 > "$log" || rc=$?
  if [ "$(wc -c < "$log")" -ge 8388608 ]; then rc=1; fi
  echo "EXIT=$rc" >> "$log"
  tail -n 40 "$log"
  echo "LOG=$log"
  exit "$rc"
fi
if ! image_id="$(timeout 10s docker image inspect "$image" --format '{{.Id}}')"; then
  echo "SKIP: image absent: $image; LOG=$log"
  exit 77
fi
timeout 10s docker image inspect "$image_id" \
  --format 'IMAGE={{.Id}} LABELS={{json .Config.Labels}}' >> "$log"
printf 'SOURCE_DIR=%s\nHELPER_DIR=%s\nNETWORK=%s\n' "$source_dir" "${UXLAB_RENDER_BIN_DIR:-image}" "${UXLAB_RENDER_NETWORK:-bridge}" >> "$log"

# Only names reserved/created by this invocation are eligible for cleanup.
# Each cleanup has one bounded removal attempt and exact-name verification; one
# failure does not prevent attempting cleanup of the sibling object.
container_created=0
volume_created=0
# Called by the EXIT trap through on_exit.
# shellcheck disable=SC2329
cleanup() {
  local failed=0 found label
  if [ "$container_created" = 1 ]; then
    found="$(timeout 10s docker ps -a --filter "name=^/${name}$" --format '{{.Names}}')" || failed=1
    if [ -n "$found" ]; then
      label="$(timeout 10s docker inspect "$name" --format '{{index .Config.Labels "uxlab-render.owner"}}')" || failed=1
      if [ "$label" = "$owner" ]; then
        timeout --kill-after=2s 30s docker rm -f "$name" >> "$log" 2>&1 || failed=1
      else
        echo "ERROR: container ownership unverified: $name" >> "$log"; failed=1
      fi
    fi
    found="$(timeout 10s docker ps -a --filter "name=^/${name}$" --format '{{.Names}}')" || failed=1
    if [ -n "$found" ]; then failed=1; fi
  fi
  if [ "$volume_created" = 1 ]; then
    label="$(timeout 10s docker volume inspect "$volume" --format '{{index .Labels "uxlab-render.owner"}}')" || failed=1
    if [ "$label" = "$owner" ]; then
      timeout --kill-after=2s 10s docker volume rm "$volume" >> "$log" 2>&1 || failed=1
    else
      echo "ERROR: volume ownership unverified: $volume" >> "$log"; failed=1
    fi
    found="$(timeout 10s docker volume ls --filter "name=^${volume}$" --format '{{.Name}}')" || failed=1
    if [ -n "$found" ]; then failed=1; fi
  fi
  if [ "$failed" = 0 ]; then echo "CLEANUP=verified" >> "$log"; fi
  return "$failed"
}
# Registered as the EXIT trap below.
# shellcheck disable=SC2329
on_exit() {
  local rc=$?
  trap - EXIT INT TERM
  if ! cleanup; then
    echo "ERROR: cleanup unverified; inspect exact names $name and $volume" >> "$log"
    if [ "$rc" = 0 ] || [ "$rc" = 77 ]; then rc=3; fi
  fi
  echo "TOTAL_SECONDS=$((SECONDS - started))" >> "$log"
  echo "EXIT=$rc" >> "$log"
  tail -n 60 "$log"
  echo "LOG=$log"
  exit "$rc"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# Refuse collisions before volume create (which otherwise reuses an existing volume).
existing="$(timeout 10s docker volume ls --filter "name=^${volume}$" --format '{{.Name}}')"
[ -z "$existing" ] || { echo "ERROR: volume name collision" >&2; exit 2; }
volume_created=1
timeout 10s docker volume create --label "uxlab-render.owner=$owner" "$volume" >> "$log"
# Normal volume copy-up preserves the image's real Nix store; never tmpfs /nix.
# Network bridge is deliberately allowed: the real devbox install needs egress.
# Copy-up of a cold image store can take minutes. Track the attempted name even
# when the CLI times out, since daemon-side creation may already have started.
existing="$(timeout 10s docker ps -a --filter "name=^/${name}$" --format '{{.Names}}')"
[ -z "$existing" ] || { echo "ERROR: container name collision" >&2; exit 2; }
container_created=1
timeout --kill-after=10s "${UXLAB_RENDER_CREATE_TIMEOUT:-600}" docker create --name "$name" --label "uxlab-render.owner=$owner" \
  --network "${UXLAB_RENDER_NETWORK:-bridge}" --user 0:0 \
  --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges --tmpfs /data \
  --mount "type=volume,source=$volume,target=/nix" \
  --entrypoint /usr/local/sbin/uzi-entrypoint \
  "${mount_args[@]}" -e "UXLAB_RENDER_IMAGE_ID=$image_id" \
  "$image_id" /bin/sh -c 'cd /app && exec /usr/local/bin/node --import tsx /work/uxlab-render/fixture.ts' >> "$log"
rc=0
# Foreground timeout bounds the whole fixture; production install retries remain
# bounded by provision.ts. head caps combined output; reaching the cap is failure.
timeout --foreground --kill-after=10s "${UXLAB_RENDER_TIMEOUT:-1800}" \
  docker start --attach "$name" 2>&1 | head -c 8388608 >> "$log" || rc=$?
if [ "$(wc -c < "$log")" -ge 8388608 ]; then rc=1; fi
exit "$rc"
