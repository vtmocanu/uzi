#!/usr/bin/env bash
# PRD #1860 M0, Q2 "restore barrier":
#   1. restart-policy containers on a persisted data root: what is already
#      started when the daemon first answers, after a sidecar restart and after a
#      successor pod, and does anything change afterwards (events window);
#   2. the barrier predicate on an inventory of only stopped `no` containers;
#   3. detection of a daemon restart with an events stream held by a client in a
#      different container (graceful restart, SIGKILL, SIGTERM).
# Usage: exp-restore.sh <rootless|rootful|all>
# Output: RESULT restore <posture> <key>=<value> ... then EXP restore <posture> PASS|FAIL|INCOMPLETE.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$here/lib.sh"
: "${PE_DEADLINE_SECONDS:=1500}"

readonly WINDOW_SECONDS=20
readonly NAMES=(c-always c-always-stopped c-unless c-unless-stopped c-onfail c-no c-no-stopped)

declare -A PRE_STATUS PRE_STARTED
declare -A FR_STATUS FR_STARTED FR_EXIT FR_RESTARTS FR_POLICY FR_LIST
declare -A END_STATUS END_STARTED END_RESTARTS
FR_SWARM='' FR_LIVE='' FR_ID='' FR_TIME='' FR_PLUGINS=0 FR_PROBES=''
CTL_RUNNING_AT_FIRST=0
CTL_NONNO_POLICY=0
SCRATCH=''

policy_of() {
  case $1 in
    c-always*) echo always ;;
    c-unless*) echo unless-stopped ;;
    c-onfail) echo on-failure ;;
    *) echo no ;;
  esac
}

# rm_known: remove every container this script may have created, by exact name.
rm_known() {
  local n
  for n in "${NAMES[@]}" b-exited b-stopped evctl; do
    pe_inner 60 rm -f "$n" >/dev/null 2>&1 || true
  done
}

setup_containers() {
  local n
  rm_known
  for n in "${NAMES[@]}"; do
    pe_inner 60 run -d --name "$n" --restart "$(policy_of "$n")" "$PE_FIXTURE_IMAGE" sleep 3600 >/dev/null
    if [[ $n == *-stopped ]]; then pe_inner 60 stop -t 1 "$n" >/dev/null; fi
  done
}

record_pre() {
  local n line
  for n in "${NAMES[@]}"; do
    line=$(pe_inner 10 inspect -f '{{.State.Status}}|{{.State.StartedAt}}' "$n")
    PRE_STATUS[$n]=${line%%|*}
    PRE_STARTED[$n]=${line#*|}
  done
}

# parse_fr <file>: fill FR_* from a first-response poller output.
parse_fr() {
  local file=$1 line rest nm st sa ec rc pol fr_done=0
  FR_STATUS=() FR_STARTED=() FR_EXIT=() FR_RESTARTS=() FR_POLICY=() FR_LIST=()
  FR_SWARM='' FR_LIVE='' FR_ID='' FR_TIME='' FR_PLUGINS=0 FR_PROBES=''
  while IFS= read -r line; do
    case $line in
      'FR_INFO '*)
        [[ $line =~ swarm=([^\ ]+) ]] && FR_SWARM=${BASH_REMATCH[1]}
        [[ $line =~ live=([^\ ]+) ]] && FR_LIVE=${BASH_REMATCH[1]}
        [[ $line =~ id=([^\ ]+) ]] && FR_ID=${BASH_REMATCH[1]}
        [[ $line =~ time=([^\ ]+) ]] && FR_TIME=${BASH_REMATCH[1]}
        ;;
      'FR_PROBES '*) FR_PROBES=${line#FR_PROBES } ;;
      'FR_PS '*)
        rest=${line#FR_PS }
        IFS='|' read -r _ nm st _ <<<"$rest"
        FR_LIST[$nm]=$st
        ;;
      'FR_INSPECT '*)
        rest=${line#FR_INSPECT /}
        IFS='|' read -r nm st sa ec rc pol <<<"$rest"
        FR_STATUS[$nm]=$st
        FR_STARTED[$nm]=$sa
        FR_EXIT[$nm]=$ec
        FR_RESTARTS[$nm]=$rc
        FR_POLICY[$nm]=$pol
        ;;
      'FR_PLUGIN '*) FR_PLUGINS=$((FR_PLUGINS + 1)) ;;
      FR_DONE) fr_done=1 ;;
      FR_ERR*) pe_fail "first-response poller: $line" ;;
    esac
  done <"$file"
  ((fr_done)) || pe_fail "first-response poller did not finish (no FR_DONE): $(tail -3 "$file")"
  [[ -n $FR_TIME && -n $FR_ID ]] || pe_fail "first-response poller produced no info line: $(head -3 "$file")"
}

