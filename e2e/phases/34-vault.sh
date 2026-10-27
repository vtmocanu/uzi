# shellcheck shell=bash
# phase:    vault
# title:    Vault modes: protected/Off restart claims, DEK sealing and legacy rewrap
# critical: no
# lane:     gitlab
# executor: any
# requires: JAR2
# provides: -
# handoff:  -
# mutates:  admin token sealing + mode; vault policy; legacy master row for user2; two API recreates
# restores: required policy; admin + user2 protected/unlocked; tokens DEK-sealed; test runs completed on success (failure cancellation best effort; driver quarantines leaks)
# =============================================================================
# Bootstrap explicitly enrolled the initially-Off seed admin before this phase.
# Preserve protected claim gating, boot unlock and lazy legacy rewrap, then prove
# an opted-out seed remains Off and runnable after restart under required policy.
say "Vault modes: protected/Off restart claims, DEK sealing and legacy rewrap"
login   # refresh the already-protected admin session

# --- vault helpers -----------------------------------------------------------
# PGPW/db_psql moved up to the general helper block (see the note there) — they are not
# vault-specific and phases above this line need them.
# sealed_of EMAIL — the sealed_with of a user's anthropic_token row.
sealed_of() { db_psql "SELECT s.sealed_with FROM user_secrets s JOIN users u ON u.id = s.user_id WHERE u.email = '$1' AND s.kind = 'anthropic_token'"; }
# vault_status_jar JAR — GET /api/vault/status with JAR's cookie; a read that never unlocks.
vault_status_jar() { curl -fsS -b "$1" "$BASE/api/vault/status"; }
assert_protected() {
  vault_status_jar "$1" | jq -e --argjson unlocked "$2" '.mode == "protected" and .policy == "required" and .exists == true and .unlocked == $unlocked' >/dev/null \
    || fail "expected protected vault (unlocked=$2)"
}
unlock_user2() {
  local token
  token="$(awk '$6=="uzi_csrf"{print $7}' "$JAR2")"
  curl -fsS -b "$JAR2" -X POST "$BASE/api/vault/unlock" -H 'Content-Type: application/json' \
    -H "X-CSRF-Token: $token" -d '{"password":"e2e-user2-password-000000"}' >/dev/null
}
# Failure recovery: one attempt per independent action, each HTTP request bounded
# to 15s. A failed sibling never prevents the others; retain the original exit.
# Do not force DB terminal states: the driver's strict leak check remains active.
vault_fail_safe() (
  rc=$?
  [ "$rc" -ne 0 ] || exit 0
  curl() { command curl --max-time 15 "$@"; }
  for run in "${RUN_V:-}" "${RUN_OFF:-}"; do
    [ -n "$run" ] || continue
    state="$(apiget "/api/runs/$run" | jq -er '.run.status')" || state=unknown
    case "$state" in
      completed|failed|cancelled) ;;
      *) apipost "/api/runs/$run/inputs" '{"kind":"cancel","body":""}' >/dev/null 2>&1 \
           || echo "vault fail-safe: run cancellation failed; driver must quarantine" >&2 ;;
    esac
  done
  state="$(apiget /api/vault/status)" || state='{}'
  case "$(printf '%s' "$state" | jq -r '.mode')" in
    unprotected) apipost /api/vault/enable "{\"password\":\"$ADMIN_PASS\"}" >/dev/null 2>&1 \
      || echo "vault fail-safe: admin enable failed" >&2 ;;
    protected)
      if [ "$(printf '%s' "$state" | jq -r '.unlocked')" != true ]; then
        apipost /api/vault/unlock "{\"password\":\"$ADMIN_PASS\"}" >/dev/null 2>&1 \
          || echo "vault fail-safe: admin unlock failed" >&2
      fi ;;
    *) echo "vault fail-safe: unknown admin state; restoration unconfirmed" >&2 ;;
  esac
  apiput /api/admin/settings '{"settings":{"vault_policy":"required"}}' >/dev/null 2>&1 \
    || echo "vault fail-safe: required policy restoration failed" >&2
  unlock_user2 >/dev/null 2>&1 || echo "vault fail-safe: user2 unlock failed" >&2
  exit "$rc"
)

