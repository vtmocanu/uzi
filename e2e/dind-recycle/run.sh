#!/usr/bin/env bash
set -euo pipefail

# Pins match deploy/chart/values.yaml and docker-compose.yml.
readonly DIND_IMAGE='docker:29-dind@sha256:5efed980cba3fc126cf54e21a5a6ff8849d05b6e0623d6e7612f48e9cd6cd17e'
readonly FIXTURE_IMAGE='alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6'
command -v timeout >/dev/null || { echo 'FAIL: timeout is required' >&2; exit 1; }
command -v docker >/dev/null || { echo 'FAIL: docker is required' >&2; exit 1; }
token="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
readonly token
[[ $token =~ ^[0-9a-f]{32}$ ]] || exit 1
readonly daemon="dind-prune-test-$token"
readonly ownership_label='dind-prune-test.owner'
readonly deadline=$((SECONDS + 360))
attempted=0

fail() { echo "FAIL: $*" >&2; exit 1; }

# Each Docker call has a cap within a shared 360-second deadline.
outer() {
  local cap=$1 remaining=$((deadline - SECONDS))
  shift
  ((remaining > 0)) || fail 'overall deadline exceeded'
  ((cap <= remaining)) || cap=$remaining
  timeout --kill-after=2s "${cap}s" docker "$@"
}
inner() {
  local cap=$1
  shift
  outer "$cap" exec "$daemon" env -i \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    docker --host unix:///var/run/docker.sock "$@"
}
cleanup() {
  local original=$? owner cleanup_rc=0
  trap - EXIT
  # A timed-out create may have succeeded. Verify ownership even on that path.
  if ((attempted)); then
    if owner=$(timeout --kill-after=2s 10s docker inspect \
      --format "{{index .Config.Labels \"$ownership_label\"}}" "$daemon"); then
      if [[ $owner == "$token" ]]; then
        timeout --kill-after=2s 30s docker rm -fv "$daemon" || cleanup_rc=$?
      else
        echo "FAIL: cleanup ownership mismatch for $daemon" >&2
        cleanup_rc=1
      fi
    else
      echo "FAIL: cannot verify cleanup ownership for $daemon" >&2
      cleanup_rc=1
    fi
  fi
  # Fail a green run on cleanup error, preserving any original failing status.
  if ((original != 0)); then exit "$original"; fi
  exit "$cleanup_rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

outer 10 info --format '{{.ServerVersion}}' >/dev/null
if ! outer 10 image inspect "$DIND_IMAGE" >/dev/null 2>&1; then
  outer 180 pull "$DIND_IMAGE"
fi
attempted=1
outer 30 create --name "$daemon" --label "$ownership_label=$token" \
  --privileged --network bridge -e DOCKER_TLS_CERTDIR= \
  -v /var/lib/docker "$DIND_IMAGE" \
  dockerd --host=unix:///var/run/docker.sock --storage-driver=vfs >/dev/null
outer 30 start "$daemon" >/dev/null
ready=0
# At most 30 five-second probes; failure aborts all fixture work.
for ((attempt = 0; attempt < 30; attempt++)); do
  if inner 5 info >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
((ready)) || fail 'disposable Docker daemon did not become ready'
# Exactly one read-only SERVER API check before fixtures or prune.
api=$(inner 10 version --format '{{.Server.APIVersion}}')
[[ $api =~ ^([0-9]+)\.([0-9]+)$ ]] || fail "invalid server API version: $api"
((10#${BASH_REMATCH[1]} > 1 || (10#${BASH_REMATCH[1]} == 1 && 10#${BASH_REMATCH[2]} >= 42))) \
  || fail "server API $api is below 1.42"
echo "SERVER API: $api"

if ! inner 10 image inspect "$FIXTURE_IMAGE" >/dev/null 2>&1; then
  inner 180 pull "$FIXTURE_IMAGE"
fi
inner 10 volume create named-keep >/dev/null
for holder in unreferenced running stopped; do
  inner 20 run -d --name "$holder" -v /fixture "$FIXTURE_IMAGE" \
    sh -c 'echo fixture > /fixture/proof; exec sleep 600' >/dev/null
done
inner 20 stop --time 1 stopped >/dev/null
[[ $(inner 10 inspect --format '{{.State.Status}}' running) == running ]] \
  || fail 'running holder is not running'
[[ $(inner 10 inspect --format '{{.State.Status}}' stopped) == exited ]] \
  || fail 'stopped holder is not stopped'

volume_of() {
  inner 10 inspect --format \
    '{{range .Mounts}}{{if eq .Destination "/fixture"}}{{.Name}}{{end}}{{end}}' "$1"
}
unreferenced=$(volume_of unreferenced)
running=$(volume_of running)
stopped=$(volume_of stopped)
for volume in "$unreferenced" "$running" "$stopped"; do
  [[ $volume =~ ^[0-9a-f]{64}$ ]] || fail "invalid anonymous volume ID: $volume"
  [[ $(inner 10 volume inspect --format \
    "{{range \$key, \$value := .Labels}}{{println \$key}}{{end}}" "$volume") \
    == com.docker.volume.anonymous ]] || fail "volume $volume is not anonymous"
done
[[ $unreferenced != "$running" && $unreferenced != "$stopped" && $running != "$stopped" ]] \
  || fail 'fixture volumes are not distinct'
# Omit -v: the orphan must still exist before pruning.
inner 20 rm -f unreferenced >/dev/null
for volume in "$unreferenced" named-keep "$running" "$stopped"; do
  inner 10 volume inspect "$volume" >/dev/null
done

# Exact command under test, routed exclusively to the disposable inner daemon.
inner 30 volume prune -f

# A successful list proves absence; failed inspect could mean a broken daemon.
volumes=$(inner 10 volume ls --format '{{.Name}}')
contains_volume() {
  local wanted=$1 existing
  while IFS= read -r existing; do
    [[ $existing != "$wanted" ]] || return 0
  done <<< "$volumes"
  return 1
}
if contains_volume "$unreferenced"; then fail 'unreferenced anonymous volume survived'; fi
for volume in named-keep "$running" "$stopped"; do
  contains_volume "$volume" || fail "protected volume $volume was removed"
  inner 10 volume inspect "$volume" >/dev/null
done
[[ $(inner 10 inspect --format '{{.State.Status}}' running) == running ]] \
  || fail 'running holder changed state'
[[ $(inner 10 inspect --format '{{.State.Status}}' stopped) == exited ]] \
  || fail 'stopped holder changed state'
echo 'PASS: unreferenced anonymous removed; named, running and stopped references retained'