count_fr_not_at_rest() { # containers running, restarting or paused at the first response
  local n c=0 s
  for n in "${!FR_STATUS[@]}"; do
    s=${FR_STATUS[$n]}
    if [[ $s == running || $s == restarting || $s == paused ]]; then c=$((c + 1)); fi
  done
  echo "$c"
}

# read_end_states: inspect every container now (end of the window).
read_end_states() {
  local n line
  END_STATUS=() END_STARTED=() END_RESTARTS=()
  for n in "${NAMES[@]}"; do
    line=$(pe_client 10 inspect -f '{{.State.Status}}|{{.State.StartedAt}}|{{.RestartCount}}' "$n")
    END_STATUS[$n]=${line%%|*}
    line=${line#*|}
    END_STARTED[$n]=${line%%|*}
    END_RESTARTS[$n]=${line#*|}
  done
}

# daemon_log_tail: the daemon container's last log lines, for failure messages.
daemon_log_tail() { timeout --kill-after=2s 10s docker logs --tail 6 "$PE_DAEMON" 2>&1 | cut -c1-200 | tr '\n' '|' || true; }

# window_run <seconds> <since> <outfile>: `docker events --since <since>` in the
# worker container, killed by `timeout` after <seconds>. Sets WIN_RC (exit status
# of the whole command) and WIN_MS (elapsed milliseconds); stderr goes to
# <outfile>.err. Never aborts on a nonzero status: the caller judges it.
window_run() {
  local secs=$1 since=$2 out=$3 t0 t1
  WIN_RC=0
  t0=${EPOCHREALTIME/[.,]/}
  pe_outer $((secs + 30)) exec "$PE_WORKER" env -i "PATH=$PE_PATH" sh -c \
    "timeout $secs docker --host '$PE_ENDPOINT' events --since '$since' --format '{{json .}}'" \
    >"$out" 2>"$out.err" || WIN_RC=$?
  t1=${EPOCHREALTIME/[.,]/}
  WIN_MS=$(((t1 - t0) / 1000))
}

run_case() { # run_case <restart|replace>
  local mode=$1 fr evf n restarted stable=1 nev ctl_t ctl_lines running_at_first win_rc win_ms win_ok
  fr=$SCRATCH/fr-$mode
  evf=$SCRATCH/window-$mode
  setup_containers
  record_pre
  for n in "${NAMES[@]}"; do
    pe_res "$mode.pre.$n" "status=${PRE_STATUS[$n]} started=${PRE_STARTED[$n]}"
  done
  case $mode in
    restart)
      pe_first_response_start "$fr"
      # --time 30: four fixtures run `sleep` as PID 1 (ignores SIGTERM), so
      # dockerd's own shutdown waits out their 10 s stop timeout. A 10 s kill
      # timeout was observed to be followed by "containerd is still running"
      # and a daemon that never answered (suspected cause: shutdown preempted).
      pe_outer 60 restart --time 30 "$PE_DAEMON" >/dev/null
      ;;
    replace)
      # A successor pod: the old daemon is killed with its pod; the new pod starts
      # on the same volumes. The poller must live in the new worker container.
      pe_pod_remove
      pe_pod_create
      pe_outer 30 start "$PE_PAUSE" >/dev/null
      pe_outer 30 start "$PE_WORKER" >/dev/null
      pe_first_response_start "$fr"
      pe_outer 30 start "$PE_DAEMON" >/dev/null
      ;;
  esac
  pe_first_response_wait || pe_fail "first-response poller failed ($mode) rc=$PE_FR_RC bytes=$(wc -c <"$fr"): $(tail -3 "$fr") daemon-log: $(daemon_log_tail)"
  parse_fr "$fr"
  pe_assert_no_public_listener
  pe_res "$mode.first_response.time" "$FR_TIME"
  pe_res "$mode.first_response.probes_before_success" "$FR_PROBES"
  pe_res "$mode.first_response.info" "swarm=$FR_SWARM live_restore=$FR_LIVE plugins=$FR_PLUGINS engine_id=$FR_ID"

  # The window: an events stream opened with --since = the first-response time.
  # Supplementary: a bounded observation, valid only with its positive control.
  window_run "$WINDOW_SECONDS" "$FR_TIME" "$evf"
  win_rc=$WIN_RC win_ms=$WIN_MS
  pe_res "$mode.window.rc" "$win_rc elapsed_ms=$win_ms"
  pe_res "$mode.window.stderr_lines" "$(wc -l <"$evf.err")"
  read_end_states

  running_at_first=0
  for n in "${NAMES[@]}"; do
    restarted=no
    if [[ ${FR_STARTED[$n]} != "${PRE_STARTED[$n]}" ]]; then restarted=yes; fi
    pe_res "$mode.first.$n" "status=${FR_STATUS[$n]} list_state=${FR_LIST[$n]:-?} restarted_by_first_response=$restarted exit=${FR_EXIT[$n]} restart_count=${FR_RESTARTS[$n]} policy=${FR_POLICY[$n]}"
    pe_res "$mode.end.$n" "status=${END_STATUS[$n]} changed_since_first_response=$([[ ${END_STATUS[$n]} == "${FR_STATUS[$n]}" && ${END_STARTED[$n]} == "${FR_STARTED[$n]}" && ${END_RESTARTS[$n]} == "${FR_RESTARTS[$n]}" ]] && echo no || echo yes)"
    if [[ ${END_STATUS[$n]} != "${FR_STATUS[$n]}" || ${END_STARTED[$n]} != "${FR_STARTED[$n]}" || ${END_RESTARTS[$n]} != "${FR_RESTARTS[$n]}" ]]; then
      stable=0
    fi
    [[ ${FR_STATUS[$n]} != running ]] || running_at_first=$((running_at_first + 1))
    [[ ${FR_POLICY[$n]} == no ]] || CTL_NONNO_POLICY=$((CTL_NONNO_POLICY + 1))
  done
  ((running_at_first <= CTL_RUNNING_AT_FIRST)) || CTL_RUNNING_AT_FIRST=$running_at_first
  pe_res "$mode.running_at_first_response" "$running_at_first"

  # c-no and c-no-stopped must not have been started by restore.
  for n in c-no c-no-stopped; do
    pe_check "$mode.$n.not-started" test "${FR_STATUS[$n]}" != running -a "${FR_STARTED[$n]}" = "${PRE_STARTED[$n]}" -a "${END_STATUS[$n]}" != running
  done

  nev=$(wc -l <"$evf")
  pe_res "$mode.window.seconds" "$WINDOW_SECONDS (bounded, supplementary)"
  pe_res "$mode.window.events_after_first_response" "$nev"
  if ((nev > 0)); then
    pe_res "$mode.window.events" "$(cut -c1-220 "$evf" | head -8)"
  fi
  pe_check "$mode.window.no-events-after-first-response" test "$nev" -eq 0
  pe_check "$mode.window.states-unchanged" test "$stable" -eq 1

  # Positive control for the window instrument: the same command shape and the
  # same --since form (RFC3339Nano daemon SystemTime), with a container run
  # (create/start/die/destroy) that it must see. Its exit status is also the
  # reference for what this image's `timeout` returns when it kills the client.
  ctl_t=$(pe_client 10 info --format '{{.SystemTime}}')
  pe_client 60 run --rm --name evctl "$PE_FIXTURE_IMAGE" true >/dev/null
  window_run 3 "$ctl_t" "$SCRATCH/ctl-$mode"
  ctl_lines=$(wc -l <"$SCRATCH/ctl-$mode")
  pe_res "$mode.window.control_events" "$ctl_lines"
  pe_res "$mode.window.control_rc" "$WIN_RC (the timeout-kill status of this image's timeout)"
  pe_control "$mode.window-sees-events" test "$ctl_lines" -ge 3
  # The window ran its full bounded time and ended by the timeout kill, with an
  # empty stderr; any other end (immediate error, early exit) means the
  # instrument failed and its zero-event reading proves nothing.
  win_ok=window-instrument-failed
  if [[ $win_rc == "$WIN_RC" && $WIN_RC != 0 && ! -s $evf.err && ! -s $SCRATCH/ctl-$mode.err ]] \
    && ((win_ms >= (WINDOW_SECONDS - 1) * 1000)); then
    win_ok=ok
  fi
  pe_res "$mode.window.instrument" "$win_ok (rc=$win_rc expected=$WIN_RC elapsed_ms=$win_ms stderr_bytes=$(wc -c <"$evf.err"))"
  pe_check "$mode.window.instrument-ok" test "$win_ok" = ok
}

