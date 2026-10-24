#!/usr/bin/env bash
# PRD #1860 M0, Q4/Q6: the reclaim predicate's inventory queries are read-only.
# With stopped containers and volumes present, an events stream is held open, the
# inventory is snapshotted, exactly the read-only queries are issued, and the
# inventory is snapshotted again: it must be byte-identical and the stream must
# carry zero events from the query phase. Positive control: a volume create and
# a container start then DO appear in the stream and DO change the inventory.
# Usage: exp-readonly.sh <rootless|rootful|all>
# Output: RESULT readonly <posture> <key>=<value> ... then EXP readonly <posture> PASS|FAIL|INCOMPLETE.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$here/lib.sh"
: "${PE_DEADLINE_SECONDS:=900}"

# snapshot <outfile>: containers (incl. State.StartedAt via inspect), volumes,
# images, networks and plugins, in a stable order. Read-only itself.
snapshot() {
  local out=$1 ids vols
  {
    echo '## containers'
    pe_inner 30 ps -a --no-trunc --format '{{.ID}}|{{.Names}}|{{.Image}}|{{.State}}' | sort
    echo '## container inspect'
    ids=$(pe_inner 30 ps -a -q --no-trunc | sort)
    # shellcheck disable=SC2086
    [[ -z $ids ]] || pe_inner 30 inspect $ids
    echo '## volumes'
    pe_inner 30 volume ls --format '{{.Driver}}|{{.Name}}' | sort
    vols=$(pe_inner 30 volume ls -q | sort)
    # shellcheck disable=SC2086
    [[ -z $vols ]] || pe_inner 30 volume inspect $vols
    echo '## images'
    pe_inner 30 images --no-trunc --digests --format '{{.ID}}|{{.Repository}}|{{.Tag}}|{{.Digest}}' | sort
    echo '## networks'
    pe_inner 30 network ls --no-trunc --format '{{.ID}}|{{.Name}}|{{.Driver}}|{{.Scope}}' | sort
    echo '## plugins'
    pe_inner 30 plugin ls -q | sort
  } >"$out"
}

# stream_line <file> <after-line> <string> [<string> [<string>]]: number of the
# first line after <after-line> holding every fixed string; status 1 if none.
stream_line() {
  awk -v after="$2" -v a="$3" -v b="${4:-}" -v c="${5:-}" '
    NR > after && index($0, a) > 0 && (b == "" || index($0, b) > 0) && (c == "" || index($0, c) > 0) {
      print NR; found = 1; exit }
    END { exit found ? 0 : 1 }' "$1"
}

# wait_stream <file> <after-line> <string>...: bounded wait (60 x 0.2 s) until a
# line after <after-line> holds every string.
wait_stream() {
  local file=$1 after=$2 i
  shift 2
  for ((i = 0; i < 60; i++)); do
    if stream_line "$file" "$after" "$@" >/dev/null; then return 0; fi
    sleep 0.2
  done
  return 1
}

