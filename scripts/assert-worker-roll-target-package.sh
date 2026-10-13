#!/usr/bin/env bash
# Extract the shipping artifact and prove the controller sees exactly the verified stamp.
set -euo pipefail
[ "$#" -eq 2 ] || { echo "usage: $0 <chart.tgz> <verified-stamp>" >&2; exit 2; }
package="$1" stamp="$2"
[[ "$stamp" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?\+g[0-9a-f]{7}$ ]] || exit 2
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
tar -xzf "$package" -C "$work"
helm template uzi "$work/uzi" --set workers.enabled=true --set api.tls.enabled=true \
  --show-only templates/controller-deployment.yaml > "$work/controller.yaml"
awk -v stamp="$stamp" '
  /- name: UZI_WORKER_ROLL_TARGET_VERSION$/ { count++; hit=1; next }
  hit && /value:/ { value=$2; gsub(/"/, "", value); if (value != stamp) bad=1; values++; hit=0 }
  END { if (bad || count != 1 || values != 1) { print "packaged controller lacks exact roll target" > "/dev/stderr"; exit 1 } }
' "$work/controller.yaml"
