#!/usr/bin/env bash
# Controlled-daemon harness for PRD #1860 M0 real-daemon experiments.
# Sourced, never executed; the caller must run under `set -euo pipefail`, and
# one posture per (sub)shell (pe_init refuses a second call in the same shell).
# Modelled on e2e/dind-recycle/run.sh.
#
# Safety: the outer daemon is shared. This library only creates and removes
# containers and volumes named pe1860-<posture>-<token>[-suffix] that carry the
# label pe1860.owner=<token>, and removes by exact name after verifying that
# label. It never prunes, lists-and-removes or globs.

# shellcheck disable=SC2034  # variables are consumed by sourcing scripts
# Image pins match deploy/chart/values.yaml (the rendered DinD images); the
# compose file pins only the rootless one.
readonly PE_ROOTLESS_IMAGE='docker:29-dind-rootless@sha256:e17fa54c2ffd511d8407c746eec77f7814e6f74fe20caf822dad1870599984c0'
readonly PE_ROOTFUL_IMAGE='docker:29-dind@sha256:5efed980cba3fc126cf54e21a5a6ff8849d05b6e0623d6e7612f48e9cd6cd17e'
readonly PE_FIXTURE_IMAGE='alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6'
readonly PE_LABEL_KEY='pe1860.owner'
readonly PE_PATH='/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
readonly PE_RUN_DIR='/data/runner'
readonly PE_EVENTS_PIDFILE='/tmp/pe-events.pid'

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
PE_EVENTS_END=''

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

# Same as pe_inner_sh with a fixed cap outside the shared deadline, so teardown
# paths still run after the deadline is spent. Never calls pe_fail.
pe_inner_sh_fixed() {
  local cap=$1 script=$2
  timeout --kill-after=2s "${cap}s" docker exec "$PE_DAEMON" env -i "PATH=$PE_PATH" \
    "XDG_RUNTIME_DIR=/run/dind" sh -c "$script"
}

