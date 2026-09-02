# shellcheck shell=bash
# phase:    least-privilege
# title:    PRD #5 privilege checks: over-privileged connect is rejected + stored nothing; compliant connection is least-privilege
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: FRESHJAR
# handoff:  -
# mutates:  -
# restores: -
# --- PRD #5: least-privilege journey (steps 3-4) -----------------------------
# The forge base the seed + SSRF allowlist use (docker-compose.e2e.yml).
FORGE_BASE="https://forge-fake.e2e"
say "PRD #5 privilege checks: over-privileged connect is rejected + stored nothing; compliant connection is least-privilege"

# Step 3: a fresh, non-admin user (registration is open) connecting an
# OVER-privileged PAT is rejected with 422 + the violation list, and NOTHING is
# stored. A fresh user isolates the "no forge_connections row afterward"
# invariant from the admin's seeded connection.
FRESHJAR="$RUNROOT/fresh.jar"
FRESH_EMAIL="contractor@uzi.e2e"
FRESH_PASS="e2e-fresh-user-password-000"
curl -fsS -c "$FRESHJAR" -X POST "$BASE/api/auth/register" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$FRESH_EMAIL\",\"password\":\"$FRESH_PASS\"}" >/dev/null \
  || fail "fresh user could not register (is registration open in the e2e overlay?)"

# POST the over-privileged PAT, capturing status + body (no -f: 422 is expected).
OVER_BODY="$RUNROOT/overpriv.json"
OVER_CODE="$(curl -sS -b "$FRESHJAR" -o "$OVER_BODY" -w '%{http_code}' \
  -X POST "$BASE/api/forge/connections" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $(awk '$6=="uzi_csrf"{print $7}' "$FRESHJAR")" \
  -d "{\"base_url\":\"$FORGE_BASE\",\"token\":\"e2e-overprivileged-pat-000000\"}")"
[ "$OVER_CODE" = 422 ] || fail "over-privileged connect: expected 422, got $OVER_CODE (body: $(cat "$OVER_BODY"))"
jq -e '.violations | length > 0' "$OVER_BODY" >/dev/null 2>&1 \
  || fail "422 body missing a violations list: $(cat "$OVER_BODY")"
pass "over-privileged PAT rejected with 422 + violations"

# Nothing stored on rejection: the fresh user has zero connections.
FRESH_CONNS="$(curl -fsS -b "$FRESHJAR" "$BASE/api/forge/connections" | jq '.connections | length')"
[ "$FRESH_CONNS" = 0 ] || fail "422 rejection must store nothing; found $FRESH_CONNS connection(s) for the rejected user"
pass "nothing stored on rejection (0 connections for the rejected user)"

# Step 4: the admin's seeded, compliant connection reports least-privilege on an
# on-demand check (api-only PAT, Developer on a protected non-Developer-pushable main).
CONN_ID="$(apiget /api/forge/connections | jq -r '.connections[0].id // empty')"
[ -n "$CONN_ID" ] || fail "no seeded forge connection for the admin"
PRIV_REPORT="$(apipost "/api/forge/connections/$CONN_ID/privilege-check" '')"
echo "$PRIV_REPORT" | jq -e '.report.status == "ok"' >/dev/null 2>&1 \
  || fail "compliant connection privilege-check not ok: $PRIV_REPORT"
pass "compliant connection reports least-privilege ✓"

