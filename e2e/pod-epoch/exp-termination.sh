#!/usr/bin/env bash
# PRD #1860 M0, Q1 "runtime link": when the DinD sidecar container is ended the
# way kubelet ends a container, are the writers of the containers inside it dead
# by the time the outer runtime reports the sidecar `exited`? Each sub-case runs
# a writer container (and a docker-exec'd writer and a backgrounded child, all in
# the inner container's own nested pid namespace) appending a counter to a file on
# the run volume every 100 ms, terminates the daemon container from the outer
# daemon, and reads the file from a separate labelled helper container that
# mounts the same volume read-only: (kill) `docker kill -s KILL`, (stop)
# `docker stop --time 10` (SIGTERM, then SIGKILL after 10 s). It then recreates
# the pod on the same volumes and shows the restored writer container is exited
# and the file still does not grow.
# Stand-in note: this exercises the outer Docker engine's containerd/runc stop
# path in place of kubelet + CRI containerd (same shim/runc family; not the CRI
# plugin). See README.md.
# Usage: exp-termination.sh <rootless|rootful|all>
# Output: RESULT termination <posture> <key>=<value> ... then EXP termination <posture> PASS|FAIL|INCOMPLETE.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$here/lib.sh"
: "${PE_DEADLINE_SECONDS:=1500}"

WINDOW_SECONDS=5
EXIT_POLL_SECONDS=45

# Executed by the helper container (mounts the run volume read-only at /rv).
# Prints one line: size, line count, mtime (stat %y: busybox has no sub-second
# %Y; spaces become _), last line, and the last line of each writer.
READ_SCRIPT='f=/rv/$1/log
[ -e "$f" ] || { echo "missing-log"; exit 1; }
echo "size=$(stat -c %s "$f") lines=$(wc -l < "$f") mt=$(stat -c %y "$f" | tr " " _) last=$(tail -n 1 "$f") m=$(grep -E "^[0-9]+\$" "$f" | tail -n 1) x=$(grep "^x" "$f" | tail -n 1) c=$(grep "^c" "$f" | tail -n 1)"'

now_us() { echo "${EPOCHREALTIME/[.,]/}"; }

# kv <line> <key>: the value of key= in a read_log line.
kv() { sed -n "s/.* $2=\\([^ ]*\\).*/\\1/p" <<<" $1"; }

# read_log <helper> <dir>: read the writers' file through an already running helper.
read_log() {
  pe_outer 20 exec "$1" sh -c "$READ_SCRIPT" rd "$2"
}

# ts_ns <timestamp>: epoch nanoseconds (host GNU date) of a stat %y or RFC 3339 time.
ts_ns() {
  local v
  # An empty or partial value would parse as "today" and pass vacuously.
  [[ $1 =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}[T\ ][0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]+ ]] \
    || pe_fail "missing or malformed time '$1'"
  v=$(date -u -d "$1" +%s%N) || pe_fail "cannot parse time '$1'"
  [[ $v =~ ^[0-9]+$ ]] || pe_fail "bad time '$1' -> '$v'"
  echo "$v"
}

# fresh_read_log <name> <dir>: read it from a new labelled helper container
# (same image, same volume, read-only) into FRESH_OUT, then remove it (verified by
# label). Called directly, not through $(...), so the container is registered for
# the EXIT cleanup in this shell; a failed run or removal fails the experiment.
FRESH_OUT=''
fresh_read_log() {
  local name=$1 rc=0
  FRESH_OUT=''
  PE_CONTAINERS+=("$name")
  pe_fill_tmpfs
  FRESH_OUT=$(pe_outer 60 run --name "$name" --label "$PE_LABEL_KEY=$PE_TOKEN" --user 0:0 \
    --entrypoint sh "${PE_TMPFS_ARGS[@]}" -v "$PE_RUN_VOL:/rv:ro" "$PE_IMAGE" \
    -c "$READ_SCRIPT" rd "$2") || rc=$?
  pe_rm_owned_container "$name" || pe_fail "could not remove helper container $name"
  ((rc == 0)) || pe_fail "fresh helper read failed (rc=$rc)"
}

# same_log <line...>: all lines identical.
same_log() {
  local first=$1 l
  for l in "$@"; do
    [[ $l == "$first" ]] || return 1
  done
}

# writer_procs: number of processes in the daemon container (its whole pid
# namespace, including the inner containers' processes) whose command line
# contains the writers' target file; the pattern is assembled so the checker's
# own command line does not contain it.
writer_procs() {
  pe_root_sh 10 '
    pat="/w/lo""g"
    n=0
    for d in /proc/[0-9]*; do
      p=${d#/proc/}
      [ "$p" != "$$" ] || continue
      c=$(tr "\0" " " 2>/dev/null < "$d/cmdline") || continue
      case "$c" in *"$pat"*) n=$((n + 1)) ;; esac
    done
    echo "$n"'
}

