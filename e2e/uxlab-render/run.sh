#!/usr/bin/env bash
# Opt-in production provisioning/render proof. No provider/model credential is used.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
cd "$repo"
scratch=.uzi/scratch
image="${UXLAB_RENDER_IMAGE:-ghcr.io/vtmocanu/uzi/agent-base:0.86.0-rc.18}"
action="${1:-canary}"
case "$action" in
  matrix|local) exec bash "$here/matrix-run.sh" "$action" ;;
  canary|build|cell) ;;
  *) echo "usage: $0 {canary|matrix|local|build}" >&2; exit 2 ;;
esac
mkdir -p "$scratch"
log="$(mktemp "$scratch/uxlab-render.XXXXXX")"
printf 'RAW_LOG=%s/%s\n' "$repo" "$log"
name="uxlab-render-$(date +%s)-$$"
volume="$name-nix"
owner="$name"
started=$SECONDS
mount_args=()
# Raw logs remain evidence; terminal reports quote every row and control byte.
report_log() {
  tail -n "$1" "$log" | python3 -c 'import json, sys
for line in sys.stdin:
    text = line.rstrip("\n")
    print("FIXTURE_LOG=" + json.dumps(text[:2048], ensure_ascii=True) + (" [truncated]" if len(text) > 2048 else ""))'
}
entrypoint=/usr/local/sbin/uzi-entrypoint
bind_ro() {
  local source="$1" target="$2"
  case "$source" in /*) ;; *) echo "ERROR: bind source must be absolute: $source" >&2; exit 2 ;; esac
  case "$source" in *,*) echo "ERROR: comma in bind source: $source" >&2; exit 2 ;; esac
  [ -e "$source" ] || { echo "ERROR: missing bind source: $source" >&2; exit 2; }
  mount_args+=(--mount "type=bind,source=$source,target=$target,readonly")
}
if [ "$action" != build ]; then
  bind_ro "$here" /work/uxlab-render
  manifest="${UXLAB_RENDER_MANIFEST:-$repo/devbox.json}"
  [ -f "$manifest" ] || { echo "ERROR: root manifest must be a regular file" >&2; exit 2; }
  bind_ro "$manifest" /work/root-devbox.json
  if [ "$action" = cell ]; then
    case "${UXLAB_RENDER_CELL:-}/${UXLAB_RENDER_MODE:-}" in base/off|base/required|candidate/off|candidate/required|local/off) ;; *) echo "ERROR: invalid matrix cell" >&2; exit 2 ;; esac
    [ -d "${UXLAB_RENDER_REPO_DIR:-}" ] || { echo "ERROR: exported full repository required" >&2; exit 2; }
    bind_ro "$UXLAB_RENDER_REPO_DIR" /work/repository
    if [ "$UXLAB_RENDER_CELL" = base ]; then
      bind_ro "${UXLAB_RENDER_CANDIDATE_DIR:?}/api/cmd/uzi/uxlab/build.sh" /work/candidate-build.sh
    fi
    if [ -n "${UXLAB_RENDER_GO_DIR:-}" ]; then
      [ -x "$UXLAB_RENDER_GO_DIR/bin/go" ] && [ -d "$UXLAB_RENDER_GO_DIR/src" ] || { echo "ERROR: Go distro needs bin/go and src" >&2; exit 2; }
      bind_ro "$UXLAB_RENDER_GO_DIR" /usr/local/lib/uxlab-go
      entrypoint=/work/uxlab-render/go-entrypoint.sh
    fi
  fi
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
  if [ "$action" = cell ]; then exit 1; fi
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
  report_log 40
  echo "FIXTURE_EXIT=$rc"
  echo "LOG=$log"
  exit "$rc"
fi
if ! image_id="$(timeout 10s docker image inspect "$image" --format '{{.Id}}')"; then
  echo "SKIP: image absent: $image; LOG=$log"
  if [ "$action" = cell ]; then exit 1; fi
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
  report_log 60
  echo "FIXTURE_EXIT=$rc"
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
fixture=fixture.ts
if [ "$action" = cell ]; then fixture=matrix.ts; fi
# Disposable /data must execute worktree and generated Devbox scripts.
# Keep nosuid/nodev while opting into executable tmpfs, like a worktree volume.
creation_started=$SECONDS
timeout --kill-after=10s "${UXLAB_RENDER_CREATE_TIMEOUT:-600}" docker create --name "$name" --label "uxlab-render.owner=$owner" \
  --network "${UXLAB_RENDER_NETWORK:-bridge}" --user 0:0 \
  --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges --tmpfs /data:rw,exec,nosuid,nodev \
  --mount "type=volume,source=$volume,target=/nix" \
  --entrypoint "$entrypoint" \
  "${mount_args[@]}" -e "UXLAB_RENDER_IMAGE_ID=$image_id" \
  -e "UXLAB_RENDER_CELL=${UXLAB_RENDER_CELL:-}" -e "UXLAB_RENDER_MODE=${UXLAB_RENDER_MODE:-}" \
  "$image_id" /bin/sh -c 'cd /app && exec /usr/local/bin/node --import tsx "$1"' sh "/work/uxlab-render/$fixture" >> "$log"
echo "CREATION_SECONDS=$((SECONDS - creation_started))" >> "$log"
if [ "$action" = cell ]; then
  timeout 30s docker start "$name" >> "$log"
  handed_off=0
  cell_started=$SECONDS
  while :; do
    running="$(timeout 10s docker inspect "$name" --format '{{.State.Running}}')" ||
      { echo "ERROR: container running-state inspection failed" >> "$log"; exit 1; }
    case "$running" in
      true) ;;
      false) break ;;
      *) echo "ERROR: invalid container running state" >> "$log"; exit 1 ;;
    esac
    if [ "$handed_off" = 0 ] && [ "$UXLAB_RENDER_CELL" != local ] &&
      timeout 10s docker exec --user 10001:10002 "$name" test -f /data/uxlab-network-ready; then
      # No host-network mode: disconnect is the proof boundary, not GOPROXY alone.
      timeout 30s docker network disconnect "${UXLAB_RENDER_NETWORK:-bridge}" "$name" >> "$log" 2>&1
      test "$(timeout 10s docker inspect "$name" --format '{{len .NetworkSettings.Networks}}')" = 0
      timeout 10s docker exec --user 10001:10002 "$name" touch /data/uxlab-offline
      echo "NETWORK=disconnected" >> "$log"
      handed_off=1
    fi
    if [ "$((SECONDS - cell_started))" -ge "${UXLAB_RENDER_TIMEOUT:-3600}" ]; then
      echo "ERROR: cell wall deadline" >> "$log"
      # One best-effort snapshot before cleanup: 30s plus 5s kill grace, <=8MiB.
      timeout --kill-after=5s 30s docker logs "$name" 2>&1 | head -c 8388608 >> "$log" ||
        echo "ERROR: timeout log collection failed or output capped" >> "$log"
      exit 1
    fi
    sleep 1
  done
  timeout 30s docker logs "$name" 2>&1 | head -c 8388608 >> "$log"
  [ "$(wc -c < "$log")" -lt 8388608 ] || { echo "ERROR: output cap" >> "$log"; exit 1; }
  rc="$(timeout 10s docker inspect "$name" --format '{{.State.ExitCode}}')"
  # Rendering evidence can be exported even when a separate boundary criterion fails.
  # Exit status remains nonzero; only the fixture completion marker permits this arm.
  if [ "$UXLAB_RENDER_CELL" != base ] && { [ "$rc" = 0 ] ||
    { [ "$UXLAB_RENDER_CELL" = candidate ] && grep -q '^{"kind":"rendering-complete",' "$log"; }; }; then
    # pipefail requires BOTH the bounded decoder and docker cp producer to pass.
    if ! artifact_path="$(timeout --kill-after=5s 30s docker cp "$name:/data/uxlab-artifacts" - |
      python3 "$here/export.py" receipt 2>> "$log")"; then
      echo "ERROR: receipt export failed" >> "$log"; exit 1
    fi
    artifacts="$(mktemp -d "$scratch/uxlab-png-${UXLAB_RENDER_CELL}-${UXLAB_RENDER_MODE}.XXXXXX")"
    if ! timeout --kill-after=5s 120s docker cp "$name:$artifact_path/." - |
      python3 "$here/export.py" "$UXLAB_RENDER_CELL" "$log" "$artifacts" fresh-owned-dir 2>> "$log"; then
      rm -rf -- "$artifacts"
      echo "ERROR: PNG export failed" >> "$log"; exit 1
    fi
    echo "PNG_EXPORT=$repo/$artifacts" >> "$log"
  fi
  exit "$rc"
fi
rc=0
# Foreground timeout bounds the whole fixture; production install retries remain
# bounded by provision.ts. head caps combined output; reaching the cap is failure.
timeout --foreground --kill-after=10s "${UXLAB_RENDER_TIMEOUT:-1800}" \
  docker start --attach "$name" 2>&1 | head -c 8388608 >> "$log" || rc=$?
if [ "$(wc -c < "$log")" -ge 8388608 ]; then rc=1; fi
exit "$rc"
