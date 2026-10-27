#!/usr/bin/env bash
# Execute shipped bootstrap/phase shell over fake HTTP, DB and compose seams.
# No Docker, network, password derivation or worker.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [ "${1:-}" = --simulate ]; then
  fixture="$2"
else
  mkdir -p "$ROOT/.uzi/scratch"
  fixture="$(mktemp -d "$ROOT/.uzi/scratch/vault-modes.XXXXXX")"
  trap 'rm -rf "$fixture"' EXIT
  awk '/^# Fresh optional policy/{copy=1} /^REPO_ID=/{copy=0} copy' "$ROOT/e2e/run-e2e.sh" > "$fixture/bootstrap.sh"
  [ -s "$fixture/bootstrap.sh" ]
  mkdir -p "$fixture/bin"
  # The phase's recovery wrapper uses command curl with a 15s bound.
  # Supply an external fake too, so that path cannot reach the network.
  cat > "$fixture/bin/curl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *"/api/vault/unlock"*)
    printf true > "$STATE/user2-unlocked"
    printf dek > "$STATE/user2-sealed"
    echo user2-unlock >> "$STATE/calls" ;;
  *) exit 90 ;;
esac
FAKE
  chmod +x "$fixture/bin/curl"
fi
export PATH="$fixture/bin:$PATH"
cases=0
passed=0

simulate() (
  scenario="$1"
  export STATE="$fixture/$scenario"
  mkdir -p "$STATE"
  ADMIN_EMAIL=admin@uzi.e2e ADMIN_PASS=dummy-password
  JAR=admin JAR2="$STATE/user2.jar" BASE=http://fake REPO_ID=repo DUMMY_ANTHROPIC=dummy
  COMPLETE_TIMEOUT_DEFAULT=1
  COMPOSE=(fake_compose)
  printf optional > "$STATE/policy"
  printf unprotected > "$STATE/admin"
  printf false > "$STATE/admin-unlocked"
  printf master_aad > "$STATE/admin-sealed"
  printf protected > "$STATE/user2"
  printf true > "$STATE/user2-unlocked"
  printf dek > "$STATE/user2-sealed"
  printf 'fake fake fake fake fake uzi_csrf dummy\n' > "$JAR2"
  say() { :; }
  pass() { :; }
  fail() { echo "$*" >&2; exit 7; }
  login() { [ ! -f "$STATE/restarted-off" ] || fail "Off claim must not login"; echo login >> "$STATE/calls"; }
  sleep() { :; }
  csrf() { printf dummy; }
  status() {
    local who="$1" mode unlocked
    mode="$(cat "$STATE/$who")"
    unlocked="$(cat "$STATE/$who-unlocked")"
    jq -cn --arg mode "$mode" --arg policy "$(cat "$STATE/policy")" --argjson unlocked "$unlocked" \
      '{mode:$mode,policy:$policy,unlocked:$unlocked,exists:($mode=="protected")}'
  }
  apiget() {
    case "$1" in
      /api/vault/status) status admin ;;
      /api/admin/settings) jq -cn --arg p "$(cat "$STATE/policy")" '{settings:{vault_policy:$p},unprotected_users:1}' ;;
      /api/admin/vault-migration)
        if [ "$(cat "$STATE/user2-sealed")" = master ]; then echo '{"master_sealed":1}'; else echo '{"master_sealed":0}'; fi ;;
      /api/admin/rate-limits)
        echo '{"users":[{"email":"admin@uzi.e2e","vault_mode":"unprotected","vault_locked":false}]}' ;;
      /api/admin/codex-rate-limits) echo '{"users":[]}' ;;
      /api/runs/*) echo '{"run":{"status":"queued"}}' ;;
      *) return 90 ;;
    esac
  }
  apiput() {
    echo "put $1" >> "$STATE/calls"
    case "$1" in
      /api/admin/settings) jq -er '.settings.vault_policy' <<< "$2" > "$STATE/policy" ;;
      /api/me/secrets/anthropic_token) printf dek > "$STATE/admin-sealed" ;;
      *) return 90 ;;
    esac
  }
  apipost() {
    echo "post $1" >> "$STATE/calls"
    case "$1" in
      /api/vault/enable)
        if [ "$scenario" = cleanup-enable-fails ] && [ -f "$STATE/restarted-off" ]; then return 22; fi
        printf protected > "$STATE/admin"; printf true > "$STATE/admin-unlocked"
        printf dek > "$STATE/admin-sealed"
        echo '{"mode":"protected","rotation_advice":"rotate backup credentials"}' ;;
      /api/vault/disable)
        [ "$(cat "$STATE/policy")" = optional ] || return 91
        printf unprotected > "$STATE/admin"; printf false > "$STATE/admin-unlocked"
        printf master_aad > "$STATE/admin-sealed"
        echo '{"mode":"unprotected"}' ;;
      /api/vault/lock) printf false > "$STATE/admin-unlocked" ;;
      /api/vault/unlock) printf true > "$STATE/admin-unlocked" ;;
      /api/repos/repo/issues) echo '{"card":{"iid":1}}' ;;
      /api/runs/*/inputs) : ;;
      *) return 90 ;;
    esac
  }
  db_psql() {
    case "$1" in
      *"INSERT INTO user_secrets"*) printf master > "$STATE/user2-sealed" ;;
      *"SELECT id FROM users"*) echo user2-id ;;
      *"count(*) FROM user_vaults"*) echo 0 ;;
      *"s.sealed_with"*)
        if [[ "$1" == *user2@uzi.e2e* ]]; then cat "$STATE/user2-sealed"; else cat "$STATE/admin-sealed"; fi ;;
      *) return 90 ;;
    esac
  }
  curl() {
    local who=admin arg
    for arg in "$@"; do [ "$arg" != "$JAR2" ] || who=user2; done
    case "$*" in
      *"/api/auth/login"*) printf true > "$STATE/user2-unlocked" ;;
      *"/api/vault/status"*) status "$who" ;;
      *"/api/vault/unlock"*) printf true > "$STATE/$who-unlocked"; printf dek > "$STATE/$who-sealed"
        echo "$who-unlock" >> "$STATE/calls" ;;
      *) return 90 ;;
    esac
  }
  docker() { printf 'UZI_SECRET_KEY=dummy\n'; }
  fake_compose() {
    case "$1" in
      exec) echo 1234 ;;
      ps) echo fake-api ;;
      up)
        echo restart >> "$STATE/calls"
        printf false > "$STATE/user2-unlocked"
        if [ "$(cat "$STATE/admin")" = protected ]; then
          printf true > "$STATE/admin-unlocked"
        else
          touch "$STATE/restarted-off"
          if [ "$scenario" = off-enrolled-at-boot ]; then printf protected > "$STATE/admin"; fi
        fi ;;
      *) return 90 ;;
    esac
  }
  wait_http() { :; }
  create_run() {
    if [ -f "$STATE/restarted-off" ]; then echo off-run; else echo protected-run; fi
  }
  wait_status() {
    echo "wait $1 $2" >> "$STATE/calls"
    if [ "$1" = off-run ]; then
      [ "$(cat "$STATE/admin")" = unprotected ] || return 92
      [ "$(cat "$STATE/admin-unlocked")" = false ] || return 93
      case "$scenario" in
        off-claim-fails|cleanup-enable-fails) fail "simulated Off claim failure" ;;
      esac
    fi
  }
  source "$fixture/bootstrap.sh"
  [ "$(cat "$STATE/admin")" = protected ]
  [ "$(cat "$STATE/admin-sealed")" = dek ]
  source "$ROOT/e2e/phases/34-vault.sh"
)

