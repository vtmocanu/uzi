#!/usr/bin/env bash
# Live compose acceptance of the runtime web modules (PRD #4686, plan milestone m5): a throwaway
# compose project is rendered to a literal file, built once, started with no modules, then with
# the example module, and driven by a browser in the pinned Playwright container.
# Opt-in (task test:web-modules-compose), never part of a gate. The api image builds from
# source, so a cold run takes many minutes.
#
# THE ACCEPTED DEVIATION FROM .claude/rules/stack.md (the owner decision recorded in the plan;
# conditions 1-6 below are that decision, in order):
#   stack.md wants `env -i HOME PATH docker compose --env-file <dummy> ...` for every call, so
#   that variables exported in the calling shell cannot reach compose's ${VAR:-default}
#   interpolation. This script does that for the RENDER only: `env -i ... config` writes a
#   fully literal file, which removes that channel. The later daemon calls (build, up, ps,
#   port, down) run from the rendered file under the inherited environment, which they need to
#   reach the daemon. The residual (CLI config, proxies and build settings can still affect
#   pulls and builds) is accepted for this check. No step re-execs around a guardrail.
#   1. Project name: mh-4686-<pid>-<nonce> (8 random hex chars per invocation, so two runs that
#      share a daemon and a PID still get distinct projects), passed with -p on every call. Never
#      -p uzi, never a bare down.
#      Before anything is built the daemon must hold NO container, volume or network of that
#      name (label or name prefix); a failed lookup or an existing resource is BROKEN (exit 2).
#   2. Ports: the published port is remapped with a `ports: !override` overlay to a free
#      127.0.0.1 port and BASE names it explicitly.
#   3. build, up and down run only from the rendered file (-f), with the same -p.
#   4. After start, the env and mounts of web and api match the render EXACTLY (env: the render over
#      the image's own defaults; mounts: the rendered volumes plus the image's declared VOLUMEs), and the web image id is
#      identical between the no-modules and the example-enabled runs (the no-rebuild proof).
#   5. Before any build or up, the rendered file is re-parsed (with an explicit EMPTY --env-file,
#      so no .env is autoloaded) under the inherited environment and compared with the clean
#      render; any mismatch stops the run. The Compose version is recorded.
#   6. Teardown: `down -v` by exact project name, only after every volume it would remove (by
#      project label and by rendered name) is confirmed prefixed mh-4686-<pid>-<nonce>_. Unnamed
#      (anonymous) volumes attached to this invocation's own containers may also be removed: with a
#      per-invocation project name they can belong to no other stack (owner-approved).
#
# Steps (each prints OK or FAIL): clean render; render assertions; round-trip; nonce through a
# throwaway container; build once (timed); baseline start and ownership, env and mount
# checks; baseline browser; enable the example module; same checks again plus the no-rebuild
# proof; enabled browser; teardown. The browser reaches the stack with `--network host` at
# BASE; if that cannot reach it, the script joins the project network and uses http://web:8080
# (after verifying the network's project label and the web container's address).
#
# Exit: 0 every step passed, 1 a check failed, 2 the instrument is broken (a tool, the daemon,
# the bind mount or the teardown guard).
set -uo pipefail

