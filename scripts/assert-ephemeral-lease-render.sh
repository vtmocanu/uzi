#!/bin/sh
# Assert the API Deployment renders ephemeral size (issue #2412, l-highmem #3383) and
# UZI_EPHEMERAL_LEASE (PRD #2006) only when workers.ephemeralDefaultSize /
# workers.ephemeralLease are set (issue #4212: both are admin settings, and an env entry
# pins the setting and locks the admin field): unset, null and "" render no entry, an
# explicit 0 survives as "0", and a --set override such as 30m takes effect. Also asserts
# that every worker tier's LimitRange admits the largest preset (issue #3383).
#
# usage: scripts/assert-ephemeral-lease-render.sh [chart-dir]
#   e.g. scripts/assert-ephemeral-lease-render.sh deploy/chart
#
# WHY THIS EXISTS. The lease is a direct env entry on the api Deployment. A template
# written with `with` or a bare truthiness test would read an explicit 0 (lease disabled)
# as unset and drop it, so an operator who pinned the feature off would silently get the
# admin default, with nothing red. This harness renders the chart and proves each
# value reaches the env as written, and that an unset value renders no entry at all.
#
# OFFLINE AND NON-VACUOUS. It calls `helm template` with no network: the CNPG `cluster`
# subchart is stripped from a temp copy of the chart (the api template never references
# it; postgres is gated off by default). Every case first proves the api Deployment
# rendered (its container env block is present); an "absent" expectation is only accepted
# from such a render, and a render that failed or lacks the env block is a BROKEN
# INSTRUMENT (exit 2), never a silent pass.
#
# EXIT CODES (the convention assert-drain-knobs-render.sh sets):
#     2 = the instrument is broken (helm/chart missing, or the env var was absent from a
#         render that was supposed to contain it, or the LimitRange render failed or did
#         not yield exactly three worker LimitRanges)
#     1 = a case rendered the wrong value
#     0 = every property holds
# `task`'s own rc is 201 for any non-zero.
set -eu

# Resolve the chart dir: explicit arg, else relative to this script (../deploy/chart).
# Clear CDPATH first so `cd` cannot resolve via it and echo an unexpected directory.
CDPATH=''
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" && pwd)
CHART_DIR="${1:-$SCRIPT_DIR/../deploy/chart}"