# term_case <kill|stop>
term_case() {
  local mode=$1 dir=tree-W-$1 T w helper st rc=0 n k
  local s0 s1 t_sig t_exit t_read elapsed delivery spid ns_in ns_out
  local at_exit after fresh restored held exitinfo pre_sig fin last_ns fin_ns
  T=$PE_RUN_DIR/$dir
  w=c-writer-$mode
  helper="$PE_DAEMON-rd-$mode"

  pe_inner_sh 20 "mkdir -p '$T'"
  # The writer container: main loop plus a backgrounded child, restart policy no.
  pe_inner 60 run -d --restart no --name "$w" -v "$T:/w" "$PE_FIXTURE_IMAGE" sh -c \
    'k=0; (while :; do k=$((k+1)); echo c$k >> /w/log; sleep 0.1; done) & i=0; while :; do i=$((i+1)); echo $i >> /w/log; sleep 0.1; done' >/dev/null
  # A second writer started with docker exec in the same container.
  pe_inner 30 exec -d "$w" sh -c 'k=0; while :; do k=$((k+1)); echo x$k >> /w/log; sleep 0.1; done'

  # The read-only reader: a labelled helper mounting the run volume, started
  # before the termination so the reading at the instant of `exited` is one
  # fast exec (a fresh container is also used for a final cross-check).
  PE_CONTAINERS+=("$helper")
  pe_fill_tmpfs
  pe_outer 60 run -d --name "$helper" --label "$PE_LABEL_KEY=$PE_TOKEN" --user 0:0 \
    --entrypoint sleep "${PE_TMPFS_ARGS[@]}" -v "$PE_RUN_VOL:/rv:ro" "$PE_IMAGE" 1000000 >/dev/null

  # ---- positive control: the writers run, the log grows, the reader sees it ----
  for ((k = 0; k < 50; k++)); do
    s0=$(read_log "$helper" "$dir" 2>/dev/null || true)
    if [[ -n $s0 && -n $(kv "$s0" m) && -n $(kv "$s0" x) && -n $(kv "$s0" c) ]]; then break; fi
    sleep 0.1
  done
  sleep 1
  s1=$(read_log "$helper" "$dir")
  pe_res "$mode.control.log_t0" "$s0"
  pe_res "$mode.control.log_t1" "$s1"
  ns_in=$(pe_inner 10 exec "$w" readlink /proc/self/ns/pid)
  ns_out=$(pe_root_sh 10 'readlink /proc/self/ns/pid')
  pe_res "$mode.control.pid_namespaces" "daemon_container=$ns_out writer_container=$ns_in"
  n=$(writer_procs)
  pe_res "$mode.control.writer_processes" "$n"
  pe_control "$mode.log-grows-and-all-writers-write" \
    test -n "$s0" -a "$(kv "$s1" size)" -gt "$(kv "$s0" size)" -a -n "$(kv "$s1" m)" -a -n "$(kv "$s1" x)" -a -n "$(kv "$s1" c)"
  pe_control "$mode.writers-in-nested-pid-namespace" test "$ns_in" != "$ns_out" -a "$n" -ge 3
  [[ $PE_EXP_STATUS != INCOMPLETE ]] || pe_exp_finish

  # ---- terminate the daemon container from the outer daemon -------------------
  # The last reading before the signal (writers must still be writing then).
  pre_sig=$(read_log "$helper" "$dir")
  pe_res "$mode.log_before_signal" "$pre_sig"
  t_sig=$(now_us)
  case $mode in
    kill)
      delivery='outer docker kill -s KILL'
      pe_outer 30 kill -s KILL "$PE_DAEMON" >/dev/null
      spid=''
      ;;
    stop)
      delivery='outer docker stop --time 10 (SIGTERM, then SIGKILL after 10 s)'
      pe_outer 40 stop --time 10 "$PE_DAEMON" >/dev/null &
      spid=$!
      ;;
  esac
  st=''
  while ((($(now_us) - t_sig) < EXIT_POLL_SECONDS * 1000000)); do
    st=$(pe_outer 5 inspect --type container -f '{{.State.Status}}' "$PE_DAEMON")
    [[ $st != exited ]] || break
    sleep 0.02
  done
  t_exit=$(now_us)
  # The very next thing after observing `exited`: read the file.
  at_exit=$(read_log "$helper" "$dir")
  t_read=$(now_us)
  if [[ -n $spid ]]; then wait "$spid" || rc=$?; fi
  elapsed=$(((t_exit - t_sig) / 1000))
  pe_res "$mode.delivery" "$delivery"
  pe_check "$mode.exited-observed" test "$st" = exited
  pe_res "$mode.time_to_exited_ms" "$elapsed"
  pe_res "$mode.read_after_exited_ms" "$(((t_read - t_exit) / 1000))"
  pe_res "$mode.stop_command_rc" "$rc"
  exitinfo=$(pe_outer 10 inspect --type container -f 'status={{.State.Status}} exit_code={{.State.ExitCode}} oom={{.State.OOMKilled}} finished={{.State.FinishedAt}}' "$PE_DAEMON")
  pe_res "$mode.daemon_container_state" "$exitinfo"
  pe_res "$mode.log_at_exit" "$at_exit"

  # Supplementary: a bounded window, then the same read again and a read from a
  # fresh helper container.
  sleep "$WINDOW_SECONDS"
  after=$(read_log "$helper" "$dir")
  fresh_read_log "$PE_DAEMON-fr-$mode" "$dir"
  fresh=$FRESH_OUT
  pe_res "$mode.log_after_window.supplementary" "window=${WINDOW_SECONDS}s $after"
  pe_res "$mode.log_fresh_helper.supplementary" "$fresh"
  pe_res "$mode.size_at_exit" "$(kv "$at_exit" size)"
  pe_res "$mode.size_after_window" "$(kv "$after" size)"
  pe_res "$mode.last_counter" "main=$(kv "$after" m) exec=$(kv "$after" x) child=$(kv "$after" c)"
  pe_check "$mode.writers-stopped-by-exit" same_log "$at_exit" "$after" "$fresh"
  pe_check "$mode.writers-active-until-signal" test "$(kv "$at_exit" size)" -gt "$(kv "$pre_sig" size)"
  # The last write (file mtime, kernel clock) is not after the outer runtime's
  # recorded exit time (dockerd, same host clock). Both values are recorded.
  fin=$(pe_outer 10 inspect --type container -f '{{.State.FinishedAt}}' "$PE_DAEMON")
  last_ns=$(ts_ns "$(kv "$after" mt | tr _ ' ')")
  fin_ns=$(ts_ns "$fin")
  pe_res "$mode.last_write_mtime" "$(kv "$after" mt)"
  pe_res "$mode.finished_at" "$fin"
  pe_res "$mode.finished_minus_last_write_ms" "$(((fin_ns - last_ns) / 1000000))"
  pe_check "$mode.last-write-not-after-exit" test "$last_ns" -le "$fin_ns"

  # ---- recreate the pod on the same volumes; the restored writer is exited ------
  pe_daemon_replace
  st=$(pe_inner 10 inspect -f '{{.State.Status}}|{{.State.ExitCode}}|{{.RestartCount}}|{{.HostConfig.RestartPolicy.Name}}' "$w")
  pe_res "$mode.restored.writer_container" "$st"
  # Observation, not a check: a stopped-state read cannot see an old-epoch survivor.
  pe_res "obs.$mode.restored.writer-exited" "$([[ ${st%%|*} == exited ]] && echo yes || echo no)"
  pe_check "$mode.restored.restart-policy-no" test "${st##*|}" = no
  pe_res "$mode.restored.running_containers" "$(pe_inner 10 ps -q | wc -l)"
  sleep 3
  restored=$(read_log "$helper" "$dir")
  pe_res "$mode.restored.log_after_3s" "$restored"
  pe_check "$mode.restored.log-does-not-grow" same_log "$at_exit" "$restored"
  held=$(writer_procs)
  pe_res "$mode.restored.writer_processes" "$held"
  # Observation, not a check: it counts processes of the new pod's pid namespace only.
  pe_res "obs.$mode.restored.no-writer-processes" "$([[ $held == 0 ]] && echo yes || echo no)"
  pe_rm_owned_container "$helper"
}

run_posture() {
  local posture=$1
  pe_init "$posture"
  pe_exp_begin termination
  pe_volumes_create
  pe_daemon_create_start
  pe_res engine "$(pe_inner 10 version --format '{{.Server.Version}}')"
  pe_res outer_engine "$(pe_outer 10 version --format '{{.Server.Version}}')"
  if ! pe_inner 10 image inspect "$PE_FIXTURE_IMAGE" >/dev/null 2>&1; then
    pe_inner 240 pull "$PE_FIXTURE_IMAGE" >/dev/null
  fi
  term_case kill
  term_case stop
  pe_exp_finish
}

pe_exp_main termination run_posture "${1:-}"