CDPATH=''
ROOT=$(cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT" || exit 2

OKS=0
ok() { OKS=$((OKS + 1)); printf 'OK    %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*" >&2; exit 1; }
broken() { printf 'BROKEN  %s\n' "$*" >&2; exit 2; }

for t in docker jq python3 curl; do command -v "$t" >/dev/null 2>&1 || broken "$t is not on PATH"; done
docker info >/dev/null 2>&1 || broken "docker daemon not reachable"
COMPOSE_VERSION=$(docker compose version 2>&1) || broken "docker compose is not available"
PLAY_IMAGE=$(sed -n 's/^IMAGE="\(.*\)"$/\1/p' scripts/web-browser-in-container.sh)
NGINX_IMAGE=$(awk '/^FROM nginxinc\/nginx-unprivileged/ { print $2; exit }' web/Dockerfile)
[ -n "$PLAY_IMAGE" ] && [ -n "$NGINX_IMAGE" ] || broken "could not read the pinned Playwright or nginx image"

# 1. The throwaway project. The prefix check is the guard every later call relies on.
NONCE=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n') || broken "cannot read /dev/urandom for the project nonce"
P="mh-4686-$$-$NONCE"
case "$P" in mh-4686-[0-9]*-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;; *) broken "project name $P is not a mh-4686-<pid>-<nonce> name" ;; esac

# 1b. Ownership precheck, before anything is created or taken. A PID can be reused and the
# daemon can be shared, so resources of this project name may already exist and would then pass
# the teardown prefix guard. Every lookup must succeed (a failed lookup is not "empty") and find
# nothing; otherwise exit BROKEN here, before STACK_TOUCHED is set, so the teardown never runs down.
# lookup <docker command...>: stdout lands in LK, stderr in LKERR, status is the command's. A CLI
# warning on stderr of a successful call must not read as an existing resource, so the two are
# never merged; a non-zero status is still a failed lookup (broken), not an empty result.
lookup() {
  local ef rc
  ef=$(mktemp) || broken "precheck: cannot create a temporary file"
  LK=$("$@" 2>"$ef")
  rc=$?
  LKERR=$(cat "$ef")
  rm -f "$ef"
  return "$rc"
}
lookup docker ps -aq --filter "label=com.docker.compose.project=$P" || broken "precheck: cannot list containers of project $P: $LKERR"
[ -z "$LK" ] || broken "precheck: project $P already has containers ($LK); not taking ownership"
lookup docker volume ls -q --filter "label=com.docker.compose.project=$P" || broken "precheck: cannot list volumes of project $P: $LKERR"
[ -z "$LK" ] || broken "precheck: project $P already has labelled volumes ($LK); not taking ownership"
lookup docker network ls -q --filter "label=com.docker.compose.project=$P" || broken "precheck: cannot list networks of project $P: $LKERR"
[ -z "$LK" ] || broken "precheck: project $P already has labelled networks ($LK); not taking ownership"
lookup docker ps -a --format '{{.Names}}' || broken "precheck: cannot list container names: $LKERR"
while IFS= read -r nm; do
  case "$nm" in "${P}-"*) broken "precheck: a container named $nm already exists; not taking ownership" ;; esac
done <<EOF
$LK
EOF
lookup docker volume ls -q || broken "precheck: cannot list volumes: $LKERR"
while IFS= read -r nm; do
  case "$nm" in "${P}_"*) broken "precheck: a volume named $nm already exists; not taking ownership" ;; esac
done <<EOF
$LK
EOF
lookup docker network ls --format '{{.Name}}' || broken "precheck: cannot list networks: $LKERR"
while IFS= read -r nm; do
  case "$nm" in "${P}_"*) broken "precheck: a network named $nm already exists; not taking ownership" ;; esac
done <<EOF
$LK
EOF

RUN="$ROOT/.uzi/scratch/mh4686-$$"
GEN="$RUN/web-modules"
DUMMY="$RUN/dummy.env"
EMPTY="$RUN/empty.env"
PORTS="$RUN/ports.yml"
RENDERED="$RUN/rendered.yml"
CLEAN_JSON="$RUN/clean.json"
RT_JSON="$RUN/roundtrip.json"
mkdir -p "$RUN" || broken "cannot create $RUN"
mkdir -m 755 "$GEN" && chmod 755 "$GEN" || broken "cannot create $GEN"
GEN_REAL=$(cd "$GEN" && pwd -P)

STACK_TOUCHED=0
TORN_DOWN=0
BCID=""
FINAL_RC=0
KEEP_RUN=0

dc() { docker compose --env-file "$EMPTY" -p "$P" -f "$RENDERED" "$@"; }

# remove_browser: the browser container goes BEFORE compose down, so the fallback path that
# joined the project network does not leave that network in use.
remove_browser() {
  if [ -n "$BCID" ]; then docker rm -f "$BCID" >/dev/null 2>&1 || true; BCID=""; fi
}

teardown() {
  [ "$TORN_DOWN" = 0 ] || return 0
  TORN_DOWN=1
  [ "$STACK_TOUCHED" = 1 ] || return 0
  remove_browser
  local v bad=0 labelled rendered_vols failed=""
  labelled=$(docker volume ls -q --filter "label=com.docker.compose.project=$P" 2>/dev/null) || failed="$failed 'docker volume ls' (project label lookup);"
  rendered_vols=$(jq -r '.volumes[]?.name' "$CLEAN_JSON" 2>/dev/null) || failed="$failed 'jq' over the rendered volumes;"
  while IFS= read -r v; do
    [ -n "$v" ] || continue
    case "$v" in "${P}_"*) ;; *) echo "teardown: volume $v is not prefixed ${P}_" >&2; bad=1 ;; esac
  done <<EOF
