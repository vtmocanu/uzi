#!/usr/bin/env bash
# Offline registry/verifier fixtures, including digest binding and exact signer identity.
set -euo pipefail
scripts="$(cd "$(dirname "$0")" && pwd)"
cases=0
# Validate the production workflow's job/output and packaging chain offline.
# An override lets mutation probes exercise this assertion without modifying production.
workflow="${ROLL_WORKFLOW_FIXTURE:-$scripts/../.github/workflows/release.yml}"
awk '
  /^  assert-worker-pin:/ { job="pin" }
  /^  publish-chart:/ { job="chart" }
  /^  [a-z][a-z-]*:/ && $0 !~ /^  (assert-worker-pin|publish-chart):/ { job="" }
  job == "pin" && index($0, "steps.worker-pin.outputs.roll_target_version") { output++ }
  job == "pin" && index($0, "roll_target_version=") && index($0, ">>") && index($0, "$GITHUB_OUTPUT") { write++ }
  job == "chart" && /needs:/ && index($0, "assert-worker-pin") { dependency++ }
  job == "chart" && index($0, "needs.assert-worker-pin.outputs.roll_target_version") { input++ }
  job == "chart" && index($0, "cp -R deploy/chart \"$chart_copy\"") { copy=NR }
  job == "chart" && index($0, "./scripts/chart-inject-roll-target.sh \"$chart_copy\" \"$WORKER_ROLL_TARGET_VERSION\"") { inject=NR }
  job == "chart" && index($0, "helm package \"$chart_copy\" -d /tmp/chart") { package=NR }
  job == "chart" && index($0, "./scripts/assert-worker-roll-target-package.sh /tmp/chart/uzi-*.tgz \"$WORKER_ROLL_TARGET_VERSION\"") { verify=NR }
  job == "chart" && index($0, "helm push /tmp/chart/uzi-*.tgz") { push=NR }
  END { if (output != 1 || write != 1 || dependency != 1 || input != 1 ||
            !copy || !(copy < inject && inject < package && package < verify && verify < push)) {
    print "FAIL: release workflow worker stamp/output/package wiring" > "/dev/stderr"; exit 1
  } }
' "$workflow"
cases=$((cases + 1)); echo 'PASS: worker-pin output and copied-chart inject/package/assert ordering'
if [ "${1:-}" = --wiring-only ]; then echo "cases=$cases passed=$cases"; exit 0; fi
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export ROLL_FIXTURE="$work" ROLL_PIN=0.85.0-rc.3
export UZI_ROLL_DOCKER="$work/docker" UZI_ROLL_COSIGN="$work/cosign"
cat > "$work/docker" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$ROLL_FIXTURE/docker.log"
[ "$1 $2 $3" = 'buildx imagetools inspect' ]
ref="$4"
case "$ref" in *agent-base*) t=base;; *agent-jvm*) t=jvm;; *) exit 1;; esac
digest="sha256:$(printf '%064d' 1)"
case "$ref" in
  *":$ROLL_PIN")
    [ "$6" = '{{.Manifest.Digest}}' ]
    [ "${ROLL_MODE:-}" != registry_error ] || exit 1
    if [ "${ROLL_MODE:-}" = bad_digest ]; then echo invalid
    elif [ "${ROLL_MODE:-}" = signed_digest_mismatch ]; then printf 'sha256:%064d\n' 2
    else echo "$digest"; fi;;
  *"@$digest")
    [ "$6" = '{{json .Image.Config}}' ]
    [ "${ROLL_MODE:-}" != config_error ] || exit 1
    cat "$ROLL_FIXTURE/$t.json";;
  *) echo 'config was not read at the resolved digest' >&2; exit 1;;