# ---- barrier predicate ------------------------------------------------------
run_barrier() {
  local fr n running bad_policy=0 list_running=0 desc
  fr=$SCRATCH/fr-barrier
  rm_known
  pe_inner 60 run --name b-exited "$PE_FIXTURE_IMAGE" true >/dev/null
  pe_inner 60 run -d --name b-stopped --restart no "$PE_FIXTURE_IMAGE" sleep 3600 >/dev/null
  pe_inner 60 stop -t 1 b-stopped >/dev/null
  pe_first_response_start "$fr"
  pe_outer 60 restart --time 10 "$PE_DAEMON" >/dev/null
  pe_first_response_wait || pe_fail "first-response poller failed (barrier) rc=$PE_FR_RC bytes=$(wc -c <"$fr"): $(tail -3 "$fr") daemon-log: $(daemon_log_tail)"
  parse_fr "$fr"
  pe_assert_no_public_listener
  pe_res barrier.first_response.info "swarm=$FR_SWARM live_restore=$FR_LIVE plugins=$FR_PLUGINS"
  running=$(count_fr_not_at_rest)
  for n in "${!FR_LIST[@]}"; do
    case ${FR_LIST[$n]} in running | restarting | paused) list_running=$((list_running + 1)) ;; esac
  done
  for n in "${!FR_POLICY[@]}"; do
    [[ ${FR_POLICY[$n]} == no ]] || bad_policy=$((bad_policy + 1))
  done
  desc=''
  for n in "${!FR_STATUS[@]}"; do desc+="$n=${FR_STATUS[$n]}/${FR_POLICY[$n]} "; done
  pe_res barrier.containers "$desc"
  pe_check barrier.no-running-restarting-paused-by-inspect test "$running" -eq 0
  pe_check barrier.no-running-restarting-paused-by-list test "$list_running" -eq 0
  pe_check barrier.every-policy-is-no test "$bad_policy" -eq 0
  pe_check barrier.swarm-inactive test "$FR_SWARM" = inactive
  pe_check barrier.plugin-ls-empty test "$FR_PLUGINS" -eq 0
  pe_check barrier.live-restore-disabled test "$FR_LIVE" = false
  pe_check barrier.has-containers test "${#FR_STATUS[@]}" -eq 2
  # Positive controls for the instrument: the earlier restore cases showed it
  # reporting running containers at the first response and non-`no` policies.
  pe_control barrier.fr-sees-running-containers test "$CTL_RUNNING_AT_FIRST" -ge 1
  pe_control barrier.fr-sees-restart-policies test "$CTL_NONNO_POLICY" -ge 1
}