$labelled
$rendered_vols
EOF
  if [ "$bad" = 1 ] || [ -n "$failed" ]; then
    [ -z "$failed" ] || echo "teardown: the volume guard could not run, failed:$failed" >&2
    [ "$bad" = 0 ] || echo "teardown: a volume outside project $P would be removed" >&2
    echo "teardown: REFUSING down -v. Stopping the stack without removing volumes." >&2
    dc down --timeout 20 >"$RUN/down.log" 2>&1 || echo "teardown: plain down also failed: $(tail -n 3 "$RUN/down.log")" >&2
    echo "teardown: manual cleanup once the volumes are checked: docker compose -p $P -f $RENDERED down -v --rmi local" >&2
    echo "teardown: the run directory $RUN is kept so that command can run" >&2
    KEEP_RUN=1
    FINAL_RC=2
    return 0
  fi
  # --rmi local removes only this project's locally built images (the api and web builds); the
  # pulled db image is kept.
  if dc down -v --rmi local --timeout 20 >"$RUN/down.log" 2>&1; then
    local left_c left_v left_n
    if left_c=$(docker ps -aq --filter "label=com.docker.compose.project=$P") \
      && left_v=$(docker volume ls -q --filter "label=com.docker.compose.project=$P") \
      && left_n=$(docker network ls -q --filter "label=com.docker.compose.project=$P"); then
      if [ -z "$left_c$left_v$left_n" ]; then
        printf 'OK    teardown: down -v on project %s removed its containers, volumes and networks (all volumes prefixed %s_)\n' "$P" "$P"
      else
        echo "FAIL  teardown: containers, volumes or networks of $P remain after down -v" >&2
        FINAL_RC=1
      fi
    else
      echo "FAIL  teardown: a lookup after down -v failed, so the removal is unverified" >&2
      FINAL_RC=1
    fi
  else
    echo "FAIL  teardown: down -v failed: $(tail -n 3 "$RUN/down.log")" >&2
    FINAL_RC=1
  fi
}

