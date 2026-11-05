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
readonly deadline=$((SECONDS + 600))
readonly inner_path='/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
attempted=0

fail() { echo "FAIL: $*" >&2; exit 1; }

# Each Docker call has a cap within a shared 600-second deadline.
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
    PATH="$inner_path" \
    docker --host unix:///var/run/docker.sock "$@"
}
# Same as inner, plus the explicit request API version the product leg pins.
inner_pinned() {
  local cap=$1 version=$2
  shift 2
  outer "$cap" exec "$daemon" env -i \
    PATH="$inner_path" DOCKER_API_VERSION="$version" \
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
  dockerd --debug --host=unix:///var/run/docker.sock --storage-driver=vfs >/dev/null
outer 30 start "$daemon" >/dev/null
ready=0
# At most 30 five-second probes; failure aborts all fixture work.
for ((attempt = 0; attempt < 30; attempt++)); do
  if inner 5 info >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
((ready)) || fail 'disposable Docker daemon did not become ready'
# Exactly one read-only SERVER API check before fixtures or prune.
versions=$(inner 10 version --format \
  '{{.Server.Version}}|{{.Server.GitCommit}}|{{.Server.APIVersion}}|{{.Server.MinAPIVersion}}|{{.Client.APIVersion}}')
IFS='|' read -r server_version server_commit api min_api client_api <<< "$versions"
echo "Server Version: $server_version"
echo "Server GitCommit: $server_commit"
echo "Server APIVersion: $api"
echo "Server MinAPIVersion: $min_api"
echo "Client APIVersion: $client_api"
[[ $api =~ ^([0-9]+)\.([0-9]+)$ ]] || fail "invalid server API version: $api"
((10#${BASH_REMATCH[1]} > 1 || (10#${BASH_REMATCH[1]} == 1 && 10#${BASH_REMATCH[2]} >= 42))) \
  || fail "server API $api is below 1.42"
echo "SERVER API: $api"
# The fail-closed probe uses a version one minor below the daemon minimum.
[[ $min_api =~ ^1\.([0-9]+)$ ]] || fail "invalid server minimum API version: $min_api"
((10#${BASH_REMATCH[1]} > 0)) || fail "no API version below minimum $min_api"
readonly probe_api="1.$((10#${BASH_REMATCH[1]} - 1))"
readonly pinned_api=1.42
((10#${BASH_REMATCH[1]} <= 42)) \
  || fail "daemon minimum $min_api exceeds the pinned request API $pinned_api"

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
    "{{range .Mounts}}{{if eq .Destination \"${2:-/fixture}\"}}{{.Name}}{{end}}{{end}}" "$1"
}
label_keys() {
  inner 10 volume inspect --format \
    "{{range \$key, \$value := .Labels}}{{println \$key}}{{end}}" "$1"
}
# Extra fixtures: a never-started holder, a volume-less container, and an
# anonymous volume with local-driver options whose container is removed without -v.
inner 20 create --name created-only -v /fixture "$FIXTURE_IMAGE" true >/dev/null
inner 20 run -d --name plain "$FIXTURE_IMAGE" sleep 600 >/dev/null
inner 30 run --name opts \
  --mount type=volume,dst=/opt,volume-opt=type=tmpfs,volume-opt=device=tmpfs \
  "$FIXTURE_IMAGE" true >/dev/null
options=$(volume_of opts /opt)
created=$(volume_of created-only)
for volume in "$options" "$created"; do
  [[ $volume =~ ^[0-9a-f]{64}$ ]] || fail "invalid anonymous volume ID: $volume"
  [[ $(label_keys "$volume") == com.docker.volume.anonymous ]] \
    || fail "volume $volume is not anonymous-labelled"
done
# Omit -v: the options volume must outlive its container.
inner 20 rm opts >/dev/null
[[ $(inner 10 inspect --format '{{.State.Status}}' created-only) == created ]] \
  || fail 'created-only holder is not in created state'
[[ $(inner 10 inspect --format '{{.State.Status}}' plain) == running ]] \
  || fail 'plain container is not running'
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
for volume in "$unreferenced" named-keep "$running" "$stopped" "$created" "$options"; do
  inner 10 volume inspect "$volume" >/dev/null
done
# Orphan created immediately before the prune: removal must not wait for any age.
inner 20 run --name fresh -v /fixture "$FIXTURE_IMAGE" \
  sh -c 'echo fixture > /fixture/proof' >/dev/null
fresh=$(volume_of fresh)
[[ $fresh =~ ^[0-9a-f]{64}$ ]] || fail "invalid anonymous volume ID: $fresh"
[[ $(label_keys "$fresh") == com.docker.volume.anonymous ]] || fail "volume $fresh is not anonymous"
inner 20 rm fresh >/dev/null
inner 10 volume inspect "$fresh" >/dev/null

# A successful list proves absence; failed inspect could mean a broken daemon.
volumes=
snapshot_volumes() { volumes=$(inner 10 volume ls --format '{{.Name}}'); }
contains_volume() {
  local wanted=$1 existing
  while IFS= read -r existing; do
    [[ $existing != "$wanted" ]] || return 0
  done <<< "$volumes"
  return 1
}
# "METHOD /path" of each volumes/prune request the owned daemon's debug log
# recorded. Startup route-registration lines carry no request-url field.
prune_paths() {
  local log line method url
  log=$(outer 20 logs "$daemon" 2>&1) || fail 'cannot read the disposable daemon log'
  while IFS= read -r line; do
    [[ -n $line ]] || continue
    method=${line#method=}
    method=${method%% *}
    url=${line##*request-url=}
    echo "$method ${url%%\?*}"
  done < <(grep -Eo 'method=[A-Z]+ module=api request-url=[^ ]*/volumes/prune[^ ]*' <<< "$log" || true)
}
# Fail unless every recorded prune path is one of the allowed arguments.
only_prune_paths() {
  local path allowed ok paths
  paths=$(prune_paths)
  while IFS= read -r path; do
    [[ -n $path ]] || continue
    ok=0
    for allowed in "$@"; do
      if [[ $path == "$allowed" ]]; then ok=1; fi
    done
    ((ok)) || fail "unexpected volumes/prune request path: $path"
  done <<< "$paths"
}

# Fail-closed probe: a request API below the daemon minimum must be rejected,
# with no retry at another version and nothing removed.
if probe_out=$(inner_pinned 30 "$probe_api" volume prune -f 2>&1); then
  fail "prune with request API $probe_api unexpectedly succeeded: $probe_out"
fi
[[ $probe_out == *'too old'* ]] || fail "probe error is not the too-old rejection: $probe_out"
echo "PROBE: request API $probe_api (daemon minimum $min_api) rejected: $probe_out"
snapshot_volumes
for volume in "$unreferenced" "$fresh" named-keep "$running" "$stopped" "$created" "$options"; do
  contains_volume "$volume" || fail "probe removed volume $volume"
done
only_prune_paths "POST /v$probe_api/volumes/prune"

# Exact command under test (the product leg's argv and pinned request API),
# routed exclusively to the disposable inner daemon.
inner_pinned 30 "$pinned_api" volume prune -f

snapshot_volumes
for volume in "$unreferenced" "$fresh"; do
  if contains_volume "$volume"; then fail "unreferenced anonymous volume $volume survived"; fi
done
for volume in named-keep "$running" "$stopped" "$created" "$options"; do
  contains_volume "$volume" || fail "protected volume $volume was removed"
  inner 10 volume inspect "$volume" >/dev/null
done
[[ $(inner 10 inspect --format '{{.State.Status}}' running) == running ]] \
  || fail 'running holder changed state'
[[ $(inner 10 inspect --format '{{.State.Status}}' stopped) == exited ]] \
  || fail 'stopped holder changed state'
[[ $(inner 10 inspect --format '{{.State.Status}}' plain) == running ]] \
  || fail 'volume-less container changed state'
[[ $(inner 10 inspect --format '{{.State.Status}}' created-only) == created ]] \
  || fail 'created-only holder changed state'
[[ $(label_keys "$options") == com.docker.volume.anonymous ]] \
  || fail 'options volume lost its anonymous label'

# The daemon log must show the pinned-version prune and no other prune path.
only_prune_paths "POST /v$probe_api/volumes/prune" "POST /v$pinned_api/volumes/prune"
final_paths=$(prune_paths)
grep -Fxq "POST /v$pinned_api/volumes/prune" <<< "$final_paths" \
  || fail "daemon log lacks POST /v$pinned_api/volumes/prune"
echo "DAEMON LOG: POST /v$pinned_api/volumes/prune recorded; no other prune version succeeded"

# Removal-first: reusing the removed orphan's ID yields a fresh, empty,
# non-anonymous volume rather than the old data.
inner 30 run --rm -v "$fresh:/fixture" "$FIXTURE_IMAGE" \
  sh -c 'test ! -e /fixture/proof' || fail 'removed orphan data survived the prune'
[[ -z $(label_keys "$fresh") ]] || fail 're-created volume carries the anonymous label'
echo 'PASS: orphans (old and just-created) removed at request API 1.42; named, running,' \
  'stopped, never-started and options references retained; too-old API failed closed;' \
  'removed-orphan ID re-created empty and unlabelled'
