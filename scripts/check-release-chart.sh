#!/usr/bin/env bash
# Render the actual tagged chart; use the existing render assertions unchanged.
set -euo pipefail
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
helm dependency build deploy/chart
helm lint deploy/chart
helm template uzi deploy/chart -f deploy/values/ci-render.yaml --namespace uzi > "$scratch/chart.yaml"
sh scripts/assert-chart-render.sh "$scratch/chart.yaml"
helm template uzi deploy/chart -f deploy/values/ci-render.yaml --namespace uzi \
  --show-only templates/controller-deployment.yaml > "$scratch/controller.yaml"
sh scripts/assert-controller-strategy.sh "$scratch/controller.yaml"