# A separate Bash preserves errexit while the parent records the nonzero exit.
if [ "${1:-}" = --simulate ]; then
  simulate "$3"
  exit
fi
for scenario in success off-enrolled-at-boot off-claim-fails cleanup-enable-fails; do
  cases=$((cases+1))
  rc=0
  bash "$0" --simulate "$fixture" "$scenario" > "$fixture/$scenario.log" 2>&1 || rc=$?
  valid=false
  if [ "$scenario" = success ]; then
    [ "$rc" -eq 0 ] && [ "$(cat "$fixture/$scenario/admin")" = protected ] \
      && [ "$(cat "$fixture/$scenario/admin-unlocked")" = true ] \
      && [ "$(cat "$fixture/$scenario/policy")" = required ] \
      && [ "$(cat "$fixture/$scenario/user2-unlocked")" = true ] \
      && grep -qF 'wait off-run completed' "$fixture/$scenario/calls" && valid=true
  else
    [ "$rc" -eq 7 ] && [ "$(cat "$fixture/$scenario/policy")" = required ] \
      && [ "$(cat "$fixture/$scenario/user2-unlocked")" = true ] && valid=true
    if [ "$scenario" = off-claim-fails ] || [ "$scenario" = cleanup-enable-fails ]; then
      grep -qF 'post /api/runs/off-run/inputs' "$fixture/$scenario/calls" || valid=false
    fi
    if [ "$scenario" = off-claim-fails ]; then
      [ "$(cat "$fixture/$scenario/admin")" = protected ] \
        && [ "$(cat "$fixture/$scenario/admin-unlocked")" = true ] \
        && [ "$(cat "$fixture/$scenario/admin-sealed")" = dek ] || valid=false
    fi
    if [ "$scenario" = cleanup-enable-fails ]; then
      grep -qF 'admin enable failed' "$fixture/$scenario.log" || valid=false
      [ "$(cat "$fixture/$scenario/admin")" = unprotected ] \
        && [ "$(cat "$fixture/$scenario/admin-unlocked")" = false ] \
        && [ "$(cat "$fixture/$scenario/admin-sealed")" = master_aad ] || valid=false
    fi
  fi
  if [ "$valid" = true ]; then
    passed=$((passed+1)); echo "PASS: vault wire simulation $scenario"
  else
    echo "FAIL: vault wire simulation $scenario (exit=$rc)"
    cat "$fixture/$scenario.log"
  fi
done
echo "cases=$cases passed=$passed"
[ "$cases" -eq 4 ] && [ "$passed" -eq "$cases" ]
