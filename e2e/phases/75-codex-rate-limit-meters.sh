# shellcheck shell=bash
# phase:    codex-rate-limit-meters
# title:    PRD #1209: per-account Codex rate-limit meters (seeded snapshot row -> /me)
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  admin codex state (seeds a linked codex_provider_account + a codex_auth alias + a rate-limit snapshot, all deleted before this phase ends)
# restores: only this phase's account and alias are deleted; prior admin Codex state is preserved
# ---------------------------------------------------------------------------
# PRD #1209 (Codex account rate-limit visibility): the Codex sibling of 47-rate-limit-meters.sh.
# The Codex usage poller is DISABLED in the overlay (UZI_CODEX_USAGE_POLL_INTERVAL=0) for the
# SAME reason the Anthropic one is: the isolated stack has no live provider and the usage
# base URL is a hardcoded const (an SSRF-guard decision, no env knob), so there is nothing to
# point at a fake. Everything the handler/CLI/TUI layers can prove is already proven in
# api/internal/handler/codex_ratelimits_livedb_test.go over the real handlers. What no lower
# layer shows is that a snapshot row written to the REAL schema (00199/00200/00239) reaches
# the REAL /api/me/codex-rate-limits endpoint, so one seeded snapshot -> /me remains.
#
# STATUS IS polling_disabled, NOT fresh: with the poller off (interval<=0) the handler's
# codexRateLimitStatus returns polling_disabled at its highest-precedence branch
# (handler/codex_ratelimits.go), and marks the retained reading stale=true — the exact
# "disabled => readings serve stale" contract the overlay's Anthropic note states. The
# seeded bucket VALUE is surfaced regardless of status, which is what this phase asserts.
say "PRD #1209: per-account Codex rate-limit meters (seeded snapshot row → /me)"
login  # refresh the admin session
CX_RL_ADMIN_ID="$(db_psql "SELECT id FROM users WHERE email = '$ADMIN_EMAIL'")"
[ -n "$CX_RL_ADMIN_ID" ] || fail "could not resolve the admin id for the codex rate-limit seed"

# The frozen []CodexRateLimitBucketDTO shape (apitypes/codex_ratelimit.go): one 'codex'
# bucket with a primary window. Single-quoted in bash so its JSON double-quotes need no
# escaping, then interpolated inside the SQL single-quoted ::jsonb literal.
CX_RL_BUCKETS='[{"id":"codex","display_name":"5-hour","allowed":true,"limit_reached":false,"primary":{"used_percent":63,"limit_window_seconds":18000,"reset_after_seconds":3600,"reset_at":1893456000},"secondary":null}]'
CX_RL_FIXTURE="$(db_psql 'SELECT gen_random_uuid()')" || fail 'codex meter: fixture identity seed failed'
[[ "$CX_RL_FIXTURE" =~ ^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$ ]] || fail 'codex meter: invalid fixture identity'
CX_RL_LABEL="e2e-codex-limits-meter-$CX_RL_FIXTURE"
CX_RL_ACCT_ID="" CX_RL_ALIAS_ID=""
cleanup_codex_meter() {
  # IDs are assigned only from successful INSERT RETURNING. Never adopt, overwrite,
  # or delete another phase's account when a seed fails.
  if [ -n "$CX_RL_ALIAS_ID" ]; then
    db_psql "DELETE FROM user_secrets WHERE id = '$CX_RL_ALIAS_ID' AND user_id = '$CX_RL_ADMIN_ID'" >/dev/null || return 1
    CX_RL_ALIAS_ID=""
  fi
  if [ -n "$CX_RL_ACCT_ID" ]; then
    db_psql "DELETE FROM codex_provider_account WHERE id = '$CX_RL_ACCT_ID' AND user_id = '$CX_RL_ADMIN_ID'" >/dev/null || return 1
    CX_RL_ACCT_ID=""
  fi
}
trap 'cleanup_codex_meter || echo "codex meter: fixture cleanup failed" >&2' EXIT