# try_ready <probes>: non-fatal wait for the daemon (probe cap 5 s, 1 s apart).
try_ready() {
  local i
  for ((i = 0; i < $1; i++)); do
    if pe_client 5 info >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  return 1
}

info_line() { pe_client 20 info --format 'swarm={{.Swarm.LocalNodeState}} live={{.LiveRestoreEnabled}}'; }

# Positive controls for the Swarm, plugin and live-restore readings used by the
# barrier predicate: each state is put into the disposable inner daemon and read
# with the same commands, then removed again.
run_barrier_controls() {
  local cfg out
  case $PE_POSTURE in
    rootless) cfg=/home/rootless/.config/docker/daemon.json ;;
    *) cfg=/etc/docker/daemon.json ;;
  esac

  # Swarm control plane bound to loopback only (the pod network is shared); the
  # data-path address too if this engine accepts it.
  if out=$(pe_client 60 swarm init --advertise-addr 127.0.0.1 --listen-addr 127.0.0.1:2377 --data-path-addr 127.0.0.1 2>&1); then
    pe_res control.swarm-init.flags "listen-addr=127.0.0.1:2377 data-path-addr=127.0.0.1"
  elif out=$(pe_client 60 swarm init --advertise-addr 127.0.0.1 --listen-addr 127.0.0.1:2377 2>&1); then
    pe_res control.swarm-init.flags "listen-addr=127.0.0.1:2377 (data-path-addr refused)"
  else
    out=${out:-init failed}
  fi
  if [[ $(pe_client 20 info --format '{{.Swarm.LocalNodeState}}') == active ]]; then
    pe_assert_no_public_listener
    pe_res control.swarm-active.read "$(info_line)"
    pe_control swarm-state-readable test "$(info_line)" = 'swarm=active live=false'
    pe_client 60 swarm leave --force >/dev/null 2>&1 || true
  else
    pe_res control.swarm-active.init_failure "${out:0:200}"
    pe_incomplete swarm-state-readable "docker swarm init failed on this posture"
  fi
  pe_check barrier-controls.swarm-back-to-inactive test "$(info_line)" = 'swarm=inactive live=false'

  pe_inner_sh 10 "mkdir -p /tmp/plug/rootfs && printf '%s' '{\"description\":\"pe\",\"entrypoint\":[\"/bin/true\"],\"interface\":{\"types\":[\"docker.volumedriver/1.0\"],\"socket\":\"pe.sock\"}}' > /tmp/plug/config.json"
  if out=$(pe_inner 60 plugin create pe-plug:latest /tmp/plug 2>&1); then
    pe_control plugin-ls-readable test "$(pe_client 20 plugin ls -q | wc -l)" -eq 1
    pe_client 60 plugin rm pe-plug:latest >/dev/null 2>&1 || true
  else
    pe_res control.plugin.create_failure "${out:0:200}"
    pe_incomplete plugin-ls-readable "docker plugin create failed on this posture"
  fi
  pe_check barrier-controls.plugin-removed test "$(pe_client 20 plugin ls -q | wc -l)" -eq 0

  pe_inner_sh 10 "mkdir -p \$(dirname $cfg) && printf '%s' '{\"live-restore\":true}' > $cfg"
  pe_outer 60 restart --time 10 "$PE_DAEMON" >/dev/null
  if try_ready 30; then
    pe_assert_no_public_listener
    pe_res control.live-restore.read "$(info_line)"
    pe_control live-restore-readable test "$(info_line)" = 'swarm=inactive live=true'
  else
    pe_incomplete live-restore-readable "daemon did not start with live-restore on this posture"
  fi
  # A new container instance (fresh writable layer) drops the config file
  # whether or not the daemon came up with it, so no separate removal step
  # (which would abort when the daemon container is down).
  pe_daemon_container_recreate
  pe_check barrier-controls.live-restore-off-again test "$(info_line)" = 'swarm=inactive live=false'
}

