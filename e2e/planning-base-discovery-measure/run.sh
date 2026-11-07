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
#   LIMITS_TAG     optional, with LIMITS_JSON or DEADLINE_SCALE: [a-z0-9-]{1,40}, label becomes
#                  __validation-<tag>, so two validation invocations into one OUT do not overwrite each other
#   DEADLINE_SCALE validation rows only: integer 2..8. Slows ONLY the helper's clock by that factor
#                  (its 28 s deadline lasts 28 s x N of real time) and stretches the outer 30 s capture
#                  timeout, the harness 60 s scope timer and this script's per-job wait by N. Every
#                  cap and memory bound stays at its production value; per-command git timers are
#                  clamped to the slowed deadline (README, "Deadline scale"). Label __validation-dl<N>
#                  (with LIMITS_TAG: __validation-<tag>-dl<N>); usable with or without LIMITS_JSON;
#                  not combinable with SMOKE_INJECT; every MEM_CONFIGS entry must use mode full only.
#                  Measures memory and charges at the caps, not timing.
#   DEADLINE_LIFT  validation rows only: 1 (anything else exits 2). The harness rewrites ONLY the
#                  helper source handed to its spawner (deadline 28 s -> 1800 s, per-command timer
#                  ceiling 5 s -> 1800 s; planning-deadline-lift.ts) and lifts the outer 30 s capture
#                  timeout to 1830 s and the harness scope timer to 1860 s; every cap and memory bound
#                  keeps its production value. Modes full (metered) and control (unmetered) only; label
#                  __validation-lift; each job's wait becomes 1900 s. Never a production row. Refused
#                  with DEADLINE_SCALE, SMOKE_INJECT, LIMITS_JSON, LIMITS_TAG or a MEM_CONFIGS mode
#                  other than full/control (so `cancel` is refused).
#   MIN_FREE_GIB   disk preflight floor in GiB: integer >= 5 (default 5; lower or non-numeric exits 2).
#   MIN_FREE_GIB_DOCKER  floor in GiB for the Docker store only (default MIN_FREE_GIB; integer, must be
#                  >= MIN_FREE_GIB, else exit 2). It can only raise that one floor, so the Docker-store
#                  refusal branch can be exercised while the host stores still pass.
#   GATE_SELFTEST  1 = prove the readability gate and nothing else: create the first MEM_CONFIGS
#                  container, run the warmup and one noop under the measurement-style label
#                  selftest__gate, truncate that label's result file to empty, then run the gate. The
#                  gate must preserve the container (stopped, never removed; PRESERVED.txt) and the
#                  script exits 6 ("selftest passed: container preserved"), distinct from a real block
#                  (exit 4) and from a selftest failure (exit 5, the gate accepted the empty file). The
#                  caller removes the container by the name and id printed. Not combinable with any other
#                  mode variable; no fixtures are exported or generated.
#
# Disk preflight (require_free): before any work, before each fixture generation and before each
# container creation, the host filesystems of /data and of $OUT and the docker store (read with a
# zero-write probe container pbd-dfprobe-<pid>, created once, `df -Pk /` of the image) must each have
# >= MIN_FREE_GIB GiB free (the docker store: MIN_FREE_GIB_DOCKER). A store below the floor, or one that cannot be read, prints
# `BLOCKED: disk preflight (<what>): <store> has <n> KiB free, need >= <floor>` and exits 3 without
# creating anything further.
#
# Readability gate (confirm_readable): a container is removed (docker rm -f -v) only after every
# result it produced is confirmed readable: the label's result .json, its term record and a
# non-empty log must each exist, be non-empty and (the JSON files) parse. There is no exception: a
# missing result is unreadable whatever the term record says, so an OOM-killed, nonzero-exit or
# harness-killed job also stops the invocation and preserves its container (that is intended).
# Otherwise the container is NOT removed: it is stopped (docker kill, docker wait; never rm, never
# -v), its id, name and the failing files are appended to $OUT/logs/PRESERVED.txt, `BLOCKED:
# unreadable results preserved in container <id>` is printed and the script exits 4. The exit trap
# applies the same check to a container still standing (a failure or interrupt mid-configuration).
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
# Both reach `timeout` and START_TIMEOUT also bash arithmetic, which would evaluate a non-number.
[[ "$START_TIMEOUT" =~ ^[1-9][0-9]{0,5}$ ]] || { echo "START_TIMEOUT must be a positive integer of seconds: $START_TIMEOUT" >&2; exit 2; }
[[ "$GEN_TIMEOUT" =~ ^[1-9][0-9]{0,5}$ ]] || { echo "GEN_TIMEOUT must be a positive integer of seconds: $GEN_TIMEOUT" >&2; exit 2; }
SMOKE_INJECT="${SMOKE_INJECT:-}"
# The gate's block exit; never read from the environment (only GATE_SELFTEST sets it, to 6).
GATE_BLOCK_EXIT=4
MIN_FREE_GIB="${MIN_FREE_GIB:-5}"
# Bounded digits keep the arithmetic below inside bash integers; a leading zero is read as decimal.
[[ "$MIN_FREE_GIB" =~ ^[0-9]{1,6}$ ]] && [ "$((10#$MIN_FREE_GIB))" -ge 5 ] \
  || { echo "MIN_FREE_GIB must be an integer >= 5: $MIN_FREE_GIB" >&2; exit 2; }
