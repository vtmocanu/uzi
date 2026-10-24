#!/usr/bin/env bash
# PRD #1860 M0, Q3 "stopped state": can a stopped container hold a mount or a
# file reference into a tree, and what does a later `docker start` or
# `docker cp` do when the tree has been deleted?
# Usage: exp-stopped.sh <rootless|rootful|all>
# Output: RESULT stopped <posture> <key>=<value> ... then EXP stopped <posture> PASS|FAIL|INCOMPLETE.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$here/lib.sh"
: "${PE_DEADLINE_SECONDS:=1500}"

# tree_state <path>: exists/inode/owner/mode/listing as the daemon's uid.
tree_state() {
  pe_inner_sh 10 "if [ -e '$1' ]; then echo \"exists=yes ino=\$(stat -c %i '$1') own=\$(stat -c %u:%g '$1') mode=\$(stat -c %a '$1') list=\$(ls -A '$1' | tr '\\n' ',')\"; else echo exists=no; fi"
}

state_field() { # state_field <state> <key>
  local rest=${1#*"$2"=}
  echo "${rest%% *}"
}

# hold_cwd <dir> <label> <inode>: a root process in the daemon container whose
# cwd is <dir>. It keeps the directory's inode allocated after rm -rf, so a
# directory recreated later cannot reuse the inode number, it is itself a
# positive control for the scanner on a deleted directory, and its
# /proc/<pid>/cwd is the handle through which the deleted directory is listed.
# Sets HOLD_PID. Verified by label.
HOLD_PID=''
hold_cwd() {
  pe_outer 10 exec -d --user 0:0 "$PE_DAEMON" env -i "PATH=$PE_PATH" \
    sh -c "cd '$1' && exec sleep 3600" >/dev/null
  HOLD_PID=$(pe_root_sh 10 "
    for d in /proc/[0-9]*; do
      c=\$(tr '\\0' ' ' 2>/dev/null < \$d/cmdline) || continue
      [ \"\$c\" = 'sleep 3600 ' ] || continue
      [ \"\$(stat -L -c %i \$d/cwd 2>/dev/null)\" = '$3' ] && { echo \${d#/proc/}; exit 0; }
    done
    exit 1") || pe_fail "cwd holder for $2 did not start"
  [[ $HOLD_PID =~ ^[0-9]+$ ]] || pe_fail "cwd holder for $2: bad pid '$HOLD_PID'"
}

# held_listing <pid>: the entries of the (deleted) directory the holder's cwd
# refers to, listed through its /proc/<pid>/cwd as root.
held_listing() {
  pe_root_sh 10 "ls -A /proc/$1/cwd | tr '\\n' ','"
}

# attempt <label> <cmd...>: run, record rc and the first 200 chars of output.
# Sets ATTEMPT_RC.
ATTEMPT_RC=0
attempt() {
  local label=$1 out rc=0
  shift
  out=$("$@" 2>&1) || rc=$?
  out=${out//$'\n'/ }
  ATTEMPT_RC=$rc
  pe_res "$label.rc" "$rc"
  pe_res "$label.out" "${out:0:200}"
}

# reresolved_ok <rc> <state> <held-inode...>: the allowed outcomes of a request
# that re-resolves a deleted bind source: the request failed, or the tree was
# recreated on an inode different from every held deleted inode.
reresolved_ok() {
  local rc=$1 state=$2 ino held
  shift 2
  ((rc != 0)) && return 0
  [[ $state == exists=yes* ]] || return 1
  ino=$(state_field "$state" ino)
  for held in "$@"; do
    [[ $ino != "$held" ]] || return 1
  done
  return 0
}

# container_binds_ok <rc> <container> <tree-state> <held-inode...>: the allowed
# outcomes of a start whose bind source was deleted: the start failed, or the
# running container's /w is the tree's recreated inode, which is none of the held
# deleted inodes. Sets BIND_INO to the inode seen inside the container.
BIND_INO=''
container_binds_ok() {
  local rc=$1 c=$2 state=$3 tino held
  shift 3
  BIND_INO=''
  ((rc != 0)) && return 0
  BIND_INO=$(pe_inner 10 exec "$c" stat -c %i /w) || return 1
  [[ $state == exists=yes* ]] || return 1
  tino=$(state_field "$state" ino)
  [[ $BIND_INO == "$tino" ]] || return 1
  for held in "$@"; do
    [[ $BIND_INO != "$held" ]] || return 1
  done
  return 0
}

# listing_is <pid> <expected>: the held deleted directory still lists exactly
# <expected> (the listing taken right after it was deleted), and not the cp'd file.
listing_is() {
  local now
  now=$(held_listing "$1") || return 1
  [[ $now == "$2" && $now != *pe-cpfile* ]]
}

# assert_deleted <label> <path>: record the tree state and require that it is gone.
assert_deleted() {
  local state
  state=$(tree_state "$2")
  pe_res "$1" "$state"
  pe_check "$1.exists-no" test "$(state_field "$state" exists)" = no
}

# ensure_stopped <label> <container>: record the state, force-stop if running.
ensure_stopped() {
  local st
  st=$(pe_inner 10 inspect -f '{{.State.Status}}' "$2")
  pe_res "$1.state_before" "$st"
  if [[ $st == running ]]; then
    pe_inner 60 stop -t 2 "$2" >/dev/null
    pe_res "$1.forced_stop" yes
  fi
  pe_check "$1.stopped" test "$(pe_inner 10 inspect -f '{{.State.Status}}' "$2")" != running
}

run_posture() {
  local posture=$1 img T TM state ino0 ino1 ino2 ids tp scan nsn tns
  local holdA='' holdB='' holdC='' holdM='' baseA='' baseB='' held_inos pre ino2='' tmino0
  pe_init "$posture"
  pe_exp_begin stopped
  img=$PE_FIXTURE_IMAGE
  pe_volumes_create
  pe_daemon_create_start
  pe_res engine "$(pe_inner 10 version --format '{{.Server.Version}}')"
  if ! pe_inner 10 image inspect "$img" >/dev/null 2>&1; then
    pe_inner 240 pull "$img" >/dev/null
  fi

  # ---- tree-B: a -v bind container -------------------------------------
  T=$PE_RUN_DIR/tree-B
  pe_inner_sh 20 "mkdir -p '$T' && echo seed > '$T/seed'"
  pe_inner 60 run -d --name c-bind -v "$T:/w" "$img" \
    sh -c 'cd /w; echo x > f; exec sleep 600' >/dev/null
  pe_inner_sh 15 "i=0; while [ ! -e '$T/f' ] && [ \$i -lt 100 ]; do sleep 0.1; i=\$((i + 1)); done; test -e '$T/f'" \
    || pe_fail 'c-bind did not write f'
  ino0=$(pe_inner_sh 10 "stat -c %i '$T'")
  pe_res tree-B.inode.initial "$ino0"
  ids=$(pe_tree_ids "$T")
  pe_res tree-B.ids "$(tr '\n' ' ' <<<"$ids")"

  # Positive control: while c-bind runs the scanner must see the bind in the
  # task's mountinfo and a process (the task) with cwd in the tree.
  tp=$(pe_inner 10 inspect -f '{{.State.Pid}}' c-bind)
  pe_res control.task_pid "$tp"
  PE_DOCKERD_NS=$(pe_dockerd_mntns)
  tns=$(pe_root_sh 10 "readlink /proc/$tp/ns/mnt")
  pe_res control.namespaces "dockerd=$PE_DOCKERD_NS task=$tns"
  scan=$(pe_tree_scan "$T" "$ids")
  pe_scan_summary control.running "$scan"
  nsn=$(grep '^SCANNED ' <<<"$scan" | sed -n 's/.*mountns=\([0-9]*\).*/\1/p')
  pe_scan_control "$scan"
  pe_res control.strict_hit_namespaces "$(grep '^MNT-STRICT ' <<<"$scan" | awk '{print $2}' | sort | uniq -c | tr '\n' ';')"
  pe_res control.task_mountinfo "$(pe_root_sh 10 "grep ' /w ' /proc/$tp/mountinfo" || true)"
  pe_res control.task_cwd "$(pe_root_sh 10 "readlink /proc/$tp/cwd; stat -L -c %d:%i /proc/$tp/cwd" | tr '\n' ' ')"
  # The scanner must have read the task's and dockerd's namespaces, and the
  # strict mount hit must be in the task's own namespace.
  if [[ $tns != 'mnt:['* ]] || ! grep -qF "MNT-STRICT ns=$tns " <<<"$scan" \
    || ! grep -qF "NS-READ ns=$tns " <<<"$scan" || ! grep -qF "NS-READ ns=$PE_DOCKERD_NS " <<<"$scan" \
    || ! grep -q "^FD pid=$tp " <<<"$scan" || ((nsn < 2)); then
    pe_incomplete tree-scan "positive control failed: scanner did not see the running bind (strict mount hit in the task's mount namespace, task cwd hit, task and dockerd namespaces read, >=2 mount namespaces)"
    pe_exp_finish
  fi
  pe_res control.running PASS
  # Positive control that the scanner reports what it cannot read.
  pe_control scan-unreadable pe_unreadable_control "$T" "$ids"

  # ---- stop ---------------------------------------------------------------
  pe_inner 60 stop -t 2 c-bind >/dev/null
  pe_res c-bind.mounts_after_stop "$(pe_inner 10 inspect -f '{{json .Mounts}}' c-bind)"
  scan=$(pe_tree_scan "$T" "$ids")
  pe_scan_summary after-stop "$scan"
  pe_check_clear after-stop.no-reference after-stop "$scan"

  pe_daemon_restart
  PE_DOCKERD_NS=$(pe_dockerd_mntns)
  scan=$(pe_tree_scan "$T" "$ids")
  pe_scan_summary after-restart "$scan"
  pe_check_clear after-restart.no-reference after-restart "$scan"

  # ---- delete tree-B, then docker cp into the stopped container -----------
  # The first holder keeps the original directory (ino0) allocated. Its
  # listing right after rm -rf is the baseline: nothing may be written into the
  # deleted directory afterwards.
  hold_cwd "$T" tree-B "$ino0"
  holdA=$HOLD_PID
  pre=$(tree_state "$T")
  pe_res tree-B.listing_before_rm "$pre"
  pe_inner_sh 20 "rm -rf '$T'"
  assert_deleted tree-B.after_rm "$T"
  baseA=$(held_listing "$holdA") || pe_fail "cannot list the deleted tree-B through holder $holdA"
  pe_res tree-B.heldA.listing_after_rm "[$baseA]"
  scan=$(pe_tree_scan "$T" "$ids")
  pe_scan_summary after-rm "$scan"
  pe_scan_complete after-rm "$scan" || true
  # Positive control for deleted inodes: the holder's cwd is the deleted tree.
  pe_res control.deleted_inode_fd "$(grep -c '^FD ' <<<"$scan" || true)"
  pe_check control.deleted-inode-visible test "$(grep -c '^FD ' <<<"$scan" || true)" -ge 1

  pe_inner_sh 10 "echo copied > /tmp/pe-cpfile"
  attempt cp-into-stopped pe_inner 30 cp /tmp/pe-cpfile c-bind:/w/
  state=$(tree_state "$T")
  pe_res tree-B.after_cp "$state"
  held_inos=$ino0
  ino1=''
  if [[ $state == exists=yes* ]]; then
    ino1=$(state_field "$state" ino)
    pe_res tree-B.after_cp.recreated yes
    pe_res tree-B.after_cp.inode_differs "$([[ $ino1 != "$ino0" ]] && echo yes || echo no)"
    pe_res tree-B.after_cp.file "$(pe_inner_sh 10 "cat '$T/pe-cpfile' 2>&1" || true)"
    hold_cwd "$T" tree-B-cp "$ino1"
    holdB=$HOLD_PID
    held_inos="$ino0 $ino1"
  else
    pe_res tree-B.after_cp.recreated no
  fi
  # Allowed outcomes: the cp failed, or the tree was recreated on an inode that
  # is none of the held deleted ones.
  pe_check cp-into-stopped.reresolution reresolved_ok "$ATTEMPT_RC" "$state" "$ino0"

  # ---- delete again; docker start of an idle -v container and of c-bind ---
  # c-bind-idle is created while the tree exists (mirroring c-bind), with a
  # command that writes nothing, so emptiness of the recreated tree is clean.
  pe_inner_sh 20 "mkdir -p '$T'"
  pe_inner 60 create --name c-bind-idle -v "$T:/w" "$img" sleep 600 >/dev/null
  pe_inner_sh 20 "rm -rf '$T'"
  assert_deleted tree-B.before_idle_start "$T"
  if [[ -n $holdB ]]; then
    baseB=$(held_listing "$holdB") || pe_fail "cannot list the deleted cp-recreated tree through holder $holdB"
    pe_res tree-B.heldB.listing_after_rm "[$baseB]"
  fi
  attempt start-idle pe_inner 60 start c-bind-idle
  state=$(tree_state "$T")
  # shellcheck disable=SC2086  # held_inos is a space-separated inode list
  pe_check start-idle.container-binds-recreated-inode container_binds_ok "$ATTEMPT_RC" c-bind-idle "$state" $held_inos
  pe_res start-idle.container_w_inode "${BIND_INO:-none}"
  pe_res tree-B.after_idle_start "$state"
  if [[ $state == exists=yes* ]]; then
    ino2=$(state_field "$state" ino)
    pe_res tree-B.after_idle_start.recreated yes
    pe_res tree-B.after_idle_start.empty "$([[ ${state##*list=} == '' ]] && echo yes || echo no)"
    pe_res tree-B.after_idle_start.inode_differs_from_initial "$([[ $ino2 != "$ino0" ]] && echo yes || echo no)"
    pe_res tree-B.after_idle_start.inode_differs_from_cp "$([[ $ino2 != "$ino1" ]] && echo yes || echo no)"
    # keep ino2 allocated so a later recreation cannot reuse its number
    hold_cwd "$T" tree-B-idle "$ino2"
    holdC=$HOLD_PID
  else
    pe_res tree-B.after_idle_start.recreated no
  fi
  # shellcheck disable=SC2086  # held_inos is a space-separated inode list
  pe_check start-idle.reresolution reresolved_ok "$ATTEMPT_RC" "$state" $held_inos
  # A recreated tree for an idle container must be empty.
  pe_check start-idle.recreated-tree-empty test "$ATTEMPT_RC" -ne 0 -o "${state##*list=}" = ''
  pe_inner 60 stop -t 2 c-bind-idle >/dev/null
  [[ -z $ino2 ]] || held_inos="$held_inos $ino2"

  pe_inner_sh 20 "rm -rf '$T'"
  assert_deleted tree-B.before_cbind_start "$T"
  attempt start-cbind pe_inner 60 start c-bind
  state=$(tree_state "$T")
  pe_res tree-B.after_cbind_start "$state"
  pe_res tree-B.after_cbind_start.note "c-bind's own command writes f into /w, so a non-empty listing is expected"
  # shellcheck disable=SC2086  # held_inos is a space-separated inode list
  pe_check start-cbind.reresolution reresolved_ok "$ATTEMPT_RC" "$state" $held_inos
  # shellcheck disable=SC2086
  pe_check start-cbind.container-binds-recreated-inode container_binds_ok "$ATTEMPT_RC" c-bind "$state" $held_inos
  pe_res start-cbind.container_w_inode "${BIND_INO:-none}"
  pe_inner 60 stop -t 2 c-bind >/dev/null

  # Observations only (not checks): the kernel refuses to create entries in an
  # unlinked directory, so an unchanged listing of a held deleted tree cannot
  # show that a request avoided writing into it.
  pe_res obs.heldA.listing_final "[$(held_listing "$holdA" || echo error)]"
  pe_res obs.heldA.listing_unchanged "$(listing_is "$holdA" "$baseA" && echo yes || echo no)"
  if [[ -n $holdB ]]; then
    pe_res obs.heldB.listing_final "[$(held_listing "$holdB" || echo error)]"
    pe_res obs.heldB.listing_unchanged "$(listing_is "$holdB" "$baseB" && echo yes || echo no)"
  fi
  [[ -z $holdC ]] || pe_res obs.heldC.listing_final "[$(held_listing "$holdC" || echo error)]"

  # ---- --mount type=bind and --volumes-from -------------------------------
  TM=$PE_RUN_DIR/tree-M
  pe_inner_sh 20 "mkdir -p '$TM'"
  tmino0=$(pe_inner_sh 10 "stat -c %i '$TM'")
  pe_res tree-M.inode.initial "$tmino0"
  hold_cwd "$TM" tree-M "$tmino0"
  holdM=$HOLD_PID
  pe_res tree-M.holder_pid "$holdM"
  pe_inner 60 run -d --name c-mount --mount "type=bind,src=$TM,dst=/w" "$img" sleep 600 >/dev/null
  pe_inner 60 create --name c-vf --volumes-from c-mount "$img" sleep 600 >/dev/null
  ensure_stopped c-mount.before-delete c-mount
  pe_inner_sh 20 "rm -rf '$TM'"
  assert_deleted tree-M.before_start_c_mount "$TM"
  attempt start-c-mount pe_inner 60 start c-mount
  pe_res tree-M.after_start_c_mount "$(tree_state "$TM")"
  pe_check start-c-mount.fails test "$ATTEMPT_RC" -ne 0
  # c-mount is running again after its start: stop it so the cp and the
  # volumes-from start act on a stopped container with a deleted source.
  ensure_stopped c-mount.before-cp c-mount
  pe_inner_sh 20 "rm -rf '$TM'"
  assert_deleted tree-M.before_cp_c_mount "$TM"
  attempt cp-into-c-mount pe_inner 30 cp /tmp/pe-cpfile c-mount:/w/
  pe_res tree-M.after_cp_c_mount "$(tree_state "$TM")"
  pe_check cp-into-c-mount.fails test "$ATTEMPT_RC" -ne 0
  ensure_stopped c-mount.before-c-vf c-mount
  pe_inner_sh 20 "rm -rf '$TM'"
  assert_deleted tree-M.before_start_c_vf "$TM"
  attempt start-c-vf pe_inner 60 start c-vf
  state=$(tree_state "$TM")
  pe_res tree-M.after_start_c_vf "$state"
  pe_check start-c-vf.reresolution reresolved_ok "$ATTEMPT_RC" "$state" "$tmino0"
  pe_check start-c-vf.container-binds-recreated-inode container_binds_ok "$ATTEMPT_RC" c-vf "$state" "$tmino0"
  pe_res start-c-vf.container_w_inode "${BIND_INO:-none}"

  pe_exp_finish
}

pe_exp_main stopped run_posture "${1:-}"
