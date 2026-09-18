#!/usr/bin/env bash
# Detached backup loop: runs backup-runs.sh every INTERVAL seconds for the given
# run ids, independent of any Claude/terminal session. Self-terminates when every
# target run is terminal, when MAX_HOURS elapses, or when a STOP file appears.
#
# Launch it DETACHED so it outlives the launching shell:
#   Linux:  setsid bash backup-loop.sh <RUN_ID>... </dev/null >>/tmp/uzi-backups/loop.log 2>&1 &
#   macOS:  ( nohup bash backup-loop.sh <RUN_ID>... </dev/null >>/tmp/uzi-backups/loop.log 2>&1 & )
#           (no setsid on macOS; the subshell double-fork orphans it to init)
#
# Stop it:  touch "$UZI_BACKUP_DIR/STOP"   (default /tmp/uzi-backups/STOP)
#     or:   kill "$(cat "$UZI_BACKUP_DIR/backup-loop.pid")"
#
# Env: UZI_BACKUP_INTERVAL (default 900s), UZI_BACKUP_MAX_HOURS (default 12),
#      UZI_BACKUP_DIR (default /tmp/uzi-backups), UZI_BIN (uzi path).
#      UZI_BACKUP_RUNS_SCRIPT overrides the one-shot script (test/custom install).
#      All backup-runs.sh env vars (UZI_CTX, UZI_WORKER_NS, UZI_REPO_SLUG,
#      UZI_BACKUP_RETENTION_DAYS, ...) are inherited and honored.
set -u
export PATH="${PATH:-/usr/local/bin:/usr/bin:/bin}"
UZI="${UZI_BIN:-uzi}"
ROOT="${UZI_BACKUP_DIR:-/tmp/uzi-backups}"
HERE="$(cd "$(dirname "$0")" && pwd)"
RUNS_SCRIPT="${UZI_BACKUP_RUNS_SCRIPT:-$HERE/backup-runs.sh}"
INTERVAL="${UZI_BACKUP_INTERVAL:-900}"
MAX_HOURS="${UZI_BACKUP_MAX_HOURS:-12}"

if [ "$#" -eq 0 ]; then
  echo "usage: backup-loop.sh <RUN_ID> [RUN_ID ...]" >&2
  exit 2
fi
RUNS=("$@")

# Reject a nonpositive/non-integer interval or window: sleep 0 would spin the
# loop with no delay and flood the uzi + k8s APIs until MAX_HOURS.
for pair in "INTERVAL:$INTERVAL" "MAX_HOURS:$MAX_HOURS"; do
  name="${pair%%:*}"; val="${pair#*:}"
  case "$val" in ''|*[!0-9]*) echo "error: UZI_BACKUP_$name must be a positive integer (got '$val')" >&2; exit 2;; esac
  # 10# forces base-10 so a leading-zero value (e.g. 08, 09) is not read as
  # invalid octal here or in the arithmetic below.
  [ "$((10#$val))" -ge 1 ] || { echo "error: UZI_BACKUP_$name must be >= 1 (got '$val')" >&2; exit 2; }
done
INTERVAL=$((10#$INTERVAL)); MAX_HOURS=$((10#$MAX_HOURS))

mkdir -p "$ROOT"
echo "$$" > "$ROOT/backup-loop.pid"
START_EPOCH="$(date +%s)"
END=$(( START_EPOCH + MAX_HOURS * 3600 ))

llog(){ printf '%s [loop] %s\n' "$(date -u +%FT%TZ)" "$*"; }
epoch_utc(){
  date -u -r "$1" +%FT%TZ 2>/dev/null || date -u -d "@$1" +%FT%TZ 2>/dev/null
}
STARTED_AT="$(epoch_utc "$START_EPOCH")"
ENDS_AT="$(epoch_utc "$END")"
{
  echo "pid=$$"
  echo "status=running"
  echo "context=${UZI_CTX:-<unset: current kube context>}"
  echo "namespaces=${UZI_WORKER_NS:-uzi-workers uzi-workers-docker}"
  echo "interval_seconds=$INTERVAL"
  echo "max_hours=$MAX_HOURS"
  echo "started_at=$STARTED_AT"
  echo "ends_at=$ENDS_AT"
  echo "retention_days=${UZI_BACKUP_RETENTION_DAYS:-14}"
  echo "runs=${RUNS[*]}"
} > "$ROOT/backup-loop.state"
llog "started pid=$$ ctx=${UZI_CTX:-<unset>} ns=[${UZI_WORKER_NS:-uzi-workers uzi-workers-docker}] interval=${INTERVAL}s max=${MAX_HOURS}h ends=$ENDS_AT retention=${UZI_BACKUP_RETENTION_DAYS:-14}d runs=${RUNS[*]}"

while :; do
  [ -e "$ROOT/STOP" ] && { llog "STOP file present; exiting"; break; }
  bash "$RUNS_SCRIPT" "${RUNS[@]}"
  backup_rc=$?
  [ "$backup_rc" -eq 0 ] || llog "backup cycle incomplete rc=$backup_rc; keeping active runs for retry"

  # Stop re-snapshotting a terminal run every cycle. Empty/unreadable status stays
  # active and is retried rather than silently disappearing from protection.
  next_runs=()
  for RID in "${RUNS[@]}"; do
    s="$("$UZI" run get "$RID" --field status 2>/dev/null)"
    case "$s" in
      completed|failed|cancelled) llog "retired terminal run $RID status=$s" ;;
      *) next_runs+=("$RID") ;;
    esac
  done
  RUNS=("${next_runs[@]}")
  [ "${#RUNS[@]}" -eq 0 ] && { llog "all runs terminal; exiting"; break; }
  [ "$(date +%s)" -ge "$END" ] && { llog "max runtime reached; exiting"; break; }

  llog "sleep ${INTERVAL}s"
  sleep "$INTERVAL"
done
rm -f "$ROOT/backup-loop.pid"
{
  echo "status=ended"
  echo "ended_at=$(date -u +%FT%TZ)"
} >> "$ROOT/backup-loop.state"
llog "loop ended"
