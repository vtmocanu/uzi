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
#      All backup-runs.sh env vars (UZI_CTX, UZI_WORKER_NS, UZI_REPO_SLUG, ...)
#      are inherited and honored.
set -u
export PATH="${PATH:-/usr/local/bin:/usr/bin:/bin}"
UZI="${UZI_BIN:-uzi}"
ROOT="${UZI_BACKUP_DIR:-/tmp/uzi-backups}"
HERE="$(cd "$(dirname "$0")" && pwd)"
RUNS_SCRIPT="$HERE/backup-runs.sh"
INTERVAL="${UZI_BACKUP_INTERVAL:-900}"
MAX_HOURS="${UZI_BACKUP_MAX_HOURS:-12}"

if [ "$#" -eq 0 ]; then
  echo "usage: backup-loop.sh <RUN_ID> [RUN_ID ...]" >&2
  exit 2
fi
RUNS=("$@")

mkdir -p "$ROOT"
echo "$$" > "$ROOT/backup-loop.pid"
END=$(( $(date +%s) + MAX_HOURS * 3600 ))

llog(){ printf '%s [loop] %s\n' "$(date -u +%FT%TZ)" "$*"; }
llog "started pid=$$ interval=${INTERVAL}s max=${MAX_HOURS}h runs=${RUNS[*]}"

while :; do
  [ -e "$ROOT/STOP" ] && { llog "STOP file present; exiting"; break; }
  bash "$RUNS_SCRIPT" "${RUNS[@]}"

  # exit once every run is definitively terminal (empty status = keep going)
  active=0
  for RID in "${RUNS[@]}"; do
    s="$("$UZI" run get "$RID" --field status 2>/dev/null)"
    case "$s" in
      completed|failed|cancelled) ;;
      *) active=1 ;;
    esac
  done
  [ "$active" -eq 0 ] && { llog "all runs terminal; exiting"; break; }
  [ "$(date +%s)" -ge "$END" ] && { llog "max runtime reached; exiting"; break; }

  llog "sleep ${INTERVAL}s"
  sleep "$INTERVAL"
done
rm -f "$ROOT/backup-loop.pid"
llog "loop ended"
