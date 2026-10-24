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
PE_PAUSE=''
PE_WORKER=''
PE_DATA_VOL=''
PE_RUN_VOL=''
PE_SOCK_VOL=''
PE_SOCK_DIR=''
PE_DEADLINE=0
PE_CONTAINERS=()
PE_VOLUMES=()
PE_EVENTS_PID=''
PE_EVENTS_RC=''
PE_EVENTS_END=''
PE_EVENTS_EOF_US=''
PE_FR_PID=''
PE_FR_RC=''

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

# pe_client <cap> <docker args...>: docker CLI in the worker container (its own
# container, so it works while the daemon container is down or restarting).
pe_client() {
  local cap=$1
  shift
  pe_outer "$cap" exec "$PE_WORKER" env -i "PATH=$PE_PATH" \
    docker --host "$PE_ENDPOINT" "$@"
}

# pe_root_sh <cap> <script>: sh -c inside the daemon container as uid 0 with the
# container's full (privileged) capability set. Instrument use only: it can
# read the per-process proc files (mountinfo, cwd, fd) of every process,
# including the rootless daemon's user-namespaced children.
pe_root_sh() {
  local cap=$1 script=$2
  pe_outer "$cap" exec --user 0:0 "$PE_DAEMON" env -i "PATH=$PE_PATH" \
    "XDG_RUNTIME_DIR=/run/dind" sh -c "$script"
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
      # The socket sits on a volume shared with the worker container (in
      # production it is the pod-lifetime dind-sock emptyDir). XDG_RUNTIME_DIR
      # (pid files, rootlesskit state) stays a per-start tmpfs.
      PE_SOCK_DIR=/run/dind-sock
      PE_ENDPOINT=unix:///run/dind-sock/docker.sock
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
  PE_PAUSE="$PE_DAEMON-pause"
  PE_WORKER="$PE_DAEMON-worker"
  PE_DATA_VOL="$PE_DAEMON-data"
  PE_RUN_VOL="$PE_DAEMON-run"
  PE_SOCK_VOL="$PE_DAEMON-sock"
  PE_DEADLINE=$((SECONDS + deadline_seconds))
  # The pod's containers are registered once, before they exist; removal of an
  # absent container is a no-op, and a replace reuses the same names. Order:
  # users of the pod network namespace first, its owner (pause) last.
  PE_CONTAINERS=("$PE_WORKER" "$PE_DAEMON" "$PE_PAUSE")
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
    PE_VOLUMES+=("$PE_SOCK_VOL")
    pe_outer 20 volume create --label "$PE_LABEL_KEY=$PE_TOKEN" "$PE_SOCK_VOL" >/dev/null
    # Fresh named volumes are root-owned; hand them to uid 1000 with a labelled
    # one-shot helper (same image, no daemon started).
    local helper="$PE_DAEMON-init"
    PE_CONTAINERS+=("$helper")
    pe_outer 60 run --name "$helper" --label "$PE_LABEL_KEY=$PE_TOKEN" \
      --user 0:0 --entrypoint sh \
      -v "$PE_DATA_VOL:$PE_DATA_ROOT" -v "$PE_RUN_VOL:$PE_RUN_DIR" \
      -v "$PE_SOCK_VOL:$PE_SOCK_DIR" "$PE_IMAGE" \
      -c "mkdir -p $PE_DATA_ROOT $PE_RUN_DIR && chown 1000:1000 $PE_DATA_ROOT $PE_RUN_DIR $PE_SOCK_DIR" >/dev/null
    pe_rm_owned_container "$helper"
  fi
}

