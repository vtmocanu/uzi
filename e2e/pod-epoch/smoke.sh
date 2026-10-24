#!/usr/bin/env bash
# Smoke test for the pod-epoch harness. Usage: smoke.sh <rootless|rootful|all>
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$here/lib.sh"

run_posture() {
  local posture=$1 names
  pe_init "$posture"
  pe_volumes_create
  pe_daemon_create_start
  pe_server_info

  if ! pe_inner 10 image inspect "$PE_FIXTURE_IMAGE" >/dev/null 2>&1; then
    pe_inner 240 pull "$PE_FIXTURE_IMAGE" >/dev/null
  fi
  pe_inner 60 run --name before "$PE_FIXTURE_IMAGE" sh -c 'echo ran' >/dev/null
  pe_inner_sh 20 "mkdir -p $PE_RUN_DIR/smoke && echo epoch1 > $PE_RUN_DIR/smoke/marker"

  pe_daemon_restart
  pe_daemon_replace

  names=$(pe_inner 10 ps -a --format '{{.Names}}')
  [[ $names == *before* ]] || pe_fail "container 'before' lost across restart/replace (got: $names)"
  [[ $(pe_inner_sh 10 "cat $PE_RUN_DIR/smoke/marker") == epoch1 ]] \
    || pe_fail 'shared run workdir did not persist'
  echo "SMOKE $posture PASS"
}

main() {
  local which=${1:-}
  case $which in
    rootless | rootful) run_posture "$which" ;;
    all)
      # One subshell per posture so each EXIT trap cleans its own resources.
      local p
      for p in rootless rootful; do
        (run_posture "$p")
      done
      ;;
    *) echo "usage: $0 <rootless|rootful|all>" >&2; exit 2 ;;
  esac
}
main "$@"
