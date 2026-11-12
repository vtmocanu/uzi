#!/usr/bin/env bash
# Real helper/phase fragments and real local Git, without Docker or a live API.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB="$ROOT/e2e/lib.sh"
P45="$ROOT/e2e/phases/45-bounded-concurrency.sh"
P52="${RESTORE_FIXTURE_PHASE52:-$ROOT/e2e/phases/52-api-outage-outbox.sh}"
mkdir -p "$ROOT/.uzi/scratch"
S="$(mktemp -d "$ROOT/.uzi/scratch/restore-fixtures.XXXXXX")"
trap 'rm -rf "$S"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { :; }
say() { :; }
eval "$(awk '/^(enable_restore_fixture_repo|restore_fixture_repo_states)\(\) \{/,/^\}/' "$LIB")"
reset() {
  RESTORE_FIXTURE_REPOS=()
  REPO_ID=base-repo
  unset OUTBOX_REPO_ID UNCERTAIN_ENABLE
  : > "$S/calls"
  jq -n '{repos:[range(0;4) | . as $i | {id:("fixture-"+($i|tostring)),
    path_with_namespace:(["group/restore45a","group/restore45b","group/restore52a","group/restore52b"][$i]),enabled:false}]}' > "$S/projects"
}
apiget() {
  case "$1" in
    /api/forge/connections) echo '{"connections":[{"id":"connection"}]}' ;;
    /api/forge/connections/connection/projects) cat "$S/projects" ;;
    *) fail "unexpected GET $1" ;;
  esac
}
apiput() {
  local id="${1##*/}" enabled
  enabled="$(jq -r .enabled <<< "$2")"
  jq --arg id "$id" --argjson enabled "$enabled" '.repos |= map(if .id==$id then .enabled=$enabled else . end)' "$S/projects" > "$S/next"
  mv "$S/next" "$S/projects"
  if [ "${UNCERTAIN_ENABLE:-}" = 1 ] && [ "$enabled" = true ]; then return 1; fi
  jq -n --argjson enabled "$enabled" '{repo:{enabled:$enabled}}'
}
apipost() {
  case "$1" in
    /api/repos/*/issues) printf 'issue %s\n' "${1%/issues}" >> "$S/calls"; echo '{"card":{"iid":99}}' ;;
    /api/runs/*/inputs) echo '{}' ;;
    *) fail "unexpected POST $1" ;;
  esac
}
create_run() { printf 'run /api/repos/%s\n' "$1" >> "$S/calls"; echo 00000000-0000-0000-0000-000000000099; }
wait_outbox_feature() { :; }
wait_custody_headroom() { :; }
wait_status() { :; }
tick_count() { echo 3; }
assert_routes() {
  local id="$1"
  grep -qxF "issue /api/repos/$id" "$S/calls" && grep -qxF "run /api/repos/$id" "$S/calls" \
    || fail "issue and run must both use isolated repo $id: $(cat "$S/calls")"
}

# Execute the shipped local seeding, protected-ref hooks and transport configuration.
reset
RUNROOT="$S/seed"
mkdir -p "$RUNROOT/fakeremote" "$RUNROOT/agent-gitconfig"
unset E2E_GIT_SMART_HTTP E2E_FAULT_PREFLIGHT
eval "$(awk '/^seed_git\(\)/' "$LIB")"
seed_block="$(awk '/^E2E_REPOSITORIES=/ {p=1} p && /^# A NEUTRAL/ {exit} p {print}' "$LIB")"
[ -n "$seed_block" ] || fail 'real seeding block not found'
eval "$seed_block" > "$S/seed.log" 2>&1
checked=()
for name in restore45a restore45b restore52a restore52b; do
  git --git-dir="$RUNROOT/fakeremote/$name.git" cat-file -e main:README.md
  [ -x "$RUNROOT/fakeremote/$name.git/hooks/pre-receive" ] || fail "missing main protection for $name"
  [ "$(git config -f "$RUNROOT/agent-gitconfig/gitconfig" --get "url./fakeremote/$name.git.insteadOf")" = "https://forge-fake.e2e/group/$name.git" ] \
    || fail "missing insteadOf mapping for $name"
  grep -qF "FORGE_FAKE_PROJECT$((3 + ${#checked[@]})): group/$name" "$ROOT/e2e/docker-compose.e2e.yml"
  checked+=("$name")
done
echo 'PASS: real seeding and both transport inputs reserve four protected, separately mapped restore bares'

reset
ids=()
for name in restore45a restore45b restore52a restore52b; do
  enable_restore_fixture_repo "$name"
  ids+=("$RESTORE_FIXTURE_REPO_ID")
done
[ "$(printf '%s\n' "${ids[@]}" | sort -u | wc -l | tr -d ' ')" = 4 ] || fail 'restore fixture repos alias'
[ "${#RESTORE_FIXTURE_REPOS[@]}" = 4 ] || fail 'each enabled fixture must be owned for exit restoration'
restore_fixture_repo_states
[ "${#RESTORE_FIXTURE_REPOS[@]}" = 0 ] || fail 'confirmed restoration must release fixture bookkeeping'
jq -e 'all(.repos[]; .enabled==false)' "$S/projects" >/dev/null
[ "$REPO_ID" = base-repo ] || fail 'base repo was overwritten'
echo 'PASS: four distinct fixture selections restore disabled state without modifying REPO_ID'

reset
UNCERTAIN_ENABLE=1
if enable_restore_fixture_repo restore45a; then fail 'lost enable response was accepted'; fi
unset UNCERTAIN_ENABLE
restore_fixture_repo_states
jq -e 'all(.repos[]; .enabled==false)' "$S/projects" >/dev/null
jq '.repos[0].enabled=true' "$S/projects" > "$S/next"; mv "$S/next" "$S/projects"
if enable_restore_fixture_repo restore45a; then fail 'already-enabled fixture was reused'; fi
restore_fixture_repo_states
jq -e '.repos[0].enabled==true' "$S/projects" >/dev/null
echo 'PASS: uncertain enable remains owned for cleanup; an already-enabled repo is refused and untouched'

reset
selection45="$(awk '/^  enable_restore_fixture_repo restore45a/ {p=1} p {print} p && /^  RUN_KB=.*create_run/ {exit}' "$P45")"
[ -n "$selection45" ] || fail 'phase45 allocation fragment not found'
eval "$selection45"
assert_routes fixture-0
assert_routes fixture-1
[ "$(grep -c '^run ' "$S/calls")" = 2 ] || fail 'phase45 must allocate exactly two isolated runs'
restore_fixture_repo_states
echo 'PASS: actual phase45 KA/KB allocation uses separate fixture repos for issue and run creation'

reset
eval "$(awk '/^make_outbox_run\(\) \{/,/^\}/' "$P52")"
make_outbox_run
assert_routes base-repo
for name in restore52a restore52b; do
  : > "$S/calls"
  selection52="$(awk -v name="$name" '$0 ~ ("^enable_restore_fixture_repo " name) {p=1} p {print} p && /^make_outbox_run / {exit}' "$P52")"
  [ -n "$selection52" ] || fail "phase52 allocation fragment missing: $name"
  eval "$selection52"
  assert_routes "$RESTORE_FIXTURE_REPO_ID"
done
unset OUTBOX_REPO_ID
restore_fixture_repo_states
[ "$REPO_ID" = base-repo ] && jq -e 'all(.repos[]; .enabled==false)' "$S/projects" >/dev/null
echo 'PASS: real phase52 producer keeps cases1-6 on the base repo and cases7/8 isolated, with selection restored'
echo 'cases=5 passed=5'
