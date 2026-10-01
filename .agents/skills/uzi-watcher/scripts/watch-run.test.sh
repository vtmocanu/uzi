#!/usr/bin/env bash
# Hermetic regression: watch-run.sh's attention stop. A dead run can read `running`, so a
# persistent non-ok health or a stale worker heartbeat must stop the poll with exit 4 and
# the evidence, while a transient single poll, a healthy run, `slow`, and the existing stop
# statuses keep their old behaviour (exit 0).
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/watch-run.sh"
WORK="$(mktemp -d)"; export WORK
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$WORK/bin"
# The stub reads scenario files from $WORK: STATUS (one status per poll, last repeats),
# HEALTH (same per-poll cadence), RUN_JSON template pieces, WORKERS (json array).
cat > "$WORK/bin/uzi" <<'STUB'
#!/usr/bin/env bash
set -eu
pollfile() { # pollfile NAME -> the line for the current poll (last line repeats)
  n=$(cat "$WORK/poll" 2>/dev/null || echo 0)
  line=$(sed -n "$((n + 1))p" "$WORK/$1"); [ -n "$line" ] || line=$(tail -n1 "$WORK/$1")
  printf '%s' "$line"
}
case "$*" in
  "run get "*" --field status") pollfile status; echo; echo $(( $(cat "$WORK/poll" 2>/dev/null || echo 0) + 1 )) > "$WORK/poll.next" ;;
  "run get "*" --json")
    jq -n --arg h "$(pollfile health)" --arg w "$(cat "$WORK/worker_id")" \
      '{status:"running",health:$h,health_reason:(if $h=="stalled" then "no progress 20m" else null end),health_since:null,worker_id:(if $w=="" then null else $w end)}'
    mv "$WORK/poll.next" "$WORK/poll" ;;
  "worker list --json") cat "$WORK/workers" ;;
  *) echo "unexpected uzi call: $*" >&2; exit 1 ;;
esac
STUB
chmod +x "$WORK/bin/uzi"

# scenario NAME STATUSES HEALTHS WORKER_ID WORKERS_JSON
reset() { printf '%s\n' "$1" > "$WORK/status"; printf '%s\n' "$2" > "$WORK/health"; printf '%s' "$3" > "$WORK/worker_id"; printf '%s\n' "$4" > "$WORK/workers"; rm -f "$WORK/poll" "$WORK/poll.next"; echo 0 > "$WORK/poll"; }
run() { PATH="$WORK/bin:$PATH" bash "$SCRIPT" rid completed,failed,cancelled 0 "${1:-6}" 2>/dev/null; }
fresh=$(date -u +%Y-%m-%dT%H:%M:%S.123456Z)
old=$(date -u -d '-1 hour' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v-1H +%Y-%m-%dT%H:%M:%SZ)

# 1. persistent stalled health -> exit 4 with evidence
reset running stalled "" '[]'
rc=0; out=$(run) || rc=$?
[ "$rc" -eq 4 ] || fail "persistent stalled: want exit 4, got $rc: $out"
case "$out" in *STOP=needs_attention*"health=stalled"*"no progress 20m"*) ;; *) fail "evidence missing: $out";; esac
case "$out" in *"does not prove the run is dead"*) ;; *) fail "wording missing: $out";; esac

# 2. a single stalled poll amid ok polls does not fire
reset running "$(printf 'ok\nstalled\nok\nok\nok\nok')" "" '[]'
rc=0; out=$(run) || rc=$?
[ "$rc" -eq 0 ] || fail "transient stalled: want exit 0, got $rc: $out"
case "$out" in *needs_attention*) fail "transient poll fired: $out";; esac

# 3. stale worker heartbeat on a running run -> exit 4, worker evidence
reset running ok w1 "[{\"id\":\"w1\",\"status\":\"online\",\"last_heartbeat_at\":\"$old\"}]"
rc=0; out=$(run) || rc=$?
[ "$rc" -eq 4 ] || fail "stale heartbeat: want exit 4, got $rc: $out"
case "$out" in *"worker=w1"*"heartbeat_age_secs="[0-9]*) ;; *) fail "heartbeat evidence missing: $out";; esac

# 4. fresh heartbeat -> no stop (ELAPSED, exit 0)
reset running ok w1 "[{\"id\":\"w1\",\"status\":\"online\",\"last_heartbeat_at\":\"$fresh\"}]"
rc=0; out=$(run) || rc=$?
[ "$rc" -eq 0 ] || fail "fresh heartbeat: want exit 0, got $rc: $out"
case "$out" in *ELAPSED*) ;; *) fail "fresh heartbeat should poll to ELAPSED: $out";; esac

# 5. unreadable worker list is no signal
reset running ok w1 'not json'
rc=0; out=$(run) || rc=$?
[ "$rc" -eq 0 ] || fail "unreadable worker list fired: $rc: $out"

# 6. slow health is ignored by default
reset running slow "" '[]'
rc=0; out=$(run) || rc=$?
[ "$rc" -eq 0 ] || fail "slow health fired: $rc: $out"

# 7. WATCH_ATTENTION_POLLS=0 disables
reset running stalled "" '[]'
rc=0; out=$(WATCH_ATTENTION_POLLS=0 run) || rc=$?
[ "$rc" -eq 0 ] || fail "disabled check fired: $rc: $out"

# 8. existing stop status still exits 0 with STOP=<status>
reset "$(printf 'running\ncompleted')" ok "" '[]'
rc=0; out=$(run) || rc=$?
[ "$rc" -eq 0 ] || fail "completed stop: want exit 0, got $rc: $out"
case "$out" in *"STOP=completed"*) ;; *) fail "completed stop line missing: $out";; esac

echo "PASS: watch-run attention stop"