# Wait for the inner daemon: at most 40 probes, each capped at 5s, within the
# shared deadline.
pe_wait_ready() {
  local attempt
  for ((attempt = 0; attempt < 40; attempt++)); do
    ((PE_DEADLINE - SECONDS > 0)) || pe_fail "overall deadline exceeded waiting for inner daemon ($PE_POSTURE)"
    if pe_client 5 info >/dev/null 2>&1; then return 0; fi
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
  # Fail closed: /proc/net/tcp must exist and be readable; /proc/net/tcp6 may be
  # absent (no IPv6 in the namespace) but, if present, must be readable. Each
  # awk's status is checked individually.
  found=$(pe_inner_sh 10 '
    [ -r /proc/net/tcp ] || { echo "unreadable /proc/net/tcp" >&2; exit 3; }
    if [ -e /proc/net/tcp6 ] && [ ! -r /proc/net/tcp6 ]; then
      echo "unreadable /proc/net/tcp6" >&2; exit 3
    fi
    for f in /proc/net/tcp /proc/net/tcp6; do
      [ -e "$f" ] || continue
      out=$(awk -v f="$f" '"'"'NR > 1 && $4 == "0A" {
        split($2, a, ":")
        if (a[1] != "0100007F" && a[1] != "00000000000000000000000001000000" \
            && a[1] != "0000000000000000FFFF00000100007F") print f " " $2
      }'"'"' "$f") || { echo "awk failed on $f" >&2; exit 4; }
      [ -z "$out" ] || echo "$out"
    done') || pe_fail "listener check failed ($PE_POSTURE)"
  [[ -z $found ]] || pe_fail "non-loopback TCP listener in daemon container ($PE_POSTURE): $found"
}

# The pod's containers, created without being started:
#   pause   owns the network namespace (the pod sandbox); it outlives a daemon
#           container restart, as the pod's netns outlives a sidecar restart.
#   worker  stands in for the uzi worker container: a docker CLI client in its
#           own container and pid namespace, so a daemon restart or death does
#           not kill the client (an in-daemon-container client would die with
#           the container and make "stream ended" meaningless).
#   daemon  the DinD sidecar.
pe_create_pause() {
  pe_outer 30 create --name "$PE_PAUSE" --label "$PE_LABEL_KEY=$PE_TOKEN" --network bridge \
    --entrypoint sleep "$PE_IMAGE" 1000000 >/dev/null
}

pe_create_worker() {
  local wargs=(create --name "$PE_WORKER" --label "$PE_LABEL_KEY=$PE_TOKEN"
    --network "container:$PE_PAUSE" --entrypoint sleep)
  [[ $PE_POSTURE != rootless ]] || wargs+=(--user 1000:1000 -v "$PE_SOCK_VOL:$PE_SOCK_DIR")
  pe_outer 30 "${wargs[@]}" "$PE_IMAGE" 1000000 >/dev/null
}

pe_create_daemon() {
  local args=(create --name "$PE_DAEMON" --label "$PE_LABEL_KEY=$PE_TOKEN"
    --privileged --network "container:$PE_PAUSE"
    -v "$PE_DATA_VOL:$PE_DATA_ROOT" -v "$PE_RUN_VOL:$PE_RUN_DIR")
  case $PE_POSTURE in
    rootless)
      # Explicit command: with flags only, the image entrypoint adds
      # --host=tcp://0.0.0.0:2375 and publishes it. A first arg of dockerd keeps
      # the entrypoint's rootlesskit branch.
      args+=(--user 1000:1000 -e XDG_RUNTIME_DIR=/run/dind -e DOCKER_TLS_CERTDIR=
        --tmpfs '/run/dind:uid=1000,gid=1000,mode=0755'
        -v "$PE_SOCK_VOL:$PE_SOCK_DIR" "$PE_IMAGE"
        dockerd --host="$PE_ENDPOINT" --storage-driver=vfs)
      ;;
    rootful)
      args+=(-e DOCKER_TLS_CERTDIR= "$PE_IMAGE"
        dockerd --host=tcp://127.0.0.1:2375 --tls=false --storage-driver=vfs)
      ;;
  esac
  pe_outer 30 "${args[@]}" >/dev/null
}

pe_pod_create() {
  pe_create_pause
  pe_create_daemon
  pe_create_worker
}

