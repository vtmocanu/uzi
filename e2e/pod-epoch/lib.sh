#!/usr/bin/env bash
# Controlled-daemon harness for PRD #1860 M0 real-daemon experiments.
# Sourced, never executed. Modelled on e2e/dind-recycle/run.sh.
#
# Safety: the outer daemon is shared. This library only creates and removes
# containers and volumes named pe1860-<posture>-<token>[-suffix] that carry the
# label pe1860.owner=<token>, and removes by exact name after verifying that
# label. It never prunes, lists-and-removes or globs.

# shellcheck disable=SC2034  # variables are consumed by sourcing scripts
# Pins match controller/internal/kube/render_dind.go and deploy/chart/values.yaml.
readonly PE_ROOTLESS_IMAGE='docker:29-dind-rootless@sha256:e17fa54c2ffd511d8407c746eec77f7814e6f74fe20caf822dad1870599984c0'
readonly PE_ROOTFUL_IMAGE='docker:29-dind@sha256:5efed980cba3fc126cf54e21a5a6ff8849d05b6e0623d6e7612f48e9cd6cd17e'
readonly PE_FIXTURE_IMAGE='alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6'
readonly PE_LABEL_KEY='pe1860.owner'
readonly PE_PATH='/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
readonly PE_RUN_DIR='/data/runner'

PE_POSTURE=''
PE_TOKEN=''
PE_IMAGE=''
PE_DATA_ROOT=''
PE_ENDPOINT=''
PE_DAEMON=''
PE_DATA_VOL=''
PE_RUN_VOL=''
PE_DEADLINE=0
PE_CONTAINERS=()
PE_VOLUMES=()
PE_EVENTS_PID=''
PE_EVENTS_RC=''

pe_fail() { echo "FAIL: $*" >&2; exit 1; }

# pe_outer <cap-seconds> <docker args...>: one capped call within the shared deadline.
pe_outer() {
  local cap=$1 remaining=$((PE_DEADLINE - SECONDS))
  shift
  ((remaining > 0)) || pe_fail 'overall deadline exceeded'
  ((cap <= remaining)) || cap=$remaining
  timeout --kill-after=2s "${cap}s" docker "$@"
}

# pe_inner <cap> <docker args...>: docker CLI inside the daemon container, aimed
# at the inner endpoint.
pe_inner() {
  local cap=$1
  shift
  pe_outer "$cap" exec "$PE_DAEMON" env -i "PATH=$PE_PATH" \
    docker --host "$PE_ENDPOINT" "$@"
}

# pe_inner_sh <cap> <script>: sh -c inside the daemon container as its uid.
pe_inner_sh() {
  local cap=$1 script=$2
  pe_outer "$cap" exec "$PE_DAEMON" env -i "PATH=$PE_PATH" \
    "XDG_RUNTIME_DIR=/run/dind" sh -c "$script"
}

# pe_init <rootless|rootful>
pe_init() {
  PE_POSTURE=$1
  command -v timeout >/dev/null || pe_fail 'timeout is required'
  command -v docker >/dev/null || pe_fail 'docker is required'
  case $PE_POSTURE in
    rootless)
      PE_IMAGE=$PE_ROOTLESS_IMAGE
      PE_DATA_ROOT=/home/rootless/.local/share/docker
      PE_ENDPOINT=unix:///run/dind/docker.sock
      ;;
    rootful)
      PE_IMAGE=$PE_ROOTFUL_IMAGE
      PE_DATA_ROOT=/var/lib/docker
      PE_ENDPOINT=tcp://127.0.0.1:2375
      ;;
    *) pe_fail "unknown posture: $PE_POSTURE" ;;
  esac
  PE_TOKEN="$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
  [[ $PE_TOKEN =~ ^[0-9a-f]{16}$ ]] || pe_fail 'token generation failed'
  PE_DAEMON="pe1860-$PE_POSTURE-$PE_TOKEN"
  PE_DATA_VOL="$PE_DAEMON-data"
  PE_RUN_VOL="$PE_DAEMON-run"
  PE_DEADLINE=$((SECONDS + ${PE_DEADLINE_SECONDS:-900}))
  PE_CONTAINERS=()
  PE_VOLUMES=()
  trap pe_cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  pe_outer 10 info --format '{{.ServerVersion}}' >/dev/null
  if ! pe_outer 10 image inspect "$PE_IMAGE" >/dev/null 2>&1; then
    pe_outer 300 pull "$PE_IMAGE" >/dev/null
  fi
}

pe_volumes_create() {
  local vol
  for vol in "$PE_DATA_VOL" "$PE_RUN_VOL"; do
    PE_VOLUMES+=("$vol")
    pe_outer 20 volume create --label "$PE_LABEL_KEY=$PE_TOKEN" "$vol" >/dev/null
  done
  if [[ $PE_POSTURE == rootless ]]; then
    # Fresh named volumes are root-owned; hand them to uid 1000 with a labelled
    # one-shot helper (same image, no daemon started).
    local helper="$PE_DAEMON-init"
    PE_CONTAINERS+=("$helper")
    pe_outer 60 run --name "$helper" --label "$PE_LABEL_KEY=$PE_TOKEN" \
      --user 0:0 --entrypoint sh \
      -v "$PE_DATA_VOL:$PE_DATA_ROOT" -v "$PE_RUN_VOL:$PE_RUN_DIR" "$PE_IMAGE" \
      -c "mkdir -p $PE_DATA_ROOT $PE_RUN_DIR && chown 1000:1000 $PE_DATA_ROOT $PE_RUN_DIR" >/dev/null
    pe_rm_owned_container "$helper"
  fi
}