MIN_FREE_KIB=$((10#$MIN_FREE_GIB * 1048576))
MIN_FREE_GIB_DOCKER="${MIN_FREE_GIB_DOCKER:-$MIN_FREE_GIB}"
[[ "$MIN_FREE_GIB_DOCKER" =~ ^[0-9]{1,6}$ ]] && [ "$((10#$MIN_FREE_GIB_DOCKER))" -ge "$((10#$MIN_FREE_GIB))" ] \
  || { echo "MIN_FREE_GIB_DOCKER must be an integer >= MIN_FREE_GIB ($MIN_FREE_GIB): $MIN_FREE_GIB_DOCKER" >&2; exit 2; }
MIN_FREE_KIB_DOCKER=$((10#$MIN_FREE_GIB_DOCKER * 1048576))

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
DEADLINE_LIFT="${DEADLINE_LIFT:-}"
if [ -n "$DEADLINE_LIFT" ]; then
  [ "$DEADLINE_LIFT" = "1" ] || { echo "DEADLINE_LIFT must be 1 or unset: $DEADLINE_LIFT" >&2; exit 2; }
  [ -z "$DEADLINE_SCALE" ] || { echo "DEADLINE_LIFT and DEADLINE_SCALE cannot be combined" >&2; exit 2; }
  [ -z "$SMOKE_INJECT" ] || { echo "DEADLINE_LIFT and SMOKE_INJECT cannot be combined" >&2; exit 2; }
  [ -z "$LIMITS_JSON" ] || { echo "DEADLINE_LIFT and LIMITS_JSON cannot be combined" >&2; exit 2; }
  [ -z "$LIMITS_TAG" ] || { echo "DEADLINE_LIFT and LIMITS_TAG cannot be combined" >&2; exit 2; }
fi
GATE_SELFTEST="${GATE_SELFTEST:-}"
if [ -n "$GATE_SELFTEST" ]; then
  [ "$GATE_SELFTEST" = "1" ] || { echo "GATE_SELFTEST must be 1 or unset: $GATE_SELFTEST" >&2; exit 2; }
  if [ -n "$DEADLINE_LIFT$DEADLINE_SCALE$SMOKE_INJECT$LIMITS_JSON$LIMITS_TAG" ]; then
    echo "GATE_SELFTEST cannot be combined with DEADLINE_LIFT, DEADLINE_SCALE, SMOKE_INJECT, LIMITS_JSON or LIMITS_TAG" >&2; exit 2
  fi
  # The self-test needs no fixture, and the repo export would only cost time.
  FIXTURES=""
fi
validation_suffix="__validation"
if [ -n "$LIMITS_TAG" ]; then
  [ -n "$LIMITS_JSON" ] || [ -n "$DEADLINE_SCALE" ] || { echo "LIMITS_TAG requires LIMITS_JSON or DEADLINE_SCALE" >&2; exit 2; }
  [[ "$LIMITS_TAG" =~ ^[a-z0-9-]{1,40}$ ]] || { echo "LIMITS_TAG must match [a-z0-9-]{1,40}: $LIMITS_TAG" >&2; exit 2; }
  validation_suffix="__validation-$LIMITS_TAG"
fi
# DEADLINE_SCALE alone (production limits) is still a validation row: label ...__validation-dl<N>
# (with LIMITS_TAG: __validation-<tag>-dl<N>). It also stretches this script's per-job wait.
# DEADLINE_LIFT rows are labelled ...__validation-lift and wait up to 1900 s per job.
deadline_field=""
job_timeout="$START_TIMEOUT"
if [ -n "$DEADLINE_SCALE" ]; then
  validation_suffix="${validation_suffix}-dl${DEADLINE_SCALE}"
  deadline_field=",\"deadlineScale\":$DEADLINE_SCALE"
  job_timeout=$((START_TIMEOUT * DEADLINE_SCALE))
fi
if [ -n "$DEADLINE_LIFT" ]; then
  validation_suffix="__validation-lift"
  deadline_field=",\"deadlineLift\":true"
  job_timeout=1900
fi

# shellcheck disable=SC2086 # deliberate word split: count the configurations.
set -- $MEM_CONFIGS
[ "$#" -gt 0 ] || { echo "MEM_CONFIGS has no configurations" >&2; exit 2; }
for config in "$@"; do
  case "$config" in
    *[!a-z0-9:,]*) echo "unsafe MEM_CONFIGS entry: $config" >&2; exit 2 ;;
  esac
  # The stretched clock lives in the metering preamble, which only `full` prepends.
  if [ -n "$DEADLINE_SCALE" ] && [ "${config##*:}" != "full" ]; then
    echo "DEADLINE_SCALE requires every MEM_CONFIGS entry to use mode full only: $config" >&2; exit 2
  fi
  if [ -n "$DEADLINE_LIFT" ]; then
    lift_modes="${config##*:}"
    for lift_mode in ${lift_modes//,/ }; do
      case "$lift_mode" in
        full|control) ;;
        *) echo "DEADLINE_LIFT allows only the full and control modes: $config" >&2; exit 2 ;;
      esac
    done
  fi
done

log() { printf '### %s\n' "$*"; }

mkdir -p "$OUT"

# Containers live outside the uzi- namespace (CLAUDE.md, Destructive operations) and are
# removed by exact name only, together with their anonymous volumes: the image declares
# VOLUME /data and /nix, and a plain `docker rm` would leave a multi-GiB /nix copy behind.
# CREATED holds the container still standing (at most one); CUR_LABELS the jobs started on it.
CREATED=()
CUR_LABELS=()
PROBE="pbd-dfprobe-$$"
PROBE_MADE=0

# json_ok <file>: exists, non-empty, parses.
json_ok() {
  [ -s "$1" ] && node -e 'JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"))' "$1" >/dev/null 2>&1
}
# unreadable_files <label>: prints the failing files (each preceded by a space), nothing when readable.
unreadable_files() {
  local label="$1" bad=""
  case "$label" in
    gen__*|warmup__*|report)
      json_ok "$OUT/logs/$label.term.json" || bad="$bad logs/$label.term.json"
      [ -s "$OUT/logs/$label.log" ] || bad="$bad logs/$label.log"
      if [ "$label" = report ]; then [ -s "$OUT/results/results.md" ] || bad="$bad results/results.md"; fi
      ;;
    *)
      json_ok "$OUT/results/$label.term.json" || bad="$bad results/$label.term.json"
      [ -s "$OUT/logs/$label.log" ] || bad="$bad logs/$label.log"
      # No exception: a missing, empty or unparseable result always counts, whatever the term record says.
      json_ok "$OUT/results/$label.json" || bad="$bad results/$label.json"
      ;;
  esac
  printf '%s' "$bad"
}
# unreadable_all <label>...: the failing files across the labels.
unreadable_all() {
  local label failing=""
  for label in "$@"; do failing="$failing$(unreadable_files "$label")"; done
  printf '%s' "$failing"
}
# preserve_container <name> <failing files>: keep the container, record it, and drop it from CREATED.
preserve_container() {
  local name="$1" failing="$2" id
  # Stop it first so nothing keeps writing; never rm, never -v.
  docker kill "$name" >/dev/null 2>&1 || true
  docker wait "$name" >/dev/null 2>&1 || true
  id="$(docker inspect -f '{{.Id}}' "$name" 2>/dev/null || echo unknown)"
  mkdir -p "$OUT/logs"
  printf '%s %s%s\n' "$id" "$name" "$failing" >> "$OUT/logs/PRESERVED.txt" || true
  printf 'PRESERVED %s %s unreadable:%s\n' "$id" "$name" "$failing" >&2
  CREATED=()
  PRESERVED_ID="$id"
}
# confirm_readable <name> <label>...: remove the container only when every result is readable.
confirm_readable() {
  local name="$1" failing
  shift
  failing="$(unreadable_all "$@")"
  if [ -z "$failing" ]; then
    docker rm -f -v "$name" >/dev/null
    CREATED=()
    return 0
  fi
  preserve_container "$name" "$failing"
  if [ "$GATE_BLOCK_EXIT" = 6 ]; then
    echo "GATE_SELFTEST passed: the gate preserved container $PRESERVED_ID ($name)" >&2
  else
    echo "BLOCKED: unreadable results preserved in container $PRESERVED_ID" >&2
  fi
  exit "$GATE_BLOCK_EXIT"
}