# pe_daemon_container_recreate: what kubelet does when the sidecar container
# dies: a NEW container instance (fresh root filesystem, same pod network
# namespace, same volumes). Unlike pe_daemon_restart, the old container's
# writable layer (e.g. a stale pid file under /var/run) does not carry over.
pe_daemon_container_recreate() {
  pe_rm_owned_container "$PE_DAEMON"
  pe_create_daemon
  pe_outer 30 start "$PE_DAEMON" >/dev/null
  pe_wait_ready
  pe_assert_no_public_listener
}

# pe_daemon_start: start the pod (pause, worker) and the daemon; wait ready.
pe_daemon_start() {
  pe_outer 30 start "$PE_PAUSE" >/dev/null
  pe_outer 30 start "$PE_WORKER" >/dev/null
  pe_outer 30 start "$PE_DAEMON" >/dev/null
  pe_wait_ready
  pe_assert_no_public_listener
}

pe_daemon_create_start() {
  pe_pod_create
  pe_daemon_start
}

# Remove the whole pod (worker, daemon, pause); volumes stay.
pe_pod_remove() {
  local name
  for name in "$PE_WORKER" "$PE_DAEMON" "$PE_PAUSE"; do
    pe_rm_owned_container "$name"
  done
}

# Sidecar restart: same daemon container (and pod), same volumes.
pe_daemon_restart() {
  pe_outer 60 restart --time 10 "$PE_DAEMON" >/dev/null
  pe_wait_ready
}

# Successor pod epoch: a new pod (new network namespace, new worker container)
# on the same volumes. The old daemon dies by `docker rm -f` (SIGKILL).
pe_daemon_replace() {
  pe_pod_remove
  pe_daemon_create_start
}

# pe_worker_sh <cap> <script>: sh -c in the worker container as its uid.
pe_worker_sh() {
  local cap=$1 script=$2
  pe_outer "$cap" exec "$PE_WORKER" env -i "PATH=$PE_PATH" sh -c "$script"
}

# Same with a fixed cap outside the shared deadline, so teardown paths still run
# after the deadline is spent. Never calls pe_fail.
pe_worker_sh_fixed() {
  local cap=$1 script=$2
  timeout --kill-after=2s "${cap}s" docker exec "$PE_WORKER" env -i "PATH=$PE_PATH" sh -c "$script"
}

# pe_events_start <outfile>: hold an events stream open in the background from
# the worker container: a client outside the daemon container, so a daemon
# restart or death does not kill it and "stream ended" means the daemon closed
# it. The stream is replayed from a timestamp taken in the worker before it
# starts (--since), so no event after that instant is missed whenever the
# subscription lands. Returns once the in-container pid file exists.
pe_events_start() {
  local out=$1 since attempt remaining=$((PE_DEADLINE - SECONDS))
  [[ -z $PE_EVENTS_PID ]] || pe_fail 'events stream already active'
  ((remaining > 0)) || pe_fail 'overall deadline exceeded'
  PE_EVENTS_RC=''
  PE_EVENTS_END=''
  pe_worker_sh 5 "rm -f $PE_EVENTS_PIDFILE"
  since=$(pe_worker_sh 5 'date +%s')
  [[ $since =~ ^[0-9]{9,12}$ ]] || pe_fail "bad since timestamp: $since"
  timeout --kill-after=2s "${remaining}s" docker exec "$PE_WORKER" env -i "PATH=$PE_PATH" \
    sh -c "echo \$\$ > $PE_EVENTS_PIDFILE; exec docker --host '$PE_ENDPOINT' events --since $since --format '{{json .}}'" \
    >"$out" 2>&1 &
  PE_EVENTS_PID=$!
  for ((attempt = 0; attempt < 10; attempt++)); do
    if pe_worker_sh 5 "test -s $PE_EVENTS_PIDFILE" 2>/dev/null; then return 0; fi
    sleep 1
  done
  pe_fail 'events stream did not start'
}

