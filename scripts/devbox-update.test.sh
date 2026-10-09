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
check contract '.jobs.validate.permissions == {"contents":"read"}'
check contract '[.jobs.validate.steps[] | select((.uses // "") | startswith("actions/checkout@")) | .with["persist-credentials"]] == [false]'
check contract '.jobs.validate != null and ((.jobs.validate | tostring | contains("secrets.")) | not)'
check contract '[.jobs.validate.steps[] | select((.uses // "") | startswith("DeterminateSystems/nix-installer-action@")) | [.env.OTEL_SDK_DISABLED, .with["diagnostic-endpoint"], .with["github-token"]]] == [["true", "", "${{ github.token }}"]]'
check contract '.jobs.propose.needs == "validate" and .jobs.propose.permissions == {"contents":"write","pull-requests":"write","issues":"write","actions":"read"}'
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
    while [ "$#" -gt 0 ]; do
      if [ "$1" = --dir ]; then dest="$2"; break; fi
      shift
    done
    mkdir -p "$dest"
    cp "$FIXTURE_LOCK" "$dest/devbox.lock"
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
      lease|foreign|diff-failed) exit 0 ;;
      remote-failed) exit 1 ;;
      *) exit 2 ;;
    esac ;;
  diff)
    case "$MODE" in foreign) echo README.md;; diff-failed) exit 1;; esac ;;
  rev-parse) echo 0123456789012345678901234567890123456789 ;;
  push) [ "$MODE" != push-refused ] ;;
  init|remote|fetch|checkout|config|add|commit) ;;
  *) echo "unexpected git command: $*" >&2; exit 2 ;;
esac
STUB
chmod +x "$TMP/bin/gh" "$TMP/bin/git"
proposal_case() {
  local mode="$1" expected="$2" rc=0
  mkdir -p "$TMP/$mode/workspace/agent/devbox-global" "$TMP/$mode/runner"
  : > "$TMP/$mode/calls"
  (cd "$TMP/$mode/workspace" && env -i PATH="$TMP/bin:$PATH" HOME="$TMP/$mode" \
    MODE="$mode" CALLS="$TMP/$mode/calls" FIXTURE_LOCK="$TMP/lock" LOCK_SHA="$LOCK_SHA" \
    GH_TOKEN=proposal-fixture ARTIFACT_TOKEN=artifact-fixture GITHUB_REPOSITORY=example/repo GITHUB_SERVER_URL=https://github.com \
    GITHUB_RUN_ID=42 GITHUB_SHA=0123456789012345678901234567890123456789 \
    RUNNER_TEMP="$TMP/$mode/runner" bash "$TMP/propose.sh") > "$TMP/$mode/out" 2>&1 || rc=$?
  case "$expected" in
    invalid)
      [ "$rc" -ne 0 ] && ! grep -E '^(git push|gh (pr create|issue create|label create))' "$TMP/$mode/calls" || return 1
      case "$mode" in
        corrupt) grep -F FAILED "$TMP/$mode/out" ;;
        extra) grep -F 'Unexpected validated artifact contents.' "$TMP/$mode/out" ;;
        symlink|missing) grep -F 'Missing or linked validated lockfile.' "$TMP/$mode/out" ;;
      esac ;;
    pr)
      [ "$rc" -eq 0 ] && grep -F 'git push origin HEAD:refs/heads/chore/devbox-toolchain-update' "$TMP/$mode/calls" \
        && grep -F 'gh pr create --base main --head chore/devbox-toolchain-update --title chore(deps): refresh devbox worker toolchain (nixpkgs pin)' "$TMP/$mode/calls" \
        && ! grep -F 'gh issue create' "$TMP/$mode/calls" ;;
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
for mode in occupied query-failed remote-failed foreign diff-failed; do check proposal_case "$mode" protected; done
printf 'cases=%s passed=%s\n' "$cases" "$passed"
[ "$cases" -eq 24 ] && [ "$cases" -eq "$passed" ]
