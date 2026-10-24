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

# hold_cwd <dir> <label>: a root process in the daemon container whose cwd is
# <dir>. It keeps the directory's inode allocated after rm -rf, so a directory
# recreated later cannot reuse the inode number, and it is itself a positive
# control for the scanner on a deleted directory. Verified by label.
hold_cwd() {
  pe_outer 10 exec -d --user 0:0 "$PE_DAEMON" env -i "PATH=$PE_PATH" \
    sh -c "cd '$1' && exec sleep 3600" >/dev/null
  pe_root_sh 10 "
    for d in /proc/[0-9]*; do
      c=\$(tr '\\0' ' ' 2>/dev/null < \$d/cmdline) || continue
      [ \"\$c\" = 'sleep 3600 ' ] || continue
      [ \"\$(stat -L -c %i \$d/cwd 2>/dev/null)\" = '$3' ] && { echo held; exit 0; }
    done
    exit 1" >/dev/null || pe_fail "cwd holder for $2 did not start"
}

# attempt <label> <cmd...>: run, record rc and the first 200 chars of output.
attempt() {
  local label=$1 out rc=0
  shift
  out=$("$@" 2>&1) || rc=$?
  out=${out//$'\n'/ }
  pe_res "$label.rc" "$rc"
  pe_res "$label.out" "${out:0:200}"
}

scan_summary() { # scan_summary <label> <scan-output>: prints counts, sets SCAN_REFS
  local label=$1 out=$2
  SCAN_REFS=$(pe_scan_refs "$out")
  pe_res "$label.refs" "$SCAN_REFS"
  pe_res "$label.mnt_strict" "$(grep -c '^MNT-STRICT ' <<<"$out" || true)"
  pe_res "$label.mnt_loose" "$(grep -c '^MNT-LOOSE ' <<<"$out" || true)"
  pe_res "$label.fd" "$(grep -c '^FD ' <<<"$out" || true)"
  pe_res "$label.map" "$(grep -c '^MAP ' <<<"$out" || true)"
  pe_res "$label.scanned" "$(grep '^SCANNED ' <<<"$out" | tail -1)"
}

run_posture() {
  local posture=$1 img T TM state ino0 ino1 ino2 ids out tp scan nsn
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
  scan=$(pe_tree_scan "$T" "$ids")
  scan_summary control.running "$scan"
  nsn=$(grep '^SCANNED ' <<<"$scan" | sed 's/.*mountns=//')
  pe_res control.task_mountinfo "$(pe_root_sh 10 "grep ' /w ' /proc/$tp/mountinfo" || true)"
  pe_res control.task_cwd "$(pe_root_sh 10 "readlink /proc/$tp/cwd; stat -L -c %d:%i /proc/$tp/cwd" | tr '\n' ' ')"
  if ! grep -q '^MNT-STRICT ' <<<"$scan" || ! grep -q "^FD pid=$tp " <<<"$scan" || ((nsn < 2)); then
    pe_incomplete tree-scan "positive control failed: scanner did not see the running bind (strict mount hit, task cwd hit, >=2 mount namespaces)"
    pe_exp_finish
  fi
  pe_res control.running PASS

  # ---- stop ---------------------------------------------------------------
  pe_inner 60 stop -t 2 c-bind >/dev/null
  pe_res c-bind.mounts_after_stop "$(pe_inner 10 inspect -f '{{json .Mounts}}' c-bind)"
  scan=$(pe_tree_scan "$T" "$ids")
  scan_summary after-stop "$scan"
  pe_check after-stop.no-reference test "$SCAN_REFS" -eq 0

  pe_daemon_restart
  scan=$(pe_tree_scan "$T" "$ids")
  scan_summary after-restart "$scan"
  pe_check after-restart.no-reference test "$SCAN_REFS" -eq 0

  # ---- delete tree-B, then docker cp into the stopped container -----------
  hold_cwd "$T" tree-B "$ino0"
  pe_inner_sh 20 "rm -rf '$T'"
  pe_res tree-B.after_rm "$(tree_state "$T")"
  scan=$(pe_tree_scan "$T" "$ids")
  # Positive control for deleted inodes: the holder's cwd is the deleted tree.
  pe_res control.deleted_inode_fd "$(grep -c '^FD ' <<<"$scan" || true)"
  pe_check control.deleted-inode-visible test "$(grep -c '^FD ' <<<"$scan" || true)" -ge 1

  pe_inner_sh 10 "echo copied > /tmp/pe-cpfile"
  attempt cp-into-stopped pe_inner 30 cp /tmp/pe-cpfile c-bind:/w/
  state=$(tree_state "$T")
  pe_res tree-B.after_cp "$state"
  ino1=$(state_field "$state" ino)
  if [[ $state == exists=yes* ]]; then
    pe_res tree-B.after_cp.recreated yes
    pe_res tree-B.after_cp.inode_differs "$([[ $ino1 != "$ino0" ]] && echo yes || echo no)"
    pe_res tree-B.after_cp.file "$(pe_inner_sh 10 "cat '$T/pe-cpfile' 2>&1" || true)"
    pe_check after-cp.new-inode test "$ino1" != "$ino0"
    hold_cwd "$T" tree-B-cp "$ino1"
  else
    pe_res tree-B.after_cp.recreated no
  fi

  # ---- delete again; docker start of an idle -v container and of c-bind ---
  # c-bind-idle is created while the tree exists (mirroring c-bind), with a
  # command that writes nothing, so emptiness of the recreated tree is clean.
  pe_inner_sh 20 "mkdir -p '$T'"
  pe_inner 60 create --name c-bind-idle -v "$T:/w" "$img" sleep 600 >/dev/null
  pe_inner_sh 20 "rm -rf '$T'"
  pe_res tree-B.before_idle_start "$(tree_state "$T")"
  attempt start-idle pe_inner 60 start c-bind-idle
  state=$(tree_state "$T")
  pe_res tree-B.after_idle_start "$state"
  ino2=$(state_field "$state" ino)
  if [[ $state == exists=yes* ]]; then
    pe_res tree-B.after_idle_start.recreated yes
    pe_res tree-B.after_idle_start.empty "$([[ ${state##*list=} == '' ]] && echo yes || echo no)"
    pe_check after-idle-start.new-inode-vs-initial test "$ino2" != "$ino0"
    pe_check after-idle-start.new-inode-vs-cp test "$ino2" != "$ino1"
  else
    pe_res tree-B.after_idle_start.recreated no
  fi
  pe_inner 60 stop -t 2 c-bind-idle >/dev/null

  pe_inner_sh 20 "rm -rf '$T'"
  attempt start-cbind pe_inner 60 start c-bind
  state=$(tree_state "$T")
  pe_res tree-B.after_cbind_start "$state"
  pe_res tree-B.after_cbind_start.note "c-bind's own command writes f into /w, so a non-empty listing is expected"
  pe_inner 60 stop -t 2 c-bind >/dev/null

  # ---- --mount type=bind and --volumes-from -------------------------------
  TM=$PE_RUN_DIR/tree-M
  pe_inner_sh 20 "mkdir -p '$TM'"
  pe_inner 60 run -d --name c-mount --mount "type=bind,src=$TM,dst=/w" "$img" sleep 600 >/dev/null
  pe_inner 60 create --name c-vf --volumes-from c-mount "$img" sleep 600 >/dev/null
  pe_inner 60 stop -t 2 c-mount >/dev/null
  pe_inner_sh 20 "rm -rf '$TM'"
  attempt start-c-mount pe_inner 60 start c-mount
  pe_res tree-M.after_start_c_mount "$(tree_state "$TM")"
  pe_inner_sh 20 "rm -rf '$TM'"
  attempt cp-into-c-mount pe_inner 30 cp /tmp/pe-cpfile c-mount:/w/
  pe_res tree-M.after_cp_c_mount "$(tree_state "$TM")"
  pe_inner_sh 20 "rm -rf '$TM'"
  attempt start-c-vf pe_inner 60 start c-vf
  pe_res tree-M.after_start_c_vf "$(tree_state "$TM")"

  pe_exp_finish
}

pe_exp_main stopped run_posture "${1:-}"