# pe_events_wait_end <max-seconds>: poll (20 ms) until the stream's local client
# exits. On success sets PE_EVENTS_EOF_US (epoch microseconds) and returns 0;
# returns 1 if the stream was still open after the bound.
pe_events_wait_end() {
  local max=$1 i
  [[ -n $PE_EVENTS_PID ]] || pe_fail 'no events stream active'
  PE_EVENTS_EOF_US=''
  for ((i = 0; i < max * 50; i++)); do
    if ! kill -0 "$PE_EVENTS_PID" 2>/dev/null; then
      PE_EVENTS_EOF_US=${EPOCHREALTIME/[.,]/}
      return 0
    fi
    sleep 0.02
  done
  return 1
}

# pe_events_stop: end the stream, then verify the in-container client is gone.
# Sets PE_EVENTS_RC (raw exit status of the local exec) and PE_EVENTS_END:
#   stopped  the stream was still open and we ended it
#   ended    the stream had already ended on its own (e.g. daemon restart)
#   deadline the overall deadline timer killed the local client (status 124)
# On every path, while the worker container is up, it kills any process in it
# whose cmdline contains " events " (the client may outlive the local exec,
# e.g. after the deadline killed only the local side), waits for them to exit,
# and requires both that none remain and that the pid file is gone. A worker
# container that is gone or stopped takes its processes with it. Returns 1 if
# the in-container client could not be verified gone.
pe_events_stop() {
  [[ -n $PE_EVENTS_PID ]] || return 0
  local pid=$PE_EVENTS_PID rc=0 i running script
  PE_EVENTS_END=ended
  if kill -0 "$pid" 2>/dev/null; then PE_EVENTS_END=stopped; fi
  running=$(timeout --kill-after=2s 10s docker inspect --type container \
    --format '{{.State.Running}}' "$PE_WORKER" 2>&1) || {
    if grep -qiE 'no such (container|object)' <<<"$running"; then running=false; else running=unknown; fi
  }
  if [[ $running != false ]]; then
    # The pattern is assembled in the script so the checker's own cmdline does
    # not contain it.
    read -r -d '' script <<'EOS' || true
pat=" ev""ents "
scan() {
  found=''
  for d in /proc/[0-9]*; do
    p=${d#/proc/}
    [ "$p" != "$$" ] || continue
    c=$(tr "\0" " " 2>/dev/null < "$d/cmdline") || continue
    case "$c" in *"$pat"*) found="$found $p" ;; esac
  done
}
scan
for p in $found; do kill "$p" 2>/dev/null; done
i=0
while [ "$i" -lt 10 ]; do
  scan
  [ -n "$found" ] || break
  sleep 1
  i=$((i + 1))
done
if [ -n "$found" ]; then
  for p in $found; do kill -9 "$p" 2>/dev/null; done
  sleep 1
  scan
fi
[ -z "$found" ] || { echo "events client still running:$found" >&2; exit 1; }
rm -f "$pidfile"
[ ! -e "$pidfile" ] || { echo "pid file remains" >&2; exit 1; }
EOS
    script="pidfile='$PE_EVENTS_PIDFILE'"$'\n'"$script"
    pe_worker_sh_fixed 40 "$script" >/dev/null || rc=1
  fi
  for ((i = 0; i < 25; i++)); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.2
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
    for ((i = 0; i < 10; i++)); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.2
    done
    kill -9 "$pid" 2>/dev/null || true
  fi
  if wait "$pid"; then PE_EVENTS_RC=0; else PE_EVENTS_RC=$?; fi
  [[ $PE_EVENTS_RC -ne 124 ]] || PE_EVENTS_END=deadline
  PE_EVENTS_PID=''
  return "$rc"
}

