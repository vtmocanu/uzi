#!/usr/bin/env bash
# Offline token-boundary and proposal contract for the actual workflow. No forge,
# Nix, Docker or real git mutation: run its final shell step against fake gh/git.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKFLOW="${1:-$ROOT/.github/workflows/devbox-update.yml}"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/devbox-update-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
yq -o=json "$WORKFLOW" > "$TMP/workflow.json"
cases=0; passed=0
check() {
  cases=$((cases + 1))
  if "$@"; then passed=$((passed + 1)); echo "PASS: $*"
  else echo "FAIL: $*"; fi
}
contract() { jq -e "$1" "$TMP/workflow.json" >/dev/null; }
check contract '.permissions == {"contents":"read"}'
check contract '.jobs.validate_tools.permissions == {"contents":"read"} and ((.jobs.validate_tools | tostring | contains("secrets.")) | not)'
check contract '[.jobs.validate_tools.steps[] | select((.uses // "") | startswith("actions/checkout@")) | .with["persist-credentials"]] == [false]'
check contract '[.jobs.validate_tools.steps[] | select((.uses // "") | startswith("DeterminateSystems/nix-installer-action@")) | [.env.OTEL_SDK_DISABLED, .with["diagnostic-endpoint"], .with["github-token"]]] == [["true", "", "${{ github.token }}"]]'
check contract '[.jobs.validate_tools.steps[] | select(.name == "Relock and validate uxlab") | .["working-directory"]] == ["api/cmd/uzi/uxlab"]'
check contract '.jobs.propose.if == "always() && !cancelled() && inputs.dry_run != true && ((needs.validate.result == '\''success'\'' && needs.validate.outputs.changed == '\''true'\'') ||\n (needs.validate_tools.result == '\''success'\'' && needs.validate_tools.outputs.changed == '\''true'\''))"'
check contract '.jobs.propose.steps[-1].env.WORKER_VALID == "${{ needs.validate.result == '\''success'\'' && needs.validate.outputs.changed == '\''true'\'' }}"'
check contract '.jobs.propose.steps[-1].env.UXLAB_VALID == "${{ needs.validate_tools.result == '\''success'\'' && needs.validate_tools.outputs.changed == '\''true'\'' }}"'

check contract '.jobs.validate.permissions == {"contents":"read"}'
check contract '[.jobs.validate.steps[] | select((.uses // "") | startswith("actions/checkout@")) | .with["persist-credentials"]] == [false]'
check contract '.jobs.validate != null and ((.jobs.validate | tostring | contains("secrets.")) | not)'
check contract '[.jobs.validate.steps[] | select((.uses // "") | startswith("DeterminateSystems/nix-installer-action@")) | [.env.OTEL_SDK_DISABLED, .with["diagnostic-endpoint"], .with["github-token"]]] == [["true", "", "${{ github.token }}"]]'
check contract '.jobs.propose.needs == ["validate", "validate_tools"] and .jobs.propose.permissions == {"contents":"write","pull-requests":"write","issues":"write","actions":"read"}'
check contract '[.jobs.propose.steps[] | has("uses")] | any | not'
check contract '(.jobs.propose.env // {}) == {} and (.jobs.propose.steps | length) == 1'
check contract '.jobs.propose.steps[-1].env.GH_TOKEN == "${{ secrets.DEVBOX_UPDATE_TOKEN || github.token }}"'
check contract '.jobs.propose.steps[-1].env.ARTIFACT_TOKEN == "${{ github.token }}"'
check contract '.on.workflow_dispatch.inputs.dry_run.default == false and (.jobs.propose.if | contains("inputs.dry_run != true"))'

# If the security split is absent, the structural regression above must fail;
# never read an absent proposal step as a vacuous behavioral success.
if ! jq -e '.jobs.propose.steps[-1].run | type == "string"' "$TMP/workflow.json" >/dev/null; then
  echo "cases=$cases passed=$passed"; exit 1