# ---- restart detection ------------------------------------------------------
sock_inode() {
  if [[ $PE_POSTURE == rootless ]]; then
    pe_worker_sh 5 "stat -c %i $PE_SOCK_DIR/docker.sock" 2>/dev/null || echo missing
  else
    echo 'n/a (loopback tcp)'
  fi
}

# hold_sock / held_sock_state: a hard link to the live socket keeps its inode
# allocated, so a socket file recreated by the next daemon start cannot reuse
# the inode number (a plain before/after comparison can: freed inode numbers are
# reused at once). Rootless only.
hold_sock() {
  if [[ $PE_POSTURE == rootless ]]; then
    pe_worker_sh 5 "ln -f $PE_SOCK_DIR/docker.sock $PE_SOCK_DIR/docker.sock.held"
  fi
}

# held_sock_state: sets HELD_SOCK_STATE ("recreated" if the socket path now has a
# different inode than the held one, "same-inode" otherwise) and returns 1 when
# either inode could not be read as a number ("unmeasured").
HELD_SOCK_STATE=''
held_sock_state() {
  HELD_SOCK_STATE='n/a (loopback tcp)'
  [[ $PE_POSTURE == rootless ]] || return 0
  local held='' cur=''
  held=$(pe_worker_sh 5 "stat -c %i $PE_SOCK_DIR/docker.sock.held") || held=''
  cur=$(pe_worker_sh 5 "stat -c %i $PE_SOCK_DIR/docker.sock") || cur=''
  pe_worker_sh 5 "rm -f $PE_SOCK_DIR/docker.sock.held" || true
  if [[ ! $held =~ ^[0-9]+$ || ! $cur =~ ^[0-9]+$ ]]; then
    HELD_SOCK_STATE="unmeasured (held='$held' now='$cur')"
    return 1
  fi
  if [[ $held != "$cur" ]]; then HELD_SOCK_STATE="recreated (held=$held now=$cur)"; else HELD_SOCK_STATE="same-inode (held=$held now=$cur)"; fi
}

