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

mkdir -p "$OUT/inputs" "$OUT/fixtures" "$OUT/results" "$OUT/tmp" "$OUT/logs"

log() { printf '### %s\n' "$*"; }

# Containers live outside the uzi- namespace (CLAUDE.md, Destructive operations) and are
# removed by exact name only.
CREATED=()
cleanup() {
  local name
  for name in "${CREATED[@]}"; do docker rm -f "$name" >/dev/null 2>&1 || true; done
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
  rm -f "$term"
  docker start "$name" >/dev/null
  local killed=false code
  if ! code="$(timeout "$limit" docker wait "$name")"; then
    # Record that the harness itself killed the container BEFORE killing it.
    killed=true
    printf '{"label":"%s","killedByHarness":true}\n' "$label" > "$term"
    docker kill "$name" >/dev/null 2>&1 || true
    code="$(docker wait "$name" 2>/dev/null || echo unknown)"
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
      label="${spec}__${mode}__${mem}"
      inject=false
      if [ "$SMOKE_INJECT" = "1" ]; then label="${label}__inject"; inject=true; fi
      run_job "$name" "$label" "$START_TIMEOUT" \
        "{\"kind\":\"measure\",\"label\":\"$label\",\"fixture\":\"/out/fixtures/$spec\",\"mode\":\"$mode\",\"out\":\"/out/results\",\"tmp\":\"/out/tmp/$label\",\"injectDeadlineAfterSizing\":$inject}"
    done
  done
done

log "rendering $OUT/results/results.md"
run_job "${CREATED[0]}" "report" 300 '{"kind":"report","out":"/out/results"}'
rm -f "$OUT/results/report.term.json"
cat "$OUT/results/results.md"