fi
jq -r '.jobs.propose.steps[-1].run' "$TMP/workflow.json" > "$TMP/propose.sh"
mkdir -p "$TMP/bin"
printf '{"lockfile_version":"1","packages":{}}\n' > "$TMP/lock"
LOCK_SHA="$(sha256sum "$TMP/lock" | awk '{print $1}')"
printf '{"lockfile_version":"1","packages":{"charm-freeze@0.2.2":{}}}\n' > "$TMP/uxlab-lock"
UXLAB_SHA="$(sha256sum "$TMP/uxlab-lock" | awk '{print $1}')"
cat > "$TMP/bin/gh" <<'STUB'
#!/bin/bash
set -eu
printf 'gh %s\n' "$*" >> "$CALLS"
if [ "$1 $2" != 'run download' ]; then
  [ "$GH_TOKEN" = proposal-fixture ] || { echo 'proposal lost its PAT binding' >&2; exit 2; }
fi
case "$1 $2" in
  'run download')
    [ "$GH_TOKEN" = artifact-fixture ] || { echo 'artifact download must use built-in token' >&2; exit 2; }
    name=""; dest=""
    while [ "$#" -gt 0 ]; do
      case "$1" in --name) name="$2"; shift 2;; --dir) dest="$2"; shift 2;; *) shift;; esac
    done
    case "$name:$WORKER_VALID:$UXLAB_VALID" in
      validated-devbox-lock:true:*|validated-uxlab-lock:*:true) ;;
      *) echo 'attempted an unvalidated artifact' >&2; exit 2;;
    esac
    mkdir -p "$dest"
    if [ "$name" = validated-uxlab-lock ]; then cp "$FIXTURE_UXLAB_LOCK" "$dest/devbox.lock"
    else cp "$FIXTURE_LOCK" "$dest/devbox.lock"; fi
    case "$MODE" in
      corrupt) echo corrupt >> "$dest/devbox.lock" ;;
      extra) echo extra > "$dest/extra" ;;
      symlink) rm "$dest/devbox.lock"; ln -s "$FIXTURE_LOCK" "$dest/devbox.lock" ;;
      missing) rm "$dest/devbox.lock" ;;
    esac
    ;;
  'pr list')
    case "$MODE" in query-failed) exit 1;; occupied) echo 123;; esac
    ;;
  'pr create') [ "$MODE" != pr-refused ] ;;
  'issue list') [ "$MODE" != issue-existing ] || echo 456 ;;
  'label create'|'issue create') ;;
  *) echo "unexpected gh command: $*" >&2; exit 2 ;;
esac
STUB
cat > "$TMP/bin/git" <<'STUB'
#!/bin/bash
set -eu
printf 'git %s\n' "$*" >> "$CALLS"
case "$1" in
  ls-remote)
    case "$MODE" in
      lease|foreign|diff-failed|outside-subset|uxlab-lease) exit 0 ;;
      remote-failed) exit 1 ;;
      *) exit 2 ;;
    esac ;;
  diff)
    case "$MODE" in foreign) echo README.md;; outside-subset|uxlab-lease) echo api/cmd/uzi/uxlab/devbox.lock;; diff-failed) exit 1;; esac ;;
  rev-parse) echo 0123456789012345678901234567890123456789 ;;
  push) [ "$MODE" != push-refused ] ;;
  init|remote|fetch|checkout|config|add|commit) ;;
  *) echo "unexpected git command: $*" >&2; exit 2 ;;