cleanup() {
  local rc=$?
  remove_browser
  teardown
  if [ "$KEEP_RUN" = 0 ]; then case "$RUN" in "$ROOT"/.uzi/scratch/mh4686-[0-9]*) rm -rf "$RUN" ;; esac; fi
  [ "$rc" -ne 0 ] || rc=$FINAL_RC
  [ "$FINAL_RC" -le "$rc" ] || rc=$FINAL_RC
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# --- inputs: dummy credentials, an empty env file, the ports overlay -----------------------
PORT=$(python3 -I -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()') || broken "no free port"
BASE="http://127.0.0.1:$PORT"
SEED_EMAIL="admin@mh4686.e2e"
SEED_PASS=$(python3 -I -c 'import secrets; print("mh4686-" + secrets.token_hex(12))')
PG_PASS=$(python3 -I -c 'import secrets; print(secrets.token_hex(16))')
JWT=$(python3 -I -c 'import secrets; print(secrets.token_hex(64))')
SECRET_KEY=$(python3 -I -c 'import base64, secrets; print(base64.b64encode(secrets.token_bytes(32)).decode())')
# POSTGRES_PASSWORD, JWT_SECRET and UZI_SECRET_KEY are the `:?` vars of docker-compose.yml. The
# seed vars create the admin the browser signs in as. FRONTEND_ORIGIN must name the remapped
# origin (its default is :8080). UZI_WEB_MODULES_DIR is read by docker-compose.modules.yml.
cat > "$DUMMY" <<EOF
POSTGRES_PASSWORD=$PG_PASS
JWT_SECRET=$JWT
UZI_SECRET_KEY=$SECRET_KEY
UZI_SEED_EMAIL=$SEED_EMAIL
UZI_SEED_PASSWORD=$SEED_PASS
UZI_SEED_NAME=mh4686
FRONTEND_ORIGIN=$BASE
UZI_WEB_MODULES_DIR=$GEN
EOF
: > "$EMPTY"
cat > "$PORTS" <<EOF
services:
  web:
    ports: !override
      - "127.0.0.1:$PORT:8080"
EOF
echo "project $P, $BASE, run dir $RUN"
echo "compose: $COMPOSE_VERSION"

# --- 1. clean render -----------------------------------------------------------------------
clean_compose() {
  env -i HOME="$HOME" PATH="$PATH" docker compose -p "$P" --env-file "$DUMMY" \
    -f docker-compose.yml -f docker-compose.modules.yml -f "$PORTS" "$@"
}
clean_compose config > "$RENDERED" 2> "$RUN/render.err" || broken "clean render failed: $(head -n 5 "$RUN/render.err")"
clean_compose config --format json > "$CLEAN_JSON" 2> "$RUN/render.err" || broken "clean JSON render failed: $(head -n 5 "$RUN/render.err")"
ok "clean render under env -i (YAML and JSON)"

# --- 2. render assertions ------------------------------------------------------------------
jq -e '(.services | keys) == ["api","db","example-module","web"]' "$CLEAN_JSON" >/dev/null \
  || fail "render: the service set is not exactly db api web example-module: $(jq -c '.services | keys' "$CLEAN_JSON")"
! grep -qF '${' "$RENDERED" || fail "render: an unexpanded \${...} remains"
jq -e --arg pg "$PG_PASS" --arg jwt "$JWT" --arg key "$SECRET_KEY" --arg email "$SEED_EMAIL" --arg pass "$SEED_PASS" --arg origin "$BASE" '
  .services.db.environment.POSTGRES_PASSWORD == $pg
  and .services.api.environment.JWT_SECRET == $jwt
  and .services.api.environment.UZI_SECRET_KEY == $key
  and .services.api.environment.UZI_SEED_EMAIL == $email
  and .services.api.environment.UZI_SEED_PASSWORD == $pass
  and .services.api.environment.FRONTEND_ORIGIN == $origin' "$CLEAN_JSON" >/dev/null \
  || fail "render: the dummy values are not what the services will receive"
jq -e --argjson port "$PORT" '
  ([.services | to_entries[] | select((.value.ports // []) | length > 0) | .key] == ["web"])
  and (.services.web.ports | length == 1)
  and (.services.web.ports[0] | .host_ip == "127.0.0.1" and (.published | tostring) == ($port | tostring) and .target == 8080)' "$CLEAN_JSON" >/dev/null \
  || fail "render: web is not the only published port, or it is not 127.0.0.1:$PORT -> 8080"
jq -e --arg p "$P" '
  .name == $p
  and ([.volumes[]?.name, .networks[]?.name] | length > 0 and all(startswith($p + "_")))' "$CLEAN_JSON" >/dev/null \
  || fail "render: a volume or network name is not prefixed ${P}_ (or the project name differs)"
jq -e '[.services[] | select(.privileged == true or ((.cap_add // []) | length > 0) or .pid == "host" or .network_mode == "host"
        or ((.volumes // []) | any((.source // "") | tostring | test("docker.sock"))))] | length == 0' "$CLEAN_JSON" >/dev/null \
  || fail "render: a service is privileged, adds capabilities, shares the host namespaces or mounts the docker socket"
jq -e --arg gen "$GEN" '.services.web.volumes | any(.type == "bind" and .source == $gen and .target == "/etc/nginx/uzi-modules" and .read_only == true)' "$CLEAN_JSON" >/dev/null \
  || fail "render: web does not mount $GEN read-only at /etc/nginx/uzi-modules"
ok "render assertions (services, no \${, dummy values, only web published on 127.0.0.1:$PORT, prefixed names, nothing privileged, read-only modules mount)"

# --- 3. pre-start round-trip (condition 5) -------------------------------------------------
dc config --format json > "$RT_JSON" 2> "$RUN/rt.err" || fail "round-trip: the rendered file does not parse: $(head -n 5 "$RUN/rt.err")"
PROJECTION='{
  services: (.services | to_entries | map({key, value: {image, environment, build, ports, volumes, privileged, cap_add, cap_drop, security_opt, pid, network_mode, networks: ((.networks // {}) | keys)}}) | from_entries),
  volumes: [.volumes[]?.name], networks: [.networks[]?.name], name
}'
jq -S "$PROJECTION" "$CLEAN_JSON" > "$RUN/clean.proj"
jq -S "$PROJECTION" "$RT_JSON" > "$RUN/rt.proj"
if ! cmp -s "$RUN/clean.proj" "$RUN/rt.proj"; then
  diff "$RUN/clean.proj" "$RUN/rt.proj" | head -n 20 >&2
  fail "round-trip: the rendered file re-parses to something else under the inherited environment (service set, environment, build, ports, mounts, privileges or resource names differ)"
fi
if ! cmp -s <(jq -S . "$CLEAN_JSON") <(jq -S . "$RT_JSON"); then echo "note  round-trip: the whole documents differ outside the compared fields"; fi
ok "round-trip: re-parse of the rendered file (empty --env-file) equals the clean render; $COMPOSE_VERSION"

# --- 4. the bind mount works through the daemon ---------------------------------------------
NONCE="nonce-$$-$(python3 -I -c 'import secrets; print(secrets.token_hex(8))')"
printf '%s\n' "$NONCE" > "$GEN/nonce"
chmod 644 "$GEN/nonce"
GOT=$(docker run --rm --entrypoint cat -v "$GEN:/x:ro" "$NGINX_IMAGE" /x/nonce 2>"$RUN/nonce.err") || broken "nonce: a throwaway container could not read $GEN through a bind mount: $(head -n 3 "$RUN/nonce.err")"
[ "$GOT" = "$NONCE" ] || broken "nonce: the bind mount shows different content than $GEN (the daemon does not see this directory)"
rm -f "$GEN/nonce"
ok "nonce written to the generated dir was read back through a throwaway container"

# --- 5. build once ---------------------------------------------------------------------------
STACK_TOUCHED=1
T0=$SECONDS
dc build > "$RUN/build.log" 2>&1 || { tail -n 20 "$RUN/build.log" >&2; fail "build failed"; }
BUILD_SECS=$((SECONDS - T0))
ok "build (db is pulled, api builds from source): ${BUILD_SECS}s"

# --- helpers over the running stack -----------------------------------------------------------
cid_of() { dc ps -q "$1" 2>/dev/null | head -n 1; }

# stack_matches_render <label>: ownership of web, and the EXACT env and mounts of web and api against the render.
stack_matches_render() {
  local label=$1 svc cid inspect img imginfo diffkeys mountdiff
  for svc in web api; do
    cid=$(cid_of "$svc")
    [ -n "$cid" ] || fail "$label: no $svc container in project $P"
    inspect=$(docker inspect "$cid" | jq '.[0]') || fail "$label: cannot inspect $svc"
    [ "$(jq -r '.Config.Labels["com.docker.compose.project"]' <<<"$inspect")" = "$P" ] \
      && [ "$(jq -r '.Config.Labels["com.docker.compose.service"]' <<<"$inspect")" = "$svc" ] \
      || fail "$label: $svc does not carry com.docker.compose.project=$P / service=$svc"
    img=$(jq -r '.Image' <<<"$inspect")
    imginfo=$(docker image inspect "$img" | jq '.[0]') || fail "$label: cannot inspect the image of $svc"
    # Environment: exactly (the image's own defaults, overridden by the rendered environment).
    # Only variable NAMES are printed; the values include the throwaway credentials.
    diffkeys=$(jq -n -c --argjson have "$(jq -c '.Config.Env // []' <<<"$inspect")" \
      --argjson img "$(jq -c '.Config.Env // []' <<<"$imginfo")" \
      --argjson rend "$(jq -c --arg s "$svc" '.services[$s].environment // {}' "$CLEAN_JSON")" '
      def kv: (index("=")) as $i | if $i == null then {key: ., value: ""} else {key: .[:$i], value: .[$i+1:]} end;
      ($img | map(kv) | from_entries) as $d
      | ($rend | with_entries(select(.value != null) | .value |= tostring)) as $r
      | (($d + $r) | to_entries | map("\(.key)=\(.value)") | sort) as $w
      | ($have | sort) as $h
      | ((($w - $h) + ($h - $w)) | map(kv | .key) | unique) as $k
      | if ($k | length) == 0 and ($w | length) != ($h | length) then ["<duplicate entries>"] else $k end') \
      || fail "$label: the environment comparison of $svc could not run"
    [ "$diffkeys" = "[]" ] || fail "$label: the environment of $svc differs from (render over image defaults) in: $diffkeys"
    # Mounts: exactly the rendered volumes plus the image's declared VOLUMEs not rendered over
    # (those are anonymous volumes), compared on type, source, destination and read-write.
    mountdiff=$(jq -n -r --argjson mounts "$(jq -c '.Mounts // []' <<<"$inspect")" \
      --argjson rvols "$(jq -c --arg s "$svc" '.services[$s].volumes // []' "$CLEAN_JSON")" \
      --argjson named "$(jq -c '.volumes // {}' "$CLEAN_JSON")" \
      --argjson imgvols "$(jq -c '(.Config.Volumes // {}) | keys' <<<"$imginfo")" \
      --arg gen "$GEN" --arg real "$GEN_REAL" '
      ($rvols | map({type, source: (if .type == "volume" then (if (.source // "") == "" then "anonymous" else ($named[.source].name // .source) end) elif .type == "bind" then .source else "" end),
                     destination: .target, rw: ((.read_only // false) | not)})) as $r
      | ($r | map(.destination)) as $rd
      | ($r + ($imgvols | map(select(. as $d | $rd | index($d) == null)) | map({type: "volume", source: "anonymous", destination: ., rw: true}))
         | sort_by(.destination)) as $want
      | ($mounts | map({type: .Type,
                        source: (if .Type == "volume" then (if (.Name | test("^[0-9a-f]{64}$")) then "anonymous" else .Name end)
                                 elif .Type == "bind" then (if .Source == $real then $gen else .Source end) else "" end),
                        destination: .Destination, rw: .RW}) | sort_by(.destination)) as $have
      | if $want == $have then "" else "want \($want | tojson) have \($have | tojson)" end') \
      || fail "$label: the mount comparison of $svc could not run"
    [ -z "$mountdiff" ] || fail "$label: the mounts of $svc differ from the render plus the image's VOLUMEs: $mountdiff"
  done
  [ "$(dc port web 8080 2>/dev/null)" = "127.0.0.1:$PORT" ] || fail "$label: docker compose port web 8080 is not 127.0.0.1:$PORT"
  ok "$label: web owns 127.0.0.1:$PORT with the project labels; env and mounts of web and api match the render"
}

wait_for() { # <seconds> <description> <command...>
  local n=$1 what=$2 i
  shift 2
  for ((i = 0; i < n; i++)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  fail "timed out after ${n}s waiting for $what"
}
modules_json_is() { curl -fsS --max-time 5 "$BASE/modules.json" | jq -e "$1" >/dev/null; }

# --- browser container ------------------------------------------------------------------------
BASE_IN="$BASE"
# browser_prepare <docker create network and name flags...>
browser_prepare() {
  BCID=$(docker create --init --ipc=host "$@" "$PLAY_IMAGE" sleep infinity) || broken "cannot create the browser container"
  docker start "$BCID" >/dev/null || broken "cannot start the browser container"
  docker exec "$BCID" mkdir -p /work/web/scripts /work/web/node_modules >/dev/null || broken "cannot prepare /work in the browser container"
  docker cp web/scripts/modules-compose-check.mjs "$BCID:/work/web/scripts/modules-compose-check.mjs" >/dev/null || broken "docker cp of the check script failed"
  docker cp web/scripts/browser-launch.mjs "$BCID:/work/web/scripts/browser-launch.mjs" >/dev/null || broken "docker cp of browser-launch.mjs failed"
  docker cp -L web/node_modules/playwright "$BCID:/work/web/node_modules/playwright" >/dev/null || broken "docker cp of playwright failed"
  docker cp -L web/node_modules/playwright-core "$BCID:/work/web/node_modules/playwright-core" >/dev/null || broken "docker cp of playwright-core failed"
}
browser_reaches() { docker exec "$BCID" node -e 'fetch(process.argv[1]).then((r) => process.exit(r.ok ? 0 : 1), () => process.exit(1))' "$1/modules.json"; }
browser_start() {
  browser_prepare --network host --name "mh4686-browser-$$"
  if browser_reaches "$BASE"; then
    BASE_IN="$BASE"
    ok "browser container on the host network reaches $BASE"
    return 0
  fi
  echo "note  --network host does not reach $BASE; joining the project network instead"
  docker rm -f "$BCID" >/dev/null 2>&1 || true
  BCID=""
  local net="${P}_default" web_ip
  [ "$(docker network inspect --format '{{index .Labels "com.docker.compose.project"}}' "$net" 2>/dev/null)" = "$P" ] \
    || broken "network $net does not carry the project label $P; not joining it"
  web_ip=$(docker inspect --format "{{(index .NetworkSettings.Networks \"$net\").IPAddress}}" "$(cid_of web)" 2>/dev/null)
  [ -n "$web_ip" ] || broken "the web container has no address on $net"
  browser_prepare --network "$net" --name "mh4686-browser-$$"
  BASE_IN="http://web:8080"
  browser_reaches "$BASE_IN" || broken "the browser container reaches neither $BASE nor $BASE_IN (web is $web_ip on $net)"
  ok "browser container on $net reaches $BASE_IN (web at $web_ip)"
}
browser_run() { # <baseline|enabled>
  local rc=0
  docker exec -e MC_EMAIL="$SEED_EMAIL" -e MC_PASSWORD="$SEED_PASS" -w /work/web "$BCID" \
    node scripts/modules-compose-check.mjs "$1" "$BASE_IN" > "$RUN/browser-$1.log" 2>&1 || rc=$?
  sed 's/^/      /' "$RUN/browser-$1.log"
  case "$rc" in
    0) ok "browser ($1): every check passed" ;;
    1) fail "browser ($1): a check failed" ;;
    *) broken "browser ($1): the instrument failed (exit $rc)" ;;
  esac
}

# --- 6. baseline: no modules -----------------------------------------------------------------
echo '[]' > "$RUN/baseline.json"
python3 -I scripts/web-modules-gen.py "$RUN/baseline.json" "$GEN" > /dev/null || fail "baseline: the generator failed on []"
[ "$({ stat -c %a "$GEN" 2>/dev/null || stat -f %Lp "$GEN"; })" = 755 ] || fail "baseline: the generated dir is not 0755"
dc up -d --no-build > "$RUN/up1.log" 2>&1 || { tail -n 20 "$RUN/up1.log" >&2; fail "baseline: up failed"; }
wait_for 90 "$BASE/modules.json to answer" curl -fsS --max-time 5 "$BASE/modules.json"
modules_json_is '. == []' || fail "baseline: $BASE/modules.json is not []"
ok "baseline: $BASE/modules.json is []"
stack_matches_render "baseline"
WEB_IMG1=$(docker inspect --format '{{.Image}}' "$(cid_of web)")
[ -n "$WEB_IMG1" ] || fail "baseline: no web image id"
ok "baseline: web image id $WEB_IMG1"
browser_start
browser_run baseline

# --- 7. enable the example module ---------------------------------------------------------------
python3 -I scripts/web-modules-gen.py deploy/compose/web-modules.example.json "$GEN" > /dev/null || fail "enable: the generator failed on the example list"
dc up -d --no-build --force-recreate web example-module > "$RUN/up2.log" 2>&1 || { tail -n 20 "$RUN/up2.log" >&2; fail "enable: up --force-recreate failed"; }
wait_for 90 "$BASE/modules.json to list example" modules_json_is 'map(.id) == ["example"]'
wait_for 90 "$BASE/modules/example/remoteEntry.js to answer 200" bash -c "[ \"\$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 '$BASE/modules/example/remoteEntry.js')\" = 200 ]"
ok "enable: $BASE/modules.json lists example and /modules/example/remoteEntry.js is 200"
stack_matches_render "enabled"
WEB_IMG2=$(docker inspect --format '{{.Image}}' "$(cid_of web)")
[ "$WEB_IMG2" = "$WEB_IMG1" ] || fail "enable: the web image changed ($WEB_IMG1 -> $WEB_IMG2): the stack was rebuilt, not just reconfigured"
ok "enable: web image id unchanged across the module change ($WEB_IMG2): no rebuild"
browser_run enabled

# --- 9. teardown (also runs from the EXIT trap on any failure) -----------------------------------
teardown
echo
echo "compose version : $COMPOSE_VERSION"
echo "build time      : ${BUILD_SECS}s"
echo "web image id    : baseline $WEB_IMG1, enabled $WEB_IMG2"
echo "project         : $P ($BASE)"
echo "steps passed    : $OKS"
[ "$FINAL_RC" -eq 0 ] || exit "$FINAL_RC"
echo "PASS: runtime web modules on the compose stack"