# pe_first_response_start <outfile>: start, in the worker container, a poller
# for the daemon's first API response after the daemon goes down and comes back
# (it first waits to observe a failed probe, so a still-live pre-restart daemon
# is never mistaken for the new incarnation). The first successful `docker info`
# is the first response; the poller then immediately lists containers
# (`ps -a`, GET /containers/json?all=1), inspects each, and lists plugins.
# Output lines: FR_INFO, FR_PROBES, FR_PS, FR_INSPECT, FR_PLUGIN, FR_DONE.
# Start it BEFORE triggering the restart or replace; read it with
# pe_first_response_wait.
pe_first_response_start() {
  local out=$1 script remaining=$((PE_DEADLINE - SECONDS))
  [[ -z $PE_FR_PID ]] || pe_fail 'first-response poller already active'
  ((remaining > 0)) || pe_fail 'overall deadline exceeded'
  read -r -d '' script <<'EOS' || true
E=$1
down=0
n=0
while :; do
  if info=$(timeout 120 docker --host "$E" info --format 'swarm={{.Swarm.LocalNodeState}} live={{.LiveRestoreEnabled}} id={{.ID}} total={{.Containers}} running={{.ContainersRunning}} paused={{.ContainersPaused}} stopped={{.ContainersStopped}} time={{.SystemTime}}' 2>/dev/null); then
    [ "$down" = 1 ] && break
  else
    down=1
  fi
  n=$((n + 1))
  sleep 0.05
done
echo "FR_INFO $info"
echo "FR_PROBES $n"
ps=$(docker --host "$E" ps -a --no-trunc --format '{{.ID}}|{{.Names}}|{{.State}}|{{.Status}}') || { echo "FR_ERR ps"; exit 1; }
[ -z "$ps" ] || echo "$ps" | sed 's/^/FR_PS /'
ids=$(echo "$ps" | cut -d'|' -f1)
# shellcheck disable=SC2086
[ -z "$ids" ] || docker --host "$E" inspect -f 'FR_INSPECT {{.Name}}|{{.State.Status}}|{{.State.StartedAt}}|{{.State.ExitCode}}|{{.RestartCount}}|{{.HostConfig.RestartPolicy.Name}}' $ids || { echo "FR_ERR inspect"; exit 1; }
docker --host "$E" plugin ls -q | sed 's/^/FR_PLUGIN /'
echo FR_DONE
EOS
  PE_FR_RC=''
  timeout --kill-after=2s "${remaining}s" docker exec "$PE_WORKER" env -i "PATH=$PE_PATH" \
    sh -c "$script" fr "$PE_ENDPOINT" >"$out" 2>&1 &
  PE_FR_PID=$!
}

# pe_first_response_wait: wait for the poller; returns its exit status.
pe_first_response_wait() {
  [[ -n $PE_FR_PID ]] || pe_fail 'no first-response poller active'
  local rc=0
  wait "$PE_FR_PID" || rc=$?
  PE_FR_PID=''
  PE_FR_RC=$rc
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
  if [[ -n $PE_FR_PID ]]; then
    kill "$PE_FR_PID" 2>/dev/null || true
    wait "$PE_FR_PID" 2>/dev/null || true
  fi
  for name in "${PE_CONTAINERS[@]}"; do
    pe_rm_owned_container "$name" || rc=1
  done
  for name in "${PE_VOLUMES[@]}"; do
    pe_rm_owned_volume "$name" || rc=1
  done
  if [[ -n $PE_EXP && $PE_EXP_DONE -eq 0 ]]; then
    # The experiment did not reach pe_exp_finish: a harness error or abort.
    echo "EXP $PE_EXP $PE_POSTURE FAIL harness-error"
    ((original != 0)) || original=1
  fi
  if ((original != 0)); then exit "$original"; fi
  exit "$rc"
}

# ---------------------------------------------------------------------------
# Experiment reporting. Each experiment script calls pe_exp_begin <name> once per
# posture, prints RESULT lines (pe_res / pe_check / pe_incomplete), and ends with
# pe_exp_finish, which prints `EXP <name> <posture> PASS|FAIL|INCOMPLETE` and
# exits 0, 1 or 3. FAIL outranks INCOMPLETE.
# ---------------------------------------------------------------------------
PE_EXP=''
PE_EXP_DONE=1
PE_EXP_STATUS=PASS

pe_exp_begin() {
  PE_EXP=$1
  PE_EXP_DONE=0
  PE_EXP_STATUS=PASS
}

