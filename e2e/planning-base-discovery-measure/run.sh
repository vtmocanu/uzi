#!/usr/bin/env bash
# Issue #3543: measure plan cross-check base discovery (the PLANNING_CAPTURE_HELPER tree walk
# behind GitCache.capturePlanningDiff) under the production bounds and a container memory
# limit. Opt-in only: no Task target or CI job runs this. Read README.md beside it first.
#
#   OUT=/abs/dir ./run.sh
#
# Environment:
#   MEASURE_IMAGE  worker image providing node + git   (default: uzi-agent-home-split:base;
#                  build one with docker build -f agent/templates/base/Dockerfile -t <tag> .)
#   OUT            absolute output dir; reused across invocations (required)
#   FIXTURES       space-separated fixture specs         (default: the full matrix below)
#   MEM_CONFIGS    space-separated mem:cpus:modes        (default: "1536m:1:full,control,cancel 4g:2:full")
#   REPO_SHA       commit for the `repo` fixture         (default: HEAD of this checkout)
#   START_TIMEOUT  seconds per measurement start         (default: 120)
#   GEN_TIMEOUT    seconds per fixture generation        (default: 1200)
#   SMOKE_INJECT   1 = add the smoke-only deadline fault after the sizing batch (label __inject)
#   LIMITS_JSON    validation rows only: one-line JSON object of positive integers merged over the
#                  helper's default planning limits, e.g. '{"baseDiscoveryEntryCap":1000}'
#                  (label __validation; unset = production defaults; not combinable with SMOKE_INJECT)
#   LIMITS_TAG     optional, with LIMITS_JSON only: [a-z0-9-]{1,40}, label becomes __validation-<tag>
#                  so two LIMITS_JSON invocations into one OUT do not overwrite each other
#   DEADLINE_SCALE validation rows only: integer 2..8. Slows ONLY the helper's clock by that factor
#                  (its 28 s deadline lasts 28 s x N of real time) and stretches the outer 30 s capture
#                  timeout, the harness 60 s scope timer and this script's per-job wait by N. Every
#                  cap and bound stays at its production value. Label __validation-dl<N> (with
#                  LIMITS_TAG: __validation-<tag>-dl<N>); usable with or without LIMITS_JSON; not
#                  combinable with SMOKE_INJECT. Measures memory and charges at the caps, not timing.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
IMAGE="${MEASURE_IMAGE:-uzi-agent-home-split:base}"
OUT="${OUT:?set OUT to an absolute output directory}"
case "$OUT" in /*) ;; *) echo "OUT must be absolute" >&2; exit 2 ;; esac
FIXTURES="${FIXTURES:-repo synthetic-10000 synthetic-50000 chain-d64 chain-d65 fanout2-d8 fanout2-d12 fanout2-d16 fanout2-d20 fanout2-d24 fanout2-d64 wide-1024x1024 wide-4096x4096 wide-16384x16384}"
MEM_CONFIGS="${MEM_CONFIGS:-1536m:1:full,control,cancel 4g:2:full}"
START_TIMEOUT="${START_TIMEOUT:-120}"
GEN_TIMEOUT="${GEN_TIMEOUT:-1200}"
SMOKE_INJECT="${SMOKE_INJECT:-}"

for spec in $FIXTURES; do
  # Specs are interpolated into job JSON and paths: allow only the generator's own grammar.
  case "$spec" in
    repo|synthetic-[0-9]*|chain-d[0-9]*|fanout2-d[0-9]*|wide-[0-9]*x[0-9]*|longpath-d[0-9]*n[0-9]*w[0-9]*f[0-9]*|distinct-[0-9]*x[0-9]*n[0-9]*|fatchain-d[0-9]*) ;;
    *) echo "unknown fixture spec: $spec" >&2; exit 2 ;;
  esac
  case "$spec" in *[!a-z0-9-]*) echo "unsafe fixture spec: $spec" >&2; exit 2 ;; esac
done

# LIMITS_JSON marks every measurement of this invocation as a VALIDATION row: the harness merges
# these planning limits over the helper's defaults (GitCache.setPlanningLimitsForTest) and the
# row label gets a __validation suffix. Production rows never set it and always run on defaults.
# Accepted shape: a non-empty one-line JSON object of identifier keys to positive integers of at
# most 15 digits (so always a safe integer); anything else is refused before any container starts.
LIMITS_JSON="${LIMITS_JSON:-}"
if [ -n "$LIMITS_JSON" ]; then
  limit_pair='"[A-Za-z][A-Za-z0-9]*":[1-9][0-9]{0,14}'
  limits_re="^\\{${limit_pair}(,${limit_pair})*\\}\$"
  [[ "$LIMITS_JSON" =~ $limits_re ]] || { echo "LIMITS_JSON must be a one-line JSON object of positive integers (max 15 digits): $LIMITS_JSON" >&2; exit 2; }
  [ -z "$SMOKE_INJECT" ] || { echo "LIMITS_JSON and SMOKE_INJECT cannot be combined" >&2; exit 2; }
fi
DEADLINE_SCALE="${DEADLINE_SCALE:-}"
if [ -n "$DEADLINE_SCALE" ]; then
  [[ "$DEADLINE_SCALE" =~ ^[2-8]$ ]] || { echo "DEADLINE_SCALE must be an integer 2..8: $DEADLINE_SCALE" >&2; exit 2; }
  [ -z "$SMOKE_INJECT" ] || { echo "DEADLINE_SCALE and SMOKE_INJECT cannot be combined" >&2; exit 2; }
fi
LIMITS_TAG="${LIMITS_TAG:-}"
validation_suffix="__validation"
if [ -n "$LIMITS_TAG" ]; then
  [ -n "$LIMITS_JSON" ] || [ -n "$DEADLINE_SCALE" ] || { echo "LIMITS_TAG requires LIMITS_JSON or DEADLINE_SCALE" >&2; exit 2; }
  [[ "$LIMITS_TAG" =~ ^[a-z0-9-]{1,40}$ ]] || { echo "LIMITS_TAG must match [a-z0-9-]{1,40}: $LIMITS_TAG" >&2; exit 2; }
  validation_suffix="__validation-$LIMITS_TAG"
fi
# DEADLINE_SCALE alone (production limits) is still a validation row: label ...__validation-dl<N>
# (with LIMITS_TAG: __validation-<tag>-dl<N>). It also stretches this script's per-job wait.
deadline_field=""
job_timeout="$START_TIMEOUT"
if [ -n "$DEADLINE_SCALE" ]; then
  validation_suffix="${validation_suffix}-dl${DEADLINE_SCALE}"
  deadline_field=",\"deadlineScale\":$DEADLINE_SCALE"
  job_timeout=$((START_TIMEOUT * DEADLINE_SCALE))
fi

# shellcheck disable=SC2086 # deliberate word split: count the configurations.
set -- $MEM_CONFIGS
[ "$#" -gt 0 ] || { echo "MEM_CONFIGS has no configurations" >&2; exit 2; }
for config in "$@"; do
  case "$config" in
    *[!a-z0-9:,]*) echo "unsafe MEM_CONFIGS entry: $config" >&2; exit 2 ;;
  esac
done

mkdir -p "$OUT/inputs" "$OUT/fixtures" "$OUT/results" "$OUT/tmp" "$OUT/logs"

log() { printf '### %s\n' "$*"; }

# Containers live outside the uzi- namespace (CLAUDE.md, Destructive operations) and are
# removed by exact name only, together with their anonymous volumes: the image declares
# VOLUME /data and /nix, and a plain `docker rm` would leave a multi-GiB /nix copy behind.
CREATED=()
cleanup() {
  local name
  for name in ${CREATED[@]+"${CREATED[@]}"}; do docker rm -f -v "$name" >/dev/null 2>&1 || true; done
}
trap cleanup EXIT

case " $FIXTURES " in
  *" repo "*)
    sha="$(git -C "$REPO" rev-parse "${REPO_SHA:-HEAD}^{commit}")"
    if [ "$(cat "$OUT/inputs/repo.sha" 2>/dev/null || true)" != "$sha" ]; then
      log "exporting repo fixture input at $sha"
      git -C "$REPO" archive --format=tar "$sha" > "$OUT/inputs/repo.tar"
      git -C "$REPO" rev-parse "$sha^{tree}" > "$OUT/inputs/repo.tree"
      printf '%s\n' "$sha" > "$OUT/inputs/repo.sha"
      rm -rf "$OUT/fixtures/repo"
    fi
    ;;
esac

# create_container <name> <mem> <cpus>: one container per memory configuration. Each later
# `docker start` gets a fresh cgroup, so memory.peak covers exactly one job. tini is PID 1, as in
# the production worker image (agent/templates/base/Dockerfile), so killed git children are
# reaped; with node as PID 1 their zombies keep the process group alive and every cancellation
# reads as a surviving group.
create_container() {
  log "creating $1 (memory=$2, cpus=$3; the first create of a large image can take minutes)"
  docker create --name "$1" --memory="$2" --memory-swap="$2" --cpus="$3" --network none \
    --user "$(id -u):$(id -g)" -e HOME=/tmp -e UZI_PLANNING_DISCOVERY_MEASURE=1 \
    -e MEASURE_MEM_LIMIT="$2" -e MEASURE_CPUS="$3" \
    --mount "type=bind,src=$REPO/agent,dst=/work/agent,readonly" \
    --mount "type=bind,src=$OUT,dst=/out" \
    -w /work/agent --entrypoint /sbin/tini "$IMAGE" \
    -- node --import tsx test/planning-base-discovery.measure.ts job /out/job.json >/dev/null
  CREATED+=("$1")
}

# run_job <container> <label> <timeout-seconds> <job-json>: start, wait, classify termination.
run_job() {
  local name="$1" label="$2" limit="$3" job="$4" term="$OUT/results/$2.term.json"
  printf '%s\n' "$job" > "$OUT/job.json"
  # A job that dies before writing its own records must never pair with an older invocation's.
  rm -f "$term" "$OUT/results/$label.json" "$OUT/results/$label.live.json" "$OUT/results/$label.helper.json"
  docker start "$name" >/dev/null
  local killed=false code rc=0
  code="$(timeout "$limit" docker wait "$name")" || rc=$?
  if [ "$rc" -eq 124 ]; then
    # Record that the harness itself killed the container BEFORE killing it.
    killed=true
    printf '{"label":"%s","killedByHarness":true}\n' "$label" > "$term"
    docker kill "$name" >/dev/null 2>&1 || true
    code="$(docker wait "$name" 2>/dev/null || echo unknown)"
  elif [ "$rc" -ne 0 ]; then
    # Never leave a job running into the next one: stop it, and record that it was not a timeout.
    code="wait_failed_rc_$rc"
    docker kill "$name" >/dev/null 2>&1 || true
    docker wait "$name" >/dev/null 2>&1 || true
  fi
  local oom
  oom="$(docker inspect -f '{{.State.OOMKilled}}' "$name")"
  docker logs --tail 40 "$name" > "$OUT/logs/$label.log" 2>&1 || true
  printf '{"label":"%s","exitCode":"%s","oomKilled":%s,"killedByHarness":%s}\n' \
    "$label" "$code" "$oom" "$killed" > "$term"
  log "$label: exit=$code oomKilled=$oom killedByHarness=$killed"
}

idx=0
generated=" "
for config in $MEM_CONFIGS; do
  IFS=: read -r mem cpus modes <<<"$config"
  idx=$((idx + 1))
  name="pbd-measure-$$-$idx"
  create_container "$name" "$mem" "$cpus"
  # The first start of a fresh container carries a node/tsx startup transient (about 140 MiB
  # cgroup peak vs about 60 MiB later); absorb it so no measurement is a first start.
  run_job "$name" "warmup__$idx" "$GEN_TIMEOUT" '{"kind":"noop"}'
  mv "$OUT/results/warmup__$idx.term.json" "$OUT/logs/warmup__$idx.term.json"
  for spec in $FIXTURES; do
    if [ ! -f "$OUT/fixtures/$spec/manifest.json" ] && [ "${generated#* "$spec" }" = "$generated" ]; then
      run_job "$name" "gen__$spec" "$GEN_TIMEOUT" \
        "{\"kind\":\"generate\",\"spec\":\"$spec\",\"dir\":\"/out/fixtures/$spec\",\"inputs\":\"/out/inputs\"}"
      mv "$OUT/results/gen__$spec.term.json" "$OUT/logs/gen__$spec.term.json"
      generated="$generated$spec "
    fi
    if [ ! -f "$OUT/fixtures/$spec/manifest.json" ]; then
      log "fixture $spec failed to generate; see $OUT/logs/gen__$spec.log"
      continue
    fi
    for mode in ${modes//,/ }; do
      # CPU is part of the identity: two configs may share a memory limit.
      label="${spec}__${mode}__${mem}-${cpus}cpu"
      inject=false
      if [ "$SMOKE_INJECT" = "1" ]; then label="${label}__inject"; inject=true; fi
      limits_field=""
      if [ -n "$LIMITS_JSON" ]; then limits_field=",\"limits\":$LIMITS_JSON"; fi
      if [ -n "$LIMITS_JSON" ] || [ -n "$DEADLINE_SCALE" ]; then label="${label}${validation_suffix}"; fi
      run_job "$name" "$label" "$job_timeout" \
        "{\"kind\":\"measure\",\"label\":\"$label\",\"fixture\":\"/out/fixtures/$spec\",\"mode\":\"$mode\",\"out\":\"/out/results\",\"tmp\":\"/out/tmp/$label\",\"injectDeadlineAfterSizing\":$inject$limits_field$deadline_field}"
    done
  done
  # Results are on the bind-mounted $OUT; free this configuration's container and volumes before
  # the next one is created. The last one stays to render the report and goes in cleanup().
  if [ "$idx" -lt "$#" ]; then
    docker rm -f -v "$name" >/dev/null
    CREATED=()
  fi
done

log "rendering $OUT/results/results.md"
run_job "$name" "report" 300 '{"kind":"report","out":"/out/results"}'
mv "$OUT/results/report.term.json" "$OUT/logs/report.term.json"
cat "$OUT/results/results.md"