command -v helm >/dev/null 2>&1 || { echo "BROKEN: helm not on PATH" >&2; exit 2; }
[ -f "$CHART_DIR/Chart.yaml" ] || { echo "BROKEN: no Chart.yaml under $CHART_DIR" >&2; exit 2; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM

# Build an offline, dependency-free copy of the chart.
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

CHART="$WORK/chart"
strip_chart "$CHART"

fail=0

# env_of <ENV_NAME> [helm args...]: render the api Deployment and print the env value, or
# the sentinel <absent> when the Deployment rendered (its WORKER_DOCKER_ENABLED entry is
# present) but carries no such entry. A failed render or a render without the sentinel
# neighbour is a broken instrument.
env_of() {
  _name="$1"; shift
  if ! helm template uzi "$CHART" --show-only templates/api-deployment.yaml "$@" \
    > "$WORK/api.yaml" 2> "$WORK/api.err"; then
    echo "BROKEN: api Deployment render failed for '$*':" >&2
    cat "$WORK/api.err" >&2
    exit 2
  fi
  grep -q -- '- name: WORKER_DOCKER_ENABLED$' "$WORK/api.yaml" || {
    echo "BROKEN: api Deployment render lacks its env block for '$*'" >&2
    exit 2
  }
  awk -v want="$_name" '
    $0 ~ "- name: " want "$" { hit = 1; next }
    hit && /value:/ { v = $2; gsub(/"/, "", v); print v; found = 1; exit }
    END { if (!found) print "<absent>" }
  ' "$WORK/api.yaml"
}

# check <ENV_NAME> <label> <expected|<absent>> [--set args...]
check() {
  _env="$1"; _label="$2"; _want="$3"; shift 3
  _got=$(env_of "$_env" "$@")
  if [ "$_got" != "$_want" ]; then
    echo "FAIL ($_label): $_env is '$_got', expected '$_want'" >&2
    fail=1
  else
    echo "OK ($_label): $_env is '$_got'"
  fi
}

# expect_fail <label> <diagnostic> [helm args...]: the render must fail with the message.
expect_fail() {
  _label="$1"; _msg="$2"; shift 2
  if helm template uzi "$CHART" "$@" > "$WORK/bad.yaml" 2> "$WORK/bad.err"; then
    echo "FAIL ($_label): render succeeded, expected a failure" >&2
    exit 1
  fi
  if ! grep -qF "$_msg" "$WORK/bad.err"; then
    echo "BROKEN ($_label): render failed without the expected diagnostic '$_msg'" >&2
    exit 2
  fi
  echo "OK ($_label): render refused with the expected diagnostic"
}

L=UZI_EPHEMERAL_LEASE
check $L "lease default (unset)" "<absent>"
check $L "lease explicit null" "<absent>" --set workers.ephemeralLease=null
check $L "lease empty string" "<absent>" --set-string workers.ephemeralLease=
check $L "lease zero (int) pins off" "0" --set workers.ephemeralLease=0
check $L "lease zero string" "0" --set-string workers.ephemeralLease=0
check $L "lease 0s" "0s" --set workers.ephemeralLease=0s
check $L "lease override 30m" "30m" --set workers.ephemeralLease=30m

if [ "$fail" -ne 0 ]; then
  echo "FAIL: workers.ephemeralLease is NOT correctly wired into the api Deployment (PRD #2006, #4212)" >&2
  exit 1
fi

echo "OK: UZI_EPHEMERAL_LEASE is absent when unset/null/empty, 0 survives, overrides take effect (PRD #2006, #4212)"

expect_fail "lease in api.config" 'Set UZI_EPHEMERAL_LEASE through workers.ephemeralLease, not api.config' \
  --set api.config.UZI_EPHEMERAL_LEASE=1h
expect_fail "lease in api.secretEnv" 'Set UZI_EPHEMERAL_LEASE through workers.ephemeralLease, not api.secretEnv' \
  --set api.secretEnv.UZI_EPHEMERAL_LEASE=another-key
echo "OK: api.config/api.secretEnv duplicates of the ephemeral lease are refused"

# Issue #2412: the adjacent ephemeral size knob is rendered on the same API
# Deployment. Keep its default, overrides and refusal cases in this offline lane.
S=UZI_EPHEMERAL_DEFAULT_SIZE
check $S "size default (unset)" "<absent>"
check $S "size empty string" "<absent>" --set-string workers.ephemeralDefaultSize=
check $S "size explicit null" "<absent>" --set workers.ephemeralDefaultSize=null
check $S "size explicit small" "s" --set workers.ephemeralDefaultSize=s
check $S "size explicit large" "l" --set workers.ephemeralDefaultSize=l
check $S "size null secretEnv" "<absent>" --set api.secretEnv=null
check $S "size explicit high-memory" "l-highmem" --set workers.ephemeralDefaultSize=l-highmem
if [ "$fail" -ne 0 ]; then
  echo "FAIL: workers.ephemeralDefaultSize is NOT correctly wired into the api Deployment (#2412, #4212)" >&2
  exit 1
fi
for invalid in unknown M L-HIGHMEM; do
  expect_fail "invalid size $invalid" 'workers.ephemeralDefaultSize must be one of s, m, l, l-highmem' \
    --set "workers.ephemeralDefaultSize=$invalid"
done
# A well-formed admin-added size name (e.g. xs) must still be refused here: the
# env value is validated at api boot before the size table exists in the store,
# so the chart accepts built-ins only. An admin-set ephemeral_default_size on
# Admin -> Workers may name any existing size.
expect_fail "custom size xs" 'workers.ephemeralDefaultSize must be one of s, m, l, l-highmem' \
  --set "workers.ephemeralDefaultSize=xs"
if helm template uzi "$CHART" --set api.secretEnv.UZI_EPHEMERAL_DEFAULT_SIZE=another-key > "$WORK/duplicate.yaml" 2> "$WORK/duplicate.err"; then
  echo "FAIL: secretEnv duplicated the chart-owned ephemeral size" >&2
  exit 1
fi
grep -qF 'Set UZI_EPHEMERAL_DEFAULT_SIZE through workers.ephemeralDefaultSize, not api.secretEnv' "$WORK/duplicate.err" || exit 2
if helm template uzi "$CHART" --set api.config.UZI_EPHEMERAL_DEFAULT_SIZE=l > "$WORK/config.yaml" 2> "$WORK/config.err"; then
  echo "FAIL: api.config set the chart-owned ephemeral size (the chart value is the one source for it)" >&2
  exit 1
fi
grep -qF 'Set UZI_EPHEMERAL_DEFAULT_SIZE through workers.ephemeralDefaultSize, not api.config' "$WORK/config.err" || exit 2
echo "OK: ephemeral size is absent when unset, accepts s/l/l-highmem, tolerates null secretEnv and rejects invalid values and api.config/secretEnv duplicates"

# Issue #3383: l-highmem's 32Gi memory limit must be admissible in every worker tier.
# Render the three worker namespaces' LimitRanges with the full CI posture (docker tier
# and isolated lane on) and assert each Container max is exactly 4 CPU / 32Gi. Exactly
# three LimitRanges, so a tier silently dropping out is a BROKEN instrument, not a pass.
if ! helm template uzi "$CHART" -f "$SCRIPT_DIR/../deploy/values/ci-render.yaml" \
  --show-only templates/worker-namespace.yaml \
  --show-only templates/worker-docker-namespace.yaml \
  --show-only templates/worker-isolated-namespace.yaml > "$WORK/limitranges.yaml" 2> "$WORK/limitranges.err"; then
  echo "BROKEN: the LimitRange render failed:" >&2
  cat "$WORK/limitranges.err" >&2
  exit 2
fi
awk '
  /^kind: LimitRange$/ { ranges++ }
  /^    - type: / { in_container = ($3 == "Container"); in_max = 0; c = "" }
  in_container && /^      max:$/ { in_max = 1; next }
  in_container && in_max && /^        cpu:/ { c = $2; gsub(/"/, "", c) }
  in_container && in_max && /^        memory:/ {
    m = $2; gsub(/"/, "", m)
    print c " " m
    in_max = 0
  }
  END { print "ranges " ranges + 0 }
' "$WORK/limitranges.yaml" > "$WORK/limitranges.out"
if [ "$(grep -c '^ranges 3$' "$WORK/limitranges.out")" -ne 1 ]; then
  echo "BROKEN: expected exactly 3 worker LimitRange documents (plain, docker, isolated), got: $(tail -n 1 "$WORK/limitranges.out")" >&2
  exit 2
fi
if [ "$(grep -c '^4 32Gi$' "$WORK/limitranges.out")" -ne 3 ] || [ "$(wc -l < "$WORK/limitranges.out")" -ne 4 ]; then
  echo "FAIL: every worker tier's Container LimitRange max must be exactly cpu 4 / memory 32Gi (l-highmem); got:" >&2
  cat "$WORK/limitranges.out" >&2
  exit 1
fi
echo "OK: all three worker LimitRanges admit l-highmem (Container max cpu 4 / memory 32Gi)"