cleanup() {
  local name failing
  for name in ${CREATED[@]+"${CREATED[@]}"}; do
    failing="$(unreadable_all ${CUR_LABELS[@]+"${CUR_LABELS[@]}"})"
    if [ -n "$failing" ]; then
      preserve_container "$name" "$failing"
      echo "BLOCKED: unreadable results preserved in container $PRESERVED_ID" >&2
    else
      docker rm -f -v "$name" >/dev/null 2>&1 || true
    fi
  done
  docker rm -f -v "$PROBE" >/dev/null 2>&1 || true
}
PRESERVED_ID=""
trap cleanup EXIT

# df_avail_kib <path>: the Available KiB of the filesystem holding <path>, empty when unreadable.
df_avail_kib() {
  df -Pk "$1" 2>/dev/null | awk 'NR == 2 && $4 ~ /^[0-9]+$/ { print $4 }'
}
# require_free <what>: exit 3 unless /data and $OUT have >= MIN_FREE_GIB GiB and the docker store >= MIN_FREE_GIB_DOCKER.
# The docker store is read through a zero-write probe container (read-only rootfs, tmpfs volumes,
# no network), created once after the first host check and started with -a on every later call.
require_free() {
  local what="$1" failed=0 store avail
  for store in /data "$OUT"; do
    avail="$(df_avail_kib "$store")"
    if [ -z "$avail" ] || [ "$avail" -lt "$MIN_FREE_KIB" ]; then
      echo "BLOCKED: disk preflight ($what): $store has ${avail:-unreadable} KiB free, need >= $MIN_FREE_KIB" >&2
      failed=1
    fi
  done
  # A failing host store creates nothing more, not even the probe.
  if [ "$failed" -eq 1 ] && [ "$PROBE_MADE" -eq 0 ]; then exit 3; fi
  if [ "$PROBE_MADE" -eq 0 ]; then
    if docker create --name "$PROBE" --read-only --network none \
        --mount type=tmpfs,dst=/nix --mount type=tmpfs,dst=/data \
        --entrypoint df "$IMAGE" -Pk / >/dev/null 2>&1; then
      PROBE_MADE=1
    else
      echo "BLOCKED: disk preflight ($what): the docker store probe $PROBE could not be created" >&2
      exit 3
    fi
  fi
  avail="$(docker start -a "$PROBE" 2>/dev/null | awk '$NF == "/" && $4 ~ /^[0-9]+$/ { print $4; exit }' || true)"
  if [ -z "$avail" ] || [ "$avail" -lt "$MIN_FREE_KIB_DOCKER" ]; then
    echo "BLOCKED: disk preflight ($what): docker store has ${avail:-unreadable} KiB free, need >= $MIN_FREE_KIB_DOCKER" >&2
    failed=1
  fi
  if [ "$failed" -eq 1 ]; then exit 3; fi
}