# (1) A linked Codex provider account. sealed_login is dummy bytes — the read path never
# decrypts it — sealed_with='dek', at generation 0 (matching the snapshot's observed fence).
CX_RL_ACCT_ID="$(db_psql "INSERT INTO codex_provider_account
    (user_id, provider_user_id, workspace_account_id, sealed_login, sealed_with, generation, credential_revision, coord_state)
  VALUES ('$CX_RL_ADMIN_ID', 'e2e-codex-user', 'e2e-codex-workspace-$CX_RL_FIXTURE', decode('deadbeef','hex'), 'dek', 0, 0, 'idle')
  ON CONFLICT DO NOTHING RETURNING id")" || fail 'codex meter: provider account seed failed'
[ -n "$CX_RL_ACCT_ID" ] || fail 'codex meter: provider account seed returned no new account'

# (2) A codex_auth alias (user_secret) whose label names the account on the meter, marked
# default only if the admin has no default Codex credential. Existing defaults
# belong to prior fixtures and are never changed to make this fixture pass.
CX_RL_ALIAS="$(db_psql "INSERT INTO user_secrets (user_id, kind, label, ciphertext, sealed_with, is_default)
  VALUES ('$CX_RL_ADMIN_ID', 'codex_auth', '$CX_RL_LABEL', decode('deadbeef','hex'), 'dek',
    NOT EXISTS (SELECT 1 FROM user_secrets WHERE user_id='$CX_RL_ADMIN_ID'
      AND kind IN ('codex_auth','openai_api_key') AND is_default))
  RETURNING json_build_object('id',id,'is_default',is_default)")" || fail 'codex meter: alias seed failed'
CX_RL_ALIAS_ID="$(printf '%s' "$CX_RL_ALIAS" | jq -er '.id')" || fail 'codex meter: alias seed returned no identity'
CX_RL_DEFAULT="$(printf '%s' "$CX_RL_ALIAS" | jq -er '.is_default | tostring')" || fail 'codex meter: alias seed returned no default flag'

# (3) The alias's credential state, LINKED to the account (the query's EXISTS(linked) filter
# is what makes the account appear on the meter).
db_psql "INSERT INTO codex_credential_state (user_secret_id, user_id, status, provider_account_id, material_revision)
  VALUES ('$CX_RL_ALIAS_ID', '$CX_RL_ADMIN_ID', 'linked', '$CX_RL_ACCT_ID', 0)" >/dev/null \
  || fail 'codex meter: linked credential seed failed'

# (4) The rate-limit snapshot, fenced on the account's (generation, credential_revision) and
# with a fresh last_success_at — the exact shape UpsertCodexAccountRateLimits writes.
db_psql "INSERT INTO codex_account_rate_limits
    (user_id, provider_account_id, buckets, observed_generation, observed_credential_revision, last_success_at, last_attempt_at, attempt_status)
  VALUES ('$CX_RL_ADMIN_ID', '$CX_RL_ACCT_ID', '$CX_RL_BUCKETS'::jsonb, 0, 0, now(), now(), 'ok')" >/dev/null \
  || fail 'codex meter: rate-limit snapshot seed failed'

# The endpoint surfaces exactly this fixture account: named by its linked-alias label,
# polling_disabled + stale (the poller is off), and carrying the seeded 63% primary window.
apiget /api/me/codex-rate-limits \
  | jq -e --arg label "$CX_RL_LABEL" --arg account "$CX_RL_ACCT_ID" --argjson default "$CX_RL_DEFAULT" \
    '[.accounts[] | select(.account_id == $account)] | length == 1 and (.[0]
      | .status == "polling_disabled"
        and .stale == true
        and .is_default == $default
        and (.aliases | index($label)) != null
        and (.buckets | length) == 1
        and .buckets[0].id == "codex"
        and .buckets[0].primary.used_percent == 63)' >/dev/null \
  || fail "/api/me/codex-rate-limits did not surface the seeded snapshot (got: $(apiget /api/me/codex-rate-limits | jq -c .))"
pass "PRD #1209: fixture Codex snapshot surfaces on /api/me/codex-rate-limits (default=$CX_RL_DEFAULT, codex bucket 63%, polling_disabled+stale)"

# Deleting only our alias/account cascades our credential state and snapshot.
cleanup_codex_meter || fail 'codex meter: fixture cleanup failed'
trap - EXIT
pass 'teardown: fixture Codex account and alias removed; prior accounts preserved'