# pe_init <rootless|rootful>
pe_init() {
  [[ -z $PE_TOKEN ]] || pe_fail 'pe_init already called in this shell'
  local deadline_seconds=${PE_DEADLINE_SECONDS:-900}
  [[ $deadline_seconds =~ ^[1-9][0-9]{0,4}$ ]] \
    || pe_fail "PE_DEADLINE_SECONDS must match ^[1-9][0-9]{0,4}\$ (got: $deadline_seconds)"
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
  PE_DEADLINE=$((SECONDS + deadline_seconds))
  # The daemon container is registered once, before it exists; removal of an
  # absent container is a no-op, and a replace reuses the same name.
  PE_CONTAINERS=("$PE_DAEMON")
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

# Wait for the inner daemon: at most 40 probes, each capped at 5s, within the
# shared deadline.
pe_wait_ready() {
  local attempt
  for ((attempt = 0; attempt < 40; attempt++)); do
    ((PE_DEADLINE - SECONDS > 0)) || pe_fail "overall deadline exceeded waiting for inner daemon ($PE_POSTURE)"
    if pe_inner 5 info >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  timeout --kill-after=2s 10s docker logs --tail 40 "$PE_DAEMON" >&2 || true
  pe_fail "inner daemon ($PE_POSTURE) did not become ready"
}

# Read-only: fail if anything listens on a non-loopback TCP port in the daemon
# container's network namespace (the image entrypoint can publish an
# unauthenticated API on the shared bridge when DOCKER_TLS_CERTDIR is empty).
pe_assert_no_public_listener() {
  local found
  found=$(pe_inner_sh 10 '
    for f in /proc/net/tcp /proc/net/tcp6; do
      [ -r "$f" ] || continue
      awk -v f="$f" '"'"'NR > 1 && $4 == "0A" {
        split($2, a, ":")
        if (a[1] != "0100007F" && a[1] != "00000000000000000000000001000000" \
            && a[1] != "0000000000000000FFFF00000100007F") print f " " $2
      }'"'"' "$f"
    done') || pe_fail "listener check failed ($PE_POSTURE)"
  [[ -z $found ]] || pe_fail "non-loopback TCP listener in daemon container ($PE_POSTURE): $found"
}

pe_daemon_create_start() {
  local args=(create --name "$PE_DAEMON" --label "$PE_LABEL_KEY=$PE_TOKEN"
    --privileged --network bridge
    -v "$PE_DATA_VOL:$PE_DATA_ROOT" -v "$PE_RUN_VOL:$PE_RUN_DIR")
  case $PE_POSTURE in
    rootless)
      # Explicit command: with flags only, the image entrypoint adds
      # --host=tcp://0.0.0.0:2375 and publishes it. A first arg of dockerd keeps
      # the entrypoint's rootlesskit branch.
      args+=(--user 1000:1000 -e XDG_RUNTIME_DIR=/run/dind -e DOCKER_TLS_CERTDIR=
        --tmpfs '/run/dind:uid=1000,gid=1000,mode=0755' "$PE_IMAGE"
        dockerd --host=unix:///run/dind/docker.sock --storage-driver=vfs)
      ;;
    rootful)
      args+=(-e DOCKER_TLS_CERTDIR= "$PE_IMAGE"
        dockerd --host=tcp://127.0.0.1:2375 --tls=false --storage-driver=vfs)
      ;;
  esac
  pe_outer 30 "${args[@]}" >/dev/null
  pe_outer 30 start "$PE_DAEMON" >/dev/null
  pe_wait_ready
  pe_assert_no_public_listener
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
# The stream is replayed from a timestamp taken inside the daemon container
# before it starts (--since), so no event after that instant is missed whenever
# the subscription lands. Returns once the in-container pid file exists.
pe_events_start() {
  local out=$1 since attempt remaining=$((PE_DEADLINE - SECONDS))
  [[ -z $PE_EVENTS_PID ]] || pe_fail 'events stream already active'
  ((remaining > 0)) || pe_fail 'overall deadline exceeded'
  PE_EVENTS_RC=''
  PE_EVENTS_END=''
  pe_inner_sh 5 "rm -f $PE_EVENTS_PIDFILE"
  since=$(pe_inner_sh 5 'date +%s')
  [[ $since =~ ^[0-9]{9,12}$ ]] || pe_fail "bad since timestamp: $since"
  timeout --kill-after=2s "${remaining}s" docker exec "$PE_DAEMON" env -i "PATH=$PE_PATH" \
    sh -c "echo \$\$ > $PE_EVENTS_PIDFILE; exec docker --host '$PE_ENDPOINT' events --since $since --format '{{json .}}'" \
    >"$out" 2>&1 &
  PE_EVENTS_PID=$!
  for ((attempt = 0; attempt < 10; attempt++)); do
    if pe_inner_sh 5 "test -s $PE_EVENTS_PIDFILE" 2>/dev/null; then return 0; fi
    sleep 1
  done
  pe_fail 'events stream did not start'
}

# pe_events_stop: end the stream and the in-container `docker events` process.
# Sets PE_EVENTS_RC (raw exit status of the local exec) and PE_EVENTS_END:
#   stopped  the stream was still open and we ended it
#   ended    the stream had already ended on its own (e.g. daemon restart)
#   deadline the overall deadline timer killed it (status 124)
# Returns 1 if the in-container process could not be verified gone.
pe_events_stop() {
  [[ -n $PE_EVENTS_PID ]] || return 0
  local pid=$PE_EVENTS_PID rc=0 i running
  PE_EVENTS_END=ended
  if kill -0 "$pid" 2>/dev/null; then
    PE_EVENTS_END=stopped
    # Only signal a pid whose cmdline is still our `docker ... events` (a pid
    # file left across a container restart may name an unrelated process).
    if ! pe_inner_sh_fixed 20 "p=\$(cat $PE_EVENTS_PIDFILE 2>/dev/null) || exit 0
      [ -n \"\$p\" ] || exit 0
      if [ -r /proc/\$p/cmdline ] && tr '\\0' ' ' < /proc/\$p/cmdline | grep -q ' events '; then
        kill \"\$p\"
        i=0
        while [ \$i -lt 10 ] && [ -d /proc/\$p ]; do sleep 1; i=\$((i + 1)); done
        [ ! -d /proc/\$p ] || exit 1
      fi
      rm -f $PE_EVENTS_PIDFILE" 2>/dev/null; then
      running=$(timeout --kill-after=2s 10s docker inspect --type container \
        --format '{{.State.Running}}' "$PE_DAEMON" 2>/dev/null) || running=unknown
      [[ $running == false ]] || rc=1
    fi
    for ((i = 0; i < 10; i++)); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.5
    done
    kill "$pid" 2>/dev/null || true
  fi
  if wait "$pid"; then PE_EVENTS_RC=0; else PE_EVENTS_RC=$?; fi
  [[ $PE_EVENTS_RC -ne 124 ]] || PE_EVENTS_END=deadline
  PE_EVENTS_PID=''
  return "$rc"
}

pe_server_info() {
  local sv go api id sd
  sv=$(pe_inner 10 version --format '{{.Server.Version}}')
  go=$(pe_inner 10 version --format '{{.Server.GoVersion}}')
  api=$(pe_inner 10 version --format '{{.Server.APIVersion}}')
  id=$(pe_inner 10 info --format '{{.ID}}')
  sd=$(pe_inner 10 info --format '{{.Driver}}')
  [[ -n $sv && -n $go && -n $api && -n $id && -n $sd ]] || pe_fail 'empty server info value'
  echo "posture=$PE_POSTURE image=$PE_IMAGE"
  echo "ServerVersion=$sv"
  echo "GoVersion=$go"
  echo "APIVersion=$api"
  echo "InfoID=$id"
  echo "StorageDriver=$sd"
}

# pe_rm_owned <kind: container|volume> <name>: remove by exact name only if it
# carries our label. Returns 0 only when removed or when docker explicitly
# reports the object absent; any other inspect error leaves it and returns 1.
pe_rm_owned() {
  local kind=$1 name=$2 owner
  [[ $PE_TOKEN =~ ^[0-9a-f]{16}$ ]] || { echo "refusing removal: bad token" >&2; return 1; }
  [[ -n $PE_LABEL_KEY && $name == pe1860-* ]] || { echo "refusing removal of $name" >&2; return 1; }
  if [[ $kind == container ]]; then
    owner=$(timeout --kill-after=2s 10s docker inspect --type container \
      --format "{{index .Config.Labels \"$PE_LABEL_KEY\"}}" "$name" 2>&1) || {
      if grep -qiE 'no such (container|object)' <<<"$owner"; then return 0; fi
      echo "LEFTOVER container $name (inspect failed: $owner)" >&2
      return 1
    }
  else
    owner=$(timeout --kill-after=2s 10s docker volume inspect \
      --format "{{index .Labels \"$PE_LABEL_KEY\"}}" "$name" 2>&1) || {
      if grep -qiE 'no such (volume|object)' <<<"$owner"; then return 0; fi
      echo "LEFTOVER volume $name (inspect failed: $owner)" >&2
      return 1
    }
  fi
  if [[ -z $owner || $owner != "$PE_TOKEN" ]]; then
    echo "LEFTOVER $kind $name (ownership mismatch)" >&2
    return 1
  fi
  if [[ $kind == container ]]; then
    timeout --kill-after=2s 60s docker rm -fv "$name" >/dev/null || { echo "LEFTOVER container $name (rm failed)" >&2; return 1; }
  else
    timeout --kill-after=2s 60s docker volume rm "$name" >/dev/null || { echo "LEFTOVER volume $name (rm failed)" >&2; return 1; }
  fi
}

pe_rm_owned_container() { pe_rm_owned container "$1"; }
pe_rm_owned_volume() { pe_rm_owned volume "$1"; }

# EXIT trap: verified removal of what this run created; preserves original status.
# Uses fixed caps (not the shared deadline) so it still runs after the deadline.
pe_cleanup() {
  local original=$? name rc=0
  trap '' INT TERM
  trap - EXIT
  pe_events_stop || rc=1
  for name in "${PE_CONTAINERS[@]}"; do
    pe_rm_owned_container "$name" || rc=1
  done
  for name in "${PE_VOLUMES[@]}"; do
    pe_rm_owned_volume "$name" || rc=1
  done
  if ((original != 0)); then exit "$original"; fi
  exit "$rc"
}
