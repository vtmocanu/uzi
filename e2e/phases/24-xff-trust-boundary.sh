# shellcheck shell=bash
# phase:    xff-trust-boundary
# title:    PRD #58: XFF forgery from the agent container must NOT mint fresh rate-limit buckets
# critical: no
# lane:     gitlab
# executor: any
# requires: -
# provides: -
# handoff:  -
# mutates:  -
# restores: -
# PRD #58: the compose XFF trust boundary. TRUSTED_PROXIES ships EMPTY, so the api
# never honors an X-Forwarded-For and every caller keys on its own peer address.
#
# THE GATE RUNS FROM INSIDE THE AGENT CONTAINER, which is the whole point: the agent
# runs a model against a user's cloned repo (semi-hostile BY DESIGN — it is why
# guardrails.ts exists) and shares the compose network with the api. It IS the
# attacker this boundary exists to stop, so a test from anywhere else would model the
# exploit instead of reproducing it.
#
# It is mutation-proof by construction rather than by inspection: with the OLD default
# (TRUSTED_PROXIES=10.0.0.0/8,172.16.0.0/12,...) the agent's own IP is inside the
# trusted set, every forged XFF below is a FRESH rate-limit bucket, no 429 ever
# arrives, and this fails. Measured on a real stack before the fix: N+1 requests, zero
# 429s.
#
# Note the isolation this rests on, because pre-fix it is exactly what did not exist:
# the agent keys on its own container IP, a different bucket from the nginx-proxied
# browser traffic the rest of this harness logs in with — so hammering the limiter here
# cannot lock the harness out of its own login.
say "PRD #58: XFF forgery from the agent container must NOT mint fresh rate-limit buckets"
# `|| true` is required, not defensive: this harness runs under `set -euo pipefail`,
# the e2e env file does NOT set RATE_LIMIT_MAX, so grep exits 1, pipefail propagates
# it, and the assignment itself aborts the script BEFORE the ${:-10} fallback can
# apply. Empty here therefore means "not overridden" and 10 is the compose default
# (docker-compose.yml: RATE_LIMIT_MAX: ${RATE_LIMIT_MAX:-10}).
RL_MAX="$( (grep -E '^RATE_LIMIT_MAX=' "$ENVFILE" || true) | cut -d= -f2 | tr -d '\r')"
RL_MAX="${RL_MAX:-10}"
XFF_CODES="$("${COMPOSE[@]}" exec -T agent sh -c '
  n=$(( '"$RL_MAX"' + 1 ))
  i=1
  while [ "$i" -le "$n" ]; do
    curl -s -o /dev/null -w "%{http_code} " \
      -X POST "http://api:8080/api/auth/login" \
      -H "Content-Type: application/json" \
      -H "X-Forwarded-For: 203.0.113.$i" \
      -d "{\"email\":\"xff-probe@e2e.invalid\",\"password\":\"wrong-on-purpose\"}"
    i=$(( i + 1 ))
  done' | tr -d '\r')"
case "$XFF_CODES" in
  *429*) pass "PRD #58 compose XFF: $((RL_MAX + 1)) forged X-Forwarded-For logins from the agent hit ONE bucket and got a 429 (codes: $XFF_CODES)" ;;
  *) fail "PRD #58 compose XFF: $((RL_MAX + 1)) logins with DISTINCT forged X-Forwarded-For headers never hit the rate limit (codes: $XFF_CODES).
     The agent minted a fresh per-IP bucket per request, so the brute-force control on
     /api/auth/login is bypassed. TRUSTED_PROXIES must be EMPTY on compose: any CIDR
     broad enough to type by hand covers the agent container, which shares this network
     and runs untrusted code by design." ;;
esac