# pe_res <key> <value...>: one machine-greppable line; newlines become " | ".
pe_res() {
  local key=$1
  shift
  local value="$*"
  value=${value//$'\n'/ | }
  echo "RESULT $PE_EXP $PE_POSTURE $key=$value"
}

# pe_check <name> <command...>: PASS if the command succeeds. A FAIL is
# recorded but does not abort, so later results are still gathered.
pe_check() {
  local name=$1
  shift
  if "$@"; then
    pe_res "check.$name" PASS
  else
    pe_res "check.$name" FAIL
    PE_EXP_STATUS=FAIL
  fi
}

# pe_control <name> <command...>: a positive control for an instrument. If it
# fails, the instrument cannot see what it is asserted to find absent, so the
# experiment is INCOMPLETE rather than passed.
pe_control() {
  local name=$1
  shift
  if "$@"; then
    pe_res "control.$name" PASS
  else
    pe_incomplete "$name" "positive control failed"
  fi
}

# pe_mark_fail: record a FAIL without a named check.
pe_mark_fail() { PE_EXP_STATUS=FAIL; }

# pe_incomplete <instrument> <reason>: an instrument could not do its job.
pe_incomplete() {
  pe_res "instrument.$1" "unavailable reason=$2"
  [[ $PE_EXP_STATUS == FAIL ]] || PE_EXP_STATUS=INCOMPLETE
}

pe_exp_finish() {
  echo "EXP $PE_EXP $PE_POSTURE $PE_EXP_STATUS"
  PE_EXP_DONE=1
  case $PE_EXP_STATUS in
    PASS) exit 0 ;;
    INCOMPLETE) exit 3 ;;
    *) exit 1 ;;
  esac
}

# pe_exp_main <experiment> <posture-function> <rootless|rootful|all>: runs the
# posture function once per posture, each in its own background subshell (so
# `set -e` stays active inside it and its EXIT trap cleans only its own pod),
# and exits 0 only if every posture passed (3 if some were INCOMPLETE and none
# failed, else 1).
pe_exp_main() {
  local exp=$1 fn=$2 which=${3:-} p child status rc=0 postures
  case $which in
    rootless | rootful) postures=("$which") ;;
    all) postures=(rootless rootful) ;;
    *)
      echo "usage: $0 <rootless|rootful|all>" >&2
      exit 2
      ;;
  esac
  PE_MAIN_CHILD=''
  trap 'if [[ -n $PE_MAIN_CHILD ]]; then kill "$PE_MAIN_CHILD" 2>/dev/null; wait "$PE_MAIN_CHILD" 2>/dev/null; fi; exit 130' INT TERM
  for p in "${postures[@]}"; do
    "$fn" "$p" &
    child=$!
    PE_MAIN_CHILD=$child
    status=0
    wait "$child" || status=$?
    PE_MAIN_CHILD=''
    case $status in
      0) ;;
      3) ((rc == 1)) || rc=3 ;;
      *) rc=1 ;;
    esac
  done
  echo "DONE $exp $which rc=$rc"
  exit "$rc"
}

# ---------------------------------------------------------------------------
# Tree-reference instrument. Everything below runs as root inside the daemon
# container (privileged, so it can read every process's mountinfo, cwd, root,
# exe, fd and maps) over the container's whole pid namespace. That pid namespace
# contains dockerd, containerd, the shims and every container task, in both
# postures (rootless: rootlesskit's child namespaces are children of the same
# pid namespace, so they are visible; `docker inspect` State.Pid is a pid in this
# namespace). It finds references to a directory tree that lives under
# /data/runner:
#   MNT-STRICT  a mount whose source is the tree or inside it (same backing
#               device as /data/runner, root field = that mount's root + tree)
#   MNT-LOOSE   any other mountinfo line that mentions "/<tree-relative-path>"
#   FD          a process cwd, root, exe or open fd whose target (stat -L,
#               resolved by the kernel, independent of the process's own mount
#               namespace) has the device:inode of the tree or of anything that
#               was inside it, including since-deleted files
#   MAP         a memory mapping of such an inode on the same device
# Mountinfo is read once per distinct mount namespace (first readable process).
# Limit: a mount namespace kept alive with no process in it is not reachable.
# ---------------------------------------------------------------------------