require_free bootstrap
mkdir -p "$OUT/inputs" "$OUT/fixtures" "$OUT/results" "$OUT/tmp" "$OUT/logs"

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
  require_free "create $1"
  log "creating $1 (memory=$2, cpus=$3; the first create of a large image can take minutes)"
  docker create --name "$1" --memory="$2" --memory-swap="$2" --cpus="$3" --network none \
    --user "$(id -u):$(id -g)" -e HOME=/tmp -e UZI_PLANNING_DISCOVERY_MEASURE=1 \
    -e MEASURE_MEM_LIMIT="$2" -e MEASURE_CPUS="$3" \
    --mount "type=bind,src=$REPO/agent,dst=/work/agent,readonly" \
    --mount "type=bind,src=$OUT,dst=/out" \
    -w /work/agent --entrypoint /sbin/tini "$IMAGE" \
    -- node --import tsx test/planning-base-discovery.measure.ts job /out/job.json >/dev/null
  CREATED+=("$1")
  CUR_LABELS=()
}

# run_job <container> <label> <timeout-seconds> <job-json>: start, wait, classify termination.
run_job() {
  local name="$1" label="$2" limit="$3" job="$4" term="$OUT/results/$2.term.json"
  CUR_LABELS+=("$label")
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
  if [ -n "$GATE_SELFTEST" ]; then
    # Prove the readability gate: a measurement-style label whose result file is empty must keep
    # the container. The real gate (confirm_readable) runs with its block exit set to 6, so it exits
    # 6 when it preserves; returning from it means it removed the container, which is a failure.
    run_job "$name" "selftest__gate" "$job_timeout" '{"kind":"noop"}'
    : > "$OUT/results/selftest__gate.json"
    log "GATE_SELFTEST: result file truncated to empty; the gate must preserve $name"
    GATE_BLOCK_EXIT=6
    confirm_readable "$name" "${CUR_LABELS[@]}"
    echo "GATE_SELFTEST FAILED: the gate accepted an empty result file and removed $name" >&2
    exit 5
  fi
  for spec in $FIXTURES; do
    if [ ! -f "$OUT/fixtures/$spec/manifest.json" ] && [ "${generated#* "$spec" }" = "$generated" ]; then
      require_free "fixture $spec"
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
      if [ -n "$LIMITS_JSON" ] || [ -n "$DEADLINE_SCALE" ] || [ -n "$DEADLINE_LIFT" ]; then label="${label}${validation_suffix}"; fi
      run_job "$name" "$label" "$job_timeout" \
        "{\"kind\":\"measure\",\"label\":\"$label\",\"fixture\":\"/out/fixtures/$spec\",\"mode\":\"$mode\",\"out\":\"/out/results\",\"tmp\":\"/out/tmp/$label\",\"injectDeadlineAfterSizing\":$inject$limits_field$deadline_field}"
    done
  done
  # Results are on the bind-mounted $OUT; free this configuration's container and volumes before
  # the next one is created, but only once every result it produced is confirmed readable. The
  # last one stays to render the report and is gated after it.
  if [ "$idx" -lt "$#" ]; then
    confirm_readable "$name" ${CUR_LABELS[@]+"${CUR_LABELS[@]}"}
  fi
done

log "rendering $OUT/results/results.md"
run_job "$name" "report" 300 '{"kind":"report","out":"/out/results"}'
mv "$OUT/results/report.term.json" "$OUT/logs/report.term.json"
confirm_readable "$name" "${CUR_LABELS[@]}"
cat "$OUT/results/results.md"
