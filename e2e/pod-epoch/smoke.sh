#!/usr/bin/env bash
# Smoke test for the pod-epoch harness. Usage: smoke.sh <rootless|rootful|all>
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$here/lib.sh"

# events start -> daemon restart -> stop (stream ends on its own), then
# start -> activity -> stop (we end it). Each must capture lines.
events_step() {
  local posture=$1 out lines
  mkdir -p "$here/../../.uzi/scratch"
  out=$(mktemp "$here/../../.uzi/scratch/pe-events.XXXXXX")
  pe_events_start "$out"
  pe_inner 60 run --rm --name evprobe "$PE_FIXTURE_IMAGE" true >/dev/null
  pe_daemon_restart
  pe_events_stop || pe_fail 'events process not verified gone after restart'
  lines=$(wc -l <"$out")
  echo "EVENTS $posture restart: PE_EVENTS_RC=$PE_EVENTS_RC end=$PE_EVENTS_END lines=$lines"
  ((lines > 0)) || pe_fail 'no events captured across restart'
  [[ $PE_EVENTS_END == ended ]] || pe_fail "expected stream to end on restart, got $PE_EVENTS_END"

  pe_events_start "$out"
  pe_inner 60 run --rm --name evprobe2 "$PE_FIXTURE_IMAGE" true >/dev/null
  pe_events_stop || pe_fail 'events process not verified gone after stop'
  lines=$(wc -l <"$out")
  echo "EVENTS $posture stop: PE_EVENTS_RC=$PE_EVENTS_RC end=$PE_EVENTS_END lines=$lines"
  ((lines > 0)) || pe_fail 'no events captured'
  [[ $PE_EVENTS_END == stopped ]] || pe_fail "expected stopped, got $PE_EVENTS_END"
  rm -f "$out"
}

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

  pe_assert_no_public_listener
  echo "LISTENERS $posture no non-loopback TCP listener"

  events_step "$posture"

  pe_daemon_restart
  pe_daemon_replace
  pe_assert_no_public_listener

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
