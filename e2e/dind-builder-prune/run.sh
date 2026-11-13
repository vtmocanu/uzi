#!/usr/bin/env bash
# Disposable DinD proof for the #4291 builder-cache prune leg. See README.md.
#
# Safety: the worker's own Docker daemon is shared. This script only pulls images and
# creates/starts/execs/cps/inspects/logs its OWN containers (dbp-test-<hex>-*), and removes
# them by exact name after verifying the ownership label. Every prune/build/system command
# runs against an owned nested daemon (docker exec into it, or its own TCP address).
# No clock is altered.
set -euo pipefail

# The daemon image pins match deploy/chart/values.yaml; the alpine fixture pin matches
# e2e/dind-recycle/run.sh (the chart does not carry an alpine pin).
readonly ROOTFUL_IMAGE='docker:29-dind@sha256:5efed980cba3fc126cf54e21a5a6ff8849d05b6e0623d6e7612f48e9cd6cd17e'
readonly ROOTLESS_IMAGE='docker:29-dind-rootless@sha256:e17fa54c2ffd511d8407c746eec77f7814e6f74fe20caf822dad1870599984c0'
readonly FIXTURE_IMAGE='alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6'
readonly OWNER_LABEL='dbp-test.owner'
readonly CLIENT_BIN='/usr/bin/docker'
readonly BUILDX_PLUGIN='/usr/libexec/docker/cli-plugins/docker-buildx'
readonly INNER_PATH='/spike/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
# TEST-ONLY scale knobs. The production floor is 24h and the production cap is 4GiB.
readonly FLOOR_S="${DBP_FLOOR_S:-25}"
readonly TMPFS_MB="${DBP_TMPFS_MB:-320}"
# The frozen production argv, an independent literal (the driver compares the product
# constant against its own copy too).
readonly -a PROD_ARGV=(builder prune -f --filter type=regular --filter until=24h --max-used-space 4GiB)

for tool in timeout docker jq node od tr awk curl sed; do
  command -v "$tool" >/dev/null || { echo "FAIL: $tool is required" >&2; exit 1; }
done
[[ -x $CLIENT_BIN ]] || { echo "FAIL: worker docker client $CLIENT_BIN is required" >&2; exit 1; }
[[ -f $BUILDX_PLUGIN ]] || { echo "FAIL: buildx plugin $BUILDX_PLUGIN is required" >&2; exit 1; }

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
readonly HERE REPO
token="$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')"
readonly token
[[ $token =~ ^[0-9a-f]{12}$ ]] || exit 1
OUT="${DBP_OUT:-$REPO/.uzi/scratch/dbp-$token}"
mkdir -p "$OUT"
readonly OUT
readonly deadline=$((SECONDS + ${DBP_DEADLINE_S:-1500}))

expect_red="${DBP_EXPECT_RED:-}"
case "$expect_red" in
  '' | switch-off) ;;
  *) echo "FAIL: DBP_EXPECT_RED must be empty or switch-off" >&2; exit 1 ;;
esac
if [[ $expect_red == switch-off ]]; then
  postures="${DBP_POSTURES:-rootful}"
  phases="${DBP_PHASES:-pressure}"
else
  postures="${DBP_POSTURES:-rootful rootless}"
  phases="${DBP_PHASES:-points real pressure}"
fi
has_phase() { [[ " $phases " == *" $1 "* ]]; }

containers=()
failures=0
blocked=0
results=()

fail() { echo "FAIL: $*" >&2; exit 1; }
record() {
  results+=("$1")
  echo "$1"
  case $1 in PASS*) ;; *) failures=$((failures + 1)) ;; esac
}
pass() { record "PASS [$P] $*"; }
failc() { record "FAIL [$P] $*"; }

