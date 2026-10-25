#!/usr/bin/env bash
# PRD #1860 M0, Q3 "volumes and build cache": can a volume or a BuildKit cache
# entry keep a reference into a tree?
#   1. a plain `local` volume has no Options; a bind-option `local` volume does
#      (and bind-mounts the tree into dockerd's mount namespace while in use);
#   2. a build whose context is the tree leaves no mount, fd or mapping reference to it
#      and a build cache that survives the tree's deletion and a daemon restart.
# Usage: exp-volumes-build.sh <rootless|rootful|all>
# Output: RESULT volumes-build <posture> <key>=<value> ... then EXP volumes-build <posture> PASS|FAIL|INCOMPLETE.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$here/lib.sh"
: "${PE_DEADLINE_SECONDS:=1500}"

declare -A HITS

# strict_in_ns <scan-output> <ns>: number of MNT-STRICT hits inside that namespace.
strict_in_ns() {
  grep -cF "MNT-STRICT ns=$2 " <<<"$1" || true
}

# cache_records: number of build-cache records; cache_types: the record types.
cache_dump() { pe_inner 60 buildx du --verbose 2>&1; }

run_posture() {
  local posture=$1 img TV TK ids scan dns out vopt n_before n_after types_before types_after
  local bpid i samples during unr rc blog TMAP mids gone
  pe_init "$posture"
  pe_exp_begin volumes-build
  img=$PE_FIXTURE_IMAGE
  pe_volumes_create
  pe_daemon_create_start
  pe_res engine "$(pe_inner 10 version --format '{{.Server.Version}}')"
  if ! pe_inner 10 image inspect "$img" >/dev/null 2>&1; then
    pe_inner 240 pull "$img" >/dev/null
  fi

  # ---- volumes ------------------------------------------------------------
  TV=$PE_RUN_DIR/tree-V
  pe_inner_sh 20 "mkdir -p '$TV' && echo v > '$TV/file'"
  ids=$(pe_tree_ids "$TV")
  dns=$(pe_dockerd_mntns)
  PE_DOCKERD_NS=$dns
  pe_res dockerd.mntns "$dns"

  pe_inner 20 volume create v-plain >/dev/null
  pe_inner 60 run -d --name c-plain -v v-plain:/v -v "$TV:/w" "$img" sleep 600 >/dev/null
  vopt=$(pe_inner 10 volume inspect -f '{{.Driver}} {{json .Options}}' v-plain)
  pe_res v-plain.driver_options "$vopt"
  pe_check v-plain.local-and-no-options test "$vopt" = 'local null'
  pe_inner 60 stop -t 2 c-plain >/dev/null

  pe_inner 20 volume create -o type=none -o o=bind -o "device=$TV" v-bind >/dev/null
  vopt=$(pe_inner 10 volume inspect -f '{{.Driver}} {{json .Options}}' v-bind)
  pe_res v-bind.driver_options "$vopt"
  # The contrast with v-plain is the control for the Options reading.
  pe_check v-bind.options-non-empty test "$vopt" != 'local null' -a "$vopt" != 'local {}'
  if ! out=$(pe_inner 60 run -d --name c-vb -v v-bind:/v "$img" sleep 600 2>&1); then
    pe_res v-bind.run_failure "${out:0:300}"
    pe_incomplete tree-scan "could not run a container on the bind-option volume"
    pe_exp_finish
  fi
  scan=$(pe_tree_scan "$TV" "$ids")
  pe_scan_summary v-bind.running "$scan"
  pe_res v-bind.running.hit_namespaces "$(grep -E '^MNT-' <<<"$scan" | awk '{print $1, $2}' | sort | uniq -c | tr '\n' ';')"
  n_after=$(strict_in_ns "$scan" "$dns")
  pe_res v-bind.running.strict_in_dockerd_ns "$n_after"
  # Positive control: the bind of the tree is visible in dockerd's namespace.
  if ((n_after < 1)); then
    pe_incomplete tree-scan "positive control failed: no strict hit in dockerd's mount namespace while the bind-option volume is in use"
    pe_exp_finish
  fi
  pe_res control.volume-visible PASS
  pe_scan_control "$scan"
  # Positive control that the scanner reports what it cannot read.
  pe_control scan-unreadable pe_unreadable_control "$TV" "$ids"

  pe_inner 60 stop -t 2 c-vb >/dev/null
  scan=$(pe_tree_scan "$TV" "$ids")
  pe_scan_summary v-bind.after-stop "$scan"
  pe_res v-bind.after-stop.strict_in_dockerd_ns "$(strict_in_ns "$scan" "$dns")"
  pe_check_clear v-bind.after-stop.no-reference v-bind.after-stop "$scan"
  pe_daemon_restart
  dns=$(pe_dockerd_mntns)
  PE_DOCKERD_NS=$dns
  scan=$(pe_tree_scan "$TV" "$ids")
  pe_scan_summary v-bind.after-restart "$scan"
  pe_res v-bind.after-restart.strict_in_dockerd_ns "$(strict_in_ns "$scan" "$dns")"
  pe_check_clear v-bind.after-restart.no-reference v-bind.after-restart "$scan"

  # ---- build --------------------------------------------------------------
  TK=$PE_RUN_DIR/tree-K
  pe_inner_sh 20 "mkdir -p '$TK' && printf '%s\n' \
    'FROM $img' \
    'RUN --mount=type=cache,target=/c echo hi > /c/x' \
    'COPY . /ctx' \
    'RUN sleep 5' > '$TK/Dockerfile' && echo data > '$TK/data.txt'"
  ids=$(pe_tree_ids "$TK")
  pe_res tree-K.ids "$(tr '\n' ' ' <<<"$ids")"

  # Positive and negative control for the scanner on this tree: a running
  # container bound to it is seen; once removed, it is not.
  pe_inner 60 run -d --name c-ctl -v "$TK:/w" "$img" sleep 600 >/dev/null
  scan=$(pe_tree_scan "$TK" "$ids")
  pe_scan_summary control.tree-K-bound "$scan"
  if ! grep -q '^MNT-STRICT ' <<<"$scan"; then
    pe_incomplete tree-scan "positive control failed: bind of tree-K not seen"
    pe_exp_finish
  fi
  pe_scan_control "$scan"
  pe_inner 60 rm -f c-ctl >/dev/null
  scan=$(pe_tree_scan "$TK" "$ids")
  pe_scan_summary control.tree-K-removed "$scan"
  pe_check_clear control.tree-K-clear-before-build control.tree-K-removed "$scan"

  # Positive control for the MAP category: a running process whose executable
  # is a file inside a tree has that file mapped, so the scanner must report a
  # MAP line for it (and the exe as an FD line). If that does not show, MAP is
  # reported as uncontrolled and the experiment is INCOMPLETE.
  TMAP=$PE_RUN_DIR/tree-MAP
  pe_inner_sh 20 "mkdir -p '$TMAP' && cp /bin/busybox '$TMAP/busybox'"
  mids=$(pe_tree_ids "$TMAP")
  if pe_inner 60 run -d --name c-map --entrypoint /w/busybox -v "$TMAP:/w" "$img" sleep 600 >/dev/null 2>&1; then
    scan=$(pe_tree_scan "$TMAP" "$mids")
    pe_scan_summary control.map-fixture "$scan"
    if grep -q '^MAP ' <<<"$scan"; then
      pe_res control.map PASS
    else
      pe_res control.map "uncontrolled (a process executing a file in the tree produced no MAP line)"
      pe_incomplete map-category "positive control failed: no MAP line for a mapped executable"
    fi
    pe_inner 60 rm -f c-map >/dev/null
  else
    pe_res control.map "uncontrolled (could not run the mapping fixture)"
    pe_incomplete map-category "mapping fixture did not run"
  fi
  pe_inner_sh 20 "rm -rf '$TMAP'"

  if ! pe_inner 30 buildx version >/dev/null 2>&1; then
    pe_incomplete buildx "docker buildx is not available in the daemon container"
    pe_exp_finish
  fi
  out=$(cache_dump) || true
  n_before=$(grep -c '^ID:' <<<"$out" || true)
  pe_res cache.before_build.records "$n_before"

  # Build in the background, sampling the scanner while it runs (supplementary:
  # a bounded sampling window, not proof of absence between samples).
  mkdir -p "$here/../../.uzi/scratch"
  blog=$(mktemp "$here/../../.uzi/scratch/pe-build.XXXXXX")
  (pe_inner 300 build -t k-img "$TK" >"$blog" 2>&1) &
  bpid=$!
  samples=0
  during=0
  unr=0
  for ((i = 0; i < 120; i++)); do
    kill -0 "$bpid" 2>/dev/null || break
    scan=$(pe_tree_scan "$TK" "$ids")
    samples=$((samples + 1))
    during=$((during + $(pe_scan_refs "$scan")))
    unr=$((unr + $(sed -n 's/^SCANNED .* unreadable=\([0-9]*\) .*$/\1/p' <<<"$scan" | tail -1)))
    while IFS= read -r hit; do
      [[ -n $hit && -z ${HITS[$hit]:-} ]] || continue
      HITS[$hit]=1
      pe_res during-build.holder "$(cut -c1-240 <<<"$hit")"
    done < <(grep -E '^(MNT|FD|MAP)' <<<"$scan" | sed -E 's/ pid=[0-9]+//' | sort -u)
  done
  rc=0
  wait "$bpid" || rc=$?
  pe_res build.rc "$rc"
  pe_res build.log_tail "$(tail -3 "$blog" | cut -c1-200)"
  rm -f "$blog"
  pe_res during-build.supplementary_sampling "samples=$samples refs_total=$during unreadable_total=$unr (bounded sampling, supplementary)"
  if ((rc != 0)); then
    pe_incomplete build "docker build failed (rc=$rc)"
    pe_exp_finish
  fi

  scan=$(pe_tree_scan "$TK" "$ids")
  pe_scan_summary after-build "$scan"
  pe_check_clear after-build.no-reference after-build "$scan"

  out=$(cache_dump) || true
  n_before=$(grep -c '^ID:' <<<"$out" || true)
  types_before=$(grep '^Type:' <<<"$out" | sort | uniq -c | tr '\n' ';')
  pe_res cache.after_build.records "$n_before"
  pe_res cache.after_build.types "$types_before"
  pe_check cache.exists-after-build test "$n_before" -gt 0
  pe_res cache.after_build.df "$(pe_inner 60 system df -v 2>&1 | grep -A2 -i 'build cache' | head -4 | cut -c1-160)"

  pe_inner_sh 20 "rm -rf '$TK'"
  gone=$(pe_inner_sh 10 "if [ -e '$TK' ]; then echo present; else echo gone; fi") || gone=exec-failed
  pe_check tree-K.deleted test "$gone" = gone
  out=$(cache_dump) || true
  n_after=$(grep -c '^ID:' <<<"$out" || true)
  types_after=$(grep '^Type:' <<<"$out" | sort | uniq -c | tr '\n' ';')
  pe_res cache.after_tree_delete.records "$n_after"
  pe_check cache.survives-tree-delete test "$n_after" -eq "$n_before" -a "$types_after" = "$types_before"
  scan=$(pe_tree_scan "$TK" "$ids")
  pe_scan_summary after-tree-delete "$scan"
  pe_check_clear after-tree-delete.no-reference after-tree-delete "$scan"

  pe_daemon_restart
  # shellcheck disable=SC2034  # read by pe_scan_complete in lib.sh
  PE_DOCKERD_NS=$(pe_dockerd_mntns)
  scan=$(pe_tree_scan "$TK" "$ids")
  pe_scan_summary after-restart "$scan"
  pe_check_clear after-restart.no-reference after-restart "$scan"
  out=$(cache_dump) || true
  n_after=$(grep -c '^ID:' <<<"$out" || true)
  pe_res cache.after_restart.records "$n_after"
  pe_check cache.survives-restart test "$n_after" -eq "$n_before"

  pe_exp_finish
}

pe_exp_main volumes-build run_posture "${1:-}"
