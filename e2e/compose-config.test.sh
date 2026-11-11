#!/usr/bin/env bash
set -euo pipefail

# This integration check needs the Compose client, but never a daemon or stack.
# Keep it outside test:e2e-lib's Docker-free contract. CI must execute the renders.
if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
  if [ -n "${CI:-}${GITHUB_ACTIONS:-}" ]; then
    echo 'FAIL: Docker Compose is required for the E2E overlay render check in CI' >&2
    exit 1
  fi
  echo 'SKIP: E2E overlay renders need Docker Compose (required in CI; no daemon needed)'
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT

# Real Compose rendering. All inputs are dummy and isolated from host credentials.
render() {
  env -i PATH="$PATH" HOME="$SCRATCH" JWT_SECRET=fixture-jwt-secret-000000000000000000 \
    UZI_SECRET_KEY=fixture-key POSTGRES_PASSWORD=fixture-password UZI_WORKER_TOKEN=fixture-token \
    E2E_RUN_DIR="$SCRATCH" E2E_WEB_PORT=28080 E2E_FAKE_PORT=28443 "$@" \
    docker compose --env-file /dev/null -p cdr-outbox-render \
    -f "$ROOT/docker-compose.yml" -f "$ROOT/e2e/docker-compose.e2e.yml" config --format json > "$SCRATCH/render.json"
}
for setting in default false true; do
  if [ "$setting" = default ]; then render; expected=''
  else render "E2E_RUN_PUBLICATION_RESUME_ENABLED=$setting"; expected="$setting"; fi
  jq -e --arg expected "$expected" '.services.api.environment.RUN_PUBLICATION_RESUME_ENABLED == $expected' "$SCRATCH/render.json" >/dev/null
done
echo 'PASS: real Compose renders default empty, explicit off and on without touching base defaults'

# #3644 independent restore: the compose slot predicate needs hostname == container id[0:12] and a private
# UTS/network namespace, so the rendered agent service must not override the hostname, join the host's UTS
# namespace, or share another namespace's network. The agent is profile-gated, so render it with its profile.
env -i PATH="$PATH" HOME="$SCRATCH" JWT_SECRET=fixture-jwt-secret-000000000000000000 \
  UZI_SECRET_KEY=fixture-key POSTGRES_PASSWORD=fixture-password UZI_WORKER_TOKEN=fixture-token \
  E2E_RUN_DIR="$SCRATCH" E2E_WEB_PORT=28080 E2E_FAKE_PORT=28443 \
  docker compose --env-file /dev/null -p cdr-outbox-render --profile agent \
  -f "$ROOT/docker-compose.yml" -f "$ROOT/e2e/docker-compose.e2e.yml" config --format json > "$SCRATCH/render-agent.json"
jq -e '.services.agent | type == "object"' "$SCRATCH/render-agent.json" >/dev/null
jq -e '.services.agent | has("hostname") | not' "$SCRATCH/render-agent.json" >/dev/null
jq -e '.services.agent | has("uts") | not' "$SCRATCH/render-agent.json" >/dev/null
jq -e '.services.agent | (.network_mode // "") | (. == "host" or startswith("container:") or startswith("service:")) | not' \
  "$SCRATCH/render-agent.json" >/dev/null
echo 'PASS: rendered agent service sets no hostname or uts and joins no host/container/service network namespace'
