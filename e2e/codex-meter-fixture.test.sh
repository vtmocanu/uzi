#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PHASE="${CODEX_METER_TEST_PHASE:-$ROOT/e2e/phases/75-codex-rate-limit-meters.sh}"
SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT
run_case() (
  local mode="$1"
  # shellcheck disable=SC2034  # consumed by the sourced real phase
  ADMIN_EMAIL=admin@example.invalid
  say() { :; }; pass() { :; }; login() { :; }
  fail() { echo "$*" >&2; exit 1; }
  db_psql() {
    local sql="$1"
    case "$sql" in
      "SELECT id FROM users"*) echo owner ;;
      'SELECT gen_random_uuid()') echo aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa ;;
      'INSERT INTO codex_provider_account'*)
        [[ "$sql" == *'e2e-codex-workspace-aaaaaaaa'* ]] || return 1
        [[ "$sql" == *'ON CONFLICT DO NOTHING RETURNING id'* ]] || return 1
        [ "$mode" != account-fail ] || return 1
        echo created > "$SCRATCH/account"; echo bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb ;;
      'INSERT INTO user_secrets'*)
        [[ "$sql" == *'NOT EXISTS'* ]] || return 1
        [ "$mode" != alias-fail ] || return 1
        echo created > "$SCRATCH/alias"
        if [ "$mode" = clean ]; then
          echo '{"id":"cccccccc-cccc-cccc-cccc-cccccccccccc","is_default":true}'
        else
          echo '{"id":"cccccccc-cccc-cccc-cccc-cccccccccccc","is_default":false}'
        fi ;;
      'INSERT INTO codex_credential_state'*) : ;;
      'INSERT INTO codex_account_rate_limits'*) [ "$mode" != snapshot-fail ] ;;
      'DELETE FROM user_secrets'*)
        [[ "$sql" == *"id = 'cccccccc-cccc-cccc-cccc-cccccccccccc' AND user_id = 'owner'"* ]] || return 1
        echo deleted > "$SCRATCH/alias" ;;
      'DELETE FROM codex_provider_account'*)
        [[ "$sql" == *"id = 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb' AND user_id = 'owner'"* ]] || return 1
        echo deleted > "$SCRATCH/account" ;;
      *) echo 'unexpected SQL' >&2; return 1 ;;
    esac
  }
  apiget() {
    local default=false percent=63
    [ "$mode" != clean ] || default=true
    [ "$mode" != assertion-fail ] || percent=12
    printf '{"accounts":[{"account_id":"prior-account","is_default":true},{"account_id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","is_default":%s,"status":"polling_disabled","stale":true,"aliases":["e2e-codex-limits-meter-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"],"buckets":[{"id":"codex","primary":{"used_percent":%s}}]}]}\n' "$default" "$percent"
  }
  # shellcheck disable=SC1090  # real phase under test, optionally replaced for RED
  source "$PHASE"
)
for mode in prior clean; do
  run_case "$mode"
  [ "$(cat "$SCRATCH/account")" = deleted ]
  [ "$(cat "$SCRATCH/alias")" = deleted ]
  echo "PASS: $mode account/default state preserved; exact fixture meter checked"
done
for mode in account-fail alias-fail snapshot-fail assertion-fail; do
  # Parent conditional would disable errexit throughout the sourced phase, so run
  # as a plain command with only the parent's errexit disabled.
  set +e
  run_case "$mode" > "$SCRATCH/failure.log" 2>&1
  rc=$?
  set -e
  [ "$rc" != 0 ]
  case "$mode" in
    account-fail) grep -F 'provider account seed failed' "$SCRATCH/failure.log" >/dev/null ;;
    alias-fail) grep -F 'alias seed failed' "$SCRATCH/failure.log" >/dev/null
      [ "$(cat "$SCRATCH/account")" = deleted ] ;;
    snapshot-fail) grep -F 'rate-limit snapshot seed failed' "$SCRATCH/failure.log" >/dev/null
      [ "$(cat "$SCRATCH/account")" = deleted ]; [ "$(cat "$SCRATCH/alias")" = deleted ] ;;
    assertion-fail) grep -F 'did not surface the seeded snapshot' "$SCRATCH/failure.log" >/dev/null
      [ "$(cat "$SCRATCH/account")" = deleted ]; [ "$(cat "$SCRATCH/alias")" = deleted ] ;;
  esac
  echo "PASS: $mode named; only successful owned seeds cleaned"
done
echo 'cases=6 passed=6'