# master_seal_hex TEXT — AES-256-GCM seal a plaintext under UZI_SECRET_KEY, matching
# secretbox's nonce||ciphertext||tag layout, so the Go master box can open it. Run in
# forge-fake (a node image) with the key passed via env, not argv. The PLAINTEXT
# rides argv (visible in the container's `ps`) — fine here because it is only ever
# a dummy fixture; never reuse this helper with a real token. The key correctly
# goes via env, not argv.
master_seal_hex() {
  "${COMPOSE[@]}" exec -T -e SK="$SECRET_KEY_B64" forge-fake node -e '
    const crypto = require("crypto");
    const key = Buffer.from(process.env.SK, "base64");
    const nonce = crypto.randomBytes(12);
    const c = crypto.createCipheriv("aes-256-gcm", key, nonce);
    const ct = Buffer.concat([c.update(Buffer.from(process.argv[1])), c.final()]);
    process.stdout.write(Buffer.concat([nonce, ct, c.getAuthTag()]).toString("hex"));
  ' "$1" | tr -d '\r\n'
}

# 1) Bootstrap explicitly converted the Off seed token to DEK sealing.
[ "$(sealed_of admin@uzi.e2e)" = dek ] || fail "seeded admin token is not sealed_with='dek'"
assert_protected "$JAR" true
assert_protected "$JAR2" true
RUN_V=""
RUN_OFF=""
trap vault_fail_safe EXIT
pass "explicitly enrolled admin protected/unlocked; token sealed_with=dek"

# 2) Saving through the handler stores a 'dek' row.
apiput /api/me/secrets/anthropic_token "{\"token\":\"$DUMMY_ANTHROPIC\"}" >/dev/null
[ "$(sealed_of admin@uzi.e2e)" = dek ] || fail "handler save did not write sealed_with='dek'"
pass "PUT /me/secrets/anthropic_token stored sealed_with='dek'"

# 3) Lock → a new run stays queued (the worker gate withholds it) → unlock → it claims.
apipost /api/vault/lock '' >/dev/null
assert_protected "$JAR" false
IID_V="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E vault gated","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
RUN_V="$(create_run "$REPO_ID" "$IID_V")" || fail "vault-gated run-create failed (non-transient; see stderr)"
sleep 1.5   # ~3 worker poll cycles (500ms each) must pass with the run LEFT queued (PRD #97 M5)
[ "$(apiget "/api/runs/$RUN_V" | jq -r '.run.status')" = queued ] \
  || fail "a locked owner's run must stay queued (never claimed, never failed)"
pass "vault locked: run $RUN_V stayed queued across several poll cycles"

apipost /api/vault/unlock "{\"password\":\"$ADMIN_PASS\"}" >/dev/null
assert_protected "$JAR" true
wait_status "$RUN_V" awaiting_approval 30
pass "after unlock, run $RUN_V claimed and reached the plan gate within a poll cycle"
apipost "/api/runs/$RUN_V/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_V" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"
pass "the unlocked run $RUN_V completed (worker freed)"

# 4) Stage a legacy master-sealed row for a NON-seed user (user2, registered
# earlier) and confirm the admin migration count sees it. Re-login user2 first so
# its session + CSRF are fresh enough to survive the api restart below.
curl -fsS -c "$JAR2" -X POST "$BASE/api/auth/login" -H 'Content-Type: application/json' \
  -d '{"email":"user2@uzi.e2e","password":"e2e-user2-password-000000"}' >/dev/null
U2ID="$(db_psql "SELECT id FROM users WHERE email = 'user2@uzi.e2e'")"
[ -n "$U2ID" ] || fail "user2 not found in the db"
# Seal the legacy row with the api's ACTUAL master key: Compose ranks the
# developer's shell UZI_SECRET_KEY above --env-file (CLAUDE.md), so the key inside
# the container may differ from the env-file — read it from the running api so the
# staged ciphertext is one the api's master box can actually open (else rewrap
# would skip it as undecryptable). The api is distroless, so read via inspect.
SECRET_KEY_B64="$(docker inspect "$("${COMPOSE[@]}" ps -q api)" --format '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^UZI_SECRET_KEY=//p' | head -1)"
[ -n "$SECRET_KEY_B64" ] || fail "could not read the api container's UZI_SECRET_KEY"
LEGACY_HEX="$(master_seal_hex 'sk-ant-e2e-legacy-master-000000')"
[ -n "$LEGACY_HEX" ] || fail "could not master-seal a legacy ciphertext"
# label/is_default and the conflict target both moved in migration 00077 (PRD #104):
# UNIQUE (user_id, kind) is gone, so the arbiter is now the partial unique index
# "this user's DEFAULT secret of this kind" — spelled by repeating its predicate.
db_psql "INSERT INTO user_secrets (user_id, kind, label, is_default, ciphertext, sealed_with)
         VALUES ('$U2ID', 'anthropic_token', 'default', true, decode('$LEGACY_HEX','hex'), 'master')
         ON CONFLICT (user_id, kind) WHERE is_default DO UPDATE SET ciphertext = decode('$LEGACY_HEX','hex'), sealed_with = 'master'" >/dev/null
