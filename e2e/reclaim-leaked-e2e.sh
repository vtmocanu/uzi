#!/usr/bin/env bash
# Reclaim leaked default-named E2E compose projects before a new run builds.
#
# WHY: run-e2e.sh names its stack `uzi-e2e-$$`. A run whose EXIT trap never fires
# (session teardown, SIGKILL mid-build) leaves that project's containers + named
# volumes behind — dockerd keeps them alive independent of the harness shell — and
# enough leaked projects fill the Docker data-root until the NEXT run's worker
# crashes at startup with ENOSPC and never heartbeats, surfacing as a misleading
# `worker status never reached 'online'` red. A completed run tears itself down
# (`down -v`); only ABORTED runs leak, and nothing else reclaims them.
#
# SAFETY (this must never destroy real or concurrent-live data):
#   - It NEVER globs `uzi-`. Only names matching ^uzi-e2e-<digits>$ (the PID-suffixed
#     DEFAULT project name) are eligible. It never matches the real dev stack `uzi`
#     (containers uzi-db-1 ...), store-it's `uzi-store-it-*`, or a custom
#     UZI_E2E_COMPOSE_PROJECT not shaped like `uzi-e2e-<digits>`. A custom name
#     that IS shaped that way matches the regex, but is still protected by the
#     current-project name-exclusion ($1) and the pid-liveness check below.
#   - The PID is embedded in the name. A project is reclaimed ONLY when `kill -0 <pid>`
#     proves the process is GONE (ESRCH). A concurrent live run (alive pid) and the
#     current run (own pid alive + name-excluded via $1) are always skipped. EPERM
#     (another user's live pid) and any ambiguous outcome are treated as ALIVE. The
#     bias is always toward NOT destroying.
#   - No host-wide pruning: no `docker system prune`, no `docker image prune`. Only a
#     per-dead-project `docker compose -p <proj> down -v --remove-orphans`, exactly how
#     the harness already tears itself down.
#   - CI-safe: ephemeral runners have zero candidates, so this is a fast no-op.
# Opt out with UZI_E2E_NO_RECLAIM=1.
set -euo pipefail

CURRENT_PROJECT="${1:-}"

if [ -n "${UZI_E2E_NO_RECLAIM:-}" ]; then
  printf '[reclaim] skipped (UZI_E2E_NO_RECLAIM set)\n'
  exit 0
fi

if ! command -v docker >/dev/null 2>&1; then
  printf '[reclaim] docker not found; skipping leaked-project reclaim\n'
  exit 0
fi

# pid_dead PID -> rc 0 ONLY when the process is definitely gone (ESRCH). Signal
# delivered (alive), EPERM (another user's live pid), or any other outcome -> rc 1
# (treated as alive; never reclaimed). ESRCH is detected via the error text, which is
# the only signal `kill -0` exposes in bash; a non-English locale that fails to match
# falls through to the safe "treat as alive" branch (a leak is missed, never a live
# project destroyed).
pid_dead() {
  local pid="$1" err
  if err="$(kill -0 "$pid" 2>&1)"; then
    return 1
  fi
  case "$err" in
    *'o such process'*) return 0 ;;
    *) return 1 ;;
  esac
}

# The trailing `|| true` keeps a docker-daemon-unreachable failure from aborting
# under `set -euo pipefail`: enumeration then yields an empty set and we fall
# through to the "no leaked e2e projects found" line rather than exiting silently.
candidates="$(
  {
    docker ps -a --format '{{.Label "com.docker.compose.project"}}'
    docker volume ls --format '{{.Label "com.docker.compose.project"}}'
  } 2>/dev/null | sort -u || true
)"

reclaimed=0
while IFS= read -r proj; do
  [ -n "$proj" ] || continue
  [[ "$proj" =~ ^uzi-e2e-([0-9]+)$ ]] || continue
  pid="${BASH_REMATCH[1]}"
  if [ "$proj" = "$CURRENT_PROJECT" ]; then continue; fi
  pid_dead "$pid" || continue
  printf '[reclaim] tearing down leaked project %s (pid %s is gone)\n' "$proj" "$pid"
  docker compose -p "$proj" down -v --remove-orphans >/dev/null 2>&1 || true
  reclaimed=$((reclaimed + 1))
done < <(printf '%s\n' "$candidates")

if [ "$reclaimed" -gt 0 ]; then
  printf '[reclaim] reclaimed %d leaked e2e project(s)\n' "$reclaimed"
else
  printf '[reclaim] no leaked e2e projects found\n'
fi
exit 0