cleanup() {
  local original=$? name owner cleanup_rc=0
  trap - EXIT
  for name in "${containers[@]}"; do
    # A timed-out create may have succeeded. Verify ownership even on that path.
    if owner=$(timeout --kill-after=2s 10s docker inspect \
      --format "{{index .Config.Labels \"$OWNER_LABEL\"}}" "$name" 2>&1); then
      if [[ $owner == "$token" ]]; then
        if timeout --kill-after=2s 60s docker rm -fv "$name" >/dev/null; then
          echo "CLEANUP removed $name"
        else
          echo "FAIL: cleanup could not remove $name" >&2
          cleanup_rc=1
        fi
      else
        echo "FAIL: cleanup ownership mismatch for $name" >&2
        cleanup_rc=1
      fi
    elif [[ $owner == *'No such object'* || $owner == *'No such container'* ]]; then
      echo "CLEANUP $name does not exist (never created or already removed)"
    else
      # A daemon error or a timeout is not proof of absence: the container may still exist.
      echo "FAIL: cannot verify cleanup ownership for $name: ${owner:-no output}" >&2
      cleanup_rc=1
    fi
  done
  if ((original != 0)); then exit "$original"; fi
  exit "$cleanup_rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Each Docker call has a cap within the shared overall deadline.
outer() {
  local cap=$1 remaining=$((deadline - SECONDS))
  shift
  ((remaining > 0)) || fail 'overall deadline exceeded'
  ((cap <= remaining)) || cap=$remaining
  timeout --kill-after=2s "${cap}s" docker "$@"
}
# The staged client inside the nested container, against its own unix socket.
inner() {
  local cap=$1
  shift
  outer "$cap" exec -u "$P_USER" "$P_NAME" env -i PATH="$INNER_PATH" \
    HOME=/spike/home DOCKER_CONFIG=/spike/cfg DOCKER_BUILDKIT=1 \
    docker --host "$P_SOCK" "$@"
}
sh_in() {
  local cap=$1 script=$2
  shift 2
  outer "$cap" exec -u "$P_USER" "$P_NAME" sh -c "$script" _ "$@"
}

# ---- posture lifecycle -------------------------------------------------------------------

P='-'
P_NAME='' P_IMAGE='' P_USER='' P_SOCK='' P_DATA='' P_IP='' PO=''

start_posture() {
  P=$1
  P_NAME="dbp-test-$token-$P"
  PO="$OUT/$P"
  mkdir -p "$PO"
  local -a create=(create --name "$P_NAME" --label "$OWNER_LABEL=$token" --privileged --network bridge -e DOCKER_TLS_CERTDIR=)
  local tmpfs_opts="rw,exec,suid,dev,size=${TMPFS_MB}m"
  if [[ $P == rootful ]]; then
    P_IMAGE=$ROOTFUL_IMAGE P_USER=0 P_SOCK=unix:///var/run/docker.sock P_DATA=/var/lib/docker
    create+=(--tmpfs "$P_DATA:$tmpfs_opts" "$P_IMAGE"
      dockerd --debug --tls=false --host=unix:///var/run/docker.sock --host=tcp://0.0.0.0:2375)
  else
    P_IMAGE=$ROOTLESS_IMAGE P_USER=1000 P_SOCK=unix:///run/user/1000/docker.sock P_DATA=/home/rootless/.local/share/docker
    create+=(--tmpfs "$P_DATA:$tmpfs_opts,uid=1000,gid=1000,mode=0700" "$P_IMAGE"
      --debug --tls=false --host=tcp://0.0.0.0:2375)
  fi
  if ! outer 10 image inspect "$P_IMAGE" >/dev/null 2>&1; then
    outer 300 pull "$P_IMAGE" >/dev/null 2>&1 || { echo "pull of $P_IMAGE failed"; return 1; }
  fi
  containers+=("$P_NAME")
  outer 30 "${create[@]}" >/dev/null || { echo "create of $P_NAME failed"; return 1; }
  outer 30 start "$P_NAME" >/dev/null || { echo "start of $P_NAME failed"; return 1; }
  outer 20 cp "$CLIENT_BIN" "$P_NAME:/opt/docker-client" || { echo 'cp of the docker client failed'; return 1; }
  outer 20 cp "$BUILDX_PLUGIN" "$P_NAME:/opt/docker-buildx" || { echo 'cp of buildx failed'; return 1; }
  outer 20 exec -u 0 "$P_NAME" sh -c 'mkdir -p /spike/bin /spike/cfg/cli-plugins /spike/home /spike/ctx &&
    mv /opt/docker-client /spike/bin/docker && mv /opt/docker-buildx /spike/cfg/cli-plugins/docker-buildx &&
    chmod 755 /spike/bin/docker /spike/cfg/cli-plugins/docker-buildx && chown -R '"$P_USER:$P_USER"' /spike' \
    || { echo 'staging the client failed'; return 1; }
  local i ready=0
  for ((i = 0; i < 60; i++)); do
    if inner 5 info >/dev/null 2>&1; then ready=1; break; fi
    sleep 1
  done
  if ((!ready)); then
    outer 10 logs --tail 40 "$P_NAME" 2>&1 | sed 's/^/  dockerd| /'
    echo 'nested daemon did not become ready'
    return 1
  fi
  P_IP=$(outer 10 inspect --format '{{.NetworkSettings.Networks.bridge.IPAddress}}' "$P_NAME") || return 1
  [[ $P_IP =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "bad container IP '$P_IP'"; return 1; }
  return 0
}

print_versions() {
  echo "--- versions [$P]"
  echo "worker client: $("$CLIENT_BIN" --version)"
  echo "worker buildx: $("$CLIENT_BIN" buildx version 2>&1 | head -1)"
  inner 15 version --format \
    'nested client={{.Client.Version}} clientAPI={{.Client.APIVersion}} | server={{.Server.Version}} serverAPI={{.Server.APIVersion}} serverMinAPI={{.Server.MinAPIVersion}} commit={{.Server.GitCommit}}'
  inner 15 buildx version
  inner 30 buildx inspect | grep -E '^(Name|Driver|Status|BuildKit version)|BuildKit version' | head -5 || true
  inner 15 info --format 'storage driver={{.Driver}} security={{range .SecurityOptions}}{{.}} {{end}}'
  echo "nested daemon: unix $P_SOCK and tcp://$P_IP:2375 (unpublished, tls disabled), --debug on"
  echo "data root $P_DATA is a size-limited tmpfs of ${TMPFS_MB} MB"
}

# ---- records -----------------------------------------------------------------------------

CUR=''
df_snap() {
  CUR="$PO/df-$1.json"
  inner 90 system df -v --format json >"$CUR"
  jq -e '.BuildCache' "$CUR" >/dev/null || fail "df json for $1 has no BuildCache"
}
show_records() {
  df_snap "$1"
  echo "--- records [$P/$1] (ID type shared inuse size lastUsedAt(UTC) description)"
  jq -r '.BuildCache[] | [(.ID | rtrimstr("*")), .CacheType, "shared=" + (.Shared | tostring),
    "inuse=" + (.InUse | tostring), (.Size | tostring), ((.LastUsedAt // "") | .[11:23]),
    (.Description | .[0:56])] | @tsv' "$CUR" | sort -k6
}
rec_id() { # type needle -> the first matching record ID in CUR
  jq -r --arg t "$1" --arg n "$2" '[.BuildCache[] | select(.CacheType == $t and (.Description | contains($n)))] | .[0].ID // empty | rtrimstr("*")' "$CUR"
}
# Like rec_id but only a record that is InUse (BuildKit re-creates a record when a build reuses
# it, so a held record's ID is resolved while the holding build runs, not beforehand).
rec_id_inuse() {
  jq -r --arg t "$1" --arg n "$2" '[.BuildCache[] | select(.CacheType == $t and (.InUse | tostring == "true") and (.Description | contains($n)))] | .[0].ID // empty | rtrimstr("*")' "$CUR"
}
has_id() { jq -e --arg i "$1" 'any(.BuildCache[]; (.ID | rtrimstr("*")) == $i)' "$CUR" >/dev/null; }
ids_of() { jq -r '.BuildCache[] | .ID | rtrimstr("*")' "$CUR" | sort; }
rec_field() { # id jqfield
  jq -r --arg i "$1" ".BuildCache[] | select((.ID | rtrimstr(\"*\")) == \$i) | $2 | tostring" "$CUR"
}
assert_present() { # what id
  if [[ -z $2 ]]; then failc "$1: record id could not be resolved"; elif has_id "$2"; then pass "$1: $2 present"; else failc "$1: $2 was deleted"; fi
}
assert_absent() {
  if [[ -z $2 ]]; then failc "$1: record id could not be resolved"; elif has_id "$2"; then failc "$1: $2 survived"; else pass "$1: $2 deleted"; fi
}

# ---- prune, build, daemon log helpers ------------------------------------------------------

PRUNE_RC=0
PRUNE_OUT=''
run_prune() { # label scaled(0|1) args...
  local label=$1 scaled=$2
  shift 2
  if ((scaled)); then
    echo ">>> PRUNE[$P/$label] TEST-ONLY scaled argv: docker $*"
  else
    echo ">>> PRUNE[$P/$label] production argv: docker $*"
  fi
  PRUNE_RC=0
  PRUNE_OUT=$(inner 150 "$@" 2>&1) || PRUNE_RC=$?
  printf '%s\n' "$PRUNE_OUT" | sed 's/^/    | /' | head -n 14
  echo ">>> PRUNE[$P/$label] rc=$PRUNE_RC"
}
scaled_argv() { SCALED=(builder prune -f --filter type=regular --filter "until=${FLOOR_S}s" --max-used-space "$1"); }

mkctx() { # name dockerfile-text (printf %b)
  sh_in 20 'mkdir -p "/spike/ctx/$1" && printf "%b" "$2" > "/spike/ctx/$1/Dockerfile"' "$1" "$2"
}
build_ctx() { # label ctx args...
  local label=$1 ctx=$2 rc=0
  shift 2
  inner 150 build --progress=quiet "$@" "/spike/ctx/$ctx" >"$PO/build-$label.out" 2>&1 || rc=$?
  if ((rc != 0)); then tail -n 20 "$PO/build-$label.out"; fail "[$P] build $label rc=$rc"; fi
}
base_df() { printf 'FROM %s\\nRUN %s\\n' "$FIXTURE_IMAGE" "$1"; }
cm_df() { printf 'FROM %s\\nRUN --mount=type=cache,target=/c,id=%s %s\\n' "$FIXTURE_IMAGE" "$1" "$2"; }
dd_cmd() { printf 'echo %s && dd if=/dev/urandom of=%s bs=1M count=%s' "$1" "$2" "$3"; }

start_hold() { # ctx sleep-seconds
  rm -f "$PO/hold-$1.rc" "$PO/build-hold-$1.out"
  (
    rc=0
    inner 200 build --progress=plain --output type=cacheonly "/spike/ctx/$1" >"$PO/build-hold-$1.out" 2>&1 || rc=$?
    echo "$rc" >"$PO/hold-$1.rc"
  ) &
  local i
  for ((i = 0; i < 60; i++)); do
    if grep -q "sleep $2" "$PO/build-hold-$1.out" 2>/dev/null; then break; fi
    sleep 1
  done
  grep -q "sleep $2" "$PO/build-hold-$1.out" 2>/dev/null || fail "[$P] hold build did not reach its sleep step"
  grep -q 'CACHED' "$PO/build-hold-$1.out" || fail "[$P] hold build did not reuse the cached held layer"
}
finish_hold() { # ctx -> prints the hold build rc
  local i
  for ((i = 0; i < 120; i++)); do
    [[ -s $PO/hold-$1.rc ]] && break
    sleep 1
  done
  [[ -s $PO/hold-$1.rc ]] || fail "[$P] hold build did not finish"
  cat "$PO/hold-$1.rc"
}

daemon_log() { outer 30 logs "$P_NAME" 2>&1; }
log_mark() { daemon_log | wc -l; }
# Build-prune-shaped API requests the nested daemon logged after line $1.
prune_reqs_since() {
  { daemon_log | tail -n +"$(($1 + 1))" | grep -Eo 'request-url=[^ ]+' | sed 's/?.*//' \
    | grep -E '/(build/prune|grpc|session)$' | sort | uniq -c; } || true
}
all_reqs_since() {
  { daemon_log | tail -n +"$(($1 + 1))" | grep -Eo 'request-url=[^ ]+' | sed 's/?.*//' | sort | uniq -c; } || true
}

pressure_pct() {
  local out fr bl bf
  out=$(sh_in 20 'stat -f -c "%S %b %f" "$1"' "$P_DATA")
  read -r fr bl bf <<<"$out"
  awk -v fr="$fr" -v bl="$bl" -v bf="$bf" 'BEGIN { printf "%.1f", (bl - bf) * 100 / bl }'
}
pct_ge() { awk -v p="$1" -v t="$2" 'BEGIN { exit !(p >= t) }'; }

run_driver() { # cap args...
  local cap=$1 remaining=$((deadline - SECONDS))
  shift
  ((remaining > 0)) || fail 'overall deadline exceeded'
  ((cap <= remaining)) || cap=$remaining
  [[ -d $REPO/agent/node_modules ]] || fail 'agent/node_modules is missing (the worker installs it at run start)'
  (cd "$REPO/agent" && timeout --kill-after=5s "${cap}s" node --import tsx ../e2e/dind-builder-prune/drive-leg.mts "$@")
}

# ---- phase: points 1-6 and the isolated missing/broken plugin cases ------------------------

NP=''
setup_noplug() {
  NP="$P_NAME-noplug"
  containers+=("$NP")
  if ! outer 10 image inspect "$FIXTURE_IMAGE" >/dev/null 2>&1; then outer 120 pull "$FIXTURE_IMAGE" >/dev/null; fi
  outer 30 create --name "$NP" --label "$OWNER_LABEL=$token" --network bridge "$FIXTURE_IMAGE" sleep 1800 >/dev/null
  outer 30 start "$NP" >/dev/null
  outer 20 cp "$CLIENT_BIN" "$NP:/opt/docker"
  local dirs
  dirs=$(outer 20 exec "$NP" sh -c 'for d in /usr/local/lib/docker/cli-plugins /usr/local/libexec/docker/cli-plugins /usr/lib/docker/cli-plugins /usr/libexec/docker/cli-plugins; do
    if [ -e "$d" ]; then echo "present: $d"; exit 1; fi; done; echo none-of-the-four-cli-plugin-dirs-exist') \
    || fail "[$P] the plugin-less container has a docker cli-plugins directory: $dirs"
  echo "plugin-less container $NP: $dirs"
}
np_run() { # cap cfgdir buildkit(0|1) argv...
  local cap=$1 cfg=$2 bk=$3
  shift 3
  local -a envs=(PATH=/usr/bin:/bin HOME="$cfg" DOCKER_CONFIG="$cfg" "DOCKER_HOST=tcp://$P_IP:2375")
  if ((bk)); then envs+=(DOCKER_BUILDKIT=1); fi
  outer "$cap" exec "$NP" env -i "${envs[@]}" /opt/docker "$@"
}

REFUSED_TEXT=''
plugin_case() { # name controlid setupkind(none|nonexec|exec)
  local name=$1 control=$2 kind=$3 cfg mark rc out text reqs
  cfg=$(outer 10 exec "$NP" mktemp -d)
  case $kind in
    nonexec) outer 10 exec "$NP" sh -c 'mkdir -p "$1/cli-plugins" && printf "garbage, not a plugin\n" > "$1/cli-plugins/docker-buildx" && chmod 644 "$1/cli-plugins/docker-buildx"' _ "$cfg" ;;
    exec) outer 10 exec "$NP" sh -c 'mkdir -p "$1/cli-plugins" && printf "garbage, not a plugin\n" > "$1/cli-plugins/docker-buildx" && chmod 755 "$1/cli-plugins/docker-buildx"' _ "$cfg" ;;
    *) ;;
  esac
  echo "--- plugin case [$P/$name] fresh empty DOCKER_CONFIG=$cfg (inside $NP), DOCKER_BUILDKIT=1"
  mark=$(log_mark)
  scaled_argv 1MB
  local variant
  for variant in frozen scaled; do
    rc=0
    if [[ $variant == frozen ]]; then
      echo ">>> CASE[$name/frozen] production argv: docker ${PROD_ARGV[*]}"
      out=$(np_run 60 "$cfg" 1 "${PROD_ARGV[@]}" 2>&1) || rc=$?
    else
      echo ">>> CASE[$name/scaled] TEST-ONLY scaled argv: docker ${SCALED[*]}"
      out=$(np_run 60 "$cfg" 1 "${SCALED[@]}" 2>&1) || rc=$?
    fi
    text=$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-400)
    echo "    rc=$rc text: $text"
    if ((rc != 0)); then pass "$name/$variant exits non-zero (rc=$rc)"; else failc "$name/$variant exited 0"; fi
    if [[ $out == *"$REFUSED_TEXT"* ]]; then
      pass "$name/$variant text contains the product refused match string '$REFUSED_TEXT'"
    else
      failc "$name/$variant text does NOT contain the product refused match string '$REFUSED_TEXT': $text"
    fi
  done
  reqs=$(prune_reqs_since "$mark")
  if [[ -z $reqs ]]; then
    pass "$name: nested daemon debug log shows no /build/prune, /grpc or /session request during the case"
  else
    failc "$name: daemon logged prune-shaped requests during the case: $reqs"
  fi
  df_snap "plugin-$name"
  assert_present "$name: deletable control survived the failed prunes (scaled argv would have deleted it)" "$control"
}

phase_points() {
  echo "===== [$P] POINTS 1-6 (TEST-ONLY scaled floor ${FLOOR_S}s; every check has a positive control deleted in the same prune)"
  show_records fresh
  local mark
  mark=$(log_mark)
  run_prune exact-fresh 0 "${PROD_ARGV[@]}"
  if ((PRUNE_RC == 0)); then pass 'exact production argv on a fresh cache exits 0'; else failc "exact production argv rc=$PRUNE_RC"; fi
  local ref
  ref=$(prune_reqs_since "$mark")
  if [[ -n $ref ]]; then
    pass "log detector sees a real prune: $(printf '%s' "$ref" | tr '\n' ';')"
  else
    failc 'log detector saw no prune request for a real prune; the no-request assertions would be blind'
    echo "requests logged: $(all_reqs_since "$mark" | tr '\n' ';')"
  fi

  echo "--- fixtures (all built now; they age together)"
  mkctx held "$(base_df "$(dd_cmd dbp-held /h 1)")"
  mkctx hold "$(base_df "$(dd_cmd dbp-held /h 1)")RUN echo dbp-hold && sleep 60\\n"
  mkctx ctrl "$(base_df "$(dd_cmd dbp-ctrl /c 1)")"
  mkctx recent "$(base_df "$(dd_cmd dbp-recent /r 1)")"
  mkctx cm "$(cm_df dbpcm "$(dd_cmd dbp-cm /c/f 2)")"
  mkctx keep "$(base_df 'echo dbp-keep > /proof')"
  mkctx x1 "$(base_df "$(dd_cmd dbp-x1 /x 2)")"
  mkctx x2 "$(base_df "$(dd_cmd dbp-x2 /x 2)")"
  mkctx cabs "$(base_df "$(dd_cmd dbp-cabs /x 1)")"
  mkctx cbn "$(base_df "$(dd_cmd dbp-cbn /x 1)")"
  mkctx cbe "$(base_df "$(dd_cmd dbp-cbe /x 1)")"
  local c
  for c in held ctrl recent cm x1 x2 cabs cbn cbe; do build_ctx "$c" "$c" --output type=cacheonly; done
  build_ctx keep keep -t dbp/keep
  local t_fix=$SECONDS
  show_records built
  local id_held id_ctrl id_recent id_cm id_x1 id_x2 id_cabs id_cbn id_cbe
    id_ctrl=$(rec_id regular 'echo dbp-ctrl &&')
  id_recent=$(rec_id regular 'echo dbp-recent &&')
  id_cm=$(rec_id exec.cachemount 'echo dbp-cm &&')
  id_x1=$(rec_id regular 'echo dbp-x1 &&')
  id_x2=$(rec_id regular 'echo dbp-x2 &&')
  id_cabs=$(rec_id regular 'echo dbp-cabs &&')
  id_cbn=$(rec_id regular 'echo dbp-cbn &&')
  id_cbe=$(rec_id regular 'echo dbp-cbe &&')
  local shared_ids
  shared_ids=$(jq -r '.BuildCache[] | select(.Shared | tostring == "true") | .ID | rtrimstr("*")' "$CUR")
  echo "IDs: ctrl=$id_ctrl recent=$id_recent cachemount=$id_cm x1=$id_x1 x2=$id_x2 control-absent=$id_cabs control-nonexec=$id_cbn control-garbage-exec=$id_cbe"
  echo "shared (tagged image) records: $(printf '%s' "$shared_ids" | tr '\n' ' ')"
  [[ -n $id_ctrl && -n $id_recent && -n $id_cm && -n $id_x1 && -n $id_x2 && -n $id_cabs && -n $id_cbn && -n $id_cbe ]] \
    || fail "[$P] a fixture record could not be resolved"
  [[ -n $shared_ids ]] || fail "[$P] the tagged image produced no shared record"

  setup_noplug
  local until_t=$((t_fix + FLOOR_S + 3))
  while ((SECONDS < until_t)); do sleep 1; done

  echo "--- hold: a concurrent build keeps the held record in use"
  start_hold hold 60
  show_records during-hold
  id_held=$(rec_id_inuse regular 'echo dbp-held &&')
  if [[ -n $id_held ]]; then pass "held record $id_held reports InUse=true during the hold build"; else failc 'no InUse record for the held layer during the hold build'; fi

  echo "===== [$P] missing / broken buildx plugin (isolated plugin-less client, DOCKER_BUILDKIT=1 pin)"
  plugin_case absent "$id_cabs" none
  plugin_case broken-nonexec "$id_cbn" nonexec
  plugin_case broken-garbage-exec "$id_cbe" exec
  echo "--- CONTRAST (recorded, not asserted): the same absent case WITHOUT the DOCKER_BUILDKIT=1 pin, frozen argv"
  local cfg crc=0 cout
  cfg=$(outer 10 exec "$NP" mktemp -d)
  mark=$(log_mark)
  cout=$(np_run 60 "$cfg" 0 "${PROD_ARGV[@]}" 2>&1) || crc=$?
  echo "CONTRAST unpinned absent: rc=$crc text: $(printf '%s' "$cout" | tr '\n' ' ' | cut -c1-400)"
  echo "CONTRAST requests logged: $(all_reqs_since "$mark" | tr '\n' ';')"

  echo "===== [$P] EXACT production argv with data present, then the scaled prunes"
  df_snap before-exact
  local before_ids after_ids
  before_ids=$(ids_of)
  run_prune exact-loaded 0 "${PROD_ARGV[@]}"
  if ((PRUNE_RC == 0)); then pass 'exact production argv with data present exits 0'; else failc "exact-loaded rc=$PRUNE_RC"; fi
  df_snap after-exact
  after_ids=$(ids_of)
  if [[ $before_ids == "$after_ids" ]]; then pass 'exact production argv deleted nothing (every record is far younger than 24h)'; else failc 'exact production argv deleted a young record'; fi

  scaled_argv 4GiB
  run_prune capabove 1 "${SCALED[@]}"
  df_snap after-capabove
  if ((PRUNE_RC == 0)); then pass 'point 6: scaled floor + type + cap 4GiB accepted (rc 0)'; else failc "capabove rc=$PRUNE_RC"; fi
  assert_present 'point 6: cap above usage deletes nothing even though the record is old and regular (cap participates in the AND)' "$id_x1"
  assert_present 'point 6: cap above usage deletes nothing (second old record)' "$id_ctrl"

  echo "--- bump recent now so its last use is inside the floor"
  build_ctx bump-recent recent --output type=cacheonly
  show_records before-P1
  id_recent=$(rec_id regular 'echo dbp-recent &&')
  echo "IDs resolved after the hold started and recent was bumped: held=$id_held recent=$id_recent"
  scaled_argv 1MB
  run_prune P1 1 "${SCALED[@]}"
  df_snap after-P1
  show_records after-P1
  if ((PRUNE_RC == 0)); then pass 'point 6: combined type + until + --max-used-space accepted (rc 0)'; else failc "P1 rc=$PRUNE_RC"; fi
  assert_absent 'point 1: old unused regular record is deleted (also the positive control for points 2-6)' "$id_ctrl"
  assert_absent 'point 1: second old unused regular record is deleted' "$id_x1"
  assert_absent 'point 1: third old unused regular record is deleted' "$id_x2"
  assert_present 'point 2: record held by a concurrent build survives while the unheld same-age control was deleted' "$id_held"
  assert_present 'point 3: exec.cachemount survives while the old regular control was deleted' "$id_cm"
  assert_present 'point 4: record used within the floor survives while the older control was deleted' "$id_recent"
  assert_absent 'point 6 (AND): old + regular + over the cap is deleted' "$id_cabs"
  assert_absent 'plugin control (broken non-executable case) is deletable by the scaled prune' "$id_cbn"
  assert_absent 'plugin control (broken executable case) is deletable by the scaled prune' "$id_cbe"
  local s
  for s in $shared_ids; do assert_present 'point 5: tagged image (shared) record survives' "$s"; done
  local hold_rc
  hold_rc=$(finish_hold hold)
  if [[ $hold_rc == 0 ]]; then pass 'point 2: the held build then completed rc=0'; else failc "held build rc=$hold_rc"; fi
  local keep_out keep_rc=0
  inner 20 image ls dbp/keep
  keep_out=$(inner 60 run --rm dbp/keep cat /proof 2>&1) || keep_rc=$?
  if ((keep_rc == 0)) && [[ $keep_out == dbp-keep ]]; then pass "point 5: docker run --rm dbp/keep works after the prune (proof=$keep_out)"; else failc "point 5: run of the tagged image rc=$keep_rc out=$keep_out"; fi
  keep_rc=0
  inner 60 run --rm dbp/keep true || keep_rc=$?
  if ((keep_rc == 0)); then pass 'point 5: docker run --rm dbp/keep true rc 0'; else failc "docker run true rc=$keep_rc"; fi

  # Snapshot before P1b so the later assert_absent on the held record is not vacuous.
  df_snap before-P1b
  assert_present 'point 2 control precondition: formerly held record is still present before P1b' "$id_held"
  echo "--- control for point 2: once the hold released (and its own child record aged past the floor), the same prune deletes the formerly held record"
  local t_released=$SECONDS
  while ((SECONDS < t_released + FLOOR_S + 3)); do sleep 1; done
  run_prune P1b 1 "${SCALED[@]}"
  df_snap after-P1b
  assert_absent 'point 2 control: formerly held record is deleted once unheld (survival was due to the hold)' "$id_held"
  assert_present 'point 3: exec.cachemount still survives' "$id_cm"
  for s in $shared_ids; do assert_present 'point 5: tagged image (shared) record still survives' "$s"; done
  before_ids=$(ids_of)
  run_prune exact-final 0 "${PROD_ARGV[@]}"
  if ((PRUNE_RC == 0)); then pass 'exact production argv (final) exits 0'; else failc "exact-final rc=$PRUNE_RC"; fi
  df_snap after-final
  after_ids=$(ids_of)
  if [[ $before_ids == "$after_ids" ]]; then pass 'exact production argv (final) deleted nothing'; else failc 'exact production argv (final) deleted a young record'; fi
}

# ---- phase: the real controller exec path ----------------------------------------------------

phase_real() {
  echo "===== [$P] REAL PATH: the production argv through the real controller exec path over tcp://$P_IP:2375"
  local reach
  if ! reach=$(timeout 10s curl -fsS "http://$P_IP:2375/_ping" 2>&1); then
    echo "LIMITATION: nested daemon tcp://$P_IP:2375 is not reachable from this worker: $reach"
    failc 'real path cannot run: nested TCP unreachable from this worker (limitation, not faked)'
    return 0
  fi
  echo "nested TCP reachable from the worker: GET /_ping -> $reach"
  local mark rc=0
  mark=$(log_mark)
  run_driver 180 real --docker-host "tcp://$P_IP:2375" || rc=$?
  if ((rc == 0)); then pass 'real controller path: completed rc 0, abort group-killed and settled, no -unsettled log'; else failc "real path driver rc=$rc"; fi
  local reqs
  reqs=$(prune_reqs_since "$mark")
  if [[ -n $reqs ]]; then pass "nested daemon log shows the real-path prune request(s): $(printf '%s' "$reqs" | tr '\n' ';')"; else failc 'nested daemon log shows no prune request from the real path'; fi
}

# ---- phase: pressure relief ------------------------------------------------------------------

phase_pressure() {
  echo "===== [$P] PRESSURE (TEST-ONLY scaled floor ${FLOOR_S}s, scaled cap 64MB; data root is a ${TMPFS_MB} MB tmpfs)"
  mkctx pheld "$(base_df "$(dd_cmd dbp-pheld /h 1)")"
  mkctx phold "$(base_df "$(dd_cmd dbp-pheld /h 1)")RUN echo dbp-phold && sleep 50\\n"
  mkctx precent "$(base_df "$(dd_cmd dbp-precent /r 1)")"
  mkctx pcm "$(cm_df dbppcm "$(dd_cmd dbp-pcm /c/f 2)")"
  mkctx pkeep "$(base_df 'echo dbp-pkeep > /proof')"
  local c
  for c in pheld precent pcm; do build_ctx "$c" "$c" --output type=cacheonly; done
  build_ctx pkeep pkeep -t dbp/pkeep
  echo "--- fill: unshared regular cache (cacheonly builds, 24 MB of random data per layer, no tag)"
  local i=0 p
  while :; do
    p=$(pressure_pct)
    if pct_ge "$p" 86; then break; fi
    ((i < 20)) || fail "[$P] could not reach 86% data-root usage (at ${p}%)"
    mkctx "fill$i" "$(base_df "$(dd_cmd "dbp-fill-$i" /f 24)")"
    build_ctx "fill$i" "fill$i" --output type=cacheonly
    i=$((i + 1))
  done
  local t_fill=$SECONDS
  echo "fill done: $i fill layers, data root ${p}% used (>= 85% high watermark)"
  inner 30 system df | sed 's/^/    df| /' | head -n 8 || true
  show_records pressure-built
  local until_t=$((t_fill + FLOOR_S + 3))
  while ((SECONDS < until_t)); do sleep 1; done
  echo "--- hold + recent bump (the held record is old and InUse; recent is used inside the floor)"
  start_hold phold 50
  build_ctx bump-precent precent --output type=cacheonly
  show_records pressure-before
  local id_held id_cm id_recent shared_ids
  id_held=$(rec_id_inuse regular 'echo dbp-pheld &&')
  id_cm=$(rec_id exec.cachemount 'echo dbp-pcm &&')
  id_recent=$(rec_id regular 'echo dbp-precent &&')
  shared_ids=$(jq -r '.BuildCache[] | select(.Shared | tostring == "true") | .ID | rtrimstr("*")' "$CUR")
  echo "IDs: held=$id_held cachemount=$id_cm recent=$id_recent shared=$(printf '%s' "$shared_ids" | tr '\n' ' ')"
  [[ -n $id_held && -n $id_cm && -n $id_recent && -n $shared_ids ]] || fail "[$P] a pressure fixture record could not be resolved"
  local pre_pct
  pre_pct=$(pressure_pct)
  local mark
  mark=$(log_mark)
  local switch=${DBP_SWITCH:-on} drc=0
  if [[ $expect_red == switch-off ]]; then switch=off; fi
  [[ $switch == on || $switch == off ]] || fail 'DBP_SWITCH must be on or off'
  run_driver 240 pressure --docker-host "tcp://$P_IP:2375" --container "$P_NAME" --user "$P_USER" \
    --inner-sock "$P_SOCK" --data-path "$P_DATA" --floor-s "$FLOOR_S" --cap 64MB --switch "$switch" || drc=$?
  local post_pct
  post_pct=$(pressure_pct)
  echo "data root: ${pre_pct}% before, ${post_pct}% after (driver rc=$drc, switch=$switch)"
  df_snap pressure-after
  show_records pressure-after
  local reqs
  reqs=$(prune_reqs_since "$mark")
  if [[ $expect_red == switch-off ]]; then
    if ((drc == 3)); then pass "RED A OBSERVED: with the switch off there is no relief and the driver reported FAIL (rc 3); pressure ${pre_pct}% -> ${post_pct}%"; else failc "red A: driver rc=$drc, expected 3 (no relief)"; fi
    if pct_ge "$post_pct" 85; then pass "red A: data root still >= 85% (${post_pct}%)"; else failc "red A: data root fell to ${post_pct}% without the leg"; fi
    if [[ -z $reqs ]]; then pass 'red A: the nested daemon logged no prune request while the leg was off'; else failc "red A: unexpected prune requests: $reqs"; fi
  else
    if ((drc == 0)); then pass "pressure relief: controller completed and the post sample is below 75% (${pre_pct}% -> ${post_pct}%)"; else failc "pressure relief driver rc=$drc"; fi
    if ! pct_ge "$post_pct" 75; then pass "post-prune own df ${post_pct}% < 75%"; else failc "post-prune own df ${post_pct}% >= 75%"; fi
    if [[ -n $reqs ]]; then pass "nested daemon log shows the prune request(s): $(printf '%s' "$reqs" | tr '\n' ';')"; else failc 'nested daemon log shows no prune request'; fi
  fi
  assert_present 'pressure: held record survives' "$id_held"
  assert_present 'pressure: exec.cachemount record survives' "$id_cm"
  assert_present 'pressure: recently used record survives' "$id_recent"
  for c in $shared_ids; do assert_present 'pressure: tagged image (shared) record survives' "$c"; done
  local hold_rc keep_out keep_rc=0
  hold_rc=$(finish_hold phold)
  if [[ $hold_rc == 0 ]]; then pass 'pressure: the held build completed rc=0'; else failc "pressure held build rc=$hold_rc"; fi
  keep_out=$(inner 60 run --rm dbp/pkeep cat /proof 2>&1) || keep_rc=$?
  if ((keep_rc == 0)) && [[ $keep_out == dbp-pkeep ]]; then pass 'pressure: the tagged image still runs'; else failc "pressure: tagged image run rc=$keep_rc out=$keep_out"; fi
}

# ---- main ------------------------------------------------------------------------------------

REFUSED_TEXT=$(sed -n 's/^const BUILDX_MISSING_TEXT = "\(.*\)";$/\1/p' "$REPO/agent/src/dind-builder-prune.ts")
[[ -n $REFUSED_TEXT ]] || fail 'could not read BUILDX_MISSING_TEXT from agent/src/dind-builder-prune.ts'
echo "product refused match string: '$REFUSED_TEXT'"
echo "output directory: $OUT"
echo "postures: $postures; phases: $phases; expect-red: ${expect_red:-none}; deadline ${DBP_DEADLINE_S:-1500}s"

timings=()
for posture in $postures; do
  case $posture in rootful | rootless) ;; *) fail "unknown posture $posture" ;; esac
  started=$SECONDS
  echo "################ POSTURE $posture ################"
  P=$posture
  if ! start_posture "$posture" >"$OUT/start-$posture.log" 2>&1; then
    start_out=$(tail -n 5 "$OUT/start-$posture.log" | tr '\n' ' ')
    echo "BLOCKED: $posture: $start_out"
    blocked=$((blocked + 1))
    continue
  fi
  timings+=("$posture setup $((SECONDS - started))s")
  print_versions
  if has_phase points; then phase_points; fi
  if has_phase real; then phase_real; fi
  if has_phase pressure; then phase_pressure; fi
  timings+=("$posture total $((SECONDS - started))s")
done

echo '================ SUMMARY ================'
printf '%s\n' "${results[@]}"
printf 'timing: %s\n' "${timings[@]}"
if ((blocked > 0)); then
  echo "RESULT: BLOCKED ($blocked posture(s) could not start)"
  exit 1
fi
if ((failures > 0)); then
  echo "RESULT: FAIL ($failures failed assertion(s))"
  exit 1
fi
if [[ $expect_red == switch-off ]]; then
  echo 'RESULT: RED A CONFIRMED (the harness fails as designed when the switch is off)'
else
  echo 'RESULT: PASS'
fi
