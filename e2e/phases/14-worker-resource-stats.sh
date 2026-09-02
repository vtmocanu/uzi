# shellcheck shell=bash
# phase:    worker-resource-stats
# title:    worker self-reports container CPU/memory stats (PRD #49)
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# --- worker self-reported resource stats (PRD #49) ---------------------------
# The worker reads its OWN cgroup v2 files (memory.current − inactive_file, cpu.stat,
# cpu.max) on every heartbeat and attaches the sample; the API stores the latest on
# the workers DTO. The e2e agent is a private-cgroupns Linux container, so the sample
# must come from the cgroup source (not the process fallback). Poll with a deadline —
# the first heartbeat lands within one WORKER_HEARTBEAT_INTERVAL (5s here) of online —
# rather than sleeping a fixed interval and hoping.
say "worker self-reports container CPU/memory stats (PRD #49)"
wait_eq cgroup 30 "worker stats source" worker_stats_source
STATS_MEM="$(worker_stats_mem)"
[ -n "$STATS_MEM" ] && [ "$STATS_MEM" -gt 0 ] 2>/dev/null \
  || fail "worker stats mem_bytes not populated after a heartbeat (got '${STATS_MEM:-none}')"
pass "worker stats populated from cgroup: source=cgroup mem_bytes=$STATS_MEM"