# Wait for the inner daemon: at most 40 probes, each capped at 5s.
pe_wait_ready() {
  local attempt
  for ((attempt = 0; attempt < 40; attempt++)); do
    if pe_inner 5 info >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  docker logs --tail 40 "$PE_DAEMON" >&2 || true
  pe_fail "inner daemon ($PE_POSTURE) did not become ready"
}

pe_daemon_create_start() {
  PE_CONTAINERS+=("$PE_DAEMON")
  local args=(create --name "$PE_DAEMON" --label "$PE_LABEL_KEY=$PE_TOKEN"
    --privileged --network bridge
    -v "$PE_DATA_VOL:$PE_DATA_ROOT" -v "$PE_RUN_VOL:$PE_RUN_DIR")
  case $PE_POSTURE in
    rootless)
      local -a dockerd_flags
      read -ra dockerd_flags <<< "${PE_ROOTLESS_DOCKERD_FLAGS:---storage-driver=vfs}"
      args+=(--user 1000:1000 -e XDG_RUNTIME_DIR=/run/dind -e DOCKER_TLS_CERTDIR=
        --tmpfs '/run/dind:uid=1000,gid=1000,mode=0755' "$PE_IMAGE"
        "${dockerd_flags[@]}")
      ;;
    rootful)
      args+=(-e DOCKER_TLS_CERTDIR= "$PE_IMAGE"
        dockerd --host=tcp://127.0.0.1:2375 --tls=false --storage-driver=vfs)
      ;;
  esac
  pe_outer 30 "${args[@]}" >/dev/null
  pe_outer 30 start "$PE_DAEMON" >/dev/null
  pe_wait_ready
}

# Sidecar restart: same container, same volumes.
pe_daemon_restart() {
  pe_outer 60 restart --time 10 "$PE_DAEMON" >/dev/null
  pe_wait_ready
}

# Successor pod epoch: a new container on the same two volumes.
pe_daemon_replace() {
  pe_rm_owned_container "$PE_DAEMON"
  pe_daemon_create_start
}

# pe_events_start <outfile>: hold an inner events stream open in the background.
pe_events_start() {
  local out=$1 remaining=$((PE_DEADLINE - SECONDS))
  ((remaining > 0)) || pe_fail 'overall deadline exceeded'
  PE_EVENTS_RC=''
  timeout --kill-after=2s "${remaining}s" docker exec "$PE_DAEMON" env -i "PATH=$PE_PATH" \
    docker --host "$PE_ENDPOINT" events --format '{{json .}}' >"$out" 2>&1 &
  PE_EVENTS_PID=$!
}

# pe_events_stop: end the stream if still open and record its exit status in
# PE_EVENTS_RC (143/137 style codes mean we stopped it, others mean it ended itself).
pe_events_stop() {
  [[ -n $PE_EVENTS_PID ]] || return 0
  if kill -0 "$PE_EVENTS_PID" 2>/dev/null; then
    kill "$PE_EVENTS_PID" 2>/dev/null || true
  fi
  PE_EVENTS_RC=0
  wait "$PE_EVENTS_PID" || PE_EVENTS_RC=$?
  PE_EVENTS_PID=''
}

pe_server_info() {
  echo "posture=$PE_POSTURE image=$PE_IMAGE"
  echo "ServerVersion=$(pe_inner 10 version --format '{{.Server.Version}}')"
  echo "GoVersion=$(pe_inner 10 version --format '{{.Server.GoVersion}}')"
  echo "APIVersion=$(pe_inner 10 version --format '{{.Server.APIVersion}}')"
  echo "InfoID=$(pe_inner 10 info --format '{{.ID}}')"
  echo "StorageDriver=$(pe_inner 10 info --format '{{.Driver}}')"
}

# Remove a container by exact name only if it carries our label. Absent is ok.
pe_rm_owned_container() {
  local name=$1 owner
  if ! owner=$(timeout --kill-after=2s 10s docker inspect \
    --format "{{index .Config.Labels \"$PE_LABEL_KEY\"}}" "$name" 2>/dev/null); then
    return 0
  fi
  [[ $owner == "$PE_TOKEN" ]] || pe_fail "ownership mismatch for container $name"
  timeout --kill-after=2s 60s docker rm -fv "$name" >/dev/null
}

pe_rm_owned_volume() {
  local name=$1 owner
  if ! owner=$(timeout --kill-after=2s 10s docker volume inspect \
    --format "{{index .Labels \"$PE_LABEL_KEY\"}}" "$name" 2>/dev/null); then
    return 0
  fi
  [[ $owner == "$PE_TOKEN" ]] || pe_fail "ownership mismatch for volume $name"
  timeout --kill-after=2s 60s docker volume rm "$name" >/dev/null
}

# EXIT trap: verified removal of what this run created; preserves original status.
pe_cleanup() {
  local original=$? name rc=0
  trap - EXIT
  pe_events_stop || true
  for name in "${PE_CONTAINERS[@]}"; do
    (pe_rm_owned_container "$name") || rc=1
  done
  for name in "${PE_VOLUMES[@]}"; do
    (pe_rm_owned_volume "$name") || rc=1
  done
  if ((original != 0)); then exit "$original"; fi
  exit "$rc"
}
