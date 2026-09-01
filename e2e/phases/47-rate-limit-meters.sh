# shellcheck shell=bash
# phase:    rate-limit-meters
# title:    PRD #53/#104: per-token rate-limit meters (seeded gauge row -> /me)
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# ---------------------------------------------------------------------------
# PRD #53: per-user Claude rate-limit meters — COLLAPSED to a one-liner by PRD #97 M4.
# The poller is DISABLED in the overlay (UZI_USAGE_POLL_INTERVAL=0) — the isolated stack
# has no live Anthropic and the client's base URL is a hardcoded const, so there is
# nothing to point at a fake. Everything this phase used to drive is proven at the
# handler layer by `api/internal/handler/ratelimits_test.go`, which runs the real
# handlers over httptest against a fake DBTX: the FULL status union on /me
# (no_token / unavailable / ok, incl. the "no key leaks" checks), stale both ways
# (3x-interval and the poller-disabled always-stale rule), the admin list shape (every
# user appears, ok + no_token + unavailable, vault_locked), the member-403 through the
# real RequireAdmin gate, and the D3b token-delete cascade. What no lower layer can show
# is that a row written to the REAL schema by the REAL poller shape reaches the REAL
# endpoint, so one seeded gauge row → /me remains.
#
# PRD #104 M5 repointed anthropic_rate_limits from PRIMARY KEY (user_id) to
# (user_secret_id), so the seed now targets the admin's DEFAULT token id (seeded from
# UZI_SEED_ANTHROPIC_TOKEN) and the response is a per-token ARRAY, not a single reading.
say "PRD #53/#104: per-token rate-limit meters (seeded gauge row → /me)"
login  # refresh the admin session
ADMIN_ID="$(db_psql "SELECT id FROM users WHERE email = '$ADMIN_EMAIL'")"
[ -n "$ADMIN_ID" ] || fail "could not resolve the admin id for the rate-limit seed"
ADMIN_SECRET_ID="$(db_psql "SELECT id FROM user_secrets WHERE user_id = '$ADMIN_ID' AND kind = 'anthropic_token' AND is_default")"
[ -n "$ADMIN_SECRET_ID" ] || fail "the admin has no default anthropic_token to attach a gauge row to"

db_psql "INSERT INTO anthropic_rate_limits
           (user_secret_id, user_id, five_hour_pct, five_hour_resets_at, seven_day_pct, seven_day_resets_at, source, synced_at)
         VALUES ('$ADMIN_SECRET_ID', '$ADMIN_ID', 55, now() + interval '2 hours', 12, now() + interval '3 days', 'usage_endpoint', now())
         ON CONFLICT (user_secret_id) DO UPDATE SET
           five_hour_pct = 55, seven_day_pct = 12, source = 'usage_endpoint', synced_at = now()" >/dev/null

apiget /api/me/rate-limits \
  | jq -e '.tokens | length == 1 and (.[0] | .is_default == true and .limits.status == "ok" and .limits.five_hour.pct == 55 and .limits.seven_day.pct == 12 and .limits.source == "usage_endpoint")' >/dev/null \
  || fail "/api/me/rate-limits did not surface the seeded per-token gauge row (got: $(apiget /api/me/rate-limits | jq -c .))"
pass "PRD #104 M5: a seeded per-token gauge row surfaces on /api/me/rate-limits (default token, 55% / 12%, usage_endpoint)"

