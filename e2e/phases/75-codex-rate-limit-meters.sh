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
# restores: the admin ends with zero codex-kind secrets and no codex_provider_account, exactly as found
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
CX_RL_LABEL="e2e-codex-limits-meter"

# (1) A linked Codex provider account. sealed_login is dummy bytes — the read path never
# decrypts it — sealed_with='dek', at generation 0 (matching the snapshot's observed fence).
CX_RL_ACCT_ID="$(db_psql "INSERT INTO codex_provider_account
    (user_id, provider_user_id, workspace_account_id, sealed_login, sealed_with, generation, credential_revision, coord_state)
  VALUES ('$CX_RL_ADMIN_ID', 'e2e-codex-user', 'e2e-codex-workspace', decode('deadbeef','hex'), 'dek', 0, 0, 'idle')
  RETURNING id")"
[ -n "$CX_RL_ACCT_ID" ] || fail "could not seed the codex_provider_account"

# (2) A codex_auth alias (user_secret) whose label names the account on the meter, marked
# default so the DTO's is_default is a meaningful, non-vacuous assertion.
CX_RL_ALIAS_ID="$(db_psql "INSERT INTO user_secrets (user_id, kind, label, ciphertext, sealed_with, is_default)
  VALUES ('$CX_RL_ADMIN_ID', 'codex_auth', '$CX_RL_LABEL', decode('deadbeef','hex'), 'dek', true)
  RETURNING id")"
[ -n "$CX_RL_ALIAS_ID" ] || fail "could not seed the codex_auth alias"

# (3) The alias's credential state, LINKED to the account (the query's EXISTS(linked) filter
# is what makes the account appear on the meter).
db_psql "INSERT INTO codex_credential_state (user_secret_id, user_id, status, provider_account_id, material_revision)
  VALUES ('$CX_RL_ALIAS_ID', '$CX_RL_ADMIN_ID', 'linked', '$CX_RL_ACCT_ID', 0)" >/dev/null

# (4) The rate-limit snapshot, fenced on the account's (generation, credential_revision) and
# with a fresh last_success_at — the exact shape UpsertCodexAccountRateLimits writes.
db_psql "INSERT INTO codex_account_rate_limits
    (user_id, provider_account_id, buckets, observed_generation, observed_credential_revision, last_success_at, last_attempt_at, attempt_status)
  VALUES ('$CX_RL_ADMIN_ID', '$CX_RL_ACCT_ID', '$CX_RL_BUCKETS'::jsonb, 0, 0, now(), now(), 'ok')" >/dev/null

# The endpoint surfaces exactly this one account: named by its linked-alias label, default,
# polling_disabled + stale (the poller is off), and carrying the seeded 63% primary window.
apiget /api/me/codex-rate-limits \
  | jq -e --arg label "$CX_RL_LABEL" '.accounts | length == 1 and (.[0]
      | .status == "polling_disabled"
        and .stale == true
        and .is_default == true
        and (.aliases | index($label)) != null
        and (.buckets | length) == 1
        and .buckets[0].id == "codex"
        and .buckets[0].primary.used_percent == 63)' >/dev/null \
  || fail "/api/me/codex-rate-limits did not surface the seeded snapshot (got: $(apiget /api/me/codex-rate-limits | jq -c .))"
pass "PRD #1209: a seeded per-account Codex snapshot surfaces on /api/me/codex-rate-limits (default account, codex bucket 63%, polling_disabled+stale)"

# Teardown: leave the admin's codex state exactly as found (zero codex-kind secrets, no
# provider account). Deleting the alias cascades its credential_state; deleting the account
# cascades its snapshot.
db_psql "DELETE FROM user_secrets WHERE id = '$CX_RL_ALIAS_ID'" >/dev/null
db_psql "DELETE FROM codex_provider_account WHERE id = '$CX_RL_ACCT_ID'" >/dev/null
[ "$(db_psql "SELECT count(*) FROM codex_provider_account WHERE user_id = '$CX_RL_ADMIN_ID'")" = 0 ] \
  || fail "codex rate-limit phase leaked a codex_provider_account for the admin"
pass "teardown: admin codex state restored (no codex_provider_account, no codex_auth alias)"