esac
STUB
cat > "$work/cosign" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$ROLL_FIXTURE/cosign.log"
[ "$1" = verify ]
[ "$2" = --certificate-identity ]
[ "$3" = "https://github.com/vtmocanu/uzi/.github/workflows/release.yml@refs/tags/v$ROLL_PIN" ]
[ "$4" = --certificate-oidc-issuer ]
[ "$5" = https://token.actions.githubusercontent.com ]
[[ "$6" =~ @sha256:[0-9a-f]{64}$ ]]
[[ "$6" = *"@sha256:$(printf '%064d' 1)" ]]
[ "${ROLL_MODE:-}" != signature_error ]
STUB
chmod +x "$work/docker" "$work/cosign"
reset() {
  unset ROLL_MODE
  printf '%s\n' '{"Env":["UZI_AGENT_VERSION=0.85.0-rc.3+gabcdef0"],"Labels":{"org.opencontainers.image.version":"0.85.0-rc.3+gabcdef0"}}' > "$work/base.json"
  cp "$work/base.json" "$work/jvm.json"
  : > "$work/docker.log"; : > "$work/cosign.log"
}
reject() {
  if "$scripts/worker-roll-target.sh" "$ROLL_PIN" > "$work/out" 2> "$work/err"; then
    echo "FAIL: accepted $1" >&2; exit 1
  fi
  [ ! -s "$work/out" ] || { echo "FAIL: failure emitted a stamp" >&2; exit 1; }
  cases=$((cases + 1)); echo "PASS: $1"
}
reset
[ "$("$scripts/worker-roll-target.sh" "$ROLL_PIN")" = '0.85.0-rc.3+gabcdef0' ]
[ "$(wc -l < "$work/docker.log" | tr -d ' ')" = 4 ]
[ "$(wc -l < "$work/cosign.log" | tr -d ' ')" = 2 ]
cases=$((cases + 1)); echo 'PASS: valid older pin, single resolution per template, exact signer and digest-bound config'
for mode in signature_error registry_error bad_digest signed_digest_mismatch config_error; do
  reset; export ROLL_MODE="$mode"; reject "$mode"
  if [ "$mode" = registry_error ]; then grep -Fq 'only a PUBLISHED one' "$work/err"; fi
  if [ "$mode" = signature_error ]; then grep -Fq "expected identity https://github.com/vtmocanu/uzi/.github/workflows/release.yml@refs/tags/v$ROLL_PIN" "$work/err"; fi
done
for mode in missing duplicate label wrong_pin invalid_suffix trailing_newline disagreement malformed; do
  reset
  case "$mode" in
    missing) jq '.Env=[]' "$work/base.json" > "$work/change.json";;
    duplicate) jq '.Env += .Env' "$work/base.json" > "$work/change.json";;
    label) jq '.Labels["org.opencontainers.image.version"]="different"' "$work/base.json" > "$work/change.json";;
    wrong_pin) jq '.Env=["UZI_AGENT_VERSION=0.86.0+gabcdef0"] | .Labels["org.opencontainers.image.version"]="0.86.0+gabcdef0"' "$work/base.json" > "$work/change.json";;
    invalid_suffix) jq '.Env=["UZI_AGENT_VERSION=0.85.0-rc.3+gabcdef00"]' "$work/base.json" > "$work/change.json";;
    trailing_newline) jq '.Env=["UZI_AGENT_VERSION=0.85.0-rc.3+gabcdef0\n"] | .Labels["org.opencontainers.image.version"]="0.85.0-rc.3+gabcdef0\n"' "$work/base.json" > "$work/change.json";;
    disagreement) jq '.Env=["UZI_AGENT_VERSION=0.85.0-rc.3+g1234567"] | .Labels["org.opencontainers.image.version"]="0.85.0-rc.3+g1234567"' "$work/base.json" > "$work/change.json";;
    malformed) printf '{broken' > "$work/change.json";;
  esac
  if [ "$mode" = disagreement ]; then cp "$work/change.json" "$work/jvm.json"; else cp "$work/change.json" "$work/base.json"; fi
  reject "$mode"
done
reset
if "$scripts/worker-roll-target.sh" '0.85.0.*' > "$work/out" 2> "$work/err"; then exit 1; fi
cases=$((cases + 1)); echo 'PASS: malformed pin'
echo "cases=$cases passed=$cases"
