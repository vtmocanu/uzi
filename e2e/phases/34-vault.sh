# shellcheck shell=bash
# phase:    vault
# title:    PRD #32: per-user vault (dek sealing, claim gating, restart lock, lazy rewrap)
# critical: no
# lane:     gitlab
# executor: any
# requires: JAR2
# provides: -
# handoff:  -
# mutates:  re-saves admin anthropic_token (sealed_with=dek, :52); vault lock/unlock cycles; stages a legacy master-sealed user_secrets row for user2 (:94, rewrapped to dek by :118); recreates api (:105)
# restores: admin left unlocked (explicit unlock + EXIT-trap fail-safe so a failed queued/create_run assertion after the lock can't strand the admin vault LOCKED for the fail-soft driver); user2's row ends sealed_with=dek
# =============================================================================
# PRD #32 — per-user vault (password-wrapped secrets). Proves: the seeded token is
# DEK-sealed at boot; saving through the handler writes 'dek'; a locked owner's
# runs stay queued (never claimed, never failed) and claim after unlock; an API
# restart boot-unlocks the seed admin while a normal user stays locked (JWT
# survives, the in-memory DEK cache does not); lazy rewrap flips a legacy
# 'master' row to 'dek' on unlock; and the admin migration count reflects it.
say "PRD #32: per-user vault (dek sealing, claim gating, restart lock, lazy rewrap)"
login   # fresh admin session; login also unlocks the admin's vault

# --- vault helpers -----------------------------------------------------------
# PGPW/db_psql moved up to the general helper block (see the note there) — they are not
# vault-specific and phases above this line need them.
# sealed_of EMAIL — the sealed_with of a user's anthropic_token row.
sealed_of() { db_psql "SELECT s.sealed_with FROM user_secrets s JOIN users u ON u.id = s.user_id WHERE u.email = '$1' AND s.kind = 'anthropic_token'"; }
# vault_status_jar JAR — GET /api/vault/status with JAR's cookie; a read that never unlocks.
vault_status_jar() { curl -fsS -b "$1" "$BASE/api/vault/status" | jq -r '.unlocked'; }
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

# 1) The seeded token is DEK-sealed at boot; /api/me reports the vault unlocked.
[ "$(sealed_of admin@uzi.e2e)" = dek ] || fail "seeded admin token is not sealed_with='dek'"
[ "$(apiget /api/auth/me | jq -r '.vault.unlocked')" = true ] || fail "admin /api/auth/me vault.unlocked should be true after login"
pass "seeded admin token sealed_with='dek'; /api/me reports the vault unlocked"

# 2) Saving through the handler stores a 'dek' row.
apiput /api/me/secrets/anthropic_token "{\"token\":\"$DUMMY_ANTHROPIC\"}" >/dev/null
[ "$(sealed_of admin@uzi.e2e)" = dek ] || fail "handler save did not write sealed_with='dek'"
pass "PUT /me/secrets/anthropic_token stored sealed_with='dek'"

# 3) Lock → a new run stays queued (the worker gate withholds it) → unlock → it claims.
apipost /api/vault/lock '' >/dev/null
# Fail-safe: a failed queued-status assertion or a create_run failure below would exit
# this subshell before the explicit unlock at the end, stranding the seed admin's vault
# LOCKED for the fail-soft driver. Unlock on ANY exit; dropped with `trap - EXIT` after
# the explicit unlock + its confirmation run.
trap 'apipost /api/vault/unlock "{\"password\":\"$ADMIN_PASS\"}" >/dev/null 2>&1 || true' EXIT
[ "$(apiget /api/auth/me | jq -r '.vault.unlocked')" = false ] || fail "vault should report locked after POST /api/vault/lock"
IID_V="$(apipost "/api/repos/$REPO_ID/issues" \
  '{"title":"E2E vault gated","description":"implements prds/4-agent-runtime-workers.md"}' | jq -r '.card.iid')"
RUN_V="$(create_run "$REPO_ID" "$IID_V")" || fail "vault-gated run-create failed (non-transient; see stderr)"
sleep 1.5   # ~3 worker poll cycles (500ms each) must pass with the run LEFT queued (PRD #97 M5)
[ "$(apiget "/api/runs/$RUN_V" | jq -r '.run.status')" = queued ] \
  || fail "a locked owner's run must stay queued (never claimed, never failed)"
pass "vault locked: run $RUN_V stayed queued across several poll cycles"

apipost /api/vault/unlock "{\"password\":\"$ADMIN_PASS\"}" >/dev/null
[ "$(apiget /api/auth/me | jq -r '.vault.unlocked')" = true ] || fail "vault should report unlocked after POST /api/vault/unlock"
trap - EXIT  # explicit unlock confirmed; drop the fail-safe
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
[ "$(vault_status_jar "$JAR")"  = true  ] || fail "the seed admin must be boot-unlocked after a restart (no interactive login)"
[ "$(vault_status_jar "$JAR2")" = false ] || fail "a normal user's vault must be LOCKED after a restart (JWT survives, DEK cache does not)"
pass "after the api restart: seed admin boot-unlocked (true); user2 locked (false)"

# 6) user2 unlocks (no re-login) → lazy rewrap flips their legacy row to 'dek', and
# the admin migration count drops back to zero.
u2csrf="$(awk '$6=="uzi_csrf"{print $7}' "$JAR2")"
curl -fsS -b "$JAR2" -X POST "$BASE/api/vault/unlock" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $u2csrf" -d '{"password":"e2e-user2-password-000000"}' >/dev/null \
  || fail "user2 unlock (no re-login) failed"
[ "$(vault_status_jar "$JAR2")" = true ] || fail "user2 vault should be unlocked after POST /api/vault/unlock"
[ "$(sealed_of user2@uzi.e2e)" = dek ] || fail "lazy rewrap did not flip user2's row 'master' -> 'dek' on unlock"
[ "$(apiget /api/admin/vault-migration | jq -r '.master_sealed')" = 0 ] \
  || fail "admin migration count should be 0 after the last master row rewrapped"
pass "lazy rewrap on unlock: user2's row flipped 'master' -> 'dek'; admin count back to 0"