# containerd_pidfile_state: read, from the (stopped or running) daemon container's
# writable layer, the containerd pid file the next start would find. Rootless
# keeps it under XDG_RUNTIME_DIR, a tmpfs here, which `docker cp` cannot read.
containerd_pidfile_state() {
  local path out rc=0
  case $PE_POSTURE in
    rootless) path=/run/dind/docker/containerd/containerd.pid ;;
    *) path=/var/run/docker/containerd/containerd.pid ;;
  esac
  timeout --kill-after=2s 20s docker cp "$PE_DAEMON:$path" - >"$SCRATCH/pidfile.tar" 2>"$SCRATCH/pidfile.err" || rc=$?
  if ((rc == 0)); then
    out=$(tar -xOf "$SCRATCH/pidfile.tar" 2>&1 | head -c 40) || out='(unreadable)'
    echo "path=$path exists=yes content=${out//$'\n'/ }"
  elif grep -qi 'could not find the file' "$SCRATCH/pidfile.err"; then
    echo "path=$path exists=no"
  else
    echo "path=$path exists=unknown cp_rc=$rc err=$(head -c 160 "$SCRATCH/pidfile.err" | tr '\n' ' ')"
  fi
}

# detect_case <control|restart|kill9|sigterm>
detect_case() {
  local mode=$1 ef id_before id_after ino_before ino_after t0 eof_ms rpid worker_up state i recover
  local ctr_before ctr_started dpids dscript delivery gone how running alive sock_rc rkey e pidlist=''
  ef=$SCRATCH/detect-$mode
  rm_known
  id_before=$(pe_client 20 info --format '{{.ID}}')
  ino_before=$(sock_inode)
  if [[ $mode != control ]]; then hold_sock; fi
  pe_res "detect.$mode.engine_id_before" "$id_before"
  pe_res "detect.$mode.socket_inode_before" "$ino_before"

  pe_events_start "$ef"
  # Control: the stream is open and receives events from an idle daemon's first
  # activity, and a client of the same stream form ends only when told to.
  pe_client 60 run --rm --name evctl "$PE_FIXTURE_IMAGE" true >/dev/null
  for ((i = 0; i < 50; i++)); do
    if [[ $(wc -l <"$ef") -ge 3 ]]; then break; fi
    sleep 0.2
  done
  pe_control "detect.$mode.stream-receives-events" test "$(wc -l <"$ef")" -ge 3
  pe_control "detect.$mode.stream-open-before-trigger" kill -0 "$PE_EVENTS_PID"

  if [[ $mode == control ]]; then
    # Hold the idle stream open for 5 s: an instrument that closes by itself
    # (a client or daemon timeout) would otherwise pass as "ends only when told".
    if pe_events_wait_end 5; then i=ended; else i=open; fi
    pe_control detect.control.stream-held-open-5s test "$i" = open
    pe_events_stop || pe_fail 'events client not verified gone'
    pe_res "detect.control.end" "$PE_EVENTS_END rc=$PE_EVENTS_RC"
    pe_check detect.control.stream-ends-only-when-stopped test "$PE_EVENTS_END" = stopped
    return 0
  fi

  # Before a kill: the dockerd pids with their start times, and the daemon
  # container's pid and start time, so the kill can be shown to have landed.
  dpids='' ctr_started='' delivery=''
  if [[ $mode == kill9 ]]; then
    ctr_before=$(pe_outer 10 inspect --type container --format '{{.State.Pid}} {{.State.StartedAt}}' "$PE_DAEMON")
    ctr_started=${ctr_before#* }
    dpids=$(pe_root_sh 10 'for p in $(pidof dockerd); do echo "$p:$(cut -d" " -f22 /proc/$p/stat)"; done' | tr '\n' ' ')
    pe_res "detect.$mode.target_before" "container_pid_startedat=[$ctr_before] dockerd_pid:starttime=[$dpids] container_init_comm=$(pe_root_sh 5 'cat /proc/1/comm') dockerd_is_pid1=$([[ " $dpids" == *' 1:'* ]] && echo yes || echo no)"
    [[ -n ${dpids// /} ]] || pe_fail 'no dockerd pid found before kill'
  fi

  t0=${EPOCHREALTIME/[.,]/}
  rpid=''
  case $mode in
    restart)
      pe_outer 70 restart --time 10 "$PE_DAEMON" >/dev/null &
      rpid=$!
      ;;
    kill9)
      # Rootful: deliver from the outer daemon, so the kill does not depend on
      # dockerd's place in the pid namespace (an in-namespace SIGKILL to PID 1 is
      # dropped). Rootless: dockerd is a child of rootlesskit, so an in-container
      # kill reaches it. Either way the effect is verified below.
      if [[ $PE_POSTURE == rootful ]]; then
        delivery=outer-docker-kill
        pe_outer 20 kill --signal KILL "$PE_DAEMON" >/dev/null || delivery='outer-docker-kill (command failed)'
      else
        delivery=in-container-kill-9
        for e in $dpids; do pidlist+="${e%%:*} "; done
        pe_root_sh 10 "kill -9 $pidlist" || delivery='in-container-kill-9 (command failed)'
      fi
      ;;
    sigterm) pe_root_sh 10 'kill -TERM $(pidof dockerd)' ;;
  esac
  if pe_events_wait_end 70; then
    eof_ms=$(((PE_EVENTS_EOF_US - t0) / 1000))
    pe_res "detect.$mode.supp.time_to_eof_ms" "$eof_ms"
  else
    eof_ms=none
    pe_res "detect.$mode.supp.time_to_eof_ms" "none within 70 s"
  fi
  if [[ -n $rpid ]]; then wait "$rpid" || pe_fail 'docker restart failed'; fi

  if [[ $mode == kill9 ]]; then
    # The kill must have removed the target: the container instance ended or was
    # replaced, or every recorded dockerd pid is gone / has a different start time.
    dscript='for e in '"$dpids"'; do p=${e%%:*}; s=${e#*:}; cur=$(cut -d" " -f22 /proc/$p/stat 2>/dev/null) || cur=""; [ "$cur" != "$s" ] || { echo alive; exit 0; }; done; echo gone'
    gone=no how=''
    for ((i = 0; i < 40; i++)); do
      running=$(timeout --kill-after=2s 10s docker inspect --type container --format '{{.State.Running}} {{.State.StartedAt}}' "$PE_DAEMON") || {
        sleep 0.25
        continue
      }
      if [[ ${running%% *} == false ]]; then
        gone=yes how=daemon-container-exited
        break
      fi
      if [[ ${running#* } != "$ctr_started" ]]; then
        gone=yes how=daemon-container-replaced
        break
      fi
      alive=$(pe_root_sh 5 "$dscript") || alive=exec-failed
      if [[ $alive == gone ]]; then
        gone=yes how=dockerd-pids-gone
        break
      fi
      sleep 0.25
    done
    pe_res "detect.$mode.delivery" "$delivery"
    pe_res "detect.$mode.kill_effect" "target_gone=$gone how=${how:-none}"
    if [[ $gone != yes ]]; then pe_res "detect.$mode.failure" kill-not-delivered; fi
    pe_check "detect.$mode.kill-delivered" test "$gone" = yes
  fi
  pe_events_stop || pe_fail 'events client not verified gone'
  pe_res "detect.$mode.stream_end" "$PE_EVENTS_END rc=$PE_EVENTS_RC"
  pe_check "detect.$mode.stream-ended-by-daemon" test "$PE_EVENTS_END" = ended -a "$eof_ms" != none
  worker_up=$(timeout --kill-after=2s 10s docker inspect --type container --format '{{.State.Running}}' "$PE_WORKER")
  pe_check "detect.$mode.client-container-survived" test "$worker_up" = true

  # What happened to the daemon container (its init exits with dockerd).
  for ((i = 0; i < 40; i++)); do
    state=$(timeout --kill-after=2s 10s docker inspect --type container --format '{{.State.Status}} exit={{.State.ExitCode}}' "$PE_DAEMON")
    [[ $mode == restart || $state != running* ]] && break
    sleep 0.5
  done
  pe_res "detect.$mode.daemon_container_after" "$state"

  # Recovery: first a plain container restart (same writable layer); if that
  # cannot bring the daemon up, a new container instance (what kubelet does).
  if [[ $mode != restart ]]; then
    pe_res "detect.$mode.containerd_pidfile_before_restart" "$(containerd_pidfile_state)"
    pe_outer 60 restart --time 5 "$PE_DAEMON" >/dev/null || true
    if try_ready 30; then
      recover=same-container-restart-ok
    else
      recover=same-container-restart-FAILED
      pe_res "detect.$mode.restart_failure_log" "$(timeout --kill-after=2s 10s docker logs --tail 5 "$PE_DAEMON" 2>&1 | cut -c1-200)"
      pe_daemon_container_recreate
    fi
    rkey=recovery
    if [[ $PE_POSTURE == rootless ]]; then
      # Rootless XDG_RUNTIME_DIR (pid files, rootlesskit state) is a per-start
      # tmpfs in this harness; in production it is a pod-lifetime emptyDir, so a
      # same-container restart there keeps those files.
      rkey=recovery.harness_tmpfs_runtime_dir
    fi
    pe_res "detect.$mode.$rkey" "$recover"
  fi
  pe_wait_ready
  pe_assert_no_public_listener
  id_after=$(pe_client 20 info --format '{{.ID}}')
  ino_after=$(sock_inode)
  pe_res "detect.$mode.engine_id_after" "$id_after"
  pe_res "detect.$mode.socket_inode_after" "$ino_after"
  pe_check "detect.$mode.engine-id-identical" test "$id_before" = "$id_after"
  if [[ $PE_POSTURE == rootless ]]; then
    pe_res "detect.$mode.socket_inode_changed_plain" "$([[ $ino_before != "$ino_after" ]] && echo yes || echo no)"
  else
    pe_res "detect.$mode.socket_inode_changed_plain" 'n/a (loopback tcp)'
  fi
  sock_rc=0
  held_sock_state || sock_rc=1
  pe_res "detect.$mode.socket_file_vs_held_inode" "$HELD_SOCK_STATE"
  pe_check "detect.$mode.socket-inodes-measured" test "$sock_rc" -eq 0
}

run_posture() {
  local posture=$1 img
  pe_init "$posture"
  pe_exp_begin restore
  img=$PE_FIXTURE_IMAGE
  mkdir -p "$here/../../.uzi/scratch"
  SCRATCH=$(mktemp -d "$here/../../.uzi/scratch/pe-restore.XXXXXX")
  pe_volumes_create
  pe_daemon_create_start
  pe_res engine "$(pe_inner 10 version --format '{{.Server.Version}}')"
  if ! pe_inner 10 image inspect "$img" >/dev/null 2>&1; then
    pe_inner 240 pull "$img" >/dev/null
  fi

  run_case restart
  run_case replace
  run_barrier
  run_barrier_controls
  detect_case control
  detect_case restart
  detect_case kill9
  detect_case sigterm

  rm -rf "$SCRATCH"
  pe_exp_finish
}

pe_exp_main restore run_posture "${1:-}"