esac
STUB
chmod +x "$TMP/bin/gh" "$TMP/bin/git"
proposal_case() {
  local mode="$1" expected="$2" rc=0 worker=true uxlab=false dry=false uxsha="$UXLAB_SHA"
  case "$mode" in uxlab-only|uxlab-lease|uxlab-bad-sha) worker=false; uxlab=true;; both|both-bad-uxlab) uxlab=true;; neither) worker=false;; dry) dry=true;; esac
  case "$mode" in uxlab-bad-sha|both-bad-uxlab) uxsha="$LOCK_SHA";; esac
  mkdir -p "$TMP/$mode/workspace/agent/devbox-global" "$TMP/$mode/workspace/api/cmd/uzi/uxlab" "$TMP/$mode/runner"
  : > "$TMP/$mode/calls"
  (cd "$TMP/$mode/workspace" && env -i PATH="$TMP/bin:$PATH" HOME="$TMP/$mode" \
    MODE="$mode" CALLS="$TMP/$mode/calls" FIXTURE_LOCK="$TMP/lock" FIXTURE_UXLAB_LOCK="$TMP/uxlab-lock" LOCK_SHA="$LOCK_SHA" \
    WORKER_VALID="$worker" UXLAB_VALID="$uxlab" DRY_RUN="$dry" UXLAB_SHA="$uxsha" \
    GH_TOKEN=proposal-fixture ARTIFACT_TOKEN=artifact-fixture GITHUB_REPOSITORY=example/repo GITHUB_SERVER_URL=https://github.com \
    GITHUB_RUN_ID=42 GITHUB_SHA=0123456789012345678901234567890123456789 \
    RUNNER_TEMP="$TMP/$mode/runner" bash "$TMP/propose.sh") > "$TMP/$mode/out" 2>&1 || rc=$?
  case "$expected" in
    invalid)
      [ "$rc" -ne 0 ] && ! grep -E '^(git push|gh (pr create|issue create|label create))' "$TMP/$mode/calls" || return 1
      case "$mode" in
        corrupt|uxlab-bad-sha|both-bad-uxlab) grep -F FAILED "$TMP/$mode/out" ;;
        extra) grep -F 'Unexpected validated artifact contents.' "$TMP/$mode/out" ;;
        symlink|missing) grep -F 'Missing or linked validated lockfile.' "$TMP/$mode/out" ;;
      esac ;;
    pr)
      [ "$rc" -eq 0 ] && grep -F 'git push origin HEAD:refs/heads/chore/devbox-toolchain-update' "$TMP/$mode/calls" \
        && grep -F 'gh pr create --base main --head chore/devbox-toolchain-update --title chore(deps): refresh devbox toolchain (nixpkgs pin)' "$TMP/$mode/calls" \
        && ! grep -F 'gh issue create' "$TMP/$mode/calls" \
        && grep -F 'main CI re-validates the worker toolchain on merge.' "$TMP/$mode/calls" ;;
    uxlab)
      [ "$rc" -eq 0 ] && grep -F 'git add api/cmd/uzi/uxlab/devbox.lock' "$TMP/$mode/calls" \
        && ! grep -F 'git add agent/devbox-global/devbox.lock' "$TMP/$mode/calls" \
        && grep -F 'Uxlab lock: validated by a successful devbox install.' "$TMP/$mode/calls" \
        && ! grep -F 'Worker lock: validated' "$TMP/$mode/calls" \
        && ! grep -F 'main CI re-validates' "$TMP/$mode/calls" ;;
    both)
      [ "$rc" -eq 0 ] && grep -F 'git add agent/devbox-global/devbox.lock api/cmd/uzi/uxlab/devbox.lock' "$TMP/$mode/calls" ;;
    none)
      [ "$rc" -eq 0 ] && [ ! -s "$TMP/$mode/calls" ] ;;
    lease)
      [ "$rc" -eq 0 ] && grep -F 'git push --force-with-lease=refs/heads/chore/devbox-toolchain-update:0123456789012345678901234567890123456789' "$TMP/$mode/calls" ;;
    issue)
      [ "$rc" -eq 0 ] && grep -F 'gh issue create' "$TMP/$mode/calls" ;;
    protected)
      [ "$rc" -eq 0 ] && ! grep -F 'git push' "$TMP/$mode/calls" && grep -F 'gh issue create' "$TMP/$mode/calls" ;;
  esac
}
for mode in corrupt extra symlink missing; do check proposal_case "$mode" invalid; done
check proposal_case create pr
check proposal_case lease lease
check proposal_case pr-refused issue
check proposal_case push-refused issue
for mode in occupied query-failed remote-failed foreign diff-failed outside-subset; do check proposal_case "$mode" protected; done
check proposal_case uxlab-only uxlab
check proposal_case both both
check proposal_case neither none
check proposal_case dry none
check proposal_case uxlab-lease lease
check proposal_case uxlab-bad-sha invalid
check proposal_case both-bad-uxlab invalid
printf 'cases=%s passed=%s\n'  "$cases" "$passed"
[ "$cases" -eq 39 ] && [ "$cases" -eq "$passed" ]
