#!/bin/sh
# Assert the controller Deployment renders the PRD #422 M5 drain knobs
# (UZI_WORKER_DRAIN_DEADLINE, UZI_WORKER_FORCE_ROLL) from their values.yaml defaults,
# and that the two --set overrides change them.
#
# usage: scripts/assert-drain-knobs-render.sh [chart-dir]
#   e.g. scripts/assert-drain-knobs-render.sh deploy/chart
#
# 🔴 WHY THIS EXISTS. M5 adds a bounded drain deadline and an operator force-roll
# override to the controller's defer-roll logic. The controller reads them from two
# envs the chart injects (workers.drainDeadline -> UZI_WORKER_DRAIN_DEADLINE,
# workers.forceRoll -> UZI_WORKER_FORCE_ROLL). A template edit that dropped or
# misnamed either env would silently fall the controller back to its built-in
# defaults (24h / false) with nothing red — so this harness renders the chart and
# proves the wiring holds and the override levers work.
#
# 🔴 OFFLINE, AND NON-VACUOUS. It calls `helm template` with no network: the CNPG
# `cluster` subchart is stripped from a temp copy of the chart (the controller
# template never references it; postgres is gated OFF by default), so no `helm
# dependency build` / OCI pull is needed. Each render EXTRACTS the env value and
# asserts it; a render that never emitted the env var is treated as a BROKEN
# INSTRUMENT (exit 2), never as a silent pass — an empty grep must not read as green.
#
# EXIT CODES (the convention scan-secrets.sh / assert-worker-tag-decoupled.sh set):
#     2 = the instrument is broken (helm/chart missing, or an env var was absent
#         from a render that was supposed to contain it)
#     1 = a knob rendered the wrong value (default wrong, or an override did not take)
#     0 = every property holds
# `task`'s own rc is 201 for any non-zero.
set -eu

DEFAULT_DEADLINE="24h"    # workers.drainDeadline default in values.yaml
DEFAULT_FORCE_ROLL="false" # workers.forceRoll default in values.yaml
OVERRIDE_DEADLINE="1h"    # a --set that must change the rendered deadline
OVERRIDE_FORCE_ROLL="true" # a --set that must change the rendered force-roll

# Resolve the chart dir: explicit arg, else relative to this script (../deploy/chart).
# Clear CDPATH first so `cd` cannot resolve via it and echo an unexpected directory.
CDPATH=''
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
CHART_DIR="${1:-$SCRIPT_DIR/../deploy/chart}"

command -v helm >/dev/null 2>&1 || { echo "BROKEN: helm not on PATH" >&2; exit 2; }
[ -f "$CHART_DIR/Chart.yaml" ] || { echo "BROKEN: no Chart.yaml under $CHART_DIR" >&2; exit 2; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM

# Build an offline, dependency-free copy of the chart. Stripping the `dependencies:`
# block lets `helm template` render without vendoring the CNPG subchart; the
# controller-deployment template does not use it and postgres.enabled defaults false.
strip_chart() {
  _dst="$1"
  cp -R "$CHART_DIR" "$_dst"
  rm -f "$_dst/Chart.lock"
  awk '
    BEGIN { skip = 0 }
    /^dependencies:/ { skip = 1; next }        # drop the dependencies: list ...
    skip && /^[^[:space:]-]/ { skip = 0 }      # ... until the next top-level key
    skip { next }
    { print }
  ' "$CHART_DIR/Chart.yaml" > "$_dst/Chart.yaml"
}

# Render controller-deployment.yaml and echo the value of the named env var, or print
# nothing (caller treats empty as a broken instrument). Extra --set args are forwarded.
render_env() {
  _chart="$1"; _var="$2"; shift 2
  helm template uzi "$_chart" \
    --set workers.enabled=true --set api.tls.enabled=true \
    --show-only templates/controller-deployment.yaml "$@" 2>/dev/null \
  | awk -v var="$_var" '
      $0 ~ "- name: " var "$" { hit = 1; next }
      hit && /value:/ {
        v = $2
        gsub(/"/, "", v)
        print v
        exit
      }
    '
}

CHART="$WORK/chart"
strip_chart "$CHART"

fail=0

# --- (a) default render: both knobs carry their values.yaml defaults, non-vacuously --
got_deadline=$(render_env "$CHART" "UZI_WORKER_DRAIN_DEADLINE")
if [ -z "$got_deadline" ]; then
  echo "BROKEN: UZI_WORKER_DRAIN_DEADLINE was absent from the default controller render" >&2
  exit 2
fi
if [ "$got_deadline" != "$DEFAULT_DEADLINE" ]; then
  echo "FAIL (a): UZI_WORKER_DRAIN_DEADLINE is '$got_deadline', expected the default '$DEFAULT_DEADLINE'" >&2
  fail=1
else
  echo "OK (a): default render pins UZI_WORKER_DRAIN_DEADLINE to '$got_deadline'"
fi

got_force=$(render_env "$CHART" "UZI_WORKER_FORCE_ROLL")
if [ -z "$got_force" ]; then
  echo "BROKEN: UZI_WORKER_FORCE_ROLL was absent from the default controller render" >&2
  exit 2
fi
if [ "$got_force" != "$DEFAULT_FORCE_ROLL" ]; then
  echo "FAIL (a): UZI_WORKER_FORCE_ROLL is '$got_force', expected the default '$DEFAULT_FORCE_ROLL'" >&2
  fail=1
else
  echo "OK (a): default render pins UZI_WORKER_FORCE_ROLL to '$got_force'"
fi

# --- (b) --set workers.forceRoll=true flips the rendered env ---------------------------
got_force_override=$(render_env "$CHART" "UZI_WORKER_FORCE_ROLL" --set workers.forceRoll=true)
if [ -z "$got_force_override" ]; then
  echo "BROKEN: UZI_WORKER_FORCE_ROLL was absent from the force-roll override render" >&2
  exit 2
fi
if [ "$got_force_override" != "$OVERRIDE_FORCE_ROLL" ]; then
  echo "FAIL (b): --set workers.forceRoll=true rendered '$got_force_override' -- the override did not take" >&2
  fail=1
else
  echo "OK (b): --set workers.forceRoll=true renders UZI_WORKER_FORCE_ROLL='$got_force_override'"
fi

# --- (c) --set workers.drainDeadline=1h changes the rendered env -----------------------
got_deadline_override=$(render_env "$CHART" "UZI_WORKER_DRAIN_DEADLINE" --set workers.drainDeadline="$OVERRIDE_DEADLINE")
if [ -z "$got_deadline_override" ]; then
  echo "BROKEN: UZI_WORKER_DRAIN_DEADLINE was absent from the deadline override render" >&2
  exit 2
fi
if [ "$got_deadline_override" != "$OVERRIDE_DEADLINE" ]; then
  echo "FAIL (c): --set workers.drainDeadline=$OVERRIDE_DEADLINE rendered '$got_deadline_override' -- the override did not take" >&2
  fail=1
else
  echo "OK (c): --set workers.drainDeadline override renders '$got_deadline_override'"
fi

if [ "$fail" -ne 0 ]; then
  echo "FAIL: the M5 drain knobs are NOT correctly wired from values.yaml into the controller Deployment (PRD #422 M5)" >&2
  exit 1
fi

echo "OK: drain knobs render UZI_WORKER_DRAIN_DEADLINE='$DEFAULT_DEADLINE' / UZI_WORKER_FORCE_ROLL='$DEFAULT_FORCE_ROLL' by default, both override levers intact (PRD #422 M5)"