[ "$(sealed_of user2@uzi.e2e)" = master ] || fail "could not stage a master-sealed row for user2"
[ "$(apiget /api/admin/vault-migration | jq -r '.master_sealed')" -ge 1 ] \
  || fail "admin migration count did not see the master-sealed row"
pass "staged a legacy master-sealed row for user2; admin migration count >= 1"

# 5) Restart the api (recreate the process → the in-memory DEK cache is gone): the
# seed admin is boot-unlocked, a normal user is not. Both JWTs survive the restart,
# so the difference is purely who the boot path re-unlocks.
"${COMPOSE[@]}" up -d --no-deps --force-recreate api >/dev/null
wait_http
assert_protected "$JAR" true
assert_protected "$JAR2" false
pass "after the api restart: seed admin boot-unlocked (true); user2 locked (false)"

# 6) user2 unlocks (no re-login) → lazy rewrap flips their legacy row to 'dek', and
# the admin migration count drops back to zero.
unlock_user2 || fail "user2 unlock (no re-login) failed"
assert_protected "$JAR2" true
[ "$(sealed_of user2@uzi.e2e)" = dek ] || fail "lazy rewrap did not flip user2's row 'master' -> 'dek' on unlock"
[ "$(apiget /api/admin/vault-migration | jq -r '.master_sealed')" = 0 ] \
  || fail "admin migration count should be 0 after the last master row rewrapped"
pass "lazy rewrap on unlock: user2's row flipped 'master' -> 'dek'; admin count back to 0"


# 7) Off restart/claim: policy changes never enroll an existing account.
apiput /api/admin/settings '{"settings":{"vault_policy":"optional"}}' >/dev/null
apipost /api/vault/disable '{}' | jq -e '.mode == "unprotected"' >/dev/null \
  || fail "admin disable did not report Off"
apiput /api/admin/settings '{"settings":{"vault_policy":"required"}}' >/dev/null
"${COMPOSE[@]}" up -d --no-deps --force-recreate api >/dev/null
wait_http
# Retained seed env must not enroll or unlock Off at boot. No login/unlock here.
vault_status_jar "$JAR" | jq -e '.mode == "unprotected" and .policy == "required" and .unlocked == false and .exists == false' >/dev/null \
  || fail "Off seed changed mode or gained a wrapper/cache after restart"
[ "$(db_psql "SELECT count(*) FROM user_vaults v JOIN users u ON u.id=v.user_id WHERE u.email='$ADMIN_EMAIL'")" = 0 ] \
  || fail "Off seed has a durable vault wrapper"
[ "$(sealed_of admin@uzi.e2e)" = master_aad ] || fail "Off admin token must use master_aad"
assert_protected "$JAR2" false
apiget /api/admin/settings | jq -e '.settings.vault_policy == "required" and .unprotected_users >= 1' >/dev/null \
  || fail "admin settings must count the existing Off user under required policy"
# The Anthropic list includes this owner. Codex omits accountless owners;
# this phase creates no linked Codex account and cannot assert its owner DTO.
apiget /api/admin/rate-limits | jq -e --arg email "$ADMIN_EMAIL" \
  '[.users[] | select(.email == $email)] | length == 1 and (.[0] | .vault_mode == "unprotected" and .vault_locked == false)' >/dev/null \
  || fail "Off admin mislabeled as locked in /api/admin/rate-limits"
IID_OFF="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E Off vault claim after restart","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
RUN_OFF="$(create_run "$REPO_ID" "$IID_OFF")" || fail "Off run-create failed"
wait_status "$RUN_OFF" awaiting_approval 30
pass "Off seed under required policy claimed after restart without login/unlock"
apipost "/api/runs/$RUN_OFF/inputs" '{"kind":"approve_plan","body":""}' >/dev/null
wait_status "$RUN_OFF" completed "${UZI_E2E_COMPLETE_TIMEOUT:-$COMPLETE_TIMEOUT_DEFAULT}"

# Restore both users after the second restart; enable itself unlocks the admin.
apipost /api/vault/enable "{\"password\":\"$ADMIN_PASS\"}" | jq -e '.mode == "protected"' >/dev/null \
  || fail "admin re-enable failed"
assert_protected "$JAR" true
unlock_user2 || fail "user2 unlock after second restart failed"
assert_protected "$JAR2" true
[ "$(sealed_of admin@uzi.e2e)" = dek ] || fail "admin restoration did not restore DEK sealing"
[ "$(sealed_of user2@uzi.e2e)" = dek ] || fail "user2 restoration lost DEK sealing"
[ "$(apiget /api/admin/vault-migration | jq -r '.master_sealed')" = 0 ] \
  || fail "protected legacy migration count must remain zero"
trap - EXIT
pass "Off run completed; required policy and both protected/unlocked users restored"
