#!/usr/bin/env bash
# Real offline Helm packaging: the baseline misses the stamp, the injected copy carries it.
set -euo pipefail
scripts="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
stamp='0.85.0-rc.3+gabcdef0' # Older worker pin, independent of the chart release.
before="$(cksum "$scripts/../deploy/chart/values.yaml")"
cp -R "$scripts/../deploy/chart" "$work/uzi"
# The chart still has its current AppVersion, but its decoupled worker pin is older.
awk '
  /^workers:/ { inw=1 }
  inw && /^[^[:space:]]/ && $0 !~ /^workers:/ { inw=0 }
  inw && /^  image:/ { inimg=1 }
  inw && inimg && /^  [^[:space:]]/ && $0 !~ /^  image:/ { inimg=0 }
  inw && inimg && /^    tag:/ { print "    tag: \"0.85.0-rc.3\""; next }
  { print }
' "$work/uzi/values.yaml" > "$work/older-values.yaml"
mv "$work/older-values.yaml" "$work/uzi/values.yaml"
# Match existing offline render tests: controller rendering does not use CNPG.
awk '/^dependencies:/ {skip=1; next} skip && /^[^[:space:]-]/ {skip=0} !skip {print}' \
  "$scripts/../deploy/chart/Chart.yaml" > "$work/uzi/Chart.yaml"
rm -f "$work/uzi/Chart.lock"
mkdir "$work/baseline" "$work/fixed" "$work/equal"
helm package "$work/uzi" -d "$work/baseline" >/dev/null
if "$scripts/assert-worker-roll-target-package.sh" "$work"/baseline/uzi-*.tgz "$stamp" > "$work/red.log" 2>&1; then
  echo 'FAIL: baseline unexpectedly supplied target identity' >&2; exit 1
fi
grep -Fq 'packaged controller lacks exact roll target' "$work/red.log"
echo 'BEHAVIORAL RED: unchanged Helm packaging renders no exact worker target'
"$scripts/chart-inject-roll-target.sh" "$work/uzi" "$stamp"
helm package "$work/uzi" -d "$work/fixed" >/dev/null
"$scripts/assert-worker-roll-target-package.sh" "$work"/fixed/uzi-*.tgz "$stamp"
echo 'PASS: packaged chart carries verified older-pin stamp'
equal_before="$(cksum "$work/uzi/values.yaml")"
"$scripts/chart-inject-roll-target.sh" "$work/uzi" "$stamp"
[ "$equal_before" = "$(cksum "$work/uzi/values.yaml")" ]
helm package "$work/uzi" -d "$work/equal" >/dev/null
"$scripts/assert-worker-roll-target-package.sh" "$work"/equal/uzi-*.tgz "$stamp"
echo 'PASS: equal non-empty value is unchanged and packages correctly'
if "$scripts/chart-inject-roll-target.sh" "$work/uzi" '0.85.0-rc.3+g1234567' > "$work/refused.log" 2>&1; then exit 1; fi
[ "$equal_before" = "$(cksum "$work/uzi/values.yaml")" ]
echo 'PASS: differing committed value refuses without modifying copy'
if "$scripts/chart-inject-roll-target.sh" "$work/uzi" '0.86.0+gabcdef0' > "$work/wrong-pin.log" 2>&1; then exit 1; fi
[ "$equal_before" = "$(cksum "$work/uzi/values.yaml")" ]
echo 'PASS: stamp for a different worker pin refuses'
helm template uzi "$work"/fixed/uzi-*.tgz --set workers.enabled=true --set api.tls.enabled=true \
  --set-string workers.rollTargetVersion=0.85.0-rc.3+g1234567 \
  --show-only templates/controller-deployment.yaml > "$work/override.yaml"
grep -Fq 'value: "0.85.0-rc.3+g1234567"' "$work/override.yaml"
echo 'PASS: per-cluster override still wins'
tracked_pin="$(awk '
  /^workers:/ { inw=1; next }
  inw && /^[^[:space:]]/ { inw=0 }
  inw && /^  image:/ { inimg=1; next }
  inw && inimg && /^  [^[:space:]]/ { inimg=0 }
  inw && inimg && /^    tag:/ { v=$2; gsub(/"/, "", v); print v; exit }
' "$scripts/../deploy/chart/values.yaml")"
if "$scripts/chart-inject-roll-target.sh" "$scripts/../deploy/chart" "${tracked_pin}+gabcdef0" > "$work/tracked.log" 2>&1; then exit 1; fi
grep -Fq 'refusing tracked chart' "$work/tracked.log"
[ "$before" = "$(cksum "$scripts/../deploy/chart/values.yaml")" ]
echo 'PASS: tracked chart mutation refused'
echo 'cases=7 passed=7'