# pe_tree_ids <tree>: print the device:inode of the tree and everything in it.
pe_tree_ids() {
  pe_root_sh 20 "find '$1' -exec stat -c '%d:%i' {} + | sort -u"
}

# pe_tree_scan <tree> <ids>: print reference lines, then a summary line
# `SCANNED pids=<n> mountns=<n>`. <ids> is the output of pe_tree_ids, taken
# while the tree existed.
pe_tree_scan() {
  local tree=$1 ids=$2 body script
  [[ $tree == "$PE_RUN_DIR"/* ]] || pe_fail "pe_tree_scan: tree outside $PE_RUN_DIR: $tree"
  read -r -d '' body <<'EOS' || true
rel=${tree#/data/runner/}
mi_line=$(grep ' /data/runner ' /proc/self/mountinfo | head -1)
[ -n "$mi_line" ] || { echo "SCAN-ERROR no /data/runner mount" >&2; exit 5; }
mi_dev=$(echo "$mi_line" | awk '{print $3}')
mi_root=$(echo "$mi_line" | awk '{print $4}')
maj=${mi_dev%:*}
min=${mi_dev#*:}
dev_hex=$(printf '%02x:%02x' "$maj" "$min")
pids=0
nsn=0
seen=' '
for d in /proc/[0-9]*; do
  p=${d#/proc/}
  [ -d "$d" ] || continue
  pids=$((pids + 1))
  ns=$(readlink "$d/ns/mnt" 2>/dev/null) || continue
  case "$seen" in *" $ns "*) ;; *)
    if cat "$d/mountinfo" > /tmp/pe-mi.$$ 2>/dev/null; then
      seen="$seen$ns "
      nsn=$((nsn + 1))
      awk -v ns="$ns" -v pid="$p" -v dev="$mi_dev" -v R="$mi_root" -v rel="$rel" '
        { strict = ($3 == dev && ($4 == R "/" rel || index($4, R "/" rel "/") == 1))
          if (strict) print "MNT-STRICT ns=" ns " pid=" pid " " $0
          else if (index($0, "/" rel) > 0) print "MNT-LOOSE ns=" ns " pid=" pid " " $0 }' /tmp/pe-mi.$$
    fi ;;
  esac
  cmd=$(tr "\0" " " 2>/dev/null < "$d/cmdline" | cut -c1-70)
  stat -L -c '%d:%i %N' "$d/cwd" "$d/root" "$d/exe" "$d"/fd/* 2>/dev/null \
    | awk -v pid="$p" -v cmd="$cmd" 'NR == FNR { ids[$1] = 1; next } ($1 in ids) { print "FD pid=" pid " cmd=[" cmd "] " $0 }' "$idsf" /dev/stdin
  awk -v pid="$p" -v dev="$dev_hex" 'NR == FNR { split($1, a, ":"); inos[a[2]] = 1; next }
    ($4 == dev && ($5 in inos)) { print "MAP pid=" pid " " $0 }' "$idsf" "$d/maps" 2>/dev/null
done
rm -f /tmp/pe-mi.$$
echo "SCANNED pids=$pids mountns=$nsn"
EOS
  script="tree='$tree'"$'\n'"idsf=/tmp/pe-ids.\$\$"$'\n'"cat > \"\$idsf\" <<'PEIDS'"$'\n'"$ids"$'\n'"PEIDS"$'\n'"$body"$'\n''rm -f "$idsf"'
  pe_root_sh 60 "$script"
}

# pe_scan_refs <scan-output>: count of reference lines (everything but SCANNED).
pe_scan_refs() {
  grep -cE '^(MNT-STRICT|MNT-LOOSE|FD|MAP) ' <<<"$1" || true
}