run_posture() {
  local posture=$1 img ev snap1 snap2 snap3 scratch t0 t1 l1 l2 between names n ctl_vol_line ctl_start_line c v
  pe_init "$posture"
  pe_exp_begin readonly
  img=$PE_FIXTURE_IMAGE
  pe_volumes_create
  pe_daemon_create_start
  pe_res engine "$(pe_inner 10 version --format '{{.Server.Version}}')"
  if ! pe_inner 10 image inspect "$img" >/dev/null 2>&1; then
    pe_inner 240 pull "$img" >/dev/null
  fi
  mkdir -p "$here/../../.uzi/scratch"
  scratch=$(mktemp -d "$here/../../.uzi/scratch/pe-readonly.XXXXXX")
  ev=$scratch/events
  snap1=$scratch/snap1
  snap2=$scratch/snap2
  snap3=$scratch/snap3

  # Stream first (--since taken before everything it must see), then fixtures.
  pe_events_start "$ev"
  pe_inner 60 run --name c-exited "$img" true >/dev/null
  pe_inner 60 run -d --name c-stopped "$img" sleep 600 >/dev/null
  pe_inner 60 stop -t 2 c-stopped >/dev/null
  pe_inner 60 create --name c-created "$img" true >/dev/null
  pe_inner 20 volume create v1 >/dev/null
  pe_inner 20 volume create v2 >/dev/null
  wait_stream "$ev" 0 '"Action":"die"' 'c-exited' || pe_fail 'no die event for c-exited'
  wait_stream "$ev" 0 '"Action":"die"' 'c-stopped' || pe_fail 'no die event for c-stopped'
  wait_stream "$ev" 0 '"Action":"create"' 'v2' || pe_fail 'no create event for v2'
  names=$(pe_inner 10 ps -a --format '{{.Names}}' | sort | tr '\n' ' ')
  n=$(pe_inner 10 ps -q | wc -l)
  pe_res fixtures "containers=[$names] running=$n"
  pe_check fixtures.nothing-running test "$n" -eq 0

  # Marker 1: once it shows in the stream, every earlier event has been
  # delivered (events are ordered), so anything after it is query-phase output.
  t0=$(pe_inner_sh 5 'date +%s')
  pe_inner 20 volume create m-mark-1 >/dev/null
  wait_stream "$ev" 0 '"Action":"create"' 'm-mark-1' || pe_fail 'marker 1 never reached the stream'
  l1=$(stream_line "$ev" 0 '"Action":"create"' 'm-mark-1')

  snapshot "$snap1"
  pe_res snapshot1.bytes "$(wc -c <"$snap1")"
  pe_check snapshot1.has-all-kinds test "$(grep -c '^## ' "$snap1")" -eq 6

  # ---- the reclaim predicate's read-only queries -------------------------------
  pe_inner 30 version >/dev/null # includes /_ping (HEAD, GET fallback)
  pe_inner 30 info >/dev/null 2>&1
  pe_inner 30 ps -a --no-trunc --format json >/dev/null
  for c in c-exited c-stopped c-created; do
    pe_inner 30 inspect "$c" >/dev/null
  done
  pe_inner 30 volume ls >/dev/null
  for v in v1 v2 m-mark-1; do
    pe_inner 30 volume inspect "$v" >/dev/null
  done
  pe_inner 30 plugin ls >/dev/null
  t1=$(pe_inner_sh 5 'date +%s')
  pe_inner 30 events --since "$t0" --until "$t1" --format '{{json .}}' >"$scratch/events-read"
  pe_res queries.events_read_lines "$(wc -l <"$scratch/events-read")"
  pe_res queries.issued "version(_ping) info ps-a-json inspect-each volume-ls volume-inspect-each plugin-ls events-since-until"

  snapshot "$snap2"
  if cmp -s "$snap1" "$snap2"; then
    pe_res inventory.identical yes
  else
    pe_res inventory.identical no
    pe_res inventory.diff "$(diff "$snap1" "$snap2" | head -6 | cut -c1-200)"
    pe_mark_fail
  fi

  # ---- positive control: state changes DO appear and DO change the inventory --
  pe_inner 20 volume create m-ctl-vol >/dev/null
  pe_inner 60 start c-stopped >/dev/null
  wait_stream "$ev" "$l1" '"Action":"create"' 'm-ctl-vol' || pe_fail 'positive control: volume create never reached the stream'
  wait_stream "$ev" "$l1" '"Action":"start"' 'c-stopped' || pe_fail 'positive control: container start never reached the stream'
  ctl_vol_line=$(stream_line "$ev" "$l1" '"Action":"create"' 'm-ctl-vol')
  ctl_start_line=$(stream_line "$ev" "$l1" '"Action":"start"' 'c-stopped')
  l2=$ctl_vol_line
  ((ctl_start_line >= l2)) || l2=$ctl_start_line
  snapshot "$snap3"
  if cmp -s "$snap2" "$snap3"; then
    pe_res control.inventory_changed no
    pe_mark_fail
  else
    pe_res control.inventory_changed yes
  fi
  pe_res control.events "volume-create-line=$ctl_vol_line container-start-line=$ctl_start_line"

  # Events strictly between marker 1 and the first control event.
  between=$((l2 - l1 - 1))
  pe_res stream.marker1_line "$l1"
  pe_res stream.first_control_line "$l2"
  pe_res stream.events_during_query_phase "$between"
  if ((between != 0)); then
    pe_res stream.unexpected "$(sed -n "$((l1 + 1)),$((l2 - 1))p" "$ev" | cut -c1-240)"
  fi
  pe_check stream.zero-events-during-query-phase test "$between" -eq 0

  # The stream must have been continuously open (one stream, never restarted).
  pe_check stream.still-open kill -0 "$PE_EVENTS_PID"
  pe_events_stop || pe_fail 'events client not verified gone'
  pe_res stream.end "$PE_EVENTS_END"
  rm -rf "$scratch"
  pe_exp_finish
}

pe_exp_main readonly run_posture "${1:-}"
